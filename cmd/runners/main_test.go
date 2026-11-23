package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/endpoints"
	"github.com/lanescape/runners/internal/deploy"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/store"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func good() map[string]string {
	return map[string]string{
		"RUNNERS_AUDIENCE":            endpoints.Service,
		"RUNNERS_PROJECT":             "lanescape-dev",
		"RUNNERS_BUCKET":              "lanescape-runners",
		"RUNNERS_CACHES_BUCKET":       "lanescape-runners-caches",
		"RUNNERS_WEBHOOK_TOPIC":       "projects/lanescape-dev/topics/lanescape-runners-webhooks",
		"RUNNERS_GITHUB_APP_ID":       "12345",
		"RUNNERS_GITHUB_APP_KEY_FILE": "/secrets/app-key",
		"RUNNERS_WEBHOOK_SECRET_FILE": "/secrets/webhook",
		"RUNNERS_RECONCILE_ACCOUNTS":  "scheduler@p.iam.gserviceaccount.com, push@p.iam.gserviceaccount.com",
		"RUNNERS_DEPLOY_ID":           "19283746-2",
	}
}

func TestSettings(t *testing.T) {
	s, err := loadSettings(env(good()))
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != "8080" || s.AppID != 12345 || s.Deploy != (deploy.ID{Run: 19283746, Attempt: 2}) ||
		!slices.Equal(s.ReconcileAccounts, []string{"scheduler@p.iam.gserviceaccount.com", "push@p.iam.gserviceaccount.com"}) {
		t.Fatalf("settings = %+v", s)
	}
}

// TestNoDeployID: a service run outside a deploy, with no ID, starts.
func TestNoDeployID(t *testing.T) {
	m := good()
	delete(m, "RUNNERS_DEPLOY_ID")
	s, err := loadSettings(env(m))
	if err != nil || !s.Deploy.IsZero() {
		t.Fatalf("settings = %+v, %v", s, err)
	}
}

