// Package machines gives queued jobs machines and ends their lives
// (specs/0002-machines.md, A machine's life): a pass makes a machine for each
// job that may have one, within its consumer's limits, and deletes what's
// finished; the job-started check decides, from GitHub's records, whether
// the job a machine's runner took may run there, and gives it its work disk;
// and the supervisor's report says the job is done.
//
// What machines exist, and what they're for, is in their names, labels and
// metadata; what jobs want is in GitHub; what can't be read back from either
// is in the bucket:
//
//	machines/<machine>/admission.json  the job a machine's runner took, and the check's decision
//	machines/<machine>/report.json     the supervisor's report
//	disks/<disk>.json                  a work disk, recorded before it's made
//	refusals/<repository>/<run>-<attempt>.json  a run attempt refused for a fork's job or a claimed scope, until it's cancelled
//	jobs/<repository>/<job>.json       when a queued job's first machine was made
//	logs/<machine>-<created>.txt       a deleted machine's boot log
package machines

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/selector"
	"github.com/lanescape/runners/internal/store"
)

// The spec's names and timings.
const (
	// Network is the service's network, which every machine joins, in its
	// subnetwork of the same name; the proxy's git machine has a
	// subnetwork of its own.
	Network    = "lanescape-runners"
	Subnetwork = "lanescape-runners"
	// AccountID is the machines' account, which has no role anywhere.
	AccountID = "lanescape-runners-machine"
	// GroupPrefix names each consumer's runner group, with its repository
	// ID.
	GroupPrefix = "lanescape-runners-"
	// ContributionEnvironment is the environment an approved contribution's
	// jobs name.
	ContributionEnvironment = "contribution"
	// ForkApproval is the only organization setting for outside
	// contributors' fork workflows the service runs with.
	ForkApproval = "all_external_contributors"

	// DiskType is every disk's type, with the shapes' IOPS and throughput.
	DiskType = "hyperdisk-balanced"
	// WorkDevice is the work disk's device name; the machine sees it as
	// /dev/disk/by-id/google-work.
	WorkDevice = "work"

	MaxAge          = 5 * time.Hour
	MaxRunDuration  = 6 * time.Hour
	IdleLimit       = 20 * time.Minute
	ReportWait      = 10 * time.Minute
	RetryLimit      = time.Hour
	CancelEvery     = time.Minute
	ForceCancel     = 5 * time.Minute
	QueueDeadline   = 2 * time.Minute
	assignmentWait  = 20 * time.Second
	assignmentRetry = 2 * time.Second
	recordsKept     = 24 * time.Hour
	bootLogLimit    = 4 << 20
)

// Labels and metadata the service gives its machines.
const (
	LabelRepository   = "lsr-repository"
	LabelSelector     = "lsr-selector"
	LabelCreated      = "lsr-created"
	LabelJob          = "lsr-job"
	LabelContribution = "lsr-contribution"
	LabelMachine      = "lsr-machine"

	MetaSelector = "lsr-selector"
	MetaService  = "lsr-service"
	MetaRunnerID = "lsr-runner-id"
	MetaJob      = "lsr-job"
	// MetaJITConfig holds the runner's just-in-time configuration, a
	// credential: it's never logged.
	MetaJITConfig = "jit-config"
)

// scopes are the machines' access scopes. Their account has no role, so
// its access tokens reach nothing; identity tokens need none.
var scopes = []string{"https://www.googleapis.com/auth/userinfo.email"}

var (
	machineName = regexp.MustCompile(`^lsr-([1-9][0-9]*)-([a-z0-9]{8})$`)
	diskName    = regexp.MustCompile(`^lsr-([1-9][0-9]*)-([a-z0-9]{8})(-work)?$`)
)

// DiskSource chooses what a job's work disk starts from: a resume image or
// a recorded restore choice (specs/0003-disks.md). It returns the image's
// name, or "" for an empty disk, and the disk's size, at least sizeGB.
type DiskSource interface {
	Choose(ctx context.Context, m Machine, a Admission, sizeGB int64) (image string, size int64, err error)
}

// Saver saves a finished job's work disk, from its stopped machine
// (specs/0003-disks.md).
type Saver interface {
	Save(ctx context.Context, m Machine, a Admission) error
}

// Preemptions deals with a Spot machine that stopped with its job admitted:
// preempted, or perhaps (specs/0002-machines.md, Preemption). It returns
// once the machine may be deleted.
type Preemptions interface {
	Stopped(ctx context.Context, m Machine, a Admission, preempted bool) error
}

