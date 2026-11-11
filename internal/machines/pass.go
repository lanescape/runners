package machines

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lanescape/runners/internal/bases"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/selector"
	"github.com/lanescape/runners/internal/store"
)

// queue is what a pass read of one consumer's queue.
type queue struct {
	repo github.Repository
	runs map[int64]github.WorkflowRun
	// jobs are the queued jobs, oldest first.
	jobs []github.Job
	// runJobs are each listed run's jobs.
	runJobs map[int64][]github.Job
	// complete says every run's jobs were read.
	complete bool
}

type record[T any] struct {
	v   T
	gen int64
}

// inventory is what a pass works from: what GitHub and Compute Engine
// report when it runs, and the bucket's records, read after the machines
// and disks they describe, since every record is written before what it
// describes is made.
type inventory struct {
	machines         map[string]Machine
	machinesComplete bool
	disks            []compute.Disk
	disksComplete    bool
	repos            map[int64]github.Repository
	reposComplete    bool
	queues           []*queue

	recordsComplete bool
	admissions      map[string]Admission
	answered        map[string]time.Time
	reports         map[string]Report
	saves           map[string]SaveRequest
	diskRecords     map[string]record[DiskRecord]
	refusals        map[string]record[Refusal]
	jobRecords      map[string]record[JobRecord]
	machineKeys     map[string][]store.Object

	// What this pass learns and does as it goes.
	groups    map[string]github.RunnerGroup
	checked   map[string]github.RunnerGroup
	images    map[string]string
	attempted map[string]bool
	deleted   map[string]bool
}

// disk is a machine's work disk's record, if it has one.
func (inv *inventory) disk(machine string) *DiskRecord {
	if r, ok := inv.diskRecords[WorkDisk(machine)]; ok {
		return &r.v
	}
	return nil
}

// Pass is the machines' part of a reconciler pass (specs/0001-architecture.md,
// Passes): it stops every machine past its age limit before anything else,
// gives queued jobs machines, follows up refused runs, and deletes what's
// finished. A failure on one machine, job or disk doesn't stop the rest.
func (s *Service) Pass(p *pass.Pass) error {
	inv := s.inventory(p)
	if p.Lost() {
		return context.Cause(p.Context())
	}
	ctx := p.Context()
	s.stopPastLimit(ctx, inv)
	if s.Bases != nil {
		s.Bases.Pass(p)
		if p.Lost() {
			return context.Cause(p.Context())
		}
	}
	s.followRefusals(ctx, inv)
	s.give(ctx, inv)
	s.cleanup(ctx, inv)
	s.followBootLogs(ctx)
	if p.Lost() {
		return context.Cause(p.Context())
	}
	return nil
}

func (s *Service) inventory(p *pass.Pass) *inventory {
	ctx := p.Context()
	inv := &inventory{machines: map[string]Machine{}, repos: map[int64]github.Repository{}, groups: nil,
		checked: map[string]github.RunnerGroup{}, images: map[string]string{}, attempted: map[string]bool{}, deleted: map[string]bool{}}

	// Machines and disks first, then records.
	inv.machinesComplete, inv.disksComplete = true, true
	for _, z := range s.Config.Shapes.Zones {
		ins, err := s.Compute.Instances(ctx, z)
		if err != nil {
			s.log().Error("listing machines", "zone", z, "err", err.Error())
			inv.machinesComplete = false
		}
		for _, in := range ins {
			if m, ok := ours(in); ok {
				inv.machines[m.Name] = m
			}
		}
		ds, err := s.Compute.Disks(ctx, z)
		if err != nil {
			s.log().Error("listing disks", "zone", z, "err", err.Error())
			inv.disksComplete = false
		}
		for _, d := range ds {
			if diskName.MatchString(d.Name) && d.Labels[LabelRepository] != "" {
				inv.disks = append(inv.disks, d)
			}
		}
	}
	s.readRecords(ctx, inv)

	qctx, cancel := p.Within(QueueDeadline)
	defer cancel()
	repos, err := s.GitHub.Repositories(qctx)
	if err != nil {
		s.log().Error("listing consumers", "err", err.Error())
	}
	inv.reposComplete = err == nil
	for _, r := range repos {
		if r.OwnerID != s.Config.Organization.ID {
			continue
		}
		inv.repos[r.ID] = r
		inv.queues = append(inv.queues, s.readQueue(qctx, r))
	}
	slices.SortFunc(inv.queues, func(a, b *queue) int { return cmp.Compare(a.repo.ID, b.repo.ID) })
	return inv
}

