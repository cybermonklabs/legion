# shellcheck shell=bash disable=SC2154 # the variables below are the caller's (see the header)
# The harness the two live two-builds proofs share: scripts/e2e/dispatch-two-builds.sh and
# scripts/e2e/dispatch-search-two-builds.sh, each of which runs the `envoy-dispatch` binary of an
# older commit beside this checkout's against one PostgreSQL, the way a rolling release serves
# both at once. Everything here belongs to both of them — the --base-commit argument, the sweep
# of what a pane exports, the teardown, the HTTP call, the prerequisites, the build of both
# binaries, the reset of the database's `public` schema, the fake Envoy and the start of a server
# — so a fix to the loopback guard or to the reset lands in one place. Each proof keeps its own
# header, its own ports, its own seed and its own cases.
#
# Sourced, never run. It sources lib/rig.sh itself, so a proof sources this alone. Before it does,
# the caller sets what lib/rig.sh asks of it —
#   root           the checkout
#   evidence       empty; two_builds_build sets it to the work directory
#   timeout_hook   empty, or a function a timed-out until_true runs before it fails
# — and defines note and fail (which exits). cleanup below also reads two of the caller's own:
#   check          the check a failure is reported at
#   started        $SECONDS when the run began
# and the caller sets its three ports, base_port, head_port and envoy_port, before it calls
# anything here.
#
# What this sets for the caller: base_commit and database_url, work and the pids fake_envoy_pid,
# base_pid and head_pid (two_builds_setup), base_sha (two_builds_prerequisites), and agent_token,
# signin_key, base_url and head_url (two_builds_identity). call sends to target_url, which the
# caller sets to the build it is talking to, and waits call_seconds for an answer, which the
# caller may set; it is 20 here. sign_in and call_person do the same as a person.
#
# A proof calls these in the order they stand in below: two_builds_setup once, then
# two_builds_prerequisites, two_builds_build, two_builds_reset_database, two_builds_fake_envoy
# and two_builds_identity, each inside a check of its own, and start_server for each build. The
# names a proof's own body reads — call, json_field, start_server and cleanup — are the ones
# those parts already had.

# shellcheck source-path=SCRIPTDIR source=rig.sh
. "$root/scripts/e2e/lib/rig.sh"

# How long one call waits for an answer. A proof whose reads take longer sets its own.
call_seconds=20

# two_builds_setup LABEL ARGUMENT... reads the run's arguments and its database URL, drops what no
# process the run starts may inherit, and arms the teardown. LABEL is the run's name in cleanup's
# last line ("dispatch two builds e2e"), which is the one thing the two proofs' teardown differs
# in.
two_builds_setup() {
  run_label=$1
  shift
  # The proof's own inputs, read before the sweep below drops every DISPATCH_* variable.
  base_commit=''
  while [ $# -gt 0 ]; do
    case "$1" in
      --base-commit)
        [ $# -ge 2 ] || fail "--base-commit takes the commit the base build is built from"
        base_commit=$2
        shift 2
        ;;
      --base-commit=*)
        base_commit=${1#--base-commit=}
        shift
        ;;
      *) fail "unknown argument '$1': this script takes --base-commit <rev> alone" ;;
    esac
  done
  database_url=${DATABASE_URL:-}

  # A Legion pane exports the real listener (ENVOY_URL), the real Dispatch (DISPATCH_URL,
  # DISPATCH_TOKEN_FILE) and an nkey for the real NATS; nothing this run starts inherits any of it.
  # PGHOSTADDR, PGSERVICE and PGSERVICEFILE go with them: require_loopback_dsn in
  # scripts/e2e/lib/loopback-dsn.sh says why a caller of the loopback guard clears the three.
  local name
  local -a inherited
  mapfile -t inherited < <(compgen -e)
  for name in "${inherited[@]}"; do
    case "$name" in
      DISPATCH_* | ENVOY_* | NATS_* | PGHOSTADDR | PGSERVICE | PGSERVICEFILE) unset "$name" ;;
    esac
  done

  # The work directory is made once the prerequisites hold, so a refused run leaves nothing behind.
  work=''
  fake_envoy_pid=''
  base_pid=''
  head_pid=''
  target_url=''
  agent_token=''

  trap 'cleanup "$?"' EXIT
  trap 'cleanup 130' INT
  trap 'cleanup 143' TERM
}

# cleanup stops what the run started and exits with rc: the script's own status for EXIT, 128 plus
# the signal's number for INT and TERM, so an interrupted run never reports success.
cleanup() {
  local rc=$1
  trap - EXIT INT TERM
  set +e
  stop_pid "$head_pid"
  stop_pid "$base_pid"
  stop_pid "$fake_envoy_pid"
  if [ -z "$work" ]; then
    echo "$run_label: FAIL at $check after $((SECONDS - started))s" >&2
  elif [ "$rc" -eq 0 ]; then
    rm -rf "$work"
    echo "$run_label: PASS in $((SECONDS - started))s"
  else
    rm -rf "${work:?}/bin" "${work:?}/base"
    echo "$run_label: FAIL at $check after $((SECONDS - started))s (logs: $work)" >&2
  fi
  exit "$rc"
}

