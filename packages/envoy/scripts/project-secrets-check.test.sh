#!/usr/bin/env bash
# packages/envoy/scripts/project-secrets-check.test.sh
#
# Proves project-secrets-check.sh's own decisions, without a stack: what it asks of each caller,
# what answer passes each row, what answer fails it and which caller and secret the failure names,
# that its negative control fails, that a run whose rows matched nothing fails, and that live mode
# refuses half its options. It does not prove the broker's rule; the driver does that on a real
# stack.
#
# It copies the real driver, and the lib/pg-url.sh it sources, into a directory of its own beside
# a fake dev-stack.sh, which prints the banner the real one prints (one export per line,
# "dev-stack: ready.", the databases line) and puts a fake agent-secrets and agent-secrets-helper
# in the stack's bin directory, as the real one puts the built binaries there. Fakes of bun
# (print-trade.ts), psql, curl, python3 (the metadata stand-in) and sleep go first on PATH, with a
# wrapper of the real jq (below). The driver starts every caller with env -i, so the fakes find
# this run's state through a path written into each of them, never through the environment. Each
# fake answers as the real program answers on the right stack, and one word in the state directory
# (the scenario) turns one answer wrong, as a broker or Dispatch with a bug would.
#
# The fakes hold what the real programs hold to the driver: the trade's answer depends on the
# sign-in the driver's login wrote and on AGENT_SECRETS_PROJECTS; a session's projects come from
# register --projects or AGENT_SECRETS_PROJECTS; a request's answer reads the fake secrets file the
# driver rewrote, so an untagged PRIV_KEY goes to every caller as it would; the issuer approves a
# device code only after the person was queued; psql answers only for the Dispatch database; the
# proxy serves PRIV_KEY only when its proxy tag names the kind the banner exports, as the real
# proxy serves only a secret behind a kind, and refuses only once the driver's own write took
# carol off PRIV.
#
#   bash scripts/project-secrets-check.test.sh
#
# It prints one PASS or FAIL line per assertion and "project-secrets-check.test: <n> passed, <m>
# failed" last, and exits 0 only when none failed. Its directory is kept and named on the last line.
# No process of a run outlives it: each run's check fails when the driver's own cleanup left one
# running, and the test then stops it itself, before the next run and again when it exits.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly real_driver="$script_dir/project-secrets-check.sh"
temporary_dir="$(mktemp -d "${TMPDIR:-/tmp}/project-secrets-check-test.XXXXXX")"
readonly temporary_dir
readonly state="$temporary_dir/state"
readonly fakes="$temporary_dir/fakes"
readonly stackbin="$temporary_dir/stackbin"
readonly repo="$temporary_dir/repo"
readonly driver="$repo/packages/envoy/scripts/project-secrets-check.sh"
real_sleep="$(command -v sleep)"
readonly real_sleep
readonly admin_url='postgres://tester:not-a-secret@127.0.0.1:5999/postgres?sslmode=disable'
mkdir -p "$state" "$fakes" "$stackbin" "$repo/packages/envoy/scripts/lib" "$repo/packages/cipher-client/scripts"
cp "$real_driver" "$driver"
cp "$script_dir/lib/pg-url.sh" "$repo/packages/envoy/scripts/lib/pg-url.sh"
: >"$repo/packages/cipher-client/scripts/print-trade.ts"

# fake writes an executable whose first lines name this run's state, the real sleep and the
# scripts directory, and whose body is the quoted text on standard input.
fake() { # <path>
  {
    printf '#!/usr/bin/env bash\nstate=%q\nreal_sleep=%q\n' "$state" "$real_sleep"
    cat
  } >"$1"
  chmod +x "$1"
}

# --- dev-stack.sh: refuses a start without --project-access, makes a work directory of its own
# holding what the real one's holds (the bin directory, the token files, the fake secrets file),
# prints the banner, and stays up until it is signalled. In the scenario empty-banner it prints a
# banner from before --project-access, with none of PRIV's exports. ---
fake "$repo/packages/envoy/scripts/dev-stack.sh" <<'EOF'
set -euo pipefail
scenario="$(cat "$state/scenario")"
access=false database="" all_args="$*"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --project-access) access=true; shift ;;
    --database-url) database="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf 'dev-stack access=%s database=%s\n' "$access" "$database" >>"$state/dev-stack.log"
