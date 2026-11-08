package main

import (
	"slices"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func good() map[string]string {
	return map[string]string{
		"RUNNERS_AUDIENCE":            "https://runners.example.test",
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
