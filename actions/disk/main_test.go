package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/service"
	"github.com/lanescape/runners/endpoints"

	"github.com/lanescape/runners/actions/internal/actions"
	"github.com/lanescape/runners/actions/internal/backoff"
	"github.com/lanescape/runners/actions/internal/metadata"
	svcclient "github.com/lanescape/runners/actions/internal/service"
	"github.com/lanescape/runners/actions/internal/testgithub"
)

// on is whether the machine is the service's, as its metadata server would
// say.
type on bool

func (o on) OnService(context.Context) bool { return bool(o) }

type world struct {
	gh   *testgithub.GitHub
	svc  *service.Emulator
	repo *ghemu.Repo
	// asked counts the requests for a disk.
	asked atomic.Int32
	// last is the job the last call to job started.
	last service.Job
}

func newWorld(t *testing.T) *world {
	gh := testgithub.New(t)
	svc := service.New(service.WithGitHub(gh.Emulator))
	t.Cleanup(svc.Close)
	repo := gh.Repo(gh.Account(endpoints.Organization), gh.NewID(), "app")
	repo.Commit("main", map[string]string{".github/runners.json": `{"caches": {"build": {}}}`})
	w := &world{gh: gh, svc: svc, repo: repo}
	svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Match: func(*http.Request) bool { w.asked.Add(1); return false }})
	return w
}

// job is a step of a job of run on one of the service's machines, with
// its client, and mounts that show /work mounted or not.
func (w *world) job(t *testing.T, run *ghemu.Run, mounted bool) (*actions.Runtime, *svcclient.Client, timing, *bytes.Buffer) {
	t.Helper()
	j, err := w.svc.StartJob(w.svc.RunOf(run), "lanescape-2vcpu-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	w.last = j
	env := w.gh.Env(testgithub.Job{Run: run})
	var log bytes.Buffer
	rt := &actions.Runtime{Getenv: func(k string) string { return env[k] }, Stdout: &log}
	c := svcclient.At(w.svc.URL(), metadata.Server{URL: w.svc.Metadata(j)})
	c.Retry = backoff.Policy{Attempts: 3, First: time.Millisecond, Max: time.Millisecond}
	info := filepath.Join(t.TempDir(), "mountinfo")
	lines := "22 1 8:1 / / rw - ext4 /dev/sda1 rw\n"
	if mounted {
		lines += "40 22 8:16 / /work rw - ext4 /dev/sdb rw\n"
	}
	if err := os.WriteFile(info, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return rt, c, timing{making: 10 * time.Millisecond, mountWait: 100 * time.Millisecond, mountEvery: 5 * time.Millisecond, mountinfo: info}, &log
}

func summary(rt *actions.Runtime) string {
	b, _ := os.ReadFile(rt.Getenv("GITHUB_STEP_SUMMARY"))
	return string(b)
}

func TestReady(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), true)
	if err := run(context.Background(), rt, on(true), c, tm); err != nil {
		t.Fatal(err)
	}
	if s := summary(rt); !strings.Contains(s, "work disk is mounted at `/work`") || !strings.Contains(s, "(`none`)") {
		t.Fatalf("summary = %s", s)
	}
}

// While the disk is being made, the program asks again, every tm.making.
func TestMaking(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, log := w.job(t, w.gh.PushRun(w.repo, "main"), true)
	w.svc.HoldDisks(true)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Match: func(*http.Request) bool {
		if w.asked.Load() == 3 {
			w.svc.HoldDisks(false)
		}
		return false
	}})
	start := time.Now()
	if err := run(context.Background(), rt, on(true), c, tm); err != nil {
		t.Fatal(err)
	}
	n := int(w.asked.Load())
	if elapsed := time.Since(start); n != 3 || time.Duration(n-1)*tm.making > elapsed {
		t.Fatalf("asked %d times in %s, every %s", n, elapsed, tm.making)
	}
	if strings.Count(log.String(), "is making the work disk") != 1 {
		t.Fatalf("log:\n%s", log)
	}
}

func TestNeverMounted(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), false)
	start := time.Now()
	err := run(context.Background(), rt, on(true), c, tm)
	if err == nil || !strings.Contains(err.Error(), "isn't mounted at /work after 100ms") || time.Since(start) < tm.mountWait {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

func TestNotTheServicesMachine(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), false)
	if err := run(context.Background(), rt, on(false), c, tm); err != nil {
		t.Fatal(err)
	}
	if s := summary(rt); !strings.Contains(s, "isn't one of the runner service's machines") || w.asked.Load() != 0 {
		t.Fatalf("summary = %s, asked %d", s, w.asked.Load())
	}
}

