package disks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
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

func (s *Service) rerun(ctx context.Context, st *state, repoID, runID int64, rs *run) error {
	key := rerunKey(repoID, runID)
	rec, gen, err := createOnce(ctx, s.Objects, key, Rerun{RepositoryID: repoID, RunID: runID, At: s.now()})
	if err != nil {
		return err
	}
	// The rerun is recorded, as asked for and as the service's own,
	// before it's asked for, so a rerun GitHub begins is never one the
	// bucket doesn't know of; if GitHub's attempt hasn't moved on after
	// the asking, it's taken back as the service's, and a maintainer's
	// rerun after it picks its restore choices again.
	if !slices.Contains(rec.Attempts, rs.attempt) || !slices.Contains(rec.Started, rs.attempt) {
		if !slices.Contains(rec.Attempts, rs.attempt) {
			rec.Attempts = append(rec.Attempts, rs.attempt)
		}
		if !slices.Contains(rec.Started, rs.attempt) {
			rec.Started = append(rec.Started, rs.attempt)
		}
		rec.At = s.now()
		if gen, err = replaceJSON(ctx, s.Objects, key, rec, gen); err != nil {
			return err
		}
		st.reruns[key] = record[Rerun]{rec, gen}
	}
	repo := st.repos[repoID]
	err = s.GitHub.RerunFailedJobs(ctx, *repo, runID)
	wr, rerr := s.GitHub.WorkflowRun(ctx, *repo, runID)
	refused := err != nil && !github.IsUnavailable(err)
	switch {
	case !refused && rerr == nil && wr.Attempt > rs.attempt:
		// Begun, whatever the answer said: an answer lost after GitHub
		// began the next attempt is a rerun all the same. A confirmed
		// refusal never is, even if another attempt has begun, which
		// then is someone else's.
		err = nil
	case rerr == nil && err == nil:
		err = fmt.Errorf("GitHub accepted the rerun but still shows attempt %d", wr.Attempt)
		fallthrough
	case rerr == nil, refused:
		// GitHub's attempt didn't move on, or GitHub confirmed its
		// refusal: not a rerun of the service's.
		rec.Started = slices.DeleteFunc(rec.Started, func(a int) bool { return a == rs.attempt })
		if g, werr := replaceJSON(ctx, s.Objects, key, rec, gen); werr == nil {
			st.reruns[key] = record[Rerun]{rec, g}
		} else {
			s.log().Warn("taking back a rerun that didn't begin", "run_id", runID, "err", werr.Error())
		}
	case err == nil:
		// Accepted, and the run can't be read back: it's taken as begun.
	default:
		err = errors.Join(err, rerr)
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

// expire marks and deletes what's superseded or expired (specs/0003-disks.md,
// Expiry and limits): older versions of a saved disk once a newer one is
// ready; a pull request's disks 14 days after their last use, or once the
// pull request has been closed for a day; main's 90 days after their last
// use, except the newest of each family; and resume images a day after
// their machine stopped, or once the job that claimed one has finished,
// though never one claimed by a job that hasn't. Marking leaves alone any
// image an unfinished run's choice names.
func (s *Service) expire(ctx context.Context, st *state) {
	byRepo := map[int64]map[string]record[Image]{}
	for _, r := range st.records {
		if byRepo[r.v.RepositoryID] == nil {
			byRepo[r.v.RepositoryID] = map[string]record[Image]{}
		}
		byRepo[r.v.RepositoryID][r.v.Image] = r
	}
	repos := make([]int64, 0, len(byRepo))
	for id := range byRepo {
		repos = append(repos, id)
	}
	slices.Sort(repos)
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
		add := func(image, reason string) {
			if _, ok := why[image]; !ok {
				due = append(due, recs[image])
				why[image] = reason
			}
		}
		latest := map[string]Image{}
		for _, r := range there {
			v := r.v
			if v.Kind != KindSaved || v.State != ImageReady {
				continue
			}
			k := Series(v.Scope, v.Arch, v.Family, v.Key)
			if b, ok := latest[k]; !ok || v.SavedAt.After(b.SavedAt) {
				latest[k] = v
			}
		}
		top := newest(there)
		for _, r := range there {
			v := r.v
			if v.State != ImageReady {
				continue
			}
			switch v.Kind {
			case KindSaved:
				if l := latest[Series(v.Scope, v.Arch, v.Family, v.Key)]; l.Image != v.Image {
					add(v.Image, "superseded by "+l.Image)
					continue
				}
				unused := s.now().Sub(v.LastUsed)
				if v.Scope == "main" {
					if unused >= MainUnused && !top[v.Image] {
						add(v.Image, "unused for 90 days")
					}
					continue
				}
				if unused >= PullRequestUnused {
					add(v.Image, "unused for 14 days")
					continue
				}
				pr, err := s.pull(ctx, st, repo, v.Scope)
				if err != nil {
					s.log().Warn("reading a saved disk's pull request", "image", v.Image, "err", err.Error())
					continue
				}
				if pr == nil || (pr.State == "closed" && !pr.ClosedAt.IsZero() && s.now().Sub(pr.ClosedAt) >= PullRequestClosed) {
					add(v.Image, "its pull request closed a day ago")
				}
			case KindResume:
				c, claimed := st.claims[claimKey(repo, v.Image)]
				// A day after its machine stopped, it goes whatever its
				// record or claim says: a claimed job's disk is made by
				// then, or chooses again.
				if s.now().Sub(v.SavedAt) >= ResumeKept {
					if claimed {
						finished[c.v.JobID] = true
					}
					add(v.Image, "a day since its machine stopped")
					continue
				}
				if claimed {
					done, err := s.finishedJob(ctx, st, repo, c.v.JobID)
					if err != nil {
						s.log().Warn("reading a resume image's job", "image", v.Image, "err", err.Error())
						continue
					}
					if done {
						finished[c.v.JobID] = true
						add(v.Image, "its job has finished")
					}
				}
			}
		}
		var marked []Image
		if len(due) > 0 {
			slices.SortFunc(due, func(x, y record[Image]) int { return strings.Compare(x.v.Image, y.v.Image) })
			var err error
			if marked, err = s.mark(ctx, repo, due, finished); err != nil {
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
	}
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
		// one with a record: until its job has finished, and for a day
		// after its machine stopped, by its preemption record, or failing
		// that its claim.
		expired := map[int64]bool{}
		for _, name := range names {
			c, claimed := st.claims[claimKey(repo, name)]
			if !claimed || !resumeName.MatchString(name) {
				continue
			}
			stopped := c.v.At
			for _, p := range st.preemptions {
				if p.v.RepositoryID == repo && p.v.ResumeImage == name {
					stopped = p.v.StoppedAt
				}
			}
			done, err := s.finishedJob(ctx, st, repo, c.v.JobID)
			if (err == nil && done) || s.now().Sub(stopped) >= ResumeKept {
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
