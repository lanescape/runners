package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/github"
)

const orgID = 303994381

type env struct {
	emu  *ghemu.Emulator
	org  *ghemu.Account
	app  *ghemu.App
	repo *ghemu.Repo
	c    *github.Client
}

func setup(t *testing.T) env {
	t.Helper()
	emu := ghemu.New()
	t.Cleanup(emu.Close)
	org := emu.Org(orgID, "lanescape")
	app := emu.NewApp(77)
	emu.Install(emu.NewApp(78), org) // another app's installation is never used
	emu.Install(app, emu.Org(1234, "elsewhere"))
	emu.Install(app, org)
	repo := emu.Repo(org, 5001, "app")
	return env{emu: emu, org: org, app: app, repo: repo, c: client(t, emu, app)}
}

func client(t *testing.T, emu *ghemu.Emulator, app *ghemu.App) *github.Client {
	t.Helper()
	c, err := github.NewAppClient(github.Options{BaseURL: emu.URL, AppID: app.ID, PrivateKey: app.PrivateKeyPEM(), OrganizationID: orgID})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRepositoryAndRun(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	repo, err := e.c.RepositoryByID(ctx, 5001)
	if err != nil {
		t.Fatal(err)
	}
	if repo.OwnerID != orgID || repo.Owner != "lanescape" || repo.Name != "app" || repo.DefaultBranch != "main" {
		t.Fatalf("repository = %+v", repo)
	}
	run := e.emu.PushRun(e.repo, "main")
	got, err := e.c.WorkflowRun(ctx, repo, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := github.WorkflowRun{ID: run.ID, RepositoryID: 5001, Event: "push", Status: "in_progress",
		HeadBranch: "main", HeadSHA: e.repo.Head("main"), HeadRepositoryID: 5001}
	if got != want {
		t.Fatalf("run = %+v, want %+v", got, want)
	}
	if _, err := e.c.RepositoryByID(ctx, 999); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("missing repository: %v", err)
	}
	if _, err := e.c.WorkflowRun(ctx, repo, 999); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("missing run: %v", err)
	}
}

func TestRenamedOrganizationKeepsItsID(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.emu.Rename(e.org, "lanescape-renamed")
	repo, err := e.c.RepositoryByID(ctx, 5001)
	if err != nil || repo.OwnerID != orgID || repo.Owner != "lanescape-renamed" {
		t.Fatalf("repository after rename = %+v, %v", repo, err)
	}
}

func TestPullRequests(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	user := e.emu.User(9, "contributor")
	fork := e.emu.Fork(e.repo, user, 7001)
	var shas []string
	for i := range 3 {
		shas = append(shas, fork.Commit("feature", map[string]string{"f": fmt.Sprint(i)}))
	}
	p := e.emu.PullRequest(e.repo, "main", fork, "feature", user)
	repo, _ := e.c.RepositoryByID(ctx, 5001)

	got, err := e.c.PullRequest(ctx, repo, p.Number)
	if err != nil {
		t.Fatal(err)
	}
	want := github.PullRequest{Number: p.Number, State: "open", BaseRepositoryID: 5001, HeadRepositoryID: 7001, HeadSHA: shas[2], BaseSHA: e.repo.Head("main"), Commits: 3}
	if got != want {
		t.Fatalf("pull request = %+v, want %+v", got, want)
	}
	commits, err := e.c.PullRequestCommits(ctx, repo, p.Number)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(commits) != fmt.Sprint(shas) {
		t.Fatalf("commits = %v, want %v", commits, shas)
	}
	if _, err := e.c.PullRequest(ctx, repo, 99); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("missing pull request: %v", err)
	}
}

