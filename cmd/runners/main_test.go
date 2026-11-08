package main

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/store"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func good() map[string]string {
	return map[string]string{
		"RUNNERS_AUDIENCE":            "https://runners.example.test",
		"RUNNERS_PROJECT":             "lanescape-dev",
		"RUNNERS_BUCKET":              "lanescape-runners",
		"RUNNERS_GITHUB_APP_ID":       "12345",
		"RUNNERS_GITHUB_APP_KEY_FILE": "/secrets/app-key",
		"RUNNERS_WEBHOOK_SECRET_FILE": "/secrets/webhook",
		"RUNNERS_RECONCILE_ACCOUNTS":  "scheduler@p.iam.gserviceaccount.com, push@p.iam.gserviceaccount.com",
	}
}

func TestSettings(t *testing.T) {
	s, err := loadSettings(env(good()))
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != "8080" || s.AppID != 12345 ||
		!slices.Equal(s.ReconcileAccounts, []string{"scheduler@p.iam.gserviceaccount.com", "push@p.iam.gserviceaccount.com"}) {
		t.Fatalf("settings = %+v", s)
	}
}

func TestBadSettings(t *testing.T) {
	for name, tc := range map[string]struct{ key, value, want string }{
		"no audience":     {"RUNNERS_AUDIENCE", "", "RUNNERS_AUDIENCE is required"},
		"http audience":   {"RUNNERS_AUDIENCE", "http://runners", "https URL"},
		"no bucket":       {"RUNNERS_BUCKET", "", "RUNNERS_BUCKET is required"},
		"no project":      {"RUNNERS_PROJECT", "", "RUNNERS_PROJECT is required"},
		"app by name":     {"RUNNERS_GITHUB_APP_ID", "lanescape-runners", "numeric ID"},
		"no accounts":     {"RUNNERS_RECONCILE_ACCOUNTS", " , ", "RUNNERS_RECONCILE_ACCOUNTS is required"},
		"a person":        {"RUNNERS_RECONCILE_ACCOUNTS", "someone@example.com", "isn't a service account"},
		"no webhook file": {"RUNNERS_WEBHOOK_SECRET_FILE", "", "RUNNERS_WEBHOOK_SECRET_FILE is required"},
		"no app key file": {"RUNNERS_GITHUB_APP_KEY_FILE", "", "RUNNERS_GITHUB_APP_KEY_FILE is required"},
		"negative app ID": {"RUNNERS_GITHUB_APP_ID", "-1", "numeric ID"},
		"missing app ID":  {"RUNNERS_GITHUB_APP_ID", "", "numeric ID"},
		"ftp audience":    {"RUNNERS_AUDIENCE", "ftp://x", "https URL"},
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
