package service

import "errors"

// World is what a test of the interface drives (specs/0005-interface.md):
// this package's emulator, or the real service wired to this repository's
// emulators of GitHub, Compute Engine, Google's issuers, Pub/Sub and Cloud
// Storage. Every method acts as GitHub would, or as the
// service would once GitHub tells it something, and fails the test itself
// if it can't. The conformance suite in package conformance runs against
// both, and they must agree. What a job asks of the service, its work disk
// and its save, it asks over HTTP, at URL, signed with Token.
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
	// Commit commits files to a branch, making the branch from the default
	// branch if it doesn't exist; an empty value deletes a file.
	Commit(repo Repository, branch string, files map[string]string)
	// Branch makes a branch, from the default branch, with a commit of its
	// own; DeleteBranch deletes one.
	Branch(repo Repository, name string)
	DeleteBranch(repo Repository, name string)
	// PullRequest opens a pull request into base from head, a branch of the
	// repository, made from the default branch with a commit of its own if
	// it doesn't exist; ForkPullRequest opens one from a contributor's
	// fork.
	PullRequest(repo Repository, base, head string) PullRequest
	ForkPullRequest(repo Repository, base string) PullRequest
	// PushPullRequest pushes a new commit to an open pull request's head
	// branch, as its author does, with files, or a file of its own if
	// files is empty: GitHub moves the pull request's head to it. Runs made
	// before the push keep the event they were made with.
	PushPullRequest(pr PullRequest, files map[string]string) PullRequest

	// Push is the run of a push to a branch, at its commit; Tag the run of
	// a push of a new tag at the default branch's commit; Event a run of
	// any other event, such as workflow_dispatch, on a branch.
	Push(repo Repository, branch string) Run
	Tag(repo Repository, tag string) Run
	Event(repo Repository, event, branch string) Run
	// PullRequestRun is a pull_request run of a pull request's head;
	// PullRequestTargetRun a pull_request_target run of it.
	PullRequestRun(pr PullRequest) Run
	PullRequestTargetRun(pr PullRequest) Run
	// Approve records a maintainer's approval of a run's contribution
	// environment.
	Approve(run Run)
	// Rerun starts a run's next attempt, as a maintainer rerunning it does.
	Rerun(run Run) Run

	// StartJob starts a job of a run whose runs-on is selector, such as
	// lanescape-2vcpu-linux-amd64, and plays it to its check on one of the
	// service's machines, as GitHub hands it to that machine's runner: the
	// check binds the run if it isn't bound, from what GitHub told the
	// runner (specs/0002-machines.md, Binding a run). It returns the job,
	// or a *Refused if the service gives the job no machine or stops it,
	// saying why, as it does for a label that isn't a selector it offers
	// and for a job it fails, cancelling its run; or ErrUnavailable if the
	// check got no confirmed answer.
	StartJob(run Run, selector string) (Job, error)
	// StartJobOn starts a job of run as StartJob does, but on a machine the
	// service made for a job of madeFor, which was cancelled before the
	// machine's runner took it: GitHub hands the machine run's job instead,
	// as it hands any queued job of the consumer to any runner of its
	// selector.
	StartJobOn(run Run, selector string, madeFor Run) (Job, error)
	// Token is the job's machine's identity token for the service.
	Token(job Job) string
	// FinishJob finishes a job with a conclusion, as its runner reports it
	// to GitHub and its machine's supervisor then reports to the service,
	// and returns once the service has done what the job's end makes it do:
	// its work disk is saved if it asked to save it, its conclusion is
	// success and its run's scope saves (specs/0003-disks.md). The
	// service refuses its machine's requests from then on.
	FinishJob(job Job, conclusion string)

	// Unavailable makes GitHub unavailable to the service while on, as when
	// it can't be reached or limits the service's requests: the service
	// confirms nothing, and answers 503.
	Unavailable(on bool)

	// Binding is a run's bound scope, if it has one.
	Binding(run Run) (string, bool)

	// HoldDisks, while on, keeps every work disk that isn't attached yet
	// being made, so its job's requests are answered making; off, as it
	// starts, a work disk is attached by the time its request is answered
	// or soon after, and answered ready.
	HoldDisks(on bool)
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
	// pull_request_target run.
	PullRequest *PullRequest
}

// Job is one of a run's jobs, on one of the service's machines.
type Job struct {
	Run Run
	ID  int64
	// Selector is the selector it names.
	Selector string
	// Machine is the name of the machine it runs on, once it has one.
	Machine string
}

// Refused is the service's refusal of a job: it got no machine, or its
// check stopped it, saying why.
type Refused struct{ Reason string }

func (r *Refused) Error() string { return "the service refused the job: " + r.Reason }

// ErrUnavailable means the job's check got no confirmed answer, and its
// hook would ask again.
var ErrUnavailable = errors.New("the job-started check got no confirmed answer")

// ErrWaiting means GitHub holds the job for its environment's approval, so
// no runner takes it yet.
var ErrWaiting = errors.New("the job waits for its environment's approval")
