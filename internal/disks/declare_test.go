package disks_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/consumer"
)

// armBuild is a label of the arm64 shape whose job asks for the family
// build, in main's scope.
func armBuild(key string) string {
	return "lanescape-16vcpu-linux-arm64-disk-build-" + key + "-main"
}

// Removing a family's declaration from the default branch deletes every disk
// of it in main's scope, of every key and both architectures, at the next
// pass, and no other family's; and nothing of a pull request's.
func TestRemovingADeclarationDeletesTheFamily(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	arm := e.saveMainLabel(armBuild(key1))
	tool := e.saveMainLabel(tools(key1, "main"))
	p, _ := e.pull()
	pr := e.savePull(p, sel(key1, "pr"+strconv.Itoa(p.Number)))
	e.pass()
	if !e.has(k1) || !e.has(arm) || !e.has(tool) || !e.has(pr) {
		t.Fatal("a declared family's disk went")
	}
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: `{"disks": {"tools": {}}}`})
	e.pass()
	if e.has(k1) || e.has(arm) {
		t.Fatalf("amd64 %v, arm64 %v; a family no longer declared kept its disks", e.has(k1), e.has(arm))
	}
	if !e.has(tool) || !e.has(pr) {
		t.Fatalf("tools %v, the pull request's %v; want kept", e.has(tool), e.has(pr))
	}
	if !e.logged("its family is no longer declared") {
		t.Fatal("no reason logged")
	}
	// Declared again, it starts afresh.
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: declares})
	if src := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}).source(); src != "" {
		t.Fatalf("a family declared again restored %q", src)
	}
}

// A default branch with no .github/runners.json declares nothing.
func TestAnAbsentFileDeclaresNothing(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: ""})
	e.pass()
	if e.has(k1) {
		t.Fatal("main's disk outlived the file that declared its family")
	}
}

// A file that isn't valid deletes nothing: not main's, nor any long-lived
// branch's, which an invalid default branch's file no longer lists either.
// A long-lived branch's own invalid file deletes nothing of its scope.
func TestAnInvalidFileDeletesNothing(t *testing.T) {
	e := setup(t)
	e.listV1()
	m := e.saveMain(key1)
	b := e.saveBranch(key1)
	for _, bad := range []string{`{"disks": `, `{"disks": {}, "images": {}}`, `{"disks": {"Build": {}}}`} {
		e.repo.Commit("main", map[string]string{consumer.RunnersFile: bad})
		e.pass()
		if !e.has(m) || !e.has(b) {
			t.Fatalf("with the default branch's file %s: main's %v, the branch's %v; want both kept", bad, e.has(m), e.has(b))
		}
	}
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: listsV1})
	e.repo.Commit("support/v1", map[string]string{consumer.RunnersFile: `{"disks": {"build": "x"}}`})
	e.pass()
	if !e.has(m) || !e.has(b) {
		t.Fatalf("with the branch's file invalid: main's %v, the branch's %v; want both kept", e.has(m), e.has(b))
	}
	if !e.logged("deleting nothing by them") {
		t.Fatal("no warning that the declarations couldn't be read")
	}
}

// A long-lived branch's scope is kept by its own file's declarations, not
// the default branch's.
func TestALongLivedBranchsOwnFileKeepsItsFamilies(t *testing.T) {
	e := setup(t)
	e.listV1()
	m := e.saveMain(key1)
	b := e.saveBranch(key1)
	e.repo.Commit("support/v1", map[string]string{consumer.RunnersFile: `{"disks": {"tools": {}}}`})
	e.pass()
	if e.has(b) || !e.has(m) {
		t.Fatalf("the branch's %v, main's %v; want the branch's alone gone", e.has(b), e.has(m))
	}
	e.repo.Commit("support/v1", map[string]string{consumer.RunnersFile: declares})
	b = e.saveBranch(key1)
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: `{"branches": {"support/v1": "v1"}, "disks": {"tools": {}}}`})
	e.pass()
	if !e.has(b) || e.has(m) {
		t.Fatalf("the branch's %v, main's %v; want main's alone gone", e.has(b), e.has(m))
	}
}

