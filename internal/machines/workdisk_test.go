package machines_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gceemu "github.com/lanescape/runners/emulators/compute"
	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/machines"
)

// admit queues a job of a run, makes its machine, and plays the job to its
// admission there.
func (e *env) admit(run *ghemu.Run, labels ...string) *machine {
	e.t.Helper()
	j := e.queue(run, labels...)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	if code := m.started(run.ID); code != http.StatusOK {
		e.t.Fatalf("job-started check: %d", code)
	}
	return m
}

// refusedFor checks that a request was refused, saying why.
func refusedFor(t *testing.T, what string, code int, body map[string]any, reason string) {
	t.Helper()
	if code != http.StatusForbidden || !strings.Contains(fmt.Sprint(body["reason"]), reason) {
		t.Fatalf("%s: %d %v, want 403 saying %q", what, code, body, reason)
	}
}

// chosen starts every work disk from a base image, as if it were a resume
// image larger than the shape's disk, and records what it was asked for.
type chosen struct {
	mu    sync.Mutex
	asked []machines.Machine
}

func (c *chosen) Choose(_ context.Context, m machines.Machine, _ machines.Admission, size int64) (string, int64, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, m)
	return "lsr-base-amd64-new", size + 20, machines.Resumed, nil
}

func (c *chosen) all() []machines.Machine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]machines.Machine(nil), c.asked...)
}

func TestAJobAsksForItsWorkDisk(t *testing.T) {
	e := setup(t)
	src := &chosen{}
	e.svc.Disks = src
	e.repo.Commit("feature", map[string]string{"feature.txt": "1"})
	p := e.gh.PullRequest(e.repo, "main", e.repo, "feature", e.org)
	run := e.gh.PullRequestRun(p)
	m := e.admit(run, sel2)
	j, _, _ := e.gh.JobOnRunner(m.in.Name)
	if restored := m.diskReady(); restored != machines.Resumed {
		t.Fatalf("restored = %q", restored)
	}
	// The service chose what it starts from, in the scope it bound the run
	// to.
	asked := src.all()
	if len(asked) != 1 || asked[0].Name != m.in.Name || asked[0].Selector.Scope.String() != "pr"+strconv.Itoa(p.Number) {
		t.Fatalf("chosen for %+v; want the run's scope", asked)
	}
	in, _ := e.gce.Instance(m.in.Zone, m.in.Name)
	if in.Disks[machines.WorkDevice] != machines.WorkDisk(m.in.Name) {
		t.Fatalf("disks = %v; want the work disk attached as %q", in.Disks, machines.WorkDevice)
	}
	var work bool
	for _, d := range e.gce.Disks() {
		if d.Name == machines.WorkDisk(m.in.Name) {
			work = true
			if d.SizeGB != 70 || !strings.HasSuffix(d.SourceImage, "lsr-base-amd64-new") || d.IOPS != 3000 || d.Throughput != 400 ||
				d.Labels[machines.LabelMachine] != m.in.Name || d.Labels[machines.LabelJob] != strconv.FormatInt(j.ID, 10) {
				t.Fatalf("work disk = %+v; want the chosen image's, at its size", d)
			}
		}
	}
	if !work {
		t.Fatal("no work disk")
	}
	rec, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json")
	if !ok || rec["state"] != machines.DiskAttached || rec["machine"] != m.in.Name || rec["job_id"] != float64(j.ID) ||
		rec["restored"] != machines.Resumed || rec["family"] != machines.LegacyFamily || rec["key"] != nil {
		t.Fatalf("disk record = %v", rec)
	}
	// Asking again is answered about the same disk, and nothing more: a
	// job has one work disk.
	code, body := m.disk()
	if code != http.StatusOK || body["state"] != machines.DiskStateReady || body["restored"] != machines.Resumed {
		t.Fatalf("again: %d %v", code, body)
	}
	if n := len(e.gce.Disks()); n != 2 || len(src.all()) != 1 {
		t.Fatalf("%d disks, %d choices; want the boot and work disks, chosen once", n, len(src.all()))
	}
}

