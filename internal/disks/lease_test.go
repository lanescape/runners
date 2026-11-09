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
	jb := e.take(run, e.gh.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}))
	if code := jb.started(); code != http.StatusServiceUnavailable {
		t.Fatalf("a check while the disks' lease is held: %d, want a retryable 503", code)
	}
	release()
	if code := jb.started(); code != http.StatusOK {
		t.Fatalf("the check once the lease is free: %d", code)
	}
	if jb.source() != img {
		t.Fatalf("restored %q, want %s", jb.source(), img)
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