# call METHOD PATH [BODY] prints what the server on $target_url answers an agent bearer, and
# returns non-zero with the answer on stderr when it is not 2xx, so a caller inside a bounded
# wait polls again and a caller outside one fails the check. The body of an agent's write
# carries its actor.
call() { call_as bearer "$@"; }

# call_person METHOD PATH [BODY] is call as the person sign_in signed in: their session cookie
# and, as every cookie write needs (enforceCookieOrigin, internal/dispatch/routes/router.go), the
# build's own origin as Origin, with no bearer, so Dispatch takes the write as that person's
# (an actor of kind user) and the body names no actor.
call_person() { call_as person "$@"; }

call_as() {
  local as=$1 method=$2 path=$3 body=${4:-} out status
  local -a credential data=()
  if [ "$as" = person ]; then
    credential=(-b "$work/person-cookies" -H "Origin: $target_url")
  else
    credential=(-H "Authorization: Bearer $agent_token")
  fi
  [ -z "$body" ] || data=(-H 'Content-Type: application/json' -d "$body")
  out=$(mktemp "$work/call.XXXXXX")
  status=$(curl -sS -o "$out" -w '%{http_code}' --max-time "$call_seconds" -X "$method" \
    "${credential[@]}" "${data[@]}" "$target_url$path") || status=000
  case "$status" in
    2??)
      cat "$out"
      rm -f "$out"
      ;;
    *)
      echo "$method $path answered $status: $(cat "$out")" >&2
      rm -f "$out"
      return 1
      ;;
  esac
}

# sign_in LOGIN signs the person LOGIN, an email, in at the dev sign-in route of the build on
# $target_url (GET /auth/_dev/signin, which start_server's DISPATCH_DEV_SIGNIN=1 mounts), which
# records them in `people` as a real sign-in does, and keeps their session cookie in
# $work/person-cookies for call_person. It returns non-zero, naming the answer on stderr, when the
# route answers anything but its redirect with a cookie. Both builds sign with one key
# (two_builds_identity) over one database, so the cookie is good against either.
sign_in() {
  local status
  rm -f "$work/person-cookies"
  status=$(curl -sS -o /dev/null -w '%{http_code}' --max-time "$call_seconds" -c "$work/person-cookies" \
    "$target_url/auth/_dev/signin?login=$1") || status=000
  if [ "$status" != 302 ] || ! grep -q dsession "$work/person-cookies" 2>/dev/null; then
    echo "GET /auth/_dev/signin for $1 answered $status with no session cookie" >&2
    return 1
  fi
}

# json_field FIELD reads one string member of a JSON object on stdin. bun is already required by
# the harness this run borrows its fake Envoy from.
json_field() {
  # shellcheck disable=SC2016 # the JavaScript below is bun's, not this shell's
  bun -e 'const field = process.argv[1]; let data = ""; process.stdin.on("data", (chunk) => { data += chunk; }); process.stdin.on("end", () => { const value = JSON.parse(data)[field]; if (typeof value !== "string" || value === "") { console.error(`no ${field} in ${data}`); process.exit(1); } console.log(value); });' "$1"
}

# two_builds_prerequisites REFUSAL [CHECK...] makes every check the two proofs share, in the order
# they make them. REFUSAL is what a run with no --base-commit is told, which each proof says in
# its own terms. CHECK, when given, is the proof's own prerequisite, run after the shared ones and
# before the two notes below, so what it notes lands where it did.
two_builds_prerequisites() {
  local refusal=$1 db_host tool port
  shift
  [ -n "$database_url" ] ||
    fail "DATABASE_URL is required: it names a loopback PostgreSQL database this run resets (postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable)"
  [ -n "$base_commit" ] || fail "$refusal"
  case "$database_url" in
    postgres://* | postgresql://*) ;;
    *) fail "DATABASE_URL must be a postgres:// or postgresql:// URL" ;;
  esac
  # tar is in the list because two_builds_build takes the base tree with `git archive | tar -x`,
  # and python3 because require_loopback_dsn below runs lib/loopback-dsn.py, which is why the
  # tool check comes first.
  for tool in go bun curl psql git tar openssl python3; do
    command -v "$tool" >/dev/null || fail "$tool is not on PATH"
  done
  # The one loopback guard of this repository (lib/loopback-dsn.sh, which lib/rig.sh sources),
  # rather than a reading of this file's own: libpq and a URL parser read different text as the
  # host, and the run must connect to the host the guard admitted. Both proofs reset the
  # database's `public` schema, so neither may reach any server but this machine's.
  require_loopback_dsn db_host DATABASE_URL "$database_url" "this run resets the database" ||
    fail "$db_host"
  base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
    fail "--base-commit '$base_commit' names no commit of this repository"
  for port in "$base_port" "$head_port" "$envoy_port"; do
    if ! [[ "$port" =~ ^[1-9][0-9]{0,4}$ ]] || [ "$port" -gt 65535 ]; then
      fail "'$port' is not a port"
    fi
    port_answers "$port" && fail "something already answers on 127.0.0.1:$port"
  done
  if [ "$base_port" = "$head_port" ] || [ "$base_port" = "$envoy_port" ] || [ "$head_port" = "$envoy_port" ]; then
    fail "the base, head and fake Envoy ports must differ ($base_port, $head_port, $envoy_port)"
  fi
  # The `vector` extension is the deployment's bootstrap's to install, as a cluster's master role
  # (0072_embeddings_core.up.sql), and the role this run connects as is not one, so a database
  # without it is one no build can migrate. It is read here rather than met with a failed boot.
  [ "$(psql "$database_url" -X -tAc "select count(*) from pg_extension where extname = 'vector'" 2>/dev/null)" = 1 ] ||
    fail "DATABASE_URL must name a database that already holds the vector extension, which only the cluster's master role can install"
  [ $# -eq 0 ] || "$@"
  note "database host $db_host (URL not printed)"
  note "base commit $base_sha"
}

