// Package github is the service's view of GitHub: what it reads, as the
// organization's GitHub App installation, with every answer classed as
// confirmed or not. Only GitHub's confirmed answers may decide anything
// (specs/0002-machines.md, Scope).
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v93/github"
)

// ErrNotFound is GitHub's confirmed answer that something doesn't exist.
// Any error that isn't Unavailable is a confirmed answer of some kind.
var ErrNotFound = errors.New("not found on GitHub")

// Unavailable wraps every other failure: GitHub couldn't be reached, timed
// out, limited the service's requests, or answered in a way that confirms
// nothing. A caller must not decide anything from it.
type Unavailable struct{ Err error }

func (u *Unavailable) Error() string { return "GitHub unavailable: " + u.Err.Error() }
func (u *Unavailable) Unwrap() error { return u.Err }

// IsUnavailable reports whether err is an Unavailable.
func IsUnavailable(err error) bool {
	var u *Unavailable
	return errors.As(err, &u)
}

// Repository is a repository, as GitHub reports it.
type Repository struct {
	ID            int64
	OwnerID       int64
	Owner         string
	Name          string
	DefaultBranch string
}

// WorkflowRun is a workflow run, as GitHub records it. For a
// pull_request_target run, its head is the base branch's.
type WorkflowRun struct {
	ID               int64
	RepositoryID     int64
	Event            string
	Status           string
	Conclusion       string
	Attempt          int
	HeadBranch       string
	HeadSHA          string
	HeadRepositoryID int64
	CreatedAt        time.Time
}

// PullRequest is a pull request. HeadRepositoryID is 0 once the head
// repository is deleted. ClosedAt is when it was last closed, zero while
// it's open.
type PullRequest struct {
	Number           int
	State            string
	ClosedAt         time.Time
	BaseRepositoryID int64
	HeadRepositoryID int64
	HeadSHA          string
	BaseSHA          string
	Commits          int
}

// API is what the service reads from GitHub.
type API interface {
	RepositoryByID(ctx context.Context, id int64) (Repository, error)
	WorkflowRun(ctx context.Context, repo Repository, id int64) (WorkflowRun, error)
	PullRequest(ctx context.Context, repo Repository, number int) (PullRequest, error)
	OpenPullRequests(ctx context.Context, repo Repository) ([]PullRequest, error)
	// PullRequestCommits lists a pull request's commits; GitHub lists at
	// most 250.
	PullRequestCommits(ctx context.Context, repo Repository, number int) ([]string, error)
	// BranchHead is a branch's commit, by its full ref, so a tag of the
	// same name can't stand in for it.
	BranchHead(ctx context.Context, repo Repository, branch string) (string, error)
	// Compare is GitHub's comparison of two commits: identical, ahead,
	// behind or diverged.
	Compare(ctx context.Context, repo Repository, base, head string) (string, error)
	// File reads a file at a commit.
	File(ctx context.Context, repo Repository, path, commit string) ([]byte, error)

	Actions
}

// Client is API over GitHub's REST API.
type Client struct {
	gh    *gh.Client
	app   *gh.Client
	orgID int64

	botMu sync.Mutex
	bot   string

	orgMu    sync.Mutex
	orgLogin string
	orgUntil time.Time
}

// Options say how to reach GitHub as the App.
type Options struct {
	// BaseURL is GitHub's API, https://api.github.com in production.
	BaseURL string
	AppID   int64
	// PrivateKey is the App's PEM private key.
	PrivateKey []byte
	// OrganizationID is the account the App is installed on.
	OrganizationID int64
	// Transport is the underlying transport; nil means the default.
	Transport http.RoundTripper
}

