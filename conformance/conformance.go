// Package conformance is the interface's test suite (specs/0005-interface.md):
// one set of cases that this repository runs, in CI, against both its
// service emulator, emulators/service, and the real service wired to its
// emulators of GitHub, Google's issuers, Pub/Sub and Cloud Storage. The two
// must answer every case alike, so a consumer that tests against the
// emulator, at a pinned version of this module, tests against what the
// service does. It's public so that consumers can read what the emulator is
// held to.
package conformance

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/lanescape/runners/emulators/service"
)

// Case is one case of the suite.
type Case struct {
	Name string
	Run  func(t T, w service.World)
}

// one is a case.
func one(name string, run func(t T, w *world)) []Case {
	return []Case{{Name: name, Run: func(t T, sw service.World) { run(t, with(t, sw)) }}}
}

// Run runs every case, each against a world of its own from newWorld.
func Run(t *testing.T, newWorld func(t *testing.T) service.World) {
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, newWorld(t))
		})
	}
}

// Cases are the suite's cases.
func Cases() []Case {
	return slices.Concat(
		one("scopes", scopes),
		one("a run is bound once", boundOnce),
		one("bind's refusals and retries", bindErrors),
		one("forks and contributions", contributions),
		one("a contribution pushed to after its approval", pushedContribution),
		one("a pull request's binding and its commits", pullCommits),
		one("a contribution's token can't widen its scope", noWidening),
	)
}

// world is a case's world, with a client for its service.
type world struct {
	service.World
	t T
	c *Client
}

func with(t T, w service.World) *world {
	return &world{World: w, t: t, c: &Client{T: t, URL: w.URL()}}
}

// bound binds a run with a new job, as the runners action does, and checks
// the scope.
func (w *world) bound(run service.Run, want string) {
	w.t.Helper()
	j := w.StartJob(run, "")
	if a := w.c.Bind(w.Token(j, nil), nil); a.Status != http.StatusOK || a.Scope() != want {
		w.t.Fatalf("bound %d %q (%s), want %s", a.Status, a.Scope(), a.Reason, want)
	}
}

// bootstrap binds a pull_request_target run with a job of its bootstrap,
// which sends what ev says, and checks the scope.
func (w *world) bootstrap(repo service.Repository, run service.Run, ev *PullRequestEvent, want string) {
	w.t.Helper()
	j := w.StartJob(run, w.Bootstrap(repo))
	if a := w.c.Bind(w.Token(j, nil), ev); a.Status != http.StatusOK || a.Scope() != want {
		w.t.Fatalf("the bootstrap bound %d %q (%s), want %s", a.Status, a.Scope(), a.Reason, want)
	}
}

// eventOf is what a bootstrap sends of a pull request from its event.
func eventOf(pr service.PullRequest) *PullRequestEvent {
	return &PullRequestEvent{PullRequest: pr.Number, HeadRepositoryID: pr.HeadRepositoryID, HeadSHA: pr.HeadSHA}
}

