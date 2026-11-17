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
# LOADTEST_REPLAY_ISSUE, before its build; (f) a --replay step without --manifest downloads the
# manifest, a project file of LOADTEST_REPLAY_ISSUE's project, from a fake `curl` before its build,
# the token on curl's standard input and never on its command line; it is refused before its build,
# writing no manifest, when it cannot download one, and when the one it downloads has another
# sha256 than the pinned one; and with --manifest FILE it downloads nothing and reaches its build.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly run_step="${here}/run-step.sh"
readonly run_series="${here}/run-series.sh"
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
# The fake curl: a Dispatch artifact version download (-o OUT) of the manifest's project file
# writes LOADTEST_TEST_MANIFEST to OUT; every other call fails. It records each command line, and
# what it read on its standard input (the bearer header), apart.
cat >"${fake_bin_dir}/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${LOADTEST_TEST_CURL_LOG:?}"
cat >>"${LOADTEST_TEST_CURL_STDIN:?}"
out="" prev=""
for a in "$@"; do
  [[ "$prev" == -o ]] && out=$a
  prev=$a
done
case ${*: -1} in
  */api/v1/projects/ACME/artifacts/replay-2026-10-26-json/versions/1) cp "${LOADTEST_TEST_MANIFEST:?}" "$out" ;;
  *) exit 22 ;;
esac
EOF
chmod +x "${fake_bin_dir}/go" "${fake_bin_dir}/uv" "${fake_bin_dir}/curl"

# A checkout that holds what run-step.sh and stack.sh look for before the build.
readonly src="${temporary_dir}/src"
mkdir -p "${src}/packages/envoy/cmd/dispatch" "${src}/packages/envoy/internal/dispatch/store/migrations"
: >"${src}/packages/envoy/internal/dispatch/store/migrations/0001_init.up.sql"
git init -q "$src"

export PATH="${fake_bin_dir}:${PATH}"
export LOADTEST_SRC="$src"
export LOADTEST_TEST_CALLS="$calls_file"
export LOADTEST_TEST_RELEASE="$release_fifo"
readonly curl_log="${temporary_dir}/curl-calls" curl_stdin="${temporary_dir}/curl-stdin"
export LOADTEST_TEST_CURL_LOG="$curl_log" LOADTEST_TEST_CURL_STDIN="$curl_stdin"
: >"$curl_log"
: >"$curl_stdin"

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

root="${temporary_dir}/root-a"
start_first "$root" bash "$run_step" first-step "${step_args[@]}"
expect_refused "$root" "a second run-step.sh on the root of a running run-step.sh" \
  bash "$run_step" second-step "${step_args[@]}"
printf 'PASS: a second run-step.sh is refused while a run-step.sh holds LOADTEST_ROOT\n'
expect_refused "$root" "a run-series.sh on the root of a running run-step.sh" bash "$run_series"
printf 'PASS: a run-series.sh is refused while a run-step.sh holds LOADTEST_ROOT\n'
stop_first

root="${temporary_dir}/root-b"
start_first "$root" bash "$run_series"
expect_refused "$root" "a run-step.sh on the root of a running run-series.sh" \
  bash "$run_step" second-step "${step_args[@]}"
printf 'PASS: a run-step.sh is refused while a run-series.sh holds LOADTEST_ROOT\n'
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

