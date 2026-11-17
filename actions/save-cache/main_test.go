//go:build linux || darwin

package main

// emulators/fault gives the service emulator's answers to /v1/caches/
// here, so that each case answers as it needs: a token about to expire,
// another version, a commit that differs. Uploads go to its Cloud Storage
// emulator. The end-to-end tests run the action against the service
// emulator's own answers.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/service"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/runners/actions/internal/actions"
	"github.com/lanescape/runners/actions/internal/archive"
	"github.com/lanescape/runners/actions/internal/backoff"
	"github.com/lanescape/runners/actions/internal/gcs"
	"github.com/lanescape/runners/actions/internal/metadata"
	svcclient "github.com/lanescape/runners/actions/internal/service"
	"github.com/lanescape/runners/actions/internal/testgithub"
)

type on bool

func (o on) OnService(context.Context) bool { return bool(o) }

const (
	token  = "ya29.cache-create-token"
	bucket = "lanescape-runners-caches"
	prefix = "1234/v1/"
)

type world struct {
	gh   *testgithub.GitHub
	svc  *service.Emulator
	emu  *gcsemu.Emulator
	repo *ghemu.Repo
	// saves and commits count the requests to save and to commit.
	saves, commits atomic.Int32
	// starts counts the uploads started.
	starts atomic.Int32
}

func newWorld(t *testing.T) *world {
	gh := testgithub.New(t)
	svc := service.New(service.WithGitHub(gh.Emulator))
	t.Cleanup(svc.Close)
	emu := gcsemu.New()
	t.Cleanup(emu.Close)
	emu.CreateBucket(bucket)
	repo := gh.Repo(gh.Account(endpoints.Organization), gh.NewID(), "app")
	repo.Commit("main", map[string]string{".github/runners.json": `{"caches": {"build": {}}}`})
	w := &world{gh: gh, svc: svc, emu: emu, repo: repo}
	svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/save", Match: func(*http.Request) bool { w.saves.Add(1); return false }})
	svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/commit", Match: func(*http.Request) bool { w.commits.Add(1); return false }})
	emu.Faults().Add(fault.Rule{PathPrefix: "/upload/", Match: func(*http.Request) bool { w.starts.Add(1); return false }})
	return w
}

