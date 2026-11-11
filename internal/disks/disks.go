// Package disks saves and restores work disks, and makes resume images of
// the work disks of jobs lost to a preemption or to the age limit
// (specs/0003-disks.md, specs/0002-machines.md, Preemption). It serves the
// machines' part of the service through the seams machines.Service offers:
// choosing what a job's work disk starts from, saving a finished job's disk,
// and recording a machine whose job was lost; and it adds its own part of
// each pass, which reruns runs that lost a job, and deletes what's no longer
// needed, as early as its rules allow.
//
// What images exist is in Compute Engine, by their names and labels; what
// they hold and who chose them is in the bucket:
//
//	images/<repository>/<image>.json            a saved disk or resume image, recorded before it's made
//	choices/<repository>/<run>/<selector hash>.json  a run's restore choice for a selector
//	recent/<repository>/<scope>/<family hash>.json  a pull request's most recent choice for a family
//	closures/<repository>/<scope>.json          when a pull request was last seen closed
//	families/<repository>/<family hash>.json    when a family of main's or a long-lived branch's was last used
//	claims/<repository>/<image>.json            the job a resume image is claimed for
//	preemptions/<repository>/<run>/<instance ID>.json  a machine whose job was lost: a stopped Spot machine, preempted or perhaps, or one stopped at its age limit
//	events/<instance ID>.json                   Compute Engine's event that it preempted an instance
//	reruns/<repository>/<run>.json              the attempts of a run the service has rerun
//	machines/<machine>/save.json                a finished job's save, once decided
//	leases/disks/<repository>                   the short lease for choosing and marking
//
// A saved disk serves the scope it was saved in and no other: main's are
// made only from jobs of runs bound main, a pull request's only from its own
// runs, and a pull request's are read only by its own later runs.
package disks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/selector"
	"github.com/lanescape/runners/internal/store"
)

// The spec's timings and counts.
const (
	// PullRequestUnused is how long an open pull request's disks are kept
	// after their last use, for pull requests that are abandoned but never
	// closed.
	PullRequestUnused = 14 * 24 * time.Hour
	// MainUnused is how long a family of main's may go unused before all
	// of it goes, newest included: a family that's been retired.
	MainUnused = 90 * 24 * time.Hour
	// MainKeys is how many of a main family's keys are kept, the newest
	// and the one before it, besides any an open pull request's most
	// recent choice names.
	MainKeys = 2
	// RunLimit is the longest GitHub lets a workflow run go on, after which
	// no run bound before a pull request closed can still save.
	RunLimit = 35 * 24 * time.Hour
	// ResumeKept is how long after its machine stopped a resume image may
	// resume a job, and how long the service looks for evidence that it
	// was preempted.
	ResumeKept = 24 * time.Hour
	// MaxAttempts is how many attempts of a run there may be in all
	// before the service reruns it no more.
	MaxAttempts = 3

	// The disks' lease is held only for one step: choosing and recording
	// a choice, or marking images for deletion.
	leaseValidity = time.Minute
	stepDeadline  = 30 * time.Second
	leaseRetry    = 100 * time.Millisecond
	// abandoned is how long an image's record may say it's being made,
	// with no image and no machine to make it from, before it's dropped.
	abandoned  = time.Hour
	eventsKept = 2 * ResumeKept
)

// The kinds of image the service makes besides its base images.
const (
	KindSaved  = "saved"
	KindResume = "resume"
)

// Labels on the service's images. Values that may be longer or freer than a
// label allows are hashes, and the bucket holds the values themselves.
const (
	LabelRepository = machines.LabelRepository
	LabelKind       = "lsr-kind"
	LabelScope      = "lsr-scope"
	LabelArch       = "lsr-arch"
	LabelFamily     = "lsr-family"
	LabelKey        = "lsr-key"
	LabelSaved      = "lsr-saved"
	LabelLastUsed   = "lsr-last-used"
	LabelRun        = "lsr-run"
	LabelSelector   = "lsr-selector"
	LabelJobName    = "lsr-job-name"
)

