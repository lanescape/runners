package machines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/store"
)

var jsonUnmarshal = json.Unmarshal

// cleanup deletes what's finished, too old or orphaned
// (specs/0002-machines.md, the table of what a pass finds).
func (s *Service) cleanup(ctx context.Context, inv *inventory) {
	names := make([]string, 0, len(inv.machines))
	for n := range inv.machines {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if ctx.Err() != nil {
			return
		}
		s.tidy(ctx, inv, inv.machines[n])
	}
	complete := inv.machinesComplete && inv.recordsComplete
	if complete && inv.disksComplete {
		s.tidyDisks(ctx, inv)
	}
	if complete {
		s.tidyRunners(ctx, inv)
		s.tidyRecords(ctx, inv)
	}
}

// stopPastLimit stops every running machine past its age limit, before the
// pass changes anything else: recording a job the limit cut short, and
// making its resume image, can take longer than a pass, and must never hold
// up another machine's stop past Compute Engine's backstop
// (specs/0002-machines.md, The age limit). Every stop is asked for at once,
// so that one Compute Engine is slow to finish holds up no other. A machine
// it stops is stopped in the inventory too, as of when the stop finished;
// one it can't stop is left for the next pass.
func (s *Service) stopPastLimit(ctx context.Context, inv *inventory) {
	var due []Machine
	for _, m := range inv.machines {
		if s.now().Sub(m.CreatedAt) >= MaxAge && !m.Stopped() {
			due = append(due, m)
		}
	}
	stopped := make([]time.Time, len(due))
	var wg sync.WaitGroup
	for i, m := range due {
		wg.Go(func() {
			err := s.Compute.StopInstance(ctx, m.Zone, m.Name, s.requestID("stop", m.Name))
			if err != nil && !errors.Is(err, compute.ErrNotFound) {
				s.log().Warn("stopping a machine past its age limit", "machine", m.Name, "err", err.Error())
				return
			}
			stopped[i] = s.now()
			s.log().Info("machine stopped at its age limit", "machine", m.Name, "repository_id", m.RepositoryID)
		})
	}
	wg.Wait()
	for i, m := range due {
		if !stopped[i].IsZero() {
			m.Status, m.LastStop = "TERMINATED", stopped[i]
			inv.machines[m.Name] = m
		}
	}
}

// refresh reads a machine's admission, report and work disk's record again,
// for a machine past its age limit: a job-started check, a report or a disk
// request may have finished since the pass read the records. A machine with
// no admission is retired, as an idle one is, so that a check that comes
// later finds it refused.
func (s *Service) refresh(ctx context.Context, inv *inventory, m Machine) error {
	a, err := s.claim(ctx, m, "it was past its age limit before its job was admitted")
	if err != nil {
		return err
	}
	inv.admissions[m.Name] = a
	rep, _, ok, err := getJSON[Report](ctx, s.Objects, reportKey(m.Name))
	if err != nil {
		return err
	}
	if ok {
		inv.reports[m.Name] = rep
	}
	disk := WorkDisk(m.Name)
	d, gen, ok, err := getJSON[DiskRecord](ctx, s.Objects, diskKey(disk))
	if err != nil {
		return err
	}
	if ok {
		inv.diskRecords[disk] = record[DiskRecord]{d, gen}
	}
	return nil
}

// lostAtLimit reports whether a machine past its age limit had a job the
// limit cut short: admitted, allowed, and not reported done by its
// supervisor. A stop leaves the supervisor no time to report, so a report
// says the job ended on its own.
func (inv *inventory) lostAtLimit(m Machine) bool {
	a, admitted := inv.admissions[m.Name]
	_, reported := inv.reports[m.Name]
	return admitted && a.Allowed && !reported
}

// gone reports whether a machine is known not to exist: it wasn't listed,
// and this pass didn't try to make it. Machine names are never used again.
func (inv *inventory) gone(name string) bool {
	_, listed := inv.machines[name]
	return inv.deleted[name] || (!listed && !inv.attempted[name])
}

