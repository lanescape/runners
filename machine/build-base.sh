#!/bin/bash
# The base image's builder (specs/0003-disks.md, Making a base image). The
# service makes a builder machine from Debian 13's image, with no account,
# and gives it this script as its startup script, which Debian's guest
# environment runs as root once the machine has booted. It installs what
# every machine of the service needs, from Debian's packages and from the
# runner's and Go's own releases, each checked against its SHA-256, builds
# the service's programs from their sources with that Go, checks what it
# made, and writes its result to the serial console as one line, which the
# service reads. Then it waits: the service stops the machine, and makes the
# base image from its boot disk.
#
# Everything it installs comes from the machine's metadata, which only the
# service sets: the architecture and key, the runner's and Go's versions and
# checksums, and lsr-files, a gzipped tar of the service's programs'
# sources and the files the image installs as they are, with where each
# goes: programs and installs. Nothing of any
# consumer's ever reaches it.
# errtrace (-E): a failure inside a function, a command substitution or a
# subshell reports itself too.
set -Eeuo pipefail
umask 022
export DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8 LC_ALL=C.UTF-8

result() {
	# The one line the service looks for. It goes to the console itself,
	# so that nothing in between can split or drop it.
	printf '\nlanescape-runners base image: %s\n' "$1" >/dev/console
}

fail() {
	trap - ERR
	result "failed: $1"
	exit 1
}

trap 'fail "line $LINENO exited $?: $BASH_COMMAND"' ERR

say() { printf 'lanescape-runners base image: %s\n' "$*"; }

# A builder's disk is fresh: a second run, such as after a restart, would
# start from what the first left, so it fails instead.
if [ -e /opt/lanescape-runners ] || [ -e /opt/actions-runner ] || getent passwd runner >/dev/null; then
	fail "the builder's disk isn't fresh from Debian's image"
fi

# Debian's own updates while a machine lives would hold the package
# manager's lock while a job's first step installs what it needs; they go
# first, so they don't hold it here either.
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer
systemctl stop apt-daily.service apt-daily-upgrade.service
systemctl mask unattended-upgrades.service

# Debian's packages, with Debian's updates as of now.
say "installing Debian's packages"
apt_get() { apt-get -q -o DPkg::Lock::Timeout=600 "$@"; }
apt_get update
apt_get -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold upgrade
apt_get -y --no-install-recommends install ca-certificates curl e2fsprogs git jq pigz python3 sudo zstd

work=$(mktemp -d /var/tmp/lanescape-runners-build.XXXXXX)

md() {
	curl -fsS --retry 5 --retry-connrefused -H 'Metadata-Flavor: Google' \
		"http://metadata.google.internal/computeMetadata/v1/instance/attributes/$1"
}

arch=$(md lsr-arch)
key=$(md lsr-key)
runner_version=$(md lsr-runner-version)
runner_sha256=$(md lsr-runner-sha256)
go_version=$(md lsr-go-version)
go_sha256=$(md lsr-go-sha256)
md lsr-files | base64 -d >"$work/files.tar.gz"

[[ $arch =~ ^(amd64|arm64)$ ]] || fail "lsr-arch isn't amd64 or arm64"
[[ $key =~ ^[0-9a-f]{20}$ ]] || fail "lsr-key isn't a base image's key"
[[ $runner_version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "lsr-runner-version isn't a release version"
[[ $go_version =~ ^1\.[0-9]+\.[0-9]+$ ]] || fail "lsr-go-version isn't a Go release"
[[ $runner_sha256 =~ ^[0-9a-f]{64}$ && $go_sha256 =~ ^[0-9a-f]{64}$ ]] || fail "a checksum isn't a SHA-256"
[ "$(dpkg --print-architecture)" = "$arch" ] || fail "this machine is $(dpkg --print-architecture), not $arch"
say "building $arch-$key"

# Go, from Go's own release, at go.mod's version.
say "installing Go $go_version"
curl -fsSL --retry 5 -o "$work/go.tar.gz" "https://go.dev/dl/go${go_version}.linux-${arch}.tar.gz"
echo "$go_sha256  $work/go.tar.gz" | sha256sum -c --quiet -
tar -xzf "$work/go.tar.gz" -C /usr/local
ln -s /usr/local/go/bin/go /usr/local/bin/go
ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# The runner's user, as on GitHub's runners: UID and GID 1001, with
# passwordless sudo.
say "making the runner's user"
groupadd --gid 1001 runner
useradd --uid 1001 --gid 1001 --create-home --shell /bin/bash runner
printf 'runner ALL=(ALL) NOPASSWD:ALL\n' >"$work/sudoers"
visudo -cqf "$work/sudoers"
install -o root -g root -m 0440 "$work/sudoers" /etc/sudoers.d/runner

# GitHub's runner, in a directory of its own, with the packages its own
# script installs.
say "installing the runner $runner_version"
case $arch in
amd64) runner_arch=x64 ;;
arm64) runner_arch=arm64 ;;
esac
curl -fsSL --retry 5 -o "$work/runner.tar.gz" \
	"https://github.com/actions/runner/releases/download/v${runner_version}/actions-runner-linux-${runner_arch}-${runner_version}.tar.gz"
