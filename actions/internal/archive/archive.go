// Package archive is how a cache's tree becomes archives and comes back
// (specs/0007-actions.md, save-cache and cache): a parallel walk, shards
// of files and symlinks balanced by the reads they take, and tar archives
// in PAX format, compressed with zstd, of each shard and of the
// directories.
package archive

import (
	"archive/tar"
	"bufio"
	"container/heap"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Kind is what an entry is.
type Kind uint8

const (
	Dir Kind = iota
	File
	Symlink
)

// Entry is a directory, regular file or symlink of the tree, as the walk
// saw it.
type Entry struct {
	// Rel is its path relative to the tree's root, with slashes; "." is the
	// root itself.
	Rel  string
	Kind Kind
	// Mode is its permissions, with the setuid, setgid and sticky bits.
	Mode fs.FileMode
	// UID and GID are its numeric owner.
	UID, GID int
	// Size is a file's size.
	Size int64
	// ModTime is its modification time, to the nanosecond.
	ModTime time.Time
}

// Tree is what a walk found.
type Tree struct {
	// Dirs are the directories, the root's first, in tree order
	// (TreeOrder).
	Dirs []Entry
	// Files are the regular files and symlinks, in no order.
	Files []Entry
	// Bytes is the files' total size.
	Bytes int64
	// Skipped counts the sockets, devices and named pipes left out.
	Skipped int
}

// Readers is how many directories the walk reads at once.
const Readers = 64

// Shards is how many archives of files a save makes, at most.
const Shards = 64

// modeBits are the bits of a mode an entry keeps.
const modeBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// Walk lists the tree under root, without following symlinks, reading
// Readers directories at once. It takes each directory's kind from its
// parent's listing and its metadata from the open directory, and lstats
// only what isn't a directory. exclude are paths relative to root, with
// slashes, left out with everything under them. Anything that can't be
// read fails the walk. Every directory is opened from root one component
// at a time, following no symlink, and every entry is looked at in its
// open directory, so nothing the tree becomes while it's walked takes the
// walk outside it or through a symlink.
func Walk(root *os.Root, exclude []string) (*Tree, error) {
	ex := map[string]bool{}
	for _, e := range exclude {
		c, err := CleanRel(strings.TrimPrefix(e, "./"))
		if err != nil || c == "." {
			return nil, fmt.Errorf("exclude %q isn't a path under the tree: it must be relative, with no . or .. in it", e)
		}
		ex[c] = true
	}
	base, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer base.Close()
	w := &walker{base: base, exclude: ex, todo: []string{"."}, pending: 1}
	w.cond = sync.NewCond(&w.mu)
	parts := make([]Tree, Readers)
	var wg sync.WaitGroup
	for i := range parts {
		wg.Go(func() { w.work(&parts[i]) })
	}
	wg.Wait()
	if w.err != nil {
		return nil, w.err
	}
	t := &Tree{}
	for _, p := range parts {
		t.Dirs = append(t.Dirs, p.Dirs...)
		t.Files = append(t.Files, p.Files...)
		t.Bytes += p.Bytes
		t.Skipped += p.Skipped
	}
	slices.SortFunc(t.Dirs, func(a, b Entry) int {
		// The root first, though a name such as -x sorts before ".".
		switch {
		case a.Rel == b.Rel:
			return 0
		case a.Rel == ".":
			return -1
		case b.Rel == ".":
			return 1
		}
		return TreeOrder(a.Rel, b.Rel)
	})
	return t, nil
}

type walker struct {
	// base is the root, open.
	base    *os.File
	exclude map[string]bool

	mu   sync.Mutex
	cond *sync.Cond
	todo []string
	// pending counts the directories listed but not yet read.
	pending int
	err     error
}

func (w *walker) next() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(w.todo) == 0 && w.pending > 0 && w.err == nil {
		w.cond.Wait()
	}
	if w.pending == 0 || w.err != nil {
		return "", false
	}
	d := w.todo[len(w.todo)-1]
	w.todo = w.todo[:len(w.todo)-1]
	return d, true
}

func (w *walker) done(subdirs []string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil && w.err == nil {
		w.err = err
	}
	w.todo = append(w.todo, subdirs...)
	w.pending += len(subdirs) - 1
	w.cond.Broadcast()
}

func (w *walker) work(out *Tree) {
	for {
		rel, ok := w.next()
		if !ok {
			return
		}
		subdirs, err := w.read(rel, out)
		w.done(subdirs, err)
	}
}

