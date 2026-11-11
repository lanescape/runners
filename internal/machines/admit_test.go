package machines_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/machines"
)

func TestAnAdmittedJobGetsNoWorkDiskFromItsCheck(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	// The hook may send the wrong run: it's only where to look first.
	code, body := m.startedBody(run.ID + 1000)
	if code != http.StatusOK || len(body) != 0 {
		t.Fatalf("job-started check: %d %v; want 200 {}", code, body)
	}
	// A job asks for its work disk in a step; the check attaches none.
	in, _ := e.gce.Instance(m.in.Zone, m.in.Name)
	if len(in.Disks) != 1 || len(e.gce.Disks()) != 1 {
		t.Fatalf("disks = %v; want the boot disk alone", in.Disks)
	}
	if _, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json"); ok {
		t.Fatal("the check recorded a work disk")
	}
	adm, ok := e.record("machines/" + m.in.Name + "/admission.json")
	if !ok || adm["allowed"] != true || adm["job_id"] != float64(j.ID) || adm["run_id"] != float64(run.ID) || adm["scope"] != "main" {
		t.Fatalf("admission = %v", adm)
	}
	// Asking again gets the same answer, and nothing more.
	if code := m.started(0); code != http.StatusOK {
		t.Fatalf("again: %d", code)
	}
	if n := len(e.gce.Disks()); n != 1 {
		t.Fatalf("%d disks, want the boot disk alone", n)
	}
	m.release(true)
	m.finished()
	if code := m.report(); code != http.StatusNoContent {
		t.Fatalf("report: %d", code)
	}
}

func TestTheCheckWaitsForGitHubToShowTheAssignment(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	e.gh.HideAssignment(j, 5)
	if code := m.started(run.ID); code != http.StatusOK {
		t.Fatalf("job-started check: %d", code)
	}
}

func TestTheCheckWithNoAnswer(t *testing.T) {
	t.Run("no job on the runner", func(t *testing.T) {
		e := setup(t)
		e.queue(e.mainRun(), sel2)
		e.pass()
		m := &machine{e: e, in: e.one()}
		if code := m.started(0); code != http.StatusServiceUnavailable {
			t.Fatalf("job-started check: %d, want a retryable 503", code)
		}
		if _, ok := e.record("machines/" + m.in.Name + "/admission.json"); ok {
			t.Fatal("a decision was recorded with no job")
		}
	})
	t.Run("GitHub down", func(t *testing.T) {
		e := setup(t)
		run := e.mainRun()
		j := e.queue(run, sel2)
		e.pass()
		m := e.boot(e.one())
		m.taken(j)
		e.gh.ServerError("/repos/lanescape/app/actions/", http.StatusBadGateway, 0)
		if code := m.started(run.ID); code != http.StatusServiceUnavailable {
			t.Fatalf("job-started check: %d, want a retryable 503", code)
		}
		e.gh.Faults().Reset()
		if code := m.started(run.ID); code != http.StatusOK {
			t.Fatalf("once GitHub is back: %d", code)
		}
	})
	t.Run("the bucket loses answers", func(t *testing.T) {
		e := setup(t)
		run := e.mainRun()
		j := e.queue(run, sel2)
		e.pass()
		m := e.boot(e.one())
		m.taken(j)
		e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 3, After: true,
			Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
		for code := m.started(run.ID); code != http.StatusOK; code = m.started(run.ID) {
			if code != http.StatusServiceUnavailable {
				t.Fatalf("job-started check: %d", code)
			}
		}
		adm, _ := e.record("machines/" + m.in.Name + "/admission.json")
		if adm["allowed"] != true {
			t.Fatalf("admission = %v", adm)
		}
	})
}

