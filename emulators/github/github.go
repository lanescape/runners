// Package github emulates the parts of GitHub the service uses: the REST
// API as go-github calls it, authenticated as a GitHub App installation; an
// Actions OIDC issuer; and signed webhook deliveries. It keeps repositories,
// commits, branches, tags, pull requests, workflow runs and their jobs,
// environments' approvals, runner groups and just-in-time runners, and hands
// queued jobs to registered runners as GitHub does. A runner stand-in
// (StandIn) plays a runner's part. It fails as GitHub does, with rate limits
// and server errors.
//
// Commits live in one store shared by every repository, as a fork network
// shares its objects, so a fork's commits can be named in its parent.
package github

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/emulators/oidc"
)

// Emulator is a running GitHub emulator.
type Emulator struct {
	// URL is the REST API's base address, with no trailing slash.
	URL string
	// Actions is the Actions OIDC issuer.
	Actions *oidc.Issuer

	srv    *httptest.Server
	faults fault.Injector
	clock  clockwork.Clock

	mu            sync.Mutex
	accounts      map[int64]*Account
	apps          map[int64]*App
	installations map[int64]*Installation
	tokens        map[string]installationToken
	repos         map[int64]*Repo
	commits       map[string]*Commit
	runs          map[int64]*Run
	nextID        int64

	jobs         map[int64]*WorkflowJob
	groups       map[int64]*RunnerGroup
	runners      map[int64]*Runner
	forkApproval map[int64]string
	protected    map[string]bool
	changed      chan struct{}
}

// Account is a user or organization.
type Account struct {
	ID    int64
	Login string
	Type  string // "Organization" or "User"
}

// Repo is a repository.
type Repo struct {
	ID            int64
	Owner         *Account
	Name          string
	DefaultBranch string
	Private       bool
	Parent        *Repo // set for a fork

	emu      *Emulator
	branches map[string]string
	tags     map[string]string
	pulls    map[int]*Pull
	nextPull int
}

// FullName is owner/name.
func (r *Repo) FullName() string { return r.Owner.Login + "/" + r.Name }

// Transfer moves a repository to another owner, keeping its ID.
func (e *Emulator) Transfer(r *Repo, to *Account) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r.Owner = to
}

// Commit is a commit and the whole tree it records, as path to contents.
type Commit struct {
	SHA     string
	Parents []string
	Files   map[string]string
	Message string
	Time    time.Time
}

// Pull is a pull request.
type Pull struct {
	Number   int
	State    string // "open" or "closed"
	Base     *Repo
	BaseRef  string
	Head     *Repo // nil once the head repository is deleted
	HeadRef  string
	HeadSHA  string
	Author   *Account
	Created  time.Time
	Closed   time.Time
	mergeSHA string
}

// Run is a workflow run, with what its jobs' environment and tokens carry.
type Run struct {
	ID         int64
	Repo       *Repo
	Event      string
	Status     string // queued, in_progress, completed, ...
	Conclusion string
	HeadBranch string
	HeadSHA    string
	HeadRepo   *Repo
	Path       string // the workflow file, such as .github/workflows/ci.yml
	Attempt    int
	Number     int
	Actor      *Account
	Pulls      []*Pull

	// What GitHub sets as GITHUB_REF and GITHUB_SHA, and signs in tokens.
	Ref     string
	RefType string
	SHA     string
	BaseRef string
	HeadRef string

	Created   time.Time
	Jobs      []*WorkflowJob
	Approvals []Approval

	// Triggers are who triggered each attempt after the first, by number:
	// a maintainer who reran it, or an App's bot.
	Triggers map[int]*Account

	// EventPull is what the event's payload says of its pull request, as
	// github.event.pull_request carries it, for pull_request and
	// pull_request_target runs: fixed when the run is made.
	EventPull *EventPull
}

// EventPull is a run's event's pull request.
type EventPull struct {
	Number           int
	HeadRepositoryID int64
	HeadSHA          string
}

// trigger is who triggered an attempt of a run: the run's actor for the
// first, unless a rerun says otherwise.
func (r *Run) trigger(attempt int) *Account {
	if a := r.Triggers[attempt]; a != nil {
		return a
	}
	return r.Actor
}

