package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fake records what the program does as root, and fails what a test says.
type fake struct {
	did  []string
	fail map[string]bool
}

func (f *fake) do(op string, args ...any) error {
	f.did = append(f.did, strings.TrimSpace(op+" "+fmt.Sprint(args...)))
	if f.fail[op] {
		return errors.New(op + " failed")
	}
	return nil
}

func (f *fake) Format(device string, uid, gid int) error {
	return f.do("format", fmt.Sprintf("%s %d:%d", device, uid, gid))
}
func (f *fake) Mount(device, target string) error { return f.do("mount", device+" "+target) }
func (f *fake) Bind(source, target string) error  { return f.do("bind", source+" "+target) }
func (f *fake) Unmount(target string) error       { return f.do("unmount", target) }
func (f *fake) Chown(path string, uid, gid int) error {
	return f.do("chown", fmt.Sprintf("%s %d:%d", path, uid, gid))
}

// env is a machine's /work and its work disk: a file of the disk's bytes,
// behind a link as Compute Engine's device name is, and its mountinfo.
type env struct {
	t   *testing.T
	cfg Config
	sys *fake
	// real is the disk's file, which the link resolves to.
	real string
}

func newEnv(t *testing.T, disk []byte) *env {
	dir := t.TempDir()
	e := &env{t: t, sys: &fake{fail: map[string]bool{}}, real: filepath.Join(dir, "nvme0n2")}
	e.cfg = Defaults()
	e.cfg.Device, e.cfg.Mountpoint, e.cfg.Staging = filepath.Join(dir, "google-work"), filepath.Join(dir, "work"), filepath.Join(dir, "staging")
	e.cfg.Mountinfo, e.cfg.Wait = filepath.Join(dir, "mountinfo"), 5*time.Second
	if err := os.WriteFile(e.real, disk, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.real, e.cfg.Device); err != nil {
		t.Fatal(err)
	}
	e.real, _ = filepath.EvalSymlinks(e.real)
	e.mounted()
	return e
}