func TestADiskRequestWhoseAnswersAreLost(t *testing.T) {
	t.Run("Compute Engine loses answers", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("main", map[string]string{".github/runners.json": declares})
		m, _ := e.admitted(sel2)
		e.gce.LoseResponse(http.MethodPost, "/disks", 1)
		e.gce.LoseResponse(http.MethodPost, "/attachDisk", 1)
		m.diskReady("build")
		in, _ := e.gce.Instance(m.in.Zone, m.in.Name)
		if in.Disks[machines.WorkDevice] != machines.WorkDisk(m.in.Name) || len(e.gce.Disks()) != 2 {
			t.Fatal("the work disk wasn't made and attached once")
		}
	})
	t.Run("the bucket loses answers", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("main", map[string]string{".github/runners.json": declares})
		m, _ := e.admitted(sel2)
		e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 3, After: true,
			Match: func(r *http.Request) bool {
				return strings.Contains(r.URL.RawQuery, "disks") || strings.Contains(r.URL.Path, "disks")
			},
			Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
		m.diskReady("build")
		if in, _ := e.gce.Instance(m.in.Zone, m.in.Name); in.Disks[machines.WorkDevice] != machines.WorkDisk(m.in.Name) || len(e.gce.Disks()) != 2 {
			t.Fatal("the work disk wasn't made and attached once")
		}
	})
}

func TestIdentity(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	e.queue(run, sel2)
	e.pass()
	in := e.one()
	full := e.gce.IdentityClaims(in.Zone, in.Name, audience, true)
	ce := full["google"].(map[string]any)["compute_engine"].(map[string]any)
	ce["instance_id"] = strconv.FormatUint(in.ID+1, 10)
	wrongID := e.gce.Identity.Mint(full)
	for name, tc := range map[string]struct {
		token string
		code  int
	}{
		"no token":                 {"", http.StatusUnauthorized},
		"the standard format":      {e.gce.Identity.Mint(e.gce.IdentityClaims(in.Zone, in.Name, audience, false)), http.StatusUnauthorized},
		"another audience":         {e.gce.Identity.Mint(e.gce.IdentityClaims(in.Zone, in.Name, "https://other.example.test", true)), http.StatusUnauthorized},
		"another instance's ID":    {wrongID, http.StatusForbidden},
		"a GitHub token":           {e.gh.ActionsToken(run, ghemu.Job{Audience: audience}, nil), http.StatusUnauthorized},
		"a machine it didn't make": {e.otherMachineToken(), http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _ := e.post("/v1/jobs/started", tc.token, machines.StartedRequest{}); code != tc.code {
				t.Fatalf("job-started: %d, want %d", code, tc.code)
			}
			if code, _ := e.post("/v1/jobs/finished", tc.token, machines.FinishedRequest{}); code != tc.code {
				t.Fatalf("job-finished: %d, want %d", code, tc.code)
			}
			if code, _ := e.post(machines.DisksPath, tc.token, map[string]string{"family": "build"}); code != tc.code {
				t.Fatalf("work disk: %d, want %d", code, tc.code)
			}
			if code, _ := e.post(machines.SavePath, tc.token, map[string]bool{"save": true}); code != tc.code {
				t.Fatalf("save: %d, want %d", code, tc.code)
			}
		})
	}
	if code, _ := e.post("/v1/jobs/started", e.token(in), map[string]any{"job_id": 1}); code != http.StatusBadRequest {
		t.Fatalf("a field the hook doesn't send: %d", code)
	}
	// The report says only that the job is done.
	if code, _ := e.post("/v1/jobs/finished", e.token(in), map[string]any{"save": true}); code != http.StatusBadRequest {
		t.Fatalf("a report that asks to save: %d", code)
	}
	if e.logged(wrongID) {
		t.Fatal("a token was logged")
	}
}

// otherMachineToken is the token of a machine of the project's that the
// service didn't make, on the machines' account.
func (e *env) otherMachineToken() string {
	e.t.Helper()
	e.queue(e.mainRun(), sel2)
	e.pass()
	var in = e.ours()[len(e.ours())-1]
	c := e.gce.IdentityClaims(in.Zone, in.Name, audience, true)
	ce := c["google"].(map[string]any)["compute_engine"].(map[string]any)
	ce["instance_name"] = "lsr-shared"
	return e.gce.Identity.Mint(c)
}

