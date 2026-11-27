# shellcheck shell=bash
# shellcheck disable=SC2153 # WORK_DIR and the other capitals read here are dev-stack.sh's own
# packages/envoy/scripts/lib/project-access-stack.sh
# The private project's half of scripts/dev-stack.sh, in one place: PRIV's pod and the broker's
# own access account, which every run mints tokens for, lists and exports, and what
# --project-access adds to a run: the broker's client of this run's Dispatch and the airlock
# beside it (its stands are lib/airlock-stack.sh's), the check that the broker's proxy bound the
# port this run picked, the settings that admit the broker's account to Dispatch's access routes,
# and the DEV_STACK_PRIV_*, DEV_STACK_PROJECT_ACCESS and DEV_STACK_AIRLOCK_* lines of the banner.
# It is sourced, never run, and dev-stack.sh is its one caller: it sources this file once its
# work directory is made, since the names below are paths in it, and calls each function below
# at the point of the run the function names. dev-stack.sh keeps the order its processes start
# in and the cleanup that stops them, the stands' container among them (airlock_stands_stop).
#
# What it reads of dev-stack.sh's: WORK_DIR, BIN_DIR and SCRIPT_DIR; PROJECT_ACCESS, the option;
# DISPATCH_URL and ISSUER_URL; BROKER_PID and BROKER_LOG; and await, with the awaited it sets.

