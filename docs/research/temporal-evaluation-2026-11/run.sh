#!/usr/bin/env bash
#
# run.sh reproduces the six experiments of the Temporal evaluation of INFRA-1546 on a
# real Temporal server.
#
#   bash docs/research/temporal-evaluation-2026-11/run.sh
#
# It runs from the repository root, or from anywhere else: it finds its own directory
# and the repository root from there. It writes nothing into the working tree. The
# Temporal command, the server's database, the server's log, the built binaries and
# each experiment's scratch directory all go under $TMPDIR, so a checkout stays clean
# and `jj status` after a run shows nothing new.
#
# It stops at the first failure up to the point where the experiments start, naming
# what failed. From there it runs all six whatever each one does, so one failure does
# not hide the other five, and ends with the line
#
#   temporal-evaluation: all 6 experiments passed
#
# only when all six passed. Otherwise it names each experiment that did not pass and
# exits non-zero without that line.

set -o errexit -o nounset -o pipefail

# The pinned Temporal command. Every version here is an exact literal, never a moving
# tag: a run months from now must install the same command this evaluation measured.
# The archive, its size and its checksum were read on 2026-11-07.
readonly cli_version='1.10.0'
readonly cli_url='https://temporal.download/cli/archive/v1.10.0?platform=linux&arch=amd64'
readonly cli_sha256='521f461f3d108902763bb71e85cd0fd382dc9357b59604ae03da6ae734d16890'
readonly cli_version_line='temporal version 1.10.0 (Server 1.32.2, UI 2.56.1)'

# The Go module proxy the prototype's dependencies come from.
readonly go_proxy_url='https://proxy.golang.org/'

# The six namespaces, one per experiment. The server's Action counter is a
# server-lifetime counter carrying a namespace label, so experiment 6 can only count
# its own Actions if nothing else ran in its namespace.
readonly namespaces=(e1 e2 e3 e4 e5 e6)

# How long to wait for the server to answer, and how long one experiment may take. The
# slowest experiment on 2026-11-07 was the fifth, at about 15 seconds.
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
# writes about 120 MB: the Temporal archive, the extracted command, the server's
# database and the built binaries. All of it belongs on the issue's own volume.
[[ -n "${TMPDIR:-}" ]] ||
	die "TMPDIR is unset. This run writes about 120 MB, which belongs on the issue's own volume, not on the container disk every agent in the pod shares. Set TMPDIR to a writable directory on that volume and run again."

