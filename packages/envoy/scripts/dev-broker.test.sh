#!/usr/bin/env bash
# packages/envoy/scripts/dev-broker.test.sh
#
# Proves dev-broker.sh's per-instance database and port isolation without a real Postgres, real
# network binds, or a real broker binary: fakes `docker`, `go`, and `curl` on PATH — recording
# every docker/curl invocation to a call-log file and answering it without touching anything real
# — plus a fake `go build -o <path> ...` that installs
# a tiny stub script at <path> instead of compiling (dev-broker.sh's own isolation logic lives
# entirely in the shell driver, never in the Go binaries it starts, so a stub proves the driver's
# behavior without depending on cmd/cipher-devrelay or cmd/cipherd compiling or running for
# real). The broker stub logs the JSON "broker listening" line, addr 127.0.0.1:<its own PID>,
# exactly like the real cmd/cipherd does once it binds — using its own PID as a stand-in for the
# kernel-assigned port a real Listen would report, since two concurrently running stub processes
# always have distinct PIDs — and then its "broker metrics listening" line, addr
# 127.0.0.1:2<its own PID>, for the metrics port bound beside it.
#
# Runs two instances of dev-broker.sh concurrently — the actual scenario
# BROKER_LISTEN_ADDR=127.0.0.1:0 exists to make safe — each with its own docker/curl call-log
# files, and asserts that: (a) each instance issues its own "create database dev_broker_<suffix>"
# call, never the literal "dispatch"; (b) the two instances mint different database names; (c)
# each instance's own EXIT-trap cleanup drops exactly the database it created, before its process
# ever exits; (d) each instance's exported AGENT_SECRETS_URL, and the address it curls for its own
# readiness check, both name the exact port its own broker logged as bound — never a different,
# stale, or script-precomputed value, the exact property that closes the race where a
# port collision let a losing instance report "ready" while a different, already-running
# instance's broker actually answered; (e) the two concurrent instances bind two different
# ports; (f) each instance's broker trusts the dev-oidc-issuer that instance started, lists
# its dev caller, its exchange account and its read entry (BROKER_EXCHANGE_READ_ACCOUNTS),
# exports the caller token that issuer wrote, has that issuer write the read entry's pod token
# at the exchange's audience, names that token's file in its banner, and stops the
# issuer on exit; (g) that same issuer serves the code flow for the broker's own sign-in
# client and signs the approver in, the broker's BROKER_SIGNIN_* naming that issuer, client,
# domain and a 0600 secret file in the run's workdir, and the banner names the broker's own
# approval page; (h) each instance registers its one service under both halves a service
# needs - BROKER_SERVICE_LOGINS for the account that logs in as it and BROKER_SERVICES for the
# account its pods run as - and its banner says how to take the secret that service owns, with
# the pod uid its own issuer bound, exports that uid and that service name for a check script
# to enrol the same session by, and runs each of its github-app examples in a shell of its
# own, so the child expands the name the grant gave it; (i) each instance's broker binds its
# metrics on a port the kernel picks (BROKER_METRICS_LISTEN_ADDR=127.0.0.1:0), and its banner
# prints the metrics address that broker logged; (j) each instance's broker lists that
# service login's account as the registrar of its pod's account and of no other
# (BROKER_POD_REGISTRARS), without which the broker refuses the one registration the script
# makes, and exits on, 403 POD_REGISTRAR_NOT_LISTED; (k) each instance writes a profiles file
# that names every github-app secret of its store, DEMO_GITHUB_APP the source of the two proxied
# profiles, and starts its broker with it and with a proxy named by a front it bound first,
# pointed at the port the broker logged, signing with the stands' authority and outside any
# cluster although the run was started as a pod's, and stops that front on exit; (l)
# DEV_BROKER_PROXIED_GITHUB_IP puts the proxied GitHub on that address's port 443 and points the
# proxy at it, and DEV_BROKER_GITHUB_PERMISSIONS reaches the stands one owner at a time; (m)
# DEV_BROKER_BROKER_BIN is the broker started, and no broker is built; (n) each of those two
# settings, malformed, is refused with exit 2 before anything is created; and (o) lib/pg-url.sh
# gives psql a URL without its password and the password, decoded, in PGPASSWORD, so a
# DEV_BROKER_POSTGRES_URL run's password is in no argument of any call it makes and no line it
# prints.
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
readonly project_root
readonly driver="${project_root}/packages/envoy/scripts/dev-broker.sh"
temporary_dir="$(mktemp -d)"
readonly temporary_dir
readonly fake_bin_dir="${temporary_dir}/bin"
mkdir -p "$fake_bin_dir"

declare -a instance_pid=()
cleanup() {
  # Best-effort: if an assertion fails mid-test, don't leave a dev-broker.sh instance (and its
  # backgrounded fake broker) running.
  for pid in "${instance_pid[@]}"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$temporary_dir"
}
trap cleanup EXIT

# --- Fake docker: reports dispatch-pg already running on dev-postgres.sh's pinned image (so
# dev-postgres.sh reuses it) or, with DEV_BROKER_TEST_PG_IMAGE_ID set, on that other image (so
# dev-postgres.sh refuses it), answers every "exec ... pg_isready"/"exec ... psql -c ..." with
# success, answers dev-postgres.sh's read of the reused server's shared_preload_libraries with
# pg_stat_statements (the preload it requires to reuse a container), and records the exact
# command line of every invocation to the caller's own DEV_BROKER_TEST_DOCKER_CALL_FILE (each
# instance below gets its own, so two concurrent instances' calls never interleave in one file). ---
cat >"${fake_bin_dir}/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${DEV_BROKER_TEST_DOCKER_CALL_FILE:?}"
pinned_id="sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11"
case "$1 $2" in
  # dev-broker.sh no longer calls `ps`; this answer keeps the stale-image case red on a
  # dev-broker.sh that goes back to finding dispatch-pg with `docker ps` and reusing it unchecked.
  # Without it that version reads the container as absent, hands off to dev-postgres.sh, and passes.
  "ps "* | "ps -a") printf 'dispatch-pg\n' ;;
  "container inspect")
    case "$*" in
      *"{{.Image}}"*) printf '%s\n' "${DEV_BROKER_TEST_PG_IMAGE_ID:-$pinned_id}" ;;
      *"{{.State.Running}}"*) printf 'true\n' ;;
      *"{{.Config.Image}}"*) printf 'postgres:16\n' ;;
    esac
    ;;
  "image inspect") printf '%s\n' "$pinned_id" ;;
  "exec dispatch-pg")
    case "$*" in
      *" psql -h 127.0.0.1 -U postgres -Atc show shared_preload_libraries") printf 'pg_stat_statements\n' ;;
    esac
    ;;
esac
exit 0
EOF
chmod +x "${fake_bin_dir}/docker"

# --- Fake psql, for the DEV_BROKER_POSTGRES_URL runs: records every call to the instance's own
# call file (the one the fake docker writes), and the PGPASSWORD each call was given, one line per
# call, to DEV_BROKER_TEST_PSQL_PASSWORD_FILE when that is set; fails `create database` when
# DEV_BROKER_TEST_CREATE_FAILS is set (the name already taken by another run), and answers
# `select current_database()` with the database the URL in its first argument connects to, as
# libpq does: a dbname= in the query string wins over the URL's path. ---
cat >"${fake_bin_dir}/psql" <<'EOF'
#!/usr/bin/env bash
printf 'psql %s\n' "$*" >>"${DEV_BROKER_TEST_DOCKER_CALL_FILE:?}"
if [[ -n "${PGPASSWORD+set}" ]]; then
  printf 'PGPASSWORD=%s\n' "$PGPASSWORD"
else
  printf 'PGPASSWORD unset\n'
fi >>"${DEV_BROKER_TEST_PSQL_PASSWORD_FILE:-/dev/null}"
if [[ -n "${DEV_BROKER_TEST_CREATE_FAILS:-}" && "$*" == *"create database"* ]]; then
  echo 'ERROR:  database already exists' >&2
  exit 1
fi
if [[ -n "${DEV_BROKER_TEST_DROP_FAILS:-}" && "$*" == *"drop database"* ]]; then
  echo 'psql: error: connection to server failed: Connection refused' >&2
  exit 2
