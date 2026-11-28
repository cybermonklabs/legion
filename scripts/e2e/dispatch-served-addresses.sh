#!/usr/bin/env bash
# The live proof of INFRA-5491's checks c-l-served, c-l-record, c-l-proxy, the API half of c-l-refs
# and c-l-cli (Verification v16): Dispatch serves a list of addresses, reads links at former ones,
# logs each request's Host, reads a trusted proxy's client address, and the `dispatch` command reads
# links at served and former addresses. It runs TestServedAddressesLive and TestTrustedProxyLive of
# packages/envoy/cmd/dispatch/served_addresses_e2e_test.go (build tag e2e), which start the real
# envoy-dispatch on a Postgres database of their own behind a TLS front door the test serves for
# the example hosts (a.example.test canonical, b.example.test served, old.example.test former),
# sign a person in through dev-oidc-issuer, check broker tokens against the stub broker of
# internal/ciphertoken/ciphertokentest, run `envoy-dispatch rebuild-refs`, and run this checkout's
# `dispatch` command with Bun. Each step of each check is one subtest, named after it.
#
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     [DISPATCH_BIN=<envoy-dispatch>] [BASE_BIN=<envoy-dispatch>] [BASE_COMMIT=<commit>] \
#     [ISSUER_BIN=<dev-oidc-issuer>] [DISPATCH_CLI=<bin/dispatch.ts>] \
#     bash scripts/e2e/dispatch-served-addresses.sh
#
# LEGION_TEST_PG_DSN is required and must name a loopback host (lib/loopback-dsn.sh): the tests
# make a database of their own for each server on that server, from a template they migrate, and
# drop them when they end. It needs the vector extension in the databases it makes, as every
# Postgres-backed Dispatch test does.
# DISPATCH_BIN names the envoy-dispatch every server but c-l-served (6)'s base one runs; unset, the
# script builds ./cmd/dispatch from this checkout. The negative control names the base commit's
# build here.
# BASE_BIN names the base commit's envoy-dispatch, the build c-l-served (6) holds this build's
# answers with no list equal to; unset, the script builds it from BASE_COMMIT, whose tree it takes
# with `git archive` into its work directory, so the checkout is never touched and its history must
# hold that commit.
# BASE_COMMIT defaults to the commit of main that the branch's latest merge of main took (the
# default below), which is the base the change sits on. A later merge of main into the branch
# moves the base: set the default below to the main commit that merge took (the merge commit's
# second parent, `git rev-parse <merge>^2`); scripts/e2e/README.md and .github/e2e-scripts.json
# name the default and repeat no commit.
# ISSUER_BIN names dev-oidc-issuer; unset, the script builds ./cmd/dev-oidc-issuer.
# DISPATCH_CLI names the bin/dispatch.ts c-l-cli runs, in a tree whose Bun dependencies are
# installed; unset, it is this checkout's packages/envoy-client/bin/dispatch.ts. The command's
# negative control names the base commit's here.
#
# Needs go, bun, python3 (lib/loopback-dsn.py) and, to build the base, git and tar; it starts no
# container. The work directory, under TMPDIR, holds the binaries, the base commit's tree, the
# tests' temporary files and go test's output: removed when the run passes, kept without the
# binaries and the base tree, and printed, when it fails. The last line is the verdict, read from
# the tests' own `--- PASS`, `--- FAIL` and `--- SKIP` lines and never from go test's exit status
# alone: PASS names how many subtests passed, FAIL names every subtest that failed or was skipped,
# since a skipped subtest is a step this run did not prove.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
check=setup
# The two tests this proof runs, and go test's -run for them.
tests='^(TestServedAddressesLive|TestTrustedProxyLive)'

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
# shellcheck source-path=SCRIPTDIR source=lib/loopback-dsn.sh
. "$root/scripts/e2e/lib/loopback-dsn.sh"
# shellcheck source-path=SCRIPTDIR source=lib/run-verdict.sh
. "$root/scripts/e2e/lib/run-verdict.sh"

# The script's inputs, read before the sweep below drops every DISPATCH_* variable.
dsn=${LEGION_TEST_PG_DSN:-}
dispatch_bin=${DISPATCH_BIN:-}
base_bin=${BASE_BIN:-}
base_commit=${BASE_COMMIT:-aa3eaba7c3b2a66a6c021c50974e1a9d8fb70bcf}
issuer_bin=${ISSUER_BIN:-}
dispatch_cli=${DISPATCH_CLI:-}

# A Legion pane exports the real listener (ENVOY_URL), the real Dispatch (DISPATCH_URL,
# DISPATCH_TOKEN_FILE) and an nkey for the real NATS; nothing this run starts inherits any of it,
# nor a SERVED_ADDRESSES_E2E_* value the script did not set. PGHOSTADDR, PGSERVICE and
# PGSERVICEFILE go with them: require_loopback_dsn in lib/loopback-dsn.sh says why a caller of the
# loopback guard clears the three.
mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    DISPATCH_* | ENVOY_* | NATS_* | SERVED_ADDRESSES_E2E_* | PGHOSTADDR | PGSERVICE | PGSERVICEFILE) unset "$name" ;;
  esac
done

work=''
# failed is every subtest the run failed, and skipped every one it skipped, set once go test's
# output is read.
failed=''
skipped=''

