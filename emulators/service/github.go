package service

import (
	"fmt"

	ghemu "github.com/lanescape/runners/emulators/github"
)

// runnersFile is the default branch's file the service reads.
const runnersFile = ".github/runners.json"

// WithGitHub makes gh, a GitHub emulator the test already runs, the
// emulator's view of GitHub, in place of a model of its own: one GitHub
// state serves the test's jobs and the service emulator, as GitHub serves
// both a job and the service. The emulator reads gh's repositories, their
// branches, commits and default branch's .github/runners.json, pull
// requests and their commits, runs and jobs, as they are when each request
// arrives, and verifies tokens against gh's Actions issuer. A test may
// change anything in gh directly, such as pushing to a pull request's
// branch and syncing it, finishing a job or closing a pull request, and the
// emulator sees it at its next request or Binding. Faults added to gh's API
// don't reach the emulator, which doesn't call it; Unavailable makes GitHub
// unavailable to it.
//
// World's methods act on gh: Repository makes a repository of the
// organization there, Push makes gh's push run, StartJob queues and starts
// a job of gh's run, Token mints gh's token for it, and so on. GitHubRepo,
// GitHubPull, GitHubRun and GitHubJob give a handle's GitHub object, and
// RunOf and JobOf the handles of a run or job the test made in gh itself.
func WithGitHub(gh *ghemu.Emulator) Option { return func(e *Emulator) { e.gh = gh } }

// GitHub is the GitHub emulator the emulator takes its view of GitHub
// from, or nil if it models GitHub itself.
func (e *Emulator) GitHub() *ghemu.Emulator { return e.gh }

func (e *Emulator) needGitHub(what string) {
	if e.gh == nil {
		panic("service emulator: " + what + " needs WithGitHub")
	}
}

// observe reads GitHub's state from the GitHub emulator. The caller holds
// e.mu.
func (e *Emulator) observe() {
	if e.gh == nil {
		return
	}
	repos, runs, jobs := map[int64]*repo{}, map[int64]*run{}, map[int64]*job{}
	e.gh.Read(func(s *ghemu.State) {
		for _, gr := range s.Repos() {
			if gr.Owner == nil || gr.Owner.ID != e.org {
				continue
			}
			rp := &repo{id: gr.ID, name: gr.FullName(), def: gr.DefaultBranch, branches: s.Branches(gr),
				history: map[string]map[string]bool{}, pulls: map[int]*pull{}}
			for b, sha := range rp.branches {
				rp.history[b] = s.Reachable(sha)
			}
			rp.runners, _ = s.File(gr, gr.DefaultBranch, runnersFile)
			for _, gp := range s.Pulls(gr) {
				p := &pull{number: gp.Number, base: gp.BaseRef, headSHA: gp.HeadSHA, open: gp.State == "open",
					commits: map[string]bool{gp.HeadSHA: true}}
				if gp.Head != nil {
					p.headRepo = gp.Head.ID
				}
				for _, c := range s.PullCommits(gp) {
					p.commits[c] = true
				}
				rp.pulls[p.number] = p
			}
			repos[rp.id] = rp
		}
		for _, gr := range s.Runs() {
			rp := repos[gr.Repo.ID]
			if rp == nil {
				continue
			}
			rn := &run{id: gr.ID, repo: rp, event: gr.Event, ref: gr.Ref, refType: gr.RefType, sha: gr.SHA, headSHA: gr.HeadSHA,
				attempt: gr.Attempt, status: gr.Status}
			if gr.HeadRepo != nil {
				rn.headRepo = gr.HeadRepo.ID
			}
			if gr.EventPull != nil && (gr.Event == "pull_request" || gr.Event == "pull_request_target") {
				rn.pull = rp.pulls[gr.EventPull.Number]
			}
			runs[rn.id] = rn
			for _, gj := range gr.Jobs {
				jobs[gj.ID] = &job{id: gj.ID, run: rn, attempt: gj.Attempt, status: gj.Status}
			}
		}
	})
	e.repos, e.runs, e.jobs = repos, runs, jobs
}