# Its arguments and the LEGION_TEST_PG_DSN it was given, for the check that the Postgres URL
# reaches it in its environment and in none of its arguments.
printf '%s\n' "$all_args" >"$state/dev-stack.args"
printf '%s\n' "${LEGION_TEST_PG_DSN-unset}" >"$state/dev-stack.dsn"
[ "$access" = true ] || { echo "fake dev-stack: started without --project-access" >&2; exit 2; }
work="$(mktemp -d "$state/dev-stack.XXXXXX")"
printf '%s\n' "$work" >"$state/current-stack"
suffix="$(printf '%s' "${work##*.}" | tr '[:upper:]' '[:lower:]')"
mkdir -p "$work/bin" "$work/logs"
cp "$state/../stackbin/"* "$work/bin/"
for file in account-token priv-exchange-token priv-pod-token device-client-secret; do
  printf 'token-of-%s\n' "$file" >"$work/$file"
done
printf 'fake-ca\n' >"$work/ca.crt"
: >"$work/dev-metadata.py"
cat >"$work/fake-secrets.json" <<JSON
{"secrets": [
  {"name": "example/agent-secrets/demo-read-token", "kms_key_id": "k", "tags": {"owner": "ada@example.com", "tier": "agent"}, "value": "v"}
]}
JSON
trap 'exit 0' TERM INT
echo "dev-stack: broker http://127.0.0.1:5101" >&2
cat <<BANNER

dev-stack: ready.

  export PATH=$work/bin:\$PATH
  export AGENT_SECRETS_URL=http://127.0.0.1:5101
  export AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$work/account-token
  export DISPATCH_URL=http://127.0.0.1:5102
  export DEV_STACK_DIR=$work
  export DEV_STACK_ISSUER_URL=http://127.0.0.1:5103
  export DEV_STACK_METADATA_SCRIPT=$work/dev-metadata.py
BANNER
if [ "$scenario" != empty-banner ]; then
  cat <<BANNER
  export DEV_STACK_PRIV_PROJECT=PRIV
  export DEV_STACK_PRIV_OWNER=carol@example.com
  export DEV_STACK_PRIV_SERVICE=legion-priv
  export DEV_STACK_PRIV_ACCOUNT=system:serviceaccount:legion-priv:legion-priv-worker
  export DEV_STACK_PRIV_SESSION=legion-priv-worker-session
  export DEV_STACK_PRIV_EXCHANGE_TOKEN_FILE=$work/priv-exchange-token
  export DEV_STACK_PRIV_POD_TOKEN_FILE=$work/priv-pod-token
  export DEV_STACK_PROJECT_ACCESS=true
  export DEV_STACK_PRIV_SECRET=PRIV_KEY
  export DEV_STACK_AIRLOCK_KIND=priv
  export DEV_STACK_AIRLOCK_PROXY_URL=http://127.0.0.1:5104
  export DEV_STACK_AIRLOCK_BOUND_URL=https://priv.airlock.test
  export DEV_STACK_AIRLOCK_CA_CERT_FILE=$work/ca.crt
BANNER
fi
cat <<BANNER

  export AGENT_SECRETS_MACHINE_IDENTITY=metadata
  export AGENT_SECRETS_MACHINE_AUDIENCE=dev-stack-machine
  export AGENT_SECRETS_SESSION=<this process's own session name>
  export AGENT_SECRETS_SIGNIN_ISSUER=http://127.0.0.1:5103
  export AGENT_SECRETS_SIGNIN_CLIENT_ID=dev-stack-device
  export AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE=$work/device-client-secret

  databases:  dev_stack_broker_$suffix and dev_stack_dispatch_$suffix on postgres://tester@127.0.0.1:5999
BANNER
while :; do
  "$real_sleep" 1 &
  wait "$!" || true
done
EOF

# --- The stack's agent-secrets: login, enroll --pod, register, request and sign, each answering
# as the real client answers on a stack whose broker holds the rule, and one answer turned wrong
# per scenario. ---
fake "$stackbin/agent-secrets" <<'EOF'
set -euo pipefail
scenario="$(cat "$state/scenario")"
printf 'agent-secrets %s | home=%s projects=%s session=%s\n' "$*" "${HOME:-}" "${AGENT_SECRETS_PROJECTS:-}" "${FAKE_SESSION:-}" >>"$state/agent-secrets.log"
secrets="$(dirname "$0")/../fake-secrets.json"
tagged="$(jq -r '[.secrets[] | select(.name | endswith("/priv-key")) | .tags["dispatch-project"] // ""][0] // ""' "$secrets")"
sees_priv() { # <person>
  case "$1" in
    carol@example.com) [ ! -e "$state/carol-off" ] ;;
    bob@example.com) return 0 ;;
    *) return 1 ;;
  esac
}
person_of() { # <sign-in dir>
  local record
  for record in "$1"/*.json; do
    [ -e "$record" ] || continue
    jq -r .person "$record"
    return 0
  done
}
command="${1:-}"
shift || true
case "$command" in
  login)
    code="FAKE-$(basename "$(dirname "$HOME")" | tr '[:lower:]' '[:upper:]')"
    [ -n "${AGENT_SECRETS_SIGNIN_DIR:-}" ] || { echo "agent-secrets login: no sign-in dir" >&2; exit 1; }
    [ -n "${GCE_METADATA_HOST:-}" ] || { echo "agent-secrets login: this machine proves no identity of its own" >&2; exit 1; }
    echo "To sign in, open http://127.0.0.1:5103/device and type the code $code" >&2
    for _ in $(seq 400); do
      [ -e "$state/approved-$code" ] && break
      "$real_sleep" 0.05
    done
    [ -e "$state/approved-$code" ] || { echo "agent-secrets login: the code was never approved" >&2; exit 1; }
    person="$(cat "$state/approved-$code")"
    mkdir -p "$AGENT_SECRETS_SIGNIN_DIR"
    printf '{"person": "%s"}\n' "$person" >"$AGENT_SECRETS_SIGNIN_DIR/$person.json"
    echo "$person is signed in on this machine (enrollment enr-signin)"
    ;;
  enroll)
    service="" token="" out=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --service) service="$2"; shift 2 ;;
        --pod-token-file) token="$2"; shift 2 ;;
        --out) out="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    [ "$service" = legion-priv ] || { echo "agent-secrets enroll: 403 SERVICE_LOGIN_REFUSED: $service" >&2; exit 1; }
    case "$token" in */priv-pod-token) ;; *) echo "agent-secrets enroll: 401 the token is no pod's: $token" >&2; exit 1 ;; esac
    mkdir -p "$out"
    printf 'key\n' >"$out/key.pem"
    printf 'enr-pod\n' >"$out/enrollment"
    echo "enrolled enr-pod"
    ;;
  register)
    projects="${AGENT_SECRETS_PROJECTS:-}" exec_command=false
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --projects) projects="$2"; shift 2 ;;
        --wait) shift 2 ;;
        --exec) exec_command=true; shift ;;
        --) shift; break ;;
        *) shift ;;
      esac
    done
    sock="${AGENT_SECRETS_HELPER_SOCK:-}"
    [ -n "$sock" ] && [ -e "$sock" ] || { echo "agent-secrets register: no helper at '$sock'" >&2; exit 2; }
    person="$(person_of "$(cat "$sock")")"
    printf 'register person=%s projects=%s\n' "$person" "$projects" >>"$state/register.log"
    refusal=""
    case " $projects " in
      *" PRIV "*)
        # D's check comes first: F's check runs after the enrollment's project checks (#t-f-9 of
        # dispatch://INFRA-3894/artifact/plan-md), so a person who cannot see PRIV is refused D's way
        # with or without F.
        if ! sees_priv "$person" && [ "$scenario" != alice-enrolled ]; then
          refusal="broker 403 PROJECT_NOT_VISIBLE: the session names a project its person cannot see: PRIV"
        elif [ "$scenario" = f ]; then
          refusal="broker 403 MACHINE_NOT_REGISTERED: machine_not_registered: PRIV is private, and this machine is not registered to it"
        fi
        ;;
    esac
    if [ "$exec_command" = false ]; then
      if [ -n "$refusal" ]; then
        echo "agent-secrets register: not enrolled yet: $refusal" >&2
        exit 1
      fi
      printf 'box:1\tenr-session\tenrolled\n'
      exit 0
    fi
    if [ -n "$refusal" ]; then
      echo "agent-secrets register: not enrolled yet ($refusal); launching anyway, and this session's secrets calls fail until the helper enrolls it" >&2
      FAKE_SESSION=unenrolled exec "$@"
    fi
    FAKE_SESSION="$person;$projects" exec "$@"
    ;;
  request)
    name="$1"
    who="" names=""
    if [ "${FAKE_SESSION:-}" = unenrolled ]; then
      echo "agent-secrets request: broker 401 NOT_ENROLLED: this session has no live enrollment" >&2
      exit 1
    elif [ -n "${FAKE_SESSION:-}" ]; then
      who="${FAKE_SESSION%%;*}" names="${FAKE_SESSION#*;}"
      [ -n "$names" ] || who="$who naming nothing"
    elif [ -n "${AGENT_SECRETS_KEY_DIR:-}" ] && [ "$(cat "$AGENT_SECRETS_KEY_DIR/enrollment" 2>/dev/null)" = enr-pod ]; then
      who=pod names=PRIV
    elif [ "${AGENT_SECRETS_MACHINE_IDENTITY:-}" = metadata ] && [ -n "${GCE_METADATA_HOST:-}" ] && [ -n "${AGENT_SECRETS_SESSION:-}" ]; then
      who=machine
    else
      echo "agent-secrets request: no session identity" >&2
      exit 1
    fi
    person="${who%% *}"
    [ "$who" = pod ] && person=carol@example.com
    state_of=denied
    case "$name" in
      PUB_KEY)
        state_of=granted
        [ "$scenario" = carol-none-pub-denied ] && [ "$who" = "carol@example.com naming nothing" ] && state_of=denied
        ;;
      PRIV_KEY)
        if [ -z "$tagged" ]; then
          state_of=granted
        elif [[ " $names " == *" PRIV "* ]] && sees_priv "$person"; then
          state_of=granted
        fi
        case "$scenario/$who" in
          pod-priv-denied/pod) state_of=denied ;;
          carol-none-priv-granted/"carol@example.com naming nothing") state_of=granted ;;
          machine-priv-granted/machine) state_of=granted ;;
        esac
        ;;
      *) echo "agent-secrets request: 404 SECRET_UNKNOWN: $name" >&2; exit 1 ;;
    esac
    if [ "$state_of" = granted ]; then
      printf '{"request_id":"req-%s","state":"granted","secrets":[{"name":"%s","decision":"automatic"}],"grant_id":"grant-%s","record_id":null,"approver":null}\n' "$name" "$name" "$name"
      exit 0
    fi
    printf '{"request_id":"req-%s","state":"denied","secrets":[{"name":"%s","decision":"deny"}],"grant_id":null,"record_id":null,"approver":null,"detail":"%s: project-not-named"}\n' "$name" "$name" "$name"
    exit 77
    ;;
  sign)
    url=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --url) url="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    [ -s "${AGENT_SECRETS_KEY_DIR:-/nonexistent}/key.pem" ] || { echo "agent-secrets sign: no key" >&2; exit 1; }
    printf 'proof-for-%s\n' "$url"
    ;;
  *) echo "fake agent-secrets: unexpected command $command" >&2; exit 2 ;;
