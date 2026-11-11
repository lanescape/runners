package machine_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	gceemu "github.com/lanescape/runners/emulators/compute"
	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/google"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/logging"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/server"
	"github.com/lanescape/runners/internal/store"
	"github.com/lanescape/runners/machine"
)

// These tests run the real service against the emulators, with each machine
// it makes booting the real supervisor, built from its embedded source,
// which runs the runner stand-in, which runs the real job-started hook's
// script and program. A job's steps ask for its work disk and its save as
// the actions do. The base image's rule that mounts the disk is played by
// the test once Compute Engine shows the disk attached: the mount program
// mounts as root, so its own tests, in mountwork, hold it to what it does.

const (
	orgID     = 303994381
	repoID    = 5001
	scheduler = "scheduler@lanescape-dev.iam.gserviceaccount.com"
	pusher    = "lanescape-runners-push@lanescape-dev.iam.gserviceaccount.com"
	small     = "lanescape-2vcpu-linux-amd64"
)

// files are the consumer's: one family of work disks, build, with no key
// files, and a setup script, which a job runs in its own steps.
var files = map[string]string{
	".github/runners.json": `{"disks": {"build": {}}}`,
	"ci/init.sh":           "echo setting up the machine\n",
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

type world struct {
	t           *testing.T
	gh          *ghemu.Emulator
	gce         *gceemu.Emulator
	gcs         *gcsemu.Emulator
	gg          *google.Issuer
	org         *ghemu.Account
	repo        *ghemu.Repo
	fork        *ghemu.Repo
	contributor *ghemu.Account
	maintainer  *ghemu.Account
	client      *github.Client
	objects     store.Objects
	act         action
	url         string
	logs        *lockedBuffer
	// later is the machines' part of the service's clock, which a test
	// moves on to bring a machine to its age limit.
	later      *laterClock
	supervisor string
	hook       string
	standIn    string

	mu       sync.Mutex
	machines map[string]*booted
	// holding makes machines wait to boot until the test releases them;
	// gates are the waiting ones, released those let go before they were
	// waiting, and ended stops any boot once the test is over.
	holding  bool
	gates    map[string]chan struct{}
	released map[string]bool
	ended    bool
}

// booted is a machine's guest: its supervisor and everything under it, and
// its console, which holds its job's log.
type booted struct {
	dir     string
	cmd     *exec.Cmd
	done    chan struct{}
	console *lockedBuffer
	tasks   sync.WaitGroup
}

func newWorld(t *testing.T) *world {
	t.Helper()
	programs, standIn := build(t)
	ctx := context.Background()
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	gcp := gceemu.New()
	t.Cleanup(gcp.Close)
	gcp.AddImage("lsr-base-amd64-0123456789abcdef0123", "", "X86_64")
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket("state")
	gg := google.New()
	t.Cleanup(gg.Close)

	org := gh.Org(orgID, "lanescape")
	gh.SetForkApproval(org, ghemu.ApproveAllExternalContributors)
	app := gh.NewApp(77)
	gh.Install(app, org)
	repo := gh.Repo(org, repoID, "app")
	repo.Commit("main", files)
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
	gcec, err := compute.New(ctx, gcp.Project, gcp.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.NewGCS(ctx, "state", gcs.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objects.Close() })

	// The service's address is its audience, known once it listens.
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	audience := srv.URL

	account := machines.MachineAccountOf(gcp.Project)
	verifier, err := auth.NewMachines(auth.Options{Issuer: auth.Issuer{URL: gcp.Identity.URL, JWKSURL: gcp.Identity.JWKSURL}, Audience: audience},
		gcp.Project, account)
	if err != nil {
		t.Fatal(err)
	}
	callers, err := auth.NewGoogle(auth.Options{Issuer: auth.Issuer{URL: gg.URL, JWKSURL: gg.JWKSURL}, Audience: audience}, []string{scheduler, pusher})
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	clock := clockwork.NewRealClock()
	binder := &binding.Binder{GitHub: ghc, Objects: objects, OrganizationID: orgID, Clock: clock, Logger: log}
	dks := &disks.Service{Config: cfg, Project: gcp.Project, GitHub: ghc, Compute: gcec, Objects: objects, Clock: clock, Logger: log}
	// A check that GitHub can't answer is answered sooner than in
	// production, so a test sees the hook ask again without a long wait.
	later := &laterClock{Clock: clock}
	svc := &machines.Service{Config: cfg, Project: gcp.Project, ServiceURL: audience, MachineAccount: account,
		GitHub: ghc, Compute: gcec, Objects: objects, Binder: binder, Machines: verifier, Clock: later, Logger: log,
		Disks: dks, Saves: dks, Preemptions: dks, FailStopDelay: time.Second, AssignmentWait: 3 * time.Second,
		AssignmentRetry: 100 * time.Millisecond}
	bootLogs, err := logging.New(ctx, gcp.Project, logging.View, gcp.LoggingOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	svc.BootLogs = bootLogs
	passes := &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock, Logger: log}
	body := pass.All(svc.Pass, dks.Pass)
	// A report can arrive as the test ends: no pass starts once cleanup
	// has begun waiting for those that did.
	var kicks sync.WaitGroup
	var kickMu sync.Mutex
	ended := false
	t.Cleanup(func() {
		kickMu.Lock()
		ended = true
		kickMu.Unlock()
		kicks.Wait()
	})
	svc.Kick = func() {
		kickMu.Lock()
		defer kickMu.Unlock()
		if !ended {
			kicks.Go(func() { _ = passes.Run(context.Background(), body) })
		}
	}
	handler, err = server.New(server.Config{
		Logger: log, OrganizationID: orgID, WebhookSecret: []byte("secret"), Publisher: nopPublisher{},
		ReconcileCallers: callers,
		Passes:           passes,
		Reconciler:       reconciler(body), Events: dks,
		JobStarted: svc.JobStarted(), JobFinished: svc.JobFinished(),
		Disks: svc.DiskRequest(), Save: svc.SaveRequest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, gh: gh, gce: gcp, gcs: gcs, gg: gg, org: org, repo: repo, fork: fork, contributor: contributor,
		maintainer: gh.User(42, "maintainer"), client: ghc, objects: objects, act: action{t: t, service: srv.URL}, url: srv.URL,
		logs: logs, later: later, supervisor: programs["supervisor"], hook: programs["hook"], standIn: standIn,
		machines: map[string]*booted{}, gates: map[string]chan struct{}{}, released: map[string]bool{}}
	gcp.OnBoot(w.boot)
	gcp.OnStop(w.halt)
	t.Cleanup(w.haltAll)
	return w
}

// laterClock is real time, moved on by as much as a test says.
type laterClock struct {
	clockwork.Clock
	by atomic.Int64
}

func (c *laterClock) Now() time.Time                  { return c.Clock.Now().Add(time.Duration(c.by.Load())) }
func (c *laterClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }
func (c *laterClock) Until(t time.Time) time.Duration { return t.Sub(c.Now()) }

// advance moves the clock on.
func (c *laterClock) advance(d time.Duration) { c.by.Add(int64(d)) }

// reconciler is a pass's body as the server runs it.
type reconciler pass.Body

func (r reconciler) Pass(p *pass.Pass) error { return r(p) }

type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, server.Delivery) error {
	return errors.New("no Pub/Sub here")
}

// holdBoots makes every machine wait to boot until the test releases it,
// so that a test chooses which runner is listening when GitHub hands out a
// job.
func (w *world) holdBoots() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.holding = true
}

