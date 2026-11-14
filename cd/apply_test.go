// Package cd_test runs CD's preview and apply, apply.sh, against stand-ins
// for gcloud and curl, so that each way it can fail fails the deploy, and
// the access token reaches no process's arguments (specs/0004-deployment.md,
// CI and CD). Nothing here calls Google Cloud.
package cd_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// gcloud stands in for gcloud: what each describe says comes from the
// environment the test sets.
const gcloud = `#!/bin/bash
echo "gcloud $*" >>"$CALLS"
case "$*" in
  "auth print-access-token") echo SECRET-TOKEN ;;
  "infra-manager deployments describe"*"value(name)"*)
    if [ "$EXISTS" = 1 ]; then echo deployment; else echo "ERROR: NOT_FOUND: no such deployment" >&2; exit 1; fi ;;
  "infra-manager deployments describe"*"value(state)"*) echo "${DEPLOYMENT_STATE:-ACTIVE}" ;;
  "infra-manager deployments describe"*"latestRevision"*)
    [ "$REVISION_FAIL" = 1 ] && exit 1; echo "$DEPLOYMENT/revisions/r-1" ;;
  "infra-manager previews create"*) [ "$PREVIEW_FAIL" = 1 ] && exit 1; true ;;
  "infra-manager previews describe"*"value(logs)"*) [ "$PREVIEW_LOGS_FAIL" = 1 ] && exit 1; echo gs://bucket/preview ;;
  "infra-manager previews describe"*"value(state)"*) echo "${PREVIEW_STATE:-SUCCEEDED}" ;;
  "infra-manager previews describe"*) echo "state: ${PREVIEW_STATE:-SUCCEEDED}" ;;
  "infra-manager previews delete"*) ;;
  "infra-manager revisions describe"*"value(state)"*) echo "${REVISION_STATE:-APPLIED}" ;;
  "infra-manager revisions describe"*"value(logs)"*) echo gs://bucket/apply ;;
  "infra-manager revisions describe"*) echo "state: ${REVISION_STATE:-APPLIED}" ;;
  "storage ls -r"*)
    [ "$LS_FAIL" = 1 ] && { echo "ERROR: 403 forbidden" >&2; exit 1; }
    echo "$4/log-1.txt"; echo "$4/plan.tfplan" ;;
  "storage cat"*) [ "$CAT_FAIL" = 1 ] && exit 1; echo "log of $3" ;;
  *) echo "unexpected: gcloud $*" >&2; exit 2 ;;
esac
`

// curl stands in for curl: it records its arguments and its configuration
// on standard input, and answers as Infrastructure Manager would.
const curl = `#!/bin/bash
config=$(cat)
echo "curl $*" >>"$CALLS"
echo "config $config" >>"$CALLS"
case "$*" in
  *" GET "*)
    if [ "$OPERATION_ERROR" = 1 ]; then echo '{"name":"op","done":true,"error":{"code":9,"message":"boom"}}'
    else echo '{"name":"op","done":true}'; fi ;;
  *) [ "$CALL_FAIL" = 1 ] && { echo '{"error":{"message":"denied"}}'; exit 22; }
     echo '{"name":"op","done":false}' ;;
esac
`

// bash is a Bash of at least 4.4, which the script needs, as Ubuntu's is.
func bash(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/opt/homebrew/bin/bash", "/usr/local/bin/bash", "/bin/bash", "/usr/bin/bash"} {
		out, err := exec.Command(p, "-c", `echo "${BASH_VERSINFO[0]} ${BASH_VERSINFO[1]}"`).Output()
		if err != nil {
			continue
		}
		f := strings.Fields(string(out))
		major, _ := strconv.Atoi(f[0])
		minor, _ := strconv.Atoi(f[1])
		if major > 4 || major == 4 && minor >= 4 {
			return p
		}
	}
	t.Skip("no Bash 4.4 or later")
	return ""
}

type result struct {
	err     error
	out     string
	calls   string
	summary string
}

func apply(t *testing.T, env ...string) result {
	t.Helper()
	sh := bash(t)
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("no jq")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"gcloud": gcloud, "curl": curl, "sleep": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(jq, filepath.Join(bin, "jq")); err != nil {
		t.Fatal(err)
	}
	calls, summary := filepath.Join(dir, "calls"), filepath.Join(dir, "summary")
	cmd := exec.Command(sh, "apply.sh")
	cmd.Env = append([]string{
		"PATH=" + bin + ":/usr/bin:/bin", "CALLS=" + calls, "GITHUB_STEP_SUMMARY=" + summary,
		"GITHUB_SHA=0123456789abcdef0123456789abcdef01234567", "GITHUB_RUN_ID=111", "GITHUB_RUN_ATTEMPT=2",
		"PROJECT=lanescape-dev", "REGION=northamerica-northeast1",
		"DEPLOYMENT=projects/lanescape-dev/locations/northamerica-northeast1/deployments/lanescape-runners",
		"INFRA_ACCOUNT=projects/lanescape-dev/serviceAccounts/lanescape-runners-infra@lanescape-dev.iam.gserviceaccount.com",
		"SOURCE=https://github.com/lanescape/runners.git", "TF_VERSION=1.5.7", "INFRA_MANAGER_BUCKET=gs://lanescape-runners-infra-manager",
		"GITHUB_APP_ID=42", "IMAGE=northamerica-northeast1-docker.pkg.dev/lanescape-dev/lanescape-runners/runners@sha256:" + strings.Repeat("a", 64),
	}, env...)
	out, err := cmd.CombinedOutput()
	c, _ := os.ReadFile(calls)
	s, _ := os.ReadFile(summary)
	return result{err: err, out: string(out), calls: string(c), summary: string(s)}
}

