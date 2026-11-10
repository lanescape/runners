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

// RunnersFile is the consumer's file naming its runners, its bootstrap
// workflow and its long-lived branches (specs/0005-interface.md).
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
	Scope scope.Scope `json:"scope"`
	// Base is a pull request's base, the scope it falls back to: the
	// listed long-lived branch it targeted when its run was bound, or else
	// main's. It's read for every other scope, which records none.
	Base         scope.Scope `json:"base,omitzero"`
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

	o, err := b.decide(ctx, c, req, repo, run)
	if err != nil {
		return Binding{}, retry(err)
	}
	s, reason := o.scope, o.reason
	bd := Binding{Scope: s, Base: o.base, RepositoryID: repo.ID, RunID: run.ID, Event: run.Event, HeadRepositoryID: o.head.repositoryID,
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
	b.log().Info("run bound", "repository_id", repo.ID, "run_id", run.ID, "event", run.Event, "scope", s.String(), "base", o.base.String(),
		"reason", reason)
	return bd, nil
}

// head is the commit a run is for, and the repository it came from.
type head struct {
	repositoryID int64
	sha          string
}

// outcome is what a rule decides: the scope it grants, and for a pull
// request's, its base; the head it found; and why.
type outcome struct {
	scope, base scope.Scope
	head        head
	reason      string
}

// read is an outcome of read, for a reason.
func read(reason string) outcome { return outcome{scope: scope.Read, reason: reason} }

// decide works out a run's scope. Any error means GitHub gave no confirmed
// answer, and nothing may be bound; anything GitHub confirmed that fails a
// rule makes the scope read, with the reason.
func (b *Binder) decide(ctx context.Context, c *auth.ActionsClaims, req Request, repo github.Repository, run github.WorkflowRun) (outcome, error) {
	// What's known of the head without a rule: GitHub's run record, which
	// for a pull_request_target run is the base's.
	h := head{repositoryID: run.HeadRepositoryID, sha: run.HeadSHA}
	if run.Event == "pull_request_target" {
		h = head{}
	}
	withHead := func(o outcome) outcome {
		o.head = h
		return o
	}
	if run.Event != c.EventName {
		return withHead(read("the token's event isn't the run's")), nil
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
	o, err := r(ctx, c, req, repo, run)
	if github.IsUnavailable(err) {
		return outcome{}, err
	}
	if err != nil {
		return withHead(read(o.reason + ": " + err.Error())), nil
	}
	if o.head.repositoryID == 0 {
		o.head = h
	}
	if o.scope.IsRead() {
		o.base = scope.Scope{}
	}
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
			return outcome{scope: scope.Read, head: o.head, reason: fmt.Sprintf("the run's head is the head of %d open pull requests", heads)}, nil
		}
	}
	return o, nil
}

// A rule returns the outcome it grants, or read with a reason; an error with
// the reason means GitHub's answer failed the rule, unless it's
// Unavailable.
type rule func(ctx context.Context, c *auth.ActionsClaims, req Request, repo github.Repository, run github.WorkflowRun) (outcome, error)