// read lists one directory, adding it and what isn't a directory in it to
// out, and returns its subdirectories.
func (w *walker) read(rel string, out *Tree) ([]string, error) {
	f, err := openDir(w.base, rel)
	if err != nil {
		return nil, fmt.Errorf("reading the directory %s: %w", rel, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading the directory %s: %w", rel, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s isn't a directory any more", rel)
	}
	out.Dirs = append(out.Dirs, entryOf(rel, Dir, info))
	list, err := f.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("reading the directory %s: %w", rel, err)
	}
	var subdirs []string
	for _, d := range list {
		child := d.Name()
		if rel != "." {
			child = rel + "/" + child
		}
		if w.exclude[child] {
			continue
		}
		if d.IsDir() {
			subdirs = append(subdirs, child)
			continue
		}
		info, err := lstatIn(f, d.Name())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", child, err)
		}
		switch m := info.Mode(); {
		case m.IsRegular():
			e := entryOf(child, File, info)
			out.Files = append(out.Files, e)
			out.Bytes += e.Size
		case m&fs.ModeSymlink != 0:
			out.Files = append(out.Files, entryOf(child, Symlink, info))
		case m.IsDir():
			// A directory that took a file's place since the listing.
			subdirs = append(subdirs, child)
		default:
			out.Skipped++
		}
	}
	return subdirs, nil
}

func entryOf(rel string, k Kind, info fs.FileInfo) Entry {
	uid, gid := owner(info)
	if s, ok := info.(*statInfo); ok {
		uid, gid = s.uid, s.gid
	}
	e := Entry{Rel: rel, Kind: k, Mode: info.Mode() & modeBits, UID: uid, GID: gid, ModTime: info.ModTime()}
	if k == File {
		e.Size = info.Size()
	}
	return e
}

// TreeOrder compares paths by their components, so that everything under
// a directory follows it at once: a, a/b, a-b, where a plain comparison
// puts a-b before a/b.
func TreeOrder(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		x, y := a[i], b[i]
		if x == y {
			continue
		}
		if x == '/' {
			return -1
		}
		if y == '/' {
			return 1
		}
		if x < y {
			return -1
		}
		return 1
	}
	return len(a) - len(b)
}

// BlockSize is the read the cost of a file counts: one for the file, and
// one more for each BlockSize of it.
const BlockSize = 64 << 10

// Cost is how many reads an entry is expected to take.
func Cost(e Entry) int64 {
	return 1 + (e.Size+BlockSize-1)/BlockSize
}

// Shard splits entries into n shards whose costs are as even as greedy
// assignment, largest first, makes them, each in path order. Shards may
// be empty.
func Shard(entries []Entry, n int) [][]Entry {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b Entry) int {
		if c := Cost(b) - Cost(a); c != 0 {
			if c < 0 {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Rel, b.Rel)
	})
	shards := make([][]Entry, n)
	h := make(loads, n)
	for i := range h {
		h[i] = load{shard: i}
	}
	for _, e := range sorted {
		h[0].cost += Cost(e)
		shards[h[0].shard] = append(shards[h[0].shard], e)
		heap.Fix(&h, 0)
	}
	for _, s := range shards {
		slices.SortFunc(s, func(a, b Entry) int { return TreeOrder(a.Rel, b.Rel) })
	}
	return shards
}

type load struct {
	shard int
	cost  int64
}

type loads []load

func (h loads) Len() int { return len(h) }
func (h loads) Less(i, j int) bool {
	if h[i].cost != h[j].cost {
		return h[i].cost < h[j].cost
	}
	return h[i].shard < h[j].shard
}
func (h loads) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *loads) Push(x any)   { *h = append(*h, x.(load)) }
func (h *loads) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// Level is the zstd level archives are compressed at.
const Level = 3

// window is the zstd window archives are written with: zstd's own for
// level 3. maxWindow is the largest a restore accepts.
const (
	window    = 2 << 20
	maxWindow = 32 << 20
)

// copyBuffer is how much of a file each read asks for.
const copyBuffer = 1 << 20

