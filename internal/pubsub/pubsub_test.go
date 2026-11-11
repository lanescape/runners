package pubsub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/google"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/pubsub"
	"github.com/lanescape/runners/internal/server"
	"github.com/lanescape/runners/internal/store"
)

const (
	topic    = "projects/lanescape-dev/topics/lanescape-runners-webhooks"
	orgID    = 303994381
	audience = "https://runners.example.test"
	pusher   = "lanescape-runners-push@lanescape-dev.iam.gserviceaccount.com"
)

func emulator(t *testing.T) (*google.PubSub, *pubsub.Publisher) {
	t.Helper()
	emu := google.NewPubSub()
	t.Cleanup(emu.Close)
	emu.CreateTopic(topic)
	p, err := pubsub.New(context.Background(), topic, emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	p.Backoff = time.Millisecond
	return emu, p
}

func TestPublish(t *testing.T) {
	emu, p := emulator(t)
	body := []byte(`{"action": "completed"}`)
	if err := p.Publish(context.Background(), server.Delivery{Event: "workflow_job", ID: "d1", Body: body}); err != nil {
		t.Fatal(err)
	}
	msgs := emu.Messages(topic)
	if len(msgs) != 1 || !bytes.Equal(msgs[0].Data, body) || msgs[0].Attributes["event"] != "workflow_job" {
		t.Fatalf("published %+v", msgs)
	}
}

func TestPublishTriesAgain(t *testing.T) {
	emu, p := emulator(t)
	emu.Faults().Add(fault.Rule{Times: 2, Respond: google.GoogleError(http.StatusServiceUnavailable, "UNAVAILABLE", "try again")})
	if err := p.Publish(context.Background(), server.Delivery{Event: "pull_request", Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if n := len(emu.Messages(topic)); n != 1 {
		t.Fatalf("%d messages", n)
	}
	// A refusal isn't tried again.
	emu.Faults().Add(fault.Rule{Times: 1, Respond: google.GoogleError(http.StatusForbidden, "PERMISSION_DENIED", "no")})
	if err := p.Publish(context.Background(), server.Delivery{Event: "pull_request", Body: []byte(`{}`)}); err == nil {
		t.Fatal("a refusal published")
	}
	// Nor is anything past the attempts.
	emu.Faults().Add(fault.Rule{Times: 3, Respond: google.GoogleError(http.StatusServiceUnavailable, "UNAVAILABLE", "try again")})
	if err := p.Publish(context.Background(), server.Delivery{Event: "pull_request", Body: []byte(`{}`)}); err == nil {
		t.Fatal("published through three failures")
	}
	if n := len(emu.Messages(topic)); n != 1 {
		t.Fatalf("%d messages", n)
	}
}

func TestNewRefusesATopicThatIsntAFullName(t *testing.T) {
	for _, name := range []string{"", "lanescape-runners-webhooks", "projects/lanescape-dev/subscriptions/x", "projects/p/topics/t"} {
		if _, err := pubsub.New(context.Background(), name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

// A pull request's closing goes from GitHub's delivery, through Pub/Sub, to
// the reconcile endpoint, which records it before acknowledging the push.
func TestAClosingReachesTheRecorder(t *testing.T) {
	ctx := context.Background()
	ps, pub := emulator(t)
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket("state")
	gg := google.New()
	t.Cleanup(gg.Close)
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	objects, err := store.NewGCS(ctx, "state", gcs.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objects.Close() })
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	callers, err := auth.NewGoogle(auth.Options{Issuer: auth.Issuer{URL: gg.URL, JWKSURL: gg.JWKSURL}, Audience: audience}, []string{pusher})
	if err != nil {
		t.Fatal(err)
	}
	clock := clockwork.NewRealClock()
	secret := []byte("webhook secret")
	h, err := server.New(server.Config{
		OrganizationID: orgID, WebhookSecret: secret, Publisher: pub, ReconcileCallers: callers,
		Passes:     &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock},
		Reconciler: nothing{}, Events: &disks.Service{Config: cfg, Objects: objects, Clock: clock},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	closedAt := time.Now().UTC().Truncate(time.Second)
	payload := map[string]any{
		"action":       "closed",
		"number":       7,
		"pull_request": map[string]any{"number": 7, "state": "closed", "closed_at": closedAt.Format(time.RFC3339)},
		"repository":   map[string]any{"id": 5001, "name": "app", "owner": map[string]any{"id": orgID, "login": "lanescape"}},
		"organization": map[string]any{"id": orgID},
	}
	resp, err := ghemu.NewDelivery(srv.URL+server.WebhookPath, secret, "pull_request", payload).Send(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook answered %d", resp.StatusCode)
	}
	msgs := ps.Messages(topic)
	if len(msgs) != 1 || msgs[0].Attributes["event"] != "pull_request" {
		t.Fatalf("published %+v", msgs)
	}
	// Pub/Sub pushes the message, signed as the push account.
	body := google.PushWith("lanescape-dev", "lanescape-runners-webhooks-reconcile", msgs[0].ID, msgs[0].Data, msgs[0].Attributes)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+server.ReconcilePath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+gg.Token(pusher, audience))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reconcile answered %d", resp.StatusCode)
	}
	data, _, ok := gcs.Object("state", "closures/5001/pr7.json")
	if !ok {
		t.Fatal("the closing wasn't recorded")
	}
	var c struct {
		SeenAt time.Time `json:"seen_at"`
	}
	if err := json.Unmarshal(data, &c); err != nil || !c.SeenAt.Equal(closedAt.Add(time.Second)) {
		t.Fatalf("closure %s", data)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatal("the closure holds a secret")
	}
}

type nothing struct{}

func (nothing) Pass(*pass.Pass) error { return nil }
