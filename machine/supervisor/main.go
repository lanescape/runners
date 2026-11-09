// Command supervisor is the service's own program on every machine
// (specs/0002-machines.md, A machine's life). It registers the machine's
// one-job GitHub runner from the just-in-time configuration the service put
// in the machine's metadata, starts it, and waits for it to exit. Then it
// reads whether the job asked to save its work disk, from the marker the
// save action leaves on the boot disk, reports to the service, and powers
// the machine off.
//
// It runs outside every job, as its own user, with its own environment and
// paths: nothing a job sets reaches it, and it acts only after the runner
// has exited, by which time GitHub has the job's result. Its settings come
// from the base image, never from a consumer.
//
// It uses only Go's standard library, so that it builds from its own source
// alone wherever the base image is made.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Config is the supervisor's settings, from the base image.
type Config struct {
	// Metadata is the metadata server's address.
	Metadata string `json:"metadata"`
	// Runner is the runner's command, which reads its just-in-time
	// configuration from ACTIONS_RUNNER_INPUT_JITCONFIG.
	Runner    []string `json:"runner"`
	RunnerDir string   `json:"runner_dir"`
	// RunnerUser runs the runner, if the supervisor can switch to it.
	RunnerUser string `json:"runner_user"`
	// Env is the runner's environment, besides what the supervisor sets.
	Env []string `json:"env"`
	// Hook is the job-started hook the runner runs.
	Hook string `json:"hook"`
	// SaveMarker is where the save action asks for the work disk to be
	// saved, writing {"save": true}.
	SaveMarker string `json:"save_marker"`
	// PowerOff is the command that powers the machine off.
	PowerOff []string `json:"poweroff"`
	// MetadataFor and ReportFor are how long it keeps trying to read its
	// metadata, and to report.
	MetadataFor duration `json:"metadata_for"`
	ReportFor   duration `json:"report_for"`
}

type duration time.Duration

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = duration(v)
	return err
}

// Defaults are the base image's layout (specs/0003-disks.md, Base images).
func Defaults() Config {
	return Config{
		Metadata:    "http://metadata.google.internal",
		Runner:      []string{"/opt/actions-runner/run.sh"},
		RunnerDir:   "/opt/actions-runner",
		RunnerUser:  "runner",
		Env:         []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"},
		Hook:        "/usr/local/lib/lanescape-runners/job-started.sh",
		SaveMarker:  "/var/lib/lanescape-runners/save",
		PowerOff:    []string{"systemctl", "poweroff"},
		MetadataFor: duration(5 * time.Minute),
		ReportFor:   duration(5 * time.Minute),
	}
}

// The metadata the service gives each machine.
const (
	metaService   = "lsr-service"
	metaJITConfig = "jit-config"
)

const finishedPath = "/v1/jobs/finished"

