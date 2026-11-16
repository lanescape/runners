package archive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
)

// Restorer extracts a cache's archives under a directory, treating them
// as untrusted: every name must be a plain relative path, and each entry
// is reached from the directory one component at a time, through
// descriptors that never follow a symlink, so nothing an archive holds,
// nor anything the tree becomes while it's extracted, takes an entry
// outside the directory or through a symlink. Archives of files may be
// extracted at once; the directories' archive goes last.
type Restorer struct {
	// base is the directory, open, and baseFD its descriptor.
	base   *os.File
	baseFD int
	// chown is whether to give entries their saved owners.
	chown bool
	// writable are the directories made writable and searchable by their
	// owner, which needn't be checked again.
	writable sync.Map

	files, bytes atomic.Int64
}

// NewRestorer extracts under root, giving entries their saved owners if
// chown is set. Close it when done.
func NewRestorer(root *os.Root, chown bool) (*Restorer, error) {
	base, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	return &Restorer{base: base, baseFD: int(base.Fd()), chown: chown}, nil
}

// Close closes the directory.
func (x *Restorer) Close() error { return x.base.Close() }

// Counts are the files and symlinks extracted so far, and the files'
// bytes.
func (x *Restorer) Counts() (entries, bytes int64) { return x.files.Load(), x.bytes.Load() }

func reader(r io.Reader) (*tar.Reader, func(), error) {
	zr, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(maxWindow))
	if err != nil {
		return nil, nil, err
	}
	return tar.NewReader(zr), zr.Close, nil
}

// Files extracts an archive of files and symlinks, overwriting what's
// there, and making the directories they're in as they're needed.
func (x *Restorer) Files(r io.Reader) error {
	tr, done, err := reader(r)
	if err != nil {
		return err
	}
	defer done()
	buf := make([]byte, copyBuffer)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		rel, err := CleanRel(h.Name)
		if err != nil || rel == "." {
			return fmt.Errorf("refusing the archive's entry %q: its name isn't a plain path under the cache's directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeReg:
			err = x.file(rel, h, tr, buf)
		case tar.TypeSymlink:
			if h.Linkname == "" || strings.ContainsRune(h.Linkname, 0) {
				err = errors.New("the symlink has no target")
			} else {
				err = x.symlink(rel, h)
			}
		default:
			return fmt.Errorf("refusing the archive's entry %s: an archive of files has only regular files and symlinks", rel)
		}
		if err != nil {
			return fmt.Errorf("restoring %s: %w", rel, err)
		}
		x.files.Add(1)
		if h.Typeflag == tar.TypeReg {
			x.bytes.Add(h.Size)
		}
	}
}

// onlyWriter hides a file's ReadFrom, so the copy uses the buffer it's
// given.
type onlyWriter struct{ w io.Writer }

func (o onlyWriter) Write(p []byte) (int, error) { return o.w.Write(p) }

// Dirs extracts the directories' archive, once every archive of files
// is extracted: it makes each directory that isn't there, in the
// archive's order, and then, deepest first, gives each its owner, mode
// and modification time, so that nothing changes a directory after its
// time is set.
func (x *Restorer) Dirs(r io.Reader) (int, error) {
	tr, done, err := reader(r)
	if err != nil {
		return 0, err
	}
	defer done()
	var dirs []*tar.Header
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("reading the directories' archive: %w", err)
		}
		rel, err := CleanRel(h.Name)
		if err != nil {
			return 0, fmt.Errorf("refusing the archive's entry %q: its name isn't a plain path under the cache's directory", h.Name)
		}
		if h.Typeflag != tar.TypeDir {
			return 0, fmt.Errorf("refusing the archive's entry %s: the directories' archive has only directories", rel)
		}
		h.Name = rel
		d, err := x.dir(rel, true)
		if err != nil {
			return 0, fmt.Errorf("restoring the directory %s: %w", rel, err)
		}
		d.Close()
		dirs = append(dirs, h)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := x.finishDir(dirs[i]); err != nil {
			return 0, fmt.Errorf("restoring the directory %s: %w", dirs[i].Name, err)
		}
	}
	return len(dirs), nil
}

// split is the directory an entry is in, and its own name.
func split(rel string) (dir, base string) {
	return path.Dir(rel), path.Base(rel)
}
