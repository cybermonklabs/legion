#!/usr/bin/env bash
# packages/envoy/scripts/dev-broker.sh
#
# The local dev surface a human can drive without AWS, Dispatch or a cluster. Boots Postgres
# (the dispatch-pg container, through dev-postgres.sh), builds the
# broker, its three clients (agent-secrets, agent-secrets-helper and agent-secrets-devrelay),
# dev-oidc-issuer and dev-airlock-stands, writes a fake secrets file standing in for Secrets
# Manager (DEMO_READ_TOKEN,
# tagged owner=APPROVER_LOGIN tier=agent, so APPROVER_LOGIN's sessions get it without asking,
# DEMO_API_KEY, tagged tier=human, which APPROVER_LOGIN approves, DEMO_SERVICE_TOKEN, tagged
# owner=SERVICE tier=agent, which every pod session of that registered service gets without
# asking and no person's session sees, and the three github-app secrets below) and a profiles
# file naming the profiles those github-app secrets are served as, then starts cmd/cipherd against
# both. Prints the exports a second shell needs to drive the clients against it.
#
# The GitHub a github-app secret is minted against is a stand-in this script starts:
# cmd/dev-proxy-stands, of whose ten stands this stack uses the two GitHub ones: github, plain
# http, where the broker mints (BROKER_AIRLOCK_TEST_GITHUB_API_BASE names it), and github-tls, the
# same GitHub over TLS, where a command behind the broker's proxy spends what was minted. It
# records every call it was sent, at the recording routes the banner names, and the answer it
# gives a token request is set while it runs, at the answer route the banner names too: the
# ordinary mint, the HTTP 422 that mints nothing, the two refusals that answer that same 422 to
# one part of a request and mint every other - the repository the answer names, and any
# permissions at all - a token wider than the body asked for, and one HTTP 503 before the
# ordinary mint, which the broker's minter retries. The App is installed on three accounts, each
# with an installation id of its own, and each installation holds the stand's own map of
# permissions, or what DEV_BROKER_GITHUB_PERMISSIONS names for its owner (below).
#
# Three github-app secrets are written for it, each holding that stand's App id and private key
# as its stored value. Two are owned by the registered service, as DEMO_SERVICE_TOKEN is, so a
# pod session of that service gets them without asking. DEMO_GITHUB_APP's github-owners tag names
# the one account acme-org. DEMO_GITHUB_APP_CASE_PAIR's names ada, on which the App has two
# installations whose logins differ only in case (ada and Ada): GitHubApp.Mint keys the released
# object by the login GitHub answered with and compares a profile's account without regard to
# case (internal/cipher/secrets/githubapp.go), so a profile of ada mints for both and the
# released object holds two installations, which is no one token to hand over. GitHub itself
# allows no two accounts whose logins differ only in case: this pair exists to drive that case
# and stands for nothing real.
#
# The third, DEMO_GITHUB_APP_HUMAN, is APPROVER_LOGIN's own at human tier, and its github-owners
# tag names acme-org as well. It is the one github-app secret of this stack a request for reaches
# a person: policy.Set.Evaluate answers a service's own secret Automatic for that service's own
# session and Deny for every other, never Approval, so a secret a service owns is approved by
# nobody. A request put to a person, and the check mint before the person is asked, are driven
# against this one.
#
# Every GitHub App token the broker mints is a profile's (policy.ReasonGitHubAppNoProfile refuses
# a github-app secret no profile names), so the stack's profiles file (profiles.json in WORK_DIR,
# BROKER_PROFILES_FILE) names five. DEMO_GITHUB_APP is the source of three of them: DEMO_GITHUB_APP,
# a profile of its own name whose release is the JSON of installations (value: installations);
# and DEMO_GITHUB_READ and DEMO_GITHUB_ADMIN_READ, each the one account's bare token (value:
# token) behind the broker's proxy, bound to api.github.test for GET and HEAD alone, the first
# with GitHub's eight reads and the second with those and administration, members and
# organization_administration at read. DEMO_GITHUB_APP_CASE_PAIR and DEMO_GITHUB_APP_HUMAN are
# each a profile of its own name. The broker asks GitHub for each profile's permissions cut to
# what the installation holds, and names what the cut left out in the release and in its log. The
# stored secrets of no type are named by no profile and are served by their own names, as
# before.
#
# A profile with a proxy block needs the broker's proxy, so the broker binds a proxy port too
# (BROKER_AIRLOCK_LISTEN_ADDR of 127.0.0.1:0) and signs each bound host's certificate with the
# stands' certificate authority. A release names the proxy's URL (BROKER_AIRLOCK_PROXY_URL), which
# the broker must hold at boot while its port is known only once it has bound, so this script
# binds one TCP front first (dev-airlock-stands front), names the broker's proxy by the front's
# address, and points the front at the port the broker logs, as dev-airlock.sh's fronts are
# pointed. The broker is started with KUBERNETES_SERVICE_HOST empty: a broker with a proxy that
# sees it is one in a cluster and refuses to start without that cluster's ranges, and this broker
# is a local process, in a Legion pod as anywhere else.
#
# The proxy reaches the GitHub stand only when DEV_BROKER_PROXIED_GITHUB_IP names a loopback
# address of 127.0.0.0/8, on whose port 443 the stands then serve github-tls
# under the name api.github.test: the proxy decrypts a bound host's port 443 alone, and the broker
# is told where that name is (BROKER_AIRLOCK_TEST_RESOLVE) and that it may dial it there
# (BROKER_AIRLOCK_TEST_DIAL_ALLOW). Binding port 443 needs a machine that lets this user bind it,
# so the stack leaves it out unless asked, and a call through the proxy to api.github.test is then
# refused, since the name resolves nowhere. github-profile-check.sh sets it, and sets
# DEV_BROKER_GITHUB_PERMISSIONS: what the App's installations hold, as one or more space-separated
# <owner>=<permission>:<level>[,<permission>:<level>...], which the stands take as
# --installation-permissions. DEV_BROKER_BROKER_BIN names a broker binary this script starts in
# place of building one, which is how a check's control swaps the broker and nothing else.
#
# The broker's approval routes, and its introspection route, take the calling service's own
# Kubernetes ServiceAccount token (BROKER_API_ACCOUNTS, verified against BROKER_K8S_OIDC_ISSUER),
# and so does the RFC 8693 token exchange's subject token. With no cluster to mint one, this
# script starts cmd/dev-oidc-issuer, the Go tests' local issuer (internal/oidc/oidctest) served on
# a loopback port, which keeps six tokens in WORK_DIR and rewrites each before it lapses, as a
# kubelet does a pod's projected token: account-token, for API_ACCOUNT, the caller "dispatch" the
# broker lists, which agent-secrets-devrelay presents in Dispatch's place to decide a machine
# login; listener-token, for LISTENER_ACCOUNT, the caller "listener", which the broker lists for
# introspection alone, so a person can see a listed caller refused a route its name does not
# carry (403 ACCOUNT_LACKS_POWER); exchange-token, for EXCHANGE_ACCOUNT's pod, which the broker's
# exchange takes for a Dispatch access token; read-token, for READ_ACCOUNT's pod, which the
# broker's exchange takes for a Dispatch access token that only reads; pod-token,
# EXCHANGE_ACCOUNT's pod's token for the broker's own audience, which names it in its
# registration; and registrar-token, for REGISTRAR_ACCOUNT, the service login
# (BROKER_SERVICE_LOGINS) that stands in for the deployment's daemon and registers the pod under
# the session name it writes as. The broker trusts that issuer for all six. A login alone
# registers nothing: BROKER_POD_REGISTRARS pairs REGISTRAR_ACCOUNT with EXCHANGE_ACCOUNT, the one
# registrar and pod account pair this stack's registration needs, and a broker whose list holds
# no such entry (or is unset) refuses that registration 403 POD_REGISTRAR_NOT_LISTED.
#
# READ_ACCOUNT is a read entry (BROKER_EXCHANGE_READ_ACCOUNTS) beside the one example entry
# (BROKER_EXCHANGE_ACCOUNTS). The token its pod exchanges for holds the scope read alone, the
# project scope none, the service READ_SERVICE and the session READ_SESSION, and names no person
# and no project. Dispatch serves such a token on every GET route it classes as a read and
# answers it 403 READ_ONLY_TOKEN on every other route, which
# packages/dispatch/e2e/read-token-live.ts drives against this stack and a
# `bun run dispatch:local` Dispatch. A read entry fixes its session, so its pod needs no
# registration below, and the broker refuses one with 409 SESSION_FIXED.
#
# SERVICE is registered under both halves a service needs: BROKER_SERVICE_LOGINS names the
# account that logs in as it, and BROKER_SERVICES names the account its pods run as
# (EXCHANGE_ACCOUNT's, the pod this script registers). Both are needed for a service-owned
# secret: the policy serves a secret tagged owner=<service> only for a service the deployment
# registered in BROKER_SERVICES, and refuses every other one as owner-tag-malformed on every
# reread, so without that half DEMO_SERVICE_TOKEN would reach nobody and a pod enrolled under
# the service login would have no secret of its own to ask for.
#
# A secret request is approved on the broker's own page, <broker>/approve/<record>, by a person
# signed in to the broker (BROKER_SIGNIN_*). The same issuer process stands in for that person's
# identity provider: --code-flow serves the broker's sign-in client, whose secret this script
# generates into WORK_DIR and whose one callback is the approval page's on loopback, taken at the
# port the broker binds, and --sign-in-as signs every authorization in as APPROVER_LOGIN with no
# page to fill in, so a browser on this machine approves a secret request on that page.
#
# Each invocation creates and drops its own isolated Postgres database (named from this run's own
# WORK_DIR, below) and binds OS-assigned ephemeral ports for its API and its metrics
# (BROKER_LISTEN_ADDR and BROKER_METRICS_LISTEN_ADDR of 127.0.0.1:0), so two
# agent sessions each running their own dev broker stack never see each other's enrollments,
# requests or grants. The database lives in the shared dispatch-pg container, which
# dev-postgres.sh creates, starts or reuses, and refuses when it runs an image other than that
# script's pinned pgvector one, or, with DEV_BROKER_POSTGRES_URL set
# (postgres://<user>@<host>:<port>/<database>, a role that may create databases), on that server
# through psql, with no Docker at all. cmd/cipherd binds before it reports anything and
# logs the address it actually bound; this script waits for that line and reads the real port from
# it, so a curl success can only ever mean this instance's own broker answered. It reads the
# metrics port from the broker's "broker metrics listening" line the same way, and prints it in
# the banner. The broker is
# started with BROKER_DRAIN_SECONDS=0 so this stack's cleanup stays instant: the default
# five-second drain exists to outlast a Kubernetes EndpointSlice propagation nothing here has, so
# here it would be pure delay on every Ctrl-C and every restart.
#
# The helper a second shell runs keeps its socket, records file and log in WORK_DIR too: the
# printed exports name WORK_DIR's helper.sock, operator file (written here), machine file and
# sessions.json, so a walk through the stack leaves nothing in the caller's directory and
# nothing of an installed helper's. The records file is named outright
# (AGENT_SECRETS_STATE_FILE): the helper keeps its records beside its machine login by default,
# and an export that named only the socket would leave this helper writing the records of the
# helper installed under this HOME, whose named sessions' signing keys are in them.
#
# DEV_BROKER_GCP_PREFIX and DEV_BROKER_GCP_KMS_KEY_NAME, set together, make the broker's store
# Google Cloud Secret Manager (BROKER_SECRETS_STORE=gcp-secret-manager) under that secrets prefix
# (projects/<project number>/secrets/<id prefix>) and that agent-secrets key, with an empty fake
# secrets file: GET /v1/settings then describes that store, so `agent-secrets secret` forms run
# against the real Secret Manager under whatever Google credential their own caller chose, while
# the broker itself serves none of the demo secrets below, which are under the AWS prefix. Unset,
# the stack is the AWS-shaped one above. One set without the other is a usage error (exit 2).
#
# Needs bash, go, curl and jq, and a Postgres server. With DEV_BROKER_POSTGRES_URL it reaches
# that server through psql, which is given the URL without its password and the password in
# PGPASSWORD (scripts/lib/pg-url.sh), and without it, it starts the dispatch-pg container through
# dev-postgres.sh, which needs Docker. It builds the fake secrets file and the profiles file with
# jq -n and reads the stand's report and the broker's JSON log with jq, so a box without jq fails
# once Postgres is up and the six builds have run.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVOY_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source-path=SCRIPTDIR source=lib/pg-url.sh
. "$SCRIPT_DIR/lib/pg-url.sh"

