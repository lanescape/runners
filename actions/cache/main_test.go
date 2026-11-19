//go:build linux || darwin

package main

// emulators/fault gives the service emulator's answers to /v1/caches/
// here, so that each case answers as it needs: a token about to expire,
// another version, an object that's gone or differs. The objects are in
// its Cloud Storage emulator, made as save-cache makes them. The
// end-to-end tests run the action against the service emulator's own
// answers.

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
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

const token = "ya29.cache-read-token"

type world struct {
	gh   *testgithub.GitHub
	svc  *service.Emulator
	emu  *gcsemu.Emulator
	repo *ghemu.Repo
	// asked counts the requests to restore.
	asked atomic.Int32
}

func newWorld(t *testing.T) *world {
	gh := testgithub.New(t)
	svc := service.New(service.WithGitHub(gh.Emulator))
	t.Cleanup(svc.Close)
	emu := gcsemu.New()
	t.Cleanup(emu.Close)
	emu.CreateBucket("lanescape-runners-caches")
	repo := gh.Repo(gh.Account(endpoints.Organization), gh.NewID(), "app")
	repo.Commit("main", map[string]string{".github/runners.json": `{"caches": {"build": {}}}`})
	w := &world{gh: gh, svc: svc, emu: emu, repo: repo}
	svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/restore", Match: func(*http.Request) bool { w.asked.Add(1); return false }})
	return w
}

// step is the action's step in a job of run on one of the service's
// machines, restoring the cache build under dir.
func (w *world) step(t *testing.T, run *ghemu.Run, dir string) (*actions.Runtime, map[string]string, *bytes.Buffer, *svcclient.Client, *gcs.Client) {
	t.Helper()
	j, err := w.svc.StartJob(w.svc.RunOf(run), "lanescape-2vcpu-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	env := w.gh.Env(testgithub.Job{Run: run, Inputs: map[string]string{"name": "build", "path": dir}})
	var log bytes.Buffer
	rt := &actions.Runtime{Getenv: func(k string) string { return env[k] }, Stdout: &log}
	c := svcclient.At(w.svc.URL(), metadata.Server{URL: w.svc.Metadata(j)})
	c.Retry = backoff.Policy{Attempts: 3, First: time.Millisecond, Max: time.Millisecond}
	st := &gcs.Client{URL: w.emu.URL, Retry: backoff.Policy{Attempts: 3, First: time.Millisecond, Max: time.Millisecond}}
	return rt, env, &log, c, st
}

type version struct {
	Restored string `json:"restored"`
	Bucket   string `json:"bucket,omitempty"`
	Objects  []obj  `json:"objects,omitempty"`
	Token    string `json:"token,omitempty"`
	Expires  string `json:"expires,omitempty"`
}

type obj struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	CRC32C     string `json:"crc32c"`
	Generation int64  `json:"generation"`
}

