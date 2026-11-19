package disks

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/store"
)

// MaxVersionObjects is the most objects a version holds.
const MaxVersionObjects = machines.MaxCacheObjects

// noCaches is the answer of a service that keeps no caches.
var noCaches = machines.RefusedError{Reason: "this service keeps no caches"}

// Restore answers a running job's request to restore a cache
// (specs/0003-disks.md, Caches): its run's restore choice for the cache on
// the job's architecture, picked the first time one of the run's jobs asks,
// and, for a version, its objects and a token that reads them alone. m is
// resolved with the run's scope; key is the cache's key at the run's
// commit. It implements machines.Caches.
func (s *Service) Restore(ctx context.Context, m machines.Machine, a machines.Admission, cache, key string) (machines.CacheRestore, error) {
	if s.Caches == nil || s.Tokens == nil {
		return machines.CacheRestore{}, noCaches
	}
	v, step, err := s.restore(ctx, m, a, cache, key, true)
	if err != nil {
		return machines.CacheRestore{}, err
	}
	if v == nil {
		return machines.CacheRestore{Restored: step}, nil
	}
	token, expires, err := s.Tokens.Read(ctx, VersionPrefix(v.RepositoryID, v.Image))
	if err != nil {
		return machines.CacheRestore{}, fmt.Errorf("a token to read %s: %w", v.Image, err)
	}
	return machines.CacheRestore{Restored: step, Bucket: s.CacheBucket, Objects: v.Objects, Token: token, Expires: &expires}, nil
}

// Check answers a running job's request to check a cache
// (specs/0003-disks.md, Caches): the step of the restore order that a
// restore of it would come from now, the run's recorded choice if it stands,
// or else what the restore order picks, without recording a choice,
// touching a version or giving a token, so the run's first restore still
// chooses. It implements machines.Caches.
func (s *Service) Check(ctx context.Context, m machines.Machine, a machines.Admission, cache, key string) (machines.CacheCheck, error) {
	if s.Caches == nil || s.Tokens == nil {
		return machines.CacheCheck{}, noCaches
	}
	_, step, err := s.restore(ctx, m, a, cache, key, false)
	if err != nil {
		return machines.CacheCheck{}, err
	}
	return machines.CacheCheck{Found: step}, nil
}

// Save answers a running job's request to save a cache, in a scope that
// saves: the one version the job saves of it, recorded before anything of
// it is uploaded, and a token that may only create objects under its
// prefix. A repeat names the same version, with a fresh token, until it's
// committed; a job saves a cache once. It implements machines.Caches.
func (s *Service) Save(ctx context.Context, m machines.Machine, a machines.Admission, cache, key string) (machines.CacheSave, error) {
	if s.Caches == nil || s.Tokens == nil {
		return machines.CacheSave{}, noCaches
	}
	repo, sel := m.RepositoryID, m.Selector
	if !sel.Scope.Saves() {
		return machines.CacheSave{Saves: false, Reason: "the run's scope, " + sel.Scope.String() + ", saves nothing"}, nil
	}
	sv, _, err := createOnce(ctx, s.Objects, savingKey(repo, a.JobID, cache), Saving{
		Version: VersionName(repo, Series(sel.Scope.String(), sel.Arch, cache, key), s.now(), m.Name),
		JobID:   a.JobID, Cache: cache, At: s.now()})
	if err != nil {
		return machines.CacheSave{}, err
	}
	// When the run was bound, so that a pull request's version saved by a
	// run from before it closed is never chosen after it's reopened.
	var bound time.Time
	if b, _, found, err := getJSON[binding.Binding](ctx, s.Objects, binding.Key(repo, a.RunID)); err != nil {
		return machines.CacheSave{}, err
	} else if found {
		bound = b.BoundAt
	}
	rec, _, err := createOnce(ctx, s.Objects, versionKey(repo, sv.Version), Image{Image: sv.Version, RepositoryID: repo, Kind: KindCache,
		State: ImageMaking, Scope: sel.Scope.String(), Arch: sel.Arch, Cache: cache, Key: key, Machine: m.Name, Zone: m.Zone,
		InstanceID: m.ID, JobID: a.JobID, JobName: a.JobName, RunID: a.RunID, RunAttempt: a.RunAttempt, Selector: a.Selector,
		SavedAt: sv.At, BoundAt: bound})
	if err != nil {
		return machines.CacheSave{}, err
	}
	if rec.JobID != a.JobID || rec.Cache != cache {
		return machines.CacheSave{}, fmt.Errorf("version %s is recorded for another job's cache", rec.Image)
	}
	if rec.State != ImageMaking {
		return machines.CacheSave{}, machines.RefusedError{Reason: "this job has saved the cache " + cache + " already: a job saves a cache once"}
	}
	prefix := VersionPrefix(repo, rec.Image)
	token, expires, err := s.Tokens.Create(ctx, prefix)
	if err != nil {
		return machines.CacheSave{}, fmt.Errorf("a token to save %s: %w", rec.Image, err)
	}
	s.log().Info("cache saving", "repository_id", repo, "run_id", a.RunID, "job_id", a.JobID, "cache", cache, "key", key,
		"scope", rec.Scope, "arch", rec.Arch, "version", rec.Image)
	return machines.CacheSave{Saves: true, Bucket: s.CacheBucket, Prefix: prefix, Token: token, Expires: &expires}, nil
}

