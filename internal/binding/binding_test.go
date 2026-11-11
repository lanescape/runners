package binding_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/consumer"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/store"
)

const orgID = 303994381

type env struct {
	t           *testing.T
	gh          *ghemu.Emulator
	gcs         *gcsemu.Emulator
	org         *ghemu.Account
	contributor *ghemu.Account
	repo        *ghemu.Repo
	fork        *ghemu.Repo
	client      *github.Client
	binder      *binding.Binder
}

func setup(t *testing.T) *env {
	t.Helper()
	gh := ghemu.New()
	t.Cleanup(gh.Close)
	gcs := gcsemu.New()
	t.Cleanup(gcs.Close)
	gcs.CreateBucket("state")

	org := gh.Org(orgID, "lanescape")
	app := gh.NewApp(77)
	gh.Install(app, org)
	repo := gh.Repo(org, 5001, "app")
	contributor := gh.User(9001, "contributor")
	fork := gh.Fork(repo, contributor, 7001)

	client, err := github.NewAppClient(github.Options{BaseURL: gh.URL, AppID: app.ID, PrivateKey: app.PrivateKeyPEM(), OrganizationID: orgID})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.NewGCS(context.Background(), "state", gcs.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objects.Close() })
	return &env{t: t, gh: gh, gcs: gcs, org: org, contributor: contributor, repo: repo, fork: fork, client: client,
		binder: &binding.Binder{GitHub: client, Objects: objects, OrganizationID: orgID, Clock: clockwork.NewRealClock()}}
}

// facts are what GitHub tells the run's job's runner, as the hook reports
// them, changed by change.
func facts(run *ghemu.Run, change func(*binding.Facts)) binding.Facts {
	f := binding.Facts{Event: run.Event, Ref: run.Ref, RefType: run.RefType, SHA: run.SHA}
	if p := run.EventPull; p != nil {
		f.PullRequest, f.HeadRepositoryID, f.HeadSHA = p.Number, p.HeadRepositoryID, p.HeadSHA
	}
	if change != nil {
		change(&f)
	}
	return f
}

