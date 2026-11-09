package disks_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/emulators/google"
	"github.com/lanescape/runners/internal/disks"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/selector"
)

const spot = "lanescape-2vcpu-linux-amd64-spot-main"

// preempt preempts an admitted job's Spot machine, and GitHub fails the job
// whose runner it lost.
func (jb *job) preempt() {
	jb.e.t.Helper()
	if err := jb.e.gce.Preempt(jb.in.Zone, jb.in.Name); err != nil {
		jb.e.t.Fatal(err)
	}
	jb.e.gh.CompleteJobByID(jb.j.ID, "failure")
}

// rerunJob is the job of a name in a run's attempt.
func (e *env) rerunJob(run *ghemu.Run, attempt int, name string) *ghemu.WorkflowJob {
	e.t.Helper()
	for _, j := range e.gh.RunJobs(run, attempt) {
		if j.Name == name && j.Status == "queued" {
			return &ghemu.WorkflowJob{ID: j.ID}
		}
	}
	e.t.Fatalf("no queued job %s in attempt %d", name, attempt)
	return nil
}

func (e *env) attempt(run *ghemu.Run) int {
	e.t.Helper()
	repo, err := e.disks.GitHub.RepositoryByID(context.Background(), repoID)
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := e.disks.GitHub.WorkflowRun(context.Background(), repo, run.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return r.Attempt
}

func TestAPreemptedJobResumes(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	if jb.in.Provisioning != "SPOT" {
		t.Fatalf("machine is %s", jb.in.Provisioning)
	}
	jb.preempt()
	e.pass()
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
		t.Fatal("a preempted machine wasn't deleted")
	}
	rec, ok := e.record("preemptions/5001/" + strconv.FormatInt(run.ID, 10) + "/" + strconv.FormatUint(jb.in.ID, 10) + ".json")
	if !ok || rec["preempted"] != disks.Preempted || rec["job_id"] != float64(jb.j.ID) || rec["job_name"] != "build" ||
		rec["run_attempt"] != float64(1) || rec["selector"] != spot || rec["zone"] != jb.in.Zone || rec["runner_id"] == float64(0) {
		t.Fatalf("preemption record = %v", rec)
	}
	resume := disks.ResumeName(repoID, jb.in.ID)
	im, ok := e.images()[resume]
	if !ok || im.Labels[disks.LabelKind] != disks.KindResume || im.Labels[disks.LabelRun] != strconv.FormatInt(run.ID, 10) ||
		im.Labels[disks.LabelSelector] != selector.Hash(spot) || im.Labels[disks.LabelJobName] != selector.Hash("build") {
		t.Fatalf("resume image = %+v, %v", im, ok)
	}
	// The run has finished with its Spot job lost, so it's rerun.
	if a := e.attempt(run); a != 2 {
		t.Fatalf("attempt %d; the run should have been rerun", a)
	}
	if rr, ok := e.record("reruns/5001/" + strconv.FormatInt(run.ID, 10) + ".json"); !ok || len(rr["attempts"].([]any)) != 1 {
		t.Fatalf("rerun record = %v", rr)
	}
	// The rerun's job of the same name resumes from it, claimed by its ID.
	j2 := e.rerunJob(run, 2, "build")
	again := e.admit(run, j2)
	if src := again.source(); src != resume {
		t.Fatalf("the rerun's job started from %q, want the resume image %s", src, resume)
	}
	claim, ok := e.record("claims/5001/" + resume + ".json")
	if !ok || claim["job_id"] != float64(j2.ID) {
		t.Fatalf("claim = %v", claim)
	}
	// Once its job has finished, the resume image goes.
	again.finish(false)
	if e.has(resume) {
		t.Fatal("a resume image outlived its job")
	}
	if a := e.attempt(run); a != 2 {
		t.Fatalf("a run whose rerun succeeded was rerun again: attempt %d", a)
	}
}

