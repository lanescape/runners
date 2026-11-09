package service_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/conformance"
	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/emulators/oidc"
	"github.com/lanescape/runners/emulators/service"
)

const (
	linux = "linux-x64"
	key   = "0123456789abcdef0123"
)

func setup(t *testing.T) (*service.Emulator, *clockwork.FakeClock, *conformance.Client, service.Repository) {
	t.Helper()
	clock := clockwork.NewFakeClockAt(time.Now())
	issuer := oidc.New(oidc.WithClock(clock))
	t.Cleanup(issuer.Close)
	e := service.New(service.WithIssuer(issuer), service.WithClock(clock))
	t.Cleanup(e.Close)
	return e, clock, &conformance.Client{T: t, URL: e.URL()}, e.Repository("app")
}

// saved has a push to main save data, and returns a later run's restore.
func saved(t *testing.T, e *service.Emulator, c *conformance.Client, repo service.Repository, data []byte) *conformance.Restored {
	t.Helper()
	run := e.Push(repo, "main")
	j := e.StartMachineJob(run)
	tok := e.Token(j, nil)
	if a := c.Bind(tok, nil); a.Scope() != "main" {
		t.Fatalf("bound %q", a.Scope())
	}
	_, r := c.Save(tok, linux, "build", key)
	c.Upload(r, data, 1<<20, false)
	c.Commit(tok, r.ID, int64(len(data)), conformance.Sum(data))
	e.FinishJob(j, "success")
	next := e.StartMachineJob(e.Push(repo, "main"))
	tok = e.Token(next, nil)
	c.Bind(tok, nil)
	_, got := c.Restore(tok, service.ServiceStore, linux, "build", key, false)
	if got == nil {
		t.Fatal("nothing restored")
	}
	return got
}

func TestURLsExpire(t *testing.T) {
	e, clock, c, repo := setup(t)
	got := saved(t, e, c, repo, []byte("data"))
	if d := got.Expires.Sub(clock.Now()); d > service.RestoreFor || d < service.RestoreFor-time.Second {
		t.Fatalf("expires %v", got.Expires)
	}
	if code := c.Get(http.MethodGet, got.URL); code != http.StatusOK {
		t.Fatalf("GET answered %d", code)
	}
	clock.Advance(service.RestoreFor)
	if code := c.Get(http.MethodGet, got.URL); code != http.StatusBadRequest {
		t.Fatalf("an expired URL answered %d", code)
	}
}

