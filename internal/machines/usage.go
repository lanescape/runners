package machines

import (
	"log/slog"
	"time"
)

// Usage is what a machine used while its runner ran, as its supervisor
// measured it and reported it with the job's end (specs/0002-machines.md,
// What a job used). A job can change anything on its machine, the
// supervisor included, so the service only records and logs it, and
// decides nothing from it. The supervisor has its own copy of this type,
// since it uses only Go's standard library; a test keeps them alike.
//
// What the machine couldn't measure is left out: zero, or nil for OOMKills
// and Pressure, whose zero means something.
type Usage struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	CPUs        int       `json:"cpus,omitempty"`
	CPUBusyMS   int64     `json:"cpu_busy_ms,omitempty"`
	CPUIOWaitMS int64     `json:"cpu_iowait_ms,omitempty"`
	CPUStealMS  int64     `json:"cpu_steal_ms,omitempty"`
	MemTotal    int64     `json:"mem_total,omitempty"`
	MemPeak     int64     `json:"mem_peak,omitempty"`
	SwapPeak    int64     `json:"swap_peak,omitempty"`
	OOMKills    *int64    `json:"oom_kills,omitempty"`
	DiskRead    int64     `json:"disk_read,omitempty"`
	DiskWritten int64     `json:"disk_written,omitempty"`
	RootSize    int64     `json:"root_size,omitempty"`
	RootPeak    int64     `json:"root_peak,omitempty"`
	WorkSize    int64     `json:"work_size,omitempty"`
	WorkPeak    int64     `json:"work_peak,omitempty"`
	Pressure    *Pressure `json:"pressure,omitempty"`
	Samples     int       `json:"samples"`
}

// Pressure is how long some, or all, of a machine's runnable work was
// stalled for want of CPU, memory or I/O, in milliseconds.
type Pressure struct {
	CPUSome    int64 `json:"cpu_some_ms"`
	MemorySome int64 `json:"memory_some_ms"`
	MemoryFull int64 `json:"memory_full_ms"`
	IOSome     int64 `json:"io_some_ms"`
	IOFull     int64 `json:"io_full_ms"`
}

// valid reports whether every number is one a machine could have measured:
// none negative, and the runner's end not before its start.
func (u Usage) valid() bool {
	n := []int64{int64(u.CPUs), u.CPUBusyMS, u.CPUIOWaitMS, u.CPUStealMS, u.MemTotal, u.MemPeak, u.SwapPeak,
		u.DiskRead, u.DiskWritten, u.RootSize, u.RootPeak, u.WorkSize, u.WorkPeak, int64(u.Samples)}
	if u.OOMKills != nil {
		n = append(n, *u.OOMKills)
	}
	if p := u.Pressure; p != nil {
		n = append(n, p.CPUSome, p.MemorySome, p.MemoryFull, p.IOSome, p.IOFull)
	}
	for _, v := range n {
		if v < 0 {
			return false
		}
	}
	return !u.From.IsZero() && !u.To.Before(u.From)
}

// attrs are the usage's log attributes, leaving out what it leaves out.
func (u Usage) attrs() []any {
	a := []any{slog.Time("from", u.From), slog.Time("to", u.To), slog.Int64("seconds", int64(u.To.Sub(u.From)/time.Second))}
	for _, f := range []struct {
		key string
		v   int64
	}{
		{"cpus", int64(u.CPUs)}, {"cpu_busy_ms", u.CPUBusyMS}, {"cpu_iowait_ms", u.CPUIOWaitMS}, {"cpu_steal_ms", u.CPUStealMS},
		{"mem_total", u.MemTotal}, {"mem_peak", u.MemPeak}, {"swap_peak", u.SwapPeak},
		{"disk_read", u.DiskRead}, {"disk_written", u.DiskWritten},
		{"root_size", u.RootSize}, {"root_peak", u.RootPeak}, {"work_size", u.WorkSize}, {"work_peak", u.WorkPeak},
	} {
		if f.v != 0 {
			a = append(a, slog.Int64(f.key, f.v))
		}
	}
	if u.OOMKills != nil {
		a = append(a, slog.Int64("oom_kills", *u.OOMKills))
	}
	a = append(a, slog.Int("samples", u.Samples))
	if p := u.Pressure; p != nil {
		a = append(a, slog.Group("pressure", slog.Int64("cpu_some_ms", p.CPUSome), slog.Int64("memory_some_ms", p.MemorySome),
			slog.Int64("memory_full_ms", p.MemoryFull), slog.Int64("io_some_ms", p.IOSome), slog.Int64("io_full_ms", p.IOFull)))
	}
	return a
}

// logUsage logs what a report says its machine used as "job usage", with
// the job its check admitted, if it admitted one, so that it can be found by
// the job's repository, run, attempt and ID (specs/0002-machines.md, What a
// job used). It's logged as the machine's records are dropped; a pass that
// logs it and then can't drop them logs it again, and the machine, which
// serves one job, tells the entries apart.
func (s *Service) logUsage(r Report) {
	attrs := []any{"machine", r.Machine, "zone", r.Zone, "machine_type", r.MachineType, "spot", r.Spot,
		"selector", r.Selector, "repository_id", r.RepositoryID}
	if r.JobID != 0 {
		attrs = append(attrs, "run_id", r.RunID, "run_attempt", r.RunAttempt, "job_id", r.JobID, "job_name", r.JobName)
	}
	attrs = append(attrs, slog.Group("usage", r.Usage.attrs()...))
	s.log().Info("job usage", attrs...)
}
