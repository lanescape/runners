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
	// A job GitHub's emulator finishes settles what it saved: the
	// emulator sees it at its next look.
	other := gh.Queue(run, ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	gh.RunJob(other)
	jtok := e.Token(e.JobOf(other, ""), nil)
	a, id := c.SaveIndex(jtok, "actions", linux, "build", key, 3, conformance.Sum([]byte("abc")))
	if a.Status != http.StatusOK {
		t.Fatalf("save answered %d: %s", a.Status, a.Reason)
	}
	if a := c.Commit(jtok, id, 3, conformance.Sum([]byte("abc"))); a.Status != http.StatusOK {
		t.Fatalf("commit answered %d: %s", a.Status, a.Reason)
	}
	gh.CompleteJob(other, "success")
	if es := e.Entries(e.RepositoryOf(repo)); len(es) != 1 || es[0].State != "ready" || es[0].Scope != want {
		t.Fatalf("entries %+v", es)
	}
}

// A closing the emulator never saw open again, because the test closed
// and reopened the pull request in GitHub between two of its requests,
// still retires what the pull request's earlier run saves, as the
// service's delivery of the closing would.
func TestAClosingSeenOnlyAfterItsReopeningCounts(t *testing.T) {
	e, gh, clock, c, repo := onGitHub(t)
	member := gh.User(8001, "member")
	repo.Commit("fix", map[string]string{"a": "1"})
	pr := gh.PullRequest(repo, "main", repo, "fix", member)
	run := gh.PullRequestRun(pr)
	wj := gh.Queue(run, ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	gh.RunJob(wj)
	tok := e.Token(e.JobOf(wj, ""), nil)
	if a := c.Bind(tok, nil); a.Scope() != fmt.Sprintf("pr%d", pr.Number) {
		t.Fatalf("bound %q (%s)", a.Scope(), a.Reason)
	}
	clock.Advance(2 * time.Second)
	gh.ClosePull(pr)
	gh.ReopenPull(pr)
	clock.Advance(2 * time.Second)
	_, id := c.SaveIndex(tok, "actions", linux, "build", key, 3, conformance.Sum([]byte("abc")))
	if a := c.Commit(tok, id, 3, conformance.Sum([]byte("abc"))); a.Status != http.StatusOK {
		t.Fatalf("commit answered %d: %s", a.Status, a.Reason)
	}
	gh.CompleteJob(wj, "success")
	if es := e.Entries(e.RepositoryOf(repo)); len(es) != 0 {
		t.Fatalf("a run bound before the closing kept %+v", es)
	}
}

func TestAnAutomaticRerunKeepsItsRunsChoicesOnGitHub(t *testing.T) {
	e, _, clock, c, repo := onGitHub(t)
	automaticRerun(t, e, clock, c, e.RepositoryOf(repo))
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

// A repository transferred out of the organization after a run restored
// is no consumer: the emulator's pass and its requests go on.
func TestATransferredRepositoryIsNoConsumer(t *testing.T) {
	e, gh, _, c, repo := onGitHub(t)
	run := gh.PushRun(repo, "main")
	wj := gh.Queue(run, ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	gh.RunJob(wj)
	tok := e.Token(e.JobOf(wj, ""), nil)
	if a := c.Bind(tok, nil); a.Scope() != "main" {
		t.Fatalf("bound %q", a.Scope())
	}
	if a, _ := c.Restore(tok, "actions", linux, "build", key, false); a.Status != http.StatusOK {
		t.Fatalf("restore answered %d", a.Status)
	}
	gh.Transfer(repo, gh.User(9100, "elsewhere"))
	e.Pass()
	if a, _ := c.Restore(tok, "actions", linux, "build", key, false); a.Status != http.StatusForbidden {
		t.Fatalf("a transferred repository's restore answered %d", a.Status)
	}
	if cs := e.Choices(e.RunOf(run)); len(cs) != 0 {
		t.Fatalf("choices %+v", cs)
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
			e.Approve(e.PullRequestTargetRun(e.PullRequest(r, "main", fmt.Sprintf("b%d", e.GitHub().NewID()))))
		})
	}
	wg.Wait()
}

// pullRun starts a job of a new pull_request run of pr, binds it, and
// returns its token.
func pullRun(t *testing.T, e *service.Emulator, gh *ghemu.Emulator, c *conformance.Client, pr *ghemu.Pull) (*ghemu.WorkflowJob, string) {
	t.Helper()
	wj := gh.Queue(gh.PullRequestRun(pr), ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	gh.RunJob(wj)
	tok := e.Token(e.JobOf(wj, ""), nil)
	if a := c.Bind(tok, nil); a.Scope() != fmt.Sprintf("pr%d", pr.Number) {
		t.Fatalf("bound %q (%s)", a.Scope(), a.Reason)
	}
	return wj, tok
}

func commitIndex(t *testing.T, c *conformance.Client, tok string) {
	t.Helper()
	_, id := c.SaveIndex(tok, "actions", linux, "build", key, 3, conformance.Sum([]byte("abc")))
	if a := c.Commit(tok, id, 3, conformance.Sum([]byte("abc"))); a.Status != http.StatusOK {
		t.Fatalf("commit answered %d: %s", a.Status, a.Reason)
	}
}

// A closing the emulator sees late counts from when GitHub closed the pull
// request, not from when the emulator looked: a run bound after it, once
// the service's time has moved on, keeps what it saves.
// Time moves on by GitHub's clock, which the service's follows, or by
// Advance, which moves the service's alone.
func TestALateSeenClosingCountsFromItsClosedAt(t *testing.T) {
	for _, clocks := range []string{"both", "the service's"} {
		t.Run(clocks, func(t *testing.T) {
			e, gh, clock, c, repo := onGitHub(t)
			repo.Commit("fix", map[string]string{"a": "1"})
			pr := gh.PullRequest(repo, "main", repo, "fix", gh.User(8001, "member"))
			gh.ClosePull(pr)
			gh.ReopenPull(pr)
			if clocks == "both" {
				clock.Advance(10 * time.Second)
			} else {
				e.Advance(10 * time.Second)
			}
			wj, tok := pullRun(t, e, gh, c, pr)
			commitIndex(t, c, tok)
			gh.CompleteJob(wj, "success")
			if es := e.Entries(e.RepositoryOf(repo)); len(es) != 1 || es[0].State != "ready" {
				t.Fatalf("a run bound after the closing kept %+v", es)
			}
		})
	}
}

// While GitHub is unavailable, a job GitHub shows finished settles
// nothing; once it answers again, its entries settle.
func TestAnOutageSettlesNothing(t *testing.T) {
	e, gh, _, c, repo := onGitHub(t)
	repo.Commit("fix", map[string]string{"a": "1"})
	pr := gh.PullRequest(repo, "main", repo, "fix", gh.User(8001, "member"))
	wj, tok := pullRun(t, e, gh, c, pr)
	commitIndex(t, c, tok)
	e.Unavailable(true)
	gh.CompleteJob(wj, "failure")
	e.Pass()
	if es := e.Entries(e.RepositoryOf(repo)); len(es) != 1 || es[0].State != "pending" {
		t.Fatalf("during an outage, entries %+v", es)
	}
	e.Unavailable(false)
	e.Pass()
	if es := e.Entries(e.RepositoryOf(repo)); len(es) != 0 {
		t.Fatalf("after the outage, entries %+v", es)
	}
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
