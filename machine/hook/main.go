// Command job-started is the job-started hook's program (specs/0002-machines.md,
// The job-started check). GitHub's runner runs the hook in the job's
// environment, before the job's first step, so it decides nothing: it asks
// the service whether the job on this machine's runner may run here, signed
// with the machine's identity token, with what GitHub told the runner about
// the job, and exits once the service allows it. If the service refuses,
// or fails the job, saying why, it prints the reason and never exits: the
// service stops the machine, and the job's steps never run. It mounts
// nothing: a work disk comes later, if the job's steps ask for one.
//
// Its settings come from the base image; the only argument, which the
// runner never passes, is another settings file, for tests.
//
// It uses only Go's standard library, so that it builds from its own source
// alone, statically, wherever the base image is made.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Config is the hook's settings.
type Config struct {
	// Metadata is the metadata server's address.
	Metadata string
	// AskEvery and RetryEvery are how long it waits before asking again
	// after an answer it can't use, and after failing to get a token or
	// the service's address.
	AskEvery   time.Duration
	RetryEvery time.Duration
}

// Defaults are the base image's.
func Defaults() Config {
	return Config{Metadata: "http://metadata.google.internal", AskEvery: 5 * time.Second, RetryEvery: 2 * time.Second}
}

const startedPath = "/v1/jobs/started"

func main() {
	quiet()
	settings := "/opt/lanescape-runners/job-started.conf"
	if len(os.Args) > 1 {
		settings = os.Args[1]
	}
	cfg := Defaults()
	readSettings(settings, &cfg)
	if !ask(context.Background(), cfg, os.Getenv, os.Stderr) {
		hold()
	}
}

// quiet stops Go's own output that could print what the hook knows or was
// sent. GODEBUG's DNS tracing, which names the service's host, is read at
// the first lookup, so unsetting GODEBUG before any stops it. The HTTP
// client logs the start of any answer it didn't ask for, which could be
// part of a token, through the standard logger, which is discarded. HTTP/2
// tracing is read before main runs, and is kept out by the client never
// speaking HTTP/2 (client).
func quiet() {
	_ = os.Unsetenv("GODEBUG")
	log.SetOutput(io.Discard)
}

// readSettings reads key=value lines; an unreadable file leaves the
// defaults.
func readSettings(path string, cfg *Config) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "metadata":
			cfg.Metadata = v
		case "ask_every":
			if d, err := time.ParseDuration(v); err == nil {
				cfg.AskEvery = d
			}
		case "retry_every":
			if d, err := time.ParseDuration(v); err == nil {
				cfg.RetryEvery = d
			}
		}
	}
}

// hold never returns: the machine is stopping.
func hold() {
	for {
		time.Sleep(time.Hour)
	}
}

// Request is what the hook asks with: what GitHub told the runner about
// the job, which the service binds the job's run from, on a fresh
// machine's first check, once GitHub's records confirm it
// (specs/0002-machines.md, Binding a run). The run is only where the
// service looks first; it finds the job from GitHub's records.
type Request struct {
	RunID            int64  `json:"run_id"`
	Event            string `json:"event"`
	Ref              string `json:"ref"`
	RefType          string `json:"ref_type"`
	SHA              string `json:"sha"`
	PullRequest      int    `json:"pull_request,omitempty"`
	HeadRepositoryID int64  `json:"head_repository_id,omitempty"`
	HeadSHA          string `json:"head_sha,omitempty"`
}

// request is the hook's question, from the job's environment and its
// event's payload. A run ID that isn't a number is sent as 0.
func request(getenv func(string) string) Request {
	r := Request{Event: getenv("GITHUB_EVENT_NAME"), Ref: getenv("GITHUB_REF"), RefType: getenv("GITHUB_REF_TYPE"), SHA: getenv("GITHUB_SHA")}
	if id, err := strconv.ParseInt(getenv("GITHUB_RUN_ID"), 10, 64); err == nil && id > 0 {
		r.RunID = id
	}
	if p := getenv("GITHUB_EVENT_PATH"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			var ev struct {
				PullRequest struct {
					Number int `json:"number"`
					Head   struct {
						SHA  string `json:"sha"`
						Repo struct {
							ID int64 `json:"id"`
						} `json:"repo"`
					} `json:"head"`
				} `json:"pull_request"`
			}
			if json.Unmarshal(b, &ev) == nil {
				r.PullRequest, r.HeadRepositoryID, r.HeadSHA = ev.PullRequest.Number, ev.PullRequest.Head.Repo.ID, ev.PullRequest.Head.SHA
			}
		}
	}
	return r
}