// refused checks that the job-started check refused a machine's job: it
// answered 403, stopped the machine, and gave it no work disk.
func (m *machine) refused(runID int64) {
	m.e.t.Helper()
	if code := m.started(runID); code != http.StatusForbidden {
		m.e.t.Fatalf("job-started check: %d, want 403", code)
	}
	in, _ := m.e.gce.Instance(m.in.Zone, m.in.Name)
	if in.Status != "TERMINATED" {
		m.e.t.Fatalf("a refused machine is %s", in.Status)
	}
	if len(in.Disks) != 1 {
		m.e.t.Fatalf("a refused machine has disks %v", in.Disks)
	}
	if code := m.started(runID); code != http.StatusForbidden && code != http.StatusServiceUnavailable {
		m.e.t.Fatalf("asking again: %d", code)
	}
}

func TestRefusals(t *testing.T) {
	t.Run("a job that asked only for self-hosted", func(t *testing.T) {
		e := setup(t)
		run := e.mainRun()
		stray := e.queue(run, "self-hosted")
		meant := e.queue(run, sel2)
		e.pass()
		// A runner's only label is its selector, so GitHub never hands
		// it a job that asked only for self-hosted.
		m := e.boot(e.one())
		m.taken(meant)
		if e.gh.Job(stray.ID).Status != "queued" {
			t.Fatal("a job that asked only for self-hosted was given the service's runner")
		}
	})
	t.Run("another run's job of the selector", func(t *testing.T) {
		e := setup(t)
		main := e.mainRun()
		meant := e.queue(main, sel2)
		e.repo.Commit("feature", map[string]string{"f": "1"})
		p := e.gh.PullRequest(e.repo, "main", e.repo, "feature", e.org)
		pr := e.gh.PullRequestRun(p)
		e.pass()
		// The machine made for main's job is general: the pull request's
		// job, of the same selector, takes it, and is admitted for itself.
		other := e.queue(pr, sel2)
		e.gh.CompleteJob(meant, "success")
		m := e.boot(e.one())
		m.taken(other)
		if code := m.started(pr.ID); code != http.StatusOK {
			t.Fatalf("job-started check: %d", code)
		}
		adm, _ := e.record("machines/" + m.in.Name + "/admission.json")
		if adm["allowed"] != true || adm["run_id"] != float64(pr.ID) || adm["scope"] != "pr"+strconv.Itoa(p.Number) {
			t.Fatalf("admission = %v", adm)
		}
	})
	t.Run("a fork's job", func(t *testing.T) {
		e := setup(t)
		main := e.mainRun()
		meant := e.queue(main, sel2)
		e.pass()
		// The fork's two jobs name the service's selector after a
		// maintainer approved its run.
		fork := e.forkPull()
		first := e.queue(fork, sel2)
		e.queue(fork, sel2)
		e.gh.CompleteJob(meant, "success")
		m := e.boot(e.one())
		m.taken(first)
		m.refused(fork.ID)
		rec, _ := e.record("refusals/5001/" + strconv.FormatInt(fork.ID, 10) + "-1.json")
		if rec["cancelled_at"] == nil {
			t.Fatalf("refusal = %v; the run wasn't cancelled", rec)
		}
		// Until the run is shown finished, no machine for its jobs; another
		// run's job of the same selector gets one.
		e.queue(e.mainRun(), sel2)
		e.pass()
		e.wantMachines(1)
		// The refused job's runner is gone, so an ordinary cancellation
		// doesn't finish the run.
		if st, _ := e.gh.RunState(fork); st == "completed" {
			t.Fatal("an ordinary cancellation finished a run whose job's runner is gone")
		}
		e.forceCancelled(fork)
		e.pass()
		e.wantMachines(1)
	})
	t.Run("an unapproved contribution's plain job runs unbound", func(t *testing.T) {
		e := setup(t)
		crun, _ := e.contribution(false)
		j := e.gh.Queue(crun, ghemu.JobSpec{Labels: []string{sel2}})
		e.pass()
		m := e.boot(e.one())
		m.taken(j)
		if code := m.started(crun.ID); code != http.StatusOK {
			t.Fatalf("job-started check: %d", code)
		}
		adm, _ := e.record("machines/" + m.in.Name + "/admission.json")
		if adm["allowed"] != true || adm["unbound"] != true {
			t.Fatalf("admission = %v", adm)
		}
		if _, ok := e.record("bindings/5001/" + strconv.FormatInt(crun.ID, 10) + ".json"); ok {
			t.Fatal("a run no maintainer approved was bound")
		}
	})
	t.Run("an unapproved contribution's other job is failed", func(t *testing.T) {
		e := setup(t)
		crun, _ := e.contribution(false)
		j := e.queue(crun, "lanescape-80vcpu-linux-amd64-spot")
		e.pass()
		// The job gets an ordinary machine of its own selector: the reason
		// is decided at its check.
		in := e.one()
		if !strings.HasSuffix(in.MachineType, "n4-highcpu-80") || in.Provisioning != "SPOT" ||
			in.Metadata[machines.MetaSelector] != "lanescape-80vcpu-linux-amd64-spot" {
			t.Fatalf("machine = %+v; want one of the job's own selector", in)
		}
		m := e.boot(in)
		m.taken(j)
		m.failed(crun.ID, "isn't approved")
		e.forceCancelled(crun)
	})
	t.Run("an approved contribution", func(t *testing.T) {
		e := setup(t)
		crun, p := e.contribution(true)
		j := e.gh.Queue(crun, ghemu.JobSpec{Labels: []string{sel2}, Environment: machines.ContributionEnvironment})
		e.pass()
		m := e.boot(e.one())
		m.taken(j)
		if code := m.started(crun.ID); code != http.StatusOK {
			t.Fatalf("job-started check: %d", code)
		}
		adm, _ := e.record("machines/" + m.in.Name + "/admission.json")
		if adm["contribution"] != true || adm["scope"] != "pr"+strconv.Itoa(p.Number) {
			t.Fatalf("admission = %v", adm)
		}
	})
}

