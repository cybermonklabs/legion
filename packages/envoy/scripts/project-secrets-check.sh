#!/usr/bin/env bash
# packages/envoy/scripts/project-secrets-check.sh
# dispatch://INFRA-3862's R11, driven on the binaries production runs: a private project's secret
# goes only to a credential that names that project.
#
# It starts one scripts/dev-stack.sh run with --project-access (the real broker, the real Dispatch,
# the real dev-oidc-issuer, the real agent-secrets and its helper, the airlock's proxy and its
# stands), makes PRIV private with carol as its owner and bob on its access list, writes PRIV_KEY
# (tagged dispatch-project=PRIV, shared, agent tier) and PUB_KEY (untagged), and then drives one
# caller at a time. For each it runs `agent-secrets request PRIV_KEY` and `agent-secrets request
# PUB_KEY` and one trade (packages/cipher-client/scripts/print-trade.ts, which prints the
# introspection's project_scope and projects), and prints one PASS or FAIL line per answer, naming
# the caller and the secret:
#   * PRIV's pod, the production path: the pod of legion-priv-worker, whose exchange entry maps it
#     to PRIV for carol. Its trade of its exchange token is project scope "named" with ["PRIV"];
#     it enrols itself (agent-secrets enroll --pod --service legion-priv) and is granted both.
#   * carol's session listing PRIV, the refusal case: a session on the stack's machine, which no
#     machine registration names, started with AGENT_SECRETS_PROJECTS=PRIV. It reads avenue F's
#     machine check (see the row).
#   * alice's session listing PRIV: her person cannot see PRIV, so her trade and her session's
#     enrollment are both refused, with or without F's check.
#   * carol's session naming nothing: PUB_KEY granted, PRIV_KEY denied project-not-named.
#   * the machine's own session: PUB_KEY as today (granted: shared and agent tier), PRIV_KEY denied.
#   * PRIV's pod behind the airlock: the stack's profiles file binds PRIV_KEY to the airlock's bound host,
#     so the pod's grant of it is spent through the proxy. A request through the proxy is served
#     by the bound host (its own answer is read, not the stands' records: the stands run in a
#     container); then carol, the person of the pod's exchange entry, is taken off PRIV, and the
#     next request's CONNECT is refused 407.
# A person's session runs through an agent-secrets-helper of that person's own box, in which the
# person signed in with `agent-secrets login` at the stack's issuer. The session's own calls read
# no sign-in, so they go to the helper, which enrols them under the person's sign-in naming the
# projects the session listed.
#
# The negative control, --control untagged, writes PRIV_KEY with no dispatch-project tag. Every
# caller then gets it, and the run fails naming the first caller that got it although it should
# have been refused. It fails for the reason the probe would be blind: a secret the rule cannot see
# is exactly an untagged one.
#
# Live mode, --broker-url URL --secret NAME: no stack and no write, the session half alone,
# against a running deployment. It needs, in its environment, the pod's exchange token
# (AGENT_SECRETS_MACHINE_TOKEN_FILE), the pod's enrolled key directory (AGENT_SECRETS_KEY_DIR), the
# operator's helper socket with its machine login (AGENT_SECRETS_HELPER_SOCK), and an account token
# the broker lists in BROKER_API_ACCOUNTS (AGENT_SECRETS_ACCOUNT_TOKEN_FILE), the only caller the
# broker lets read a token's introspection. It runs the pod's trade, which names the project; the
# pod's request for the secret; and the operator's session listing that project, which reads F's
# check as the stack's row does. It takes no Dispatch URL: nothing of that half reads one.
#
# It exits 0 only when every row ran and passed. The last line is "project-secrets-check: <n> rows
# ran, <m> passed"; a run whose rows matched nothing (a stack whose banner lacks PRIV's exports, a
# setup step that failed) says 0 and exits 1.
#
# It needs what dev-stack.sh needs (go unless --binaries, psql, curl, jq, python3, and docker for the
# airlock's stands), bun with this repository's packages installed, and a Postgres server to create
# this run's databases on: --database-url, else LEGION_TEST_PG_DSN, else DATABASE_URL.
#
#   LEGION_TEST_PG_DSN=… bash scripts/project-secrets-check.sh
#   LEGION_TEST_PG_DSN=… bash scripts/project-secrets-check.sh --control untagged
#   bash scripts/project-secrets-check.sh --broker-url https://secrets… --secret PROBE_PRIV
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
PRINT_TRADE="$REPO_ROOT/packages/cipher-client/scripts/print-trade.ts"

# The two people of the rows besides PRIV's owner: bob, on PRIV's access list, and alice, who
# cannot see PRIV. Both are in the stack's sign-in domain (BROKER_SIGNIN_DOMAIN, example.com).
BOB="bob@example.com"
ALICE="alice@example.com"
PUB_SECRET="PUB_KEY"
# How long the airlock row waits after taking carol off PRIV: the broker keeps each K3 answer for
# projectaccess.CacheTTL (30 s, internal/cipher/projectaccess/projectaccess.go), so a refusal
# before then would read a cached answer. Five seconds over it.
K3_CACHE_WAIT=35
# How long a person's session waits for its helper to enrol it.
REGISTER_WAIT=20
# Avenue F's refusal of a session listing a private project from a machine not registered to it
# (#t-f-9, its codes KF2, dispatch://INFRA-3894/artifact/plan-md): the trade's hint opens with the
# first, the enrollment is refused 403 with the second.
F_TRADE_REFUSAL="machine_not_registered"
F_ENROLL_REFUSAL="MACHINE_NOT_REGISTERED"
# The number of rows each mode runs; a run that ran fewer is not a pass.
STACK_ROWS=16
LIVE_ROWS=3

usage() {
  cat <<'EOF'
usage: project-secrets-check.sh [--database-url <postgres url>] [--binaries <dir>]
                                [--control untagged]
       project-secrets-check.sh --broker-url <url> --secret <NAME>

Drive dispatch://INFRA-3862's R11 on the binaries production runs: a private project's secret goes
only to a credential that names the project. With no --broker-url it starts one dev-stack.sh run
of its own and drives every caller against it.

  --database-url   the Postgres server dev-stack.sh creates this run's databases on
  --binaries       prebuilt binaries, as dev-stack.sh --project-access takes them, so
                   dev-airlock-stands among them
  --control untagged
                   write PRIV_KEY with no dispatch-project tag: every caller then gets it and this
                   run fails, naming the first caller that should have been refused
  --broker-url, --secret
                   live mode: drive the session half against a running deployment, writing
                   nothing. Its environment names the pod's exchange token
                   (AGENT_SECRETS_MACHINE_TOKEN_FILE), the pod's key dir (AGENT_SECRETS_KEY_DIR),
                   the operator's helper (AGENT_SECRETS_HELPER_SOCK) and an account token the
                   broker lets introspect (AGENT_SECRETS_ACCOUNT_TOKEN_FILE)
EOF
}

