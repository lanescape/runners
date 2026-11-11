package machine_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lanescape/runners/machine"
)

// untar reads Archive's gzipped tar into a map of its files.
func untar(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg || h.Uid != 0 || h.Gid != 0 || strings.Contains(h.Name, "..") || strings.HasPrefix(h.Name, "/") {
			t.Fatalf("archive entry %+v isn't a plain file of root's under the archive", h)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = data
	}
	return out
}

// TestTheBuildersArchive checks what the builder installs of the service's
// own: every program's sources and every installed file, with where each
// goes, and nothing else; and that each program builds statically from its
// sources in the archive alone, as the builder builds it
// (specs/0003-disks.md, Making a base image).
func TestTheBuildersArchive(t *testing.T) {
	b, err := machine.Archive()
	if err != nil {
		t.Fatal(err)
	}
	again, err := machine.Archive()
	if err != nil || !bytes.Equal(b, again) {
		t.Fatal("the archive isn't the same for the same files, so the builder's metadata would change for nothing")
	}
	// Compute Engine allows 256 KiB in one metadata value, which holds it
	// in base64.
	if n := (len(b) + 2) / 3 * 4; n > 256<<10 {
		t.Fatalf("the archive is %d bytes in base64, over a metadata value's 256 KiB", n)
	}
	got := untar(t, b)
	files := machine.Files()
	want := map[string][]byte{}
	var programs, installs strings.Builder
	for _, p := range machine.Programs() {
		fmt.Fprintf(&programs, "%s %s\n", p.Source, p.Path)
		for path, data := range files {
			if strings.HasPrefix(path, p.Source+"/") {
				want["src/"+path] = data
			}
		}
	}
	for _, in := range machine.Installs() {
		fmt.Fprintf(&installs, "%s %s %o\n", in.File, in.Path, in.Mode)
		want["files/"+in.File] = files[in.File]
	}
	want["programs"], want["installs"] = []byte(programs.String()), []byte(installs.String())
	if len(got) != len(want) {
		t.Errorf("the archive has %d files, want %d", len(got), len(want))
	}
	for name, data := range want {
		if !bytes.Equal(got[name], data) {
			t.Errorf("%s in the archive isn't %q", name, data)
		}
	}
	// The script's checks of each line.
	line := regexp.MustCompile(`^[a-z]+ /opt/lanescape-runners/[a-z-]+$`)
	for _, l := range strings.Split(strings.TrimSpace(programs.String()), "\n") {
		if !line.MatchString(l) {
			t.Errorf("programs line %q fails the script's check", l)
		}
	}
	inst := regexp.MustCompile(`^[a-z0-9.-]+ /[a-z0-9./-]+ [0-7]{3,4}$`)
	for _, l := range strings.Split(strings.TrimSpace(installs.String()), "\n") {
		if !inst.MatchString(l) {
			t.Errorf("installs line %q fails the script's check", l)
		}
	}

	// Each program builds from the archive alone, for Linux, statically,
	// with the builder's settings.
	dir := t.TempDir()
	for _, p := range machine.Programs() {
		src := filepath.Join(dir, p.Source)
		for name, data := range got {
			if rest, ok := strings.CutPrefix(name, "src/"+p.Source+"/"); ok {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(src, rest)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(src, rest), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module "+p.Source+"\n\ngo 1.27\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, arch := range []string{"amd64", "arm64"} {
			cmd := exec.Command(goTool(), "build", "-trimpath", "-o", filepath.Join(dir, p.Source+"-"+arch), ".")
			cmd.Dir = src
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch, "GOWORK=off", "GOFLAGS=-mod=mod")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s doesn't build for linux/%s from the archive alone: %v\n%s", p.Source, arch, err, out)
			}
		}
	}
}

