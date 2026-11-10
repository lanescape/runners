package disks_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"

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

// A pull request that closes, merged or not, has its whole scope deleted
// at once, and its most recent choices with it; reopened, it starts again
// from main.
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
	recent := "recent/5001/" + pr + "/" + selector.Hash("amd64\x00build") + ".json"
	if _, ok := e.record(recent); !ok {
		t.Fatal("no record of the pull request's most recent choice")
	}
	e.gh.ClosePull(p)
	e.pass()
	if e.has(closed) || e.has(other) {
		t.Fatal("a closed pull request's disks outlived it")
	}
	if _, ok := e.record(recent); ok {
		t.Fatal("a closed pull request's most recent choice was kept")
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

// Main keeps the two newest keys of each family, and the versions of
// neither older one.
func TestMainKeepsItsTwoNewestKeys(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	tool := e.saveMainLabel(tools(key1, "main"))
	e.clock.Advance(time.Minute)
	k2 := e.saveMain(key2)
	e.clock.Advance(time.Minute)
	if !e.has(k1) || !e.has(k2) {
		t.Fatal("main lost one of its family's two newest keys")
	}
	k3 := e.saveMain(key3)
	if e.has(k1) || !e.has(k2) || !e.has(k3) || !e.has(tool) {
		t.Fatalf("key1 %v, key2 %v, key3 %v, tools %v; want only the oldest key of build deleted", e.has(k1), e.has(k2), e.has(k3), e.has(tool))
	}
}

// An older key of main's stays while an open pull request's most recent
// choice names it, after the run that chose it has finished, and goes once
// the pull request closes.
func TestMainKeepsAKeyAnOpenPullRequestChose(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	p, run := e.pull()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "pr"+strconv.Itoa(p.Number))}})
	if jb.source() != k1 {
		t.Fatal("no restore")
	}
	jb.finish(false)
	e.eventually("the run is finished", func() bool { s, _ := e.gh.RunState(run); return s == "completed" })
	e.clock.Advance(time.Minute)
	e.saveMain(key2)
	e.clock.Advance(time.Minute)
	e.saveMain(key3)
	e.pass()
	if !e.has(k1) {
		t.Fatal("main's key that an open pull request's most recent choice names was deleted")
	}
	e.gh.ClosePull(p)
	e.pass()
	if e.has(k1) {
		t.Fatal("main's older key outlived the pull request that chose it")
	}
}

// A family of main's that no one has used for 90 days goes, newest
// included; one in use stays.
func TestARetiredFamilyGoes(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	newest := e.saveMain(key2)
	tool := e.saveMainLabel(tools(key1, "main"))
	e.clock.Advance(disks.MainUnused - 24*time.Hour)
	used := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{tools(key1, "main")}})
	if used.source() != tool {
		t.Fatal("no restore")
	}
	used.finish(false)
	e.clock.Advance(2 * 24 * time.Hour)
	e.pass()
	if e.has(old) || e.has(newest) {
		t.Fatal("a family unused for 90 days was kept")
	}
	if !e.has(tool) {
		t.Fatal("a family in use went")
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
	if _, ok := e.record("choices/5001/" + strconv.FormatInt(run.ID, 10) + "/" + selector.Hash(label) + ".json"); ok {
		t.Fatal("a finished run due no rerun kept its choice")
	}
}

// A run that lost a Spot job may be due a rerun that needs its choices,
// even before a pass has recorded the stopped machine: a sibling's save in
// the same pass, whichever machine it comes to first, doesn't delete what
// the run chose, even once it's no longer among its family's newest keys.
func TestASaveDoesntDeleteWhatARunDueARerunChose(t *testing.T) {
	e := setup(t)
	m1 := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	e.saveMain(key3)
	e.clock.Advance(time.Minute)
	run := e.mainRun()
	saver := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key2, "main")}})
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

// A closed pull request's most recent choice goes even when its consumer
// has no saved disks at all.
func TestAClosedPullRequestsChoiceGoesWithNoImages(t *testing.T) {
	e := setup(t)
	p, run := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	if src := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}}).source(); src != "" {
		t.Fatalf("restored %q from nothing", src)
	}
	recent := "recent/5001/" + pr + "/" + selector.Hash("amd64\x00build") + ".json"
	if rec, ok := e.record(recent); !ok || rec["step"] != disks.StepNone {
		t.Fatalf("recent choice = %v", rec)
	}
	e.gh.ClosePull(p)
	e.pass()
	if _, ok := e.record(recent); ok {
		t.Fatal("a closed pull request's most recent choice was kept")
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

// A family of main's counts as used when any version of it was, even one
// that's since been deleted as superseded.
func TestAFamilysUseOutlivesTheVersionUsed(t *testing.T) {
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
	e.clock.Advance(31 * 24 * time.Hour)
	e.pass()
	if !e.has(newer) {
		t.Fatal("a family used 31 days ago was deleted as retired")
	}
	e.clock.Advance(60 * 24 * time.Hour)
	e.pass()
	if e.has(newer) {
		t.Fatal("a family unused for 90 days was kept")
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

// A family's retirement is decided again under the lease: a use of one of
// its keys during a pass keeps every key of it.
func TestAFamilyUsedDuringAPassIsKept(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	k2 := e.saveMain(key2)
	e.clock.Advance(disks.MainUnused + time.Hour)
	family := "families/5001/" + selector.Hash("main\x00amd64\x00build") + ".json"
	var once sync.Once
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Match: func(r *http.Request) bool {
		if strings.HasPrefix(r.URL.Query().Get("name"), "leases/disks/") {
			// A job chooses key1 after the pass has read the family's
			// records, and before it marks anything.
			once.Do(func() {
				b, _ := json.Marshal(disks.Family{RepositoryID: repoID, Scope: "main", Arch: "amd64", Family: "build", LastUsed: e.clock.Now().UTC()})
				e.gcs.Put("state", family, b)
			})
		}
		return false
	}})
	e.pass()
	if !e.has(k1) || !e.has(k2) {
		t.Fatalf("key1 %v, key2 %v; a family used during the pass was retired", e.has(k1), e.has(k2))
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
