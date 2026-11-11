// Package selector reads the one label a job asks for a machine by
// (specs/0002-machines.md, Selectors):
//
//	lanescape-<vCPUs>vcpu-linux-<amd64|arm64>[-spot]
//
// A selector names a machine and nothing else: no image, disk, scope or
// run. GitHub compares labels without regard to case, so a selector is read
// in lowercase and kept that way throughout. What a job's machine works on,
// the run's scope and its work disk's family and key, the service works out
// for itself, from GitHub's records and the job's own disk request; a
// Selector carries them once resolved, but they're never part of its label.
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

// MaxName is the longest an image's name or a family may be: 1 to 32
// lowercase letters, digits and hyphens, starting with a letter and not
// ending with a hyphen (specs/0005-interface.md, A consumer's files).
const MaxName = 32

// HashLength is how many hex digits of a value's SHA-256 a Compute Engine
// label carries where the value itself may not fit.
const HashLength = 20

// Selector is a parsed selector.
type Selector struct {
	// Shape is the selector's shape, lanescape-<vCPUs>vcpu-linux-<arch>.
	Shape string
	VCPUs int
	Arch  string
	Spot  bool

	// Scope, Family and Key are resolved by the service, never read from
	// the label: the run's bound scope, and the family of the work disk
	// the job asked for, with its key for the run's commit
	// (specs/0003-disks.md, Keys).
	Scope  scope.Scope
	Family string
	Key    string
}

var (
	selectorPattern = regexp.MustCompile(`^(lanescape-([1-9][0-9]{0,3})vcpu-linux-(amd64|arm64))(-spot)?$`)
	namePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// ValidName reports whether name may name an image or a family: 1 to 32
// lowercase letters, digits and hyphens, starting with a letter and not
// ending with a hyphen.
func ValidName(name string) bool {
	return len(name) <= MaxName && namePattern.MatchString(name) && !strings.HasSuffix(name, "-")
}

// Parse reads a selector, in any case.
func Parse(label string) (Selector, error) {
	m := selectorPattern.FindStringSubmatch(strings.ToLower(label))
	if m == nil {
		return Selector{}, fmt.Errorf("selector: %q isn't lanescape-<vCPUs>vcpu-linux-<amd64|arm64>[-spot]", label)
	}
	n, _ := strconv.Atoi(m[2])
	return Selector{Shape: m[1], VCPUs: n, Arch: m[3], Spot: m[4] != ""}, nil
}

// String is the selector's label, in its one lowercase form.
func (s Selector) String() string {
	if s.Spot {
		return s.Shape + "-spot"
	}
	return s.Shape
}

// Same reports whether two selectors are the same label, whatever either
// has resolved.
func (s Selector) Same(o Selector) bool { return s.String() == o.String() }

// Hash is the first HashLength hex digits of the selector's SHA-256.
func (s Selector) Hash() string { return Hash(s.String()) }

// Hash is a value's first HashLength hex digits of SHA-256.
func Hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:HashLength]
}

// HasDisk reports whether the job has asked for a work disk, of a family.
func (s Selector) HasDisk() bool { return s.Family != "" }

// Plain reports whether the selector is the smallest shape, standard: what
// an unbound pull_request_target run's jobs may have (specs/0002-machines.md,
// Unbound runs).
func (s Selector) Plain(smallest string) bool { return s.Shape == smallest && !s.Spot }

// RunnerLabels are the labels a runner made for the selector carries: the
// selector alone, none of GitHub's default labels.
func (s Selector) RunnerLabels() []string { return []string{s.String()} }

// ErrNoSelector means a job's labels name no selector at all.
var ErrNoSelector = errors.New("selector: the job's labels name no selector")

// ErrNotSelector means a job names a label that claims to be a selector,
// starting with lanescape-, and isn't one.
var ErrNotSelector = errors.New("selector: the job names a label that isn't a selector")

// ErrOtherLabels means a job names a selector together with other labels,
// which no runner of the service's has.
var ErrOtherLabels = errors.New("selector: the job names a selector with other labels")

// OfJob reads the selector a job's labels ask for: exactly one label, which
// is one. A job that names no label that claims to be a selector has none;
// one that names a selector with any other label is ErrOtherLabels; one
// whose label claims to be a selector and isn't is ErrNotSelector.
func OfJob(labels []string) (Selector, error) {
	var claims []string
	for _, l := range labels {
		if strings.HasPrefix(strings.ToLower(l), Prefix) {
			claims = append(claims, l)
		}
	}
	if len(claims) == 0 {
		return Selector{}, ErrNoSelector
	}
	if len(claims) > 1 {
		return Selector{}, fmt.Errorf("%w: it names %d selectors", ErrOtherLabels, len(claims))
	}
	s, err := Parse(claims[0])
	if err != nil {
		return Selector{}, fmt.Errorf("%w: %w", ErrNotSelector, err)
	}
	if len(labels) > 1 {
		return Selector{}, fmt.Errorf("%w: %d labels", ErrOtherLabels, len(labels))
	}
	return s, nil
}

// MatchesMachine reports whether a job's labels are exactly one selector,
// the machine's (specs/0002-machines.md, The job-started check).
func MatchesMachine(jobLabels []string, machine Selector) error {
	s, err := OfJob(jobLabels)
	if err != nil {
		return err
	}
	if !s.Same(machine) {
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
