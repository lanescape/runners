// Package conformance is the interface's test suite (specs/0005-interface.md):
// one set of cases that this repository runs, in CI, against both its
// service emulator, emulators/service, and the real service wired to its
// emulators of GitHub, Google's issuers and Cloud Storage. The two must
// answer every case alike, so a consumer that tests against the emulator,
// at a pinned version of this module, tests against what the service does.
// It's public so that consumers can read what the emulator is held to.
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

// IndexStore is the consumer's store the cases use: an index, whose
// entries hold no bytes.
const IndexStore = "actions"

// both is a case run once in an index store, from jobs on any runner, and
// once in the service's own store, from jobs on the service's machines.
func both(name string, run func(t T, w *world)) []Case {
	var out []Case
	for _, store := range []string{IndexStore, ServiceStore} {
		out = append(out, Case{Name: name + " (" + store + " store)", Run: func(t T, sw service.World) {
			w := with(t, sw)
			w.store = store
			run(t, w)
		}})
	}
	return out
}

// one is a case that runs once, in the store it chooses.
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
		both("the restore order", restoreOrder),
		both("a long-lived branch's restore order", branchOrder),
		both("platforms are kept apart", platforms),
		both("a lookup records the run's choice", lookup),
		both("a choice holds for its run", choiceHolds),
		both("a maintainer's rerun picks again", rerun),
		both("only a successful job's entries are restored", successGating),
		both("read runs restore main's and save nothing", readRuns),
		both("a closed pull request's entries are never restored again", closing),
		both("forks and contributions", contributions),
		both("a contribution's token can't widen its scope", noWidening),
		both("the cache's refusals and retries", cacheErrors),
		one("stores are kept apart", stores),
		one("the service's store is its machines' alone", serviceStore),
		one("an index's entries hold no bytes", indexEntries),
		one("uploads resume and downloads take ranges", transfers),
	)
}

const (
	linux = "linux-x64"
	mac   = "macos-arm64"
	key1  = "0123456789abcdef0123"
	key2  = "fedcba9876543210fedc"
	key3  = "00112233445566778899"
)

// world is a case's world, with a client for its service, and the store
// its cache's cases use.
type world struct {
	service.World
	t     T
	c     *Client
	store string
	rid   string
}

func with(t T, w service.World) *world {
	return &world{World: w, t: t, c: &Client{T: t, URL: w.URL()}}
}

// job starts a job of run and returns its token: on one of the service's
// machines if the case uses the service's store, or else on any runner.
func (w *world) job(run service.Run) (service.Job, string) {
	j := w.StartJob(run, "")
	if w.store == ServiceStore {
		j = w.StartMachineJob(run)
	}
	return j, w.Token(j, nil)
}

// rs restores in the case's store.
func (w *world) rs(token, platform, family, key string, lookup bool) (Answer, *Restored) {
	w.t.Helper()
	return w.c.Restore(token, w.store, platform, family, key, lookup)
}

// reserve saves an entry of data in the case's store, short of its commit:
// in the service's store, it uploads the bytes; in an index, it records
// their size and SHA-256. It returns the reservation's ID, or "" and the
// refusal.
func (w *world) reserve(token, platform, family, key string, data []byte) (string, Answer) {
	w.t.Helper()
	if w.store != ServiceStore {
		a, id := w.c.SaveIndex(token, w.store, platform, family, key, int64(len(data)), Sum(data))
		return id, a
	}
	a, r := w.c.Save(token, platform, family, key)
	if r == nil {
		return "", a
	}
	w.c.Upload(r, data, 256<<10, false)
	return r.ID, a
}

// bound binds a run with its first job, as the runners action does, and
// checks the scope.
func (w *world) bound(run service.Run, want string) {
	w.t.Helper()
	_, tok := w.job(run)
	if a := w.c.Bind(tok, nil); a.Status != http.StatusOK || a.Scope() != want {
		w.t.Fatalf("bound %d %q (%s), want %s", a.Status, a.Scope(), a.Reason, want)
	}
}

