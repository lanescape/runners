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
// commit, once per run: the keys of every family it declares
// (specs/0003-disks.md, Keys). It's recorded in the
// bucket as runs/<repository>/<run>/files.json, so every job of the run,
// in every attempt, resolves the same.
type RunFiles struct {
	RepositoryID int64  `json:"repository_id"`
	RunID        int64  `json:"run_id"`
	Commit       string `json:"commit"`
	// Invalid is why the file isn't valid, if it isn't: every job that
	// relies on it is failed, saying so.
	Invalid string `json:"invalid,omitempty"`
	// Families are each declared family's key, or why it has none.
	Families map[string]FamilyKey `json:"families,omitempty"`
}

// FamilyKey is a family's key for a run, or why it has none: a file it
// lists doesn't exist.
type FamilyKey struct {
	Key     string `json:"key,omitempty"`
	Missing string `json:"missing,omitempty"`
}

func runFilesKey(repo, run int64) string { return fmt.Sprintf("runs/%d/%d/files.json", repo, run) }

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
	rf := RunFiles{RepositoryID: repo.ID, RunID: bd.RunID, Commit: bd.Commit, Families: map[string]FamilyKey{}}
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
		names := make([]string, 0, len(f.Disks))
		for n := range f.Disks {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, family := range names {
			d := f.Disks[family]
			var files []consumer.PathContents
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
				files = append(files, consumer.PathContents{Path: p, Contents: b})
			}
			if missing != "" {
				rf.Families[family] = FamilyKey{Missing: missing}
				continue
			}
			rf.Families[family] = FamilyKey{Key: consumer.Key(family, files, d.From != nil)}
		}
	}
	got, _, err := createOnce(ctx, s.Objects, key, rf)
	return got, err
}

// familyKey is a family's key for a run, or errFiles saying why the job
// that names it can't run.
func familyKey(rf RunFiles, family string) (string, error) {
	if rf.Invalid != "" {
		return "", errFiles{consumer.RunnersFile + " at the run's commit isn't valid: " + strings.TrimPrefix(rf.Invalid, "invalid: ")}
	}
	fk, ok := rf.Families[family]
	if !ok {
		return "", errFiles{fmt.Sprintf("%s at the run's commit doesn't declare the family %s", consumer.RunnersFile, family)}
	}
	if fk.Missing != "" {
		return "", errFiles{fmt.Sprintf("the family %s lists %s, which doesn't exist at the run's commit", family, fk.Missing)}
	}
	return fk.Key, nil
}