func main() {
	path := flag.String("config", "/etc/lanescape-runners/supervisor.json", "the supervisor's settings")
	flag.Parse()
	logger := log.New(os.Stdout, "lanescape-runners supervisor: ", 0)
	cfg := Defaults()
	if b, err := os.ReadFile(*path); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			logger.Printf("settings %s: %v", *path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		logger.Printf("settings %s: %v", *path, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	supervise(ctx, cfg, logger, os.Stdout)
}

// metadata reads and writes the machine's metadata.
type metadata struct {
	base string
	hc   *http.Client
}

func (m metadata) do(ctx context.Context, method, path string, body io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(m.base, "/")+"/computeMetadata/v1/"+path, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := m.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata %s: %s", strings.SplitN(path, "?", 2)[0], resp.Status)
	}
	return string(b), nil
}

// attribute reads one of the instance's attributes, trying again until it
// has it or for.
func (m metadata) attribute(ctx context.Context, key string, keepTrying time.Duration) (string, error) {
	end := time.Now().Add(keepTrying)
	for {
		v, err := m.do(ctx, http.MethodGet, "instance/attributes/"+key, nil)
		if err == nil && v != "" {
			return v, nil
		}
		if err == nil {
			err = fmt.Errorf("metadata attribute %s is empty", key)
		}
		if time.Now().After(end) || ctx.Err() != nil {
			return "", err
		}
		sleep(ctx, 2*time.Second)
	}
}

// identity is the machine's identity token for the service, in the full
// format. It's a credential, and never logged.
func (m metadata) identity(ctx context.Context, audience string) (string, error) {
	q := url.Values{"audience": {audience}, "format": {"full"}}
	return m.do(ctx, http.MethodGet, "instance/service-accounts/default/identity?"+q.Encode(), nil)
}

// state records how far the supervisor has got in a guest attribute, for
// whoever reads the machine's state.
func (m metadata) state(ctx context.Context, s string) {
	_, _ = m.do(ctx, http.MethodPut, "instance/guest-attributes/lanescape-runners/state", strings.NewReader(s))
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// supervise is the machine's life: run the runner, report, power off. The
// machine powers off however it goes.
func supervise(ctx context.Context, cfg Config, logger *log.Logger, out io.Writer) {
	defer powerOff(cfg, logger)
	md := metadata{base: cfg.Metadata, hc: &http.Client{Timeout: 30 * time.Second}}
	md.state(ctx, "starting")
	service, err := md.attribute(ctx, metaService, time.Duration(cfg.MetadataFor))
	if err != nil {
		logger.Printf("no service to report to: %v", err)
		return
	}
	jit, err := md.attribute(ctx, metaJITConfig, time.Duration(cfg.MetadataFor))
	if err != nil {
		logger.Printf("no runner configuration: %v", err)
		md.state(ctx, "reporting")
		report(ctx, md, service, false, cfg, logger)
		return
	}
	// A request from before the job isn't the job's.
	if err := os.Remove(cfg.SaveMarker); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Printf("removing a stale save request: %v", err)
	}
	md.state(ctx, "running")
	logger.Printf("starting the runner")
	if err := runRunner(ctx, cfg, jit, out); err != nil {
		logger.Printf("the runner exited: %v", err)
	} else {
		logger.Printf("the runner exited")
	}
	save := asked(cfg.SaveMarker)
	md.state(ctx, "reporting")
	report(ctx, md, service, save, cfg, logger)
	md.state(ctx, "done")
}

// runnerEnv is the runner's environment: the base image's, and what the
// supervisor gives it. Nothing of the supervisor's own environment is passed
// on.
func runnerEnv(cfg Config, jit string, home string) []string {
	env := append([]string(nil), cfg.Env...)
	if home != "" {
		env = append(env, "HOME="+home)
	}
	return append(env,
		"ACTIONS_RUNNER_INPUT_JITCONFIG="+jit,
		"ACTIONS_RUNNER_HOOK_JOB_STARTED="+cfg.Hook,
	)
}

// runRunner runs the runner until it exits, as the runner's user when the
// supervisor may switch to it.
func runRunner(ctx context.Context, cfg Config, jit string, out io.Writer) error {
	if len(cfg.Runner) == 0 {
		return errors.New("no runner command")
	}
	cmd := exec.CommandContext(ctx, cfg.Runner[0], cfg.Runner[1:]...)
	cmd.Dir = cfg.RunnerDir
	cmd.Stdout, cmd.Stderr = out, out
	home := ""
	if cfg.RunnerUser != "" && os.Geteuid() == 0 {
		u, err := user.Lookup(cfg.RunnerUser)
		if err != nil {
			return fmt.Errorf("runner user: %w", err)
		}
		uid, _ := strconv.ParseUint(u.Uid, 10, 32)
		gid, _ := strconv.ParseUint(u.Gid, 10, 32)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
		home = u.HomeDir
	}
	cmd.Env = runnerEnv(cfg, jit, home)
	cmd.WaitDelay = 10 * time.Second
	return cmd.Run()
}

// maxSaveRequest bounds what's read of the save request, which is a few
// bytes.
const maxSaveRequest = 1 << 10

// asked reports whether the job asked to save its work disk: the file is a
// regular one, not a link, holding the JSON object {"save": true}.
// Anything else in it, or none at all, asks for no save.
func asked(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSaveRequest+1))
	if err != nil || len(data) > maxSaveRequest {
		return false
	}
	var req struct {
		Save *bool `json:"save"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Save == nil || dec.More() {
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		return false
	}
	return *req.Save
}

// report tells the service the job is done, and whether it asked to save,
// trying again while the service has no answer, for as long as the
// settings allow.
func report(ctx context.Context, md metadata, service string, save bool, cfg Config, logger *log.Logger) {
	end := time.Now().Add(time.Duration(cfg.ReportFor))
	body, _ := json.Marshal(map[string]bool{"save": save})
	hc := &http.Client{Timeout: 30 * time.Second}
	wait := time.Second
	for {
		code, err := send(ctx, md, hc, service, body)
		switch {
		case err == nil && code/100 == 2:
			logger.Printf("reported, save %v", save)
			return
		case err == nil && code/100 == 4 && code != http.StatusTooManyRequests:
			logger.Printf("the service refused the report: %d", code)
			return
		case err != nil:
			logger.Printf("reporting: %v", err)
		default:
			logger.Printf("reporting: the service answered %d", code)
		}
		if time.Now().Add(wait).After(end) || ctx.Err() != nil {
			logger.Printf("giving up on reporting")
			return
		}
		sleep(ctx, wait)
		wait = min(2*wait, 30*time.Second)
	}
}

func send(ctx context.Context, md metadata, hc *http.Client, service string, body []byte) (int, error) {
	tok, err := md.identity(ctx, service)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(service, "/")+finishedPath, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func powerOff(cfg Config, logger *log.Logger) {
	if len(cfg.PowerOff) == 0 {
		return
	}
	logger.Printf("powering off")
	if out, err := exec.Command(cfg.PowerOff[0], cfg.PowerOff[1:]...).CombinedOutput(); err != nil {
		logger.Printf("powering off: %v: %s", err, bytes.TrimSpace(out))
	}
}
