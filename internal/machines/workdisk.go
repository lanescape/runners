package machines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
)

// The work disk's endpoint (specs/0005-interface.md, Work disks).
const DisksPath = "/v1/disks"

// diskTimeout bounds a work disk request: what isn't done by then is
// carried on by the caller's next request, every 5 seconds.
const diskTimeout = 60 * time.Second

// The states a work disk request answers.
const (
	DiskStateMaking = "making"
	DiskStateReady  = "ready"
)

// DiskAnswer is the answer to POST /v1/disks.
type DiskAnswer struct {
	State    string `json:"state"`
	Restored string `json:"restored"`
}

// refuse answers a request the service refuses, saying why.
func refuse(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusForbidden, map[string]any{"error": http.StatusText(http.StatusForbidden), "reason": reason})
}

// runningJob is a running job's request: the machine it came from, the
// job the check admitted there, its repository and its run's binding.
type runningJob struct {
	Machine    Machine
	Admission  Admission
	Repository github.Repository
	Binding    binding.Binding
}

// activeJob authenticates a request from a job, signed with its machine's
// identity token, and reads its body. The service answers only a machine of
// its own whose job it admitted, while that job runs, by its machine's
// state, its report and GitHub's record of the job, for that job's run
// once it's bound, and of a repository that's still the organization's
// (specs/0005-interface.md, Requests from a job); activeJob answers any
// other request itself, and reports whether the caller should go on.
func (s *Service) activeJob(ctx context.Context, w http.ResponseWriter, r *http.Request, body any) (runningJob, bool) {
	m, ok := s.caller(ctx, w, r, body)
	if !ok {
		return runningJob{}, false
	}
	a, found, err := s.AdmissionOf(ctx, m.Name)
	if err != nil {
		retryLater(w, "the machine's job couldn't be read; try again")
		return runningJob{}, false
	}
	if !found || !a.Allowed {
		refuse(w, "this machine has no admitted job")
		return runningJob{}, false
	}
	if m.Status != "RUNNING" {
		refuse(w, "this machine's job has ended")
		return runningJob{}, false
	}
	if _, _, reported, err := getJSON[Report](ctx, s.Objects, reportKey(m.Name)); err != nil {
		retryLater(w, "the machine's job couldn't be read; try again")
		return runningJob{}, false
	} else if reported {
		refuse(w, "this machine's job has ended")
		return runningJob{}, false
	}
	if a.Unbound {
		refuse(w, "the run isn't bound: a contribution's run is bound once a maintainer approves its contribution job")
		return runningJob{}, false
	}
	repo, err := s.GitHub.RepositoryByID(ctx, a.RepositoryID)
	if err != nil {
		retryLater(w, "GitHub couldn't confirm the repository; try again")
		return runningJob{}, false
	}
	if repo.OwnerID != s.Config.Organization.ID {
		refuse(w, "the repository is no longer the organization's")
		return runningJob{}, false
	}
	// A job GitHub shows completed has ended, even if its machine hasn't
	// reported yet, such as when its supervisor died: what's left on the
	// machine is no longer the job. A job GitHub doesn't show, in a
	// repository it shows as the organization's, has gone too.
	job, err := s.GitHub.Job(ctx, repo, a.JobID)
	switch {
	case errors.Is(err, github.ErrNotFound) || err == nil && job.Status == "completed":
		refuse(w, "this machine's job has ended")
		return runningJob{}, false
	case err != nil:
		retryLater(w, "GitHub couldn't confirm the job; try again")
		return runningJob{}, false
	case job.Status != "in_progress":
		retryLater(w, "GitHub doesn't show the job running yet; try again")
		return runningJob{}, false
	}
	bd, bound, err := s.Binder.Lookup(ctx, repo.ID, a.RunID)
	if err != nil {
		retryLater(w, "the run's binding couldn't be read; try again")
		return runningJob{}, false
	}
	if !bound {
		refuse(w, "the run isn't bound")
		return runningJob{}, false
	}
	return runningJob{Machine: m, Admission: a, Repository: repo, Binding: bd}, true
}

// DiskAsk is the body of POST /v1/disks: the work disk a job asks for, any
// of whose settings it may leave out for the service's defaults. Each is
// read as the JSON it was sent as, so that one that isn't a positive whole
// number is refused with a reason rather than as a body that isn't a
// request.
type DiskAsk struct {
	SizeGB          json.RawMessage `json:"size_gb,omitempty"`
	IOPS            json.RawMessage `json:"iops,omitempty"`
	ThroughputMiBps json.RawMessage `json:"throughput_mibps,omitempty"`
}

