package cache_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/conformance"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/internal/cache"
	"github.com/lanescape/runners/internal/servicetest"
)

// These tests drive the real service, wired to the emulators, through the
// interface, as the conformance suite does, and look at what it keeps.

const (
	linux = "linux-x64"
	key1  = "0123456789abcdef0123"
	key2  = "fedcba9876543210fedc"
	key3  = "00112233445566778899"
)

type env struct {
	*servicetest.World
	t     *testing.T
	c     *conformance.Client
	clock *clockwork.FakeClock
	repo  service.Repository
	// store is the store the test uses; the service's, unless it says.
	store string
}

func setup(t *testing.T) *env {
	t.Helper()
	clock := clockwork.NewFakeClockAt(time.Now())
	w := servicetest.New(t, clock)
	e := &env{World: w, t: t, c: &conformance.Client{T: t, URL: w.URL()}, clock: clock, store: cache.ServiceStore}
	e.repo = w.Repository("app")
	return e
}

func (e *env) job(run service.Run) (service.Job, string) {
	j := e.StartJob(run, "")
	if e.store == cache.ServiceStore {
		j = e.StartMachineJob(run)
	}
	return j, e.Token(j, nil)
}

func (e *env) bind(run service.Run, want string) {
	e.t.Helper()
	_, tok := e.job(run)
	if a := e.c.Bind(tok, nil); a.Scope() != want {
		e.t.Fatalf("bound %q (%s), want %s", a.Scope(), a.Reason, want)
	}
}

// save saves data from a new job of run, which succeeds, and returns the
// entry's ID.
func (e *env) save(run service.Run, key string, data []byte) string {
	e.t.Helper()
	before := map[string]bool{}
	for _, en := range e.Entries(e.repo) {
		before[en.ID] = true
	}
	j, tok := e.job(run)
	var id string
	if e.store == cache.ServiceStore {
		_, r := e.c.Save(tok, linux, "build", key)
		e.c.Upload(r, data, 1<<20, false)
		id = r.ID
	} else {
		_, id = e.c.SaveIndex(tok, e.store, linux, "build", key, int64(len(data)), conformance.Sum(data))
	}
	if a := e.c.Commit(tok, id, int64(len(data)), conformance.Sum(data)); a.Status != http.StatusOK {
		e.t.Fatalf("commit: %d %s", a.Status, a.Reason)
	}
	id = ""
	for _, en := range e.Entries(e.repo) {
		if !before[en.ID] {
			id = en.ID
		}
	}
	e.FinishJob(j, "success")
	return id
}

func (e *env) mainSave(key string, data []byte) string {
	e.t.Helper()
	run := e.Push(e.repo, "main")
	e.bind(run, "main")
	return e.save(run, key, data)
}

func (e *env) has(id string) bool {
	for _, en := range e.Entries(e.repo) {
		if en.ID == id {
			return true
		}
	}
	return false
}