// tidy applies the table to one machine. Only the age limit holds without
// the records; every other row depends on them.
func (s *Service) tidy(ctx context.Context, inv *inventory, m Machine) {
	age := s.now().Sub(m.CreatedAt)
	a, admitted := inv.admissions[m.Name]
	rep, reported := inv.reports[m.Name]
	// A failed job's machine waits until its hook has read its reason.
	if admitted && s.answering(inv, a) {
		return
	}
	// A refused machine that's running is stopped before anything else,
	// whatever the rest of the records or its boot log hold up.
	if admitted && !a.Allowed && !m.Stopped() {
		if err := s.Compute.StopInstance(ctx, m.Zone, m.Name, s.requestID("stop", m.Name)); err != nil {
			s.log().Error("stopping a refused machine", "machine", m.Name, "err", err.Error())
		}
	}
	switch {
	case age >= MaxAge:
		// A job the limit cut short is recorded, with a resume image of its
		// work disk, so that its run's rerun resumes it, as from a
		// preemption (specs/0002-machines.md, The age limit). The pass
		// stopped the machine first; one it couldn't stop waits for the
		// next. A stopped machine waits for its record, and, without the
		// records, for them, since whether it was admitted isn't known.
		if s.Preemptions != nil {
			if !inv.recordsComplete {
				return
			}
			if err := s.refresh(ctx, inv, m); err != nil {
				s.log().Warn("reading a machine's records again at its age limit; it waits for them", "machine", m.Name, "err", err.Error())
				return
			}
			a = inv.admissions[m.Name]
		}
		if s.Preemptions != nil && inv.lostAtLimit(m) {
			if !m.Stopped() {
				return
			}
			preempted := m.Spot && s.preempted(ctx, m)
			if err := s.Preemptions.Stopped(ctx, a.Resolve(m, inv.disk(m.Name)), a, preempted, true); err != nil {
				s.log().Warn("recording a machine stopped at its age limit; it waits for its record", "machine", m.Name, "err", err.Error())
				return
			}
			s.finish(ctx, inv, m, "past "+MaxAge.String()+", its job recorded to resume, saving nothing", nil)
			return
		}
		// A retired machine settles its runner first, if it can: at the
		// age limit it goes all the same, and says what may be lost.
		if ra, ok := inv.admissions[m.Name]; ok && ra.retiredClaim() && !s.settleRunner(ctx, inv, m, ra) {
			s.log().Error("deleting a retired machine at its age limit, though its runner's job couldn't be settled; a job it took may be lost",
				"machine", m.Name, "repository_id", m.RepositoryID, "runner_id", m.RunnerID)
		}
		s.finish(ctx, inv, m, "past "+MaxAge.String()+", saving nothing", nil)
	case !inv.recordsComplete:
		return
	case admitted && a.retiredClaim():
		// A machine an earlier pass retired, but didn't delete, settles
		// its runner first, even if its runner has since exited and its
		// supervisor reported: its job, never admitted, was lost.
		if inv.unrecorded(m.Name) || !s.settleRunner(ctx, inv, m, a) {
			return
		}
		s.finish(ctx, inv, m, strings.TrimPrefix(a.Reason, "retired: "), nil)
	case reported:
		s.finish(ctx, inv, m, "its supervisor reported", &rep)
	case admitted && !a.Allowed:
		if inv.unrecorded(m.Name) {
			// Its admission is the only record that its run must be
			// cancelled: the machine waits for the refusal.
			return
		}
		s.finish(ctx, inv, m, "its job was refused", nil)
	case admitted && m.Stopped() && m.Spot:
		s.stoppedSpot(ctx, inv, m, a)
	case admitted:
		repo, err := s.repository(ctx, inv, m.RepositoryID)
		if err != nil {
			return
		}
		job, err := s.GitHub.Job(ctx, repo, a.JobID)
		if err != nil && !errors.Is(err, github.ErrNotFound) {
			s.log().Warn("reading a machine's job", "machine", m.Name, "job_id", a.JobID, "err", err.Error())
			return
		}
		if errors.Is(err, github.ErrNotFound) || (job.Status == "completed" && s.now().Sub(job.CompletedAt) >= ReportWait) {
			s.finish(ctx, inv, m, "its job finished "+ReportWait.String()+" ago with no report; going on without saving", nil)
		}
	case m.Stopped():
		s.retireAndFinish(ctx, inv, m, "it stopped before its job started")
	case s.retiresForImage(ctx, inv, m):
		image, _ := s.staleImage(ctx, inv, m)
		s.retireAndFinish(ctx, inv, m, "its base image, "+image+", wasn't made with the service's programs")
	case age >= IdleLimit:
		s.retireAndFinish(ctx, inv, m, "it hasn't picked up a job after "+IdleLimit.String())
	}
}

// retireAndFinish retires a machine no job was admitted on, settles its
// runner, and deletes it, unless a check decided first: one that refused
// leaves it to be deleted, and one that allowed keeps it for its job.
func (s *Service) retireAndFinish(ctx context.Context, inv *inventory, m Machine, why string) {
	a, ok := s.retire(ctx, m, why)
	if !ok || !s.settleRunner(ctx, inv, m, a) {
		return
	}
	s.finish(ctx, inv, m, why, nil)
}

