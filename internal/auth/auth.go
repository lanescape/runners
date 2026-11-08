// Package auth authenticates the service's callers (specs/0001-architecture.md,
// Endpoints): GitHub's webhook deliveries by their HMAC signature, Google's
// signed calls from the scheduler and push subscriptions, GitHub Actions
// jobs by their OIDC tokens, and the service's machines by their Compute
// Engine identity tokens.
//
// Errors say why a caller was refused, never with the token itself.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ErrUnauthenticated means the caller didn't prove who it is: no signature
// or token, or one that doesn't verify.
var ErrUnauthenticated = errors.New("unauthenticated")

// ErrForbidden means the caller proved who it is, and isn't allowed.
var ErrForbidden = errors.New("forbidden")

// ErrTooLarge means a body was larger than its limit.
var ErrTooLarge = errors.New("body too large")

// ReadSigned reads a webhook delivery's body, at most limit bytes, and checks
// GitHub's X-Hub-Signature-256 over it before anything reads it as JSON.
func ReadSigned(r *http.Request, secret []byte, limit int64) ([]byte, error) {
	if len(secret) == 0 {
		return nil, errors.New("no webhook secret configured")
	}
	sig, ok := strings.CutPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	if !ok {
		return nil, fmt.Errorf("%w: no X-Hub-Signature-256", ErrUnauthenticated)
	}
	want, err := hex.DecodeString(sig)
	if err != nil || len(want) != sha256.Size {
		return nil, fmt.Errorf("%w: malformed X-Hub-Signature-256", ErrUnauthenticated)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, ErrTooLarge
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	if !hmac.Equal(m.Sum(nil), want) {
		return nil, fmt.Errorf("%w: X-Hub-Signature-256 doesn't match", ErrUnauthenticated)
	}
	return body, nil
}

// Bearer is a request's bearer token.
func Bearer(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || strings.ContainsAny(tok, " \t") {
		return "", fmt.Errorf("%w: no bearer token", ErrUnauthenticated)
	}
	return tok, nil
}

// Issuer is where an OIDC issuer's tokens come from and its keys are
// published.
type Issuer struct {
	URL     string
	JWKSURL string
}

// The production issuers.
var (
	GoogleIssuer  = Issuer{URL: "https://accounts.google.com", JWKSURL: "https://www.googleapis.com/oauth2/v3/certs"}
	ActionsIssuer = Issuer{URL: "https://token.actions.githubusercontent.com", JWKSURL: "https://token.actions.githubusercontent.com/.well-known/jwks"}
)

// Options are what every verifier needs.
type Options struct {
	Issuer   Issuer
	Audience string
	// HTTPClient fetches the issuer's keys; nil means one with a timeout.
	HTTPClient *http.Client
	// Now is the time tokens are checked at; nil means time.Now.
	Now func() time.Time
}

func (o Options) verifier() (*oidc.IDTokenVerifier, error) {
	if o.Issuer.URL == "" || o.Issuer.JWKSURL == "" || o.Audience == "" {
		return nil, errors.New("auth: issuer, key set and audience are all required")
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), hc), o.Issuer.JWKSURL)
	return oidc.NewVerifier(o.Issuer.URL, keys, &oidc.Config{
		ClientID:             o.Audience,
		SupportedSigningAlgs: []string{oidc.RS256},
		Now:                  o.Now,
	}), nil
}

func verify(ctx context.Context, v *oidc.IDTokenVerifier, raw string, claims any) error {
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		// go-oidc's errors name what failed, never the token itself.
		return fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if err := tok.Claims(claims); err != nil {
		return fmt.Errorf("%w: claims: %v", ErrUnauthenticated, err)
	}
	return nil
}

// Google verifies the reconcile endpoint's callers: Google-signed ID tokens
// for the service, from the scheduler's or the push subscriptions' accounts,
// with the email verified as Google's.
type Google struct {
	v        *oidc.IDTokenVerifier
	accounts []string
}

// NewGoogle returns a verifier for tokens of the given accounts.
func NewGoogle(o Options, accounts []string) (*Google, error) {
	if len(accounts) == 0 {
		return nil, errors.New("auth: no accounts may call the reconcile endpoint")
	}
	v, err := o.verifier()
	if err != nil {
		return nil, err
	}
	return &Google{v: v, accounts: slices.Clone(accounts)}, nil
}

// Verify checks a token and returns its account's email.
func (g *Google) Verify(ctx context.Context, raw string) (string, error) {
	var c struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
	}
	if err := verify(ctx, g.v, raw, &c); err != nil {
		return "", err
	}
	if c.EmailVerified == nil || !*c.EmailVerified {
		return "", fmt.Errorf("%w: email_verified isn't true", ErrForbidden)
	}
	if !slices.Contains(g.accounts, c.Email) {
		return "", fmt.Errorf("%w: email isn't an allowed account", ErrForbidden)
	}
	return c.Email, nil
}

// ActionsClaims are what the service uses of a GitHub Actions job's token.
type ActionsClaims struct {
	Repository        string
	RepositoryID      int64
	RepositoryOwnerID int64
	RunID             int64
	RunAttempt        int64
	EventName         string
	Ref               string
	RefType           string
	SHA               string
	JobWorkflowRef    string
}

