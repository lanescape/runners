package disks_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"

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

// Nothing goes for want of use or for its age: an open pull request's disk,
// main's and a long-lived branch's stay, however long no one uses them.
func TestNothingGoesForWantOfUseOrAge(t *testing.T) {
	e := setup(t)
	e.listV1()
	m := e.saveMain(key1)
	b := e.saveBranch(key1)
	p, _ := e.pull()
	pr := e.savePull(p, sel(key1, "pr"+strconv.Itoa(p.Number)))
	e.clock.Advance(1000 * 24 * time.Hour)
	e.pass()
	if !e.has(m) || !e.has(b) || !e.has(pr) {
		t.Fatalf("main's %v, the branch's %v, the open pull request's %v; want every one kept", e.has(m), e.has(b), e.has(pr))
	}
	if _, ok := e.images()[m].Labels["lsr-last-used"]; ok {
		t.Fatal("a saved disk labelled with its last use")
	}
}

// A pull request that closes, merged or not, has its whole scope deleted
// at once; reopened, it starts again from main.
func TestAClosedPullRequestsDisksGoAtOnce(t *testing.T) {
	e := setup(t)
	m := e.saveMain(key1)
	p, run := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}})
	if src := jb.source(); src != m {
		t.Fatalf("restored %q, want main's %s", src, m)
	}
	jb.finish(false)
	closed := e.savePull(p, sel(key2, pr))
	other := e.savePull(p, tools(key2, pr))
	e.gh.ClosePull(p)
	e.pass()
	if e.has(closed) || e.has(other) {
		t.Fatal("a closed pull request's disks outlived it")
	}
	if !e.has(m) {
		t.Fatal("main's disk went with the pull request")
	}
	e.gh.ReopenPull(p)
	if src := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key2, pr)}}).source(); src != m {
		t.Fatalf("a reopened pull request restored %q, want main's %s", src, m)
	}
}

// A pull request keeps one disk of each family: saving one replaces its
// others of that family, whatever their key, and no other family's.
func TestAPullRequestKeepsOneDiskOfEachFamily(t *testing.T) {
	e := setup(t)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	first := e.savePull(p, sel(key1, pr))
	tool := e.savePull(p, tools(key1, pr))
	e.clock.Advance(time.Minute)
	second := e.savePull(p, sel(key2, pr))
	if e.has(first) || !e.has(second) || !e.has(tool) {
		t.Fatalf("first %v, second %v, tools %v; want the family's earlier disk replaced", e.has(first), e.has(second), e.has(tool))
	}
	// The restore order is unchanged: the family's one disk is its newest.
	if src := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}}).source(); src != second {
		t.Fatalf("restored %q, want the pull request's newest %s", src, second)
	}
}

// A save under a new key replaces main's disks of the family's older keys,
// as it replaces older versions of its own, and no other family's
// (specs/0003-disks.md, Deletion).
func TestANewKeysSaveReplacesTheOldKeys(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	tool := e.saveMainLabel(tools(key1, "main"))
	e.clock.Advance(time.Minute)
	again := e.saveMain(key1)
	if e.has(k1) || !e.has(again) {
		t.Fatal("a newer version of a key didn't replace the older")
	}
	e.clock.Advance(time.Minute)
	k2 := e.saveMain(key2)
	e.pass()
	if e.has(again) || !e.has(k2) || !e.has(tool) {
		t.Fatalf("key1 %v, key2 %v, tools %v; want the old key's disk replaced, and no other family's", e.has(again), e.has(k2), e.has(tool))
	}
	// A pull request still of the old key starts from main's newest.
	p, run := e.pull()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "pr"+strconv.Itoa(p.Number))}})
	if src := jb.source(); src != k2 {
		t.Fatalf("a pull request of the old key restored %q, want main's newest %s", src, k2)
	}
}

// An open pull request that chose main's disk of an older key keeps it only
// while its run is unfinished: once a newer key's save replaces it, it goes
// as soon as that run has finished, the pull request still open.
func TestAnOpenPullRequestKeepsNoOlderKeyOfMains(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	p, run := e.pull()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "pr"+strconv.Itoa(p.Number))}})
	if jb.source() != k1 {
		t.Fatal("no restore")
	}
	e.clock.Advance(time.Minute)
	e.saveMain(key2)
	if !e.has(k1) {
		t.Fatal("a newer key's save deleted the disk an unfinished run's choice names")
	}
	jb.finish(false)
	e.eventually("the run is finished", func() bool { s, _ := e.gh.RunState(run); return s == "completed" })
	e.pass()
	if e.has(k1) {
		t.Fatal("main's older key outlived the run that chose it, kept for its open pull request")
	}
}

