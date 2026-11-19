package disks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/selector"
)

// Choose picks what a job's work disk starts from (specs/0002-machines.md,
// The work disk): its run's resume image for the job's name and
// architecture, if one is eligible and it can claim it, or nothing. m is
// resolved with the run's scope. It returns "" for an empty disk, the
// disk's size, the shape's or the image's if that's larger, and where it
// came from: machines.Resumed or machines.NotRestored. It implements
// machines.DiskSource.
func (s *Service) Choose(ctx context.Context, m machines.Machine, a machines.Admission, sizeGB int64) (string, int64, string, error) {
	im, ok, err := s.resume(ctx, m, a)
	if err != nil {
		return "", 0, "", err
	}
	if !ok {
		return "", sizeGB, machines.NotRestored, nil
	}
	s.log().Info("work disk from a resume image", "machine", m.Name, "run_id", a.RunID, "job_id", a.JobID, "image", im.Image)
	return im.Image, max(sizeGB, im.SizeGB), machines.Resumed, nil
}

// resume finds the job's resume image, and claims it for the job's ID
// (specs/0002-machines.md, Resuming): one made in an earlier attempt of the
// job's run, for the job's name and architecture, within a day of its machine
// stopping, where exactly one job had that name in the attempt that was
// preempted and exactly one has it in the one starting. A claim is created
// only if absent, so no other job ever gets the image.
func (s *Service) resume(ctx context.Context, m machines.Machine, a machines.Admission) (Image, bool, error) {
	if !m.Selector.Scope.Saves() || a.RunAttempt <= 1 {
		return Image{}, false, nil
	}
	recs, err := listJSON[Preemption](ctx, s.Objects, preemptionPrefix(m.RepositoryID, a.RunID))
	if err != nil {
		return Image{}, false, err
	}
	var cands []Preemption
	for _, r := range recs {
		p := r.v
		if p.ResumeImage != "" && p.RepositoryID == m.RepositoryID && p.RunID == a.RunID &&
			p.Arch == m.Selector.Arch && p.JobName == a.JobName && p.RunAttempt < a.RunAttempt && s.now().Sub(p.StoppedAt) < ResumeKept {
			cands = append(cands, p)
		}
	}
	if len(cands) == 0 {
		return Image{}, false, nil
	}
	// The latest attempt's image is the furthest on.
	slices.SortFunc(cands, func(x, y Preemption) int {
		return cmp.Or(cmp.Compare(y.RunAttempt, x.RunAttempt), y.StoppedAt.Compare(x.StoppedAt))
	})
	repo, err := s.GitHub.RepositoryByID(ctx, m.RepositoryID)
	if err != nil {
		return Image{}, false, err
	}
	if repo.OwnerID != s.Config.Organization.ID {
		return Image{}, false, nil
	}
	unique := func(attempt int) (bool, error) {
		jobs, err := s.GitHub.AttemptJobs(ctx, repo, a.RunID, attempt)
		if err != nil {
			return false, err
		}
		n := 0
		for _, j := range jobs {
			if j.Name == a.JobName {
				n++
			}
		}
		return n == 1, nil
	}
	if ok, err := unique(a.RunAttempt); err != nil || !ok {
		if !ok && err == nil {
			s.log().Info("no resume: the job's name isn't unique in its attempt", "run_id", a.RunID, "job_id", a.JobID)
		}
		return Image{}, false, err
	}
	for _, p := range cands {
		if ok, err := unique(p.RunAttempt); err != nil {
			return Image{}, false, err
		} else if !ok {
			s.log().Info("no resume: the job's name wasn't unique in the preempted attempt", "run_id", a.RunID,
				"job_id", a.JobID, "attempt", p.RunAttempt)
			continue
		}
		var got Image
		var claimed bool
		err := s.step(ctx, m.RepositoryID, func(ctx context.Context) error {
			rec, _, ok, err := getJSON[Image](ctx, s.Objects, imageKey(m.RepositoryID, p.ResumeImage))
			if err != nil || !ok || rec.State != ImageReady || rec.Kind != KindResume || rec.RunID != a.RunID ||
				rec.Arch != m.Selector.Arch || rec.JobName != a.JobName {
				return err
			}
			im, err := s.Compute.Image(ctx, rec.Image)
			if errors.Is(err, compute.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if !matches(im, rec) {
				return nil
			}
			c, _, err := createOnce(ctx, s.Objects, claimKey(m.RepositoryID, rec.Image), Claim{Image: rec.Image,
				RepositoryID: m.RepositoryID, RunID: a.RunID, RunAttempt: a.RunAttempt, JobID: a.JobID, At: s.now()})
			if err != nil {
				return err
			}
			if c.JobID != a.JobID {
				return nil
			}
			if err := s.touch(ctx, imageKey(m.RepositoryID, rec.Image), rec.Image); err != nil {
				return err
			}
			got, claimed = rec, true
			return nil
		})
		if err != nil {
			return Image{}, false, err
		}
		if claimed {
			return got, true, nil
		}
	}
	return Image{}, false, nil
}

// restore is the run's restore choice for a cache on the job's
// architecture, picked and recorded under the disks' lease the first time,
// then the same for every job of the run that restores that cache on that
// architecture. A recorded choice is read, and its version touched, under
// the lease too, so a pass that drops the choice or marks the version does
// so wholly before or after. It returns nil for none, and the step of the
// restore order the choice came from. Without record, it's a check: what a
// restore would choose now, the recorded choice if it stands or else what
// the restore order picks, with nothing written or touched, so the run's
// first restore still chooses.
func (s *Service) restore(ctx context.Context, m machines.Machine, a machines.Admission, cache, cacheKey string, record bool) (*Image, string, error) {
	repo, sel := m.RepositoryID, m.Selector
	key := choiceKey(repo, a.RunID, cache, sel.Arch)
	// valid is a recorded choice's version, if it still has one to give.
	valid := func(ctx context.Context, c Choice) (*Image, bool, error) {
		if c.Image == "" {
			return nil, true, nil
		}
		rec, _, ok, err := getJSON[Image](ctx, s.Objects, versionKey(repo, c.Image))
		if err != nil {
			return nil, false, err
		}
		if !ok || rec.Kind != KindCache || rec.State != ImageReady {
			return nil, false, nil
		}
		return &rec, true, nil
	}
	var out *Image
	outStep := StepNone
	err := s.step(ctx, repo, func(ctx context.Context) error {
		c, gen, ok, err := getJSON[Choice](ctx, s.Objects, key)
		if err != nil {
			return err
		}
		stale := false
		if ok && a.RunAttempt > c.Attempt {
			// Every attempt since the choice's must be one the service's
			// own reruns started, by GitHub's record of who triggered it.
			stale, err = s.rerunBySomeoneElse(ctx, m.RepositoryID, a.RunID, c.Attempt, a.RunAttempt)
			if err != nil {
				return err
			}
			if stale {
				s.log().Info("a maintainer's rerun picks its restore choice again", "run_id", a.RunID, "cache", cache,
					"arch", sel.Arch, "attempt", a.RunAttempt)
			}
		}
		if ok && !stale {
			im, good, err := valid(ctx, c)
			if err != nil {
				return err
			}
			if good {
				if im != nil && record {
					if err := s.touch(ctx, versionKey(repo, im.Image), im.Image); err != nil {
						return err
					}
				}
				out, outStep = im, c.Step
				return nil
			}
			// The recorded version is gone, which only happens if someone
			// else deleted it: the run picks again.
			s.log().Warn("a run's restore choice is gone; picking again", "run_id", a.RunID, "cache", cache, "version", c.Image)
		}
		recs, err := listJSON[Image](ctx, s.Objects, fmt.Sprintf("caches/%d/", repo))
		if err != nil {
			return err
		}
		chain, err := s.chain(ctx, repo, a.RunID, sel.Scope)
		if err != nil {
			return err
		}
		var closure *Closure
		if _, isPR := sel.Scope.PullRequestNumber(); isPR {
			c, _, found, err := getJSON[Closure](ctx, s.Objects, closureKey(repo, sel.Scope.String()))
			if err != nil {
				return err
			}
			if found {
				closure = &c
			}
		}
		var pick *Image
		step := StepNone
		if cands := candidates(recs, sel, cache, cacheKey, chain, closure); len(cands) > 0 {
			pick, step = &cands[0].Image, cands[0].step
		}
		if !record {
			out, outStep = pick, step
			return nil
		}
		choice := Choice{RepositoryID: repo, RunID: a.RunID, Selector: sel.String(), Scope: sel.Scope.String(), Arch: sel.Arch,
			Cache: cache, Step: step, Attempt: a.RunAttempt, At: s.now()}
		if pick != nil {
			choice.Image = pick.Image
		}
		if ok {
			_, err = replaceJSON(ctx, s.Objects, key, choice, gen)
		} else {
			// Under the lease, no one else writes it; a lost answer to our
			// own write leaves what we wrote.
			_, _, err = createOnce(ctx, s.Objects, key, choice)
		}
		if err != nil {
			return err
		}
		if pick != nil {
			if err := s.touch(ctx, versionKey(repo, pick.Image), pick.Image); err != nil {
				// Not chosen after all: the choice names a version that
				// isn't ready, so the next try picks again.
				return err
			}
		}
		// What each restore found, so the order and what's kept can be
		// tuned from evidence. Nothing here is a URL, token or payload.
		s.log().Info("restore choice", "repository_id", repo, "run_id", a.RunID, "selector", a.Selector, "scope", choice.Scope,
			"arch", choice.Arch, "cache", choice.Cache, "key", cacheKey, "step", step, "version", choice.Image)
		out, outStep = pick, step
		return nil
	})
	return out, outStep, err
}

// rerunBySomeoneElse reports whether any attempt of a run after from, up to
// to, was triggered by anyone but the App's own bot: by GitHub's record of
// each attempt.
func (s *Service) rerunBySomeoneElse(ctx context.Context, repoID, runID int64, from, to int) (bool, error) {
	repo, err := s.GitHub.RepositoryByID(ctx, repoID)
	if err != nil {
		return false, err
	}
	bot, err := s.GitHub.Bot(ctx)
	if err != nil {
		return false, err
	}
	for k := from + 1; k <= to; k++ {
		by, err := s.GitHub.AttemptTrigger(ctx, repo, runID, k)
		if err != nil {
			return false, err
		}
		if by != bot {
			return true, nil
		}
	}
	return false, nil
}

// chain is a run's restore chain, from its binding (specs/0002-machines.md,
// Scope): its own scope, if it saves, then its base. The job-started check
// admits only a job whose selector's scope is its run's binding, so the
// binding is there; if it isn't, the selector's scope stands in, with
// main's as its base.
func (s *Service) chain(ctx context.Context, repo, runID int64, sc scope.Scope) ([]scope.Scope, error) {
	bd, _, found, err := getJSON[binding.Binding](ctx, s.Objects, binding.Key(repo, runID))
	if err != nil {
		return nil, err
	}
	if !found || bd.Scope != sc {
		return scope.Chain(sc, scope.Main), nil
	}
	return bd.Chain(), nil
}

// Steps names the steps of the restore order that a choice from a scope
// came from, exact and newest (specs/0003-disks.md).
func Steps(sc scope.Scope) (exact, newest string) {
	switch {
	case sc.IsMain():
		return StepMainExact, StepMainNewest
	case sc.Lasting():
		return StepBranchExact, StepBranchNewest
	}
	return StepPullRequestExact, StepPullRequestNewest
}

// candidates are the versions of a cache a job may restore, in the order to
// try them (specs/0003-disks.md, Caches): for each scope of the run's
// restore chain in turn, its version for the cache's key, then its newest
// of the cache. Only versions of the job's architecture, ready and
// unmarked, count, and none of a pull request's saved by a run bound before
// it was last seen closed; of several versions, the newest comes first.
func candidates(recs map[string]record[Image], sel selector.Selector, cache, key string, chain []scope.Scope, closure *Closure) []candidate {
	var pool []Image
	for _, r := range recs {
		v := r.v
		if v.Kind == KindCache && v.State == ImageReady && v.Arch == sel.Arch && v.Cache == cache && !retired(v, closure) {
			pool = append(pool, v)
		}
	}
	slices.SortFunc(pool, func(x, y Image) int { return cmp.Or(y.SavedAt.Compare(x.SavedAt), strings.Compare(y.Image, x.Image)) })
	var out []candidate
	add := func(scope string, exact bool, step string) {
		for _, v := range pool {
			if v.Scope == scope && (!exact || v.Key == key) && !slices.ContainsFunc(out, func(o candidate) bool { return o.Image.Image == v.Image }) {
				out = append(out, candidate{v, step})
			}
		}
	}
	for _, sc := range chain {
		exact, newest := Steps(sc)
		add(sc.String(), true, exact)
		add(sc.String(), false, newest)
	}
	return out
}

// candidate is a version a job may restore, and the step of the restore
// order it comes from.
type candidate struct {
	Image Image
	step  string
}

// isUnavailable reports whether an error confirms nothing.
func isUnavailable(err error) bool { return github.IsUnavailable(err) || compute.IsUnavailable(err) }
