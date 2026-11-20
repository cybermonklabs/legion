#!/usr/bin/env bash
# Keeps the devbox Dispatch on the newest main commit with a published image.
set -euo pipefail

POLL="${POLL:-300}"
REPO="${REPO:-/home/ubuntu/.worktrees/legion/dn-an}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-envoy-dispatch}"
COMPOSE_DIR="${COMPOSE_DIR:-$REPO/packages/envoy/deploy/compose}"
BACKUPS="${BACKUPS:-/home/ubuntu/tmp/dispatch-backups}"
IMAGE="${IMAGE:-ghcr.io/cybermonklabs/legion/envoy}"
COMPOSE_FILE="$COMPOSE_DIR/dispatch.compose.yml"
COMPOSE_ENV="$COMPOSE_DIR/.env"
# FAILED_DEPLOY records each deploy that started a new image and did not succeed, one line each:
#
#   <commit> <state> <the backup taken before that deploy>
#
# where <state> is `rolled-back`, the previous image serves again, or `stopped`: the rollback was
# refused or failed, or the pass ended between the new image's start and its result, and Dispatch
# is left for a person. A pass deploys no commit that a rolled-back line names, nor any ancestor of
# one: only a newer published commit. A stopped line ends the continuous loop, and while it is
# there no pass deploys anything. prune_backups keeps every backup the file names. The deployer
# removes the file after a deploy succeeds; a person removes it to let the deployer deploy a
# recorded commit again, or to start again after a stop.
FAILED_DEPLOY="$BACKUPS/failed-deploy"
# MIGRATION_KEY is the advisory lock key Dispatch's migration runner holds for each migration
# (internal/dispatch/store/store.go, migrationLockKey; store's deployer_key_test.go holds the two
# equal).
MIGRATION_KEY=8150001
# pg_dump N dumps servers <= N; the production database is Aurora PostgreSQL 16.
PG_DUMP_IMAGE="${PG_DUMP_IMAGE:-postgres:16.11-alpine}"
# SCHEMA_READ_DEADLINE is how long the read of the schema before a deploy polls the migration
# advisory key before it gives up on reading the applied set, in whole seconds. It bounds the whole
# read: each try's psql is given the time this deadline leaves. A read it ends refuses the deploy,
# which changes nothing: the previous image still serves, and the next pass reads again. It does
# not bound the read after a failed deploy's stop, which decides the rollback (schema_applied).
SCHEMA_READ_DEADLINE="${SCHEMA_READ_DEADLINE:-60}"
# PASS_DEADLINE is how long one pass may take, in whole seconds, counted from its start. It is the
# pass's one deadline: every call of the pass that waits on the registry, Docker or the database
# is given the time it leaves, so a call that answers slowly is waited for, and only one that has
# not answered when the pass's time is spent is stopped, named as having run out the pass's
# deadline. Every step of a deploy keeps ROLLBACK_RESERVE of that time for the deploy's rollback
# (deploy_bound), and every step of the rollback takes what is left. Three steps of a deploy keep
# a bound of their own, each cut to what the pass leaves after that reserve: PULL_BOUND, UP_BOUND
# and the health check's HEALTH_BOUND. The deadline is the pass's and not the poll's: a deploy's
# rollback comes after those three, so a deadline as short as POLL would leave every slow deploy's
# rollback no time, and the rollback would be refused.
PASS_DEADLINE="${PASS_DEADLINE:-3600}"
# PULL_BOUND is the pull's own bound. A pull that runs it out changes nothing: the previous image
# still serves, and the next pass pulls again.
PULL_BOUND=900
# UP_BOUND is the deploy's limit for the new image's start, `docker compose up -d`: a start that
# has not finished within it is rolled back, and the previous image serves again.
UP_BOUND=300
# The health check asks /healthz up to HEALTH_TRIES times, each try a wait of HEALTH_INTERVAL
# seconds and then a request bounded at HEALTH_REQUEST_BOUND seconds, so HEALTH_BOUND is the most
# one health check takes.
HEALTH_TRIES=30
HEALTH_INTERVAL=3
HEALTH_REQUEST_BOUND=3
HEALTH_BOUND=$((HEALTH_TRIES * (HEALTH_INTERVAL + HEALTH_REQUEST_BOUND)))
# ROLLBACK_RESERVE is the time of the pass every step of a deploy leaves for the deploy's rollback:
# the rollback's health check's HEALTH_BOUND, the one step of a rollback with a bound of its own.
# So a deploy whose steps run until the pass is nearly spent, a health check that fails late among
# them, still leaves its rollback that time, and a deploy the pass has no more than that time left
# for does not start.
ROLLBACK_RESERVE=$HEALTH_BOUND

case "$#" in
  0)
    once=false
    ;;
  1)
    if [[ "$1" != "--once" ]]; then
      printf 'usage: %s [--once]\n' "$0" >&2
      exit 2
    fi
    once=true
    ;;
  *)
    printf 'usage: %s [--once]\n' "$0" >&2
    exit 2
    ;;
esac

# Each setting of seconds is a whole number above 0, or the deployer refuses to start: bash reads
# `300s` as an arithmetic error, which a walk that reads it fails on and takes for no image at all.
for setting in POLL PASS_DEADLINE SCHEMA_READ_DEADLINE; do
  if [[ ! "${!setting}" =~ ^[1-9][0-9]{0,8}$ ]]; then
    printf '%s is %q, which is no whole number of seconds from 1 to 999999999 (write 300, not 300s or 5m); not starting\n' \
      "$setting" "${!setting}" >&2
    exit 2
  fi
