#!/usr/bin/env bash
# packages/envoy/scripts/store-forms-check.test.sh
#
# Tests store-forms-check.sh with stand-ins first on PATH, so no stack starts: a dev-broker.sh that
# prints a banner (or one without its exports), an agent-secrets that keeps its secrets in the
# memory secrets file the banner's DEV_BROKER_DIR names and answers each form as the real one
# does, `show --json` among them. Each control must fail as it states, beside a
# run that must pass:
#
#   1. every form answering as it should passes: `7 rows ran, 7 passed`, exit 0;
#   2. a show that fails (STUB_FAIL=show) prints `FAIL show`, fails each row that reads its effect
#      back through show, `7 rows ran, 1 passed`, and exits 1;
#   3. a banner that lacks the exports runs no form: `0 rows ran`, exit 1;
#   4. --control vanished, which removes the secret from the memory file after create, fails on
#      show;
#   5. set, retag, delete and restore that change nothing (STUB_NOOP=1) and print only the line of
#      a reread the broker took: each of the four fails, `7 rows ran, 3 passed`, exit 1.
#
# It prints a PASS: or FAIL: line per property and then `store-forms-check.test.sh: passed` or
# `store-forms-check.test.sh: failed (<n> of <m>)`, and exits 0 or 1.
#
# Usage: packages/envoy/scripts/store-forms-check.test.sh
#
# Needs jq.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/store-forms-check.sh"
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

mkdir -p "$WORK/bin"
# The stand-in stack: its banner names a broker URL and DEV_BROKER_DIR, where the stand-in
# agent-secrets keeps the memory secrets file, and then it waits to be stopped. STUB_NO_EXPORTS
# prints the ready line alone.
cat >"$WORK/bin/dev-broker.sh" <<'EOF'
#!/usr/bin/env bash
dir="$(mktemp -d)"
echo '{"secrets": []}' >"$dir/memory-secrets.json"
echo "dev-broker: ready."
if [ -z "${STUB_NO_EXPORTS:-}" ]; then
  echo "  export AGENT_SECRETS_URL=http://127.0.0.1:9"
  echo "  export AGENT_SECRETS_APPROVER=ada@example.com"
  echo "  export DEV_BROKER_DIR=$dir"
fi
trap 'exit 0' TERM
while :; do sleep 0.1; done
EOF
# The stand-in agent-secrets: each form reads and writes the memory file as the memory store
# would, under the slug of the name, and prints the real form's lines; `show --json` answers the
# fields the check reads. STUB_FAIL names a form that exits 1. STUB_NOOP=1 makes set, retag,
# delete and restore change nothing and print the line of a reread the broker took, which the
# real CLI prints where the broker answers 202.
cat >"$WORK/bin/agent-secrets" <<'EOF'
#!/usr/bin/env bash
shift
form="$1" name="$2"
file="$DEV_BROKER_DIR/memory-secrets.json"
id="example/agent-secrets/$(echo "$name" | tr '[:upper:]_' '[:lower:]-')"
held() { jq -e --arg id "$id" 'any(.secrets[]; .name == $id)' "$file" >/dev/null; }
change() { jq --arg id "$id" "$1" "$file" >"$file.new" && mv "$file.new" "$file"; }
if [ "${STUB_FAIL:-}" = "$form" ]; then echo "agent-secrets secret $form: failed" >&2; exit 1; fi
if [ -n "${STUB_NOOP:-}" ]; then
  case "$form" in
  set | retag | delete | restore) held || exit 1; echo "broker: rereading $name from the memory store"; exit 0 ;;
  esac
fi
case "$form" in
create)
  value="$(head -n1)"
  jq --arg id "$id" --arg v "$value" '.secrets += [{name: $id, value: $v, tags: {tier: "agent"}}]' "$file" >"$file.new" && mv "$file.new" "$file"
  echo "broker: serving $name from the memory store" ;;
show)
  held || { echo "agent-secrets secret show: $name: not found" >&2; exit 1; }
  if [ "${3:-}" = --json ]; then
    jq --arg id "$id" --arg n "$name" '.secrets[] | select(.name == $id) | {name: $n, secret_name: .name, tier: .tags.tier, has_value: (.value != ""), deleted: .deleted_at}' "$file"
  else
    echo "name: $name"
  fi ;;