esac
EOF

# --- The stack's agent-secrets-helper: serve writes its socket path with the sign-in directory it
# reads, so a register through it acts for whoever signed in there, and stays up until signalled. ---
fake "$stackbin/agent-secrets-helper" <<'EOF'
[ "${1:-}" = serve ] || { echo "fake helper: unexpected $*" >&2; exit 2; }
[ -n "${AGENT_SECRETS_HELPER_SOCK:-}" ] && [ -n "${AGENT_SECRETS_SIGNIN_DIR:-}" ] || { echo "fake helper: no socket or sign-in dir" >&2; exit 2; }
[ -n "${GCE_METADATA_HOST:-}" ] || { echo "fake helper: no machine identity" >&2; exit 2; }
trap 'rm -f "$AGENT_SECRETS_HELPER_SOCK"; exit 0' TERM INT
printf '%s\n' "$AGENT_SECRETS_SIGNIN_DIR" >"$AGENT_SECRETS_HELPER_SOCK"
while :; do
  "$real_sleep" 1 &
  wait "$!" || true
done
EOF

# --- bun print-trade.ts: the introspection of one trade, as print-trade.ts prints it. A pod's
# exchange token is named [PRIV]; a person signed in is answered under the person scope with the
# projects AGENT_SECRETS_PROJECTS lists, or refused when the person cannot see one; a machine with
# no sign-in names no project. ---
fake "$fakes/bun" <<'EOF'
set -euo pipefail
scenario="$(cat "$state/scenario")"
printf 'bun %s | identity=%s projects=%s signins=%s\n' "$*" "${AGENT_SECRETS_MACHINE_IDENTITY:-}" "${AGENT_SECRETS_PROJECTS:-}" "${AGENT_SECRETS_SIGNIN_DIR:-}" >>"$state/bun.log"
case "${1:-}" in */print-trade.ts) ;; *) echo "fake bun: unexpected $*" >&2; exit 2 ;; esac
[ -s "${AGENT_SECRETS_ACCOUNT_TOKEN_FILE:-/nonexistent}" ] || { echo "print-trade: AGENT_SECRETS_ACCOUNT_TOKEN_FILE names no account token" >&2; exit 2; }
answer() { printf '{"active":true,"person":"%s","service":"%s","project_scope":"%s","projects":%s}\n' "$@"; }
case "${AGENT_SECRETS_MACHINE_IDENTITY:-}" in
  kubernetes)
    case "${AGENT_SECRETS_MACHINE_TOKEN_FILE:-}" in */priv-exchange-token) ;; *) echo "print-trade: invalid_grant: no exchange entry" >&2; exit 3 ;; esac
    [ "${AGENT_SECRETS_SESSION:-legion-priv-worker-session}" = legion-priv-worker-session ] || { echo "print-trade: invalid_grant: session mismatch" >&2; exit 3; }
    scope=named
    [ "$scenario" = pod-scope-person ] && scope=person
    answer carol@example.com "" "$scope" '["PRIV"]'
    ;;
  metadata)
    [ -n "${GCE_METADATA_HOST:-}" ] || { echo "print-trade: this machine proves no identity" >&2; exit 3; }
    person=""
    for record in "${AGENT_SECRETS_SIGNIN_DIR:-/nonexistent}"/*.json; do
      [ -e "$record" ] && person="$(jq -r .person "$record")"
    done
    if [ -z "$person" ]; then
      answer "" example-fleet person '[]'
      exit 0
    fi
    case " ${AGENT_SECRETS_PROJECTS:-} " in
      *" PRIV "*)
        if [ "$person" = carol@example.com ]; then
          case "$scenario" in
            # f-trade-only: a broker whose machine check refuses the trade and not the enrollment.
            f | f-trade-only) echo "print-trade: invalid_request: The subject token was refused: machine_not_registered: PRIV is private, and this machine is not registered to it" >&2; exit 3 ;;
            carol-other-refusal) echo "print-trade: temporarily_unavailable: Dispatch could not be asked whether the token's person sees the projects it names; try again." >&2; exit 3 ;;
            carol-other-list) answer "$person" "" person '["PUB"]'; exit 0 ;;
          esac
          answer "$person" "" person '["PRIV"]'
        elif [ "$scenario" = alice-minted ]; then
          answer "$person" "" person '["PRIV"]'
        else
          echo "print-trade: invalid_request: The token names a project its person cannot see: PRIV." >&2
          exit 3
        fi
        ;;
      *) answer "$person" "" person '[]' ;;
    esac
    ;;
  *) echo "print-trade: this environment makes no broker client" >&2; exit 2 ;;
esac
EOF

# --- psql: answers only for the Dispatch database, records the stand-in writes (PRIV private,
# carol off PRIV) and reads PRIV's row back as Dispatch holds it. In the scenario
# no-access-schema it is a Dispatch without avenue A's columns. ---
fake "$fakes/psql" <<'EOF'
printf 'psql %s\n' "$*" >>"$state/psql.log"
scenario="$(cat "$state/scenario")"
case "${1:-}" in *dev_stack_dispatch_*) ;; *) echo "fake psql: $1 is not this run's Dispatch database" >&2; exit 2 ;; esac
sql="$*"
case "$sql" in
  *"insert into projects"*)
    if [ "$scenario" = no-access-schema ]; then
      echo 'ERROR:  column "visibility" of relation "projects" does not exist' >&2
      exit 1
    fi
    touch "$state/priv-private"
    ;;
esac
case "$sql" in *"set owner = 'bob@example.com'"*) touch "$state/carol-off" ;; esac
case "$sql" in
  *"select p.visibility"*)
    if [ -e "$state/carol-off" ]; then
      echo 'private|bob@example.com|bob@example.com'
    elif [ -e "$state/priv-private" ]; then
      echo 'private|carol@example.com|bob@example.com'
    fi
    ;;
esac
exit 0
EOF

# --- curl: the issuer's two control routes (a device code is approved only for a person queued
# first), the metadata stand-in, the broker's reread and values routes, and the airlock's proxy,
# which serves a request carrying the grant's placeholder until carol is off PRIV and refuses the
# CONNECT 407 after. ---
fake "$fakes/curl" <<'EOF'
printf 'curl %s\n' "$*" >>"$state/curl.log"
scenario="$(cat "$state/scenario")"
url="" data="" out="" proxy="" proxyuser="" proof="" authorization=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -d | --data | --data-raw) data="$2"; shift 2 ;;
    -o) out="$2"; shift 2 ;;
    -w | --proxy | --cacert | --noproxy | -X)
      [ "$1" = --proxy ] && proxy="$2"
      shift 2
      ;;
    -H)
      # curl reads a header argument that starts with "@" from the file it names, one per line.
      header="$2"
      [ "${header#@}" = "$header" ] || header="$(cat -- "${header#@}")"
      case "$header" in
        Proof:*) proof="${header#Proof: }" ;;
        Authorization:*) authorization="${header#Authorization: }" ;;
      esac
      shift 2
      ;;
    -K | --config)
      # curl reads options from the file -K names; the driver gives it proxy-user alone.
      while IFS= read -r line; do
        case "$line" in
          'proxy-user = "'*'"')
            proxyuser="${line#proxy-user = \"}"
            proxyuser="${proxyuser%\"}"
            ;;
        esac
      done <"$2"
      shift 2
      ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
if [ -n "$proxy" ]; then
  secrets="$(cat "$state/current-stack")/fake-secrets.json"
  tagged="$(jq -r '[.secrets[] | select(.name | endswith("/priv-key")) | .tags["dispatch-project"] // ""][0] // ""' "$secrets")"
  behind="$(jq -r '[.secrets[] | select(.name | endswith("/priv-key")) | .tags.proxy // ""][0] // ""' "$secrets")"
  refused=false
  [ "$proxy" = "http://127.0.0.1:5104" ] || refused=true
  [ "$proxyuser" = "agent-secrets:pw-not-a-secret" ] || refused=true
  [ "$authorization" = "Bearer agent-secrets-placeholder-not-a-secret" ] || refused=true
  [ "$url" = "https://priv.airlock.test/ok" ] || refused=true
  [ "$behind" = priv ] || refused=true
  [ "$scenario" = airlock-first-refused ] && refused=true
  if [ -e "$state/carol-off" ] && [ -n "$tagged" ] && [ "$scenario" != airlock-still-served ]; then
    refused=true
  fi
  if [ "$refused" = true ]; then
    printf '407 000'
    echo "curl: (56) CONNECT tunnel failed, response 407" >&2
    exit 56
  fi
  printf 'the s stand-in answered\n' >"$out"
  printf '200 200'
  exit 0
fi
case "$url" in
  */_dev/sign-in-as)
    # The real broker admits a person only when the token's hd is the sign-in domain, so a queued
    # sign-in without the email's own domain in hd is one the real run would refuse.
    [ "$(jq -r '.hd // ""' <<<"$data")" = "$(jq -r '.email | split("@")[1]' <<<"$data")" ] || exit 22
    jq -r .email <<<"$data" >"$state/queued"
    ;;
  */_dev/approve-device-code)
    [ -s "$state/queued" ] || exit 22
    mv "$state/queued" "$state/approved-$(jq -r .user_code <<<"$data")"
    ;;
  *127.0.0.1:4242/computeMetadata/*) printf 'machine-identity-token' ;;
  */v1/secrets/*/reread) ;;
  */v1/grants/*/values)
    [ "$proof" = "proof-for-$url" ] || { echo '{"error":"no proof"}'; exit 0; }
    echo '{"airlock":{"names":{"PRIV_KEY":{"value":"agent-secrets-placeholder-not-a-secret"}},"proxy_password":"pw-not-a-secret"}}'
    ;;
  *) echo "fake curl: unexpected $url" >&2; exit 2 ;;
esac
EOF

# --- python3, as the metadata stand-in the banner names: writes its port where it is told to and
# stays up until signalled. ---
fake "$fakes/python3" <<'EOF'
case "${1:-}" in */dev-metadata.py) ;; *) echo "fake python3: unexpected $*" >&2; exit 2 ;; esac
[ -n "${DEV_STACK_METADATA_PORT_FILE:-}" ] || { echo "fake metadata: no port file named" >&2; exit 2; }
trap 'exit 0' TERM INT
printf '4242\n' >"$DEV_STACK_METADATA_PORT_FILE"
while :; do
  "$real_sleep" 1 &
  wait "$!" || true