func TestAnEmptyWorkDisk(t *testing.T) {
	e := setup(t)
	m := e.admit(e.mainRun(), sel2)
	if restored := m.diskReady(); restored != machines.NotRestored {
		t.Fatalf("restored = %q; with no disk source, every disk starts empty", restored)
	}
	for _, d := range e.gce.Disks() {
		if d.Name == machines.WorkDisk(m.in.Name) && (d.SizeGB != 50 || d.SourceImage != "") {
			t.Fatalf("work disk = %+v; want an empty disk of the shape's size", d)
		}
	}
	// A body naming a family, as work disks once had, is refused.
	code, body := e.post(machines.DisksPath, e.token(m.in), map[string]string{"family": "build"})
	if code != http.StatusBadRequest && code != http.StatusForbidden {
		t.Fatalf("a work disk request naming a family: %d %v", code, body)
	}
}

// caches records the cache requests it's asked, and answers with what the
// test sets.
type caches struct {
	mu      sync.Mutex
	asked   []string
	scopes  []string
	restore machines.CacheRestore
	err     error
}

func (c *caches) note(what string, m machines.Machine, cache, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, what+" "+cache+" "+key)
	c.scopes = append(c.scopes, m.Selector.Scope.String())
	return c.err
}

func (c *caches) Restore(_ context.Context, m machines.Machine, _ machines.Admission, cache, key string) (machines.CacheRestore, error) {
	return c.restore, c.note("restore", m, cache, key)
}

func (c *caches) Save(_ context.Context, m machines.Machine, _ machines.Admission, cache, key string) (machines.CacheSave, error) {
	return machines.CacheSave{Saves: true, Bucket: "b", Prefix: "1/v/", Token: "t"}, c.note("save", m, cache, key)
}

func (c *caches) Commit(_ context.Context, m machines.Machine, _ machines.Admission, cache string) (machines.CacheCommit, error) {
	return machines.CacheCommit{Objects: 1, Bytes: 2}, c.note("commit", m, cache, "")
}

func (c *caches) all() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.asked...), append([]string(nil), c.scopes...)
}

func TestCacheRequests(t *testing.T) {
	e := setup(t)
	cs := &caches{restore: machines.CacheRestore{Restored: "main_exact", Bucket: "b", Token: "t",
		Objects: []machines.CacheObject{{Name: "1/v/shard-000.tar.zst", Size: 2, CRC32C: "AAAAAA==", Generation: 3}}}}
	e.svc.Caches = cs
	e.repo.Commit("main", map[string]string{".github/runners.json": `{"caches": {"build": {"from": ["deps.txt"]}}}`, "deps.txt": "1\n"})
	m := e.admit(e.mainRun(), sel2)
	code, body := m.cache(machines.CacheRestorePath, "build")
	if code != http.StatusOK || body["restored"] != "main_exact" || body["bucket"] != "b" || body["token"] != "t" {
		t.Fatalf("restore: %d %v", code, body)
	}
	code, body = m.cache(machines.CacheSavePath, "build")
	if code != http.StatusOK || body["saves"] != true || body["prefix"] != "1/v/" {
		t.Fatalf("save: %d %v", code, body)
	}
	code, body = m.cache(machines.CacheCommitPath, "build")
	if code != http.StatusOK || body["objects"] != float64(1) || body["bytes"] != float64(2) {
		t.Fatalf("commit: %d %v", code, body)
	}
	// Each was asked with the run's scope, and restore and save with the
	// cache's key at the run's commit, which the service makes, never the
	// job.
	asked, scopes := cs.all()
	key := asked[0][len("restore build "):]
	if len(asked) != 3 || len(key) != 20 || asked[0] != "restore build "+key || asked[1] != "save build "+key || asked[2] != "commit build " ||
		scopes[0] != "main" || scopes[1] != "main" || scopes[2] != "main" {
		t.Fatalf("asked %v in %v", asked, scopes)
	}
	// A refusal is answered 403 with its reason, and anything else is to be
	// tried again.
	cs.err = machines.RefusedError{Reason: "no, because"}
	code, body = m.cache(machines.CacheSavePath, "build")
	refusedFor(t, "a refused save", code, body, "no, because")
	cs.err = fmt.Errorf("the bucket couldn't be read")
	code, body = m.cache(machines.CacheCommitPath, "build")
	if code != http.StatusServiceUnavailable || body["retryable"] != true {
		t.Fatalf("a failed commit: %d %v; want 503, retryable", code, body)
	}
}

