package binding_test

import (
	"context"
	"encoding/json"
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
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/store"
)

const (
	orgID     = 303994381
	audience  = "https://runners.example.test"
	bootstrap = "lanescape/app/.github/workflows/bootstrap.yml@refs/heads/main"
)

type env struct {
	t           *testing.T
	gh          *ghemu.Emulator
	gcs         *gcsemu.Emulator
	org         *ghemu.Account
	contributor *ghemu.Account
	repo        *ghemu.Repo
	fork        *ghemu.Repo
	verifier    *auth.Actions
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
	repo.Commit("main", map[string]string{binding.RunnersFile: `{
  "lanescape-80vcpu-linux-amd64": "ubuntu-24.04",
  "bootstrap": "` + bootstrap + `"
}`})
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
	v, err := auth.NewActions(auth.Options{Issuer: auth.Issuer{URL: gh.Actions.URL, JWKSURL: gh.Actions.JWKSURL}, Audience: audience}, orgID)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, gh: gh, gcs: gcs, org: org, contributor: contributor, repo: repo, fork: fork, verifier: v,
		binder: &binding.Binder{GitHub: client, Objects: objects, OrganizationID: orgID, Clock: clockwork.NewRealClock()}}
}

// bind sends the run's job's token, as the runners action does.
func (e *env) bind(run *ghemu.Run, job ghemu.Job, req binding.Request, overrides map[string]any) (binding.Binding, error) {
	e.t.Helper()
	return e.bindCtx(context.Background(), run, job, req, overrides)
}

func (e *env) bindCtx(ctx context.Context, run *ghemu.Run, job ghemu.Job, req binding.Request, overrides map[string]any) (binding.Binding, error) {
	e.t.Helper()
	job.Audience = audience
	c, err := e.verifier.Verify(ctx, e.gh.ActionsToken(run, job, overrides))
	if err != nil {
		e.t.Fatalf("token didn't verify: %v", err)
	}
	return e.binder.Bind(ctx, c, req)
}

func (e *env) want(run *ghemu.Run, job ghemu.Job, req binding.Request, overrides map[string]any, want scope.Scope) {
	e.t.Helper()
	got, err := e.bind(run, job, req, overrides)
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
}

func (e *env) bound(run *ghemu.Run) bool {
	_, _, ok := e.gcs.Object("state", binding.Key(run.Repo.ID, run.ID))
	return ok
}

var noJob = ghemu.Job{}

func boot() ghemu.Job { return ghemu.Job{WorkflowRef: bootstrap} }

// event is what the bootstrap workflow sends: what its run's event says of
// the pull request.
func event(run *ghemu.Run) binding.Request {
	return binding.Request{PullRequest: run.EventPull.Number, HeadRepositoryID: run.EventPull.HeadRepositoryID, HeadSHA: run.EventPull.HeadSHA}
}