func TestOnlyAJobOfAUniqueNameResumes(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	a := e.start(run, ghemu.JobSpec{Name: "test", Labels: []string{spot}})
	b := e.start(run, ghemu.JobSpec{Name: "test", Labels: []string{spot}})
	a.preempt()
	b.finish(false)
	e.pass()
	if e.attempt(run) != 2 {
		t.Fatal("the run wasn't rerun")
	}
	j := e.rerunJob(run, 2, "test")
	if src := e.admit(run, j).source(); src != "" {
		t.Fatalf("a job whose name its matrix shares resumed from %s", src)
	}
}

func TestAnotherJobNeverGetsAResumeImage(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	a := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	a.preempt()
	e.pass()
	resume := disks.ResumeName(repoID, a.in.ID)
	// The rerun's job claims it; another job of the rerun of the same name
	// can't be, so a second machine for the same job ID is the one case.
	j := e.rerunJob(run, 2, "build")
	first := e.admit(run, j)
	if first.source() != resume {
		t.Fatal("no resume")
	}
	// Another run's job of the same name and selector doesn't get it.
	other := e.mainRun()
	if src := e.start(other, ghemu.JobSpec{Name: "build", Labels: []string{spot}}).source(); src != "" {
		t.Fatalf("another run's job started from %s", src)
	}
}

func TestAReadRunIsRerunWithoutAResumeImage(t *testing.T) {
	e := setup(t)
	run := e.gh.EventRun(e.repo, "workflow_dispatch", "main")
	if e.bind(run) != "read" {
		t.Fatal("not read")
	}
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{"lanescape-2vcpu-linux-amd64-spot-read"}})
	jb.preempt()
	e.pass()
	if len(e.images()) != 0 {
		t.Fatalf("images %v; the read scope saves nothing", e.images())
	}
	if e.attempt(run) != 2 {
		t.Fatal("a read run that lost a Spot job wasn't rerun")
	}
}

