package disks_test

import (
	"strconv"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/disks"
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
