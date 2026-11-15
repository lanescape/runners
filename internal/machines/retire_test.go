package machines_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lanescape/runners/internal/bases"
	"github.com/lanescape/runners/internal/deploy"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/pass"
	baseimage "github.com/lanescape/runners/machine"
)

// otherPrograms deploys other programs than those the earlier image was
// made with: the service's base images' service is replaced by one of
// another amd64 key, whose image isn't made yet, and other programs. It
// returns the new keys.
func (e *env) otherPrograms() map[string]string {
	old := e.svc.Bases.(*bases.Service)
	keys := map[string]string{"amd64": strings.Repeat("e", 20), "arm64": old.Keys["arm64"]}
	e.svc.Bases = &bases.Service{Config: old.Config, Compute: old.Compute, Objects: old.Objects, Clock: old.Clock, Logger: old.Logger,
		Network: old.Network, Subnetwork: old.Subnetwork, DiskType: old.DiskType, Keys: keys, Programs: strings.Repeat("e", 64)}
	return keys
}

func (e *env) admission(name string) (map[string]any, bool) {
	return e.record("machines/" + name + "/admission.json")
}

// TestIdleMachinesOfOtherProgramsAreRetired: once the service's programs
// change, every machine no job was admitted on whose base image was made
// with other programs is retired, its admission claimed first, as an idle
// one is, and deleted; a machine whose job was admitted keeps it; the
// queued job waits for the new image and gets a machine on it, which isn't
// retired (specs/0002-machines.md, A machine's life).
func TestIdleMachinesOfOtherProgramsAreRetired(t *testing.T) {
	e, _ := newKey(t, map[string]string{bases.LabelKind: bases.KindBase, bases.LabelPrograms: programs()})
	run := e.mainRun()
	e.queue(run, sel2)
	e.queue(run, sel2)
	e.pass()
	ms := e.wantMachines(2)
	for _, m := range ms {
		if got := e.source(m.Name); got != earlierImage {
			t.Fatalf("machine %s boots %s", m.Name, got)
		}
	}
	// One machine's job is admitted.
	busy := e.boot(ms[0])
	e.eventually("a job is on "+busy.in.Name, func() bool { _, _, ok := e.gh.JobOnRunner(busy.in.Name); return ok })
	if code := busy.started(run.ID); code != http.StatusOK {
		t.Fatalf("job-started check: %d", code)
	}
	idle := ms[1]
	// A pass with the programs unchanged retires nothing.
	e.pass()
	e.wantMachines(2)

	keys := e.otherPrograms()
	e.pass()
	adm, ok := e.admission(idle.Name)
	if !ok || adm["allowed"] == true || !strings.Contains(fmt.Sprint(adm["reason"]), "retired: its base image, "+earlierImage+
		", wasn't made with the service's programs") {
		t.Fatalf("the idle machine's admission = %v", adm)
	}
	if e.exists(idle.Name) {
		t.Fatal("the idle machine wasn't deleted")
	}
	if !e.exists(busy.in.Name) {
		t.Fatal("the machine whose job was admitted was retired")
	}
	// The job left queued waits for the new image, then gets a machine on
	// it, which later passes keep.
	e.pass()
	if len(e.ours()) != 1 {
		t.Fatalf("machines = %+v; the queued job waits for the new image", e.ours())
	}
	e.gce.WriteSerial(zoneA, bases.BuilderName("amd64", keys["amd64"]), baseimage.Built+"amd64-"+keys["amd64"]+"\n")
	e.pass()
	ms = e.wantMachines(2)
	var fresh string
	for _, m := range ms {
		if m.Name != busy.in.Name {
			fresh = m.Name
		}
	}
	if got := e.source(fresh); got != bases.ImageName("amd64", keys["amd64"]) {
		t.Fatalf("the new machine boots %s", got)
	}
	e.pass()
	e.pass()
	if !e.exists(fresh) {
		t.Fatal("a machine of the current key's image was retired")
	}
	if _, ok := e.admission(fresh); ok {
		t.Fatal("a machine of the current key's image was claimed")
	}
}