// The states an image is recorded in.
const (
	ImageMaking = "making"
	ImageReady  = "ready"
	// ImageMarked is an image marked for deletion: it's never chosen, and
	// it's deleted next.
	ImageMarked = "marked"
)

// The steps of the restore order a choice can come from
// (specs/0003-disks.md, Saved disks), as its record and the service's log
// name them.
const (
	StepPullRequestExact  = "pull_request_exact"
	StepPullRequestNewest = "pull_request_newest"
	StepMainExact         = "main_exact"
	StepMainNewest        = "main_newest"
	StepBranchExact       = "branch_exact"
	StepBranchNewest      = "branch_newest"
	StepNone              = "none"
)

// The preemption evidence a record holds: a standard machine is never
// preempted.
const (
	Preempted    = "yes"
	Unknown      = "unknown"
	NotPreempted = "no"
)

var (
	savedName  = regexp.MustCompile(`^lsr-([1-9][0-9]*)-([0-9a-f]{20})-([1-9][0-9]*)-([a-z0-9]{8})$`)
	resumeName = regexp.MustCompile(`^lsr-([1-9][0-9]*)-resume-([1-9][0-9]*)$`)
)

// Service is the disks' part of the service.
type Service struct {
	Config *config.Config
	// Project is the service's project, whose Compute Engine events it
	// records.
	Project string
	GitHub  github.API
	Compute compute.API
	Objects store.Objects
	Clock   clockwork.Clock
	Logger  *slog.Logger
	// StepDeadline is how long a step under the disks' lease, waiting for
	// it included, may take; it defaults to 30 seconds, well inside the
	// lease's validity.
	StepDeadline time.Duration
}

func (s *Service) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

func (s *Service) now() time.Time { return s.Clock.Now().UTC() }

// day is a time's date, as labels carry it.
func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Series is the hash that names a saved disk's versions: of its scope, its
// architecture, its family and its key, so that a family's disks of one
// architecture never stand in for another's.
func Series(scope, arch, family, key string) string {
	return selector.Hash(scope + "\x00" + arch + "\x00" + family + "\x00" + key)
}

// SavedName names a new version of a saved disk: lsr-<repository
// ID>-<series>-<save time, in Unix seconds>-<the random part of the
// machine's name>, since two jobs can save the same series in one second.
func SavedName(repositoryID int64, series string, at time.Time, machine string) string {
	return fmt.Sprintf("lsr-%d-%s-%d-%s", repositoryID, series, at.Unix(), machine[strings.LastIndex(machine, "-")+1:])
}

// ResumeName names a lost job's resume image, by its machine's numeric
// instance ID.
func ResumeName(repositoryID int64, instanceID uint64) string {
	return fmt.Sprintf("lsr-%d-resume-%d", repositoryID, instanceID)
}

// ours reports whether an image is one of the service's saved disks or
// resume images, and its consumer's.
func ours(im compute.Image) (int64, bool) {
	m := savedName.FindStringSubmatch(im.Name)
	if m == nil {
		m = resumeName.FindStringSubmatch(im.Name)
	}
	if m == nil || im.Labels[LabelRepository] != m[1] {
		return 0, false
	}
	repo, err := strconv.ParseInt(m[1], 10, 64)
	return repo, err == nil
}

