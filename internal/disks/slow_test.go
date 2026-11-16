package disks_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/disks"
)

// quick is longer than any pass of these tests takes, and shorter than the
// emulator's answer to a wait for an operation that isn't done, as Compute
// Engine's is.
const quick = 30 * time.Second

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

// TestAResumeImageDeletedBeforeItsRecordIsMadeAgain: an image made, whose
// record couldn't be marked ready, and deleted since, is asked for again
// under a new request ID, rather than waited for under the old one, whose
// operation finished with nothing left.
func TestAResumeImageDeletedBeforeItsRecordIsMadeAgain(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	resume := disks.ResumeName(repoID, jb.in.ID)
	// The bucket refuses the image's record being marked ready, once.
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1, Respond: forbidden,
		Match: func(r *http.Request) bool {
			if r.URL.Query().Get("name") != "images/5001/"+resume+".json" {
				return false
			}
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			return bytes.Contains(body, []byte(`"state":"ready"`))
		}})
	jb.preempt()
	e.pass()
	if rec, _ := e.record("images/5001/" + resume + ".json"); rec["state"] != disks.ImageMaking {
		t.Fatalf("resume image record = %v, want it still being made", rec)
	}
	if im, ok := e.images()[resume]; !ok || im.Status != "READY" {
		t.Fatalf("resume image = %+v, %v; want it made", im, ok)
	}
	if err := e.disks.Compute.DeleteImage(context.Background(), resume, compute.RequestID("delete", resume)); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.pass()
	}
	if rec, _ := e.record("images/5001/" + resume + ".json"); rec["state"] != disks.ImageReady || rec["attempt"] != float64(1) {
		t.Fatalf("resume image record = %v, want it made again, ready, under a second attempt", rec)
	}
	if got, ok := e.images()[resume]; !ok || got.Status != "READY" {
		t.Fatalf("resume image = %+v, %v, want it made again", got, ok)
	}
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
		t.Fatal("the machine outlived its resume image")
	}
	if src := e.admit(run, e.rerunJob(run, 2, "build")).source(); src != resume {
		t.Fatalf("the rerun's job started from %q, want the resume image %s", src, resume)
	}
}
