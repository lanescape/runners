package machines_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lanescape/runners/config"
	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/bases"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/selector"
	baseimage "github.com/lanescape/runners/machine"
)

func TestAMachineForAQueuedJob(t *testing.T) {
	e := setup(t)
	run := e.mainRun()
	j := e.queue(run, sel2)
	e.pass()
	m := e.one()
	sel, _ := selector.Parse(e.gh.Job(j.ID).Labels[0])
	if len(m.Labels) != 5 || m.Labels[machines.LabelRepository] != "5001" || m.Labels[machines.LabelSelector] != sel.Hash() ||
		m.Labels[machines.LabelJob] != strconv.FormatInt(j.ID, 10) || m.Labels[machines.LabelContribution] != "false" ||
		m.Labels[machines.LabelCreated] != strconv.FormatInt(e.clock.Now().Unix(), 10) {
		t.Fatalf("labels = %v", m.Labels)
	}
	if m.Metadata[machines.MetaSelector] != sel2 || m.Metadata[machines.MetaService] != audience || m.Metadata[machines.MetaJITConfig] == "" ||
		m.Metadata[machines.MetaRunnerID] == "" {
		t.Fatal("metadata lacks the selector, the service or the runner's configuration")
	}
	if m.ServiceAccount != machines.MachineAccountOf(e.gce.Project) || !slices.Equal(m.Scopes, []string{"https://www.googleapis.com/auth/userinfo.email"}) ||
		m.Network != machines.Network || m.Subnetwork != machines.Subnetwork || !m.ExternalIP || m.MaxRun != machines.MaxAge+30*time.Minute || m.Provisioning != "STANDARD" ||
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
	if len(rs) != 1 || rs[0].Name != m.Name || rs[0].Group.ID != g.ID || !slices.Equal(rs[0].Labels, []string{sel.String()}) {
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
		"a fork's pull request":      func(e *env) { e.queue(e.forkPull(), sel2) },
		"an unknown shape":           func(e *env) { e.queue(e.mainRun(), "lanescape-4vcpu-linux-amd64") },
		"another label":              func(e *env) { e.queue(e.mainRun(), sel2, "gpu") },
		"two selectors":              func(e *env) { e.queue(e.mainRun(), sel2, "lanescape-80vcpu-linux-amd64") },
		"only self-hosted":           func(e *env) { e.queue(e.mainRun(), "self-hosted") },
		"a selector naming a run":    func(e *env) { e.queue(e.mainRun(), sel2+"-run-1") },
		"a selector naming an image": func(e *env) { e.queue(e.gh.PushRun(e.repo, "main"), sel2+"-image-build") },
		"a selector naming a disk":   func(e *env) { e.queue(e.mainRun(), sel2+"-disk-build") },
		"GitHub's own runners":       func(e *env) { e.queue(e.mainRun(), "ubuntu-24.04") },
		"another architecture":       func(e *env) { e.queue(e.mainRun(), sel2, "arm64") },
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

func TestAnUnboundRunsJobGetsAMachineOfItsSelector(t *testing.T) {
	e := setup(t)
	run := e.gh.PushRun(e.repo, "main")
	j := e.queue(run, "lanescape-80vcpu-linux-amd64-spot")
	e.pass()
	m := e.one()
	if m.MachineType != "n4-highcpu-80" || m.Provisioning != "SPOT" {
		t.Fatalf("machine = %+v; want one of the job's selector", m)
	}
	for _, d := range e.gce.Disks() {
		if !strings.HasSuffix(d.SourceImage, "lsr-base-amd64-new") {
			t.Fatalf("boot disk = %+v; every machine boots the base image", d)
		}
	}
	// Its check binds the run.
	b := e.boot(m)
	b.taken(j)
	if code := b.started(run.ID); code != http.StatusOK {
		t.Fatalf("job-started check: %d", code)
	}
	if bd, ok := e.record("bindings/5001/" + strconv.FormatInt(run.ID, 10) + ".json"); !ok || bd["scope"] != "main" {
		t.Fatalf("binding = %v", bd)
	}
}

// A job the service will fail gets an ordinary machine of its own selector,
// whose check answers it with the reason.
func TestAFailedJobsMachineIsOfItsOwnSelector(t *testing.T) {
	e := setup(t)
	e.repo.Commit("feature", map[string]string{"f": "1"})
	p := e.gh.PullRequest(e.repo, "main", e.repo, "feature", e.org)
	e.repo.Branch("copy", e.repo.Head("feature"))
	e.gh.PullRequest(e.repo, "main", e.repo, "copy", e.org)
	run := e.gh.PullRequestRun(p)
	if bd := e.bind(run); bd.Fail == "" {
		t.Fatalf("binding = %+v; want one that fails its jobs", bd)
	}
	j := e.queue(run, "lanescape-16vcpu-linux-arm64")
	e.pass()
	in := e.one()
	if in.MachineType != "c4a-highcpu-16" || in.Provisioning == "SPOT" || in.Metadata[machines.MetaSelector] != "lanescape-16vcpu-linux-arm64" {
		t.Fatalf("machine = %+v; want an ordinary one of the job's selector", in)
	}
	m := e.boot(in)
	m.taken(j)
	m.failed(run.ID, "one open pull request")
	// Until the run is shown finished, its jobs get no machines.
	next := e.queue(run, "lanescape-16vcpu-linux-arm64")
	e.pass()
	for _, in := range e.ours() {
		if in.Labels[machines.LabelJob] == strconv.FormatInt(next.ID, 10) {
			t.Fatalf("machine %s was made for a job of a run being cancelled", in.Name)
		}
	}
}

func TestTheHeadOfTwoPullRequestsFailsItsJobs(t *testing.T) {
	e := setup(t)
	e.repo.Commit("feature", map[string]string{"f": "1"})
	p := e.gh.PullRequest(e.repo, "main", e.repo, "feature", e.org)
	e.repo.Branch("copy", e.repo.Head("feature"))
	e.gh.PullRequest(e.repo, "main", e.repo, "copy", e.org)
	run := e.gh.PullRequestRun(p)
	j := e.queue(run, sel2)
	e.pass()
	m := e.boot(e.one())
	m.taken(j)
	// Its check binds the run read, and fails it, saying why.
	m.failed(run.ID, "one open pull request")
}

func TestAnApprovedContribution(t *testing.T) {
	e := setup(t)
	run, p := e.contribution(true)
	_ = p
	e.bind(run)
	e.gh.Queue(run, ghemu.JobSpec{Labels: []string{sel2}, Environment: machines.ContributionEnvironment})
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
	crun, _ := e.contribution(true)
	e.bind(crun)
	for range 2 {
		e.gh.Queue(crun, ghemu.JobSpec{Labels: []string{sel2}, Environment: machines.ContributionEnvironment})
	}
	other := e.gh.Repo(e.org, repoID+1, "other")
	orun := e.gh.PushRun(other, "main")
	e.bind(orun)
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
	e.bind(orun)
	oldest := e.queue(orun, sel2)
	// GitHub gives jobs' times to the second.
	time.Sleep(1100 * time.Millisecond)
	run := e.mainRun()
	younger := e.queue(run, sel2)
	youngest := e.queue(run, sel2)
	arm := e.queue(run, "lanescape-16vcpu-linux-arm64")
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
	big := "lanescape-80vcpu-linux-amd64"
	bigSpot := "lanescape-80vcpu-linux-amd64-spot"
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
	e.queue(e.mainRun(), "lanescape-16vcpu-linux-arm64")
	e.pass()
	m := e.one()
	if m.MachineType != "c4a-highcpu-16" {
		t.Fatalf("machine type %s", m.MachineType)
	}
	rs := e.gh.Runners()
	if !slices.Equal(rs[0].Labels, []string{"lanescape-16vcpu-linux-arm64"}) {
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

// A younger job of a smaller shape doesn't take the quota an older, larger
// job of the same machine family is waiting for.
func TestASmallerJobWaitsForAnOlderLargerOne(t *testing.T) {
	e := setup(t)
	e.gce.SetQuota("northamerica-northeast1", "N4_CPUS", 2)
	e.gce.SetQuota("northamerica-northeast1", "C4_CPUS", 0)
	big := e.queue(e.mainRun(), "lanescape-80vcpu-linux-amd64")
	time.Sleep(1100 * time.Millisecond)
	small := e.queue(e.mainRun(), sel2)
	arm := e.queue(e.mainRun(), "lanescape-16vcpu-linux-arm64")
	e.pass()
	ms := e.wantMachines(1)
	if ms[0].Labels[machines.LabelJob] != strconv.FormatInt(arm.ID, 10) {
		t.Fatalf("the machine is job %s's; want only the Arm job's", ms[0].Labels[machines.LabelJob])
	}
	if e.gh.Job(big.ID).Status != "queued" || e.gh.Job(small.ID).Status != "queued" {
		t.Fatal("a job held back by quota isn't queued")
	}
	e.gce.SetQuota("northamerica-northeast1", "N4_CPUS", 82)
	e.pass()
	e.wantMachines(3)
}

// Spot machines of every family share one quota: a younger Spot job of
// another family doesn't take what an older Spot job is waiting for.
func TestAYoungerSpotJobWaitsForAnOlderOne(t *testing.T) {
	e := setup(t)
	e.gce.SetQuota("northamerica-northeast1", "PREEMPTIBLE_CPUS", 16)
	e.gce.SetQuota("northamerica-northeast1", "N4_CPUS", 0)
	e.gce.SetQuota("northamerica-northeast1", "C4_CPUS", 0)
	big := e.queue(e.mainRun(), "lanescape-80vcpu-linux-amd64-spot")
	time.Sleep(1100 * time.Millisecond)
	arm := e.queue(e.mainRun(), "lanescape-16vcpu-linux-arm64-spot")
	e.pass()
	e.wantMachines(0)
	if e.gh.Job(big.ID).Status != "queued" || e.gh.Job(arm.ID).Status != "queued" {
		t.Fatal("a job held back by quota isn't queued")
	}
}

func TestARunnerWithOtherLabelsGetsNoMachine(t *testing.T) {
	e := setup(t)
	e.gh.AddDefaultLabels(true)
	e.queue(e.mainRun(), sel2)
	e.pass()
	e.wantMachines(0)
	if n := len(e.gh.Runners()); n != 0 {
		t.Fatalf("%d runner registrations left", n)
	}
	if !e.logged("not its selector alone") {
		t.Fatal("why wasn't logged")
	}
}

// A queued job that names a label no runner of the service's has gets no
// machine: its run is cancelled, and why is logged, since nothing can print
// it in the job's log.
func TestARunIsCancelledForALabelNoRunnerHas(t *testing.T) {
	for name, tc := range map[string]struct {
		labels []string
		reason string
	}{
		"a selector with other labels":     {[]string{sel2, "self-hosted"}, "with other labels"},
		"a selector naming a run":          {[]string{sel2 + "-run-1"}, "isn't a selector"},
		"a selector naming a disk":         {[]string{sel2 + "-disk-build"}, "isn't a selector"},
		"a label that claims to be one":    {[]string{"Lanescape-runners"}, "isn't a selector"},
		"a shape the service doesn't have": {[]string{"lanescape-4vcpu-linux-amd64-spot"}, "a shape the service doesn't offer"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			run := e.mainRun()
			e.queue(run, tc.labels...)
			e.pass()
			e.wantMachines(0)
			rec, ok := e.record("refusals/5001/" + strconv.FormatInt(run.ID, 10) + "-1.json")
			if !ok || !strings.Contains(fmt.Sprint(rec["reason"]), tc.reason) || rec["cancelled_at"] == nil {
				t.Fatalf("refusal = %v; want the run cancelled, saying %q", rec, tc.reason)
			}
			if !e.logged("refusing a run without a machine") || !e.logged(tc.reason) {
				t.Fatal("why wasn't logged")
			}
			if st, c := e.gh.RunState(run); st != "completed" || c != "cancelled" {
				t.Fatalf("run: %s %s", st, c)
			}
		})
	}
}

// Refusals block runs, not selectors: a refused run's jobs get no machine,
// and another run's job of the same selector does.
func TestARefusedRunDoesntBlockAnotherRunsJob(t *testing.T) {
	e := setup(t)
	refused := e.mainRun()
	// The run's job of the selector is older than the one that gets it
	// refused, in the same pass.
	e.queue(refused, sel2)
	e.queue(refused, sel2, "gpu")
	other := e.mainRun()
	j := e.queue(other, sel2)
	e.pass()
	ms := e.wantMachines(1)
	if ms[0].Labels[machines.LabelJob] != strconv.FormatInt(j.ID, 10) {
		t.Fatalf("the machine is job %s's; want the other run's", ms[0].Labels[machines.LabelJob])
	}
}

// fakeBases is the base images' part of a pass, as the machines' pass sees
// it.
type fakeBases struct {
	key string
	e   *env
	// machinesAtPass are how many machines there were when it ran.
	machinesAtPass []int
}

func (f *fakeBases) Pass(*pass.Pass) {
	f.machinesAtPass = append(f.machinesAtPass, len(f.e.gce.Instances()))
}

func (f *fakeBases) Boot(ims []compute.Image, arch string) (string, bool) {
	return bases.Boot(ims, arch, f.key)
}

// TestMachinesBootTheCurrentKeysBaseImage: machines boot the current key's
// base image once it's ready, even an older one, and the previous until
// then; the base images move on before queued jobs are given machines, so
// they never starve a builder (specs/0003-disks.md, Which image machines
// boot; specs/0001-architecture.md, Passes).
func TestMachinesBootTheCurrentKeysBaseImage(t *testing.T) {
	e := setup(t)
	fb := &fakeBases{key: "made-later", e: e}
	e.svc.Bases = fb
	run := e.mainRun()
	e.queue(run, sel2)
	e.pass()
	if !slices.Equal(fb.machinesAtPass, []int{0}) {
		t.Fatalf("the base images' part ran with %v machines made; it runs before any is", fb.machinesAtPass)
	}
	source := func(name string) string {
		for _, d := range e.gce.Disks() {
			if d.Name == name {
				return d.SourceImage[strings.LastIndex(d.SourceImage, "/")+1:]
			}
		}
		return ""
	}
	first := e.one().Name
	if got := source(first); got != "lsr-base-amd64-new" {
		t.Fatalf("with the current key's image not ready, a machine boots %s, not the newest", got)
	}
	// A reverted deploy's key, whose image is older, is booted once it's
	// there.
	fb.key = "old"
	e.queue(run, sel2)
	e.pass()
	e.wantMachines(2)
	for _, m := range e.gce.Instances() {
		if got := source(m.Name); m.Name != first && got != "lsr-base-amd64-old" {
			t.Fatalf("with the current key's image ready, a machine boots %s", got)
		}
	}
}

// TestTheFirstDeploysJobsWaitForTheBaseImage: with no base image at all, as
// at the first deploy, a queued job waits while the builder makes one, and
// gets its machine in the pass that makes the image.
func TestTheFirstDeploysJobsWaitForTheBaseImage(t *testing.T) {
	e := setup(t)
	e.gce.DeleteImageNow("lsr-base-amd64-old")
	e.gce.DeleteImageNow("lsr-base-amd64-new")
	e.gce.AddPublicImage(config.DebianProject, e.cfg.Base.Debian["amd64"], "X86_64")
	keys := bases.Keys(e.cfg)
	// arm64's image is there already.
	e.gce.AddImage(bases.ImageName("arm64", keys["arm64"]), "", "ARM64")
	gcec, err := compute.New(context.Background(), e.gce.Project, e.gce.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Bases = &bases.Service{Config: e.cfg, Compute: gcec, Objects: e.svc.Objects, Clock: e.clock, Logger: e.svc.Logger,
		Network: machines.Network, Subnetwork: machines.Subnetwork, DiskType: machines.DiskType, Keys: keys}
	run := e.mainRun()
	e.queue(run, sel2)
	e.pass()
	e.wantMachines(0)
	builder := bases.BuilderName("amd64", keys["amd64"])
	st, ok := e.gce.Instance(zoneA, builder)
	if !ok {
		t.Fatal("no builder")
	}
	if st.ServiceAccount != "" || len(e.gh.Runners()) != 0 {
		t.Fatalf("builder = %+v; runners %+v", st, e.gh.Runners())
	}
	e.gce.WriteSerial(zoneA, builder, baseimage.Built+"amd64-"+keys["amd64"]+"\n")
	e.pass()
	m := e.one()
	for _, d := range e.gce.Disks() {
		if d.Name == m.Name && !strings.HasSuffix(d.SourceImage, "/"+bases.ImageName("amd64", keys["amd64"])) {
			t.Fatalf("the job's machine boots %s", d.SourceImage)
		}
	}
}