// settleRunner deals with the runner of a machine a pass retired, before
// the machine goes (specs/0002-machines.md, Retiring a machine). GitHub
// hands a runner its job before the hook asks, so the runner may have taken
// one just before the claim, whose check the claim refuses: that job is
// lost. settleRunner removes the runner's registration, so that GitHub
// gives it no further job, and GitHub's answer says whether it had one:
// removed, it had none, though the job the machine was made for is checked
// all the same; refused as running a job, it has one, assigned or running;
// gone already, it may have run one, which GitHub has ended since and
// removed the runner after, as it does a just-in-time runner's, so it's
// looked for among the runs in progress and finished too. A job it had is
// recorded, if the check would have allowed it, so that its run is rerun as
// a preempted job's is. It reports whether the machine may go: not while
// GitHub can't be read, nor while it refuses to remove the runner as running
// a job it doesn't show, nor while the record can't be written. A claim that
// isn't a retirement, such as a check's refusal, settles nothing.
func (s *Service) settleRunner(ctx context.Context, inv *inventory, m Machine, a Admission) bool {
	if !a.retiredClaim() || m.RunnerID == 0 {
		return true
	}
	log := s.log().With("machine", m.Name, "repository_id", m.RepositoryID, "runner_id", m.RunnerID)
	// What an earlier pass learnt of the runner: that it removed it, with
	// no job then, or that GitHub refused, as it had one.
	was, err := s.runnerState(ctx, m)
	if err != nil {
		log.Warn("reading what's known of a retired machine's runner; the machine waits for it", "err", err.Error())
		return false
	}
	err = s.GitHub.DeleteRunner(ctx, m.RunnerID)
	busy, gone := errors.Is(err, github.ErrRefused), errors.Is(err, github.ErrNotFound)
	if err != nil && !busy && !gone {
		log.Warn("removing a retired machine's runner; the machine waits for it", "err", err.Error())
		return false
	}
	// removed says this service removed the runner, which had no job then;
	// hadJob, that GitHub refused to remove it, now or before, as it had
	// one, which it may have ended since.
	removed := err == nil || gone && was.Removed
	hadJob := busy || gone && was.Busy
	if (err == nil || busy) && !s.noteRunner(ctx, inv, m, RunnerState{Removed: err == nil, Busy: busy}) {
		log.Warn("recording what's known of a retired machine's runner; the machine waits for it")
		return false
	}
	repo, err := s.GitHub.RepositoryByID(ctx, m.RepositoryID)
	if errors.Is(err, github.ErrNotFound) {
		// Nothing of a repository GitHub doesn't show is rerun.
		log.Warn("a retired machine's repository isn't shown; a job its runner took isn't looked for")
		return true
	}
	if err != nil {
		log.Warn("reading a retired machine's repository; the machine waits for it", "err", err.Error())
		return false
	}
	var job github.Job
	var run github.WorkflowRun
	var found bool
	if removed {
		// It had no job when it was removed; but the job its machine was
		// made for, read by its ID, is checked all the same.
		job, run, found, err = s.madeFor(ctx, repo, m)
	} else {
		job, run, found, err = s.runnerJob(ctx, repo, m, busy)
	}
	switch {
	case err != nil:
		log.Warn("looking for a job on a retired machine's runner; the machine waits for it", "err", err.Error())
		return false
	case !found && !hadJob:
		// Removed with no job, or gone with none GitHub shows.
		return true
	case !found:
		log.Warn("a retired machine's runner had a job, which GitHub doesn't show yet; the machine waits for it", "busy", busy)
		return false
	}
	log = log.With("run_id", run.ID, "job_id", job.ID)
	if job.RunAttempt != run.Attempt {
		// Its run has been rerun since: the job is past rerunning.
		log.Info("a job lost on a retired machine isn't rerun: its run has been rerun since")
		return true
	}
	lost, decided, bd, bound, err := s.judge(ctx, repo, m, job, run)
	if err != nil {
		log.Warn("judging a job lost on a retired machine; the machine waits for it", "err", err.Error())
		return false
	}
	switch {
	case decided:
	case bound:
		lost = s.conclude(lost, m, run, bd, true)
	case run.Event == "pull_request_target":
		// An approved contribution's run would have been bound at its
		// check, as its rerun's job's check binds it.
		approved, err := s.approved(ctx, repo, run.ID)
		if err != nil {
			log.Warn("reading a contribution's approval for a job lost on a retired machine; the machine waits for it", "err", err.Error())
			return false
		}
		if approved {
			lost.Allowed = true
		} else {
			lost = s.conclude(lost, m, run, bd, false)
		}
	default:
		// An unbound run's job would have bound it at its check, from
		// what GitHub told the runner, which a rerun's job reports
		// instead.
		lost.Allowed = true
	}
	if !lost.Allowed {
		log.Info("a job lost on a retired machine isn't rerun: its check would have refused it", "reason", lost.Reason)
		return true
	}
	if s.Preemptions == nil {
		log.Warn("a job lost on a retired machine can't be recorded to be rerun")
		return true
	}
	if err := s.Preemptions.Lost(ctx, m, lost); err != nil {
		log.Warn("recording a job lost on a retired machine; the machine waits for it", "err", err.Error())
		return false
	}
	log.Info("a job lost on a retired machine is recorded, so that its run is rerun")
	return true
}

