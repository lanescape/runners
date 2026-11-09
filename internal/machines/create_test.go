package machines_test

import (
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/selector"
)

func TestAMachineForAQueuedJob(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.one()
	sel, _ := selector.Parse(sel2)
	if m.Labels[machines.LabelRepository] != "5001" || m.Labels[machines.LabelSelector] != sel.Hash() ||
		m.Labels[machines.LabelJob] != strconv.FormatInt(j.ID, 10) || m.Labels[machines.LabelContribution] != "false" ||
		m.Labels[machines.LabelCreated] != strconv.FormatInt(e.clock.Now().Unix(), 10) {
		t.Fatalf("labels = %v", m.Labels)
	}
	if m.Metadata[machines.MetaSelector] != sel2 || m.Metadata[machines.MetaService] != audience || m.Metadata[machines.MetaJITConfig] == "" ||
		m.Metadata[machines.MetaRunnerID] == "" {
		t.Fatal("metadata lacks the selector, the service or the runner's configuration")
	}
	if m.ServiceAccount != machines.MachineAccountOf(e.gce.Project) || !slices.Equal(m.Scopes, []string{"https://www.googleapis.com/auth/userinfo.email"}) ||
		m.Network != machines.Network || m.Subnetwork != machines.Subnetwork || !m.ExternalIP || m.MaxRun != 6*time.Hour || m.Provisioning != "STANDARD" ||
		m.MachineType != "n4-highcpu-2" || m.Zone != zoneA {
		t.Fatalf("machine = %+v", m)
	}
	if len(m.Disks) != 1 {
		t.Fatalf("a new machine has disks %v; it has a boot disk and no work disk yet", m.Disks)
	}
	for _, d := range e.gce.Disks() {
		if d.SizeGB != 100 || d.IOPS != 3000 || d.Throughput != 400 || d.Type != "hyperdisk-balanced" ||
			d.SourceImage == "" || d.SourceImage[len(d.SourceImage)-len("lsr-base-amd64-new"):] != "lsr-base-amd64-new" {
			t.Fatalf("boot disk = %+v; want the newest base image", d)
		}
	}
	gs := e.gh.RunnerGroups(e.org)
	g := gs[len(gs)-1]
	if g.Name != "lanescape-runners-5001" || g.Visibility != "selected" || !slices.Equal(g.Repositories, []int64{repoID}) || !g.AllowsPublicRepositories {
		t.Fatalf("runner group = %+v", g)
	}
	rs := e.gh.Runners()
	if len(rs) != 1 || rs[0].Name != m.Name || rs[0].Group.ID != g.ID || !slices.Equal(rs[0].Labels, []string{sel2, "self-hosted", "linux", "x64"}) {
		t.Fatalf("runners = %+v", rs)
	}
	// A machine being made or idle counts against the queue.
	e.pass()
	e.one()
	e.queue(run, sel2)
	e.pass()
	e.wantMachines(2)
	if e.logged(m.Metadata[machines.MetaJITConfig]) {
		t.Fatal("a just-in-time configuration was logged")
	}
}

