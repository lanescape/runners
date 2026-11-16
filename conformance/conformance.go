// Package conformance is the interface's test suite (specs/0005-interface.md):
// one set of cases that this repository runs, in CI, against both its
// service emulator, emulators/service, and the real service wired to its
// emulators of GitHub, Compute Engine, Google's issuers, Pub/Sub and Cloud
// Storage. The two must answer every case alike, so a
// consumer that tests against the emulator, at a pinned version of this
// module, tests against what the service does. It's public so that
// consumers can read what the emulator is held to.
package conformance

import (
	"errors"
	"fmt"
	"github.com/lanescape/runners/emulators/fault"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/internal/consumer"
)

// Case is one case of the suite.
type Case struct {
	Name string
	Run  func(t T, w service.World)
}

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
		one("a run is bound once, and kept by a maintainer's rerun", boundOnce),
		one("a tag named like a branch", tagLikeABranch),
		one("selectors name only a shape", selectors),
		one("forks and approved contributions", contributions),
		one("a contribution pushed to after its approval", pushedContribution),
		one("a contribution bound to its own pull request only", ownPullRequest),
		one("a run not approved left unbound and refused disks and caches", unapproved),
		one("a job on a machine made for another run's job", anotherRunsMachine),
		one("disk and cache requests from a run's own machine only", diskRequests),
		one("caches kept to their repository", ownRepository),
		one("a cache in the run's own scope from each step of the restore order", restoreOrder),
		one("a new key's save replaces the old key's version once no unfinished run chose it", newKey),
		one("one work disk per job, and one save of each cache", oneWorkDisk),
		one("caches saved", saves),
		one("the newest save kept, whichever job finished first", overlappingSaves),
		one("a version within its limits, and whole, or not kept", wholeVersions),
		one("refusals and retries", refusals),
	)
}

// The shapes the suite's jobs name.
const (
	// Small is the smallest shape, which most of the suite's jobs name.
	Small = "lanescape-2vcpu-linux-amd64"
	// Mid is the amd64 shape between the smallest and the largest.
	Mid = "lanescape-16vcpu-linux-amd64"
	// Arm is a shape of the other architecture.
	Arm = "lanescape-16vcpu-linux-arm64"
)

// Files are a consumer's files, which the suite's cases commit: a
// long-lived branch, support/v1, and two caches, build, keyed by deps.txt,
// and tools, with one key.
var Files = map[string]string{
	consumer.RunnersFile: `{"branches": {"support/v1": "v1"},
 "caches": {"build": {"from": ["deps.txt"]}, "tools": {}}}`,
	"deps.txt": "1\n",
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

// consumer is a repository with the suite's files on its default branch.
func (w *world) consumer(name string) service.Repository {
	repo := w.Repository(name)
	w.Commit(repo, "main", Files)
	return repo
}

// start starts a job that must get a machine.
func (w *world) start(run service.Run, selector string) service.Job {
	w.t.Helper()
	j, err := w.StartJob(run, selector)
	if err != nil {
		w.t.Fatalf("a job of run %d (%s) naming %s: %v", run.ID, run.Event, selector, err)
	}
	return j
}

// refused starts a job that the service must refuse, saying why.
func (w *world) refused(run service.Run, selector string) string {
	w.t.Helper()
	_, err := w.StartJob(run, selector)
	var r *service.Refused
	if !errors.As(err, &r) || r.Reason == "" {
		w.t.Fatalf("a job of run %d (%s) naming %s: %v; want a refusal with a reason", run.ID, run.Event, selector, err)
	}
	return r.Reason
}

// bound checks a run's binding.
func (w *world) bound(run service.Run, want string) {
	w.t.Helper()
	got, ok := w.Binding(run)
	if want == "" {
		if ok {
			w.t.Fatalf("run %d (%s) is bound %s; want it unbound", run.ID, run.Event, got)
		}
		return
	}
	if !ok || got != want {
		w.t.Fatalf("run %d (%s) is bound %q (%v); want %s", run.ID, run.Event, got, ok, want)
	}
}

// status checks a request's status.
func (w *world) status(what string, a Answer, want int) {
	w.t.Helper()
	if a.Status != want {
		w.t.Fatalf("%s answered %d %q; want %d", what, a.Status, a.Reason, want)
	}
}

// disk asks for a job's work disk, again while it's being made, as the disk
// action does, until it's ready, and returns where it came from.
func (w *world) disk(j service.Job) string {
	w.t.Helper()
	restored := ""
	for end := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		a, d := w.c.Disk(w.Token(j))
		w.status("job "+strconv.FormatInt(j.ID, 10)+"'s work disk", a, http.StatusOK)
		if restored != "" && d.Restored != restored {
			w.t.Fatalf("job %d's work disk came from %s, then %s", j.ID, restored, d.Restored)
		}
		restored = d.Restored
		if d.State == "ready" {
			return restored
		}
		if time.Now().After(end) {
			w.t.Fatalf("job %d's work disk was never ready", j.ID)
		}
	}
}