func TestEvidenceFromTheRecordedEvent(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	jb.preempt()
	entry, _ := e.gce.LogEntry(jb.in.Zone, jb.in.Name, "compute.instances.preempted")
	// Google forgets the operation before the pass sees it.
	e.gce.ForgetOperations()
	e.pass()
	key := "preemptions/5001/" + strconv.FormatInt(run.ID, 10) + "/" + strconv.FormatUint(jb.in.ID, 10) + ".json"
	if rec, _ := e.record(key); rec["preempted"] != disks.Unknown {
		t.Fatalf("record = %v; want unknown", rec)
	}
	if e.attempt(run) != 1 {
		t.Fatal("a run whose job was never shown preempted was rerun")
	}
	if !e.has(disks.ResumeName(repoID, jb.in.ID)) {
		t.Fatal("no resume image while the evidence is unknown")
	}
	// The event arrives late, through the push subscription.
	if err := e.disks.RecordEvent(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	e.pass()
	if rec, _ := e.record(key); rec["preempted"] != disks.Preempted {
		t.Fatalf("record = %v; want yes", rec)
	}
	if e.attempt(run) != 2 {
		t.Fatal("the run wasn't rerun once its job was shown preempted")
	}
}

func TestEventsOfOtherKinds(t *testing.T) {
	e := setup(t)
	jb := e.start(e.mainRun(), ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	shutdown, _ := e.gce.LogEntry(jb.in.Zone, jb.in.Name, "compute.instances.guestTerminate")
	if _, ok := e.disks.ParseEvent(shutdown); ok {
		t.Fatal("a guest's shutdown was read as a preemption")
	}
	preempted, _ := e.gce.LogEntry(jb.in.Zone, jb.in.Name, "compute.instances.preempted")
	ev, ok := e.disks.ParseEvent(preempted)
	if !ok || ev.InstanceID != jb.in.ID || ev.Machine != jb.in.Name || ev.Zone != jb.in.Zone {
		t.Fatalf("event = %+v, %v", ev, ok)
	}
	other := *e.disks
	other.Project = "someone-else"
	if _, ok := other.ParseEvent(preempted); ok {
		t.Fatal("another project's event was read")
	}
	for _, junk := range []string{"", "{}", "not json", `{"protoPayload": {"methodName": "compute.instances.preempted"}}`} {
		if _, ok := e.disks.ParseEvent([]byte(junk)); ok {
			t.Fatalf("%q read as an event", junk)
		}
	}
	// A push's data is the entry, as Pub/Sub delivers it.
	if len(google.Push(e.gce.Project, "s", "1", preempted)) == 0 {
		t.Fatal("no push")
	}
}

func TestReruns(t *testing.T) {
	t.Run("up to three attempts", func(t *testing.T) {
		e := setup(t)
		run := e.mainRun()
		jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
		jb.preempt()
		e.pass()
		jb = e.admit(run, e.rerunJob(run, 2, "build"))
		jb.preempt()
		e.pass()
		if e.attempt(run) != 3 {
			t.Fatalf("attempt %d, want 3", e.attempt(run))
		}
		jb = e.admit(run, e.rerunJob(run, 3, "build"))
		jb.preempt()
		e.pass()
		e.pass()
		if e.attempt(run) != 3 {
			t.Fatalf("a run was rerun past 3 attempts: %d", e.attempt(run))
		}
	})
	t.Run("not while a job runs", func(t *testing.T) {
		e := setup(t)
		run := e.mainRun()
		lost := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
		other := e.start(run, ghemu.JobSpec{Name: "lint", Labels: []string{"lanescape-2vcpu-linux-amd64-main"}})
		lost.preempt()
		e.pass()
		if e.attempt(run) != 1 {
			t.Fatal("a run was rerun before it finished")
		}
		other.finish(false)
		if e.attempt(run) != 2 {
			t.Fatal("the run wasn't rerun once it finished")
		}
		// Rerunning failed jobs carried the one that succeeded.
		for _, j := range e.gh.RunJobs(run, 2) {
			if j.Name == "lint" && j.Status != "completed" {
				t.Fatalf("lint was rerun: %+v", j)
			}
		}
	})
	t.Run("a third attempt whose rerun failed once", func(t *testing.T) {
		e := setup(t)
		run := e.mainRun()
		jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
		jb.preempt()
		e.pass()
		jb = e.admit(run, e.rerunJob(run, 2, "build"))
		jb.preempt()
		e.gh.ServerError("/repos/lanescape/app/actions/runs/"+strconv.FormatInt(run.ID, 10)+"/rerun-failed-jobs", 502, 1)
		e.pass()
		if e.attempt(run) != 2 {
			t.Fatal("rerun despite the error")
		}
		e.pass()
		if e.attempt(run) != 3 {
			t.Fatalf("attempt %d; the failed rerun wasn't asked for again", e.attempt(run))
		}
	})
	t.Run("an answer lost after GitHub began the rerun", func(t *testing.T) {
		e := setup(t)
		run := e.mainRun()
		jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
		jb.preempt()
		path := "/repos/lanescape/app/actions/runs/" + strconv.FormatInt(run.ID, 10) + "/rerun-failed-jobs"
		e.gh.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1, After: true,
			Match:   func(r *http.Request) bool { return r.URL.Path == path },
			Respond: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }})
		e.pass()
		e.pass()
		if e.attempt(run) != 2 {
			t.Fatalf("attempt %d; a rerun whose answer was lost counts once, and isn't asked for again", e.attempt(run))
		}
		rr, _ := e.record("reruns/5001/" + strconv.FormatInt(run.ID, 10) + ".json")
		if n := len(rr["attempts"].([]any)); n != 1 {
			t.Fatalf("rerun record = %v; want one attempt", rr)
		}
	})
	t.Run("a GitHub error before the rerun", func(t *testing.T) {
		e := setup(t)
		run := e.mainRun()
		jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
		jb.preempt()
		e.gh.ServerError("/repos/lanescape/app/actions/runs/"+strconv.FormatInt(run.ID, 10)+"/rerun-failed-jobs", 502, 1)
		e.pass()
		if e.attempt(run) != 1 {
			t.Fatal("rerun despite the error")
		}
		e.pass()
		if e.attempt(run) != 2 {
			t.Fatal("not rerun after GitHub came back")
		}
	})
}

