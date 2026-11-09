package disks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	"github.com/lanescape/runners/internal/disks"
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
	key1     = "0123456789abcdef0123"
	key2     = "fedcba9876543210fedc"
	zero     = "00000000000000000000"
)

// sel is a selector for the 2-vCPU amd64 shape, with a saved disk of family
// build and key, in scope.
func sel(key, scope string) string {
	return "lanescape-2vcpu-linux-amd64-disk-build-" + key + "-" + scope
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

type env struct {
	t       *testing.T
	clock   *clockwork.FakeClock
	gh      *ghemu.Emulator
	gce     *gceemu.Emulator
	gcs     *gcsemu.Emulator
	org     *ghemu.Account
	repo    *ghemu.Repo
	cfg     *config.Config
	svc     *machines.Service
	disks   *disks.Service
	objects store.Objects
	binder  *binding.Binder
	actions *auth.Actions
	runner  *pass.Runner
	url     string
	logs    *lockedBuffer

	mu        sync.Mutex
	stops     []context.CancelFunc
	byMachine map[string]context.CancelFunc
	used      map[string]bool
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	// The service's and Compute Engine's clock; GitHub keeps real time,
	// as its installation tokens' lifetimes are read in real time.
	clock := clockwork.NewFakeClockAt(time.Now())
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	gce := gceemu.New(gceemu.WithClock(clock))
	t.Cleanup(gce.Close)
	gce.AddImage("lsr-base-amd64-1", "", "X86_64")
	gce.AddImage("lsr-base-arm64-1", "", "ARM64")
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket("state")

	org := gh.Org(orgID, "lanescape")
	gh.SetForkApproval(org, ghemu.ApproveAllExternalContributors)
	app := gh.NewApp(77)
	gh.Install(app, org)
	repo := gh.Repo(org, repoID, "app")
	repo.Commit("main", map[string]string{"README": "app"})

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
	actions, err := auth.NewActions(auth.Options{Issuer: auth.Issuer{URL: gh.Actions.URL, JWKSURL: gh.Actions.JWKSURL}, Audience: audience}, orgID)
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
	dks := &disks.Service{Config: cfg, Project: gce.Project, GitHub: ghc, Compute: gcec, Objects: objects, Clock: clock, Logger: log,
		StepDeadline: time.Second}
	svc := &machines.Service{Config: cfg, Project: gce.Project, ServiceURL: audience, MachineAccount: account,
		GitHub: ghc, Compute: gcec, Objects: objects, Binder: binder, Machines: verifier, Clock: clock, Logger: log,
		AssignmentWait: 3 * time.Second, AssignmentRetry: 50 * time.Millisecond, Disks: dks, Saves: dks, Preemptions: dks}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/jobs/started", svc.JobStarted())
	mux.Handle("POST /v1/jobs/finished", svc.JobFinished())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e := &env{t: t, clock: clock, gh: gh, gce: gce, gcs: gcs, org: org, repo: repo, cfg: cfg, svc: svc, disks: dks, objects: objects,
		binder: binder, actions: actions, url: srv.URL, logs: logs, byMachine: map[string]context.CancelFunc{}, used: map[string]bool{},
		runner: &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock, Logger: log}}
	gce.OnStop(func(m gceemu.Machine) {
		e.mu.Lock()
		stop := e.byMachine[m.Name]
		e.mu.Unlock()
		if stop != nil {
			stop()
		}
	})
	t.Cleanup(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, s := range e.stops {
			s()
		}
	})
	return e
}

// pass runs one pass, the machines' part and then the disks'.
func (e *env) pass() {
	e.t.Helper()
	if err := e.runner.Run(context.Background(), pass.All(e.svc.Pass, e.disks.Pass)); err != nil {
		e.t.Fatalf("pass: %v", err)
	}
}

func (e *env) bind(run *ghemu.Run) string {
	e.t.Helper()
	c, err := e.actions.Verify(context.Background(), e.gh.ActionsToken(run, ghemu.Job{Audience: audience}, nil))
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := e.binder.Bind(context.Background(), c, binding.Request{})
	if err != nil {
		e.t.Fatal(err)
	}
	return b.Scope.String()
}

// mainRun is a push to main, bound main.
func (e *env) mainRun() *ghemu.Run {
	e.t.Helper()
	run := e.gh.PushRun(e.repo, "main")
	if s := e.bind(run); s != "main" {
		e.t.Fatalf("a push to main bound %s", s)
	}
	return run
}

// pull is a same-repository pull request, with a run bound to its scope.
func (e *env) pull() (*ghemu.Pull, *ghemu.Run) {
	e.t.Helper()
	branch := "feature-" + strconv.Itoa(len(e.used))
	e.repo.Commit(branch, map[string]string{"x": branch})
	p := e.gh.PullRequest(e.repo, "main", e.repo, branch, e.gh.User(42, "maintainer"))
	return p, e.pullRun(p)
}

func (e *env) pullRun(p *ghemu.Pull) *ghemu.Run {
	e.t.Helper()
	run := e.gh.PullRequestRun(p)
	if s := e.bind(run); s != "pr"+strconv.Itoa(p.Number) {
		e.t.Fatalf("a pull request's run bound %s", s)
	}
	return run
}