// mounted writes mountinfo with these mounts, "<source> <target>", after
// the root's.
func (e *env) mounted(lines ...string) {
	info := "22 1 259:1 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p1 rw\n"
	for i, l := range lines {
		src, target, _ := strings.Cut(l, " ")
		target = strings.ReplaceAll(target, " ", `\040`)
		info += fmt.Sprintf("%d 22 259:%d / %s rw,noatime shared:%d - ext4 %s rw,discard\n", 30+i, 2+i, target, 2+i, src)
	}
	if err := os.WriteFile(e.cfg.Mountinfo, []byte(info), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) run() (error, string) {
	var out bytes.Buffer
	err := mountWork(e.cfg, e.sys, &out)
	return err, out.String()
}

// A new disk, all zeros, and a saved disk's ext4 filesystem.
func zeros() []byte { return make([]byte, 2<<20) }
func ext4Disk() []byte {
	b := zeros()
	binary.LittleEndian.PutUint16(b[1024+56:], 0xEF53)
	return b
}

// specs/0002-machines.md, The work disk: a new disk is formatted, its root
// given to the runner's user, out of sight, and only then mounted on
// /work.
func TestTheWorkDiskIsFormattedAndMounted(t *testing.T) {
	e := newEnv(t, zeros())
	if err, out := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := []string{
		"format " + e.cfg.Device + " 1001:1001",
		"mount " + e.cfg.Device + " " + e.cfg.Staging,
		"chown " + e.cfg.Staging + " 1001:1001",
		"bind " + e.cfg.Staging + " " + e.cfg.Mountpoint,
		"unmount " + e.cfg.Staging,
	}
	if !slices.Equal(e.sys.did, want) {
		t.Fatalf("did %q, want %q", e.sys.did, want)
	}
}

// A saved disk keeps its filesystem, and its root, whatever its job made
// of it, is the runner's user's before it's seen at /work.
func TestAWorkDiskWithAFilesystemIsKept(t *testing.T) {
	e := newEnv(t, ext4Disk())
	if err, out := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := []string{
		"mount " + e.cfg.Device + " " + e.cfg.Staging,
		"chown " + e.cfg.Staging + " 1001:1001",
		"bind " + e.cfg.Staging + " " + e.cfg.Mountpoint,
		"unmount " + e.cfg.Staging,
	}
	if !slices.Equal(e.sys.did, want) {
		t.Fatalf("did %q, want %q", e.sys.did, want)
	}
}

// A disk that's neither empty nor ext4 is left alone: neither formatted nor
// mounted.
func TestAWorkDiskWithSomethingElseIsLeftAlone(t *testing.T) {
	b := zeros()
	b[600<<10] = 1
	e := newEnv(t, b)
	if err, _ := e.run(); err == nil || !strings.Contains(err.Error(), "left alone") || len(e.sys.did) != 0 {
		t.Fatalf("%v, did %q", err, e.sys.did)
	}
}

// A run that stopped after staging the disk carries on from there.
func TestAStagedWorkDiskIsPublished(t *testing.T) {
	e := newEnv(t, zeros())
	e.mounted(e.real + " " + e.cfg.Staging)
	if err, out := e.run(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := []string{"chown " + e.cfg.Staging + " 1001:1001", "bind " + e.cfg.Staging + " " + e.cfg.Mountpoint, "unmount " + e.cfg.Staging}
	if !slices.Equal(e.sys.did, want) {
		t.Fatalf("did %q, want %q", e.sys.did, want)
	}
}

// Run again, it mounts nothing, and only repairs the owner.
func TestAWorkDiskAlreadyMountedIsLeftAlone(t *testing.T) {
	e := newEnv(t, ext4Disk())
	e.mounted(e.real + " " + e.cfg.Mountpoint)
	if err, out := e.run(); err != nil || !slices.Equal(e.sys.did, []string{"chown " + e.cfg.Mountpoint + " 1001:1001"}) {
		t.Fatalf("%v, did %q\n%s", err, e.sys.did, out)
	}
}

func TestAWorkDiskThatFailsIsntMounted(t *testing.T) {
	e := newEnv(t, zeros())
	e.sys.fail["format"] = true
	if err, _ := e.run(); err == nil || slices.ContainsFunc(e.sys.did, func(l string) bool { return !strings.HasPrefix(l, "format") }) {
		t.Fatalf("a disk that couldn't be formatted: %v, did %q", err, e.sys.did)
	}
	e = newEnv(t, zeros())
	e.sys.fail["mount"] = true
	if err, _ := e.run(); err == nil || slices.ContainsFunc(e.sys.did, func(l string) bool { return strings.HasPrefix(l, "chown") }) {
		t.Fatalf("a disk that wasn't mounted was given away: %v, did %q", err, e.sys.did)
	}
}

// The rule can start the program as the disk's link appears: it waits for
// it, and gives up if it never comes.
func TestTheWorkDiskIsWaitedFor(t *testing.T) {
	e := newEnv(t, zeros())
	_ = os.Remove(e.cfg.Device)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.Symlink(e.real, e.cfg.Device)
	}()
	if err, out := e.run(); err != nil || len(e.sys.did) != 5 {
		t.Fatalf("%v, did %q\n%s", err, e.sys.did, out)
	}

	e = newEnv(t, zeros())
	_ = os.Remove(e.cfg.Device)
	e.cfg.Wait = 300 * time.Millisecond
	if err, _ := e.run(); err == nil || !strings.Contains(err.Error(), "never appeared") || len(e.sys.did) != 0 {
		t.Fatalf("%v, did %q", err, e.sys.did)
	}
}

// Mountinfo's escapes, such as a space in a mount point, are read back.
func TestMountinfo(t *testing.T) {
	e := newEnv(t, zeros())
	e.mounted("/dev/sdb /a dir")
	ms, err := mounts(e.cfg.Mountinfo)
	if err != nil || len(ms) != 2 || ms[1] != (mount{source: "/dev/sdb", target: "/a dir"}) {
		t.Fatalf("mounts = %+v, %v", ms, err)
	}
}

func TestSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mount-work.conf")
	_ = os.WriteFile(path, []byte("device=/dev/x\nowner=5:6\nwait=3s\nmkfs=/sbin/mkfs.ext4\n"), 0o644)
	cfg := Defaults()
	if err := readSettings(path, &cfg); err != nil || cfg.Device != "/dev/x" || cfg.UID != 5 || cfg.GID != 6 || cfg.Wait != 3*time.Second ||
		cfg.Mkfs != "/sbin/mkfs.ext4" || cfg.Mountpoint != "/work" {
		t.Fatalf("settings = %+v, %v", cfg, err)
	}
	_ = os.WriteFile(path, []byte("owner=runner\n"), 0o644)
	if err := readSettings(path, &cfg); err == nil {
		t.Fatal("an owner that isn't a UID and GID was taken")
	}
	if err := readSettings(filepath.Join(t.TempDir(), "none"), &cfg); err != nil {
		t.Fatalf("no settings file: %v", err)
	}
}