fi
if [[ "$*" == *"select current_database()"* ]]; then
  url=$1 query=""
  [[ "$url" == *\?* ]] && query=${url#*\?}
  if [[ "&$query" =~ \&dbname=([^&]+) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
  else
    path=${url%%\?*}
    printf '%s\n' "${path##*/}"
  fi
fi
exit 0
EOF
chmod +x "${fake_bin_dir}/psql"

# --- Fake go: "go build -o <path> ./cmd/<pkg>" installs a stub executable at <path> instead of
# compiling. The client stubs are never run by dev-broker.sh (it only prints their directory); the
# broker stub logs the issuer, the callers, the exchange's account and read entries, the service
# logins, the services, the pod registrars, the sign-in client and the metrics address it was
# configured with, then the JSON "broker listening" line with its addr field, exactly like the
# real cmd/cipherd does once it binds — using its own PID as a stand-in for the kernel-assigned
# port a real Listen would report, since two concurrently running stub processes always have
# distinct PIDs — and the "broker metrics listening" line after it, 2<PID> standing in for that
# port, with the "airlock proxy listening" line between the two for a broker given a profiles file
# that gives a profile a proxy block, 1<PID> standing in for its proxy's port, as the real broker
# binds a proxy only for a deployment whose profiles hold one, and then stays alive as the
# backgrounded "server" process dev-broker.sh waits on and kills during cleanup. The
# dev-oidc-issuer stub writes each --token's file, logs the --code-flow client, the mode of the
# secret file it would read, its callback and the --sign-in-as person, then logs
# "issuer listening url=..." as the real one does, its own PID standing in for its port the
# same way, and stays alive until cleanup kills it. The dev-airlock-stands stub serves its stands
# and its front mode alike (below). Each build names its package in the caller's own
# DEV_BROKER_TEST_GO_CALL_FILE, so a run that builds no broker of its own says so. ---
cat >"${fake_bin_dir}/go" <<'EOF'
#!/usr/bin/env bash
out=""
printf '%s\n' "${*: -1}" >>"${DEV_BROKER_TEST_GO_CALL_FILE:-/dev/null}"
pkg=""
prev=""
for arg in "$@"; do
  if [[ "$prev" == "-o" ]]; then
    out="$arg"
  fi
  pkg="$arg"
  prev="$arg"
done
case "$pkg" in
  */cipher|*/cipher-helper|*/cipher-devrelay)
    cat >"$out" <<'DEVRELAY'
#!/usr/bin/env bash
exit 0
DEVRELAY
    ;;
  */dev-oidc-issuer)
    cat >"$out" <<'ISSUER'
#!/usr/bin/env bash
client="" secret_file="" redirect_uri="" sign_in_as=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --token)
      audience_account_and_file="${2}"
      account_and_file="${2#*=}"
      printf 'fake-token-for-%s\n' "${account_and_file%%=*}" >"${account_and_file#*=}"
      # The real issuer's own line for each file it wrote, which dev-broker.sh reads the
      # pod uid of its pod token out of: `<file> holds <account>'s token for audience
      # <audience> (pod <uid>)`. One uid per account, as the real issuer binds one pod
      # per service account.
      account="${account_and_file%%=*}"
      echo "dev-oidc-issuer: ${account_and_file#*=} holds ${account}'s token for audience ${audience_account_and_file%%=*} (pod pod-uid-of-${account##*:})" >&2
      ;;
    --code-flow)
      client="${2%%=*}"
      secret_and_uri="${2#*=}"
      secret_file="${secret_and_uri%%=*}"
      redirect_uri="${secret_and_uri#*=}"
      ;;
    --sign-in-as) sign_in_as="$2" ;;
    *)
      echo "fake dev-oidc-issuer: unexpected argument $1" >&2
      exit 2
      ;;
  esac
  shift 2 || exit 2
done
# The real issuer reads the code flow's client secret as it parses --code-flow, and refuses a
# file it cannot read or one holding nothing but whitespace, so the file must be there by now.
secret_mode="empty-or-missing"
if [[ -s "$secret_file" ]]; then
  secret_mode="$(stat -c %a "$secret_file")"
fi
echo "fake issuer: code-flow client=$client secret-file=$secret_file secret-mode=$secret_mode redirect-uri=$redirect_uri sign-in-as=$sign_in_as" >&2
echo "dev-oidc-issuer: issuer listening url=http://127.0.0.1:$$" >&2
exec sleep 999999
ISSUER
    ;;
  */dev-proxy-stands)
    cat >"$out" <<'STANDS'
#!/usr/bin/env bash
# The front the broker's proxy is named by: it reports the address it bound, its own PID standing
# in for the port, logs the line dev-broker.sh waits for and the backends file it reads, and stays
# alive until cleanup stops it.
if [[ "${1:-}" == front ]]; then
  backends="" prev=""
  for arg in "$@"; do
    if [[ "$prev" == --backends-file ]]; then backends="$arg"; fi
    prev="$arg"
  done
  printf '{"address": "127.0.0.1:%s", "url": "http://127.0.0.1:%s"}\n' "$$" "$$"
  echo "fake front: pid=$$ backends-file=$backends" >&2
  echo "dev-airlock-stands: front listening addr=127.0.0.1:$$" >&2
  exec sleep 999999
fi
dir=""
owners=""
hosts=""
addresses=""
held=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir) dir="$2" ;;
    --installation) owners="${owners}${owners:+ }$2" ;;
    --host) hosts="${hosts}${hosts:+ }$2" ;;
    --address) addresses="${addresses}${addresses:+ }$2" ;;
    --installation-permissions) held="${held}${held:+ }$2" ;;
    *)
      echo "fake dev-airlock-stands: unexpected argument $1" >&2
      exit 2
      ;;
  esac
  shift 2 || exit 2
done
mkdir -p "$dir"
# The real command writes this run's GitHub App key and its certificate authority and names them
# in its report, and dev-broker.sh reads the key into its fake secret store and hands the
# authority to its broker, so a stub that wrote none would fail that read rather than the
# behaviour under test.
printf '%s\n' "-----BEGIN PRIVATE KEY-----" "the-stand-in-app-key" "-----END PRIVATE KEY-----" >"$dir/github-app.pem"
printf 'a fake authority certificate\n' >"$dir/ca.crt"
printf 'a fake authority key\n' >"$dir/ca.key"
# The report, with the fields dev-broker.sh reads; its own PID stands in for the ports the real
# command binds, as the broker and issuer stubs' do.
cat <<JSON
{"dir": "$dir",
 "ca_cert_file": "$dir/ca.crt",
 "ca_key_file": "$dir/ca.key",
 "records_url": "http://127.0.0.1:$$",
 "model_answer_url": "http://127.0.0.1:$$/answers/model",
 "github_answer_url": "http://127.0.0.1:$$/answers/github",
 "github_app_id": "123456",
 "github_app_key_file": "$dir/github-app.pem",
 "github_commit_sha": "5fd1c0a5a4e3b2c1d0e9f8a7b6c5d4e3f2a1b0c9",
 "stands": {"github": {"address": "127.0.0.1:$$", "url": "http://127.0.0.1:$$",
                       "records_url": "http://127.0.0.1:$$/records/github"},
            "github-tls": {"address": "127.0.0.1:1$$", "url": "https://127.0.0.1:1$$",
                           "records_url": "http://127.0.0.1:$$/records/github-tls"}}}
JSON
echo "fake stands: dir=$dir owners=[$owners] hosts=[$hosts] addresses=[$addresses] held=[$held]" >&2
echo "dev-airlock-stands: stands listening records=http://127.0.0.1:$$" >&2
exec sleep 999999
STANDS
    ;;
  */cipherd)
    cat >"$out" <<'BROKER'
