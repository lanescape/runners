package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Usage is what the machine used while its runner ran (specs/0002-machines.md,
// What a job used), measured from the kernel's own counters for the whole
// machine, which serves that one job. It holds numbers alone: no process,
// command line, path or anything else a job's steps name. What the kernel
// doesn't offer, or a counter that couldn't be read at the start, is left
// out, as zero, or as nil for OOMKills and Pressure, whose zero means
// something.
type Usage struct {
	// From and To are when the runner started and exited, by the
	// machine's clock.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	CPUs int       `json:"cpus,omitempty"`
	// CPU time, summed over every CPU: running anything, idle waiting for
	// I/O, and taken by the hypervisor.
	CPUBusyMS   int64 `json:"cpu_busy_ms,omitempty"`
	CPUIOWaitMS int64 `json:"cpu_iowait_ms,omitempty"`
	CPUStealMS  int64 `json:"cpu_steal_ms,omitempty"`
	// Memory: the machine's total, and the most in use at any sample, as
	// the total less what the kernel could make available without
	// swapping, so that the page cache doesn't count.
	MemTotal int64 `json:"mem_total,omitempty"`
	MemPeak  int64 `json:"mem_peak,omitempty"`
	SwapPeak int64 `json:"swap_peak,omitempty"`
	// OOMKills is how many processes the kernel killed for want of memory.
	OOMKills *int64 `json:"oom_kills,omitempty"`
	// Bytes read from and written to the machine's disks.
	DiskRead    int64 `json:"disk_read,omitempty"`
	DiskWritten int64 `json:"disk_written,omitempty"`
	// The boot disk's filesystem, and the work disk's once it's mounted:
	// its size, and the most in use at any sample.
	RootSize int64 `json:"root_size,omitempty"`
	RootPeak int64 `json:"root_peak,omitempty"`
	WorkSize int64 `json:"work_size,omitempty"`
	WorkPeak int64 `json:"work_peak,omitempty"`
	// Pressure is how long some, or all, of the machine's runnable work
	// was stalled for want of CPU, memory or I/O, from the kernel's
	// pressure stall information, if it has any.
	Pressure *Pressure `json:"pressure,omitempty"`
	// Samples is how many times memory and filesystems were sampled.
	Samples int `json:"samples"`
}

// Pressure is stall time, in milliseconds.
type Pressure struct {
	CPUSome    int64 `json:"cpu_some_ms"`
	MemorySome int64 `json:"memory_some_ms"`
	MemoryFull int64 `json:"memory_full_ms"`
	IOSome     int64 `json:"io_some_ms"`
	IOFull     int64 `json:"io_full_ms"`
}

// A meter measures the machine while its runner runs: counters at the
// start and end, and memory and filesystems every interval in between.
type meter struct {
	proc  string // where procfs is mounted
	root  string // the boot disk's filesystem
	work  string // where the work disk is mounted
	every time.Duration
	now   func() time.Time

	u             Usage
	cpu0          cpuTimes
	oom0          int64
	psi0          Pressure
	disks0, disks map[string]diskCounts
	rootDev       uint64
	// Whether the starting counters could be read: a difference from a
	// counter that wasn't would be a total since boot.
	cpuOK, oomOK, psiOK, disksOK, rootOK bool
}

// userHZ is the unit of /proc/stat's times: USER_HZ, which is 100 on every
// architecture Linux runs the base image on.
const userHZ = 100

func newMeter(proc, root, work string, every time.Duration) *meter {
	return &meter{proc: proc, root: root, work: work, every: every, now: time.Now}
}

// measure measures the machine until ctx is done, and sends what was used.
// It runs on its own, since a filesystem that doesn't answer could hold it
// up for good, and a report mustn't wait for it.
func (m *meter) measure(ctx context.Context, used chan<- Usage) {
	m.start()
	m.run(ctx)
	used <- m.finish()
}

// start takes the counters at the runner's start.
func (m *meter) start() {
	m.u = Usage{From: m.now().UTC()}
	m.cpu0, m.u.CPUs, m.cpuOK = m.cpuTimes()
	m.oom0, m.oomOK = m.vmstat("oom_kill")
	m.psi0, m.psiOK = m.pressure()
	m.disks0, m.disksOK = m.diskCounts()
	m.disks = map[string]diskCounts{}
	m.rootDev, m.rootOK = deviceOf(m.root)
	m.sample()
}

// run samples until ctx is done.
func (m *meter) run(ctx context.Context) {
	t := time.NewTicker(m.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sample()
		}
	}
}

// sample records memory, swap and filesystem use, and the disks' counters,
// so that a disk attached after the start is counted from its first sample.
func (m *meter) sample() {
	m.u.Samples++
	if mi, ok := m.meminfo(); ok {
		m.u.MemTotal = mi["MemTotal"]
		if avail, ok := mi["MemAvailable"]; ok {
			m.u.MemPeak = max(m.u.MemPeak, mi["MemTotal"]-avail)
		}
		m.u.SwapPeak = max(m.u.SwapPeak, mi["SwapTotal"]-mi["SwapFree"])
	}
	if size, used, ok := fsUse(m.root); ok {
		m.u.RootSize, m.u.RootPeak = size, max(m.u.RootPeak, used)
	}
	// /work is a directory on the boot disk until the work disk is
	// mounted there.
	if dev, ok := deviceOf(m.work); ok && m.rootOK && dev != m.rootDev {
		if size, used, ok := fsUse(m.work); ok {
			m.u.WorkSize, m.u.WorkPeak = size, max(m.u.WorkPeak, used)
		}
	}
	if !m.disksOK {
		return
	}
	counts, _ := m.diskCounts()
	for name, c := range counts {
		if _, seen := m.disks0[name]; !seen {
			m.disks0[name] = diskCounts{}
		}
		m.disks[name] = c
	}
}

