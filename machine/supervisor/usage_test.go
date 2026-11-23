package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lanescape/runners/internal/machines"
)

// A procfs's files, as the kernel writes them, at a job's start and end.
var startProc = map[string]string{
	"stat": "cpu  1000 10 200 9000 50 0 5 7 0 0\ncpu0 500 5 100 4500 25 0 2 3 0 0\ncpu1 500 5 100 4500 25 0 3 4 0 0\nintr 1 2 3\n",
	"meminfo": "MemTotal:       8000000 kB\nMemFree:        6000000 kB\nMemAvailable:   7000000 kB\n" +
		"SwapTotal:            0 kB\nSwapFree:             0 kB\n",
	"vmstat":          "nr_free_pages 1\noom_kill 2\n",
	"diskstats":       "   8       0 sda 10 0 1000 0 20 0 2000 0 0 0 0\n   8       1 sda1 10 0 1000 0 20 0 2000 0 0 0 0\n   7       0 loop0 5 0 500 0 0 0 0 0 0 0 0\n",
	"pressure/cpu":    "some avg10=0.00 avg60=0.00 avg300=0.00 total=1000000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
	"pressure/memory": "some avg10=0.00 avg60=0.00 avg300=0.00 total=2000000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1000000\n",
	"pressure/io":     "some avg10=0.00 avg60=0.00 avg300=0.00 total=3000000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=2000000\n",
}

// The same at the end: 300 more ticks busy, 20 waiting for I/O and 3
// stolen, a kill for want of memory, a work disk attached during the job
// with its own I/O, and longer stalls.
var endProc = map[string]string{
	"stat":    "cpu  1250 10 240 99000 70 0 15 10 0 0\ncpu0 600 5 120 49500 35 0 7 5 0 0\ncpu1 650 5 120 49500 35 0 8 5 0 0\n",
	"meminfo": startProc["meminfo"],
	"vmstat":  "nr_free_pages 1\noom_kill 3\n",
	"diskstats": "   8       0 sda 10 0 3000 0 20 0 6000 0 0 0 0\n   8       1 sda1 10 0 3000 0 20 0 6000 0 0 0 0\n" +
		" 259       0 nvme0n1 10 0 100 0 20 0 400 0 0 0 0\n   7       0 loop0 5 0 90000 0 0 0 0 0 0 0 0\n",
	"pressure/cpu":    "some avg10=0.00 avg60=0.00 avg300=0.00 total=1500000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
	"pressure/memory": "some avg10=0.00 avg60=0.00 avg300=0.00 total=2250000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1100000\n",
	"pressure/io":     "some avg10=0.00 avg60=0.00 avg300=0.00 total=7000000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=2000000\n",
}

// A moment of the job's when memory peaked: 6,000,000 kB in use, then less
// again before it ended.
const peakMeminfo = "MemTotal:       8000000 kB\nMemFree:         500000 kB\nMemAvailable:   2000000 kB\n" +
	"SwapTotal:            0 kB\nSwapFree:             0 kB\n"