echo "$runner_sha256  $work/runner.tar.gz" | sha256sum -c --quiet -
install -d -o runner -g runner -m 0755 /opt/actions-runner
tar -xzf "$work/runner.tar.gz" -C /opt/actions-runner --no-same-owner
chown -R runner:runner /opt/actions-runner
/opt/actions-runner/bin/installdependencies.sh

# The service's own programs, built from their sources, statically, and the
# files installed with them.
say "installing the service's programs"
mkdir "$work/files"
tar -xzf "$work/files.tar.gz" -C "$work/files" --no-same-owner
install -d -o root -g root -m 0755 /opt/lanescape-runners
while read -r source path; do
	[[ $source =~ ^[a-z]+$ && $path =~ ^/opt/lanescape-runners/[a-z-]+$ ]] || fail "lsr-files names a program $source at $path"
	(
		cd "$work/files/src/$source"
		printf 'module %s\n\ngo %s\n' "$source" "${go_version%.*}" >go.mod
		CGO_ENABLED=0 GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=mod GOCACHE="$work/gocache" GOPATH="$work/gopath" \
			/usr/local/go/bin/go build -trimpath -o "$path" .
	)
	chown root:root "$path"
	chmod 0755 "$path"
done <"$work/files/programs"
while read -r file path mode; do
	[[ $file =~ ^[a-z0-9.-]+$ && $path =~ ^/[a-z0-9./-]+$ && $mode =~ ^[0-7]{3,4}$ ]] || fail "lsr-files installs $file at $path"
	install -D -o root -g root -m "$mode" "$work/files/files/$file" "$path"
done <"$work/files/installs"
systemctl enable lanescape-runners-supervisor.service

# The work disk's mount point: empty, and owned by root, so the runner's
# user can't write there until the disk is mounted.
install -d -o root -g root -m 0755 /work

# Check what was made before saying it was built.
say "checking the image"
for tool in git curl jq python3 zstd pigz mkfs.ext4 go gofmt sudo; do
	command -v "$tool" >/dev/null || fail "$tool isn't on PATH"
done
[ "$(go env GOVERSION)" = "go$go_version" ] || fail "go is $(go env GOVERSION), not go$go_version"
while read -r source path; do
	[ -x "$path" ] || fail "$path wasn't installed"
	if ldd "$path" >/dev/null 2>&1; then
		fail "$path isn't statically linked"
	fi
done <"$work/files/programs"
[ "$(id -u runner):$(id -g runner)" = "1001:1001" ] || fail "the runner's user isn't UID and GID 1001"
runuser -u runner -- sudo -n true || fail "the runner's user has no passwordless sudo"
[ "$(stat -c '%U:%G %a' /work)" = "root:root 755" ] || fail "/work isn't owned by root, mode 755"
[ -z "$(ls -A /work)" ] || fail "/work isn't empty"
if command -v docker >/dev/null || command -v dockerd >/dev/null; then
	fail "the image has Docker"
fi
runuser -u runner -- unshare --user --map-root-user true || fail "the runner's user can't make an unprivileged user namespace"
systemctl is-enabled lanescape-runners-supervisor.service >/dev/null || fail "the supervisor isn't enabled"

# What's of this builder alone goes, so each machine is its own.
say "clearing the builder's own state"
apt-get clean
rm -rf "$work" /root/.cache /root/.config/go /root/go
rm -f /var/lib/systemd/timers/stamp-apt-daily.timer /var/lib/systemd/timers/stamp-apt-daily-upgrade.timer
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id
journalctl --rotate >/dev/null 2>&1 || true
journalctl --vacuum-time=1s >/dev/null 2>&1 || true
sync

trap - ERR
result "built $arch-$key"
