package machines_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/emulators/fault"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/machines"
)

// admitted makes a machine for a main job, boots it and admits the job.
func (e *env) admitted(sel string) (*machine, int64) {
	e.t.Helper()
	run := e.mainRun()
	j := e.queue(run, sel)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	if code := m.started(run.ID); code != http.StatusOK {
		e.t.Fatalf("job-started check: %d", code)
	}
	return m, j.ID
}

func (e *env) exists(name string) bool {
	_, ok := e.gce.Instance(zoneA, name)
	if !ok {
		_, ok = e.gce.Instance(zoneB, name)
	}
	return ok
}

func (e *env) recordsOf(machine string) []string {
	var out []string
	for _, n := range e.gcs.Names("state") {
		if strings.Contains(n, machine) && !strings.HasPrefix(n, "logs/") {
			out = append(out, n)
		}
	}
	return out
}

func TestAReportEndsTheMachine(t *testing.T) {
	e := setup(t)
	m, _ := e.admitted(sel2)
	e.gce.WriteSerial(m.in.Zone, m.in.Name, "supervisor: runner exited\n")
	m.release(true)
	m.finished()
	if code := m.report(false); code != http.StatusNoContent {
		t.Fatalf("report: %d", code)
	}
	if code := m.report(true); code != http.StatusNoContent {
		t.Fatalf("a repeated report: %d", code)
	}
	rep, _ := e.record("machines/" + m.in.Name + "/report.json")
	if rep["save"] != false {
		t.Fatalf("report = %v; the first one stands", rep)
	}
	e.pass()
	if e.exists(m.in.Name) {
		t.Fatal("a reported machine wasn't deleted")
	}
	if n := len(e.gce.Disks()); n != 0 {
		t.Fatalf("%d disks left", n)
	}
	log, _, ok := e.gcs.Object("state", "logs/"+m.in.Name+"-"+m.in.Labels[machines.LabelCreated]+".txt")
	if !ok || string(log) != "supervisor: runner exited\n" {
		t.Fatalf("boot log = %q, %v", log, ok)
	}
	if rs := e.recordsOf(m.in.Name); len(rs) != 0 {
		t.Fatalf("records left: %v", rs)
	}
}

func TestASaveIsntBuiltYet(t *testing.T) {
	e := setup(t)
	m, _ := e.admitted("lanescape-2vcpu-linux-amd64-disk-build-0123456789abcdef0123-main")
	m.release(true)
	m.finished()
	m.report(true)
	e.pass()
	if e.exists(m.in.Name) || !e.logged("saving isn't built yet") {
		t.Fatal("a save that isn't built yet should be logged, and the machine deleted")
	}
}

func TestNoReport(t *testing.T) {
	e := setup(t)
	m, _ := e.admitted(sel2)
	m.release(true)
	m.finished()
	// The supervisor died before reporting.
	e.clock.Advance(machines.ReportWait - time.Minute)
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("deleted before the report's wait was over")
	}
	e.clock.Advance(time.Minute + time.Second)
	e.pass()
	if e.exists(m.in.Name) {
		t.Fatalf("not deleted %s after its job finished with no report", machines.ReportWait)
	}
}

func TestAMachinePastFiveHours(t *testing.T) {
	e := setup(t)
	m, _ := e.admitted(sel2)
	e.clock.Advance(machines.MaxAge - time.Second)
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("a machine mid-job was deleted early")
	}
	e.clock.Advance(time.Second)
	e.pass()
	if e.exists(m.in.Name) {
		t.Fatal("a machine past 5 hours wasn't deleted")
	}
}

func TestAMachineThatStoppedBeforeItsJob(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	e.queue(run, sel2)
	e.pass()
	first := e.one()
	e.gce.Shutdown(first.Zone, first.Name)
	e.pass()
	ms := e.wantMachines(1)
	if ms[0].Name == first.Name {
		t.Fatal("the stopped machine is still there, and no other was made")
	}
	// An hour from the job's first machine, no more are made for it.
	e.clock.Advance(machines.RetryLimit)
	e.gce.Shutdown(ms[0].Zone, ms[0].Name)
	e.pass()
	e.wantMachines(0)
	if !e.logged("leaving a job queued") {
		t.Fatal("why the job was left queued wasn't logged")
	}
}

func TestAnIdleMachine(t *testing.T) {
	e := setup(t)
	e.queue(e.mainRun(), sel2)
	e.pass()
	m := e.one()
	e.clock.Advance(machines.IdleLimit - time.Second)
	e.pass()
	if !e.exists(m.Name) {
		t.Fatal("deleted before the idle limit")
	}
	e.clock.Advance(time.Second)
	e.pass()
	if e.exists(m.Name) {
		t.Fatal("an idle machine wasn't deleted")
	}
	// The job it was made for gets another.
	e.pass()
	e.wantMachines(1)
}

type preemptions struct {
	mu    sync.Mutex
	calls map[string]bool
}

