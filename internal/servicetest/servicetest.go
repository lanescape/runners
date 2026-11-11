// Package servicetest wires the real service, its machines, disks and
// webhook, to this repository's emulators of GitHub, Google's issuers,
// Pub/Sub, Compute Engine and Cloud Storage, as a service.World: what the
// conformance suite drives. GitHub's deliveries reach the service as they
// do in production: signed to its webhook, published to Pub/Sub, and pushed
// to its reconcile endpoint, signed as the push account. A job is played to
// its check as the service's machines play it: a pass makes its machine,
// a runner stand-in takes the job, and the hook's question goes to the
// job-started check with what GitHub told the runner.
package servicetest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/google"
	"github.com/lanescape/runners/emulators/service"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/pubsub"
	"github.com/lanescape/runners/internal/server"
	"github.com/lanescape/runners/internal/store"
)

const (
	orgID   = 303994381
	pusher  = "lanescape-runners-push@lanescape-dev.iam.gserviceaccount.com"
	topic   = "projects/lanescape-dev/topics/lanescape-runners-webhooks"
	secret  = "webhook secret"
	State   = "state"
	project = "lanescape-dev"
)

// World is the real service, wired to the emulators.
type World struct {
	T        testing.TB
	GitHub   *ghemu.Emulator
	Storage  *gcsemu.Emulator
	PubSub   *google.PubSub
	Google   *google.Issuer
	Compute  *gceemu.Emulator
	Org      *ghemu.Account
	Inst     *ghemu.Installation
	Objects  store.Objects
	Disks    *disks.Service
	Machines *machines.Service
	Passes   *pass.Runner
	Body     pass.Body
	Logs     *Logs

	url        string
	maintainer *ghemu.Account

	mu        sync.Mutex
	nextID    int64
	repos     map[int64]*ghemu.Repo
	pulls     map[string]*ghemu.Pull
	runs      map[int64]*ghemu.Run
	jobs      map[int64]*ghemu.WorkflowJob
	machineOf map[int64]gceemu.InstanceState
	claims    map[int64]map[string]any
	stops     []context.CancelFunc
	published int
}

// Logs is what the service logged, for tests to search.
type Logs struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *Logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// New starts the service and the emulators, for one test, with clock as
// the service's clock; nil is real time.
func New(t testing.TB, clock clockwork.Clock) *World {
	t.Helper()
	ctx := context.Background()
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket(State)
	ps := google.NewPubSub()
	t.Cleanup(ps.Close)
	ps.CreateTopic(topic)
	gg := google.New()
	t.Cleanup(gg.Close)
	gce := gceemu.New(gceemu.WithClock(clock))
	t.Cleanup(gce.Close)
	gce.AddImage("lsr-base-amd64-1", "", "X86_64")
	gce.AddImage("lsr-base-arm64-1", "", "ARM64")

	org := gh.Org(orgID, "lanescape")
	gh.SetForkApproval(org, ghemu.ApproveAllExternalContributors)
	app := gh.NewApp(77)
	inst := gh.Install(app, org)
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	ghc, err := github.NewAppClient(github.Options{BaseURL: gh.URL, AppID: app.ID, PrivateKey: app.PrivateKeyPEM(), OrganizationID: orgID})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.NewGCS(ctx, State, gcs.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objects.Close() })
	gcec, err := compute.New(ctx, gce.Project, gce.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := pubsub.New(ctx, topic, ps.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}

	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	audience := srv.URL
	callers, err := auth.NewGoogle(auth.Options{Issuer: auth.Issuer{URL: gg.URL, JWKSURL: gg.JWKSURL}, Audience: audience}, []string{pusher})
	if err != nil {
		t.Fatal(err)
	}
	account := machines.MachineAccountOf(gce.Project)
	verifier, err := auth.NewMachines(auth.Options{Issuer: auth.Issuer{URL: gce.Identity.URL, JWKSURL: gce.Identity.JWKSURL},
		Audience: audience, Now: clock.Now}, gce.Project, account)
	if err != nil {
		t.Fatal(err)
	}
	logs := &Logs{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	binder := &binding.Binder{GitHub: ghc, Objects: objects, OrganizationID: orgID, Clock: clock, Logger: log}
	dks := &disks.Service{Config: cfg, Project: project, GitHub: ghc, Compute: gcec, Objects: objects, Clock: clock, Logger: log,
		StepDeadline: 5 * time.Second}
	svc := &machines.Service{Config: cfg, Project: gce.Project, ServiceURL: audience, MachineAccount: account, GitHub: ghc, Compute: gcec,
		Objects: objects, Binder: binder, Machines: verifier, Clock: clock, Logger: log, AssignmentWait: 5 * time.Second,
		AssignmentRetry: 50 * time.Millisecond, FailStopDelay: 100 * time.Millisecond, Disks: dks, Saves: dks, Preemptions: dks}
	passes := &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock, Logger: log}
	body := pass.All(svc.Pass, dks.Pass)
	handler, err = server.New(server.Config{
		Logger: log, OrganizationID: orgID, WebhookSecret: []byte(secret), Publisher: pub, ReconcileCallers: callers,
		Passes: passes, Reconciler: reconciler(body), Events: dks, JobStarted: svc.JobStarted(), JobFinished: svc.JobFinished(),
		Disks: svc.DiskRequest(), Save: svc.SaveRequest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &World{T: t, GitHub: gh, Storage: gcs, PubSub: ps, Google: gg, Compute: gce, Org: org, Inst: inst,
		Objects: objects, Disks: dks, Machines: svc, Passes: passes, Body: body, Logs: logs, url: audience,
		maintainer: gh.User(42, "maintainer"), nextID: 5000, repos: map[int64]*ghemu.Repo{}, pulls: map[string]*ghemu.Pull{},
		runs: map[int64]*ghemu.Run{}, jobs: map[int64]*ghemu.WorkflowJob{}, machineOf: map[int64]gceemu.InstanceState{},
		claims: map[int64]map[string]any{}}
	t.Cleanup(func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, s := range w.stops {
			s()
		}
	})
	return w
}

type reconciler pass.Body

func (r reconciler) Pass(p *pass.Pass) error { return r(p) }

var _ service.World = (*World)(nil)

// URL implements service.World.
func (w *World) URL() string { return w.url }

// Organization implements service.World.
func (w *World) Organization() int64 { return orgID }

func (w *World) id() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nextID++
	return w.nextID
}

