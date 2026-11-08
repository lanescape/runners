package github

import (
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The organization setting that decides which outside contributors' fork
// pull request workflows wait for a maintainer's approval, with GitHub's
// values; GitHub's default is the first.
const (
	ApproveFirstTimeContributors       = "first_time_contributors"
	ApproveFirstTimeContributorsNew    = "first_time_contributors_new_to_github"
	ApproveAllExternalContributors     = "all_external_contributors"
	defaultForkApproval                = ApproveFirstTimeContributors
	runnerOnlineWindow                 = 30 * time.Second
	lostRunnerAfter                    = 10 * time.Minute
	defaultRunnerGroupName             = "Default"
	maxRunnerLabels                    = 100
	runnerGroupRepositoriesURLTemplate = "%s/orgs/%s/actions/runner-groups/%d/repositories"
)

// WorkflowJob is one job of a workflow run.
type WorkflowJob struct {
	ID      int64
	Run     *Run
	Attempt int
	Name    string
	// Labels are the job's runs-on labels, as the workflow wrote them.
	Labels []string
	// Environment is the environment the job names. A job whose
	// environment is protected waits until a reviewer approves its run.
	Environment string
	Status      string // waiting, queued, in_progress or completed
	Conclusion  string
	Created     time.Time
	Started     time.Time
	Completed   time.Time
	Runner      *Runner
	// Env is what the job's own environment sets, as a workflow's env:
	// can, when the runner runs its job-started hook and its steps. It
	// can't change GitHub's own GITHUB_ and RUNNER_ variables.
	Env map[string]string
	// Steps is a bash script the runner stand-in runs as the job's steps,
	// with the job's environment; its exit status decides the job's
	// conclusion. Empty means steps that succeed at once.
	Steps string

	delivered  bool
	cancelling bool
	hidden     int
}

// JobSpec says what a queued job is.
type JobSpec struct {
	Name        string
	Labels      []string
	Environment string
	Env         map[string]string
	Steps       string
}

// Approval is a reviewer's decision on a run's pending deployments, as its
// review history records it.
type Approval struct {
	State        string // approved or rejected
	Environments []string
	User         *Account
	Comment      string
	At           time.Time
}

// RunnerGroup is an organization's runner group.
type RunnerGroup struct {
	ID                       int64
	Org                      *Account
	Name                     string
	Visibility               string // all, selected or private
	Repositories             []int64
	AllowsPublicRepositories bool
	Default                  bool
	RestrictedToWorkflows    bool
}

// Runner is a self-hosted runner registered in a group.
type Runner struct {
	ID        int64
	Name      string
	Group     *RunnerGroup
	Labels    []string
	Ephemeral bool
	WorkDir   string

	token    string
	Job      *WorkflowJob
	used     bool
	removed  bool
	inflight int
	seen     time.Time
	waiting  time.Time
}

func (e *Emulator) initActions() {
	e.jobs = map[int64]*WorkflowJob{}
	e.groups = map[int64]*RunnerGroup{}
	e.runners = map[int64]*Runner{}
	e.forkApproval = map[int64]string{}
	e.protected = map[string]bool{}
	e.changed = make(chan struct{})
}

// notify wakes everything waiting for the emulator's state to change. The
// caller holds e.mu.
func (e *Emulator) notify() {
	close(e.changed)
	e.changed = make(chan struct{})
}

// defaultGroup is an organization's Default group, made with it: visible to
// all its repositories, but not to public ones.
func (e *Emulator) defaultGroup(org *Account) *RunnerGroup {
	for _, g := range e.groups {
		if g.Org == org && g.Default {
			return g
		}
	}
	g := &RunnerGroup{ID: e.id(), Org: org, Name: defaultRunnerGroupName, Visibility: "all", Default: true}
	e.groups[g.ID] = g
	return g
}

// SetForkApproval sets the organization's setting for which outside
// contributors' fork pull request workflows need approval.
func (e *Emulator) SetForkApproval(org *Account, policy string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.forkApproval[org.ID] = policy
}

// ProtectEnvironment gives a repository's environment required reviewers:
// jobs that name it wait until one approves their run.
func (e *Emulator) ProtectEnvironment(r *Repo, env string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.protected[envKey(r, env)] = true
}

func envKey(r *Repo, env string) string { return fmt.Sprintf("%d/%s", r.ID, strings.ToLower(env)) }

// Queue adds a job to a run's current attempt: queued for a runner, or
// waiting if its environment is protected and its run isn't yet approved
// for it.
func (e *Emulator) Queue(r *Run, spec JobSpec) *WorkflowJob {
	e.mu.Lock()
	defer e.mu.Unlock()
	j := &WorkflowJob{ID: e.id(), Run: r, Attempt: r.Attempt, Name: spec.Name, Labels: slices.Clone(spec.Labels),
		Environment: spec.Environment, Env: spec.Env, Steps: spec.Steps, Status: "queued", Created: e.clock.Now()}
	if j.Name == "" {
		j.Name = fmt.Sprintf("job-%d", j.ID)
	}
	if j.Environment != "" && e.protected[envKey(r.Repo, j.Environment)] && !r.approved(j.Environment) {
		j.Status = "waiting"
	}
	e.jobs[j.ID] = j
	r.Jobs = append(r.Jobs, j)
	if r.Status == "completed" {
		r.Status, r.Conclusion = "in_progress", ""
	}
	e.notify()
	return j
}

func (r *Run) approved(env string) bool {
	for _, a := range r.Approvals {
		if a.State == "approved" && slices.ContainsFunc(a.Environments, func(n string) bool { return strings.EqualFold(n, env) }) {
			return true
		}
	}
	return false
}

// Approve records a reviewer's approval of a run's deployments to env, and
// queues its jobs that were waiting for it.
func (e *Emulator) Approve(r *Run, env string, reviewer *Account) {
	e.review(r, env, reviewer, "approved")
}

// Reject records a reviewer's rejection; the jobs that waited for env fail.
func (e *Emulator) Reject(r *Run, env string, reviewer *Account) {
	e.review(r, env, reviewer, "rejected")
}

func (e *Emulator) review(r *Run, env string, reviewer *Account, state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r.Approvals = append(r.Approvals, Approval{State: state, Environments: []string{env}, User: reviewer, At: e.clock.Now()})
	for _, j := range r.Jobs {
		if j.Status != "waiting" || !strings.EqualFold(j.Environment, env) {
			continue
		}
		if state == "approved" {
			j.Status = "queued"
		} else {
			e.complete(j, "failure")
		}
	}
	e.notify()
}

// Rerun starts a finished run's next attempt, as rerunning it does; jobs
// queued after it are the new attempt's.
func (e *Emulator) Rerun(r *Run) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r.Attempt++
	r.Status, r.Conclusion = "in_progress", ""
	e.notify()
}