func (e *env) bindCtx(ctx context.Context, run *ghemu.Run, change func(*binding.Facts)) (binding.Binding, error) {
	e.t.Helper()
	repo, err := e.client.RepositoryByID(ctx, run.Repo.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	wr, err := e.client.WorkflowRun(ctx, repo, run.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.binder.Bind(ctx, repo, wr, facts(run, change))
}

func (e *env) bind(run *ghemu.Run, change func(*binding.Facts)) (binding.Binding, error) {
	e.t.Helper()
	return e.bindCtx(context.Background(), run, change)
}

func (e *env) want(run *ghemu.Run, change func(*binding.Facts), want scope.Scope) binding.Binding {
	e.t.Helper()
	got, err := e.bind(run, change)
	if err != nil {
		e.t.Fatalf("bind: %v", err)
	}
	if got.Scope != want {
		e.t.Fatalf("scope = %v (%s), want %v", got.Scope, got.Reason, want)
	}
	stored, ok, err := e.binder.Lookup(context.Background(), run.Repo.ID, run.ID)
	if err != nil || !ok || stored.Scope != want {
		e.t.Fatalf("stored binding = %+v, %v, %v", stored, ok, err)
	}
	return got
}

func (e *env) bound(run *ghemu.Run) bool {
	_, _, ok := e.gcs.Object("state", binding.Key(run.Repo.ID, run.ID))
	return ok
}

// A contributor's pull request from the fork.
func (e *env) contribution() *ghemu.Pull {
	e.fork.Commit("feature", map[string]string{"src.go": "package app"})
	return e.gh.PullRequest(e.repo, "main", e.fork, "feature", e.contributor)
}

// A pull request from a branch of the repository itself.
func (e *env) samePull(branch string) *ghemu.Pull {
	e.repo.Commit(branch, map[string]string{"f": branch})
	return e.gh.PullRequest(e.repo, "main", e.repo, branch, e.org)
}

// approved is a contribution's pull_request_target run, approved.
func (e *env) approved(p *ghemu.Pull) *ghemu.Run {
	run := e.gh.PullRequestTargetRun(p)
	e.gh.Approve(run, binding.ContributionEnvironment, e.org)
	return run
}

func ref(r string) func(*binding.Facts) { return func(f *binding.Facts) { f.Ref = r } }

func TestMain(t *testing.T) {
	t.Run("push to the default branch", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PushRun(e.repo, "main")
		bd := e.want(run, nil, scope.Main)
		if bd.Commit != run.HeadSHA || bd.HeadRepositoryID != e.repo.ID {
			t.Fatalf("binding = %+v", bd)
		}
	})
	t.Run("an older commit of the default branch", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PushRun(e.repo, "main")
		e.repo.Commit("main", map[string]string{"later": "1"})
		e.want(run, nil, scope.Main)
	})
	t.Run("a tag named like the default branch", func(t *testing.T) {
		e := setup(t)
		e.repo.Tag("main", e.repo.Commit("evil", map[string]string{"x": "evil"}))
		e.want(e.gh.TagRun(e.repo, "main"), nil, scope.Read)
	})
	t.Run("a tag named like the default branch, on its commit", func(t *testing.T) {
		e := setup(t)
		e.repo.Tag("main", e.repo.Head("main"))
		e.want(e.gh.TagRun(e.repo, "main"), nil, scope.Read)
	})
	for _, branch := range []string{"feature", "Main", "refs/heads/main", "heads/main", "main-copy"} {
		t.Run("a branch named "+branch, func(t *testing.T) {
			e := setup(t)
			e.repo.Commit(branch, map[string]string{"x": "1"})
			e.want(e.gh.PushRun(e.repo, branch), nil, scope.Read)
		})
	}
	t.Run("a branch at the default branch's commit", func(t *testing.T) {
		e := setup(t)
		e.repo.Branch("copy", e.repo.Head("main"))
		e.want(e.gh.PushRun(e.repo, "copy"), nil, scope.Read)
	})
	t.Run("reported as the default branch, with a commit that isn't on it", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("feature", map[string]string{"x": "1"})
		e.want(e.gh.PushRun(e.repo, "feature"), ref("refs/heads/main"), scope.Read)
	})
	t.Run("ref type tag", func(t *testing.T) {
		e := setup(t)
		e.want(e.gh.PushRun(e.repo, "main"), func(f *binding.Facts) { f.RefType = "tag" }, scope.Read)
	})
	t.Run("the runner's commit isn't the run's", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PushRun(e.repo, "main")
		other := e.repo.Commit("main", map[string]string{"x": "2"})
		e.want(run, func(f *binding.Facts) { f.SHA = other }, scope.Read)
	})
	t.Run("the runner's event isn't the run's", func(t *testing.T) {
		e := setup(t)
		run := e.gh.EventRun(e.repo, "workflow_dispatch", "main")
		e.want(run, func(f *binding.Facts) { f.Event = "push" }, scope.Read)
	})
	for _, event := range []string{"workflow_dispatch", "schedule", "merge_group", "release"} {
		t.Run(event+" on the default branch", func(t *testing.T) {
			e := setup(t)
			e.want(e.gh.EventRun(e.repo, event, "main"), nil, scope.Read)
		})
	}
	t.Run("a pull request named in a push", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		e.want(e.gh.PushRun(e.repo, "feature"), func(f *binding.Facts) { f.PullRequest = p.Number }, scope.Read)
	})
}

