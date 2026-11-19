# shellcheck shell=bash disable=SC2034 # the case groups beside this file read what it sets
# The deployer suite's fixture, which autodeploy_test.sh sources before the case groups beside
# it: a scratch tree, the stand-ins the deployer runs on its PATH (docker, jj, curl, date, sleep,
# timeout, and psql and pg_dump inside the database clients' container), and the helpers each case
# reads its result with. Every case begins with case_begin and fresh_env.

root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
# The repository the walk and the order reads run against: a real jj repository (repo_init),
# which only the order reads reach.
repo="$root/repo"
compose="$root/compose"
backups="$root/backups"
# The deployer's record of failed deploys (FAILED_DEPLOY), under BACKUPS.
failed_record="$backups/failed-deploy"
bin="$root/bin"
tools="$root/tools"
log="$root/compose.log"
argv="$root/argv.log"
schema="$root/schema"
# The registry's answers to the deployer's manifest reads (the docker stand-in below says how they
# are written), and the tag of every read it made, in order.
manifests="$root/manifests"
manifest_log="$root/manifest.log"
# The bound each call the deployer makes through `timeout` is given, one `<seconds> <clock> <call>`
# line each (the timeout stand-in below), where <clock> is the pass's clock when the call was made;
# and the clock itself: the seconds it has moved on from clock_base, which no real time moves (the
# date stand-in below). So a bound a case reads is exact, whatever the box's load.
bounds_log="$root/bounds"
clock="$root/clock"
clock_base=2000000000
# The deployer under test: the script beside this directory, or AUTODEPLOY_SUITE_DEPLOYER, a copy
# a check of the suite itself runs it against (a mutant, say).
deployer_script=${AUTODEPLOY_SUITE_DEPLOYER:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/autodeploy.sh}
# The real timeout, date and jj, which the stand-ins below hand calls to.
real_timeout=$(command -v timeout)
real_date=$(command -v date)
if ! real_jj=$(command -v jj); then
  printf '%s\n' 'jj is not installed here (.github/actions/install-jj installs it in CI): the order reads run the real jj' >&2
  exit 1
fi
mkdir -p "$compose" "$backups" "$bin" "$tools"
# The fixture makes its credential and URL at run time. The source carries neither a password
# assignment nor a URL with userinfo, so the public-source check has no credential-like test data.
password='s@cret-'canary
encoded_password=${password/@/%40}
url_user='postgres://dispatch'
default_database_target='db.test:5432/dispatch'
url_with_credential() {
  printf '%s:%s@%s' "$url_user" "$1" "$2"
}
url_without_credential() {
  printf '%s@%s' "$url_user" "$1"
}
url_without_password="$(url_without_credential "$default_database_target")?sslmode=require"
# The schema read, as it reaches psql: the connection on the command line, and on standard input
# a script that tries for the migration key, reads the versions only once the try was granted, and
# reads the key's holder where it was not. The fake psql below reads that script on standard input,
# refuses a run that sends none, and records every statement it actually runs as a psql-sql line of
# the argv log, so the test counts the reads apart from the runs.
psql_run="^psql .*$url_without_password\$"
schema_try='psql-sql select pg_try_advisory_lock(8150001)'
schema_read='psql-sql select version from schema_migrations order by version'
holder_read='psql-sql select coalesce(string_agg('

# The jj configuration every jj of the suite reads, the deployer's included, so no setting of the
# box's own (a signing key, say) reaches the repository.
jj_config="$root/jj-config.toml"
printf '%s\n' '[user]' 'name = "autodeploy test"' 'email = "autodeploy-test@example.test"' >"$jj_config"
# repo_init makes $repo a jj repository of three commits, each a bookmark the cases name as a tag:
# old-image, its child new-image, and its grandchild newer-image.
repo_init() {
  local jj=(env JJ_CONFIG="$jj_config" "$real_jj" --no-pager)
  "${jj[@]}" git init "$repo" >/dev/null 2>&1
  local tag
  for tag in old-image new-image newer-image; do
    "${jj[@]}" -R "$repo" describe -m "$tag" >/dev/null 2>&1
    "${jj[@]}" -R "$repo" bookmark create "$tag" -r @ >/dev/null 2>&1
    "${jj[@]}" -R "$repo" new >/dev/null 2>&1
  done
}
repo_init

