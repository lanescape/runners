package disks_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/machines"
)

// hold takes the consumer's disks lease, as another step would, until the
// returned function releases it.
func (e *env) hold() func() {
	e.t.Helper()
	h, err := lease.New(e.objects, "leases/disks/5001", time.Minute, e.clock).Acquire(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return func() { _ = h.Release(context.Background()) }
}

func TestAChoiceWaitsForTheLease(t *testing.T) {
	e := setup(t)
	img := e.saveMain(key1)
	release := e.hold()
	run := e.mainRun()
	jb := e.take(run, e.queueJob(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}))
	if code := jb.askDisk(); code != http.StatusServiceUnavailable {
		t.Fatalf("a disk request while the disks' lease is held: %d, want a retryable 503", code)
	}
	release()
	if code := jb.askDisk(); code != http.StatusOK {
		t.Fatalf("the disk request once the lease is free: %d", code)
	}
	if jb.source() != img {
		t.Fatalf("restored %q, want %s", jb.source(), img)
	}
	// The run's next job reuses its choice, under the lease too.
	release = e.hold()
	next := e.take(run, e.queueJob(run, ghemu.JobSpec{Name: "test", Labels: []string{sel(key1, "main")}}))
	if code := next.askDisk(); code != http.StatusServiceUnavailable {
		t.Fatalf("reusing a choice while the disks' lease is held: %d, want a retryable 503", code)
	}
	release()
	if code := next.askDisk(); code != http.StatusOK || next.source() != img {
		t.Fatalf("reusing the choice: %d, %q", code, next.source())
	}
}

func TestADeletionThatFailedIsTriedAgain(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	e.gce.FailNextOperation("images.delete", "INTERNAL_ERROR")
	newer := e.saveMain(key1)
	if !e.has(old) {
		t.Fatal("the deletion didn't fail")
	}
	e.pass()
	if e.has(old) || !e.has(newer) {
		t.Fatal("a marked image whose deletion failed wasn't deleted on the next try")
	}
}

func TestMarkingWaitsForTheLease(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	// The save of a newer version can't mark the older one while the
	// lease is held; the save itself is done.
	release := e.hold()
	before := e.images()
	jb.finish(true)
	newer := e.saved(before).Name
	if !e.has(old) || !e.has(newer) {
		t.Fatalf("old %v, newer %v; want both while the lease is held", e.has(old), e.has(newer))
	}
	release()
	e.pass()
	if e.has(old) || !e.has(newer) {
		t.Fatalf("old %v, newer %v; want the older version deleted once the lease is free", e.has(old), e.has(newer))
	}
}

func TestAMarkedImageIsNeverChosen(t *testing.T) {
	e := setup(t)
	img := e.saveMain(key1)
	key := "images/5001/" + img + ".json"
	data, gen, _ := e.gcs.Object("state", key)
	var rec disks.Image
	_ = json.Unmarshal(data, &rec)
	rec.State = disks.ImageMarked
	b, _ := json.Marshal(rec)
	if _, err := e.objects.Replace(context.Background(), key, b, gen); err != nil {
		t.Fatal(err)
	}
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if src := jb.source(); src != "" {
		t.Fatalf("a marked image was chosen: %s", src)
	}
	// And the next pass deletes it.
	e.pass()
	if e.has(img) {
		t.Fatal("a marked image wasn't deleted")
	}
}

func TestOrphans(t *testing.T) {
	e := setup(t)
	img := e.saveMain(key1)
	// An image of the service's names with no record, as one would be if
	// its record were lost, goes.
	if err := e.objects.Delete(context.Background(), "images/5001/"+img+".json", mustGen(t, e, "images/5001/"+img+".json")); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if e.has(img) {
		t.Fatal("an orphaned image wasn't deleted")
	}
	// A record of an image that's gone goes too, and a run's choice of it
	// picks again.
	img = e.saveMain(key1)
	run := e.mainRun()
	if e.start(run, ghemu.JobSpec{Name: "a", Labels: []string{sel(key1, "main")}}).source() != img {
		t.Fatal("no restore")
	}
	e.gce.DeleteImageNow(img)
	if src := e.start(run, ghemu.JobSpec{Name: "b", Labels: []string{sel(key1, "main")}}).source(); src != "" {
		t.Fatalf("a choice of a deleted image restored %s", src)
	}
	e.pass()
	if _, ok := e.record("images/5001/" + img + ".json"); ok {
		t.Fatal("the record of a deleted image was kept")
	}
	// Base images are never the disks' to delete.
	for _, im := range e.gce.Images() {
		if im.Name == "lsr-base-amd64-1" {
			return
		}
	}
	t.Fatal("a base image was deleted")
}

func TestASaveNeedsAnAllowedAdmission(t *testing.T) {
	e := setup(t)
	if err := e.disks.Save(context.Background(), machines.Machine{}, machines.Admission{}); err != nil || len(e.images()) != 0 {
		t.Fatalf("a save with no admission: %v, images %v", err, e.images())
	}
}

func mustGen(t *testing.T, e *env, key string) int64 {
	t.Helper()
	_, gen, ok := e.gcs.Object("state", key)
	if !ok {
		t.Fatalf("no %s", key)
	}
	return gen
}

