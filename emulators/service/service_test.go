package service_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/lanescape/runners/conformance"
	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/internal/binding"
)

// onGitHub is an emulator over a GitHub emulator the test runs, with a
// client for it.
func onGitHub(t *testing.T) (*service.Emulator, *ghemu.Emulator, *conformance.Client) {
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	e := service.New(service.WithGitHub(gh))
	t.Cleanup(e.Close)
	return e, gh, &conformance.Client{T: t, URL: e.URL()}
}

// Faults a test adds to the GitHub emulator's API don't reach the service
// emulator, which reads GitHub's state in-process; Unavailable does.
func TestGitHubFaultsDontReachTheEmulator(t *testing.T) {
	e, gh, c := onGitHub(t)
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	gh.Faults().Add(fault.Rule{Respond: fault.Status(http.StatusServiceUnavailable, nil, `{"message": "Service Unavailable"}`)})
	j, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := c.Disk(e.Token(j), "build"); a.Status != http.StatusOK {
		t.Fatalf("a work disk request with the GitHub emulator's API failing: %d", a.Status)
	}
	e.Unavailable(true)
	if a, _ := c.Disk(e.Token(j), "cache"); a.Status != http.StatusServiceUnavailable {
		t.Fatalf("a work disk request with GitHub unavailable: %d", a.Status)
	}
}

// A job's machine's metadata server mints its identity token, which the
// service takes, and names the service.
func TestMetadata(t *testing.T) {
	e, _, c := onGitHub(t)
	repo := e.Repository("app")
	j, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, e.Metadata(j)+path, nil)
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, url := get("/computeMetadata/v1/instance/attributes/lsr-service"); code != http.StatusOK || url != e.URL() {
		t.Fatalf("the service's address: %d %q", code, url)
	}
	code, tok := get("/computeMetadata/v1/instance/service-accounts/default/identity?audience=" + e.URL() + "&format=full")
	if code != http.StatusOK {
		t.Fatalf("the identity token: %d", code)
	}
	if a, _ := c.Save(tok); a.Status != http.StatusForbidden || !strings.Contains(a.Reason, "no work disk") {
		t.Fatalf("a request signed with the metadata server's token: %d %q", a.Status, a.Reason)
	}
}

// A job the test queues in the GitHub emulator itself, naming a protected
// environment, waits for its approval before it's played to its check.
func TestWorkflowJobs(t *testing.T) {
	e, gh, _ := onGitHub(t)
	rp := e.GitHubRepo(e.Repository("app"))
	gh.ProtectEnvironment(rp, "deploy")
	gr := gh.PushRun(rp, "main")
	wj := gh.Queue(gr, ghemu.JobSpec{Name: "deploy", Labels: []string{conformance.Small}, Environment: "deploy"})
	if _, err := e.StartWorkflowJob(wj); !errors.Is(err, service.ErrWaiting) {
		t.Fatalf("a job waiting for its environment: %v", err)
	}
	gh.Approve(gr, "deploy", gh.User(gh.NewID(), "reviewer"))
	j, err := e.StartWorkflowJob(wj)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.JobOf(wj); got != j || got.Run != e.RunOf(gr) || e.GitHubJob(j) != wj {
		t.Fatalf("the job's handle %+v, from %+v", got, j)
	}
	if scope, ok := e.Binding(j.Run); !ok || scope != "main" {
		t.Fatalf("the run is bound %q, %v", scope, ok)
	}
	e.FinishJob(j, "success")
	if got := gh.Job(wj.ID); got.Status != "completed" || got.Conclusion != "success" {
		t.Fatalf("the finished job: %s %s", got.Status, got.Conclusion)
	}
}

// A test reads what was attached and asked to be saved.
func TestWhatWasAttachedAndSaved(t *testing.T) {
	e, _, c := onGitHub(t)
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	j, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.WorkDisk(j); ok {
		t.Fatal("a work disk before the job asked")
	}
	c.Disk(e.Token(j), "build")
	c.Save(e.Token(j))
	d, ok := e.WorkDisk(j)
	if !ok || d.Family != "build" || d.Scope != "main" || d.Architecture != "amd64" || d.Restored != service.StepNone || !d.Ready || !d.Save {
		t.Fatalf("the work disk: %+v", d)
	}
	e.FinishJob(j, "success")
	if saved := e.SavedDisks(repo); len(saved) != 1 || saved[0].Key != d.Key || saved[0].Scope != "main" || saved[0].Job != j.ID {
		t.Fatalf("the saved disks: %+v", saved)
	}
}