// NewAppClient returns a client that acts as the App's installation on the
// organization, found by the organization's ID when first needed.
func NewAppClient(o Options) (*Client, error) {
	tr := o.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	apps, err := ghinstallation.NewAppsTransport(tr, o.AppID, o.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	base := strings.TrimRight(o.BaseURL, "/")
	apps.BaseURL = base
	appClient, err := newClient(&http.Client{Transport: apps}, base)
	if err != nil {
		return nil, err
	}
	it := &installationTransport{app: appClient.gh, base: tr, orgID: o.OrganizationID, sem: make(chan struct{}, 1)}
	c, err := newClient(&http.Client{Transport: it}, base)
	if err != nil {
		return nil, err
	}
	c.orgID, c.app = o.OrganizationID, appClient.gh
	return c, nil
}

// Bot is the login of the App's bot user, which acts for its installation,
// such as when it reruns a run: <slug>[bot].
func (c *Client) Bot(ctx context.Context) (string, error) {
	c.botMu.Lock()
	defer c.botMu.Unlock()
	if c.bot != "" {
		return c.bot, nil
	}
	a, _, err := c.app.Apps.Get(ctx, "")
	if err != nil {
		return "", classify(err)
	}
	if a.GetSlug() == "" {
		return "", &Unavailable{Err: errors.New("the App has no slug")}
	}
	c.bot = a.GetSlug() + "[bot]"
	return c.bot, nil
}

func newClient(hc *http.Client, base string) (*Client, error) {
	u := base + "/"
	c, err := gh.NewClient(gh.WithHTTPClient(hc), gh.WithURLs(&u, &u))
	if err != nil {
		return nil, err
	}
	return &Client{gh: c}, nil
}

// installationTransport finds the App's installation on the organization,
// by the account's ID, and authenticates as it with an installation token
// that it refreshes before it expires. Callers wait for a refresh only as
// long as their own contexts allow, so a slow GitHub can't hold a call past
// its deadline.
type installationTransport struct {
	app   *gh.Client
	base  http.RoundTripper
	orgID int64

	// sem guards the fields below; it's a channel so that waiting for it
	// can give up with the caller's context.
	sem            chan struct{}
	installationID int64
	token          string
	expires        time.Time
}

// refreshBefore is how long before a token expires it's replaced.
const refreshBefore = 5 * time.Minute

func (t *installationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.currentToken(req.Context())
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "token "+tok)
	return t.base.RoundTrip(r)
}

func (t *installationTransport) currentToken(ctx context.Context) (string, error) {
	select {
	case t.sem <- struct{}{}:
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
	defer func() { <-t.sem }()
	if t.token != "" && time.Until(t.expires) > refreshBefore {
		return t.token, nil
	}
	if t.installationID == 0 {
		id, err := t.find(ctx)
		if err != nil {
			return "", &Unavailable{Err: err}
		}
		t.installationID = id
	}
	tok, _, err := t.app.Apps.CreateInstallationToken(ctx, t.installationID, nil)
	if err != nil {
		return "", &Unavailable{Err: fmt.Errorf("making an installation token: %w", err)}
	}
	if tok.GetToken() == "" {
		return "", &Unavailable{Err: errors.New("making an installation token: empty answer")}
	}
	t.token, t.expires = tok.GetToken(), tok.GetExpiresAt().Time
	return t.token, nil
}

func (t *installationTransport) find(ctx context.Context) (int64, error) {
	opts := &gh.ListOptions{PerPage: 100}
	for {
		ins, resp, err := t.app.Apps.ListInstallations(ctx, opts)
		if err != nil {
			return 0, fmt.Errorf("listing the app's installations: %w", err)
		}
		for _, in := range ins {
			if in.GetAccount().GetID() == t.orgID {
				return in.GetID(), nil
			}
		}
		if resp.NextPage == 0 {
			return 0, fmt.Errorf("the app isn't installed on organization %d", t.orgID)
		}
		opts.Page = resp.NextPage
	}
}

// classify makes every error either ErrNotFound or Unavailable.
func classify(err error) error {
	if err == nil {
		return nil
	}
	// A failure to authenticate says nothing about what was asked for,
	// whatever GitHub answered the token request with.
	if IsUnavailable(err) {
		return err
	}
	var er *gh.ErrorResponse
	if errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, er.Message)
	}
	return &Unavailable{Err: err}
}

func repoOf(r *gh.Repository) Repository {
	return Repository{
		ID:            r.GetID(),
		OwnerID:       r.GetOwner().GetID(),
		Owner:         r.GetOwner().GetLogin(),
		Name:          r.GetName(),
		DefaultBranch: r.GetDefaultBranch(),
	}
}

// RepositoryByID implements API.
func (c *Client) RepositoryByID(ctx context.Context, id int64) (Repository, error) {
	r, _, err := c.gh.Repositories.GetByID(ctx, id)
	if err != nil {
		return Repository{}, classify(err)
	}
	repo := repoOf(r)
	if repo.ID != id || repo.OwnerID == 0 || repo.DefaultBranch == "" {
		return Repository{}, &Unavailable{Err: fmt.Errorf("repository %d: incomplete answer", id)}
	}
	return repo, nil
}

