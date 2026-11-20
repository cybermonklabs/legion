#!/usr/bin/env bash
# shellcheck disable=SC2317,SC2329 # cleanup is trap-invoked and each reader is invoked by name
# packages/envoy/scripts/dev-stack.sh
#
# The whole local stack a session is driven against, on one machine, with no cluster, no AWS and
# no container runtime: cmd/dev-oidc-issuer (the local stand-in for a cluster's ServiceAccount
# token issuer and for the identity provider a person signs in at), cmd/cipherd on a fake secrets
# file, internal/testnats/cmd/natsd (one throwaway NATS with JetStream), cmd/listener on that
# NATS, and cmd/dispatch, the real Dispatch server, pointed at that broker and that listener. It
# is dev-broker.sh's broker with the two things that script has no room for -- a real Dispatch and
# a real Envoy listener -- so a session on it gets a broker access token, writes to Dispatch with
# it and sends an Envoy message, all against the processes production runs.
#
# It takes one input, the URL of a Postgres server it may create databases on: --database-url,
# else LEGION_TEST_PG_DSN (a Legion pod has one), else DATABASE_URL. With none it refuses, naming
# all three. The URL's own database is never written: this run creates two databases of its own on
# that server, dev_stack_broker_<suffix> for the broker and dev_stack_dispatch_<suffix> for
# Dispatch, named from this run's own work directory as dev-broker.sh names its one, and drops
# both on exit. They are two because they are two schemas: the broker records its migrations in
# broker_schema_migrations and Dispatch records its own in schema_migrations, each migrator
# applying every migration it does not find in its own table, and production gives them separate
# databases for the same reason.
#
# It builds the Go binaries it runs into this run's own directory with one go build, unless
# --binaries <dir> names a directory that already holds them, built elsewhere: a Legion pod sends
# a Go build out to the builds farm rather than running it itself. Then it runs those in place of
# its go build, and needs no go on PATH. It requires each of them in that directory, broker,
# dev-oidc-issuer, dispatch, listener, agent-secrets, agent-secrets-helper, natsd and natsprobe,
# each an executable file, and refuses before it creates or starts anything -- no database, no
# port, no process -- when one is missing or is not, naming each such file. It copies them into
# this run's own bin directory, where a build would have put them, so everything after that step
# is the same either way.
#
# Every port is chosen by the kernel and read back from the process that holds it: the issuer's
# "issuer listening url=", the broker's JSON "broker listening" addr, natsd's "Listening for
# client connections on", the listener's JSON "envoy-listener listening" addr, and Dispatch's
# "dispatch: listening" addr. So a URL this script prints can only ever name this run's own
# process, never one left over from another. Two of the five are the exceptions to "bind, then
# report", and both serve on a socket a keeper (scripts/e2e/lib/held-port.py keep) opened on a
# port the kernel chose and holds for the whole run (as docs/site/media/broker/rig.sh does):
# Dispatch, because the broker must name its machine-login page before it starts while Dispatch
# must name the broker's URL before it starts, so Dispatch serves on a copy of its keeper's socket
# (DISPATCH_LISTEN_FD); and the listener, because it must be told its own public origin before it
# starts, the origin a DPoP proof's target URI is checked against (ENVOY_PUBLIC_URL), so it serves
# on a copy of its keeper's socket (ENVOY_LISTEN_FD) and that port goes in that one variable,
# which a restart (below) serves again. Each still reports its port in its own listening line, and
# those lines are what this script reads.
#
# Dispatch runs on its dev sign-in route (DISPATCH_DEV_SIGNIN=1), so a browser on this machine is
# signed in as any person it names with no sign-in pool, and the broker's approval page signs a
# person in at dev-oidc-issuer's code flow, as dev-broker.sh's does. The issuer also serves the
# device authorization grant (--device-flow) for a client with no browser of its own, and approves
# a pending user code at POST <issuer>/_dev/approve-device-code, so a sign-in a person would do at
# Google can be driven from a shell here. Every process is started with env -i and the variables
# it needs alone: a Legion pod exports AGENT_SECRETS_URL, DISPATCH_URL, ENVOY_URL and
# ENVOY_NATS_URL of the real deployment, and none of them may reach a process of this stack.
#
# It prints one line per process as that process reports its port, then "dev-stack: ready." and
# the exports a second shell copies, and nothing else is needed to use the stack:
#
#   PATH                                  this run's binaries first (agent-secrets and the servers)
#   AGENT_SECRETS_URL                     the broker
#   AGENT_SECRETS_ACCOUNT_TOKEN_FILE      the caller "dispatch"'s ServiceAccount token file
#   AGENT_SECRETS_APPROVER                the person the issuer signs in, who approves a secret
#   AGENT_SECRETS_APPROVE_URL             Dispatch, where a machine login's code is typed in
#   AGENT_SECRETS_HELPER_SOCK             agent-secrets-helper's socket, in the work directory
#   AGENT_SECRETS_OPERATOR_FILE           the helper's operator file
#   AGENT_SECRETS_MACHINE_FILE            the machine credential agent-secrets writes
#   AGENT_SECRETS_STATE_FILE              this run's own session records, in the work directory
#   AGENT_SECRETS_KEY_DIR                 a session key directory of this run's own
#   DISPATCH_URL, DISPATCH_TOKEN          Dispatch and its shared agent token
#   ENVOY_URL                             the listener
#   NATS_URLS                             this run's NATS
#   DEV_STACK_DIR                         the work directory (logs, tokens, fake secrets)
#   DEV_STACK_PID                         this script, which SIGUSR1 and SIGUSR2 restart natsd and
#                                         the listener through (below)
#   DEV_STACK_ISSUER_URL                  dev-oidc-issuer, for a sign-in or a minted token
#   DEV_STACK_EXCHANGE_TOKEN_FILE         the pod's token for the broker's exchange audience
#   DEV_STACK_POD_TOKEN_FILE              that same pod's token for the broker's own audience
#   DEV_STACK_REGISTRAR_TOKEN_FILE        the service login that registers a pod for a session
#   DEV_STACK_LISTENER_TOKEN_FILE         the caller "listener", which may introspect alone: the
#                                         listener of this run presents it to check an access
#                                         token a session sends /v1 under
#
# One stack, two boxes. The exports above are the first box, this machine: a helper and a
# metadata stand-in started from them act for it. Beside it the script lays out a second box, for
# the one thing a single box cannot show — a session that moves from one box to another, whose
# grant, pending request and withheld names the broker carries from one enrolment of that session
# name to the next. A box at the broker is a key: launcher_credentials_live_key allows one
# live launcher credential per key, so a second box that shared any state with the first would be
# the first. It therefore has a HOME, an XDG_RUNTIME_DIR, an XDG_STATE_HOME, a key directory, a
# machine file, a sign-in directory, an operator file, a helper socket and a metadata stand-in
# port file of its own, all under one directory of its own, and the banner prints each as
# DEV_STACK_BOX2_<the variable it stands for>. The two boxes differ in their state and in nothing
# else: both reach this run's one broker, one Dispatch and one listener, as two boxes of one
# deployment do. A caller starts that box's stand-in and its helper from the two lines the banner
# prints for them, as it starts the first box's from the lines printed there, and then runs
# agent-secrets under the second box's variables; each box trades its machine identity for a
# launcher credential of its own, bound to the identity key its own HOME holds.
#
# It then stays in the foreground until it is interrupted or any process it started stops, and
# restarts two of them in place when it is asked, for a proof that a client keeps receiving across
# a restart (scripts/e2e/nats-callout-sessions.sh): SIGUSR1 restarts natsd on the client port it
# served, its monitor port with --callout, the same JetStream store and the same callout keys;
# SIGUSR2 restarts the listener on its keeper's socket, so on the same port. Each stops the old
# process by its pid, keeps its log as <name>.log.<n>, and prints "dev-stack: natsd restarted (pid
# <old> -> <new>) <url>" or "dev-stack: listener restarted (pid <old> -> <new>) <url>" once the new
# one serves; a process that comes back on another port stops the whole stack, naming both ports,
# since every client of the stack was told the old one. The banner exports DEV_STACK_PID, this
# script's pid, which those signals go to.
#
# One trap stops every process by the pid this script started it as, drops the two databases it
# created and nothing else, and leaves the logs; it runs on a normal exit, on Ctrl-C, on SIGTERM
# and on a failure part way through, so a start that fails before "ready" leaves nothing running.
# A SIGTERM sent to the whole process group, as a supervisor's stop sends it, is that same stop,
# and so is the same signal repeated while the stop runs. The exception is a signal this script
# cannot take: a run ended by SIGKILL, which no process can trap, leaves its two databases on the
# server for someone to drop by hand, as does any other signal this script does not trap.
#
# With --callout the hub runs behind the NATS auth service, in two brokers, which
# scripts/callout-check.sh drives; scripts/lib/callout-stack.sh's header says what that adds.
#
#   LEGION_TEST_PG_DSN=... bash scripts/dev-stack.sh    # a Legion pod: the variable is already set
#   LEGION_TEST_PG_DSN=... bash scripts/dev-stack.sh \  # a Legion pod, with the binaries
#     --binaries "$TMPDIR/dev-stack-bin"                # the builds farm made
#   bash scripts/dev-postgres.sh                        # a box with Docker: start the shared
#   bash scripts/dev-stack.sh \                         # dispatch-pg, then name it here
#     --database-url 'postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable'
#
# Both from packages/envoy. It needs go (unless --binaries names the binaries), psql, curl, jq and
# python3, and nothing else from the machine: no credential, no private hostname, no production
# service.
set -euo pipefail

