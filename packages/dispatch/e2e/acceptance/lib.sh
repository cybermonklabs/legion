# shellcheck shell=bash
# The preamble the acceptance drivers share: the bearer each run reads, the directory it works in,
# the lines it prints for a check and the CLI it runs. Sourced, never run. Each function that
# refuses prints why on stderr and exits the calling script with 2; one that names its usage calls
# the caller's own `usage`.
#
# The functions below read what the run sets: `token` (read_bearer), `work` (work_dir), and
# `bun_bin`, `saved_path`, `saved_tmpdir` and the `home` and `state` directories under $work
# (cli_env). `cli` needs three more, which each driver sets itself and nothing here sets: the
# `base` URL it talks to, the driver's own `session` and its `cli_entry`.
#
# The check vocabulary below (body, want_status, expect, query) reads the caller's own `work`,
# `status`, `base` and `database_url`, which every driver sets after sourcing this file and before
# its first check: a function resolves them when it runs, not when it is read. Those four are
# named below so a reader of this file - and shellcheck, which reads one file at a time - sees what
# it reads of its caller. The caller assigns each before its first check, and may make its own
# readonly afterwards.
: "${work:=}" "${status:=}" "${base:=}" "${database_url:=}"

# require_env NAME…: refuses the first named environment variable that is unset or empty.
require_env() {
  local variable
  for variable in "$@"; do
    if [[ -z "${!variable:-}" ]]; then
      printf 'required environment variable %s is unset\n' "$variable" >&2
      usage
      exit 2
    fi
  done
}

