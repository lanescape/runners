package machines_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/internal/deploy"
	"github.com/lanescape/runners/internal/machines"
)

// registered reports whether GitHub still has a runner registered under a
// machine's name.
func (e *env) registered(name string) bool {
	for _, r := range e.gh.Runners() {
		if r.Name == name {
			return true
		}
	}
	return false
}

// withLost gives the service a record of lost jobs it can be asked about.
func (e *env) withLost() *preemptions {
	p := &preemptions{calls: map[string]bool{}}
	e.svc.Preemptions = p
	return p
}

// TestAnIdleMachinesRunnerIsRemovedBeforeItGoes: a machine retired after 20
// minutes with no job has its runner's registration removed, so that GitHub
// gives it none, and nothing is recorded lost.
func TestAnIdleMachinesRunnerIsRemovedBeforeItGoes(t *testing.T) {
	e := setup(t)
	p := e.withLost()
	e.queue(e.mainRun(), sel2)
	e.pass()
	m := e.one()
	if !e.registered(m.Name) {
		t.Fatal("no runner registered")
	}
	e.clock.Advance(machines.IdleLimit)
	e.pass()
	if e.exists(m.Name) || e.registered(m.Name) {
		t.Fatal("the idle machine or its runner is still there")
	}
	if _, ok := p.lostOn(m.Name); ok {
		t.Fatal("a job was recorded lost for a runner that had none")
	}
}

// TestARunnerAlreadyGoneCountsAsRemoved: a runner GitHub no longer has, 404,
// is removed already.
func TestARunnerAlreadyGoneCountsAsRemoved(t *testing.T) {
	e := setup(t)
	e.withLost()
	e.queue(e.mainRun(), sel2)
	e.pass()
	m := e.one()
	runner, _ := strconv.ParseInt(m.Metadata[machines.MetaRunnerID], 10, 64)
	if err := e.svc.GitHub.DeleteRunner(context.Background(), runner); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(machines.IdleLimit)
	e.pass()
	if e.exists(m.Name) {
		t.Fatal("a machine whose runner was gone wasn't deleted")
	}
}

// TestAnIdleMachinesTakenJobIsRecordedLost: a machine retired after 20
// minutes whose runner took a job just before the claim: GitHub refuses to
// remove a runner running a job, 422, so the job is looked for, recorded
// lost as its check would have decided it, and the machine goes. That holds
// in a revision a later deploy replaced too, which still retires idle
// machines.
func TestAnIdleMachinesTakenJobIsRecordedLost(t *testing.T) {
	for name, replaced := range map[string]bool{"the latest revision": false, "a replaced revision": true} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			p := e.withLost()
			run := e.mainRun()
			j := e.queue(run, sel2)
			e.pass()
			m := e.boot(e.one())
			m.taken(j)
			if replaced {
				ctx := context.Background()
				e.svc.Fence = &deploy.Fence{Own: deploy.ID{Run: 1, Attempt: 1}, Objects: e.svc.Objects, Clock: e.clock}
				if err := (&deploy.Fence{Own: deploy.ID{Run: 2, Attempt: 1}, Objects: e.svc.Objects, Clock: e.clock}).Serving(ctx); err != nil {
					t.Fatal(err)
				}
			}
			e.clock.Advance(machines.IdleLimit)
			e.pass()
			lost, ok := p.lostOn(m.in.Name)
			if !ok || lost.JobID != j.ID || lost.RunID != run.ID || !lost.Allowed || lost.Scope.String() != "main" {
				t.Fatalf("lost = %+v, %v", lost, ok)
			}
			if e.exists(m.in.Name) {
				t.Fatal("the machine wasn't deleted once its runner's job was recorded")
			}
		})
	}
}

// TestAJobGitHubDoesntShowYetIsWaitedFor: a runner GitHub refuses to remove
// as busy has a job: it's looked for for a while, as the check does, and
// if GitHub doesn't show it by then, the machine waits for a later pass,
// which finds it.
func TestAJobGitHubDoesntShowYetIsWaitedFor(t *testing.T) {
	e := setup(t)
	p := e.withLost()
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	// GitHub hides the assignment from more reads than the wait takes.
	e.gh.HideAssignment(j, 1000)
	e.clock.Advance(machines.IdleLimit)
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("a machine whose runner's job couldn't be found was deleted")
	}
	if !e.logged("doesn't show yet; the machine waits for it") {
		t.Fatal("why the machine waits isn't logged")
	}
	if _, ok := p.lostOn(m.in.Name); ok {
		t.Fatal("recorded before it was found")
	}
	e.gh.HideAssignment(j, 0)
	e.pass()
	if lost, ok := p.lostOn(m.in.Name); !ok || lost.JobID != j.ID {
		t.Fatalf("lost = %+v, %v", lost, ok)
	}
	if e.exists(m.in.Name) {
		t.Fatal("the machine wasn't deleted")
	}
}

