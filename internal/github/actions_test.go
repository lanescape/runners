package github_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/github"
)

func (e env) repository(t *testing.T) github.Repository {
	t.Helper()
	r, err := e.c.RepositoryByID(context.Background(), e.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRepositories(t *testing.T) {
	e := setup(t)
	e.emu.Repo(e.org, 5002, "lib")
	e.emu.Repo(e.emu.Org(1234, "lookalike"), 6001, "app")
	rs, err := e.c.Repositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []int64{5001, 5002}) {
		t.Fatalf("repositories = %v, want only the organization's", ids)
	}
}

func TestRunsAndJobs(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	repo := e.repository(t)
	run := e.emu.PushRun(e.repo, "main")
	j := e.emu.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{"lanescape-2vcpu-linux-amd64-main"}})
	done := e.emu.PushRun(e.repo, "main")
	e.emu.SetRunStatus(done, "completed")

	runs, err := e.c.Runs(ctx, repo, "in_progress")
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("in-progress runs = %+v, %v", runs, err)
	}
	jobs, err := e.c.RunJobs(ctx, repo, run.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	got := jobs[0]
	if got.ID != j.ID || got.RunID != run.ID || got.RunAttempt != 1 || got.Name != "build" || got.Status != "queued" ||
		!slices.Equal(got.Labels, j.Labels) || got.RunnerID != 0 || got.RunnerName != "" {
		t.Fatalf("job = %+v", got)
	}
	if one, err := e.c.Job(ctx, repo, j.ID); err != nil || one.ID != j.ID {
		t.Fatalf("job by ID = %+v, %v", one, err)
	}
	if _, err := e.c.Job(ctx, repo, 1); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
}

func TestApprovals(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	repo := e.repository(t)
	e.emu.ProtectEnvironment(e.repo, "contribution")
	run := e.emu.PushRun(e.repo, "main")
	j := e.emu.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{"x"}, Environment: "contribution"})
	if e.emu.Job(j.ID).Status != "waiting" {
		t.Fatal("a job in a protected environment should wait for approval")
	}
	if as, err := e.c.RunApprovals(ctx, repo, run.ID); err != nil || len(as) != 0 {
		t.Fatalf("approvals before review = %+v, %v", as, err)
	}
	maintainer := e.emu.User(42, "maintainer")
	e.emu.Approve(run, "contribution", maintainer)
	as, err := e.c.RunApprovals(ctx, repo, run.ID)
	if err != nil || len(as) != 1 || as[0].State != "approved" || !slices.Equal(as[0].Environments, []string{"contribution"}) {
		t.Fatalf("approvals = %+v, %v", as, err)
	}
	if e.emu.Job(j.ID).Status != "queued" {
		t.Fatal("an approved job should be queued")
	}
}

func TestForkApprovalPolicy(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	if p, err := e.c.ForkApprovalPolicy(ctx); err != nil || p != ghemu.ApproveFirstTimeContributors {
		t.Fatalf("default policy = %q, %v", p, err)
	}
	e.emu.SetForkApproval(e.org, ghemu.ApproveAllExternalContributors)
	if p, err := e.c.ForkApprovalPolicy(ctx); err != nil || p != ghemu.ApproveAllExternalContributors {
		t.Fatalf("policy = %q, %v", p, err)
	}
	// Renamed: found again by ID.
	e.emu.Rename(e.org, "lanescape-renamed")
	c := client(t, e.emu, e.app)
	if p, err := c.ForkApprovalPolicy(ctx); err != nil || p != ghemu.ApproveAllExternalContributors {
		t.Fatalf("after a rename: %q, %v", p, err)
	}
}

