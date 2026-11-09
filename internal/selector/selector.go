// Package selector reads the one label a job asks for a machine by
// (specs/0002-machines.md, Selectors):
//
//	lanescape-<vCPUs>vcpu-linux-<amd64|arm64>[-spot][-disk-<family>-<key>]-<scope>
//
// GitHub compares labels without regard to case, so a selector is read in
// lowercase and kept that way throughout.
package selector

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/internal/scope"
)

// Prefix begins every selector, and every label that claims to be one.
const Prefix = "lanescape-"

// MaxFamily is the longest a saved disk's family may be: 1 to 32
// lowercase letters, digits and hyphens, starting with a letter
// (specs/0005-interface.md, Selectors).
const MaxFamily = 32

// HashLength is how many hex digits of a selector's SHA-256 its machine's
// labels carry.
const HashLength = 20

// Selector is a parsed selector.
type Selector struct {
	// Shape is the selector's shape, lanescape-<vCPUs>vcpu-linux-<arch>.
	Shape  string
	VCPUs  int
	Arch   string
	Spot   bool
	Family string
	Key    string
	Scope  scope.Scope
}

var pattern = regexp.MustCompile(`^(lanescape-([1-9][0-9]{0,3})vcpu-linux-(amd64|arm64))(-spot)?(?:-disk-([a-z][a-z0-9-]*)-([0-9a-f]{20}))?-(main|read|pr[1-9][0-9]{0,8})$`)

// Parse reads a selector, in any case.
func Parse(label string) (Selector, error) {
	l := strings.ToLower(label)
	m := pattern.FindStringSubmatch(l)
	if m == nil {
		return Selector{}, fmt.Errorf("selector: %q isn't lanescape-<vCPUs>vcpu-linux-<amd64|arm64>[-spot][-disk-<family>-<key>]-<scope>", label)
	}
	if len(m[5]) > MaxFamily {
		return Selector{}, fmt.Errorf("selector: %q: family is longer than %d characters", label, MaxFamily)
	}
	sc, err := scope.Parse(m[7])
	if err != nil {
		return Selector{}, fmt.Errorf("selector: %q: %w", label, err)
	}
	n, _ := strconv.Atoi(m[2])
	return Selector{Shape: m[1], VCPUs: n, Arch: m[3], Spot: m[4] != "", Family: m[5], Key: m[6], Scope: sc}, nil
}

// String is the selector in its one lowercase form.
func (s Selector) String() string {
	var b strings.Builder
	b.WriteString(s.Shape)
	if s.Spot {
		b.WriteString("-spot")
	}
	if s.Family != "" {
		b.WriteString("-disk-" + s.Family + "-" + s.Key)
	}
	b.WriteString("-" + s.Scope.String())
	return b.String()
}

// Hash is the first HashLength hex digits of the selector's SHA-256, which
// fits a Compute Engine label where the selector itself may not.
func (s Selector) Hash() string { return Hash(s.String()) }

// Hash is a value's first HashLength hex digits of SHA-256.
func Hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:HashLength]
}

// HasDisk reports whether the selector asks for a saved disk.
func (s Selector) HasDisk() bool { return s.Family != "" }

// archLabel is GitHub's own label for an architecture.
func archLabel(arch string) string {
	if arch == "arm64" {
		return "arm64"
	}
	return "x64"
}

// RunnerLabels are the labels a runner made for the selector carries:
// exactly the selector, and GitHub's own self-hosted, OS and architecture
// labels.
func (s Selector) RunnerLabels() []string {
	return []string{s.String(), "self-hosted", "linux", archLabel(s.Arch)}
}

// IsGitHubLabel reports whether label is one of GitHub's own labels that a
// runner for arch has.
func IsGitHubLabel(label, arch string) bool {
	switch strings.ToLower(label) {
	case "self-hosted", "linux", archLabel(arch):
		return true
	}
	return false
}

// ErrNoSelector means a job's labels name no selector at all.
var ErrNoSelector = errors.New("selector: the job's labels name no selector")

// OfJob reads the selector a job's labels ask for: exactly one label that
// claims to be a selector, and that is one, with nothing else but GitHub's
// own labels that its runner would have.
func OfJob(labels []string) (Selector, error) {
	var claims []string
	for _, l := range labels {
		if strings.HasPrefix(strings.ToLower(l), Prefix) {
			claims = append(claims, l)
		}
	}
	switch len(claims) {
	case 0:
		return Selector{}, ErrNoSelector
	case 1:
	default:
		return Selector{}, fmt.Errorf("selector: the job's labels name %d selectors", len(claims))
	}
	s, err := Parse(claims[0])
	if err != nil {
		return Selector{}, err
	}
	for _, l := range labels {
		if l != claims[0] && !IsGitHubLabel(l, s.Arch) {
			return Selector{}, fmt.Errorf("selector: the job's label %q isn't one its runner has", l)
		}
	}
	return s, nil
}

// MatchesMachine reports whether a job's labels hold exactly one selector,
// the machine's, with nothing else but GitHub's own labels that the
// machine's runner has (specs/0002-machines.md, The job-started check).
func MatchesMachine(jobLabels []string, machine Selector) error {
	s, err := OfJob(jobLabels)
	if err != nil {
		return err
	}
	if s != machine {
		return fmt.Errorf("selector: the job asks for %s, not this machine's %s", s, machine)
	}
	return nil
}

// Placement is one place a machine may be made: a machine type in a zone,
// Spot or standard.
type Placement struct {
	MachineType string
	Zone        string
	Spot        bool
}

// Placements are where a selector's machine may be made, in the order to
// try them: each of the shape's machine types in its order of preference,
// in each zone in order. A Spot selector tries every Spot placement first,
// and then, only for want of Spot capacity, every standard one; the caller
// decides when to go on to those.
func Placements(sh *config.Shape, zones []string, spot bool) []Placement {
	var out []Placement
	add := func(spot bool) {
		for _, mt := range sh.MachineTypes {
			for _, z := range zones {
				out = append(out, Placement{MachineType: mt, Zone: z, Spot: spot})
			}
		}
	}
	if spot {
		add(true)
	}
	add(false)
	return slices.Clip(out)
}