func TestOpenPullRequestsPages(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	for i := range 230 {
		b := fmt.Sprintf("b%d", i)
		e.repo.Commit(b, map[string]string{"x": b})
		p := e.emu.PullRequest(e.repo, "main", e.repo, b, e.org)
		if i%10 == 0 {
			e.emu.ClosePull(p)
		}
	}
	repo, _ := e.c.RepositoryByID(ctx, 5001)
	open, err := e.c.OpenPullRequests(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 207 {
		t.Fatalf("listed %d open pull requests, want 207", len(open))
	}
}

func TestPullRequestCommitsStopAt250(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	var last string
	for i := range 260 {
		last = e.repo.Commit("long", map[string]string{"x": fmt.Sprint(i)})
	}
	p := e.emu.PullRequest(e.repo, "main", e.repo, "long", e.org)
	repo, _ := e.c.RepositoryByID(ctx, 5001)
	commits, err := e.c.PullRequestCommits(ctx, repo, p.Number)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 250 {
		t.Fatalf("listed %d commits, want GitHub's 250", len(commits))
	}
	for _, c := range commits {
		if c == last {
			t.Fatal("the 260th commit was listed")
		}
	}
}

func TestBranchHeadIgnoresATagOfTheSameName(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	main := e.repo.Head("main")
	other := e.repo.Commit("other", map[string]string{"x": "y"})
	e.repo.Tag("main", other)
	repo, _ := e.c.RepositoryByID(ctx, 5001)
	got, err := e.c.BranchHead(ctx, repo, "main")
	if err != nil || got != main {
		t.Fatalf("branch head = %s, %v; want %s", got, err, main)
	}
	// The emulator resolves short names as git does, tags first; this is
	// why the service names commits by SHA.
	if f, err := e.c.File(ctx, repo, "x", "main"); err != nil || string(f) != "y" {
		t.Fatalf("file at short name main = %q, %v; want the tag's", f, err)
	}
	if _, err := e.c.BranchHead(ctx, repo, "nope"); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("missing branch: %v", err)
	}
}

func TestCompare(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	old := e.repo.Head("main")
	newer := e.repo.Commit("main", map[string]string{"a": "1"})
	e.repo.Branch("side", old)
	side := e.repo.Commit("side", map[string]string{"b": "1"})
	repo, _ := e.c.RepositoryByID(ctx, 5001)
	for _, tc := range []struct{ base, head, want string }{
		{newer, newer, "identical"},
		{newer, old, "behind"},
		{old, newer, "ahead"},
		{newer, side, "diverged"},
	} {
		got, err := e.c.Compare(ctx, repo, tc.base, tc.head)
		if err != nil || got != tc.want {
			t.Errorf("compare %s...%s = %q, %v; want %q", tc.base[:7], tc.head[:7], got, err, tc.want)
		}
	}
	if _, err := e.c.Compare(ctx, repo, newer, "0123456789012345678901234567890123456789"); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("compare with an unknown commit: %v", err)
	}
}