// restored restores a job's cache, as the cache action does, and checks
// where it came from; a version's objects are there, as its answer names
// them.
func (w *world) restored(j service.Job, cache, want string) {
	w.t.Helper()
	a, r := w.c.Restore(w.Token(j), cache)
	w.status("job "+strconv.FormatInt(j.ID, 10)+"'s restore of "+cache, a, http.StatusOK)
	if r.Restored != want {
		w.t.Fatalf("job %d (run %d, %s) restored %s from %s; want %s", j.ID, j.Run.ID, j.Run.Event, cache, r.Restored, want)
	}
	for _, o := range r.Objects {
		data, gen, ok := w.CacheStorage().Object(r.Bucket, o.Name)
		if !ok || gen != o.Generation || int64(len(data)) != o.Size {
			w.t.Fatalf("job %d's restore of %s names %+v, which isn't there as named", j.ID, cache, o)
		}
	}
}

// save saves a job's cache, as the save-cache action does: in a scope that
// saves, it uploads an object under the version's prefix, and commits it.
func (w *world) save(j service.Job, cache string, saves bool) {
	w.t.Helper()
	a, s := w.c.Save(w.Token(j), cache)
	w.status("job "+strconv.FormatInt(j.ID, 10)+"'s save of "+cache, a, http.StatusOK)
	if s.Saves != saves {
		w.t.Fatalf("job %d (run %d, %s) asked to save %s: %+v; want saves %v", j.ID, j.Run.ID, j.Run.Event, cache, s, saves)
	}
	if !saves {
		return
	}
	w.CacheStorage().Put(s.Bucket, s.Prefix+"shard-000.tar.zst", []byte("job "+strconv.FormatInt(j.ID, 10)+"'s "+cache))
	a, c := w.c.Commit(w.Token(j), cache)
	w.status("job "+strconv.FormatInt(j.ID, 10)+"'s commit of "+cache, a, http.StatusOK)
	if c.Objects != 1 {
		w.t.Fatalf("job %d's commit of %s recorded %d objects; want 1", j.ID, cache, c.Objects)
	}
}