func TestNoMachine(t *testing.T) {
	for name, tc := range map[string]func(e *env){
		"a run with no binding": func(e *env) { e.queue(e.gh.PushRun(e.repo, "main"), sel2) },
		"a scope that isn't its run's": func(e *env) {
			e.queue(e.mainRun(), "lanescape-2vcpu-linux-amd64-pr1")
		},
		"a fork's pull request": func(e *env) {
			e.queue(e.forkPull(), "lanescape-2vcpu-linux-amd64-read")
		},
		"an unapproved contribution": func(e *env) {
			run, p := e.contribution(false)
			e.gh.Queue(run, ghemu.JobSpec{Labels: []string{"lanescape-2vcpu-linux-amd64-pr" + strconv.Itoa(p.Number)}})
		},
		"an unknown shape":     func(e *env) { e.queue(e.mainRun(), "lanescape-4vcpu-linux-amd64-main") },
		"another label":        func(e *env) { e.queue(e.mainRun(), sel2, "gpu") },
		"two selectors":        func(e *env) { e.queue(e.mainRun(), sel2, "lanescape-80vcpu-linux-amd64-main") },
		"only self-hosted":     func(e *env) { e.queue(e.mainRun(), "self-hosted") },
		"not a selector":       func(e *env) { e.queue(e.mainRun(), "lanescape-2vcpu-linux-amd64") },
		"GitHub's own runners": func(e *env) { e.queue(e.mainRun(), "ubuntu-24.04") },
		"another architecture": func(e *env) { e.queue(e.mainRun(), sel2, "arm64") },
		"a job still in review": func(e *env) {
			e.gh.Queue(e.mainRun(), ghemu.JobSpec{Labels: []string{sel2}, Environment: "contribution"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			tc(e)
			e.pass()
			e.wantMachines(0)
		})
	}
}

func TestAnApprovedContribution(t *testing.T) {
	e := setup(t)
	run, p := e.contribution(true)
	sel := "lanescape-2vcpu-linux-amd64-pr" + strconv.Itoa(p.Number)
	e.gh.Queue(run, ghemu.JobSpec{Labels: []string{sel}, Environment: machines.ContributionEnvironment})
	e.pass()
	if m := e.one(); m.Labels[machines.LabelContribution] != "true" {
		t.Fatalf("labels = %v", m.Labels)
	}
}

func TestForkApprovalSetting(t *testing.T) {
	e := setup(t)
	e.gh.SetForkApproval(e.org, ghemu.ApproveFirstTimeContributors)
	e.queue(e.mainRun(), sel2)
	e.pass()
	e.wantMachines(0)
	if !e.logged("fork pull request approval setting") {
		t.Fatal("the reason wasn't logged")
	}
	e.gh.SetForkApproval(e.org, ghemu.ApproveAllExternalContributors)
	e.pass()
	e.wantMachines(1)
}

// Every consumer draws on one pool, and contributions on the same one:
// nothing but Compute Engine's quota and capacity holds a job back.
func TestOnePool(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	for range 3 {
		e.queue(run, sel2)
	}
	crun, p := e.contribution(true)
	csel := "lanescape-2vcpu-linux-amd64-pr" + strconv.Itoa(p.Number)
	for range 2 {
		e.gh.Queue(crun, ghemu.JobSpec{Labels: []string{csel}, Environment: machines.ContributionEnvironment})
	}
	other := e.gh.Repo(e.org, repoID+1, "other")
	orun := e.gh.PushRun(other, "main")
	e.bind(orun, ghemu.Job{}, binding.Request{})
	for range 2 {
		e.queue(orun, sel2)
	}
	e.pass()
	ms := e.wantMachines(7)
	contributions := 0
	for _, m := range ms {
		if m.Labels[machines.LabelContribution] == "true" {
			contributions++
		}
	}
	if contributions != 2 {
		t.Fatalf("%d contribution machines, want 2", contributions)
	}
}

// Machines go to queued jobs oldest first, across consumers. A job that
// quota or capacity holds back stays queued, and isn't overtaken by a
// younger job that could have had its machine; jobs of other machine types
// go on.
func TestOldestFirst(t *testing.T) {
	e := setup(t)
	// Room for one 2-vCPU machine.
	e.gce.SetQuota("northamerica-northeast1", "N4_CPUS", 2)
	e.gce.SetQuota("northamerica-northeast1", "C4_CPUS", 0)
	other := e.gh.Repo(e.org, repoID+1, "other")
	orun := e.gh.PushRun(other, "main")
	e.bind(orun, ghemu.Job{}, binding.Request{})
	oldest := e.queue(orun, sel2)
	// GitHub gives jobs' times to the second.
	time.Sleep(1100 * time.Millisecond)
	run := e.mainRun()
	younger := e.queue(run, sel2)
	youngest := e.queue(run, sel2)
	arm := e.queue(run, "lanescape-16vcpu-linux-arm64-main")
	e.pass()
	ms := e.wantMachines(2)
	byJob := map[string]bool{}
	for _, m := range ms {
		byJob[m.Labels[machines.LabelJob]] = true
	}
	if !byJob[strconv.FormatInt(oldest.ID, 10)] || !byJob[strconv.FormatInt(arm.ID, 10)] {
		t.Fatalf("machines for jobs %v; want the oldest's and the Arm job's", byJob)
	}
	if e.gh.Job(younger.ID).Status != "queued" || e.gh.Job(youngest.ID).Status != "queued" {
		t.Fatal("a job held back by quota isn't queued")
	}
	// Once there's room, the next oldest gets its machine.
	e.gce.SetQuota("northamerica-northeast1", "N4_CPUS", 4)
	e.pass()
	ms = e.wantMachines(3)
	byJob = map[string]bool{}
	for _, m := range ms {
		byJob[m.Labels[machines.LabelJob]] = true
	}
	if !byJob[strconv.FormatInt(younger.ID, 10)] || byJob[strconv.FormatInt(youngest.ID, 10)] {
		t.Fatalf("machines for jobs %v; want the younger's before the youngest's", byJob)
	}
}

func TestPlacement(t *testing.T) {
	big := "lanescape-80vcpu-linux-amd64-main"
	bigSpot := "lanescape-80vcpu-linux-amd64-spot-main"
	for _, tc := range []struct {
		name       string
		sel        string
		short      func(e *env)
		zone, typ  string
		spot       bool
		noMachines bool
	}{
		{"the first zone", big, func(*env) {}, zoneA, "n4-highcpu-80", false, false},
		{"a stockout in the first zone", big, func(e *env) { e.gce.Stockout(zoneA, "n4-highcpu-80", false) }, zoneB, "n4-highcpu-80", false, false},
		{"no N4 capacity", big, func(e *env) {
			e.gce.Stockout(zoneA, "n4-highcpu-80", false)
			e.gce.Stockout(zoneB, "n4-highcpu-80", false)
		}, zoneA, "c4-highcpu-96", false, false},
		{"a capacity refusal in the first zone", big, func(e *env) { e.gce.RefuseForCapacity(zoneA, "n4-highcpu-80", false) }, zoneB, "n4-highcpu-80", false, false},
		{"no N4 quota", big, func(e *env) { e.gce.SetQuota("northamerica-northeast1", "N4_CPUS", 24) }, zoneA, "c4-highcpu-96", false, false},
		{"no capacity anywhere", big, func(e *env) {
			for _, z := range []string{zoneA, zoneB} {
				e.gce.Stockout(z, "n4-highcpu-80", false)
				e.gce.Stockout(z, "c4-highcpu-96", false)
			}
		}, "", "", false, true},
		{"spot", bigSpot, func(*env) {}, zoneA, "n4-highcpu-80", true, false},
		{"spot in another zone before standard", bigSpot, func(e *env) { e.gce.Stockout(zoneA, "n4-highcpu-80", true) }, zoneB, "n4-highcpu-80", true, false},
		{"spot on another machine type before standard", bigSpot, func(e *env) {
			e.gce.Stockout(zoneA, "n4-highcpu-80", true)
			e.gce.Stockout(zoneB, "n4-highcpu-80", true)
		}, zoneA, "c4-highcpu-96", true, false},
		{"standard only for want of spot capacity", bigSpot, func(e *env) {
			for _, z := range []string{zoneA, zoneB} {
				e.gce.Stockout(z, "n4-highcpu-80", true)
				e.gce.Stockout(z, "c4-highcpu-96", true)
			}
		}, zoneA, "n4-highcpu-80", false, false},
		{"no spot quota", bigSpot, func(e *env) { e.gce.SetQuota("northamerica-northeast1", "PREEMPTIBLE_CPUS", 8) }, zoneA, "n4-highcpu-80", false, false},
		{"Compute Engine failing", big, func(e *env) { e.gce.ServerError(http.MethodPost, "/instances", http.StatusInternalServerError, 0) }, "", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			tc.short(e)
			e.queue(e.mainRun(), tc.sel)
			e.pass()
			if tc.noMachines {
				e.wantMachines(0)
				if len(e.gh.Runners()) != 0 && tc.name != "Compute Engine failing" {
					t.Fatal("a runner registration was left for a machine never made")
				}
				return
			}
			m := e.one()
			if m.Zone != tc.zone || m.MachineType != tc.typ || (m.Provisioning == "SPOT") != tc.spot {
				t.Fatalf("machine in %s, %s, %s; want %s, %s, spot %v", m.Zone, m.MachineType, m.Provisioning, tc.zone, tc.typ, tc.spot)
			}
		})
	}
}

func TestAnArmMachine(t *testing.T) {
	e := setup(t)
	e.queue(e.mainRun(), "lanescape-16vcpu-linux-arm64-main")
	e.pass()
	m := e.one()
	if m.MachineType != "c4a-highcpu-16" {
		t.Fatalf("machine type %s", m.MachineType)
	}
	rs := e.gh.Runners()
	if !slices.Equal(rs[0].Labels, []string{"lanescape-16vcpu-linux-arm64-main", "self-hosted", "linux", "arm64"}) {
		t.Fatalf("runner labels = %v", rs[0].Labels)
	}
}

func TestARunnerGroupIsKeptToItsRepository(t *testing.T) {
	e := setup(t)
	e.queue(e.mainRun(), sel2)
	e.pass()
	// Someone widens the group.
	g := e.gh.RunnerGroups(e.org)[1]
	e.gh.Repo(e.org, 5002, "other")
	e.gh.SetGroupRepositories(g.ID, []int64{repoID, 5002})
	e.queue(e.mainRun(), sel2)
	e.pass()
	if got := e.gh.RunnerGroups(e.org)[1]; !slices.Equal(got.Repositories, []int64{repoID}) {
		t.Fatalf("group's repositories = %v", got.Repositories)
	}
}