done
# The pass's deadline leaves a rollback its time: a pass whose deploy steps each run out their own
# bound still has ROLLBACK_RESERVE left, the rollback's health check. The rollback's other steps
# have no bound of their own and share it.
PASS_DEADLINE_MINIMUM=$((PULL_BOUND + UP_BOUND + HEALTH_BOUND + ROLLBACK_RESERVE))
if ((PASS_DEADLINE < PASS_DEADLINE_MINIMUM)); then
  printf 'PASS_DEADLINE is %ss, below the %ss a pass needs: %ss for its deploy steps (the pull %ss, the up %ss, the health check %ss) and %ss for its rollback'"'"'s health check; not starting\n' \
    "$PASS_DEADLINE" "$PASS_DEADLINE_MINIMUM" "$((PULL_BOUND + UP_BOUND + HEALTH_BOUND))" "$PULL_BOUND" "$UP_BOUND" "$HEALTH_BOUND" "$ROLLBACK_RESERVE" >&2
  exit 2
fi

mkdir -p "$BACKUPS"

log() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

# clock [call] prints the time the pass's deadline is read against, in whole seconds since the
# epoch. A date that answers anything else (an error, nothing, a number with a unit) is named on
# stderr with [call], what could not start without it (by default the pass's next step), and clock
# fails, so the pass fails on that line, rather than on an arithmetic error of bash's that names
# nothing or on a number that is no time.
clock() {
  local now call="${1:-the next step of the pass could start}"
  now=$(date +%s 2>/dev/null) || now=
  if [[ ! "$now" =~ ^[1-9][0-9]{0,17}$ ]]; then
    now=${now//[^[:print:]]/?}
    log "the clock, date +%s, answered '${now:0:60}', which is no time in seconds, before $call" >&2
    return 1
  fi
  printf '%s\n' "$now"
}

# pass_bound <call> [own] prints the seconds <call> may wait: what the pass's deadline leaves, or
# own, the call's own bound, where that is less. Once the deadline has passed it logs, on stderr,
# that <call> could not start, naming the deadline, and fails, printing nothing: no call is ever
# handed 0, which `timeout` and pg_client read as no bound at all. <call> says what is not done.
pass_bound() {
  local now left
  now=$(clock "$1") || return 1
  left=$((pass_deadline - now))
  if ((left < 1)); then
    log "the pass's deadline (PASS_DEADLINE=${PASS_DEADLINE}s) passed before $1" >&2
    return 1
  fi
  if [[ -n "${2:-}" ]] && ((left > $2)); then
    left=$2
  fi
  printf '%s\n' "$left"
}

# deploy_bound <call> [own] is pass_bound for a step of a deploy: what the pass leaves once
# ROLLBACK_RESERVE is kept for the deploy's rollback, or own where that is less. Where the pass
# leaves no more than that reserve it logs that <call> could not start, naming both, and fails.
deploy_bound() {
  local now left
  now=$(clock "$1") || return 1
  if ((now >= pass_deadline)); then
    log "the pass's deadline (PASS_DEADLINE=${PASS_DEADLINE}s) passed before $1" >&2
    return 1
  fi
  left=$((pass_deadline - ROLLBACK_RESERVE - now))
  if ((left < 1)); then
    log "the pass leaves $((pass_deadline - now))s, no more than the ${ROLLBACK_RESERVE}s it keeps for a rollback, before $1" >&2
    return 1
  fi
  if [[ -n "${2:-}" ]] && ((left > $2)); then
    left=$2
  fi
  printf '%s\n' "$left"
}

# bound_note <status> [own] prints, for a call `timeout` stopped (status 124), the words that say
# which bound stopped it: the pass's deadline, where that has passed; the time the pass keeps for a
# rollback, where only that is left (deploy_bound); or else the call's own bound of own seconds. It
# prints nothing for any other status.
bound_note() {
  local now
  (($1 == 124)) || return 0
  now=$(clock) || return 0
  if ((now >= pass_deadline)); then
    printf " (it had not answered when the pass's deadline, PASS_DEADLINE=%ss, passed)" "$PASS_DEADLINE"
  elif ((now >= pass_deadline - ROLLBACK_RESERVE)); then
    printf " (it had not answered when the pass had only the %ss it keeps for a rollback left)" "$ROLLBACK_RESERVE"
  elif [[ -n "${2:-}" ]]; then
    printf ' (it ran out its own %ss)' "$2"
  fi
}

compose() {
  local duration="$1"
  shift
  timeout "$duration" docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" "$@"
}

compose_with_tag() {
  local tag="$1"
  local duration="$2"
  shift 2
  env ENVOY_IMAGE_TAG="$tag" timeout "$duration" docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" "$@"
}

env_value() {
  sed -nE "s/^$1=(.*)$/\1/p" "$COMPOSE_ENV"
}

deployed_tag() {
  env_value ENVOY_IMAGE_TAG
}

set_env_value() {
  if grep -q "^$1=" "$COMPOSE_ENV"; then
    sed -i -E "s|^$1=.*|$1=$2|" "$COMPOSE_ENV"
  else
    printf '%s=%s\n' "$1" "$2" >>"$COMPOSE_ENV"
  fi
}

# database_url_split reads DATABASE_URL into db_url, the URL without its password, and db_password,
# that password percent-decoded. The database clients below get db_url as their argument and the
# password as PGPASSWORD, an environment variable, so no process's arguments carry a credential:
# a container's processes are the host's, and /proc/<pid>/cmdline is world-readable on a shared
# host. It accepts exactly one shape and fails on every other, printing nothing:
#
#   postgres[ql]://<user>[:<password>]@<host>[:<port>]/<database>[?<key>=<value>[&<key>=<value>]…]
#
#   - <user>, <database> and each <value>: ASCII letters, digits, `.`, `_` and `-`.
#   - <password>: ASCII letters, digits, `.`, `_`, `~` and `-`, and %XX escapes other than %00,
#     whose decoded bytes are valid UTF-8. Any other character (@ : / ? # [ ], %, a space,
#     non-ASCII) has to be percent-encoded.
#   - <host>: dot-separated labels of ASCII letters, digits and hyphens, none empty and none
#     starting or ending with a hyphen. That is a host name or an IPv4 address; no brackets, no
#     second host, no socket path.
#   - <port>: 1 to 65535.
#   - <key>: sslmode, connect_timeout or application_name.
#
# In that shape every reading of the URL, the deployer's, libpq's and pgx's, puts the password in
# the same place, and nowhere else. LC_ALL=C makes the ranges ASCII whatever the caller's locale:
# under en_US.UTF-8, [a-z] and [0-9] also match letters and digits outside ASCII, and under a
# double-byte locale such as GBK one character can hide an @.
database_url_split() {
  local LC_ALL=C
  local value='[A-Za-z0-9._-]+'
  local key='(sslmode|connect_timeout|application_name)'
  local escape='%(0[1-9A-Fa-f]|[1-9A-Fa-f][0-9A-Fa-f])'
  local label='[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?'
  local port='([1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])'
  local grammar="^(postgres|postgresql)://$value(:([A-Za-z0-9._~-]|$escape)+)?@$label(\\.$label)*(:$port)?/$value(\\?$key=$value(&$key=$value)*)?\$"
  local userinfo
  local password=""
  if [[ ! "$1" =~ $grammar ]]; then
    return 1
  fi
  # An accepted URL holds exactly one @, so the user and password are before it, and the rest of
  # the URL, the clients' part, is after it.
  userinfo="${1#*://}"
  userinfo="${userinfo%%@*}"
  if [[ "$userinfo" == *:* ]]; then
    password="${userinfo#*:}"
  fi
  printf -v db_password '%b' "${password//%/\\x}"
  # Docker rewrites an environment value that is not valid UTF-8, so such a password would reach
  # the clients as different bytes than libpq or pgx read from the URL. The check reads it from a
  # pipe, never an argument.
  if ! printf '%s' "$db_password" | iconv -f UTF-8 -t UTF-8 >/dev/null 2>&1; then
    return 1
  fi
  db_url="${1%%://*}://${userinfo%%:*}@${1#*@}"
}

# pg_client runs shell command $2 in the pg_dump image, bounded at $1 seconds, with the connection
# in its environment: DATABASE_URL (db_url) and PGPASSWORD (db_password), each passed to docker by
# name, so docker reads the value from this process's environment. Every caller takes $1 from
# deploy_bound or pass_bound, which never give 0, the value `timeout` reads as no bound. --init runs
# the command under an init that passes on the signal docker forwards when `timeout` fires; as PID 1
# the shell would ignore it, and the container would outlive the bound.
pg_client() {
  PGPASSWORD="$db_password" DATABASE_URL="$db_url" timeout "$1" \
    docker run --init --rm --network host -e PGPASSWORD -e DATABASE_URL "$PG_DUMP_IMAGE" sh -c "$2"
}

# schema_applied <deadline> <bounder> prints a value standing for the set of migrations the
# database has applied:
#
#   <max>-<count>-<sha256 of the versions, ascending, one per line>
#
# Not max(version) alone, the `schema_version` Dispatch's /healthz reports: a migration whose
# version is below one already applied, a numbered migration merged after a timestamped one
# deployed, leaves max where it was, and a guard reading max alone would start the previous image
# on a schema it never ran against. The max and the count are there for a reader of a log line;
# the digest is what changes whenever the set changes.
#
# It reads under the advisory lock Dispatch's migration runner holds for each migration
# (MIGRATION_KEY): one psql session runs a script that tries for the key and then, only once the
# try was granted, reads the versions. A migration transaction still in flight, one a stopped image
# sent its commit for included, is waited out as far as <deadline> reaches, a time of the pass's
# clock: one that ends before it ends before the read takes its snapshot, and one still holding the
# key when <deadline> comes is not waited out at all: that read fails with status 75, and prints
# the session the server named as the key's holder, and the caller refuses to deploy or to roll
# back rather than read behind a held key. The read before a deploy is given SCHEMA_READ_DEADLINE,
# since a read refused there changes nothing. The read after a failed deploy's stop is given the
# pass's deadline: the holder is then a migration the stopped image sent, which still works, and a
# read refused while it works would leave Dispatch stopped for a person. The versions a granted
# try reads come back ordered by the database, and the key is still held as they do (measured on
# PostgreSQL 18.6: while the read runs, the reading backend is the one session holding the key in
# that database). The session's end releases it. One statement could not take the key and read
# behind it: a statement's snapshot is taken when it starts, before its own one-time filter calls
# pg_try_advisory_lock, so its read would still be of the schema as it stood before the migration
# committed (measured too).
#
# It polls the key rather than waiting for it, because any blocking wait on this key deadlocks
# against the runner that holds it. A backend blocked on a lock keeps the snapshot its statement
# took, and CREATE INDEX CONCURRENTLY waits for every snapshot older than its own, so a holder
# building an index concurrently and a waiter on the key wait for each other (PostgreSQL 18.6,
# SQLSTATE 40P01). The runner polls for the same reason (store.go, lockMigrations). So each try is
# a psql session of its own, with no statement open between two tries: a try refused the key
# answers f and the key's holder, takes nothing and reads nothing of the schema, and the next try
# follows a second later as a fresh session holding nothing. The \if is what keeps the read out of
# a refused try, and it is no nicety. A read that ran behind a held key can fail (a first boot
# holds the key before schema_migrations is committed, and the runner's one-time widening ALTER
# TABLE makes the read wait on its lock), and under ON_ERROR_STOP=1 that failure ends psql non-zero,
# which would end this read in its caller rather than try the key again.
#
# Every wait of a try takes what <deadline> leaves: its psql's `timeout` inside the container, and
# lock_timeout, which ends a statement's wait for a table lock, so a try that starts a second before
# <deadline> ends at it. lock_timeout is in milliseconds and Postgres takes no more than 2147483647
# of them (INT_MAX), so a deadline further off than that, 24 days, gives it that most. The
# container's start is Docker's, and is given what <bounder> leaves (deploy_bound before a deploy,
# pass_bound for a rollback): a Docker that creates a container slowly still answers.
#
# psql's stderr is dropped, since its diagnostics can quote the connection; a failure prints
# nothing and returns psql's status, or 1 for an answer that is no list of versions.
schema_applied() {
  local deadline="$1"
  local bounder="$2"
  local sql answer versions line count max digest now left lock bound holder=""
  while true; do
    now=$(clock "the next try of the schema read could start") || return 1
    left=$((deadline - now))
    if ((left < 1)); then
      printf '%s\n' "${holder:-no session the server named}"
      return 75
    fi
    bound=$("$bounder" "the schema read's container could start") || return 1
    lock=$((left > 2147483 ? 2147483 : left))
    # The script reaches psql on its standard input, since \gset and \if are psql's own and -c
    # takes SQL alone. The here-document is the container shell's, so the script travels as part
    # of sh -c's argument (pg_client) and nothing of it reaches psql's own arguments; it carries no
    # credential, only the connection's name in the environment.
    sql="psql -X -q -At -v ON_ERROR_STOP=1 -f - \"\$DATABASE_URL\" <<'PSQL'
set lock_timeout = ${lock}000;
select pg_try_advisory_lock($MIGRATION_KEY) as got \\gset
\\echo :got
\\if :got
select version from schema_migrations order by version;
\\else
select coalesce(string_agg(format('pid %s (%s, %s, in a transaction for %s)', l.pid, coalesce(nullif(a.application_name, ''), 'no application name'), coalesce(a.state, 'no state shown'), coalesce(date_trunc('second', now() - a.xact_start)::text, 'no time shown')), ', ' order by l.pid), 'no session the server named') from pg_locks l left join pg_stat_activity a on a.pid = l.pid where l.locktype = 'advisory' and l.granted and l.classid = 0 and l.objid = $MIGRATION_KEY and l.objsubid = 1;
\\endif
PSQL
"
    answer=$(pg_client "$bound" "timeout $left $sql" 2>/dev/null) ||
      return
    case "$answer" in
      t | t$'\n'*) break ;;
      # f: another session holds the key, and the line after it names that session.
      f) holder= ;;
      f$'\n'*)
        holder=${answer#f$'\n'}
        holder=${holder%%$'\n'*}
        holder=${holder//[^[:print:]]/?}
        holder=${holder:0:300}
        ;;
      # Any other answer is none of this read's.
      *) return 1 ;;
    esac
    sleep 1
  done
  versions="${answer#t}"
  versions="${versions#$'\n'}"
  count=0
  max=0
  digest=""
  if [[ -n "$versions" ]]; then
    while IFS= read -r line; do
      [[ "$line" =~ ^[0-9]+$ ]] || return 1
      digest+="$line"$'\n'
      count=$((count + 1))
      max="$line"
    done <<<"$versions"
  fi
  digest=$(printf '%s' "$digest" | sha256sum) || return 1
  printf '%s-%s-%s\n' "$max" "$count" "${digest%% *}"
}