func runOf(r *gh.WorkflowRun) WorkflowRun {
	return WorkflowRun{
		ID:               r.GetID(),
		RepositoryID:     r.GetRepository().GetID(),
		Event:            r.GetEvent(),
		Status:           r.GetStatus(),
		Conclusion:       r.GetConclusion(),
		Attempt:          r.GetRunAttempt(),
		HeadBranch:       r.GetHeadBranch(),
		HeadSHA:          r.GetHeadSHA(),
		HeadRepositoryID: r.GetHeadRepository().GetID(),
		CreatedAt:        r.GetCreatedAt().Time,
	}
}

// WorkflowRun implements API.
func (c *Client) WorkflowRun(ctx context.Context, repo Repository, id int64) (WorkflowRun, error) {
	r, _, err := c.gh.Actions.GetWorkflowRunByID(ctx, repo.Owner, repo.Name, id)
	if err != nil {
		return WorkflowRun{}, classify(err)
	}
	return runOf(r), nil
}

func pullOf(p *gh.PullRequest) PullRequest {
	return PullRequest{
		Number:           p.GetNumber(),
		State:            p.GetState(),
		ClosedAt:         p.GetClosedAt().Time,
		BaseRepositoryID: p.GetBase().GetRepo().GetID(),
		HeadRepositoryID: p.GetHead().GetRepo().GetID(),
		HeadSHA:          p.GetHead().GetSHA(),
		BaseSHA:          p.GetBase().GetSHA(),
		Commits:          p.GetCommits(),
	}
}

// PullRequest implements API.
func (c *Client) PullRequest(ctx context.Context, repo Repository, number int) (PullRequest, error) {
	p, _, err := c.gh.PullRequests.Get(ctx, repo.Owner, repo.Name, number)
	if err != nil {
		return PullRequest{}, classify(err)
	}
	return pullOf(p), nil
}

// OpenPullRequests implements API.
func (c *Client) OpenPullRequests(ctx context.Context, repo Repository) ([]PullRequest, error) {
	opts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []PullRequest
	for {
		ps, resp, err := c.gh.PullRequests.List(ctx, repo.Owner, repo.Name, opts)
		if err != nil {
			return nil, classify(err)
		}
		for _, p := range ps {
			out = append(out, pullOf(p))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// PullRequestCommits implements API.
func (c *Client) PullRequestCommits(ctx context.Context, repo Repository, number int) ([]string, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var out []string
	for {
		cs, resp, err := c.gh.PullRequests.ListCommits(ctx, repo.Owner, repo.Name, number, opts)
		if err != nil {
			return nil, classify(err)
		}
		for _, cm := range cs {
			out = append(out, cm.GetSHA())
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// BranchHead implements API.
func (c *Client) BranchHead(ctx context.Context, repo Repository, branch string) (string, error) {
	ref, _, err := c.gh.Git.GetRef(ctx, repo.Owner, repo.Name, "heads/"+branch)
	if err != nil {
		return "", classify(err)
	}
	if ref.GetRef() != "refs/heads/"+branch || ref.GetObject().GetSHA() == "" {
		return "", &Unavailable{Err: fmt.Errorf("ref %s: unexpected answer %s", branch, ref.GetRef())}
	}
	return ref.GetObject().GetSHA(), nil
}

// Compare implements API.
func (c *Client) Compare(ctx context.Context, repo Repository, base, head string) (string, error) {
	cmp, _, err := c.gh.Repositories.CompareCommits(ctx, repo.Owner, repo.Name, base, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return "", classify(err)
	}
	switch s := cmp.GetStatus(); s {
	case "identical", "ahead", "behind", "diverged":
		return s, nil
	default:
		return "", &Unavailable{Err: fmt.Errorf("comparison status %q", s)}
	}
}

// maxFile bounds what File reads; configuration files are small.
const maxFile = 64 << 10

// File implements API.
func (c *Client) File(ctx context.Context, repo Repository, path, commit string) ([]byte, error) {
	f, _, _, err := c.gh.Repositories.GetContents(ctx, repo.Owner, repo.Name, path, &gh.RepositoryContentGetOptions{Ref: commit})
	if err != nil {
		return nil, classify(err)
	}
	if f == nil {
		// A directory: there's no such file.
		return nil, fmt.Errorf("%w: %s is a directory", ErrNotFound, path)
	}
	if f.GetSize() > maxFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxFile)
	}
	s, err := f.GetContent()
	if err != nil {
		return nil, &Unavailable{Err: err}
	}
	return []byte(s), nil
}
