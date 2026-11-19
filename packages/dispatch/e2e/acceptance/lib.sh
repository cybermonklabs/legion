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
# its first check: a function resolves them when it runs, not when it is read. The HTTP vocabulary
# at the end reads `driver`, the script's name its first PASS line names, `session`, the session
# its bearer writes act for, `stamp`, the run's own suffix, `login`, the person person_send sends
# as, and `key`, the issue invoke writes a lens on; checked_issue adds to `keys`, the issues the
# caller closes when it exits. Those are named below so a reader of this file - and shellcheck,
# which reads one file at a time - sees what it reads of its caller. The caller assigns each it
# uses before its first check, and may make its own readonly afterwards.
: "${work:=}" "${status:=}" "${base:=}" "${database_url:=}"
: "${driver:=}" "${session:=}" "${stamp:=}" "${login:=}" "${key:=}" "${cli_entry:=}"

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

# require_server_bin sets the caller's readonly `server_bin` to the envoy-dispatch binary the run
# serves or runs, DISPATCH_E2E_SERVER_BIN made absolute or else $TMPDIR/bin/envoy-dispatch, this
# head's build, and refuses a path that is not an executable file.
require_server_bin() {
  # shellcheck disable=SC2031 # the TMPDIR `cli` exports is its own; this one is the caller's
  server_bin="${DISPATCH_E2E_SERVER_BIN:-${TMPDIR:-/tmp}/bin/envoy-dispatch}"
  [[ "$server_bin" == /* ]] || server_bin="$PWD/$server_bin"
  readonly server_bin
  [[ -x "$server_bin" ]] || {
    printf 'DISPATCH_E2E_SERVER_BIN names %s, which is not an executable file\n' "$server_bin" >&2
    usage
    exit 2
  }
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

# psql_run SECTION WHAT ARGS…: psql against DATABASE_URL with ARGS, stopping at the first error, its
# output in $work/psql.out; it runs without PGHOSTADDR, as query does. Its stdin is the caller's, so
# SQL a heredoc gives it may name a psql variable ARGS binds (-v NAME=VALUE, read as :'NAME'),
# which psql substitutes there and not in a -c command.
psql_run() {
  local section="$1" what="$2"
  shift 2
  env -u PGHOSTADDR psql "$database_url" -X -q -At -v ON_ERROR_STOP=1 "$@" >"$work/psql.out" 2>&1 ||
    fail "$section" "$what" "psql: $(<"$work/psql.out")"
}

# with_vars SECTION WHAT SQL NAME=VALUE…: runs SQL with each NAME bound as a psql variable (read as
# :'NAME'), its one value in $answer. The SQL is read from stdin, where psql substitutes a variable.
# shellcheck disable=SC2034 # $answer is this function's answer, read by its caller
with_vars() {
  local section="$1" what="$2" sql="$3" pair
  shift 3
  local -a vars=()
  for pair in "$@"; do
    vars+=(-v "$pair")
  done
  answer="$(env -u PGHOSTADDR psql "$database_url" -X -q -At -v ON_ERROR_STOP=1 "${vars[@]}" <<<"$sql" 2>"$work/psql")" ||
    fail "$section" "$what" "psql: $(<"$work/psql")"
}

# want_value SECTION WHAT WANT SQL: the one value SQL reads is WANT.
want_value() {
  query "$1" "$2" "$4"
  [[ "$answer" == "$3" ]] || fail "$1" "$2" "read '$answer', want '$3' (from: $4)"
  pass "$1" "$2"
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

# --- the HTTP vocabulary of a driver that talks to a Dispatch server ----------------------------
#
# Each request goes to the caller's $base and leaves the answer's body in $work/body, its HTTP
# status in $status and its Content-Type in $content_type. The agent bearer is the header line the
# caller writes to $work/auth once read_bearer has read it, and a person's token the line
# mint_token writes, so no bearer reaches an argument list. A driver that defines one of these
# names itself keeps its own.

# The ten lens kinds dispatch://INFRA-1297 records, which the catalogue's migration seeds.
seeded_kinds='["best-practice","boring-solution","botec","conway","first-principles","inversion","pre-mortem","reversibility","second-order","trade-off"]'

# The id of the personal token mint_token minted for each person, by login, which revoke_tokens
# revokes.
declare -A minted=()

# request AS METHOD PATH [JSON [RUN]]: sends METHOD PATH as AS: `agent`, the bearer in $work/auth;
# `cookie:<login>`, the session cookie sign_in left in $work/jar.<login>, with the server's origin,
# which the server requires of every write a cookie authenticates (enforceCookieOrigin); or
# `token:<login>`, the personal token mint_token left in $work/auth.<login>. RUN names a run of the
# command bar in the Dispatch-Going-Run header (contracts.GoingRunHeader). A request is given up
# after $max_time seconds, 60 unless the caller sets fewer.
# shellcheck disable=SC2034 # $content_type is this function's answer, read by its caller
request() {
  local -a auth data=() run=()
  local host answer
  case "$1" in
    cookie:*)
      host="${base#*://}"
      auth=(-b "$work/jar.${1#cookie:}" -H "Origin: ${base%%://*}://${host%%/*}")
      ;;
    token:*) auth=(-H "@$work/auth.${1#token:}") ;;
    *) auth=(-H "@$work/auth") ;;
  esac
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  [[ $# -lt 5 ]] || run=(-H "Dispatch-Going-Run: $5")
  if answer="$(curl -sS --max-time "${max_time:-60}" -X "$2" -o "$work/body" -w '%{http_code} %{content_type}' \
    "${auth[@]}" "${data[@]}" "${run[@]}" "$base$3" 2>"$work/curl")"; then
    status="${answer%% *}" content_type="${answer#* }"
  else
    status="curl failed: $(<"$work/curl")" content_type=""
  fi
}

# send METHOD PATH [JSON]: request as the agent bearer.
send() { request agent "$@"; }
# person_send METHOD PATH [JSON]: request as the person $login names, whom sign_in signed in.
person_send() { request "cookie:$login" "$@"; }

# actor: the session the caller's writes act for.
actor() { jq -cn --arg s "$session" '{kind: "session", id: $s}'; }

# close_issue KEY: the agent bearer closes the issue KEY, from a cleanup trap: a refusal is printed
# on stderr and stops nothing.
close_issue() {
  send PATCH "/api/v1/issues/$1" "$(jq -cn --argjson a "$(actor)" '{status: "done", actor: $a}')"
  [[ "$status" == 200 ]] ||
    printf '%s: could not close %s: HTTP %s: %s\n' "$driver" "$1" "$status" "$(body)" >&2
}

# sign_in SECTION LOGIN [MORE]: signs LOGIN in through the server's dev sign-in route,
# /auth/_dev/signin, keeping the session cookie in $work/jar.LOGIN. Its line is `PASS: SECTION
# signed in as LOGIN`, with MORE after it where given.
sign_in() {
  status="$(curl -sS --max-time 60 -o "$work/body" -w '%{http_code}' -c "$work/jar.$2" \
    "$base/auth/_dev/signin?login=$(q "$2")" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
  want_status 302 "$1" "dev sign-in as $2"
  grep -q $'\tdsession\t' "$work/jar.$2" || fail "$1" "dev sign-in as $2" "the answer set no session cookie"
  pass "$1" "signed in as $2${3-}"
}

# mint_token LOGIN: LOGIN's cookie mints a personal token with a name and no scopes, so it holds
# that person's own scopes (POST /api/v1/me/agent-tokens, as e2e/api.ts's mintAgentToken does). Its
# plaintext, the answer's `token`, goes to $work/auth.LOGIN as the header request sends, and its id
# to minted[LOGIN]. The answer, which holds the plaintext, is cleared once read, so no failure
# message prints it.
mint_token() {
  local plaintext scopes
  request "cookie:$1" POST /api/v1/me/agent-tokens "$(jq -cn --arg n "${driver%.sh} $1 $stamp" '{name: $n}')"
  want_status 201 setup "mint a personal token for $1"
  plaintext="$(jq -er '.token | select(type == "string" and . != "")' "$work/body")" ||
    fail setup "mint a personal token for $1" "the answer holds no token"
  minted[$1]="$(jq -er '.id' "$work/body")" || fail setup "mint a personal token for $1" "the answer holds no id"
  scopes="$(jq -c '.scopes' "$work/body")"
  printf 'Authorization: Bearer %s\n' "$plaintext" >"$work/auth.$1"
  : >"$work/body"
  pass setup "minted a personal token for $1 with no scopes asked for; it holds $scopes"
}

# revoke_tokens: each person's cookie revokes the token mint_token minted for them, from a cleanup
# trap: a refusal is printed on stderr and stops nothing.
revoke_tokens() {
  local person
  for person in "${!minted[@]}"; do
    request "cookie:$person" DELETE "/api/v1/me/agent-tokens/$(q "${minted[$person]}")"
    [[ "$status" == 2?? ]] ||
      printf '%s: could not revoke the token %s minted: HTTP %s: %s\n' "$driver" "$person" "$status" "$(body)" >&2
  done
}

# invoke SECTION WHAT AS BODY: AS invokes the lens BODY names on the caller's issue $key (POST
# /api/v1/issues/{key}/lenses), and the answer is 201. It sets $lens to the lens's address and
# $lens_id to its uuid, and keeps the answer in $work/invoked.
invoke() {
  request "$3" POST "/api/v1/issues/$key/lenses" "$4"
  want_status 201 "$1" "$2"
  cp "$work/body" "$work/invoked"
  lens="$(jq -er '.address' "$work/invoked")" || fail "$1" "$2" "no address: $(body)"
  lens_id="$(jq -er '.id' "$work/invoked")" || fail "$1" "$2" "no id: $(body)"
  [[ "$lens" == "dispatch://lens/$lens_id" ]] ||
    fail "$1" "$2" "the lens answers the address $lens, want dispatch://lens/$lens_id"
  pass "$1" "$2 ($lens)"
}

# seeded_catalogue: the agent bearer reads the lens catalogue (GET /api/v1/schema/blocks), kept in
# $work/catalogue.json, and it holds the ten kinds of $seeded_kinds. Its line, read before any lens
# is written, is `PASS: <driver> the catalogue holds its ten kinds`.
seeded_catalogue() {
  local held
  send GET /api/v1/schema/blocks
  want_status 200 "$driver" "read the catalogue"
  cp "$work/body" "$work/catalogue.json"
  held="$(jq -cS --argjson want "$seeded_kinds" '[.lens_kinds[].id] as $ids | [$want[] | select(. as $id | $ids | index($id))]' \
    "$work/catalogue.json")" || fail "$driver" "read the catalogue" "unreadable: $(body)"
  [[ "$held" == "$seeded_kinds" ]] ||
    fail "$driver" "the catalogue holds its ten kinds" "it holds $held of $seeded_kinds"
  pass "$driver" "the catalogue holds its ten kinds"
}

# checked_issue SECTION PROJECT TITLE: the agent bearer creates an issue in PROJECT titled TITLE,
# adds it to the caller's `keys`, the issues it closes when it exits, and writes the issue's
# verification document holding one check, c1, which it reads back among that document's blocks as
# a check. It sets $checked_key to the issue, $checked_check to the check's address and
# $checked_assignee to the person the issue went to: the project's default assignee, since the
# bearer acts for no person, and so a person who has signed in.
# shellcheck disable=SC2034 # $checked_assignee is this function's answer, read by its caller
checked_issue() {
  send POST /api/v1/issues "$(jq -cn --arg p "$2" --arg t "$3" --argjson a "$(actor)" \
    '{project: $p, title: $t, spec: "An issue whose verification document holds one check.", force: true, actor: $a}')"
  want_status 201 "$1" "create the issue \"$3\" in $2"
  checked_key="$(jq -er '.key' "$work/body")" || fail "$1" "create the issue \"$3\" in $2" "no key: $(body)"
  keys+=("$checked_key")
  checked_assignee="$(jq -r '.assignee // empty' "$work/body")"
  send POST "/api/v1/issues/$checked_key/artifacts" "$(jq -cn --arg k "$checked_key" --argjson a "$(actor)" \
    '{name: "verification.md", content: "How \($k) is proved.\n\n:::check{#c1}\nThe one check of \($k).\n:::\n", actor: $a}')"
  want_status 201 "$1" "write the verification document of $checked_key"
  checked_check="dispatch://$checked_key/artifact/verification-md#b-c1"
  send GET "/api/v1/issues/$checked_key/artifacts/verification-md/blocks"
  want_status 200 "$1" "read the blocks of the verification document of $checked_key"
  expect "$1" "created $checked_key in $2, whose verification document holds the check $checked_check" '["check"]' \
    '[.[] | select(.id == "c1") | .type]'
}

# another_project SECTION ASSIGNEE: the agent bearer creates a project of the run's own, whose
# default assignee is ASSIGNEE, a person who has signed in: the bearer acts for no person, so the
# create names one (createProject in packages/envoy/internal/dispatch/api/projects.go). It sets
# $other_project to the project's key, an X and nine letters spelling the run's second, modulo
# 100 000, and the last four digits of its process id. No route removes a project, so it stays on
# the server.
another_project() {
  other_project="X$(printf '%09d' "$((($(date +%s) % 100000) * 10000 + $$ % 10000))" | tr 0-9 A-J)"
  send POST /api/v1/projects "$(jq -cn --arg k "$other_project" --arg d "$2" --arg n "${driver%.sh} $stamp" \
    --argjson a "$(actor)" '{key: $k, name: "Another project of \($n)", default_assignee: $d, actor: $a}')"
  want_status 201 "$1" "create the project $other_project, whose default assignee is $2"
  pass "$1" "created the project $other_project, whose default assignee is $2"
}