func TestOutside(t *testing.T) {
	w := newWorld(t)
	fork := w.gh.Fork(w.repo, w.gh.User(9001, "contributor"), w.gh.NewID())
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), false)
	env := w.gh.Env(testgithub.Job{Run: w.gh.PushRun(fork, "main")})
	rt.Getenv = func(k string) string { return env[k] }
	if err := run(context.Background(), rt, on(true), c, tm); err != nil {
		t.Fatal(err)
	}
	if s := summary(rt); !strings.Contains(s, "Outside the lanescape organization") || w.asked.Load() != 0 {
		t.Fatalf("summary = %s, asked %d", s, w.asked.Load())
	}
}

func TestRefused(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), true)
	w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Respond: fault.Refuse("the job's machine has no work disk to give")})
	if err := run(context.Background(), rt, on(true), c, tm); err == nil ||
		!strings.Contains(err.Error(), "refused the work disk: the job's machine has no work disk to give") {
		t.Fatalf("err = %v", err)
	}
}

// A rerun of a job lost to a preemption or the age limit gets its disk
// from its resume image, which the summary says; anything else the
// interface doesn't have fails.
func TestWhereTheDiskCameFrom(t *testing.T) {
	for restored, says := range map[string]string{"resume": "the resume image of this job's own disk", "main_exact": "doesn't have"} {
		w := newWorld(t)
		rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), true)
		w.svc.Faults().Add(fault.Rule{PathPrefix: "/v1/disks", Respond: fault.Status(http.StatusOK, nil, `{"state": "ready", "restored": "`+restored+`"}`)})
		err := run(context.Background(), rt, on(true), c, tm)
		got := summary(rt)
		if err != nil {
			got = err.Error()
		}
		if !strings.Contains(got, says) {
			t.Errorf("%s: %s", restored, got)
		}
	}
}

// withInputs gives the step the action's inputs, as its action.yml passes
// them.
func withInputs(rt *actions.Runtime, inputs map[string]string) {
	get := rt.Getenv
	rt.Getenv = func(k string) string {
		for name, v := range inputs {
			if k == "INPUT_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_")) {
				return v
			}
		}
		return get(k)
	}
}

// The disk is the size and performance the inputs ask for, and the
// service's default for what they leave empty.
func TestTheDiskItsInputsAskFor(t *testing.T) {
	// The service's defaults, as specs/0005-interface.md gives them.
	defaults := service.DiskSettings{SizeGB: 50, IOPS: 3000, ThroughputMiBps: 400}
	for name, tc := range map[string]struct {
		inputs map[string]string
		want   service.DiskSettings
		says   string
	}{
		"all": {map[string]string{"size-gb": "500", "iops": "10000", "throughput-mibps": "2400"},
			service.DiskSettings{SizeGB: 500, IOPS: 10000, ThroughputMiBps: 2400}, "of 500 GB, 10000 IOPS and 2400 MiB/s"},
		"size only": {map[string]string{"size-gb": " 200 ", "iops": ""},
			service.DiskSettings{SizeGB: 200, IOPS: defaults.IOPS, ThroughputMiBps: defaults.ThroughputMiBps},
			"of 200 GB, the runner service's default IOPS and the runner service's default throughput"},
		"none": {nil,
			defaults,
			"of the runner service's default size"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			rt, c, tm, log := w.job(t, w.gh.PushRun(w.repo, "main"), true)
			withInputs(rt, tc.inputs)
			if err := run(context.Background(), rt, on(true), c, tm); err != nil {
				t.Fatal(err)
			}
			if got, ok := w.svc.WorkDiskSettings(w.last); !ok || got != tc.want {
				t.Fatalf("work disk = %+v (made: %v); want %+v", got, ok, tc.want)
			}
			if s := summary(rt); !strings.Contains(s, tc.says) || !strings.Contains(log.String(), tc.says) {
				t.Fatalf("summary = %s\nlog:\n%s\nwant both to say %q", s, log, tc.says)
			}
		})
	}
}

// An input that isn't a positive whole number fails the step before it
// asks for anything.
func TestABadInput(t *testing.T) {
	for _, v := range []string{"0", "-5", "1.5", "lots", "+5", "05", "1e3", "9223372036854775808", "1\n::error::x"} {
		w := newWorld(t)
		rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), true)
		withInputs(rt, map[string]string{"iops": v})
		err := run(context.Background(), rt, on(true), c, tm)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("input iops is %q; it must be a positive whole number", v)) || w.asked.Load() != 0 {
			t.Errorf("%q: err = %v, asked %d", v, err, w.asked.Load())
		}
	}
}

// A disk Compute Engine won't make fails the step with its reason.
func TestADiskComputeEngineWontMake(t *testing.T) {
	w := newWorld(t)
	rt, c, tm, _ := w.job(t, w.gh.PushRun(w.repo, "main"), true)
	withInputs(rt, map[string]string{"size-gb": "50", "iops": "30000"})
	err := run(context.Background(), rt, on(true), c, tm)
	if err == nil || !strings.Contains(err.Error(), "refused the work disk: Compute Engine refused the work disk: Invalid value for field 'resource.provisionedIops'") {
		t.Fatalf("err = %v", err)
	}
}
