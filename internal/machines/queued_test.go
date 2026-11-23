package machines_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/machines"
)

// delivery is GitHub's workflow_job delivery for a job of a run, as the
// service reads it.
func delivery(t *testing.T, action string, run *ghemu.Run, owner int64, labels ...string) []byte {
	t.Helper()
	return jobDelivery(t, action, run, 1, owner, labels...)
}

func jobDelivery(t *testing.T, action string, run *ghemu.Run, job, owner int64, labels ...string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"action":       action,
		"workflow_job": map[string]any{"id": job, "run_id": run.ID, "labels": labels},
		"repository":   map[string]any{"id": run.Repo.ID, "owner": map[string]any{"id": owner}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// hints lists a run's hints, by name.
func (e *env) hints(run *ghemu.Run) []string {
	objs, err := e.svc.Objects.List(context.Background(), "queued/"+strconv.FormatInt(run.Repo.ID, 10)+"/"+strconv.FormatInt(run.ID, 10)+"/")
	if err != nil {
		e.t.Fatal(err)
	}
	var names []string
	for _, o := range objs {
		names = append(names, o.Name)
	}
	return names
}

func (e *env) hint(run *ghemu.Run) (string, bool) {
	names := e.hints(run)
	if len(names) == 0 {
		return "", false
	}
	return names[0], true
}

// TestAJobOfARunGitHubsListsLeftOut: GitHub's lists by status can leave a
// queued run out for a while, as they did a repository's first CD run, still
// queued and unbound. Its queued job's delivery leaves a hint, the pass looks
// the run up by ID, and the job gets a machine; without the hint, it waits.
func TestAJobOfARunGitHubsListsLeftOut(t *testing.T) {
	e := setup(t)
	run := e.gh.PushRun(e.repo, "main")
	run.Status, run.Unindexed = "queued", true
	j := e.queue(run, sel2)
	// Without a hint, the lists by status are all the pass reads.
	e.pass()
	e.wantMachines(0)

	if err := e.svc.RecordQueued(context.Background(), delivery(t, "queued", run, orgID, sel2)); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.hint(run); !ok {
		t.Fatal("no hint recorded")
	}
	e.pass()
	m := e.one()
	if m.Labels[machines.LabelJob] != strconv.FormatInt(j.ID, 10) {
		t.Fatalf("machine for job %s, want %d", m.Labels[machines.LabelJob], j.ID)
	}
	if !e.logged("a run GitHub's lists by status left out") {
		t.Fatal("the hinted run wasn't logged")
	}
	// Once the run has finished, its hint goes.
	run.Status = "completed"
	e.pass()
	if _, ok := e.hint(run); ok {
		t.Fatal("the finished run's hint stayed")
	}
}

// TestHintsComeOnlyFromQueuedJobsOfTheServicesSelectors: a delivery of
// anything else records nothing.
func TestHintsComeOnlyFromQueuedJobsOfTheServicesSelectors(t *testing.T) {
	e := setup(t)
	run := e.gh.PushRun(e.repo, "main")
	for name, data := range map[string][]byte{
		"completed":            delivery(t, "completed", run, orgID, sel2),
		"another organization": delivery(t, "queued", run, 1234, sel2),
		"GitHub's runners":     delivery(t, "queued", run, orgID, "ubuntu-26.04"),
		"not JSON":             []byte("{"),
	} {
		if err := e.svc.RecordQueued(context.Background(), data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, ok := e.hint(run); ok {
			t.Fatalf("%s: a hint was recorded", name)
		}
	}
	// A selector in any case is ours.
	if err := e.svc.RecordQueued(context.Background(), delivery(t, "queued", run, orgID, "Lanescape-2vcpu-linux-amd64")); err != nil {
		t.Fatal(err)
	}
	first, ok := e.hint(run)
	if !ok {
		t.Fatal("no hint for a selector in capitals")
	}
	_, gen, _ := e.gcs.Object("state", first)
	// The same push delivered again changes nothing; another job of the run,
	// such as a rerun's, has a hint of its own.
	if err := e.svc.RecordQueued(context.Background(), delivery(t, "queued", run, orgID, sel2)); err != nil {
		t.Fatal(err)
	}
	if _, again, _ := e.gcs.Object("state", first); again != gen {
		t.Fatal("a repeated delivery rewrote the hint")
	}
	if err := e.svc.RecordQueued(context.Background(), jobDelivery(t, "queued", run, 2, orgID, sel2)); err != nil {
		t.Fatal(err)
	}
	if n := len(e.hints(run)); n != 2 {
		t.Fatalf("%d hints for two jobs of a run, want 2", n)
	}
}

// TestOldOrDeadHintsGo: a hint older than HintLimit, or of a run GitHub no
// longer has, is dropped; one of a listed run is kept until the run ends.
func TestOldOrDeadHintsGo(t *testing.T) {
	e := setup(t)
	listed := e.mainRun()
	e.queue(listed, sel2)
	old := e.gh.PushRun(e.repo, "main")
	old.Status, old.Unindexed = "queued", true
	for _, run := range []*ghemu.Run{listed, old} {
		if err := e.svc.RecordQueued(context.Background(), delivery(t, "queued", run, orgID, sel2)); err != nil {
			t.Fatal(err)
		}
	}
	// Fresh, both stay through a pass.
	e.pass()
	if _, ok := e.hint(listed); !ok {
		t.Fatal("a fresh hint of a listed run went")
	}
	e.clock.Advance(machines.HintLimit + time.Minute)
	e.pass()
	if _, ok := e.hint(old); ok {
		t.Fatal("a hint older than HintLimit stayed")
	}
	if _, ok := e.hint(listed); ok {
		t.Fatal("the listed run's old hint stayed")
	}
	// A run GitHub no longer has.
}

// TestHintsOfRunsNotYetDoneStay: a hinted run that's listed, or waiting,
// keeps its hint through a pass; one whose lookup fails keeps it too, and
// leaves the queue incomplete, so the job's record of its first machine
// stays.
func TestHintsOfRunsNotYetDoneStay(t *testing.T) {
	e := setup(t)
	listed := e.mainRun()
	e.queue(listed, sel2)
	waiting := e.gh.PushRun(e.repo, "main")
	waiting.Status, waiting.Unindexed = "waiting", true
	left := e.gh.PushRun(e.repo, "main")
	left.Status, left.Unindexed = "queued", true
	j := e.queue(left, sel2)
	for _, run := range []*ghemu.Run{listed, waiting, left} {
		if err := e.svc.RecordQueued(context.Background(), delivery(t, "queued", run, orgID, sel2)); err != nil {
			t.Fatal(err)
		}
	}
	e.pass()
	for name, run := range map[string]*ghemu.Run{"listed": listed, "waiting": waiting, "left out": left} {
		if _, ok := e.hint(run); !ok {
			t.Fatalf("the %s run's hint went", name)
		}
	}
	e.wantMachines(2)
	record := "jobs/" + strconv.FormatInt(e.repo.ID, 10) + "/" + strconv.FormatInt(j.ID, 10) + ".json"
	if _, _, ok := e.gcs.Object("state", record); !ok {
		t.Fatal("the left-out job's first machine wasn't recorded")
	}
	// GitHub can't be asked about the left-out run: its hint and its job's
	// record stay.
	e.gh.Faults().Add(fault.Rule{Method: http.MethodGet, PathPrefix: "/repos/lanescape/app/actions/runs/" + strconv.FormatInt(left.ID, 10),
		Match:   func(r *http.Request) bool { return !strings.Contains(r.URL.Path, "/jobs") },
		Respond: fault.Status(http.StatusBadGateway, nil, "{}")})
	e.pass()
	e.gh.Faults().Reset()
	if !e.logged("reading a hinted run") {
		t.Fatal("the hinted run's lookup didn't fail")
	}
	if _, ok := e.hint(left); !ok {
		t.Fatal("a hint whose run couldn't be looked up went")
	}
	if _, _, ok := e.gcs.Object("state", record); !ok {
		t.Fatal("the job's record went while its run couldn't be looked up")
	}
}

// TestADepartedRepositorysHintsExpire: hints of a repository the service no
// longer serves go once they're older than HintLimit.
func TestADepartedRepositorysHintsExpire(t *testing.T) {
	e := setup(t)
	e.gcs.Put("state", "queued/99999/1/2.json", []byte(`{"run_id":1}`))
	e.gcs.Put("state", "queued/not-a-repository/x/y.json", []byte(`{}`))
	e.pass()
	if _, _, ok := e.gcs.Object("state", "queued/99999/1/2.json"); !ok {
		t.Fatal("a fresh hint went")
	}
	if _, _, ok := e.gcs.Object("state", "queued/not-a-repository/x/y.json"); ok {
		t.Fatal("a misnamed hint stayed")
	}
	e.clock.Advance(machines.HintLimit + time.Minute)
	e.pass()
	if _, _, ok := e.gcs.Object("state", "queued/99999/1/2.json"); ok {
		t.Fatal("an expired hint of a departed repository stayed")
	}
}

// TestAHintOutlastsANotFound: GitHub can answer 404 for a run it has
// while the App's access changes, so a hint whose run is answered so stays
// until it expires, the queue counting as incomplete meanwhile.
func TestAHintOutlastsANotFound(t *testing.T) {
	e := setup(t)
	gone := &ghemu.Run{ID: 999999, Repo: e.repo}
	if err := e.svc.RecordQueued(context.Background(), delivery(t, "queued", gone, orgID, sel2)); err != nil {
		t.Fatal(err)
	}
	e.pass()
	e.clock.Advance(11 * time.Minute)
	e.pass()
	if _, ok := e.hint(gone); !ok {
		t.Fatal("a hint went at GitHub's 404")
	}
	e.clock.Advance(machines.HintLimit)
	e.pass()
	if _, ok := e.hint(gone); ok {
		t.Fatal("an expired hint of a run GitHub doesn't have stayed")
	}
}
