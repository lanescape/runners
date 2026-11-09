package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v93/github"
)

// ErrConflict is GitHub's confirmed answer that a change conflicts with
// what's there: a runner group or runner of that name exists, or a run to
// cancel has already finished.
var ErrConflict = errors.New("conflict on GitHub")

// ErrRefused is GitHub's confirmed refusal of a change, such as removing a
// runner that's running a job.
var ErrRefused = errors.New("refused by GitHub")

// Job is a workflow job, as GitHub records it. RunnerID and RunnerName are
// the runner GitHub handed it to, once it shows the assignment.
type Job struct {
	ID            int64
	RunID         int64
	RunAttempt    int
	Name          string
	Status        string
	Conclusion    string
	Labels        []string
	RunnerID      int64
	RunnerName    string
	RunnerGroupID int64
	CreatedAt     time.Time
	StartedAt     time.Time
	CompletedAt   time.Time
}

// Approval is one review in a run's review history of its deployments.
type Approval struct {
	State        string
	Environments []string
}

// RunnerGroup is one of the organization's runner groups.
type RunnerGroup struct {
	ID                       int64
	Name                     string
	Visibility               string
	Default                  bool
	AllowsPublicRepositories bool
	RestrictedToWorkflows    bool
}

// Runner is a self-hosted runner registered in the organization.
type Runner struct {
	ID     int64
	Name   string
	Status string
	Busy   bool
	Labels []string
}

// Actions is what the service reads and does in GitHub Actions, as the
// App's installation on the organization.
type Actions interface {
	// Repositories lists the repositories the installation can reach.
	Repositories(ctx context.Context) ([]Repository, error)
	// Runs lists a repository's runs with a status, such as queued.
	Runs(ctx context.Context, repo Repository, status string) ([]WorkflowRun, error)
	// RunJobs lists the jobs of a run's latest attempt.
	RunJobs(ctx context.Context, repo Repository, runID int64) ([]Job, error)
	// AttemptJobs lists the jobs of one of a run's attempts.
	AttemptJobs(ctx context.Context, repo Repository, runID int64, attempt int) ([]Job, error)
	Job(ctx context.Context, repo Repository, id int64) (Job, error)
	// RunApprovals is a run's review history: who approved or rejected its
	// deployments to which environments.
	RunApprovals(ctx context.Context, repo Repository, runID int64) ([]Approval, error)
	// CancelRun and ForceCancelRun return ErrConflict if the run has
	// already finished.
	CancelRun(ctx context.Context, repo Repository, runID int64) error
	ForceCancelRun(ctx context.Context, repo Repository, runID int64) error
	// RerunFailedJobs starts a finished run's next attempt, rerunning its
	// failed jobs and those that depend on them.
	RerunFailedJobs(ctx context.Context, repo Repository, runID int64) error
	// ForkApprovalPolicy is the organization's setting for which outside
	// contributors' fork pull request workflows need a maintainer's
	// approval.
	ForkApprovalPolicy(ctx context.Context) (string, error)
	RunnerGroups(ctx context.Context) ([]RunnerGroup, error)
	// CreateRunnerGroup makes a group that only repositoryID may use;
	// ErrConflict means one of that name exists.
	CreateRunnerGroup(ctx context.Context, name string, repositoryID int64) (RunnerGroup, error)
	RunnerGroupRepositories(ctx context.Context, groupID int64) ([]int64, error)
	SetRunnerGroupRepositories(ctx context.Context, groupID int64, ids []int64) error
	GroupRunners(ctx context.Context, groupID int64) ([]Runner, error)
	// DeleteRunner removes a runner registration; ErrNotFound means it's
	// gone already, and ErrRefused that it's running a job.
	DeleteRunner(ctx context.Context, id int64) error
	// JITConfig registers a just-in-time runner in a group and returns it
	// with its encoded configuration, which is a credential. ErrConflict
	// means a runner of that name exists.
	JITConfig(ctx context.Context, groupID int64, name string, labels []string) (Runner, string, error)
}

