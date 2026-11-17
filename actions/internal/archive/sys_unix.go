//go:build linux || darwin

package archive

import (
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// owner is a file's numeric owner.
func owner(info fs.FileInfo) (uid, gid int) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return 0, 0
}

func openDirAt(fd int, name string) (int, error) {
	return unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// resolveDir opens the directory rel from base, one component at a time,
// following no symlink.
func resolveDir(base *os.File, rel string) (int, error) {
	fd, err := unix.Dup(int(base.Fd()))
	if err != nil {
		return -1, err
	}
	if rel == "." {
		return fd, nil
	}
	for comp := range strings.SplitSeq(rel, "/") {
		next, err := openDirAt(fd, comp)
		unix.Close(fd)
		if err != nil {
			return -1, &fs.PathError{Op: "open", Path: rel, Err: err}
		}
		fd = next
	}
	return fd, nil
}

// openDir opens the directory rel of base to list it, failing if it, or a
// directory it's in, is anything else, a symlink included.
func openDir(base *os.File, rel string) (*os.File, error) {
	fd, err := resolveDir(base, rel)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// openFile opens the file rel of base to read, failing if it, or a
// directory it's in, is a symlink, and without waiting if it's become a
// named pipe: the caller checks what it opened.
func openFile(base *os.File, rel string) (*os.File, error) {
	dir, name := split(rel)
	pfd, err := resolveDir(base, dir)
	if err != nil {
		return nil, err
	}
	defer unix.Close(pfd)
	fd, err := unix.Openat(pfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: rel, Err: err}
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// readlinkIn is the target of the symlink rel of base, reached as
// openFile reaches a file.
func readlinkIn(base *os.File, rel string) (string, error) {
	dir, name := split(rel)
	pfd, err := resolveDir(base, dir)
	if err != nil {
		return "", err
	}
	defer unix.Close(pfd)
	for size := 256; ; size *= 2 {
		b := make([]byte, size)
		n, err := unix.Readlinkat(pfd, name, b)
		if err != nil {
			return "", &fs.PathError{Op: "readlink", Path: rel, Err: err}
		}
		if n < size {
			return string(b[:n]), nil
		}
	}
}

// lstatIn is what the open directory dir has as name, itself.
func lstatIn(dir *os.File, name string) (fs.FileInfo, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: err}
	}
	return &statInfo{name: name, mode: modeOf(uint32(st.Mode)), size: st.Size, mtime: mtimeOf(&st), uid: int(st.Uid), gid: int(st.Gid)}, nil
}

// lchtimes sets the modification time of rel in root, itself, not what it
// links to. Its access time, which caches don't keep, is set the same.
func lchtimes(root *os.Root, rel string, mtime time.Time) error {
	dir, base := path.Split(rel)
	if dir == "" {
		dir = "."
	}
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := unix.UtimesNanoAt(int(d.Fd()), base, timespecs(mtime), unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "lutimes", Path: rel, Err: err}
	}
	return nil
}

func timespecs(t time.Time) []unix.Timespec {
	// From the seconds and nanoseconds, not UnixNano, which ends in 2262.
	ts, err := unix.TimeToTimespec(t)
	if err != nil {
		// Out of a 32-bit system's range: as near as it has.
		ts = unix.NsecToTimespec(t.UnixNano())
	}
	return []unix.Timespec{ts, ts}
}

func mtimeOf(st *unix.Stat_t) time.Time { return time.Unix(st.Mtim.Unix()) }