// push is for a push to the default branch, which binds main's scope, or to
// a long-lived branch the default branch's .github/runners.json lists,
// which binds that branch's: by its token's signed ref and ref type, whose
// head commit GitHub shows is on that branch.
func (b *Binder) push(ctx context.Context, c *auth.ActionsClaims, _ Request, repo github.Repository, run github.WorkflowRun) (outcome, error) {
	branch, ok := strings.CutPrefix(c.Ref, "refs/heads/")
	if !ok || c.RefType != "branch" || branch == "" {
		return read("push to a ref other than a branch"), nil
	}
	granted, why := scope.Main, "push to the default branch"
	if branch != repo.DefaultBranch {
		listed, err := b.Runners(ctx, repo)
		if err != nil {
			return read("reading " + RunnersFile), err
		}
		name, ok := listed.Branches[branch]
		if !ok {
			return read("push to a branch that isn't the default branch or a listed long-lived branch"), nil
		}
		granted, why = scope.Branch(name), "push to the long-lived branch "+branch
	}
	if c.SHA == "" || c.SHA != run.HeadSHA {
		return read("the token's commit isn't the run's"), nil
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
	listed, err := b.Runners(ctx, repo)
	if err != nil {
		if github.IsUnavailable(err) {
			return scope.Scope{}, err
		}
		return scope.Main, nil
	}
	if name, ok := listed.Branches[branch]; ok {
		return scope.Branch(name), nil
	}
	return scope.Main, nil
}

var pullRef = regexp.MustCompile(`^refs/pull/([0-9]+)/merge$`)

// pullRequest is for a pull_request run, by the pull request its token's
// signed ref names. Nothing the job says is used: a same-repository pull
// request's author controls its workflow.
func (b *Binder) pullRequest(ctx context.Context, c *auth.ActionsClaims, _ Request, repo github.Repository, run github.WorkflowRun) (outcome, error) {
	m := pullRef.FindStringSubmatch(c.Ref)
	if m == nil {
		return read("pull_request token's ref names no pull request"), nil
	}
	n, ok := scope.ParseNumber(m[1])
	if !ok {
		return read("pull_request token's ref names no pull request"), nil
	}
	if run.HeadRepositoryID != repo.ID {
		// A fork's pull request is never one of the repository's own
		// (specs/0002-machines.md, the scope table).
		return read("the run's head is in another repository"), nil
	}
	pr, err := b.GitHub.PullRequest(ctx, repo, n)
	if err != nil {
		return read("reading pull request " + strconv.Itoa(n)), err
	}
	if pr.BaseRepositoryID != repo.ID {
		return read("the pull request doesn't target this repository"), nil
	}
	if pr.HeadRepositoryID != run.HeadRepositoryID || pr.HeadSHA != run.HeadSHA {
		return read("the pull request's head isn't the run's"), nil
	}
	base, err := b.baseOf(ctx, repo, pr.BaseRef)
	if err != nil {
		return read("reading " + RunnersFile), err
	}
	return outcome{scope: scope.PullRequest(n), base: base, head: head{repositoryID: run.HeadRepositoryID, sha: run.HeadSHA},
		reason: "pull_request run of its pull request"}, nil
}

// ErrRunnersFile means .github/runners.json isn't a JSON object, which
// lists nothing.
var ErrRunnersFile = errors.New(RunnersFile + " isn't a JSON object")

// Runners is what the service reads of the default branch's
// .github/runners.json (specs/0005-interface.md, A consumer's files): its
// bootstrap workflow, and its long-lived branches.
type Runners struct {
	Bootstrap string
	// Branches maps each listed long-lived branch to its scope's name,
	// without the entries the service ignores.
	Branches map[string]string
}

// ParseRunners reads .github/runners.json as the service does. It ignores
// a long-lived branch's entry for the default branch, one whose name isn't
// valid, and every entry whose name another entry shares, and returns why
// it ignored each; a bootstrap that isn't a string is none. Only a file
// that isn't a JSON object is an error.
func ParseRunners(data []byte, defaultBranch string) (Runners, []string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return Runners{}, nil, ErrRunnersFile
	}
	f := Runners{Branches: map[string]string{}}
	var problems []string
	if raw, ok := m["bootstrap"]; ok {
		if err := json.Unmarshal(raw, &f.Bootstrap); err != nil {
			f.Bootstrap = ""
			problems = append(problems, "bootstrap isn't a workflow reference")
		}
	}
	raw, ok := m["branches"]
	if !ok {
		return f, problems, nil
	}
	var listed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &listed); err != nil || listed == nil {
		return f, append(problems, "branches isn't an object"), nil
	}
	names := map[string][]string{}
	for branch, v := range listed {
		var name string
		switch {
		case json.Unmarshal(v, &name) != nil:
			problems = append(problems, fmt.Sprintf("branch %q: its scope's name isn't a string", branch))
		case branch == defaultBranch:
			problems = append(problems, fmt.Sprintf("branch %q: the default branch's scope is main's", branch))
		case branch == "":
			problems = append(problems, "an entry names no branch")
		case !scope.ValidBranchName(name):
			problems = append(problems, fmt.Sprintf("branch %q: %q isn't 1 to 16 lowercase letters and digits, starting with a letter", branch, name))
		default:
			names[name] = append(names[name], branch)
		}
	}
	for name, branches := range names {
		if len(branches) > 1 {
			slices.Sort(branches)
			problems = append(problems, fmt.Sprintf("branches %s share the name %q", strings.Join(branches, ", "), name))
			continue
		}
		f.Branches[branches[0]] = name
	}
	slices.Sort(problems)
	return f, problems, nil
}

