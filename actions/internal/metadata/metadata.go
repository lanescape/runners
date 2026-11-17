// Package metadata asks the machine's metadata server, as Compute Engine
// serves it, whether the machine is one of the runner service's, and for
// its identity token: for an audience, in the full format, which names the
// machine. A token is a credential, and never logged.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Server is a machine's metadata server.
type Server struct {
	URL  string
	HTTP *http.Client
	// AskFor is how long OnService waits for an answer; zero means
	// AttributeTimeout.
	AskFor time.Duration
}

// AttributeTimeout is how long OnService waits: a few seconds, so that on
// a runner with no metadata server it decides at once.
const AttributeTimeout = 3 * time.Second

// OnService reports whether this is one of the runner service's machines,
// by the attribute lsr-service, which the metadata server answers there
// and nowhere else (specs/0005-interface.md, Where the service is). Anything but an answer with a value, including no metadata
// server at all, means it isn't.
func (s Server) OnService(ctx context.Context) bool {
	wait := s.AskFor
	if wait <= 0 {
		wait = AttributeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(s.URL, "/")+"/computeMetadata/v1/instance/attributes/lsr-service", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := s.client().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return err == nil && resp.StatusCode == http.StatusOK && strings.TrimSpace(string(b)) != ""
}

// client is the server's HTTP client, which never follows a redirect: the
// metadata server gives none.
func (s Server) client() *http.Client {
	hc := &http.Client{Timeout: 10 * time.Second}
	if s.HTTP != nil {
		copied := *s.HTTP
		hc = &copied
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return hc
}

// Token is the machine's identity token for audience, got afresh.
func (s Server) Token(ctx context.Context, audience string) (string, error) {
	q := url.Values{"audience": {audience}, "format": {"full"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(s.URL, "/")+"/computeMetadata/v1/instance/service-accounts/default/identity?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := s.client().Do(req)
	if err != nil {
		return "", errors.New("the machine's metadata server couldn't be reached")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil || resp.StatusCode != http.StatusOK || len(b) == 0 {
		return "", fmt.Errorf("the machine's metadata server answered %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(b)), nil
}