// Commit answers a running job's word that it has uploaded the version of
// a cache it's saving: the service lists what's under the version's prefix
// and records it, as Cloud Storage holds it, so that a restore reads those
// objects alone, at those generations. The version becomes ready once
// GitHub's conclusion for the job is success (settle). A repeat answers
// what was recorded. It implements machines.Caches.
func (s *Service) Commit(ctx context.Context, m machines.Machine, a machines.Admission, cache string) (machines.CacheCommit, error) {
	if s.Caches == nil || s.Tokens == nil {
		return machines.CacheCommit{}, noCaches
	}
	repo := m.RepositoryID
	sv, _, found, err := getJSON[Saving](ctx, s.Objects, savingKey(repo, a.JobID, cache))
	if err != nil {
		return machines.CacheCommit{}, err
	}
	if !found {
		return machines.CacheCommit{}, machines.RefusedError{Reason: "this job hasn't asked to save the cache " + cache}
	}
	rec, gen, found, err := getJSON[Image](ctx, s.Objects, versionKey(repo, sv.Version))
	if err != nil {
		return machines.CacheCommit{}, err
	}
	if !found || rec.JobID != a.JobID || rec.Kind != KindCache {
		return machines.CacheCommit{}, fmt.Errorf("version %s isn't recorded for the job; ask to save again", sv.Version)
	}
	switch rec.State {
	case ImageCommitted, ImageReady:
		return machines.CacheCommit{Objects: len(rec.Objects), Bytes: rec.Bytes}, nil
	case ImageMaking:
	default:
		return machines.CacheCommit{}, machines.RefusedError{Reason: "the cache " + cache + "'s version is no longer being saved"}
	}
	prefix := VersionPrefix(repo, rec.Image)
	objs, err := s.Caches.List(ctx, prefix)
	if err != nil {
		return machines.CacheCommit{}, err
	}
	if len(objs) == 0 {
		return machines.CacheCommit{}, machines.RefusedError{Reason: "nothing is uploaded under " + prefix + " for the cache " + cache}
	}
	rec.Objects, rec.Bytes = make([]machines.CacheObject, 0, len(objs)), 0
	for _, o := range objs {
		var crc [4]byte
		binary.BigEndian.PutUint32(crc[:], o.CRC32C)
		rec.Objects = append(rec.Objects, machines.CacheObject{Name: o.Name, Size: o.Size, CRC32C: base64.StdEncoding.EncodeToString(crc[:]),
			Generation: o.Generation})
		rec.Bytes += o.Size
	}
	if err := machines.CheckCacheObjects(cache, rec.Objects); err != nil {
		return machines.CacheCommit{}, err
	}
	rec.State, rec.CommittedAt = ImageCommitted, s.now()
	if _, err := replaceJSON(ctx, s.Objects, versionKey(repo, rec.Image), rec, gen); err != nil {
		return machines.CacheCommit{}, err
	}
	s.log().Info("cache committed", "repository_id", repo, "run_id", a.RunID, "job_id", a.JobID, "cache", cache, "version", rec.Image,
		"objects", len(rec.Objects), "bytes", rec.Bytes)
	return machines.CacheCommit{Objects: len(rec.Objects), Bytes: rec.Bytes}, nil
}

