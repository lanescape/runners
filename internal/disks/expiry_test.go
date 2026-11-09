package disks_test

import (
	"strconv"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/selector"
)

func tools(key, scope string) string {
	return "lanescape-2vcpu-linux-amd64-disk-tools-" + key + "-" + scope
}

// savePull runs one of a pull request's jobs that saves, and returns the
// image.
func (e *env) savePull(p *ghemu.Pull, label string) string {
	e.t.Helper()
	before := e.images()
	e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{label}}).finish(true)
	return e.saved(before).Name
}

func (e *env) has(image string) bool { _, ok := e.images()[image]; return ok }

func TestAPullRequestsDisksExpire(t *testing.T) {
	e := setup(t)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	unused := e.savePull(p, sel(key1, pr))
	e.clock.Advance(disks.PullRequestUnused - time.Hour)
	e.pass()
	if !e.has(unused) {
		t.Fatal("a pull request's disk went before 14 days unused")
	}
	e.clock.Advance(time.Hour)
	e.pass()
	if e.has(unused) {
		t.Fatal("a pull request's disk outlived 14 days unused")
	}
	if _, ok := e.record("images/5001/" + unused + ".json"); ok {
		t.Fatal("a deleted image's record was kept")
	}
}

func TestAClosedPullRequestsDisksGo(t *testing.T) {
	e := setup(t)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	closed := e.savePull(p, sel(key2, pr))
	e.gh.ClosePull(p)
	e.clock.Advance(disks.PullRequestClosed - time.Hour)
	e.pass()
	if !e.has(closed) {
		t.Fatal("a closed pull request's disk went before a day")
	}
	e.clock.Advance(2 * time.Hour)
	e.pass()
	if e.has(closed) {
		t.Fatal("a pull request's disk outlived its pull request by a day")
	}
}

func TestUsingADiskKeepsIt(t *testing.T) {
	e := setup(t)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	img := e.savePull(p, sel(key1, pr))
	e.clock.Advance(10 * 24 * time.Hour)
	used := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}})
	if used.source() != img {
		t.Fatalf("restored %s, want %s", used.source(), img)
	}
	if got := e.images()[img].Labels[disks.LabelLastUsed]; got != e.clock.Now().UTC().Format("2006-01-02") {
		t.Fatalf("last-used label %q", got)
	}
	rec, _ := e.record("images/5001/" + img + ".json")
	if at, _ := time.Parse(time.RFC3339Nano, rec["last_used"].(string)); e.clock.Now().Sub(at) > time.Minute {
		t.Fatalf("last used %s in the record; the restore didn't mark it", at)
	}
	used.finish(false)
	e.clock.Advance(10 * 24 * time.Hour)
	e.pass()
	if !e.has(img) {
		t.Fatal("a disk used 10 days ago went")
	}
}

func TestMainsDisksExpireExceptTheNewestOfEachFamily(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	newest := e.saveMain(key2)
	e.clock.Advance(disks.MainUnused)
	e.pass()
	if e.has(old) {
		t.Fatal("main's disk outlived 90 days unused")
	}
	if !e.has(newest) {
		t.Fatal("the newest of main's family was deleted")
	}
}

func TestSizeLimits(t *testing.T) {
	e := setup(t)
	// Each disk here is 50 GB.
	e.cfg.Limits.Defaults.MainDisksGB = 100
	e.cfg.Limits.Defaults.PullRequestDisksGB = 100
	m1 := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	m2 := e.saveMain(key2)

	// A pull request fills its scope's limit, then makes room in its own
	// scope only.
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	a := e.savePull(p, sel(key1, pr))
	e.clock.Advance(time.Minute)
	b := e.savePull(p, sel(key2, pr))
	e.clock.Advance(time.Minute)
	c := e.savePull(p, sel(zero, pr))
	if e.has(a) || !e.has(b) || !e.has(c) {
		t.Fatalf("a full scope deletes its least recently used disk: a %v, b %v, c %v", e.has(a), e.has(b), e.has(c))
	}
	if !e.has(m1) || !e.has(m2) {
		t.Fatal("a pull request's save pushed main's disks out")
	}
	// Another pull request finds the pull requests' limit full of the
	// first's, and saves nothing.
	q, qrun := e.pull()
	before := e.images()
	e.start(qrun, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "pr"+strconv.Itoa(q.Number))}}).finish(true)
	if len(e.images()) != len(before) || !e.logged("work disk not saved: it would pass its scope's size limit") {
		t.Fatal("a save past its limit wasn't skipped, or the service didn't say why")
	}

	// Main makes room too, but keeps the newest of each family other than
	// the one it saves.
	e.clock.Advance(time.Minute)
	t1 := e.saveMainLabel(tools(key1, "main"))
	if e.has(m1) || !e.has(m2) || !e.has(t1) {
		t.Fatalf("main's save: m1 %v, m2 %v, tools %v; want the old build disk deleted", e.has(m1), e.has(m2), e.has(t1))
	}
	e.clock.Advance(time.Minute)
	t2 := e.saveMainLabel(tools(key2, "main"))
	if !e.has(m2) || e.has(t1) || !e.has(t2) {
		t.Fatalf("main's save: build %v, tools %v, %v; want build's newest kept", e.has(m2), e.has(t1), e.has(t2))
	}
}

