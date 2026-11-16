package machines

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/consumer"
	"github.com/lanescape/runners/internal/github"
)

// RunFiles is what the service read of a run's .github/runners.json, at its
// commit, once per run: the keys of every cache it declares
// (specs/0003-disks.md, Keys). It's recorded in the bucket as
// runs/<repository>/<run>/caches.json, so every job of the run, in every
// attempt, resolves the same.
type RunFiles struct {
	RepositoryID int64  `json:"repository_id"`
	RunID        int64  `json:"run_id"`
	Commit       string `json:"commit"`
	// Invalid is why the file isn't valid, if it isn't: every job that
	// relies on it is failed, saying so.
	Invalid string `json:"invalid,omitempty"`
	// Caches are each declared cache's key, or why it has none.
	Caches map[string]CacheKey `json:"caches,omitempty"`
}

// CacheKey is a cache's key for a run, or why it has none: a file it lists
// doesn't exist.
type CacheKey struct {
	Key     string `json:"key,omitempty"`
	Missing string `json:"missing,omitempty"`
}

func runFilesKey(repo, run int64) string { return fmt.Sprintf("runs/%d/%d/caches.json", repo, run) }

// errFiles means the run's files say the job can't run as it asked: the
// error's text is the reason it's told.
type errFiles struct{ reason string }

func (e errFiles) Error() string { return e.reason }

// runFiles reads, or the first time makes, a run's record of its files. An
// error that isn't errFiles means GitHub or the bucket gave no confirmed
// answer.
func (s *Service) runFiles(ctx context.Context, repo github.Repository, bd binding.Binding) (RunFiles, error) {
	key := runFilesKey(repo.ID, bd.RunID)
	if rf, _, ok, err := getJSON[RunFiles](ctx, s.Objects, key); err != nil || ok {
		return rf, err
	}
	rf := RunFiles{RepositoryID: repo.ID, RunID: bd.RunID, Commit: bd.Commit, Caches: map[string]CacheKey{}}
	data, err := s.GitHub.File(ctx, repo, consumer.RunnersFile, bd.Commit)
	switch {
	case errors.Is(err, github.ErrNotFound):
		data = []byte("{}")
	case err != nil:
		return RunFiles{}, err
	}
	f, err := consumer.Parse(data)
	if err != nil {
		rf.Invalid = err.Error()
	} else {
		names := make([]string, 0, len(f.Caches))
		for n := range f.Caches {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, cache := range names {
			d := f.Caches[cache]
			// Each file is hashed as it's read, so no more than one file's
			// contents is held at a time, however many the cache lists.
			var kh *consumer.KeyHash
			if d.From != nil {
				kh = consumer.NewKeyHash(cache)
			}
			missing := ""
			for _, p := range d.From {
				b, err := s.GitHub.File(ctx, repo, p, bd.Commit)
				if errors.Is(err, github.ErrNotFound) {
					missing = p
					break
				}
				if err != nil {
					if github.IsUnavailable(err) {
						return RunFiles{}, err
					}
					missing = p + ": " + err.Error()
					break
				}
				kh.Add(p, b)
			}
			if missing != "" {
				rf.Caches[cache] = CacheKey{Missing: missing}
				continue
			}
			key := consumer.NoKey
			if kh != nil {
				key = kh.Sum()
			}
			rf.Caches[cache] = CacheKey{Key: key}
		}
	}
	got, _, err := createOnce(ctx, s.Objects, key, rf)
	return got, err
}

// cacheKey is a cache's key for a run, or errFiles saying why the job that
// names it can't have it.
func cacheKey(rf RunFiles, cache string) (string, error) {
	if rf.Invalid != "" {
		return "", errFiles{consumer.RunnersFile + " at the run's commit isn't valid: " + strings.TrimPrefix(rf.Invalid, "invalid: ")}
	}
	fk, ok := rf.Caches[cache]
	if !ok {
		return "", errFiles{fmt.Sprintf("%s at the run's commit doesn't declare the cache %s", consumer.RunnersFile, cache)}
	}
	if fk.Missing != "" {
		return "", errFiles{fmt.Sprintf("the cache %s lists %s, which doesn't exist at the run's commit", cache, fk.Missing)}
	}
	return fk.Key, nil
}
