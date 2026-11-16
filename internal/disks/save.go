package disks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/store"
)

// unreported are the runs, by ID, with a machine whose job was admitted and
// that hasn't reported, other than except: one may be a stopped Spot
// machine that a pass hasn't yet recorded, or couldn't, whose run may be
// due a rerun that needs its choices. A machine's records go only once
// it's deleted, after any such record is made.
func (s *Service) unreported(ctx context.Context, except string) (map[int64]bool, error) {
	objs, err := s.Objects.List(ctx, "machines/")
	if err != nil {
		return nil, err
	}
	admitted, reported := map[string]bool{}, map[string]bool{}
	for _, o := range objs {
		parts := strings.Split(o.Name, "/")
		if len(parts) != 3 || parts[1] == except {
			continue
		}
		switch parts[2] {
		case "admission.json":
			admitted[parts[1]] = true
		case "report.json":
			reported[parts[1]] = true
		}
	}
	out := map[int64]bool{}
	for m := range admitted {
		if reported[m] {
			continue
		}
		a, _, ok, err := getJSON[machines.Admission](ctx, s.Objects, "machines/"+m+"/admission.json")
		if err != nil {
			return nil, err
		}
		if ok && a.Allowed {
			out[a.RunID] = true
		}
	}
	return out, nil
}

// settled reports whether a run has finished with no automatic rerun it is,
// or may yet be, due, read afresh from GitHub and the bucket, as a pass
// reads it: a run GitHub confirms is gone has settled. A run among
// unreported hasn't. A repository the pass's state, pass, can't confirm
// settles nothing, and one this read can't confirm is recorded there.
func (s *Service) settled(ctx context.Context, pass *state, repo, runID int64, unreported map[int64]bool) (bool, error) {
	if unreported[runID] {
		return false, nil
	}
	ps, err := listJSON[Preemption](ctx, s.Objects, preemptionPrefix(repo, runID))
	if err != nil {
		return false, err
	}
	st := &state{preemptions: ps, reruns: map[string]record[Rerun]{}, repos: map[int64]*github.Repository{}, unconfirmed: pass.unconfirmed,
		runs: map[string]*run{}}
	if rr, _, ok, err := getJSON[Rerun](ctx, s.Objects, rerunKey(repo, runID)); err != nil {
		return false, err
	} else if ok {
		st.reruns[rerunKey(repo, runID)] = record[Rerun]{v: rr}
	}
	rs, err := s.runState(ctx, st, repo, runID)
	if err != nil {
		return false, err
	}
	return rs.finished && !rs.due, nil
}

// named are the images that any recorded restore choice names, and the
// resume images any claim holds whose job isn't among finished. A choice
// is dropped once its run has finished and has no automatic rerun left, so
// every choice there is names an image an unfinished run may still need.
// It's read inside the disks' lease.
func (s *Service) named(ctx context.Context, repo int64, finished map[int64]bool) (map[string]bool, error) {
	out := map[string]bool{}
	choices, err := listJSON[Choice](ctx, s.Objects, fmt.Sprintf("choices/%d/", repo))
	if err != nil {
		return nil, err
	}
	for _, c := range choices {
		if c.v.Image != "" {
			out[c.v.Image] = true
		}
	}
	claims, err := listJSON[Claim](ctx, s.Objects, fmt.Sprintf("claims/%d/", repo))
	if err != nil {
		return nil, err
	}
	for _, c := range claims {
		if !finished[c.v.JobID] {
			out[c.v.Image] = true
		}
	}
	return out, nil
}

// markOne marks a ready image for deletion, with the generation it was read
// at, so a choice or claim of it since then makes the mark fail.
func (s *Service) markOne(ctx context.Context, rec Image, gen int64) (Image, error) {
	rec.State, rec.MarkedAt = ImageMarked, s.now()
	if _, err := replaceJSON(ctx, s.Objects, recordKey(rec), rec, gen); err != nil {
		return rec, err
	}
	return rec, nil
}

// mark marks images for deletion under the consumer's disks lease, except
// those a choice names or a claim of an unfinished job holds, read inside
// the lease after the candidates were; it returns those it marked. Each is
// marked only at the generation its candidate was read at, when the
// decision to delete it was made, so a choice or use of it since then,
// which rewrites the record, leaves it for a later pass. recheck, if set,
// is asked under the lease whether each candidate should still go, for a
// decision that rests on more than its own record.
func (s *Service) mark(ctx context.Context, repo int64, cands []record[Image], finished map[int64]bool,
	recheck func(ctx context.Context, rec Image) (bool, error)) ([]Image, error) {
	var marked []Image
	err := s.step(ctx, repo, func(ctx context.Context) error {
		named, err := s.named(ctx, repo, finished)
		if err != nil {
			return err
		}
		for _, c := range cands {
			rec, gen := c.v, c.gen
			if named[rec.Image] || rec.State != ImageReady {
				continue
			}
			if recheck != nil {
				if ok, err := recheck(ctx, rec); err != nil {
					return err
				} else if !ok {
					continue
				}
			}
			v, err := s.markOne(ctx, rec, gen)
			if errors.Is(err, store.ErrPrecondition) {
				continue
			}
			if err != nil {
				return err
			}
			marked = append(marked, v)
		}
		return nil
	})
	return marked, err
}