// Option configures an Emulator.
type Option func(*Emulator)

// WithClock sets the emulator's clock, for tokens and timestamps.
func WithClock(c clockwork.Clock) Option { return func(e *Emulator) { e.clock = c } }

// New starts an emulator. Close it when done.
func New(opts ...Option) *Emulator {
	e := &Emulator{
		clock:         clockwork.NewRealClock(),
		accounts:      map[int64]*Account{},
		apps:          map[int64]*App{},
		installations: map[int64]*Installation{},
		tokens:        map[string]installationToken{},
		repos:         map[int64]*Repo{},
		commits:       map[string]*Commit{},
		runs:          map[int64]*Run{},
		nextID:        1_000_000,
	}
	e.initActions()
	for _, o := range opts {
		o(e)
	}
	e.Actions = oidc.New(oidc.WithJWKSPath("/.well-known/jwks"), oidc.WithClock(e.clock))
	e.srv = httptest.NewServer(e.faults.Wrap(e.routes()))
	e.URL = e.srv.URL
	return e
}

// Close stops the emulator.
func (e *Emulator) Close() {
	e.srv.Close()
	e.Actions.Close()
}

// Faults is where tests make requests fail in other ways, such as slowly.
func (e *Emulator) Faults() *fault.Injector { return &e.faults }

func (e *Emulator) id() int64 {
	e.nextID++
	return e.nextID
}

// Org adds an organization, with its Default runner group.
func (e *Emulator) Org(id int64, login string) *Account {
	a := e.account(id, login, "Organization")
	e.mu.Lock()
	defer e.mu.Unlock()
	e.defaultGroup(a)
	return a
}

// User adds a user.
func (e *Emulator) User(id int64, login string) *Account {
	return e.account(id, login, "User")
}

func (e *Emulator) account(id int64, login, typ string) *Account {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := &Account{ID: id, Login: login, Type: typ}
	e.accounts[id] = a
	return a
}

// Rename changes an account's login, as GitHub allows; its ID stays.
func (e *Emulator) Rename(a *Account, login string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a.Login = login
}

func newSHA(seed string) string {
	var salt [16]byte
	_, _ = rand.Read(salt[:])
	sum := sha1.Sum(append(salt[:], seed...))
	return hex.EncodeToString(sum[:])
}

// Repo adds a repository whose default branch, main, has one commit.
func (e *Emulator) Repo(owner *Account, id int64, name string) *Repo {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := &Repo{ID: id, Owner: owner, Name: name, DefaultBranch: "main", emu: e,
		branches: map[string]string{}, tags: map[string]string{}, pulls: map[int]*Pull{}}
	c := &Commit{SHA: newSHA(name), Files: map[string]string{"README.md": name}, Message: "Initial commit", Time: e.clock.Now()}
	e.commits[c.SHA] = c
	r.branches["main"] = c.SHA
	e.repos[id] = r
	return r
}

// Fork makes owner's fork of r, with r's branches.
func (e *Emulator) Fork(r *Repo, owner *Account, id int64) *Repo {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := &Repo{ID: id, Owner: owner, Name: r.Name, DefaultBranch: r.DefaultBranch, Parent: r, emu: e,
		branches: maps.Clone(r.branches), tags: map[string]string{}, pulls: map[int]*Pull{}}
	e.repos[id] = f
	return f
}

// Commit adds a commit on top of branch, changing files (an empty value
// deletes one), creating the branch from the default branch if it doesn't
// exist. It returns the new commit's SHA.
func (r *Repo) Commit(branch string, files map[string]string) string {
	e := r.emu
	e.mu.Lock()
	defer e.mu.Unlock()
	parent, ok := r.branches[branch]
	if !ok {
		parent = r.branches[r.DefaultBranch]
	}
	tree := maps.Clone(e.commits[parent].Files)
	for p, v := range files {
		if v == "" {
			delete(tree, p)
		} else {
			tree[p] = v
		}
	}
	c := &Commit{SHA: newSHA(branch), Parents: []string{parent}, Files: tree, Message: "Change " + branch, Time: e.clock.Now()}
	e.commits[c.SHA] = c
	r.branches[branch] = c.SHA
	return c.SHA
}