func (p *preemptions) Stopped(_ context.Context, m machines.Machine, _ machines.Admission, preempted bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[m.Name] = preempted
	return nil
}

func TestAStoppedSpotMachine(t *testing.T) {
	for _, preempted := range []bool{true, false} {
		t.Run("preempted "+strconv.FormatBool(preempted), func(t *testing.T) {
			e := setup(t)
			p := &preemptions{calls: map[string]bool{}}
			e.svc.Preemptions = p
			m, _ := e.admitted("lanescape-2vcpu-linux-amd64-spot-main")
			if preempted {
				if err := e.gce.Preempt(m.in.Zone, m.in.Name); err != nil {
					t.Fatal(err)
				}
			} else {
				e.gce.Shutdown(m.in.Zone, m.in.Name)
			}
			e.pass()
			if e.exists(m.in.Name) {
				t.Fatal("a stopped Spot machine wasn't deleted")
			}
			got, called := p.calls[m.in.Name]
			if !called || got != preempted {
				t.Fatalf("preemption seam: called %v, preempted %v; want %v", called, got, preempted)
			}
		})
	}
}

func TestOrphanedDisks(t *testing.T) {
	e := setup(t)
	m, _ := e.admitted(sel2)
	idle := e.mainRun()
	e.queue(idle, sel2)
	e.pass()
	var waiting string
	for _, in := range e.ours() {
		if in.Name != m.in.Name {
			waiting = in.Name
		}
	}
	// A disk recorded as made for a machine that's there waits for it.
	madeFor := machines.WorkDisk(waiting)
	e.gce.AddDisk(zoneA, madeFor, 50, map[string]string{machines.LabelRepository: "5001"})
	e.gcs.Put("state", "disks/"+madeFor+".json", []byte(`{"disk":"`+madeFor+`","machine":"`+waiting+`","zone":"`+zoneA+`","state":"made"}`))
	// A disk recorded for a machine that's gone, and one not recorded.
	goneFor := "lsr-5001-zzzzzzzz-work"
	e.gce.AddDisk(zoneA, goneFor, 50, map[string]string{machines.LabelRepository: "5001"})
	e.gcs.Put("state", "disks/"+goneFor+".json", []byte(`{"disk":"`+goneFor+`","machine":"lsr-5001-zzzzzzzz","zone":"`+zoneA+`","state":"making"}`))
	unrecorded := "lsr-5001-yyyyyyyy-work"
	e.gce.AddDisk(zoneB, unrecorded, 50, map[string]string{machines.LabelRepository: "5001"})
	// Disks the service didn't make are left alone.
	e.gce.AddDisk(zoneB, "lsr-proxy-git-mirror", 500, map[string]string{"lsr-repository": "x"})
	e.gce.AddDisk(zoneB, "lsr-5001-xxxxxxxx-work", 50, nil)
	e.pass()
	have := map[string]bool{}
	for _, d := range e.gce.Disks() {
		have[d.Name] = true
	}
	if !have[madeFor] || !have[machines.WorkDisk(m.in.Name)] || !have["lsr-proxy-git-mirror"] || !have["lsr-5001-xxxxxxxx-work"] {
		t.Fatalf("disks = %v; a disk waiting for its machine, an attached one or one not the service's was deleted", have)
	}
	if have[goneFor] || have[unrecorded] {
		t.Fatalf("disks = %v; orphans were left", have)
	}
	if _, ok := e.record("disks/" + goneFor + ".json"); ok {
		t.Fatal("an orphan's record was left")
	}
}

func TestOfflineRunnerRegistrations(t *testing.T) {
	e := setup(t)
	e.queue(e.mainRun(), sel2)
	e.pass()
	live := e.one()
	// A registration whose machine is gone, as when a machine was made
	// with no answer and never was.
	g := e.gh.RunnerGroups(e.org)[1]
	ctx := context.Background()
	_, _, err := e.svc.GitHub.JITConfig(ctx, g.ID, "lsr-5001-gonegone", []string{sel2})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.GitHub.JITConfig(ctx, g.ID, "someone-elses", []string{sel2}); err != nil {
		t.Fatal(err)
	}
	e.pass()
	names := map[string]bool{}
	for _, r := range e.gh.Runners() {
		names[r.Name] = true
	}
	if !names[live.Name] || !names["someone-elses"] || names["lsr-5001-gonegone"] {
		t.Fatalf("runners = %v; want the gone machine's removed and the rest kept", names)
	}
}

func TestCleanupWithoutTheRecords(t *testing.T) {
	e := setup(t)
	m, _ := e.admitted(sel2)
	e.clock.Advance(machines.IdleLimit + time.Minute)
	// A refusal the storage client doesn't retry, so the pass goes on
	// without the machines' records.
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1, Match: func(r *http.Request) bool { return r.URL.Query().Get("prefix") == "machines/" },
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("an admitted machine was deleted when its admission couldn't be read")
	}
	// Only the age limit holds without the records.
	e.clock.Advance(machines.MaxAge)
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1, Match: func(r *http.Request) bool { return r.URL.Query().Get("prefix") == "machines/" },
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	e.pass()
	if e.exists(m.in.Name) {
		t.Fatal("a machine past 5 hours was kept")
	}
}

