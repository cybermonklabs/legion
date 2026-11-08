#!/usr/bin/env bash
#
# run.sh reproduces the eighteen experiments of the workflow-engine evaluation of
# INFRA-1546 against the real thing in every case.
#
#   bash docs/research/temporal-evaluation-2026-11/run.sh
#
# The directory is named temporal-evaluation because the issue started as a question
# about Temporal; the evaluation now measures five products:
#
#   1 to 6    Temporal, against a dev server this script installs and runs
#   7 to 9    DBOS Transact, a Go library on the pod's PostgreSQL
#   10 to 12  Restate, against a restate-server this script installs
#   13 to 15  go-workflows, a Go library on the pod's PostgreSQL
#   16 to 18  durabletask-go, a Go library on the pod's PostgreSQL
#
# It runs from the repository root, or from anywhere else: it finds its own directory
# and the repository root from there. It writes nothing into the working tree. The
# Temporal command, the restate-server, the servers' data, their logs, the built
# binaries and each experiment's scratch directory all go under $TMPDIR, so a
# checkout stays clean and `jj status` after a run shows nothing new.
#
# It stops at the first failure up to the point where the experiments start, naming
# what failed. From there it runs all eighteen whatever each one does, so one failure
# does not hide the other seventeen, and ends with the line
#
#   temporal-evaluation: all 18 experiments passed
#
# only when all eighteen passed. Otherwise it names each experiment that did not pass
# and exits non-zero without that line.

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

# The six Temporal namespaces, one per Temporal experiment. The server's Action
# counter is a server-lifetime counter carrying a namespace label, so experiment 6 can
# only count its own Actions if nothing else ran in its namespace.
readonly namespaces=(e1 e2 e3 e4 e5 e6)

# How long to wait for the server to answer, and how long one experiment may take. The
# slowest experiment on 2026-11-07 was the eleventh, at about 50 seconds.
readonly server_ready_timeout=120
readonly experiment_timeout=300

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

# --- 2. the PostgreSQL nine of the eighteen need -------------------------------------

# Experiments 7 to 9, 13 to 15 and 16 to 18 measure libraries whose whole state lives
# in the application's own PostgreSQL. Without a database there is nothing to measure,
# and a run that skipped them would not be this evaluation.
[[ -n "${LEGION_TEST_PG_DSN:-}" ]] ||
	die "LEGION_TEST_PG_DSN is empty. Nine of the eighteen experiments -- 7 to 9 for DBOS, 13 to 15 for go-workflows and 16 to 18 for durabletask-go -- measure libraries that keep their workflow state in PostgreSQL, so this run cannot measure them without one. Set LEGION_TEST_PG_DSN to a PostgreSQL URL this process may create a database in and run again."
readonly postgres_dsn="$LEGION_TEST_PG_DSN"
say 'LEGION_TEST_PG_DSN is set'

# --- the trap -----------------------------------------------------------------------

server_pid=''

stop_dev_server() {
	[[ -n "$server_pid" ]] || return 0
	if kill -0 "$server_pid" 2>/dev/null; then
		kill -TERM "$server_pid" 2>/dev/null || true
		# Give it a moment to close its database, then insist.
		local waited=0
		while kill -0 "$server_pid" 2>/dev/null && ((waited < 50)); do
			sleep 0.2
			waited=$((waited + 1))
		done
		kill -KILL "$server_pid" 2>/dev/null || true
	fi
	wait "$server_pid" 2>/dev/null || true
	server_pid=''
}

