// Command mount-work mounts the work disk at /work (specs/0002-machines.md,
// The work disk). The base image's rule for the device named work starts
// it, through systemd, in the machine's own mount namespace, outside every
// job, once Compute Engine has attached the disk the job's disk action asked
// for. It formats the disk if it's empty, as a new disk is, and keeps the
// filesystem of one made from a saved disk; it mounts the disk out of sight
// first, at a staging directory, where its root is given to the runner's
// user, as a saved disk's may not be if its job changed it, and only then
// binds it onto /work, so a job never sees it there with another owner. Run
// again, it finds the disk mounted and only repairs its owner.
//
// The one tool it runs is mkfs.ext4, Debian's, at a path its settings name.
// Its settings come from the base image; the only argument, which the base
// image's unit never passes, is another settings file, for tests.
//
// It uses only Go's standard library, so that it builds from its own source
// alone, statically, wherever the base image is made.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the mount program's settings.
type Config struct {
	Device     string
	Mountpoint string
	Staging    string
	UID, GID   int
	// Wait is how long it waits for the device to appear.
	Wait time.Duration
	// Mkfs is mkfs.ext4's path.
	Mkfs string
	// Mountinfo lists what's mounted in the program's mount namespace.
	Mountinfo string
}

// Defaults are the base image's.
func Defaults() Config {
	return Config{Device: "/dev/disk/by-id/google-work", Mountpoint: "/work", Staging: "/run/lanescape-runners/work-disk",
		UID: 1001, GID: 1001, Wait: 120 * time.Second, Mkfs: "/usr/sbin/mkfs.ext4", Mountinfo: "/proc/self/mountinfo"}
}

func main() {
	settings := "/opt/lanescape-runners/mount-work.conf"
	if len(os.Args) > 1 {
		settings = os.Args[1]
	}
	cfg := Defaults()
	if err := readSettings(settings, &cfg); err != nil {
		say(os.Stderr, "settings %s: %v", settings, err)
		os.Exit(1)
	}
	if err := mountWork(cfg, realSystem{mkfs: cfg.Mkfs}, os.Stderr); err != nil {
		say(os.Stderr, "%v", err)
		os.Exit(1)
	}
}

// readSettings reads key=value lines; a missing file leaves the defaults.
func readSettings(path string, cfg *Config) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "device":
			cfg.Device = v
		case "mountpoint":
			cfg.Mountpoint = v
		case "staging":
			cfg.Staging = v
		case "owner":
			u, g, ok := strings.Cut(v, ":")
			uid, err1 := strconv.Atoi(u)
			gid, err2 := strconv.Atoi(g)
			if !ok || err1 != nil || err2 != nil {
				return fmt.Errorf("owner %q isn't <uid>:<gid>", v)
			}
			cfg.UID, cfg.GID = uid, gid
		case "wait":
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("wait: %v", err)
			}
			cfg.Wait = d
		case "mkfs":
			cfg.Mkfs = v
		case "mountinfo":
			cfg.Mountinfo = v
		}
	}
	return sc.Err()
}

func say(out io.Writer, format string, args ...any) {
	fmt.Fprintf(out, "lanescape-runners: "+format+"\n", args...)
}

// system is what the program does as root, apart from reading the disk.
type system interface {
	// Format makes an ext4 filesystem on the device, whose root belongs to
	// uid and gid.
	Format(device string, uid, gid int) error
	// Mount mounts the device's ext4 filesystem at target.
	Mount(device, target string) error
	// Bind binds source onto target.
	Bind(source, target string) error
	Unmount(target string) error
	Chown(path string, uid, gid int) error
}

// realSystem is the machine's.
type realSystem struct{ mkfs string }

func (s realSystem) Format(device string, uid, gid int) error {
	cmd := exec.Command(s.mkfs, "-q", "-m", "0", "-L", "work", "-E", fmt.Sprintf("root_owner=%d:%d", uid, gid), device)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func (realSystem) Chown(path string, uid, gid int) error { return os.Chown(path, uid, gid) }

// mount is one line of mountinfo: what's mounted where.
type mount struct{ source, target string }

// mounts reads mountinfo (proc(5)): the fifth field is the mount point,
// and the second after the separator, the source.
func mounts(path string) ([]mount, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []mount
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		sep := -1
		for i, x := range f {
			if x == "-" && i >= 6 {
				sep = i
				break
			}
		}
		if sep < 0 || len(f) < sep+3 {
			continue
		}
		out = append(out, mount{source: unescape(f[sep+2]), target: unescape(f[4])})
	}
	return out, nil
}