// job is a job as a test plays it: queued, given a machine, which takes it,
// and admitted.
type job struct {
	e    *env
	run  *ghemu.Run
	j    *ghemu.WorkflowJob
	in   gceemu.InstanceState
	gate string
	done chan error
}

// start queues a job and plays it to its admission, which it expects to
// succeed.
func (e *env) start(run *ghemu.Run, spec ghemu.JobSpec) *job {
	e.t.Helper()
	j := e.gh.Queue(run, spec)
	return e.admit(run, j)
}

// admit gives a queued job a machine and plays it to its admission.
func (e *env) admit(run *ghemu.Run, j *ghemu.WorkflowJob) *job {
	e.t.Helper()
	jb := e.take(run, j)
	if code := jb.started(); code != http.StatusOK {
		e.t.Fatalf("job-started check: %d", code)
	}
	return jb
}

// take makes a machine for a queued job and lets it take the job, short of
// its admission.
func (e *env) take(run *ghemu.Run, j *ghemu.WorkflowJob) *job {
	e.t.Helper()
	e.pass()
	var in gceemu.InstanceState
	for _, m := range e.gce.Instances() {
		if m.Labels[machines.LabelJob] == strconv.FormatInt(j.ID, 10) && !e.used[m.Name] {
			in = m
		}
	}
	if in.Name == "" {
		e.t.Fatalf("no machine for job %d", j.ID)
	}
	e.used[in.Name] = true
	dir := e.t.TempDir()
	gate := filepath.Join(dir, "gate")
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte(`while [ ! -e "`+gate+`" ]; do sleep 0.02; done; [ "$(cat "`+gate+`")" = ok ]`), 0o755); err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.stops = append(e.stops, cancel)
	e.byMachine[in.Name] = cancel
	e.mu.Unlock()
	jb := &job{e: e, run: run, j: j, in: in, gate: gate, done: make(chan error, 1)}
	s := ghemu.StandIn{JITConfig: in.Metadata[machines.MetaJITConfig], Hook: hook, Dir: dir, Poll: 100 * time.Millisecond}
	go func() { jb.done <- s.Run(ctx) }()
	e.eventually("the job is on "+in.Name, func() bool {
		got := e.gh.Job(j.ID)
		return got.Status == "in_progress" && got.Runner != nil && got.Runner.Name == in.Name
	})
	return jb
}

func (jb *job) started() int {
	jb.e.t.Helper()
	tok := jb.e.gce.Identity.Mint(jb.e.gce.IdentityClaims(jb.in.Zone, jb.in.Name, audience, true))
	b, _ := json.Marshal(machines.StartedRequest{RunID: jb.run.ID})
	req, _ := http.NewRequest(http.MethodPost, jb.e.url+"/v1/jobs/started", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		jb.e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// finish lets the job's steps run, waits for its runner to exit, and sends
// the supervisor's report, then powers the machine off, as the supervisor
// does, and runs a pass.
func (jb *job) finish(save bool) {
	jb.e.t.Helper()
	_ = os.WriteFile(jb.gate, []byte("ok"), 0o644)
	select {
	case err := <-jb.done:
		if err != nil {
			jb.e.t.Fatalf("runner: %v", err)
		}
	case <-time.After(10 * time.Second):
		jb.e.t.Fatal("the runner never finished")
	}
	tok := jb.e.gce.Identity.Mint(jb.e.gce.IdentityClaims(jb.in.Zone, jb.in.Name, audience, true))
	b, _ := json.Marshal(machines.FinishedRequest{Save: save})
	req, _ := http.NewRequest(http.MethodPost, jb.e.url+"/v1/jobs/finished", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		jb.e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		jb.e.t.Fatalf("report: %d", resp.StatusCode)
	}
	jb.e.gce.Shutdown(jb.in.Zone, jb.in.Name)
	jb.e.pass()
}

// source is the image the job's work disk was made from, or "".
func (jb *job) source() string {
	jb.e.t.Helper()
	for _, d := range jb.e.gce.Disks() {
		if d.Name == machines.WorkDisk(jb.in.Name) {
			return d.SourceImage[strings.LastIndex(d.SourceImage, "/")+1:]
		}
	}
	jb.e.t.Fatalf("%s has no work disk", jb.in.Name)
	return ""
}

func (jb *job) size() int64 {
	for _, d := range jb.e.gce.Disks() {
		if d.Name == machines.WorkDisk(jb.in.Name) {
			return d.SizeGB
		}
	}
	return 0
}

func (e *env) eventually(what string, cond func() bool) {
	e.t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	e.t.Fatalf("timed out waiting until %s", what)
}

// images are the service's saved disks and resume images, by name.
func (e *env) images() map[string]gceemu.ImageState {
	out := map[string]gceemu.ImageState{}
	for _, im := range e.gce.Images() {
		if !strings.HasPrefix(im.Name, "lsr-base-") {
			out[im.Name] = im
		}
	}
	return out
}

// saved is the one image made since before, which the test expects.
func (e *env) saved(before map[string]gceemu.ImageState) gceemu.ImageState {
	e.t.Helper()
	var made []gceemu.ImageState
	for n, im := range e.images() {
		if _, ok := before[n]; !ok {
			made = append(made, im)
		}
	}
	if len(made) != 1 {
		e.t.Fatalf("%d images made, want 1: %v", len(made), made)
	}
	return made[0]
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
