package machine_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

// bins builds the supervisor, from its embedded source alone, and the
// runner stand-in, once for every test.
var bins struct {
	once       sync.Once
	dir        string
	supervisor string
	standIn    string
	err        error
}

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

func build(t *testing.T) (supervisor, standIn string) {
	t.Helper()
	bins.once.Do(func() {
		bins.dir, bins.err = os.MkdirTemp("", "lanescape-runners-machine-")
		if bins.err != nil {
			return
		}
		// The supervisor builds from the embedded files and nothing else.
		src := filepath.Join(bins.dir, "src")
		for path, b := range machine.Files() {
			if !strings.HasPrefix(path, "supervisor/") {
				continue
			}
			p := filepath.Join(src, strings.TrimPrefix(path, "supervisor/"))
			if bins.err = os.MkdirAll(filepath.Dir(p), 0o755); bins.err != nil {
				return
			}
			if bins.err = os.WriteFile(p, b, 0o644); bins.err != nil {
				return
			}
		}
		if bins.err = os.WriteFile(filepath.Join(src, "go.mod"), []byte("module supervisor\n\ngo 1.27\n"), 0o644); bins.err != nil {
			return
		}
		bins.supervisor = filepath.Join(bins.dir, "supervisor")
		cmd := exec.Command(goTool(), "build", "-o", bins.supervisor, ".")
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			bins.err = fmt.Errorf("building the supervisor from its embedded source: %v\n%s", err, out)
			return
		}
		bins.standIn = filepath.Join(bins.dir, "standin")
		cmd = exec.Command(goTool(), "build", "-o", bins.standIn, "github.com/lanescape/runners/emulators/github/cmd/standin")
		if out, err := cmd.CombinedOutput(); err != nil {
			bins.err = fmt.Errorf("building the runner stand-in: %v\n%s", err, out)
		}
	})
	if bins.err != nil {
		t.Fatal(bins.err)
	}
	return bins.supervisor, bins.standIn
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
	// Every file of the supervisor's is embedded.
	srcs, _ := filepath.Glob("supervisor/*.go")
	for _, s := range srcs {
		if !strings.HasSuffix(s, "_test.go") {
			if _, ok := fs[filepath.ToSlash(s)]; !ok {
				t.Errorf("%s isn't embedded", s)
			}
		}
	}
	if _, ok := fs["job-started.sh"]; !ok {
		t.Error("the hook isn't embedded")
	}
	// The supervisor's defaults are the paths the base image gives.
	sup := string(fs["supervisor/main.go"])
	for _, p := range []string{machine.HookPath, machine.SaveMarker} {
		if !strings.Contains(sup, `"`+p+`"`) {
			t.Errorf("the supervisor's defaults don't name %s", p)
		}
	}
	if d := machine.Digest(); len(d) != 64 || d != machine.Digest() {
		t.Fatalf("digest = %q", d)
	}
	build(t)
}

// fakeBin is a directory with a sudo that records what it was asked and
// does only what a test needs: mkdir, and blkid that finds a filesystem
// only if told to.
func fakeBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sudo := `#!/bin/bash
[ "$1" = -n ] || exit 1
shift
echo "$*" >> "$SUDO_LOG"
case $1 in
blkid) [ -n "${HAS_FS:-}" ] ;;
mkdir) "$@" ;;
*) exit 0 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// hookEnv is one machine for the hook to run on, and a service it asks.
type hookEnv struct {
	t        *testing.T
	emu      *gceemu.Emulator
	dir      string
	settings string
	sudoLog  string
	device   string
	work     string

	mu     sync.Mutex
	codes  []int
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
	h.device = filepath.Join(h.dir, "google-work")
	h.work = filepath.Join(h.dir, "work")
	h.sudoLog = filepath.Join(h.dir, "sudo.log")
	h.settings = filepath.Join(h.dir, "job-started.conf")
	conf := fmt.Sprintf("metadata=%s\ndevice=%s\nmountpoint=%s\n", h.emu.MetadataURL(zone, inst.Name), h.device, h.work)
	if err := os.WriteFile(h.settings, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.device, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// run starts the hook as the runner does, with the job's environment.
func (h *hookEnv) run(env ...string) (*exec.Cmd, chan error, *lockedBuffer) {
	h.t.Helper()
	cmd := exec.Command("bash", "job-started.sh", h.settings)
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append([]string{"PATH=" + fakeBin(h.t) + ":" + os.Getenv("PATH"), "SUDO_LOG=" + h.sudoLog, "HOME=" + h.dir}, env...)
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

func (h *hookEnv) sudo() string {
	b, _ := os.ReadFile(h.sudoLog)
	return string(b)
}

func TestTheHookMountsTheWorkDisk(t *testing.T) {
	h := newHookEnv(t)
	_, done, out := h.run("GITHUB_RUN_ID=77")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hook failed: %v\n%s", err, out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the hook didn't return once allowed")
	}
	want := []string{
		"blkid -p " + h.device,
		"mkfs.ext4 -q -m 0 -L work " + h.device,
		"mkdir -p " + h.work,
		"mount -o discard,noatime " + h.device + " " + h.work,
		fmt.Sprintf("chown %d:%d %s", os.Getuid(), os.Getgid(), h.work),
	}
	if got := strings.Split(strings.TrimSpace(h.sudo()), "\n"); !slices.Equal(got, want) {
		t.Fatalf("the hook did %q, want %q", got, want)
	}
	asked, tokens := h.requests()
	if len(asked) != 1 || asked[0] != `{"run_id": 77}` {
		t.Fatalf("asked %q", asked)
	}
	tok := tokens[0]
	if strings.Count(tok, ".") != 2 || strings.Contains(out.String(), tok) {
		t.Fatal("no identity token was sent, or it was printed")
	}
}

func TestTheHookKeepsAFilesystem(t *testing.T) {
	h := newHookEnv(t)
	_, done, out := h.run("HAS_FS=1")
	if err := <-done; err != nil {
		t.Fatalf("hook failed: %v\n%s", err, out)
	}
	if strings.Contains(h.sudo(), "mkfs") {
		t.Fatal("a disk with a filesystem was formatted")
	}
}

func TestTheHookNeverReturnsOnARefusal(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := newHookEnv(t)
			h.codes = []int{code}
			_, done, out := h.run()
			select {
			case err := <-done:
				t.Fatalf("the hook returned (%v) on a refusal:\n%s", err, out)
			case <-time.After(3 * time.Second):
			}
			if h.sudo() != "" {
				t.Fatal("a refused hook mounted something")
			}
			if !strings.Contains(out.String(), "refused") {
				t.Fatalf("the hook said %q", out)
			}
		})
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
		"CURL_HOME="+curlHome, "GITHUB_RUN_ID=1; touch /tmp/x")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hook failed: %v\n%s", err, out)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the hook was redirected:\n%s", out)
	}
	if asked, _ := h.requests(); asked[0] != `{"run_id": 0}` {
		t.Fatalf("asked %q; a run ID that isn't a number is sent as 0", asked[0])
	}
}

func TestTheHookWaitsForItsDisk(t *testing.T) {
	h := newHookEnv(t)
	os.Remove(h.device)
	// The disk appears after a while, as udev makes its link.
	go func() {
		time.Sleep(2 * time.Second)
		_ = os.WriteFile(h.device, nil, 0o644)
	}()
	_, done, out := h.run()
	if err := <-done; err != nil {
		t.Fatalf("hook failed waiting for its disk: %v\n%s", err, out)
	}
}