func (w *World) repo(r service.Repository) *ghemu.Repo {
	w.mu.Lock()
	defer w.mu.Unlock()
	rp := w.repos[r.ID]
	if rp == nil {
		w.T.Fatalf("no repository %d", r.ID)
	}
	return rp
}

// Repository implements service.World.
func (w *World) Repository(name string) service.Repository {
	id := w.id()
	rp := w.GitHub.Repo(w.Org, id, name)
	w.mu.Lock()
	w.repos[id] = rp
	w.mu.Unlock()
	return service.Repository{ID: id, Name: rp.FullName()}
}

// Commit implements service.World.
func (w *World) Commit(r service.Repository, branch string, files map[string]string) {
	w.repo(r).Commit(branch, files)
}

// Branch implements service.World.
func (w *World) Branch(r service.Repository, name string) {
	w.repo(r).Commit(name, map[string]string{"branch": name})
}

// DeleteBranch implements service.World.
func (w *World) DeleteBranch(r service.Repository, name string) { w.repo(r).DeleteBranch(name) }

func (w *World) addPull(p *ghemu.Pull) service.PullRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pulls[fmt.Sprintf("%d/%d", p.Base.ID, p.Number)] = p
	return service.PullRequest{Repository: service.Repository{ID: p.Base.ID, Name: p.Base.FullName()}, Number: p.Number,
		HeadRepositoryID: p.Head.ID, HeadSHA: p.HeadSHA}
}

func (w *World) pull(pr service.PullRequest) *ghemu.Pull {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pulls[fmt.Sprintf("%d/%d", pr.Repository.ID, pr.Number)]
}

// PullRequest implements service.World.
func (w *World) PullRequest(r service.Repository, base, head string) service.PullRequest {
	rp := w.repo(r)
	if rp.Head(head) == "" {
		rp.Commit(head, map[string]string{"head": head})
	}
	return w.addPull(w.GitHub.PullRequest(rp, base, rp, head, w.maintainer))
}

// ForkPullRequest implements service.World.
func (w *World) ForkPullRequest(r service.Repository, base string) service.PullRequest {
	rp := w.repo(r)
	id := w.id()
	contributor := w.GitHub.User(id, "contributor"+strconv.FormatInt(id, 10))
	fork := w.GitHub.Fork(rp, contributor, w.id())
	fork.Commit("feature", map[string]string{"src.go": "package app"})
	return w.addPull(w.GitHub.PullRequest(rp, base, fork, "feature", contributor))
}