func TestLongLivedBranches(t *testing.T) {
	list := func(e *env, branches string) {
		e.repo.Commit("main", map[string]string{consumer.RunnersFile: `{"branches": ` + branches + `}`})
	}
	t.Run("a push to a listed branch", func(t *testing.T) {
		e := setup(t)
		list(e, `{"support/v1": "v1"}`)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		e.want(e.gh.PushRun(e.repo, "support/v1"), nil, scope.Branch("v1"))
	})
	t.Run("a push to a branch that isn't listed", func(t *testing.T) {
		e := setup(t)
		list(e, `{"support/v1": "v1"}`)
		e.repo.Commit("support/v2", map[string]string{"x": "1"})
		e.want(e.gh.PushRun(e.repo, "support/v2"), nil, scope.Read)
	})
	t.Run("listed only on the pushed branch's own copy", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("support/v1", map[string]string{consumer.RunnersFile: `{"branches": {"support/v1": "v1"}}`})
		e.want(e.gh.PushRun(e.repo, "support/v1"), nil, scope.Read)
	})
	t.Run("a tag named like a listed branch", func(t *testing.T) {
		e := setup(t)
		list(e, `{"v1": "v1"}`)
		e.repo.Tag("v1", e.repo.Head("main"))
		e.want(e.gh.TagRun(e.repo, "v1"), nil, scope.Read)
	})
	t.Run("an invalid file lists nothing", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("main", map[string]string{consumer.RunnersFile: `{"branches": {"support/v1": "v1"}, "other": 1}`})
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		e.want(e.gh.PushRun(e.repo, "support/v1"), nil, scope.Read)
	})
	t.Run("a pull request into a listed branch has it as its base", func(t *testing.T) {
		e := setup(t)
		list(e, `{"support/v1": "v1"}`)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		e.repo.Commit("fix", map[string]string{"y": "1"})
		p := e.gh.PullRequest(e.repo, "support/v1", e.repo, "fix", e.org)
		bd := e.want(e.gh.PullRequestRun(p), nil, scope.PullRequest(p.Number))
		if bd.Base != scope.Branch("v1") {
			t.Fatalf("base = %v", bd.Base)
		}
	})
	t.Run("a pull request into a branch that isn't listed falls back to main", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("other", map[string]string{"x": "1"})
		e.repo.Commit("fix", map[string]string{"y": "1"})
		p := e.gh.PullRequest(e.repo, "other", e.repo, "fix", e.org)
		bd := e.want(e.gh.PullRequestRun(p), nil, scope.PullRequest(p.Number))
		if bd.Base != scope.Main {
			t.Fatalf("base = %v", bd.Base)
		}
	})
}

func TestPullRequest(t *testing.T) {
	t.Run("its own pull request", func(t *testing.T) {
		e := setup(t)
		e.samePull("first")
		p := e.samePull("feature")
		run := e.gh.PullRequestRun(p)
		bd := e.want(run, nil, scope.PullRequest(p.Number))
		if bd.Commit != p.HeadSHA || bd.Base != scope.Main {
			t.Fatalf("binding = %+v", bd)
		}
	})
	t.Run("an event naming another pull request", func(t *testing.T) {
		e := setup(t)
		other := e.samePull("other")
		p := e.samePull("feature")
		e.want(e.gh.PullRequestRun(p), func(f *binding.Facts) { f.PullRequest = other.Number }, scope.PullRequest(p.Number))
	})
	t.Run("a ref naming another pull request", func(t *testing.T) {
		e := setup(t)
		other := e.samePull("other")
		p := e.samePull("feature")
		e.want(e.gh.PullRequestRun(p), ref(fmt.Sprintf("refs/pull/%d/merge", other.Number)), scope.Read)
	})
	for _, r := range []string{"refs/pull/1/head", "refs/pull/01/merge", "refs/pull/x/merge", "refs/heads/main", "refs/pull/1/merge/"} {
		t.Run("ref "+r, func(t *testing.T) {
			e := setup(t)
			p := e.samePull("feature")
			e.want(e.gh.PullRequestRun(p), ref(r), scope.Read)
		})
	}
	t.Run("a fork's pull request", func(t *testing.T) {
		e := setup(t)
		e.want(e.gh.PullRequestRun(e.contribution()), nil, scope.Read)
	})
	t.Run("an earlier commit of the pull request", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		run := e.gh.PullRequestRun(p)
		e.repo.Commit("feature", map[string]string{"g": "2"})
		e.gh.Sync(p)
		e.want(run, nil, scope.PullRequest(p.Number))
	})
	t.Run("the head of two open pull requests fails the run's jobs", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		e.repo.Branch("feature-copy", e.repo.Head("feature"))
		e.gh.PullRequest(e.repo, "main", e.repo, "feature-copy", e.org)
		bd := e.want(e.gh.PullRequestRun(p), nil, scope.Read)
		if !strings.Contains(bd.Fail, "one open pull request") {
			t.Fatalf("fail = %q", bd.Fail)
		}
	})
	t.Run("the head of one open and one closed pull request", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		e.repo.Branch("feature-copy", e.repo.Head("feature"))
		e.gh.ClosePull(e.gh.PullRequest(e.repo, "main", e.repo, "feature-copy", e.org))
		e.want(e.gh.PullRequestRun(p), nil, scope.PullRequest(p.Number))
	})
}

