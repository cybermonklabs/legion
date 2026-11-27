# shellcheck shell=bash
# The preamble the acceptance drivers share: the bearer each run reads, the directory it works in,
# the lines it prints for a check, the CLI it runs and the steps of the two looks at production.
# Sourced, never run. Each function that refuses prints why on stderr and exits the calling script
# with 2; one that names its usage calls the caller's own `usage`.
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

# The two looks at production, priorities-production.sh and priority-list-production.sh, share
# what follows: each reads Dispatch's access log in production's container logs, and only once
# production's fleet serves a commit that carries MERGE_SHA. A look that cannot measure prints why
# on stderr and exits 2, as a refusal above does; a look that waits prints its line on stdout and
# exits 3. `fleet_commit` reads the caller's `base` and `work`, `carries` its `merge` (MERGE_SHA,
# checked) and `work`, and `access_log` its `work` and LOGGING_PROJECT.

# cannot_measure WHY: the look cannot measure (exit 2).
cannot_measure() {
  printf '%s\n' "$1" >&2
  exit 2
}

# stamp SECONDS: the time SECONDS after the epoch, as a look prints a time.
# shellcheck disable=SC2016 # the jq filter names a jq variable, not a shell one
stamp() { jq -rn --argjson s "$1" '$s | todateiso8601'; }

# epoch NAME: sets `epoch_s` to the time the variable NAME holds, in seconds since the epoch. A
# time a look does not print cannot be measured against.
# shellcheck disable=SC2016 # the jq filter names a jq variable, not a shell one
# shellcheck disable=SC2034 # $epoch_s is this function's answer, read by its caller
epoch() {
  local pattern='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'
  [[ "${!1}" =~ $pattern ]] ||
    cannot_measure "$1 is ${!1}, not a time as this script prints one (YYYY-MM-DDTHH:MM:SSZ)"
  epoch_s="$(jq -rn --arg t "${!1}" '$t | fromdateiso8601 | floor')"
}

# p90 FILE: the p90 of the numbers FILE holds, one a line: the value at rank ceil(0.9 n) of them in
# ascending order.
p90() {
  local -a sorted
  mapfile -t sorted < <(sort -n "$1")
  local rank=$(((9 * ${#sorted[@]} + 9) / 10))
  printf '%s' "${sorted[rank - 1]}"
}

# fleet_commit CODE: sets `fleet` to /healthz's `fleet_commit`, the commit every live server
# process serves, from the answer of GET $base/healthz the caller wrote to $work/healthz, CODE its
# status (000 where none came) and curl's stderr in $work/curl. No answer ends the look, which
# cannot measure; an answer that names no one commit ends it too, and the look waits.
fleet_commit() {
  [[ "$1" =~ ^[0-9]{3}$ && "$1" != 000 ]] ||
    cannot_measure "GET $base/healthz got no answer: $(head -c 300 "$work/curl" | tr '\n' ' ')"
  fleet="$(jq -r '.fleet_commit // empty' "$work/healthz" 2>/dev/null)" || fleet=''
  if [[ ! "$fleet" =~ ^[0-9a-f]{40}$ ]]; then
    printf "production's fleet serves no one commit: GET /healthz answered HTTP %s with fleet_commit %s\n" \
      "$1" "${fleet:-null}"
    exit 3
  fi
}

# carries COMMIT: GitHub's compare of MERGE_SHA with COMMIT. Sets `compared`, its status, and
# `carried`, yes where COMMIT carries MERGE_SHA (`identical` or `ahead`) and no where it does not
# (`behind` or `diverged`). A compare GitHub does not answer ends the look, which waits.
# shellcheck disable=SC2034 # $carried is this function's answer, read by its caller
# shellcheck disable=SC2154 # merge is set by the look that sources this file
carries() {
  if ! compared="$(gh api "repos/cybermonklabs/legion/compare/$merge...$1" --jq .status \
    2>"$work/gh" </dev/null)"; then
    printf 'GitHub did not answer whether %s carries %s: %s\n' "$1" "$merge" \
      "$(head -c 300 "$work/gh" | tr '\n' ' ')"
    exit 3
  fi
  case "$compared" in
    identical | ahead) carried=yes ;;
    behind | diverged) carried=no ;;
    *)
      printf "GitHub's compare of %s with %s answered %s, which says neither way whether it carries it\n" \
        "$merge" "$1" "${compared:-nothing}"
      exit 3
      ;;
  esac
}

# access_log TERM...: the lines of Dispatch's access log in production's container logs that each
# TERM of a Logging filter matches as well, from `gcloud logging read` into $work/log. A read that
# fails ends the look, which cannot measure.
access_log() {
  local filter
  printf -v filter '%s AND ' 'resource.type="k8s_container"' \
    'resource.labels.namespace_name="dispatch"' 'textPayload:"msg=\"dispatch: request\""' "$@"
  gcloud logging read "${filter% AND }" --project "$LOGGING_PROJECT" --format='value(textPayload)' \
    >"$work/log" 2>"$work/gcloud" </dev/null ||
    cannot_measure "gcloud logging read failed: $(head -c 600 "$work/gcloud" | tr '\n' ' ')"
}
