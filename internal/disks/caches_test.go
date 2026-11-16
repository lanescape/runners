package disks_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/consumer"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/machines"
)

// orphan removes a version's record, as if it were lost, and leaves its
// objects.
func (e *env) orphan(version string) {
	e.t.Helper()
	key := "caches/5001/" + version + ".json"
	if err := e.objects.Delete(context.Background(), key, mustGen(e.t, e, key)); err != nil {
		e.t.Fatal(err)
	}
}

// removeObjects deletes a version's objects, as someone else might.
func (e *env) removeObjects(version string) {
	e.t.Helper()
	for _, n := range e.versions()[version] {
		_, gen, _ := e.gcs.Object(cachesBucket, n)
		if err := e.caches.Delete(context.Background(), n, gen); err != nil {
			e.t.Fatal(err)
		}
	}
}

// legacyImage is an image named as a saved disk was before caches replaced
// saved disks, with no record.
func (e *env) legacyImage() string {
	e.t.Helper()
	machine := fmt.Sprintf("lsr-5001-legacy%02d", len(e.images()))
	name := disks.VersionName(repoID, disks.Series("main", "amd64", "build", key1), e.clock.Now(), machine)
	e.gce.AddLabelledImage(name, "", "X86_64", map[string]string{disks.LabelRepository: "5001", disks.LabelKind: "saved"})
	return name
}

// legacySaved is a saved disk from before caches replaced saved disks: its
// image, and its record as it was written then.
func (e *env) legacySaved() string {
	e.t.Helper()
	name := e.legacyImage()
	rec := fmt.Sprintf(`{"image": %q, "repository_id": 5001, "kind": "saved", "state": "ready", "scope": "main", "arch": "amd64", `+
		`"family": "build", "key": %q, "machine": "lsr-5001-legacy00", "zone": "us-central1-a", "size_gb": 50, "saved_at": %q}`,
		name, key1, e.clock.Now().UTC().Format(time.RFC3339Nano))
	e.gcs.Put("state", "images/5001/"+name+".json", []byte(rec))
	return name
}

// crc32c is an object's CRC32C as Cloud Storage gives it: base64, big-endian.
func crc32c(data []byte) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))
	return base64.StdEncoding.EncodeToString(b[:])
}

// specs/0003-disks.md, Caches: a version is restored only once GitHub
// concludes the job that saved it succeeded; committed, or still being
// saved, it's never chosen.
func TestAVersionIsRestorableOnlyOnceItsJobSucceeded(t *testing.T) {
	e := setup(t)
	saver := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	code, ans := saver.askSave()
	if code != http.StatusOK || !ans.Saves {
		t.Fatalf("save: %d, %+v", code, ans)
	}
	v := versionOfObject(ans.Prefix + "x")
	if rec, _ := e.record("caches/5001/" + v + ".json"); rec["state"] != disks.ImageMaking || rec["kind"] != disks.KindCache {
		t.Fatalf("a version being saved = %v", rec)
	}
	saver.upload(ans.Prefix)
	if v := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}).restored(); v != "" {
		t.Fatalf("a version still being saved was restored: %s", v)
	}
	if code, _ := saver.commit(); code != http.StatusOK {
		t.Fatalf("commit: %d", code)
	}
	if rec, _ := e.record("caches/5001/" + v + ".json"); rec["state"] != disks.ImageCommitted {
		t.Fatalf("a committed version = %v", rec)
	}
	// A pass while its job runs leaves it committed.
	e.pass()
	if rec, _ := e.record("caches/5001/" + v + ".json"); rec["state"] != disks.ImageCommitted {
		t.Fatalf("a committed version whose job runs on = %v", rec)
	}
	if got := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}).restored(); got != "" {
		t.Fatalf("a version whose job hadn't finished was restored: %s", got)
	}
	saver.finish(false)
	if !e.ready(v) {
		t.Fatal("a version whose job succeeded isn't ready")
	}
	if got := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}).restored(); got != v {
		t.Fatalf("restored %q, want %s", got, v)
	}
}

// A version its job never committed goes once the job has finished, with
// whatever it uploaded, though the job succeeded.
func TestAnUncommittedVersionGoes(t *testing.T) {
	e := setup(t)
	for _, uploaded := range []bool{true, false} {
		jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
		code, ans := jb.askSave()
		if code != http.StatusOK || !ans.Saves {
			t.Fatalf("save: %d, %+v", code, ans)
		}
		v := versionOfObject(ans.Prefix + "x")
		if uploaded {
			jb.upload(ans.Prefix)
		}
		jb.finish(false)
		if e.has(v) || e.recorded(v) {
			t.Fatalf("an uncommitted version (uploaded %v) was kept", uploaded)
		}
	}
	if !e.logged("its job finished without committing it") {
		t.Fatal("no reason logged")
	}
}

