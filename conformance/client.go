package conformance

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
)

// T is what a case needs of its test; *testing.T is one.
type T interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Client speaks the interface for actions (specs/0005-interface.md) as an
// action does, and checks every answer's shape as it goes: 200 with the
// endpoint's object, 403 with a reason, or 503, retryable, with a
// Retry-After, and nothing else.
type Client struct {
	T   T
	URL string
	// HTTP is the client requests go through; nil means
	// http.DefaultClient.
	HTTP *http.Client
}

// Answer is an answer's status, and what a 403 or 503 says.
type Answer struct {
	Status    int
	Reason    string
	Retryable bool
	Body      map[string]any
}

var scopePattern = regexp.MustCompile(`^(main|read|pr[1-9][0-9]*|branch-[a-z][a-z0-9]{0,15})$`)

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// post sends a request to the service with the job's token, and checks the
// answer's shape.
func (c *Client) post(path, token string, body []byte) Answer {
	c.T.Helper()
	req, err := http.NewRequest(http.MethodPost, c.URL+path, bytes.NewReader(body))
	if err != nil {
		c.T.Fatalf("%s: %v", path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		c.T.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		c.T.Fatalf("%s answered %d with a body that isn't a JSON object: %v", path, resp.StatusCode, err)
	}
	a := Answer{Status: resp.StatusCode, Body: out}
	a.Reason, _ = out["reason"].(string)
	a.Retryable, _ = out["retryable"].(bool)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		if a.Reason == "" {
			c.T.Fatalf("%s answered 403 with no reason", path)
		}
	case http.StatusServiceUnavailable:
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); !a.Retryable || err != nil || n <= 0 {
			c.T.Fatalf("%s answered 503 without retryable and a Retry-After: %v, %q", path, out, resp.Header.Get("Retry-After"))
		}
	default:
		c.T.Fatalf("%s answered %d, which the interface doesn't have", path, resp.StatusCode)
	}
	return a
}

func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Bind is POST /v1/bind, with what a bootstrap sends from its event, if
// event isn't nil. A 200 holds a scope.
func (c *Client) Bind(token string, event *PullRequestEvent) Answer {
	c.T.Helper()
	var body []byte
	if event != nil {
		body = marshal(event)
	}
	return c.BindRaw(token, body)
}

// BindRaw is POST /v1/bind with any body.
func (c *Client) BindRaw(token string, body []byte) Answer {
	c.T.Helper()
	a := c.post("/v1/bind", token, body)
	if a.Status == http.StatusOK {
		if s, _ := a.Body["scope"].(string); !scopePattern.MatchString(s) {
			c.T.Fatalf("bind answered 200 with scope %v", a.Body["scope"])
		}
	}
	return a
}

// Scope is a 200 bind answer's scope.
func (a Answer) Scope() string { s, _ := a.Body["scope"].(string); return s }

// PullRequestEvent is what a bootstrap sends from its own event.
type PullRequestEvent struct {
	PullRequest      int    `json:"pull_request"`
	HeadRepositoryID int64  `json:"head_repository_id"`
	HeadSHA          string `json:"head_sha"`
}