// PushPullRequest implements service.World.
func (w *World) PushPullRequest(pr service.PullRequest, files map[string]string) service.PullRequest {
	p := w.pull(pr)
	if len(files) == 0 {
		files = map[string]string{"pushed": strconv.FormatInt(w.id(), 10)}
	}
	p.Head.Commit(p.HeadRef, files)
	w.GitHub.Sync(p)
	pr.HeadSHA = w.GitHub.PullHead(p)
	return pr
}

func (w *World) addRun(r *ghemu.Run, pr *service.PullRequest) service.Run {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.runs[r.ID] = r
	return service.Run{Repository: service.Repository{ID: r.Repo.ID, Name: r.Repo.FullName()}, ID: r.ID, Attempt: r.Attempt,
		Event: r.Event, PullRequest: pr}
}

func (w *World) run(r service.Run) *ghemu.Run {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runs[r.ID]
}

// Push implements service.World.
func (w *World) Push(r service.Repository, branch string) service.Run {
	return w.addRun(w.GitHub.PushRun(w.repo(r), branch), nil)
}

// Tag implements service.World.
func (w *World) Tag(r service.Repository, tag string) service.Run {
	rp := w.repo(r)
	rp.Tag(tag, rp.Head("main"))
	return w.addRun(w.GitHub.TagRun(rp, tag), nil)
}

// Event implements service.World.
func (w *World) Event(r service.Repository, event, branch string) service.Run {
	return w.addRun(w.GitHub.EventRun(w.repo(r), event, branch), nil)
}

// PullRequestRun implements service.World.
func (w *World) PullRequestRun(pr service.PullRequest) service.Run {
	return w.addRun(w.GitHub.PullRequestRun(w.pull(pr)), &pr)
}

// PullRequestTargetRun implements service.World.
func (w *World) PullRequestTargetRun(pr service.PullRequest) service.Run {
	return w.addRun(w.GitHub.PullRequestTargetRun(w.pull(pr)), &pr)
}

// Approve implements service.World.
func (w *World) Approve(r service.Run) {
	w.GitHub.Approve(w.run(r), binding.ContributionEnvironment, w.maintainer)
}

// Rerun implements service.World.
func (w *World) Rerun(r service.Run) service.Run {
	gr := w.run(r)
	w.GitHub.RerunAs(gr, w.maintainer)
	r.Attempt = gr.Attempt
	return r
}

// Pass runs one pass of the service's.
func (w *World) Pass() {
	w.T.Helper()
	if err := w.Passes.Run(context.Background(), w.Body); err != nil {
		w.T.Fatalf("pass: %v", err)
	}
}

// facts are what GitHub tells a run's job's runner.
func facts(gr *ghemu.Run) binding.Facts {
	f := binding.Facts{Event: gr.Event, Ref: gr.Ref, RefType: gr.RefType, SHA: gr.SHA}
	if p := gr.EventPull; p != nil {
		f.PullRequest, f.HeadRepositoryID, f.HeadSHA = p.Number, p.HeadRepositoryID, p.HeadSHA
	}
	return f
}

// StartJob implements service.World: a pass makes the job's machine, a
// runner stand-in on it takes the job, and its hook asks the job-started
// check.
func (w *World) StartJob(r service.Run, selector string) (service.Job, error) {
	w.T.Helper()
	wj := w.queue(r, selector)
	w.Pass()
	in, ok := w.machineFor(wj)
	if !ok {
		return service.Job{}, w.noMachine(r, wj)
	}
	return w.check(r, wj, in)
}

// StartJobOn implements service.World: a pass makes a machine for a job of
// madeFor, which is cancelled before the machine's runner takes it, so the
// runner takes the job of run queued after it.
func (w *World) StartJobOn(r service.Run, selector string, madeFor service.Run) (service.Job, error) {
	w.T.Helper()
	other := w.queue(madeFor, selector)
	w.Pass()
	in, ok := w.machineFor(other)
	if !ok {
		return service.Job{}, w.noMachine(madeFor, other)
	}
	w.GitHub.CompleteJob(other, "cancelled")
	return w.check(r, w.queue(r, selector), in)
}