fresh_env() {
  cat >"$compose/.env" <<EOF
ENVOY_IMAGE_TAG=old-image
DATABASE_URL=${1:-"$(url_with_credential "$encoded_password" "$default_database_target")?sslmode=require"}
DISPATCH_SERVER_URL=http://browser.test:8766
EOF
  # The versions schema_migrations holds, one per line, as the deployer reads them: a set, not a
  # level. A migration applied below the highest one changes the set and leaves max(version) where
  # it was.
  seq 1 112 >"$schema"
  rm -f "$schema.up" "$schema.running" "$schema.candidate" "$schema.held" "$schema.key" "$schema.refused" "$schema.pulled" \
    "$schema.passes" "$manifests" "$manifests.hung" "$failed_record"
  touch "$schema.running"
  : >"$log"
  : >"$argv"
  : >"$manifest_log"
  : >"$bounds_log"
  printf '0\n' >"$clock"
  rm -f "$clock.calls"
}
# deployed_tag_is <tag>: the tag compose/.env says is deployed.
deployed_tag_is() {
  sed -i -E "s/^ENVOY_IMAGE_TAG=.*/ENVOY_IMAGE_TAG=$1/" "$compose/.env"
}

# jj answers the walk's list of commits from AUTODEPLOY_TEST_COMMITS, newest first, and hands the
# deployer's order read, the one whose template asks contained_in, to the real jj, against $repo:
# what the order of two commits is, and how the deployer's own revset and template read it, is
# real jj's answer, never this stand-in's. AUTODEPLOY_TEST_ORDER_FAILS=1 makes it answer no order.
# A `git fetch` answers at once; with AUTODEPLOY_TEST_FETCH_FAILS=1 it fails, and with
# AUTODEPLOY_TEST_FETCH_HANG=1 it never answers.
cat >"$bin/jj" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *" git fetch"*)
    [[ "${AUTODEPLOY_TEST_FETCH_FAILS:-}" != 1 ]] || exit 1
    [[ "${AUTODEPLOY_TEST_FETCH_HANG:-}" != 1 ]] || exec /bin/sleep 3600
    ;;
  *"ancestors(main@origin, 30)"*) printf '%s\n' ${AUTODEPLOY_TEST_COMMITS:-new-image} ;;
  *"description.first_line()"*) printf 'test deployment\n' ;;
  *"contained_in"*)
    [[ "${AUTODEPLOY_TEST_ORDER_FAILS:-}" != 1 ]] || exit 1
    exec "$AUTODEPLOY_TEST_REAL_JJ" --no-pager "$@"
    ;;
  *)
    printf 'unexpected jj invocation: %s\n' "$*" >&2
    exit 1
    ;;
esac
EOF
# date answers `date +%s`, the pass's clock, as clock_base plus the seconds $AUTODEPLOY_TEST_CLOCK
# holds, which the stand-ins below move on to stand for time a call or a wait took: no real time
# moves it, so every bound the deployer computes from it is the same on a loaded box as on an idle
# one. Each answer is recorded in $AUTODEPLOY_TEST_CLOCK.calls. A clock file holding `garbled-once`
# makes the first answer `300s`, which is no time in seconds, and every later one the clock at 0;
# one holding `broken` makes date fail, printing nothing. Every other date it answers as the real
# one does.
cat >"$bin/date" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == "+%s" ]]; then
  moved=$(<"$AUTODEPLOY_TEST_CLOCK")
  if [[ "$moved" == garbled-once ]]; then
    printf '0\n' >"$AUTODEPLOY_TEST_CLOCK"
    printf '300s\n' | tee -a "$AUTODEPLOY_TEST_CLOCK.calls"
    exit 0
  fi
  if [[ "$moved" == broken ]]; then
    printf 'broken\n' >>"$AUTODEPLOY_TEST_CLOCK.calls"
    printf 'date: cannot read the clock\n' >&2
    exit 1
  fi
  printf '%s\n' "$((AUTODEPLOY_TEST_CLOCK_BASE + moved))" | tee -a "$AUTODEPLOY_TEST_CLOCK.calls"
  exit 0
