// Package e2e runs the actions' programs as GitHub would run them, with a
// job's environment and event, on one of the runner service's machines as
// Compute Engine gives it a metadata server, against this repository's
// emulator of the service, emulators/service, and its GitHub emulator
// (specs/0007-actions.md, Code and tests). The conformance suite holds the
// emulator to the real service's answers.
//
// There is one GitHub: each test makes its repository, branches, pull
// requests, runs, jobs and approvals in the GitHub emulator, which serves
// the API and event a job sees, and the service emulator reads the same
// state through service.WithGitHub. Its RepositoryOf, RunOf and JobOf give
// the service's handles for what a test made there.
//
// The programs are built with the e2e tag, which lets them call the
// emulator in place of endpoints.Service, use a test's metadata server and
// stand-in for /proc/self/mountinfo, and wait milliseconds; nothing else
// differs from what the actions build.
package e2e

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/runners/actions/internal/testgithub"
)

const (
	small = "lanescape-2vcpu-linux-amd64"
	large = "lanescape-80vcpu-linux-amd64-spot"
)

// files are the repository's: two caches, build, keyed by go.sum, and
// deps, with one key.
var files = map[string]string{
	".github/runners.json": `{"caches": {"build": {"from": ["go.sum"]}, "deps": {}}}`,
	"go.sum":               "example.com/a v1.0.0 h1:x\n",
}

// mounts are a machine's mounts, as /proc/self/mountinfo shows them, before
// its work disk is mounted, and the line its mount adds.
const (
	mounts = "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
		"25 22 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw\n"
	workMount = "40 22 8:16 / /work rw,relatime shared:20 - ext4 /dev/sdb rw\n"
)

// bins are the programs, built once.
var bins struct {
	dir                              string
	disk, checkout, cache, saveCache string
}

func TestMain(m *testing.M) {
	code, err := buildAndRun(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		code = 1
	}
	os.Exit(code)
}

func buildAndRun(m *testing.M) (int, error) {
	dir, err := os.MkdirTemp("", "lanescape-actions-e2e-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	bins.dir = dir
	build := func(name string) (string, error) {
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-trimpath", "-tags", "e2e", "-o", out, "./"+name)
		cmd.Dir = ".."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
		if b, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("go build %s: %v\n%s", name, err, b)
		}
		return out, nil
	}
	for name, into := range map[string]*string{"disk": &bins.disk, "checkout": &bins.checkout, "cache": &bins.cache, "save-cache": &bins.saveCache} {
		if *into, err = build(name); err != nil {
			return 0, err
		}
	}
	return m.Run(), nil
}

type world struct {
	t           *testing.T
	gh          *testgithub.GitHub
	svc         *service.Emulator
	org         *ghemu.Account
	repo        *ghemu.Repo
	contributor *ghemu.Account
	// elsewhere is the metadata server of a machine that isn't the
	// service's: it has no attribute lsr-service, and gives no tokens.
	elsewhere string

	mu sync.Mutex
	// requests are the requests that reached the service, by path.
	requests []string
}

// newWorld starts the GitHub emulator, with the organization by its real
// ID and a repository whose commit declares caches, and
// the service emulator, which reads the same GitHub.
func newWorld(t *testing.T) *world {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end tests run the service emulator and the built programs")
	}
	gh := testgithub.New(t)
	svc := service.New(service.WithGitHub(gh.Emulator))
	t.Cleanup(svc.Close)
	if svc.Organization() != endpoints.Organization {
		t.Fatalf("the service emulator serves %d, not the organization", svc.Organization())
	}
	org := gh.Account(endpoints.Organization)
	repo := gh.Repo(org, gh.NewID(), "app")
	repo.Commit("main", files)
	gh.ProtectEnvironment(repo, "contribution")
	elsewhere := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(elsewhere.Close)
	w := &world{t: t, gh: gh, svc: svc, org: org, repo: repo, contributor: gh.User(9001, "contributor"), elsewhere: elsewhere.URL}
	svc.Faults().Add(fault.Rule{PathPrefix: "/v1/", Match: func(r *http.Request) bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.requests = append(w.requests, r.URL.Path)
		return false
	}})
	return w
}

