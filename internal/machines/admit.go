package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/selector"
)

// Limits on the machines' endpoints.
const (
	machineBodyLimit = 1 << 10
	startedTimeout   = 90 * time.Second
	finishedTimeout  = 30 * time.Second
)

// StartedRequest is what the job-started hook sends. RunID is only where to
// look first: the service finds the machine's job from GitHub's record of
// which job is on its runner, never from what the machine says.
type StartedRequest struct {
	RunID int64 `json:"run_id,omitempty"`
}

// StartedAnswer is what the hook is told when its job may run: where its
// work disk is.
type StartedAnswer struct {
	Device string `json:"device"`
}

// FinishedRequest is the supervisor's report.
type FinishedRequest struct {
	Save bool `json:"save"`
}

// errNotOurs means the caller isn't a machine the service made.
var errNotOurs = errors.New("not one of the service's machines")

// errNotRunning means a machine asked to admit a job while not running.
var errNotRunning = errors.New("the machine isn't running")

// errRetry means the check got no confirmed answer, and the hook should ask
// again.
var errRetry = errors.New("no confirmed answer yet")

func retryErr(err error) error { return fmt.Errorf("%w: %w", errRetry, err) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func retryLater(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", "5")
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": msg, "retryable": true})
}

func forbidden(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]any{"error": http.StatusText(http.StatusForbidden)})
}

// caller authenticates a machine's request and finds its machine. It
// answers the request itself if it can't.
func (s *Service) caller(ctx context.Context, w http.ResponseWriter, r *http.Request, body any) (Machine, bool) {
	tok, err := auth.Bearer(r)
	var c *auth.MachineClaims
	if err == nil {
		c, err = s.Machines.Verify(ctx, tok)
	}
	if err != nil {
		code := http.StatusUnauthorized
		if errors.Is(err, auth.ErrForbidden) {
			code = http.StatusForbidden
		}
		s.log().Warn("machine refused", "path", r.URL.Path, "status", code, "reason", err.Error())
		writeJSON(w, code, map[string]any{"error": http.StatusText(code)})
		return Machine{}, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, machineBodyLimit+1))
	if err != nil || len(data) > machineBodyLimit {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body unreadable or too large"})
		return Machine{}, false
	}
	if len(bytes.TrimSpace(data)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body isn't a request"})
			return Machine{}, false
		}
	}
	m, err := s.identify(ctx, c)
	switch {
	case errors.Is(err, errNotOurs):
		s.log().Warn("machine refused", "path", r.URL.Path, "zone", c.Zone, "instance", c.InstanceName, "reason", err.Error())
		forbidden(w)
		return Machine{}, false
	case err != nil:
		s.log().Warn("machine not identified", "path", r.URL.Path, "instance", c.InstanceName, "err", err.Error())
		retryLater(w, "Compute Engine couldn't confirm the machine; try again")
		return Machine{}, false
	}
	return m, true
}

// identify finds the machine a token names: in one of the service's zones,
// by name, with the token's instance ID, and made by the service.
func (s *Service) identify(ctx context.Context, c *auth.MachineClaims) (Machine, error) {
	if !slices.Contains(s.Config.Shapes.Zones, c.Zone) {
		return Machine{}, fmt.Errorf("%w: zone %s isn't the service's", errNotOurs, c.Zone)
	}
	in, err := s.Compute.Instance(ctx, c.Zone, c.InstanceName)
	if errors.Is(err, compute.ErrNotFound) {
		return Machine{}, fmt.Errorf("%w: no such instance", errNotOurs)
	}
	if err != nil {
		return Machine{}, err
	}
	if in.ID != c.InstanceID {
		return Machine{}, fmt.Errorf("%w: the instance's ID isn't the token's", errNotOurs)
	}
	m, ok := ours(in)
	if !ok {
		return Machine{}, fmt.Errorf("%w: the service didn't make it", errNotOurs)
	}
	return m, nil
}

// JobStarted is the job-started check's endpoint.
func (s *Service) JobStarted() http.Handler { return http.HandlerFunc(s.jobStarted) }

// JobFinished is the supervisor's report's endpoint.
func (s *Service) JobFinished() http.Handler { return http.HandlerFunc(s.jobFinished) }

