package machine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gce "google.golang.org/api/compute/v1"

	gceemu "github.com/lanescape/runners/emulators/compute"
	"github.com/lanescape/runners/machine"
)

const zone = "northamerica-northeast1-a"

// bins builds the service's programs, each from its embedded source alone,
// and the runner stand-in, once for every test.
var bins struct {
	once     sync.Once
	dir      string
	programs map[string]string
	standIn  string
	err      error
}

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// build builds the programs, and returns where each is, by its source's
// directory, and the runner stand-in.
func build(t *testing.T) (map[string]string, string) {
	t.Helper()
	bins.once.Do(func() {
		bins.dir, bins.err = os.MkdirTemp("", "lanescape-runners-machine-")
		if bins.err != nil {
			return
		}
		bins.programs = map[string]string{}
		// Each program builds from its embedded files and nothing else.
		for _, p := range machine.Programs() {
			if bins.programs[p.Source], bins.err = buildEmbedded(bins.dir, p.Source); bins.err != nil {
				return
			}
		}
		bins.standIn = filepath.Join(bins.dir, "standin")
		cmd := exec.Command(goTool(), "build", "-o", bins.standIn, "github.com/lanescape/runners/emulators/github/cmd/standin")
		if out, err := cmd.CombinedOutput(); err != nil {
			bins.err = fmt.Errorf("building the runner stand-in: %v\n%s", err, out)
		}
	})
	if bins.err != nil {
		t.Fatal(bins.err)
	}
	return bins.programs, bins.standIn
}

// buildEmbedded builds one of the embedded programs from its embedded
// files alone, in a module of its own with no requirements, statically,
// as the base image does.
func buildEmbedded(dir, prog string) (string, error) {
	src := filepath.Join(dir, "src-"+prog)
	for path, b := range machine.Files() {
		if !strings.HasPrefix(path, prog+"/") {
			continue
		}
		p := filepath.Join(src, strings.TrimPrefix(path, prog+"/"))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module "+prog+"\n\ngo 1.27\n"), 0o644); err != nil {
		return "", err
	}
	out := filepath.Join(dir, prog)
	cmd := exec.Command(goTool(), "build", "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building %s from its embedded source: %v\n%s", prog, err, b)
	}
	return out, nil
}

func TestMain(m *testing.M) {
	code := m.Run()
	if bins.dir != "" {
		os.RemoveAll(bins.dir)
	}
	os.Exit(code)
}

func TestEmbedded(t *testing.T) {
	fs := machine.Files()
	for path, b := range fs {
		disk, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(disk, b) {
			t.Errorf("%s: embedded copy isn't the file", path)
		}
	}
	// Every file of the programs' is embedded, and none of their tests;
	// every program is in the service's own directory.
	for _, p := range machine.Programs() {
		srcs, _ := filepath.Glob(p.Source + "/*.go")
		if len(srcs) == 0 {
			t.Errorf("%s has no source", p.Source)
		}
		for _, s := range srcs {
			_, ok := fs[filepath.ToSlash(s)]
			if test := strings.HasSuffix(s, "_test.go"); ok == test {
				t.Errorf("%s: embedded %v", s, ok)
			}
		}
		if filepath.Dir(p.Path) != machine.Dir {
			t.Errorf("%s is installed at %s, outside %s", p.Source, p.Path, machine.Dir)
		}
	}
	// Every other file is installed somewhere, and every installed file is
	// embedded.
	installed := map[string]bool{}
	for _, in := range machine.Installs() {
		installed[in.File] = true
		if _, ok := fs[in.File]; !ok || in.Path == "" || in.Mode == 0 {
			t.Errorf("%+v isn't an embedded file with a place on the base image", in)
		}
	}
	for path := range fs {
		if !strings.HasSuffix(path, ".go") && !installed[path] {
			t.Errorf("%s has no place on the base image", path)
		}
	}
	// The programs' defaults are the paths the base image gives, and the
	// script, units and rule name the programs by their full paths
	// (specs/0002-machines.md, A machine's life). The mount program runs
	// mkfs.ext4 from the one path machine.MkfsPath names.
	for file, wants := range map[string][]string{
		"supervisor/main.go":                   {`"` + machine.HookPath + `"`},
		"job-started.sh":                       {"\nexec " + machine.HookProgramPath + ` "$@"` + "\n"},
		"lanescape-runners-supervisor.service": {"ExecStart=" + machine.SupervisorPath + "\n", "StandardOutput=journal+console\n"},
		"lanescape-runners-work-disk.service":  {"ExecStart=" + machine.MountPath + "\n"},
		"90-lanescape-runners-work.rules":      {`SYSTEMD_WANTS}+="` + filepath.Base(machine.WorkDiskUnitPath) + `"`, `SYMLINK=="disk/by-id/google-work"`},
		"mountwork/main.go": {`Mkfs: "` + machine.MkfsPath + `"`, `Mountpoint: "` + machine.WorkPath + `"`, `Device: "/dev/disk/by-id/google-work"`,
			"UID: 1001, GID: 1001"},
	} {
		for _, want := range wants {
			if !strings.Contains(string(fs[file]), want) {
				t.Errorf("%s doesn't say %q", file, want)
			}
		}
	}
	if machine.WorkOwner != "1001:1001" {
		t.Errorf("the work disk's owner is %s", machine.WorkOwner)
	}
	// The script runs the hook's program and nothing else.
	var lines []string
	for _, l := range strings.Split(string(fs["job-started.sh"]), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Errorf("the hook's script runs more than its program: %q", lines)
	}
	if d := machine.Digest(); len(d) != 64 || d != machine.Digest() {
		t.Fatalf("digest = %q", d)
	}
	// Every program builds from what's embedded, statically.
	build(t)
}

