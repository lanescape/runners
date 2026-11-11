package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/google"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/server"
	"github.com/lanescape/runners/internal/store"
)

const (
	orgID     = 303994381
	audience  = "https://runners.example.test"
	scheduler = "scheduler@lanescape-dev.iam.gserviceaccount.com"
	pusher    = "push@lanescape-dev.iam.gserviceaccount.com"
	bootstrap = "lanescape/app/.github/workflows/bootstrap.yml@refs/heads/main"
)

var secret = []byte("webhook secret")

type publisher struct {
	mu   sync.Mutex
	got  []server.Delivery
	fail bool
}

func (p *publisher) Publish(_ context.Context, d server.Delivery) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("pubsub down")
	}
	p.got = append(p.got, d)
	return nil
}

func (p *publisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.got)
}

type reconciler struct {
	mu     sync.Mutex
	passes int
	block  chan struct{}
}

func (r *reconciler) Pass(p *pass.Pass) error {
	r.mu.Lock()
	r.passes++
	block := r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-p.Context().Done():
		}
	}
	return nil
}

// events records pushed messages' data, as the service's event recorder
// does, failing while fail is set.
type events struct {
	mu    sync.Mutex
	got   [][]byte
	attrs []map[string]string
	fail  bool
}

func (ev *events) RecordEvent(_ context.Context, attrs map[string]string, data []byte) error {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if ev.fail {
		return errors.New("bucket down")
	}
	ev.got = append(ev.got, data)
	ev.attrs = append(ev.attrs, attrs)
	return nil
}

func (ev *events) count() int {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return len(ev.got)
}

