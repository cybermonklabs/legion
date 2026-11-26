#!/usr/bin/env bash
# packages/envoy/scripts/dev-airlock.sh
#
# The whole local surface the airlock's checks run on, in one command, with no container runtime
# and no cluster. It starts, and prints the exports for:
#
#   - Postgres, dev-oidc-issuer and that script's own broker, by running scripts/dev-broker.sh
#     and reading what it prints. Everything this script needs of a local stack but the airlock
#     itself is there already - the six built binaries, dev-airlock-stands among them, the local
#     token issuer with a pod's, a registrar's and two listed callers' tokens, the broker's own
#     sign-in client, and the Postgres server - so this script builds and starts none of it a
#     second time. What that costs is three processes of dev-broker.sh's that this script never
#     points at: its broker, on a database and a profiles file of its own, the front of that
#     broker's proxy, and the cmd/dev-proxy-stands it starts for its own github-app secrets.
#     What it buys is that the identities, the token files and
#     the issuer are the ones dev-broker.sh documents, and that a change there reaches both
#     scripts. The airlock's own brokers get a database of their own (below), so they never
#     share a policy or a secret store with that broker.
#   - cmd/dev-proxy-stands: the two HTTPS services S and T, which record every request they
#     get; the stand-in GitHub and Google endpoints a github-app and a google-login secret mint
#     at; the stand-in Microsoft token endpoint the azure kind exchanges at, which also plays
#     the resource host an access token is spent at; a stand-in AWS service that verifies
#     Signature Version 4; the three the model judge calls - a model endpoint, a Dispatch
#     holding one open issue and one done one, and a Compute Engine metadata server, which also
#     answers the machine's access token; and the anthropic kind's three - its Admin API at
#     api.anthropic.com, a recording HTTPS service like S, IAM's generateIdToken, where an
#     anthropic-federation secret's Google identity token is minted, and Anthropic's token
#     exchange, where that token is traded for an sk-ant-oat01- token. It writes the certificate
#     authority the proxy signs each bound host with, the AWS key pair its own stand verifies,
#     and the GitHub App key its GitHub stand mints for; this script reads all of it out of the
#     JSON report that command prints.
#   - The model judge, on every broker: the six BROKER_JUDGE_* settings pointed at the model
#     and Dispatch stands, and GCE_METADATA_HOST at the metadata stand, so a human-tier request
#     whose key's judge tag gives the judge seconds of it is decided by the model rather than by
#     a person.
#     AIRLOCK_JUDGE_DISPATCH_URL, when set, points the judge at that Dispatch instead of the
#     Dispatch stand (a local stack's, whose access log then shows each judge read and the
#     User-Agent it names); the judge presents the stand's token either way, so such a
#     Dispatch may refuse the read, and the line is logged all the same.
#   - A profiles file (BROKER_PROFILES_FILE) in the kinds form and a fake Secrets Manager (the
#     file BROKER_FAKE_SECRETS_FILE names). The file's kinds say where a credential of each kind
#     may go: stest, a stored secret bound to S; scoped, bound to S in authorization and x-key
#     with GET /read alone; ghapp and glogin, a github-app and a google-login secret's minted
#     token bound to S; azure, kind azure's exchange at the Microsoft stand-in and the stand-in
#     resource host; aws, a stored AWS key pair signed again for the AWS stand-in; judged, the
#     judge's five human-tier secrets bound to S; and anthropic, exactly as the spec writes it.
#     Each secret is behind the proxy because its proxy tag in the fake store names one of them.
#     The judge's five: two whose judge tags give the judge 900 and 600 seconds, each with a
#     guidance tag of its own, one with no judge tag, one owned by nobody that anyone approves,
#     and one tagged an administrator's whose judge tag gives the judge 900 seconds of it all the
#     same, which the judge must refuse because the tag says a person approves every release of
#     it. The two anthropic-federation secrets are an administrator's, each bound to a Google
#     account of its own (BROKER_SERVICE_ACCOUNT_SECRETS): AIRLOCK_ANTHROPIC_NARROWED carries
#     proxy-narrowed=true and one profile with ANTHROPIC_ADMIN_HDO's three allow rows, and
#     AIRLOCK_ANTHROPIC_KIND has no profile, so the kind's allow alone bounds it. The file's
#     profiles are the github-app secret's own, with the github block every github-app profile
#     carries, that narrowed secret's, and AIRLOCK_PLAIN_GITHUB, a profile with no proxy whose
#     release is the value itself: its source is a second github-app secret of the same App with
#     no proxy tag, since a profile takes its source's tag, a github-app secret no profile names
#     is refused (github-app-no-profile), and the broker reads profiles at boot alone, so a
#     check's control that needs a GitHub token released to the command finds one here rather
#     than adding one at run time.
#   - TWO broker processes, each with the whole BROKER_AIRLOCK_* block, both on one database and
#     each on three ports of its own, its API, proxy and metrics ports (--one-broker starts one).
#   - One TCP front over both brokers' API ports and one over both proxy ports
#     (cmd/dev-proxy-stands front), so a client reaches either process. Each new connection
#     goes to the next broker in turn; round-robin per connection is all the checks ask for,
#     since the two brokers share one database and either answers any call. The front is Go
#     because a Legion issue pod has neither socat nor nginx, and nginx passes no CONNECT.
#   - One enrolled pod session: a service login at POST /v1/launcher-credentials/token under the
#     stack's legion-daemon registrar account, then POST /v1/enrollments of kind pod signed with
#     a launcher proof, which is how the Legion daemon enrols a worker pod. agent-secrets-helper
#     cannot be the identity here: it pins the far end of its socket with SO_PEERPIDFD, which a
#     Legion issue pod's gVisor kernel does not have. The enrolment id is written beside the
#     session key, so AGENT_SECRETS_KEY_DIR names the directory and `agent-secrets self` answers
#     kind: pod.
#   - That session's renewer, `agent-secrets renew --pod-token-file`, for as long as the stack
#     runs, as the Legion worker shim runs one for each pod (internal/shim, startRenewer). The
#     broker ends a session whose lease (BROKER_LEASE_SECONDS, 900 seconds by default) passes
#     unrenewed, and every grant of it with it, so a check that holds a grant for longer would
#     have its calls refused 407 midway. Each renewal sends the pod token dev-oidc-issuer
#     rewrites in place, as a kubelet rewrites a projected token.
#
# Every bound host but one is a name of this stack's own (s.airlock.test and the rest), which no
# resolver answers; the one is api.anthropic.com, the anthropic kind's host as the spec writes
# it. BROKER_AIRLOCK_TEST_RESOLVE tells the proxy where each one is, the Admin API stand for
# api.anthropic.com, so no call of this stack reaches Anthropic, and
# BROKER_AIRLOCK_TEST_DIAL_ALLOW lets it dial that one loopback address and port, while every
# other loopback, private and link-local address stays refused - which is what check k4 reads.
# Each of them listens on port 443 of a loopback address of its own (127.0.0.2 and up): the
# airlock refuses a request whose Host header is not its CONNECT target, and an HTTP client
# writes the port into that header for every port but 443, so a bound host anywhere else is
# refused "host header mismatch" before it can prove anything. A machine that will not let this
# user bind 443 cannot run this stack; the stands then fail to start and say so.
#
# The airlock's secrets are owned by a registered service (BROKER_SERVICES), and the pod this
# script enrols runs as that service's account, so its requests for them are granted at once. A
# pod session has no operator, so the azure-login secret, which the broker releases its
# projected token for only at human tier with a person's approval, and the two
# anthropic-federation secrets, which the broker serves at human tier alone, wait: the exports
# name the approver and the sign-in URL for them, and the broker takes that decision on its own
# page alone.
#
# Everything this script makes lives under ${TMPDIR:-/tmp}, as dev-broker.sh's work directory
# now does: in a Legion pod /tmp is the container's own small disk, and one run of the checks
# fills it.
#
# Options:
#   --keep-values     BROKER_AIRLOCK_TEST_KEEP_VALUES=true (check k5's negative control: the
#                     proxy keeps a grant's values across a reread and past a minted token's
#                     margin)
#   --no-kind         BROKER_AIRLOCK_TEST_NO_KIND=true (check k6's negative control: every proxy
#                     block reads as having no kind, so the azure exchange and the AWS signing
#                     are off)
#   --idle-seconds N  BROKER_AIRLOCK_IDLE_SECONDS (check k9's control sets 60)
#   --one-broker      start one broker rather than two, each front over that one
#   --judge-skip-checks
#                     BROKER_JUDGE_TEST_SKIP_CHECKS=true (the judge's negative control: its
#                     checks before the model are off, so a request one of them would hold back
#                     is put to the model anyway)
#   --help            print this and exit
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# psql is given a database URL without its password, and the password in PGPASSWORD.
# shellcheck source-path=SCRIPTDIR source=lib/pg-url.sh
. "$SCRIPT_DIR/lib/pg-url.sh"