// queue queues a job of a run that names a selector.
func (w *World) queue(r service.Run, selector string) *ghemu.WorkflowJob {
	wj := w.GitHub.Queue(w.run(r), ghemu.JobSpec{Labels: []string{selector}})
	w.mu.Lock()
	w.jobs[wj.ID] = wj
	w.mu.Unlock()
	return wj
}

// machineFor is the machine the service made for a job, if it made one.
func (w *World) machineFor(wj *ghemu.WorkflowJob) (gceemu.InstanceState, bool) {
	for _, m := range w.Compute.Instances() {
		if m.Labels[machines.LabelJob] == strconv.FormatInt(wj.ID, 10) {
			return m, true
		}
	}
	return gceemu.InstanceState{}, false
}

// noMachine is why a job got no machine: its run's refusal, or no
// confirmed answer.
func (w *World) noMachine(r service.Run, wj *ghemu.WorkflowJob) error {
	if rec, _, ok := w.Storage.Object(State, fmt.Sprintf("refusals/%d/%d-%d.json", r.Repository.ID, r.ID, wj.Attempt)); ok {
		var rf struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(rec, &rf)
		return &service.Refused{Reason: rf.Reason}
	}
	return service.ErrUnavailable
}

// check starts a runner stand-in on a machine, waits until it has taken
// the job, and asks the job-started check as its hook does.
func (w *World) check(r service.Run, wj *ghemu.WorkflowJob, in gceemu.InstanceState) (service.Job, error) {
	w.T.Helper()
	gr := w.run(r)
	dir, err := os.MkdirTemp("", "servicetest-runner-")
	if err != nil {
		w.T.Fatal(err)
	}
	hook := filepath.Join(dir, "hook.sh")
	_ = os.WriteFile(hook, []byte("sleep 3600"), 0o755)
	ctx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.stops = append(w.stops, func() { cancel(); _ = os.RemoveAll(dir) })
	w.mu.Unlock()
	s := ghemu.StandIn{JITConfig: in.Metadata[machines.MetaJITConfig], Hook: hook, Dir: dir, Poll: 100 * time.Millisecond}
	go func() { _ = s.Run(ctx) }()
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		got := w.GitHub.Job(wj.ID)
		if got.Status == "in_progress" && got.Runner != nil && got.Runner.Name == in.Name {
			break
		}
		if time.Now().After(end) {
			w.T.Fatalf("job %d never reached its machine %s", wj.ID, in.Name)
		}
	}
	// The machine's claims are kept, so that its token can still be minted
	// once the service has deleted it.
	claims := w.Compute.IdentityClaims(in.Zone, in.Name, w.url, true)
	b, _ := json.Marshal(machines.StartedRequest{RunID: r.ID, Facts: facts(gr)})
	req, _ := http.NewRequest(http.MethodPost, w.url+server.JobStartedPath, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+w.Compute.Identity.Mint(claims))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.T.Fatal(err)
	}
	defer resp.Body.Close()
	var ans struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ans)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		if ans.Reason == "" {
			ans.Reason = "the job-started check refused the job"
		}
		return service.Job{}, &service.Refused{Reason: ans.Reason}
	default:
		return service.Job{}, service.ErrUnavailable
	}
	w.mu.Lock()
	w.machineOf[wj.ID] = in
	w.claims[wj.ID] = claims
	w.mu.Unlock()
	r.Attempt = wj.Attempt
	sel := strings.ToLower(wj.Labels[0])
	return service.Job{Run: r, ID: wj.ID, Selector: sel, Machine: in.Name}, nil
}

// Token implements service.World.
func (w *World) Token(j service.Job) string {
	w.mu.Lock()
	claims := w.claims[j.ID]
	w.mu.Unlock()
	if claims == nil {
		w.T.Fatalf("job %d has no machine", j.ID)
	}
	return w.Compute.Identity.Mint(claims)
}

// WorkflowJob is the GitHub emulator's job behind a Job.
func (w *World) WorkflowJob(j service.Job) *ghemu.WorkflowJob {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.jobs[j.ID]
}