// TestAMachineWhoseImageWasDeletedIsRetired: a machine no job was admitted
// on whose base image has since been deleted, so that its programs can't be
// known, is retired; one whose disks couldn't be listed isn't.
func TestAMachineWhoseImageWasDeletedIsRetired(t *testing.T) {
	e, _ := newKey(t, map[string]string{bases.LabelKind: bases.KindBase, bases.LabelPrograms: programs()})
	e.queue(e.mainRun(), sel2)
	e.pass()
	m := e.one()
	e.gce.DeleteImageNow(earlierImage)
	e.gce.ServerError(http.MethodGet, "/disks", http.StatusForbidden, 0)
	e.pass()
	if !e.exists(m.Name) {
		t.Fatal("a machine whose disks couldn't be listed was retired")
	}
	e.gce.Faults().Reset()
	e.pass()
	adm, _ := e.admission(m.Name)
	if !strings.Contains(fmt.Sprint(adm["reason"]), ", since deleted, wasn't made with the service's programs") || e.exists(m.Name) {
		t.Fatalf("admission = %v; the machine whose image was deleted wasn't retired", adm)
	}
}

// TestAReplacedRevisionLeavesJobsAlone: a replaced revision decides nothing
// of queued jobs: a job whose record says it waits for the latest
// revision's base image keeps its wait, however that revision's own images
// look to the replaced one.
func TestAReplacedRevisionLeavesJobsAlone(t *testing.T) {
	e, _, run, job := aJobWithAFirstMachine(t)
	e.clock.Advance(10 * time.Minute)
	e.pass()
	key := "jobs/5001/" + strconv.FormatInt(job.ID, 10) + ".json"
	before, _ := e.record(key)
	if before["waiting_since"] == nil {
		t.Fatalf("job record = %v; want its wait recorded", before)
	}
	ctx := context.Background()
	e.svc.Fence = &deploy.Fence{Own: deploy.ID{Run: 1, Attempt: 1}, Objects: e.svc.Objects, Clock: e.clock}
	if err := (&deploy.Fence{Own: deploy.ID{Run: 2, Attempt: 1}, Objects: e.svc.Objects, Clock: e.clock}).Serving(ctx); err != nil {
		t.Fatal(err)
	}
	// To the replaced revision, an image is ready, and the job's hour has
	// run out by its record.
	e.gce.HoldImages(false)
	e.clock.Advance(machines.RetryLimit)
	e.pass()
	e.pass()
	if after, _ := e.record(key); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("job record = %v; the replaced revision changed it from %v", after, before)
	}
	if _, ok := e.record("refusals/5001/" + strconv.FormatInt(run.ID, 10) + "-1.json"); ok {
		t.Fatal("the replaced revision refused the run")
	}
	if len(e.ours()) != 0 {
		t.Fatalf("machines = %+v", e.ours())
	}
}

// TestARetiredMachinesTakenJobIsRecordedLost: a machine retired for its
// programs after its runner took a job, but before the job's check, has
// its runner's job recorded as lost, so that its run is rerun; the check,
// coming later, is refused, and the machine goes (specs/0002-machines.md,
// Retiring a machine).
func TestARetiredMachinesTakenJobIsRecordedLost(t *testing.T) {
	e, _ := newKey(t, map[string]string{bases.LabelKind: bases.KindBase, bases.LabelPrograms: programs()})
	p := &preemptions{calls: map[string]bool{}}
	e.svc.Preemptions = p
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	e.otherPrograms()
	// The machine is retired, and its deletion fails, so that the check,
	// which comes later, finds it there.
	e.gce.ServerError(http.MethodDelete, "/instances/"+m.in.Name, http.StatusInternalServerError, 0)
	e.pass()
	if adm, _ := e.admission(m.in.Name); !strings.Contains(fmt.Sprint(adm["reason"]), "retired: its base image") || adm["retired"] != true {
		t.Fatalf("admission = %v", adm)
	}
	lost, ok := p.lostOn(m.in.Name)
	if !ok || lost.JobID != j.ID || lost.RunID != run.ID || lost.RunAttempt != 1 || lost.JobName == "" || !lost.Allowed {
		t.Fatalf("lost = %+v, %v; want the job recorded as its check would have allowed it", lost, ok)
	}
	if code := m.started(run.ID); code != http.StatusForbidden {
		t.Fatalf("the check after the machine was retired: %d, want 403", code)
	}
	e.gce.Faults().Reset()
	e.pass()
	if e.exists(m.in.Name) {
		t.Fatal("the retired machine is still there")
	}
	if len(e.ours()) != 0 {
		t.Fatalf("machines = %+v; the job isn't queued, so it gets none", e.ours())
	}
	if _, ok := e.record("refusals/5001/" + strconv.FormatInt(run.ID, 10) + "-1.json"); ok {
		t.Fatal("the run was refused")
	}
}

