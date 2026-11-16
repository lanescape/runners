package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/runners/actions/internal/actions"
	"github.com/lanescape/runners/actions/internal/testgithub"
)

func runtime(env map[string]string) *actions.Runtime {
	return &actions.Runtime{Getenv: func(k string) string { return env[k] }, Stdout: &bytes.Buffer{}}
}

func TestDecide(t *testing.T) {
	gh := testgithub.New(t)
	org := gh.Org(endpoints.Organization, "lanescape")
	repo := gh.Repo(org, 5001, "app")
	contributor := gh.User(9001, "contributor")
	fork := gh.Fork(repo, contributor, 7001)
	head := fork.Commit("change", map[string]string{"a": "1"})
	pr := gh.PullRequest(repo, "main", fork, "change", contributor)
	ownFork := gh.Fork(repo, gh.User(9002, "other"), 7002)
	ownFork.Commit("change", map[string]string{"b": "2"})
	ownPR := gh.PullRequest(ownFork, "main", ownFork, "change", ownFork.Owner)

	type want struct {
		repository, ref, persist, unsafe, expect, where string
	}
	ordinary := func(where string) want { return want{"", "", "true", "false", "", where} }
	for _, tc := range []struct {
		name         string
		job          testgithub.Job
		contribution string
		want         *want
		refusal      string
	}{
		{name: "push", job: testgithub.Job{Run: gh.PushRun(repo, "main")}, want: ptr(ordinary("Inside"))},
		{name: "fork's pull request", job: testgithub.Job{Run: gh.PullRequestRun(pr), Pull: pr}, want: ptr(ordinary("Outside"))},
		{name: "pull_request_target without contribution", job: testgithub.Job{Run: gh.PullRequestTargetRun(pr), Pull: pr}, want: ptr(ordinary("Inside"))},
		{name: "approved contribution", job: testgithub.Job{Run: gh.PullRequestTargetRun(pr), Pull: pr}, contribution: "true",
			want: &want{"contributor/app", head, "false", "true", head, "Inside"}},
		{name: "fork's own pull_request_target", job: testgithub.Job{Run: gh.PullRequestTargetRun(ownPR), Pull: ownPR}, contribution: "true",
			want: &want{"other/app", ownPR.HeadSHA, "false", "true", ownPR.HeadSHA, "Outside"}},
		{name: "contribution in a pull_request run", job: testgithub.Job{Run: gh.PullRequestRun(pr), Pull: pr}, contribution: "true",
			refusal: "only in a pull_request_target run"},
		{name: "contribution in a push", job: testgithub.Job{Run: gh.PushRun(repo, "main")}, contribution: "true",
			refusal: "only in a pull_request_target run"},
		{name: "contribution: yes", job: testgithub.Job{Run: gh.PushRun(repo, "main")}, contribution: "yes",
			refusal: "true or false"},
	} {
		tc.job.Inputs = map[string]string{"contribution": tc.contribution}
		env := gh.Env(tc.job)
		err := decide(runtime(env))
		out := testgithub.Outputs(t, env)
		if tc.refusal != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refusal) || len(out) != 0 {
				t.Errorf("%s: err = %v, outputs %v", tc.name, err, out)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		got := want{out["repository"], out["ref"], out["persist-credentials"], out["allow-unsafe-pr-checkout"], out["expect"], ""}
		w := *tc.want
		w.where = ""
		if got != w {
			t.Errorf("%s: outputs %+v, want %+v", tc.name, got, w)
		}
		if s := testgithub.Summary(t, env); !strings.Contains(s, tc.want.where+" the lanescape organization") {
			t.Errorf("%s: summary %q", tc.name, s)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// A contribution's checkout is the commit its run was made for, though
// the pull request has moved on since.
func TestDecideKeepsTheRunsCommit(t *testing.T) {
	gh := testgithub.New(t)
	repo := gh.Repo(gh.Org(endpoints.Organization, "lanescape"), 5001, "app")
	contributor := gh.User(9001, "contributor")
	fork := gh.Fork(repo, contributor, 7001)
	first := fork.Commit("change", map[string]string{"a": "1"})
	pr := gh.PullRequest(repo, "main", fork, "change", contributor)
	run := gh.PullRequestTargetRun(pr)
	later := fork.Commit("change", map[string]string{"a": "2"})
	gh.Sync(pr)
	if gh.PullHead(pr) != later || later == first {
		t.Fatalf("the pull request's head is %s, not %s", gh.PullHead(pr), later)
	}
	env := gh.Env(testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"contribution": "true"}})
	if err := decide(runtime(env)); err != nil {
		t.Fatal(err)
	}
	if out := testgithub.Outputs(t, env); out["ref"] != first || out["expect"] != first {
		t.Fatalf("outputs %v; want the run's commit, %s", out, first)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.org",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.org")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestVerify(t *testing.T) {
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q")
	gitIn(t, ws, "commit", "-q", "--allow-empty", "--no-gpg-sign", "-m", "one")
	first := gitIn(t, ws, "rev-parse", "HEAD")
	gitIn(t, ws, "commit", "-q", "--allow-empty", "--no-gpg-sign", "-m", "two")
	second := gitIn(t, ws, "rev-parse", "HEAD")
	ctx := context.Background()
	env := map[string]string{"GITHUB_WORKSPACE": ws}

	if err := verify(ctx, runtime(env)); err != nil {
		t.Fatalf("nothing expected: %v", err)
	}
	env["INPUT_EXPECT"] = second
	if err := verify(ctx, runtime(env)); err != nil {
		t.Fatalf("the right commit: %v", err)
	}
	env["INPUT_EXPECT"] = first
	if err := verify(ctx, runtime(env)); err == nil || !strings.Contains(err.Error(), "not the contribution's commit") {
		t.Fatalf("the wrong commit: %v", err)
	}
	env["INPUT_EXPECT"] = second
	for _, kv := range [][2]string{
		{"http.https://github.com/.extraheader", "AUTHORIZATION: basic x"},
		{"includeIf.gitdir:/x.path", "/tmp/credentials"},
		{"credential.helper", "store"},
	} {
		gitIn(t, ws, "config", "--local", kv[0], kv[1])
		if err := verify(ctx, runtime(env)); err == nil || !strings.Contains(err.Error(), "may hold credentials") {
			t.Errorf("%s: %v", kv[0], err)
		}
		gitIn(t, ws, "config", "--local", "--unset", kv[0])
	}
}
