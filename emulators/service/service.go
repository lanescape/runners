// Package service emulates this repository's service as its interface for
// actions describes it (specs/0005-interface.md), and nothing more: run
// binding. It runs in-process, as an httptest server, and verifies tokens
// against an OIDC issuer emulator, as the service verifies GitHub's.
//
// Consumers test their actions against it, at a pinned version of this
// module; the conformance suite in package conformance holds it to the
// real service's answers, in this repository's CI (specs/0004-deployment.md).
// Tests drive it through World: they declare repositories, their
// runners.json and branches, pull requests, runs and jobs, as GitHub would
// show them; finish jobs; rerun runs; make GitHub unavailable; and read
// what was bound. Faults adds any other failure,
// with Refuse and Unavailable answering as the service does. A test that
// runs this module's GitHub emulator hands it over with WithGitHub, and the
// emulator takes its view of GitHub from that one state.
package service

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	emuoidc "github.com/lanescape/runners/emulators/oidc"
	"github.com/lanescape/runners/endpoints"
)

// Emulator is a running emulator of the service.
type Emulator struct {
	url    string
	issuer *emuoidc.Issuer
	ownsIs bool
	srv    *httptest.Server
	faults fault.Injector
	clock  clockwork.Clock
	org    int64
	login  string
	verify *oidc.IDTokenVerifier

	mu          sync.Mutex
	nextID      int64
	repos       map[int64]*repo
	runs        map[int64]*run
	jobs        map[int64]*job
	bindings    map[int64]*binding
	unavailable bool

	// With WithGitHub: the GitHub emulator the emulator reads, and the
	// maintainer who opens pull requests and reruns runs there.
	gh         *ghemu.Emulator
	maintainer *ghemu.Account
}

// Option configures an Emulator.
type Option func(*Emulator)

// WithIssuer verifies tokens against issuer, and mints them with it, in
// place of an issuer of the emulator's own.
func WithIssuer(issuer *emuoidc.Issuer) Option { return func(e *Emulator) { e.issuer = issuer } }

// WithClock sets the emulator's clock, against which tokens' expiry is
// checked.
func WithClock(c clockwork.Clock) Option { return func(e *Emulator) { e.clock = c } }

// WithOrganization sets the organization the emulator serves, by ID and
// login; endpoints.Organization, lanescape, by default.
func WithOrganization(id int64, login string) Option {
	return func(e *Emulator) { e.org, e.login = id, login }
}

// New starts an emulator. Close it when done.
func New(opts ...Option) *Emulator {
	e := &Emulator{clock: clockwork.NewRealClock(), org: endpoints.Organization, login: "lanescape", nextID: 1_000_000,
		repos: map[int64]*repo{}, runs: map[int64]*run{}, jobs: map[int64]*job{}, bindings: map[int64]*binding{}}
	for _, o := range opts {
		o(e)
	}
	if e.gh != nil {
		if e.issuer != nil && e.issuer != e.gh.Actions {
			panic("service emulator: WithGitHub verifies the GitHub emulator's own tokens, not another issuer's")
		}
		e.issuer = e.gh.Actions
	}
	if e.issuer == nil {
		e.issuer = emuoidc.New(emuoidc.WithClock(e.clock))
		e.ownsIs = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/bind", e.bind)
	e.srv = httptest.NewServer(e.faults.Wrap(mux))
	e.url = e.srv.URL
	keys := oidc.NewRemoteKeySet(context.Background(), e.issuer.JWKSURL)
	e.verify = oidc.NewVerifier(e.issuer.URL, keys, &oidc.Config{ClientID: e.url, SupportedSigningAlgs: []string{oidc.RS256},
		Now: e.clock.Now})
	return e
}

// Close stops the emulator, and its issuer if it's its own.
func (e *Emulator) Close() {
	e.srv.Close()
	if e.ownsIs {
		e.issuer.Close()
	}
}

// URL implements World.
func (e *Emulator) URL() string { return e.url }

// Organization implements World.
func (e *Emulator) Organization() int64 { return e.org }

// Issuer is the issuer whose tokens the emulator accepts.
func (e *Emulator) Issuer() *emuoidc.Issuer { return e.issuer }

// Faults is where tests make requests fail in other ways, such as answering
// with Refuse or Unavailable, slowly, or by dropping the connection.
func (e *Emulator) Faults() *fault.Injector { return &e.faults }

// Refuse answers as the service does when it won't do something: 403, with
// the reason, which the caller doesn't retry.
func Refuse(reason string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": http.StatusText(http.StatusForbidden), "reason": reason})
	}
}