func scopes(t T, w *world) {
	repo := w.Repository("app")
	w.SetRunners(repo, fmt.Sprintf(`{"bootstrap": %q, "branches": {"support/v1": "v1"}}`, w.Bootstrap(repo)))
	w.Branch(repo, "support/v1")
	w.Branch(repo, "feature")
	w.bound(w.Push(repo, "main"), "main")
	w.bound(w.Push(repo, "support/v1"), "branch-v1")
	// A listed branch that's gone binds nothing of its own.
	w.Branch(repo, "support/v2")
	w.SetRunners(repo, fmt.Sprintf(`{"bootstrap": %q, "branches": {"support/v1": "v1", "support/v2": "v2"}}`, w.Bootstrap(repo)))
	gone := w.Push(repo, "support/v2")
	w.DeleteBranch(repo, "support/v2")
	w.bound(gone, "read")
	// Nor does one made again from the default branch, for a commit of
	// the branch that went.
	old := w.Push(repo, "support/v1")
	w.DeleteBranch(repo, "support/v1")
	w.Branch(repo, "support/v1")
	w.bound(old, "read")
	w.bound(w.Push(repo, "support/v1"), "branch-v1")
	w.bound(w.Push(repo, "feature"), "read")
	w.bound(w.Tag(repo, "v1"), "read")
	w.bound(w.Event(repo, "workflow_dispatch", "main"), "read")
	pr := w.PullRequest(repo, "main", "fix")
	w.bound(w.PullRequestRun(pr), fmt.Sprintf("pr%d", pr.Number))
	into := w.PullRequest(repo, "support/v1", "backport")
	w.bound(w.PullRequestRun(into), fmt.Sprintf("pr%d", into.Number))
	fork := w.ForkPullRequest(repo, "main")
	w.bound(w.PullRequestRun(fork), "read")

	// A pull_request_target run is a pull request's only from its
	// bootstrap, with what its event says.
	run := w.PullRequestTargetRun(fork)
	j := w.StartJob(run, w.Bootstrap(repo))
	ev := &PullRequestEvent{PullRequest: fork.Number, HeadRepositoryID: fork.HeadRepositoryID, HeadSHA: fork.HeadSHA}
	if a := w.c.Bind(w.Token(j, nil), ev); a.Status != http.StatusOK || a.Scope() != fmt.Sprintf("pr%d", fork.Number) {
		t.Fatalf("the bootstrap bound %d %q (%s)", a.Status, a.Scope(), a.Reason)
	}
	other := w.PullRequestTargetRun(fork)
	j = w.StartJob(other, w.Bootstrap(repo))
	bad := *ev
	bad.HeadSHA = strings.Repeat("a", 40)
	if a := w.c.Bind(w.Token(j, nil), &bad); a.Status != http.StatusOK || a.Scope() != "read" {
		t.Fatalf("a bootstrap naming another head commit bound %d %q", a.Status, a.Scope())
	}
	// A push to main signed as another event binds read.
	run = w.Push(repo, "main")
	j = w.StartJob(run, "")
	if a := w.c.Bind(w.Token(j, map[string]any{"event_name": "workflow_dispatch"}), nil); a.Status != http.StatusOK || a.Scope() != "read" {
		t.Fatalf("a token whose event isn't the run's bound %d %q", a.Status, a.Scope())
	}
	if s, ok := w.Binding(run); !ok || s != "read" {
		t.Fatalf("the binding the service keeps is %q, %v", s, ok)
	}
}

func boundOnce(t T, w *world) {
	repo := w.Repository("app")
	w.SetRunners(repo, fmt.Sprintf(`{"bootstrap": %q}`, w.Bootstrap(repo)))
	run := w.Push(repo, "main")
	w.bound(run, "main")
	// Every later call, from any of the run's jobs, gets the same.
	j := w.StartJob(run, "")
	if a := w.c.Bind(w.Token(j, map[string]any{"ref": "refs/heads/feature"}), nil); a.Scope() != "main" {
		t.Fatalf("a later call got %q", a.Scope())
	}
	// A contribution's run whose first call isn't its bootstrap's is read
	// for good: the bootstrap's later call gets read too.
	fork := w.ForkPullRequest(repo, "main")
	prt := w.PullRequestTargetRun(fork)
	w.bound(prt, "read")
	j = w.StartJob(prt, w.Bootstrap(repo))
	ev := &PullRequestEvent{PullRequest: fork.Number, HeadRepositoryID: fork.HeadRepositoryID, HeadSHA: fork.HeadSHA}
	if a := w.c.Bind(w.Token(j, nil), ev); a.Status != http.StatusOK || a.Scope() != "read" {
		t.Fatalf("a bootstrap after another call bound %d %q", a.Status, a.Scope())
	}
	// A maintainer's rerun of a finished run keeps its binding: GitHub
	// shows the run in progress again, and its new attempt's jobs get the
	// same.
	done := w.Push(repo, "main")
	j = w.StartJob(done, "")
	if a := w.c.Bind(w.Token(j, nil), nil); a.Scope() != "main" {
		t.Fatalf("bound %q (%s)", a.Scope(), a.Reason)
	}
	w.FinishJob(j, "success")
	again := w.Rerun(done)
	if again.Attempt != 2 {
		t.Fatalf("the rerun is attempt %d", again.Attempt)
	}
	j = w.StartJob(again, "")
	if a := w.c.Bind(w.Token(j, map[string]any{"ref": "refs/heads/feature"}), nil); a.Status != http.StatusOK || a.Scope() != "main" {
		t.Fatalf("a rerun's job bound %d %q (%s)", a.Status, a.Scope(), a.Reason)
	}
}

