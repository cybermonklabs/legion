#!/usr/bin/env bash
# Drive c-duplicate-neighbourhood on a running dispatch:local server with test hooks enabled.
set -euo pipefail
set +x

usage() {
  printf '%s\n' 'Usage: DISPATCH_URL=<local server> DISPATCH_TOKEN=<bearer> PROJECT=CORE FAKE_GITHUB_PORT=<port> duplicate-neighbourhood.sh' >&2
}

# shellcheck source-path=SCRIPTDIR source=lib-processes.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-processes.sh"
require_env DISPATCH_URL PROJECT FAKE_GITHUB_PORT TMPDIR
require_commands bun curl date jq sleep sort
read_bearer

readonly driver=duplicate-neighbourhood.sh
readonly base="${DISPATCH_URL%/}"
[[ "$base" =~ ^http://(127\.0\.0\.1|localhost):[0-9]+$ && "$FAKE_GITHUB_PORT" =~ ^[0-9]+$ ]] || {
  usage
  exit 2
}
readonly project="$PROJECT"
readonly cli_entry="$e2e_dir/../../envoy-client/bin/dispatch.ts"
stamp="$(date +%s)-$$"
readonly stamp session="neighbourhood-$stamp" repo="example/neighbourhood-$stamp" component="neighbourhood-$stamp"
work_dir
cli_env
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"
keys=()
ask=""
source_path=""
poller=""
cleanup() {
  local code=$? index
  if [[ -n "$poller" ]]; then
    kill "$poller" 2>/dev/null || true
    wait "$poller" 2>/dev/null || true
  fi
  if [[ -n "$ask" ]]; then
    cli resolve-ask --ask "$ask" --kind resolved --reason 'Acceptance check complete.' >"$work/cleanup" 2>&1 ||
      printf '%s: could not resolve %s: %s\n' "$driver" "$ask" "$(<"$work/cleanup")" >&2
  fi
  for ((index=${#keys[@]}-1; index>=0; index--)); do
    close_issue "${keys[index]}"
  done
  if [[ -n "$source_path" ]]; then
    send DELETE "$source_path" "$(jq -cn --argjson actor "$(actor)" '{actor: $actor}')"
    if [[ "$status" != 2?? ]]; then
      printf '%s: could not remove %s: HTTP %s: %s\n' "$driver" "$source_path" "$status" "$(body)" >&2
      code=1
    fi
  fi
  rm -rf -- "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

jq -cn --arg repo "$repo" --arg file "$component.md" \
  '{($repo): {contents: "read", installation_id: 367, files: {($file): "---\ntitle: Platform\n---\nWindlass.\n"}}}' \
  >"$work/repos.json"
status="$(curl -sS --max-time 15 -X PUT -o "$work/body" -w '%{http_code}' \
  -H 'Content-Type: application/json' --data-binary "@$work/repos.json" \
  "http://127.0.0.1:$FAKE_GITHUB_PORT/__fixture/repos")" || fail setup 'seed fake GitHub' 'fixture request failed'
want_status 200 setup 'seed fake GitHub'
source_path="/api/v1/projects/$project/architecture-sources/$repo"
send PUT "$source_path" "$(jq -cn --argjson actor "$(actor)" '{branch: "main", actor: $actor}')"
want_status 200 setup 'add architecture source'
send POST "$source_path/sync"
want_status 200 setup 'sync architecture source'
expect setup 'the source imports one component' '[null,1]' '[.last_error, .components]'
readonly k_ref="dispatch://$project/component/$component"
attach=(--components-json "$(jq -cn --arg id "$component" '{mode:"explicit",ids:[$id]}')")

# create VAR PROJECT TITLE SPEC [FLAGS...] stores the key in VAR and keeps the CLI output.
create() {
  local var="$1" project_key="$2" title="$3" spec="$4"
  shift 4
  printf '%s\n' "$spec" >"$work/spec.md"
  run seed "create $var ($title)" issue --project "$project_key" --title "$title" --spec-file "$work/spec.md" "$@"
  [[ "$(<"$work/out")" =~ ^Created\ ([A-Z][A-Z0-9]*-[0-9]+): ]] ||
    fail seed "create $var" "no issue key in: $(<"$work/out")"
  local created="${BASH_REMATCH[1]}"
  printf -v "$var" '%s' "$created"
  keys+=("$created")
}

create p "$project" Pelican Cinnabar "${attach[@]}"
create x "$project" Xylophone Juniper "${attach[@]}"
create l "$project" Lichen Spindle "${attach[@]}"
create n "$project" Narwhal Viridian "${attach[@]}"
create q_issue "$project" Quartz Tundra "${attach[@]}"
run seed 'ask M on L' ask --issue "$l" --question 'Marmoset?' --option Lemur --option Tamarin
[[ "$(<"$work/out")" =~ ^Asked\ ([0-9a-f-]{36}) ]] || fail seed 'ask M' "no ask id in: $(<"$work/out")"
ask="${BASH_REMATCH[1]}"
readonly m_ref="dispatch://$l/ask/$ask"

create a "$project" Amaranth Grouse "${attach[@]}" --parent "$p" --blocked-by "$x"
create b "$project" Basalt Parsnip "${attach[@]}" --parent "$p"
run seed 'close B' issue-update --issue "$b" --status done --reason Garnet
create r "$project" Rhodium Pumice --filed-from "$m_ref"
run seed 'close R' issue-update --issue "$r" --status done --reason Zircon
create c "$project" Cicada "$m_ref"
create h "$project" Hyacinth Rook --filed-from "$m_ref"
create y "$project" Yew Plover --parent "$n"
for changed in "$h" "$c" "$y" "$p" "$x" "$n" "$q_issue" "$l"; do
  run seed "update the priority of $changed" issue-update --issue "$changed" --priority 2
done

# The foreign issue points at the same ask, not a component scoped to its project.
another_project seed alice
create e "$other_project" Echidna "$m_ref"

create d "$project" Dahlia "Obsidian dispatch://$n" "${attach[@]}" --parent "$p" --blocked-by "$x" \
  --filed-from "$m_ref" --duplicate-of "$q_issue"
# Stop at the first non-neighbour line, so the text search and other advice are not counted.
mapfile -t neighbours < <(awk '/^Graph neighbours \(shared targets\):$/ { section=1; next } section && /^- / { print; next } section { exit }' "$work/out")
[[ "${#neighbours[@]}" == 5 ]] || fail ranking 'five graph neighbours' "$(<"$work/out")"

check_neighbour() {
  local index="$1" candidate="$2" status_word="$3" title="$4" line prefix suffix targets got want
  shift 4
  line="${neighbours[index]}"
  prefix="- $candidate [$status_word] $title · shares "
  suffix=" → $base/issues/$candidate"
  [[ "$line" == "$prefix"*"$suffix" ]] || fail ranking "row $((index+1)) names $candidate" "$line"
  targets="${line#"$prefix"}"
  targets="${targets%"$suffix"}"
  got="$(printf '%s\n' "${targets//, /$'\n'}" | LC_ALL=C sort)"
  want="$(printf '%s\n' "$@" | LC_ALL=C sort)"
  [[ "$got" == "$want" ]] || fail ranking "exact shared targets for $candidate" "$line; want $*"
}
check_neighbour 0 "$a" triage Amaranth "$k_ref" "dispatch://$p" "dispatch://$x"
check_neighbour 1 "$y" triage Yew "dispatch://$n"
check_neighbour 2 "$c" triage Cicada "$m_ref"
check_neighbour 3 "$h" triage Hyacinth "$m_ref"
check_neighbour 4 "$b" done Basalt "$k_ref" "dispatch://$p"
pass ranking 'five ranked neighbours with their exact shared targets'

create f "$project" 'Fennel carrageenan xylitol' Saffron
create g "$project" 'Fennel carrageenan xylitol' Tarragon --force
awk -v key="$f" '/^Related items/ { section=1; next } section && /^- / { if ($2 == key) found=1; next } section { exit } END { exit !found }' "$work/out" ||
  fail text "G's text search names $f" "$(<"$work/out")"
grep -Fxq 'Graph neighbourhood check found no issue that shares a target.' "$work/out" ||
  fail text "G has no graph neighbours ($g)" "$(<"$work/out")"
pass text 'the text search finds F when the graph read finds nothing'

send PUT /api/v1/_test/holds/neighbourhood-read '{"duration":"15s"}'
want_status 204 allowance 'hold the next graph read'
send PUT /api/v1/_test/holds/related-search '{"duration":"4s"}'
want_status 204 allowance 'hold the next related search'
readonly held_title="Zebrawood $stamp"
ms() { date +%s%3N; }
started="$(ms)"
(
  while (( $(ms) - started < 7000 )); do
    if curl -fsS --max-time 1 -H "@$work/auth" "$base/api/v1/issues?project=$project" >"$work/list.json" &&
      jq -e --arg title "$held_title" 'any(.[]; .title == $title)' "$work/list.json" >/dev/null; then
      ms >"$work/visible"
      exit 0
    fi
    sleep 0.1
  done
  exit 1
) &
poller="$!"
create t_issue "$project" "$held_title" Obelisk "${attach[@]}"
elapsed=$(( $(ms) - started ))
wait "$poller" || fail allowance "the list reads $t_issue" "not seen; create took ${elapsed}ms"
poller=""
visible=$(( $(<"$work/visible") - started ))
(( visible <= 1000 && elapsed <= 7000 )) ||
  fail allowance 'the create commits before waiting for the reads' "listed after ${visible}ms; answered after ${elapsed}ms"
grep -Eq '^Related items|^Related-item check searched' "$work/out" ||
  fail allowance "$t_issue keeps the related search result" "$(<"$work/out")"
grep -Fxq 'No graph neighbourhood check was made: the graph read did not answer within 5s.' "$work/out" ||
  fail allowance "$t_issue says the graph read did not finish" "$(<"$work/out")"
pass allowance "listed at ${visible}ms, answered at ${elapsed}ms"

run links "read $d" read --issue "$d"
links="$(awk '/^Links:$/ { section=1; next } section && /^- / { print; next } section { exit }' "$work/out")"
got="$(printf '%s\n' "$links" | cut -d ' ' -f 2-4 | LC_ALL=C sort)"
want="$(printf '%s\n' "child_of issue dispatch://$p" "needs issue dispatch://$x" "filed_from ask $m_ref" \
  "duplicate_of issue dispatch://$q_issue" "affects component $k_ref" | LC_ALL=C sort)"
[[ "$got" == "$want" ]] || fail links "$d has each outbound relation once" "$links"
grep -Fxq "Components: $component" "$work/out" || fail links "$d names its component" "$(<"$work/out")"
node_line="$(grep '^Node:' "$work/out")" || fail links "$d has a Node line" "$(<"$work/out")"
[[ "$node_line" == *" · part of dispatch://$p" ]] || fail links "$d names its parent on the Node line" "$node_line"
pass links 'five outbound relations, the component header and the parent on the node line'
pass "$driver" 'complete 4 sections'