fi
exec "$AUTODEPLOY_TEST_REAL_DATE" "$@"
EOF
# docker runs a container's command for real, as the container would: in an environment holding
# only the variables passed by name with -e, and with the fake psql and pg_dump below on its PATH.
# Every process it starts records its arguments, so the test reads what a host's
# /proc/<pid>/cmdline would show.
#
# A manifest read records its tag in $AUTODEPLOY_TEST_MANIFEST_LOG and answers what a
# `<tag> <answer>` line of $AUTODEPLOY_TEST_MANIFESTS says for that tag: `late:<s>`, the manifest
# after <s> real seconds; `clock:<s>`, the manifest after the pass's clock moved on <s> seconds;
# `unknown`, the registry's answer for a tag it does not hold, as ghcr.io words it;
# `unknown-after:<s>` and `unknown-clock:<s>`, that answer after <s> real seconds, or after the
# clock moved on <s>; `refused`, a rate limit's refusal; `refused-once`, that refusal to the first
# read alone and the manifest to every later one; and `hang`, no answer at all, as a read whose pid
# and start time it records in $AUTODEPLOY_TEST_MANIFESTS.hung. A tag with no line is published, at
# once. A wait ends its sleep when the read is stopped, so a stopped read leaves no process of the
# stand-in's behind.
#
# `compose up` with ENVOY_IMAGE_TAG set starts that tag as the candidate, which it records in
# $AUTODEPLOY_TEST_SCHEMA.candidate; an `up` without it, the rollback's, starts the deployed tag
# again. A container run (the database clients) never answers with
# AUTODEPLOY_TEST_RUN_HANG_AFTER_UP=1 once a candidate was started; a pull fails with
# AUTODEPLOY_TEST_PULL_FAIL=1, the first pull alone with AUTODEPLOY_TEST_PULL_FAIL_ONCE=1, and never
# answers with AUTODEPLOY_TEST_PULL_HANG=1; a stop never answers with AUTODEPLOY_TEST_STOP_HANG=1.
# With AUTODEPLOY_TEST_KEY_HELD_AFTER_STOP=<n>, the stop leaves the migration key held for <n> more
# tries (`always`: for every try), as a migration the stopped image sent still runs.
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
advance() {
  printf '%s\n' "$(($(<"$AUTODEPLOY_TEST_CLOCK") + $1))" >"$AUTODEPLOY_TEST_CLOCK"
}
case "$1" in
  manifest)
    tag=${3##*:}
    printf '%s\n' "$tag" >>"$AUTODEPLOY_TEST_MANIFEST_LOG"
    answer=$(sed -n "s/^$tag //p" "$AUTODEPLOY_TEST_MANIFESTS" 2>/dev/null || true)
    pause() {
      /bin/sleep "$1" &
      sleeper=$!
      trap 'kill "$sleeper"; exit 143' TERM
      wait "$sleeper"
    }
    case "$answer" in
      '') exit 0 ;;
      late:*) pause "${answer#late:}"; exit 0 ;;
      clock:*) advance "${answer#clock:}"; exit 0 ;;
      unknown) printf 'manifest unknown\n' >&2; exit 1 ;;
      unknown-after:*) pause "${answer#unknown-after:}"; printf 'manifest unknown\n' >&2; exit 1 ;;
      unknown-clock:*) advance "${answer#unknown-clock:}"; printf 'manifest unknown\n' >&2; exit 1 ;;
      refused) printf 'toomanyrequests: retry later\n' >&2; exit 1 ;;
      refused-once)
        [[ -e "$AUTODEPLOY_TEST_SCHEMA.refused" ]] && exit 0
        touch "$AUTODEPLOY_TEST_SCHEMA.refused"
        printf 'toomanyrequests: retry later\n' >&2
        exit 1
        ;;
      hang)
        printf '%s %s\n' "$$" "$(cut -d' ' -f22 "/proc/$$/stat")" >>"$AUTODEPLOY_TEST_MANIFESTS.hung"
        exec /bin/sleep 3600
        ;;
      *) printf 'unexpected manifest answer: %s\n' "$answer" >&2; exit 1 ;;
    esac
    ;;
  run)
    printf 'docker %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
    if [[ "${AUTODEPLOY_TEST_RUN_HANG_AFTER_UP:-}" == 1 && -e "$AUTODEPLOY_TEST_SCHEMA.up" ]]; then
      exec /bin/sleep 3600
    fi
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
    printf '%s\n' "$*" >>"$AUTODEPLOY_TEST_LOG"
    case "$*" in
      *" pull "*)
        if [[ "${AUTODEPLOY_TEST_PULL_FAIL:-}" == 1 ]]; then
          exit 1
        fi
        if [[ "${AUTODEPLOY_TEST_PULL_FAIL_ONCE:-}" == 1 && ! -e "$AUTODEPLOY_TEST_SCHEMA.pulled" ]]; then
          touch "$AUTODEPLOY_TEST_SCHEMA.pulled"
          exit 1
        fi
        if [[ "${AUTODEPLOY_TEST_PULL_HANG:-}" == 1 ]]; then
          exec /bin/sleep 3600
        fi
        ;;
      *" up "*)
        if [[ -n "${ENVOY_IMAGE_TAG:-}" ]]; then
          touch "$AUTODEPLOY_TEST_SCHEMA.up"
          printf '%s\n' "$ENVOY_IMAGE_TAG" >"$AUTODEPLOY_TEST_SCHEMA.candidate"
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
        if [[ "${AUTODEPLOY_TEST_STOP_HANG:-}" == 1 ]]; then
          exec /bin/sleep 3600
        fi
        held_migration_commits
        if [[ -n "${AUTODEPLOY_TEST_KEY_HELD_AFTER_STOP:-}" ]]; then
          printf '%s\n' "$AUTODEPLOY_TEST_KEY_HELD_AFTER_STOP" >"$AUTODEPLOY_TEST_SCHEMA.key"
        fi
        if [[ "${AUTODEPLOY_TEST_CLOCK_BREAKS_ON_STOP:-}" == 1 ]]; then
          printf 'broken\n' >"$AUTODEPLOY_TEST_CLOCK"
        fi
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
# curl answers /healthz 200 with NATS connected, but for a candidate with
# AUTODEPLOY_TEST_CANDIDATE_NATS set, which it reports as `nats`. A candidate's /healthz takes the
# seconds AUTODEPLOY_TEST_HEALTH_TAKES gives of the pass's clock, and one that takes its --max-time
# or more is ended at it, as curl ends it (exit 28). /auth/start redirects to the wrong origin,
# which fails the deploy's origin check, unless AUTODEPLOY_TEST_ORIGIN_OK=1. Every request records
# the bound it was given (--max-time) in the bounds log, as `curl <path>`.
cat >"$bin/curl" <<'EOF'
#!/usr/bin/env bash
max=
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  [[ "${args[i]}" == --max-time ]] && max=${args[i + 1]}
done
path=other
case "$*" in
  *"/healthz"*) path=/healthz ;;
  *"/auth/start"*) path=/auth/start ;;
