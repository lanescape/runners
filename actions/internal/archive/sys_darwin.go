package archive

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// macOS has no way to set a file's times to the nanosecond by its
// descriptor that Go reaches, so they're set by name in the directory the
// entry is in, never following a symlink. The runner service's machines,
// where caches are restored, run Linux, which sets them by descriptor.

func fileTimes(_, pfd int, name string, t time.Time) error {
	return unix.UtimesNanoAt(pfd, name, timespecs(t), unix.AT_SYMLINK_NOFOLLOW)
}

func dirTimes(fd int, t time.Time) error {
	return unix.UtimesNanoAt(fd, ".", timespecs(t), 0)
}

func linkMeta(pfd int, name string, chown bool, uid, gid int, t time.Time) error {
	var st unix.Stat_t
	if err := unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return errors.New("something else took the symlink's place")
	}
	if chown {
		if err := unix.Fchownat(pfd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
	}
	return unix.UtimesNanoAt(pfd, name, timespecs(t), unix.AT_SYMLINK_NOFOLLOW)
}