DATABASE_URL_OPTION=""
BINARIES_OPTION=""
CONTROL=""
LIVE_BROKER_URL=""
LIVE_SECRET=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --database-url) DATABASE_URL_OPTION="${2:-}"; shift 2 ;;
    --database-url=*) DATABASE_URL_OPTION="${1#--database-url=}"; shift ;;
    --binaries) BINARIES_OPTION="${2:-}"; shift 2 ;;
    --binaries=*) BINARIES_OPTION="${1#--binaries=}"; shift ;;
    --control)
      CONTROL="${2:-}"
      if [ "$CONTROL" != untagged ]; then
        echo "project-secrets-check: --control takes untagged, not '${CONTROL}'" >&2
        exit 2
      fi
      shift 2
      ;;
    --broker-url) LIVE_BROKER_URL="${2:-}"; shift 2 ;;
    --secret) LIVE_SECRET="${2:-}"; shift 2 ;;
    --help | -h) usage; exit 0 ;;
    *)
      echo "project-secrets-check: unexpected argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

LIVE=false
if [ -n "$LIVE_BROKER_URL$LIVE_SECRET" ]; then
  LIVE=true
  if [ -z "$LIVE_BROKER_URL" ] || [ -z "$LIVE_SECRET" ]; then
    echo "project-secrets-check: live mode needs --broker-url and --secret together" >&2
    exit 2
  fi
  if [ -n "$CONTROL" ]; then
    echo "project-secrets-check: --control writes a secret, which live mode never does" >&2
    exit 2
  fi
  if [ -n "$DATABASE_URL_OPTION$BINARIES_OPTION" ]; then
    echo "project-secrets-check: live mode starts no stack, so --database-url and --binaries are not its" >&2
    exit 2
  fi
  missing=()
  for name in AGENT_SECRETS_MACHINE_TOKEN_FILE AGENT_SECRETS_KEY_DIR AGENT_SECRETS_HELPER_SOCK; do
    [ -n "${!name:-}" ] || missing+=("$name")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    echo "project-secrets-check: live mode reads the pod and the operator from its environment, and it names no ${missing[*]}" >&2
    exit 2
  fi
fi

tools=(bun jq curl)
[ "$LIVE" = true ] || tools+=(psql)
for tool in "${tools[@]}"; do
  command -v "$tool" >/dev/null || {
    echo "project-secrets-check: $tool is required on PATH" >&2
    exit 1
  }
done
[ -f "$PRINT_TRADE" ] || {
  echo "project-secrets-check: the one-shot trade $PRINT_TRADE is missing" >&2
  exit 1
}