// DiskSettings is a work disk's size and provisioned performance.
type DiskSettings struct {
	SizeGB          int64
	IOPS            int64
	ThroughputMiBps int64
}

// wholeNumber is a JSON number that's a positive whole number.
var wholeNumber = regexp.MustCompile(`^[1-9][0-9]*$`)

// given is what d names, each nil where it's left out, or why the service
// refuses it: each must be a positive whole number.
func (d DiskAsk) given() ([3]*int64, string) {
	var out [3]*int64
	for i, f := range []struct {
		name string
		raw  json.RawMessage
	}{{"size_gb", d.SizeGB}, {"iops", d.IOPS}, {"throughput_mibps", d.ThroughputMiBps}} {
		if f.raw == nil {
			continue
		}
		n, err := strconv.ParseInt(string(f.raw), 10, 64)
		if !wholeNumber.Match(f.raw) || err != nil {
			return [3]*int64{}, fmt.Sprintf("the work disk's %s must be a positive whole number", f.name)
		}
		out[i] = &n
	}
	return out, ""
}

// Settings is what a job asked for, with the defaults shapes gives in place
// of what it left out, or why the service refuses it: each must be a
// positive whole number. Whether Compute Engine allows them, for the
// disk's type and the job's machine, is Compute Engine's to say.
func (d DiskAsk) Settings(shapes config.Shapes) (DiskSettings, string) {
	g, why := d.given()
	if why != "" {
		return DiskSettings{}, why
	}
	out := DiskSettings{SizeGB: int64(shapes.WorkDiskGB), IOPS: int64(shapes.DiskIOPS), ThroughputMiBps: int64(shapes.DiskThroughputMiBps)}
	for i, to := range []*int64{&out.SizeGB, &out.IOPS, &out.ThroughputMiBps} {
		if g[i] != nil {
			*to = *g[i]
		}
	}
	return out, ""
}

// Names reports whether d names any setting.
func (d DiskAsk) Names() bool {
	return d.SizeGB != nil || d.IOPS != nil || d.ThroughputMiBps != nil
}

// Matches reports whether what d names is what was asked for before,
// whose defaults may since have changed.
func (d DiskAsk) Matches(before DiskSettings) bool {
	g, why := d.given()
	if why != "" {
		return false
	}
	for i, was := range []int64{before.SizeGB, before.IOPS, before.ThroughputMiBps} {
		if g[i] != nil && *g[i] != was {
			return false
		}
	}
	return true
}

// DiskRequest is the work disk's endpoint, POST /v1/disks.
func (s *Service) DiskRequest() http.Handler { return http.HandlerFunc(s.disk) }

// disk gives a running job its work disk (specs/0002-machines.md, The work
// disk), of the size and performance it asks for: empty, or, for the rerun
// of a job lost to a preemption or the age limit, made from its resume
// image. A job has one work disk: a later request carries on with the same
// disk, and must ask for what the first did.
func (s *Service) disk(w http.ResponseWriter, r *http.Request) {
	// The disk is carried on if its caller goes, so that what a request
	// started is recorded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), diskTimeout)
	defer cancel()
	var body DiskAsk
	j, ok := s.activeJob(ctx, w, r, &body)
	if !ok {
		return
	}
	want, why := body.Settings(s.Config.Shapes)
	if why != "" {
		refuse(w, why)
		return
	}
	m, a := j.Machine, j.Admission
	log := s.log().With("machine", m.Name, "repository_id", a.RepositoryID, "run_id", a.RunID, "job_id", a.JobID)
	key := diskKey(WorkDisk(m.Name))
	rec, gen, found, err := getJSON[DiskRecord](ctx, s.Objects, key)
	if err != nil {
		retryLater(w, "the work disk's record couldn't be read; try again")
		return
	}
	if !found {
		if rec, gen, why, err = s.chooseDisk(ctx, m, a, want); err != nil {
			log.Warn("choosing a work disk", "err", err.Error())
			retryLater(w, "the work disk couldn't be chosen; try again")
			return
		}
		if why != "" {
			refuse(w, why)
			return
		}
	}
	if rec.Machine != m.Name || rec.JobID != a.JobID {
		refuse(w, "the machine's work disk is recorded for another job")
		return
	}
	// A record from before jobs asked for their disks asked for nothing, so
	// it answers only a request that asks for nothing either.
	if rec.IOPS == 0 && body.Names() {
		refuse(w, "the job's work disk was asked for before the service took a disk's settings, with none; a job has one work disk")
		return
	}
	if rec.IOPS != 0 && !body.Matches(rec.asked()) {
		refuse(w, fmt.Sprintf("the job's work disk was asked for as %s; a job has one work disk", rec.asked()))
		return
	}
	if err := s.attach(ctx, m, a, &rec, gen); err != nil {
		// Carried on by the caller's next request.
		log.Warn("making or attaching a work disk", "disk", rec.Disk, "state", rec.State, "err", err.Error())
	}
	if rec.State == DiskRefused {
		refuse(w, rec.Refused)
		return
	}
	state := DiskStateMaking
	if rec.State == DiskAttached {
		state = DiskStateReady
	}
	writeJSON(w, http.StatusOK, DiskAnswer{State: state, Restored: rec.Restored})
}