// naming is the run's event's request, naming another pull request.
func naming(run *ghemu.Run, n int) binding.Request {
	r := event(run)
	r.PullRequest = n
	return r
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

func TestMain(t *testing.T) {
	t.Run("push to the default branch", func(t *testing.T) {
		e := setup(t)
		e.want(e.gh.PushRun(e.repo, "main"), noJob, binding.Request{}, nil, scope.Main)
	})
	t.Run("an older commit of the default branch", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PushRun(e.repo, "main")
		e.repo.Commit("main", map[string]string{"later": "1"})
		e.want(run, noJob, binding.Request{}, nil, scope.Main)
	})
	t.Run("a tag named like the default branch", func(t *testing.T) {
		e := setup(t)
		e.repo.Tag("main", e.repo.Commit("evil", map[string]string{"x": "evil"}))
		e.want(e.gh.TagRun(e.repo, "main"), noJob, binding.Request{}, nil, scope.Read)
	})
	t.Run("a tag named like the default branch, on its commit", func(t *testing.T) {
		e := setup(t)
		e.repo.Tag("main", e.repo.Head("main"))
		e.want(e.gh.TagRun(e.repo, "main"), noJob, binding.Request{}, nil, scope.Read)
	})
	for _, branch := range []string{"feature", "Main", "refs/heads/main", "heads/main", "main-copy"} {
		t.Run("a branch named "+branch, func(t *testing.T) {
			e := setup(t)
			e.repo.Commit(branch, map[string]string{"x": "1"})
			e.want(e.gh.PushRun(e.repo, branch), noJob, binding.Request{}, nil, scope.Read)
		})
	}
	t.Run("a branch at the default branch's commit", func(t *testing.T) {
		e := setup(t)
		e.repo.Branch("copy", e.repo.Head("main"))
		e.want(e.gh.PushRun(e.repo, "copy"), noJob, binding.Request{}, nil, scope.Read)
	})
	t.Run("signed as the default branch, with a commit that isn't on it", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("feature", map[string]string{"x": "1"})
		run := e.gh.PushRun(e.repo, "feature")
		e.want(run, noJob, binding.Request{}, map[string]any{"ref": "refs/heads/main"}, scope.Read)
	})
	t.Run("ref type tag", func(t *testing.T) {
		e := setup(t)
		e.want(e.gh.PushRun(e.repo, "main"), noJob, binding.Request{}, map[string]any{"ref_type": "tag"}, scope.Read)
	})
	t.Run("the token's commit isn't the run's", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PushRun(e.repo, "main")
		other := e.repo.Commit("main", map[string]string{"x": "2"})
		e.want(run, noJob, binding.Request{}, map[string]any{"sha": other}, scope.Read)
	})
	t.Run("the token's event isn't the run's", func(t *testing.T) {
		e := setup(t)
		run := e.gh.EventRun(e.repo, "workflow_dispatch", "main")
		e.want(run, noJob, binding.Request{}, map[string]any{"event_name": "push"}, scope.Read)
	})
	for _, event := range []string{"workflow_dispatch", "schedule", "merge_group", "release"} {
		t.Run(event+" on the default branch", func(t *testing.T) {
			e := setup(t)
			e.want(e.gh.EventRun(e.repo, event, "main"), noJob, binding.Request{}, nil, scope.Read)
		})
	}
	t.Run("the bootstrap's word in a push", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		run := e.gh.PushRun(e.repo, "feature")
		e.want(run, boot(), binding.Request{PullRequest: p.Number}, nil, scope.Read)
	})
}

func TestPullRequest(t *testing.T) {
	t.Run("its own pull request", func(t *testing.T) {
		e := setup(t)
		e.samePull("first")
		p := e.samePull("feature")
		e.want(e.gh.PullRequestRun(p), noJob, binding.Request{}, nil, scope.PullRequest(p.Number))
	})
	t.Run("claiming another pull request", func(t *testing.T) {
		e := setup(t)
		other := e.samePull("other")
		p := e.samePull("feature")
		e.want(e.gh.PullRequestRun(p), noJob, binding.Request{PullRequest: other.Number}, nil, scope.PullRequest(p.Number))
	})
	t.Run("claiming another pull request as the bootstrap", func(t *testing.T) {
		e := setup(t)
		other := e.samePull("other")
		p := e.samePull("feature")
		e.want(e.gh.PullRequestRun(p), boot(), binding.Request{PullRequest: other.Number}, nil, scope.PullRequest(p.Number))
	})
	t.Run("signed for another pull request", func(t *testing.T) {
		e := setup(t)
		other := e.samePull("other")
		p := e.samePull("feature")
		ref := fmt.Sprintf("refs/pull/%d/merge", other.Number)
		e.want(e.gh.PullRequestRun(p), noJob, binding.Request{}, map[string]any{"ref": ref}, scope.Read)
	})
	for _, ref := range []string{"refs/pull/1/head", "refs/pull/01/merge", "refs/pull/x/merge", "refs/heads/main", "refs/pull/1/merge/"} {
		t.Run("ref "+ref, func(t *testing.T) {
			e := setup(t)
			p := e.samePull("feature")
			e.want(e.gh.PullRequestRun(p), noJob, binding.Request{}, map[string]any{"ref": ref}, scope.Read)
		})
	}
	t.Run("a pull request that doesn't exist", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		e.want(e.gh.PullRequestRun(p), noJob, binding.Request{}, map[string]any{"ref": "refs/pull/99/merge"}, scope.Read)
	})
	t.Run("a fork's pull request", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		e.want(e.gh.PullRequestRun(p), noJob, binding.Request{}, nil, scope.Read)
	})
	t.Run("the pull request has moved on", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		run := e.gh.PullRequestRun(p)
		e.repo.Commit("feature", map[string]string{"g": "2"})
		e.gh.Sync(p)
		e.want(run, noJob, binding.Request{}, nil, scope.Read)
	})
	t.Run("the head of two open pull requests", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		e.repo.Branch("feature-copy", e.repo.Head("feature"))
		e.gh.PullRequest(e.repo, "main", e.repo, "feature-copy", e.org)
		e.want(e.gh.PullRequestRun(p), noJob, binding.Request{}, nil, scope.Read)
	})
	t.Run("the head of one open and one closed pull request", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		e.repo.Branch("feature-copy", e.repo.Head("feature"))
		e.gh.ClosePull(e.gh.PullRequest(e.repo, "main", e.repo, "feature-copy", e.org))
		e.want(e.gh.PullRequestRun(p), noJob, binding.Request{}, nil, scope.PullRequest(p.Number))
	})
}

