package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Delivery is a webhook delivery as GitHub sends it. Tests may change its
// body or headers before sending, to tamper with it.
type Delivery struct {
	URL    string
	Header http.Header
	Body   []byte
}

// Sign signs a body with a webhook secret, as X-Hub-Signature-256 carries it.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// NewDelivery makes a signed delivery of event, with payload as its JSON body.
func NewDelivery(url string, secret []byte, event string, payload any) *Delivery {
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "GitHub-Hookshot/emulator")
	h.Set("X-GitHub-Event", event)
	h.Set("X-GitHub-Delivery", fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:]))
	h.Set("X-GitHub-Hook-Installation-Target-Type", "integration")
	h.Set("X-Hub-Signature-256", Sign(secret, body))
	legacy := hmac.New(sha1.New, secret)
	legacy.Write(body)
	h.Set("X-Hub-Signature", "sha1="+hex.EncodeToString(legacy.Sum(nil)))
	return &Delivery{URL: url, Header: h, Body: body}
}

// Send posts the delivery.
func (d *Delivery) Send(ctx context.Context) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(d.Body))
	if err != nil {
		return nil, err
	}
	req.Header = d.Header.Clone()
	return http.DefaultClient.Do(req)
}

// WorkflowJobPayload is a workflow_job event for one of run's jobs.
func (e *Emulator) WorkflowJobPayload(run *Run, in *Installation, action string, jobID int64, labels []string) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return map[string]any{
		"action": action,
		"workflow_job": map[string]any{
			"id":          jobID,
			"run_id":      run.ID,
			"run_attempt": run.Attempt,
			"head_sha":    run.HeadSHA,
			"status":      action,
			"labels":      labels,
		},
		"repository":   e.repoJSON(run.Repo),
		"organization": accountJSON(run.Repo.Owner),
		"sender":       accountJSON(run.Actor),
		"installation": map[string]any{"id": in.ID},
	}
}

// PullRequestPayload is a pull_request event of p, such as its closing,
// with its state as it is now.
func (e *Emulator) PullRequestPayload(p *Pull, in *Installation, action string) map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var closedAt any
	if !p.Closed.IsZero() {
		closedAt = p.Closed.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"action": action,
		"number": p.Number,
		"pull_request": map[string]any{
			"number":    p.Number,
			"state":     p.State,
			"closed_at": closedAt,
			"base":      map[string]any{"ref": p.BaseRef, "repo": e.repoJSON(p.Base)},
			"head":      map[string]any{"ref": p.HeadRef, "sha": p.HeadSHA, "repo": e.repoJSON(p.Head)},
		},
		"repository":   e.repoJSON(p.Base),
		"organization": accountJSON(p.Base.Owner),
		"sender":       accountJSON(p.Author),
		"installation": map[string]any{"id": in.ID},
	}
}