// hookEnv is one machine for the hook to run on, and a service it asks.
type hookEnv struct {
	t        *testing.T
	emu      *gceemu.Emulator
	dir      string
	settings string

	mu     sync.Mutex
	codes  []int
	body   string
	asked  []string
	tokens []string
}

func newHookEnv(t *testing.T) *hookEnv {
	t.Helper()
	h := &hookEnv{t: t, dir: t.TempDir()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.asked = append(h.asked, string(b))
		h.tokens = append(h.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		code := http.StatusOK
		if len(h.codes) > 0 {
			code = h.codes[0]
			if len(h.codes) > 1 {
				h.codes = h.codes[1:]
			}
		}
		if r.URL.Path != "/v1/jobs/started" {
			code = http.StatusNotFound
		}
		w.WriteHeader(code)
		if code != http.StatusOK && h.body != "" {
			_, _ = io.WriteString(w, h.body)
		}
	}))
	t.Cleanup(srv.Close)
	h.emu = gceemu.New()
	t.Cleanup(h.emu.Close)
	h.emu.AddImage("base", "", "X86_64")
	api, err := gce.NewService(context.Background(), h.emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	inst := &gce.Instance{Name: "lsr-5001-abcdefgh", MachineType: "zones/" + zone + "/machineTypes/n4-highcpu-2",
		Metadata: &gce.Metadata{Items: []*gce.MetadataItems{{Key: "lsr-service", Value: new(srv.URL)}}},
		Disks: []*gce.AttachedDisk{{Boot: true, InitializeParams: &gce.AttachedDiskInitializeParams{
			SourceImage: "global/images/base", DiskType: "zones/" + zone + "/diskTypes/hyperdisk-balanced"}}},
		ServiceAccounts: []*gce.ServiceAccount{{Email: "lanescape-runners-machine@lanescape-dev.iam.gserviceaccount.com"}}}
	if _, err := api.Instances.Insert(h.emu.Project, zone, inst).Do(); err != nil {
		t.Fatal(err)
	}
	h.settings = filepath.Join(h.dir, "job-started.conf")
	if err := os.WriteFile(h.settings, []byte("metadata="+h.emu.MetadataURL(zone, inst.Name)+"\nask_every=100ms\nretry_every=100ms\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// run starts the hook as the runner does, with the job's environment: its
// script, with bash, which runs its program, built from its embedded
// source, here from where the test built it.
func (h *hookEnv) run(env ...string) (*exec.Cmd, chan error, *lockedBuffer) {
	h.t.Helper()
	programs, _ := build(h.t)
	script := filepath.Join(h.dir, "job-started.sh")
	text := strings.ReplaceAll(string(machine.Files()["job-started.sh"]), machine.HookProgramPath, programs["hook"])
	if err := os.WriteFile(script, []byte(text), 0o755); err != nil {
		h.t.Fatal(err)
	}
	cmd := exec.Command("bash", script, h.settings)
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.dir}, env...)
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	h.t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, done, out
}

// requests are what the service was sent: bodies and tokens.
func (h *hookEnv) requests() ([]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.asked), slices.Clone(h.tokens)
}

