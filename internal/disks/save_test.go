package disks_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/disks"
)

// save runs a job of a run that saves its cache, and returns the version,
// which it expects ready once the job has finished.
func (e *env) save(run *ghemu.Run, label string) string {
	e.t.Helper()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{label}})
	jb.finish(true)
	if jb.saved == "" {
		e.t.Fatalf("a job of %s saved nothing", label)
	}
	if !e.ready(jb.saved) {
		rec, _ := e.record("caches/5001/" + jb.saved + ".json")
		e.t.Fatalf("version %s = %v; want it ready", jb.saved, rec)
	}
	return jb.saved
}

// saveMain runs a main job that saves its cache, and returns the version.
func (e *env) saveMain(key string) string {
	e.t.Helper()
	return e.save(e.mainRun(), sel(key, "main"))
}

// saveMainLabel is saveMain with a full label.
func (e *env) saveMainLabel(label string) string {
	e.t.Helper()
	return e.save(e.mainRun(), label)
}

// ready reports whether a version is recorded ready, with its objects in
// the caches' bucket.
func (e *env) ready(version string) bool {
	rec, ok := e.record("caches/5001/" + version + ".json")
	return ok && rec["state"] == disks.ImageReady && e.has(version)
}

func TestSavingAndRestoring(t *testing.T) {
	e := setup(t)
	// Nothing saved yet: nothing is restored, and the work disk starts
	// empty.
	first := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if v, src := first.restored(), first.source(); v != "" || src != "" || first.restore.Restored != disks.StepNone {
		t.Fatalf("the first job restored %q, %+v, its disk from %q", v, first.restore, src)
	}
	first.finish(true)
	mainVersion := first.saved
	vs := e.versions()
	if len(vs) != 1 || len(vs[mainVersion]) != 2 {
		t.Fatalf("versions = %v, want main's, with the objects its job uploaded", vs)
	}
	series := disks.Series("main", "amd64", "build", key1)
	if want := "lsr-5001-" + series + "-"; !strings.HasPrefix(mainVersion, want) || len(mainVersion) <= len(want) {
		t.Fatalf("version %s isn't named lsr-<repository>-<series>-<save time>-<machine>", mainVersion)
	}
	rec, ok := e.record("caches/5001/" + mainVersion + ".json")
	if !ok || rec["kind"] != disks.KindCache || rec["scope"] != "main" || rec["cache"] != "build" || rec["key"] != key1 ||
		rec["arch"] != "amd64" || rec["state"] != disks.ImageReady || rec["job_id"] != float64(first.j.ID) ||
		rec["run_id"] != float64(first.run.ID) || len(rec["objects"].([]any)) != 2 {
		t.Fatalf("version record = %v", rec)
	}
	// A cache is no image.
	if len(e.images()) != 0 {
		t.Fatalf("images %v; a cache's version made an image", e.images())
	}
	if !e.logged(`msg="cache saved"`) {
		t.Fatal("the save wasn't logged")
	}

	// A pull request reads main's, and saves its own.
	p, run := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}})
	if v := jb.restored(); v != mainVersion {
		t.Fatalf("a pull request restored %q, want main's %s", v, mainVersion)
	}
	jb.finish(true)
	prVersion := jb.saved
	if rec, _ := e.record("caches/5001/" + prVersion + ".json"); rec["scope"] != pr || !e.ready(prVersion) || !e.has(mainVersion) {
		t.Fatalf("the pull request's version = %v", rec)
	}
	// Its next run reads its own.
	again := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}})
	if v := again.restored(); v != prVersion {
		t.Fatalf("the pull request's next run restored %q, want its own %s", v, prVersion)
	}
	again.finish(false)

	// Nothing the pull request wrote reaches main, or another pull request.
	other, orun := e.pull()
	o := e.start(orun, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "pr"+strconv.Itoa(other.Number))}})
	if v := o.restored(); v != mainVersion {
		t.Fatalf("another pull request restored %q, want main's %s", v, mainVersion)
	}
	o.finish(false)
	m := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if v := m.restored(); v != mainVersion {
		t.Fatalf("main restored %q, want its own %s", v, mainVersion)
	}
	m.finish(false)
	// A read run reads main's, and saves nothing.
	rd := e.gh.EventRun(e.repo, "workflow_dispatch", "main")
	if s := e.bind(rd); s != "read" {
		t.Fatalf("a dispatch bound %s", s)
	}
	r := e.start(rd, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "read")}})
	if v := r.restored(); v != mainVersion {
		t.Fatalf("a read run restored %q", v)
	}
	if code, ans := r.askSave(); code != 200 || ans.Saves || !strings.Contains(ans.Reason, "read") {
		t.Fatalf("a read run's save: %d, %+v; want it told its scope saves nothing", code, ans)
	}
	before := len(e.versions())
	r.finish(true)
	if len(e.versions()) != before {
		t.Fatal("a read run saved a version")
	}
	// Caches are kept apart by architecture.
	arm := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{"lanescape-16vcpu-linux-arm64-cache-build-" + key1 + "-main"}})
	if v := arm.restored(); v != "" {
		t.Fatalf("an arm64 job restored %s, an amd64 version", v)
	}
	arm.finish(false)
}

