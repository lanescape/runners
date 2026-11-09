package service

import (
	"fmt"
	"strings"
	"time"
)

// The service's retention (specs/0006-cache.md, Deleting), which decides
// what a later restore can find.
const (
	PullRequestUnused = 14 * 24 * time.Hour
	LastingUnused     = 90 * 24 * time.Hour
	LastingKeys       = 2
	PendingFor        = 5 * 24 * time.Hour
)

// recent is a pull request's most recent choice for a store, platform and
// family: a lasting scope keeps the key it names while the pull request is
// open.
type recent struct {
	entryScope, key string
}

func lineageKey(repo int64, scope, store, platform, family string) string {
	return fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s", repo, scope, store, platform, family)
}

func (en *entry) lineage() string {
	return lineageKey(en.repo, en.scope, en.store, en.platform, en.family)
}
func (en *entry) series() string { return en.lineage() + "\x00" + en.key }

func lasting(scope string) bool { return scope == "main" || strings.HasPrefix(scope, "branch-") }

func newer(x, y *entry) bool {
	return x.committedAt.After(y.committedAt) || (x.committedAt.Equal(y.committedAt) && x.id > y.id)
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// used marks an entry used, as a restore that hands it out does: at most
// once a day.
func (e *Emulator) used(en *entry) {
	if day(en.lastUsed) == day(e.now()) {
		return
	}
	en.lastUsed = e.now()
	if lasting(en.scope) {
		e.familyUsed[en.lineage()] = e.now()
	}
}

// retain deletes what the service deletes once nothing needs it, as its
// pass does after GitHub tells it a job finished or a pull request closed,
// or on its own: what an unfinished run's choice names stays, and a
// finished run's choices go. The caller holds e.mu.
func (e *Emulator) retain() {
	// A pull request seen closed now, whose entries or most recent choices
	// are held, is recorded as closed now: what a run bound up to now
	// saves is retired, even once it's reopened.
	// An expired reservation goes, with whatever is at its upload's name,
	// and so does an upload with no reservation, such as one finished
	// later through its session.
	reserved := map[string]bool{}
	for id, r := range e.reserved {
		if !e.now().Before(r.expires) {
			delete(e.objects, uploadObject(r))
			delete(e.reserved, id)
			continue
		}
		reserved[uploadObject(r)] = true
	}
	for name := range e.objects {
		if strings.HasPrefix(name, "uploads/") && !reserved[name] {
			delete(e.objects, name)
		}
	}
	// What rests on GitHub's word is decided only while GitHub answers:
	// closings, runs' ends and branches' listings.
	github := !e.unavailable
	if github {
		for _, en := range e.entries {
			e.seenClosed(en.repo, en.scope)
		}
		for k := range e.recents {
			e.seenClosed(e.recentScope(k))
		}
	}
	named := map[string]bool{}
	for k, c := range e.choices {
		if rn := e.runs[c.run]; rn.due || e.runInProgress(rn) || !github {
			if c.entry != "" {
				named[c.entry] = true
			}
			continue
		}
		delete(e.choices, k)
	}
	retired := func(en *entry) bool {
		closed, ok := e.closures[closureKey(en.repo, en.scope)]
		return ok && !en.boundAt.After(closed)
	}
	latest, lineage := map[string]*entry{}, map[string]*entry{}
	used := map[string]time.Time{}
	for k, t := range e.familyUsed {
		used[k] = t
	}
	for _, en := range e.entries {
		if en.state != "ready" || retired(en) {
			continue
		}
		if l := latest[en.series()]; l == nil || newer(en, l) {
			latest[en.series()] = en
		}
		if l := lineage[en.lineage()]; l == nil || newer(en, l) {
			lineage[en.lineage()] = en
		}
		if lasting(en.scope) && en.lastUsed.After(used[en.lineage()]) {
			used[en.lineage()] = en.lastUsed
		}
	}
	// A lasting scope keeps its two newest keys, by their newest versions,
	// and any an open pull request's most recent choice names.
	keys := map[string][]*entry{}
	for _, en := range latest {
		if lasting(en.scope) {
			keys[en.lineage()] = append(keys[en.lineage()], en)
		}
	}
	keep := map[string]bool{}
	for _, vs := range keys {
		for range LastingKeys {
			var best *entry
			for _, v := range vs {
				if !keep[v.series()] && (best == nil || newer(v, best)) {
					best = v
				}
			}
			if best != nil {
				keep[best.series()] = true
			}
		}
	}
	for k, r := range e.recents {
		repo, scope := e.recentScope(k)
		if p := e.pullOf(repo, scope); github && (p == nil || !p.open) {
			delete(e.recents, k)
			continue
		}
		var store, platform, family string
		parts := strings.Split(k, "\x00")
		store, platform, family = parts[2], parts[3], parts[4]
		keep[lineageKey(repo, r.entryScope, store, platform, family)+"\x00"+r.key] = true
	}
	for id, en := range e.entries {
		// A pending entry whose job GitHub doesn't show finished within 5
		// days goes.
		// It's kept while GitHub can't confirm its job's state.
		if en.state == "pending" && e.now().Sub(en.committedAt) >= PendingFor && !e.unavailable {
			if en.object != "" {
				delete(e.objects, en.object)
			}
			delete(e.entries, id)
			continue
		}
		if en.state != "ready" || named[id] {
			continue
		}
		gone := false
		switch {
		case retired(en):
			gone = true
		case latest[en.series()] != en:
			gone = true
		case lasting(en.scope):
			gone = (github && e.delisted(en)) || e.now().Sub(used[en.lineage()]) >= LastingUnused || !keep[en.series()]
		default:
			p := e.pullOf(en.repo, en.scope)
			gone = (github && (p == nil || !p.open)) || lineage[en.lineage()] != en || e.now().Sub(en.lastUsed) >= PullRequestUnused
		}
		if gone {
			if en.object != "" {
				delete(e.objects, en.object)
			}
			delete(e.entries, id)
		}
	}
}

// seenClosed records a pull request scope's pull request as closed now,
// if it is.
func (e *Emulator) seenClosed(repo int64, scope string) {
	if !strings.HasPrefix(scope, "pr") {
		return
	}
	if p := e.pullOf(repo, scope); p == nil || !p.open {
		e.seen(repo, scope, e.now())
	}
}

// seen records a pull request scope's closing as seen at t, keeping
// whichever time is later: a closure only moves forward.
func (e *Emulator) seen(repo int64, scope string, t time.Time) {
	k := closureKey(repo, scope)
	if cur, ok := e.closures[k]; !ok || t.After(cur) {
		e.closures[k] = t
	}
}

func (e *Emulator) recentScope(k string) (int64, string) {
	var repo int64
	parts := strings.Split(k, "\x00")
	fmt.Sscan(parts[0], &repo)
	return repo, parts[1]
}

// pullOf is a pull request scope's pull request.
func (e *Emulator) pullOf(repo int64, scope string) *pull {
	rp := e.repos[repo]
	var n int
	if rp == nil || !strings.HasPrefix(scope, "pr") {
		return nil
	}
	fmt.Sscan(strings.TrimPrefix(scope, "pr"), &n)
	return rp.pulls[n]
}

// delisted reports whether a long-lived branch's scope has lost its branch:
// no longer listed in the default branch's runners.json, or gone.
func (e *Emulator) delisted(en *entry) bool {
	name, ok := strings.CutPrefix(en.scope, "branch-")
	if !ok {
		return false
	}
	rp := e.repos[en.repo]
	for branch, n := range parseRunners(rp.runners).branches {
		if n == name {
			_, exists := rp.branches[branch]
			return !exists
		}
	}
	return true
}
