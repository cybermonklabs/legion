#!/usr/bin/env bash
# packages/envoy/scripts/store-forms-check.test.sh
#
# Tests store-forms-check.sh with stand-ins first on PATH, so no stack starts: a dev-broker.sh that
# prints a banner (or one without its exports), an agent-secrets that keeps its secrets in the
# memory secrets file the banner's DEV_BROKER_DIR names and answers each form as the real one
# does, on a 1Password vault as well (where restore refuses, naming the 1Password app's Archive),
# and an op that deletes an item of that file. Each control must fail as it states, beside a
# run that must pass:
#
#   1. every form answering as it should passes: `7 rows ran, 7 passed`, exit 0;
#   2. a show that fails (STUB_FAIL=show) prints `FAIL show` and exits 1;
#   3. a banner that lacks the exports runs no form: `0 rows ran`, exit 1;
#   4. --control vanished, which removes the secret from the memory file after create, fails on
#      show;
#   5. --store onepassword, every form answering as on a 1Password vault, passes, its restore row
#      on the store's refusal, and starts the stack on the vault with no GCP setting;
#   6. on 1Password a restore that succeeds (STUB_RESTORES=1) fails the restore row;
#   7. --store onepassword without its vault and token file exits 2 and starts no stack;
#   8. --store onepassword --control vanished, which deletes the item through op, fails on show.
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
echo "${DEV_BROKER_ONEPASSWORD_VAULT:-}|${DEV_BROKER_ONEPASSWORD_TOKEN_FILE:-}|${DEV_BROKER_GCP_PREFIX:-}|${DEV_BROKER_GCP_KMS_KEY_NAME:-}" >"$STUB_STACK_ENV"
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
# would, under the slug of the name, and prints the real form's lines. STUB_FAIL names a form that
# exits 1. On a 1Password vault (DEV_BROKER_ONEPASSWORD_VAULT set) restore refuses as the real
# form does there, unless STUB_RESTORES is set.
cat >"$WORK/bin/agent-secrets" <<'EOF'
#!/usr/bin/env bash
shift
form="$1" name="$2"
file="$DEV_BROKER_DIR/memory-secrets.json"
id="example/agent-secrets/$(echo "$name" | tr '[:upper:]_' '[:lower:]-')"
held() { jq -e --arg id "$id" 'any(.secrets[]; .name == $id)' "$file" >/dev/null; }
if [ "${STUB_FAIL:-}" = "$form" ]; then echo "agent-secrets secret $form: failed" >&2; exit 1; fi
case "$form" in
create)
  jq --arg id "$id" '.secrets += [{name: $id}]' "$file" >"$file.new" && mv "$file.new" "$file"
  echo "broker: serving $name from the memory store" ;;
show) held || { echo "agent-secrets secret show: $name: not found" >&2; exit 1; }; echo "name: $name" ;;
list) jq -c '[.secrets[] | {name: (.name | ltrimstr("example/agent-secrets/") | ascii_upcase | gsub("-"; "_"))}]' "$file" ;;
set | retag) held || exit 1; echo "broker: serving $name from the memory store" ;;
restore)
  if [ -n "${DEV_BROKER_ONEPASSWORD_VAULT:-}" ] && [ -z "${STUB_RESTORES:-}" ]; then
    echo "agent-secrets secret restore: the store has no call that restores a deleted secret: 1Password has no call that restores an archived item; restore the item agent-secrets-$(echo "$name" | tr '[:upper:]_' '[:lower:]-') from the 1Password app's Archive" >&2
    exit 1
  fi
  held || exit 1; echo "broker: serving $name from the memory store" ;;
delete) held || exit 1; echo "broker: $name is deleted in the memory store" ;;
esac
EOF
# The stand-in op: `op item delete <title> --vault <vault>` deletes, for good, the secret of that
# title from the memory file, as op deletes an item of the vault.
cat >"$WORK/bin/op" <<'EOF'
#!/usr/bin/env bash
[ "$1 $2" = "item delete" ] && [ "$4" = --vault ] && [ "$5" = "$DEV_BROKER_ONEPASSWORD_VAULT" ] || { echo "op: unexpected $*" >&2; exit 1; }
file="$DEV_BROKER_DIR/memory-secrets.json"
id="example/agent-secrets/${3#agent-secrets-}"
jq --arg id "$id" '.secrets |= map(select(.name != $id))' "$file" >"$file.new" && mv "$file.new" "$file"
EOF
chmod +x "$WORK/bin/dev-broker.sh" "$WORK/bin/agent-secrets" "$WORK/bin/op"

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
  env PATH="$WORK/bin:$PATH" STUB_STACK_ENV="$WORK/$name.stack" "${env[@]}" bash "$CHECK" "$@" >"$WORK/$name.out" 2>"$WORK/$name.err"
  echo "$?" >"$WORK/$name.exit"
  set -e
}
seen() { printf 'exit %s, printed %s' "$(cat "$WORK/$1.exit")" "$(tr '\n' '|' <"$WORK/$1.out")"; }
has() { grep -qxF -- "$2" "$WORK/$1.out"; }