// In read, a save says it saves nothing, and asks nothing of the caches.
func TestACacheSaveInRead(t *testing.T) {
	e := setup(t)
	cs := &caches{}
	e.svc.Caches = cs
	e.repo.Commit("feature", map[string]string{".github/runners.json": declares})
	r := e.admit(e.gh.PushRun(e.repo, "feature"), sel2)
	if a, _, _ := e.svc.AdmissionOf(context.Background(), r.in.Name); !a.Scope.IsRead() {
		t.Fatalf("admission = %+v; want the run bound read", a)
	}
	code, body := r.cache(machines.CacheSavePath, "build")
	if code != http.StatusOK || body["saves"] != false || !strings.Contains(fmt.Sprint(body["reason"]), "read") {
		t.Fatalf("a save in read: %d %v, want 200 saving nothing, saying why", code, body)
	}
	if asked, _ := cs.all(); len(asked) != 0 {
		t.Fatalf("a save in read asked the caches: %v", asked)
	}
}

func TestACacheRequestIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		file, cache, reason string
	}{
		"an undeclared cache":              {`{}`, "build", "doesn't declare the cache build"},
		"a listed file that doesn't exist": {`{"caches": {"build": {"from": ["deps.json"]}}}`, "build", "lists deps.json"},
		"a file that isn't valid":          {`{"disks": {"build": {}}}`, "build", "isn't valid"},
		"no cache":                         {declares, "Build!", "names no cache"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			cs := &caches{}
			e.svc.Caches = cs
			e.repo.Commit("main", map[string]string{".github/runners.json": tc.file})
			m := e.admit(e.mainRun(), sel2)
			for _, path := range []string{machines.CacheRestorePath, machines.CacheSavePath} {
				code, body := m.cache(path, tc.cache)
				refusedFor(t, path, code, body, tc.reason)
			}
			if asked, _ := cs.all(); len(asked) != 0 {
				t.Fatalf("a refused request asked the caches: %v", asked)
			}
			// The job goes on: its machine isn't stopped.
			if in, _ := e.gce.Instance(m.in.Zone, m.in.Name); in.Status != "RUNNING" {
				t.Fatalf("the machine is %s", in.Status)
			}
		})
	}
	// A service with no caches refuses them.
	e := setup(t)
	e.repo.Commit("main", map[string]string{".github/runners.json": declares})
	m := e.admit(e.mainRun(), sel2)
	code, body := m.cache(machines.CacheRestorePath, "build")
	refusedFor(t, "a restore from a service with no caches", code, body, "keeps no caches")
}

// requests are every request a job may send, as the actions do.
func (m *machine) requests() map[string]func() (int, map[string]any) {
	return map[string]func() (int, map[string]any){
		"work disk": m.disk,
		"restore":   func() (int, map[string]any) { return m.cache(machines.CacheRestorePath, "build") },
		"save":      func() (int, map[string]any) { return m.cache(machines.CacheSavePath, "build") },
		"commit":    func() (int, map[string]any) { return m.cache(machines.CacheCommitPath, "build") },
	}
}

// Only a running machine's admitted job, of a bound run and a repository
// that's still the organization's, is answered.
func TestAJobsRequestsAreAnsweredOnlyWhileItMayHaveThem(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(e *env) *machine
		reason string
	}{
		"a job that wasn't admitted": {func(e *env) *machine {
			j := e.queue(e.mainRun(), sel2)
			e.pass()
			m := e.boot(e.one())
			m.taken(j)
			return m
		}, "no admitted job"},
		"an unbound contribution's job": {func(e *env) *machine {
			crun, _ := e.contribution(false)
			m := e.admit(crun, sel2)
			if adm, _ := e.record("machines/" + m.in.Name + "/admission.json"); adm["unbound"] != true {
				e.t.Fatalf("admission = %v", adm)
			}
			return m
		}, "isn't bound"},
		"a machine that isn't running": {func(e *env) *machine {
			m := e.admit(e.mainRun(), sel2)
			e.gce.Shutdown(m.in.Zone, m.in.Name)
			return m
		}, "has ended"},
		"a job that reported": {func(e *env) *machine {
			m := e.admit(e.mainRun(), sel2)
			m.release(true)
			m.finished()
			if code := m.report(); code != http.StatusNoContent {
				e.t.Fatalf("report: %d", code)
			}
			return m
		}, "has ended"},
		"a job GitHub shows completed, before its machine reports": {func(e *env) *machine {
			m := e.admit(e.mainRun(), sel2)
			j, _, _ := e.gh.JobOnRunner(m.in.Name)
			e.gh.CompleteJobByID(j.ID, "success")
			return m
		}, "has ended"},
		"a transferred repository": {func(e *env) *machine {
			m := e.admit(e.mainRun(), sel2)
			e.gh.Transfer(e.repo, e.gh.Org(4242, "elsewhere"))
			return m
		}, "no longer the organization's"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			cs := &caches{}
			e.svc.Caches = cs
			e.repo.Commit("main", map[string]string{".github/runners.json": declares})
			m := tc.setup(e)
			for what, ask := range m.requests() {
				code, body := ask()
				refusedFor(t, what, code, body, tc.reason)
			}
			if _, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json"); ok || len(e.gce.Disks()) != 1 {
				t.Fatal("a refused request made a work disk")
			}
			if asked, _ := cs.all(); len(asked) != 0 {
				t.Fatalf("a refused request asked the caches: %v", asked)
			}
		})
	}
}

