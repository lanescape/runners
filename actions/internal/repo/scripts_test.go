package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// stepScript is the run script of the step named name in an action or a
// workflow.
func stepScript(t *testing.T, path, name string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Runs struct {
			Steps []step `yaml:"steps"`
		} `yaml:"runs"`
		Jobs map[string]struct {
			Steps []step `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	steps := doc.Runs.Steps
	for _, j := range doc.Jobs {
		steps = append(steps, j.Steps...)
	}
	for _, s := range steps {
		if s.Name == name {
			return s.Run
		}
	}
	t.Fatalf("%s has no step %q", path, name)
	return ""
}

func bash(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	return p
}

func outputs(t *testing.T, path string) map[string]string {
	t.Helper()
	b, _ := os.ReadFile(path)
	out := map[string]string{}
	for line := range strings.SplitSeq(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

// The action's own steps find Go and build its program for this machine,
// whatever Go settings the job has, in its environment or saved with
// go env -w: never for another platform, never with the end-to-end tests'
// tag, and never in a workspace.
func TestTheBuildIgnoresTheJobsGoSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a program")
	}
	sh := bash(t)
	program, err := filepath.Abs(filepath.Join(actionsDir, "disk"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	goenv := filepath.Join(dir, "goenv")
	if err := os.WriteFile(goenv, []byte("GOOS=plan9\nGOARCH=386\nGOFLAGS=-tags=e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "go.work")
	if err := os.WriteFile(work, []byte("go 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gocache, _ := exec.Command("go", "env", "GOCACHE").Output()
	gomodcache, _ := exec.Command("go", "env", "GOMODCACHE").Output()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"GOCACHE=" + strings.TrimSpace(string(gocache)), "GOMODCACHE=" + strings.TrimSpace(string(gomodcache)),
		"GOENV=" + goenv, "GOWORK=" + work, "GOFLAGS=-tags=e2e", "GOOS=windows", "GOEXPERIMENT=nosuchexperiment",
		"GO111MODULE=off", "GOROOT=" + dir, "GODEBUG=nosuchsetting=1", "GOAMD64=v4", "GOARM64=v9.5",
		"GOTOOLCHAIN=local", "RUNNER_TEMP=" + dir, "RUNNER_OS=" + map[string]string{"darwin": "macOS", "linux": "Linux"}[runtime.GOOS],
		"PROGRAM=" + program, "GITHUB_ACTION_PATH=" + program,
	}
	path := filepath.Join(actionsDir, "disk", "action.yml")

	out := filepath.Join(dir, "find-output")
	cmd := exec.Command(sh, "-e", "-c", stepScript(t, path, "Find Go"))
	cmd.Env = append(env, "GITHUB_OUTPUT="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Find Go: %v\n%s", err, b)
	}
	if o := outputs(t, out); o["found"] != "true" || "go"+o["version"] != runtime.Version() {
		t.Fatalf("Find Go = %v, with %s", o, runtime.Version())
	}

	out = filepath.Join(dir, "build-output")
	cmd = exec.Command(sh, "-e", "-c", stepScript(t, path, "Build"))
	cmd.Env = append(env, "GITHUB_OUTPUT="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Build: %v\n%s", err, b)
	}
	bin := outputs(t, out)["bin"]
	info, err := exec.Command("go", "version", "-m", bin).Output()
	if err != nil {
		t.Fatalf("go version -m %s: %v", bin, err)
	}
	s := string(info)
	for _, want := range []string{"GOOS=" + runtime.GOOS, "GOARCH=" + runtime.GOARCH, "CGO_ENABLED=0", "-trimpath=true"} {
		if !strings.Contains(s, want) {
			t.Errorf("the program wasn't built with %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "-tags") {
		t.Errorf("the program was built with tags:\n%s", s)
	}
}

// When Go must be set up, actions/setup-go runs go with none of the job's
// Go settings either.
func TestSettingUpGoIgnoresTheJobsGoSettings(t *testing.T) {
	want := map[string]string{"GOENV": "off", "GOWORK": "off", "GOTOOLCHAIN": "local", "GOFLAGS": "", "GOOS": "",
		"GOARCH": "", "GOAMD64": "", "GOARM64": "", "GOEXPERIMENT": "", "GOROOT": "", "GODEBUG": "", "GOFIPS140": "", "GO111MODULE": "",
		// It prints go env: nothing that may hold a credential.
		"GOPROXY": "", "GONOPROXY": "", "GOPRIVATE": "", "GOSUMDB": "", "GONOSUMDB": "", "GOINSECURE": "", "GOAUTH": "", "GOVCS": "",
		"GOCACHEPROG": "", "GO_DOWNLOAD_BASE_URL": ""}
	for name, a := range actions(t) {
		found := false
		for _, s := range a.Runs.Steps {
			if !strings.HasPrefix(s.Uses, "actions/setup-go@") {
				continue
			}
			found = true
			if !equalMaps(s.Env, want) {
				t.Errorf("%s: Set up Go's env is %v, want %v", name, s.Env, want)
			}
			if s.With["go-version"] != "${{ steps.go.outputs.version }}" {
				t.Errorf("%s: Set up Go doesn't set up go.mod's exact toolchain: %v", name, s.With)
			}
		}
		if !found {
			t.Errorf("%s doesn't set up Go", name)
		}
	}
}

// cd.yml's step that moves the tag, run against a stand-in for gh
// that answers as GitHub would after --jq, and records every call.
func TestTheReleaseMovesTheTagOnlyForward(t *testing.T) {
	sh := bash(t)
	script := stepScript(t, filepath.Join(root, ".github", "workflows", "cd.yml"), "Move the tag forward")
	const sha, old = "1111111111111111111111111111111111111111", "0000000000000000000000000000000000000000"
	for _, tc := range []struct {
		name                     string
		onMain, current, forward string
		fail                     bool
		call                     string
	}{
		{name: "first release", onMain: "identical", current: "", call: "api --silent -X POST repos/lanescape/runners/git/refs -f ref=refs/tags/v1 -f sha=" + sha},
		{name: "forward", onMain: "ahead", current: "commit " + old, forward: "ahead",
			call: "api --silent -X PATCH repos/lanescape/runners/git/refs/tags/v1 -f sha=" + sha + " -F force=false"},
		{name: "already there", onMain: "identical", current: "commit " + sha},
		{name: "not on main", onMain: "diverged", fail: true},
		{name: "backward", onMain: "identical", current: "commit " + old, forward: "behind", fail: true},
		{name: "diverged", onMain: "identical", current: "commit " + old, forward: "diverged", fail: true},
		{name: "annotated tag", onMain: "identical", current: "tag " + old, fail: true},
		{name: "GitHub fails", onMain: "FAIL", fail: true},
		{name: "reading the tag fails", onMain: "identical", current: "FAIL", fail: true},
	} {
		dir := t.TempDir()
		log := filepath.Join(dir, "calls")
		stub := `#!/usr/bin/env bash
echo "$*" >> "$GH_LOG"
[ "$GH_TOKEN" = app-token ] || { echo "no App token" >&2; exit 1; }
answer() { if [ "$1" = FAIL ]; then echo "HTTP 502" >&2; exit 1; fi; [ -z "$1" ] || echo "$1"; }
case "$*" in
  *"compare/` + sha + `...main"*) answer "$ON_MAIN" ;;
  *matching-refs/tags/v1*) answer "$CURRENT" ;;
  *"compare/` + old + `...` + sha + `"*) answer "$FORWARD" ;;
  *"-X POST"*|*"-X PATCH"*) ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(sh, "-e", "-c", script)
		cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "GH_LOG=" + log, "GH_TOKEN=app-token",
			"REPO=lanescape/runners", "SHA=" + sha, "TAG=v1", "ON_MAIN=" + tc.onMain, "CURRENT=" + tc.current, "FORWARD=" + tc.forward}
		b, err := cmd.CombinedOutput()
		if (err != nil) != tc.fail {
			t.Errorf("%s: err = %v, want failure %v\n%s", tc.name, err, tc.fail, b)
		}
		calls, _ := os.ReadFile(log)
		var mutations []string
		for c := range strings.SplitSeq(strings.TrimSpace(string(calls)), "\n") {
			if strings.Contains(c, "-X ") {
				mutations = append(mutations, c)
			}
		}
		switch {
		case tc.call == "" && len(mutations) != 0:
			t.Errorf("%s: moved the tag: %v", tc.name, mutations)
		case tc.call != "" && (len(mutations) != 1 || mutations[0] != tc.call):
			t.Errorf("%s: calls %v, want %s", tc.name, mutations, tc.call)
		}
	}
}

