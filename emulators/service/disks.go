package service

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/consumer"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/selector"
)

// Where a work disk came from: the steps of the restore order
// (specs/0003-disks.md, Saved disks), or a resume image, which the
// emulator never makes.
const (
	StepPullRequestExact  = "pull_request_exact"
	StepPullRequestNewest = "pull_request_newest"
	StepMainExact         = "main_exact"
	StepMainNewest        = "main_newest"
	StepBranchExact       = "branch_exact"
	StepBranchNewest      = "branch_newest"
	StepNone              = "none"
	Resumed               = "resume"
)

// WorkDisk is a job's work disk, as the emulator made it.
type WorkDisk struct {
	Family, Key, Architecture string
	// Scope is the scope of the job's run's binding, which any save of the
	// disk is in.
	Scope string
	// Restored is where it came from: a step of the restore order.
	Restored string
	// Ready reports whether it's attached, and Save whether its job asked
	// for it to be saved.
	Ready, Save bool
}

// SavedDisk is a disk a job saved in its run's scope.
type SavedDisk struct {
	Scope, Family, Architecture, Key string
	// Job is the job that saved it.
	Job int64
}

// diskState is the emulator's work disks and saved disks, under its lock.
type diskState struct {
	// work are each machine's work disk.
	work map[string]*workDisk
	// saved are every disk saved, oldest first.
	saved []*savedDisk
	// choices are each run's restore choice, by family and architecture.
	choices map[string]*choice
	// closures are when each pull request was last seen closed, by the
	// emulator's clock, which runs' bindings are made by.
	closures map[string]time.Time
}

func newDiskState() diskState {
	return diskState{work: map[string]*workDisk{}, choices: map[string]*choice{}, closures: map[string]time.Time{}}
}

type workDisk struct {
	WorkDisk
	scope scope.Scope
}

type savedDisk struct {
	SavedDisk
	repo int64
	// boundAt is when the run that saved it was bound, which a later
	// closing of its pull request retires it by.
	boundAt time.Time
	// replaced is set once a pull request's later save of the family
	// replaces it.
	replaced bool
}

// choice is a run's restore choice for a family and architecture: the
// first time one of its jobs asks, made for every job of the run that asks
// in that attempt (specs/0003-disks.md, Saved disks).
type choice struct {
	attempt int
	disk    *savedDisk
	step    string
}

// HoldDisks implements World. The emulator attaches a work disk at once:
// held, it answers making until released, and then ready.
func (e *Emulator) HoldDisks(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.holdDisks = on
}

// WorkDisk is a job's work disk, if it has asked for one.
func (e *Emulator) WorkDisk(j Job) (WorkDisk, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.dsk.work[j.Machine]
	if d == nil {
		return WorkDisk{}, false
	}
	return d.WorkDisk, true
}

// SavedDisks are a repository's saved disks that a restore may still
// choose, oldest first.
func (e *Emulator) SavedDisks(repo Repository) []SavedDisk {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []SavedDisk
	for _, d := range e.dsk.saved {
		if d.repo == repo.ID && !d.replaced && !e.retired(d) {
			out = append(out, d.SavedDisk)
		}
	}
	return out
}

// steps are a scope's steps of the restore order: its disk for the key,
// then its newest.
func steps(sc scope.Scope) (exact, newest string) {
	switch {
	case sc.IsMain():
		return StepMainExact, StepMainNewest
	case strings.HasPrefix(sc.String(), "branch-"):
		return StepBranchExact, StepBranchNewest
	default:
		return StepPullRequestExact, StepPullRequestNewest
	}
}

// familyKey is a family's key for a run, or the reason a request for it is
// refused, in the service's words.
func familyKey(rf machines.RunFiles, family string) (string, error) {
	if rf.Invalid != "" {
		return "", refusal{consumer.RunnersFile + " at the run's commit isn't valid: " + strings.TrimPrefix(rf.Invalid, "invalid: ")}
	}
	fk, ok := rf.Families[family]
	if !ok {
		return "", refusal{fmt.Sprintf("%s at the run's commit doesn't declare the family %s", consumer.RunnersFile, family)}
	}
	if fk.Missing != "" {
		return "", refusal{fmt.Sprintf("the family %s lists %s, which doesn't exist at the run's commit", family, fk.Missing)}
	}
	return fk.Key, nil
}

