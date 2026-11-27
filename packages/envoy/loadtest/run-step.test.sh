#!/usr/bin/env bash
# packages/envoy/loadtest/run-step.test.sh
#
# Proves that one LOADTEST_ROOT runs one step or series at a time, without a real stack: fakes `go`
# and `uv` on PATH. The fake `go` records each call with the name of the run that made it, and the
# first run's call blocks on a FIFO, so the first run stays inside `stack.sh setup` while the test
# starts a second run on the same root.
#
# On a live stack a second run-step.sh on the root of a running step stops that step's Dispatch
# and listener in its reset, and the first step then finishes with exit 0 and a row of connection
# failures: few of its writes commit, few of its requests get 2xx, and it has no CPU or heap
# profile. So the second run must be refused before it touches the stack:
# before `stack.sh setup`, whose first build is the fake `go` call this test watches for.
#
# Asserts that: (a) a second run-step.sh is refused while a run-step.sh holds the root; (b) a
# run-series.sh is refused while a run-step.sh holds it; (c) a run-step.sh is refused while a
# run-series.sh holds it; (d) once the first run has exited, a new run on the root is not refused;
# (e) a --replay step refuses --events, a --dispatch-commit that names no commit, and a missing
# LOADTEST_REPLAY_ISSUE, before its build; (f) a step and a series refuse two CPU lists that
# overlap, and a step a CPU list taskset refuses, before their build; (g) a step, a --replay step
# and a series refuse a LOADTEST_GOMAXPROCS (--gomaxprocs) other than the server list's CPU count,
# naming both numbers, before their build; (h) a step whose loadgen run refused it before its
# window, for a GOMAXPROCS (meta.json) other than the server list's CPU count, closes on that
# refusal and not on the generic failure; (i) a step refused at a flag check (also one whose --out
# comes after the refused flag), at the lock another run holds (naming that run's pid), by the
# stack.sh setup it runs, at that GOMAXPROCS refusal, and one whose loadgen run ended before its
# window, each leave a not-run record beside the step directory with their exit status, their
# reason and their last lines; (j) a run of a step's name after its not-run record is not refused
# for it, opens its window and leaves no record; (k) common.sh's wait_for_window on a run-step.sh
# prints the start of the window it opens, and, for one that the stack.sh setup refuses, returns
# non-zero within a second of the refusal, giving the record's exit status and reason; on the
# quiet-box gate, its readings faked through the faked stack's `loadgen box-busy`: (l) a step whose
# box reads at or under LOADTEST_MAX_BUSY_BEFORE starts at once, after one reading, and loadgen run
# gets that reading and the bar beside the step's own box_busy_before, read after it; (m) a step
# whose box reads busy and then quiet waits, says so, and starts, naming both readings, which
# loadgen run gets; (n) a step whose box stays busy past LOADTEST_QUIET_WAIT is refused before its
# reset, and its not-run record carries every reading, the bar and the wait; (o) the bar follows
# LOADTEST_MAX_BUSY_BEFORE and the wait LOADTEST_QUIET_WAIT (0: one reading, then the refusal); (p)
# a step, a series and a restart step whose generator list, with the server list, asks for more
# CPUs than the pod's CPU quota are refused before their build, naming the quota, both lists and
# the list auto gives, the step's not-run record carries that refusal, and restart-step.sh refuses
# it itself, before it starts its step; on the faked stack, (q) a step beside
# LOADTEST_SERVER_CPUS under a CPU quota whose generator list is unset, and one whose list is auto,
# run loadgen run on the list auto gives, while with no quota an unset list stays unset.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly run_step="${here}/run-step.sh"
readonly run_series="${here}/run-series.sh"
readonly restart_step="${here}/restart-step.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
readonly calls_file="${temporary_dir}/go-calls"
readonly release_fifo="${temporary_dir}/release"

