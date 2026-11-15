package machines_test

import (
	"context"
	"net/http"

	gce "google.golang.org/api/compute/v1"

	"github.com/lanescape/runners/emulators/fault"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/internal/machines"
)

func usage() *machines.Usage {
	from := time.Date(2026, 11, 15, 17, 0, 0, 0, time.UTC)
	return &machines.Usage{From: from, To: from.Add(30 * time.Minute), CPUs: 80, CPUBusyMS: 98765432, MemTotal: 80 << 30,
		MemPeak: 61 << 30, DiskRead: 1 << 30, DiskWritten: 2 << 30, WorkSize: 500 << 30, WorkPeak: 310 << 30,
		Pressure: &machines.Pressure{MemorySome: 1234}, Samples: 1800}
}

// admittedMachine is a machine whose job was admitted and has finished.
func admittedMachine(t *testing.T, e *env) (*machine, int64, int64) {
	t.Helper()
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	if code := m.started(run.ID); code != http.StatusOK {
		t.Fatalf("job-started check: %d", code)
	}
	m.release(true)
	m.finished()
	return m, run.ID, j.ID
}

// usageLines are the "job usage" log entries.
func usageLines(e *env) []string {
	var out []string
	for l := range strings.Lines(e.logs.String()) {
		if strings.Contains(l, `msg="job usage"`) {
			out = append(out, l)
		}
	}
	return out
}

// The report's usage is recorded with it, and logged as "job usage", with
// the job its check admitted, as the machine's records are dropped
// (specs/0002-machines.md, What a job used). The first report recorded is
// the record, so a job's own report can't change what's logged.
func TestWhatAJobUsedIsRecordedAndLogged(t *testing.T) {
	e := setup(t)
	m, runID, jobID := admittedMachine(t, e)
	if code, _ := e.post("/v1/jobs/finished", e.token(m.in), machines.FinishedRequest{Usage: usage()}); code != http.StatusNoContent {
		t.Fatalf("report: %d", code)
	}
	other := usage()
	other.MemPeak = 1
	if code, _ := e.post("/v1/jobs/finished", e.token(m.in), machines.FinishedRequest{Usage: other}); code != http.StatusNoContent {
		t.Fatalf("a second report: %d", code)
	}
	rep, ok := e.record("machines/" + m.in.Name + "/report.json")
	u, _ := rep["usage"].(map[string]any)
	if !ok || u["mem_peak"] != float64(61<<30) || u["cpus"] != float64(80) {
		t.Fatalf("report = %v", rep)
	}
	if len(usageLines(e)) != 0 {
		t.Fatal("usage was logged before the machine's records were dropped")
	}
	e.pass()
	e.pass()
	if e.exists(m.in.Name) || len(e.recordsOf(m.in.Name)) != 0 {
		t.Fatalf("the machine or its records are left: %v", e.recordsOf(m.in.Name))
	}
	lines := usageLines(e)
	if len(lines) != 1 {
		t.Fatalf("logged %d times, want once:\n%s", len(lines), e.logs.String())
	}
	for _, want := range []string{"machine=" + m.in.Name, "zone=" + m.in.Zone, "machine_type=", "spot=", "selector=" + sel2,
		"repository_id=5001", "run_id=" + itoa(runID), "run_attempt=1", "job_id=" + itoa(jobID),
		"usage.mem_peak=" + itoa(61<<30), "usage.seconds=1800", "usage.pressure.memory_some_ms=1234", "usage.cpu_busy_ms=98765432"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("job usage log lacks %s:\n%s", want, lines[0])
		}
	}
	// What wasn't measured isn't logged as a measurement.
	if strings.Contains(lines[0], "oom_kills") || strings.Contains(lines[0], "root_size") {
		t.Fatalf("job usage log has what wasn't measured:\n%s", lines[0])
	}
}

// A report recorded whose answer was lost, and that's never sent again, is
// logged all the same.
func TestUsageWhoseRecordsAnswerWasLost(t *testing.T) {
	e := setup(t)
	m, _, jobID := admittedMachine(t, e)
	var lost atomic.Int32
	// Written, then an answer the client doesn't retry.
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1, After: true,
		Match: func(r *http.Request) bool {
			if strings.Contains(r.URL.RawQuery, "report.json") {
				lost.Add(1)
				return true
			}
			return false
		},
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	if code, _ := e.post("/v1/jobs/finished", e.token(m.in), machines.FinishedRequest{Usage: usage()}); code != http.StatusServiceUnavailable {
		t.Fatalf("report whose answer was lost: %d", code)
	}
	if lost.Load() != 1 {
		t.Fatal("no answer was lost")
	}
	e.pass()
	if lines := usageLines(e); len(lines) != 1 || !strings.Contains(lines[0], "job_id="+itoa(jobID)) {
		t.Fatalf("job usage = %q", lines)
	}
}

