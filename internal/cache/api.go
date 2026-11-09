package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/machines"
)

// The cache's endpoints (specs/0005-interface.md, The cache).
const (
	RestorePath = "/v1/cache/restore"
	SavePath    = "/v1/cache/save"
	CommitPath  = "/v1/cache/commit"
)

const (
	bodyLimit      = 4 << 10
	requestTimeout = 30 * time.Second
)

// refusal is the service's refusal, which the caller doesn't retry: 403,
// with the reason.
type refusal struct{ reason string }

func (r *refusal) Error() string { return r.reason }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// Handler serves the cache's three endpoints. Each answers 200 with its
// object, 403 with a reason, or 503, retryable, with a Retry-After, and
// nothing else.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+RestorePath, s.serve(RestorePath, s.restore))
	mux.HandleFunc("POST "+SavePath, s.serve(SavePath, s.save))
	mux.HandleFunc("POST "+CommitPath, s.serve(CommitPath, s.commit))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// serve authenticates the caller and answers with what h returns. Nothing
// logged holds the token, a URL or a session URI.
func (s *Service) serve(path string, h func(ctx context.Context, c *caller, body []byte) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		body, err := io.ReadAll(io.LimitReader(r.Body, bodyLimit+1))
		var out any
		var c *caller
		switch {
		case err != nil || len(body) > bodyLimit:
			err = refuse("the body is unreadable or too large")
		default:
			c, err = s.authenticate(ctx, r)
		}
		if err == nil {
			out, err = h(ctx, c, body)
		}
		var rf *refusal
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, out)
		case errors.As(err, &rf):
			attrs := []any{"path", path, "reason", rf.reason}
			if c != nil {
				attrs = append(attrs, "repository_id", c.repo.ID, "run_id", c.claims.RunID, "job_id", c.claims.CheckRunID)
			}
			s.log().Warn("cache request refused", attrs...)
			writeJSON(w, http.StatusForbidden, map[string]any{"error": http.StatusText(http.StatusForbidden), "reason": rf.reason})
		default:
			s.log().Warn("cache request not answered; the caller tries again", "path", path, "err", err.Error())
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the service couldn't confirm an answer; try again", "retryable": true})
		}
	}
}

// decode reads a request body as exactly one JSON object with no other
// fields.
func decode(body []byte, into any, what string) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return refuse("the body isn't a %s request", what)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		return refuse("the body isn't a %s request", what)
	}
	return nil
}

// caller is a request's job, as GitHub signed and records it, and its run's
// binding.
type caller struct {
	claims  *auth.ActionsClaims
	repo    github.Repository
	binding binding.Binding
}

// authenticate checks a request's caller (specs/0006-cache.md, Who may use
// it): a GitHub OIDC token for the service, from the organization's
// repository, naming its job by check_run_id; GitHub must show that job in
// progress, in the token's run and attempt; the run must be bound; and a
// run whose head is another repository's must be an approved contribution.
func (s *Service) authenticate(ctx context.Context, r *http.Request) (*caller, error) {
	tok, err := auth.Bearer(r)
	if err != nil {
		return nil, refuse("no token")
	}
	claims, err := s.Actions.Verify(ctx, tok)
	if errors.Is(err, auth.ErrUnavailable) {
		return nil, err
	}
	if err != nil {
		return nil, refuse("the token isn't one the service accepts: %v", err)
	}
	if claims.CheckRunID == 0 {
		return nil, refuse("the token names no job: it has no check_run_id")
	}
	if claims.RepositoryOwnerID != s.OrganizationID {
		return nil, refuse("the token's repository isn't the organization's")
	}
	repo, err := s.GitHub.RepositoryByID(ctx, claims.RepositoryID)
	if github.IsUnavailable(err) {
		return nil, err
	}
	if err != nil {
		return nil, refuse("GitHub shows no repository %d", claims.RepositoryID)
	}
	if repo.OwnerID != s.OrganizationID {
		return nil, refuse("GitHub reports the repository's owner isn't the organization")
	}
	job, err := s.GitHub.Job(ctx, repo, claims.CheckRunID)
	if github.IsUnavailable(err) {
		return nil, err
	}
	if err != nil {
		return nil, refuse("GitHub shows no job %d in the token's repository", claims.CheckRunID)
	}
	if job.RunID != claims.RunID || int64(job.RunAttempt) != claims.RunAttempt {
		return nil, refuse("the token's job isn't of the token's run and attempt")
	}
	if job.Status != "in_progress" {
		return nil, refuse("the token's job isn't running")
	}
	bd, ok, err := s.Binder.Lookup(ctx, repo.ID, claims.RunID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, refuse("the run isn't bound: its runners job binds it first")
	}
	if bd.HeadRepositoryID != repo.ID {
		if bd.Event != "pull_request_target" {
			return nil, refuse("the run is a fork's pull request's")
		}
		as, err := s.GitHub.RunApprovals(ctx, repo, claims.RunID)
		if github.IsUnavailable(err) {
			return nil, err
		}
		approved := false
		for _, a := range as {
			if a.State == "approved" && slices.ContainsFunc(a.Environments, func(e string) bool {
				return strings.EqualFold(e, machines.ContributionEnvironment)
			}) {
				approved = true
			}
		}
		if err != nil || !approved {
			return nil, refuse("the contribution's run has no approval of the %s environment", machines.ContributionEnvironment)
		}
	}
	return &caller{claims: claims, repo: repo, binding: bd}, nil
}
