#!/usr/bin/env bash
# Reads the priorities board back after scripts/priorities-seed.ts has run, against the table that
# seed was pointed at: every row of that table whose `Where` cell does not begin with the word DONE
# stands on the board, the priorities run in the table's order, and every issue a priority's record
# names as work that advances it says so itself.
#
# The table is read at the version the board records, never at whatever version it stands at now:
# each priority the seed wrote carries `Table row <#> of <document>, read at version <n>.` as the
# first line of its record, and this check reads the table at the greatest version those markers
# name, so a row a person wrote after the seed ran is no row this check reads as dropped. A
# priority whose marker names another document belongs to another table and is passed over, and
# one the seed did not write carries no marker at all.
#
# It names every row it did not find, rather than stopping at the first, then every priority whose
# record names an issue that does not serve it, as that issue's own `advances` answers. It writes
# nothing: Dispatch is only read, and the document reference is the only thing it is told.
#
# The bearer reaches no command's arguments, which /proc/<pid>/cmdline would then carry for every
# user of the machine: it is written once into a curl configuration file of mode 600 that each read
# passes with -K, removed by its own path on exit, interrupt and termination.
#
# usage: scripts/priorities-seed-check.sh --artifact <document reference>
#   --artifact <ref>     the priorities table, as the seed's own argument names it:
#                        dispatch://KEY/artifact/SLUG, dispatch://PROJECT/artifact/SLUG or
#                        dispatch://KEY/spec, each with an optional @v<N> this check passes over,
#                        since the version it reads is the one the board records
#   --help               print this block and exit 0
#   DISPATCH_URL         Dispatch's own API root, e.g. https://dispatch.example.org
#   DISPATCH_TOKEN_FILE  the file holding the bearer every read presents
#   exit: 0 the board holds the table, 1 it does not, 2 the check could not run
set -euo pipefail

name=priorities-seed-check

# Prints the header's usage block, from its `# usage:` line through its `# exit:` line, so a header
# edit above or within it never shifts what is printed.
usage() {
  sed -n '/^# usage: /,/^#   exit: /p' "$0"
}

# refuse <text>: the run was asked for something it cannot do - a usage error.
refuse() {
  echo "$name: $*" >&2
  usage >&2
  exit 2
}

# stop <text>: the check could not be made at all, so it judges nothing.
stop() {
  echo "$name: $*" >&2
  exit 2
}

faults=0

# fault <text>: one thing the board does not hold. Every one is named before the check exits 1.
fault() {
  echo "$name: $*" >&2
  faults=$((faults + 1))
}