# loopback_database_url URL: succeeds only for a single loopback postgres URL authority.
# Connection overrides and escaped parameter names are refused, and so is any `@` after the
# authority: libpq reads user credentials up to the first `@` or `/`, not stopping at `?`, so a query
# value holding `@` can name the host, and an override can replace even an explicit loopback one.
loopback_database_url() {
  local url="$1" authority host query parameter name
  [[ "$url" == postgres://* || "$url" == postgresql://* ]] || return 1
  [[ "$url" != *\#* ]] || return 1
  authority="${url#*://}"
  authority="${authority%%[/?]*}"
  [[ "${url#*://"$authority"}" != *@* ]] || return 1
  host="${authority#*@}"
  [[ "$host" =~ ^(127\.0\.0\.1|localhost|\[::1\])(:[0-9]+)?$ ]] || return 1
  if [[ "$url" == *\?* ]]; then
    query="${url#*\?}"
    while :; do
      parameter="${query%%&*}"
      name="${parameter%%=*}"
      case "$name" in host | hostaddr | service | *%*) return 1 ;; esac
      [[ "$query" == *'&'* ]] || break
      query="${query#*&}"
    done
  fi
}

# require_commands COMMAND…: refuses the first named command that is not on PATH.
require_commands() {
  local command
  for command in "$@"; do
    command -v "$command" >/dev/null || {
      printf 'required command %s is unavailable\n' "$command" >&2
      exit 2
    }
  done
}

# read_bearer sets the caller's unexported `token` to the agent bearer, DISPATCH_TOKEN or else
# DISPATCH_TOKEN_FILE's contents trimmed of surrounding whitespace, and refuses an unreadable file
# or an empty bearer. It never prints the bearer, and it unsets both variables, so no child process
# the caller starts inherits the bearer from its environment.
read_bearer() {
  if [[ -n "${DISPATCH_TOKEN:-}" ]]; then
    token="$DISPATCH_TOKEN"
  elif [[ -n "${DISPATCH_TOKEN_FILE:-}" ]]; then
    [[ -r "$DISPATCH_TOKEN_FILE" ]] || {
      printf 'DISPATCH_TOKEN_FILE names %s, which cannot be read\n' "$DISPATCH_TOKEN_FILE" >&2
      exit 2
    }
    token="$(<"$DISPATCH_TOKEN_FILE")"
    token="${token#"${token%%[![:space:]]*}"}"
    token="${token%"${token##*[![:space:]]}"}"
  else
    printf 'set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE to an agent bearer\n' >&2
    usage
    exit 2
  fi
  [[ -n "$token" ]] || {
    printf 'the agent bearer is empty\n' >&2
    exit 2
  }
  unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
}

# work_dir sets the caller's `work` to a directory of its own, which the caller removes. The umask
# is the run's own, so nothing it writes there is readable by another account.
work_dir() {
  umask 077
  work="$(mktemp -d)"
  readonly work
}

# cli_env saves what `cli` needs from the environment it scrubs — the caller's `bun_bin`,
# `saved_path` and `saved_tmpdir` — and makes the `home` and `state` directories under $work that
# `cli` points the CLI at. A driver that runs the CLI calls it after work_dir.
cli_env() {
  bun_bin="$(command -v bun)"
  readonly bun_bin
  readonly saved_path="$PATH"
  readonly saved_tmpdir="${TMPDIR:-}"
  mkdir "$work/home" "$work/state"
}

pass() { printf 'PASS: %s %s\n' "$1" "$2"; }
fail() {
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
  exit 1
}
skip() { printf 'SKIP: %s %s\n' "$1" "$2"; }

# body: the head of the last answer's body, for a failure message.
body() { head -c 600 "$work/body"; }

# want_status CODE SECTION WHAT: the last answer's status is CODE.
want_status() {
  [[ "$status" == "$1" ]] || fail "$2" "$3" "HTTP $status, want $1: $(body)"
}

# expect SECTION WHAT WANT JQ-ARGS...: jq -cS JQ-ARGS over the last answer's body prints WANT.
expect() {
  local section="$1" what="$2" want="$3" got
  shift 3
  got="$(jq -cS "$@" "$work/body" 2>&1)" || fail "$section" "$what" "jq: $got; body: $(body)"
  [[ "$got" == "$want" ]] || fail "$section" "$what" "got $got, want $want"
  pass "$section" "$what"
}

# q VALUE: VALUE as one path or query component of a URL.
# shellcheck disable=SC2016 # the jq filter names a jq variable, not a shell one
q() { jq -rn --arg v "$1" '$v|@uri'; }

# query SECTION WHAT SQL: one value of one row, read through DATABASE_URL into $answer. psql runs
# without PGHOSTADDR, which libpq would otherwise connect to in place of the DSN's host, and without
# PGSERVICE and PGSERVICEFILE, whose service entry can carry a hostaddr of its own: the DSN alone
# names the server, as the loopback guard read it.
# shellcheck disable=SC2034 # $answer is this function's answer, read by its caller
query() {
  local section="$1" what="$2" sql="$3"
  answer="$(env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE psql "$database_url" -Atqc "$sql" 2>"$work/psql")" ||
    fail "$section" "$what" "psql: $(<"$work/psql")"
}

# observe_horizon SECTION: the observer's half of an observation of the event horizon as
# events.Horizon takes one (horizonObserveSQL): a statement in a transaction of its own, which
# draws a transaction id, the observer's X, as it runs. Every transaction id drawn before X must
# end before the observation is proven, so X decides which transactions hold it back. It sets
# $answer to X and the database's clock as X was drawn, in seconds since the epoch, separated by a
# space: one argument of own_horizon_hold.
observe_horizon() {
  query "$1" "observe the event horizon" \
    "select pg_current_xact_id()::text || ' ' || extract(epoch from clock_timestamp())"
}

# own_horizon_hold SECTION OBSERVATION...: judges a hold against the newest of the OBSERVATIONs,
# each one observe_horizon answered, that has waited 5 s (horizontest.OwnHoldBound). It sets
# $answer to the oldest transaction of this run's database that has held that observation back,
# named with its pid, its client, its start and its query, or to '' where none has; and
# $observation_age to how long that observation has waited, in seconds, or to '' where none of them
# has waited 5 s. One statement reads both, so the age is the one the judgement read. Each
# observation's time goes into that statement as written: observe_horizon writes a number, and
# node-projects-horizon-hold.test.ts writes an expression on the statement's own clock, so the age
# it judges is one no scheduling can move.
#
# The horizon proves an observation only once no transaction id below the observer's is running
# (events/horizon.go, "Safe horizon"). So a transaction holds an observation back only while it
# holds an id drawn before the observer's, and one that drew its first id after the observation
# does not hold that one back, however long ago it began. A transaction that holds it back has held
# it for the shorter of the observation's age and the transaction's own, as horizontest's heldFor
# reads a stall warning, both read on the database's clock. The newest observation that has waited
# 5 s is the one to judge by: a transaction holding back an older one holds back that one too,
# since its id is below both observers'. Two ids are compared by their age(), which one statement
# reads against one counter. The holders are read as events.StallHolderSQL reads them, a prepared
# transaction's as well as a backend's, the oldest id first. pg_stat_activity shows a session's
# xact_start only to a superuser, a member of pg_read_all_stats or a member of the session's own
# role, and a holder this role cannot date is not named.
# shellcheck disable=SC2034 # $observation_age is this function's answer too, read by its caller
own_horizon_hold() {
  local section="$1" observation observer observed_at given=""
  shift
  for observation; do
    read -r observer observed_at <<<"$observation"
    given+="${given:+, }('$observer'::xid8, $observed_at)"
  done
  query "$section" "read the oldest transaction of this run's database that holds the event horizon back" "
    with judged as (
      select observer, now() - to_timestamp(observed_at) as waited
      from (values $given) as observation (observer, observed_at)
      where now() - to_timestamp(observed_at) >= interval '5 seconds'
      order by waited limit 1
    )
    select coalesce((select extract(epoch from waited)::text from judged), '') || '|' || coalesce((select name from (
      select format('backend %s (application %s), open since %s: %s', pid, coalesce(application_name, ''), xact_start,
          left(coalesce(query, ''), 80)) as name, xact_start as started, age(backend_xid) as age
      from pg_stat_activity
      where backend_xid is not null and pid <> pg_backend_pid() and datname = current_database()
      union all
      select format('prepared transaction %s, prepared at %s', gid, prepared), prepared, age(transaction)
      from pg_prepared_xacts where database = current_database()
    ) holder, judged
    where holder.age > age(judged.observer::xid) and now() - holder.started >= interval '5 seconds'
    order by holder.age desc limit 1), '')"
  observation_age="${answer%%|*}"
  answer="${answer#*|}"
}

# start_horizon_watch SECTION: starts the watch of the event horizon that watch_horizon carries
# on, and returns once the watch's first observation has waited 5 s. Until then it polls as
# watch_horizon polls, about once a second, so the watch holds an observation of each second from
# its start on. own_horizon_hold names a transaction only against an observer that drew its id
# after the transaction drew its own, and only once that observation has waited 5 s. So a watch
# begun at the moment from which a hold must be named would name a transaction that drew its id
# before that moment only once it had held the id 5 s more, and never one that ended sooner,
# though it may have held the horizon back far longer. A driver starts the watch 5 s before that
# moment instead: a transaction that drew its id before the watch began and still holds it when
# this returns fails the section here, and one that drew its id later is judged against the
# observations of the seconds after it.
start_horizon_watch() {
  horizon_observations=()
  watch_horizon "$1"
  until [[ -n "$observation_age" ]]; do
    sleep 1
    watch_horizon "$1"
  done
}

# watch_horizon SECTION: one poll of the watch start_horizon_watch began. It observes the horizon
# (observe_horizon), keeps the observation in $horizon_observations with every one the watch took
# before it, judges a hold against them all (own_horizon_hold), and fails the section naming the
# transaction that holds the horizon back. It sets $observation_age as own_horizon_hold does. It
# refuses to poll a watch that was never started: its judgement would then have no observation
# older than its own polls, the defect start_horizon_watch's comment names.
watch_horizon() {
  declare -p horizon_observations >/dev/null 2>&1 || {
    printf 'watch_horizon polls only a watch that start_horizon_watch began\n' >&2
    exit 2
  }
  observe_horizon "$1"
  horizon_observations+=("$answer")
  own_horizon_hold "$1" "${horizon_observations[@]}"
  [[ -z "$answer" ]] ||
    fail "$1" "no transaction of this run's database holds the event horizon back" \
      "$answer has held back an observation of the horizon that has waited $observation_age s on it, 5 s or more, holding a transaction id drawn before that observer's in this run's database, which holds the server's event readers back"
}

# The real `dispatch` CLI as an agent session runs it, in an environment holding only what this
# driver names: nothing a calling shell exported (its Dispatch URL, token file or Envoy config)
# reaches it. It reads the caller's $base, $token, $session, $work, $cli_entry and what cli_env
# saved.
cli() {
  (
    set +x
    local name
    local -a exported
    mapfile -t exported < <(compgen -e)
    for name in "${exported[@]}"; do
      unset "$name" 2>/dev/null || true
    done
    export PATH="$saved_path" HOME="$work/home" DISPATCH_STATE_DIR="$work/state"
    if [[ -n "$saved_tmpdir" ]]; then
      export TMPDIR="$saved_tmpdir"
    fi
    # shellcheck disable=SC2154 # session is set by the driver that sources this file
    export DISPATCH_HOST=omp DISPATCH_SESSION_ID="$session" DISPATCH_URL="$base"
    export DISPATCH_TOKEN="$token"
    # shellcheck disable=SC2154 # cli_entry is set by the driver that sources this file
    exec "$bun_bin" "$cli_entry" "$@"
  )
}

# run SECTION WHAT ARGS...: runs the CLI with ARGS, its output in $work/out; a non-zero exit fails.
run() {
  local section="$1" what="$2"
  shift 2
  cli "$@" >"$work/out" 2>&1 || fail "$section" "$what" "dispatch $1 exited $?: $(<"$work/out")"
}