// RunnerState is what a pass learnt of a retired machine's runner, in
// machines/<machine>/runner.json, which goes with the machine: Removed,
// that it removed it, with no job then; Busy, that GitHub refused to, as it
// had one.
type RunnerState struct {
	Machine  string    `json:"machine"`
	RunnerID int64     `json:"runner_id"`
	Removed  bool      `json:"removed,omitempty"`
	Busy     bool      `json:"busy,omitempty"`
	At       time.Time `json:"at"`
}

func runnerKey(machine string) string { return "machines/" + machine + "/runner.json" }

func (s *Service) runnerState(ctx context.Context, m Machine) (RunnerState, error) {
	st, _, _, err := getJSON[RunnerState](ctx, s.Objects, runnerKey(m.Name))
	return st, err
}

// noteRunner adds what a pass learnt of a retired machine's runner to its
// record, as one of the machine's records.
func (s *Service) noteRunner(ctx context.Context, inv *inventory, m Machine, add RunnerState) bool {
	key := runnerKey(m.Name)
	for range 3 {
		st, gen, found, err := getJSON[RunnerState](ctx, s.Objects, key)
		if err != nil {
			return false
		}
		want := RunnerState{Machine: m.Name, RunnerID: m.RunnerID, Removed: st.Removed || add.Removed, Busy: st.Busy || add.Busy, At: s.now()}
		if found && want.Removed == st.Removed && want.Busy == st.Busy {
			return true
		}
		if found {
			gen, err = replaceJSON(ctx, s.Objects, key, want, gen)
		} else {
			gen, err = createJSON(ctx, s.Objects, key, want)
		}
		if errors.Is(err, store.ErrPrecondition) {
			continue
		}
		if err != nil {
			return false
		}
		inv.machineKeys[m.Name] = append(slices.DeleteFunc(inv.machineKeys[m.Name], func(o store.Object) bool { return o.Name == key }),
			store.Object{Name: key, Generation: gen})
		return true
	}
	return false
}

// runnerJob is the job GitHub shows on a machine's runner, by the runner's
// ID and name, whatever the job's status, read afresh, never from what an
// earlier lookup read before the runner's removal: the job the machine was
// made for, which its runner takes most often, by its ID; for a runner
// GitHub refused to remove as running a job, one of the repository's runs
// in progress, looked for again for a while, as the check does, until
// GitHub shows it; and for one already gone, one of the runs in progress, or
// of the finished runs created since the longest a run lasts before the
// machine was made and updated since it was made, as a run whose job it
// took was when it finished. Only a run's latest attempt is looked at: a job lost in an
// earlier one is past rerunning, since its run has been rerun already.
func (s *Service) runnerJob(ctx context.Context, repo github.Repository, m Machine, busy bool) (github.Job, github.WorkflowRun, bool, error) {
	none := func(err error) (github.Job, github.WorkflowRun, bool, error) {
		return github.Job{}, github.WorkflowRun{}, false, err
	}
	if job, run, ok, err := s.madeFor(ctx, repo, m); err != nil || ok {
		return job, run, ok, err
	}
	if busy {
		job, run, err := s.findJob(ctx, repo, m, 0)
		switch {
		case err == nil:
			return job, run, true, nil
		case !errors.Is(err, errNoAssignment):
			return none(err)
		}
		return none(nil)
	}
	// Runs of every status, since a run whose job the runner took may be
	// in progress, finished, or waiting on another of its jobs: those in
	// progress, and the rest updated since the machine was made, as a run
	// was when the job it lost ended. GitHub's clock and the service's may
	// differ a little.
	runs, err := s.GitHub.RunsCreated(ctx, repo, "", m.CreatedAt.Add(-RunLimit-clockSkew), s.now().Add(clockSkew))
	if err != nil {
		return none(err)
	}
	since := m.CreatedAt.Add(-clockSkew)
	runs = slices.DeleteFunc(runs, func(r github.WorkflowRun) bool { return r.Status != "in_progress" && r.UpdatedAt.Before(since) })
	return s.jobAmong(ctx, repo, m, runs)
}

