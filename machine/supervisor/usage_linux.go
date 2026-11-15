package main

import (
	"os"
	"syscall"
)

// fsUse is the size of the filesystem holding path, and how much of it is
// in use, in bytes, as df counts them.
func fsUse(path string) (size, used int64, ok bool) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, 0, false
	}
	bs := int64(s.Bsize)
	return int64(s.Blocks) * bs, int64(s.Blocks-s.Bfree) * bs, true
}

// deviceOf is the device of the filesystem holding path.
func deviceOf(path string) (uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
