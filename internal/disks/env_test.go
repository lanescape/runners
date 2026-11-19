package disks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/lanescape/runners/internal/consumer"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/selector"
	"github.com/lanescape/runners/internal/store"
)

const (
	orgID    = 303994381
	repoID   = 5001
	audience = "https://runners.example.test"
	key1     = "0123456789abcdef0123"
	key2     = "fedcba9876543210fedc"
	key3     = "00112233445566778899"
	zero     = "00000000000000000000"
	// cachesBucket is the caches' bucket, in the storage emulator beside
	// the state bucket.
	cachesBucket = "caches"
)

// declares is a .github/runners.json that declares every cache the tests'
// jobs restore and save, which keeps their versions in main's scope and, on
// a long-lived branch, in its scope.
const declares = `{"caches": {"build": {}, "tools": {}, "other": {}}}`

// sel is the 2-vCPU amd64 shape whose job restores and saves the cache
// build, whose key for the run is key, in the run's scope: a test's
// shorthand, which start and take make into the job's selector, the shape
// alone, recording the key as the run's, and the cache as what the job's
// actions name.
func sel(key, scope string) string {
	return "lanescape-2vcpu-linux-amd64-cache-build-" + key + "-" + scope
}

// testLabel is a test's shorthand for a job: its shape, then, for a job that
// asks for a work disk and restores and saves a cache, -cache-<cache>-<the
// cache's key for the run>, then its run's scope.
var testLabel = regexp.MustCompile(`^(lanescape-[0-9]+vcpu-linux-[a-z0-9]+(?:-spot)?)(?:-cache-([a-z][a-z0-9-]*?)-([0-9a-f]{20}))?-(main|read|pr[0-9]+|branch-[a-z0-9]+)$`)

// newLabel is a test's label as the job's selector: its shape alone.
func newLabel(label string) string {
	if m := testLabel.FindStringSubmatch(label); m != nil {
		return m[1]
	}
	return label
}

// cacheOfLabels is the cache a test's label has its job restore and save,
// or "".
func cacheOfLabels(labels []string) string {
	for _, l := range labels {
		if m := testLabel.FindStringSubmatch(l); m != nil && m[2] != "" {
			return m[2]
		}
	}
	return ""
}