func TestPullRequestTarget(t *testing.T) {
	t.Run("the bootstrap names its pull request", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, boot(), event(run), nil, scope.PullRequest(p.Number))
		bd, _, _ := e.binder.Lookup(context.Background(), run.Repo.ID, run.ID)
		if bd.HeadRepositoryID != e.fork.ID {
			t.Fatalf("binding's head repository = %d, want the fork's", bd.HeadRepositoryID)
		}
	})
	t.Run("GitHub's run record is the base's", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		if run.HeadSHA != e.repo.Head("main") || run.HeadRepo != e.repo || run.SHA != e.repo.Head("main") {
			t.Fatal("the emulator's pull_request_target run doesn't record the base's head")
		}
		e.want(run, boot(), event(run), nil, scope.PullRequest(p.Number))
	})
	t.Run("a same-repository pull request", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, boot(), event(run), nil, scope.PullRequest(p.Number))
		bd, _, _ := e.binder.Lookup(context.Background(), run.Repo.ID, run.ID)
		if bd.HeadRepositoryID != e.repo.ID {
			t.Fatalf("binding's head repository = %d, want the repository's", bd.HeadRepositoryID)
		}
	})
	t.Run("the pull request has moved on since the run", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		e.fork.Commit("feature", map[string]string{"more": "1"})
		e.gh.Sync(p)
		e.want(run, boot(), event(run), nil, scope.PullRequest(p.Number))
	})
	t.Run("a contributor's job claims to be the bootstrap", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		// A job of the calling workflow has the caller's job_workflow_ref.
		e.want(run, noJob, event(run), nil, scope.Read)
	})
	t.Run("a token without job_workflow_ref", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, noJob, event(run), map[string]any{"job_workflow_ref": nil, "job_workflow_sha": nil}, scope.Read)
	})
	t.Run("another reusable workflow", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		job := ghemu.Job{WorkflowRef: "lanescape/app/.github/workflows/build.yml@refs/heads/main"}
		e.want(run, job, event(run), nil, scope.Read)
	})
	t.Run("the bootstrap at another ref", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		job := ghemu.Job{WorkflowRef: "lanescape/app/.github/workflows/bootstrap.yml@refs/heads/feature"}
		e.want(run, job, event(run), nil, scope.Read)
	})
	t.Run("the bootstrap names another pull request", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		other := e.samePull("other")
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, boot(), naming(run, other.Number), nil, scope.Read)
	})
	t.Run("the bootstrap names the fork's other pull request", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		e.fork.Branch("second", e.fork.Head("main"))
		e.fork.Commit("second", map[string]string{"s": "1"})
		other := e.gh.PullRequest(e.repo, "main", e.fork, "second", e.contributor)
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, boot(), naming(run, other.Number), nil, scope.Read)
	})
	t.Run("the bootstrap names another head repository", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		req := event(run)
		req.HeadRepositoryID = e.repo.ID
		e.want(run, boot(), req, nil, scope.Read)
	})
	t.Run("the bootstrap names the base's commit", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		req := event(run)
		req.HeadSHA = run.HeadSHA
		e.want(run, boot(), req, nil, scope.Read)
	})
	t.Run("the bootstrap names a commit of another branch", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		req := event(run)
		req.HeadSHA = e.fork.Commit("unrelated", map[string]string{"u": "1"})
		e.want(run, boot(), req, nil, scope.Read)
	})
	for name, req := range map[string]func(binding.Request) binding.Request{
		"no pull request":      func(r binding.Request) binding.Request { r.PullRequest = 0; return r },
		"no head repository":   func(r binding.Request) binding.Request { r.HeadRepositoryID = 0; return r },
		"no head commit":       func(r binding.Request) binding.Request { r.HeadSHA = ""; return r },
		"a short head commit":  func(r binding.Request) binding.Request { r.HeadSHA = r.HeadSHA[:12]; return r },
		"an uppercase commit":  func(r binding.Request) binding.Request { r.HeadSHA = strings.ToUpper(r.HeadSHA); return r },
		"nothing from the run": func(binding.Request) binding.Request { return binding.Request{} },
	} {
		t.Run("the bootstrap sends "+name, func(t *testing.T) {
			e := setup(t)
			p := e.contribution()
			run := e.gh.PullRequestTargetRun(p)
			e.want(run, boot(), req(event(run)), nil, scope.Read)
		})
	}
	t.Run("a pull request that doesn't exist", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, boot(), naming(run, 99), nil, scope.Read)
	})
	t.Run("the head of two open pull requests", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		e.fork.Branch("copy", e.fork.Head("feature"))
		e.gh.PullRequest(e.repo, "main", e.fork, "copy", e.contributor)
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, boot(), event(run), nil, scope.Read)
	})
	t.Run("no bootstrap on the default branch", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("main", map[string]string{binding.RunnersFile: `{"lanescape-2vcpu-linux-amd64": "ubuntu-24.04"}`})
		// The contributor's branch adds one, which mustn't count.
		e.fork.Commit("feature", map[string]string{binding.RunnersFile: `{"bootstrap": "` + bootstrap + `"}`})
		p := e.gh.PullRequest(e.repo, "main", e.fork, "feature", e.contributor)
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, boot(), event(run), nil, scope.Read)
	})
	t.Run("no runners file", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("main", map[string]string{binding.RunnersFile: ""})
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, boot(), event(run), nil, scope.Read)
	})
	for name, file := range map[string]string{
		"not JSON":                  `{`,
		"bootstrap not a string":    `{"bootstrap": 1}`,
		"bootstrap in another repo": `{"bootstrap": "lanescape/other/.github/workflows/bootstrap.yml@refs/heads/main"}`,
		"bootstrap on a branch":     `{"bootstrap": "lanescape/app/.github/workflows/bootstrap.yml@refs/heads/feature"}`,
		"bootstrap at a tag":        `{"bootstrap": "lanescape/app/.github/workflows/bootstrap.yml@refs/tags/main"}`,
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.repo.Commit("main", map[string]string{binding.RunnersFile: file})
			p := e.contribution()
			var b struct{ Bootstrap any }
			_ = json.Unmarshal([]byte(file), &b)
			ref, _ := b.Bootstrap.(string)
			run := e.gh.PullRequestTargetRun(p)
			e.want(run, ghemu.Job{WorkflowRef: ref}, event(run), nil, scope.Read)
		})
	}
	t.Run("a tag named like the default branch", func(t *testing.T) {
		e := setup(t)
		evil := "lanescape/app/.github/workflows/evil.yml@refs/heads/main"
		e.repo.Tag("main", e.repo.Commit("evil", map[string]string{binding.RunnersFile: `{"bootstrap": "` + evil + `"}`}))
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, ghemu.Job{WorkflowRef: evil}, event(run), nil, scope.Read)
		// The default branch's own bootstrap still works.
		run = e.gh.PullRequestTargetRun(p)
		e.want(run, boot(), event(run), nil, scope.PullRequest(p.Number))
	})
	t.Run("more commits than GitHub lists", func(t *testing.T) {
		e := setup(t)
		var shas []string
		for i := range 260 {
			shas = append(shas, e.fork.Commit("long", map[string]string{"n": fmt.Sprint(i)}))
		}
		p := e.gh.PullRequest(e.repo, "main", e.fork, "long", e.contributor)
		run := e.gh.PullRequestTargetRun(p)
		e.want(run, boot(), event(run), nil, scope.PullRequest(p.Number))

		// A run for the 255th commit, after which the pull request moved on.
		older := e.gh.PullRequestTargetRun(p)
		req := event(older)
		req.HeadSHA = shas[254]
		e.want(older, boot(), req, nil, scope.PullRequest(p.Number))

		// A run whose event names the base's commit, not the pull request's.
		base := e.gh.PullRequestTargetRun(p)
		req = event(base)
		req.HeadSHA = e.repo.Head("main")
		e.want(base, boot(), req, nil, scope.Read)
	})
	t.Run("the head repository has been deleted", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		p.Head = nil
		e.want(run, boot(), event(run), nil, scope.Read)
	})
	t.Run("the bootstrap's word in a pull_request run", func(t *testing.T) {
		e := setup(t)
		p := e.samePull("feature")
		other := e.samePull("other")
		run := e.gh.PullRequestRun(p)
		req := binding.Request{PullRequest: other.Number, HeadRepositoryID: e.repo.ID, HeadSHA: other.HeadSHA}
		e.want(run, boot(), req, nil, scope.PullRequest(p.Number))
	})
}