func TestAFirstApplyCreatesTheDeployment(t *testing.T) {
	r := apply(t, "EXISTS=0")
	if r.err != nil {
		t.Fatalf("apply failed: %v\n%s", r.err, r.out)
	}
	// The preview is of a new deployment; the apply creates it, from git
	// at the commit, with the image by digest and the run as deploy_id.
	if strings.Contains(r.calls, "previews create") && strings.Contains(r.calls, "--deployment=") {
		t.Error("the first preview names a deployment that doesn't exist")
	}
	for _, want := range []string{
		"previews create projects/lanescape-dev/locations/northamerica-northeast1/previews/lanescape-runners-111-2",
		"--git-source-ref=0123456789abcdef0123456789abcdef01234567",
		"deploy_id=111-2",
		"-X POST",
		"https://config.googleapis.com/v1/projects/lanescape-dev/locations/northamerica-northeast1/deployments?deploymentId=lanescape-runners",
		`"gitSource":{"repo":"https://github.com/lanescape/runners.git","directory":"infra","ref":"0123456789abcdef0123456789abcdef01234567"}`,
		`"deploy_id":{"inputValue":"111-2"}`,
		"previews delete",
	} {
		if !strings.Contains(strings.ReplaceAll(strings.ReplaceAll(r.calls, "\n", ""), " ", ""), strings.ReplaceAll(want, " ", "")) {
			t.Errorf("no %s in the calls:\n%s", want, r.calls)
		}
	}
	// Both logs are printed, and not Terraform's binary plan.
	if !strings.Contains(r.out, "log of gs://bucket/preview/log-1.txt") || !strings.Contains(r.out, "log of gs://bucket/apply/log-1.txt") ||
		strings.Contains(r.out, "plan.tfplan") {
		t.Errorf("logs printed:\n%s", r.out)
	}
	if !strings.Contains(r.summary, "revisions/r-1") {
		t.Errorf("summary = %q", r.summary)
	}
}

func TestALaterApplyUpdatesTheDeployment(t *testing.T) {
	r := apply(t, "EXISTS=1")
	if r.err != nil {
		t.Fatalf("apply failed: %v\n%s", r.err, r.out)
	}
	if !strings.Contains(r.calls, "--deployment=projects/lanescape-dev/locations/northamerica-northeast1/deployments/lanescape-runners") ||
		!regexp.MustCompile(`(?s)-X PATCH .*? https://config\.googleapis\.com/v1/projects/lanescape-dev/locations/northamerica-northeast1/deployments/lanescape-runners\n`).MatchString(r.calls) {
		t.Fatalf("calls:\n%s", r.calls)
	}
	// gcloud's own apply, which lists the shared staging bucket, is never
	// used.
	if strings.Contains(r.calls, "deployments apply") {
		t.Error("gcloud's apply was used")
	}
}

// TestTheTokenIsInNoArguments: the access token reaches curl only as its
// configuration, on its standard input, and is never printed.
func TestTheTokenIsInNoArguments(t *testing.T) {
	r := apply(t, "EXISTS=1")
	if r.err != nil {
		t.Fatalf("apply failed: %v\n%s", r.err, r.out)
	}
	for _, l := range strings.Split(r.calls, "\n") {
		if strings.HasPrefix(l, "curl ") && strings.Contains(l, "SECRET-TOKEN") {
			t.Fatalf("the token is in curl's arguments: %s", l)
		}
	}
	if !strings.Contains(r.calls, `config header = "Authorization: Bearer SECRET-TOKEN"`) {
		t.Fatalf("the token didn't reach curl's configuration:\n%s", r.calls)
	}
	if strings.Contains(r.out, "SECRET-TOKEN") {
		t.Fatal("the token was printed")
	}
}

func TestEveryFailureFailsTheDeploy(t *testing.T) {
	for _, tc := range []struct {
		env, want string
	}{
		{"PREVIEW_FAIL=1", "Preview failed"},
		{"PREVIEW_STATE=FAILED", "Preview failed"},
		{"CALL_FAIL=1", "Apply failed"},
		{"OPERATION_ERROR=1", "Apply failed"},
		{"DEPLOYMENT_STATE=FAILED", "not ACTIVE and APPLIED"},
		{"REVISION_STATE=FAILED", "not ACTIVE and APPLIED"},
		{"REVISION_FAIL=1", "Couldn't find the apply's revision"},
		{"PREVIEW_LOGS_FAIL=1", "Couldn't find the preview's logs"},
		{"LS_FAIL=1", "Couldn't list"},
		{"CAT_FAIL=1", "Couldn't read"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			r := apply(t, "EXISTS=1", tc.env)
			if r.err == nil {
				t.Fatalf("the deploy succeeded:\n%s", r.out)
			}
			if !strings.Contains(r.out, tc.want) {
				t.Fatalf("output doesn't say %q:\n%s", tc.want, r.out)
			}
		})
	}
	// A failed preview applies nothing.
	r := apply(t, "EXISTS=1", "PREVIEW_STATE=FAILED")
	if strings.Contains(r.calls, "PATCH") || strings.Contains(r.calls, "POST") {
		t.Fatal("applied after a failed preview")
	}
}
