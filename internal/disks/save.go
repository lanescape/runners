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
	"github.com/lanescape/runners/internal/store"
)

// Save saves a finished job's work disk, from its stopped machine, as a new
// version of its scope's disk for its selector's family and key
// (specs/0003-disks.md, Saved disks). The machines' part has checked that
// the job succeeded, asked, and that its scope saves; this checks the scope
// and selector again, keeps to the scope's size limit, and deletes the older
// versions no unfinished run's choice names. It implements machines.Saver,
// and is called again until it returns nil.
func (s *Service) Save(ctx context.Context, m machines.Machine, a machines.Admission) error {
	sel := m.Selector
	if !a.Allowed || a.Machine != m.Name || a.Selector != sel.String() || !sel.Scope.Saves() || !sel.HasDisk() {
		return nil
	}
	log := s.log().With("machine", m.Name, "repository_id", m.RepositoryID, "run_id", a.RunID, "job_id", a.JobID, "selector", a.Selector)
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
		fits, why, err := s.makeRoom(ctx, m.RepositoryID, sel.Scope.String(), sel.Arch, sel.Family, sv.SizeGB)
		if err != nil {
			return err
		}
		if !fits {
			// The job has finished, so its own log can't say why; the
			// service's does, with the job's ID.
			log.Warn("work disk not saved: it would pass its scope's size limit", "reason", why, "size_gb", sv.SizeGB)
			sv.Skipped = why
			_, err := replaceJSON(ctx, s.Objects, saveKey(m.Name), sv, gen)
			return err
		}
		rec = Image{Image: sv.Image, RepositoryID: m.RepositoryID, Kind: KindSaved, State: ImageMaking, Scope: sel.Scope.String(),
			Arch: sel.Arch, Family: sel.Family, Key: sel.Key, Machine: m.Name, Zone: m.Zone, InstanceID: m.ID, JobID: a.JobID,
			JobName: a.JobName, RunID: a.RunID, RunAttempt: a.RunAttempt, Selector: a.Selector, SizeGB: sv.SizeGB,
			SavedAt: sv.At, LastUsed: sv.At}
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

// settled reports whether a run has finished with no automatic rerun it is,
// or may yet be, due, read afresh from GitHub and the bucket, as a pass
// reads it: a run or repository GitHub confirms is gone has settled.
func (s *Service) settled(ctx context.Context, repo, runID int64) (bool, error) {
	ps, err := listJSON[Preemption](ctx, s.Objects, preemptionPrefix(repo, runID))
	if err != nil {
		return false, err
	}
	st := &state{preemptions: ps, reruns: map[string]record[Rerun]{}, repos: map[int64]*github.Repository{}, runs: map[string]*run{}}
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

// counts reports whether a saved disk of scope counts against the limit of
// a save in scope save: main's alone, or all pull requests' together.
func counts(scope, save string) bool {
	return (scope == "main") == (save == "main")
}

// newest are main's newest disk of each family and architecture.
func newest(recs map[string]record[Image]) map[string]bool {
	best := map[string]Image{}
	for _, r := range recs {
		v := r.v
		if v.Kind != KindSaved || v.Scope != "main" || v.State != ImageReady {
			continue
		}
		k := v.Arch + "/" + v.Family
		if b, ok := best[k]; !ok || v.SavedAt.After(b.SavedAt) || (v.SavedAt.Equal(b.SavedAt) && v.Image > b.Image) {
			best[k] = v
		}
	}
	out := map[string]bool{}
	for _, v := range best {
		out[v.Image] = true
	}
	return out
}

// makeRoom keeps a save within its scope's limit (specs/0003-disks.md,
// Expiry and limits): if the save would pass it, it first deletes that
// scope's least recently used disks, never another scope's, nor the newest
// of a main family, other than the one the save replaces as newest, nor any
// that an unfinished run's choice names. If that wouldn't be enough, it
// deletes nothing, and says why the save is skipped. The saving job's own
// run has often just finished, and its choice is still recorded: a run
// counts as finished only once GitHub shows it, read inside the lease.
func (s *Service) makeRoom(ctx context.Context, repo int64, scope, arch, family string, sizeGB int64) (bool, string, error) {
	lim := s.Config.Limits.For(repo)
	limit := int64(lim.PullRequestDisksGB)
	if scope == "main" {
		limit = int64(lim.MainDisksGB)
	}
	// Saves happen only in passes, one at a time, so a save that fits
	// needs no lease; one that must make room marks under it.
	// A marked image counts until Compute Engine confirms it's deleted:
	// what's still stored isn't room.
	total := func(recs map[string]record[Image]) int64 {
		var n int64
		for _, r := range recs {
			if r.v.Kind == KindSaved && counts(r.v.Scope, scope) {
				n += r.v.SizeGB
			}
		}
		return n
	}
	recs, err := listJSON[Image](ctx, s.Objects, fmt.Sprintf("images/%d/", repo))
	if err != nil {
		return false, "", err
	}
	// Marked images of the limit's scopes go first.
	var pending bool
	for _, r := range recs {
		if r.v.Kind == KindSaved && r.v.State == ImageMarked && counts(r.v.Scope, scope) {
			if err := s.deleteMarked(ctx, r.v); err != nil {
				return false, "", fmt.Errorf("deleting a marked disk before a save: %w", err)
			}
			pending = true
		}
	}
	if pending {
		if recs, err = listJSON[Image](ctx, s.Objects, fmt.Sprintf("images/%d/", repo)); err != nil {
			return false, "", err
		}
	}
	if total(recs)+sizeGB <= limit {
		return true, "", nil
	}
	var fits bool
	var why string
	var marked []Image
	err = s.step(ctx, repo, func(ctx context.Context) error {
		recs, err := listJSON[Image](ctx, s.Objects, fmt.Sprintf("images/%d/", repo))
		if err != nil {
			return err
		}
		total := total(recs)
		need := total + sizeGB - limit
		if need <= 0 {
			fits = true
			return nil
		}
		choices, err := listJSON[Choice](ctx, s.Objects, fmt.Sprintf("choices/%d/", repo))
		if err != nil {
			return err
		}
		namers := map[string][]string{}
		for k, c := range choices {
			if c.v.Image != "" {
				namers[c.v.Image] = append(namers[c.v.Image], k)
			}
		}
		// A choice protects its image while its run may need it. One of a
		// run that GitHub shows finished, with no automatic rerun due, is
		// dropped before its image is marked, so that a maintainer's rerun
		// picks again, under this lease.
		settled := map[int64]bool{}
		free := func(image string) (bool, error) {
			for _, k := range namers[image] {
				run := choices[k].v.RunID
				ok, seen := settled[run]
				if !seen {
					if ok, err = s.settled(ctx, repo, run); err != nil {
						return false, err
					}
					settled[run] = ok
				}
				if !ok {
					return false, nil
				}
			}
			return true, nil
		}
		top := newest(recs)
		var cands []record[Image]
		for _, r := range recs {
			v := r.v
			if v.Kind != KindSaved || v.State != ImageReady || v.Scope != scope {
				continue
			}
			if top[v.Image] && !(v.Arch == arch && v.Family == family) {
				continue
			}
			cands = append(cands, r)
		}
		slices.SortFunc(cands, func(x, y record[Image]) int {
			return cmp.Or(x.v.LastUsed.Compare(y.v.LastUsed), x.v.SavedAt.Compare(y.v.SavedAt), strings.Compare(x.v.Image, y.v.Image))
		})
		var freed int64
		var pick []record[Image]
		for _, c := range cands {
			if freed >= need {
				break
			}
			ok, err := free(c.v.Image)
			if err != nil {
				return err
			}
			if ok {
				pick, freed = append(pick, c), freed+c.v.SizeGB
			}
		}
		if freed < need {
			why = fmt.Sprintf("the scope's disks would be %d GB of its %d GB limit, and only %d GB of them may be deleted to make room",
				total+sizeGB, limit, freed)
			return nil
		}
		for _, c := range pick {
			for _, k := range namers[c.v.Image] {
				if err := s.Objects.Delete(ctx, k, choices[k].gen); err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
			}
			v, err := s.markOne(ctx, c.v, c.gen)
			if err != nil {
				return err
			}
			marked = append(marked, v)
		}
		fits = true
		return nil
	})
	if err != nil {
		return false, "", err
	}
	for _, v := range marked {
		s.log().Info("saved disk deleted to make room", "image", v.Image, "repository_id", repo, "scope", v.Scope)
		if err := s.deleteMarked(ctx, v); err != nil {
			return false, "", err
		}
	}
	return fits, why, nil
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
// which rewrites the record, leaves it for a later pass.
func (s *Service) mark(ctx context.Context, repo int64, cands []record[Image], finished map[int64]bool) ([]Image, error) {
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

// supersede deletes the older versions of a saved disk once a new one is
// ready, except those an unfinished run's choice names, which go once it
// has finished.
func (s *Service) supersede(ctx context.Context, rec Image) error {
	recs, err := listJSON[Image](ctx, s.Objects, fmt.Sprintf("images/%d/", rec.RepositoryID))
	if err != nil {
		return err
	}
	var older []record[Image]
	for _, r := range recs {
		v := r.v
		if v.Kind == KindSaved && v.State == ImageReady && v.Image != rec.Image && v.Scope == rec.Scope && v.Arch == rec.Arch &&
			v.Family == rec.Family && v.Key == rec.Key && v.SavedAt.Before(rec.SavedAt) {
			older = append(older, r)
		}
	}
	if len(older) == 0 {
		return nil
	}
	slices.SortFunc(older, func(x, y record[Image]) int { return strings.Compare(x.v.Image, y.v.Image) })
	marked, err := s.mark(ctx, rec.RepositoryID, older, nil)
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range marked {
		s.log().Info("older version of a saved disk deleted", "image", v.Image, "newer", rec.Image)
		errs = append(errs, s.deleteMarked(ctx, v))
	}
	return errors.Join(errs...)
}