// release lets a held machine boot, whether or not it's waiting yet.
func (w *world) release(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.released[name] = true
	if g := w.gates[name]; g != nil {
		close(g)
		delete(w.gates, name)
	}
}

// gate is what a booting machine waits on, if the test holds it.
func (w *world) gate(name string) chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.holding || w.released[name] || w.ended {
		return nil
	}
	g := make(chan struct{})
	w.gates[name] = g
	return g
}

// boot starts a machine's guest: the supervisor, with the base image's
// settings pointed at the emulators, writing to the machine's serial
// console; the hook's script, which runs the hook's program; an empty
// /work; and the base image's rule that mounts a work disk there.
func (w *world) boot(m gceemu.Machine) {
	if g := w.gate(m.Name); g != nil {
		<-g
	}
	dir, err := os.MkdirTemp("", "lsr-machine-")
	if err != nil {
		return
	}
	settings := filepath.Join(dir, "job-started.conf")
	_ = os.WriteFile(settings, []byte(fmt.Sprintf("metadata=%s\nask_every=500ms\nretry_every=200ms\n", m.MetadataURL)), 0o644)
	// The base image's script, running the program from where the test
	// built it, with the test's settings.
	script := strings.ReplaceAll(string(machine.Files()["job-started.sh"]), `"$@"`, settings)
	script = strings.ReplaceAll(script, machine.HookProgramPath, w.hook)
	hook := filepath.Join(dir, "job-started.sh")
	_ = os.WriteFile(hook, []byte(script), 0o755)
	runner := filepath.Join(dir, "runner")
	_ = os.MkdirAll(runner, 0o755)
	// /work is there, empty, before any job asks for a disk.
	_ = os.MkdirAll(filepath.Join(dir, "work"), 0o755)
	cfg := map[string]any{
		"metadata": m.MetadataURL, "runner": []string{w.standIn}, "runner_dir": runner, "runner_user": "",
		// A job's steps find the metadata server and /work where these
		// say, and the machine's own directory, where a test leaves
		// what a step waits for.
		"env": []string{"PATH=" + os.Getenv("PATH"), "LSR_TEST_METADATA=" + m.MetadataURL, "LSR_TEST_WORK=" + filepath.Join(dir, "work"),
			"LSR_TEST_DIR=" + dir},
		"hook":         hook,
		"poweroff":     []string{"curl", "-fsS", "-X", "POST", m.MetadataURL + "/_emulator/poweroff"},
		"metadata_for": "10s", "report_for": "30s",
	}
	b, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "supervisor.json")
	_ = os.WriteFile(path, b, 0o644)
	cmd := exec.Command(w.supervisor, "-config", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	bt := &booted{dir: dir, cmd: cmd, done: make(chan struct{}), console: &lockedBuffer{}}
	w.mu.Lock()
	if w.ended {
		w.mu.Unlock()
		os.RemoveAll(dir)
		return
	}
	if err := cmd.Start(); err != nil {
		w.mu.Unlock()
		return
	}
	w.machines[m.Name] = bt
	w.mu.Unlock()
	bt.tasks.Go(func() { w.mountWork(m, bt) })
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			bt.console.Write([]byte(sc.Text() + "\n"))
			w.gce.WriteSerial(m.Zone, m.Name, sc.Text()+"\n")
		}
		_ = cmd.Wait()
		close(bt.done)
	}()
}

