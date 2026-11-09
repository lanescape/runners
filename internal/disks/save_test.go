package disks_test

import (
	"strconv"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/selector"
)

// saveMain runs a main job that saves its disk, and returns the image.
func (e *env) saveMain(key string) string {
	e.t.Helper()
	before := e.images()
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key, "main")}})
	jb.finish(true)
	return e.saved(before).Name
}

func TestSavingAndRestoring(t *testing.T) {
	e := setup(t)
	// Nothing saved yet: the work disk starts empty.
	first := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if src := first.source(); src != "" {
		t.Fatalf("the first job's disk came from %s", src)
	}
	first.finish(true)
	imgs := e.images()
	if len(imgs) != 1 {
		t.Fatalf("images = %v, want main's saved disk", imgs)
	}
	var mainImage string
	for n, im := range imgs {
		mainImage = n
		series := disks.Series("main", "amd64", "build", key1)
		if want := "lsr-5001-" + series + "-"; len(n) <= len(want) || n[:len(want)] != want {
			t.Fatalf("image %s isn't named lsr-<repository>-<series>-<save time>", n)
		}
		if im.Labels[disks.LabelRepository] != "5001" || im.Labels[disks.LabelKind] != disks.KindSaved ||
			im.Labels[disks.LabelScope] != selector.Hash("main") || im.Labels[disks.LabelFamily] != selector.Hash("build") ||
			im.Labels[disks.LabelKey] != selector.Hash(key1) || im.Labels[disks.LabelArch] != "amd64" ||
			im.Labels[disks.LabelSaved] == "" || im.Labels[disks.LabelLastUsed] == "" || im.SourceDisk != first.in.Name+"-work" {
			t.Fatalf("image = %+v", im)
		}
	}
	rec, ok := e.record("images/5001/" + mainImage + ".json")
	if !ok || rec["scope"] != "main" || rec["family"] != "build" || rec["key"] != key1 || rec["state"] != disks.ImageReady {
		t.Fatalf("image record = %v", rec)
	}

	// A pull request reads main's, and saves its own.
	p, run := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}})
	if src := jb.source(); src != mainImage {
		t.Fatalf("a pull request's disk came from %q, want main's %s", src, mainImage)
	}
	before := e.images()
	jb.finish(true)
	prImage := e.saved(before)
	if prImage.Labels[disks.LabelScope] != selector.Hash(pr) {
		t.Fatalf("the pull request's disk = %+v", prImage)
	}
	// Its next run reads its own.
	again := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}})
	if src := again.source(); src != prImage.Name {
		t.Fatalf("the pull request's next run's disk came from %q, want its own %s", src, prImage.Name)
	}
	again.finish(false)

	// Nothing the pull request wrote reaches main, or another pull request.
	other, orun := e.pull()
	o := e.start(orun, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "pr"+strconv.Itoa(other.Number))}})
	if src := o.source(); src != mainImage {
		t.Fatalf("another pull request's disk came from %q, want main's %s", src, mainImage)
	}
	o.finish(false)
	m := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if src := m.source(); src != mainImage {
		t.Fatalf("main's disk came from %q, want its own %s", src, mainImage)
	}
	m.finish(false)
	// A read run reads main's, and saves nothing.
	rd := e.gh.EventRun(e.repo, "workflow_dispatch", "main")
	if s := e.bind(rd); s != "read" {
		t.Fatalf("a dispatch bound %s", s)
	}
	r := e.start(rd, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "read")}})
	if src := r.source(); src != mainImage {
		t.Fatalf("a read run's disk came from %q", src)
	}
	before = e.images()
	r.finish(true)
	if len(e.images()) != len(before) {
		t.Fatal("a read run saved a disk")
	}
	// Families are kept apart by architecture.
	arm := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{"lanescape-16vcpu-linux-arm64-disk-build-" + key1 + "-main"}})
	if src := arm.source(); src != "" {
		t.Fatalf("an arm64 job's disk came from %s, an amd64 disk", src)
	}
	arm.finish(false)
}