// A job's requests are answered only once GitHub confirms that the job is
// still running: while GitHub can't say, the caller is told to try again.
func TestAJobsRequestsWaitForGitHubToConfirmItRuns(t *testing.T) {
	e := setup(t)
	cs := &caches{}
	e.svc.Caches = cs
	e.repo.Commit("main", map[string]string{".github/runners.json": declares})
	m := e.admit(e.mainRun(), sel2)
	e.gh.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 4, Respond: fault.Status(http.StatusBadGateway, nil, `{"message": "Bad Gateway"}`),
		Match: func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/actions/jobs/") }})
	for what, ask := range m.requests() {
		if code, body := ask(); code != http.StatusServiceUnavailable || body["retryable"] != true {
			t.Fatalf("%s with GitHub failing: %d %v; want 503, retryable", what, code, body)
		}
	}
	// GitHub showing the job queued, behind what the machine's check saw,
	// confirms nothing either.
	j, run, _ := e.gh.JobOnRunner(m.in.Name)
	queued := fmt.Sprintf(`{"id": %d, "run_id": %d, "status": "queued"}`, j.ID, run.ID)
	e.gh.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 4, Respond: fault.Status(http.StatusOK, http.Header{"Content-Type": {"application/json"}}, queued),
		Match: func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/actions/jobs/") }})
	for what, ask := range m.requests() {
		if code, body := ask(); code != http.StatusServiceUnavailable || body["retryable"] != true {
			t.Fatalf("%s with GitHub showing the job queued: %d %v; want 503, retryable", what, code, body)
		}
	}
	if _, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json"); ok {
		t.Fatal("a request GitHub didn't confirm made a work disk")
	}
	if asked, _ := cs.all(); len(asked) != 0 {
		t.Fatalf("a request GitHub didn't confirm asked the caches: %v", asked)
	}
	if restored := m.diskReady(); restored != machines.NotRestored {
		t.Fatalf("restored = %q", restored)
	}
	if code, body := m.cache(machines.CacheSavePath, "build"); code != http.StatusOK || body["saves"] != true {
		t.Fatalf("save: %d %v", code, body)
	}
	// A job GitHub doesn't show at all, in a repository it shows as the
	// organization's, has gone.
	e.gh.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 8, Respond: fault.Status(http.StatusNotFound, nil, `{"message": "Not Found"}`),
		Match: func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/actions/jobs/") }})
	for what, ask := range m.requests() {
		code, body := ask()
		refusedFor(t, what+" for a job GitHub doesn't show", code, body, "has ended")
	}
}

// workDisk is the machine's work disk in Compute Engine, if it's there.
func (e *env) workDisk(m *machine) (gceemu.DiskState, bool) {
	for _, d := range e.gce.Disks() {
		if d.Name == machines.WorkDisk(m.in.Name) {
			return d, true
		}
	}
	return gceemu.DiskState{}, false
}