esac
printf '%s %s curl %s\n' "${max:-none}" "$(<"$AUTODEPLOY_TEST_CLOCK")" "$path" >>"$AUTODEPLOY_TEST_BOUNDS"
takes=${AUTODEPLOY_TEST_HEALTH_TAKES:-}
if [[ "$path" == /healthz && -n "$takes" && -e "$AUTODEPLOY_TEST_SCHEMA.candidate" ]]; then
  if [[ -n "$max" ]] && ((takes >= max)); then
    printf '%s\n' "$(($(<"$AUTODEPLOY_TEST_CLOCK") + max))" >"$AUTODEPLOY_TEST_CLOCK"
    exit 28
  fi
  printf '%s\n' "$(($(<"$AUTODEPLOY_TEST_CLOCK") + takes))" >"$AUTODEPLOY_TEST_CLOCK"
fi
case "$path" in
  /healthz)
    nats=true
    if [[ -e "$AUTODEPLOY_TEST_SCHEMA.candidate" ]]; then
      nats="${AUTODEPLOY_TEST_CANDIDATE_NATS:-true}"
    fi
    printf '{"ok":true,"db":true,"nats":%s}' "$nats"
    ;;
  /auth/start)
    host=wrong.test
    [[ "${AUTODEPLOY_TEST_ORIGIN_OK:-}" == 1 ]] && host=browser.test
    printf 'https://github.com/login/oauth/authorize?redirect_uri=http%%3A%%2F%%2F%s%%3A8766%%2Fauth%%2Fcallback' "$host"
    ;;