func TestADayCountsFromTheStop(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	stopped := e.clock.Now().UTC()
	if err := e.gce.Preempt(jb.in.Zone, jb.in.Name); err != nil {
		t.Fatal(err)
	}
	// The service was down for an hour.
	e.clock.Advance(time.Hour)
	e.pass()
	rec, _ := e.record("preemptions/5001/" + strconv.FormatInt(run.ID, 10) + "/" + strconv.FormatUint(jb.in.ID, 10) + ".json")
	at, _ := time.Parse(time.RFC3339Nano, rec["stopped_at"].(string))
	// Compute Engine's timestamps are in milliseconds.
	if d := at.Sub(stopped); d < -time.Second || d > time.Second {
		t.Fatalf("stopped at %s, but the machine stopped at %s", at, stopped)
	}
	img, _ := e.record("images/5001/" + disks.ResumeName(repoID, jb.in.ID) + ".json")
	if img["size_gb"] != float64(50) {
		t.Fatalf("resume image record = %v; want its disk's size", img)
	}
	e.clock.Advance(disks.ResumeKept - time.Hour + time.Minute)
	e.pass()
	if e.has(disks.ResumeName(repoID, jb.in.ID)) {
		t.Fatal("a resume image outlived a day from its machine's stop")
	}
}

func TestAClaimedResumeImageGoesAfterADayAllTheSame(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	jb.preempt()
	e.pass()
	resume := disks.ResumeName(repoID, jb.in.ID)
	e.clock.Advance(disks.ResumeKept - time.Hour)
	// The rerun's job claims it late in its day, and runs on.
	again := e.admit(run, e.rerunJob(run, 2, "build"))
	if again.source() != resume {
		t.Fatal("no resume")
	}
	e.clock.Advance(2 * time.Hour)
	e.pass()
	if e.has(resume) {
		t.Fatal("a claimed resume image outlived its day")
	}
}

func TestAResumeImageGoesADayAfterItsMachineStopped(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	if err := e.gce.Preempt(jb.in.Zone, jb.in.Name); err != nil {
		t.Fatal(err)
	}
	// Its job's run never finishes, so it's never rerun.
	e.pass()
	resume := disks.ResumeName(repoID, jb.in.ID)
	if !e.has(resume) {
		t.Fatal("no resume image")
	}
	e.clock.Advance(disks.ResumeKept - time.Minute)
	e.pass()
	if !e.has(resume) {
		t.Fatal("a resume image went before a day")
	}
	e.clock.Advance(2 * time.Minute)
	e.pass()
	if e.has(resume) {
		t.Fatal("a resume image outlived its day")
	}
}

// A Spot machine that a pass first finds stopped past its age limit is
// recorded all the same, with no resume image, and its run rerun.
func TestAPreemptionPastTheAgeLimit(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	e.clock.Advance(machines.MaxAge - time.Minute)
	jb.preempt()
	e.clock.Advance(2 * time.Minute)
	e.pass()
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
		t.Fatal("a machine past its age limit wasn't deleted")
	}
	rec, ok := e.record("preemptions/5001/" + strconv.FormatInt(run.ID, 10) + "/" + strconv.FormatUint(jb.in.ID, 10) + ".json")
	if !ok || rec["preempted"] != disks.Preempted {
		t.Fatalf("preemption record = %v", rec)
	}
	if len(e.images()) != 0 {
		t.Fatalf("images %v; a machine past its age limit saves nothing", e.images())
	}
	if e.attempt(run) != 2 {
		t.Fatal("the run wasn't rerun")
	}
}

// An automatic rerun keeps its run's restore choices.
func TestAnAutomaticRerunKeepsItsRunsChoice(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	run := e.mainRun()
	label := "lanescape-2vcpu-linux-amd64-spot-disk-build-" + key1 + "-main"
	lost := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{label}})
	if lost.source() != old {
		t.Fatal("no restore")
	}
	e.clock.Advance(time.Minute)
	newer := e.saveMain(key1)
	lost.preempt()
	e.pass()
	if e.attempt(run) != 2 {
		t.Fatal("not rerun")
	}
	if !e.has(old) {
		t.Fatal("the choice of a run due a rerun lost its image")
	}
	if src := e.admit(run, e.rerunJob(run, 2, "build")).source(); src != disks.ResumeName(repoID, lost.in.ID) {
		t.Fatalf("the rerun's lost job started from %q, not its resume image", src)
	}
	other := e.start(run, ghemu.JobSpec{Name: "test", Labels: []string{label}})
	if src := other.source(); src != old || src == newer {
		t.Fatalf("an automatic rerun's job started from %q, want its run's choice %s", src, old)
	}
}

