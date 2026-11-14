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
		"\nmake_runner_user\n",
		`[ "$(id -u runner):$(id -g runner)" = "1001:1001" ]`,
		`[ "$(id -Gn runner)" = runner ]`,
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

// scriptFunction is the definition of the builder's script's function
// name, from its first line to its closing brace.
func scriptFunction(t *testing.T, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\) \{\n.*?^\}\n`).FindString(machine.BuildScript())
	if m == "" {
		t.Fatalf("the script has no function %s", name)
	}
	return m
}

// TestTheBuildersScriptMakesTheRunnersUser runs the script's functions that
// make the runner's user, with its own result, failure and ERR trap, against
// account files of the test's own, with Debian's tools that read and write
// them stood in for: groupadd and useradd fail as Debian's do on an ID
// that's taken. Google's guest agent makes google-sudoers, docker and lxd as
// it starts with the next free GIDs from 1000, so docker holds GID 1001: a
// group holding it that nothing depends on moves to the highest free system
// GID, keeping its name, as does one the agent makes at 1001 once it's
// freed, whether before groupadd looks for GID 1001 or after, when both
// hold it; anything else holding UID or GID 1001 fails the build, naming it,
// with nothing changed (specs/0003-disks.md, Making a base image).
func TestTheBuildersScriptMakesTheRunnersUser(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	const (
		debian = "root:x:0:\nadm:x:4:\nsudo:x:27:\nusers:x:100:\nsystemd-journal:x:999:\nsystemd-network:x:998:\nnogroup:x:65534:\n"
		passwd = "root:x:0:0:root:/root:/bin/bash\nnobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin\n"
		guest  = "google-sudoers:x:1000:\ndocker:x:1001:\nlxd:x:1002:\n"
		runner = "runner:x:1001:\n"
		user   = "runner:x:1001:1001::/home/runner:/bin/bash\n"
	)
	var full strings.Builder
	for gid := 101; gid <= 999; gid++ {
		fmt.Fprintf(&full, "sys%d:x:%d:\n", gid, gid)
	}
	// The script's own result, failure, ERR trap and say, from its text.
	prelude := regexp.MustCompile(`(?ms)^result\(\) \{\n.*?^say\(\) \{[^\n]*\}\n`).FindString(machine.BuildScript())
	if !strings.Contains(prelude, "\ntrap 'fail ") {
		t.Fatalf("the script's prelude isn't where the test looks for it:\n%s", prelude)
	}
	stubs := map[string]string{
		// getent DB KEY: the entry whose name or ID is KEY, or exit 2; or
		// fail with what the test's fail-getent-DB-KEY file says.
		"getent": `[ ! -e "$DB/fail-getent-$1-$2" ] || exit "$(cat "$DB/fail-getent-$1-$2")"
awk -F: -v k="$2" '$1 == k || $3 == k { print; found = 1; exit } END { exit !found }' "$DB/$1" || exit 2`,
		// groupmod --gid GID NAME
		"groupmod": `[ ! -e "$DB/fail-groupmod" ] || exit "$(cat "$DB/fail-groupmod")"
echo "groupmod $*" >>"$DB/calls"
awk -F: -v OFS=: -v g="$2" -v n="$3" '$1 == n { $3 = g } { print }' "$DB/group" >"$DB/group.new" && mv "$DB/group.new" "$DB/group"`,
		// groupadd --gid GID NAME, which, as Debian's, looks for the GID
		// before it locks the group file. The guest agent may make the
		// next group of the test's agent file before it looks, or of its
		// agent-during file after, as Debian's groupadd does with no
		// GID: the GID above the highest from 1000.
		"groupadd": `echo "groupadd $*" >>"$DB/calls"
agent() {
	[ -s "$DB/$1" ] || return 0
	g=$(head -n 1 "$DB/$1"); sed 1d "$DB/$1" >"$DB/$1.new"; mv "$DB/$1.new" "$DB/$1"
	echo "$g:x:$(awk -F: 'BEGIN { hi = 999 } $3 >= 1000 && $3 <= 60000 && $3 > hi { hi = $3 } END { print hi + 1 }' "$DB/group"):" >>"$DB/group"
}
agent agent
awk -F: -v g="$2" -v n="$3" '$1 == n { exit 9 } $3 == g { exit 4 }' "$DB/group" || exit
agent agent-during
echo "$3:x:$2:" >>"$DB/group"`,
		// useradd --uid UID --gid GID --create-home --shell SHELL NAME.
		// Before it, a user of the test's agent-user file may be made.
		"useradd": `echo "useradd $*" >>"$DB/calls"
[ ! -s "$DB/agent-user" ] || cat "$DB/agent-user" >>"$DB/passwd"
awk -F: -v u="$2" -v n="$8" '$1 == n { exit 9 } $3 == u { exit 4 }' "$DB/passwd" || exit
echo "$8:x:$2:$4::/home/$8:$7" >>"$DB/passwd"`,
		// find / ... -gid 1001 -print -quit: the test's own list of what
		// GID 1001 owns, or a failure.
		"find": `[ ! -e "$DB/fail-find" ] || exit "$(cat "$DB/fail-find")"
case " $* " in *" -gid 1001 "*) cat "$DB/owned" ;; *) exit 1 ;; esac`,
	}
	for _, tc := range []struct {
		name, group, passwd string
		// The test's own files: what GID 1001 owns, the groups the guest
		// agent makes as the script runs, a user it makes, failures.
		files map[string]string
		// wantGroup and wantPasswd are the files after a success; want,
		// what a failure says; changed, that a failure comes after a
		// change, so the files aren't checked.
		wantGroup, wantPasswd, want string
		changed                     bool
	}{
		{name: "no group holds GID 1001", group: debian, passwd: passwd,
			wantGroup: debian + runner, wantPasswd: passwd + user},
		{name: "the guest agent's docker holds GID 1001", group: debian + guest, passwd: passwd,
			wantGroup: debian + "google-sudoers:x:1000:\ndocker:x:997:\nlxd:x:1002:\n" + runner, wantPasswd: passwd + user},
		{name: "the guest agent takes GID 1001 again once it's freed", group: debian + "google-sudoers:x:1000:\ndocker:x:1001:\n", passwd: passwd,
			files:     map[string]string{"agent": "lxd\n"},
			wantGroup: debian + "google-sudoers:x:1000:\ndocker:x:997:\nlxd:x:996:\n" + runner, wantPasswd: passwd + user},
		{name: "the guest agent takes GID 1001 as groupadd makes runner", group: debian + "google-sudoers:x:1000:\ndocker:x:1001:\n", passwd: passwd,
			files:     map[string]string{"agent-during": "lxd\n"},
			wantGroup: debian + "google-sudoers:x:1000:\ndocker:x:997:\nlxd:x:996:\n" + runner, wantPasswd: passwd + user},
		{name: "the guest agent takes GID 1001 again each time", group: debian + "google-sudoers:x:1000:\ndocker:x:1001:\n", passwd: passwd,
			files: map[string]string{"agent": "g1\ng2\ng3\ng4\ng5\ng6\ng7\ng8\n"},
			want:  "GID 1001 was taken again each of 8 times it was freed", changed: true},
		{name: "a group with members holds GID 1001", group: debian + "google-sudoers:x:1000:\ndocker:x:1001:alice,bob\n", passwd: passwd,
			want: "GID 1001 is the group docker's, with members alice,bob"},
		{name: "a user's primary group holds GID 1001", group: debian + "google-sudoers:x:1000:\nalice:x:1001:\n", passwd: passwd + "alice:x:1000:1001::/home/alice:/bin/bash\n",
			want: "GID 1001 is the group alice's, the user alice's own group"},
		{name: "a group owning a file holds GID 1001", group: debian + guest, passwd: passwd, files: map[string]string{"owned": "/srv/data\n"},
			want: "GID 1001 is the group docker's, which owns /srv/data"},
		{name: "looking for its files fails", group: debian + guest, passwd: passwd, files: map[string]string{"fail-find": "1"},
			want: "exited 1: file=$(find / -xdev -ignore_readdir_race -gid 1001 -print -quit)"},
		{name: "no system GID is free", group: "root:x:0:\n" + full.String() + guest, passwd: passwd,
			want: "GID 1001 is the group docker's, and no system GID is free to move it to"},
		{name: "moving it fails", group: debian + guest, passwd: passwd, files: map[string]string{"fail-groupmod": "10"},
			want: `exited 10: groupmod --gid "$gid" "$name"`},
		{name: "a user holds UID 1001", group: debian + guest, passwd: passwd + "bob:x:1001:1003::/home/bob:/bin/bash\n",
			want: "UID 1001 is the user bob's"},
		{name: "looking for UID 1001 fails", group: debian + guest, passwd: passwd + "bob:x:1001:1003::/home/bob:/bin/bash\n",
			files: map[string]string{"fail-getent-passwd-1001": "1"},
			want:  "getent passwd 1001 exited 1"},
		{name: "looking up the group holding GID 1001 fails", group: debian + guest, passwd: passwd, files: map[string]string{"fail-getent-group-docker": "1"},
			want: "getent group docker exited 1"},
		{name: "a user takes UID 1001 as the runner's is made", group: debian + guest, passwd: passwd,
			files: map[string]string{"agent-user": "carol:x:1001:1003::/home/carol:/bin/bash\n"},
			want:  "useradd --uid 1001 runner exited 4: UID 1001 is the user carol's", changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db, bin := filepath.Join(dir, "db"), filepath.Join(dir, "bin")
			for _, d := range []string{db, bin} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string]string{"group": tc.group, "passwd": tc.passwd, "owned": ""}
			for name, data := range tc.files {
				files[name] = data
			}
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(db, name), []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for name, body := range stubs {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			console := filepath.Join(dir, "console")
			script := "set -Eeuo pipefail\n" + prelude
			for _, fn := range []string{"lookup", "free_gid_1001", "make_runner_user"} {
				script += scriptFunction(t, fn)
			}
			script = strings.NewReplacer(">/dev/console", ">>"+console, "/etc/passwd", filepath.Join(db, "passwd"), "/etc/group", filepath.Join(db, "group")).Replace(script)
			cmd := exec.Command(bash, "-c", script+"make_runner_user\ntrap - ERR\nresult \"built\"\n")
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + dir, "DB=" + db}
			out, err := cmd.CombinedOutput()
			got, _ := os.ReadFile(console)
			lines := strings.Split(strings.TrimSpace(string(got)), "\n")
			last := lines[len(lines)-1]
			group, _ := os.ReadFile(filepath.Join(db, "group"))
			pw, _ := os.ReadFile(filepath.Join(db, "passwd"))
			calls, _ := os.ReadFile(filepath.Join(db, "calls"))
			if tc.want != "" {
				if err == nil || !strings.HasPrefix(last, machine.Failed) || !strings.Contains(last, tc.want) {
					t.Fatalf("err = %v, console %q, output:\n%s\nwant it to end saying it failed, with %q", err, got, out, tc.want)
				}
				if !tc.changed && (string(group) != tc.group || string(pw) != tc.passwd || len(calls) != 0) {
					t.Errorf("a failure changed the accounts: ran %q, group:\n%s\npasswd:\n%s", calls, group, pw)
				}
				return
			}
			if err != nil || last != machine.ResultPrefix+"built" {
				t.Fatalf("err = %v, console %q, output:\n%s", err, got, out)
			}
			if string(group) != tc.wantGroup || string(pw) != tc.wantPasswd {
				t.Errorf("group:\n%s\npasswd:\n%s\nwant group:\n%s\npasswd:\n%s\noutput:\n%s", group, pw, tc.wantGroup, tc.wantPasswd, out)
			}
		})
	}
}
