#!/usr/bin/env bash
# c-private-bus (private projects, verification): no message of a private project reaches a
# connection whose token does not list it, on the bus itself, and each project's messages still
# reach its own sessions.
#
# It starts, from this checkout, every process the bus path runs, each the program production runs:
#   hub       testnats.HubImage, the hub's nats-server, in a container, running the start script
#             `envoy-infra render nats-start-script` renders for an authorization with the auth
#             callout and privateProjects: true (bus/callout-keys.ts makes the callout's keys);
#   auth      dev-oidc-issuer, the dev stack's stand-in for the cluster's token issuer, and the
#             broker (cmd/broker), which is the auth service on that hub and mints every session's
#             token with its project list from its exchange entries, as production's does;
#   dispatch  Dispatch on that hub, as its own service, on a database of its own, seeded through its
#             routes: carol, who owns PUB and PRIV, each with an issue, its spec and documents;
#   privacy   GET /v1/project-privacy, the answer a required-mode listener reads at each decision:
#             the broker's own route, or until it exists bus/project-privacy.ts serving it from
#             Dispatch's rows as that route does;
#   listener  the Envoy listener in required mode, as its own service on the hub, reading that
#             answer (ENVOY_PROJECT_PRIVACY_URL);
#   sessions  three sessions, each the Oh My Pi Envoy extension under the stand-in host
#             (packages/pi-envoy/scripts/role-takeover/omp-client.ts) with its own broker token: one
#             whose token lists PUB, one whose token lists PRIV, and one whose token lists none;
#   daemon    a Legion daemon's connection (bus/daemon-reader.ts) whose token lists PUB, which the
#             auth service holds to PUB by natsgrant.WithinProjects(natsgrant.Daemon(), ...).
# Then, in sections:
#   writes      carol comments on PUB-1 mentioning the PUB session, on a document of PUB and sends
#               the PUB session a direct message that names no issue; the same on PRIV for the PRIV
#               session; and waits until each session has the issue's event, the document's event,
#               the mention and the message. Every text carries a marker of its project. Nothing
#               publishes a GitHub event, which is a message of no project.
#   pub-reads   each message the PUB daemon's connection and the PUB session received, and each
#               stored message the PUB daemon may read: none is of PRIV (bus/decide.sh's
#               audit_reads), and a reader that read nothing is UNKNOWN, which fails.
#   both-ways   each message the PRIV session received: none is of PUB; and the session whose token
#               lists no project received nothing of PRIV.
#   privacy-read the listener made at least one privacy read during the run.
#
# Each section runs only when this checkout holds every input it needs (bus/decide.sh, bus_inputs):
# a section whose input is not merged prints `not run: <section> (<the missing input>)`, a section
# that stands in for an input prints `stand-in: <section>: ...` (bus_stand_ins), and either fails
# the run at its end, as does a judgement that failed or saw nothing. So the run exits 0 only when
# every section ran on its real inputs and every judgement passed.
#
# Usage, from anywhere in the checkout:
#
#   DATABASE_URL=<a loopback PostgreSQL this run may create databases on> \
#     bash packages/dispatch/e2e/acceptance/project-access-bus.sh [--control <name>]
#   bash packages/dispatch/e2e/acceptance/project-access-bus.sh --inputs
#
# --inputs starts nothing: it prints, for each section, `ready <section>`, its stand-in line or its
# not-run line, and exits 0 only when every section is ready on real inputs.
#
# The controls, each run on its own, each of which must fail the run:
#   --control daemon-every-project   the auth service grants the PUB daemon every project's names
#                                    (its BROKER_NATS_SERVICES entry is `*`, the whole
#                                    natsgrant.Daemon()): pub-reads fails, quoting a PRIV subject.
#   --control privacy-unread         the listener reads its privacy answer from a route that answers
#                                    {"projects":[]} and counts nothing, as a reader that sends no
#                                    request answers: privacy-read fails, naming the listener.
#
# It needs go, bun with this checkout's node_modules (`bun install`), docker with testnats.HubImage
# pulled, psql, curl, jq, python3 and openssl. It reads DATABASE_URL only for the server it names,
# makes a database for the broker and one for Dispatch there, and drops both at the end; it removes
# the hub's container. It inherits no DISPATCH_*, ENVOY_*, NATS_* or AGENT_SECRETS_* variable.
set -euo pipefail

root=$(cd "$(dirname "$0")/../../../.." && pwd)
bus=$root/packages/dispatch/e2e/acceptance/bus
drivers=$root/packages/pi-envoy/scripts/role-takeover
started=$SECONDS
# shellcheck disable=SC2034 # read by access-lib.sh's mask
run_epoch=$(date +%s)
check=setup

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
# What lib/rig.sh, which lib/two-builds.sh sources, reads of its caller.
evidence=''
# shellcheck disable=SC2034 # read by lib/rig.sh's until_true
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=../../../../scripts/e2e/lib/two-builds.sh
. "$root/scripts/e2e/lib/two-builds.sh"
# shellcheck source-path=SCRIPTDIR source=access-lib.sh
. "$(dirname "$0")/access-lib.sh"
# shellcheck source-path=SCRIPTDIR source=bus/decide.sh
. "$(dirname "$0")/bus/decide.sh"