// CompleteJob finishes a job with a conclusion, as if its runner reported
// it.
func (e *Emulator) CompleteJob(j *WorkflowJob, conclusion string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.complete(j, conclusion)
	e.notify()
}

// complete finishes a job; an ephemeral runner that ran it is removed, as
// GitHub removes one once its job is done. The caller holds e.mu.
func (e *Emulator) complete(j *WorkflowJob, conclusion string) {
	if j.Status == "completed" {
		return
	}
	j.Status, j.Conclusion, j.Completed, j.cancelling = "completed", conclusion, e.clock.Now(), false
	if j.Started.IsZero() {
		j.Started = j.Completed
	}
	if r := j.Runner; r != nil && r.Ephemeral {
		r.removed = true
	}
	e.settle(j.Run)
}

// settle completes a run once every job of its attempt has. The caller holds
// e.mu.
func (e *Emulator) settle(r *Run) {
	var jobs []*WorkflowJob
	for _, j := range r.Jobs {
		if j.Attempt == r.Attempt {
			jobs = append(jobs, j)
		}
	}
	if len(jobs) == 0 {
		return
	}
	conclusion := "success"
	for _, j := range jobs {
		switch {
		case j.Status != "completed":
			return
		case j.Conclusion == "cancelled":
			conclusion = "cancelled"
		case j.Conclusion == "failure" && conclusion != "cancelled":
			conclusion = "failure"
		}
	}
	r.Status, r.Conclusion = "completed", conclusion
}

// SetGroupRepositories changes which repositories may use a runner group,
// as an organization owner can.
func (e *Emulator) SetGroupRepositories(groupID int64, ids []int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if g := e.groups[groupID]; g != nil {
		g.Repositories = slices.Clone(ids)
	}
	e.notify()
}

// HideAssignment makes the next n reads of a job that's been assigned a
// runner show it without one, as GitHub can for a moment after handing it
// out.
func (e *Emulator) HideAssignment(j *WorkflowJob, n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j.hidden = n
}

// Job returns a job's current state, for tests to inspect.
func (e *Emulator) Job(id int64) WorkflowJob {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tick()
	return *e.jobs[id]
}

// RunState returns a run's current status and conclusion.
func (e *Emulator) RunState(r *Run) (status, conclusion string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tick()
	return r.Status, r.Conclusion
}

