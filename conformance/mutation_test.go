package conformance_test

import (
	"bytes"
	"encoding/json"
	"errors"
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

// Every deliberate divergence from the service's answers fails the suite:
// the suite's cases pass against the emulator, and each of these changes to
// its answers makes at least one fail.
func TestADivergenceFailsTheSuite(t *testing.T) {
	mutations := map[string]func() mutation{
		"a work disk in another scope's restore chain": func() mutation {
			return func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
				if p == "/v1/disks" && b["restored"] == "none" {
					b["restored"] = "main_exact"
				}
				return c, b
			}
		},
		"a work disk from a pull request's own disk answered as main's": func() mutation {
			return func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
				if p == "/v1/disks" && b["restored"] == "pull_request_exact" {
					b["restored"] = "main_exact"
				}
				return c, b
			}
		},
		"a work disk from another step of the restore order": func() mutation {
			return func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
				if p == "/v1/disks" && b["restored"] == "main_exact" {
					b["restored"] = "main_newest"
				}
				return c, b
			}
		},
		"a work disk refused answered": func() mutation {
			return func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
				if p == "/v1/disks" && c == http.StatusForbidden {
					return http.StatusOK, map[string]any{"state": "ready", "restored": "none"}
				}
				return c, b
			}
		},
		"a disk being made answered ready": func() mutation {
			return func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
				if p == "/v1/disks" && b["state"] == "making" {
					b["state"] = "ready"
				}
				return c, b
			}
		},
		"a save in a scope that saves nothing answered as saving": func() mutation {
			return func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
				if p == "/v1/disks/save" && b["saves"] == false {
					return c, map[string]any{"saves": true}
				}
				return c, b
			}
		},
		"a save without a work disk answered": func() mutation {
			return func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
				if p == "/v1/disks/save" && c == http.StatusForbidden {
					return http.StatusOK, map[string]any{"saves": true}
				}
				return c, b
			}
		},
		"an answer with a field the interface doesn't give": func() mutation {
			return func(p string, c int, b map[string]any, _ http.Header) (int, map[string]any) {
				if c == http.StatusOK {
					b["scope"] = "main"
				}
				return c, b
			}
		},
		"a refusal answered as retryable": func() mutation {
			return func(p string, c int, b map[string]any, h http.Header) (int, map[string]any) {
				if c == http.StatusForbidden {
					h.Set("Retry-After", "5")
					return http.StatusServiceUnavailable, map[string]any{"retryable": true}
				}
				return c, b
			}
		},
		"a refusal without its reason": func() mutation {
			return func(p string, c int, b map[string]any, h http.Header) (int, map[string]any) {
				if c == http.StatusForbidden {
					delete(b, "reason")
				}
				return c, b
			}
		},
		"a 503 without a Retry-After": func() mutation {
			return func(p string, c int, b map[string]any, h http.Header) (int, map[string]any) {
				h.Del("Retry-After")
				return c, b
			}
		},
	}
	for name, m := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			failsSomeCase(t, func(t *testing.T) service.World { return newDiverging(t, m()) })
		})
	}
}

// failsSomeCase checks that at least one of the suite's cases fails against
// the worlds newWorld makes.
func failsSomeCase(t *testing.T, newWorld func(t *testing.T) service.World) {
	var failed []string
	for _, c := range conformance.Cases() {
		r := &recorder{}
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(r, newWorld(t))
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
}

// binds is the emulator with its bindings changed in one way, as a
// divergence of its binding rules would.
type binds struct {
	service.World
	change func(scope string) string
}

func (b binds) Binding(r service.Run) (string, bool) {
	s, ok := b.World.Binding(r)
	if ok {
		s = b.change(s)
	}
	return s, ok
}

// forks is the emulator giving a fork's pull_request run a machine.
type forks struct{ service.World }

func (f forks) StartJob(r service.Run, selector string) (service.Job, error) {
	j, err := f.World.StartJob(r, selector)
	var refused *service.Refused
	if r.Event == "pull_request" && errors.As(err, &refused) {
		return service.Job{Run: r}, nil
	}
	return j, err
}

// failing is the emulator giving a job it should fail a machine.
type failing struct{ service.World }

func (f failing) StartJob(r service.Run, selector string) (service.Job, error) {
	j, err := f.World.StartJob(r, selector)
	var refused *service.Refused
	if errors.As(err, &refused) && !strings.Contains(refused.Reason, "fork") {
		return service.Job{Run: r}, nil
	}
	return j, err
}

// unsaved is the emulator never saving a finished job's work disk.
type unsaved struct{ service.World }

func (u unsaved) FinishJob(j service.Job, conclusion string) { u.World.FinishJob(j, "failure") }

// Every deliberate divergence of the binding rules, or of what a job's end
// saves, fails the suite.
func TestABindingDivergenceFailsTheSuite(t *testing.T) {
	for name, change := range map[string]func(string) string{
		"a pull request's run bound read": func(s string) string {
			if strings.HasPrefix(s, "pr") {
				return "read"
			}
			return s
		},
		"a long-lived branch's push bound main": func(s string) string {
			if s == "branch-v1" {
				return "main"
			}
			return s
		},
		"a read run bound main": func(s string) string {
			if s == "read" {
				return "main"
			}
			return s
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			failsSomeCase(t, func(t *testing.T) service.World {
				e := service.New()
				t.Cleanup(e.Close)
				return binds{World: e, change: change}
			})
		})
	}
	for name, wrap := range map[string]func(service.World) service.World{
		"a fork's pull request's job given a machine": func(w service.World) service.World { return forks{w} },
		"a job the service fails given a machine":     func(w service.World) service.World { return failing{w} },
		"a finished job's work disk never saved":      func(w service.World) service.World { return unsaved{w} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			failsSomeCase(t, func(t *testing.T) service.World {
				e := service.New()
				t.Cleanup(e.Close)
				return wrap(e)
			})
		})
	}
}
