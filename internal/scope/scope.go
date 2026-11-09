// Package scope is a run's scope: where it may read and save disks and cache
// entries (specs/0002-machines.md, Scope).
package scope

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Scope is main, read, a long-lived branch's, branch-<name>, or a pull
// request's, pr<number>.
type Scope struct {
	pr     int
	branch string
	main   bool
}

var (
	Main = Scope{main: true}
	Read = Scope{}
)

// branchName is a long-lived branch's scope name: 1 to 16 lowercase letters
// and digits, starting with a letter, so that it never holds a key's 20 hex
// digits and a selector reads one way only.
var branchName = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// ValidBranchName reports whether name may name a long-lived branch's scope.
func ValidBranchName(name string) bool { return branchName.MatchString(name) }

// PullRequest is pull request n's scope.
func PullRequest(n int) Scope {
	if n <= 0 {
		panic(fmt.Sprintf("scope: pull request number %d", n))
	}
	return Scope{pr: n}
}

// Branch is the scope of the long-lived branch the consumer names name.
func Branch(name string) Scope {
	if !ValidBranchName(name) {
		panic(fmt.Sprintf("scope: branch scope name %q", name))
	}
	return Scope{branch: name}
}

// PullRequestNumber is the scope's pull request, if it's one's.
func (s Scope) PullRequestNumber() (int, bool) { return s.pr, s.pr > 0 }

// BranchName is the scope's long-lived branch's name, if it's one's.
func (s Scope) BranchName() (string, bool) { return s.branch, s.branch != "" }

// IsMain reports whether the scope is main's.
func (s Scope) IsMain() bool { return s.main }

// IsRead reports whether the scope is read.
func (s Scope) IsRead() bool { return s == Read }

// Saves reports whether jobs in the scope may save disks and cache entries.
func (s Scope) Saves() bool { return s.main || s.pr > 0 || s.branch != "" }

// Lasting reports whether the scope is main's or a long-lived branch's:
// one that keeps its two newest keys of each family, and whose families go
// once unused for 90 days (specs/0003-disks.md, Expiry).
func (s Scope) Lasting() bool { return s.main || s.branch != "" }

func (s Scope) String() string {
	switch {
	case s.main:
		return "main"
	case s.pr > 0:
		return "pr" + strconv.Itoa(s.pr)
	case s.branch != "":
		return "branch-" + s.branch
	default:
		return "read"
	}
}

// ParseNumber reads a pull request number as GitHub writes it: decimal,
// positive, with no sign or leading zeros.
func ParseNumber(s string) (int, bool) {
	if s == "" || s[0] == '0' || len(s) > 9 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0
}

// Parse reads a scope as String writes it.
func Parse(s string) (Scope, error) {
	switch s {
	case "main":
		return Main, nil
	case "read":
		return Read, nil
	}
	if rest, ok := strings.CutPrefix(s, "pr"); ok {
		if n, ok := ParseNumber(rest); ok {
			return PullRequest(n), nil
		}
	}
	if rest, ok := strings.CutPrefix(s, "branch-"); ok && ValidBranchName(rest) {
		return Branch(rest), nil
	}
	return Scope{}, fmt.Errorf("scope: %q isn't main, read, branch-<name> or pr<number>", s)
}

// MarshalText implements encoding.TextMarshaler.
func (s Scope) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Scope) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	if err == nil {
		*s = v
	}
	return err
}

// Chain is a run's restore chain (specs/0002-machines.md, Scope): what it
// reads, in order. That's its own scope, if it saves, then its base, the
// scope it falls back to, if it has one: a pull request's base is the one
// its binding recorded, the listed branch it targets or else main's; a
// long-lived branch's push falls back to main's, so a new branch starts
// warm; main's has none; and read reads main's alone.
func Chain(s, base Scope) []Scope {
	switch {
	case s.IsMain() || s.IsRead():
		return []Scope{Main}
	case s.pr > 0:
		if base.IsMain() || base.branch != "" {
			return []Scope{s, base}
		}
		return []Scope{s, Main}
	case s.branch != "":
		return []Scope{s, Main}
	}
	return []Scope{Main}
}