func bindErrors(t T, w *world) {
	repo := w.Repository("app")
	run := w.Push(repo, "main")
	j := w.StartJob(run, "")
	for name, a := range map[string]Answer{
		"no token":           w.c.BindRaw("", nil),
		"another audience":   w.c.Bind(w.Token(j, map[string]any{"aud": "https://example.com"}), nil),
		"another owner":      w.c.Bind(w.Token(j, map[string]any{"repository_owner_id": "1"}), nil),
		"a body of null":     w.c.BindRaw(w.Token(j, nil), []byte("null")),
		"a body of a list":   w.c.BindRaw(w.Token(j, nil), []byte("[]")),
		"an unknown field":   w.c.BindRaw(w.Token(j, nil), []byte(`{"scope": "main"}`)),
		"two objects":        w.c.BindRaw(w.Token(j, nil), []byte(`{} {}`)),
		"no such run":        w.c.Bind(w.Token(j, map[string]any{"run_id": "1"}), nil),
		"a run of elsewhere": w.c.Bind(w.Token(j, map[string]any{"repository_id": "2"}), nil),
	} {
		if a.Status != http.StatusForbidden {
			t.Errorf("%s: bind answered %d", name, a.Status)
		}
	}
	if _, ok := w.Binding(run); ok {
		t.Fatalf("a refused call bound the run")
	}
	w.Unavailable(true)
	a := w.c.Bind(w.Token(j, nil), nil)
	w.Unavailable(false)
	if a.Status != http.StatusServiceUnavailable {
		t.Fatalf("bind with GitHub unavailable answered %d", a.Status)
	}
	if _, ok := w.Binding(run); ok {
		t.Fatalf("an unconfirmed call bound the run")
	}
	w.bound(run, "main")
	// A finished run binds nothing.
	done := w.Push(repo, "main")
	j = w.StartJob(done, "")
	w.FinishJob(j, "success")
	if a := w.c.Bind(w.Token(j, nil), nil); a.Status != http.StatusForbidden {
		t.Fatalf("a finished run's bind answered %d", a.Status)
	}
}

func contributions(t T, w *world) {
	repo := w.Repository("app")
	w.SetRunners(repo, fmt.Sprintf(`{"bootstrap": %q}`, w.Bootstrap(repo)))
	fork := w.ForkPullRequest(repo, "main")
	want := fmt.Sprintf("pr%d", fork.Number)
	// A fork's pull_request run gets nothing of its pull request's.
	w.bound(w.PullRequestRun(fork), "read")
	// A contribution's run gets its pull request's scope from its
	// bootstrap, and each later run gets it again from its own.
	for range 2 {
		prt := w.PullRequestTargetRun(fork)
		w.bootstrap(repo, prt, eventOf(fork), want)
		// The run's other jobs get the same.
		w.bound(prt, want)
	}
	// A pull_request_target run's own workflow, rather than its bootstrap,
	// binds read, whatever it sends.
	prt := w.PullRequestTargetRun(fork)
	j := w.StartJob(prt, "")
	if a := w.c.Bind(w.Token(j, nil), eventOf(fork)); a.Status != http.StatusOK || a.Scope() != "read" {
		t.Fatalf("a contribution's own workflow bound %d %q", a.Status, a.Scope())
	}
	// A bootstrap naming another fork's head for the pull request, or a
	// pull request that isn't there, binds read.
	bad := eventOf(w.ForkPullRequest(repo, "main"))
	bad.PullRequest = fork.Number
	w.bootstrap(repo, w.PullRequestTargetRun(fork), bad, "read")
	bad = eventOf(fork)
	bad.PullRequest = 999
	w.bootstrap(repo, w.PullRequestTargetRun(fork), bad, "read")
}