// ghOrg is the organization's account in the GitHub emulator, made if the
// test hasn't made it.
func (e *Emulator) ghOrg() *ghemu.Account {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a := e.gh.Account(e.org); a != nil {
		return a
	}
	return e.gh.Org(e.org, e.login)
}

// ghMaintainer is the maintainer who opens pull requests and reruns runs.
func (e *Emulator) ghMaintainer() *ghemu.Account {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.maintainer == nil {
		e.maintainer = e.gh.User(e.gh.NewID(), "maintainer")
	}
	return e.maintainer
}

// GitHubRepo is a repository's GitHub object.
func (e *Emulator) GitHubRepo(r Repository) *ghemu.Repo {
	e.needGitHub("GitHubRepo")
	var out *ghemu.Repo
	e.gh.Read(func(s *ghemu.State) { out = s.Repo(r.ID) })
	if out == nil {
		panic(fmt.Sprintf("service emulator: GitHub has no repository %d", r.ID))
	}
	return out
}

// GitHubPull is a pull request's GitHub object.
func (e *Emulator) GitHubPull(pr PullRequest) *ghemu.Pull {
	e.needGitHub("GitHubPull")
	var out *ghemu.Pull
	e.gh.Read(func(s *ghemu.State) {
		if rp := s.Repo(pr.Repository.ID); rp != nil {
			for _, p := range s.Pulls(rp) {
				if p.Number == pr.Number {
					out = p
				}
			}
		}
	})
	if out == nil {
		panic(fmt.Sprintf("service emulator: GitHub has no pull request %d", pr.Number))
	}
	return out
}

// GitHubRun is a run's GitHub object.
func (e *Emulator) GitHubRun(r Run) *ghemu.Run {
	e.needGitHub("GitHubRun")
	var out *ghemu.Run
	e.gh.Read(func(s *ghemu.State) { out = s.Run(r.ID) })
	if out == nil {
		panic(fmt.Sprintf("service emulator: GitHub has no run %d", r.ID))
	}
	return out
}

// GitHubJob is a job's GitHub object.
func (e *Emulator) GitHubJob(j Job) *ghemu.WorkflowJob {
	e.needGitHub("GitHubJob")
	var out *ghemu.WorkflowJob
	e.gh.Read(func(s *ghemu.State) { out = s.Job(j.ID) })
	if out == nil {
		panic(fmt.Sprintf("service emulator: GitHub has no job %d", j.ID))
	}
	return out
}

// pullRequestOf is a pull request's handle; the caller is inside Read.
func pullRequestOf(p *ghemu.Pull) PullRequest {
	pr := PullRequest{Repository: Repository{ID: p.Base.ID, Name: p.Base.FullName()}, Number: p.Number, HeadSHA: p.HeadSHA}
	if p.Head != nil {
		pr.HeadRepositoryID = p.Head.ID
	}
	return pr
}

// RunOf is the handle of a run the test made in the GitHub emulator, at
// its current attempt.
func (e *Emulator) RunOf(gr *ghemu.Run) Run {
	e.needGitHub("RunOf")
	var out Run
	e.gh.Read(func(s *ghemu.State) {
		out = Run{Repository: Repository{ID: gr.Repo.ID, Name: gr.Repo.FullName()}, ID: gr.ID, Attempt: gr.Attempt, Event: gr.Event}
		if ep := gr.EventPull; ep != nil {
			out.PullRequest = &PullRequest{Repository: out.Repository, Number: ep.Number, HeadRepositoryID: ep.HeadRepositoryID, HeadSHA: ep.HeadSHA}
		}
	})
	return out
}

// JobOf is the handle of a job the test made in the GitHub emulator;
// workflowRef is its job_workflow_ref, or empty for its run's own
// workflow's.
func (e *Emulator) JobOf(wj *ghemu.WorkflowJob, workflowRef string) Job {
	e.needGitHub("JobOf")
	var id int64
	var attempt int
	var gr *ghemu.Run
	e.gh.Read(func(s *ghemu.State) { id, attempt, gr = wj.ID, wj.Attempt, wj.Run })
	r := e.RunOf(gr)
	r.Attempt = attempt
	return Job{Run: r, ID: id, WorkflowRef: workflowRef}
}