# cleanup prints the run's last line through lib/run-verdict.sh, which exits with rc: the
# script's own status for EXIT, 128 plus the signal's number for INT and TERM. PASS needs rc 0 and
# run_end, the script's last statement, so a run a signal ended reads FAIL. The work directory goes
# with a pass; a failed run keeps it without the binaries and the base commit's tree.
cleanup() {
  local rc=$1
  trap - EXIT INT TERM
  set +e
  if [ -n "$work" ]; then
    if run_passed "$rc"; then
      rm -rf "$work"
    else
      rm -rf "${work:?}/bin" "${work:?}/base"
    fi
  fi
  run_verdict "$rc" \
    "dispatch served addresses e2e: PASS in $((SECONDS - started))s: $passed_count subtests passed" \
    "dispatch served addresses e2e: FAIL at $check after $((SECONDS - started))s${failed:+, failing: $failed}${skipped:+, skipped: $skipped}${work:+ (logs: $work)}"
}
passed_count=0
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

begin prerequisites
[ -n "$dsn" ] ||
  fail "LEGION_TEST_PG_DSN is required: it names a loopback PostgreSQL server the tests make and drop their databases on (postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable)"
tools=(go bun python3)
[ -n "$base_bin" ] || tools+=(git tar)
for tool in "${tools[@]}"; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
# require_loopback_dsn assigns db_host the host libpq reads, or its refusal (printf -v).
db_host=''
require_loopback_dsn db_host LEGION_TEST_PG_DSN "$dsn" "the tests make and drop databases there" || fail "$db_host"
for input in DISPATCH_BIN:"$dispatch_bin" BASE_BIN:"$base_bin" ISSUER_BIN:"$issuer_bin"; do
  value=${input#*:}
  if [ -n "$value" ]; then
    [ -x "$value" ] || fail "${input%%:*}=$value is not an executable file"
  fi
done
if [ -n "$dispatch_cli" ]; then
  [ -r "$dispatch_cli" ] || fail "DISPATCH_CLI=$dispatch_cli is not a readable file"
  dispatch_cli=$(cd "$(dirname "$dispatch_cli")" && pwd)/$(basename "$dispatch_cli")
fi
if [ -z "$base_bin" ]; then
  base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
    fail "BASE_COMMIT '$base_commit' names no commit of this repository"
  note "base commit $base_sha"
fi
note "database host $db_host (URL not printed)"
pass

begin build
work=$(mktemp -d "${TMPDIR:-/tmp}/dispatch-served-addresses.XXXXXXXX")
note "work directory $work"
mkdir -p "$work/bin" "$work/tmp"
if [ -z "$dispatch_bin" ]; then
  dispatch_bin=$work/bin/envoy-dispatch
  go -C "$root/packages/envoy" build -o "$dispatch_bin" ./cmd/dispatch || fail "./cmd/dispatch did not build"
  note "built envoy-dispatch from $root/packages/envoy"
else
  note "envoy-dispatch is DISPATCH_BIN=$dispatch_bin"
fi
if [ -z "$issuer_bin" ]; then
  issuer_bin=$work/bin/dev-oidc-issuer
  go -C "$root/packages/envoy" build -o "$issuer_bin" ./cmd/dev-oidc-issuer || fail "./cmd/dev-oidc-issuer did not build"
  note "built dev-oidc-issuer from $root/packages/envoy"
else
  note "dev-oidc-issuer is ISSUER_BIN=$issuer_bin"
fi
if [ -z "$base_bin" ]; then
  # The base tree is the commit's own, taken out of the repository rather than checked out, so this
  # working copy is never touched.
  mkdir -p "$work/base"
  git -C "$root" archive "$base_sha" | tar -x -C "$work/base" || fail "could not take the tree of $base_sha"
  base_bin=$work/bin/envoy-dispatch-base
  go -C "$work/base/packages/envoy" build -o "$base_bin" ./cmd/dispatch || fail "the base commit's ./cmd/dispatch did not build"
  note "built the base commit's envoy-dispatch from $base_sha"
else
  note "the base commit's envoy-dispatch is BASE_BIN=$base_bin"
fi
if [ -n "$dispatch_cli" ]; then
  note "the dispatch command is DISPATCH_CLI=$dispatch_cli"
else
  note "the dispatch command is this checkout's packages/envoy-client/bin/dispatch.ts"
fi
pass

begin tests
log=$work/go-test.log
status=0
# TMPDIR is the work directory's, so every temporary file of the tests (the servers' homes, the
# issuer's secret files, the CA the dispatch command trusts) goes with it.
env DISPATCH_TEST_DATABASE_URL="$dsn" \
  SERVED_ADDRESSES_E2E_DISPATCH_BIN="$dispatch_bin" \
  SERVED_ADDRESSES_E2E_BASE_BIN="$base_bin" \
  SERVED_ADDRESSES_E2E_ISSUER_BIN="$issuer_bin" \
  SERVED_ADDRESSES_E2E_DISPATCH_CLI="$dispatch_cli" \
  TMPDIR="$work/tmp" \
  go -C "$root/packages/envoy" test -tags e2e -count=1 -v -timeout 30m -run "$tests" ./cmd/dispatch 2>&1 | tee "$log" || status=$?
# The verdict is the tests' own lines: a subtest that failed or was skipped is named, and a run
# whose two tests did not each print `--- PASS` fails whatever go test exited with.
failed=$(sed -n 's/^ *--- FAIL: \([^ ]*\) .*/\1/p' "$log" | paste -sd ' ' -)
skipped=$(sed -n 's/^ *--- SKIP: \([^ ]*\) .*/\1/p' "$log" | paste -sd ' ' -)
# A subtest's line is indented under its test's; the two tests' own lines are not.
passed_count=$(grep -c '^  *--- PASS: ' "$log" || true)
[ -z "$failed" ] || fail "the tests failed (go test exited $status)"
[ -z "$skipped" ] || fail "subtests were skipped, so their steps were not run"
for test in TestServedAddressesLive TestTrustedProxyLive; do
  grep -q "^--- PASS: $test " "$log" || fail "$test printed no --- PASS line (go test exited $status)"
done
[ "$status" -eq 0 ] || fail "go test exited $status with no subtest failed"
pass
run_end