// mountWork plays the base image's rule for the device named work: once
// Compute Engine shows the job's work disk attached, outside the job, it
// marks /work as the mounted disk, as a step that waits for the disk sees
// it, and records that it did, in work-disk.log.
func (w *world) mountWork(m gceemu.Machine, bt *booted) {
	for {
		select {
		case <-bt.done:
			return
		case <-time.After(100 * time.Millisecond):
		}
		in, ok := w.gce.Instance(m.Zone, m.Name)
		if !ok {
			return
		}
		disk := in.Disks[machines.WorkDevice]
		if disk == "" {
			continue
		}
		_ = os.WriteFile(filepath.Join(bt.dir, "work-disk.log"), []byte("mounted "+disk+"\n"), 0o644)
		_ = os.WriteFile(filepath.Join(bt.dir, "work", ".disk"), []byte(disk), 0o644)
		return
	}
}

// halt kills a stopped machine's guest, all of it.
func (w *world) halt(m gceemu.Machine) {
	w.mu.Lock()
	bt := w.machines[m.Name]
	w.mu.Unlock()
	if bt != nil {
		_ = syscall.Kill(-bt.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (w *world) haltAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ended = true
	for name, g := range w.gates {
		close(g)
		delete(w.gates, name)
	}
	for _, bt := range w.machines {
		_ = syscall.Kill(-bt.cmd.Process.Pid, syscall.SIGKILL)
		<-bt.done
		bt.tasks.Wait()
		os.RemoveAll(bt.dir)
	}
}

func (w *world) guest(name string) *booted {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.machines[name]
}

// console is everything a machine's guest wrote to its console: its
// supervisor's lines, and its job's log.
func (w *world) console(name string) string {
	if bt := w.guest(name); bt != nil {
		return bt.console.String()
	}
	return ""
}

// diskAnswer is where the work disk a job's steps asked for on a machine
// came from, by the service's ready answer.
func (w *world) diskAnswer(name string) string {
	w.t.Helper()
	for _, a := range w.act.stepAnswers(w.console(name)) {
		if a.Path == "/v1/disks" && a.Code == http.StatusOK && a.Body["state"] == "ready" {
			return a.Body["restored"].(string)
		}
	}
	w.t.Fatalf("the job on %s never had its work disk:\n%s", name, w.console(name))
	return ""
}

func (w *world) post(path, token string, body any) (int, map[string]any) {
	w.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, w.url+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// scope is a run's bound scope, as the bucket records it, or "".
func (w *world) scope(run *ghemu.Run) string {
	data, _, ok := w.gcs.Object("state", binding.Key(run.Repo.ID, run.ID))
	if !ok {
		return ""
	}
	var bd binding.Binding
	_ = json.Unmarshal(data, &bd)
	return bd.Scope.String()
}

// token is a machine's identity token for the service, as its metadata
// server mints it.
func (w *world) token(in gceemu.InstanceState) string {
	return w.gce.Identity.Mint(w.gce.IdentityClaims(in.Zone, in.Name, w.url, true))
}

// pass is the scheduler calling the reconcile endpoint. A pass that a
// report started may hold the lease; the call is made again, as the next
// sweep would be.
func (w *world) pass() {
	w.t.Helper()
	for end := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		code, out := w.post(server.ReconcilePath, w.gg.Token(scheduler, w.url), map[string]any{})
		if code == http.StatusNoContent {
			return
		}
		if code != http.StatusServiceUnavailable || time.Now().After(end) {
			w.t.Fatalf("reconcile: %d %v", code, out)
		}
	}
}

func (w *world) eventually(what string, d time.Duration, cond func() bool) {
	w.t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	w.t.Fatalf("timed out waiting until %s", what)
}

func (w *world) ours() []gceemu.InstanceState {
	var out []gceemu.InstanceState
	for _, in := range w.gce.Instances() {
		if strings.HasPrefix(in.Name, "lsr-") {
			out = append(out, in)
		}
	}
	return out
}

// madeFor is the machine made for a job.
func (w *world) madeFor(j *ghemu.WorkflowJob) gceemu.InstanceState {
	w.t.Helper()
	for _, in := range w.ours() {
		if in.Labels[machines.LabelJob] == strconv.FormatInt(j.ID, 10) {
			return in
		}
	}
	w.t.Fatalf("no machine made for job %d", j.ID)
	return gceemu.InstanceState{}
}

// gone reports whether a machine has been deleted.
func (w *world) gone(name string) bool {
	for _, in := range w.ours() {
		if in.Name == name {
			return false
		}
	}
	return true
}

// saved are the saved disks and resume images there are, by name.
func (w *world) saved() map[string]gceemu.ImageState {
	out := map[string]gceemu.ImageState{}
	for _, im := range w.gce.Images() {
		if !strings.HasPrefix(im.Name, "lsr-base-") {
			out[im.Name] = im
		}
	}
	return out
}

// push is the push subscription delivering a message to the reconcile
// endpoint.
func (w *world) push(subscription string, data []byte) int {
	w.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, w.url+server.ReconcilePath, bytes.NewReader(google.Push(w.gce.Project, subscription, "1", data)))
	req.Header.Set("Authorization", "Bearer "+w.gg.Token(pusher, w.url))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (w *world) done(j *ghemu.WorkflowJob) bool { return w.gh.Job(j.ID).Status == "completed" }

// runnerOf is the machine whose runner GitHub handed a job to, or "".
func (w *world) runnerOf(j *ghemu.WorkflowJob) string {
	if r := w.gh.Job(j.ID).Runner; r != nil {
		return r.Name
	}
	return ""
}

// noCredentials checks that nothing the service logged, nor any machine's
// console, holds a credential: a just-in-time configuration, something
// like an identity token, an access token, or any of the secrets given.
func (w *world) noCredentials(secrets ...string) {
	w.t.Helper()
	logs := w.logs.String()
	w.mu.Lock()
	for _, bt := range w.machines {
		logs += bt.console.String()
	}
	w.mu.Unlock()
	for _, in := range w.gce.Instances() {
		if jit := in.Metadata[machines.MetaJITConfig]; jit != "" && strings.Contains(logs, jit) {
			w.t.Fatal("a just-in-time configuration was logged")
		}
	}
	if strings.Contains(logs, "eyJ") || strings.Contains(logs, "ya29.") {
		w.t.Fatal("something like a token was logged")
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(logs, s) {
			w.t.Fatal("a token was logged")
		}
	}
}

// specs/0002-machines.md, A machine's life: a job runs on a machine of its
// selector, its check binds its run, it asks for its work disk and its save
// in its steps, and its supervisor's report starts the machine's cleanup,
// which saves the disk and the boot log.
func TestAJobRunsOnItsMachine(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	// Nothing binds the run before its first job's check, and nothing is
	// attached before the job asks.
	j := w.gh.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{w.act.selector(small, false)},
		Steps: w.act.steps(w.act.noDiskYetStep(), w.act.diskStep("build"), w.act.saveStep())})
	w.pass()
	ms := w.ours()
	if len(ms) != 1 {
		t.Fatalf("%d machines, want 1", len(ms))
	}
	m := ms[0]
	if len(m.Disks) != 1 {
		t.Fatalf("the machine was made with disks %v; a work disk comes only when its job asks", m.Disks)
	}
	w.eventually("the job is done", 60*time.Second, func() bool { return w.done(j) })
	if got := w.gh.Job(j.ID); got.Conclusion != "success" {
		t.Fatalf("job %s; its steps should have had their work disk and save:\n%s", got.Conclusion, w.console(m.Name))
	}
	if s := w.scope(run); s != "main" {
		t.Fatalf("the job's check bound the run %q, want main", s)
	}
	if got := w.diskAnswer(m.Name); got != "none" {
		t.Fatalf("main's first work disk was restored from %q, want none", got)
	}
	var saves []stepAnswer
	for _, a := range w.act.stepAnswers(w.console(m.Name)) {
		if a.Path == "/v1/disks/save" {
			saves = append(saves, a)
		}
	}
	if len(saves) != 1 || saves[0].Code != http.StatusOK || saves[0].Body["saves"] != true {
		t.Fatalf("save answers = %+v", saves)
	}
	// The report starts a pass at once, which deletes the machine and its
	// records, so what the report did is read from the service's log.
	w.eventually("the supervisor reports", 30*time.Second, func() bool {
		return strings.Contains(w.logs.String(), `msg="machine reported" machine=`+m.Name)
	})
	w.eventually("the reported machine is deleted", 30*time.Second, func() bool { return w.gone(m.Name) })
	// The base image's rule mounted the disk the job asked for, outside
	// the job, and only once Compute Engine had attached it.
	did, _ := os.ReadFile(filepath.Join(w.guest(m.Name).dir, "work-disk.log"))
	if !strings.HasPrefix(string(did), "mounted ") || !strings.Contains(string(did), machines.WorkDisk(m.Name)) {
		t.Fatalf("the work disk's rule did %q", did)
	}
	if n := len(w.gce.Disks()); n != 0 {
		t.Fatalf("%d disks left", n)
	}
	if !strings.Contains(w.logs.String(), `msg="work disk saved" machine=`+m.Name) || len(w.saved()) != 1 {
		t.Fatalf("the save the job asked for wasn't made: images %v", w.saved())
	}
	// Its boot log is read from Cloud Logging once the machine has gone.
	var log []byte
	w.eventually("the boot log is saved", 30*time.Second, func() bool {
		w.pass()
		var ok bool
		log, _, ok = w.gcs.Object("state", "logs/"+m.Name+"-"+m.Labels[machines.LabelCreated]+".txt")
		return ok
	})
	if !strings.Contains(string(log), "the runner exited") || !strings.Contains(string(log), "Running job: build") {
		t.Fatalf("boot log = %q", log)
	}
	w.noCredentials()
}