#!/usr/bin/env bash
if [[ $# -ne 0 ]]; then
  echo "fake broker: unexpected arguments: $*" >&2
  exit 2
fi
echo "fake broker: issuer=${BROKER_K8S_OIDC_ISSUER:-} callers=${BROKER_API_ACCOUNTS:-} exchange=${BROKER_EXCHANGE_ACCOUNTS:-} read-exchange=${BROKER_EXCHANGE_READ_ACCOUNTS:-} service-logins=${BROKER_SERVICE_LOGINS:-} services=${BROKER_SERVICES:-} pod-registrars=${BROKER_POD_REGISTRARS:-} github=${BROKER_AIRLOCK_TEST_GITHUB_API_BASE:-}" >&2
echo "fake broker: sign-in issuer=${BROKER_SIGNIN_ISSUER:-} client=${BROKER_SIGNIN_CLIENT_ID:-} secret-file=${BROKER_SIGNIN_CLIENT_SECRET_FILE:-} domain=${BROKER_SIGNIN_DOMAIN:-}" >&2
echo "fake broker: metrics-listen=${BROKER_METRICS_LISTEN_ADDR:-}" >&2
echo "fake broker: store=${BROKER_SECRETS_STORE:-} prefix=${BROKER_SECRETS_PREFIX:-} key-arn=${BROKER_SECRETS_KMS_KEY_ARN:-} key-name=${BROKER_SECRETS_KMS_KEY_NAME:-} fake-secrets=${BROKER_FAKE_SECRETS_FILE:-}" >&2
echo "fake broker airlock: profiles=${BROKER_PROFILES_FILE:-} listen=${BROKER_AIRLOCK_LISTEN_ADDR:-} proxy_url=${BROKER_AIRLOCK_PROXY_URL:-} ca_cert=${BROKER_AIRLOCK_CA_CERT_FILE:-} ca_key=${BROKER_AIRLOCK_CA_KEY_FILE:-} upstream_ca=${BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE:-} resolve=[${BROKER_AIRLOCK_TEST_RESOLVE:-}] dial_allow=[${BROKER_AIRLOCK_TEST_DIAL_ALLOW:-}] kubernetes_service_host=[${KUBERNETES_SERVICE_HOST-unset}]" >&2
printf '{"time":"2026-01-01T00:00:00Z","severity":"INFO","message":"broker listening","addr":"127.0.0.1:%s"}\n' "$$" >&2
if [[ -n "${BROKER_PROFILES_FILE:-}" ]] && jq -e 'any(.profiles[]; .proxy != null)' "$BROKER_PROFILES_FILE" >/dev/null 2>&1; then
  printf '{"time":"2026-01-01T00:00:00Z","severity":"INFO","message":"airlock proxy listening","addr":"127.0.0.1:1%s"}\n' "$$" >&2
fi
printf '{"time":"2026-01-01T00:00:00Z","severity":"INFO","message":"broker metrics listening","addr":"127.0.0.1:2%s"}\n' "$$" >&2
exec sleep 999999
BROKER
    ;;
  *)
    echo "fake go: unhandled build target $pkg" >&2
    exit 1
    ;;
esac
chmod +x "$out"
EOF
chmod +x "${fake_bin_dir}/go"

# --- Fake curl: dev-broker.sh only ever curls its own broker's /healthz; record the exact URL
# (which names the port dev-broker.sh read from its own broker's log) to the caller's own
# DEV_BROKER_TEST_CURL_CALL_FILE and answer success at once, so the readiness loop never actually
# waits on or opens a socket. ---
cat >"${fake_bin_dir}/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${DEV_BROKER_TEST_CURL_CALL_FILE:?}"
exit 0
EOF
chmod +x "${fake_bin_dir}/curl"

# --- lib/pg-url.sh, the one split every psql call of dev-broker.sh (and of the scripts beside
# it) goes through. Each case runs the fake psql above through pg_url_run, in a caller whose own
# PGPASSWORD is a sentinel, and holds it to the arguments and the PGPASSWORD the fake was given,
# and pg_url_without_password to the URL a banner would print. ---
# shellcheck source-path=SCRIPTDIR source=lib/pg-url.sh
. "${project_root}/packages/envoy/scripts/lib/pg-url.sh"
readonly helper_calls="${temporary_dir}/helper-calls"
readonly callers_own=the-callers-own-not-a-secret
helper_case() { # <label> <url> <want URL argument> <want PGPASSWORD line>
  local label="$1" url="$2" want_url="$3" want_password="$4" got_call got_password shown
  : >"$helper_calls"
  : >"${helper_calls}.passwords"
  PGPASSWORD="$callers_own" PATH="${fake_bin_dir}:${PATH}" DEV_BROKER_TEST_DOCKER_CALL_FILE="$helper_calls" \
    DEV_BROKER_TEST_PSQL_PASSWORD_FILE="${helper_calls}.passwords" pg_url_run "$url" psql -Atqc 'select 1'
  got_call="$(<"$helper_calls")"
  got_password="$(<"${helper_calls}.passwords")"
  shown="$(pg_url_without_password "$url")"
  if [[ "$got_call" != "psql ${want_url} -Atqc select 1" || "$got_password" != "$want_password" || "$shown" != "$want_url" ]]; then
    echo "FAIL: pg_url_run, ${label}: psql got '${got_call}' with ${got_password}, and pg_url_without_password printed '${shown}'; want 'psql ${want_url} -Atqc select 1' with ${want_password}, and '${want_url}'" >&2
    exit 1
  fi
  printf 'PASS: pg_url_run, %s\n' "$label"
}
helper_case 'an encoded password: psql gets the URL without it, and the password decoded in PGPASSWORD' \
  'postgres://tester:not-a-secret%40%2Fw%25rd@127.0.0.1:5999/postgres?sslmode=disable' \
  'postgres://tester@127.0.0.1:5999/postgres?sslmode=disable' 'PGPASSWORD=not-a-secret@/w%rd'
helper_case 'a URL with no user and no password: psql gets it whole, and the caller'"'"'s own PGPASSWORD' \
  'postgres://127.0.0.1:5999/postgres?sslmode=disable' \
  'postgres://127.0.0.1:5999/postgres?sslmode=disable' "PGPASSWORD=$callers_own"
helper_case 'a password= in the query string, however its name is encoded: each one is taken out with its &, the last one is in PGPASSWORD, and every other parameter is kept' \
  'postgres://tester:not-a-secret-userinfo@127.0.0.1:5999/postgres?pass%77ord=not-a-secret-first&sslmode=disable&password=not-a-secret-q%26uery&application_name=a%3Db' \
  'postgres://tester@127.0.0.1:5999/postgres?sslmode=disable&application_name=a%3Db' 'PGPASSWORD=not-a-secret-q&uery'
helper_case 'a URL whose user has no password: psql gets it whole, and the caller'"'"'s own PGPASSWORD' \
  'postgres://tester@127.0.0.1:5999/postgres?sslmode=disable' \
  'postgres://tester@127.0.0.1:5999/postgres?sslmode=disable' "PGPASSWORD=$callers_own"
# A password libpq cannot decode is refused before psql runs: libpq would quote it in its error.
: >"$helper_calls"
helper_status=0
PATH="${fake_bin_dir}:${PATH}" DEV_BROKER_TEST_DOCKER_CALL_FILE="$helper_calls" \
  pg_url_run 'postgres://tester:not-a-secret%zz@127.0.0.1:5999/postgres' psql -Atqc 'select 1' 2>"${helper_calls}.err" || helper_status=$?
if [[ "$helper_status" -ne 2 || -s "$helper_calls" ]] || grep -qF 'not-a-secret' "${helper_calls}.err" \
  || ! grep -qF 'refused a Postgres URL: its password holds a % that starts no %XX escape, or %00 (the URL is not printed)' "${helper_calls}.err"; then
  cat "$helper_calls" "${helper_calls}.err" >&2
  echo "FAIL: pg_url_run, a password with a % that starts no escape: exited ${helper_status}; want 2, no psql call, and a refusal that names the fault and prints nothing of the URL" >&2
  exit 1
fi
printf 'PASS: pg_url_run, a password with a %% that starts no escape: refused with 2 before psql runs, printing nothing of the URL\n'
# The shared corpus, fixtures/pg-url.json, which scripts/search-measure/pg_test.py and
# packages/dispatch/e2e/__tests__/psql-password.test.ts read too, so the three splits are held to
# one table, each case of which was checked against libpq's own PQconninfoParse. Each accepted
# case goes through helper_case; each refused one must exit 2 before psql runs, naming its fault
# and printing nothing of the URL.
readonly corpus="${project_root}/packages/envoy/scripts/fixtures/pg-url.json"
corpus_cases=0
while IFS= read -r -d '' url && IFS= read -r -d '' bare && IFS= read -r -d '' password; do
  corpus_cases=$((corpus_cases + 1))
  if [[ "$password" == none ]]; then want="PGPASSWORD=$callers_own"; else want="PGPASSWORD=${password#=}"; fi
  helper_case "the shared corpus's accepted case ${corpus_cases} (${bare})" "$url" "$bare" "$want"
done < <(jq -j '.accepted[] | .url, "\u0000", .bare, "\u0000", (if .password == null then "none" else "=" + .password end), "\u0000"' "$corpus")
while IFS= read -r -d '' url && IFS= read -r -d '' fault; do
  corpus_cases=$((corpus_cases + 1))
  : >"$helper_calls"
  helper_status=0
  PATH="${fake_bin_dir}:${PATH}" DEV_BROKER_TEST_DOCKER_CALL_FILE="$helper_calls" \
    pg_url_run "$url" psql -Atqc 'select 1' 2>"${helper_calls}.err" || helper_status=$?
  if [[ "$helper_status" -ne 2 || -s "$helper_calls" ]] || grep -qF 'not-a-secret' "${helper_calls}.err" ||
    ! grep -qF "refused a Postgres URL: ${fault} (the URL is not printed)" "${helper_calls}.err"; then
    cat "$helper_calls" "${helper_calls}.err" >&2
    echo "FAIL: pg_url_run, the shared corpus's refused case ${corpus_cases} (${fault}): exited ${helper_status}; want 2, no psql call, and a refusal that names the fault and prints nothing of the URL" >&2
    exit 1
  fi
  printf 'PASS: pg_url_run, the shared corpus'"'"'s refused case %d: %s\n' "$corpus_cases" "$fault"
done < <(jq -j '.refused[] | .url, "\u0000", .fault, "\u0000"' "$corpus")
if [[ "$corpus_cases" -eq 0 ]]; then
  echo "FAIL: fixtures/pg-url.json gave no case" >&2
  exit 1
fi

# Under xtrace, pg-url.sh traces neither the password nor the URL that holds it. A caller run
# under bash -x hands pg_url_run and pg_url_without_password a URL whose password is
# percent-encoded, from its environment; the trace may hold neither the decoded password nor
# its encoded form, but in the caller's own call lines, which bash traces with their arguments
# before any function runs. The caller also shows that pg_url_run returned COMMAND's status and
# that tracing was back on after it.
readonly trace_dir="${temporary_dir}/xtrace"
mkdir -p "$trace_dir"
cat >"${trace_dir}/caller" <<'EOF'
#!/usr/bin/env bash
. "$PG_URL_LIB"
set -x
stand_in() { return 3; }
pg_url_run "$TRACE_URL" stand_in -c 'select 1'
status=$?
[[ $- == *x* ]] && traced=again
pg_url_without_password "$TRACE_URL" >/dev/null
set +x
printf 'status=%s traced=%s\n' "$status" "${traced:-off}"
EOF
trace_said="$(PG_URL_LIB="${project_root}/packages/envoy/scripts/lib/pg-url.sh" \
  TRACE_URL='postgres://tester:not-a-secret%40%2Ftrace@127.0.0.1:5999/postgres?sslmode=disable' \
  bash "${trace_dir}/caller" 2>"${trace_dir}/trace")"
traced_secret="$(grep -n -e 'not-a-secret@/trace' -e 'not-a-secret%40%2Ftrace' "${trace_dir}/trace" |
  grep -v -E '^[0-9]+:\++ (pg_url_run|pg_url_without_password) ' || true)"
if [[ "$trace_said" != "status=3 traced=again" || -n "$traced_secret" ]]; then
  echo "FAIL: pg-url.sh under xtrace: the caller said '${trace_said}' (want 'status=3 traced=again'), and these trace lines hold the password: ${traced_secret:-none}" >&2
  exit 1
fi
printf 'PASS: pg-url.sh under xtrace traces neither the password nor its encoded form, returns COMMAND'"'"'s status and turns tracing back on\n'