func (s *Service) readRecords(ctx context.Context, inv *inventory) {
	inv.recordsComplete = true
	inv.admissions, inv.reports, inv.answered = map[string]Admission{}, map[string]Report{}, map[string]time.Time{}
	inv.saves = map[string]SaveRequest{}
	inv.diskRecords, inv.refusals, inv.jobRecords = map[string]record[DiskRecord]{}, map[string]record[Refusal]{}, map[string]record[JobRecord]{}
	inv.machineKeys = map[string][]store.Object{}
	list := func(prefix string, each func(o store.Object) error) {
		objs, err := s.Objects.List(ctx, prefix)
		if err != nil {
			s.log().Error("listing records", "prefix", prefix, "err", err.Error())
			inv.recordsComplete = false
			return
		}
		for _, o := range objs {
			if err := each(o); err != nil {
				s.log().Error("reading a record", "record", o.Name, "err", err.Error())
				inv.recordsComplete = false
			}
		}
	}
	read := func(name string, into func(gen int64, data func(any) error)) error {
		data, gen, err := s.Objects.Get(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var derr error
		into(gen, func(v any) error { derr = jsonUnmarshal(data, v); return derr })
		return derr
	}
	list("machines/", func(o store.Object) error {
		parts := strings.Split(o.Name, "/")
		if len(parts) != 3 {
			return nil
		}
		inv.machineKeys[parts[1]] = append(inv.machineKeys[parts[1]], o)
		switch parts[2] {
		case "admission.json":
			return read(o.Name, func(_ int64, dec func(any) error) {
				var a Admission
				if dec(&a) == nil {
					inv.admissions[parts[1]] = a
				}
			})
		case "answered.json":
			return read(o.Name, func(_ int64, dec func(any) error) {
				var an Answered
				if dec(&an) == nil {
					inv.answered[parts[1]] = an.At
				}
			})
		case "report.json":
			return read(o.Name, func(_ int64, dec func(any) error) {
				var r Report
				if dec(&r) == nil {
					inv.reports[parts[1]] = r
				}
			})
		case "save-request.json":
			return read(o.Name, func(_ int64, dec func(any) error) {
				var r SaveRequest
				if dec(&r) == nil {
					inv.saves[parts[1]] = r
				}
			})
		}
		return nil
	})
	list("disks/", func(o store.Object) error {
		return read(o.Name, func(gen int64, dec func(any) error) {
			var d DiskRecord
			if dec(&d) == nil {
				inv.diskRecords[d.Disk] = record[DiskRecord]{d, gen}
			}
		})
	})
	list("refusals/", func(o store.Object) error {
		return read(o.Name, func(gen int64, dec func(any) error) {
			var r Refusal
			if dec(&r) == nil {
				inv.refusals[o.Name] = record[Refusal]{r, gen}
			}
		})
	})
	list("jobs/", func(o store.Object) error {
		return read(o.Name, func(gen int64, dec func(any) error) {
			var j JobRecord
			if dec(&j) == nil {
				inv.jobRecords[o.Name] = record[JobRecord]{j, gen}
			}
		})
	})
}

// readQueue lists a consumer's queued jobs: those of its queued and
// in-progress runs.
func (s *Service) readQueue(ctx context.Context, repo github.Repository) *queue {
	q := &queue{repo: repo, runs: map[int64]github.WorkflowRun{}, runJobs: map[int64][]github.Job{}, complete: true}
	for _, status := range []string{"queued", "in_progress"} {
		runs, err := s.GitHub.Runs(ctx, repo, status)
		if err != nil {
			s.log().Error("listing runs", "repository_id", repo.ID, "status", status, "err", err.Error())
			q.complete = false
			continue
		}
		for _, r := range runs {
			q.runs[r.ID] = r
		}
	}
	ids := make([]int64, 0, len(q.runs))
	for id := range q.runs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		jobs, err := s.GitHub.RunJobs(ctx, repo, id)
		if err != nil {
			s.log().Error("listing jobs", "repository_id", repo.ID, "run_id", id, "err", err.Error())
			q.complete = false
			continue
		}
		q.runJobs[id] = jobs
		for _, j := range jobs {
			if j.Status == "queued" {
				q.jobs = append(q.jobs, j)
			}
		}
	}
	slices.SortFunc(q.jobs, func(a, b github.Job) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return q
}

func (inv *inventory) refused(m string) bool {
	a, ok := inv.admissions[m]
	return ok && !a.Allowed
}

func (inv *inventory) reported(m string) bool {
	_, ok := inv.reports[m]
	return ok
}

// live reports whether a machine is being made, idle or running a job: it
// hasn't stopped, been refused or reported.
func (inv *inventory) live(m Machine) bool {
	return !m.Stopped() && !inv.refused(m.Name) && !inv.reported(m.Name)
}

// activeRefusals are a consumer's refused runs that GitHub hasn't yet shown
// finished.
func (inv *inventory) activeRefusals(repoID int64) []Refusal {
	var out []Refusal
	for _, r := range inv.refusals {
		if r.v.RepositoryID == repoID && r.v.FinishedAt.IsZero() {
			out = append(out, r.v)
		}
	}
	return out
}

// refusalRecorded reports whether a refused admission that cancels its run
// has its run attempt's refusal recorded.
func (inv *inventory) refusalRecorded(a Admission) bool {
	_, ok := inv.refusals[refusalKey(a.RepositoryID, a.RunID, a.RunAttempt)]
	return ok
}

// unrecorded reports whether a machine's refused admission is the only
// record that its run must be cancelled: it, and its machine, are kept
// until the refusal is recorded.
func (inv *inventory) unrecorded(machine string) bool {
	a, ok := inv.admissions[machine]
	return ok && !a.Allowed && a.Cancel && !inv.refusalRecorded(a)
}

// blocked are the runs no machine is made for: a consumer's refused runs,
// recorded, until each is shown finished, and those of refusals not yet
// recorded. A selector names no run, so runs are what's blocked: another
// run's jobs of the same selector still get machines.
func (inv *inventory) blocked(q *queue) map[int64]bool {
	out := map[int64]bool{}
	for _, a := range inv.admissions {
		if a.RepositoryID == q.repo.ID && inv.unrecorded(a.Machine) {
			out[a.RunID] = true
		}
	}
	for _, rf := range inv.activeRefusals(q.repo.ID) {
		out[rf.RunID] = true
	}
	return out
}

// followRefusals cancels refused runs until GitHub shows them finished,
// forcing them once an ordinary cancellation hasn't finished them in time.
// A refused admission whose run was never recorded, because the check
// stopped short, is recorded first.
func (s *Service) followRefusals(ctx context.Context, inv *inventory) {
	for _, a := range inv.admissions {
		if a.Allowed || !a.Cancel || s.answering(inv, a) {
			continue
		}
		if inv.refusalRecorded(a) {
			continue
		}
		rf, gen, err := s.recordRefusal(ctx, a)
		if err != nil {
			s.log().Warn("recording a refused run", "run_id", a.RunID, "err", err.Error())
			continue
		}
		inv.refusals[refusalKey(a.RepositoryID, a.RunID, a.RunAttempt)] = record[Refusal]{rf, gen}
	}
	keys := make([]string, 0, len(inv.refusals))
	for k := range inv.refusals {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		r := inv.refusals[k]
		if !r.v.FinishedAt.IsZero() {
			continue
		}
		repo, err := s.repository(ctx, inv, r.v.RepositoryID)
		if err != nil {
			continue
		}
		run, err := s.GitHub.WorkflowRun(ctx, repo, r.v.RunID)
		if errors.Is(err, github.ErrNotFound) || (err == nil && attemptOver(run, r.v)) {
			r.v.FinishedAt = s.now()
			if gen, err := replaceJSON(ctx, s.Objects, k, r.v, r.gen); err == nil {
				inv.refusals[k] = record[Refusal]{r.v, gen}
			}
			s.log().Info("refused run finished", "repository_id", r.v.RepositoryID, "run_id", r.v.RunID)
			continue
		}
		if err != nil {
			s.log().Warn("reading a refused run", "run_id", r.v.RunID, "err", err.Error())
			continue
		}
		s.cancel(ctx, repo, r.v, r.gen)
		if v, gen, ok, err := getJSON[Refusal](ctx, s.Objects, k); err == nil && ok {
			inv.refusals[k] = record[Refusal]{v, gen}
		}
	}
}

func (s *Service) repository(ctx context.Context, inv *inventory, id int64) (github.Repository, error) {
	if r, ok := inv.repos[id]; ok {
		return r, nil
	}
	r, err := s.GitHub.RepositoryByID(ctx, id)
	if err != nil {
		return github.Repository{}, err
	}
	inv.repos[id] = r
	return r, nil
}

// answering reports whether a failed job's hook may still be reading its
// reason: until FailStopDelay after it was answered, or, with no record of
// the answer yet, AnswerWait after the decision, nothing cancels its run or
// stops its machine.
func (s *Service) answering(inv *inventory, a Admission) bool {
	if a.Fail == "" {
		return false
	}
	delay := s.FailStopDelay
	if delay == 0 {
		delay = DefaultFailStopDelay
	}
	if at, ok := inv.answered[a.Machine]; ok {
		return s.now().Sub(at) < delay
	}
	return s.now().Sub(a.At) < AnswerWait
}

// CheckForkApproval checks the organization's setting that outside
// contributors' fork workflows wait for a maintainer's approval, without
// which the service doesn't run (specs/0002-machines.md).
func CheckForkApproval(ctx context.Context, gh github.API) error {
	p, err := gh.ForkApprovalPolicy(ctx)
	if err != nil {
		return err
	}
	if p != ForkApproval {
		return fmt.Errorf("the organization's fork pull request approval setting is %q, not %q", p, ForkApproval)
	}
	return nil
}

// wanted is a queued job that may have a machine of its selector, and is
// still without one.
type wanted struct {
	repo         github.Repository
	sel          selector.Selector
	contribution bool
	job          github.Job
}

// give makes a machine for each queued job that may have one, unless as
// many are already being made or idle for its consumer and selector. Every
// consumer draws on one pool, whose only ceiling is the project's Compute
// Engine quota: machines go to queued jobs oldest first, across all
// consumers, so no consumer or run starves another (specs/0002-machines.md,
// One pool). A job whose machine can't be made for want of quota or
// capacity stays queued, and is tried again by later passes; meanwhile no
// younger job gets a machine that draws on a quota it could have drawn on:
// of its machine families, such as N4, whose machines share their quota,
// or, if it's a Spot job, of the Spot quota every family shares.
func (s *Service) give(ctx context.Context, inv *inventory) {
	if !inv.machinesComplete || !inv.recordsComplete {
		s.log().Warn("making no machines this pass: the machines or records couldn't all be read")
		return
	}
	if err := CheckForkApproval(ctx, s.GitHub); err != nil {
		s.log().Error("making no machines", "err", err.Error())
		return
	}
	var all []wanted
	for _, q := range inv.queues {
		if ctx.Err() != nil {
			return
		}
		all = append(all, s.wants(ctx, inv, q)...)
	}
	slices.SortStableFunc(all, func(a, b wanted) int {
		return cmp.Or(a.job.CreatedAt.Compare(b.job.CreatedAt), cmp.Compare(a.repo.ID, b.repo.ID), cmp.Compare(a.job.ID, b.job.ID))
	})
	short := map[string]bool{}
	for _, w := range all {
		if ctx.Err() != nil {
			return
		}
		// The quota pools the job's machine may draw on: its machine
		// families', and for a Spot job, the Spot quota every family
		// shares.
		var pools []string
		for _, t := range s.Config.Shapes.Shapes[w.sel.Shape].MachineTypes {
			pools = append(pools, t[:strings.Index(t, "-")])
		}
		if w.sel.Spot {
			pools = append(pools, spotPool)
		}
		if slices.ContainsFunc(pools, func(f string) bool { return short[f] }) {
			s.log().Info("a job waits: an older job is waiting for quota or capacity it would draw on", "repository_id", w.repo.ID,
				"job_id", w.job.ID, "selector", w.sel.String())
			continue
		}
		err := s.make(ctx, inv, w)
		if err == nil {
			continue
		}
		s.log().Warn("no machine made; the job stays queued", "repository_id", w.repo.ID, "job_id", w.job.ID, "selector", w.sel.String(),
			"err", err.Error())
		if compute.IsCapacity(err) {
			for _, f := range pools {
				short[f] = true
			}
		}
	}
}

// spotPool names the Spot quota, which every machine family's Spot
// machines share.
const spotPool = "spot"

// wants are a consumer's queued jobs that may have a machine and need a
// new one: of each selector's jobs, those beyond the machines already
// being made or idle for it, oldest first among those with no machine made
// for them. Every job of an offered selector gets a machine of it, a job
// the service will fail included, since the reason is decided for whichever
// job takes the machine, at its check (specs/0002-machines.md, Failing a
// job). A run whose job names a label no runner of the service's has, or
// whose machines never started, is cancelled instead, and why is logged.
func (s *Service) wants(ctx context.Context, inv *inventory, q *queue) []wanted {
	repo := q.repo
	idle := map[string][]Machine{}
	for _, m := range inv.machines {
		if m.RepositoryID != repo.ID || !inv.live(m) {
			continue
		}
		if _, admitted := inv.admissions[m.Name]; !admitted {
			idle[m.Selector.String()] = append(idle[m.Selector.String()], m)
		}
	}
	blocked := inv.blocked(q)
	// A contribution's run is a pull_request_target run, whose own head is
	// the base repository: only its binding, once a maintainer approved it,
	// has its pull request's head.
	contributions := map[int64]bool{}
	contribution := func(run github.WorkflowRun) bool {
		if run.Event != "pull_request_target" {
			return false
		}
		if c, ok := contributions[run.ID]; ok {
			return c
		}
		bd, bound, err := s.Binder.Lookup(ctx, repo.ID, run.ID)
		if err != nil {
			s.log().Warn("reading a run's binding", "repository_id", repo.ID, "run_id", run.ID, "err", err.Error())
			return false
		}
		contributions[run.ID] = bound && bd.HeadRepositoryID != repo.ID
		return contributions[run.ID]
	}

	bySel := map[string][]wanted{}
	for _, job := range q.jobs {
		log := s.log().With("repository_id", repo.ID, "run_id", job.RunID, "job_id", job.ID)
		sel, err := selector.OfJob(job.Labels)
		if errors.Is(err, selector.ErrNoSelector) {
			continue
		}
		if blocked[job.RunID] {
			log.Info("no machine for a job of a refused run until it's finished")
			continue
		}
		// A run refused here is blocked for the rest of the pass too: none
		// of its queued jobs gets a machine.
		refuse := func(reason string) {
			s.refuseRun(ctx, inv, repo, job, reason)
			blocked[job.RunID] = true
		}
		switch {
		case errors.Is(err, selector.ErrOtherLabels):
			refuse("the job names one of the service's selectors with other labels, which no runner of the service's has")
			continue
		case err != nil:
			refuse(fmt.Sprintf("the job names %s, which isn't a selector: lanescape-<vCPUs>vcpu-linux-<amd64|arm64>[-spot]",
				firstClaim(job.Labels)))
			continue
		case s.Config.Shapes.Shapes[sel.Shape] == nil:
			refuse(fmt.Sprintf("the job names %s, a shape the service doesn't offer", sel))
			continue
		}
		run := q.runs[job.RunID]
		if run.Event != "pull_request_target" && run.HeadRepositoryID != repo.ID {
			refuse("a fork's pull request's run gets none of the service's machines")
			continue
		}
		if jr, ok := s.jobRecord(ctx, inv, repo.ID, job.ID); ok && s.now().Sub(jr.FirstMachineAt) >= RetryLimit {
			refuse(fmt.Sprintf("no machine made for job %d started its runner within %s of its first, made at %s",
				job.ID, RetryLimit, jr.FirstMachineAt.Format(time.RFC3339)))
			continue
		}
		w := wanted{repo: repo, sel: sel, job: job, contribution: contribution(run)}
		bySel[sel.String()] = append(bySel[sel.String()], w)
	}

	var out []wanted
	for k, jobs := range bySel {
		jobs = slices.DeleteFunc(jobs, func(w wanted) bool { return blocked[w.job.RunID] })
		have := idle[k]
		madeFor := map[int64]bool{}
		for _, m := range have {
			madeFor[m.MadeFor] = true
		}
		// Jobs without a machine made for them come first, oldest first.
		slices.SortStableFunc(jobs, func(a, b wanted) int {
			return cmp.Or(cmp.Compare(boolInt(madeFor[a.job.ID]), boolInt(madeFor[b.job.ID])), a.job.CreatedAt.Compare(b.job.CreatedAt))
		})
		n := max(len(jobs)-len(have), 0)
		out = append(out, jobs[:n]...)
	}
	return out
}

// firstClaim is the first of a job's labels that claims to be a selector.
func firstClaim(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(strings.ToLower(l), selector.Prefix) {
			return l
		}
	}
	return ""
}