// pushedContribution is an approved contribution whose contributor pushed
// again after its run was approved: the run's bootstrap names the head its
// event has, which is still one of the pull request's commits, and binds
// the pull request's scope, as specs/0002-machines.md says.
func pushedContribution(t T, w *world) {
	repo := w.Repository("app")
	w.SetRunners(repo, fmt.Sprintf(`{"bootstrap": %q}`, w.Bootstrap(repo)))
	fork := w.ForkPullRequest(repo, "main")
	prt := w.PullRequestTargetRun(fork)
	pushed := w.PushPullRequest(fork)
	if pushed.HeadSHA == fork.HeadSHA || pushed.Number != fork.Number || pushed.HeadRepositoryID != fork.HeadRepositoryID {
		t.Fatalf("the push didn't move the pull request's head")
	}
	want := fmt.Sprintf("pr%d", fork.Number)
	w.bootstrap(repo, prt, eventOf(fork), want)
	if s, ok := w.Binding(prt); !ok || s != want {
		t.Fatalf("the binding the service keeps is %q, %v", s, ok)
	}
	// The next run's bootstrap names the new head, and gets the same
	// scope.
	w.bootstrap(repo, w.PullRequestTargetRun(pushed), eventOf(pushed), want)
}

// pullCommits holds a pull request's binding to its commits: a
// pull_request run's head must still be its pull request's when it binds,
// and a bootstrap's head must be one of its pull request's commits.
func pullCommits(t T, w *world) {
	repo := w.Repository("app")
	w.SetRunners(repo, fmt.Sprintf(`{"bootstrap": %q}`, w.Bootstrap(repo)))
	pr := w.PullRequest(repo, "main", "fix")
	other := w.PullRequest(repo, "main", "other")
	want := fmt.Sprintf("pr%d", pr.Number)
	// A pull_request run made before a push binds read: its head is no
	// longer its pull request's.
	early := w.PullRequestRun(pr)
	pushed := w.PushPullRequest(pr)
	w.bound(early, "read")
	w.bound(w.PullRequestRun(pushed), want)
	// A bootstrap naming its pull request's earlier head, or its head now,
	// binds the pull request's scope.
	for _, head := range []string{pr.HeadSHA, pushed.HeadSHA} {
		run := w.PullRequestTargetRun(pushed)
		j := w.StartJob(run, w.Bootstrap(repo))
		ev := &PullRequestEvent{PullRequest: pr.Number, HeadRepositoryID: repo.ID, HeadSHA: head}
		if a := w.c.Bind(w.Token(j, nil), ev); a.Status != http.StatusOK || a.Scope() != want {
			t.Fatalf("a bootstrap naming one of its pull request's commits bound %d %q (%s)", a.Status, a.Scope(), a.Reason)
		}
	}
	// One naming a commit of the repository that was never one of its
	// pull request's binds read: another pull request's head.
	run := w.PullRequestTargetRun(pushed)
	j := w.StartJob(run, w.Bootstrap(repo))
	ev := &PullRequestEvent{PullRequest: pr.Number, HeadRepositoryID: repo.ID, HeadSHA: other.HeadSHA}
	if a := w.c.Bind(w.Token(j, nil), ev); a.Status != http.StatusOK || a.Scope() != "read" {
		t.Fatalf("a bootstrap naming another pull request's head bound %d %q", a.Status, a.Scope())
	}
	// A commit its head branch had before the pull request was opened is
	// one of its commits too, unless its base has it.
	w.Branch(repo, "release")
	first := w.PullRequest(repo, "main", "feature")
	second := w.PullRequest(repo, "release", "feature")
	run = w.PullRequestTargetRun(second)
	j = w.StartJob(run, w.Bootstrap(repo))
	ev = &PullRequestEvent{PullRequest: second.Number, HeadRepositoryID: repo.ID, HeadSHA: first.HeadSHA}
	if a := w.c.Bind(w.Token(j, nil), ev); a.Status != http.StatusOK || a.Scope() != fmt.Sprintf("pr%d", second.Number) {
		t.Fatalf("a bootstrap naming its head branch's earlier commit bound %d %q (%s)", a.Status, a.Scope(), a.Reason)
	}
	// Nor does a fork's earlier head, for another pull request.
	fork := w.ForkPullRequest(repo, "main")
	w.PushPullRequest(fork)
	run = w.PullRequestTargetRun(other)
	j = w.StartJob(run, w.Bootstrap(repo))
	ev = &PullRequestEvent{PullRequest: other.Number, HeadRepositoryID: fork.HeadRepositoryID, HeadSHA: fork.HeadSHA}
	if a := w.c.Bind(w.Token(j, nil), ev); a.Status != http.StatusOK || a.Scope() != "read" {
		t.Fatalf("a bootstrap naming a fork's commit for another pull request bound %d %q", a.Status, a.Scope())
	}
}