// deleteObjects deletes every object under a prefix of the caches' bucket.
func (s *Service) deleteObjects(ctx context.Context, prefix string) error {
	if s.Caches == nil {
		return nil
	}
	objs, err := s.Caches.List(ctx, prefix)
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range objs {
		if err := s.Caches.Delete(ctx, o.Name, o.Generation); err != nil && !errors.Is(err, store.ErrNotFound) &&
			!errors.Is(err, store.ErrPrecondition) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// settle decides the versions whose jobs have finished (specs/0003-disks.md,
// Caches): one committed by a job GitHub concludes succeeded, whose objects
// are all there, becomes ready, and replaces its scope's older versions of
// the cache on its architecture; any other, committed or not, goes. A version whose
// repository GitHub doesn't show as the organization's this pass waits.
func (s *Service) settle(ctx context.Context, st *state) {
	keys := make([]string, 0, len(st.records))
	for k, r := range st.records {
		if r.v.Kind == KindCache && (r.v.State == ImageMaking || r.v.State == ImageCommitted) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if ctx.Err() != nil {
			return
		}
		r := st.records[k]
		v := r.v
		done, err := s.finishedJob(ctx, st, v.RepositoryID, v.JobID)
		if err != nil {
			if !errors.Is(err, errNotShown) {
				s.log().Warn("reading a cache version's job", "version", v.Image, "err", err.Error())
			}
			continue
		}
		if !done {
			continue
		}
		j := st.jobs[v.JobID]
		succeeded := v.State == ImageCommitted && j != nil && j.Conclusion == "success"
		intact := st.intact(v)
		if succeeded && !intact {
			// Listed afresh before anything goes: a listing from before the
			// pass's work, by another instance's clock, can't show what was
			// uploaded since. One that can't be had decides nothing.
			gone, err := s.objectsGone(ctx, v)
			if err != nil {
				s.log().Warn("listing a cache version's objects", "version", v.Image, "err", err.Error())
				continue
			}
			intact = !gone
		}
		if succeeded && intact {
			v.State = ImageReady
			gen, err := replaceJSON(ctx, s.Objects, k, v, r.gen)
			if err != nil {
				s.log().Warn("recording a cache version ready", "version", v.Image, "err", err.Error())
				continue
			}
			st.records[k] = record[Image]{v, gen}
			s.log().Info("cache saved", "repository_id", v.RepositoryID, "run_id", v.RunID, "job_id", v.JobID, "cache", v.Cache,
				"key", v.Key, "scope", v.Scope, "arch", v.Arch, "version", v.Image, "objects", len(v.Objects), "bytes", v.Bytes)
			if err := s.supersede(ctx, v); err != nil {
				// The pass deletes them later.
				s.log().Warn("deleting older versions of a cache", "version", v.Image, "err", err.Error())
			}
			continue
		}
		why := "its job didn't succeed"
		switch {
		case v.State == ImageMaking:
			why = "its job finished without committing it"
		case j != nil && j.Conclusion == "success":
			why = "its objects are gone"
		}
		// Never ready, so never chosen: it's marked at the generation read,
		// under the disks' lease, and goes. GitHub must still show the
		// repository as the organization's once its job has been read: it's
		// read again under the lease, and a repository it no longer shows
		// loses nothing.
		var marked Image
		shown := false
		err = s.step(ctx, v.RepositoryID, func(ctx context.Context) error {
			if shown = s.stillShown(ctx, st, v.RepositoryID); !shown {
				return nil
			}
			var err error
			marked, err = s.markOne(ctx, v, r.gen)
			return err
		})
		if err != nil {
			s.log().Warn("marking an unsaved cache version", "version", v.Image, "err", err.Error())
			continue
		}
		if !shown {
			continue
		}
		st.records[k] = record[Image]{v: marked}
		if err := s.deleteMarked(ctx, marked); err != nil {
			s.log().Warn("deleting an unsaved cache version", "version", v.Image, "err", err.Error())
			continue
		}
		delete(st.records, k)
		s.log().Info("cache version not saved", "repository_id", v.RepositoryID, "job_id", v.JobID, "cache", v.Cache,
			"version", v.Image, "reason", why)
	}
}

// versionOf is the version a caches' bucket object belongs to, and its
// repository, if it's named as the service names them.
func versionOf(object string) (int64, string, bool) {
	repo, rest, ok := strings.Cut(object, "/")
	if !ok {
		return 0, "", false
	}
	version, _, ok := strings.Cut(rest, "/")
	m := savedName.FindStringSubmatch(version)
	if !ok || m == nil || m[1] != repo {
		return 0, "", false
	}
	var id int64
	if _, err := fmt.Sscan(repo, &id); err != nil {
		return 0, "", false
	}
	return id, version, true
}
