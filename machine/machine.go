// Package machine holds what the service puts on its base images, embedded
// in the service's binary so that nothing in a consumer's repository can
// change what a machine does before its job starts or after it ends
// (specs/0004-deployment.md): the supervisor's source, which uses only Go's
// standard library and builds from these files alone, and the job-started
// hook.
package machine

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"slices"
)

//go:embed job-started.sh supervisor/main.go
var files embed.FS

// The paths the base image gives them.
const (
	HookPath       = "/usr/local/lib/lanescape-runners/job-started.sh"
	SupervisorPath = "/usr/local/lib/lanescape-runners/supervisor"
	// SaveMarker is where the save action asks for the job's work disk
	// to be saved, on the boot disk, for the supervisor to read once the
	// runner has exited (specs/0005-actions.md).
	SaveMarker = "/var/lib/lanescape-runners/save"
)

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