// refuseRun records a queued job's run as refused, with its reason, and
// cancels it, without making it a machine: a job that names a selector
// with other labels, a label that isn't an offered selector, comes from a
// fork's pull request, or whose machines never started within the hour.
// Nothing can print the reason in its log, so it's logged here.
func (s *Service) refuseRun(ctx context.Context, inv *inventory, repo github.Repository, job github.Job, reason string) {
	key := refusalKey(repo.ID, job.RunID, job.RunAttempt)
	if _, ok := inv.refusals[key]; ok {
		return
	}
	s.log().Warn("refusing a run without a machine", "repository_id", repo.ID, "run_id", job.RunID, "job_id", job.ID, "reason", reason)
	rf, gen, err := createOnce(ctx, s.Objects, key, Refusal{RepositoryID: repo.ID, RunID: job.RunID, RunAttempt: job.RunAttempt, Reason: reason,
		RefusedAt: s.now()})
	if err != nil {
		s.log().Warn("recording a refused run", "run_id", job.RunID, "err", err.Error())
		return
	}
	inv.refusals[key] = record[Refusal]{rf, gen}
	s.cancel(ctx, repo, rf, gen)
}

// jobRecord is a job's confirmed first machine. A tentative record is
// confirmed once a machine made for the job is seen; otherwise no machine is
// known to have been made, and it doesn't count.
func (s *Service) jobRecord(ctx context.Context, inv *inventory, repoID, jobID int64) (JobRecord, bool) {
	key := jobKey(repoID, jobID)
	r, ok := inv.jobRecords[key]
	if !ok {
		return JobRecord{}, false
	}
	if !r.v.Tentative {
		return r.v, true
	}
	m, listed := inv.machines[r.v.Machine]
	if listed && m.Labels[LabelJob] != strconv.FormatInt(jobID, 10) {
		return JobRecord{}, false
	}
	if !listed {
		// A machine that's gone was made if its boot log was saved, or
		// recorded to be read, as one is before every deletion.
		gone := Machine{Instance: compute.Instance{Name: r.v.Machine}, CreatedAt: r.v.FirstMachineAt}
		_, _, err := s.Objects.Get(ctx, bootLogKey(gone))
		if err != nil {
			if _, _, perr := s.Objects.Get(ctx, pendingLogKey(gone)); perr != nil {
				return JobRecord{}, false
			}
		}
	}
	// The machine shows the record's time was its first, whether or not
	// its confirmation is written now; one that isn't is tried again, and
	// the machine isn't deleted until it is.
	confirmed := r.v
	confirmed.Tentative = false
	if gen, err := replaceJSON(ctx, s.Objects, key, confirmed, r.gen); err == nil {
		inv.jobRecords[key] = record[JobRecord]{confirmed, gen}
	}
	return confirmed, true
}

