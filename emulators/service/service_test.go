package service_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/lanescape/runners/conformance"
	"github.com/lanescape/runners/emulators/fault"
	"github.com/lanescape/runners/emulators/oidc"
	"github.com/lanescape/runners/emulators/service"
)

func TestFaults(t *testing.T) {
	e := service.New()
	t.Cleanup(e.Close)
	c := &conformance.Client{T: t, URL: e.URL()}
	j := e.StartJob(e.Push(e.Repository("app"), "main"), "")
	tok := e.Token(j, nil)
	e.Faults().Add(fault.Rule{PathPrefix: "/v1/bind", Times: 1, Respond: service.Refuse("no binding today")})
	if a := c.Bind(tok, nil); a.Status != http.StatusForbidden || a.Reason != "no binding today" {
		t.Fatalf("bind answered %d %q", a.Status, a.Reason)
	}
	e.Faults().Add(fault.Rule{PathPrefix: "/v1/bind", Times: 1, Respond: service.Unavailable(30 * time.Second)})
	if a := c.Bind(tok, nil); a.Status != http.StatusServiceUnavailable {
		t.Fatalf("bind answered %d", a.Status)
	}
	if a := c.Bind(tok, nil); a.Status != http.StatusOK || a.Scope() != "main" {
		t.Fatalf("bind answered %d %q once the faults were used", a.Status, a.Scope())
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
}