esac
EOF
# sleep returns at once and moves the pass's clock on by the seconds it was asked for, so a wait
# of the deployer's (the health check's, the schema read's poll) counts against the pass's
# deadline as it would, and no case waits it out. A case of the continuous loop sets
# AUTODEPLOY_TEST_LOOP_PASSES=<n>: the loop's own wait between two passes (a sleep of POLL) is
# counted, and the n-th ends the loop with SIGTERM, as its supervisor's stop would, after its n-th
# pass.
cat >"$bin/sleep" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${AUTODEPLOY_TEST_LOOP_PASSES:-}" && "$*" == "${POLL:-}" ]]; then
  printf 'pass\n' >>"$AUTODEPLOY_TEST_SCHEMA.passes"
  if (($(wc -l <"$AUTODEPLOY_TEST_SCHEMA.passes") >= AUTODEPLOY_TEST_LOOP_PASSES)); then
    kill -TERM "$PPID"
    exit 0
  fi
fi
moved=$(<"$AUTODEPLOY_TEST_CLOCK")
if [[ "$moved" =~ ^[0-9]+$ && "$1" =~ ^[0-9]+$ ]]; then
  printf '%s\n' "$((moved + $1))" >"$AUTODEPLOY_TEST_CLOCK"
fi
EOF
# timeout records the bound each call of the deployer is given, one `<seconds> <clock> <call>`
# line in $AUTODEPLOY_TEST_BOUNDS: `manifest <tag>`, `compose <command>`, `run` (a database client
# container, which it also records in the argument log as it always has), or the program's name
# (`jj`). A manifest read goes to the real timeout, because the registry-read cases read how the
# pass's deadline ends it. Every other call it runs unbounded, as it would have run, so every case
# runs as it did before this stand-in existed, unless AUTODEPLOY_TEST_TIMEOUT_CAP is set: then each
# call goes to the real timeout too. And under that cap a call is stopped after the cap's seconds,
# where its bound is longer. A call `timeout` stopped moves the pass's clock on by its whole
# bound: a case reads in seconds what the deployer does when a call runs out its bound, or the
# pass's deadline of an hour. A candidate's `compose up` takes the seconds AUTODEPLOY_TEST_UP_TAKES
# gives of the pass's clock: one that takes longer than its bound is stopped at the bound (status
# 124) before it starts anything, and one that takes no longer starts the candidate and moves the
# clock on by those seconds.
cat >"$bin/timeout" <<'EOF'
#!/usr/bin/env bash
bound=$1
case "$2 $3" in
  "docker run")
    call=run
    printf 'timeout %s\n' "$*" >>"$AUTODEPLOY_TEST_ARGV"
    ;;
  "docker manifest") call="manifest ${5##*:}" ;;
  "docker compose") call="compose $8" ;;
  *) call="$2" ;;
esac
printf '%s %s %s\n' "$bound" "$(<"$AUTODEPLOY_TEST_CLOCK")" "$call" >>"$AUTODEPLOY_TEST_BOUNDS"
if [[ "$call" == "compose up" && -n "${ENVOY_IMAGE_TAG:-}" && -n "${AUTODEPLOY_TEST_UP_TAKES:-}" ]]; then
  if ((AUTODEPLOY_TEST_UP_TAKES > bound)); then
    printf '%s\n' "$(($(<"$AUTODEPLOY_TEST_CLOCK") + bound))" >"$AUTODEPLOY_TEST_CLOCK"
    exit 124
  fi
  status=0
  "${@:2}" || status=$?
  printf '%s\n' "$(($(<"$AUTODEPLOY_TEST_CLOCK") + AUTODEPLOY_TEST_UP_TAKES))" >"$AUTODEPLOY_TEST_CLOCK"
  exit "$status"