// noWidening holds a contribution's job, whose token the contributor's code
// can mint while it runs, to what its run already has (specs/0001-architecture.md,
// Forks and contributions): /v1/bind is the only request a GitHub token is
// good for, and it answers the run's binding, never another. A
// pull_request_target run's token names the default branch's ref, which
// binds nothing of main's.
func noWidening(t T, w *world) {
	repo := w.Repository("app")
	w.SetRunners(repo, fmt.Sprintf(`{"bootstrap": %q}`, w.Bootstrap(repo)))
	fork := w.ForkPullRequest(repo, "main")
	want := fmt.Sprintf("pr%d", fork.Number)
	bodies := [][]byte{nil, []byte(`{}`), marshal(eventOf(fork)),
		[]byte(`{"pull_request": 1, "head_repository_id": 1, "head_sha": "` + strings.Repeat("b", 40) + `"}`)}
	// The contributor's job's token, whatever it sends, gets the binding
	// its run's bootstrap made, never another.
	prt := w.PullRequestTargetRun(fork)
	w.bootstrap(repo, prt, eventOf(fork), want)
	cj := w.StartJob(prt, "")
	for _, body := range bodies {
		if a := w.c.BindRaw(w.Token(cj, nil), body); a.Status != http.StatusOK || a.Scope() != want {
			t.Fatalf("a contribution's call bound %d %q", a.Status, a.Scope())
		}
	}
	// Nor does what its token says of its ref or event change that.
	for _, o := range []map[string]any{{"ref": "refs/heads/main"}, {"ref": "refs/heads/main", "event_name": "push"}} {
		if a := w.c.Bind(w.Token(cj, o), nil); a.Status != http.StatusOK || a.Scope() != want {
			t.Fatalf("a contribution's call claiming %v bound %d %q", o, a.Status, a.Scope())
		}
	}
	// A contribution's job that calls before its run's bootstrap, as one
	// that doesn't wait for the bootstrap could, binds its run read, never
	// main's, though its token's ref is the default branch's; and the
	// bootstrap can't change that afterwards.
	early := w.PullRequestTargetRun(fork)
	ej := w.StartJob(early, "")
	for _, body := range bodies {
		if a := w.c.BindRaw(w.Token(ej, nil), body); a.Status != http.StatusOK || a.Scope() != "read" {
			t.Fatalf("a contribution's call before its bootstrap bound %d %q", a.Status, a.Scope())
		}
	}
	w.bootstrap(repo, early, eventOf(fork), "read")
	if s, _ := w.Binding(prt); s != want {
		t.Fatalf("the run's binding became %q", s)
	}
}