// The --jq that reads the tag gives its type and commit, or nothing when
// there's no tag, from GitHub's matching-refs answer.
func TestTheReleasesTagQuery(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("no jq")
	}
	script := stepScript(t, filepath.Join(root, ".github", "workflows", "cd.yml"), "Move the tag forward")
	start := strings.Index(script, `--jq "`)
	if start < 0 {
		t.Fatal("no --jq")
	}
	q := script[start+len(`--jq "`):]
	q = strings.ReplaceAll(q[:strings.Index(q, `")"`)], `\"`, `"`)
	q = strings.ReplaceAll(q, "$TAG", "v1")
	for in, want := range map[string]string{
		`[]`: "",
		`[{"ref": "refs/tags/v10", "object": {"type": "commit", "sha": "b"}}]`:                                                                    "",
		`[{"ref": "refs/tags/v1", "object": {"type": "commit", "sha": "a"}}, {"ref": "refs/tags/v10", "object": {"type": "commit", "sha": "b"}}]`: "commit a",
		`[{"ref": "refs/tags/v1", "object": {"type": "tag", "sha": "c"}}]`:                                                                        "tag c",
	} {
		cmd := exec.Command(jq, "-r", q)
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("%s: %q, %v, want %q", in, out, err, want)
		}
	}
}