func TestABindingIsMadeOnce(t *testing.T) {
	e := setup(t)
	p := e.contribution()
	run := e.gh.PullRequestTargetRun(p)
	// The first call isn't the bootstrap's, so the run is read for good.
	e.want(run, noJob, event(run), nil, scope.Read)
	e.want(run, boot(), event(run), nil, scope.Read)

	e2 := setup(t)
	main := e2.gh.PushRun(e2.repo, "main")
	e2.want(main, noJob, binding.Request{}, nil, scope.Main)
	// A later job can't change it, whatever its token says.
	e2.want(main, noJob, binding.Request{}, map[string]any{"ref": "refs/heads/other"}, scope.Main)
}

func TestConcurrentFirstJobsAgree(t *testing.T) {
	e := setup(t)
	p := e.samePull("feature")
	run := e.gh.PullRequestRun(p)
	var wg sync.WaitGroup
	results := make([]binding.Binding, 6)
	errs := make([]error, len(results))
	for i := range results {
		wg.Go(func() { results[i], errs[i] = e.bind(run, noJob, binding.Request{}, nil) })
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Scope != scope.PullRequest(p.Number) || !results[i].BoundAt.Equal(results[0].BoundAt) {
			t.Fatalf("job %d: %+v, %v; want the same binding as job 0, %+v", i, results[i], errs[i], results[0])
		}
	}
}

