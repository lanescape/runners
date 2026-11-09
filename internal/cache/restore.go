package cache

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/store"
)

type restoreRequest struct {
	Family   string `json:"family"`
	Key      string `json:"key"`
	Platform string `json:"platform"`
	Store    string `json:"store"`
	Lookup   bool   `json:"lookup"`
}

func (q restoreRequest) valid() bool {
	return familyPattern.MatchString(q.Family) && keyPattern.MatchString(q.Key) && platformPattern.MatchString(q.Platform) &&
		storePattern.MatchString(q.Store)
}

// restore is POST /v1/cache/restore: the run's choice for the store,
// platform, family and key, picked and recorded the first time a job of the
// run asks. An entry of the service's store comes with a signed URL to read
// it, unless it's a lookup, and only to a job on one of the service's own
// machines; an index's comes without, for the job to check the bytes it
// finds in its own runner's cache against.
func (s *Service) restore(ctx context.Context, c *caller, body []byte) (any, error) {
	var q restoreRequest
	if err := decode(body, &q, "restore"); err != nil {
		return nil, err
	}
	if !q.valid() {
		return nil, refuse("the body isn't a restore request: its family, key, platform or store isn't the interface's")
	}
	if err := s.storeOpen(ctx, c, q.Store); err != nil {
		return nil, err
	}
	e, step, err := s.choose(ctx, c, q)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return map[string]any{"entry": nil}, nil
	}
	out := map[string]any{"scope": e.Scope, "key": e.Key, "step": step, "size": e.Size, "sha256": e.SHA256}
	if !q.Lookup && e.Store == ServiceStore {
		expires := s.now().Add(RestoreFor)
		u, err := s.Signer.SignedURL(ctx, e.Object, "GET", url.Values{"generation": {strconv.FormatInt(e.Generation, 10)}}, nil, expires)
		if err != nil {
			return nil, err
		}
		out["url"], out["expires"] = u, expires.Format(time.RFC3339)
	}
	return map[string]any{"entry": out}, nil
}

// errNotReady means an entry is no longer ready to be chosen.
var errNotReady = errors.New("the entry is no longer ready")

// choose is the run's choice for a platform, family and key, picked and
// recorded under the consumer's short lease the first time, then the same
// for every request of the run, until an attempt the service's automatic
// reruns didn't start picks again. It returns nil for none.
func (s *Service) choose(ctx context.Context, c *caller, q restoreRequest) (*Entry, string, error) {
	repo, run, attempt := c.repo.ID, c.claims.RunID, c.claims.RunAttempt
	key := choiceKey(repo, run, q.Store, q.Platform, q.Family, q.Key)
	var out *Entry
	var step string
	err := s.Disks.Step(ctx, repo, func(ctx context.Context) error {
		ch, gen, ok, err := getJSON[Choice](ctx, s.Objects, key)
		if err != nil {
			return err
		}
		stale := false
		if ok && attempt > ch.Attempt {
			if stale, err = s.Disks.RerunBySomeoneElse(ctx, repo, run, int(ch.Attempt), int(attempt)); err != nil {
				return err
			}
		}
		if ok && !stale {
			if ch.Entry == "" {
				step = ch.Step
				return nil
			}
			e, err := s.touch(ctx, repo, ch.Entry, !q.Lookup)
			if err == nil {
				out, step = &e, ch.Step
				return nil
			}
			if !errors.Is(err, errNotReady) {
				return err
			}
			// Its entry is gone, which only happens if someone else
			// deleted it: the run picks again.
			s.log().Warn("a run's cache choice is gone; picking again", "repository_id", repo, "run_id", run, "entry", ch.Entry)
		}
		pick, pickStep, err := s.pick(ctx, c, q)
		if err != nil {
			return err
		}
		choice := Choice{RepositoryID: repo, RunID: run, Scope: c.binding.Scope.String(), Store: q.Store, Platform: q.Platform,
			Family: q.Family, Key: q.Key, Step: pickStep, Attempt: attempt, At: s.now()}
		if pick != nil {
			choice.Entry = pick.ID
		}
		// A pull request's most recent choice outlives its run's, so a
		// lasting scope keeps the key it names while the pull request is
		// open. It's written before the entry is touched: a pass that read
		// the entry's record before the touch fails to mark it, and one that
		// read it after reads this too.
		if _, isPR := c.binding.Scope.PullRequestNumber(); isPR {
			rec := Recent{RepositoryID: repo, Scope: choice.Scope, Store: q.Store, Platform: q.Platform, Family: q.Family, Step: pickStep,
				RunID: run, At: s.now()}
			if pick != nil {
				rec.Entry, rec.EntryScope, rec.Key = pick.ID, pick.Scope, pick.Key
			}
			if err := putJSON(ctx, s.Objects, recentKey(repo, rec.Scope, rec.Store, rec.Platform, rec.Family), rec); err != nil {
				return err
			}
		}
		if ok {
			_, err = replaceJSON(ctx, s.Objects, key, choice, gen)
		} else {
			_, _, err = createOnce(ctx, s.Objects, key, choice)
		}
		if err != nil {
			return err
		}
		if pick != nil {
			e, err := s.touch(ctx, repo, pick.ID, !q.Lookup)
			if err != nil {
				// Not chosen after all: the next request picks again.
				return err
			}
			pick = &e
		}
		// What each restore found, so the order and what's kept can be
		// tuned from evidence. Nothing here is a URL, a token or anything
		// of the entry's contents.
		s.log().Info("cache restore choice", "repository_id", repo, "run_id", run, "scope", choice.Scope, "store", q.Store, "platform", q.Platform,
			"family", q.Family, "key", q.Key, "step", pickStep, "entry", choice.Entry)
		out, step = pick, pickStep
		return nil
	})
	return out, step, err
}

