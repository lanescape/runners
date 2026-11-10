package selector_test

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/selector"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]selector.Selector{
		"lanescape-80vcpu-linux-amd64-main": {Shape: "lanescape-80vcpu-linux-amd64", VCPUs: 80, Arch: "amd64", Scope: scope.Main},
		"lanescape-80vcpu-linux-amd64-spot-disk-build-3f9c2e7a10b4d5e6f708-pr42": {Shape: "lanescape-80vcpu-linux-amd64", VCPUs: 80,
			Arch: "amd64", Spot: true, Family: "build", Key: "3f9c2e7a10b4d5e6f708", Scope: scope.PullRequest(42)},
		"lanescape-16vcpu-linux-arm64-disk-tool-chain-0123456789abcdef0123-read": {Shape: "lanescape-16vcpu-linux-arm64", VCPUs: 16,
			Arch: "arm64", Family: "tool-chain", Key: "0123456789abcdef0123", Scope: scope.Read},
		"lanescape-2vcpu-linux-amd64-disk-build-0123456789abcdef0123-branch-v1": {Shape: "lanescape-2vcpu-linux-amd64", VCPUs: 2,
			Arch: "amd64", Family: "build", Key: "0123456789abcdef0123", Scope: scope.Branch("v1")},
		"lanescape-2vcpu-linux-amd64-branch-release2026": {Shape: "lanescape-2vcpu-linux-amd64", VCPUs: 2, Arch: "amd64",
			Scope: scope.Branch("release2026")},
		"LaneScape-2VCPU-Linux-AMD64-Spot-PR7": {Shape: "lanescape-2vcpu-linux-amd64", VCPUs: 2, Arch: "amd64", Spot: true, Scope: scope.PullRequest(7)},
		// A family is letters, digits and hyphens, starting with a
		// letter, up to 32 of them; hyphens anywhere after the first.
		"lanescape-2vcpu-linux-amd64-disk-a--b--0123456789abcdef0123-main": {Shape: "lanescape-2vcpu-linux-amd64", VCPUs: 2,
			Arch: "amd64", Family: "a--b-", Key: "0123456789abcdef0123", Scope: scope.Main},
		"lanescape-2vcpu-linux-amd64-disk-" + strings.Repeat("a", 32) + "-0123456789abcdef0123-main": {Shape: "lanescape-2vcpu-linux-amd64", VCPUs: 2,
			Arch: "amd64", Family: strings.Repeat("a", 32), Key: "0123456789abcdef0123", Scope: scope.Main},
		// A family may be named like the parts around it.
		"lanescape-2vcpu-linux-amd64-disk-spot-main-0123456789abcdef0123-main": {Shape: "lanescape-2vcpu-linux-amd64", VCPUs: 2,
			Arch: "amd64", Family: "spot-main", Key: "0123456789abcdef0123", Scope: scope.Main},
	} {
		got, err := selector.Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", in, got, err, want)
			continue
		}
		if got.String() != strings.ToLower(in) {
			t.Errorf("%q round-trips as %q", in, got.String())
		}
	}
	for _, in := range []string{
		"",
		"lanescape-80vcpu-linux-amd64",                                       // no scope
		"lanescape-80vcpu-linux-x86-main",                                    // architecture
		"lanescape-80vcpu-windows-amd64-main",                                // OS
		"lanescape-0vcpu-linux-amd64-main",                                   // no vCPUs
		"lanescape-080vcpu-linux-amd64-main",                                 // leading zero
		"lanescape-80vcpu-linux-amd64-pr0",                                   // pull request 0
		"lanescape-80vcpu-linux-amd64-pr042",                                 // leading zero
		"lanescape-80vcpu-linux-amd64-pr",                                    // no number
		"lanescape-80vcpu-linux-amd64-feature",                               // not a scope
		"lanescape-80vcpu-linux-amd64-branch-",                               // no branch name
		"lanescape-80vcpu-linux-amd64-branch-v-1",                            // a hyphen in a branch name
		"lanescape-80vcpu-linux-amd64-branch-1v",                             // a branch name starting with a digit
		"lanescape-80vcpu-linux-amd64-branch-abcdefghijklmnopq",              // a branch name of 17
		"lanescape-80vcpu-linux-amd64-main-spot",                             // out of order
		"lanescape-80vcpu-linux-amd64-disk-build-main",                       // no key
		"lanescape-80vcpu-linux-amd64-disk-build-3f9c2e7a10b4d5e6f70-main",   // key too short
		"lanescape-80vcpu-linux-amd64-disk-build-3f9c2e7a10b4d5e6f7088-main", // key too long
		"lanescape-80vcpu-linux-amd64-disk--3f9c2e7a10b4d5e6f708-main",       // empty family
		"lanescape-80vcpu-linux-amd64-disk-b_d-3f9c2e7a10b4d5e6f708-main",    // underscore
		"lanescape-80vcpu-linux-amd64-disk-1build-3f9c2e7a10b4d5e6f708-main", // starts with a digit
		"lanescape-80vcpu-linux-amd64-disk--build-3f9c2e7a10b4d5e6f708-main", // starts with a hyphen
		"lanescape-80vcpu-linux-amd64-disk-" + strings.Repeat("a", 33) + "-3f9c2e7a10b4d5e6f708-main",
		" lanescape-80vcpu-linux-amd64-main",
		"lanescape-80vcpu-linux-amd64-main\n",
		"lanescape-80vcpu-linux-amd64-spot-spot-main",
	} {
		if s, err := selector.Parse(in); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", in, s)
		}
	}
}