# refuse_old_bash MAJOR MINOR VERSION: exits 2, in one line naming the bash this script needs and
# VERSION, where MAJOR.MINOR is older than 5.1. The wait at the end (`wait -n -p`, which names the
# process that stopped) is bash 5.1's: an older bash refuses -p there, and the loop around it never
# sees a process stop and spins at once forever. So the first command hands it this bash's own
# version, before anything is created or started; scripts/dev-stack.test.sh runs it with others.
refuse_old_bash() {
  if (($1 < 5 || ($1 == 5 && $2 < 1))); then
    echo "dev-stack: needs bash 5.1 or newer (for wait -n -p), and this is bash $3" >&2
    exit 2
  fi
}
refuse_old_bash "${BASH_VERSINFO[0]}" "${BASH_VERSINFO[1]}" "$BASH_VERSION"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$ENVOY_DIR/../.." && pwd)"
HELD_PORT="$REPO_ROOT/scripts/e2e/lib/held-port.py"

usage() {
  cat <<'EOF'
Usage: dev-stack.sh [--database-url <postgres url>] [--binaries <dir>] [--callout] [--help]

Boot dev-oidc-issuer, the secrets broker, a throwaway NATS, the Envoy listener and the Dispatch
server on one machine, each on a port the kernel chooses, and print the exports a second shell
drives them with. The Postgres URL is this one option, else LEGION_TEST_PG_DSN, else
DATABASE_URL; this run creates and drops two databases of its own on that server.

With --binaries, run the prebuilt broker, dev-oidc-issuer, dispatch, listener, agent-secrets,
agent-secrets-helper, natsd and natsprobe in <dir> in place of building them; a directory short
of any of them, or holding one that is not an executable file, is refused, naming each, before
anything is created or started.

With --callout, run the hub behind the NATS auth service in two brokers, and the listener and
Dispatch with NATS service tokens rather than no credential.
EOF
}

CALLOUT=false
DATABASE_URL_OPTION=""
BINARIES_OPTION=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --database-url)
      if [ "$#" -lt 2 ]; then
        echo "dev-stack: --database-url needs a postgres:// URL" >&2
        exit 2
      fi
      DATABASE_URL_OPTION="$2"
      shift 2
      ;;
    --database-url=*)
      DATABASE_URL_OPTION="${1#--database-url=}"
      shift
      ;;
    --callout)
      CALLOUT=true
      shift
      ;;
    --binaries)
      if [ "$#" -lt 2 ] || [ -z "$2" ]; then
        echo "dev-stack: --binaries needs the directory that holds the stack's binaries" >&2
        exit 2
      fi
      BINARIES_OPTION="$2"
      shift 2
      ;;
    --binaries=*)
      BINARIES_OPTION="${1#--binaries=}"
      if [ -z "$BINARIES_OPTION" ]; then
        echo "dev-stack: --binaries needs the directory that holds the stack's binaries" >&2
        exit 2
      fi
      shift
      ;;
    --help | -h)
      usage
      exit 0
      ;;
    *)
      echo "dev-stack: unexpected argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

# --- The one input, checked before anything is created, so a missing or malformed URL leaves
# nothing behind. The three sources in order, as the header names them. ---
ADMIN_URL="${DATABASE_URL_OPTION:-${LEGION_TEST_PG_DSN:-${DATABASE_URL:-}}}"
if [ -z "$ADMIN_URL" ]; then
  cat >&2 <<'EOF'
dev-stack: no Postgres server to create this run's databases on. Name one with --database-url, or
set LEGION_TEST_PG_DSN (a Legion pod already has it) or DATABASE_URL, as
postgres://<user>@<host>:<port>/<database> naming a login that may create and drop databases. On a
box with Docker, scripts/dev-postgres.sh starts the shared dispatch-pg container and its server is
postgres://postgres:dispatch@127.0.0.1:55432/postgres?sslmode=disable.
EOF
  exit 2
