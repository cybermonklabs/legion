#!/usr/bin/env bash
# Drives the membership answer (dispatch://INFRA-3862, contract K3 of its plan) as the secrets
# broker reads it: its three routes, GET /api/v1/access/projects/{key}, GET /api/v1/access/person
# and GET /api/v1/access/private-projects, read with the broker's own projected service-account
# token, and refused to a person's dsp_ token and to a person's cookie.
#
# It runs against a stack of its own, under the harness switch that gives the broker's token
# (DISPATCH_E2E_ACCESS_ISSUER=1, e2e/harness-access.ts): the harness starts a dev OIDC issuer that
# keeps the token in the file DISPATCH_E2E_ACCESS_TOKEN_FILE names, has its server read the
# membership answer for that account, and exports that variable with the rest. From
# packages/dispatch:
#
#   DATABASE_URL=<a PostgreSQL this run may truncate> DISPATCH_E2E_ACCESS_ISSUER=1 \
#     bash e2e/acceptance/with-harness.sh bash e2e/acceptance/membership-answer.sh
#
# setup signs alice@example.com and bob@example.com in through the harness's dev sign-in; alice
# makes a project of the run's own, gives it the Slack channel C0MEMBER01, and mints a dsp_ token
# of her own. `person` reads alice's member kind and an unknown login's. `project` reads the
# project for bob while it is public, compared whole but for `repositories`, which this driver
# does not read and #4156 removes from the answer. `private` reads the private-projects list,
# exactly {"projects":[]} while no project is private, then makes the project private with SQL
# (nothing in this release switches one) and reads it listed with its channel and the time its
# list changed, and reads the project again for bob, who no longer sees it, and for alice, its
# owner. `refused` sends each of the three routes alice's dsp_ token and her cookie, each 403
# ACCESS_ACCOUNT_REQUIRED. On exit it makes the project public again, so the next run's list starts
# empty; a run that cannot do so fails, naming the project.
#
# Its inputs are what with-harness.sh exports: DISPATCH_E2E_PORT (the server), DATABASE_URL (the
# database it serves, on this machine; one naming no loopback host is refused, since the driver
# writes it) and DISPATCH_E2E_ACCESS_TOKEN_FILE. Without the switch the harness exports no token
# file and the run stops there, exit 2, naming the switch; a file it cannot read, or an empty one,
# stops it the same way, naming the file. The token is never printed and never on a command line:
# curl reads its header from a file.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1.

set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage, from packages/dispatch:
  DATABASE_URL=<a PostgreSQL this run may truncate> DISPATCH_E2E_ACCESS_ISSUER=1 \
    bash e2e/acceptance/with-harness.sh bash e2e/acceptance/membership-answer.sh
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_E2E_PORT DATABASE_URL
if [[ -z "${DISPATCH_E2E_ACCESS_TOKEN_FILE:-}" ]]; then
  printf "DISPATCH_E2E_ACCESS_TOKEN_FILE is unset: the harness keeps the secrets broker's token, and its server reads the membership answer for that account, only under DISPATCH_E2E_ACCESS_ISSUER=1\n" >&2
  usage
  exit 2
fi
readonly token_file="$DISPATCH_E2E_ACCESS_TOKEN_FILE"
if [[ ! -r "$token_file" || ! -s "$token_file" ]]; then
  printf "DISPATCH_E2E_ACCESS_TOKEN_FILE names %s, which holds no token this run can read: the harness's access issuer (e2e/run-access-issuer.sh) writes it before it reports its port\n" "$token_file" >&2
  exit 2
fi
require_commands curl jq psql

# shellcheck disable=SC2153 # DATABASE_URL is the caller's input, checked by name above
readonly database_url="$DATABASE_URL"
# This driver writes the database (it makes a project private and public again), so it reads only
# this machine's own test database, as the other writing drivers do.
loopback_database_url "$database_url" ||
  fail setup "DATABASE_URL names this machine" "the URL must name one loopback host with no connection override; this driver writes this machine's own test database, never a deployment's"
work_dir
readonly base="http://127.0.0.1:$DISPATCH_E2E_PORT"
readonly alice=alice@example.com bob=bob@example.com channel=C0MEMBER01
project="MEMB$(printf '%05d' "$((RANDOM % 100000))" | tr 0-9 A-J)"
readonly project
made=""

# cleanup makes the run's project public again, through the server `query` reaches (psql without
# PGHOSTADDR, PGSERVICE or PGSERVICEFILE). A restore that fails, or that finds no such project,
# fails the run, naming why: no stack truncates this database, so a project left private would
# make the next run's empty-list check fail on state that run did not make.
cleanup() {
  local code=$? restored=""
  if [[ -n "$made" ]]; then
    restored="$(env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE psql "$database_url" -v ON_ERROR_STOP=1 -Atqc \
      "update projects set visibility = 'public' where key = '$project' returning key" 2>"$work/psql")" || true
    if [[ "$restored" != "$project" ]]; then
      printf 'membership-answer.sh: could not make %s public again, so the next run on this database would find it private: %s\n' \
        "$project" "${restored:-no row came back} $(<"$work/psql")" >&2
      [[ "$code" -ne 0 ]] || code=1
    fi
  fi
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'Authorization: Bearer %s\n' "$(<"$token_file")" >"$work/broker.auth"

# call CALLER METHOD PATH [JSON]: sends METHOD PATH as CALLER - `broker` (the broker's token),
# `token` (alice's dsp_ token), `alice` or `bob` (that person's cookie, with the dashboard's
# origin, which a write the cookie authenticates must name). The body goes to $work/body and the
# status to $status.
call() {
  local -a auth=() data=()
  case "$1" in
    broker | token) auth=(-H "@$work/$1.auth") ;;
    alice | bob) auth=(-b "$work/$1.jar" -H "Origin: $base") ;;
  esac
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  status="$(curl -sS -X "$2" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$base$3" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