// pick is the first entry, in the restore order, of the run's restore
// chain (specs/0006-cache.md, Restoring): each scope's ready entry for the
// platform, family and key, then its newest of the platform and family,
// whatever its key. A pull request's entry committed by a run bound before
// it was last seen closed is never chosen.
func (s *Service) pick(ctx context.Context, c *caller, q restoreRequest) (*Entry, string, error) {
	recs, err := listJSON[Entry](ctx, s.Objects, entryPrefix(c.repo.ID))
	if err != nil {
		return nil, "", err
	}
	var closure *disks.Closure
	if _, isPR := c.binding.Scope.PullRequestNumber(); isPR {
		if closure, err = s.Disks.ClosureOf(ctx, c.repo.ID, c.binding.Scope.String()); err != nil {
			return nil, "", err
		}
	}
	var pool []Entry
	for _, r := range recs {
		e := r.v
		if e.State == Ready && (e.Object == "" || e.Generation > 0) && e.Store == q.Store && e.Platform == q.Platform && e.Family == q.Family &&
			!disks.Retired(e.Scope, e.BoundAt, closure) {
			pool = append(pool, e)
		}
	}
	slices.SortFunc(pool, func(x, y Entry) int { return cmp.Or(y.CommittedAt.Compare(x.CommittedAt), strings.Compare(y.ID, x.ID)) })
	for _, sc := range c.binding.Chain() {
		exact, newest := disks.Steps(sc)
		for _, e := range pool {
			if e.Scope == sc.String() && e.Key == q.Key {
				return &e, exact, nil
			}
		}
		for _, e := range pool {
			if e.Scope == sc.String() {
				return &e, newest, nil
			}
		}
	}
	return nil, disks.StepNone, nil
}

// touch rewrites a ready entry's record, as a choice or a reading of one
// does, so that any mark of it made from an earlier reading fails. A use,
// unlike a lookup, also marks it used, at most once a day, in the same
// write, after its lasting scope's family record. It returns errNotReady if
// the entry isn't ready.
func (s *Service) touch(ctx context.Context, repo int64, id string, use bool) (Entry, error) {
	key := entryKey(repo, id)
	for range 3 {
		e, gen, ok, err := getJSON[Entry](ctx, s.Objects, key)
		if err != nil {
			return Entry{}, err
		}
		if !ok || e.State != Ready || (e.Object != "" && e.Generation == 0) {
			return Entry{}, errNotReady
		}
		e.TouchedAt = s.now()
		if use && day(e.LastUsed) != day(s.now()) {
			if lasting(e.Scope) {
				if err := putJSON(ctx, s.Objects, familyKey(repo, e.Scope, e.Store, e.Platform, e.Family), Family{RepositoryID: repo,
					Scope: e.Scope, Store: e.Store, Platform: e.Platform, Family: e.Family, LastUsed: s.now()}); err != nil {
					return Entry{}, err
				}
			}
			e.LastUsed = s.now()
		}
		if _, err := replaceJSON(ctx, s.Objects, key, e, gen); err == nil {
			return e, nil
		} else if !errors.Is(err, store.ErrPrecondition) {
			return Entry{}, err
		}
	}
	return Entry{}, errors.New("an entry's record kept changing")
}