first_pid=""
cleanup() {
  if [[ -n "$first_pid" ]] && kill -0 "$first_pid" 2>/dev/null; then
    # Unblock the fake go, then stop the first run.
    { printf 'release\n' >"$release_fifo"; } 2>/dev/null &
    kill -TERM "$first_pid" 2>/dev/null || true
    wait "$first_pid" 2>/dev/null || true
  fi
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

mkdir -p "$fake_bin_dir"
cat >"${fake_bin_dir}/go" <<'EOF'
#!/usr/bin/env bash
printf '%s go %s\n' "${LOADTEST_TEST_RUN:?}" "$*" >>"${LOADTEST_TEST_CALLS:?}"
if [[ "$LOADTEST_TEST_RUN" == first ]]; then
  read -r _ <"${LOADTEST_TEST_RELEASE:?}"
fi
exit 1
EOF
cat >"${fake_bin_dir}/uv" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
chmod +x "${fake_bin_dir}/go" "${fake_bin_dir}/uv"

# A checkout that holds what run-step.sh and stack.sh look for before the build.
readonly src="${temporary_dir}/src"
mkdir -p "${src}/packages/envoy/cmd/dispatch" "${src}/packages/envoy/internal/dispatch/store/migrations"
: >"${src}/packages/envoy/internal/dispatch/store/migrations/0001_init.up.sql"
git init -q "$src"

export PATH="${fake_bin_dir}:${PATH}"
export LOADTEST_SRC="$src"
export LOADTEST_TEST_CALLS="$calls_file"
export LOADTEST_TEST_RELEASE="$release_fifo"
# A cgroup root with no file: a pod with no CPU quota, whatever the box this test runs on
# (common.sh, CGROUP_ROOT), so every step's generator list is the one it gives. quota2 is a pod of
# CPUs 0 and 1 under a CPU quota of 2.
readonly no_cgroup="${temporary_dir}/no-cgroup" quota2="${temporary_dir}/cgroup-quota2"
mkdir -p "$no_cgroup" "$quota2"
printf '200000 100000\n' >"${quota2}/cpu.max"
printf '0-1\n' >"${quota2}/cpuset.cpus.effective"
export LOADTEST_CGROUP_ROOT="$no_cgroup"

step_args=(--events 0 --sessions 1 --requests 0)

# start_first ROOT CMD...: start CMD as the first run on ROOT and wait until it is inside the fake
# go, holding whatever it holds of the root.
start_first() {
  local root=$1
  shift
  : >"$calls_file"
  rm -f "$release_fifo"
  mkfifo "$release_fifo"
  LOADTEST_ROOT="$root" LOADTEST_TEST_RUN=first "$@" >"${temporary_dir}/first-output" 2>&1 &
  first_pid=$!
  local i
  for ((i = 0; i < 300; i++)); do
    grep -q '^first go ' "$calls_file" 2>/dev/null && return 0
    if ! kill -0 "$first_pid" 2>/dev/null; then
      cat "${temporary_dir}/first-output" >&2
      printf 'FAIL: the first run exited before it reached its build\n' >&2
      exit 1
    fi
    sleep 0.1
  done
  cat "${temporary_dir}/first-output" >&2
  printf 'FAIL: the first run did not reach its build within 30 s\n' >&2
  exit 1
}

stop_first() {
  printf 'release\n' >"$release_fifo"
  wait "$first_pid" 2>/dev/null || true
  first_pid=""
}

# expect_refused ROOT WHAT CMD...: CMD, run on ROOT while the first run holds it, exits non-zero
# and never reaches its build.
expect_refused() {
  local root=$1 what=$2
  shift 2
  local rc=0
  LOADTEST_ROOT="$root" LOADTEST_TEST_RUN=second "$@" >"${temporary_dir}/second-output" 2>&1 || rc=$?
  if grep -q '^second go ' "$calls_file"; then
    cat "${temporary_dir}/second-output" >&2
    printf 'FAIL: %s was not refused: it went on to stack.sh setup (its go build ran), so on a live stack it would reset the running step'"'"'s Dispatch and listener\n' "$what" >&2
    exit 1
  fi
  if ((rc == 0)); then
    cat "${temporary_dir}/second-output" >&2
    printf 'FAIL: %s exited 0 while another run held the root\n' "$what" >&2
    exit 1
  fi
}

# expect_record WHAT FILE STATUS REASON [LINE]: FILE is the not-run record of the step WHAT names,
# with exit status STATUS, a reason that holds REASON and, when LINE is given, a last line that holds
# LINE.
expect_record() {
  local what=$1 file=$2 status=$3 reason=$4 line=${5:-}
  if [[ ! -f $file ]]; then
    printf 'FAIL: %s left no not-run record at %s\n' "$what" "$file" >&2
    exit 1
  fi
  if ! jq -e --argjson s "$status" --arg r "$reason" --arg l "$line" \
    '.exit_status == $s and (.reason | contains($r)) and ($l == "" or ([.last_lines[] | select(contains($l))] | length > 0))' \
    "$file" >/dev/null; then
    cat "$file" >&2
    printf 'FAIL: %s: its not-run record does not hold exit status %s, a reason with "%s" and a last line with "%s"\n' \
      "$what" "$status" "$reason" "$line" >&2
    exit 1
  fi
  printf 'PASS: %s leaves a not-run record with its exit status and reason\n' "$what"
}

root="${temporary_dir}/root-a"
start_first "$root" bash "$run_step" first-step "${step_args[@]}"
expect_refused "$root" "a second run-step.sh on the root of a running run-step.sh" \
  bash "$run_step" second-step "${step_args[@]}"
printf 'PASS: a second run-step.sh is refused while a run-step.sh holds LOADTEST_ROOT\n'
expect_record "a run-step.sh refused at the lock a run-step.sh holds" "${root}/runs/second-step.not-run.json" 1 \
  "is in use by another run (pid ${first_pid},"
expect_refused "$root" "a run-series.sh on the root of a running run-step.sh" bash "$run_series"
printf 'PASS: a run-series.sh is refused while a run-step.sh holds LOADTEST_ROOT\n'
stop_first

root="${temporary_dir}/root-b"
start_first "$root" bash "$run_series"
expect_refused "$root" "a run-step.sh on the root of a running run-series.sh" \
  bash "$run_step" second-step "${step_args[@]}"
printf 'PASS: a run-step.sh is refused while a run-series.sh holds LOADTEST_ROOT\n'
expect_record "a run-step.sh refused at the lock a run-series.sh holds" "${root}/runs/second-step.not-run.json" 1 \
  "is in use by another run (pid ${first_pid},"
stop_first

# Once the first run has exited, the root is free again.
: >"$calls_file"
LOADTEST_ROOT="$root" LOADTEST_TEST_RUN=third bash "$run_step" third-step "${step_args[@]}" \
  >"${temporary_dir}/third-output" 2>&1 || true
if ! grep -q '^third go ' "$calls_file"; then
  cat "${temporary_dir}/third-output" >&2
  printf 'FAIL: a run-step.sh after the first run exited did not reach its build: the root stayed held\n' >&2
  exit 1
fi
printf 'PASS: a run on LOADTEST_ROOT after the first run exited is not refused\n'
expect_record "a run-step.sh whose stack.sh setup failed in its build" "${root}/runs/third-step.not-run.json" 1 \
  "run-step.sh exited 1 before the window opened, with no refusal of its own" "building envoy-dispatch"

# expect_refused_before_build WHAT PATTERN CMD...: CMD, on a free root, exits non-zero with PATTERN
# in its output and never reaches its build.
expect_refused_before_build() {
  local what=$1 pattern=$2
  shift 2
  : >"$calls_file"
  local rc=0
  LOADTEST_ROOT="${temporary_dir}/root-c" LOADTEST_TEST_RUN=fourth "$@" >"${temporary_dir}/fourth-output" 2>&1 || rc=$?
  if ((rc == 0)) || grep -q '^fourth go ' "$calls_file" || ! grep -qF -- "$pattern" "${temporary_dir}/fourth-output"; then
    cat "${temporary_dir}/fourth-output" >&2
    printf 'FAIL: %s was not refused before its build with "%s" (exit %d)\n' "$what" "$pattern" "$rc" >&2
    exit 1
  fi
  printf 'PASS: %s is refused before its build\n' "$what"
}
expect_refused_before_build "a --replay step with --events" "--events is not a replay flag" \
  bash "$run_step" replay-step --replay --events 1
expect_refused_before_build "a --replay step with --dispatch-commit deadbeef" "--dispatch-commit 'deadbeef' names no commit" \
  bash "$run_step" replay-step --replay --dispatch-commit deadbeef
expect_refused_before_build "a --replay step without LOADTEST_REPLAY_ISSUE" "LOADTEST_REPLAY_ISSUE is required" \
  env -u LOADTEST_REPLAY_ISSUE bash "$run_step" replay-step --replay
expect_refused_before_build "a step with overlapping CPU lists" "LOADTEST_SERVER_CPUS='0' and LOADTEST_GENERATOR_CPUS='0' overlap on CPU 0" \
  env LOADTEST_SERVER_CPUS=0 LOADTEST_GENERATOR_CPUS=0 bash "$run_step" cpu-step "${step_args[@]}"
expect_refused_before_build "a step with a CPU list taskset refuses" "LOADTEST_GENERATOR_CPUS='4095' is a CPU list taskset refuses" \
  env LOADTEST_GENERATOR_CPUS=4095 bash "$run_step" cpu-step "${step_args[@]}"
expect_refused_before_build "a series with overlapping CPU lists" "overlap on CPU 0" \
  env LOADTEST_SERVER_CPUS=0 LOADTEST_GENERATOR_CPUS=0 bash "$run_series"
expect_refused_before_build "a step whose LOADTEST_GOMAXPROCS is not the server list's CPU count" \
  "LOADTEST_GOMAXPROCS=2 (run-step.sh --gomaxprocs) differs from LOADTEST_SERVER_CPUS='0', 1 CPU" \
  env LOADTEST_SERVER_CPUS=0 LOADTEST_GOMAXPROCS=2 bash "$run_step" cpu-step "${step_args[@]}"
expect_refused_before_build "a --replay step whose --gomaxprocs is not the server list's CPU count" \
  "LOADTEST_GOMAXPROCS=2 (run-step.sh --gomaxprocs) differs from LOADTEST_SERVER_CPUS='0', 1 CPU" \
  env LOADTEST_SERVER_CPUS=0 LOADTEST_REPLAY_ISSUE=INFRA-1 bash "$run_step" replay-step --replay --gomaxprocs 2
expect_refused_before_build "a series whose LOADTEST_GOMAXPROCS is not the server list's CPU count" \
  "LOADTEST_GOMAXPROCS=2 (run-step.sh --gomaxprocs) differs from LOADTEST_SERVER_CPUS='0', 1 CPU" \
  env LOADTEST_SERVER_CPUS=0 LOADTEST_GOMAXPROCS=2 bash "$run_series"
expect_refused_before_build "a step whose generator list is too wide for the pod's CPU quota" \
  "LOADTEST_GENERATOR_CPUS='1-3', 3 CPUs, and LOADTEST_SERVER_CPUS='0', 1 CPU, ask for 4 CPUs, more than the pod's CPU quota of 2 CPUs ('200000 100000' under ${quota2})" \
  env LOADTEST_CGROUP_ROOT="$quota2" LOADTEST_SERVER_CPUS=0 LOADTEST_GENERATOR_CPUS=1-3 bash "$run_step" cpu-step "${step_args[@]}"
expect_record "a step whose generator list is too wide for the pod's CPU quota" "${temporary_dir}/root-c/runs/cpu-step.not-run.json" 1 \
  "the pinned server side with them, since taskset reserves no CPU inside it. Set LOADTEST_GENERATOR_CPUS=auto, which is '1' here"
expect_refused_before_build "a series whose generator list is too wide for the pod's CPU quota" \
  "ask for 4 CPUs, more than the pod's CPU quota of 2 CPUs" \
  env LOADTEST_CGROUP_ROOT="$quota2" LOADTEST_SERVER_CPUS=0 LOADTEST_GENERATOR_CPUS=1-3 bash "$run_series"
# restart-step.sh settles the lists before its lock, so the refusal is its own, not its step's.
expect_refused_before_build "a restart step whose generator list is too wide for the pod's CPU quota" \
  "restart-step.sh: LOADTEST_GENERATOR_CPUS='1-3', 3 CPUs, and LOADTEST_SERVER_CPUS='0', 1 CPU, ask for 4 CPUs" \
  env LOADTEST_CGROUP_ROOT="$quota2" LOADTEST_SERVER_CPUS=0 LOADTEST_GENERATOR_CPUS=1-3 bash "$restart_step" restart-quota

# A step refused at a flag check leaves its not-run record beside its step directory, also when the
# --out that names that directory comes after the refused flag.
rc=0
LOADTEST_ROOT="${temporary_dir}/root-c" LOADTEST_TEST_RUN=fifth bash "$run_step" flag-step --events 1 --sessions 1 \
  >"${temporary_dir}/fifth-output" 2>&1 || rc=$?
((rc == 1)) || { cat "${temporary_dir}/fifth-output" >&2; printf 'FAIL: a step without --requests exited %d, not 1\n' "$rc" >&2; exit 1; }
expect_record "a step refused at a flag check (no --requests)" "${temporary_dir}/root-c/runs/flag-step.not-run.json" 1 \
  "run-step.sh: --requests is required (0 sends no requests)" "run-step.sh: --requests is required"
rc=0
LOADTEST_ROOT="${temporary_dir}/root-c" LOADTEST_TEST_RUN=fifth bash "$run_step" flag-step --bogus 1 \
  --out "${temporary_dir}/elsewhere/flag-out" >"${temporary_dir}/fifth-output" 2>&1 || rc=$?
((rc == 1)) || { cat "${temporary_dir}/fifth-output" >&2; printf 'FAIL: a step with --bogus exited %d, not 1\n' "$rc" >&2; exit 1; }
expect_record "a step refused at a flag check before its --out" "${temporary_dir}/elsewhere/flag-out.not-run.json" 1 \
  "run-step.sh: unknown flag '--bogus'"

# A step that loadgen run refused before its window, its meta.json recording a GOMAXPROCS other
# than the server list's CPU count, closes on that refusal, naming both numbers, and not on the
# generic failure. The stack is faked: run-step.sh and common.sh are copied beside a stack.sh
# whose every command succeeds but `loadgen run`, which writes that meta.json and exits 1. The fake
# does what the steps after this one need under three variables: LOADTEST_TEST_SETUP=refuse refuses
# the setup 1.5 s in, as stack.sh refuses a port that another process holds, and
# LOADTEST_TEST_LOADGEN=early ends `loadgen run` before it writes meta.json, as a failed dashboard
# probe does, while LOADTEST_TEST_LOADGEN=window opens a window 1.2 s in and holds it until the test
# creates end-window in the root (10 s at most); LOADTEST_TEST_BUSY names a file of readings, the
# Nth `loadgen box-busy` printing its Nth line, and 0 past its end or without it, as an idle box
# reads. Each `loadgen box-busy` is counted in box-busy-calls in the root, and each `loadgen run`
# writes its arguments, one a line, to loadgen-run-args there, and the LOADTEST_GENERATOR_CPUS it
# got ("unset" for none) to loadgen-run-generator-cpus.
readonly relay_dir="${temporary_dir}/relay"
readonly relay_root="${temporary_dir}/root-d"
mkdir -p "$relay_dir" "${relay_root}/replay" "${relay_root}/logs"
cp "$run_step" "${here}/common.sh" "$relay_dir/"
cat >"${relay_dir}/stack.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
# shellcheck source=/dev/null
source "$(dirname -- "$0")/common.sh"
case "$1" in
  setup)
    if [ "${LOADTEST_TEST_SETUP:-}" = refuse ]; then
      sleep 1.5
      printf '%s\n' "$EPOCHREALTIME" >"${LOADTEST_ROOT}/setup-refused-at"
      die "port 10004 (listener, for envoy-listener) is already in use by pid 1 (sleep)"
    fi
    ;;
  build-info) echo "abc123 false" ;;
  loadgen)
    case "$2" in
      box-busy)
        printf 'x\n' >>"${LOADTEST_ROOT}/box-busy-calls"
        reading=$(sed -n "$(wc -l <"${LOADTEST_ROOT}/box-busy-calls")p" "${LOADTEST_TEST_BUSY:-/dev/null}")
        echo "${reading:-0}"
        ;;
      run)
        shift 2
        printf '%s\n' "$@" >"${LOADTEST_ROOT}/loadgen-run-args"
        printf '%s\n' "${LOADTEST_GENERATOR_CPUS-unset}" >"${LOADTEST_ROOT}/loadgen-run-generator-cpus"
        while [ "$1" != -out ]; do shift; done
        case ${LOADTEST_TEST_LOADGEN:-gomaxprocs} in
          gomaxprocs)
            printf '%s\n' '{"gomaxprocs":"32","server_cpus":"0-1","server_cpu_count":2}' >"$2/meta.json"
            echo "loadgen run: refused before the window" >&2
            exit 1
            ;;
          early)
            echo "loadgen run: the dashboard probe failed: no stream answered" >&2
            exit 1
            ;;
          window)
            printf '%s\n' '{"window_start":"0001-01-01T00:00:00Z"}' >"$2/meta.json"
            sleep 1.2
            printf '{"window_start":"%s"}\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$2/meta.json"
            for ((i = 0; i < 200; i++)); do
              [ ! -e "${LOADTEST_ROOT}/end-window" ] || break
              sleep 0.05
            done
            echo '{}' >"$2/summary.json"
            ;;
        esac
        ;;
    esac
    ;;