// RunnerGroups returns an organization's runner groups, for tests to
// inspect.
func (e *Emulator) RunnerGroups(org *Account) []RunnerGroup {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.defaultGroup(org)
	var out []RunnerGroup
	for _, g := range e.groups {
		if g.Org == org {
			c := *g
			c.Repositories = slices.Clone(g.Repositories)
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b RunnerGroup) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Runners returns the registered runners, for tests to inspect.
func (e *Emulator) Runners() []Runner {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Runner
	for _, r := range e.runners {
		if !r.removed {
			out = append(out, *r)
		}
	}
	slices.SortFunc(out, func(a, b Runner) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// online reports whether a runner is connected now: it has a request in
// flight, or made one recently.
func (e *Emulator) online(r *Runner) bool {
	return !r.removed && (r.inflight > 0 || (!r.seen.IsZero() && e.clock.Now().Sub(r.seen) < runnerOnlineWindow))
}

// tick fails jobs whose runner has been gone too long, as GitHub does when
// it loses touch with one. The caller holds e.mu.
func (e *Emulator) tick() {
	for _, j := range e.jobs {
		if j.Status == "in_progress" && j.Runner != nil && !e.online(j.Runner) && !j.Runner.seen.IsZero() &&
			e.clock.Now().Sub(j.Runner.seen) > lostRunnerAfter {
			e.complete(j, "failure")
		}
	}
}

// labelsMatch reports whether a runner's labels include all of a job's,
// without regard to case, as GitHub compares them.
func labelsMatch(runner, job []string) bool {
	for _, l := range job {
		if !slices.ContainsFunc(runner, func(r string) bool { return strings.EqualFold(r, l) }) {
			return false
		}
	}
	return len(job) > 0
}

// usable reports whether a group's runners may take a repository's jobs.
func (g *RunnerGroup) usable(r *Repo) bool {
	if g.Org != r.Owner || (!r.Private && !g.AllowsPublicRepositories) {
		return false
	}
	switch g.Visibility {
	case "all":
		return true
	case "selected":
		return slices.Contains(g.Repositories, r.ID)
	}
	return false
}

// assign hands queued jobs, oldest first, to idle connected runners whose
// labels include all of the job's, in a group the job's repository may use,
// whichever runner has waited longest: any such runner, whatever job it was
// made for. The caller holds e.mu.
func (e *Emulator) assign() {
	var queued []*WorkflowJob
	for _, j := range e.jobs {
		if j.Status == "queued" {
			queued = append(queued, j)
		}
	}
	slices.SortFunc(queued, func(a, b *WorkflowJob) int { return cmp.Compare(a.ID, b.ID) })
	for _, j := range queued {
		var best *Runner
		for _, r := range e.runners {
			if r.removed || r.Job != nil || r.used || r.waiting.IsZero() || !e.online(r) ||
				!r.Group.usable(j.Run.Repo) || !labelsMatch(r.Labels, j.Labels) {
				continue
			}
			if best == nil || r.waiting.Before(best.waiting) || (r.waiting.Equal(best.waiting) && r.ID < best.ID) {
				best = r
			}
		}
		if best == nil {
			continue
		}
		best.Job, best.used = j, best.Ephemeral
		j.Runner, j.Status, j.Started = best, "in_progress", e.clock.Now()
		j.Run.Status, j.Run.Conclusion = "in_progress", ""
	}
}

// The REST API's Actions endpoints.

func (e *Emulator) actionsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /installation/repositories", e.authed(e.listInstallationRepos))
	mux.HandleFunc("GET /organizations/{id}", e.authed(e.getOrgByID))
	mux.HandleFunc("GET /orgs/{org}/actions/permissions/fork-pr-contributor-approval", e.authed(e.org(e.getForkApproval)))
	mux.HandleFunc("GET /orgs/{org}/actions/runner-groups", e.authed(e.org(e.listGroups)))
	mux.HandleFunc("POST /orgs/{org}/actions/runner-groups", e.authed(e.org(e.createGroup)))
	mux.HandleFunc("GET /orgs/{org}/actions/runner-groups/{id}/repositories", e.authed(e.org(e.group(e.listGroupRepos))))
	mux.HandleFunc("PUT /orgs/{org}/actions/runner-groups/{id}/repositories", e.authed(e.org(e.group(e.setGroupRepos))))
	mux.HandleFunc("GET /orgs/{org}/actions/runner-groups/{id}/runners", e.authed(e.org(e.group(e.listGroupRunners))))
	mux.HandleFunc("POST /orgs/{org}/actions/runners/generate-jitconfig", e.authed(e.org(e.generateJIT)))
	mux.HandleFunc("GET /orgs/{org}/actions/runners/{id}", e.authed(e.org(e.getRunner)))
	mux.HandleFunc("DELETE /orgs/{org}/actions/runners/{id}", e.authed(e.org(e.deleteRunner)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs", e.authed(e.repo(e.listRuns)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{id}/jobs", e.authed(e.repo(e.listRunJobs)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{id}/attempts/{attempt}/jobs", e.authed(e.repo(e.listRunJobs)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{id}/approvals", e.authed(e.repo(e.listApprovals)))
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runs/{id}/cancel", e.authed(e.repo(e.cancelRun)))
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runs/{id}/force-cancel", e.authed(e.repo(e.forceCancelRun)))
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/jobs/{id}", e.authed(e.repo(e.getJob)))

	mux.HandleFunc("POST /_runner/connect", e.standIn(e.runnerConnect))
	mux.HandleFunc("POST /_runner/acquire", e.standIn(e.runnerAcquire))
	mux.HandleFunc("GET /_runner/jobs/{id}", e.standIn(e.runnerJob))
	mux.HandleFunc("POST /_runner/jobs/{id}/complete", e.standIn(e.runnerComplete))
}

func (e *Emulator) listInstallationRepos(w http.ResponseWriter, r *http.Request, in *Installation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var repos []*Repo
	for _, rp := range e.repos {
		if rp.Owner == in.Account {
			repos = append(repos, rp)
		}
	}
	slices.SortFunc(repos, func(a, b *Repo) int { return cmp.Compare(a.ID, b.ID) })
	out := []map[string]any{}
	for _, rp := range page(w, r, repos, e.URL) {
		out = append(out, e.repoJSON(rp))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(repos), "repository_selection": "all", "repositories": out})
}

func (e *Emulator) getOrgByID(w http.ResponseWriter, r *http.Request, _ *Installation) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.accounts[id]
	if a == nil || a.Type != "Organization" {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, accountJSON(a))
}

// org finds the organization a request names, by login without regard to
// case, which the installation must be on, and holds e.mu while h runs.
func (e *Emulator) org(h func(http.ResponseWriter, *http.Request, *Account)) func(http.ResponseWriter, *http.Request, *Installation) {
	return func(w http.ResponseWriter, r *http.Request, in *Installation) {
		login := r.PathValue("org")
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, a := range e.accounts {
			if a.Type == "Organization" && strings.EqualFold(a.Login, login) {
				if a != in.Account {
					writeError(w, http.StatusForbidden, "Resource not accessible by integration")
					return
				}
				e.tick()
				h(w, r, a)
				return
			}
		}
		writeError(w, http.StatusNotFound, "Not Found")
	}
}

func (e *Emulator) getForkApproval(w http.ResponseWriter, _ *http.Request, org *Account) {
	p := e.forkApproval[org.ID]
	if p == "" {
		p = defaultForkApproval
	}
	writeJSON(w, http.StatusOK, map[string]any{"approval_policy": p})
}

func (e *Emulator) groupJSON(g *RunnerGroup) map[string]any {
	m := map[string]any{
		"id":                         g.ID,
		"name":                       g.Name,
		"visibility":                 g.Visibility,
		"default":                    g.Default,
		"inherited":                  false,
		"allows_public_repositories": g.AllowsPublicRepositories,
		"restricted_to_workflows":    g.RestrictedToWorkflows,
		"selected_workflows":         []string{},
		"runners_url":                fmt.Sprintf("%s/orgs/%s/actions/runner-groups/%d/runners", e.URL, g.Org.Login, g.ID),
		"hosted_runners_url":         fmt.Sprintf("%s/orgs/%s/actions/runner-groups/%d/hosted-runners", e.URL, g.Org.Login, g.ID),
	}
	if g.Visibility == "selected" {
		m["selected_repositories_url"] = fmt.Sprintf(runnerGroupRepositoriesURLTemplate, e.URL, g.Org.Login, g.ID)
	}
	return m
}

func (e *Emulator) listGroups(w http.ResponseWriter, r *http.Request, org *Account) {
	e.defaultGroup(org)
	var gs []*RunnerGroup
	for _, g := range e.groups {
		if g.Org == org {
			gs = append(gs, g)
		}
	}
	slices.SortFunc(gs, func(a, b *RunnerGroup) int { return cmp.Compare(a.ID, b.ID) })
	out := []map[string]any{}
	for _, g := range page(w, r, gs, e.URL) {
		out = append(out, e.groupJSON(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(gs), "runner_groups": out})
}

func (e *Emulator) validationFailed(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"message": "Validation Failed", "errors": []map[string]any{{"message": msg}},
		"documentation_url": "https://docs.github.com/rest", "status": "422",
	})
}

func (e *Emulator) createGroup(w http.ResponseWriter, r *http.Request, org *Account) {
	var body struct {
		Name                     string  `json:"name"`
		Visibility               string  `json:"visibility"`
		SelectedRepositoryIDs    []int64 `json:"selected_repository_ids"`
		AllowsPublicRepositories bool    `json:"allows_public_repositories"`
		RestrictedToWorkflows    bool    `json:"restricted_to_workflows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		e.validationFailed(w, "name is required")
		return
	}
	if body.Visibility == "" {
		body.Visibility = "all"
	}
	if body.Visibility != "all" && body.Visibility != "selected" && body.Visibility != "private" {
		e.validationFailed(w, "visibility is not one of all, selected, private")
		return
	}
	e.defaultGroup(org)
	for _, g := range e.groups {
		if g.Org == org && strings.EqualFold(g.Name, body.Name) {
			writeJSON(w, http.StatusConflict, map[string]any{"message": "Name has already been taken", "status": "409"})
			return
		}
	}
	for _, id := range body.SelectedRepositoryIDs {
		if rp := e.repos[id]; rp == nil || rp.Owner != org {
			e.validationFailed(w, fmt.Sprintf("repository %d isn't the organization's", id))
			return
		}
	}
	g := &RunnerGroup{ID: e.id(), Org: org, Name: body.Name, Visibility: body.Visibility,
		AllowsPublicRepositories: body.AllowsPublicRepositories, RestrictedToWorkflows: body.RestrictedToWorkflows}
	if g.Visibility == "selected" {
		g.Repositories = slices.Clone(body.SelectedRepositoryIDs)
	}
	e.groups[g.ID] = g
	e.notify()
	writeJSON(w, http.StatusCreated, e.groupJSON(g))
}

// group finds the runner group a request names in the organization.
func (e *Emulator) group(h func(http.ResponseWriter, *http.Request, *RunnerGroup)) func(http.ResponseWriter, *http.Request, *Account) {
	return func(w http.ResponseWriter, r *http.Request, org *Account) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		e.defaultGroup(org)
		g := e.groups[id]
		if g == nil || g.Org != org {
			writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		h(w, r, g)
	}
}

func (e *Emulator) listGroupRepos(w http.ResponseWriter, r *http.Request, g *RunnerGroup) {
	var repos []*Repo
	for _, id := range g.Repositories {
		if rp := e.repos[id]; rp != nil {
			repos = append(repos, rp)
		}
	}
	out := []map[string]any{}
	for _, rp := range page(w, r, repos, e.URL) {
		out = append(out, e.repoJSON(rp))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(repos), "repositories": out})
}

func (e *Emulator) setGroupRepos(w http.ResponseWriter, r *http.Request, g *RunnerGroup) {
	var body struct {
		SelectedRepositoryIDs []int64 `json:"selected_repository_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		e.validationFailed(w, "selected_repository_ids is required")
		return
	}
	if g.Visibility != "selected" {
		writeError(w, http.StatusConflict, "The group's visibility isn't selected")
		return
	}
	for _, id := range body.SelectedRepositoryIDs {
		if rp := e.repos[id]; rp == nil || rp.Owner != g.Org {
			e.validationFailed(w, fmt.Sprintf("repository %d isn't the organization's", id))
			return
		}
	}
	g.Repositories = slices.Clone(body.SelectedRepositoryIDs)
	e.notify()
	w.WriteHeader(http.StatusNoContent)
}

func (e *Emulator) runnerJSON(rn *Runner) map[string]any {
	status := "offline"
	if e.online(rn) {
		status = "online"
	}
	labels := []map[string]any{}
	for i, l := range rn.Labels {
		typ := "custom"
		switch strings.ToLower(l) {
		case "self-hosted", "linux", "x64", "arm64", "macos", "windows":
			typ = "read-only"
		}
		labels = append(labels, map[string]any{"id": i + 1, "name": l, "type": typ})
	}
	return map[string]any{
		"id":        rn.ID,
		"name":      rn.Name,
		"os":        "Linux",
		"status":    status,
		"busy":      rn.Job != nil && rn.Job.Status != "completed",
		"ephemeral": rn.Ephemeral,
		"labels":    labels,
	}
}

func (e *Emulator) listGroupRunners(w http.ResponseWriter, r *http.Request, g *RunnerGroup) {
	var rs []*Runner
	for _, rn := range e.runners {
		if rn.Group == g && !rn.removed {
			rs = append(rs, rn)
		}
	}
	slices.SortFunc(rs, func(a, b *Runner) int { return cmp.Compare(a.ID, b.ID) })
	out := []map[string]any{}
	for _, rn := range page(w, r, rs, e.URL) {
		out = append(out, e.runnerJSON(rn))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(rs), "runners": out})
}

// jitConfig is what the emulator's just-in-time configuration carries: what
// the runner stand-in needs to connect as its runner.
type jitConfig struct {
	URL      string `json:"url"`
	RunnerID int64  `json:"runner_id"`
	Name     string `json:"name"`
	Token    string `json:"token"`
	WorkDir  string `json:"work_folder"`
}

func (e *Emulator) generateJIT(w http.ResponseWriter, r *http.Request, org *Account) {
	var body struct {
		Name          string   `json:"name"`
		RunnerGroupID int64    `json:"runner_group_id"`
		Labels        []string `json:"labels"`
		WorkFolder    string   `json:"work_folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		e.validationFailed(w, "name is required")
		return
	}
	if len(body.Labels) == 0 || len(body.Labels) > maxRunnerLabels {
		e.validationFailed(w, "labels must have between 1 and 100 items")
		return
	}
	e.defaultGroup(org)
	g := e.groups[body.RunnerGroupID]
	if g == nil || g.Org != org {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, rn := range e.runners {
		if !rn.removed && rn.Group.Org == org && strings.EqualFold(rn.Name, body.Name) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"message": fmt.Sprintf("Already exists - A runner with the name %s already exists.", body.Name), "status": "409"})
			return
		}
	}
	var tok [20]byte
	_, _ = rand.Read(tok[:])
	work := body.WorkFolder
	if work == "" {
		work = "_work"
	}
	rn := &Runner{ID: e.id(), Name: body.Name, Group: g, Labels: slices.Clone(body.Labels), Ephemeral: true,
		WorkDir: work, token: hex.EncodeToString(tok[:])}
	e.runners[rn.ID] = rn
	cfg, _ := json.Marshal(jitConfig{URL: e.URL, RunnerID: rn.ID, Name: rn.Name, Token: rn.token, WorkDir: work})
	files, _ := json.Marshal(map[string]string{
		".runner":      base64.StdEncoding.EncodeToString(cfg),
		".credentials": base64.StdEncoding.EncodeToString([]byte(`{"scheme":"OAuth"}`)),
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"runner":             e.runnerJSON(rn),
		"encoded_jit_config": base64.StdEncoding.EncodeToString(files),
	})
}

func (e *Emulator) orgRunner(w http.ResponseWriter, r *http.Request, org *Account) *Runner {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rn := e.runners[id]
	if rn == nil || rn.removed || rn.Group.Org != org {
		writeError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return rn
}

func (e *Emulator) getRunner(w http.ResponseWriter, r *http.Request, org *Account) {
	if rn := e.orgRunner(w, r, org); rn != nil {
		writeJSON(w, http.StatusOK, e.runnerJSON(rn))
	}
}

func (e *Emulator) deleteRunner(w http.ResponseWriter, r *http.Request, org *Account) {
	rn := e.orgRunner(w, r, org)
	if rn == nil {
		return
	}
	if rn.Job != nil && rn.Job.Status != "completed" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"message": fmt.Sprintf("Bad request - Runner %q is still running a job\"", rn.Name), "status": "422"})
		return
	}
	rn.removed = true
	e.notify()
	w.WriteHeader(http.StatusNoContent)
}

// runStatuses are the statuses GitHub's runs list filters by; the rest of
// the filter's values are conclusions.
var runStatuses = map[string]bool{"queued": true, "in_progress": true, "completed": true, "waiting": true, "requested": true, "pending": true}

func (e *Emulator) listRuns(w http.ResponseWriter, r *http.Request, rp *Repo) {
	e.tick()
	q := r.URL.Query()
	status := q.Get("status")
	var runs []*Run
	for _, run := range e.runs {
		if run.Repo != rp {
			continue
		}
		if status != "" && ((runStatuses[status] && run.Status != status) || (!runStatuses[status] && run.Conclusion != status)) {
			continue
		}
		if ev := q.Get("event"); ev != "" && run.Event != ev {
			continue
		}
		if sha := q.Get("head_sha"); sha != "" && run.HeadSHA != sha {
			continue
		}
		runs = append(runs, run)
	}
	slices.SortFunc(runs, func(a, b *Run) int { return cmp.Compare(b.ID, a.ID) })
	out := []map[string]any{}
	for _, run := range page(w, r, runs, e.URL) {
		out = append(out, e.runJSON(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "workflow_runs": out})
}

func (e *Emulator) jobJSON(j *WorkflowJob) map[string]any {
	ts := func(t time.Time) any {
		if t.IsZero() {
			return nil
		}
		return t.UTC().Format(time.RFC3339)
	}
	var conclusion any
	if j.Conclusion != "" {
		conclusion = j.Conclusion
	}
	m := map[string]any{
		"id":                j.ID,
		"run_id":            j.Run.ID,
		"run_attempt":       j.Attempt,
		"run_url":           fmt.Sprintf("%s/repos/%s/actions/runs/%d", e.URL, j.Run.Repo.FullName(), j.Run.ID),
		"node_id":           fmt.Sprintf("CR_%d", j.ID),
		"head_sha":          j.Run.HeadSHA,
		"head_branch":       j.Run.HeadBranch,
		"url":               fmt.Sprintf("%s/repos/%s/actions/jobs/%d", e.URL, j.Run.Repo.FullName(), j.ID),
		"html_url":          fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", j.Run.Repo.FullName(), j.Run.ID, j.ID),
		"status":            j.Status,
		"conclusion":        conclusion,
		"created_at":        ts(j.Created),
		"started_at":        ts(j.Started),
		"completed_at":      ts(j.Completed),
		"name":              j.Name,
		"steps":             []any{},
		"labels":            j.Labels,
		"runner_id":         nil,
		"runner_name":       nil,
		"runner_group_id":   nil,
		"runner_group_name": nil,
		"workflow_name":     j.Run.Path,
	}
	if rn := j.Runner; rn != nil {
		if j.hidden > 0 {
			j.hidden--
			m["status"], m["started_at"] = "queued", nil
			return m
		}
		m["runner_id"], m["runner_name"], m["runner_group_id"], m["runner_group_name"] = rn.ID, rn.Name, rn.Group.ID, rn.Group.Name
	}
	return m
}

func (e *Emulator) listRunJobs(w http.ResponseWriter, r *http.Request, rp *Repo) {
	e.tick()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run := e.runs[id]
	if run == nil || run.Repo != rp {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	attempt := run.Attempt
	if s := r.PathValue("attempt"); s != "" {
		attempt, _ = strconv.Atoi(s)
	}
	all := r.PathValue("attempt") == "" && r.URL.Query().Get("filter") == "all"
	var jobs []*WorkflowJob
	for _, j := range run.Jobs {
		if all || j.Attempt == attempt {
			jobs = append(jobs, j)
		}
	}
	out := []map[string]any{}
	for _, j := range page(w, r, jobs, e.URL) {
		out = append(out, e.jobJSON(j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(jobs), "jobs": out})
}

func (e *Emulator) getJob(w http.ResponseWriter, r *http.Request, rp *Repo) {
	e.tick()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	j := e.jobs[id]
	if j == nil || j.Run.Repo != rp {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, e.jobJSON(j))
}

func (e *Emulator) listApprovals(w http.ResponseWriter, r *http.Request, rp *Repo) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run := e.runs[id]
	if run == nil || run.Repo != rp {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	out := []map[string]any{}
	for i := len(run.Approvals) - 1; i >= 0; i-- {
		a := run.Approvals[i]
		envs := []map[string]any{}
		for _, n := range a.Environments {
			envs = append(envs, map[string]any{
				"id": len(n), "node_id": "EN_" + n, "name": n,
				"url":        fmt.Sprintf("%s/repos/%s/environments/%s", e.URL, rp.FullName(), n),
				"html_url":   fmt.Sprintf("https://github.com/%s/deployments/activity_log?environments_filter=%s", rp.FullName(), n),
				"created_at": a.At.UTC().Format(time.RFC3339), "updated_at": a.At.UTC().Format(time.RFC3339),
			})
		}
		out = append(out, map[string]any{"state": a.State, "comment": a.Comment, "environments": envs, "user": accountJSON(a.User)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Emulator) runToCancel(w http.ResponseWriter, r *http.Request, rp *Repo) *Run {
	e.tick()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run := e.runs[id]
	if run == nil || run.Repo != rp {
		writeError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	if run.Status == "completed" {
		writeError(w, http.StatusConflict, "Cannot cancel a workflow run that is completed.")
		return nil
	}
	return run
}

// cancelRun cancels a run as GitHub does: jobs that haven't started are
// cancelled at once, and running ones once their runner acknowledges, which
// a runner that's gone never does.
func (e *Emulator) cancelRun(w http.ResponseWriter, r *http.Request, rp *Repo) {
	run := e.runToCancel(w, r, rp)
	if run == nil {
		return
	}
	for _, j := range run.Jobs {
		switch j.Status {
		case "queued", "waiting":
			e.complete(j, "cancelled")
		case "in_progress":
			j.cancelling = true
		}
	}
	if len(run.Jobs) == 0 {
		run.Status, run.Conclusion = "completed", "cancelled"
	}
	e.notify()
	writeJSON(w, http.StatusAccepted, map[string]any{})
}

// forceCancelRun ends a run and all its jobs at once, whatever their
// runners do.
func (e *Emulator) forceCancelRun(w http.ResponseWriter, r *http.Request, rp *Repo) {
	run := e.runToCancel(w, r, rp)
	if run == nil {
		return
	}
	for _, j := range run.Jobs {
		e.complete(j, "cancelled")
	}
	run.Status, run.Conclusion = "completed", "cancelled"
	e.notify()
	writeJSON(w, http.StatusAccepted, map[string]any{})
}

// The runner stand-in's own endpoints, in place of the runner's protocol
// with GitHub: authenticated by the runner's credential from its
// just-in-time configuration.

func (e *Emulator) standIn(h func(http.ResponseWriter, *http.Request, *Runner)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "RemoteAuth ")
		e.mu.Lock()
		var rn *Runner
		for _, c := range e.runners {
			if tok != "" && c.token == tok {
				rn = c
			}
		}
		if rn == nil || rn.removed {
			e.mu.Unlock()
			writeError(w, http.StatusUnauthorized, "Runner not registered")
			return
		}
		rn.inflight++
		rn.seen = e.clock.Now()
		e.mu.Unlock()
		defer func() {
			e.mu.Lock()
			rn.inflight--
			rn.seen = e.clock.Now()
			e.mu.Unlock()
		}()
		h(w, r, rn)
	}
}

func (e *Emulator) runnerConnect(w http.ResponseWriter, _ *http.Request, rn *Runner) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if rn.used && rn.Job == nil {
		writeError(w, http.StatusConflict, "This ephemeral runner has already run its job")
		return
	}
	if rn.waiting.IsZero() {
		rn.waiting = e.clock.Now()
	}
	e.notify()
	w.WriteHeader(http.StatusNoContent)
}

// jobEnv is what a job's environment holds when the runner runs its hook
// and steps: what the job sets, then GitHub's own variables, which it can't
// change.
func (e *Emulator) jobEnv(j *WorkflowJob) map[string]string {
	env := map[string]string{}
	for k, v := range j.Env {
		env[k] = v
	}
	run := j.Run
	for k, v := range map[string]string{
		"CI":                         "true",
		"GITHUB_ACTIONS":             "true",
		"GITHUB_RUN_ID":              strconv.FormatInt(run.ID, 10),
		"GITHUB_RUN_ATTEMPT":         strconv.Itoa(j.Attempt),
		"GITHUB_RUN_NUMBER":          strconv.Itoa(run.Number),
		"GITHUB_JOB":                 j.Name,
		"GITHUB_REPOSITORY":          run.Repo.FullName(),
		"GITHUB_REPOSITORY_ID":       strconv.FormatInt(run.Repo.ID, 10),
		"GITHUB_REPOSITORY_OWNER":    run.Repo.Owner.Login,
		"GITHUB_REPOSITORY_OWNER_ID": strconv.FormatInt(run.Repo.Owner.ID, 10),
		"GITHUB_EVENT_NAME":          run.Event,
		"GITHUB_REF":                 run.Ref,
		"GITHUB_SHA":                 run.SHA,
		"RUNNER_NAME":                j.Runner.Name,
		"RUNNER_OS":                  "Linux",
	} {
		env[k] = v
	}
	return env
}

// runnerAcquire waits for the runner's job, as the runner's listener long
// polls for one, and answers 204 if none came within the wait.
func (e *Emulator) runnerAcquire(w http.ResponseWriter, r *http.Request, rn *Runner) {
	wait := 20 * time.Second
	if d, err := time.ParseDuration(r.URL.Query().Get("wait")); err == nil && d > 0 && d < wait {
		wait = d
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	e.mu.Lock()
	if rn.waiting.IsZero() {
		rn.waiting = e.clock.Now()
	}
	for {
		e.assign()
		if j := rn.Job; j != nil && !j.delivered {
			j.delivered = true
			body := map[string]any{
				"id": j.ID, "run_id": j.Run.ID, "attempt": j.Attempt, "name": j.Name,
				"repository": j.Run.Repo.FullName(), "env": e.jobEnv(j), "steps": j.Steps,
			}
			e.notify()
			e.mu.Unlock()
			writeJSON(w, http.StatusOK, body)
			return
		}
		ch := e.changed
		e.mu.Unlock()
		select {
		case <-ch:
		case <-timer.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
		e.mu.Lock()
	}
}

func (e *Emulator) runnerJobOf(w http.ResponseWriter, r *http.Request, rn *Runner) *WorkflowJob {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	j := rn.Job
	if j == nil || j.ID != id {
		writeError(w, http.StatusNotFound, "Not this runner's job")
		return nil
	}
	return j
}

func (e *Emulator) runnerJob(w http.ResponseWriter, r *http.Request, rn *Runner) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if j := e.runnerJobOf(w, r, rn); j != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": j.Status, "cancelling": j.cancelling})
	}
}

func (e *Emulator) runnerComplete(w http.ResponseWriter, r *http.Request, rn *Runner) {
	var body struct {
		Conclusion string `json:"conclusion"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	e.mu.Lock()
	defer e.mu.Unlock()
	j := e.runnerJobOf(w, r, rn)
	if j == nil {
		return
	}
	switch {
	case j.cancelling:
		body.Conclusion = "cancelled"
	case body.Conclusion != "success" && body.Conclusion != "failure" && body.Conclusion != "cancelled":
		body.Conclusion = "failure"
	}
	e.complete(j, body.Conclusion)
	e.notify()
	w.WriteHeader(http.StatusNoContent)
}
