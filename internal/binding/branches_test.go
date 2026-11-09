package binding_test

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"testing"

	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/scope"
)

// listing makes the default branch's .github/runners.json list long-lived
// branches, with the bootstrap as before.
func (e *env) listing(branches string) {
	e.repo.Commit("main", map[string]string{binding.RunnersFile: `{"bootstrap": "` + bootstrap + `", "branches": ` + branches + `}`})
}

func TestLongLivedBranches(t *testing.T) {
	t.Run("a push to a listed branch", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"support/v1": "v1"}`)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		e.want(e.gh.PushRun(e.repo, "support/v1"), noJob, binding.Request{}, nil, scope.Branch("v1"))
	})
	t.Run("a listed branch's older commit", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"support/v1": "v1"}`)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		run := e.gh.PushRun(e.repo, "support/v1")
		e.repo.Commit("support/v1", map[string]string{"x": "2"})
		e.want(run, noJob, binding.Request{}, nil, scope.Branch("v1"))
	})
	t.Run("a push to a branch that isn't listed", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"support/v1": "v1"}`)
		e.repo.Commit("support/v2", map[string]string{"x": "1"})
		e.want(e.gh.PushRun(e.repo, "support/v2"), noJob, binding.Request{}, nil, scope.Read)
	})
	t.Run("listed only on the pushed branch's own copy", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("support/v1", map[string]string{binding.RunnersFile: `{"branches": {"support/v1": "v1"}}`})
		e.want(e.gh.PushRun(e.repo, "support/v1"), noJob, binding.Request{}, nil, scope.Read)
	})
	t.Run("a tag named like a listed branch", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"v1": "v1"}`)
		e.repo.Tag("v1", e.repo.Commit("evil", map[string]string{"x": "evil"}))
		e.want(e.gh.TagRun(e.repo, "v1"), noJob, binding.Request{}, nil, scope.Read)
	})
	t.Run("signed as a listed branch, with a commit that isn't on it", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"support/v1": "v1"}`)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		e.repo.Commit("feature", map[string]string{"x": "evil"})
		e.want(e.gh.PushRun(e.repo, "feature"), noJob, binding.Request{}, map[string]any{"ref": "refs/heads/support/v1"}, scope.Read)
	})
	for name, branches := range map[string]string{
		"an entry for the default branch": `{"main": "v1"}`,
		"a name with a hyphen":            `{"support/v1": "v-1"}`,
		"a name starting with a digit":    `{"support/v1": "1v"}`,
		"a name of 17":                    `{"support/v1": "abcdefghijklmnopq"}`,
		"a name in capitals":              `{"support/v1": "V1"}`,
		"a name that isn't a string":      `{"support/v1": 1}`,
		"a name another entry shares":     `{"support/v1": "v1", "release/v1": "v1"}`,
		"branches that aren't an object":  `["support/v1"]`,
	} {
		t.Run(name+" is ignored", func(t *testing.T) {
			e := setup(t)
			e.listing(branches)
			e.repo.Commit("support/v1", map[string]string{"x": "1"})
			e.want(e.gh.PushRun(e.repo, "support/v1"), noJob, binding.Request{}, nil, scope.Read)
		})
	}
	t.Run("GitHub can't read the listing", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"support/v1": "v1"}`)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		run := e.gh.PushRun(e.repo, "support/v1")
		e.gh.Faults().Add(fault.Rule{PathPrefix: "/repos/lanescape/app/contents/", Times: 1,
			Respond: fault.Status(http.StatusBadGateway, nil, `{"message": "Server Error"}`)})
		if _, err := e.bind(run, noJob, binding.Request{}, nil); err == nil {
			t.Fatal("bound with no answer from GitHub")
		}
		if e.bound(run) {
			t.Fatal("a binding was made")
		}
		e.want(run, noJob, binding.Request{}, nil, scope.Branch("v1"))
	})
}