mode=run
control=''
while [ $# -gt 0 ]; do
  case "$1" in
    --inputs) mode=inputs ;;
    --control)
      [ $# -ge 2 ] || fail "--control takes daemon-every-project or privacy-unread"
      control=$2
      shift
      ;;
    --control=*) control=${1#--control=} ;;
    *) fail "unknown argument '$1': this driver takes --inputs or --control <name>" ;;
  esac
  shift
done
case "$control" in
  '' | daemon-every-project | privacy-unread) ;;
  *) fail "--control is '$control'; the controls are daemon-every-project and privacy-unread" ;;
esac

# Which inputs this checkout holds, read once: every section's decision below answers from it.
bus_read_inputs

# --inputs: the census of this checkout's inputs, and nothing started.
if [ "$mode" = inputs ]; then
  work=$(mktemp -d "${TMPDIR:-/tmp}/project-access-bus-inputs.XXXXXX")
  bus_files_reset
  for row in "${bus_sections[@]}"; do
    section=${row%%|*}
    if section_gate "$section" && ! stand_in "$section"; then
      echo "ready $section"
    fi
  done
  status=0
  [ ! -s "$work/unrun" ] && [ ! -s "$work/stood-in" ] || status=1
  rm -rf -- "${work:?}"
  exit "$status"
fi

# two_builds_setup reads DATABASE_URL, drops every DISPATCH_*, ENVOY_* and NATS_* setting the shell
# inherited and its PGHOSTADDR, PGSERVICE and PGSERVICEFILE, and arms two-builds' teardown, which
# bus_cleanup below runs last.
two_builds_setup 'project access bus e2e'
for variable in $(compgen -e); do
  case $variable in AGENT_SECRETS_*) unset "$variable" ;; esac
done
issuer_pid='' broker_pid='' dispatch_pid='' dispatch_keeper_pid='' listener_pid='' listener_keeper_pid=''
privacy_pid='' uncounted_pid='' daemon_reader_pid=''
hub_container=''
# Assigned through printf -v by keep_port, own_database and await_port_file below.
dispatch_port='' listener_port='' privacy_port='' uncounted_port=''
broker_database_url='' dispatch_database_url=''
declare -A session_pid=() session_fd=()
bus_cleanup() {
  local fd
  set +e
  for fd in "${session_fd[@]}"; do
    exec {fd}>&-
  done
  stop_all "${session_pid[@]}" "$daemon_reader_pid" "$listener_pid" "$listener_keeper_pid" "$privacy_pid" \
    "$uncounted_pid" "$dispatch_pid" "$dispatch_keeper_pid" "$broker_pid" "$issuer_pid"
  if [ -n "$hub_container" ]; then
    docker logs "$hub_container" >"$work/logs/hub.log" 2>&1
    docker rm -f "$hub_container" >/dev/null 2>&1 || echo "could not remove the container $hub_container; remove it by hand" >&2
  fi
  drop_own_databases
  cleanup "$1"
}
trap 'bus_cleanup "$?"' EXIT
trap 'bus_cleanup 130' INT
trap 'bus_cleanup 143' TERM

begin prerequisites
[ -n "$database_url" ] ||
  fail "DATABASE_URL is required: it names a loopback PostgreSQL server this run makes its databases on"
for tool in go bun curl psql jq python3 openssl docker; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
require_loopback_dsn db_host DATABASE_URL "$database_url" "this run creates and drops databases there" || fail "$db_host"
[ -f "$root/node_modules/nats/package.json" ] || fail "$root/node_modules has no nats package: run bun install"
docker version --format '{{.Server.Version}}' >/dev/null 2>&1 || fail "docker answers no server"
note "database host $db_host (URL not printed)"
pass

begin build
# A short directory: the port keepers bind unix sockets in it, and a socket path past 107 bytes is
# refused.
work=$(mktemp -d "${TMPDIR:-/tmp}/pab.XXXXXX")
evidence=$work
mkdir -p "$work/logs" "$work/bin" "$work/tokens" "$work/web-dist" "$work/records"
bus_files_reset
note "work directory $work"
go -C "$root/packages/envoy" build -o "$work/bin/" ./cmd/dispatch ./cmd/listener ./cmd/broker ./cmd/dev-oidc-issuer \
  ./cmd/natsprobe || fail "this checkout's dispatch, listener, broker, dev-oidc-issuer or natsprobe did not build"
go -C "$root/packages/envoy/infra" build -o "$work/bin/" ./cmd/envoy-infra || fail "this checkout's envoy-infra did not build"
# shellcheck source-path=SCRIPTDIR source=../../../../scripts/e2e/lib/nats-image.sh
. "$root/scripts/e2e/lib/nats-image.sh"
run=$(openssl rand -hex 4)
two_builds_identity
pass