// unescape undoes mountinfo's octal escapes, such as \040 for a space.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// What a disk holds, as far as the program cares.
const (
	empty = iota
	ext4
	other
)

// scanned is how much of the disk's start must be zero for it to count as
// empty: past every filesystem's superblock that might be there.
const scanned = 1 << 20

// contents says whether a disk is empty, as a new disk is, all zeros at
// its start; holds an ext4 filesystem, by its superblock's magic number; or
// holds something else, which the program leaves alone.
func contents(device string) (int, error) {
	f, err := os.Open(device)
	if err != nil {
		return other, err
	}
	defer f.Close()
	buf := make([]byte, scanned)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return other, err
	}
	buf = buf[:n]
	// The superblock is at 1024 bytes; its magic, 0xEF53, 56 bytes in.
	if len(buf) >= 1024+58 && binary.LittleEndian.Uint16(buf[1024+56:]) == 0xEF53 {
		return ext4, nil
	}
	for _, c := range buf {
		if c != 0 {
			return other, nil
		}
	}
	return empty, nil
}

// mountWork does the program's work.
func mountWork(cfg Config, sys system, out io.Writer) error {
	end := time.Now().Add(cfg.Wait)
	for {
		if _, err := os.Stat(cfg.Device); err == nil {
			break
		}
		if time.Now().After(end) {
			return fmt.Errorf("the work disk never appeared at %s", cfg.Device)
		}
		time.Sleep(200 * time.Millisecond)
	}
	real, err := filepath.EvalSymlinks(cfg.Device)
	if err != nil {
		return fmt.Errorf("resolving %s: %v", cfg.Device, err)
	}
	ms, err := mounts(cfg.Mountinfo)
	if err != nil {
		return fmt.Errorf("listing what's mounted: %v", err)
	}
	staged := false
	for _, m := range ms {
		if m.source != real {
			continue
		}
		if m.target == cfg.Mountpoint {
			if err := sys.Chown(cfg.Mountpoint, cfg.UID, cfg.GID); err != nil {
				return fmt.Errorf("giving %s to the runner's user: %v", cfg.Mountpoint, err)
			}
			say(out, "the work disk is already mounted at %s", cfg.Mountpoint)
			return nil
		}
		staged = staged || m.target == cfg.Staging
	}
	if !staged {
		what, err := contents(cfg.Device)
		switch {
		case err != nil:
			return fmt.Errorf("reading the work disk: %v", err)
		case what == other:
			return errors.New("the work disk holds something that isn't an ext4 filesystem, and isn't empty: it's left alone")
		case what == empty:
			if err := sys.Format(cfg.Device, cfg.UID, cfg.GID); err != nil {
				return fmt.Errorf("formatting the work disk: %v", err)
			}
		}
		if err := os.MkdirAll(cfg.Staging, 0o700); err != nil {
			return fmt.Errorf("making %s: %v", cfg.Staging, err)
		}
		if err := sys.Mount(cfg.Device, cfg.Staging); err != nil {
			return fmt.Errorf("mounting the work disk at %s: %v", cfg.Staging, err)
		}
	}
	if err := sys.Chown(cfg.Staging, cfg.UID, cfg.GID); err != nil {
		return fmt.Errorf("giving the work disk to the runner's user: %v", err)
	}
	if err := os.MkdirAll(cfg.Mountpoint, 0o755); err != nil {
		return fmt.Errorf("making %s: %v", cfg.Mountpoint, err)
	}
	if err := sys.Bind(cfg.Staging, cfg.Mountpoint); err != nil {
		return fmt.Errorf("mounting the work disk at %s: %v", cfg.Mountpoint, err)
	}
	if err := sys.Unmount(cfg.Staging); err != nil {
		say(out, "the work disk is still mounted at %s too: %v", cfg.Staging, err)
	}
	say(out, "the work disk is mounted at %s", cfg.Mountpoint)
	return nil
}