func (e *Emulator) ghRepository(name string) Repository {
	rp := e.gh.Repo(e.ghOrg(), e.gh.NewID(), name)
	return e.RepositoryOf(rp)
}

// RepositoryOf is the handle of a repository the test made in the GitHub
// emulator.
func (e *Emulator) RepositoryOf(rp *ghemu.Repo) Repository {
	e.needGitHub("RepositoryOf")
	var out Repository
	e.gh.Read(func(*ghemu.State) { out = Repository{ID: rp.ID, Name: rp.FullName()} })
	return out
}

// PullRequestOf is the handle of a pull request the test made in the
// GitHub emulator, with its head now.
func (e *Emulator) PullRequestOf(p *ghemu.Pull) PullRequest {
	e.needGitHub("PullRequestOf")
	var out PullRequest
	e.gh.Read(func(*ghemu.State) { out = pullRequestOf(p) })
	return out
}

func (e *Emulator) ghPullRequest(r Repository, base, head string) PullRequest {
	rp := e.GitHubRepo(r)
	rp.Commit(head, map[string]string{"head": head})
	return e.PullRequestOf(e.gh.PullRequest(rp, base, rp, head, e.ghMaintainer()))
}

func (e *Emulator) ghForkPullRequest(r Repository, base string) PullRequest {
	rp := e.GitHubRepo(r)
	id := e.gh.NewID()
	contributor := e.gh.User(id, fmt.Sprintf("contributor%d", id))
	fork := e.gh.Fork(rp, contributor, e.gh.NewID())
	fork.Commit("feature", map[string]string{"feature": fmt.Sprint(id)})
	return e.PullRequestOf(e.gh.PullRequest(rp, base, fork, "feature", contributor))
}

func (e *Emulator) ghPushPullRequest(pr PullRequest) PullRequest {
	p := e.GitHubPull(pr)
	var head *ghemu.Repo
	var ref string
	var open bool
	e.gh.Read(func(*ghemu.State) { head, ref, open = p.Head, p.HeadRef, p.State == "open" })
	if !open || head == nil {
		panic(fmt.Sprintf("service emulator: pull request %d isn't open, or has no head repository", pr.Number))
	}
	head.Commit(ref, map[string]string{"pushed": fmt.Sprint(e.gh.NewID())})
	e.gh.Sync(p)
	return e.PullRequestOf(p)
}

func (e *Emulator) ghTag(r Repository, tag string) Run {
	rp := e.GitHubRepo(r)
	rp.Tag(tag, rp.Head(rp.DefaultBranch))
	return e.RunOf(e.gh.TagRun(rp, tag))
}

func (e *Emulator) ghRerun(r Run) Run {
	gr := e.GitHubRun(r)
	e.gh.RerunAs(gr, e.ghMaintainer())
	return e.RunOf(gr)
}

func (e *Emulator) ghStartJob(r Run, workflowRef string) Job {
	wj := e.gh.Queue(e.GitHubRun(r), ghemu.JobSpec{Labels: []string{"ubuntu-24.04"}})
	e.gh.RunJob(wj)
	return e.JobOf(wj, workflowRef)
}

func (e *Emulator) ghClaims(j Job, overrides map[string]any) map[string]any {
	wj := e.GitHubJob(j)
	var gr *ghemu.Run
	var attempt int
	e.gh.Read(func(*ghemu.State) { gr, attempt = wj.Run, wj.Attempt })
	c := e.gh.ActionsClaims(gr, ghemu.Job{Audience: e.url, WorkflowRef: j.WorkflowRef, CheckRunID: j.ID})
	c["run_attempt"] = fmt.Sprint(attempt)
	for k, v := range overrides {
		c[k] = v
	}
	return c
}
