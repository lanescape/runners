package service

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/lanescape/runners/internal/store"
	"net/http"
	"slices"
	"strings"
	"time"

	gceemu "github.com/lanescape/runners/emulators/compute"
	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/consumer"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/selector"
)

// Where a cache came from: the steps of the restore order
// (specs/0003-disks.md, Caches); and where a work disk came from: a resume
// image, which the emulator never makes, or nothing.
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

// CachesBucket is the bucket of the emulator's storage emulator that holds
// what jobs save, as the service's caches' bucket does.
const CachesBucket = "lanescape-runners-caches"

// Token is every token the emulator hands out for its caches' bucket: its
// storage emulator checks none.
const Token = "emulated-cache-token"

// WorkDisk is a job's work disk, as the emulator made it: always empty.
type WorkDisk struct {
	Architecture string
	// Scope is the scope of the job's run's binding.
	Scope string
	// Restored is where it came from: none.
	Restored string
	// Ready reports whether it's attached.
	Ready bool
	// SizeGB, IOPS and ThroughputMiBps are what its job asked for, with the
	// service's defaults for what it left out.
	SizeGB, IOPS, ThroughputMiBps int64
	// Refused is why Compute Engine refused to make it, if it did.
	Refused string
}

// CacheVersion is a version of a cache that a job saved in its run's scope
// and that a restore may still choose.
type CacheVersion struct {
	Scope, Cache, Architecture, Key string
	// Job is the job that saved it, and Objects how many objects it holds.
	Job     int64
	Objects int
}

// diskState is the emulator's work disks and caches, under its lock.
type diskState struct {
	// work are each machine's work disk.
	work map[string]*WorkDisk
	// versions are every cache version saved or being saved, oldest first.
	versions []*cacheVersion
	// choices are each run's restore choice, by cache and architecture.
	choices map[string]*choice
	// closures are when each pull request was last seen closed, by the
	// emulator's clock, which runs' bindings are made by.
	closures map[string]time.Time
}

func newDiskState() diskState {
	return diskState{work: map[string]*WorkDisk{}, choices: map[string]*choice{}, closures: map[string]time.Time{}}
}

// The states a cache version is in, as the service records it.
const (
	versionSaving    = "making"
	versionCommitted = "committed"
	versionReady     = "ready"
)

type cacheVersion struct {
	CacheVersion
	repo    int64
	name    string
	state   string
	objects []machines.CacheObject
	// boundAt is when the run that saved it was bound, which a later
	// closing of its pull request retires it by; savedAt is when its job
	// asked to save it, which orders versions as the service orders them.
	boundAt, savedAt time.Time
	// replaced is set once its scope's later save of the cache, on its
	// architecture, replaces it, whatever its key.
	replaced bool
}

// prefix is where a version's objects are in the caches' bucket.
func (v *cacheVersion) prefix() string { return fmt.Sprintf("%d/%s/", v.repo, v.name) }

// choice is a run's restore choice for a cache and architecture: the first
// time one of its jobs asks, made for every job of the run that asks in that
// attempt (specs/0003-disks.md, Caches).
type choice struct {
	run     int64
	attempt int
	version *cacheVersion
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
	return *d, true
}

// WorkDiskSettings implements World.
func (e *Emulator) WorkDiskSettings(j Job) (DiskSettings, bool) {
	d, ok := e.WorkDisk(j)
	if !ok || d.Refused != "" {
		return DiskSettings{}, false
	}
	return DiskSettings{SizeGB: d.SizeGB, IOPS: d.IOPS, ThroughputMiBps: d.ThroughputMiBps}, true
}

// Caches implements World: a repository's cache versions that a restore may
// still choose, oldest first.
func (e *Emulator) Caches(repo Repository) []CacheVersion {
	e.resettle(repo.ID)
	unfinished := e.unfinished(repo.ID)
	e.mu.Lock()
	defer e.mu.Unlock()
	held := e.held(unfinished)
	var out []CacheVersion
	for _, v := range e.dsk.versions {
		if v.repo == repo.ID && v.state == versionReady && (!v.replaced || held[v]) && !e.retired(v) {
			out = append(out, v.CacheVersion)
		}
	}
	return out
}

// steps are a scope's steps of the restore order: its version for the key,
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

