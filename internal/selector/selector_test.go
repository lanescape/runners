package selector_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/selector"
)

func TestParse(t *testing.T) {
	const amd = "lanescape-80vcpu-linux-amd64"
	for in, want := range map[string]selector.Selector{
		amd:                                   {Shape: amd, VCPUs: 80, Arch: "amd64"},
		amd + "-spot":                         {Shape: amd, VCPUs: 80, Arch: "amd64", Spot: true},
		"lanescape-16vcpu-linux-arm64":        {Shape: "lanescape-16vcpu-linux-arm64", VCPUs: 16, Arch: "arm64"},
		"LaneScape-2VCPU-Linux-AMD64-Spot":    {Shape: "lanescape-2vcpu-linux-amd64", VCPUs: 2, Arch: "amd64", Spot: true},
		"lanescape-9999vcpu-linux-arm64-spot": {Shape: "lanescape-9999vcpu-linux-arm64", VCPUs: 9999, Arch: "arm64", Spot: true},
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
		amd + "-main",                      // a scope, which selectors don't carry
		amd + "-run-42",                    // a run, which selectors no longer carry
		amd + "-spot-run-42",               //
		amd + "-image-build",               // an image, which a job asks for in a step
		amd + "-disk-build",                // a disk, likewise
		amd + "-spot-spot",                 //
		"lanescape-80vcpu-linux-x86",       // architecture
		"lanescape-0vcpu-linux-amd64",      // no vCPUs
		"lanescape-080vcpu-linux-amd64",    // leading zero
		"lanescape-10000vcpu-linux-amd64",  // too many digits
		"lanescape-80vcpu-linux-amd64spot", //
		" " + amd,
		amd + "\n",
	} {
		if s, err := selector.Parse(in); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", in, s)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"build", "a", "a-b", "a--b", "tool-chain2", "spot", "x-image", "disk-x", "a-run-b", strings.Repeat("x", 32)} {
		if !selector.ValidName(n) {
			t.Errorf("%q isn't valid", n)
		}
	}
	for _, n := range []string{"", "1a", "-a", "a-", "A", strings.Repeat("x", 33), "a_b", "a/b", "a.b"} {
		if selector.ValidName(n) {
			t.Errorf("%q is valid", n)
		}
	}
}

func TestHash(t *testing.T) {
	s, _ := selector.Parse("Lanescape-80vcpu-linux-amd64-Spot")
	sum := sha256.Sum256([]byte("lanescape-80vcpu-linux-amd64-spot"))
	if s.Hash() != hex.EncodeToString(sum[:])[:20] {
		t.Fatalf("hash = %s; it's of the lowercase selector", s.Hash())
	}
	// What's resolved isn't part of it.
	r := s
	r.Scope, r.Family, r.Key = scope.Main, "build", "0123456789abcdef0123"
	if r.Hash() != s.Hash() || !r.Same(s) || r.String() != s.String() {
		t.Fatal("a resolved scope, family or key changed the selector")
	}
	if s.HasDisk() || !r.HasDisk() {
		t.Fatal("HasDisk isn't whether a family is resolved")
	}
}

func TestOfJob(t *testing.T) {
	sel := "lanescape-16vcpu-linux-arm64"
	for _, ok := range [][]string{{sel}, {strings.ToUpper(sel)}} {
		if s, err := selector.OfJob(ok); err != nil || s.String() != sel {
			t.Errorf("OfJob(%v) = %v, %v", ok, s, err)
		}
	}
	for _, none := range [][]string{nil, {"self-hosted"}, {"ubuntu-24.04"}, {"self-hosted", "linux"}} {
		if _, err := selector.OfJob(none); !errors.Is(err, selector.ErrNoSelector) {
			t.Errorf("OfJob(%v) = %v, want no selector", none, err)
		}
	}
	for _, other := range [][]string{{"self-hosted", sel}, {sel, "linux"}, {sel, "lanescape-16vcpu-linux-arm64-spot"}} {
		if _, err := selector.OfJob(other); !errors.Is(err, selector.ErrOtherLabels) {
			t.Errorf("OfJob(%v) = %v, want other labels", other, err)
		}
	}
	for _, not := range [][]string{{"lanescape-16vcpu"}, {"lanescape-16vcpu-linux-arm64-run-4"}, {"Lanescape-runners"}} {
		if _, err := selector.OfJob(not); !errors.Is(err, selector.ErrNotSelector) {
			t.Errorf("OfJob(%v) = %v, want a label that isn't a selector", not, err)
		}
	}
	m, _ := selector.Parse(sel)
	if err := selector.MatchesMachine([]string{sel}, m); err != nil {
		t.Error(err)
	}
	spot, _ := selector.Parse(sel + "-spot")
	if err := selector.MatchesMachine([]string{sel}, spot); err == nil {
		t.Error("a standard job matched a Spot machine")
	}
	if got := m.RunnerLabels(); !slices.Equal(got, []string{sel}) {
		t.Errorf("runner labels = %v", got)
	}
}

func TestPlain(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shapes.Smallest() != "lanescape-2vcpu-linux-amd64" {
		t.Fatalf("smallest = %s", cfg.Shapes.Smallest())
	}
	p, _ := selector.Parse("lanescape-2vcpu-linux-amd64")
	if !p.Plain(cfg.Shapes.Smallest()) {
		t.Fatal("the smallest shape, standard, isn't plain")
	}
	for _, l := range []string{"lanescape-2vcpu-linux-amd64-spot", "lanescape-16vcpu-linux-amd64", "lanescape-80vcpu-linux-amd64", "lanescape-16vcpu-linux-arm64"} {
		q, _ := selector.Parse(l)
		if q.Plain(cfg.Shapes.Smallest()) {
			t.Errorf("%s is plain", l)
		}
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
