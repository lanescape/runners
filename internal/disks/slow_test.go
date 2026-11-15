package disks_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/machines"
)

// quick is longer than any pass of these tests takes, and shorter than the
// emulator's answer to a wait for an operation that isn't done, as Compute
// Engine's is.
const quick = 30 * time.Second

// TestASaveDoesntWaitForItsImage: a pass asks for a saved disk's image and
// goes on, while Compute Engine takes its time to make it, rather than hold
// the pass's lease, and every queued job with it; the machine waits for a
// later pass, which finds the image ready and records it.
func TestASaveDoesntWaitForItsImage(t *testing.T) {
	e := setup(t)
	e.gce.HoldImages(true)
	before := e.images()
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	start := time.Now()
	jb.finish(true)
	if d := time.Since(start); d >= quick {
		t.Fatalf("the pass took %v: it waited for the image", d)
	}
	im := e.saved(before)
	if im.Status != "PENDING" {
		t.Fatalf("image = %+v, want one still being made", im)
	}
	if rec, ok := e.record("images/5001/" + im.Name + ".json"); !ok || rec["state"] != disks.ImageMaking {
		t.Fatalf("image record = %v, want it being made", rec)
	}
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); !ok {
		t.Fatal("the machine went before its disk was saved")
	}
	if !e.logged(`level=INFO msg="saving a work disk; the machine waits for it"`) || e.logged(`level=WARN msg="saving a work disk`) {
		t.Fatal("an image still being made wasn't logged as information alone")
	}
	// A pass while it's still being made doesn't wait for it either.
	start = time.Now()
	e.pass()
	if d := time.Since(start); d >= quick {
		t.Fatalf("the next pass took %v: it waited for the image", d)
	}
	if rec, _ := e.record("images/5001/" + im.Name + ".json"); rec["state"] != disks.ImageMaking {
		t.Fatalf("image record = %v, want it still being made", rec)
	}
	// Once Compute Engine has made it, the next pass records it, and the
	// machine goes.
	e.gce.HoldImages(false)
	e.pass()
	if rec, _ := e.record("images/5001/" + im.Name + ".json"); rec["state"] != disks.ImageReady {
		t.Fatalf("image record = %v, want it ready", rec)
	}
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
		t.Fatal("the machine outlived its saved disk")
	}
	if !e.logged(`msg="work disk saved"`) {
		t.Fatal("the save wasn't logged")
	}
	if src := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}}).source(); src != im.Name {
		t.Fatalf("the next run's disk came from %q, want the saved %s", src, im.Name)
	}
}

// TestAResumeImageDoesntHoldUpAPass: a preempted job's resume image is
// asked for, and the pass goes on; the job's run is rerun only once the
// image is ready, so that its rerun resumes from it.
func TestAResumeImageDoesntHoldUpAPass(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	e.gce.HoldImages(true)
	jb.preempt()
	start := time.Now()
	e.pass()
	if d := time.Since(start); d >= quick {
		t.Fatalf("the pass took %v: it waited for the resume image", d)
	}
	resume := disks.ResumeName(repoID, jb.in.ID)
	if im, ok := e.images()[resume]; !ok || im.Status != "PENDING" {
		t.Fatalf("resume image = %+v, %v, want one still being made", im, ok)
	}
	key := "preemptions/5001/" + strconv.FormatInt(run.ID, 10) + "/" + strconv.FormatUint(jb.in.ID, 10) + ".json"
	if rec, ok := e.record(key); !ok || rec["done"] == true {
		t.Fatalf("preemption record = %v, want it waiting for its image", rec)
	}
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); !ok {
		t.Fatal("the machine went before its resume image was made")
	}
	if a := e.attempt(run); a != 1 {
		t.Fatalf("attempt %d: the run was rerun before its resume image was ready", a)
	}
	if !e.logged(`level=INFO msg="recording a stopped Spot machine; it waits for its record"`) ||
		strings.Contains(e.logs.String(), `level=WARN msg="recording a stopped Spot machine`) {
		t.Fatal("a resume image still being made wasn't logged as information alone")
	}
	e.gce.HoldImages(false)
	e.pass()
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
		t.Fatal("the machine outlived its resume image")
	}
	if a := e.attempt(run); a != 2 {
		t.Fatalf("attempt %d; the run should have been rerun", a)
	}
	if src := e.admit(run, e.rerunJob(run, 2, "build")).source(); src != resume {
		t.Fatalf("the rerun's job started from %q, want the resume image %s", src, resume)
	}
}