// saved puts a version of the tree under src in Cloud Storage, as
// save-cache makes it, and returns the service's answer for it.
func (w *world) saved(t *testing.T, src string) version {
	t.Helper()
	r, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tr, err := archive.Walk(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := version{Restored: "main_exact", Bucket: "lanescape-runners-caches", Token: token,
		Expires: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	put := func(name string, entries []archive.Entry) {
		var b bytes.Buffer
		if err := archive.Write(&b, r, entries); err != nil {
			t.Fatal(err)
		}
		full := "1234/v1/" + name
		gen := w.emu.Put(v.Bucket, full, b.Bytes())
		v.Objects = append(v.Objects, obj{Name: full, Size: int64(b.Len()),
			CRC32C: gcs.FormatCRC32C(crc32.Checksum(b.Bytes(), crc32.MakeTable(crc32.Castagnoli))), Generation: gen})
	}
	for i, s := range archive.Shard(tr.Files, 3) {
		put(fmt.Sprintf("shard-%03d.tar.zst", i), s)
	}
	put("dirs.tar.zst", tr.Dirs)
	return v
}

func (w *world) answer(v version, times int) {
	b, _ := json.Marshal(v)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/restore", Times: times, Respond: fault.Status(http.StatusOK, nil, string(b))})
}

func source(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	write := func(rel, data string, mode fs.FileMode) {
		p := filepath.Join(src, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("a/one", "one", 0o644)
	write("a/run", "#!/bin/sh\n", 0o755)
	write("a-b", "sibling", 0o600)
	write("empty", "", 0o644)
	os.MkdirAll(filepath.Join(src, "nested/empty/dirs"), 0o700)
	os.Symlink("../a-b", filepath.Join(src, "a/link"))
	os.Symlink("missing", filepath.Join(src, "dangling"))
	mt := time.Unix(1_700_000_000, 123_456_789)
	for _, p := range []string{"a/one", "nested/empty/dirs", "nested/empty", "a"} {
		os.Chtimes(filepath.Join(src, p), mt, mt)
	}
	return src
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
		s := fmt.Sprintf("%v %d", info.Mode().Type(), info.ModTime().UnixNano())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			s += " -> " + target
		case info.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			s += fmt.Sprintf(" %v %q", info.Mode().Perm(), b)
		default:
			s += fmt.Sprintf(" %v", info.Mode().Perm())
		}
		out[rel] = s
		return nil
	})
	return out
}

func TestRestore(t *testing.T) {
	w := newWorld(t)
	src := source(t)
	w.answer(w.saved(t, src), 0)
	dst := filepath.Join(t.TempDir(), "work", "tree")
	rt, env, log, c, st := w.step(t, w.gh.PushRun(w.repo, "main"), dst)
	if err := run(context.Background(), rt, on(true), c, st); err != nil {
		t.Fatal(err)
	}
	want, got := scan(t, src), scan(t, dst)
	if len(want) != len(got) {
		t.Errorf("restored %d entries, of %d", len(got), len(want))
	}
	for rel, s := range want {
		if got[rel] != s {
			t.Errorf("%s: restored %s, saved %s", rel, got[rel], s)
		}
	}
	if o := testgithub.Outputs(t, env); o["restored"] != "main_exact" {
		t.Errorf("outputs %v", o)
	}
	if s := testgithub.Summary(t, env); !strings.Contains(s, "from main's version for this key (`main_exact`): 6 files and symlinks") {
		t.Errorf("summary = %s", s)
	}
	if strings.Count(log.String(), token) != 1 || !strings.Contains(log.String(), "::add-mask::"+token) {
		t.Errorf("the token isn't masked before anything else:\n%s", log)
	}
	if w.asked.Load() != 1 {
		t.Errorf("asked %d times", w.asked.Load())
	}
}

func TestNothingToRestore(t *testing.T) {
	w := newWorld(t)
	w.answer(version{Restored: "none"}, 0)
	dst := filepath.Join(t.TempDir(), "tree")
	rt, env, _, c, st := w.step(t, w.gh.PushRun(w.repo, "main"), dst)
	if err := run(context.Background(), rt, on(true), c, st); err != nil {
		t.Fatal(err)
	}
	if o := testgithub.Outputs(t, env); o["restored"] != "none" {
		t.Errorf("outputs %v", o)
	}
	if s := testgithub.Summary(t, env); !strings.Contains(s, "There's no version of the cache to restore") {
		t.Errorf("summary = %s", s)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("made the directory")
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
			rt, _, _, svc, st := w.step(t, w.gh.PushRun(w.repo, "main"), t.TempDir())
			env := w.gh.Env(testgithub.Job{Run: c.run, Inputs: map[string]string{"name": "build", "path": t.TempDir()}})
			rt.Getenv = func(k string) string { return env[k] }
			if err := run(context.Background(), rt, on(c.on), svc, st); err != nil {
				t.Fatal(err)
			}
			if s := testgithub.Summary(t, env); !strings.Contains(s, c.says) || w.asked.Load() != 0 {
				t.Fatalf("summary = %s, asked %d", s, w.asked.Load())
			}
			if o := testgithub.Outputs(t, env); o["restored"] != "none" {
				t.Errorf("outputs %v", o)
			}
		})
	}
}