list) jq -c '[.secrets[] | {name: (.name | ltrimstr("example/agent-secrets/") | ascii_upcase | gsub("-"; "_"))}]' "$file" ;;
set)
  held || exit 1
  value="$(head -n1)"
  jq --arg id "$id" --arg v "$value" '.secrets |= map(if .name == $id then .value = $v else . end)' "$file" >"$file.new" && mv "$file.new" "$file"
  echo "broker: serving $name from the memory store" ;;
retag) held || exit 1; change '.secrets |= map(if .name == $id then .tags.tier = "human" else . end)'; echo "broker: serving $name from the memory store" ;;
delete) held || exit 1; change '.secrets |= map(if .name == $id then .deleted_at = "2026-11-28T00:00:00Z" else . end)'; echo "broker: $name is deleted in the memory store" ;;
restore) held || exit 1; change '.secrets |= map(if .name == $id then del(.deleted_at) else . end)'; echo "broker: serving $name from the memory store" ;;
esac
EOF
chmod +x "$WORK/bin/dev-broker.sh" "$WORK/bin/agent-secrets"

# run NAME ENV... -- ARGS...: the check with the stand-ins, keeping its output and exit status.
run() {
  local name="$1"
  shift
  local env=()
  while [ "$1" != -- ]; do
    env+=("$1")
    shift
  done
  shift
  set +e
  env PATH="$WORK/bin:$PATH" "${env[@]}" bash "$CHECK" "$@" >"$WORK/$name.out" 2>"$WORK/$name.err"
  echo "$?" >"$WORK/$name.exit"
  set -e
}
seen() { printf 'exit %s, printed %s' "$(cat "$WORK/$1.exit")" "$(tr '\n' '|' <"$WORK/$1.out")"; }
has() { grep -qxF -- "$2" "$WORK/$1.out"; }

run full -- --store memory
judge "$([ "$(cat "$WORK/full.exit")" = 0 ] && has full 'store-forms-check: 7 rows ran, 7 passed' && echo 0 || echo 1)" \
  "control 1: every form answering as it should passes" "$(seen full)"

run failed STUB_FAIL=show -- --store memory
judge "$([ "$(cat "$WORK/failed.exit")" = 1 ] && grep -q '^FAIL show: ' "$WORK/failed.out" && has failed 'store-forms-check: 7 rows ran, 1 passed' && echo 0 || echo 1)" \
  "control 2: a show that fails fails the check, naming show, and every write row that reads its effect back through it" "$(seen failed)"

run no-exports STUB_NO_EXPORTS=1 -- --store memory
judge "$([ "$(cat "$WORK/no-exports.exit")" = 1 ] && has no-exports 'store-forms-check: 0 rows ran, 0 passed' && echo 0 || echo 1)" \
  "control 3: a banner without its exports runs no form and fails" "$(seen no-exports)"

run vanished -- --store memory --control vanished
judge "$([ "$(cat "$WORK/vanished.exit")" = 1 ] && has vanished 'PASS create' && grep -q '^FAIL show: ' "$WORK/vanished.out" && echo 0 || echo 1)" \
  "control 4: a secret removed behind the CLI's back fails the check on show" "$(seen vanished)"

run noop STUB_NOOP=1 -- --store memory
judge "$([ "$(cat "$WORK/noop.exit")" = 1 ] && has noop 'PASS create' && has noop 'PASS show' && has noop 'PASS list' &&
  grep -q '^FAIL set: ' "$WORK/noop.out" && grep -q '^FAIL retag: ' "$WORK/noop.out" && grep -q '^FAIL delete: ' "$WORK/noop.out" &&
  grep -q '^FAIL restore: ' "$WORK/noop.out" && has noop 'store-forms-check: 7 rows ran, 3 passed' && echo 0 || echo 1)" \
  "control 5: writes that change nothing fail their rows, though each prints a reread the broker took" "$(seen noop)"

if [ "$FAILED" -ne 0 ]; then
  echo "store-forms-check.test.sh: failed ($FAILED of $((PASSED + FAILED)))"
  exit 1
fi
echo "store-forms-check.test.sh: passed"
