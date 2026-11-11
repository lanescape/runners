// Package binding binds each run's scope, once, in the bucket
// (specs/0002-machines.md, Binding a run). A run is bound by its first
// job's check, on a fresh machine booted from the service's base image,
// from what GitHub told that job's runner, checked against GitHub's own
// records of the run; nothing a job's steps say is ever used.
package binding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/internal/consumer"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/store"
)

// ErrRetry means no binding was made because GitHub or the bucket couldn't
// give a confirmed answer; the caller should try again.
var ErrRetry = errors.New("run binding not made; try again")

// ErrUnapproved means a pull_request_target run has no approval of the
// contribution environment yet: it stays unbound.
var ErrUnapproved = errors.New("the contribution's run isn't approved yet")

// ContributionEnvironment is the environment an approved contribution's
// contribution job names.
const ContributionEnvironment = "contribution"

// Facts are what GitHub told a job's runner about the job, which the
// job-started hook sends from a fresh base-image machine before any step:
// GITHUB_EVENT_NAME, GITHUB_REF, GITHUB_REF_TYPE and GITHUB_SHA, and from
// the event's payload, a pull request's number, head repository and head
// commit.
type Facts struct {
	Event            string `json:"event"`
	Ref              string `json:"ref"`
	RefType          string `json:"ref_type,omitempty"`
	SHA              string `json:"sha"`
	PullRequest      int    `json:"pull_request,omitempty"`
	HeadRepositoryID int64  `json:"head_repository_id,omitempty"`
	HeadSHA          string `json:"head_sha,omitempty"`
}

// Binding is a run's bound scope, as recorded in the bucket.
type Binding struct {
	Scope scope.Scope `json:"scope"`
	// Base is a pull request's base, the scope it falls back to: the
	// listed long-lived branch it targeted when its run was bound, or else
	// main's. It's read for every other scope, which records none.
	Base         scope.Scope `json:"base,omitzero"`
	RepositoryID int64       `json:"repository_id"`
	RunID        int64       `json:"run_id"`
	Event        string      `json:"event"`
	// Commit is the run's commit, whose files decide its saved disks'
	// keys: a push's head commit, a pull request run's head commit, and a
	// contribution's head commit, the event's.
	Commit string `json:"commit"`
	// HeadRepositoryID is the repository the run's commit came from, by
	// GitHub's records: the pull request's head repository for a pull
	// request's scope, and the repository itself for main's. It's 0 when
	// unknown, as for a read binding of a pull_request_target run, which
	// must then be treated as a fork's.
	HeadRepositoryID int64 `json:"head_repository_id,omitempty"`
	// Reason says why the run has its scope, for whoever reads the bucket.
	Reason string `json:"reason"`
	// Fail says the run's jobs are to be failed, with this reason: a pull
	// request run whose head is more than one open pull request's, or an
	// approved contribution whose pull request fails the rules.
	Fail    string    `json:"fail,omitempty"`
	BoundAt time.Time `json:"bound_at"`
}

// Chain is the run's restore chain: what it reads, in order
// (specs/0002-machines.md, Scope).
func (b Binding) Chain() []scope.Scope { return scope.Chain(b.Scope, b.Base) }

// Key is where a run's binding is kept.
func Key(repositoryID, runID int64) string {
	return fmt.Sprintf("bindings/%d/%d.json", repositoryID, runID)
}

// Binder binds runs.
type Binder struct {
	GitHub         github.API
	Objects        store.Objects
	OrganizationID int64
	Clock          clockwork.Clock
	Logger         *slog.Logger
}

func (b *Binder) log() *slog.Logger {
	if b.Logger == nil {
		return slog.Default()
	}
	return b.Logger
}

