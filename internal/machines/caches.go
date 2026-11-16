package machines

import (
	"context"
	"errors"
	"net/http"

	"github.com/lanescape/runners/internal/selector"
)

// The caches' endpoints (specs/0005-interface.md, Caches).
const (
	CacheRestorePath = "/v1/caches/restore"
	CacheSavePath    = "/v1/caches/save"
	CacheCommitPath  = "/v1/caches/commit"
)

// CacheRestoreRequest is POST /v1/caches/restore.
func (s *Service) CacheRestoreRequest() http.Handler { return http.HandlerFunc(s.cacheRestore) }

// CacheSaveRequest is POST /v1/caches/save.
func (s *Service) CacheSaveRequest() http.Handler { return http.HandlerFunc(s.cacheSave) }

// CacheCommitRequest is POST /v1/caches/commit.
func (s *Service) CacheCommitRequest() http.Handler { return http.HandlerFunc(s.cacheCommit) }

// cacheRequest authenticates a running job's cache request and reads the
// cache it names, and, with withKey, the cache's key at the run's commit,
// which a cache must be declared to have. It answers any request it can't
// go on with itself, and reports whether the caller should go on.
func (s *Service) cacheRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, withKey bool) (runningJob, string, string, bool) {
	var body struct {
		Cache string `json:"cache"`
	}
	j, ok := s.activeJob(ctx, w, r, &body)
	if !ok {
		return runningJob{}, "", "", false
	}
	if s.Caches == nil {
		refuse(w, "this service keeps no caches")
		return runningJob{}, "", "", false
	}
	if !selector.ValidName(body.Cache) {
		refuse(w, "the body names no cache: 1 to 32 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen")
		return runningJob{}, "", "", false
	}
	if !withKey {
		return j, body.Cache, "", true
	}
	rf, err := s.runFiles(ctx, j.Repository, j.Binding)
	if err != nil {
		s.log().Warn("reading the run's files", "machine", j.Machine.Name, "run_id", j.Admission.RunID, "err", err.Error())
		retryLater(w, "the run's files couldn't be read; try again")
		return runningJob{}, "", "", false
	}
	key, err := cacheKey(rf, body.Cache)
	var ef errFiles
	if errors.As(err, &ef) {
		s.log().Info("cache request refused", "machine", j.Machine.Name, "run_id", j.Admission.RunID, "cache", body.Cache, "reason", ef.reason)
		refuse(w, ef.reason)
		return runningJob{}, "", "", false
	}
	return j, body.Cache, key, true
}

// cacheAnswer answers a cache request from what Caches said: its answer, a
// refusal with its reason, or, for any other error, a retry.
func (s *Service) cacheAnswer(w http.ResponseWriter, j runningJob, what, cache string, answer any, err error) {
	var refused RefusedError
	switch {
	case errors.As(err, &refused):
		refuse(w, refused.Reason)
	case err != nil:
		s.log().Warn("a cache request", "what", what, "machine", j.Machine.Name, "run_id", j.Admission.RunID, "cache", cache,
			"err", err.Error())
		retryLater(w, "the cache couldn't be "+what+"; try again")
	default:
		writeJSON(w, http.StatusOK, answer)
	}
}

func (s *Service) cacheRestore(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), diskTimeout)
	defer cancel()
	j, cache, key, ok := s.cacheRequest(ctx, w, r, true)
	if !ok {
		return
	}
	got, err := s.Caches.Restore(ctx, j.Admission.Resolve(j.Machine), j.Admission, cache, key)
	s.cacheAnswer(w, j, "chosen", cache, got, err)
}

func (s *Service) cacheSave(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), diskTimeout)
	defer cancel()
	j, cache, key, ok := s.cacheRequest(ctx, w, r, true)
	if !ok {
		return
	}
	if a := j.Admission; !a.Scope.Saves() {
		writeJSON(w, http.StatusOK, CacheSave{Saves: false, Reason: "the run's scope, " + a.Scope.String() + ", saves nothing"})
		return
	}
	got, err := s.Caches.Save(ctx, j.Admission.Resolve(j.Machine), j.Admission, cache, key)
	s.cacheAnswer(w, j, "saved", cache, got, err)
}

func (s *Service) cacheCommit(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), diskTimeout)
	defer cancel()
	j, cache, _, ok := s.cacheRequest(ctx, w, r, false)
	if !ok {
		return
	}
	got, err := s.Caches.Commit(ctx, j.Admission.Resolve(j.Machine), j.Admission, cache)
	s.cacheAnswer(w, j, "committed", cache, got, err)
}