func (d DiskSettings) String() string {
	return fmt.Sprintf("%d GB, %d IOPS and %d MiB/s", d.SizeGB, d.IOPS, d.ThroughputMiBps)
}

// chooseDisk picks what a job's work disk starts from, and records the disk
// before it's made, or says why the service refuses it. A request that
// raced another gets the record written first.
func (s *Service) chooseDisk(ctx context.Context, m Machine, a Admission, want DiskSettings) (DiskRecord, int64, string, error) {
	size, image, restored := want.SizeGB, "", NotRestored
	if s.Disks != nil {
		var err error
		if image, size, restored, err = s.Disks.Choose(ctx, a.Resolve(m), a, size); err != nil {
			return DiskRecord{}, 0, "", err
		}
	}
	if why, err := s.diskFits(ctx, m, size); err != nil || why != "" {
		return DiskRecord{}, 0, why, err
	}
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	rec := DiskRecord{Disk: WorkDisk(m.Name), Machine: m.Name, Zone: m.Zone, RepositoryID: m.RepositoryID, JobID: a.JobID, State: DiskMaking,
		Restored: restored, LegacyFamily: LegacyFamily, SizeGB: size, RequestedGB: want.SizeGB, IOPS: want.IOPS,
		ThroughputMiBps: want.ThroughputMiBps, SourceImage: image, Nonce: hex.EncodeToString(nonce[:]), At: s.now()}
	rec, gen, err := createOnce(ctx, s.Objects, diskKey(rec.Disk), rec)
	return rec, gen, "", err
}

// diskFits says why a work disk of sizeGB can't be made for m, or "", by
// the sizes Compute Engine reports its disk type may be and the most the
// disks of m's machine type may hold in all, its boot disk included. What
// Compute Engine doesn't report, it decides when the disk is made.
func (s *Service) diskFits(ctx context.Context, m Machine, sizeGB int64) (string, error) {
	dt, err := s.Compute.DiskType(ctx, m.Zone, DiskType)
	if err != nil {
		return "", fmt.Errorf("reading disk type %s: %w", DiskType, err)
	}
	if dt.MinSizeGB > 0 && sizeGB < dt.MinSizeGB || dt.MaxSizeGB > 0 && sizeGB > dt.MaxSizeGB {
		return fmt.Sprintf("the work disk would be %d GB, but Compute Engine makes %s disks of %d to %d GB",
			sizeGB, DiskType, dt.MinSizeGB, dt.MaxSizeGB), nil
	}
	mt, err := s.Compute.MachineType(ctx, m.Zone, m.MachineType)
	if err != nil {
		return "", fmt.Errorf("reading machine type %s: %w", m.MachineType, err)
	}
	if boot := int64(s.Config.Shapes.BootDiskGB); mt.MaxDisksSizeGB > 0 && boot+sizeGB > mt.MaxDisksSizeGB {
		return fmt.Sprintf("the work disk would be %d GB, but Compute Engine lets a %s machine's disks hold %d GB in all, and its boot disk is %d GB",
			sizeGB, m.MachineType, mt.MaxDisksSizeGB, boot), nil
	}
	return "", nil
}

