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

// EventRecorder records what a pushed message says that a pass can't read
// back later, such as Compute Engine's word that it preempted a machine,
// which Google keeps elsewhere for as little as an hour. It ignores any
// other message.
type EventRecorder interface {
	RecordEvent(ctx context.Context, data []byte) error
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
	// Events records pushed messages before they're acknowledged; unset,
	// nothing is.
	Events EventRecorder

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
	// A pass works from what GitHub and Compute Engine report when it
	// runs, never from the message that started it; but what a message
	// says that can't be read back later is recorded first, whether or not
	// a pass can run now, so it's recorded before it's acknowledged.
	body, err := io.ReadAll(io.LimitReader(r.Body, pushLimit+1))
	if err != nil || len(body) > pushLimit {
		writeError(w, http.StatusBadRequest, "body unreadable or too large")
		return
	}
	if data, ok := pushData(body); ok && s.Events != nil {
		if err := s.Events.RecordEvent(r.Context(), data); err != nil {
			s.log.Error("recording a pushed event", "err", err)
			retryLater(w, "event not recorded")
			return
		}
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

// bind is POST /v1/bind, which answers exactly as specs/0005-interface.md
// says: 200 with the run's scope; 403, with a reason, when the token or its
// run isn't one the service binds, or the body isn't a binding request; or
// 503 when GitHub or the bucket couldn't confirm an answer, which the caller
// retries with backoff. The reason never holds the token.
// push is a Pub/Sub push subscription's request body.
type push struct {
	Message *struct {
		Data      []byte `json:"data"`
		MessageID string `json:"messageId"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

// pushData is a pushed message's data, if the body is a push; the
// scheduler's calls aren't.
func pushData(body []byte) ([]byte, bool) {
	var p push
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &p) != nil || p.Message == nil || len(p.Message.Data) == 0 {
		return nil, false
	}
	return p.Message.Data, true
}

func (s *server) bind(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), bindTimeout)
	defer cancel()
	refuse := func(reason string) {
		s.log.Warn("run binding refused", "reason", reason)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": http.StatusText(http.StatusForbidden), "reason": reason})
	}
	tok, err := auth.Bearer(r)
	var claims *auth.ActionsClaims
	if err == nil {
		claims, err = s.Actions.Verify(ctx, tok)
	}
	if err != nil {
		refuse("the token isn't one the service binds: " + err.Error())
		return
	}
	var req binding.Request
	body, err := io.ReadAll(io.LimitReader(r.Body, bindLimit+1))
	if err != nil || len(body) > bindLimit {
		refuse("the body is unreadable or too large")
		return
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 {
		// A request is an object: not null, nor anything else.
		if trimmed[0] != '{' {
			refuse("the body isn't a binding request")
			return
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
			refuse("the body isn't a binding request")
			return
		}
	}
	bd, err := s.Binder.Bind(ctx, claims, req)
	switch {
	case errors.Is(err, binding.ErrRefused):
		s.log.Warn("run binding refused", "repository_id", claims.RepositoryID, "run_id", claims.RunID, "reason", err.Error())
		writeJSON(w, http.StatusForbidden, map[string]any{"error": http.StatusText(http.StatusForbidden), "reason": err.Error()})
	case err != nil:
		s.log.Warn("run binding not made", "repository_id", claims.RepositoryID, "run_id", claims.RunID, "err", err.Error())
		retryLater(w, "GitHub couldn't confirm the run; try again")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"scope": bd.Scope.String()})
	}
}