// Past its age limit, a stopped Spot machine waits for its record, however
// old it is.
func TestAPreemptionPastTheAgeLimitWaitsForItsRecord(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	e.clock.Advance(machines.MaxAge + time.Minute)
	jb.preempt()
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1,
		Match:   func(r *http.Request) bool { return strings.HasPrefix(r.URL.Query().Get("name"), "preemptions/") },
		Respond: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }})
	e.pass()
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); !ok {
		t.Fatal("a stopped Spot machine went without its record")
	}
	e.pass()
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
		t.Fatal("the machine wasn't deleted once recorded")
	}
	if e.attempt(run) != 2 {
		t.Fatal("the run wasn't rerun")
	}
}

// A rerun the service asked for and GitHub refused isn't the service's:
// a maintainer's rerun after it picks again.
func TestARefusedRerunThenAMaintainersPicksAgain(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	run := e.mainRun()
	label := "lanescape-2vcpu-linux-amd64-spot-disk-build-" + key1 + "-main"
	lost := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{label}})
	if lost.source() != old {
		t.Fatal("no restore")
	}
	e.clock.Advance(time.Minute)
	newer := e.saveMain(key1)
	lost.preempt()
	e.gh.ServerError("/repos/lanescape/app/actions/runs/"+strconv.FormatInt(run.ID, 10)+"/rerun-failed-jobs", http.StatusForbidden, 1)
	e.pass()
	if e.attempt(run) != 1 {
		t.Fatal("rerun despite GitHub's refusal")
	}
	e.gh.Rerun(run)
	if src := e.start(run, ghemu.JobSpec{Name: "test", Labels: []string{label}}).source(); src != newer {
		t.Fatalf("a maintainer's rerun started from %q, want the newest %s", src, newer)
	}
}

// A repository transferred out of the organization is no longer a
// consumer: nothing of it is rerun.
func TestATransferredRepositoryIsntRerun(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	e.gh.Transfer(e.repo, e.gh.Org(1234, "elsewhere"))
	jb.preempt()
	e.pass()
	if a := e.gh.RunJobs(run, 2); len(a) != 0 {
		t.Fatal("a transferred repository's run was rerun")
	}
}

// Without its admission, a stopped Spot machine past its age limit isn't
// known to have had no job: it waits for the records.
func TestAPreemptionPastTheAgeLimitWaitsForTheRecords(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	e.clock.Advance(machines.MaxAge + time.Minute)
	jb.preempt()
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1,
		Match:   func(r *http.Request) bool { return r.URL.Query().Get("prefix") == "machines/" },
		Respond: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }})
	e.pass()
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); !ok {
		t.Fatal("a stopped Spot machine went while its admission couldn't be read")
	}
	e.pass()
	if e.attempt(run) != 2 {
		t.Fatal("the run wasn't rerun once the records were read")
	}
}

// A Spot machine still running past its age limit is stopped and recorded
// before it's deleted, so a preemption after the pass read it isn't lost.
func TestARunningSpotMachinePastTheAgeLimitIsRecorded(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	e.clock.Advance(machines.MaxAge + time.Minute)
	e.pass()
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
		t.Fatal("a machine past its age limit wasn't deleted")
	}
	rec, ok := e.record("preemptions/5001/" + strconv.FormatInt(run.ID, 10) + "/" + strconv.FormatUint(jb.in.ID, 10) + ".json")
	if !ok || rec["preempted"] != disks.Unknown {
		t.Fatalf("record = %v; want one, with no evidence", rec)
	}
	if len(e.images()) != 0 {
		t.Fatal("a machine past its age limit saved something")
	}
}

