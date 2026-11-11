// Package service emulates this repository's service as its interface for
// actions describes it (specs/0005-interface.md), and nothing more: runs
// bound at their first job's check, as the service binds them, and work
// disks and their saves, chosen from the saved disks of the run's restore
// chain. It runs
// in-process, as an httptest server, over a GitHub emulator of this
// module's: its own, or one the test hands it with WithGitHub, so that one
// GitHub state serves the test's jobs and the service emulator, as GitHub
// serves both a job and the service. It reads that state in-process, past
// any faults the test adds to the GitHub emulator's API. It verifies
// machines' identity tokens against an issuer of its own, as the service
// verifies Compute Engine's, and serves each job's machine a metadata
// server that mints them.
//
// What it doesn't model: a machine's age limit, Spot preemption and resume
// images, the deletion of saved disks beyond a scope's one disk of each
// family and architecture and a closed pull request's, such as a family's
// once its declaration is removed, which the service does at its next
// pass, and the mounting of a work disk, which is the test's to arrange.
//
// Consumers test their actions against it, at a pinned version of this
// module; the conformance suite in package conformance holds it to the
// real service's answers, in this repository's CI (specs/0004-deployment.md).
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/emulators/fault"
	ghemu "github.com/lanescape/runners/emulators/github"
	emuoidc "github.com/lanescape/runners/emulators/oidc"
	"github.com/lanescape/runners/endpoints"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/selector"
)

// Project and Account are the project and account the emulator's machines'
// identity tokens name.
const (
	Project = "lanescape-dev"
	Account = "lanescape-runners-machine@lanescape-dev.iam.gserviceaccount.com"
)

// retryAfter is what the emulator's 503s ask a caller to wait, as the
// service's do.
const retryAfter = 5 * time.Second

// Emulator is a running emulator of the service.
type Emulator struct {
	url    string
	issuer *emuoidc.Issuer
	srv    *httptest.Server
	faults fault.Injector
	clock  clockwork.Clock
	org    int64
	login  string
	verify *oidc.IDTokenVerifier
	shapes config.Shapes

	gh         *ghemu.Emulator
	ownsGH     bool
	ghc        *github.Client
	maintainer *ghemu.Account
	objects    *memStore
	binder     *binding.Binder
	files      *machines.Service

	mu          sync.Mutex
	nextID      int64
	unavailable bool
	holdDisks   bool
	machines    map[string]*machine
	jobs        map[int64]*jobState
	pulls       map[string]*ghemu.Pull

	dsk diskState
}

// machine is a job's machine, as the emulator plays it.
type machine struct {
	name     string
	id       uint64
	zone     string
	repo     int64
	run      int64
	job      int64
	selector selector.Selector
	attempt  int
	unbound  bool
	// finished is set once the job's end is reported: the machine's
	// requests are refused from then on.
	finished bool
}

// jobState is a job the emulator played to its check, and the machine it
// got, if any.
type jobState struct {
	wj      *ghemu.WorkflowJob
	machine string
}

// Option configures an Emulator.
type Option func(*Emulator)