// Branch points a branch at a commit.
func (r *Repo) Branch(name, sha string) {
	r.emu.mu.Lock()
	defer r.emu.mu.Unlock()
	r.branches[name] = sha
}

// DeleteBranch deletes a branch.
func (r *Repo) DeleteBranch(name string) {
	r.emu.mu.Lock()
	defer r.emu.mu.Unlock()
	delete(r.branches, name)
}

// Tag points a tag at a commit.
func (r *Repo) Tag(name, sha string) {
	r.emu.mu.Lock()
	defer r.emu.mu.Unlock()
	r.tags[name] = sha
}

// Head is a branch's commit.
func (r *Repo) Head(branch string) string {
	r.emu.mu.Lock()
	defer r.emu.mu.Unlock()
	return r.branches[branch]
}

// PullRequest opens a pull request into base's baseRef from head's headRef.
func (e *Emulator) PullRequest(base *Repo, baseRef string, head *Repo, headRef string, author *Account) *Pull {
	e.mu.Lock()
	defer e.mu.Unlock()
	base.nextPull++
	p := &Pull{Number: base.nextPull, State: "open", Base: base, BaseRef: baseRef, Head: head, HeadRef: headRef,
		HeadSHA: head.branches[headRef], Author: author, Created: e.clock.Now()}
	if p.HeadSHA == "" {
		panic(fmt.Sprintf("github emulator: %s has no branch %s", head.FullName(), headRef))
	}
	base.pulls[p.Number] = p
	return p
}

// Sync moves a pull request's head to its head branch's current commit, as a
// push to that branch does.
func (e *Emulator) Sync(p *Pull) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p.HeadSHA = p.Head.branches[p.HeadRef]
	p.mergeSHA = ""
}

// PullHead is a pull request's head commit now.
func (e *Emulator) PullHead(p *Pull) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return p.HeadSHA
}

// ClosePull closes a pull request.
func (e *Emulator) ClosePull(p *Pull) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p.State, p.Closed = "closed", e.clock.Now()
}

// ReopenPull reopens a closed pull request.
func (e *Emulator) ReopenPull(p *Pull) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p.State, p.Closed = "open", time.Time{}
}

// mergeCommit is the commit GitHub makes at refs/pull/<n>/merge.
func (e *Emulator) mergeCommit(p *Pull) string {
	if p.mergeSHA == "" {
		base := p.Base.branches[p.BaseRef]
		c := &Commit{SHA: newSHA("merge"), Parents: []string{base, p.HeadSHA},
			Files: maps.Clone(e.commits[p.HeadSHA].Files), Message: fmt.Sprintf("Merge %s into %s", p.HeadSHA, base), Time: e.clock.Now()}
		e.commits[c.SHA] = c
		p.mergeSHA = c.SHA
	}
	return p.mergeSHA
}

func (e *Emulator) addRun(r *Run) *Run {
	r.ID = e.id()
	r.Created = e.clock.Now()
	if r.Status == "" {
		r.Status = "in_progress"
	}
	if r.Attempt == 0 {
		r.Attempt = 1
	}
	if r.Path == "" {
		r.Path = ".github/workflows/ci.yml"
	}
	r.Number = int(r.ID % 10000)
	if r.Actor == nil {
		r.Actor = r.Repo.Owner
	}
	if r.HeadRepo == nil {
		r.HeadRepo = r.Repo
	}
	e.runs[r.ID] = r
	return r
}

// PushRun is the run of a push to branch, at its current commit.
func (e *Emulator) PushRun(r *Repo, branch string) *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	sha := r.branches[branch]
	return e.addRun(&Run{Repo: r, Event: "push", HeadBranch: branch, HeadSHA: sha,
		Ref: "refs/heads/" + branch, RefType: "branch", SHA: sha})
}

// TagRun is the run of a push of tag.
func (e *Emulator) TagRun(r *Repo, tag string) *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	sha := r.tags[tag]
	return e.addRun(&Run{Repo: r, Event: "push", HeadBranch: tag, HeadSHA: sha,
		Ref: "refs/tags/" + tag, RefType: "tag", SHA: sha})
}