fi
# The calls a case makes hang, which alone the cap stops: every other call runs to its answer, so
# the box's load can make none of them run out a cap of a few seconds. A manifest read always runs
# under the real timeout, with its own bound where it does not hang.
hangs=false
case "$call" in
  manifest*) [[ $(sed -n "s/^${call#manifest } //p" "$AUTODEPLOY_TEST_MANIFESTS" 2>/dev/null) != hang ]] || hangs=true ;;
  "compose pull") [[ "${AUTODEPLOY_TEST_PULL_HANG:-}" != 1 ]] || hangs=true ;;
  "compose stop") [[ "${AUTODEPLOY_TEST_STOP_HANG:-}" != 1 ]] || hangs=true ;;
  run) [[ "${AUTODEPLOY_TEST_RUN_HANG_AFTER_UP:-}" != 1 || ! -e "$AUTODEPLOY_TEST_SCHEMA.up" ]] || hangs=true ;;
  jj) [[ "${AUTODEPLOY_TEST_FETCH_HANG:-}" != 1 ]] || hangs=true ;;
esac
if [[ "$call" == manifest* ]] || [[ "$hangs" == true && -n "${AUTODEPLOY_TEST_TIMEOUT_CAP:-}" ]]; then
  limit=$bound
  cap=$bound
  [[ "$hangs" != true ]] || cap=${AUTODEPLOY_TEST_TIMEOUT_CAP:-$bound}
  ((cap < bound)) && limit=$cap
  status=0
  "$AUTODEPLOY_TEST_REAL_TIMEOUT" "$limit" "${@:2}" || status=$?
  if ((status == 124)); then
    printf '%s\n' "$(($(<"$AUTODEPLOY_TEST_CLOCK") + bound))" >"$AUTODEPLOY_TEST_CLOCK"
  fi
  exit "$status"
fi
exec "${@:2}"
EOF
# The container's own timeout, which each schema read's psql runs under, records the bound it
# was given in the argument log and then runs psql under the real timeout.
cat >"$tools/timeout" <<'EOF'
#!/usr/bin/env bash
printf 'container-timeout %s\n' "$1" >>"$AUTODEPLOY_TEST_ARGV"
exec "$AUTODEPLOY_TEST_REAL_TIMEOUT" "$@"
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
# one that took it answers t; a refused try takes AUTODEPLOY_TEST_KEY_TRY_TAKES seconds of the
# pass's clock, where that is set, so a case reaches the pass's deadline in a few tries. No fake can
# model the snapshot the read takes, so what this pins is which statements run, in which order, in
# the one run.
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
    'set lock_timeout = '*)
      [[ "${stmt#set lock_timeout = }" =~ ^[1-9][0-9]*$ ]] || {
        printf 'psql: ERROR: invalid value for parameter "lock_timeout": %s\n' "${stmt#set lock_timeout = }" >&2
        exit 1
      }
      ;;
    'select pg_try_advisory_lock(8150001)')
      if key_held; then got=f; else got=t; fi
      if [[ "$got" == f && -n "${AUTODEPLOY_TEST_KEY_TRY_TAKES:-}" ]]; then
        printf '%s\n' "$(($(<"$AUTODEPLOY_TEST_CLOCK") + AUTODEPLOY_TEST_KEY_TRY_TAKES))" >"$AUTODEPLOY_TEST_CLOCK"
      fi
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
        # runner's widening ALTER TABLE makes the read wait on its lock. Under ON_ERROR_STOP=1
        # that ends psql non-zero, and a deployer that ran the read whatever the try answered
        # would take that for a failed read rather than try the key again.
        printf 'psql: ERROR: canceling statement due to lock timeout\n' >&2
        exit 1
      fi
      # The versions as `order by version` returns them.
      sql_out=$(sort -n "$AUTODEPLOY_TEST_SCHEMA")
      ;;
    "select coalesce(string_agg(format('pid %s (%s, %s, in a transaction for %s)', l.pid, coalesce(nullif(a.application_name, ''), 'no application name'), coalesce(a.state, 'no state shown'), coalesce(date_trunc('second', now() - a.xact_start)::text, 'no time shown')), ', ' order by l.pid), 'no session the server named') from pg_locks l left join pg_stat_activity a on a.pid = l.pid where l.locktype = 'advisory' and l.granted and l.classid = 0 and l.objid = 8150001 and l.objsubid = 1")
      # The read of the key's holder, which only a refused try runs.
      if [[ "$got" != f ]]; then
        printf 'psql: the holder read ran in a session that was granted the key\n' >&2
        exit 3
      fi
      sql_out="pid 4242 (dispatch-migrate, active, in a transaction for 00:01:15)"
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
# it, \echo prints a variable, what stands between \if and \else runs only where the variable is
# true, and what stands between \else and \endif only where it is not.
skip=0
while IFS= read -r line; do
  case "$line" in
    '') ;;
    '\echo :got') ((skip)) || printf '%s\n' "$got" ;;
    '\if :got') [[ "$got" == t ]] || skip=1 ;;
    '\else') if [[ "$got" == t ]]; then skip=1; else skip=0; fi ;;
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
chmod +x "$bin/jj" "$bin/date" "$bin/docker" "$bin/curl" "$bin/sleep" "$bin/timeout" "$tools/timeout" "$tools/psql" "$tools/pg_dump"