func TestTheRestoreOrder(t *testing.T) {
	e := setup(t)
	main1 := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	main2 := e.saveMain(key2)
	p, run := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	// Main's for the key, then main's newest for the family.
	if src := e.start(run, ghemu.JobSpec{Name: "a", Labels: []string{sel(key1, pr)}}).source(); src != main1 {
		t.Fatalf("main's for the key: %s, want %s", src, main1)
	}
	if src := e.start(run, ghemu.JobSpec{Name: "b", Labels: []string{sel(zero, pr)}}).source(); src != main2 {
		t.Fatalf("main's newest: %s, want %s", src, main2)
	}
	// The pull request saves for the fixed key, an ordinary one.
	before := e.images()
	e.start(e.pullRun(p), ghemu.JobSpec{Name: "c", Labels: []string{sel(zero, pr)}}).finish(true)
	prZero := e.saved(before).Name
	// Its own newest comes before main's for the key.
	if src := e.start(e.pullRun(p), ghemu.JobSpec{Name: "d", Labels: []string{sel(key1, pr)}}).source(); src != prZero {
		t.Fatalf("the pull request's newest: %s, want %s", src, prZero)
	}
}

func TestAChoiceIsOncePerRunAndSelector(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	run := e.mainRun()
	a := e.start(run, ghemu.JobSpec{Name: "a", Labels: []string{sel(key1, "main")}})
	if a.source() != old {
		t.Fatal("the first job didn't restore main's disk")
	}
	// A newer version is saved while the run goes on.
	e.clock.Advance(time.Minute)
	newer := e.saveMain(key1)
	if newer == old {
		t.Fatal("no new version")
	}
	if _, ok := e.images()[old]; !ok {
		t.Fatal("an older version that an unfinished run's choice names was deleted")
	}
	b := e.start(run, ghemu.JobSpec{Name: "b", Labels: []string{sel(key1, "main")}})
	if b.source() != old {
		t.Fatalf("the run's second job started from %s, not its run's choice %s", b.source(), old)
	}
	// Another run picks the newest.
	if src := e.start(e.mainRun(), ghemu.JobSpec{Name: "c", Labels: []string{sel(key1, "main")}}).source(); src != newer {
		t.Fatalf("a new run started from %s, want %s", src, newer)
	}
	// Once the run has finished, its choice goes, and the old version
	// with it.
	a.finish(false)
	b.finish(false)
	e.eventually("the run is finished", func() bool { s, _ := e.gh.RunState(run); return s == "completed" })
	e.pass()
	if _, ok := e.images()[old]; ok {
		t.Fatal("a superseded version outlived the choice that named it")
	}
	if _, ok := e.record("choices/5001/" + strconv.FormatInt(run.ID, 10) + "/" + selector.Hash(sel(key1, "main")) + ".json"); ok {
		t.Fatal("a finished run's choice was kept")
	}
}

func TestOnlyASuccessfulJobSaves(t *testing.T) {
	e := setup(t)
	before := e.images()
	e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}, Steps: "exit 1"}).finish(true)
	if len(e.images()) != len(before) {
		t.Fatal("a failed job's disk was saved")
	}
	// Nor one with no family and key.
	e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{"lanescape-2vcpu-linux-amd64-main"}}).finish(true)
	if len(e.images()) != len(before) {
		t.Fatal("a disk with no family and key was saved")
	}
}

func TestARestoredDiskIsAtLeastItsImage(t *testing.T) {
	e := setup(t)
	img := e.saveMain(key1)
	// Main's shape is 50 GB; the image says how big its disk was.
	if e.images()[img].SizeGB != 50 {
		t.Fatalf("image = %+v", e.images()[img])
	}
	big := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if big.size() != 50 {
		t.Fatalf("work disk %d GB", big.size())
	}
}

// A maintainer's rerun picks again, as a new run would, even before a pass
// has dropped the run's choices; an automatic rerun keeps them.
func TestAMaintainersRerunPicksAgain(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	run := e.mainRun()
	first := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if first.source() != old {
		t.Fatal("no restore")
	}
	e.clock.Advance(time.Minute)
	newer := e.saveMain(key1)
	// The run finishes, and a maintainer reruns it at once.
	e.gh.CompleteJobByID(first.j.ID, "success")
	e.gh.Rerun(run)
	if src := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}).source(); src != newer {
		t.Fatalf("a maintainer's rerun started from %q, want the newest %s", src, newer)
	}
}