// The hook asks with what GitHub told the runner, returns once allowed, and
// mounts nothing: a work disk comes only from the job's own steps.
func TestTheHookAsksAndReturns(t *testing.T) {
	h := newHookEnv(t)
	event := filepath.Join(h.dir, "event.json")
	_ = os.WriteFile(event, []byte(`{"pull_request": {"number": 12, "head": {"sha": "`+strings.Repeat("c", 40)+`", "repo": {"id": 7001}}}}`), 0o644)
	_, done, out := h.run("GITHUB_RUN_ID=77", "GITHUB_EVENT_NAME=pull_request_target", "GITHUB_REF=refs/heads/main", "GITHUB_REF_TYPE=branch",
		"GITHUB_SHA="+strings.Repeat("a", 40), "GITHUB_EVENT_PATH="+event)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hook failed: %v\n%s", err, out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the hook didn't return once allowed")
	}
	asked, tokens := h.requests()
	var got map[string]any
	if len(asked) != 1 || json.Unmarshal([]byte(asked[0]), &got) != nil {
		t.Fatalf("asked %q", asked)
	}
	told := map[string]any{"run_id": 77.0, "event": "pull_request_target", "ref": "refs/heads/main", "ref_type": "branch",
		"sha": strings.Repeat("a", 40), "pull_request": 12.0, "head_repository_id": 7001.0, "head_sha": strings.Repeat("c", 40)}
	if !reflect.DeepEqual(got, told) {
		t.Fatalf("asked %v, want what GitHub told the runner, %v", got, told)
	}
	tok := tokens[0]
	if strings.Count(tok, ".") != 2 || strings.Contains(out.String(), tok) {
		t.Fatal("no identity token was sent, or it was printed")
	}
}

func TestTheHookNeverReturnsOnARefusal(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := newHookEnv(t)
			h.codes = []int{code}
			_, done, out := h.run()
			select {
			case err := <-done:
				t.Fatalf("the hook returned (%v) on a refusal:\n%s", err, out)
			case <-time.After(3 * time.Second):
			}
			if !strings.Contains(out.String(), "refused") {
				t.Fatalf("the hook said %q", out)
			}
		})
	}
}

func TestTheHookPrintsWhyItsJobFails(t *testing.T) {
	h := newHookEnv(t)
	h.codes = []int{http.StatusForbidden}
	h.body = `{"error": "Forbidden", "reason": "the run's pull request isn't the head of one open pull request"}`
	_, done, out := h.run()
	select {
	case err := <-done:
		t.Fatalf("the hook returned (%v) for a failed job:\n%s", err, out)
	case <-time.After(3 * time.Second):
	}
	if !strings.Contains(out.String(), "the run's pull request isn't the head of one open pull request") {
		t.Fatalf("the hook said %q", out)
	}
}

func TestTheHookAsksAgainWithNoAnswer(t *testing.T) {
	h := newHookEnv(t)
	h.codes = []int{http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusOK}
	_, done, out := h.run()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hook failed: %v\n%s", err, out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the hook never got its answer")
	}
	if asked, _ := h.requests(); len(asked) != 3 {
		t.Fatalf("asked %d times, want 3", len(asked))
	}
}

func TestTheJobsEnvironmentCantRedirectTheHook(t *testing.T) {
	h := newHookEnv(t)
	curlHome := filepath.Join(h.dir, "curl")
	_ = os.MkdirAll(curlHome, 0o755)
	_ = os.WriteFile(filepath.Join(curlHome, ".curlrc"), []byte("proxy = \"http://127.0.0.1:9\"\n"), 0o644)
	_, done, out := h.run("http_proxy=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9", "all_proxy=http://127.0.0.1:9",
		"HTTPS_PROXY=http://127.0.0.1:9", "CURL_HOME="+curlHome, "GITHUB_RUN_ID=1; touch /tmp/x")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hook failed: %v\n%s", err, out)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the hook was redirected:\n%s", out)
	}
	var got struct {
		RunID int64 `json:"run_id"`
	}
	if asked, _ := h.requests(); json.Unmarshal([]byte(asked[0]), &got) != nil || got.RunID != 0 {
		t.Fatalf("asked %q; a run ID that isn't a number is sent as 0", asked[0])
	}
}

func TestTheJobsEnvironmentCantTraceTheToken(t *testing.T) {
	h := newHookEnv(t)
	_, done, out := h.run("SHELLOPTS=xtrace", "BASH_XTRACEFD=2", "PS4=+ ", "GODEBUG=http2debug=2,http2client=1", "GOTRACEBACK=all")
	if err := <-done; err != nil {
		t.Fatalf("hook failed: %v\n%s", err, out)
	}
	_, tokens := h.requests()
	if len(tokens) == 0 || strings.Contains(out.String(), tokens[0]) || strings.Contains(out.String(), "eyJ") {
		t.Fatalf("the hook traced its token:\n%s", out)
	}
}