func (s *Service) jobStarted(w http.ResponseWriter, r *http.Request) {
	// A check carries on if its caller goes: a refused machine is stopped
	// mid-request, which ends its hook's connection, and what follows the
	// stop must still be done.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), startedTimeout)
	defer cancel()
	var req StartedRequest
	m, ok := s.caller(ctx, w, r, &req)
	if !ok {
		return
	}
	a, err := s.admit(ctx, m, req.RunID)
	switch {
	case errors.Is(err, errNotRunning):
		s.log().Warn("job-started check from a machine that isn't running", "machine", m.Name, "status", m.Status)
		forbidden(w)
	case err != nil:
		s.log().Warn("job-started check not answered", "machine", m.Name, "err", err.Error())
		retryLater(w, "the job-started check got no confirmed answer; ask again")
	case !a.Allowed:
		forbidden(w)
	default:
		writeJSON(w, http.StatusOK, StartedAnswer{Device: "/dev/disk/by-id/google-" + WorkDevice})
	}
}

// admit decides once whether the job on a machine's runner may run there,
// and carries out the decision: a refused machine is stopped at once, and
// an allowed job gets its work disk. A retried check carries on from what's
// recorded.
func (s *Service) admit(ctx context.Context, m Machine, hint int64) (Admission, error) {
	a, _, found, err := getJSON[Admission](ctx, s.Objects, admissionKey(m.Name))
	if err != nil {
		return Admission{}, err
	}
	if !found {
		if m.Status != "RUNNING" {
			// A token outlives its machine's guest: a machine that isn't
			// running has no job to admit.
			return Admission{}, errNotRunning
		}
		repo, err := s.GitHub.RepositoryByID(ctx, m.RepositoryID)
		if err != nil {
			// A confirmed answer that the repository is gone leaves the
			// machine with no job it may run; it stops on its own when
			// cleanup finds it idle.
			return Admission{}, retryErr(err)
		}
		job, run, err := s.findJob(ctx, repo, m, hint)
		if err != nil {
			return Admission{}, err
		}
		decided, err := s.decide(ctx, repo, m, job, run)
		if err != nil {
			return Admission{}, err
		}
		if a, _, err = createOnce(ctx, s.Objects, admissionKey(m.Name), decided); err != nil {
			return Admission{}, retryErr(err)
		}
		if a.Allowed {
			s.log().Info("job admitted", "machine", m.Name, "repository_id", a.RepositoryID, "run_id", a.RunID, "job_id", a.JobID,
				"selector", a.Selector, "contribution", a.Contribution)
		} else {
			s.log().Warn("job refused", "machine", m.Name, "repository_id", a.RepositoryID, "run_id", a.RunID, "job_id", a.JobID,
				"selector", a.Selector, "reason", a.Reason)
		}
	}
	if !a.Allowed {
		s.refuse(ctx, m, a)
		return a, nil
	}
	if err := s.workDisk(ctx, m, a); err != nil {
		return a, retryErr(err)
	}
	return a, nil
}

var errNoAssignment = errors.New("GitHub shows no job on the machine's runner")

// findJob asks GitHub which job is on the machine's runner, by the job's
// runner ID and name, retrying for a while until GitHub shows the
// assignment.
func (s *Service) findJob(ctx context.Context, repo github.Repository, m Machine, hint int64) (github.Job, github.WorkflowRun, error) {
	wait, every := s.AssignmentWait, s.AssignmentRetry
	if wait == 0 {
		wait = assignmentWait
	}
	if every == 0 {
		every = assignmentRetry
	}
	end := time.Now().Add(wait)
	for {
		job, run, err := s.searchJob(ctx, repo, m, hint)
		if err == nil {
			return job, run, nil
		}
		if !errors.Is(err, errNoAssignment) && !github.IsUnavailable(err) {
			return github.Job{}, github.WorkflowRun{}, retryErr(err)
		}
		if !time.Now().Add(every).Before(end) {
			return github.Job{}, github.WorkflowRun{}, retryErr(err)
		}
		select {
		case <-ctx.Done():
			return github.Job{}, github.WorkflowRun{}, retryErr(ctx.Err())
		case <-time.After(every):
		}
	}
}