// failed checks that the job-started check failed a machine's job, saying
// why: it answered 403 with the reason, stopped the machine, and gave it no
// work disk.
func (m *machine) failed(runID int64, reason string) {
	m.e.t.Helper()
	code, body := m.startedBody(runID)
	if code != http.StatusForbidden || !strings.Contains(fmt.Sprint(body["reason"]), reason) {
		m.e.t.Fatalf("job-started check: %d %v, want 403 saying %q", code, body, reason)
	}
	m.e.eventually("the failed job's machine stops", func() bool {
		in, _ := m.e.gce.Instance(m.in.Zone, m.in.Name)
		return in.Status == "TERMINATED"
	})
	if in, _ := m.e.gce.Instance(m.in.Zone, m.in.Name); len(in.Disks) != 1 {
		m.e.t.Fatalf("a failed job's machine has disks %v", in.Disks)
	}
	m.e.eventually("the failed job's run is refused", func() bool {
		_, ok := m.e.record("refusals/5001/" + strconv.FormatInt(runID, 10) + "-1.json")
		return ok
	})
}

// forceCancelled checks that a refused run, which an ordinary cancellation
// doesn't finish, is force-cancelled once that's had its time.
func (e *env) forceCancelled(run *ghemu.Run) {
	e.t.Helper()
	e.clock.Advance(machines.ForceCancel)
	e.pass()
	if st, c := e.gh.RunState(run); st != "completed" || c != "cancelled" {
		e.t.Fatalf("after %s: %s %s; want it force-cancelled", machines.ForceCancel, st, c)
	}
	e.pass()
	rec, _ := e.record("refusals/5001/" + strconv.FormatInt(run.ID, 10) + "-1.json")
	if rec["finished_at"] == nil {
		e.t.Fatalf("refusal = %v; want it finished", rec)
	}
}