// TestAReplacedRevisionMakesNoMachines: once a later deploy's revision has
// recorded itself, at the start of its pass, an earlier revision makes no
// machines and retires none for their programs, and says why; its other
// cleanup goes on (specs/0004-deployment.md, Which revision makes
// machines).
func TestAReplacedRevisionMakesNoMachines(t *testing.T) {
	e, _ := newKey(t, map[string]string{bases.LabelKind: bases.KindBase, bases.LabelPrograms: programs()})
	ctx := context.Background()
	own := &deploy.Fence{Own: deploy.ID{Run: 10, Attempt: 1}, Objects: e.svc.Objects, Clock: e.clock}
	e.svc.Fence = own
	run := e.mainRun()
	e.queue(run, sel2)
	if err := e.runner.Run(ctx, pass.All(own.Pass, e.svc.Pass)); err != nil {
		t.Fatal(err)
	}
	m := e.one()
	if rec, ok := e.record(deploy.Key); !ok || rec["run_id"] != float64(10) {
		t.Fatalf("latest deploy = %v; the serving revision's pass records it", rec)
	}

	later := &deploy.Fence{Own: deploy.ID{Run: 11, Attempt: 1}, Objects: e.svc.Objects, Clock: e.clock}
	if err := later.Serving(ctx); err != nil {
		t.Fatal(err)
	}
	// A queued job, whose image is there, gets no machine from it.
	e.queue(run, sel2)
	e.queue(run, sel2)
	if err := e.runner.Run(ctx, pass.All(own.Pass, e.svc.Pass)); err != nil {
		t.Fatal(err)
	}
	if ms := e.ours(); len(ms) != 1 || ms[0].Name != m.Name {
		t.Fatalf("machines = %+v; the replaced revision makes none", ms)
	}
	// Nor does it retire one for its programs, which aren't the service's.
	orig := e.svc.Bases
	e.otherPrograms()
	if err := e.runner.Run(ctx, pass.All(own.Pass, e.svc.Pass)); err != nil {
		t.Fatal(err)
	}
	if ms := e.ours(); len(ms) != 1 || ms[0].Name != m.Name {
		t.Fatalf("machines = %+v; the replaced revision retires none for its programs", ms)
	}
	if _, ok := e.admission(m.Name); ok {
		t.Fatal("the replaced revision retired a machine for its programs")
	}
	if !e.logged("making no machine: a later deploy replaced this revision") || !e.logged("latest=11-1") {
		t.Fatal("why the revision makes no machines isn't logged")
	}
	if rec, _ := e.record(deploy.Key); rec["run_id"] != float64(11) {
		t.Fatalf("latest deploy = %v; the replaced revision wrote over it", rec)
	}
	// The idle limit still holds.
	e.clock.Advance(machines.IdleLimit)
	e.pass()
	if e.exists(m.Name) {
		t.Fatal("the replaced revision's cleanup stopped")
	}
	// The later revision makes the machines.
	e.svc.Fence, e.svc.Bases = later, orig
	e.pass()
	if len(e.ours()) == 0 {
		t.Fatal("the latest revision made no machine")
	}
}

// TestARevisionWithNoDeployIDGivesWayToADeploy: a revision run outside a
// deploy, with no ID, makes machines while no deploy is recorded, and none
// once one is: it can't stop a deploy's revision, which records itself
// whatever it finds.
func TestARevisionWithNoDeployIDGivesWayToADeploy(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.svc.Fence = &deploy.Fence{Objects: e.svc.Objects, Clock: e.clock}
	run := e.mainRun()
	e.queue(run, sel2)
	if err := e.runner.Run(ctx, pass.All(e.svc.Fence.Pass, e.svc.Pass)); err != nil {
		t.Fatal(err)
	}
	e.wantMachines(1)
	if _, ok := e.record(deploy.Key); ok {
		t.Fatal("a revision with no deploy ID recorded one")
	}
	if err := (&deploy.Fence{Own: deploy.ID{Run: 1, Attempt: 1}, Objects: e.svc.Objects, Clock: e.clock}).Serving(ctx); err != nil {
		t.Fatal(err)
	}
	e.queue(run, sel2)
	e.clock.Advance(time.Second)
	e.pass()
	e.wantMachines(1)
}