// A report whose job can't be read is sent again, not recorded without it.
func TestUsageWhoseJobCantBeRead(t *testing.T) {
	e := setup(t)
	m, _, jobID := admittedMachine(t, e)
	// An error the client doesn't retry by itself, as it would a 503.
	var refused atomic.Int32
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1,
		Match: func(r *http.Request) bool {
			if strings.Contains(r.URL.Path, "admission.json") {
				refused.Add(1)
				return true
			}
			return false
		},
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	if code, _ := e.post("/v1/jobs/finished", e.token(m.in), machines.FinishedRequest{Usage: usage()}); code != http.StatusServiceUnavailable {
		t.Fatalf("report whose job couldn't be read: %d", code)
	}
	if _, ok := e.record("machines/" + m.in.Name + "/report.json"); ok || refused.Load() != 1 {
		t.Fatalf("a report whose job couldn't be read was recorded, or its job was refused %d times", refused.Load())
	}
	if code, _ := e.post("/v1/jobs/finished", e.token(m.in), machines.FinishedRequest{Usage: usage()}); code != http.StatusNoContent {
		t.Fatalf("report sent again: %d", code)
	}
	if rep, _ := e.record("machines/" + m.in.Name + "/report.json"); rep["job_id"] != float64(jobID) {
		t.Fatalf("report = %v", rep)
	}
}

// The report holds its job, so it's logged with it even when the
// admission went first, as it does when a report arrives as a pass drops
// the machine's records; and a report a pass can't read is left for a
// later one, not dropped unlogged.
func TestUsageOfAReportReadLate(t *testing.T) {
	e := setup(t)
	m, runID, jobID := admittedMachine(t, e)
	if code, _ := e.post("/v1/jobs/finished", e.token(m.in), machines.FinishedRequest{Usage: usage()}); code != http.StatusNoContent {
		t.Fatalf("report: %d", code)
	}
	// The machine goes, by Compute Engine's hand.
	api, err := gce.NewService(context.Background(), e.gce.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Instances.Delete(e.gce.Project, m.in.Zone, m.in.Name).Do(); err != nil {
		t.Fatal(err)
	}
	key := "machines/" + m.in.Name + "/admission.json"
	_, gen, _ := e.gcs.Object("state", key)
	if err := e.svc.Objects.Delete(context.Background(), key, gen); err != nil {
		t.Fatal(err)
	}
	var refused atomic.Int32
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1,
		Match: func(r *http.Request) bool {
			if strings.Contains(r.URL.Path, "report.json") {
				refused.Add(1)
				return true
			}
			return false
		},
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	e.pass()
	if refused.Load() != 1 {
		t.Fatalf("the report was refused %d times", refused.Load())
	}
	if _, ok := e.record("machines/" + m.in.Name + "/report.json"); !ok || len(usageLines(e)) != 0 {
		t.Fatal("a report the pass couldn't read was dropped, or logged")
	}
	e.pass()
	lines := usageLines(e)
	if len(lines) != 1 || !strings.Contains(lines[0], "job_id="+itoa(jobID)) || !strings.Contains(lines[0], "run_id="+itoa(runID)) {
		t.Fatalf("job usage = %q", lines)
	}
	if rs := e.recordsOf(m.in.Name); len(rs) != 0 {
		t.Fatalf("records left: %v", rs)
	}
}

// A report that outlives its machine, as one whose request stalled while a
// pass deleted the machine and its records, isn't recorded, which would
// log its usage without its job; a machine that had no job admitted still
// reports what it used.
func TestAReportThatOutlivesItsMachine(t *testing.T) {
	e := setup(t)
	m, _, _ := admittedMachine(t, e)
	key := "machines/" + m.in.Name + "/admission.json"
	_, gen, _ := e.gcs.Object("state", key)
	if err := e.svc.Objects.Delete(context.Background(), key, gen); err != nil {
		t.Fatal(err)
	}
	// The machine is there as the request is checked, and gone once its
	// admission is found missing.
	var gets atomic.Int32
	e.gce.Faults().Add(fault.Rule{Method: http.MethodGet,
		Match: func(r *http.Request) bool {
			return strings.HasSuffix(r.URL.Path, "/instances/"+m.in.Name) && gets.Add(1) == 2
		},
		Respond: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error": {"code": 404, "message": "not found", "errors": [{"reason": "notFound", "message": "not found"}]}}`))
		}})
	if code, _ := e.post("/v1/jobs/finished", e.token(m.in), machines.FinishedRequest{Usage: usage()}); code != http.StatusForbidden {
		t.Fatalf("a report that outlived its machine: %d", code)
	}
	if _, ok := e.record("machines/" + m.in.Name + "/report.json"); ok {
		t.Fatal("a report that outlived its machine was recorded")
	}

	idle := e.boot(e.one())
	if code, _ := e.post("/v1/jobs/finished", e.token(idle.in), machines.FinishedRequest{Usage: usage()}); code != http.StatusNoContent {
		t.Fatalf("report of a machine that had no job: %d", code)
	}
	if rep, _ := e.record("machines/" + idle.in.Name + "/report.json"); rep["usage"] == nil || rep["job_id"] != nil {
		t.Fatalf("report = %v", rep)
	}
}

