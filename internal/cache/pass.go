package cache

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/store"
)

// retiredBy reports whether a pull request's closure retires an entry: one
// committed by a run bound before the pull request was last seen closed.
func retiredBy(e Entry, c *disks.Closure) bool { return disks.Retired(e.Scope, e.BoundAt, c) }

// state is what the cache's part of a pass works from: the cache bucket's
// objects, listed before the records that describe them, since every record
// is written before what it describes.
type state struct {
	uploads, objects map[string]store.Object
	entries          map[string]record[Entry]
	reservations     map[string]record[Reservation]
	choices          map[string]record[Choice]
	recents          map[string]record[Recent]
	families         map[string]record[Family]
	closures         map[string]record[disks.Closure]
	complete         bool

	repos map[int64]*github.Repository
	// settled are runs found finished with no automatic rerun left.
	settled map[string]bool
}

// Pass is the cache's part of a pass (specs/0006-cache.md, Deleting), after
// the disks': it settles pending entries by their jobs, drops the choices of
// runs that need them no more, and deletes entries, uploads and records as
// early as their rules allow. A failure on one doesn't stop the rest.
func (s *Service) Pass(p *pass.Pass) error {
	ctx := p.Context()
	st := s.read(ctx)
	if p.Lost() {
		return context.Cause(ctx)
	}
	s.settleAll(ctx, st)
	if st.complete {
		s.dropChoices(ctx, st)
		s.expire(ctx, st)
		s.tidy(ctx, st)
	} else {
		s.log().Warn("deleting no cache entries this pass: the bucket's objects or the records couldn't all be read")
	}
	if p.Lost() {
		return context.Cause(ctx)
	}
	return nil
}

func (s *Service) read(ctx context.Context) *state {
	st := &state{complete: true, uploads: map[string]store.Object{}, objects: map[string]store.Object{},
		repos: map[int64]*github.Repository{}, settled: map[string]bool{}}
	fail := func(what string, err error) {
		s.log().Error("listing the cache's "+what, "err", err.Error())
		st.complete = false
	}
	for prefix, into := range map[string]map[string]store.Object{"uploads/": st.uploads, "entries/": st.objects} {
		objs, err := s.Bucket.List(ctx, prefix)
		if err != nil {
			fail(prefix, err)
		}
		for _, o := range objs {
			into[o.Name] = o
		}
	}
	var err error
	if st.entries, err = listJSON[Entry](ctx, s.Objects, "cache/entries/"); err != nil {
		fail("entries", err)
	}
	if st.reservations, err = listJSON[Reservation](ctx, s.Objects, "cache/reservations/"); err != nil {
		fail("reservations", err)
	}
	if st.choices, err = listJSON[Choice](ctx, s.Objects, "cache/choices/"); err != nil {
		fail("choices", err)
	}
	// After the entries: a choice writes its pull request's recent record,
	// and a use its family's, before it touches the entry.
	if st.recents, err = listJSON[Recent](ctx, s.Objects, "cache/recent/"); err != nil {
		fail("recent choices", err)
	}
	if st.families, err = listJSON[Family](ctx, s.Objects, "cache/families/"); err != nil {
		fail("families", err)
	}
	if st.closures, err = listJSON[disks.Closure](ctx, s.Objects, "closures/"); err != nil {
		fail("closures", err)
	}
	return st
}

