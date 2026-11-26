#!/usr/bin/env bash
# packages/envoy/scripts/store-forms-check.sh --store memory|gcp|onepassword [--control vanished]
#
# Runs every secret form of agent-secrets against a running broker on one store, the surface check
# of the CLI and the broker reading the same store. It starts dev-broker.sh in that store's mode
# (memory: the default; gcp: DEV_BROKER_GCP_PREFIX and DEV_BROKER_GCP_KMS_KEY_NAME, which the
# caller sets, under whatever Google credential the caller's environment gives agent-secrets;
# onepassword: DEV_BROKER_ONEPASSWORD_VAULT and DEV_BROKER_ONEPASSWORD_TOKEN_FILE, which the caller
# sets, the forms signing in as 1Password's own tools do, OP_SERVICE_ACCOUNT_TOKEN or the caller's
# op), waits for its banner, takes the banner's exports, and with the agent-secrets the stack
# built runs, on a fresh name STORE_CHECK_<hex>, in order:
#
#   create (the value piped in), show, list --json, set, retag --tier human, delete, restore
#
# Each row passes when the form exits 0 and prints what it must: after a write, the line in which
# it reports the broker's answer to the reread of the name it asked for
# (POST /v1/secrets/NAME/reread): `broker: serving NAME from `, `broker: NAME is deleted in `
# after delete, or `broker: rereading NAME from ` where the broker could not be asked with a
# session and answered 202; on show, the secret; on list --json, the secret in its array. On a
# 1Password vault, which has no call that restores an item, the restore row passes on the form's
# refusal naming the 1Password app's Archive, and the run leaves the item there, archived. It asks
# no reread of its own: the broker holds rereads to a budget (secretstore.DefaultRereadBudget), and
# a second reread per write would spend it on the check rather than on the forms.
#
# --control vanished removes the secret behind the CLI's back after create (memory: from the
# memory secrets file the broker follows; gcp: `gcloud secrets delete`; onepassword: `op item
# delete`, which deletes the item for good), and the run must then fail on show: a check that
# still passes there reads no store at all.
#
# It prints one `PASS <row>` or `FAIL <row>: <why>` per form and then
# `store-forms-check: <n> rows ran, <m> passed`, and exits 0 only when all seven rows ran and
# passed; a stack that never printed its banner's exports runs no row and exits 1. It stops the
# stack when it ends.
#
# Needs bash and jq, what dev-broker.sh needs (go, a Postgres: DEV_BROKER_POSTGRES_URL or Docker),
# and, for --control vanished, gcloud on gcp and op on onepassword. dev-broker.sh, agent-secrets and
# op are taken from PATH where a test puts stand-ins there, else from this directory and the stack.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
usage="usage: store-forms-check.sh --store memory|gcp|onepassword [--control vanished]"

store=""
control=""
while [ "$#" -gt 0 ]; do
  case "$1" in
  --store)
    store="${2:-}"
    shift 2 || true
    ;;
  --control)
    control="${2:-}"
    shift 2 || true
    ;;
  *)
    echo "store-forms-check: unknown argument $1; $usage" >&2
    exit 2
    ;;
  esac
done
case "$store" in
memory | gcp | onepassword) ;;
*)
  echo "store-forms-check: --store must be memory, gcp or onepassword; $usage" >&2
  exit 2
  ;;
esac
if [ -n "$control" ] && [ "$control" != vanished ]; then
  echo "store-forms-check: --control takes vanished alone; $usage" >&2
  exit 2
fi
if [ "$store" = gcp ] && { [ -z "${DEV_BROKER_GCP_PREFIX:-}" ] || [ -z "${DEV_BROKER_GCP_KMS_KEY_NAME:-}" ]; }; then
  echo "store-forms-check: --store gcp needs DEV_BROKER_GCP_PREFIX and DEV_BROKER_GCP_KMS_KEY_NAME" >&2
  exit 2
fi
if [ "$store" = onepassword ] && { [ -z "${DEV_BROKER_ONEPASSWORD_VAULT:-}" ] || [ -z "${DEV_BROKER_ONEPASSWORD_TOKEN_FILE:-}" ]; }; then
  echo "store-forms-check: --store onepassword needs DEV_BROKER_ONEPASSWORD_VAULT and DEV_BROKER_ONEPASSWORD_TOKEN_FILE" >&2
  exit 2
fi
# The stack runs on the one store asked for: dev-broker.sh refuses the settings of two.
if [ "$store" != gcp ]; then
  unset DEV_BROKER_GCP_PREFIX DEV_BROKER_GCP_KMS_KEY_NAME
fi
if [ "$store" != onepassword ]; then
  unset DEV_BROKER_ONEPASSWORD_VAULT DEV_BROKER_ONEPASSWORD_TOKEN_FILE DEV_BROKER_ONEPASSWORD_DAILY_REQUESTS
fi