// disk is POST /v1/disks: the job's work disk, of the family it names, in
// its run's bound scope (specs/0002-machines.md, The work disk). A job's
// first request decides its family; a later one for the same family is
// answered about the same disk, and one for another is refused.
func (e *Emulator) disk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Family string `json:"family"`
	}
	rq, ok := e.active(w, r, &body)
	if !ok {
		return
	}
	if !selector.ValidName(body.Family) {
		fault.Refuse("the body names no family: 1 to 32 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen")(w, r)
		return
	}
	e.mu.Lock()
	d := e.dsk.work[rq.m.name]
	e.mu.Unlock()
	if d == nil {
		rf, err := e.files.RunFilesOf(r.Context(), rq.repo, rq.bd)
		if err != nil {
			fault.Unavailable(retryAfter)(w, r)
			return
		}
		key, err := familyKey(rf, body.Family)
		if err != nil {
			answer(w, r, nil, err)
			return
		}
		d = e.choose(rq, body.Family, key)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if d.Family != body.Family {
		fault.Refuse(fmt.Sprintf("this job's work disk is of the family %s: a job has one work disk", d.Family))(w, r)
		return
	}
	if !d.Ready && !e.holdDisks {
		d.Ready = true
	}
	state := "making"
	if d.Ready {
		state = "ready"
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": state, "restored": d.Restored})
}

// choose records a job's work disk of a family, made from its run's
// restore choice for the family and its machine's architecture, picked the
// first time one of the run's jobs asks in its attempt. A request that
// raced another of the same job's gets the disk recorded first.
func (e *Emulator) choose(rq request, family, key string) *workDisk {
	m := rq.m
	// Each pull request seen closed is recorded as closed until now, as the
	// service records the last time it saw one closed.
	var closed []int
	e.gh.Read(func(s *ghemu.State) {
		for _, p := range s.Pulls(s.Repo(rq.repo.ID)) {
			if p.State == "closed" {
				closed = append(closed, p.Number)
			}
		}
	})
	now := e.clock.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if d := e.dsk.work[m.name]; d != nil {
		return d
	}
	for _, n := range closed {
		k := fmt.Sprintf("%d/pr%d", rq.repo.ID, n)
		if now.After(e.dsk.closures[k]) {
			e.dsk.closures[k] = now
		}
	}
	ck := fmt.Sprintf("%s/%s", runKey(rq.repo.ID, m.run, family), m.selector.Arch)
	c := e.dsk.choices[ck]
	// Every rerun the emulator sees is a maintainer's, which picks again,
	// as a new run would.
	if c == nil || c.attempt < m.attempt {
		c = &choice{attempt: m.attempt, step: StepNone}
		var pool []*savedDisk
		for _, d := range e.dsk.saved {
			if d.repo == rq.repo.ID && d.Family == family && d.Architecture == m.selector.Arch && !d.replaced && !e.retired(d) {
				pool = append(pool, d)
			}
		}
		slices.Reverse(pool)
	pick:
		for _, sc := range rq.bd.Chain() {
			exact, newest := steps(sc)
			for _, d := range pool {
				if d.Scope == sc.String() && d.Key == key {
					c.disk, c.step = d, exact
					break pick
				}
			}
			for _, d := range pool {
				if d.Scope == sc.String() {
					c.disk, c.step = d, newest
					break pick
				}
			}
		}
		e.dsk.choices[ck] = c
	}
	d := &workDisk{WorkDisk: WorkDisk{Family: family, Key: key, Architecture: m.selector.Arch, Scope: rq.bd.Scope.String(),
		Restored: c.step}, scope: rq.bd.Scope}
	e.dsk.work[m.name] = d
	return d
}

// retired reports whether a saved disk's pull request was seen closed
// after its run was bound: it's never chosen again, even once the pull
// request is reopened. The emulator looks at a disk request, so it misses a
// pull request closed and reopened in between, which the service would
// learn of from GitHub's delivery. The caller holds e.mu.
func (e *Emulator) retired(d *savedDisk) bool {
	at, ok := e.dsk.closures[fmt.Sprintf("%d/%s", d.repo, d.Scope)]
	return ok && !d.boundAt.After(at)
}

// save is POST /v1/disks/save: the job asks for its work disk to be saved
// once it has finished, which the service still decides then.
func (e *Emulator) save(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Save *bool `json:"save"`
	}
	rq, ok := e.active(w, r, &body)
	if !ok {
		return
	}
	if body.Save == nil || !*body.Save {
		fault.Refuse(`the body must be {"save": true}`)(w, r)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.dsk.work[rq.m.name]
	if d == nil {
		fault.Refuse("this job has no work disk: a job asks for one with the disk action before it asks to save it")(w, r)
		return
	}
	d.Save = true
	if !rq.bd.Scope.Saves() {
		writeJSON(w, http.StatusOK, map[string]any{"saves": false, "reason": "the run's scope, " + rq.bd.Scope.String() + ", saves nothing"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saves": true})
}

// saveDisk saves a finished job's work disk, if it was attached, its job
// asked, and its scope saves. A pull request keeps one disk of each family
// and architecture. The caller holds e.mu.
func (e *Emulator) saveDisk(m *machine) {
	d := e.dsk.work[m.name]
	if d == nil || !d.Ready || !d.Save || !d.scope.Saves() {
		return
	}
	var boundAt time.Time
	if bd, ok, err := e.binder.Lookup(context.Background(), m.repo, m.run); err == nil && ok {
		boundAt = bd.BoundAt
	}
	saved := &savedDisk{SavedDisk: SavedDisk{Scope: d.Scope, Family: d.Family, Architecture: d.Architecture, Key: d.Key, Job: m.job},
		repo: m.repo, boundAt: boundAt}
	// A disk saved by a run bound before its pull request was last seen
	// closed is never chosen, and replaces none of the pull request's
	// other disks (specs/0003-disks.md, Expiry).
	if _, ok := d.scope.PullRequestNumber(); ok && !e.retired(saved) {
		for _, o := range e.dsk.saved {
			if o.repo == m.repo && o.Scope == d.Scope && o.Family == d.Family && o.Architecture == d.Architecture {
				o.replaced = true
			}
		}
	}
	e.dsk.saved = append(e.dsk.saved, saved)
}