type saveAnswer struct {
	Saves   bool   `json:"saves"`
	Reason  string `json:"reason,omitempty"`
	Bucket  string `json:"bucket,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
	Token   string `json:"token,omitempty"`
	Expires string `json:"expires,omitempty"`
}

func saves(expires time.Duration) saveAnswer {
	return saveAnswer{Saves: true, Bucket: bucket, Prefix: prefix, Token: token, Expires: time.Now().Add(expires).UTC().Format(time.RFC3339)}
}

func (w *world) answer(path string, v any, times int) {
	b, _ := json.Marshal(v)
	w.svc.Faults().Add(fault.Rule{PathPrefix: path, Times: times, Respond: fault.Status(http.StatusOK, nil, string(b))})
}

// step is the action's step in a job of run on one of the service's
// machines, saving dir as the cache build.
func (w *world) step(t *testing.T, run *ghemu.Run, inputs map[string]string) (*actions.Runtime, map[string]string, *bytes.Buffer, *svcclient.Client, *gcs.Client) {
	t.Helper()
	j, err := w.svc.StartJob(w.svc.RunOf(run), "lanescape-2vcpu-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	env := w.gh.Env(testgithub.Job{Run: run, Inputs: inputs})
	var log bytes.Buffer
	rt := &actions.Runtime{Getenv: func(k string) string { return env[k] }, Stdout: &log}
	c := svcclient.At(w.svc.URL(), metadata.Server{URL: w.svc.Metadata(j)})
	c.Retry = backoff.Policy{Attempts: 3, First: time.Millisecond, Max: time.Millisecond}
	st := &gcs.Client{URL: w.emu.URL, ChunkSize: 256 << 10, Retry: backoff.Policy{Attempts: 3, First: time.Millisecond, Max: time.Millisecond}}
	return rt, env, &log, c, st
}

func source(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	write := func(rel string, data []byte, mode fs.FileMode) {
		p := filepath.Join(src, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("a/one", []byte("one"), 0o644)
	write("a/run", []byte("#!/bin/sh\n"), 0o755)
	write("a-b", []byte("sibling"), 0o600)
	write("big", bytes.Repeat([]byte("0123456789abcdef"), 100_000), 0o644)
	write("src/out/obj", []byte("left out"), 0o644)
	os.MkdirAll(filepath.Join(src, "nested/empty"), 0o700)
	os.Symlink("../a-b", filepath.Join(src, "a/link"))
	mt := time.Unix(1_700_000_000, 987_654_321)
	for _, p := range []string{"a/one", "nested/empty", "a"} {
		os.Chtimes(filepath.Join(src, p), mt, mt)
	}
	return src
}

func inputs(dir string) map[string]string {
	return map[string]string{"name": "build", "path": dir, "exclude": "src/out\n\n  ./nothing/here  \n"}
}

func TestSave(t *testing.T) {
	w := newWorld(t)
	src := source(t)
	w.answer("/v1/caches/save", saves(time.Hour), 0)
	w.recordCommits()
	rt, env, log, c, st := w.step(t, w.gh.PushRun(w.repo, "main"), inputs(src))
	if err := run(context.Background(), rt, on(true), c, st); err != nil {
		t.Fatal(err)
	}
	names := w.emu.Names(bucket)
	if !slices.Contains(names, prefix+"dirs.tar.zst") || len(names) != 6 {
		t.Fatalf("objects %v: the directories' and one for each of the 5 files and symlinks", names)
	}
	// What was saved restores to the tree, without what was left out.
	dst := t.TempDir()
	root, _ := os.OpenRoot(dst)
	defer root.Close()
	x, err := archive.NewRestorer(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	for _, name := range names {
		if name == prefix+"dirs.tar.zst" {
			continue
		}
		b, _, _ := w.emu.Object(bucket, name)
		if err := x.Files(bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	b, _, _ := w.emu.Object(bucket, prefix+"dirs.tar.zst")
	if _, err := x.Dirs(bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	// The source as it would be without what was left out.
	info, _ := os.Lstat(filepath.Join(src, "src"))
	os.RemoveAll(filepath.Join(src, "src/out"))
	os.Chtimes(filepath.Join(src, "src"), info.ModTime(), info.ModTime())
	if want, got := scan(t, src), scan(t, dst); !maps(want, got) {
		t.Fatalf("restored %v, saved %v", got, want)
	}
	if w.commits.Load() != 1 || !strings.Contains(testgithub.Summary(t, env), "as 6 objects") {
		t.Fatalf("committed %d times; summary %s", w.commits.Load(), testgithub.Summary(t, env))
	}
	if strings.Count(log.String(), token) != 1 || !strings.Contains(log.String(), "::add-mask::"+token) {
		t.Fatalf("the token isn't masked before anything else:\n%s", log)
	}
}

// A commit that doesn't record what was uploaded fails the save, so the
// job fails, and the service doesn't keep the version.
func TestACommitThatDiffers(t *testing.T) {
	w := newWorld(t)
	w.answer("/v1/caches/save", saves(time.Hour), 0)
	w.answer("/v1/caches/commit", map[string]int{"objects": 1, "bytes": 10}, 0)
	rt, _, _, c, st := w.step(t, w.gh.PushRun(w.repo, "main"), inputs(source(t)))
	if err := run(context.Background(), rt, on(true), c, st); err == nil || !strings.Contains(err.Error(), "committed 1 objects of 10 bytes") {
		t.Fatalf("err = %v", err)
	}
}

func TestATokenAboutToExpire(t *testing.T) {
	w := newWorld(t)
	w.answer("/v1/caches/save", saves(30*time.Second), 1)
	again := saves(time.Hour)
	again.Token = "ya29.fresh"
	w.answer("/v1/caches/save", again, 0)
	w.recordCommits()
	var auth []string
	var mu sync.Mutex
	w.emu.Faults().Add(fault.Rule{PathPrefix: "/upload/", Method: http.MethodPost, Match: func(r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Query().Get("upload_id") == "" {
			auth = append(auth, r.Header.Get("Authorization"))
		}
		return false
	}})
	rt, _, log, c, st := w.step(t, w.gh.PushRun(w.repo, "main"), inputs(source(t)))
	if err := run(context.Background(), rt, on(true), c, st); err != nil {
		t.Fatal(err)
	}
	if w.saves.Load() != 2 || !strings.Contains(log.String(), "::add-mask::ya29.fresh") {
		t.Fatalf("asked %d times:\n%s", w.saves.Load(), log)
	}
	// Every upload started with the new token.
	if len(auth) != 6 {
		t.Fatalf("%d uploads started", len(auth))
	}
	for _, a := range auth {
		if a != "Bearer ya29.fresh" {
			t.Fatalf("an upload started with %q", a)
		}
	}
}

// Asked again for a token, the service must give the same version.
func TestAnotherVersion(t *testing.T) {
	w := newWorld(t)
	w.answer("/v1/caches/save", saves(30*time.Second), 1)
	again := saves(time.Hour)
	again.Prefix = "1234/v2/"
	w.answer("/v1/caches/save", again, 0)
	rt, _, _, c, st := w.step(t, w.gh.PushRun(w.repo, "main"), inputs(source(t)))
	err := run(context.Background(), rt, on(true), c, st)
	if err == nil || !strings.Contains(err.Error(), "gave another version of the cache build to save") || w.saves.Load() != 2 || w.commits.Load() != 0 {
		t.Fatalf("err = %v, after %d saves and %d commits", err, w.saves.Load(), w.commits.Load())
	}
}

func TestTheScopeSavesNothing(t *testing.T) {
	w := newWorld(t)
	w.answer("/v1/caches/save", saveAnswer{Reason: "the run's scope, read, saves nothing"}, 0)
	rt, env, _, c, st := w.step(t, w.gh.PushRun(w.repo, "main"), inputs(source(t)))
	if err := run(context.Background(), rt, on(true), c, st); err != nil {
		t.Fatal(err)
	}
	if s := testgithub.Summary(t, env); !strings.Contains(s, "so the cache wasn't saved: the run's scope, read, saves nothing") ||
		w.starts.Load() != 0 || w.commits.Load() != 0 {
		t.Fatalf("summary = %s, after %d uploads and %d commits", s, w.starts.Load(), w.commits.Load())
	}
}

func TestOutsideOrOffTheService(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.gh.User(9001, "contributor"), w.gh.NewID())
	for name, c := range map[string]struct {
		run  *ghemu.Run
		on   bool
		says string
	}{
		"a fork's own run":          {w.gh.PushRun(fork, "main"), true, "Outside the lanescape organization"},
		"not the service's machine": {w.gh.PushRun(w.repo, "main"), false, "isn't one of the runner service's machines"},
	} {
		t.Run(name, func(t *testing.T) {
			rt, _, _, svc, st := w.step(t, w.gh.PushRun(w.repo, "main"), nil)
			env := w.gh.Env(testgithub.Job{Run: c.run, Inputs: inputs(t.TempDir())})
			rt.Getenv = func(k string) string { return env[k] }
			if err := run(context.Background(), rt, on(c.on), svc, st); err != nil {
				t.Fatal(err)
			}
			if s := testgithub.Summary(t, env); !strings.Contains(s, c.says) || w.saves.Load() != 0 {
				t.Fatalf("summary = %s, asked %d", s, w.saves.Load())
			}
		})
	}
}

func TestFailures(t *testing.T) {
	unreadable := func(t *testing.T) string {
		src := source(t)
		os.Chmod(filepath.Join(src, "nested"), 0)
		t.Cleanup(func() { os.Chmod(filepath.Join(src, "nested"), 0o755) })
		if f, err := os.Open(filepath.Join(src, "nested")); err == nil {
			f.Close()
			t.Skip("this process reads every directory")
		}
		return src
	}
	for name, c := range map[string]struct {
		src    func(*testing.T) string
		faults func(*world)
		says   string
		asks   int32
	}{
		"a refusal": {src: source, faults: func(w *world) {
			w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/save", Respond: fault.Refuse("the run's commit doesn't declare the cache build")})
		}, says: "refused to save the cache build: the run's commit doesn't declare", asks: 1},
		"the service unavailable": {src: source, faults: func(w *world) {
			w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/save", Respond: fault.Unavailable(time.Second)})
		}, says: "couldn't answer, and the retries are spent", asks: 3},
		"a directory it can't read": {src: unreadable, faults: func(w *world) { w.answer("/v1/caches/save", saves(time.Hour), 0) },
			says: "reading the directory nested", asks: 1},
		"a path that isn't a directory": {src: func(t *testing.T) string { return filepath.Join(source(t), "a/one") },
			says: "isn't a directory"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if c.faults != nil {
				c.faults(w)
			}
			rt, _, _, svc, st := w.step(t, w.gh.PushRun(w.repo, "main"), inputs(c.src(t)))
			err := run(context.Background(), rt, on(true), svc, st)
			if err == nil || !strings.Contains(err.Error(), c.says) || w.saves.Load() != c.asks || w.starts.Load() != 0 || w.commits.Load() != 0 {
				t.Fatalf("err = %v, after %d saves, %d uploads and %d commits", err, w.saves.Load(), w.starts.Load(), w.commits.Load())
			}
		})
	}
}

func TestInputs(t *testing.T) {
	w := newWorld(t)
	for _, in := range []map[string]string{{"name": "Build", "path": "/tmp/x"}, {"name": "build"},
		{"name": "build", "path": "/tmp/x", "exclude": "../up"}, {"name": "build", "path": "/tmp/x", "exclude": "/abs"}} {
		rt, _, _, svc, st := w.step(t, w.gh.PushRun(w.repo, "main"), nil)
		env := w.gh.Env(testgithub.Job{Run: w.gh.PushRun(w.repo, "main"), Inputs: in})
		rt.Getenv = func(k string) string { return env[k] }
		if err := run(context.Background(), rt, on(true), svc, st); err == nil || !strings.Contains(err.Error(), "input") || w.saves.Load() != 0 {
			t.Errorf("%v: %v", in, err)
		}
	}
}

// scan is each entry's kind, mode, mtime, target and contents.
func scan(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		info, _ := os.Lstat(p)
		s := info.Mode().Type().String() + " " + info.ModTime().Format(time.RFC3339Nano)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			s += " -> " + target
		case info.Mode().IsRegular():
			f, _ := os.Open(p)
			b, _ := io.ReadAll(f)
			f.Close()
			s += " " + info.Mode().Perm().String() + " " + string(b)
		default:
			s += " " + info.Mode().Perm().String()
		}
		out[rel] = s
		return nil
	})
	return out
}

func maps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// recordCommits answers each commit with what's uploaded under the prefix, as
// the service records it.
func (w *world) recordCommits() {
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/commit", Respond: func(rw http.ResponseWriter, r *http.Request) {
		n, size := 0, 0
		for _, name := range w.emu.Names(bucket) {
			if b, _, _ := w.emu.Object(bucket, name); strings.HasPrefix(name, prefix) {
				n, size = n+1, size+len(b)
			}
		}
		b, _ := json.Marshal(map[string]int{"objects": n, "bytes": size})
		fault.Status(http.StatusOK, nil, string(b))(rw, r)
	}})
}
