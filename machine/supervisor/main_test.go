package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gce "google.golang.org/api/compute/v1"

	gceemu "github.com/lanescape/runners/emulators/compute"
)

const (
	zone = "northamerica-northeast1-a"
	jit  = "eyJqaXQiOiJjb25maWcifQ=="
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// service is the report's endpoint, answering with the codes it's given,
// then 204.
type service struct {
	mu      sync.Mutex
	codes   []int
	reports []string
	tokens  []string
}

func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path != finishedPath {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	s.reports = append(s.reports, string(body))
	s.tokens = append(s.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	code := http.StatusNoContent
	if len(s.codes) > 0 {
		code, s.codes = s.codes[0], s.codes[1:]
	}
	w.WriteHeader(code)
}

type env struct {
	t       *testing.T
	emu     *gceemu.Emulator
	svc     *service
	url     string
	dir     string
	cfg     Config
	out     *lockedBuffer
	poweron string
}

// setup makes a machine whose metadata names the service and holds a
// runner's configuration, unless jitConfig is empty, and a runner script
// that records what it was given and runs steps.
func setup(t *testing.T, jitConfig, steps string) *env {
	t.Helper()
	emu := gceemu.New()
	t.Cleanup(emu.Close)
	emu.AddImage("base", "", "X86_64")
	svc := &service{}
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	items := []*gce.MetadataItems{{Key: metaService, Value: new(srv.URL)}, {Key: "enable-guest-attributes", Value: new("TRUE")}}
	if jitConfig != "" {
		items = append(items, &gce.MetadataItems{Key: metaJITConfig, Value: new(jitConfig)})
	}
	api, err := gce.NewService(context.Background(), emu.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	inst := &gce.Instance{Name: "lsr-5001-abcdefgh", MachineType: "zones/" + zone + "/machineTypes/n4-highcpu-2",
		Metadata: &gce.Metadata{Items: items},
		Disks: []*gce.AttachedDisk{{Boot: true, InitializeParams: &gce.AttachedDiskInitializeParams{
			SourceImage: "global/images/base", DiskType: "zones/" + zone + "/diskTypes/hyperdisk-balanced"}}},
		ServiceAccounts: []*gce.ServiceAccount{{Email: "lanescape-runners-machine@lanescape-dev.iam.gserviceaccount.com"}}}
	if _, err := api.Instances.Insert(emu.Project, zone, inst).Do(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	runner := filepath.Join(dir, "run.sh")
	script := "#!/bin/bash\nenv > \"$PWD/env\"\n" + steps + "\n"
	if err := os.WriteFile(runner, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	md := emu.MetadataURL(zone, inst.Name)
	cfg := Defaults()
	cfg.Metadata = md
	cfg.Runner = []string{runner}
	cfg.RunnerDir = dir
	cfg.RunnerUser = ""
	cfg.Env = []string{"PATH=" + os.Getenv("PATH")}
	cfg.Hook = "/opt/lanescape-runners/job-started.sh"
	cfg.PowerOff = []string{"curl", "-fsS", "-X", "POST", md + "/_emulator/poweroff"}
	cfg.MetadataFor = duration(time.Second)
	cfg.ReportFor = duration(10 * time.Second)
	return &env{t: t, emu: emu, svc: svc, url: srv.URL, dir: dir, cfg: cfg, out: &lockedBuffer{}}
}

func (e *env) supervise() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	supervise(ctx, e.cfg, log.New(e.out, "supervisor: ", 0), e.out)
}

func (e *env) poweredOff() bool {
	st, _ := e.emu.Instance(zone, "lsr-5001-abcdefgh")
	return st.Status == "TERMINATED"
}

func (e *env) runnerEnv() map[string]string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "env"))
	if err != nil {
		e.t.Fatal("the runner didn't run")
	}
	out := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			out[k] = v
		}
	}
	return out
}

