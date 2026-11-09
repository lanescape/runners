// Package binding binds each run's scope, once, in the bucket
// (specs/0002-machines.md, Scope). A binding comes from what GitHub signed
// in the run's first job's token, checked against GitHub's records, never
// from what the job says, with one narrow exception: the pull request, head
// repository and head commit that the consumer's bootstrap workflow reads
// from its event in a pull_request_target run, each checked against GitHub's
// pull request (specs/0001-architecture.md, Forks and contributions).
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

	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/scope"
	"github.com/lanescape/runners/internal/store"
)

// ErrRefused means the caller may not bind this run at all: its repository
// isn't the organization's, or its run isn't in progress.
var ErrRefused = errors.New("run binding refused")

// ErrRetry means no binding was made because GitHub or the bucket couldn't
// give a confirmed answer; the caller should try again.
var ErrRetry = errors.New("run binding not made; try again")

// RunnersFile is the consumer's file naming its runners and its bootstrap
// workflow (specs/0005-interface.md).
const RunnersFile = ".github/runners.json"

// Request is what the caller may say. Only the bootstrap workflow's word is
// used, and only in a pull_request_target run, whose run record and token
// name the base branch's commit rather than the pull request's: it sends what
// its own github.event says, github.event.pull_request.number, .head.repo.id
// and .head.sha, and the service checks each against GitHub's pull request.
type Request struct {
	PullRequest      int    `json:"pull_request,omitempty"`
	HeadRepositoryID int64  `json:"head_repository_id,omitempty"`
	HeadSHA          string `json:"head_sha,omitempty"`
}

// Binding is a run's bound scope, as recorded in the bucket.
type Binding struct {
	Scope        scope.Scope `json:"scope"`
	RepositoryID int64       `json:"repository_id"`
	RunID        int64       `json:"run_id"`
	Event        string      `json:"event"`
	// HeadRepositoryID is the repository the run's commit came from, by
	// GitHub's records: the pull request's head repository for a pull
	// request's scope, and the repository itself for main's. It's 0 when
	// unknown, as for a read binding of a pull_request_target run, which
	// must then be treated as a fork's.
	HeadRepositoryID int64 `json:"head_repository_id,omitempty"`
	// Reason says why the run has its scope, for whoever reads the bucket.
	Reason  string    `json:"reason"`
	BoundAt time.Time `json:"bound_at"`
}

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

// Bind returns the run's binding, making it if it has none. Every call is
// checked in full: the caller's repository must be the organization's and its
// run in progress, by GitHub's records.
func (b *Binder) Bind(ctx context.Context, c *auth.ActionsClaims, req Request) (Binding, error) {
	if c.RepositoryOwnerID != b.OrganizationID {
		return Binding{}, fmt.Errorf("%w: token's repository isn't the organization's", ErrRefused)
	}
	repo, err := b.GitHub.RepositoryByID(ctx, c.RepositoryID)
	if github.IsUnavailable(err) {
		return Binding{}, retry(err)
	}
	if err != nil {
		return Binding{}, fmt.Errorf("%w: repository %d: %w", ErrRefused, c.RepositoryID, err)
	}
	if repo.OwnerID != b.OrganizationID {
		return Binding{}, fmt.Errorf("%w: GitHub reports repository %d's owner isn't the organization", ErrRefused, repo.ID)
	}
	run, err := b.GitHub.WorkflowRun(ctx, repo, c.RunID)
	if github.IsUnavailable(err) {
		return Binding{}, retry(err)
	}
	if err != nil {
		return Binding{}, fmt.Errorf("%w: run %d: %w", ErrRefused, c.RunID, err)
	}
	if run.RepositoryID != repo.ID {
		return Binding{}, fmt.Errorf("%w: run %d isn't repository %d's", ErrRefused, run.ID, repo.ID)
	}
	if run.Status != "in_progress" {
		return Binding{}, fmt.Errorf("%w: run %d is %s, not in progress", ErrRefused, run.ID, run.Status)
	}

	if bd, ok, err := b.Lookup(ctx, repo.ID, run.ID); err != nil {
		return Binding{}, retry(err)
	} else if ok {
		return bd, nil
	}

	s, h, reason, err := b.decide(ctx, c, req, repo, run)
	if err != nil {
		return Binding{}, retry(err)
	}
	bd := Binding{Scope: s, RepositoryID: repo.ID, RunID: run.ID, Event: run.Event, HeadRepositoryID: h.repositoryID,
		Reason: reason, BoundAt: b.Clock.Now().UTC()}
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
	b.log().Info("run bound", "repository_id", repo.ID, "run_id", run.ID, "event", run.Event, "scope", s.String(), "reason", reason)
	return bd, nil
}