// cacheKey is a cache's key for a run, or the reason a request for it is
// refused, in the service's words.
func cacheKey(rf machines.RunFiles, cache string) (string, error) {
	if rf.Invalid != "" {
		return "", refusal{consumer.RunnersFile + " at the run's commit isn't valid: " + strings.TrimPrefix(rf.Invalid, "invalid: ")}
	}
	fk, ok := rf.Caches[cache]
	if !ok {
		return "", refusal{fmt.Sprintf("%s at the run's commit doesn't declare the cache %s", consumer.RunnersFile, cache)}
	}
	if fk.Missing != "" {
		return "", refusal{fmt.Sprintf("the cache %s lists %s, which doesn't exist at the run's commit", cache, fk.Missing)}
	}
	return fk.Key, nil
}

// disk is POST /v1/disks: the job's work disk, empty, in its run's bound
// scope, of the size and performance it asks for (specs/0002-machines.md,
// The work disk). What Compute Engine wouldn't make, by the limits the
// compute emulator gives it, is refused as the service refuses it. A later
// request is answered about the same disk, if it asks for the same.
func (e *Emulator) disk(w http.ResponseWriter, r *http.Request) {
	var body machines.DiskAsk
	rq, ok := e.active(w, r, &body)
	if !ok {
		return
	}
	want, why := body.Settings(e.shapes)
	if why != "" {
		fault.Refuse(why)(w, r)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.dsk.work[rq.m.name]
	if d == nil {
		if why := e.diskFits(rq.m, want.SizeGB); why != "" {
			fault.Refuse(why)(w, r)
			return
		}
		d = &WorkDisk{Architecture: rq.m.selector.Arch, Scope: rq.bd.Scope.String(), Restored: StepNone,
			SizeGB: want.SizeGB, IOPS: want.IOPS, ThroughputMiBps: want.ThroughputMiBps}
		if why := gceemu.HyperdiskBalancedRefusal(want.SizeGB, want.IOPS, want.ThroughputMiBps); why != "" {
			d.Refused = "Compute Engine refused the work disk: " + why
		}
		e.dsk.work[rq.m.name] = d
	}
	was := machines.DiskSettings{SizeGB: d.SizeGB, IOPS: d.IOPS, ThroughputMiBps: d.ThroughputMiBps}
	if !body.Matches(was) {
		fault.Refuse(fmt.Sprintf("the job's work disk was asked for as %s; a job has one work disk", was))(w, r)
		return
	}
	if d.Refused != "" {
		fault.Refuse(d.Refused)(w, r)
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

// diskFits is why the service would refuse a work disk of sizeGB for m, by
// what the compute emulator reports of Hyperdisk Balanced and of the first
// machine type of m's shape, or "".
func (e *Emulator) diskFits(m *machine, sizeGB int64) string {
	if sizeGB < gceemu.HyperdiskBalancedMinGB || sizeGB > gceemu.HyperdiskBalancedMaxGB {
		return fmt.Sprintf("the work disk would be %d GB, but Compute Engine makes %s disks of %d to %d GB",
			sizeGB, machines.DiskType, gceemu.HyperdiskBalancedMinGB, gceemu.HyperdiskBalancedMaxGB)
	}
	mt := ""
	if sh := e.shapes.Shapes[m.selector.Shape]; sh != nil && len(sh.MachineTypes) > 0 {
		mt = sh.MachineTypes[0]
	}
	if boot := int64(e.shapes.BootDiskGB); boot+sizeGB > gceemu.MaxDisksSizeGB {
		return fmt.Sprintf("the work disk would be %d GB, but Compute Engine lets a %s machine's disks hold %d GB in all, and its boot disk is %d GB",
			sizeGB, mt, int64(gceemu.MaxDisksSizeGB), boot)
	}
	return ""
}

// cacheRequest reads a cache request's job and cache, and, with withKey, the
// cache's key at the run's commit, as the service does; it answers any
// request it can't go on with itself.
func (e *Emulator) cacheRequest(w http.ResponseWriter, r *http.Request, withKey bool) (request, string, string, bool) {
	var body struct {
		Cache string `json:"cache"`
	}
	rq, ok := e.active(w, r, &body)
	if !ok {
		return request{}, "", "", false
	}
	if !selector.ValidName(body.Cache) {
		fault.Refuse("the body names no cache: 1 to 32 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen")(w, r)
		return request{}, "", "", false
	}
	if !withKey {
		return rq, body.Cache, "", true
	}
	rf, err := e.files.RunFilesOf(r.Context(), rq.repo, rq.bd)
	if err != nil {
		fault.Unavailable(retryAfter)(w, r)
		return request{}, "", "", false
	}
	key, err := cacheKey(rf, body.Cache)
	if err != nil {
		answer(w, r, nil, err)
		return request{}, "", "", false
	}
	return rq, body.Cache, key, true
}

// restoreCache is POST /v1/caches/restore: the run's restore choice for the
// cache on the job's architecture, picked the first time one of the run's
// jobs asks in its attempt, and the version's objects.
func (e *Emulator) restoreCache(w http.ResponseWriter, r *http.Request) {
	rq, cache, key, ok := e.cacheRequest(w, r, true)
	if !ok {
		return
	}
	c := e.choose(rq, cache, key)
	if c.version == nil {
		writeJSON(w, http.StatusOK, map[string]any{"restored": c.step})
		return
	}
	expires := e.clock.Now().Add(time.Hour).UTC()
	writeJSON(w, http.StatusOK, machines.CacheRestore{Restored: c.step, Bucket: CachesBucket, Objects: c.version.objects, Token: Token,
		Expires: &expires})
}

// choose is a run's restore choice for a cache and its job's architecture,
// picked the first time one of the run's jobs asks in its attempt.
func (e *Emulator) choose(rq request, cache, key string) *choice {
	m := rq.m
	e.resettle(rq.repo.ID)
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
	unfinished := e.unfinished(rq.repo.ID)
	now := e.clock.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, n := range closed {
		k := fmt.Sprintf("%d/pr%d", rq.repo.ID, n)
		if now.After(e.dsk.closures[k]) {
			e.dsk.closures[k] = now
		}
	}
	ck := fmt.Sprintf("%s/%s", runKey(rq.repo.ID, m.run, cache), m.selector.Arch)
	c := e.dsk.choices[ck]
	// Every rerun the emulator sees is a maintainer's, which picks again,
	// as a new run would.
	if c == nil || c.attempt < m.attempt {
		c = &choice{run: m.run, attempt: m.attempt, step: StepNone}
		held := e.held(unfinished)
		var pool []*cacheVersion
		for _, v := range e.dsk.versions {
			if v.repo == rq.repo.ID && v.state == versionReady && v.Cache == cache && v.Architecture == m.selector.Arch &&
				(!v.replaced || held[v]) && !e.retired(v) {
				pool = append(pool, v)
			}
		}
		// Newest saved first, as the service orders them.
		slices.SortFunc(pool, func(x, y *cacheVersion) int {
			switch {
			case olderThan(y, x):
				return -1
			case olderThan(x, y):
				return 1
			}
			return 0
		})
	pick:
		for _, sc := range rq.bd.Chain() {
			exact, newest := steps(sc)
			for _, v := range pool {
				if v.Scope == sc.String() && v.Key == key {
					c.version, c.step = v, exact
					break pick
				}
			}
			for _, v := range pool {
				if v.Scope == sc.String() {
					c.version, c.step = v, newest
					break pick
				}
			}
		}
		e.dsk.choices[ck] = c
	}
	return c
}

// saveCache is POST /v1/caches/save: in a scope that saves, the one version
// the job saves of the cache, recorded before anything of it is uploaded.
func (e *Emulator) saveCache(w http.ResponseWriter, r *http.Request) {
	rq, cache, key, ok := e.cacheRequest(w, r, true)
	if !ok {
		return
	}
	if !rq.bd.Scope.Saves() {
		writeJSON(w, http.StatusOK, machines.CacheSave{Saves: false, Reason: "the run's scope, " + rq.bd.Scope.String() + ", saves nothing"})
		return
	}
	var boundAt time.Time
	if bd, found, err := e.binder.Lookup(context.Background(), rq.m.repo, rq.m.run); err == nil && found {
		boundAt = bd.BoundAt
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	v := e.version(rq.m.job, cache)
	if v == nil {
		v = &cacheVersion{CacheVersion: CacheVersion{Scope: rq.bd.Scope.String(), Cache: cache, Architecture: rq.m.selector.Arch, Key: key,
			Job: rq.m.job}, repo: rq.m.repo, name: fmt.Sprintf("lsr-%d-%s-%d-%s", rq.m.repo, selector.Hash(rq.bd.Scope.String()+"\x00"+
			rq.m.selector.Arch+"\x00"+cache+"\x00"+key), e.clock.Now().Unix(), rq.m.name[strings.LastIndex(rq.m.name, "-")+1:]),
			state: versionSaving, boundAt: boundAt, savedAt: e.clock.Now()}
		e.dsk.versions = append(e.dsk.versions, v)
	}
	if v.state != versionSaving {
		fault.Refuse("this job has saved the cache "+cache+" already: a job saves a cache once")(w, r)
		return
	}
	expires := e.clock.Now().Add(time.Hour).UTC()
	writeJSON(w, http.StatusOK, machines.CacheSave{Saves: true, Bucket: CachesBucket, Prefix: v.prefix(), Token: Token, Expires: &expires})
}

// version is the version a job is saving, or has saved, of a cache. The
// caller holds e.mu.
func (e *Emulator) version(job int64, cache string) *cacheVersion {
	for _, v := range e.dsk.versions {
		if v.Job == job && v.Cache == cache {
			return v
		}
	}
	return nil
}

// commitCache is POST /v1/caches/commit: what's under the version's prefix
// in the storage emulator, recorded.
func (e *Emulator) commitCache(w http.ResponseWriter, r *http.Request) {
	rq, cache, _, ok := e.cacheRequest(w, r, false)
	if !ok {
		return
	}
	e.mu.Lock()
	v := e.version(rq.m.job, cache)
	e.mu.Unlock()
	if v == nil {
		fault.Refuse("this job hasn't asked to save the cache "+cache)(w, r)
		return
	}
	// A commit asked again is answered as before, without listing again, as
	// the service answers it from its record.
	e.mu.Lock()
	if v.state != versionSaving {
		defer e.mu.Unlock()
		writeJSON(w, http.StatusOK, v.committed())
		return
	}
	e.mu.Unlock()
	objs, err := e.caches.List(r.Context(), v.prefix())
	if err != nil {
		fault.Unavailable(retryAfter)(w, r)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if v.state == versionSaving {
		if len(objs) == 0 {
			fault.Refuse("nothing is uploaded under "+v.prefix()+" for the cache "+cache)(w, r)
			return
		}
		var committed []machines.CacheObject
		for _, o := range objs {
			var crc [4]byte
			binary.BigEndian.PutUint32(crc[:], o.CRC32C)
			committed = append(committed, machines.CacheObject{Name: o.Name, Size: o.Size, CRC32C: base64.StdEncoding.EncodeToString(crc[:]),
				Generation: o.Generation})
		}
		if err := machines.CheckCacheObjects(cache, committed); err != nil {
			var refused machines.RefusedError
			if errors.As(err, &refused) {
				fault.Refuse(refused.Reason)(w, r)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		v.objects = committed
		v.Objects, v.state = len(v.objects), versionCommitted
	}
	writeJSON(w, http.StatusOK, v.committed())
}

// committed is the answer to a version's commit.
func (v *cacheVersion) committed() machines.CacheCommit {
	var bytes int64
	for _, o := range v.objects {
		bytes += o.Size
	}
	return machines.CacheCommit{Objects: len(v.objects), Bytes: bytes}
}

// olderThan orders versions by when they were saved, then by name, as the
// service does.
func olderThan(x, y *cacheVersion) bool {
	return x.savedAt.Before(y.savedAt) || (x.savedAt.Equal(y.savedAt) && x.name < y.name)
}

// settle decides a finished job's cache versions, as a pass of the service
// does once GitHub shows the job finished: one it committed becomes ready
// if it succeeded, and it and its scope's other versions of the cache on
// its architecture, whatever their key, keep only the newest saved; any
// other goes. The caller holds e.mu, and settles only a job whose
// repository GitHub shows as the organization's, as the service decides
// nothing of any other's.
func (e *Emulator) settle(job int64, succeeded bool) {
	kept := e.dsk.versions[:0]
	var gone []*cacheVersion
	for _, v := range e.dsk.versions {
		if v.Job != job || v.state == versionReady {
			kept = append(kept, v)
			continue
		}
		whole := false
		if v.state == versionCommitted && succeeded {
			var err error
			// One whose objects can't be listed decides nothing: it waits,
			// to be settled again (resettle), as the service's pass defers.
			if whole, err = e.intact(v); err != nil {
				kept = append(kept, v)
				continue
			}
		}
		if !whole {
			gone = append(gone, v)
			continue
		}
		v.state = versionReady
		// A version saved by a run bound before its pull request was last
		// seen closed is never chosen, and replaces none of the pull
		// request's other versions (specs/0003-disks.md, Deletion); of the
		// rest, the newest saved replaces the others, whichever finished
		// first.
		if !e.retired(v) {
			for _, o := range e.dsk.versions {
				if o == v || o.repo != v.repo || o.state != versionReady || o.Scope != v.Scope || o.Cache != v.Cache ||
					o.Architecture != v.Architecture || e.retired(o) {
					continue
				}
				if olderThan(o, v) {
					o.replaced = true
				} else {
					v.replaced = true
				}
			}
		}
		kept = append(kept, v)
	}
	for _, v := range gone {
		objs, err := e.listCaches(v.prefix())
		if err != nil {
			// Its objects go once they can be listed.
			kept = append(kept, v)
			continue
		}
		if !e.deleteCaches(objs) {
			// What's left goes when it's settled again.
			kept = append(kept, v)
		}
	}
	e.dsk.versions = kept
}

// deleteCaches deletes objects of the caches' bucket, each within a
// deadline, and reports whether they all went.
func (e *Emulator) deleteCaches(objs []store.Object) bool {
	all := true
	for _, o := range objs {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := e.caches.Delete(ctx, o.Name, o.Generation)
		cancel()
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			all = false
		}
	}
	return all
}

// listCaches lists the caches' bucket under prefix, within a deadline, so
// that storage the client retries without end can't hang the emulator.
func (e *Emulator) listCaches(prefix string) ([]store.Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return e.caches.List(ctx, prefix)
}

// intact reports whether every object a version committed is in the
// caches' bucket at the generation recorded: one that isn't never becomes
// ready, as the service decides.
func (e *Emulator) intact(v *cacheVersion) (bool, error) {
	objs, err := e.listCaches(v.prefix())
	if err != nil {
		return false, err
	}
	listed := map[string]int64{}
	for _, o := range objs {
		listed[o.Name] = o.Generation
	}
	for _, o := range v.objects {
		if g, ok := listed[o.Name]; !ok || g != o.Generation {
			return false, nil
		}
	}
	return len(v.objects) > 0, nil
}

// resettle settles a repository's versions whose jobs GitHub shows
// completed, by GitHub's conclusion, as a pass of the service would: jobs a
// test finished in GitHub directly, and jobs that finished while GitHub
// didn't show the repository as the organization's, which wait until it
// does.
func (e *Emulator) resettle(repo int64) {
	e.mu.Lock()
	jobs := map[int64]bool{}
	for _, v := range e.dsk.versions {
		if v.repo == repo && v.state != versionReady {
			jobs[v.Job] = true
		}
	}
	e.mu.Unlock()
	if len(jobs) == 0 || !e.ours(repo) {
		return
	}
	done := map[int64]string{}
	e.gh.Read(func(s *ghemu.State) {
		for id := range jobs {
			if j := s.Job(id); j != nil && j.Status == "completed" {
				done[id] = j.Conclusion
			}
		}
	})
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, conclusion := range done {
		e.settle(id, conclusion == "success")
	}
}

// unfinished are a repository's runs that GitHub doesn't show completed,
// with the attempt each is in.
func (e *Emulator) unfinished(repo int64) map[int64]int {
	out := map[int64]int{}
	e.gh.Read(func(s *ghemu.State) {
		for _, r := range s.Runs() {
			if r.Repo != nil && r.Repo.ID == repo && r.Status != "completed" {
				out[r.ID] = r.Attempt
			}
		}
	})
	return out
}

// held are the versions that an unfinished run's choice names, picked in
// the attempt it's in: the service deletes none of them, even once a later
// save replaces them, until the run has finished, so a restore may still
// choose one. A choice of an earlier attempt holds nothing: every rerun the
// emulator sees is a maintainer's, which picks again, and the service drops
// a finished run's choices. The caller holds e.mu.
func (e *Emulator) held(unfinished map[int64]int) map[*cacheVersion]bool {
	out := map[*cacheVersion]bool{}
	for _, c := range e.dsk.choices {
		if attempt, ok := unfinished[c.run]; ok && c.version != nil && c.attempt == attempt {
			out[c.version] = true
		}
	}
	return out
}

// retired reports whether a version's pull request was seen closed after
// its run was bound: it's never chosen again, even once the pull request is
// reopened. The emulator looks at a restore request, so it misses a pull
// request closed and reopened in between, which the service would learn of
// from GitHub's delivery. The caller holds e.mu.
func (e *Emulator) retired(v *cacheVersion) bool {
	at, ok := e.dsk.closures[fmt.Sprintf("%d/%s", v.repo, v.Scope)]
	return ok && !v.boundAt.After(at)
}