// Lookup returns a run's binding, if it has one.
func (b *Binder) Lookup(ctx context.Context, repositoryID, runID int64) (Binding, bool, error) {
	data, _, err := b.Objects.Get(ctx, Key(repositoryID, runID))
	if errors.Is(err, store.ErrNotFound) {
		return Binding{}, false, nil
	}
	if err != nil {
		return Binding{}, false, err
	}
	var bd Binding
	if err := json.Unmarshal(data, &bd); err != nil {
		return Binding{}, false, fmt.Errorf("binding %s: %w", Key(repositoryID, runID), err)
	}
	return bd, true, nil
}

func retry(err error) error { return fmt.Errorf("%w: %w", ErrRetry, err) }

// Bind returns the run's binding, making it if it has none, from the facts
// a base-image machine's first check reported for a job of that run, which
// the caller found on that machine's runner by GitHub's records. A
// pull_request_target run that isn't approved yet is ErrUnapproved, and
// stays unbound; any other error means no confirmed answer, and nothing is
// bound.
func (b *Binder) Bind(ctx context.Context, repo github.Repository, run github.WorkflowRun, f Facts) (Binding, error) {
	if repo.OwnerID != b.OrganizationID || run.RepositoryID != repo.ID {
		return Binding{}, fmt.Errorf("run %d isn't the organization's repository %d's", run.ID, repo.ID)
	}
	if bd, ok, err := b.Lookup(ctx, repo.ID, run.ID); err != nil {
		return Binding{}, retry(err)
	} else if ok {
		return bd, nil
	}
	o, err := b.decide(ctx, repo, run, f)
	if errors.Is(err, ErrUnapproved) {
		return Binding{}, err
	}
	if err != nil {
		return Binding{}, retry(err)
	}
	if o.scope.IsRead() {
		o.base = scope.Scope{}
	}
	bd := Binding{Scope: o.scope, Base: o.base, RepositoryID: repo.ID, RunID: run.ID, Event: run.Event, Commit: o.head.sha,
		HeadRepositoryID: o.head.repositoryID, Reason: o.reason, Fail: o.fail, BoundAt: b.Clock.Now().UTC()}
	data, err := json.Marshal(bd)
	if err != nil {
		return Binding{}, err
	}
	if _, err := b.Objects.Create(ctx, Key(repo.ID, run.ID), data); errors.Is(err, store.ErrPrecondition) {
		// Another of the run's jobs bound it first, or our own write's
		// response was lost: what's there is the binding.
		existing, ok, lerr := b.Lookup(ctx, repo.ID, run.ID)
		if lerr != nil || !ok {
			return Binding{}, retry(errors.Join(err, lerr))
		}
		return existing, nil
	} else if err != nil {
		return Binding{}, retry(err)
	}
	b.log().Info("run bound", "repository_id", repo.ID, "run_id", run.ID, "event", run.Event, "scope", bd.Scope.String(),
		"base", bd.Base.String(), "reason", bd.Reason, "fail", bd.Fail)
	return bd, nil
}

// head is the commit a run is for, and the repository it came from.
type head struct {
	repositoryID int64
	sha          string
}

// outcome is what a rule decides: the scope it grants, and for a pull
// request's, its base; the head it found; why; and whether the run's jobs
// are failed.
type outcome struct {
	scope, base scope.Scope
	head        head
	reason      string
	fail        string
}

// read is an outcome of read, for a reason.
func read(reason string) outcome { return outcome{scope: scope.Read, reason: reason} }