// Unavailable answers as the service does when it can't confirm an answer:
// 503, retryable, with a Retry-After of retryAfter, at least a second.
func Unavailable(retryAfter time.Duration) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", fmt.Sprint(max(int(retryAfter/time.Second), 1)))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the service couldn't confirm an answer; try again", "retryable": true})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (e *Emulator) id() int64 {
	e.nextID++
	return e.nextID
}

func newSHA(seed string, n int64) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%s/%d/%d", seed, n, time.Now().UnixNano())))
	return hex.EncodeToString(sum[:])
}

// repo is a repository as the emulator models GitHub's: its branches'
// commits, its default branch's runners.json, and its pull requests.
type repo struct {
	id   int64
	name string
	// def is the default branch: main.
	def      string
	branches map[string]string
	// history is each branch's commits: what's reachable from its tip.
	history  map[string]map[string]bool
	runners  string
	pulls    map[int]*pull
	nextPull int
}

type pull struct {
	number   int
	base     string
	headRepo int64
	headSHA  string
	open     bool
	// branch is the head branch of a pull request from the repository
	// itself; empty for a fork's.
	branch string
	// commits are the pull request's commits: every head it has had.
	commits map[string]bool
}

type run struct {
	id       int64
	repo     *repo
	event    string
	ref      string
	refType  string
	sha      string
	headRepo int64
	// headSHA is a pull_request run's head commit, as GitHub records it
	// when the run is made.
	headSHA string
	attempt int
	pull    *pull
	// status is GitHub's status of the run, with a GitHub emulator; with
	// none, the run's jobs say whether it's in progress.
	status string
}

type job struct {
	id          int64
	run         *run
	attempt     int
	workflowRef string
	status      string
}

func (e *Emulator) repo(r Repository) *repo {
	rp := e.repos[r.ID]
	if rp == nil {
		panic(fmt.Sprintf("service emulator: no repository %d", r.ID))
	}
	return rp
}