func scopes(t T, w *world) {
	repo := w.Repository("app")
	w.Commit(repo, "main", map[string]string{consumer.RunnersFile: `{"branches": {"support/v1": "v1"}}`})
	main := w.Push(repo, "main")
	w.start(main, Small)
	w.bound(main, "main")

	w.Branch(repo, "support/v1")
	branch := w.Push(repo, "support/v1")
	w.start(branch, Small)
	w.bound(branch, "branch-v1")

	w.Branch(repo, "feature")
	other := w.Push(repo, "feature")
	w.start(other, Small)
	w.bound(other, "read")

	dispatch := w.Event(repo, "workflow_dispatch", "main")
	w.start(dispatch, Small)
	w.bound(dispatch, "read")

	pr := w.PullRequest(repo, "main", "fix")
	run := w.PullRequestRun(pr)
	w.start(run, Small)
	w.bound(run, "pr"+strconv.Itoa(pr.Number))

	// A pull request into a listed branch.
	into := w.PullRequest(repo, "support/v1", "backport")
	intoRun := w.PullRequestRun(into)
	w.start(intoRun, Small)
	w.bound(intoRun, "pr"+strconv.Itoa(into.Number))

	// A head that's the head of two open pull requests binds read, and its
	// run's jobs are failed, saying why.
	w.PullRequest(repo, "main", "twice")
	again := w.PullRequest(repo, "support/v1", "twice")
	twice := w.PullRequestRun(again)
	w.refused(twice, Small)
	w.bound(twice, "read")

	fork := w.ForkPullRequest(repo, "main")
	forkRun := w.PullRequestRun(fork)
	w.refused(forkRun, Small)
	w.bound(forkRun, "")
}

func boundOnce(t T, w *world) {
	repo := w.Repository("app")
	run := w.Push(repo, "main")
	j := w.start(run, Small)
	w.bound(run, "main")
	w.FinishJob(j, "failure")
	run = w.Rerun(run)
	if run.Attempt != 2 {
		t.Fatalf("a rerun's attempt is %d", run.Attempt)
	}
	w.start(run, Small)
	w.bound(run, "main")
}

func tagLikeABranch(t T, w *world) {
	repo := w.Repository("app")
	run := w.Tag(repo, "main")
	w.start(run, Small)
	w.bound(run, "read")
}

func selectors(t T, w *world) {
	repo := w.Repository("app")
	// A label that claims to be a selector and isn't one the service
	// offers gets no machine.
	for _, label := range []string{"lanescape-3vcpu-linux-amd64", Small + "-run-1", Small + "-image-build", "lanescape-2vcpu-linux-riscv"} {
		w.refused(w.Push(repo, "main"), label)
	}
	// Every offered shape, standard or Spot, gets one.
	for _, sel := range []string{Small, Small + "-spot", Mid, Mid + "-spot", Arm, Arm + "-spot"} {
		run := w.Push(repo, "main")
		j := w.start(run, sel)
		if j.Selector != sel {
			t.Fatalf("a job naming %s got a machine of %s", sel, j.Selector)
		}
		w.bound(run, "main")
	}
}

func contributions(t T, w *world) {
	repo := w.consumer("app")
	pr := w.ForkPullRequest(repo, "main")
	run := w.PullRequestTargetRun(pr)
	// Before a maintainer approves it, only the smallest shape, standard,
	// and the run stays unbound; any other of its jobs is failed.
	w.start(run, Small)
	w.bound(run, "")
	w.refused(run, "lanescape-80vcpu-linux-amd64")
	w.refused(w.PullRequestTargetRun(pr), Small+"-spot")
	w.bound(run, "")

	approved := w.PullRequestTargetRun(pr)
	w.Approve(approved)
	j := w.start(approved, Small)
	w.bound(approved, "pr"+strconv.Itoa(pr.Number))
	// An approved contribution's run saves in its pull request's scope,
	// which its next run restores, and starts from main's.
	w.restored(j, "build", service.StepNone)
	w.save(j, "build", true)
	w.FinishJob(j, "success")
	next := w.PullRequestTargetRun(pr)
	w.Approve(next)
	w.restored(w.start(next, "lanescape-80vcpu-linux-amd64"), "build", service.StepPullRequestExact)
	// Main never sees it.
	w.restored(w.start(w.Push(repo, "main"), Small), "build", service.StepNone)
}

func pushedContribution(t T, w *world) {
	repo := w.Repository("app")
	pr := w.ForkPullRequest(repo, "main")
	run := w.PullRequestTargetRun(pr)
	w.Approve(run)
	w.PushPullRequest(pr, nil)
	w.start(run, Small)
	w.bound(run, "pr"+strconv.Itoa(pr.Number))
}

