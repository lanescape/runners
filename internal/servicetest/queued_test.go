package servicetest

import (
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	ghemu "github.com/lanescape/runners/emulators/github"
)

// TestAQueuedDeliveryFindsARunTheListsLeftOut: GitHub's lists by status
// can leave a queued run out for a while, as they did a repository's first
// CD run. Its job's signed queued delivery, through the webhook and
// Pub/Sub, leaves a hint, and the pass gives the job a machine.
func TestAQueuedDeliveryFindsARunTheListsLeftOut(t *testing.T) {
	w := New(t, clockwork.NewFakeClockAt(time.Now()))
	r := w.Repository("app")
	run := w.GitHub.PushRun(w.repo(r), "main")
	run.Status, run.Unindexed = "queued", true
	labels := []string{"lanescape-2vcpu-linux-amd64"}
	j := w.GitHub.Queue(run, ghemu.JobSpec{Labels: labels})
	w.Pass()
	if n := machinesOf(w); n != 0 {
		t.Fatalf("%d machines before the delivery, want none", n)
	}
	w.deliver("workflow_job", w.GitHub.WorkflowJobPayload(run, w.Inst, "queued", j.ID, labels))
	w.Pass()
	if n := machinesOf(w); n != 1 {
		t.Fatalf("%d machines after the delivery, want 1", n)
	}
}

func machinesOf(w *World) int {
	n := 0
	for _, in := range w.Compute.Instances() {
		if strings.HasPrefix(in.Name, "lsr-") && !strings.HasPrefix(in.Name, "lsr-build-") {
			n++
		}
	}
	return n
}
