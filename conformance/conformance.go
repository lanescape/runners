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
		one("a run not approved left unbound and refused disks and saves", unapproved),
		one("a job on a machine made for another run's job", anotherRunsMachine),
		one("disk requests from a run's own machine only", diskRequests),
		one("work disks kept to their repository", ownRepository),
		one("a work disk in the run's own scope from each step of the restore order", restoreOrder),
		one("a new key's save replaces the old key's disk once no unfinished run chose it", newKey),
		one("one work disk per job", oneWorkDisk),
		one("saves asked for", saves),
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
// long-lived branch, support/v1, and two families of work disks, build,
// keyed by deps.txt, and cache, with one key.
var Files = map[string]string{
	consumer.RunnersFile: `{"branches": {"support/v1": "v1"},
 "disks": {"build": {"from": ["deps.txt"]}, "cache": {}}}`,
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

// disk asks for a job's work disk of a family, again while it's being
// made, as the disk action does, until it's ready, and returns where it
// came from.
func (w *world) disk(j service.Job, family string) string {
	w.t.Helper()
	restored := ""
	for end := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		a, d := w.c.Disk(w.Token(j), family)
		w.status("job "+strconv.FormatInt(j.ID, 10)+"'s work disk of "+family, a, http.StatusOK)
		if restored != "" && d.Restored != restored {
			w.t.Fatalf("job %d's work disk came from %s, then %s", j.ID, restored, d.Restored)
		}
		restored = d.Restored
		if d.State == "ready" {
			return restored
		}
		if time.Now().After(end) {
			w.t.Fatalf("job %d's work disk of %s was never ready", j.ID, family)
		}
	}
}

// restored checks where a job's work disk of a family came from.
func (w *world) restored(j service.Job, family, want string) {
	w.t.Helper()
	if got := w.disk(j, family); got != want {
		w.t.Fatalf("job %d (run %d, %s) has a work disk of %s from %s; want %s", j.ID, j.Run.ID, j.Run.Event, family, got, want)
	}
}