func ownPullRequest(t T, w *world) {
	repo := w.Repository("app")
	a := w.ForkPullRequest(repo, "main")
	b := w.ForkPullRequest(repo, "main")
	ra, rb := w.PullRequestTargetRun(a), w.PullRequestTargetRun(b)
	w.Approve(ra)
	w.Approve(rb)
	w.start(rb, Small)
	w.start(ra, Small)
	w.bound(ra, "pr"+strconv.Itoa(a.Number))
	w.bound(rb, "pr"+strconv.Itoa(b.Number))
}

func unapproved(t T, w *world) {
	repo := w.consumer("app")
	pr := w.ForkPullRequest(repo, "main")
	run := w.PullRequestTargetRun(pr)
	j := w.start(run, Small)
	w.bound(run, "")
	// Nothing that needs a scope: no work disk, no cache.
	a, _ := w.c.Disk(w.Token(j))
	w.status("an unbound run's work disk request", a, http.StatusForbidden)
	a, _ = w.c.Restore(w.Token(j), "build")
	w.status("an unbound run's restore", a, http.StatusForbidden)
	a, _ = w.c.Save(w.Token(j), "build")
	w.status("an unbound run's save", a, http.StatusForbidden)
	w.bound(run, "")
}

func anotherRunsMachine(t T, w *world) {
	repo := w.consumer("app")
	// A machine made for a push to main takes a read run's job instead:
	// the job is answered for its own run, whatever the machine was made
	// for.
	madeFor := w.Push(repo, "main")
	run := w.Event(repo, "workflow_dispatch", "main")
	j, err := w.StartJobOn(run, Small, madeFor)
	if err != nil {
		t.Fatalf("a job on a machine made for another run's job: %v", err)
	}
	w.bound(run, "read")
	w.bound(madeFor, "")
	w.restored(j, "build", service.StepNone)
	w.save(j, "build", false)
}

func diskRequests(t T, w *world) {
	repo := w.consumer("app")
	run := w.Push(repo, "main")
	j := w.start(run, Small)
	j2 := w.start(run, Small)
	w.disk(j)
	w.restored(j, "build", service.StepNone)
	// A request with no token, or one that isn't a machine's, is refused.
	a, _ := w.c.Disk("")
	if a.Status != http.StatusUnauthorized {
		t.Fatalf("a work disk request with no token answered %d", a.Status)
	}
	a, _ = w.c.Restore("not.a.token", "build")
	if a.Status != http.StatusUnauthorized {
		t.Fatalf("a restore with a token that isn't one answered %d", a.Status)
	}
	// A job GitHub shows completed can ask for nothing, even before its
	// machine reports, as when its supervisor has died.
	jc := w.start(run, Small)
	w.save(jc, "tools", true)
	w.CompleteJob(jc, "success")
	a, _ = w.c.Disk(w.Token(jc))
	w.status("a completed job's work disk request, before its machine reports", a, http.StatusForbidden)
	a, _ = w.c.Restore(w.Token(jc), "build")
	w.status("a completed job's restore, before its machine reports", a, http.StatusForbidden)
	a, _ = w.c.Save(w.Token(jc), "build")
	w.status("a completed job's save, before its machine reports", a, http.StatusForbidden)
	a, _ = w.c.Commit(w.Token(jc), "tools")
	w.status("a completed job's commit, before its machine reports", a, http.StatusForbidden)
	// A machine whose job has finished can ask for nothing.
	w.FinishJob(j, "success")
	a, _ = w.c.Disk(w.Token(j))
	w.status("a finished job's work disk request", a, http.StatusForbidden)
	a, _ = w.c.Save(w.Token(j), "build")
	w.status("a finished job's save", a, http.StatusForbidden)
	// The run's other jobs still can, each with a disk of its own; what the
	// completed job saved is restored by later runs, since it succeeded.
	w.disk(j2)
	w.save(j2, "build", true)
	w.restored(w.start(w.Push(repo, "main"), Small), "tools", service.StepMainExact)
}

