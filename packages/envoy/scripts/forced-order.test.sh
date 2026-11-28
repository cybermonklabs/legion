#!/usr/bin/env bash
# packages/envoy/scripts/forced-order.test.sh: refusals, events and verdicts of forced-order.sh.
# Each refusal case requires a non-zero exit and its own message, with nothing printed past the
# refusal. A stand-in that fails earlier must fail each refusal case. The summary and cleanup
# cases use a temporary Go module. Needs internal/testnats/hooktest/skiponly/.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
envoy="$(dirname "$here")"
out="$(mktemp -d)"
trap 'rm -rf -- "$out"' EXIT
failed=0

# The stand-in: a line forced-order.sh prints when go mod download fails, and its exit status.
early="$out/early-failure.sh"
cat >"$early" <<'EOF'
#!/usr/bin/env bash
echo "forced-order.sh: go mod download could not fetch github.com/nats-io/nats-server/v2@v2.15.1" >&2
exit 1
EOF
chmod +x "$early"

# judge <script> <log> <message> <absent> <args...>: whether <script> <args...>, run in packages/envoy,
# exits non-zero, prints <message> (fixed text) and prints no line matching <absent> (grep -E). It
# prints why.
judge() {
  local script=$1 log=$2 message=$3 absent=$4 code=0
  shift 4
  (cd "$envoy" && RUNNER_TEMP="$out/runner" "$script" "$@") >"$log" 2>&1 || code=$?
  if [ "$code" -eq 0 ]; then
    echo "exit 0, want a refusal"
    return 1
  fi
  if ! grep -qF -- "$message" "$log"; then
    echo "exit $code without the refusal's message \"$message\"; its last line: $(tail -n 1 "$log")"
    return 1
  fi
  if grep -qE -- "$absent" "$log"; then
    echo "exit $code, but the output holds /$absent/: $(grep -E -- "$absent" "$log" | head -n 1)"
    return 1
  fi
  echo "exit $code: $(grep -F -- "$message" "$log" | head -n 1)"
}

# refused <name> <message> <absent> <args...>
refused() {
  local name=$1 message=$2 absent=$3 why
  shift 3
  if why="$(judge "$early" "$out/$name.control" "$message" "$absent" "$@")"; then
    echo "FAIL $name: the case passes a run that fails early for another reason ($why)"
    failed=$((failed + 1))
    return
  fi
  echo "control $name: a run that fails early for another reason fails the case ($why)"
  if why="$(judge scripts/forced-order.sh "$out/$name" "$message" "$absent" "$@")"; then
    echo "PASS $name: $why"
  else
    echo "FAIL $name: $why: forced-order.sh $*"
    failed=$((failed + 1))
  fi
}

# A package whose forced-order tests all skip runs no forced-order test.
refused skip-only 'forced-order.sh: no TestForcedOrder test ran in: github.com/sjawhar/envoy/internal/testnats/hooktest/skiponly' \
  '^ok[[:space:]]' ./internal/testnats/hooktest/skiponly/
# -drop names a hold point; a name that is none is refused before any line of nats-server is cut
# and before the build, so a control never "fails" on a compile error.
refused drop-not-a-point 'forced-order.sh: -drop stream: no hook of nats-server ' \
  'dropped [0-9]+ hook line|^# github.com/nats-io/nats-server' -drop stream ./internal/testnats/hooktest/
# -server with an empty value is no version, not the default.
refused server-empty "forced-order.sh: -server '' names no version" \
  '^=== RUN' -server '' ./internal/testnats/hooktest/
# -C with an empty value names no module, not the caller's directory.
refused c-empty "forced-order.sh: -C '' names no module directory" \
  '^=== RUN' -C '' ./internal/testnats/hooktest/
# A module whose go.mod replaces nats-server requires one version and links another, and the
# hooked copy would take the replacement's place: the script refuses it rather than hook the
# version the requirement names. The fixture replaces it with a local module, so no go command
# needs the network; its directory is not named for the case, whose log is $out/<case name>. It
# is a module with no workspace, so the case runs with GOWORK=off: a go.work in a directory above
# $TMPDIR would otherwise take it in, and the script would refuse that workspace instead.
mkdir -p "$out/replacing-module/fork"
printf 'module github.com/nats-io/nats-server/v2\n\ngo 1.21\n' >"$out/replacing-module/fork/go.mod"
printf 'module example.com/replaced\n\ngo 1.21\n\nrequire github.com/nats-io/nats-server/v2 v2.15.1\n\nreplace github.com/nats-io/nats-server/v2 => ./fork\n' \
  >"$out/replacing-module/go.mod"
GOWORK=off refused replaced 'replaces github.com/nats-io/nats-server/v2 with ./fork' \
  '^forced-order.sh: nats-server v[0-9]' -C "$out/replacing-module" .

# The failing fixture prints a line shaped like a package's `ok` line.
# The log must show it once, alongside Go's verdict lines and without an appended summary.
fixture="$out/summary-module"
mkdir -p "$fixture/mixed" "$fixture/pass" "$out/summary-runner" "$out/local"
printf 'module example.com/summary\n\ngo 1.21\n\nrequire github.com/nats-io/nats-server/v2 v2.15.1\n' >"$fixture/go.mod"
cat >"$fixture/mixed/summary_test.go" <<'EOF'
package mixed

