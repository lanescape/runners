package machines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/selector"
)

// The work disk's endpoints (specs/0005-interface.md, Work disks).
const (
	DisksPath = "/v1/disks"
	SavePath  = "/v1/disks/save"
)

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

// SaveAnswer is the answer to POST /v1/disks/save.
type SaveAnswer struct {
	Saves  bool   `json:"saves"`
	Reason string `json:"reason,omitempty"`
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
// its own whose job it admitted, while that job runs, for that job's run
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

// DiskRequest is the work disk's endpoint, POST /v1/disks.
func (s *Service) DiskRequest() http.Handler { return http.HandlerFunc(s.disk) }

// SaveRequest is the save request's endpoint, POST /v1/disks/save.
func (s *Service) SaveRequest() http.Handler { return http.HandlerFunc(s.save) }

// disk gives a running job its work disk of the family it names
// (specs/0002-machines.md, The work disk): the job chooses the family, the
// service everything else. A job's first request decides its family; a
// later one for the same family carries on with the same disk, and one for
// another is refused.
func (s *Service) disk(w http.ResponseWriter, r *http.Request) {
	// The disk is carried on if its caller goes, so that what a request
	// started is recorded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), diskTimeout)
	defer cancel()
	var body struct {
		Family string `json:"family"`
	}
	j, ok := s.activeJob(ctx, w, r, &body)
	if !ok {
		return
	}
	m, a := j.Machine, j.Admission
	log := s.log().With("machine", m.Name, "repository_id", a.RepositoryID, "run_id", a.RunID, "job_id", a.JobID)
	if !selector.ValidName(body.Family) {
		refuse(w, "the body names no family: 1 to 32 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen")
		return
	}
	key := diskKey(WorkDisk(m.Name))
	rec, gen, found, err := getJSON[DiskRecord](ctx, s.Objects, key)
	if err != nil {
		retryLater(w, "the work disk's record couldn't be read; try again")
		return
	}
	if !found {
		rf, err := s.runFiles(ctx, j.Repository, j.Binding)
		if err != nil {
			log.Warn("reading the run's files", "err", err.Error())
			retryLater(w, "the run's files couldn't be read; try again")
			return
		}
		fkey, err := familyKey(rf, body.Family)
		var ef errFiles
		if errors.As(err, &ef) {
			log.Info("work disk refused", "family", body.Family, "reason", ef.reason)
			refuse(w, ef.reason)
			return
		}
		if rec, gen, err = s.chooseDisk(ctx, m, a, body.Family, fkey); err != nil {
			log.Warn("choosing a work disk", "family", body.Family, "err", err.Error())
			retryLater(w, "the work disk couldn't be chosen; try again")
			return
		}
	}
	if rec.Machine != m.Name || rec.JobID != a.JobID {
		refuse(w, "the machine's work disk is recorded for another job")
		return
	}
	if rec.Family != body.Family {
		refuse(w, fmt.Sprintf("this job's work disk is of the family %s: a job has one work disk", rec.Family))
		return
	}
	if err := s.attach(ctx, m, a, &rec, gen); err != nil {
		// Carried on by the caller's next request.
		log.Warn("making or attaching a work disk", "disk", rec.Disk, "state", rec.State, "err", err.Error())
	}
	state := DiskStateMaking
	if rec.State == DiskAttached {
		state = DiskStateReady
	}
	writeJSON(w, http.StatusOK, DiskAnswer{State: state, Restored: rec.Restored})
}

// chooseDisk picks what a job's work disk of a family starts from, and
// records the disk before it's made. A request that raced another gets the
// record written first.
func (s *Service) chooseDisk(ctx context.Context, m Machine, a Admission, family, key string) (DiskRecord, int64, error) {
	shape := s.Config.Shapes.Shapes[m.Selector.Shape]
	if shape == nil {
		return DiskRecord{}, 0, fmt.Errorf("the machine's shape %s isn't configured", m.Selector.Shape)
	}
	size, image, restored := int64(shape.WorkDiskGB), "", "none"
	if s.Disks != nil {
		resolved := a.Resolve(m, nil)
		resolved.Selector.Family, resolved.Selector.Key = family, key
		var err error
		if image, size, restored, err = s.Disks.Choose(ctx, resolved, a, size); err != nil {
			return DiskRecord{}, 0, err
		}
	}
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	rec := DiskRecord{Disk: WorkDisk(m.Name), Machine: m.Name, Zone: m.Zone, RepositoryID: m.RepositoryID, JobID: a.JobID, State: DiskMaking,
		Family: family, Key: key, Restored: restored, SizeGB: size, SourceImage: image, Nonce: hex.EncodeToString(nonce[:]), At: s.now()}
	return createOnce(ctx, s.Objects, diskKey(rec.Disk), rec)
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
		err := s.Compute.CreateDisk(ctx, m.Zone, compute.DiskSpec{
			Name: rec.Disk, SizeGB: rec.SizeGB, DiskType: DiskType, SourceImage: rec.SourceImage,
			ProvisionedIOPS: int64(s.Config.Shapes.DiskIOPS), ProvisionedThroughput: int64(s.Config.Shapes.DiskThroughputMiBps),
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
		s.log().Info("work disk attached", "machine", m.Name, "disk", rec.Disk, "family", rec.Family, "restored", rec.Restored)
	}
	return nil
}

// save records a running job's request that its work disk be saved once it
// has finished (specs/0003-disks.md). Whether it's saved is still the
// service's to decide then, from GitHub's conclusion for the job.
func (s *Service) save(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), finishedTimeout)
	defer cancel()
	var body struct {
		Save *bool `json:"save"`
	}
	j, ok := s.activeJob(ctx, w, r, &body)
	if !ok {
		return
	}
	m, a := j.Machine, j.Admission
	if body.Save == nil || !*body.Save {
		refuse(w, `the body must be {"save": true}`)
		return
	}
	rec, _, found, err := getJSON[DiskRecord](ctx, s.Objects, diskKey(WorkDisk(m.Name)))
	if err != nil {
		retryLater(w, "the work disk's record couldn't be read; try again")
		return
	}
	if !found || rec.Machine != m.Name || rec.JobID != a.JobID {
		refuse(w, "this job has no work disk: a job asks for one with the disk action before it asks to save it")
		return
	}
	if _, _, err := createOnce(ctx, s.Objects, saveRequestKey(m.Name), SaveRequest{Machine: m.Name, JobID: a.JobID, At: s.now()}); err != nil {
		retryLater(w, "the save request wasn't recorded; try again")
		return
	}
	s.log().Info("work disk save asked for", "machine", m.Name, "repository_id", a.RepositoryID, "run_id", a.RunID, "job_id", a.JobID,
		"family", rec.Family, "scope", a.Scope.String())
	if !a.Scope.Saves() {
		writeJSON(w, http.StatusOK, SaveAnswer{Saves: false, Reason: "the run's scope, " + a.Scope.String() + ", saves nothing"})
		return
	}
	writeJSON(w, http.StatusOK, SaveAnswer{Saves: true})
}