// A report the age limit's cleanup can't read is left for a later pass, not
// dropped unlogged.
func TestUsageOfAReportReadAfterTheAgeLimit(t *testing.T) {
	e := setup(t)
	m, _, jobID := admittedMachine(t, e)
	e.gce.Faults().Add(fault.Rule{Method: http.MethodDelete, Match: func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/instances/") },
		Respond: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }})
	if code, _ := e.post("/v1/jobs/finished", e.token(m.in), machines.FinishedRequest{Usage: usage()}); code != http.StatusNoContent {
		t.Fatalf("report: %d", code)
	}
	e.pass()
	if !e.exists(m.in.Name) {
		t.Fatal("the machine was deleted despite the fault")
	}
	e.clock.Advance(machines.MaxAge)
	e.gce.Faults().Reset()
	var refused atomic.Int32
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1,
		Match: func(r *http.Request) bool {
			if strings.Contains(r.URL.Path, "report.json") {
				refused.Add(1)
				return true
			}
			return false
		},
		Respond: gcsemu.JSONError(http.StatusForbidden, "forbidden", "Forbidden")})
	e.pass()
	if e.exists(m.in.Name) || refused.Load() != 1 {
		t.Fatalf("the machine is there, or its report was refused %d times", refused.Load())
	}
	if _, ok := e.record("machines/" + m.in.Name + "/report.json"); !ok || len(usageLines(e)) != 0 {
		t.Fatal("a report the pass couldn't read was dropped, or logged")
	}
	e.pass()
	if lines := usageLines(e); len(lines) != 1 || !strings.Contains(lines[0], "job_id="+itoa(jobID)) {
		t.Fatalf("job usage = %q", lines)
	}
	if rs := e.recordsOf(m.in.Name); len(rs) != 0 {
		t.Fatalf("records left: %v", rs)
	}
}

// Usage that no machine could have measured is left out, and the report is
// recorded without it: the report itself mustn't be lost.
func TestUsageNoMachineCouldMeasure(t *testing.T) {
	e := setup(t)
	m, _, _ := admittedMachine(t, e)
	u := usage()
	u.MemPeak = -1
	if code, _ := e.post("/v1/jobs/finished", e.token(m.in), machines.FinishedRequest{Usage: u}); code != http.StatusNoContent {
		t.Fatalf("report: %d", code)
	}
	rep, ok := e.record("machines/" + m.in.Name + "/report.json")
	if _, has := rep["usage"]; !ok || has {
		t.Fatalf("report = %v", rep)
	}
	// A field the service doesn't know is refused, as in any request.
	if code, _ := e.post("/v1/jobs/finished", e.token(m.in), map[string]any{"usage": map[string]any{"from": u.From, "gpu": 1}}); code != http.StatusBadRequest {
		t.Fatalf("usage with a field the service doesn't know: %d", code)
	}
	e.pass()
	if e.exists(m.in.Name) || e.logged(`msg="job usage"`) || !e.logged("recording the report without it") {
		t.Fatal("the machine wasn't deleted, usage no machine could measure was logged, or its omission wasn't")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