// asked is how many requests reached the service at path.
func (w *world) asked(path string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, p := range w.requests {
		if p == path {
			n++
		}
	}
	return n
}

// start starts a job of a run, naming a selector, on one of the service's
// machines, which binds the run as the service's check would.
func (w *world) start(run *ghemu.Run, selector string) *machine {
	w.t.Helper()
	j, err := w.svc.StartJob(w.svc.RunOf(run), selector)
	if err != nil {
		w.t.Fatalf("the job's check: %v", err)
	}
	return w.on(j)
}

// machine is one of the service's machines, running a job, with the
// mounts its job's steps see.
type machine struct {
	job       service.Job
	mountinfo string
}

func (w *world) on(j service.Job) *machine {
	w.t.Helper()
	m := &machine{job: j, mountinfo: filepath.Join(w.t.TempDir(), "mountinfo")}
	m.mount(w.t, false)
	return m
}

// mount shows the work disk mounted at /work, or not, as the machine
// mounts it on the host.
func (m *machine) mount(t *testing.T, mounted bool) {
	info := mounts
	if mounted {
		info += workMount
	}
	tmp := m.mountinfo + ".new"
	if err := os.WriteFile(tmp, []byte(info), 0o644); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, m.mountinfo); err != nil {
		t.Error(err)
	}
}

// result is what a step left.
type result struct {
	err     error
	log     string
	outputs map[string]string
	summary string
}

// step runs a program as a step of a job, as GitHub would. On one of the
// service's machines, m, its metadata server answers the attribute
// lsr-service and mints the machine's identity token; without one, it's
// some other runner, whose metadata server has no such attribute.
func (w *world) step(bin string, j testgithub.Job, m *machine, args ...string) result {
	w.t.Helper()
	env := w.gh.Env(j)
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANESCAPE_TEST_SERVICE=" + w.svc.URL(),
		"LANESCAPE_TEST_STORAGE=" + w.svc.CacheStorage().URL}
	if m != nil {
		cmd.Env = append(cmd.Env, "LANESCAPE_TEST_METADATA="+w.svc.Metadata(m.job), "LANESCAPE_TEST_MOUNTINFO="+m.mountinfo)
	} else {
		info := filepath.Join(w.t.TempDir(), "mountinfo")
		if err := os.WriteFile(info, []byte(mounts), 0o644); err != nil {
			w.t.Fatal(err)
		}
		cmd.Env = append(cmd.Env, "LANESCAPE_TEST_METADATA="+w.elsewhere, "LANESCAPE_TEST_MOUNTINFO="+info)
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		w.t.Fatal(err)
	}
	r := result{err: err, log: log.String(), outputs: testgithub.Outputs(w.t, env), summary: testgithub.Summary(w.t, env)}
	// No token handed to the job appears in its log or summary, nor the
	// machine's identity token, nor the caches' token, but in the command
	// that masks it.
	secrets := append(w.gh.Secrets(), service.Token)
	if m != nil {
		secrets = append(secrets, w.svc.Token(m.job))
	}
	for _, s := range secrets {
		if strings.Contains(strings.ReplaceAll(r.log, "::add-mask::"+s, ""), s) || strings.Contains(r.summary, s) {
			w.t.Fatalf("a token is in the step's log or summary:\n%s\n%s", r.log, r.summary)
		}
	}
	if strings.Contains(r.log, "eyJ") || strings.Contains(r.summary, "eyJ") {
		w.t.Fatalf("something like a token is in the step's log or summary:\n%s\n%s", r.log, r.summary)
	}
	return r
}

