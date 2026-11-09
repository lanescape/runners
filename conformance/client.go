package conformance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"
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

// Restored is a restore's entry; nil for none.
type Restored struct {
	Scope   string
	Key     string
	Step    string
	Size    int64
	SHA256  string
	URL     string
	Expires time.Time
}

// Restore is POST /v1/cache/restore. A 200 is checked, and its entry
// returned, nil for none.
func (c *Client) Restore(token, platform, family, key string, lookup bool) (Answer, *Restored) {
	c.T.Helper()
	q := map[string]any{"family": family, "key": key, "platform": platform}
	if lookup {
		q["lookup"] = true
	}
	return c.RestoreRaw(token, marshal(q), lookup)
}

var (
	stepPattern   = regexp.MustCompile(`^(pull_request|main|branch)_(exact|newest)$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// RestoreRaw is POST /v1/cache/restore with any body.
func (c *Client) RestoreRaw(token string, body []byte, lookup bool) (Answer, *Restored) {
	c.T.Helper()
	a := c.post("/v1/cache/restore", token, body)
	if a.Status != http.StatusOK {
		return a, nil
	}
	raw, ok := a.Body["entry"]
	if !ok {
		c.T.Fatalf("restore answered 200 with no entry: %v", a.Body)
	}
	if raw == nil {
		return a, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		c.T.Fatalf("restore's entry isn't an object: %v", raw)
	}
	r := &Restored{}
	r.Scope, _ = m["scope"].(string)
	r.Key, _ = m["key"].(string)
	r.Step, _ = m["step"].(string)
	r.SHA256, _ = m["sha256"].(string)
	size, _ := m["size"].(float64)
	r.Size = int64(size)
	r.URL, _ = m["url"].(string)
	if !scopePattern.MatchString(r.Scope) || !keyPattern.MatchString(r.Key) || !stepPattern.MatchString(r.Step) ||
		!sha256Pattern.MatchString(r.SHA256) || size < 0 {
		c.T.Fatalf("restore's entry isn't the interface's: %v", m)
	}
	exp, _ := m["expires"].(string)
	if lookup {
		if r.URL != "" || exp != "" {
			c.T.Fatalf("a lookup's entry has a url or expires: %v", m)
		}
		return a, r
	}
	t, err := time.Parse(time.RFC3339, exp)
	if r.URL == "" || err != nil {
		c.T.Fatalf("a restore's entry has no url or expires: %v", m)
	}
	r.Expires = t
	return a, r
}

// Reserved is a save's reservation.
type Reserved struct {
	ID      string
	URL     string
	Headers map[string]string
	Expires time.Time
}

var (
	familyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	keyPattern    = regexp.MustCompile(`^[0-9a-f]{20}$`)
)

// Save is POST /v1/cache/save.
func (c *Client) Save(token, platform, family, key string) (Answer, *Reserved) {
	c.T.Helper()
	a := c.post("/v1/cache/save", token, marshal(map[string]any{"family": family, "key": key, "platform": platform}))
	if a.Status != http.StatusOK {
		return a, nil
	}
	r := &Reserved{Headers: map[string]string{}}
	r.ID, _ = a.Body["id"].(string)
	r.URL, _ = a.Body["url"].(string)
	hs, _ := a.Body["headers"].(map[string]any)
	for k, v := range hs {
		s, ok := v.(string)
		if !ok {
			c.T.Fatalf("save's header %s isn't a string", k)
		}
		r.Headers[k] = s
	}
	exp, _ := a.Body["expires"].(string)
	t, err := time.Parse(time.RFC3339, exp)
	if r.ID == "" || r.URL == "" || len(r.Headers) == 0 || err != nil {
		c.T.Fatalf("save answered 200 without id, url, headers and expires: %v", a.Body)
	}
	r.Expires = t
	return a, r
}

// Commit is POST /v1/cache/commit.
func (c *Client) Commit(token, id string, size int64, sum string) Answer {
	c.T.Helper()
	a := c.post("/v1/cache/commit", token, marshal(map[string]any{"id": id, "size": size, "sha256": sum}))
	if a.Status == http.StatusOK && a.Body["state"] != "pending" {
		c.T.Fatalf("commit answered 200 with state %v", a.Body["state"])
	}
	return a
}

// Upload starts a resumable upload with a reservation's URL and headers,
// and sends data to its session URI in chunks of chunk bytes, as Cloud
// Storage's resumable uploads take them. If interrupt, it loses its first
// chunk's answer, asks the session how far it has got, and resumes from
// there.
func (c *Client) Upload(r *Reserved, data []byte, chunk int, interrupt bool) {
	c.T.Helper()
	req, _ := http.NewRequest(http.MethodPost, r.URL, nil)
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		c.T.Fatalf("starting an upload: %v", err)
	}
	resp.Body.Close()
	session := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK || session == "" {
		c.T.Fatalf("starting an upload answered %d", resp.StatusCode)
	}
	put := func(start int, part []byte, last bool) int {
		total := "*"
		if last {
			total = strconv.Itoa(len(data))
		}
		req, _ := http.NewRequest(http.MethodPut, session, bytes.NewReader(part))
		if len(part) == 0 {
			req.Header.Set("Content-Range", "bytes */"+total)
		} else {
			req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%s", start, start+len(part)-1, total))
		}
		resp, err := c.http().Do(req)
		if err != nil {
			c.T.Fatalf("uploading: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusPermanentRedirect {
			var n int
			if rng := resp.Header.Get("Range"); rng != "" {
				var a int
				if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &a, &n); err != nil {
					c.T.Fatalf("uploading: Range %q", rng)
				}
				n++
			}
			return n
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			c.T.Fatalf("uploading answered %d", resp.StatusCode)
		}
		return len(data)
	}
	sent := 0
	if len(data) == 0 {
		put(0, nil, true)
		return
	}
	for sent < len(data) {
		end := min(sent+chunk, len(data))
		got := put(sent, data[sent:end], end == len(data))
		if interrupt {
			// The answer was lost: ask how far the upload has got.
			interrupt = false
			got = put(0, nil, false)
		}
		sent = got
	}
}

// Download reads a restored entry through its URL, in ranges of chunk
// bytes, as a job does, and checks its size and SHA-256 before returning
// it.
func (c *Client) Download(r *Restored, chunk int64) []byte {
	c.T.Helper()
	var out []byte
	for int64(len(out)) < r.Size {
		req, _ := http.NewRequest(http.MethodGet, r.URL, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", len(out), min(int64(len(out))+chunk, r.Size)-1))
		resp, err := c.http().Do(req)
		if err != nil {
			c.T.Fatalf("downloading: %v", err)
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusPartialContent {
			c.T.Fatalf("downloading answered %d", resp.StatusCode)
		}
		out = append(out, b...)
	}
	if int64(len(out)) != r.Size || Sum(out) != r.SHA256 {
		c.T.Fatalf("the restored entry's size or SHA-256 isn't what restore said")
	}
	return out
}

// Get is a plain request of a URL the service handed out, with method, and
// its status.
func (c *Client) Get(method, url string) int {
	c.T.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := c.http().Do(req)
	if err != nil {
		c.T.Fatalf("%s: %v", method, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Sum is data's SHA-256, as the interface writes it.
func Sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}
