#!/usr/bin/env bash
#
# run.sh reproduces the twenty-one experiments of the workflow-engine evaluation of
# INFRA-1546 against the real thing in every case.
#
#   bash docs/research/temporal-evaluation-2026-11/run.sh
#
# The directory is named temporal-evaluation because the issue started as a question
# about Temporal; the evaluation now measures five products:
#
#   1 to 6, 19 to 21  Temporal, against a dev server this script installs and runs in a container
#   7 to 9          DBOS Transact, a Go library on the pod's PostgreSQL
#   10 to 12        Restate, against a restate-server this script installs
#   13 to 15        go-workflows, a Go library on the pod's PostgreSQL
#   16 to 18        durabletask-go, a Go library on the pod's PostgreSQL
#
# It runs from the repository root, or from anywhere else: it finds its own directory
# and the repository root from there. It writes nothing into the working tree. The
# Temporal command, the restate-server, the servers' data, their logs, the built
# binaries and each experiment's scratch directory all go under $TMPDIR, so a
# checkout stays clean and `jj status` after a run shows nothing new.
#
# It stops at the first failure up to the point where the experiments start, naming
# what failed. From there it runs all twenty-one whatever each one does, so one failure
# does not hide the other twenty, and ends with the line
#
#   temporal-evaluation: all 21 experiments passed
#
# only when all twenty-one passed and every control failed. Otherwise it names each
# experiment that did not pass and each control that did not fail, and exits non-zero
# without that line.
#
# Each of the five kill experiments -- 3, 7, 10, 13 and 16 -- is followed by its
# control: the same experiment run again, with the same flags and -no-kill added, in a
# scratch directory of its own. The control must fail, because an experiment that
# passes with nothing killed measures nothing; a control that exits zero is a failure
# of this run, named apart from the experiment itself. A control is not an experiment
# and is not counted as one, so the count in that last line stays 21.

set -o errexit -o nounset -o pipefail

# The pinned Temporal command. Every version here is an exact literal, never a moving
# tag: a run months from now must install the same command this evaluation measured.
# The archive, its size and its checksum were read on 2026-11-07.
readonly cli_version='1.10.0'
readonly cli_url='https://temporal.download/cli/archive/v1.10.0?platform=linux&arch=amd64'
readonly cli_sha256='521f461f3d108902763bb71e85cd0fd382dc9357b59604ae03da6ae734d16890'
readonly cli_version_line='temporal version 1.10.0 (Server 1.32.2, UI 2.56.1)'

# The pinned restate-server, likewise an exact literal. Its archive is .tar.xz and
# this pod has no xz binary, so it is unpacked with Python's lzma and tarfile.
readonly restate_version='1.8.1'
readonly restate_url='https://github.com/restatedev/restate/releases/download/v1.8.1/restate-server-x86_64-unknown-linux-musl.tar.xz'
readonly restate_sha256='56f12ccc54ee227657d4d8b070622849977444425184ee242a33e3ccc28bf745'
readonly restate_bytes='50130840'
readonly restate_member='restate-server-x86_64-unknown-linux-musl/restate-server'
readonly restate_version_line='restate-server 1.8.1'

# The Go module proxy the prototype's dependencies come from.
readonly go_proxy_url='https://proxy.golang.org/'

# The nine Temporal namespaces, one per Temporal experiment. The server's Action
# counter is a server-lifetime counter carrying a namespace label, so experiment 6 can
# only count its own Actions if nothing else ran in its namespace.
readonly namespaces=(e1 e2 e3 e4 e5 e6 e7 e8 e9)

# How long to wait for the server to answer, and how long one experiment may take. The
# slowest experiment on 2026-11-07, experiment 20 aside, was the eleventh, at about 50
# seconds.
readonly server_ready_timeout=120
readonly experiment_timeout=300

