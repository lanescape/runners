package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	ghemu "github.com/lanescape/runners/emulators/github"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/selector"
)

// orgAccount is the organization's account in the GitHub emulator.
func (e *Emulator) orgAccount() *ghemu.Account { return e.gh.Account(e.org) }

// GitHubRepo is a repository's GitHub object.
func (e *Emulator) GitHubRepo(r Repository) *ghemu.Repo {
	var out *ghemu.Repo
	e.gh.Read(func(s *ghemu.State) { out = s.Repo(r.ID) })
	if out == nil {
		panic(fmt.Sprintf("service emulator: no repository %d", r.ID))
	}
	return out
}

// GitHubPull is a pull request's GitHub object.
func (e *Emulator) GitHubPull(pr PullRequest) *ghemu.Pull {
	e.mu.Lock()
	p := e.pulls[fmt.Sprintf("%d/%d", pr.Repository.ID, pr.Number)]
	e.mu.Unlock()
	if p != nil {
		return p
	}
	e.gh.Read(func(s *ghemu.State) {
		for _, c := range s.Pulls(s.Repo(pr.Repository.ID)) {
			if c.Number == pr.Number {
				p = c
			}
		}
	})
	if p == nil {
		panic(fmt.Sprintf("service emulator: no pull request %d", pr.Number))
	}
	return p
}

// GitHubRun is a run's GitHub object.
func (e *Emulator) GitHubRun(r Run) *ghemu.Run {
	var out *ghemu.Run
	e.gh.Read(func(s *ghemu.State) { out = s.Run(r.ID) })
	if out == nil {
		panic(fmt.Sprintf("service emulator: no run %d", r.ID))
	}
	return out
}

// GitHubJob is a job's GitHub object.
func (e *Emulator) GitHubJob(j Job) *ghemu.WorkflowJob {
	var out *ghemu.WorkflowJob
	e.gh.Read(func(s *ghemu.State) { out = s.Job(j.ID) })
	if out == nil {
		panic(fmt.Sprintf("service emulator: no job %d", j.ID))
	}
	return out
}

// RepositoryOf is the handle of a repository the test made in the GitHub
// emulator itself.
func (e *Emulator) RepositoryOf(rp *ghemu.Repo) Repository {
	var out Repository
	e.gh.Read(func(*ghemu.State) { out = Repository{ID: rp.ID, Name: rp.FullName()} })
	return out
}

// PullRequestOf is the handle of a pull request the test made in the
// GitHub emulator itself.
func (e *Emulator) PullRequestOf(p *ghemu.Pull) PullRequest {
	var out PullRequest
	e.gh.Read(func(*ghemu.State) {
		out = PullRequest{Repository: Repository{ID: p.Base.ID, Name: p.Base.FullName()}, Number: p.Number, HeadSHA: p.HeadSHA}
		if p.Head != nil {
			out.HeadRepositoryID = p.Head.ID
		}
	})
	e.mu.Lock()
	e.pulls[fmt.Sprintf("%d/%d", out.Repository.ID, out.Number)] = p
	e.mu.Unlock()
	return out
}

// RunOf is the handle of a run the test made in the GitHub emulator itself.
func (e *Emulator) RunOf(gr *ghemu.Run) Run {
	var r Run
	e.gh.Read(func(*ghemu.State) {
		r = Run{Repository: Repository{ID: gr.Repo.ID, Name: gr.Repo.FullName()}, ID: gr.ID, Attempt: gr.Attempt, Event: gr.Event}
		if ep := gr.EventPull; ep != nil {
			r.PullRequest = &PullRequest{Repository: r.Repository, Number: ep.Number, HeadRepositoryID: ep.HeadRepositoryID, HeadSHA: ep.HeadSHA}
		}
	})
	return r
}

// JobOf is the handle of a job the test queued in the GitHub emulator
// itself, with the machine the emulator played it to its check on, if
// StartWorkflowJob has.
func (e *Emulator) JobOf(wj *ghemu.WorkflowJob) Job {
	var run *ghemu.Run
	var labels []string
	var attempt int
	e.gh.Read(func(*ghemu.State) { run, labels, attempt = wj.Run, wj.Labels, wj.Attempt })
	j := Job{Run: e.RunOf(run), ID: wj.ID}
	j.Run.Attempt = attempt
	if sel, err := selector.OfJob(labels); err == nil {
		j.Selector = sel.String()
	}
	e.mu.Lock()
	if js := e.jobs[wj.ID]; js != nil {
		j.Machine = js.machine
	}
	e.mu.Unlock()
	return j
}

// Repository implements World.
func (e *Emulator) Repository(name string) Repository {
	return e.RepositoryOf(e.gh.Repo(e.orgAccount(), e.id(), name))
}

// Commit implements World.
func (e *Emulator) Commit(r Repository, branch string, files map[string]string) {
	e.GitHubRepo(r).Commit(branch, files)
}