cleanup() {
	local status=$?
	trap - EXIT INT TERM
	stop_dev_server
	# Any experiment, worker or restate-server still running was started from this
	# run's directory, whose path is in its command line and in no other process's.
	pkill -KILL -f -- "$bin_dir/" 2>/dev/null || true
	pkill -KILL -f -- "$run_dir/" 2>/dev/null || true
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

# A free port, found rather than written down: several agents share this pod and a
# literal port collides with another run of this same script.
port_is_free() {
	local port=$1
	! (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null
}

# The ports this run has already handed out. free_port runs in a command
# substitution, which is a subshell, so it cannot record anything itself: every
# caller adds what it was given to this list, and the next subshell inherits it.
taken_ports=()

port_taken() {
	local port=$1 taken
	for taken in ${taken_ports[*]:-}; do
		[[ "$taken" == "$port" ]] && return 0
	done
	return 1
}

free_port() {
	local port
	for _ in {1..200}; do
		port=$((20000 + RANDOM % 20000))
		if ! port_taken "$port" && port_is_free "$port"; then
			printf '%s\n' "$port"
			return 0
		fi
	done
	return 1
}

grpc_port=$(free_port) || die 'found no free port for the server'
taken_ports+=("$grpc_port")
metrics_port=$(free_port) || die 'found no free port for the metrics endpoint'
taken_ports+=("$metrics_port")
readonly grpc_port metrics_port
readonly server_address="127.0.0.1:$grpc_port"
readonly metrics_url="http://127.0.0.1:$metrics_port/metrics"

namespace_flags=()
for ns in "${namespaces[@]}"; do
	namespace_flags+=(--namespace "$ns")
done

say "starting the dev server on $server_address, metrics on $metrics_url"
"$temporal_bin" server start-dev \
	--headless \
	--port "$grpc_port" \
	--metrics-port "$metrics_port" \
	--db-filename "$server_dir/dev.db" \
	"${namespace_flags[@]}" \
	--log-level warn \
	>"$server_log" 2>&1 &
server_pid=$!

# Ready means: the last namespace exists and the metrics endpoint answers. Both are
# what an experiment needs, and the server reports both only once it is serving.
ready=''
for _ in $(seq 1 "$server_ready_timeout"); do
	if ! kill -0 "$server_pid" 2>/dev/null; then
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
	cat -- "$server_log" >&2 || true
	die "the dev server did not answer on $server_address within ${server_ready_timeout}s; its log is above and in $server_log"
}
say "the dev server answers on $server_address"

# --- 10. the eighteen experiments ------------------------------------------------------

# Every experiment is given a scratch directory of its own and the flags its own
# product needs. The Restate experiments start and stop their own restate-server, one
# per experiment, on ports this script reserves for them.
failed=()
for n in $(seq 1 18); do
	work_dir="$run_dir/work/e$n"
	mkdir -p -- "$work_dir"

	args=(-experiment "$n" -work-dir "$work_dir")
	label=''
	case "$n" in
	[1-6])
		namespace=${namespaces[$((n - 1))]}
		label=" (namespace $namespace)"
		args+=(-address "$server_address" -namespace "$namespace" -repo-root "$repo_root")
		case "$n" in
		3) args+=(-session-worker "$bin_dir/sessionworker") ;;
		6) args+=(-metrics-url "$metrics_url") ;;
		esac
		;;
	7 | 8 | 9)
		args+=(-postgres-dsn "$postgres_dsn" -dbos-worker "$bin_dir/dbosworker")
		;;
	10 | 11 | 12)
		ingress_port=$(free_port) || die "found no free port for experiment $n's Restate ingress"
		taken_ports+=("$ingress_port")
		admin_port=$(free_port) || die "found no free port for experiment $n's Restate admin API"
		taken_ports+=("$admin_port")
		label=" (restate ingress 127.0.0.1:$ingress_port, admin 127.0.0.1:$admin_port)"
		args+=(
			-restate-server "$restate_bin"
			-restate-service "$bin_dir/restateservice"
			-restate-ingress "127.0.0.1:$ingress_port"
			-restate-admin "127.0.0.1:$admin_port"
		)
		;;
	13 | 14 | 15)
		args+=(-postgres-dsn "$postgres_dsn" -gw-worker "$bin_dir/gwworker")
		;;
	16 | 17 | 18)
		args+=(-postgres-dsn "$postgres_dsn" -dt-worker "$bin_dir/dtworker")
		;;
	esac

	printf '\n=== experiment %d%s ===\n' "$n" "$label"
	status=0
	timeout "$experiment_timeout" "$bin_dir/experiments" "${args[@]}" || status=$?
	if ((status != 0)); then
		failed+=("$n")
		printf 'experiment %d: exit status %d\n' "$n" "$status"
	fi

	# The Temporal dev server is only for experiments 1 to 6; nothing after them
	# needs a server this script runs.
	if ((n == 6)); then
		stop_dev_server
		say 'the dev server is stopped: experiments 7 to 18 measure libraries and a server of their own'
	fi
done

# --- 11. the verdict ------------------------------------------------------------------

printf '\n'
if ((${#failed[@]} > 0)); then
	for n in "${failed[@]}"; do
		printf 'temporal-evaluation: experiment %d did not pass\n' "$n" >&2
	done
	printf 'temporal-evaluation: %d of 18 experiments did not pass\n' "${#failed[@]}" >&2
	exit 1
fi
printf 'temporal-evaluation: all 18 experiments passed\n'
