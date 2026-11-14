package disks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/store"
)

// Save saves a finished job's work disk, from its stopped machine, as a new
// version of its scope's disk for its selector's family and key
// (specs/0003-disks.md, Saved disks). The machines' part has checked that
// the job succeeded, asked, and that its scope saves; this checks the scope
// and selector again, and deletes what the new disk replaces that no
// unfinished run's choice names: its scope's older disks of the family on
// its architecture, whatever their key. It
// implements machines.Saver, and is called again until it returns nil.
func (s *Service) Save(ctx context.Context, m machines.Machine, a machines.Admission) error {
	sel := m.Selector
	if !a.Allowed || a.Machine != m.Name || a.Selector != sel.String() || !sel.Scope.Saves() || !sel.HasDisk() {
		return nil
	}
	log := s.log().With("machine", m.Name, "repository_id", m.RepositoryID, "run_id", a.RunID, "job_id", a.JobID, "selector", a.Selector)
	// Only the organization's consumers save.
	if repo, err := s.GitHub.RepositoryByID(ctx, m.RepositoryID); err != nil {
		return err
	} else if repo.OwnerID != s.Config.Organization.ID {
		log.Warn("work disk not saved: the repository is no longer the organization's")
		return nil
	}
	sv, gen, found, err := getJSON[Save](ctx, s.Objects, saveKey(m.Name))
	if err != nil {
		return err
	}
	if found && (sv.Done || sv.Skipped != "") {
		return nil
	}
	if !found {
		d, err := s.Compute.Disk(ctx, m.Zone, machines.WorkDisk(m.Name))
		if errors.Is(err, compute.ErrNotFound) {
			log.Warn("the job asked to save its work disk, and it has none")
			_, _, err := createOnce(ctx, s.Objects, saveKey(m.Name), Save{Machine: m.Name, Skipped: "no work disk", At: s.now()})
			return err
		}
		if err != nil {
			return err
		}
		series := Series(sel.Scope.String(), sel.Arch, sel.Family, sel.Key)
		sv, gen, err = createOnce(ctx, s.Objects, saveKey(m.Name), Save{Machine: m.Name, Image: SavedName(m.RepositoryID, series, s.now(), m.Name),
			SizeGB: d.SizeGB, At: s.now()})
		if err != nil {
			return err
		}
	}
	key := imageKey(m.RepositoryID, sv.Image)
	rec, rgen, ok, err := getJSON[Image](ctx, s.Objects, key)
	if err != nil {
		return err
	}
	if !ok {
		// When the run was bound, so that a pull request's disk saved by a
		// run from before it closed is never chosen after it's reopened.
		var bound time.Time
		if b, _, found, err := getJSON[binding.Binding](ctx, s.Objects, binding.Key(m.RepositoryID, a.RunID)); err != nil {
			return err
		} else if found {
			bound = b.BoundAt
		}
		rec = Image{Image: sv.Image, RepositoryID: m.RepositoryID, Kind: KindSaved, State: ImageMaking, Scope: sel.Scope.String(),
			Arch: sel.Arch, Family: sel.Family, Key: sel.Key, Machine: m.Name, Zone: m.Zone, InstanceID: m.ID, JobID: a.JobID,
			JobName: a.JobName, RunID: a.RunID, RunAttempt: a.RunAttempt, Selector: a.Selector, SizeGB: sv.SizeGB,
			SavedAt: sv.At, BoundAt: bound}
		if rec, rgen, err = createOnce(ctx, s.Objects, key, rec); err != nil {
			return err
		}
	}
	if rec, _, err = s.make(ctx, key, rec, rgen); err != nil {
		return fmt.Errorf("saving the work disk as %s: %w", rec.Image, err)
	}
	log.Info("work disk saved", "image", rec.Image, "scope", rec.Scope, "family", rec.Family, "key", rec.Key, "size_gb", rec.SizeGB)
	if err := s.supersede(ctx, rec); err != nil {
		// The pass deletes them later.
		log.Warn("deleting older versions of a saved disk", "err", err.Error())
	}
	sv.Done = true
	_, err = replaceJSON(ctx, s.Objects, saveKey(m.Name), sv, gen)
	return err
}

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
	if _, err := replaceJSON(ctx, s.Objects, imageKey(rec.RepositoryID, rec.Image), rec, gen); err != nil {
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

// deleteMarked deletes a marked image, then its record, and any claim of
// it.
func (s *Service) deleteMarked(ctx context.Context, rec Image) error {
	if rec.State != ImageMarked {
		return fmt.Errorf("image %s isn't marked for deletion", rec.Image)
	}
	err := s.Compute.DeleteImage(ctx, rec.Image, freshID("delete image", rec.Image))
	if err != nil && !errors.Is(err, compute.ErrNotFound) {
		return err
	}
	if c, gen, ok, err := getJSON[Claim](ctx, s.Objects, claimKey(rec.RepositoryID, rec.Image)); err == nil && ok && c.Image == rec.Image {
		_ = s.Objects.Delete(ctx, claimKey(rec.RepositoryID, rec.Image), gen)
	}
	_, gen, ok, err := getJSON[Image](ctx, s.Objects, imageKey(rec.RepositoryID, rec.Image))
	if err != nil || !ok {
		return err
	}
	if err := s.Objects.Delete(ctx, imageKey(rec.RepositoryID, rec.Image), gen); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// replaces reports whether a ready saved disk, older than a newly saved one,
// is one the new one replaces: any of its scope's disks of the family on its
// architecture, an older version of its key or of any other, since a scope
// keeps one disk of each family and architecture, its newest save
// (specs/0003-disks.md, Deletion).
func replaces(newer, v Image) bool {
	return v.Kind == KindSaved && v.State == ImageReady && v.Image != newer.Image && v.Scope == newer.Scope && v.Arch == newer.Arch &&
		v.Family == newer.Family && olderThan(v, newer)
}

// olderThan orders saved disks by when they were saved, then by name.
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

// supersede deletes what a newly ready saved disk replaces, except what an
// unfinished run's choice names, which goes once it has finished. A pull
// request's disk saved by a run bound before the pull request was last seen
// closed replaces nothing: it's never chosen, and goes itself. That's read
// again under the lease, as the closure may be recorded meanwhile.
func (s *Service) supersede(ctx context.Context, rec Image) error {
	recs, err := listJSON[Image](ctx, s.Objects, fmt.Sprintf("images/%d/", rec.RepositoryID))
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
		s.log().Info("saved disk deleted: a newer one replaces it", "image", v.Image, "newer", rec.Image, "scope", v.Scope)
		errs = append(errs, s.deleteMarked(ctx, v))
	}
	return errors.Join(errs...)
}