# --- The rows: each line names the caller and the secret, so a reader of the output alone knows
# which credential was answered what. ---
rows_run=0
rows_passed=0
# The first caller that got PRIV_KEY although its row expected a refusal: the control's answer.
FIRST_LEAK=""
# The private project's secret, read off the banner (or --secret in live mode); named here so a run
# that stops before the banner is read can still say which secret its control was about.
PRIV_SECRET="PRIV_KEY"
row_pass() {
  rows_run=$((rows_run + 1))
  rows_passed=$((rows_passed + 1))
  printf 'PASS: %s\n' "$*"
}
row_fail() {
  rows_run=$((rows_run + 1))
  printf 'FAIL: %s\n' "$*"
}
note() { printf '      %s\n' "$*"; }

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/project-secrets-check.XXXXXX")"
mkdir -p "$WORK_DIR/calls"
# Every process this run starts in the background, stopped by pid, the last started first.
PIDS=()
# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  local rc=$? i
  trap '' INT TERM
  for ((i = ${#PIDS[@]} - 1; i >= 0; i--)); do
    kill -TERM "${PIDS[$i]}" 2>/dev/null || true
    wait "${PIDS[$i]}" 2>/dev/null || true
  done
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# finish prints the count of rows and leaves: 0 only when every row of this mode ran and passed.
# The count is the last line.
finish() { # <rows this mode runs>
  local status=0
  if [ "$CONTROL" = untagged ]; then
    status=1
    if [ -n "$FIRST_LEAK" ]; then
      printf 'control: the first caller that got %s although it should have been refused: %s\n' "$PRIV_SECRET" "$FIRST_LEAK"
    else
      printf 'control: UNKNOWN: no caller that should have been refused got %s, so this run did not show that the rule can fail\n' "$PRIV_SECRET"
    fi
  elif [ "$rows_run" -eq 0 ]; then
    echo "project-secrets-check: no row ran, so nothing was shown"
    status=1
  elif [ "$rows_run" -ne "$1" ]; then
    echo "project-secrets-check: $rows_run of the $1 rows ran"
    status=1
  elif [ "$rows_passed" -ne "$rows_run" ]; then
    status=1
  fi
  note "this run's calls and logs: $WORK_DIR (kept)"
  printf 'project-secrets-check: %d rows ran, %d passed\n' "$rows_run" "$rows_passed"
  exit "$status"
}
setup_fail() {
  printf 'project-secrets-check: setup failed: %s\n' "$*"
  finish "$STACK_ROWS"
}

# capture runs one command, its standard output in OUT, its standard error in ERR and its exit
# status in RC, each call under a name of its own in this run's directory.
OUT="" ERR="" RC=0
capture() { # <name> <command...>
  OUT="$WORK_DIR/calls/$1.out" ERR="$WORK_DIR/calls/$1.err"
  shift
  RC=0
  "$@" >"$OUT" 2>"$ERR" || RC=$?
}
said() { tr '\n' ' ' <"$ERR" | sed 's/ *$//'; }

# --- Trades. print-trade.ts prints {"active", "person", "service", "project_scope", "projects"},
# the broker's introspection of the token it got. ---
trade() { # <name> <env assignments...>
  local name="$1"
  shift
  capture "$name" env -i "$@" bun "$PRINT_TRADE"
}
trade_field() { jq -r "$1" "$OUT" 2>/dev/null || true; }
describe_trade() { printf 'exit %s, answer %s, stderr %s' "$RC" "$(tr -d '\n' <"$OUT")" "$(said)"; }
# expect_trade passes a trade answered with projects (as compact JSON), under scope when one is
# named.
expect_trade() { # <caller> <scope or ""> <projects JSON>
  local scope projects shown
  scope="$(trade_field '.project_scope // ""')"
  projects="$(trade_field '.projects // [] | tojson')"
  shown="$projects"
  [ "$projects" != "[]" ] || shown="no project"
  if [ "$RC" = 0 ] && [ "$(trade_field '.active')" = true ] && { [ -z "$2" ] || [ "$scope" = "$2" ]; } && [ "$projects" = "$3" ]; then
    row_pass "$1: its trade: project scope $scope, $shown"
  else
    row_fail "$1: its trade: want project scope ${2:-any} with $3; got $(describe_trade)"
  fi
}

# --- Requests. agent-secrets request --json prints the broker's answer: state, grant_id, detail.
# A denial exits 77. ---
request_field() { jq -r "$1" "$OUT" 2>/dev/null || true; }
describe_request() {
  printf 'exit %s, state %s, detail %s, stderr %s' "$RC" "$(request_field '.state // "none"')" \
    "$(request_field '.detail // "none"')" "$(said)"
}
expect_granted() { # <caller> <secret> [<why>]
  if [ "$RC" = 0 ] && [ "$(request_field '.state')" = granted ]; then
    row_pass "$1: $2: granted${3:+ $3}"
  else
    row_fail "$1: $2: want granted${3:+ $3}; got $(describe_request)"
  fi
}
# noted_leak keeps the first caller that got PRIV_KEY although its row expected a refusal.
noted_leak() { # <caller> <secret>
  if [ "$2" = "$PRIV_SECRET" ] && [ "$(request_field '.state')" = granted ] && [ -z "$FIRST_LEAK" ]; then
    FIRST_LEAK="$1"
  fi
}
expect_not_named() { # <caller> <secret>
  if [ "$RC" = 77 ] && [ "$(request_field '.state')" = denied ] &&
    [[ "$(request_field '.detail // ""')" == *"$2: project-not-named"* ]]; then
    row_pass "$1: $2: denied project-not-named"
  else
    noted_leak "$1" "$2"
    row_fail "$1: $2: want denied project-not-named; got $(describe_request)"
  fi
}
expect_f_refusal() { # <caller> <secret>
  if [ "$(request_field '.state')" != granted ] && grep -qF -- "$F_ENROLL_REFUSAL" "$ERR"; then
    row_pass "$1: $2: refused at enrollment by F's check ($F_ENROLL_REFUSAL)"
  else
    noted_leak "$1" "$2"
    row_fail "$1: $2: want refused at enrollment by F's check ($F_ENROLL_REFUSAL); got $(describe_request)"
  fi
}

# expect_listing_trade is the refusal case's trade, which reads avenue F's machine check, and sets
# BRANCH to the branch that ran: "f" when F refused it, "before-f" otherwise.
#
# Before F's check exists (#t-f-9 of dispatch://INFRA-3894/artifact/plan-md), the broker mints, and
# the trade passes only on D1's answer: a token of the person scope carrying [project] for a person
# who may see it. When F's check is in the broker under test, F's task deletes this before-F branch,
# so the row expects F's refusal alone. Any other answer fails: another refusal, a token with
# another list, or a token for another person.
BRANCH=""
expect_listing_trade() { # <caller> <project> <person>
  BRANCH=before-f
  if [ "$RC" = 3 ] && grep -qF -- "$F_TRADE_REFUSAL" "$ERR"; then
    BRANCH=f
    row_pass "$1: its trade: refused by F's check ($F_TRADE_REFUSAL)"
  elif [ "$RC" = 0 ] && [ "$(trade_field '.active')" = true ] && [ "$(trade_field '.project_scope')" = person ] &&
    [ "$(trade_field '.projects | tojson')" = "[\"$2\"]" ] && [ "$(trade_field '.person')" = "$3" ]; then
    row_pass "$1: its trade: before F's check: project scope person, [\"$2\"] (D1's rule)"
  else
    row_fail "$1: its trade: want F's refusal ($F_TRADE_REFUSAL) or, before F's check, project scope person with [\"$2\"] for $3; got $(describe_trade)"
  fi
  note "the refusal case ran the ${BRANCH} branch"
}

# ===================================== live mode =====================================
if [ "$LIVE" = true ]; then
  BROKER_URL="$LIVE_BROKER_URL"
  PRIV_SECRET="$LIVE_SECRET"
  note "live mode against the broker $BROKER_URL, secret $PRIV_SECRET"
  mkdir -p "$WORK_DIR/no-signins" "$WORK_DIR/no-keys"
  live_env=(PATH="$PATH" HOME="$HOME" AGENT_SECRETS_URL="$BROKER_URL" AGENT_SECRETS_SIGNIN_DIR="$WORK_DIR/no-signins")
  [ -z "${XDG_RUNTIME_DIR:-}" ] || live_env+=(XDG_RUNTIME_DIR="$XDG_RUNTIME_DIR")
  pod_env=("${live_env[@]}" AGENT_SECRETS_KEY_DIR="$AGENT_SECRETS_KEY_DIR" AGENT_SECRETS_HELPER_SOCK=)
  POD="the pod"
  PROJECT=""
  if [ -z "${AGENT_SECRETS_ACCOUNT_TOKEN_FILE:-}" ]; then
    row_fail "$POD: its trade: UNKNOWN: cannot read the introspection: AGENT_SECRETS_ACCOUNT_TOKEN_FILE names no account token the broker lists in BROKER_API_ACCOUNTS"
  else
    trade_env=("${pod_env[@]}" AGENT_SECRETS_MACHINE_IDENTITY=kubernetes
      AGENT_SECRETS_MACHINE_TOKEN_FILE="$AGENT_SECRETS_MACHINE_TOKEN_FILE"
      AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$AGENT_SECRETS_ACCOUNT_TOKEN_FILE")
    [ -z "${AGENT_SECRETS_SESSION:-}" ] || trade_env+=(AGENT_SECRETS_SESSION="$AGENT_SECRETS_SESSION")
    trade live-pod-trade "${trade_env[@]}"
    if [ "$RC" = 0 ] && [ "$(trade_field '.project_scope')" = named ] && [ "$(trade_field '.projects | length')" = 1 ]; then
      PROJECT="$(trade_field '.projects[0]')"
      row_pass "$POD: its trade: project scope named, [\"$PROJECT\"]"
    else
      row_fail "$POD: its trade: want project scope named with one project; got $(describe_trade)"
    fi
  fi
  capture live-pod-request env -i "${pod_env[@]}" agent-secrets request "$PRIV_SECRET" --json
  expect_granted "$POD" "$PRIV_SECRET"
  OPERATOR="the operator's machine session listing ${PROJECT:-the project of the pod}"
  if [ -z "$PROJECT" ]; then
    row_fail "$OPERATOR: $PRIV_SECRET: UNKNOWN: no project to list, since the pod's introspection was not read"
  else
    capture live-operator-request env -i "${live_env[@]}" AGENT_SECRETS_HELPER_SOCK="$AGENT_SECRETS_HELPER_SOCK" \
      AGENT_SECRETS_KEY_DIR="$WORK_DIR/no-keys" AGENT_SECRETS_PROJECTS="$PROJECT" \
      agent-secrets register --wait "$REGISTER_WAIT" --exec -- agent-secrets request "$PRIV_SECRET" --json
    if grep -qF -- "$F_ENROLL_REFUSAL" "$ERR"; then
      expect_f_refusal "$OPERATOR" "$PRIV_SECRET"
    else
      expect_granted "$OPERATOR" "$PRIV_SECRET" "(before F's check)"
    fi
  fi
  finish "$LIVE_ROWS"
fi

# ===================================== the stack =====================================
STACK_LOG="$WORK_DIR/dev-stack.log"
start_stack() {
  : >"$STACK_LOG"
  local -a options=(--project-access)
  [ -z "$DATABASE_URL_OPTION" ] || options+=(--database-url "$DATABASE_URL_OPTION")
  [ -z "$BINARIES_OPTION" ] || options+=(--binaries "$BINARIES_OPTION")
  bash "$SCRIPT_DIR/dev-stack.sh" "${options[@]}" >"$STACK_LOG" 2>&1 &
  PIDS+=("$!")
  local waited=0
  # A first run builds every binary, so the wait is long: 20 minutes.
  while [ "$waited" -lt 2400 ]; do
    grep -q 'dev-stack: ready\.' "$STACK_LOG" 2>/dev/null && return 0
    kill -0 "${PIDS[0]}" 2>/dev/null || break
    sleep 0.5
    waited=$((waited + 1))
  done
  echo "project-secrets-check: the stack never reached ready; its log:"
  cat "$STACK_LOG"
  setup_fail "dev-stack.sh --project-access did not start"
}

# exported reads one "export NAME=..." line of the stack's banner, as a person copies it.
exported() { sed -n "s/^ *export $1=//p" "$STACK_LOG" | head -n1; }

start_stack
declare -A banner=()
unread=()
for name in AGENT_SECRETS_URL AGENT_SECRETS_ACCOUNT_TOKEN_FILE DISPATCH_URL DEV_STACK_DIR DEV_STACK_ISSUER_URL \
  DEV_STACK_METADATA_SCRIPT AGENT_SECRETS_MACHINE_AUDIENCE AGENT_SECRETS_SIGNIN_ISSUER \
  AGENT_SECRETS_SIGNIN_CLIENT_ID AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE DEV_STACK_PROJECT_ACCESS \
  DEV_STACK_PRIV_PROJECT DEV_STACK_PRIV_OWNER DEV_STACK_PRIV_SERVICE DEV_STACK_PRIV_SESSION \
  DEV_STACK_PRIV_EXCHANGE_TOKEN_FILE DEV_STACK_PRIV_POD_TOKEN_FILE DEV_STACK_PRIV_SECRET \
  DEV_STACK_AIRLOCK_PROXY_URL DEV_STACK_AIRLOCK_BOUND_URL DEV_STACK_AIRLOCK_CA_CERT_FILE; do
  banner[$name]="$(exported "$name")"
  [ -n "${banner[$name]}" ] || unread+=("$name")
done
for name in "${unread[@]}"; do
  echo "project-secrets-check: UNKNOWN: the stack's banner names no $name"
done
[ "${#unread[@]}" -eq 0 ] || setup_fail "the banner of $STACK_LOG is not that of dev-stack.sh --project-access"
[ "${banner[DEV_STACK_PROJECT_ACCESS]}" = true ] || setup_fail "the stack runs without --project-access"

BROKER_URL="${banner[AGENT_SECRETS_URL]}"
ACCOUNT_TOKEN_FILE="${banner[AGENT_SECRETS_ACCOUNT_TOKEN_FILE]}"
STACK_DIR="${banner[DEV_STACK_DIR]}"
ISSUER_URL="${banner[DEV_STACK_ISSUER_URL]}"
METADATA_SCRIPT="${banner[DEV_STACK_METADATA_SCRIPT]}"
MACHINE_AUDIENCE="${banner[AGENT_SECRETS_MACHINE_AUDIENCE]}"
PRIV_PROJECT="${banner[DEV_STACK_PRIV_PROJECT]}"
PRIV_OWNER="${banner[DEV_STACK_PRIV_OWNER]}"
PRIV_SERVICE="${banner[DEV_STACK_PRIV_SERVICE]}"
PRIV_SESSION="${banner[DEV_STACK_PRIV_SESSION]}"
PRIV_EXCHANGE_TOKEN_FILE="${banner[DEV_STACK_PRIV_EXCHANGE_TOKEN_FILE]}"
PRIV_POD_TOKEN_FILE="${banner[DEV_STACK_PRIV_POD_TOKEN_FILE]}"
PRIV_SECRET="${banner[DEV_STACK_PRIV_SECRET]}"
AIRLOCK_PROXY_URL="${banner[DEV_STACK_AIRLOCK_PROXY_URL]}"
AIRLOCK_BOUND_URL="${banner[DEV_STACK_AIRLOCK_BOUND_URL]}"
AIRLOCK_CA_CERT_FILE="${banner[DEV_STACK_AIRLOCK_CA_CERT_FILE]}"
BIN_DIR="$STACK_DIR/bin"
FAKE_SECRETS_FILE="$STACK_DIR/fake-secrets.json"
signin_client_env=(AGENT_SECRETS_SIGNIN_ISSUER="${banner[AGENT_SECRETS_SIGNIN_ISSUER]}"
  AGENT_SECRETS_SIGNIN_CLIENT_ID="${banner[AGENT_SECRETS_SIGNIN_CLIENT_ID]}"
  AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE="${banner[AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE]}")
note "the stack is ready: $STACK_DIR"
note "broker $BROKER_URL, Dispatch ${banner[DISPATCH_URL]}, airlock proxy $AIRLOCK_PROXY_URL"

# Dispatch's database, for the two stand-in writes below: the server this run's databases were
# created on, as dev-stack.sh reads it, and the database name its banner prints.
ADMIN_URL="${DATABASE_URL_OPTION:-${LEGION_TEST_PG_DSN:-${DATABASE_URL:-}}}"
DISPATCH_DB="$(grep -o 'dev_stack_dispatch_[a-z0-9]*' "$STACK_LOG" | head -n1 || true)"
[ -n "$DISPATCH_DB" ] || setup_fail "the stack's banner names no Dispatch database"
[[ "$ADMIN_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]] || setup_fail "the Postgres URL is not postgres://<user>@<host>:<port>/<database>"
DISPATCH_DATABASE_URL="${BASH_REMATCH[1]}/${DISPATCH_DB}${BASH_REMATCH[3]:-}"
dispatch_psql() { psql "$DISPATCH_DATABASE_URL" -v ON_ERROR_STOP=1 -q -At -F '|' "$@"; }
priv_row() {
  dispatch_psql -c "select p.visibility, p.owner, coalesce((select string_agg(a.principal, ' ' order by a.principal) from project_access a where a.project_key = p.key), '') from projects p where p.key = '$PRIV_PROJECT';" 2>&1 || true
}

# --- The two stand-ins for avenue A's PATCH /api/v1/projects/{key}/access, and the only writes
# this run makes to Dispatch's tables. That route answers only once t-a-5
# (DISPATCH_ACCESS_ACCOUNTS at boot) and t-a-20 (DISPATCH_PRIVATE_PROJECTS and the bus write) are
# on the branch under test; until then it answers 409 PRIVATE_PROJECTS_OFF. So each writes the
# rows the route would, which are the one thing K3's answer and the read rule both read, and its
# caller reads them back and says so. When the route answers, each body becomes that one call. ---
# standin_patch_access_private stands in for PATCH /api/v1/projects/PRIV/access
# {"visibility": "private"} and PUT /api/v1/projects/PRIV/access/people/bob: PRIV private, carol
# its owner, bob on its access list.
standin_patch_access_private() {
  dispatch_psql \
    -c "insert into projects (key, name, default_assignee, owner, visibility) values ('$PRIV_PROJECT', '$PRIV_PROJECT', '$PRIV_OWNER', '$PRIV_OWNER', 'private') on conflict (key) do update set owner = excluded.owner, visibility = excluded.visibility;" \
    -c "insert into project_access (project_key, principal_kind, principal, added_by) values ('$PRIV_PROJECT', 'person', '$BOB', jsonb_build_object('kind', 'user', 'id', '$PRIV_OWNER')) on conflict do nothing;"
}
# standin_patch_access_owner stands in for PATCH /api/v1/projects/PRIV/access {"owner": bob}:
# carol giving PRIV to bob, which takes her off it. The route needs bob signed in to Dispatch.
standin_patch_access_owner() {
  dispatch_psql \
    -c "update projects set owner = '$BOB' where key = '$PRIV_PROJECT';" \
    -c "delete from project_access where project_key = '$PRIV_PROJECT' and principal_kind = 'person' and principal = '$PRIV_OWNER';"
}

# --- The secrets: PRIV_KEY of the private project, shared and agent tier, so owner and tier alone
# would give it to every session at once and only the project decides; PUB_KEY with no project at
# all, which must go on being served to everyone its owner and tier allow. The control run writes
# PRIV_KEY with no project tag, exactly a secret the rule cannot see. The broker reads both again
# at once (its reread route). ---
priv_tags="$(jq -cn --arg project "$PRIV_PROJECT" --arg control "$CONTROL" \
  '{owner: "shared", tier: "agent"} + (if $control == "untagged" then {} else {"dispatch-project": $project} end)')"
store_name() { printf 'example/agent-secrets/%s' "$(printf '%s' "$1" | tr '[:upper:]_' '[:lower:]-')"; }
jq --arg priv "$(store_name "$PRIV_SECRET")" --arg pub "$(store_name "$PUB_SECRET")" --argjson tags "$priv_tags" '
  .secrets |= map(select(.name != $priv and .name != $pub))
  | .secrets += [
      {name: $priv, kms_key_id: "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab", tags: $tags, value: "priv-key-value"},
      {name: $pub, kms_key_id: "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab", tags: {owner: "shared", tier: "agent"}, value: "pub-key-value"}
    ]' "$FAKE_SECRETS_FILE" >"$WORK_DIR/fake-secrets.json" || setup_fail "could not rewrite $FAKE_SECRETS_FILE"
cat "$WORK_DIR/fake-secrets.json" >"$FAKE_SECRETS_FILE"
for secret in "$PRIV_SECRET" "$PUB_SECRET"; do
  curl -sf -X POST "$BROKER_URL/v1/secrets/$secret/reread" >/dev/null || setup_fail "the broker would not reread $secret"
done
note "wrote $PRIV_SECRET (tags $priv_tags) and $PUB_SECRET (no project)"

# --- PRIV, private, with carol its owner and bob on its access list, through its stand-in. ---
standin_patch_access_private >"$WORK_DIR/calls/priv-private.out" 2>&1 ||
  setup_fail "could not make $PRIV_PROJECT private, the stand-in for PATCH /api/v1/projects/$PRIV_PROJECT/access (avenue A's t-a-5 and t-a-20): $(tr '\n' ' ' <"$WORK_DIR/calls/priv-private.out")"
read_back="$(priv_row)"
[ "$read_back" = "private|$PRIV_OWNER|$BOB" ] ||
  setup_fail "$PRIV_PROJECT reads back as '$read_back', not private with owner $PRIV_OWNER and $BOB listed"
note "made $PRIV_PROJECT private with owner $PRIV_OWNER and $BOB listed by writing its rows: the stand-in for PATCH /api/v1/projects/$PRIV_PROJECT/access {\"visibility\":\"private\"} and PUT /api/v1/projects/$PRIV_PROJECT/access/people/$BOB until avenue A's t-a-5 and t-a-20 land"

# --- The stack's machine: its metadata stand-in (the fleet example-fleet, which no machine
# registration names), started once and asked once, so its identity token is minted now and kept
# for ten minutes. Each mint queues the machine's claims at the issuer's sign-in control, so a mint
# while a person's sign-in is queued could take that person's place; after this one, none runs
# during the sign-ins. ---
METADATA_PORT_FILE="$WORK_DIR/metadata-port"
env -i PATH="$PATH" HOME="$WORK_DIR" DEV_STACK_METADATA_PORT_FILE="$METADATA_PORT_FILE" \
  python3 "$METADATA_SCRIPT" >"$WORK_DIR/metadata.log" 2>&1 &
PIDS+=("$!")
for _ in $(seq 120); do
  [ -s "$METADATA_PORT_FILE" ] && break
  sleep 0.5
done
METADATA_PORT="$(tr -dc '0-9' <"$METADATA_PORT_FILE" 2>/dev/null || true)"
[ -n "$METADATA_PORT" ] || setup_fail "the metadata stand-in wrote no port; see $WORK_DIR/metadata.log"
curl -sf -H 'Metadata-Flavor: Google' -o /dev/null \
  "http://127.0.0.1:$METADATA_PORT/computeMetadata/v1/instance/service-accounts/default/identity?audience=$MACHINE_AUDIENCE&format=full" \
  >/dev/null || setup_fail "the metadata stand-in minted no identity token; see $WORK_DIR/metadata.log"
machine_env=(AGENT_SECRETS_MACHINE_IDENTITY=metadata AGENT_SECRETS_MACHINE_AUDIENCE="$MACHINE_AUDIENCE"
  GCE_METADATA_HOST="127.0.0.1:$METADATA_PORT")

# --- One box per caller: a HOME, runtime and state directories, a key dir and sign-in stores of
# its own, so no caller's state is another's. ---
box() { # <name>
  local dir="$WORK_DIR/boxes/$1"
  mkdir -p "$dir/home" "$dir/run" "$dir/state" "$dir/keys" "$dir/signins" "$dir/session-keys" "$dir/session-signins" "$dir/helper-keys"
  chmod 700 "$dir" "$dir"/*
  printf '%s' "$dir"
}
base_env() { # <box dir>
  printf '%s\n' PATH="$BIN_DIR:$PATH" HOME="$1/home" XDG_RUNTIME_DIR="$1/run" XDG_STATE_HOME="$1/state" AGENT_SECRETS_URL="$BROKER_URL"
}

# sign_in signs person in on box with `agent-secrets login` at the stack's issuer: the person is
# queued at the issuer's sign-in control and the code the login printed is approved, which is
# this stack's stand-in for a person typing that code in at Google.
sign_in() { # <box dir> <person>
  local dir="$1" person="$2" code="" pid rc=0
  local -a env_of
  mapfile -t env_of < <(base_env "$dir")
  # Made here, before the login starts: its own redirections open them only once it runs, and the
  # poll below may read first.
  : >"$dir/login.out"
  : >"$dir/login.err"
  env -i "${env_of[@]}" "${machine_env[@]}" "${signin_client_env[@]}" AGENT_SECRETS_SIGNIN_DIR="$dir/signins" \
    AGENT_SECRETS_KEY_DIR="$dir/keys" AGENT_SECRETS_HELPER_SOCK= \
    agent-secrets login >"$dir/login.out" 2>"$dir/login.err" &
  pid=$!
  PIDS+=("$pid")
  for _ in $(seq 240); do
    code="$(sed -n 's/.*type the code \([^ ]*\).*/\1/p' "$dir/login.err" | head -n1)"
    [ -z "$code" ] || break
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.5
  done
  [ -n "$code" ] || setup_fail "$person's agent-secrets login printed no code: $(tr '\n' ' ' <"$dir/login.err")"
  # hd is the account's Google Workspace domain, as Google's own ID token carries it: the broker's
  # exchange admits a person only from BROKER_SIGNIN_DOMAIN, read from that claim.
  curl -sf -X POST "$ISSUER_URL/_dev/sign-in-as" -H 'Content-Type: application/json' \
    -d "$(jq -cn --arg p "$person" '{sub: $p, email: $p, email_verified: true, hd: ($p | split("@")[1])}')" >/dev/null ||
    setup_fail "the issuer would not queue $person's sign-in"
  curl -sf -X POST "$ISSUER_URL/_dev/approve-device-code" -H 'Content-Type: application/json' \
    -d "$(jq -cn --arg c "$code" '{user_code: $c}')" >/dev/null ||
    setup_fail "the issuer would not approve $person's code $code"
  wait "$pid" || rc=$?
  if [ "$rc" != 0 ] || ! grep -qF -- "$person is signed in" "$dir/login.out"; then
    setup_fail "$person's agent-secrets login exited $rc: $(tr '\n' ' ' <"$dir/login.err")"
  fi
}

# start_helper starts the box's agent-secrets-helper, which reads the box's sign-ins and enrols
# each session registered with it under the person signed in there, naming the session's
# projects.
start_helper() { # <box dir> <person> <name>
  local dir="$1"
  local -a env_of
  mapfile -t env_of < <(base_env "$dir")
  printf '%s\n' "$2" >"$dir/operator"
  env -i "${env_of[@]}" "${machine_env[@]}" "${signin_client_env[@]}" AGENT_SECRETS_SIGNIN_DIR="$dir/signins" \
    AGENT_SECRETS_HELPER_SOCK="$dir/helper.sock" AGENT_SECRETS_KEY_DIR="$dir/helper-keys" \
    AGENT_SECRETS_MACHINE_FILE="$dir/machine.json" AGENT_SECRETS_STATE_FILE="$dir/sessions.json" \
    AGENT_SECRETS_OPERATOR_FILE="$dir/operator" AGENT_SECRETS_SESSION="project-secrets-check-$3-box" \
    agent-secrets-helper serve >"$dir/helper.log" 2>&1 &
  PIDS+=("$!")
  for _ in $(seq 120); do
    [ -e "$dir/helper.sock" ] && return 0
    sleep 0.5
  done
  setup_fail "$3's agent-secrets-helper made no socket; see $dir/helper.log"
}

# person_trade_env and session_env are a person's two environments on a box: the trade reads the
# person's sign-in; the session reads none, so its calls go to the box's helper.
person_trade_env() { # <box dir>
  base_env "$1"
  printf '%s\n' "${machine_env[@]}" "${signin_client_env[@]}" AGENT_SECRETS_SIGNIN_DIR="$1/signins" \
    AGENT_SECRETS_KEY_DIR="$1/keys" AGENT_SECRETS_HELPER_SOCK= AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$ACCOUNT_TOKEN_FILE"
}
session_env() { # <box dir>
  base_env "$1"
  printf '%s\n' AGENT_SECRETS_HELPER_SOCK="$1/helper.sock" AGENT_SECRETS_SIGNIN_DIR="$1/session-signins" \
    AGENT_SECRETS_KEY_DIR="$1/session-keys"
}
# session_request asks for one secret from a session the box's helper registers and enrols,
# naming projects ("" for none).
session_request() { # <name> <box dir> <projects> <secret>
  local -a env_of
  mapfile -t env_of < <(session_env "$2")
  [ -z "$3" ] || env_of+=(AGENT_SECRETS_PROJECTS="$3")
  capture "$1" env -i "${env_of[@]}" agent-secrets register --wait "$REGISTER_WAIT" --exec -- \
    agent-secrets request "$4" --json
}

POD_BOX="$(box pod)"
CAROL_BOX="$(box carol)"
ALICE_BOX="$(box alice)"
MACHINE_BOX="$(box machine)"
sign_in "$CAROL_BOX" "$PRIV_OWNER"
sign_in "$ALICE_BOX" "$ALICE"
start_helper "$CAROL_BOX" "$PRIV_OWNER" carol
start_helper "$ALICE_BOX" "$ALICE" alice
note "$PRIV_OWNER and $ALICE are signed in on boxes of their own, each with its helper"
echo

# --- PRIV's pod, the production path. ---
POD="PRIV's pod"
mapfile -t pod_env < <(base_env "$POD_BOX")
pod_env+=(AGENT_SECRETS_KEY_DIR="$POD_BOX/keys" AGENT_SECRETS_SIGNIN_DIR="$POD_BOX/signins" AGENT_SECRETS_HELPER_SOCK=)
trade pod-trade "${pod_env[@]}" AGENT_SECRETS_MACHINE_IDENTITY=kubernetes \
  AGENT_SECRETS_MACHINE_TOKEN_FILE="$PRIV_EXCHANGE_TOKEN_FILE" AGENT_SECRETS_SESSION="$PRIV_SESSION" \
  AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$ACCOUNT_TOKEN_FILE"
expect_trade "$POD" named "[\"$PRIV_PROJECT\"]"
capture pod-enroll env -i "${pod_env[@]}" agent-secrets enroll --pod --service "$PRIV_SERVICE" \
  --pod-token-file "$PRIV_POD_TOKEN_FILE" --out "$POD_BOX/keys"
[ "$RC" = 0 ] || note "$POD: agent-secrets enroll --pod exited $RC: $(said)"
for secret in "$PRIV_SECRET" "$PUB_SECRET"; do
  capture "pod-$secret" env -i "${pod_env[@]}" agent-secrets request "$secret" --json
  expect_granted "$POD" "$secret"
done

# --- carol's session listing PRIV: the refusal case. It reads avenue F's machine check
# (dispatch://INFRA-3894/artifact/plan-md, #t-f-9): F's refusal passes the row, and before F's check
# exists the row passes only on D1's answer, a person-scope token carrying ["PRIV"]. When F's check
# lands, F's task deletes the before-F branch of expect_listing_trade and the granted branch below,
# so the row expects F's refusal alone. ---
CAROL_LISTING="carol's session listing $PRIV_PROJECT"
mapfile -t carol_trade_env < <(person_trade_env "$CAROL_BOX")
trade carol-listing-trade "${carol_trade_env[@]}" AGENT_SECRETS_PROJECTS="$PRIV_PROJECT"
expect_listing_trade "$CAROL_LISTING" "$PRIV_PROJECT" "$PRIV_OWNER"
for secret in "$PRIV_SECRET" "$PUB_SECRET"; do
  session_request "carol-listing-$secret" "$CAROL_BOX" "$PRIV_PROJECT" "$secret"
  if [ "$BRANCH" = f ]; then
    expect_f_refusal "$CAROL_LISTING" "$secret"
  else
    expect_granted "$CAROL_LISTING" "$secret" "(before F's check)"
  fi
done

# --- alice's session listing PRIV: her person cannot see PRIV, so the trade and the enrollment
# are both refused, with or without F's check (F's check runs after D's). ---
ALICE_LISTING="alice's session listing $PRIV_PROJECT"
mapfile -t alice_trade_env < <(person_trade_env "$ALICE_BOX")
trade alice-trade "${alice_trade_env[@]}" AGENT_SECRETS_PROJECTS="$PRIV_PROJECT"
if [ "$RC" = 3 ] && grep -qF -- "cannot see" "$ERR" && grep -qF -- "$PRIV_PROJECT" "$ERR"; then
  row_pass "$ALICE_LISTING: its trade: refused: $(said)"
else
  row_fail "$ALICE_LISTING: its trade: want refused, her person cannot see $PRIV_PROJECT; got $(describe_trade)"
fi
mapfile -t alice_session_env < <(session_env "$ALICE_BOX")
capture alice-enroll env -i "${alice_session_env[@]}" AGENT_SECRETS_PROJECTS="$PRIV_PROJECT" \
  agent-secrets register --wait "$REGISTER_WAIT"
if [ "$RC" != 0 ] && grep -qF -- PROJECT_NOT_VISIBLE "$ERR"; then
  row_pass "$ALICE_LISTING: its enrollment: refused PROJECT_NOT_VISIBLE"
else
  row_fail "$ALICE_LISTING: its enrollment: want refused PROJECT_NOT_VISIBLE; got exit $RC, stdout $(tr '\n' ' ' <"$OUT"), stderr $(said)"
fi

# --- carol's session naming nothing. ---
CAROL_NONE="carol's session naming nothing"
trade carol-none-trade "${carol_trade_env[@]}"
expect_trade "$CAROL_NONE" person "[]"
session_request carol-none-pub "$CAROL_BOX" "" "$PUB_SECRET"
expect_granted "$CAROL_NONE" "$PUB_SECRET"
session_request carol-none-priv "$CAROL_BOX" "" "$PRIV_SECRET"
expect_not_named "$CAROL_NONE" "$PRIV_SECRET"

# --- The machine's own session: this machine's identity, no person signed in and no helper, so
# agent-secrets registers a session of the machine's own in the key dir. ---
MACHINE="the machine's own session"
mapfile -t machine_session_env < <(base_env "$MACHINE_BOX")
machine_session_env+=("${machine_env[@]}" AGENT_SECRETS_KEY_DIR="$MACHINE_BOX/keys"
  AGENT_SECRETS_SIGNIN_DIR="$MACHINE_BOX/signins" AGENT_SECRETS_HELPER_SOCK=
  AGENT_SECRETS_SESSION=project-secrets-check-machine)
trade machine-trade "${machine_session_env[@]}" AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$ACCOUNT_TOKEN_FILE"
expect_trade "$MACHINE" "" "[]"
capture machine-pub env -i "${machine_session_env[@]}" agent-secrets request "$PUB_SECRET" --json
expect_granted "$MACHINE" "$PUB_SECRET"
capture machine-priv env -i "${machine_session_env[@]}" agent-secrets request "$PRIV_SECRET" --json
expect_not_named "$MACHINE" "$PRIV_SECRET"

# --- PRIV's pod behind the airlock. The grant's values are released as the client releases them
# (a request, a signed POST to its values route), and its placeholder is spent through the proxy at
# the bound host, whose own answer is read. ---
AIRLOCK="PRIV's pod behind the airlock"
SERVED="a proxied request is served while $PRIV_OWNER is on $PRIV_PROJECT"
REFUSED="the next proxied request is refused 407 once $PRIV_OWNER is off $PRIV_PROJECT"
placeholder="" password=""
capture airlock-request env -i "${pod_env[@]}" agent-secrets request "$PRIV_SECRET" --ttl 30m --json
grant="$(request_field '.grant_id // ""')"
if [ -n "$grant" ]; then
  values_url="$BROKER_URL/v1/grants/$grant/values"
  capture airlock-sign env -i "${pod_env[@]}" agent-secrets sign --method POST --url "$values_url"
  proof="$(tr -d '\n' <"$OUT")"
  capture airlock-values curl -sS -X POST -H "Proof: $proof" "$values_url"
  placeholder="$(jq -r --arg n "$PRIV_SECRET" '.airlock.names[$n].value // ""' "$OUT" 2>/dev/null || true)"
  password="$(jq -r '.airlock.proxy_password // ""' "$OUT" 2>/dev/null || true)"
fi
# proxied spends the placeholder at the bound host through the proxy, leaving the CONNECT's status
# and the bound host's in OUT ("407 000" for a refused CONNECT) and the host's answer beside it.
# The proxy's credential and the Authorization header are read from files (-K <(printf …) for
# proxy-user, -H @<(printf …)), never given as arguments, so neither the grant's proxy password
# nor its placeholder is in a command line another process of the pod can read.
proxied() { # <name>
  capture "$1" curl -sS --noproxy '' --cacert "$AIRLOCK_CA_CERT_FILE" \
    --proxy "http://${AIRLOCK_PROXY_URL#*://}" -K <(printf 'proxy-user = "agent-secrets:%s"\n' "$password") \
    -H @<(printf 'Authorization: Bearer %s\n' "$placeholder") -o "$WORK_DIR/calls/$1.body" \
    -w '%{http_connect} %{http_code}' "$AIRLOCK_BOUND_URL/ok"
}
if [ -z "$placeholder" ] || [ -z "$password" ]; then
  why="$PRIV_SECRET was released with no airlock placeholder: grant '${grant}', $(tr '\n' ' ' <"$OUT") $(said)"
  row_fail "$AIRLOCK: $PRIV_SECRET: $SERVED: $why"
  row_fail "$AIRLOCK: $PRIV_SECRET: $REFUSED: not run, since $why"
else
  proxied airlock-before
  codes="$(cat "$OUT")"
  body="$(tr '\n' ' ' <"$WORK_DIR/calls/airlock-before.body" 2>/dev/null | sed 's/ *$//' || true)"
  if [ "$codes" = "200 200" ] && [[ "$body" == *"stand-in answered"* ]]; then
    row_pass "$AIRLOCK: $PRIV_SECRET: $SERVED (CONNECT 200, then 200: \"$body\")"
  else
    row_fail "$AIRLOCK: $PRIV_SECRET: $SERVED: want CONNECT 200 then 200 from the bound host; got '$codes', body '$body', stderr $(said)"
  fi
  # carol, the person of the pod's exchange entry, taken off PRIV through its stand-in.
  if standin_patch_access_owner >"$WORK_DIR/calls/owner-off.out" 2>&1 && [ "$(priv_row)" = "private|$BOB|$BOB" ]; then
    note "took $PRIV_OWNER off $PRIV_PROJECT by writing its rows: the stand-in for PATCH /api/v1/projects/$PRIV_PROJECT/access {\"owner\":\"$BOB\"} until avenue A's t-a-5 and t-a-20 land"
    note "waiting ${K3_CACHE_WAIT}s: the broker keeps a K3 answer for projectaccess.CacheTTL (30 s)"
    sleep "$K3_CACHE_WAIT"
    proxied airlock-after
    codes="$(cat "$OUT")"
    if [ "${codes%% *}" = 407 ]; then
      row_pass "$AIRLOCK: $PRIV_SECRET: $REFUSED"
    else
      body="$(tr '\n' ' ' <"$WORK_DIR/calls/airlock-after.body" 2>/dev/null | sed 's/ *$//' || true)"
      row_fail "$AIRLOCK: $PRIV_SECRET: $REFUSED: want the CONNECT refused 407; got '$codes', body '$body', stderr $(said)"
    fi
  else
    row_fail "$AIRLOCK: $PRIV_SECRET: $REFUSED: not run, since $PRIV_OWNER could not be taken off $PRIV_PROJECT (the stand-in for PATCH /api/v1/projects/$PRIV_PROJECT/access): $(tr '\n' ' ' <"$WORK_DIR/calls/owner-off.out") $(priv_row)"
  fi
fi

echo
finish "$STACK_ROWS"
