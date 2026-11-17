// Package actions is what a step's program gets from GitHub Actions and
// gives back: its environment, its inputs, its outputs, its summary, and
// the masking of values the log must never show.
//
// Composite actions get no INPUT_ variables of their own, so each
// action.yml passes its inputs to its program as INPUT_<NAME> in the
// step's env, in upper case with hyphens as underscores.
package actions

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Runtime is a step's view of the runner. Tests fill it in; Default reads
// the process's own environment.
type Runtime struct {
	Getenv func(string) string
	// Stdout is the step's log, where workflow commands such as
	// ::add-mask:: also go.
	Stdout io.Writer

	// mu keeps lines whole when goroutines log at once.
	mu sync.Mutex
}

// Default is the process's own runtime.
func Default() *Runtime {
	return &Runtime{Getenv: os.Getenv, Stdout: os.Stdout}
}

// Env is an environment variable.
func (r *Runtime) Env(name string) string { return r.Getenv(name) }

// Input is an input of the action, as the action.yml passed it.
func (r *Runtime) Input(name string) string {
	return strings.TrimSpace(r.Getenv("INPUT_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))))
}

// BoolInput is a true or false input; empty is def.
func (r *Runtime) BoolInput(name string, def bool) (bool, error) {
	switch v := strings.ToLower(r.Input(name)); v {
	case "":
		return def, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("input %s is %q; it must be true or false", name, v)
	}
}

// Logf writes a line to the step's log. Never pass it a credential.
func (r *Runtime) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.Stdout, format+"\n", args...)
}

// Mask asks the runner to hide value wherever the log would show it. The
// runner reads the command and doesn't echo it.
func (r *Runtime) Mask(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for line := range strings.SplitSeq(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			fmt.Fprintf(r.Stdout, "::add-mask::%s\n", line)
		}
	}
}

// SetOutput sets one of the step's outputs, in $GITHUB_OUTPUT, with a
// delimiter the value can't contain.
func (r *Runtime) SetOutput(name, value string) error {
	path := r.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return errors.New("GITHUB_OUTPUT isn't set; this program runs only as a step of an action")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	delim := "lanescape_" + hex.EncodeToString(b[:])
	if strings.Contains(name, "\n") || strings.Contains(value, delim) {
		return fmt.Errorf("output %s can't be written", name)
	}
	return appendFile(path, fmt.Sprintf("%s<<%s\n%s\n%s\n", name, delim, value, delim))
}

// Summary appends Markdown to the job's summary. Without a summary file it
// writes to the log instead.
func (r *Runtime) Summary(markdown string) error {
	path := r.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		_, err := io.WriteString(r.Stdout, markdown)
		return err
	}
	return appendFile(path, markdown)
}

func appendFile(path, s string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Fail ends the step with an error, as an error annotation.
func Fail(err error) {
	msg := strings.ReplaceAll(err.Error(), "%", "%25")
	msg = strings.ReplaceAll(msg, "\r", "%0D")
	msg = strings.ReplaceAll(msg, "\n", "%0A")
	fmt.Fprintf(os.Stdout, "::error::%s\n", msg)
	os.Exit(1)
}