// Runners reads the default branch's .github/runners.json, never a
// caller's copy, and logs why it ignores any of it. A file that's missing
// is an empty one; one that isn't a JSON object is ErrRunnersFile, with
// what's there treated as empty.
func (b *Binder) Runners(ctx context.Context, repo github.Repository) (Runners, error) {
	return ReadRunners(ctx, b.GitHub, repo, b.log())
}

// ReadRunners is Binder.Runners, for any reader of GitHub.
func ReadRunners(ctx context.Context, gh github.API, repo github.Repository, log *slog.Logger) (Runners, error) {
	branch, err := gh.BranchHead(ctx, repo, repo.DefaultBranch)
	if err != nil {
		return Runners{}, err
	}
	data, err := gh.File(ctx, repo, RunnersFile, branch)
	if errors.Is(err, github.ErrNotFound) {
		return Runners{Branches: map[string]string{}}, nil
	}
	if err != nil {
		return Runners{}, err
	}
	f, problems, err := ParseRunners(data, repo.DefaultBranch)
	if err != nil {
		f.Branches = map[string]string{}
	}
	for _, p := range problems {
		log.Warn("an entry of the default branch's "+RunnersFile+" is ignored", "repository_id", repo.ID, "why", p)
	}
	return f, err
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// pullRequestTarget is for a pull_request_target run, whose token and run
// record name the base branch's commit, and no pull request: only the
// consumer's bootstrap workflow, named in the default branch's
// .github/runners.json, may say which pull request, head repository and
// head commit its event is for, and GitHub must show that pull request
// targets this repository from that head repository, with that commit among
// its commits.
func (b *Binder) pullRequestTarget(ctx context.Context, c *auth.ActionsClaims, req Request, repo github.Repository, _ github.WorkflowRun) (outcome, error) {
	f, err := b.Runners(ctx, repo)
	if err != nil {
		return read("reading " + RunnersFile), err
	}
	if f.Bootstrap == "" {
		return read("no bootstrap workflow: " + RunnersFile + " names none"), nil
	}
	// The bootstrap must be a workflow of this repository's, on its default
	// branch, so that only what's merged there can name a pull request.
	prefix := strings.ToLower(c.Repository + "/.github/workflows/")
	if !strings.HasPrefix(strings.ToLower(f.Bootstrap), prefix) || !strings.HasSuffix(f.Bootstrap, "@refs/heads/"+repo.DefaultBranch) {
		return read("the bootstrap isn't a workflow on this repository's default branch"), nil
	}
	if c.JobWorkflowRef != f.Bootstrap {
		return read("the caller isn't the bootstrap workflow"), nil
	}
	if req.PullRequest <= 0 {
		return read("the bootstrap named no pull request"), nil
	}
	if req.HeadRepositoryID <= 0 || !commitSHA.MatchString(req.HeadSHA) {
		return read("the bootstrap named no head repository and commit"), nil
	}
	pr, err := b.GitHub.PullRequest(ctx, repo, req.PullRequest)
	if err != nil {
		return read("reading pull request " + strconv.Itoa(req.PullRequest)), err
	}
	if pr.BaseRepositoryID != repo.ID {
		return read("the pull request doesn't target this repository"), nil
	}
	if pr.HeadRepositoryID == 0 || pr.HeadRepositoryID != req.HeadRepositoryID {
		return read("the pull request's head repository isn't the event's"), nil
	}
	ok, err := b.isPullCommit(ctx, repo, pr, req.HeadSHA)
	if err != nil {
		return read("checking the pull request's commits"), err
	}
	if !ok {
		return read("the event's head commit isn't one of the pull request's"), nil
	}
	base, err := b.baseOf(ctx, repo, pr.BaseRef)
	if err != nil {
		return read("reading " + RunnersFile), err
	}
	return outcome{scope: scope.PullRequest(req.PullRequest), base: base, head: head{repositoryID: req.HeadRepositoryID, sha: req.HeadSHA},
		reason: "pull_request_target run, named by the bootstrap workflow"}, nil
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
