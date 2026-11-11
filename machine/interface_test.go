package machine_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/lanescape/runners/endpoints"
	"github.com/lanescape/runners/machine"
)

// action is a client that speaks the interface for actions
// (specs/0005-interface.md) as the actions in lanescape/actions do, and
// nothing more: it writes a job's selector, which names a shape alone; it
// writes the steps that ask for the job's work disk and its save, which
// run in the job, signed with the machine's identity token from its
// metadata server, and wait for /work to be the mounted disk. These tests are the service's promise to the actions; one
// that has to change breaks /v1/.
type action struct {
	t *testing.T
	// service stands in for endpoints.Service, which is also the audience
	// of every token sent to it.
	service string
}

var shapePattern = regexp.MustCompile(`^lanescape-[1-9][0-9]*vcpu-linux-(amd64|arm64)$`)

// selector writes a job's selector, as a workflow does:
// lanescape-<vCPUs>vcpu-linux-<arch>[-spot].
func (a action) selector(shape string, spot bool) string {
	a.t.Helper()
	if !shapePattern.MatchString(shape) {
		a.t.Fatalf("no selector for shape %q", shape)
	}
	if spot {
		return shape + "-spot"
	}
	return shape
}

// stepRequest is how the actions call the service from a job's step: the
// service's address and a fresh identity token from the metadata server,
// which $LSR_TEST_METADATA stands in for, the token in a header read from
// standard input, never in an argument another process could see. It
// prints the answer's status, leaves its body in the file $answer, and
// prints both in the job's log, as "<path> <status> <body>", where a test
// reads them.
const stepRequest = `set -e
answer=$(mktemp)
trap 'rm -f "$answer"' EXIT
lsr_request() {
	local md=$LSR_TEST_METADATA service token code
	service=$(curl -fsS -H 'Metadata-Flavor: Google' "$md/computeMetadata/v1/instance/attributes/lsr-service") || return 1
	token=$(curl -fsS -G -H 'Metadata-Flavor: Google' --data-urlencode "audience=$service" --data-urlencode format=full \
		"$md/computeMetadata/v1/instance/service-accounts/default/identity") || return 1
	code=$(printf 'header = "Authorization: Bearer %s"\n' "$token" |
		curl -sS --config - -o "$answer" -w '%{http_code}' -X POST -H 'Content-Type: application/json' --data "$2" "$service$1") || code=000
	echo "lanescape-runners test action: $1 $code $(tr -d '\n' <"$answer")" >&2
	echo "$code"
}
`

// steps joins a job's steps into the script the runner runs: each must
// succeed for the next to run, as a workflow's steps do.
func (a action) steps(parts ...string) string {
	return stepRequest + strings.Join(parts, "\n") + "\n"
}

// diskStep asks for the job's work disk of a family, as the disk action
// does: POST /v1/disks until the answer is ready, then waits until /work,
// which $LSR_TEST_WORK stands in for, is the mounted disk. A refusal fails
// the step.
func (a action) diskStep(family string) string {
	return fmt.Sprintf(`while :; do
	code=$(lsr_request /v1/disks '{"family": "%s"}') || code=000
	case $code in
	200) [ "$(jq -r .state "$answer")" = ready ] && break ;;
	403) exit 1 ;;
	esac
	sleep 0.2
done
until [ -e "$LSR_TEST_WORK/.disk" ]; do sleep 0.1; done`, family)
}

// saveStep asks for the job's work disk to be saved when the job finishes,
// as the save-disk action does. A refusal fails the step.
func (a action) saveStep() string {
	return `code=$(lsr_request /v1/disks/save '{"save": true}') || code=000
[ "$code" = 200 ]`
}

// noDiskYetStep fails unless /work is still without a disk, as it is
// before the job asks for one.
func (a action) noDiskYetStep() string {
	return `[ ! -e "$LSR_TEST_WORK/.disk" ]`
}

// stepAnswer is one of a job's requests from its steps, as its log shows
// it.
type stepAnswer struct {
	Path string
	Code int
	Body map[string]any
}

var stepAnswerLine = regexp.MustCompile(`lanescape-runners test action: (\S+) (\d{3}) (.*)$`)

// stepAnswers reads a job's requests from its log, checking each answer
// is one the interface has.
func (a action) stepAnswers(log string) []stepAnswer {
	a.t.Helper()
	var out []stepAnswer
	for _, l := range strings.Split(log, "\n") {
		m := stepAnswerLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		code, _ := strconv.Atoi(m[2])
		var body map[string]any
		if err := json.Unmarshal([]byte(m[3]), &body); err != nil {
			a.t.Fatalf("%s answered %d with a body that isn't JSON: %q", m[1], code, m[3])
		}
		sa := stepAnswer{Path: m[1], Code: code, Body: body}
		a.checkStepAnswer(sa)
		out = append(out, sa)
	}
	return out
}

// The steps of the restore order a work disk can come from, and resume.
var restoredSteps = []string{"resume", "pull_request_exact", "pull_request_newest", "main_exact", "main_newest", "branch_exact",
	"branch_newest", "none"}

func (a action) checkStepAnswer(sa stepAnswer) {
	a.t.Helper()
	switch {
	case sa.Code == http.StatusForbidden:
		if r, _ := sa.Body["reason"].(string); r == "" {
			a.t.Fatalf("%s answered 403 with no reason: %v", sa.Path, sa.Body)
		}
	case sa.Code == http.StatusServiceUnavailable:
		if sa.Body["retryable"] != true {
			a.t.Fatalf("%s answered 503 without retryable: %v", sa.Path, sa.Body)
		}
	case sa.Code != http.StatusOK:
		a.t.Fatalf("%s answered %d, which the interface doesn't have", sa.Path, sa.Code)
	case sa.Path == "/v1/disks":
		state, _ := sa.Body["state"].(string)
		restored, _ := sa.Body["restored"].(string)
		if (state != "making" && state != "ready") || !slices.Contains(restoredSteps, restored) || len(sa.Body) != 2 {
			a.t.Fatalf("/v1/disks answered %v, which the interface doesn't have", sa.Body)
		}
	case sa.Path == "/v1/disks/save":
		saves, ok := sa.Body["saves"].(bool)
		reason, _ := sa.Body["reason"].(string)
		if !ok || (saves && reason != "") || (!saves && reason == "") {
			a.t.Fatalf("/v1/disks/save answered %v, which the interface doesn't have", sa.Body)
		}
	}
}

func TestTheInterfacesConstants(t *testing.T) {
	if endpoints.Organization != orgID || !strings.HasPrefix(endpoints.Service, "https://") {
		t.Fatalf("endpoints = %d %s", endpoints.Organization, endpoints.Service)
	}
	// The work disk is mounted where the actions wait for it.
	if machine.WorkPath != "/work" {
		t.Fatalf("the work disk is at %s", machine.WorkPath)
	}
}
