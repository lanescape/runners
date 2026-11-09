package service

// World is what a test of the interface drives (specs/0005-interface.md):
// this package's emulator, or the real service wired to this repository's
// emulators of GitHub, Google's OIDC issuers and Cloud Storage. Every
// method acts as GitHub would, or as the service would once GitHub tells
// it something, and fails the test itself if it can't. The conformance
// suite in package conformance runs against both, and they must agree.
type World interface {
	// URL is the service's address, and the audience of every token sent
	// to it.
	URL() string
	// Organization is the organization's ID, which the service serves
	// alone.
	Organization() int64

	// Repository adds a repository of the organization, whose default
	// branch, main, has a commit, and no .github/runners.json.
	Repository(name string) Repository
	// SetRunners commits the default branch's .github/runners.json.
	SetRunners(repo Repository, file string)
	// Bootstrap is the job_workflow_ref a repository's runners.json names
	// for its bootstrap workflow: <owner>/<repo>/.github/workflows/bootstrap.yml@refs/heads/main.
	Bootstrap(repo Repository) string
	// Branch makes a branch, from the default branch, with a commit of its
	// own; DeleteBranch deletes one.
	Branch(repo Repository, name string)
	DeleteBranch(repo Repository, name string)
	// PullRequest opens a pull request into base from a new branch head of
	// the repository itself; ForkPullRequest opens one from a
	// contributor's fork.
	PullRequest(repo Repository, base, head string) PullRequest
	ForkPullRequest(repo Repository, base string) PullRequest
	// ClosePullRequest closes a pull request, and the service learns of it
	// from GitHub's delivery; ReopenPullRequest reopens it.
	ClosePullRequest(pr PullRequest)
	ReopenPullRequest(pr PullRequest)

	// Push is the run of a push to a branch, at its commit; Tag the run of
	// a push of a new tag at the default branch's commit; Event a run of
	// any other event, such as workflow_dispatch, on a branch.
	Push(repo Repository, branch string) Run
	Tag(repo Repository, tag string) Run
	Event(repo Repository, event, branch string) Run
	// PullRequestRun is a pull_request run of a pull request's head;
	// PullRequestTargetRun a pull_request_target run of it, which runs the
	// default branch's workflows.
	PullRequestRun(pr PullRequest) Run
	PullRequestTargetRun(pr PullRequest) Run
	// Rerun starts a run's next attempt, as a maintainer rerunning it does.
	Rerun(run Run) Run
	// Approve records a maintainer's approval of a run's deployments to the
	// contribution environment.
	Approve(run Run)

	// StartJob starts a job of a run, on a runner outside the service:
	// GitHub shows it in progress. workflowRef is its job_workflow_ref, or
	// empty for the run's own workflow's.
	StartJob(run Run, workflowRef string) Job
	// StartMachineJob starts a job of a run on one of the service's own
	// machines, admitted by its job-started check: the only jobs the
	// service's own store is open to.
	StartMachineJob(run Run) Job
	// Token mints a job's GitHub OIDC token, for the service, with claims
	// changed by overrides; an override of nil removes a claim.
	Token(job Job, overrides map[string]any) string
	// FinishJob finishes a job with a conclusion, and the service learns of
	// it from GitHub's delivery.
	FinishJob(job Job, conclusion string)

	// Unavailable makes GitHub unavailable to the service while on, as when
	// it can't be reached or limits the service's requests: the service
	// confirms nothing, and answers 503.
	Unavailable(on bool)
	// Pass lets the service do what it does on its own between requests,
	// such as deleting what's no longer needed.
	Pass()

	// Binding is a run's bound scope, if it has one.
	Binding(run Run) (string, bool)
	// Entries are what a repository's jobs saved and committed, that the
	// service still has.
	Entries(repo Repository) []Entry
	// Choices are a run's restore choices.
	Choices(run Run) []Choice
}

// Repository is a repository of the organization.
type Repository struct {
	ID int64
	// Name is its full name, owner/repo.
	Name string
}

// PullRequest is a pull request into a repository.
type PullRequest struct {
	Repository Repository
	Number     int
	// HeadRepositoryID and HeadSHA are its head.
	HeadRepositoryID int64
	HeadSHA          string
}

// Run is a workflow run.
type Run struct {
	Repository Repository
	ID         int64
	Attempt    int
	Event      string
	// PullRequest is the event's pull request, for a pull_request or
	// pull_request_target run: what its bootstrap sends.
	PullRequest *PullRequest
}

// Job is one of a run's jobs, by its check run's ID, as its token's
// check_run_id names it.
type Job struct {
	Run         Run
	ID          int64
	WorkflowRef string
}

// Entry is a cache entry as the service keeps it.
type Entry struct {
	ID       string
	Scope    string
	Store    string
	Platform string
	Family   string
	Key      string
	// State is pending, until its job is shown to have succeeded, or ready.
	State  string
	Size   int64
	SHA256 string
}

// Choice is a run's restore choice for a platform, family and key: the step
// of the restore order it came from, and its entry's ID, or none.
type Choice struct {
	Store    string
	Platform string
	Family   string
	Key      string
	Step     string
	Entry    string
}
