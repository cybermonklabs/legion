# shellcheck shell=bash
# shellcheck disable=SC2154,SC2034 # the leg that sources this file sets what it reads, and reads what it sets
# The browser gate's grants control (row grants of outside-agent-tailnet.sh): a full member's own
# list of credential grants, GET /api/v1/credential-grants?approver=me, read with the full member's
# personal token (OUTSIDE_SWEEP_FULL_TOKEN_FILE, the person scope). outside-agent-tailnet.sh sources
# this file from beside it, after outside-agent-lib.sh, and the functions read its `work`, `tab`,
# `person` and `token_header` and set `grants_before`, `grants_probe` and `grants_problem` (and
# `whoami_named` and `whoami_problem`, whoami_person's own).
#
# A grant every signed-in person is listed (one the model judge approved, whose approver is
# `anyone`, or one a pod that acts for no person got without asking, with no approver and no
# operator) is what the test person's own list must not hold. The probe grant the runner revokes is
# the first grant of the list with no approver and no operator, which no person may end at the
# broker whatever Dispatch relays, and whose approver and operator name neither the full member
# nor the test person: a revoke Dispatch lets through ends nothing, and nothing here makes or ends
# a grant.
#
# Run alone, `bash outside-agent-grants.sh`, it is the browser gate's preflight, to run before a
# live gate is asked for: with OUTSIDE_SWEEP_FULL_TOKEN_FILE, OUTSIDE_TAILNET_DISPATCH_URL and
# OUTSIDE_PERSON (and OUTSIDE_ALLOW_HTTP=1 for a local stack), it reads the list as the gate reads
# it before its run and prints `grant <id>`, the probe grant the gate would pass (exit 0); `no grant
# with no approver and no operator` (exit 1); or `UNKNOWN: <why>` (exit 2). It writes nothing but
# its own scratch directory, which it removes.

# full_token_get PATH OUT: GET PATH at Dispatch's private origin with the full member's personal
# token, which reaches curl in a header file of mode 0600 alone, never on a command line; prints the
# answer's status, 000 when none came, and leaves its body in OUT.
full_token_get() {
  local status
  status=$(curl -q -sS -g --noproxy '*' --max-time 15 -o "$2" -w '%{http_code}' -H "@$token_header" \
    "$OUTSIDE_TAILNET_DISPATCH_URL$1" 2>"$work/token.err" </dev/null)
  [ "${#status}" -eq 3 ] || status=000
  printf '%s' "$status"
}

# token_setup: writes the full member's token into token_header, mode 0600; prints why it could
# not, or nothing.
token_setup() {
  local problem
  problem=$(lacking OUTSIDE_SWEEP_FULL_TOKEN_FILE OUTSIDE_TAILNET_DISPATCH_URL)
  [ -n "$problem" ] || [ -r "$OUTSIDE_SWEEP_FULL_TOKEN_FILE" ] ||
    problem="OUTSIDE_SWEEP_FULL_TOKEN_FILE names $OUTSIDE_SWEEP_FULL_TOKEN_FILE, which cannot be read"
  [ -n "$problem" ] || [ -n "$work" ] || problem='mktemp -d made no scratch directory'
  if [ -z "$problem" ]; then
    (umask 077 && printf 'Authorization: Bearer %s\n' "$(tr -d '\r\n' <"$OUTSIDE_SWEEP_FULL_TOKEN_FILE")" >"$token_header") ||
      problem='the token header file could not be written'
  fi
  printf '%s' "$problem"
}

# grant_rows FILE: each grant of the credential-grants answer in FILE, one a line,
# `<grant_id><TAB><granted><TAB><approver><TAB><operator>`, the approver `null` where the grant
# names none; nothing for an answer that holds no `grants` list. Dispatch relays the broker's
# compact JSON, in which each grant is an object that opens with "grant_id" and holds one
# "operator", its enrollment's.
grant_rows() {
  local rest chunk
  rest=$(tr -d ' \n' <"$1" 2>/dev/null)
  case $rest in *'"grants":['*) ;; *) return 0 ;; esac
  rest=${rest#*\"grants\":[}
  while :; do
    case $rest in *'{"grant_id":'*) ;; *) break ;; esac
    rest=${rest#*\{\"grant_id\":}
    chunk="\"grant_id\":${rest%%\{\"grant_id\":*}"
    printf '%s\t%s\t%s\t%s\n' "$(json_field grant_id "$chunk")" "$(json_field granted "$chunk")" \
      "$(json_field approver "$chunk")" "$(json_field operator "$chunk")"
  done
}

# everyone_grants ROWS: the ids of the grant_rows lines in the file ROWS that every signed-in person
# is listed, one a line.
everyone_grants() {
  local id granted approver operator
  while IFS="$tab" read -r id granted approver operator || [ -n "$id" ]; do
    case $approver:$granted:$operator in
      anyone:*:* | null:automatic:) printf '%s\n' "$id" ;;
    esac
  done <"$1"
}

# grants_read LABEL: the full member's list into $work/grants-LABEL.json, its rows into
# $work/grants-LABEL.rows; prints why it was not read, or nothing.
grants_read() {
  local status
  status=$(full_token_get '/api/v1/credential-grants?approver=me' "$work/grants-$1.json")
  if [ "$status" != 200 ]; then
    printf 'the full member'"'"'s credential-grants list answered %s %s the run: %s' "$status" "$1" "$(text_of "$work/grants-$1.json")"
    return
  fi
  grant_rows "$work/grants-$1.json" >"$work/grants-$1.rows"
}