// PullRequestRun is a pull_request run for p's current head. GitHub lists
// the run's pull requests only when the head is in the same repository.
func (e *Emulator) PullRequestRun(p *Pull) *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := &Run{Repo: p.Base, Event: "pull_request", HeadBranch: p.HeadRef, HeadSHA: p.HeadSHA, HeadRepo: p.Head,
		Ref: fmt.Sprintf("refs/pull/%d/merge", p.Number), RefType: "branch", SHA: e.mergeCommit(p),
		BaseRef: p.BaseRef, HeadRef: p.HeadRef, Actor: p.Author, EventPull: eventPull(p)}
	if p.Head == p.Base {
		r.Pulls = []*Pull{p}
	}
	return e.addRun(r)
}

func eventPull(p *Pull) *EventPull {
	ep := &EventPull{Number: p.Number, HeadSHA: p.HeadSHA}
	if p.Head != nil {
		ep.HeadRepositoryID = p.Head.ID
	}
	return ep
}

// PullRequestTargetRun is a pull_request_target run for p's current head,
// which runs the default branch's workflow at the default branch's commit.
// As GitHub records it, the run's head is the base branch's: its head_sha
// and head_repository are the base's, as GITHUB_SHA is, and only the event's
// payload names the pull request's head.
func (e *Emulator) PullRequestTargetRun(p *Pull) *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	def := p.Base.DefaultBranch
	sha := p.Base.branches[def]
	r := &Run{Repo: p.Base, Event: "pull_request_target", HeadBranch: def, HeadSHA: sha, HeadRepo: p.Base,
		Ref: "refs/heads/" + def, RefType: "branch", SHA: sha,
		BaseRef: p.BaseRef, HeadRef: p.HeadRef, Actor: p.Author, Path: ".github/workflows/contribution.yml",
		EventPull: eventPull(p)}
	if p.Head == p.Base {
		r.Pulls = []*Pull{p}
	}
	return e.addRun(r)
}

// EventRun is a run of any other event on branch, such as
// workflow_dispatch.
func (e *Emulator) EventRun(r *Repo, event, branch string) *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	sha := r.branches[branch]
	return e.addRun(&Run{Repo: r, Event: event, HeadBranch: branch, HeadSHA: sha,
		Ref: "refs/heads/" + branch, RefType: "branch", SHA: sha})
}

// SetRunStatus changes a run's status, such as to completed.
func (e *Emulator) SetRunStatus(r *Run, status string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r.Status = status
	e.notify()
}

// Job is what distinguishes one job's token from another's in a run.
type Job struct {
	// WorkflowRef is the job's job_workflow_ref; by default the run's own
	// workflow at the run's ref. A job of a reusable workflow has that
	// workflow's.
	WorkflowRef string
	// WorkflowSHA is the job_workflow_sha of a reusable workflow's job; by
	// default the run's commit, as for a workflow in the same repository
	// at the run's ref.
	WorkflowSHA string
	Audience    string
	Environment string
	// CheckRunID is the job's check run, which GitHub signs as
	// check_run_id: its workflow job's ID. Zero leaves the claim out.
	CheckRunID int64
}