func writeProc(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// steps is a runner's steps that change the test's procfs as a job would
// change the kernel's counters: memory peaks for a while, then falls, and
// the counters move on.
func steps(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	write := func(name, content string) {
		b.WriteString("printf '%s' '" + content + "' > \"$PROC/" + name + ".new\" && mv \"$PROC/" + name + ".new\" \"$PROC/" + name + "\"\n")
	}
	write("meminfo", peakMeminfo)
	b.WriteString("sleep 0.3\n")
	for _, name := range []string{"stat", "meminfo", "vmstat", "diskstats", "pressure/cpu", "pressure/memory", "pressure/io"} {
		write(name, endProc[name])
	}
	return b.String()
}

func (e *env) usage(i int) *Usage {
	e.t.Helper()
	var f struct {
		Usage *Usage `json:"usage"`
	}
	dec := json.NewDecoder(strings.NewReader(e.svc.reports[i]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		e.t.Fatalf("report %q: %v", e.svc.reports[i], err)
	}
	return f.Usage
}

// What the machine used while the runner ran is measured from the kernel's
// counters, at the start and end, and memory's peak from samples between
// (specs/0002-machines.md, What a job used).
func TestTheReportSaysWhatTheMachineUsed(t *testing.T) {
	e := setup(t, jit, steps(t))
	before := time.Now().UTC()
	e.supervise()
	u := e.usage(0)
	if u == nil {
		t.Fatalf("reports = %q", e.svc.reports)
	}
	if u.From.Before(before.Add(-time.Second)) || u.To.Before(u.From) || u.CPUs != 2 {
		t.Fatalf("from %v to %v, %d CPUs", u.From, u.To, u.CPUs)
	}
	// Busy is user, nice, system, irq and softirq: (1250+10+240+15) -
	// (1000+10+200+5) = 300 ticks of 10 ms.
	if u.CPUBusyMS != 3000 || u.CPUIOWaitMS != 200 || u.CPUStealMS != 30 {
		t.Fatalf("CPU busy %d, iowait %d, steal %d ms", u.CPUBusyMS, u.CPUIOWaitMS, u.CPUStealMS)
	}
	if u.MemTotal != 8000000*1024 || u.MemPeak != 6000000*1024 || u.SwapPeak != 0 {
		t.Fatalf("memory total %d, peak %d, swap %d", u.MemTotal, u.MemPeak, u.SwapPeak)
	}
	if u.OOMKills == nil || *u.OOMKills != 1 {
		t.Fatalf("OOM kills = %v", u.OOMKills)
	}
	// Whole disks alone, in 512-byte sectors: sda's 2,000 more read and
	// 4,000 more written, and all of the work disk's, attached during the
	// job; not its partition again, nor a loop device.
	if u.DiskRead != (2000+100)*512 || u.DiskWritten != (4000+400)*512 {
		t.Fatalf("disk read %d, written %d", u.DiskRead, u.DiskWritten)
	}
	want := &Pressure{CPUSome: 500, MemorySome: 250, MemoryFull: 100, IOSome: 4000, IOFull: 0}
	if !reflect.DeepEqual(u.Pressure, want) {
		t.Fatalf("pressure = %+v, want %+v", u.Pressure, want)
	}
	if u.Samples < 3 {
		t.Fatalf("%d samples", u.Samples)
	}
	// The boot log has the same numbers.
	if !strings.Contains(e.out.String(), `used: {"from":`) {
		t.Fatalf("the boot log doesn't say what was used:\n%s", e.out)
	}
}

// A counter that couldn't be read at the start gives nothing, not a total
// since boot: here they can be read only at the end, which the job's steps
// write.
func TestWhatTheKernelDoesntSayIsLeftOut(t *testing.T) {
	e := setup(t, jit, steps(t))
	for _, name := range []string{"pressure/cpu", "pressure/memory", "pressure/io", "vmstat", "diskstats"} {
		if err := os.RemoveAll(filepath.Join(e.cfg.Proc, name)); err != nil {
			t.Fatal(err)
		}
	}
	e.supervise()
	// The job's steps wrote them again, so only the start's readings were
	// missing.
	for _, name := range []string{"pressure/cpu", "pressure/memory", "pressure/io", "vmstat", "diskstats"} {
		if b, err := os.ReadFile(filepath.Join(e.cfg.Proc, name)); err != nil || string(b) != endProc[name] {
			t.Fatalf("%s at the end: %q, %v", name, b, err)
		}
	}
	u := e.usage(0)
	if u == nil || u.Pressure != nil || u.OOMKills != nil || u.DiskRead != 0 || u.DiskWritten != 0 || u.CPUBusyMS != 3000 {
		t.Fatalf("usage = %+v", u)
	}
	for _, field := range []string{"pressure", "oom_kills", "disk_read", "disk_written"} {
		if strings.Contains(e.svc.reports[0], field) {
			t.Fatalf("report = %s", e.svc.reports[0])
		}
	}
}

// A sample whose meminfo lacks MemTotal or MemAvailable records neither
// memory nor swap, never a figure worked out from one that isn't there;
// the samples before it still count.
func TestMeminfoNeedsTotalAndAvailable(t *testing.T) {
	const swap = "SwapTotal:      1000000 kB\nSwapFree:        400000 kB\n"
	for name, content := range map[string]string{
		"no MemAvailable": "MemTotal:       8000000 kB\nMemFree:         500000 kB\n" + swap,
		"no MemTotal":     "MemFree:         500000 kB\nMemAvailable:   2000000 kB\n" + swap,
	} {
		dir := t.TempDir()
		writeProc(t, dir, map[string]string{"meminfo": content})
		m := newMeter(dir, dir, filepath.Join(dir, "work"), time.Second)
		if _, ok := m.meminfo(); ok {
			t.Errorf("%s: meminfo was read", name)
		}
		m.sample()
		if m.u.MemTotal != 0 || m.u.MemPeak != 0 || m.u.SwapPeak != 0 || m.u.Samples != 1 {
			t.Errorf("%s: usage = %+v", name, m.u)
		}
		// A whole sample, with 200,000 kB of swap in use, and then another
		// without, whose 600,000 kB of swap isn't recorded.
		writeProc(t, dir, map[string]string{"meminfo": "MemTotal:       8000000 kB\nMemAvailable:   2000000 kB\n" +
			"SwapTotal:      1000000 kB\nSwapFree:        800000 kB\n"})
		m.sample()
		writeProc(t, dir, map[string]string{"meminfo": content})
		m.sample()
		if m.u.MemTotal != 8000000*1024 || m.u.MemPeak != 6000000*1024 || m.u.SwapPeak != 200000*1024 || m.u.Samples != 3 {
			t.Errorf("%s: after a whole sample, usage = %+v", name, m.u)
		}
	}
}

// Measuring mustn't hold up the report: a file that doesn't answer, as a
// filesystem that hangs wouldn't, leaves it unmeasured, and the report goes
// without it once the runner has exited.
func TestAReportDoesntWaitForMeasuring(t *testing.T) {
	e := setup(t, jit, "")
	fifo := filepath.Join(e.cfg.Proc, "meminfo")
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Let the reader go.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	e.cfg.MeasureFor = duration(300 * time.Millisecond)
	e.supervise()
	if len(e.svc.reports) != 1 || e.svc.reports[0] != "{}" || !e.poweredOff() {
		t.Fatalf("reports = %q, powered off %v", e.svc.reports, e.poweredOff())
	}
	if !strings.Contains(e.out.String(), "wasn't measured in time") {
		t.Fatalf("the log doesn't say why:\n%s", e.out)
	}
}

// A service that doesn't read usage refuses it as a field it doesn't know;
// the report goes again without it, so the job's save isn't lost.
func TestAServiceThatRefusesUsageStillGetsTheReport(t *testing.T) {
	e := setup(t, jit, "")
	e.svc.codes = []int{http.StatusBadRequest}
	e.supervise()
	if len(e.svc.reports) != 2 || e.usage(0) == nil || e.svc.reports[1] != "{}" || !e.poweredOff() {
		t.Fatalf("reports = %q, powered off %v", e.svc.reports, e.poweredOff())
	}
}

// The supervisor's Usage is the service's, field for field, and the
// largest a report could hold fits the service's limit on a machine's
// request.
func TestTheServiceReadsTheUsage(t *testing.T) {
	big := int64(math.MaxInt64)
	now := time.Date(2026, 11, 15, 23, 59, 59, 999999999, time.UTC)
	u := Usage{From: now, To: now, CPUs: math.MaxInt32, CPUBusyMS: big, CPUIOWaitMS: big, CPUStealMS: big,
		MemTotal: big, MemPeak: big, SwapPeak: big, OOMKills: &big, DiskRead: big, DiskWritten: big,
		RootSize: big, RootPeak: big, WorkSize: big, WorkPeak: big,
		Pressure: &Pressure{CPUSome: big, MemorySome: big, MemoryFull: big, IOSome: big, IOFull: big}, Samples: math.MaxInt32}
	b, err := json.Marshal(finished{Usage: &u})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 1<<10 {
		t.Fatalf("the largest report is %d bytes, more than the service reads", len(b))
	}
	var got machines.FinishedRequest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	back, _ := json.Marshal(got)
	if !bytes.Equal(back, b) {
		t.Fatalf("the service reads\n%s\nas\n%s", b, back)
	}
	s, m := reflect.TypeFor[Usage](), reflect.TypeFor[machines.Usage]()
	if s.NumField() != m.NumField() {
		t.Fatalf("%d fields here, %d in the service's", s.NumField(), m.NumField())
	}
}
