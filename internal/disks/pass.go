package disks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/consumer"
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
	closures    map[string]record[Closure]
	workDisks   map[string]record[machines.DiskRecord]
	choices     map[string]record[Choice]
	claims      map[string]record[Claim]
	preemptions map[string]record[Preemption]
	events      map[string]record[Event]
	reruns      map[string]record[Rerun]
	complete    bool

	repos map[int64]*github.Repository
	// unconfirmed are the repositories GitHub didn't show as the
	// organization's when last read in the pass, with why: GitHub's 404,
	// another owner, or a failure to read it. For the rest of the pass,
	// none of their images is marked, and nothing of their runs decided.
	unconfirmed map[int64]error
	runs        map[string]*run
	pulls       map[string]*github.PullRequest
	jobs        map[int64]*github.Job
	preempted   map[string][]uint64
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
// were preempted, reruns runs that lost a job to a preemption or to the age
// limit once they've finished, drops the restore choices of runs that need them no
// more, and deletes saved disks that are replaced or no longer declared,
// resume images nothing can use, and images that are orphaned. A failure on
// one image or run doesn't stop the rest.
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
	st := &state{images: map[string]compute.Image{}, complete: true, repos: map[int64]*github.Repository{}, unconfirmed: map[int64]error{},
		runs:  map[string]*run{},
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

// errNotShown is the answer for a repository that GitHub doesn't show as
// the organization's: it answers 404 for it, as it does for a deleted
// repository and for one the App can no longer read, or it shows it as
// another owner's, transferred. Such a repository confirms nothing, as
// GitHub being unavailable doesn't: none of its images goes, none of its
// pull requests counts as closed, and nothing of its runs is decided or
// rerun, until GitHub shows it as the organization's again
// (specs/0003-disks.md, Deletion).
var errNotShown = errors.New("GitHub doesn't show the repository as the organization's")

// repository is a consumer's repository, read once a pass. A repository
// GitHub doesn't show as the organization's gives errNotShown, and a
// warning naming it and why. Once a read of it fails, every later one in
// the pass gives the same error, even after one succeeded before.
func (s *Service) repository(ctx context.Context, st *state, id int64) (*github.Repository, error) {
	if err := st.unconfirmed[id]; err != nil {
		return nil, err
	}
	if r, ok := st.repos[id]; ok {
		return r, nil
	}
	r, err := s.GitHub.RepositoryByID(ctx, id)
	switch {
	case errors.Is(err, github.ErrNotFound):
		err = s.notShown(id, "GitHub answers 404 for it: it's deleted, or the App can't read it")
	case err != nil:
	case r.OwnerID != s.Config.Organization.ID:
		err = s.notShown(id, "GitHub shows it as another owner's: it's been transferred")
	default:
		st.repos[id] = &r
		return &r, nil
	}
	delete(st.repos, id)
	if st.unconfirmed == nil {
		st.unconfirmed = map[int64]error{}
	}
	st.unconfirmed[id] = err
	return nil, err
}

// notShown is errNotShown for a repository, and why, which it logs.
func (s *Service) notShown(id int64, why string) error {
	s.log().Warn("a repository with records isn't the organization's by GitHub's answer", "repository_id", id, "why", why)
	return fmt.Errorf("repository %d: %w: %s", id, errNotShown, why)
}

// confirmed is a 404 for something of a repository's, such as a run, a pull
// request, a branch or a file, once GitHub, asked again, still shows the
// repository as the organization's; otherwise it's what that read gave
// instead. GitHub answers 404 for everything of a repository the App can no
// longer read, so a 404 alone confirms nothing's absence.
func (s *Service) confirmed(ctx context.Context, st *state, id int64, err error) error {
	if !errors.Is(err, github.ErrNotFound) {
		return err
	}
	delete(st.repos, id)
	if _, rerr := s.repository(ctx, st, id); rerr != nil {
		return rerr
	}
	return err
}

// stillShown reads a repository again just before a pass acts on what it
// decided of it, and reports whether GitHub still shows it as the
// organization's: a read since that GitHub answered with 404 because the
// App lost access, confirmed or not, then shows here.
func (s *Service) stillShown(ctx context.Context, st *state, id int64) bool {
	if st.unconfirmed[id] != nil {
		return false
	}
	delete(st.repos, id)
	_, err := s.repository(ctx, st, id)
	return err == nil
}

// evidence looks, by instance ID, for evidence that each stopped Spot
// machine recorded as perhaps preempted was: Compute Engine's recorded
// event, or a zone operation of type compute.instances.preempted, for a
// day after it stopped. A machine stopped at its age limit needs none: its
// job was lost either way.
func (s *Service) evidence(ctx context.Context, st *state) {
	for key, r := range st.preemptions {
		p := r.v
		if p.Preempted != Unknown || p.AgeLimit || s.now().Sub(p.StoppedAt) >= ResumeKept {
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
// job shown preempted, or a job lost at the age limit; or, while a day
// hasn't passed, perhaps preempted. It's rerun only once every failed job's
// record is done, its resume image ready if it gets one, or a day has
// passed since its machine stopped, and no machine of the run that's
// admitted is still unreported. An
// error means GitHub gave no confirmed answer.
func (s *Service) runState(ctx context.Context, st *state, repoID, runID int64) (*run, error) {
	if err := st.unconfirmed[repoID]; err != nil {
		return nil, err
	}
	key := runKey(repoID, runID)
	if r, ok := st.runs[key]; ok {
		return r, nil
	}
	repo, err := s.repository(ctx, st, repoID)
	if err != nil {
		return nil, err
	}
	r := &run{finished: true}
	wr, err := s.GitHub.WorkflowRun(ctx, *repo, runID)
	err = s.confirmed(ctx, st, repoID, err)
	switch {
	case errors.Is(err, github.ErrNotFound):
	case err != nil:
		return nil, err
	default:
		r.found, r.finished, r.attempt = true, wr.Status == "completed", wr.Attempt
	}
	if r.found && r.finished {
		jobs, err := s.GitHub.RunJobs(ctx, *repo, runID)
		if err != nil {
			return nil, s.confirmed(ctx, st, repoID, err)
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
			// unready says some failed job's record isn't done: its resume
			// image isn't ready yet, and the rerun, which reruns every
			// failed job, waits for it, or else that job would start
			// without it.
			unready := false
			for _, p := range st.preemptions {
				v := p.v
				if v.RepositoryID != repoID || v.RunID != runID || v.RunAttempt != r.attempt || !r.failed[v.JobID] {
					continue
				}
				if !v.Done && s.now().Sub(v.StoppedAt) < ResumeKept {
					unready = true
				}
				switch {
				case v.Preempted == Preempted || v.AgeLimit:
					r.due, r.rerun = true, true
				case v.Preempted == Unknown && s.now().Sub(v.StoppedAt) < ResumeKept:
					r.due = true
				}
			}
			if r.rerun && !unready {
				// A machine of the run that's admitted and hasn't reported,
				// such as one stopped whose record isn't written yet, may
				// have a job to record: the rerun waits for it too.
				if st.live == nil {
					live, err := s.unreported(ctx, "")
					if err != nil {
						return nil, err
					}
					st.live = live
				}
				unready = st.live[runID]
			}
			if unready {
				r.rerun = false
			}
		}
	}
	st.runs[key] = r
	return r, nil
}

// failedConclusion are the conclusions of the jobs that rerunning a run's
// failed jobs runs again.
var failedConclusion = map[string]bool{"failure": true, "cancelled": true, "timed_out": true, "startup_failure": true}

// follow reruns each finished run that lost a job to a preemption or to the
// age limit: its failed jobs, rather than each lost job and its dependents,
// so there's one rerun per run attempt. Each rerun is recorded, by the
// attempt it reran, before it's asked for.
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
			s.log().Warn("reading a run that lost a job", "run_id", p.RunID, "err", err.Error())
			continue
		}
		if !rs.rerun {
			continue
		}
		if err := s.rerun(ctx, st, p.RepositoryID, p.RunID, rs); err != nil {
			s.log().Warn("rerunning a run that lost a job", "run_id", p.RunID, "err", err.Error())
		}
	}
}

// rerun asks GitHub to rerun a run's failed jobs, under the disks' lease,
// which every reading of a restore choice takes.
func (s *Service) rerun(ctx context.Context, st *state, repoID, runID int64, rs *run) error {
	return s.step(ctx, repoID, func(ctx context.Context) error { return s.rerunLocked(ctx, st, repoID, runID, rs) })
}

func (s *Service) rerunLocked(ctx context.Context, st *state, repoID, runID int64, rs *run) error {
	repo, err := s.repository(ctx, st, repoID)
	if err != nil {
		return err
	}
	// What the pass read may have changed while it waited for the lease:
	// a maintainer may have rerun the run since.
	if wr, err := s.GitHub.WorkflowRun(ctx, *repo, runID); err != nil {
		return s.confirmed(ctx, st, repoID, err)
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
	err = s.confirmed(ctx, st, repoID, s.GitHub.RerunFailedJobs(ctx, *repo, runID))
	if err != nil && github.IsUnavailable(err) {
		// An answer lost after GitHub began the next attempt is a rerun
		// all the same.
		if wr, rerr := s.GitHub.WorkflowRun(ctx, *repo, runID); rerr == nil && wr.Attempt > rs.attempt {
			err = nil
		} else if rerr != nil {
			_ = s.confirmed(ctx, st, repoID, rerr)
		}
	}
	if err != nil {
		return err
	}
	rs.finished, rs.rerun = false, false
	s.log().Info("run rerun: a job was lost to a preemption or to the age limit", "repository_id", repoID, "run_id", runID, "attempt", rs.attempt+1)
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
				if ok, err := s.settled(ctx, st, repo, r.v.RunID, live); err != nil || !ok {
					continue
				}
				// GitHub must still show the repository as the
				// organization's once the run has been read: it's read
				// again before its choice is dropped.
				if !s.stillShown(ctx, st, repo) {
					return nil
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
	if err := st.unconfirmed[repoID]; err != nil {
		return nil, err
	}
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
	if err != nil {
		return nil, err
	}
	pr, err := s.GitHub.PullRequest(ctx, *repo, n)
	err = s.confirmed(ctx, st, repoID, err)
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
	if err := st.unconfirmed[repoID]; err != nil {
		return false, err
	}
	j, ok := st.jobs[jobID]
	if !ok {
		repo, err := s.repository(ctx, st, repoID)
		if err != nil {
			return false, err
		}
		got, err := s.GitHub.Job(ctx, *repo, jobID)
		err = s.confirmed(ctx, st, repoID, err)
		switch {
		case errors.Is(err, github.ErrNotFound):
		case err != nil:
			return false, err
		default:
			j = &got
		}
		st.jobs[jobID] = j
	}
	return j == nil || j.Status == "completed", nil
}

// expire marks and deletes what its rules say goes (specs/0003-disks.md,
// Deletion): in main's scope and each long-lived branch's, every disk of a
// family the scope's branch no longer declares, and all of a branch's once
// it's no longer listed or no longer exists; in every scope, its older
// disks of a family on an architecture once a newer one is ready, whatever
// their key; in a pull request's, all of it once it has closed; and resume
// images once their job has started from one, or has finished, or their
// run has finished with no rerun left. Nothing goes for want of use or for
// its age, nor while GitHub doesn't show its repository as the
// organization's. Marking leaves alone any image an unfinished run's choice
// names, and a resume image a job has claimed and not yet started from.
func (s *Service) expire(ctx context.Context, st *state) {
	byRepo := map[int64]map[string]record[Image]{}
	for _, r := range st.records {
		if byRepo[r.v.RepositoryID] == nil {
			byRepo[r.v.RepositoryID] = map[string]record[Image]{}
		}
		byRepo[r.v.RepositoryID][r.v.Image] = r
	}
	// A consumer with closures and no images still has them tidied.
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
		// None of a repository's images goes by these rules unless GitHub
		// shows the repository as the organization's this pass: one it
		// answers 404 for, or shows as another owner's, confirms nothing
		// until GitHub shows it again, as one it can't read now doesn't.
		// What a pass marked before still goes.
		_, err := s.repository(ctx, st, repo)
		shown := err == nil
		if err != nil && !errors.Is(err, errNotShown) {
			s.log().Warn("reading a consumer with images; deleting none of them this pass", "repository_id", repo, "err", err.Error())
		}
		// Which disk of a scope's family is newest is read only from images
		// Compute Engine has, ready: a record of one that's gone mustn't
		// stand in for it, and leave the one that's there unprotected.
		there := map[string]record[Image]{}
		for n, r := range recs {
			if im, ok := st.images[r.v.Image]; shown && ok && im.Status == "READY" && matches(im, r.v) {
				there[n] = r
			}
		}
		finished := map[int64]bool{}
		var due []record[Image]
		why := map[string]string{}
		// The disk that replaces another, by which a pass decided the other
		// goes: that it isn't itself retired is read again under the lease,
		// since a closing may be recorded meanwhile.
		replacedBy := map[string]Image{}
		add := func(image, reason string) {
			if _, ok := why[image]; !ok {
				due = append(due, recs[image])
				why[image] = reason
			}
		}
		// Every pull request whose disks this consumer holds is read once;
		// one seen closed is recorded as closed before anything else, so
		// that, reopened, it never chooses what its earlier runs saved.
		closed := map[string]bool{}
		scopes := map[string]bool{}
		for _, r := range there {
			if _, pr := isPullRequest(r.v.Scope); pr && r.v.Kind == KindSaved {
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
		decl := s.declarations(ctx, st, repo, there)
		// Each scope keeps one disk of each family and architecture: its
		// newest save, whatever its key.
		lineage := map[string]Image{}
		for _, r := range there {
			v := r.v
			// A retired disk of a pull request's replaces nothing: it goes
			// itself.
			if v.Kind != KindSaved || v.State != ImageReady || retired(v, closure(v.Scope)) {
				continue
			}
			f := v.Scope + "\x00" + v.Arch + "\x00" + v.Family
			if b, ok := lineage[f]; !ok || olderThan(b, v) {
				lineage[f] = v
			}
		}
		for _, r := range there {
			v := r.v
			if v.State != ImageReady {
				continue
			}
			switch v.Kind {
			case KindSaved:
				// What a scope's branch declares, where GitHub confirmed it:
				// a scope it couldn't confirm loses nothing by it.
				if d, ok := decl[v.Scope]; ok {
					if d.gone != "" {
						add(v.Image, d.gone)
						continue
					}
					if !d.families[v.Family] {
						add(v.Image, "its family is no longer declared in "+consumer.RunnersFile+" at its scope's branch's head")
						continue
					}
				}
				if closed[v.Scope] {
					add(v.Image, "its pull request closed")
					continue
				}
				if retired(v, closure(v.Scope)) {
					add(v.Image, "saved by a run from before its pull request closed")
					continue
				}
				if l, ok := lineage[v.Scope+"\x00"+v.Arch+"\x00"+v.Family]; ok && l.Image != v.Image {
					add(v.Image, "its scope saved "+l.Image+" of the family since")
					replacedBy[v.Image] = l
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
			// GitHub must still show the repository as the organization's
			// when its images are marked, under the lease: it's read again
			// once, before the first.
			checked := false
			recheck := func(ctx context.Context, v Image) (bool, error) {
				if !checked {
					checked = true
					s.stillShown(ctx, st, repo)
				}
				if st.unconfirmed[repo] != nil {
					return false, nil
				}
				by, ok := replacedBy[v.Image]
				if !ok {
					return true, nil
				}
				if _, pr := isPullRequest(by.Scope); !pr {
					return true, nil
				}
				c, _, found, err := getJSON[Closure](ctx, s.Objects, closureKey(repo, by.Scope))
				if err != nil || !found {
					return err == nil, err
				}
				return !retired(by, &c), nil
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

// declared is what a pass confirmed of a scope of main's or a long-lived
// branch's: that all of it goes, and why, or else the families its
// branch's .github/runners.json declares at its head.
type declared struct {
	gone     string
	families map[string]bool
}

// declarations reads what keeps the disks of each of main's and long-lived
// branches' scopes that a consumer's images hold, by GitHub's confirmed
// answers, once a pass. A scope left out is one GitHub couldn't confirm,
// which loses nothing by its declarations this pass.
func (s *Service) declarations(ctx context.Context, st *state, repo int64, there map[string]record[Image]) map[string]declared {
	scopes := map[string]bool{}
	for _, r := range there {
		if r.v.Kind == KindSaved && lasting(r.v.Scope) {
			scopes[r.v.Scope] = true
		}
	}
	if len(scopes) == 0 {
		return nil
	}
	rp, err := s.repository(ctx, st, repo)
	if err != nil {
		s.log().Warn("reading a consumer with saved disks; deleting nothing by its declarations this pass", "repository_id", repo, "err", err.Error())
		return nil
	}
	confirm := func(err error) error { return s.confirmed(ctx, st, repo, err) }
	out, err := readDeclarations(ctx, s.GitHub, s.log(), *rp, slices.Sorted(maps.Keys(scopes)), confirm)
	if err != nil {
		s.log().Warn("reading a consumer's declarations; deleting nothing by them where they couldn't be read", "repository_id", repo,
			"err", err.Error())
	}
	return out
}

// readDeclarations reads, for each of a consumer's scopes of main's and
// long-lived branches', what its branch's .github/runners.json declares at
// its head: the default branch's for main's, and each listed branch's own
// for its scope. A file that's absent declares nothing. A scope whose
// branch is no longer listed, or no longer exists, goes. What GitHub
// couldn't confirm, and a file that isn't valid, is left out, with the
// error: a default branch's file that isn't valid confirms nothing, for
// main's scope or any branch's. A 404 for a branch or a file counts only
// once confirm confirms it.
func readDeclarations(ctx context.Context, gh github.API, log *slog.Logger, repo github.Repository, scopes []string,
	confirm func(error) error) (map[string]declared, error) {
	out := map[string]declared{}
	tip, err := gh.BranchHead(ctx, repo, repo.DefaultBranch)
	if err = confirm(err); err != nil {
		return out, fmt.Errorf("reading the default branch: %w", err)
	}
	f, err := readRunnersFile(ctx, gh, repo, tip, confirm)
	if err != nil {
		return out, fmt.Errorf("the default branch's %s: %w", consumer.RunnersFile, err)
	}
	listed, problems := f.BranchScopes(repo.DefaultBranch)
	for _, p := range problems {
		log.Warn("an entry of the default branch's "+consumer.RunnersFile+" is ignored", "repository_id", repo.ID, "why", p)
	}
	byScope := map[string]string{}
	for branch, name := range listed {
		byScope["branch-"+name] = branch
	}
	var errs []error
	for _, sc := range scopes {
		if sc == "main" {
			out[sc] = declared{families: families(f)}
			continue
		}
		branch, ok := byScope[sc]
		if !ok {
			out[sc] = declared{gone: "its long-lived branch is no longer listed"}
			continue
		}
		head, err := gh.BranchHead(ctx, repo, branch)
		err = confirm(err)
		if errors.Is(err, github.ErrNotFound) {
			out[sc] = declared{gone: "its long-lived branch no longer exists"}
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("reading %s's branch: %w", sc, err))
			continue
		}
		bf, err := readRunnersFile(ctx, gh, repo, head, confirm)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s's branch's %s: %w", sc, consumer.RunnersFile, err))
			continue
		}
		out[sc] = declared{families: families(bf)}
	}
	return out, errors.Join(errs...)
}

// readRunnersFile reads and parses a consumer's .github/runners.json at a
// commit; one that's absent, by a 404 confirm confirms, declares nothing. A
// directory in its place confirms nothing, as a file that isn't valid
// doesn't.
func readRunnersFile(ctx context.Context, gh github.API, repo github.Repository, commit string, confirm func(error) error) (consumer.File, error) {
	data, err := gh.File(ctx, repo, consumer.RunnersFile, commit)
	if errors.Is(err, github.ErrNotFound) && !errors.Is(err, github.ErrDirectory) {
		err = confirm(err)
	}
	if errors.Is(err, github.ErrNotFound) && !errors.Is(err, github.ErrDirectory) {
		data = []byte("{}")
	} else if err != nil {
		return consumer.File{}, err
	}
	return consumer.Parse(data)
}

// families are the families a file declares.
func families(f consumer.File) map[string]bool {
	out := map[string]bool{}
	for family := range f.Disks {
		out[family] = true
	}
	return out
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

// tidyScopes drops a consumer's closures once the scope has no images and
// no run bound before the closure can still be going on.
func (s *Service) tidyScopes(ctx context.Context, st *state, repo int64, unknown map[string]bool) {
	if st.unconfirmed[repo] != nil {
		return
	}
	have := map[string]bool{}
	for _, r := range st.records {
		v := r.v
		if v.RepositoryID == repo && v.Kind == KindSaved {
			have[v.Scope] = true
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
		// Nor does any go unless GitHub shows its repository as the
		// organization's.
		if _, err := s.repository(ctx, st, repo); err != nil {
			continue
		}
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
			// GitHub must still show the repository as the organization's
			// when its images are marked, under the lease.
			if !s.stillShown(ctx, st, repo) {
				return nil
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
	var done []string
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
		done = append(done, key)
	}
	// Nothing of a run is let go unless GitHub still shows its repository
	// as the organization's once everything of its runs has been read: it's
	// read again then, once.
	shown := map[int64]bool{}
	shownNow := func(id int64) bool {
		if st.unconfirmed[id] != nil {
			return false
		}
		if v, ok := shown[id]; ok {
			return v
		}
		shown[id] = s.stillShown(ctx, st, id)
		return shown[id]
	}
	for _, key := range done {
		r := st.preemptions[key]
		p := r.v
		if !shownNow(p.RepositoryID) {
			runsLeft[runKey(p.RepositoryID, p.RunID)] = true
			continue
		}
		if err := s.Objects.Delete(ctx, key, r.gen); err != nil {
			runsLeft[runKey(p.RepositoryID, p.RunID)] = true
		}
	}
	for key, r := range st.reruns {
		if !runsLeft[runKey(r.v.RepositoryID, r.v.RunID)] {
			if !shownNow(r.v.RepositoryID) {
				continue
			}
			_ = s.Objects.Delete(ctx, key, r.gen)
		}
	}
	for key, r := range st.events {
		if s.now().Sub(r.v.RecordedAt) >= eventsKept {
			_ = s.Objects.Delete(ctx, key, r.gen)
		}
	}
}
