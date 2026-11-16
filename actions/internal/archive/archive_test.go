//go:build linux || darwin

package archive

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// node is what a test makes of an entry of a tree, and compares.
type node struct {
	kind    string
	mode    fs.FileMode
	size    int64
	mtime   int64
	target  string
	sum     [32]byte
	uid, id int
}

// scan is every entry under root, by its relative path.
func scan(t *testing.T, root string) map[string]node {
	t.Helper()
	out := map[string]node{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		n := node{mode: info.Mode() & modeBits, mtime: info.ModTime().UnixNano()}
		if os.Geteuid() == 0 {
			n.uid, n.id = owner(info)
		}
		switch {
		case info.IsDir():
			n.kind = "dir"
		case info.Mode().IsRegular():
			n.kind, n.size = "file", info.Size()
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			n.sum = sha256.Sum256(b)
		case info.Mode()&fs.ModeSymlink != 0:
			n.kind = "symlink"
			n.mode = 0 // a symlink's own mode isn't kept everywhere
			n.target, _ = os.Readlink(p)
		default:
			n.kind = "other"
		}
		out[filepath.ToSlash(rel)] = n
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// writable lets the test's cleanup remove dirs it made read-only.
func writable(t *testing.T, root string) {
	t.Cleanup(func() {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(p, 0o755)
			}
			return nil
		})
	})
}

func at(sec, ns int64) time.Time { return time.Unix(sec, ns) }