// Image is a saved disk or a resume image, as the bucket records it,
// before it's made.
type Image struct {
	Image        string `json:"image"`
	RepositoryID int64  `json:"repository_id"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	// A saved disk's scope, family and key; every image's architecture.
	Scope  string `json:"scope,omitempty"`
	Arch   string `json:"arch"`
	Family string `json:"family,omitempty"`
	Key    string `json:"key,omitempty"`
	// What it was made from.
	Machine    string `json:"machine"`
	Zone       string `json:"zone"`
	InstanceID uint64 `json:"instance_id,omitempty"`
	// The job whose disk it is: for a resume image, the job it resumes.
	JobID      int64  `json:"job_id"`
	JobName    string `json:"job_name,omitempty"`
	RunID      int64  `json:"run_id"`
	RunAttempt int    `json:"run_attempt"`
	Selector   string `json:"selector"`
	SizeGB     int64  `json:"size_gb"`
	// Attempt counts the confirmed failures to make it, and names the
	// request ID of the next try.
	Attempt  int       `json:"attempt,omitempty"`
	SavedAt  time.Time `json:"saved_at"`
	LastUsed time.Time `json:"last_used"`
	// BoundAt is when a saved disk's run was bound: one of a pull
	// request's bound before the pull request was last closed is never
	// chosen again.
	BoundAt time.Time `json:"bound_at,omitzero"`
	// TouchedAt is when a job last chose or claimed it: writing it changes
	// the record's generation, so marking it, which writes the same
	// record with the generation it read, can't succeed past a choice.
	TouchedAt time.Time `json:"touched_at,omitzero"`
	MarkedAt  time.Time `json:"marked_at,omitzero"`
	// GoneAt is when a pass first found the machine it's made from gone
	// while it was still being made.
	GoneAt time.Time `json:"gone_at,omitzero"`
}

// Choice is a run's restore choice for a selector: the exact image version
// every job of the run with that selector starts from, or none, and which
// step of the restore order it came from.
type Choice struct {
	RepositoryID int64  `json:"repository_id"`
	RunID        int64  `json:"run_id"`
	Selector     string `json:"selector"`
	Scope        string `json:"scope"`
	Arch         string `json:"arch"`
	Family       string `json:"family"`
	Step         string `json:"step"`
	Image        string `json:"image,omitempty"`
	// Attempt is the run attempt it was picked in. It holds for the
	// attempts the service's automatic reruns started from it; one a
	// maintainer started picks again, as a new run would.
	Attempt int       `json:"attempt"`
	At      time.Time `json:"at"`
}

// Recent is a pull request's most recent restore choice for a family on an
// architecture, kept while the pull request is open, after its run's choice
// has gone: main keeps an older key while one names it.
type Recent struct {
	RepositoryID int64  `json:"repository_id"`
	Scope        string `json:"scope"`
	Arch         string `json:"arch"`
	Family       string `json:"family"`
	Step         string `json:"step"`
	// The image chosen, and its scope and key; none if the choice was an
	// empty disk.
	Image      string    `json:"image,omitempty"`
	ImageScope string    `json:"image_scope,omitempty"`
	Key        string    `json:"key,omitempty"`
	RunID      int64     `json:"run_id"`
	At         time.Time `json:"at"`
}

// Closure is a pull request the service has seen closed: its disks saved by
// runs bound up to the last time it was seen closed are never chosen again,
// even once it's reopened, and go as soon as no unfinished run's choice
// names them. SeenAt moves on with every pass that still sees it closed, so
// that a run bound while it was closed counts too; GitHub's ClosedAt, to
// the second, is for whoever reads the bucket.
type Closure struct {
	RepositoryID int64     `json:"repository_id"`
	Scope        string    `json:"scope"`
	ClosedAt     time.Time `json:"closed_at,omitzero"`
	SeenAt       time.Time `json:"seen_at"`
}

// retired reports whether a saved disk of a pull request's was saved by a
// run bound before its pull request was last seen closed.
func retired(v Image, c *Closure) bool {
	return c != nil && v.Kind == KindSaved && v.Scope == c.Scope && !v.BoundAt.After(c.SeenAt)
}

// Family is when a family of main's or a long-lived branch's was last
// used, kept apart from its images' records so that deleting a version never makes the family look
// unused for longer than it has been.
type Family struct {
	RepositoryID int64     `json:"repository_id"`
	Scope        string    `json:"scope"`
	Arch         string    `json:"arch"`
	Family       string    `json:"family"`
	LastUsed     time.Time `json:"last_used"`
}

// Claim is the one job a resume image is for.
type Claim struct {
	Image        string    `json:"image"`
	RepositoryID int64     `json:"repository_id"`
	RunID        int64     `json:"run_id"`
	RunAttempt   int       `json:"run_attempt"`
	JobID        int64     `json:"job_id"`
	At           time.Time `json:"at"`
}

// Preemption is a machine that stopped with its job admitted and
// unfinished, recorded before it's deleted: a Spot machine, preempted or
// perhaps, or any machine the service stopped at its age limit. Its job is
// lost either way, and its run's rerun resumes it.
type Preemption struct {
	InstanceID   uint64 `json:"instance_id"`
	Machine      string `json:"machine"`
	Zone         string `json:"zone"`
	RepositoryID int64  `json:"repository_id"`
	JobID        int64  `json:"job_id"`
	RunnerID     int64  `json:"runner_id"`
	JobName      string `json:"job_name"`
	RunID        int64  `json:"run_id"`
	RunAttempt   int    `json:"run_attempt"`
	Selector     string `json:"selector"`
	// Arch and Family are its work disk's, if its job asked for one: a
	// resume image resumes only a job of the same name that asks for the
	// same family on the same architecture.
	Arch   string `json:"arch"`
	Family string `json:"family,omitempty"`
	// Preempted is yes, or unknown while there's no evidence, or no for a
	// standard machine.
	Preempted string `json:"preempted"`
	// AgeLimit says the service stopped the machine at its age limit, its
	// job unfinished: the job was lost, whatever the evidence of a
	// preemption.
	AgeLimit  bool      `json:"age_limit,omitempty"`
	StoppedAt time.Time `json:"stopped_at"`
	// ResumeImage is the resume image made of its work disk, if its scope
	// saves and it had one.
	ResumeImage string `json:"resume_image,omitempty"`
	NoDisk      bool   `json:"no_disk,omitempty"`
	// Done says nothing more is to be made for the job: its resume image is
	// ready, or it gets none. Its run is rerun only once it's done, or once
	// the day a resume image could resume it has passed.
	Done bool `json:"done,omitempty"`
}

// Event is Compute Engine's word that it preempted an instance, as the log
// sink publishes it.
type Event struct {
	InstanceID uint64    `json:"instance_id"`
	Zone       string    `json:"zone"`
	Machine    string    `json:"machine"`
	Method     string    `json:"method"`
	At         time.Time `json:"at"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Rerun is the attempts of a run the service has asked GitHub to rerun,
// each recorded before it asks.
type Rerun struct {
	RepositoryID int64     `json:"repository_id"`
	RunID        int64     `json:"run_id"`
	Attempts     []int     `json:"attempts"`
	At           time.Time `json:"at"`
}

// Save is a finished job's save, once decided: the image it's made as, or
// why it's skipped.
type Save struct {
	Machine string    `json:"machine"`
	Image   string    `json:"image,omitempty"`
	SizeGB  int64     `json:"size_gb"`
	Skipped string    `json:"skipped,omitempty"`
	Done    bool      `json:"done,omitempty"`
	At      time.Time `json:"at"`
}

func imageKey(repo int64, image string) string { return fmt.Sprintf("images/%d/%s.json", repo, image) }
func choicePrefix(repo, run int64) string      { return fmt.Sprintf("choices/%d/%d/", repo, run) }

// choiceKey is a run's restore choice for a family on an architecture: one
// per run, family and architecture (specs/0003-disks.md).
func choiceKey(repo, run int64, sel selector.Selector) string {
	return choicePrefix(repo, run) + selector.Hash(sel.Family+"\x00"+sel.Arch) + ".json"
}
func recentKey(repo int64, scope, arch, family string) string {
	return fmt.Sprintf("recent/%d/%s/%s.json", repo, scope, selector.Hash(arch+"\x00"+family))
}
func closureKey(repo int64, scope string) string {
	return fmt.Sprintf("closures/%d/%s.json", repo, scope)
}
func familyKey(repo int64, scope, arch, family string) string {
	return fmt.Sprintf("families/%d/%s.json", repo, selector.Hash(scope+"\x00"+arch+"\x00"+family))
}
func claimKey(repo int64, image string) string { return fmt.Sprintf("claims/%d/%s.json", repo, image) }
func preemptionPrefix(repo, run int64) string  { return fmt.Sprintf("preemptions/%d/%d/", repo, run) }
func preemptionKey(repo, run int64, id uint64) string {
	return preemptionPrefix(repo, run) + strconv.FormatUint(id, 10) + ".json"
}
func eventKey(id uint64) string       { return "events/" + strconv.FormatUint(id, 10) + ".json" }
func rerunKey(repo, run int64) string { return fmt.Sprintf("reruns/%d/%d.json", repo, run) }
func saveKey(machine string) string   { return "machines/" + machine + "/save.json" }
func leaseKey(repo int64) string      { return fmt.Sprintf("leases/disks/%d", repo) }

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

func replaceJSON(ctx context.Context, o store.Objects, key string, v any, gen int64) (int64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return o.Replace(ctx, key, data, gen)
}

// createOnce writes a record only if it's absent, and returns what's there
// either way.
func createOnce[T any](ctx context.Context, o store.Objects, key string, v T) (T, int64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return v, 0, err
	}
	gen, err := o.Create(ctx, key, data)
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

// listJSON reads every record under a prefix, with its generation.
func listJSON[T any](ctx context.Context, o store.Objects, prefix string) (map[string]record[T], error) {
	objs, err := o.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := map[string]record[T]{}
	for _, ob := range objs {
		v, gen, ok, err := getJSON[T](ctx, o, ob.Name)
		if err != nil {
			return nil, err
		}
		if ok {
			out[ob.Name] = record[T]{v, gen}
		}
	}
	return out, nil
}

type record[T any] struct {
	v   T
	gen int64
}

// freshID is a request ID for a deletion, new each time it's tried:
// Compute Engine answers an ID it has seen with its first outcome, even a
// failure, and a deletion is safe to ask for again. Within one call, a
// retry with no answer keeps the ID.
func freshID(parts ...string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return compute.RequestID(append(parts, hex.EncodeToString(b[:]))...)
}

// errBusy means the disks' lease was held for longer than a step waits.
var errBusy = errors.New("the disks' lease is held; try again")

// step runs f under the consumer's disks lease, apart from the pass's, held
// only for f, with a deadline well inside the lease's validity, so that no
// two steps of choosing or marking overlap (specs/0003-disks.md, Saved
// disks).
func (s *Service) step(ctx context.Context, repo int64, f func(ctx context.Context) error) error {
	deadline := s.StepDeadline
	if deadline == 0 || deadline > stepDeadline {
		deadline = stepDeadline
	}
	sctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	l := lease.New(s.Objects, leaseKey(repo), leaseValidity, s.Clock)
	var held *lease.Held
	for {
		h, err := l.Acquire(sctx)
		if err == nil {
			held = h
			break
		}
		if !errors.Is(err, lease.ErrHeld) {
			return err
		}
		t := time.NewTimer(leaseRetry)
		select {
		case <-sctx.Done():
			t.Stop()
			return fmt.Errorf("%w: %w", errBusy, sctx.Err())
		case <-t.C:
		}
	}
	err := f(sctx)
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer rcancel()
	if rerr := held.Release(rctx); rerr != nil {
		s.log().Warn("releasing the disks' lease; it will expire", "repository_id", repo, "err", rerr.Error())
	}
	return err
}

// touch rewrites an image's record, as a choice or claim of it does, so
// that any marking of it that read the record before fails its
// precondition. It fails if the image has been marked.
func (s *Service) touch(ctx context.Context, repo int64, image string) error {
	for range 3 {
		rec, gen, ok, err := getJSON[Image](ctx, s.Objects, imageKey(repo, image))
		if err != nil {
			return err
		}
		if !ok || rec.State != ImageReady {
			return fmt.Errorf("image %s is no longer ready", image)
		}
		rec.TouchedAt = s.now()
		// Choosing a saved disk is using it, at most once a day, in the
		// same write: expiry reads it. A family of main's is marked used
		// first, in its own record, which a pass reads after the images':
		// a pass that read this image's record before the touch fails to
		// mark it, and one that read it after reads the family's use.
		if rec.Kind == KindSaved && day(rec.LastUsed) != day(s.now()) {
			if lasting(rec.Scope) {
				if err := s.putJSON(ctx, familyKey(repo, rec.Scope, rec.Arch, rec.Family), Family{RepositoryID: repo,
					Scope: rec.Scope, Arch: rec.Arch, Family: rec.Family, LastUsed: s.now()}); err != nil {
					return err
				}
			}
			rec.LastUsed = s.now()
		}
		if _, err := replaceJSON(ctx, s.Objects, imageKey(repo, image), rec, gen); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrPrecondition) {
			return err
		}
	}
	return fmt.Errorf("image %s's record kept changing", image)
}

