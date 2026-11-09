package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/store"
)

// workflowJob is what the cache reads of GitHub's workflow_job delivery.
type workflowJob struct {
	Action      string `json:"action"`
	WorkflowJob *struct {
		ID int64 `json:"id"`
	} `json:"workflow_job"`
	Repository *struct {
		ID    int64 `json:"id"`
		Owner *struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	} `json:"repository"`
}

// RecordEvent settles the pending entries of a job that GitHub's
// workflow_job delivery says has completed, before the push is
// acknowledged, so an entry becomes restorable, or goes, as soon as GitHub
// shows its job finished (specs/0006-cache.md, Saving). What decides is
// GitHub's record of the job, read afresh; each pass checks every pending
// entry too, in case a delivery was lost. It ignores every other message.
func (s *Service) RecordEvent(ctx context.Context, attributes map[string]string, data []byte) error {
	if attributes["event"] != "workflow_job" {
		return nil
	}
	var d workflowJob
	if json.Unmarshal(data, &d) != nil || d.Action != "completed" || d.WorkflowJob == nil || d.WorkflowJob.ID <= 0 ||
		d.Repository == nil || d.Repository.Owner == nil || d.Repository.Owner.ID != s.OrganizationID || d.Repository.ID <= 0 {
		return nil
	}
	recs, err := listJSON[Entry](ctx, s.Objects, entryPrefix(d.Repository.ID))
	if err != nil {
		return err
	}
	var pending []record[Entry]
	for _, r := range recs {
		if r.v.State == Pending && r.v.JobID == d.WorkflowJob.ID {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	job, err := s.job(ctx, d.Repository.ID, d.WorkflowJob.ID)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range pending {
		errs = append(errs, s.settle(ctx, r.v, job))
	}
	return errors.Join(errs...)
}

// job is GitHub's record of a job, or nil if GitHub confirms there's none,
// or its repository is gone or no longer the organization's. An error
// confirms nothing.
func (s *Service) job(ctx context.Context, repoID, id int64) (*github.Job, error) {
	repo, err := s.GitHub.RepositoryByID(ctx, repoID)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if repo.OwnerID != s.OrganizationID {
		return nil, nil
	}
	j, err := s.GitHub.Job(ctx, repo, id)
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// settle decides a pending entry by its job, as GitHub records it: one whose
// job succeeded becomes ready, unless its pull request has closed since its
// run was bound; one whose job finished otherwise, or that GitHub doesn't
// show finished within 5 days, or whose commit never finished, goes.
func (s *Service) settle(ctx context.Context, e Entry, job *github.Job) error {
	switch {
	case job == nil:
		return s.discard(ctx, e, "GitHub shows no such job of the organization's")
	case job.Status != "completed":
		if s.now().Sub(e.CommittedAt) >= PendingFor {
			return s.discard(ctx, e, "its job isn't shown finished within 5 days")
		}
		return nil
	case job.Conclusion != "success":
		return s.discard(ctx, e, "its job's conclusion is "+job.Conclusion)
	case e.Generation == 0:
		return s.discard(ctx, e, "its commit never finished")
	}
	return s.ready(ctx, e)
}

// ready makes a pending entry restorable, under the consumer's short lease,
// and deletes what it replaces. A new entry counts as used when it becomes
// ready.
func (s *Service) ready(ctx context.Context, e Entry) error {
	var made *Entry
	retired := false
	err := s.Disks.Step(ctx, e.RepositoryID, func(ctx context.Context) error {
		cur, gen, ok, err := getJSON[Entry](ctx, s.Objects, entryKey(e.RepositoryID, e.ID))
		if err != nil || !ok || cur.State != Pending {
			return err
		}
		if isPullRequest(cur.Scope) {
			c, err := s.Disks.ClosureOf(ctx, cur.RepositoryID, cur.Scope)
			if err != nil {
				return err
			}
			if retiredBy(cur, c) {
				retired = true
				cur.State, cur.MarkedAt = Marked, s.now()
				_, err = replaceJSON(ctx, s.Objects, entryKey(cur.RepositoryID, cur.ID), cur, gen)
				return err
			}
		}
		if lasting(cur.Scope) {
			if err := putJSON(ctx, s.Objects, familyKey(cur.RepositoryID, cur.Scope, cur.Platform, cur.Family), Family{
				RepositoryID: cur.RepositoryID, Scope: cur.Scope, Platform: cur.Platform, Family: cur.Family, LastUsed: s.now()}); err != nil {
				return err
			}
		}
		cur.State, cur.ReadyAt, cur.LastUsed = Ready, s.now(), s.now()
		if _, err := replaceJSON(ctx, s.Objects, entryKey(cur.RepositoryID, cur.ID), cur, gen); err != nil {
			return err
		}
		made = &cur
		return nil
	})
	if err != nil {
		return err
	}
	if retired {
		s.log().Info("cache entry deleted: its pull request closed after its run was bound", "repository_id", e.RepositoryID, "entry", e.ID)
		cur, _, _, err := getJSON[Entry](ctx, s.Objects, entryKey(e.RepositoryID, e.ID))
		if err != nil {
			return err
		}
		return s.deleteMarked(ctx, cur)
	}
	if made == nil {
		return nil
	}
	s.log().Info("cache entry ready", "repository_id", made.RepositoryID, "entry", made.ID, "scope", made.Scope,
		"platform", made.Platform, "family", made.Family, "key", made.Key)
	if err := s.supersede(ctx, *made); err != nil {
		// The pass deletes them later.
		s.log().Warn("deleting what a ready cache entry replaces", "entry", made.ID, "err", err.Error())
	}
	return nil
}

// replaces reports whether a ready entry, older than a newly ready one, is
// one the new one replaces: an older version of its series, or in a pull
// request's scope, any of its entries of the platform and family, whatever
// its key, since a pull request keeps one of each.
func replaces(newer, v Entry) bool {
	if v.State != Ready || v.ID == newer.ID || v.Scope != newer.Scope || v.Platform != newer.Platform ||
		v.Family != newer.Family || !olderThan(v, newer) {
		return false
	}
	return isPullRequest(newer.Scope) || v.Key == newer.Key
}

// supersede deletes what a newly ready entry replaces, except what an
// unfinished run's choice names. A pull request's entry committed by a run
// bound before its pull request was last seen closed replaces nothing:
// that's read again under the lease, as the closure may be recorded
// meanwhile.
func (s *Service) supersede(ctx context.Context, newer Entry) error {
	recs, err := listJSON[Entry](ctx, s.Objects, entryPrefix(newer.RepositoryID))
	if err != nil {
		return err
	}
	var older []record[Entry]
	for k, r := range recs {
		if replaces(newer, r.v) {
			older = append(older, record[Entry]{r.v, recs[k].gen})
		}
	}
	if len(older) == 0 {
		return nil
	}
	marked, err := s.mark(ctx, newer.RepositoryID, older, func(ctx context.Context, _ Entry) (bool, error) {
		return s.stillReplaces(ctx, newer)
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range marked {
		s.log().Info("cache entry deleted: a newer one replaces it", "entry", v.ID, "newer", newer.ID, "scope", v.Scope)
		errs = append(errs, s.deleteMarked(ctx, v))
	}
	return errors.Join(errs...)
}

// stillReplaces reports, under the lease, whether an entry still replaces
// others: one of a pull request's that its closure retires replaces
// nothing.
func (s *Service) stillReplaces(ctx context.Context, by Entry) (bool, error) {
	if !isPullRequest(by.Scope) {
		return true, nil
	}
	c, err := s.Disks.ClosureOf(ctx, by.RepositoryID, by.Scope)
	if err != nil {
		return false, err
	}
	return !retiredBy(by, c), nil
}

// discard marks a pending entry for deletion, under the lease, and deletes
// it.
func (s *Service) discard(ctx context.Context, e Entry, why string) error {
	cur, gen, ok, err := getJSON[Entry](ctx, s.Objects, entryKey(e.RepositoryID, e.ID))
	if err != nil || !ok || cur.State == Ready {
		return err
	}
	if cur.State == Pending {
		marked, err := s.mark(ctx, cur.RepositoryID, []record[Entry]{{cur, gen}}, nil)
		if err != nil {
			return err
		}
		if len(marked) == 0 {
			return nil
		}
		cur = marked[0]
	}
	s.log().Info("cache entry deleted", "repository_id", cur.RepositoryID, "entry", cur.ID, "reason", why)
	return s.deleteMarked(ctx, cur)
}

// named are the entries any recorded choice of the consumer's names. A
// choice is dropped once its run has finished and has no automatic rerun
// left, so every choice there is names an entry an unfinished run may
// still need. It's read inside the lease.
func (s *Service) named(ctx context.Context, repo int64) (map[string]bool, error) {
	choices, err := listJSON[Choice](ctx, s.Objects, fmt.Sprintf("cache/choices/%d/", repo))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range choices {
		if c.v.Entry != "" {
			out[c.v.Entry] = true
		}
	}
	return out, nil
}

// mark marks entries for deletion under the consumer's short lease, except
// those a choice names, read inside the lease after the candidates were; it
// returns those it marked. Each is marked only at the generation its
// candidate was read at, when the decision to delete it was made, so a
// choice or use of it since, which rewrites the record, leaves it for a
// later pass. recheck, if set, is asked under the lease whether each
// candidate should still go.
func (s *Service) mark(ctx context.Context, repo int64, cands []record[Entry], recheck func(ctx context.Context, e Entry) (bool, error)) ([]Entry, error) {
	slices.SortFunc(cands, func(x, y record[Entry]) int { return strings.Compare(x.v.ID, y.v.ID) })
	var marked []Entry
	err := s.Disks.Step(ctx, repo, func(ctx context.Context) error {
		named, err := s.named(ctx, repo)
		if err != nil {
			return err
		}
		for _, c := range cands {
			e := c.v
			if named[e.ID] || e.State == Marked {
				continue
			}
			if recheck != nil {
				if ok, err := recheck(ctx, e); err != nil {
					return err
				} else if !ok {
					continue
				}
			}
			e.State, e.MarkedAt = Marked, s.now()
			if _, err := replaceJSON(ctx, s.Objects, entryKey(repo, e.ID), e, c.gen); errors.Is(err, store.ErrPrecondition) {
				continue
			} else if err != nil {
				return err
			}
			marked = append(marked, e)
		}
		return nil
	})
	return marked, err
}

// deleteMarked deletes a marked entry's object, then its record.
func (s *Service) deleteMarked(ctx context.Context, e Entry) error {
	if e.State != Marked {
		return fmt.Errorf("entry %s isn't marked for deletion", e.ID)
	}
	if e.Object != "" {
		a, err := s.Bucket.Attrs(ctx, e.Object)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			if err := s.Bucket.Delete(ctx, e.Object, a.Generation); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	_, gen, ok, err := getJSON[Entry](ctx, s.Objects, entryKey(e.RepositoryID, e.ID))
	if err != nil || !ok {
		return err
	}
	if err := s.Objects.Delete(ctx, entryKey(e.RepositoryID, e.ID), gen); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}