// madeFor is the job a machine was made for, read by its ID, if GitHub
// shows it on the machine's runner.
func (s *Service) madeFor(ctx context.Context, repo github.Repository, m Machine) (github.Job, github.WorkflowRun, bool, error) {
	if m.MadeFor <= 0 {
		return github.Job{}, github.WorkflowRun{}, false, nil
	}
	j, err := s.GitHub.Job(ctx, repo, m.MadeFor)
	if errors.Is(err, github.ErrNotFound) || err == nil && !onRunner(j, m) {
		return github.Job{}, github.WorkflowRun{}, false, nil
	}
	if err != nil {
		return github.Job{}, github.WorkflowRun{}, false, err
	}
	run, err := s.GitHub.WorkflowRun(ctx, repo, j.RunID)
	if err != nil {
		return github.Job{}, github.WorkflowRun{}, false, err
	}
	return j, run, true, nil
}

// runsInProgress are a repository's runs in progress that a machine's
// runner may have taken a job of: created since the longest GitHub lets a
// run last before the machine was made. GitHub lists at most 1,000 runs for
// one filter, so the span is split as RunsCreated does.
func (s *Service) runsInProgress(ctx context.Context, repo github.Repository, m Machine) ([]github.WorkflowRun, error) {
	return s.GitHub.RunsCreated(ctx, repo, "in_progress", m.CreatedAt.Add(-RunLimit-clockSkew), s.now().Add(clockSkew))
}

// clockSkew is as far as GitHub's clock is taken to be from the service's.
const clockSkew = 5 * time.Minute

// onRunner reports whether GitHub shows a job on a machine's runner.
func onRunner(j github.Job, m Machine) bool {
	return j.RunnerID == m.RunnerID && strings.EqualFold(j.RunnerName, m.Name)
}

// jobAmong is the job on a machine's runner among runs' latest attempts'
// jobs.
func (s *Service) jobAmong(ctx context.Context, repo github.Repository, m Machine, runs []github.WorkflowRun) (github.Job, github.WorkflowRun, bool, error) {
	for _, run := range runs {
		if run.RepositoryID != repo.ID {
			continue
		}
		jobs, err := s.GitHub.RunJobs(ctx, repo, run.ID)
		if errors.Is(err, github.ErrNotFound) {
			continue
		}
		if err != nil {
			return github.Job{}, github.WorkflowRun{}, false, err
		}
		for _, j := range jobs {
			if onRunner(j, m) && j.RunID == run.ID {
				return j, run, true, nil
			}
		}
	}
	return github.Job{}, github.WorkflowRun{}, false, nil
}

// retiresForImage reports whether an unadmitted machine is retired for its
// base image's programs (staleImage).
func (s *Service) retiresForImage(ctx context.Context, inv *inventory, m Machine) bool {
	_, stale := s.staleImage(ctx, inv, m)
	return stale
}

// requestID is a request ID for a change a pass makes, new each time it's
// tried: Compute Engine answers an ID it has seen with its first outcome,
// and a deletion or stop is safe to ask for again. Within one call, a
// retry with no answer keeps the ID.
func (s *Service) requestID(parts ...string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return compute.RequestID(append(parts, hex.EncodeToString(b[:]))...)
}

// retire claims an unadmitted machine for cleanup through its admission
// record, created only if absent, as the job-started check's decision is: a
// check that decides first keeps the machine, and one that comes later finds
// it refused. It returns the admission, the claim or the check's, and
// reports whether the machine may be deleted.
func (s *Service) retire(ctx context.Context, m Machine, why string) (Admission, bool) {
	a, err := s.claim(ctx, m, why)
	if err != nil {
		s.log().Warn("retiring a machine", "machine", m.Name, "err", err.Error())
		return Admission{}, false
	}
	return a, !a.Allowed
}

// claim writes a machine's admission as retired, only if it has none, and
// returns the admission it has: that one, or the check's, whichever was
// first.
func (s *Service) claim(ctx context.Context, m Machine, why string) (Admission, error) {
	a, _, err := createOnce(ctx, s.Objects, admissionKey(m.Name), Admission{Machine: m.Name, Zone: m.Zone,
		RepositoryID: m.RepositoryID, Selector: m.Selector.String(), Reason: "retired: " + why, Retired: true, At: s.now()})
	return a, err
}