# Experiment 20 holds one handed-out activity open for five minutes of real time with
# no worker alive, which is the whole measurement and cannot be shortened. It gets a
# bound of its own rather than lifting every other experiment's, which would let a
# hung experiment sit for a quarter of an hour: fifteen minutes leaves the hold, the
# handout around it and the completion after it room to finish.
readonly experiment_20_timeout=900

die() {
	printf 'temporal-evaluation: %s\n' "$*" >&2
	exit 1
}

say() {
	printf 'temporal-evaluation: %s\n' "$*"
}

# --- where everything lives ---------------------------------------------------------

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
readonly script_dir
readonly prototype_dir="$script_dir/prototype"
repo_root=$(cd -- "$script_dir/../../.." && pwd -P)
readonly repo_root

# The file that names the Legion repository's root, the same marker the prototype's
# harness uses. Experiments 1, 5 and 6 read the repository's own code through it.
readonly repo_marker='packages/daemon/internal/workflow/table.go'
[[ -f "$repo_root/$repo_marker" ]] ||
	die "$repo_root holds no $repo_marker, so it is not the Legion repository's root; run this script from its place in the repository"
[[ -d "$prototype_dir" ]] || die "no prototype directory at $prototype_dir"

# --- 1. the temporary directory -----------------------------------------------------

# The container's disk is small and shared with every other agent in the pod. A run
# writes about 300 MB: the Temporal archive, the extracted command, the restate-server
# archive and binary, the servers' data and the built binaries. All of it belongs on
# the issue's own volume.
[[ -n "${TMPDIR:-}" ]] ||
	die "TMPDIR is unset. This run writes about 300 MB, which belongs on the issue's own volume, not on the container disk every agent in the pod shares. Set TMPDIR to a writable directory on that volume and run again."