func TestTheRestoreOrder(t *testing.T) {
	e := setup(t)
	main1 := e.saveMain(key1)
	p, run := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	// Main's for the key, then main's newest of the cache.
	if v := e.start(run, ghemu.JobSpec{Name: "a", Labels: []string{sel(key1, pr)}}).restored(); v != main1 {
		t.Fatalf("main's for the key: %s, want %s", v, main1)
	}
	e.clock.Advance(time.Minute)
	main2 := e.saveMain(key2)
	brun := e.pullRun(p)
	if v := e.start(brun, ghemu.JobSpec{Name: "b", Labels: []string{sel(zero, pr)}}).restored(); v != main2 {
		t.Fatalf("main's newest: %s, want %s", v, main2)
	}
	// The pull request saves for the fixed key, an ordinary one.
	prZero := e.save(e.pullRun(p), sel(zero, pr))
	// Its own newest comes before main's for the key.
	drun := e.pullRun(p)
	d := e.start(drun, ghemu.JobSpec{Name: "d", Labels: []string{sel(key1, pr)}})
	if v := d.restored(); v != prZero {
		t.Fatalf("the pull request's newest: %s, want %s", v, prZero)
	}
	if d.restore.Restored != disks.StepPullRequestNewest {
		t.Fatalf("the restore's answer says %s, want %s", d.restore.Restored, disks.StepPullRequestNewest)
	}
	// Each choice records the step of the order it came from, with its
	// cache and scope, and the service logs it.
	for _, c := range []struct {
		run         *ghemu.Run
		label, step string
	}{
		{run, sel(key1, pr), disks.StepMainExact},
		{brun, sel(zero, pr), disks.StepMainNewest},
		{drun, sel(key1, pr), disks.StepPullRequestNewest},
	} {
		rec, ok := e.record(e.choice(c.run, c.label))
		if !ok || rec["step"] != c.step || rec["cache"] != "build" || rec["scope"] != pr || rec["arch"] != "amd64" {
			t.Fatalf("choice for %s = %v, want step %s", c.label, rec, c.step)
		}
		if !e.logged(`msg="restore choice"`) || !e.logged("step="+c.step) {
			t.Fatalf("the service didn't log the choice's step %s", c.step)
		}
	}
	// So does a choice of nothing.
	none := e.mainRun()
	label := "lanescape-2vcpu-linux-amd64-cache-other-" + key1 + "-main"
	e.start(none, ghemu.JobSpec{Name: "e", Labels: []string{label}})
	if rec, _ := e.record(e.choice(none, label)); rec["step"] != disks.StepNone || rec["image"] != nil {
		t.Fatalf("a choice of nothing = %v", rec)
	}
}

func TestAChoiceIsOncePerRunCacheAndArch(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	run := e.mainRun()
	a := e.start(run, ghemu.JobSpec{Name: "a", Labels: []string{sel(key1, "main")}})
	if a.restored() != old {
		t.Fatal("the first job didn't restore main's version")
	}
	// A newer version is saved while the run goes on.
	e.clock.Advance(time.Minute)
	newer := e.saveMain(key1)
	if newer == old {
		t.Fatal("no new version")
	}
	if !e.has(old) || !e.recorded(old) {
		t.Fatal("an older version that an unfinished run's choice names was deleted")
	}
	b := e.start(run, ghemu.JobSpec{Name: "b", Labels: []string{sel(key1, "main")}})
	if b.restored() != old {
		t.Fatalf("the run's second job restored %s, not its run's choice %s", b.restored(), old)
	}
	// Another cache, or another architecture, is another choice.
	tool := e.start(run, ghemu.JobSpec{Name: "c", Labels: []string{tools(key1, "main")}})
	arm := e.start(run, ghemu.JobSpec{Name: "d", Labels: []string{armBuild(key1)}})
	if tool.restored() != "" || arm.restored() != "" {
		t.Fatalf("tools restored %q, arm64 %q; want nothing", tool.restored(), arm.restored())
	}
	// Another run picks the newest.
	if v := e.start(e.mainRun(), ghemu.JobSpec{Name: "c", Labels: []string{sel(key1, "main")}}).restored(); v != newer {
		t.Fatalf("a new run restored %s, want %s", v, newer)
	}
	// Once the run has finished, its choice goes, and the old version
	// with it, its objects and then its record.
	for _, jb := range []*job{a, b, tool, arm} {
		jb.finish(false)
	}
	e.eventually("the run is finished", func() bool { s, _ := e.gh.RunState(run); return s == "completed" })
	e.pass()
	if e.has(old) || e.recorded(old) {
		t.Fatal("a superseded version outlived the choice that named it")
	}
	if _, ok := e.record(e.choice(run, sel(key1, "main"))); ok {
		t.Fatal("a finished run's choice was kept")
	}
}

// specs/0003-disks.md, Caches: a version is restorable only once GitHub
// concludes its job succeeded; a failed job's version goes, objects and
// record.
func TestOnlyASuccessfulJobSaves(t *testing.T) {
	e := setup(t)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}, Steps: "exit 1"})
	jb.finish(true)
	if jb.saved == "" {
		t.Fatal("the job saved nothing")
	}
	if e.has(jb.saved) || e.recorded(jb.saved) {
		t.Fatal("a failed job's version was kept")
	}
	if !e.logged("its job didn't succeed") {
		t.Fatal("no reason logged")
	}
	if v := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}).restored(); v != "" {
		t.Fatalf("a failed job's version was restored: %s", v)
	}
}

// A maintainer's rerun picks again, as a new run would, even before a pass
// has dropped the run's choices; an automatic rerun keeps them.
func TestAMaintainersRerunPicksAgain(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	run := e.mainRun()
	first := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if first.restored() != old {
		t.Fatal("no restore")
	}
	e.clock.Advance(time.Minute)
	newer := e.saveMain(key1)
	// The run finishes, and a maintainer reruns it at once.
	e.gh.CompleteJobByID(first.j.ID, "success")
	e.gh.Rerun(run)
	if v := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}).restored(); v != newer {
		t.Fatalf("a maintainer's rerun restored %q, want the newest %s", v, newer)
	}
}
