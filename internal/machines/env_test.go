package machines_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	gceemu "github.com/lanescape/runners/emulators/compute"
	ghemu "github.com/lanescape/runners/emulators/github"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/store"
)

const (
	orgID    = 303994381
	repoID   = 5001
	audience = "https://runners.example.test"
	// sel2 is the smallest shape's selector.
	sel2  = "lanescape-2vcpu-linux-amd64"
	zoneA = "northamerica-northeast1-a"
	zoneB = "northamerica-northeast1-b"
)

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

type env struct {
	t           *testing.T
	clock       *clockwork.FakeClock
	gh          *ghemu.Emulator
	gce         *gceemu.Emulator
	gcs         *gcsemu.Emulator
	org         *ghemu.Account
	contributor *ghemu.Account
	maintainer  *ghemu.Account
	repo        *ghemu.Repo
	fork        *ghemu.Repo
	cfg         *config.Config
	svc         *machines.Service
	binder      *binding.Binder
	ghc         *github.Client
	runner      *pass.Runner
	url         string
	logs        *lockedBuffer
	stopMu      sync.Mutex
	stops       []context.CancelFunc
	byMachine   map[string]context.CancelFunc
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	clock := clockwork.NewFakeClockAt(time.Now())
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	gce := gceemu.New(gceemu.WithClock(clock))
	t.Cleanup(gce.Close)
	gce.AddImage("lsr-base-amd64-old", "", "X86_64")
	clock.Advance(time.Second)
	gce.AddImage("lsr-base-amd64-new", "", "X86_64")
	gce.AddImage("lsr-base-arm64-1", "", "ARM64")
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket("state")

	org := gh.Org(orgID, "lanescape")
	gh.SetForkApproval(org, ghemu.ApproveAllExternalContributors)
	app := gh.NewApp(77)
	gh.Install(app, org)
	repo := gh.Repo(org, repoID, "app")
	gh.ProtectEnvironment(repo, machines.ContributionEnvironment)
	contributor := gh.User(9001, "contributor")
	fork := gh.Fork(repo, contributor, 7001)

	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	ghc, err := github.NewAppClient(github.Options{BaseURL: gh.URL, AppID: app.ID, PrivateKey: app.PrivateKeyPEM(), OrganizationID: orgID})
	if err != nil {
		t.Fatal(err)
	}
	gcec, err := compute.New(ctx, gce.Project, gce.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.NewGCS(ctx, "state", gcs.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objects.Close() })
	account := machines.MachineAccountOf(gce.Project)
	verifier, err := auth.NewMachines(auth.Options{Issuer: auth.Issuer{URL: gce.Identity.URL, JWKSURL: gce.Identity.JWKSURL},
		Audience: audience, Now: clock.Now}, gce.Project, account)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	binder := &binding.Binder{GitHub: ghc, Objects: objects, OrganizationID: orgID, Clock: clock, Logger: log}
	svc := &machines.Service{Config: cfg, Project: gce.Project, ServiceURL: audience, MachineAccount: account,
		GitHub: ghc, Compute: gcec, Objects: objects, Binder: binder, Machines: verifier, Clock: clock, Logger: log,
		AssignmentWait: 3 * time.Second, AssignmentRetry: 50 * time.Millisecond, FailStopDelay: 100 * time.Millisecond}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/jobs/started", svc.JobStarted())
	mux.Handle("POST /v1/jobs/finished", svc.JobFinished())
	mux.Handle("POST "+machines.DisksPath, svc.DiskRequest())
	mux.Handle("POST "+machines.SavePath, svc.SaveRequest())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e := &env{t: t, clock: clock, gh: gh, gce: gce, gcs: gcs, org: org, contributor: contributor, maintainer: gh.User(42, "maintainer"),
		repo: repo, fork: fork, cfg: cfg, svc: svc, binder: binder, ghc: ghc, url: srv.URL, logs: logs,
		runner: &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock, Logger: log}}
	e.byMachine = map[string]context.CancelFunc{}
	// A machine's runner dies with it.
	gce.OnStop(func(m gceemu.Machine) {
		e.stopMu.Lock()
		stop := e.byMachine[m.Name]
		e.stopMu.Unlock()
		if stop != nil {
			stop()
		}
	})
	t.Cleanup(e.stopAll)
	return e
}