# schema_failure <status> <output> <deadline> prints the words for a schema_applied that failed
# with <status> and printed <output>: for 75, the key's holder, named as holding the key when
# <deadline> came; else the status, with the bound that stopped the read where `timeout` did.
schema_failure() {
  if (($1 == 75)); then
    printf 'the migration key, %s, was still held by %s when %s' "$MIGRATION_KEY" "$2" "$3"
  else
    printf 'status %s%s' "$1" "$(bound_note "$1")"
  fi
}

# schema_changed answers whether the applied set moved from a recorded baseline to the set the
# database holds now: 0 when it moved, 1 when it did not. A baseline in the form above holds the
# whole set, so any difference is a move; a bare max(version), which the deployer before this one
# recorded, holds only the highest version, so all it can show is a version applied above it.
schema_changed() {
  local baseline="$1"
  local now="$2"
  if [[ "$baseline" == *-* ]]; then
    [[ "$now" != "$baseline" ]]
    return
  fi
  ((${now%%-*} > baseline))
}

# schema_label describes a value for a log line: the highest version applied, and, for a value
# that holds the set, how many versions that set holds.
schema_label() {
  local rest
  if [[ "$1" == *-* ]]; then
    rest="${1#*-}"
    printf '%s (%s applied)\n' "${1%%-*}" "${rest%%-*}"
  else
    printf '%s\n' "$1"
  fi
}