# Sets the global instance_pid array (never wrapped in a subshell: the EXIT trap's safety-net
# kill must see the same variable this assignment updates). The log is emptied here, before the
# fork, not only by the background job's own redirect: a reused log still holds the previous
# run's "dev-broker: ready." line until that job runs, and wait_for_ready would read it. Each
# instance makes its workdir under TMPDIR, and dev-broker.sh keeps that workdir when it exits, so
# TMPDIR names this test's own directory and the EXIT trap's rm -rf takes every instance's workdir
# with it.
run_instance() {
  local which="$1" log_file="$2" docker_call_file="$3" curl_call_file="$4" postgres_url="${5:-}"
  : >"$log_file"
  : >"$docker_call_file"
  : >"${docker_call_file}.passwords"
  : >"$curl_call_file"
  : >"${docker_call_file}.go"
  # KUBERNETES_SERVICE_HOST is set as Kubernetes sets it in every container of a pod, which the
  # broker, a local process with a proxy, must not inherit.
  PATH="${fake_bin_dir}:${PATH}" DEV_BROKER_POSTGRES_URL="$postgres_url" TMPDIR="$temporary_dir" \
    DEV_BROKER_TEST_DOCKER_CALL_FILE="$docker_call_file" DEV_BROKER_TEST_CURL_CALL_FILE="$curl_call_file" \
    DEV_BROKER_TEST_GO_CALL_FILE="${docker_call_file}.go" KUBERNETES_SERVICE_HOST=34.118.224.1 \
    DEV_BROKER_TEST_PSQL_PASSWORD_FILE="${docker_call_file}.passwords" \
    bash "$driver" >"$log_file" 2>&1 &
  instance_pid[which]=$!
}