# two_builds_build NAME makes the run's work directory, under TMPDIR and named for the proof, and
# builds both binaries into it.
two_builds_build() {
  work=$(mktemp -d "${TMPDIR:-/tmp}/$1.XXXXXXXX")
  evidence=$work
  note "work directory $work"
  mkdir -p "$work/bin" "$work/base" "$work/web-dist" "$work/logs"
  # The base tree is the commit's own, taken out of the repository rather than checked out, so this
  # working copy is never touched.
  git -C "$root" archive "$base_sha" | tar -x -C "$work/base" ||
    fail "could not take the tree of $base_sha"
  go -C "$work/base/packages/envoy" build -o "$work/bin/dispatch-base" ./cmd/dispatch ||
    fail "the base build's ./cmd/dispatch did not build"
  go -C "$root/packages/envoy" build -o "$work/bin/dispatch-head" ./cmd/dispatch ||
    fail "this checkout's ./cmd/dispatch did not build"
  note "built the base build from $base_sha and this build from $root"
}

# two_builds_reset_database empties the database's public schema. Empty, migrations included: the
# base build migrates the new schema at boot, and this build migrates it forward, which is the
# upgrade both proofs are about. empty_public_schema (lib/loopback-dsn.sh) says what goes and why
# the schema's extensions stay.
two_builds_reset_database() {
  empty_public_schema "$database_url" || fail "could not empty the database's public schema"
}

# two_builds_fake_envoy starts the listener both builds are pointed at, which is the Dispatch
# browser harness's own fake, and waits for it to answer.
two_builds_fake_envoy() {
  FAKE_ENVOY_PORT=$envoy_port start_process fake_envoy bun "$root/packages/dispatch/e2e/fake-envoy.ts"
  await_start fake_envoy "$fake_envoy_pid" 60 "the fake Envoy listener to answer /healthz" \
    curl -fsS --max-time 2 "http://127.0.0.1:$envoy_port/healthz"
  note "fake Envoy listener on http://127.0.0.1:$envoy_port"
}

# two_builds_identity mints the agent bearer both builds take and the key both sign their cookies
# with, and names each build's URL.
two_builds_identity() {
  agent_token=$(openssl rand -hex 24)
  # Both processes serve one database, so each must sign its cookies with the key the other
  # holds, as e2e/run-server.sh gives two-process mode: a process with a key of its own is
  # refused registration by the one already live.
  signin_key=$(openssl rand -hex 32)
  base_url=http://127.0.0.1:$base_port
  head_url=http://127.0.0.1:$head_port
}

# start_server NAME BINARY PORT [VARIABLE=VALUE...] starts one build with this environment and
# nothing else: identity is the production cookie, minted at the dev sign-in route with a signing
# key the process generates for itself, HOME and the XDG directories name nothing, and both
# processes share one agent token, so a session's bearer is good against either. Each VARIABLE
# given is set beside them, which is where the two proofs differ: the search proof passes
# DISPATCH_DEV_EMBEDDER=1, which embeds in this process and calls no model service, and the
# reference-grammar proof passes none.
start_server() {
  local name=$1 binary=$2 port=$3
  shift 3
  start_process "$name" env -i \
    PATH="$PATH" \
    TMPDIR="$work" \
    HOME=/nonexistent \
    XDG_CACHE_HOME=/nonexistent \
    XDG_CONFIG_HOME=/nonexistent \
    XDG_DATA_HOME=/nonexistent \
    DATABASE_URL="$database_url" \
    DISPATCH_AGENT_TOKEN="$agent_token" \
    DISPATCH_DEV_SIGNIN=1 \
    DISPATCH_DEV_SIGNIN_SHARED_KEY="$signin_key" \
    DISPATCH_IDENTITY=cookie \
    DISPATCH_INSECURE_COOKIE=1 \
    DISPATCH_LISTEN_HOST=127.0.0.1 \
    DISPATCH_NATS_DISABLED=1 \
    DISPATCH_PORT="$port" \
    DISPATCH_SERVER_URL="http://127.0.0.1:$port" \
    DISPATCH_WEB_DIST="$work/web-dist" \
    ENVOY_URL="http://127.0.0.1:$envoy_port" \
    "$@" \
    "$binary"
}
