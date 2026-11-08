// Package scope is a run's scope: where it may read and save disks
// (specs/0002-machines.md, Selectors).
package scope

import (
	"fmt"
	"strconv"
	"strings"
)

// Scope is main, read, or a pull request's, pr<number>.
type Scope struct {
	pr   int
	main bool
}

var (
	Main = Scope{main: true}
	Read = Scope{}
)

// PullRequest is pull request n's scope.
func PullRequest(n int) Scope {
	if n <= 0 {
		panic(fmt.Sprintf("scope: pull request number %d", n))
	}
	return Scope{pr: n}
}

// PullRequestNumber is the scope's pull request, if it's one's.
func (s Scope) PullRequestNumber() (int, bool) { return s.pr, s.pr > 0 }

// IsMain reports whether the scope is main's.
func (s Scope) IsMain() bool { return s.main }

// Saves reports whether jobs in the scope may save disks.
func (s Scope) Saves() bool { return s.main || s.pr > 0 }

func (s Scope) String() string {
	switch {
	case s.main:
		return "main"
	case s.pr > 0:
		return "pr" + strconv.Itoa(s.pr)
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
	return Scope{}, fmt.Errorf("scope: %q isn't main, read or pr<number>", s)
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