# newest_published_main prints the newest of main's last 30 commits whose image the registry holds,
# and returns 1 when the registry answers that it holds none of them. It passes over a commit only
# when the registry answered that it holds no such manifest. Silence and every other answer (a
# refused credential, a rate limit, a server error) prove nothing about the image, and a walk that
# passed over such a commit would pick an older one: where that commit is the one deployed, the
# pass would start an image older than its database's schema. So such a read is logged, naming the
# image it asked for, and returns 2, which fails the pass with nothing deployed.
#
# A read has no bound of its own, and the walk none either. Each read waits as long as the pass's
# deadline leaves (pass_bound), since a registry that answers slowly still answers, and only a
# read that has not answered when the pass's time is spent is stopped, named as having run out
# the pass's deadline. `docker manifest inspect` asks the registry and makes nothing on this host,
# so a stopped read leaves nothing to remove. The fetch of main before the walk is given what the
# pass leaves too; one that fails is named, and the walk reads main as it was last fetched, since
# the order read in run_once keeps a stale main from deploying anything older. The log lines go to
# stderr, because the caller reads this function's stdout as the commit.
newest_published_main() {
  local sha answer status=0 bound
  bound=$(pass_bound "jj git fetch of $REPO could start; not deploying") || return 2
  timeout "$bound" jj -R "$REPO" git fetch >/dev/null 2>&1 || status=$?
  if ((status != 0)); then
    log "jj git fetch of $REPO failed with status $status$(bound_note "$status"); the walk reads main as it was last fetched" >&2
  fi
  while IFS= read -r sha; do
    bound=$(pass_bound "docker manifest inspect $IMAGE:$sha could start; not deploying") || return 2
    if answer=$(timeout "$bound" docker manifest inspect "$IMAGE:$sha" 2>&1 >/dev/null); then
      printf '%s\n' "$sha"
      return 0
    else
      status=$?
    fi
    if ((status == 124)); then
      log "docker manifest inspect $IMAGE:$sha had not answered when the pass's deadline (PASS_DEADLINE=${PASS_DEADLINE}s) passed; not deploying" >&2
      return 2
    fi
    case "$answer" in
      *"manifest unknown"* | *"no such manifest"*) continue ;;
    esac
    log "docker manifest inspect $IMAGE:$sha failed with status $status, an answer that does not say the image is missing: ${answer%%$'\n'*}; not deploying" >&2
    return 2
  done < <(jj -R "$REPO" log -r 'ancestors(main@origin, 30)' --no-graph -T 'commit_id ++ "\n"')
  return 1
}