// saveMain with a full label.
func (e *env) saveMainLabel(label string) string {
	e.t.Helper()
	before := e.images()
	e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{label}}).finish(true)
	return e.saved(before).Name
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
	if _, ok := e.record("choices/5001/" + strconv.FormatInt(run.ID, 10) + "/" + choiceHash(label) + ".json"); ok {
		t.Fatal("a finished run due no rerun kept its choice")
	}
}

// A run that lost a Spot job may be due a rerun that needs its choices,
// even before a pass has recorded the stopped machine: a sibling's save in
// the same pass, whichever machine it comes to first, doesn't delete what
// the run chose, even though the save, of a newer key, replaces it.
func TestASaveDoesntDeleteWhatARunDueARerunChose(t *testing.T) {
	e := setup(t)
	m1 := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	run := e.mainRun()
	// Another push saves the family's new key; this run's lost job chose
	// the old.
	saver := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key2, "main")}})
	lost := e.start(run, ghemu.JobSpec{Name: "test", Labels: []string{"lanescape-2vcpu-linux-amd64-spot-disk-build-" + key1 + "-main"}})
	if lost.source() != m1 {
		t.Fatal("the run's lost job didn't restore main's disk")
	}
	if err := e.gce.Preempt(lost.in.Zone, lost.in.Name); err != nil {
		t.Fatal(err)
	}
	e.gh.CompleteJobByID(lost.j.ID, "failure")
	saver.finish(true)
	if !e.has(m1) {
		t.Fatal("a save deleted the disk that a run due a rerun chose")
	}
	if e.attempt(run) != 2 {
		t.Fatal("the run wasn't rerun")
	}
}

// A pull request closed while a run's choice holds one of its disks keeps
// the disk only for that run: reopened, its new runs start again from
// main's, and the disk goes once the run that chose it has finished.
func TestAReopenedPullRequestNeverRestoresWhatItsEarlierRunsSaved(t *testing.T) {
	e := setup(t)
	m := e.saveMain(key1)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	img := e.savePull(p, sel(key1, pr))
	jb := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}})
	if jb.source() != img {
		t.Fatal("no restore of the pull request's own disk")
	}
	e.gh.ClosePull(p)
	e.pass()
	if !e.has(img) {
		t.Fatal("a disk an unfinished run's choice names was deleted")
	}
	e.gh.ReopenPull(p)
	if src := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}}).source(); src != m {
		t.Fatalf("a reopened pull request restored %q, want main's %s", src, m)
	}
	jb.finish(false)
	e.pass()
	if e.has(img) {
		t.Fatal("a disk saved before its pull request closed outlived the run that chose it")
	}
}

// A superseded version stays while an unfinished run's choice names it,
// and goes once that run has finished; the newer one stays however long
// it goes unused.
func TestASupersededVersionGoesOnceNoChoiceNamesIt(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	run := e.mainRun()
	first := e.start(run, ghemu.JobSpec{Name: "a", Labels: []string{sel(key1, "main")}})
	if first.source() != old {
		t.Fatal("no restore")
	}
	e.clock.Advance(time.Minute)
	newer := e.saveMain(key1)
	// Weeks later, the run's next job uses the old version, by its choice.
	e.clock.Advance(60 * 24 * time.Hour)
	second := e.start(run, ghemu.JobSpec{Name: "b", Labels: []string{sel(key1, "main")}})
	if second.source() != old {
		t.Fatal("the run's second job didn't start from its run's choice")
	}
	// The first job's machine went past its age limit long ago.
	e.gh.CompleteJobByID(first.j.ID, "success")
	second.finish(false)
	e.eventually("the run is finished", func() bool { s, _ := e.gh.RunState(run); return s == "completed" })
	e.pass()
	if e.has(old) {
		t.Fatal("a superseded version outlived the choice that named it")
	}
	e.clock.Advance(1000 * 24 * time.Hour)
	e.pass()
	if !e.has(newer) {
		t.Fatal("a family's newest disk went for want of use")
	}
}

// A save by a run bound before its pull request closed replaces nothing a
// run after its reopening saved, and goes itself.
func TestARetiredRunsLateSaveReplacesNothing(t *testing.T) {
	e := setup(t)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	e.savePull(p, sel(key1, pr))
	late := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key2, pr)}})
	e.gh.ClosePull(p)
	e.pass()
	e.gh.ReopenPull(p)
	e.clock.Advance(time.Minute)
	fresh := e.savePull(p, sel(key1, pr))
	e.clock.Advance(time.Minute)
	late.finish(true)
	if !e.logged("saved by a run from before its pull request closed") {
		t.Fatal("the retired run's late save wasn't made and deleted")
	}
	if !e.has(fresh) {
		t.Fatal("a retired run's late save replaced the reopened pull request's disk")
	}
	// The pull request's first disk, which the late run's choice kept, goes
	// too, replaced by the reopened pull request's.
	if len(e.images()) != 1 {
		t.Fatalf("images %v; want only the reopened pull request's disk", e.images())
	}
}