func (s *Service) searchJob(ctx context.Context, repo github.Repository, m Machine, hint int64) (github.Job, github.WorkflowRun, error) {
	match := func(run github.WorkflowRun, jobs []github.Job) (github.Job, bool) {
		for _, j := range jobs {
			if j.RunnerID == m.RunnerID && strings.EqualFold(j.RunnerName, m.Name) && j.Status == "in_progress" && j.RunID == run.ID {
				return j, true
			}
		}
		return github.Job{}, false
	}
	look := func(run github.WorkflowRun) (github.Job, bool, error) {
		if run.RepositoryID != repo.ID {
			return github.Job{}, false, nil
		}
		jobs, err := s.GitHub.RunJobs(ctx, repo, run.ID)
		if errors.Is(err, github.ErrNotFound) {
			return github.Job{}, false, nil
		}
		if err != nil {
			return github.Job{}, false, err
		}
		j, ok := match(run, jobs)
		return j, ok, nil
	}
	if hint > 0 {
		run, err := s.GitHub.WorkflowRun(ctx, repo, hint)
		if err != nil && !errors.Is(err, github.ErrNotFound) {
			return github.Job{}, github.WorkflowRun{}, err
		}
		if err == nil {
			if j, ok, err := look(run); err != nil {
				return github.Job{}, github.WorkflowRun{}, err
			} else if ok {
				return j, run, nil
			}
		}
	}
	runs, err := s.GitHub.Runs(ctx, repo, "in_progress")
	if err != nil {
		return github.Job{}, github.WorkflowRun{}, err
	}
	for _, run := range runs {
		if run.ID == hint {
			continue
		}
		if j, ok, err := look(run); err != nil {
			return github.Job{}, github.WorkflowRun{}, err
		} else if ok {
			return j, run, nil
		}
	}
	return github.Job{}, github.WorkflowRun{}, errNoAssignment
}

// isFork reports whether a run is from a fork's pull request: its head is
// in another repository, by GitHub's run record or, for a pull_request_target
// run, whose record is the base's, by its binding. A pull_request_target run
// with no pull request's binding counts as a fork's.
func isFork(repo github.Repository, run github.WorkflowRun, bd binding.Binding, bound bool) bool {
	if run.HeadRepositoryID != repo.ID {
		return true
	}
	return run.Event == "pull_request_target" && (!bound || bd.HeadRepositoryID != repo.ID)
}

// approved reports whether GitHub records an approval of the contribution
// environment for a run.
func (s *Service) approved(ctx context.Context, repo github.Repository, runID int64) (bool, error) {
	as, err := s.GitHub.RunApprovals(ctx, repo, runID)
	if err != nil {
		return false, err
	}
	for _, a := range as {
		if a.State == "approved" && slices.ContainsFunc(a.Environments, func(e string) bool { return strings.EqualFold(e, ContributionEnvironment) }) {
			return true, nil
		}
	}
	return false, nil
}

// decide applies the job-started check's four conditions to the job on a
// machine's runner (specs/0002-machines.md, The job-started check). An error
// means GitHub or the bucket gave no confirmed answer.
func (s *Service) decide(ctx context.Context, repo github.Repository, m Machine, job github.Job, run github.WorkflowRun) (Admission, error) {
	a := Admission{Machine: m.Name, Zone: m.Zone, RepositoryID: m.RepositoryID, JobID: job.ID, RunID: run.ID,
		RunAttempt: job.RunAttempt, JobName: job.Name, Selector: m.Selector.String(), At: s.now()}
	refuse := func(reason string, cancel bool) (Admission, error) {
		a.Allowed, a.Reason, a.Cancel = false, reason, cancel
		return a, nil
	}
	// The job's repository is the machine's consumer, and the
	// organization's.
	if repo.ID != m.RepositoryID || run.RepositoryID != repo.ID || repo.OwnerID != s.Config.Organization.ID {
		return refuse("the job's repository isn't the machine's consumer", false)
	}
	bd, bound, err := s.Binder.Lookup(ctx, repo.ID, run.ID)
	if err != nil {
		return Admission{}, retryErr(err)
	}
	// Its run isn't from a fork's pull request, unless it's an approved
	// contribution.
	if isFork(repo, run, bd, bound) {
		if run.Event != "pull_request_target" {
			return refuse("the run is a fork's pull request's", true)
		}
		ok, err := s.approved(ctx, repo, run.ID)
		if github.IsUnavailable(err) {
			return Admission{}, retryErr(err)
		}
		if err != nil || !ok {
			return refuse("the contribution's run has no approval of the "+ContributionEnvironment+" environment", true)
		}
		a.Contribution = true
		// A contribution counts against its own limit, which only the
		// machines made for contributions do.
		if !m.Contribution {
			return refuse("a contribution's job on a machine not made for contributions", false)
		}
	}
	// Its labels hold exactly one selector, the machine's, with nothing
	// else but GitHub's own labels that the runner has.
	if err := selector.MatchesMachine(job.Labels, m.Selector); err != nil {
		return refuse(err.Error(), false)
	}
	// The selector's scope is the run's binding.
	if !bound {
		return refuse("the job's run has no binding", true)
	}
	if bd.Scope != m.Selector.Scope {
		return refuse(fmt.Sprintf("the selector's scope %s isn't the run's binding, %s", m.Selector.Scope, bd.Scope), true)
	}
	a.Allowed = true
	return a, nil
}

