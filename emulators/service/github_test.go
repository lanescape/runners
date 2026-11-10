package service_test

import (
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/conformance"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/endpoints"
)

const bootstrap = "lanescape/app/.github/workflows/bootstrap.yml@refs/heads/main"

// onGitHub starts a GitHub emulator, with the organization and a
// repository whose default branch lists a bootstrap, as a consumer's test
// would, and the service emulator, which takes its view of GitHub from it.
func onGitHub(t *testing.T) (*service.Emulator, *ghemu.Emulator, *clockwork.FakeClock, *conformance.Client, *ghemu.Repo) {
	t.Helper()
	clock := clockwork.NewFakeClockAt(time.Now())
	gh := ghemu.New(ghemu.WithClock(clock))
	t.Cleanup(gh.Close)
	org := gh.Org(endpoints.Organization, "lanescape")
	repo := gh.Repo(org, 7001, "app")
	repo.Commit("main", map[string]string{".github/runners.json": `{"bootstrap": "` + bootstrap + `"}`})
	e := service.New(service.WithGitHub(gh), service.WithClock(clock))
	t.Cleanup(e.Close)
	return e, gh, clock, &conformance.Client{T: t, URL: e.URL()}, repo
}

// A test that runs GitHub's emulator binds a contribution with the head
// commit GitHub's own event names, with the token GitHub's emulator mints,
// and nothing translated between the two.
func TestOneGitHubServesTheTestAndTheEmulator(t *testing.T) {
	e, gh, _, c, repo := onGitHub(t)
	contributor := gh.User(9001, "contributor")
	fork := gh.Fork(repo, contributor, 9002)
	fork.Commit("change", map[string]string{"a": "1"})
	pr := gh.PullRequest(repo, "main", fork, "change", contributor)
	run := gh.PullRequestTargetRun(pr)
	// The contributor pushes again after the run was approved.
	gh.Approve(run, "contribution", gh.User(8001, "maintainer"))
	fork.Commit("change", map[string]string{"a": "2"})
	gh.Sync(pr)
	wj := gh.Queue(run, ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	gh.RunJob(wj)
	tok := gh.JobToken(wj, ghemu.Job{Audience: e.URL(), WorkflowRef: bootstrap}, nil)
	ev := run.EventPull
	if ev.HeadSHA == gh.PullHead(pr) {
		t.Fatal("the pull request didn't move")
	}
	want := fmt.Sprintf("pr%d", pr.Number)
	if a := c.Bind(tok, &conformance.PullRequestEvent{PullRequest: ev.Number, HeadRepositoryID: ev.HeadRepositoryID, HeadSHA: ev.HeadSHA}); a.Status != http.StatusOK || a.Scope() != want {
		t.Fatalf("bound %d %q (%s)", a.Status, a.Scope(), a.Reason)
	}
	if s, ok := e.Binding(e.RunOf(run)); !ok || s != want {
		t.Fatalf("the emulator keeps %q, %v", s, ok)
	}
	// Once GitHub's emulator shows the run finished, the emulator sees it
	// at its next request, and binds nothing more of it.
	gh.CompleteJob(wj, "success")
	if a := c.Bind(tok, nil); a.Status != http.StatusForbidden {
		t.Fatalf("a finished run's bind answered %d", a.Status)
	}
}

// A pull request closed in GitHub's emulator no longer counts among the
// open pull requests whose heads a binding compares, as GitHub's list of
// them shows.
func TestAClosedPullRequestsHeadIsNoLongerOpen(t *testing.T) {
	e, gh, _, c, repo := onGitHub(t)
	member := gh.User(8001, "member")
	repo.Commit("fix", map[string]string{"a": "1"})
	pr := gh.PullRequest(repo, "main", repo, "fix", member)
	repo.Commit("release", map[string]string{"r": "1"})
	twin := gh.PullRequest(repo, "release", repo, "fix", member)
	if gh.PullHead(pr) != gh.PullHead(twin) {
		t.Fatal("the pull requests have different heads")
	}
	bind := func() string {
		wj := gh.Queue(gh.PullRequestRun(pr), ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
		gh.RunJob(wj)
		a := c.Bind(e.Token(e.JobOf(wj, ""), nil), nil)
		if a.Status != http.StatusOK {
			t.Fatalf("bind answered %d: %s", a.Status, a.Reason)
		}
		return a.Scope()
	}
	if s := bind(); s != "read" {
		t.Fatalf("a head two open pull requests share bound %q", s)
	}
	gh.ClosePull(twin)
	if s := bind(); s != fmt.Sprintf("pr%d", pr.Number) {
		t.Fatalf("with the other closed, bound %q", s)
	}
}

// World's handles name GitHub's own objects.
func TestHandlesNameGitHubsObjects(t *testing.T) {
	e, gh, _, _, repo := onGitHub(t)
	r := e.RepositoryOf(repo)
	if e.GitHubRepo(r) != repo {
		t.Fatal("the repository isn't GitHub's")
	}
	pr := e.ForkPullRequest(r, "main")
	p := e.GitHubPull(pr)
	if p.Head == nil || p.Head.ID != pr.HeadRepositoryID || gh.PullHead(p) != pr.HeadSHA {
		t.Fatal("the pull request isn't GitHub's")
	}
	pushed := e.PushPullRequest(pr)
	if gh.PullHead(p) != pushed.HeadSHA || pushed.HeadSHA == pr.HeadSHA {
		t.Fatal("the push isn't GitHub's")
	}
	run := e.PullRequestTargetRun(pushed)
	if gr := e.GitHubRun(run); gr.EventPull.HeadSHA != pushed.HeadSHA || run.PullRequest.HeadSHA != pushed.HeadSHA {
		t.Fatal("the run isn't GitHub's")
	}
	j := e.StartJob(run, e.Bootstrap(r))
	if wj := e.GitHubJob(j); wj.Run != e.GitHubRun(run) || gh.Job(j.ID).Status != "in_progress" {
		t.Fatal("the job isn't GitHub's")
	}
	if got := e.JobOf(e.GitHubJob(j), j.WorkflowRef); !reflect.DeepEqual(got, j) {
		t.Fatalf("JobOf = %+v, want %+v", got, j)
	}
}

// A repository transferred out of the organization after a run was bound
// is no consumer: its later requests are refused.
func TestATransferredRepositoryIsNoConsumer(t *testing.T) {
	e, gh, _, c, repo := onGitHub(t)
	run := gh.PushRun(repo, "main")
	wj := gh.Queue(run, ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	gh.RunJob(wj)
	tok := e.Token(e.JobOf(wj, ""), nil)
	if a := c.Bind(tok, nil); a.Scope() != "main" {
		t.Fatalf("bound %q", a.Scope())
	}
	gh.Transfer(repo, gh.User(9100, "elsewhere"))
	if a := c.Bind(tok, nil); a.Status != http.StatusForbidden {
		t.Fatalf("a transferred repository's bind answered %d", a.Status)
	}
}

// A fork the organization owns is one of its repositories.
func TestAnOrganizationsForkIsItsRepository(t *testing.T) {
	e, gh, _, c, repo := onGitHub(t)
	fork := gh.Fork(repo, gh.Account(endpoints.Organization), 7002)
	run := gh.PushRun(fork, "main")
	wj := gh.Queue(run, ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	gh.RunJob(wj)
	if a := c.Bind(e.Token(e.JobOf(wj, ""), nil), nil); a.Status != http.StatusOK || a.Scope() != "main" {
		t.Fatalf("bound %d %q (%s)", a.Status, a.Scope(), a.Reason)
	}
}

// World's methods may be called at once.
func TestWorldsMethodsRunTogether(t *testing.T) {
	e, _, _, _, repo := onGitHub(t)
	r := e.RepositoryOf(repo)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			e.StartJob(e.PullRequestTargetRun(e.PullRequest(r, "main", fmt.Sprintf("b%d", e.GitHub().NewID()))), e.Bootstrap(r))
		})
	}
	wg.Wait()
}

// Repositories made at once share the organization's one account.
func TestRepositoriesMadeAtOnceShareTheOrganization(t *testing.T) {
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	e := service.New(service.WithGitHub(gh))
	t.Cleanup(e.Close)
	var wg sync.WaitGroup
	repos := make([]service.Repository, 8)
	for i := range repos {
		wg.Go(func() { repos[i] = e.Repository(fmt.Sprintf("app%d", i)) })
	}
	wg.Wait()
	org := gh.Account(endpoints.Organization)
	for _, r := range repos {
		if e.GitHubRepo(r).Owner != org {
			t.Fatalf("%s has another organization's account", r.Name)
		}
	}
}