done
EOF

# --- sleep: records each wait, so the run takes no real time and a test can read how long the
# driver waited before a step. ---
fake "$fakes/sleep" <<'EOF'
printf '%s\n' "$*" >>"$state/sleeps"
"$real_sleep" 0.01
EOF

# --- jq, the real one, which the driver and the fakes above read JSON with. Each of them runs
# under env -i with HOME set to this test's directory, and a version manager's jq shim (mise's)
# reads whether the user trusts their configuration under HOME, so under this HOME it refuses to
# run at all and every row fails. The wrapper runs the jq this test found on its own PATH with the
# HOME this test was started with; a jq that is no shim ignores HOME. ---
real_jq="$(command -v jq)"
printf '#!/usr/bin/env bash\nHOME=%q exec %q "$@"\n' "$HOME" "$real_jq" >"$fakes/jq"
chmod +x "$fakes/jq"

# --- Running the driver, and the assertions. ---
passed=0
failed=0
RUN_RC=0
RUN_OUT=""
RUN_LEFT=0

# Every process a run starts — the fake stack, the two helpers, the metadata stand-in and each
# fake the driver calls — runs from a path under this test's directory, and nothing else does, so
# the processes of the run are the ones whose command line names it.
run_processes() { pgrep -f -- "$temporary_dir/"; }