// contributionFor is another approved pull_request_target run of p.
func (e *env) contributionFor(p *ghemu.Pull) (*ghemu.Run, *ghemu.Pull) {
	run := e.gh.PullRequestTargetRun(p)
	e.gh.Approve(run, machines.ContributionEnvironment, e.maintainer)
	return run, p
}

func TestTheHookCanBeTamperedWith(t *testing.T) {
	// A job whose hook never asks gets no work disk; its machine goes once
	// it's been up for the idle limit with no admission.
	e := setup(t)
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	m.release(true)
	m.finished()
	in, _ := e.gce.Instance(m.in.Zone, m.in.Name)
	if len(in.Disks) != 1 {
		t.Fatal("a job that never asked got a disk")
	}
	e.clock.Advance(machines.IdleLimit)
	e.pass()
	if _, ok := e.gce.Instance(m.in.Zone, m.in.Name); ok {
		t.Fatal("the machine wasn't deleted")
	}
}

func TestAPassRecordsARefusalTheCheckDidnt(t *testing.T) {
	e := setup(t)
	e.queue(e.mainRun(), sel2)
	e.pass()
	// A fork's job took the machine, and the check recorded its decision
	// and stopped short of the rest.
	fork := e.forkPull()
	j := e.queue(fork, sel2)
	m := e.one()
	adm := machines.Admission{Machine: m.Name, Zone: m.Zone, RepositoryID: repoID, JobID: j.ID, RunID: fork.ID, RunAttempt: 1, Selector: sel2,
		Reason: "the run is a fork's pull request's", Cancel: true}
	b, _ := json.Marshal(adm)
	e.gcs.Put("state", "machines/"+m.Name+"/admission.json", b)
	e.pass()
	rec, ok := e.record("refusals/5001/" + strconv.FormatInt(fork.ID, 10) + "-1.json")
	if !ok || rec["reason"] != adm.Reason {
		t.Fatalf("refusal = %v; the pass didn't record the refused run from its admission", rec)
	}
	if st, c := e.gh.RunState(fork); st != "completed" || c != "cancelled" {
		t.Fatalf("fork's run: %s %s", st, c)
	}
}

func TestAWorkDiskThatCouldntBeMadeIsTriedAgain(t *testing.T) {
	e := setup(t)
	e.repo.Commit("main", map[string]string{".github/runners.json": declares})
	m, _ := e.admitted(sel2)
	e.gce.StockoutDisks(m.in.Zone, true)
	// The request is answered: the disk is being made, and the caller asks
	// again.
	if code, body := m.disk("build"); code != http.StatusOK || body["state"] != machines.DiskStateMaking {
		t.Fatalf("with no disk capacity: %d %v, want 200 making", code, body)
	}
	// Compute Engine answers a request ID it has seen with its first
	// outcome, so the next try needs another.
	e.gce.StockoutDisks(m.in.Zone, false)
	if code, body := m.disk("build"); code != http.StatusOK || body["state"] != machines.DiskStateReady {
		t.Fatalf("once there's capacity: %d %v, want 200 ready", code, body)
	}
}

// vanishing chooses an image that isn't there the first time, as one
// deleted just after it was chosen would be, and none after that.
type vanishing struct {
	mu    sync.Mutex
	calls int
}

func (v *vanishing) Choose(_ context.Context, _ machines.Machine, _ machines.Admission, size int64) (string, int64, string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.calls == 1 {
		return "lsr-5001-0123456789abcdef0123-1-gonegone", size, "main_exact", nil
	}
	return "", size, "none", nil
}

func (v *vanishing) choices() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

