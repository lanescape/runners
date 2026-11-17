package where

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/runners/actions/internal/event"
)

const org = endpoints.Organization

// load builds a run from the job's variables and a payload, as GitHub gives
// them.
func load(t *testing.T, name string, owner, repo int64, payload string) event.Run {
	t.Helper()
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"GITHUB_EVENT_NAME": name, "GITHUB_REPOSITORY": "lanescape/app", "GITHUB_SHA": strings.Repeat("a", 40),
		"GITHUB_API_URL": "https://api.github.com", "GITHUB_EVENT_PATH": path,
	}
	if owner != 0 {
		env["GITHUB_REPOSITORY_OWNER_ID"] = itoa(owner)
	}
	if repo != 0 {
		env["GITHUB_REPOSITORY_ID"] = itoa(repo)
	}
	r, err := event.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func itoa(n int64) string {
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

const (
	push       = `{"repository": {"id": 5001, "full_name": "lanescape/app", "owner": {"id": 303994381}}}`
	ownPull    = `{"repository": {"id": 5001}, "pull_request": {"number": 4, "head": {"sha": "` + sha + `", "repo": {"id": 5001, "full_name": "lanescape/app"}}}}`
	forkPull   = `{"repository": {"id": 5001}, "pull_request": {"number": 4, "head": {"sha": "` + sha + `", "repo": {"id": 7001, "full_name": "someone/app"}}}}`
	deletedFor = `{"repository": {"id": 5001}, "pull_request": {"number": 4, "head": {"sha": "` + sha + `", "repo": null}}}`
	sha        = "0123456789abcdef0123456789abcdef01234567"
)

func TestOf(t *testing.T) {
	for _, tc := range []struct {
		name    string
		event   string
		owner   int64
		payload string
		inside  bool
		fork    bool
	}{
		{"organization push", "push", org, push, true, false},
		{"organization pull request", "pull_request", org, ownPull, true, false},
		{"organization dispatch", "workflow_dispatch", org, push, true, false},
		{"fork's pull request into the organization", "pull_request", org, forkPull, false, true},
		{"fork's review of a pull request", "pull_request_review", org, forkPull, false, true},
		{"fork's pull request whose head is gone", "pull_request", org, deletedFor, false, true},
		{"approved contribution", "pull_request_target", org, forkPull, true, false},
		{"own pull_request_target", "pull_request_target", org, ownPull, true, false},
		{"another organization", "push", 1234, push, false, false},
		{"a fork's own push", "push", 9001, push, false, false},
		{"a fork's own pull_request_target", "pull_request_target", 9001, forkPull, false, false},
	} {
		p := Of(load(t, tc.event, tc.owner, 5001, tc.payload))
		if p.Inside != tc.inside || p.ForkIntoOrganization != tc.fork {
			t.Errorf("%s: %+v, want inside %v, fork into the organization %v", tc.name, p, tc.inside, tc.fork)
		}
		if p.Why == "" || !strings.HasPrefix(p.Summary(), map[bool]string{true: "Inside", false: "Outside"}[tc.inside]) {
			t.Errorf("%s: summary %q", tc.name, p.Summary())
		}
	}
}

// The owner is compared by ID, never by name: an account named lanescape
// that isn't the organization is outside.
func TestOfComparesIDsNotNames(t *testing.T) {
	r := load(t, "push", 42, 5001, push)
	r.Repository = "lanescape/app"
	if Of(r).Inside {
		t.Fatal("an account named lanescape was taken for the organization")
	}
}

// Without GITHUB_REPOSITORY_OWNER_ID, the payload's owner is used.
func TestOwnerFromPayload(t *testing.T) {
	if !Of(load(t, "push", 0, 0, push)).Inside {
		t.Fatal("the payload's owner wasn't used")
	}
}
