package service_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

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

// saveCache saves a job's cache as the save-cache action does: it asks to
// save, uploads an object under the version's prefix, and commits it.
func saveCache(t *testing.T, e *service.Emulator, c *conformance.Client, j service.Job, cache string) {
	t.Helper()
	a, s := c.Save(e.Token(j), cache)
	if a.Status != http.StatusOK || !s.Saves {
		t.Fatalf("job %d's save of %s: %d %q %+v", j.ID, cache, a.Status, a.Reason, s)
	}
	e.CacheStorage().Put(s.Bucket, s.Prefix+"shard-000.tar.zst", []byte(cache))
	if a, _ := c.Commit(e.Token(j), cache); a.Status != http.StatusOK {
		t.Fatalf("job %d's commit of %s: %d %q", j.ID, cache, a.Status, a.Reason)
	}
}

// restored restores a job's cache and returns where it came from.
func restored(t *testing.T, e *service.Emulator, c *conformance.Client, j service.Job, cache string) string {
	t.Helper()
	a, r := c.Restore(e.Token(j), cache)
	if a.Status != http.StatusOK {
		t.Fatalf("job %d's restore of %s: %d %q", j.ID, cache, a.Status, a.Reason)
	}
	return r.Restored
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
	if a, _ := c.Disk(e.Token(j)); a.Status != http.StatusOK {
		t.Fatalf("a work disk request with the GitHub emulator's API failing: %d", a.Status)
	}
	if a, _ := c.Restore(e.Token(j), "build"); a.Status != http.StatusOK {
		t.Fatalf("a restore with the GitHub emulator's API failing: %d", a.Status)
	}
	e.Unavailable(true)
	if a, _ := c.Disk(e.Token(j)); a.Status != http.StatusServiceUnavailable {
		t.Fatalf("a work disk request with GitHub unavailable: %d", a.Status)
	}
	if a, _ := c.Restore(e.Token(j), "build"); a.Status != http.StatusServiceUnavailable {
		t.Fatalf("a restore with GitHub unavailable: %d", a.Status)
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
	if a, d := c.Disk(tok); a.Status != http.StatusOK || d.Restored != service.StepNone {
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

// A test reads what was attached and what was saved, which a restore may
// choose only once its job has succeeded.
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
	c.Disk(e.Token(j))
	d, ok := e.WorkDisk(j)
	if !ok || d.Scope != "main" || d.Architecture != "amd64" || d.Restored != service.StepNone || !d.Ready {
		t.Fatalf("the work disk: %+v", d)
	}
	saveCache(t, e, c, j, "build")
	if saved := e.Caches(repo); len(saved) != 0 {
		t.Fatalf("caches before the saving job finished: %+v", saved)
	}
	e.FinishJob(j, "success")
	saved := e.Caches(repo)
	if len(saved) != 1 || saved[0].Cache != "build" || saved[0].Key == "" || saved[0].Scope != "main" || saved[0].Architecture != "amd64" ||
		saved[0].Job != j.ID || saved[0].Objects != 1 {
		t.Fatalf("the caches: %+v", saved)
	}
}

// A pull request keeps one version of each cache, and a closed one's are
// never chosen again, even once it's reopened.
func TestPullRequestCaches(t *testing.T) {
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
		if got := restored(t, e, c, j, "build"); got != want {
			t.Fatalf("restored %s, want %s", got, want)
		}
		saveCache(t, e, c, j, "build")
		e.FinishJob(j, "success")
	}
	saveOne(service.StepNone)
	pr = e.PushPullRequest(pr, map[string]string{"deps.txt": "2\n"})
	saveOne(service.StepPullRequestNewest)
	if saved := e.Caches(repo); len(saved) != 1 {
		t.Fatalf("a pull request's versions of one cache: %+v", saved)
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

// Main keeps one version of each cache too: a save under a new key replaces
// its version of the old key, so a pull request still of the old key starts
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
		if got := restored(t, e, c, j, "build"); got != want {
			t.Fatalf("restored %s, want %s", got, want)
		}
		saveCache(t, e, c, j, "build")
		e.FinishJob(j, "success")
	}
	saveMain(service.StepNone)
	e.Commit(repo, "main", map[string]string{"deps.txt": "2\n"})
	saveMain(service.StepMainNewest)
	saved := e.Caches(repo)
	if len(saved) != 1 || saved[0].Scope != "main" {
		t.Fatalf("main's versions of one cache after a new key's save: %+v", saved)
	}
	j, err := e.StartJob(e.PullRequestRun(e.PullRequest(repo, "main", "old")), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored(t, e, c, j, "build"); got != service.StepMainNewest {
		t.Fatalf("a pull request of the old key restored %s, want %s", got, service.StepMainNewest)
	}
}

// A save from a run bound before its pull request was closed, finishing
// after the reopened pull request saved again, replaces nothing: the newer
// version is the one later runs start from (specs/0003-disks.md, Deletion).
func TestALateSaveFromBeforeAClosureReplacesNothing(t *testing.T) {
	e, gh, c := onGitHub(t)
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	pr := e.PullRequest(repo, "main", "fix")
	old, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored(t, e, c, old, "build"); got != service.StepNone {
		t.Fatalf("the old run restored %s", got)
	}
	saveCache(t, e, c, old, "build")
	p := e.GitHubPull(pr)
	gh.ClosePull(p)
	// The emulator sees the closure at a restore request.
	probe, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	restored(t, e, c, probe, "build")
	gh.ReopenPull(p)
	fresh, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	restored(t, e, c, fresh, "build")
	saveCache(t, e, c, fresh, "build")
	e.FinishJob(fresh, "success")
	e.FinishJob(old, "success")
	next, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored(t, e, c, next, "build"); got != service.StepPullRequestExact {
		t.Fatalf("after a late save from before the closure, restored %s; want the reopened pull request's own", got)
	}
	if saved := e.Caches(repo); len(saved) != 1 || saved[0].Job != fresh.ID {
		t.Fatalf("after a late save from before the closure, the caches: %+v; want the reopened pull request's own", saved)
	}
}

// While a pull request is closed, a check answers what a restore then would,
// which retires what the pull request's runs saved before; and a check never
// records the closure, so a pull request reopened after only checks still
// finds its own version, as one reopened before any restore does.
func TestACheckSeesAClosureAsARestoreWould(t *testing.T) {
	for name, clock := range map[string]clockwork.Clock{"at the zero time": clockwork.NewFakeClockAt(time.Time{}),
		"the real time": clockwork.NewRealClock()} {
		t.Run(name, func(t *testing.T) {
			gh := ghemu.New()
			t.Cleanup(gh.Close)
			e := service.New(service.WithGitHub(gh), service.WithClock(clock))
			t.Cleanup(e.Close)
			c := &conformance.Client{T: t, URL: e.URL()}
			repo := e.Repository("app")
			e.Commit(repo, "main", conformance.Files)
			pr := e.PullRequest(repo, "main", "fix")
			j, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
			if err != nil {
				t.Fatal(err)
			}
			restored(t, e, c, j, "build")
			saveCache(t, e, c, j, "build")
			e.FinishJob(j, "success")
			p := e.GitHubPull(pr)
			gh.ClosePull(p)
			run := e.PullRequestRun(pr)
			checker, err := e.StartJob(run, conformance.Small)
			if err != nil {
				t.Fatal(err)
			}
			if a, found := c.Check(e.Token(checker), "build"); a.Status != http.StatusOK || found != service.StepNone {
				t.Fatalf("a check while closed: %d %s; want none, as a restore would", a.Status, found)
			}
			gh.ReopenPull(p)
			next, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
			if err != nil {
				t.Fatal(err)
			}
			if got := restored(t, e, c, next, "build"); got != service.StepPullRequestExact {
				t.Fatalf("reopened after only a check, restored %s; want its own version, the closure unrecorded", got)
			}
			e.FinishJob(checker, "success")
			e.FinishJob(next, "success")
		})
	}
	// A restore while closed records the closure, the first one included at
	// the zero time, so the reopened pull request starts again from its base.
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	e := service.New(service.WithGitHub(gh), service.WithClock(clockwork.NewFakeClockAt(time.Time{})))
	t.Cleanup(e.Close)
	c := &conformance.Client{T: t, URL: e.URL()}
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	pr := e.PullRequest(repo, "main", "fix")
	j, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	restored(t, e, c, j, "build")
	saveCache(t, e, c, j, "build")
	e.FinishJob(j, "success")
	p := e.GitHubPull(pr)
	gh.ClosePull(p)
	probe, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	restored(t, e, c, probe, "build")
	gh.ReopenPull(p)
	next, err := e.StartJob(e.PullRequestRun(pr), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored(t, e, c, next, "build"); got != service.StepNone {
		t.Fatalf("reopened after a restore while closed, at the zero time, restored %s; want none", got)
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

// A version whose job finishes while its repository isn't the
// organization's waits, as the service decides nothing of such a
// repository, and is settled once GitHub shows it again.
func TestAVersionWaitsOutATransfer(t *testing.T) {
	e, gh, c := onGitHub(t)
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	j, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	saveCache(t, e, c, j, "build")
	rp := e.GitHubRepo(repo)
	org := rp.Owner
	gh.Transfer(rp, gh.Org(4242, "elsewhere"))
	e.FinishJob(j, "success")
	if saved := e.Caches(repo); len(saved) != 0 {
		t.Fatalf("caches while the repository isn't the organization's: %+v", saved)
	}
	gh.Transfer(rp, org)
	if saved := e.Caches(repo); len(saved) != 1 || saved[0].Job != j.ID {
		t.Fatalf("caches once the repository is back: %+v; want the version, settled", saved)
	}
}

// A job a test completes in GitHub directly has its versions settled by
// GitHub's conclusion, as the service settles them from GitHub's record.
func TestAVersionOfAJobCompletedInGitHub(t *testing.T) {
	e, gh, c := onGitHub(t)
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	j, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	saveCache(t, e, c, j, "build")
	gh.CompleteJob(e.GitHubJob(j), "success")
	if saved := e.Caches(repo); len(saved) != 1 || saved[0].Job != j.ID {
		t.Fatalf("caches = %+v; want the version, settled", saved)
	}
}

// A version whose objects can't be listed as its job finishes waits, and is
// settled once they can be, as the service's pass defers.
func TestAVersionWaitsOutAListingThatFails(t *testing.T) {
	e, _, c := onGitHub(t)
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	j, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	saveCache(t, e, c, j, "build")
	e.CacheStorage().Faults().Add(fault.Rule{Method: http.MethodGet, Respond: fault.Status(http.StatusForbidden, nil, `{"error": {"code": 403}}`)})
	e.FinishJob(j, "success")
	e.CacheStorage().Faults().Reset()
	if saved := e.Caches(repo); len(saved) != 1 || saved[0].Job != j.ID {
		t.Fatalf("caches = %+v; want the version, settled once its objects could be listed", saved)
	}
	next, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored(t, e, c, next, "build"); got != service.StepMainExact {
		t.Fatalf("restored from %s", got)
	}
}

// Versions saved at the same time are ordered by name, as the service
// orders them, whichever was saved first: the higher-named is the newer,
// and a restore picks it even while another run holds the other.
func TestVersionsSavedAtOnceAreOrderedByName(t *testing.T) {
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	e := service.New(service.WithGitHub(gh), service.WithClock(clockwork.NewFakeClock()))
	t.Cleanup(e.Close)
	c := &conformance.Client{T: t, URL: e.URL()}
	repo := e.Repository("app")
	e.Commit(repo, "main", conformance.Files)
	start := func() service.Job {
		t.Helper()
		j, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	save := func(j service.Job) string {
		t.Helper()
		a, s := c.Save(e.Token(j), "build")
		if a.Status != http.StatusOK || !s.Saves {
			t.Fatalf("a save: %d %q", a.Status, a.Reason)
		}
		e.CacheStorage().Put(s.Bucket, s.Prefix+"shard-000.tar.zst", []byte(s.Prefix))
		if a, _ := c.Commit(e.Token(j), "build"); a.Status != http.StatusOK {
			t.Fatalf("a commit: %d %q", a.Status, a.Reason)
		}
		return s.Prefix
	}
	restore := func(j service.Job) string {
		t.Helper()
		a, r := c.Restore(e.Token(j), "build")
		if a.Status != http.StatusOK || len(r.Objects) != 1 {
			t.Fatalf("a restore: %d %q %+v", a.Status, a.Reason, r)
		}
		return r.Objects[0].Name
	}
	// The emulator names machines in the order it makes them, so the job
	// started later saves first, and its version is the higher-named.
	lo := start()
	hi := start()
	hiPrefix := save(hi)
	if loPrefix := save(lo); hiPrefix < loPrefix {
		t.Fatalf("%s saved first isn't the higher-named, beside %s", hiPrefix, loPrefix)
	}
	e.FinishJob(lo, "success")
	holder := start()
	restore(holder)
	e.FinishJob(hi, "success")
	if got := restore(start()); !strings.HasPrefix(got, hiPrefix) {
		t.Fatalf("restored %s; want the higher-named version, under %s", got, hiPrefix)
	}
}

// A failed job's version whose objects can't be deleted, refused or never
// answered, is kept until they can be, and then they go.
func TestAFailedVersionsObjectsGoOnceTheyCan(t *testing.T) {
	for name, respond := range map[string]http.HandlerFunc{
		"refused":       fault.Status(http.StatusForbidden, nil, `{"error": {"code": 403}}`),
		"not available": fault.Status(http.StatusServiceUnavailable, nil, `{"error": {"code": 503}}`),
	} {
		t.Run(name, func(t *testing.T) {
			e, _, c := onGitHub(t)
			repo := e.Repository("app")
			e.Commit(repo, "main", conformance.Files)
			j, err := e.StartJob(e.Push(repo, "main"), conformance.Small)
			if err != nil {
				t.Fatal(err)
			}
			a, s := c.Save(e.Token(j), "build")
			if a.Status != http.StatusOK {
				t.Fatalf("a save: %d", a.Status)
			}
			obj := s.Prefix + "shard-000.tar.zst"
			e.CacheStorage().Put(s.Bucket, obj, []byte("x"))
			e.CacheStorage().Faults().Add(fault.Rule{Method: http.MethodDelete, Respond: respond})
			e.FinishJob(j, "failure")
			e.CacheStorage().Faults().Reset()
			if _, _, ok := e.CacheStorage().Object(s.Bucket, obj); !ok {
				t.Fatal("deleted through the fault")
			}
			e.Caches(repo)
			if _, _, ok := e.CacheStorage().Object(s.Bucket, obj); ok {
				t.Fatal("a failed job's object was kept once it could be deleted")
			}
		})
	}
}