// What GitHub can't confirm deletes nothing: a pass that can't read the
// file, or the branch's head, keeps the family, and the next pass that can
// deletes it.
func TestUnreadableDeclarationsDeleteNothing(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: `{"disks": {"tools": {}}}`})
	for _, path := range []string{"/repos/lanescape/app/contents/", "/repos/lanescape/app/git/ref/"} {
		e.gh.Faults().Add(fault.Rule{PathPrefix: path, Respond: fault.Status(http.StatusBadGateway, nil, `{"message": "Bad Gateway"}`)})
		e.pass()
		e.gh.Faults().Reset()
		if !e.has(k1) {
			t.Fatalf("a family went while %s couldn't be read", path)
		}
	}
	e.gh.RateLimit("/repos/lanescape/app/contents/", 0, time.Second)
	e.pass()
	e.gh.Faults().Reset()
	if !e.has(k1) {
		t.Fatal("a family went while GitHub limited the service's requests")
	}
	// Once GitHub's limit resets, a pass reads the file, and the family goes.
	e.eventually("a family no longer declared goes once GitHub can be read", func() bool {
		e.pass()
		return !e.has(k1)
	})
}

// Removing a declaration never deletes a disk an unfinished run's choice
// names: it goes once the run has finished.
func TestRemovingADeclarationKeepsWhatAnUnfinishedRunChose(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if jb.source() != k1 {
		t.Fatal("no restore")
	}
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: `{}`})
	e.pass()
	if !e.has(k1) {
		t.Fatal("a disk an unfinished run's choice names went with its declaration")
	}
	jb.finish(false)
	e.eventually("the run is finished", func() bool { s, _ := e.gh.RunState(run); return s == "completed" })
	e.pass()
	if e.has(k1) {
		t.Fatal("a family no longer declared kept its disk after the run that chose it finished")
	}
}

// notFound answers 404, as GitHub does for everything of a repository the
// App can't read.
var notFound = fault.Status(http.StatusNotFound, nil, `{"message": "Not Found"}`)

// A repository GitHub answers 404 for, as it does once the App can no
// longer read it, deletes nothing: not main's disks, a long-lived branch's
// or a pull request's, nor a family whose declaration was removed
// meanwhile; its pull request isn't taken as closed, and a finished run's
// restore choice isn't dropped. Once GitHub shows it again, its
// declarations decide as before.
func TestARepositoryGitHubDoesntFindDeletesNothing(t *testing.T) {
	e := setup(t)
	e.listV1()
	m := e.saveMain(key1)
	b := e.saveBranch(key1)
	p, _ := e.pull()
	pr := e.savePull(p, sel(key1, "pr"+strconv.Itoa(p.Number)))
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if jb.source() != m {
		t.Fatal("no restore")
	}
	choice := "choices/5001/" + strconv.FormatInt(run.ID, 10) + "/" + choiceHash(sel(key1, "main")) + ".json"
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: `{"branches": {"support/v1": "v1"}, "disks": {"tools": {}}}`})
	e.gh.Faults().Add(fault.Rule{PathPrefix: "/repos", Respond: notFound})
	jb.finish(false)
	e.eventually("the run is finished", func() bool { s, _ := e.gh.RunState(run); return s == "completed" })
	e.pass()
	e.gh.Faults().Reset()
	if !e.has(m) || !e.has(b) || !e.has(pr) {
		t.Fatalf("main's %v, the branch's %v, the pull request's %v; want all kept while GitHub answered 404", e.has(m), e.has(b), e.has(pr))
	}
	if _, ok := e.record(choice); !ok {
		t.Fatal("a finished run's choice was dropped while GitHub answered 404 for its repository")
	}
	if _, ok := e.record("closures/5001/pr" + strconv.Itoa(p.Number) + ".json"); ok {
		t.Fatal("a pull request was recorded closed while GitHub answered 404 for its repository")
	}
	if !e.logged("GitHub answers 404 for it") {
		t.Fatal("no warning naming the repository GitHub doesn't find")
	}
	e.pass()
	if _, ok := e.record(choice); ok {
		t.Fatal("a finished run's choice wasn't dropped once GitHub showed its repository again")
	}
	if e.has(m) {
		t.Fatal("a family no longer declared kept its disk once GitHub showed its repository again")
	}
	if !e.has(b) || !e.has(pr) {
		t.Fatalf("the branch's %v, the pull request's %v; want both kept", e.has(b), e.has(pr))
	}
}