# prune_backups keeps the ten newest pre-deploy backups, and every backup FAILED_DEPLOY names: the
# one taken before a deploy that failed is what a person recovering from that deploy may need, so
# it stays until a later deploy succeeds and the record goes.
prune_backups() {
  local -a dumps=()
  local -A kept=()
  local dump
  local i
  if [[ -e "$FAILED_DEPLOY" ]]; then
    while read -r _ _ dump; do
      [[ -z "$dump" ]] || kept["$dump"]=1
    done <"$FAILED_DEPLOY"
  fi
  while IFS= read -r -d '' dump; do
    dumps+=("$dump")
  done < <(find "$BACKUPS" -maxdepth 1 -type f -name 'pre-*.dump' -printf '%T@:%p\0' | sort -znr)
  for ((i = 10; i < ${#dumps[@]}; i++)); do
    dump="${dumps[i]#*:}"
    [[ -n "${kept[$dump]:-}" ]] || rm -f -- "$dump"
  done
}

# record_failed <commit> <state> <backup> writes the line FAILED_DEPLOY holds for <commit>, in place
# of any line it held for it, through a file written beside it and renamed over it. It logs a write
# that failed and fails.
record_failed() {
  local others=""
  if [[ -e "$FAILED_DEPLOY" ]] && ! others=$(awk -v commit="$1" '$1 != commit' "$FAILED_DEPLOY"); then
    log "could not read $FAILED_DEPLOY to record the failed deploy of $1"
    return 1
  fi
  if ! { { [[ -z "$others" ]] || printf '%s\n' "$others"; } && printf '%s %s %s\n' "$1" "$2" "$3"; } >"$FAILED_DEPLOY.new" ||
    ! mv -f -- "$FAILED_DEPLOY.new" "$FAILED_DEPLOY"; then
    log "could not record the failed deploy of $1 as $2 in $FAILED_DEPLOY"
    return 1
  fi
}

# stopped_commit prints the commit of FAILED_DEPLOY's first stopped line, and nothing where it holds
# none or there is no such file.
stopped_commit() {
  [[ -e "$FAILED_DEPLOY" ]] || return 0
  awk '$2 == "stopped" { print $1; exit }' "$FAILED_DEPLOY"
}

# skips_failed <sha> answers whether a pass must not deploy <sha> because FAILED_DEPLOY holds a
# deploy that failed of <sha> or of a descendant of it: 0, and it logs one line naming both commits
# and the file; 1 when <sha> is newer than every commit the file holds; 2, logged, when jj cannot
# answer the order of the two.
skips_failed() {
  local failed order
  [[ -e "$FAILED_DEPLOY" ]] || return 1
  while read -r failed _; do
    [[ -n "$failed" ]] || continue
    order=$(commit_order "$1" "$failed") || order=
    case "$order" in
      newer) ;;
      older)
        if [[ "$1" == "$failed" ]]; then
          log "$1 failed its deploy before, as $FAILED_DEPLOY records: deploying only a commit newer than it (delete $FAILED_DEPLOY to deploy it again)"
        else
          log "$1 is an ancestor of $failed, whose deploy failed, as $FAILED_DEPLOY records: deploying only a commit newer than it (delete $FAILED_DEPLOY to deploy either)"
        fi
        return 0
        ;;
      *)
        log "could not read from $REPO whether $1 is newer than $failed, whose deploy failed ($FAILED_DEPLOY); not deploying"
        return 2
        ;;
    esac
  done <"$FAILED_DEPLOY"
  return 1
}

oauth_redirect_uri() {
  local origin="$1"
  local port="$2"
  local bound="$3"
  local host="${origin#http://}"
  host="${host#https://}"
  curl -s --max-time "$bound" -o /dev/null -w '%{redirect_url}' -H "Host: $host" "http://127.0.0.1:$port/auth/start" |
    python3 -c 'import sys, urllib.parse as u; query = u.parse_qs(u.urlparse(sys.stdin.read()).query); print(query.get("redirect_uri", [""])[0])'
}

# wait_for_health <port> <bounder> <what> asks /healthz on port <port> up to HEALTH_TRIES times,
# each try a wait of HEALTH_INTERVAL s and a request bounded at HEALTH_REQUEST_BOUND s (HEALTH_BOUND
# at most), to answer 200 with NATS connected, or turned off (`nats` null). It is the deploy's
# health limit, and it asks Dispatch, not Docker. Each try is cut to what <bounder> (deploy_bound
# for a deploy's check, pass_bound for a rollback's) leaves, and the check fails at the try that
# has no time left, so a deploy's check that fails still leaves its rollback ROLLBACK_RESERVE.
# Dispatch serves while NATS is down or refuses its credential, so a 200 alone
# would keep an image whose NATS connection fails; reading `nats` rolls such an image back, as when
# Dispatch exited at boot without NATS. A NATS outage fails the previous image's check too.
wait_for_health() {
  local port="$1"
  local bounder="$2"
  local attempt bound wait
  for ((attempt = 0; attempt < HEALTH_TRIES; attempt++)); do
    bound=$("$bounder" "try $((attempt + 1)) of the health check of $3 could start" "$((HEALTH_INTERVAL + HEALTH_REQUEST_BOUND))") ||
      return 1
    wait=$((bound > HEALTH_INTERVAL ? HEALTH_INTERVAL : bound - 1))
    sleep "$wait"
    if curl -sf --max-time "$((bound - wait))" "http://127.0.0.1:$port/healthz" |
      python3 -c 'import json, sys; sys.exit(json.load(sys.stdin).get("nats") is False)' 2>/dev/null; then
      return 0
    fi
  done
  return 1
}