# gone_by waits until no process of the run is left or the clock reaches the second deadline (of
# $SECONDS), and answers whether none is left. The deadline is the clock's, not a count of polls,
# since one pgrep over a busy box's process table can take a second by itself.
gone_by() { # <deadline>
  while run_processes >/dev/null; do
    [ "$SECONDS" -lt "$1" ] || return 1
    "$real_sleep" 0.2
  done
}

# reap stops every process of the run still up and prints how many there were: the test's own
# cleanup, whatever the driver under test did, so a driver whose cleanup kills nothing (the
# control that shows no_process_left can fail) leaves nothing running behind the test either.
reap() {
  local pids
  pids="$(run_processes || true)"
  if [ -z "$pids" ]; then
    echo 0
    return
  fi
  # shellcheck disable=SC2086 # one pid per word
  kill -TERM $pids 2>/dev/null || true
  gone_by $((SECONDS + 5)) || pkill -KILL -f -- "$temporary_dir/" 2>/dev/null || true
  wc -w <<<"$pids"
}
trap 'reap >/dev/null' EXIT

# outlived gives the processes of the run five seconds to end once the driver has returned, as
# its own cleanup TERMs them and waits, then reaps the ones still up and prints how many.
outlived() {
  if gone_by $((SECONDS + 5)); then
    echo 0
  else
    reap
  fi
}

