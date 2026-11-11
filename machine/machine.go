// Package machine holds what the service puts on its base images, embedded
// in the service's binary so that nothing in a consumer's repository can
// change what a machine does before its job starts or after it ends
// (specs/0004-deployment.md): the sources of the service's three programs,
// the supervisor, the job-started hook's program and the program that
// mounts the work disk, each using only Go's standard library and building
// from its own files alone, as a statically linked binary; the job-started
// hook's script, which only runs its program; and the units and rule that
// start the others (specs/0002-machines.md, A machine's life); and the
// builder's script, which installs them and everything else a base image
// holds (specs/0003-disks.md, Making a base image).
package machine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

// The programs' sources are named one by one: their tests aren't the
// programs, and import more than the standard library.
//
//go:embed supervisor/main.go hook/main.go mountwork/main.go mountwork/sys_linux.go mountwork/sys_other.go
//go:embed job-started.sh lanescape-runners-supervisor.service lanescape-runners-work-disk.service
//go:embed 90-lanescape-runners-work.rules
var files embed.FS

// buildScript is the builder's script: see BuildScript.
//
//go:embed build-base.sh
var buildScript string

// BuildScript is the base image's builder's startup script
// (specs/0003-disks.md, Making a base image). Its text is part of the base
// images' key.
func BuildScript() string { return buildScript }

// The lines the builder's script writes to the serial console, which the
// service reads: Built, then the architecture and key, as
// "<arch>-<key>"; or Failed, then why.
const (
	ResultPrefix = "lanescape-runners base image: "
	Built        = ResultPrefix + "built "
	Failed       = ResultPrefix + "failed: "
)

// Dir is the service's own directory on the base image, owned by root,
// which holds its programs and the hook's script (specs/0002-machines.md,
// A machine's life).
const Dir = "/opt/lanescape-runners"

// The paths the base image gives them.
const (
	SupervisorPath  = Dir + "/supervisor"
	HookPath        = Dir + "/job-started.sh"
	HookProgramPath = Dir + "/job-started"
	MountPath       = Dir + "/mount-work"

	SupervisorUnitPath = "/etc/systemd/system/lanescape-runners-supervisor.service"
	WorkDiskUnitPath   = "/etc/systemd/system/lanescape-runners-work-disk.service"
	WorkDiskRulePath   = "/etc/udev/rules.d/90-lanescape-runners-work.rules"

	// MkfsPath is the one tool the mount program runs: Debian's
	// mkfs.ext4, from the base image's e2fsprogs, updated with each
	// rebuild of the base image.
	MkfsPath = "/usr/sbin/mkfs.ext4"

	// WorkPath is where a work disk is mounted: until it is, an empty
	// directory of the base image's, owned by root and mode 0755, which
	// the runner's user can't write (specs/0002-machines.md, The work
	// disk).
	WorkPath = "/work"
	// WorkOwner is the runner's user's UID and GID, which own a work
	// disk's root, as on GitHub's hosted runners.
	WorkOwner = "1001:1001"
)

// Program is one of the service's programs: the directory of its source,
// and where the base image puts it, built statically from its embedded
// files alone.
type Program struct {
	Source string
	Path   string
}

// Programs are the service's programs.
func Programs() []Program {
	return []Program{
		{Source: "supervisor", Path: SupervisorPath},
		{Source: "hook", Path: HookProgramPath},
		{Source: "mountwork", Path: MountPath},
	}
}

// Install is where the base image puts one of the embedded files that's
// installed as it is, and its mode.
type Install struct {
	File string
	Path string
	Mode fs.FileMode
}

// Installs are the embedded files the base image installs as they are. The
// supervisor's unit is enabled; the work disk's unit is started only by the
// rule, when the disk appears.
func Installs() []Install {
	return []Install{
		{File: "job-started.sh", Path: HookPath, Mode: 0o755},
		{File: "lanescape-runners-supervisor.service", Path: SupervisorUnitPath, Mode: 0o644},
		{File: "lanescape-runners-work-disk.service", Path: WorkDiskUnitPath, Mode: 0o644},
		{File: "90-lanescape-runners-work.rules", Path: WorkDiskRulePath, Mode: 0o644},
	}
}

// Files are the embedded files, by their paths in this directory.
func Files() map[string][]byte {
	out := map[string][]byte{}
	_ = fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := files.ReadFile(path)
		out[path] = b
		return err
	})
	return out
}

// Digest is the SHA-256 of every embedded file and its path, which the base
// images' key covers, so that a change to either makes a new image
// (specs/0003-disks.md, Base images).
func Digest() string {
	fs := Files()
	paths := make([]string, 0, len(fs))
	for p := range fs {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	h := sha256.New()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(fs[p])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Archive is what the builder installs of the service's own, as a gzipped
// tar (specs/0003-disks.md, Making a base image): each program's sources,
// in src/<source>/; each file installed as it is, in files/<file>; and
// where each goes, one to a line, in programs, "<source> <path>", and in
// installs, "<file> <path> <mode>". It's the same for the same files.
func Archive() ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	fs := Files()
	var programs, installs strings.Builder
	for _, p := range Programs() {
		fmt.Fprintf(&programs, "%s %s\n", p.Source, p.Path)
		var srcs []string
		for path := range fs {
			if strings.HasPrefix(path, p.Source+"/") {
				srcs = append(srcs, path)
			}
		}
		slices.Sort(srcs)
		for _, path := range srcs {
			if err := add("src/"+path, fs[path]); err != nil {
				return nil, err
			}
		}
	}
	for _, in := range Installs() {
		fmt.Fprintf(&installs, "%s %s %o\n", in.File, in.Path, in.Mode.Perm())
		if err := add("files/"+in.File, fs[in.File]); err != nil {
			return nil, err
		}
	}
	if err := add("programs", []byte(programs.String())); err != nil {
		return nil, err
	}
	if err := add("installs", []byte(installs.String())); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
