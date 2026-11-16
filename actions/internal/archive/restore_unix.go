//go:build linux || darwin

package archive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// dir opens the directory rel, as dirFD does.
func (x *Restorer) dir(rel string, create bool) (*os.File, error) {
	fd, err := x.dirFD(rel, create)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// dirFD opens the directory rel from the base, one component at a time,
// never following a symlink, and, with create, makes each that's
// missing. Each it opens is made writable and searchable by its owner, so
// that a tree restored before, with directories saved read-only, can be
// restored onto; the directories' archive gives each its saved mode at
// the end.
func (x *Restorer) dirFD(rel string, create bool) (int, error) {
	fd, err := unix.Dup(x.baseFD)
	if err != nil {
		return -1, err
	}
	if err := x.ready(fd, "."); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if rel == "." {
		return fd, nil
	}
	done := ""
	for comp := range strings.SplitSeq(rel, "/") {
		done = path.Join(done, comp)
		next, err := openDirAt(fd, comp)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(fd, comp, 0o755); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = openDirAt(fd, comp)
			}
		}
		unix.Close(fd)
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return -1, fmt.Errorf("%s isn't a directory: something else is in its place", done)
		}
		if err != nil {
			return -1, fmt.Errorf("%s: %w", done, err)
		}
		fd = next
		if err := x.ready(fd, done); err != nil {
			unix.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

// ready makes the open directory rel writable and searchable by its
// owner, once.
func (x *Restorer) ready(fd int, rel string) error {
	if _, ok := x.writable.Load(rel); ok {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%s isn't a directory", rel)
	}
	if st.Mode&0o700 != 0o700 {
		if err := unix.Fchmod(fd, uint32(st.Mode&0o7777|0o700)); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	x.writable.Store(rel, true)
	return nil
}

func (x *Restorer) file(rel string, h *tar.Header, r io.Reader, buf []byte) error {
	dir, base := split(rel)
	pfd, err := x.dirFD(dir, true)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	// O_EXCL never follows a symlink that's there: it fails, and what's
	// there is replaced.
	const flags = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(pfd, base, flags, 0o600)
	if errors.Is(err, unix.EEXIST) {
		if err := x.remove(pfd, rel, base); err != nil {
			return err
		}
		fd, err = unix.Openat(pfd, base, flags, 0o600)
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), rel)
	n, err := io.CopyBuffer(onlyWriter{f}, io.LimitReader(r, h.Size), buf)
	if err == nil && n != h.Size {
		err = errors.New("the archive ends within it")
	}
	if err == nil && x.chown {
		err = unix.Fchown(fd, h.Uid, h.Gid)
	}
	if err == nil {
		// After the owner, which clears the setuid and setgid bits.
		err = unix.Fchmod(fd, uint32(h.Mode&0o7777))
	}
	if err == nil {
		err = fileTimes(fd, pfd, base, h.ModTime)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (x *Restorer) symlink(rel string, h *tar.Header) error {
	dir, base := split(rel)
	pfd, err := x.dirFD(dir, true)
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	err = unix.Symlinkat(h.Linkname, pfd, base)
	if errors.Is(err, unix.EEXIST) {
		if err := x.remove(pfd, rel, base); err != nil {
			return err
		}
		err = unix.Symlinkat(h.Linkname, pfd, base)
	}
	if err != nil {
		return err
	}
	return linkMeta(pfd, base, x.chown, h.Uid, h.Gid, h.ModTime)
}

// remove removes name, rel, from the directory pfd, so that something
// else can take its place: a directory with everything in it.
func (x *Restorer) remove(pfd int, rel, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Unlinkat(pfd, name, 0)
	}
	x.writable.Range(func(k, _ any) bool {
		if s := k.(string); s == rel || strings.HasPrefix(s, rel+"/") {
			x.writable.Delete(k)
		}
		return true
	})
	return removeTree(pfd, name)
}

// removeTree removes the directory name in pfd and everything in it,
// making each directory writable by its owner first, and following no
// symlink.
func removeTree(pfd int, name string) error {
	fd, err := openDirAt(pfd, name)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&0o700 != 0o700 {
		if err := unix.Fchmod(fd, uint32(st.Mode&0o7777|0o700)); err != nil {
			return err
		}
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, n := range names {
		var cst unix.Stat_t
		if err := unix.Fstatat(fd, n, &cst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if cst.Mode&unix.S_IFMT == unix.S_IFDIR {
			err = removeTree(fd, n)
		} else {
			err = unix.Unlinkat(fd, n, 0)
		}
		if err != nil {
			return err
		}
	}
	return unix.Unlinkat(pfd, name, unix.AT_REMOVEDIR)
}

// finishDir gives the directory h.Name its saved owner, modification time
// and mode, by its descriptor.
func (x *Restorer) finishDir(h *tar.Header) error {
	fd, err := x.dirFD(h.Name, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if x.chown {
		if err := unix.Fchown(fd, h.Uid, h.Gid); err != nil {
			return err
		}
	}
	// The time before the mode, which may take away the search a time set
	// by name needs; a mode doesn't change the time.
	if err := dirTimes(fd, h.ModTime); err != nil {
		return err
	}
	return unix.Fchmod(fd, uint32(h.Mode&0o7777))
}