// specs/0002-machines.md, Failing a job: a job the service can't serve is
// stopped at its check, with the reason in its log, none of its steps run,
// and its run is cancelled.
func TestAJobFailedWithItsReason(t *testing.T) {
	w := newWorld(t)
	w.fork.Commit("feature", map[string]string{"src.go": "package app"})
	p := w.gh.PullRequest(w.repo, "main", w.fork, "feature", w.contributor)
	run := w.gh.PullRequestTargetRun(p)
	// A contribution's run that no maintainer has approved may have only
	// the smallest shape, standard; this job asks for Spot. It's given a
	// machine like any queued job, and its check fails it.
	j := w.gh.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{w.act.selector(small, true)},
		Steps: `touch "$LSR_TEST_DIR/a-step-ran"`, AlwaysSteps: `touch "$LSR_TEST_DIR/an-always-step-ran"`})
	w.pass()
	m := w.madeFor(j)
	w.eventually("the hook prints why in the job's log", 60*time.Second, func() bool {
		return strings.Contains(w.console(m.Name), "this job can't run: the contribution's run isn't approved")
	})
	w.eventually("the failed job's machine stops", 60*time.Second, func() bool {
		in, ok := w.gce.Instance(m.Zone, m.Name)
		return !ok || in.Status == "TERMINATED"
	})
	// The service stops the machine before it records the refusal and
	// cancels the run, so the record can come just after the stop.
	w.eventually("the run is cancelled, saying why", 60*time.Second, func() bool {
		rf, _, ok := w.gcs.Object("state", "refusals/5001/"+strconv.FormatInt(run.ID, 10)+"-1.json")
		var rec machines.Refusal
		return ok && json.Unmarshal(rf, &rec) == nil && strings.Contains(rec.Reason, "isn't approved") && !rec.CancelledAt.IsZero()
	})
	if c := w.gh.Job(j.ID).Conclusion; c == "success" {
		t.Fatal("a failed job succeeded")
	}
	// The hook never returns, so not even a step whose condition is
	// always() runs.
	for _, f := range []string{"a-step-ran", "an-always-step-ran"} {
		if _, err := os.Stat(filepath.Join(w.guest(m.Name).dir, f)); err == nil {
			t.Fatalf("a failed job's step ran: %s", f)
		}
	}
	if s := w.scope(run); s != "" {
		t.Fatalf("an unapproved contribution's run was bound %q", s)
	}
	w.noCredentials()
}