# rollback starts $previous again, once roll_back_or_stop has read that the applied migrations are
# still the ones $previous runs against. Its `up` has no bound of its own and waits as long as the
# pass's deadline leaves: Dispatch is down until it returns, and a Docker that starts a container
# slowly still answers, so a bound of its own would only turn a slow rollback into one a person
# finishes. A rollback that fails returns 3: Dispatch is left for a person, and its line says that
# $previous may be started again by hand, since the schema is still its own.
rollback() {
  local previous="$1"
  local dispatchPort="$2"
  local bound
  local status=0
  local by_hand="so Dispatch is left for a person: the applied migrations are still the ones $previous runs against, so it may be started again by hand"
  sed -i -E "s|^ENVOY_IMAGE_TAG=.*|ENVOY_IMAGE_TAG=$previous|" "$COMPOSE_ENV"
  bound=$(pass_bound "the rollback's docker compose up of $previous could start, $by_hand") || return 3
  compose "$bound" up -d --no-deps dispatch || status=$?
  if ((status != 0)); then
    log "rollback compose failed for $previous$(bound_note "$status"), $by_hand"
    return 3
  fi
  if ! wait_for_health "$dispatchPort" pass_bound "$previous"; then
    log "rollback to $previous did not become healthy in the tries its health check had (at most $HEALTH_TRIES), $by_hand"
    return 3
  fi
}

# roll_back_or_stop answers a deploy of $sha that failed. It restores $previous only while the
# schema is still the one $previous runs against. A failed image may have migrated before it failed,
# and an image older than its database's schema can write what the newer release fails to read
# (packages/envoy/cmd/dispatch/README.md, "Database migrations"). So it stops $sha first and
# confirms the stop, then reads the schema, so nothing $sha runs can move it after the read. Once the
# set of applied migrations has changed, or the stop or the read fails, it starts nothing and
# returns 3, for an operator to recover forward; each such line says whether $sha changed the
# applied migrations, or that it is not known because the schema was not read. It compares the
# whole set, not max(version): a migration applied below one already applied leaves the max where
# it was.
#
# A $baseline the deployer before this one recorded is a bare max(version), with no set behind it.
# While the tag that wrote it is still deployed, it is compared the old way, so a retry in flight
# across the upgrade keeps the safety it started with; the first deploy under a new tag records the
# set.
#
# Its stop, its confirmation and its schema read have no bound of their own: each waits as long as
# the pass's deadline leaves (pass_bound), and so do the rollback's up and its health check after
# them. Docker answers each of them, slowly on a loaded host, and a migration the stopped image
# sent still holds the key while it works, and a bound of their own that fired would refuse the
# rollback and leave Dispatch stopped for a person; only a call that has not answered by then fails,
# named.
roll_back_or_stop() {
  local sha="$1"
  local previous="$2"
  local baseline="$3"
  local dispatchPort="$4"
  local running
  local now
  local status=0
  local refusing
  local unknown
  local bound
  refusing="so refusing to roll back to $previous, which runs against schema $(schema_label "$baseline")"
  unknown="$sha may still run, and whether it changed the applied migrations is not known, since the schema was not read: forward recovery is required"
  bound=$(pass_bound "docker compose stop of $sha could start, $refusing; $unknown") || return 3
  compose "$bound" stop dispatch || status=$?
  if ((status != 0)); then
    log "could not stop $sha$(bound_note "$status"), $refusing; $unknown"
    return 3
  fi
  bound=$(pass_bound "docker compose ps could confirm $sha stopped, $refusing; $unknown") || return 3
  running=$(compose "$bound" ps --status running -q dispatch) || status=$?
  if ((status != 0)) || [[ -n "$running" ]]; then
    log "could not confirm $sha stopped$(bound_note "$status"), $refusing; $unknown"
    return 3
  fi
  unknown="$sha is stopped, and whether it changed the applied migrations is not known, since the schema was not read: forward recovery is required"
  now=$(schema_applied "$pass_deadline" pass_bound) || {
    status=$?
    log "could not read the schema version after stopping $sha ($(schema_failure "$status" "$now" "the pass's deadline (PASS_DEADLINE=${PASS_DEADLINE}s) passed")), $refusing; $unknown"
    return 3
  }
  if schema_changed "$baseline" "$now"; then
    log "refusing to roll back to $previous: it runs against schema $(schema_label "$baseline"), and $sha changed the applied migrations to $(schema_label "$now") before it failed. $sha is stopped, and forward recovery is required: deploy an image that fixes it, or start $sha by hand once its failure is understood. Never start $previous against schema $(schema_label "$now")"
    return 3
  fi
  log "rolling back to $previous (schema $(schema_label "$now"))"
  rollback "$previous" "$dispatchPort"
}

# failed_deploy <sha> <previous> <baseline> <port> <dump> answers a deploy that started $sha and
# failed. FAILED_DEPLOY already records $sha as stopped (deploy writes it before the start), so a
# pass that dies inside the rollback leaves the loop stopped rather than starting $sha again. It
# rolls back, and a rollback that restored $previous records the failure as rolled-back, and the
# pass fails with 1: no later pass deploys $sha again. A rollback refused or failed returns 3,
# which ends the continuous loop, and so does a record it could not rewrite, which still says
# stopped. Its callers run it as a command of its own, never in a condition, and so does it run
# roll_back_or_stop: set -e then stays on inside both, and a refusal ends the pass at once with its
# status.
failed_deploy() {
  roll_back_or_stop "$1" "$2" "$3" "$4"
  if ! record_failed "$1" rolled-back "$5"; then
    log "$FAILED_DEPLOY still records the deploy of $1 as stopped, so the deployer stops"
    return 3
  fi
  return 1
}