// Access lost during a pass deletes nothing: a 404 for a pull request, or
// for the default branch's file, counts as its absence only if GitHub,
// asked again, still shows the repository.
func TestAccessLostDuringAPassDeletesNothing(t *testing.T) {
	for name, path := range map[string]string{
		"a pull request":            "/repos/lanescape/app/pulls/",
		"the default branch's file": "/repos/lanescape/app/contents/" + consumer.RunnersFile,
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			m := e.saveMain(key1)
			p, _ := e.pull()
			pr := e.savePull(p, sel(key1, "pr"+strconv.Itoa(p.Number)))
			// From the first read of path on, GitHub answers 404 for
			// everything of the repository's.
			var lost atomic.Bool
			e.gh.Faults().Add(fault.Rule{PathPrefix: "/repos", Respond: notFound, Match: func(r *http.Request) bool {
				if strings.HasPrefix(r.URL.Path, path) {
					lost.Store(true)
				}
				return lost.Load()
			}})
			e.pass()
			e.gh.Faults().Reset()
			if !lost.Load() {
				t.Fatalf("the pass never read %s", path)
			}
			if !e.has(m) || !e.has(pr) {
				t.Fatalf("main's %v, the pull request's %v; want both kept", e.has(m), e.has(pr))
			}
			if _, ok := e.record("closures/5001/pr" + strconv.Itoa(p.Number) + ".json"); ok {
				t.Fatal("a pull request was recorded closed once access was lost")
			}
		})
	}
}

// A repository GitHub can't be read for deletes nothing, not even what a
// recorded closing would: once it can be, that goes.
func TestAnUnreadableRepositoryDeletesNothing(t *testing.T) {
	e := setup(t)
	p, _ := e.pull()
	pr := "pr" + strconv.Itoa(p.Number)
	img := e.savePull(p, sel(key1, pr))
	delivery, _ := json.Marshal(map[string]any{"action": "closed",
		"pull_request": map[string]any{"number": p.Number, "closed_at": e.clock.Now().UTC().Add(time.Second).Format(time.RFC3339)},
		"repository":   map[string]any{"id": repoID, "owner": map[string]any{"id": orgID}}})
	if err := e.disks.RecordEvent(context.Background(), map[string]string{"event": "pull_request"}, delivery); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	e.gh.Faults().Add(fault.Rule{PathPrefix: "/repositories/", Respond: fault.Status(http.StatusBadGateway, nil, `{"message": "Bad Gateway"}`)})
	e.pass()
	e.gh.Faults().Reset()
	if !e.has(img) {
		t.Fatal("a disk went while GitHub couldn't be read for its repository")
	}
	if !e.logged("deleting none of them this pass") {
		t.Fatal("no warning that the repository couldn't be read")
	}
	e.pass()
	if e.has(img) {
		t.Fatal("a disk saved before its pull request's closing outlived it")
	}
}

// A deleted repository's disks are kept, for a maintainer to remove by
// hand: GitHub's 404 for it is the same answer as for one the App can't
// read. So is an image of its with no record.
func TestADeletedRepositorysDisksAreKept(t *testing.T) {
	e := setup(t)
	m := e.saveMain(key1)
	orphan := e.saveMainLabel(tools(key1, "main"))
	p, _ := e.pull()
	pr := e.savePull(p, sel(key1, "pr"+strconv.Itoa(p.Number)))
	if err := e.objects.Delete(context.Background(), "images/5001/"+orphan+".json", mustGen(t, e, "images/5001/"+orphan+".json")); err != nil {
		t.Fatal(err)
	}
	e.gh.Delete(e.repo)
	e.pass()
	if !e.has(m) || !e.has(pr) || !e.has(orphan) {
		t.Fatalf("main's %v, the pull request's %v, the image with no record %v; a deleted repository's images went", e.has(m), e.has(pr),
			e.has(orphan))
	}
	if !e.logged("GitHub answers 404 for it") {
		t.Fatal("no warning naming the deleted repository")
	}
}