func ownRepository(t T, w *world) {
	a, b := w.consumer("one"), w.consumer("two")
	ja := w.start(w.Push(a, "main"), Small)
	w.restored(ja, "build", service.StepNone)
	w.save(ja, "build", true)
	w.FinishJob(ja, "success")
	w.restored(w.start(w.Push(a, "main"), Small), "build", service.StepMainExact)
	// Another repository's main, of the same family and key, starts empty.
	w.restored(w.start(w.Push(b, "main"), Small), "build", service.StepNone)
}

func restoreOrder(t T, w *world) {
	repo := w.consumer("app")
	// Nothing saved yet: the disk starts empty; main saves it.
	j := w.start(w.Push(repo, "main"), Small)
	w.restored(j, "build", service.StepNone)
	w.save(j, "build", true)
	w.FinishJob(j, "success")
	// Main's next run starts from it; a machine of the other architecture
	// starts empty.
	m2 := w.Push(repo, "main")
	w.restored(w.start(m2, Small), "build", service.StepMainExact)
	w.restored(w.start(m2, Arm), "build", service.StepNone)

	// A pull request of main's key starts from main's, and saves its own.
	// The run's other jobs start from the same choice while it runs; its
	// next run, and a maintainer's rerun once it has finished, from the
	// pull request's own.
	pa := w.PullRequest(repo, "main", "a")
	ra := w.PullRequestRun(pa)
	j = w.start(ra, Small)
	other := w.start(ra, Small)
	w.restored(j, "build", service.StepMainExact)
	w.save(j, "build", true)
	w.FinishJob(j, "success")
	w.restored(other, "build", service.StepMainExact)
	w.FinishJob(other, "success")
	w.restored(w.start(w.PullRequestRun(pa), Small), "build", service.StepPullRequestExact)
	w.restored(w.start(w.Rerun(ra), Small), "build", service.StepPullRequestExact)
	// A push that moves its key starts from its newest.
	pa = w.PushPullRequest(pa, map[string]string{"deps.txt": "2\n"})
	w.restored(w.start(w.PullRequestRun(pa), Small), "build", service.StepPullRequestNewest)
	// Another pull request never sees it: of that key, it starts from
	// main's newest.
	w.Commit(repo, "b", map[string]string{"deps.txt": "2\n"})
	w.restored(w.start(w.PullRequestRun(w.PullRequest(repo, "main", "b")), Small), "build", service.StepMainNewest)

	// A listed branch with nothing of its own starts from main's, and
	// saves its own, which its later pushes, and pull requests into it,
	// start from.
	w.Branch(repo, "support/v1")
	j = w.start(w.Push(repo, "support/v1"), Small)
	w.restored(j, "build", service.StepMainExact)
	w.save(j, "build", true)
	w.FinishJob(j, "success")
	w.restored(w.start(w.Push(repo, "support/v1"), Small), "build", service.StepBranchExact)
	w.restored(w.start(w.PullRequestRun(w.PullRequest(repo, "support/v1", "c")), Small), "build", service.StepBranchExact)
	w.Commit(repo, "support/v1", map[string]string{"deps.txt": "3\n"})
	w.restored(w.start(w.Push(repo, "support/v1"), Small), "build", service.StepBranchNewest)

	// A read run reads main's.
	w.restored(w.start(w.Event(repo, "workflow_dispatch", "main"), Small), "build", service.StepMainExact)
}