run full -- --store memory
judge "$([ "$(cat "$WORK/full.exit")" = 0 ] && has full 'store-forms-check: 7 rows ran, 7 passed' && echo 0 || echo 1)" \
  "control 1: every form answering as it should passes" "$(seen full)"

run failed STUB_FAIL=show -- --store memory
judge "$([ "$(cat "$WORK/failed.exit")" = 1 ] && grep -q '^FAIL show: ' "$WORK/failed.out" && has failed 'store-forms-check: 7 rows ran, 6 passed' && echo 0 || echo 1)" \
  "control 2: a show that fails fails the check, naming show" "$(seen failed)"

run no-exports STUB_NO_EXPORTS=1 -- --store memory
judge "$([ "$(cat "$WORK/no-exports.exit")" = 1 ] && has no-exports 'store-forms-check: 0 rows ran, 0 passed' && echo 0 || echo 1)" \
  "control 3: a banner without its exports runs no form and fails" "$(seen no-exports)"

run vanished -- --store memory --control vanished
judge "$([ "$(cat "$WORK/vanished.exit")" = 1 ] && has vanished 'PASS create' && grep -q '^FAIL show: ' "$WORK/vanished.out" && echo 0 || echo 1)" \
  "control 4: a secret removed behind the CLI's back fails the check on show" "$(seen vanished)"

vault="vault0000000000000000000aa"
token="$WORK/onepassword-token"
printf 'a-token-for-the-test\n' >"$token"
onepassword=(DEV_BROKER_ONEPASSWORD_VAULT="$vault" DEV_BROKER_ONEPASSWORD_TOKEN_FILE="$token" DEV_BROKER_GCP_PREFIX=projects/0/secrets/x- DEV_BROKER_GCP_KMS_KEY_NAME=k)

run op-full "${onepassword[@]}" -- --store onepassword
judge "$([ "$(cat "$WORK/op-full.exit")" = 0 ] && has op-full 'store-forms-check: 7 rows ran, 7 passed' && has op-full 'PASS restore' \
  && [ "$(cat "$WORK/op-full.stack")" = "$vault|$token||" ] && echo 0 || echo 1)" \
  "control 5: on a 1Password vault every form passes, restore on the store's refusal, on a stack with no GCP setting" "$(seen op-full), stack env $(cat "$WORK/op-full.stack" 2>/dev/null)"

run op-restores "${onepassword[@]}" STUB_RESTORES=1 -- --store onepassword
judge "$([ "$(cat "$WORK/op-restores.exit")" = 1 ] && grep -q '^FAIL restore: ' "$WORK/op-restores.out" && has op-restores 'store-forms-check: 7 rows ran, 6 passed' && echo 0 || echo 1)" \
  "control 6: on a 1Password vault a restore that succeeds fails the restore row" "$(seen op-restores)"

run op-unset -- --store onepassword
judge "$([ "$(cat "$WORK/op-unset.exit")" = 2 ] && grep -qF 'store-forms-check: --store onepassword needs DEV_BROKER_ONEPASSWORD_VAULT and DEV_BROKER_ONEPASSWORD_TOKEN_FILE' "$WORK/op-unset.err" && [ ! -e "$WORK/op-unset.stack" ] && echo 0 || echo 1)" \
  "control 7: --store onepassword without its vault and token file exits 2 and starts no stack" "$(seen op-unset), stderr $(tr '\n' '|' <"$WORK/op-unset.err")"

run op-vanished "${onepassword[@]}" -- --store onepassword --control vanished
judge "$([ "$(cat "$WORK/op-vanished.exit")" = 1 ] && has op-vanished 'PASS create' && grep -q '^FAIL show: ' "$WORK/op-vanished.out" && echo 0 || echo 1)" \
  "control 8: on a 1Password vault an item deleted through op behind the CLI's back fails the check on show" "$(seen op-vanished)"

if [ "$FAILED" -ne 0 ]; then
  echo "store-forms-check.test.sh: failed ($FAILED of $((PASSED + FAILED)))"
  exit 1
fi
echo "store-forms-check.test.sh: passed"