func TestAWorkDiskWhoseImageWentIsChosenAgain(t *testing.T) {
	e := setup(t)
	v := &vanishing{}
	e.svc.Disks = v
	e.repo.Commit("main", map[string]string{".github/runners.json": declares})
	m, _ := e.admitted(sel2)
	if code, body := m.disk("build"); code != http.StatusOK || body["state"] != machines.DiskStateMaking {
		t.Fatalf("with its image gone: %d %v, want 200 making", code, body)
	}
	if _, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json"); ok {
		t.Fatal("the record of a disk whose image went was kept")
	}
	code, body := m.disk("build")
	if code != http.StatusOK || body["state"] != machines.DiskStateReady || body["restored"] != "none" || v.choices() != 2 {
		t.Fatalf("the next try: %d %v, %d choices; want it to choose again", code, body, v.choices())
	}
}

// An image that goes after its disk's request was accepted fails the
// request's operation, which Compute Engine keeps under its request ID: the
// next try, chosen again, is a new request.
func TestAWorkDiskWhoseImageWentInItsOperationIsChosenAgain(t *testing.T) {
	e := setup(t)
	e.svc.Disks = baseImage{}
	e.repo.Commit("main", map[string]string{".github/runners.json": declares})
	m, _ := e.admitted(sel2)
	e.gce.FailNextOperation("disks.insert", "RESOURCE_NOT_FOUND")
	if code, body := m.disk("build"); code != http.StatusOK || body["state"] != machines.DiskStateMaking {
		t.Fatalf("with its image gone: %d %v, want 200 making", code, body)
	}
	if code, body := m.disk("build"); code != http.StatusOK || body["state"] != machines.DiskStateReady {
		t.Fatalf("the next try, which chose the same image again: %d %v", code, body)
	}
}

// baseImage starts every work disk from a base image, as if it were a
// saved disk.
type baseImage struct{}

func (baseImage) Choose(_ context.Context, _ machines.Machine, _ machines.Admission, size int64) (string, int64, string, error) {
	return "lsr-base-amd64-new", size, "main_exact", nil
}

func TestAMachineThatIsntRunningGetsNoJob(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	tok := e.token(m.in)
	e.gce.Shutdown(m.in.Zone, m.in.Name)
	if code, _ := e.post("/v1/jobs/started", tok, machines.StartedRequest{RunID: run.ID}); code != http.StatusForbidden {
		t.Fatalf("a stopped machine's token: %d, want 403", code)
	}
	if _, ok := e.record("machines/" + m.in.Name + "/admission.json"); ok {
		t.Fatal("a decision was recorded for a machine that isn't running")
	}
}

func TestAReportStartsAPass(t *testing.T) {
	e := setup(t)
	kicked := make(chan struct{}, 1)
	e.svc.Kick = func() { kicked <- struct{}{} }
	m, _ := e.admitted(sel2)
	m.release(true)
	m.finished()
	m.report()
	select {
	case <-kicked:
	default:
		t.Fatal("a report didn't start a pass")
	}
}

func TestACancellationConflictIsntAFinish(t *testing.T) {
	e := setup(t)
	main := e.mainRun()
	meant := e.queue(main, sel2)
	e.pass()
	fork := e.forkPull()
	stray := e.queue(fork, sel2)
	e.gh.CompleteJob(meant, "success")
	// GitHub answers the first cancellation with a conflict, though the
	// run goes on.
	e.gh.ServerError("/repos/lanescape/app/actions/runs/"+strconv.FormatInt(fork.ID, 10)+"/cancel", http.StatusConflict, 1)
	m := e.boot(e.one())
	m.taken(stray)
	m.refused(fork.ID)
	rec, _ := e.record("refusals/5001/" + strconv.FormatInt(fork.ID, 10) + "-1.json")
	if rec["finished_at"] != nil {
		t.Fatalf("refusal = %v; a conflict isn't GitHub showing the run finished", rec)
	}
	e.forceCancelled(fork)
}