artifact=''
while (($#)); do
  case $1 in
  --help | -h)
    usage
    exit 0
    ;;
  --artifact)
    (($# >= 2)) || refuse "--artifact takes the priorities table's document reference"
    artifact=$2
    shift 2
    ;;
  --artifact=*)
    artifact=${1#--artifact=}
    shift
    ;;
  *) refuse "$1 is no argument of this check" ;;
  esac
done
[[ -n $artifact ]] || refuse "no --artifact: name the priorities table's document reference"

for tool in curl jq sed; do
  command -v "$tool" >/dev/null || stop "$tool is required and is not on PATH"
done
[[ -n ${DISPATCH_URL:-} ]] || stop "DISPATCH_URL is not set: name Dispatch's own API root"
[[ -n ${DISPATCH_TOKEN_FILE:-} ]] || stop "DISPATCH_TOKEN_FILE is not set: name the file holding the bearer"
token=$(cat -- "$DISPATCH_TOKEN_FILE") || stop "could not read DISPATCH_TOKEN_FILE"
[[ -n $token ]] || stop "the file DISPATCH_TOKEN_FILE names is empty"

# The reference's grammar is parseDocumentReference's in scripts/priorities-seed.ts, over the two
# shapes parseDispatchRef reads: an issue's spec or document, and a project's document. A pinned
# version is dropped, because a marker records the document with no version and this check reads
# the version that marker names. The slug is held to the characters a path segment carries plainly,
# so no read of this check has to encode one.
reference=$artifact
[[ $reference =~ ^(.*)@v[0-9]+$ ]] && reference=${BASH_REMATCH[1]}
project_part='[A-Z][A-Z0-9]{1,9}'
issue_part="$project_part-[1-9][0-9]*"
slug_part='[A-Za-z0-9._-]+'
if [[ $reference =~ ^dispatch://($issue_part)/spec$ ]]; then
  owner_path="issues/${BASH_REMATCH[1]}/artifacts/spec"
  document="dispatch://${BASH_REMATCH[1]}/spec"
elif [[ $reference =~ ^dispatch://($issue_part)/artifact/($slug_part)$ ]]; then
  owner_path="issues/${BASH_REMATCH[1]}/artifacts/${BASH_REMATCH[2]}"
  document="dispatch://${BASH_REMATCH[1]}/artifact/${BASH_REMATCH[2]}"
elif [[ $reference =~ ^dispatch://($project_part)/artifact/($slug_part)$ ]]; then
  owner_path="projects/${BASH_REMATCH[1]}/artifacts/${BASH_REMATCH[2]}"
  document="dispatch://${BASH_REMATCH[1]}/artifact/${BASH_REMATCH[2]}"
else
  refuse "--artifact $artifact is no document reference: name the table as dispatch://KEY/artifact/SLUG, dispatch://PROJECT/artifact/SLUG or dispatch://KEY/spec, its slug written in letters, digits, '.', '-' and '_'"
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/$name.XXXXXX") || stop "could not make a directory for this run"
config="$work/curl.conf"
(
  umask 077
  : >"$config"
) || stop "could not make $config"
chmod 600 "$config"
printf 'header = "Authorization: Bearer %s"\n' "$token" >"$config"

cleanup() {
  rm -f -- "$config"
  rm -rf -- "$work"
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# get <path> <file>: writes the answer's body to <file> and prints its status, 000 where curl
# itself failed. The bearer rides the configuration file, so no argument here carries it.
get() {
  local path=$1 file=$2 status
  status=$(curl -sS -o "$file" -w '%{http_code}' --max-time 30 -K "$config" "$DISPATCH_URL$path") ||
    status=000
  printf '%s' "$status"
}

# read_json <path> <file>: a read the check cannot go on without.
read_json() {
  local status
  status=$(get "$1" "$2")
  [[ $status == 200 ]] || stop "GET $1 answered $status"
}

read_json /api/v1/priorities "$work/priorities.json"

# Each priority of this document, in the order the board draws them (rank order), as
# <id> <row> <version> <the keys its Advances line names>. The record's own marker is matched
# whole, so a description that merely opens with those words is no marker, and the Advances line
# is the line under the marker rather than the first line that reads like one.
board_ids=() board_rows=() board_versions=() board_advances=()
while IFS=$'\t' read -r id row version advances; do
  board_ids+=("$id")
  board_rows+=("$row")
  board_versions+=("$version")
  board_advances+=("$advances")
done < <(jq -r --arg document "$document" '
  .priorities[]
  | . as $priority
  | (.description | split("\n")) as $lines
  | ([$lines | to_entries[]
      | select(.value | test("^Table row [^ ]+ of [^ ]+, read at version ([0-9]+|none)\\.$"))][0]) as $marker
  | select($marker != null)
  | ($marker.value | split(" ")) as $words
  | select($words[4] == $document + ",")
  | ($lines[$marker.key + 1] // "") as $under
  | [$priority.id, $words[2], ($words[8] | rtrimstr(".")),
     (if ($under | startswith("Advances: ")) then ($under | ltrimstr("Advances: ") | rtrimstr(".")) else "" end)]
  | @tsv' "$work/priorities.json")

((${#board_ids[@]} > 0)) || {
  echo "$name: no priority of the board records a row of $document: the board holds nothing this table was seeded into" >&2
  exit 1
}

# The version to read the table at: the greatest any marker names, read in base ten, since a
# number written with a leading zero is no octal here. A marker reading `none` is a priority
# written from a document whose read answered no version, and names no version to read.
version=''
for recorded in "${board_versions[@]}"; do
  [[ $recorded == none ]] && continue
  if [[ -z $version ]] || ((10#$recorded > 10#$version)); then version=$recorded; fi
done
if [[ -n $version ]]; then
  read_json "/api/v1/$owner_path/versions/$version" "$work/table.json"
  read_at="version $version"
else
  read_json "/api/v1/$owner_path/text" "$work/table.json"
  read_at="its latest text (no marker of the board names a version)"
fi

mapfile -t lines < <(jq -r '.markdown' "$work/table.json")

cells=()
# read_cells <line>: fills cells with the line's cells, trimmed, where it is a table row, and
# answers 1 where it is not. A `\|` is a pipe inside a cell rather than the edge of one, and the
# text outside the row's own leading and trailing pipes is no cell - tableCells in
# scripts/priorities-seed.ts, read the same way here.
read_cells() {
  local line=$1 marked part parts=()
  line=${line#"${line%%[![:space:]]*}"}
  line=${line%"${line##*[![:space:]]}"}
  cells=()
  [[ $line == '|'* ]] || return 1
  marked=${line//\\|/$'\001'}
  IFS='|' read -r -a parts <<<"$marked"
  parts=("${parts[@]:1}")
  if ((${#parts[@]} > 0)); then
    part=${parts[-1]}
    part=${part#"${part%%[![:space:]]*}"}
    part=${part%"${part##*[![:space:]]}"}
    [[ -z $part ]] && parts=("${parts[@]:0:${#parts[@]} - 1}")
  fi
  for part in ${parts[@]+"${parts[@]}"}; do
    part=${part//$'\001'/|}
    part=${part#"${part%%[![:space:]]*}"}
    part=${part%"${part##*[![:space:]]}"}
    cells+=("$part")
  done
  return 0
}

# The first table carrying all four columns, as parseTable reads it: a header over a row of `---`
# cells, then every row under it until a line that is no table row at all.
row_numbers=() row_titles=() row_wheres=()
found_table=0
for ((at = 0; at + 1 < ${#lines[@]}; at++)); do
  read_cells "${lines[at]}" || continue
  header=(${cells[@]+"${cells[@]}"})
  read_cells "${lines[at + 1]}" || continue
  under=(${cells[@]+"${cells[@]}"})
  ((${#under[@]} > 0)) || continue
  ruled=1
  for cell in "${under[@]}"; do
    [[ $cell =~ ^:?-+:?$ ]] || {
      ruled=0
      break
    }
  done
  ((ruled)) || continue
  place_number=-1 place_title=-1 place_owner=-1 place_where=-1
  for ((column = 0; column < ${#header[@]}; column++)); do
    cell=${header[column]#\\}
    case ${cell,,} in
    '#') ((place_number < 0)) && place_number=$column ;;
    priority) ((place_title < 0)) && place_title=$column ;;
    owner) ((place_owner < 0)) && place_owner=$column ;;
    where) ((place_where < 0)) && place_where=$column ;;
    esac
  done
  ((place_number >= 0 && place_title >= 0 && place_owner >= 0 && place_where >= 0)) || continue
  found_table=1
  for ((line_at = at + 2; line_at < ${#lines[@]}; line_at++)); do
    read_cells "${lines[line_at]}" || break
    number=${cells[place_number]:-}
    [[ -n $number ]] ||
      stop "row $((${#row_numbers[@]} + 1)) of the table at $document has an empty # cell, so no priority of it can be found by its number"
    [[ $number =~ [[:space:]] ]] &&
      stop "the # cell \"$number\" of the table at $document holds whitespace: a row number is one word, because each priority's record names it"
    for seen in ${row_numbers[@]+"${row_numbers[@]}"}; do
      [[ $seen == "$number" ]] &&
        stop "two rows of the table at $document carry the # cell $number, so neither names one priority"
    done
    row_numbers+=("$number")
    row_titles+=("${cells[place_title]:-}")
    row_wheres+=("${cells[place_where]:-}")
  done
  break
done
((found_table)) ||
  stop "the table at $document, read at $read_at, holds no table with the columns #, Priority, Owner and Where"

# Which priority holds each row. The seed writes one priority per row, so a row two of them record
# is a board no run of the seed wrote.
declare -A priority_of=()
for ((p = 0; p < ${#board_ids[@]}; p++)); do
  row=${board_rows[p]}
  if [[ -n ${priority_of[$row]:-} ]]; then
    fault "two priorities record row $row of $document: ${priority_of[$row]} and ${board_ids[p]}"
    continue
  fi
  priority_of[$row]=${board_ids[p]}
done

# Every row the board must hold, named one by one rather than one at a time.
matched_rows=()
passed=0
for ((r = 0; r < ${#row_numbers[@]}; r++)); do
  number=${row_numbers[r]}
  if [[ ${row_wheres[r]} =~ ^DONE([^[:alnum:]_]|$) ]]; then
    passed=$((passed + 1))
    continue
  fi
  if [[ -z ${priority_of[$number]:-} ]]; then
    fault "row $number of $document (${row_titles[r]}) stands on no priority of the board"
    continue
  fi
  matched_rows+=("$number")
done

# The board draws its priorities in rank order and the seed writes the rows in the table's order,
# so the rows the board holds run in the table's order too.
board_order=()
for ((p = 0; p < ${#board_ids[@]}; p++)); do
  for number in ${matched_rows[@]+"${matched_rows[@]}"}; do
    [[ $number == "${board_rows[p]}" ]] && {
      board_order+=("${board_rows[p]}")
      break
    }
  done
done
for ((r = 0; r < ${#matched_rows[@]} && r < ${#board_order[@]}; r++)); do
  if [[ ${board_order[r]} != "${matched_rows[r]}" ]]; then
    fault "the board draws row ${board_order[r]} of $document where the table writes row ${matched_rows[r]}: its priorities do not run in the table's order"
    break
  fi
done

# Every key a priority's record names must be an issue that says it advances that priority, by a
# relation of its own: a relation it inherits from an issue above it is that issue's, not this
# one's, and the seed states the issue's own.
declare -A issue_own=()
declare -A issue_missing=()
relations=0
for ((p = 0; p < ${#board_ids[@]}; p++)); do
  id=${board_ids[p]}
  row=${board_rows[p]}
  advances=${board_advances[p]}
  [[ -n $advances && $advances != none ]] || continue
  keys=()
  IFS=',' read -r -a keys <<<"$advances"
  for key in "${keys[@]}"; do
    key=${key#"${key%%[![:space:]]*}"}
    key=${key%"${key##*[![:space:]]}"}
    [[ -n $key ]] || continue
    if [[ ! $key =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]]; then
      fault "priority $id (row $row of $document) records \"$key\" as work that advances it, which is no issue key"
      continue
    fi
    if [[ -z ${issue_own[$key]+held} ]]; then
      status=$(get "/api/v1/issues/$key" "$work/issue.json")
      case $status in
      200) issue_own[$key]=$(jq -r '[.advances[]? | select(.inherited != true) | .id] | join(" ")' "$work/issue.json") ;;
      404)
        issue_own[$key]=''
        issue_missing[$key]=yes
        ;;
      *) stop "GET /api/v1/issues/$key answered $status" ;;
      esac
    fi
    relations=$((relations + 1))
    if [[ -n ${issue_missing[$key]:-} ]]; then
      fault "priority $id (row $row of $document) records that $key advances it, and no issue carries that key"
    elif [[ " ${issue_own[$key]} " != *" $id "* ]]; then
      fault "priority $id (row $row of $document) records that $key advances it, and $key's own advances does not name it"
    fi
  done
done

if ((faults > 0)); then
  echo "$name: $document, read at $read_at: $faults faults" >&2
  exit 1
fi
echo "$name: $document, read at $read_at: ${#matched_rows[@]} rows stand on the board in the table's order, $passed rows passed over (DONE), $relations relations read back"