func newKey(t T, w *world) {
	repo := w.consumer("app")
	j := w.start(w.Push(repo, "main"), Small)
	w.restored(j, "build", service.StepNone)
	w.save(j, "build", true)
	w.FinishJob(j, "success")
	// A pull request of main's key starts from main's, and holds it while
	// its run is unfinished.
	w.Commit(repo, "old", map[string]string{"deps.txt": "1\n"})
	held := w.start(w.PullRequestRun(w.PullRequest(repo, "main", "old")), Small)
	w.restored(held, "build", service.StepMainExact)
	// Main moves its key and saves: its disk of the old key is replaced, but
	// kept while the pull request's run is unfinished, so another of the
	// old key still starts from it.
	w.Commit(repo, "main", map[string]string{"deps.txt": "2\n"})
	j = w.start(w.Push(repo, "main"), Small)
	w.restored(j, "build", service.StepMainNewest)
	w.save(j, "build", true)
	w.FinishJob(j, "success")
	w.Commit(repo, "older", map[string]string{"deps.txt": "1\n"})
	older := w.PullRequest(repo, "main", "older")
	also := w.start(w.PullRequestRun(older), Small)
	w.restored(also, "build", service.StepMainExact)
	// Once both runs have finished, the old key's disk is gone: neither a
	// new run of the old key nor a maintainer's rerun of one that chose it
	// starts from it.
	w.FinishJob(held, "success")
	w.FinishJob(also, "success")
	w.restored(w.start(w.PullRequestRun(older), Small), "build", service.StepMainNewest)
	w.restored(w.start(w.Rerun(held.Run), Small), "build", service.StepMainNewest)
}

func oneWorkDisk(t T, w *world) {
	repo := w.consumer("app")
	run := w.Push(repo, "main")
	j := w.start(run, Small)
	// While it's being made, a repeated request is answered about the same
	// disk.
	w.HoldDisks(true)
	for range 2 {
		a, d := w.c.Disk(w.Token(j))
		if a.Status != http.StatusOK || d.State != "making" || d.Restored != service.StepNone {
			t.Fatalf("a held work disk: %d %+v", a.Status, d)
		}
	}
	w.HoldDisks(false)
	if got := w.disk(j); got != service.StepNone {
		t.Fatalf("a work disk came from %s", got)
	}
	// A job saves each cache once: a repeat before its commit names the
	// same version, and one after is refused; it may save another cache.
	_, first := w.c.Save(w.Token(j), "build")
	_, again := w.c.Save(w.Token(j), "build")
	if first.Prefix == "" || again.Prefix != first.Prefix {
		t.Fatalf("a repeated save named %q, then %q", first.Prefix, again.Prefix)
	}
	w.CacheStorage().Put(first.Bucket, first.Prefix+"shard-000.tar.zst", []byte("build"))
	a, c := w.c.Commit(w.Token(j), "build")
	w.status("a commit", a, http.StatusOK)
	a, again2 := w.c.Commit(w.Token(j), "build")
	w.status("a repeated commit", a, http.StatusOK)
	if again2 != c {
		t.Fatalf("a repeated commit answered %+v, then %+v", c, again2)
	}
	a, _ = w.c.Save(w.Token(j), "build")
	w.status("a save of a cache the job has saved", a, http.StatusForbidden)
	w.save(j, "tools", true)
	w.FinishJob(j, "success")
	next := w.start(w.Push(repo, "main"), Small)
	w.restored(next, "build", service.StepMainExact)
	w.restored(next, "tools", service.StepMainExact)
}