func (s *Service) repository(ctx context.Context, st *state, id int64) (*github.Repository, error) {
	if r, ok := st.repos[id]; ok {
		return r, nil
	}
	r, err := s.GitHub.RepositoryByID(ctx, id)
	if errors.Is(err, github.ErrNotFound) {
		st.repos[id] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.OwnerID != s.OrganizationID {
		st.repos[id] = nil
		return nil, nil
	}
	st.repos[id] = &r
	return &r, nil
}

// settleAll checks every pending entry's job, in case a delivery was lost.
func (s *Service) settleAll(ctx context.Context, st *state) {
	byJob := map[[2]int64][]Entry{}
	for _, r := range st.entries {
		if r.v.State == Pending {
			k := [2]int64{r.v.RepositoryID, r.v.JobID}
			byJob[k] = append(byJob[k], r.v)
		}
	}
	for k, es := range byJob {
		if ctx.Err() != nil {
			return
		}
		job, err := s.job(ctx, k[0], k[1])
		if err != nil {
			s.log().Warn("reading a pending cache entry's job", "repository_id", k[0], "job_id", k[1], "err", err.Error())
			continue
		}
		for _, e := range es {
			if err := s.settle(ctx, e, job); err != nil {
				s.log().Warn("settling a pending cache entry", "entry", e.ID, "err", err.Error())
			}
		}
	}
}

// dropChoices drops the choices of runs that have finished and have no
// automatic rerun left, under the lease, once GitHub, read under it, still
// shows the run finished, as the disks drop theirs.
func (s *Service) dropChoices(ctx context.Context, st *state) {
	byRun := map[[2]int64][]string{}
	for key, r := range st.choices {
		k := [2]int64{r.v.RepositoryID, r.v.RunID}
		byRun[k] = append(byRun[k], key)
	}
	byRepo := map[int64][][2]int64{}
	for k := range byRun {
		ok, err := s.Disks.Settled(ctx, k[0], k[1], nil)
		if err == nil && ok {
			byRepo[k[0]] = append(byRepo[k[0]], k)
		}
	}
	for repo, runs := range byRepo {
		err := s.Disks.Step(ctx, repo, func(ctx context.Context) error {
			live, err := s.Disks.Unreported(ctx)
			if err != nil {
				return err
			}
			for _, k := range runs {
				if ok, err := s.Disks.Settled(ctx, repo, k[1], live); err != nil || !ok {
					continue
				}
				st.settled[runKey(repo, k[1])] = true
				for _, key := range byRun[k] {
					if err := s.Objects.Delete(ctx, key, st.choices[key].gen); err == nil || errors.Is(err, store.ErrNotFound) {
						delete(st.choices, key)
					}
				}
			}
			return nil
		})
		if err != nil {
			s.log().Warn("dropping finished runs' cache choices", "repository_id", repo, "err", err.Error())
		}
	}
}

func runKey(repo, run int64) string {
	return strconv.FormatInt(repo, 10) + "/" + strconv.FormatInt(run, 10)
}

// familyUnused is why every entry of a retired family goes.
const familyUnused = "its family unused for 90 days"

// expire marks and deletes the entries that no longer need keeping
// (specs/0006-cache.md, Deleting): older versions of a series once a newer
// one is ready; in a pull request's scope, its other entries of a platform
// and family once one is ready, all of them once it has closed, those its
// closure retires, and any unused for 14 days; in a lasting scope, main's or
// a long-lived branch's, every key of a family and platform but the two
// newest and any an open pull request's most recent choice names, all of a
// family and platform unused for 90 days, and all of a branch's once it's
// no longer listed or no longer exists. Marking leaves alone any entry an
// unfinished run's choice names.
func (s *Service) expire(ctx context.Context, st *state) {
	byRepo := map[int64]map[string]record[Entry]{}
	add := func(repo int64) {
		if byRepo[repo] == nil {
			byRepo[repo] = map[string]record[Entry]{}
		}
	}
	for _, r := range st.entries {
		add(r.v.RepositoryID)
		byRepo[r.v.RepositoryID][r.v.ID] = r
	}
	for _, r := range st.recents {
		add(r.v.RepositoryID)
	}
	for _, repo := range slices.Sorted(maps.Keys(byRepo)) {
		if ctx.Err() != nil {
			return
		}
		s.expireRepo(ctx, st, repo, byRepo[repo])
	}
}

func (s *Service) expireRepo(ctx context.Context, st *state, repo int64, recs map[string]record[Entry]) {
	// Which version is newest is read only from entries whose objects are
	// there, or that are an index's, with no object: a record of one whose
	// object is gone mustn't stand in for it.
	there := map[string]record[Entry]{}
	for id, r := range recs {
		if _, ok := st.objects[r.v.Object]; (ok || r.v.Object == "") && r.v.State == Ready {
			there[id] = r
		}
	}
	rp, rerr := s.repository(ctx, st, repo)
	// Every pull request whose entries or most recent choices this consumer
	// holds is read once; one seen closed, or gone, is recorded as closed
	// before anything else.
	prs, branches := map[string]bool{}, map[string]bool{}
	for _, r := range recs {
		switch {
		case isPullRequest(r.v.Scope):
			prs[r.v.Scope] = true
		case lasting(r.v.Scope) && r.v.Scope != "main":
			branches[r.v.Scope] = true
		}
	}
	for _, r := range st.recents {
		if r.v.RepositoryID == repo {
			prs[r.v.Scope] = true
		}
	}
	closed := map[string]bool{}
	if rerr != nil {
		s.log().Warn("reading a consumer with cache entries", "repository_id", repo, "err", rerr.Error())
	} else {
		for _, sc := range slices.Sorted(maps.Keys(prs)) {
			c, err := s.seenClosed(ctx, st, rp, repo, sc)
			if err != nil {
				s.log().Warn("reading a cache scope's pull request", "scope", sc, "err", err.Error())
				continue
			}
			closed[sc] = c
		}
	}
	delisted := map[string]bool{}
	if rerr == nil && len(branches) > 0 {
		var err error
		delisted, err = disks.Delisted(ctx, s.GitHub, rp, s.log(), slices.Sorted(maps.Keys(branches)))
		if err != nil {
			s.log().Warn("reading a consumer's long-lived branches", "repository_id", repo, "err", err.Error())
		}
	}
	closure := func(sc string) *disks.Closure {
		if c, ok := st.closures[disks.ClosureKey(repo, sc)]; ok {
			return &c.v
		}
		return nil
	}

	latest, lineage := map[string]Entry{}, map[string]Entry{}
	used := map[string]time.Time{}
	for _, r := range st.families {
		if f := r.v; f.RepositoryID == repo {
			used[f.Scope+"\x00"+f.Store+"\x00"+f.Platform+"\x00"+f.Family] = f.LastUsed
		}
	}
	for _, r := range there {
		v := r.v
		// A retired entry replaces nothing: it goes itself.
		if retiredBy(v, closure(v.Scope)) {
			continue
		}
		if b, ok := latest[v.Series]; !ok || olderThan(b, v) {
			latest[v.Series] = v
		}
		f := v.Scope + "\x00" + v.Store + "\x00" + v.Platform + "\x00" + v.Family
		if b, ok := lineage[f]; !ok || olderThan(b, v) {
			lineage[f] = v
		}
		if lasting(v.Scope) && v.LastUsed.After(used[f]) {
			used[f] = v.LastUsed
		}
	}
	// A lasting scope keeps its two newest keys of each family and
	// platform, by their newest versions, and any key an open pull
	// request's most recent choice names.
	keys := map[string][]Entry{}
	for _, v := range latest {
		if lasting(v.Scope) {
			f := v.Scope + "\x00" + v.Store + "\x00" + v.Platform + "\x00" + v.Family
			keys[f] = append(keys[f], v)
		}
	}
	keep := map[string]bool{}
	for _, vs := range keys {
		slices.SortFunc(vs, func(x, y Entry) int {
			if olderThan(y, x) {
				return -1
			}
			return 1
		})
		for _, v := range vs[:min(LastingKeys, len(vs))] {
			keep[v.Series] = true
		}
	}
	for key, r := range st.recents {
		c := r.v
		if c.RepositoryID != repo {
			continue
		}
		if closed[c.Scope] {
			if err := s.Objects.Delete(ctx, key, r.gen); err == nil || errors.Is(err, store.ErrNotFound) {
				delete(st.recents, key)
			}
			continue
		}
		if lasting(c.EntryScope) {
			keep[Series(c.EntryScope, c.Store, c.Platform, c.Family, c.Key)] = true
		}
	}

	var due []record[Entry]
	why := map[string]string{}
	replacedBy := map[string]Entry{}
	mark := func(r record[Entry], reason string) {
		if _, ok := why[r.v.ID]; !ok {
			due = append(due, r)
			why[r.v.ID] = reason
		}
	}
	for _, r := range there {
		v := r.v
		f := v.Scope + "\x00" + v.Store + "\x00" + v.Platform + "\x00" + v.Family
		if l, ok := latest[v.Series]; ok && l.ID != v.ID {
			mark(r, "superseded by "+l.ID)
			replacedBy[v.ID] = l
			continue
		}
		if lasting(v.Scope) {
			switch {
			case delisted[v.Scope]:
				mark(r, "its long-lived branch is no longer listed, or no longer exists")
			case s.now().Sub(used[f]) >= LastingUnused:
				mark(r, familyUnused)
			case !keep[v.Series]:
				mark(r, "not among its family's two newest keys, nor named by an open pull request's most recent choice")
			}
			continue
		}
		switch {
		case closed[v.Scope]:
			mark(r, "its pull request closed")
		case retiredBy(v, closure(v.Scope)):
			mark(r, "committed by a run from before its pull request closed")
		case lineage[f].ID != v.ID:
			mark(r, "its pull request has a newer entry of the family since")
			replacedBy[v.ID] = lineage[f]
		case s.now().Sub(v.LastUsed) >= PullRequestUnused:
			mark(r, "unused for 14 days")
		}
	}
	// A ready entry whose object is gone, which only happens if someone
	// else deleted it, goes too, record and all, once Cloud Storage
	// confirms it.
	for _, r := range recs {
		if r.v.State != Ready || r.v.Object == "" {
			continue
		}
		if _, ok := st.objects[r.v.Object]; ok {
			continue
		}
		if _, err := s.Bucket.Attrs(ctx, r.v.Object); errors.Is(err, store.ErrNotFound) {
			mark(r, "its object is gone")
		}
	}
	if len(due) > 0 {
		marked, err := s.mark(ctx, repo, due, func(ctx context.Context, v Entry) (bool, error) {
			if by, ok := replacedBy[v.ID]; ok {
				return s.stillReplaces(ctx, by)
			}
			if why[v.ID] != familyUnused {
				return true, nil
			}
			// A family's retirement rests on its other entries' use too,
			// which a choice of one of them records in the family's record.
			f, _, found, err := getJSON[Family](ctx, s.Objects, familyKey(repo, v.Scope, v.Store, v.Platform, v.Family))
			if err != nil || !found {
				return err == nil, err
			}
			return s.now().Sub(f.LastUsed) >= LastingUnused, nil
		})
		if err != nil {
			s.log().Warn("marking cache entries for deletion", "repository_id", repo, "err", err.Error())
		}
		for _, v := range marked {
			recs[v.ID] = record[Entry]{v: v}
			s.log().Info("cache entry deleted", "repository_id", repo, "entry", v.ID, "scope", v.Scope, "reason", why[v.ID])
		}
	}
	// What's marked goes, whichever pass or request marked it.
	for _, r := range recs {
		if r.v.State == Marked {
			if err := s.deleteMarked(ctx, r.v); err != nil {
				s.log().Warn("deleting a marked cache entry", "entry", r.v.ID, "err", err.Error())
			}
		}
	}
}

// seenClosed reports whether a pull request scope's pull request is closed,
// or gone, and if so records that it was seen closed now, in the closure the
// disks share.
func (s *Service) seenClosed(ctx context.Context, st *state, rp *github.Repository, repo int64, sc string) (bool, error) {
	parsed, err := scope.Parse(sc)
	if err != nil {
		return false, err
	}
	n, _ := parsed.PullRequestNumber()
	var closedAt time.Time
	if rp != nil {
		pr, err := s.GitHub.PullRequest(ctx, *rp, n)
		switch {
		case errors.Is(err, github.ErrNotFound):
		case err != nil:
			return false, err
		case pr.State != "closed":
			return false, nil
		default:
			closedAt = pr.ClosedAt.UTC()
		}
	}
	c, err := s.Disks.SeenClosed(ctx, repo, sc, closedAt)
	if err != nil {
		return true, err
	}
	st.closures[disks.ClosureKey(repo, sc)] = record[disks.Closure]{v: c}
	return true, nil
}

// tidy deletes uploads whose reservations have expired, and reservations
// with them; objects of the cache's names that no record describes, under
// the lease; and families with no entries left.
func (s *Service) tidy(ctx context.Context, st *state) {
	reserved := map[string]bool{}
	for key, r := range st.reservations {
		v := r.v
		if s.now().Before(v.Expires) {
			reserved[UploadObject(v.RepositoryID, v.ID)] = true
			continue
		}
		s.dropUpload(ctx, v)
		if err := s.Objects.Delete(ctx, key, r.gen); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log().Warn("deleting an expired cache reservation", "reservation", v.ID, "err", err.Error())
			reserved[UploadObject(v.RepositoryID, v.ID)] = true
		}
	}
	recorded := map[string]bool{}
	for _, r := range st.entries {
		recorded[r.v.Object] = true
	}
	orphans := map[int64][]store.Object{}
	for name, o := range st.uploads {
		if repo, ok := repoOf(name); ok && !reserved[name] {
			orphans[repo] = append(orphans[repo], o)
		}
	}
	for name, o := range st.objects {
		if repo, ok := repoOf(name); ok && !recorded[name] {
			orphans[repo] = append(orphans[repo], o)
		}
	}
	for _, repo := range slices.Sorted(maps.Keys(orphans)) {
		s.orphans(ctx, repo, orphans[repo])
	}
	have := map[string]bool{}
	for _, r := range st.entries {
		have[familyKey(r.v.RepositoryID, r.v.Scope, r.v.Store, r.v.Platform, r.v.Family)] = true
	}
	for key, r := range st.families {
		if !have[key] {
			_ = s.Objects.Delete(ctx, key, r.gen)
		}
	}
}