// refuseFork has a fork's job take a machine made for main's, and returns
// the fork's run once its job is refused.
func (e *env) refuseFork() (*ghemu.Run, *machine) {
	e.t.Helper()
	main := e.mainRun()
	meant := e.queue(main, sel2)
	e.pass()
	fork := e.forkPull()
	stray := e.queue(fork, sel2)
	e.gh.CompleteJob(meant, "success")
	m := e.boot(e.one())
	m.taken(stray)
	m.refused(fork.ID)
	return fork, m
}

func TestARefusalThatCantBeRecorded(t *testing.T) {
	e := setup(t)
	refusals := fault.Rule{Method: http.MethodPost,
		Match:   func(r *http.Request) bool { return strings.Contains(r.URL.RawQuery, "refusals") },
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")}
	e.gcs.Faults().Add(refusals)
	fork, m := e.refuseFork()
	// The machine and its admission stay, and its run is blocked, until the
	// refusal is recorded; another run's job of the selector gets a machine.
	e.queue(e.mainRun(), sel2)
	e.pass()
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("a refused machine went before its run's refusal was recorded")
	}
	e.wantMachines(2)
	if _, ok := e.record("machines/" + m.in.Name + "/admission.json"); !ok {
		t.Fatal("the only record of the refusal was removed")
	}
	e.gcs.Faults().Reset()
	e.pass()
	if _, ok := e.record("refusals/5001/" + strconv.FormatInt(fork.ID, 10) + "-1.json"); !ok {
		t.Fatal("the refusal wasn't recorded once it could be")
	}
	if e.exists(m.in.Name) {
		t.Fatal("the refused machine wasn't deleted once its refusal was recorded")
	}
}

func TestARerunIsRefusedAfresh(t *testing.T) {
	e := setup(t)
	fork, _ := e.refuseFork()
	e.forceCancelled(fork)
	// A maintainer reruns it, and its job takes another machine.
	e.gh.Rerun(fork)
	meant := e.queue(e.mainRun(), sel2)
	e.pass()
	stray := e.queue(fork, sel2)
	e.gh.CompleteJob(meant, "success")
	var in = e.ours()[len(e.ours())-1]
	m := e.boot(in)
	m.taken(stray)
	m.refused(fork.ID)
	rec, ok := e.record("refusals/5001/" + strconv.FormatInt(fork.ID, 10) + "-2.json")
	if !ok || rec["cancelled_at"] == nil || rec["finished_at"] != nil {
		t.Fatalf("the rerun's refusal = %v, %v; want it recorded and cancelled, not finished", rec, ok)
	}
	// The refused machine goes, and no other is made for its run's jobs;
	// another run's job gets its own.
	e.queue(e.mainRun(), sel2)
	e.pass()
	e.wantMachines(1)
}