func saves(t T, w *world) {
	repo := w.consumer("app")
	j := w.start(w.Push(repo, "main"), Small)
	// A commit names a cache the job is saving, with something uploaded.
	a, _ := w.c.Commit(w.Token(j), "build")
	w.status("a commit of a cache the job isn't saving", a, http.StatusForbidden)
	a, s := w.c.Save(w.Token(j), "build")
	w.status("a save", a, http.StatusOK)
	a, _ = w.c.Commit(w.Token(j), "build")
	w.status("a commit with nothing uploaded", a, http.StatusForbidden)
	w.CacheStorage().Put(s.Bucket, s.Prefix+"shard-000.tar.zst", []byte("x"))
	a, _ = w.c.Commit(w.Token(j), "build")
	w.status("a commit", a, http.StatusOK)
	// A job that fails saves nothing.
	w.FinishJob(j, "failure")
	j = w.start(w.Push(repo, "main"), Small)
	w.restored(j, "build", service.StepNone)
	// Nor does one that didn't commit.
	w.c.Save(w.Token(j), "build")
	w.FinishJob(j, "success")
	j = w.start(w.Push(repo, "main"), Small)
	w.restored(j, "build", service.StepNone)
	w.save(j, "build", true)
	w.FinishJob(j, "success")
	w.restored(w.start(w.Push(repo, "main"), Small), "build", service.StepMainExact)

	// A scope that saves nothing says so.
	for _, run := range []service.Run{w.Event(repo, "workflow_dispatch", "main"), w.Tag(repo, "v1")} {
		j = w.start(run, Small)
		w.restored(j, "build", service.StepMainExact)
		w.save(j, "build", false)
		w.FinishJob(j, "success")
	}
}

func overlappingSaves(t T, w *world) {
	repo := w.consumer("app")
	first, later := w.start(w.Push(repo, "main"), Small), w.start(w.Push(repo, "main"), Small)
	prefix := map[service.Job]string{}
	for _, j := range []service.Job{first, later} {
		a, s := w.c.Save(w.Token(j), "build")
		w.status("a save", a, http.StatusOK)
		w.CacheStorage().Put(s.Bucket, s.Prefix+"shard-000.tar.zst", []byte(s.Prefix))
		a, _ = w.c.Commit(w.Token(j), "build")
		w.status("a commit", a, http.StatusOK)
		prefix[j] = s.Prefix
		time.Sleep(10 * time.Millisecond)
	}
	// The later save's job finishes first: it's still the newer, and the
	// one kept.
	w.FinishJob(later, "success")
	w.FinishJob(first, "success")
	j := w.start(w.Push(repo, "main"), Small)
	a, r := w.c.Restore(w.Token(j), "build")
	w.status("a restore", a, http.StatusOK)
	if r.Restored != service.StepMainExact || len(r.Objects) != 1 || !strings.HasPrefix(r.Objects[0].Name, prefix[later]) {
		t.Fatalf("restored %s %+v; want main's version saved later, under %s", r.Restored, r.Objects, prefix[later])
	}
}

func wholeVersions(t T, w *world) {
	repo := w.consumer("app")
	j := w.start(w.Push(repo, "main"), Small)
	// More objects than a version holds: refused.
	a, s := w.c.Save(w.Token(j), "build")
	w.status("a save", a, http.StatusOK)
	for i := range 1001 {
		w.CacheStorage().Put(s.Bucket, fmt.Sprintf("%sshard-%04d", s.Prefix, i), []byte("x"))
	}
	a, _ = w.c.Commit(w.Token(j), "build")
	w.status("a commit of 1,001 objects", a, http.StatusForbidden)
	// Objects whose names fit, but whose record doesn't: 1,000 of 500
	// bytes, 500,000 bytes of names, more than 512 KiB to record.
	m := w.start(w.Push(repo, "main"), Small)
	a, s = w.c.Save(w.Token(m), "build")
	w.status("a save", a, http.StatusOK)
	for i := range 1000 {
		head := fmt.Sprintf("%sshard-%04d-", s.Prefix, i)
		w.CacheStorage().Put(s.Bucket, head+strings.Repeat("x", 500-len(head)), []byte("x"))
	}
	a, _ = w.c.Commit(w.Token(m), "build")
	w.status("a commit whose record is too large", a, http.StatusForbidden)
	// A version one of whose objects changed after its commit is never
	// restored, and replaces nothing.
	k := w.start(w.Push(repo, "main"), Small)
	a, s = w.c.Save(w.Token(k), "tools")
	w.status("a save", a, http.StatusOK)
	w.CacheStorage().Put(s.Bucket, s.Prefix+"shard-000.tar.zst", []byte("first"))
	a, _ = w.c.Commit(w.Token(k), "tools")
	w.status("a commit", a, http.StatusOK)
	// A commit asked again is answered from what was committed, without
	// listing again: the bucket's listings failing doesn't matter.
	list := "/storage/v1/b/" + s.Bucket + "/o"
	w.CacheStorage().Faults().Add(fault.Rule{Method: http.MethodGet, Match: func(r *http.Request) bool { return r.URL.Path == list },
		Respond: fault.Status(http.StatusForbidden, nil, `{"error": {"code": 403}}`)})
	a, c := w.c.Commit(w.Token(k), "tools")
	w.CacheStorage().Faults().Reset()
	w.status("a commit asked again", a, http.StatusOK)
	if c.Objects != 1 {
		t.Fatalf("a commit asked again: %+v; want the one object committed", c)
	}
	w.CacheStorage().Put(s.Bucket, s.Prefix+"shard-000.tar.zst", []byte("changed after its commit"))
	w.FinishJob(k, "success")
	w.FinishJob(m, "success")
	w.FinishJob(j, "success")
	next := w.start(w.Push(repo, "main"), Small)
	w.restored(next, "tools", service.StepNone)
	w.restored(next, "build", service.StepNone)
}