// checkout checks for Git 2.18 or newer before it calls actions/checkout,
// and fails, saying so, without it.
func TestCheckoutNeedsGit(t *testing.T) {
	sh := bash(t)
	if runtime.GOOS == "windows" {
		t.Skip("stands in for git with a shell script")
	}
	steps := actions(t)["checkout"].Runs.Steps
	git := slices.IndexFunc(steps, func(s step) bool { return s.Name == "Check for Git" })
	co := slices.IndexFunc(steps, func(s step) bool { return strings.HasPrefix(s.Uses, "actions/checkout@") })
	if git < 0 || co < 0 || git > co {
		t.Fatalf("checkout checks for Git at step %d, and calls actions/checkout at %d", git, co)
	}
	script := stepScript(t, filepath.Join(actionsDir, "checkout", "action.yml"), "Check for Git")
	sed, err := exec.LookPath("sed")
	if err != nil {
		t.Skip("no sed")
	}
	for _, tc := range []struct {
		name, version string
		says          string
	}{
		{name: "no Git", says: "there's no Git here"},
		{name: "Git 1.9", version: "git version 1.9.5", says: "Git here is 1.9"},
		{name: "Git 2.17", version: "git version 2.17.1", says: "Git here is 2.17"},
		{name: "Git 2.18", version: "git version 2.18.0"},
		{name: "Apple's Git", version: "git version 2.39.5 (Apple Git-154)"},
		{name: "Git for Windows", version: "git version 2.47.1.windows.2"},
		{name: "Git 3", version: "git version 3.0.0"},
	} {
		dir := t.TempDir()
		if err := os.Symlink(sed, filepath.Join(dir, "sed")); err != nil {
			t.Fatal(err)
		}
		if tc.version != "" {
			stub := "#!" + sh + "\n[ \"$1\" = --version ] && echo '" + tc.version + "'\n"
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(sh, "-e", "-c", script)
		cmd.Env = []string{"PATH=" + dir}
		b, err := cmd.CombinedOutput()
		if tc.says == "" {
			if err != nil {
				t.Errorf("%s: %v\n%s", tc.name, err, b)
			}
			continue
		}
		if err == nil || !strings.Contains(string(b), "::error::checkout needs Git 2.18 or newer") || !strings.Contains(string(b), tc.says) {
			t.Errorf("%s: err = %v\n%s", tc.name, err, b)
		}
	}
}

// cd.yml moves v1 only once its own deploy has applied infra/ at the
// commit (specs/0004-deployment.md, Releasing the actions): the release job
// needs the deploy job's output deployed, which only cd/apply.sh's step
// gives, and only after an apply, not merely the job's success, since
// apply.sh succeeds without applying when main has moved on.
func TestTheReleaseNeedsADeploy(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "cd.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Needs       any               `yaml:"needs"`
			If          string            `yaml:"if"`
			Environment string            `yaml:"environment"`
			Outputs     map[string]string `yaml:"outputs"`
			Steps       []step            `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatal(err)
	}
	release, deploy := wf.Jobs["release"], wf.Jobs["deploy"]
	if release.Needs != "deploy" || release.Environment != "release" {
		t.Errorf("release needs %v, in environment %q", release.Needs, release.Environment)
	}
	if !strings.Contains(release.If, "needs.deploy.outputs.deployed == 'true'") {
		t.Errorf("release runs if %q, not only once the deploy applied", release.If)
	}
	if deploy.Outputs["deployed"] != "${{ steps.apply.outputs.deployed }}" {
		t.Errorf("deploy's output deployed is %q", deploy.Outputs["deployed"])
	}
	i := slices.IndexFunc(deploy.Steps, func(s step) bool { return s.ID == "apply" })
	if i < 0 || deploy.Steps[i].Run != "bash cd/apply.sh" {
		t.Fatal("deploy has no step apply that runs cd/apply.sh")
	}
	// Nothing in the release job checks out or runs the repository's code.
	for _, s := range release.Steps {
		if strings.HasPrefix(s.Uses, "actions/checkout@") || strings.HasPrefix(s.Uses, "./") {
			t.Errorf("release step %q uses %s", s.Name, s.Uses)
		}
	}
}