# Waits up to 30 s for an instance to print its ready line: the stack's own waits poll every half
# second (the stands, the proxy's front and each of the broker's three listening lines), so on a
# loaded machine the fakes take seconds to get through them, and dev-airlock.test.sh's wait for
# that same stack has the same bound.
wait_for_ready() {
  local log_file="$1"
  for _ in $(seq 1 300); do
    grep -q 'dev-broker: ready\.' "$log_file" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

# Waits up to 5 s for instance <n> to exit by itself and sets the global "exited_status" to its
# exit status. A run meant to refuse that is still running then went on to serve: it is killed
# and "exited_status" stays 0, which each caller reads as the failure, so the wait is bounded and
# reports that case rather than hanging.
exited_status=0
wait_for_exit() {
  local n="$1"
  local pid="${instance_pid[$n]}"
  exited_status=0
  for _ in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  else
    wait "$pid" || exited_status=$?
  fi
  instance_pid[n]=""
}

created_database() {
  local docker_call_file="$1"
  # `|| true`: under this script's own set -o pipefail, grep -o finding nothing (the exact
  # regression this function exists to detect, e.g. dev-broker.sh reverted to the shared literal
  # "dispatch" database) exits 1, and since head/awk on empty input both exit 0, that 1 becomes
  # the whole pipeline's status. Force success here so an absent match reaches the caller as an
  # empty string, which it already checks for explicitly.
  grep -o 'create database dev_broker_[a-z0-9]\+' "$docker_call_file" | head -n1 | awk '{print $3}' || true
}

logged_port() {
  # The fake broker's own JSON "broker listening" line, its addr 127.0.0.1:<port>, landing in the
  # instance's combined stdout/stderr log exactly as dev-broker.sh's own tee writes it.
  local log_file="$1"
  grep -o '"message":"broker listening","addr":"127\.0\.0\.1:[0-9]\+"' "$log_file" | head -n1 | sed -E 's/.*:([0-9]+)"$/\1/' || true
}

curled_port() {
  # See created_database's own comment: `|| true` keeps a no-match here from tripping this
  # script's own errexit before the caller's own emptiness check can print its diagnostic.
  local curl_call_file="$1"
  grep -o 'http://127\.0\.0\.1:[0-9]\+/healthz' "$curl_call_file" | head -n1 | sed -E 's#.*:([0-9]+)/healthz#\1#' || true
}

exported_port() {
  # dev-broker.sh's own printed "export AGENT_SECRETS_URL=http://127.0.0.1:<port>" line: the
  # value a human or agent-secrets would actually be told to use.
  local log_file="$1"
  grep -o 'AGENT_SECRETS_URL=http://127\.0\.0\.1:[0-9]\+' "$log_file" | head -n1 | sed -E 's/.*:([0-9]+)$/\1/' || true
}

# Sets the global "db"/"port" variables to the database and port this instance used; exits the
# whole test on any assertion failure.
db=""
port=""
assert_instance_isolated() {
  local n="$1" label="$2" log_file="$3" docker_call_file="$4" curl_call_file="$5"
  # How this instance's cleanup drops a database: through docker by default, or through psql on
  # the admin URL in a DEV_BROKER_POSTGRES_URL run.
  local drop_prefix="${6:-exec dispatch-pg psql -U postgres -v ON_ERROR_STOP=1 -q}"
  if ! wait_for_ready "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label never reached ready" >&2
    exit 1
  fi

  db="$(created_database "$docker_call_file")"
  if [[ -z "$db" ]]; then
    cat "$docker_call_file" >&2
    echo "FAIL: $label never issued a create database call" >&2
    exit 1
  fi
  if [[ "$db" == "dispatch" ]]; then
    echo "FAIL: $label created the shared dispatch database instead of an isolated one" >&2
    exit 1
  fi
  printf 'PASS: %s creates isolated database %s\n' "$label" "$db"

  local bound curled exported
  bound="$(logged_port "$log_file")"
  if [[ -z "$bound" ]]; then
    cat "$log_file" >&2
    echo "FAIL: $label's broker never logged its bound address" >&2
    exit 1
  fi
  curled="$(curled_port "$curl_call_file")"
  if [[ -z "$curled" ]]; then
    cat "$curl_call_file" >&2
    echo "FAIL: $label never issued a healthz curl call" >&2
    exit 1
  fi
  if [[ "$curled" != "$bound" ]]; then
    echo "FAIL: $label curled port $curled for readiness, but its own broker logged binding $bound" >&2
    exit 1
  fi
  exported="$(exported_port "$log_file")"
  if [[ -z "$exported" ]]; then
    cat "$log_file" >&2
    echo "FAIL: $label never printed an AGENT_SECRETS_URL export" >&2
    exit 1
  fi
  if [[ "$exported" != "$bound" ]]; then
    echo "FAIL: $label exported port $exported, but its own broker logged binding $bound" >&2
    exit 1
  fi
  printf 'PASS: %s exports the exact port its own broker logged as bound (%s)\n' "$label" "$bound"
  # The metrics port: bound on a port the kernel picks, since a fixed 13382 is a port only one of
  # the two concurrent instances could hold, and named in the banner as the address the broker
  # logged for it, which is the one a reader scrapes.
  local metrics
  metrics="$(grep -o '"message":"broker metrics listening","addr":"127\.0\.0\.1:[0-9]\+"' "$log_file" | head -n1 | sed -E 's/.*"addr":"([^"]+)"$/\1/' || true)"
  if [[ -z "$metrics" ]] || ! grep -qF "fake broker: metrics-listen=127.0.0.1:0" "$log_file" \
    || ! grep -qF "export DEV_BROKER_METRICS_URL=http://${metrics}/metrics" "$log_file" \
    || ! grep -qE "^  metrics: +http://${metrics//./\\.}/metrics " "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label does not start its broker with BROKER_METRICS_LISTEN_ADDR=127.0.0.1:0 and print the metrics address it logged (${metrics:-none logged})" >&2
    exit 1
  fi
  printf 'PASS: %s binds its broker'"'"'s metrics on a port the kernel picks and prints the address it logged (%s)\n' "$label" "$metrics"
  if ! grep -q "database: *postgres://[^ ]*/${db}[? ]" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's broker database URL does not name its own database ($db)" >&2
    exit 1
  fi
  printf 'PASS: %s points its broker at its own database\n' "$label"
  # With no DEV_BROKER_GCP_* the store is the AWS-shaped fake one, as before those settings.
  if ! grep -qF "fake broker: store= prefix=example/agent-secrets/ key-arn=arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab key-name= " "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's broker is not started on the AWS-shaped fake store" >&2
    exit 1
  fi
  printf 'PASS: %s starts its broker on the AWS-shaped fake store\n' "$label"
  # Its broker trusts the issuer this instance started, lists the dev account as the caller
  # "dispatch" and the listener account as the caller "listener" (introspection alone), the
  # worker account at the exchange and the reader account as the exchange's read entry, and the
  # caller token devrelay is told to read is the one that issuer wrote for the listed account.
  local issuer token_file
  issuer="$(grep -o 'issuer listening url=http://127\.0\.0\.1:[0-9]\+' "$log_file" | head -n1 | cut -d= -f2- || true)"
  if [[ -z "$issuer" ]] || ! grep -qF "fake broker: issuer=${issuer} callers=dispatch=system:serviceaccount:example-ns:example-dispatch listener=system:serviceaccount:example-ns:example-listener exchange=system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com read-exchange=system:serviceaccount:example-ns:example-reader=example-reader=example-reader-session" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's broker does not trust its own issuer (${issuer:-none logged}) with both dev callers, the exchange account and the read entry listed" >&2
    exit 1
  fi
  token_file="$(grep -o 'AGENT_SECRETS_ACCOUNT_TOKEN_FILE=[^ ]*' "$log_file" | head -n1 | cut -d= -f2- || true)"
  if [[ -z "$token_file" ]] || [[ "$(cat "$token_file" 2>/dev/null)" != "fake-token-for-system:serviceaccount:example-ns:example-dispatch" ]]; then
    echo "FAIL: $label's AGENT_SECRETS_ACCOUNT_TOKEN_FILE (${token_file:-none printed}) does not hold the token its issuer wrote for the listed caller" >&2
    exit 1
  fi
  printf 'PASS: %s trusts its own issuer (%s), lists its caller, and exports the caller token that issuer wrote\n' "$label" "$issuer"
  # The same issuer process signs people in for the broker's own client: the issuer serves the
  # code flow for the client and secret file the broker names, a file in this run's own workdir
  # that was there, non-empty and readable by its owner alone, before the issuer started, and
  # signs in the approver whose own domain the broker holds a signed-in person to; the secret
  # itself is never printed; and the banner tells a person to approve a secret on this broker's
  # own page.
  local workdir signin_file
  workdir="$(grep -o 'dev-broker: workdir [^ ]*' "$log_file" | head -n1 | cut -d' ' -f3 || true)"
  signin_file="${workdir}/signin-client-secret"
  if [[ -z "$workdir" ]] \
    || ! grep -qF "fake issuer: code-flow client=dev-broker secret-file=${signin_file} secret-mode=600 redirect-uri=http://127.0.0.1/signin/callback sign-in-as=ada@example.com" "$log_file" \
    || ! grep -qF "fake broker: sign-in issuer=${issuer} client=dev-broker secret-file=${signin_file} domain=example.com" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's issuer and broker do not share one sign-in client with a 0600 secret in ${workdir:-its workdir}" >&2
    exit 1
  fi
  if grep -qF "$(cat "$signin_file")" "$log_file"; then
    echo "FAIL: $label printed its sign-in client's secret" >&2
    exit 1
  fi
  if ! grep -qF "(http://127.0.0.1:${bound}/approve/<record>)" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label does not tell a person to approve a secret on its own broker's page (port $bound)" >&2
    exit 1
  fi
  printf 'PASS: %s signs people in to its broker at that same issuer, with a 0600 secret it never prints, and names its broker'"'"'s approval page\n' "$label"

  # The read entry's pod token. The issuer mints it for the reader account at the exchange's own
  # audience, the one the exchange token carries, into the workdir's read-token, and binds it to a
  # pod of its own; the banner names that file and says what the token it exchanges for may do.
  # A token minted for the broker's audience instead would be refused at the exchange, which takes
  # only the exchange's audience.
  local reader="system:serviceaccount:example-ns:example-reader"
  local read_token="${workdir}/read-token"
  if [[ "$(cat "$read_token" 2>/dev/null)" != "fake-token-for-${reader}" ]] \
    || ! grep -qF "dev-oidc-issuer: ${read_token} holds ${reader}'s token for audience legion-broker-exchange (pod pod-uid-of-${reader##*:})" "$log_file" \
    || ! grep -qF "  read token:          ${read_token}  (${reader}'s pod, audience legion-broker-exchange: exchanges under example-reader-session for a token of the scope read alone, which Dispatch serves on every GET route it classes as a read and answers 403 READ_ONLY_TOKEN on every other route)" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's issuer does not write the read entry's pod token at the exchange's audience into ${read_token}, or its banner does not name that file and what its token may do" >&2
    exit 1
  fi
  printf 'PASS: %s has its issuer write the read entry'"'"'s pod token for the exchange and names that file in its banner\n' "$label"

  # The registered service's two halves. A stack that logs the daemon account in
  # (BROKER_SERVICE_LOGINS) and binds no pod account to that service (BROKER_SERVICES) serves no
  # secret the service owns - the policy refuses the owner tag as malformed on every reread -
  # and enrols no pod of that account (403 POD_NOT_LAUNCHED), so the two are asserted together,
  # with the pod account the issuer minted the pod token for. The banner's enrolment command
  # names that pod by the uid the issuer bound it to, so a reader takes the service-owned secret
  # by pasting it.
  local service="legion-daemon"
  local registrar="system:serviceaccount:example-ns:example-daemon"
  local pod_account="system:serviceaccount:example-ns:example-worker"
  if ! grep -qF "service-logins=${service}=${registrar} services=${service}=${pod_account}" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's broker does not register ${service} with both halves (the login's account and its pods')" >&2
    exit 1
  fi
  # `--` before each pattern: these start with a dash, which grep would otherwise read as its
  # own option.
  if ! grep -qF -- "--pod-uid pod-uid-of-${pod_account##*:}" "$log_file" \
    || ! grep -qF -- "--service ${service}" "$log_file" \
    || ! grep -qF -- "DEMO_SERVICE_TOKEN" "$log_file" \
    || ! grep -qF "export DEV_BROKER_POD_UID=pod-uid-of-${pod_account##*:}" "$log_file" \
    || ! grep -qF "export DEV_BROKER_SERVICE=${service}" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's banner does not say how to take its service-owned secret as a pod of ${service}, or exports no pod uid and service name for it" >&2
    exit 1
  fi
  printf 'PASS: %s registers %s under both halves, tells a reader how to take the secret it owns, and exports that pod uid and service name\n' "$label" "$service"
  # The registration's authority. A login alone registers no pod: the broker admits a
  # registration only from a registrar an entry of BROKER_POD_REGISTRARS pairs with the pod's
  # account, and with the list unset it admits none, so a stack that logged the daemon account
  # in and listed it nowhere would be refused the one registration it makes (403
  # POD_REGISTRAR_NOT_LISTED) and exit there. The value is matched whole, up to the field after
  # it, so a list holding any other entry beside this pair fails too.
  if ! grep -qF " pod-registrars=${registrar}=${pod_account} github=" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's broker does not list ${registrar} as the one registrar of ${pod_account}'s pods (want BROKER_POD_REGISTRARS=${registrar}=${pod_account})" >&2
    exit 1
  fi
  printf 'PASS: %s lists %s as the one registrar of %s'"'"'s pods (BROKER_POD_REGISTRARS)\n' "$label" "$registrar" "$pod_account"
  # The stand-in GitHub and the three github-app secrets it mints: the stands are started for the
  # three installations the case pair needs, the broker is pointed at that stand's own address,
  # and the fake store holds all three with the type, the tier, the owner and the github-owners
  # tag each one's release rests on - the two the registered service owns, which a pod session of
  # it gets without asking, and the approver's own human-tier one, whose release a person
  # decides. The App key is the file the stands reported, so a report that named another file
  # would leave the store holding something else.
  local stands_url stands_owners
  stands_url="$(grep -o 'github stand: *http://127\.0\.0\.1:[0-9]\+' "$log_file" | head -n1 | grep -o 'http://.*' || true)"
  stands_owners="$(grep -o 'fake stands: dir=[^ ]* owners=\[[^]]*\]' "$log_file" | head -n1 | sed -E 's/.*owners=\[(.*)\]$/\1/' || true)"
  if [[ "$stands_owners" != "acme-org=7 ada=8 Ada=9" ]] || [[ -z "$stands_url" ]] \
    || ! grep -qF "github=${stands_url}" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's broker does not mint github-app secrets at its own stand-in (owners '${stands_owners:-none}', stand ${stands_url:-none})" >&2
    exit 1
  fi
  local tags want_tags
  tags="$(jq -r '
    [.secrets[] | select(.tags.type == "github-app")
     | "\(.name) \(.tags.type) \(.tags.tier) \(.tags.owner) \(.tags["github-owners"]) \(.value | fromjson | .app_id)"] | sort | join("; ")
    ' "${workdir}/fake-secrets.json" 2>/dev/null || true)"
  want_tags="example/agent-secrets/demo-github-app github-app agent legion-daemon acme-org 123456"
  want_tags="${want_tags}; example/agent-secrets/demo-github-app-case-pair github-app agent legion-daemon ada 123456"
  want_tags="${want_tags}; example/agent-secrets/demo-github-app-human github-app human ada@example.com acme-org 123456"
  if [[ "$tags" != "$want_tags" ]]; then
    echo "FAIL: $label's fake store holds no such three github-app secrets: ${tags:-none}" >&2
    exit 1
  fi
  if ! grep -qF -- "-----BEGIN PRIVATE KEY-----" <(jq -r --arg prefix "example/agent-secrets/" '.secrets[] | select(.name == ($prefix + "demo-github-app")) | .value | fromjson | .private_key' "${workdir}/fake-secrets.json"); then
    echo "FAIL: $label's github-app secret holds no App key from the stands' report" >&2
    exit 1
  fi
  # The banner exports the name of each github-app secret, the approver's own beside the two the
  # service owns, since a check drives each of them by the name this export names.
  if ! grep -qF "export DEV_BROKER_GITHUB_SECRET=DEMO_GITHUB_APP" "$log_file" \
    || ! grep -qF "export DEV_BROKER_GITHUB_CASE_SECRET=DEMO_GITHUB_APP_CASE_PAIR" "$log_file" \
    || ! grep -qF "export DEV_BROKER_GITHUB_HUMAN_SECRET=DEMO_GITHUB_APP_HUMAN" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's banner does not export the name of each of its three github-app secrets" >&2
    exit 1
  fi
  printf 'PASS: %s starts a stand-in GitHub for three installations, mints against it, holds its three github-app secrets and exports each name\n' "$label"
  # The profiles file the broker is given: each github-app secret a profile of its own name, and
  # DEMO_GITHUB_APP the source of the two proxied ones, each the one account's bare token bound
  # to the github-tls stand's name for GET and HEAD alone. Every github-app secret of the store
  # is a profile's, since the broker refuses one no profile names (github-app-no-profile), and
  # the stands' certificate for github-tls carries the name the proxied ones bind. With no
  # DEV_BROKER_PROXIED_GITHUB_IP the github-tls stand keeps a port the system picks and the broker
  # is told of no address for that name, and with no DEV_BROKER_GITHUB_PERMISSIONS the stands are
  # given no map, so each installation holds the stand's own.
  local profiles want_profiles unnamed
  profiles="$(jq -r '[.profiles[] | "\(.name) \(.source // .name) \(.github.value // "installations") \((.proxy.hosts // []) | join(",")) \((.proxy.allow // []) | map(.method) | join(","))"] | join("; ")' "${workdir}/profiles.json" 2>/dev/null || true)"
  want_profiles="DEMO_GITHUB_APP DEMO_GITHUB_APP installations  ; DEMO_GITHUB_READ DEMO_GITHUB_APP token api.github.test GET,HEAD"
  want_profiles="${want_profiles}; DEMO_GITHUB_ADMIN_READ DEMO_GITHUB_APP token api.github.test GET,HEAD"
  want_profiles="${want_profiles}; DEMO_GITHUB_APP_CASE_PAIR DEMO_GITHUB_APP_CASE_PAIR installations  ; DEMO_GITHUB_APP_HUMAN DEMO_GITHUB_APP_HUMAN installations  "
  unnamed="$(jq -rn --slurpfile store "${workdir}/fake-secrets.json" --slurpfile file "${workdir}/profiles.json" '
    ([$file[0].profiles[] | (.source // .name)]) as $named
    | [$store[0].secrets[] | select(.tags.type == "github-app") | .name | sub("^example/agent-secrets/"; "") | ascii_upcase | gsub("-"; "_")
       | select(. as $n | $named | index($n) | not)] | join(" ")' 2>/dev/null || echo unread)"
  if [[ "$profiles" != "$want_profiles" ]] || [[ -n "$unnamed" ]] \
    || ! grep -qF "fake stands: dir=${workdir}/stands owners=[acme-org=7 ada=8 Ada=9] hosts=[github-tls=api.github.test] addresses=[] held=[]" "$log_file" \
    || ! grep -qF "export DEV_BROKER_PROFILES_FILE=${workdir}/profiles.json" "$log_file" \
    || ! grep -qF "export DEV_BROKER_GITHUB_READ_PROFILE=DEMO_GITHUB_READ" "$log_file" \
    || ! grep -qF "export DEV_BROKER_GITHUB_ADMIN_READ_PROFILE=DEMO_GITHUB_ADMIN_READ" "$log_file" \
    || ! grep -qF "export DEV_BROKER_GITHUB_PROXIED_URL=https://api.github.test" "$log_file" \
    || ! grep -qF "export DEV_BROKER_GITHUB_COMMIT=5fd1c0a5a4e3b2c1d0e9f8a7b6c5d4e3f2a1b0c9" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's profiles file is [${profiles:-unread}], want [$want_profiles]; github-app secrets no profile names: [${unnamed}]" >&2
    exit 1
  fi
  printf 'PASS: %s writes a profiles file naming every github-app secret, with the two proxied profiles of DEMO_GITHUB_APP, and exports their names\n' "$label"
  # The broker's proxy, which a profiles file with a proxy block needs: the file, a port the
  # kernel picks, the URL of the front this run bound before the broker, the stands' authority to
  # sign with and trust upstream, no address for the proxied GitHub (none was asked for), and
  # KUBERNETES_SERVICE_HOST empty although the run was started with it set, as in a pod. The front
  # is pointed at the proxy port the broker logged, by its backends file.
  local front_pid front_backends broker_pid
  front_pid="$(grep -o 'fake front: pid=[0-9]\+' "$log_file" | head -n1 | cut -d= -f2 || true)"
  front_backends="$(grep -o 'fake front: pid=[0-9]* backends-file=[^ ]*' "$log_file" | head -n1 | sed -E 's/.*backends-file=//' || true)"
  broker_pid="$bound"
  if [[ -z "$front_pid" ]] \
    || ! grep -qF "fake broker airlock: profiles=${workdir}/profiles.json listen=127.0.0.1:0 proxy_url=http://127.0.0.1:${front_pid} ca_cert=${workdir}/stands/ca.crt ca_key=${workdir}/stands/ca.key upstream_ca=${workdir}/stands/ca.crt resolve=[] dial_allow=[] kubernetes_service_host=[]" "$log_file" \
    || ! grep -qF "export DEV_BROKER_PROXY_URL=http://127.0.0.1:${front_pid}" "$log_file" \
    || [[ "$front_backends" != "${workdir}/proxy-backends" ]] \
    || [[ "$(cat "$front_backends" 2>/dev/null)" != "127.0.0.1:1${broker_pid}" ]]; then
    cat "$log_file" >&2
    echo "FAIL: $label's broker is not started with its proxy behind the front this run bound (front pid ${front_pid:-none}, backends file ${front_backends:-none} holding '$(cat "$front_backends" 2>/dev/null)')" >&2
    exit 1
  fi
  printf 'PASS: %s starts its broker with the profiles file and a proxy named by the front it bound first, pointed at the proxy port the broker logged, outside any cluster\n' "$label"
  # The two examples that take a github-app profile run a shell of their own, so that shell
  # expands the name the grant put in the child's environment. An example that printed the name
  # with a backslash before it, outside any shell, would hand curl or jq those characters, and
  # the stand would answer 401 to a token it never minted. The proxied one's header reaches curl
  # on standard input, so not even its placeholder is in a command line, and the other prints the
  # installation ids alone, never a token. grep -F reads a newline as another pattern, so the one
  # line that carries the shell is the pattern here.
  local github_example proxied_example
  github_example=$'sh -c \'printf %s "$DEMO_GITHUB_APP" | jq ".installations | map_values(.installation_id)"\''
  proxied_example=$'sh -c \'printf "Authorization: Bearer %s" "$DEMO_GITHUB_READ" | curl -fsS -H @- https://api.github.test/repos/acme-org/proofs\''
  if ! grep -qF -- "$github_example" "$log_file" || ! grep -qF -- "$proxied_example" "$log_file"; then
    cat "$log_file" >&2
    echo "FAIL: $label's banner does not run its two github-app examples in a shell that expands DEMO_GITHUB_APP and DEMO_GITHUB_READ" >&2
    exit 1
  fi
  printf 'PASS: %s prints a github-app example and a proxied one, each a child shell that expands the name the grant gave it\n' "$label"

  port="$bound"

  local pid="${instance_pid[$n]}"
  kill -TERM "$pid"
  wait "$pid" 2>/dev/null || true
  instance_pid[n]=""
  if kill -0 "${issuer##*:}" 2>/dev/null; then
    echo "FAIL: $label's cleanup left its issuer (pid ${issuer##*:}) running" >&2
    exit 1
  fi
  printf 'PASS: %s stops its issuer on exit\n' "$label"
  if kill -0 "$front_pid" 2>/dev/null; then
    echo "FAIL: $label's cleanup left the proxy's front (pid $front_pid) running" >&2
    exit 1
  fi
  printf 'PASS: %s stops the proxy'"'"'s front on exit\n' "$label"

  if ! grep -qxF "${drop_prefix} -c drop database if exists ${db};" "$docker_call_file"; then
    cat "$docker_call_file" >&2
    echo "FAIL: $label's cleanup never dropped its own database ($db)" >&2
    exit 1
  fi
  printf 'PASS: %s drops exactly the database it created on exit\n' "$label"
}

log1="${temporary_dir}/instance1.log"
log2="${temporary_dir}/instance2.log"
docker_calls1="${temporary_dir}/docker-calls-1"
docker_calls2="${temporary_dir}/docker-calls-2"
curl_calls1="${temporary_dir}/curl-calls-1"
curl_calls2="${temporary_dir}/curl-calls-2"

# Both instances start before either is waited on: this is the actual scenario
# BROKER_LISTEN_ADDR=127.0.0.1:0 exists to make safe, not two sequential runs that
# never actually contend for anything.
run_instance 1 "$log1" "$docker_calls1" "$curl_calls1"
run_instance 2 "$log2" "$docker_calls2" "$curl_calls2"

assert_instance_isolated 1 'instance 1' "$log1" "$docker_calls1" "$curl_calls1"
db1="$db"
port1="$port"

assert_instance_isolated 2 'instance 2' "$log2" "$docker_calls2" "$curl_calls2"
db2="$db"
port2="$port"

if [[ "$db1" == "$db2" ]]; then
  echo "FAIL: instance 1 ($db1) and instance 2 ($db2) minted the same database name" >&2
  exit 1
fi
printf 'PASS: two concurrent instances of dev-broker.sh mint two different databases (%s, %s), never the shared "dispatch" database\n' "$db1" "$db2"

if [[ "$port1" == "$port2" ]]; then
  echo "FAIL: instance 1 (port $port1) and instance 2 (port $port2) bound the same port" >&2
  exit 1
fi
printf 'PASS: two concurrent instances of dev-broker.sh bind two different ports (%s, %s)\n' "$port1" "$port2"

# --- A password DEV_BROKER_POSTGRES_URL carries, here in its query string, reaches psql in
# PGPASSWORD alone: each of the run's three psql calls (create, the current_database() check,
# drop) names the server without it, and the fake psql was given it in PGPASSWORD each time. No
# argument of any call the run made, docker, psql or curl, holds it in either spelling, and no line
# the run printed does: the banner names the database without it. ---
password_url='postgres://tester@127.0.0.1:5999/postgres?sslmode=disable&password=not-a-secret%40canary'
run_instance 1 "$log1" "$docker_calls1" "$curl_calls1" "$password_url"
if ! wait_for_ready "$log1"; then
  cat "$log1" >&2
  echo "FAIL: a DEV_BROKER_POSTGRES_URL run with a password in its query string never reached ready" >&2
  exit 1
fi
kill -TERM "${instance_pid[1]}"
wait "${instance_pid[1]}" 2>/dev/null || true
instance_pid[1]=""
psql_calls="$(grep -c '^psql ' "$docker_calls1" || true)"
bare_calls="$(grep -c '^psql postgres://tester@127\.0\.0\.1:5999/[a-z0-9_]*?sslmode=disable -' "$docker_calls1" || true)"
password_calls="$(grep -cxF 'PGPASSWORD=not-a-secret@canary' "${docker_calls1}.passwords" || true)"
if [[ "$psql_calls" -ne 3 || "$bare_calls" -ne 3 || "$password_calls" -ne 3 ]] || grep -q 'not-a-secret' "$docker_calls1" "$curl_calls1"; then
  cat "$docker_calls1" "${docker_calls1}.passwords" "$curl_calls1" >&2
  echo "FAIL: a DEV_BROKER_POSTGRES_URL password must reach each psql call in PGPASSWORD alone, and no argument of any call (${psql_calls} psql calls, ${bare_calls} naming the server without it, ${password_calls} given it in PGPASSWORD; want 3 of each)" >&2
  exit 1
fi
printf 'PASS: a DEV_BROKER_POSTGRES_URL password reaches each of the run'"'"'s psql calls in PGPASSWORD alone, and no argument of any call\n'
if grep -q 'not-a-secret' "$log1"; then
  grep 'not-a-secret' "$log1" >&2
  echo "FAIL: a DEV_BROKER_POSTGRES_URL run printed the password its URL's query string carries" >&2
  exit 1
fi
printf 'PASS: a DEV_BROKER_POSTGRES_URL run prints no line holding the password its URL'"'"'s query string carries\n'

# --- The same two concurrent instances against a Postgres server named by
# DEV_BROKER_POSTGRES_URL, through the fake psql: each creates, points its broker at, checks and
# drops only its own database on that server, and every psql call names the server without the
# URL's password. ---
admin_url='postgres://tester:not-a-secret%40canary@127.0.0.1:5999/postgres?sslmode=disable'
admin_url_bare='postgres://tester@127.0.0.1:5999/postgres?sslmode=disable'
run_instance 1 "$log1" "$docker_calls1" "$curl_calls1" "$admin_url"
run_instance 2 "$log2" "$docker_calls2" "$curl_calls2" "$admin_url"
assert_instance_isolated 1 'URL instance 1' "$log1" "$docker_calls1" "$curl_calls1" "psql ${admin_url_bare} -v ON_ERROR_STOP=1 -q"
url_db1="$db"
assert_instance_isolated 2 'URL instance 2' "$log2" "$docker_calls2" "$curl_calls2" "psql ${admin_url_bare} -v ON_ERROR_STOP=1 -q"
if [[ "$url_db1" == "$db" ]]; then
  echo "FAIL: the two DEV_BROKER_POSTGRES_URL instances minted the same database name ($db)" >&2
  exit 1
fi
for log in "$log1" "$log2"; do
  if grep -q 'not-a-secret' "$log"; then
    echo "FAIL: a DEV_BROKER_POSTGRES_URL instance printed the URL's password" >&2
    exit 1
  fi
done
printf 'PASS: two concurrent DEV_BROKER_POSTGRES_URL instances use two databases of their own (%s, %s), and print no password\n' "$url_db1" "$db"

# --- A dbname= in DEV_BROKER_POSTGRES_URL's query string would point the broker at another
# database: the run refuses before the broker starts, and drops only the database it created. ---
run_instance 1 "$log1" "$docker_calls1" "$curl_calls1" "${admin_url}&dbname=victim"
wait_for_exit 1
refused_status="$exited_status"
refused_db="$(created_database "$docker_calls1")"
if [[ "$refused_status" -eq 0 ]] || ! grep -q "dev-broker: refused: the broker's database URL connects to 'victim'" "$log1" || grep -q 'starting broker' "$log1"; then
  cat "$log1" >&2
  echo "FAIL: a DEV_BROKER_POSTGRES_URL naming dbname=victim must be refused before the broker starts (exit $refused_status)" >&2
  exit 1
fi
if [[ -z "$refused_db" ]] || ! grep -qxF "psql ${admin_url_bare}&dbname=victim -v ON_ERROR_STOP=1 -q -c drop database if exists ${refused_db};" "$docker_calls1" || grep -q 'database if exists victim\|database victim' "$docker_calls1"; then
  cat "$docker_calls1" >&2
  echo "FAIL: the refused run must drop its own database ($refused_db) and never touch victim" >&2
  exit 1
fi
printf 'PASS: a DEV_BROKER_POSTGRES_URL whose dbname= names another database is refused, and only %s is dropped\n' "$refused_db"

# --- A run whose create database fails (the name taken by another run) drops nothing: the
# database it would drop is that other run's. ---
: >"$docker_calls1"
create_status=0
PATH="${fake_bin_dir}:${PATH}" DEV_BROKER_POSTGRES_URL="$admin_url" DEV_BROKER_TEST_CREATE_FAILS=1 TMPDIR="$temporary_dir" \
  DEV_BROKER_TEST_DOCKER_CALL_FILE="$docker_calls1" DEV_BROKER_TEST_CURL_CALL_FILE="$curl_calls1" \
  bash "$driver" >"$log1" 2>&1 || create_status=$?
if [[ "$create_status" -eq 0 ]] || ! grep -q 'create database dev_broker_' "$docker_calls1" || grep -q 'drop database' "$docker_calls1"; then
  cat "$log1" "$docker_calls1" >&2
  echo "FAIL: a run whose create database failed must exit nonzero and drop nothing (exit $create_status)" >&2
  exit 1
fi
printf 'PASS: a run whose create database fails drops no database\n'

# --- A run whose drop database fails (the server gone before its cleanup ran) says so, and
# never claims a drop that did not happen: the database is still there for someone to drop. ---
export DEV_BROKER_TEST_DROP_FAILS=1
run_instance 1 "$log1" "$docker_calls1" "$curl_calls1" "$admin_url"
unset DEV_BROKER_TEST_DROP_FAILS
if ! wait_for_ready "$log1"; then
  cat "$log1" >&2
  echo "FAIL: the run whose drop will fail never reached ready" >&2
  exit 1
fi
kill -TERM "${instance_pid[1]}"
wait "${instance_pid[1]}" 2>/dev/null || true
instance_pid[1]=""
drop_db=$(grep -o 'drop database if exists dev_broker_[a-z0-9]*' "$docker_calls1" | head -n 1 | awk '{print $NF}')
if [[ -z "$drop_db" ]] || grep -q "dropped this instance's database" "$log1" \
  || ! grep -qF "could not drop this instance's database ${drop_db}" "$log1"; then
  cat "$log1" "$docker_calls1" >&2
  echo "FAIL: a run whose drop database failed must say it could not drop ${drop_db:-its database}, and never that it dropped it" >&2
  exit 1
fi
printf 'PASS: a run whose drop database fails says so and names the database left behind\n'

# --- A dispatch-pg on another image than dev-postgres.sh's pinned one (an older script's
# postgres:16) is refused through dev-postgres.sh before this run creates a database or starts a
# broker, and the refusal prints the dump that moves its databases. ---
DEV_BROKER_TEST_PG_IMAGE_ID="sha256:eb4759788a2182f08257135e61a34f2cfc3c2914079f3465d64ee62350f4d081" \
  run_instance 1 "$log1" "$docker_calls1" "$curl_calls1"
wait_for_exit 1
stale_status="$exited_status"
if [[ "$stale_status" -eq 0 ]] || ! grep -qF "dev-postgres: dispatch-pg runs postgres:16" "$log1" \
  || ! grep -qF "dev-postgres:   docker exec dispatch-pg pg_dumpall -U postgres --clean --if-exists > " "$log1" \
  || grep -qF "no pgvector" "$log1" \
  || grep -q 'create database' "$docker_calls1" || grep -q 'starting broker' "$log1" \
  || grep -qE '^(start|run|stop|rm|rename) ' "$docker_calls1"; then
  cat "$log1" >&2
  echo "FAIL: a dispatch-pg on another image than the pinned one was not refused before anything was created" >&2
  exit 1
fi
printf 'PASS: a dispatch-pg on another image than the pinned one is refused through dev-postgres.sh, before any database or broker\n'

# --- DEV_BROKER_GCP_PREFIX and DEV_BROKER_GCP_KMS_KEY_NAME give the broker Google Cloud Secret
# Manager as its store under that prefix and key, with no AWS key and an empty fake secrets file,
# and the banner says so; either one alone is refused before anything is created. ---
gcp_prefix="projects/100000000001/secrets/agent-secrets-"
gcp_key="projects/acme-deployment/locations/global/keyRings/agent-secrets/cryptoKeys/agent-secrets"
DEV_BROKER_GCP_PREFIX="$gcp_prefix" DEV_BROKER_GCP_KMS_KEY_NAME="$gcp_key" \
  run_instance 1 "$log1" "$docker_calls1" "$curl_calls1"
if ! wait_for_ready "$log1"; then
  cat "$log1" >&2
  echo "FAIL: a DEV_BROKER_GCP_* run, whose profiles file gives no profile a proxy block, never reached ready" >&2
  exit 1
fi
gcp_workdir="$(grep -o 'dev-broker: workdir [^ ]*' "$log1" | head -n1 | cut -d' ' -f3 || true)"
if ! grep -qF "fake broker: store=gcp-secret-manager prefix=${gcp_prefix} key-arn= key-name=${gcp_key} fake-secrets=${gcp_workdir}/fake-secrets.json" "$log1" \
  || [[ "$(jq -c . "${gcp_workdir}/fake-secrets.json" 2>/dev/null || true)" != '{"secrets":[]}' ]] \
  || [[ "$(jq -c . "${gcp_workdir}/profiles.json" 2>/dev/null || true)" != '{"profiles":[]}' ]] \
  || ! grep -qF "  secret store:        gcp-secret-manager under ${gcp_prefix}, key ${gcp_key}" "$log1"; then
  cat "$log1" >&2
  echo "FAIL: a DEV_BROKER_GCP_* run does not start its broker on Secret Manager under that prefix and key, with an empty fake secrets file and a profiles file naming none, and say so" >&2
  exit 1
fi
printf 'PASS: DEV_BROKER_GCP_PREFIX and DEV_BROKER_GCP_KMS_KEY_NAME start the broker on Secret Manager under them, with an empty fake secrets file and a profiles file naming no profile\n'

# --- A profiles file that gives no profile a proxy block, as the one under DEV_BROKER_GCP_* is:
# the broker binds no proxy port for it and logs no line for one (the fake broker does as the real
# one does), so the stack starts without waiting for that line, points the proxy's front at
# nothing, and its banner says there is no proxy port. A stack that waited for the line would
# stop 15 seconds in, never ready. ---
if grep -qF "dev-broker: broker never logged its proxy's address" "$log1" \
  || grep -qF '"message":"airlock proxy listening"' "$log1" \
  || [[ -s "${gcp_workdir}/proxy-backends" ]] \
  || ! grep -qF "  proxy:               none  (no profile of the profiles file has a proxy block, so the broker binds no proxy port)" "$log1"; then
  cat "$log1" >&2
  echo "FAIL: a stack whose profiles file gives no profile a proxy block waited for a proxy port, pointed its front at one, or did not say it has none" >&2
  exit 1
fi
kill -TERM "${instance_pid[1]}"
wait "${instance_pid[1]}" 2>/dev/null || true
instance_pid[1]=""
printf 'PASS: a profiles file with no proxy block starts the stack without waiting for a proxy port, and the banner says it has none\n'

for half in DEV_BROKER_GCP_PREFIX DEV_BROKER_GCP_KMS_KEY_NAME; do
  : >"$docker_calls1"
  half_status=0
  env "${half}=set-alone" PATH="${fake_bin_dir}:${PATH}" \
    DEV_BROKER_TEST_DOCKER_CALL_FILE="$docker_calls1" DEV_BROKER_TEST_CURL_CALL_FILE="$curl_calls1" \
    bash "$driver" >"$log1" 2>&1 || half_status=$?
  if [[ "$half_status" -ne 2 ]] || ! grep -qF "dev-broker: DEV_BROKER_GCP_PREFIX and DEV_BROKER_GCP_KMS_KEY_NAME are set together or not at all" "$log1" || [[ -s "$docker_calls1" ]]; then
    cat "$log1" >&2
    echo "FAIL: ${half} alone exited ${half_status}; want 2 with the refusal and nothing created" >&2
    exit 1
  fi
done
printf 'PASS: either DEV_BROKER_GCP_* setting alone is refused with exit 2 before anything is created\n'

# --- DEV_BROKER_PROXIED_GITHUB_IP puts the github-tls stand on that address's port 443 and tells
# the broker's proxy where api.github.test is and that it may dial it there, and the banner says
# where it is; DEV_BROKER_GITHUB_PERMISSIONS reaches the stands as one --installation-permissions
# per owner it names, an owner followed by nothing among them. ---
DEV_BROKER_PROXIED_GITHUB_IP=127.0.0.9 DEV_BROKER_GITHUB_PERMISSIONS="acme-org=contents:read,administration:read ada=" \
  run_instance 1 "$log1" "$docker_calls1" "$curl_calls1"
if ! wait_for_ready "$log1"; then
  cat "$log1" >&2
  echo "FAIL: a DEV_BROKER_PROXIED_GITHUB_IP run never reached ready" >&2
  exit 1
fi
if ! grep -qE "^fake stands: dir=[^ ]* owners=\[acme-org=7 ada=8 Ada=9\] hosts=\[github-tls=api\.github\.test\] addresses=\[github-tls=127\.0\.0\.9:443\] held=\[acme-org=contents:read,administration:read ada=\]$" "$log1" \
  || ! grep -qF " resolve=[api.github.test=127.0.0.9] dial_allow=[api.github.test:443] " "$log1" \
  || ! grep -qF "(the github-tls stand, the same GitHub over TLS, which a command reaches through the proxy: at 127.0.0.9:443;" "$log1" \
  || ! grep -qF "  github holds:        acme-org=contents:read,administration:read ada=  (" "$log1"; then
  cat "$log1" >&2
  echo "FAIL: DEV_BROKER_PROXIED_GITHUB_IP and DEV_BROKER_GITHUB_PERMISSIONS do not reach the stands and the broker's proxy as they say, or the banner does not say so" >&2
  exit 1
fi
kill -TERM "${instance_pid[1]}"
wait "${instance_pid[1]}" 2>/dev/null || true
instance_pid[1]=""
printf 'PASS: DEV_BROKER_PROXIED_GITHUB_IP serves the proxied GitHub on that address'"'"'s port 443 and points the proxy at it, and DEV_BROKER_GITHUB_PERMISSIONS reaches the stands one owner at a time\n'

# --- DEV_BROKER_BROKER_BIN is the broker the stack starts, and no broker is built: every other
# binary is, and the stack comes up on that one as on its own. ---
override_broker="${temporary_dir}/override-broker"
cat >"$override_broker" <<'OVERRIDE'
#!/usr/bin/env bash
echo "override broker: started with profiles=${BROKER_PROFILES_FILE:-}" >&2
printf '{"time":"2026-01-01T00:00:00Z","severity":"INFO","message":"broker listening","addr":"127.0.0.1:%s"}\n' "$$" >&2
printf '{"time":"2026-01-01T00:00:00Z","severity":"INFO","message":"airlock proxy listening","addr":"127.0.0.1:1%s"}\n' "$$" >&2
printf '{"time":"2026-01-01T00:00:00Z","severity":"INFO","message":"broker metrics listening","addr":"127.0.0.1:2%s"}\n' "$$" >&2
exec sleep 999999
OVERRIDE
chmod +x "$override_broker"
DEV_BROKER_BROKER_BIN="$override_broker" run_instance 1 "$log1" "$docker_calls1" "$curl_calls1"
if ! wait_for_ready "$log1"; then
  cat "$log1" >&2
  echo "FAIL: a DEV_BROKER_BROKER_BIN run never reached ready" >&2
  exit 1
fi
override_workdir="$(grep -o 'dev-broker: workdir [^ ]*' "$log1" | head -n1 | cut -d' ' -f3 || true)"
built="$(sort "${docker_calls1}.go" | tr '\n' ' ')"
if ! grep -qF "override broker: started with profiles=${override_workdir}/profiles.json" "$log1" \
  || grep -q '^fake broker:' "$log1" \
  || [[ "$built" != "./cmd/cipher ./cmd/cipher-devrelay ./cmd/cipher-helper ./cmd/dev-oidc-issuer ./cmd/dev-proxy-stands " ]] \
  || ! grep -qF "dev-broker: built five into ${override_workdir}/bin, and the broker is a copy of DEV_BROKER_BROKER_BIN (${override_broker})" "$log1"; then
  cat "$log1" >&2
  echo "FAIL: a DEV_BROKER_BROKER_BIN run did not start that broker in place of a built one (built: ${built:-nothing})" >&2
  exit 1
fi
kill -TERM "${instance_pid[1]}"
wait "${instance_pid[1]}" 2>/dev/null || true
instance_pid[1]=""
printf 'PASS: DEV_BROKER_BROKER_BIN is the broker the stack starts, and the other five are built as before\n'

# --- A DEV_BROKER_PROXIED_GITHUB_IP outside loopback, and a DEV_BROKER_BROKER_BIN that is no
# executable file, are each refused with exit 2 before anything is created. ---
for refused in "DEV_BROKER_PROXIED_GITHUB_IP=10.0.0.9" "DEV_BROKER_BROKER_BIN=${temporary_dir}/no-such-broker"; do
  : >"$docker_calls1"
  refused_status=0
  env "$refused" PATH="${fake_bin_dir}:${PATH}" \
    DEV_BROKER_TEST_DOCKER_CALL_FILE="$docker_calls1" DEV_BROKER_TEST_CURL_CALL_FILE="$curl_calls1" \
    bash "$driver" >"$log1" 2>&1 || refused_status=$?
  if [[ "$refused_status" -ne 2 ]] || ! grep -qF "dev-broker: ${refused%%=*}" "$log1" || [[ -s "$docker_calls1" ]]; then
    cat "$log1" >&2
    echo "FAIL: ${refused} exited ${refused_status}; want 2 with a refusal naming ${refused%%=*} and nothing created" >&2
    exit 1
  fi
done
printf 'PASS: a DEV_BROKER_PROXIED_GITHUB_IP outside loopback and a DEV_BROKER_BROKER_BIN that is no executable file are refused with exit 2 before anything is created\n'