func TestPullRequestTarget(t *testing.T) {
	t.Run("an approved contribution binds its event's pull request", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.approved(p)
		bd := e.want(run, nil, scope.PullRequest(p.Number))
		if bd.HeadRepositoryID != e.fork.ID || bd.Commit != p.HeadSHA || bd.Fail != "" {
			t.Fatalf("binding = %+v", bd)
		}
	})
	t.Run("one that isn't approved stays unbound", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PullRequestTargetRun(e.contribution())
		if _, err := e.bind(run, nil); !errors.Is(err, binding.ErrUnapproved) {
			t.Fatalf("error = %v, want ErrUnapproved", err)
		}
		if e.bound(run) {
			t.Fatal("bound before approval")
		}
		e.gh.Approve(run, "other-environment", e.org)
		if _, err := e.bind(run, nil); !errors.Is(err, binding.ErrUnapproved) {
			t.Fatalf("another environment's approval: %v", err)
		}
	})
	for name, change := range map[string]func(*binding.Facts){
		"another pull request":     func(f *binding.Facts) { f.PullRequest = 99 },
		"another head repository":  func(f *binding.Facts) { f.HeadRepositoryID = 5001 },
		"a commit not of the pull": func(f *binding.Facts) { f.HeadSHA = strings.Repeat("a", 40) },
		"no pull request":          func(f *binding.Facts) { f.PullRequest = 0 },
		"a malformed commit":       func(f *binding.Facts) { f.HeadSHA = "HEAD" },
	} {
		t.Run("an event naming "+name+" fails the run's jobs", func(t *testing.T) {
			e := setup(t)
			bd := e.want(e.approved(e.contribution()), change, scope.Read)
			if bd.Fail == "" {
				t.Fatal("the run's jobs aren't failed")
			}
		})
	}
	t.Run("a pull request closed while its run waited fails the run's jobs", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.approved(p)
		e.gh.ClosePull(p)
		if bd := e.want(run, nil, scope.Read); bd.Fail == "" {
			t.Fatal("not failed")
		}
	})
	t.Run("a contribution pushed to after its approval binds its own commit", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.approved(p)
		first := p.HeadSHA
		e.fork.Commit("feature", map[string]string{"more": "1"})
		e.gh.Sync(p)
		bd := e.want(run, nil, scope.PullRequest(p.Number))
		if bd.Commit != first {
			t.Fatalf("commit = %s, want the approved run's %s", bd.Commit, first)
		}
	})
	t.Run("a same-repository pull request", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		e.want(e.approved(p), nil, scope.PullRequest(p.Number))
	})
}

func TestABindingIsMadeOnce(t *testing.T) {
	e := setup(t)
	main := e.gh.PushRun(e.repo, "main")
	e.want(main, nil, scope.Main)
	// A later job can't change it, whatever its runner says.
	e.want(main, ref("refs/heads/other"), scope.Main)
}

func TestConcurrentFirstJobsAgree(t *testing.T) {
	e := setup(t)
	p := e.samePull("feature")
	run := e.gh.PullRequestRun(p)
	var wg sync.WaitGroup
	results := make([]binding.Binding, 6)
	errs := make([]error, len(results))
	for i := range results {
		wg.Go(func() { results[i], errs[i] = e.bind(run, nil) })
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Scope != scope.PullRequest(p.Number) || !results[i].BoundAt.Equal(results[0].BoundAt) {
			t.Fatalf("job %d: %+v, %v; want the same binding as job 0, %+v", i, results[i], errs[i], results[0])
		}
	}
}

