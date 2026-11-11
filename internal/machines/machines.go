// Package machines gives queued jobs machines and ends their lives
// (specs/0002-machines.md, A machine's life): a pass makes a machine for each
// job that may have one, from one pool for every consumer, oldest job
// first, and deletes what's
// finished; the job-started check decides, from GitHub's records, whether
// the job a machine's runner took may run there; an admitted job asks for its
// work disk and its save in its steps; and the supervisor's report says the
// job is done.
//
// What machines exist, and what they're for, is in their names, labels and
// metadata; what jobs want is in GitHub; what can't be read back from either
// is in the bucket:
//
//	machines/<machine>/facts.json      what a machine's first check reported GitHub told its runner
//	machines/<machine>/admission.json  the job a machine's runner took, and the check's decision
//	machines/<machine>/answered.json   when a failed job's hook was answered with its reason
//	machines/<machine>/report.json     the supervisor's report
//	machines/<machine>/save-request.json  the job's request to save its work disk
//	disks/<disk>.json                  a work disk, with its family, recorded before it's made
//	refusals/<repository>/<run>-<attempt>.json  a run attempt refused or cancelled, until GitHub shows it finished
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
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/selector"
	"github.com/lanescape/runners/internal/store"
)

// The spec's names and timings.
const (
	// Network is the service's network, which every machine joins, in its
	// subnetwork of the same name.
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

	// MaxAge is the most a machine lives, from its creation: a job on the
	// service should take under 30 minutes, 60 at worst, and this is the
	// absolute maximum. A machine past it is deleted, even mid-job, saving
	// nothing; a Spot machine is stopped and recorded first, with no resume
	// image (specs/0002-machines.md). It's the one constant that says so.
	MaxAge = 90 * time.Minute
	// MaxRunDuration is when Compute Engine itself stops a machine, the
	// backstop for when the service can't. Its margin over MaxAge covers
	// the service's own path, so the backstop never races it: a pass runs
	// within 5 minutes of a machine reaching MaxAge, or waits up to 10 for
	// a lease an earlier pass holds, then has up to 7 to stop and record a
	// Spot machine, which takes a minute or two; that's under 25 minutes.
	MaxRunDuration  = MaxAge + 30*time.Minute
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

// DiskSource chooses what a job's work disk of a family starts from: a
// resume image or its run's restore choice (specs/0003-disks.md). m is
// resolved with the run's scope and the family and its key. It returns the
// image's name, or "" for an empty disk, the disk's size, at least sizeGB,
// and where it came from: Resumed, or the step of the restore order.
type DiskSource interface {
	Choose(ctx context.Context, m Machine, a Admission, sizeGB int64) (image string, size int64, restored string, err error)
}

// Resumed is where a work disk made from a resume image came from.
const Resumed = "resume"

// Saver saves a finished job's work disk, from its stopped machine
// (specs/0003-disks.md).
type Saver interface {
	Save(ctx context.Context, m Machine, a Admission) error
}

// Preemptions deals with a Spot machine that stopped with its job admitted:
// preempted, or perhaps (specs/0002-machines.md, Preemption). It returns
// once the machine may be deleted. resume says whether to make a resume
// image of its work disk, which a machine past its age limit doesn't get.
type Preemptions interface {
	Stopped(ctx context.Context, m Machine, a Admission, preempted, resume bool) error
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
	// FailStopDelay is how long after a failed job's hook is answered its
	// machine is stopped; zero is DefaultFailStopDelay.
	FailStopDelay time.Duration
	// BootLogs reads machines' serial consoles from Cloud Logging, after
	// they're deleted; unset, a boot log is read from Compute Engine
	// before its machine goes.
	BootLogs BootLogs
}

// BootLogs reads a machine's serial console, by its numeric ID, from Cloud
// Logging.
type BootLogs interface {
	Serial(ctx context.Context, instanceID uint64, limit int) (string, error)
}

// DefaultFailStopDelay is long enough for a failed job's hook to read its
// answer and print the reason.
const DefaultFailStopDelay = 5 * time.Second

// BootLogWait is how long after its machine goes a boot log's last entries
// are waited for, before it's recorded as it is, or as missing.
const BootLogWait = 10 * time.Minute

// LastLine is what the supervisor writes last, before the machine powers
// off: a boot log that has it is complete.
const LastLine = "lanescape-runners supervisor: powering off"

// PendingLog is a deleted machine's boot log, still to be read from Cloud
// Logging.
type PendingLog struct {
	Machine    string    `json:"machine"`
	InstanceID uint64    `json:"instance_id"`
	CreatedAt  time.Time `json:"created_at"`
	DeletedAt  time.Time `json:"deleted_at"`
}

func pendingLogKey(m Machine) string {
	return fmt.Sprintf("bootlogs/%s-%d.json", m.Name, m.CreatedAt.Unix())
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
	// fork, or is failed with its reason.
	Cancel bool `json:"cancel,omitempty"`
	// Fail is the reason a failed job is told, in its log.
	Fail string `json:"fail,omitempty"`
	// What the check resolved for an allowed job: the run's scope and
	// base, and its commit.
	Scope  scope.Scope `json:"scope,omitzero"`
	Base   scope.Scope `json:"base,omitzero"`
	Commit string      `json:"commit,omitempty"`
	// Unbound says the job was allowed on an unbound pull_request_target
	// run: the smallest shape, standard, with no work disk.
	Unbound bool      `json:"unbound,omitempty"`
	At      time.Time `json:"at"`
}

// Resolve is the machine as its admitted job resolved it: with the run's
// scope, and the family and key of the work disk the job asked for, if it
// did, none of which the label carries.
func (a Admission) Resolve(m Machine, d *DiskRecord) Machine {
	if a.Allowed && !a.Unbound {
		m.Selector.Scope = a.Scope
		if d != nil && d.Machine == m.Name && d.JobID == a.JobID {
			m.Selector.Family, m.Selector.Key = d.Family, d.Key
		}
	}
	return m
}

// Report is the supervisor's report that its machine's job is done. It says
// nothing else: a job asks to save its work disk itself, and no report
// means no save.
type Report struct {
	Machine string    `json:"machine"`
	At      time.Time `json:"at"`
}

// SaveRequest is a job's request, through the save-disk action, that its
// work disk be saved once it has finished (specs/0003-disks.md). Nothing
// withdraws it.
type SaveRequest struct {
	Machine string    `json:"machine"`
	JobID   int64     `json:"job_id"`
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
	// Family is what the job asked for, and Key the family's key at the
	// run's commit; Restored is where the disk came from, as the request's
	// answer says.
	Family      string `json:"family"`
	Key         string `json:"key"`
	Restored    string `json:"restored"`
	SizeGB      int64  `json:"size_gb"`
	SourceImage string `json:"source_image,omitempty"`
	// Attempt counts the confirmed failures to make or attach it, and
	// names the request ID of the next try. Nonce is new with each record,
	// so a record made again after its image went, whose attempts count
	// from 0 again, never reuses an earlier record's request.
	Attempt int       `json:"attempt,omitempty"`
	Nonce   string    `json:"nonce,omitempty"`
	At      time.Time `json:"at"`
}

// Refusal is a run attempt the service cancels: its job was refused for
// coming from a fork, or failed with a reason, or it was cancelled without a
// machine, for a job that named a label no runner of the service's has or
// whose machines never started. No machine is made for its jobs until
// GitHub shows it finished. A rerun is another attempt, decided afresh.
type Refusal struct {
	RepositoryID int64     `json:"repository_id"`
	RunID        int64     `json:"run_id"`
	RunAttempt   int       `json:"run_attempt"`
	Reason       string    `json:"reason"`
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
func factsKey(machine string) string     { return "machines/" + machine + "/facts.json" }
func answeredKey(machine string) string  { return "machines/" + machine + "/answered.json" }

// Answered is when a failed job's hook was answered with its reason.
type Answered struct {
	Machine string    `json:"machine"`
	At      time.Time `json:"at"`
}

// AnswerWait is how long a failed job's machine waits for its answer to be
// recorded before passes stop it anyway.
const AnswerWait = 2 * time.Minute

func reportKey(machine string) string      { return "machines/" + machine + "/report.json" }
func saveRequestKey(machine string) string { return "machines/" + machine + "/save-request.json" }
func diskKey(disk string) string           { return "disks/" + disk + ".json" }
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