// tree makes a small tree of every kind of entry a cache keeps, with
// modes and times to the nanosecond, and some it leaves out.
func tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writable(t, root)
	big := make([]byte, 3<<20+12345)
	rand.Read(big)
	files := map[string]struct {
		data []byte
		mode fs.FileMode
	}{
		"a/one":            {[]byte("one\n"), 0o644},
		"a/run.sh":         {[]byte("#!/bin/sh\n"), 0o755},
		"a/private":        {[]byte("secret"), 0o600},
		"a/empty":          {nil, 0o644},
		"a-b":              {[]byte("a sibling of a/"), 0o644},
		"big/blob":         {big, 0o644},
		"locked/inside":    {[]byte("in a 0700 dir"), 0o640},
		"readonly/kept":    {[]byte("in a 0500 dir"), 0o444},
		"setuid":           {[]byte("x"), 0o755 | fs.ModeSetuid},
		"skip/me":          {[]byte("excluded"), 0o644},
		"a/skip.tmp":       {[]byte("excluded"), 0o644},
		"deep/x/y/z/file":  {[]byte("deep"), 0o644},
		"spaces in/a name": {[]byte("spaces"), 0o644},
		"-early/x":         {[]byte("sorts before ."), 0o644},
	}
	for rel, f := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"empty/nested/dirs", "a/b/c", "unsearchable"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{"a/rel": "../a-b", "abs": "/etc/hosts", "dangling": "nowhere/at/all", "a/dirlink": "b"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(root, "a/fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Times to the nanosecond, deepest first, so a directory's isn't
	// changed after it's set.
	var all []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		all = append(all, p)
		return nil
	})
	slices.Reverse(all)
	for i, p := range all {
		mt := at(1_700_000_000+int64(i)*1000, int64(i)*7919+123456789%1_000_000_000)
		info, _ := os.Lstat(p)
		if info.Mode()&fs.ModeSymlink != 0 {
			r, _ := os.OpenRoot(filepath.Dir(p))
			if err := lchtimes(r, filepath.Base(p), mt); err != nil {
				t.Fatal(err)
			}
			r.Close()
			continue
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	// A time past 2262, beyond what UnixNano holds.
	far := time.Date(2300, 1, 1, 0, 0, 0, 123456789, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "a/one"), far, far); err != nil {
		t.Fatal(err)
	}
	for d, m := range map[string]fs.FileMode{"locked": 0o700, "readonly": 0o500, "empty/nested": 0o750, "unsearchable": 0o400} {
		if err := os.Chmod(filepath.Join(root, d), m); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// save is what a save makes of root, without uploading it: the shards'
// archives and the directories'.
func save(t *testing.T, root string, shards int, exclude ...string) (files [][]byte, dirs []byte, tr *Tree) {
	t.Helper()
	r := rootOf(t, root)
	tr, err := Walk(r, exclude)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range Shard(tr.Files, shards) {
		if len(s) == 0 {
			continue
		}
		var b bytes.Buffer
		if err := Write(&b, r, s); err != nil {
			t.Fatal(err)
		}
		files = append(files, b.Bytes())
	}
	var b bytes.Buffer
	if err := Write(&b, r, tr.Dirs); err != nil {
		t.Fatal(err)
	}
	return files, b.Bytes(), tr
}

// restore extracts archives under dir, the files' at once, then the
// directories'.
func restore(t *testing.T, dir string, files [][]byte, dirs []byte) error {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writable(t, dir)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	x, err := NewRestorer(root, os.Geteuid() == 0)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	var wg sync.WaitGroup
	errs := make([]error, len(files))
	for i, f := range files {
		wg.Go(func() { errs[i] = x.Files(bytes.NewReader(f)) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	if dirs != nil {
		_, err = x.Dirs(bytes.NewReader(dirs))
	}
	return err
}

func TestRoundTrip(t *testing.T) {
	src := tree(t)
	files, dirs, tr := save(t, src, 4, "skip", "./a/skip.tmp")
	if tr.Skipped != 1 {
		t.Errorf("skipped %d, want the fifo", tr.Skipped)
	}
	if len(files) != 4 {
		t.Errorf("%d shards", len(files))
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if err := restore(t, dst, files, dirs); err != nil {
		t.Fatal(err)
	}
	want, got := scan(t, src), scan(t, dst)
	for _, gone := range []string{"skip", "skip/me", "a/skip.tmp", "a/fifo"} {
		if _, ok := want[gone]; !ok {
			t.Fatalf("the tree has no %s", gone)
		}
		delete(want, gone)
	}
	for rel, w := range want {
		g, ok := got[rel]
		if !ok {
			t.Errorf("%s wasn't restored", rel)
		} else if g != w {
			t.Errorf("%s: restored %+v, saved %+v", rel, g, w)
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("%s was restored, but wasn't saved", rel)
		}
	}
	if w, g := want["big/blob"], got["big/blob"]; w.size < 3<<20 || g.mtime%1_000_000_000 == 0 {
		t.Errorf("the large file is %+v", g)
	}
}

// A tree restored before, with directories saved read-only, can be
// restored onto again.
func TestRestoreTwice(t *testing.T) {
	src := tree(t)
	files, dirs, _ := save(t, src, 3, "skip")
	dst := filepath.Join(t.TempDir(), "restored")
	for range 2 {
		if err := restore(t, dst, files, dirs); err != nil {
			t.Fatal(err)
		}
	}
	want, got := scan(t, src), scan(t, dst)
	delete(want, "skip")
	delete(want, "skip/me")
	delete(want, "a/fifo")
	if !equalScans(want, got) {
		t.Fatalf("restored twice: %v\nsaved: %v", got, want)
	}
}

// Restoring onto a tree overwrites what's there, replacing a symlink in a
// file's place rather than writing through it.
func TestRestoreOverwrites(t *testing.T) {
	src := tree(t)
	files, dirs, _ := save(t, src, 2)
	dst := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("untouched"), 0o644)
	os.MkdirAll(filepath.Join(dst, "a"), 0o755)
	os.Symlink(outside, filepath.Join(dst, "a/one"))
	os.WriteFile(filepath.Join(dst, "a/run.sh"), []byte("old contents, longer than the new"), 0o600)
	os.MkdirAll(filepath.Join(dst, "abs/full"), 0o755)
	os.WriteFile(filepath.Join(dst, "abs/full/f"), []byte("in a read-only directory where a symlink goes"), 0o644)
	os.Chmod(filepath.Join(dst, "abs/full"), 0o500)
	os.Chmod(filepath.Join(dst, "abs"), 0o500)
	os.MkdirAll(filepath.Join(dst, "a-b"), 0o755)
	os.WriteFile(filepath.Join(dst, "a-b/f"), []byte("in a read-only directory where a file goes"), 0o644)
	os.Chmod(filepath.Join(dst, "a-b"), 0o500)
	os.MkdirAll(filepath.Join(dst, "setuid/was/a/dir"), 0o755)
	if err := restore(t, dst, files, dirs); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "untouched" {
		t.Fatalf("wrote through a symlink: %q", b)
	}
	want, got := scan(t, src), scan(t, dst)
	for _, rel := range []string{"a-b", "a/one", "a/run.sh", "setuid", "abs"} {
		if got[rel] != want[rel] {
			t.Errorf("%s: %+v, want %+v", rel, got[rel], want[rel])
		}
	}
}

// archive makes a tar.zst of headers, each with its body.
func archive(t *testing.T, entries ...any) []byte {
	t.Helper()
	var b bytes.Buffer
	zw, _ := zstd.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for i := 0; i < len(entries); i++ {
		h := entries[i].(*tar.Header)
		if h.Mode == 0 && h.Typeflag != tar.TypeXGlobalHeader {
			h.Mode = 0o644
		}
		var body []byte
		if i+1 < len(entries) {
			if s, ok := entries[i+1].(string); ok {
				body = []byte(s)
				h.Size = int64(len(body))
				i++
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

func reg(name string) *tar.Header { return &tar.Header{Typeflag: tar.TypeReg, Name: name} }
func link(name, target string) *tar.Header {
	return &tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target}
}
func dir(name string) *tar.Header { return &tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o777} }

// Archives are untrusted: nothing in one reaches outside the directory
// it's restored under.
func TestRefusesEscapes(t *testing.T) {
	outer := t.TempDir()
	victim := filepath.Join(outer, "victim")
	os.MkdirAll(victim, 0o755)
	os.WriteFile(filepath.Join(victim, "file"), []byte("untouched"), 0o644)
	os.MkdirAll(filepath.Join(victim, "b"), 0o755)
	before := scan(t, outer)
	for name, c := range map[string]struct {
		files [][]byte
		dirs  []byte
		// inTurn extracts the archives of files one after another, rather
		// than at once.
		inTurn bool
	}{
		"..":                           {files: [][]byte{archive(t, reg("../victim/file"), "x")}},
		"an absolute name":             {files: [][]byte{archive(t, reg(filepath.Join(victim, "file")), "x")}},
		".. within":                    {files: [][]byte{archive(t, reg("a/../../victim/file"), "x")}},
		". within":                     {files: [][]byte{archive(t, reg("a/./b"), "x")}},
		"the root as a file":           {files: [][]byte{archive(t, reg("."), "x")}},
		"an absolute symlink's parent": {files: [][]byte{archive(t, link("l", victim), reg("l/file"), "x")}},
		"a relative symlink's parent":  {files: [][]byte{archive(t, link("l", "../../victim"), reg("l/file"), "x")}},
		"a symlink's parent, in another archive": {inTurn: true, files: [][]byte{
			archive(t, link("l", victim)), archive(t, reg("l/x/file"), "x")}},
		"a symlink as a directory": {files: [][]byte{archive(t, link("l", victim))}, dirs: archive(t, dir("l/"))},
		"a symlink's parent, as a directory": {files: [][]byte{archive(t, link("l", victim))},
			dirs: archive(t, dir("l/new/"))},
		"a symlink in place of a directory already made": {files: [][]byte{archive(t, reg("a/b/first"), "x", link("a", victim), reg("a/b/injected"), "x")}},
		"a symlink to a directory inside":                {files: [][]byte{archive(t, reg("d/x"), "x", link("l", "d"), reg("l/y"), "y")}},
		"a hard link":                                    {files: [][]byte{archive(t, &tar.Header{Typeflag: tar.TypeLink, Name: "h", Linkname: "../victim/file"})}},
		"a device":                                       {files: [][]byte{archive(t, &tar.Header{Typeflag: tar.TypeChar, Name: "dev"})}},
		"a directory among files":                        {files: [][]byte{archive(t, dir("d/"))}},
		"a file among directories":                       {dirs: archive(t, reg("f"), "x")},
		"a directory outside":                            {dirs: archive(t, dir("../victim/"))},
		"a file cut short":                               {files: [][]byte{archive(t, reg("f"), "x")[:20]}},
		"a symlink with no target":                       {files: [][]byte{archive(t, link("l", ""))}},
		"an absolute directory":                          {dirs: archive(t, dir(victim+"/"))},
		"a global header":                                {files: [][]byte{archive(t, &tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "x"}})}},
		"not an archive":                                 {files: [][]byte{[]byte("not zstd")}},
		"a directory in the wrong dir":                   {dirs: archive(t, dir("a/../../victim/"))},
	} {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "root")
			var err error
			if c.inTurn {
				for _, f := range c.files {
					if err = restore(t, dst, [][]byte{f}, nil); err != nil {
						break
					}
				}
			} else {
				err = restore(t, dst, c.files, c.dirs)
			}
			if err == nil {
				t.Fatal("restored")
			}
			if after := scan(t, outer); !equalScans(before, after) {
				t.Fatalf("outside changed: %v", after)
			}
		})
	}
}

func equalScans(a, b map[string]node) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestTreeOrder(t *testing.T) {
	paths := []string{"a-b", "a/x", "a", "a/x/y", "b", ".", "a.b", "a/x-y", "a0"}
	slices.SortFunc(paths, func(a, b string) int {
		if a == "." {
			return -1
		}
		if b == "." {
			return 1
		}
		return TreeOrder(a, b)
	})
	want := []string{".", "a", "a/x", "a/x/y", "a/x-y", "a-b", "a.b", "a0", "b"}
	if !slices.Equal(paths, want) {
		t.Fatalf("%v, want %v", paths, want)
	}
}

// The walk lists the root first, and each directory before everything
// under it.
func TestWalkOrdersDirectories(t *testing.T) {
	tr, err := Walk(rootOf(t, tree(t)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Dirs[0].Rel != "." {
		t.Fatalf("first %s", tr.Dirs[0].Rel)
	}
	seen := map[string]bool{".": true}
	for _, d := range tr.Dirs[1:] {
		parent := filepath.Dir(d.Rel)
		if !seen[parent] {
			t.Fatalf("%s before its parent", d.Rel)
		}
		seen[d.Rel] = true
	}
}

// Shards are balanced by the reads they take: a file each, and one for
// each 64 KiB of it.
func TestShardsBalanceReads(t *testing.T) {
	var entries []Entry
	for i := range 1000 {
		entries = append(entries, Entry{Rel: fmt.Sprintf("small/%04d", i), Kind: File, Size: 100})
	}
	for i := range 10 {
		entries = append(entries, Entry{Rel: fmt.Sprintf("big/%d", i), Kind: File, Size: 10 << 20})
	}
	shards := Shard(entries, 8)
	var lo, hi int64 = 1 << 62, 0
	n := 0
	for _, s := range shards {
		var cost int64
		for _, e := range s {
			cost += Cost(e)
		}
		lo, hi = min(lo, cost), max(hi, cost)
		n += len(s)
		if !slices.IsSortedFunc(s, func(a, b Entry) int { return TreeOrder(a.Rel, b.Rel) }) {
			t.Error("a shard isn't in path order")
		}
	}
	if n != len(entries) {
		t.Fatalf("%d entries in shards, of %d", n, len(entries))
	}
	// 10 files of 161 reads and 1000 of 2 among 8 shards: 451 each.
	if hi-lo > 161 {
		t.Fatalf("shards cost from %d to %d", lo, hi)
	}
}

func TestWalkFailures(t *testing.T) {
	for _, ex := range []string{"/abs", "../up", "a/../b", "."} {
		if _, err := Walk(rootOf(t, t.TempDir()), []string{ex}); err == nil || !strings.Contains(err.Error(), "exclude") {
			t.Errorf("exclude %q: %v", ex, err)
		}
	}
	root := t.TempDir()
	writable(t, root)
	os.MkdirAll(filepath.Join(root, "closed"), 0o755)
	os.Chmod(filepath.Join(root, "closed"), 0)
	// Root, or a process that may read any directory, reads it anyway.
	if f, err := os.Open(filepath.Join(root, "closed")); err == nil {
		f.Close()
	} else {
		if _, err := Walk(rootOf(t, root), nil); err == nil {
			t.Error("walked past a directory it can't read")
		}
	}
}

// A file that's gone, or can't be read, by the time it's archived fails
// the save.
func TestWriteFailures(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644)
	r := rootOf(t, root)
	tr, err := Walk(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(root, "f"))
	if err := Write(io.Discard, r, tr.Files); err == nil || !strings.Contains(err.Error(), "the file f") {
		t.Errorf("a file that vanished: %v", err)
	}
	os.Symlink("x", filepath.Join(root, "f"))
	if err := Write(io.Discard, r, tr.Files); err == nil {
		t.Error("archived a symlink that took a file's place")
	}
}

func rootOf(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// What the tree becomes after the walk can't take the save outside it,
// through a directory that's become a symlink, nor stop it on a named
// pipe in a file's place.
func TestWriteStaysInTheTree(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(root, "a"), 0o755)
	os.WriteFile(filepath.Join(root, "a/secret"), []byte("inside"), 0o644)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("OUTSIDE"), 0o644)
	r := rootOf(t, root)
	tr, err := Walk(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	os.Rename(filepath.Join(root, "a"), filepath.Join(root, "a.old"))
	os.Symlink(outside, filepath.Join(root, "a"))
	var b bytes.Buffer
	if err := Write(&b, r, tr.Files); err == nil || bytes.Contains(b.Bytes(), []byte("OUTSIDE")) {
		t.Fatalf("archived through a symlink to outside: %v", err)
	}

	os.Remove(filepath.Join(root, "a"))
	os.Rename(filepath.Join(root, "a.old"), filepath.Join(root, "a"))
	os.Remove(filepath.Join(root, "a/secret"))
	if err := syscall.Mkfifo(filepath.Join(root, "a/secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Write(io.Discard, r, tr.Files) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "isn't a regular file") {
			t.Fatalf("a named pipe in a file's place: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waited on a named pipe in a file's place")
	}
	if f, err := openDir(baseOf(t, r), "a/secret"); err == nil {
		f.Close()
		t.Fatal("opened a named pipe as a directory")
	}
}

// What's become a symlink since the walk is never followed, inside the
// tree either.
func TestWriteFollowsNoSymlink(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "d"), 0o755)
	os.WriteFile(filepath.Join(root, "f"), []byte("f"), 0o644)
	os.WriteFile(filepath.Join(root, "other"), []byte("OTHER"), 0o644)
	r := rootOf(t, root)
	tr, err := Walk(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	var f []Entry
	for _, e := range tr.Files {
		if e.Rel == "f" {
			f = append(f, e)
		}
	}
	os.Remove(filepath.Join(root, "f"))
	os.Symlink("other", filepath.Join(root, "f"))
	var b bytes.Buffer
	if err := Write(&b, r, f); err == nil {
		t.Fatal("archived the file a symlink in its place links to")
	}
	// Nor through a directory that's become a symlink.
	os.MkdirAll(filepath.Join(root, "x/d"), 0o755)
	os.WriteFile(filepath.Join(root, "x/d/f"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(root, "excluded"), 0o755)
	os.WriteFile(filepath.Join(root, "excluded/f"), []byte("EXCLUDED"), 0o644)
	tr, err = Walk(r, []string{"excluded"})
	if err != nil {
		t.Fatal(err)
	}
	f = nil
	for _, e := range tr.Files {
		if e.Rel == "x/d/f" {
			f = append(f, e)
		}
	}
	os.RemoveAll(filepath.Join(root, "x/d"))
	os.Symlink("../excluded", filepath.Join(root, "x/d"))
	b.Reset()
	if err := Write(&b, r, f); err == nil {
		t.Fatal("archived a file through a directory that became a symlink")
	}
	os.Symlink("d", filepath.Join(root, "l"))
	if d, err := openDir(baseOf(t, r), "l"); err == nil {
		d.Close()
		t.Fatal("opened a symlink as a directory")
	}
}

func baseOf(t *testing.T, r *os.Root) *os.File {
	t.Helper()
	f, err := r.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