section=setup
for person in alice bob; do
  curl -sS -o /dev/null -c "$work/$person.jar" "$base/auth/_dev/signin?login=$(q "${!person}")" \
    2>"$work/curl" || fail "$section" "sign ${!person} in" "$(<"$work/curl")"
  [[ -s "$work/$person.jar" ]] || fail "$section" "sign ${!person} in" "the dev sign-in route left no cookie"
done
pass "$section" "signed $alice and $bob in"
call alice POST /api/v1/projects "$(jq -cn --arg k "$project" '{key: $k, name: "Membership answer"}')"
want_status 201 "$section" "alice creates project $project"
made=1
pass "$section" "alice created project $project"
call alice PATCH "/api/v1/projects/$project/access" "$(jq -cn --arg c "$channel" '{slack_channels: [$c]}')"
want_status 200 "$section" "alice lists $channel on $project"
changed_at="$(jq -r .slack_channels_changed_at "$work/body")"
readonly changed_at
[[ "$changed_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z$ ]] ||
  fail "$section" "alice lists $channel on $project" "slack_channels_changed_at is $changed_at, want an RFC 3339 UTC time"
pass "$section" "alice listed $channel on $project at $changed_at"
call alice POST /api/v1/me/agent-tokens '{"name":"membership-answer"}'
want_status 201 "$section" "alice mints a dsp_ token"
jq -r .token "$work/body" >"$work/token"
[[ "$(<"$work/token")" == dsp_* ]] || fail "$section" "alice mints a dsp_ token" "the answer holds no dsp_ token"
printf 'Authorization: Bearer %s\n' "$(<"$work/token")" >"$work/token.auth"
pass "$section" "alice minted a dsp_ token"

section=person
call broker GET "/api/v1/access/person?person=$(q "$alice")"
want_status 200 "$section" "the broker reads $alice"
expect "$section" "$alice is a full member" \
  "$(jq -cnS --arg p "$alice" '{member_kind: "full", person: $p}')" .
call broker GET "/api/v1/access/person?person=$(q nobody@example.com)"
want_status 200 "$section" "the broker reads a login nobody has signed in with"
expect "$section" "a login nobody has signed in with is none" '"none"' .member_kind

section=project
call broker GET "/api/v1/access/projects/$project?person=$(q "$bob")"
want_status 200 "$section" "the broker reads $project for $bob"
expect "$section" "$bob sees public $project, owned by $alice, with its channel and its time" \
  "$(jq -cnS --arg p "$project" --arg o "$alice" --arg b "$bob" --arg c "$channel" --arg t "$changed_at" \
    '{project: $p, visibility: "public", owner: $o, person: $b, member_kind: "full", visible: true,
      slack_channels: [$c], slack_channels_changed_at: $t}')" 'del(.repositories)'

section=private
call broker GET /api/v1/access/private-projects
want_status 200 "$section" "the broker reads the private-projects list"
expect "$section" "the list is empty while no project is private" '{"projects":[]}' .
query "$section" "make $project private" \
  "update projects set visibility = 'private' where key = '$project' returning key"
[[ "$answer" == "$project" ]] || fail "$section" "make $project private" "the update returned '$answer'"
pass "$section" "made $project private with SQL"
call broker GET /api/v1/access/private-projects
want_status 200 "$section" "the broker reads the private-projects list"
expect "$section" "the list holds $project with its channel and the time its list changed" \
  "$(jq -cnS --arg p "$project" --arg c "$channel" --arg t "$changed_at" \
    '{projects: [{project: $p, slack_channels: [$c], slack_channels_changed_at: $t}]}')" .
call broker GET "/api/v1/access/projects/$project?person=$(q "$bob")"
want_status 200 "$section" "the broker reads private $project for $bob"
expect "$section" "$bob does not see private $project" '{"visibility":"private","visible":false}' \
  '{visibility, visible}'
call broker GET "/api/v1/access/projects/$project?person=$(q "$alice")"
want_status 200 "$section" "the broker reads private $project for $alice"
expect "$section" "$alice, its owner, sees private $project" '{"visibility":"private","visible":true}' \
  '{visibility, visible}'

section=refused
for route in "/api/v1/access/projects/$project?person=$(q "$bob")" \
  "/api/v1/access/person?person=$(q "$alice")" /api/v1/access/private-projects; do
  path="${route%%\?*}"
  for caller in token alice; do
    case "$caller" in
      token) who="alice's dsp_ token" ;;
      alice) who="alice's cookie" ;;
    esac
    call "$caller" GET "$route"
    want_status 403 "$section" "$who reads $path"
    expect "$section" "$who is refused $path" '"ACCESS_ACCOUNT_REQUIRED"' .code
  done
done