func (e *env) stopAll() {
	e.stopMu.Lock()
	defer e.stopMu.Unlock()
	for _, s := range e.stops {
		s()
	}
}

// pass runs one pass.
func (e *env) pass() {
	e.t.Helper()
	if err := e.runner.Run(context.Background(), e.svc.Pass); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

// facts are what GitHub tells a run's jobs' runners, as the hook reports
// them.
func facts(run *ghemu.Run) binding.Facts {
	f := binding.Facts{Event: run.Event, Ref: run.Ref, RefType: run.RefType, SHA: run.SHA}
	if p := run.EventPull; p != nil {
		f.PullRequest, f.HeadRepositoryID, f.HeadSHA = p.Number, p.HeadRepositoryID, p.HeadSHA
	}
	return f
}

// bind binds a run as a machine's first check would.
func (e *env) bind(run *ghemu.Run) binding.Binding {
	e.t.Helper()
	ctx := context.Background()
	repo, err := e.ghc.RepositoryByID(ctx, run.Repo.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	wr, err := e.ghc.WorkflowRun(ctx, repo, run.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := e.binder.Bind(ctx, repo, wr, facts(run))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// mainRun is a push to main, bound main.
func (e *env) mainRun() *ghemu.Run {
	run := e.gh.PushRun(e.repo, "main")
	e.bind(run)
	return run
}

// contribution is a pull_request_target run of a fork's pull request,
// approved or not, and unbound: its contribution job's check binds it.
func (e *env) contribution(approve bool) (*ghemu.Run, *ghemu.Pull) {
	e.fork.Commit("feature", map[string]string{"src.go": "package app"})
	p := e.gh.PullRequest(e.repo, "main", e.fork, "feature", e.contributor)
	run := e.gh.PullRequestTargetRun(p)
	if approve {
		e.gh.Approve(run, machines.ContributionEnvironment, e.maintainer)
	}
	return run, p
}

// forkPull is a fork's pull_request run.
func (e *env) forkPull() *ghemu.Run {
	e.fork.Commit("evil", map[string]string{"x": "1"})
	p := e.gh.PullRequest(e.repo, "main", e.fork, "evil", e.contributor)
	return e.gh.PullRequestRun(p)
}

// queue queues a job of a run, naming labels.
func (e *env) queue(run *ghemu.Run, labels ...string) *ghemu.WorkflowJob {
	return e.gh.Queue(run, ghemu.JobSpec{Labels: labels})
}

// ours lists the service's machines.
func (e *env) ours() []gceemu.InstanceState {
	var out []gceemu.InstanceState
	for _, in := range e.gce.Instances() {
		if strings.HasPrefix(in.Name, "lsr-") && !strings.HasPrefix(in.Name, "lsr-build-") {
			out = append(out, in)
		}
	}
	return out
}

func (e *env) one() gceemu.InstanceState {
	e.t.Helper()
	ms := e.ours()
	if len(ms) != 1 {
		e.t.Fatalf("%d machines, want 1", len(ms))
	}
	return ms[0]
}

func (e *env) wantMachines(n int) []gceemu.InstanceState {
	e.t.Helper()
	ms := e.ours()
	if len(ms) != n {
		e.t.Fatalf("%d machines, want %d", len(ms), n)
	}
	return ms
}

// machine is a machine as a test plays its part: its runner stand-in, whose
// hook waits until the test has asked the service and lets it go on.
type machine struct {
	e    *env
	in   gceemu.InstanceState
	gate string
	done chan error
	stop context.CancelFunc
}

// boot starts a machine's runner, which waits for GitHub to hand it a job.
func (e *env) boot(in gceemu.InstanceState) *machine {
	e.t.Helper()
	dir := e.t.TempDir()
	gate := filepath.Join(dir, "gate")
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte(`while [ ! -e "`+gate+`" ]; do sleep 0.02; done; [ "$(cat "`+gate+`")" = ok ]`), 0o755); err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.stopMu.Lock()
	e.stops = append(e.stops, cancel)
	e.byMachine[in.Name] = cancel
	e.stopMu.Unlock()
	m := &machine{e: e, in: in, gate: gate, done: make(chan error, 1), stop: cancel}
	s := ghemu.StandIn{JITConfig: in.Metadata[machines.MetaJITConfig], Hook: hook, Dir: dir, Poll: 300 * time.Millisecond}
	go func() { m.done <- s.Run(ctx) }()
	return m
}

// taken waits until GitHub shows the job on the machine's runner.
func (m *machine) taken(j *ghemu.WorkflowJob) {
	m.e.t.Helper()
	m.e.eventually("the job is on "+m.in.Name, func() bool {
		got := m.e.gh.Job(j.ID)
		return got.Status == "in_progress" && got.Runner != nil && got.Runner.Name == m.in.Name
	})
}

// release lets the hook return, as it does once the service allows the
// job, or fail.
func (m *machine) release(ok bool) {
	v := "ok"
	if !ok {
		v = "no"
	}
	_ = os.WriteFile(m.gate, []byte(v), 0o644)
}

// finished waits for the runner to report its job and exit.
func (m *machine) finished() {
	m.e.t.Helper()
	select {
	case err := <-m.done:
		if err != nil {
			m.e.t.Fatalf("runner: %v", err)
		}
	case <-time.After(10 * time.Second):
		m.e.t.Fatal("the runner never finished")
	}
}

func (e *env) post(path, token string, body any) (int, map[string]any) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.url+path, bytes.NewReader(b))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// token is a machine's identity token for the service, in the full format.
func (e *env) token(in gceemu.InstanceState) string {
	return e.gce.Identity.Mint(e.gce.IdentityClaims(in.Zone, in.Name, audience, true))
}

// started asks the job-started check, as the hook does: with what GitHub
// told the runner about the job it took, and runID as where to look first.
func (m *machine) started(runID int64) int {
	m.e.t.Helper()
	code, _ := m.startedBody(runID)
	return code
}

func (m *machine) startedBody(runID int64) (int, map[string]any) {
	m.e.t.Helper()
	req := machines.StartedRequest{RunID: runID}
	if _, run, ok := m.e.gh.JobOnRunner(m.in.Name); ok {
		req.Facts = facts(run)
	}
	return m.e.post("/v1/jobs/started", m.e.token(m.in), req)
}

// report sends the supervisor's report.
func (m *machine) report() int {
	m.e.t.Helper()
	code, _ := m.e.post("/v1/jobs/finished", m.e.token(m.in), machines.FinishedRequest{})
	return code
}

// disk asks once for the job's work disk of a family, as the disk action
// does.
func (m *machine) disk(family string) (int, map[string]any) {
	m.e.t.Helper()
	return m.e.post(machines.DisksPath, m.e.token(m.in), map[string]string{"family": family})
}

// diskReady asks for the job's work disk of a family until it's ready, as
// the disk action does, and returns where it came from.
func (m *machine) diskReady(family string) string {
	m.e.t.Helper()
	var restored string
	m.e.eventually("the work disk is ready", func() bool {
		code, body := m.disk(family)
		if code != http.StatusOK && code != http.StatusServiceUnavailable {
			m.e.t.Fatalf("work disk request: %d %v", code, body)
		}
		restored, _ = body["restored"].(string)
		return body["state"] == machines.DiskStateReady
	})
	return restored
}

// save asks for the job's work disk to be saved, as the save-disk action
// does.
func (m *machine) save() (int, map[string]any) {
	m.e.t.Helper()
	return m.e.post(machines.SavePath, m.e.token(m.in), map[string]bool{"save": true})
}

// declares is a .github/runners.json that declares the family build, with
// no files.
const declares = `{"disks": {"build": {}}}`

func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	e.t.Fatalf("timed out waiting until %s", what)
}

func (e *env) record(name string) (map[string]any, bool) {
	data, _, ok := e.gcs.Object("state", name)
	if !ok {
		return nil, false
	}
	var v map[string]any
	_ = json.Unmarshal(data, &v)
	return v, true
}

func (e *env) logged(s string) bool { return strings.Contains(e.logs.String(), s) }
