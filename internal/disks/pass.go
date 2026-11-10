package disks

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/store"
)

// state is what the disks' part of a pass works from: Compute Engine's
// images, read before the records that describe them, since every record
// is written before what it describes is made.
type state struct {
	images      map[string]compute.Image
	imagesRead  bool
	records     map[string]record[Image]
	recents     map[string]record[Recent]
	families    map[string]record[Family]
	closures    map[string]record[Closure]
	workDisks   map[string]record[machines.DiskRecord]
	choices     map[string]record[Choice]
	claims      map[string]record[Claim]
	preemptions map[string]record[Preemption]
	events      map[string]record[Event]
	reruns      map[string]record[Rerun]
	complete    bool

	repos     map[int64]*github.Repository
	runs      map[string]*run
	pulls     map[string]*github.PullRequest
	jobs      map[int64]*github.Job
	preempted map[string][]uint64
	// live are the runs with an admitted machine that hasn't reported,
	// read once a pass when first needed.
	live map[int64]bool
}

// run is what a pass learns of one run.
type run struct {
	found    bool
	finished bool
	attempt  int
	// failed are the IDs of its latest attempt's failed jobs, once it's
	// finished.
	failed map[int64]bool
	// due says it's finished and still due an automatic rerun, or may yet
	// be, while a lost job's evidence is unknown.
	due bool
	// rerun says it should be rerun now.
	rerun bool
}

func runKey(repo, id int64) string {
	return strconv.FormatInt(repo, 10) + "/" + strconv.FormatInt(id, 10)
}

// Pass is the disks' part of a pass (specs/0001-architecture.md, Passes),
// after the machines': it looks for evidence that stopped Spot machines
// were preempted, reruns runs that lost a Spot job to preemption once
// they've finished, drops the restore choices of runs that need them no
// more, and deletes saved disks and resume images that are superseded,
// expired or orphaned. A failure on one image or run doesn't stop the rest.
func (s *Service) Pass(p *pass.Pass) error {
	ctx := p.Context()
	st := s.read(ctx)
	if p.Lost() {
		return context.Cause(ctx)
	}
	s.evidence(ctx, st)
	s.follow(ctx, st)
	if st.complete {
		s.dropChoices(ctx, st)
		s.expire(ctx, st)
		s.tidy(ctx, st)
	} else {
		s.log().Warn("deleting no images this pass: the images or records couldn't all be read")
	}
	if p.Lost() {
		return context.Cause(ctx)
	}
	return nil
}

func (s *Service) read(ctx context.Context) *state {
	st := &state{images: map[string]compute.Image{}, complete: true, repos: map[int64]*github.Repository{}, runs: map[string]*run{},
		pulls: map[string]*github.PullRequest{}, jobs: map[int64]*github.Job{}, preempted: map[string][]uint64{}}
	ims, err := s.Compute.Images(ctx)
	if err != nil {
		s.log().Error("listing images", "err", err.Error())
		st.complete = false
	} else {
		st.imagesRead = true
	}
	for _, im := range ims {
		if _, ok := ours(im); ok {
			st.images[im.Name] = im
		}
	}
	list := func(prefix string, into func() error) {
		if err := into(); err != nil {
			s.log().Error("listing records", "prefix", prefix, "err", err.Error())
			st.complete = false
		}
	}
	list("images/", func() (err error) { st.records, err = listJSON[Image](ctx, s.Objects, "images/"); return })
	// After the images' records: a choice writes its pull request's recent
	// record before it touches the image it chose.
	list("recent/", func() (err error) { st.recents, err = listJSON[Recent](ctx, s.Objects, "recent/"); return })
	// After the images' records too: a use marks its family before it
	// touches the image.
	list("families/", func() (err error) { st.families, err = listJSON[Family](ctx, s.Objects, "families/"); return })
	list("closures/", func() (err error) { st.closures, err = listJSON[Closure](ctx, s.Objects, "closures/"); return })
	list("disks/", func() (err error) {
		st.workDisks, err = listJSON[machines.DiskRecord](ctx, s.Objects, "disks/")
		return
	})
	list("choices/", func() (err error) { st.choices, err = listJSON[Choice](ctx, s.Objects, "choices/"); return })
	list("claims/", func() (err error) { st.claims, err = listJSON[Claim](ctx, s.Objects, "claims/"); return })
	list("preemptions/", func() (err error) { st.preemptions, err = listJSON[Preemption](ctx, s.Objects, "preemptions/"); return })
	list("events/", func() (err error) { st.events, err = listJSON[Event](ctx, s.Objects, "events/"); return })
	list("reruns/", func() (err error) { st.reruns, err = listJSON[Rerun](ctx, s.Objects, "reruns/"); return })
	return st
}

