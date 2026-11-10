package github

import (
	"cmp"
	"maps"
	"slices"
)

// State is the emulator's state, held still while Read's function runs:
// what another emulator that takes its view of GitHub from this one, such
// as emulators/service, reads, in-process, as the service reads GitHub's
// API. What its methods return is the emulator's own; it's read only inside
// Read's function, and never changed.
type State struct{ e *Emulator }

// Read calls f with the emulator's state held still. f only reads, and
// calls none of the emulator's other methods.
func (e *Emulator) Read(f func(s *State)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tick()
	f(&State{e: e})
}

// Repos are every repository, by ID.
func (s *State) Repos() []*Repo {
	out := slices.Collect(maps.Values(s.e.repos))
	slices.SortFunc(out, func(a, b *Repo) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Repo is the repository with an ID, or nil.
func (s *State) Repo(id int64) *Repo { return s.e.repos[id] }

// Branches are a repository's branches and their commits.
func (s *State) Branches(r *Repo) map[string]string { return maps.Clone(r.branches) }

// Reachable is every commit reachable from sha, itself included.
func (s *State) Reachable(sha string) map[string]bool {
	out := map[string]bool{}
	for c := range s.e.reachable(sha) {
		if _, ok := s.e.commits[c]; ok {
			out[c] = true
		}
	}
	return out
}

// File is a file's contents at a branch's commit.
func (s *State) File(r *Repo, branch, path string) (string, bool) {
	c := s.e.commits[r.branches[branch]]
	if c == nil {
		return "", false
	}
	v, ok := c.Files[path]
	return v, ok
}

// Pulls are a repository's pull requests, by number.
func (s *State) Pulls(r *Repo) []*Pull {
	out := slices.Collect(maps.Values(r.pulls))
	slices.SortFunc(out, func(a, b *Pull) int { return a.Number - b.Number })
	return out
}

// PullCommits are a pull request's commits, as GitHub counts them: those
// its head has that its base branch doesn't, every one, not only the 250
// its API lists.
func (s *State) PullCommits(p *Pull) []string {
	return s.e.between(p.Base.branches[p.BaseRef], p.HeadSHA)
}

// Runs are every workflow run, by ID.
func (s *State) Runs() []*Run {
	out := slices.Collect(maps.Values(s.e.runs))
	slices.SortFunc(out, func(a, b *Run) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Run is the run with an ID, or nil.
func (s *State) Run(id int64) *Run { return s.e.runs[id] }

// Job is the workflow job with an ID, or nil.
func (s *State) Job(id int64) *WorkflowJob { return s.e.jobs[id] }
