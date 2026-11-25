# shellcheck shell=bash
# shellcheck disable=SC2154,SC2034 # restricted-sweep.sh sources this file: it sets what this reads, and reads what this sets
# The fills file of the restricted sweep (restricted-sweep.sh sources this file, beside it): what
# fills mode writes of the full member's half and a runner or a writer reads, each line of it
# written and read here. It defines functions and the format's two constants, over the entry
# script's `work`, `project`, `origin`, `member`, `fills_file`, `routes_sha256`, the row's `can`
# and `ft`, and every name `fill_names` lists.

# The format's version: a runner refuses fills of another.
readonly fills_version=4
# The fills a whole file holds, each a variable of the sweep by the same name, which the lookup
# sets: the objects of the project it found, and the values a runner fills its rows with. Fills
# mode writes no file where one is empty, and a runner or a writer refuses fills that lack one.
readonly fill_names='issue issue_title doc doc_slug block pdoc pdoc_slug comment ask message
message_issue handoff handoff_issue handoff_phase near_copy session person proxy_repo priority
broadcast delivery_run going_run ref_prefix dm_session dm_message conversation_session
conversation_slug'

# put NAME VALUE: one fill, a line of the fills file fills mode writes. A value no line can hold
# whole and plain (a tab, a line break, a byte outside printable ASCII) stops the run, as no fill
# then says what it was.
put() {
  case $2 in *[!\ -~]*) unknown "the fill $1 holds a byte the fills file cannot: $(printf '%s' "$2" | cut -c1-60)" ;; esac
  printf '%s\t%s\n' "$1" "$2" >>"$work/fills" || unknown 'could not write a fill'
}

# put_row LABEL: fills mode's half of the row LABEL, as it was judged: a `can_fail` line where it
# can fail, and the full member's status of it (`full_status <status> <row>`), which every row
# the fills judged has.
put_row() {
  [ "$can" = 0 ] || put can_fail "$1"
  put full_status "${ft%% *} $1"
}

# fills_can LABEL: on a runner, whether the fills say the row LABEL can fail.
fills_can() {
  if grep -q -x -F -e "$1" "$work/can-fail"; then echo 1; else echo 0; fi
}
# fills_status LABEL: on a runner, the full member's status of the row LABEL, as the fills keep it,
# or nothing for a row the fills never judged.
fills_status() { word_before "$work/full-status" "$1"; }

# deployment_of FILE: which deployment FILE, a list of projects as GET /api/v1/projects answers
# it, was read at: the SHA-256 of the key and creation time of each of the member's own projects
# (`own`) it lists, sorted. A project is created once, on one deployment, so another deployment's
# projects of the same keys were created at other times; and the full member and the member, at
# the private origin and the public one of one deployment, are answered the same times. It fails,
# printing nothing, where FILE is no list jq reads or gives one of those projects no creation time
# (`created_at`, which the API requires): read as it is, that would read alike at every
# deployment, so fills mode and a runner would compare nothing. (The run checked at its start
# that sha256sum or shasum runs, so a failure here is the list's.)
deployment_of() {
  jq -r --rawfile own "$work/own" '($own | split("\n") | map(select(length > 0))) as $keys
    | .[]? | select(type == "object" and (.key as $key | any($keys[]; . == $key)))
    | if (.created_at | type) == "string" and .created_at != "" then "\(.key) \(.created_at)"
      else error("no creation time") end' "$1" >"$work/creation-times" 2>/dev/null || return 1
  sort "$work/creation-times" | sha256_of
}

