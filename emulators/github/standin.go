package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StandIn plays a just-in-time runner's part against the emulator, as the
// GitHub runner does on a machine: it registers with its just-in-time
// configuration, waits for GitHub to hand it a job, runs the job-started hook
// with the job's environment as the runner does, before any step, then the
// job's steps, reports the job's result and exits. A hook that fails fails the
// job, and its steps don't run; one that never returns holds the job until
// it's cancelled or the runner is killed.
type StandIn struct {
	// JITConfig is the encoded just-in-time configuration, which the runner
	// reads from ACTIONS_RUNNER_INPUT_JITCONFIG.
	JITConfig string
	// Hook is the job-started hook, from ACTIONS_RUNNER_HOOK_JOB_STARTED.
	Hook string
	// Env is the runner's own environment, which the hook and steps
	// inherit, without the runner's ACTIONS_RUNNER_INPUT_ inputs.
	Env []string
	// Dir is the runner's directory; its work folder is under it.
	Dir    string
	Stdout io.Writer
	Stderr io.Writer
	// Poll is how often it checks whether its job was cancelled.
	Poll time.Duration
}

// StandInFromEnv is a stand-in configured as the runner is, from its
// environment.
func StandInFromEnv() StandIn {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ACTIONS_RUNNER_INPUT_") {
			env = append(env, kv)
		}
	}
	dir, _ := os.Getwd()
	return StandIn{JITConfig: os.Getenv("ACTIONS_RUNNER_INPUT_JITCONFIG"), Hook: os.Getenv("ACTIONS_RUNNER_HOOK_JOB_STARTED"),
		Env: env, Dir: dir, Stdout: os.Stdout, Stderr: os.Stderr}
}

func decodeJIT(encoded string) (jitConfig, error) {
	var files map[string]string
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || json.Unmarshal(raw, &files) != nil {
		return jitConfig{}, errors.New("the just-in-time configuration isn't one GitHub made")
	}
	runner, err := base64.StdEncoding.DecodeString(files[".runner"])
	var c jitConfig
	if err != nil || json.Unmarshal(runner, &c) != nil || c.URL == "" || c.Token == "" {
		return jitConfig{}, errors.New("the just-in-time configuration has no runner")
	}
	return c, nil
}

type standInJob struct {
	ID    int64             `json:"id"`
	RunID int64             `json:"run_id"`
	Name  string            `json:"name"`
	Env   map[string]string `json:"env"`
	Steps string            `json:"steps"`
}

// Run registers, takes one job, runs it and reports it. It returns once the
// job is reported, or with ctx's error if it's stopped first.
func (s StandIn) Run(ctx context.Context) error {
	cfg, err := decodeJIT(s.JITConfig)
	if err != nil {
		return err
	}
	if s.Poll == 0 {
		s.Poll = 200 * time.Millisecond
	}
	if s.Stdout == nil {
		s.Stdout = io.Discard
	}
	if s.Stderr == nil {
		s.Stderr = io.Discard
	}
	call := func(ctx context.Context, method, path string, body any) (*http.Response, error) {
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, cfg.URL+path, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "RemoteAuth "+cfg.Token)
		return http.DefaultClient.Do(req)
	}
	resp, err := call(ctx, http.MethodPost, "/_runner/connect", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("registering %s: %s", cfg.Name, resp.Status)
	}
	fmt.Fprintf(s.Stdout, "√ Connected to GitHub as %s\nListening for Jobs\n", cfg.Name)

	var job standInJob
	for {
		resp, err := call(ctx, http.MethodPost, "/_runner/acquire?wait=5s", nil)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(s.Poll)
			continue
		}
		if resp.StatusCode == http.StatusOK {
			err = json.NewDecoder(resp.Body).Decode(&job)
			resp.Body.Close()
			if err != nil {
				return err
			}
			break
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("waiting for a job: %s", resp.Status)
		}
	}
	fmt.Fprintf(s.Stdout, "Running job: %s\n", job.Name)

	// The job's processes stop when it's cancelled, or when the runner is.
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			select {
			case <-jctx.Done():
				return
			case <-time.After(s.Poll):
			}
			resp, err := call(jctx, http.MethodGet, fmt.Sprintf("/_runner/jobs/%d", job.ID), nil)
			if err != nil {
				continue
			}
			var st struct {
				Cancelling bool `json:"cancelling"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&st)
			resp.Body.Close()
			if st.Cancelling {
				cancel()
				return
			}
		}
	}()

	env := append([]string(nil), s.Env...)
	for k, v := range job.Env {
		env = append(env, k+"="+v)
	}
	work := filepath.Join(s.Dir, cfg.WorkDir)
	if filepath.IsAbs(cfg.WorkDir) {
		work = cfg.WorkDir
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	runBash := func(args ...string) error {
		cmd := exec.CommandContext(jctx, "bash", args...)
		cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = env, work, s.Stdout, s.Stderr
		cmd.WaitDelay = time.Second
		return cmd.Run()
	}
	conclusion := "success"
	if s.Hook != "" {
		fmt.Fprintf(s.Stdout, "A job started hook has been configured by the self-hosted runner administrator\nRun '%s'\n", s.Hook)
		if err := runBash(s.Hook); err != nil {
			conclusion = "failure"
		}
	}
	if conclusion == "success" && job.Steps != "" {
		if err := runBash("-c", job.Steps); err != nil {
			conclusion = "failure"
		}
	}
	if ctx.Err() != nil {
		// Killed with its job: it reports nothing.
		return ctx.Err()
	}
	cancel()
	for {
		resp, err := call(ctx, http.MethodPost, fmt.Sprintf("/_runner/jobs/%d/complete", job.ID), map[string]string{"conclusion": conclusion})
		if err == nil {
			resp.Body.Close()
			fmt.Fprintf(s.Stdout, "Job %s completed with result: %s\n", job.Name, conclusion)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(s.Poll)
	}
}
