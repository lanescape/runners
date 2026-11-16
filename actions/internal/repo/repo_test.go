// Package repo checks what holds across the actions and the repository's
// workflows: the actions' metadata, the workflows' pins, what the actions'
// programs import, and the steps that build them and move the release's
// tag.
package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// actionsDir is actions/, and root the repository's.
const (
	actionsDir = "../.."
	root       = "../../.."
)

type action struct {
	Inputs map[string]struct {
		Default string `yaml:"default"`
	} `yaml:"inputs"`
	Runs struct {
		Using string `yaml:"using"`
		Steps []step `yaml:"steps"`
	} `yaml:"runs"`
}

type step struct {
	Name  string            `yaml:"name"`
	ID    string            `yaml:"id"`
	If    string            `yaml:"if"`
	Uses  string            `yaml:"uses"`
	Shell string            `yaml:"shell"`
	Run   string            `yaml:"run"`
	Env   map[string]string `yaml:"env"`
	With  map[string]string `yaml:"with"`
}

// actions are the directories with an action.yml.
func actions(t *testing.T) map[string]action {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(actionsDir, "*", "action.yml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no actions: %v", err)
	}
	out := map[string]action{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var a action
		if err := yaml.Unmarshal(b, &a); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out[filepath.Base(filepath.Dir(p))] = a
	}
	return out
}

// The actions are the ones specs/0007-actions.md has, each with a section
// of its own.
func TestTheActions(t *testing.T) {
	var names []string
	for name := range actions(t) {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"cache", "checkout", "disk", "save-cache"}; !slices.Equal(names, want) {
		t.Fatalf("actions %v, want %v", names, want)
	}
	spec, err := os.ReadFile(filepath.Join(root, "specs", "0007-actions.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if !strings.Contains(string(spec), "\n## `"+name+"`\n") {
			t.Errorf("specs/0007-actions.md has no section for %s", name)
		}
	}
}

// Every action builds its program the same way (specs/0007-actions.md, How
// actions run): the steps up to Build are the same in each.
func TestActionsBuildAlike(t *testing.T) {
	var first []step
	var firstName string
	for name, a := range actions(t) {
		if a.Runs.Using != "composite" {
			t.Errorf("%s isn't a composite action", name)
		}
		i := slices.IndexFunc(a.Runs.Steps, func(s step) bool { return s.ID == "build" })
		if i < 0 {
			t.Fatalf("%s has no build step", name)
		}
		if _, err := os.Stat(filepath.Join(actionsDir, name, "main.go")); err != nil {
			t.Errorf("%s has no program: %v", name, err)
		}
		steps := a.Runs.Steps[:i+1]
		if first == nil {
			first, firstName = steps, name
			continue
		}
		if !slices.EqualFunc(first, steps, func(a, b step) bool {
			return a.Name == b.Name && a.ID == b.ID && a.If == b.If && a.Uses == b.Uses && a.Run == b.Run && a.Shell == b.Shell &&
				equalMaps(a.Env, b.Env) && equalMaps(a.With, b.With)
		}) {
			t.Errorf("%s's build steps differ from %s's", name, firstName)
		}
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Each input reaches the program as INPUT_<NAME>, through the step's env,
// never interpolated into a script.
func TestInputsReachThePrograms(t *testing.T) {
	expr := regexp.MustCompile(`\$\{\{`)
	for name, a := range actions(t) {
		for in := range a.Inputs {
			want := "${{ inputs." + in + " }}"
			key := "INPUT_" + strings.ToUpper(strings.ReplaceAll(in, "-", "_"))
			found := false
			for _, s := range a.Runs.Steps {
				if s.Env[key] == want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: no step passes %s as %s", name, in, key)
			}
		}
		for _, s := range a.Runs.Steps {
			if expr.MatchString(s.Run) {
				t.Errorf("%s: step %q interpolates an expression into its script", name, s.Name)
			}
		}
	}
}

var pinned = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9._-]+(/[a-z0-9._/-]+)?@[0-9a-f]{40}$`)

// Every action used, in the actions and the workflows, is pinned by its
// full commit SHA, with the release it is as a comment.
func TestUsesArePinned(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(actionsDir, "*", "action.yml"))
	wf, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	files = append(files, wf...)
	uses := regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s*(\S+)(.*)$`)
	n := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range uses.FindAllStringSubmatch(string(b), -1) {
			n++
			if strings.HasPrefix(m[1], "./") {
				continue
			}
			if !pinned.MatchString(m[1]) || !regexp.MustCompile(`#\s*v[0-9]+\.[0-9]+\.[0-9]+`).MatchString(m[2]) {
				t.Errorf("%s: %s%s isn't pinned by a full SHA with its release", f, m[1], m[2])
			}
		}
	}
	if n == 0 {
		t.Fatal("found no uses")
	}
}

// The actions' programs rely on nothing of the service's but its interface
// for actions (specs/0007-actions.md, Code and tests): of this module, they
// import only endpoints/ and actions/, with or without the end-to-end
// tests' tag. Their tests may use the emulators and conformance/ too.
func TestTheProgramsImportOnlyTheInterface(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	const module = "github.com/lanescape/runners/"
	for name := range actions(t) {
		for _, tags := range []string{"", "e2e"} {
			cmd := exec.Command("go", "list", "-tags="+tags, "-deps", "-f", "{{.ImportPath}}", "./actions/"+name)
			cmd.Dir = root
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("go list %s: %v", name, err)
			}
			for dep := range strings.FieldsSeq(string(out)) {
				rest, ok := strings.CutPrefix(dep, module)
				if !ok {
					continue
				}
				if rest != "endpoints" && !strings.HasPrefix(rest, "actions/") {
					t.Errorf("%s (tags %q) imports %s", name, tags, dep)
				}
			}
		}
	}
}