// WithGitHub makes gh, a GitHub emulator the test already runs, the
// emulator's view of GitHub: the emulator reads its repositories, files,
// pull requests, runs, jobs and approvals in-process, as they are when each
// request arrives, through a GitHub App it installs on the organization
// there, past any faults the test adds to gh's API. A test may change
// anything in gh directly; World's methods act on gh, and RepositoryOf,
// PullRequestOf, RunOf and JobOf give the handles of what the test made
// there itself. Without it, the emulator starts a GitHub emulator of its
// own.
func WithGitHub(gh *ghemu.Emulator) Option { return func(e *Emulator) { e.gh = gh } }

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
		machines: map[string]*machine{}, jobs: map[int64]*jobState{}, pulls: map[string]*ghemu.Pull{}, objects: newMemStore(),
		dsk: newDiskState()}
	for _, o := range opts {
		o(e)
	}
	cfg, err := config.Default()
	if err != nil {
		panic("service emulator: " + err.Error())
	}
	e.shapes = cfg.Shapes
	if e.gh == nil {
		e.gh = ghemu.New()
		e.ownsGH = true
	}
	org := e.gh.Account(e.org)
	if org == nil {
		org = e.gh.Org(e.org, e.login)
	}
	e.gh.SetForkApproval(org, ghemu.ApproveAllExternalContributors)
	app := e.gh.NewApp(e.gh.NewID())
	e.gh.Install(app, org)
	ghc, err := github.NewAppClient(github.Options{BaseURL: e.gh.URL, AppID: app.ID, PrivateKey: app.PrivateKeyPEM(), OrganizationID: e.org,
		Transport: gitHubTransport{e: e, next: e.gh.Transport()}})
	if err != nil {
		panic("service emulator: " + err.Error())
	}
	e.ghc = ghc
	e.maintainer = e.gh.User(e.gh.NewID(), "maintainer")
	e.binder = &binding.Binder{GitHub: ghc, Objects: e.objects, OrganizationID: e.org, Clock: e.clock}
	e.files = &machines.Service{GitHub: ghc, Objects: e.objects}
	e.issuer = emuoidc.New(emuoidc.WithClock(e.clock))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/disks", e.disk)
	mux.HandleFunc("POST /v1/disks/save", e.save)
	mux.HandleFunc("GET /_metadata/{machine}/computeMetadata/v1/instance/service-accounts/default/identity", e.identity)
	mux.HandleFunc("GET /_metadata/{machine}/computeMetadata/v1/instance/attributes/lsr-service", e.serviceAttribute)
	e.srv = httptest.NewServer(e.faults.Wrap(mux))
	e.url = e.srv.URL
	keys := oidc.NewRemoteKeySet(context.Background(), e.issuer.JWKSURL)
	e.verify = oidc.NewVerifier(e.issuer.URL, keys, &oidc.Config{ClientID: e.url, SupportedSigningAlgs: []string{oidc.RS256},
		Now: e.clock.Now})
	return e
}

// gitHubTransport is the emulator's way to GitHub's state: in-process,
// failing every request while the test makes GitHub unavailable.
type gitHubTransport struct {
	e    *Emulator
	next http.RoundTripper
}

func (t gitHubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.e.mu.Lock()
	down := t.e.unavailable
	t.e.mu.Unlock()
	if !down {
		return t.next.RoundTrip(r)
	}
	body := `{"message": "Service Unavailable"}`
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Proto: "HTTP/1.1", ProtoMajor: 1,
		ProtoMinor: 1, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)), Request: r}, nil
}

// Close stops the emulator, its issuer, and its GitHub emulator if it's
// its own.
func (e *Emulator) Close() {
	e.srv.Close()
	e.issuer.Close()
	if e.ownsGH {
		e.gh.Close()
	}
}

// URL implements World.
func (e *Emulator) URL() string { return e.url }

// Organization implements World.
func (e *Emulator) Organization() int64 { return e.org }

// GitHub is the GitHub emulator the emulator takes its view of GitHub from.
func (e *Emulator) GitHub() *ghemu.Emulator { return e.gh }

// Faults is where tests make requests fail in other ways, such as answering
// with fault.Refuse or fault.Unavailable, slowly, or by dropping the
// connection.
func (e *Emulator) Faults() *fault.Injector { return &e.faults }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (e *Emulator) id() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nextID++
	return e.nextID
}

// Token implements World: the job's machine's identity token, in Compute
// Engine's full format, for the service's audience.
func (e *Emulator) Token(j Job) string { return e.tokenFor(j.Machine, e.url) }

func (e *Emulator) tokenFor(name, audience string) string {
	e.mu.Lock()
	m := e.machines[name]
	e.mu.Unlock()
	if m == nil {
		panic("service emulator: no machine " + name)
	}
	return e.issuer.Mint(map[string]any{"aud": audience, "sub": "machine", "email": Account, "email_verified": true,
		"google": map[string]any{"compute_engine": map[string]any{"project_id": Project, "zone": m.zone,
			"instance_id": strconv.FormatUint(m.id, 10), "instance_name": m.name}}})
}

// Metadata is the address of a job's machine's metadata server, as Compute
// Engine's is at http://metadata.google.internal: it mints the machine's
// identity token, and answers the attribute lsr-service with the service's
// address (specs/0005-interface.md, Where the service is).
func (e *Emulator) Metadata(j Job) string { return e.url + "/_metadata/" + j.Machine }

// metadata answers a metadata server's request for a machine, as Compute
// Engine's does, and reports whether the machine is one of the emulator's.
func (e *Emulator) metadata(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Header.Get("Metadata-Flavor") != "Google" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "Missing Metadata-Flavor:Google header."})
		return "", false
	}
	name := r.PathValue("machine")
	e.mu.Lock()
	_, ok := e.machines[name]
	e.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
	}
	return name, ok
}