// specs/0002-machines.md, A machine's life: machines are general, so a job
// may take a machine made for another run's job, and is admitted, bound and
// given its work disk for its own run.
func TestAJobTakesAMachineMadeForAnotherRunsJob(t *testing.T) {
	w := newWorld(t)
	w.holdBoots()
	main := w.gh.PushRun(w.repo, "main")
	w.repo.Commit("feature", map[string]string{"f": "1"})
	p := w.gh.PullRequest(w.repo, "main", w.repo, "feature", w.maintainer)
	pr := w.gh.PullRequestRun(p)
	steps := w.act.steps(w.act.diskStep("build"))
	first := w.gh.Queue(main, ghemu.JobSpec{Name: "build", Labels: []string{small}, Steps: steps})
	second := w.gh.Queue(pr, ghemu.JobSpec{Name: "build", Labels: []string{small}, Steps: steps})
	w.pass()
	forFirst, forSecond := w.madeFor(first).Name, w.madeFor(second).Name
	// The machine made for the pull request's job boots first, and GitHub
	// hands its runner the oldest queued job of its selector, main's.
	w.release(forSecond)
	w.eventually("main's job is on the pull request's job's machine", 60*time.Second, func() bool {
		return w.runnerOf(first) == forSecond
	})
	w.release(forFirst)
	w.eventually("both jobs are done", 60*time.Second, func() bool { return w.done(first) && w.done(second) })
	for _, j := range []*ghemu.WorkflowJob{first, second} {
		if c := w.gh.Job(j.ID).Conclusion; c != "success" {
			t.Fatalf("job %d: %s", j.ID, c)
		}
	}
	if w.runnerOf(second) != forFirst {
		t.Fatalf("the pull request's job ran on %s, want %s", w.runnerOf(second), forFirst)
	}
	if s := w.scope(main); s != "main" {
		t.Fatalf("main's run bound %q", s)
	}
	if s := w.scope(pr); s != "pr"+strconv.Itoa(p.Number) {
		t.Fatalf("the pull request's run bound %q", s)
	}
	// Each job was admitted, and its work disk chosen, for its own run.
	logs := w.logs.String()
	for _, c := range []struct {
		machine string
		run     *ghemu.Run
	}{{forSecond, main}, {forFirst, pr}} {
		for _, msg := range []string{`msg="job admitted"`, `msg="work disk starts empty: no saved disk to restore"`} {
			if !strings.Contains(logs, msg+" machine="+c.machine+" repository_id=5001 run_id="+strconv.FormatInt(c.run.ID, 10)) &&
				!strings.Contains(logs, msg+" machine="+c.machine+" run_id="+strconv.FormatInt(c.run.ID, 10)) {
				t.Fatalf("no %s for run %d on %s", msg, c.run.ID, c.machine)
			}
		}
	}
	w.noCredentials()
}