// head is the commit a run is for, and the repository it came from.
type head struct {
	repositoryID int64
	sha          string
}

// decide works out a run's scope. Any error means GitHub gave no confirmed
// answer, and nothing may be bound; anything GitHub confirmed that fails a
// rule makes the scope read, with the reason.
func (b *Binder) decide(ctx context.Context, c *auth.ActionsClaims, req Request, repo github.Repository, run github.WorkflowRun) (scope.Scope, head, string, error) {
	// What's known of the head without a rule: GitHub's run record, which
	// for a pull_request_target run is the base's.
	h := head{repositoryID: run.HeadRepositoryID, sha: run.HeadSHA}
	if run.Event == "pull_request_target" {
		h = head{}
	}
	if run.Event != c.EventName {
		return scope.Read, h, "the token's event isn't the run's", nil
	}
	var r rule
	switch run.Event {
	case "push":
		r = b.main
	case "pull_request":
		r = b.pullRequest
	case "pull_request_target":
		r = b.pullRequestTarget
	default:
		return scope.Read, h, "event " + run.Event + " binds read", nil
	}
	s, ruleHead, reason, err := r(ctx, c, req, repo, run)
	if github.IsUnavailable(err) {
		return scope.Read, h, "", err
	}
	if err != nil {
		return scope.Read, h, reason + ": " + err.Error(), nil
	}
	if ruleHead.repositoryID != 0 {
		h = ruleHead
	}
	if _, ok := s.PullRequestNumber(); ok {
		// A head that's more than one open pull request's can't say which
		// pull request's run this is.
		open, err := b.GitHub.OpenPullRequests(ctx, repo)
		if err != nil {
			return scope.Read, h, "", err
		}
		heads := 0
		for _, p := range open {
			if p.HeadRepositoryID == h.repositoryID && p.HeadSHA == h.sha {
				heads++
			}
		}
		if heads > 1 {
			return scope.Read, h, fmt.Sprintf("the run's head is the head of %d open pull requests", heads), nil
		}
	}
	return s, h, reason, nil
}

// A rule returns the scope it grants and the head it found, or read with a
// reason; an error with the reason means GitHub's answer failed the rule,
// unless it's Unavailable.
type rule func(ctx context.Context, c *auth.ActionsClaims, req Request, repo github.Repository, run github.WorkflowRun) (scope.Scope, head, string, error)

// main is for a push to the default branch, by its token's signed ref and
// ref type, whose head commit GitHub shows is on the default branch.
func (b *Binder) main(ctx context.Context, c *auth.ActionsClaims, _ Request, repo github.Repository, run github.WorkflowRun) (scope.Scope, head, string, error) {
	if c.Ref != "refs/heads/"+repo.DefaultBranch || c.RefType != "branch" {
		return scope.Read, head{}, "push to a ref other than the default branch", nil
	}
	if c.SHA == "" || c.SHA != run.HeadSHA {
		return scope.Read, head{}, "the token's commit isn't the run's", nil
	}
	branch, err := b.GitHub.BranchHead(ctx, repo, repo.DefaultBranch)
	if err != nil {
		return scope.Read, head{}, "reading the default branch", err
	}
	status, err := b.GitHub.Compare(ctx, repo, branch, run.HeadSHA)
	if err != nil {
		return scope.Read, head{}, "comparing with the default branch", err
	}
	// The run's commit is the branch's or behind it: it's on the branch.
	if status != "identical" && status != "behind" {
		return scope.Read, head{}, "the run's commit is " + status + " of the default branch", nil
	}
	return scope.Main, head{repositoryID: repo.ID, sha: run.HeadSHA}, "push to the default branch", nil
}

var pullRef = regexp.MustCompile(`^refs/pull/([0-9]+)/merge$`)