// A job saves a cache once: its repeated requests name the same version
// until it's committed, and a save after its commit is refused. A repeated
// commit answers what was recorded.
func TestAJobSavesACacheOnce(t *testing.T) {
	e := setup(t)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	_, first := jb.askSave()
	e.clock.Advance(time.Minute)
	code, again := jb.askSave()
	if code != http.StatusOK || again.Prefix != first.Prefix || again.Token != "create "+first.Prefix || !again.Expires.After(*first.Expires) {
		t.Fatalf("a repeated save = %d, %+v; want the same version as %+v, with a fresh token", code, again, first)
	}
	jb.upload(first.Prefix)
	code, got := jb.commit()
	if code != http.StatusOK || got.Objects != 2 || got.Bytes == 0 {
		t.Fatalf("commit: %d, %+v", code, got)
	}
	if code, _ := jb.askSave(); code != http.StatusForbidden {
		t.Fatalf("a save after the commit: %d, want 403", code)
	}
	// What's uploaded after the commit is no part of the version.
	e.gcs.Put(cachesBucket, first.Prefix+"late", []byte("late"))
	if code, repeat := jb.commit(); code != http.StatusOK || repeat != got {
		t.Fatalf("a repeated commit: %d, %+v; want %+v", code, repeat, got)
	}
	jb.finish(false)
	v := versionOfObject(first.Prefix + "x")
	if !e.ready(v) {
		t.Fatal("not saved")
	}
	if b := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}); len(b.restore.Objects) != 2 {
		t.Fatalf("restore = %+v; want the two objects committed", b.restore)
	}
	// Another job's save of the same cache is a version of its own.
	other := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if _, ans := other.askSave(); ans.Prefix == first.Prefix {
		t.Fatal("another job's save named the same version")
	}
}

// A commit with nothing uploaded is refused, as is one with no save, or
// of a cache the run's file doesn't declare.
func TestACommitNeedsItsObjects(t *testing.T) {
	e := setup(t)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if code, _ := jb.commit(); code != http.StatusForbidden {
		t.Fatalf("a commit with no save: %d, want 403", code)
	}
	_, ans := jb.askSave()
	if code, _ := jb.commit(); code != http.StatusForbidden {
		t.Fatalf("a commit with nothing uploaded: %d, want 403", code)
	}
	if rec, _ := e.record("caches/5001/" + versionOfObject(ans.Prefix+"x") + ".json"); rec["state"] != disks.ImageMaking {
		t.Fatalf("version = %v; want it still being saved", rec)
	}
	jb.upload(ans.Prefix)
	if code, got := jb.commit(); code != http.StatusOK || got.Objects != 2 {
		t.Fatalf("a commit once uploaded: %d, %+v", code, got)
	}
	for _, cache := range []string{"undeclared", "Build", ""} {
		for _, path := range []string{machines.CacheRestorePath, machines.CacheSavePath} {
			if code := jb.call(path, map[string]string{"cache": cache}, nil); code != http.StatusForbidden {
				t.Fatalf("%s of %q: %d, want 403", path, cache, code)
			}
		}
	}
}

// A commit records each object as Cloud Storage holds it: its size, CRC32C
// and generation, and a restore gives the job exactly those.
func TestACommitRecordsTheObjects(t *testing.T) {
	e := setup(t)
	saved := e.saveMain(key1)
	rec, _ := e.record("caches/5001/" + saved + ".json")
	if rec["committed_at"] == nil {
		t.Fatalf("version record = %v; want when it was committed", rec)
	}
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if jb.restored() != saved || len(jb.restore.Objects) != 2 {
		t.Fatalf("restore = %+v", jb.restore)
	}
	var total int64
	for _, o := range jb.restore.Objects {
		data, gen, ok := e.gcs.Object(cachesBucket, o.Name)
		if !ok || gen != o.Generation || int64(len(data)) != o.Size || o.CRC32C != crc32c(data) {
			t.Fatalf("object %+v; the bucket holds %d bytes at generation %d, CRC32C %s", o, len(data), gen, crc32c(data))
		}
		total += o.Size
	}
	if rec["bytes"] != float64(total) {
		t.Fatalf("version record = %v; want its objects' total size, %d", rec, total)
	}
}