// classifyChange is classify for calls that change something, whose
// conflicts and refusals are confirmed answers too.
func classifyChange(err error) error {
	var er *gh.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		switch er.Response.StatusCode {
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", ErrConflict, er.Message)
		case http.StatusUnprocessableEntity:
			return fmt.Errorf("%w: %s", ErrRefused, er.Message)
		}
	}
	return classify(err)
}

// org is the organization's login, which GitHub's organization endpoints
// name it by. It's looked up by ID and kept for a while, so a renamed
// organization is found again.
func (c *Client) org(ctx context.Context) (string, error) {
	c.orgMu.Lock()
	defer c.orgMu.Unlock()
	if c.orgLogin != "" && time.Now().Before(c.orgUntil) {
		return c.orgLogin, nil
	}
	o, _, err := c.gh.Organizations.GetByID(ctx, c.orgID)
	if err != nil {
		return "", classify(err)
	}
	if o.GetID() != c.orgID || o.GetLogin() == "" {
		return "", &Unavailable{Err: fmt.Errorf("organization %d: unexpected answer", c.orgID)}
	}
	c.orgLogin, c.orgUntil = o.GetLogin(), time.Now().Add(10*time.Minute)
	return c.orgLogin, nil
}

// Repositories implements Actions.
func (c *Client) Repositories(ctx context.Context) ([]Repository, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var out []Repository
	for {
		rs, resp, err := c.gh.Apps.ListRepos(ctx, opts)
		if err != nil {
			return nil, classify(err)
		}
		for _, r := range rs.Repositories {
			out = append(out, repoOf(r))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// Runs implements Actions.
func (c *Client) Runs(ctx context.Context, repo Repository, status string) ([]WorkflowRun, error) {
	opts := &gh.ListWorkflowRunsOptions{Status: status, ListOptions: gh.ListOptions{PerPage: 100}}
	var out []WorkflowRun
	for {
		rs, resp, err := c.gh.Actions.ListRepositoryWorkflowRuns(ctx, repo.Owner, repo.Name, opts)
		if err != nil {
			return nil, classify(err)
		}
		for _, r := range rs.WorkflowRuns {
			out = append(out, runOf(r))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

func jobOf(j *gh.WorkflowJob) Job {
	return Job{
		ID:            j.GetID(),
		RunID:         j.GetRunID(),
		RunAttempt:    int(j.GetRunAttempt()),
		Name:          j.GetName(),
		Status:        j.GetStatus(),
		Conclusion:    j.GetConclusion(),
		Labels:        slices.Clone(j.Labels),
		RunnerID:      j.GetRunnerID(),
		RunnerName:    j.GetRunnerName(),
		RunnerGroupID: j.GetRunnerGroupID(),
		CreatedAt:     j.GetCreatedAt().Time,
		StartedAt:     j.GetStartedAt().Time,
		CompletedAt:   j.GetCompletedAt().Time,
	}
}

// RunJobs implements Actions.
func (c *Client) RunJobs(ctx context.Context, repo Repository, runID int64) ([]Job, error) {
	opts := &gh.ListWorkflowJobsOptions{Filter: "latest", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []Job
	for {
		js, resp, err := c.gh.Actions.ListWorkflowJobs(ctx, repo.Owner, repo.Name, runID, opts)
		if err != nil {
			return nil, classify(err)
		}
		for _, j := range js.Jobs {
			out = append(out, jobOf(j))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// AttemptJobs implements Actions.
func (c *Client) AttemptJobs(ctx context.Context, repo Repository, runID int64, attempt int) ([]Job, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var out []Job
	for {
		js, resp, err := c.gh.Actions.ListWorkflowJobsAttempt(ctx, repo.Owner, repo.Name, runID, int64(attempt), opts)
		if err != nil {
			return nil, classify(err)
		}
		for _, j := range js.Jobs {
			out = append(out, jobOf(j))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// Job implements Actions.
func (c *Client) Job(ctx context.Context, repo Repository, id int64) (Job, error) {
	j, _, err := c.gh.Actions.GetWorkflowJobByID(ctx, repo.Owner, repo.Name, id)
	if err != nil {
		return Job{}, classify(err)
	}
	return jobOf(j), nil
}

// RunApprovals implements Actions. go-github has no call for the review
// history, so it's asked for directly.
func (c *Client) RunApprovals(ctx context.Context, repo Repository, runID int64) ([]Approval, error) {
	req, err := c.gh.NewRequest(ctx, http.MethodGet, fmt.Sprintf("repos/%s/%s/actions/runs/%d/approvals", repo.Owner, repo.Name, runID), nil)
	if err != nil {
		return nil, err
	}
	var reviews []struct {
		State        string `json:"state"`
		Environments []struct {
			Name string `json:"name"`
		} `json:"environments"`
	}
	if _, err := c.gh.Do(req, &reviews); err != nil {
		return nil, classify(err)
	}
	var out []Approval
	for _, r := range reviews {
		a := Approval{State: r.State}
		for _, e := range r.Environments {
			a.Environments = append(a.Environments, e.Name)
		}
		out = append(out, a)
	}
	return out, nil
}

// CancelRun implements Actions. GitHub answers 202 Accepted, which go-github
// reports as an AcceptedError: the cancellation has started.
func (c *Client) CancelRun(ctx context.Context, repo Repository, runID int64) error {
	_, err := c.gh.Actions.CancelWorkflowRunByID(ctx, repo.Owner, repo.Name, runID)
	return accepted(err)
}

// ForceCancelRun implements Actions; go-github has no call for it.
func (c *Client) ForceCancelRun(ctx context.Context, repo Repository, runID int64) error {
	req, err := c.gh.NewRequest(ctx, http.MethodPost, fmt.Sprintf("repos/%s/%s/actions/runs/%d/force-cancel", repo.Owner, repo.Name, runID), nil)
	if err != nil {
		return err
	}
	_, err = c.gh.Do(req, nil)
	return accepted(err)
}

// RerunFailedJobs implements Actions. GitHub answers 201 Created.
func (c *Client) RerunFailedJobs(ctx context.Context, repo Repository, runID int64) error {
	_, err := c.gh.Actions.RerunFailedJobsByID(ctx, repo.Owner, repo.Name, runID)
	return accepted(err)
}

func accepted(err error) error {
	var ae *gh.AcceptedError
	if err == nil || errors.As(err, &ae) {
		return nil
	}
	return classifyChange(err)
}

// ForkApprovalPolicy implements Actions.
func (c *Client) ForkApprovalPolicy(ctx context.Context) (string, error) {
	org, err := c.org(ctx)
	if err != nil {
		return "", err
	}
	p, _, err := c.gh.Actions.GetOrganizationForkPRContributorApprovalPermissions(ctx, org)
	if err != nil {
		return "", classify(err)
	}
	if p.ApprovalPolicy == "" {
		return "", &Unavailable{Err: errors.New("fork pull request approval policy: empty answer")}
	}
	return p.ApprovalPolicy, nil
}

func groupOf(g *gh.RunnerGroup) RunnerGroup {
	return RunnerGroup{
		ID:                       g.GetID(),
		Name:                     g.GetName(),
		Visibility:               g.GetVisibility(),
		Default:                  g.GetDefault(),
		AllowsPublicRepositories: g.GetAllowsPublicRepositories(),
		RestrictedToWorkflows:    g.GetRestrictedToWorkflows(),
	}
}

// RunnerGroups implements Actions.
func (c *Client) RunnerGroups(ctx context.Context) ([]RunnerGroup, error) {
	org, err := c.org(ctx)
	if err != nil {
		return nil, err
	}
	opts := &gh.ListOrgRunnerGroupOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var out []RunnerGroup
	for {
		gs, resp, err := c.gh.Actions.ListOrganizationRunnerGroups(ctx, org, opts)
		if err != nil {
			return nil, classify(err)
		}
		for _, g := range gs.RunnerGroups {
			out = append(out, groupOf(g))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// CreateRunnerGroup implements Actions. The group allows public
// repositories, since a consumer may be one, and only repositoryID may use
// it.
func (c *Client) CreateRunnerGroup(ctx context.Context, name string, repositoryID int64) (RunnerGroup, error) {
	org, err := c.org(ctx)
	if err != nil {
		return RunnerGroup{}, err
	}
	g, _, err := c.gh.Actions.CreateOrganizationRunnerGroup(ctx, org, gh.CreateRunnerGroupRequest{
		Name:                     name,
		Visibility:               gh.Ptr("selected"),
		SelectedRepositoryIDs:    []int64{repositoryID},
		AllowsPublicRepositories: gh.Ptr(true),
		RestrictedToWorkflows:    gh.Ptr(false),
	})
	if err != nil {
		return RunnerGroup{}, classifyChange(err)
	}
	return groupOf(g), nil
}

// RunnerGroupRepositories implements Actions.
func (c *Client) RunnerGroupRepositories(ctx context.Context, groupID int64) ([]int64, error) {
	org, err := c.org(ctx)
	if err != nil {
		return nil, err
	}
	opts := &gh.ListOptions{PerPage: 100}
	var out []int64
	for {
		rs, resp, err := c.gh.Actions.ListRepositoryAccessRunnerGroup(ctx, org, groupID, opts)
		if err != nil {
			return nil, classify(err)
		}
		for _, r := range rs.Repositories {
			out = append(out, r.GetID())
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// SetRunnerGroupRepositories implements Actions.
func (c *Client) SetRunnerGroupRepositories(ctx context.Context, groupID int64, ids []int64) error {
	org, err := c.org(ctx)
	if err != nil {
		return err
	}
	_, err = c.gh.Actions.SetRepositoryAccessRunnerGroup(ctx, org, groupID, gh.SetRepoAccessRunnerGroupRequest{SelectedRepositoryIDs: ids})
	if err != nil {
		return classifyChange(err)
	}
	return nil
}

func runnerOf(r *gh.Runner) Runner {
	out := Runner{ID: r.GetID(), Name: r.GetName(), Status: r.GetStatus(), Busy: r.GetBusy()}
	for _, l := range r.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	return out
}

// GroupRunners implements Actions.
func (c *Client) GroupRunners(ctx context.Context, groupID int64) ([]Runner, error) {
	org, err := c.org(ctx)
	if err != nil {
		return nil, err
	}
	opts := &gh.ListOptions{PerPage: 100}
	var out []Runner
	for {
		rs, resp, err := c.gh.Actions.ListRunnerGroupRunners(ctx, org, groupID, opts)
		if err != nil {
			return nil, classify(err)
		}
		for _, r := range rs.Runners {
			out = append(out, runnerOf(r))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// DeleteRunner implements Actions.
func (c *Client) DeleteRunner(ctx context.Context, id int64) error {
	org, err := c.org(ctx)
	if err != nil {
		return err
	}
	if _, err := c.gh.Actions.RemoveOrganizationRunner(ctx, org, id); err != nil {
		return classifyChange(err)
	}
	return nil
}

// JITConfig implements Actions.
func (c *Client) JITConfig(ctx context.Context, groupID int64, name string, labels []string) (Runner, string, error) {
	org, err := c.org(ctx)
	if err != nil {
		return Runner{}, "", err
	}
	cfg, _, err := c.gh.Actions.CreateOrgJITConfig(ctx, org, gh.CreateJITConfigRequest{Name: name, RunnerGroupID: groupID, Labels: labels})
	if err != nil {
		return Runner{}, "", classifyChange(err)
	}
	r := runnerOf(cfg.GetRunner())
	if r.ID == 0 || !strings.EqualFold(r.Name, name) || cfg.GetEncodedJITConfig() == "" {
		return Runner{}, "", &Unavailable{Err: fmt.Errorf("just-in-time runner %s: incomplete answer", name)}
	}
	return r, cfg.GetEncodedJITConfig(), nil
}