// attach makes a recorded work disk and attaches it to its running machine,
// as the device named work, each step recorded, so that a retried request
// carries on with the disk already recorded (specs/0002-machines.md, The
// work disk). The base image mounts it at /work once it appears.
func (s *Service) attach(ctx context.Context, m Machine, a Admission, rec *DiskRecord, gen int64) error {
	key := diskKey(rec.Disk)
	step := func(state string) error {
		next := *rec
		next.State, next.At = state, s.now()
		g, err := replaceJSON(ctx, s.Objects, key, next, gen)
		if err != nil {
			return err
		}
		*rec, gen = next, g
		return nil
	}
	attempt := strconv.Itoa(rec.Attempt)
	if rec.Nonce != "" {
		attempt += "/" + rec.Nonce
	}
	// A confirmed failure is tried again under a new request ID, since
	// Compute Engine answers a request ID it has seen with its first
	// outcome.
	failed := func(err error) error {
		if compute.IsUnavailable(err) {
			return err
		}
		next := *rec
		next.Attempt++
		if g, rerr := replaceJSON(ctx, s.Objects, key, next, gen); rerr == nil {
			*rec, gen = next, g
		}
		return err
	}
	if rec.State == DiskMaking {
		iops, throughput := rec.IOPS, rec.ThroughputMiBps
		if iops == 0 {
			// Recorded before jobs asked for their disks.
			iops, throughput = int64(s.Config.Shapes.DiskIOPS), int64(s.Config.Shapes.DiskThroughputMiBps)
		}
		err := s.Compute.CreateDisk(ctx, m.Zone, compute.DiskSpec{
			Name: rec.Disk, SizeGB: rec.SizeGB, DiskType: DiskType, SourceImage: rec.SourceImage,
			ProvisionedIOPS: iops, ProvisionedThroughput: throughput,
			Labels: map[string]string{LabelRepository: strconv.FormatInt(m.RepositoryID, 10), LabelMachine: m.Name,
				LabelJob: strconv.FormatInt(a.JobID, 10), LabelCreated: strconv.FormatInt(s.now().Unix(), 10)},
		}, compute.RequestID("work disk", rec.Disk, strconv.FormatInt(a.JobID, 10), attempt))
		if err != nil && rec.SourceImage != "" && errors.Is(err, compute.ErrNotFound) {
			// The image it was to start from went after it was chosen: the
			// record goes, and the next request chooses again.
			s.log().Warn("a work disk's image is gone; choosing again", "machine", m.Name, "image", rec.SourceImage)
			if derr := s.Objects.Delete(ctx, key, gen); derr != nil {
				return errors.Join(err, derr)
			}
			rec.State = DiskMaking
			return err
		}
		if errors.Is(err, compute.ErrInvalid) {
			// Compute Engine refuses what the job asked for: nothing was
			// made, and every later request is answered with its reason.
			next := *rec
			next.State, next.Refused, next.At = DiskRefused, "Compute Engine refused the work disk: "+refusalText(err), s.now()
			g, rerr := replaceJSON(ctx, s.Objects, key, next, gen)
			if rerr != nil {
				// Perhaps a request at the same time recorded the refusal
				// first: its record is the answer.
				if cur, cg, ok, gerr := getJSON[DiskRecord](ctx, s.Objects, key); gerr == nil && ok && cur.State == DiskRefused && cur.Disk == rec.Disk {
					*rec, gen = cur, cg
					return nil
				}
				return errors.Join(err, rerr)
			}
			*rec, gen = next, g
			s.log().Info("work disk refused by Compute Engine", "machine", m.Name, "disk", rec.Disk, "err", err.Error())
			return nil
		}
		if err != nil && !errors.Is(err, compute.ErrExists) {
			return failed(err)
		}
		if err := step(DiskMade); err != nil {
			return err
		}
	}
	if rec.State == DiskMade {
		err := s.Compute.AttachDisk(ctx, m.Zone, m.Name, rec.Disk, WorkDevice, compute.RequestID("attach", rec.Disk, m.Name, attempt))
		if errors.Is(err, compute.ErrInUse) || (err != nil && !compute.IsUnavailable(err)) {
			// Perhaps our own earlier attachment, whose answer was lost.
			if in, ierr := s.Compute.Instance(ctx, m.Zone, m.Name); ierr == nil &&
				slices.ContainsFunc(in.Disks, func(d compute.AttachedDisk) bool { return d.Disk == rec.Disk && d.DeviceName == WorkDevice }) {
				err = nil
			}
		}
		if err != nil {
			return failed(err)
		}
		if err := step(DiskAttached); err != nil {
			return err
		}
		s.log().Info("work disk attached", "machine", m.Name, "disk", rec.Disk, "restored", rec.Restored)
	}
	return nil
}

// refusalText is Compute Engine's own message in an ErrInvalid.
func refusalText(err error) string {
	msg := err.Error()
	if _, after, ok := strings.Cut(msg, compute.ErrInvalid.Error()+": "); ok {
		return after
	}
	return msg
}

// asked is what the job asked for its disk.
func (d DiskRecord) asked() DiskSettings {
	return DiskSettings{SizeGB: d.RequestedGB, IOPS: d.IOPS, ThroughputMiBps: d.ThroughputMiBps}
}