// labels are the labels an image's record calls for.
func labels(rec Image) map[string]string {
	l := map[string]string{
		LabelRepository: strconv.FormatInt(rec.RepositoryID, 10),
		LabelKind:       rec.Kind,
		LabelArch:       rec.Arch,
		LabelSaved:      day(rec.SavedAt),
	}
	switch rec.Kind {
	case KindSaved:
		l[LabelScope] = selector.Hash(rec.Scope)
		l[LabelFamily] = selector.Hash(rec.Family)
		l[LabelKey] = selector.Hash(rec.Key)
		l[LabelLastUsed] = day(rec.LastUsed)
	case KindResume:
		l[LabelRun] = strconv.FormatInt(rec.RunID, 10)
		l[LabelFamily] = selector.Hash(rec.Family)
		l[LabelJobName] = selector.Hash(rec.JobName)
	}
	return l
}

// matches reports whether an image in Compute Engine is the one a record
// describes, and ready to make disks from.
func matches(im compute.Image, rec Image) bool {
	if im.Status != "READY" {
		return false
	}
	want := labels(rec)
	for _, k := range []string{LabelRepository, LabelKind, LabelArch, LabelScope, LabelFamily, LabelKey, LabelRun, LabelJobName} {
		if im.Labels[k] != want[k] {
			return false
		}
	}
	return true
}

