package machines

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/lanescape/runners/internal/binding"
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
	reports         map[string]Report
	diskRecords     map[string]record[DiskRecord]
	refusals        map[string]record[Refusal]
	jobRecords      map[string]record[JobRecord]
	machineKeys     map[string][]store.Object

	// What this pass learns and does as it goes.
	groups    map[string]github.RunnerGroup
	checked   map[int64]github.RunnerGroup
	images    map[string]string
	bindings  map[int64]*binding.Binding
	approvals map[int64]bool
	attempted map[string]bool
	deleted   map[string]bool
}

// Pass is the machines' part of a reconciler pass (specs/0001-architecture.md,
// Passes): it gives queued jobs machines, follows up refused runs, and
// deletes what's finished. A failure on one machine, job or disk doesn't
// stop the rest.
func (s *Service) Pass(p *pass.Pass) error {
	inv := s.inventory(p)
	if p.Lost() {
		return context.Cause(p.Context())
	}
	ctx := p.Context()
	s.followRefusals(ctx, inv)
	s.give(ctx, inv)
	s.cleanup(ctx, inv)
	if p.Lost() {
		return context.Cause(p.Context())
	}
	return nil
}

func (s *Service) inventory(p *pass.Pass) *inventory {
	ctx := p.Context()
	inv := &inventory{machines: map[string]Machine{}, repos: map[int64]github.Repository{}, groups: nil,
		checked: map[int64]github.RunnerGroup{}, images: map[string]string{}, bindings: map[int64]*binding.Binding{},
		approvals: map[int64]bool{}, attempted: map[string]bool{}, deleted: map[string]bool{}}

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
	inv.admissions, inv.reports = map[string]Admission{}, map[string]Report{}
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
		case "report.json":
			return read(o.Name, func(_ int64, dec func(any) error) {
				var r Report
				if dec(&r) == nil {
					inv.reports[parts[1]] = r
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

// live reports whether a machine counts against its consumer's limits: it
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

// blocked are the selectors no machine is made for: those of a consumer's
// refused runs, recorded and still queued, until each run is shown finished.
func (inv *inventory) blocked(q *queue) map[string]bool {
	out := map[string]bool{}
	for _, rf := range inv.activeRefusals(q.repo.ID) {
		for _, sel := range rf.Selectors {
			out[sel] = true
		}
		for _, j := range q.runJobs[rf.RunID] {
			if sel, err := selector.OfJob(j.Labels); err == nil && j.Status != "completed" {
				out[sel.String()] = true
			}
		}
	}
	return out
}

// followRefusals cancels refused runs until GitHub shows them finished,
// forcing them once an ordinary cancellation hasn't finished them in time.
// A refused admission whose run was never recorded, because the check
// stopped short, is recorded first.
func (s *Service) followRefusals(ctx context.Context, inv *inventory) {
	for _, a := range inv.admissions {
		if a.Allowed || !a.Cancel {
			continue
		}
		key := refusalKey(a.RepositoryID, a.RunID)
		if r, ok := inv.refusals[key]; ok && slices.Contains(r.v.Selectors, a.Selector) {
			continue
		}
		rf, gen, err := s.recordRefusal(ctx, a)
		if err != nil {
			s.log().Warn("recording a refused run", "run_id", a.RunID, "err", err.Error())
			continue
		}
		inv.refusals[key] = record[Refusal]{rf, gen}
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
		if errors.Is(err, github.ErrNotFound) || (err == nil && run.Status == "completed") {
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

// lookup is a run's binding, read once a pass.
func (s *Service) lookup(ctx context.Context, inv *inventory, repoID, runID int64) (binding.Binding, bool, error) {
	if b, ok := inv.bindings[runID]; ok {
		if b == nil {
			return binding.Binding{}, false, nil
		}
		return *b, true, nil
	}
	b, ok, err := s.Binder.Lookup(ctx, repoID, runID)
	if err != nil {
		return binding.Binding{}, false, err
	}
	if ok {
		inv.bindings[runID] = &b
	} else {
		inv.bindings[runID] = nil
	}
	return b, ok, nil
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

// demand is the queued jobs of one consumer that may have a machine of one
// selector.
type demand struct {
	sel          selector.Selector
	contribution bool
	jobs         []github.Job
}

// give makes a machine for each queued job that may have one, unless as
// many are already being made or idle for its consumer and selector, within
// the consumer's limits.
func (s *Service) give(ctx context.Context, inv *inventory) {
	if !inv.machinesComplete || !inv.recordsComplete {
		s.log().Warn("making no machines this pass: the machines or records couldn't all be read")
		return
	}
	if err := CheckForkApproval(ctx, s.GitHub); err != nil {
		s.log().Error("making no machines", "err", err.Error())
		return
	}
	for _, q := range inv.queues {
		if ctx.Err() != nil {
			return
		}
		s.giveConsumer(ctx, inv, q)
	}
}

func (s *Service) giveConsumer(ctx context.Context, inv *inventory, q *queue) {
	repo := q.repo
	lim := s.Config.Limits.For(repo.ID)
	live, liveContributions := 0, 0
	idle := map[string][]Machine{}
	for _, m := range inv.machines {
		if m.RepositoryID != repo.ID || !inv.live(m) {
			continue
		}
		if m.Contribution {
			liveContributions++
		} else {
			live++
		}
		if _, admitted := inv.admissions[m.Name]; !admitted {
			idle[m.Selector.String()] = append(idle[m.Selector.String()], m)
		}
	}
	blocked := inv.blocked(q)
	refusedRuns := map[int64]bool{}
	for _, rf := range inv.activeRefusals(repo.ID) {
		refusedRuns[rf.RunID] = true
	}

	wants := map[string]*demand{}
	for _, job := range q.jobs {
		sel, err := selector.OfJob(job.Labels)
		if errors.Is(err, selector.ErrNoSelector) {
			continue
		}
		log := s.log().With("repository_id", repo.ID, "run_id", job.RunID, "job_id", job.ID)
		if err != nil {
			log.Info("no machine for a job whose labels aren't one selector", "reason", err.Error())
			continue
		}
		if s.Config.Shapes.Shapes[sel.Shape] == nil {
			log.Info("no machine for a job of an unknown shape", "selector", sel.String())
			continue
		}
		if blocked[sel.String()] || refusedRuns[job.RunID] {
			log.Info("no machine for a selector of a refused run until it's finished", "selector", sel.String())
			continue
		}
		run := q.runs[job.RunID]
		bd, bound, err := s.lookup(ctx, inv, repo.ID, run.ID)
		if err != nil {
			log.Warn("reading a run's binding", "err", err.Error())
			continue
		}
		if !bound {
			log.Debug("no machine for a job of a run with no binding")
			continue
		}
		if bd.Scope != sel.Scope {
			log.Info("no machine for a job whose selector's scope isn't its run's", "selector", sel.String(), "binding", bd.Scope.String())
			continue
		}
		contribution := false
		if isFork(repo, run, bd, bound) {
			if run.Event != "pull_request_target" {
				log.Info("no machine for a fork's pull request's job")
				continue
			}
			ok, cached := inv.approvals[run.ID]
			if !cached {
				ok, err = s.approved(ctx, repo, run.ID)
				if err != nil {
					log.Warn("reading a contribution's approvals", "err", err.Error())
					continue
				}
				inv.approvals[run.ID] = ok
			}
			if !ok {
				log.Info("no machine for a contribution that isn't approved")
				continue
			}
			contribution = true
		}
		if jr, ok := inv.jobRecords[jobKey(repo.ID, job.ID)]; ok && s.now().Sub(jr.v.FirstMachineAt) >= RetryLimit {
			log.Warn("leaving a job queued: its first machine was made over an hour ago", "first_machine_at", jr.v.FirstMachineAt)
			continue
		}
		key := sel.String() + "/" + strconv.FormatBool(contribution)
		d := wants[key]
		if d == nil {
			d = &demand{sel: sel, contribution: contribution}
			wants[key] = d
		}
		d.jobs = append(d.jobs, job)
	}

	keys := make([]string, 0, len(wants))
	for k := range wants {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		d := wants[k]
		have := idle[d.sel.String()]
		n := len(d.jobs) - len(have)
		room := lim.Machines - live
		if d.contribution {
			room = lim.ContributionMachines - liveContributions
		}
		if n > room {
			s.log().Info("jobs wait for room under the consumer's limit", "repository_id", repo.ID, "selector", d.sel.String(),
				"waiting", n-max(room, 0), "contribution", d.contribution)
			n = room
		}
		// Jobs without a machine made for them come first.
		madeFor := map[int64]bool{}
		for _, m := range have {
			madeFor[m.MadeFor] = true
		}
		jobs := slices.Clone(d.jobs)
		slices.SortStableFunc(jobs, func(a, b github.Job) int {
			return cmp.Compare(boolInt(madeFor[a.ID]), boolInt(madeFor[b.ID]))
		})
		for i := 0; i < n && i < len(jobs); i++ {
			if ctx.Err() != nil {
				return
			}
			if err := s.make(ctx, inv, repo, d.sel, jobs[i], d.contribution); err != nil {
				s.log().Warn("no machine made", "repository_id", repo.ID, "job_id", jobs[i].ID, "selector", d.sel.String(), "err", err.Error())
				continue
			}
			if d.contribution {
				liveContributions++
			} else {
				live++
			}
		}
	}
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
	if g, ok := inv.checked[repo.ID]; ok {
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
	name := GroupName(repo.ID)
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
		inv.groups[name], inv.checked[repo.ID] = made, made
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
	inv.checked[repo.ID] = g
	return g, nil
}

// baseImage is the newest ready base image for an architecture,
// lsr-base-<arch>-<key> (specs/0003-disks.md).
func (s *Service) baseImage(ctx context.Context, inv *inventory, arch string) (string, error) {
	if im, ok := inv.images[arch]; ok {
		return im, nil
	}
	ims, err := s.Compute.Images(ctx)
	if err != nil {
		return "", err
	}
	var best compute.Image
	for _, im := range ims {
		if strings.HasPrefix(im.Name, "lsr-base-"+arch+"-") && im.Status == "READY" && (best.Name == "" || im.Created.After(best.Created)) {
			best = im
		}
	}
	if best.Name == "" {
		return "", fmt.Errorf("there's no ready base image for %s", arch)
	}
	inv.images[arch] = best.Name
	return best.Name, nil
}

// make makes one machine for a job: a just-in-time runner for the selector
// in the consumer's group, then the machine, in the first placement with
// capacity. A Spot selector's machine is standard only if no Spot placement
// had capacity.
func (s *Service) make(ctx context.Context, inv *inventory, repo github.Repository, sel selector.Selector, job github.Job, contribution bool) error {
	shape := s.Config.Shapes.Shapes[sel.Shape]
	g, err := s.group(ctx, inv, repo)
	if err != nil {
		return fmt.Errorf("runner group: %w", err)
	}
	image, err := s.baseImage(ctx, inv, sel.Arch)
	if err != nil {
		return err
	}
	name := newName(repo.ID)
	inv.attempted[name] = true
	runner, jit, err := s.GitHub.JITConfig(ctx, g.ID, name, sel.RunnerLabels())
	if err != nil {
		return fmt.Errorf("registering a runner: %w", err)
	}
	created := s.now()
	// The job's first machine is recorded once it's made, or may have
	// been: another is made while it's queued for up to an hour from then.
	noteJob := func() {
		if _, _, err := createOnce(ctx, s.Objects, jobKey(repo.ID, job.ID), JobRecord{RepositoryID: repo.ID, JobID: job.ID, FirstMachineAt: created}); err != nil {
			s.log().Warn("recording a job's first machine", "job_id", job.ID, "err", err.Error())
		}
	}
	spec := compute.InstanceSpec{
		Name: name, Image: image, BootDiskGB: int64(s.Config.Shapes.BootDiskGB), DiskType: DiskType,
		ProvisionedIOPS: int64(s.Config.Shapes.DiskIOPS), ProvisionedThroughput: int64(s.Config.Shapes.DiskThroughputMiBps),
		Labels: map[string]string{
			LabelRepository: strconv.FormatInt(repo.ID, 10), LabelSelector: sel.Hash(), LabelCreated: strconv.FormatInt(created.Unix(), 10),
			LabelJob: strconv.FormatInt(job.ID, 10), LabelContribution: strconv.FormatBool(contribution),
		},
		Metadata: map[string]string{
			MetaSelector: sel.String(), MetaService: s.ServiceURL, MetaRunnerID: strconv.FormatInt(runner.ID, 10),
			MetaJob: strconv.FormatInt(job.ID, 10), MetaJITConfig: jit, "enable-guest-attributes": "TRUE",
		},
		ServiceAccount: s.MachineAccount, Scopes: scopes, Network: Network, MaxRunDuration: MaxRunDuration,
	}
	var last error
	for _, pl := range selector.Placements(shape, s.Config.Shapes.Zones, sel.Spot) {
		spec.MachineType, spec.Spot = pl.MachineType, pl.Spot
		err := s.Compute.CreateInstance(ctx, pl.Zone, spec, compute.RequestID("create", name, pl.Zone, pl.MachineType, strconv.FormatBool(pl.Spot)))
		if err == nil {
			noteJob()
			s.log().Info("machine made", "machine", name, "repository_id", repo.ID, "job_id", job.ID, "selector", sel.String(),
				"zone", pl.Zone, "machine_type", pl.MachineType, "spot", pl.Spot, "contribution", contribution)
			return nil
		}
		if compute.IsCapacity(err) {
			s.log().Info("no capacity for a machine", "machine", name, "zone", pl.Zone, "machine_type", pl.MachineType, "spot", pl.Spot, "err", err.Error())
			last = err
			continue
		}
		if compute.IsUnavailable(err) {
			// It may have been made: cleanup deals with it either way.
			noteJob()
			return err
		}
		last = err
		break
	}
	// Nothing was made: the runner's registration goes too.
	if err := s.GitHub.DeleteRunner(ctx, runner.ID); err != nil && !errors.Is(err, github.ErrNotFound) {
		s.log().Warn("removing an unused runner registration", "runner", name, "err", err.Error())
	}
	return last
}