// save saves data as an entry from a new job of run, and finishes the job
// with conclusion.
func (w *world) save(run service.Run, platform, family, key string, data []byte, conclusion string) {
	w.t.Helper()
	j, tok := w.job(run)
	id, a := w.reserve(tok, platform, family, key, data)
	if a.Status != http.StatusOK {
		w.t.Fatalf("save answered %d: %s", a.Status, a.Reason)
	}
	if a := w.c.Commit(tok, id, int64(len(data)), Sum(data)); a.Status != http.StatusOK {
		w.t.Fatalf("commit answered %d: %s", a.Status, a.Reason)
	}
	w.FinishJob(j, conclusion)
}

// mainSaves saves data from a push to main.
func (w *world) mainSaves(repo service.Repository, platform, key string, data []byte) {
	w.t.Helper()
	run := w.Push(repo, "main")
	w.bound(run, "main")
	w.save(run, platform, "build", key, data, "success")
}

// restore restores as a new job of run, and checks what it got: the step,
// or none if step is "".
func (w *world) restore(run service.Run, platform, key, step string, data []byte) *Restored {
	w.t.Helper()
	_, tok := w.job(run)
	a, r := w.rs(tok, platform, "build", key, false)
	if a.Status != http.StatusOK {
		w.t.Fatalf("restore answered %d: %s", a.Status, a.Reason)
	}
	switch {
	case step == "" && r != nil:
		w.t.Fatalf("restored %s's %s from step %s, want nothing", r.Scope, r.Key, r.Step)
	case step == "":
		return nil
	case r == nil:
		w.t.Fatalf("restored nothing, want step %s", step)
	case r.Step != step:
		w.t.Fatalf("restored from step %s, want %s", r.Step, step)
	}
	if w.store != ServiceStore {
		// The job checks the bytes in its own runner's cache against what
		// the index says.
		if data != nil && (r.SHA256 != Sum(data) || r.Size != int64(len(data))) {
			w.t.Fatalf("restored another entry")
		}
		return r
	}
	if got := w.c.Download(r, 100<<10); data != nil && !slices.Equal(got, data) {
		w.t.Fatalf("restored other bytes")
	}
	return r
}

func data(s string) []byte { return []byte(strings.Repeat(s, 64<<10/len(s)+1)) }