// specs/0002-machines.md, The job-started check: a fork's job that GitHub
// hands to a machine made for another job is refused at its check, before
// any step, the machine stops, and the fork's run is cancelled.
func TestAForksJobHandedToAMachine(t *testing.T) {
	w := newWorld(t)
	w.holdBoots()
	main := w.gh.PushRun(w.repo, "main")
	sel := w.act.selector(small, false)
	// A fork's pull request names the service's selector, and a maintainer
	// approved its run; its jobs are queued first. GitHub won't cancel
	// runs for now, so they stay queued after the pass that refuses the
	// fork's run, and GitHub hands one of them to the machine made for
	// main's job.
	w.fork.Commit("evil", map[string]string{"x": "1"})
	p := w.gh.PullRequest(w.repo, "main", w.fork, "evil", w.contributor)
	forkRun := w.gh.PullRequestRun(p)
	stray := w.gh.Queue(forkRun, ghemu.JobSpec{Name: "steal", Labels: []string{sel}, Steps: `touch "$LSR_TEST_DIR/a-fork-ran"`,
		AlwaysSteps: `touch "$LSR_TEST_DIR/a-fork-ran"`})
	second := w.gh.Queue(forkRun, ghemu.JobSpec{Name: "steal-too", Labels: []string{sel}})
	meant := w.gh.Queue(main, ghemu.JobSpec{Name: "build", Labels: []string{sel}})
	w.gh.Faults().Add(fault.Rule{Method: http.MethodPost, Match: func(r *http.Request) bool {
		return strings.HasSuffix(r.URL.Path, "/cancel") || strings.HasSuffix(r.URL.Path, "/force-cancel")
	}, Respond: fault.Status(http.StatusBadGateway, nil, `{"message": "Bad Gateway"}`)})
	w.pass()
	ms := w.ours()
	if len(ms) != 1 || ms[0].Labels[machines.LabelJob] != strconv.FormatInt(meant.ID, 10) {
		t.Fatalf("machines %v; only main's job gets one", ms)
	}
	w.release(ms[0].Name)
	w.eventually("a fork's job is on main's job's machine", 60*time.Second, func() bool {
		return w.runnerOf(stray) == ms[0].Name || w.runnerOf(second) == ms[0].Name
	})
	w.eventually("the machine is stopped", 60*time.Second, func() bool {
		in, ok := w.gce.Instance(ms[0].Zone, ms[0].Name)
		return !ok || in.Status == "TERMINATED" || in.Status == "STOPPED"
	})
	if _, err := os.Stat(filepath.Join(w.guest(ms[0].Name).dir, "a-fork-ran")); err == nil {
		t.Fatal("a fork's job's step ran")
	}
	if !strings.Contains(w.logs.String(), `msg="job refused" machine=`+ms[0].Name) {
		t.Fatal("the fork's job wasn't refused at its check")
	}
	// Once GitHub cancels again, the fork's run is cancelled, and main's job
	// runs on a machine of its own.
	w.gh.Faults().Reset()
	for _, n := range w.ours() {
		w.release(n.Name)
	}
	w.pass()
	if !strings.Contains(w.logs.String(), `msg="refused run cancelled" repository_id=5001 run_id=`+strconv.FormatInt(forkRun.ID, 10)) {
		t.Fatal("the refused run wasn't cancelled once GitHub would")
	}
	// Its refused job's runner is gone, so it takes force; here, sooner
	// than the service would.
	repo, _ := w.client.RepositoryByID(context.Background(), w.repo.ID)
	if st, _ := w.gh.RunState(forkRun); st != "completed" {
		if err := w.client.ForceCancelRun(context.Background(), repo, forkRun.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, j := range []*ghemu.WorkflowJob{stray, second} {
		if c := w.gh.Job(j.ID).Conclusion; c == "success" {
			t.Fatalf("a fork's job %s", c)
		}
	}
	if w.scope(forkRun) != "" {
		t.Fatal("a fork's pull request's run was bound")
	}
	w.eventually("main's job runs on a machine of its own", 60*time.Second, func() bool {
		w.pass()
		for _, n := range w.ours() {
			w.release(n.Name)
		}
		return w.done(meant)
	})
	if c := w.gh.Job(meant.ID).Conclusion; c != "success" {
		t.Fatalf("main's job: %s", c)
	}
	if strings.Contains(w.logs.String(), `msg="job admitted"`+" machine="+ms[0].Name) {
		t.Fatal("a fork's job was admitted")
	}
	w.noCredentials()
}

// specs/0002-machines.md, Binding a run: an approved contribution's run is
// bound by its contribution job's check, and its next job works in its pull
// request's scope.
func TestAnApprovedContribution(t *testing.T) {
	w := newWorld(t)
	w.fork.Commit("feature", map[string]string{"src.go": "package app"})
	p := w.gh.PullRequest(w.repo, "main", w.fork, "feature", w.contributor)
	run := w.gh.PullRequestTargetRun(p)
	// The contribution job names the environment, so GitHub holds it until
	// a maintainer approves; its check binds the run.
	j := w.gh.Queue(run, ghemu.JobSpec{Name: "contribution", Labels: []string{w.act.selector(small, false)},
		Environment: machines.ContributionEnvironment})
	w.pass()
	if n := len(w.ours()); n != 0 {
		t.Fatal("a machine for a contribution that's waiting for approval")
	}
	w.gh.Approve(run, machines.ContributionEnvironment, w.maintainer)
	w.pass()
	w.eventually("the contribution job runs", 60*time.Second, func() bool { return w.done(j) })
	if c := w.gh.Job(j.ID).Conclusion; c != "success" {
		t.Fatalf("the contribution job: %s", c)
	}
	if s := w.scope(run); s != "pr"+strconv.Itoa(p.Number) {
		t.Fatalf("the approved contribution bound %q", s)
	}
	// Its next job asks for a work disk, in the pull request's scope.
	next := w.gh.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{w.act.selector(small, false)},
		Steps: w.act.steps(w.act.diskStep("build"), w.act.saveStep())})
	w.pass()
	w.eventually("the contribution's build runs", 60*time.Second, func() bool { return w.done(next) })
	if c := w.gh.Job(next.ID).Conclusion; c != "success" {
		t.Fatalf("the contribution's build: %s", c)
	}
	if got := w.diskAnswer(w.runnerOf(next)); got != "none" {
		t.Fatalf("the contribution's disk was restored from %q, want none", got)
	}
	w.noCredentials()
}

// specs/0002-machines.md, A machine's life: the hook waits for the
// service's answer, and its job's steps don't run until it has one.
func TestTheHookWaitsForAnAnswer(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	// GitHub can't say which job is on the runner for a while.
	w.gh.ServerError("/repos/lanescape/app/actions/runs/", http.StatusBadGateway, 0)
	j := w.gh.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{w.act.selector(small, false)}})
	// The pass reads the queue before GitHub fails.
	w.gh.Faults().Reset()
	w.pass()
	w.gh.ServerError("/repos/lanescape/app/actions/", http.StatusBadGateway, 0)
	m := w.ours()[0]
	w.eventually("the hook is told to ask again", 60*time.Second, func() bool {
		return strings.Contains(w.logs.String(), `msg="job-started check not answered" machine=`+m.Name)
	})
	if w.done(j) {
		t.Fatal("the job ran with no answer from the service")
	}
	if in, _ := w.gce.Instance(m.Zone, m.Name); len(in.Disks) != 1 {
		t.Fatal("a disk attached with no answer")
	}
	w.gh.Faults().Reset()
	w.eventually("the job runs once GitHub answers", 90*time.Second, func() bool { return w.done(j) })
	if c := w.gh.Job(j.ID).Conclusion; c != "success" {
		t.Fatalf("job: %s", c)
	}
}