// repoOf is the consumer whose object an object of the cache's names is.
func repoOf(name string) (int64, bool) {
	parts := strings.Split(name, "/")
	if len(parts) < 3 || (parts[0] != "uploads" && parts[0] != "entries") {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	return id, err == nil && id > 0
}

// orphans deletes a consumer's objects of the cache's names that no record
// describes, under its lease: an upload with no reservation, and an entry's
// object with no record, which is recorded as marked first, unless a
// choice names it, as an orphaned image is.
func (s *Service) orphans(ctx context.Context, repo int64, objs []store.Object) {
	var marked []Entry
	err := s.Disks.Step(ctx, repo, func(ctx context.Context) error {
		named, err := s.named(ctx, repo)
		if err != nil {
			return err
		}
		for _, o := range objs {
			if strings.HasPrefix(o.Name, "uploads/") {
				id := o.Name[strings.LastIndex(o.Name, "/")+1:]
				if _, _, ok, err := getJSON[Reservation](ctx, s.Objects, reservationKey(repo, id)); err != nil || ok {
					continue
				}
				if err := s.Bucket.Delete(ctx, o.Name, o.Generation); err == nil {
					s.log().Info("an upload with no reservation deleted", "repository_id", repo, "reservation", id)
				}
				continue
			}
			parts := strings.Split(o.Name, "/")
			if len(parts) != 4 || !idPattern.MatchString(parts[3]) {
				continue
			}
			id := parts[3]
			if named[id] {
				s.log().Warn("a cache object with no record is named by a choice; it's kept", "object_entry", id)
				continue
			}
			got, _, err := createOnce(ctx, s.Objects, entryKey(repo, id), Entry{ID: id, RepositoryID: repo, Object: o.Name,
				State: Marked, MarkedAt: s.now()})
			if err != nil {
				return err
			}
			if got.State == Marked && got.Object == o.Name {
				marked = append(marked, got)
			}
		}
		return nil
	})
	if err != nil {
		s.log().Warn("deleting the cache's orphaned objects", "repository_id", repo, "err", err.Error())
	}
	for _, e := range marked {
		if err := s.deleteMarked(ctx, e); err != nil {
			s.log().Warn("deleting an orphaned cache object", "entry", e.ID, "err", err.Error())
			continue
		}
		s.log().Info("orphaned cache object deleted", "repository_id", repo, "entry", e.ID)
	}
}