// saveMain with a full label.
func (e *env) saveMainLabel(label string) string {
	e.t.Helper()
	before := e.images()
	e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{label}}).finish(true)
	return e.saved(before).Name
}

func TestAFailedDeletionDoesntMakeRoom(t *testing.T) {
	e := setup(t)
	e.cfg.Limits.Defaults.MainDisksGB = 100
	m1 := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	m2 := e.saveMain(key2)
	e.clock.Advance(time.Minute)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{tools(key1, "main")}})
	e.gce.FailNextOperation("images.delete", "INTERNAL_ERROR")
	before := e.images()
	jb.finish(true)
	// The disk making room was marked, but its deletion failed: the save
	// waits, and the pass's sweep of marked images tries the deletion
	// again. The limit is never passed.
	for n := range e.images() {
		if _, ok := before[n]; !ok {
			t.Fatalf("images %v; a save went ahead of the deletion that made room for it", e.images())
		}
	}
	e.pass()
	if e.has(m1) || !e.has(m2) || len(e.images()) != 2 {
		t.Fatalf("images %v; want the old disk deleted, then the save", e.images())
	}
}

func TestAPreemptedJobThatSucceededLetsItsRunsChoiceGo(t *testing.T) {
	e := setup(t)
	img := e.saveMain(key1)
	run := e.mainRun()
	label := "lanescape-2vcpu-linux-amd64-spot-disk-build-" + key1 + "-main"
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{label}})
	if jb.source() != img {
		t.Fatal("no restore")
	}
	// Preempted after its last step, with GitHub's success recorded.
	if err := e.gce.Preempt(jb.in.Zone, jb.in.Name); err != nil {
		t.Fatal(err)
	}
	e.gh.CompleteJobByID(jb.j.ID, "success")
	e.pass()
	if e.attempt(run) != 1 {
		t.Fatal("a run whose preempted job succeeded was rerun")
	}
	if _, ok := e.record("choices/5001/" + strconv.FormatInt(run.ID, 10) + "/" + selector.Hash(label) + ".json"); ok {
		t.Fatal("a finished run due no rerun kept its choice")
	}
}

// A run that lost a Spot job may be due a rerun that needs its choices,
// even before a pass has recorded the stopped machine: a sibling's save in
// the same pass, whichever machine it comes to first, doesn't evict what
// the run chose.
func TestASaveDoesntEvictWhatARunDueARerunChose(t *testing.T) {
	e := setup(t)
	e.cfg.Limits.Defaults.MainDisksGB = 50
	m1 := e.saveMain(key1)
	run := e.mainRun()
	saver := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key2, "main")}})
	lost := e.start(run, ghemu.JobSpec{Name: "test", Labels: []string{"lanescape-2vcpu-linux-amd64-spot-disk-build-" + key1 + "-main"}})
	if saver.source() != m1 || lost.source() != m1 {
		t.Fatal("the run's jobs didn't restore main's disk")
	}
	if err := e.gce.Preempt(lost.in.Zone, lost.in.Name); err != nil {
		t.Fatal(err)
	}
	e.gh.CompleteJobByID(lost.j.ID, "failure")
	saver.finish(true)
	if !e.has(m1) {
		t.Fatal("a save evicted the disk that a run due a rerun chose")
	}
	if e.attempt(run) != 2 {
		t.Fatal("the run wasn't rerun")
	}
}

// A record of an image that's gone counts nothing against a limit.
func TestAGoneImageDoesntCount(t *testing.T) {
	e := setup(t)
	e.cfg.Limits.Defaults.PullRequestDisksGB = 50
	p, _ := e.pull()
	gone := e.savePull(p, sel(key1, "pr"+strconv.Itoa(p.Number)))
	e.gce.DeleteImageNow(gone)
	q, _ := e.pull()
	if img := e.savePull(q, sel(key1, "pr"+strconv.Itoa(q.Number))); !e.has(img) {
		t.Fatal("no save")
	}
}