func (r result) ok(t *testing.T) result {
	t.Helper()
	if r.err != nil {
		t.Fatalf("the step failed: %v\n%s", r.err, r.log)
	}
	return r
}

func (r result) failed(t *testing.T, says string) result {
	t.Helper()
	if r.err == nil || !strings.Contains(r.log, "::error::") || !strings.Contains(r.log, says) {
		t.Fatalf("err = %v; want a failure saying %q:\n%s", r.err, says, r.log)
	}
	return r
}

func (r result) says(t *testing.T, texts ...string) result {
	t.Helper()
	for _, s := range texts {
		if !strings.Contains(r.summary, s) {
			t.Fatalf("the summary doesn't say %q:\n%s\nlog:\n%s", s, r.summary, r.log)
		}
	}
	return r
}

func disk(run *ghemu.Run) testgithub.Job { return testgithub.Job{Run: run} }

// cacheStep is a step of cache or save-cache, for the cache name at path.
func cacheStep(run *ghemu.Run, pr *ghemu.Pull, name, path string) testgithub.Job {
	return testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"name": name, "path": path}}
}

func TestAWorkDiskReadyAtOnce(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.step(bins.disk, disk(run), m).ok(t).
		says(t, "Inside the lanescape organization", "work disk is mounted at `/work`", "a new, empty disk (`none`)")
	if d, ok := w.svc.WorkDisk(m.job); !ok || d.Restored != "none" || !d.Ready || d.Scope != "main" {
		t.Fatalf("the service's work disk = %+v, %v", d, ok)
	}
	if n := w.asked("/v1/disks"); n != 1 {
		t.Fatalf("asked %d times for a disk ready at once", n)
	}
}

func TestAWorkDiskMadeWhileTheActionAsks(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	// The disk is made once the program has asked three times, and the
	// machine mounts it a moment after.
	w.svc.HoldDisks(true)
	var asked atomic.Int32
	done := make(chan struct{})
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Match: func(*http.Request) bool {
		if asked.Add(1) == 3 {
			w.svc.HoldDisks(false)
			go func() {
				defer close(done)
				time.Sleep(50 * time.Millisecond)
				m.mount(t, true)
			}()
		}
		return false
	}})
	r := w.step(bins.disk, disk(run), m).ok(t).says(t, "is mounted at `/work`")
	<-done
	if !strings.Contains(r.log, "is making the work disk") || w.asked("/v1/disks") != 3 {
		t.Fatalf("asked %d times while the disk was made:\n%s", w.asked("/v1/disks"), r.log)
	}
}

func TestAWorkDiskNeverMounted(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	w.step(bins.disk, disk(run), m).failed(t, "isn't mounted at /work after")
}

// A job has one work disk: asking again is answered about the same one.
func TestOneWorkDiskForAJob(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.step(bins.disk, disk(run), m).ok(t)
	w.step(bins.disk, disk(run), m).ok(t).says(t, "work disk is mounted")
}

// A job's work disk is the size and performance its inputs ask for; one
// Compute Engine won't make fails the step with its reason; and asking
// again for other settings is refused, since a job has one work disk.
func TestAWorkDiskOfTheSizeAndPerformanceAsked(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	ask := testgithub.Job{Run: run, Inputs: map[string]string{"size-gb": "500", "iops": "10000", "throughput-mibps": "2400"}}
	w.step(bins.disk, ask, m).ok(t).says(t, "work disk is mounted at `/work`, a disk of 500 GB, 10000 IOPS and 2400 MiB/s")
	if d, ok := w.svc.WorkDiskSettings(m.job); !ok || d != (service.DiskSettings{SizeGB: 500, IOPS: 10000, ThroughputMiBps: 2400}) {
		t.Fatalf("the service's work disk = %+v, %v", d, ok)
	}
	w.step(bins.disk, testgithub.Job{Run: run, Inputs: map[string]string{"size-gb": "501"}}, m).failed(t, "a job has one work disk")

	run2 := w.gh.PushRun(w.repo, "main")
	other := w.start(run2, large)
	w.step(bins.disk, testgithub.Job{Run: run2, Inputs: map[string]string{"size-gb": "50", "iops": "30000"}}, other).
		failed(t, "Compute Engine refused the work disk: Invalid value for field 'resource.provisionedIops'")
	w.step(bins.disk, testgithub.Job{Run: run2, Inputs: map[string]string{"iops": "0"}}, other).
		failed(t, "input iops is \"0\"; it must be a positive whole number")
}

