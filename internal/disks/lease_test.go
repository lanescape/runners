package disks_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
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

// forbidden answers 403, which no client tries again.
var forbidden = fault.Status(http.StatusForbidden, nil, `{"error": {"code": 403, "message": "Forbidden"}}`)

func TestAChoiceWaitsForTheLease(t *testing.T) {
	e := setup(t)
	v := e.saveMain(key1)
	release := e.hold()
	run := e.mainRun()
	jb := e.take(run, e.queueJob(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}))
	if code := jb.askRestore(); code != http.StatusServiceUnavailable {
		t.Fatalf("a restore while the disks' lease is held: %d, want a retryable 503", code)
	}
	release()
	if code := jb.askRestore(); code != http.StatusOK {
		t.Fatalf("the restore once the lease is free: %d", code)
	}
	if jb.restored() != v {
		t.Fatalf("restored %q, want %s", jb.restored(), v)
	}
	// The run's next job reuses its choice, under the lease too.
	release = e.hold()
	next := e.take(run, e.queueJob(run, ghemu.JobSpec{Name: "test", Labels: []string{sel(key1, "main")}}))
	if code := next.askRestore(); code != http.StatusServiceUnavailable {
		t.Fatalf("reusing a choice while the disks' lease is held: %d, want a retryable 503", code)
	}
	release()
	if code := next.askRestore(); code != http.StatusOK || next.restored() != v {
		t.Fatalf("reusing the choice: %d, %q", code, next.restored())
	}
}

func TestADeletionThatFailedIsTriedAgain(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	// The deletion of each of its two objects fails, and so does the next
	// pass's first, as the waiting job's machine is made.
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodDelete, Times: 3, Respond: forbidden,
		Match: func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/b/"+cachesBucket+"/o/") }})
	newer := e.saveMain(key1)
	if !e.has(old) || !e.recorded(old) {
		t.Fatal("the deletion didn't fail")
	}
	if rec, _ := e.record("caches/5001/" + old + ".json"); rec["state"] != disks.ImageMarked {
		t.Fatalf("version record = %v; want it marked", rec)
	}
	run := e.mainRun()
	jb := e.take(run, e.queueJob(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}))
	if !e.recorded(old) {
		t.Fatal("the second try didn't fail")
	}
	// Marked, it's never restored.
	if code := jb.askRestore(); code != http.StatusOK || jb.restored() != newer {
		t.Fatalf("restore: %d, %q, want %s", code, jb.restored(), newer)
	}
	e.pass()
	if e.has(old) || e.recorded(old) || !e.ready(newer) {
		t.Fatal("a marked version whose deletion failed wasn't deleted on the next try")
	}
}

func TestMarkingWaitsForTheLease(t *testing.T) {
	e := setup(t)
	// The saving job starts before the older version is saved, so no
	// choice of its keeps it.
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	old := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	// A newer version can't mark the older one while the lease is held;
	// it's ready all the same.
	release := e.hold()
	jb.finish(true)
	newer := jb.saved
	if !e.has(old) || !e.ready(newer) {
		t.Fatalf("old %v, newer %v; want both while the lease is held", e.has(old), e.ready(newer))
	}
	release()
	e.pass()
	if e.has(old) || !e.has(newer) {
		t.Fatalf("old %v, newer %v; want the older version deleted once the lease is free", e.has(old), e.has(newer))
	}
}

func TestAMarkedVersionIsNeverChosen(t *testing.T) {
	e := setup(t)
	v := e.saveMain(key1)
	// A job waiting to restore, given its machine before the version is
	// marked, since a pass deletes what's marked.
	run := e.mainRun()
	jb := e.take(run, e.queueJob(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}))
	key := "caches/5001/" + v + ".json"
	data, gen, _ := e.gcs.Object("state", key)
	var rec disks.Image
	_ = json.Unmarshal(data, &rec)
	rec.State = disks.ImageMarked
	b, _ := json.Marshal(rec)
	if _, err := e.objects.Replace(context.Background(), key, b, gen); err != nil {
		t.Fatal(err)
	}
	if code := jb.askRestore(); code != http.StatusOK || jb.restored() != "" || !e.has(v) {
		t.Fatalf("restore: %d, %q; want a marked version, still there, never chosen", code, jb.restored())
	}
	// And the next pass deletes it, objects and record.
	e.pass()
	if e.has(v) || e.recorded(v) {
		t.Fatal("a marked version wasn't deleted")
	}
}