// The report says only that the job is done: an empty object, signed with
// the machine's identity token. Whether the disk is saved is the job's to
// ask from its steps (specs/0005-interface.md, Work disks).
func TestAJobsReport(t *testing.T) {
	t.Setenv("SUPERVISOR_SECRET", "not for the runner")
	e := setup(t, jit, `echo '{"save": true}' > "$PWD/save"`)
	e.supervise()
	env := e.runnerEnv()
	if env["ACTIONS_RUNNER_INPUT_JITCONFIG"] != jit || env["ACTIONS_RUNNER_HOOK_JOB_STARTED"] != e.cfg.Hook {
		t.Fatalf("runner's environment = %v", env)
	}
	if _, ok := env["SUPERVISOR_SECRET"]; ok {
		t.Fatal("the supervisor's own environment reached the runner")
	}
	if len(e.svc.reports) != 1 || e.svc.reports[0] != "{}" {
		t.Fatalf("reports = %q, want one empty object", e.svc.reports)
	}
	if !e.poweredOff() {
		t.Fatal("the machine wasn't powered off")
	}
	tok := e.svc.tokens[0]
	if tok == "" || strings.Contains(e.out.String(), tok) || strings.Contains(e.out.String(), jit) {
		t.Fatal("no token was sent, or a credential was logged")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || !strings.Contains(decode(t, parts[1]), `"compute_engine"`) || !strings.Contains(decode(t, parts[1]), e.url) {
		t.Fatal("the report isn't signed with a full-format identity token for the service")
	}
	if v, ok := e.guest(); !ok || v != "done" {
		t.Fatalf("guest attribute state = %q", v)
	}
}

func decode(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *env) guest() (string, bool) {
	api, _ := gce.NewService(context.Background(), e.emu.ClientOptions()...)
	ga, err := api.Instances.GetGuestAttributes(e.emu.Project, zone, "lsr-5001-abcdefgh").VariableKey("lanescape-runners/state").Do()
	if err != nil {
		return "", false
	}
	return ga.VariableValue, true
}

func TestAFailedJobIsReported(t *testing.T) {
	e := setup(t, jit, "exit 3")
	e.supervise()
	if len(e.svc.reports) != 1 || e.svc.reports[0] != "{}" {
		t.Fatalf("reports = %q", e.svc.reports)
	}
	if !e.poweredOff() {
		t.Fatal("not powered off")
	}
}

func TestTheReportIsSentAgain(t *testing.T) {
	e := setup(t, jit, "")
	e.svc.codes = []int{http.StatusServiceUnavailable, http.StatusInternalServerError}
	e.supervise()
	if len(e.svc.reports) != 3 || !e.poweredOff() {
		t.Fatalf("%d reports, powered off %v; want 3 and powered off", len(e.svc.reports), e.poweredOff())
	}
}

func TestARefusedReportIsntSentAgain(t *testing.T) {
	e := setup(t, jit, "")
	e.svc.codes = []int{http.StatusForbidden}
	e.supervise()
	if len(e.svc.reports) != 1 || !e.poweredOff() {
		t.Fatalf("%d reports, powered off %v", len(e.svc.reports), e.poweredOff())
	}
}

func TestNoRunnerConfiguration(t *testing.T) {
	e := setup(t, "", "")
	e.supervise()
	if _, err := os.Stat(filepath.Join(e.dir, "env")); err == nil {
		t.Fatal("a runner ran with no configuration")
	}
	if len(e.svc.reports) != 1 || e.svc.reports[0] != "{}" || !e.poweredOff() {
		t.Fatalf("reports = %q; want one, and the machine powered off", e.svc.reports)
	}
}

func TestSettings(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"report_for": "90s", "runner": ["x"]}`), &c); err != nil ||
		time.Duration(c.ReportFor) != 90*time.Second || len(c.Runner) != 1 {
		t.Fatalf("settings = %+v, %v", c, err)
	}
	// The hook the runner calls is the service's own, in its own
	// directory (specs/0002-machines.md, A machine's life).
	if d := Defaults(); !strings.HasPrefix(d.Hook, "/opt/lanescape-runners/") {
		t.Fatalf("the base image's hook is %s", d.Hook)
	}
}