import (
	"fmt"
	"testing"
)

func TestForcedOrderPasses(t *testing.T) {}
func TestForcedOrderFails(t *testing.T) {
	fmt.Println("ok\tfixture-output")
	t.Fatal("a fixture failure")
}
func TestForcedOrderSkips(t *testing.T) { t.Skip("a fixture skip") }
EOF
printf 'package pass\n\nimport "testing"\n\nfunc TestForcedOrderPasses(t *testing.T) {}\n' >"$fixture/pass/summary_test.go"
status=0
RUNNER_TEMP="$out/summary-runner" GOWORK=off "$here/forced-order.sh" -C "$fixture" ./mixed \
  >"$out/summary.log" 2>&1 || status=$?
sed 's/\x1b\[[0-9;]*m//g' "$out/summary.log" >"$out/summary.no-color.log"
events=("$out/summary-runner/go-test-events"/forced-order-*.jsonl)
if [ "$status" -eq 1 ] && [ "${#events[@]}" -eq 1 ] && [ -s "${events[0]}" ] &&
  jq -es 'map(select(.Test and (.Action == "pass" or .Action == "fail" or .Action == "skip")) | .Action) == ["pass", "fail", "skip"]' "${events[0]}" >/dev/null &&
  [ "$(grep -cxF $'ok\tfixture-output' "$out/summary.log")" -eq 1 ] &&
  grep -q '^--- PASS: TestForcedOrderPasses ' "$out/summary.log" &&
  grep -q '^--- FAIL: TestForcedOrderFails ' "$out/summary.log" &&
  grep -q '^--- SKIP: TestForcedOrderSkips ' "$out/summary.log" &&
  ! grep -qE '^(DONE |=== (Skipped|Failed|(SKIP|FAIL):))' "$out/summary.no-color.log" &&
  grep -q '^FAIL[[:space:]]\+example.com/summary/mixed' "$out/summary.log" &&
  ! grep -qF 'no TestForcedOrder test ran in:' "$out/summary.log"; then
  echo "PASS summary: Go's verbose verdict and one events file, with no appended summary"
else
  echo "FAIL summary: exit $status"
  cat "$out/summary.log"
  failed=$((failed + 1))
fi

# Both packages' PASS lines can arrive before either package's ok line. A single count
# reset at the first ok would wrongly report the second package as having skipped all tests.
mkdir "$out/interleaved-bin"
cat >"$out/interleaved-bin/go" <<'EOF'
#!/usr/bin/env bash
# List the fixture's tests without compiling them for the interleaved run.
if [ "$1" = test ]; then
  for arg in "${@:2}"; do
    [ "$arg" = -list ] || continue
    printf '%s\n' TestForcedOrderPasses
    printf 'ok  \texample.com/summary/pass\t0.001s\n'
    printf '%s\n' TestForcedOrderPasses TestForcedOrderFails TestForcedOrderSkips
    printf 'ok  \texample.com/summary/mixed\t0.001s\n'
    exit 0
  done
fi
if [ "$1 ${2:-}" != "test -json" ]; then
  exec "$REAL_GO" "$@"
fi
for pkg in example.com/summary/pass example.com/summary/mixed; do
  printf '{"Action":"start","Package":"%s"}\n' "$pkg"
  printf '{"Action":"run","Package":"%s","Test":"TestForcedOrderPasses"}\n' "$pkg"
  printf '{"Action":"output","Package":"%s","Test":"TestForcedOrderPasses","Output":"--- PASS: TestForcedOrderPasses (0.00s)\\n"}\n' "$pkg"
  printf '{"Action":"pass","Package":"%s","Test":"TestForcedOrderPasses","Elapsed":0}\n' "$pkg"
done
for pkg in example.com/summary/pass example.com/summary/mixed; do
  printf '{"Action":"output","Package":"%s","Output":"ok \\t%s\\t0.001s\\n"}\n' "$pkg" "$pkg"
  printf '{"Action":"pass","Package":"%s","Elapsed":0.001}\n' "$pkg"
done
EOF
chmod +x "$out/interleaved-bin/go"

status=0
env -u RUNNER_TEMP TMPDIR="$out/local" GOWORK=off REAL_GO="$(type -P go)" PATH="$out/interleaved-bin:$PATH" \
  "$here/forced-order.sh" -C "$fixture" ./pass ./mixed \
  >"$out/local.log" 2>&1 || status=$?
if [ "$status" -eq 0 ] && grep -q '^--- PASS: TestForcedOrderPasses' "$out/local.log" &&
  grep -q '^ok[[:space:]]\+example.com/summary/pass' "$out/local.log" &&
  grep -q '^ok[[:space:]]\+example.com/summary/mixed' "$out/local.log" &&
  ! grep -q 'no TestForcedOrder test ran in:' "$out/local.log" &&
  [ -z "$(find "$out/local" -mindepth 1 -print)" ]; then
  echo "PASS local: interleaved passing packages keep their verdicts, and the run leaves no file behind"
else
  echo "FAIL local: exit $status"
  cat "$out/local.log"
  failed=$((failed + 1))
fi

echo "forced-order.test.sh: $failed failed of 7"
[ "$failed" -eq 0 ]
