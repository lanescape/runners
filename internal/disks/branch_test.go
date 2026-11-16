package disks_test

import (
	"strconv"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/consumer"
	"github.com/lanescape/runners/internal/disks"
)

// listsV1 is a .github/runners.json that lists support/v1 as the
// long-lived branch v1, and declares the tests' caches.
const listsV1 = `{"branches": {"support/v1": "v1"}, "caches": {"build": {}, "tools": {}, "other": {}}}`

// listV1 lists support/v1 as the long-lived branch v1 on the default
// branch, and gives support/v1 a commit of its own, which declares the
// same caches.
func (e *env) listV1() {
	e.t.Helper()
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: listsV1})
	e.repo.Commit("support/v1", map[string]string{"v1": "1"})
}

// branchRun is a push to support/v1, bound branch-v1.
func (e *env) branchRun() *ghemu.Run {
	e.t.Helper()
	run := e.gh.PushRun(e.repo, "support/v1")
	if s := e.bind(run); s != "branch-v1" {
		e.t.Fatalf("a push to a listed branch bound %s", s)
	}
	return run
}

func (e *env) saveBranch(key string) string {
	e.t.Helper()
	return e.save(e.branchRun(), sel(key, "branch-v1"))
}

func (e *env) step(run *ghemu.Run, label string) string {
	e.t.Helper()
	rec, ok := e.record(e.choice(run, label))
	if !ok {
		e.t.Fatalf("no choice for %s", label)
	}
	s, _ := rec["step"].(string)
	return s
}

// A long-lived branch's push restores its own, then main's; a pull request
// into it restores its own, then the branch's, never main's.
func TestALongLivedBranchsRestoreOrder(t *testing.T) {
	e := setup(t)
	e.listV1()
	main1 := e.saveMain(key1)
	// A new branch starts warm, from main's.
	run := e.branchRun()
	first := e.start(run, ghemu.JobSpec{Name: "a", Labels: []string{sel(key1, "branch-v1")}})
	if src := first.restored(); src != main1 || e.step(run, sel(key1, "branch-v1")) != disks.StepMainExact {
		t.Fatalf("a branch's first push restored %q", src)
	}
	first.finish(true)
	run = e.branchRun()
	b := e.start(run, ghemu.JobSpec{Name: "b", Labels: []string{sel(key1, "branch-v1")}})
	if src := b.restored(); src != first.saved || e.step(run, sel(key1, "branch-v1")) != disks.StepBranchExact {
		t.Fatalf("the branch's own for the key: %q, step %s", src, e.step(run, sel(key1, "branch-v1")))
	}
	b.finish(false)
	e.clock.Advance(time.Minute)
	v1 := e.saveBranch(key2)
	// A run has one key per cache: another key is another run's.
	run = e.branchRun()
	if src := e.start(run, ghemu.JobSpec{Name: "c", Labels: []string{sel(zero, "branch-v1")}}).restored(); src != v1 ||
		e.step(run, sel(zero, "branch-v1")) != disks.StepBranchNewest {
		t.Fatalf("the branch's newest: %q, want %s", src, v1)
	}

	e.repo.Commit("fix", map[string]string{"y": "1"})
	p := e.gh.PullRequest(e.repo, "support/v1", e.repo, "fix", e.gh.User(42, "maintainer"))
	prun := e.pullRun(p)
	pr := "pr" + strconv.Itoa(p.Number)
	if src := e.start(prun, ghemu.JobSpec{Name: "d", Labels: []string{sel(key2, pr)}}).restored(); src != v1 ||
		e.step(prun, sel(key2, pr)) != disks.StepBranchExact {
		t.Fatalf("a pull request into the branch: %q, want %s", src, v1)
	}
	prun = e.pullRun(p)
	if src := e.start(prun, ghemu.JobSpec{Name: "e", Labels: []string{sel(key3, pr)}}).restored(); src != v1 ||
		e.step(prun, sel(key3, pr)) != disks.StepBranchNewest {
		t.Fatalf("a pull request into the branch, another key: %q, want %s", src, v1)
	}
}

// A long-lived branch keeps one version of each cache, its newest save, as
// main does, and all of its versions go once it's no longer listed.
func TestALongLivedBranchsCachesGoWhenItsDelisted(t *testing.T) {
	e := setup(t)
	e.listV1()
	k1 := e.saveBranch(key1)
	e.clock.Advance(time.Minute)
	k2 := e.saveBranch(key2)
	if e.has(k1) || !e.has(k2) {
		t.Fatalf("key1 %v, key2 %v; want the branch's newest save alone", e.has(k1), e.has(k2))
	}
	main1 := e.saveMain(key1)
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: declares})
	e.pass()
	if e.has(k2) || !e.has(main1) {
		t.Fatalf("after the branch was delisted: the branch's %v, main's %v", e.has(k2), e.has(main1))
	}
}

// A long-lived branch's versions go once its branch is gone, even while
// it's listed.
func TestALongLivedBranchsCachesGoWithItsBranch(t *testing.T) {
	e := setup(t)
	e.listV1()
	k1 := e.saveBranch(key1)
	e.pass()
	if !e.has(k1) {
		t.Fatal("a listed branch's version went")
	}
	e.repo.DeleteBranch("support/v1")
	e.pass()
	if e.has(k1) {
		t.Fatal("a deleted branch's version stayed")
	}
}