// A run's key for a cache is made from its file at the run's commit, once
// for the run (specs/0003-disks.md, Keys).
func TestACachesKeyIsTheRunsCommits(t *testing.T) {
	e := setup(t)
	deps := []byte("v1\n")
	e.repo.Commit("main", map[string]string{consumer.RunnersFile: `{"caches": {"build": {"from": ["deps.txt"]}}}`, "deps.txt": string(deps)})
	run := e.mainRun()
	// A job whose label names no cache, which asks for one itself.
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{"lanescape-2vcpu-linux-amd64-main"}})
	var ans machines.CacheSave
	if code := jb.call(machines.CacheSavePath, map[string]string{"cache": "build"}, &ans); code != http.StatusOK || !ans.Saves {
		t.Fatalf("save: %d, %+v", code, ans)
	}
	want := consumer.Key("build", []consumer.PathContents{{Path: "deps.txt", Contents: deps}}, true)
	if rec, _ := e.record("caches/5001/" + versionOfObject(ans.Prefix+"x") + ".json"); rec["key"] != want {
		t.Fatalf("version = %v; want the key %s", rec, want)
	}
	if rec, ok := e.record("runs/5001/" + strconv.FormatInt(run.ID, 10) + "/caches.json"); !ok ||
		rec["caches"].(map[string]any)["build"].(map[string]any)["key"] != want {
		t.Fatalf("the run's caches = %v", rec)
	}
}

// A token that can't be had is a retryable failure.
func TestATokenThatCantBeHadIsTriedAgain(t *testing.T) {
	e := setup(t)
	v := e.saveMain(key1)
	e.tokens.fail.Store(true)
	run := e.mainRun()
	jb := e.take(run, e.queueJob(run, ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}))
	if code := jb.askRestore(); code != http.StatusServiceUnavailable {
		t.Fatalf("a restore with no token: %d, want 503", code)
	}
	if code, _ := jb.askSave(); code != http.StatusServiceUnavailable {
		t.Fatalf("a save with no token: %d, want 503", code)
	}
	e.tokens.fail.Store(false)
	if code := jb.askRestore(); code != http.StatusOK || jb.restored() != v {
		t.Fatalf("the restore once a token can be had: %d, %q", code, jb.restored())
	}
	if code, ans := jb.askSave(); code != http.StatusOK || !ans.Saves {
		t.Fatalf("the save once a token can be had: %d, %+v", code, ans)
	}
}

// What saved disks left, from before caches replaced them, goes: their
// images and records, and an image named as one with no record.
func TestSavedDisksGo(t *testing.T) {
	e := setup(t)
	v := e.saveMain(key1)
	saved := e.legacySaved()
	orphan := e.legacyImage()
	e.pass()
	if _, ok := e.images()[saved]; ok || e.recorded(saved) {
		t.Fatal("a saved disk was kept")
	}
	if _, ok := e.images()[orphan]; ok {
		t.Fatal("an image named as a saved disk, with no record, was kept")
	}
	if !e.logged("saved disks are no longer kept") {
		t.Fatal("no reason logged")
	}
	if !e.ready(v) {
		t.Fatal("a cache's version went with the saved disks")
	}
}

// Objects in the caches' bucket that no record describes go, once GitHub
// shows their repository as the organization's: a version's whose record
// was lost, and one named as a version that was never recorded. Objects
// not named as the service names versions are no version's, and stay.
func TestOrphanedObjectsGo(t *testing.T) {
	e := setup(t)
	kept := e.saveMain(key1)
	never := disks.VersionName(repoID, disks.Series("main", "amd64", "build", key2), e.clock.Now(), "lsr-5001-neverrec")
	e.gcs.Put(cachesBucket, disks.VersionPrefix(repoID, never)+"0.tar.zst", []byte("x"))
	e.gcs.Put(cachesBucket, "5001/elsewhere/x", []byte("x"))
	e.gh.Faults().Add(fault.Rule{PathPrefix: "/repos", Respond: notFound})
	e.pass()
	e.gh.Faults().Reset()
	if !e.has(never) {
		t.Fatal("orphaned objects went while GitHub answered 404 for their repository")
	}
	e.pass()
	if e.has(never) {
		t.Fatal("objects of a version never recorded were kept")
	}
	if _, ok := e.versions()["elsewhere"]; !ok {
		t.Fatal("an object not named as a version's was deleted")
	}
	if !e.ready(kept) {
		t.Fatal("a recorded version went as an orphan")
	}
}

