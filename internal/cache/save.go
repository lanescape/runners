package cache

import (
	"context"
	"errors"
	"time"

	"github.com/lanescape/runners/internal/store"
)

type saveRequest struct {
	Family   string `json:"family"`
	Key      string `json:"key"`
	Platform string `json:"platform"`
	Store    string `json:"store"`
	// Size and SHA256 are an index entry's: what the job keeps in its own
	// runner's cache. The service's store's are given at commit.
	Size   *int64 `json:"size"`
	SHA256 string `json:"sha256"`
}

// The headers a reservation's URL is signed with, which the caller sends
// exactly: it can only start a resumable upload that creates its object,
// once.
var uploadHeaders = map[string]string{"x-goog-resumable": "start", "x-goog-if-generation-match": "0"}

// save is POST /v1/cache/save: it reserves an entry for the job, in its
// run's own scope. In the service's store, it hands out a signed URL that
// starts a resumable upload of the reservation's one object; in an index,
// it records the size and SHA-256 the job gives, and nothing is uploaded.
func (s *Service) save(ctx context.Context, c *caller, body []byte) (any, error) {
	var q saveRequest
	if err := decode(body, &q, "save"); err != nil {
		return nil, err
	}
	index := q.Store != ServiceStore
	if !familyPattern.MatchString(q.Family) || !keyPattern.MatchString(q.Key) || !platformPattern.MatchString(q.Platform) ||
		!storePattern.MatchString(q.Store) {
		return nil, refuse("the body isn't a save request: its family, key, platform or store isn't the interface's")
	}
	if index != (q.Size != nil && *q.Size >= 0 && sha256Pattern.MatchString(q.SHA256)) || (!index && (q.Size != nil || q.SHA256 != "")) {
		return nil, refuse("the body isn't a save request: an index's entry has its size and sha256, and the %s store's has neither", ServiceStore)
	}
	if err := s.storeOpen(ctx, c, q.Store); err != nil {
		return nil, err
	}
	if !c.binding.Scope.Saves() {
		return nil, refuse("the run's scope, %s, saves nothing", c.binding.Scope)
	}
	now := s.now()
	r := Reservation{ID: newID(), RepositoryID: c.repo.ID, RunID: c.claims.RunID, RunAttempt: c.claims.RunAttempt,
		JobID: c.claims.CheckRunID, Scope: c.binding.Scope.String(), Store: q.Store, Platform: q.Platform, Family: q.Family, Key: q.Key,
		BoundAt: c.binding.BoundAt, Created: now, Expires: now.Add(ReserveFor)}
	if index {
		r.Size, r.SHA256 = *q.Size, q.SHA256
	}
	if _, _, err := createOnce(ctx, s.Objects, reservationKey(r.RepositoryID, r.ID), r); err != nil {
		return nil, err
	}
	if index {
		s.log().Info("cache index entry reserved", "repository_id", r.RepositoryID, "run_id", r.RunID, "job_id", r.JobID,
			"reservation", r.ID, "scope", r.Scope, "store", r.Store, "platform", r.Platform, "family", r.Family, "key", r.Key)
		return map[string]any{"id": r.ID, "expires": r.Expires.Format(time.RFC3339)}, nil
	}
	var headers []string
	for _, k := range []string{"x-goog-if-generation-match", "x-goog-resumable"} {
		headers = append(headers, k+":"+uploadHeaders[k])
	}
	u, err := s.Signer.SignedURL(ctx, UploadObject(r.RepositoryID, r.ID), "POST", nil, headers, r.Expires)
	if err != nil {
		return nil, err
	}
	s.log().Info("cache upload reserved", "repository_id", r.RepositoryID, "run_id", r.RunID, "job_id", r.JobID, "reservation", r.ID,
		"scope", r.Scope, "platform", r.Platform, "family", r.Family, "key", r.Key)
	return map[string]any{"id": r.ID, "url": u, "headers": uploadHeaders, "expires": r.Expires.Format(time.RFC3339)}, nil
}

type commitRequest struct {
	ID     string `json:"id"`
	Size   *int64 `json:"size"`
	SHA256 string `json:"sha256"`
}