func TestBadSettings(t *testing.T) {
	for name, tc := range map[string]struct{ key, value, want string }{
		"no audience":      {"RUNNERS_AUDIENCE", "", "RUNNERS_AUDIENCE is required"},
		"http audience":    {"RUNNERS_AUDIENCE", "http://lanescape-runners-168731703350.northamerica-northeast1.run.app", "endpoints.Service"},
		"other audience":   {"RUNNERS_AUDIENCE", "https://runners.example.test", "endpoints.Service"},
		"no bucket":        {"RUNNERS_BUCKET", "", "RUNNERS_BUCKET is required"},
		"no caches bucket": {"RUNNERS_CACHES_BUCKET", "", "RUNNERS_CACHES_BUCKET is required"},
		"no topic":         {"RUNNERS_WEBHOOK_TOPIC", "", "RUNNERS_WEBHOOK_TOPIC is required"},
		"no project":       {"RUNNERS_PROJECT", "", "RUNNERS_PROJECT is required"},
		"app by name":      {"RUNNERS_GITHUB_APP_ID", "lanescape-runners", "numeric ID"},
		"no accounts":      {"RUNNERS_RECONCILE_ACCOUNTS", " , ", "RUNNERS_RECONCILE_ACCOUNTS is required"},
		"a person":         {"RUNNERS_RECONCILE_ACCOUNTS", "someone@example.com", "isn't a service account"},
		"no webhook file":  {"RUNNERS_WEBHOOK_SECRET_FILE", "", "RUNNERS_WEBHOOK_SECRET_FILE is required"},
		"no app key file":  {"RUNNERS_GITHUB_APP_KEY_FILE", "", "RUNNERS_GITHUB_APP_KEY_FILE is required"},
		"negative app ID":  {"RUNNERS_GITHUB_APP_ID", "-1", "numeric ID"},
		"missing app ID":   {"RUNNERS_GITHUB_APP_ID", "", "numeric ID"},
		"bad deploy ID":    {"RUNNERS_DEPLOY_ID", "19283746", "RUNNERS_DEPLOY_ID"},
	} {
		t.Run(name, func(t *testing.T) {
			m := good()
			m[tc.key] = tc.value
			_, err := loadSettings(env(m))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestKicker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emu := gcsemu.New()
	defer emu.Close()
	emu.CreateBucket("state")
	objects, err := store.NewGCS(ctx, "state", emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	defer objects.Close()
	clock := clockwork.NewRealClock()
	passes := &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock}

	// A pass is running when the report arrives.
	running, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = passes.Run(ctx, func(*pass.Pass) error {
			close(running)
			<-release
			return nil
		})
	}()
	<-running
	ran := make(chan struct{}, 10)
	kick := kicker(ctx, passes, func(*pass.Pass) error { ran <- struct{}{}; return nil }, 20*time.Millisecond, slog.Default())
	kick()
	kick()
	time.Sleep(100 * time.Millisecond)
	if len(ran) != 0 {
		t.Fatal("a pass ran while another held the lease")
	}
	close(release)
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("no pass ran after the report")
	}
}

// TestStopReleasesThePassBeforeTheServer: stop ends a pass running in a
// request, with the lease given back, before it stops the server, which
// would otherwise wait for that request until Cloud Run ended the instance.
// The handler here stands in for reconcile, whose own answer to a stopped
// pass internal/server's tests hold.
func TestStopReleasesThePassBeforeTheServer(t *testing.T) {
	emu := gcsemu.New()
	defer emu.Close()
	emu.CreateBucket("state")
	objects, err := store.NewGCS(context.Background(), "state", emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	defer objects.Close()
	clock := clockwork.NewRealClock()
	passes := &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock}

	running := make(chan struct{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := passes.Run(r.Context(), func(p *pass.Pass) error {
			close(running)
			<-p.Context().Done()
			return p.Context().Err()
		})
		if errors.Is(err, pass.ErrStopping) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	answer := make(chan int, 1)
	go func() {
		resp, err := http.Post("http://"+ln.Addr().String()+"/v1/reconcile", "application/json", nil)
		if err != nil {
			answer <- 0
			return
		}
		resp.Body.Close()
		answer <- resp.StatusCode
	}()
	<-running
	if _, _, ok := emu.Object("state", pass.LeaseName); !ok {
		t.Fatal("the pass holds no lease")
	}
	began := time.Now()
	if err := stop(passes, srv, slog.Default()); err != nil {
		t.Fatalf("stop = %v", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("stopping took %s", took)
	}
	if code := <-answer; code != http.StatusServiceUnavailable {
		t.Errorf("the pass's request answered %d, not 503", code)
	}
	if _, _, ok := emu.Object("state", pass.LeaseName); ok {
		t.Fatal("the lease was left held")
	}
}

// TestPushedEventsAreRouted: a workflow_job delivery's queued run goes to the
// machines' hints; any other pushed message goes to the disks' records.
func TestPushedEventsAreRouted(t *testing.T) {
	ctx := context.Background()
	emu := gcsemu.New()
	defer emu.Close()
	emu.CreateBucket("state")
	objects, err := store.NewGCS(ctx, "state", emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	defer objects.Close()
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	clock := clockwork.NewFakeClock()
	ev := machines.Events{
		Machines: &machines.Service{Config: cfg, Objects: objects, Clock: clock},
		Others:   &disks.Service{Config: cfg, Objects: objects, Clock: clock},
	}
	queued := fmt.Sprintf(`{"action":"queued","workflow_job":{"id":7,"run_id":42,"labels":["lanescape-2vcpu-linux-amd64"]},"repository":{"id":5001,"owner":{"id":%d}}}`, cfg.Organization.ID)
	if err := ev.RecordEvent(ctx, map[string]string{"event": "workflow_job"}, []byte(queued)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := emu.Object("state", "queued/5001/42/7.json"); !ok {
		t.Fatal("the queued job's delivery left no hint")
	}
	// The same body as another event isn't a hint.
	if err := ev.RecordEvent(ctx, map[string]string{"event": "workflow_run"}, []byte(strings.Replace(queued, "42", "43", 1))); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := emu.Object("state", "queued/5001/43/7.json"); ok {
		t.Fatal("a workflow_run message left a hint")
	}
}