func (e *Emulator) identity(w http.ResponseWriter, r *http.Request) {
	name, ok := e.metadata(w, r)
	if !ok {
		return
	}
	aud := r.URL.Query().Get("audience")
	if aud == "" || r.URL.Query().Get("format") != "full" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(e.tokenFor(name, aud)))
}

func (e *Emulator) serviceAttribute(w http.ResponseWriter, r *http.Request) {
	if _, ok := e.metadata(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(e.url))
}

// caller verifies a machine's identity token and finds its machine.
func (e *Emulator) caller(w http.ResponseWriter, r *http.Request) (*machine, bool) {
	auth := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || tok == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": http.StatusText(http.StatusUnauthorized)})
		return nil, false
	}
	id, err := e.verify.Verify(r.Context(), tok)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": http.StatusText(http.StatusUnauthorized)})
		return nil, false
	}
	var c struct {
		Email  string `json:"email"`
		Google struct {
			ComputeEngine struct {
				ProjectID    string `json:"project_id"`
				InstanceID   string `json:"instance_id"`
				InstanceName string `json:"instance_name"`
			} `json:"compute_engine"`
		} `json:"google"`
	}
	if err := id.Claims(&c); err != nil || c.Email != Account || c.Google.ComputeEngine.ProjectID != Project {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": http.StatusText(http.StatusUnauthorized)})
		return nil, false
	}
	e.mu.Lock()
	m := e.machines[c.Google.ComputeEngine.InstanceName]
	e.mu.Unlock()
	if m == nil || strconv.FormatUint(m.id, 10) != c.Google.ComputeEngine.InstanceID {
		fault.Refuse("the machine isn't one the service made")(w, r)
		return nil, false
	}
	return m, true
}

// request is a running job's request, as the service checks it.
type request struct {
	m    *machine
	repo github.Repository
	bd   binding.Binding
}

// active checks a request from a job and reads its body, as the service
// does (specs/0005-interface.md, Requests from a job): it answers only a
// machine it made whose job it admitted, while that job runs, for that job
// and its run, once the run is bound, of a repository that's still the
// organization's. It answers any other request itself, and reports whether
// the caller should go on.
func (e *Emulator) active(w http.ResponseWriter, r *http.Request, body any) (request, bool) {
	m, ok := e.caller(w, r)
	if !ok {
		return request{}, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<10+1))
	if err != nil || len(data) > 1<<10 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body unreadable or too large"})
		return request{}, false
	}
	if len(bytes.TrimSpace(data)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body isn't a request"})
			return request{}, false
		}
	}
	e.mu.Lock()
	finished, unbound := m.finished, m.unbound
	e.mu.Unlock()
	if finished {
		fault.Refuse("this machine's job has ended")(w, r)
		return request{}, false
	}
	if unbound {
		fault.Refuse("the run isn't bound: a contribution's run is bound once a maintainer approves its contribution job")(w, r)
		return request{}, false
	}
	repo, err := e.ghc.RepositoryByID(r.Context(), m.repo)
	if err != nil {
		fault.Unavailable(retryAfter)(w, r)
		return request{}, false
	}
	if repo.OwnerID != e.org {
		fault.Refuse("the repository is no longer the organization's")(w, r)
		return request{}, false
	}
	bd, bound, err := e.binder.Lookup(r.Context(), repo.ID, m.run)
	if err != nil {
		fault.Unavailable(retryAfter)(w, r)
		return request{}, false
	}
	if !bound {
		fault.Refuse("the run isn't bound")(w, r)
		return request{}, false
	}
	return request{m: m, repo: repo, bd: bd}, true
}

// refusal is an answer the service refuses a request with, saying why.
type refusal struct{ reason string }

func (r refusal) Error() string { return r.reason }

// answer writes a request's outcome: 200 with v, 403 for a refusal, or 503
// for anything GitHub didn't confirm.
func answer(w http.ResponseWriter, r *http.Request, v any, err error) {
	var re refusal
	switch {
	case errors.As(err, &re):
		fault.Refuse(re.reason)(w, r)
	case err != nil:
		fault.Unavailable(retryAfter)(w, r)
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

// Unavailable implements World.
func (e *Emulator) Unavailable(on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.unavailable = on
}

// Binding implements World.
func (e *Emulator) Binding(r Run) (string, bool) {
	bd, ok, err := e.binder.Lookup(context.Background(), r.Repository.ID, r.ID)
	if err != nil || !ok {
		return "", false
	}
	return bd.Scope.String(), true
}

// runKey names a run's own state.
func runKey(repo, run int64, name string) string { return fmt.Sprintf("%d/%d/%s", repo, run, name) }