// TestASaveUnderWayAtTheAgeLimitFinishes: a job that finished just before
// its machine's age limit has its save go on past the limit, for
// machines.SaveGrace, while Compute Engine makes the image; past that, the
// machine goes, saving nothing.
func TestASaveUnderWayAtTheAgeLimitFinishes(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "in time", true: "too late"}[late], func(t *testing.T) {
			e := setup(t)
			e.gce.HoldImages(true)
			before := e.images()
			jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
			e.clock.Advance(machines.MaxAge - time.Minute)
			jb.finish(true)
			im := e.saved(before)
			e.clock.Advance(5 * time.Minute)
			e.pass()
			if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); !ok {
				t.Fatal("the age limit deleted a machine whose save was under way")
			}
			if rec, _ := e.record("images/5001/" + im.Name + ".json"); rec["state"] != disks.ImageMaking {
				t.Fatalf("image record = %v, want it still being made", rec)
			}
			if late {
				e.clock.Advance(machines.SaveGrace)
				e.pass()
				if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
					t.Fatal("a machine waited for its save past the grace")
				}
				if !e.logged(`level=WARN msg="a work disk wasn't saved within 1h0m0s of its machine's age limit`) {
					t.Fatal("giving up a save wasn't logged")
				}
				return
			}
			e.gce.HoldImages(false)
			e.pass()
			if rec, _ := e.record("images/5001/" + im.Name + ".json"); rec["state"] != disks.ImageReady {
				t.Fatalf("image record = %v, want it ready", rec)
			}
			if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
				t.Fatal("the machine outlived its saved disk")
			}
		})
	}
}

// TestASaveAtTheAgeLimitDoesntNeedTheMachineSeenStopped: a job that asked
// to save and reported before the limit, on a machine the pass couldn't
// stop at the limit, as when the machine was listed running before it
// crossed the limit, is saved all the same: finish stops it first.
func TestASaveAtTheAgeLimitDoesntNeedTheMachineSeenStopped(t *testing.T) {
	e := setup(t)
	before := e.images()
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	e.clock.Advance(machines.MaxAge - time.Minute)
	jb.report(true)
	e.clock.Advance(2 * time.Minute)
	// The pass's stop at the limit is refused; finish's own goes through.
	e.gce.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1,
		Match:   func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/instances/"+jb.in.Name+"/stop") },
		Respond: fault.Status(http.StatusForbidden, nil, `{"error": {"code": 403, "message": "Forbidden"}}`)})
	e.pass()
	im := e.saved(before)
	if rec, _ := e.record("images/5001/" + im.Name + ".json"); rec["state"] != disks.ImageReady {
		t.Fatalf("image record = %v, want it ready", rec)
	}
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
		t.Fatal("the machine outlived its saved disk")
	}
}

// TestASaveAskedForDuringTheAgeLimitsPassIsKept: a job that asks to save,
// and reports, before the limit, while the pass that finds its machine past
// the limit goes on, after the pass read the machines' records, is saved:
// the pass reads the save request again with the report.
func TestASaveAskedForDuringTheAgeLimitsPassIsKept(t *testing.T) {
	e := setup(t)
	before := e.images()
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	e.clock.Advance(machines.MaxAge - time.Minute)
	jb.runSteps()
	at := e.clock.Now().UTC().Format(time.RFC3339Nano)
	e.clock.Advance(2 * time.Minute)
	// The records arrive as the pass claims the machine at its age limit,
	// after it read them all.
	var once sync.Once
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Match: func(r *http.Request) bool {
		if r.URL.Query().Get("name") == "machines/"+jb.in.Name+"/admission.json" {
			once.Do(func() {
				e.gcs.Put("state", "machines/"+jb.in.Name+"/save-request.json",
					[]byte(`{"machine":"`+jb.in.Name+`","job_id":`+strconv.FormatInt(jb.j.ID, 10)+`,"at":"`+at+`"}`))
				e.gcs.Put("state", "machines/"+jb.in.Name+"/report.json", []byte(`{"machine":"`+jb.in.Name+`","at":"`+at+`"}`))
			})
		}
		return false
	}})
	e.pass()
	im := e.saved(before)
	if rec, _ := e.record("images/5001/" + im.Name + ".json"); rec["state"] != disks.ImageReady {
		t.Fatalf("image record = %v, want it ready", rec)
	}
}

// TestASavedImageDeletedBeforeItsRecordIsMadeAgain: an image made, whose
// record couldn't be marked ready, and deleted since, is asked for again
// under a new request ID, rather than waited for under the old one, whose
// operation finished with nothing left.
func TestASavedImageDeletedBeforeItsRecordIsMadeAgain(t *testing.T) {
	e := setup(t)
	before := e.images()
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{sel(key1, "main")}})
	// The bucket refuses the image's record being marked ready, once.
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1,
		Match: func(r *http.Request) bool {
			q := r.URL.Query()
			return strings.HasPrefix(q.Get("name"), "images/5001/") && q.Get("ifGenerationMatch") != "" && q.Get("ifGenerationMatch") != "0"
		},
		Respond: fault.Status(http.StatusForbidden, nil, `{"error": {"code": 403, "message": "Forbidden"}}`)})
	jb.finish(true)
	im := e.saved(before)
	if rec, _ := e.record("images/5001/" + im.Name + ".json"); rec["state"] != disks.ImageMaking {
		t.Fatalf("image record = %v, want it still being made", rec)
	}
	if err := e.disks.Compute.DeleteImage(context.Background(), im.Name, compute.RequestID("delete", im.Name)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.pass()
	}
	if rec, _ := e.record("images/5001/" + im.Name + ".json"); rec["state"] != disks.ImageReady || rec["attempt"] != float64(1) {
		t.Fatalf("image record = %v, want it made again, ready, under a second attempt", rec)
	}
	if got, ok := e.images()[im.Name]; !ok || got.Status != "READY" {
		t.Fatalf("image = %+v, %v, want it made again", got, ok)
	}
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
		t.Fatal("the machine outlived its saved disk")
	}
}