tmp_root=${TMPDIR%/}
[[ "$tmp_root" != /tmp && "$tmp_root" != /tmp/* ]] ||
	die "TMPDIR is $TMPDIR, which is under /tmp. This run writes about 300 MB, which belongs on the issue's own volume, not on the container disk every agent in the pod shares. Set TMPDIR to a writable directory on that volume and run again."
[[ -d "$tmp_root" ]] || die "TMPDIR is $TMPDIR, which is not a directory"

# A symbolic link to /tmp is still /tmp.
tmp_real=$(cd -- "$tmp_root" && pwd -P) ||
	die "TMPDIR is $TMPDIR, which cannot be entered"
[[ "$tmp_real" != /tmp && "$tmp_real" != /tmp/* ]] ||
	die "TMPDIR is $TMPDIR, which resolves to $tmp_real, under /tmp. This run writes about 300 MB, which belongs on the issue's own volume, not on the container disk every agent in the pod shares. Set TMPDIR to a writable directory on that volume and run again."
[[ -w "$tmp_real" ]] || die "TMPDIR is $TMPDIR, which is not writable"

readonly tmp_real
readonly base_dir="$tmp_real/temporal-evaluation-2026-11"
# The installed commands are kept between runs; everything a run produces is not.
readonly cli_dir="$base_dir/temporal-cli-$cli_version"
readonly temporal_bin="$cli_dir/temporal"
readonly restate_dir="$base_dir/restate-server-$restate_version"
readonly restate_bin="$restate_dir/restate-server"
readonly run_dir="$base_dir/run"
readonly bin_dir="$run_dir/bin"
readonly server_dir="$run_dir/server"
readonly server_log="$server_dir/server.log"

rm -rf -- "$run_dir"
mkdir -p -- "$cli_dir" "$restate_dir" "$bin_dir" "$server_dir"
say "temporary directory: $base_dir"
say "repository root: $repo_root"

# --- 2. the PostgreSQL nine of the twenty-one need -----------------------------------

# Experiments 7 to 9, 13 to 15 and 16 to 18 measure libraries whose whole state lives
# in the application's own PostgreSQL. Without a database there is nothing to measure,
# and a run that skipped them would not be this evaluation.
[[ -n "${LEGION_TEST_PG_DSN:-}" ]] ||
	die "LEGION_TEST_PG_DSN is empty. Nine of the twenty-one experiments -- 7 to 9 for DBOS, 13 to 15 for go-workflows and 16 to 18 for durabletask-go -- measure libraries that keep their workflow state in PostgreSQL, so this run cannot measure them without one. Set LEGION_TEST_PG_DSN to a PostgreSQL URL this process may create a database in and run again."
readonly postgres_dsn="$LEGION_TEST_PG_DSN"
say 'LEGION_TEST_PG_DSN is set'

# --- the trap -----------------------------------------------------------------------

# The cleanup signals no process by its name or its command line, only the ones this
# script started and recorded: the experiment running now, by the pid and the start time
# (/proc/<pid>/stat field 22) it was started with, and the dev server's container, by the
# id its own `docker create` printed. An experiment stops what it started itself on
# SIGTERM (its deferred stops, which hold those processes' own pids), so SIGTERM comes
# first.
container_id=''
experiment_pid=''
experiment_started=''

# start_time PID: the start time of PID, field 22 of /proc/PID/stat, or nothing when PID
# is gone.
start_time() {
	local stat fields
	stat=$(cat "/proc/$1/stat" 2>/dev/null) || return 0
	stat=${stat##*) }
	read -r -a fields <<<"$stat"
	printf '%s\n' "${fields[19]}"
}

# bounded SECONDS COMMAND...: COMMAND under `timeout SECONDS`, in the background so a
# signal to this script interrupts the wait, its pid and start time recorded for the
# cleanup; returns COMMAND's status (124 when the bound ran out). timeout leads a process
# group of its own, which holds COMMAND.
bounded() {
	local status=0
	timeout "$@" &
	experiment_pid=$!
	experiment_started=$(start_time "$experiment_pid")
	wait "$experiment_pid" || status=$?
	experiment_pid='' experiment_started=''
	return "$status"
}

# experiment_running: the recorded experiment still runs, as the process it was started
# as: its pid is live with the start time recorded for it, so a pid the kernel has reused
# is never signalled.
experiment_running() {
	[[ -n "$experiment_started" && "$(start_time "$experiment_pid")" == "$experiment_started" ]]
}

stop_experiment() {
	experiment_running || return 0
	local waited=0
	kill -TERM -- "-$experiment_pid" 2>/dev/null || true
	while experiment_running && ((waited < 100)); do
		sleep 0.2
		waited=$((waited + 1))
	done
	if experiment_running; then
		kill -KILL -- "-$experiment_pid" 2>/dev/null || true
	fi
	wait "$experiment_pid" 2>/dev/null || true
	experiment_pid='' experiment_started=''
}

stop_dev_server() {
	[[ -n "$container_id" ]] || return 0
	docker logs "$container_id" >"$server_log" 2>&1 || true
	# Give it a moment to close its database, then insist.
	docker stop --time 10 "$container_id" >/dev/null 2>&1 || true
	docker rm --force --volumes "$container_id" >/dev/null 2>&1 || true
	container_id=''
}

cleanup() {
	local status=$?
	trap - EXIT INT TERM
	stop_experiment
	stop_dev_server
	exit "$status"
}
trap cleanup EXIT
trap 'die "interrupted"' INT TERM

# --- 3. the three network prerequisites ----------------------------------------------

# Answers means: the host completed an HTTP response. The archive URL refuses HEAD with
# 405, which is an answer; a 5xx is the host failing, and this run would fail later and
# less clearly.
host_answers() {
	local url=$1 code
	code=$(curl --silent --show-error --location --head --max-time 30 \
		--output /dev/null --write-out '%{http_code}' -- "$url" 2>/dev/null) || return 1
	[[ "$code" =~ ^[1-4][0-9][0-9]$ ]]
}

host_answers "$cli_url" ||
	die "the Temporal download host does not answer: $cli_url. This run cannot install the pinned Temporal command without it."
say 'the Temporal download host answers'
host_answers "$restate_url" ||
	die "the Restate download host does not answer: $restate_url. This run cannot install the pinned restate-server without it."
say 'the Restate download host answers'
host_answers "$go_proxy_url" ||
	die "the Go module proxy does not answer: $go_proxy_url. This run cannot build the prototype without it."
say 'the Go module proxy answers'

# --- 4. the pinned Temporal command --------------------------------------------------

install_cli() {
	local work="$cli_dir.download"
	rm -rf -- "$work"
	mkdir -p -- "$work"

	say "downloading the Temporal command $cli_version"
	curl --silent --show-error --fail --location --max-time 300 \
		--output "$work/temporal.tar.gz" -- "$cli_url" ||
		die "downloading the Temporal command from $cli_url failed"

	local got
	got=$(sha256sum -- "$work/temporal.tar.gz" | cut -d' ' -f1)
	[[ "$got" == "$cli_sha256" ]] ||
		die "the Temporal archive from $cli_url has sha256 $got, not the pinned $cli_sha256; refusing to run it"

	tar -xzf "$work/temporal.tar.gz" -C "$work" temporal ||
		die "the Temporal archive holds no temporal binary"
	chmod +x "$work/temporal"
	mv -f -- "$work/temporal" "$temporal_bin"
	rm -rf -- "$work"
}

[[ -x "$temporal_bin" ]] || install_cli

got_version=$("$temporal_bin" --version 2>&1 | head -n 1) ||
	die "$temporal_bin does not run"
[[ "$got_version" == "$cli_version_line" ]] ||
	die "$temporal_bin prints '$got_version', not the pinned '$cli_version_line'; refusing to measure against another version"
say "temporal command: $got_version"

# --- 5. the pinned restate-server ----------------------------------------------------

install_restate() {
	local work="$restate_dir.download"
	rm -rf -- "$work"
	mkdir -p -- "$work"

	say "downloading restate-server $restate_version"
	curl --silent --show-error --fail --location --max-time 600 \
		--output "$work/restate.tar.xz" -- "$restate_url" ||
		die "downloading restate-server from $restate_url failed"

	local got bytes
	bytes=$(stat -c %s -- "$work/restate.tar.xz")
	[[ "$bytes" == "$restate_bytes" ]] ||
		die "the Restate archive from $restate_url is $bytes bytes, not the pinned $restate_bytes; refusing to run it"
	got=$(sha256sum -- "$work/restate.tar.xz" | cut -d' ' -f1)
	[[ "$got" == "$restate_sha256" ]] ||
		die "the Restate archive from $restate_url has sha256 $got, not the pinned $restate_sha256; refusing to run it"

	# This pod has no xz binary, so the archive is unpacked by Python's lzma and
	# tarfile. filter='data' refuses a member that would write outside the directory.
	python3 -c 'import lzma, sys, tarfile
with lzma.open(sys.argv[1]) as raw, tarfile.open(fileobj=raw) as archive:
    archive.extractall(sys.argv[2], filter="data")' "$work/restate.tar.xz" "$work" ||
		die "unpacking the Restate archive with Python failed"

	[[ -f "$work/$restate_member" ]] ||
		die "the Restate archive holds no $restate_member"
	chmod +x "$work/$restate_member"
	mv -f -- "$work/$restate_member" "$restate_bin"
	rm -rf -- "$work"
}

[[ -x "$restate_bin" ]] || install_restate

got_restate=$("$restate_bin" --version 2>&1 | head -n 1) ||
	die "$restate_bin does not run"
[[ "$got_restate" == "$restate_version_line" ]] ||
	die "$restate_bin prints '$got_restate', not the pinned '$restate_version_line'; refusing to measure against another version"
say "restate-server: $got_restate"

# --- 6. gofmt -----------------------------------------------------------------------

# The root go.work binds the daemon and the Envoy modules only, and this issue forbids
# changing it, so every Go command here runs with the workspace off.
export GOWORK=off

gofmt_bin="$(go env GOROOT)/bin/gofmt"
[[ -x "$gofmt_bin" ]] || die "no gofmt at $gofmt_bin"
unformatted=$("$gofmt_bin" -l "$prototype_dir")
if [[ -n "$unformatted" ]]; then
	printf '%s\n' "$unformatted"
	die 'gofmt -l printed the files above; run gofmt -w over them'
fi
say 'gofmt -l: clean'

# --- 7. go vet ----------------------------------------------------------------------

(cd -- "$prototype_dir" && go vet ./...) || die 'go vet failed in the prototype'
say 'go vet: clean'

# --- 8. the six binaries --------------------------------------------------------------

for command in experiments sessionworker dbosworker restateservice gwworker dtworker; do
	(cd -- "$prototype_dir" && go build -o "$bin_dir/$command" "./cmd/$command") ||
		die "building cmd/$command failed"
done
say "built experiments, sessionworker, dbosworker, restateservice, gwworker and dtworker in $bin_dir"

# --- 9. the dev server ----------------------------------------------------------------

# The dev server runs in a container of its own: the pinned Temporal command above, a
# static binary mounted read-only, on the pinned base image below. Inside the container
# it listens on fixed ports, in a network namespace that nothing else uses. Docker
# publishes each of the two on 127.0.0.1 at a host port that Docker chooses and binds
# itself (`-p 127.0.0.1::<port>`), and the script reads each host port back with
# `docker port`, so this script chooses no port: the dev server takes no listening socket
# from its caller, and it refuses port 0 ("missing frontend port"). The base image is the
# Debian that the repository's CI images pin (.github/ci-images.yaml, debian-trixie-slim),
# through Google's mirror, as the devbox proofs that no CI job runs take theirs (AGENTS.md).
readonly server_image='mirror.gcr.io/library/debian@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4'
readonly grpc_container_port=7233 metrics_container_port=9090
docker version >/dev/null 2>&1 ||
	die 'docker does not answer: the dev server runs in a container, whose ports Docker publishes at host ports it chooses itself'

namespace_flags=()
for ns in "${namespaces[@]}"; do
	namespace_flags+=(--namespace "$ns")
done

# host_port CONTAINER_PORT: the host port Docker published the container's port at.
host_port() {
	docker port "$container_id" "$1/tcp" 2>/dev/null | awk -F: 'NR == 1 { print $NF }'
}

say "starting the dev server in a container of $server_image"
# `docker create` prints the container's id before anything binds a port, so a start that
# fails still leaves the id recorded, and the cleanup removes that container by it. The
# container runs as this script's own user, so the dev server's database stays this
# user's to remove; that uid has no passwd entry in the image, so USER names it for the
# server's own lookup of its user (a static binary has no cgo to read one otherwise).
container_id=$(docker create --label created-by=temporal-evaluation-run.sh \
	--user "$(id -u):$(id -g)" --env HOME=/tmp --env USER=temporal-evaluation --workdir /data \
	--publish "127.0.0.1::$grpc_container_port" --publish "127.0.0.1::$metrics_container_port" \
	--volume "$cli_dir:/opt/temporal:ro" --volume "$server_dir:/data" \
	"$server_image" /opt/temporal/temporal server start-dev \
	--headless \
	--ip 0.0.0.0 \
	--port "$grpc_container_port" \
	--metrics-port "$metrics_container_port" \
	--db-filename /data/dev.db \
	"${namespace_flags[@]}" \
	--log-level warn) || {
	container_id=''
	die "docker could not create the dev server's container from $server_image"
}
[[ "$container_id" =~ ^[0-9a-f]{64}$ ]] || {
	container_id=''
	die "docker create printed no container id"
}
docker start "$container_id" >/dev/null ||
	die "docker could not start the dev server's container ${container_id:0:12}"
grpc_port=$(host_port "$grpc_container_port")
metrics_port=$(host_port "$metrics_container_port")
[[ "$grpc_port" =~ ^[1-9][0-9]*$ && "$metrics_port" =~ ^[1-9][0-9]*$ ]] ||
	die "docker port names no host port for the dev server's ports $grpc_container_port and $metrics_container_port"
readonly grpc_port metrics_port
readonly server_address="127.0.0.1:$grpc_port"
readonly metrics_url="http://127.0.0.1:$metrics_port/metrics"
say "the dev server's container ${container_id:0:12} publishes gRPC at $server_address, metrics at $metrics_url"

# Ready means: the last namespace exists and the metrics endpoint answers. Both are
# what an experiment needs, and the server reports both only once it is serving.
ready=''
for _ in $(seq 1 "$server_ready_timeout"); do
	if [[ "$(docker inspect --format '{{.State.Running}}' "$container_id" 2>/dev/null)" != true ]]; then
		docker logs "$container_id" >"$server_log" 2>&1 || true
		cat -- "$server_log" >&2 || true
		die "the dev server stopped while starting; its log is above and in $server_log"
	fi
	if "$temporal_bin" operator namespace describe \
		--address "$server_address" --namespace "${namespaces[-1]}" >/dev/null 2>&1 &&
		curl --silent --fail --max-time 5 --output /dev/null -- "$metrics_url"; then
		ready=yes
		break
	fi
	sleep 1
done
[[ -n "$ready" ]] || {
	docker logs "$container_id" >"$server_log" 2>&1 || true
	cat -- "$server_log" >&2 || true
	die "the dev server did not answer on $server_address within ${server_ready_timeout}s; its log is above and in $server_log"
}
say "the dev server answers on $server_address"

# --- 10. the twenty-one experiments ----------------------------------------------------

# The flags one Restate run needs. Each run of a Restate experiment -- and each control
# of one -- starts a restate-server of its own, which serves its admin API, its ingress
# and its node's own channel on unix sockets in the experiment's directory and binds no
# TCP port, and holds the loopback port of each service endpoint it starts from the
# kernel's choice to the end of the run (prototype/internal/restatex), so this script
# chooses no port for them.
readonly restate_flags=(-restate-server "$restate_bin" -restate-service "$bin_dir/restateservice")

# Every experiment is given a scratch directory of its own and the flags its own
# product needs. Experiments 19, 20 and 21 run straight after experiment 6 because they
# need the dev server those six share, which is stopped as soon as 21 is done.
# Experiment 21 needs no bound of its own: its two handouts carry a start-to-close
# timeout of four hours, which nothing in it waits for, and every wait it takes is
# bounded in seconds, so the bound every other experiment shares covers it. The
# Restate experiments start and stop their own restate-server, one per experiment, on
# unix sockets of its own.
#
# Each kill experiment that passed is followed by its control: the same experiment,
# the same flags, -no-kill added, a scratch directory of its own so it neither reads
# nor overwrites the experiment's files, and the experiment's own timeout. A control
# that fails proves the experiment measures its kill; one that passes proves it
# measures nothing, and is counted here apart from the experiments.
failed=()
controls_that_passed=()
for n in 1 2 3 4 5 6 19 20 21 7 8 9 10 11 12 13 14 15 16 17 18; do
	work_dir="$run_dir/work/e$n"
	mkdir -p -- "$work_dir"

	# The flags this experiment's product needs, which its control needs too.
	flags=()
	label=''
	# How long this experiment may take, and its control with it. Only experiment 20
	# needs more than the bound every other experiment shares.
	bound=$experiment_timeout
	case "$n" in
	[1-6])
		namespace=${namespaces[$((n - 1))]}
		label=" (namespace $namespace)"
		flags+=(-address "$server_address" -namespace "$namespace" -repo-root "$repo_root")
		case "$n" in
		3) flags+=(-session-worker "$bin_dir/sessionworker") ;;
		6) flags+=(-metrics-url "$metrics_url") ;;
		esac
		;;
	19)
		namespace=${namespaces[6]}
		label=" (namespace $namespace)"
		flags+=(-address "$server_address" -namespace "$namespace")
		;;
	20)
		namespace=${namespaces[7]}
		label=" (namespace $namespace)"
		flags+=(-address "$server_address" -namespace "$namespace")
		bound=$experiment_20_timeout
		;;
	21)
		namespace=${namespaces[8]}
		label=" (namespace $namespace)"
		flags+=(-address "$server_address" -namespace "$namespace")
		;;
	7 | 8 | 9)
		flags+=(-postgres-dsn "$postgres_dsn" -dbos-worker "$bin_dir/dbosworker")
		;;
	10 | 11 | 12)
		flags+=("${restate_flags[@]}")
		;;
	13 | 14 | 15)
		flags+=(-postgres-dsn "$postgres_dsn" -gw-worker "$bin_dir/gwworker")
		;;
	16 | 17 | 18)
		flags+=(-postgres-dsn "$postgres_dsn" -dt-worker "$bin_dir/dtworker")
		;;
	esac

	printf '\n=== experiment %d%s ===\n' "$n" "$label"
	status=0
	bounded "$bound" "$bin_dir/experiments" \
		-experiment "$n" -work-dir "$work_dir" "${flags[@]}" || status=$?
	if ((status != 0)); then
		failed+=("$n")
		printf 'experiment %d: exit status %d\n' "$n" "$status"
	fi

	# The control of a kill experiment, run while everything that experiment needed
	# is still up: the dev server for experiment 3, the database for the rest.
	case "$n" in
	3 | 7 | 10 | 13 | 16)
		if ((status != 0)); then
			printf '\n=== experiment %d control (no kill) ===\n' "$n"
			printf 'experiment %d control: not run, because experiment %d did not pass, and a control of an experiment that failed proves nothing\n' "$n" "$n"
		else
			control_dir="$run_dir/work/e$n-control"
			mkdir -p -- "$control_dir"
			printf '\n=== experiment %d control (no kill)%s ===\n' "$n" "$label"
			control_status=0
			bounded "$bound" "$bin_dir/experiments" \
				-experiment "$n" -work-dir "$control_dir" "${flags[@]}" -no-kill ||
				control_status=$?
			if ((control_status == 0)); then
				controls_that_passed+=("$n")
				printf "experiment %d control: exit status 0 -- the run that killed nothing passed, so experiment %d measures nothing\n" "$n" "$n"
			else
				printf 'experiment %d control: exit status %d -- the run that killed nothing failed, as a control must\n' "$n" "$control_status"
			fi
		fi
		;;
	esac

	# The Temporal dev server is only for experiments 1 to 6, 19, 20 and 21, which
	# run one after another; nothing after them needs a server this script runs.
	if ((n == 21)); then
		stop_dev_server
		say 'the dev server is stopped: experiments 1 to 6 and 19 to 21 are done, and 7 to 18 measure libraries and a server of their own'
	fi
done

# --- 11. the verdict ------------------------------------------------------------------

printf '\n'
if ((${#failed[@]} > 0 || ${#controls_that_passed[@]} > 0)); then
	for n in "${failed[@]}"; do
		printf 'temporal-evaluation: experiment %d did not pass\n' "$n" >&2
	done
	for n in "${controls_that_passed[@]}"; do
		printf "temporal-evaluation: experiment %d's control did not fail, so experiment %d measures nothing\n" "$n" "$n" >&2
	done
	((${#failed[@]} == 0)) ||
		printf 'temporal-evaluation: %d of 21 experiments did not pass\n' "${#failed[@]}" >&2
	((${#controls_that_passed[@]} == 0)) ||
		printf "temporal-evaluation: %d of the 5 kill experiments' controls did not fail\n" "${#controls_that_passed[@]}" >&2
	exit 1
fi
printf 'temporal-evaluation: all 21 experiments passed\n'