// noteJob records, before a job's machine is made, when its first machine
// is: tentatively, unless one is already confirmed. It returns the record's
// generation if this attempt wrote it, or 0.
func (s *Service) noteJob(ctx context.Context, inv *inventory, repoID, jobID int64, machine string, at time.Time) (int64, error) {
	key := jobKey(repoID, jobID)
	rec := JobRecord{RepositoryID: repoID, JobID: jobID, FirstMachineAt: at, Machine: machine, Tentative: true}
	if _, ok := s.jobRecord(ctx, inv, repoID, jobID); ok {
		return 0, nil
	}
	if r, ok := inv.jobRecords[key]; ok {
		gen, err := replaceJSON(ctx, s.Objects, key, rec, r.gen)
		if err != nil {
			return 0, err
		}
		inv.jobRecords[key] = record[JobRecord]{rec, gen}
		return gen, nil
	}
	gen, err := createJSON(ctx, s.Objects, key, rec)
	if errors.Is(err, store.ErrPrecondition) {
		// Written already, perhaps by our own write whose answer was lost.
		if v, g, ok, rerr := getJSON[JobRecord](ctx, s.Objects, key); rerr == nil && ok {
			inv.jobRecords[key] = record[JobRecord]{v, g}
			if v.Tentative && v.Machine == machine {
				return g, nil
			}
		}
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	inv.jobRecords[key] = record[JobRecord]{rec, gen}
	return gen, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// group is a consumer's runner group, made on first use: limited to its
// repository, which is checked and set right each pass.
func (s *Service) group(ctx context.Context, inv *inventory, repo github.Repository) (github.RunnerGroup, error) {
	name := GroupName(repo.ID)
	if g, ok := inv.checked[name]; ok {
		return g, nil
	}
	if inv.groups == nil {
		gs, err := s.GitHub.RunnerGroups(ctx)
		if err != nil {
			return github.RunnerGroup{}, err
		}
		inv.groups = map[string]github.RunnerGroup{}
		for _, g := range gs {
			inv.groups[g.Name] = g
		}
	}
	g, ok := inv.groups[name]
	if !ok {
		made, err := s.GitHub.CreateRunnerGroup(ctx, name, repo.ID)
		if errors.Is(err, github.ErrConflict) {
			inv.groups = nil
			return s.group(ctx, inv, repo)
		}
		if err != nil {
			return github.RunnerGroup{}, err
		}
		s.log().Info("runner group made", "repository_id", repo.ID, "group", name)
		inv.groups[name], inv.checked[name] = made, made
		return made, nil
	}
	if g.Visibility != "selected" || g.RestrictedToWorkflows || g.Default {
		return github.RunnerGroup{}, fmt.Errorf("runner group %s isn't limited to its repository as the service made it", name)
	}
	ids, err := s.GitHub.RunnerGroupRepositories(ctx, g.ID)
	if err != nil {
		return github.RunnerGroup{}, err
	}
	if !slices.Equal(ids, []int64{repo.ID}) {
		if err := s.GitHub.SetRunnerGroupRepositories(ctx, g.ID, []int64{repo.ID}); err != nil {
			return github.RunnerGroup{}, err
		}
		s.log().Warn("runner group limited to its repository again", "group", name, "had", ids)
	}
	inv.checked[name] = g
	return g, nil
}

// baseImage is the base image machines of an architecture boot,
// lsr-base-<arch>-<key>: the current key's once it's ready, and until then
// the previous one (specs/0003-disks.md, Which image machines boot).
func (s *Service) baseImage(ctx context.Context, inv *inventory, arch string) (string, error) {
	if im, ok := inv.images[arch]; ok {
		return im, nil
	}
	ims, err := s.Compute.Images(ctx)
	if err != nil {
		return "", err
	}
	var name string
	var ok bool
	if s.Bases != nil {
		name, ok = s.Bases.Boot(ims, arch)
	} else {
		name, ok = bases.Boot(ims, arch, "")
	}
	if !ok {
		return "", fmt.Errorf("there's no ready base image for %s", arch)
	}
	inv.images[arch] = name
	return name, nil
}

// make makes one machine for a job: a just-in-time runner for the selector
// in its consumer's runner group, then the machine, booted from the base
// image, in the first placement with capacity. A Spot selector's machine is
// standard only if no Spot placement had capacity.
func (s *Service) make(ctx context.Context, inv *inventory, w wanted) error {
	repo, sel, job := w.repo, w.sel, w.job
	shape := s.Config.Shapes.Shapes[sel.Shape]
	g, err := s.group(ctx, inv, repo)
	if err != nil {
		return fmt.Errorf("runner group: %w", err)
	}
	image, err := s.baseImage(ctx, inv, sel.Arch)
	if err != nil {
		return err
	}
	bootGB := int64(s.Config.Shapes.BootDiskGB)
	name := newName(repo.ID)
	inv.attempted[name] = true
	runner, jit, err := s.GitHub.JITConfig(ctx, g.ID, name, sel.RunnerLabels())
	if err != nil {
		return fmt.Errorf("registering a runner: %w", err)
	}
	// A runner's only label is its selector: one GitHub gave any other is
	// removed, and no machine is made for it.
	if !onlyLabel(runner.Labels, sel.String()) {
		if err := s.GitHub.DeleteRunner(ctx, runner.ID); err != nil && !errors.Is(err, github.ErrNotFound) {
			s.log().Warn("removing a runner with other labels", "runner", name, "err", err.Error())
		}
		return fmt.Errorf("GitHub registered runner %s with labels %v, not its selector alone", name, runner.Labels)
	}
	created := s.now()
	// The job's first machine is recorded before it's made: another is made
	// while the job is queued for up to an hour from then. If no machine
	// was made, the record this attempt made is taken back.
	jkey := jobKey(repo.ID, job.ID)
	jgen, err := s.noteJob(ctx, inv, repo.ID, job.ID, name, created)
	if err != nil {
		_ = s.GitHub.DeleteRunner(ctx, runner.ID)
		return fmt.Errorf("recording the job's first machine: %w", err)
	}
	confirm := func() {
		if jgen == 0 {
			return
		}
		rec := JobRecord{RepositoryID: repo.ID, JobID: job.ID, FirstMachineAt: created, Machine: name}
		if gen, err := replaceJSON(ctx, s.Objects, jkey, rec, jgen); err == nil {
			inv.jobRecords[jkey] = record[JobRecord]{rec, gen}
		}
	}
	// A tentative record whose machine was never made is ignored, so a
	// failure to take it back only leaves it to be replaced.
	takeBack := func() {
		if jgen != 0 && s.Objects.Delete(ctx, jkey, jgen) == nil {
			delete(inv.jobRecords, jkey)
		}
	}
	labels := map[string]string{
		LabelRepository: strconv.FormatInt(repo.ID, 10), LabelSelector: sel.Hash(), LabelCreated: strconv.FormatInt(created.Unix(), 10),
		LabelJob: strconv.FormatInt(job.ID, 10), LabelContribution: strconv.FormatBool(w.contribution),
	}
	metadata := map[string]string{
		MetaSelector: sel.String(), MetaService: s.ServiceURL, MetaRunnerID: strconv.FormatInt(runner.ID, 10),
		MetaJob: strconv.FormatInt(job.ID, 10), MetaJITConfig: jit, "enable-guest-attributes": "TRUE", "serial-port-logging-enable": "TRUE",
	}
	spec := compute.InstanceSpec{
		Name: name, Image: image, BootDiskGB: bootGB, DiskType: DiskType,
		ProvisionedIOPS: int64(s.Config.Shapes.DiskIOPS), ProvisionedThroughput: int64(s.Config.Shapes.DiskThroughputMiBps),
		Labels: labels, Metadata: metadata,
		ServiceAccount: s.MachineAccount, Scopes: scopes, Network: Network, Subnetwork: Subnetwork, MaxRunDuration: MaxRunDuration,
	}
	var last error
	for _, pl := range selector.Placements(shape, s.Config.Shapes.Zones, sel.Spot) {
		spec.MachineType, spec.Spot = pl.MachineType, pl.Spot
		err := s.Compute.CreateInstance(ctx, pl.Zone, spec, compute.RequestID("create", name, pl.Zone, pl.MachineType, strconv.FormatBool(pl.Spot)))
		if err == nil {
			confirm()
			s.log().Info("machine made", "machine", name, "repository_id", repo.ID, "job_id", job.ID, "selector", sel.String(),
				"zone", pl.Zone, "machine_type", pl.MachineType, "spot", pl.Spot, "contribution", w.contribution, "image", image)
			return nil
		}
		if compute.IsCapacity(err) {
			s.log().Info("no capacity for a machine", "machine", name, "zone", pl.Zone, "machine_type", pl.MachineType, "spot", pl.Spot, "err", err.Error())
			last = err
			continue
		}
		if compute.IsUnavailable(err) {
			// It may have been made: cleanup deals with it either way.
			return err
		}
		last = err
		break
	}
	// Nothing was made: the runner's registration goes too.
	takeBack()
	if err := s.GitHub.DeleteRunner(ctx, runner.ID); err != nil && !errors.Is(err, github.ErrNotFound) {
		s.log().Warn("removing an unused runner registration", "runner", name, "err", err.Error())
	}
	return last
}

// onlyLabel reports whether a runner's labels are exactly its selector.
func onlyLabel(labels []string, sel string) bool {
	return len(labels) == 1 && strings.EqualFold(labels[0], sel)
}