begin hub
mkdir -m 700 "$work/callout"
publics=$(bun "$bus/callout-keys.ts" "$work/callout") || fail "bus/callout-keys.ts made no keys"
# The auth service's own user is the hub's only user, with the grant the render holds it to: it
# subscribes the authorization subject and publishes its answers alone.
jq -n --argjson keys "$publics" '{
  users: [{name: "auth-service", nkey: $keys.service, hasSeed: true,
    publish: {allow: ["$SYS._INBOX.>"], deny: []}, subscribe: {allow: ["$SYS.REQ.USER.AUTH"], deny: []}}],
  noAuthUser: "",
  callout: {issuer: $keys.issuer, xkey: $keys.xkey, serviceUser: "auth-service"},
  privateProjects: true
}' >"$work/hub-authorization.json"
"$work/bin/envoy-infra" render nats-start-script --authorization-file "$work/hub-authorization.json" \
  --server-arg -js --config-path /tmp/nats.conf >"$work/hub-start.sh" 2>"$work/logs/render.log" ||
  fail "envoy-infra render nats-start-script refused the authorization: $(<"$work/logs/render.log")"
grep -q 'auth_callout' "$work/hub-start.sh" || fail "the rendered start script holds no auth_callout block"
grep -qF "$(jq -r .issuer <<<"$publics")" "$work/hub-start.sh" || fail "the rendered start script names another issuer"
hub_container=project-access-bus-$run
# The limits every live proof's NATS takes on a shared box (scripts/e2e/README.md).
docker run -d --name "$hub_container" --label owner=project-access-bus --cpus 1 --memory 512m -p 127.0.0.1::4222 \
  --entrypoint /bin/sh "$nats_image" -c "$(<"$work/hub-start.sh")" >"$work/logs/hub-container" 2>&1 ||
  fail "the hub's container did not start: $(<"$work/logs/hub-container")"
hub_ready() {
  docker logs "$hub_container" >"$work/logs/hub.log" 2>&1 && grep -q 'Server is ready' "$work/logs/hub.log"
}
until_true 60 "the hub to be ready" hub_ready
hub_address=$(docker port "$hub_container" 4222/tcp) || fail "docker names no host port for the hub"
hub_address=${hub_address%%$'\n'*}
hub_url=nats://$hub_address
note "the hub, $nats_image, serves $hub_url behind the auth callout"
pass

