// Package servicetest wires the real service, its run binding, cache,
// disks and webhook, to this repository's emulators of GitHub, Google's
// issuers, IAM, Pub/Sub, Compute Engine and Cloud Storage, as a
// service.World: what the conformance suite and the cache's own tests
// drive. GitHub's deliveries reach the service as they do in production:
// signed to its webhook, published to Pub/Sub, and pushed to its reconcile
// endpoint, signed as the push account.
package servicetest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
	"github.com/lanescape/runners/internal/cache"
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
	signer  = "lanescape-runners-service@lanescape-dev.iam.gserviceaccount.com"
	topic   = "projects/lanescape-dev/topics/lanescape-runners-webhooks"
	secret  = "webhook secret"
	State   = "state"
	Cache   = "cache"
	project = "lanescape-dev"
)

// World is the real service, wired to the emulators.
type World struct {
	T       testing.TB
	GitHub  *ghemu.Emulator
	Storage *gcsemu.Emulator
	PubSub  *google.PubSub
	Google  *google.Issuer
	IAM     *google.IAM
	Compute *gceemu.Emulator
	Org     *ghemu.Account
	Inst    *ghemu.Installation
	Objects store.Objects
	Cache   *cache.Service
	Disks   *disks.Service
	Passes  *pass.Runner
	Body    pass.Body
	Logs    *Logs

	url        string
	maintainer *ghemu.Account
	clock      clockwork.Clock

	mu        sync.Mutex
	nextID    int64
	repos     map[int64]*ghemu.Repo
	pulls     map[string]*ghemu.Pull
	runs      map[int64]*ghemu.Run
	jobs      map[int64]*ghemu.WorkflowJob
	onMachine map[int64]string
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
// the service's clock; nil is real time, which Advance moves on.
func New(t testing.TB, clock clockwork.Clock) *World {
	t.Helper()
	ctx := context.Background()
	if clock == nil {
		clock = &offsetClock{Clock: clockwork.NewRealClock()}
	}
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket(State)
	gcs.CreateBucket(Cache)
	gcs.AddLifecycleRule(Cache, "uploads/", 1)
	iam := google.NewIAM()
	t.Cleanup(iam.Close)
	gcs.TrustSigner(signer, iam.AddAccount(signer))
	ps := google.NewPubSub()
	t.Cleanup(ps.Close)
	ps.CreateTopic(topic)
	gg := google.New()
	t.Cleanup(gg.Close)
	gce := gceemu.New(gceemu.WithClock(clock))
	t.Cleanup(gce.Close)

	org := gh.Org(orgID, "lanescape")
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
	blobs, err := store.NewGCS(ctx, Cache, gcs.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	gcec, err := compute.New(ctx, gce.Project, gce.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := pubsub.New(ctx, topic, ps.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := cache.NewIAMSigner(ctx, Cache, signer, iam.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(gcs.URL)
	sg.Hostname, sg.Insecure = u.Host, true

	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	audience := srv.URL
	callers, err := auth.NewGoogle(auth.Options{Issuer: auth.Issuer{URL: gg.URL, JWKSURL: gg.JWKSURL}, Audience: audience}, []string{pusher})
	if err != nil {
		t.Fatal(err)
	}
	// GitHub's tokens are minted and checked in real time, whatever the
	// service's clock says.
	actions, err := auth.NewActions(auth.Options{Issuer: auth.Issuer{URL: gh.Actions.URL, JWKSURL: gh.Actions.JWKSURL}, Audience: audience}, orgID)
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
	cch := &cache.Service{OrganizationID: orgID, GitHub: ghc, Objects: objects, Bucket: blobs, Signer: sg, Actions: actions,
		Binder: binder, Disks: dks, Clock: clock, Logger: log}
	dks.ScopesHeld = cch.ScopesHeld
	passes := &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock, Logger: log}
	body := pass.All(dks.Pass, cch.Pass)
	handler, err = server.New(server.Config{
		Logger: log, OrganizationID: orgID, WebhookSecret: []byte(secret), Publisher: pub, ReconcileCallers: callers,
		Passes: passes, Reconciler: reconciler(body), Events: server.Recorders{dks, cch}, Actions: actions, Binder: binder,
		Cache: cch.Handler(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &World{T: t, GitHub: gh, Storage: gcs, PubSub: ps, Google: gg, IAM: iam, Compute: gce, Org: org, Inst: inst,
		Objects: objects, Cache: cch, Disks: dks, Passes: passes, Body: body, Logs: logs, url: audience, clock: clock,
		maintainer: gh.User(42, "maintainer"), nextID: 5000, repos: map[int64]*ghemu.Repo{}, pulls: map[string]*ghemu.Pull{},
		runs: map[int64]*ghemu.Run{}, jobs: map[int64]*ghemu.WorkflowJob{}, onMachine: map[int64]string{}}
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

// SetRunners implements service.World.
func (w *World) SetRunners(r service.Repository, file string) {
	w.repo(r).Commit("main", map[string]string{binding.RunnersFile: file})
}

// Bootstrap implements service.World.
func (w *World) Bootstrap(r service.Repository) string {
	return r.Name + "/.github/workflows/bootstrap.yml@refs/heads/main"
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
	rp.Commit(head, map[string]string{"head": head})
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

// PushPullRequest implements service.World: a commit on the pull request's
// head branch, in its repository or fork, which GitHub's pull request then
// follows, as a push to that branch makes it.
func (w *World) PushPullRequest(pr service.PullRequest) service.PullRequest {
	p := w.pull(pr)
	p.Head.Commit(p.HeadRef, map[string]string{"pushed": strconv.FormatInt(w.id(), 10)})
	w.GitHub.Sync(p)
	pr.HeadSHA = w.GitHub.PullHead(p)
	return pr
}

// ClosePullRequest implements service.World.
func (w *World) ClosePullRequest(pr service.PullRequest) {
	p := w.pull(pr)
	w.GitHub.ClosePull(p)
	w.deliver("pull_request", w.GitHub.PullRequestPayload(p, w.Inst, "closed"))
}

// ReopenPullRequest implements service.World.
func (w *World) ReopenPullRequest(pr service.PullRequest) { w.GitHub.ReopenPull(w.pull(pr)) }

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

// Rerun implements service.World.
func (w *World) Rerun(r service.Run) service.Run {
	gr := w.run(r)
	w.GitHub.Rerun(gr)
	r.Attempt = gr.Attempt
	return r
}

// Approve implements service.World.
func (w *World) Approve(r service.Run) {
	w.GitHub.Approve(w.run(r), machines.ContributionEnvironment, w.maintainer)
}

// StartJob implements service.World.
func (w *World) StartJob(r service.Run, workflowRef string) service.Job {
	wj := w.GitHub.Queue(w.run(r), ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	w.GitHub.RunJob(wj)
	w.mu.Lock()
	w.jobs[wj.ID] = wj
	w.mu.Unlock()
	r.Attempt = wj.Attempt
	return service.Job{Run: r, ID: wj.ID, WorkflowRef: workflowRef}
}

// StartMachineJob implements service.World: GitHub shows the job on one
// of the service's machines, by its runner's name, and the machine's
// job-started check has admitted it, as its record says.
func (w *World) StartMachineJob(r service.Run) service.Job {
	wj := w.GitHub.Queue(w.run(r), ghemu.JobSpec{Labels: []string{"self-hosted"}})
	name := fmt.Sprintf("lsr-%d-%08x", r.Repository.ID, w.id())
	w.GitHub.RunJobOn(wj, name)
	data, err := json.Marshal(machines.Admission{Machine: name, Zone: "northamerica-northeast1-a", RepositoryID: r.Repository.ID,
		JobID: wj.ID, RunID: r.ID, RunAttempt: wj.Attempt, Allowed: true, At: time.Now().UTC()})
	if err != nil {
		w.T.Fatal(err)
	}
	w.Storage.Put(State, machines.AdmissionKey(name), data)
	w.mu.Lock()
	w.jobs[wj.ID] = wj
	w.onMachine[wj.ID] = name
	w.mu.Unlock()
	r.Attempt = wj.Attempt
	return service.Job{Run: r, ID: wj.ID}
}

// WorkflowJob is the GitHub emulator's job behind a Job.
func (w *World) WorkflowJob(j service.Job) *ghemu.WorkflowJob {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.jobs[j.ID]
}

// Token implements service.World.
func (w *World) Token(j service.Job, overrides map[string]any) string {
	return w.GitHub.JobToken(w.WorkflowJob(j), ghemu.Job{Audience: w.url, WorkflowRef: j.WorkflowRef}, overrides)
}

// FinishJob implements service.World. A job on one of the service's
// machines has its machine's report recorded, as its supervisor sends it.
func (w *World) FinishJob(j service.Job, conclusion string) {
	wj := w.WorkflowJob(j)
	w.GitHub.CompleteJob(wj, conclusion)
	w.mu.Lock()
	name := w.onMachine[j.ID]
	w.mu.Unlock()
	if name != "" {
		data, _ := json.Marshal(machines.Report{Machine: name, At: time.Now().UTC()})
		w.Storage.Put(State, "machines/"+name+"/report.json", data)
	}
	w.deliver("workflow_job", w.GitHub.WorkflowJobPayload(wj.Run, w.Inst, "completed", wj.ID, wj.Labels))
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

// FinishRun finishes every job of a run still going, as FinishJob does.
func (w *World) FinishRun(r service.Run, conclusion string) {
	w.mu.Lock()
	var jobs []int64
	for id, wj := range w.jobs {
		if wj.Run.ID == r.ID {
			jobs = append(jobs, id)
		}
	}
	w.mu.Unlock()
	slices.Sort(jobs)
	for _, id := range jobs {
		if w.GitHub.Job(id).Status != "completed" {
			w.FinishJob(service.Job{Run: r, ID: id}, conclusion)
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

// Advance implements service.World: the service's clock moves on by d.
// Cloud Storage's emulator, and the signing of URLs, keep real time, so a
// test that moves time on doesn't rely on a URL's expiry across it.
func (w *World) Advance(d time.Duration) {
	w.T.Helper()
	switch c := w.clock.(type) {
	case *clockwork.FakeClock:
		c.Advance(d)
	case *offsetClock:
		c.advance(d)
	default:
		w.T.Fatal("the service's clock can't be moved on")
	}
}

// offsetClock is real time, moved on by Advance: time still passes between
// a test's steps, as it does for the service.
type offsetClock struct {
	clockwork.Clock
	mu     sync.Mutex
	offset time.Duration
}

func (c *offsetClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

func (c *offsetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Clock.Now().Add(c.offset)
}

func (c *offsetClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }
func (c *offsetClock) Until(t time.Time) time.Duration { return t.Sub(c.Now()) }

// Pass implements service.World: one pass, the disks' part and the
// cache's.
func (w *World) Pass() {
	w.T.Helper()
	if err := w.Passes.Run(context.Background(), w.Body); err != nil {
		w.T.Fatalf("pass: %v", err)
	}
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

func (w *World) records(prefix string, into func([]byte)) {
	for _, name := range w.Storage.Names(State) {
		if strings.HasPrefix(name, prefix) {
			data, _, ok := w.Storage.Object(State, name)
			if ok {
				into(data)
			}
		}
	}
}

// Entries implements service.World.
func (w *World) Entries(r service.Repository) []service.Entry {
	var out []service.Entry
	w.records(fmt.Sprintf("cache/entries/%d/", r.ID), func(data []byte) {
		var e cache.Entry
		if json.Unmarshal(data, &e) == nil && e.State != cache.Marked {
			out = append(out, service.Entry{ID: e.ID, Scope: e.Scope, Store: e.Store, Platform: e.Platform, Family: e.Family, Key: e.Key,
				State: e.State, Size: e.Size, SHA256: e.SHA256})
		}
	})
	return out
}

// Choices implements service.World.
func (w *World) Choices(r service.Run) []service.Choice {
	var out []service.Choice
	w.records(fmt.Sprintf("cache/choices/%d/%d/", r.Repository.ID, r.ID), func(data []byte) {
		var c cache.Choice
		if json.Unmarshal(data, &c) == nil {
			out = append(out, service.Choice{Store: c.Store, Platform: c.Platform, Family: c.Family, Key: c.Key, Step: c.Step, Entry: c.Entry})
		}
	})
	return out
}