// Every GitHub call on each rule's path can fail transiently; none may leave
// a binding, and a later call binds as if nothing had happened.
func TestTransientFailuresLeaveNoBinding(t *testing.T) {
	type path struct {
		name  string
		run   func(*env) *ghemu.Run
		calls []string
	}
	paths := []path{
		{name: "main", run: func(e *env) *ghemu.Run { return e.gh.PushRun(e.repo, "main") },
			calls: []string{"/repos/lanescape/app/git/ref/", "/repos/lanescape/app/compare/"}},
		{name: "pull_request", run: func(e *env) *ghemu.Run { return e.gh.PullRequestRun(e.samePull("feature")) },
			calls: []string{"/repos/lanescape/app/pulls/1", "/repos/lanescape/app/pulls"}},
		{name: "pull_request_target", run: func(e *env) *ghemu.Run {
			p := e.contribution()
			run := e.approved(p)
			e.fork.Commit("feature", map[string]string{"more": "1"})
			e.gh.Sync(p)
			return run
		}, calls: []string{"/repos/lanescape/app/actions/runs/", "/repos/lanescape/app/pulls/1/commits", "/repos/lanescape/app/pulls"}},
	}
	failures := map[string]func(e *env, prefix string){
		"rate limit":  func(e *env, p string) { e.gh.RateLimit(p, 1, time.Second) },
		"bad gateway": func(e *env, p string) { e.gh.ServerError(p, http.StatusBadGateway, 1) },
		"dropped": func(e *env, p string) {
			r := fault.Rule{PathPrefix: p, Times: 2, Respond: fault.Drop}
			if !strings.HasSuffix(p, "/") {
				r.Match = func(req *http.Request) bool { return req.URL.Path == p }
			}
			e.gh.Faults().Add(r)
		},
	}
	for _, pt := range paths {
		for _, call := range pt.calls {
			for fname, fail := range failures {
				t.Run(pt.name+" "+call+" "+fname, func(t *testing.T) {
					e := setup(t)
					run := pt.run(e)
					ctx := context.Background()
					repo, _ := e.client.RepositoryByID(ctx, run.Repo.ID)
					wr, _ := e.client.WorkflowRun(ctx, repo, run.ID)
					fail(e, call)
					if _, err := e.binder.Bind(ctx, repo, wr, facts(run, nil)); !errors.Is(err, binding.ErrRetry) {
						t.Fatalf("error = %v, want ErrRetry", err)
					}
					if e.bound(run) {
						t.Fatal("a binding was made from an unconfirmed answer")
					}
				})
			}
		}
	}
	t.Run("a slow GitHub", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PushRun(e.repo, "main")
		ctx := context.Background()
		repo, _ := e.client.RepositoryByID(ctx, run.Repo.ID)
		wr, _ := e.client.WorkflowRun(ctx, repo, run.ID)
		e.gh.Faults().Add(fault.Rule{PathPrefix: "/repos/lanescape/app/compare/", Delay: time.Minute})
		short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		if _, err := e.binder.Bind(short, repo, wr, facts(run, nil)); !errors.Is(err, binding.ErrRetry) {
			t.Fatalf("error = %v, want ErrRetry", err)
		}
		if e.bound(run) {
			t.Fatal("bound after a timeout")
		}
	})
}

func TestStorageFailures(t *testing.T) {
	t.Run("lost response", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PushRun(e.repo, "main")
		e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1, After: true,
			Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
		e.want(run, nil, scope.Main)
	})
}

func TestAnotherOwnersRunIsNeverBound(t *testing.T) {
	e := setup(t)
	other := e.gh.Org(1234, "lanescape-lookalike")
	theirs := e.gh.Repo(other, 6001, "app")
	run := e.gh.PushRun(theirs, "main")
	ctx := context.Background()
	repo := github.Repository{ID: 6001, OwnerID: 1234, Owner: "lanescape-lookalike", Name: "app", DefaultBranch: "main"}
	if _, err := e.binder.Bind(ctx, repo, github.WorkflowRun{ID: run.ID, RepositoryID: 6001, Event: "push"}, facts(run, nil)); err == nil {
		t.Fatal("bound another owner's run")
	}
}

func TestAStackedPullRequestCantClaimTheOtherPullRequest(t *testing.T) {
	e := setup(t)
	a := e.samePull("a")
	// B is stacked on A: A's head is among B's commits.
	e.repo.Branch("b", e.repo.Head("a"))
	e.repo.Commit("b", map[string]string{"b": "1"})
	b := e.gh.PullRequest(e.repo, "a", e.repo, "b", e.org)
	e.want(e.gh.PullRequestRun(a), ref(fmt.Sprintf("refs/pull/%d/merge", b.Number)), scope.Read)
}