func TestOrphans(t *testing.T) {
	e := setup(t)
	v := e.saveMain(key1)
	// A version's objects with no record, as they would be if its record
	// were lost, go.
	e.orphan(v)
	e.pass()
	if e.has(v) {
		t.Fatal("a version's orphaned objects weren't deleted")
	}
	if !e.logged(`msg="orphaned cache objects deleted"`) {
		t.Fatal("not logged")
	}
	// A record of a ready version whose objects are gone goes too, and a
	// run's choice of it picks again.
	v = e.saveMain(key1)
	run := e.mainRun()
	if e.start(run, ghemu.JobSpec{Name: "a", Labels: []string{sel(key1, "main")}}).restored() != v {
		t.Fatal("no restore")
	}
	e.removeObjects(v)
	e.clock.Advance(time.Second)
	e.pass()
	if e.recorded(v) {
		t.Fatal("the record of a version whose objects are gone was kept")
	}
	if b := e.start(run, ghemu.JobSpec{Name: "b", Labels: []string{sel(key1, "main")}}); b.restored() != "" ||
		b.restore.Restored != disks.StepNone {
		t.Fatalf("a choice of a version whose objects are gone restored %+v", b.restore)
	}
	if !e.logged("a run's restore choice is gone; picking again") {
		t.Fatal("picking again wasn't logged")
	}
	// Base images are never the disks' to delete.
	for _, im := range e.gce.Images() {
		if im.Name == "lsr-base-amd64-1" {
			return
		}
	}
	t.Fatal("a base image was deleted")
}

// A cache request is a running job's: once its job has ended, it's refused.
func TestCacheRequestsNeedARunningJob(t *testing.T) {
	e := setup(t)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	code, ans := jb.askSave()
	if code != http.StatusOK || !ans.Saves {
		t.Fatalf("save: %d, %+v", code, ans)
	}
	jb.upload(ans.Prefix)
	jb.runSteps()
	if code, _ := jb.commit(); code != http.StatusForbidden {
		t.Fatalf("a commit once the job has ended: %d, want 403", code)
	}
	if code := jb.askRestore(); code != http.StatusForbidden {
		t.Fatalf("a restore once the job has ended: %d, want 403", code)
	}
	jb.sendReport()
	e.gce.Shutdown(jb.in.Zone, jb.in.Name)
	e.pass()
	// Never committed, its version goes with its objects.
	if len(e.versions()) != 0 {
		t.Fatalf("versions %v; want the uncommitted one gone", e.versions())
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

// A resume image whose making failed, and left a failed image behind, is
// deleted and made again; the machine waits for it, and its run is rerun
// only once it's ready.
func TestAFailedImageIsMadeAgain(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	e.gce.FailNextOperation("images.insert.leave", "INTERNAL_ERROR")
	jb.preempt()
	e.pass()
	resume := disks.ResumeName(repoID, jb.in.ID)
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); !ok {
		t.Fatal("the machine went before its resume image was made")
	}
	if e.attempt(run) != 1 {
		t.Fatal("the run was rerun before its resume image was ready")
	}
	for i := 0; i < 3 && e.attempt(run) == 1; i++ {
		e.pass()
	}
	if im, ok := e.images()[resume]; !ok || im.Status != "READY" {
		t.Fatalf("resume image = %+v, %v; want it made again", im, ok)
	}
	if rec, _ := e.record("images/5001/" + resume + ".json"); rec["state"] != disks.ImageReady {
		t.Fatalf("resume image record = %v", rec)
	}
	if e.attempt(run) != 2 {
		t.Fatal("not rerun once the resume image was made")
	}
	if src := e.admit(run, e.rerunJob(run, 2, "build")).source(); src != resume {
		t.Fatalf("the rerun's job started from %q, want %s", src, resume)
	}
}

// A resume image whose making was abandoned, its machine gone, goes after
// an hour.
func TestAnAbandonedImageGoes(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	jb.preempt()
	e.pass()
	img := disks.ResumeName(repoID, jb.in.ID)
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
	if _, ok := e.images()[img]; !ok {
		t.Fatal("an image being made went before an hour")
	}
	e.clock.Advance(time.Hour)
	e.pass()
	if _, ok := e.images()[img]; ok {
		t.Fatal("an abandoned image wasn't deleted")
	}
	if _, ok := e.record(key); ok {
		t.Fatal("an abandoned image's record was kept")
	}
}

// A repository transferred out of the organization between its job's
// admission and its restore gets nothing of the organization's: no
// version, nor a work disk.
func TestATransferredRepositoryGetsNoCache(t *testing.T) {
	e := setup(t)
	e.saveMain(key1)
	release := e.hold()
	run := e.mainRun()
	jb := e.take(run, e.queueJob(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}))
	if code := jb.askRestore(); code != http.StatusServiceUnavailable {
		t.Fatalf("while the lease is held: %d", code)
	}
	e.gh.Transfer(e.repo, e.gh.Org(1234, "elsewhere"))
	release()
	if code := jb.askRestore(); code != http.StatusForbidden || jb.restored() != "" {
		t.Fatalf("a transferred repository's restore: %d, want 403", code)
	}
	if code := jb.call(machines.DisksPath, map[string]string{}, nil); code != http.StatusForbidden {
		t.Fatalf("a transferred repository's disk request: %d, want 403", code)
	}
	in, _ := e.gce.Instance(jb.in.Zone, jb.in.Name)
	if in.Disks[machines.WorkDevice] != "" {
		t.Fatalf("machine with disks %v; want no work disk", in.Disks)
	}
}