// client goes straight to its address, whatever the job's environment says
// of proxies, and speaks HTTP/1.1 alone, so that nothing the environment
// sets, such as GODEBUG's HTTP/2 tracing, can print a request's headers and
// with them the token. It offers only HTTP/1.1 in the TLS handshake: a
// server that prefers HTTP/2, as Cloud Run's front end does, would otherwise
// choose it, and a client that can't speak it would fail every request.
func client(timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	t.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	return &http.Client{Timeout: timeout, Transport: t}
}

func say(out io.Writer, format string, args ...any) {
	fmt.Fprintf(out, "lanescape-runners: "+format+"\n", args...)
}

// metadata reads the metadata server.
func metadata(ctx context.Context, hc *http.Client, base, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/computeMetadata/v1/"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", statusError(resp.StatusCode)
	}
	return strings.TrimSpace(string(b)), nil
}

// printable keeps a reason to one line of printable text.
func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, s)
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}

// ask asks the service until it answers, and reports whether the job may
// run. It never prints the token, an error that could hold it, or the
// service's answer beyond its reason.
func ask(ctx context.Context, cfg Config, getenv func(string) string, out io.Writer) bool {
	md := client(10 * time.Second)
	var service string
	for {
		s, err := metadata(ctx, md, cfg.Metadata, "instance/attributes/lsr-service")
		if err == nil && s != "" {
			service = s
			break
		}
		time.Sleep(cfg.RetryEvery)
	}
	body, _ := json.Marshal(request(getenv))
	hc := client(2 * time.Minute)
	say(out, "asking the service whether this job may run here")
	q := url.Values{"audience": {service}, "format": {"full"}}
	// Why it's asking again is said once each time it changes, so that a
	// hook that never gets an answer shows why in the job's log.
	last := ""
	again := func(why string) {
		if why != last {
			say(out, "asking again: %s", printable(why))
			last = why
		}
	}
	for {
		token, err := metadata(ctx, md, cfg.Metadata, "instance/service-accounts/default/identity?"+q.Encode())
		if err != nil || token == "" {
			why := "it was empty"
			if err != nil {
				why = describe(err)
			}
			again("no identity token yet: " + why)
			time.Sleep(cfg.RetryEvery)
			continue
		}
		code, reason, failed := post(ctx, hc, strings.TrimRight(service, "/")+startedPath, token, body)
		switch code {
		case http.StatusOK:
			say(out, "the service allows this job here")
			return true
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			// A job the service fails is told why, in its log.
			if reason != "" {
				say(out, "this job can't run: %s. The service is cancelling its run and stopping this machine", printable(reason))
			} else {
				say(out, "the service refused this job on this machine, which is stopping")
			}
			return false
		case 0:
			again("the service couldn't be reached: " + failed)
		default:
			again(fmt.Sprintf("the service answered %d", code))
		}
		time.Sleep(cfg.AskEvery)
	}
}

// describe says why a request failed in words of its own, never the
// error's text: an error can hold an address, a URL from a redirect, or
// part of a malformed answer, which may be a token.
func describe(err error) string {
	var status statusError
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var op *net.OpError
	var ne net.Error
	switch {
	case errors.As(err, &status):
		return fmt.Sprintf("the metadata server answered %d", int(status))
	case errors.As(err, &dns):
		return "the name didn't resolve"
	case errors.As(err, &cert):
		return "the certificate wasn't trusted"
	case errors.As(err, &record), errors.Is(err, http.ErrSchemeMismatch):
		return "the answer wasn't TLS"
	case errors.As(err, &ne) && ne.Timeout():
		return "the request timed out"
	case errors.As(err, &op) && op.Op == "dial":
		return "no connection could be made"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &op):
		return "the connection was closed"
	}
	return "the answer couldn't be read"
}

// statusError is a metadata server's answer other than 200.
type statusError int

func (s statusError) Error() string { return fmt.Sprintf("the metadata server answered %d", int(s)) }

// post sends the question and returns the answer's status, 0 if there was
// none, with why (describe); and a refusal's reason.
func post(ctx context.Context, hc *http.Client, target, token string, body []byte) (int, string, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, "", "the request couldn't be made"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", describe(err)
	}
	defer resp.Body.Close()
	var answer struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&answer)
	return resp.StatusCode, answer.Reason, ""
}