func TestHash(t *testing.T) {
	s, _ := selector.Parse("Lanescape-80vcpu-linux-amd64-Main")
	sum := sha256.Sum256([]byte("lanescape-80vcpu-linux-amd64-main"))
	if s.Hash() != hex.EncodeToString(sum[:])[:20] {
		t.Fatalf("hash = %s; it's of the lowercase selector", s.Hash())
	}
}

func TestOfJob(t *testing.T) {
	sel := "lanescape-16vcpu-linux-arm64-main"
	for _, ok := range [][]string{
		{sel},
		{strings.ToUpper(sel)},
		{"self-hosted", sel},
		{sel, "Linux", "ARM64", "self-hosted"},
	} {
		if s, err := selector.OfJob(ok); err != nil || s.String() != sel {
			t.Errorf("OfJob(%v) = %v, %v", ok, s, err)
		}
	}
	for _, bad := range [][]string{
		nil,
		{"self-hosted"},
		{"ubuntu-24.04"},
		{sel, "x64"},
		{sel, "gpu"},
		{sel, "lanescape-16vcpu-linux-arm64-pr1"},
		{"lanescape-16vcpu"},
	} {
		if s, err := selector.OfJob(bad); err == nil {
			t.Errorf("OfJob(%v) = %v, want an error", bad, s)
		}
	}
	m, _ := selector.Parse(sel)
	if err := selector.MatchesMachine([]string{sel, "self-hosted"}, m); err != nil {
		t.Error(err)
	}
	other, _ := selector.Parse("lanescape-16vcpu-linux-arm64-read")
	if err := selector.MatchesMachine([]string{sel}, other); err == nil {
		t.Error("a job for main matched a machine made for read")
	}
	if got := m.RunnerLabels(); !slices.Equal(got, []string{sel, "self-hosted", "linux", "arm64"}) {
		t.Errorf("runner labels = %v", got)
	}
}

func TestPlacements(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	sh := cfg.Shapes.Shapes["lanescape-80vcpu-linux-amd64"]
	a, b := cfg.Shapes.Zones[0], cfg.Shapes.Zones[1]
	want := []selector.Placement{
		{"n4-highcpu-80", a, false}, {"n4-highcpu-80", b, false}, {"c4-highcpu-96", a, false}, {"c4-highcpu-96", b, false},
	}
	if got := selector.Placements(sh, cfg.Shapes.Zones, false); !slices.Equal(got, want) {
		t.Fatalf("standard placements = %v", got)
	}
	spot := selector.Placements(sh, cfg.Shapes.Zones, true)
	if len(spot) != 8 || !spot[0].Spot || !spot[3].Spot || spot[4].Spot || !slices.Equal(spot[4:], want) {
		t.Fatalf("spot placements = %v", spot)
	}
}