// Nor does it save: a save asked for once it's transferred is refused, and
// a version committed before waits, neither ready nor deleted, while GitHub
// shows the repository as another owner's; transferred back, it's ready.
func TestATransferredRepositoryDoesntSave(t *testing.T) {
	e := setup(t)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	e.gh.Transfer(e.repo, e.gh.Org(1234, "elsewhere"))
	if code, _ := jb.askSave(); code != http.StatusForbidden {
		t.Fatalf("a transferred repository's save: %d, want 403", code)
	}
	jb.finish(false)
	if len(e.versions()) != 0 {
		t.Fatalf("versions %v; a transferred repository saved", e.versions())
	}

	e.gh.Transfer(e.repo, e.org)
	jb = e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	v := jb.saveCache()
	e.gh.Transfer(e.repo, e.gh.Org(1234, "elsewhere"))
	jb.finish(false)
	e.pass()
	if rec, _ := e.record("caches/5001/" + v + ".json"); rec["state"] != disks.ImageCommitted || !e.has(v) {
		t.Fatalf("version %v; want it kept, committed, while its repository is another owner's", rec)
	}
	e.gh.Transfer(e.repo, e.org)
	e.pass()
	if !e.ready(v) {
		t.Fatal("a version wasn't made ready once its repository was transferred back")
	}
}

// Which version of a cache is newest is read from the versions there are:
// a record of one whose objects are gone doesn't leave the one that's there
// unprotected.
func TestTheNewestThatsThereIsKept(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	// A newer version of the cache, recorded ready, whose objects are gone.
	e.clock.Advance(time.Minute)
	gone := disks.VersionName(repoID, disks.Series("main", "amd64", "build", key2), e.clock.Now(), "lsr-5001-goneaway")
	b, _ := json.Marshal(disks.Image{Image: gone, RepositoryID: repoID, Kind: disks.KindCache, State: disks.ImageReady, Scope: "main",
		Arch: "amd64", Cache: "build", Key: key2, Machine: "lsr-5001-goneaway", SavedAt: e.clock.Now().UTC(),
		Objects: []machines.CacheObject{{Name: disks.VersionPrefix(repoID, gone) + "0.tar.zst", Size: 1, CRC32C: "AAAAAA==", Generation: 1}}})
	e.gcs.Put("state", "caches/5001/"+gone+".json", b)
	e.pass()
	if !e.has(old) || !e.ready(old) {
		t.Fatal("a cache's newest version that's there went, replaced by one whose objects are gone")
	}
	if e.recorded(gone) {
		t.Fatal("the record of a version whose objects are gone was kept")
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
	if _, ok := e.images()[resume]; !ok {
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
	if _, ok := e.images()[resume]; ok {
		t.Fatal("a claimed orphan outlived its job")
	}
}