// Repository implements World.
func (e *Emulator) Repository(name string) Repository {
	if e.gh != nil {
		return e.ghRepository(name)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	id := e.id()
	full := e.login + "/" + name
	rp := &repo{id: id, name: full, def: "main", branches: map[string]string{}, history: map[string]map[string]bool{}, pulls: map[int]*pull{}}
	rp.commit("main", newSHA(full, id))
	e.repos[id] = rp
	return Repository{ID: id, Name: full}
}

// commit adds a commit on top of a branch, making the branch from the
// default branch if it doesn't exist.
func (rp *repo) commit(branch, sha string) {
	if _, ok := rp.branches[branch]; !ok {
		rp.history[branch] = map[string]bool{}
		for c := range rp.history["main"] {
			rp.history[branch][c] = true
		}
	}
	rp.branches[branch] = sha
	rp.history[branch][sha] = true
}

// onBranch reports whether a commit is reachable from a branch's tip.
func (rp *repo) onBranch(branch, sha string) bool { return rp.history[branch][sha] }

// SetRunners implements World.
func (e *Emulator) SetRunners(r Repository, file string) {
	if e.gh != nil {
		e.GitHubRepo(r).Commit(e.GitHubRepo(r).DefaultBranch, map[string]string{runnersFile: file})
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp := e.repo(r)
	rp.runners = file
	rp.commit("main", newSHA(rp.name, e.id()))
}

// Bootstrap implements World.
func (e *Emulator) Bootstrap(r Repository) string {
	return r.Name + "/.github/workflows/bootstrap.yml@refs/heads/main"
}

// Branch implements World.
func (e *Emulator) Branch(r Repository, name string) {
	if e.gh != nil {
		e.GitHubRepo(r).Commit(name, map[string]string{"branch": name})
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp := e.repo(r)
	rp.commit(name, newSHA(rp.name+"/"+name, e.id()))
}

// DeleteBranch implements World.
func (e *Emulator) DeleteBranch(r Repository, name string) {
	if e.gh != nil {
		e.GitHubRepo(r).DeleteBranch(name)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp := e.repo(r)
	delete(rp.branches, name)
	delete(rp.history, name)
}

// PullRequest implements World.
func (e *Emulator) PullRequest(r Repository, base, head string) PullRequest {
	if e.gh != nil {
		return e.ghPullRequest(r, base, head)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp := e.repo(r)
	rp.commit(head, newSHA(rp.name+"/"+head, e.id()))
	pr := e.openPull(rp, base, rp.id, rp.branches[head])
	rp.pulls[pr.Number].branch = head
	return pr
}

// ForkPullRequest implements World.
func (e *Emulator) ForkPullRequest(r Repository, base string) PullRequest {
	if e.gh != nil {
		return e.ghForkPullRequest(r, base)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp := e.repo(r)
	fork := e.id()
	return e.openPull(rp, base, fork, newSHA("fork", fork))
}

func (e *Emulator) openPull(rp *repo, base string, headRepo int64, sha string) PullRequest {
	rp.nextPull++
	p := &pull{number: rp.nextPull, base: base, headRepo: headRepo, headSHA: sha, open: true, commits: map[string]bool{sha: true}}
	rp.pulls[p.number] = p
	return PullRequest{Repository: Repository{ID: rp.id, Name: rp.name}, Number: p.number, HeadRepositoryID: headRepo, HeadSHA: sha}
}

// pullCommit reports whether a commit is one of a pull request's, as
// GitHub counts them: every head it has had, and, for one from a branch of
// the repository, each commit its head branch has that its base doesn't.
func (rp *repo) pullCommit(p *pull, sha string) bool {
	if p.commits[sha] {
		return true
	}
	return p.branch != "" && rp.onBranch(p.branch, sha) && !rp.onBranch(p.base, sha)
}

// PushPullRequest implements World.
func (e *Emulator) PushPullRequest(pr PullRequest) PullRequest {
	if e.gh != nil {
		return e.ghPushPullRequest(pr)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp, p := e.repo(pr.Repository), e.pull(pr)
	if !p.open {
		panic(fmt.Sprintf("service emulator: pull request %d isn't open", p.number))
	}
	sha := newSHA("push", e.id())
	if p.branch != "" {
		rp.commit(p.branch, sha)
	}
	p.headSHA, p.commits[sha] = sha, true
	pr.HeadSHA = sha
	return pr
}

func (e *Emulator) pull(pr PullRequest) *pull {
	p := e.repo(pr.Repository).pulls[pr.Number]
	if p == nil {
		panic(fmt.Sprintf("service emulator: no pull request %d", pr.Number))
	}
	return p
}

func (e *Emulator) addRun(rp *repo, r *run) Run {
	r.id, r.repo, r.attempt = e.id(), rp, 1
	if r.headRepo == 0 {
		r.headRepo = rp.id
	}
	e.runs[r.id] = r
	out := Run{Repository: Repository{ID: rp.id, Name: rp.name}, ID: r.id, Attempt: 1, Event: r.event}
	if r.pull != nil {
		out.PullRequest = &PullRequest{Repository: out.Repository, Number: r.pull.number, HeadRepositoryID: r.pull.headRepo, HeadSHA: r.pull.headSHA}
	}
	return out
}

// Push implements World.
func (e *Emulator) Push(r Repository, branch string) Run {
	if e.gh != nil {
		return e.RunOf(e.gh.PushRun(e.GitHubRepo(r), branch))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp := e.repo(r)
	sha, ok := rp.branches[branch]
	if !ok {
		panic("service emulator: no branch " + branch)
	}
	return e.addRun(rp, &run{event: "push", ref: "refs/heads/" + branch, refType: "branch", sha: sha})
}

// Tag implements World.
func (e *Emulator) Tag(r Repository, tag string) Run {
	if e.gh != nil {
		return e.ghTag(r, tag)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp := e.repo(r)
	return e.addRun(rp, &run{event: "push", ref: "refs/tags/" + tag, refType: "tag", sha: rp.branches["main"]})
}

// Event implements World.
func (e *Emulator) Event(r Repository, event, branch string) Run {
	if e.gh != nil {
		return e.RunOf(e.gh.EventRun(e.GitHubRepo(r), event, branch))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp := e.repo(r)
	return e.addRun(rp, &run{event: event, ref: "refs/heads/" + branch, refType: "branch", sha: rp.branches[branch]})
}

// PullRequestRun implements World.
func (e *Emulator) PullRequestRun(pr PullRequest) Run {
	if e.gh != nil {
		return e.RunOf(e.gh.PullRequestRun(e.GitHubPull(pr)))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp, p := e.repo(pr.Repository), e.pull(pr)
	return e.addRun(rp, &run{event: "pull_request", ref: fmt.Sprintf("refs/pull/%d/merge", p.number), refType: "branch",
		sha: newSHA("merge", e.id()), headRepo: p.headRepo, headSHA: p.headSHA, pull: p})
}

// PullRequestTargetRun implements World. As GitHub records it, the run's
// head is the base's; only its event names the pull request's.
func (e *Emulator) PullRequestTargetRun(pr PullRequest) Run {
	if e.gh != nil {
		return e.RunOf(e.gh.PullRequestTargetRun(e.GitHubPull(pr)))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rp, p := e.repo(pr.Repository), e.pull(pr)
	return e.addRun(rp, &run{event: "pull_request_target", ref: "refs/heads/main", refType: "branch", sha: rp.branches["main"], pull: p})
}

// Rerun implements World.
func (e *Emulator) Rerun(r Run) Run {
	if e.gh != nil {
		return e.ghRerun(r)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rn := e.runs[r.ID]
	rn.attempt++
	r.Attempt = rn.attempt
	return r
}

// StartJob implements World.
func (e *Emulator) StartJob(r Run, workflowRef string) Job {
	if e.gh != nil {
		return e.ghStartJob(r, workflowRef)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rn := e.runs[r.ID]
	j := &job{id: e.id(), run: rn, attempt: rn.attempt, workflowRef: workflowRef, status: "in_progress"}
	if j.workflowRef == "" {
		path := ".github/workflows/ci.yml"
		if rn.event == "pull_request_target" {
			path = ".github/workflows/contribution.yml"
		}
		j.workflowRef = rn.repo.name + "/" + path + "@" + rn.ref
	}
	e.jobs[j.id] = j
	r.Attempt = rn.attempt
	return Job{Run: r, ID: j.id, WorkflowRef: j.workflowRef}
}

// Claims are a job's token's claims, as GitHub signs them, for the
// service.
func (e *Emulator) Claims(j Job) map[string]any {
	if e.gh != nil {
		return e.ghClaims(j, nil)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	jb := e.jobs[j.ID]
	rn := jb.run
	return map[string]any{
		"sub":                 "repo:" + rn.repo.name + ":ref:" + rn.ref,
		"aud":                 e.url,
		"ref":                 rn.ref,
		"ref_type":            rn.refType,
		"sha":                 rn.sha,
		"repository":          rn.repo.name,
		"repository_id":       fmt.Sprint(rn.repo.id),
		"repository_owner":    e.login,
		"repository_owner_id": fmt.Sprint(e.org),
		"run_id":              fmt.Sprint(rn.id),
		"run_attempt":         fmt.Sprint(jb.attempt),
		"event_name":          rn.event,
		"job_workflow_ref":    jb.workflowRef,
		"check_run_id":        fmt.Sprint(jb.id),
	}
}

// Token implements World.
func (e *Emulator) Token(j Job, overrides map[string]any) string {
	c := e.Claims(j)
	for k, v := range overrides {
		c[k] = v
	}
	return e.issuer.Mint(c)
}

// FinishJob implements World.
func (e *Emulator) FinishJob(j Job, conclusion string) {
	if e.gh != nil {
		e.gh.CompleteJobByID(j.ID, conclusion)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.jobs[j.ID].status = "completed"
}

// Unavailable implements World.
func (e *Emulator) Unavailable(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.unavailable = on
}

// Binding implements World.
func (e *Emulator) Binding(r Run) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observe()
	b := e.bindings[r.ID]
	if b == nil {
		return "", false
	}
	return b.scope, true
}

var _ World = (*Emulator)(nil)
