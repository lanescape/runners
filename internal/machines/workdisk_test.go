package machines_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// chosen starts every work disk from a base image, as if it were a saved
// disk of its family larger than the shape's, and records what it was asked
// for.
type chosen struct {
	mu    sync.Mutex
	asked []machines.Machine
}

func (c *chosen) Choose(_ context.Context, m machines.Machine, _ machines.Admission, size int64) (string, int64, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, m)
	return "lsr-base-amd64-new", size + 20, "pull_request_exact", nil
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
	e.repo.Commit("feature", map[string]string{".github/runners.json": declares})
	p := e.gh.PullRequest(e.repo, "main", e.repo, "feature", e.org)
	run := e.gh.PullRequestRun(p)
	m := e.admit(run, sel2)
	j, _, _ := e.gh.JobOnRunner(m.in.Name)
	if restored := m.diskReady("build"); restored != "pull_request_exact" {
		t.Fatalf("restored = %q", restored)
	}
	// The job chose the family; the service chose the rest, in the scope it
	// bound the run to.
	asked := src.all()
	if len(asked) != 1 || asked[0].Name != m.in.Name || asked[0].Selector.Scope.String() != "pr"+strconv.Itoa(p.Number) ||
		asked[0].Selector.Family != "build" || asked[0].Selector.Key == "" {
		t.Fatalf("chosen for %+v; want the run's scope, the family and its key", asked)
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
		rec["family"] != "build" || rec["key"] != asked[0].Selector.Key || rec["restored"] != "pull_request_exact" {
		t.Fatalf("disk record = %v", rec)
	}
	// Asking again is answered about the same disk, and nothing more.
	code, body := m.disk("build")
	if code != http.StatusOK || body["state"] != machines.DiskStateReady || body["restored"] != "pull_request_exact" {
		t.Fatalf("again: %d %v", code, body)
	}
	if n := len(e.gce.Disks()); n != 2 || len(src.all()) != 1 {
		t.Fatalf("%d disks, %d choices; want the boot and work disks, chosen once", n, len(src.all()))
	}
	// A job has one work disk.
	code, body = m.disk("other")
	refusedFor(t, "another family", code, body, "of the family build")
	if n := len(e.gce.Disks()); n != 2 {
		t.Fatalf("%d disks after another family's request", n)
	}
}

func TestAnEmptyWorkDisk(t *testing.T) {
	e := setup(t)
	e.repo.Commit("main", map[string]string{".github/runners.json": declares})
	m := e.admit(e.mainRun(), sel2)
	if restored := m.diskReady("build"); restored != "none" {
		t.Fatalf("restored = %q; with no disk source, every disk starts empty", restored)
	}
	for _, d := range e.gce.Disks() {
		if d.Name == machines.WorkDisk(m.in.Name) && (d.SizeGB != 50 || d.SourceImage != "") {
			t.Fatalf("work disk = %+v; want an empty disk of the shape's size", d)
		}
	}
}

func TestAWorkDiskIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		file, family, reason string
	}{
		"an undeclared family":             {`{}`, "build", "doesn't declare the family build"},
		"a listed file that doesn't exist": {`{"disks": {"build": {"from": ["deps.json"]}}}`, "build", "lists deps.json"},
		"a file that isn't valid":          {`{"disk": {}}`, "build", "isn't valid"},
		"no family":                        {declares, "Build!", "names no family"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.repo.Commit("main", map[string]string{".github/runners.json": tc.file})
			m := e.admit(e.mainRun(), sel2)
			code, body := m.disk(tc.family)
			refusedFor(t, "work disk request", code, body, tc.reason)
			// Asking again is refused again.
			code, body = m.disk(tc.family)
			refusedFor(t, "again", code, body, tc.reason)
			if _, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json"); ok || len(e.gce.Disks()) != 1 {
				t.Fatal("a refused request made a work disk")
			}
			// The job goes on: its machine isn't stopped.
			if in, _ := e.gce.Instance(m.in.Zone, m.in.Name); in.Status != "RUNNING" {
				t.Fatalf("the machine is %s", in.Status)
			}
		})
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
		"a transferred repository": {func(e *env) *machine {
			m := e.admit(e.mainRun(), sel2)
			e.gh.Transfer(e.repo, e.gh.Org(4242, "elsewhere"))
			return m
		}, "no longer the organization's"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.repo.Commit("main", map[string]string{".github/runners.json": declares})
			m := tc.setup(e)
			code, body := m.disk("build")
			refusedFor(t, "work disk request", code, body, tc.reason)
			code, body = m.save()
			refusedFor(t, "save request", code, body, tc.reason)
			if _, ok := e.record("disks/" + machines.WorkDisk(m.in.Name) + ".json"); ok || len(e.gce.Disks()) != 1 {
				t.Fatal("a refused request made a work disk")
			}
			if _, ok := e.record("machines/" + m.in.Name + "/save-request.json"); ok {
				t.Fatal("a refused request recorded a save")
			}
		})
	}
}