fi
if ! [[ "$ADMIN_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]]; then
  echo "dev-stack: the Postgres URL must be postgres://<user>@<host>:<port>/<database>" >&2
  exit 2
fi
POSTGRES_SERVER="${BASH_REMATCH[1]}"
POSTGRES_QUERY="${BASH_REMATCH[3]:-}"

# --- The binaries this stack runs and their packages, the one list of them, as
# <binary>=<package>: the build's arguments and the names --binaries checks and copies all come
# from here. go build names each binary for its package's last path element, so a package whose
# directory is not its binary's name is renamed after the build. The header and the usage name
# them for a reader. ---
STACK_ENTRIES=(broker=./cmd/cipherd dev-oidc-issuer=./cmd/dev-oidc-issuer dispatch=./cmd/dispatch
  listener=./cmd/listener agent-secrets=./cmd/cipher agent-secrets-helper=./cmd/cipher-helper
  natsd=./internal/testnats/cmd/natsd natsprobe=./cmd/natsprobe)
STACK_PACKAGES=()
STACK_BINARIES=()
for stack_entry in "${STACK_ENTRIES[@]}"; do
  STACK_BINARIES+=("${stack_entry%%=*}")
  STACK_PACKAGES+=("${stack_entry#*=}")
done

# --- The prebuilt binaries --binaries names, checked here for the reason the URL is: a directory
# short of one leaves nothing behind, since nothing is created or started before this. Every one
# of them is required, each an executable file, and the refusal names every file that is not,
# rather than the first, so one look at it says what the build left out. ---
if [ -n "$BINARIES_OPTION" ]; then
  unusable=()
  for binary in "${STACK_BINARIES[@]}"; do
    file="$BINARIES_OPTION/$binary"
    if [ ! -e "$file" ]; then
      unusable+=("  $file: missing")
    elif [ ! -f "$file" ] || [ ! -x "$file" ]; then
      unusable+=("  $file: not an executable file")
    fi
  done
  if [ "${#unusable[@]}" -gt 0 ]; then
    echo "dev-stack: refused: --binaries $BINARIES_OPTION does not hold the ${#STACK_BINARIES[@]} binaries this stack runs (${STACK_BINARIES[*]}), each an executable file:" >&2
    printf '%s\n' "${unusable[@]}" >&2
    exit 2
  fi
fi

# go builds the binaries, so a run given them needs none.
required_tools=(psql curl jq python3)
if [ -z "$BINARIES_OPTION" ]; then
  required_tools=(go "${required_tools[@]}")
fi
for tool in "${required_tools[@]}"; do
  command -v "$tool" >/dev/null || {
    echo "dev-stack: $tool is required on PATH" >&2
    exit 1
  }
done

# --- This run's own directory: every binary, log, token and socket lives under it, so a walk
# through the stack leaves nothing in the caller's directory. TMPDIR, where a pod keeps the disk
# that is allowed to grow. ---
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/dev-stack.XXXXXX")"
RUN_SUFFIX="$(printf '%s' "${WORK_DIR##*.}" | tr '[:upper:]' '[:lower:]')"
BIN_DIR="$WORK_DIR/bin"
LOG_DIR="$WORK_DIR/logs"
mkdir -p "$BIN_DIR" "$LOG_DIR" "$WORK_DIR/keys" "$WORK_DIR/dispatch-home"

ISSUER_LOG="$LOG_DIR/dev-oidc-issuer.log"
BROKER_LOG="$LOG_DIR/broker.log"
NATS_LOG="$LOG_DIR/natsd.log"
LISTENER_LOG="$LOG_DIR/listener.log"
DISPATCH_LOG="$LOG_DIR/dispatch.log"
KEEPER_LOG="$LOG_DIR/dispatch-port-keeper.log"
KEEPER_SOCKET="$WORK_DIR/dispatch-port.sock"
KEEPER_PORT_FILE="$WORK_DIR/dispatch-port"
# The listener's keeper, which holds its port for the whole run so a restart serves it again.
LISTENER_KEEPER_LOG="$LOG_DIR/listener-port-keeper.log"
LISTENER_KEEPER_SOCKET="$WORK_DIR/listener-port.sock"
LISTENER_KEEPER_PORT_FILE="$WORK_DIR/listener-port"

# The issuer and what it mints for, as dev-broker.sh names them (internal/cipher/ciphertest's rig):
# the audience every listed caller's and every enrolment's token carries, the exchange's own
# audience, and the accounts. The pod's two token files are one pod, since dev-oidc-issuer binds
# one pod uid per service account.
BROKER_AUDIENCE="legion-broker-pod"
EXCHANGE_AUDIENCE="legion-broker-exchange"
API_ACCOUNT="system:serviceaccount:example-ns:example-dispatch"
LISTENER_ACCOUNT="system:serviceaccount:example-ns:example-listener"
EXCHANGE_ACCOUNT="system:serviceaccount:example-ns:example-worker"
REGISTRAR_ACCOUNT="system:serviceaccount:example-ns:example-daemon"
EXCHANGE_PROJECT="EXAMPLE"
EXCHANGE_SESSION="example-worker-session"
ACCOUNT_TOKEN_FILE="$WORK_DIR/account-token"
LISTENER_TOKEN_FILE="$WORK_DIR/listener-token"
EXCHANGE_TOKEN_FILE="$WORK_DIR/exchange-token"
REGISTRAR_TOKEN_FILE="$WORK_DIR/registrar-token"
POD_TOKEN_FILE="$WORK_DIR/pod-token"
# Dispatch's identity at the listener: its own account's token for the listener's audience, which
# the listener verifies to take a send's role mark from Dispatch alone (ENVOY_DISPATCH_PRINCIPAL),
# as the cluster's projected token is; without it Dispatch refuses every send for a `role:` route.
LISTENER_AUDIENCE="legion-envoy-listener"
DISPATCH_LISTENER_TOKEN_FILE="$WORK_DIR/dispatch-listener-token"
APPROVER_LOGIN="ada@example.com"
OPERATOR_FILE="$WORK_DIR/operator"
FAKE_SECRETS_FILE="$WORK_DIR/fake-secrets.json"
# The namespace the fake secrets sit under and an example KMS key in AWS's documentation account;
# nothing here calls AWS.
SECRETS_PREFIX="example/agent-secrets/"
SECRETS_KMS_KEY_ARN="arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
# The broker's own sign-in client at that issuer, through which a person signs in to its approval
# page, and the device-grant client a shell signs in through; each secret is generated below.
SIGNIN_CLIENT_ID="dev-stack-broker"
SIGNIN_SECRET_FILE="$WORK_DIR/signin-client-secret"
SIGNIN_DOMAIN="${APPROVER_LOGIN#*@}"
DEVICE_CLIENT_ID="dev-stack-device"
DEVICE_SECRET_FILE="$WORK_DIR/device-client-secret"
# This stack's machine: a fleet of one, whose Google identity token the broker takes as a subject
# of its exchange and as the actor token of a person's sign-in. The issuer mints it through a
# device-flow client of its own, so the machine's audience and the person's are two audiences and
# neither token is spendable where the other is. The account is a Google service account's shape
# in an example project; nothing here reaches Google. BROKER_VM_EXCHANGE_ACCOUNTS below gives
# that account the person and the project a controller's box is listed with in production, so the
# Dispatch access token this machine trades its identity for acts for that person and writes that
# one project; the entry fixes no session, so each process names its own (AGENT_SECRETS_SESSION).
MACHINE_CLIENT_ID="dev-stack-machine"
MACHINE_SECRET_FILE="$WORK_DIR/machine-client-secret"
FLEET_SERVICE="example-fleet"
FLEET_ACCOUNT="example-fleet@example-project.iam.gserviceaccount.com"
# A second fleet, listed in BROKER_VM_EXCHANGE_ACCOUNTS below, so the stack carries both kinds of
# machine the broker mints for: this one's token acts for a person and writes that person's
# project, as a controller's box does, while example-fleet above is listed in no such entry and
# gets a token that names its service and no person, which Dispatch refuses every write. A stack
# with only one of the two could prove only half of what this change does.
PERSON_FLEET_SERVICE="example-controller"
PERSON_FLEET_ACCOUNT="example-controller@example-project.iam.gserviceaccount.com"
METADATA_SCRIPT="$WORK_DIR/dev-metadata.py"

# The second box, whose whole state is under one directory of its own: a key directory, a machine
# file, a sign-in store, an operator file, a helper socket and a metadata stand-in port file that
# the first box's shell never reads, since a box at the broker is a key and two live launcher
# credentials never sit on one key (launcher_credentials_live_key). HOME is its own because the
# identity key a machine trades with lives at $HOME/.config/agent-secrets/identity-key.pem
# (helper.LoadOrMakeIdentityKey) and that key is what the broker names this box's credential by;
# XDG_RUNTIME_DIR and XDG_STATE_HOME are its own because the client and the helper fall back to
# both for the key directory, the socket and the sign-in store. BOX2_SESSION is the session name
# this box's own machine trade writes under, which BROKER_VM_EXCHANGE_ACCOUNTS leaves to the
# process; it is not the session a walk moves from one box to the other, which each agent-secrets
# call names for itself. Its records file is its own for a reason of the shell rather than of the
# broker: the start line below is `env`, not `env -i`, so a caller with the first box's
# AGENT_SECRETS_STATE_FILE exported would have this box's helper write the first box's records,
# which hold the first box's named sessions' signing keys (dispatch://INFRA-1167).
BOX2_DIR="$WORK_DIR/box-two"
BOX2_HOME="$BOX2_DIR/home"
BOX2_RUNTIME_DIR="$BOX2_DIR/run"
BOX2_STATE_DIR="$BOX2_DIR/state"
BOX2_KEY_DIR="$BOX2_DIR/keys"
BOX2_SIGNIN_DIR="$BOX2_DIR/signins"
BOX2_MACHINE_FILE="$BOX2_DIR/machine.json"
BOX2_STATE_FILE="$BOX2_DIR/sessions.json"
BOX2_OPERATOR_FILE="$BOX2_DIR/operator"
BOX2_HELPER_SOCK="$BOX2_DIR/helper.sock"
BOX2_METADATA_PORT_FILE="$BOX2_DIR/metadata-port"
BOX2_METADATA_LOG="$LOG_DIR/dev-metadata-box-two.log"
BOX2_HELPER_LOG="$LOG_DIR/agent-secrets-helper-box-two.log"
BOX2_SESSION="dev-stack-box-two"

BROKER_DB="dev_stack_broker_${RUN_SUFFIX}"
DISPATCH_DB="dev_stack_dispatch_${RUN_SUFFIX}"
BROKER_DATABASE_URL="${POSTGRES_SERVER}/${BROKER_DB}${POSTGRES_QUERY}"
DISPATCH_DATABASE_URL="${POSTGRES_SERVER}/${DISPATCH_DB}${POSTGRES_QUERY}"
# The listener's machine id, every broker's database URL, and what --callout adds are
# scripts/lib/callout-stack.sh's. It names paths in the work directory and reads the URLs above,
# so it is sourced here, on every run.
# shellcheck source-path=SCRIPTDIR source=lib/callout-stack.sh
. "$SCRIPT_DIR/lib/callout-stack.sh"

admin_psql() { psql "$ADMIN_URL" -v ON_ERROR_STOP=1 -q "$@"; }

KEEPER_PID=""
ISSUER_PID=""
BROKER_PID=""
BROKER2_PID=""
NATS_PID=""
LISTENER_PID=""
LISTENER_KEEPER_PID=""
DISPATCH_PID=""
# Each is set once this run's own create database succeeded: a run whose create failed (the name
# taken by another run) must never drop that other run's database.
BROKER_DB_CREATED=false
DISPATCH_DB_CREATED=false

# cleanup stops what this run started and exits with rc, the status the trap that ran it passes:
# this script's own for EXIT, and the conventional 128 plus the signal's number for INT and TERM,
# so an interrupted run never reports success. Each process is stopped by the pid this script
# started it as, in the reverse of the order they were started, and each database is dropped with
# (force) once its server has no connection of this run's left to lose.
#
# A stop by signal reaches this shell more than once: a process group's SIGTERM is delivered to
# this shell as well as to every process of the stack, a supervisor repeats the signal, and a run
# already on its way out through the EXIT trap (a process of the stack took that same signal
# first) has its own still to take. So the first thing cleanup does is ignore INT and TERM for
# the rest of the run, and it returns at once when it is already running. What it must not do is
# put either signal back on its default disposition, as `trap - INT TERM` here once did: a repeat
# arriving then kills the run between the kills and the drops below, and a signal bash has taken
# but not yet run the handler for is re-raised by bash at its next trap check, the `wait` in the
# loop below among them ("run_pending_traps: bad value in trap_list[15]: (nil)", then "signal
# handler is SIG_DFL, resending 15 (SIGTERM) to myself"). Either way the two databases outlived
# the run. Ignoring is also the one disposition a child inherits through the exec, so the psql
# each drop runs is out of the reach of a signal sent to the whole group too, and no drop is
# killed half done.
cleanup_running=false
cleanup() {
  local rc="$1" pid
  if [ "$cleanup_running" = true ]; then
    return 0
  fi
  cleanup_running=true
  trap - EXIT
  trap '' INT TERM
  set +e
  for pid in "$DISPATCH_PID" "$LISTENER_PID" "$LISTENER_KEEPER_PID" "$BROKER2_PID" "$BROKER_PID" "$NATS_PID" "$ISSUER_PID" "$KEEPER_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null
    fi
  done
  # Best-effort: cleanup runs on every exit path and must never itself fail, even if the server is
  # already gone or a drop fails for some other reason.
  if [ "$BROKER_DB_CREATED" = true ]; then
    admin_psql -c "drop database if exists ${BROKER_DB} with (force);" >/dev/null 2>&1
    echo "dev-stack: dropped this run's broker database $BROKER_DB" >&2
  fi
  if [ "$DISPATCH_DB_CREATED" = true ]; then
    admin_psql -c "drop database if exists ${DISPATCH_DB} with (force);" >/dev/null 2>&1
    echo "dev-stack: dropped this run's Dispatch database $DISPATCH_DB" >&2
  fi
  echo "dev-stack: stopped; logs kept in $LOG_DIR" >&2
  exit "$rc"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM
# The two in-place restarts, asked for by signal and run by the wait at the end of this script
# once the stack is up: a request that arrives during the start waits for it. Trapped from here
# on, so neither signal can end the run past the trap above, which their default action would.
RESTARTS=""
trap 'RESTARTS="$RESTARTS natsd"' USR1
trap 'RESTARTS="$RESTARTS listener"' USR2

echo "dev-stack: workdir $WORK_DIR" >&2

# --- The two databases, created before anything is started so a server that refuses them costs
# no process. ---
admin_psql -c "create database ${BROKER_DB};"
BROKER_DB_CREATED=true
admin_psql -c "create database ${DISPATCH_DB};"
DISPATCH_DB_CREATED=true
echo "dev-stack: created databases $BROKER_DB and $DISPATCH_DB" >&2

# A dbname= in the URL's query string overrides the URL's own database, which would point the
# broker and Dispatch, and their migrations, at a database this run neither created nor drops.
for url in "$BROKER_DATABASE_URL" "$DISPATCH_DATABASE_URL"; do
  want="${url%%\?*}"
  want="${want##*/}"
  connected="$(psql "$url" -v ON_ERROR_STOP=1 -Atqc 'select current_database()')"
  if [ "$connected" != "$want" ]; then
    echo "dev-stack: refused: a database URL of this run connects to '$connected', not $want; drop any dbname= from the Postgres URL's query string" >&2
    exit 1
  fi
done
# The banner names the databases without any password their URL carries.
DISPLAY_SERVER="$(printf '%s' "$POSTGRES_SERVER" | sed -E 's#^(postgres(ql)?://[^:/@?]+):[^@/?]*@#\1:***@#')"

# --- Every binary this stack runs, in this run's own directory. One go build: the packages share
# most of their closure, so building them together is one compile of it. With --binaries, a copy
# of each one checked above instead, into the directory the build writes, so nothing below can
# tell the two apart. ---
if [ -n "$BINARIES_OPTION" ]; then
  echo "dev-stack: copying ${STACK_BINARIES[*]} from $BINARIES_OPTION..." >&2
  for binary in "${STACK_BINARIES[@]}"; do
    cp "$BINARIES_OPTION/$binary" "$BIN_DIR/$binary"
  done
else
  echo "dev-stack: building ${STACK_BINARIES[*]}..." >&2
  (
    cd "$ENVOY_DIR"
    GOTOOLCHAIN=go1.27.3 go build -o "$BIN_DIR/" "${STACK_PACKAGES[@]}"
  )
  for stack_entry in "${STACK_ENTRIES[@]}"; do
    built="${stack_entry#*=}"
    built="${built##*/}"
    [ "$built" = "${stack_entry%%=*}" ] || mv "$BIN_DIR/$built" "$BIN_DIR/${stack_entry%%=*}"
  done
fi

# --- Each process's readiness is read from its own log, never from a port this script chose:
# await runs one reader until it prints a value, and fails the moment the process it watches is
# gone. The readers each end in `|| true` territory: under this script's pipefail a grep that has
# not matched yet is the normal case on every pass before the line is written, not a failure. ---
awaited=""
await() {
  local name="$1" pid="$2" log="$3" source="$4" what="$5"
  shift 5
  local deadline=$((SECONDS + 60))
  awaited=""
  while ((SECONDS < deadline)); do
    awaited="$("$@" "$source" 2>/dev/null || true)"
    if [ -n "$awaited" ]; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "dev-stack: $name exited during startup" >&2
      cat "$log" >&2 || true
      return 1
    fi
    sleep 0.25
  done
  echo "dev-stack: $name did not report $what within 60s" >&2
  cat "$log" >&2 || true
  return 1
}

# until_true runs a command until it succeeds, for the reason await exists: nothing here waits on
# a fixed delay.
until_true() {
  local name="$1"
  shift
  local deadline=$((SECONDS + 60))
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.25
  done
  echo "dev-stack: $name did not answer within 60s" >&2
  return 1
}

issuer_url_in() { grep -o 'issuer listening url=[^ ]*' "$1" | head -n1 | cut -d= -f2-; }
broker_addr_in() { jq -Rnr '[inputs | fromjson? | select(.message == "broker listening") | .addr][0] // empty' "$1"; }
nats_port_in() {
  grep -q 'Server is ready' "$1" || return 1
  sed -n 's/.*Listening for client connections on 127\.0\.0\.1:\([0-9][0-9]*\)$/\1/p' "$1" | head -n1
}
listener_addr_in() { jq -Rnr '[inputs | fromjson? | select(.msg == "envoy-listener listening") | .addr][0] // empty' "$1"; }
dispatch_addr_in() { sed -n 's/.*msg="dispatch: listening" addr=\([^ ]*\).*/\1/p' "$1" | head -n1; }
kept_port_in() { grep -o '^[0-9][0-9]*$' "$1" | head -n1; }

# --- Dispatch's port, held from the kernel's choice until Dispatch serves on it. The broker names
# Dispatch's machine-login page to every machine that logs in, and Dispatch names the broker's URL
# at its own boot, so one of the two must know the other's port before either starts; a keeper
# holds Dispatch's, and Dispatch serves on a copy of its socket (held-port.py keep/exec). ---
echo "dev-stack: holding a port for Dispatch..." >&2
python3 "$HELD_PORT" keep --host 127.0.0.1 --port-file "$KEEPER_PORT_FILE" --socket "$KEEPER_SOCKET" \
  >"$KEEPER_LOG" 2>&1 &
KEEPER_PID=$!
# The keeper writes its port file only once it is serving its socket, so that file naming a port
# is also the word that Dispatch's start below can be handed the socket.
await 'the Dispatch port keeper' "$KEEPER_PID" "$KEEPER_LOG" "$KEEPER_PORT_FILE" 'the port it holds' kept_port_in
DISPATCH_PORT="$awaited"
DISPATCH_URL="http://127.0.0.1:${DISPATCH_PORT}"

# --- The local issuer. It writes every token file before it logs its URL, so once that line is
# there the files are there to be read. Every sign-in it serves, through the code flow or the
# device grant, signs the approver in: nothing here queues another person at its
# POST /_dev/sign-in-as control route. The two client secrets are written under umask 077, so
# neither is readable by anyone else even for a moment, and neither process takes one on its
# command line. ---
(umask 077 && od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$SIGNIN_SECRET_FILE")
(umask 077 && od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$DEVICE_SECRET_FILE")
(umask 077 && od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$MACHINE_SECRET_FILE")
echo "dev-stack: starting dev-oidc-issuer..." >&2
: >"$ISSUER_LOG"
env -i PATH="$PATH" "$BIN_DIR/dev-oidc-issuer" "${callout_issuer_args[@]}" \
  --token "$BROKER_AUDIENCE=$API_ACCOUNT=$ACCOUNT_TOKEN_FILE" \
  --token "$BROKER_AUDIENCE=$LISTENER_ACCOUNT=$LISTENER_TOKEN_FILE" \
  --token "$EXCHANGE_AUDIENCE=$EXCHANGE_ACCOUNT=$EXCHANGE_TOKEN_FILE" \
  --token "$BROKER_AUDIENCE=$EXCHANGE_ACCOUNT=$POD_TOKEN_FILE" \
  --token "$BROKER_AUDIENCE=$REGISTRAR_ACCOUNT=$REGISTRAR_TOKEN_FILE" \
  --token "$LISTENER_AUDIENCE=$API_ACCOUNT=$DISPATCH_LISTENER_TOKEN_FILE" \
  --code-flow "$SIGNIN_CLIENT_ID=$SIGNIN_SECRET_FILE" \
  --device-flow "$DEVICE_CLIENT_ID=$DEVICE_SECRET_FILE" \
  --device-flow "$MACHINE_CLIENT_ID=$MACHINE_SECRET_FILE" \
  --sign-in-as "$APPROVER_LOGIN" \
  >"$ISSUER_LOG" 2>&1 &
ISSUER_PID=$!
await dev-oidc-issuer "$ISSUER_PID" "$ISSUER_LOG" "$ISSUER_LOG" 'its URL' issuer_url_in
ISSUER_URL="$awaited"
echo "dev-stack: dev-oidc-issuer $ISSUER_URL" >&2

# --- The bus: one nats-server in a process of this run's own (internal/testnats/cmd/natsd), with
# JetStream in the work directory, since an agent's machine has no container runtime. -1 is
# nats-server's own "choose a port". With --callout it runs behind the auth callout, whose seeds
# it makes before it serves, so it starts before the brokers that read them.
#
# start_natsd PORT MONITOR starts it on the client port PORT and, with --callout, the monitor port
# MONITOR in place of callout_natsd_args's -1: -1 both at the start, and at a restart the ports it
# reported then, since every client of the stack was told them. The store and the callout seeds
# are the same at every start, and natsd keeps the seeds it finds (internal/testnats/cmd/natsd), so
# the brokers' auth service still answers a restarted server. It sets NATS_PID, and NATS_PORT to
# the client port the server itself reported; callout_after_natsd, after it, waits for the monitor
# port. ---
start_natsd() {
  local natsd_args=("${callout_natsd_args[@]}") at
  for at in "${!natsd_args[@]}"; do
    [ "${natsd_args[$at]}" != -m ] || natsd_args[at + 1]=$2
  done
  : >"$NATS_LOG"
  env -i PATH="$PATH" "$BIN_DIR/natsd" -p "$1" -store_dir "$WORK_DIR/nats-store" "${natsd_args[@]}" \
    >"$NATS_LOG" 2>&1 &
  NATS_PID=$!
  await natsd "$NATS_PID" "$NATS_LOG" "$NATS_LOG" 'its client port' nats_port_in
  NATS_PORT="$awaited"
}
echo "dev-stack: starting natsd..." >&2
start_natsd -1 -1
NATS_URL="nats://127.0.0.1:$NATS_PORT"
echo "dev-stack: natsd $NATS_URL" >&2
callout_after_natsd

# --- The broker, on a fake Secrets Manager holding one agent-tier secret its owner's sessions get
# without asking, one human-tier secret its owner approves — both owned by the person the issuer
# signs in — and one the fleet's service owns, which this stack's machine reads and no person's
# session does. BROKER_LISTEN_ADDR and BROKER_PUBLIC_URL are port 0: cmd/cipherd reads a public URL
# whose port is 0 as "derive it from whatever address I bind", and the real one is read from its
# own listening line below. BROKER_METRICS_LISTEN_ADDR is port 0 too, so two stacks on one machine
# never contend for the metrics port's fixed default. BROKER_DRAIN_SECONDS=0 turns off the
# five-second drain that exists to outlast a Kubernetes EndpointSlice propagation nothing here
# has. BROKER_ACCESS_TOKEN_SECONDS is
# the caller's when it names one, else the broker's own default: a walk that must see a client
# renew an access token sets it to a few seconds rather than waiting out the quarter hour (the
# broker's floor is 1, and at 1 an answer's expires_in truncates to 0, which every client
# refuses, so the usable minimum is 2). ---
cat >"$FAKE_SECRETS_FILE" <<EOF
{"secrets": [
  {"name": "${SECRETS_PREFIX}demo-read-token", "kms_key_id": "${SECRETS_KMS_KEY_ARN}",
   "tags": {"owner": "${APPROVER_LOGIN}", "tier": "agent"}, "value": "demo-read-token-value"},
  {"name": "${SECRETS_PREFIX}demo-api-key", "kms_key_id": "${SECRETS_KMS_KEY_ARN}",
   "tags": {"owner": "${APPROVER_LOGIN}", "tier": "human"}, "value": "demo-api-key-value"},
  {"name": "${SECRETS_PREFIX}fleet-token", "kms_key_id": "${SECRETS_KMS_KEY_ARN}",
   "tags": {"owner": "${FLEET_SERVICE}", "tier": "agent"}, "value": "fleet-token-value"}
]}
EOF
chmod 600 "$FAKE_SECRETS_FILE"
echo "$APPROVER_LOGIN" >"$OPERATOR_FILE"
# start_broker starts one broker logging to $1 and sets started_pid: every broker of a run, as
# every replica of one deployment, is the same binary and settings on the same database.
started_pid=""
start_broker() {
  : >"$1"
  env -i PATH="$PATH" \
    BROKER_DATABASE_URL="$BROKER_POOL_DATABASE_URL" \
    BROKER_ACCESS_TOKEN_SECONDS="${BROKER_ACCESS_TOKEN_SECONDS:-900}" \
    BROKER_LISTEN_ADDR="127.0.0.1:0" \
    BROKER_PUBLIC_URL="http://127.0.0.1:0" \
    BROKER_METRICS_LISTEN_ADDR="127.0.0.1:0" \
    BROKER_MACHINE_LOGIN_URL="$DISPATCH_URL/credentials/machine" \
    BROKER_SECRETS_PREFIX="$SECRETS_PREFIX" \
    BROKER_SECRETS_KMS_KEY_ARN="$SECRETS_KMS_KEY_ARN" \
    BROKER_FAKE_SECRETS_FILE="$FAKE_SECRETS_FILE" \
    BROKER_DRAIN_SECONDS=0 \
    BROKER_K8S_OIDC_ISSUER="$ISSUER_URL" \
    BROKER_K8S_OIDC_AUDIENCE="$BROKER_AUDIENCE" \
    BROKER_API_ACCOUNTS="dispatch=$API_ACCOUNT listener=$LISTENER_ACCOUNT" \
    BROKER_SIGNIN_ISSUER="$ISSUER_URL" \
    BROKER_SIGNIN_CLIENT_ID="$SIGNIN_CLIENT_ID" \
    BROKER_SIGNIN_CLIENT_SECRET_FILE="$SIGNIN_SECRET_FILE" \
    BROKER_SIGNIN_DOMAIN="$SIGNIN_DOMAIN" \
    BROKER_EXCHANGE_K8S_AUDIENCE="$EXCHANGE_AUDIENCE" \
    BROKER_EXCHANGE_ACCOUNTS="$EXCHANGE_ACCOUNT=$EXCHANGE_PROJECT=$APPROVER_LOGIN" \
    BROKER_GOOGLE_OIDC_ISSUER="$ISSUER_URL" \
    BROKER_GOOGLE_OIDC_AUDIENCE="$MACHINE_CLIENT_ID" \
    BROKER_VM_SERVICES="$FLEET_SERVICE=$FLEET_ACCOUNT $PERSON_FLEET_SERVICE=$PERSON_FLEET_ACCOUNT" \
    BROKER_VM_EXCHANGE_ACCOUNTS="$PERSON_FLEET_ACCOUNT=$EXCHANGE_PROJECT=$APPROVER_LOGIN" \
    BROKER_SIGNIN_DEVICE_AUDIENCE="$DEVICE_CLIENT_ID" \
    BROKER_SERVICE_LOGINS="legion-daemon=$REGISTRAR_ACCOUNT" \
    BROKER_POD_REGISTRARS="$REGISTRAR_ACCOUNT=$EXCHANGE_ACCOUNT" \
    "${callout_broker_env[@]}" \
    "$BIN_DIR/broker" >"$1" 2>&1 &
  started_pid=$!
}
echo "dev-stack: starting broker..." >&2
start_broker "$BROKER_LOG"
BROKER_PID=$started_pid
await broker "$BROKER_PID" "$BROKER_LOG" "$BROKER_LOG" 'its bound address' broker_addr_in
BROKER_URL="http://$awaited"
until_true "the broker's health check" curl -sf "$BROKER_URL/healthz"
echo "dev-stack: broker $BROKER_URL" >&2
callout_after_first_broker

# --- This stack's pod, registered for the session name it exchanges under. The broker binds a
# session name to the listed account and never to a pod, so what keeps two pods of one account
# apart is this row, which a deployment's daemon writes as it starts a pod; here the registrar
# account stands in for that daemon, and the body carries the pod's own broker-audience token.
# Every broker of the run lists that account as the registrar of the pod's account
# (BROKER_POD_REGISTRARS, in start_broker), and a broker with no such entry refuses the write 403
# POD_REGISTRAR_NOT_LISTED. Without the row the exchange is refused invalid_grant, naming the
# session. Both tokens stay out of every command line: the body reaches curl on standard input
# from a here-string, which the shell writes before curl starts, so no pipeline's status can hide
# curl's, and the header from a file curl reads. ---
registration=$(printf '{"pod_token": "%s", "session": "%s"}' "$(cat "$POD_TOKEN_FILE")" "$EXCHANGE_SESSION")
if ! curl -sf -X POST "$BROKER_URL/v1/oauth/exchange/pod-sessions" \
  -H @<(printf 'Authorization: Bearer %s' "$(cat "$REGISTRAR_TOKEN_FILE")") \
  -H 'Content-Type: application/json' \
  --data @- <<<"$registration" >/dev/null; then
  echo "dev-stack: registering the exchange pod for $EXCHANGE_SESSION failed" >&2
  exit 1
fi

# --- The listener, on that bus. ENVOY_HOST_BRIDGE=127.0.0.1 because every session of this stack
# runs on this machine. It serves on a copy of a socket a keeper opened on a port the kernel chose
# and holds for the whole run (ENVOY_LISTEN_FD=3) rather than on PORT=0, for the reason Dispatch
# does: the listener has to be told its own public origin before it starts, since a DPoP proof's
# target URI is checked against that origin and nothing else, and `{port}` below is the port of
# that very socket (held-port.py exec substitutes it). The keeper is why a restart (SIGUSR2)
# serves the same port: every session of the stack was told it. The listener still reports the
# address it serves on in its own listening line, and that line is what this script reads.
#
# It takes three credentials: none, which is what every session of this stack uses and what the
# listener allows on loopback; and a broker access token, which it checks at this run's broker
# (ENVOY_AGENT_SECRETS_URL) as the caller "listener", presenting the projected token the issuer
# wrote for it. A session holding such a token is bound to the session the broker named: it may
# subscribe, claim a role, send and publish as that session and as no other.
#
# start_listener starts it on the keeper's socket and sets LISTENER_PID, and LISTENER_ADDRESS to
# the address it reported once its health check answers. ---
echo "dev-stack: holding a port for the listener..." >&2
python3 "$HELD_PORT" keep --host 127.0.0.1 --port-file "$LISTENER_KEEPER_PORT_FILE" \
  --socket "$LISTENER_KEEPER_SOCKET" >"$LISTENER_KEEPER_LOG" 2>&1 &
LISTENER_KEEPER_PID=$!
await 'the listener port keeper' "$LISTENER_KEEPER_PID" "$LISTENER_KEEPER_LOG" \
  "$LISTENER_KEEPER_PORT_FILE" 'the port it holds' kept_port_in
# The listener takes a bearer: the shared token every session and the banner's shell present, and
# Dispatch's own account token for its audience, the identity it takes a send's role mark from
# (ENVOY_DISPATCH_PRINCIPAL), as a deployment's listener does. A verifier closes /v1 to a caller
# with no bearer, so the shared token is exported in the banner (ENVOY_TOKEN).
LISTENER_API_TOKEN="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
start_listener() {
  : >"$LISTENER_LOG"
  python3 "$HELD_PORT" exec --from "$LISTENER_KEEPER_SOCKET" --fd-env ENVOY_LISTEN_FD -- \
    env -i PATH="$PATH" \
    ENVOY_LISTEN_FD=3 \
    ENVOY_LISTEN_HOST=127.0.0.1 \
    ENVOY_API_TOKEN="$LISTENER_API_TOKEN" \
    ENVOY_OIDC_ISSUER="$ISSUER_URL" \
    ENVOY_OIDC_AUDIENCE="$LISTENER_AUDIENCE" \
    ENVOY_DISPATCH_PRINCIPAL="$API_ACCOUNT" \
    ENVOY_MACHINE_ID="$LISTENER_MACHINE_ID" \
    ENVOY_HOST_BRIDGE=127.0.0.1 \
    ENVOY_PUBLIC_URL="http://127.0.0.1:{port}" \
    ENVOY_AGENT_SECRETS_URL="$BROKER_URL" \
    ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$LISTENER_TOKEN_FILE" \
    NATS_URLS="$NATS_URL" \
    "${callout_listener_env[@]}" \
    "$BIN_DIR/listener" >"$LISTENER_LOG" 2>&1 &
  LISTENER_PID=$!
  await listener "$LISTENER_PID" "$LISTENER_LOG" "$LISTENER_LOG" 'its address' listener_addr_in
  LISTENER_ADDRESS="$awaited"
  until_true "the listener's health check" curl -sf "http://$LISTENER_ADDRESS/healthz"
}
echo "dev-stack: starting listener..." >&2
start_listener
LISTENER_URL="http://$LISTENER_ADDRESS"
echo "dev-stack: listener $LISTENER_URL" >&2

# --- Dispatch, on the keeper's socket (DISPATCH_LISTEN_FD=3, which held-port.py puts the socket
# on), pointed at this run's broker, listener and NATS. Its dev sign-in route stands in for the
# sign-in pool, and its fence requires exactly what this stack is: a loopback listener, a loopback
# database, a loopback broker and listener, no DISPATCH_SIGNIN_* and no signing key of a
# deployment's. HOME and the XDG directories are this run's own, so the server reads no app.json
# and writes no key where a developer keeps theirs. ---
DISPATCH_TOKEN_VALUE="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
echo "dev-stack: starting dispatch at $DISPATCH_URL..." >&2
: >"$DISPATCH_LOG"
python3 "$HELD_PORT" exec --from "$KEEPER_SOCKET" --fd-env DISPATCH_LISTEN_FD -- \
  env -i PATH="$PATH" \
  HOME="$WORK_DIR/dispatch-home" \
  XDG_CONFIG_HOME="$WORK_DIR/dispatch-home/config" \
  XDG_DATA_HOME="$WORK_DIR/dispatch-home/data" \
  XDG_CACHE_HOME="$WORK_DIR/dispatch-home/cache" \
  DATABASE_URL="$DISPATCH_DATABASE_URL" \
  DISPATCH_LISTEN_FD=3 \
  DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_SERVER_URL="$DISPATCH_URL" \
  DISPATCH_IDENTITY=cookie \
  DISPATCH_DEV_SIGNIN=1 \
  DISPATCH_INSECURE_COOKIE=1 \
  DISPATCH_AGENT_TOKEN="$DISPATCH_TOKEN_VALUE" \
  DISPATCH_AGENT_SECRETS_URL="$BROKER_URL" \
  DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$ACCOUNT_TOKEN_FILE" \
  DISPATCH_WEB_DIST="$REPO_ROOT/packages/dispatch/web/dist" \
  ENVOY_URL="$LISTENER_URL" \
  ENVOY_TOKEN="$LISTENER_API_TOKEN" \
  ENVOY_ACCOUNT_TOKEN_FILE="$DISPATCH_LISTENER_TOKEN_FILE" \
  NATS_URLS="$NATS_URL" \
  "${callout_dispatch_env[@]}" \
  "$BIN_DIR/dispatch" >"$DISPATCH_LOG" 2>&1 &
DISPATCH_PID=$!
await dispatch "$DISPATCH_PID" "$DISPATCH_LOG" "$DISPATCH_LOG" 'the address it serves on' dispatch_addr_in
DISPATCH_URL="http://$awaited"
until_true "Dispatch's route index" curl -sf "$DISPATCH_URL/api/v1"
echo "dev-stack: dispatch $DISPATCH_URL" >&2

# --- The stand-in for a Compute Engine metadata server, written here and started by whoever
# wants it. This stack has no cloud, so a machine's Google identity token comes from the one
# issuer that can mint one with an `email` claim: dev-oidc-issuer's device grant, driven in four
# loopback calls (a device authorization; the machine's claims queued at /_dev/sign-in-as; the
# approval at /_dev/approve-device-code, which is what a person typing a code in at Google
# stands for; and the poll). It serves exactly the path internal/googleidentity asks
# (?format=full, Metadata-Flavor: Google), so GCE_METADATA_HOST pointed at it drives the real
# reader, the real client and the real broker.
#
# dev-stack.sh starts no process for it: scripts/dev-stack.test.sh drives this driver with a
# fake python3 that runs held-port.py and refuses every other script, and this stand-in is not
# part of the stack anyway — it stands in for the machine the stack runs on. The banner below
# prints the three lines that start it. ---
cat >"$METADATA_SCRIPT" <<PY
#!/usr/bin/env python3
"""A stand-in for the Compute Engine metadata server, for dev-stack.sh (which wrote this file).

It answers one path, the instance's identity token, by minting a fresh one at this run's
dev-oidc-issuer through that issuer's device grant: the machine's own claims are queued at the
issuer's loopback sign-in control and the pending code is approved at once, which is this
stack's stand-in for a person typing a code in at Google. The token is cached until it is a
quarter spent, so a renewal gets a token of its own.
"""
import http.server, json, os, socket, sys, threading, time, urllib.parse, urllib.request

ISSUER = "$ISSUER_URL"
CLIENT_ID = "$MACHINE_CLIENT_ID"
SECRET_FILE = "$MACHINE_SECRET_FILE"
# Which machine this stand-in is, and where it writes the port it listens on: the service-only
# fleet and the first box's port file by default, and whatever the caller names otherwise. Two of
# these, on two ports and two port files, is how one stack serves two boxes, so neither variable
# may be baked in: with one port file for both, the second stand-in to start would overwrite the
# first box's port with its own.
ACCOUNT = os.environ.get("DEV_STACK_METADATA_ACCOUNT") or "$FLEET_ACCOUNT"
PORT_FILE = os.environ.get("DEV_STACK_METADATA_PORT_FILE") or "$WORK_DIR/metadata-port"
IDENTITY_PATH = "/computeMetadata/v1/instance/service-accounts/default/identity"
CACHE_SECONDS = 600

lock = threading.Lock()
cached = {"token": "", "at": 0.0}

def post(url, form=None, body=None):
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
        headers = {"Content-Type": "application/x-www-form-urlencoded"}
    else:
        data = json.dumps(body).encode()
        headers = {"Content-Type": "application/json"}
    request = urllib.request.Request(url, data=data, headers=headers, method="POST")
    with urllib.request.urlopen(request, timeout=10) as answer:
        raw = answer.read()
    return json.loads(raw) if raw.strip() else {}

def mint():
    secret = open(SECRET_FILE).read().strip()
    authorization = post(ISSUER + "/device_authorization",
                         form={"client_id": CLIENT_ID, "scope": "openid email"})
    post(ISSUER + "/_dev/sign-in-as",
         body={"sub": ACCOUNT, "email": ACCOUNT, "email_verified": True})
    post(ISSUER + "/_dev/approve-device-code", body={"user_code": authorization["user_code"]})
    answered = post(ISSUER + "/token", form={
        "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
        "device_code": authorization["device_code"],
        "client_id": CLIENT_ID, "client_secret": secret})
    return answered["id_token"]

def identity():
    with lock:
        if cached["token"] and time.time() - cached["at"] < CACHE_SECONDS:
            return cached["token"]
        cached["token"], cached["at"] = mint(), time.time()
        return cached["token"]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        query = urllib.parse.parse_qs(parsed.query)
        if parsed.path != IDENTITY_PATH:
            return self.refuse(404, "this stand-in serves the instance identity path alone")
        if self.headers.get("Metadata-Flavor") != "Google":
            return self.refuse(403, "a metadata request carries Metadata-Flavor: Google")
        if query.get("format", [""])[0] != "full":
            return self.refuse(400, "the identity must be asked for with format=full")
        if query.get("audience", [""])[0] != CLIENT_ID:
            return self.refuse(400, "this stand-in mints for one audience: " + CLIENT_ID)
        try:
            token = identity().encode()
        except Exception as failure:  # the issuer is down, or refused this client
            return self.refuse(502, "mint an identity token at " + ISSUER + ": " + str(failure))
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(token)))
        self.end_headers()
        self.wfile.write(token)

    def refuse(self, status, message):
        body = message.encode()
        self.send_response(status)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        sys.stderr.write("dev-metadata: " + (fmt % args) + "\n")

server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
port = server.socket.getsockname()[1]
with open(PORT_FILE, "w") as handle:
    handle.write(str(port) + "\n")
sys.stderr.write("dev-metadata: listening host=127.0.0.1:%d\n" % port)
sys.stderr.flush()
server.serve_forever()
PY
chmod 700 "$METADATA_SCRIPT"

# --- The second box's state: the directories its helper and its client write into, and the
# operator file a machine login of its own names an approver by, which is the first box's person.
# The script creates them and starts nothing with them; the banner prints the two lines that
# start this box's metadata stand-in and its helper, as it prints the first box's. ---
mkdir -p "$BOX2_HOME" "$BOX2_RUNTIME_DIR" "$BOX2_STATE_DIR" "$BOX2_KEY_DIR" "$BOX2_SIGNIN_DIR"
chmod 700 "$BOX2_DIR" "$BOX2_HOME" "$BOX2_RUNTIME_DIR" "$BOX2_STATE_DIR" "$BOX2_KEY_DIR" "$BOX2_SIGNIN_DIR"
echo "$APPROVER_LOGIN" >"$BOX2_OPERATOR_FILE"

cat <<EOF

dev-stack: ready.

  export PATH=$BIN_DIR:\$PATH
  export AGENT_SECRETS_URL=$BROKER_URL
  export AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$ACCOUNT_TOKEN_FILE
  export AGENT_SECRETS_APPROVER=$APPROVER_LOGIN
  export AGENT_SECRETS_APPROVE_URL=$DISPATCH_URL
  export AGENT_SECRETS_HELPER_SOCK=$WORK_DIR/helper.sock
  export AGENT_SECRETS_OPERATOR_FILE=$OPERATOR_FILE
  export AGENT_SECRETS_MACHINE_FILE=$WORK_DIR/machine.json
  export AGENT_SECRETS_STATE_FILE=$WORK_DIR/sessions.json
  export AGENT_SECRETS_KEY_DIR=$WORK_DIR/keys
  export DISPATCH_URL=$DISPATCH_URL
  export DISPATCH_TOKEN=$DISPATCH_TOKEN_VALUE
  export ENVOY_URL=$LISTENER_URL
  export ENVOY_TOKEN=$LISTENER_API_TOKEN
  export NATS_URLS=$NATS_URL
  export DEV_STACK_DIR=$WORK_DIR
  export DEV_STACK_PID=$$
  export DEV_STACK_ISSUER_URL=$ISSUER_URL
  export DEV_STACK_EXCHANGE_TOKEN_FILE=$EXCHANGE_TOKEN_FILE
  export DEV_STACK_POD_TOKEN_FILE=$POD_TOKEN_FILE
  export DEV_STACK_REGISTRAR_TOKEN_FILE=$REGISTRAR_TOKEN_FILE
  export DEV_STACK_LISTENER_TOKEN_FILE=$LISTENER_TOKEN_FILE
  export DEV_STACK_MACHINE_ACCOUNT=$FLEET_ACCOUNT
  export DEV_STACK_PERSON_MACHINE_ACCOUNT=$PERSON_FLEET_ACCOUNT
  export DEV_STACK_METADATA_SCRIPT=$METADATA_SCRIPT$callout_exports

  the token clients (Go and TypeScript alike), for a session that trades this machine's identity
  or signs a person in. The exchange's answer is a Dispatch access token acting for
  $APPROVER_LOGIN on the project $EXCHANGE_PROJECT, under the session the caller names:

  export AGENT_SECRETS_MACHINE_IDENTITY=metadata
  export AGENT_SECRETS_MACHINE_AUDIENCE=$MACHINE_CLIENT_ID
  export AGENT_SECRETS_SESSION=<this process's own session name>

  Two machines, and the stand-in is whichever one you start it as. Unset, it is $FLEET_ACCOUNT,
  whose token names its service and no person: it reads that service's secrets and Dispatch
  refuses its every write. With DEV_STACK_METADATA_ACCOUNT=$PERSON_FLEET_ACCOUNT it is the fleet
  BROKER_VM_EXCHANGE_ACCOUNTS lists, whose token acts for $APPROVER_LOGIN and writes
  $EXCHANGE_PROJECT. Start one of each, on ports of their own, to drive both.
  export AGENT_SECRETS_SIGNIN_ISSUER=$ISSUER_URL
  export AGENT_SECRETS_SIGNIN_CLIENT_ID=$DEVICE_CLIENT_ID
  export AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE=$DEVICE_SECRET_FILE

  binaries:   $BIN_DIR
  logs:       $LOG_DIR  (one per process)
  databases:  $BROKER_DB and $DISPATCH_DB on $DISPLAY_SERVER (created and dropped by this run)
  fake secrets: $FAKE_SECRETS_FILE  (DEMO_READ_TOKEN at the agent tier and DEMO_API_KEY at the
                human tier, both owned by $APPROVER_LOGIN; FLEET_TOKEN at the agent tier, owned
                by the service $FLEET_SERVICE, which this machine's own sessions read and no
                person's session does)

  This stack has no cloud, so a machine's Google identity token comes from a stand-in for the
  Compute Engine metadata server, written for you at $METADATA_SCRIPT. Start it and point the
  client at it (it mints through dev-oidc-issuer's device grant for $FLEET_ACCOUNT):

    python3 $METADATA_SCRIPT >$LOG_DIR/dev-metadata.log 2>&1 &
    until [ -s $WORK_DIR/metadata-port ]; do sleep 0.1; done
    export GCE_METADATA_HOST=127.0.0.1:\$(cat $WORK_DIR/metadata-port)

  With that and the client's variables above, in a shell with no AGENT_SECRETS_HELPER_SOCK,
  \`agent-secrets FLEET_TOKEN -- <command>\` trades this machine's identity for a launcher
  credential, registers a key of its own under it and reads the fleet's secret; and
  \`agent-secrets login\` signs a person in, printing the code to approve with
  POST $ISSUER_URL/_dev/approve-device-code and {"user_code": "..."}, after which that person's
  session reads DEMO_READ_TOKEN as them.

  A second box, for a session that moves from one box to another. A box at the broker is a key —
  launcher_credentials_live_key allows one live launcher credential per key — so this box shares
  nothing with the box above: its own HOME, its own XDG directories, its own key directory,
  machine file, records file, sign-in store, operator file and helper socket, and a metadata
  stand-in of its own on a port of its own. Each is the variable it stands for under
  DEV_STACK_BOX2_:

  export DEV_STACK_BOX2_DIR=$BOX2_DIR
  export DEV_STACK_BOX2_HOME=$BOX2_HOME
  export DEV_STACK_BOX2_XDG_RUNTIME_DIR=$BOX2_RUNTIME_DIR
  export DEV_STACK_BOX2_XDG_STATE_HOME=$BOX2_STATE_DIR
  export DEV_STACK_BOX2_AGENT_SECRETS_KEY_DIR=$BOX2_KEY_DIR
  export DEV_STACK_BOX2_AGENT_SECRETS_MACHINE_FILE=$BOX2_MACHINE_FILE
  export DEV_STACK_BOX2_AGENT_SECRETS_STATE_FILE=$BOX2_STATE_FILE
  export DEV_STACK_BOX2_AGENT_SECRETS_SIGNIN_DIR=$BOX2_SIGNIN_DIR
  export DEV_STACK_BOX2_AGENT_SECRETS_OPERATOR_FILE=$BOX2_OPERATOR_FILE
  export DEV_STACK_BOX2_AGENT_SECRETS_HELPER_SOCK=$BOX2_HELPER_SOCK
  export DEV_STACK_BOX2_METADATA_PORT_FILE=$BOX2_METADATA_PORT_FILE
  export DEV_STACK_BOX2_SESSION=$BOX2_SESSION

  Its metadata stand-in is the one script above on a port of its own, which it takes from
  DEV_STACK_METADATA_PORT_FILE; with that variable unset a stand-in writes $WORK_DIR/metadata-port,
  the first box's, as it always has. With the block above exported, these start this box's stand-in
  and then its helper:

    DEV_STACK_METADATA_PORT_FILE=\$DEV_STACK_BOX2_METADATA_PORT_FILE \\
      DEV_STACK_METADATA_ACCOUNT=$PERSON_FLEET_ACCOUNT \\
      python3 $METADATA_SCRIPT >$BOX2_METADATA_LOG 2>&1 &
    until [ -s \$DEV_STACK_BOX2_METADATA_PORT_FILE ]; do sleep 0.1; done

    env HOME=\$DEV_STACK_BOX2_HOME \\
      XDG_RUNTIME_DIR=\$DEV_STACK_BOX2_XDG_RUNTIME_DIR \\
      XDG_STATE_HOME=\$DEV_STACK_BOX2_XDG_STATE_HOME \\
      AGENT_SECRETS_KEY_DIR=\$DEV_STACK_BOX2_AGENT_SECRETS_KEY_DIR \\
      AGENT_SECRETS_MACHINE_FILE=\$DEV_STACK_BOX2_AGENT_SECRETS_MACHINE_FILE \\
      AGENT_SECRETS_STATE_FILE=\$DEV_STACK_BOX2_AGENT_SECRETS_STATE_FILE \\
      AGENT_SECRETS_SIGNIN_DIR=\$DEV_STACK_BOX2_AGENT_SECRETS_SIGNIN_DIR \\
      AGENT_SECRETS_OPERATOR_FILE=\$DEV_STACK_BOX2_AGENT_SECRETS_OPERATOR_FILE \\
      AGENT_SECRETS_HELPER_SOCK=\$DEV_STACK_BOX2_AGENT_SECRETS_HELPER_SOCK \\
      AGENT_SECRETS_URL=$BROKER_URL \\
      AGENT_SECRETS_MACHINE_IDENTITY=metadata \\
      AGENT_SECRETS_MACHINE_AUDIENCE=$MACHINE_CLIENT_ID \\
      AGENT_SECRETS_SESSION=\$DEV_STACK_BOX2_SESSION \\
      GCE_METADATA_HOST=127.0.0.1:\$(cat \$DEV_STACK_BOX2_METADATA_PORT_FILE) \\
      $BIN_DIR/agent-secrets-helper serve >$BOX2_HELPER_LOG 2>&1 &

  That helper trades this box's identity for a launcher credential of its own, bound to the key it
  keeps under its own HOME, so the broker holds one live credential for each of the two boxes and a
  session can enrol on this one after it enrolled on the first. The same env prefix is how the
  client runs on this box: under it every agent-secrets call reads this box's key directory, its
  helper socket and its sign-in store, and none of the first box's.

  Sign in to Dispatch at $DISPATCH_URL/auth/_dev/signin?login=$APPROVER_LOGIN, and approve a
  secret request on the broker's own page, $BROKER_URL/approve/<record>, which signs the same
  person in at dev-oidc-issuer with no page to fill in. The pod $EXCHANGE_ACCOUNT exchanges
  $EXCHANGE_TOKEN_FILE's token for an access token that writes $EXCHANGE_PROJECT as
  $APPROVER_LOGIN, under the session $EXCHANGE_SESSION this run registered it for.

  That access token reaches the listener too: POST \$ENVOY_URL/v1/messages/send under
  \`Authorization: DPoP <token>\` with a DPoP proof of the key it is bound to acts as
  $EXCHANGE_SESSION, and a body naming any other source_session is refused 403. The listener
  checks it at $BROKER_URL as the caller "listener".

  kill -USR1 \$DEV_STACK_PID restarts natsd on the same port and store, and kill -USR2
  \$DEV_STACK_PID the listener on the same port; each prints "dev-stack: <natsd|listener>
  restarted" once the new process serves.

  Press Ctrl-C to stop every process and drop both databases.
EOF

# stop_process PID stops one process of the stack by its pid and reaps it. The wait is repeated
# because a restart signal arriving meanwhile ends a wait early, with the process still there.
stop_process() {
  kill "$1" 2>/dev/null || true
  while kill -0 "$1" 2>/dev/null; do
    wait "$1" 2>/dev/null || true
  done
}

# The two in-place restarts. Each keeps the stopped process's log as <log>.<n>, starts the new one
# on the port the old one served, and refuses one that reports another port: every client was
# told the old one, so a stack serving elsewhere is no longer the stack they were told of, and the
# return stops it here.
natsd_restarts=0
restart_natsd() {
  # The monitor port is callout_monitor_url's (callout_after_natsd), and none without --callout.
  local old="$NATS_PID" port="$NATS_PORT" monitor="${callout_monitor_url:-}" came_back
  monitor="${monitor##*:}"
  natsd_restarts=$((natsd_restarts + 1))
  echo "dev-stack: restarting natsd (pid $old) on port $port..." >&2
  stop_process "$old"
  mv "$NATS_LOG" "$NATS_LOG.$natsd_restarts"
  start_natsd "$port" "$monitor"
  callout_after_natsd
  came_back="${callout_monitor_url:-}"
  came_back="${came_back##*:}"
  if [ "$NATS_PORT" != "$port" ] || [ "$came_back" != "$monitor" ]; then
    echo "dev-stack: natsd came back on port $NATS_PORT (monitor ${came_back:-none}), not port $port (monitor ${monitor:-none}) it served before the restart; stopping" >&2
    return 1
  fi
  echo "dev-stack: natsd restarted (pid $old -> $NATS_PID) $NATS_URL" >&2
}
listener_restarts=0
restart_listener() {
  local old="$LISTENER_PID"
  listener_restarts=$((listener_restarts + 1))
  echo "dev-stack: restarting the listener (pid $old) at $LISTENER_URL..." >&2
  stop_process "$old"
  mv "$LISTENER_LOG" "$LISTENER_LOG.$listener_restarts"
  start_listener
  if [ "http://$LISTENER_ADDRESS" != "$LISTENER_URL" ]; then
    echo "dev-stack: the listener came back at http://$LISTENER_ADDRESS, not $LISTENER_URL it served before the restart; stopping" >&2
    return 1
  fi
  echo "dev-stack: listener restarted (pid $old -> $LISTENER_PID) $LISTENER_URL" >&2
}

# The stack stands until it is interrupted or any one of its processes stops; either way the trap
# above stops the rest and drops the databases. The second broker is not one of them: with
# --callout it is the replica scripts/callout-check.sh kills to show a stopped replica delays no
# connection, so the stack outlives it. A restart signal ends the wait too, its trap having
# recorded the request (which leaves `stopped` unset): each restart asked for runs, and the wait
# starts again over the new pids. A one-second tick is among the pids waited on, so a request whose
# signal landed between the look at RESTARTS and the start of the wait runs a second later rather
# than at the next signal.
while :; do
  requested="$RESTARTS"
  RESTARTS=""
  for restart in $requested; do
    "restart_$restart"
  done
  sleep 1 &
  ticker=$!
  status=0
  wait -n -p stopped "$ticker" "$KEEPER_PID" "$LISTENER_KEEPER_PID" "$ISSUER_PID" "$NATS_PID" \
    "$BROKER_PID" "$LISTENER_PID" "$DISPATCH_PID" || status=$?
  case "${stopped:-}" in
    "" | "$ticker") ;;
    *) break ;;
  esac
done
echo "dev-stack: a process of this stack stopped (status $status); stopping the rest" >&2
exit "$status"