// tree makes a small tree of every kind of entry a cache keeps, with
// modes and times to the nanosecond.
func tree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tree")
	write := func(rel, data string, mode os.FileMode) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("src/main.go", "package main\n", 0o644)
	write("src/run.sh", "#!/bin/sh\n", 0o755)
	write("src-gen/empty", "", 0o600)
	write("out/obj/big.o", strings.Repeat("0123456789abcdef", 300_000), 0o644)
	if err := os.MkdirAll(filepath.Join(root, "nested/empty/dirs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"src/link": "../src-gen/empty", "abs": "/etc/hosts", "dangling": "nowhere"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	mt := time.Unix(1_700_000_000, 123_456_789)
	for _, p := range []string{"src/main.go", "out/obj/big.o", "nested/empty/dirs", "nested/empty", "out/obj", "out", "src"} {
		if err := os.Chtimes(filepath.Join(root, p), mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// scan is each entry under root: its kind, mode, modification time to the
// nanosecond, and its target or contents.
func scan(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		s := fmt.Sprintf("%v %d", info.Mode(), info.ModTime().UnixNano())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			s = fmt.Sprintf("symlink %d -> %s", info.ModTime().UnixNano(), target)
		case info.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			s += fmt.Sprintf(" %d %x", len(b), sha256.Sum256(b))
		}
		out[rel] = s
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTrees(t *testing.T, want, got string) {
	t.Helper()
	w, g := scan(t, want), scan(t, got)
	for rel, s := range w {
		if g[rel] != s {
			t.Errorf("%s: restored %q, saved %q", rel, g[rel], s)
		}
	}
	for rel := range g {
		if _, ok := w[rel]; !ok {
			t.Errorf("%s was restored, but wasn't saved", rel)
		}
	}
}

// saved runs a job of a run in main that saves the tree src as the cache
// build, and finishes with conclusion.
func (w *world) saved(t *testing.T, src, conclusion string) *machine {
	t.Helper()
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.step(bins.disk, disk(run), m).ok(t)
	w.step(bins.saveCache, cacheStep(run, nil, "build", src), m).ok(t).
		says(t, "Saving the cache `build`", "as 8 objects", "keeps the version if the job succeeds")
	w.svc.FinishJob(m.job, conclusion)
	return m
}

// A cache saved in main is restored by a later run in main exactly as it
// was saved, from main's version for its key.
func TestACacheSavedAndRestored(t *testing.T) {
	w := newWorld(t)
	src := tree(t)
	w.saved(t, src, "success")
	if v := w.svc.Caches(w.svc.RepositoryOf(w.repo)); len(v) != 1 || v[0].Scope != "main" || v[0].Cache != "build" || v[0].Objects != 8 {
		t.Fatalf("the service's versions = %+v", v)
	}
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	dst := filepath.Join(t.TempDir(), "work", "tree")
	r := w.step(bins.cache, cacheStep(run, nil, "build", dst), m).ok(t).
		says(t, "Restored `"+dst+"` from main's version for this key (`main_exact`)")
	if r.outputs["restored"] != "main_exact" {
		t.Fatalf("outputs %v", r.outputs)
	}
	sameTrees(t, src, dst)
	// Restoring onto what's there gives the same tree again.
	w.step(bins.cache, cacheStep(run, nil, "build", dst), m).ok(t)
	sameTrees(t, src, dst)
}

// A pull request's run restores main's version, its base's, as the
// restore order has it.
func TestAPullRequestRestoresMains(t *testing.T) {
	w := newWorld(t)
	src := tree(t)
	w.saved(t, src, "success")
	w.repo.Commit("feature", map[string]string{"README": "a change"})
	pr := w.gh.PullRequest(w.repo, "main", w.repo, "feature", w.org)
	run := w.gh.PullRequestRun(pr)
	m := w.start(run, large)
	m.mount(t, true)
	dst := filepath.Join(t.TempDir(), "tree")
	r := w.step(bins.cache, cacheStep(run, pr, "build", dst), m).ok(t).says(t, "(`main_exact`)")
	if r.outputs["restored"] != "main_exact" {
		t.Fatalf("outputs %v", r.outputs)
	}
	sameTrees(t, src, dst)
	// Its own save is the pull request's, and main's stays main's.
	w.step(bins.saveCache, cacheStep(run, pr, "build", dst), m).ok(t)
	w.svc.FinishJob(m.job, "success")
	scopes := map[string]bool{}
	for _, v := range w.svc.Caches(w.svc.RepositoryOf(w.repo)) {
		scopes[v.Scope] = true
	}
	if !scopes["main"] || !scopes[fmt.Sprintf("pr%d", pr.Number)] {
		t.Fatalf("the service's versions are in %v", scopes)
	}
}

// A push to a branch the consumer doesn't list is in the read scope, which
// saves nothing: save-cache succeeds, saying why, and uploads nothing.
func TestASaveInAScopeThatSavesNothing(t *testing.T) {
	w := newWorld(t)
	w.repo.Commit("feature", map[string]string{"x": "1"})
	run := w.gh.PushRun(w.repo, "feature")
	m := w.start(run, large)
	w.step(bins.saveCache, cacheStep(run, nil, "build", tree(t)), m).ok(t).says(t, "so the cache wasn't saved", "read")
	w.svc.FinishJob(m.job, "success")
	if v := w.svc.Caches(w.svc.RepositoryOf(w.repo)); len(v) != 0 {
		t.Fatalf("the service's versions = %+v", v)
	}
	if names := w.svc.CacheStorage().Names(service.CachesBucket); len(names) != 0 || w.asked("/v1/caches/commit") != 0 {
		t.Fatalf("uploaded %v", names)
	}
}

// The version a failed job saved is never restored, and its objects go.
func TestAFailedJobsVersionIsntRestored(t *testing.T) {
	w := newWorld(t)
	w.saved(t, tree(t), "failure")
	if v := w.svc.Caches(w.svc.RepositoryOf(w.repo)); len(v) != 0 {
		t.Fatalf("the service's versions = %+v", v)
	}
	if names := w.svc.CacheStorage().Names(service.CachesBucket); len(names) != 0 {
		t.Fatalf("the failed job's objects are still there: %v", names)
	}
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	dst := filepath.Join(t.TempDir(), "tree")
	r := w.step(bins.cache, cacheStep(run, nil, "build", dst), m).ok(t).says(t, "nothing was restored (`none`)")
	if r.outputs["restored"] != "none" {
		t.Fatalf("outputs %v", r.outputs)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("made the directory to restore under")
	}
}

// A cache the run's commit doesn't declare is refused, with the
// service's reason; a name that isn't one is never asked about.
func TestACacheTheCommitDoesntDeclare(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	w.step(bins.cache, cacheStep(run, nil, "tests", t.TempDir()), m).failed(t, "doesn't declare the cache tests")
	w.step(bins.saveCache, cacheStep(run, nil, "tests", t.TempDir()), m).failed(t, "doesn't declare the cache tests")
	before := len(w.requests)
	for _, n := range []string{"", "Build", "build-", "1build", strings.Repeat("a", 33), "a b"} {
		w.step(bins.cache, cacheStep(run, nil, n, t.TempDir()), m).failed(t, "isn't a cache's name")
		w.step(bins.saveCache, cacheStep(run, nil, n, t.TempDir()), m).failed(t, "isn't a cache's name")
	}
	if len(w.requests) != before {
		t.Fatalf("asked the service about names that aren't caches': %v", w.requests[before:])
	}
}

// A job saves a cache once: a second save after the commit is refused.
func TestAJobSavesACacheOnce(t *testing.T) {
	w := newWorld(t)
	src := tree(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	w.step(bins.saveCache, cacheStep(run, nil, "build", src), m).ok(t)
	w.step(bins.saveCache, cacheStep(run, nil, "build", src), m).failed(t, "a job saves a cache once")
}

func TestOnAMachineThatIsntTheServices(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	r := w.step(bins.disk, disk(run), nil).ok(t).says(t, "Inside the lanescape organization", "isn't one of the runner service's machines")
	if !strings.Contains(r.log, "does nothing here") {
		t.Fatalf("log:\n%s", r.log)
	}
	w.step(bins.saveCache, cacheStep(run, nil, "build", tree(t)), nil).ok(t).says(t, "isn't one of the runner service's machines")
	if r := w.step(bins.cache, cacheStep(run, nil, "build", t.TempDir()), nil).ok(t).says(t, "isn't one of the runner service's machines"); r.outputs["restored"] != "none" {
		t.Fatalf("outputs %v", r.outputs)
	}
	if len(w.requests) != 0 {
		t.Fatalf("asked the service %v", w.requests)
	}
}

func TestAForksOwnRun(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.contributor, w.gh.NewID())
	run := w.gh.PushRun(fork, "main")
	w.step(bins.disk, disk(run), nil).ok(t).says(t, "Outside the lanescape organization", "disk did nothing")
	w.step(bins.saveCache, cacheStep(run, nil, "build", tree(t)), nil).ok(t).says(t, "Outside the lanescape organization", "save-cache did nothing")
	w.step(bins.cache, cacheStep(run, nil, "build", t.TempDir()), nil).ok(t).says(t, "Outside the lanescape organization", "cache did nothing")
	if len(w.requests) != 0 {
		t.Fatalf("asked the service %v", w.requests)
	}
}

func TestAForksPullRequestIntoTheOrganization(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.contributor, w.gh.NewID())
	fork.Commit("feature", map[string]string{"x": "1"})
	pr := w.gh.PullRequest(w.repo, "main", fork, "feature", w.contributor)
	run := w.gh.PullRequestRun(pr)
	// The service gives such a run none of its machines.
	var refused *service.Refused
	if _, err := w.svc.StartJob(w.svc.RunOf(run), large); !errors.As(err, &refused) {
		t.Fatalf("the service gave a fork's pull request's job a machine: %v", err)
	}
	w.step(bins.disk, testgithub.Job{Run: run, Pull: pr}, nil).ok(t).says(t, "Outside the lanescape organization", "a fork's pull request")
	w.step(bins.saveCache, cacheStep(run, pr, "build", tree(t)), nil).ok(t).says(t, "a fork's pull request")
	w.step(bins.cache, cacheStep(run, pr, "build", t.TempDir()), nil).ok(t).says(t, "a fork's pull request")
	if len(w.requests) != 0 {
		t.Fatalf("asked the service %v", w.requests)
	}
}

// An approved contribution's pull_request_target run is bound at its
// contribution job's check, once a maintainer approves it, and its build
// job, which needs that job, gets its work disk and saves its caches in
// the pull request's scope, and checks out the contribution's head.
func TestAnApprovedContributionsBuildJob(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.contributor, w.gh.NewID())
	fork.Commit("feature", map[string]string{"x": "1"})
	pr := w.gh.PullRequest(w.repo, "main", fork, "feature", w.contributor)
	run := w.gh.PullRequestTargetRun(pr)
	contribution := w.gh.Queue(run, ghemu.JobSpec{Name: "contribution", Labels: []string{small}, Environment: "contribution"})
	if _, err := w.svc.StartWorkflowJob(contribution); !errors.Is(err, service.ErrWaiting) {
		t.Fatalf("the contribution job before its approval: %v", err)
	}
	// Until then the run is unbound, and a job of it on the smallest
	// shape gets no work disk.
	unbound := w.start(run, small)
	job := testgithub.Job{Run: run, Pull: pr}
	w.step(bins.disk, job, unbound).failed(t, "isn't bound")

	w.svc.Approve(w.svc.RunOf(run))
	if _, err := w.svc.StartWorkflowJob(contribution); err != nil {
		t.Fatalf("the approved contribution job's check: %v", err)
	}
	scope := fmt.Sprintf("pr%d", pr.Number)
	if s, ok := w.svc.Binding(w.svc.RunOf(run)); !ok || s != scope {
		t.Fatalf("the contribution bound %q, %v", s, ok)
	}
	m := w.start(run, large)
	m.mount(t, true)
	w.step(bins.disk, job, m).ok(t).says(t, "Inside the lanescape organization", "pull_request_target", "is mounted at `/work`")
	if d, _ := w.svc.WorkDisk(m.job); d.Scope != scope {
		t.Fatalf("the work disk is in %q", d.Scope)
	}
	r := w.step(bins.checkout, testgithub.Job{Run: run, Pull: pr, Inputs: map[string]string{"contribution": "true"}}, m, "decide").ok(t)
	if r.outputs["ref"] != run.EventPull.HeadSHA || r.outputs["persist-credentials"] != "false" {
		t.Fatalf("checkout outputs %v", r.outputs)
	}
	w.step(bins.saveCache, cacheStep(run, pr, "build", tree(t)), m).ok(t).says(t, "Saved `")
	w.svc.FinishJob(m.job, "success")
	if v := w.svc.Caches(w.svc.RepositoryOf(w.repo)); len(v) != 1 || v[0].Scope != scope {
		t.Fatalf("the service's versions = %+v", v)
	}
}

func TestTheServiceRetriesUntilGitHubConfirms(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	// GitHub confirms nothing until the program has asked twice.
	w.svc.Unavailable(true)
	var asked atomic.Int32
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/", Match: func(*http.Request) bool {
		if asked.Add(1) == 3 {
			w.svc.Unavailable(false)
		}
		return false
	}})
	r := w.step(bins.disk, disk(run), m).ok(t)
	if !strings.Contains(r.log, "trying again") || w.asked("/v1/disks") != 3 {
		t.Fatalf("no retries were logged:\n%s", r.log)
	}
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/save", Times: 2, Respond: fault.Unavailable(time.Second)})
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/commit", Times: 2, Respond: fault.Unavailable(time.Second)})
	if r := w.step(bins.saveCache, cacheStep(run, nil, "build", tree(t)), m).ok(t); strings.Count(r.log, "trying again") < 4 {
		t.Fatalf("no retries were logged:\n%s", r.log)
	}
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/caches/restore", Times: 2, Respond: fault.Unavailable(time.Second)})
	if r := w.step(bins.cache, cacheStep(run, nil, "build", t.TempDir()), m).ok(t); !strings.Contains(r.log, "trying again") {
		t.Fatalf("no retries were logged:\n%s", r.log)
	}
}

