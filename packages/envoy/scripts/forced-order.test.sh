#!/usr/bin/env bash
# packages/envoy/scripts/forced-order.test.sh: the refusals forced-order.sh must make. Each case
# runs the script as a developer or a CI step does, and passes only when the run exits non-zero,
# prints that refusal's own message and prints nothing a run past the refusal would. Each case is
# first judged against a stand-in that fails early for another reason, as a failed download does,
# and must fail there: a case that passes on any refusal would pass a run that never reached the
# one it names. Needs the fixture package internal/testnats/hooktest/skiponly/.
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
  (cd "$envoy" && "$script" "$@") >"$log" 2>&1 || code=$?
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

echo "forced-order.test.sh: $failed failed of 5"
[ "$failed" -eq 0 ]