// save asks for a job's work disk to be saved, which must be answered.
func (w *world) save(j service.Job, saves bool) {
	w.t.Helper()
	a, s := w.c.Save(w.Token(j))
	w.status("job "+strconv.FormatInt(j.ID, 10)+"'s save", a, http.StatusOK)
	if s.Saves != saves {
		w.t.Fatalf("job %d (run %d, %s) asked to save: %+v; want saves %v", j.ID, j.Run.ID, j.Run.Event, s, saves)
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
	w.save(j, true)
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
	// Nothing that needs a scope: no work disk, no save.
	a, _ := w.c.Disk(w.Token(j), "build")
	w.status("an unbound run's work disk request", a, http.StatusForbidden)
	a, _ = w.c.Save(w.Token(j))
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
	w.save(j, false)
}

func diskRequests(t T, w *world) {
	repo := w.consumer("app")
	run := w.Push(repo, "main")
	j := w.start(run, Small)
	j2 := w.start(run, Small)
	w.restored(j, "build", service.StepNone)
	// A request with no token, or one that isn't a machine's, is refused.
	a, _ := w.c.Disk("", "build")
	if a.Status != http.StatusUnauthorized {
		t.Fatalf("a work disk request with no token answered %d", a.Status)
	}
	a, _ = w.c.Disk("not.a.token", "build")
	if a.Status != http.StatusUnauthorized {
		t.Fatalf("a work disk request with a token that isn't one answered %d", a.Status)
	}
	// A machine whose job has finished can ask for nothing.
	w.FinishJob(j, "success")
	a, _ = w.c.Disk(w.Token(j), "build")
	w.status("a finished job's work disk request", a, http.StatusForbidden)
	a, _ = w.c.Save(w.Token(j))
	w.status("a finished job's save", a, http.StatusForbidden)
	// The run's other jobs still can, each with a disk of its own.
	w.restored(j2, "build", service.StepNone)
	w.save(j2, true)
}

func ownRepository(t T, w *world) {
	a, b := w.consumer("one"), w.consumer("two")
	ja := w.start(w.Push(a, "main"), Small)
	w.restored(ja, "build", service.StepNone)
	w.save(ja, true)
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
	w.save(j, true)
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
	w.save(j, true)
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
	w.save(j, true)
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
	w.save(j, true)
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
	w.save(j, true)
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
	// disk, and another family is refused.
	w.HoldDisks(true)
	for range 2 {
		a, d := w.c.Disk(w.Token(j), "build")
		if a.Status != http.StatusOK || d.State != "making" || d.Restored != service.StepNone {
			t.Fatalf("a held work disk: %d %+v", a.Status, d)
		}
	}
	a, _ := w.c.Disk(w.Token(j), "cache")
	w.status("a second family while the first is made", a, http.StatusForbidden)
	w.HoldDisks(false)
	w.restored(j, "build", service.StepNone)
	w.restored(j, "build", service.StepNone)
	a, _ = w.c.Disk(w.Token(j), "cache")
	w.status("a second family", a, http.StatusForbidden)
	// Another job of the run may have the other family.
	w.restored(w.start(run, Small), "cache", service.StepNone)
}

func saves(t T, w *world) {
	repo := w.consumer("app")
	j := w.start(w.Push(repo, "main"), Small)
	a, _ := w.c.Save(w.Token(j))
	w.status("a save without a work disk", a, http.StatusForbidden)
	w.disk(j, "build")
	w.save(j, true)
	w.save(j, true)
	// A job that fails saves nothing.
	w.FinishJob(j, "failure")
	j = w.start(w.Push(repo, "main"), Small)
	w.restored(j, "build", service.StepNone)
	// Nor does one that didn't ask.
	w.FinishJob(j, "success")
	j = w.start(w.Push(repo, "main"), Small)
	w.restored(j, "build", service.StepNone)
	w.save(j, true)
	w.FinishJob(j, "success")
	w.restored(w.start(w.Push(repo, "main"), Small), "build", service.StepMainExact)

	// A scope that saves nothing says so.
	for _, run := range []service.Run{w.Event(repo, "workflow_dispatch", "main"), w.Tag(repo, "v1")} {
		j = w.start(run, Small)
		w.restored(j, "build", service.StepMainExact)
		a, s := w.c.Save(w.Token(j))
		if a.Status != http.StatusOK || s.Saves || s.Reason == "" {
			t.Fatalf("a save in a scope that saves nothing: %d %+v", a.Status, s)
		}
		w.FinishJob(j, "success")
	}
}

func refusals(t T, w *world) {
	repo := w.consumer("app")
	j := w.start(w.Push(repo, "main"), Small)
	for _, name := range []string{"other", "Build", "", "build-", strings.Repeat("a", 33)} {
		a, _ := w.c.Disk(w.Token(j), name)
		w.status("a request for the family "+strconv.Quote(name), a, http.StatusForbidden)
	}

	// A family's listed file that doesn't exist, and a .github/runners.json
	// that isn't valid, an "images" field included.
	w.Commit(repo, "main", map[string]string{"deps.txt": ""})
	gone := w.start(w.Push(repo, "main"), Small)
	a, _ := w.c.Disk(w.Token(gone), "build")
	if a.Status != http.StatusForbidden || !strings.Contains(a.Reason, "deps.txt") {
		t.Fatalf("a family whose listed file doesn't exist: %d %q", a.Status, a.Reason)
	}
	for _, file := range []string{`{"disks": {"cache": {}}, "images": {"build": ".github/images/build"}}`, `{"disks": {"cache": {"from": "x"}}}`, `[]`,
		`{"disks": {"cache": null}}`, `{"disks": {"cache": {"FROM": []}}}`} {
		w.Commit(repo, "main", map[string]string{consumer.RunnersFile: file})
		invalid := w.start(w.Push(repo, "main"), Small)
		a, _ = w.c.Disk(w.Token(invalid), "cache")
		w.status("a family of a .github/runners.json that isn't valid, "+file, a, http.StatusForbidden)
	}

	// GitHub unavailable: the service confirms nothing, and the caller
	// asks again.
	w.Unavailable(true)
	a, _ = w.c.Disk(w.Token(j), "cache")
	w.status("a work disk request with GitHub unavailable", a, http.StatusServiceUnavailable)
	a, _ = w.c.Save(w.Token(j))
	w.status("a save with GitHub unavailable", a, http.StatusServiceUnavailable)
	_, err := w.StartJob(w.Push(repo, "main"), Small)
	w.Unavailable(false)
	if !errors.Is(err, service.ErrUnavailable) {
		t.Fatalf("a job's check with GitHub unavailable: %v", err)
	}
	w.restored(j, "cache", service.StepNone)
	w.save(j, true)
}
