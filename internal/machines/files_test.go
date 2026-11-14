package machines_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"weak"

	"github.com/lanescape/runners/internal/consumer"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/machines"
)

// retention watches the files the service reads for a family's key: before
// each file is read, every file read before it must be garbage, and the
// live heap no larger than a few files' worth above where it started.
type retention struct {
	github.API
	mu       sync.Mutex
	read     []weak.Pointer[byte]
	base     uint64
	peak     uint64
	retained int
}

func (r *retention) File(ctx context.Context, repo github.Repository, path, commit string) ([]byte, error) {
	if path == consumer.RunnersFile {
		return r.API.File(ctx, repo, path, commit)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if r.base == 0 {
		r.base = ms.HeapAlloc
	}
	r.peak = max(r.peak, ms.HeapAlloc)
	for _, w := range r.read {
		if w.Value() != nil {
			r.retained++
		}
	}
	b, err := r.API.File(ctx, repo, path, commit)
	if len(b) > 0 {
		r.read = append(r.read, weak.Make(&b[0]))
	}
	return b, err
}

// A family may list many large files: the service hashes each as it reads
// it, and holds no more than one file's contents at a time, so a
// declaration can't make it hold them all (specs/0003-disks.md, Keys).
func TestAFamilysKeyHoldsOneFileAtATime(t *testing.T) {
	const files, size = 24, 2 << 20 // over the contents API's limit, so read as blobs
	e := setup(t)
	commit := map[string]string{}
	var from []string
	var want []consumer.PathContents
	for i := range files {
		p := fmt.Sprintf("deps/%02d.bin", i)
		c := bytes.Repeat([]byte{byte('a' + i)}, size)
		from = append(from, p)
		commit[p] = string(c)
		want = append(want, consumer.PathContents{Path: p, Contents: c})
	}
	decl, _ := json.Marshal(map[string]any{"disks": map[string]any{"big": map[string]any{"from": from}}})
	commit[".github/runners.json"] = string(decl)
	e.repo.Commit("main", commit)
	key := consumer.Key("big", want, true)
	want, commit = nil, nil

	r := &retention{API: e.svc.GitHub}
	e.svc.GitHub = r
	m := e.admit(e.mainRun(), sel2)
	m.diskReady("big")

	rec, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json")
	if !ok || rec["key"] != key {
		t.Fatalf("disk record = %v; want the key %s", rec, key)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.read) != files {
		t.Fatalf("read %d files, want %d", len(r.read), files)
	}
	if r.retained != 0 {
		t.Fatalf("%d files' contents were still held when the next was read", r.retained)
	}
	if grew := int64(r.peak) - int64(r.base); grew > 4*size {
		t.Fatalf("the live heap grew by %d bytes while reading %d files of %d bytes; want no more than a few files' worth", grew, files, size)
	}
}