func TestTheActionsFailOnceTheirRetriesAreSpent(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.svc.Unavailable(true)
	w.step(bins.disk, disk(run), m).failed(t, "retries are spent")
	if n := w.asked("/v1/disks"); n != 8 {
		t.Fatalf("asked %d times, not 8", n)
	}
	w.step(bins.saveCache, cacheStep(run, nil, "build", tree(t)), m).failed(t, "retries are spent")
	w.step(bins.cache, cacheStep(run, nil, "build", t.TempDir()), m).failed(t, "retries are spent")
}

// Cloud Storage's own failures are retried as well, and an object that
// isn't there as the version recorded it fails the restore.
func TestCloudStorageFailures(t *testing.T) {
	w := newWorld(t)
	src := tree(t)
	gcs := w.svc.CacheStorage()
	gcs.Faults().Add(fault.Rule{PathPrefix: "/upload/", Times: 2, Respond: storage.JSONError(http.StatusServiceUnavailable, "backendError", "busy")})
	w.saved(t, src, "success")
	gcs.Faults().Add(fault.Rule{PathPrefix: "/storage/", Times: 2, Respond: storage.JSONError(http.StatusTooManyRequests, "rateLimitExceeded", "slow down")})
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	dst := filepath.Join(t.TempDir(), "tree")
	w.step(bins.cache, cacheStep(run, nil, "build", dst), m).ok(t)
	sameTrees(t, src, dst)

	// An object replaced since the commit fails the next run's restore,
	// before anything is restored.
	for _, name := range gcs.Names(service.CachesBucket) {
		if strings.HasSuffix(name, "dirs.tar.zst") {
			gcs.Put(service.CachesBucket, name, []byte("replaced"))
		}
	}
	run = w.gh.PushRun(w.repo, "main")
	m = w.start(run, large)
	dst = filepath.Join(t.TempDir(), "again")
	w.step(bins.cache, cacheStep(run, nil, "build", dst), m).failed(t, "can't be restored")
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Fatalf("restored part of it: %v", entries)
	}
}