esac
EOF
chmod +x "${relay_dir}/stack.sh"
: >"${relay_root}/replay/last-reset.json"
: >"${relay_root}/logs/listener.out"
: >"${relay_root}/logs/nats.out"
rc=0
env -u LOADTEST_SERVER_CPUS -u LOADTEST_GENERATOR_CPUS -u LOADTEST_GOMAXPROCS -u LOADTEST_MAX_BUSY_BEFORE -u LOADTEST_QUIET_WAIT \
  LOADTEST_ROOT="$relay_root" LOADTEST_REPLAY_ISSUE=INFRA-1 bash "${relay_dir}/run-step.sh" relay-step --replay \
  >"${temporary_dir}/relay-output" 2>&1 || rc=$?
last=$(tail -n 1 "${temporary_dir}/relay-output")
if ((rc == 0)) || grep -q 'failed: loadgen run exited' "${temporary_dir}/relay-output" ||
  [[ $last != "run-step.sh: STEP relay-step IS REFUSED before its window opened, so nothing was measured: Dispatch ran with GOMAXPROCS 32 (meta.json gomaxprocs) pinned to LOADTEST_SERVER_CPUS='0-1', 2 CPUs: "* ]]; then
  cat "${temporary_dir}/relay-output" >&2
  printf 'FAIL: a step loadgen run refused for its GOMAXPROCS did not close on that refusal (exit %d)\n' "$rc" >&2
  exit 1
