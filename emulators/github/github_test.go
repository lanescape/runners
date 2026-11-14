package github_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/golang-jwt/jwt/v5"
	gh "github.com/google/go-github/v93/github"

	ghemu "github.com/lanescape/runners/emulators/github"
)

// These tests check the emulator against go-github directly, for what the
// service's client doesn't exercise.

func get(t *testing.T, url, auth string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestRequestsNeedAnInstallationToken(t *testing.T) {
	emu := ghemu.New()
	defer emu.Close()
	org := emu.Org(1, "org")
	emu.Repo(org, 10, "app")
	for _, a := range []string{"", "token ghs_guess", "Bearer ghs_guess"} {
		if code := get(t, emu.URL+"/repositories/10", a); code != http.StatusUnauthorized {
			t.Errorf("auth %q: %d, want 401", a, code)
		}
	}
}

func TestAppJWTs(t *testing.T) {
	emu := ghemu.New()
	defer emu.Close()
	app := emu.NewApp(5)
	other := emu.NewApp(6)
	sign := func(k *ghemu.App, iss string, iat, exp time.Time) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
			Issuer: iss, IssuedAt: jwt.NewNumericDate(iat), ExpiresAt: jwt.NewNumericDate(exp)})
		s, err := tok.SignedString(k.Key)
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + s
	}
	now := time.Now()
	if code := get(t, emu.URL+"/app/installations", sign(app, "5", now, now.Add(5*time.Minute))); code != http.StatusOK {
		t.Fatalf("good JWT: %d", code)
	}
	for name, a := range map[string]string{
		"none":               "",
		"other key":          sign(other, "5", now, now.Add(5*time.Minute)),
		"unknown app":        sign(app, "99", now, now.Add(5*time.Minute)),
		"expired":            sign(app, "5", now.Add(-20*time.Minute), now.Add(-10*time.Minute)),
		"too long":           sign(app, "5", now, now.Add(time.Hour)),
		"issued later":       sign(app, "5", now.Add(10*time.Minute), now.Add(15*time.Minute)),
		"not a number":       sign(app, "app", now, now.Add(5*time.Minute)),
		"a token, not a JWT": "Bearer ghs_abc",
	} {
		if code := get(t, emu.URL+"/app/installations", a); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
}

func TestInstallationSeesItsAccountAndPublicRepositories(t *testing.T) {
	ctx := context.Background()
	emu := ghemu.New()
	defer emu.Close()
	org := emu.Org(1, "org")
	app := emu.NewApp(5)
	in := emu.Install(app, org)
	mine := emu.Repo(org, 10, "app")
	mine.Private = true
	other := emu.Org(2, "other")
	public := emu.Repo(other, 20, "lib")
	private := emu.Repo(other, 30, "secret")
	private.Private = true

	tr, err := ghinstallation.New(http.DefaultTransport, app.ID, in.ID, app.PrivateKeyPEM())
	if err != nil {
		t.Fatal(err)
	}
	tr.BaseURL = emu.URL
	base := emu.URL + "/"
	c, err := gh.NewClient(gh.WithHTTPClient(&http.Client{Transport: tr}), gh.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*ghemu.Repo{mine, public} {
		if _, _, err := c.Repositories.GetByID(ctx, r.ID); err != nil {
			t.Errorf("%s: %v", r.FullName(), err)
		}
		if _, _, err := c.Repositories.Get(ctx, strings.ToUpper(r.Owner.Login), r.Name); err != nil {
			t.Errorf("%s by name in another case: %v", r.FullName(), err)
		}
	}
	if _, resp, err := c.Repositories.GetByID(ctx, private.ID); err == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("another account's private repository: %v", err)
	}
}

func TestADeletedRepositoryIsNotFound(t *testing.T) {
	ctx := context.Background()
	emu := ghemu.New()
	defer emu.Close()
	org := emu.Org(1, "org")
	app := emu.NewApp(5)
	in := emu.Install(app, org)
	r := emu.Repo(org, 10, "app")
	emu.Delete(r)
	tr, err := ghinstallation.New(http.DefaultTransport, app.ID, in.ID, app.PrivateKeyPEM())
	if err != nil {
		t.Fatal(err)
	}
	tr.BaseURL = emu.URL
	base := emu.URL + "/"
	c, err := gh.NewClient(gh.WithHTTPClient(&http.Client{Transport: tr}), gh.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	if _, resp, err := c.Repositories.GetByID(ctx, r.ID); err == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("a deleted repository by ID: %v", err)
	}
	if _, resp, err := c.Repositories.Get(ctx, "org", "app"); err == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("a deleted repository by name: %v", err)
	}
}

func TestDeliveriesAreSigned(t *testing.T) {
	secret := []byte("s")
	var got http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	resp, err := ghemu.NewDelivery(srv.URL, secret, "workflow_job", map[string]any{"action": "queued"}).Send(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("X-GitHub-Event") != "workflow_job" || got.Get("X-GitHub-Delivery") == "" || got.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", got)
	}
	if got.Get("X-Hub-Signature-256") != ghemu.Sign(secret, body) {
		t.Fatal("signature doesn't cover the body")
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil || v["action"] != "queued" {
		t.Fatalf("body = %s", body)
	}
}

func TestActionsTokensDefaultToTheRun(t *testing.T) {
	emu := ghemu.New()
	defer emu.Close()
	org := emu.Org(1, "org")
	repo := emu.Repo(org, 10, "app")
	user := emu.User(2, "someone")
	fork := emu.Fork(repo, user, 11)
	fork.Commit("f", map[string]string{"a": "b"})
	p := emu.PullRequest(repo, "main", fork, "f", user)

	pr := emu.ActionsClaims(emu.PullRequestRun(p), ghemu.Job{})
	if pr["ref"] != "refs/pull/1/merge" || pr["event_name"] != "pull_request" || pr["head_ref"] != "f" || pr["base_ref"] != "main" {
		t.Errorf("pull_request claims = %v", pr)
	}
	if pr["sha"] == p.HeadSHA {
		t.Error("a pull_request run's sha should be its merge commit, not its head")
	}
	prt := emu.ActionsClaims(emu.PullRequestTargetRun(p), ghemu.Job{})
	if prt["ref"] != "refs/heads/main" || prt["sha"] != repo.Head("main") {
		t.Errorf("pull_request_target claims = %v", prt)
	}
	if prt["job_workflow_ref"] != "org/app/.github/workflows/contribution.yml@refs/heads/main" {
		t.Errorf("job_workflow_ref = %v", prt["job_workflow_ref"])
	}
	repo.Tag("v1", repo.Head("main"))
	tag := emu.ActionsClaims(emu.TagRun(repo, "v1"), ghemu.Job{})
	if tag["ref"] != "refs/tags/v1" || tag["ref_type"] != "tag" {
		t.Errorf("tag claims = %v", tag)
	}
}
