package disks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/selector"
)

// Choose picks what an admitted job's work disk starts from
// (specs/0002-machines.md, The job-started check): its run's resume image
// for its selector and name, if one is eligible and it can claim it;
// otherwise its run's restore choice for its selector, made the first time
// the run's job with that selector is admitted. It returns "" for an empty
// disk, and the disk's size: the shape's, or the image's if that's larger.
// It implements machines.DiskSource.
func (s *Service) Choose(ctx context.Context, m machines.Machine, a machines.Admission, sizeGB int64) (string, int64, error) {
	if im, ok, err := s.resume(ctx, m, a); err != nil {
		return "", 0, err
	} else if ok {
		s.log().Info("work disk from a resume image", "machine", m.Name, "run_id", a.RunID, "job_id", a.JobID, "image", im.Image)
		return im.Image, max(sizeGB, im.SizeGB), nil
	}
	if !m.Selector.HasDisk() {
		return "", sizeGB, nil
	}
	im, err := s.restore(ctx, m, a)
	if err != nil {
		return "", 0, err
	}
	if im == nil {
		s.log().Info("work disk starts empty: no saved disk to restore", "machine", m.Name, "run_id", a.RunID, "selector", a.Selector)
		return "", sizeGB, nil
	}
	s.used(ctx, *im)
	s.log().Info("work disk restored", "machine", m.Name, "run_id", a.RunID, "selector", a.Selector, "image", im.Image, "scope", im.Scope, "key", im.Key)
	return im.Image, max(sizeGB, im.SizeGB), nil
}

// resume finds the job's resume image, and claims it for the job's ID
// (specs/0002-machines.md, Resuming): one made in an earlier attempt of the
// job's run, for the job's selector and name, within a day of its machine
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
		if p.ResumeImage != "" && p.RepositoryID == m.RepositoryID && p.RunID == a.RunID && p.Selector == a.Selector &&
			p.JobName == a.JobName && p.RunAttempt < a.RunAttempt && s.now().Sub(p.StoppedAt) < ResumeKept {
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
				rec.Selector != a.Selector || rec.JobName != a.JobName {
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
			if err := s.touch(ctx, m.RepositoryID, rec.Image); err != nil {
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

// restore is the run's restore choice for the job's selector, picked and
// recorded under the disks' lease the first time, then the same for every
// job of the run with that selector. It returns nil for an empty disk.
func (s *Service) restore(ctx context.Context, m machines.Machine, a machines.Admission) (*Image, error) {
	repo, sel := m.RepositoryID, m.Selector
	key := choiceKey(repo, a.RunID, sel)
	// valid is a recorded choice's image, if it still has one to give.
	valid := func(ctx context.Context, c Choice) (*Image, bool, error) {
		if c.Image == "" {
			return nil, true, nil
		}
		rec, _, ok, err := getJSON[Image](ctx, s.Objects, imageKey(repo, c.Image))
		if err != nil {
			return nil, false, err
		}
		if !ok || rec.State != ImageReady {
			return nil, false, nil
		}
		im, err := s.Compute.Image(ctx, rec.Image)
		if errors.Is(err, compute.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return &rec, matches(im, rec), nil
	}
	if c, _, ok, err := getJSON[Choice](ctx, s.Objects, key); err != nil {
		return nil, err
	} else if ok {
		im, good, err := valid(ctx, c)
		if err != nil || good {
			return im, err
		}
	}
	var out *Image
	err := s.step(ctx, repo, func(ctx context.Context) error {
		c, gen, ok, err := getJSON[Choice](ctx, s.Objects, key)
		if err != nil {
			return err
		}
		if ok {
			im, good, err := valid(ctx, c)
			if err != nil {
				return err
			}
			if good {
				out = im
				return nil
			}
			// The recorded image is gone, which only happens if someone
			// else deleted it: the run picks again.
			s.log().Warn("a run's restore choice is gone; picking again", "run_id", a.RunID, "selector", a.Selector, "image", c.Image)
		}
		recs, err := listJSON[Image](ctx, s.Objects, fmt.Sprintf("images/%d/", repo))
		if err != nil {
			return err
		}
		var pick *Image
		for _, cand := range candidates(recs, sel) {
			im, err := s.Compute.Image(ctx, cand.Image)
			if errors.Is(err, compute.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if matches(im, cand) {
				pick = &cand
				break
			}
		}
		choice := Choice{RepositoryID: repo, RunID: a.RunID, Selector: sel.String(), At: s.now()}
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
			if err := s.touch(ctx, repo, pick.Image); err != nil {
				// Not chosen after all: the choice names an image that
				// isn't ready, so the next try picks again.
				return err
			}
		}
		out = pick
		return nil
	})
	return out, err
}

// candidates are the saved disks a selector may start from, in the order to
// try them (specs/0003-disks.md, Saved disks): in a pull request's scope,
// that pull request's disk for the family and key, then its newest for the
// family; then main's for the family and key, then main's newest for the
// family. Only disks of the selector's architecture, ready and unmarked,
// count; of several versions, the newest comes first.
func candidates(recs map[string]record[Image], sel selector.Selector) []Image {
	var pool []Image
	for _, r := range recs {
		v := r.v
		if v.Kind == KindSaved && v.State == ImageReady && v.Arch == sel.Arch && v.Family == sel.Family {
			pool = append(pool, v)
		}
	}
	slices.SortFunc(pool, func(x, y Image) int { return cmp.Or(y.SavedAt.Compare(x.SavedAt), strings.Compare(y.Image, x.Image)) })
	var out []Image
	add := func(scope string, exact bool) {
		for _, v := range pool {
			if v.Scope == scope && (!exact || v.Key == sel.Key) && !slices.ContainsFunc(out, func(o Image) bool { return o.Image == v.Image }) {
				out = append(out, v)
			}
		}
	}
	if _, ok := sel.Scope.PullRequestNumber(); ok {
		add(sel.Scope.String(), true)
		add(sel.Scope.String(), false)
	}
	add("main", true)
	add("main", false)
	return out
}

// used marks a saved disk used, at most once a day: in its record, which
// expiry reads, and in its last-used label. It's best effort: a failure
// leaves the mark to the next use.
func (s *Service) used(ctx context.Context, rec Image) {
	if day(rec.LastUsed) == day(s.now()) {
		return
	}
	key := imageKey(rec.RepositoryID, rec.Image)
	cur, gen, ok, err := getJSON[Image](ctx, s.Objects, key)
	if err != nil || !ok || cur.State != ImageReady {
		return
	}
	cur.LastUsed = s.now()
	if _, err := replaceJSON(ctx, s.Objects, key, cur, gen); err != nil {
		s.log().Warn("marking a saved disk used", "image", rec.Image, "err", err.Error())
		return
	}
	im, err := s.Compute.Image(ctx, rec.Image)
	if err != nil {
		return
	}
	l := map[string]string{}
	for k, v := range im.Labels {
		l[k] = v
	}
	l[LabelLastUsed] = day(cur.LastUsed)
	if err := s.Compute.SetImageLabels(ctx, rec.Image, l, im.LabelFingerprint); err != nil && !errors.Is(err, compute.ErrFingerprint) {
		s.log().Warn("labelling a saved disk used", "image", rec.Image, "err", err.Error())
	}
}

// isUnavailable reports whether an error confirms nothing.
func isUnavailable(err error) bool { return github.IsUnavailable(err) || compute.IsUnavailable(err) }
