package scope

import (
	"encoding/json"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for _, s := range []Scope{Main, Read, PullRequest(1), PullRequest(42)} {
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
	for _, s := range []string{"", "Main", "pr", "pr0", "pr042", "pr-1", "pr+1", "pr1x", "pr 1", "pr1234567890", "write"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded", s)
		}
	}
}

func TestSaves(t *testing.T) {
	if !Main.Saves() || !PullRequest(3).Saves() || Read.Saves() {
		t.Error("only main and pull requests' scopes save")
	}
	if n, ok := PullRequest(7).PullRequestNumber(); !ok || n != 7 {
		t.Error("pull request number lost")
	}
	if _, ok := Main.PullRequestNumber(); ok {
		t.Error("main has a pull request number")
	}
}