# PRIV's pod, the production path of a private project's agent (scripts/project-secrets-check.sh):
# a Kubernetes service account BROKER_EXCHANGE_ACCOUNTS maps to the project PRIV for its owner,
# and binds in BROKER_SERVICE_LOGINS alone, as the service legion-priv, so the pod can log in
# with its own token to enrol itself (agent-secrets enroll --pod). It is not in BROKER_SERVICES:
# the pod reads no service-owned secret, and its project comes from its exchange entry through
# its verified subject. Its two token files are one pod, as EXCHANGE_ACCOUNT's are. The entry fixes
# the session the pod's tokens write under, as a long-running service's entry does, so its trade
# needs no registration of a session name.
PRIV_PROJECT="PRIV"
PRIV_OWNER="carol@example.com"
PRIV_SERVICE="legion-priv"
PRIV_ACCOUNT="system:serviceaccount:legion-priv:legion-priv-worker"
PRIV_SESSION="legion-priv-worker-session"
PRIV_EXCHANGE_TOKEN_FILE="$WORK_DIR/priv-exchange-token"
PRIV_POD_TOKEN_FILE="$WORK_DIR/priv-pod-token"
# The broker's own account for Dispatch's access routes, and Dispatch's audience, as production
# names them (system:serviceaccount:dispatch:envoy-broker, audience dispatch). Used only with
# --project-access: the broker presents this token to Dispatch, and Dispatch admits the account
# in both of its lists (DISPATCH_OIDC_ACCOUNTS and DISPATCH_ACCESS_ACCOUNTS).
DISPATCH_AUDIENCE="dispatch"
BROKER_ACCESS_ACCOUNT="system:serviceaccount:example-ns:example-broker"
BROKER_ACCESS_TOKEN_FILE="$WORK_DIR/broker-access-token"
# The airlock, under --project-access alone: the one secret behind it, the kind it is behind and
# that kind's one host, the profiles file, and the container the stands run in (one per run, named
# from this run's own suffix so two runs never share one). The secret is behind the kind by its own
# proxy tag, which whoever writes it into the store sets (scripts/project-secrets-check.sh does,
# from the kind the banner exports); the profiles file declares the kind and names no profile. The
# image is any with a glibc: the only thing run in it is the stands binary this run built,
# bind-mounted in.
PRIV_SECRET="PRIV_KEY"
AIRLOCK_KIND="priv"
AIRLOCK_BOUND_HOST="priv.airlock.test"
AIRLOCK_PROFILES_FILE="$WORK_DIR/profiles.json"
AIRLOCK_IMAGE="${DEV_STACK_AIRLOCK_IMAGE:-docker.io/library/python:3.12-slim}"
AIRLOCK_CONTAINER="dev-stack-stands-$(printf '%s' "${WORK_DIR##*.}" | tr '[:upper:]' '[:lower:]')"

# project_access_broker_setup sets, for the broker dev-stack.sh starts next, the settings
# --project-access gives it, and starts the airlock they name; without the option it leaves each
# empty and starts nothing:
#   PROJECT_ACCESS_ENV   BROKER_PROJECT_ACCESS_URL and BROKER_PROJECT_ACCESS_TOKEN_FILE
#   AIRLOCK_ENV          BROKER_PROFILES_FILE and the BROKER_AIRLOCK_* settings (airlock_broker_env)
#   AIRLOCK_PROXY_PORT   the port this run picked for the broker's proxy
#   AIRLOCK_PROXY_URL    http://127.0.0.1:<that port>
# The first two are arrays of NAME=value the broker's start line passes on.
#
# With --project-access the broker asks this run's Dispatch about projects, at the port the keeper
# already holds for it, presenting its own account's token; without it the broker holds no such
# client and serves no project-tagged secret, as before.
# shellcheck disable=SC2034 # PROJECT_ACCESS_ENV and AIRLOCK_ENV are the caller's: dev-stack.sh starts the broker with them
project_access_broker_setup() {
  PROJECT_ACCESS_ENV=()
  AIRLOCK_ENV=()
  AIRLOCK_PROXY_URL=""
  AIRLOCK_PROXY_PORT=""
  if [ "$PROJECT_ACCESS" = true ]; then
    PROJECT_ACCESS_ENV=(BROKER_PROJECT_ACCESS_URL="$DISPATCH_URL" BROKER_PROJECT_ACCESS_TOKEN_FILE="$BROKER_ACCESS_TOKEN_FILE")
    # The airlock, so a proxied request of a private project's secret can be driven on this stack
    # (scripts/project-secrets-check.sh). The stands run in a container of their own: a bound host
    # must listen on port 443 (the proxy refuses any other port as a host header mismatch), which no
    # unprivileged process binds on a machine whose ip_unprivileged_port_start is above it or whose
    # 443 another listener holds. scripts/lib/airlock-stack.sh starts and stops that container, and
    # dev-stack.sh, through this file, is its one caller: dev-airlock.sh starts its own stands as a
    # process of this machine, four of them on loopback addresses' port 443, each with options of
    # its own.
    # shellcheck source=lib/airlock-stack.sh
    . "$SCRIPT_DIR/lib/airlock-stack.sh"
    airlock_stands_start "$BIN_DIR/dev-airlock-stands" "$WORK_DIR" "$AIRLOCK_BOUND_HOST" "$AIRLOCK_IMAGE" "$AIRLOCK_CONTAINER"
    airlock_profiles_write "$AIRLOCK_PROFILES_FILE" "$AIRLOCK_KIND" "$AIRLOCK_BOUND_HOST"
    # The proxy's port, which the broker must be told at boot and binds itself: the kernel picks a
    # free one here, this script lets it go, and the broker takes it a moment later. Nothing holds
    # it in between, so another process on this machine can take it first; that race is why the
    # broker's own proxy line is read back (project_access_proxy_check) and a run whose proxy is
    # not on this port stops rather than printing an address nothing serves.
    AIRLOCK_PROXY_PORT="$(python3 -c "
import socket
s = socket.socket()
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()
")"
    AIRLOCK_PROXY_URL="http://127.0.0.1:${AIRLOCK_PROXY_PORT}"
    mapfile -t AIRLOCK_ENV < <(airlock_broker_env "$AIRLOCK_PROFILES_FILE" "$AIRLOCK_PROXY_URL" "127.0.0.1:${AIRLOCK_PROXY_PORT}")
  fi
}

# airlock_addr_in prints the address the broker's proxy reports it listens on, read from the
# broker's log $1, as dev-stack.sh's own readers read each process's; await runs it by name.
airlock_addr_in() { jq -Rnr '[inputs | fromjson? | select(.message == "airlock proxy listening") | .addr][0] // empty' "$1"; }

# project_access_proxy_check, under --project-access, waits for the broker's proxy line and stops
# the run when the proxy is not on the port project_access_broker_setup picked; dev-stack.sh calls
# it once the broker answers its health check. Without the option it does nothing.
# shellcheck disable=SC2154 # awaited is dev-stack.sh's, set by its await
project_access_proxy_check() {
  if [ "$PROJECT_ACCESS" = true ]; then
    # The port the broker's proxy actually bound, read from its own line, against the one this
    # script picked: a lost race stops the run here rather than leaving a caller to find that the
    # address the banner names serves nothing.
    await "the broker's airlock proxy" "$BROKER_PID" "$BROKER_LOG" "$BROKER_LOG" 'its proxy address' airlock_addr_in
    if [ "$awaited" != "127.0.0.1:${AIRLOCK_PROXY_PORT}" ]; then
      echo "dev-stack: the broker's airlock proxy is on $awaited, not the 127.0.0.1:${AIRLOCK_PROXY_PORT} this run picked: another process took that port first. Start the run again." >&2
      exit 1
    fi
    echo "dev-stack: airlock proxy $AIRLOCK_PROXY_URL, bound host $AIRLOCK_S_URL" >&2
  fi
}

# project_access_dispatch_env sets DISPATCH_ACCESS_ENV, an array of NAME=value Dispatch's start
# line passes on; dev-stack.sh calls it once the issuer is up and before Dispatch starts.
#
# With --project-access, Dispatch takes this run's issuer's service-account tokens for its own
# audience, admits the broker's account (DISPATCH_OIDC_ACCOUNTS, R13's list) and lets it read the
# access routes (DISPATCH_ACCESS_ACCOUNTS), the two lists avenue A's access answers require the
# broker in. Without it, Dispatch takes no service-account token, as before.
# shellcheck disable=SC2034 # DISPATCH_ACCESS_ENV is the caller's: dev-stack.sh starts Dispatch with it
project_access_dispatch_env() {
  DISPATCH_ACCESS_ENV=()
  if [ "$PROJECT_ACCESS" = true ]; then
    DISPATCH_ACCESS_ENV=(DISPATCH_OIDC_ISSUER="$ISSUER_URL" DISPATCH_OIDC_AUDIENCE="$DISPATCH_AUDIENCE"
      DISPATCH_OIDC_ACCOUNTS="$BROKER_ACCESS_ACCOUNT" DISPATCH_ACCESS_ACCOUNTS="$BROKER_ACCESS_ACCOUNT")
  fi
}

# project_access_exports prints the banner's lines for PRIV's pod, the option and the airlock, as
# the banner prints every export: two spaces, then `export NAME=value`. dev-stack.sh's banner
# prints them after DEV_STACK_METADATA_SCRIPT's, in its one write. In a run without
# --project-access the airlock's four values are empty.
project_access_exports() {
  local kind=""
  [ "$PROJECT_ACCESS" != true ] || kind="$AIRLOCK_KIND"
  cat <<EOF
  export DEV_STACK_PRIV_PROJECT=$PRIV_PROJECT
  export DEV_STACK_PRIV_OWNER=$PRIV_OWNER
  export DEV_STACK_PRIV_SERVICE=$PRIV_SERVICE
  export DEV_STACK_PRIV_ACCOUNT=$PRIV_ACCOUNT
  export DEV_STACK_PRIV_SESSION=$PRIV_SESSION
  export DEV_STACK_PRIV_EXCHANGE_TOKEN_FILE=$PRIV_EXCHANGE_TOKEN_FILE
  export DEV_STACK_PRIV_POD_TOKEN_FILE=$PRIV_POD_TOKEN_FILE
  export DEV_STACK_PROJECT_ACCESS=$PROJECT_ACCESS
  export DEV_STACK_PRIV_SECRET=$PRIV_SECRET
  export DEV_STACK_AIRLOCK_KIND=$kind
  export DEV_STACK_AIRLOCK_PROXY_URL=$AIRLOCK_PROXY_URL
  export DEV_STACK_AIRLOCK_BOUND_URL=${AIRLOCK_S_URL:-}
  export DEV_STACK_AIRLOCK_CA_CERT_FILE=${AIRLOCK_CA_CERT_FILE:-}
EOF
}