fi
printf 'PASS: a step refused for a GOMAXPROCS other than the server list'"'"'s CPU count closes on that refusal\n'

# The environment of a step on the faked stack.
relay_env=(env -u LOADTEST_SERVER_CPUS -u LOADTEST_GENERATOR_CPUS -u LOADTEST_GOMAXPROCS -u LOADTEST_MAX_BUSY_BEFORE
  -u LOADTEST_QUIET_WAIT LOADTEST_ROOT="$relay_root" LOADTEST_REPLAY_ISSUE=INFRA-1)

# A step whose loadgen run ends before its window, writing no meta.json: its record holds the
# refusal run-step.sh dies with and loadgen run's own line. Then the record of the step above,
# whose loadgen run did write meta.json, with no window in it.
rc=0
"${relay_env[@]}" LOADTEST_TEST_LOADGEN=early bash "${relay_dir}/run-step.sh" early-step --replay \
  >"${temporary_dir}/early-output" 2>&1 || rc=$?
((rc == 1)) || { cat "${temporary_dir}/early-output" >&2; printf 'FAIL: a step whose loadgen run ended before its window exited %d, not 1\n' "$rc" >&2; exit 1; }
expect_record "a step whose loadgen run ended before its window" "${relay_root}/runs/early-step.not-run.json" 1 \
  "run-step.sh: step early-step failed: loadgen run exited 1 before it wrote summary.json" \
  "loadgen run: the dashboard probe failed: no stream answered"