# fold_login LOGIN: LOGIN as Dispatch and the broker fold a person's login before they store or
# compare it (fold.Login: the blanks around it cut, the letters A-Z lowered and no other byte
# changed): Dispatch's whoami answers its login so folded (model.CanonicalLogin), and the broker
# so folds a grant's approver and its enrollment's operator (record.CanonicalLogin). So the full
# member, the test person and a grant's two fields are held to each other in that one namespace.
fold_login() {
  local login=$1
  login=${login#"${login%%[![:space:]]*}"}
  login=${login%"${login##*[![:space:]]}"}
  # shellcheck disable=SC2018,SC2019 # A-Z alone, as fold.ASCII lowers: no other letter changes
  printf '%s' "$login" | LC_ALL=C tr 'A-Z' 'a-z'
}

# whoami_person FILE: the person the GET /api/v1/whoami answer in FILE names, into whoami_named,
# or why it names none, into whoami_problem (returning 1). Dispatch answers a person's browser
# session `{"kind":"user","login":...}` and every other credential, a personal token that holds the
# person scope among them, `{"kind":"agent","owner":...}`, whose owner is null where the credential
# acts for no person (issue_assignee.go, whoami). Each names the person by their address, folded
# (fold_login). Any other kind, no person, or a person written otherwise is a problem that names
# what was answered: a grant's approver and operator could not be held against it.
whoami_person() {
  local answer kind field named
  whoami_named=''
  whoami_problem=''
  answer=$(tr -d ' \n' <"$1" 2>/dev/null)
  kind=$(json_field kind "$answer")
  case $kind in
    agent) field=owner ;;
    user) field=login ;;
    *)
      whoami_problem="whoami answered kind ${kind:-none}, neither agent (a token) nor user (a browser session): ${answer:0:200}"
      return 1
      ;;
  esac
  named=$(json_field "$field" "$answer")
  case $named in
    '' | null)
      whoami_problem="whoami answered kind $kind with no $field, so the credential acts for no person: ${answer:0:200}"
      return 1
      ;;
  esac
  if [ "$(fold_login "$named")" != "$named" ] || [ "${named#*@}" = "$named" ]; then
    whoami_problem="whoami answered kind $kind with $field $named, which is no folded address, the form a grant names a person in"
    return 1
  fi
  whoami_named=$named
}

# grants_read_before: the full member's login and list, and the probe grant: sets grants_before
# (the ids every signed-in person is listed, one a line), grants_probe and grants_problem. The
# probe is the first grant with no approver and no operator, each field folded and compared with
# the full member's login (whoami_person, with the full member's token) and the test person's
# address, folded too (fold_login).
grants_read_before() {
  local status self who id granted approver operator
  [ -z "$grants_problem" ] || return 0
  status=$(full_token_get /api/v1/whoami "$work/token-whoami.json")
  if [ "$status" != 200 ]; then
    grants_problem="whoami with the full member's token answered $status: $(text_of "$work/token-whoami.json")"
    return 0
  fi
  if ! whoami_person "$work/token-whoami.json"; then
    grants_problem="whoami with the full member's token: $whoami_problem"
    return 0
  fi
  self=$whoami_named
  grants_problem=$(grants_read before)
  [ -z "$grants_problem" ] || return 0
  grants_before=$(everyone_grants "$work/grants-before.rows")
  who=$(fold_login "$person")
  while IFS="$tab" read -r id granted approver operator || [ -n "$id" ]; do
    [ "$granted:$approver:$operator" = automatic:null: ] || continue
    approver=$(fold_login "$approver")
    operator=$(fold_login "$operator")
    [ "$approver" != "$self" ] && [ "$operator" != "$self" ] || continue
    [ -z "$who" ] || { [ "$approver" != "$who" ] && [ "$operator" != "$who" ]; } || continue
    grants_probe=$id
    break
  done <"$work/grants-before.rows"
}

# The preflight, where this file runs alone.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  case $0 in */*) here=${0%/*} ;; *) here=. ;; esac
  # shellcheck source=outside-agent-lib.sh
  . "$here/outside-agent-lib.sh"
  tab=$(printf '\t')
  allow_http=${OUTSIDE_ALLOW_HTTP:-}
  person=${OUTSIDE_PERSON:-}
  work=$(mktemp -d) || {
    echo 'UNKNOWN: mktemp -d made no scratch directory'
    exit 2
  }
  trap 'rm -rf "$work"' EXIT
  token_header="$work/full-token.h"
  grants_before=''
  grants_probe=''
  grants_problem=$(lacking OUTSIDE_PERSON)
  [ -n "$grants_problem" ] || grants_problem=$(token_setup)
  grants_read_before
  if [ -n "$grants_problem" ]; then
    printf 'UNKNOWN: %s\n' "$(cell "$grants_problem")"
    exit 2
  fi
  if [ -z "$grants_probe" ]; then
    echo 'no grant with no approver and no operator'
    exit 1
  fi
  printf 'grant %s\n' "$grants_probe"
fi