// objects are the cache bucket's objects under prefix.
func (e *env) objects(prefix string) []string {
	var out []string
	for _, n := range e.Storage.Names(servicetest.Cache) {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	return out
}

func (e *env) restore(run service.Run, key string) *conformance.Restored {
	e.t.Helper()
	_, tok := e.job(run)
	a, r := e.c.Restore(tok, e.store, linux, "build", key, false)
	if a.Status != http.StatusOK {
		e.t.Fatalf("restore: %d %s", a.Status, a.Reason)
	}
	return r
}

func TestANewerVersionReplacesTheOlder(t *testing.T) {
	e := setup(t)
	old := e.mainSave(key1, []byte("old"))
	e.clock.Advance(time.Second)
	newer := e.mainSave(key1, []byte("new"))
	if e.has(old) || !e.has(newer) {
		t.Fatalf("old %v, new %v", e.has(old), e.has(newer))
	}
	if objs := e.objects("entries/"); len(objs) != 1 {
		t.Fatalf("objects %v", objs)
	}
	if objs := e.objects("uploads/"); len(objs) != 0 {
		t.Fatalf("uploads left %v", objs)
	}
}

func TestARunsChoiceKeepsWhatItChose(t *testing.T) {
	e := setup(t)
	old := e.mainSave(key1, []byte("old"))
	run := e.Push(e.repo, "main")
	e.bind(run, "main")
	e.restore(run, key1)
	e.clock.Advance(time.Second)
	e.mainSave(key1, []byte("new"))
	e.Pass()
	if !e.has(old) {
		t.Fatal("an entry an unfinished run chose went")
	}
	// Once the run has finished, with no rerun due, its choice goes, and
	// so does the entry.
	e.FinishRun(run, "success")
	e.Pass()
	e.Pass()
	if e.has(old) {
		t.Fatal("a superseded entry stayed once its run had finished")
	}
}

func TestAPullRequestKeepsOneEntryOfEachFamily(t *testing.T) {
	e := setup(t)
	pr := e.PullRequest(e.repo, "main", "fix")
	run := e.PullRequestRun(pr)
	e.bind(run, fmt.Sprintf("pr%d", pr.Number))
	first := e.save(run, key1, []byte("one"))
	e.clock.Advance(time.Second)
	second := e.save(run, key2, []byte("two"))
	if e.has(first) || !e.has(second) {
		t.Fatalf("first %v, second %v", e.has(first), e.has(second))
	}
}

func TestLastingScopesKeepTheirTwoNewestKeys(t *testing.T) {
	e := setup(t)
	k1 := e.mainSave(key1, []byte("1"))
	// An open pull request's most recent choice names main's key1.
	pr := e.PullRequest(e.repo, "main", "fix")
	run := e.PullRequestRun(pr)
	e.bind(run, fmt.Sprintf("pr%d", pr.Number))
	e.restore(run, key1)
	e.FinishRun(run, "success")
	e.clock.Advance(time.Second)
	k2 := e.mainSave(key2, []byte("2"))
	e.clock.Advance(time.Second)
	k3 := e.mainSave(key3, []byte("3"))
	e.Pass()
	if !e.has(k1) || !e.has(k2) || !e.has(k3) {
		t.Fatalf("key1 %v, key2 %v, key3 %v; want all, key1 for the open pull request", e.has(k1), e.has(k2), e.has(k3))
	}
	e.ClosePullRequest(pr)
	e.Pass()
	if e.has(k1) || !e.has(k2) || !e.has(k3) {
		t.Fatalf("key1 %v, key2 %v, key3 %v; want the two newest", e.has(k1), e.has(k2), e.has(k3))
	}
}

func TestAClosedPullRequestsEntriesGoAtOnce(t *testing.T) {
	e := setup(t)
	pr := e.PullRequest(e.repo, "main", "fix")
	run := e.PullRequestRun(pr)
	e.bind(run, fmt.Sprintf("pr%d", pr.Number))
	id := e.save(run, key1, []byte("pr"))
	e.ClosePullRequest(pr)
	if e.has(id) {
		t.Fatal("a closed pull request's entry stayed past the pass its closing started")
	}
	if _, _, ok := e.Storage.Object(servicetest.State, fmt.Sprintf("closures/%d/pr%d.json", e.repo.ID, pr.Number)); !ok {
		t.Fatal("no closure")
	}
}

func TestUnusedEntriesGo(t *testing.T) {
	e := setup(t)
	main := e.mainSave(key1, []byte("main"))
	pr := e.PullRequest(e.repo, "main", "fix")
	run := e.PullRequestRun(pr)
	e.bind(run, fmt.Sprintf("pr%d", pr.Number))
	own := e.save(run, key2, []byte("pr"))
	e.FinishRun(run, "success")
	e.clock.Advance(cache.PullRequestUnused - time.Hour)
	e.Pass()
	if !e.has(own) {
		t.Fatal("a pull request's entry went before 14 days unused")
	}
	e.clock.Advance(time.Hour)
	e.Pass()
	if e.has(own) || !e.has(main) {
		t.Fatalf("pull request's %v, main's %v", e.has(own), e.has(main))
	}
	e.clock.Advance(cache.LastingUnused)
	e.Pass()
	if e.has(main) {
		t.Fatal("a family of main's unused for 90 days stayed")
	}
}

func TestADelistedBranchsEntriesGo(t *testing.T) {
	e := setup(t)
	e.SetRunners(e.repo, `{"branches": {"support/v1": "v1"}}`)
	e.Branch(e.repo, "support/v1")
	run := e.Push(e.repo, "support/v1")
	e.bind(run, "branch-v1")
	id := e.save(run, key1, []byte("v1"))
	e.Pass()
	if !e.has(id) {
		t.Fatal("a listed branch's entry went")
	}
	e.SetRunners(e.repo, `{}`)
	e.Pass()
	if e.has(id) {
		t.Fatal("a delisted branch's entry stayed")
	}
}

func TestUploadsAndOrphansGo(t *testing.T) {
	e := setup(t)
	run := e.Push(e.repo, "main")
	e.bind(run, "main")
	_, tok := e.job(run)
	_, r := e.c.Save(tok, linux, "build", key1)
	e.c.Upload(r, []byte("never committed"), 1<<20, false)
	e.Storage.Put(servicetest.Cache, fmt.Sprintf("entries/%d/%s/%s", e.repo.ID, strings.Repeat("a", 20), strings.Repeat("b", 32)), []byte("orphan"))
	e.Pass()
	if objs := e.objects(fmt.Sprintf("uploads/%d/", e.repo.ID)); len(objs) != 1 {
		t.Fatalf("an unexpired reservation's upload went: %v", objs)
	}
	if objs := e.objects("entries/"); len(objs) != 0 {
		t.Fatalf("an orphaned entry object stayed: %v", objs)
	}
	e.clock.Advance(cache.ReserveFor)
	// Its commit is refused once the reservation has expired.
	if a := e.c.Commit(tok, r.ID, 15, conformance.Sum([]byte("never committed"))); a.Status != http.StatusForbidden {
		t.Fatalf("a commit after expiry answered %d", a.Status)
	}
	e.Pass()
	if objs := e.objects("uploads/"); len(objs) != 0 {
		t.Fatalf("an expired reservation's upload stayed: %v", objs)
	}
	for _, n := range e.Storage.Names(servicetest.State) {
		if strings.HasPrefix(n, "cache/reservations/") {
			t.Fatalf("an expired reservation stayed: %s", n)
		}
	}
	// An upload with no reservation goes too.
	e.Storage.Put(servicetest.Cache, fmt.Sprintf("uploads/%d/%s", e.repo.ID, strings.Repeat("c", 32)), []byte("stray"))
	e.Pass()
	if objs := e.objects("uploads/"); len(objs) != 0 {
		t.Fatalf("an upload with no reservation stayed: %v", objs)
	}
}

func TestAPendingEntryGoesAfterFiveDays(t *testing.T) {
	e := setup(t)
	run := e.Push(e.repo, "main")
	e.bind(run, "main")
	_, tok := e.job(run)
	_, r := e.c.Save(tok, linux, "build", key1)
	e.c.Upload(r, []byte("x"), 1<<20, false)
	e.c.Commit(tok, r.ID, 1, conformance.Sum([]byte("x")))
	e.clock.Advance(cache.PendingFor - time.Hour)
	e.Pass()
	if len(e.Entries(e.repo)) != 1 {
		t.Fatal("a pending entry went early")
	}
	e.clock.Advance(time.Hour)
	e.Pass()
	if len(e.Entries(e.repo)) != 0 || len(e.objects("entries/")) != 0 {
		t.Fatal("a pending entry stayed past 5 days")
	}
}

func TestNothingSecretIsLogged(t *testing.T) {
	e := setup(t)
	run := e.Push(e.repo, "main")
	e.bind(run, "main")
	j, tok := e.job(run)
	_, r := e.c.Save(tok, linux, "build", key1)
	e.c.Upload(r, []byte("x"), 1<<20, false)
	e.c.Commit(tok, r.ID, 1, conformance.Sum([]byte("x")))
	e.FinishJob(j, "success")
	next := e.Push(e.repo, "main")
	e.bind(next, "main")
	got := e.restore(next, key1)
	logs := e.Logs.String()
	for _, secret := range []string{tok, r.URL, got.URL, "X-Goog-Signature", "upload_id"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the logs hold a credential: %.40s", secret)
		}
	}
	if !strings.Contains(logs, `msg="cache restore choice"`) || !strings.Contains(logs, "step=main_exact") {
		t.Fatal("the restore choice wasn't logged")
	}
}

