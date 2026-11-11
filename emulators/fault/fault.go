// Package fault makes an emulator fail requests as the real service does:
// before doing the work, after doing it with the response lost, slowly, or by
// dropping the connection.
package fault

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rule says which requests fail, and how.
type Rule struct {
	// Method and PathPrefix select requests; empty matches any. PathPrefix is
	// compared with the request's unescaped path.
	Method     string
	PathPrefix string
	// Match, if set, must also return true.
	Match func(*http.Request) bool
	// Times is how many matching requests fail; 0 means every one until Reset.
	Times int
	// Delay holds the request this long first, or until the caller gives up.
	Delay time.Duration
	// After does the work and then loses the response, as when a connection
	// breaks after the service has committed a change.
	After bool
	// Respond writes the failure. Nil with a Delay just delays the request.
	Respond func(http.ResponseWriter, *http.Request)
}

// Injector holds rules and applies them to a handler.
type Injector struct {
	mu    sync.Mutex
	rules []*rule
}

type rule struct {
	Rule
	left int
}

// Add installs a rule. Rules apply in the order added; the first that matches
// a request is the one used.
func (in *Injector) Add(r Rule) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.rules = append(in.rules, &rule{Rule: r, left: r.Times})
}

// Reset removes every rule.
func (in *Injector) Reset() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.rules = nil
}

func (in *Injector) take(r *http.Request) *Rule {
	in.mu.Lock()
	defer in.mu.Unlock()
	for i, ru := range in.rules {
		if ru.Method != "" && ru.Method != r.Method {
			continue
		}
		if ru.PathPrefix != "" && !strings.HasPrefix(r.URL.Path, ru.PathPrefix) {
			continue
		}
		if ru.Match != nil && !ru.Match(r) {
			continue
		}
		if ru.Times > 0 {
			ru.left--
			if ru.left == 0 {
				in.rules = append(in.rules[:i:i], in.rules[i+1:]...)
			}
		}
		chosen := ru.Rule
		return &chosen
	}
	return nil
}

// Wrap applies the injector's rules in front of next.
func (in *Injector) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ru := in.take(r)
		if ru == nil {
			next.ServeHTTP(w, r)
			return
		}
		if ru.Delay > 0 {
			// Go's server notices a caller hanging up only once the body
			// has been read, so read it first and hand it on.
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			t := time.NewTimer(ru.Delay)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				return
			}
		}
		if ru.Respond == nil {
			next.ServeHTTP(w, r)
			return
		}
		if ru.After {
			next.ServeHTTP(httptest.NewRecorder(), r)
		}
		ru.Respond(w, r)
	})
}

// Status answers with a fixed status, headers and body.
func Status(code int, header http.Header, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if body != "" && w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

// Drop closes the connection without an answer.
func Drop(w http.ResponseWriter, _ *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("fault: response writer can't be hijacked")
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// Refuse answers as this repository's service does when it won't do what a
// request asks (specs/0005-interface.md, Requests from a job): 403, with
// the reason, which the caller doesn't retry.
func Refuse(reason string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, http.StatusForbidden, map[string]any{"error": http.StatusText(http.StatusForbidden), "reason": reason})
	}
}

// Unavailable answers as this repository's service does when GitHub
// couldn't confirm an answer: 503, retryable, with a Retry-After of
// retryAfter, rounded down to whole seconds and at least one.
func Unavailable(retryAfter time.Duration) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter/time.Second))))
		answerJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no confirmed answer; try again", "retryable": true})
	}
}

func answerJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
