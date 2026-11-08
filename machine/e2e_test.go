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
	"syscall"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	gceemu "github.com/lanescape/runners/emulators/compute"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/google"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/server"
	"github.com/lanescape/runners/internal/store"
)

// These tests run the real service against the emulators, with each machine
// it makes booting the real supervisor, which runs the runner stand-in,
// which runs the real job-started hook.

const (
	orgID     = 303994381
	scheduler = "scheduler@lanescape-dev.iam.gserviceaccount.com"
	bootstrap = "lanescape/app/.github/workflows/bootstrap.yml@refs/heads/main"
	sel       = "lanescape-2vcpu-linux-amd64-main"
	diskSel   = "lanescape-2vcpu-linux-amd64-disk-build-0123456789abcdef0123-main"
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
	client      *github.Client
	url         string
	logs        *lockedBuffer
	fake        string
	supervisor  string
	standIn     string

	mu       sync.Mutex
	machines map[string]*booted
}

// booted is a machine's guest: its supervisor and everything under it.
type booted struct {
	dir  string
	cmd  *exec.Cmd
	done chan struct{}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	supervisor, standIn := build(t)
	ctx := context.Background()
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	gcp := gceemu.New()
	t.Cleanup(gcp.Close)
	gcp.AddImage("lsr-base-amd64-1", "", "X86_64")
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket("state")
	gg := google.New()
	t.Cleanup(gg.Close)

	org := gh.Org(orgID, "lanescape")
	gh.SetForkApproval(org, ghemu.ApproveAllExternalContributors)
	app := gh.NewApp(77)
	gh.Install(app, org)
	repo := gh.Repo(org, 5001, "app")
	repo.Commit("main", map[string]string{binding.RunnersFile: `{"bootstrap": "` + bootstrap + `"}`})
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
	tokens, err := auth.NewMachines(auth.Options{Issuer: auth.Issuer{URL: gcp.Identity.URL, JWKSURL: gcp.Identity.JWKSURL}, Audience: audience},
		gcp.Project, account)
	if err != nil {
		t.Fatal(err)
	}
	callers, err := auth.NewGoogle(auth.Options{Issuer: auth.Issuer{URL: gg.URL, JWKSURL: gg.JWKSURL}, Audience: audience}, []string{scheduler})
	if err != nil {
		t.Fatal(err)
	}
	actions, err := auth.NewActions(auth.Options{Issuer: auth.Issuer{URL: gh.Actions.URL, JWKSURL: gh.Actions.JWKSURL}, Audience: audience}, orgID)
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
	svc := &machines.Service{Config: cfg, Project: gcp.Project, ServiceURL: audience, MachineAccount: account,
		GitHub: ghc, Compute: gcec, Objects: objects, Binder: binder, Machines: tokens, Clock: clock, Logger: log}
	passes := &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock, Logger: log}
	var kicks sync.WaitGroup
	t.Cleanup(kicks.Wait)
	svc.Kick = func() { kicks.Go(func() { _ = passes.Run(context.Background(), svc.Pass) }) }
	handler, err = server.New(server.Config{
		Logger: log, OrganizationID: orgID, WebhookSecret: []byte("secret"), Publisher: nopPublisher{},
		ReconcileCallers: callers,
		Passes:           passes,
		Reconciler:       svc, Actions: actions, Binder: binder,
		JobStarted: svc.JobStarted(), JobFinished: svc.JobFinished(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, gh: gh, gce: gcp, gcs: gcs, gg: gg, org: org, repo: repo, fork: fork, contributor: contributor, client: ghc,
		url: srv.URL, logs: logs, fake: fakeBin(t), supervisor: supervisor, standIn: standIn, machines: map[string]*booted{}}
	gcp.OnBoot(w.boot)
	gcp.OnStop(w.halt)
	t.Cleanup(w.haltAll)
	return w
}

type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, server.Delivery) error {
	return errors.New("no Pub/Sub here")
}