// An index's entries keep every rule the service's store's do, and no
// object of the cache's bucket is ever involved.
func TestAnIndexsEntriesFollowTheSameRules(t *testing.T) {
	e := setup(t)
	e.store = "actions"
	old := e.mainSave(key1, []byte("old"))
	e.clock.Advance(time.Second)
	newer := e.mainSave(key1, []byte("new"))
	if e.has(old) || !e.has(newer) {
		t.Fatalf("old %v, new %v", e.has(old), e.has(newer))
	}
	pr := e.PullRequest(e.repo, "main", "fix")
	run := e.PullRequestRun(pr)
	e.bind(run, fmt.Sprintf("pr%d", pr.Number))
	first := e.save(run, key1, []byte("one"))
	e.clock.Advance(time.Second)
	second := e.save(run, key2, []byte("two"))
	if e.has(first) || !e.has(second) {
		t.Fatalf("first %v, second %v", e.has(first), e.has(second))
	}
	failed := e.Push(e.repo, "main")
	e.bind(failed, "main")
	j, tok := e.job(failed)
	_, id := e.c.SaveIndex(tok, e.store, linux, "build", key3, 1, conformance.Sum([]byte("x")))
	e.c.Commit(tok, id, 1, conformance.Sum([]byte("x")))
	e.FinishJob(j, "failure")
	for _, en := range e.Entries(e.repo) {
		if en.Key == key3 {
			t.Fatal("a failed job's index entry stayed")
		}
	}
	e.ClosePullRequest(pr)
	if e.has(second) {
		t.Fatal("a closed pull request's index entry stayed")
	}
	if objs := e.objects(""); len(objs) != 0 {
		t.Fatalf("an index's entries touched the bucket: %v", objs)
	}
}

// The service's store is open only to a job its machine's job-started
// check admitted, by GitHub's runner name and the admission's job.
func TestTheServicesStoreNeedsTheAdmittedJob(t *testing.T) {
	e := setup(t)
	run := e.Push(e.repo, "main")
	e.bind(run, "main")
	j := e.StartMachineJob(run)
	wj := e.WorkflowJob(j)
	// Another job of the run, on a runner named like a machine whose
	// admission was for the first job.
	other := e.StartJob(run, "")
	e.GitHub.RunJobOn(e.WorkflowJob(other), wj.Runner.Name)
	if a, _ := e.c.Restore(e.Token(other, nil), cache.ServiceStore, linux, "build", key1, false); a.Status != http.StatusForbidden {
		t.Fatalf("a job its machine didn't admit answered %d", a.Status)
	}
	if a, _ := e.c.Restore(e.Token(j, nil), cache.ServiceStore, linux, "build", key1, false); a.Status != http.StatusOK {
		t.Fatalf("the admitted job answered %d: %s", a.Status, a.Reason)
	}
}