// decide works out a run's scope. Any error but ErrUnapproved means GitHub
// gave no confirmed answer, and nothing may be bound; anything GitHub
// confirmed that fails a rule makes the scope read, with the reason.
func (b *Binder) decide(ctx context.Context, repo github.Repository, run github.WorkflowRun, f Facts) (outcome, error) {
	// What's known of the head without a rule: GitHub's run record.
	h := head{repositoryID: run.HeadRepositoryID, sha: run.HeadSHA}
	withHead := func(o outcome) outcome {
		if o.head.sha == "" {
			o.head = h
		}
		return o
	}
	if f.Event != run.Event {
		return withHead(read("the runner's event isn't the run's")), nil
	}
	var r rule
	switch run.Event {
	case "push":
		r = b.push
	case "pull_request":
		r = b.pullRequest
	case "pull_request_target":
		r = b.pullRequestTarget
	default:
		return withHead(read("event " + run.Event + " binds read")), nil
	}
	o, err := r(ctx, repo, run, f)
	if errors.Is(err, ErrUnapproved) || github.IsUnavailable(err) {
		return outcome{}, err
	}
	if err != nil {
		o = read(o.reason + ": " + err.Error())
		if run.Event == "pull_request_target" {
			// An approved contribution whose pull request fails the rules
			// fails its jobs: its contribution job included.
			o.fail = "the approved contribution's pull request isn't the run's: " + o.reason
		}
		return withHead(o), nil
	}
	if o.scope.IsRead() && run.Event == "pull_request_target" && o.fail == "" {
		o.fail = "the approved contribution's pull request isn't the run's: " + o.reason
	}
	o = withHead(o)
	if _, ok := o.scope.PullRequestNumber(); ok {
		// A head that's more than one open pull request's can't say which
		// pull request's run this is.
		open, err := b.GitHub.OpenPullRequests(ctx, repo)
		if err != nil {
			return outcome{}, err
		}
		heads := 0
		for _, p := range open {
			if p.HeadRepositoryID == o.head.repositoryID && p.HeadSHA == o.head.sha {
				heads++
			}
		}
		if heads > 1 {
			return outcome{scope: scope.Read, head: o.head, reason: fmt.Sprintf("the run's head is the head of %d open pull requests", heads),
				fail: "each head commit must be the head of one open pull request: this run's is the head of " + strconv.Itoa(heads)}, nil
		}
	}
	return o, nil
}

// A rule returns the outcome it grants, or read with a reason; an error with
// the reason means GitHub's answer failed the rule, unless it's
// Unavailable or ErrUnapproved.
type rule func(ctx context.Context, repo github.Repository, run github.WorkflowRun, f Facts) (outcome, error)

// push is for a push to the default branch, which binds main's scope, or to
// a long-lived branch the default branch's .github/runners.json lists,
// which binds that branch's: by the runner's ref and ref type, whose head
// commit GitHub shows is on that branch.
func (b *Binder) push(ctx context.Context, repo github.Repository, run github.WorkflowRun, f Facts) (outcome, error) {
	branch, ok := strings.CutPrefix(f.Ref, "refs/heads/")
	if !ok || f.RefType != "branch" || branch == "" {
		return read("push to a ref other than a branch"), nil
	}
	if f.SHA == "" || f.SHA != run.HeadSHA {
		return read("the runner's commit isn't the run's"), nil
	}
	if run.HeadBranch != "" && run.HeadBranch != branch {
		return read("the runner's branch isn't the run's"), nil
	}
	granted, why := scope.Main, "push to the default branch"
	if branch != repo.DefaultBranch {
		listed, err := b.Branches(ctx, repo)
		if err != nil {
			return read("reading " + consumer.RunnersFile), err
		}
		name, ok := listed[branch]
		if !ok {
			return read("push to a branch that isn't the default branch or a listed long-lived branch"), nil
		}
		granted, why = scope.Branch(name), "push to the long-lived branch "+branch
	}
	tip, err := b.GitHub.BranchHead(ctx, repo, branch)
	if err != nil {
		return read("reading the branch " + branch), err
	}
	status, err := b.GitHub.Compare(ctx, repo, tip, run.HeadSHA)
	if err != nil {
		return read("comparing with the branch " + branch), err
	}
	// The run's commit is the branch's or behind it: it's on the branch.
	if status != "identical" && status != "behind" {
		return read("the run's commit is " + status + " of the branch " + branch), nil
	}
	return outcome{scope: granted, head: head{repositoryID: repo.ID, sha: run.HeadSHA}, reason: why}, nil
}