// restored is the image the service logged a machine's work disk was
// restored from, or "".
func (w *world) restored(machine string) string {
	for _, l := range strings.Split(w.logs.String(), "\n") {
		if strings.Contains(l, "machine="+machine+" ") && (strings.Contains(l, `msg="work disk restored"`) || strings.Contains(l, `msg="work disk from a resume image"`)) {
			if _, img, ok := strings.Cut(l, " image="); ok {
				return strings.Fields(img)[0]
			}
		}
	}
	return ""
}

// runJob queues a job, gives it a machine, and waits for it to finish
// successfully and its machine to go, returning the machine it ran on.
func (w *world) runJob(run *ghemu.Run, spec ghemu.JobSpec) string {
	w.t.Helper()
	j := w.gh.Queue(run, spec)
	w.pass()
	w.eventually("the job is done", 60*time.Second, func() bool { return w.done(j) })
	name := w.runnerOf(j)
	if c := w.gh.Job(j.ID).Conclusion; c != "success" {
		w.t.Fatalf("job %s:\n%s", c, w.console(name))
	}
	w.eventually("its machine is deleted", 60*time.Second, func() bool { return w.gone(name) })
	return name
}

// specs/0003-disks.md, Saved disks: a pull request's run is bound from
// GitHub's records, its job's work disk starts from main's saved disk, its
// save is its own, and its next run starts from it, while main's goes on
// from main's.
func TestAPullRequestsRunThroughToItsSaves(t *testing.T) {
	w := newWorld(t)
	build := func(run *ghemu.Run) ghemu.JobSpec {
		return ghemu.JobSpec{Name: "build", Labels: []string{w.act.selector(small, false)},
			Steps: w.act.steps(w.act.diskStep("build"), w.act.saveStep())}
	}
	// Main saves first.
	main := w.gh.PushRun(w.repo, "main")
	if got := w.diskAnswer(w.runJob(main, build(main))); got != "none" {
		t.Fatalf("main's first disk was restored from %q", got)
	}
	mains := w.saved()
	if len(mains) != 1 {
		t.Fatalf("images %v, want main's", mains)
	}
	var mainImage string
	for n := range mains {
		mainImage = n
	}

	// A pull request starts from main's, and saves its own.
	w.repo.Commit("feature", map[string]string{"x": "1"})
	p := w.gh.PullRequest(w.repo, "main", w.repo, "feature", w.maintainer)
	run := w.gh.PullRequestRun(p)
	m := w.runJob(run, build(run))
	if s := w.scope(run); s != "pr"+strconv.Itoa(p.Number) {
		t.Fatalf("the pull request's run bound %s", s)
	}
	if got, img := w.diskAnswer(m), w.restored(m); got != "main_exact" || img != mainImage {
		t.Fatalf("the pull request's job restored %q from %q, want main's %s", got, img, mainImage)
	}
	var prImage string
	for n := range w.saved() {
		if n != mainImage {
			prImage = n
		}
	}
	if prImage == "" || len(w.saved()) != 2 {
		t.Fatalf("images %v; the pull request's save is missing", w.saved())
	}

	// Its next run starts from its own; main's next run from main's.
	next := w.gh.PullRequestRun(p)
	if m := w.runJob(next, build(next)); w.diskAnswer(m) != "pull_request_exact" || w.restored(m) != prImage {
		t.Fatalf("the pull request's next run restored %q from %q, want %s", w.diskAnswer(m), w.restored(m), prImage)
	}
	again := w.gh.PushRun(w.repo, "main")
	if m := w.runJob(again, build(again)); w.diskAnswer(m) != "main_exact" || w.restored(m) != mainImage {
		t.Fatalf("main restored %q from %q, want its own %s", w.diskAnswer(m), w.restored(m), mainImage)
	}
	w.noCredentials()
}