// stoppedSpot deals with a Spot machine that stopped with its job admitted:
// preempted, by Compute Engine's record, or perhaps.
func (s *Service) stoppedSpot(ctx context.Context, inv *inventory, m Machine, a Admission) {
	preempted := s.preempted(ctx, m)
	if s.Preemptions != nil {
		if err := s.Preemptions.Stopped(ctx, a.Resolve(m, inv.disk(m.Name)), a, preempted, false); err != nil {
			s.log().Warn("recording a stopped Spot machine", "machine", m.Name, "err", err.Error())
			return
		}
	}
	why := "its Spot machine might have been preempted"
	if preempted {
		why = "its Spot machine was preempted"
	}
	s.finish(ctx, inv, m, why, nil)
}

// preempted reports whether Compute Engine's zone operations show it
// preempted a machine.
func (s *Service) preempted(ctx context.Context, m Machine) bool {
	ids, err := s.Compute.Preempted(ctx, m.Zone)
	return err == nil && slices.Contains(ids, m.ID)
}

// finish ends a machine's life: once its supervisor has reported, it saves
// the work disk if the job asked and the job and scope allow; then it saves
// the boot log, and deletes the machine and its disks, then their records.
// No report means no save.
func (s *Service) finish(ctx context.Context, inv *inventory, m Machine, why string, rep *Report) {
	log := s.log().With("machine", m.Name, "repository_id", m.RepositoryID)
	if _, asked := inv.saves[m.Name]; rep != nil && asked {
		if save, err := s.saves(ctx, inv, m); err != nil {
			log.Warn("deciding whether to save a work disk", "err", err.Error())
			return
		} else if save {
			if s.Saves == nil {
				log.Warn("the job asked to save its work disk, but this service saves no disks; deleting it")
			} else {
				if !m.Stopped() {
					// The disk is saved from the stopped machine. The
					// report says the job is done, and the supervisor
					// powers the machine off next; the service stops it
					// itself rather than wait for the next pass.
					if err := s.Compute.StopInstance(ctx, m.Zone, m.Name, s.requestID("stop", m.Name)); err != nil && !errors.Is(err, compute.ErrNotFound) {
						log.Warn("stopping a reported machine to save its disk", "err", err.Error())
						return
					}
				}
				a := inv.admissions[m.Name]
				if err := s.Saves.Save(ctx, a.Resolve(m, inv.disk(m.Name)), a); err != nil {
					log.Warn("saving a work disk", "err", err.Error())
					return
				}
			}
		}
	}
	// The boot log is saved before the machine, its only copy, goes; a
	// machine whose log can't be saved waits for a later pass, unless it's
	// past its age limit.
	if err := s.saveBootLog(ctx, m); err != nil {
		if s.now().Sub(m.CreatedAt) < MaxAge {
			log.Warn("saving a boot log; the machine waits for it", "err", err.Error())
			return
		}
		log.Warn("saving a boot log; the machine is past its age limit and goes without it", "err", err.Error())
	}
	// A tentative first-machine record naming this machine is confirmed
	// while the machine is there to show it was made.
	key := jobKey(m.RepositoryID, m.MadeFor)
	if r, ok := inv.jobRecords[key]; ok && r.v.Tentative && r.v.Machine == m.Name {
		r.v.Tentative = false
		gen, err := replaceJSON(ctx, s.Objects, key, r.v, r.gen)
		switch {
		case err == nil:
			inv.jobRecords[key] = record[JobRecord]{r.v, gen}
		case s.now().Sub(m.CreatedAt) < MaxAge:
			log.Warn("confirming a job's first machine; the machine waits for it", "err", err.Error())
			return
		default:
			// The age limit comes first. If the bucket takes neither this
			// nor the boot log, nothing is left to show the machine was
			// made, and its job may get machines for another hour: the cost
			// of a bucket that's down, bounded by that hour.
			log.Warn("confirming a job's first machine; the machine is past its age limit and goes without it", "err", err.Error())
		}
	}
	if err := s.Compute.DeleteInstance(ctx, m.Zone, m.Name, s.requestID("delete", m.Name)); err != nil && !errors.Is(err, compute.ErrNotFound) {
		log.Error("deleting a machine", "err", err.Error())
		return
	}
	inv.deleted[m.Name] = true
	// Its work disk goes with it; one that's somehow left is deleted here.
	disk := WorkDisk(m.Name)
	if err := s.Compute.DeleteDisk(ctx, m.Zone, disk, s.requestID("delete", disk)); err != nil &&
		!errors.Is(err, compute.ErrNotFound) {
		log.Warn("deleting a work disk", "disk", disk, "err", err.Error())
	}
	s.dropRecords(ctx, inv, m.Name)
	log.Info("machine deleted", "reason", why)
}

