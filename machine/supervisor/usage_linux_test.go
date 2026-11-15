package main

import (
	"testing"
	"time"
)

// The boot disk's filesystem is measured, and /work only once another
// filesystem is mounted there: until then it's a directory on the boot
// disk, which would count it twice.
func TestFilesystems(t *testing.T) {
	root := t.TempDir()
	m := newMeter(t.TempDir(), root, t.TempDir(), time.Hour)
	m.start()
	u := m.finish()
	if u.RootSize <= 0 || u.RootPeak <= 0 || u.RootPeak > u.RootSize {
		t.Fatalf("root size %d, peak %d", u.RootSize, u.RootPeak)
	}
	if u.WorkSize != 0 || u.WorkPeak != 0 {
		t.Fatalf("work size %d, peak %d, from a directory on the root's filesystem", u.WorkSize, u.WorkPeak)
	}
	// /dev/shm is another filesystem, as a mounted work disk is.
	m = newMeter(t.TempDir(), root, "/dev/shm", time.Hour)
	m.start()
	if u := m.finish(); u.WorkSize <= 0 {
		t.Fatalf("work size %d with a filesystem of its own", u.WorkSize)
	}
}