// TestTheBuildersScript checks the builder's script as far as it can be
// checked off a builder: it parses, it says its result in the lines the
// service reads, and it holds to what the base image must and mustn't be.
func TestTheBuildersScript(t *testing.T) {
	script := machine.BuildScript()
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("the script doesn't parse: %v\n%s", err, out)
		}
	}
	if !strings.HasPrefix(script, "#!/bin/bash\n") {
		t.Error("the script doesn't start with #!/bin/bash")
	}
	// Its result lines are the ones the service reads, written to the
	// console, last.
	if !strings.Contains(script, `printf '\n`+machine.ResultPrefix+`%s\n' "$1" >/dev/console`) {
		t.Errorf("the script doesn't write its result as %q to the console", machine.ResultPrefix)
	}
	built := "result \"" + strings.TrimPrefix(machine.Built, machine.ResultPrefix) + "$arch-$key\"\n"
	if !strings.HasSuffix(script, built) {
		t.Errorf("the script doesn't end by saying %q", built)
	}
	if !strings.Contains(script, `result "`+strings.TrimPrefix(machine.Failed, machine.ResultPrefix)+`$1"`) {
		t.Error("the script doesn't say why it failed as the service reads it")
	}
	for _, md := range []string{"lsr-arch", "lsr-key", "lsr-runner-version", "lsr-runner-sha256", "lsr-go-version", "lsr-go-sha256", "lsr-files"} {
		if !strings.Contains(script, "md "+md) {
			t.Errorf("the script doesn't read %s", md)
		}
	}
	// It checks both downloads, installs no Docker, leaves user
	// namespaces alone, never powers the builder off, and checks that
	// the runner's user can make a user namespace.
	for _, want := range []string{
		`echo "$go_sha256  $work/go.tar.gz" | sha256sum -c`,
		`echo "$runner_sha256  $work/runner.tar.gz" | sha256sum -c`,
		`runuser -u runner -- unshare --user --map-root-user true`,
		`install -d -o root -g root -m 0755 /work`,
		`systemctl disable --now apt-daily.timer apt-daily-upgrade.timer`,
		`systemctl mask unattended-upgrades.service`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script doesn't %s", want)
		}
	}
	for _, never := range []string{"sysctl", "apparmor", "docker.io", "docker-ce", "containerd", "poweroff", "shutdown", "reboot", "set -x"} {
		if strings.Contains(script, never) {
			t.Errorf("the script mentions %q", never)
		}
	}
}

// TestTheBuildersScriptSaysWhyItFailed runs the script with Debian's tools
// stood in for, failing in turn: inside a function, in a command
// substitution, and at its own checks. Each time its last word on the
// console is why it failed, never that it built the image.
func TestTheBuildersScriptSaysWhyItFailed(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	for _, tc := range []struct {
		name, aptGet, curl, want string
	}{
		{"apt fails inside its function", "exit 100", "exit 0", "exited 100: apt-get -q"},
		{"the metadata server fails in a command substitution", "exit 0", "exit 22", "exited 22"},
		{"a setting isn't valid", "exit 0", `case "$*" in *lsr-files) printf AAAA ;; *lsr-arch) printf x86 ;; *) printf 0 ;; esac`, "lsr-arch isn't amd64 or arm64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			console := filepath.Join(dir, "console")
			// The script as a builder runs it, but with its console, /opt
			// and /var/tmp in the test's own directory. The console is
			// appended to, as /dev/console is, never truncated.
			script := strings.NewReplacer(">/dev/console", ">>"+console, "/opt/", dir+"/opt/", "/var/tmp/", dir+"/").Replace(machine.BuildScript())
			stubs := filepath.Join(dir, "bin")
			if err := os.Mkdir(stubs, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{
				"systemctl": "exit 0", "getent": "exit 2", "apt-get": tc.aptGet, "curl": tc.curl, "dpkg": "echo amd64",
			} {
				if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(bash, "-c", script)
			cmd.Env = []string{"PATH=" + stubs + ":/usr/bin:/bin", "HOME=" + dir}
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the script succeeded:\n%s", out)
			}
			got, _ := os.ReadFile(console)
			lines := strings.Split(strings.TrimSpace(string(got)), "\n")
			last := lines[len(lines)-1]
			if !strings.HasPrefix(last, machine.Failed) || !strings.Contains(string(got), tc.want) || strings.Contains(string(got), machine.Built) {
				t.Fatalf("console = %q; want it to end saying it failed, with %q\noutput:\n%s", got, tc.want, out)
			}
		})
	}
}