// Branch implements World.
func (e *Emulator) Branch(r Repository, name string) {
	e.GitHubRepo(r).Commit(name, map[string]string{"branch": name})
}

// DeleteBranch implements World.
func (e *Emulator) DeleteBranch(r Repository, name string) { e.GitHubRepo(r).DeleteBranch(name) }

// PullRequest implements World.
func (e *Emulator) PullRequest(r Repository, base, head string) PullRequest {
	rp := e.GitHubRepo(r)
	if rp.Head(head) == "" {
		rp.Commit(head, map[string]string{"head": head})
	}
	return e.PullRequestOf(e.gh.PullRequest(rp, base, rp, head, e.maintainer))
}

// ForkPullRequest implements World.
func (e *Emulator) ForkPullRequest(r Repository, base string) PullRequest {
	rp := e.GitHubRepo(r)
	id := e.id()
	contributor := e.gh.User(id, "contributor"+strconv.FormatInt(id, 10))
	fork := e.gh.Fork(rp, contributor, e.id())
	fork.Commit("feature", map[string]string{"src.go": "package app"})
	return e.PullRequestOf(e.gh.PullRequest(rp, base, fork, "feature", contributor))
}

// PushPullRequest implements World.
func (e *Emulator) PushPullRequest(pr PullRequest, files map[string]string) PullRequest {
	p := e.GitHubPull(pr)
	if len(files) == 0 {
		files = map[string]string{"pushed": strconv.FormatInt(e.id(), 10)}
	}
	var head *ghemu.Repo
	var ref string
	e.gh.Read(func(*ghemu.State) { head, ref = p.Head, p.HeadRef })
	head.Commit(ref, files)
	e.gh.Sync(p)
	pr.HeadSHA = e.gh.PullHead(p)
	return pr
}

// Push implements World.
func (e *Emulator) Push(r Repository, branch string) Run {
	return e.RunOf(e.gh.PushRun(e.GitHubRepo(r), branch))
}

// Tag implements World.
func (e *Emulator) Tag(r Repository, tag string) Run {
	rp := e.GitHubRepo(r)
	rp.Tag(tag, rp.Head("main"))
	return e.RunOf(e.gh.TagRun(rp, tag))
}

// Event implements World.
func (e *Emulator) Event(r Repository, event, branch string) Run {
	return e.RunOf(e.gh.EventRun(e.GitHubRepo(r), event, branch))
}

// PullRequestRun implements World.
func (e *Emulator) PullRequestRun(pr PullRequest) Run {
	return e.RunOf(e.gh.PullRequestRun(e.GitHubPull(pr)))
}

// PullRequestTargetRun implements World.
func (e *Emulator) PullRequestTargetRun(pr PullRequest) Run {
	return e.RunOf(e.gh.PullRequestTargetRun(e.GitHubPull(pr)))
}

// Approve implements World.
func (e *Emulator) Approve(r Run) {
	e.gh.Approve(e.GitHubRun(r), binding.ContributionEnvironment, e.maintainer)
}

// Rerun implements World.
func (e *Emulator) Rerun(r Run) Run {
	gr := e.GitHubRun(r)
	e.gh.RerunAs(gr, e.maintainer)
	return e.RunOf(gr)
}

// facts are what GitHub tells a run's job's runner.
func facts(gr *ghemu.Run) binding.Facts {
	f := binding.Facts{Event: gr.Event, Ref: gr.Ref, RefType: gr.RefType, SHA: gr.SHA}
	if p := gr.EventPull; p != nil {
		f.PullRequest, f.HeadRepositoryID, f.HeadSHA = p.Number, p.HeadRepositoryID, p.HeadSHA
	}
	return f
}

// StartJob implements World.
func (e *Emulator) StartJob(r Run, label string) (Job, error) {
	return e.StartWorkflowJob(e.gh.Queue(e.GitHubRun(r), ghemu.JobSpec{Labels: []string{label}}))
}

// StartJobOn implements World. Every machine of a selector is the same, so
// the job is checked as any other: by GitHub's records of its own run.
func (e *Emulator) StartJobOn(r Run, label string, madeFor Run) (Job, error) {
	e.gh.CompleteJob(e.gh.Queue(e.GitHubRun(madeFor), ghemu.JobSpec{Labels: []string{label}}), "cancelled")
	return e.StartJob(r, label)
}

