package event

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func env(t *testing.T, name, payload string) func(string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{
		"GITHUB_EVENT_NAME": name, "GITHUB_REPOSITORY": "lanescape/app", "GITHUB_SHA": sha,
		"GITHUB_API_URL": "https://api.github.com/", "GITHUB_EVENT_PATH": path,
		"GITHUB_REPOSITORY_ID": "5001", "GITHUB_REPOSITORY_OWNER_ID": "303994381",
	}
	return func(k string) string { return m[k] }
}

func TestLoad(t *testing.T) {
	r, err := Load(env(t, "push", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.APIURL != "https://api.github.com" || r.OwnerID != 303994381 || r.RepositoryID != 5001 || r.SHA != sha {
		t.Fatalf("run = %+v", r)
	}
	if _, err := Load(func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "GITHUB_EVENT_PATH") {
		t.Fatalf("err = %v", err)
	}
}

func TestContribution(t *testing.T) {
	r, err := Load(env(t, "pull_request_target", `{"pull_request": {"number": 7, "head": {"sha": "`+sha+`", "repo": {"id": 7001, "full_name": "someone/app"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	n, head, s, err := r.Contribution()
	if err != nil || n != 7 || head.ID != 7001 || head.FullName != "someone/app" || s != sha {
		t.Fatalf("contribution = %d %+v %s %v", n, head, s, err)
	}
	for name, tc := range map[string]struct{ event, payload, want string }{
		"another event": {"pull_request", `{"pull_request": {"number": 7}}`, "not a pull_request_target run"},
		"no pull":       {"pull_request_target", `{}`, "names no pull request"},
		"head gone":     {"pull_request_target", `{"pull_request": {"number": 7, "head": {"sha": "` + sha + `", "repo": null}}}`, "head repository is gone"},
		"short sha":     {"pull_request_target", `{"pull_request": {"number": 7, "head": {"sha": "abc", "repo": {"id": 1, "full_name": "a/b"}}}}`, "isn't a commit SHA"},
	} {
		r, err := Load(env(t, tc.event, tc.payload))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := r.Contribution(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