// A server in front of the service that's busy is retried too; any other
// answer outside the interface fails at once.
func TestAnswersFromInFront(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	m.mount(t, true)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Status(http.StatusBadGateway, nil, "<html>bad gateway</html>")})
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Status(http.StatusTooManyRequests, nil, "")})
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Times: 1, Respond: fault.Drop})
	w.step(bins.disk, disk(run), m).ok(t)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Times: 1,
		Respond: fault.Status(http.StatusFound, http.Header{"Location": {w.svc.URL() + "/v1/disks"}}, "")})
	before := w.asked("/v1/disks")
	w.step(bins.disk, disk(run), m).failed(t, "answered 302, which its interface doesn't have")
	if n := w.asked("/v1/disks") - before; n != 1 {
		t.Fatalf("asked %d times, following a redirect or retrying it", n)
	}
}

// A refusal fails the step with the service's reason, on one line, and
// nothing the service says can bring the machine's token into the log.
func TestARefusalFailsWithItsReason(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	m := w.start(run, large)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/", Respond: func(rw http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		escaped := `\u` + fmt.Sprintf("%04x", tok[0]) + tok[1:]
		fault.Status(http.StatusForbidden, nil, `{"reason": "no such thing\n::add-mask::x for `+escaped+`"}`)(rw, r)
	}})
	r := w.step(bins.disk, disk(run), m).failed(t, "no such thing ::add-mask::x for ***")
	if strings.Contains(r.log, "\n::add-mask::x") {
		t.Fatalf("the reason started a workflow command:\n%s", r.log)
	}
	w.step(bins.saveCache, cacheStep(run, nil, "build", tree(t)), m).failed(t, "no such thing")
	w.step(bins.cache, cacheStep(run, nil, "build", t.TempDir()), m).failed(t, "no such thing")
}

func TestCheckoutRefusesAContributionOutsidePullRequestTarget(t *testing.T) {
	w := newWorld(t)
	run := w.gh.PushRun(w.repo, "main")
	r := w.step(bins.checkout, testgithub.Job{Run: run, Inputs: map[string]string{"contribution": "true"}}, nil, "decide")
	if r.err == nil || !strings.Contains(r.log, "only in a pull_request_target run") || len(r.outputs) != 0 {
		t.Fatalf("err = %v, outputs %v\n%s", r.err, r.outputs, r.log)
	}
}