func refusals(t T, w *world) {
	repo := w.consumer("app")
	j := w.start(w.Push(repo, "main"), Small)
	for _, name := range []string{"other", "Build", "", "build-", strings.Repeat("a", 33)} {
		a, _ := w.c.Restore(w.Token(j), name)
		w.status("a restore of the cache "+strconv.Quote(name), a, http.StatusForbidden)
		a, _ = w.c.Save(w.Token(j), name)
		w.status("a save of the cache "+strconv.Quote(name), a, http.StatusForbidden)
	}

	// A cache's listed file that doesn't exist, and a .github/runners.json
	// that isn't valid, a "disks" or "images" field included.
	w.Commit(repo, "main", map[string]string{"deps.txt": ""})
	gone := w.start(w.Push(repo, "main"), Small)
	a, _ := w.c.Restore(w.Token(gone), "build")
	if a.Status != http.StatusForbidden || !strings.Contains(a.Reason, "deps.txt") {
		t.Fatalf("a cache whose listed file doesn't exist: %d %q", a.Status, a.Reason)
	}
	for _, file := range []string{`{"caches": {"tools": {}}, "images": {"build": ".github/images/build"}}`, `{"caches": {"tools": {"from": "x"}}}`, `[]`,
		`{"caches": {"tools": null}}`, `{"caches": {"tools": {"FROM": []}}}`, `{"disks": {"tools": {}}}`} {
		w.Commit(repo, "main", map[string]string{consumer.RunnersFile: file})
		invalid := w.start(w.Push(repo, "main"), Small)
		a, _ = w.c.Restore(w.Token(invalid), "tools")
		w.status("a cache of a .github/runners.json that isn't valid, "+file, a, http.StatusForbidden)
	}

	// GitHub unavailable: the service confirms nothing, and the caller
	// asks again.
	w.Unavailable(true)
	a, _ = w.c.Restore(w.Token(j), "tools")
	w.status("a restore with GitHub unavailable", a, http.StatusServiceUnavailable)
	a, _ = w.c.Save(w.Token(j), "tools")
	w.status("a save with GitHub unavailable", a, http.StatusServiceUnavailable)
	_, err := w.StartJob(w.Push(repo, "main"), Small)
	w.Unavailable(false)
	if !errors.Is(err, service.ErrUnavailable) {
		t.Fatalf("a job's check with GitHub unavailable: %v", err)
	}
	w.restored(j, "tools", service.StepNone)
	w.save(j, "tools", true)
}