type env struct {
	t     *testing.T
	ev    *events
	gh    *ghemu.Emulator
	gg    *google.Issuer
	gcs   *gcsemu.Emulator
	org   *ghemu.Account
	repo  *ghemu.Repo
	inst  *ghemu.Installation
	pub   *publisher
	rec   *reconciler
	url   string
	logs  *bytes.Buffer
	logMu *sync.Mutex
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

func setup(t *testing.T) *env { return setupPass(t, 0) }

// setupPass sets the passes' deadline; 0 is the spec's.
func setupPass(t *testing.T, deadline time.Duration) *env {
	t.Helper()
	ctx := context.Background()
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	gg := google.New()
	t.Cleanup(gg.Close)
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket("state")

	org := gh.Org(orgID, "lanescape")
	app := gh.NewApp(77)
	inst := gh.Install(app, org)
	repo := gh.Repo(org, 5001, "app")

	objects, err := store.NewGCS(ctx, "state", gcs.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objects.Close() })
	callers, err := auth.NewGoogle(auth.Options{Issuer: auth.Issuer{URL: gg.URL, JWKSURL: gg.JWKSURL}, Audience: audience}, []string{scheduler, pusher})
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	logMu := &sync.Mutex{}
	log := slog.New(slog.NewTextHandler(lockedWriter{logMu, logs}, nil))
	clock := clockwork.NewRealClock()
	pub := &publisher{}
	rec := &reconciler{}
	ev := &events{}
	h, err := server.New(server.Config{
		Events:           ev,
		Logger:           log,
		OrganizationID:   orgID,
		WebhookSecret:    secret,
		Publisher:        pub,
		ReconcileCallers: callers,
		Passes:           &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock, Logger: log, Deadline: deadline},
		Reconciler:       rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{t: t, ev: ev, gh: gh, gg: gg, gcs: gcs, org: org, repo: repo, inst: inst, pub: pub, rec: rec, url: srv.URL, logs: logs, logMu: logMu}
}

func (e *env) post(path, token, body string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.url+path, strings.NewReader(body))
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

// noSecrets checks that nothing the callers sent reached the logs.
func (e *env) noSecrets(secrets ...string) {
	e.t.Helper()
	e.logMu.Lock()
	defer e.logMu.Unlock()
	for _, s := range secrets {
		if s != "" && strings.Contains(e.logs.String(), s) {
			e.t.Fatal("a credential was logged")
		}
	}
}

func TestWebhook(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	run := e.gh.PushRun(e.repo, "main")
	payload := e.gh.WorkflowJobPayload(run, e.inst, "queued", 1, []string{"lanescape-2vcpu-linux-amd64-main"})
	send := func(d *ghemu.Delivery) int {
		resp, err := d.Send(ctx)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := send(ghemu.NewDelivery(e.url+server.WebhookPath, secret, "workflow_job", payload)); code != http.StatusAccepted || e.pub.count() != 1 {
		t.Fatalf("good delivery: %d, %d published", code, e.pub.count())
	}
	if got := e.pub.got[0]; got.Event != "workflow_job" || got.ID == "" || !bytes.Contains(got.Body, []byte(`"queued"`)) {
		t.Fatalf("published %+v", got)
	}
	// A pull request's closing starts a pass too, merged or not, so its
	// scope's disks go at once.
	closed := map[string]any{"action": "closed", "number": 7, "repository": payload["repository"]}
	if code := send(ghemu.NewDelivery(e.url+server.WebhookPath, secret, "pull_request", closed)); code != http.StatusAccepted || e.pub.count() != 2 {
		t.Fatalf("a pull request's closing: %d, %d published", code, e.pub.count())
	}
	if got := e.pub.got[1]; got.Event != "pull_request" {
		t.Fatalf("published %+v", got)
	}

	for _, tc := range []struct {
		name   string
		d      func() *ghemu.Delivery
		status int
	}{
		{"wrong secret", func() *ghemu.Delivery {
			return ghemu.NewDelivery(e.url+server.WebhookPath, []byte("guess"), "workflow_job", payload)
		}, http.StatusUnauthorized},
		{"unsigned", func() *ghemu.Delivery {
			d := ghemu.NewDelivery(e.url+server.WebhookPath, secret, "workflow_job", payload)
			d.Header.Del("X-Hub-Signature-256")
			return d
		}, http.StatusUnauthorized},
		{"tampered body", func() *ghemu.Delivery {
			d := ghemu.NewDelivery(e.url+server.WebhookPath, secret, "workflow_job", payload)
			d.Body = bytes.Replace(d.Body, []byte("queued"), []byte("in_progress"), 1)
			return d
		}, http.StatusUnauthorized},
		{"too large", func() *ghemu.Delivery {
			big := map[string]any{"padding": strings.Repeat("x", server.DefaultWebhookLimit), "repository": payload["repository"]}
			return ghemu.NewDelivery(e.url+server.WebhookPath, secret, "workflow_job", big)
		}, http.StatusRequestEntityTooLarge},
		{"signed but not JSON", func() *ghemu.Delivery {
			d := ghemu.NewDelivery(e.url+server.WebhookPath, secret, "workflow_job", payload)
			d.Body = []byte("{not json")
			d.Header.Set("X-Hub-Signature-256", ghemu.Sign(secret, d.Body))
			return d
		}, http.StatusBadRequest},
		{"another organization", func() *ghemu.Delivery {
			other := e.gh.Repo(e.gh.Org(1234, "lanescape-lookalike"), 6001, "app")
			p := e.gh.WorkflowJobPayload(e.gh.PushRun(other, "main"), e.inst, "queued", 2, nil)
			return ghemu.NewDelivery(e.url+server.WebhookPath, secret, "workflow_job", p)
		}, http.StatusNoContent},
		{"organization disagrees with the repository", func() *ghemu.Delivery {
			p := e.gh.WorkflowJobPayload(run, e.inst, "queued", 3, nil)
			p["organization"] = map[string]any{"id": 1234, "login": "lanescape"}
			return ghemu.NewDelivery(e.url+server.WebhookPath, secret, "workflow_job", p)
		}, http.StatusNoContent},
		{"no repository", func() *ghemu.Delivery {
			return ghemu.NewDelivery(e.url+server.WebhookPath, secret, "workflow_job", map[string]any{"organization": map[string]any{"id": orgID}})
		}, http.StatusNoContent},
		{"another event", func() *ghemu.Delivery {
			return ghemu.NewDelivery(e.url+server.WebhookPath, secret, "push", payload)
		}, http.StatusNoContent},
		{"a pull request that opens", func() *ghemu.Delivery {
			return ghemu.NewDelivery(e.url+server.WebhookPath, secret, "pull_request",
				map[string]any{"action": "opened", "number": 7, "repository": payload["repository"]})
		}, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := e.pub.count()
			if code := send(tc.d()); code != tc.status {
				t.Fatalf("status = %d, want %d", code, tc.status)
			}
			if e.pub.count() != before {
				t.Fatal("delivery was published")
			}
		})
	}

	t.Run("pubsub down", func(t *testing.T) {
		e.pub.fail = true
		defer func() { e.pub.fail = false }()
		if code := send(ghemu.NewDelivery(e.url+server.WebhookPath, secret, "workflow_run", payload)); code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", code)
		}
	})
	e.noSecrets(string(secret))
}