// A repository GitHub shows as another owner's, transferred, deletes
// nothing either; transferred back, it's the same consumer, with its disks.
func TestATransferredRepositorysDisksAreKept(t *testing.T) {
	e := setup(t)
	m := e.saveMain(key1)
	p, _ := e.pull()
	pr := e.savePull(p, sel(key1, "pr"+strconv.Itoa(p.Number)))
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	choice := "choices/5001/" + strconv.FormatInt(run.ID, 10) + "/" + choiceHash(sel(key1, "main")) + ".json"
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: `{"disks": {"tools": {}}}`})
	e.gh.Transfer(e.repo, e.gh.Org(1234, "elsewhere"))
	jb.finish(false)
	e.eventually("the run is finished", func() bool { s, _ := e.gh.RunState(run); return s == "completed" })
	e.pass()
	if !e.has(m) || !e.has(pr) {
		t.Fatalf("main's %v, the pull request's %v; a transferred repository's disks went", e.has(m), e.has(pr))
	}
	if _, ok := e.record(choice); !ok {
		t.Fatal("a finished run's choice was dropped while its repository was another owner's")
	}
	if !e.logged("another owner's") {
		t.Fatal("no warning naming the transferred repository")
	}
	e.gh.Transfer(e.repo, e.org)
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: declares})
	e.pass()
	if !e.has(m) || !e.has(pr) {
		t.Fatalf("main's %v, the pull request's %v; want both kept once transferred back", e.has(m), e.has(pr))
	}
}

// A directory where the default branch's .github/runners.json should be
// confirms nothing: it's no file's absence.
func TestADirectoryInPlaceOfTheFileDeletesNothing(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	e.gh.Faults().Add(fault.Rule{PathPrefix: "/repos/lanescape/app/contents/" + consumer.RunnersFile,
		Respond: fault.Status(http.StatusOK, nil, `[{"type": "file", "name": "x.json", "path": ".github/runners.json/x.json"}]`)})
	e.pass()
	e.gh.Faults().Reset()
	if !e.has(k1) {
		t.Fatal("main's disk went while its file was a directory")
	}
	if !e.logged("a directory") {
		t.Fatal("no warning that the file was a directory")
	}
}

// Entries the default branch's file lists that the service ignores, such as
// two branches sharing a scope's name, are logged with why, and their
// scopes' disks go, as no longer listed.
func TestIgnoredBranchEntriesAreLogged(t *testing.T) {
	e := setup(t)
	e.listV1()
	b := e.saveBranch(key1)
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: `{"branches": {"support/v1": "v1", "support/v2": "v1"}, "disks": {"build": {}}}`})
	e.pass()
	if e.has(b) {
		t.Fatal("a scope whose name two branches share kept its disks")
	}
	if !e.logged("is ignored") {
		t.Fatal("ignored entries weren't logged")
	}
}

// A file too large for GitHub's contents API is read as its blob; a blob
// GitHub doesn't find, of a file it showed, confirms nothing.
func TestALargeFilesMissingBlobDeletesNothing(t *testing.T) {
	e := setup(t)
	k1 := e.saveMain(key1)
	large := `{"disks": {"tools": {}}}` + strings.Repeat(" ", 2<<20)
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: large})
	e.gh.Faults().Add(fault.Rule{PathPrefix: "/repos/lanescape/app/git/blobs/",
		Respond: fault.Status(http.StatusNotFound, nil, `{"message": "Not Found"}`)})
	e.pass()
	e.gh.Faults().Reset()
	if !e.has(k1) {
		t.Fatal("main's disk went when its file's blob couldn't be read")
	}
	// Read whole, the file no longer declares the family.
	e.pass()
	if e.has(k1) {
		t.Fatal("a family no longer declared in a large file kept its disk")
	}
}

// A file read at a commit GitHub says it has no record of, as at a head
// moved meanwhile, confirms nothing, for main's scope or a branch's.
func TestAFileAtACommitGitHubDoesntFindDeletesNothing(t *testing.T) {
	e := setup(t)
	e.listV1()
	m := e.saveMain(key1)
	b := e.saveBranch(key1)
	e.gh.Faults().Add(fault.Rule{PathPrefix: "/repos/lanescape/app/contents/" + consumer.RunnersFile,
		Respond: fault.Status(http.StatusNotFound, nil, `{"message": "No commit found for the ref 0123456789abcdef0123456789abcdef01234567"}`)})
	e.pass()
	e.gh.Faults().Reset()
	if !e.has(m) || !e.has(b) {
		t.Fatalf("main's %v, the branch's %v; want both kept", e.has(m), e.has(b))
	}
}