func TestASessionTakesBytesForAWeek(t *testing.T) {
	e, clock, c, repo := setup(t)
	j := e.StartMachineJob(e.Push(repo, "main"))
	tok := e.Token(j, nil)
	c.Bind(tok, nil)
	_, r := c.Save(tok, linux, "build", key)
	req, _ := http.NewRequest(http.MethodPost, r.URL, nil)
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	session := resp.Header.Get("Location")
	// The reservation's URL has expired, but the session goes on.
	clock.Advance(service.ReserveFor)
	put := func() int {
		req, _ := http.NewRequest(http.MethodPut, session, bytes.NewReader([]byte("x")))
		req.Header.Set("Content-Range", "bytes 0-0/*")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put(); code != http.StatusPermanentRedirect {
		t.Fatalf("a session's chunk answered %d", code)
	}
	// Its commit is refused, since the reservation has expired.
	if a := c.Commit(tok, r.ID, 1, conformance.Sum([]byte("x"))); a.Status != http.StatusForbidden {
		t.Fatalf("a commit after expiry answered %d", a.Status)
	}
	clock.Advance(service.SessionFor)
	if code := put(); code != http.StatusGone {
		t.Fatalf("a week-old session answered %d", code)
	}
}

func TestFaults(t *testing.T) {
	e, _, c, repo := setup(t)
	j := e.StartMachineJob(e.Push(repo, "main"))
	tok := e.Token(j, nil)
	e.Faults().Add(fault.Rule{PathPrefix: "/v1/bind", Times: 1, Respond: service.Refuse("no binding today")})
	if a := c.Bind(tok, nil); a.Status != http.StatusForbidden || a.Reason != "no binding today" {
		t.Fatalf("bind answered %d %q", a.Status, a.Reason)
	}
	e.Faults().Add(fault.Rule{PathPrefix: "/v1/cache/", Times: 1, Respond: service.Unavailable(30 * time.Second)})
	c.Bind(tok, nil)
	if a, _ := c.Restore(tok, service.ServiceStore, linux, "build", key, false); a.Status != http.StatusServiceUnavailable {
		t.Fatalf("restore answered %d", a.Status)
	}
	if a, _ := c.Restore(tok, service.ServiceStore, linux, "build", key, false); a.Status != http.StatusOK {
		t.Fatalf("restore answered %d once the fault was used", a.Status)
	}
}

func TestAnAutomaticRerunKeepsItsRunsChoices(t *testing.T) {
	e, clock, c, repo := setup(t)
	first := saved(t, e, c, repo, []byte("first"))
	run := e.Push(repo, "main")
	j := e.StartMachineJob(run)
	tok := e.Token(j, nil)
	c.Bind(tok, nil)
	if _, r := c.Restore(tok, service.ServiceStore, linux, "build", key, false); r == nil || r.SHA256 != first.SHA256 {
		t.Fatal("no first choice")
	}
	clock.Advance(time.Second)
	saved(t, e, c, repo, []byte("second"))
	e.PreemptJob(j)
	again := e.AutomaticRerun(run)
	tok = e.Token(e.StartMachineJob(again), nil)
	if _, r := c.Restore(tok, service.ServiceStore, linux, "build", key, false); r == nil || r.SHA256 != first.SHA256 {
		t.Fatal("an automatic rerun picked again")
	}
	if es := e.Entries(repo); len(es) != 2 {
		t.Fatalf("entries %+v", es)
	}
	if b, ok := e.Object(e.Choices(run)[0].Entry); !ok || string(b) != "first" {
		t.Fatalf("the chosen entry's bytes %q", b)
	}
}

// When the issuer's keys can't be read, nothing is confirmed: the emulator
// answers 503, as the service does.
func TestAnIssuerOutageIsRetryable(t *testing.T) {
	issuer := oidc.New()
	e := service.New(service.WithIssuer(issuer))
	t.Cleanup(e.Close)
	c := &conformance.Client{T: t, URL: e.URL()}
	repo := e.Repository("app")
	j := e.StartJob(e.Push(repo, "main"), "")
	tok := e.Token(j, nil)
	issuer.Close()
	if a := c.Bind(tok, nil); a.Status != http.StatusServiceUnavailable {
		t.Fatalf("bind answered %d", a.Status)
	}
	if a, _ := c.Restore(tok, "actions", linux, "build", key, false); a.Status != http.StatusServiceUnavailable {
		t.Fatalf("restore answered %d", a.Status)
	}
}

// A pull request's closing counts from the second after GitHub's closed_at,
// which is to the second: a run bound in that second, even once it's
// reopened, saves nothing it restores.
func TestAClosingCountsFromTheNextSecond(t *testing.T) {
	e, clock, c, repo := setup(t)
	clock.Advance(time.Second - time.Duration(clock.Now().Nanosecond()) + 100*time.Millisecond)
	pr := e.PullRequest(repo, "main", "fix")
	run := e.PullRequestRun(pr)
	e.ClosePullRequest(pr)
	e.ReopenPullRequest(pr)
	clock.Advance(500 * time.Millisecond)
	j := e.StartMachineJob(run)
	tok := e.Token(j, nil)
	c.Bind(tok, nil)
	_, r := c.Save(tok, linux, "build", key)
	c.Upload(r, []byte("x"), 1<<20, false)
	c.Commit(tok, r.ID, 1, conformance.Sum([]byte("x")))
	e.FinishJob(j, "success")
	next := e.StartMachineJob(e.PullRequestRun(pr))
	tok = e.Token(next, nil)
	c.Bind(tok, nil)
	if _, got := c.Restore(tok, service.ServiceStore, linux, "build", key, false); got != nil {
		t.Fatalf("restored %s's entry, saved by a run bound in the closing's second", got.Scope)
	}
}
