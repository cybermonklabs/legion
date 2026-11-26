#!/usr/bin/env bash
# packages/envoy/scripts/backend-count.test.sh
#
# Tests backend-count.sh, the counter of each named test's passing runs on each store the registry
# holds, with a stub `go` first on PATH that prints a go test -json report the test writes, so no
# Go test runs. Each control must fail as it states, or this test fails, and each has a run beside
# it the counter must pass, so a counter that fails everything fails here too:
#
#   1. a report whose named test passed on aws and gcp and not on memory fails the counter, which
#      prints `memory=0`; the same report with the memory pass added passes, printing
#      `aws=1 gcp=1 memory=1`;
#   2. a registry directory that holds no store file (BACKEND_COUNT_REGISTRY) prints
#      `UNKNOWN: no registry file` and fails; the real registry, beside it, counts aws, gcp and
#      memory.
#
# It prints a PASS: or FAIL: line per property and then `backend-count.test.sh: passed` or
# `backend-count.test.sh: failed (<n> of <m>)`, and exits 0 or 1.
#
# Usage: packages/envoy/scripts/backend-count.test.sh
#
# Needs jq.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COUNTER="$SCRIPT_DIR/backend-count.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

PASSED=0
FAILED=0
judge() { # <0 for a pass> <property> <what was seen>
  if [ "$1" = 0 ]; then
    PASSED=$((PASSED + 1))
    echo "PASS: $2"
  else
    FAILED=$((FAILED + 1))
    echo "FAIL: $2 ($3)"
  fi
}

# The stub go prints the report $STUB_REPORT names, as go test -json would, and exits 0.
mkdir -p "$WORK/bin"
cat >"$WORK/bin/go" <<'EOF'
#!/usr/bin/env bash
cat "$STUB_REPORT"
EOF
chmod +x "$WORK/bin/go"

# ev ACTION TEST: one event of a go test -json report of ./cmd/cipher.
ev() { jq -nc --arg a "$1" --arg t "$2" '{Action: $a, Package: "github.com/sjawhar/envoy/cmd/cipher", Test: $t}'; }

# count NAME [ENV...]: runs the counter on TestNamed with the stub go, keeping its output and exit
# status as NAME.
count() {
  local name="$1"
  shift
  set +e
  env PATH="$WORK/bin:$PATH" "$@" bash "$COUNTER" ./cmd/cipher/ -- TestNamed >"$WORK/$name.out" 2>"$WORK/$name.err"
  echo "$?" >"$WORK/$name.exit"
  set -e
}
seen() { printf 'exit %s, printed %s, stderr %s' "$(cat "$WORK/$1.exit")" "$(tr '\n' '|' <"$WORK/$1.out")" "$(tr '\n' '|' <"$WORK/$1.err")"; }

echo "backend-count.test.sh: control 1, no memory pass" >&2
{ ev pass TestNamed/aws; ev pass TestNamed/gcp; ev pass TestNamed; } >"$WORK/no-memory.json"
count no-memory STUB_REPORT="$WORK/no-memory.json"
judge "$([ "$(cat "$WORK/no-memory.exit")" = 1 ] && grep -qxF 'TestNamed aws=1 gcp=1 memory=0' "$WORK/no-memory.out" && echo 0 || echo 1)" \
  "control 1: a named test with no memory pass fails the counter, naming memory" "$(seen no-memory)"
{ ev pass TestNamed/aws; ev pass TestNamed/gcp; ev pass TestNamed/memory; ev pass TestNamed; } >"$WORK/every.json"
count every STUB_REPORT="$WORK/every.json"
judge "$([ "$(cat "$WORK/every.exit")" = 0 ] && grep -qxF 'TestNamed aws=1 gcp=1 memory=1' "$WORK/every.out" && echo 0 || echo 1)" \
  "control 1: the same test with a pass on every store passes" "$(seen every)"

echo "backend-count.test.sh: control 2, no registry file" >&2
mkdir -p "$WORK/empty-registry"
cp "$SCRIPT_DIR/../internal/cipher/secretstore/backends/backends.go" "$WORK/empty-registry/"
count no-registry STUB_REPORT="$WORK/every.json" BACKEND_COUNT_REGISTRY="$WORK/empty-registry"
judge "$([ "$(cat "$WORK/no-registry.exit")" = 1 ] && grep -q '^UNKNOWN: no registry file' "$WORK/no-registry.out" && echo 0 || echo 1)" \
  "control 2: a registry directory holding no store file is UNKNOWN" "$(seen no-registry)"

if [ "$FAILED" -ne 0 ]; then
  echo "backend-count.test.sh: failed ($FAILED of $((PASSED + FAILED)))"
  exit 1
fi
echo "backend-count.test.sh: passed"