// FinishJob implements service.World: GitHub records the job's conclusion
// and delivers its workflow_job event, the machine's supervisor reports,
// and the machine powers off; passes then save its work disk, if they
// should, and delete it.
func (w *World) FinishJob(j service.Job, conclusion string) {
	w.T.Helper()
	wj := w.WorkflowJob(j)
	w.mu.Lock()
	in, ok := w.machineOf[j.ID]
	w.mu.Unlock()
	if wj == nil || !ok {
		w.T.Fatalf("job %d has no machine", j.ID)
	}
	w.GitHub.CompleteJob(wj, conclusion)
	w.deliver("workflow_job", w.GitHub.WorkflowJobPayload(wj.Run, w.Inst, "completed", wj.ID, wj.Labels))
	if code := w.post(server.JobFinishedPath, w.Token(j), machines.FinishedRequest{}); code != http.StatusNoContent {
		w.T.Fatalf("the supervisor's report: %d", code)
	}
	w.Compute.Shutdown(in.Zone, in.Name)
	w.until(fmt.Sprintf("machine %s is deleted", in.Name), func() bool {
		_, there := w.Compute.Instance(in.Zone, in.Name)
		return !there
	})
}

// post sends a request to the service, signed with token, and returns its
// status.
func (w *World) post(path, token string, body any) int {
	w.T.Helper()
	code, _ := w.call(path, token, body)
	return code
}

func (w *World) call(path, token string, body any) (int, []byte) {
	w.T.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, w.url+path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.T.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// until runs passes until cond holds.
func (w *World) until(what string, cond func() bool) {
	w.T.Helper()
	for range 50 {
		if cond() {
			return
		}
		w.Pass()
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		w.T.Fatalf("passes never made it so that %s", what)
	}
}

// HoldDisks implements service.World: while on, Compute Engine fails to
// attach a disk, as when it's short of capacity, so the service answers a
// work disk request making, and carries on at the next.
func (w *World) HoldDisks(on bool) {
	w.Compute.Faults().Reset()
	if on {
		w.Compute.Faults().Add(fault.Rule{Method: http.MethodPost, Match: func(r *http.Request) bool {
			return strings.HasSuffix(r.URL.Path, "/attachDisk")
		}, Respond: fault.Status(http.StatusServiceUnavailable, nil, `{"error": {"code": 503, "message": "Service Unavailable"}}`)})
	}
}

// deliver sends GitHub's signed delivery to the service's webhook, and
// pushes what it published to the reconcile endpoint, signed as the push
// account, as Pub/Sub does.
func (w *World) deliver(event string, payload map[string]any) {
	w.T.Helper()
	ctx := context.Background()
	resp, err := ghemu.NewDelivery(w.url+server.WebhookPath, []byte(secret), event, payload).Send(ctx)
	if err != nil {
		w.T.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		w.T.Fatalf("the webhook answered %s's delivery %d", event, resp.StatusCode)
	}
	w.PushMessages()
}

// PushMessages delivers every message published since the last push to the
// reconcile endpoint, again while it answers that it should be retried.
func (w *World) PushMessages() {
	w.T.Helper()
	msgs := w.PubSub.Messages(topic)
	w.mu.Lock()
	todo := msgs[w.published:]
	w.published = len(msgs)
	w.mu.Unlock()
	for _, m := range todo {
		body := google.PushWith(project, "lanescape-runners-webhooks-reconcile", m.ID, m.Data, m.Attributes)
		for attempt := 0; ; attempt++ {
			req, _ := http.NewRequest(http.MethodPost, w.url+server.ReconcilePath, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+w.Google.Token(pusher, w.url))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				w.T.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				break
			}
			if resp.StatusCode != http.StatusServiceUnavailable || attempt == 50 {
				w.T.Fatalf("the reconcile endpoint answered a push %d", resp.StatusCode)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// Unavailable implements service.World: every call to GitHub's API answers
// 503 while it's on.
func (w *World) Unavailable(on bool) {
	if !on {
		w.GitHub.Faults().Reset()
		return
	}
	w.GitHub.Faults().Add(fault.Rule{Respond: fault.Status(http.StatusServiceUnavailable, nil, `{"message": "Service Unavailable"}`)})
}

// Binding implements service.World.
func (w *World) Binding(r service.Run) (string, bool) {
	data, _, ok := w.Storage.Object(State, binding.Key(r.Repository.ID, r.ID))
	if !ok {
		return "", false
	}
	var b binding.Binding
	if err := json.Unmarshal(data, &b); err != nil {
		w.T.Fatal(err)
	}
	return b.Scope.String(), true
}