// Write writes entries of the tree under root to w as a tar archive in
// PAX format, compressed with zstd, reading each file as it goes: its
// header is what the open file says, and a file that's gone, has changed
// kind, or can't be read whole fails it. Every file is opened from root
// one component at a time, following no symlink, and never waiting on a
// named pipe in its place.
func Write(w io.Writer, root *os.Root, entries []Entry) error {
	base, err := root.Open(".")
	if err != nil {
		return err
	}
	defer base.Close()
	zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(Level)),
		zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(window))
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(zw, 256<<10)
	tw := tar.NewWriter(bw)
	buf := make([]byte, copyBuffer)
	for _, e := range entries {
		if err := writeEntry(tw, base, e, buf); err != nil {
			zw.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		zw.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}

func header(e Entry) *tar.Header {
	h := &tar.Header{
		Name:    e.Rel,
		Mode:    tarMode(e.Mode),
		Uid:     e.UID,
		Gid:     e.GID,
		ModTime: e.ModTime,
		Format:  tar.FormatPAX,
	}
	switch e.Kind {
	case Dir:
		h.Typeflag = tar.TypeDir
		h.Name += "/"
	case File:
		h.Typeflag = tar.TypeReg
		h.Size = e.Size
	case Symlink:
		h.Typeflag = tar.TypeSymlink
	}
	return h
}

func writeEntry(tw *tar.Writer, base *os.File, e Entry, buf []byte) error {
	switch e.Kind {
	case Dir:
		return tw.WriteHeader(header(e))
	case Symlink:
		target, err := readlinkIn(base, e.Rel)
		if err != nil {
			return fmt.Errorf("the symlink %s: %w", e.Rel, err)
		}
		h := header(e)
		h.Linkname = target
		return tw.WriteHeader(h)
	}
	f, err := openFile(base, e.Rel)
	if err != nil {
		return fmt.Errorf("the file %s: %w", e.Rel, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("the file %s: %w", e.Rel, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a regular file any more", e.Rel)
	}
	now := entryOf(e.Rel, File, info)
	if err := tw.WriteHeader(header(now)); err != nil {
		return err
	}
	fr := &fileReader{f: f}
	n, err := io.CopyBuffer(tw, io.LimitReader(fr, now.Size), buf)
	if fr.err != nil {
		return fmt.Errorf("reading the file %s: %w", e.Rel, fr.err)
	}
	if err != nil {
		return err
	}
	if n != now.Size {
		return fmt.Errorf("the file %s got shorter while it was saved", e.Rel)
	}
	return nil
}

// fileReader keeps a file's read error apart from the archive's write errors.
type fileReader struct {
	f   *os.File
	err error
}

func (r *fileReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

// tarMode is a mode as tar has it.
func tarMode(m fs.FileMode) int64 {
	v := int64(m.Perm())
	if m&fs.ModeSetuid != 0 {
		v |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		v |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		v |= 0o1000
	}
	return v
}

// fileMode is a tar mode as Go has it.
func fileMode(v int64) fs.FileMode {
	m := fs.FileMode(v & 0o777)
	if v&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if v&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if v&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// CleanRel checks that p is a path under a tree: relative, with slashes,
// and none of its components empty, . or ..; "." is the root itself. A
// trailing slash is dropped.
func CleanRel(p string) (string, error) {
	p = strings.TrimSuffix(p, "/")
	if p == "." {
		return ".", nil
	}
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%q isn't a relative path", p)
	}
	for part := range strings.SplitSeq(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%q isn't a plain relative path", p)
		}
	}
	return path.Clean(p), nil
}

// statInfo is what an lstat in an open directory says of an entry.
type statInfo struct {
	name     string
	mode     fs.FileMode
	size     int64
	mtime    time.Time
	uid, gid int
}

func (s *statInfo) Name() string       { return s.name }
func (s *statInfo) Size() int64        { return s.size }
func (s *statInfo) Mode() fs.FileMode  { return s.mode }
func (s *statInfo) ModTime() time.Time { return s.mtime }
func (s *statInfo) IsDir() bool        { return s.mode.IsDir() }
func (s *statInfo) Sys() any           { return nil }

// modeOf is a Unix mode as Go has it.
func modeOf(m uint32) fs.FileMode {
	mode := fileMode(int64(m & 0o7777))
	switch m & 0o170000 {
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	case 0o100000:
	case 0o010000:
		mode |= fs.ModeNamedPipe
	case 0o140000:
		mode |= fs.ModeSocket
	case 0o020000:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case 0o060000:
		mode |= fs.ModeDevice
	default:
		mode |= fs.ModeIrregular
	}
	return mode
}