expect_record "a step loadgen run refused before its window for its GOMAXPROCS" "${relay_root}/runs/relay-step.not-run.json" 1 \
  "run-step.sh: STEP relay-step IS REFUSED before its window opened, so nothing was measured: Dispatch ran with GOMAXPROCS 32" \
  "loadgen run: refused before the window"

# wait_window STEP PID: common.sh's wait_for_window STEP PID on the faked stack's root. Sets rc,
# window (what it printed) and said (what it said on stderr).
wait_window() {
  rc=0
  window=$(LOADTEST_ROOT="$relay_root" bash -c 'source "$1"; resolve_paths; wait_for_window "$2" "$3"' wait \
    "${relay_dir}/common.sh" "$1" "$2" 2>"${temporary_dir}/wait-said") || rc=$?
  said=$(<"${temporary_dir}/wait-said")
}

# A step that its stack.sh setup refuses 1.5 s in, while wait_for_window waits on it: the wait
# returns non-zero within a second of the refusal, giving the record's exit status and the message
# stack.sh died with, which the record holds as its reason.
"${relay_env[@]}" LOADTEST_TEST_SETUP=refuse bash "${relay_dir}/run-step.sh" refused-step --replay \
  >"${temporary_dir}/refused-output" 2>&1 &
refused_pid=$!
wait_window refused-step "$refused_pid"
returned=$EPOCHREALTIME
wait "$refused_pid" || true
expect_record "a step its stack.sh setup refused" "${relay_root}/runs/refused-step.not-run.json" 1 \
  "stack.sh: port 10004 (listener, for envoy-listener) is already in use"