# (f) The replay manifest. The fake curl serves this file as the manifest's project file; its sha256
# replaces the pinned one for the runs that download it.
readonly served_manifest="${temporary_dir}/served-manifest.json"
printf '{"schema": 1, "project": "ACME", "bundle": {"parts": []}, "seed": null}\n' >"$served_manifest"
served_sha="$(sha256sum <"$served_manifest" | cut -d' ' -f1)"
readonly served_sha
readonly token_file="${temporary_dir}/dispatch-token"
printf 'run-step-test-token\n' >"$token_file"
export LOADTEST_TEST_MANIFEST="$served_manifest"
readonly download_env=(LOADTEST_REPLAY_ISSUE=ACME-7 DISPATCH_URL=http://dispatch.invalid DISPATCH_TOKEN_FILE="$token_file")

# expect_reaches_build WHAT ROOT CMD...: CMD, on the free root ROOT, goes on to its build.
expect_reaches_build() {
  local what=$1 root=$2
  shift 2
  : >"$calls_file"
  : >"$curl_log"
  : >"$curl_stdin"
  LOADTEST_ROOT="$root" LOADTEST_TEST_RUN=fifth "$@" >"${temporary_dir}/fifth-output" 2>&1 || true
  if ! grep -q '^fifth go ' "$calls_file"; then
    cat "${temporary_dir}/fifth-output" >&2
    printf 'FAIL: %s did not reach its build\n' "$what" >&2
    exit 1
  fi
}

# fail_with WHAT: the last run's output, then WHAT, and exit 1.
fail_with() {
  cat "${temporary_dir}/fifth-output" >&2
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

root="${temporary_dir}/root-d"
expect_reaches_build "a --replay step that downloads its manifest" "$root" \
  env "${download_env[@]}" LOADTEST_REPLAY_MANIFEST_SHA256="$served_sha" bash "$run_step" replay-step --replay
grep -qx -- '.* http://dispatch.invalid/api/v1/projects/ACME/artifacts/replay-2026-10-26-json/versions/1' "$curl_log" ||
  fail_with "the step did not download the manifest's project file of project ACME: $(cat "$curl_log")"
cmp -s "$served_manifest" "${root}/replay/parts/replay-2026-10-26.json" ||
  fail_with "the downloaded manifest is not at ${root}/replay/parts/replay-2026-10-26.json, beside the parts"
! grep -qF run-step-test-token "$curl_log" || fail_with "the token is on curl's command line"
grep -qx 'Authorization: Bearer run-step-test-token' "$curl_stdin" ||
  fail_with "curl did not read the bearer token on its standard input"
printf 'PASS: a --replay step without --manifest downloads the manifest from the issue'"'"'s project before its build, the token on curl'"'"'s standard input\n'

expect_refused_before_build "a --replay step that cannot download its manifest" "no replay manifest: give --manifest FILE" \
  env -u DISPATCH_URL LOADTEST_REPLAY_ISSUE=ACME-7 bash "$run_step" replay-step --replay
[[ ! -e "${temporary_dir}/root-c/replay" ]] || { printf 'FAIL: the refused step wrote %s\n' "${temporary_dir}/root-c/replay" >&2; exit 1; }

: >"$curl_log"
expect_refused_before_build "a --replay step whose downloaded manifest has another sha256" \
  "the replay manifest replay-2026-10-26.json: sha256 ${served_sha}, not the $(printf '0%.0s' {1..64}) it must have; deleted it" \
  env "${download_env[@]}" LOADTEST_REPLAY_MANIFEST_SHA256="$(printf '0%.0s' {1..64})" bash "$run_step" replay-step --replay
grep -q /api/v1/projects/ACME/ "$curl_log" || { printf 'FAIL: the step was refused before it downloaded the manifest, so the sha256 check was not reached\n' >&2; exit 1; }
[[ ! -e "${temporary_dir}/root-c/replay/parts/replay-2026-10-26.json" ]] ||
  { printf 'FAIL: the manifest of another sha256 was kept\n' >&2; exit 1; }

root="${temporary_dir}/root-e"
expect_reaches_build "a --replay step with --manifest FILE and no DISPATCH_URL" "$root" \
  env -u DISPATCH_URL LOADTEST_REPLAY_ISSUE=ACME-7 bash "$run_step" replay-step --replay --manifest "$served_manifest"
[[ ! -s "$curl_log" ]] || fail_with "a step with --manifest FILE ran curl: $(cat "$curl_log")"
[[ ! -e "${root}/replay/parts/replay-2026-10-26.json" ]] || fail_with "a step with --manifest FILE fetched the manifest"
printf 'PASS: a --replay step with --manifest FILE downloads no manifest and reaches its build\n'
