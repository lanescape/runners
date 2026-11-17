package archive

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// fileTimes sets the modification time of the open file fd, by its
// descriptor; its access time, which caches don't keep, is set the same.
func fileTimes(fd, _ int, _ string, t time.Time) error {
	return unix.UtimesNanoAt(fd, "", timespecs(t), unix.AT_EMPTY_PATH)
}

// dirTimes sets the open directory fd's times, by its descriptor.
func dirTimes(fd int, t time.Time) error {
	return unix.UtimesNanoAt(fd, "", timespecs(t), unix.AT_EMPTY_PATH)
}

// linkMeta gives the symlink name in the directory pfd its owner, if
// chown is set, and its time, through a descriptor of the symlink itself,
// so that nothing put in its place since is changed instead.
func linkMeta(pfd int, name string, chown bool, uid, gid int, t time.Time) error {
	fd, err := unix.Openat(pfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return errors.New("something else took the symlink's place")
	}
	if chown {
		if err := unix.Fchownat(fd, "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
			return err
		}
	}
	return unix.UtimesNanoAt(fd, "", timespecs(t), unix.AT_EMPTY_PATH)
}