func TestRefusals(t *testing.T) {
	for _, status := range []string{"queued", "completed", "waiting", "requested"} {
		t.Run("run "+status, func(t *testing.T) {
			e := setup(t)
			run := e.gh.PushRun(e.repo, "main")
			e.gh.SetRunStatus(run, status)
			if _, err := e.bind(run, noJob, binding.Request{}, nil); !errors.Is(err, binding.ErrRefused) {
				t.Fatalf("error = %v, want ErrRefused", err)
			}
			if e.bound(run) {
				t.Fatal("a run that isn't in progress was bound")
			}
		})
	}
	t.Run("GitHub reports another owner", func(t *testing.T) {
		e := setup(t)
		other := e.gh.Org(1234, "lanescape-lookalike")
		theirs := e.gh.Repo(other, 6001, "app")
		run := e.gh.PushRun(theirs, "main")
		// Signed as if the organization owned it.
		_, err := e.bind(run, noJob, binding.Request{}, map[string]any{"repository_owner_id": fmt.Sprint(orgID)})
		if !errors.Is(err, binding.ErrRefused) {
			t.Fatalf("error = %v, want ErrRefused", err)
		}
		if e.bound(run) {
			t.Fatal("another owner's run was bound")
		}
	})
	t.Run("another repository's run", func(t *testing.T) {
		e := setup(t)
		other := e.gh.Repo(e.org, 5002, "other")
		run := e.gh.PushRun(other, "main")
		_, err := e.bind(run, noJob, binding.Request{}, map[string]any{"repository_id": "5001", "repository": "lanescape/app"})
		if !errors.Is(err, binding.ErrRefused) {
			t.Fatalf("error = %v, want ErrRefused", err)
		}
	})
}