check() { # <description> <command...>
  if "${@:2}"; then
    printf 'PASS: %s\n' "$1"
    passed=$((passed + 1))
  else
    printf 'FAIL: %s (exit %s; output %s)\n' "$1" "$RUN_RC" "$RUN_OUT"
    failed=$((failed + 1))
  fi
}
exits() { [ "$RUN_RC" = "$1" ]; }
says() { grep -qF -- "$1" "$RUN_OUT"; }
says_not() { ! grep -qF -- "$1" "$RUN_OUT"; }
fails() { [ "$(grep -c '^FAIL: ' "$RUN_OUT")" = "$1" ]; }
# not_in_curl_args: no curl command line of the run (the fake's log of its arguments) holds text.
not_in_curl_args() { ! grep -qF -- "$1" "$state/curl.log"; }
# stack_url_in_env: the run's dev-stack.sh got the --database-url as LEGION_TEST_PG_DSN, and no
# argument of it holds the URL's password.
stack_url_in_env() {
  [ "$(cat "$state/dev-stack.dsn")" = "$admin_url" ] && ! grep -qF -- not-a-secret "$state/dev-stack.args"
}
# no_process_left: every process the run started was gone once the driver returned, its own
# cleanup's doing; what outlived it the test has already reaped (outlived).
no_process_left() { [ "$RUN_LEFT" = 0 ]; }

run_driver() { # <name> <scenario> [driver arguments...]
  local name="$1"
  printf '%s\n' "$2" >"$state/scenario"
  shift 2
  rm -f "$state"/approved-* "$state/queued" "$state/carol-off" "$state/priv-private" \
    "$state/sleeps" "$state/register.log" "$state/bun.log" "$state/psql.log" "$state/curl.log" \
    "$state/agent-secrets.log"
  RUN_OUT="$temporary_dir/$name.out"
  RUN_RC=0
  # env -i: the variables of whoever runs this test (a real AGENT_SECRETS_HELPER_SOCK, a real
  # LEGION_TEST_PG_DSN) must not reach the driver.
  env -i PATH="$fakes:$PATH" HOME="$temporary_dir" TMPDIR="$temporary_dir" bash "$driver" "$@" >"$RUN_OUT" 2>&1 || RUN_RC=$?
  RUN_LEFT="$(outlived)"
  check "$name: no process of the run outlived the driver ($RUN_LEFT reaped)" no_process_left
}

readonly all_rows="project-secrets-check: 16 rows ran, 16 passed"

# The right answers, before avenue F's machine check: every row passes and says which branch the
# dev-box row took.
run_driver before-f before-f --database-url "$admin_url"
check "before F: the run exits 0" exits 0
check "before F: the last line counts 16 rows, all passed" says "$all_rows"
check "before F: the pod's trade is named [PRIV]" says "PASS: PRIV's pod: its trade: project scope named, [\"PRIV\"]"
check "before F: the pod is granted PRIV_KEY" says "PASS: PRIV's pod: PRIV_KEY: granted"
check "before F: the pod is granted PUB_KEY" says "PASS: PRIV's pod: PUB_KEY: granted"
check "before F: carol's dev-box row says it ran the before-F branch" says "PASS: carol's session listing PRIV: its trade: before F's check: project scope person, [\"PRIV\"] (D1's rule)"
check "before F: carol's session listing PRIV is granted PRIV_KEY" says "PASS: carol's session listing PRIV: PRIV_KEY: granted"
check "before F: alice's trade is refused, her person cannot see PRIV" says "PASS: alice's session listing PRIV: its trade: refused:"
check "before F: alice's enrollment is refused PROJECT_NOT_VISIBLE" says "PASS: alice's session listing PRIV: its enrollment: refused PROJECT_NOT_VISIBLE"
check "before F: carol naming nothing is granted PUB_KEY" says "PASS: carol's session naming nothing: PUB_KEY: granted"
check "before F: carol naming nothing is denied PRIV_KEY project-not-named" says "PASS: carol's session naming nothing: PRIV_KEY: denied project-not-named"
check "before F: the machine's own session is granted PUB_KEY" says "PASS: the machine's own session: PUB_KEY: granted"
check "before F: the machine's own session is denied PRIV_KEY" says "PASS: the machine's own session: PRIV_KEY: denied project-not-named"
check "before F: the airlock serves a proxied request while carol is on PRIV" says "PASS: PRIV's pod behind the airlock: PRIV_KEY: a proxied request is served"
check "before F: the airlock refuses the next proxied request once carol is off PRIV" says "PASS: PRIV's pod behind the airlock: PRIV_KEY: the next proxied request is refused 407"
check "before F: the write making PRIV private is marked as the stand-in for avenue A's route" says "the stand-in for PATCH /api/v1/projects/PRIV/access {\"visibility\":\"private\"}"
check "before F: the write taking carol off PRIV is marked as the stand-in for avenue A's route" says "the stand-in for PATCH /api/v1/projects/PRIV/access {\"owner\":\"bob@example.com\"}"
check "before F: PRIV_KEY was written tagged dispatch-project=PRIV" \
  test "$(jq -r '.secrets[] | select(.name | endswith("/priv-key")) | .tags["dispatch-project"]' "$(cat "$state/current-stack")/fake-secrets.json")" = PRIV
