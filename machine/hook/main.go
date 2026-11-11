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
	"fmt"
	"io"
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
// with them the token.
func client(timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
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
		return "", fmt.Errorf("the metadata server answered %d", resp.StatusCode)
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
	for {
		token, err := metadata(ctx, md, cfg.Metadata, "instance/service-accounts/default/identity?"+q.Encode())
		if err != nil || token == "" {
			time.Sleep(cfg.RetryEvery)
			continue
		}
		code, reason := post(ctx, hc, strings.TrimRight(service, "/")+startedPath, token, body)
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
		}
		time.Sleep(cfg.AskEvery)
	}
}

// post sends the question and returns the answer's status, 0 if there was
// none, and a refusal's reason.
func post(ctx context.Context, hc *http.Client, target, token string, body []byte) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	var answer struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&answer)
	return resp.StatusCode, answer.Reason
}