# read_fills: the fills SWEEP_FILLS_FILE names, every one, read before the first request, so fills
# the run cannot read stop it before it reaches the deployment. It sets every name of fill_names,
# fills_member, fills_origin_sha256, fills_deployment, fills_age and fills_own, and writes the found
# values, the names a line prints by their kind, the rows that can fail and each row's full
# member's status to the run's `values`, `named`, `can-fail` and `full-status`. A runner refuses
# fills made from another route index than its own (routes.txt, by its SHA-256): they judged other
# rows than it calls.
read_fills() {
  local first name value lacking made_at='' fills_project='' fills_routes=''
  [ -f "$fills_file" ] && [ -r "$fills_file" ] ||
    unknown "SWEEP_FILLS_FILE names no file this run can read: $fills_file"
  [ -s "$fills_file" ] || unknown "SWEEP_FILLS_FILE is empty: $fills_file"
  IFS= read -r first <"$fills_file" || unknown "SWEEP_FILLS_FILE holds no line: $fills_file"
  [ "$first" = "restricted-sweep-fills	$fills_version" ] ||
    unknown "SWEEP_FILLS_FILE holds no fills of this sweep's version $fills_version: its first line is $(printf '%s' "$first" | cut -c1-60)"
  : >"$work/fills-own"
  fills_member='' fills_origin_sha256='' fills_deployment=''
  while IFS='	' read -r name value || [ -n "${name:-}" ]; do
    case $name in
      restricted-sweep-fills) ;;
      project) fills_project=$value ;;
      origin_sha256) fills_origin_sha256=$value ;;
      routes) fills_routes=$value ;;
      deployment) fills_deployment=$value ;;
      made_at) made_at=$value ;;
      member) fills_member=$value ;;
      own) printf '%s\n' "$value" >>"$work/fills-own" ;;
      value) printf '%s\n' "$value" >>"$work/values" ;;
      named) printf '%s\n' "$value" >>"$work/named" ;;
      can_fail) printf '%s\n' "$value" >>"$work/can-fail" ;;
      full_status) printf '%s\n' "$value" >>"$work/full-status" ;;
      *)
        # shellcheck disable=SC2086 # the list is words on purpose
        case " $(printf '%s ' $fill_names)" in
          *" $name "*) printf -v "$name" '%s' "$value" ;;
          *) unknown "SWEEP_FILLS_FILE holds a fill this sweep does not read: $(printf '%s' "$name" | cut -c1-60)" ;;
        esac
        ;;
    esac
  done <"$fills_file"
  [ "$fills_project" = "$project" ] ||
    unknown "SWEEP_FILLS_FILE holds the fills of project ${fills_project:-none}, not of $project"
  [ -n "$fills_routes" ] || unknown 'SWEEP_FILLS_FILE names no route index it was made from'
  [ "$mode" != runner ] || [ "$fills_routes" = "$routes_sha256" ] ||
    unknown "SWEEP_FILLS_FILE was made from another route index: its routes.txt's SHA-256 is $(printf '%s' "$fills_routes" | cut -c1-64), and this run's is $routes_sha256"
  [ -n "$fills_deployment" ] || unknown 'SWEEP_FILLS_FILE names no deployment it was made at'
  is_count "$made_at" || unknown "SWEEP_FILLS_FILE says no time it was made: ${made_at:-none}"
  fills_age=$(($(date -u +%s) - made_at))
  [ "$fills_age" -ge -300 ] || unknown "SWEEP_FILLS_FILE was made $((-fills_age)) s in the future"
  [ "$fills_age" -le 3300 ] ||
    unknown "SWEEP_FILLS_FILE was made $((fills_age / 60)) minutes ago, past 55: its question's run is kept an hour, read only by its asker"
  [ -n "$fills_member" ] || unknown 'SWEEP_FILLS_FILE names no member'
  is_sha256 "$fills_origin_sha256" || unknown 'SWEEP_FILLS_FILE names the origin it was made at by no SHA-256'
  lacking=''
  for name in $fill_names; do [ -n "${!name}" ] || lacking="$lacking $name"; done
  [ -z "$lacking" ] || unknown "SWEEP_FILLS_FILE lacks fills that whole fills hold:$lacking"
  fills_own=$(sort -u "$work/fills-own")
}

# write_fills: fills mode's end, the full member's half of every row whole in SWEEP_FILLS_FILE:
# the header (the route index's SHA-256 and the deployment among it), every fill of fill_names,
# the found values and the names among them a line prints by their kind, then each row's lines,
# which the rows wrote to the run's `fills` as they were judged. A fill left empty writes no file.
write_fills() {
  local name fill_value own value fills_bytes partial deployment
  mv "$work/fills" "$work/fills-rows" 2>/dev/null || : >"$work/fills-rows"
  : >"$work/fills"
  deployment=$(deployment_of "$work/full-projects.json") ||
    unknown "GET /api/v1/projects with SWEEP_FULL_TOKEN gives no creation time of each of the member's own projects, which tells the deployment, so no fills were written"
  put restricted-sweep-fills "$fills_version"
  put project "$project"
  # The origin by its SHA-256 alone: a runner names it by that, and the fills may be a public
  # workflow's input where they were made at a private origin.
  put origin_sha256 "$(printf '%s' "$origin" | sha256_of)"
  put routes "$routes_sha256"
  put deployment "$deployment"
  put made_at "$(date -u +%s)"
  put member "$member"
  sort -u "$work/own" >"$work/own-sorted"
  while read -r own; do [ -z "$own" ] || put own "$own"; done <"$work/own-sorted"
  for name in $fill_names; do
    fill_value=${!name}
    [ -n "$fill_value" ] || unknown "the lookup left the fill $name empty, so no fills were written"
    put "$name" "$fill_value"
  done
  sort -u "$work/values" >"$work/values-sorted"
  while IFS= read -r value; do [ -z "$value" ] || put value "$value"; done <"$work/values-sorted"
  sort -u "$work/named" >"$work/named-sorted"
  while IFS= read -r value; do [ -z "$value" ] || put named "$value"; done <"$work/named-sorted"
  cat "$work/fills-rows" >>"$work/fills" || unknown 'could not gather the fills'
  fills_bytes=$(wc -c <"$work/fills" | tr -d ' ')
  [ "$fills_bytes" -le 40960 ] ||
    unknown "the fills hold $fills_bytes bytes, above the 40 KiB one workflow input can carry; no fills were written"
  partial="$fills_file.partial.$$"
  if ! cp "$work/fills" "$partial" || ! mv -f "$partial" "$fills_file"; then
    rm -f "$partial"
    unknown "could not write SWEEP_FILLS_FILE: $fills_file"
  fi
  printf 'FILLS: %s fills of %s, %s bytes, %s rows that can fail, written to %s; a runner may read them for 55 minutes\n' \
    "$(wc -l <"$work/fills" | tr -d ' ')" "$project" "$fills_bytes" "$live" "$fills_file"
}