// Every GitHub call on each rule's path can fail transiently; none may leave
// a binding, and a later call binds as if nothing had happened.
func TestTransientFailuresLeaveNoBinding(t *testing.T) {
	type path struct {
		name   string
		run    func(*env) (*ghemu.Run, ghemu.Job, binding.Request)
		calls  []string
		expect func(*env, *ghemu.Run) scope.Scope
	}
	paths := []path{
		{
			name: "main",
			run: func(e *env) (*ghemu.Run, ghemu.Job, binding.Request) {
				return e.gh.PushRun(e.repo, "main"), noJob, binding.Request{}
			},
			calls: []string{"/repositories/", "/repos/lanescape/app/actions/runs/", "/repos/lanescape/app/git/ref/", "/repos/lanescape/app/compare/"},
		},
		{
			name: "pull_request",
			run: func(e *env) (*ghemu.Run, ghemu.Job, binding.Request) {
				return e.gh.PullRequestRun(e.samePull("feature")), noJob, binding.Request{}
			},
			calls: []string{"/repos/lanescape/app/pulls/1", "/repos/lanescape/app/pulls"},
		},
		{
			name: "pull_request_target",
			run: func(e *env) (*ghemu.Run, ghemu.Job, binding.Request) {
				p := e.contribution()
				run := e.gh.PullRequestTargetRun(p)
				// The pull request moves on, so its commits are listed.
				e.fork.Commit("feature", map[string]string{"more": "1"})
				e.gh.Sync(p)
				return run, boot(), event(run)
			},
			calls: []string{"/repos/lanescape/app/git/ref/", "/repos/lanescape/app/contents/", "/repos/lanescape/app/pulls/1/commits", "/repos/lanescape/app/pulls"},
		},
	}
	failures := map[string]func(e *env, prefix string){
		"rate limit":           func(e *env, p string) { e.gh.RateLimit(p, 1, time.Second) },
		"secondary rate limit": func(e *env, p string) { e.gh.SecondaryRateLimit(p, 1) },
		"bad gateway":          func(e *env, p string) { e.gh.ServerError(p, http.StatusBadGateway, 1) },
		"unavailable":          func(e *env, p string) { e.gh.ServerError(p, http.StatusServiceUnavailable, 1) },
		"dropped": func(e *env, p string) {
			// Go's transport retries once on a dropped reused connection.
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
					run, job, req := pt.run(e)
					fail(e, call)
					if _, err := e.bind(run, job, req, nil); !errors.Is(err, binding.ErrRetry) {
						t.Fatalf("error = %v, want ErrRetry", err)
					}
					if e.bound(run) {
						t.Fatal("a binding was made from an unconfirmed answer")
					}
				})
			}
		}
	}
	t.Run("retry after a rate limit", func(t *testing.T) {
		e := setup(t)
		p := e.contribution()
		run := e.gh.PullRequestTargetRun(p)
		e.fork.Commit("feature", map[string]string{"more": "1"})
		e.gh.Sync(p)
		e.gh.RateLimit("/repos/lanescape/app/pulls/1/commits", 1, time.Second)
		if _, err := e.bind(run, boot(), event(run), nil); !errors.Is(err, binding.ErrRetry) {
			t.Fatalf("error = %v, want ErrRetry", err)
		}
		time.Sleep(1100 * time.Millisecond)
		e.want(run, boot(), event(run), nil, scope.PullRequest(p.Number))
	})
	t.Run("a slow GitHub", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PushRun(e.repo, "main")
		e.gh.Faults().Add(fault.Rule{PathPrefix: "/repos/lanescape/app/compare/", Delay: time.Minute})
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if _, err := e.bindCtx(ctx, run, noJob, binding.Request{}, nil); !errors.Is(err, binding.ErrRetry) {
			t.Fatalf("error = %v, want ErrRetry", err)
		}
		if e.bound(run) {
			t.Fatal("bound after a timeout")
		}
	})
}

func TestStorageFailures(t *testing.T) {
	t.Run("storage down", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PushRun(e.repo, "main")
		e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost,
			Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := e.bindCtx(ctx, run, noJob, binding.Request{}, nil); !errors.Is(err, binding.ErrRetry) {
			t.Fatalf("error = %v, want ErrRetry", err)
		}
	})
	t.Run("lost response", func(t *testing.T) {
		e := setup(t)
		run := e.gh.PushRun(e.repo, "main")
		e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1, After: true,
			Respond: gcsemu.JSONError(http.StatusServiceUnavailable, "backendError", "Backend Error")})
		e.want(run, noJob, binding.Request{}, nil, scope.Main)
	})
}