// A running Spot machine past its age limit is stopped even while the
// records can't be read, and waits for them.
func TestARunningSpotMachinePastTheAgeLimitStopsWithoutTheRecords(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	jb := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{spot}})
	e.clock.Advance(machines.MaxAge + time.Minute)
	e.gcs.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 1,
		Match:   func(r *http.Request) bool { return r.URL.Query().Get("prefix") == "machines/" },
		Respond: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }})
	e.pass()
	in, ok := e.gce.Instance(jb.in.Zone, jb.in.Name)
	if !ok || in.Status != "TERMINATED" {
		t.Fatalf("machine %v, %s; want it stopped and kept", ok, in.Status)
	}
	e.pass()
	if _, ok := e.gce.Instance(jb.in.Zone, jb.in.Name); ok {
		t.Fatal("not deleted once the records were read")
	}
	if _, ok := e.record("preemptions/5001/" + strconv.FormatInt(run.ID, 10) + "/" + strconv.FormatUint(jb.in.ID, 10) + ".json"); !ok {
		t.Fatal("not recorded")
	}
}

// A rerun GitHub refused, whose run then can't be read back, isn't the
// service's: a maintainer's rerun after it picks again.
func TestARefusedRerunWithNoReadBack(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	run := e.mainRun()
	label := "lanescape-2vcpu-linux-amd64-spot-disk-build-" + key1 + "-main"
	lost := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{label}})
	if lost.source() != old {
		t.Fatal("no restore")
	}
	e.clock.Advance(time.Minute)
	newer := e.saveMain(key1)
	lost.preempt()
	runPath := "/repos/lanescape/app/actions/runs/" + strconv.FormatInt(run.ID, 10)
	e.gh.ServerError(runPath+"/rerun-failed-jobs", http.StatusForbidden, 1)
	// The read-back after the refusal fails; the pass's own reads before
	// it don't.
	var refused atomic.Bool
	e.gh.Faults().Add(fault.Rule{Method: http.MethodGet, Times: 0,
		Match: func(r *http.Request) bool {
			if r.URL.Path == runPath+"/rerun-failed-jobs" {
				refused.Store(true)
			}
			return refused.Load() && r.URL.Path == runPath
		},
		Respond: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }})
	e.pass()
	e.gh.Faults().Reset()
	if e.attempt(run) != 1 {
		t.Fatal("rerun despite GitHub's refusal")
	}
	rr, _ := e.record("reruns/5001/" + strconv.FormatInt(run.ID, 10) + ".json")
	if started, _ := rr["started"].([]any); len(started) != 0 {
		t.Fatalf("rerun record = %v; a refused rerun is recorded as the service's", rr)
	}
	e.gh.Rerun(run)
	if src := e.start(run, ghemu.JobSpec{Name: "test", Labels: []string{label}}).source(); src != newer {
		t.Fatalf("a maintainer's rerun started from %q, want the newest %s", src, newer)
	}
}

// A rerun GitHub refuses because a maintainer's has begun isn't the
// service's.
func TestARefusalWhileAMaintainersRerunBegins(t *testing.T) {
	e := setup(t)
	old := e.saveMain(key1)
	run := e.mainRun()
	label := "lanescape-2vcpu-linux-amd64-spot-disk-build-" + key1 + "-main"
	lost := e.start(run, ghemu.JobSpec{Name: "build", Labels: []string{label}})
	if lost.source() != old {
		t.Fatal("no restore")
	}
	e.clock.Advance(time.Minute)
	newer := e.saveMain(key1)
	lost.preempt()
	// The maintainer reruns it just as the service asks.
	path := "/repos/lanescape/app/actions/runs/" + strconv.FormatInt(run.ID, 10) + "/rerun-failed-jobs"
	e.gh.Faults().Add(fault.Rule{Method: http.MethodPost, Times: 1,
		Match: func(r *http.Request) bool { return r.URL.Path == path },
		Respond: func(w http.ResponseWriter, _ *http.Request) {
			e.gh.Rerun(run)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message": "This workflow is already running"}`))
		}})
	e.pass()
	rr, _ := e.record("reruns/5001/" + strconv.FormatInt(run.ID, 10) + ".json")
	if started, _ := rr["started"].([]any); len(started) != 0 {
		t.Fatalf("rerun record = %v; a refused rerun is recorded as the service's", rr)
	}
	if src := e.start(run, ghemu.JobSpec{Name: "test", Labels: []string{label}}).source(); src != newer {
		t.Fatalf("the maintainer's rerun started from %q, want the newest %s", src, newer)
	}
}