func TestFailures(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*world, *version)
		says   string
		// untouched is whether nothing may be extracted: the version is
		// refused before.
		untouched bool
	}{
		"a CRC32C mismatch": {func(_ *world, v *version) { v.Objects[0].CRC32C = "AAAAAA==" }, "can't be restored: checking shard-000.tar.zst", true},
		"a CRC32C mismatch in the directories' archive": {func(_ *world, v *version) { v.Objects[len(v.Objects)-1].CRC32C = "AAAAAA==" },
			"can't be restored: checking dirs.tar.zst", true},
		"a size mismatch":                   {func(_ *world, v *version) { v.Objects[1].Size++ }, "can't be restored", true},
		"a generation that's gone":          {func(_ *world, v *version) { v.Objects[0].Generation++ }, "isn't there at its generation", true},
		"an object that's gone":             {func(w *world, v *version) { w.emu.Put(v.Bucket, v.Objects[2].Name, []byte("replaced")) }, "isn't there at its generation", true},
		"an object save-cache doesn't make": {func(_ *world, v *version) { v.Objects[0].Name = "1234/v1/other" }, "save-cache doesn't make", true},
		"an object's name that isn't repeated": {func(_ *world, v *version) {
			v.Objects[0].Name = "https://storage.googleapis.com/b/o?X-Goog-Signature=secret"
		}, "has an object save-cache doesn't make", true},
		"no directories' archive": {func(_ *world, v *version) { v.Objects = v.Objects[:len(v.Objects)-1] }, "no directories' archive", true},
		"a refusal": {func(w *world, _ *version) {
			w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/restore", Respond: fault.Refuse("the run's commit doesn't declare the cache build")})
		}, "refused to restore the cache build: the run's commit doesn't declare", true},
		"an escaping entry": {func(w *world, v *version) {
			bad := escaping()
			gen := w.emu.Put(v.Bucket, v.Objects[0].Name, bad)
			v.Objects[0] = obj{Name: v.Objects[0].Name, Size: int64(len(bad)),
				CRC32C: gcs.FormatCRC32C(crc32.Checksum(bad, crc32.MakeTable(crc32.Castagnoli))), Generation: gen}
		}, "isn't a plain path", false},
		"an escaping entry under the service's own part of a name": {func(w *world, v *version) {
			bad := escaping()
			name := "ya29.secret-token/shard-000.tar.zst"
			gen := w.emu.Put(v.Bucket, name, bad)
			v.Objects[0] = obj{Name: name, Size: int64(len(bad)),
				CRC32C: gcs.FormatCRC32C(crc32.Checksum(bad, crc32.MakeTable(crc32.Castagnoli))), Generation: gen}
		}, "restoring shard-000.tar.zst", false},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			v := w.saved(t, source(t))
			c.change(w, &v)
			w.answer(v, 0)
			dst := filepath.Join(t.TempDir(), "tree")
			rt, env, _, svc, st := w.step(t, w.gh.PushRun(w.repo, "main"), dst)
			err := run(context.Background(), rt, on(true), svc, st)
			if err == nil || !strings.Contains(err.Error(), c.says) || strings.Contains(strings.ToLower(err.Error()), "secret") {
				t.Fatalf("err = %v", err)
			}
			if entries, _ := os.ReadDir(dst); c.untouched && len(entries) != 0 {
				t.Errorf("restored part of a version that can't be restored: %v", entries)
			}
			if o := testgithub.Outputs(t, env); o["restored"] != "" {
				t.Errorf("outputs %v", o)
			}
		})
	}
}

// escaping is an archive of files with an entry outside its directory.
func escaping() []byte {
	var b bytes.Buffer
	zw, _ := zstd.NewWriter(&b)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "../escaped", Mode: 0o644, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	zw.Close()
	return b.Bytes()
}

// A token about to expire is asked for again, as is one Cloud Storage
// refuses, and the version must be the same.
func TestNewTokens(t *testing.T) {
	expired := func(tok string) fault.Rule {
		return fault.Rule{Match: func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+tok },
			Respond: gcsemu.JSONError(http.StatusUnauthorized, "authError", "Invalid Credentials")}
	}
	for name, c := range map[string]struct {
		first  func(*version)
		again  func(*version)
		faults func(*world)
		asks   int32
		says   string
	}{
		"one about to expire": {first: func(v *version) { v.Expires = time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339) },
			again: func(v *version) { v.Token = "ya29.fresh" }, asks: 2},
		"one refused": {again: func(v *version) { v.Token = "ya29.fresh" }, faults: func(w *world) { w.emu.Faults().Add(expired(token)) }, asks: 2},
		"the same version, in another order": {first: func(v *version) { v.Expires = time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339) },
			again: func(v *version) { v.Token = "ya29.fresh"; slices.Reverse(v.Objects) }, asks: 2},
		"another version": {first: func(v *version) { v.Expires = time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339) },
			again: func(v *version) { v.Objects[0].Generation++ }, asks: 2, says: "gave another version of the cache build"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			v := w.saved(t, source(t))
			first, again := v, v
			first.Objects, again.Objects = append([]obj(nil), v.Objects...), append([]obj(nil), v.Objects...)
			if c.first != nil {
				c.first(&first)
			}
			c.again(&again)
			w.answer(first, 1)
			w.answer(again, 0)
			if c.faults != nil {
				c.faults(w)
			}
			rt, _, log, svc, st := w.step(t, w.gh.PushRun(w.repo, "main"), filepath.Join(t.TempDir(), "tree"))
			err := run(context.Background(), rt, on(true), svc, st)
			if c.says == "" && err != nil || c.says != "" && (err == nil || !strings.Contains(err.Error(), c.says)) {
				t.Fatalf("err = %v", err)
			}
			if w.asked.Load() != c.asks {
				t.Fatalf("asked %d times", w.asked.Load())
			}
			if c.says == "" && !strings.Contains(log.String(), "::add-mask::ya29.fresh") {
				t.Fatalf("the new token isn't masked:\n%s", log)
			}
		})
	}
}