# deployer runs the deployer with the stand-ins first on PATH and its arguments: deploy_once is one
# pass, as `--once`, and the continuous loop is the deployer with none. Each run is under the real
# timeout, deployer_prefix, at 300 s, far past any case's own run, so a deployer that waits on
# something no case answers (a poll that lost its deadline, say) fails its case rather than hold
# the suite; a case sets its own prefix where its subject is how a run ends.
deployer_prefix=("$real_timeout" 300)
deployer() {
  PATH="$bin:$PATH" AUTODEPLOY_TEST_LOG="$log" AUTODEPLOY_TEST_ARGV="$argv" AUTODEPLOY_TEST_SCHEMA="$schema" \
    AUTODEPLOY_TEST_TOOLS="$tools" AUTODEPLOY_TEST_PASSWORD="${test_password-$password}" REPO="$repo" COMPOSE_DIR="$compose" \
    AUTODEPLOY_TEST_MANIFESTS="$manifests" AUTODEPLOY_TEST_MANIFEST_LOG="$manifest_log" \
    AUTODEPLOY_TEST_REAL_TIMEOUT="$real_timeout" AUTODEPLOY_TEST_REAL_DATE="$real_date" \
    AUTODEPLOY_TEST_REAL_JJ="$real_jj" JJ_CONFIG="$jj_config" \
    AUTODEPLOY_TEST_BOUNDS="$bounds_log" AUTODEPLOY_TEST_CLOCK="$clock" AUTODEPLOY_TEST_CLOCK_BASE="$clock_base" \
    BACKUPS="$backups" COMPOSE_PROJECT=dispatch-test IMAGE=example.test/dispatch \
    "${deployer_prefix[@]}" "$deployer_script" "$@" 2>&1
}
deploy_once() {
  deployer --once
}
# case_begin <name> counts a case of the group being run, by the name its report lists.
cases=0
group_cases=0
case_begin() {
  cases=$((cases + 1))
  group_cases=$((group_cases + 1))
  current_case=$1
}
fail() {
  printf '%s: %s\n' "${current_case:-setup}" "$1" >&2
  printf -- '--- deployer output:\n%s\n--- compose:\n%s\n--- argv:\n%s\n--- bounds:\n%s\n' "${output:-}" "$(<"$log")" "$(<"$argv")" "$(<"$bounds_log")" >&2
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
# bounds_of <call>: the bounds the deployer gave the calls the timeout stand-in records as <call>,
# in order, one a line; clocks_of <call>: the pass's clock at each of those calls, in the same
# order.
bounds_of() {
  awk -v call="$1" '{ bound = $1; $1 = ""; $2 = ""; sub(/^  /, ""); if ($0 == call) print bound }' "$bounds_log"
}
clocks_of() {
  awk -v call="$1" '{ at = $2; $1 = ""; $2 = ""; sub(/^  /, ""); if ($0 == call) print at }' "$bounds_log"
}
# is_exactly <what> <want> <got>: fails the case unless <got> is the number <want>.
is_exactly() {
  if [[ "$3" != "$2" ]]; then
    fail "$1 was ${3:-nothing}, not $2"
  fi
}
# still_hung: the pid of the manifest read the stand-in recorded as never answering, where that
# process still runs (same start time, no zombie); empty otherwise.
still_hung() {
  local pid since stat fields
  [[ -s "$manifests.hung" ]] || return 0
  read -r pid since <"$manifests.hung"
  stat=$(cat "/proc/$pid/stat" 2>/dev/null) || return 0
  stat=${stat##*) }
  read -r -a fields <<<"$stat"
  if [[ "${fields[0]}" != Z && "${fields[19]}" == "$since" ]]; then
    printf '%s\n' "$pid"
  fi
}
