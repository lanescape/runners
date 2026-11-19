package conformance

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
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

// Steps are where a cache came from: the restore order's steps, or none.
var Steps = map[string]bool{service.StepPullRequestExact: true, service.StepPullRequestNewest: true, service.StepMainExact: true,
	service.StepMainNewest: true, service.StepBranchExact: true, service.StepBranchNewest: true, service.StepNone: true}

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

// Disk asks for the job's work disk, as the disk action does, once, and
// checks the answer.
func (c *Client) Disk(token string) (Answer, DiskAnswer) {
	c.T.Helper()
	return c.DiskAs(token, map[string]any{})
}

// DiskAs asks for the job's work disk with these settings, as the disk
// action does with its inputs, once, and checks the answer.
func (c *Client) DiskAs(token string, settings map[string]any) (Answer, DiskAnswer) {
	c.T.Helper()
	body, _ := json.Marshal(settings)
	a := c.do(http.MethodPost, "/v1/disks", token, body)
	if a.Status != http.StatusOK {
		return a, DiskAnswer{}
	}
	c.only("a work disk's answer", a.Body, "state", "restored")
	var d DiskAnswer
	d.State, _ = a.Body["state"].(string)
	d.Restored, _ = a.Body["restored"].(string)
	if (d.State != "making" && d.State != "ready") || (d.Restored != service.StepNone && d.Restored != service.Resumed) {
		c.T.Fatalf("a work disk's answer isn't one the interface gives: %v", a.Body)
	}
	return a, d
}

func cacheBody(cache string) []byte {
	b, _ := json.Marshal(map[string]string{"cache": cache})
	return b
}

// RestoreAnswer is the answer to a cache restore request.
type RestoreAnswer struct {
	Restored, Bucket string
	// Objects are the version's objects' names, sizes and generations.
	Objects []CacheObject
	Token   string
}

// CacheObject is one object of a version, as a restore's answer gives it.
type CacheObject struct {
	Name       string
	Size       int64
	CRC32C     string
	Generation int64
}

// Check asks where a restore of a cache would come from, as the cache
// action does with check-only, and checks the answer: a step of the restore
// order, or none, and nothing else.
func (c *Client) Check(token, cache string) (Answer, string) {
	c.T.Helper()
	a := c.do(http.MethodPost, "/v1/caches/check", token, cacheBody(cache))
	if a.Status != http.StatusOK {
		return a, ""
	}
	// Said by its fields' names alone, never their values, so that no token a
	// check wrongly gave is printed.
	found, _ := a.Body["found"].(string)
	if !Steps[found] || len(a.Body) != 1 {
		names := slices.Sorted(maps.Keys(a.Body))
		c.T.Fatalf("a check's answer isn't one the interface gives, {\"found\": <step>}: it has %q, and found is a step: %v", names, Steps[found])
	}
	return a, found
}

// Restore asks to restore a cache, as the cache action does, and checks the
// answer: none, or a version's objects with a token to read them.
func (c *Client) Restore(token, cache string) (Answer, RestoreAnswer) {
	c.T.Helper()
	a := c.do(http.MethodPost, "/v1/caches/restore", token, cacheBody(cache))
	if a.Status != http.StatusOK {
		return a, RestoreAnswer{}
	}
	c.only("a restore's answer", a.Body, "restored", "bucket", "objects", "token", "expires")
	var r RestoreAnswer
	r.Restored, _ = a.Body["restored"].(string)
	if !Steps[r.Restored] {
		c.T.Fatalf("a restore's answer isn't one the interface gives: %v", a.Body)
	}
	if r.Restored == service.StepNone {
		if len(a.Body) != 1 {
			c.T.Fatalf("a restore of nothing gives nothing else: %v", a.Body)
		}
		return a, r
	}
	r.Bucket, _ = a.Body["bucket"].(string)
	r.Token, _ = a.Body["token"].(string)
	exp, _ := a.Body["expires"].(string)
	objs, _ := a.Body["objects"].([]any)
	if r.Bucket == "" || r.Token == "" || exp == "" || len(objs) == 0 {
		c.T.Fatalf("a restore's answer isn't one the interface gives: %v", a.Body)
	}
	for _, o := range objs {
		m, _ := o.(map[string]any)
		c.only("a restore's object", m, "name", "size", "crc32c", "generation")
		name, _ := m["name"].(string)
		size, _ := m["size"].(float64)
		crc, _ := m["crc32c"].(string)
		gen, _ := m["generation"].(float64)
		if name == "" || crc == "" || gen <= 0 {
			c.T.Fatalf("a restore's object isn't one the interface gives: %v", m)
		}
		r.Objects = append(r.Objects, CacheObject{Name: name, Size: int64(size), CRC32C: crc, Generation: int64(gen)})
	}
	return a, r
}

// SaveAnswer is the answer to a cache save request.
type SaveAnswer struct {
	Saves                  bool
	Reason, Bucket, Prefix string
	Token                  string
}

// Save asks to save a cache, as the save-cache action does, and checks the
// answer.
func (c *Client) Save(token, cache string) (Answer, SaveAnswer) {
	c.T.Helper()
	a := c.do(http.MethodPost, "/v1/caches/save", token, cacheBody(cache))
	if a.Status != http.StatusOK {
		return a, SaveAnswer{}
	}
	c.only("a save's answer", a.Body, "saves", "reason", "bucket", "prefix", "token", "expires")
	var s SaveAnswer
	var ok bool
	s.Saves, ok = a.Body["saves"].(bool)
	s.Reason, _ = a.Body["reason"].(string)
	s.Bucket, _ = a.Body["bucket"].(string)
	s.Prefix, _ = a.Body["prefix"].(string)
	s.Token, _ = a.Body["token"].(string)
	exp, _ := a.Body["expires"].(string)
	switch {
	case !ok, s.Saves == (s.Reason != ""):
		c.T.Fatalf("a save's answer isn't one the interface gives: %v", a.Body)
	case s.Saves && (s.Bucket == "" || s.Prefix == "" || s.Token == "" || exp == ""):
		c.T.Fatalf("a save's answer names no bucket, prefix, token or expiry: %v", a.Body)
	case !s.Saves && len(a.Body) != 2:
		c.T.Fatalf("a save in a scope that saves nothing gives nothing else: %v", a.Body)
	}
	return a, s
}

// CommitAnswer is the answer to a cache commit request.
type CommitAnswer struct {
	Objects int
	Bytes   int64
}

// Commit says a cache's version is uploaded, as the save-cache action does,
// and checks the answer.
func (c *Client) Commit(token, cache string) (Answer, CommitAnswer) {
	c.T.Helper()
	a := c.do(http.MethodPost, "/v1/caches/commit", token, cacheBody(cache))
	if a.Status != http.StatusOK {
		return a, CommitAnswer{}
	}
	c.only("a commit's answer", a.Body, "objects", "bytes")
	n, ok1 := a.Body["objects"].(float64)
	b, ok2 := a.Body["bytes"].(float64)
	if !ok1 || !ok2 || n < 1 {
		c.T.Fatalf("a commit's answer isn't one the interface gives: %v", a.Body)
	}
	return a, CommitAnswer{Objects: int(n), Bytes: int64(b)}
}