// The service's 503s are waited out, as every request's are.
func TestTheServiceUnavailable(t *testing.T) {
	w := newWorld(t)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/restore", Times: 2, Respond: fault.Unavailable(time.Second)})
	w.answer(w.saved(t, source(t)), 0)
	rt, env, _, svc, st := w.step(t, w.gh.PushRun(w.repo, "main"), filepath.Join(t.TempDir(), "tree"))
	if err := run(context.Background(), rt, on(true), svc, st); err != nil {
		t.Fatal(err)
	}
	if o := testgithub.Outputs(t, env); o["restored"] != "main_exact" || w.asked.Load() != 3 {
		t.Fatalf("outputs %v after %d requests", o, w.asked.Load())
	}
}

// With check-only, the action asks only where a restore would come from,
// and says so; it restores nothing, needs no path, and makes nothing.
func TestCheckOnly(t *testing.T) {
	for _, found := range []string{"main_exact", "none"} {
		t.Run(found, func(t *testing.T) {
			w := newWorld(t)
			var checks atomic.Int32
			w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/check", Match: func(*http.Request) bool { checks.Add(1); return false }})
			if found != "none" {
				w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/check", Respond: fault.Status(http.StatusOK, nil, `{"found": "`+found+`"}`)})
			}
			run0 := w.gh.PushRun(w.repo, "main")
			rt, _, log, c, st := w.step(t, run0, "")
			env := w.gh.Env(testgithub.Job{Run: run0, Inputs: map[string]string{"name": "build", "check-only": "true"}})
			rt.Getenv = func(k string) string { return env[k] }
			if err := run(context.Background(), rt, on(true), c, st); err != nil {
				t.Fatal(err)
			}
			if o := testgithub.Outputs(t, env); o["restored"] != found {
				t.Errorf("outputs %v; want restored %s", o, found)
			}
			if s := testgithub.Summary(t, env); !strings.Contains(s, "Only checked, so nothing was restored") || !strings.Contains(s, "(`"+found+"`)") {
				t.Errorf("summary = %s", s)
			}
			if checks.Load() != 1 || w.asked.Load() != 0 || strings.Contains(log.String(), "::add-mask::") {
				t.Errorf("checked %d times, asked to restore %d times; log:\n%s", checks.Load(), w.asked.Load(), log)
			}
		})
	}
	// Only true or false.
	w := newWorld(t)
	rt, _, _, c, st := w.step(t, w.gh.PushRun(w.repo, "main"), "")
	env := w.gh.Env(testgithub.Job{Run: w.gh.PushRun(w.repo, "main"), Inputs: map[string]string{"name": "build", "check-only": "yes"}})
	rt.Getenv = func(k string) string { return env[k] }
	if err := run(context.Background(), rt, on(true), c, st); err == nil || !strings.Contains(err.Error(), "check-only") {
		t.Errorf("check-only: yes: %v", err)
	}
}

func TestInputs(t *testing.T) {
	w := newWorld(t)
	for _, in := range []map[string]string{{"name": "Build", "path": "/tmp/x"}, {"name": "build-", "path": "/tmp/x"}, {"name": "build"}} {
		rt, _, _, svc, st := w.step(t, w.gh.PushRun(w.repo, "main"), "")
		env := w.gh.Env(testgithub.Job{Run: w.gh.PushRun(w.repo, "main"), Inputs: in})
		rt.Getenv = func(k string) string { return env[k] }
		if err := run(context.Background(), rt, on(true), svc, st); err == nil || !strings.Contains(err.Error(), "input") {
			t.Errorf("%v: %v", in, err)
		}
	}
}
