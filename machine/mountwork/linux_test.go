//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// loopDisk is a work disk on a loop device: a file of the disk's bytes,
// behind a link as Compute Engine's device name is.
type loopDisk struct {
	t          *testing.T
	file, link string
	dev        string
}

// newLoopDisk makes an empty disk of size bytes, as a new disk is.
func newLoopDisk(t *testing.T, size int64) *loopDisk {
	dir := t.TempDir()
	d := &loopDisk{t: t, file: filepath.Join(dir, "disk"), link: filepath.Join(dir, "google-work")}
	f, err := os.Create(d.file)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	d.attach()
	return d
}

func (d *loopDisk) attach() {
	out, err := exec.Command("losetup", "--find", "--show", d.file).CombinedOutput()
	if err != nil {
		d.t.Fatalf("losetup: %v: %s", err, out)
	}
	d.dev = strings.TrimSpace(string(out))
	_ = os.Remove(d.link)
	if err := os.Symlink(d.dev, d.link); err != nil {
		d.t.Fatal(err)
	}
	dev := d.dev
	d.t.Cleanup(func() { _ = exec.Command("losetup", "-d", dev).Run() })
}

// grow makes the disk larger, as the service makes a disk from a saved one
// at a larger shape's size: the same bytes, then zeros.
func (d *loopDisk) grow(size int64) {
	if out, err := exec.Command("losetup", "-d", d.dev).CombinedOutput(); err != nil {
		d.t.Fatalf("losetup -d: %v: %s", err, out)
	}
	if err := os.Truncate(d.file, size); err != nil {
		d.t.Fatal(err)
	}
	d.attach()
}

// mountOnce runs the program as the base image's unit does, with Debian's
// tools, on a fresh /work, and returns it.
func (d *loopDisk) mountOnce() string {
	dir := d.t.TempDir()
	cfg := Defaults()
	cfg.Device, cfg.Mountpoint, cfg.Staging = d.link, filepath.Join(dir, "work"), filepath.Join(dir, "staging")
	cfg.Mkfs, cfg.Resize2fs = tool(d.t, "mkfs.ext4"), tool(d.t, "resize2fs")
	// Whatever it leaves mounted goes with the test, even if it fails
	// part-way.
	d.t.Cleanup(func() {
		_ = syscall.Unmount(cfg.Mountpoint, 0)
		_ = syscall.Unmount(cfg.Staging, 0)
	})
	var out bytes.Buffer
	if err := mountWork(cfg, realSystem{mkfs: cfg.Mkfs, resize2fs: cfg.Resize2fs}, &out); err != nil {
		d.t.Fatalf("%v\n%s", err, out.String())
	}
	return cfg.Mountpoint
}

func tool(t *testing.T, name string) string {
	for _, dir := range []string{"/usr/sbin", "/sbin"} {
		if p := filepath.Join(dir, name); fileExists(p) {
			return p
		}
	}
	t.Skipf("no %s", name)
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func size(t *testing.T, mountpoint string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountpoint, &st); err != nil {
		t.Fatal(err)
	}
	return int64(st.Blocks) * st.Bsize
}

// specs/0002-machines.md, The work disk: a disk saved at one shape's size
// and restored at a larger one's is mounted at /work with its contents, its
// filesystem grown to the whole disk. This needs root and loop devices, as
// on a Linux machine of one's own; elsewhere it's skipped.
func TestARestoredDiskGrowsToItsDisk(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root, for loop devices and mounts")
	}
	if f, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0); err != nil {
		t.Skipf("no loop devices: %v", err)
	} else {
		f.Close()
	}
	const small, large = 64 << 20, 256 << 20
	d := newLoopDisk(t, small)
	work := d.mountOnce()
	if got := size(t, work); got > small {
		t.Fatalf("a new disk's filesystem is %d bytes, larger than its disk", got)
	}
	if err := os.WriteFile(filepath.Join(work, "saved"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Saved, as the machine stopping unmounts it, then restored onto a larger
	// disk.
	if err := syscall.Unmount(work, 0); err != nil {
		t.Fatal(err)
	}
	d.grow(large)
	work = d.mountOnce()
	if got := size(t, work); got < large*9/10 {
		t.Fatalf("the restored filesystem is %d bytes, on a disk of %d", got, large)
	}
	if b, err := os.ReadFile(filepath.Join(work, "saved")); err != nil || string(b) != "kept" {
		t.Fatalf("the saved disk's contents: %q, %v", b, err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(work, &st); err != nil || st.Uid != 1001 || st.Gid != 1001 {
		t.Fatalf("/work is owned by %d:%d, %v", st.Uid, st.Gid, err)
	}
}