func TestCancel(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	repo := e.repository(t)

	t.Run("queued jobs end at once", func(t *testing.T) {
		run := e.emu.PushRun(e.repo, "main")
		e.emu.Queue(run, ghemu.JobSpec{Labels: []string{"x"}})
		if err := e.c.CancelRun(ctx, repo, run.ID); err != nil {
			t.Fatal(err)
		}
		if st, c := e.emu.RunState(run); st != "completed" || c != "cancelled" {
			t.Fatalf("run = %s %s", st, c)
		}
		if err := e.c.CancelRun(ctx, repo, run.ID); !errors.Is(err, github.ErrConflict) {
			t.Fatalf("cancelling a finished run: %v, want ErrConflict", err)
		}
	})
	t.Run("a running job ends once its runner acknowledges", func(t *testing.T) {
		run := e.emu.PushRun(e.repo, "main")
		j := e.emu.Queue(run, ghemu.JobSpec{Labels: []string{"x"}, Steps: "sleep 30"})
		standIn(t, e, "here", []string{"x"}, "", nil, func(ctx context.Context, s ghemu.StandIn) {
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			waitFor(t, func() bool { return e.emu.Job(j.ID).Status == "in_progress" })
			if err := e.c.CancelRun(context.Background(), repo, run.ID); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
		if st, c := e.emu.RunState(run); st != "completed" || c != "cancelled" {
			t.Fatalf("run = %s %s", st, c)
		}
	})
	t.Run("a running job whose runner is gone needs force", func(t *testing.T) {
		run := e.emu.PushRun(e.repo, "main")
		j := e.emu.Queue(run, ghemu.JobSpec{Labels: []string{"x"}, Steps: "sleep 30"})
		standIn(t, e, "gone", []string{"x"}, "", nil, func(ctx context.Context, s ghemu.StandIn) {
			// The stand-in takes the job and is killed mid-job.
			go func() { _ = s.Run(ctx) }()
			waitFor(t, func() bool { return e.emu.Job(j.ID).Status == "in_progress" })
		})
		if err := e.c.CancelRun(ctx, repo, run.ID); err != nil {
			t.Fatal(err)
		}
		if st, _ := e.emu.RunState(run); st == "completed" {
			t.Fatal("a cancellation finished a job whose runner is gone")
		}
		if err := e.c.ForceCancelRun(ctx, repo, run.ID); err != nil {
			t.Fatal(err)
		}
		if st, c := e.emu.RunState(run); st != "completed" || c != "cancelled" {
			t.Fatalf("after force-cancel: %s %s", st, c)
		}
	})
}

func TestRunnerGroups(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	gs, err := e.c.RunnerGroups(ctx)
	if err != nil || len(gs) != 1 || !gs[0].Default {
		t.Fatalf("groups = %+v, %v", gs, err)
	}
	g, err := e.c.CreateRunnerGroup(ctx, "lanescape-runners-5001", 5001)
	if err != nil || g.Visibility != "selected" || !g.AllowsPublicRepositories || g.RestrictedToWorkflows {
		t.Fatalf("created %+v, %v", g, err)
	}
	if _, err := e.c.CreateRunnerGroup(ctx, "lanescape-runners-5001", 5001); !errors.Is(err, github.ErrConflict) {
		t.Fatalf("a second group of the same name: %v, want ErrConflict", err)
	}
	if ids, err := e.c.RunnerGroupRepositories(ctx, g.ID); err != nil || !slices.Equal(ids, []int64{5001}) {
		t.Fatalf("group's repositories = %v, %v", ids, err)
	}
	e.emu.Repo(e.org, 5002, "lib")
	if err := e.c.SetRunnerGroupRepositories(ctx, g.ID, []int64{5002}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := e.c.RunnerGroupRepositories(ctx, g.ID); !slices.Equal(ids, []int64{5002}) {
		t.Fatalf("after setting: %v", ids)
	}
}

func TestJITRunners(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	g, err := e.c.CreateRunnerGroup(ctx, "lanescape-runners-5001", 5001)
	if err != nil {
		t.Fatal(err)
	}
	labels := []string{"lanescape-2vcpu-linux-amd64-main", "self-hosted", "linux", "X64"}
	r, cfg, err := e.c.JITConfig(ctx, g.ID, "lsr-5001-abcdefgh", labels)
	if err != nil || r.ID == 0 || r.Name != "lsr-5001-abcdefgh" || cfg == "" || r.Status != "offline" {
		t.Fatalf("runner = %+v, %v", r, err)
	}
	if _, _, err := e.c.JITConfig(ctx, g.ID, "lsr-5001-abcdefgh", labels); !errors.Is(err, github.ErrConflict) {
		t.Fatalf("a second runner of the same name: %v, want ErrConflict", err)
	}
	rs, err := e.c.GroupRunners(ctx, g.ID)
	if err != nil || len(rs) != 1 || rs[0].ID != r.ID || !slices.Equal(rs[0].Labels, labels) {
		t.Fatalf("group's runners = %+v, %v", rs, err)
	}
	if err := e.c.DeleteRunner(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.c.DeleteRunner(ctx, r.ID); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("deleting it again: %v, want ErrNotFound", err)
	}
}

// standIn registers a just-in-time runner in a group for the repository and
// hands a stand-in for it to use, whose output goes to the test's log.
func standIn(t *testing.T, e env, name string, labels []string, hook string, env []string, use func(context.Context, ghemu.StandIn)) {
	t.Helper()
	ctx := context.Background()
	gs, _ := e.c.RunnerGroups(ctx)
	var gid int64
	for _, g := range gs {
		if g.Name == "lanescape-runners-5001" {
			gid = g.ID
		}
	}
	if gid == 0 {
		g, err := e.c.CreateRunnerGroup(ctx, "lanescape-runners-5001", 5001)
		if err != nil {
			t.Fatal(err)
		}
		gid = g.ID
	}
	_, cfg, err := e.c.JITConfig(ctx, gid, name, labels)
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	use(rctx, ghemu.StandIn{JITConfig: cfg, Hook: hook, Env: env, Dir: t.TempDir(), Poll: 20 * time.Millisecond})
	cancel()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out")
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestStandInTakesAnyJobItsLabelsMatch(t *testing.T) {
	e := setup(t)
	repo := e.repository(t)
	sel := "lanescape-2vcpu-linux-amd64-main"
	run := e.emu.PushRun(e.repo, "main")
	// A job that asked only for self-hosted is queued first, so it's handed
	// out first, to a runner made for the other.
	stray := e.emu.Queue(run, ghemu.JobSpec{Name: "stray", Labels: []string{"Self-Hosted"},
		Env: map[string]string{"GITHUB_RUN_ID": "1", "FROM_JOB": "yes"}, Steps: `echo "steps ran" > "$OUT/steps"`})
	meant := e.emu.Queue(run, ghemu.JobSpec{Name: "meant", Labels: []string{sel}})

	dir := t.TempDir()
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte(`echo "$GITHUB_JOB $GITHUB_RUN_ID $FROM_JOB $RUNNER_NAME" > "$OUT/hook"`), 0o755); err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	standIn(t, e, "lsr-5001-aaaaaaaa", []string{sel, "self-hosted", "linux", "x64"}, hook, append(os.Environ(), "OUT="+dir), func(ctx context.Context, s ghemu.StandIn) {
		s.Stdout = out
		if err := s.Run(ctx); err != nil {
			t.Fatal(err)
		}
	})
	got, _ := e.c.Job(context.Background(), repo, stray.ID)
	if got.Status != "completed" || got.Conclusion != "success" || got.RunnerName != "lsr-5001-aaaaaaaa" || got.RunnerID == 0 {
		t.Fatalf("stray job = %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "hook")); strings.TrimSpace(string(b)) != "stray "+strconv.FormatInt(run.ID, 10)+" yes lsr-5001-aaaaaaaa" {
		t.Fatalf("hook saw %q; the job's environment sets what it can, never GitHub's own", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "steps")); err != nil {
		t.Fatal("steps didn't run after the hook")
	}
	if e.emu.Job(meant.ID).Status != "queued" {
		t.Fatal("a one-job runner took a second job")
	}
	if len(e.emu.Runners()) != 0 {
		t.Fatal("an ephemeral runner wasn't removed after its job")
	}
	if !strings.Contains(out.String(), "completed with result: success") {
		t.Fatalf("stand-in said %q", out.String())
	}
}

// A hook that fails fails the job: its steps don't run, but those whose
// condition is always() or failure() do, as on the real runner.
func TestStandInHookFailureFailsTheJob(t *testing.T) {
	e := setup(t)
	run := e.emu.PushRun(e.repo, "main")
	dir := t.TempDir()
	j := e.emu.Queue(run, ghemu.JobSpec{Labels: []string{"x"}, Steps: `touch "$OUT/steps"`, AlwaysSteps: `touch "$OUT/always"`})
	hook := filepath.Join(dir, "hook.sh")
	_ = os.WriteFile(hook, []byte("exit 3"), 0o755)
	standIn(t, e, "lsr-5001-bbbbbbbb", []string{"x"}, hook, append(os.Environ(), "OUT="+dir), func(ctx context.Context, s ghemu.StandIn) {
		if err := s.Run(ctx); err != nil {
			t.Fatal(err)
		}
	})
	if got := e.emu.Job(j.ID); got.Conclusion != "failure" {
		t.Fatalf("job = %s %s, want a failure", got.Status, got.Conclusion)
	}
	if _, err := os.Stat(filepath.Join(dir, "steps")); err == nil {
		t.Fatal("a job's steps ran after its hook failed")
	}
	if _, err := os.Stat(filepath.Join(dir, "always")); err != nil {
		t.Fatal("a step whose condition is always() didn't run after the hook failed")
	}
}

func TestAssignmentHonoursRunnerGroups(t *testing.T) {
	e := setup(t)
	other := e.emu.Repo(e.org, 5002, "other")
	run := e.emu.PushRun(other, "main")
	j := e.emu.Queue(run, ghemu.JobSpec{Labels: []string{"x"}})
	standIn(t, e, "lsr-5001-cccccccc", []string{"x"}, "", nil, func(ctx context.Context, s ghemu.StandIn) {
		go func() { _ = s.Run(ctx) }()
		time.Sleep(300 * time.Millisecond)
	})
	if e.emu.Job(j.ID).Status != "queued" {
		t.Fatal("another repository's job was handed to a runner in a group it may not use")
	}
}

func TestAssignmentNeedsPublicRepositoriesAllowed(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	// The Default group doesn't allow public repositories.
	gs, _ := e.c.RunnerGroups(ctx)
	_, cfg, err := e.c.JITConfig(ctx, gs[0].ID, "public-test", []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	run := e.emu.PushRun(e.repo, "main")
	j := e.emu.Queue(run, ghemu.JobSpec{Labels: []string{"x"}})
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = ghemu.StandIn{JITConfig: cfg, Dir: t.TempDir()}.Run(rctx) }()
	time.Sleep(300 * time.Millisecond)
	if e.emu.Job(j.ID).Status != "queued" {
		t.Fatal("a public repository's job went to a group that doesn't allow public repositories")
	}
	e.repo.Private = true
	e.emu.SetRunStatus(run, "in_progress") // wakes the assignment
	waitFor(t, func() bool { return e.emu.Job(j.ID).Status != "queued" })
}

func TestRunnerStatus(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	run := e.emu.PushRun(e.repo, "main")
	j := e.emu.Queue(run, ghemu.JobSpec{Labels: []string{"x"}, Steps: "sleep 30"})
	standIn(t, e, "busy", []string{"x"}, "", nil, func(rctx context.Context, s ghemu.StandIn) {
		go func() { _ = s.Run(rctx) }()
		waitFor(t, func() bool { return e.emu.Job(j.ID).Status == "in_progress" })
		gs, _ := e.c.RunnerGroups(ctx)
		var r github.Runner
		for _, g := range gs {
			rs, _ := e.c.GroupRunners(ctx, g.ID)
			for _, x := range rs {
				if x.Name == "busy" {
					r = x
				}
			}
		}
		if r.Status != "online" || !r.Busy {
			t.Fatalf("runner = %+v, want online and busy", r)
		}
		if err := e.c.DeleteRunner(ctx, r.ID); !errors.Is(err, github.ErrRefused) {
			t.Fatalf("deleting a busy runner: %v, want ErrRefused", err)
		}
	})
}

func TestRerunFailedJobs(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	repo := e.repository(t)
	run := e.emu.PushRun(e.repo, "main")
	lost := e.emu.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{"lanescape-2vcpu-linux-amd64-spot-main"}})
	ok := e.emu.Queue(run, ghemu.JobSpec{Name: "lint"})
	if err := e.c.RerunFailedJobs(ctx, repo, run.ID); err == nil {
		t.Fatal("a run in progress was rerun")
	}
	e.emu.CompleteJob(lost, "failure")
	e.emu.CompleteJob(ok, "success")
	if err := e.c.RerunFailedJobs(ctx, repo, run.ID); err != nil {
		t.Fatal(err)
	}
	r, err := e.c.WorkflowRun(ctx, repo, run.ID)
	if err != nil || r.Attempt != 2 || r.Status != "in_progress" {
		t.Fatalf("run = %+v, %v", r, err)
	}
	second, err := e.c.AttemptJobs(ctx, repo, run.ID, 2)
	if err != nil || len(second) != 2 {
		t.Fatalf("second attempt's jobs = %+v, %v", second, err)
	}
	for _, j := range second {
		switch j.Name {
		case "build":
			if j.ID == lost.ID || j.Status != "queued" || j.RunAttempt != 2 {
				t.Fatalf("rerun job = %+v", j)
			}
		case "lint":
			if j.Status != "completed" || j.Conclusion != "success" {
				t.Fatalf("carried job = %+v", j)
			}
		}
	}
	first, err := e.c.AttemptJobs(ctx, repo, run.ID, 1)
	if err != nil || len(first) != 2 || !slices.ContainsFunc(first, func(j github.Job) bool { return j.ID == lost.ID && j.Conclusion == "failure" }) {
		t.Fatalf("first attempt's jobs = %+v, %v", first, err)
	}
	if latest, _ := e.c.RunJobs(ctx, repo, run.ID); len(latest) != 2 || latest[0].RunAttempt != 2 {
		t.Fatalf("latest jobs = %+v", latest)
	}
}

func TestWhoTriggeredAnAttempt(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	repo := e.repository(t)
	run := e.emu.PushRun(e.repo, "main")
	j := e.emu.Queue(run, ghemu.JobSpec{Name: "build"})
	e.emu.CompleteJob(j, "failure")
	if err := e.c.RerunFailedJobs(ctx, repo, run.ID); err != nil {
		t.Fatal(err)
	}
	bot, err := e.c.Bot(ctx)
	if err != nil || bot != "lanescape-runners[bot]" {
		t.Fatalf("bot = %q, %v", bot, err)
	}
	if by, err := e.c.AttemptTrigger(ctx, repo, run.ID, 2); err != nil || by != bot {
		t.Fatalf("attempt 2 triggered by %q, %v; want the App's bot", by, err)
	}
	e.emu.CompleteJobByID(e.emu.RunJobs(run, 2)[0].ID, "success")
	e.emu.Rerun(run)
	if by, err := e.c.AttemptTrigger(ctx, repo, run.ID, 3); err != nil || by == bot {
		t.Fatalf("attempt 3 triggered by %q, %v; want the maintainer", by, err)
	}
	if _, err := e.c.AttemptTrigger(ctx, repo, run.ID, 4); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("an attempt that isn't: %v", err)
	}
}
