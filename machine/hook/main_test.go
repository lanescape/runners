package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The hook offers only HTTP/1.1 in the TLS handshake. A server that prefers
// HTTP/2, as Cloud Run's front end does, chooses it whenever it's offered,
// and the hook's client can't speak it: every question would fail before
// reaching the service.
func TestTheHookOffersOnlyHTTP1(t *testing.T) {
	offered := make(chan []string, 1)
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		select {
		case offered <- slices.Clone(hello.SupportedProtos):
		default:
		}
		return nil, nil
	}}
	srv.StartTLS()
	defer srv.Close()

	// The hook doesn't trust the test's certificate, so the handshake fails
	// after the offer is seen.
	_, _, _ = post(context.Background(), client(10*time.Second), srv.URL+startedPath, "a.b.c", []byte("{}"))
	select {
	case got := <-offered:
		if !slices.Equal(got, []string{"http/1.1"}) {
			t.Fatalf("the hook offered %q, want only http/1.1", got)
		}
	default:
		t.Fatal("the hook never reached the server")
	}
}

// Why there was no answer is said in the hook's own words: never the
// error's text, which can hold the service's address or part of a
// malformed answer.
func TestTheHookSaysWhyThereWasNoAnswer(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "secret.token.value garbage\r\n\r\n")
			_ = c.Close()
		}
	}()
	garbled := "http://" + ln.Addr().String()
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	notTLS := strings.Replace(plain.URL, "http://", "https://", 1)

	for _, tc := range []struct{ target, want string }{
		{closedURL, "no connection could be made"},
		{garbled, "the answer couldn't be read"},
		{notTLS, "the answer wasn't TLS"},
	} {
		code, _, failed := post(context.Background(), client(5*time.Second), tc.target+startedPath, "a.b.c", []byte("{}"))
		if code != 0 || failed != tc.want {
			t.Errorf("post to %s = %d, %q, want 0, %q", tc.target, code, failed, tc.want)
		}
	}
}

// A metadata server's malformed answer is never repeated.
func TestTheHookNeverRepeatsAMetadataAnswer(t *testing.T) {
	for _, err := range []error{statusError(404), errors.New("secret.token.value")} {
		if got := describe(err); strings.Contains(got, "secret") {
			t.Fatalf("describe(%v) = %q", err, got)
		}
	}
	if got := describe(statusError(404)); got != "the metadata server answered 404" {
		t.Fatalf("describe = %q", got)
	}
}

// TestMain runs the hook itself when a test starts this binary as the hook.
func TestMain(m *testing.M) {
	if os.Getenv("LSR_HOOK_TEST_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// Nothing the job's environment turns on, and nothing a metadata server
// sends beyond its answer, reaches the job's log: not Go's DNS tracing,
// which names the service's host, nor the HTTP client's log of an answer
// it didn't ask for, which could be part of a token.
func TestTheHookPrintsNothingOfGosOwn(t *testing.T) {
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "lsr-service") {
			_, _ = io.WriteString(w, "http://service-address.invalid")
			return
		}
		// An answer longer than it says it is: the rest arrives on an
		// idle connection.
		c, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\na.b.csecret.token.value")
		_ = buf.Flush()
		time.Sleep(200 * time.Millisecond)
	}))
	defer md.Close()
	settings := filepath.Join(t.TempDir(), "job-started.conf")
	if err := os.WriteFile(settings, []byte("metadata="+md.URL+"\nask_every=100ms\nretry_every=100ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], settings)
	cmd.Env = []string{"LSR_HOOK_TEST_MAIN=1", "GODEBUG=netdns=go+2,http2debug=2", "PATH=" + os.Getenv("PATH")}
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	// It has had its token, with the rest of the answer, and looked the
	// service up, once it says the name didn't resolve; the client's log of
	// the rest comes as the next request finds the connection.
	tried := "asking again: the service couldn't be reached: the name didn't resolve"
	for end := time.Now().Add(30 * time.Second); !strings.Contains(out.String(), tried); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("the hook never tried the service:\n%s", out)
		}
	}
	time.Sleep(time.Second)
	got := out.String()
	for _, leak := range []string{"secret", "service-address", "Unsolicited", "go package net"} {
		if strings.Contains(got, leak) {
			t.Fatalf("the hook printed %q:\n%s", leak, got)
		}
	}
}

// syncBuffer is a buffer a subprocess writes to while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