func TestFile(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	long := ""
	for range 100 {
		long += "a long line of configuration\n"
	}
	sha := e.repo.Commit("main", map[string]string{".github/runners.json": long})
	repo, _ := e.c.RepositoryByID(ctx, 5001)
	got, err := e.c.File(ctx, repo, ".github/runners.json", sha)
	if err != nil || string(got) != long {
		t.Fatalf("file = %q, %v", got, err)
	}
	if _, err := e.c.File(ctx, repo, ".github/proxy.json", sha); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestFailuresAreUnavailable(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		inject func(*ghemu.Emulator)
	}{
		{"rate limit", func(e *ghemu.Emulator) { e.RateLimit("/repositories/", 1, time.Minute) }},
		{"secondary rate limit", func(e *ghemu.Emulator) { e.SecondaryRateLimit("/repositories/", 1) }},
		{"bad gateway", func(e *ghemu.Emulator) { e.ServerError("/repositories/", 502, 1) }},
		{"unavailable", func(e *ghemu.Emulator) { e.ServerError("/repositories/", 503, 1) }},
		{"dropped connection", func(e *ghemu.Emulator) {
			// Go's transport retries a request once on a reused connection
			// that drops, so drop every one.
			e.Faults().Add(fault.Rule{PathPrefix: "/repositories/", Respond: fault.Drop})
		}},
		{"no installation token", func(e *ghemu.Emulator) { e.ServerError("/app/installations/", 500, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			tc.inject(e.emu)
			_, err := e.c.RepositoryByID(ctx, 5001)
			if !github.IsUnavailable(err) || errors.Is(err, github.ErrNotFound) {
				t.Fatalf("error = %v, want Unavailable", err)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		e := setup(t)
		if _, err := e.c.RepositoryByID(ctx, 5001); err != nil { // get a token first
			t.Fatal(err)
		}
		e.emu.Faults().Add(fault.Rule{PathPrefix: "/repositories/", Delay: time.Minute})
		tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		if _, err := e.c.RepositoryByID(tctx, 5001); !github.IsUnavailable(err) {
			t.Fatalf("error = %v, want Unavailable", err)
		}
	})
}

func TestAppMustBeInstalledOnTheOrganization(t *testing.T) {
	emu := ghemu.New()
	defer emu.Close()
	org := emu.Org(orgID, "lanescape")
	emu.Repo(org, 5001, "app")
	app := emu.NewApp(77)
	emu.Install(app, emu.Org(1, "lanescape-lookalike"))
	_, err := client(t, emu, app).RepositoryByID(context.Background(), 5001)
	if err == nil || !github.IsUnavailable(err) {
		t.Fatalf("error = %v, want Unavailable", err)
	}
}

func TestWrongKeyIsRefused(t *testing.T) {
	e := setup(t)
	impostor := e.emu.NewApp(1)
	impostor.ID = e.app.ID // its key isn't the App's
	_, err := client(t, e.emu, impostor).RepositoryByID(context.Background(), 5001)
	if !github.IsUnavailable(err) {
		t.Fatalf("error = %v, want Unavailable", err)
	}
}

func TestCallsWaitingForATokenKeepTheirDeadlines(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	// Making the installation token hangs.
	e.emu.Faults().Add(fault.Rule{PathPrefix: "/app/installations/", Delay: time.Minute})
	first, cancelFirst := context.WithCancel(ctx)
	defer cancelFirst()
	go e.c.RepositoryByID(first, 5001)
	time.Sleep(100 * time.Millisecond)

	second, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := e.c.RepositoryByID(second, 5001); !github.IsUnavailable(err) {
		t.Fatalf("error = %v, want Unavailable", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a call waiting for another's token took %v, past its deadline", d)
	}
	cancelFirst()
	e.emu.Faults().Reset()
	if _, err := e.c.RepositoryByID(ctx, 5001); err != nil {
		t.Fatalf("after the slow token: %v", err)
	}
}

func TestTokensAreReused(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	for range 3 {
		if _, err := e.c.RepositoryByID(ctx, 5001); err != nil {
			t.Fatal(err)
		}
	}
	// Were a token made for each call, this would fail the next one.
	e.emu.ServerError("/app/installations/", 500, 0)
	if _, err := e.c.RepositoryByID(ctx, 5001); err != nil {
		t.Fatalf("call with a cached token: %v", err)
	}
}

// GitHub answers 404 when it won't make an installation token; that's no
// answer about what the call asked for.
func TestATokenRefusalIsNotANotFound(t *testing.T) {
	ctx := context.Background()
	for _, path := range []string{"/app/installations/", "/app/installations"} {
		e := setup(t)
		e.emu.ServerError(path, http.StatusNotFound, 1)
		repo := github.Repository{ID: 5001, Owner: "lanescape", Name: "app", DefaultBranch: "main"}
		_, err := e.c.BranchHead(ctx, repo, "main")
		if errors.Is(err, github.ErrNotFound) || !github.IsUnavailable(err) {
			t.Fatalf("%s refused: error = %v, want Unavailable", path, err)
		}
	}
}

func TestInstallationFoundOnALaterPage(t *testing.T) {
	emu := ghemu.New()
	defer emu.Close()
	app := emu.NewApp(77)
	for i := range 150 {
		emu.Install(app, emu.Org(int64(10+i), fmt.Sprintf("org%d", i)))
	}
	org := emu.Org(orgID, "lanescape")
	emu.Install(app, org)
	emu.Repo(org, 5001, "app")
	if _, err := client(t, emu, app).RepositoryByID(context.Background(), 5001); err != nil {
		t.Fatalf("installation past the first page: %v", err)
	}
}