// A pull request keeps one disk of each family, and a closed one's are
// never chosen again, even once it's reopened.
func TestPullRequestDisks(t *testing.T) {
	e, gh, c := onGitHub(t)
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	pr := e.PullRequest(repo, "main", "fix")
	saveOne := func(want string) {
		t.Helper()
		j, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
		if err != nil {
			t.Fatal(err)
		}
		if _, d := c.Disk(e.Token(j), "build"); d.Restored != want {
			t.Fatalf("restored %s, want %s", d.Restored, want)
		}
		c.Save(e.Token(j))
		e.FinishJob(j, "success")
	}
	saveOne(service.StepNone)
	pr = e.PushPullRequest(pr, map[string]string{"deps.txt": "2\n"})
	saveOne(service.StepPullRequestNewest)
	if saved := e.SavedDisks(repo); len(saved) != 1 {
		t.Fatalf("a pull request's disks of one family: %+v", saved)
	}
	p := e.GitHubPull(pr)
	gh.ClosePull(p)
	saveOne(service.StepNone)
	gh.ReopenPull(p)
	// What its runs saved up to when it was last seen closed is never
	// chosen again: it starts again from its base.
	saveOne(service.StepNone)
	saveOne(service.StepPullRequestExact)
}

// Main keeps one disk of each family too: a save under a new key replaces
// its disk of the old key, so a pull request still of the old key starts
// from main's newest.
func TestANewKeysSaveReplacesTheOldKeys(t *testing.T) {
	e, _, c := onGitHub(t)
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	e.Commit(repo, "old", map[string]string{"x": "1"})
	saveMain := func(want string) {
		t.Helper()
		j, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
		if err != nil {
			t.Fatal(err)
		}
		if _, d := c.Disk(e.Token(j), "build"); d.Restored != want {
			t.Fatalf("restored %s, want %s", d.Restored, want)
		}
		c.Save(e.Token(j))
		e.FinishJob(j, "success")
	}
	saveMain(service.StepNone)
	e.Commit(repo, "main", map[string]string{"deps.txt": "2\n"})
	saveMain(service.StepMainNewest)
	saved := e.SavedDisks(repo)
	if len(saved) != 1 || saved[0].Scope != "main" {
		t.Fatalf("main's disks of one family after a new key's save: %+v", saved)
	}
	j, err := e.StartJob(e.PullRequestRun(e.PullRequest(repo, "main", "old")), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	if _, d := c.Disk(e.Token(j), "build"); d.Restored != service.StepMainNewest {
		t.Fatalf("a pull request of the old key restored %s, want %s", d.Restored, service.StepMainNewest)
	}
}

// A save from a run bound before its pull request was closed, finishing
// after the reopened pull request saved again, replaces nothing: the newer
// disk is the one later runs start from (specs/0003-disks.md, Deletion).
func TestALateSaveFromBeforeAClosureReplacesNothing(t *testing.T) {
	e, gh, c := onGitHub(t)
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	pr := e.PullRequest(repo, "main", "fix")
	old, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	if _, d := c.Disk(e.Token(old), "build"); d.State != "ready" {
		t.Fatalf("the old run's disk: %+v", d)
	}
	c.Save(e.Token(old))
	p := e.GitHubPull(pr)
	gh.ClosePull(p)
	// The emulator sees the closure at a disk request.
	probe, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	c.Disk(e.Token(probe), "build")
	gh.ReopenPull(p)
	fresh, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	c.Disk(e.Token(fresh), "build")
	c.Save(e.Token(fresh))
	e.FinishJob(fresh, "success")
	e.FinishJob(old, "success")
	next, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	if _, d := c.Disk(e.Token(next), "build"); d.Restored != service.StepPullRequestExact {
		t.Fatalf("after a late save from before the closure, restored %s; want the reopened pull request's own", d.Restored)
	}
}

// The contribution environment's approval is what binds a contribution.
func TestApprovalIsTheContributionEnvironments(t *testing.T) {
	e, gh, _ := onGitHub(t)
	repo := e.Repository("app")
	pr := e.ForkPullRequest(repo, "main")
	run := e.PullRequestTargetRun(pr)
	gh.Approve(e.GitHubRun(run), "other", gh.User(gh.NewID(), "reviewer"))
	if _, err := e.StartJob(run, conformance.Small); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Binding(run); ok {
		t.Fatal("an approval of another environment bound a contribution")
	}
	run = e.PullRequestTargetRun(pr)
	gh.Approve(e.GitHubRun(run), binding.ContributionEnvironment, gh.User(gh.NewID(), "reviewer"))
	if _, err := e.StartJob(run, conformance.Small); err != nil {
		t.Fatal(err)
	}
	if scope, ok := e.Binding(run); !ok || scope == "read" {
		t.Fatalf("an approved contribution is bound %q, %v", scope, ok)
	}
}