usage() {
  cat <<'EOF'
usage: dev-airlock.sh [--keep-values] [--no-kind] [--idle-seconds N] [--one-broker]
                      [--judge-skip-checks]

Starts the airlock's whole local stack - dev-broker.sh's Postgres, issuer and broker, the
stand-in services, a certificate authority, a profiles file, two brokers behind one API front and
one proxy front, and an enrolled pod session - and prints the exports a check needs.

  --keep-values     BROKER_AIRLOCK_TEST_KEEP_VALUES=true, check k5's negative control
  --no-kind         BROKER_AIRLOCK_TEST_NO_KIND=true, check k6's negative control
  --idle-seconds N  BROKER_AIRLOCK_IDLE_SECONDS, between 10 and 3600; check k9's control sets 60
  --one-broker      start one broker rather than two
  --judge-skip-checks
                    BROKER_JUDGE_TEST_SKIP_CHECKS=true, the model judge's negative control
  --help            print this
EOF
}

KEEP_VALUES=false
NO_KIND=false
IDLE_SECONDS=300
BROKER_COUNT=2
JUDGE_SKIP_CHECKS=false
while [ $# -gt 0 ]; do
  case "$1" in
    --keep-values) KEEP_VALUES=true ;;
    --no-kind) NO_KIND=true ;;
    --idle-seconds)
      if [ $# -lt 2 ] || ! [[ "${2:-}" =~ ^[0-9]+$ ]]; then
        echo "dev-airlock: --idle-seconds wants a number of seconds" >&2
        exit 2
      fi
      IDLE_SECONDS="$2"
      if [ "$IDLE_SECONDS" -lt 10 ] || [ "$IDLE_SECONDS" -gt 3600 ]; then
        echo "dev-airlock: --idle-seconds is between 10 and 3600, the bounds BROKER_AIRLOCK_IDLE_SECONDS takes" >&2
        exit 2
      fi
      shift
      ;;
    --one-broker) BROKER_COUNT=1 ;;
    --judge-skip-checks) JUDGE_SKIP_CHECKS=true ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "dev-airlock: unexpected argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

# --- This run's own directory, under TMPDIR as dev-broker.sh's is. ---
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/agent-secrets-airlock.XXXXXX")"
STANDS_DIR="$WORK_DIR/stands"
STANDS_JSON="$WORK_DIR/stands.json"
STANDS_LOG="$WORK_DIR/stands.log"
DEV_BROKER_LOG="$WORK_DIR/dev-broker.log"
PROFILES_FILE="$WORK_DIR/profiles.json"
FAKE_SECRETS_FILE="$WORK_DIR/fake-secrets.json"
AZURE_TOKEN_FILE="$WORK_DIR/azure-identity-token"
JUDGE_DISPATCH_TOKEN_FILE="$WORK_DIR/judge-dispatch-token"
KEY_DIR="$WORK_DIR/session-key"
ENROLL_JSON="$WORK_DIR/enrollment.json"
RENEW_LOG="$WORK_DIR/renew.log"
API_FRONT_JSON="$WORK_DIR/api-front.json"
API_FRONT_LOG="$WORK_DIR/api-front.log"
API_BACKENDS="$WORK_DIR/api-backends"
PROXY_FRONT_JSON="$WORK_DIR/proxy-front.json"
PROXY_FRONT_LOG="$WORK_DIR/proxy-front.log"
PROXY_BACKENDS="$WORK_DIR/proxy-backends"

# The namespace and the key this run's fake secrets sit under. The key is an example ARN in AWS's
# documentation account, and nothing here calls AWS; a secret on any other key is refused by the
# policy, so the two must agree.
SECRETS_PREFIX="example/agent-secrets/"
SECRETS_KMS_KEY_ARN="arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"
# The one service name this stack uses: the name the registrar account logs in as
# (BROKER_SERVICE_LOGINS), the name the enrolled pod's own account is bound to
# (BROKER_SERVICES), and the owner of the airlock's secrets. A pod's session acts for a service
# only when both halves name that one service, as a deployment's daemon and its worker pods do,
# so a request of the pod's for one of those secrets is granted at once.
SERVICE="legion-daemon"
# The names a check asks for, the stand-in hosts the kinds their proxy tags name bind them to, and
# where each one listens. Each host but api.anthropic.com is a .test name no resolver answers:
# BROKER_AIRLOCK_TEST_RESOLVE tells the proxy where it is and BROKER_AIRLOCK_TEST_DIAL_ALLOW lets
# it dial that one address.
#
# Every bound host listens on port 443 of a loopback address of its own, rather than on one
# address and a port the system picks. The airlock refuses a request whose Host header is not
# its CONNECT target, and a client writes the port in that header for every port but 443, so a
# bound host on any other port is refused "host header mismatch" before anything else it could
# prove. 127.0.0.0/8 is a whole /8 of loopback addresses, so each stand takes one.
S_HOST="s.airlock.test"
S_ADDRESS="127.0.0.2:443"
T_HOST="t.airlock.test"
T_ADDRESS="127.0.0.3:443"
MICROSOFT_HOST="microsoft.airlock.test"
AZURE_RESOURCE_HOST="azure-resource.airlock.test"
MICROSOFT_ADDRESS="127.0.0.4:443"
AWS_HOST="aws.airlock.test"
AWS_ADDRESS="127.0.0.5:443"
# The anthropic kind's one host is Anthropic's own name, as the spec's kind binds it, and the
# Admin API stand answers it here; it is never dialled anywhere else (the header).
ANTHROPIC_HOST="api.anthropic.com"
ANTHROPIC_ADDRESS="127.0.0.6:443"
GITHUB_OWNER="acme-org"
GITHUB_INSTALLATION=7
DEMO_SECRET="AIRLOCK_DEMO_TOKEN"
SCOPED_SECRET="AIRLOCK_SCOPED_TOKEN"
GITHUB_SECRET="AIRLOCK_GITHUB_APP"
# The profile with no proxy (the header): the value released to the command, for a control. Its
# source is a github-app secret of the same App with no proxy tag, because a profile is behind
# the proxy when its source's proxy tag names a kind.
PLAIN_GITHUB_PROFILE="AIRLOCK_PLAIN_GITHUB"
PLAIN_GITHUB_SOURCE="AIRLOCK_PLAIN_GITHUB_APP"
# What every GitHub profile of this stack asks for, which the stand's own map of each
# installation holds, so its cut leaves nothing out.
GITHUB_PERMISSIONS='{"contents": "read", "metadata": "read"}'
GOOGLE_SECRET="AIRLOCK_GOOGLE_LOGIN"
AZURE_SECRET="AIRLOCK_AZURE_LOGIN"
AWS_SECRET="AIRLOCK_AWS_KEY"
# The two anthropic-federation secrets of the anthropic kind's check (k47), each an
# administrator's: human tier, admin "true", a max-grant of an hour (the kind's max_grant), and a
# requesters list naming the approver, who owns both, and this stack's service, which the
# enrolled pod acts for (policy.RequesterRule holds the type to its owner named once). Each is
# bound to its own Google account (BROKER_SERVICE_ACCOUNT_SECRETS), whose identity token the iam
# stand mints, and names that account in its stored value with the Anthropic ids the exchange
# stand records. NARROWED carries proxy-narrowed=true and one profile with the three allow rows
# of ANTHROPIC_ADMIN_HDO's profile in the deployment repository; KIND carries neither, so its refusals come
# from the kind's allow alone.
ANTHROPIC_NARROWED_SECRET="AIRLOCK_ANTHROPIC_NARROWED"
ANTHROPIC_KIND_SECRET="AIRLOCK_ANTHROPIC_KIND"
ANTHROPIC_NARROWED_ACCOUNT="anthropic-narrowed@airlock-stack.iam.gserviceaccount.com"
ANTHROPIC_KIND_ACCOUNT="anthropic-kind@airlock-stack.iam.gserviceaccount.com"
ANTHROPIC_MAX_GRANT="1h"
# The three allow rows the anthropic kind and the narrowed secret's profile share: GET, POST and
# DELETE under /v1/organizations/, the Admin API.
ANTHROPIC_ALLOW='[{"method": "GET", "pathPrefix": "/v1/organizations/"}, {"method": "POST", "pathPrefix": "/v1/organizations/"}, {"method": "DELETE", "pathPrefix": "/v1/organizations/"}]'
# The five the model judge is asked about. Each is human tier, so each waits for a person
# unless the judge decides it first, and each is a stored value with no type: what the judge is
# about is who may approve a request, and a type's minting is another check's subject.
# JUDGED_SECRET is the one whose judge tag gives the judge seconds of it; UNJUDGED_SECRET the one
# with no judge tag, which must still wait for its person; SHARED_SECRET the one
# nobody owns, which anyone signed in approves and which the judge therefore approves in
# anyone's place; ADMIN_SECRET the administrator's credential, which carries the same judge
# tag as JUDGED_SECRET and which the judge must refuse all the same - the stack writes
# exactly the tags the defect allowed, so a check can see the refusal come from the broker
# rather than from the absence of a judge.
#
# ADMIN_SECRET carries two tags the others do not, because the policy refuses an admin secret
# without them (policy.TagRules): a requesters list, which must name this stack's own service
# for the enrolled pod to be allowed to ask at all, and a max-grant of an hour or less, since
# an administrator's grant is one somebody reviews while it is live.
JUDGED_SECRET="AIRLOCK_JUDGED_TOKEN"
UNJUDGED_SECRET="AIRLOCK_UNJUDGED_TOKEN"
SHARED_SECRET="AIRLOCK_SHARED_TOKEN"
ADMIN_SECRET="AIRLOCK_ADMIN_TOKEN"
# SECOND_JUDGED_SECRET is a second judge key of the approver's, bound to S as JUDGED_SECRET is,
# with a shorter judge tag and guidance of its own. A request for it beside JUDGED_SECRET is two
# judge keys, which the judge decides together, each against its own guidance, for at most the
# shorter of the two judge tags.
SECOND_JUDGED_SECRET="AIRLOCK_SECOND_JUDGED_TOKEN"
ADMIN_MAX_GRANT="1h"
# The seconds the judge tags let the judge decide of the judge keys, which is also the longest
# lifetime a request for one of them may sign, and the lifetime a request naming none gets:
# JUDGE_SECONDS for JUDGED_SECRET, SHARED_SECRET and ADMIN_SECRET, SECOND_JUDGE_SECONDS for
# SECOND_JUDGED_SECRET.
JUDGE_SECONDS=900
SECOND_JUDGE_SECONDS=600
# The guidance tags of JUDGED_SECRET and of SECOND_JUDGED_SECRET: the key owner's instruction,
# which the stand-in model is shown with every request for the key. Each is printed
# single-quoted in the exports below and written into the fake store's JSON as it is, and each
# holds only what an AWS Secrets Manager tag value may (letters, digits, spaces and
# `_ . : / = + - @`), as the guidance of a real key on that store must.
JUDGE_GUIDANCE="grant for the stack check runs that name the run in the reason"
SECOND_JUDGE_GUIDANCE="grant for the stack release job"
# What the judge is configured with: the model id its verdicts are recorded under
# (record.JudgeActor writes "judge:<id>"), the protocol the model stand speaks, and the
# audience of the identity token the broker mints for it at the metadata stand.
JUDGE_MODEL="the-stand-in-judge-model"
JUDGE_MODEL_PROTOCOL="anthropic-messages"
JUDGE_MODEL_AUDIENCE="https://model.airlock.test"
# The bearer the judge presents to the Dispatch stand, which takes any bearer and refuses a
# read that carries none.
JUDGE_DISPATCH_TOKEN="the-stand-in-dispatch-service-account-token"
# The values the fake store holds for the two stored secrets. They are this script's own strings
# and open nothing; a check compares what a stand-in was sent against them by length and prefix.
DEMO_VALUE="the-real-airlock-demo-token-9f3a7c"
SCOPED_VALUE="the-real-airlock-scoped-token-4b1e"
# The values of the judge's own five secrets, which are stored values like the two above: what
# a judged grant releases is the same sort of credential, read back by a check from the stand-in
# its kind, judged, binds it to.
JUDGED_VALUE="the-real-airlock-judged-token-7c2d"
UNJUDGED_VALUE="the-real-airlock-unjudged-token-5a8b"
SHARED_VALUE="the-real-airlock-shared-token-2e6f"
ADMIN_VALUE="the-real-airlock-admin-token-8d41"
SECOND_JUDGED_VALUE="the-real-airlock-second-judged-token-6b19"
# The session token of the stack's AWS key pair. One string for both halves of the stand: the
# fake store hands it to the broker as part of the stored value, and --aws-session-token tells
# the stand-in AWS service to require exactly it, so a call the proxy signed again verifies.
AWS_SESSION_TOKEN="the-stand-in-aws-session-token"
# A federated token in the shape the azure-login minter reads: a JWT whose exp claim is
# 4102444800 (2100-01-01). It is signed by nobody and the stand-in Microsoft endpoint takes any
# assertion.
AZURE_FEDERATED_TOKEN="eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl"

