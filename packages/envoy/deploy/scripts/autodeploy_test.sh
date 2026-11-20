#!/usr/bin/env bash
set -euo pipefail

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
repo="$root/repo"
compose="$repo/packages/envoy/deploy/compose"
backups="$root/backups"
bin="$root/bin"
tools="$root/tools"
log="$root/compose.log"
argv="$root/argv.log"
schema="$root/schema"
mkdir -p "$compose" "$backups" "$bin" "$tools"
# The deployer reads Dispatch's settings from Compose's own resolution (`docker compose config`), so
# the fake docker below hands `compose version` and `compose ... config` to the real binary, run on
# the real compose file. A run without it fails here, by name: it never skips.
real_docker=$(command -v docker || true)
[[ -n "$real_docker" ]] || { printf 'autodeploy_test: docker is not on PATH; this test reads the resolution of docker compose config\n' >&2; exit 1; }
"$real_docker" compose version >&2 || { printf 'autodeploy_test: docker compose version failed with %s; this test needs Docker Compose\n' "$real_docker" >&2; exit 1; }
cp "$(dirname "$0")/../compose/dispatch.compose.yml" "$compose/dispatch.compose.yml"
# Compose takes the shell's value of a setting over the .env's: only a row that means to set one does.
unset ENVOY_ACCOUNT_GOOGLE_AUDIENCE ENVOY_ACCOUNT_TOKEN_FILE DISPATCH_SERVER_URL DATABASE_URL \
  DISPATCH_AGENT_TOKEN NATS_URLS ENVOY_IMAGE_TAG AUTODEPLOY_TEST_UNSET
# The database password, percent-encoded in DATABASE_URL as s%40cret-canary. Neither spelling may
# appear in the deployer's output or in any process's arguments.
password='s@cret-canary'
url_without_password='postgres://dispatch@db.test:5432/dispatch?sslmode=require'
# The schema read, as it reaches psql: the connection on the command line, and on standard input
# a script that tries for the migration key and reads the versions only once the try was granted.
# The fake psql below reads that script on standard input, refuses a run that sends none, and
# records every statement it actually runs as a psql-sql line of the argv log, so the test counts
# the reads apart from the runs.
psql_run="^psql .*$url_without_password\$"
schema_try='psql-sql select pg_try_advisory_lock(8150001)'
schema_read='psql-sql select version from schema_migrations order by version'
fresh_env() {
  cat >"$compose/.env" <<EOF
ENVOY_IMAGE_TAG=old-image
DATABASE_URL=${1:-postgres://dispatch:s%40cret-canary@db.test:5432/dispatch?sslmode=require}
DISPATCH_SERVER_URL=http://browser.test:8766
ENVOY_ACCOUNT_GOOGLE_AUDIENCE=envoy-listener
DISPATCH_AGENT_TOKEN=agent-token
NATS_URLS=nats://nats.test:4222
EOF
  # The versions schema_migrations holds, one per line, as the deployer reads them: a set, not a
  # level. A migration applied below the highest one changes the set and leaves max(version) where
  # it was.
  seq 1 112 >"$schema"
  rm -f "$schema.up" "$schema.running" "$schema.candidate" "$schema.held" "$schema.key"
  touch "$schema.running"
  : >"$log"
  : >"$argv"
}

cat >"$bin/jj" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *" git fetch"*) ;;
  *"ancestors(main@origin, 30)"*) printf 'new-image\n' ;;
  *"description.first_line()"*) printf 'test deployment\n' ;;
esac
EOF
# docker runs a container's command for real, as the container would: in an environment holding
# only the variables passed by name with -e, and with the fake psql and pg_dump below on its PATH.
# Every process it starts records its arguments, so the test reads what a host's
# /proc/<pid>/cmdline would show.
cat >"$bin/docker" <<'EOF'
#!/usr/bin/env bash
held_migration_commits() {
  # A migration the candidate held open commits once Compose stops or replaces the candidate, and
  # commits once however often Compose runs.
  if [[ -n "${AUTODEPLOY_TEST_MIGRATE_ON_STOP:-}" && -e "$AUTODEPLOY_TEST_SCHEMA.up" && ! -e "$AUTODEPLOY_TEST_SCHEMA.held" ]]; then
    printf '%s\n' "$AUTODEPLOY_TEST_MIGRATE_ON_STOP" >>"$AUTODEPLOY_TEST_SCHEMA"
    touch "$AUTODEPLOY_TEST_SCHEMA.held"
  fi
}
case "$1" in
  manifest)
    exit 0
    ;;
  run)
    printf 'docker %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
    shift
    names=()
    while [[ "$1" == -* ]]; do
      case "$1" in
        --rm | --init) shift ;;
        --network) shift 2 ;;
        -e) names+=("$2"); shift 2 ;;
        *) printf 'unexpected docker run option: %s\n' "$1" >&2; exit 1 ;;
      esac
    done
    shift
    printf 'container %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
    container_env=(PATH="$AUTODEPLOY_TEST_TOOLS:/usr/bin:/bin")
    for name in "${names[@]}" ${!AUTODEPLOY_TEST_@}; do
      if [[ -n "${!name+set}" ]]; then
        container_env+=("$name=${!name}")
      fi
    done
    exec env -i "${container_env[@]}" "$@"
    ;;
  compose)
    # Compose's own reading of the file: the real binary, and no line of the compose log.
    # With AUTODEPLOY_TEST_ENV_READ_FAIL=1, a read of the env file's variables alone fails: the
    # deployer's reader document on standard input (`-f -`), or `config --environment`.
    if [[ "${AUTODEPLOY_TEST_ENV_READ_FAIL:-}" == 1 && (" $* " == *" -f - "* || " $* " == *" --environment "*) ]]; then
      printf 'the env read failed\n' >&2
      exit 1
    fi
    case "$*" in
      "compose version"* | *" config "*) exec "$AUTODEPLOY_TEST_REAL_DOCKER" "$@" ;;
    esac
    printf '%s\n' "$*" >>"$AUTODEPLOY_TEST_LOG"
    case "$*" in
      *" pull "*)
        if [[ "${AUTODEPLOY_TEST_PULL_FAIL:-}" == 1 ]]; then
          exit 1
        fi
        ;;
      *" up "*)
        if [[ "${ENVOY_IMAGE_TAG:-}" == new-image ]]; then
          touch "$AUTODEPLOY_TEST_SCHEMA.up" "$AUTODEPLOY_TEST_SCHEMA.candidate"
          if [[ -n "${AUTODEPLOY_TEST_MIGRATE_TO:-}" ]]; then
            printf '%s\n' "$AUTODEPLOY_TEST_MIGRATE_TO" >>"$AUTODEPLOY_TEST_SCHEMA"
          fi
        else
          held_migration_commits
          rm -f "$AUTODEPLOY_TEST_SCHEMA.candidate"
        fi
        touch "$AUTODEPLOY_TEST_SCHEMA.running"
        ;;
      *" stop "*)
        if [[ "${AUTODEPLOY_TEST_STOP_FAIL:-}" == 1 ]]; then
          exit 1
        fi
        held_migration_commits
        if [[ "${AUTODEPLOY_TEST_STOP_LEAVES_RUNNING:-}" != 1 ]]; then
          rm -f "$AUTODEPLOY_TEST_SCHEMA.running"
        fi
        ;;
      *" ps "*)
        if [[ -e "$AUTODEPLOY_TEST_SCHEMA.running" ]]; then
          printf 'candidate-container\n'
        fi
        ;;
    esac
    ;;
  *)
    printf 'unexpected docker invocation: %s\n' "$*" >&2
    exit 1
    ;;
esac
EOF
# curl answers on Dispatch's port alone, AUTODEPLOY_TEST_PORT (default 8766): a check that dials
# another, a quoted port among them, gets nothing, as from a port no process listens on. /healthz
# answers 200 with NATS connected, but for new-image with AUTODEPLOY_TEST_CANDIDATE_NATS set, which
# it reports as `nats`. /auth/start redirects to AUTODEPLOY_TEST_REDIRECT, the callback a good
# deploy's login check expects, and to a wrong host when that is unset.
cat >"$bin/curl" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"http://127.0.0.1:${AUTODEPLOY_TEST_PORT:-8766}/"*) ;;
  *) exit 7 ;;