func (s *Service) repository(ctx context.Context, st *state, id int64) (*github.Repository, error) {
	if r, ok := st.repos[id]; ok {
		return r, nil
	}
	r, err := s.GitHub.RepositoryByID(ctx, id)
	if errors.Is(err, github.ErrNotFound) {
		st.repos[id] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.OwnerID != s.Config.Organization.ID {
		// Transferred away: no longer a consumer, so nothing of it is
		// read or rerun.
		s.log().Warn("a repository with records is no longer the organization's", "repository_id", id)
		st.repos[id] = nil
		return nil, nil
	}
	st.repos[id] = &r
	return &r, nil
}

// evidence looks, by instance ID, for evidence that each stopped Spot
// machine recorded as perhaps preempted was: Compute Engine's recorded
// event, or a zone operation of type compute.instances.preempted, for a
// day after it stopped.
func (s *Service) evidence(ctx context.Context, st *state) {
	for key, r := range st.preemptions {
		p := r.v
		if p.Preempted != Unknown || s.now().Sub(p.StoppedAt) >= ResumeKept {
			continue
		}
		_, found := st.events[eventKey(p.InstanceID)]
		if !found {
			ids, ok := st.preempted[p.Zone]
			if !ok {
				var err error
				if ids, err = s.Compute.Preempted(ctx, p.Zone); err != nil {
					s.log().Warn("listing preemptions", "zone", p.Zone, "err", err.Error())
					continue
				}
				st.preempted[p.Zone] = ids
			}
			found = slices.Contains(ids, p.InstanceID)
		}
		if !found {
			continue
		}
		p.Preempted = Preempted
		if gen, err := replaceJSON(ctx, s.Objects, key, p, r.gen); err == nil {
			st.preemptions[key] = record[Preemption]{p, gen}
			s.log().Info("a stopped Spot machine was preempted", "machine", p.Machine, "instance_id", p.InstanceID, "run_id", p.RunID)
		}
	}
}

// runState is what GitHub shows of a run, and whether it's due an automatic
// rerun (specs/0002-machines.md, Reruns): finished, with fewer than 3
// attempts in all, and a failed job of its latest attempt that was a Spot
// job shown preempted; or, while a day hasn't passed, perhaps preempted. An
// error means GitHub gave no confirmed answer.
func (s *Service) runState(ctx context.Context, st *state, repoID, runID int64) (*run, error) {
	key := runKey(repoID, runID)
	if r, ok := st.runs[key]; ok {
		return r, nil
	}
	repo, err := s.repository(ctx, st, repoID)
	if err != nil {
		return nil, err
	}
	r := &run{finished: true}
	if repo != nil {
		wr, err := s.GitHub.WorkflowRun(ctx, *repo, runID)
		switch {
		case errors.Is(err, github.ErrNotFound):
		case err != nil:
			return nil, err
		default:
			r.found, r.finished, r.attempt = true, wr.Status == "completed", wr.Attempt
		}
	}
	if r.found && r.finished {
		jobs, err := s.GitHub.RunJobs(ctx, *repo, runID)
		if err != nil {
			return nil, err
		}
		r.failed = map[int64]bool{}
		for _, j := range jobs {
			if failedConclusion[j.Conclusion] {
				r.failed[j.ID] = true
			}
		}
		// An attempt already recorded as rerun, which GitHub still shows
		// as the latest, is one whose rerun didn't take: it's asked for
		// again, without counting again.
		asked, pending := 0, false
		if rr, ok := st.reruns[rerunKey(repoID, runID)]; ok {
			asked, pending = len(rr.v.Attempts), slices.Contains(rr.v.Attempts, r.attempt)
		}
		if r.attempt < MaxAttempts && (asked < MaxAttempts-1 || pending) {
			for _, p := range st.preemptions {
				v := p.v
				if v.RepositoryID != repoID || v.RunID != runID || v.RunAttempt != r.attempt || !r.failed[v.JobID] {
					continue
				}
				switch {
				case v.Preempted == Preempted:
					r.due, r.rerun = true, true
				case s.now().Sub(v.StoppedAt) < ResumeKept:
					r.due = true
				}
			}
		}
	}
	st.runs[key] = r
	return r, nil
}

// failedConclusion are the conclusions of the jobs that rerunning a run's
// failed jobs runs again.
var failedConclusion = map[string]bool{"failure": true, "cancelled": true, "timed_out": true, "startup_failure": true}

// follow reruns each finished run that lost a Spot job to preemption: its
// failed jobs, rather than each lost job and its dependents, so there's one
// rerun per run attempt. Each rerun is recorded, by the attempt it reran,
// before it's asked for.
func (s *Service) follow(ctx context.Context, st *state) {
	seen := map[string]bool{}
	for _, r := range st.preemptions {
		p := r.v
		key := runKey(p.RepositoryID, p.RunID)
		if seen[key] || ctx.Err() != nil {
			continue
		}
		seen[key] = true
		rs, err := s.runState(ctx, st, p.RepositoryID, p.RunID)
		if err != nil {
			s.log().Warn("reading a run that lost a Spot machine", "run_id", p.RunID, "err", err.Error())
			continue
		}
		if !rs.rerun {
			continue
		}
		if err := s.rerun(ctx, st, p.RepositoryID, p.RunID, rs); err != nil {
			s.log().Warn("rerunning a run that lost a Spot job to preemption", "run_id", p.RunID, "err", err.Error())
		}
	}
}

// rerun asks GitHub to rerun a run's failed jobs, under the disks' lease,
// which every reading of a restore choice takes.
func (s *Service) rerun(ctx context.Context, st *state, repoID, runID int64, rs *run) error {
	return s.step(ctx, repoID, func(ctx context.Context) error { return s.rerunLocked(ctx, st, repoID, runID, rs) })
}

func (s *Service) rerunLocked(ctx context.Context, st *state, repoID, runID int64, rs *run) error {
	repo := st.repos[repoID]
	// What the pass read may have changed while it waited for the lease:
	// a maintainer may have rerun the run since.
	if wr, err := s.GitHub.WorkflowRun(ctx, *repo, runID); err != nil {
		return err
	} else if wr.Attempt != rs.attempt || wr.Status != "completed" {
		rs.rerun = false
		return nil
	}
	key := rerunKey(repoID, runID)
	rec, gen, err := createOnce(ctx, s.Objects, key, Rerun{RepositoryID: repoID, RunID: runID, At: s.now()})
	if err != nil {
		return err
	}
	// Each attempt rerun is counted before it's asked for, so a lost
	// answer never lets one be asked for twice. Whether the attempt it
	// began is the service's own is GitHub's record of who triggered it.
	if !slices.Contains(rec.Attempts, rs.attempt) {
		rec.Attempts, rec.At = append(rec.Attempts, rs.attempt), s.now()
		if gen, err = replaceJSON(ctx, s.Objects, key, rec, gen); err != nil {
			return err
		}
		st.reruns[key] = record[Rerun]{rec, gen}
	}
	err = s.GitHub.RerunFailedJobs(ctx, *repo, runID)
	if err != nil && github.IsUnavailable(err) {
		// An answer lost after GitHub began the next attempt is a rerun
		// all the same.
		if wr, rerr := s.GitHub.WorkflowRun(ctx, *repo, runID); rerr == nil && wr.Attempt > rs.attempt {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	rs.finished, rs.rerun = false, false
	s.log().Info("run rerun: a Spot job was preempted", "repository_id", repoID, "run_id", runID, "attempt", rs.attempt+1)
	return nil
}

// dropChoices drops the restore choices of runs that have finished and have
// no automatic rerun left; a later rerun, by a maintainer, picks again.
// Each is dropped under the disks' lease, which every reading of a choice
// takes, once GitHub shows its run still finished: a rerun's job that read
// the choice first has its run shown in progress, and one that comes after
// picks again.
func (s *Service) dropChoices(ctx context.Context, st *state) {
	byRepo := map[int64][]string{}
	for key, r := range st.choices {
		rs, err := s.runState(ctx, st, r.v.RepositoryID, r.v.RunID)
		if err != nil || !rs.finished || rs.due {
			continue
		}
		byRepo[r.v.RepositoryID] = append(byRepo[r.v.RepositoryID], key)
	}
	for repo, keys := range byRepo {
		slices.Sort(keys)
		err := s.step(ctx, repo, func(ctx context.Context) error {
			live, err := s.unreported(ctx, "")
			if err != nil {
				return err
			}
			for _, key := range keys {
				r := st.choices[key]
				if ok, err := s.settled(ctx, repo, r.v.RunID, live); err != nil || !ok {
					continue
				}
				if err := s.Objects.Delete(ctx, key, r.gen); err == nil || errors.Is(err, store.ErrNotFound) {
					delete(st.choices, key)
				}
			}
			return nil
		})
		if err != nil {
			s.log().Warn("dropping finished runs' restore choices", "repository_id", repo, "err", err.Error())
		}
	}
}

// pull is a pull request, read once a pass; nil if GitHub has none of that
// number.
func (s *Service) pull(ctx context.Context, st *state, repoID int64, sc string) (*github.PullRequest, error) {
	key := strconv.FormatInt(repoID, 10) + "/" + sc
	if p, ok := st.pulls[key]; ok {
		return p, nil
	}
	parsed, err := scope.Parse(sc)
	if err != nil {
		return nil, err
	}
	n, _ := parsed.PullRequestNumber()
	repo, err := s.repository(ctx, st, repoID)
	if err != nil || repo == nil {
		return nil, err
	}
	pr, err := s.GitHub.PullRequest(ctx, *repo, n)
	if errors.Is(err, github.ErrNotFound) {
		st.pulls[key] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st.pulls[key] = &pr
	return &pr, nil
}

// finishedJob reports whether GitHub shows a job finished.
func (s *Service) finishedJob(ctx context.Context, st *state, repoID, jobID int64) (bool, error) {
	j, ok := st.jobs[jobID]
	if !ok {
		repo, err := s.repository(ctx, st, repoID)
		if err != nil {
			return false, err
		}
		if repo != nil {
			got, err := s.GitHub.Job(ctx, *repo, jobID)
			switch {
			case errors.Is(err, github.ErrNotFound):
			case err != nil:
				return false, err
			default:
				j = &got
			}
		}
		st.jobs[jobID] = j
	}
	return j == nil || j.Status == "completed", nil
}

// expire marks and deletes what no longer needs keeping, as early as the
// rules allow (specs/0003-disks.md, Expiry): older versions of a saved disk
// once a newer one is ready; in a pull request's scope, its other disks of
// a family once it saves one, all of them once it has closed, and any
// unused for 14 days; in main's and each long-lived branch's, every key of
// a family but the two newest and any an open pull request's most recent
// choice names, all of a family unused for 90 days, and all of a branch's
// once it's no longer listed or no longer exists; and resume images once
// their job has started from one, or has finished, or their run has
// finished with no rerun left.
// Marking leaves alone any image an unfinished run's choice names, and a
// resume image a job has claimed and not yet started from.
func (s *Service) expire(ctx context.Context, st *state) {
	byRepo := map[int64]map[string]record[Image]{}
	for _, r := range st.records {
		if byRepo[r.v.RepositoryID] == nil {
			byRepo[r.v.RepositoryID] = map[string]record[Image]{}
		}
		byRepo[r.v.RepositoryID][r.v.Image] = r
	}
	// A consumer with pull requests' recent choices and no images still has
	// its closed pull requests' choices dropped, and one with closures
	// still has them tidied.
	for _, r := range st.recents {
		if byRepo[r.v.RepositoryID] == nil {
			byRepo[r.v.RepositoryID] = map[string]record[Image]{}
		}
	}
	for _, r := range st.closures {
		if byRepo[r.v.RepositoryID] == nil {
			byRepo[r.v.RepositoryID] = map[string]record[Image]{}
		}
	}
	repos := make([]int64, 0, len(byRepo))
	for id := range byRepo {
		repos = append(repos, id)
	}
	slices.Sort(repos)
	// A resume image a job has started from: its work disk is made.
	started := map[string]int64{}
	for _, r := range st.workDisks {
		d := r.v
		if d.SourceImage != "" && (d.State == machines.DiskMade || d.State == machines.DiskAttached) {
			started[d.SourceImage] = d.JobID
		}
	}
	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		recs := byRepo[repo]
		// Which version is newest, of a series or of a main family, is
		// read only from images Compute Engine has, ready: a record of
		// one that's gone mustn't stand in for it, and leave the one
		// that's there unprotected.
		there := map[string]record[Image]{}
		for n, r := range recs {
			if im, ok := st.images[r.v.Image]; ok && im.Status == "READY" && matches(im, r.v) {
				there[n] = r
			}
		}
		finished := map[int64]bool{}
		var due []record[Image]
		why := map[string]string{}
		// The disk that replaces a pull request's, by which a pass decided
		// it goes: that it isn't itself retired is read again under the
		// lease, since a closing may be recorded meanwhile.
		replacedBy := map[string]Image{}
		add := func(image, reason string) {
			if _, ok := why[image]; !ok {
				due = append(due, recs[image])
				why[image] = reason
			}
		}
		// Every pull request whose disks or most recent choices this
		// consumer holds is read once; one seen closed is recorded as
		// closed before anything else, so that, reopened, it never chooses
		// what its earlier runs saved.
		closed := map[string]bool{}
		scopes := map[string]bool{}
		for _, r := range there {
			if _, pr := isPullRequest(r.v.Scope); pr && r.v.Kind == KindSaved {
				scopes[r.v.Scope] = true
			}
		}
		for _, r := range st.recents {
			if r.v.RepositoryID == repo {
				scopes[r.v.Scope] = true
			}
		}
		unknown := map[string]bool{}
		for _, sc := range slices.Sorted(maps.Keys(scopes)) {
			c, err := s.seenClosed(ctx, st, repo, sc)
			if err != nil {
				s.log().Warn("reading a scope's pull request", "scope", sc, "err", err.Error())
				unknown[sc] = true
				continue
			}
			closed[sc] = c
		}
		closure := func(sc string) *Closure {
			if c, ok := st.closures[closureKey(repo, sc)]; ok {
				return &c.v
			}
			return nil
		}
		delisted := s.delisted(ctx, st, repo, there)
		latest, lineage := map[string]Image{}, map[string]Image{}
		mainKeys := map[string][]Image{}
		mainUsed := map[string]time.Time{}
		for _, r := range st.families {
			if f := r.v; f.RepositoryID == repo {
				mainUsed[f.Scope+"\x00"+f.Arch+"\x00"+f.Family] = f.LastUsed
			}
		}
		for _, r := range there {
			v := r.v
			if v.Kind != KindSaved || v.State != ImageReady {
				continue
			}
			// A retired disk of a pull request's replaces nothing: it goes
			// itself.
			if retired(v, closure(v.Scope)) {
				continue
			}
			k := Series(v.Scope, v.Arch, v.Family, v.Key)
			if b, ok := latest[k]; !ok || olderThan(b, v) {
				latest[k] = v
			}
			f := v.Scope + "\x00" + v.Arch + "\x00" + v.Family
			if b, ok := lineage[f]; !ok || olderThan(b, v) {
				lineage[f] = v
			}
			if lasting(v.Scope) && v.LastUsed.After(mainUsed[f]) {
				mainUsed[f] = v.LastUsed
			}
		}
		for _, v := range latest {
			if lasting(v.Scope) {
				f := v.Scope + "\x00" + v.Arch + "\x00" + v.Family
				mainKeys[f] = append(mainKeys[f], v)
			}
		}
		// Main keeps its two newest keys of each family, by their newest
		// versions.
		keep := map[string]bool{}
		for _, vs := range mainKeys {
			slices.SortFunc(vs, func(x, y Image) int {
				if olderThan(y, x) {
					return -1
				}
				return 1
			})
			for _, v := range vs[:min(MainKeys, len(vs))] {
				keep[Series(v.Scope, v.Arch, v.Family, v.Key)] = true
			}
		}
		// And any key an open pull request's most recent choice names. A
		// closed pull request's recent choices go; one whose state can't
		// be read keeps what it names.
		for key, r := range st.recents {
			c := r.v
			if c.RepositoryID != repo {
				continue
			}
			if closed[c.Scope] {
				if err := s.Objects.Delete(ctx, key, r.gen); err == nil || errors.Is(err, store.ErrNotFound) {
					delete(st.recents, key)
				}
				continue
			}
			if lasting(c.ImageScope) {
				keep[Series(c.ImageScope, c.Arch, c.Family, c.Key)] = true
			}
		}
		for _, r := range there {
			v := r.v
			if v.State != ImageReady {
				continue
			}
			switch v.Kind {
			case KindSaved:
				if l, ok := latest[Series(v.Scope, v.Arch, v.Family, v.Key)]; ok && l.Image != v.Image {
					add(v.Image, "superseded by "+l.Image)
					replacedBy[v.Image] = l
					continue
				}
				f := v.Scope + "\x00" + v.Arch + "\x00" + v.Family
				if delisted[v.Scope] {
					add(v.Image, "its long-lived branch is no longer listed, or no longer exists")
					continue
				}
				if lasting(v.Scope) {
					switch {
					case s.now().Sub(mainUsed[f]) >= MainUnused:
						add(v.Image, familyUnused)
					case !keep[Series(v.Scope, v.Arch, v.Family, v.Key)]:
						add(v.Image, "not among its family's two newest keys, nor named by an open pull request's most recent choice")
					}
					continue
				}
				if closed[v.Scope] {
					add(v.Image, "its pull request closed")
					continue
				}
				if retired(v, closure(v.Scope)) {
					add(v.Image, "saved by a run from before its pull request closed")
					continue
				}
				if l, ok := lineage[f]; ok && l.Image != v.Image {
					add(v.Image, "its pull request saved "+l.Image+" of the family since")
					replacedBy[v.Image] = l
					continue
				}
				if s.now().Sub(v.LastUsed) >= PullRequestUnused {
					add(v.Image, "unused for 14 days")
				}
			case KindResume:
				if c, claimed := st.claims[claimKey(repo, v.Image)]; claimed {
					if started[v.Image] == c.v.JobID {
						finished[c.v.JobID] = true
						add(v.Image, "its job has started from it")
						continue
					}
					done, err := s.finishedJob(ctx, st, repo, c.v.JobID)
					if err != nil {
						s.log().Warn("reading a resume image's job", "image", v.Image, "err", err.Error())
						continue
					}
					if done {
						finished[c.v.JobID] = true
						add(v.Image, "its job has finished")
						continue
					}
				}
				ok, err := s.runSettled(ctx, st, repo, v.RunID)
				if err != nil {
					s.log().Warn("reading a resume image's run", "image", v.Image, "err", err.Error())
					continue
				}
				if ok {
					if c, claimed := st.claims[claimKey(repo, v.Image)]; claimed {
						finished[c.v.JobID] = true
					}
					add(v.Image, "its run has finished with no rerun left")
				}
			}
		}
		var marked []Image
		if len(due) > 0 {
			slices.SortFunc(due, func(x, y record[Image]) int { return strings.Compare(x.v.Image, y.v.Image) })
			var err error
			// A family's retirement rests on its other versions' use too,
			// which a choice of one of them marks in the family's record:
			// that's read again under the lease.
			recheck := func(ctx context.Context, v Image) (bool, error) {
				if by, ok := replacedBy[v.Image]; ok {
					if _, pr := isPullRequest(by.Scope); !pr {
						return true, nil
					}
					c, _, found, err := getJSON[Closure](ctx, s.Objects, closureKey(repo, by.Scope))
					if err != nil || !found {
						return err == nil, err
					}
					return !retired(by, &c), nil
				}
				if why[v.Image] != familyUnused {
					return true, nil
				}
				f, _, found, err := getJSON[Family](ctx, s.Objects, familyKey(repo, v.Scope, v.Arch, v.Family))
				if err != nil || !found {
					return err == nil, err
				}
				return s.now().Sub(f.LastUsed) >= MainUnused, nil
			}
			if marked, err = s.mark(ctx, repo, due, finished, recheck); err != nil {
				s.log().Warn("marking images for deletion", "repository_id", repo, "err", err.Error())
			}
		}
		for _, v := range marked {
			recs[v.Image] = record[Image]{v: v}
			s.log().Info("image deleted", "image", v.Image, "repository_id", repo, "reason", why[v.Image])
		}
		// What's marked goes, whichever pass marked it.
		for _, r := range recs {
			if r.v.State == ImageMarked {
				if err := s.deleteMarked(ctx, r.v); err != nil {
					s.log().Warn("deleting a marked image", "image", r.v.Image, "err", err.Error())
				}
			}
		}
		s.tidyScopes(ctx, st, repo, unknown)
	}
}

// delisted are the long-lived branches' scopes, of those a consumer's
// images hold, whose branches are no longer listed in the default branch's
// .github/runners.json, or no longer exist, by GitHub's confirmed answers.
// What GitHub can't confirm is kept.
func (s *Service) delisted(ctx context.Context, st *state, repo int64, there map[string]record[Image]) map[string]bool {
	scopes := map[string]bool{}
	for _, r := range there {
		if r.v.Kind == KindSaved && lasting(r.v.Scope) && r.v.Scope != "main" {
			scopes[r.v.Scope] = true
		}
	}
	if len(scopes) == 0 {
		return nil
	}
	rp, err := s.repository(ctx, st, repo)
	if err != nil {
		s.log().Warn("reading a consumer with long-lived branches", "repository_id", repo, "err", err.Error())
		return nil
	}
	out, err := Delisted(ctx, s.GitHub, rp, s.log(), slices.Sorted(maps.Keys(scopes)))
	if err != nil {
		s.log().Warn("reading a consumer's long-lived branches", "repository_id", repo, "err", err.Error())
	}
	return out
}

// Delisted reports which of a consumer's long-lived branches' scopes have
// lost their branch: no longer listed in the default branch's
// .github/runners.json, or no longer there. A repository that's gone, or
// no longer the organization's, delists nothing: nothing of it is decided.
// Scopes GitHub couldn't confirm are left out, with the error.
func Delisted(ctx context.Context, gh github.API, repo *github.Repository, log *slog.Logger, scopes []string) (map[string]bool, error) {
	out := map[string]bool{}
	if repo == nil {
		return out, nil
	}
	// A file that isn't a JSON object lists nothing, as it binds nothing;
	// any other failure to read it confirms nothing.
	f, err := binding.ReadRunners(ctx, gh, *repo, log)
	if err != nil && !errors.Is(err, binding.ErrRunnersFile) {
		return out, err
	}
	byName := map[string]string{}
	for branch, name := range f.Branches {
		byName["branch-"+name] = branch
	}
	var errs []error
	for _, sc := range scopes {
		branch, ok := byName[sc]
		if !ok {
			out[sc] = true
			continue
		}
		_, err := gh.BranchHead(ctx, *repo, branch)
		switch {
		case errors.Is(err, github.ErrNotFound):
			out[sc] = true
		case err != nil:
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

// seenClosed reports whether a pull request scope's pull request is closed,
// or gone, by GitHub's record read once a pass, and if it is, records that
// it was seen closed now.
func (s *Service) seenClosed(ctx context.Context, st *state, repo int64, sc string) (bool, error) {
	pr, err := s.pull(ctx, st, repo, sc)
	if err != nil {
		return false, err
	}
	if pr != nil && pr.State != "closed" {
		return false, nil
	}
	var closedAt time.Time
	if pr != nil {
		closedAt = pr.ClosedAt.UTC()
	}
	rec, err := s.closeScope(ctx, repo, sc, s.now(), closedAt)
	if err != nil {
		return true, err
	}
	st.closures[closureKey(repo, sc)] = record[Closure]{v: rec}
	return true, nil
}

// tidyScopes drops a consumer's family records once the family has no
// images left, and its closures once the scope has none and no run bound
// before the closure can still be going on.
func (s *Service) tidyScopes(ctx context.Context, st *state, repo int64, unknown map[string]bool) {
	have := map[string]bool{}
	for _, r := range st.records {
		v := r.v
		if v.RepositoryID == repo && v.Kind == KindSaved {
			have[v.Scope] = true
			have[v.Scope+"\x00"+v.Arch+"\x00"+v.Family] = true
		}
	}
	for key, r := range st.families {
		f := r.v
		if f.RepositoryID == repo && !have[f.Scope+"\x00"+f.Arch+"\x00"+f.Family] {
			_ = s.Objects.Delete(ctx, key, r.gen)
		}
	}
	for key, r := range st.closures {
		c := r.v
		if c.RepositoryID == repo && !have[c.Scope] && !unknown[c.Scope] && s.now().Sub(c.SeenAt) >= RunLimit {
			if err := s.Objects.Delete(ctx, key, r.gen); err == nil {
				delete(st.closures, key)
			}
		}
	}
}

// familyUnused is why every disk of a retired family of main's goes.
const familyUnused = "its family unused for 90 days"

// runSettled reports whether a run has finished with no automatic rerun it
// is, or may yet be, due, counting a run with an admitted machine that
// hasn't reported as unfinished.
func (s *Service) runSettled(ctx context.Context, st *state, repo, runID int64) (bool, error) {
	if st.live == nil {
		live, err := s.unreported(ctx, "")
		if err != nil {
			return false, err
		}
		st.live = live
	}
	if st.live[runID] {
		return false, nil
	}
	rs, err := s.runState(ctx, st, repo, runID)
	if err != nil {
		return false, err
	}
	return rs.finished && !rs.due, nil
}

// tidy deletes images of the service's names that no record describes,
// records of images that are gone, claims of images that are gone, and
// preemption, event and rerun records once nothing needs them.
func (s *Service) tidy(ctx context.Context, st *state) {
	recorded := map[string]bool{}
	for key, r := range st.records {
		v := r.v
		recorded[v.Image] = true
		// An image whose making was abandoned, its machine gone, never
		// becomes ready: it goes, image and record, and stops counting
		// against its scope's limit.
		if v.State == ImageMaking && st.imagesRead {
			if _, err := s.Compute.Instance(ctx, v.Zone, v.Machine); errors.Is(err, compute.ErrNotFound) {
				if v.GoneAt.IsZero() {
					// The hour counts from when its machine is first
					// seen gone.
					v.GoneAt = s.now()
					if g, err := replaceJSON(ctx, s.Objects, key, v, r.gen); err == nil {
						st.records[key] = record[Image]{v, g}
					}
					continue
				}
				if s.now().Sub(v.GoneAt) < abandoned {
					continue
				}
				if err := s.Compute.DeleteImage(ctx, v.Image, freshID("delete image", v.Image)); err != nil && !errors.Is(err, compute.ErrNotFound) {
					s.log().Warn("deleting an abandoned image", "image", v.Image, "err", err.Error())
					continue
				}
				if err := s.Objects.Delete(ctx, key, r.gen); err == nil {
					delete(st.records, key)
					delete(st.images, v.Image)
					s.log().Info("abandoned image removed", "image", v.Image)
				}
				continue
			}
		}
		if _, there := st.images[v.Image]; there || !st.imagesRead || v.State == ImageMarked {
			continue
		}
		// Not listed: gone, or made since the list was read.
		switch v.State {
		case ImageReady:
		case ImageMaking:
			if s.now().Sub(v.SavedAt) < abandoned {
				continue
			}
			if _, err := s.Compute.Instance(ctx, v.Zone, v.Machine); !errors.Is(err, compute.ErrNotFound) {
				continue
			}
		}
		if _, err := s.Compute.Image(ctx, v.Image); !errors.Is(err, compute.ErrNotFound) {
			continue
		}
		if err := s.Objects.Delete(ctx, key, r.gen); err == nil {
			delete(st.records, key)
			s.log().Info("record of an image that's gone removed", "image", v.Image, "state", v.State)
		}
	}
	// An image of the service's names that no record describes goes like
	// any other: under the disks' lease, never while a choice or claim
	// names it, recorded as marked before it's deleted.
	orphans := map[int64][]string{}
	for name, im := range st.images {
		if !recorded[name] {
			repo, _ := ours(im)
			orphans[repo] = append(orphans[repo], name)
		}
	}
	for repo, names := range orphans {
		slices.Sort(names)
		// A claim keeps an orphaned resume image only while it would keep
		// one with a record: until its job has started from it, or has
		// finished.
		expired := map[int64]bool{}
		for _, name := range names {
			c, claimed := st.claims[claimKey(repo, name)]
			if !claimed || !resumeName.MatchString(name) {
				continue
			}
			startedFrom := false
			for _, d := range st.workDisks {
				if d.v.SourceImage == name && d.v.JobID == c.v.JobID && (d.v.State == machines.DiskMade || d.v.State == machines.DiskAttached) {
					startedFrom = true
				}
			}
			done, err := s.finishedJob(ctx, st, repo, c.v.JobID)
			if startedFrom || (err == nil && done) {
				expired[c.v.JobID] = true
			}
		}
		var marked []Image
		err := s.step(ctx, repo, func(ctx context.Context) error {
			named, err := s.named(ctx, repo, expired)
			if err != nil {
				return err
			}
			for _, name := range names {
				if named[name] {
					s.log().Warn("an image with no record is named by a choice or claim; it's kept", "image", name)
					continue
				}
				kind := KindSaved
				if resumeName.MatchString(name) {
					kind = KindResume
				}
				rec := Image{Image: name, RepositoryID: repo, Kind: kind, State: ImageMarked, MarkedAt: s.now()}
				got, _, err := createOnce(ctx, s.Objects, imageKey(repo, name), rec)
				if err != nil {
					return err
				}
				if got.State == ImageMarked {
					marked = append(marked, got)
				}
			}
			return nil
		})
		if err != nil {
			s.log().Warn("marking orphaned images", "repository_id", repo, "err", err.Error())
		}
		for _, v := range marked {
			if err := s.deleteMarked(ctx, v); err != nil {
				s.log().Warn("deleting an orphaned image", "image", v.Image, "err", err.Error())
				continue
			}
			s.log().Info("orphaned image deleted", "image", v.Image)
		}
	}
	present := map[string]bool{}
	for _, r := range st.records {
		present[imageKey(r.v.RepositoryID, r.v.Image)] = true
	}
	// A claim goes once its image is gone from both the bucket and
	// Compute Engine: one that keeps an orphaned image stays with it.
	for key, r := range st.claims {
		if _, there := st.images[r.v.Image]; !present[imageKey(r.v.RepositoryID, r.v.Image)] && !there && st.imagesRead {
			_ = s.Objects.Delete(ctx, key, r.gen)
		}
	}
	runsLeft := map[string]bool{}
	for key, r := range st.preemptions {
		p := r.v
		if s.now().Sub(p.StoppedAt) < ResumeKept || (p.ResumeImage != "" && present[imageKey(p.RepositoryID, p.ResumeImage)]) {
			runsLeft[runKey(p.RepositoryID, p.RunID)] = true
			continue
		}
		rs, err := s.runState(ctx, st, p.RepositoryID, p.RunID)
		if err != nil || !rs.finished || rs.due {
			runsLeft[runKey(p.RepositoryID, p.RunID)] = true
			continue
		}
		if err := s.Objects.Delete(ctx, key, r.gen); err != nil {
			runsLeft[runKey(p.RepositoryID, p.RunID)] = true
		}
	}
	for key, r := range st.reruns {
		if !runsLeft[runKey(r.v.RepositoryID, r.v.RunID)] {
			_ = s.Objects.Delete(ctx, key, r.gen)
		}
	}
	for key, r := range st.events {
		if s.now().Sub(r.v.RecordedAt) >= eventsKept {
			_ = s.Objects.Delete(ctx, key, r.gen)
		}
	}
}