work="$(mktemp -d)"
broker_pid=""
# The stack runs in a session of its own (setsid), so it leads a process group whose id is the pid
# $! names, and cleanup stops the whole group with SIGTERM: dev-broker.sh's EXIT trap stops its
# processes and drops its database, and a go build it is still running is a process of its own
# that a signal to dev-broker.sh alone would leave running.
cleanup() {
  if [ -n "$broker_pid" ]; then
    kill -TERM -- "-$broker_pid" 2>/dev/null || true
    wait "$broker_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

ran=0
passed=0
pass() {
  ran=$((ran + 1))
  passed=$((passed + 1))
  echo "PASS $1"
}
fail() {
  ran=$((ran + 1))
  echo "FAIL $1: $2"
}
summary() {
  echo "store-forms-check: $ran rows ran, $passed passed"
  if [ "$ran" -eq 7 ] && [ "$passed" -eq 7 ]; then
    exit 0
  fi
  exit 1
}

dev_broker="$(command -v dev-broker.sh || echo "$SCRIPT_DIR/dev-broker.sh")"
setsid bash "$dev_broker" >"$work/broker.out" 2>"$work/broker.err" &
broker_pid=$!
# dev-broker.sh builds the broker and its clients first, which a cold build cache makes slow: the
# wait is 30 minutes, and ends at once when the stack exits.
ready=false
for _ in $(seq 1 3600); do
  if grep -q '^dev-broker: ready\.' "$work/broker.out" 2>/dev/null; then
    ready=true
    break
  fi
  if ! kill -0 "$broker_pid" 2>/dev/null; then
    break
  fi
  sleep 0.5
done
# The banner's exports, each `  export NAME=VALUE`; PATH's value names the stack's bin directory
# before the caller's own PATH.
while IFS= read -r line; do
  if [[ "$line" =~ ^\ \ export\ ([A-Z_][A-Z0-9_]*)=(.*)$ ]]; then
    name="${BASH_REMATCH[1]}"
    value="${BASH_REMATCH[2]}"
    if [ "$name" = PATH ]; then
      value="${value%:\$PATH}:$PATH"
    fi
    export "$name=$value"
  fi
done <"$work/broker.out"
if [ "$ready" != true ] || [ -z "${AGENT_SECRETS_URL:-}" ] || [ -z "${DEV_BROKER_DIR:-}" ]; then
  echo "store-forms-check: the stack printed no banner with AGENT_SECRETS_URL and DEV_BROKER_DIR, so no form ran" >&2
  tail -20 "$work/broker.err" >&2 || true
  summary
fi

name="STORE_CHECK_$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n' | tr '[:lower:]' '[:upper:]')"
owner="${AGENT_SECRETS_APPROVER:-ada@example.com}"

# form ROW ARGS...: runs agent-secrets secret ARGS with this row's stdin, keeping its output.
form() {
  local row="$1"
  shift
  set +e
  agent-secrets secret "$@" <"$work/$row.in" >"$work/$row.out" 2>"$work/$row.err"
  echo "$?" >"$work/$row.exit"
  set -e
}
said() { grep -qF -- "$2" "$work/$1.out"; }
# why ROW: the row's exit status and everything it printed, each on one line.
why() { printf 'exit %s, stdout %s, stderr %s' "$(cat "$work/$1.exit")" "$(tr '\n' '|' <"$work/$1.out")" "$(tr '\n' '|' <"$work/$1.err")"; }
# write_row ROW WANT ARGS...: a write form, which passes when it exits 0 and prints the broker's
# answer to its reread: WANT, or the line of a reread the broker took with no proof.
write_row() {
  local row="$1" want="$2"
  shift 2
  form "$row" "$@"
  if [ "$(cat "$work/$row.exit")" != 0 ]; then
    fail "$row" "$(why "$row")"
  elif ! said "$row" "$want" && ! said "$row" "broker: rereading $name from "; then
    fail "$row" "no reread line ($want): $(why "$row")"
  else
    pass "$row"
  fi
}

printf 'store-check-value-1\n' >"$work/create.in"
write_row create "broker: serving $name from " create "$name" --owner "$owner" --tier agent

if [ "$control" = vanished ]; then
  case "$store" in
  memory)
    file="$DEV_BROKER_DIR/memory-secrets.json"
    jq --arg id "example/agent-secrets/$(echo "$name" | tr '[:upper:]_' '[:lower:]-')" \
      '.secrets |= map(select(.name != $id))' "$file" >"$work/vanished.json"
    cat "$work/vanished.json" >"$file"
    ;;
  gcp)
    id="${DEV_BROKER_GCP_PREFIX##*/secrets/}$(echo "$name" | tr '[:upper:]_' '[:lower:]-')"
    project="${DEV_BROKER_GCP_PREFIX#projects/}"
    project="${project%%/*}"
    gcloud secrets delete "$id" --project "$project" --quiet >/dev/null
    ;;
  onepassword)
    op item delete "agent-secrets-$(echo "$name" | tr '[:upper:]_' '[:lower:]-')" --vault "$DEV_BROKER_ONEPASSWORD_VAULT" >/dev/null
    ;;
  esac
  echo "store-forms-check: control vanished: removed $name behind the CLI's back" >&2
fi

: >"$work/show.in"
form show show "$name"
if [ "$(cat "$work/show.exit")" = 0 ] && said show "$name"; then pass show; else fail show "$(why show)"; fi

: >"$work/list.in"
form list list --json
if [ "$(cat "$work/list.exit")" = 0 ] && jq -e --arg n "$name" 'any(.[]; .name == $n)' "$work/list.out" >/dev/null 2>&1; then
  pass list
else
  fail list "$name is not in the list: $(why list)"
fi

printf 'store-check-value-2\n' >"$work/set.in"
write_row set "broker: serving $name from " set "$name"

: >"$work/retag.in"
write_row retag "broker: serving $name from " retag "$name" --tier human

: >"$work/delete.in"
write_row delete "broker: $name is deleted in " delete "$name"

: >"$work/restore.in"
if [ "$store" = onepassword ]; then
  form restore restore "$name"
  if [ "$(cat "$work/restore.exit")" = 1 ] && grep -qF "from the 1Password app's Archive" "$work/restore.err"; then
    pass restore
  else
    fail restore "a 1Password vault has no restore, so the form must refuse naming the 1Password app's Archive: $(why restore)"
  fi
else
  write_row restore "broker: serving $name from " restore "$name"
fi

# The secret this run made is left deleted, so a run leaves nothing a broker serves.
: >"$work/cleanup.in"
form cleanup delete "$name" || true

summary
