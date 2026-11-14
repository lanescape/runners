package disks_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/google"
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/pubsub"
	"github.com/lanescape/runners/internal/server"
)

const (
	webhookSecret = "webhook secret"
	webhookTopic  = "projects/lanescape-dev/topics/lanescape-runners-webhooks"
)

// webhook serves the service's webhook, which records closings with the
// disks' service and publishes to Pub/Sub's emulator, and returns its
// address and the emulator.
func (e *env) webhook() (string, *google.PubSub) {
	e.t.Helper()
	ctx := context.Background()
	ps := google.NewPubSub()
	e.t.Cleanup(ps.Close)
	ps.CreateTopic(webhookTopic)
	pub, err := pubsub.New(ctx, webhookTopic, ps.ClientOptions()...)
	if err != nil {
		e.t.Fatal(err)
	}
	pub.Backoff = time.Millisecond
	gg := google.New()
	e.t.Cleanup(gg.Close)
	callers, err := auth.NewGoogle(auth.Options{Issuer: auth.Issuer{URL: gg.URL, JWKSURL: gg.JWKSURL}, Audience: audience},
		[]string{"push@lanescape-dev.iam.gserviceaccount.com"})
	if err != nil {
		e.t.Fatal(err)
	}
	h, err := server.New(server.Config{OrganizationID: orgID, WebhookSecret: []byte(webhookSecret), Publisher: pub,
		ReconcileCallers: callers, Passes: e.runner, Reconciler: reconcile(pass.All(e.svc.Pass, e.disks.Pass)),
		Events: e.disks, Closings: e.disks})
	if err != nil {
		e.t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	e.t.Cleanup(srv.Close)
	return srv.URL, ps
}

type reconcile pass.Body

func (r reconcile) Pass(p *pass.Pass) error { return r(p) }

// The review's case: a pull request closes while Pub/Sub refuses its
// closing's delivery, which GitHub doesn't send again, and is reopened
// before any pass sees it closed. The webhook recorded the closing before
// it tried to publish, so the reopened pull request starts again from
// main's disk, and what it saved before goes (specs/0003-disks.md,
// Deletion).
func TestAClosingIsRecordedAtTheWebhookThoughItsDeliveryFails(t *testing.T) {
	e := setup(t)
	url, ps := e.webhook()
	m := e.saveMain(key1)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	img := e.savePull(p, sel(key1, pr))

	e.gh.ClosePull(p)
	payload := e.gh.PullRequestPayload(p, nil, "closed")
	// GitHub's emulator keeps real time; the service's is the test's.
	payload["pull_request"].(map[string]any)["closed_at"] = e.clock.Now().UTC().Add(time.Second).Format(time.RFC3339)
	ps.Faults().Add(fault.Rule{Times: 10, Respond: google.GoogleError(http.StatusServiceUnavailable, "UNAVAILABLE", "try again")})
	resp, err := ghemu.NewDelivery(url+server.WebhookPath, []byte(webhookSecret), "pull_request", payload).Send(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || len(ps.Messages(webhookTopic)) != 0 {
		t.Fatalf("the delivery: %d, %d published; want it refused, with Pub/Sub down", resp.StatusCode, len(ps.Messages(webhookTopic)))
	}
	if _, ok := e.record("closures/5001/" + pr + ".json"); !ok {
		t.Fatal("the closing wasn't recorded at the webhook")
	}

	e.gh.ReopenPull(p)
	e.clock.Advance(time.Minute)
	if src := e.start(e.pullRun(p), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, pr)}}).source(); src != m {
		t.Fatalf("a pull request reopened after a lost delivery restored %q, want main's %s", src, m)
	}
	if e.has(img) {
		t.Fatal("a disk saved before its pull request's closing outlived it")
	}
}

// A slow bucket doesn't keep the webhook from answering GitHub, which hangs
// up after 10 seconds: it waits for a closing to be recorded no more than 5,
// then publishes the delivery all the same, while the recording, and the
// release of the disks' lease it took, go on.
func TestASlowBucketDoesntKeepAClosingFromBeingPublished(t *testing.T) {
	e := setup(t)
	url, ps := e.webhook()
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	e.gh.ClosePull(p)
	payload := e.gh.PullRequestPayload(p, nil, "closed")
	payload["pull_request"].(map[string]any)["closed_at"] = e.clock.Now().UTC().Add(time.Second).Format(time.RFC3339)
	// The disks' steps have their deadline in production, 30 seconds, which
	// the webhook's own bounds.
	e.disks.StepDeadline = 0
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1, Delay: 4 * time.Second,
		Match: func(r *http.Request) bool { return strings.Contains(r.URL.Path, "closures/5001/") }})
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodDelete, Times: 1, Delay: 8 * time.Second,
		Match: func(r *http.Request) bool { return strings.Contains(r.URL.Path, "leases/disks/5001") }})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := ghemu.NewDelivery(url+server.WebhookPath, []byte(webhookSecret), "pull_request", payload).Send(ctx)
	if err != nil {
		t.Fatalf("GitHub hung up after %s: %v", time.Since(start), err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || len(ps.Messages(webhookTopic)) != 1 {
		t.Fatalf("the delivery: %d, %d published, after %s", resp.StatusCode, len(ps.Messages(webhookTopic)), time.Since(start))
	}
	if d := time.Since(start); d > 7*time.Second {
		t.Fatalf("answered after %s", d)
	}
	// Recorded, though the lease it took is still being released.
	if _, ok := e.record("closures/5001/" + pr + ".json"); !ok {
		t.Fatal("the closing wasn't recorded")
	}
}