// saveBootLog saves a machine's serial console output to the bucket, once:
// from Cloud Logging, where Compute Engine sends it, once the machine has
// gone, by a record a later pass follows; or, without Cloud Logging, from
// Compute Engine before it goes.
func (s *Service) saveBootLog(ctx context.Context, m Machine) error {
	if s.BootLogs != nil {
		_, _, err := createOnce(ctx, s.Objects, pendingLogKey(m), PendingLog{Machine: m.Name, InstanceID: m.ID, CreatedAt: m.CreatedAt,
			DeletedAt: s.now()})
		return err
	}
	out, err := s.Compute.SerialOutput(ctx, m.Zone, m.Name, bootLogLimit)
	if errors.Is(err, compute.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := s.Objects.Create(ctx, bootLogKey(m), []byte(out)); err != nil && !errors.Is(err, store.ErrPrecondition) {
		return err
	}
	return nil
}

// saves reports whether a finished job's work disk is to be saved, once it
// asked: the job succeeded, by GitHub's conclusion, its scope saves, and it
// has a work disk, of a family, which its disk action asked for
// (specs/0003-disks.md).
func (s *Service) saves(ctx context.Context, inv *inventory, m Machine) (bool, error) {
	a, ok := inv.admissions[m.Name]
	if !ok || !a.Allowed || a.Unbound || !a.Scope.Saves() {
		return false, nil
	}
	if sr := inv.saves[m.Name]; sr.JobID != a.JobID || !a.Resolve(m, inv.disk(m.Name)).Selector.HasDisk() {
		return false, nil
	}
	repo, err := s.repository(ctx, inv, m.RepositoryID)
	if err != nil {
		return false, err
	}
	if repo.OwnerID != s.Config.Organization.ID {
		// Transferred out of the organization: no longer a consumer.
		return false, nil
	}
	job, err := s.GitHub.Job(ctx, repo, a.JobID)
	if err != nil {
		return false, err
	}
	return job.Status == "completed" && job.Conclusion == "success", nil
}

// dropRecords removes a deleted machine's records, and its work disk's,
// except a refused admission whose run's refusal isn't yet recorded.
func (s *Service) dropRecords(ctx context.Context, inv *inventory, machine string) {
	keep := inv.unrecorded(machine)
	for _, o := range inv.machineKeys[machine] {
		if keep && o.Name == admissionKey(machine) {
			continue
		}
		if err := s.Objects.Delete(ctx, o.Name, o.Generation); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log().Warn("removing a machine's record", "record", o.Name, "err", err.Error())
		}
	}
	for name, r := range inv.diskRecords {
		if r.v.Machine == machine {
			if err := s.Objects.Delete(ctx, diskKey(name), r.gen); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.log().Warn("removing a disk's record", "disk", name, "err", err.Error())
			}
			delete(inv.diskRecords, name)
		}
	}
	delete(inv.machineKeys, machine)
}

// tidyDisks deletes disks attached to nothing that aren't recorded as being
// made or waiting to be attached to a machine that's there.
func (s *Service) tidyDisks(ctx context.Context, inv *inventory) {
	for _, d := range inv.disks {
		if len(d.Users) > 0 {
			continue
		}
		r, recorded := inv.diskRecords[d.Name]
		if recorded && (r.v.State == DiskMaking || r.v.State == DiskMade) && !inv.gone(r.v.Machine) {
			continue
		}
		err := s.Compute.DeleteDisk(ctx, d.Zone, d.Name, s.requestID("delete", d.Name))
		if errors.Is(err, compute.ErrInUse) {
			// Attached since it was listed.
			continue
		}
		if err != nil && !errors.Is(err, compute.ErrNotFound) {
			s.log().Warn("deleting an orphaned disk", "disk", d.Name, "err", err.Error())
			continue
		}
		if recorded {
			if err := s.Objects.Delete(ctx, diskKey(d.Name), r.gen); err == nil {
				delete(inv.diskRecords, d.Name)
			}
		}
		s.log().Info("orphaned disk deleted", "disk", d.Name, "zone", d.Zone)
	}
}