// refuse carries out a refusal: the machine stops at once, and the run of a
// job refused for coming from a fork or claiming a scope is recorded and
// cancelled. Anything left undone here, a pass does, from the admission.
func (s *Service) refuse(ctx context.Context, m Machine, a Admission) {
	if err := s.Compute.StopInstance(ctx, m.Zone, m.Name, compute.RequestID("stop", m.Name)); err != nil && !errors.Is(err, compute.ErrNotFound) {
		s.log().Error("stopping a refused machine", "machine", m.Name, "err", err.Error())
	}
	if !a.Cancel {
		return
	}
	rf, gen, err := s.recordRefusal(ctx, a)
	if err != nil {
		s.log().Error("recording a refused run", "run_id", a.RunID, "err", err.Error())
		return
	}
	if rf.CancelledAt.IsZero() && rf.FinishedAt.IsZero() {
		if repo, err := s.GitHub.RepositoryByID(ctx, a.RepositoryID); err == nil {
			s.cancel(ctx, repo, rf, gen)
		}
	}
}

// recordRefusal records a refused run, once, with the refused job's
// selector among those no machine is made for.
func (s *Service) recordRefusal(ctx context.Context, a Admission) (Refusal, int64, error) {
	key := refusalKey(a.RepositoryID, a.RunID)
	rf := Refusal{RepositoryID: a.RepositoryID, RunID: a.RunID, Reason: a.Reason, Selectors: []string{a.Selector}, RefusedAt: s.now()}
	got, gen, err := createOnce(ctx, s.Objects, key, rf)
	if err != nil {
		return Refusal{}, 0, err
	}
	if !slices.Contains(got.Selectors, a.Selector) {
		got.Selectors = append(got.Selectors, a.Selector)
		if gen, err = replaceJSON(ctx, s.Objects, key, got, gen); err != nil {
			return Refusal{}, 0, err
		}
	}
	return got, gen, nil
}

// cancel asks GitHub to cancel a refused run, or to force it once it's been
// refused for longer than an ordinary cancellation should take, counted
// from the refusal's record, which is written before any cancellation; and
// records what it asked. Only GitHub's record of the run as completed
// finishes a refusal: GitHub can answer a cancellation with a conflict while
// the run goes on.
func (s *Service) cancel(ctx context.Context, repo github.Repository, rf Refusal, gen int64) {
	now := s.now()
	key := refusalKey(rf.RepositoryID, rf.RunID)
	var err error
	switch {
	case now.Sub(rf.RefusedAt) >= ForceCancel:
		err = s.GitHub.ForceCancelRun(ctx, repo, rf.RunID)
		if err == nil {
			rf.ForcedAt = now
			s.log().Warn("refused run force-cancelled", "repository_id", rf.RepositoryID, "run_id", rf.RunID)
		}
	case rf.LastCancel.IsZero() || now.Sub(rf.LastCancel) >= CancelEvery:
		err = s.GitHub.CancelRun(ctx, repo, rf.RunID)
		if err == nil {
			if rf.CancelledAt.IsZero() {
				rf.CancelledAt = now
			}
			rf.LastCancel = now
			s.log().Info("refused run cancelled", "repository_id", rf.RepositoryID, "run_id", rf.RunID)
		}
	default:
		return
	}
	if errors.Is(err, github.ErrConflict) {
		run, rerr := s.GitHub.WorkflowRun(ctx, repo, rf.RunID)
		switch {
		case rerr == nil && run.Status == "completed", errors.Is(rerr, github.ErrNotFound):
			rf.FinishedAt = now
			err = nil
		case rerr == nil:
			err = fmt.Errorf("GitHub refused the cancellation of a run that's %s: %w", run.Status, err)
		default:
			err = rerr
		}
	}
	if err != nil {
		s.log().Warn("cancelling a refused run", "run_id", rf.RunID, "err", err.Error())
		return
	}
	if _, err := replaceJSON(ctx, s.Objects, key, rf, gen); err != nil {
		s.log().Warn("recording a refused run's cancellation", "run_id", rf.RunID, "err", err.Error())
	}
}

