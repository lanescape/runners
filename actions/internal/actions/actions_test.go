package actions

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntime(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"GITHUB_OUTPUT": filepath.Join(dir, "output"), "GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"),
		"INPUT_FAMILY_NAME": "  build\n", "INPUT_ALLOW_UNSAFE": "TRUE", "INPUT_BAD": "yes",
	}
	var log bytes.Buffer
	rt := &Runtime{Getenv: func(k string) string { return env[k] }, Stdout: &log}
	if rt.Input("family-name") != "build" {
		t.Errorf("input = %q", rt.Input("family-name"))
	}
	if v, err := rt.BoolInput("allow-unsafe", false); err != nil || !v {
		t.Errorf("bool = %v, %v", v, err)
	}
	if v, err := rt.BoolInput("missing", true); err != nil || !v {
		t.Errorf("default = %v, %v", v, err)
	}
	if _, err := rt.BoolInput("bad", false); err == nil {
		t.Error("yes was taken for a boolean")
	}
	if err := rt.SetOutput("multi", "{\"a\":\n1}"); err != nil {
		t.Fatal(err)
	}
	if err := rt.SetOutput("other", "x"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(env["GITHUB_OUTPUT"])
	lines := strings.Split(string(out), "\n")
	if len(lines) != 8 || !strings.HasPrefix(lines[0], "multi<<lanescape_") || lines[1] != `{"a":` || lines[2] != "1}" || lines[3] != strings.TrimPrefix(lines[0], "multi<<") {
		t.Errorf("output file = %q", out)
	}
	if err := rt.Summary("# a\n"); err != nil {
		t.Fatal(err)
	}
	if err := rt.Summary("b\n"); err != nil {
		t.Fatal(err)
	}
	if s, _ := os.ReadFile(env["GITHUB_STEP_SUMMARY"]); string(s) != "# a\nb\n" {
		t.Errorf("summary = %q", s)
	}
	rt.Mask("one\ntwo\n")
	if log.String() != "::add-mask::one\n::add-mask::two\n" {
		t.Errorf("log = %q", log.String())
	}
}

func TestSetOutputNeedsTheFile(t *testing.T) {
	rt := &Runtime{Getenv: func(string) string { return "" }, Stdout: &bytes.Buffer{}}
	if err := rt.SetOutput("a", "b"); err == nil {
		t.Fatal("no error without GITHUB_OUTPUT")
	}
}
