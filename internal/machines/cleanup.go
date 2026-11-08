package machines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"

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
	// A refused machine that's running is stopped before anything else,
	// whatever the rest of the records or its boot log hold up.
	if admitted && !a.Allowed && !m.Stopped() {
		if err := s.Compute.StopInstance(ctx, m.Zone, m.Name, s.requestID("stop", m.Name)); err != nil {
			s.log().Error("stopping a refused machine", "machine", m.Name, "err", err.Error())
		}
	}
	switch {
	case age >= MaxAge:
		s.finish(ctx, inv, m, "past "+MaxAge.String()+", deleted even mid-job, saving nothing", nil)
	case !inv.recordsComplete:
		return
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
			s.finish(ctx, inv, m, "its job finished "+ReportWait.String()+" ago with no report; going on as if it asked for no save", &Report{Machine: m.Name})
		}
	case m.Stopped():
		if s.retire(ctx, m, "it stopped before its job started") {
			s.finish(ctx, inv, m, "it stopped before its job started", nil)
		}
	case age >= IdleLimit:
		if s.retire(ctx, m, "it hasn't picked up a job after "+IdleLimit.String()) {
			s.finish(ctx, inv, m, "it hasn't picked up a job after "+IdleLimit.String(), nil)
		}
	}
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
// it refused. It reports whether the machine may be deleted.
func (s *Service) retire(ctx context.Context, m Machine, why string) bool {
	a, _, err := createOnce(ctx, s.Objects, admissionKey(m.Name), Admission{Machine: m.Name, Zone: m.Zone,
		RepositoryID: m.RepositoryID, Selector: m.Selector.String(), Reason: "retired: " + why, At: s.now()})
	if err != nil {
		s.log().Warn("retiring a machine", "machine", m.Name, "err", err.Error())
		return false
	}
	return !a.Allowed
}

// stoppedSpot deals with a Spot machine that stopped with its job admitted:
// preempted, by Compute Engine's record, or perhaps.
func (s *Service) stoppedSpot(ctx context.Context, inv *inventory, m Machine, a Admission) {
	preempted := false
	if ids, err := s.Compute.Preempted(ctx, m.Zone); err == nil {
		preempted = slices.Contains(ids, m.ID)
	}
	if s.Preemptions != nil {
		if err := s.Preemptions.Stopped(ctx, m, a, preempted); err != nil {
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

// finish ends a machine's life: it saves the work disk if the report asks
// and the job and scope allow, saves the boot log, and deletes the machine
// and its disks, then their records.
func (s *Service) finish(ctx context.Context, inv *inventory, m Machine, why string, rep *Report) {
	log := s.log().With("machine", m.Name, "repository_id", m.RepositoryID)
	if rep != nil && rep.Save {
		if save, err := s.saves(ctx, inv, m); err != nil {
			log.Warn("deciding whether to save a work disk", "err", err.Error())
			return
		} else if save {
			if s.Saves == nil {
				log.Warn("the job asked to save its work disk, but saving isn't built yet; deleting it")
			} else {
				if !m.Stopped() {
					// The disk is saved from the stopped machine.
					return
				}
				a := inv.admissions[m.Name]
				if err := s.Saves.Save(ctx, m, a); err != nil {
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

// saveBootLog saves a machine's serial console output to the bucket, once.
func (s *Service) saveBootLog(ctx context.Context, m Machine) error {
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

// saves reports whether a finished job's work disk is to be saved: the job
// succeeded, by GitHub's conclusion, its scope saves, and its selector
// names a family and key (specs/0003-disks.md).
func (s *Service) saves(ctx context.Context, inv *inventory, m Machine) (bool, error) {
	a, ok := inv.admissions[m.Name]
	if !ok || !a.Allowed || !m.Selector.Scope.Saves() || !m.Selector.HasDisk() {
		return false, nil
	}
	repo, err := s.repository(ctx, inv, m.RepositoryID)
	if err != nil {
		return false, err
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