func scopes(t T, w *world) {
	repo := w.Repository("app")
	w.SetRunners(repo, fmt.Sprintf(`{"bootstrap": %q, "branches": {"support/v1": "v1"}}`, w.Bootstrap(repo)))
	w.Branch(repo, "support/v1")
	w.Branch(repo, "feature")
	w.bound(w.Push(repo, "main"), "main")
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

func restoreOrder(t T, w *world) {
	repo := w.Repository("app")
	main1, main2 := data("main1"), data("main2")
	w.mainSaves(repo, linux, key1, main1)
	w.mainSaves(repo, linux, key2, main2)
	pr := w.PullRequest(repo, "main", "fix")
	run := w.PullRequestRun(pr)
	w.bound(run, fmt.Sprintf("pr%d", pr.Number))
	w.restore(run, linux, key1, "main_exact", main1)
	w.restore(run, linux, key3, "main_newest", main2)
	own := data("pull request")
	w.save(run, linux, "build", key3, own, "success")
	next := w.PullRequestRun(pr)
	w.bound(next, fmt.Sprintf("pr%d", pr.Number))
	w.restore(next, linux, key3, "pull_request_exact", own)
	r := w.restore(next, linux, key1, "pull_request_newest", own)
	if r.Scope != fmt.Sprintf("pr%d", pr.Number) || r.Key != key3 {
		t.Fatalf("restored %s's %s", r.Scope, r.Key)
	}
	// Each choice records its step.
	steps := map[string]string{}
	for _, c := range w.Choices(next) {
		steps[c.Key] = c.Step
	}
	if steps[key3] != "pull_request_exact" || steps[key1] != "pull_request_newest" {
		t.Fatalf("choices %v", w.Choices(next))
	}
	// Main's own runs read main's alone.
	push := w.Push(repo, "main")
	w.bound(push, "main")
	w.restore(push, linux, key3, "main_newest", main2)
}

func branchOrder(t T, w *world) {
	repo := w.Repository("app")
	w.SetRunners(repo, `{"branches": {"support/v1": "v1"}}`)
	w.Branch(repo, "support/v1")
	main1 := data("main1")
	w.mainSaves(repo, linux, key1, main1)
	// A new branch starts warm, from main's.
	push := w.Push(repo, "support/v1")
	w.bound(push, "branch-v1")
	w.restore(push, linux, key1, "main_exact", main1)
	v1 := data("v1")
	w.save(push, linux, "build", key2, v1, "success")
	next := w.Push(repo, "support/v1")
	w.bound(next, "branch-v1")
	w.restore(next, linux, key2, "branch_exact", v1)
	w.restore(next, linux, key3, "branch_newest", v1)
	// A pull request into the branch reads its own, then the branch's,
	// never main's.
	pr := w.PullRequest(repo, "support/v1", "backport")
	run := w.PullRequestRun(pr)
	w.bound(run, fmt.Sprintf("pr%d", pr.Number))
	w.restore(run, linux, key1, "branch_newest", v1)
	w.restore(run, linux, key2, "branch_exact", v1)
	// Main never reads the branch's.
	push = w.Push(repo, "main")
	w.bound(push, "main")
	w.restore(push, linux, key2, "main_newest", main1)
}

func platforms(t T, w *world) {
	repo := w.Repository("app")
	w.mainSaves(repo, linux, key1, data("linux"))
	run := w.Push(repo, "main")
	w.bound(run, "main")
	w.restore(run, mac, key1, "", nil)
	mine := data("mac")
	w.mainSaves(repo, mac, key2, mine)
	next := w.Push(repo, "main")
	w.bound(next, "main")
	w.restore(next, mac, key1, "main_newest", mine)
}

func lookup(t T, w *world) {
	repo := w.Repository("app")
	old := data("old")
	w.mainSaves(repo, linux, key1, old)
	run := w.Push(repo, "main")
	w.bound(run, "main")
	_, tok := w.job(run)
	a, r := w.rs(tok, linux, "build", key1, true)
	if a.Status != http.StatusOK || r == nil || r.Step != "main_exact" || r.SHA256 != Sum(old) {
		t.Fatalf("lookup answered %d %+v", a.Status, r)
	}
	if cs := w.Choices(run); len(cs) != 1 || cs[0].Step != "main_exact" || cs[0].Entry == "" {
		t.Fatalf("a lookup's choice %+v", cs)
	}
	// A later restore of the run gets the same entry, even once a newer
	// one is ready.
	w.mainSaves(repo, linux, key1, data("new"))
	w.restore(run, linux, key1, "main_exact", old)
	// A lookup of nothing records none too.
	_, tok = w.job(run)
	if a, r := w.rs(tok, linux, "build", key2, true); a.Status != http.StatusOK || r == nil || r.Step != "main_newest" {
		t.Fatalf("lookup of another key answered %d %+v", a.Status, r)
	}
	if a, r := w.rs(tok, linux, "other", key2, true); a.Status != http.StatusOK || r != nil {
		t.Fatalf("lookup of nothing answered %d %+v", a.Status, r)
	}
}

func choiceHolds(t T, w *world) {
	repo := w.Repository("app")
	first := data("first")
	w.mainSaves(repo, linux, key1, first)
	pr := w.PullRequest(repo, "main", "fix")
	run := w.PullRequestRun(pr)
	w.bound(run, fmt.Sprintf("pr%d", pr.Number))
	w.restore(run, linux, key1, "main_exact", first)
	// A sibling's entry, ready since, doesn't change the run's choice.
	w.save(run, linux, "build", key1, data("sibling"), "success")
	w.restore(run, linux, key1, "main_exact", first)
	// Nor does main's newer one; a new run picks again.
	second := data("second")
	w.mainSaves(repo, linux, key1, second)
	w.restore(run, linux, key1, "main_exact", first)
	next := w.PullRequestRun(pr)
	w.bound(next, fmt.Sprintf("pr%d", pr.Number))
	w.restore(next, linux, key1, "pull_request_exact", data("sibling"))
	// A run that chose nothing keeps nothing.
	none := w.Push(repo, "main")
	w.bound(none, "main")
	w.restore(none, mac, key1, "", nil)
	w.mainSaves(repo, mac, key1, data("mac"))
	w.restore(none, mac, key1, "", nil)
}

func rerun(t T, w *world) {
	repo := w.Repository("app")
	first := data("first")
	w.mainSaves(repo, linux, key1, first)
	run := w.Push(repo, "main")
	j, tok := w.job(run)
	if a := w.c.Bind(tok, nil); a.Scope() != "main" {
		t.Fatalf("bound %q", a.Scope())
	}
	w.restore(run, linux, key1, "main_exact", first)
	second := data("second")
	w.mainSaves(repo, linux, key1, second)
	w.restore(run, linux, key1, "main_exact", first)
	w.FinishJob(j, "failure")
	again := w.Rerun(run)
	if again.Attempt != 2 {
		t.Fatalf("the rerun is attempt %d", again.Attempt)
	}
	w.restore(again, linux, key1, "main_exact", second)
}

func successGating(t T, w *world) {
	repo := w.Repository("app")
	run := w.Push(repo, "main")
	w.bound(run, "main")
	j, tok := w.job(run)
	body := data("pending")
	id, _ := w.reserve(tok, linux, "build", key1, body)
	if a := w.c.Commit(tok, id, int64(len(body)), Sum(body)); a.Status != http.StatusOK {
		t.Fatalf("commit answered %d: %s", a.Status, a.Reason)
	}
	if es := w.Entries(repo); len(es) != 1 || es[0].State != "pending" || es[0].Scope != "main" || es[0].SHA256 != Sum(body) {
		t.Fatalf("entries %+v", es)
	}
	// Pending: no one restores it, not even its own run.
	other := w.Push(repo, "main")
	w.bound(other, "main")
	w.restore(other, linux, key1, "", nil)
	w.FinishJob(j, "success")
	later := w.Push(repo, "main")
	w.bound(later, "main")
	w.restore(later, linux, key1, "main_exact", body)
	if es := w.Entries(repo); len(es) != 1 || es[0].State != "ready" {
		t.Fatalf("entries %+v", es)
	}
	// A failed job's entry goes.
	failed := w.Push(repo, "main")
	w.bound(failed, "main")
	w.save(failed, linux, "build", key2, data("failed"), "failure")
	cancelled := w.Push(repo, "main")
	w.bound(cancelled, "main")
	w.save(cancelled, linux, "build", key2, data("cancelled"), "cancelled")
	w.Pass()
	for _, e := range w.Entries(repo) {
		if e.Key == key2 {
			t.Fatalf("an unsuccessful job's entry is there: %+v", e)
		}
	}
	last := w.Push(repo, "main")
	w.bound(last, "main")
	w.restore(last, linux, key2, "main_newest", body)
}

func readRuns(t T, w *world) {
	repo := w.Repository("app")
	main1 := data("main1")
	w.mainSaves(repo, linux, key1, main1)
	run := w.Event(repo, "workflow_dispatch", "main")
	w.bound(run, "read")
	w.restore(run, linux, key1, "main_exact", main1)
	_, tok := w.job(run)
	if _, a := w.reserve(tok, linux, "build", key1, data("read")); a.Status != http.StatusForbidden {
		t.Fatalf("a read run's save answered %d", a.Status)
	}
}

func closing(t T, w *world) {
	repo := w.Repository("app")
	main1 := data("main1")
	w.mainSaves(repo, linux, key1, main1)
	pr := w.PullRequest(repo, "main", "fix")
	run := w.PullRequestRun(pr)
	w.bound(run, fmt.Sprintf("pr%d", pr.Number))
	w.save(run, linux, "build", key1, data("pull request"), "success")
	w.ClosePullRequest(pr)
	w.Pass()
	w.ReopenPullRequest(pr)
	// Reopened, it starts again from its base.
	next := w.PullRequestRun(pr)
	w.bound(next, fmt.Sprintf("pr%d", pr.Number))
	w.restore(next, linux, key1, "main_exact", main1)
	w.restore(next, linux, key2, "main_newest", main1)
}

func contributions(t T, w *world) {
	repo := w.Repository("app")
	w.SetRunners(repo, fmt.Sprintf(`{"bootstrap": %q}`, w.Bootstrap(repo)))
	main1 := data("main1")
	w.mainSaves(repo, linux, key1, main1)
	fork := w.ForkPullRequest(repo, "main")
	// A fork's pull_request run gets nothing.
	run := w.PullRequestRun(fork)
	w.bound(run, "read")
	_, tok := w.job(run)
	if a, _ := w.rs(tok, linux, "build", key1, false); a.Status != http.StatusForbidden {
		t.Fatalf("a fork's restore answered %d", a.Status)
	}
	// An approved contribution gets its pull request's scope.
	prt := w.PullRequestTargetRun(fork)
	j := w.StartJob(prt, w.Bootstrap(repo))
	ev := &PullRequestEvent{PullRequest: fork.Number, HeadRepositoryID: fork.HeadRepositoryID, HeadSHA: fork.HeadSHA}
	if a := w.c.Bind(w.Token(j, nil), ev); a.Scope() != fmt.Sprintf("pr%d", fork.Number) {
		t.Fatalf("the bootstrap bound %q (%s)", a.Scope(), a.Reason)
	}
	_, tok = w.job(prt)
	if a, _ := w.rs(tok, linux, "build", key1, false); a.Status != http.StatusForbidden {
		t.Fatalf("an unapproved contribution's restore answered %d", a.Status)
	}
	w.Approve(prt)
	w.restore(prt, linux, key1, "main_exact", main1)
	mine := data("contribution")
	w.save(prt, linux, "build", key2, mine, "success")
	for _, e := range w.Entries(repo) {
		if e.Key == key2 && e.Scope != fmt.Sprintf("pr%d", fork.Number) {
			t.Fatalf("a contribution saved in %s", e.Scope)
		}
	}
	next := w.PullRequestTargetRun(fork)
	j = w.StartJob(next, w.Bootstrap(repo))
	if a := w.c.Bind(w.Token(j, nil), ev); a.Scope() != fmt.Sprintf("pr%d", fork.Number) {
		t.Fatalf("the bootstrap bound %q", a.Scope())
	}
	w.Approve(next)
	w.restore(next, linux, key2, "pull_request_exact", mine)
	// Main never reads it.
	push := w.Push(repo, "main")
	w.bound(push, "main")
	w.restore(push, linux, key2, "main_newest", main1)
}

func noWidening(t T, w *world) {
	repo := w.Repository("app")
	w.SetRunners(repo, fmt.Sprintf(`{"bootstrap": %q}`, w.Bootstrap(repo)))
	fork := w.ForkPullRequest(repo, "main")
	prt := w.PullRequestTargetRun(fork)
	j := w.StartJob(prt, w.Bootstrap(repo))
	ev := &PullRequestEvent{PullRequest: fork.Number, HeadRepositoryID: fork.HeadRepositoryID, HeadSHA: fork.HeadSHA}
	want := fmt.Sprintf("pr%d", fork.Number)
	if a := w.c.Bind(w.Token(j, nil), ev); a.Scope() != want {
		t.Fatalf("the bootstrap bound %q", a.Scope())
	}
	w.Approve(prt)
	// The contributor's job's token, whatever it sends, gets the run's
	// binding, never another.
	cj, tok := w.job(prt)
	for _, body := range [][]byte{nil, []byte(`{"pull_request": 1, "head_repository_id": 1, "head_sha": "` + strings.Repeat("b", 40) + `"}`)} {
		if a := w.c.BindRaw(tok, body); a.Status != http.StatusOK || a.Scope() != want {
			t.Fatalf("a contribution's call bound %d %q", a.Status, a.Scope())
		}
	}
	// It saves in its own scope only, and can't commit another job's
	// reservation, of its run or another's.
	rid, _ := w.reserve(tok, linux, "build", key1, nil)
	if rid == "" {
		t.Fatalf("an approved contribution couldn't save")
	}
	main := w.Push(repo, "main")
	w.bound(main, "main")
	mj, mtok := w.job(main)
	payload := data("x")
	mid, _ := w.reserve(mtok, linux, "build", key1, payload)
	if a := w.c.Commit(tok, mid, int64(len(payload)), Sum(payload)); a.Status != http.StatusForbidden {
		t.Fatalf("a contribution committed main's reservation: %d", a.Status)
	}
	sib, stok := w.job(prt)
	if a := w.c.Commit(stok, rid, 0, Sum(nil)); a.Status != http.StatusForbidden {
		t.Fatalf("a sibling committed another job's reservation: %d", a.Status)
	}
	// Its token can't name another run or job: those are GitHub's.
	for name, o := range map[string]map[string]any{
		"another run": {"run_id": fmt.Sprint(main.ID)},
		"another job": {"check_run_id": fmt.Sprint(mj.ID)},
	} {
		if a, _ := w.rs(w.Token(cj, o), linux, "build", key1, false); a.Status != http.StatusForbidden {
			t.Fatalf("%s: restore answered %d", name, a.Status)
		}
	}
	// Nor does what it claims of its ref or event change its scope: it
	// reads its run's restore chain alone.
	if a, r := w.rs(w.Token(cj, map[string]any{"ref": "refs/heads/main", "event_name": "push"}), linux, "build", key1, false); a.Status == http.StatusOK && r != nil {
		t.Fatalf("restored %s's entry, which main saved after its run chose", r.Scope)
	}
	w.FinishJob(sib, "success")
	if s, _ := w.Binding(prt); s != want {
		t.Fatalf("the run's binding became %q", s)
	}
}

func cacheErrors(t T, w *world) {
	repo := w.Repository("app")
	run := w.Push(repo, "main")
	j, tok := w.job(run)
	if a, _ := w.rs(tok, linux, "build", key1, false); a.Status != http.StatusForbidden {
		t.Fatalf("an unbound run's restore answered %d", a.Status)
	}
	w.bound(run, "main")
	refused := map[string]Answer{
		"no token":               w.c.post("/v1/cache/restore", "", marshal(map[string]any{"family": "build", "key": key1, "platform": linux, "store": w.store})),
		"no check_run_id":        first(w.rs(w.Token(j, map[string]any{"check_run_id": nil}), linux, "build", key1, false)),
		"another job's":          first(w.rs(w.Token(j, map[string]any{"check_run_id": "1"}), linux, "build", key1, false)),
		"another attempt's":      first(w.rs(w.Token(j, map[string]any{"run_attempt": "2"}), linux, "build", key1, false)),
		"another audience":       first(w.rs(w.Token(j, map[string]any{"aud": "https://example.com"}), linux, "build", key1, false)),
		"an unknown field":       first(w.c.RestoreRaw(tok, []byte(`{"family": "build", "key": "`+key1+`", "platform": "linux-x64", "store": "`+w.store+`", "scope": "main"}`), false)),
		"no platform":            first(w.c.RestoreRaw(tok, []byte(`{"family": "build", "key": "`+key1+`", "store": "`+w.store+`"}`), false)),
		"no store":               first(w.c.RestoreRaw(tok, []byte(`{"family": "build", "key": "`+key1+`", "platform": "linux-x64"}`), false)),
		"a store with a hyphen":  first(w.c.Restore(tok, "my-store", linux, "build", key1, false)),
		"a key of 19":            first(w.rs(tok, linux, "build", key1[:19], false)),
		"a family in capitals":   first(w.rs(tok, linux, "Build", key1, false)),
		"a platform of one part": first(w.rs(tok, "linux", "build", key1, false)),
		"a body of a list":       first(w.c.RestoreRaw(tok, []byte(`[]`), false)),
		"a save's unknown field": w.c.post("/v1/cache/save", tok, []byte(`{"family": "build", "key": "`+key1+`", "platform": "linux-x64", "store": "`+w.store+`", "scope": "pr1"}`)),
		"no such reservation":    w.c.Commit(tok, strings.Repeat("0", 32), 1, Sum(nil)),
		"a reservation ID of 31": w.c.Commit(tok, strings.Repeat("0", 31), 1, Sum(nil)),
	}
	body := data("x")
	if w.store == ServiceStore {
		_, r := w.c.Save(tok, linux, "build", key1)
		refused["nothing uploaded"] = w.c.Commit(tok, r.ID, 1, Sum(nil))
		w.c.Upload(r, body, 1<<20, false)
		refused["another size"] = w.c.Commit(tok, r.ID, int64(len(body))+1, Sum(body))
		refused["a SHA-256 in capitals"] = w.c.Commit(tok, r.ID, int64(len(body)), strings.ToUpper(Sum(body)))
		refused["a save with a size"] = w.c.post("/v1/cache/save", tok, marshal(map[string]any{"family": "build", "key": key1,
			"platform": linux, "store": ServiceStore, "size": 1, "sha256": Sum(nil)}))
		w.rid = r.ID
	} else {
		refused["an index's save without its size"] = w.c.post("/v1/cache/save", tok, marshal(map[string]any{"family": "build",
			"key": key1, "platform": linux, "store": w.store}))
		_, id := w.c.SaveIndex(tok, w.store, linux, "build", key1, int64(len(body)), Sum(body))
		refused["another size"] = w.c.Commit(tok, id, int64(len(body))+1, Sum(body))
		refused["another SHA-256"] = w.c.Commit(tok, id, int64(len(body)), Sum(nil))
		w.rid = id
	}
	r := &Reserved{ID: w.rid}
	for name, a := range refused {
		if a.Status != http.StatusForbidden {
			t.Errorf("%s: answered %d", name, a.Status)
		}
	}
	if a := w.c.Commit(tok, r.ID, int64(len(body)), Sum(body)); a.Status != http.StatusOK {
		t.Fatalf("the commit answered %d: %s", a.Status, a.Reason)
	}
	// A commit tried again, after a lost answer, is answered the same.
	if a := w.c.Commit(tok, r.ID, int64(len(body)), Sum(body)); a.Status != http.StatusOK {
		t.Fatalf("the commit tried again answered %d: %s", a.Status, a.Reason)
	}
	w.Unavailable(true)
	a, _ := w.rs(tok, linux, "build", key1, false)
	w.Unavailable(false)
	if a.Status != http.StatusServiceUnavailable {
		t.Fatalf("a restore with GitHub unavailable answered %d", a.Status)
	}
	// A finished job's token is good for nothing.
	w.FinishJob(j, "success")
	if a, _ := w.rs(tok, linux, "build", key1, false); a.Status != http.StatusForbidden {
		t.Fatalf("a finished job's restore answered %d", a.Status)
	}
}

func first(a Answer, _ any) Answer { return a }

func transfers(t T, w *world) {
	w.store = ServiceStore
	repo := w.Repository("app")
	run := w.Push(repo, "main")
	w.bound(run, "main")
	j, tok := w.job(run)
	big := data("a block of a build tree ")
	big = append(big, data("and some more ")...)
	big = append(big, big...)
	big = append(big, big...)
	_, r := w.c.Save(tok, linux, "build", key1)
	if r.Headers["x-goog-resumable"] != "start" || r.Headers["x-goog-if-generation-match"] != "0" {
		t.Fatalf("save's headers %v", r.Headers)
	}
	// The URL starts an upload only with exactly its headers.
	if code := w.c.Get(http.MethodPost, r.URL); code < 400 {
		t.Fatalf("an upload started without its headers: %d", code)
	}
	w.c.Upload(r, big, 256<<10, true)
	if a := w.c.Commit(tok, r.ID, int64(len(big)), Sum(big)); a.Status != http.StatusOK {
		t.Fatalf("commit answered %d: %s", a.Status, a.Reason)
	}
	w.FinishJob(j, "success")
	next := w.Push(repo, "main")
	w.bound(next, "main")
	got := w.restore(next, linux, key1, "main_exact", big)
	// A URL is for its method alone.
	if code := w.c.Get(http.MethodPut, got.URL); code < 400 {
		t.Fatalf("a restore's URL took a PUT: %d", code)
	}
	if code := w.c.Get(http.MethodGet, got.URL); code != http.StatusOK {
		t.Fatalf("a restore's URL answered %d", code)
	}
}

func stores(t T, w *world) {
	w.store = IndexStore
	repo := w.Repository("app")
	main1 := data("main1")
	w.mainSaves(repo, linux, key1, main1)
	run := w.Push(repo, "main")
	w.bound(run, "main")
	w.restore(run, linux, key1, "main_exact", main1)
	// Another store of the consumer's has nothing of it.
	w.store = "other"
	w.restore(run, linux, key1, "", nil)
	// Nor does the service's, on the service's own machine.
	w.store = ServiceStore
	w.restore(run, linux, key1, "", nil)
	if es := w.Entries(repo); len(es) != 1 || es[0].Store != IndexStore {
		t.Fatalf("entries %+v", es)
	}
	for _, c := range w.Choices(run) {
		if c.Store == "other" && c.Entry != "" {
			t.Fatalf("another store's choice %+v", c)
		}
	}
}

func serviceStore(t T, w *world) {
	repo := w.Repository("app")
	run := w.Push(repo, "main")
	w.bound(run, "main")
	// A job on any other runner gets nothing of the service's store.
	_, tok := w.job(run)
	if a, _ := w.c.Restore(tok, ServiceStore, linux, "build", key1, false); a.Status != http.StatusForbidden {
		t.Fatalf("an outside job's restore of the service's store answered %d", a.Status)
	}
	if a, _ := w.c.Save(tok, linux, "build", key1); a.Status != http.StatusForbidden {
		t.Fatalf("an outside job's save in the service's store answered %d", a.Status)
	}
	// A job on one of the service's machines gets URLs for it.
	w.store = ServiceStore
	payload := data("on the service's machine")
	w.save(run, linux, "build", key1, payload, "success")
	next := w.Push(repo, "main")
	w.bound(next, "main")
	if r := w.restore(next, linux, key1, "main_exact", payload); r.URL == "" {
		t.Fatalf("a machine's restore of the service's store has no URL")
	}
	// And an index's entry without one, like any other job.
	w.store = IndexStore
	w.save(next, linux, "build", key2, data("index"), "success")
	last := w.Push(repo, "main")
	w.bound(last, "main")
	j := w.StartMachineJob(last)
	if a, r := w.c.Restore(w.Token(j, nil), IndexStore, linux, "build", key2, false); a.Status != http.StatusOK || r == nil || r.URL != "" {
		t.Fatalf("a machine's restore of an index answered %d %+v", a.Status, r)
	}
}

func indexEntries(t T, w *world) {
	w.store = IndexStore
	repo := w.Repository("app")
	run := w.Push(repo, "main")
	w.bound(run, "main")
	j, tok := w.job(run)
	payload := data("kept in the runner's own cache")
	a, id := w.c.SaveIndex(tok, IndexStore, linux, "build", key1, int64(len(payload)), Sum(payload))
	if a.Status != http.StatusOK {
		t.Fatalf("an index's save answered %d: %s", a.Status, a.Reason)
	}
	if a := w.c.Commit(tok, id, int64(len(payload)), Sum(payload)); a.Status != http.StatusOK {
		t.Fatalf("an index's commit answered %d: %s", a.Status, a.Reason)
	}
	w.FinishJob(j, "success")
	next := w.Push(repo, "main")
	w.bound(next, "main")
	_, tok = w.job(next)
	a, r := w.c.Restore(tok, IndexStore, linux, "build", key1, false)
	if a.Status != http.StatusOK || r == nil || r.URL != "" || r.Size != int64(len(payload)) || r.SHA256 != Sum(payload) ||
		r.Scope != "main" || r.Key != key1 || r.Step != "main_exact" {
		t.Fatalf("an index's restore answered %d %+v", a.Status, r)
	}
	// A pull request's index entry is its own: main never reads it.
	pr := w.PullRequest(repo, "main", "fix")
	prun := w.PullRequestRun(pr)
	w.bound(prun, fmt.Sprintf("pr%d", pr.Number))
	w.save(prun, linux, "build", key2, data("pr"), "success")
	w.restore(prun, linux, key2, "pull_request_exact", data("pr"))
	push := w.Push(repo, "main")
	w.bound(push, "main")
	w.restore(push, linux, key2, "main_newest", payload)
}
