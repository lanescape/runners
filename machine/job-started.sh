#!/bin/bash
# The job-started hook (specs/0002-machines.md, The job-started check). The
# runner runs it in the job's environment, before the job's first step, so it
# decides nothing: it asks the service whether the job on this machine's
# runner may run here, signed with the machine's identity token, and if the
# service allows it, mounts the work disk the service attached at /work. If
# the service refuses, or gives no answer it can use, it never returns: the
# service stops the machine, and the job's steps never run.
#
# Its settings come from the base image; the only argument, which the runner
# never passes, is another settings file, for tests.
set -u

settings=${1:-/etc/lanescape-runners/job-started.conf}
metadata=http://metadata.google.internal
device=/dev/disk/by-id/google-work
mountpoint=/work
if [ -r "$settings" ]; then
	while IFS='=' read -r key value; do
		case $key in
		metadata) metadata=$value ;;
		device) device=$value ;;
		mountpoint) mountpoint=$value ;;
		esac
	done <"$settings"
fi

say() { echo "lanescape-runners: $*" >&2; }

# hold never returns: the machine is stopping.
hold() {
	say "$*"
	while :; do sleep 3600; done
}

fail() {
	say "$*"
	exit 1
}

# get reads the metadata server. Nothing in the job's environment may point
# curl elsewhere: no configuration files, no proxies.
get() {
	curl -q --noproxy '*' -fsS --max-time 10 -H 'Metadata-Flavor: Google' "$@"
}

service=
until service=$(get "$metadata/computeMetadata/v1/instance/attributes/lsr-service") && [ -n "$service" ]; do
	sleep 2
done

# The run is only where the service looks first; it finds the job from
# GitHub's records.
run_id=${GITHUB_RUN_ID:-0}
case $run_id in '' | *[!0-9]*) run_id=0 ;; esac

say "asking the service whether this job may run here"
while :; do
	if ! token=$(get -G --data-urlencode "audience=$service" --data-urlencode format=full \
		"$metadata/computeMetadata/v1/instance/service-accounts/default/identity"); then
		sleep 2
		continue
	fi
	# The token goes in a header read from standard input, never in an
	# argument another process could see.
	code=$(printf 'header = "Authorization: Bearer %s"\n' "$token" |
		curl -q --noproxy '*' -sS --config - -o /dev/null -w '%{http_code}' --max-time 120 \
			-X POST -H 'Content-Type: application/json' --data "{\"run_id\": $run_id}" \
			"$service/v1/jobs/started") || code=000
	case $code in
	200) break ;;
	401 | 403 | 404) hold "the service refused this job on this machine, which is stopping" ;;
	*) sleep 5 ;;
	esac
done

i=0
until [ -e "$device" ]; do
	i=$((i + 1))
	[ "$i" -le 120 ] || fail "the work disk never appeared at $device"
	sleep 1
done
# A new disk has no filesystem yet; one made from a saved disk has.
if ! sudo -n blkid -p "$device" >/dev/null 2>&1; then
	sudo -n mkfs.ext4 -q -m 0 -L work "$device" || fail "formatting the work disk"
fi
sudo -n mkdir -p "$mountpoint" || fail "making $mountpoint"
sudo -n mount -o discard,noatime "$device" "$mountpoint" || fail "mounting the work disk"
sudo -n chown "$(id -u):$(id -g)" "$mountpoint" || fail "giving $mountpoint to the runner's user"
say "the work disk is mounted at $mountpoint"