// A job's work disk is the size and performance it asks for, and the
// service's defaults for what it leaves out.
func TestAWorkDiskIsWhatItsJobAsksFor(t *testing.T) {
	for name, tc := range map[string]struct {
		ask                    map[string]any
		size, iops, throughput int64
	}{
		"everything": {map[string]any{"size_gb": 500, "iops": 10000, "throughput_mibps": 2400}, 500, 10000, 2400},
		"only size":  {map[string]any{"size_gb": 200}, 200, 3000, 400},
		"nothing":    {map[string]any{}, 50, 3000, 400},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			m := e.admit(e.mainRun(), sel2)
			e.eventually("the work disk is ready", func() bool {
				code, body := m.diskAs(tc.ask)
				if code != http.StatusOK && code != http.StatusServiceUnavailable {
					t.Fatalf("work disk request: %d %v", code, body)
				}
				return body["state"] == machines.DiskStateReady
			})
			d, ok := e.workDisk(m)
			if !ok || d.SizeGB != tc.size || d.IOPS != tc.iops || d.Throughput != tc.throughput {
				t.Fatalf("work disk = %+v; want %d GB, %d IOPS and %d MiB/s", d, tc.size, tc.iops, tc.throughput)
			}
			// Asking again for the same is answered about the same disk;
			// asking for other settings is refused, since a job has one
			// work disk.
			if code, body := m.diskAs(tc.ask); code != http.StatusOK || body["state"] != machines.DiskStateReady {
				t.Fatalf("again: %d %v", code, body)
			}
			code, body := m.diskAs(map[string]any{"size_gb": tc.size + 1})
			refusedFor(t, "other settings", code, body, "a job has one work disk")
		})
	}
}

// What the service can tell is wrong with a request, it refuses before
// any disk is made: settings that aren't positive whole numbers, and a size
// Compute Engine reports its disk type, or the job's machine type, doesn't
// allow.
func TestAWorkDiskThatCantBeMadeIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		ask    map[string]any
		setup  func(e *env)
		reason string
	}{
		"no size":            {ask: map[string]any{"size_gb": 0}, reason: "size_gb must be a positive whole number"},
		"negative IOPS":      {ask: map[string]any{"iops": -1}, reason: "iops must be a positive whole number"},
		"no throughput":      {ask: map[string]any{"throughput_mibps": 0}, reason: "throughput_mibps must be a positive whole number"},
		"a fraction":         {ask: map[string]any{"size_gb": 1.5}, reason: "size_gb must be a positive whole number"},
		"null":               {ask: map[string]any{"iops": nil}, reason: "iops must be a positive whole number"},
		"a string":           {ask: map[string]any{"size_gb": "50"}, reason: "size_gb must be a positive whole number"},
		"an exponent":        {ask: map[string]any{"iops": json.RawMessage("1e4")}, reason: "iops must be a positive whole number"},
		"too large a number": {ask: map[string]any{"size_gb": json.RawMessage("9223372036854775808")}, reason: "size_gb must be a positive whole number"},
		"too small":          {ask: map[string]any{"size_gb": 3}, reason: "Compute Engine makes hyperdisk-balanced disks of 4 to 65536 GB"},
		"too large":          {ask: map[string]any{"size_gb": 65537}, reason: "Compute Engine makes hyperdisk-balanced disks of 4 to 65536 GB"},
		"more than it can hold": {
			ask:    map[string]any{"size_gb": 1000},
			setup:  func(e *env) { e.gce.SetMaxDisksSize("n4-highcpu-2", 1000) },
			reason: "lets a n4-highcpu-2 machine's disks hold 1000 GB in all, and its boot disk is 100 GB",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			m := e.admit(e.mainRun(), sel2)
			code, body := m.diskAs(tc.ask)
			refusedFor(t, name, code, body, tc.reason)
			if _, ok := e.workDisk(m); ok {
				t.Fatal("a work disk was made")
			}
			if _, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json"); ok {
				t.Fatal("a work disk was recorded")
			}
		})
	}
}

