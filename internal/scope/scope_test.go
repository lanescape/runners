package scope

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for _, s := range []Scope{Main, Read, PullRequest(1), PullRequest(42), Branch("v1"), Branch("release2026abcde")} {
		got, err := Parse(s.String())
		if err != nil || got != s {
			t.Errorf("Parse(%q) = %v, %v", s, got, err)
		}
		b, _ := json.Marshal(s)
		var back Scope
		if err := json.Unmarshal(b, &back); err != nil || back != s {
			t.Errorf("JSON round trip of %v: %s, %v, %v", s, b, back, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"", "Main", "pr", "pr0", "pr042", "pr-1", "pr+1", "pr1x", "pr 1", "pr1234567890", "write",
		"branch-", "branch-V1", "branch-1v", "branch-v-1", "branch-release2026abcdefg", "branch-main_", "branchv1"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded", s)
		}
	}
}

func TestSaves(t *testing.T) {
	if !Main.Saves() || !PullRequest(3).Saves() || !Branch("v1").Saves() || Read.Saves() {
		t.Error("main, long-lived branches' and pull requests' scopes save, and read doesn't")
	}
	if n, ok := PullRequest(7).PullRequestNumber(); !ok || n != 7 {
		t.Error("pull request number lost")
	}
	if _, ok := Main.PullRequestNumber(); ok {
		t.Error("main has a pull request number")
	}
	if n, ok := Branch("v1").BranchName(); !ok || n != "v1" {
		t.Error("branch name lost")
	}
	if !Main.Lasting() || !Branch("v1").Lasting() || PullRequest(1).Lasting() || Read.Lasting() {
		t.Error("only main's and long-lived branches' scopes last")
	}
}

func TestChain(t *testing.T) {
	v1 := Branch("v1")
	for _, c := range []struct {
		scope, base Scope
		want        []Scope
	}{
		{Main, Scope{}, []Scope{Main}},
		{Read, Scope{}, []Scope{Main}},
		{PullRequest(4), Main, []Scope{PullRequest(4), Main}},
		{PullRequest(4), v1, []Scope{PullRequest(4), v1}},
		// A pull request's binding with no base recorded falls back to
		// main's.
		{PullRequest(4), Scope{}, []Scope{PullRequest(4), Main}},
		{v1, Scope{}, []Scope{v1, Main}},
	} {
		if got := Chain(c.scope, c.base); !slices.Equal(got, c.want) {
			t.Errorf("Chain(%v, %v) = %v, want %v", c.scope, c.base, got, c.want)
		}
	}
}
