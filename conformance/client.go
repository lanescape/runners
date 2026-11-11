package conformance

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/lanescape/runners/emulators/service"
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
// endpoint's object and nothing else in it, 403 with a reason, or 503,
// retryable, with a Retry-After, and no other status.
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

// Steps are where a work disk came from: the restore order's steps, or a
// resume image.
var Steps = map[string]bool{service.StepPullRequestExact: true, service.StepPullRequestNewest: true, service.StepMainExact: true,
	service.StepMainNewest: true, service.StepBranchExact: true, service.StepBranchNewest: true, service.StepNone: true,
	service.Resumed: true}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// do sends a request to the service signed with a machine's token, and
// checks the answer's shape.
func (c *Client) do(method, path, token string, body []byte) Answer {
	c.T.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.URL+path, r)
	if err != nil {
		c.T.Fatalf("%s: %v", path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		c.T.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	a := Answer{Status: resp.StatusCode}
	if err := json.Unmarshal(raw, &a.Body); (err != nil || a.Body == nil) && resp.StatusCode != http.StatusUnauthorized {
		c.T.Fatalf("%s answered %d with a body that isn't a JSON object: %q", path, resp.StatusCode, raw)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		a.Reason, _ = a.Body["reason"].(string)
		if a.Reason == "" {
			c.T.Fatalf("%s refused with no reason: %s", path, raw)
		}
	case http.StatusServiceUnavailable:
		a.Retryable, _ = a.Body["retryable"].(bool)
		ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if !a.Retryable || err != nil || ra < 1 {
			c.T.Fatalf("%s answered 503 without retryable and a Retry-After: %s, %q", path, raw, resp.Header.Get("Retry-After"))
		}
	case http.StatusUnauthorized:
	default:
		c.T.Fatalf("%s answered %d, which the interface doesn't: %s", path, resp.StatusCode, raw)
	}
	return a
}

// only checks that an object has no field but those named.
func (c *Client) only(what string, m map[string]any, fields ...string) {
	c.T.Helper()
	allowed := map[string]bool{}
	for _, f := range fields {
		allowed[f] = true
	}
	for k := range m {
		if !allowed[k] {
			c.T.Fatalf("%s has a field the interface doesn't give, %q: %v", what, k, m)
		}
	}
}

// DiskAnswer is the answer to a work disk request.
type DiskAnswer struct{ State, Restored string }

// Disk asks for the job's work disk of a family, as the disk action does,
// once, and checks the answer.
func (c *Client) Disk(token, family string) (Answer, DiskAnswer) {
	c.T.Helper()
	body, _ := json.Marshal(map[string]string{"family": family})
	a := c.do(http.MethodPost, "/v1/disks", token, body)
	if a.Status != http.StatusOK {
		return a, DiskAnswer{}
	}
	c.only("a work disk's answer", a.Body, "state", "restored")
	var d DiskAnswer
	d.State, _ = a.Body["state"].(string)
	d.Restored, _ = a.Body["restored"].(string)
	if (d.State != "making" && d.State != "ready") || !Steps[d.Restored] {
		c.T.Fatalf("a work disk's answer isn't one the interface gives: %v", a.Body)
	}
	return a, d
}

// SaveAnswer is the answer to a save request.
type SaveAnswer struct {
	Saves  bool
	Reason string
}

// Save asks for the job's work disk to be saved when it finishes, as the
// save-disk action does, and checks the answer.
func (c *Client) Save(token string) (Answer, SaveAnswer) {
	c.T.Helper()
	a := c.do(http.MethodPost, "/v1/disks/save", token, []byte(`{"save": true}`))
	if a.Status != http.StatusOK {
		return a, SaveAnswer{}
	}
	c.only("a save's answer", a.Body, "saves", "reason")
	var s SaveAnswer
	var ok bool
	s.Saves, ok = a.Body["saves"].(bool)
	s.Reason, _ = a.Body["reason"].(string)
	if !ok || s.Saves == (s.Reason != "") {
		c.T.Fatalf("a save's answer isn't one the interface gives: %v", a.Body)
	}
	return a, s
}