// StartWorkflowJob plays a job the test queued in the GitHub emulator
// itself, with the name, labels and environment it gave it, to its check on
// one of the service's machines, as StartJob does. A job GitHub holds for
// its environment's approval is ErrWaiting.
func (e *Emulator) StartWorkflowJob(wj *ghemu.WorkflowJob) (Job, error) {
	var gr *ghemu.Run
	var labels []string
	var status string
	var f binding.Facts
	var fork bool
	e.gh.Read(func(*ghemu.State) {
		gr, labels, status, f = wj.Run, wj.Labels, wj.Status, facts(wj.Run)
		fork = gr.Event != "pull_request_target" && gr.HeadRepo != nil && gr.HeadRepo.ID != gr.Repo.ID
	})
	switch status {
	case "waiting":
		return Job{}, ErrWaiting
	case "queued":
	default:
		return Job{}, fmt.Errorf("service emulator: job %d is %s, so no runner takes it", wj.ID, status)
	}
	e.mu.Lock()
	e.jobs[wj.ID] = &jobState{wj: wj}
	down := e.unavailable
	e.mu.Unlock()
	j := e.JobOf(wj)
	// What the service decides without a machine: a job whose labels
	// aren't one selector it offers, or a fork's pull request's, gets none,
	// and its run is cancelled.
	sel, err := selector.OfJob(labels)
	if errors.Is(err, selector.ErrNoSelector) {
		return Job{}, &Refused{Reason: "the job names none of the service's selectors"}
	}
	if err != nil {
		return e.cancel(gr, err.Error())
	}
	if e.shapes.Shapes[sel.Shape] == nil {
		return e.cancel(gr, fmt.Sprintf("%s isn't a selector of a shape the service offers", sel))
	}
	if fork {
		return e.cancel(gr, "a fork's pull request's run gets none of the service's machines")
	}
	if down {
		return Job{}, ErrUnavailable
	}
	ctx := context.Background()
	repo, err := e.ghc.RepositoryByID(ctx, j.Run.Repository.ID)
	if err != nil {
		return Job{}, ErrUnavailable
	}
	wr, err := e.ghc.WorkflowRun(ctx, repo, j.Run.ID)
	if err != nil {
		return Job{}, ErrUnavailable
	}
	bd, bound, err := e.binder.Lookup(ctx, repo.ID, j.Run.ID)
	if err != nil {
		return Job{}, ErrUnavailable
	}
	if !bound {
		bd, err = e.binder.Bind(ctx, repo, wr, f)
		switch {
		case errors.Is(err, binding.ErrUnapproved):
		case err != nil:
			return Job{}, ErrUnavailable
		default:
			bound = true
		}
	}
	m := &machine{name: fmt.Sprintf("lsr-%d-%08d", repo.ID, e.id()%100_000_000), id: uint64(e.id()), zone: "northamerica-northeast1-a",
		repo: repo.ID, run: j.Run.ID, job: wj.ID, selector: sel, attempt: j.Run.Attempt}
	smallest := e.shapes.Smallest()
	switch {
	case !bound && wr.Event == "pull_request_target" && sel.Plain(smallest):
		// An unbound contribution's job may have the smallest shape,
		// standard: nothing restored, nothing saved.
		m.unbound = true
	case !bound && wr.Event == "pull_request_target":
		return e.cancel(gr, "the contribution's run isn't approved, so its jobs may have only "+smallest+
			", standard, until its contribution job is approved")
	case !bound:
		return Job{}, &Refused{Reason: "the job's run has no binding"}
	case bd.Fail != "":
		return e.cancel(gr, bd.Fail)
	}
	e.gh.RunJob(wj)
	e.mu.Lock()
	e.machines[m.name] = m
	e.jobs[wj.ID].machine = m.name
	e.mu.Unlock()
	j.Selector, j.Machine = sel.String(), m.name
	return j, nil
}

// cancel refuses a job, saying why, and cancels its run, as the service
// does for a job it gives no machine or fails.
func (e *Emulator) cancel(gr *ghemu.Run, reason string) (Job, error) {
	var open []*ghemu.WorkflowJob
	e.gh.Read(func(*ghemu.State) {
		for _, j := range gr.Jobs {
			if j.Attempt == gr.Attempt && j.Status != "completed" {
				open = append(open, j)
			}
		}
	})
	for _, j := range open {
		e.gh.CompleteJob(j, "cancelled")
	}
	return Job{}, &Refused{Reason: reason}
}

// FinishJob implements World: GitHub records the job's conclusion, and the
// emulator, as the service does on its machine's report, saves its work
// disk if it was attached, the job asked to save it, its conclusion is
// success, its run's scope saves and its repository is still the
// organization's (specs/0003-disks.md).
func (e *Emulator) FinishJob(j Job, conclusion string) {
	e.mu.Lock()
	js := e.jobs[j.ID]
	e.mu.Unlock()
	if js == nil {
		return
	}
	e.gh.CompleteJob(js.wj, conclusion)
	ours := false
	e.gh.Read(func(s *ghemu.State) {
		if rp := s.Repo(j.Run.Repository.ID); rp != nil {
			ours = rp.Owner.ID == e.org
		}
	})
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.machines[js.machine]
	if m == nil || m.finished {
		return
	}
	m.finished = true
	if conclusion == "success" && ours {
		e.saveDisk(m)
	}
}