// TestAJobThatEndedWhileTheMachineWaitedIsFound: a job GitHub ended while
// the retired machine waited, so its run has finished, is found among the
// runs finished since a day before the claim.
func TestAJobThatEndedWhileTheMachineWaitedIsFound(t *testing.T) {
	e := setup(t)
	p := e.withLost()
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	e.gh.HideAssignment(j, 1000)
	e.clock.Advance(machines.IdleLimit)
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("the machine didn't wait")
	}
	// GitHub ends the job, its runner gone, and the run with it.
	e.gh.HideAssignment(j, 0)
	e.gh.CompleteJobByID(j.ID, "failure")
	if st, _ := e.gh.RunState(run); st != "completed" {
		t.Fatalf("run %s", st)
	}
	e.pass()
	if lost, ok := p.lostOn(m.in.Name); !ok || lost.JobID != j.ID {
		t.Fatalf("lost = %+v, %v", lost, ok)
	}
	if e.exists(m.in.Name) {
		t.Fatal("the machine wasn't deleted")
	}
}

// TestARetiredMachineWaitsWhileGitHubCantBeRead: while GitHub can't be read,
// or limits the service's requests, a retired machine's runner can't be
// settled, and the machine isn't deleted.
func TestARetiredMachineWaitsWhileGitHubCantBeRead(t *testing.T) {
	for name, fail := range map[string]func(e *env){
		"removing the runner fails": func(e *env) {
			e.gh.Faults().Add(fault.Rule{Method: http.MethodDelete,
				Match:   func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/actions/runners/") },
				Respond: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }})
		},
		"the runs are rate limited": func(e *env) {
			// Resetting at once, so that the client, which keeps the limit's
			// reset, tries again once the faults go.
			e.gh.RateLimit("/repos/lanescape/app/actions/runs", 1000, 0)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			p := e.withLost()
			run := e.mainRun()
			j := e.queue(run, sel2)
			e.pass()
			m := e.boot(e.one())
			m.taken(j)
			e.clock.Advance(machines.IdleLimit)
			fail(e)
			e.pass()
			if !e.exists(m.in.Name) {
				t.Fatal("a retired machine was deleted while GitHub couldn't be read")
			}
			e.gh.Faults().Reset()
			e.pass()
			if lost, ok := p.lostOn(m.in.Name); !ok || lost.JobID != j.ID {
				t.Fatalf("lost = %+v, %v", lost, ok)
			}
			if e.exists(m.in.Name) {
				t.Fatal("the machine wasn't deleted once GitHub could be read")
			}
		})
	}
}

// TestAJobTheCheckWouldRefuseIsntRecorded: a job the job-started check would
// have refused, such as a fork's pull request's, isn't recorded lost, and so
// isn't rerun.
func TestAJobTheCheckWouldRefuseIsntRecorded(t *testing.T) {
	e := setup(t)
	p := e.withLost()
	mj := e.queue(e.mainRun(), sel2)
	e.pass()
	// The job the machine was made for is cancelled, and a fork's job,
	// which gets no machine of its own, takes it.
	e.gh.CompleteJobByID(mj.ID, "cancelled")
	m := e.boot(e.one())
	fork := e.forkPull()
	fj := e.queue(fork, sel2)
	m.taken(fj)
	e.clock.Advance(machines.IdleLimit)
	e.pass()
	if _, ok := p.lostOn(m.in.Name); ok {
		t.Fatal("a fork's job was recorded lost")
	}
	if !e.logged("its check would have refused it") {
		t.Fatal("why isn't logged")
	}
	if e.exists(m.in.Name) {
		t.Fatal("the machine wasn't deleted")
	}
}

// TestALostJobThatCantBeRecordedWaits: a lost job whose record can't be
// written keeps its machine for a later pass.
func TestALostJobThatCantBeRecordedWaits(t *testing.T) {
	e := setup(t)
	p := e.withLost()
	p.failLost = context.DeadlineExceeded
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	e.clock.Advance(machines.IdleLimit)
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("deleted before its lost job was recorded")
	}
	p.mu.Lock()
	p.failLost = nil
	p.mu.Unlock()
	e.pass()
	if _, ok := p.lostOn(m.in.Name); !ok || e.exists(m.in.Name) {
		t.Fatal("not recorded and deleted once it could be")
	}
}

// TestARetiredMachineGoesAtItsAgeLimit: a retired machine whose runner's job
// can't be settled waits, but not past its age limit: then it goes, and an
// error says a job it took may be lost.
func TestARetiredMachineGoesAtItsAgeLimit(t *testing.T) {
	e := setup(t)
	e.withLost()
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	e.gh.HideAssignment(j, 1000000)
	e.clock.Advance(machines.IdleLimit)
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("the machine didn't wait")
	}
	e.clock.Advance(machines.MaxAge)
	e.pass()
	if e.exists(m.in.Name) {
		t.Fatal("a retired machine outlived its age limit")
	}
	if !e.logged("a job it took may be lost") {
		t.Fatal("not logged")
	}
}