// ActionsClaims are the claims GitHub signs in a job's OIDC token, which
// tests may change before minting.
func (e *Emulator) ActionsClaims(r *Run, j Job) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	workflowRef := r.Repo.FullName() + "/" + r.Path + "@" + r.Ref
	jobRef, jobSHA := j.WorkflowRef, j.WorkflowSHA
	if jobRef == "" {
		jobRef = workflowRef
	}
	if jobSHA == "" {
		jobSHA = r.SHA
	}
	aud := j.Audience
	if aud == "" {
		aud = "https://github.com/" + r.Repo.Owner.Login
	}
	sub := "repo:" + r.Repo.FullName() + ":ref:" + r.Ref
	switch {
	case j.Environment != "":
		sub = "repo:" + r.Repo.FullName() + ":environment:" + j.Environment
	case r.Event == "pull_request" || r.Event == "pull_request_target":
		sub = "repo:" + r.Repo.FullName() + ":pull_request"
	}
	vis := "public"
	if r.Repo.Private {
		vis = "private"
	}
	c := map[string]any{
		"sub":                   sub,
		"aud":                   aud,
		"ref":                   r.Ref,
		"sha":                   r.SHA,
		"repository":            r.Repo.FullName(),
		"repository_owner":      r.Repo.Owner.Login,
		"repository_owner_id":   fmt.Sprint(r.Repo.Owner.ID),
		"repository_id":         fmt.Sprint(r.Repo.ID),
		"repository_visibility": vis,
		"run_id":                fmt.Sprint(r.ID),
		"run_number":            fmt.Sprint(r.Number),
		"run_attempt":           fmt.Sprint(r.Attempt),
		"actor":                 r.Actor.Login,
		"actor_id":              fmt.Sprint(r.Actor.ID),
		"workflow":              strings.TrimSuffix(r.Path[strings.LastIndex(r.Path, "/")+1:], ".yml"),
		"workflow_ref":          workflowRef,
		"workflow_sha":          r.SHA,
		"job_workflow_ref":      jobRef,
		"job_workflow_sha":      jobSHA,
		"event_name":            r.Event,
		"ref_type":              r.RefType,
		"ref_protected":         "false",
		"head_ref":              r.HeadRef,
		"base_ref":              r.BaseRef,
		"runner_environment":    "github-hosted",
	}
	if j.Environment != "" {
		c["environment"] = j.Environment
	}
	if j.CheckRunID != 0 {
		c["check_run_id"] = fmt.Sprint(j.CheckRunID)
	}
	return c
}

// ActionsToken mints a job's OIDC token, with claims changed by overrides;
// an override of nil removes a claim.
func (e *Emulator) ActionsToken(r *Run, j Job, overrides map[string]any) string {
	c := e.ActionsClaims(r, j)
	maps.Copy(c, overrides)
	return e.Actions.Mint(c)
}

// JobToken mints the OIDC token of a workflow job, as GitHub signs it for
// that job: with its check run and its attempt. job says the rest, as for
// ActionsToken.
func (e *Emulator) JobToken(wj *WorkflowJob, job Job, overrides map[string]any) string {
	job.CheckRunID = wj.ID
	c := e.ActionsClaims(wj.Run, job)
	c["run_attempt"] = fmt.Sprint(wj.Attempt)
	maps.Copy(c, overrides)
	return e.Actions.Mint(c)
}

// reachable is every commit reachable from sha, with each one's distance.
func (e *Emulator) reachable(sha string) map[string]int {
	seen := map[string]int{}
	queue := []string{sha}
	seen[sha] = 0
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		c := e.commits[s]
		if c == nil {
			continue
		}
		for _, p := range c.Parents {
			if _, ok := seen[p]; !ok {
				seen[p] = seen[s] + 1
				queue = append(queue, p)
			}
		}
	}
	return seen
}

// between is the commits reachable from head but not base, oldest first.
func (e *Emulator) between(base, head string) []string {
	fromBase := e.reachable(base)
	fromHead := e.reachable(head)
	var out []string
	for s := range fromHead {
		if _, ok := fromBase[s]; !ok {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return fromHead[b] - fromHead[a] })
	return out
}

// isSHA reports whether s is a full commit SHA.
func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// resolve finds what a name means in a repository, as git does: a full SHA,
// a full ref, then a tag, then a branch. A tag named like a branch wins.
func (e *Emulator) resolve(r *Repo, name string) (string, bool) {
	if isSHA(name) {
		_, ok := e.commits[name]
		return name, ok
	}
	if b, ok := strings.CutPrefix(name, "refs/heads/"); ok {
		s, ok := r.branches[b]
		return s, ok
	}
	if t, ok := strings.CutPrefix(name, "refs/tags/"); ok {
		s, ok := r.tags[t]
		return s, ok
	}
	if s, ok := r.tags[name]; ok {
		return s, true
	}
	s, ok := r.branches[name]
	return s, ok
}
