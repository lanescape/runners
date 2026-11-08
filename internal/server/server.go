// Package server is the service's HTTP interface, versioned under /v1/
// (specs/0001-architecture.md, Endpoints). Every endpoint authenticates its
// caller itself, since the service is reachable from the internet.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/pass"
)

// Routes are the service's endpoints.
const (
	WebhookPath     = "/v1/webhook"
	ReconcilePath   = "/v1/reconcile"
	BindPath        = "/v1/bind"
	JobStartedPath  = "/v1/jobs/started"
	JobFinishedPath = "/v1/jobs/finished"
)

// Limits on what the service reads from callers.
const (
	// GitHub's workflow_job and workflow_run deliveries are tens of
	// kilobytes; anything near this is not one of them.
	DefaultWebhookLimit = 1 << 20
	pushLimit           = 1 << 20
	bindLimit           = 4 << 10
	bindTimeout         = 30 * time.Second
)

// Delivery is a webhook delivery whose signature has been checked.
type Delivery struct {
	Event string
	ID    string
	Body  []byte
}

// Publisher hands deliveries on to the reconcile endpoint, through Pub/Sub.
type Publisher interface {
	Publish(ctx context.Context, d Delivery) error
}

// Reconciler does the work of a pass.
type Reconciler interface {
	Pass(p *pass.Pass) error
}

// Config is what the server needs.
type Config struct {
	Logger         *slog.Logger
	OrganizationID int64

	WebhookSecret []byte
	WebhookLimit  int64
	Publisher     Publisher

	ReconcileCallers *auth.Google
	Passes           *pass.Runner
	Reconciler       Reconciler

	Actions *auth.Actions
	Binder  *binding.Binder

	// JobStarted and JobFinished are the machines' endpoints, which check
	// a Compute Engine identity token themselves; unset, they aren't
	// served.
	JobStarted  http.Handler
	JobFinished http.Handler
}

type server struct {
	Config
	log *slog.Logger
}

// New returns the service's handler.
func New(c Config) (http.Handler, error) {
	if c.OrganizationID <= 0 || len(c.WebhookSecret) == 0 || c.Publisher == nil ||
		c.ReconcileCallers == nil || c.Passes == nil || c.Reconciler == nil || c.Actions == nil || c.Binder == nil {
		return nil, errors.New("server: incomplete configuration")
	}
	if c.WebhookLimit == 0 {
		c.WebhookLimit = DefaultWebhookLimit
	}
	s := &server{Config: c, log: c.Logger}
	if s.log == nil {
		s.log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+WebhookPath, s.webhook)
	mux.HandleFunc("POST "+ReconcilePath, s.reconcile)
	mux.HandleFunc("POST "+BindPath, s.bind)
	if c.JobStarted != nil {
		mux.Handle("POST "+JobStartedPath, c.JobStarted)
	}
	if c.JobFinished != nil {
		mux.Handle("POST "+JobFinishedPath, c.JobFinished)
	}
	return mux, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// retryLater answers with a retryable error, which Pub/Sub redelivers with
// backoff and the runners action tries again.
func retryLater(w http.ResponseWriter, msg string) {
	w.Header().Set("Retry-After", "5")
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": msg, "retryable": true})
}

// refuse answers an authentication failure. The caller learns only the
// status; the log says why, without the credential.
func (s *server) refuse(w http.ResponseWriter, r *http.Request, err error) {
	code := http.StatusUnauthorized
	if errors.Is(err, auth.ErrForbidden) {
		code = http.StatusForbidden
	}
	s.log.Warn("caller refused", "path", r.URL.Path, "status", code, "reason", err.Error())
	writeError(w, code, http.StatusText(code))
}

// webhookHeader is what the service reads of a delivery to know whose it
// is: the repository's owner, and the organization if the event names one.
type webhookHeader struct {
	Repository *struct {
		ID    int64 `json:"id"`
		Owner *struct {
			ID int64 `json:"id"`
		} `json:"owner"`
	} `json:"repository"`
	Organization *struct {
		ID int64 `json:"id"`
	} `json:"organization"`
}

var webhookEvents = map[string]bool{"workflow_job": true, "workflow_run": true}

func (s *server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := auth.ReadSigned(r, s.WebhookSecret, s.WebhookLimit)
	if errors.Is(err, auth.ErrTooLarge) {
		s.log.Warn("webhook delivery too large", "limit", s.WebhookLimit)
		writeError(w, http.StatusRequestEntityTooLarge, "delivery too large")
		return
	}
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	event, id := r.Header.Get("X-GitHub-Event"), r.Header.Get("X-GitHub-Delivery")
	if !webhookEvents[event] {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var h webhookHeader
	if err := json.Unmarshal(body, &h); err != nil {
		writeError(w, http.StatusBadRequest, "delivery isn't JSON")
		return
	}
	if h.Repository == nil || h.Repository.Owner == nil || h.Repository.Owner.ID != s.OrganizationID ||
		(h.Organization != nil && h.Organization.ID != s.OrganizationID) {
		s.log.Info("webhook delivery for another organization ignored", "event", event, "delivery", id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.Publisher.Publish(r.Context(), Delivery{Event: event, ID: id, Body: body}); err != nil {
		s.log.Error("publishing webhook delivery", "event", event, "delivery", id, "err", err)
		retryLater(w, "delivery not published")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) reconcile(w http.ResponseWriter, r *http.Request) {
	tok, err := auth.Bearer(r)
	if err == nil {
		_, err = s.ReconcileCallers.Verify(r.Context(), tok)
	}
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	// A push's body says what arrived; a pass works from what GitHub and
	// Compute Engine report, so for now it's read only to its limit.
	if _, err := io.Copy(io.Discard, io.LimitReader(r.Body, pushLimit)); err != nil {
		writeError(w, http.StatusBadRequest, "reading body")
		return
	}
	err = s.Passes.Run(r.Context(), s.Reconciler.Pass)
	switch {
	case errors.Is(err, pass.ErrHeld):
		retryLater(w, "a pass is running")
	case err != nil:
		s.log.Error("pass failed", "err", err)
		retryLater(w, "pass failed")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *server) bind(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), bindTimeout)
	defer cancel()
	tok, err := auth.Bearer(r)
	var claims *auth.ActionsClaims
	if err == nil {
		claims, err = s.Actions.Verify(ctx, tok)
	}
	if err != nil {
		s.refuse(w, r, err)
		return
	}
	var req binding.Request
	body, err := io.ReadAll(io.LimitReader(r.Body, bindLimit+1))
	if err != nil || len(body) > bindLimit {
		writeError(w, http.StatusBadRequest, "body unreadable or too large")
		return
	}
	if len(body) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "body isn't a binding request")
			return
		}
	}
	bd, err := s.Binder.Bind(ctx, claims, req)
	switch {
	case errors.Is(err, binding.ErrRefused):
		s.log.Warn("run binding refused", "repository_id", claims.RepositoryID, "run_id", claims.RunID, "reason", err.Error())
		writeError(w, http.StatusForbidden, http.StatusText(http.StatusForbidden))
	case err != nil:
		s.log.Warn("run binding not made", "repository_id", claims.RepositoryID, "run_id", claims.RunID, "err", err.Error())
		retryLater(w, "GitHub couldn't confirm the run; try again")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"scope": bd.Scope.String()})
	}
}