deploy() {
  local sha="$1"
  local previous
  local databaseUrl
  local db_url
  local db_password
  local dispatchPort
  local status=0
  local before
  local recorded
  local baseline
  local dump
  local origin
  local redirect
  local expected
  local bound
  local now
  local read_end
  local read_ended

  previous=$(deployed_tag)
  origin=$(env_value DISPATCH_SERVER_URL)
  if [[ -z "$origin" ]]; then
    log "DISPATCH_SERVER_URL is missing from $COMPOSE_ENV; not deploying $sha"
    return 1
  fi
  databaseUrl=$(env_value DATABASE_URL)
  if [[ -z "$databaseUrl" ]]; then
    log "DATABASE_URL is missing from $COMPOSE_ENV; not deploying $sha"
    return 1
  fi
  if ! database_url_split "$databaseUrl"; then
    log "DATABASE_URL in $COMPOSE_ENV is not in the one form the deployer accepts, the only one in which it can keep the password off its database clients' process arguments; see packages/envoy/deploy/README.md, \"Auto-deployer\", for the form; not deploying $sha"
    return 1
  fi
  dispatchPort=$(env_value DISPATCH_PORT)
  dispatchPort="${dispatchPort:-8766}"
  # A deploy starts only where the pass leaves more than ROLLBACK_RESERVE: one with no more than
  # that left would start an image whose rollback the pass has no time for. Nothing has changed
  # yet, and the next pass starts with the whole deadline.
  deploy_bound "the deploy of $sha could start; $previous still serves" >/dev/null || return 1
  # The read before the deploy ends at SCHEMA_READ_DEADLINE, or where the deploy's own steps must
  # end (deploy_bound), whichever comes first.
  now=$(clock "the schema read could start; not deploying $sha") || return 1
  read_end=$((now + SCHEMA_READ_DEADLINE))
  read_ended="SCHEMA_READ_DEADLINE=${SCHEMA_READ_DEADLINE}s had passed"
  if ((read_end > pass_deadline - ROLLBACK_RESERVE)); then
    read_end=$((pass_deadline - ROLLBACK_RESERVE))
    read_ended="the pass had only the ${ROLLBACK_RESERVE}s it keeps for a rollback left"
  fi
  before=$(schema_applied "$read_end" deploy_bound) || {
    status=$?
    log "could not read the schema version ($(schema_failure "$status" "$before" "$read_ended")); not deploying $sha"
    return 1
  }
  # ENVOY_IMAGE_SCHEMA is <tag>:<value>, where <value> stands for the set of migrations the
  # deployed tag was running against when the deployer first tried to replace it (schema_applied,
  # above). Kept while that tag stays deployed, it is what a rollback to it needs: a retry after a
  # failed deploy that migrated starts from the moved schema. A value the deployer before this one
  # wrote is a bare max(version), which roll_back_or_stop compares the old way.
  recorded=$(env_value ENVOY_IMAGE_SCHEMA)
  if [[ -n "$recorded" && "${recorded%:*}" == "$previous" ]]; then
    baseline="${recorded##*:}"
    if [[ ! "$baseline" =~ ^[0-9]+-[0-9]+-[0-9a-f]{64}$ && ! "$baseline" =~ ^[0-9]+$ ]]; then
      log "ENVOY_IMAGE_SCHEMA in $COMPOSE_ENV is '$recorded', not <tag>:<max>-<count>-<digest>; not deploying $sha"
      return 1
    fi
  else
    baseline="$before"
    if ! set_env_value ENVOY_IMAGE_SCHEMA "$previous:$baseline"; then
      log "could not record the schema version $previous runs against; not deploying $sha"
      return 1
    fi
  fi
  dump="$BACKUPS/pre-${sha:0:12}-$(date -u +%Y%m%dT%H%M%SZ).dump"
  bound=$(deploy_bound "the backup could start; not deploying $sha") || return 1
  # shellcheck disable=SC2016 # $DATABASE_URL expands in the container's shell, from its environment
  pg_client "$bound" 'pg_dump -Fc "$DATABASE_URL"' >"$dump" || status=$?
  if ((status != 0)); then
    log "backup failed$(bound_note "$status"); not deploying $sha"
    rm -f "$dump"
    return 1
  fi
  prune_backups
  bound=$(deploy_bound "the pull of $sha could start; $previous still serves" "$PULL_BOUND") || return 1
  compose_with_tag "$sha" "$bound" pull -q dispatch || status=$?
  if ((status != 0)); then
    log "could not pull $sha$(bound_note "$status" "$PULL_BOUND"); $previous still serves"
    return 1
  fi
  bound=$(deploy_bound "the up of $sha could start; $previous still serves" "$UP_BOUND") || return 1
  # From the start of $sha until its result, FAILED_DEPLOY records it as stopped: a pass that ends
  # anywhere in between, by a failure no line of this script names, leaves the deployer stopped,
  # since what runs is then not known. The result rewrites the record: removed after a deploy that
  # succeeds, rolled-back after a rollback that restored $previous.
  if ! record_failed "$sha" stopped "$dump"; then
    log "not deploying $sha, since $FAILED_DEPLOY could not record its start; $previous still serves"
    return 1
  fi
  compose_with_tag "$sha" "$bound" up -d --no-deps dispatch || status=$?
  if ((status != 0)); then
    log "compose up failed for $sha$(bound_note "$status" "$UP_BOUND")"
    failed_deploy "$sha" "$previous" "$baseline" "$dispatchPort" "$dump"
    return
  fi
  if ! wait_for_health "$dispatchPort" deploy_bound "$sha"; then
    log "deployed $sha but /healthz did not answer 200 with NATS connected in the tries its health check had (at most $HEALTH_TRIES, ${HEALTH_INTERVAL} s apart)"
    failed_deploy "$sha" "$previous" "$baseline" "$dispatchPort" "$dump"
    return
  fi
  expected="$origin/auth/callback"
  if bound=$(deploy_bound "the origin check of $sha could start"); then
    redirect=$(oauth_redirect_uri "$origin" "$dispatchPort" "$bound") || redirect="(no answer: status $?)"
  else
    redirect="(no origin check: the pass had no time left for it)"
  fi
  if [[ "$redirect" != "$expected" ]]; then
    log "deployed $sha but the OAuth redirect_uri is '$redirect', expected '$expected' - login would break"
    failed_deploy "$sha" "$previous" "$baseline" "$dispatchPort" "$dump"
    return
  fi
  if ! sed -i -E "s|^ENVOY_IMAGE_TAG=.*|ENVOY_IMAGE_TAG=$sha|" "$COMPOSE_ENV"; then
    log "could not persist deployed tag $sha"
    failed_deploy "$sha" "$previous" "$baseline" "$dispatchPort" "$dump"
    return
  fi
  log "deployed $sha ($(jj -R "$REPO" log -r "$sha" --no-graph -T 'description.first_line()' | cut -c1-90)); login origin $origin verified; backup $dump"
  if [[ -e "$FAILED_DEPLOY" ]]; then
    if rm -f -- "$FAILED_DEPLOY"; then
      log "removed $FAILED_DEPLOY: $sha deployed, so the backups taken before the failed deploys it recorded may be pruned"
    else
      log "could not remove $FAILED_DEPLOY after $sha deployed; remove it by hand"
    fi
  fi
}