// A version committed while a pass reads, after it listed the caches'
// bucket, whose job then succeeded, is made ready, and isn't taken for one
// whose objects are gone: the bucket was listed before they were there.
func TestAVersionCommittedDuringAPassKeepsItsRecord(t *testing.T) {
	e := setup(t)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	_, ans := jb.askSave()
	v := versionOfObject(ans.Prefix + "x")
	jb.upload(ans.Prefix)
	// The pass lists the bucket as it was before the job uploaded; the
	// job commits, and GitHub concludes it succeeded, before the pass
	// reads the records.
	var committed atomic.Int32
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1,
		Match: func(r *http.Request) bool {
			return strings.HasSuffix(r.URL.Path, "/b/"+cachesBucket+"/o") && r.URL.Query().Get("prefix") == ""
		},
		Respond: func(w http.ResponseWriter, _ *http.Request) {
			code, _ := jb.commit()
			committed.Store(int32(code))
			e.gh.CompleteJobByID(jb.j.ID, "success")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"kind": "storage#objects"}`))
		}})
	e.pass()
	if committed.Load() != http.StatusOK {
		t.Fatalf("commit: %d", committed.Load())
	}
	if !e.ready(v) {
		rec, _ := e.record("caches/5001/" + v + ".json")
		t.Fatalf("version = %v; want it ready", rec)
	}
	e.pass()
	if !e.ready(v) {
		t.Fatal("not kept")
	}
}

// A committed version whose objects are gone by the time its job has
// succeeded is never ready, and replaces nothing: the older version stays.
func TestACommittedVersionWhoseObjectsAreGoneReplacesNothing(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	e.clock.Advance(time.Minute)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key2, "main")}})
	newer := jb.saveCache()
	e.removeObjects(newer)
	e.clock.Advance(time.Second)
	jb.finish(false)
	if e.recorded(newer) {
		t.Fatal("a version whose objects are gone was kept")
	}
	if !e.logged("its objects are gone") {
		t.Fatal("no reason logged")
	}
	if !e.ready(old) {
		t.Fatal("a version whose objects are gone replaced the one that's there")
	}
}

// A ready version missing any object it committed restores nothing, though
// other objects are under its prefix: its record goes, then the objects.
func TestAVersionMissingACommittedObjectGoes(t *testing.T) {
	e := setup(t)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	v := jb.saveCache()
	// The job uploads another object after its commit.
	e.gcs.Put(cachesBucket, disks.VersionPrefix(repoID, v)+"late", []byte("late"))
	jb.finish(false)
	if !e.ready(v) {
		t.Fatal("not saved")
	}
	// Someone else deletes one of its committed objects.
	n := disks.VersionPrefix(repoID, v) + "0.tar.zst"
	_, gen, _ := e.gcs.Object(cachesBucket, n)
	if err := e.caches.Delete(context.Background(), n, gen); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Second)
	e.pass()
	if e.recorded(v) {
		t.Fatal("the record of a version missing a committed object was kept")
	}
	e.pass()
	if e.has(v) {
		t.Fatal("what was left of it was kept")
	}
}

// A version holds at most what its record, which every pass reads, can
// hold: a commit of more objects, or of names too long to record, is
// refused, and the version is still being saved; within the limits, it's
// committed.
func TestAVersionsLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		n, nameLen int
		ok         bool
	}{
		"the most objects":               {disks.MaxVersionObjects, 8, true},
		"one object too many":            {disks.MaxVersionObjects + 1, 8, false},
		"names too long to record":       {600, 900, false},
		"long names, few enough of them": {100, 900, true},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
			_, ans := jb.askSave()
			for i := range tc.n {
				name := fmt.Sprintf("%s%0*d", ans.Prefix, tc.nameLen, i)
				e.gcs.Put(cachesBucket, name, []byte("x"))
			}
			code, got := jb.commit()
			if tc.ok {
				if code != http.StatusOK || got.Objects != tc.n {
					t.Fatalf("a commit of %d objects: %d %+v", tc.n, code, got)
				}
				return
			}
			if code != http.StatusForbidden {
				t.Fatalf("a commit of %d objects with names of %d characters: %d, want 403", tc.n, tc.nameLen, code)
			}
			if rec, _ := e.record("caches/5001/" + versionOfObject(ans.Prefix+"x") + ".json"); rec["state"] != disks.ImageMaking || rec["objects"] != nil {
				t.Fatalf("version = %v; want it still being saved, with nothing recorded", rec)
			}
		})
	}
}

// The same, when the instance that committed it has a clock behind the
// pass's: its commit looks older than the pass's listing, which didn't see
// its objects, so the pass lists them afresh before deciding they're gone,
// and keeps it.
func TestAVersionCommittedDuringAPassByASlowClockKeepsItsObjects(t *testing.T) {
	e := setup(t)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	_, ans := jb.askSave()
	v := versionOfObject(ans.Prefix + "x")
	names := jb.upload(ans.Prefix)
	key := "caches/5001/" + v + ".json"
	var committed atomic.Int32
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1,
		Match: func(r *http.Request) bool {
			return strings.HasSuffix(r.URL.Path, "/b/"+cachesBucket+"/o") && r.URL.Query().Get("prefix") == ""
		},
		Respond: func(w http.ResponseWriter, _ *http.Request) {
			code, _ := jb.commit()
			committed.Store(int32(code))
			data, _, _ := e.gcs.Object("state", key)
			var rec disks.Image
			_ = json.Unmarshal(data, &rec)
			rec.CommittedAt = rec.CommittedAt.Add(-time.Second)
			b, _ := json.Marshal(rec)
			e.gcs.Put("state", key, b)
			e.gh.CompleteJobByID(jb.j.ID, "success")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"kind": "storage#objects"}`))
		}})
	e.pass()
	if committed.Load() != http.StatusOK {
		t.Fatalf("commit: %d", committed.Load())
	}
	if !e.ready(v) {
		rec, _ := e.record(key)
		t.Fatalf("version = %v; want it ready", rec)
	}
	for _, n := range names {
		if _, _, ok := e.gcs.Object(cachesBucket, n); !ok {
			t.Fatalf("%s was deleted", n)
		}
	}
}