// boot starts a machine's guest: the supervisor, with the base image's
// settings pointed at the emulators, writing to the machine's serial
// console.
func (w *world) boot(m gceemu.Machine) {
	dir, err := os.MkdirTemp("", "lsr-machine-")
	if err != nil {
		return
	}
	device := filepath.Join(dir, "google-work")
	_ = os.WriteFile(device, nil, 0o644)
	settings := filepath.Join(dir, "job-started.conf")
	_ = os.WriteFile(settings, []byte(fmt.Sprintf("metadata=%s\ndevice=%s\nmountpoint=%s\n", m.MetadataURL, device, filepath.Join(dir, "work"))), 0o644)
	hook, _ := filepath.Abs("job-started.sh")
	wrapper := filepath.Join(dir, "job-started")
	_ = os.WriteFile(wrapper, []byte("#!/bin/bash\nexec bash "+hook+" "+settings+"\n"), 0o755)
	runner := filepath.Join(dir, "runner")
	_ = os.MkdirAll(runner, 0o755)
	cfg := map[string]any{
		"metadata": m.MetadataURL, "runner": []string{w.standIn}, "runner_dir": runner, "runner_user": "",
		"env": []string{"PATH=" + w.fake + ":" + os.Getenv("PATH"), "SUDO_LOG=" + filepath.Join(dir, "sudo.log"),
			"LSR_TEST_SAVE=" + filepath.Join(dir, "save")},
		"hook": wrapper, "save_marker": filepath.Join(dir, "save"),
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
	if err := cmd.Start(); err != nil {
		return
	}
	bt := &booted{dir: dir, cmd: cmd, done: make(chan struct{})}
	w.mu.Lock()
	w.machines[m.Name] = bt
	w.mu.Unlock()
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			w.gce.WriteSerial(m.Zone, m.Name, sc.Text()+"\n")
		}
		_ = cmd.Wait()
		close(bt.done)
	}()
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
	for _, bt := range w.machines {
		_ = syscall.Kill(-bt.cmd.Process.Pid, syscall.SIGKILL)
		<-bt.done
		os.RemoveAll(bt.dir)
	}
}