POSTGRES_CONTAINER="dispatch-pg"

# The kernel never hands out a port a live listener already holds, so binding 127.0.0.1:0 (an
# OS-assigned ephemeral port) makes a same-port collision between two dev instances impossible.
# BROKER_PUBLIC_URL mirrors it (also port 0): cmd/cipherd/main.go treats a BROKER_PUBLIC_URL whose
# port is 0 as "derive my public URL from whatever address I actually bind," since
# BROKER_PUBLIC_URL must be set before boot but the real port is only known once Listen succeeds.
# Both roles' real, reachable value is read from the broker's own "broker listening" log line
# once it's running (below).
LISTEN_ADDR="127.0.0.1:0"
PUBLIC_URL="http://127.0.0.1:0"
APPROVER_LOGIN="ada@example.com"

# DEV_BROKER_POSTGRES_URL is checked before anything is created, so a malformed one leaves nothing
# behind.
if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ] && ! [[ "$DEV_BROKER_POSTGRES_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]]; then
  echo "dev-broker: DEV_BROKER_POSTGRES_URL must be postgres://<user>@<host>:<port>/<database>" >&2
  exit 2
fi
POSTGRES_SERVER="${BASH_REMATCH[1]:-}"
POSTGRES_QUERY="${BASH_REMATCH[3]:-}"
# The two GCP store settings are checked before anything is created as well: neither means
# anything without the other.
if { [ -n "${DEV_BROKER_GCP_PREFIX:-}" ] && [ -z "${DEV_BROKER_GCP_KMS_KEY_NAME:-}" ]; } ||
  { [ -z "${DEV_BROKER_GCP_PREFIX:-}" ] && [ -n "${DEV_BROKER_GCP_KMS_KEY_NAME:-}" ]; }; then
  echo "dev-broker: DEV_BROKER_GCP_PREFIX and DEV_BROKER_GCP_KMS_KEY_NAME are set together or not at all" >&2
  exit 2
fi
# The proxied GitHub's address and the broker to start in place of a build are checked before
# anything is created too (the header): a stack that came up without the one, or on a broker
# nobody meant, would answer a check for some other stack than the one it asked for.
if [ -n "${DEV_BROKER_PROXIED_GITHUB_IP:-}" ] && ! [[ "$DEV_BROKER_PROXIED_GITHUB_IP" =~ ^127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
  echo "dev-broker: DEV_BROKER_PROXIED_GITHUB_IP must be a loopback address of 127.0.0.0/8, on whose port 443 the stands serve the proxied GitHub" >&2
  exit 2
fi
if [ -n "${DEV_BROKER_BROKER_BIN:-}" ] && { [ ! -f "$DEV_BROKER_BROKER_BIN" ] || [ ! -x "$DEV_BROKER_BROKER_BIN" ]; }; then
  echo "dev-broker: DEV_BROKER_BROKER_BIN ($DEV_BROKER_BROKER_BIN) is no executable file" >&2
  exit 2
fi

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/agent-secrets-dev.XXXXXX")"
BIN_DIR="$WORK_DIR/bin"
BROKER_BIN="$BIN_DIR/broker"
FAKE_SECRETS_FILE="$WORK_DIR/fake-secrets.json"
# The namespace and the agent-secrets key the fake secrets sit under; the key is an example ARN in
# AWS's documentation account, and nothing here calls AWS. DEV_BROKER_GCP_* replaces both with a
# Secret Manager prefix and a Cloud KMS key name, and the store with Secret Manager (the header).
SECRETS_STORE=""
SECRETS_PREFIX="example/agent-secrets/"
SECRETS_KMS_KEY_ARN="arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
SECRETS_KMS_KEY_NAME=""
STORE_LINE="the fake secrets file below, in Secrets Manager's shape (BROKER_SECRETS_STORE unset: aws-secrets-manager)"
if [ -n "${DEV_BROKER_GCP_PREFIX:-}" ]; then
  SECRETS_STORE="gcp-secret-manager"
  SECRETS_PREFIX="$DEV_BROKER_GCP_PREFIX"
  SECRETS_KMS_KEY_ARN=""
  SECRETS_KMS_KEY_NAME="$DEV_BROKER_GCP_KMS_KEY_NAME"
  STORE_LINE="gcp-secret-manager under $SECRETS_PREFIX, key $SECRETS_KMS_KEY_NAME; the fake secrets file is empty, so the broker serves none of the demo secrets below"
fi
BROKER_LOG="$WORK_DIR/broker.log"
OPERATOR_FILE="$WORK_DIR/operator"
# The profiles file (the header), and the front the broker's proxy is named by before it binds:
# its report, its log, and the file it reads its one backend from.
PROFILES_FILE="$WORK_DIR/profiles.json"
PROXY_FRONT_JSON="$WORK_DIR/proxy-front.json"
PROXY_FRONT_LOG="$WORK_DIR/proxy-front.log"
PROXY_BACKENDS="$WORK_DIR/proxy-backends"
# The stand-in GitHub, its files and the accounts it answers as installations of the App. The
# three are one account for a check that narrows a token to a repository of it, and the pair
# whose logins differ only in case, which is the one way a release covers two installations
# of one account (see the header).
STANDS_BIN="$BIN_DIR/dev-airlock-stands"
STANDS_DIR="$WORK_DIR/stands"
STANDS_JSON="$WORK_DIR/stands.json"
STANDS_LOG="$WORK_DIR/stands.log"
GITHUB_OWNER="acme-org"
GITHUB_INSTALLATION=7
GITHUB_CASE_OWNER="ada"
GITHUB_CASE_INSTALLATION=8
GITHUB_CASE_OTHER_OWNER="Ada"
GITHUB_CASE_OTHER_INSTALLATION=9
# The name the two proxied profiles bind, which the github-tls stand's certificate carries: a
# .test name no resolver answers, so the proxy reaches it only where the broker is told where it
# is (DEV_BROKER_PROXIED_GITHUB_IP, the header).
GITHUB_PROXIED_HOST="api.github.test"
# The three github-app secrets, named for the account each one's github-owners tag names. The
# human one is the approver's own, and the only one whose release a person decides. Each is a
# profile of its own name, and the first is the source of the two proxied profiles as well.
GITHUB_SECRET="DEMO_GITHUB_APP"
GITHUB_CASE_SECRET="DEMO_GITHUB_APP_CASE_PAIR"
GITHUB_HUMAN_SECRET="DEMO_GITHUB_APP_HUMAN"
GITHUB_READ_PROFILE="DEMO_GITHUB_READ"
GITHUB_ADMIN_READ_PROFILE="DEMO_GITHUB_ADMIN_READ"
# The local issuer and what it mints for (above): the audience every enrolment and listed caller's
# token carries, the exchange's own audience, and the four accounts, as internal/cipher/ciphertest's
# rig names them, beside the read entry's account below. The exchange entry fixes no session, so
# a pod names its own — and a pod of such an entry exchanges only under a name its deployment
# registered for it, which here is the registrar account's one call below. The pod's two token
# files are one pod: dev-oidc-issuer binds one pod uid per service account, as a pod's projected
# tokens for two audiences carry one uid.
ISSUER_BIN="$BIN_DIR/dev-oidc-issuer"
ISSUER_LOG="$WORK_DIR/issuer.log"
BROKER_AUDIENCE="legion-broker-pod"
EXCHANGE_AUDIENCE="legion-broker-exchange"
API_ACCOUNT="system:serviceaccount:example-ns:example-dispatch"
LISTENER_ACCOUNT="system:serviceaccount:example-ns:example-listener"
EXCHANGE_ACCOUNT="system:serviceaccount:example-ns:example-worker"
REGISTRAR_ACCOUNT="system:serviceaccount:example-ns:example-daemon"
# The one registered service this stack has, under both halves a service needs: the name the
# registrar account logs in as (BROKER_SERVICE_LOGINS) and the name the pod account is bound to
# (BROKER_SERVICES). A secret tagged owner=<service> is served only when the deployment
# registered that service, so without the second half the policy refuses this stack's
# service-owned secret as owner-tag-malformed on every reread; with it, the pod a daemon enrolls
# under that service gets the secret without asking, as a Legion worker pod gets its daemon's.
SERVICE="legion-daemon"
EXCHANGE_PROJECT="EXAMPLE"
# The session name this stack's pod is registered for, the one it exchanges under.
EXCHANGE_SESSION="example-worker-session"
# The read entry (see the header), named as BROKER_EXCHANGE_READ_ACCOUNTS' own example in
# internal/cipher/config names it. Its account is a pod account of its own, so dev-oidc-issuer
# binds its token to a pod of its own, and it is listed in no other setting: the broker refuses an
# account that both exchange settings name, since one account mints one kind of token.
READ_ACCOUNT="system:serviceaccount:example-ns:example-reader"
READ_SERVICE="example-reader"
READ_SESSION="example-reader-session"
ACCOUNT_TOKEN_FILE="$WORK_DIR/account-token"
LISTENER_TOKEN_FILE="$WORK_DIR/listener-token"
EXCHANGE_TOKEN_FILE="$WORK_DIR/exchange-token"
READ_TOKEN_FILE="$WORK_DIR/read-token"
REGISTRAR_TOKEN_FILE="$WORK_DIR/registrar-token"
POD_TOKEN_FILE="$WORK_DIR/pod-token"
# The broker's own sign-in client at that issuer (BROKER_SIGNIN_*), through which a person signs in
# to the broker's approval page, and the file holding its secret (generated below). The issuer
# takes no domain of its own: it puts each signed-in person's own email domain in their hd claim,
# so the domain the broker holds them to is APPROVER_LOGIN's.
SIGNIN_CLIENT_ID="dev-broker"
SIGNIN_SECRET_FILE="$WORK_DIR/signin-client-secret"
SIGNIN_DOMAIN="${APPROVER_LOGIN#*@}"
# This instance's own database, named from WORK_DIR's mktemp-generated random suffix (already
# unique per invocation). Postgres unquoted identifiers fold to lowercase anyway, but lowercase
# explicitly rather than rely on that.
DB_NAME="dev_broker_$(printf '%s' "${WORK_DIR##*.}" | tr '[:upper:]' '[:lower:]')"
if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ]; then
  POSTGRES_URL="${POSTGRES_SERVER}/${DB_NAME}${POSTGRES_QUERY}"
  POSTGRES_WHERE="the server DEV_BROKER_POSTGRES_URL names"
  admin_psql() { pg_url_run "$DEV_BROKER_POSTGRES_URL" psql -v ON_ERROR_STOP=1 -q "$@"; }
else
  POSTGRES_URL="postgres://postgres:dispatch@127.0.0.1:55432/${DB_NAME}?sslmode=disable"
  POSTGRES_WHERE="the shared $POSTGRES_CONTAINER container, left running ('docker stop $POSTGRES_CONTAINER' tears it down)"
  admin_psql() { docker exec "$POSTGRES_CONTAINER" psql -U postgres -v ON_ERROR_STOP=1 -q "$@"; }
fi

BROKER_PID=""
ISSUER_PID=""
STANDS_PID=""
PROXY_FRONT_PID=""
# Set once this run's own create database succeeded: a run whose create failed (the name taken by
# another run) must never drop that other run's database.
CREATED_DB=false
cleanup() {
  # The broker first, then the front before its proxy, the issuer whose keys it reads and the
  # stand-in GitHub it mints at.
  local pid
  for pid in "$BROKER_PID" "$PROXY_FRONT_PID" "$ISSUER_PID" "$STANDS_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [ "$CREATED_DB" = true ]; then
    # Best-effort: cleanup runs on every exit path and must never itself fail, even if the server
    # is already gone or the drop fails for some other reason. It says which way the drop went, so
    # a database a failed drop leaves behind is named and not called dropped.
    if admin_psql -c "drop database if exists ${DB_NAME};" 2>/dev/null; then
      echo "dev-broker: dropped this instance's database $DB_NAME in $POSTGRES_WHERE" >&2
    else
      echo "dev-broker: could not drop this instance's database $DB_NAME in $POSTGRES_WHERE; drop it by hand" >&2
    fi
  fi
  echo "dev-broker: workdir kept at $WORK_DIR (rm -rf it once its helper has stopped)" >&2
}
trap cleanup EXIT

echo "dev-broker: workdir $WORK_DIR" >&2

if [ -z "${DEV_BROKER_POSTGRES_URL:-}" ]; then
  # --- Postgres: dev-postgres.sh creates, starts or reuses dispatch-pg, and refuses one on an
  # image other than its pinned one before this run creates anything. ---
  echo "dev-broker: Postgres via dev-postgres.sh..." >&2
  "$SCRIPT_DIR/dev-postgres.sh" >&2
  for _ in $(seq 1 30); do
    if docker exec "$POSTGRES_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  if ! docker exec "$POSTGRES_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then
    echo "dev-broker: $POSTGRES_CONTAINER never became ready" >&2
    exit 1
  fi
fi

# --- Create this instance's own isolated database (never the shared "dispatch" database). ---
admin_psql -c "create database ${DB_NAME};"
CREATED_DB=true
echo "dev-broker: created isolated database $DB_NAME" >&2

# A dbname= in DEV_BROKER_POSTGRES_URL's query string overrides the URL's own database, which
# would point the broker, and its migrations, at a database this run neither created nor drops.
if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ]; then
  connected="$(pg_url_run "$POSTGRES_URL" psql -v ON_ERROR_STOP=1 -Atqc 'select current_database()')"
  if [ "$connected" != "$DB_NAME" ]; then
    echo "dev-broker: refused: the broker's database URL connects to '$connected', not this run's $DB_NAME; drop any dbname= from DEV_BROKER_POSTGRES_URL's query string" >&2
    exit 1
  fi
fi
# The banner names the database without any password the URL carries.
DISPLAY_URL="$(pg_url_without_password "$POSTGRES_URL")"

# --- Build the broker, the local issuer, the stand-ins, and the clients a second shell drives it
# with. Each entry is <binary>=<command directory under cmd/>. ---
echo "dev-broker: building broker, dev-oidc-issuer, dev-airlock-stands, agent-secrets, agent-secrets-helper and agent-secrets-devrelay..." >&2
mkdir -p "$BIN_DIR"
for entry in broker=cipherd dev-oidc-issuer=dev-oidc-issuer dev-airlock-stands=dev-proxy-stands \
  agent-secrets=cipher agent-secrets-helper=cipher-helper agent-secrets-devrelay=cipher-devrelay; do
  # DEV_BROKER_BROKER_BIN stands in for the broker's build (the header), so nothing else of the
  # stack differs from a run without it.
  if [ "${entry%%=*}" = broker ] && [ -n "${DEV_BROKER_BROKER_BIN:-}" ]; then
    cp "$DEV_BROKER_BROKER_BIN" "$BROKER_BIN"
    continue
  fi
  ( cd "$ENVOY_DIR" && GOTOOLCHAIN=go1.27.3 go build -o "$BIN_DIR/${entry%%=*}" "./cmd/${entry#*=}" )
done
# The end of the build, which github-profile-check.sh waits for under a bound of its own: on a
# cold build cache the six builds take far longer than the start of what they built.
if [ -n "${DEV_BROKER_BROKER_BIN:-}" ]; then
  echo "dev-broker: built five into $BIN_DIR, and the broker is a copy of DEV_BROKER_BROKER_BIN ($DEV_BROKER_BROKER_BIN)" >&2
else
  echo "dev-broker: built all six into $BIN_DIR" >&2
fi

# await <what> <seconds> <log file> <grep -E pattern> [pid to watch]
# Waits for a line one process logs, and fails the run the moment that process dies instead of
# looping until the wait gives up. scripts/dev-airlock.sh has the same helper, and the two
# scripts' waits read alike.
await() {
  local what="$1" seconds="$2" file="$3" pattern="$4" watch="${5:-}" waited=0
  while [ "$waited" -lt "$((seconds * 2))" ]; do
    if grep -qE "$pattern" "$file" 2>/dev/null; then
      return 0
    fi
    if [ -n "$watch" ] && ! kill -0 "$watch" 2>/dev/null; then
      echo "dev-broker: $what exited during startup (see $file)" >&2
      exit 1
    fi
    sleep 0.5
    waited=$((waited + 1))
  done
  echo "dev-broker: $what did not happen within ${seconds}s (see $file)" >&2
  exit 1
}

# --- The stand-in GitHub the three github-app secrets are minted against. dev-airlock-stands
# serves ten stands and this stack uses the two GitHub ones; the others cost a loopback port each
# and nothing points at them. It writes this run's App key and the certificate authority the
# broker's proxy signs with, and prints one JSON report naming those files, the App id and every
# address, which is where the secrets below, the broker and the banner get theirs. Its output is
# teed as the issuer's and the broker's are, so a human sees its lines live. The github-tls stand
# carries the proxied profiles' host in its certificate, and with DEV_BROKER_PROXIED_GITHUB_IP it
# binds that address's port 443, where the proxy dials it (the header); what each installation
# holds is DEV_BROKER_GITHUB_PERMISSIONS', one --installation-permissions per owner it names. ---
STANDS_ARGS=(--dir "$STANDS_DIR"
  --installation "$GITHUB_OWNER=$GITHUB_INSTALLATION"
  --installation "$GITHUB_CASE_OWNER=$GITHUB_CASE_INSTALLATION"
  --installation "$GITHUB_CASE_OTHER_OWNER=$GITHUB_CASE_OTHER_INSTALLATION"
  --host "github-tls=$GITHUB_PROXIED_HOST")
if [ -n "${DEV_BROKER_PROXIED_GITHUB_IP:-}" ]; then
  STANDS_ARGS+=(--address "github-tls=$DEV_BROKER_PROXIED_GITHUB_IP:443")
fi
read -r -a held_permissions <<<"${DEV_BROKER_GITHUB_PERMISSIONS:-}"
for held in "${held_permissions[@]+"${held_permissions[@]}"}"; do
  STANDS_ARGS+=(--installation-permissions "$held")
done
echo "dev-broker: starting dev-airlock-stands (its GitHub stand mints the github-app secrets)..." >&2
: >"$STANDS_LOG"
"$STANDS_BIN" "${STANDS_ARGS[@]}" \
  >"$STANDS_JSON" 2> >(trap '' TERM INT; exec tee -a "$STANDS_LOG" >&2) &
STANDS_PID=$!
# The stands write their report and then log this line, so once it is in STANDS_LOG the JSON is
# whole and every stand is listening.
await "dev-airlock-stands" 30 "$STANDS_LOG" 'stands listening records=' "$STANDS_PID"
stand() { jq -r "$1" "$STANDS_JSON"; }
# The GitHub stand is reached by the broker's own minter, which trusts the host's roots and
# nothing else, so it is served over plain http on a port the system picked and is named by that
# address. Its TLS twin is named by the proxied profiles' host, which is how a command reaches it.
GITHUB_URL="$(stand '.stands.github.url')"
GITHUB_RECORDS_URL="$(stand '.stands.github.records_url')"
GITHUB_TLS_RECORDS_URL="$(stand '.stands["github-tls"].records_url')"
GITHUB_PROXIED_URL="https://$GITHUB_PROXIED_HOST"
GITHUB_ANSWER_URL="$(stand .github_answer_url)"
GITHUB_APP_ID="$(stand .github_app_id)"
GITHUB_APP_KEY_FILE="$(stand .github_app_key_file)"
GITHUB_COMMIT_SHA="$(stand .github_commit_sha)"
CA_CERT_FILE="$(stand .ca_cert_file)"
CA_KEY_FILE="$(stand .ca_key_file)"

# --- Fake Secrets Manager: one agent-tier secret its owner's sessions get without asking, two
# human-tier secrets its owner approves, one of them a github-app secret, one agent-tier secret
# the registered service owns, which every pod session of that service gets without asking and
# no person's session sees, and the two other github-app secrets, which that same service owns
# for the same reason. The service-owned ones
# are what BROKER_SERVICES makes servable: a secret owned by a service the deployment never
# registered is refused owner-tag-malformed by the policy and reaches nobody. Written with jq, so
# the App's PEM is escaped once and correctly. Under DEV_BROKER_GCP_* the file is empty: the demo
# secrets sit under the AWS prefix, which the GCP store's policy would refuse. ---
if [ -n "$SECRETS_STORE" ]; then
  printf '{"secrets": []}\n' >"$FAKE_SECRETS_FILE"
else
  jq -n \
    --arg prefix "$SECRETS_PREFIX" --arg kms "$SECRETS_KMS_KEY_ARN" \
    --arg approver "$APPROVER_LOGIN" --arg service "$SERVICE" \
    --arg appid "$GITHUB_APP_ID" --rawfile appkey "$GITHUB_APP_KEY_FILE" \
    --arg owner "$GITHUB_OWNER" --arg casepair "$GITHUB_CASE_OWNER" \
    '{secrets: [
       {name: ($prefix + "demo-read-token"), kms_key_id: $kms,
        tags: {owner: $approver, tier: "agent"}, value: "demo-read-token-value"},
       {name: ($prefix + "demo-api-key"), kms_key_id: $kms,
        tags: {owner: $approver, tier: "human"}, value: "demo-api-key-value"},
       {name: ($prefix + "demo-service-token"), kms_key_id: $kms,
        tags: {owner: $service, tier: "agent"}, value: "demo-service-token-value"},
       {name: ($prefix + "demo-github-app"), kms_key_id: $kms,
        tags: {owner: $service, tier: "agent", type: "github-app", "github-owners": $owner},
        value: ({app_id: $appid, private_key: $appkey} | tojson)},
       {name: ($prefix + "demo-github-app-case-pair"), kms_key_id: $kms,
        tags: {owner: $service, tier: "agent", type: "github-app", "github-owners": $casepair},
        value: ({app_id: $appid, private_key: $appkey} | tojson)},
       {name: ($prefix + "demo-github-app-human"), kms_key_id: $kms,
        tags: {owner: $approver, tier: "human", type: "github-app", "github-owners": $owner},
        value: ({app_id: $appid, private_key: $appkey} | tojson)}
     ]}' >"$FAKE_SECRETS_FILE"
fi
chmod 600 "$FAKE_SECRETS_FILE"
echo "$APPROVER_LOGIN" >"$OPERATOR_FILE"

# --- The profiles file (the header): each github-app secret a profile of its own name, and the
# two proxied profiles of DEMO_GITHUB_APP, bound to the github-tls stand's name for GET and HEAD
# alone, so a request of any other method carries no token. The permissions are each profile's
# ask, which the broker cuts to what the installation holds at every mint: DEMO_GITHUB_APP's
# write set leaves out what the stand's own map does not hold (issues, workflows, statuses at
# write), so even a stack driven with no DEV_BROKER_GITHUB_PERMISSIONS names a cut in its release.
# Under DEV_BROKER_GCP_* the file names no profile, as the fake store holds no secret: the demo
# github-app secrets are not there for a profile to name. ---
if [ -n "$SECRETS_STORE" ]; then
  printf '{"profiles": []}\n' >"$PROFILES_FILE"
else
  jq -n \
    --arg app "$GITHUB_SECRET" --arg casepair "$GITHUB_CASE_SECRET" --arg human "$GITHUB_HUMAN_SECRET" \
    --arg read "$GITHUB_READ_PROFILE" --arg adminread "$GITHUB_ADMIN_READ_PROFILE" \
    --arg owner "$GITHUB_OWNER" --arg caseowner "$GITHUB_CASE_OWNER" --arg host "$GITHUB_PROXIED_HOST" \
    '{"actions": "read", "attestations": "read", "checks": "read", "contents": "read",
      "issues": "read", "metadata": "read", "pull_requests": "read", "statuses": "read"} as $reads
     | {"hosts": [$host], "allow": [{"method": "GET", "pathPrefix": "/"}, {"method": "HEAD", "pathPrefix": "/"}]} as $proxy
     | {profiles: [
         {name: $app, github: {accounts: [$owner], permissions: {
           "contents": "write", "issues": "read", "metadata": "read", "pull_requests": "write",
           "statuses": "write", "workflows": "write"}}},
         {name: $read, source: $app,
          github: {accounts: [$owner], permissions: $reads, value: "token"}, proxy: $proxy},
         {name: $adminread, source: $app,
          github: {accounts: [$owner], value: "token", permissions: ($reads + {
            "administration": "read", "members": "read", "organization_administration": "read"})},
          proxy: $proxy},
         {name: $casepair, github: {accounts: [$caseowner], permissions: {"contents": "read", "metadata": "read"}}},
         {name: $human, github: {accounts: [$owner], permissions: {"contents": "read", "metadata": "read"}}}
       ]}' >"$PROFILES_FILE"