refused_at=$(cat "${relay_root}/setup-refused-at" 2>/dev/null || echo 0)
after=$(awk -v a="$refused_at" -v b="$returned" 'BEGIN { printf "%.2f", b - a }')
if ((rc == 0)) || [[ -n $window ]] ||
  [[ $said != *"run-step.sh exited 1 at "*"stack.sh: port 10004 (listener, for envoy-listener) is already in use"* ]] ||
  ! awk -v s="$after" 'BEGIN { exit !(s < 1) }'; then
  cat "${temporary_dir}/refused-output" >&2
  printf 'wait_for_window said: %s\n' "$said" >&2
  printf 'FAIL: wait_for_window on a step its setup refused returned %d, %s s after the refusal, not non-zero within a second with the record'"'"'s exit status and reason\n' \
    "$rc" "$after" >&2
  exit 1
fi
printf 'PASS: wait_for_window returns within a second (%s s) of a refused step'"'"'s end, with its record'"'"'s exit status and reason\n' "$after"

# A step refused at a flag check, then run again with its flags right: its record does not make
# the run refuse its step directory, wait_for_window prints the start of the window the run opens,
# and once the step has ended no record of it is left.
rc=0
"${relay_env[@]}" bash "${relay_dir}/run-step.sh" win-step --replay --bogus 1 >"${temporary_dir}/win-output" 2>&1 || rc=$?
((rc == 1)) || { cat "${temporary_dir}/win-output" >&2; printf 'FAIL: a step with --bogus exited %d, not 1\n' "$rc" >&2; exit 1; }
expect_record "a --replay step refused at a flag check" "${relay_root}/runs/win-step.not-run.json" 1 \
  "run-step.sh: unknown flag '--bogus'"
"${relay_env[@]}" LOADTEST_TEST_LOADGEN=window bash "${relay_dir}/run-step.sh" win-step --replay \
  >"${temporary_dir}/win-output" 2>&1 &
win_pid=$!
wait_window win-step "$win_pid"
: >"${relay_root}/end-window"
win_rc=0
wait "$win_pid" || win_rc=$?
if ((rc != 0)) || [[ -z $window || $window == 0001* ||
  $window != "$(jq -r .window_start "${relay_root}/runs/win-step/meta.json" 2>/dev/null)" ]]; then
  cat "${temporary_dir}/win-output" >&2
  printf 'wait_for_window said: %s\n' "$said" >&2
  printf 'FAIL: wait_for_window did not print the start of the window that a run of win-step opened after its not-run record (exit %d, printed "%s")\n' \
    "$rc" "$window" >&2
  exit 1
fi
printf 'PASS: wait_for_window prints the start of the window a step opens\n'
if ((win_rc != 0)) || [[ -e "${relay_root}/runs/win-step.not-run.json" || ! -f "${relay_root}/runs/win-step/summary.json" ]]; then
  cat "${temporary_dir}/win-output" >&2
  printf 'FAIL: a run of win-step after its not-run record exited %d; its record is %s, and its summary.json %s\n' "$win_rc" \
    "$([[ -e "${relay_root}/runs/win-step.not-run.json" ]] && echo "still there" || echo "gone")" \
    "$([[ -f "${relay_root}/runs/win-step/summary.json" ]] && echo "written" || echo "missing")" >&2
  exit 1
fi
printf 'PASS: a run of a step'"'"'s name after its not-run record is not refused for it, and leaves no record once it opened its window\n'

# The quiet-box gate on the faked stack. gate_step STEP READINGS [NAME=VALUE...]: a --replay step
# whose `loadgen box-busy` readings are READINGS, space separated (the gate's, then the step's own
# box_busy_before; 0 past them), its loadgen run opening a window at once, under the NAME=VALUE
# variables. Sets rc, out (what the step wrote), calls (how many box-busy readings it took), before
# (the -box-busy-before loadgen run got) and gate (its -box-busy-gate and -box-busy-gate-max), the
# last two "none" when loadgen run did not run.
gate_step() {
  local step=$1 busy
  local -a readings
  read -ra readings <<<"$2"
  shift 2
  printf '%s\n' "${readings[@]}" >"${temporary_dir}/busy-readings"
  rm -f "${relay_root}/box-busy-calls" "${relay_root}/loadgen-run-args"
  : >"${relay_root}/end-window"
  rc=0
  "${relay_env[@]}" LOADTEST_TEST_BUSY="${temporary_dir}/busy-readings" LOADTEST_TEST_LOADGEN=window "$@" \
    bash "${relay_dir}/run-step.sh" "$step" --replay >"${temporary_dir}/gate-output" 2>&1 || rc=$?
  out=$(<"${temporary_dir}/gate-output")
  busy="${relay_root}/box-busy-calls"
  calls=0
  [[ ! -f $busy ]] || calls=$(wc -l <"$busy")
  before=none gate=none
  if [[ -f "${relay_root}/loadgen-run-args" ]]; then
    before=$(awk 'prev == "-box-busy-before" { print; exit } { prev = $0 }' "${relay_root}/loadgen-run-args")
    gate=$(awk 'prev == "-box-busy-gate" || prev == "-box-busy-gate-max" { printf "%s%s", sep, $0; sep = " " } { prev = $0 }' \
      "${relay_root}/loadgen-run-args")
  fi
}

