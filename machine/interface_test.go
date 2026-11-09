package machine_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/lanescape/runners/endpoints"
	"github.com/lanescape/runners/machine"
)

// action is a client that speaks the interface for actions
// (specs/0005-interface.md) as the actions in lanescape/actions do, and
// nothing more: it binds a run with the job's token, builds selectors from
// the binding, and asks for a save by writing the request file. These tests
// are the service's promise to the actions; one that has to change breaks
// /v1/.
type action struct {
	t *testing.T
	// service stands in for endpoints.Service, which is also the audience
	// of every token sent to it.
	service string
}

// bindAnswer is what POST /v1/bind answers.
type bindAnswer struct {
	Status int
	Scope  string
	Reason string
}

// bind calls POST /v1/bind with the job's GitHub OIDC token, whose audience
// is the service, and a body that's empty, except from a bootstrap workflow
// in a pull_request_target run.
func (a action) bind(token string, event *bindEvent) bindAnswer {
	a.t.Helper()
	var body []byte
	if event != nil {
		body, _ = json.Marshal(event)
	}
	req, _ := http.NewRequest(http.MethodPost, a.service+"/v1/bind", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Scope     string `json:"scope"`
		Reason    string `json:"reason"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		a.t.Fatalf("bind answered %d with a body that isn't JSON: %v", resp.StatusCode, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if !scopePattern.MatchString(out.Scope) {
			a.t.Fatalf("bind answered 200 with scope %q", out.Scope)
		}
	case http.StatusForbidden:
		if out.Reason == "" {
			a.t.Fatal("bind answered 403 with no reason")
		}
	case http.StatusServiceUnavailable:
		if !out.Retryable || resp.Header.Get("Retry-After") == "" {
			a.t.Fatalf("bind answered 503 without retryable and Retry-After: %+v", out)
		}
	default:
		a.t.Fatalf("bind answered %d, which the interface doesn't have", resp.StatusCode)
	}
	return bindAnswer{Status: resp.StatusCode, Scope: out.Scope, Reason: out.Reason}
}

// bindEvent is what a bootstrap workflow sends from its own event.
type bindEvent struct {
	PullRequest      int    `json:"pull_request"`
	HeadRepositoryID int64  `json:"head_repository_id"`
	HeadSHA          string `json:"head_sha"`
}

var (
	scopePattern  = regexp.MustCompile(`^(main|read|pr[1-9][0-9]*)$`)
	shapePattern  = regexp.MustCompile(`^lanescape-[1-9][0-9]*vcpu-linux-(amd64|arm64)$`)
	familyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	keyPattern    = regexp.MustCompile(`^[0-9a-f]{20}$`)
)

// selector builds a job's selector, as the runners action does:
// lanescape-<vCPUs>vcpu-linux-<arch>[-spot][-disk-<family>-<key>]-<scope>,
// in lowercase, with the scope the binding returned.
func (a action) selector(shape string, spot bool, family, key, scope string) string {
	a.t.Helper()
	if !shapePattern.MatchString(shape) || !scopePattern.MatchString(scope) {
		a.t.Fatalf("no selector for shape %q and scope %q", shape, scope)
	}
	var b strings.Builder
	b.WriteString(shape)
	if spot {
		b.WriteString("-spot")
	}
	if family != "" {
		if !familyPattern.MatchString(family) || !keyPattern.MatchString(key) {
			a.t.Fatalf("family %q and key %q aren't the interface's", family, key)
		}
		fmt.Fprintf(&b, "-disk-%s-%s", family, key)
	}
	b.WriteString("-" + scope)
	return b.String()
}

// saveStep is the step that asks to save the work disk, as the save action
// does on the service's machines: {"save": true} in the request file, which
// on a real machine is machine.SaveMarker and here is $LSR_TEST_SAVE.
func (a action) saveStep() string {
	return fmt.Sprintf(`printf '%%s' '%s' > "$LSR_TEST_SAVE"`, machine.SaveRequest)
}

func TestTheInterfacesConstants(t *testing.T) {
	if endpoints.Organization != orgID || !strings.HasPrefix(endpoints.Service, "https://") || !strings.HasPrefix(endpoints.Proxy, "https://") {
		t.Fatalf("endpoints = %d %s %s", endpoints.Organization, endpoints.Service, endpoints.Proxy)
	}
	if machine.SaveMarker != "/var/lib/lanescape-runners/save" || machine.SaveRequest != `{"save": true}` {
		t.Fatalf("save request = %s in %s", machine.SaveRequest, machine.SaveMarker)
	}
}