// An image whose making failed, and left a failed image behind, isn't a
// save: it's deleted and made again, and nothing is superseded meanwhile.
func TestAFailedImageIsMadeAgain(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	e.gce.FailNextOperation("images.insert.leave", "INTERNAL_ERROR")
	before := e.images()
	jb.finish(true)
	if !e.has(old) {
		t.Fatal("an older version went for a save that failed")
	}
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); !ok {
		t.Fatal("the machine went before its save was made")
	}
	ready := func() bool {
		for n, im := range e.images() {
			if _, ok := before[n]; !ok && im.Status == "READY" {
				return true
			}
		}
		return false
	}
	for i := 0; i < 3 && !ready(); i++ {
		e.pass()
	}
	var newer string
	for n, im := range e.images() {
		if _, ok := before[n]; !ok {
			if im.Status != "READY" {
				t.Fatalf("saved image %s is %s", n, im.Status)
			}
			newer = n
		}
	}
	if newer == "" {
		t.Fatalf("no save: images %v", e.images())
	}
	e.pass()
	if e.has(old) {
		t.Fatal("the older version outlived the save that superseded it")
	}
}

// An image whose making was abandoned, its machine gone, goes after an
// hour, and stops counting against its scope's limit.
func TestAnAbandonedImageGoes(t *testing.T) {
	e := setup(t)
	img := e.saveMain(key1)
	key := "images/5001/" + img + ".json"
	data, gen, _ := e.gcs.Object("state", key)
	var rec disks.Image
	_ = json.Unmarshal(data, &rec)
	rec.State, rec.Machine = disks.ImageMaking, "lsr-5001-gonegone"
	b, _ := json.Marshal(rec)
	if _, err := e.objects.Replace(context.Background(), key, b, gen); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if !e.has(img) {
		t.Fatal("an image being made went before an hour")
	}
	e.clock.Advance(time.Hour)
	e.pass()
	if e.has(img) {
		t.Fatal("an abandoned image wasn't deleted")
	}
	if _, ok := e.record(key); ok {
		t.Fatal("an abandoned image's record was kept")
	}
}

// A repository transferred out of the organization between its job's
// admission and its work disk gets nothing of the organization's.
func TestATransferredRepositoryGetsNoDisk(t *testing.T) {
	e := setup(t)
	e.saveMain(key1)
	release := e.hold()
	run := e.mainRun()
	jb := e.take(run, e.queueJob(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}))
	if code := jb.askDisk(); code != http.StatusServiceUnavailable {
		t.Fatalf("while the lease is held: %d", code)
	}
	e.gh.Transfer(e.repo, e.gh.Org(1234, "elsewhere"))
	release()
	if code := jb.askDisk(); code != http.StatusForbidden {
		t.Fatalf("a transferred repository's disk request: %d, want 403", code)
	}
	in, _ := e.gce.Instance(jb.in.Zone, jb.in.Name)
	if in.Disks[machines.WorkDevice] != "" {
		t.Fatalf("machine with disks %v; want no work disk", in.Disks)
	}
}

// Nor does it save.
func TestATransferredRepositoryDoesntSave(t *testing.T) {
	e := setup(t)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if code := jb.call("/v1/disks/save", map[string]bool{"save": true}, nil); code != http.StatusOK {
		t.Fatalf("save request: %d", code)
	}
	e.gh.Transfer(e.repo, e.gh.Org(1234, "elsewhere"))
	jb.finish(false)
	if len(e.images()) != 0 {
		t.Fatalf("images %v; a transferred repository saved", e.images())
	}
}

// Which disk is a family's newest is read from the images there are: a
// record of one that's gone doesn't leave the one that's there unprotected.
func TestTheNewestThatsThereIsKept(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	gone := e.saveMain(key2)
	e.gce.DeleteImageNow(gone)
	e.clock.Advance(time.Minute)
	newest := e.saveMain(key3)
	e.pass()
	if !e.has(old) || !e.has(newest) {
		t.Fatal("a family's two newest disks that are there weren't both kept")
	}
}

// An image with no record that an unfinished run's choice names is kept.
func TestAnOrphanAChoiceNamesIsKept(t *testing.T) {
	e := setup(t)
	img := e.saveMain(key1)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if jb.source() != img {
		t.Fatal("no restore")
	}
	key := "images/5001/" + img + ".json"
	if err := e.objects.Delete(context.Background(), key, mustGen(t, e, key)); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if !e.has(img) {
		t.Fatal("an image an unfinished run's choice names was deleted as an orphan")
	}
}

// A resume image with no record that a claim holds keeps its claim, and so
// is kept, pass after pass, until its job is shown to have started from it.
func TestAClaimedOrphanKeepsItsClaim(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	jb.preempt()
	e.pass()
	resume := disks.ResumeName(repoID, jb.in.ID)
	again := e.admit(run, e.rerunJob(run, 2, "build"))
	if again.source() != resume {
		t.Fatal("no resume")
	}
	key := "images/5001/" + resume + ".json"
	if err := e.objects.Delete(context.Background(), key, mustGen(t, e, key)); err != nil {
		t.Fatal(err)
	}
	// Nor is there a record of the work disk made from it.
	disk := "disks/" + machines.WorkDisk(again.in.Name) + ".json"
	if err := e.objects.Delete(context.Background(), disk, mustGen(t, e, disk)); err != nil {
		t.Fatal(err)
	}
	e.pass()
	e.pass()
	if !e.has(resume) {
		t.Fatal("a claimed image with no record was deleted")
	}
	if _, ok := e.record("claims/5001/" + resume + ".json"); !ok {
		t.Fatal("the claim keeping an image was removed")
	}
}

// A claimed orphan goes once its claiming job has finished.
func TestAClaimedOrphanGoesWithItsJob(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	jb.preempt()
	e.pass()
	resume := disks.ResumeName(repoID, jb.in.ID)
	again := e.admit(run, e.rerunJob(run, 2, "build"))
	key := "images/5001/" + resume + ".json"
	if err := e.objects.Delete(context.Background(), key, mustGen(t, e, key)); err != nil {
		t.Fatal(err)
	}
	again.finish(false)
	e.pass()
	if e.has(resume) {
		t.Fatal("a claimed orphan outlived its job")
	}
}