func TestReconcile(t *testing.T) {
	e := setup(t)
	for _, acct := range []string{scheduler, pusher} {
		tok := e.gg.Token(acct, audience)
		if code, _ := e.post(server.ReconcilePath, tok, `{"message":{}}`); code != http.StatusNoContent {
			t.Fatalf("%s: status %d", acct, code)
		}
		e.noSecrets(tok)
	}
	if e.rec.passes != 2 {
		t.Fatalf("%d passes ran, want 2", e.rec.passes)
	}

	unverified := e.gg.Claims(scheduler, audience)
	unverified["email_verified"] = false
	gh := e.gh.ActionsToken(e.gh.PushRun(e.repo, "main"), ghemu.Job{Audience: audience}, nil)
	for _, tc := range []struct {
		name, token string
		status      int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"another account", e.gg.Token("someone@lanescape-dev.iam.gserviceaccount.com", audience), http.StatusForbidden},
		{"unverified email", e.gg.Mint(unverified), http.StatusForbidden},
		{"wrong audience", e.gg.Token(scheduler, "https://elsewhere.example.test"), http.StatusUnauthorized},
		{"a GitHub token", gh, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := e.post(server.ReconcilePath, tc.token, "{}"); code != tc.status {
				t.Fatalf("status = %d, want %d", code, tc.status)
			}
			e.noSecrets(tc.token)
		})
	}
	if e.rec.passes != 2 {
		t.Fatal("a refused caller ran a pass")
	}

	t.Run("lease held", func(t *testing.T) {
		e.rec.block = make(chan struct{})
		done := make(chan int)
		go func() {
			code, _ := e.post(server.ReconcilePath, e.gg.Token(pusher, audience), "{}")
			done <- code
		}()
		for {
			if _, _, ok := e.gcs.Object("state", pass.LeaseName); ok {
				break
			}
			time.Sleep(time.Millisecond)
		}
		code, body := e.post(server.ReconcilePath, e.gg.Token(pusher, audience), "{}")
		if code != http.StatusServiceUnavailable || body["retryable"] != true {
			t.Fatalf("push while a pass runs: %d %v, want a retryable 503", code, body)
		}
		close(e.rec.block)
		if code := <-done; code != http.StatusNoContent {
			t.Fatalf("first pass: %d", code)
		}
	})
}

func TestThereIsNoBindEndpoint(t *testing.T) {
	e := setup(t)
	// Runs are bound at their first job's check: no endpoint takes a
	// GitHub token.
	tok := e.gh.ActionsToken(e.gh.PushRun(e.repo, "main"), ghemu.Job{Audience: audience}, nil)
	if code, _ := e.post("/v1/bind", tok, ""); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/bind: %d", code)
	}
}

func TestPushedEventsAreRecordedBeforeTheyreAcknowledged(t *testing.T) {
	e := setup(t)
	tok := e.gg.Token(pusher, audience)
	data := []byte(`{"protoPayload": {"methodName": "compute.instances.preempted"}}`)
	body := string(google.Push("lanescape-dev", "lanescape-runners-compute-events-reconcile", "1", data))
	if code, _ := e.post(server.ReconcilePath, tok, body); code != http.StatusNoContent || e.ev.count() != 1 || string(e.ev.got[0]) != string(data) {
		t.Fatalf("push: %d, recorded %d", code, e.ev.count())
	}
	// A webhook delivery's message comes with its event's name.
	closed := []byte(`{"action": "closed"}`)
	wbody := string(google.PushWith("lanescape-dev", "lanescape-runners-webhooks-reconcile", "2", closed, map[string]string{"event": "pull_request"}))
	if code, _ := e.post(server.ReconcilePath, tok, wbody); code != http.StatusNoContent || e.ev.count() != 2 || e.ev.attrs[1]["event"] != "pull_request" {
		t.Fatalf("webhook push: %d, recorded %d", code, e.ev.count())
	}
	// A message that can't be recorded isn't acknowledged, and Pub/Sub
	// delivers it again; no pass runs for it.
	e.ev.fail = true
	passes := e.rec.passes
	if code, body := e.post(server.ReconcilePath, tok, body); code != http.StatusServiceUnavailable || body["retryable"] != true || e.rec.passes != passes {
		t.Fatalf("unrecorded push: %d %v", code, body)
	}
	e.ev.fail = false
	// A push that finds the lease held is recorded all the same.
	e.rec.block = make(chan struct{})
	done := make(chan int)
	go func() {
		code, _ := e.post(server.ReconcilePath, e.gg.Token(scheduler, audience), "{}")
		done <- code
	}()
	for {
		if _, _, ok := e.gcs.Object("state", pass.LeaseName); ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if code, _ := e.post(server.ReconcilePath, tok, body); code != http.StatusServiceUnavailable || e.ev.count() != 3 {
		t.Fatalf("push while a pass runs: %d, recorded %d", code, e.ev.count())
	}
	close(e.rec.block)
	<-done
	// The scheduler's calls carry no message.
	if code, _ := e.post(server.ReconcilePath, e.gg.Token(scheduler, audience), "{}"); code != http.StatusNoContent || e.ev.count() != 3 {
		t.Fatalf("scheduler: %d, recorded %d", code, e.ev.count())
	}
}

func TestAPassCutShortIsRetried(t *testing.T) {
	e := setupPass(t, 200*time.Millisecond)
	e.rec.block = make(chan struct{}) // never closed: the pass runs to its deadline
	code, body := e.post(server.ReconcilePath, e.gg.Token(pusher, audience), "{}")
	if code != http.StatusServiceUnavailable || body["retryable"] != true {
		t.Fatalf("pass cut short: %d %v, want a retryable 503", code, body)
	}
	if _, _, ok := e.gcs.Object("state", pass.LeaseName); !ok {
		t.Fatal("lease released past the deadline; it should be left to expire")
	}
}