func (w *world) guest(name string) *booted {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.machines[name]
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

// bind is the runners action binding a run, from its first job.
func (w *world) bind(run *ghemu.Run, job ghemu.Job, req binding.Request) string {
	w.t.Helper()
	job.Audience = w.url
	code, out := w.post(server.BindPath, w.gh.ActionsToken(run, job, nil), req)
	if code != http.StatusOK {
		w.t.Fatalf("bind: %d %v", code, out)
	}
	return out["scope"].(string)
}

// pass is the scheduler calling the reconcile endpoint.
func (w *world) pass() {
	w.t.Helper()
	if code, out := w.post(server.ReconcilePath, w.gg.Token(scheduler, w.url), map[string]any{}); code != http.StatusNoContent {
		w.t.Fatalf("reconcile: %d %v", code, out)
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

func (w *world) done(j *ghemu.WorkflowJob) bool { return w.gh.Job(j.ID).Status == "completed" }

func (w *world) noCredentials() {
	w.t.Helper()
	logs := w.logs.String()
	for _, in := range w.gce.Instances() {
		if jit := in.Metadata[machines.MetaJITConfig]; jit != "" && strings.Contains(logs, jit) {
			w.t.Fatal("a just-in-time configuration was logged")
		}
	}
	if strings.Contains(logs, "eyJ") {
		w.t.Fatal("something like a token was logged")
	}
}

func TestAJobRunsOnItsMachine(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	if scope := w.bind(run, ghemu.Job{}, binding.Request{}); scope != "main" {
		t.Fatalf("scope %s", scope)
	}
	j := w.gh.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{diskSel},
		Steps: `[ -d "$(dirname "$LSR_TEST_SAVE")/work" ] && touch "$LSR_TEST_SAVE"`})
	w.pass()
	ms := w.ours()
	if len(ms) != 1 {
		t.Fatalf("%d machines, want 1", len(ms))
	}
	m := ms[0]
	w.eventually("the job is done", 60*time.Second, func() bool { return w.done(j) })
	if got := w.gh.Job(j.ID); got.Conclusion != "success" {
		t.Fatalf("job %s; the hook should have let it run, with /work", got.Conclusion)
	}
	w.eventually("the supervisor reports", 30*time.Second, func() bool {
		_, _, ok := w.gcs.Object("state", "machines/"+m.Name+"/report.json")
		return ok
	})
	rep, _, _ := w.gcs.Object("state", "machines/"+m.Name+"/report.json")
	if !strings.Contains(string(rep), `"save":true`) {
		t.Fatalf("report = %s", rep)
	}
	// The report starts a pass at once, which deletes the machine.
	w.eventually("the reported machine is deleted", 30*time.Second, func() bool {
		_, ok := w.gce.Instance(m.Zone, m.Name)
		return !ok
	})
	sudo, _ := os.ReadFile(filepath.Join(w.guest(m.Name).dir, "sudo.log"))
	if !strings.Contains(string(sudo), "mkfs.ext4") || !strings.Contains(string(sudo), "mount -o discard,noatime") {
		t.Fatalf("the hook did %q", sudo)
	}
	if n := len(w.gce.Disks()); n != 0 {
		t.Fatalf("%d disks left", n)
	}
	log, _, ok := w.gcs.Object("state", "logs/"+m.Name+"-"+m.Labels[machines.LabelCreated]+".txt")
	if !ok || !strings.Contains(string(log), "the runner exited") || !strings.Contains(string(log), "Running job: build") {
		t.Fatalf("boot log = %q", log)
	}
	if !strings.Contains(w.logs.String(), "saving isn't built yet") {
		t.Fatal("the save the job asked for wasn't considered")
	}
	w.noCredentials()
}

func TestAForksJobHandedToAMachine(t *testing.T) {
	w := newWorld(t)
	main := w.gh.PushRun(w.repo, "main")
	w.bind(main, ghemu.Job{}, binding.Request{})
	// A fork's pull request names main's selector, and a maintainer
	// approved its run; its jobs are queued first, so GitHub hands one out
	// first, to the machine made for main's job.
	w.fork.Commit("evil", map[string]string{"x": "1"})
	p := w.gh.PullRequest(w.repo, "main", w.fork, "evil", w.contributor)
	forkRun := w.gh.PullRequestRun(p)
	if scope := w.bind(forkRun, ghemu.Job{}, binding.Request{}); scope != "read" {
		t.Fatalf("a fork's run bound %s", scope)
	}
	stray := w.gh.Queue(forkRun, ghemu.JobSpec{Name: "steal", Labels: []string{sel}, Steps: "touch /tmp/should-never-run-$$"})
	second := w.gh.Queue(forkRun, ghemu.JobSpec{Name: "steal-too", Labels: []string{sel}})
	meant := w.gh.Queue(main, ghemu.JobSpec{Name: "build", Labels: []string{sel}})
	w.pass()
	ms := w.ours()
	if len(ms) != 1 {
		t.Fatalf("%d machines; only main's job gets one", len(ms))
	}
	first := ms[0]
	w.eventually("the refused machine stops", 60*time.Second, func() bool {
		in, ok := w.gce.Instance(first.Zone, first.Name)
		return ok && in.Status == "TERMINATED"
	})
	adm, _, _ := w.gcs.Object("state", "machines/"+first.Name+"/admission.json")
	if !strings.Contains(string(adm), `"allowed":false`) || !strings.Contains(string(adm), strconv.FormatInt(stray.ID, 10)) {
		t.Fatalf("admission = %s", adm)
	}
	if in, _ := w.gce.Instance(first.Zone, first.Name); len(in.Disks) != 1 {
		t.Fatalf("a refused job's machine has disks %v", in.Disks)
	}
	if _, _, ok := w.gcs.Object("state", "machines/"+first.Name+"/report.json"); ok {
		t.Fatal("a refused machine's supervisor reported")
	}
	w.eventually("the fork's run is cancelled", 30*time.Second, func() bool { return w.gh.Job(second.ID).Conclusion == "cancelled" })
	// Until GitHub shows the run finished, no machine for its selector.
	w.pass()
	if n := len(w.ours()); n != 0 {
		t.Fatalf("%d machines while the fork's run is unfinished", n)
	}
	// Its refused job's runner is gone, so it takes force; here, sooner
	// than the service would.
	repo, _ := w.client.RepositoryByID(context.Background(), w.repo.ID)
	if err := w.client.ForceCancelRun(context.Background(), repo, forkRun.ID); err != nil {
		t.Fatal(err)
	}
	w.pass()
	w.pass()
	w.eventually("main's job runs on a machine of its own", 60*time.Second, func() bool { return w.done(meant) })
	if c := w.gh.Job(meant.ID).Conclusion; c != "success" {
		t.Fatalf("main's job: %s", c)
	}
	w.noCredentials()
}

func TestAnApprovedContribution(t *testing.T) {
	w := newWorld(t)
	w.fork.Commit("feature", map[string]string{"src.go": "package app"})
	p := w.gh.PullRequest(w.repo, "main", w.fork, "feature", w.contributor)
	run := w.gh.PullRequestTargetRun(p)
	ev := binding.Request{PullRequest: p.Number, HeadRepositoryID: run.EventPull.HeadRepositoryID, HeadSHA: run.EventPull.HeadSHA}
	scope := w.bind(run, ghemu.Job{WorkflowRef: bootstrap}, ev)
	if scope != "pr"+strconv.Itoa(p.Number) {
		t.Fatalf("the contribution bound %s", scope)
	}
	j := w.gh.Queue(run, ghemu.JobSpec{Name: "test", Labels: []string{"lanescape-2vcpu-linux-amd64-" + scope},
		Environment: machines.ContributionEnvironment})
	w.pass()
	if n := len(w.ours()); n != 0 {
		t.Fatal("a machine for a contribution that's waiting for approval")
	}
	w.gh.Approve(run, machines.ContributionEnvironment, w.gh.User(42, "maintainer"))
	w.pass()
	w.eventually("the contribution's job runs", 60*time.Second, func() bool { return w.done(j) })
	if c := w.gh.Job(j.ID).Conclusion; c != "success" {
		t.Fatalf("contribution's job: %s", c)
	}
}

func TestTheHookWaitsForAnAnswer(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	w.bind(run, ghemu.Job{}, binding.Request{})
	// GitHub can't say which job is on the runner for a while.
	w.gh.ServerError("/repos/lanescape/app/actions/runs/", http.StatusBadGateway, 0)
	j := w.gh.Queue(run, ghemu.JobSpec{Name: "build", Labels: []string{sel}})
	// The pass reads the queue before GitHub fails.
	w.gh.Faults().Reset()
	w.pass()
	w.gh.ServerError("/repos/lanescape/app/actions/", http.StatusBadGateway, 0)
	m := w.ours()[0]
	w.eventually("the job is on the machine's runner", 30*time.Second, func() bool { return w.gh.Job(j.ID).Status == "in_progress" })
	time.Sleep(3 * time.Second)
	if w.done(j) {
		t.Fatal("the job ran with no answer from the service")
	}
	if in, _ := w.gce.Instance(m.Zone, m.Name); len(in.Disks) != 1 {
		t.Fatal("a work disk with no answer")
	}
	w.gh.Faults().Reset()
	w.eventually("the job runs once GitHub answers", 90*time.Second, func() bool { return w.done(j) })
	if c := w.gh.Job(j.ID).Conclusion; c != "success" {
		t.Fatalf("job: %s", c)
	}
}