esac
case "$*" in
  *"/healthz"*)
    nats=true
    if [[ -e "$AUTODEPLOY_TEST_SCHEMA.candidate" ]]; then
      nats="${AUTODEPLOY_TEST_CANDIDATE_NATS:-true}"
    fi
    printf '{"ok":true,"db":true,"nats":%s}' "$nats"
    ;;
  *"/auth/start"*)
    if [[ -n "${AUTODEPLOY_TEST_REDIRECT:-}" ]]; then
      printf 'https://github.com/login/oauth/authorize?redirect_uri=%s' \
        "$(python3 -c 'import sys, urllib.parse as u; print(u.quote(sys.argv[1], safe=""))' "$AUTODEPLOY_TEST_REDIRECT")"
    else
      printf 'https://github.com/login/oauth/authorize?redirect_uri=http%%3A%%2F%%2Fwrong.test%%3A8766%%2Fauth%%2Fcallback'
    fi
    ;;
esac
EOF
# The health check's three-second waits pass at once, so a check that never passes fails in no
# time. A case whose subject is how long the deployer takes, such as the poll of the migration
# key against its deadline, sets AUTODEPLOY_TEST_SLEEP_REAL=1 and gets the real sleep instead;
# every other case keeps its speed.
cat >"$bin/sleep" <<'EOF'
#!/usr/bin/env bash
if [[ "${AUTODEPLOY_TEST_SLEEP_REAL:-}" == 1 ]]; then
  exec /bin/sleep "$@"
fi
EOF
# timeout records the bound a database client container was given and then runs it, so a case can
# read what each try for the migration key was allowed. It bounds nothing itself: whatever the
# duration, 0 (pg_client's unbounded) included, the command runs as it would have, so every case
# runs as it did before this stand-in existed. Only a container run is recorded, because the
# deployer also bounds its compose and manifest runs this way, and a case that reaches no client
# reads an empty argument log as proof that nothing ran. With AUTODEPLOY_TEST_CONFIG_TIMEOUT=1 a
# `docker compose ... config` run ends as a timeout ends it, status 124, without running.
cat >"$bin/timeout" <<'EOF'
#!/usr/bin/env bash
if [[ "$2 $3" == "docker run" ]]; then
  printf 'timeout %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
fi
if [[ "${AUTODEPLOY_TEST_CONFIG_TIMEOUT:-}" == 1 && "$2 $3" == "docker compose" && " $* " == *" config "* ]]; then
  exit 124
fi
exec "${@:2}"
EOF
# python3 is the real one, but for a run with AUTODEPLOY_TEST_PYTHON_FAIL=1, where it exits 3 having
# read its input and printed nothing, as an interpreter that cannot run does. The real one is named
# by the interpreter's own path (sys.executable), not looked up on PATH again, where a shim (mise's)
# would find this fake and recurse.
real_python3=$(python3 -c 'import sys; print(sys.executable)')
cat >"$bin/python3" <<EOF
#!/usr/bin/env bash
if [[ "\${AUTODEPLOY_TEST_PYTHON_FAIL:-}" == 1 ]]; then
  cat >/dev/null
  exit 3
fi
exec "$real_python3" "\$@"
EOF
# mv is the real one, but a move onto the env file records its source and the source's mode, and,
# for a run with AUTODEPLOY_TEST_KILL_AT_ENV_MV=1, kills the deployer there with SIGKILL, which no
# trap sees, or, with AUTODEPLOY_TEST_TERM_AT_ENV_MV=1, sends it TERM, as a deployer stopped between
# its write and its rename is.
cat >"$bin/mv" <<'EOF'
#!/usr/bin/env bash
if [[ "${*: -1}" == */.env ]]; then
  source=${*: -2:1}
  printf 'mv-source %s %s\n' "$source" "$(stat -c %a "$source")" >>"$AUTODEPLOY_TEST_ARGV"
  if [[ "${AUTODEPLOY_TEST_KILL_AT_ENV_MV:-}" == 1 ]]; then
    kill -9 "$PPID"
    exit 137
  fi
  if [[ "${AUTODEPLOY_TEST_TERM_AT_ENV_MV:-}" == 1 ]]; then
    kill -TERM "$PPID"
    exit 1
  fi
fi
exec /bin/mv "$@"
EOF
# The fake libpq, which splits a URL as libpq does rather than as the deployer does: the
# credentials run to the first @ before the first /, a raw ? included, and a query key or value is
# percent-decoded, so password= (however its key is spelled) sets the password. The password is
# PGPASSWORD, else the URL's. A malformed percent-encoding is quoted on stderr, as psql 16 does.
cat >"$tools/libpq.sh" <<'EOF'
percent_decode() {
  local valid='^([^%]|%[0-9A-Fa-f]{2})*$'
  [[ "$2" =~ $valid ]] || return 1
  printf -v "$1" '%b' "${2//%/\\x}"
}
connection_password() {
  local url="${!#}"
  local rest="${url#*://}"
  local before_path="${rest%%/*}"
  local after_credentials="$rest"
  local userinfo query param key value
  libpq_password="${PGPASSWORD-}"
  if [[ "$before_path" == *@* ]]; then
    userinfo="${before_path%%@*}"
    after_credentials="${rest#*@}"
    if [[ "$userinfo" == *:* ]] && ! percent_decode libpq_password "${userinfo#*:}"; then
      printf '%s: error: invalid percent-encoded token: "%s"\n' "$1" "${userinfo#*:}" >&2
      return 2
    fi
  fi
  if [[ "$after_credentials" == *\?* ]]; then
    query="${after_credentials#*\?}"
    while [[ -n "$query" ]]; do
      param="${query%%&*}"
      if [[ "$query" == *\&* ]]; then query="${query#*&}"; else query=""; fi
      percent_decode key "${param%%=*}" && percent_decode value "${param#*=}" || return 2
      if [[ "$key" == password ]]; then
        libpq_password="$value"
      fi
    done
  fi
  if [[ "$libpq_password" != "$AUTODEPLOY_TEST_PASSWORD" ]]; then
    printf '%s: error: password authentication failed for user "dispatch"\n' "$1" >&2
    return 2
  fi
}
EOF
cat >"$tools/psql" <<'EOF'
#!/usr/bin/env bash
printf 'psql %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
# What a psql diagnostic quoting its own arguments would print. The deployer must not pass it on.
printf 'psql: PSQL-STDERR-CANARY %s\n' "$*" >&2
source "$AUTODEPLOY_TEST_TOOLS/libpq.sh"
connection_password psql "$@" || exit 2
if [[ "${AUTODEPLOY_TEST_SCHEMA_UNREADABLE:-}" == 1 ||
  ("${AUTODEPLOY_TEST_SCHEMA_UNREADABLE_AFTER_UP:-}" == 1 && -e "$AUTODEPLOY_TEST_SCHEMA.up") ]]; then
  exit 2
fi
# The migration advisory key, as the server holds it. $schema.key, holding a number above zero,
# stands for another session holding the key; every statement that reaches for it takes one off
# that number, so the key comes free after that many tries. The word `always` stands for a holder
# that never lets go of it inside the read's deadline, as a backend left building an index
# concurrently by a killed pod does. A try (pg_try_advisory_lock) refused the key answers f, and
# one that took it answers t. No fake can model the snapshot the read takes, so what this pins is
# which statements run, in which order, in the one run.
key_held() {
  local remaining
  [[ -s "$AUTODEPLOY_TEST_SCHEMA.key" ]] || return 1
  remaining=$(<"$AUTODEPLOY_TEST_SCHEMA.key")
  [[ "$remaining" == always ]] && return 0
  ((remaining > 0)) || return 1
  printf '%s\n' "$((remaining - 1))" >"$AUTODEPLOY_TEST_SCHEMA.key"
}
got=
sql_out=
# run_statement runs one statement of this session and leaves what it returns in sql_out, as psql
# does under ON_ERROR_STOP=1: a statement that errors ends the run, whatever was to follow it.
run_statement() {
  local stmt="${1%;}"
  stmt="${stmt% as got}"
  sql_out=
  printf 'psql-sql %s\n' "$stmt" >>"$AUTODEPLOY_TEST_ARGV"
  case "$stmt" in
    'set lock_timeout = 30000') ;;
    'select pg_try_advisory_lock(8150001)')
      if key_held; then got=f; else got=t; fi
      sql_out="$got"
      ;;
    'select version from schema_migrations order by version')
      if [[ -z "$got" ]]; then
        printf 'psql: read of schema_migrations outside the session that tried for the key: %s\n' "$stmt" >&2
        exit 3
      fi
      if [[ "$got" == f ]]; then
        # The read the deployer must never reach. Behind a key another session holds, a read can
        # fail: a first boot holds the key before schema_migrations is committed, and the
        # runner's widening ALTER TABLE makes the read wait out its lock_timeout. Under
        # ON_ERROR_STOP=1 that ends psql non-zero, and a deployer that ran the read whatever the
        # try answered would take that for a failed read rather than try the key again.
        printf 'psql: ERROR: canceling statement due to lock timeout\n' >&2
        exit 1
      fi
      # The versions as `order by version` returns them.
      sql_out=$(sort -n "$AUTODEPLOY_TEST_SCHEMA")
      ;;
    *)
      printf 'psql: unexpected statement: %s\n' "$stmt" >&2
      exit 3
      ;;
  esac
}
# The run's script: the deployer sends it on standard input with -f -, and a run without -f -
# fails here rather than passing unnoticed.
script=0
args=("$@")
i=0
while ((i < ${#args[@]})); do
  case "${args[i]}" in
    -f)
      [[ "${args[i + 1]}" == - ]] && script=1
      i=$((i + 2))
      ;;
    -v) i=$((i + 2)) ;;
    *) i=$((i + 1)) ;;
  esac