// What Compute Engine refuses to make, such as more IOPS than a disk of its
// size may have, fails the job's request with Compute Engine's reason, and
// every later request with the same, without asking Compute Engine again.
func TestAWorkDiskComputeEngineRefusesFailsTheRequest(t *testing.T) {
	e := setup(t)
	m := e.admit(e.mainRun(), sel2)
	var mu sync.Mutex
	inserts := 0
	e.gce.Faults().Add(fault.Rule{Method: http.MethodPost, Match: func(r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/disks") {
			mu.Lock()
			inserts++
			mu.Unlock()
		}
		return false
	}})
	ask := map[string]any{"size_gb": 50, "iops": 30000}
	code, body := m.diskAs(ask)
	refusedFor(t, "too many IOPS", code, body, "Compute Engine refused the work disk: Invalid value for field 'resource.provisionedIops': '30000'")
	if _, ok := e.workDisk(m); ok {
		t.Fatal("a work disk was made")
	}
	rec, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json")
	if !ok || rec["state"] != machines.DiskRefused || rec["iops"] != float64(30000) {
		t.Fatalf("disk record = %v; want it refused", rec)
	}
	code, again := m.diskAs(ask)
	refusedFor(t, "again", code, again, fmt.Sprint(body["reason"]))
	mu.Lock()
	defer mu.Unlock()
	if inserts != 1 {
		t.Fatalf("Compute Engine was asked to make the disk %d times; want once", inserts)
	}
}

// Compute Engine can refuse a disk as invalid in its operation, once it
// has accepted the request, as well as at once: that too is a refusal.
func TestAWorkDiskRefusedInItsOperation(t *testing.T) {
	e := setup(t)
	m := e.admit(e.mainRun(), sel2)
	e.gce.FailNextOperation("disks.insert", "INVALID_FIELD_VALUE")
	code, body := m.disk()
	refusedFor(t, "refused in its operation", code, body, "Compute Engine refused the work disk: ")
	rec, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json")
	if !ok || rec["state"] != machines.DiskRefused {
		t.Fatalf("disk record = %v; want it refused", rec)
	}
}

// Two requests at once that Compute Engine both refuses are both refused,
// whichever records the refusal.
func TestAWorkDiskRefusedToTwoRequestsAtOnce(t *testing.T) {
	e := setup(t)
	m := e.admit(e.mainRun(), sel2)
	// Each insert is held a second, so both requests read the record, and
	// ask Compute Engine, before either records the refusal. The rule's
	// match only counts: the injector holds its lock while it matches.
	var mu sync.Mutex
	arrived := 0
	e.gce.Faults().Add(fault.Rule{Method: http.MethodPost, Delay: time.Second, Match: func(r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/disks") {
			return false
		}
		mu.Lock()
		arrived++
		mu.Unlock()
		return true
	}})
	ask := map[string]any{"size_gb": 50, "iops": 30000}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	bodies := make([]map[string]any, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], bodies[i] = m.diskAs(ask)
		}()
	}
	wg.Wait()
	mu.Lock()
	n := arrived
	mu.Unlock()
	if n != 2 {
		t.Fatalf("Compute Engine was asked %d times; the test needs both requests to ask", n)
	}
	for i := range 2 {
		refusedFor(t, fmt.Sprintf("request %d", i), codes[i], bodies[i], "Compute Engine refused the work disk: ")
	}
}

// A disk recorded before jobs asked for their disks' settings asked for
// none: a request that names settings is refused, since the service can't
// tell they're the disk's, and one that names none carries on with it.
func TestAWorkDiskRecordedBeforeSettings(t *testing.T) {
	e := setup(t)
	m := e.admit(e.mainRun(), sel2)
	j, _, _ := e.gh.JobOnRunner(m.in.Name)
	rec, _ := json.Marshal(machines.DiskRecord{Disk: machines.WorkDisk(m.in.Name), Machine: m.in.Name, Zone: m.in.Zone,
		RepositoryID: e.repo.ID, JobID: j.ID, State: machines.DiskMaking, Restored: machines.NotRestored, SizeGB: 50,
		Nonce: "0123456789abcdef", At: time.Now()})
	e.gcs.Put("state", "disks/"+machines.WorkDisk(m.in.Name)+".json", rec)
	code, body := m.diskAs(map[string]any{"size_gb": 500, "iops": 10000})
	refusedFor(t, "settings against an older record", code, body, "before the service took a disk's settings")
	if restored := m.diskReady(); restored != machines.NotRestored {
		t.Fatalf("restored = %q", restored)
	}
	if d, ok := e.workDisk(m); !ok || d.SizeGB != 50 || d.IOPS != 3000 || d.Throughput != 400 {
		t.Fatalf("work disk = %+v; want the older record's, at the service's defaults", d)
	}
}