// A pull request's closing is recorded from GitHub's delivery, so one closed
// and reopened before any pass saw it closed still starts again from main's.
func TestAClosingIsRecordedFromItsDelivery(t *testing.T) {
	e := setup(t)
	m := e.saveMain(key1)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	img := e.savePull(p, sel(key1, pr))
	e.gh.ClosePull(p)
	closedAt := e.clock.Now().UTC().Add(time.Second)
	e.gh.ReopenPull(p)
	delivery, _ := json.Marshal(map[string]any{"action": "closed",
		"pull_request": map[string]any{"number": p.Number, "closed_at": closedAt.Format(time.RFC3339)},
		"repository":   map[string]any{"id": repoID, "owner": map[string]any{"id": orgID}}})
	if err := e.disks.RecordEvent(context.Background(), map[string]string{"event": "pull_request"}, delivery); err != nil {
		t.Fatal(err)
	}
	// Another organization's delivery records nothing.
	other, _ := json.Marshal(map[string]any{"action": "closed",
		"pull_request": map[string]any{"number": 99, "closed_at": closedAt.Format(time.RFC3339)},
		"repository":   map[string]any{"id": repoID, "owner": map[string]any{"id": 1}}})
	if err := e.disks.RecordEvent(context.Background(), map[string]string{"event": "pull_request"}, other); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.record("closures/5001/pr99.json"); ok {
		t.Fatal("another organization's closing was recorded")
	}
	e.clock.Advance(time.Minute)
	if src := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}}).source(); src != m {
		t.Fatalf("a pull request reopened before a pass restored %q, want main's %s", src, m)
	}
	if e.has(img) {
		t.Fatal("a disk saved before its pull request's closing outlived it")
	}
}

// A closure only moves forward: a pass that sees a pull request closed never
// moves back the time a later closing's delivery recorded.
func TestAClosureOnlyMovesForward(t *testing.T) {
	e := setup(t)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	e.savePull(p, sel(key1, pr))
	e.gh.ClosePull(p)
	later := e.clock.Now().UTC().Add(time.Hour).Truncate(time.Second)
	delivery, _ := json.Marshal(map[string]any{"action": "closed",
		"pull_request": map[string]any{"number": p.Number, "closed_at": later.Format(time.RFC3339)},
		"repository":   map[string]any{"id": repoID, "owner": map[string]any{"id": orgID}}})
	if err := e.disks.RecordEvent(context.Background(), map[string]string{"event": "pull_request"}, delivery); err != nil {
		t.Fatal(err)
	}
	e.pass()
	rec, ok := e.record("closures/5001/" + pr + ".json")
	if !ok {
		t.Fatal("no closure recorded")
	}
	if at, _ := time.Parse(time.RFC3339Nano, rec["seen_at"].(string)); !at.Equal(later.Add(time.Second)) {
		t.Fatalf("closure seen at %s, want the delivery's %s", at, later.Add(time.Second))
	}
}

// A closing recorded after a pass has read the closures, while it decides
// what's replaced, keeps a disk a reopened pull request saved that a retired
// run's late save would otherwise replace.
func TestAClosingDuringAPassKeepsWhatARetiredSaveWouldReplace(t *testing.T) {
	e := setup(t)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	// A run bound before the pull request closes, and saves late.
	late := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key2, pr)}})
	closed := e.clock.Now().UTC().Add(30 * time.Second)
	e.clock.Advance(time.Minute)
	// After it's reopened, a run saves, and another holds that disk.
	fresh := e.savePull(p, sel(key1, pr))
	holder := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}})
	if holder.source() != fresh {
		t.Fatal("no restore")
	}
	e.clock.Advance(time.Minute)
	late.finish(true)
	if !e.has(fresh) {
		t.Fatal("a late save replaced a disk an unfinished run's choice names")
	}
	// The closing's delivery is recorded once the next pass has read its
	// records: here, as it first takes the disks' lease.
	var done atomic.Bool
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Match: func(r *http.Request) bool {
		if strings.HasPrefix(r.URL.Query().Get("name"), "leases/disks/") && done.CompareAndSwap(false, true) {
			b, _ := json.Marshal(disks.Closure{RepositoryID: repoID, Scope: pr, SeenAt: closed})
			e.gcs.Put("state", "closures/5001/"+pr+".json", b)
		}
		return false
	}})
	holder.finish(false)
	if !done.Load() {
		t.Fatal("the pass took no lease")
	}
	if !e.has(fresh) {
		t.Fatal("a retired run's late save replaced the reopened pull request's disk")
	}
	e.pass()
	if !e.has(fresh) || len(e.images()) != 1 {
		t.Fatalf("images %v; want only the reopened pull request's disk", e.images())
	}
}
