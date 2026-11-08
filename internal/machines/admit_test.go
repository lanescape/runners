package machines_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/lanescape/runners/config"
	gceemu "github.com/lanescape/runners/emulators/compute"
	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/machines"
)

func TestAnAdmittedJobGetsItsWorkDisk(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	// The hook may send the wrong run: it's only where to look first.
	if code := m.started(run.ID + 1000); code != http.StatusOK {
		t.Fatalf("job-started check: %d", code)
	}
	in, _ := e.gce.Instance(m.in.Zone, m.in.Name)
	if in.Disks[machines.WorkDevice] != machines.WorkDisk(m.in.Name) {
		t.Fatalf("disks = %v; want the work disk attached as %q", in.Disks, machines.WorkDevice)
	}
	var work bool
	for _, d := range e.gce.Disks() {
		if d.Name == machines.WorkDisk(m.in.Name) {
			work = true
			if d.SizeGB != 50 || d.SourceImage != "" || d.IOPS != 3000 || d.Throughput != 400 || d.Labels[machines.LabelMachine] != m.in.Name {
				t.Fatalf("work disk = %+v; want an empty 50 GB disk", d)
			}
		}
	}
	if !work {
		t.Fatal("no work disk")
	}
	rec, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json")
	if !ok || rec["state"] != machines.DiskAttached || rec["machine"] != m.in.Name || rec["job_id"] != float64(j.ID) {
		t.Fatalf("disk record = %v", rec)
	}
	adm, ok := e.record("machines/" + m.in.Name + "/admission.json")
	if !ok || adm["allowed"] != true || adm["job_id"] != float64(j.ID) || adm["run_id"] != float64(run.ID) {
		t.Fatalf("admission = %v", adm)
	}
	// Asking again gets the same answer, and nothing more.
	if code := m.started(0); code != http.StatusOK {
		t.Fatalf("again: %d", code)
	}
	if n := len(e.gce.Disks()); n != 2 {
		t.Fatalf("%d disks, want the boot and work disks", n)
	}
	m.release(true)
	m.finished()
	if code := m.report(false); code != http.StatusNoContent {
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
		if n := len(e.gce.Disks()); n != 2 {
			t.Fatalf("%d disks, want the boot and work disks", n)
		}
	})
	t.Run("Compute Engine loses answers", func(t *testing.T) {
		e := setup(t)
		run := e.mainRun()
		j := e.queue(run, sel2)
		e.pass()
		m := e.boot(e.one())
		m.taken(j)
		e.gce.LoseResponse(http.MethodPost, "/disks", 1)
		e.gce.LoseResponse(http.MethodPost, "/attachDisk", 1)
		for code := m.started(run.ID); code != http.StatusOK; code = m.started(run.ID) {
			if code != http.StatusServiceUnavailable {
				t.Fatalf("job-started check: %d", code)
			}
		}
		in, _ := e.gce.Instance(m.in.Zone, m.in.Name)
		if in.Disks[machines.WorkDevice] == "" || len(e.gce.Disks()) != 2 {
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
		"another audience":         {e.gce.Identity.Mint(e.gce.IdentityClaims(in.Zone, in.Name, "https://proxy.example.test", true)), http.StatusUnauthorized},
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
		})
	}
	if code, _ := e.post("/v1/jobs/started", e.token(in), map[string]any{"job_id": 1}); code != http.StatusBadRequest {
		t.Fatalf("a field the hook doesn't send: %d", code)
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
	ce["instance_name"] = "lsr-proxy-git"
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
		m := e.boot(e.one())
		m.taken(stray)
		m.refused(run.ID)
		if st, _ := e.gh.RunState(run); st == "completed" {
			t.Fatal("the run was cancelled; only fork and scope refusals cancel")
		}
		// The machine goes, and the job it was made for gets another.
		e.pass()
		ms := e.wantMachines(1)
		if ms[0].Name == m.in.Name || ms[0].Labels[machines.LabelJob] != strconv.FormatInt(meant.ID, 10) {
			t.Fatalf("machines = %+v", ms)
		}
	})
	t.Run("a job of a scope that isn't its run's", func(t *testing.T) {
		e := setup(t)
		main := e.mainRun()
		meant := e.queue(main, sel2)
		e.repo.Commit("feature", map[string]string{"f": "1"})
		p := e.gh.PullRequest(e.repo, "main", e.repo, "feature", e.org)
		pr := e.gh.PullRequestRun(p)
		e.bind(pr, ghemu.Job{}, bindingRequest())
		e.pass()
		// The pull request's job claims main's scope, and is the only one
		// queued when the machine's runner connects.
		claim := e.queue(pr, sel2)
		e.gh.CompleteJob(meant, "success")
		m := e.boot(e.one())
		m.taken(claim)
		m.refused(pr.ID)
		if _, ok := e.record("refusals/5001/" + strconv.FormatInt(pr.ID, 10) + "-1.json"); !ok {
			t.Fatal("the refusal wasn't recorded")
		}
		e.forceCancelled(pr)
	})
	t.Run("a fork's job", func(t *testing.T) {
		e := setup(t)
		main := e.mainRun()
		meant := e.queue(main, sel2)
		e.pass()
		// The fork's two jobs name main's selector after a maintainer
		// approved its run.
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
		// Until the run is shown finished, no machine for its selectors,
		// even for another run's job.
		e.queue(e.mainRun(), sel2)
		e.pass()
		e.wantMachines(0)
		// The refused job's runner is gone, so an ordinary cancellation
		// doesn't finish the run.
		if st, _ := e.gh.RunState(fork); st == "completed" {
			t.Fatal("an ordinary cancellation finished a run whose job's runner is gone")
		}
		e.forceCancelled(fork)
		e.pass()
		e.wantMachines(1)
	})
	t.Run("an unapproved contribution", func(t *testing.T) {
		e := setup(t)
		crun, p := e.contribution(false)
		csel := "lanescape-2vcpu-linux-amd64-pr" + strconv.Itoa(p.Number)
		// The machine is made for an approved run's job of the same pull
		// request, and the unapproved run's job takes it.
		approved, _ := e.contributionFor(p)
		meant := e.gh.Queue(approved, ghemu.JobSpec{Labels: []string{csel}, Environment: machines.ContributionEnvironment})
		e.pass()
		// The unapproved run's job doesn't name the environment, so it
		// doesn't wait for a review.
		stray := e.queue(crun, csel)
		e.gh.CompleteJob(meant, "success")
		m := e.boot(e.one())
		m.taken(stray)
		m.refused(crun.ID)
		e.forceCancelled(crun)
	})
	t.Run("an approved contribution", func(t *testing.T) {
		e := setup(t)
		crun, p := e.contribution(true)
		j := e.gh.Queue(crun, ghemu.JobSpec{Labels: []string{"lanescape-2vcpu-linux-amd64-pr" + strconv.Itoa(p.Number)},
			Environment: machines.ContributionEnvironment})
		e.pass()
		m := e.boot(e.one())
		m.taken(j)
		if code := m.started(crun.ID); code != http.StatusOK {
			t.Fatalf("job-started check: %d", code)
		}
		adm, _ := e.record("machines/" + m.in.Name + "/admission.json")
		if adm["contribution"] != true {
			t.Fatalf("admission = %v", adm)
		}
	})
	t.Run("a job with GitHub's own labels", func(t *testing.T) {
		e := setup(t)
		run := e.mainRun()
		meant := e.queue(run, sel2)
		e.pass()
		stray := e.queue(run, sel2, "Linux", "self-hosted")
		e.gh.CompleteJob(meant, "success")
		m := e.boot(e.one())
		m.taken(stray)
		// Its labels are the machine's selector and GitHub's own, which the
		// runner has: it's allowed.
		if code := m.started(run.ID); code != http.StatusOK {
			t.Fatalf("job-started check: %d", code)
		}
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
	e.bind(run, ghemu.Job{WorkflowRef: bootstrap}, eventRequest(run))
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
	fork := e.forkPull()
	j := e.queue(fork, sel2)
	// The check recorded its decision and stopped short of the rest.
	e.queue(e.mainRun(), sel2)
	e.pass()
	m := e.one()
	adm := machines.Admission{Machine: m.Name, Zone: m.Zone, RepositoryID: repoID, JobID: j.ID, RunID: fork.ID, RunAttempt: 1, Selector: sel2,
		Reason: "the run is a fork's pull request's", Cancel: true}
	b, _ := json.Marshal(adm)
	e.gcs.Put("state", "machines/"+m.Name+"/admission.json", b)
	e.pass()
	if _, ok := e.record("refusals/5001/" + strconv.FormatInt(fork.ID, 10) + "-1.json"); !ok {
		t.Fatal("the pass didn't record the refused run")
	}
	if st, c := e.gh.RunState(fork); st != "completed" || c != "cancelled" {
		t.Fatalf("fork's run: %s %s", st, c)
	}
}

func TestAWorkDiskThatCouldntBeMadeIsTriedAgain(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	e.gce.StockoutDisks(m.in.Zone, true)
	if code := m.started(run.ID); code != http.StatusServiceUnavailable {
		t.Fatalf("with no disk capacity: %d, want a retryable 503", code)
	}
	// Compute Engine answers a request ID it has seen with its first
	// outcome, so the next try needs another.
	e.gce.StockoutDisks(m.in.Zone, false)
	if code := m.started(run.ID); code != http.StatusOK {
		t.Fatalf("once there's capacity: %d", code)
	}
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
	m.report(false)
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

func TestAContributionOnAnOrdinaryMachine(t *testing.T) {
	e := setup(t)
	e.cfg.Limits.Repositories = map[int64]config.Override{repoID: {ContributionMachines: ptr(0)}}
	// A contribution whose bootstrap didn't run binds read, and is
	// approved.
	e.fork.Commit("feature", map[string]string{"src.go": "package app"})
	p := e.gh.PullRequest(e.repo, "main", e.fork, "feature", e.contributor)
	crun := e.gh.PullRequestTargetRun(p)
	e.bind(crun, ghemu.Job{}, bindingRequest())
	e.gh.Approve(crun, machines.ContributionEnvironment, e.maintainer)
	read := "lanescape-2vcpu-linux-amd64-read"
	stray := e.gh.Queue(crun, ghemu.JobSpec{Labels: []string{read}, Environment: machines.ContributionEnvironment})
	other := e.gh.EventRun(e.repo, "workflow_dispatch", "main")
	e.bind(other, ghemu.Job{}, bindingRequest())
	e.queue(other, read)
	e.pass()
	m := e.boot(e.one())
	m.taken(stray)
	m.refused(crun.ID)
	if adm, _ := e.record("machines/" + m.in.Name + "/admission.json"); !strings.Contains(adm["reason"].(string), "not made for contributions") {
		t.Fatalf("admission = %v", adm)
	}
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
	// The machine and its admission stay, and its selector is blocked,
	// until the refusal is recorded.
	e.queue(e.mainRun(), sel2)
	e.pass()
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("a refused machine went before its run's refusal was recorded")
	}
	e.wantMachines(1)
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
	// The refused machine goes, and no other is made for its selector.
	e.queue(e.mainRun(), sel2)
	e.pass()
	e.wantMachines(0)
}

func TestAnOrdinaryJobOnAContributionMachine(t *testing.T) {
	e := setup(t)
	e.cfg.Limits.Repositories = map[int64]config.Override{repoID: {Machines: ptr(1), ContributionMachines: ptr(1)}}
	read := "lanescape-2vcpu-linux-amd64-read"
	other := e.gh.EventRun(e.repo, "workflow_dispatch", "main")
	e.bind(other, ghemu.Job{}, bindingRequest())
	// The consumer's own job is older, and its machine is made first; the
	// contribution's comes next.
	ordinary := e.queue(other, read)
	e.fork.Commit("feature", map[string]string{"src.go": "package app"})
	p := e.gh.PullRequest(e.repo, "main", e.fork, "feature", e.contributor)
	crun := e.gh.PullRequestTargetRun(p)
	e.bind(crun, ghemu.Job{}, bindingRequest())
	e.gh.Approve(crun, machines.ContributionEnvironment, e.maintainer)
	e.gh.Queue(crun, ghemu.JobSpec{Labels: []string{read}, Environment: machines.ContributionEnvironment})
	e.pass()
	ms := e.wantMachines(2)
	var contribution gceemu.InstanceState
	for _, in := range ms {
		if in.Labels[machines.LabelContribution] == "true" {
			contribution = in
		}
	}
	// Only the contribution's machine boots: GitHub hands it the older,
	// ordinary job.
	m := e.boot(contribution)
	m.taken(ordinary)
	m.refused(other.ID)
	if adm, _ := e.record("machines/" + m.in.Name + "/admission.json"); !strings.Contains(adm["reason"].(string), "made for one") {
		t.Fatalf("admission = %v", adm)
	}
}

func TestAClaimedScopeOnAContributionMachineCancels(t *testing.T) {
	e := setup(t)
	crun, p := e.contribution(true)
	csel := "lanescape-2vcpu-linux-amd64-pr" + strconv.Itoa(p.Number)
	meant := e.gh.Queue(crun, ghemu.JobSpec{Labels: []string{csel}, Environment: machines.ContributionEnvironment})
	e.pass()
	// A push to main's job claims the pull request's scope, and takes the
	// contribution's machine.
	main := e.mainRun()
	claim := e.queue(main, csel)
	e.gh.CompleteJob(meant, "success")
	m := e.boot(e.one())
	m.taken(claim)
	m.refused(main.ID)
	adm, _ := e.record("machines/" + m.in.Name + "/admission.json")
	if adm["cancel"] != true || !strings.Contains(adm["reason"].(string), "scope") {
		t.Fatalf("admission = %v; a claimed scope cancels its run", adm)
	}
	if _, ok := e.record("refusals/5001/" + strconv.FormatInt(main.ID, 10) + "-1.json"); !ok {
		t.Fatal("the refusal wasn't recorded")
	}
}

func TestARefusedAttemptDoesntCancelARerun(t *testing.T) {
	e := setup(t)
	fork := e.forkPull()
	// Attempt 1 was refused, and finished; a maintainer reran it before
	// the refusal was followed up.
	rf := machines.Refusal{RepositoryID: repoID, RunID: fork.ID, RunAttempt: 1, Selectors: []string{sel2}, RefusedAt: e.clock.Now()}
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
