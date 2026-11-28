#!/usr/bin/env bash
# packages/envoy/scripts/store-forms-check.sh --store memory|gcp [--control vanished]
#
# Runs every secret form of agent-secrets against a running broker on one store, the surface check
# of the CLI and the broker reading the same store. It starts dev-broker.sh in that store's mode
# (memory: the default; gcp: DEV_BROKER_GCP_PREFIX and DEV_BROKER_GCP_KMS_KEY_NAME, which the
# caller sets, under whatever Google credential the caller's environment gives agent-secrets),
# waits for its banner, takes the banner's exports, and with the agent-secrets the stack built runs,
# on a fresh name STORE_CHECK_<hex>, in order:
#
#   create (the value piped in), show, list --json, set, retag --tier human, delete, restore
#
# Each row passes when the form exits 0 and prints what it must; on show, the secret; on list
# --json, the secret in its array. A write's row reads its effect back before it passes: `secret
# show NAME --json` must show the tier retag set, the deletion delete scheduled and restore cleared
# (a restore passes only after a delete that was seen), and the store itself, read without the CLI
# (the memory secrets file, or Secret Manager through gcloud), must hold the value create and set
# wrote. A write's row also needs the line in which the form reports the broker's answer to the
# reread of the name it asked for (POST /v1/secrets/NAME/reread), and its PASS line says which it
# was: `broker: serving NAME from `, or `broker: NAME is deleted in ` after delete, is the broker
# answering the reread; `broker: rereading NAME from `, where the broker could not be asked with a
# session and answered 202, is the broker accepting the reread only, and says nothing of what it
# serves. That the broker serves the value a reread found is the broker's own behaviour, which its
# own tests hold; this check observes the store. It asks no reread of its own: the broker holds
# rereads to a budget (secretstore.DefaultRereadBudget), and a second reread per write would spend
# it on the check rather than on the forms.
#
# --control vanished removes the secret behind the CLI's back after create (memory: from the
# memory secrets file the broker follows; gcp: `gcloud secrets delete`), and the run must then fail
# on show: a check that still passes there reads no store at all.
#
# It prints one `PASS <row>` (for a write, `PASS <row>: <what it observed>`) or `FAIL <row>: <why>`
# per form and then
# `store-forms-check: <n> rows ran, <m> passed`, and exits 0 only when all seven rows ran and
# passed; a stack that never printed its banner's exports runs no row and exits 1. It stops the
# stack when it ends.
#
# Needs bash and jq, what dev-broker.sh needs (go, a Postgres: DEV_BROKER_POSTGRES_URL or Docker),
# and, for --store gcp, gcloud. dev-broker.sh and agent-secrets are taken from
# PATH where a test puts stand-ins there, else from this directory and the stack.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
usage="usage: store-forms-check.sh --store memory|gcp [--control vanished]"

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
memory | gcp) ;;
*)
  echo "store-forms-check: --store must be memory or gcp; $usage" >&2
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
if [ "$store" = memory ]; then
  unset DEV_BROKER_GCP_PREFIX DEV_BROKER_GCP_KMS_KEY_NAME
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
  echo "PASS $1${2:+: $2}"
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
# shown ROW: the secret as `secret show NAME --json` answers it after ROW, kept in ROW.shown, which
# is empty where show failed.
shown() {
  set +e
  agent-secrets secret show "$name" --json </dev/null >"$work/$1.shown" 2>"$work/$1.shown.err"
  local code=$?
  set -e
  if [ "$code" != 0 ]; then : >"$work/$1.shown"; fi
}
# stored_value ROW: the value the store itself holds for the secret ROW.shown names, read without
# the CLI: the memory secrets file the broker follows, or Secret Manager's latest version.
stored_value() {
  local secret_name
  secret_name="$(jq -r '.secret_name // empty' "$work/$1.shown" 2>/dev/null || true)"
  if [ -z "$secret_name" ]; then return 0; fi
  case "$store" in
  memory)
    jq -r --arg id "$secret_name" '.secrets[] | select(.name == $id) | .value' "$DEV_BROKER_DIR/memory-secrets.json" 2>/dev/null || true
    ;;
  gcp)
    local project="${DEV_BROKER_GCP_PREFIX#projects/}"
    gcloud secrets versions access latest --secret "${secret_name##*/secrets/}" --project "${project%%/*}" 2>/dev/null || true
    ;;
  esac
}
# write_row ROW WANT EFFECT VALUE ARGS...: a write form, which passes when it exits 0, prints the
# broker's answer to its reread (WANT) or the line of a reread the broker only accepted, and its
# effect reads back: `secret show --json` answers true to the jq filter EFFECT, and, when VALUE is
# set, the store itself holds VALUE. Its PASS line says which of the two reread lines it saw.
write_row() {
  local row="$1" want="$2" effect="$3" value="$4" reread
  shift 4
  form "$row" "$@"
  if [ "$(cat "$work/$row.exit")" != 0 ]; then
    fail "$row" "$(why "$row")"
    return
  fi
  if said "$row" "$want"; then
    reread="the broker answered its reread"
  elif said "$row" "broker: rereading $name from "; then
    reread="the broker accepted its reread (HTTP 202), which says nothing of what it serves"
  else
    fail "$row" "no reread line ($want): $(why "$row")"
    return
  fi
  shown "$row"
  if ! jq -e "$effect" "$work/$row.shown" >/dev/null 2>&1; then
    fail "$row" "show does not read the write back ($effect): $(tr '\n' ' ' <"$work/$row.shown")$(tr '\n' '|' <"$work/$row.shown.err")"
    return
  fi
  if [ -n "$value" ] && [ "$(stored_value "$row")" != "$value" ]; then
    fail "$row" "the store does not hold the value the form wrote"
    return
  fi
  pass "$row" "$reread; the store holds the write"
}

printf 'store-check-value-1\n' >"$work/create.in"
write_row create "broker: serving $name from " '.tier == "agent" and .has_value == true' store-check-value-1 \
  create "$name" --owner "$owner" --tier agent

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
write_row set "broker: serving $name from " '.has_value == true' store-check-value-2 set "$name"

: >"$work/retag.in"
write_row retag "broker: serving $name from " '.tier == "human"' "" retag "$name" --tier human

: >"$work/delete.in"
write_row delete "broker: $name is deleted in " '.deleted != null' "" delete "$name"

# A restore can be seen only after a deletion was: with none, `deleted` reads empty either way.
: >"$work/restore.in"
if jq -e '.deleted != null' "$work/delete.shown" >/dev/null 2>&1; then
  write_row restore "broker: serving $name from " '.deleted == null' "" restore "$name"
else
  fail restore "no deletion was seen before it, so a restore would show nothing"
fi

# The secret this run made is left deleted, so a run leaves nothing a broker serves.
: >"$work/cleanup.in"
form cleanup delete "$name" || true

summary