// finish takes the counters at the runner's exit, and returns what was
// used.
func (m *meter) finish() Usage {
	m.sample()
	u := m.u
	u.To = m.now().UTC()
	if cpu, _, ok := m.cpuTimes(); ok && m.cpuOK {
		ms := func(a, b int64) int64 { return max(0, a-b) * 1000 / userHZ }
		u.CPUBusyMS = ms(cpu.busy, m.cpu0.busy)
		u.CPUIOWaitMS = ms(cpu.iowait, m.cpu0.iowait)
		u.CPUStealMS = ms(cpu.steal, m.cpu0.steal)
	}
	if oom, ok := m.vmstat("oom_kill"); ok && m.oomOK {
		u.OOMKills = new(max(0, oom-m.oom0))
	}
	if p, ok := m.pressure(); ok && m.psiOK {
		d := func(a, b int64) int64 { return max(0, a-b) }
		u.Pressure = &Pressure{
			CPUSome:    d(p.CPUSome, m.psi0.CPUSome),
			MemorySome: d(p.MemorySome, m.psi0.MemorySome),
			MemoryFull: d(p.MemoryFull, m.psi0.MemoryFull),
			IOSome:     d(p.IOSome, m.psi0.IOSome),
			IOFull:     d(p.IOFull, m.psi0.IOFull),
		}
	}
	for name, c := range m.disks {
		c0 := m.disks0[name]
		u.DiskRead += max(0, c.read-c0.read)
		u.DiskWritten += max(0, c.written-c0.written)
	}
	return u
}

func (m *meter) read(name string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(m.proc, name))
	return b, err == nil
}

type cpuTimes struct{ busy, iowait, steal int64 }

// cpuTimes reads /proc/stat's total line, in USER_HZ, and counts the CPUs.
// user includes guest time already, so guest isn't added again.
func (m *meter) cpuTimes() (cpuTimes, int, bool) {
	b, ok := m.read("stat")
	if !ok {
		return cpuTimes{}, 0, false
	}
	var t cpuTimes
	found, cpus := false, 0
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "cpu" && len(f) >= 9:
			v := make([]int64, 8)
			for i := range v {
				v[i], _ = strconv.ParseInt(f[i+1], 10, 64)
			}
			// user nice system idle iowait irq softirq steal
			t = cpuTimes{busy: v[0] + v[1] + v[2] + v[5] + v[6], iowait: v[4], steal: v[7]}
			found = true
		case strings.HasPrefix(f[0], "cpu") && f[0] != "cpu":
			cpus++
		}
	}
	return t, cpus, found
}

// meminfo reads /proc/meminfo, in bytes.
func (m *meter) meminfo() (map[string]int64, bool) {
	b, ok := m.read("meminfo")
	if !ok {
		return nil, false
	}
	out := map[string]int64{}
	for line := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(line, ":")
		f := strings.Fields(v)
		if !ok || len(f) == 0 {
			continue
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		if len(f) > 1 && f[1] == "kB" {
			n *= 1024
		}
		out[k] = n
	}
	_, ok = out["MemTotal"]
	return out, ok
}

// vmstat reads one of /proc/vmstat's counters.
func (m *meter) vmstat(key string) (int64, bool) {
	b, ok := m.read("vmstat")
	if !ok {
		return 0, false
	}
	for line := range strings.Lines(string(b)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok && k == key {
			n, err := strconv.ParseInt(v, 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// pressure reads the totals of /proc/pressure's files, in milliseconds.
func (m *meter) pressure() (Pressure, bool) {
	totals := func(name string) (some, full int64, ok bool) {
		b, ok := m.read(filepath.Join("pressure", name))
		if !ok {
			return 0, 0, false
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 0 {
				continue
			}
			for _, kv := range f[1:] {
				if v, found := strings.CutPrefix(kv, "total="); found {
					us, err := strconv.ParseInt(v, 10, 64)
					if err != nil {
						return 0, 0, false
					}
					switch f[0] {
					case "some":
						some = us / 1000
					case "full":
						full = us / 1000
					}
				}
			}
		}
		return some, full, true
	}
	var p Pressure
	var ok1, ok2, ok3 bool
	p.CPUSome, _, ok1 = totals("cpu")
	p.MemorySome, p.MemoryFull, ok2 = totals("memory")
	p.IOSome, p.IOFull, ok3 = totals("io")
	return p, ok1 && ok2 && ok3
}

type diskCounts struct{ read, written int64 }

// wholeDisk matches the names Linux gives Compute Engine's disks, SCSI or
// NVMe, and virtio's: whole disks, not their partitions, which would count
// the same I/O twice.
var wholeDisk = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|nvme[0-9]+n[0-9]+)$`)

// diskCounts reads /proc/diskstats' sectors read and written, which are
// always of 512 bytes, for each whole disk.
func (m *meter) diskCounts() (map[string]diskCounts, bool) {
	out := map[string]diskCounts{}
	b, ok := m.read("diskstats")
	if !ok {
		return out, false
	}
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) < 10 || !wholeDisk.MatchString(f[2]) {
			continue
		}
		r, err1 := strconv.ParseInt(f[5], 10, 64)
		w, err2 := strconv.ParseInt(f[9], 10, 64)
		if err1 == nil && err2 == nil {
			out[f[2]] = diskCounts{read: r * 512, written: w * 512}
		}
	}
	return out, true
}