// deleteMarked deletes a marked version's objects, or a marked image, then
// its record, and any claim of it.
func (s *Service) deleteMarked(ctx context.Context, rec Image) error {
	if rec.State != ImageMarked {
		return fmt.Errorf("%s isn't marked for deletion", rec.Image)
	}
	if rec.Kind == KindCache {
		if err := s.deleteObjects(ctx, VersionPrefix(rec.RepositoryID, rec.Image)); err != nil {
			return err
		}
	} else {
		err := s.Compute.DeleteImage(ctx, rec.Image, freshID("delete image", rec.Image))
		if err != nil && !errors.Is(err, compute.ErrNotFound) {
			return err
		}
	}
	if c, gen, ok, err := getJSON[Claim](ctx, s.Objects, claimKey(rec.RepositoryID, rec.Image)); err == nil && ok && c.Image == rec.Image {
		_ = s.Objects.Delete(ctx, claimKey(rec.RepositoryID, rec.Image), gen)
	}
	_, gen, ok, err := getJSON[Image](ctx, s.Objects, recordKey(rec))
	if err != nil || !ok {
		return err
	}
	if err := s.Objects.Delete(ctx, recordKey(rec), gen); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// replaces reports whether a ready version, older than a newly ready one, is
// one the new one replaces: any of its scope's versions of the cache on its
// architecture, of its key or of any other, since a scope keeps one version
// of each cache and architecture, its newest save (specs/0003-disks.md,
// Deletion).
func replaces(newer, v Image) bool {
	return v.Kind == KindCache && v.State == ImageReady && v.Image != newer.Image && v.Scope == newer.Scope && v.Arch == newer.Arch &&
		v.Cache == newer.Cache && olderThan(v, newer)
}

// olderThan orders versions by when they were saved, then by name.
func olderThan(x, y Image) bool {
	return x.SavedAt.Before(y.SavedAt) || (x.SavedAt.Equal(y.SavedAt) && x.Image < y.Image)
}

// isPullRequest reports whether a scope is a pull request's.
func isPullRequest(sc string) (int, bool) {
	parsed, err := scope.Parse(sc)
	if err != nil {
		return 0, false
	}
	return parsed.PullRequestNumber()
}

// lasting reports whether a scope is main's or a long-lived branch's.
func lasting(sc string) bool {
	parsed, err := scope.Parse(sc)
	return err == nil && parsed.Lasting()
}

// supersede deletes what a newly ready version replaces, except what an
// unfinished run's choice names, which goes once it has finished. A pull
// request's version saved by a run bound before the pull request was last
// seen closed replaces nothing: it's never chosen, and goes itself. That's
// read again under the lease, as the closure may be recorded meanwhile.
func (s *Service) supersede(ctx context.Context, rec Image) error {
	recs, err := listJSON[Image](ctx, s.Objects, fmt.Sprintf("caches/%d/", rec.RepositoryID))
	if err != nil {
		return err
	}
	var older []record[Image]
	for _, r := range recs {
		if replaces(rec, r.v) {
			older = append(older, r)
		}
	}
	if len(older) == 0 {
		return nil
	}
	slices.SortFunc(older, func(x, y record[Image]) int { return strings.Compare(x.v.Image, y.v.Image) })
	// GitHub must still show the repository as the organization's when its
	// images are marked, under the lease: it's read again once, before the
	// first. If it doesn't, nothing is replaced now; a pass replaces it
	// once GitHub shows the repository again.
	checked, shown := false, false
	marked, err := s.mark(ctx, rec.RepositoryID, older, nil, func(ctx context.Context, _ Image) (bool, error) {
		if !checked {
			repo, err := s.GitHub.RepositoryByID(ctx, rec.RepositoryID)
			switch {
			case errors.Is(err, github.ErrNotFound):
				_ = s.notShown(rec.RepositoryID, "GitHub answers 404 for it: it's deleted, or the App can't read it")
			case err != nil:
				return false, err
			case repo.OwnerID != s.Config.Organization.ID:
				_ = s.notShown(rec.RepositoryID, "GitHub shows it as another owner's: it's been transferred")
			default:
				shown = true
			}
			checked = true
		}
		if !shown {
			return false, nil
		}
		if _, pr := isPullRequest(rec.Scope); !pr {
			return true, nil
		}
		c, _, found, err := getJSON[Closure](ctx, s.Objects, closureKey(rec.RepositoryID, rec.Scope))
		if err != nil || !found {
			return err == nil, err
		}
		return !retired(rec, &c), nil
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range marked {
		s.log().Info("cache version deleted: a newer one replaces it", "version", v.Image, "newer", rec.Image, "scope", v.Scope,
			"cache", v.Cache)
		errs = append(errs, s.deleteMarked(ctx, v))
	}
	return errors.Join(errs...)
}