check "before F: PUB_KEY was written with no project" \
  test "$(jq -r '.secrets[] | select(.name | endswith("/pub-key")) | .tags["dispatch-project"] // "none"' "$(cat "$state/current-stack")/fake-secrets.json")" = none
check "before F: PRIV_KEY was written tagged proxy=priv, the kind the banner exports" \
  test "$(jq -r '.secrets[] | select(.name | endswith("/priv-key")) | .tags.proxy // "none"' "$(cat "$state/current-stack")/fake-secrets.json")" = priv
check "before F: PUB_KEY was written with no proxy tag, so it is released as its value" \
  test "$(jq -r '.secrets[] | select(.name | endswith("/pub-key")) | .tags.proxy // "none"' "$(cat "$state/current-stack")/fake-secrets.json")" = none
check "before F: the driver waited out the broker's 30 s K3 cache before the refused request" grep -qx '35' "$state/sleeps"
check "before F: alice's session named PRIV at enrollment" grep -qF 'register person=alice@example.com projects=PRIV' "$state/register.log"
check "before F: carol's session naming nothing named no project" grep -qx 'register person=carol@example.com projects=' "$state/register.log"
check "before F: the airlock calls reached curl (its log names the bound host)" grep -qF 'https://priv.airlock.test/ok' "$state/curl.log"
check "before F: no curl command line carried the grant's proxy password" not_in_curl_args pw-not-a-secret
check "before F: no curl command line carried the grant's placeholder" not_in_curl_args agent-secrets-placeholder-not-a-secret
check "before F: dev-stack.sh is given the --database-url in LEGION_TEST_PG_DSN and in none of its arguments" stack_url_in_env

# F's machine check present: the dev-box row passes on F's refusal alone, and says so.
run_driver f f --database-url "$admin_url"
check "F: the run exits 0" exits 0
check "F: the last line counts 16 rows, all passed" says "$all_rows"
check "F: carol's trade passes as refused by F's check" says "PASS: carol's session listing PRIV: its trade: refused by F's check (machine_not_registered)"
check "F: carol's PRIV_KEY passes as refused at enrollment by F's check" says "PASS: carol's session listing PRIV: PRIV_KEY: refused at enrollment by F's check (MACHINE_NOT_REGISTERED)"
check "F: no before-F line is printed" says_not "before F's check"

# One answer wrong at a time: each fails its own row, naming its caller and its secret, and no
# other row fails but the ones that row's answer feeds (a pod denied PRIV_KEY has no grant to
# spend behind the airlock either, so its two airlock rows fail with it).
while IFS='|' read -r scenario failing line; do
  run_driver "$scenario" "$scenario" --database-url "$admin_url"
  check "$scenario: the run exits 1" exits 1
  check "$scenario: the failure names the caller and the secret: $line" says "$line"
  check "$scenario: $failing row(s) failed" fails "$failing"
  check "$scenario: the last line counts $((16 - failing)) of 16 passed" says "project-secrets-check: 16 rows ran, $((16 - failing)) passed"
done <<'ROWS'
pod-scope-person|1|FAIL: PRIV's pod: its trade:
pod-priv-denied|3|FAIL: PRIV's pod: PRIV_KEY:
carol-other-list|1|FAIL: carol's session listing PRIV: its trade:
carol-other-refusal|1|FAIL: carol's session listing PRIV: its trade:
alice-minted|1|FAIL: alice's session listing PRIV: its trade:
alice-enrolled|1|FAIL: alice's session listing PRIV: its enrollment:
carol-none-priv-granted|1|FAIL: carol's session naming nothing: PRIV_KEY:
carol-none-pub-denied|1|FAIL: carol's session naming nothing: PUB_KEY:
machine-priv-granted|1|FAIL: the machine's own session: PRIV_KEY:
airlock-first-refused|1|FAIL: PRIV's pod behind the airlock: PRIV_KEY: a proxied request
airlock-still-served|1|FAIL: PRIV's pod behind the airlock: PRIV_KEY: the next proxied request
f-trade-only|2|FAIL: carol's session listing PRIV: PRIV_KEY:
ROWS

# The negative control: PRIV_KEY written with no tag reaches every caller, and the run fails
# naming the first one that should have been refused.
run_driver control before-f --database-url "$admin_url" --control untagged
check "control: the run exits 1" exits 1
check "control: PRIV_KEY was written with no project" \
  test "$(jq -r '.secrets[] | select(.name | endswith("/priv-key")) | .tags["dispatch-project"] // "none"' "$(cat "$state/current-stack")/fake-secrets.json")" = none
check "control: PRIV_KEY was still written behind the kind, proxy=priv" \
  test "$(jq -r '.secrets[] | select(.name | endswith("/priv-key")) | .tags.proxy // "none"' "$(cat "$state/current-stack")/fake-secrets.json")" = priv
check "control: the first caller that got PRIV_KEY is named" says "control: the first caller that got PRIV_KEY although it should have been refused: carol's session naming nothing"
check "control: that caller's row fails on PRIV_KEY" says "FAIL: carol's session naming nothing: PRIV_KEY:"