// commit is POST /v1/cache/commit (specs/0006-cache.md, Saving): from the
// job that reserved the upload, before its reservation expires, it checks
// that the upload is there with exactly the size given, records the entry
// as pending, with when its run was bound, copies the upload within the
// bucket to the entry's object, records the copy's generation, and deletes
// the upload. The entry becomes restorable only once GitHub shows its job
// succeeded. A commit tried again carries on where the last stopped.
func (s *Service) commit(ctx context.Context, c *caller, body []byte) (any, error) {
	var q commitRequest
	if err := decode(body, &q, "commit"); err != nil {
		return nil, err
	}
	if !idPattern.MatchString(q.ID) || q.Size == nil || *q.Size < 0 || !sha256Pattern.MatchString(q.SHA256) {
		return nil, refuse("the body isn't a commit request: its id, size or sha256 isn't the interface's")
	}
	repo := c.repo.ID
	key := reservationKey(repo, q.ID)
	var r Reservation
	for attempt := 0; ; attempt++ {
		got, gen, ok, err := getJSON[Reservation](ctx, s.Objects, key)
		if err != nil {
			return nil, err
		}
		if !ok || got.RepositoryID != repo {
			return nil, refuse("there's no such reservation")
		}
		if got.JobID != c.claims.CheckRunID || got.RunID != c.claims.RunID {
			return nil, refuse("the reservation isn't this job's")
		}
		r = got
		if r.Entry != "" {
			if r.Size != *q.Size || r.SHA256 != q.SHA256 {
				return nil, refuse("the reservation was committed with another size or SHA-256")
			}
			break
		}
		if !s.now().Before(r.Expires) {
			s.dropUpload(ctx, r)
			return nil, refuse("the reservation has expired")
		}
		if r.Store != ServiceStore {
			// An index's entry: nothing was uploaded, and the commit says
			// again what the save did.
			if r.Size != *q.Size || r.SHA256 != q.SHA256 {
				return nil, refuse("the commit's size or SHA-256 isn't the save's")
			}
			r.Entry = newID()
			if _, err = replaceJSON(ctx, s.Objects, key, r, gen); err == nil {
				break
			}
			if !errors.Is(err, store.ErrPrecondition) || attempt == 2 {
				return nil, err
			}
			continue
		}
		up, err := s.Bucket.Attrs(ctx, UploadObject(repo, r.ID))
		if errors.Is(err, store.ErrNotFound) {
			return nil, refuse("nothing has been uploaded for the reservation")
		}
		if err != nil {
			return nil, err
		}
		if up.Size != *q.Size {
			return nil, refuse("the upload's size, %d, isn't the commit's", up.Size)
		}
		r.Entry, r.Size, r.SHA256, r.UploadGeneration = newID(), *q.Size, q.SHA256, up.Generation
		_, err = replaceJSON(ctx, s.Objects, key, r, gen)
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrPrecondition) || attempt == 2 {
			return nil, err
		}
	}
	series := Series(r.Scope, r.Store, r.Platform, r.Family, r.Key)
	object := ""
	if r.Store == ServiceStore {
		object = EntryObject(repo, series, r.Entry)
	}
	e, egen, err := createOnce(ctx, s.Objects, entryKey(repo, r.Entry), Entry{ID: r.Entry, RepositoryID: repo, Scope: r.Scope,
		Store: r.Store, Platform: r.Platform, Family: r.Family, Key: r.Key, Series: series, State: Pending, Object: object,
		Size: r.Size, SHA256: r.SHA256, JobID: r.JobID, RunID: r.RunID, RunAttempt: r.RunAttempt, Reservation: r.ID,
		BoundAt: r.BoundAt, CommittedAt: s.now()})
	if err != nil {
		return nil, err
	}
	if e.State != Pending {
		return nil, refuse("the entry this reservation made is %s", e.State)
	}
	if e.Object != "" && e.Generation == 0 {
		a, err := s.Bucket.CopyNew(ctx, UploadObject(repo, r.ID), r.UploadGeneration, e.Object)
		if errors.Is(err, store.ErrPrecondition) || errors.Is(err, store.ErrNotFound) {
			// Copied by an earlier try, whose answer was lost; or the
			// upload is gone, which only that copy's success deletes.
			a, err = s.Bucket.Attrs(ctx, e.Object)
			if errors.Is(err, store.ErrNotFound) {
				return nil, refuse("nothing has been uploaded for the reservation")
			}
		}
		if err != nil {
			return nil, err
		}
		if a.Size != e.Size {
			return nil, refuse("the copied entry's size isn't the commit's")
		}
		e.Generation = a.Generation
		if _, err := replaceJSON(ctx, s.Objects, entryKey(repo, e.ID), e, egen); err != nil {
			return nil, err
		}
	}
	if e.Object != "" {
		s.dropUpload(ctx, r)
	}
	s.log().Info("cache entry committed", "repository_id", repo, "run_id", r.RunID, "job_id", r.JobID, "entry", e.ID,
		"scope", e.Scope, "store", e.Store, "platform", e.Platform, "family", e.Family, "key", e.Key, "size", e.Size)
	return map[string]any{"state": Pending}, nil
}

// dropUpload deletes whatever is at a reservation's upload's name, best
// effort: a pass deletes anything it leaves.
func (s *Service) dropUpload(ctx context.Context, r Reservation) {
	name := UploadObject(r.RepositoryID, r.ID)
	a, err := s.Bucket.Attrs(ctx, name)
	if err != nil {
		return
	}
	if err := s.Bucket.Delete(ctx, name, a.Generation); err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrPrecondition) {
		s.log().Warn("deleting an upload", "reservation", r.ID, "err", err.Error())
	}
}
