#!/usr/bin/env bash
# packages/envoy/scripts/aws-moto-check.sh
#
# Runs the built agent-secrets against moto's AWS, with no AWS account
# (cmd/agent-secrets/secret_moto_test.go, TestTheBuiltCLIAgainstMoto): it starts moto 5.2.5 on a
# loopback port the kernel chooses, which moto names in its log, under the account the tests' key
# ARN names, waits until it answers, runs the test with AGENT_SECRETS_MOTO_URL naming it and
# AGENT_SECRETS_BINARY naming BINARY (empty: the test builds the CLI from this tree), stops moto,
# and exits with go test's status.
#
#   packages/envoy/scripts/aws-moto-check.sh [BINARY]
#
# BINARY is how a negative control runs an older release against the same moto. It needs uv
# (uvx), curl and go.
set -euo pipefail

binary="${1:-}"
if [ -n "$binary" ]; then
  binary="$(cd "$(dirname "$binary")" && pwd)/$(basename "$binary")"
  [ -x "$binary" ] || { echo "aws-moto-check.sh: $binary is not an executable" >&2; exit 2; }
fi
cd "$(dirname "$0")/.."
work="$(mktemp -d)"
moto_pid=""
cleanup() {
  if [ -n "$moto_pid" ]; then
    kill "$moto_pid" 2>/dev/null || true
    wait "$moto_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

# moto binds a port the kernel chooses (-p 0) and names it in its `Running on` line, so no port is
# free between its choice and moto's bind: a port read from a probe socket that was closed first
# is any program's to take before moto binds it. The log is made before moto starts: the
# background shell opens it only after its fork, and the first read below can come before that.
: >"$work/moto.log"
MOTO_ACCOUNT_ID=111122223333 uvx --from 'moto[server]==5.2.5' moto_server -H 127.0.0.1 -p 0 >"$work/moto.log" 2>&1 &
moto_pid=$!

# The first run downloads moto, so the wait is long; a moto that exits stops it at once.
url=""
for _ in $(seq 1 240); do
  if [ -z "$url" ]; then
    port=$(awk 'match($0, /Running on http:\/\/127\.0\.0\.1:[0-9]+/) { s = substr($0, RSTART, RLENGTH); sub(/.*:/, "", s); print s; exit }' "$work/moto.log")
    [ -z "$port" ] || url="http://127.0.0.1:$port"
  fi
  [ -n "$url" ] && curl -sf "$url/moto-api/" >/dev/null 2>&1 && break
  if ! kill -0 "$moto_pid" 2>/dev/null; then
    echo "aws-moto-check.sh: moto exited before it answered${url:+ at $url}:" >&2
    cat "$work/moto.log" >&2
    exit 1
  fi
  sleep 0.5
done
if [ -z "$url" ] || ! curl -sf "$url/moto-api/" >/dev/null 2>&1; then
  echo "aws-moto-check.sh: moto did not answer at ${url:-a port it named} within 120 s:" >&2
  cat "$work/moto.log" >&2
  exit 1
fi
echo "aws-moto-check.sh: moto answers at $url; running ${binary:-the CLI built from this tree}"

status=0
AGENT_SECRETS_MOTO_URL="$url" AGENT_SECRETS_BINARY="$binary" go test -tags moto -run '^TestTheBuiltCLIAgainstMoto$' -count=1 -v ./cmd/agent-secrets/ || status=$?
exit "$status"