tmp_root=${TMPDIR%/}
[[ "$tmp_root" != /tmp && "$tmp_root" != /tmp/* ]] ||
	die "TMPDIR is $TMPDIR, which is under /tmp. This run writes about 120 MB, which belongs on the issue's own volume, not on the container disk every agent in the pod shares. Set TMPDIR to a writable directory on that volume and run again."
[[ -d "$tmp_root" ]] || die "TMPDIR is $TMPDIR, which is not a directory"

# A symbolic link to /tmp is still /tmp.
tmp_real=$(cd -- "$tmp_root" && pwd -P) ||
	die "TMPDIR is $TMPDIR, which cannot be entered"
[[ "$tmp_real" != /tmp && "$tmp_real" != /tmp/* ]] ||
	die "TMPDIR is $TMPDIR, which resolves to $tmp_real, under /tmp. This run writes about 120 MB, which belongs on the issue's own volume, not on the container disk every agent in the pod shares. Set TMPDIR to a writable directory on that volume and run again."
[[ -w "$tmp_real" ]] || die "TMPDIR is $TMPDIR, which is not writable"

readonly tmp_real
readonly base_dir="$tmp_real/temporal-evaluation-2026-11"
# The installed command is kept between runs; everything a run produces is not.
readonly cli_dir="$base_dir/temporal-cli-$cli_version"
readonly temporal_bin="$cli_dir/temporal"
readonly run_dir="$base_dir/run"
readonly bin_dir="$run_dir/bin"
readonly server_dir="$run_dir/server"
readonly server_log="$server_dir/server.log"

rm -rf -- "$run_dir"
mkdir -p -- "$cli_dir" "$bin_dir" "$server_dir"
say "temporary directory: $base_dir"
say "repository root: $repo_root"

# --- the trap -----------------------------------------------------------------------

server_pid=''

cleanup() {
	local status=$?
	trap - EXIT INT TERM
	if [[ -n "$server_pid" ]] && kill -0 "$server_pid" 2>/dev/null; then
		kill -TERM "$server_pid" 2>/dev/null || true
		# Give it a moment to close its database, then insist.
		local waited=0
		while kill -0 "$server_pid" 2>/dev/null && ((waited < 50)); do
			sleep 0.2
			waited=$((waited + 1))
		done
		kill -KILL "$server_pid" 2>/dev/null || true
		wait "$server_pid" 2>/dev/null || true
	fi
	# Any experiment or session worker still running was started from this run's bin
	# directory, whose path is in its command line and in no other process's.
	pkill -KILL -f -- "$bin_dir/" 2>/dev/null || true
	exit "$status"
}
trap cleanup EXIT
trap 'die "interrupted"' INT TERM

# --- 2. the two network prerequisites -----------------------------------------------

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
host_answers "$go_proxy_url" ||
	die "the Go module proxy does not answer: $go_proxy_url. This run cannot build the prototype without it."
say 'the Go module proxy answers'

# --- 3. the pinned Temporal command -------------------------------------------------

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

# --- 4. gofmt -----------------------------------------------------------------------

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

# --- 5. go vet ----------------------------------------------------------------------

(cd -- "$prototype_dir" && go vet ./...) || die 'go vet failed in the prototype'
say 'go vet: clean'

# --- 6. the two binaries ------------------------------------------------------------

(cd -- "$prototype_dir" && go build -o "$bin_dir/experiments" ./cmd/experiments) ||
	die 'building cmd/experiments failed'
(cd -- "$prototype_dir" && go build -o "$bin_dir/sessionworker" ./cmd/sessionworker) ||
	die 'building cmd/sessionworker failed'
say "built experiments and sessionworker in $bin_dir"

# --- 7. the dev server --------------------------------------------------------------

# A free port, found rather than written down: several agents share this pod and a
# literal port collides with another run of this same script.
port_is_free() {
	local port=$1
	! (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null
}

free_port() {
	local taken=${1:-0} port
	for _ in {1..200}; do
		port=$((20000 + RANDOM % 20000))
		if [[ "$port" != "$taken" ]] && port_is_free "$port"; then
			printf '%s\n' "$port"
			return 0
		fi
	done
	return 1
}

grpc_port=$(free_port) || die 'found no free port for the server'
metrics_port=$(free_port "$grpc_port") || die 'found no free port for the metrics endpoint'
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

# --- 8. the six experiments ---------------------------------------------------------

failed=()
for n in 1 2 3 4 5 6; do
	namespace=${namespaces[$((n - 1))]}
	work_dir="$run_dir/work/$namespace"
	mkdir -p -- "$work_dir"

	args=(
		-experiment "$n"
		-address "$server_address"
		-namespace "$namespace"
		-work-dir "$work_dir"
		-repo-root "$repo_root"
	)
	case "$n" in
	3) args+=(-session-worker "$bin_dir/sessionworker") ;;
	6) args+=(-metrics-url "$metrics_url") ;;
	esac

	printf '\n=== experiment %d (namespace %s) ===\n' "$n" "$namespace"
	status=0
	timeout "$experiment_timeout" "$bin_dir/experiments" "${args[@]}" || status=$?
	if ((status != 0)); then
		failed+=("$n")
		printf 'experiment %d: exit status %d\n' "$n" "$status"
	fi
done

# --- 9. the verdict -----------------------------------------------------------------

printf '\n'
if ((${#failed[@]} > 0)); then
	for n in "${failed[@]}"; do
		printf 'temporal-evaluation: experiment %d did not pass\n' "$n" >&2
	done
	printf 'temporal-evaluation: %d of 6 experiments did not pass\n' "${#failed[@]}" >&2
	exit 1
fi
printf 'temporal-evaluation: all 6 experiments passed\n'
