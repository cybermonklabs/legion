#!/usr/bin/env bash
# packages/envoy/scripts/aws-moto-check.sh
#
# Runs the built agent-secrets against moto's AWS, with no AWS account
# (cmd/agent-secrets/secret_moto_test.go, TestTheBuiltCLIAgainstMoto): it starts moto 5.2.5 on a
# free loopback port under the account the tests' key ARN names, waits until it answers, runs the
# test with AGENT_SECRETS_MOTO_URL naming it and AGENT_SECRETS_BINARY naming BINARY (empty: the
# test builds the CLI from this tree), stops moto, and exits with go test's status.
#
#   packages/envoy/scripts/aws-moto-check.sh [BINARY]
#
# BINARY is how a negative control runs an older release against the same moto. It needs uv
# (uvx), curl, python3 and go.
# go-offline-file: a person runs it beside moto; CI vets the moto-tagged test it runs (envoy-go)
# and runs neither.
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

port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
url="http://127.0.0.1:$port"
MOTO_ACCOUNT_ID=111122223333 uvx --from 'moto[server]==5.2.5' moto_server -H 127.0.0.1 -p "$port" >"$work/moto.log" 2>&1 &
moto_pid=$!

# The first run downloads moto, so the wait is long; a moto that exits stops it at once.
for _ in $(seq 1 240); do
  curl -sf "$url/moto-api/" >/dev/null 2>&1 && break
  if ! kill -0 "$moto_pid" 2>/dev/null; then
    echo "aws-moto-check.sh: moto exited before it answered at $url:" >&2
    cat "$work/moto.log" >&2
    exit 1
  fi
  sleep 0.5
done
if ! curl -sf "$url/moto-api/" >/dev/null 2>&1; then
  echo "aws-moto-check.sh: moto did not answer at $url within 120 s:" >&2
  cat "$work/moto.log" >&2
  exit 1
fi
echo "aws-moto-check.sh: moto answers at $url; running ${binary:-the CLI built from this tree}"

status=0
AGENT_SECRETS_MOTO_URL="$url" AGENT_SECRETS_BINARY="$binary" go test -tags moto -run '^TestTheBuiltCLIAgainstMoto$' -count=1 -v ./cmd/agent-secrets/ || status=$?
exit "$status"