// tidyRunners removes offline runner registrations in the service's groups
// that are named after its machines and whose machine is gone.
func (s *Service) tidyRunners(ctx context.Context, inv *inventory) {
	if inv.groups == nil {
		gs, err := s.GitHub.RunnerGroups(ctx)
		if err != nil {
			s.log().Warn("listing runner groups", "err", err.Error())
			return
		}
		inv.groups = map[string]github.RunnerGroup{}
		for _, g := range gs {
			inv.groups[g.Name] = g
		}
	}
	for name, g := range inv.groups {
		if !strings.HasPrefix(name, GroupPrefix) {
			continue
		}
		rs, err := s.GitHub.GroupRunners(ctx, g.ID)
		if err != nil {
			s.log().Warn("listing a group's runners", "group", name, "err", err.Error())
			continue
		}
		for _, r := range rs {
			if m, ok := inv.machines[r.Name]; ok && machineName.MatchString(r.Name) && !onlyLabel(r.Labels, m.Selector.String()) {
				// GitHub's record shows a label besides the selector: the
				// runner and its machine go.
				s.log().Error("a runner has labels besides its selector; removing it and its machine", "runner", r.Name, "labels", r.Labels)
				if err := s.GitHub.DeleteRunner(ctx, r.ID); err != nil && !errors.Is(err, github.ErrNotFound) {
					s.log().Warn("removing a runner with other labels", "runner", r.Name, "err", err.Error())
				}
				if err := s.Compute.StopInstance(ctx, m.Zone, m.Name, s.requestID("stop", m.Name)); err != nil && !errors.Is(err, compute.ErrNotFound) {
					s.log().Warn("stopping a machine whose runner has other labels", "machine", m.Name, "err", err.Error())
				}
				continue
			}
			if !machineName.MatchString(r.Name) || r.Status != "offline" || r.Busy || !inv.gone(r.Name) {
				continue
			}
			if err := s.GitHub.DeleteRunner(ctx, r.ID); err != nil && !errors.Is(err, github.ErrNotFound) {
				s.log().Warn("removing an offline runner", "runner", r.Name, "err", err.Error())
				continue
			}
			s.log().Info("offline runner removed", "runner", r.Name, "group", name)
		}
	}
}

// tidyRecords removes records of machines that are gone, of jobs no longer
// queued, and of refused runs a day after they finished.
func (s *Service) tidyRecords(ctx context.Context, inv *inventory) {
	for machine := range inv.machineKeys {
		if inv.gone(machine) {
			s.dropRecords(ctx, inv, machine)
		}
	}
	for name, r := range inv.diskRecords {
		if inv.gone(r.v.Machine) && !slices.ContainsFunc(inv.disks, func(d compute.Disk) bool { return d.Name == name }) {
			if err := s.Objects.Delete(ctx, diskKey(name), r.gen); err == nil {
				delete(inv.diskRecords, name)
			}
		}
	}
	queued := map[string]bool{}
	complete := inv.reposComplete
	for _, q := range inv.queues {
		complete = complete && q.complete
		for _, j := range q.jobs {
			queued[jobKey(q.repo.ID, j.ID)] = true
		}
	}
	if complete {
		for key, r := range inv.jobRecords {
			if !queued[key] {
				_ = s.Objects.Delete(ctx, key, r.gen)
			}
		}
	}
	for key, r := range inv.refusals {
		if !r.v.FinishedAt.IsZero() && s.now().Sub(r.v.FinishedAt) >= recordsKept {
			_ = s.Objects.Delete(ctx, key, r.gen)
		}
	}
}

// followBootLogs reads deleted machines' boot logs from Cloud Logging into
// the bucket: once the supervisor's last line has arrived, or, waiting up to
// BootLogWait for it, as they are, or as missing.
func (s *Service) followBootLogs(ctx context.Context) {
	if s.BootLogs == nil {
		return
	}
	objs, err := s.Objects.List(ctx, "bootlogs/")
	if err != nil {
		s.log().Warn("listing boot logs to read", "err", err.Error())
		return
	}
	for _, o := range objs {
		if ctx.Err() != nil {
			return
		}
		p, gen, ok, err := getJSON[PendingLog](ctx, s.Objects, o.Name)
		if err != nil || !ok {
			continue
		}
		text, err := s.BootLogs.Serial(ctx, p.InstanceID, bootLogLimit)
		if err != nil {
			s.log().Warn("reading a boot log from Cloud Logging", "machine", p.Machine, "err", err.Error())
			continue
		}
		waited := s.now().Sub(p.DeletedAt) >= BootLogWait
		if !strings.Contains(text, LastLine) && !waited {
			continue
		}
		if text == "" {
			text = "lanescape-runners: no boot log arrived from Cloud Logging within " + BootLogWait.String() + "\n"
			s.log().Warn("a boot log is missing", "machine", p.Machine)
		}
		key := bootLogKey(Machine{Instance: compute.Instance{Name: p.Machine}, CreatedAt: p.CreatedAt})
		if _, err := s.Objects.Create(ctx, key, []byte(text)); err != nil && !errors.Is(err, store.ErrPrecondition) {
			s.log().Warn("saving a boot log", "machine", p.Machine, "err", err.Error())
			continue
		}
		_ = s.Objects.Delete(ctx, o.Name, gen)
	}
}
