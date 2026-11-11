// Package consumer reads what a consumer's repository says to the service
// (specs/0005-interface.md, A consumer's files): its .github/runners.json,
// and the keys of its saved disks (specs/0003-disks.md, Keys).
package consumer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/selector"
)

// RunnersFile is the consumer's file.
const RunnersFile = ".github/runners.json"

// NoKey is the one key of a family declared without "from".
const NoKey = "00000000000000000000"

// ErrInvalid means a file isn't what the spec says it must be.
var ErrInvalid = errors.New("invalid")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// Disk is a family's declaration.
type Disk struct {
	// From are the files the family's contents depend on, in order.
	From []string `json:"from,omitempty"`
}

// File is a .github/runners.json.
type File struct {
	// Branches maps each long-lived branch to its scope's name, as written:
	// see Branches for what the service keeps of it.
	Branches map[string]string
	Disks    map[string]Disk
}

// strict decodes a JSON value, refusing unknown fields and anything after
// it.
func strict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("more than one JSON value")
	}
	return nil
}

// Parse reads a .github/runners.json. A file that isn't an object of the
// two fields, exactly so named, or whose disks aren't what the spec says,
// is invalid: any other field, "images" included, makes it invalid
// (specs/0005-interface.md, A consumer's files).
func Parse(data []byte) (File, error) {
	var m map[string]json.RawMessage
	if err := strict(data, &m); err != nil || m == nil {
		return File{}, invalid("%s isn't a JSON object", RunnersFile)
	}
	f := File{Branches: map[string]string{}, Disks: map[string]Disk{}}
	for k, v := range m {
		switch k {
		case "branches":
			var b map[string]json.RawMessage
			if err := json.Unmarshal(v, &b); err != nil || b == nil {
				return File{}, invalid("%s: branches isn't an object", RunnersFile)
			}
			for branch, raw := range b {
				var name string
				if json.Unmarshal(raw, &name) != nil {
					// A branch whose scope name isn't a string is ignored,
					// as Branches ignores one that isn't valid.
					name = "\x00"
				}
				f.Branches[branch] = name
			}
		case "disks":
			var d map[string]json.RawMessage
			if err := json.Unmarshal(v, &d); err != nil || d == nil {
				return File{}, invalid("%s: disks isn't an object", RunnersFile)
			}
			for family, raw := range d {
				if !selector.ValidName(family) {
					return File{}, invalid("%s: %q isn't a family's name", RunnersFile, family)
				}
				// Exactly {} or {"from": [<path>, ...]}: no other field, no
				// other case, and neither null.
				var decl map[string]json.RawMessage
				if err := strict(raw, &decl); err != nil || decl == nil {
					return File{}, invalid("%s: family %s: its declaration isn't {\"from\": [...]} or {}", RunnersFile, family)
				}
				var dd Disk
				for k, v := range decl {
					if k != "from" || !bytes.HasPrefix(bytes.TrimSpace(v), []byte("[")) || json.Unmarshal(v, &dd.From) != nil {
						return File{}, invalid("%s: family %s: its declaration isn't {\"from\": [...]} or {}", RunnersFile, family)
					}
					if dd.From == nil {
						dd.From = []string{}
					}
				}
				seen := map[string]bool{}
				for _, p := range dd.From {
					if !ValidPath(p) {
						return File{}, invalid("%s: family %s: %q isn't a path relative to the repository's root", RunnersFile, family, p)
					}
					if seen[p] {
						return File{}, invalid("%s: family %s lists %s twice", RunnersFile, family, p)
					}
					seen[p] = true
				}
				f.Disks[family] = dd
			}
		default:
			return File{}, invalid("%s has a field %q it may not", RunnersFile, k)
		}
	}
	return f, nil
}

// ValidPath reports whether p is a path relative to the repository's root:
// no leading or trailing slash, no ., .. or empty segment, no backslash.
func ValidPath(p string) bool {
	if p == "" || strings.ContainsRune(p, '\\') || strings.ContainsRune(p, 0) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// BranchScopes is what the service keeps of the default branch's long-lived
// branches: it ignores an entry for the default branch, one whose name
// isn't valid, and every entry whose name another entry shares, and returns
// why it ignored each (specs/0002-machines.md, Long-lived branches).
func (f File) BranchScopes(defaultBranch string) (map[string]string, []string) {
	out := map[string]string{}
	var problems []string
	names := map[string][]string{}
	for branch, name := range f.Branches {
		switch {
		case branch == defaultBranch:
			problems = append(problems, fmt.Sprintf("branch %q: the default branch's scope is main's", branch))
		case branch == "":
			problems = append(problems, "an entry names no branch")
		case !scope.ValidBranchName(name):
			problems = append(problems, fmt.Sprintf("branch %q: its scope's name isn't 1 to 16 lowercase letters and digits, starting with a letter", branch))
		default:
			names[name] = append(names[name], branch)
		}
	}
	for name, branches := range names {
		if len(branches) > 1 {
			slices.Sort(branches)
			problems = append(problems, fmt.Sprintf("branches %s share the name %q", strings.Join(branches, ", "), name))
			continue
		}
		out[branches[0]] = name
	}
	slices.Sort(problems)
	return out, problems
}

// part writes one part of a key: its length in bytes, an
// unsigned 64-bit big-endian number, then its bytes.
func part(h io.Writer, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	h.Write(n[:])
	h.Write(b)
}

// PathContents is a file's path and its contents.
type PathContents struct {
	Path     string
	Contents []byte
}

// Key is a family's key: the first 20 hex digits of the SHA-256 of the
// family, then each listed path and its contents, in the order listed
// (specs/0003-disks.md, Keys). A family declared with no files has NoKey.
func Key(family string, files []PathContents, declared bool) string {
	if !declared {
		return NoKey
	}
	h := sha256.New()
	part(h, []byte(family))
	for _, f := range files {
		part(h, []byte(f.Path))
		part(h, f.Contents)
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}