// Service is the machines' part of the service.
type Service struct {
	Config *config.Config
	// Project is the service's project, where its machines are.
	Project string
	// ServiceURL is the service's address, which its machines call and
	// their identity tokens name as the audience.
	ServiceURL     string
	MachineAccount string

	GitHub   github.API
	Compute  compute.API
	Objects  store.Objects
	Binder   *binding.Binder
	Machines *auth.Machines
	Clock    clockwork.Clock
	Logger   *slog.Logger

	// Kick starts a pass soon, without waiting for it: the service acts on
	// a report at once, rather than at the next sweep. Unset, reports wait
	// for the next pass.
	Kick func()

	// AssignmentWait and AssignmentRetry are how long, and how often, the
	// job-started check asks GitHub which job is on a machine's runner
	// before it answers that the hook should ask again.
	AssignmentWait  time.Duration
	AssignmentRetry time.Duration

	// The later parts of a machine's life, which the disks package serves
	// (specs/0003-disks.md): unset, every work disk starts empty, nothing
	// is saved, and a stopped Spot machine is deleted.
	Disks       DiskSource
	Saves       Saver
	Preemptions Preemptions
}

// MachineAccountOf is the machines' account in a project.
func MachineAccountOf(project string) string {
	return AccountID + "@" + project + ".iam.gserviceaccount.com"
}

func (s *Service) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

func (s *Service) now() time.Time { return s.Clock.Now().UTC() }

// Machine is one of the service's machines, read from its name, labels and
// metadata.
type Machine struct {
	compute.Instance
	RepositoryID int64
	Selector     selector.Selector
	RunnerID     int64
	// MadeFor is the job the machine was made for, which need not be the
	// one its runner takes.
	MadeFor      int64
	Contribution bool
	CreatedAt    time.Time
}

// ours reads an instance as one of the service's machines, if it's one.
func ours(in compute.Instance) (Machine, bool) {
	m := machineName.FindStringSubmatch(in.Name)
	if m == nil || in.Labels[LabelRepository] != m[1] {
		return Machine{}, false
	}
	repo, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return Machine{}, false
	}
	sel, err := selector.Parse(in.Metadata[MetaSelector])
	if err != nil || sel.String() != in.Metadata[MetaSelector] || in.Labels[LabelSelector] != sel.Hash() {
		return Machine{}, false
	}
	created, err := strconv.ParseInt(in.Labels[LabelCreated], 10, 64)
	if err != nil {
		return Machine{}, false
	}
	runner, _ := strconv.ParseInt(in.Metadata[MetaRunnerID], 10, 64)
	job, _ := strconv.ParseInt(in.Metadata[MetaJob], 10, 64)
	return Machine{Instance: in, RepositoryID: repo, Selector: sel, RunnerID: runner, MadeFor: job,
		Contribution: in.Labels[LabelContribution] == "true", CreatedAt: time.Unix(created, 0).UTC()}, true
}

const nameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// newName names a new machine for a repository: lsr-<repository ID>-<random>.
func newName(repositoryID int64) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	for i := range b {
		b[i] = nameAlphabet[int(b[i])%len(nameAlphabet)]
	}
	return fmt.Sprintf("lsr-%d-%s", repositoryID, b[:])
}

// WorkDisk is a machine's work disk's name.
func WorkDisk(machine string) string { return machine + "-work" }

// GroupName is a consumer's runner group.
func GroupName(repositoryID int64) string { return GroupPrefix + strconv.FormatInt(repositoryID, 10) }

// Admission is the job-started check's record of the job a machine's runner
// took, and what it decided. It's created only if absent, so a machine has
// one job, and one decision, for good.
type Admission struct {
	Machine      string `json:"machine"`
	Zone         string `json:"zone"`
	RepositoryID int64  `json:"repository_id"`
	JobID        int64  `json:"job_id"`
	RunID        int64  `json:"run_id"`
	RunAttempt   int    `json:"run_attempt"`
	JobName      string `json:"job_name"`
	Selector     string `json:"selector"`
	Allowed      bool   `json:"allowed"`
	Reason       string `json:"reason,omitempty"`
	Contribution bool   `json:"contribution,omitempty"`
	// Cancel says the refused job's run is to be cancelled: it came from a
	// fork, or claimed a scope that isn't its run's.
	Cancel bool      `json:"cancel,omitempty"`
	At     time.Time `json:"at"`
}