// Actions verifies GitHub Actions OIDC tokens for the service, from the
// organization's repositories.
type Actions struct {
	v     *oidc.IDTokenVerifier
	orgID int64
}

// NewActions returns a verifier of tokens from the organization's jobs.
func NewActions(o Options, organizationID int64) (*Actions, error) {
	if organizationID <= 0 {
		return nil, errors.New("auth: organization ID required")
	}
	v, err := o.verifier()
	if err != nil {
		return nil, err
	}
	return &Actions{v: v, orgID: organizationID}, nil
}

// id reads one of GitHub's numeric claims, which it signs as decimal
// strings.
func id(name, s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != s {
		return 0, fmt.Errorf("%w: %s isn't a numeric ID", ErrUnauthenticated, name)
	}
	return n, nil
}

// Verify checks a token: its signature against the issuer's published keys,
// its issuer, audience and expiry, and that its repository's owner is the
// organization, by ID.
func (a *Actions) Verify(ctx context.Context, raw string) (*ActionsClaims, error) {
	var c struct {
		Repository        string `json:"repository"`
		RepositoryID      string `json:"repository_id"`
		RepositoryOwnerID string `json:"repository_owner_id"`
		RunID             string `json:"run_id"`
		RunAttempt        string `json:"run_attempt"`
		EventName         string `json:"event_name"`
		Ref               string `json:"ref"`
		RefType           string `json:"ref_type"`
		SHA               string `json:"sha"`
		JobWorkflowRef    string `json:"job_workflow_ref"`
	}
	if err := verify(ctx, a.v, raw, &c); err != nil {
		return nil, err
	}
	owner, err := id("repository_owner_id", c.RepositoryOwnerID)
	if err != nil {
		return nil, err
	}
	if owner != a.orgID {
		return nil, fmt.Errorf("%w: repository_owner_id isn't the organization's", ErrForbidden)
	}
	out := &ActionsClaims{
		Repository: c.Repository, RepositoryOwnerID: owner, EventName: c.EventName,
		Ref: c.Ref, RefType: c.RefType, SHA: c.SHA, JobWorkflowRef: c.JobWorkflowRef,
	}
	if out.RepositoryID, err = id("repository_id", c.RepositoryID); err != nil {
		return nil, err
	}
	if out.RunID, err = id("run_id", c.RunID); err != nil {
		return nil, err
	}
	if out.RunAttempt, err = id("run_attempt", c.RunAttempt); err != nil {
		return nil, err
	}
	if c.EventName == "" || c.Ref == "" || c.Repository == "" {
		return nil, fmt.Errorf("%w: event_name, ref and repository are required", ErrUnauthenticated)
	}
	return out, nil
}

// MachineClaims identify a machine, from its Compute Engine identity token
// in the full format.
type MachineClaims struct {
	Email        string
	ProjectID    string
	Zone         string
	InstanceID   uint64
	InstanceName string
}

// Machines verifies the identity tokens of the service's machines: signed
// by Google, for the service, in the full format, whose account is the
// machines' and whose project is the service's. Which machine it is, the
// caller checks against Compute Engine.
type Machines struct {
	v                *oidc.IDTokenVerifier
	project, account string
}

// NewMachines returns a verifier of project's machines' tokens, issued for
// account.
func NewMachines(o Options, project, account string) (*Machines, error) {
	if project == "" || account == "" {
		return nil, errors.New("auth: the machines' project and account are required")
	}
	v, err := o.verifier()
	if err != nil {
		return nil, err
	}
	return &Machines{v: v, project: project, account: account}, nil
}

// Verify checks a machine's token.
func (m *Machines) Verify(ctx context.Context, raw string) (*MachineClaims, error) {
	var c struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Google        *struct {
			ComputeEngine *struct {
				ProjectID    string `json:"project_id"`
				Zone         string `json:"zone"`
				InstanceID   string `json:"instance_id"`
				InstanceName string `json:"instance_name"`
			} `json:"compute_engine"`
		} `json:"google"`
	}
	if err := verify(ctx, m.v, raw, &c); err != nil {
		return nil, err
	}
	if c.Google == nil || c.Google.ComputeEngine == nil {
		return nil, fmt.Errorf("%w: not a Compute Engine identity token in the full format", ErrUnauthenticated)
	}
	ce := c.Google.ComputeEngine
	if c.EmailVerified == nil || !*c.EmailVerified || c.Email != m.account {
		return nil, fmt.Errorf("%w: the token's account isn't the machines'", ErrForbidden)
	}
	if ce.ProjectID != m.project {
		return nil, fmt.Errorf("%w: the token's project isn't the service's", ErrForbidden)
	}
	n, err := strconv.ParseUint(ce.InstanceID, 10, 64)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != ce.InstanceID || ce.Zone == "" || ce.InstanceName == "" {
		return nil, fmt.Errorf("%w: the token names no instance", ErrUnauthenticated)
	}
	return &MachineClaims{Email: c.Email, ProjectID: ce.ProjectID, Zone: ce.Zone, InstanceID: n, InstanceName: ce.InstanceName}, nil
}