func TestBases(t *testing.T) {
	base := func(e *env, want scope.Scope, chain ...scope.Scope) func(bd binding.Binding) {
		return func(bd binding.Binding) {
			e.t.Helper()
			if bd.Base != want || !slices.Equal(bd.Chain(), chain) {
				e.t.Fatalf("base %v, chain %v; want %v, %v", bd.Base, bd.Chain(), want, chain)
			}
		}
	}
	t.Run("a pull request into the default branch", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"support/v1": "v1"}`)
		p := e.samePull("feature")
		bd, err := e.bind(e.gh.PullRequestRun(p), noJob, binding.Request{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		base(e, scope.Main, scope.PullRequest(p.Number), scope.Main)(bd)
	})
	t.Run("a pull request into a listed branch", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"support/v1": "v1"}`)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		e.repo.Commit("fix", map[string]string{"y": "1"})
		p := e.gh.PullRequest(e.repo, "support/v1", e.repo, "fix", e.org)
		bd, err := e.bind(e.gh.PullRequestRun(p), noJob, binding.Request{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		base(e, scope.Branch("v1"), scope.PullRequest(p.Number), scope.Branch("v1"))(bd)
	})
	t.Run("a pull request into a branch that isn't listed", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		e.repo.Commit("fix", map[string]string{"y": "1"})
		p := e.gh.PullRequest(e.repo, "support/v1", e.repo, "fix", e.org)
		bd, err := e.bind(e.gh.PullRequestRun(p), noJob, binding.Request{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		base(e, scope.Main, scope.PullRequest(p.Number), scope.Main)(bd)
	})
	t.Run("a contribution into a listed branch", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"support/v1": "v1"}`)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		e.fork.Commit("fix", map[string]string{"src.go": "package app"})
		p := e.gh.PullRequest(e.repo, "support/v1", e.fork, "fix", e.contributor)
		run := e.gh.PullRequestTargetRun(p)
		bd, err := e.bind(run, boot(), event(run), nil)
		if err != nil {
			t.Fatal(err)
		}
		if bd.Scope != scope.PullRequest(p.Number) {
			t.Fatalf("scope %v (%s)", bd.Scope, bd.Reason)
		}
		base(e, scope.Branch("v1"), scope.PullRequest(p.Number), scope.Branch("v1"))(bd)
	})
	t.Run("a long-lived branch's push falls back to main's", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"support/v1": "v1"}`)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		bd, err := e.bind(e.gh.PushRun(e.repo, "support/v1"), noJob, binding.Request{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		base(e, scope.Read, scope.Branch("v1"), scope.Main)(bd)
	})
	t.Run("main's and read's chains are main's", func(t *testing.T) {
		e := setup(t)
		bd, err := e.bind(e.gh.PushRun(e.repo, "main"), noJob, binding.Request{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		base(e, scope.Read, scope.Main)(bd)
		bd, err = e.bind(e.gh.EventRun(e.repo, "workflow_dispatch", "main"), noJob, binding.Request{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		base(e, scope.Read, scope.Main)(bd)
	})
	t.Run("a retargeted pull request keeps its run's base", func(t *testing.T) {
		e := setup(t)
		e.listing(`{"support/v1": "v1"}`)
		e.repo.Commit("support/v1", map[string]string{"x": "1"})
		e.repo.Commit("fix", map[string]string{"y": "1"})
		p := e.gh.PullRequest(e.repo, "support/v1", e.repo, "fix", e.org)
		run := e.gh.PullRequestRun(p)
		if _, err := e.bind(run, noJob, binding.Request{}, nil); err != nil {
			t.Fatal(err)
		}
		e.listing(`{}`)
		bd, ok, err := e.binder.Lookup(context.Background(), e.repo.ID, run.ID)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		base(e, scope.Branch("v1"), scope.PullRequest(p.Number), scope.Branch("v1"))(bd)
	})
}

func TestParseRunners(t *testing.T) {
	f, problems, err := binding.ParseRunners([]byte(`{
  "lanescape-2vcpu-linux-amd64": "ubuntu-24.04",
  "bootstrap": "x",
  "branches": {"support/v1": "v1", "main": "m", "release/a": "dup", "release/b": "dup", "bad": "B", "n": 3, "support/v2": "v2"}
}`), "main")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"support/v1": "v1", "support/v2": "v2"}
	if f.Bootstrap != "x" || !maps.Equal(f.Branches, want) {
		t.Fatalf("parsed %+v", f)
	}
	if len(problems) != 4 {
		t.Fatalf("problems %q", problems)
	}
	for _, bad := range []string{``, `[]`, `null`, `"x"`} {
		if _, _, err := binding.ParseRunners([]byte(bad), "main"); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if f, problems, err := binding.ParseRunners([]byte(`{"bootstrap": 1}`), "main"); err != nil || f.Bootstrap != "" || len(problems) != 1 {
		t.Errorf("a bootstrap that isn't a string: %+v %q %v", f, problems, err)
	}
}