func TestARefusedAttemptDoesntCancelARerun(t *testing.T) {
	e := setup(t)
	fork := e.forkPull()
	// Attempt 1 was refused, and finished; a maintainer reran it before
	// the refusal was followed up.
	rf := machines.Refusal{RepositoryID: repoID, RunID: fork.ID, RunAttempt: 1, RefusedAt: e.clock.Now()}
	b, _ := json.Marshal(rf)
	e.gcs.Put("state", "refusals/5001/"+strconv.FormatInt(fork.ID, 10)+"-1.json", b)
	e.gh.SetRunStatus(fork, "completed")
	e.gh.Rerun(fork)
	j := e.gh.Queue(fork, ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	e.pass()
	if st, _ := e.gh.RunState(fork); st == "completed" || e.gh.Job(j.ID).Status != "queued" {
		t.Fatal("following up an earlier attempt's refusal cancelled the rerun")
	}
	rec, _ := e.record("refusals/5001/" + strconv.FormatInt(fork.ID, 10) + "-1.json")
	if rec["finished_at"] == nil {
		t.Fatalf("refusal = %v; the earlier attempt is over", rec)
	}
}

func TestARefusedMachineThatWontStop(t *testing.T) {
	e := setup(t)
	main := e.mainRun()
	meant := e.queue(main, sel2)
	e.pass()
	fork := e.forkPull()
	stray := e.queue(fork, sel2)
	e.gh.CompleteJob(meant, "success")
	// Neither the stop nor the refusal's record gets through.
	e.gce.ServerError(http.MethodPost, "/stop", http.StatusInternalServerError, 0)
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost,
		Match:   func(r *http.Request) bool { return strings.Contains(r.URL.RawQuery, "refusals") },
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	m := e.boot(e.one())
	m.taken(stray)
	if code := m.started(fork.ID); code != http.StatusForbidden {
		t.Fatalf("job-started check: %d, want 403", code)
	}
	// Compute Engine recovers; storage doesn't.
	e.gce.Faults().Reset()
	e.pass()
	if in, _ := e.gce.Instance(m.in.Zone, m.in.Name); in.Status != "TERMINATED" {
		t.Fatalf("a refused machine is %s once Compute Engine is back", in.Status)
	}
}

func TestARecordedRefusalsMachineIsStoppedAgain(t *testing.T) {
	e := setup(t)
	main := e.mainRun()
	meant := e.queue(main, sel2)
	e.pass()
	fork := e.forkPull()
	stray := e.queue(fork, sel2)
	e.gh.CompleteJob(meant, "success")
	// The stop fails; the refusal is recorded; the boot log can't be saved.
	e.gce.ServerError(http.MethodPost, "/stop", http.StatusInternalServerError, 0)
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost,
		Match:   func(r *http.Request) bool { return strings.Contains(r.URL.RawQuery, "logs") },
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	m := e.boot(e.one())
	m.taken(stray)
	if code := m.started(fork.ID); code != http.StatusForbidden {
		t.Fatalf("job-started check: %d, want 403", code)
	}
	e.gce.Faults().Reset()
	e.pass()
	if in, ok := e.gce.Instance(m.in.Zone, m.in.Name); !ok || in.Status != "TERMINATED" {
		t.Fatalf("a refused machine waiting for its boot log is %s", in.Status)
	}
}

func TestAWorkDiskWhoseOperationFailed(t *testing.T) {
	e := setup(t)
	e.repo.Commit("main", map[string]string{".github/runners.json": declares})
	m, _ := e.admitted(sel2)
	e.gce.FailNextOperation("disks.insert", "INTERNAL_ERROR")
	if code, body := m.disk("build"); code != http.StatusOK || body["state"] != machines.DiskStateMaking {
		t.Fatalf("work disk request: %d %v, want 200 making", code, body)
	}
	if code, body := m.disk("build"); code != http.StatusOK || body["state"] != machines.DiskStateReady {
		t.Fatalf("asked again: %d %v; a finished operation's failure needs a new request", code, body)
	}
}

func TestARefusedMachineIsStoppedWhateverElseWaits(t *testing.T) {
	e := setup(t)
	main := e.mainRun()
	meant := e.queue(main, sel2)
	e.pass()
	fork := e.forkPull()
	stray := e.queue(fork, sel2)
	e.gh.CompleteJob(meant, "success")
	e.gce.ServerError(http.MethodPost, "/stop", http.StatusInternalServerError, 0)
	m := e.boot(e.one())
	m.taken(stray)
	if code := m.started(fork.ID); code != http.StatusForbidden {
		t.Fatalf("job-started check: %d, want 403", code)
	}
	// Compute Engine recovers, but the disks' records can't be listed.
	e.gce.Faults().Reset()
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1, Match: func(r *http.Request) bool { return r.URL.Query().Get("prefix") == "disks/" },
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	e.pass()
	if in, _ := e.gce.Instance(m.in.Zone, m.in.Name); in.Status != "TERMINATED" {
		t.Fatalf("a refused machine is %s while other records couldn't be read", in.Status)
	}
}