// pullRequest is for a pull_request run, by the pull request its token's
// signed ref names. Nothing the job says is used: a same-repository pull
// request's author controls its workflow.
func (b *Binder) pullRequest(ctx context.Context, c *auth.ActionsClaims, _ Request, repo github.Repository, run github.WorkflowRun) (scope.Scope, head, string, error) {
	m := pullRef.FindStringSubmatch(c.Ref)
	if m == nil {
		return scope.Read, head{}, "pull_request token's ref names no pull request", nil
	}
	n, ok := scope.ParseNumber(m[1])
	if !ok {
		return scope.Read, head{}, "pull_request token's ref names no pull request", nil
	}
	if run.HeadRepositoryID != repo.ID {
		// A fork's pull request is never one of the repository's own
		// (specs/0002-machines.md, the scope table).
		return scope.Read, head{}, "the run's head is in another repository", nil
	}
	pr, err := b.GitHub.PullRequest(ctx, repo, n)
	if err != nil {
		return scope.Read, head{}, "reading pull request " + strconv.Itoa(n), err
	}
	if pr.BaseRepositoryID != repo.ID {
		return scope.Read, head{}, "the pull request doesn't target this repository", nil
	}
	if pr.HeadRepositoryID != run.HeadRepositoryID || pr.HeadSHA != run.HeadSHA {
		return scope.Read, head{}, "the pull request's head isn't the run's", nil
	}
	return scope.PullRequest(n), head{repositoryID: run.HeadRepositoryID, sha: run.HeadSHA}, "pull_request run of its pull request", nil
}

// runnersFile is the part of .github/runners.json the service reads.
type runnersFile struct {
	Bootstrap string
}

func parseRunners(data []byte) (runnersFile, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return runnersFile{}, fmt.Errorf("%s isn't a JSON object", RunnersFile)
	}
	raw, ok := m["bootstrap"]
	if !ok {
		return runnersFile{}, fmt.Errorf("%s names no bootstrap workflow", RunnersFile)
	}
	var f runnersFile
	if err := json.Unmarshal(raw, &f.Bootstrap); err != nil || f.Bootstrap == "" {
		return runnersFile{}, fmt.Errorf("%s's bootstrap isn't a workflow reference", RunnersFile)
	}
	return f, nil
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// pullRequestTarget is for a pull_request_target run, whose token and run
// record name the base branch's commit, and no pull request: only the
// consumer's bootstrap workflow, named in the default branch's
// .github/runners.json, may say which pull request, head repository and
// head commit its event is for, and GitHub must show that pull request
// targets this repository from that head repository, with that commit among
// its commits.
func (b *Binder) pullRequestTarget(ctx context.Context, c *auth.ActionsClaims, req Request, repo github.Repository, _ github.WorkflowRun) (scope.Scope, head, string, error) {
	branch, err := b.GitHub.BranchHead(ctx, repo, repo.DefaultBranch)
	if err != nil {
		return scope.Read, head{}, "reading the default branch", err
	}
	data, err := b.GitHub.File(ctx, repo, RunnersFile, branch)
	if err != nil {
		return scope.Read, head{}, "reading " + RunnersFile, err
	}
	f, err := parseRunners(data)
	if err != nil {
		return scope.Read, head{}, "no bootstrap workflow", err
	}
	// The bootstrap must be a workflow of this repository's, on its default
	// branch, so that only what's merged there can name a pull request.
	prefix := strings.ToLower(c.Repository + "/.github/workflows/")
	if !strings.HasPrefix(strings.ToLower(f.Bootstrap), prefix) || !strings.HasSuffix(f.Bootstrap, "@refs/heads/"+repo.DefaultBranch) {
		return scope.Read, head{}, "the bootstrap isn't a workflow on this repository's default branch", nil
	}
	if c.JobWorkflowRef != f.Bootstrap {
		return scope.Read, head{}, "the caller isn't the bootstrap workflow", nil
	}
	if req.PullRequest <= 0 {
		return scope.Read, head{}, "the bootstrap named no pull request", nil
	}
	if req.HeadRepositoryID <= 0 || !commitSHA.MatchString(req.HeadSHA) {
		return scope.Read, head{}, "the bootstrap named no head repository and commit", nil
	}
	pr, err := b.GitHub.PullRequest(ctx, repo, req.PullRequest)
	if err != nil {
		return scope.Read, head{}, "reading pull request " + strconv.Itoa(req.PullRequest), err
	}
	if pr.BaseRepositoryID != repo.ID {
		return scope.Read, head{}, "the pull request doesn't target this repository", nil
	}
	if pr.HeadRepositoryID == 0 || pr.HeadRepositoryID != req.HeadRepositoryID {
		return scope.Read, head{}, "the pull request's head repository isn't the event's", nil
	}
	ok, err := b.isPullCommit(ctx, repo, pr, req.HeadSHA)
	if err != nil {
		return scope.Read, head{}, "checking the pull request's commits", err
	}
	if !ok {
		return scope.Read, head{}, "the event's head commit isn't one of the pull request's", nil
	}
	return scope.PullRequest(req.PullRequest), head{repositoryID: req.HeadRepositoryID, sha: req.HeadSHA},
		"pull_request_target run, named by the bootstrap workflow", nil
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