if section_gate hub-tag; then
  begin hub-tag
  # The tag is read from its one declaration, natsgrant.PrivateProjectsServerTag, never written here.
  tag=$(sed -n 's/^[[:space:]]*PrivateProjectsServerTag[[:space:]]*=[[:space:]]*"\(.*\)"$/\1/p' \
    "$root"/packages/envoy/natsgrant/*.go)
  [ -n "$tag" ] || fail "natsgrant declares no PrivateProjectsServerTag"
  grep -E 'server_tags' "$work/hub-start.sh" | grep -qF "\"$tag\"" ||
    fail "the start script rendered with privateProjects carries no server_tags naming $tag"
  note "the hub carries the server tag $tag"
  pass
fi

begin auth
ns=system:serviceaccount:project-access-bus
listener_account=$ns:listener dispatch_account=$ns:dispatch broker_account=$ns:broker
registrar_account=$ns:registrar daemon_account=$ns:pub-daemon
declare -A session_account=([pub]=$ns:pub-session [priv]=$ns:priv-session [open]=$ns:open-session)
declare -A session_name=([pub]=pab-pub-$run [priv]=pab-priv-$run [open]=pab-open-$run)
broker_audience=legion-broker-pod exchange_audience=legion-broker-exchange
nats_audience=envoy-nats dispatch_audience=dispatch-access
tokens=$work/tokens
issuer_args=(
  --token "$nats_audience=$listener_account=$tokens/nats-listener"
  --token "$nats_audience=$dispatch_account=$tokens/nats-dispatch"
  --token "$nats_audience=$daemon_account=$tokens/nats-daemon"
  --token "$broker_audience=$listener_account=$tokens/pod-listener"
  --token "$broker_audience=$dispatch_account=$tokens/pod-dispatch"
  --token "$broker_audience=$registrar_account=$tokens/pod-registrar"
  --token "$dispatch_audience=$broker_account=$tokens/access-broker"
)
for label in pub priv open; do
  issuer_args+=(--token "$broker_audience=${session_account[$label]}=$tokens/pod-$label"
    --token "$exchange_audience=${session_account[$label]}=$tokens/exchange-$label")
done
# The broker's own approval page signs a person in at the issuer's code flow, under a client secret
# no command line carries; nothing here signs in there, but the broker needs the page to start.
(umask 077 && openssl rand -hex 32 >"$work/signin-secret")
issuer_args+=(--code-flow "project-access-bus-broker=$work/signin-secret")
start_process issuer env -i PATH="$PATH" "$work/bin/dev-oidc-issuer" "${issuer_args[@]}"
# Each reader prints what it read and fails while there is nothing, so await_start polls it.
issuer_url_in() {
  local url
  url=$(grep -o 'issuer listening url=[^ ]*' "$evidence/logs/issuer.log" | head -n1 | cut -d= -f2-)
  [ -n "$url" ] && printf '%s\n' "$url"
}
await_start issuer "$issuer_pid" 60 "dev-oidc-issuer to report its URL" issuer_url_in
issuer_url=$(issuer_url_in)
# Dispatch's port and the listener's are held from here, so the broker can name Dispatch's before
# it starts and Dispatch the listener's.
keep_port dispatch_keeper dispatch_port
keep_port listener_keeper listener_port
dispatch_url=http://127.0.0.1:$dispatch_port
listener_url=http://127.0.0.1:$listener_port
listener_id=project-access-bus-$run
own_database broker_database_url broker
cat >"$work/fake-secrets.json" <<'EOF'
{"secrets": []}
EOF
chmod 600 "$work/fake-secrets.json"
# The daemon's entry is held to its exchange projects (`-`), and to every project (`*`) under the
# check's negative control. It is listed only once the auth service can hold it: before then the
# daemon section does not run.
nats_services="listener/$listener_id/*=$listener_account dispatch/project-access-bus-$run/*=$dispatch_account"
daemon_scope=-
[ "$control" != daemon-every-project ] || daemon_scope='*'
if [ -z "$(section_missing daemon)" ]; then
  nats_services+=" daemon/-/$daemon_scope=$daemon_account"
fi
exchange_accounts="${session_account[pub]}=PUB=$carol ${session_account[priv]}=PRIV=$carol"
exchange_accounts+=" ${session_account[open]}==$carol $daemon_account=PUB=$carol"
pod_registrars=''
for label in pub priv open; do
  pod_registrars+="${pod_registrars:+ }$registrar_account=${session_account[$label]}"
done
start_process broker env -i PATH="$PATH" \
  BROKER_DATABASE_URL="$broker_database_url" \
  BROKER_LISTEN_ADDR=127.0.0.1:0 \
  BROKER_PUBLIC_URL=http://127.0.0.1:0 \
  BROKER_METRICS_LISTEN_ADDR=127.0.0.1:0 \
  BROKER_DRAIN_SECONDS=0 \
  BROKER_MACHINE_LOGIN_URL="$dispatch_url/credentials/machine" \
  BROKER_SECRETS_PREFIX=project-access-bus/ \
  BROKER_SECRETS_KMS_KEY_ARN=arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab \
  BROKER_FAKE_SECRETS_FILE="$work/fake-secrets.json" \
  BROKER_K8S_OIDC_ISSUER="$issuer_url" \
  BROKER_K8S_OIDC_AUDIENCE="$broker_audience" \
  BROKER_SIGNIN_ISSUER="$issuer_url" \
  BROKER_SIGNIN_CLIENT_ID=project-access-bus-broker \
  BROKER_SIGNIN_CLIENT_SECRET_FILE="$work/signin-secret" \
  BROKER_SIGNIN_DOMAIN=example.com \
  BROKER_API_ACCOUNTS="dispatch=$dispatch_account listener=$listener_account" \
  BROKER_EXCHANGE_K8S_AUDIENCE="$exchange_audience" \
  BROKER_EXCHANGE_ACCOUNTS="$exchange_accounts" \
  BROKER_SERVICE_LOGINS="legion-daemon=$registrar_account" \
  BROKER_POD_REGISTRARS="$pod_registrars" \
  BROKER_PROJECT_ACCESS_URL="$dispatch_url" \
  BROKER_PROJECT_ACCESS_TOKEN_FILE="$tokens/access-broker" \
  BROKER_NATS_URLS="$hub_url" \
  BROKER_NATS_SEED_FILE="$work/callout/service.seed" \
  BROKER_NATS_ISSUER_SEED_FILE="$work/callout/issuer.seed" \
  BROKER_NATS_XKEY_SEED_FILE="$work/callout/xkey.seed" \
  BROKER_NATS_SERVICES="$nats_services" \
  BROKER_NATS_K8S_AUDIENCE="$nats_audience" \
  "$work/bin/broker"
broker_addr_in() {
  local addr
  addr=$(jq -Rnr '[inputs | fromjson? | select(.message == "broker listening") | .addr][0] // empty' "$evidence/logs/broker.log")
  [ -n "$addr" ] && printf '%s\n' "$addr"
}
await_start broker "$broker_pid" 60 "the broker to report its address" broker_addr_in
broker_url=http://$(broker_addr_in)
await_start broker "$broker_pid" 60 "the broker's NATS auth service to answer the hub" \
  grep -q 'the NATS auth service is answering' "$evidence/logs/broker.log"
# The hub sends every client to the auth service: one with no credential is refused in the hub's
# own words, and Dispatch's service token is admitted as Dispatch.
probe_out=$(env -i PATH="$PATH" HOME="$work" "$work/bin/natsprobe" --servers "$hub_url" --no-credential \
  --subscribe _INBOX.project-access-bus --wait 1s 2>&1) && probe_status=0 || probe_status=$?
if [ "$probe_status" != 4 ] || ! grep -q '^not admitted: Authorization Violation' <<<"$probe_out"; then
  fail "a connection with no credential was not refused by the hub's auth service: natsprobe exit $probe_status: $probe_out"
fi
probe_out=$(env -i PATH="$PATH" HOME="$work" "$work/bin/natsprobe" --servers "$hub_url" \
  --service-token-file "$tokens/nats-dispatch" --subscribe _INBOX.project-access-bus --wait 1s 2>&1) &&
  probe_status=0 || probe_status=$?
if [ "$probe_status" != 0 ] || ! grep -q '^served _INBOX.project-access-bus' <<<"$probe_out"; then
  fail "the auth service did not admit Dispatch's service token: natsprobe exit $probe_status: $probe_out"
fi
note "the broker $broker_url answers the hub's auth callout: no credential is refused, Dispatch's token admitted"
pass

begin dispatch
own_database dispatch_database_url dispatch
# The deployment's own settings for what this check reads: the bus on, through the hub as
# Dispatch's own service; PRIV provisioned as private; the broker's account the one reader of the
# private-projects answer (DISPATCH_ACCESS_ACCOUNTS), by the issuer's tokens for Dispatch's audience;
# and the broker that checks every session's token.
private_hub_env=()
if [ -z "$(section_missing private)" ] && [ -z "$(stand_in_missing private)" ]; then
  # The admission proof's settings, plan A's t-a-20 and plan E's t-e-65: the one hub, Dispatch's
  # token for the admission audience, and the listener whose readiness the proof reads.
  admission_audience=$(sed -n 's/^[[:space:]]*HubAdmissionAudience[[:space:]]*=[[:space:]]*"\(.*\)"$/\1/p' \
    "$root"/packages/envoy/natsgrant/*.go)
  private_hub_env=(DISPATCH_PRIVATE_HUBS="$hub_url" DISPATCH_HUB_ADMISSION_TOKEN_FILE="$tokens/admission-dispatch"
    DISPATCH_PRIVATE_LISTENERS="$listener_url")
  curl -fsS --max-time 5 -X POST -H 'Content-Type: application/json' "$issuer_url/_dev/mint" \
    -d "$(jq -cn --arg s "$dispatch_account" --arg a "$admission_audience" '{subject: $s, audience: $a}')" |
    jq -r .token >"$tokens/admission-dispatch" || fail "the issuer minted no admission token for Dispatch"
fi
start_kept dispatch dispatch_keeper DISPATCH_LISTEN_FD -- env -i \
  PATH="$PATH" TMPDIR="$work" HOME=/nonexistent XDG_CACHE_HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent \
  XDG_DATA_HOME=/nonexistent \
  DATABASE_URL="$dispatch_database_url" \
  DISPATCH_AGENT_TOKEN="$agent_token" \
  DISPATCH_DEV_SIGNIN=1 \
  DISPATCH_DEV_SIGNIN_SHARED_KEY="$signin_key" \
  DISPATCH_IDENTITY=cookie \
  DISPATCH_INSECURE_COOKIE=1 \
  DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_LISTEN_FD=3 \
  DISPATCH_SERVER_URL="$dispatch_url" \
  DISPATCH_WEB_DIST="$work/web-dist" \
  ENVOY_URL="$listener_url" \
  NATS_URLS="$hub_url" \
  NATS_SERVICE_TOKEN_FILE="$tokens/nats-dispatch" \
  DISPATCH_PRIVATE_PROJECTS=PRIV \
  DISPATCH_OIDC_ISSUER="$issuer_url" \
  DISPATCH_OIDC_AUDIENCE="$dispatch_audience" \
  DISPATCH_OIDC_ACCOUNTS="$broker_account" \
  DISPATCH_ACCESS_ACCOUNTS="$broker_account" \
  DISPATCH_AGENT_SECRETS_URL="$broker_url" \
  DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$tokens/pod-dispatch" \
  "${private_hub_env[@]}" \
  "$work/bin/dispatch"
await_start dispatch "$dispatch_pid" 120 "Dispatch to answer /healthz" curl -fsS --max-time 2 "$dispatch_url/healthz"
build_url[main]=$dispatch_url
seed_people main
make_project main PUB
make_project main PRIV
seed_objects main PUB carol
seed_objects main PRIV carol
note "Dispatch $dispatch_url holds PUB and PRIV, each with its issue, spec, comment, ask, message and documents"
pass

if section_gate tokens; then
  begin tokens
  # Each session's pod is registered for its session's name, as a deployment's daemon registers a
  # pod it starts, and the broker mints each a token listing its exchange entry's project: the
  # stand-in broker of the check is the dev stack's, the code production's broker runs.
  printf 'Authorization: Bearer %s\n' "$(<"$tokens/pod-registrar")" >"$work/registrar-header"
  printf 'Authorization: Bearer %s\n' "$(<"$tokens/pod-listener")" >"$work/listener-header"
  chmod 600 "$work/registrar-header" "$work/listener-header"
  declare -A want_projects=([pub]='["PUB"]' [priv]='["PRIV"]' [open]='[]')
  for label in pub priv open; do
    jq -cn --rawfile p "$tokens/pod-$label" --arg s "${session_name[$label]}" \
      '{pod_token: ($p | sub("\n+$"; "")), session: $s}' >"$work/register-$label.json"
    status=$(curl -sS --max-time 20 -o "$work/body" -w '%{http_code}' -X POST "$broker_url/v1/oauth/exchange/pod-sessions" \
      -H "@$work/registrar-header" -H 'Content-Type: application/json' --data-binary "@$work/register-$label.json") ||
      status=unreachable
    case "$status" in 200 | 201 | 204) ;; *) fail "registering the $label pod for ${session_name[$label]} answered $status: $(head -c 300 "$work/body")" ;; esac
    mkdir -p "$work/home-$label"
    answer=$(env -i PATH="$PATH" HOME="$work/home-$label" TMPDIR="$work" \
      AGENT_SECRETS_URL="$broker_url" AGENT_SECRETS_MACHINE_IDENTITY=kubernetes \
      AGENT_SECRETS_MACHINE_TOKEN_FILE="$tokens/exchange-$label" BUS_INTROSPECT_TOKEN_FILE="$tokens/pod-listener" \
      bun "$bus/token-projects.ts" "${session_name[$label]}" 2>&1) ||
      fail "the broker minted no token for ${session_name[$label]}: $answer"
    got=$(jq -c .projects <<<"$answer")
    [ "$got" = "${want_projects[$label]}" ] ||
      fail "${session_name[$label]}'s token lists $got, want ${want_projects[$label]}"
    note "${session_name[$label]}'s token lists $got"
  done
  pass
fi

if section_gate private; then
  begin private
  # What an operator runs against the database before a project can be made private: the fill of
  # every event's session project and the scrub of the stored access events (cmd/dispatch README).
  for command in backfill-session-projects scrub-access-events; do
    env -i PATH="$PATH" HOME=/nonexistent TMPDIR="$work" DATABASE_URL="$dispatch_database_url" \
      "$work/bin/dispatch" "$command" >"$work/$command.out" 2>&1 ||
      fail "envoy-dispatch $command failed: $(tail -n 5 "$work/$command.out")"
  done
  send main carol PATCH /api/v1/projects/PRIV/access '{"visibility":"private"}'
  if stand_in private; then
    # The route reaches the bus only once PRIV is provisioned and the store complete, so its 409
    # PRIVATE_PROJECTS_UNAVAILABLE says every other precondition holds; any other answer fails.
    [ "$status" = 409 ] && [ "$(jq -r '.code // empty' "$work/body" 2>/dev/null)" = PRIVATE_PROJECTS_UNAVAILABLE ] ||
      fail "the visibility route answered HTTP $status $(head -c 300 "$work/body"), not the 409 PRIVATE_PROJECTS_UNAVAILABLE the stand-in stands in for"
    psql "$dispatch_database_url" -X -q -v ON_ERROR_STOP=1 -c "update projects set visibility = 'private' where key = 'PRIV'" >/dev/null ||
      fail "the stand-in could not make PRIV private"
  else
    expect_status 200 "carol makes PRIV private through the visibility route, which proves the hub admits Dispatch"
  fi
  visibility=$(psql "$dispatch_database_url" -X -tAc "select visibility from projects where key = 'PRIV'")
  [ "$visibility" = private ] || fail "PRIV's visibility is '$visibility' after the switch"
  note "PRIV is private"
  pass
fi

if section_gate privacy; then
  begin privacy
  privacy_mode=(--forward "$broker_url")
  if stand_in privacy; then
    privacy_mode=(--dispatch "$dispatch_url" --dispatch-token-file "$tokens/access-broker")
  fi
  start_process privacy env -i PATH="$PATH" HOME="$work" bun "$bus/project-privacy.ts" \
    --port-file "$work/privacy.port" --log "$work/privacy-reads.jsonl" --caller "listener=$tokens/pod-listener" \
    "${privacy_mode[@]}"
  await_port_file privacy "$work/privacy.port" privacy_port
  privacy_url=http://127.0.0.1:$privacy_port
  # Read as the listener: PRIV, and nothing else, is private. Read as Dispatch's app account, which
  # no route of a listener's names: refused.
  status=$(curl -sS --max-time 10 -o "$work/body" -w '%{http_code}' -H "@$work/listener-header" "$privacy_url/v1/project-privacy") ||
    status=unreachable
  [ "$status" = 200 ] && [ "$(jq -c '[.projects[].project]' "$work/body" 2>/dev/null)" = '["PRIV"]' ] ||
    fail "the privacy route answered the listener HTTP $status $(head -c 300 "$work/body"), want 200 naming PRIV alone"
  printf 'Authorization: Bearer %s\n' "$(<"$tokens/pod-dispatch")" >"$work/dispatch-header"
  chmod 600 "$work/dispatch-header"
  status=$(curl -sS --max-time 10 -o "$work/body" -w '%{http_code}' -H "@$work/dispatch-header" "$privacy_url/v1/project-privacy") ||
    status=unreachable
  [ "$status" = 403 ] || fail "the privacy route answered Dispatch's app account HTTP $status, want 403 not_listener"
  # The privacy reads counted from here are the listener's own.
  privacy_baseline=$(wc -l <"$work/privacy-reads.jsonl")
  listener_privacy_url=$privacy_url
  if [ "$control" = privacy-unread ]; then
    start_process uncounted env -i PATH="$PATH" HOME="$work" bun "$bus/project-privacy.ts" \
      --port-file "$work/uncounted.port" --log "$work/uncounted-reads.jsonl" --uncounted
    await_port_file uncounted "$work/uncounted.port" uncounted_port
    listener_privacy_url=http://127.0.0.1:$uncounted_port
    note "control privacy-unread: the listener reads $listener_privacy_url, which counts nothing"
  fi
  note "GET /v1/project-privacy answers the listener PRIV and refuses Dispatch's app account"
  pass
fi

if section_gate listener; then
  begin listener
  start_kept listener listener_keeper ENVOY_LISTEN_FD -- env -i \
    PATH="$PATH" \
    ENVOY_LISTEN_FD=3 \
    ENVOY_LISTEN_HOST=127.0.0.1 \
    ENVOY_MACHINE_ID="$listener_id" \
    ENVOY_HOST_BRIDGE=127.0.0.1 \
    ENVOY_PUBLIC_URL='http://127.0.0.1:{port}' \
    ENVOY_AGENT_SECRETS_URL="$broker_url" \
    ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE="$tokens/pod-listener" \
    NATS_URLS="$hub_url" \
    NATS_SERVICE_TOKEN_FILE="$tokens/nats-listener" \
    ENVOY_PROJECT_PRIVACY_REQUIRED=1 \
    ENVOY_PROJECT_PRIVACY_URL="$listener_privacy_url" \
    "$work/bin/listener"
  await_start listener "$listener_pid" 120 "the listener to answer /healthz" curl -fsS --max-time 2 "$listener_url/healthz"
  note "the listener $listener_url runs in required mode, reading $listener_privacy_url"
  pass
fi

# session_start LABEL: the session LABEL, the Oh My Pi Envoy extension under the stand-in host, its
# NATS connection presenting its own broker token (NATS_AUTH=broker) traded from its pod's identity.
session_start() {
  local label=$1 home=$work/session-$1 fd
  mkdir -p "$home/cwd"
  jq -n --arg url "$listener_url" --arg nats "$hub_url" \
    '{envoy: {url: $url, natsUrls: [$nats]}, dispatch: {enabled: false}, askgate: {enabled: false}}' >"$home/envoy.json"
  mkfifo "$work/session-$label.stdin"
  exec {fd}<>"$work/session-$label.stdin"
  session_fd[$label]=$fd
  : >"$work/session-$label.out"
  (cd -- "$root/packages/pi-envoy" &&
    exec env -i PATH="$PATH" HOME="$home" TMPDIR="$work" ENVOY_CONFIG_FILE="$home/envoy.json" \
      ENVOY_HEARTBEAT_MS=5000 ROLE_TAKEOVER_CWD="$home/cwd" ROLE_TAKEOVER_CLIENT_LOG="$work/logs/session-$label-notices.log" \
      NATS_AUTH=broker AGENT_SECRETS_URL="$broker_url" AGENT_SECRETS_MACHINE_IDENTITY=kubernetes \
      AGENT_SECRETS_MACHINE_TOKEN_FILE="$tokens/exchange-$label" \
      bun --preload "$drivers/host-stub.ts" "$drivers/omp-client.ts") \
    <"$work/session-$label.stdin" >"$work/session-$label.out" 2>"$work/logs/session-$label.err" &
  session_pid[$label]=$!
  until_true 90 "the $label session's client to start" grep -q '"ready":true' "$work/session-$label.out"
}

# session_send LABEL JSON: one command to the session LABEL's client, its answer left in `reply`.
reply='' command_id=0
session_send() {
  local label=$1 id
  command_id=$((command_id + 1))
  id=c$command_id
  jq -c --arg id "$id" '. + {id: $id}' <<<"$2" >&"${session_fd[$label]}"
  until_true 60 "the $label session to answer $2" grep -q "\"id\":\"$id\"" "$work/session-$label.out"
  reply=$(grep -F "\"id\":\"$id\"" "$work/session-$label.out" | tail -n 1)
}

# session_record LABEL: every delivery the session LABEL's host was handed, one record each in
# $work/records/session-LABEL.jsonl, its subject the delivery's topic where the card names one.
session_record() {
  session_send "$1" '{"op":"deliveries"}'
  jq -c '.deliveries[]?, (.turns[]? | {content: .}) | {subject: (.details.topic // .details.subject // ""), data: .}' \
    <<<"$reply" >"$work/records/session-$1.jsonl"
}

if section_gate sessions; then
  begin sessions
  for label in pub priv open; do
    session_start "$label"
    session_send "$label" "$(jq -cn --arg s "${session_name[$label]}" '{op: "start", session: $s}')"
    # Each follows both issues: the listener takes the topic of a project its token lists and
    # refuses the other's 403 project_not_listed, which is kept in its answer, not judged here.
    session_send "$label" "$(jq -cn '{op: "tool", name: "envoy_subscribe", args: {topics: ["notifications.dispatch.issue.PUB.PUB-1.>", "notifications.dispatch.issue.PRIV.PRIV-1.>"]}}')"
    printf '%s\n' "$reply" >"$work/session-$label-subscribe.json"
  done
  note "three sessions follow PUB-1 and PRIV-1: ${session_name[pub]}, ${session_name[priv]} and ${session_name[open]}"
  pass
fi

if section_gate daemon; then
  begin daemon
  start_process daemon_reader env -i PATH="$PATH" HOME="$work" bun "$bus/daemon-reader.ts" \
    --servers "$hub_url" --token-file "$tokens/nats-daemon" --projects PUB,PRIV \
    --record "$work/records/daemon-pub.jsonl" --refusals "$work/records/daemon-pub-refusals.txt" \
    --ready-file "$work/daemon.ready" --stop-file "$work/daemon.stop"
  await_start daemon_reader "$daemon_reader_pid" 60 "the PUB daemon's connection to be admitted" test -e "$work/daemon.ready"
  note "the PUB daemon's connection is admitted, its entry's scope '$daemon_scope' ('-' its exchange projects, '*' every project)"
  pass
fi

# session_holds LABEL TEXT: the session LABEL's host has been handed a delivery holding TEXT.
session_holds() {
  session_record "$1"
  grep -qF -- "$2" "$work/records/session-$1.jsonl"
}

# exited PID: the process PID is gone.
exited() { ! kill -0 "$1" 2>/dev/null; }

if section_gate writes; then
  begin writes
  for project in PUB PRIV; do
    label=${project,,}
    session=${session_name[$label]}
    marker="$project-marker-$(openssl rand -hex 6)"
    printf '%s\n' "$marker" "$project-1" "$session" "$(recalled main "$project notes")" >"$work/markers-$project"
    send main carol POST "/api/v1/issues/$project-1/comments" "$(jq -cn --arg b "A remark for @$session ($marker issue)." \
      --arg t "session:$session" '{body: $b, mentions: [{target: $t}]}')"
    expect_status 201 "carol comments on $project-1 mentioning $session"
    send main carol POST "$(resolve main "/api/v1/artifacts/{$project notes}/comments")" \
      "$(jq -cn --arg b "A remark on the notes ($marker document)." '{body: $b}')"
    expect_status 201 "carol comments on a document of $project"
    send main carol POST "/api/v1/agents/$session/messages" \
      "$(jq -cn --arg b "A direct word, on no issue ($marker message)." '{body: $b, delivery: "btw"}')"
    expect_status 201 "carol sends $session a direct message that names no issue"
    # The session has each of the four once its host was handed a delivery holding each text.
    for part in issue document message; do
      until_true 120 "$session to receive the $project $part write" session_holds "$label" "$marker $part"
    done
  done
  # The session whose token lists no project is sent a direct message of no project (the message
  # takes no project when its target lists none, #t-c-8's send rule), which every token sees, so
  # its record holds a message to judge and an empty read cannot pass for a clean one.
  marker="open-marker-$(openssl rand -hex 6)"
  send main carol POST "/api/v1/agents/${session_name[open]}/messages" \
    "$(jq -cn --arg b "A direct word to a session of no project ($marker message)." '{body: $b, delivery: "btw"}')"
  expect_status 201 "carol sends ${session_name[open]} a direct message"
  until_true 120 "${session_name[open]} to receive its direct message" session_holds open "$marker message"
  note "each session received its project's issue event, document event, mention and direct message, and the session of no project its direct message"
  pass
fi

if section_gate pub-reads; then
  begin pub-reads
  : >"$work/daemon.stop"
  until_true 120 "the PUB daemon's reader to read every stored message it may" exited "$daemon_reader_pid"
  wait "$daemon_reader_pid" || bus_fail "UNKNOWN the PUB daemon's reader ended with an error; see $work/logs/daemon_reader.log"
  daemon_reader_pid=''
  session_record pub
  for reader in daemon-pub session-pub; do
    line=$(audit_reads "the PUB ${reader%%-*}'s connection" "$work/records/$reader.jsonl" PRIV "$work/markers-PRIV") ||
      bus_fail "$line"
    note "$line"
  done
  pass
fi

if section_gate both-ways; then
  begin both-ways
  session_record priv
  session_record open
  line=$(audit_reads "the PRIV session's connection" "$work/records/session-priv.jsonl" PUB "$work/markers-PUB") || bus_fail "$line"
  note "$line"
  line=$(audit_reads "the no-project session's connection" "$work/records/session-open.jsonl" PRIV "$work/markers-PRIV") ||
    bus_fail "$line"
  note "$line"
  pass
fi

if section_gate privacy-read; then
  begin privacy-read
  tail -n "+$((privacy_baseline + 1))" "$work/privacy-reads.jsonl" >"$work/privacy-reads-run.jsonl"
  line=$(judge_privacy_reads "$work/privacy-reads-run.jsonl" listener) || bus_fail "$line"
  note "$line"
  pass
fi

begin verdict
bus_verdict || fail "see the lines above"
pass
