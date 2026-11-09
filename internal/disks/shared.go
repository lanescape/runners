package disks

import (
	"context"
	"time"
)

// What the cache shares with saved disks (specs/0006-cache.md, Deleting):
// the consumer's short lease, the closures of pull requests, and whether a
// run has settled, with its automatic reruns.

// Step runs f under the consumer's short lease, the disks', held only for
// f, which every step of choosing, reading a choice and marking takes, the
// cache's as well as the disks'.
func (s *Service) Step(ctx context.Context, repo int64, f func(ctx context.Context) error) error {
	return s.step(ctx, repo, f)
}

// SeenClosed records that a pull request scope's pull request was seen
// closed now, under the lease, moving its closure only forward, and returns
// what's recorded. closedAt is GitHub's, if known.
func (s *Service) SeenClosed(ctx context.Context, repo int64, sc string, closedAt time.Time) (Closure, error) {
	return s.closeScope(ctx, repo, sc, s.now(), closedAt)
}

// ClosureOf is a pull request scope's closure, if it has one.
func (s *Service) ClosureOf(ctx context.Context, repo int64, sc string) (*Closure, error) {
	c, _, found, err := getJSON[Closure](ctx, s.Objects, closureKey(repo, sc))
	if err != nil || !found {
		return nil, err
	}
	return &c, nil
}

// Retired reports whether something a pull request's run bound at boundAt
// saved in scope sc is retired by its closure: saved by a run bound before
// the pull request was last seen closed, it's never chosen again and
// replaces nothing.
func Retired(sc string, boundAt time.Time, c *Closure) bool {
	return c != nil && c.Scope == sc && !boundAt.After(c.SeenAt)
}

// Unreported are the runs, by ID, with a machine whose job was admitted and
// that hasn't reported: they count as unfinished.
func (s *Service) Unreported(ctx context.Context) (map[int64]bool, error) {
	return s.unreported(ctx, "")
}

// Settled reports whether a run has finished with no automatic rerun it is,
// or may yet be, due, read afresh from GitHub and the bucket. A run among
// unreported hasn't.
func (s *Service) Settled(ctx context.Context, repo, runID int64, unreported map[int64]bool) (bool, error) {
	return s.settled(ctx, repo, runID, unreported)
}

// RerunBySomeoneElse reports whether any attempt of a run after from, up to
// to, was triggered by anyone but the App's own bot, by GitHub's record of
// each: a choice made in attempt from holds for to only if not.
func (s *Service) RerunBySomeoneElse(ctx context.Context, repo, runID int64, from, to int) (bool, error) {
	return s.rerunBySomeoneElse(ctx, repo, runID, from, to)
}

// ClosureKey is where a pull request scope's closure is kept.
func ClosureKey(repo int64, sc string) string { return closureKey(repo, sc) }