// baseOf is the base of a pull request into branch: that branch's scope if
// the default branch's .github/runners.json lists it, or else main's.
func (b *Binder) baseOf(ctx context.Context, repo github.Repository, branch string) (scope.Scope, error) {
	if branch == repo.DefaultBranch {
		return scope.Main, nil
	}
	listed, err := b.Branches(ctx, repo)
	if err != nil {
		return scope.Scope{}, err
	}
	if name, ok := listed[branch]; ok {
		return scope.Branch(name), nil
	}
	return scope.Main, nil
}

var pullRef = regexp.MustCompile(`^refs/pull/([0-9]+)/merge$`)

// pullRequest is for a pull_request run, by the pull request its ref
// names. A same-repository pull request's author controls its workflow, but
// not GitHub's own variables, which the job's steps haven't run to change.
func (b *Binder) pullRequest(ctx context.Context, repo github.Repository, run github.WorkflowRun, f Facts) (outcome, error) {
	m := pullRef.FindStringSubmatch(f.Ref)
	if m == nil {
		return read("the pull_request run's ref names no pull request"), nil
	}
	n, ok := scope.ParseNumber(m[1])
	if !ok {
		return read("the pull_request run's ref names no pull request"), nil
	}
	if run.HeadRepositoryID != repo.ID {
		// A fork's pull request is never one of the repository's own
		// (specs/0002-machines.md, the scope table).
		return read("the run's head is in another repository"), nil
	}
	if len(run.PullRequests) > 0 && !slices.Contains(run.PullRequests, n) {
		// GitHub's record of the run names the pull requests whose head is
		// its own: the ref must name one of them.
		return read("the ref names a pull request that GitHub's record of the run doesn't"), nil
	}
	pr, err := b.GitHub.PullRequest(ctx, repo, n)
	if err != nil {
		return read("reading pull request " + strconv.Itoa(n)), err
	}
	if pr.BaseRepositoryID != repo.ID {
		return read("the pull request doesn't target this repository"), nil
	}
	if pr.HeadRepositoryID != run.HeadRepositoryID {
		return read("the pull request's head repository isn't the run's"), nil
	}
	ok, err = b.isPullCommit(ctx, repo, pr, run.HeadSHA)
	if err != nil {
		return read("checking the pull request's commits"), err
	}
	if !ok {
		return read("the run's head commit isn't one of the pull request's"), nil
	}
	base, err := b.baseOf(ctx, repo, pr.BaseRef)
	if err != nil {
		return read("reading " + consumer.RunnersFile), err
	}
	return outcome{scope: scope.PullRequest(n), base: base, head: head{repositoryID: run.HeadRepositoryID, sha: run.HeadSHA},
		reason: "pull_request run of its pull request"}, nil
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Approved reports whether GitHub records an approval of the contribution
// environment for a run.
func Approved(ctx context.Context, gh github.API, repo github.Repository, runID int64) (bool, error) {
	as, err := gh.RunApprovals(ctx, repo, runID)
	if err != nil {
		return false, err
	}
	for _, a := range as {
		if a.State == "approved" && slices.ContainsFunc(a.Environments, func(e string) bool { return strings.EqualFold(e, ContributionEnvironment) }) {
			return true, nil
		}
	}
	return false, nil
}

// pullRequestTarget is for an approved contribution's pull_request_target
// run, whose ref and run record are the base branch's and name no pull
// request: from its event's pull request, only once GitHub records an
// approval of the contribution environment for the run, and only if that
// pull request is open, targets this repository, and has the event's head
// repository, with the event's head commit among its commits.
func (b *Binder) pullRequestTarget(ctx context.Context, repo github.Repository, run github.WorkflowRun, f Facts) (outcome, error) {
	ok, err := Approved(ctx, b.GitHub, repo, run.ID)
	if err != nil {
		if github.IsUnavailable(err) {
			return outcome{}, err
		}
		return outcome{}, &github.Unavailable{Err: err}
	}
	if !ok {
		return outcome{}, ErrUnapproved
	}
	if f.SHA == "" || f.SHA != run.HeadSHA {
		return read("the runner's commit isn't the run's"), nil
	}
	if f.PullRequest <= 0 || f.HeadRepositoryID <= 0 || !commitSHA.MatchString(f.HeadSHA) {
		return read("the event names no pull request, head repository and head commit"), nil
	}
	pr, err := b.GitHub.PullRequest(ctx, repo, f.PullRequest)
	if err != nil {
		return read("reading pull request " + strconv.Itoa(f.PullRequest)), err
	}
	if pr.State != "open" {
		return read("the pull request isn't open"), nil
	}
	if pr.BaseRepositoryID != repo.ID {
		return read("the pull request doesn't target this repository"), nil
	}
	if pr.HeadRepositoryID == 0 || pr.HeadRepositoryID != f.HeadRepositoryID {
		return read("the pull request's head repository isn't the event's"), nil
	}
	ok, err = b.isPullCommit(ctx, repo, pr, f.HeadSHA)
	if err != nil {
		return read("checking the pull request's commits"), err
	}
	if !ok {
		return read("the event's head commit isn't one of the pull request's"), nil
	}
	base, err := b.baseOf(ctx, repo, pr.BaseRef)
	if err != nil {
		return read("reading " + consumer.RunnersFile), err
	}
	return outcome{scope: scope.PullRequest(f.PullRequest), base: base, head: head{repositoryID: f.HeadRepositoryID, sha: f.HeadSHA},
		reason: "approved pull_request_target run of its event's pull request"}, nil
}

// isPullCommit reports whether sha is one of a pull request's commits: its
// head, one GitHub lists, or, past the 250 GitHub lists, one that its head
// descends from and its base doesn't.
func (b *Binder) isPullCommit(ctx context.Context, repo github.Repository, pr github.PullRequest, sha string) (bool, error) {
	if sha == "" {
		return false, nil
	}
	if sha == pr.HeadSHA {
		return true, nil
	}
	commits, err := b.GitHub.PullRequestCommits(ctx, repo, pr.Number)
	if err != nil {
		return false, err
	}
	if slices.Contains(commits, sha) {
		return true, nil
	}
	if len(commits) >= pr.Commits || pr.BaseSHA == "" {
		return false, nil
	}
	// The head descends from sha if sha is behind it or the same.
	toHead, err := b.GitHub.Compare(ctx, repo, pr.HeadSHA, sha)
	if err != nil || (toHead != "behind" && toHead != "identical") {
		return false, err
	}
	// The base doesn't contain sha if sha is ahead of it or diverged.
	fromBase, err := b.GitHub.Compare(ctx, repo, pr.BaseSHA, sha)
	if err != nil {
		return false, err
	}
	return fromBase == "ahead" || fromBase == "diverged", nil
}

// Branches reads the default branch's long-lived branches from its
// .github/runners.json, never a run's own copy, and logs why it ignores any
// entry. A file that's missing lists none; one that isn't valid lists none,
// and says so.
func (b *Binder) Branches(ctx context.Context, repo github.Repository) (map[string]string, error) {
	return ReadBranches(ctx, b.GitHub, repo, b.log())
}

// ReadBranches is Binder.Branches, for any reader of GitHub.
func ReadBranches(ctx context.Context, gh github.API, repo github.Repository, log *slog.Logger) (map[string]string, error) {
	tip, err := gh.BranchHead(ctx, repo, repo.DefaultBranch)
	if err != nil {
		return nil, err
	}
	data, err := gh.File(ctx, repo, consumer.RunnersFile, tip)
	if errors.Is(err, github.ErrNotFound) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	f, err := consumer.Parse(data)
	if err != nil {
		log.Warn("the default branch's "+consumer.RunnersFile+" isn't valid: it lists no long-lived branches", "repository_id", repo.ID,
			"why", err.Error())
		return map[string]string{}, nil
	}
	listed, problems := f.BranchScopes(repo.DefaultBranch)
	for _, p := range problems {
		log.Warn("an entry of the default branch's "+consumer.RunnersFile+" is ignored", "repository_id", repo.ID, "why", p)
	}
	return listed, nil
}