done
if ((script == 0)); then
  printf 'psql: this run sends no script on standard input (-f -): %s\n' "$*" >&2
  exit 3
fi
# psql reading a script: \gset keeps a statement's one value in a variable instead of printing
# it, \echo prints a variable, and what stands between \if and \endif runs only where the
# variable is true.
skip=0
while IFS= read -r line; do
  case "$line" in
    '') ;;
    '\echo :got') ((skip)) || printf '%s\n' "$got" ;;
    '\if :got') [[ "$got" == t ]] || skip=1 ;;
    '\endif') skip=0 ;;
    *'\gset')
      ((skip)) || run_statement "${line% \\gset}"
      ;;
    \\*)
      printf 'psql: unexpected backslash command: %s\n' "$line" >&2
      exit 3
      ;;
    *)
      if ((skip)); then continue; fi
      run_statement "$line"
      [[ -z "$sql_out" ]] || printf '%s\n' "$sql_out"
      ;;
  esac
done
exit 0
EOF
cat >"$tools/pg_dump" <<'EOF'
#!/usr/bin/env bash
printf 'pg_dump %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
source "$AUTODEPLOY_TEST_TOOLS/libpq.sh"
connection_password pg_dump "$@" || exit 1
printf 'backup'
EOF
chmod +x "$bin/jj" "$bin/docker" "$bin/curl" "$bin/sleep" "$bin/timeout" "$bin/python3" "$bin/mv" "$tools/psql" "$tools/pg_dump"
# The python3 fake runs the real interpreter, on any PATH this box has (a mise shim included).
PATH="$bin:$PATH" python3 -c 'import sys' ||
  { printf 'autodeploy_test: the python3 stand-in cannot run the real python3 (%s)\n' "$real_python3" >&2; exit 1; }

deploy_once() {
  PATH="$bin:$PATH" AUTODEPLOY_TEST_LOG="$log" AUTODEPLOY_TEST_ARGV="$argv" AUTODEPLOY_TEST_SCHEMA="$schema" \
    AUTODEPLOY_TEST_TOOLS="$tools" AUTODEPLOY_TEST_PASSWORD="${test_password-$password}" REPO="$repo" COMPOSE_DIR="$compose" \
    AUTODEPLOY_TEST_REAL_DOCKER="${test_real_docker-$real_docker}" \
    BACKUPS="${test_backups-$backups}" COMPOSE_PROJECT=dispatch-test IMAGE=example.test/dispatch \
    "$(dirname "$0")/autodeploy.sh" --once 2>&1
}
fail() {
  printf '%s\n' "$1" >&2
  printf -- '--- deployer output:\n%s\n--- compose:\n%s\n--- argv:\n%s\n' "$output" "$(<"$log")" "$(<"$argv")" >&2
  exit 1
}
# No secret in the output, and none in any process's arguments, Docker's or the container's.
no_secret() {
  if [[ "$output" == *cret-canary* || "$output" == *review-secret* || "$output" == *PSQL-STDERR-CANARY* ]] ||
    grep -q -e 'cret-canary' -e 'review-secret' "$argv"; then
    fail "$1: a password or psql's stderr reached the output or a process's arguments"
  fi
}
# The value the deployer records for the version set in $schema: <max>-<count>-<sha256 of the
# versions, ascending, one per line>.
schema_value() {
  local versions
  versions=$(sort -n "$schema")
  printf '%s-%s-%s\n' "$(printf '%s\n' "$versions" | tail -n1)" "$(printf '%s\n' "$versions" | wc -l)" \
    "$(printf '%s\n' "$versions" | sha256sum | cut -d' ' -f1)"
}

# A .env with neither of Dispatch's identity settings is refused before anything of the deploy
# runs: Dispatch's container refuses to start without one and restart-loops, and so would the
# rollback's, under the same compose file. No backup is taken, compose is not run, and the previous
# image still serves.
fresh_env
sed -i '/^ENVOY_ACCOUNT_GOOGLE_AUDIENCE=/d' "$compose/.env"
if output=$(deploy_once); then
  fail 'deployer deployed with no identity for Dispatch in .env'
fi
if [[ "$output" != *"neither ENVOY_ACCOUNT_GOOGLE_AUDIENCE nor ENVOY_ACCOUNT_TOKEN_FILE"*"not deploying new-image"* ]]; then
  fail 'deployer did not name both identity settings when it refused'