# jj_string <text> prints <text> as the body of a jj string literal, with its backslashes and
# double quotes escaped, so a tag a person wrote cannot end the literal it is put in.
jj_string() {
  local text="${1//\\/\\\\}"
  printf '%s' "${text//\"/\\\"}"
}

# commit_order <sha> <deployed> prints `older` when <sha> is <deployed> or an ancestor of it, and
# `newer` when it is not, read from the history of the repository the walk reads. A deployed tag
# that names no commit the repository knows (`local`, a release tag set by hand) has no history for
# <sha> to be older than, so the answer is `newer`: the newest published main replaces it, as it
# always has. Where jj answers neither, it prints nothing. Each name reaches jj inside a string
# literal (jj_string), the deployed one inside two, so the revset jj reads is the one written here
# whatever a tag holds.
commit_order() {
  local candidate deployed
  candidate=$(jj_string "$1")
  deployed=$(jj_string "$(jj_string "$2")")
  jj -R "$REPO" log --no-graph -r "present(\"$candidate\")" \
    -T "if(self.contained_in(\"::present(\\\"$deployed\\\")\"), \"older\", \"newer\") ++ \"\\n\""
}

# run_once is one pass, against the pass's deadline (PASS_DEADLINE, counted from here). It never
# deploys a commit older than the deployed one: the walk passes over a commit the registry says it
# holds no image of, and where that is the deployed commit itself (its image was removed, or IMAGE
# names another repository), the commit it reaches next is older. So the newest published commit
# is deployed only when the repository's history says it is not an ancestor of the deployed one;
# an ancestor is named on one line with the deployed commit and nothing is deployed, and an order
# jj cannot read fails the pass, since a deploy needs the answer `newer`, not the absence of
# `older`. It deploys nothing FAILED_DEPLOY says failed (skips_failed), and nothing at all while
# that file holds a stopped deploy: it then fails with 3.
run_once() {
  local sha status deployed order now stopped
  now=$(clock "the pass could start") || return 1
  pass_deadline=$((now + PASS_DEADLINE))
  stopped=$(stopped_commit) || stopped=
  if [[ -n "$stopped" ]]; then
    log "$FAILED_DEPLOY records that the deploy of $stopped left Dispatch for a person, and forward recovery is required: deploying nothing until a person deletes $FAILED_DEPLOY"
    return 3
  fi
  if sha=$(newest_published_main); then
    deployed=$(deployed_tag)
    [[ "$sha" != "$deployed" ]] || return 0
    order=$(commit_order "$sha" "$deployed") || order=
    case "$order" in
      newer)
        status=0
        skips_failed "$sha" || status=$?
        case "$status" in
          0) return 0 ;;
          2) return 1 ;;
        esac
        log "main has a newer published image: $sha (deployed $deployed)"
        deploy "$sha"
        ;;
      older)
        log "the newest published main commit, $sha, is an ancestor of the deployed $deployed; deploying nothing older over it"
        ;;
      *)
        log "could not read from $REPO whether $sha is older than the deployed $deployed; not deploying"
        return 1
        ;;
    esac
  else
    status=$?
    # 2: a read that proves nothing, which newest_published_main has logged.
    ((status == 1)) || return 1
    log "no published image among the last 30 main commits"
  fi
}

if [[ "$once" == true ]]; then
  run_once
  exit 0
fi
# An expansion error (an arithmetic one, say) aborts the whole top-level command it is in, the `if`
# above included, and bash goes on with the next command. A --once run must never reach the loop
# below, which starts a --once run of its own: each would start the next. So it ends here.
[[ "$once" != true ]] || exit 1

# The continuous loop runs each pass as a process of its own, a --once run, so a pass ends under
# set -e as one run does, and a failed pass ends that process alone: it has logged its one named
# line, the loop logs that it failed, and the next pass tries again after POLL. A `run_once ||`
# here would turn set -e off inside every pass. A pass whose rollback was refused or failed ends
# with 3, or leaves a stopped line in FAILED_DEPLOY, and the loop then stops: it starts nothing
# again, and a person recovers forward.
log "autodeploy started; deployed=$(deployed_tag)"
while true; do
  status=0
  "$BASH" "$0" --once || status=$?
  stopped=$(stopped_commit 2>/dev/null) || stopped=
  if ((status == 3)) || [[ -n "$stopped" ]]; then
    log "the pass ended with status $status after a deploy${stopped:+ of $stopped} whose rollback did not restore the previous image, and forward recovery is required (the line above names why; $FAILED_DEPLOY records the deploy): the deployer stops, and deploys nothing until a person deletes $FAILED_DEPLOY"
    exit 3
  fi
  ((status == 0)) || log "the pass failed (exit $status); the next pass starts in ${POLL}s"
  sleep "$POLL"
done