// Report is the supervisor's report that its machine's job is done.
type Report struct {
	Machine string    `json:"machine"`
	Save    bool      `json:"save"`
	At      time.Time `json:"at"`
}

// The states a work disk is recorded in.
const (
	DiskMaking   = "making"
	DiskMade     = "made"
	DiskAttached = "attached"
)

// DiskRecord is a work disk, recorded before it's made: cleanup never
// deletes one that's being made or waiting to be attached while its machine
// is there.
type DiskRecord struct {
	Disk         string `json:"disk"`
	Machine      string `json:"machine"`
	Zone         string `json:"zone"`
	RepositoryID int64  `json:"repository_id"`
	JobID        int64  `json:"job_id"`
	State        string `json:"state"`
	SizeGB       int64  `json:"size_gb"`
	SourceImage  string `json:"source_image,omitempty"`
	// Attempt counts the confirmed failures to make or attach it, and
	// names the request ID of the next try.
	Attempt int       `json:"attempt,omitempty"`
	At      time.Time `json:"at"`
}

// Refusal is a run attempt whose job was refused for coming from a fork or
// claiming a scope that isn't its run's: it's cancelled, and no machine is
// made for its selectors, until GitHub shows it finished. A rerun is another
// attempt, refused afresh.
type Refusal struct {
	RepositoryID int64     `json:"repository_id"`
	RunID        int64     `json:"run_id"`
	RunAttempt   int       `json:"run_attempt"`
	Reason       string    `json:"reason"`
	Selectors    []string  `json:"selectors"`
	RefusedAt    time.Time `json:"refused_at"`
	CancelledAt  time.Time `json:"cancelled_at,omitzero"`
	LastCancel   time.Time `json:"last_cancel,omitzero"`
	ForcedAt     time.Time `json:"forced_at,omitzero"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
}

// JobRecord is when a queued job's first machine was made: another is made
// while it's still queued, for up to RetryLimit from then. It's written
// before the machine is made, tentative until the machine is known to have
// been: a tentative record doesn't start the hour.
type JobRecord struct {
	RepositoryID   int64     `json:"repository_id"`
	JobID          int64     `json:"job_id"`
	FirstMachineAt time.Time `json:"first_machine_at"`
	Machine        string    `json:"machine"`
	Tentative      bool      `json:"tentative,omitempty"`
}

func admissionKey(machine string) string { return "machines/" + machine + "/admission.json" }
func reportKey(machine string) string    { return "machines/" + machine + "/report.json" }
func diskKey(disk string) string         { return "disks/" + disk + ".json" }
func refusalKey(repo, run int64, attempt int) string {
	return fmt.Sprintf("refusals/%d/%d-%d.json", repo, run, attempt)
}
func jobKey(repo, job int64) string { return fmt.Sprintf("jobs/%d/%d.json", repo, job) }
func bootLogKey(m Machine) string {
	return fmt.Sprintf("logs/%s-%d.txt", m.Name, m.CreatedAt.Unix())
}

func getJSON[T any](ctx context.Context, o store.Objects, key string) (T, int64, bool, error) {
	var v T
	data, gen, err := o.Get(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return v, 0, false, nil
	}
	if err != nil {
		return v, 0, false, err
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return v, 0, false, fmt.Errorf("record %s: %w", key, err)
	}
	return v, gen, true, nil
}

func createJSON(ctx context.Context, o store.Objects, key string, v any) (int64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return o.Create(ctx, key, data)
}

func replaceJSON(ctx context.Context, o store.Objects, key string, v any, gen int64) (int64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return o.Replace(ctx, key, data, gen)
}

// createOnce writes a record only if it's absent, and returns what's there
// either way: the caller's, or the one written first, perhaps by its own
// write whose answer was lost.
func createOnce[T any](ctx context.Context, o store.Objects, key string, v T) (T, int64, error) {
	gen, err := createJSON(ctx, o, key, v)
	if err == nil {
		return v, gen, nil
	}
	if !errors.Is(err, store.ErrPrecondition) {
		return v, 0, err
	}
	got, gen, ok, err := getJSON[T](ctx, o, key)
	if err != nil {
		return v, 0, err
	}
	if !ok {
		return v, 0, fmt.Errorf("record %s vanished as it was written", key)
	}
	return got, gen, nil
}