# gate_fail WHAT: the gate row WHAT failed: print what the step wrote and what loadgen run got, and
# stop.
gate_fail() {
  printf '%s\n' "$out" >&2
  printf 'FAIL: %s (exit %d, %s box-busy readings, loadgen run got -box-busy-before %s and the gate %s)\n' \
    "$1" "$rc" "$calls" "$before" "$gate" >&2
  exit 1
}

# (l) A box at or under the bar: one reading, and the step starts at once. loadgen run gets that
# reading and the bar, and its -box-busy-before is the step's own reading, taken after the gate's.
gate_step gate-quiet "0.0400 0.0200"
((rc == 0 && calls == 2)) && [[ $gate == "0.0400 0.10" && $before == 0.0200 &&
  $out == *"run-step.sh: the box is 0.0400 busy (loadgen box-busy), at or under LOADTEST_MAX_BUSY_BEFORE=0.10: the step starts"* &&
  $out != *"waits for it to go quiet"* && ! -e "${relay_root}/runs/gate-quiet.not-run.json" ]] ||
  gate_fail "a step on a box 4% busy did not start at once on its one reading, with that reading and the bar passed to loadgen run"
printf 'PASS: a step on a quiet box starts at once, and loadgen run gets the reading and the bar beside its own box_busy_before\n'

# (m) A box busy, then quiet: the step says it waits, reads again after the pause, and starts,
# naming both readings, which loadgen run gets.
gate_step gate-wait "0.4200 0.0400 0.0200" LOADTEST_QUIET_WAIT=1
((rc == 0 && calls == 3)) && [[ $gate == "0.4200,0.0400 0.10" && $before == 0.0200 &&
  $out == *"run-step.sh: the box is 0.4200 busy (loadgen box-busy), above LOADTEST_MAX_BUSY_BEFORE=0.10: the step waits for it to go quiet, reading again in 1 s (0 of LOADTEST_QUIET_WAIT=1 s waited)"* &&
  $out == *"run-step.sh: the box is 0.0400 busy (loadgen box-busy), at or under LOADTEST_MAX_BUSY_BEFORE=0.10, after 1 s of waiting: the step starts, its readings 0.4200, 0.0400"* &&
  ! -e "${relay_root}/runs/gate-wait.not-run.json" ]] ||
  gate_fail "a step on a box 42% then 4% busy did not wait, say so, and start naming both readings"
printf 'PASS: a step on a busy box waits for it to go quiet and starts, naming both readings\n'

# (n) A box busy past the wait: the step is refused before its reset and its own box_busy_before
# read, and its not-run record carries every reading, the bar and the wait.
gate_step gate-busy "0.4200 0.4300" LOADTEST_QUIET_WAIT=1
((rc == 1 && calls == 2)) && [[ $gate == none && ! -e "${relay_root}/runs/gate-busy" ]] ||
  gate_fail "a step on a box busy past LOADTEST_QUIET_WAIT was not refused before its own box_busy_before and its reset"
expect_record "a step on a box busy past LOADTEST_QUIET_WAIT" "${relay_root}/runs/gate-busy.not-run.json" 1 \
  "run-step.sh: the box stayed busier than LOADTEST_MAX_BUSY_BEFORE=0.10 through LOADTEST_QUIET_WAIT=1 s, so the step is not run: 2 readings of loadgen box-busy, 0.4200, 0.4300." \
  "the box is 0.4200 busy (loadgen box-busy), above LOADTEST_MAX_BUSY_BEFORE=0.10: the step waits"
jq -e '.box_busy_gate_readings == [0.42, 0.43] and .box_busy_gate_max == 0.1 and .box_busy_gate_wait_s == 1' \
  "${relay_root}/runs/gate-busy.not-run.json" >/dev/null || {
  out=$(cat "${relay_root}/runs/gate-busy.not-run.json")
  gate_fail "the not-run record of a step on a box busy past the wait lacks its readings, the bar or the wait"
}
printf 'PASS: a step on a box busy past the wait is refused, its not-run record carrying every reading, the bar and the wait\n'