// specs/0002-machines.md, Preemption: a Spot machine preempted mid-job
// leaves a resume image of its work disk, its run is rerun, and the rerun's
// job's disk request is answered from the resume image.
func TestASpotMachinePreemptedMidJobAndItsRunResumed(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	label := w.act.selector(small, true)
	// The first attempt runs until it's preempted; its rerun finishes.
	j := w.gh.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{label},
		Steps: w.act.steps(w.act.diskStep("build"), `if [ "$GITHUB_RUN_ATTEMPT" = 1 ]; then sleep 600; fi`)})
	w.pass()
	m := w.ours()[0]
	if m.Provisioning != "SPOT" {
		t.Fatalf("machine is %s", m.Provisioning)
	}
	w.eventually("the job runs with its work disk", 60*time.Second, func() bool {
		bt := w.guest(m.Name)
		if bt == nil {
			return false
		}
		_, err := os.Stat(filepath.Join(bt.dir, "work", ".disk"))
		return w.gh.Job(j.ID).Status == "in_progress" && err == nil
	})
	if err := w.gce.Preempt(m.Zone, m.Name); err != nil {
		t.Fatal(err)
	}
	// Compute Engine's event arrives through the push subscription, which
	// records it before acknowledging; the zone operation is forgotten.
	entry, _ := w.gce.LogEntry(m.Zone, m.Name, "compute.instances.preempted")
	w.gce.ForgetOperations()
	if code := w.push("lanescape-runners-compute-events-reconcile", entry); code != http.StatusNoContent {
		t.Fatalf("push: %d", code)
	}
	if _, _, ok := w.gcs.Object("state", "events/"+strconv.FormatUint(m.ID, 10)+".json"); !ok {
		t.Fatal("the preemption event wasn't recorded")
	}
	// GitHub fails the job whose runner it lost.
	w.gh.CompleteJobByID(j.ID, "failure")
	resume := disks.ResumeName(repoID, m.ID)
	w.eventually("the run is rerun, with a resume image", 60*time.Second, func() bool {
		w.pass()
		_, ok := w.saved()[resume]
		status, _ := w.gh.RunState(run)
		return ok && status == "in_progress"
	})
	rec, _, ok := w.gcs.Object("state", "preemptions/5001/"+strconv.FormatInt(run.ID, 10)+"/"+strconv.FormatUint(m.ID, 10)+".json")
	if !ok || !strings.Contains(string(rec), `"preempted":"yes"`) {
		t.Fatalf("preemption record = %s", rec)
	}
	var rerun *ghemu.WorkflowJob
	for _, j := range w.gh.RunJobs(run, 2) {
		if j.Name == "build" {
			rerun = &ghemu.WorkflowJob{ID: j.ID}
		}
	}
	if rerun == nil {
		t.Fatal("no rerun job")
	}
	w.pass()
	w.eventually("the rerun's job is done", 60*time.Second, func() bool { return w.done(rerun) })
	second := w.runnerOf(rerun)
	if c := w.gh.Job(rerun.ID).Conclusion; c != "success" {
		t.Fatalf("the rerun's job: %s", c)
	}
	if got, img := w.diskAnswer(second), w.restored(second); got != "resume" || img != resume {
		t.Fatalf("the rerun's job started from %q, %q, want its resume image %s", got, img, resume)
	}
	w.eventually("the resume image goes once its job has started from it", 60*time.Second, func() bool {
		w.pass()
		_, ok := w.saved()[resume]
		return !ok
	})
	w.noCredentials()
}

// specs/0002-machines.md, The age limit: a standard machine whose job is
// still running at the age limit is stopped, its job recorded with a resume
// image of its work disk, and its run is rerun, and the rerun's job's disk
// request is answered from the resume image, as after a preemption.
func TestAJobStoppedAtTheAgeLimitAndItsRunResumed(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	// The first attempt runs until its machine reaches the age limit; its
	// rerun finishes.
	j := w.gh.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{w.act.selector(small, false)},
		Steps: w.act.steps(w.act.diskStep("build"), `if [ "$GITHUB_RUN_ATTEMPT" = 1 ]; then sleep 600; fi`)})
	w.pass()
	m := w.ours()[0]
	if m.Provisioning != "STANDARD" {
		t.Fatalf("machine is %s", m.Provisioning)
	}
	w.eventually("the job runs with its work disk", 60*time.Second, func() bool {
		bt := w.guest(m.Name)
		if bt == nil {
			return false
		}
		_, err := os.Stat(filepath.Join(bt.dir, "work", ".disk"))
		return w.gh.Job(j.ID).Status == "in_progress" && err == nil
	})
	w.later.advance(machines.MaxAge)
	w.pass()
	if !w.gone(m.Name) {
		t.Fatal("a machine past its age limit wasn't deleted")
	}
	resume := disks.ResumeName(repoID, m.ID)
	if _, ok := w.saved()[resume]; !ok {
		t.Fatalf("images %v; want the resume image %s", w.saved(), resume)
	}
	rec, _, ok := w.gcs.Object("state", "preemptions/5001/"+strconv.FormatInt(run.ID, 10)+"/"+strconv.FormatUint(m.ID, 10)+".json")
	if !ok || !strings.Contains(string(rec), `"age_limit":true`) || !strings.Contains(string(rec), `"preempted":"no"`) {
		t.Fatalf("record = %s", rec)
	}
	// GitHub fails the job whose runner it lost.
	w.gh.CompleteJobByID(j.ID, "failure")
	w.eventually("the run is rerun", 60*time.Second, func() bool {
		w.pass()
		status, _ := w.gh.RunState(run)
		return status == "in_progress"
	})
	var rerun *ghemu.WorkflowJob
	for _, j := range w.gh.RunJobs(run, 2) {
		if j.Name == "build" {
			rerun = &ghemu.WorkflowJob{ID: j.ID}
		}
	}
	if rerun == nil {
		t.Fatal("no rerun job")
	}
	w.pass()
	w.eventually("the rerun's job is done", 60*time.Second, func() bool { return w.done(rerun) })
	second := w.runnerOf(rerun)
	if c := w.gh.Job(rerun.ID).Conclusion; c != "success" {
		t.Fatalf("the rerun's job: %s", c)
	}
	if got, img := w.diskAnswer(second), w.restored(second); got != "resume" || img != resume {
		t.Fatalf("the rerun's job started from %q, %q, want its resume image %s", got, img, resume)
	}
	w.eventually("the resume image goes once its job has started from it", 60*time.Second, func() bool {
		w.pass()
		_, ok := w.saved()[resume]
		return !ok
	})
	w.noCredentials()
}

// A run's first job's check, while GitHub can't confirm what its runner
// said, answers the hook to ask again, and binds once GitHub can.
func TestBindingWhileGitHubIsDown(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	w.gh.ServerError("/repos/lanescape/app/compare/", http.StatusBadGateway, 2)
	j := w.gh.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{w.act.selector(small, false)}})
	w.pass()
	w.eventually("the job runs once GitHub confirms its run", 90*time.Second, func() bool { return w.done(j) })
	if c := w.gh.Job(j.ID).Conclusion; c != "success" || w.scope(run) != "main" {
		t.Fatalf("job %s, scope %q", c, w.scope(run))
	}
}