// A version's record is under caches/, never images/, which a revision of
// the service from before caches reads as images' records and drops when it
// finds no image of its name.
func TestAVersionsRecordIsApartFromImages(t *testing.T) {
	e := setup(t)
	v := e.saveMain(key1)
	if _, ok := e.record("caches/5001/" + v + ".json"); !ok {
		t.Fatal("no record under caches/")
	}
	for _, n := range e.gcs.Names("state") {
		if strings.HasPrefix(n, "images/") {
			t.Fatalf("a record under images/: %s", n)
		}
	}
}

// A version committed by an instance whose clock is ahead, one of whose
// objects is replaced after its commit, is never made ready, whatever the
// times say, and the version before it is kept.
func TestAVersionCommittedByAFastClockIsCheckedAllTheSame(t *testing.T) {
	e := setup(t)
	old := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	_, ans := old.askSave()
	vOld := versionOfObject(ans.Prefix + "x")
	oldNames := old.upload(ans.Prefix)
	if code, _ := old.commit(); code != http.StatusOK {
		t.Fatalf("commit: %d", code)
	}
	e.gh.CompleteJobByID(old.j.ID, "success")
	e.pass()
	if !e.ready(vOld) {
		t.Fatal("the first version isn't ready")
	}

	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	_, ans = jb.askSave()
	v := versionOfObject(ans.Prefix + "x")
	names := jb.upload(ans.Prefix)
	if code, _ := jb.commit(); code != http.StatusOK {
		t.Fatalf("commit: %d", code)
	}
	key := "caches/5001/" + v + ".json"
	data, _, _ := e.gcs.Object("state", key)
	var rec disks.Image
	_ = json.Unmarshal(data, &rec)
	rec.CommittedAt = rec.CommittedAt.Add(time.Hour)
	b, _ := json.Marshal(rec)
	e.gcs.Put("state", key, b)
	e.gcs.Put(cachesBucket, names[0], []byte("replaced after its commit"))
	e.gh.CompleteJobByID(jb.j.ID, "success")
	e.pass()
	if e.ready(v) {
		t.Fatal("a version whose object was replaced after its commit was made ready")
	}
	if !e.ready(vOld) {
		t.Fatal("the version before it went")
	}
	for _, n := range oldNames {
		if _, _, ok := e.gcs.Object(cachesBucket, n); !ok {
			t.Fatalf("%s, of the version kept, was deleted", n)
		}
	}
}

// A version whose record was lost is kept while an unfinished run's choice
// names it, as any version is, and goes once none does.
func TestAChosenOrphanIsKept(t *testing.T) {
	e := setup(t)
	v := e.saveMain(key1)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	if got := jb.restored(); got != v {
		t.Fatalf("restored %s; want %s", got, v)
	}
	e.orphan(v)
	e.pass()
	if !e.has(v) {
		t.Fatal("an orphaned version an unfinished run chose was deleted")
	}
	jb.finish(true)
	e.pass()
	e.pass()
	if e.has(v) {
		t.Fatal("an orphaned version was kept once no run's choice named it")
	}
}