// keys records the keys a test's labels give their caches as the run's, as
// the service would have made them from the run's commit.
func (e *env) keys(run *ghemu.Run, labels []string) {
	e.t.Helper()
	name := "runs/5001/" + strconv.FormatInt(run.ID, 10) + "/caches.json"
	var rf machines.RunFiles
	if data, _, ok := e.gcs.Object("state", name); ok {
		_ = json.Unmarshal(data, &rf)
	} else {
		rf = machines.RunFiles{RepositoryID: repoID, RunID: run.ID, Commit: run.HeadSHA, Caches: map[string]machines.CacheKey{}}
	}
	changed := false
	for _, l := range labels {
		m := testLabel.FindStringSubmatch(l)
		if m == nil || m[2] == "" {
			continue
		}
		if had, ok := rf.Caches[m[2]]; ok && had.Key != m[3] {
			e.t.Fatalf("run %d's cache %s has the key %s already, not %s: one key per cache and run", run.ID, m[2], had.Key, m[3])
		}
		rf.Caches[m[2]] = machines.CacheKey{Key: m[3]}
		changed = true
	}
	if changed {
		b, _ := json.Marshal(rf)
		e.gcs.Put("state", name, b)
	}
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

// tokens are the tests' disks.Tokens: each names what it may do and the
// prefix it's for, as a real one is downscoped to it, and none is given
// while fail is set.
type tokens struct {
	clock clockwork.Clock
	fail  atomic.Bool
}

func (t *tokens) mint(what, prefix string) (string, time.Time, error) {
	if t.fail.Load() {
		return "", time.Time{}, errors.New("the token exchange failed")
	}
	return what + " " + prefix, t.clock.Now().Add(time.Hour).UTC(), nil
}

func (t *tokens) Read(_ context.Context, prefix string) (string, time.Time, error) {
	return t.mint("read", prefix)
}

func (t *tokens) Create(_ context.Context, prefix string) (string, time.Time, error) {
	return t.mint("create", prefix)
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
	caches  store.Objects
	tokens  *tokens
	binder  *binding.Binder
	ghc     *github.Client
	runner  *pass.Runner
	url     string
	logs    *lockedBuffer

	mu        sync.Mutex
	stops     []context.CancelFunc
	byMachine map[string]context.CancelFunc
	used      map[string]bool
	// cacheNames are the cache each job's actions name, by its run and
	// name, so that a rerun's job names what its first attempt's did.
	cacheNames map[string]string
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
	gcs.CreateBucket(cachesBucket)

	org := gh.Org(orgID, "lanescape")
	gh.SetForkApproval(org, ghemu.ApproveAllExternalContributors)
	app := gh.NewApp(77)
	gh.Install(app, org)
	repo := gh.Repo(org, repoID, "app")
	repo.Commit("main", map[string]string{"README": "app", consumer.RunnersFile: declares})

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
	caches, err := store.NewGCS(ctx, cachesBucket, gcs.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { caches.Close() })
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
	toks := &tokens{clock: clock}
	binder := &binding.Binder{GitHub: ghc, Objects: objects, OrganizationID: orgID, Clock: clock, Logger: log}
	dks := &disks.Service{Config: cfg, Project: gce.Project, GitHub: ghc, Compute: gcec, Objects: objects, Caches: caches,
		CacheBucket: cachesBucket, Tokens: toks, Clock: clock, Logger: log, StepDeadline: time.Second}
	svc := &machines.Service{Config: cfg, Project: gce.Project, ServiceURL: audience, MachineAccount: account,
		GitHub: ghc, Compute: gcec, Objects: objects, Binder: binder, Machines: verifier, Clock: clock, Logger: log,
		AssignmentWait: 3 * time.Second, AssignmentRetry: 50 * time.Millisecond, FailStopDelay: 100 * time.Millisecond, Disks: dks, Caches: dks, Preemptions: dks}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/jobs/started", svc.JobStarted())
	mux.Handle("POST /v1/jobs/finished", svc.JobFinished())
	mux.Handle("POST "+machines.DisksPath, svc.DiskRequest())
	mux.Handle("POST "+machines.CacheRestorePath, svc.CacheRestoreRequest())
	mux.Handle("POST "+machines.CacheCheckPath, svc.CacheCheckRequest())
	mux.Handle("POST "+machines.CacheSavePath, svc.CacheSaveRequest())
	mux.Handle("POST "+machines.CacheCommitPath, svc.CacheCommitRequest())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e := &env{t: t, clock: clock, gh: gh, gce: gce, gcs: gcs, org: org, repo: repo, cfg: cfg, svc: svc, disks: dks, objects: objects,
		caches: caches, tokens: toks, binder: binder, ghc: ghc, url: srv.URL, logs: logs, byMachine: map[string]context.CancelFunc{},
		used: map[string]bool{}, cacheNames: map[string]string{},
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

// facts are what GitHub tells a run's jobs' runners, as the hook reports
// them.
func facts(run *ghemu.Run) binding.Facts {
	f := binding.Facts{Event: run.Event, Ref: run.Ref, RefType: run.RefType, SHA: run.SHA}
	if p := run.EventPull; p != nil {
		f.PullRequest, f.HeadRepositoryID, f.HeadSHA = p.Number, p.HeadRepositoryID, p.HeadSHA
	}
	return f
}

// bind binds a run as its first job's check on a base-image machine would.
func (e *env) bind(run *ghemu.Run) string {
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
	// checked says the job-started check has allowed it.
	checked bool
	// restore is the answer to the job's restore of its cache, and saved
	// the version it saved, if it did.
	restore machines.CacheRestore
	saved   string
}

// start queues a job and plays it to its admission, which it expects to
// succeed.
func (e *env) start(run *ghemu.Run, spec ghemu.JobSpec) *job {
	e.t.Helper()
	j := e.queueJob(run, spec)
	return e.admit(run, j)
}

// queueJob queues a job, its labels made selectors of its run, with their
// caches' keys recorded as the run's.
func (e *env) queueJob(run *ghemu.Run, spec ghemu.JobSpec) *ghemu.WorkflowJob {
	e.t.Helper()
	e.keys(run, spec.Labels)
	cache := cacheOfLabels(spec.Labels)
	labels := make([]string, len(spec.Labels))
	for i, l := range spec.Labels {
		labels[i] = newLabel(l)
	}
	spec.Labels = labels
	e.cacheNames[fmt.Sprint(run.ID, "/", spec.Name)] = cache
	return e.gh.Queue(run, spec)
}

// admit gives a queued job a machine and plays it to its admission, then,
// if its label names a cache, asks for its work disk, as the disk action
// does, and restores the cache, as the cache action does.
func (e *env) admit(run *ghemu.Run, j *ghemu.WorkflowJob) *job {
	e.t.Helper()
	jb := e.take(run, j)
	if code := jb.started(); code != http.StatusOK {
		e.t.Fatalf("job-started check: %d", code)
	}
	jb.checked = true
	if e.cacheOf(j.ID) != "" {
		jb.disk()
		jb.restoreCache()
	}
	return jb
}

// call sends a request from the job's machine, signed with its identity
// token, and decodes the answer into out, if it's given.
func (jb *job) call(path string, body, out any) int {
	jb.e.t.Helper()
	tok := jb.e.gce.Identity.Mint(jb.e.gce.IdentityClaims(jb.in.Zone, jb.in.Name, audience, true))
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, jb.e.url+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		jb.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// cacheBody is a cache request's body, naming the job's cache.
func (jb *job) cacheBody() map[string]string {
	return map[string]string{"cache": jb.e.cacheOf(jb.j.ID)}
}

// askRestore is the job's check, if it hasn't had one, and then one request
// to restore its label's cache. It returns the request's status, and keeps
// the answer if it's 200.
func (jb *job) askRestore() int {
	jb.e.t.Helper()
	if !jb.checked {
		if code := jb.started(); code != http.StatusOK {
			jb.e.t.Fatalf("job-started check: %d", code)
		}
		jb.checked = true
	}
	var ans machines.CacheRestore
	code := jb.call(machines.CacheRestorePath, jb.cacheBody(), &ans)
	if code == http.StatusOK {
		jb.restore = ans
		jb.checkRestore()
	}
	return code
}

// restoreCache restores the job's cache, as its cache action does, asking
// again while the service says to.
func (jb *job) restoreCache() {
	jb.e.t.Helper()
	jb.e.eventually("the cache is restored", func() bool {
		code := jb.askRestore()
		if code != http.StatusOK && code != http.StatusServiceUnavailable {
			jb.e.t.Fatalf("cache restore: %d", code)
		}
		return code == http.StatusOK
	})
}

// checkRestore checks a restore's answer: none, or a version's objects in
// the caches' bucket, all of one version, with a token that reads only that
// version's.
func (jb *job) checkRestore() {
	jb.e.t.Helper()
	r := jb.restore
	if r.Restored == disks.StepNone {
		if len(r.Objects) != 0 || r.Token != "" || r.Bucket != "" {
			jb.e.t.Fatalf("a restore of nothing = %+v", r)
		}
		return
	}
	if len(r.Objects) == 0 || r.Bucket != cachesBucket || r.Expires == nil {
		jb.e.t.Fatalf("restore = %+v", r)
	}
	prefix := disks.VersionPrefix(repoID, versionOfObject(r.Objects[0].Name))
	if r.Token != "read "+prefix {
		jb.e.t.Fatalf("a restore's token is %q, want one that reads %s alone", r.Token, prefix)
	}
	for _, o := range r.Objects {
		if !strings.HasPrefix(o.Name, prefix) {
			jb.e.t.Fatalf("a restore's objects are of more than one version: %+v", r.Objects)
		}
	}
}

// versionOfObject is the version a caches' bucket object belongs to.
func versionOfObject(name string) string {
	parts := strings.SplitN(name, "/", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// restored is the version the job's restore chose, or "".
func (jb *job) restored() string {
	if len(jb.restore.Objects) == 0 {
		return ""
	}
	return versionOfObject(jb.restore.Objects[0].Name)
}

// disk asks for the job's work disk, as its disk action does, until it's
// ready, and returns where it came from.
func (jb *job) disk() string {
	jb.e.t.Helper()
	var ans machines.DiskAnswer
	jb.e.eventually("the work disk is ready", func() bool {
		ans = machines.DiskAnswer{}
		code := jb.call(machines.DisksPath, map[string]string{}, &ans)
		if code != http.StatusOK && code != http.StatusServiceUnavailable {
			jb.e.t.Fatalf("work disk request: %d", code)
		}
		return ans.State == machines.DiskStateReady
	})
	return ans.Restored
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
	b, _ := json.Marshal(machines.StartedRequest{RunID: jb.run.ID, Facts: facts(jb.run)})
	req, _ := http.NewRequest(http.MethodPost, jb.e.url+"/v1/jobs/started", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		jb.e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// askCheck is one request to check the job's cache, and its answer.
func (jb *job) askCheck() (int, machines.CacheCheck) {
	jb.e.t.Helper()
	var ans machines.CacheCheck
	return jb.call(machines.CacheCheckPath, jb.cacheBody(), &ans), ans
}

// askSave is one request to save the job's cache, and its answer.
func (jb *job) askSave() (int, machines.CacheSave) {
	jb.e.t.Helper()
	var ans machines.CacheSave
	return jb.call(machines.CacheSavePath, jb.cacheBody(), &ans), ans
}

// commit is one request to commit the job's cache, and its answer.
func (jb *job) commit() (int, machines.CacheCommit) {
	jb.e.t.Helper()
	var ans machines.CacheCommit
	return jb.call(machines.CacheCommitPath, jb.cacheBody(), &ans), ans
}

// upload writes a version's objects under the prefix a save named, as the
// save-cache action does with its token: a tar shard or two, whose contents
// name the job.
func (jb *job) upload(prefix string) []string {
	jb.e.t.Helper()
	var names []string
	for i := range 2 {
		name := fmt.Sprintf("%s%d.tar.zst", prefix, i)
		jb.e.gcs.Put(cachesBucket, name, []byte(fmt.Sprintf("job %d's shard %d of its cache", jb.j.ID, i)))
		names = append(names, name)
	}
	return names
}

// saveCache saves the job's cache, as its save-cache action does: it asks
// to save, uploads its objects under the version's prefix, and commits
// them. It returns the version, or "" if the run's scope saves nothing.
func (jb *job) saveCache() string {
	jb.e.t.Helper()
	code, ans := jb.askSave()
	if code != http.StatusOK {
		jb.e.t.Fatalf("save request: %d", code)
	}
	if !ans.Saves {
		if ans.Reason == "" || ans.Token != "" || ans.Prefix != "" {
			jb.e.t.Fatalf("a save refused = %+v", ans)
		}
		return ""
	}
	version := versionOfObject(ans.Prefix + "x")
	if ans.Bucket != cachesBucket || ans.Prefix != disks.VersionPrefix(repoID, version) || ans.Token != "create "+ans.Prefix ||
		ans.Expires == nil {
		jb.e.t.Fatalf("save = %+v; want the caches' bucket, a version's prefix and a token that creates objects there alone", ans)
	}
	jb.upload(ans.Prefix)
	if code, got := jb.commit(); code != http.StatusOK || got.Objects != 2 {
		jb.e.t.Fatalf("commit: %d, %+v", code, got)
	}
	jb.saved = version
	return version
}

// finish saves the job's cache, if save, as its save-cache action does,
// lets the job's steps run, waits for its runner to exit, and sends the
// supervisor's report, then powers the machine off, as the supervisor
// does, and runs a pass.
func (jb *job) finish(save bool) {
	jb.e.t.Helper()
	jb.report(save)
	jb.e.gce.Shutdown(jb.in.Zone, jb.in.Name)
	jb.e.pass()
}

// report is finish up to the supervisor's report: the machine is still
// running.
func (jb *job) report(save bool) {
	jb.e.t.Helper()
	if save && jb.e.cacheOf(jb.j.ID) != "" {
		jb.saveCache()
	}
	jb.runSteps()
	jb.sendReport()
}

// runSteps lets the job's steps run, and waits for its runner to exit.
func (jb *job) runSteps() {
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
}

// sendReport sends the supervisor's report.
func (jb *job) sendReport() {
	jb.e.t.Helper()
	tok := jb.e.gce.Identity.Mint(jb.e.gce.IdentityClaims(jb.in.Zone, jb.in.Name, audience, true))
	b, _ := json.Marshal(machines.FinishedRequest{})
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

// images are the service's images, resume images and any made as saved
// disks were, by name.
func (e *env) images() map[string]gceemu.ImageState {
	out := map[string]gceemu.ImageState{}
	for _, im := range e.gce.Images() {
		if !strings.HasPrefix(im.Name, "lsr-base-") {
			out[im.Name] = im
		}
	}
	return out
}

// versions are the versions that have objects in the caches' bucket, with
// their objects' names.
func (e *env) versions() map[string][]string {
	out := map[string][]string{}
	for _, n := range e.gcs.Names(cachesBucket) {
		v := versionOfObject(n)
		out[v] = append(out[v], n)
	}
	return out
}

// has reports whether a version's objects are in the caches' bucket.
func (e *env) has(version string) bool { _, ok := e.versions()[version]; return ok }

// hasImage reports whether Compute Engine has an image.
func (e *env) hasImage(name string) bool { _, ok := e.images()[name]; return ok }

// recorded reports whether a version or image has a record.
func (e *env) recorded(name string) bool {
	_, ok := e.record("caches/5001/" + name + ".json")
	if !ok {
		_, ok = e.record("images/5001/" + name + ".json")
	}
	return ok
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

// choiceHash names a run's restore choice for a test's label's cache and
// architecture.
func choiceHash(label string) string {
	m := testLabel.FindStringSubmatch(label)
	if m == nil {
		return ""
	}
	shape := strings.TrimSuffix(m[1], "-spot")
	return selector.Hash(m[2] + "\x00" + shape[strings.LastIndex(shape, "-")+1:])
}

// choice is the record of a run's restore choice for a test's label.
func (e *env) choice(run *ghemu.Run, label string) string {
	return "choices/5001/" + strconv.FormatInt(run.ID, 10) + "/" + choiceHash(label) + ".json"
}

// cacheOf is the cache a job's actions name, by its run and name.
func (e *env) cacheOf(jobID int64) string {
	j := e.gh.Job(jobID)
	return e.cacheNames[fmt.Sprint(j.Run.ID, "/", j.Name)]
}