func TestASaveRequest(t *testing.T) {
	t.Run("in a scope that saves", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("main", map[string]string{".github/runners.json": declares})
		m := e.admit(e.mainRun(), sel2)
		m.diskReady("build")
		code, body := m.save()
		if code != http.StatusOK || body["saves"] != true || body["reason"] != nil {
			t.Fatalf("save request: %d %v, want 200 saves", code, body)
		}
		rec, ok := e.record("machines/" + m.in.Name + "/save-request.json")
		if !ok || rec["machine"] != m.in.Name || rec["job_id"] == nil {
			t.Fatalf("save request record = %v", rec)
		}
		// Asking again is answered the same, and the first record stands.
		e.clock.Advance(time.Second)
		if code, body := m.save(); code != http.StatusOK || body["saves"] != true {
			t.Fatalf("again: %d %v", code, body)
		}
		if again, _ := e.record("machines/" + m.in.Name + "/save-request.json"); again["at"] != rec["at"] {
			t.Fatalf("save request record = %v, was %v", again, rec)
		}
	})
	t.Run("in read", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("feature", map[string]string{".github/runners.json": declares})
		m := e.admit(e.gh.PushRun(e.repo, "feature"), sel2)
		if a, _, _ := e.svc.AdmissionOf(context.Background(), m.in.Name); !a.Scope.IsRead() {
			t.Fatalf("admission = %+v; want the run bound read", a)
		}
		m.diskReady("build")
		code, body := m.save()
		if code != http.StatusOK || body["saves"] != false || !strings.Contains(fmt.Sprint(body["reason"]), "read") {
			t.Fatalf("save request: %d %v, want 200 saving nothing, saying why", code, body)
		}
	})
	t.Run("with no work disk", func(t *testing.T) {
		e := setup(t)
		m := e.admit(e.mainRun(), sel2)
		code, body := m.save()
		refusedFor(t, "save request", code, body, "no work disk")
		if _, ok := e.record("machines/" + m.in.Name + "/save-request.json"); ok {
			t.Fatal("a refused save was recorded")
		}
	})
	t.Run("not asking to save", func(t *testing.T) {
		e := setup(t)
		e.repo.Commit("main", map[string]string{".github/runners.json": declares})
		m := e.admit(e.mainRun(), sel2)
		m.diskReady("build")
		for _, b := range []any{map[string]bool{"save": false}, map[string]any{}} {
			code, body := e.post(machines.SavePath, e.token(m.in), b)
			refusedFor(t, fmt.Sprintf("save request %v", b), code, body, `{"save": true}`)
		}
		if _, ok := e.record("machines/" + m.in.Name + "/save-request.json"); ok {
			t.Fatal("a refused save was recorded")
		}
	})
}

// saver records the work disks it's asked to save.
type saver struct {
	mu    sync.Mutex
	saved []machines.Machine
}

func (s *saver) Save(_ context.Context, m machines.Machine, _ machines.Admission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, m)
	return nil
}

func (s *saver) all() []machines.Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]machines.Machine(nil), s.saved...)
}

// A work disk is saved only if its job asked, its supervisor reported, GitHub's
// conclusion is success, and its scope saves.
func TestAWorkDiskIsSavedOnlyWhenItMayBe(t *testing.T) {
	for name, tc := range map[string]struct {
		branch                string
		ask, succeed, reports bool
		saved                 bool
	}{
		"asked, succeeded and reported":     {"main", true, true, true, true},
		"not asked":                         {"main", false, true, true, false},
		"asked, but the job failed":         {"main", true, false, true, false},
		"asked, but no report":              {"main", true, true, false, false},
		"asked, in a scope that saves none": {"feature", true, true, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			sv := &saver{}
			e.svc.Saves = sv
			e.repo.Commit(tc.branch, map[string]string{".github/runners.json": declares})
			m := e.admit(e.gh.PushRun(e.repo, tc.branch), sel2)
			m.diskReady("build")
			if tc.ask {
				if code, _ := m.save(); code != http.StatusOK {
					t.Fatalf("save request: %d", code)
				}
			}
			j, _, _ := e.gh.JobOnRunner(m.in.Name)
			m.release(tc.succeed)
			m.finished()
			if c := e.gh.Job(j.ID).Conclusion; (c == "success") != tc.succeed {
				t.Fatalf("the job's conclusion is %s", c)
			}
			if tc.reports {
				if code := m.report(); code != http.StatusNoContent {
					t.Fatalf("report: %d", code)
				}
			} else {
				e.clock.Advance(machines.ReportWait + time.Second)
			}
			e.pass()
			if e.exists(m.in.Name) {
				t.Fatal("the machine wasn't deleted")
			}
			saved := sv.all()
			if !tc.saved {
				if len(saved) != 0 {
					t.Fatalf("saved %+v", saved)
				}
				return
			}
			if len(saved) != 1 || saved[0].Name != m.in.Name || saved[0].Selector.Scope.String() != "main" ||
				saved[0].Selector.Family != "build" || saved[0].Selector.Key == "" {
				t.Fatalf("saved %+v; want the job's work disk, resolved with its run's scope, family and key", saved)
			}
		})
	}
}