# A run whose rows matched nothing fails: a stack from before --project-access names none of
# PRIV's exports.
run_driver empty-banner empty-banner --database-url "$admin_url"
check "no rows: the run exits 1" exits 1
check "no rows: it says what it could not read" says "UNKNOWN: the stack's banner names no DEV_STACK_PRIV_PROJECT"
check "no rows: the last line says no row ran" says "project-secrets-check: 0 rows ran, 0 passed"

# A Dispatch without avenue A's columns: the stand-in fails, naming the route it stands in for,
# and no row runs.
run_driver no-access-schema no-access-schema --database-url "$admin_url"
check "no access schema: the run exits 1" exits 1
check "no access schema: the failure names avenue A's route the write stands in for" says "PATCH /api/v1/projects/PRIV/access"
check "no access schema: no row ran" says "project-secrets-check: 0 rows ran, 0 passed"

# Live mode refuses half its options, and every option of the stack. It takes no Dispatch URL:
# nothing of the session half it drives reads one.
run_driver live-broker-only before-f --broker-url https://broker.example
check "live: --broker-url alone exits 2" exits 2
check "live: --broker-url alone names the two options" says "live mode needs --broker-url and --secret together"
run_driver live-secret-only before-f --secret PRIV_KEY
check "live: --secret alone exits 2" exits 2
run_driver live-dispatch-url before-f --broker-url https://broker.example --dispatch-url https://dispatch.example --secret PRIV_KEY
check "live: --dispatch-url exits 2" exits 2
check "live: --dispatch-url is no option" says "unexpected argument --dispatch-url"
run_driver live-control before-f --broker-url https://broker.example --secret PRIV_KEY --control untagged
check "live: --control exits 2" exits 2
check "live: --control is refused because it writes" says "--control writes a secret, which live mode never does"
run_driver live-database before-f --broker-url https://broker.example --secret PRIV_KEY --database-url "$admin_url"
check "live: --database-url exits 2" exits 2
check "live: --database-url is refused because live mode starts no stack" says "live mode starts no stack"
run_driver live-no-env before-f --broker-url https://broker.example --secret PRIV_KEY
check "live: with none of its variables it exits 2" exits 2
check "live: it names the pod's exchange token variable" says "AGENT_SECRETS_MACHINE_TOKEN_FILE"
check "live: it names the pod's key dir variable" says "AGENT_SECRETS_KEY_DIR"
check "live: it names the operator's helper socket variable" says "AGENT_SECRETS_HELPER_SOCK"

# Live mode on a deployment with F's check, from an environment holding the pod's token and key
# dir, an account token and the operator's helper: the pod's half passes, and the operator's
# session listing the project the pod's introspection named is refused by F.
live="$temporary_dir/live"
mkdir -p "$live/pod-keys" "$live/signins"
printf 'token\n' >"$live/priv-exchange-token"
printf 'token\n' >"$live/account-token"
printf 'key\n' >"$live/pod-keys/key.pem"
printf 'enr-pod\n' >"$live/pod-keys/enrollment"
printf '{"person": "carol@example.com"}\n' >"$live/signins/carol@example.com.json"
printf '%s\n' "$live/signins" >"$live/helper.sock"
cat >"$temporary_dir/fake-secrets.json" <<'JSON'
{"secrets": [{"name": "example/agent-secrets/priv-key", "tags": {"dispatch-project": "PRIV", "owner": "shared", "tier": "agent"}}]}
JSON
printf '%s\n' "$temporary_dir" >"$state/current-stack"
run_live() { # <name> <account token file>
  printf 'f\n' >"$state/scenario"
  rm -f "$state/register.log"
  RUN_OUT="$temporary_dir/$1.out"
  RUN_RC=0
  env -i PATH="$fakes:$stackbin:$PATH" HOME="$temporary_dir" TMPDIR="$temporary_dir" \
    AGENT_SECRETS_MACHINE_IDENTITY=kubernetes AGENT_SECRETS_MACHINE_TOKEN_FILE="$live/priv-exchange-token" \
    AGENT_SECRETS_KEY_DIR="$live/pod-keys" AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$2" \
    AGENT_SECRETS_HELPER_SOCK="$live/helper.sock" \
    bash "$driver" --broker-url https://broker.example --secret PRIV_KEY \
    >"$RUN_OUT" 2>&1 || RUN_RC=$?
  RUN_LEFT="$(outlived)"
  check "$1: no process of the run outlived the driver ($RUN_LEFT reaped)" no_process_left
}
run_live live-f "$live/account-token"
check "live, F: the run exits 0" exits 0
check "live, F: three rows ran and passed" says "project-secrets-check: 3 rows ran, 3 passed"
check "live, F: the pod's trade names the project" says "PASS: the pod: its trade: project scope named, [\"PRIV\"]"
check "live, F: the pod is granted the secret" says "PASS: the pod: PRIV_KEY: granted"
check "live, F: the operator's session is refused by F" says "PASS: the operator's machine session listing PRIV: PRIV_KEY: refused at enrollment by F's check (MACHINE_NOT_REGISTERED)"
check "live, F: the operator's session listed the project the pod's introspection named" grep -qF 'projects=PRIV' "$state/register.log"
run_live live-no-account ""
check "live, no account token: the run exits 1" exits 1
check "live, no account token: the trade line is UNKNOWN, not a pass" says "FAIL: the pod: its trade: UNKNOWN: cannot read the introspection"

check "no process of any run is left when the test ends" test "$(outlived)" = 0

printf 'project-secrets-check.test: %d passed, %d failed (fixtures kept in %s)\n' "$passed" "$failed" "$temporary_dir"
[ "$failed" -eq 0 ]