// make makes a recorded image from its machine's work disk, then records
// it ready. A confirmed failure is tried again under a new request ID.
func (s *Service) make(ctx context.Context, key string, rec Image, gen int64) (Image, int64, error) {
	if rec.State != ImageMaking {
		return rec, gen, nil
	}
	err := s.Compute.CreateImage(ctx, compute.ImageSpec{Name: rec.Image, SourceZone: rec.Zone, SourceDisk: machines.WorkDisk(rec.Machine),
		Labels: labels(rec)}, compute.RequestID("image", rec.Image, strconv.Itoa(rec.Attempt)))
	if err == nil || errors.Is(err, compute.ErrExists) {
		// Made, or there from an earlier request of ours, since only the
		// service makes images of these names; but it's made only once
		// Compute Engine shows it ready. One still being made is waited
		// for; a failed one is deleted, and made again by the next try.
		im, ierr := s.Compute.Image(ctx, rec.Image)
		switch {
		case ierr != nil:
			err = ierr
		case im.Status == "READY" && matches(im, rec):
			err = nil
		case im.Status == "PENDING":
			return rec, gen, fmt.Errorf("image %s is still being made", rec.Image)
		default:
			s.log().Warn("an image that failed, or isn't the one recorded, is deleted to be made again", "image", rec.Image, "status", im.Status)
			if derr := s.Compute.DeleteImage(ctx, rec.Image, freshID("delete image", rec.Image)); derr != nil && !errors.Is(derr, compute.ErrNotFound) {
				return rec, gen, derr
			}
			err = fmt.Errorf("image %s was %s", rec.Image, im.Status)
		}
	}
	if err != nil {
		if !compute.IsUnavailable(err) {
			rec.Attempt++
			if g, rerr := replaceJSON(ctx, s.Objects, key, rec, gen); rerr == nil {
				gen = g
			}
		}
		return rec, gen, err
	}
	rec.State = ImageReady
	g, err := replaceJSON(ctx, s.Objects, key, rec, gen)
	if err != nil {
		return rec, gen, err
	}
	return rec, g, nil
}
