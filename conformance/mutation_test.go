package conformance_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/lanescape/runners/conformance"
	"github.com/lanescape/runners/emulators/service"
)

// recorder is a conformance.T that records a case's failure rather than
// failing the test.
type recorder struct {
	mu     sync.Mutex
	failed bool
	first  string
}

func (r *recorder) Helper()             {}
func (r *recorder) Logf(string, ...any) {}
func (r *recorder) fail(f string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.failed {
		r.failed, r.first = true, fmt.Sprintf(f, a...)
	}
}
func (r *recorder) Errorf(f string, a ...any) { r.fail(f, a...) }
func (r *recorder) Fatalf(f string, a ...any) { r.fail(f, a...); runtime.Goexit() }

// diverging is the emulator behind a proxy that changes its answers in one
// way, as a divergence of either side would.
type diverging struct {
	service.World
	url string
}

func (d diverging) URL() string { return d.url }

// A mutation changes one answer of the emulator's: the request's path, its
// status and body, and returns the new status and body.
type mutation func(path string, status int, body map[string]any, h http.Header) (int, map[string]any)

func newDiverging(t *testing.T, m mutation) service.World {
	e := service.New()
	t.Cleanup(e.Close)
	target, _ := url.Parse(e.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if !strings.HasPrefix(resp.Request.URL.Path, "/v1/") {
			return nil
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		var body map[string]any
		if json.Unmarshal(raw, &body) == nil {
			code, changed := m(resp.Request.URL.Path, resp.StatusCode, body, resp.Header)
			resp.StatusCode = code
			raw, _ = json.Marshal(changed)
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		resp.ContentLength = int64(len(raw))
		resp.Header.Set("Content-Length", strconv.Itoa(len(raw)))
		return nil
	}
	srv := httptest.NewServer(proxy)
	t.Cleanup(srv.Close)
	// Tokens name the emulator's own address as their audience; the
	// proxy only relays them.
	return diverging{World: e, url: srv.URL}
}

func entry(body map[string]any) map[string]any { m, _ := body["entry"].(map[string]any); return m }

// Every deliberate divergence from the service's answers fails the suite:
// the suite's cases pass against the emulator, and each of these changes to
// its answers makes at least one fail.
func TestADivergenceFailsTheSuite(t *testing.T) {
	mutations := map[string]mutation{
		"a step misnamed": func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
			if e := entry(b); e != nil && e["step"] == "main_exact" {
				e["step"] = "main_newest"
			}
			return c, b
		},
		"a pull request's run bound read": func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
			if s, _ := b["scope"].(string); strings.HasPrefix(s, "pr") {
				b["scope"] = "read"
			}
			return c, b
		},
		"a long-lived branch's push bound main": func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
			if b["scope"] == "branch-v1" {
				b["scope"] = "main"
			}
			return c, b
		},
		"nothing restored": func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
			if p == "/v1/cache/restore" && c == http.StatusOK {
				b["entry"] = nil
			}
			return c, b
		},
		"a refusal answered as retryable": func(p string, c int, b map[string]any, h http.Header) (int, map[string]any) {
			if c == http.StatusForbidden && strings.HasPrefix(p, "/v1/cache/") {
				h.Set("Retry-After", "5")
				return http.StatusServiceUnavailable, map[string]any{"retryable": true}
			}
			return c, b
		},
		"a 503 without a Retry-After": func(p string, c int, b map[string]any, h http.Header) (int, map[string]any) {
			h.Del("Retry-After")
			return c, b
		},
		"a refused commit accepted": func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
			if p == "/v1/cache/commit" && c == http.StatusForbidden {
				return http.StatusOK, map[string]any{"state": "pending"}
			}
			return c, b
		},
		"a read run allowed to save": func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
			if p == "/v1/cache/save" && c == http.StatusForbidden && strings.Contains(fmt.Sprint(b["reason"]), "read") {
				return http.StatusOK, map[string]any{"id": strings.Repeat("0", 32), "url": "http://127.0.0.1:1/x",
					"headers": map[string]any{"x-goog-resumable": "start"}, "expires": "2099-01-01T00:00:00Z"}
			}
			return c, b
		},
		"a lookup handing out a URL": func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
			if e := entry(b); e != nil && e["url"] == nil {
				e["url"], e["expires"] = "http://127.0.0.1:1/x", "2099-01-01T00:00:00Z"
			}
			return c, b
		},
	}
	for name, m := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var failed []string
			for _, c := range conformance.Cases() {
				r := &recorder{}
				done := make(chan struct{})
				go func() {
					defer close(done)
					c.Run(r, newDiverging(t, m))
				}()
				<-done
				if r.failed {
					failed = append(failed, c.Name)
				}
			}
			if len(failed) == 0 {
				t.Fatal("the suite passed against a diverging emulator")
			}
			t.Logf("failed %d cases: %s", len(failed), strings.Join(failed, "; "))
		})
	}
}

// heads is the emulator behind a proxy that binds a bootstrap's run read
// unless its event's head commit is its pull request's head now, as the
// emulator once did, where specs/0002-machines.md asks only that it be one
// of the pull request's commits.
type heads struct {
	service.World
	url   string
	mu    sync.Mutex
	heads map[int]string
}

func (h *heads) URL() string { return h.url }

func (h *heads) record(pr service.PullRequest) service.PullRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.heads[pr.Number] = pr.HeadSHA
	return pr
}

func (h *heads) PullRequest(r service.Repository, base, head string) service.PullRequest {
	return h.record(h.World.PullRequest(r, base, head))
}

func (h *heads) ForkPullRequest(r service.Repository, base string) service.PullRequest {
	return h.record(h.World.ForkPullRequest(r, base))
}

func (h *heads) PushPullRequest(pr service.PullRequest) service.PullRequest {
	return h.record(h.World.PushPullRequest(pr))
}

func newHeads(t *testing.T) service.World {
	e := service.New()
	t.Cleanup(e.Close)
	h := &heads{World: e, heads: map[int]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, e.URL()+r.URL.RequestURI(), bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var ev conformance.PullRequestEvent
		var answer map[string]any
		if r.URL.Path == "/v1/bind" && json.Unmarshal(body, &ev) == nil && ev.HeadSHA != "" && json.Unmarshal(raw, &answer) == nil {
			h.mu.Lock()
			now := h.heads[ev.PullRequest]
			h.mu.Unlock()
			if s, _ := answer["scope"].(string); strings.HasPrefix(s, "pr") && now != ev.HeadSHA {
				answer["scope"] = "read"
				raw, _ = json.Marshal(answer)
			}
		}
		for k, vs := range resp.Header {
			w.Header()[k] = vs
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

// The suite fails an emulator that binds a bootstrap's run only for its
// pull request's head now.
func TestABootstrapHeldToItsPullRequestsHeadFailsTheSuite(t *testing.T) {
	var failed []string
	for _, c := range conformance.Cases() {
		r := &recorder{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(r, newHeads(t))
		}()
		<-done
		if r.failed {
			failed = append(failed, c.Name)
		}
	}
	if len(failed) == 0 {
		t.Fatal("the suite passed against an emulator that binds read for a pull request pushed to after its approval")
	}
	t.Logf("failed %d cases: %s", len(failed), strings.Join(failed, "; "))
}