fi

# --- The broker's sign-in client's secret, which the issuer and the broker each read from this
# file (neither takes it on its command line or in its environment). Written under umask 077, so
# it is never readable by anyone else, even for a moment. ---
( umask 077 && od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$SIGNIN_SECRET_FILE" )

# --- Start the local issuer, teed like the broker below. It writes every token file before it
# logs its URL, so once that line is in ISSUER_LOG the files are there for devrelay and a pod to
# read, and the URL is the issuer the broker discovers at boot, for ServiceAccount tokens and for
# a person's sign-in alike. Every sign-in it serves signs APPROVER_LOGIN in: nothing here queues
# another person at its POST /_dev/sign-in-as control route. ---
echo "dev-broker: starting dev-oidc-issuer..." >&2
: >"$ISSUER_LOG"
"$ISSUER_BIN" \
  --token "$BROKER_AUDIENCE=$API_ACCOUNT=$ACCOUNT_TOKEN_FILE" \
  --token "$BROKER_AUDIENCE=$LISTENER_ACCOUNT=$LISTENER_TOKEN_FILE" \
  --token "$EXCHANGE_AUDIENCE=$EXCHANGE_ACCOUNT=$EXCHANGE_TOKEN_FILE" \
  --token "$EXCHANGE_AUDIENCE=$READ_ACCOUNT=$READ_TOKEN_FILE" \
  --token "$BROKER_AUDIENCE=$EXCHANGE_ACCOUNT=$POD_TOKEN_FILE" \
  --token "$BROKER_AUDIENCE=$REGISTRAR_ACCOUNT=$REGISTRAR_TOKEN_FILE" \
  --code-flow "$SIGNIN_CLIENT_ID=$SIGNIN_SECRET_FILE=http://127.0.0.1/signin/callback" \
  --sign-in-as "$APPROVER_LOGIN" \
  > >(trap '' TERM INT; exec tee -a "$ISSUER_LOG" >&2) 2>&1 &
ISSUER_PID=$!
ISSUER_URL=""
for _ in $(seq 1 30); do
  if ! kill -0 "$ISSUER_PID" 2>/dev/null; then
    echo "dev-broker: dev-oidc-issuer exited during startup" >&2
    wait "$ISSUER_PID" || true
    exit 1
  fi
  # `|| true`, as for the broker's address below: no line yet is the normal case, not a failure.
  ISSUER_URL="$(grep -o 'issuer listening url=[^ ]*' "$ISSUER_LOG" 2>/dev/null | head -n1 | cut -d= -f2- || true)"
  if [ -n "$ISSUER_URL" ]; then
    break
  fi
  sleep 0.5
done
if [ -z "$ISSUER_URL" ]; then
  echo "dev-broker: dev-oidc-issuer never logged its URL within 15s" >&2
  exit 1
fi

# The uid dev-oidc-issuer bound to the pod it minted POD_TOKEN_FILE for, off its own line for
# that file: `<file> holds <account>'s token for audience <audience> (pod <uid>)`. An enrolment
# of that pod names it, so the banner below prints the one command that takes
# DEMO_SERVICE_TOKEN rather than leaving a reader to find the uid in the issuer's log. The
# issuer writes every token file and logs every such line before it logs its URL, so the line is
# there by now.
POD_UID="$(sed -n "s#^dev-oidc-issuer: ${POD_TOKEN_FILE} holds .* (pod \(.*\))\$#\1#p" "$ISSUER_LOG" | head -n1)"
if [ -z "$POD_UID" ]; then
  echo "dev-broker: dev-oidc-issuer logged no pod uid for $POD_TOKEN_FILE" >&2
  exit 1
fi

# --- The front the broker's proxy is named by (the header), bound before the broker so its
# address is the BROKER_AIRLOCK_PROXY_URL every release names. It reads its one backend from
# PROXY_BACKENDS, which is written once the broker logs the port its proxy bound. ---
echo "dev-broker: starting the proxy's front..." >&2
: >"$PROXY_FRONT_LOG"
: >"$PROXY_BACKENDS"
"$STANDS_BIN" front --listen 127.0.0.1:0 --backends-file "$PROXY_BACKENDS" \
  >"$PROXY_FRONT_JSON" 2> >(trap '' TERM INT; exec tee -a "$PROXY_FRONT_LOG" >&2) &
PROXY_FRONT_PID=$!
await "the proxy's front" 30 "$PROXY_FRONT_LOG" 'front listening addr=' "$PROXY_FRONT_PID"
PROXY_URL="$(jq -r .url "$PROXY_FRONT_JSON")"
# Where the proxy finds the github-tls stand, and that it may dial it there although it is a
# loopback address, when the stands bound it on port 443 (the header); empty is neither.
GITHUB_RESOLVE=""
GITHUB_DIAL_ALLOW=""
if [ -n "${DEV_BROKER_PROXIED_GITHUB_IP:-}" ]; then
  GITHUB_RESOLVE="$GITHUB_PROXIED_HOST=$DEV_BROKER_PROXIED_GITHUB_IP"
  GITHUB_DIAL_ALLOW="$GITHUB_PROXIED_HOST:443"
fi

# --- Start the broker: BROKER_LISTEN_ADDR/BROKER_PUBLIC_URL of port 0 (above); its own log names
# the real address once Listen succeeds. Output is teed to BROKER_LOG (read below) and to this
# script's own stderr, so a human watching this script sees the broker's log lines live (it keeps
# no per-request access log). tee ignores SIGTERM and SIGINT: a signal to this script's process
# group (Ctrl-C, or a kill of the group) reaches the broker and tee together,
# and a tee that died first would leave the broker writing its shutdown into a closed pipe and
# dying of SIGPIPE before it drained. tee ends once the broker exits and closes the pipe. ---
echo "dev-broker: starting broker..." >&2
: >"$BROKER_LOG"
# BROKER_MACHINE_LOGIN_URL is a placeholder: no Dispatch runs here, and devrelay stands in for it
# (it approves a login by its code without a page). BROKER_DRAIN_SECONDS=0 turns the drain off
# (see the header). The BROKER_AIRLOCK_* block is the proxy the profiles file's proxy blocks need
# (the header): its port, the front's URL, the stands' authority, which it signs with and trusts
# upstream, and the github-tls stand's address; KUBERNETES_SERVICE_HOST is empty so a run in a pod
# starts it as the local process it is.
BROKER_DATABASE_URL="$POSTGRES_URL" \
BROKER_PUBLIC_URL="$PUBLIC_URL" \
BROKER_MACHINE_LOGIN_URL="http://127.0.0.1/credentials/machine" \
BROKER_SECRETS_STORE="$SECRETS_STORE" \
BROKER_SECRETS_PREFIX="$SECRETS_PREFIX" \
BROKER_SECRETS_KMS_KEY_ARN="$SECRETS_KMS_KEY_ARN" \
BROKER_SECRETS_KMS_KEY_NAME="$SECRETS_KMS_KEY_NAME" \
BROKER_FAKE_SECRETS_FILE="$FAKE_SECRETS_FILE" \
BROKER_DRAIN_SECONDS=0 \
BROKER_LISTEN_ADDR="$LISTEN_ADDR" \
BROKER_METRICS_LISTEN_ADDR="127.0.0.1:0" \
BROKER_K8S_OIDC_ISSUER="$ISSUER_URL" \
BROKER_K8S_OIDC_AUDIENCE="$BROKER_AUDIENCE" \
BROKER_API_ACCOUNTS="dispatch=$API_ACCOUNT listener=$LISTENER_ACCOUNT" \
BROKER_SIGNIN_ISSUER="$ISSUER_URL" \
BROKER_SIGNIN_CLIENT_ID="$SIGNIN_CLIENT_ID" \
BROKER_SIGNIN_CLIENT_SECRET_FILE="$SIGNIN_SECRET_FILE" \
BROKER_SIGNIN_DOMAIN="$SIGNIN_DOMAIN" \
BROKER_EXCHANGE_K8S_AUDIENCE="$EXCHANGE_AUDIENCE" \
BROKER_EXCHANGE_ACCOUNTS="$EXCHANGE_ACCOUNT=$EXCHANGE_PROJECT=$APPROVER_LOGIN" \
BROKER_EXCHANGE_READ_ACCOUNTS="$READ_ACCOUNT=$READ_SERVICE=$READ_SESSION" \
BROKER_SERVICE_LOGINS="$SERVICE=$REGISTRAR_ACCOUNT" \
BROKER_POD_REGISTRARS="$REGISTRAR_ACCOUNT=$EXCHANGE_ACCOUNT" \
BROKER_SERVICES="$SERVICE=$EXCHANGE_ACCOUNT" \
BROKER_AIRLOCK_TEST_GITHUB_API_BASE="$GITHUB_URL" \
BROKER_PROFILES_FILE="$PROFILES_FILE" \
BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" \
BROKER_AIRLOCK_PROXY_URL="$PROXY_URL" \
BROKER_AIRLOCK_CA_CERT_FILE="$CA_CERT_FILE" \
BROKER_AIRLOCK_CA_KEY_FILE="$CA_KEY_FILE" \
BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE="$CA_CERT_FILE" \
BROKER_AIRLOCK_TEST_RESOLVE="$GITHUB_RESOLVE" \
BROKER_AIRLOCK_TEST_DIAL_ALLOW="$GITHUB_DIAL_ALLOW" \
KUBERNETES_SERVICE_HOST='' \
"$BROKER_BIN" > >(trap '' TERM INT; exec tee -a "$BROKER_LOG" >&2) 2>&1 &
BROKER_PID=$!

# --- Both readiness loops below poll this instance's own process; fail loudly the moment it
# dies instead of looping until either wait's own timeout finally gives up. ---
die_if_broker_exited() {
  if ! kill -0 "$BROKER_PID" 2>/dev/null; then
    echo "dev-broker: broker exited during startup" >&2
    wait "$BROKER_PID" || true
    exit 1
  fi
}

# --- Wait for one of the broker's own "... listening" log lines and set LOGGED_ADDR to the
# address it names: only once a bind has actually succeeded does the broker report an address, so
# this can only ever name this instance's own socket, never a different, already-running one. The
# broker logs one JSON object per line, and the address is that line's addr field; a line still
# being written parses as nothing, so half an address is never read as the whole one. It sets a
# variable rather than printing, since die_if_broker_exited's exit has to end this script and
# not a command substitution's subshell. ---
await_logged_addr() { # <message>
  LOGGED_ADDR=""
  for _ in $(seq 1 30); do
    die_if_broker_exited
    # `|| true`: under this script's set -e, a jq that failed would end the script here, since
    # the assignment takes the command substitution's status, before the loop could sleep and
    # read again. Before the broker has logged the line, jq prints nothing and exits 0.
    LOGGED_ADDR="$(jq -Rnr --arg message "$1" '[inputs | fromjson? | select(.message == $message) | .addr][0] // empty' "$BROKER_LOG" 2>/dev/null || true)"
    if [ -n "$LOGGED_ADDR" ]; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}
if ! await_logged_addr 'broker listening'; then
  echo "dev-broker: broker never logged its bound address within 15s" >&2
  exit 1
fi
BOUND_ADDR="$LOGGED_ADDR"
PUBLIC_URL="http://${BOUND_ADDR}"
LISTEN_ADDR="$BOUND_ADDR"
# The metrics port, bound by the same boot after the API's and logged just after it.
if ! await_logged_addr 'broker metrics listening'; then
  echo "dev-broker: broker never logged its metrics address within 15s" >&2
  exit 1
fi
METRICS_URL="http://${LOGGED_ADDR}/metrics"
# The proxy's port, which the broker binds with the API's and logs between the two lines above,
# so by now its line is there; the front is pointed at it, and rereads its backends file when it
# changes. The broker binds it only when a profile of its profiles file has a proxy block, which
# none under DEV_BROKER_GCP_* has (the profiles file's header), so the line is waited for only
# then; a broker that logged none for such a file never bound the proxy its profiles need.
PROXY_BANNER="none  (no profile of the profiles file has a proxy block, so the broker binds no proxy port)"
if jq -e 'any(.profiles[]; .proxy != null)' "$PROFILES_FILE" >/dev/null; then
  if ! await_logged_addr 'airlock proxy listening'; then
    echo "dev-broker: broker never logged its proxy's address within 15s" >&2
    exit 1
  fi
  PROXY_ADDR="$LOGGED_ADDR"
  printf '%s\n' "$PROXY_ADDR" >"$PROXY_BACKENDS"
  PROXY_BANNER="$PROXY_URL  (BROKER_AIRLOCK_PROXY_URL, a front over the broker's own proxy port $PROXY_ADDR)"
fi

ready=false
for _ in $(seq 1 30); do
  die_if_broker_exited
  if curl -sf "${PUBLIC_URL}/healthz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.5
done
if [ "$ready" != true ]; then
  echo "dev-broker: broker did not become healthy within 15s" >&2
  exit 1
fi

# --- Register this stack's pod for the session name it exchanges under. The broker binds a
# session name to the listed account and never to a pod, so two pods of one account are one
# identity there; what keeps them apart is this row, which the deployment's daemon writes as it
# starts a pod. Here the registrar account stands in for that daemon: it logs in as the service
# (BROKER_SERVICE_LOGINS), BROKER_POD_REGISTRARS lists it for the pod's account, and the body
# carries the pod's own broker-audience token, the one a daemon enrolls that pod with. Without
# it the exchange below is refused invalid_grant, naming the session. Both tokens stay out of
# every command line: the body reaches curl on standard input from a here-string, which the
# shell writes before curl starts, so no pipeline's status can hide curl's, and the header from a
# file curl reads. ---
registration=$(printf '{"pod_token": "%s", "session": "%s"}' "$(cat "$POD_TOKEN_FILE")" "$EXCHANGE_SESSION")
if ! curl -sf -X POST "${PUBLIC_URL}/v1/oauth/exchange/pod-sessions" \
  -H @<(printf 'Authorization: Bearer %s' "$(cat "$REGISTRAR_TOKEN_FILE")") \
  -H 'Content-Type: application/json' \
  --data @- <<<"$registration" >/dev/null; then
  echo "dev-broker: registering the exchange pod for $EXCHANGE_SESSION failed" >&2
  exit 1
fi
echo "dev-broker: registered the exchange pod to write under $EXCHANGE_SESSION" >&2

# Two banner lines that depend on a setting, worded here: bash reads a quote inside a ${...}
# of a here-document as unclosed.
PROXIED_GITHUB_WHERE="nowhere, since DEV_BROKER_PROXIED_GITHUB_IP is unset"
if [ -n "${DEV_BROKER_PROXIED_GITHUB_IP:-}" ]; then
  PROXIED_GITHUB_WHERE="at $DEV_BROKER_PROXIED_GITHUB_IP:443"
fi
GITHUB_HOLDS="the stand's own map for each installation"
if [ -n "${DEV_BROKER_GITHUB_PERMISSIONS:-}" ]; then
  GITHUB_HOLDS="$DEV_BROKER_GITHUB_PERMISSIONS"
fi

cat <<EOF

dev-broker: ready.

  export AGENT_SECRETS_URL=$PUBLIC_URL
  export AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$ACCOUNT_TOKEN_FILE
  export AGENT_SECRETS_APPROVER=$APPROVER_LOGIN
  export AGENT_SECRETS_HELPER_SOCK=$WORK_DIR/helper.sock
  export AGENT_SECRETS_OPERATOR_FILE=$OPERATOR_FILE
  export AGENT_SECRETS_MACHINE_FILE=$WORK_DIR/machine.json
  export AGENT_SECRETS_STATE_FILE=$WORK_DIR/sessions.json
  export DEV_BROKER_DIR=$WORK_DIR
  export DEV_BROKER_METRICS_URL=$METRICS_URL
  export DEV_BROKER_POD_UID=$POD_UID
  export DEV_BROKER_SERVICE=$SERVICE
  export DEV_BROKER_GITHUB_URL=$GITHUB_URL
  export DEV_BROKER_GITHUB_RECORDS_URL=$GITHUB_RECORDS_URL
  export DEV_BROKER_GITHUB_ANSWER_URL=$GITHUB_ANSWER_URL
  export DEV_BROKER_GITHUB_SECRET=$GITHUB_SECRET
  export DEV_BROKER_GITHUB_OWNER=$GITHUB_OWNER
  export DEV_BROKER_GITHUB_INSTALLATION=$GITHUB_INSTALLATION
  export DEV_BROKER_GITHUB_CASE_SECRET=$GITHUB_CASE_SECRET
  export DEV_BROKER_GITHUB_CASE_OWNER=$GITHUB_CASE_OWNER
  export DEV_BROKER_GITHUB_CASE_INSTALLATIONS=$GITHUB_CASE_INSTALLATION,$GITHUB_CASE_OTHER_INSTALLATION
  export DEV_BROKER_GITHUB_HUMAN_SECRET=$GITHUB_HUMAN_SECRET
  export DEV_BROKER_GITHUB_READ_PROFILE=$GITHUB_READ_PROFILE
  export DEV_BROKER_GITHUB_ADMIN_READ_PROFILE=$GITHUB_ADMIN_READ_PROFILE
  export DEV_BROKER_GITHUB_PROXIED_URL=$GITHUB_PROXIED_URL
  export DEV_BROKER_GITHUB_TLS_RECORDS_URL=$GITHUB_TLS_RECORDS_URL
  export DEV_BROKER_GITHUB_COMMIT=$GITHUB_COMMIT_SHA
  export DEV_BROKER_PROFILES_FILE=$PROFILES_FILE
  export DEV_BROKER_PROXY_URL=$PROXY_URL
  export PATH=$BIN_DIR:\$PATH

  binaries:            $BIN_DIR  (broker, dev-oidc-issuer, dev-airlock-stands, agent-secrets, agent-secrets-helper, agent-secrets-devrelay)
  secret store:        $STORE_LINE
  fake secrets file:   $FAKE_SECRETS_FILE  (each secret's tags and value, for confirming a released grant)
  approver login:      $APPROVER_LOGIN (whom the issuer signs in to approve a secret, and whom devrelay names deciding a machine login)
  database:            $DISPLAY_URL  (this instance's own; created and dropped by this script)
  metrics:             $METRICS_URL  (GET: the broker's Prometheus metrics, on a port of its own, BROKER_METRICS_LISTEN_ADDR)
  token issuer:        $ISSUER_URL  (dev-oidc-issuer, logging to $ISSUER_LOG)
  sign-in:             $ISSUER_URL  (the broker's client $SIGNIN_CLIENT_ID, hd $SIGNIN_DOMAIN; secret in $SIGNIN_SECRET_FILE)
  caller token:        $ACCOUNT_TOKEN_FILE  ($API_ACCOUNT, the caller "dispatch")
  listener token:      $LISTENER_TOKEN_FILE  ($LISTENER_ACCOUNT, the caller "listener")
  exchange token:      $EXCHANGE_TOKEN_FILE  ($EXCHANGE_ACCOUNT's pod, audience $EXCHANGE_AUDIENCE)
  pod token:           $POD_TOKEN_FILE  (the same pod, audience $BROKER_AUDIENCE: what its registration named it by)
  read token:          $READ_TOKEN_FILE  ($READ_ACCOUNT's pod, audience $EXCHANGE_AUDIENCE: exchanges under $READ_SESSION for a token of the scope read alone, which Dispatch serves on every GET route it classes as a read and answers 403 READ_ONLY_TOKEN on every other route)
  registrar token:     $REGISTRAR_TOKEN_FILE  ($REGISTRAR_ACCOUNT, the service login "$SERVICE", listed to register $EXCHANGE_ACCOUNT's pods: BROKER_POD_REGISTRARS=$REGISTRAR_ACCOUNT=$EXCHANGE_ACCOUNT)
  registered service:  $SERVICE  (BROKER_SERVICE_LOGINS=$REGISTRAR_ACCOUNT, BROKER_SERVICES=$EXCHANGE_ACCOUNT)
  github stand:        $GITHUB_URL  (BROKER_AIRLOCK_TEST_GITHUB_API_BASE; plain http, since the broker's minter trusts this host's own roots and nothing else)
  github records:      $GITHUB_RECORDS_URL  (GET: every call that stand was sent, as JSON)
  proxied github:      $GITHUB_PROXIED_URL  (the github-tls stand, the same GitHub over TLS, which a command reaches through the proxy: $PROXIED_GITHUB_WHERE; its calls at $GITHUB_TLS_RECORDS_URL)
  github answers:      $GITHUB_ANSWER_URL  (POST {"mode": "ordinary"|"refuse"|"refuse-repository"|"refuse-permissions"|"widen"|"fail-once", "repository": "<name>"}: what each token request is answered from then on; the repository is refuse-repository's own, and fail-once answers the next one 503, which the broker retries, and the rest as ordinary)
  github holds:        $GITHUB_HOLDS  (what the App's installations hold, DEV_BROKER_GITHUB_PERMISSIONS; every commit status is for $GITHUB_COMMIT_SHA)
  profiles file:       $PROFILES_FILE  (BROKER_PROFILES_FILE: the profile each github-app secret is served as, and the two proxied ones)
  proxy:               $PROXY_BANNER
  stand-in files:      $STANDS_DIR  (this run's GitHub App key and the stands' certificate authority; the whole report in $STANDS_JSON, its log in $STANDS_LOG)

  One agent-tier secret (DEMO_READ_TOKEN, granted without asking) and two human-tier secrets
  (DEMO_API_KEY, and the github-app secret $GITHUB_HUMAN_SECRET, whose github-owners tag names
  $GITHUB_OWNER), all three owned by $APPROVER_LOGIN, whose sessions ask for them and who
  approves each release of the two human-tier ones, and three agent-tier secrets owned by the
  registered service $SERVICE, which a pod session enrolled under that service gets without
  asking and a person's session never sees: DEMO_SERVICE_TOKEN, a stored value, and the two
  github-app secrets $GITHUB_SECRET, whose github-owners tag names $GITHUB_OWNER, and
  $GITHUB_CASE_SECRET, whose
  tag names $GITHUB_CASE_OWNER, on which the App is installed twice under logins that differ
  only in case ($GITHUB_CASE_OWNER and $GITHUB_CASE_OTHER_OWNER), so a release of it holds two
  installations. A github-app request reaches a person only through $GITHUB_HUMAN_SECRET: a
  secret the service owns is granted to that service's own sessions and denied to every other,
  and put to nobody. A grant lasts for the --ttl its request names, else 15 minutes for
  the human-tier ones and 12 hours for the agent-tier ones.

  Each github-app secret is served as the profile of its own name in the profiles file, whose
  release is the JSON of its installations, and $GITHUB_SECRET is the source of two more:
  $GITHUB_READ_PROFILE and $GITHUB_ADMIN_READ_PROFILE, each $GITHUB_OWNER's bare token behind the proxy, for GET
  and HEAD at $GITHUB_PROXIED_URL alone. The broker asks GitHub for each profile's permissions cut
  to what the installation holds, names what the cut left out in the release (github_left_out)
  and in its log, and refuses a release whose cut leaves nothing but metadata of a profile that
  asks for more (PROFILE_MINT_REFUSED).
  Drive it with agent-secrets and agent-secrets-helper in another shell;
  press Ctrl-C here to stop the broker, its issuer, its proxy's front and the stand-ins.

  This script enrols no pod session of its own. To take DEMO_SERVICE_TOKEN or a github-app
  secret, enrol one as the deployment's daemon does — the registrar token above, and the pod
  token of the account BROKER_SERVICES binds — from packages/envoy:

    KEYS=\$(mktemp -d)
    dev-airlock-stands enroll-pod --url $PUBLIC_URL --service $SERVICE \\
      --token-file $REGISTRAR_TOKEN_FILE --pod-token-file $POD_TOKEN_FILE --pod-uid $POD_UID \\
      --thumbprint "\$(agent-secrets keygen --out "\$KEYS")" | jq -r .enrollment_id >"\$KEYS/enrollment"
    AGENT_SECRETS_KEY_DIR=\$KEYS agent-secrets DEMO_SERVICE_TOKEN -- printenv DEMO_SERVICE_TOKEN
    AGENT_SECRETS_KEY_DIR=\$KEYS agent-secrets $GITHUB_SECRET -- \\
      sh -c 'printf %s "\$$GITHUB_SECRET" | jq ".installations | map_values(.installation_id)"'
    AGENT_SECRETS_KEY_DIR=\$KEYS agent-secrets $GITHUB_READ_PROFILE -- \\
      sh -c 'printf "Authorization: Bearer %s" "\$$GITHUB_READ_PROFILE" | curl -fsS -H @- $GITHUB_PROXIED_URL/repos/$GITHUB_OWNER/proofs'

  The last one holds a placeholder in \$$GITHUB_READ_PROFILE, for which the proxy puts the token in the
  request, and it reaches the GitHub stand only where DEV_BROKER_PROXIED_GITHUB_IP gave that
  stand port 443 of a loopback address.

  To approve a pending request for DEMO_API_KEY, open the approve_url agent-secrets prints for
  it (${PUBLIC_URL}/approve/<record>) in a browser: the broker sends you to sign in at
  dev-oidc-issuer, which signs you in as $APPROVER_LOGIN with no page to
  fill in, and brings you back to the record's page, where you Approve or Deny it.

  The broker lists two callers (BROKER_API_ACCOUNTS) and trusts dev-oidc-issuer's ServiceAccount
  tokens (BROKER_K8S_OIDC_ISSUER); the issuer rewrites each token file before its token lapses.
  The caller "dispatch" reaches every named-caller route; the caller "listener" reaches
  POST /v1/oauth/introspect alone, so the listener token on an approval or revocation route is
  403 ACCOUNT_LACKS_POWER naming that caller and that route. agent-secrets-devrelay presents the
  caller token (AGENT_SECRETS_ACCOUNT_TOKEN_FILE) to approve or deny a machine login by its code
  (machine-approve) as Dispatch's server does. The RFC 8693 token exchange (POST
  /v1/oauth/token) takes the exchange token as a pod's subject token, with a DPoP proof of the
  caller's own key, and mints an access token that writes $EXCHANGE_PROJECT for $APPROVER_LOGIN.
  That pod exchanges under $EXCHANGE_SESSION and under no other name: this script registered it
  for that one (POST /v1/oauth/exchange/pod-sessions, the registrar token on the Authorization
  header and the pod token in the body, admitted because BROKER_POD_REGISTRARS pairs the
  registrar's account with the pod's), and any other name is refused invalid_grant naming it.
  Registering the same pod for a second name there is all it takes to add one; a registrar no
  entry of that list pairs with the pod's account is refused 403 POD_REGISTRAR_NOT_LISTED,
  naming both accounts. The read token
  exchanges the same way for a token that only reads: service $READ_SERVICE, session
  $READ_SESSION, no person and no project (BROKER_EXCHANGE_READ_ACCOUNTS). Its entry fixes that
  session, so its pod needs no registration and a registration of it is refused 409
  SESSION_FIXED. To drive it against a Dispatch, start one from packages/dispatch with
  DISPATCH_E2E_AGENT_SECRETS_URL=$PUBLIC_URL and
  DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$ACCOUNT_TOKEN_FILE before bun run dispatch:local,
  and run e2e/read-token-live.ts as its header says.
EOF

wait "$BROKER_PID"