fi
if [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv" ||
  [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer backed up or ran compose with no identity for Dispatch in .env'
fi

# The four settings the deployer checks before its backup are read from Compose's own resolution
# (`docker compose config`, the real binary on the real compose file). For each, as the setting the
# check depends on alone (the other identity setting taken out first, `drop`): the spellings Compose
# resolves empty, `NAME=""` and `NAME="${AUTODEPLOY_TEST_UNSET:-}"`, are refused before anything of
# the deploy runs, naming the setting; the spellings Compose resolves to the value, `export NAME=v`,
# `NAME=v # a note` and `NAME=${AUTODEPLOY_TEST_VALUE}` with that variable in the deployer's
# environment, go on to the backup.
with_line() {
  grep -v "^$1=" "$compose/.env" >"$compose/.env.new"
  printf '%s\n' "$2" >>"$compose/.env.new"
  mv "$compose/.env.new" "$compose/.env"
}
identity_refusal='neither ENVOY_ACCOUNT_GOOGLE_AUDIENCE nor ENVOY_ACCOUNT_TOKEN_FILE'
# The database URL fresh_env writes, read back rather than spelled again.
fresh_env
database_url=$(sed -n 's/^DATABASE_URL=//p' "$compose/.env")
[[ -n "$database_url" ]] || fail 'fresh_env wrote no DATABASE_URL'
for setting in \
  "ENVOY_ACCOUNT_GOOGLE_AUDIENCE|envoy-listener|$identity_refusal|-" \
  "ENVOY_ACCOUNT_TOKEN_FILE|/srv/dispatch/token|$identity_refusal|ENVOY_ACCOUNT_GOOGLE_AUDIENCE" \
  "DISPATCH_SERVER_URL|http://browser.test:8766|DISPATCH_SERVER_URL|-" \
  "DATABASE_URL|$database_url|DATABASE_URL|-"; do
  IFS='|' read -r name value refusal drop <<<"$setting"
  for spelling in "$name=\"\"" "$name=\"\${AUTODEPLOY_TEST_UNSET:-}\"" "export $name=$value" \
    "$name=$value # a note" "$name=\${AUTODEPLOY_TEST_VALUE}"; do
    fresh_env
    [[ "$drop" == - ]] || sed -i "/^$drop=/d" "$compose/.env"
    with_line "$name" "$spelling"
    case "$spelling" in
      *=\"\" | *UNSET*)
        if output=$(deploy_once); then
          fail "deployer deployed with $spelling, which Compose resolves empty"
        fi
        no_secret "the refusal of $name resolved empty"
        if [[ "$output" != *"$refusal"*"not deploying new-image"* ]]; then
          fail "deployer did not refuse $spelling, which Compose resolves empty, naming $name"
        fi
        if [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv"; then
          fail "deployer backed up or ran compose with $spelling, which Compose resolves empty"
        fi
        ;;
      *)
        output=$(AUTODEPLOY_TEST_VALUE="$value" deploy_once) || true
        if [[ "$output" == *"$refusal"*"not deploying"* ]] || ! grep -q '^pg_dump ' "$argv"; then
          fail "deployer did not go on to its backup with $spelling, which Compose resolves to $value"
        fi
        ;;
    esac
  done
done

# A required variable missing a value (here NATS_URLS, `${NATS_URLS:?...}` in the compose file) is
# Compose's own refusal: the deployer refuses before its backup with Compose's first line. And a
# DATABASE_URL Compose quotes back in an error (Compose's message about a value with the password in
# it) keeps the password out of the log: an unclosed quote in the .env makes Compose name the line.
fresh_env
sed -i '/^NATS_URLS=/d' "$compose/.env"
if output=$(deploy_once); then
  fail 'deployer deployed with NATS_URLS missing, which Compose refuses'
fi
if [[ "$output" != *"Compose refused"*"NATS_URLS"*"not deploying new-image"* ]] ||
  [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv"; then
  fail 'deployer did not refuse a configuration Compose refuses, by Compose'"'"'s own line, before its backup'
fi
fresh_env
with_line DATABASE_URL "DATABASE_URL=\"postgres://dispatch:$password@db.test:5432/dispatch"
if output=$(deploy_once); then
  fail 'deployer deployed with a DATABASE_URL Compose cannot read'
fi
no_secret 'Compose'"'"'s refusal of a DATABASE_URL it cannot read'
[[ "$output" == *"not deploying new-image"* ]] || fail 'deployer did not refuse a DATABASE_URL Compose cannot read'

# A configuration Compose refuses is logged by no text of Compose's: its output can begin with a
# warning, and can quote a .env line whole. With a `${...}` warning first and NATS_URLS missing, the
# refusal names NATS_URLS and holds no warning; an unterminated quote around a password with a space
# in it, or around the agent token, logs neither value, and names the command that shows Compose's
# message. The python3 step and Compose's timeout each name their cause.
fresh_env
sed -i '/^NATS_URLS=/d' "$compose/.env"
with_line DISPATCH_PORT "DISPATCH_PORT=\${AUTODEPLOY_TEST_UNSET}"
if output=$(deploy_once); then
  fail 'deployer deployed with NATS_URLS missing behind a warning'
fi
[[ "$output" == *"a required variable is missing a value: NATS_URLS; not deploying new-image"* && "$output" != *level=warning* && "$output" != *AUTODEPLOY_TEST_UNSET* ]] ||
  fail 'deployer did not name NATS_URLS alone when Compose warned first'
for line in "DATABASE_URL=\"postgres://dispatch@db.test:5432/dispatch?password='se cret'" 'DISPATCH_AGENT_TOKEN="agent-token-CANARY-1234'; do
  fresh_env
  with_line "${line%%=*}" "$line"
  if output=$(deploy_once); then
    fail "deployer deployed with an unterminated quote: ${line%%=*}"
  fi
  if [[ "$output" == *cret* || "$output" == *CANARY* ]] ||
    [[ "$output" != *"docker compose config exited "[0-9]*"; its message is not logged"*"docker compose -p dispatch-test -f $compose/dispatch.compose.yml config"* ]]; then
    fail "deployer logged a value from Compose's refusal of ${line%%=*}, or no command to read it"
  fi
done
fresh_env
if output=$(AUTODEPLOY_TEST_PYTHON_FAIL=1 deploy_once); then
  fail 'deployer deployed when python3 could not read the configuration'
fi
[[ "$output" == *"from docker compose config: python3 exited 3; could not read the deployed ENVOY_IMAGE_TAG; not deploying new-image"* && ! -s "$log" ]] ||
  fail 'deployer did not name the python3 failure'
fresh_env
if output=$(AUTODEPLOY_TEST_CONFIG_TIMEOUT=1 deploy_once); then
  fail 'deployer deployed when docker compose config timed out'
fi
[[ "$output" == *"docker compose config did not answer within 60 s; run \`docker compose -p dispatch-test -f $compose/dispatch.compose.yml config\` to see why; could not read the deployed ENVOY_IMAGE_TAG; not deploying new-image"* && ! -s "$log" ]] ||
  fail 'deployer did not name the config timeout'

# A read of the deployed tag that fails is refused by name, before anything of the deploy runs, and
# never taken for an env file that names no tag. On `d8760c42c9`, whose read swallowed the failure,
# the deployer reported "Compose resolves no ENVOY_IMAGE_TAG" instead.
fresh_env
if output=$(AUTODEPLOY_TEST_ENV_READ_FAIL=1 deploy_once); then
  fail 'deployer deployed when it could not read the deployed tag'
fi
if [[ "$output" != *"docker compose config exited 1; its message is not logged"*"could not read the deployed ENVOY_IMAGE_TAG; not deploying new-image"* ]] ||
  [[ "$output" == *"resolves no ENVOY_IMAGE_TAG"* || -s "$log" ]] || grep -q '^pg_dump ' "$argv"; then
  fail 'deployer did not refuse a failed read of the deployed tag by name, before its backup'
fi

# Without `docker compose`, the deployer refuses by name, before its backup.
fresh_env
test_real_docker=/bin/false
if output=$(deploy_once); then
  fail 'deployer deployed when docker compose cannot run'
fi
unset test_real_docker
if [[ "$output" != *"docker compose cannot run"*"not deploying new-image"* ]] ||
  [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv"; then
  fail 'deployer did not refuse, by name and before its backup, when docker compose cannot run'
fi

# compose_resolves NAME [VAR=value...] prints NAME as Compose resolves it from the test's
# compose/.env, by the real binary (`docker compose config --environment`), in the environment the
# deployer had: the VARs a row exported, and no ENVOY_IMAGE_TAG, which the deployer unsets.
compose_resolves() {
  local name=$1
  shift
  (
    for assignment in "$@"; do
      export "${assignment?}"
    done
    unset ENVOY_IMAGE_TAG
    "$real_docker" compose -f "$compose/dispatch.compose.yml" config --environment 2>/dev/null |
      awk -v key="$name=" 'index($0, key) == 1 && !found { print substr($0, length(key) + 1); found = 1 }'
  )
}
# good_deploy NAME VAR=value... deploys with the login check's callback right, so only a check that
# dials the wrong port, or a write Compose reads otherwise, can stop it. NAME is the row's.
good_deploy() {
  local name=$1 tag written schema_read
  shift
  output=$(
    for assignment in "$@" AUTODEPLOY_TEST_REDIRECT=http://browser.test:8766/auth/callback; do
      export "${assignment?}"
    done
    deploy_once
  ) || true
  if [[ "$output" != *"deployed new-image ("* ]] || [[ "$output" == *"rolling back"* ]]; then
    fail "$name: a good deploy did not land, or was rolled back"
  fi
  tag=$(compose_resolves ENVOY_IMAGE_TAG "$@")
  [[ "$tag" == new-image ]] || fail "$name: Compose resolves ENVOY_IMAGE_TAG to '$tag' after the deploy, not new-image"
  [[ $(grep -cE '^[[:space:]]*(export[[:space:]]+)?ENVOY_IMAGE_TAG[[:space:]]*=' "$compose/.env") == 1 ]] ||
    fail "$name: more than one line sets ENVOY_IMAGE_TAG after the deploy"
  # The schema record the deployer wrote is the value Compose reads back.
  written=$(sed -n 's/^ENVOY_IMAGE_SCHEMA=//p' "$compose/.env")
  schema_read=$(compose_resolves ENVOY_IMAGE_SCHEMA "$@")
  [[ "$written" == old-image:* && "$schema_read" == "$written" ]] ||
    fail "$name: the deployer wrote ENVOY_IMAGE_SCHEMA=$written, and Compose resolves '$schema_read'"
}

# DISPATCH_PORT is read as Compose resolves it for the container: quoted, or from the shell through
# `${...}`, the health and login checks dial the port Dispatch listens on, and a good deploy lands.
fresh_env
with_line DISPATCH_PORT 'DISPATCH_PORT="8766"'
good_deploy 'DISPATCH_PORT="8766"'
fresh_env
with_line DISPATCH_PORT "DISPATCH_PORT=\${PORT_FROM_SHELL}"
# The row's name is the .env line as written, `${...}` unexpanded.
# shellcheck disable=SC2016
good_deploy 'DISPATCH_PORT=${PORT_FROM_SHELL}' PORT_FROM_SHELL=9123 AUTODEPLOY_TEST_PORT=9123
# The tag an operator wrote as `export ENVOY_IMAGE_TAG="old-image"` is read as old-image, and the
# deploy's write leaves Compose reading new-image from one line, not the operator's line.
fresh_env
with_line ENVOY_IMAGE_TAG 'export ENVOY_IMAGE_TAG="old-image"'
good_deploy 'export ENVOY_IMAGE_TAG="old-image"'
[[ "$output" == *"(deployed old-image)"* ]] || fail 'the deployer did not read export ENVOY_IMAGE_TAG="old-image" as old-image'
# A DISPATCH_PORT Compose resolves to no port is refused before the backup.
fresh_env
with_line DISPATCH_PORT 'DISPATCH_PORT=not-a-port'
if output=$(deploy_once); then
  fail 'deployer deployed with DISPATCH_PORT=not-a-port'
fi
if [[ "$output" != *"DISPATCH_PORT to 'not-a-port'"*"not deploying new-image"* || -s "$log" ]] ||
  grep -q '^pg_dump ' "$argv"; then
  fail 'deployer did not refuse DISPATCH_PORT=not-a-port before its backup'
fi

# The static check: in autodeploy.sh and up-dispatch.sh, no line outside env_file, the one
# writer, refers to the env file's path - `COMPOSE_ENV`, `compose/.env`, or any `.env` path - so the
# only reads of it are Compose's own, and an alias, a backtick, an eval or a direct path, each of
# which has to name the path somewhere, is refused wherever it sits. Comment lines are not code. It
# fails, as UNKNOWN, when it finds the writer naming no path, which would mean it read nothing. Its
# controls are copies of autodeploy.sh with each of those four reads planted, each refused.
env_reads() {
  python3 - "$@" <<'PY'
import re, sys
writer = "env_file"
reference = re.compile(r"COMPOSE_ENV|compose/\.env|(^|[^A-Za-z0-9_.])\.env\b")
problems, writer_refs = [], 0
for path in sys.argv[1:]:
    function = None
    for n, line in enumerate(open(path).read().splitlines(), 1):
        start = re.match(r"([A-Za-z_][A-Za-z0-9_]*)\(\)\s*\{", line)
        if start:
            function = start.group(1)
            continue
        if line == "}":
            function = None
            continue
        if line.strip().startswith("#") or not reference.search(line):
            continue
        if function == writer:
            writer_refs += 1
            continue
        problems.append(f"{path}:{n}: refers to the env file's path outside {writer}: {line.strip()}")
if writer_refs == 0:
    problems.append(f"UNKNOWN: {writer} names the env file's path nowhere in {sys.argv[1:]}, so nothing was judged")
print("\n".join(problems))
sys.exit(1 if problems else 0)
PY
}
here=$(dirname "$0")
env_reads "$here/autodeploy.sh" "$here/up-dispatch.sh" >"$root/env-reads.txt" ||
  fail "a script refers to the env file's path outside its one writer: $(<"$root/env-reads.txt")"
# The plants are written as the script would hold them, `$(...)` and `$COMPOSE_DIR` unexpanded.
# shellcheck disable=SC2016
for plant in \
  'f="$COMPOSE_DIR/.env"
  x=$(sed -n "s/^DISPATCH_PORT=//p" "$f")' \
  'x=`sed -n s/^DISPATCH_PORT=//p $COMPOSE_DIR/.env`' \
  'eval '"'"'x=$(sed -n "s/^DISPATCH_PORT=//p" "$COMPOSE_DIR/.env")'"'"'' \
  'x=$(sed -n "s/^DISPATCH_PORT=//p" compose/.env)'; do
  PLANT=$plant python3 -c '
import os, sys
text = open(sys.argv[1]).read()
anchor = "deployed_tag() {\n"
assert text.count(anchor) == 1
sys.stdout.write(text.replace(anchor, anchor + "  " + os.environ["PLANT"] + "\n"))
' "$here/autodeploy.sh" >"$root/planted.sh"
  if env_reads "$root/planted.sh" >/dev/null; then
    fail "the static check passed a copy of autodeploy.sh with this read of the env file planted: $plant"
  fi
done

# The text scan above is fast, and it is a text scan: a path spelled in pieces or matched by a glob
# passes it. The behavioural check: every setting the deployer reads is written so that its raw
# text differs from Compose's resolution of it - single-quoted with a note after it, double-quoted,
# an `export` prefix, or a `${...}` reference to the deployer's environment - so a read of the
# file's text, however it is spelled, gets a value Compose does not give, and this good deploy does
# not land.
fresh_env
with_line ENVOY_IMAGE_TAG "ENVOY_IMAGE_TAG='old-image' # the tag that serves"
# shellcheck disable=SC2016 # the .env lines are written as the file holds them, `${...}` unexpanded
with_line DISPATCH_SERVER_URL 'DISPATCH_SERVER_URL="${AUTODEPLOY_TEST_ORIGIN}"'
with_line DATABASE_URL "DATABASE_URL=\"$database_url\""
# shellcheck disable=SC2016
with_line DISPATCH_PORT 'DISPATCH_PORT="${AUTODEPLOY_TEST_PORT_VALUE}" # the port'
with_line ENVOY_ACCOUNT_GOOGLE_AUDIENCE "export ENVOY_ACCOUNT_GOOGLE_AUDIENCE='envoy-listener'"
good_deploy 'a .env whose every raw value differs from Compose'"'"'s resolution' \
  AUTODEPLOY_TEST_ORIGIN=http://browser.test:8766 AUTODEPLOY_TEST_PORT_VALUE=9124 AUTODEPLOY_TEST_PORT=9124

# The env file owns the deployed tag: a deployer started with ENVOY_IMAGE_TAG in its environment
# deploys once, reads the tag it wrote back, and the next pass deploys nothing, where one that let
# the inherited value win over the file would deploy the same image again on every pass.
fresh_env
good_deploy 'a deployer started with ENVOY_IMAGE_TAG=old-image exported' ENVOY_IMAGE_TAG=old-image
: >"$log"
: >"$argv"
output=$(export ENVOY_IMAGE_TAG=old-image; deploy_once) || true
if [[ "$output" == *"main has a newer published image"* || -s "$log" ]] || grep -q '^pg_dump ' "$argv"; then
  fail 'a deployer started with ENVOY_IMAGE_TAG exported deployed the same image again on its next pass'
fi

# The env file is written through a staging file beside it, `.env.stage.XXXXXX`, mode 0600, renamed
# over it. A deployer killed there by SIGKILL, which no trap sees, leaves that file: git ignores
# exactly that name (packages/envoy/.gitignore), and the next start removes it, and nothing else in
# the directory. One stopped there by TERM removes it itself (its trap).
repo_root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
fresh_env
AUTODEPLOY_TEST_KILL_AT_ENV_MV=1 deploy_once >/dev/null 2>&1 || true
read -r _ staged staged_mode < <(grep '^mv-source ' "$argv" | head -n 1) ||
  fail 'UNKNOWN: the deployer reached no move onto the env file, so nothing of its write was judged'
[[ "$(dirname "$staged")" == "$compose" && "$(basename "$staged")" == .env.stage.?????? ]] ||
  fail "the deployer staged the env file's new contents at $staged, not $compose/.env.stage.XXXXXX"
[[ "$staged_mode" == 600 ]] || fail "the staging file's mode was $staged_mode, want 600"
[[ -f "$staged" ]] || fail 'UNKNOWN: a deployer killed by SIGKILL at its rename left no staging file, so the sweep was not judged'
git -C "$repo_root" check-ignore -q "packages/envoy/deploy/compose/$(basename "$staged")" ||
  fail "git does not ignore the staging file a killed deployer leaves ($(basename "$staged"))"
if git -C "$repo_root" check-ignore -q packages/envoy/deploy/compose/env.stage.keep; then
  fail 'git ignores more than the staging prefix (env.stage.keep)'
fi
printf 'keep\n' >"$compose/env.stage.keep"
printf 'keep\n' >"$compose/.env.staged"
mkdir -p "$compose/.env.stage.dir"
deploy_once >/dev/null 2>&1 || true
[[ ! -e "$staged" ]] || fail "the next start did not remove the staging file a killed deployer left: $staged"
[[ -f "$compose/env.stage.keep" && -f "$compose/.env.staged" && -d "$compose/.env.stage.dir" ]] ||
  fail 'the start removed a file or directory of COMPOSE_DIR other than a regular .env.stage.* file'
rm -rf "$compose/env.stage.keep" "$compose/.env.staged" "$compose/.env.stage.dir"
fresh_env
AUTODEPLOY_TEST_TERM_AT_ENV_MV=1 deploy_once >/dev/null 2>&1 || true
grep -q '^mv-source ' "$argv" || fail 'UNKNOWN: the deployer reached no move onto the env file under TERM'
left=$(find "$compose" -maxdepth 1 -name '.env.stage.*' | head -n 1)
[[ -z "$left" ]] || fail "a deployer stopped by TERM at its rename left its staging file: $left"

# With the schema already recorded for the serving tag, the deploy's first write of the env file is
# its tag's, after the new image serves; BACKUPS on another filesystem than the env file changes
# nothing for it, and the deploy lands with its tag persisted.
other_fs=$(mktemp -d /dev/shm/autodeploy-test.XXXXXX) || fail 'UNKNOWN: no /dev/shm to put BACKUPS on another filesystem'
[[ "$(stat -c %d "$other_fs")" != "$(stat -c %d "$compose")" ]] ||
  fail 'UNKNOWN: /dev/shm is on the compose directory'"'"'s filesystem here, so the case was not judged'
fresh_env
with_line ENVOY_IMAGE_SCHEMA 'ENVOY_IMAGE_SCHEMA=old-image:5'
good_deploy 'the schema recorded for the serving tag and BACKUPS on another filesystem' test_backups="$other_fs"
rm -rf "$other_fs"

# The deployer reads only through `docker compose config --format json`, so it runs on Compose
# releases that lack `config --environment` (before 2.28): a good deploy lands through Compose
# 2.12.2, the oldest release measured, the release binary pinned by its published sha256 and cached
# between runs. A wrapper is the `docker` that hands `compose ...` to it, and counts the calls, so
# the row cannot pass on another Compose.
compose_2_12=${XDG_CACHE_HOME:-$HOME/.cache}/legion-autodeploy-test/docker-compose-v2.12.2
compose_2_12_sha256=36d1728ce001c7f021294be43bdfa3f508038bb00886c34b0794f7731cc9bf4b
if [[ "$(sha256sum "$compose_2_12" 2>/dev/null | cut -d' ' -f1)" != "$compose_2_12_sha256" ]]; then
  mkdir -p "$(dirname "$compose_2_12")"
  curl -fsSL -o "$compose_2_12.part" https://github.com/docker/compose/releases/download/v2.12.2/docker-compose-linux-x86_64 ||
    fail 'UNKNOWN: could not download Compose 2.12.2, so the oldest release was not judged'
  [[ "$(sha256sum "$compose_2_12.part" | cut -d' ' -f1)" == "$compose_2_12_sha256" ]] ||
    fail 'UNKNOWN: the Compose 2.12.2 download does not match its published sha256'
  chmod +x "$compose_2_12.part"
  mv "$compose_2_12.part" "$compose_2_12"
fi
cat >"$root/docker-2.12" <<EOF
#!/usr/bin/env bash
[[ "\$1" == compose ]] || exec "$real_docker" "\$@"
shift
printf '%s\n' "\$*" >>"$root/compose-2.12.calls"
exec "$compose_2_12" "\$@"
EOF
chmod +x "$root/docker-2.12"
[[ "$("$root/docker-2.12" compose version --short)" == 2.12.2 ]] || fail 'UNKNOWN: the Compose 2.12.2 wrapper does not run 2.12.2'
: >"$root/compose-2.12.calls"
fresh_env
test_real_docker="$root/docker-2.12"
good_deploy 'a deploy through Compose 2.12.2'
unset test_real_docker
[[ $(grep -c ' config ' "$root/compose-2.12.calls") -ge 3 ]] ||
  fail "UNKNOWN: Compose 2.12.2 served $(grep -c ' config ' "$root/compose-2.12.calls") config reads, want the tag's, the settings' and the schema record's"

# A deploy that fails its origin check with the schema where it was stops the candidate, confirms
# it stopped, reads the schema, and rolls back. Every database client gets the URL without its
# password and the password as PGPASSWORD.
fresh_env
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment'
fi
no_secret 'the rollback'
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer did not restore the previous image tag after the origin check failed'
fi
if ! compgen -G "$backups/pre-new-image-*.dump" >/dev/null; then
  fail 'deployer did not write the pre-deploy database backup'
fi
if [[ $(<"$log") != *" pull "*$'\n'*" up "*$'\n'*" stop dispatch"$'\n'*" ps "*$'\n'*" up "* ]] || [[ $(wc -l <"$log") -ne 5 ]]; then
  fail 'deployer did not pull, deploy, stop, confirm the stop, and roll back after the origin check failed'
fi
if [[ "$output" != *"rolling back to old-image"* ]]; then
  fail 'deployer did not report the redirect-uri rollback'
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_SCHEMA=old-image:$(schema_value)"* ]]; then
  fail 'deployer did not record the set of migrations old-image runs against'
fi
if [[ $(grep -c "^pg_dump -Fc $url_without_password\$" "$argv") -ne 1 ]] ||
  [[ $(grep -c "$psql_run" "$argv") -ne 2 ]] || [[ $(grep -cF "$schema_try" "$argv") -ne 2 ]] ||
  [[ $(grep -cF "$schema_read" "$argv") -ne 2 ]]; then
  fail 'deployer did not run pg_dump once and psql twice (before the deploy and after the stop) on the URL without its password, each run the try for the runner key and then the read'
fi
# Each client container runs under an init that passes on a signal, so `timeout` ends it.
if [[ $(grep -c '^docker run ' "$argv") -ne 3 || $(grep -c '^docker run --init ' "$argv") -ne 3 ]]; then
  fail 'deployer ran a database client container without --init, which a timeout could not stop'
fi

# A new image that serves with its NATS connection failing (`nats` false) fails the health check
# and is rolled back, as when Dispatch exited at boot without NATS; one with NATS turned off
# (`nats` null) passes it and goes on to the origin check.
fresh_env
if output=$(AUTODEPLOY_TEST_CANDIDATE_NATS=false deploy_once); then
  fail 'deployer kept an image whose NATS connection failed'
fi
if [[ "$output" != *"did not answer 200 with NATS connected"*"rolling back to old-image"* ]] ||
  [[ "$output" == *"OAuth redirect_uri"* ]] || [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer did not roll back an image whose /healthz reported nats false'
fi
fresh_env
if output=$(AUTODEPLOY_TEST_CANDIDATE_NATS=null deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment with NATS turned off'
fi
if [[ "$output" != *"OAuth redirect_uri"*"rolling back to old-image"* ]] || [[ "$output" == *"NATS connected"* ]]; then
  fail 'deployer did not pass the health check of an image with NATS turned off (nats null)'
fi

# A deploy whose image moved the schema before it failed refuses to start the previous image
# against that schema: it stops the new one, names both versions, and fails.
fresh_env
if output=$(AUTODEPLOY_TEST_MIGRATE_TO=113 deploy_once); then
  fail 'deployer did not fail after a deploy that moved the schema failed its origin check'
fi
no_secret 'the refusal'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112 (112 applied)"*"113 (113 applied)"*"forward recovery is required"* ]]; then
  fail 'deployer did not refuse the rollback naming both schema versions'
fi
if [[ $(grep -c ' up ' "$log") -ne 1 || $(grep -c ' stop dispatch' "$log") -ne 1 ]]; then
  fail 'deployer did not deploy once and stop the new image once, without rolling back'
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer persisted the failed image tag after refusing its rollback'
fi

# The next pass retries the same image. The schema is already at 113 before it starts, and the
# rollback is still refused: old-image runs against 112, as recorded on the first attempt.
: >"$log"
if output=$(deploy_once); then
  fail 'deployer did not fail on the retry of an image whose rollback it refused'
fi
no_secret 'the retry'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112 (112 applied)"*"113 (113 applied)"* ]] || [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
  fail 'deployer rolled back to old-image against schema 113 on the retry'
fi

# A migration the failed image held open commits only as the image stops. The deployer reads the
# schema after the stop, so it sees 113 and refuses; a read before the stop would see 112.
fresh_env
if output=$(AUTODEPLOY_TEST_MIGRATE_ON_STOP=113 deploy_once); then
  fail 'deployer did not fail when the failed image migrated as it stopped'
fi
no_secret 'the migration committing as the image stops'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112 (112 applied)"*"113 (113 applied)"* ]] || [[ $(grep -c ' up ' "$log") -ne 1 ]] ||
  [[ $(schema_value) != 113-113-* ]]; then
  fail 'deployer rolled back to old-image while the failed image held a migration open'
fi

# An out-of-order migration: the failed image applied a version below the highest one already
# applied, a numbered migration under a timestamped one, so max(version) is where it was and only
# the set changed. The deployer compares the set, so it refuses this rollback too.
fresh_env
printf '%s\n' 20260115123000 >>"$schema"
if output=$(AUTODEPLOY_TEST_MIGRATE_TO=113 deploy_once); then
  fail 'deployer did not fail after a deploy that applied an out-of-order migration failed its origin check'
fi
no_secret 'the out-of-order refusal'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 20260115123000 (113 applied)"*"20260115123000 (114 applied)"*"forward recovery is required"* ]]; then
  fail 'deployer did not refuse the rollback after a migration applied below the highest one left max(version) where it was'
fi
if [[ $(grep -c ' up ' "$log") -ne 1 || $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer started old-image against a schema holding a migration old-image never ran'
fi

# A compose/.env the deployer before this one wrote records <tag>:<max(version)>, with no set
# behind it. While that tag is still deployed it is compared the old way, so a retry in flight
# across the upgrade keeps the safety it started with: a schema whose max has moved refuses, and
# the record is left as it is.
fresh_env
printf 'ENVOY_IMAGE_SCHEMA=old-image:112\n' >>"$compose/.env"
if output=$(AUTODEPLOY_TEST_MIGRATE_TO=113 deploy_once); then
  fail 'deployer did not fail on an old-form record after a deploy that moved the schema failed'
fi
no_secret 'the old-form record'
if [[ "$output" != *"refusing to roll back to old-image"*"schema 112"*"113 (113 applied)"*"forward recovery is required"* ]] ||
  [[ $(<"$compose/.env") != *"ENVOY_IMAGE_SCHEMA=old-image:112"* ]]; then
  fail 'deployer did not refuse the rollback against an old-form ENVOY_IMAGE_SCHEMA record, or rewrote it'
fi
# The same record with the schema where old-image left it rolls back.
fresh_env
printf 'ENVOY_IMAGE_SCHEMA=old-image:112\n' >>"$compose/.env"
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment on an old-form record'
fi
no_secret 'the old-form record with the schema unmoved'
if [[ "$output" != *"rolling back to old-image"* ]] || [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer did not roll back on an old-form record with the schema where old-image left it'
fi

# A stop that fails, or that leaves the candidate running, proves nothing about the schema.
for stop in AUTODEPLOY_TEST_STOP_FAIL AUTODEPLOY_TEST_STOP_LEAVES_RUNNING; do
  fresh_env
  if output=$(export "$stop=1" && deploy_once); then
    fail "deployer did not fail when $stop"
  fi
  no_secret "the refused stop ($stop)"
  if [[ "$output" != *"refusing to roll back to old-image"* ]] || [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
    fail "deployer rolled back without a confirmed stop ($stop)"
  fi
done

# A schema version it cannot read after the stop is no proof the schema stayed: it refuses.
fresh_env
if output=$(AUTODEPLOY_TEST_SCHEMA_UNREADABLE_AFTER_UP=1 deploy_once); then
  fail 'deployer did not fail when it could not read the schema version after a failed deploy'
fi
no_secret 'the unreadable schema'
if [[ "$output" != *"could not read the schema version"*"status 2"*"refusing to roll back to old-image"* ]] ||
  [[ $(grep -c ' up ' "$log") -ne 1 ]]; then
  fail 'deployer rolled back without knowing the schema version'
fi

# A schema version it cannot read before a deploy stops the deploy before anything changes.
fresh_env
if output=$(AUTODEPLOY_TEST_SCHEMA_UNREADABLE=1 deploy_once); then
  fail 'deployer deployed without reading the schema version first'
fi
no_secret 'the unreadable schema before the deploy'
if [[ "$output" != *"could not read the schema version"*"not deploying new-image"* ]] || [[ -s "$log" ]]; then
  fail 'deployer did not stop before deploying when the schema version was unreadable'
fi

# A key another session holds does not make the read wait on it: a blocking wait deadlocks against
# a migration runner building an index concurrently under that key, so the read tries the key,
# reads nothing while it is held, and tries again. Here the key is held against the first two
# tries; the third takes it, and the deploy runs as it does with a free key.
fresh_env
printf '2\n' >"$schema.key"
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment behind a held key'
fi
no_secret 'the held migration key'
# While the key is held the read must not run at all, not merely be dropped from the answer: the
# fake fails such a read, as the server can, and under ON_ERROR_STOP=1 psql's non-zero status
# would end the deployer's read rather than its poll. So of the four runs only the two granted
# the key read anything: the third try, which took it, and the one after the stop.
if [[ $(grep -cF "$schema_read" "$argv") -ne 2 ]] || [[ "$output" == *"could not read the schema version"* ]]; then
  fail 'deployer read the applied set while another session held the migration key, so a failing read ended its poll'
fi
if [[ $(<"$schema.key") -ne 0 ]]; then
  fail 'deployer did not try the held migration key more than once'
fi
# Four psql runs, each trying the key: it refused the first two, the third took it and read the
# set, and one more after the stop. An f answer taken for the applied set would record 0 versions
# and refuse the rollback.
if [[ $(grep -c "$psql_run" "$argv") -ne 4 ]] || [[ $(grep -cF "$schema_try" "$argv") -ne 4 ]] ||
  [[ "$output" != *"rolling back to old-image"* ]] ||
  [[ "$output" == *"refusing to roll back"* ]] || [[ $(<"$compose/.env") != *"ENVOY_IMAGE_SCHEMA=old-image:$(schema_value)"* ]]; then
  fail 'deployer did not poll the held migration key and deploy once it came free'
fi
# Nothing in the run waits on the key inside the database, in a statement run or in an argument.
if grep -qF 'select pg_advisory_lock(' "$argv"; then
  fail 'deployer waited on the migration key in a blocking statement, which deadlocks the migration runner'
fi

# A key no session ever lets go of, as a pod killed mid-CREATE INDEX CONCURRENTLY leaves behind:
# the backend goes on building the index, and holds the key for minutes. The deployer tries the
# key a second apart for its whole deadline, never reads behind it, and then refuses: a schema
# version it cannot read is no ground to deploy on. (The deployer's waits are the fake sleep
# above, which returns at once, so its 60 tries take no time here.)
fresh_env
printf 'always\n' >"$schema.key"
if output=$(deploy_once); then
  fail 'deployer deployed without reading the schema version behind a key no session released'
fi
no_secret 'the migration key held past the deadline'
# The whole poll, not a first try taken for an answer: 60 tries, one per psql run, and no read in
# any of them. A deployer that gave up on the first refusal would try once.
if [[ $(grep -cF "$schema_try" "$argv") -ne 60 ]] || [[ $(grep -c "$psql_run" "$argv") -ne 60 ]]; then
  fail 'deployer did not poll the migration key through its deadline before giving up'
fi
if grep -qF "$schema_read" "$argv"; then
  fail 'deployer read the applied set while another session held the migration key'
fi
if [[ "$output" != *"could not read the schema version"*"not deploying new-image"* ]]; then
  fail 'deployer did not report the unreadable schema version behind the held key and stop'
fi
# Nothing of the deploy ran: no pg_dump, which is how the pre-deploy backup is taken, and no
# compose, so the previous image still serves. ($backups holds the dumps of the cases above, so
# what this reads is the argument log of this run, which fresh_env emptied.)
if [[ -s "$log" ]] || grep -q '^pg_dump ' "$argv" ||
  [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer deployed, or backed up for a deploy, behind a migration key it could not take'
fi

# What ends that poll is the deadline, and the count of tries is only the bound behind it, for a
# run whose sleep returns early. With a real second between two tries the deadline comes first: a
# three-second deadline holds three or four tries, nowhere near the sixty the counter allows. A
# poll that had lost its deadline would run all sixty here, over a minute, and the count below is
# what says which of the two bounds ended this one.
fresh_env
printf 'always\n' >"$schema.key"
if output=$(SCHEMA_READ_DEADLINE=3 AUTODEPLOY_TEST_SLEEP_REAL=1 deploy_once); then
  fail 'deployer deployed behind a key no session released under a three-second deadline'
fi
no_secret 'the poll its deadline ended'
tries=$(grep -cF "$schema_try" "$argv" || true)
if ((tries < 2 || tries >= 10)); then
  fail "deployer made $tries tries for the migration key under a three-second deadline, a real second apart: the count of tries ended its poll, not the deadline"
fi
if grep -qF "$schema_read" "$argv"; then
  fail 'deployer read the applied set while another session held the migration key'
fi
if [[ "$output" != *"could not read the schema version"*"not deploying new-image"* ]] || [[ -s "$log" ]]; then
  fail 'deployer did not report the unreadable schema version and stop once its deadline had passed'
fi

# Each try is bounded by what the deadline leaves, never by a fresh minute of its own: a try that
# started a second before the deadline would otherwise end a minute past it, while the script's
# comments and store.go promise a read that ends inside the deadline. The timeout stand-in
# recorded the bound every client container was given, so this reads those bounds back.
fresh_env
printf 'always\n' >"$schema.key"
if output=$(SCHEMA_READ_DEADLINE=3 AUTODEPLOY_TEST_SLEEP_REAL=1 deploy_once); then
  fail 'deployer deployed behind a key no session released under a three-second deadline'
fi
no_secret 'the bounds the tries were given'
bounds=$(grep -o '^timeout [0-9]* docker run ' "$argv" | awk '{print $2}' || true)
if [[ -z "$bounds" ]]; then
  fail 'no container run of the schema read recorded the bound it was given'
fi
if [[ $(printf '%s\n' "$bounds" | wc -l) -ne $(grep -cF "$schema_try" "$argv") ]]; then
  fail 'the bounds recorded are not one for each try for the migration key'
fi
while IFS= read -r bound; do
  if ((bound < 1 || bound > 3)); then
    fail "a try for the migration key was given $bound seconds under a three-second deadline: 0 is unbounded to pg_client, and anything past the deadline is a fresh timeout that outlives it"
  fi
done <<<"$bounds"
# And the default deadline is what it was for an operator: against a free key the first try still
# gets the whole minute, and no try gets more.
fresh_env
if output=$(deploy_once); then
  fail 'deployer did not fail after the origin check rejected the deployment it read the schema for'
fi
first=$(grep -o '^timeout [0-9]* docker run ' "$argv" | awk 'NR == 1 {print $2}' || true)
if ((first < 1 || first > 60)); then
  fail "the first try for the migration key was given $first seconds, not the minute the default deadline leaves it"
fi

# Both URL loops below run under en_US.UTF-8, where bash's [[ =~ ]] ranges such as [a-z] and
# [0-9] also match non-ASCII characters, so a grammar that left the locale in force would accept
# what it should refuse. The control needs that locale installed and widening the ranges.
if ! LC_ALL=en_US.UTF-8 bash -c '[[ "ä" =~ ^[a-z]$ ]]' 2>/dev/null; then
  fail 'en_US.UTF-8 is not installed here (sudo locale-gen en_US.UTF-8), so the URL loops would not test the locale'
fi

# The deployer accepts one URL shape and refuses every other before any client starts, so no
# password in a refused URL can reach a process's arguments, and the refusal never prints the URL.
# The cases cover each way out of the shape:
#   - the password: a malformed escape, %00, an escape that decodes to bytes that are not UTF-8, a
#     raw ? / @ or space, raw non-ASCII, and a double-byte character whose second byte is @;
#   - the query: a password under any spelling (password, sslpassword, an encoded key, a space
#     beside the key), a key outside the list, an @, and a non-ASCII value;
#   - the user, host and port: a non-ASCII user, a bracketed, IPv6, multi-host or socket host, an
#     empty or hyphen-edged host label, a port of 0, above 65535 or not ASCII digits;
#   - an @ in the path, and a keyword/value string.
# Most are URLs pgx or libpq would connect with; the fake is given the password libpq would read,
# so a deployer that let one through would show it in some client's arguments.
for case in \
  'review-secret|postgres://dispatch:review-secret%oops@db.test:5432/dispatch' \
  's|postgres://dispatch:s%00cret-canary@db.test:5432/dispatch' \
  'x|postgres://dispatch:s%FFcret-canary@db.test:5432/dispatch' \
  's?cret-canary|postgres://dispatch:s?cret-canary@db.test:5432/dispatch?sslmode=require' \
  's/cret-canary|postgres://dispatch:s/cret-canary@db.test:5432/dispatch' \
  's|postgres://dispatch:s@cret-canary@db.test:5432/dispatch' \
  's cret-canary|postgres://dispatch:s cret-canary@db.test:5432/dispatch' \
  'pässwörd-cret-canary|postgres://dispatch:pässwörd-cret-canary@db.test:5432/dispatch' \
  $'x|postgres://dispatch:pw\xa9@cret-canary@db.test:5432/dispatch' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?sslmode=require&password=review-secret' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?sslpassword=review-secret' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?pass%77ord=review-secret' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?sslmode=require& password=review-secret' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?password =review-secret' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?sslmode=require&sslpassword =review-secret' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:5432/dispatch?target_session_attrs=any' \
  'review-secret|postgres://dispatch@db.test:5432/dispatch?application_name=a@b&password=review-secret' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:5432/dispatch?application_name=ｄｅｐｌｏｙ' \
  's@cret-canary|postgres://dispätch:s%40cret-canary@db.test:5432/dispatch' \
  'review-secret|postgres://dispatch@[::1?x]:5432,db.test:5432/dispatch?password=review-secret&sslmode=require' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@[::1]:5432/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db1.test:5432,db2.test:5432/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@%2Fvar%2Frun%2Fpostgresql/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db..test:5432/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@-db.test:5432/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:0/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:65536/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:5²/dispatch' \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:5432/dis@patch' \
  'review-secret|host=db.test user=dispatch password=review-secret dbname=dispatch'; do
  fresh_env "${case#*|}"
  if output=$(LC_ALL=en_US.UTF-8 test_password="${case%%|*}" deploy_once); then
    fail "deployer deployed with ${case#*|}"
  fi
  no_secret "${case#*|}"
  if [[ "$output" != *"DATABASE_URL"*"not deploying new-image"* ]] || [[ "$output" == *"${case#*|}"* ]] ||
    [[ -s "$log" || -s "$argv" ]]; then
    fail "deployer did not refuse ${case#*|} before running anything, without printing it"
  fi
done

# The accepted shape splits as intended: each allowed query key, the highest port, a URL with no
# port, escapes that decode to reserved characters or to multi-byte UTF-8, and a URL with no
# password at all. The clients get the URL without its password as their last argument, the
# password reaches the fake libpq only as PGPASSWORD, and the deploy goes on (here to the origin
# check's rollback).
for case in \
  's@cret-canary|postgres://dispatch:s%40cret-canary@db.test:5432/dispatch?sslmode=require&connect_timeout=10&application_name=dispatch-deploy|postgres://dispatch@db.test:5432/dispatch?sslmode=require&connect_timeout=10&application_name=dispatch-deploy' \
  'Az09._~-:/?#[] @|postgresql://dispatch.ro:Az09._~-%3A%2F%3F%23%5B%5D%20%40@db-1.internal.example/dispatch_prod|postgresql://dispatch.ro@db-1.internal.example/dispatch_prod' \
  'süß-canary|postgres://dispatch:s%C3%BC%C3%9F-canary@10.0.0.7:65535/dispatch|postgres://dispatch@10.0.0.7:65535/dispatch' \
  '|postgres://dispatch@db.test/dispatch|postgres://dispatch@db.test/dispatch'; do
  IFS='|' read -r case_password case_url case_client_url <<<"$case"
  fresh_env "$case_url"
  if output=$(LC_ALL=en_US.UTF-8 test_password="$case_password" deploy_once); then
    fail "deployer did not reach the origin check with $case_url"
  fi
  no_secret "$case_url"
  if [[ "$output" != *"rolling back to old-image"* ]] ||
    [[ $(awk -v url="$case_client_url" '$NF == url' "$argv" | wc -l) -ne 3 ]] ||
    { [[ -n "$case_password" ]] && grep -qF -- "$case_password" "$argv"; }; then
    fail "deployer did not hand the clients $case_client_url, with the password as PGPASSWORD alone"
  fi
done

# Compose prints a value's `$` doubled in `config --format json`; the deployer halves it back. An
# origin written `x$$y` in the .env, which Compose reads as `x$y`, is the origin the login check
# expects, with one `$`.
fresh_env
# shellcheck disable=SC2016
with_line DISPATCH_SERVER_URL 'DISPATCH_SERVER_URL=http://browser.test:8766/x$$y'
output=$(deploy_once) || true
[[ "$output" == *"expected 'http://browser.test:8766/x\$y/auth/callback'"* ]] ||
  fail 'deployer did not read a $ in DISPATCH_SERVER_URL as Compose gives it to the container'

# A failed pull changes nothing: the previous image still serves, so there is nothing to stop or
# restart, and the next pass tries the pull again.
fresh_env
for _ in 1 2; do
  if output=$(AUTODEPLOY_TEST_PULL_FAIL=1 deploy_once); then
    fail 'deployer did not fail when the image pull failed'
  fi
done
no_secret 'the failed pull'
if [[ $(grep -c ' pull ' "$log") -ne 2 || $(wc -l <"$log") -ne 2 ]]; then
  fail 'deployer did not retry the failed image pull on the next --once pass, or touched the running service'
fi
if [[ $(<"$compose/.env") != *"ENVOY_IMAGE_TAG=old-image"* ]]; then
  fail 'deployer persisted the failed image tag after a pull failure'
fi
printf '%s\n' 'autodeploy --once rollback, schema-set refusal (out-of-order migration and old-form record included), stop-first, migration-key polling, credential and pull-retry cases: PASS'