// workDisk gives an admitted job its work disk: recorded before it's made,
// then made, then attached, each step recorded, so a retried check carries
// on with the disk already recorded.
func (s *Service) workDisk(ctx context.Context, m Machine, a Admission) error {
	name := WorkDisk(m.Name)
	key := diskKey(name)
	rec, gen, found, err := getJSON[DiskRecord](ctx, s.Objects, key)
	if err != nil {
		return err
	}
	if !found {
		shape := s.Config.Shapes.Shapes[m.Selector.Shape]
		if shape == nil {
			return fmt.Errorf("the machine's shape %s isn't configured", m.Selector.Shape)
		}
		size, image := int64(shape.WorkDiskGB), ""
		if s.Disks != nil {
			if image, size, err = s.Disks.Choose(ctx, m, a, size); err != nil {
				return err
			}
		}
		rec = DiskRecord{Disk: name, Machine: m.Name, Zone: m.Zone, RepositoryID: m.RepositoryID, JobID: a.JobID, State: DiskMaking,
			SizeGB: size, SourceImage: image, At: s.now()}
		if rec, gen, err = createOnce(ctx, s.Objects, key, rec); err != nil {
			return err
		}
	}
	if rec.Machine != m.Name || rec.JobID != a.JobID {
		return fmt.Errorf("work disk %s is recorded for another machine or job", name)
	}
	step := func(state string) error {
		rec.State, rec.At = state, s.now()
		g, err := replaceJSON(ctx, s.Objects, key, rec, gen)
		if err != nil {
			return err
		}
		gen = g
		return nil
	}
	attempt := strconv.Itoa(rec.Attempt)
	// A confirmed failure is tried again under a new request ID, since
	// Compute Engine answers a request ID it has seen with its first
	// outcome.
	failed := func(err error) error {
		if compute.IsUnavailable(err) {
			return err
		}
		rec.Attempt++
		if g, rerr := replaceJSON(ctx, s.Objects, key, rec, gen); rerr == nil {
			gen = g
		}
		return err
	}
	if rec.State == DiskMaking {
		err := s.Compute.CreateDisk(ctx, m.Zone, compute.DiskSpec{
			Name: name, SizeGB: rec.SizeGB, DiskType: DiskType, SourceImage: rec.SourceImage,
			ProvisionedIOPS: int64(s.Config.Shapes.DiskIOPS), ProvisionedThroughput: int64(s.Config.Shapes.DiskThroughputMiBps),
			Labels: map[string]string{LabelRepository: strconv.FormatInt(m.RepositoryID, 10), LabelMachine: m.Name,
				LabelJob: strconv.FormatInt(a.JobID, 10), LabelCreated: strconv.FormatInt(s.now().Unix(), 10)},
		}, compute.RequestID("work disk", name, strconv.FormatInt(a.JobID, 10), attempt))
		if err != nil && !errors.Is(err, compute.ErrExists) {
			return failed(err)
		}
		if err := step(DiskMade); err != nil {
			return err
		}
	}
	if rec.State == DiskMade {
		err := s.Compute.AttachDisk(ctx, m.Zone, m.Name, name, WorkDevice, compute.RequestID("attach", name, m.Name, attempt))
		if errors.Is(err, compute.ErrInUse) || (err != nil && !compute.IsUnavailable(err)) {
			// Perhaps our own earlier attachment, whose answer was lost.
			if in, ierr := s.Compute.Instance(ctx, m.Zone, m.Name); ierr == nil &&
				slices.ContainsFunc(in.Disks, func(d compute.AttachedDisk) bool { return d.Disk == name && d.DeviceName == WorkDevice }) {
				err = nil
			}
		}
		if err != nil {
			return failed(err)
		}
		if err := step(DiskAttached); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) jobFinished(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), finishedTimeout)
	defer cancel()
	var req FinishedRequest
	m, ok := s.caller(ctx, w, r, &req)
	if !ok {
		return
	}
	rep, _, err := createOnce(ctx, s.Objects, reportKey(m.Name), Report{Machine: m.Name, Save: req.Save, At: s.now()})
	if err != nil {
		s.log().Warn("recording a report", "machine", m.Name, "err", err.Error())
		retryLater(w, "the report wasn't recorded; send it again")
		return
	}
	s.log().Info("machine reported", "machine", m.Name, "save", rep.Save)
	w.WriteHeader(http.StatusNoContent)
	if s.Kick != nil {
		s.Kick()
	}
}