func TestCleanupClaimsAMachineBeforeACheckCan(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	// Cleanup claims the idle machine, and the deletion fails.
	e.gce.ServerError(http.MethodDelete, "/instances/"+m.in.Name, http.StatusInternalServerError, 0)
	e.clock.Advance(machines.IdleLimit)
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("the machine should still be there")
	}
	// The hook asks after the claim: the machine is no longer for any job.
	if code := m.started(run.ID); code != http.StatusForbidden {
		t.Fatalf("a check after cleanup's claim: %d, want 403", code)
	}
}

func TestAMachineThatMayHaveBeenMadeCountsAgainstTheLimit(t *testing.T) {
	e := setup(t)
	e.cfg.Limits.Repositories = map[int64]config.Override{repoID: {Machines: ptr(1)}}
	run := e.mainRun()
	e.queue(run, sel2)
	e.queue(run, "lanescape-16vcpu-linux-arm64-main")
	// Every answer to making the first is lost, after it's made.
	e.gce.LoseResponse(http.MethodPost, "/instances", 4)
	e.pass()
	e.wantMachines(1)
}

func TestNoCapacityDoesntStartTheJobsHour(t *testing.T) {
	e := setup(t)
	j := e.queue(e.mainRun(), sel2)
	for _, z := range []string{zoneA, zoneB} {
		e.gce.Stockout(z, "n4-highcpu-2", false)
		e.gce.Stockout(z, "c4-highcpu-2", false)
	}
	e.pass()
	if _, ok := e.record("jobs/5001/" + strconv.FormatInt(j.ID, 10) + ".json"); ok {
		t.Fatal("a job with no machine has its first machine recorded")
	}
}

func TestAFirstMachineThatWasntMade(t *testing.T) {
	e := setup(t)
	j := e.queue(e.mainRun(), sel2)
	for _, z := range []string{zoneA, zoneB} {
		e.gce.Stockout(z, "n4-highcpu-2", false)
		e.gce.Stockout(z, "c4-highcpu-2", false)
	}
	// The tentative record can't be taken back.
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodDelete, Times: 1,
		Match:   func(r *http.Request) bool { return strings.Contains(r.URL.Path, "jobs") },
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	e.pass()
	if rec, ok := e.record("jobs/5001/" + strconv.FormatInt(j.ID, 10) + ".json"); !ok || rec["tentative"] != true {
		t.Fatalf("job record = %v, %v", rec, ok)
	}
	e.clock.Advance(machines.RetryLimit + time.Minute)
	for _, z := range []string{zoneA, zoneB} {
		e.gce.ClearStockout(z, "n4-highcpu-2", false)
	}
	e.pass()
	e.wantMachines(1)
	if rec, _ := e.record("jobs/5001/" + strconv.FormatInt(j.ID, 10) + ".json"); rec["tentative"] != nil {
		t.Fatalf("job record = %v; a made machine confirms it", rec)
	}
}

func TestABootLogThatCouldntBeSaved(t *testing.T) {
	e := setup(t)
	m, _ := e.admitted(sel2)
	m.release(true)
	m.finished()
	m.report(false)
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1,
		Match: func(r *http.Request) bool {
			return strings.Contains(r.URL.RawQuery, "logs") || strings.Contains(r.URL.Path, "logs")
		},
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("a machine was deleted though its boot log couldn't be saved")
	}
	e.pass()
	if e.exists(m.in.Name) {
		t.Fatal("not deleted once its boot log was saved")
	}
	if _, _, ok := e.gcs.Object("state", "logs/"+m.in.Name+"-"+m.in.Labels[machines.LabelCreated]+".txt"); !ok {
		t.Fatal("no boot log")
	}
}

func TestAFirstMachineIsConfirmedBeforeItGoes(t *testing.T) {
	e := setup(t)
	j := e.queue(e.mainRun(), sel2)
	// The record is written, but its confirmation once the machine is made
	// fails.
	writes := 0
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1,
		Match: func(r *http.Request) bool {
			if !strings.Contains(r.URL.RawQuery, "jobs") {
				return false
			}
			writes++
			return writes == 2
		},
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	e.pass()
	key := "jobs/5001/" + strconv.FormatInt(j.ID, 10) + ".json"
	if rec, _ := e.record(key); rec["tentative"] != true {
		t.Fatalf("job record = %v; want it left tentative", rec)
	}
	// GitHub's queue can't be read when the idle machine goes.
	e.clock.Advance(machines.IdleLimit)
	e.gh.ServerError("/repos/lanescape/app/actions/runs", http.StatusBadGateway, 0)
	e.pass()
	e.wantMachines(0)
	if rec, _ := e.record(key); rec["tentative"] != nil {
		t.Fatalf("job record = %v; the machine should have confirmed it before going", rec)
	}
}