# DEV_BROKER_POSTGRES_URL is read exactly as dev-broker.sh reads it, and checked here before
# anything is created, so a malformed one leaves nothing behind. dev-broker.sh checks it again
# for its own database.
if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ] && ! [[ "$DEV_BROKER_POSTGRES_URL" =~ ^(postgres(ql)?://[^/?]+)/[^/?]*(\?.*)?$ ]]; then
  echo "dev-airlock: DEV_BROKER_POSTGRES_URL must be postgres://<user>@<host>:<port>/<database>" >&2
  exit 2
fi
POSTGRES_SERVER="${BASH_REMATCH[1]:-}"
POSTGRES_QUERY="${BASH_REMATCH[3]:-}"

# The brokers' own database, named from this run's own mktemp suffix, created and dropped here.
# It is never dev-broker.sh's: that broker reads another profiles file and another secret store,
# so a policy of this stack's must not be a policy of its.
DB_NAME="dev_airlock_$(printf '%s' "${WORK_DIR##*.}" | tr '[:upper:]' '[:lower:]')"
POSTGRES_CONTAINER="dispatch-pg"
if [ -n "${DEV_BROKER_POSTGRES_URL:-}" ]; then
  POSTGRES_URL="${POSTGRES_SERVER}/${DB_NAME}${POSTGRES_QUERY}"
  POSTGRES_WHERE="the server DEV_BROKER_POSTGRES_URL names"
  admin_psql() { pg_url_run "$DEV_BROKER_POSTGRES_URL" psql -v ON_ERROR_STOP=1 -q "$@"; }
else
  POSTGRES_URL="postgres://postgres:dispatch@127.0.0.1:55432/${DB_NAME}?sslmode=disable"
  POSTGRES_WHERE="the shared $POSTGRES_CONTAINER container dev-broker.sh started"
  admin_psql() { docker exec "$POSTGRES_CONTAINER" psql -U postgres -v ON_ERROR_STOP=1 -q "$@"; }
fi

DEV_BROKER_PID=""
RENEW_PID=""
STANDS_PID=""
API_FRONT_PID=""
PROXY_FRONT_PID=""
BROKER_PIDS=()
CREATED_DB=false

cleanup() {
  # This stack's own processes first, the session's renewer before the brokers it renews at,
  # then dev-broker.sh, whose own EXIT trap stops its broker and its issuer and drops its
  # database.
  local pid
  for pid in "$RENEW_PID" "${BROKER_PIDS[@]+"${BROKER_PIDS[@]}"}" "$API_FRONT_PID" "$PROXY_FRONT_PID" "$STANDS_PID" "$DEV_BROKER_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [ "$CREATED_DB" = true ]; then
    # Best-effort: cleanup runs on every exit path and must never itself fail. It says which way
    # the drop went, so a database a failed drop leaves behind is named and not called dropped.
    if admin_psql -c "drop database if exists ${DB_NAME};" 2>/dev/null; then
      echo "dev-airlock: dropped this instance's database $DB_NAME in $POSTGRES_WHERE" >&2
    else
      echo "dev-airlock: could not drop this instance's database $DB_NAME in $POSTGRES_WHERE; drop it by hand" >&2
    fi
  fi
  echo "dev-airlock: workdir kept at $WORK_DIR (rm -rf it once nothing reads it)" >&2
}
trap cleanup EXIT

echo "dev-airlock: workdir $WORK_DIR" >&2

# --- dev-broker.sh: Postgres, dev-oidc-issuer, the six binaries and its own broker. Its output
# is teed to a log of this run's own and to this script's stderr, so a human sees it live and
# this script can read the identities and addresses out of it. tee ignores SIGTERM and SIGINT so
# a signal to the process group reaches dev-broker.sh and tee together. ---
echo "dev-airlock: starting dev-broker.sh (Postgres, dev-oidc-issuer, its broker)..." >&2
: >"$DEV_BROKER_LOG"
bash "$SCRIPT_DIR/dev-broker.sh" > >(trap '' TERM INT; exec tee -a "$DEV_BROKER_LOG" >&2) 2>&1 &
DEV_BROKER_PID=$!

# await <what> <seconds> <log file> <grep -E pattern> [pid to watch]
# Waits for a line one process logs, and fails the run the moment that process dies instead of
# looping until the wait gives up.
await() {
  local what="$1" seconds="$2" file="$3" pattern="$4" watch="${5:-}" waited=0
  while [ "$waited" -lt "$((seconds * 2))" ]; do
    if grep -qE "$pattern" "$file" 2>/dev/null; then
      return 0
    fi
    if [ -n "$watch" ] && ! kill -0 "$watch" 2>/dev/null; then
      echo "dev-airlock: $what exited during startup (see $file)" >&2
      exit 1
    fi
    sleep 0.5
    waited=$((waited + 1))
  done
  echo "dev-airlock: $what did not happen within ${seconds}s (see $file)" >&2
  exit 1
}

await "dev-broker.sh" 300 "$DEV_BROKER_LOG" '^dev-broker: ready\.$' "$DEV_BROKER_PID"

# --- What dev-broker.sh left behind. Its work directory is the one export this script needs by
# name; the files under it are the ones its banner documents, and each is checked here rather
# than assumed, so a rename there fails loudly instead of enrolling nothing. ---
# `|| true`: under pipefail a grep that finds nothing would end the whole run before the
# caller's own check could say what was missing.
first_match() { grep -oEm1 "$1" "$2" 2>/dev/null | head -n1 || true; }
# logged_addr is the addr field of the first line of a broker's log whose message is the one
# named. The broker logs one JSON object per line, and a line still being written parses as
# nothing, so half an address is never read as the whole one.
logged_addr() { jq -Rnr --arg message "$1" '[inputs | fromjson? | select(.message == $message) | .addr][0] // empty' "$2" 2>/dev/null || true; }

BASE_DIR="$(first_match 'export DEV_BROKER_DIR=[^[:space:]]+' "$DEV_BROKER_LOG" | cut -d= -f2-)"
if [ -z "$BASE_DIR" ] || [ ! -d "$BASE_DIR" ]; then
  echo "dev-airlock: dev-broker.sh printed no DEV_BROKER_DIR" >&2
  exit 1
fi
BASE_BIN_DIR="$BASE_DIR/bin"
BROKER_BIN="$BASE_BIN_DIR/broker"
AGENT_SECRETS_BIN="$BASE_BIN_DIR/agent-secrets"
STANDS_BIN="$BASE_BIN_DIR/dev-airlock-stands"
ACCOUNT_TOKEN_FILE="$BASE_DIR/account-token"
LISTENER_TOKEN_FILE="$BASE_DIR/listener-token"
REGISTRAR_TOKEN_FILE="$BASE_DIR/registrar-token"
POD_TOKEN_FILE="$BASE_DIR/pod-token"
SIGNIN_SECRET_FILE="$BASE_DIR/signin-client-secret"
for needed in "$BROKER_BIN" "$AGENT_SECRETS_BIN" "$STANDS_BIN" "$ACCOUNT_TOKEN_FILE" \
  "$LISTENER_TOKEN_FILE" "$REGISTRAR_TOKEN_FILE" "$POD_TOKEN_FILE" "$SIGNIN_SECRET_FILE"; do
  if [ ! -s "$needed" ]; then
    echo "dev-airlock: dev-broker.sh left no $needed; this script reads its work directory" >&2
    exit 1
  fi
done

APPROVER_LOGIN="$(first_match 'export AGENT_SECRETS_APPROVER=[^[:space:]]+' "$DEV_BROKER_LOG" | cut -d= -f2-)"
ISSUER_URL="$(first_match 'issuer listening url=[^[:space:]]+' "$DEV_BROKER_LOG" | cut -d= -f2-)"
if [ -z "$APPROVER_LOGIN" ] || [ -z "$ISSUER_URL" ]; then
  echo "dev-airlock: dev-broker.sh printed no approver or no issuer URL" >&2
  exit 1
fi
# dev-broker.sh's sign-in client and the domain it holds a person to, which is the approver's
# own: the issuer puts each signed-in person's email domain in their hd claim.
SIGNIN_CLIENT_ID="dev-broker"
SIGNIN_DOMAIN="${APPROVER_LOGIN#*@}"

# dev-oidc-issuer's own line for each token file it wrote:
#   dev-oidc-issuer: <file> holds <account>'s token for audience <audience> (pod <uid>)
# It is where the accounts, the broker's audience and the pod's uid come from, so this stack's
# brokers list exactly the identities that issuer minted for.
issuer_field() {
  local line
  line="$(grep -m1 -F "dev-oidc-issuer: $1 holds " "$DEV_BROKER_LOG" || true)"
  if [ -z "$line" ]; then
    echo "dev-airlock: dev-oidc-issuer wrote no line for $1" >&2
    exit 1
  fi
  case "$2" in
    account) printf '%s\n' "$line" | sed -E "s/.* holds (.*)'s token for audience .*/\1/" ;;
    audience) printf '%s\n' "$line" | sed -E 's/.* for audience ([^ ]+) \(pod .*/\1/' ;;
    uid) printf '%s\n' "$line" | sed -E 's/.*\(pod ([^)]*)\)$/\1/' ;;
  esac
}
API_ACCOUNT="$(issuer_field "$ACCOUNT_TOKEN_FILE" account)"
LISTENER_ACCOUNT="$(issuer_field "$LISTENER_TOKEN_FILE" account)"
REGISTRAR_ACCOUNT="$(issuer_field "$REGISTRAR_TOKEN_FILE" account)"
POD_ACCOUNT="$(issuer_field "$POD_TOKEN_FILE" account)"
BROKER_AUDIENCE="$(issuer_field "$POD_TOKEN_FILE" audience)"
POD_UID="$(issuer_field "$POD_TOKEN_FILE" uid)"

# --- This stack's own database, on the server dev-broker.sh is already using. ---
admin_psql -c "create database ${DB_NAME};"
CREATED_DB=true
echo "dev-airlock: created isolated database $DB_NAME" >&2
# The banner names the database without any password the URL carries.
DISPLAY_URL="$(pg_url_without_password "$POSTGRES_URL")"

# --- The stand-ins. Each HTTPS stand's certificate carries the name a kind binds to it, since
# the proxy verifies the name it dialled against the certificate the stand presents. The
# Microsoft stand carries the resource host's name too: it answers the tenant's OpenID
# configuration document, which is the first thing az reads of an authority, and a token
# request at /{tenant}/oauth2/v2.0/token, and leaves every other request to its own body, so
# one process plays the authority and the service an access token is spent at. The anthropic
# stand carries api.anthropic.com, the Admin API a call behind the proxy reaches. ---
echo "dev-airlock: starting dev-airlock-stands..." >&2
: >"$STANDS_LOG"
"$STANDS_BIN" --dir "$STANDS_DIR" \
  --address "s=$S_ADDRESS" --host "s=$S_HOST" \
  --address "t=$T_ADDRESS" --host "t=$T_HOST" \
  --address "microsoft=$MICROSOFT_ADDRESS" \
  --host "microsoft=$MICROSOFT_HOST" --host "microsoft=$AZURE_RESOURCE_HOST" \
  --address "aws=$AWS_ADDRESS" --host "aws=$AWS_HOST" \
  --address "anthropic=$ANTHROPIC_ADDRESS" --host "anthropic=$ANTHROPIC_HOST" \
  --installation "$GITHUB_OWNER=$GITHUB_INSTALLATION" \
  --google-lifetime 7m \
  --aws-session-token "$AWS_SESSION_TOKEN" \
  --ask-answered-by "$APPROVER_LOGIN" \
  >"$STANDS_JSON" 2> >(trap '' TERM INT; exec tee -a "$STANDS_LOG" >&2) &
STANDS_PID=$!
await "dev-airlock-stands" 60 "$STANDS_LOG" 'stands listening records=' "$STANDS_PID"

stand() { jq -r "$1" "$STANDS_JSON"; }
CA_CERT_FILE="$(stand .ca_cert_file)"
CA_KEY_FILE="$(stand .ca_key_file)"
RECORDS_URL="$(stand .records_url)"
AWS_KEY_ID="$(stand .aws_key_id)"
AWS_SECRET_KEY="$(stand .aws_secret_key)"
GITHUB_APP_ID="$(stand .github_app_id)"
GITHUB_APP_KEY_FILE="$(stand .github_app_key_file)"
# The GitHub and Google stands are reached by the broker itself, not through the proxy, so
# they keep a port the system picked on 127.0.0.1 and are named by address; so are the two an
# anthropic-federation secret is minted at, and the three the model judge calls.
GITHUB_URL="$(stand '.stands.github.url')"
GOOGLE_URL="$(stand '.stands.google.url')"
ANTHROPIC_TOKEN_URL="$(stand '.stands["anthropic-token"].url')/v1/oauth/token"
IAM_URL="$(stand '.stands.iam.url')"
ANTHROPIC_ANSWER_URL="$(stand .anthropic_answer_url)"
ANTHROPIC_TOKENS_URL="$(stand .anthropic_tokens_url)"
IAM_TOKENS_URL="$(stand .iam_tokens_url)"
MODEL_URL="$(stand '.stands.model.url')"
MODEL_ANSWER_URL="$(stand .model_answer_url)"
DISPATCH_URL="$(stand '.stands.dispatch.url')"
JUDGE_DISPATCH_URL="${AIRLOCK_JUDGE_DISPATCH_URL:-$DISPATCH_URL}"
# GCE_METADATA_HOST is a host and no scheme, so the metadata stand is named by its address.
METADATA_URL="$(stand '.stands.metadata.url')"
METADATA_ADDRESS="$(stand '.stands.metadata.address')"
DISPATCH_OPEN_ISSUE="$(stand .dispatch_open_issue)"
DISPATCH_DONE_ISSUE="$(stand .dispatch_done_issue)"
DISPATCH_ANSWERED_ASK="$(stand .dispatch_answered_ask)"
# Every bound host is reached by its name on 443, which is how its Host header matches its
# CONNECT target, so no URL here carries a port.
S_URL="https://${S_HOST}"
T_URL="https://${T_HOST}"
MICROSOFT_URL="https://${MICROSOFT_HOST}"
AZURE_RESOURCE_URL="https://${AZURE_RESOURCE_HOST}"
AWS_URL="https://${AWS_HOST}"
ANTHROPIC_URL="https://${ANTHROPIC_HOST}"
# What the proxy may dial although it resolves to loopback, and where each name is. The two
# together are what check k4 reads: in one run S and T on loopback are reachable and the
# broker's own loopback port is still refused, which no switch for loopback could give.
DIAL_ALLOW="${S_HOST}:443 ${T_HOST}:443 ${MICROSOFT_HOST}:443 ${AZURE_RESOURCE_HOST}:443 ${AWS_HOST}:443 ${ANTHROPIC_HOST}:443"
RESOLVE="${S_HOST}=${S_ADDRESS%:*} ${T_HOST}=${T_ADDRESS%:*} ${MICROSOFT_HOST}=${MICROSOFT_ADDRESS%:*} ${AZURE_RESOURCE_HOST}=${MICROSOFT_ADDRESS%:*} ${AWS_HOST}=${AWS_ADDRESS%:*} ${ANTHROPIC_HOST}=${ANTHROPIC_ADDRESS%:*}"

# --- The profiles file, in the kinds form. Its kinds say where a credential of each kind may go
# and in which headers; a secret is behind the proxy when its proxy tag in the fake store below
# names one of them, and who decides a request for it is its key's own judge tag there. The
# anthropic kind is the spec's own: its host, its one header, its variable, its way, its longest
# grant and its three allow rows. The other kinds are this stack's test kinds, each with the
# hosts and headers the stack's secrets had before kinds, none with a variable, since a check
# reads each placeholder in the variable named as its secret, and none with a max_grant, since a
# kind that sets one refuses every secret of it with no max-grant tag. The profiles are the
# github-app secret's own, with the github block every GitHub profile carries; AIRLOCK_PLAIN_GITHUB
# (the header); and the narrowed anthropic secret's, whose own allow is the three rows of
# ANTHROPIC_ADMIN_HDO's profile, which its proxy-narrowed tag requires it to have. No profile has
# a proxy object: this stack holds no dpop capability. ---
cat >"$PROFILES_FILE" <<EOF
{"kinds": [
  {"name": "stest", "hosts": ["$S_HOST"], "ways": ["swap"]},
  {"name": "scoped", "hosts": ["$S_HOST"], "headers": ["authorization", "x-key"], "ways": ["swap"],
   "allow": [{"method": "GET", "pathPrefix": "/read"}]},
  {"name": "ghapp", "hosts": ["$S_HOST"], "ways": ["mint"]},
  {"name": "glogin", "hosts": ["$S_HOST"], "ways": ["mint"]},
  {"name": "azure", "hosts": ["$MICROSOFT_HOST", "$AZURE_RESOURCE_HOST"], "ways": ["exchange"]},
  {"name": "aws", "hosts": ["$AWS_HOST"], "ways": ["sign"]},
  {"name": "judged", "hosts": ["$S_HOST"], "ways": ["swap"]},
  {"name": "anthropic", "hosts": ["$ANTHROPIC_HOST"], "headers": ["Authorization"], "variables": ["ANTHROPIC_ADMIN_TOKEN"],
   "ways": ["mint"], "max_grant": "$ANTHROPIC_MAX_GRANT", "allow": $ANTHROPIC_ALLOW}
],
"profiles": [
  {"name": "$GITHUB_SECRET", "github": {"accounts": ["$GITHUB_OWNER"], "permissions": $GITHUB_PERMISSIONS}},
  {"name": "$PLAIN_GITHUB_PROFILE", "source": "$PLAIN_GITHUB_SOURCE",
   "github": {"accounts": ["$GITHUB_OWNER"], "permissions": $GITHUB_PERMISSIONS}},
  {"name": "$ANTHROPIC_NARROWED_SECRET", "allow": $ANTHROPIC_ALLOW}
]}
EOF

# --- The fake Secrets Manager the brokers read, written with jq so the GitHub App's PEM and
# each object value are escaped once and correctly. Every value here is this stack's own and
# opens nothing. Every secret but one is behind the proxy by its proxy tag (policy.TagProxy),
# which names its kind; the one is the plain github-app source, which a control needs released
# as its value. The two stored secrets, the github-app and the google-login secret are owned
# by the service the enrolled pod runs as, so a request for one is granted at once; the
# azure-login secret is human tier and owned by the approver, because the broker releases its
# projected Azure token only for a human-tier name a person approved. The judge's five are
# human tier too, so each waits for a person: four of them the approver's own, and one
# owned by nobody (the owner tag "shared", policy.OwnerShared), which anyone signed in
# approves and which the judge therefore approves in anyone's place. Four of them carry a judge
# tag (policy.TagJudge) and two a guidance tag (policy.TagGuidance): the judge decides those
# four, the administrator's among them by its tag but never in fact. One of the approver's is
# the approver's administrator's credential (admin: "true"), which carries the requesters list
# and the hour's cap that tag demands of a secret: a list naming the approver and this stack's
# service, since the enrolled pod acts for that service and a list no requester is on is
# refused before owner and tier are read at all. The two anthropic-federation secrets carry the
# same administrator's tags, the type's own, and the anthropic kind's proxy tag; the narrowed one
# also proxy-narrowed=true (policy.TagProxyNarrowed), which is what lets its profile's allow be
# in force. ---
jq -n \
  --arg prefix "$SECRETS_PREFIX" --arg kms "$SECRETS_KMS_KEY_ARN" \
  --arg service "$SERVICE" --arg approver "$APPROVER_LOGIN" \
  --arg demo "$DEMO_VALUE" --arg scoped "$SCOPED_VALUE" \
  --arg judged "$JUDGED_VALUE" --arg unjudged "$UNJUDGED_VALUE" --arg shared "$SHARED_VALUE" \
  --arg admin "$ADMIN_VALUE" --arg second "$SECOND_JUDGED_VALUE" --arg maxgrant "$ADMIN_MAX_GRANT" \
  --arg judge "$JUDGE_SECONDS" --arg secondjudge "$SECOND_JUDGE_SECONDS" \
  --arg guidance "$JUDGE_GUIDANCE" --arg secondguidance "$SECOND_JUDGE_GUIDANCE" \
  --arg appid "$GITHUB_APP_ID" --rawfile appkey "$GITHUB_APP_KEY_FILE" \
  --arg owner "$GITHUB_OWNER" \
  --arg awskey "$AWS_KEY_ID" --arg awssecret "$AWS_SECRET_KEY" --arg awssession "$AWS_SESSION_TOKEN" \
  --arg anthropicgrant "$ANTHROPIC_MAX_GRANT" \
  --arg narrowedaccount "$ANTHROPIC_NARROWED_ACCOUNT" --arg kindaccount "$ANTHROPIC_KIND_ACCOUNT" \
  '{secrets: [
     {name: ($prefix + "airlock-demo-token"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent", proxy: "stest"}, value: $demo},
     {name: ($prefix + "airlock-scoped-token"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent", proxy: "scoped"}, value: $scoped},
     {name: ($prefix + "airlock-github-app"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent", type: "github-app", "github-owners": $owner, proxy: "ghapp"},
      value: ({app_id: $appid, private_key: $appkey} | tojson)},
     {name: ($prefix + "airlock-plain-github-app"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent", type: "github-app", "github-owners": $owner},
      value: ({app_id: $appid, private_key: $appkey} | tojson)},
     {name: ($prefix + "airlock-google-login"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent", type: "google-login", proxy: "glogin"},
      value: ({type: "authorized_user", client_id: "the-stand-in-client",
               client_secret: "the-stand-in-client-secret",
               refresh_token: "the-stand-in-refresh-token"} | tojson)},
     {name: ($prefix + "airlock-azure-login"), kms_key_id: $kms,
      tags: {owner: $approver, tier: "human", type: "azure-login", proxy: "azure"},
      value: ({tenant_id: "the-stand-in-tenant", client_id: "the-stand-in-application"} | tojson)},
     {name: ($prefix + "airlock-aws-key"), kms_key_id: $kms,
      tags: {owner: $service, tier: "agent", proxy: "aws"},
      value: ({access_key_id: $awskey, secret_access_key: $awssecret,
               session_token: $awssession} | tojson)},
     {name: ($prefix + "airlock-judged-token"), kms_key_id: $kms,
      tags: {owner: $approver, tier: "human", judge: $judge, guidance: $guidance, proxy: "judged"}, value: $judged},
     {name: ($prefix + "airlock-unjudged-token"), kms_key_id: $kms,
      tags: {owner: $approver, tier: "human", proxy: "judged"}, value: $unjudged},
     {name: ($prefix + "airlock-shared-token"), kms_key_id: $kms,
      tags: {owner: "shared", tier: "human", judge: $judge, proxy: "judged"}, value: $shared},
     {name: ($prefix + "airlock-admin-token"), kms_key_id: $kms,
      tags: {owner: $approver, tier: "human", admin: "true",
             requesters: ($approver + " " + $service), "max-grant": $maxgrant, judge: $judge, proxy: "judged"},
      value: $admin},
     {name: ($prefix + "airlock-second-judged-token"), kms_key_id: $kms,
      tags: {owner: $approver, tier: "human", judge: $secondjudge, guidance: $secondguidance, proxy: "judged"}, value: $second},
     {name: ($prefix + "airlock-anthropic-narrowed"), kms_key_id: $kms,
      tags: {owner: $approver, tier: "human", admin: "true", type: "anthropic-federation",
             requesters: ($approver + " " + $service), "max-grant": $anthropicgrant,
             proxy: "anthropic", "proxy-narrowed": "true"},
      value: ({organization_id: "00000000-0000-4000-8000-0000000000a1", federation_rule_id: "fdrl_01airlocknarrowed",
               service_account_id: "svac_01airlocknarrowed", service_account: $narrowedaccount,
               workspace_id: "wrkspc_01airlocknarrowed"} | tojson)},
     {name: ($prefix + "airlock-anthropic-kind"), kms_key_id: $kms,
      tags: {owner: $approver, tier: "human", admin: "true", type: "anthropic-federation",
             requesters: ($approver + " " + $service), "max-grant": $anthropicgrant, proxy: "anthropic"},
      value: ({organization_id: "00000000-0000-4000-8000-0000000000a2", federation_rule_id: "fdrl_01airlockkind",
               service_account_id: "svac_01airlockkind", service_account: $kindaccount,
               workspace_id: "wrkspc_01airlockkind"} | tojson)}
   ]}' >"$FAKE_SECRETS_FILE"
chmod 600 "$FAKE_SECRETS_FILE"
printf '%s' "$AZURE_FEDERATED_TOKEN" >"$AZURE_TOKEN_FILE"
chmod 600 "$AZURE_TOKEN_FILE"
printf '%s' "$JUDGE_DISPATCH_TOKEN" >"$JUDGE_DISPATCH_TOKEN_FILE"
chmod 600 "$JUDGE_DISPATCH_TOKEN_FILE"

# --- The two fronts, bound before the brokers exist. Each broker's BROKER_PUBLIC_URL and
# BROKER_AIRLOCK_PROXY_URL must name the front at boot - a session's proof is verified against
# the public URL, so both brokers have to be one address - and a broker's own ports are only
# known once it has bound them. So each front binds first and reads its backends from a file
# this script writes once the brokers report their addresses. ---
# start_front sets FRONT_PID, rather than leaving the caller to read $!, because the wait for
# the front's own log line sits between the two.
FRONT_PID=""
start_front() {
  local name="$1" listen_json="$2" log="$3" backends="$4"
  : >"$log"
  : >"$backends"
  "$STANDS_BIN" front --listen 127.0.0.1:0 --backends-file "$backends" \
    >"$listen_json" 2> >(trap '' TERM INT; exec tee -a "$log" >&2) &
  FRONT_PID=$!
  await "the $name front" 30 "$log" 'front listening addr=' "$FRONT_PID"
}
echo "dev-airlock: starting the API and proxy fronts..." >&2
start_front "API" "$API_FRONT_JSON" "$API_FRONT_LOG" "$API_BACKENDS"
API_FRONT_PID="$FRONT_PID"
start_front "proxy" "$PROXY_FRONT_JSON" "$PROXY_FRONT_LOG" "$PROXY_BACKENDS"
PROXY_FRONT_PID="$FRONT_PID"
API_FRONT_URL="$(jq -r .url "$API_FRONT_JSON")"
PROXY_FRONT_URL="$(jq -r .url "$PROXY_FRONT_JSON")"

# --- The brokers. Both read one database, one profiles file and one fake store, and each binds an
# API port, a proxy port and a metrics port the system picks (BROKER_*_LISTEN_ADDR of port 0), so
# two of them never collide. BROKER_DRAIN_SECONDS=0 as dev-broker.sh does: the five-second default
# exists to outlast a Kubernetes EndpointSlice propagation nothing here has.
#
# BROKER_AIRLOCK_TEST_AZURE_AUTHORITY makes the Microsoft stand-in the token authority the
# azure exchange is held to, in place of login.microsoftonline.com: the proxy refuses an
# exchange at any other host, and the stand-in is where this stack's az signs in.
#
# BROKER_SERVICE_ACCOUNT_SECRETS binds each anthropic-federation secret to its own Google account,
# and BROKER_AIRLOCK_TEST_IAM_CREDENTIALS_URL and BROKER_AIRLOCK_TEST_ANTHROPIC_TOKEN_URL point
# that type's minter at the iam and anthropic-token stands: it reads the account's identity token
# at the first, spending the access token the metadata stand answers, and trades it at the
# second, as it mints at Google and Anthropic in production.
#
# Each also gets the model judge: the six BROKER_JUDGE_* settings, which are all-or-none, and
# GCE_METADATA_HOST. The credential the judge presents to its model endpoint is the broker's
# own Google machine identity, read from the metadata server at call time; pointing
# GCE_METADATA_HOST at the metadata stand rather than adding a test-only setting that hands the
# broker a token leaves that path exactly as production runs it, so what this stack exercises
# is the code a deployment runs and not a shortcut around it.
#
# And each is started with KUBERNETES_SERVICE_HOST empty. Kubernetes sets it in every container,
# so a run in a Legion pod would hand it to both brokers, and a broker with an airlock that sees it
# is one deployed in a cluster: it refuses to start unless BROKER_AIRLOCK_CLUSTER_RANGES holds that
# cluster's Service range, which a pod cannot read. These brokers are local processes on loopback,
# so they start as a local run does, and a BROKER_AIRLOCK_CLUSTER_RANGES the caller exports still
# reaches them. ---
start_broker() {
  local n="$1" log="$2"
  : >"$log"
  BROKER_DATABASE_URL="$POSTGRES_URL" \
  BROKER_PUBLIC_URL="$API_FRONT_URL" \
  BROKER_LISTEN_ADDR="127.0.0.1:0" \
  BROKER_METRICS_LISTEN_ADDR="127.0.0.1:0" \
  BROKER_MACHINE_LOGIN_URL="http://127.0.0.1/credentials/machine" \
  BROKER_SECRETS_PREFIX="$SECRETS_PREFIX" \
  BROKER_SECRETS_KMS_KEY_ARN="$SECRETS_KMS_KEY_ARN" \
  BROKER_FAKE_SECRETS_FILE="$FAKE_SECRETS_FILE" \
  BROKER_DRAIN_SECONDS=0 \
  BROKER_K8S_OIDC_ISSUER="$ISSUER_URL" \
  BROKER_K8S_OIDC_AUDIENCE="$BROKER_AUDIENCE" \
  BROKER_API_ACCOUNTS="dispatch=$API_ACCOUNT listener=$LISTENER_ACCOUNT" \
  BROKER_SERVICE_LOGINS="$SERVICE=$REGISTRAR_ACCOUNT" \
  BROKER_SERVICES="$SERVICE=$POD_ACCOUNT" \
  BROKER_SIGNIN_ISSUER="$ISSUER_URL" \
  BROKER_SIGNIN_CLIENT_ID="$SIGNIN_CLIENT_ID" \
  BROKER_SIGNIN_CLIENT_SECRET_FILE="$SIGNIN_SECRET_FILE" \
  BROKER_SIGNIN_DOMAIN="$SIGNIN_DOMAIN" \
  BROKER_AZURE_TOKEN_FILE="$AZURE_TOKEN_FILE" \
  BROKER_AZURE_LOGIN_SECRET="$AZURE_SECRET" \
  BROKER_SERVICE_ACCOUNT_SECRETS="$ANTHROPIC_NARROWED_SECRET=$ANTHROPIC_NARROWED_ACCOUNT $ANTHROPIC_KIND_SECRET=$ANTHROPIC_KIND_ACCOUNT" \
  BROKER_PROFILES_FILE="$PROFILES_FILE" \
  BROKER_AIRLOCK_LISTEN_ADDR="127.0.0.1:0" \
  BROKER_AIRLOCK_PROXY_URL="$PROXY_FRONT_URL" \
  BROKER_AIRLOCK_CA_CERT_FILE="$CA_CERT_FILE" \
  BROKER_AIRLOCK_CA_KEY_FILE="$CA_KEY_FILE" \
  BROKER_AIRLOCK_IDLE_SECONDS="$IDLE_SECONDS" \
  BROKER_AIRLOCK_TEST_GITHUB_API_BASE="$GITHUB_URL" \
  BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL="$GOOGLE_URL/token" \
  BROKER_AIRLOCK_TEST_ANTHROPIC_TOKEN_URL="$ANTHROPIC_TOKEN_URL" \
  BROKER_AIRLOCK_TEST_IAM_CREDENTIALS_URL="$IAM_URL" \
  BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE="$CA_CERT_FILE" \
  BROKER_AIRLOCK_TEST_DIAL_ALLOW="$DIAL_ALLOW" \
  BROKER_AIRLOCK_TEST_RESOLVE="$RESOLVE" \
  BROKER_AIRLOCK_TEST_KEEP_VALUES="$KEEP_VALUES" \
  BROKER_AIRLOCK_TEST_NO_KIND="$NO_KIND" \
  BROKER_AIRLOCK_TEST_AZURE_AUTHORITY="$MICROSOFT_HOST" \
  BROKER_JUDGE_MODEL_URL="$MODEL_URL" \
  BROKER_JUDGE_MODEL="$JUDGE_MODEL" \
  BROKER_JUDGE_MODEL_PROTOCOL="$JUDGE_MODEL_PROTOCOL" \
  BROKER_JUDGE_MODEL_AUDIENCE="$JUDGE_MODEL_AUDIENCE" \
  BROKER_JUDGE_DISPATCH_URL="$JUDGE_DISPATCH_URL" \
  BROKER_JUDGE_DISPATCH_TOKEN_FILE="$JUDGE_DISPATCH_TOKEN_FILE" \
  BROKER_JUDGE_TEST_SKIP_CHECKS="$JUDGE_SKIP_CHECKS" \
  GCE_METADATA_HOST="$METADATA_ADDRESS" \
  KUBERNETES_SERVICE_HOST='' \
  "$BROKER_BIN" > >(trap '' TERM INT; exec tee -a "$log" >&2) 2>&1 &
  BROKER_PIDS[n]=$!
}

BROKER_LOGS=()
BROKER_API_ADDRS=()
BROKER_PROXY_ADDRS=()
for n in $(seq 1 "$BROKER_COUNT"); do
  log="$WORK_DIR/broker$n.log"
  BROKER_LOGS[n]="$log"
  echo "dev-airlock: starting broker $n of $BROKER_COUNT..." >&2
  start_broker "$n" "$log"
done
for n in $(seq 1 "$BROKER_COUNT"); do
  await "broker $n" 120 "${BROKER_LOGS[n]}" '"message":"airlock proxy listening"' "${BROKER_PIDS[n]}"
  BROKER_API_ADDRS[n]="$(logged_addr 'broker listening' "${BROKER_LOGS[n]}")"
  BROKER_PROXY_ADDRS[n]="$(logged_addr 'airlock proxy listening' "${BROKER_LOGS[n]}")"
  if [ -z "${BROKER_API_ADDRS[n]}" ] || [ -z "${BROKER_PROXY_ADDRS[n]}" ]; then
    echo "dev-airlock: broker $n logged no API or proxy address" >&2
    exit 1
  fi
done

# --- The fronts learn their backends. Each front rereads this file when it changes, so writing
# it here is all it takes. ---
printf '%s\n' "${BROKER_API_ADDRS[@]}" >"$API_BACKENDS"
printf '%s\n' "${BROKER_PROXY_ADDRS[@]}" >"$PROXY_BACKENDS"

ready=false
for _ in $(seq 1 60); do
  if curl -sf "${API_FRONT_URL}/healthz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.5
done
if [ "$ready" != true ]; then
  echo "dev-airlock: the API front never answered /healthz within 30s" >&2
  exit 1
fi

# --- The pod session. agent-secrets makes the key and prints its thumbprint; the enrolment
# logs the registrar account in and enrols the pod under it, as the Legion daemon does; the
# enrollment id goes beside the key, which is the whole of what AGENT_SECRETS_KEY_DIR holds. ---
echo "dev-airlock: enrolling a pod session..." >&2
mkdir -p "$KEY_DIR"
THUMBPRINT="$("$AGENT_SECRETS_BIN" keygen --out "$KEY_DIR")"
"$STANDS_BIN" enroll-pod \
  --url "$API_FRONT_URL" \
  --service "$SERVICE" \
  --token-file "$REGISTRAR_TOKEN_FILE" \
  --pod-token-file "$POD_TOKEN_FILE" \
  --pod-uid "$POD_UID" \
  --thumbprint "$THUMBPRINT" \
  >"$ENROLL_JSON"
ENROLLMENT_ID="$(jq -r .enrollment_id "$ENROLL_JSON")"
if [ -z "$ENROLLMENT_ID" ] || [ "$ENROLLMENT_ID" = null ]; then
  echo "dev-airlock: the enrolment answered no enrollment id" >&2
  exit 1
fi
printf '%s' "$ENROLLMENT_ID" >"$KEY_DIR/enrollment"
echo "dev-airlock: enrolled pod session $ENROLLMENT_ID (pod $POD_UID, service $SERVICE)" >&2

# --- The session's renewer, for as long as this stack runs: the same command and the same token
# file the Legion worker shim gives a pod's renewer, so the session's lease outlives any check
# however long it holds a grant. It renews at once and then a third of the way through each
# lease, retries what the broker refuses for now, and exits only on a refusal it can never get
# past (76), which its log names. ---
AGENT_SECRETS_URL="$API_FRONT_URL" AGENT_SECRETS_KEY_DIR="$KEY_DIR" \
  "$AGENT_SECRETS_BIN" renew --pod-token-file "$POD_TOKEN_FILE" >"$RENEW_LOG" 2>&1 &
RENEW_PID=$!
echo "dev-airlock: renewing pod session $ENROLLMENT_ID's lease (agent-secrets renew, pid $RENEW_PID, log $RENEW_LOG)" >&2

# One banner line per broker and one for the session's renewer, built here rather than
# substituted into the banner below: a command substitution there would lose the last line's
# newline.
BROKER_BANNER=""
for n in $(seq 1 "$BROKER_COUNT"); do
  if [ -n "$BROKER_BANNER" ]; then
    BROKER_BANNER+=$'\n'
  fi
  BROKER_BANNER+="  broker $n:            api ${BROKER_API_ADDRS[n]}, proxy ${BROKER_PROXY_ADDRS[n]}, pid ${BROKER_PIDS[n]}, log ${BROKER_LOGS[n]}"
done
BROKER_BANNER+=$'\n'"  session renewer:     pid $RENEW_PID, agent-secrets renew --pod-token-file $POD_TOKEN_FILE, log $RENEW_LOG"

cat <<EOF

dev-airlock: ready.

  export AGENT_SECRETS_URL=$API_FRONT_URL
  export AGENT_SECRETS_KEY_DIR=$KEY_DIR
  export AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$ACCOUNT_TOKEN_FILE
  export AIRLOCK_PROXY_URL=$PROXY_FRONT_URL
  export AIRLOCK_DIR=$WORK_DIR
  export AIRLOCK_CA_CERT_FILE=$CA_CERT_FILE
  export AIRLOCK_CA_KEY_FILE=$CA_KEY_FILE
  export AIRLOCK_PROFILES_FILE=$PROFILES_FILE
  export AIRLOCK_FAKE_SECRETS_FILE=$FAKE_SECRETS_FILE
  export AIRLOCK_RECORDS_URL=$RECORDS_URL
  export AIRLOCK_S_URL=$S_URL
  export AIRLOCK_S_RECORDS=$RECORDS_URL/records/s
  export AIRLOCK_T_URL=$T_URL
  export AIRLOCK_T_RECORDS=$RECORDS_URL/records/t
  export AIRLOCK_GITHUB_URL=$GITHUB_URL
  export AIRLOCK_GITHUB_RECORDS=$RECORDS_URL/records/github
  export AIRLOCK_GOOGLE_URL=$GOOGLE_URL
  export AIRLOCK_GOOGLE_RECORDS=$RECORDS_URL/records/google
  export AIRLOCK_MICROSOFT_URL=$MICROSOFT_URL
  export AIRLOCK_MICROSOFT_RECORDS=$RECORDS_URL/records/microsoft
  export AIRLOCK_AZURE_RESOURCE_URL=$AZURE_RESOURCE_URL
  export AIRLOCK_AWS_URL=$AWS_URL
  export AIRLOCK_AWS_RECORDS=$RECORDS_URL/records/aws
  export AIRLOCK_AWS_KEY_ID=$AWS_KEY_ID
  export AIRLOCK_GITHUB_APP_KEY_FILE=$GITHUB_APP_KEY_FILE
  export AIRLOCK_MODEL_URL=$MODEL_URL
  export AIRLOCK_MODEL_RECORDS=$RECORDS_URL/records/model
  export AIRLOCK_MODEL_ANSWER_URL=$MODEL_ANSWER_URL
  export AIRLOCK_DISPATCH_URL=$DISPATCH_URL
  export AIRLOCK_DISPATCH_RECORDS=$RECORDS_URL/records/dispatch
  export AIRLOCK_DISPATCH_TOKEN_FILE=$JUDGE_DISPATCH_TOKEN_FILE
  export AIRLOCK_DISPATCH_OPEN_ISSUE=$DISPATCH_OPEN_ISSUE
  export AIRLOCK_DISPATCH_DONE_ISSUE=$DISPATCH_DONE_ISSUE
  export AIRLOCK_DISPATCH_ANSWERED_ASK=$DISPATCH_ANSWERED_ASK
  export AIRLOCK_METADATA_URL=$METADATA_URL
  export AIRLOCK_METADATA_RECORDS=$RECORDS_URL/records/metadata
  export AIRLOCK_ANTHROPIC_URL=$ANTHROPIC_URL
  export AIRLOCK_ANTHROPIC_ADDRESS=${ANTHROPIC_ADDRESS%:*}
  export AIRLOCK_ANTHROPIC_RECORDS=$RECORDS_URL/records/anthropic
  export AIRLOCK_ANTHROPIC_TOKEN_URL=$ANTHROPIC_TOKEN_URL
  export AIRLOCK_ANTHROPIC_TOKEN_RECORDS=$RECORDS_URL/records/anthropic-token
  export AIRLOCK_ANTHROPIC_TOKENS_URL=$ANTHROPIC_TOKENS_URL
  export AIRLOCK_ANTHROPIC_ANSWER_URL=$ANTHROPIC_ANSWER_URL
  export AIRLOCK_IAM_URL=$IAM_URL
  export AIRLOCK_IAM_RECORDS=$RECORDS_URL/records/iam
  export AIRLOCK_IAM_TOKENS_URL=$IAM_TOKENS_URL
  export AIRLOCK_ANTHROPIC_NARROWED_SECRET=$ANTHROPIC_NARROWED_SECRET
  export AIRLOCK_ANTHROPIC_KIND_SECRET=$ANTHROPIC_KIND_SECRET
  export AIRLOCK_ANTHROPIC_NARROWED_ACCOUNT=$ANTHROPIC_NARROWED_ACCOUNT
  export AIRLOCK_ANTHROPIC_KIND_ACCOUNT=$ANTHROPIC_KIND_ACCOUNT
  export AIRLOCK_JUDGE_MODEL=$JUDGE_MODEL
  export AIRLOCK_JUDGE_AUDIENCE=$JUDGE_MODEL_AUDIENCE
  export AIRLOCK_JUDGE_SECONDS=$JUDGE_SECONDS
  export AIRLOCK_SECOND_JUDGE_SECONDS=$SECOND_JUDGE_SECONDS
  export AIRLOCK_JUDGED_SECRET=$JUDGED_SECRET
  export AIRLOCK_UNJUDGED_SECRET=$UNJUDGED_SECRET
  export AIRLOCK_SHARED_SECRET=$SHARED_SECRET
  export AIRLOCK_ADMIN_SECRET=$ADMIN_SECRET
  export AIRLOCK_SECOND_JUDGED_SECRET=$SECOND_JUDGED_SECRET
  export AIRLOCK_JUDGE_GUIDANCE='$JUDGE_GUIDANCE'
  export AIRLOCK_SECOND_JUDGE_GUIDANCE='$SECOND_JUDGE_GUIDANCE'
  export AIRLOCK_ADMIN_MAX_GRANT=$ADMIN_MAX_GRANT
  export AIRLOCK_APPROVER=$APPROVER_LOGIN
  export AIRLOCK_SIGNIN_URL=$ISSUER_URL
  export AIRLOCK_ENROLLMENT=$ENROLLMENT_ID
  export DEV_BROKER_DIR=$BASE_DIR
  export PATH=$BASE_BIN_DIR:\$PATH

$BROKER_BANNER
  secrets:             $DEMO_SECRET and $SCOPED_SECRET (stored, kinds stest and scoped, bound to
                       $S_HOST; $SCOPED_SECRET in authorization and x-key, GET /read alone),
                       $GITHUB_SECRET (github-app, kind ghapp, installation $GITHUB_INSTALLATION on
                       $GITHUB_OWNER; $PLAIN_GITHUB_PROFILE is a profile of $PLAIN_GITHUB_SOURCE, the
                       same App with no proxy tag, released as its value), $GOOGLE_SECRET
                       (google-login, kind glogin), $AZURE_SECRET (kind azure, human tier),
                       $AWS_SECRET (kind aws), and $ANTHROPIC_NARROWED_SECRET and $ANTHROPIC_KIND_SECRET
                       (anthropic-federation, kind anthropic, human tier, an administrator's;
                       $ANTHROPIC_NARROWED_SECRET proxy-narrowed with a profile of the kind's three
                       rows, $ANTHROPIC_KIND_SECRET with none), each behind the proxy by its proxy tag;
                       the kinds are in $PROFILES_FILE
  anthropic:           the Admin API stand answers $ANTHROPIC_HOST on ${ANTHROPIC_ADDRESS%:*}; the
                       broker mints at the iam stand ($IAM_URL) and the token exchange stand
                       ($ANTHROPIC_TOKEN_URL), which answers an expires_in of 3600 until a check
                       POSTs {"expires_in": <seconds>} to $ANTHROPIC_ANSWER_URL; what each minted is
                       at $IAM_TOKENS_URL and $ANTHROPIC_TOKENS_URL
  judge:               $JUDGE_MODEL at $MODEL_URL, which answers grant until a check
                       POSTs another answer to $MODEL_ANSWER_URL; it reads, as data for the
                       model, $DISPATCH_OPEN_ISSUE (open) and $DISPATCH_DONE_ISSUE (done) at
                       $DISPATCH_URL and the one ask that stand holds, $DISPATCH_ANSWERED_ASK on
                       $DISPATCH_OPEN_ISSUE, answered by $APPROVER_LOGIN, and mints its own
                       credential at the metadata stand on $METADATA_ADDRESS.
                       Who decides each key is its own judge tag: the judge decides up to
                       $JUDGE_SECONDS seconds of $JUDGED_SECRET and $SECOND_JUDGE_SECONDS of
                       $SECOND_JUDGED_SECRET (owned by $APPROVER_LOGIN, each against its guidance
                       tag) and $JUDGE_SECONDS of $SHARED_SECRET (owned by nobody). A request for
                       them alone is the judge's: no approve_url,
                       a read-only page, never listed; granted, or refused for good. No request
                       has to cite anything. $ADMIN_SECRET carries the same $JUDGE_SECONDS-second judge tag
                       and is an administrator's secret (admin: "true", requesters $APPROVER_LOGIN
                       and $SERVICE, max-grant $ADMIN_MAX_GRANT), and $UNJUDGED_SECRET carries no
                       judge tag: each is $APPROVER_LOGIN's card, and so is any request with one of
                       them beside a judge key. Ask with
                       agent-secrets request $JUDGED_SECRET --ttl 10m --reason "<what, naming the stack check run>"
  approver:            $APPROVER_LOGIN, who approves $AZURE_SECRET, $ANTHROPIC_NARROWED_SECRET and
                       $ANTHROPIC_KIND_SECRET on ${API_FRONT_URL}/approve/<record>;
                       the sign-in at $ISSUER_URL signs that person in with no page to fill in,
                       and a POST to ${API_FRONT_URL}/approve needs an Origin header or the page answers 403
  database:            $DISPLAY_URL  (this instance's own; created and dropped by this script)
  stand-ins:           $STANDS_DIR  (each one's certificate; the authority is $CA_CERT_FILE)
  dev-broker.sh:       $BASE_DIR  (its own broker, issuer, tokens and database, logged to $DEV_BROKER_LOG)

  A command behind the airlock gets a placeholder, not a value: ask for one with
  agent-secrets $DEMO_SECRET -- <command>, whose HTTPS_PROXY is the proxy front above, and read
  what $S_HOST was actually sent at $RECORDS_URL/records/s. Press Ctrl-C here to stop
  every process this script started.

  dev-broker.sh printed a banner of its own above, for its own broker: where the two name the
  same variable - AGENT_SECRETS_URL, AGENT_SECRETS_ACCOUNT_TOKEN_FILE, PATH - this one is the
  airlock's stack and the one to export.
EOF

wait "${BROKER_PIDS[1]}"