# (o) The bar follows LOADTEST_MAX_BUSY_BEFORE: a box 42% busy starts a step held to 0.50 at once
# (LOADTEST_QUIET_WAIT=0, so a bar not followed refuses it at once). The wait follows
# LOADTEST_QUIET_WAIT: under 0 a busy box is read once and refused, its later quiet reading unread.
gate_step gate-bar "0.4200" LOADTEST_MAX_BUSY_BEFORE=0.50 LOADTEST_QUIET_WAIT=0
((rc == 0 && calls == 2)) && [[ $gate == "0.4200 0.50" &&
  $out == *"run-step.sh: the box is 0.4200 busy (loadgen box-busy), at or under LOADTEST_MAX_BUSY_BEFORE=0.50: the step starts"* ]] ||
  gate_fail "a step on a box 42% busy under LOADTEST_MAX_BUSY_BEFORE=0.50 did not start at once, held to that bar"
gate_step gate-no-wait "0.4200 0.0400" LOADTEST_QUIET_WAIT=0
((rc == 1 && calls == 1)) && [[ $gate == none ]] ||
  gate_fail "a step on a busy box under LOADTEST_QUIET_WAIT=0 was not refused on its one reading"
expect_record "a step on a busy box under LOADTEST_QUIET_WAIT=0" "${relay_root}/runs/gate-no-wait.not-run.json" 1 \
  "through LOADTEST_QUIET_WAIT=0 s, so the step is not run: 1 reading of loadgen box-busy, 0.4200."
jq -e '.box_busy_gate_readings == [0.42] and .box_busy_gate_max == 0.1 and .box_busy_gate_wait_s == 0' \
  "${relay_root}/runs/gate-no-wait.not-run.json" >/dev/null || {
  out=$(cat "${relay_root}/runs/gate-no-wait.not-run.json")
  gate_fail "the not-run record of a step refused under LOADTEST_QUIET_WAIT=0 lacks its one reading or the wait"
}
printf 'PASS: the gate'"'"'s bar follows LOADTEST_MAX_BUSY_BEFORE and its wait LOADTEST_QUIET_WAIT\n'

# (q) The generator's list on the faked stack, beside LOADTEST_SERVER_CPUS=0. The pin's check takes
# a verdict kept for the test's own boot, so it times no loop. generator_step STEP CGROUP_ROOT
# [NAME=VALUE...]: gate_step's step on a quiet box (every reading 0), on the cgroup root
# CGROUP_ROOT, under the NAME=VALUE variables. Sets rc and out as gate_step does, and gen (the
# LOADTEST_GENERATOR_CPUS loadgen run got: "unset" for none, "none" when loadgen run did not run).
readonly boot_file="${temporary_dir}/boot_id" boot_id=0f1e2d3c-0000-4000-8000-0000000000aa
printf '%s\n' "$boot_id" >"$boot_file"
printf '{"boot_id":"%s","at":"2026-11-17T00:00:00Z","cpu":0,"unpinned_s":1.00,"pinned_s":2.10,"ratio":2.1,"tries":[]}\n' \
  "$boot_id" >"${relay_root}/affinity.json"
generator_step() {
  local step=$1 root=$2
  shift 2
  rm -f "${relay_root}/loadgen-run-generator-cpus"
  gate_step "$step" 0 LOADTEST_CGROUP_ROOT="$root" LOADTEST_SERVER_CPUS=0 LOADTEST_BOOT_ID_FILE="$boot_file" "$@"
  gen=$(cat "${relay_root}/loadgen-run-generator-cpus" 2>/dev/null || echo none)
}

# generator_fail WHAT: the row WHAT failed: print what the step wrote, and stop.
generator_fail() {
  printf '%s\n' "$out" >&2
  printf 'FAIL: %s (exit %d, loadgen run got LOADTEST_GENERATOR_CPUS %s)\n' "$1" "$rc" "$gen" >&2
  exit 1
}

# Under a quota of 2 on a pod of 0-1, the list unset and the list auto are CPU 1 alone, the CPU the
# quota leaves beside the server's 0, which check_cpu_lists exported; with no quota, unset pins
# nothing.
generator_step gen-unset "$quota2"
((rc == 0)) && [[ $gen == 1 &&
  $out == *"LOADTEST_GENERATOR_CPUS is unset, which beside LOADTEST_SERVER_CPUS under a CPU quota is auto: the generator runs on 1, 1 CPU of the pod outside LOADTEST_SERVER_CPUS='0'"* ]] ||
  generator_fail "a step beside LOADTEST_SERVER_CPUS=0 under a quota of 2, its generator list unset, did not run loadgen run on auto's CPU 1"
generator_step gen-auto "$quota2" LOADTEST_GENERATOR_CPUS=auto
((rc == 0)) && [[ $gen == 1 ]] ||
  generator_fail "a step with LOADTEST_GENERATOR_CPUS=auto under a quota of 2 did not run loadgen run on CPU 1"
generator_step gen-no-quota "$no_cgroup"
((rc == 0)) && [[ $gen == unset ]] ||
  generator_fail "a step beside LOADTEST_SERVER_CPUS=0 with no CPU quota pinned its generator"
printf 'PASS: beside the server list under a CPU quota, an unset or auto generator list is the list auto gives, and loadgen run gets it; with no quota an unset list stays unset\n'
