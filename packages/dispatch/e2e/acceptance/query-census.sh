#!/usr/bin/env bash
# The census driver of `c-query-once`. That check's pull request refuses a query key named twice on
# every row of Dispatch's route table with 400 INVALID_QUERY, unless the row declares that key a
# list, so a client of ours that sends a one-value key twice would start to be refused. The pull
# request body holds a census of every client site that sends a query key, one table row each, and
# this driver holds that table to the tree.
#
# It runs four searches at HEAD of the repository that holds this file, as the check states them,
# with HEAD read once as its commit:
#
#   1. git grep -nE '(searchParams|query|params|next|search|url\.searchParams)\.append\(' HEAD --
#        packages/dispatch/web/src packages/envoy-client packages/pi-legion packages/contracts
#        ':!*.test.ts' ':!*.test.tsx' ':!*__tests__*' ':!**/test/**' ':!**/e2e/**'
#   2. git grep -nE 'URLSearchParams|searchParams\.set\(' HEAD -- the same four directories and
#        packages/pi-envoy, packages/pi-shared and packages/claude-envoy, with the same exclusions;
#   3. git grep -lE '"/api/v1/' HEAD -- '*.go' ':!*_test.go' ':!packages/envoy/internal/dispatch/**',
#        the Go clients: each Go file outside the server that names a route;
#   4. git grep -nE '[?&][a-z_]+=' HEAD -- <each file command 3 found>, their literal query strings.
#
# It prints each command, every line it found and its count of files, and fails a command that
# found no file, since a search that matches nothing would read as a clean census.
#
# The body. CENSUS_BODY_FILE names a file holding it, for a run that needs no GitHub (a local run,
# a negative control). Unset, the driver reads the body of the pull request CENSUS_PR names in
# cybermonklabs/legion (a number, a URL or a branch, as `gh pr view` takes it); CENSUS_PR defaults
# to `legion/$LEGION_ISSUE`, the branch Legion pushes an issue's tree to, whose key a Legion pod
# exports as LEGION_ISSUE. That `gh pr view --json body` is the driver's one call outside the
# repository, and a read. A body's carriage returns are dropped before it is read.
#
# The table it reads. The census section opens at the first heading `### Census…` and closes at the
# next heading of level 1, 2 or 3; a fenced code block inside it is skipped. Its table is every
# table there whose header row is `| Client | Site | Where the query goes | Sends a key twice? |`,
# and each row under that header has those four cells, none holding a `|`. Of each row it reads:
#
#   - Site: each backticked span starting `packages/` names a file by its repository path, cut at
#     its first `:` (`packages/x/y.ts:110-122` names packages/x/y.ts). A row may name several files
#     and a file may be named by several rows; a row whose Site names no such span (a summary row)
#     names no file.
#   - Sends a key twice?: a cell that starts with the word `No` (after any `*` or `_` of emphasis)
#     says the row's client sends no key the contract declares one value more than once. Any other
#     cell says it sends keys more than once, and each backticked span in it is one of those keys,
#     so it holds no other backticked text. Such a row's Where the query goes cell then names in
#     backticks each route it sends them to, `GET /api/v1/…`, the path spelled as the OpenAPI
#     document spells it; that cell's other backticked text is not read.
#
# It fails, naming what it read, on:
#
#   - a file that a command finds and no row names;
#   - a row that says it sends key K more than once to route R where the committed OpenAPI document
#     (packages/contracts/schemas/dispatch-openapi.json at HEAD) declares K one value on R, or does
#     not declare K on R at all, since the layer refuses an undeclared key named twice too. K is a
#     list on R where R's operation declares a query parameter K whose schema type is `array`, the
#     rule contractQueryKeys reads the document by for TestEverySingleValueQueryKeyRefusesARepeat;
#   - a row whose last cell neither starts with `No` nor names a key, a row that names keys and no
#     route, a route the document holds no operation for, and a row of other than four cells;
#   - a body with no census section, with more than one, or with no row under the census header.
#
# Every failure prints `FAIL: <section> <what>: <observed>` on stderr. The run goes on to the next
# check, so that one run names every unnamed file and every wrong row, and then exits 1; only a
# failure that leaves nothing after it to judge (a search git could not run, an OpenAPI document it
# could not read, a body with no census section or no census row) exits 1 at once. A run where
# every check held prints one `PASS: census <summary>` line, its last, and exits 0. A usage problem
# exits 2: no body input, a body file that cannot be read, an empty body, a body gh could not read,
# a missing command, or no git repository around this file. The driver never prints a line starting
# `SKIP`.
#
# No check is part of an `&&` list: under `set -e` a failing command anywhere but last in such a
# list does not stop the script.

set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: [CENSUS_BODY_FILE=<file holding the pull request body>] query-census.sh
       (unset, the body is read with gh from the pull request CENSUS_PR names in
       cybermonklabs/legion, legion/$LEGION_ISSUE by default; the four searches and the OpenAPI
       document are read at HEAD of the repository that holds this file)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_commands date git head jq tr

readonly repository=cybermonklabs/legion
readonly openapi_path=packages/contracts/schemas/dispatch-openapi.json
readonly census_header=("Client" "Site" "Where the query goes" "Sends a key twice?")
readonly exclusions=(':!*.test.ts' ':!*.test.tsx' ':!*__tests__*' ':!**/test/**' ':!**/e2e/**')
readonly client_dirs=(packages/dispatch/web/src packages/envoy-client packages/pi-legion packages/contracts)
readonly plugin_dirs=(packages/pi-envoy packages/pi-shared packages/claude-envoy)
# A line that opens or closes a fenced code block, whose lines are not the body's own.
readonly fence_pattern='^[[:space:]]*(```|~~~)'

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
root="$(git -C "$script_dir" rev-parse --show-toplevel 2>/dev/null)" || {
  printf 'no git repository holds %s, so there is no HEAD to search\n' "$script_dir" >&2
  exit 2
}
readonly root
commit="$(git -C "$root" rev-parse --verify --quiet 'HEAD^{commit}')" || {
  printf 'the repository at %s has no HEAD commit to search\n' "$root" >&2
  exit 2
}
readonly commit

work_dir
# The EXIT trap below invokes this; shellcheck misses that in a script whose every path exits.
# shellcheck disable=SC2329
cleanup() {
  local code=$?
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

failures=0
# fail SECTION WHAT OBSERVED: one failed check, counted; the run goes on to the next. This replaces
# lib.sh's fail, which exits at the first, so that one run names every unnamed file and wrong row.
fail() {
  failures=$((failures + 1))
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
}
# refuse SECTION WHAT OBSERVED: a failed check that leaves nothing after it to judge.
refuse() {
  fail "$@"
  exit 1
}

# The body, into $work/body.md with its carriage returns dropped: a body edited on GitHub's page
# ends its lines with CRLF.
readonly body="$work/body.md"
if [[ -n "${CENSUS_BODY_FILE:-}" ]]; then
  [[ -f "$CENSUS_BODY_FILE" && -r "$CENSUS_BODY_FILE" ]] || {
    printf 'CENSUS_BODY_FILE names %s, which is no file this run can read\n' "$CENSUS_BODY_FILE" >&2
    usage
    exit 2
  }
  body_source="the file $CENSUS_BODY_FILE"
  tr -d '\r' <"$CENSUS_BODY_FILE" >"$body"
else
  pull_request="${CENSUS_PR:-}"
  if [[ -z "$pull_request" && -n "${LEGION_ISSUE:-}" ]]; then
    pull_request="legion/$LEGION_ISSUE"
  fi
  [[ -n "$pull_request" ]] || {
    printf 'set CENSUS_BODY_FILE to a file holding the pull request body, or CENSUS_PR (or LEGION_ISSUE) to name the pull request\n' >&2
    usage
    exit 2
  }
  require_commands gh
  gh pr view "$pull_request" --repo "$repository" --json body -q .body >"$work/gh.out" 2>"$work/gh.err" || {
    printf 'gh pr view %s --repo %s exited %s: %s\n' "$pull_request" "$repository" "$?" "$(head -c 600 "$work/gh.err")" >&2
    exit 2
  }
  body_source="the body of pull request $pull_request in $repository"
  tr -d '\r' <"$work/gh.out" >"$body"
fi
readonly body_source
[[ -s "$body" ]] || {
  printf '%s is empty, so it holds no census to read\n' "$body_source" >&2
  exit 2
}

printf 'query-census.sh at %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf 'repository: %s\n' "$root"
printf 'HEAD: %s\n' "$commit"
printf 'body: %s\n' "$body_source"

# shown ARGUMENT...: the arguments as one line a reader can paste, each holding a shell character
# in single quotes. None of the four commands holds a single quote.
shown() {
  local argument line=""
  for argument in "$@"; do
    if [[ "$argument" =~ ^[[:alnum:]_./=-]+$ ]]; then
      line+=" $argument"
    else
      line+=" '$argument'"
    fi
  done
  printf '%s' "${line# }"
}

# The files the four commands found, each once in the order first found, with the commands that
# found it (found_by); and the files of the last search, in order (searched).
found=()
declare -A found_by=()
searched=()
counts=()

# search NUMBER WHAT GIT-GREP-ARGUMENT...: runs one git grep at $commit and prints the command,
# every line it found, without the commit in front, and its count of files. A command that found no
# file fails; one that could not run (git grep's status 2 or more) ends the run.
search() {
  local number="$1" what="$2"
  shift 2
  local status=0 line path count=0
  local -A seen=()
  printf '\ncommand %s, %s:\n  git -C %s grep %s\n' "$number" "$what" "$root" "$(shown "$@")"
  git -C "$root" grep "$@" >"$work/hits" 2>"$work/grep.err" || status=$?
  ((status <= 1)) ||
    refuse "command $number" "ran" "git grep exited $status: $(head -c 600 "$work/grep.err")"
  searched=()
  while IFS= read -r line; do
    line="${line#"$commit":}"
    path="${line%%:*}"
    printf '  %s\n' "$line"
    [[ -z "${seen[$path]:-}" ]] || continue
    seen[$path]=1
    searched+=("$path")
    count=$((count + 1))
    if [[ -z "${found_by[$path]:-}" ]]; then
      found+=("$path")
      found_by[$path]="$number"
    else
      found_by[$path]+=", $number"
    fi
  done <"$work/hits"
  counts+=("$count")
  printf 'command %s found %d files\n' "$number" "$count"
  ((count > 0)) ||
    fail "command $number" "found a file" "it found none, and a search that matches nothing cannot read as a clean census"
}

search 1 "the query builders that append a key" \
  -nE '(searchParams|query|params|next|search|url\.searchParams)\.append\(' "$commit" -- \
  "${client_dirs[@]}" "${exclusions[@]}"
search 2 "every URLSearchParams and searchParams.set" \
  -nE 'URLSearchParams|searchParams\.set\(' "$commit" -- \
  "${client_dirs[@]}" "${plugin_dirs[@]}" "${exclusions[@]}"
search 3 "the Go clients, each Go file outside the server that names a route" \
  -lE '"/api/v1/' "$commit" -- '*.go' ':!*_test.go' ':!packages/envoy/internal/dispatch/**'
go_files=("${searched[@]}")
if ((${#go_files[@]} > 0)); then
  search 4 "the literal query strings of the files command 3 found" \
    -nE '[?&][a-z_]+=' "$commit" -- "${go_files[@]}"
else
  counts+=(0)
  printf '\ncommand 4, the literal query strings of the files command 3 found: not run\n'
  fail "command 4" "found a file" "command 3 found no Go file for it to search"
fi

# The committed OpenAPI document: each operation it holds, as `METHOD /path`, and each query
# parameter one declares, with its schema type (`untyped` for a schema that names none), as
# `METHOD /path key`.
git -C "$root" show "$commit:$openapi_path" >"$work/openapi.json" 2>"$work/show.err" ||
  refuse openapi "document is read at HEAD" "git show $commit:$openapi_path exited $?: $(head -c 600 "$work/show.err")"
jq -r '
  .paths | to_entries[] | .key as $path | .value | to_entries[]
  | select(.key | test("^(get|put|post|delete|patch|head|options|trace)$"))
  | [(.key | ascii_upcase), $path, ((.value.parameters // [])[] | select(.in == "query") | .name, (.schema.type // "untyped"))]
  | @tsv' "$work/openapi.json" >"$work/operations.tsv" 2>"$work/jq.err" ||
  refuse openapi "document is read at HEAD" "jq could not read it: $(head -c 600 "$work/jq.err")"
declare -A operations=() query_types=()
while IFS=$'\t' read -r -a fields; do
  operation="${fields[0]} ${fields[1]}"
  operations[$operation]=1
  for ((i = 2; i + 1 < ${#fields[@]}; i += 2)); do
    query_types["$operation ${fields[i]}"]="${fields[i + 1]}"
  done
done <"$work/operations.tsv"
((${#operations[@]} > 0)) ||
  refuse openapi "document is read at HEAD" "$openapi_path at $commit holds no operation"

# trimmed TEXT: TEXT without the blanks around it.
trimmed() {
  local text="$1"
  text="${text#"${text%%[![:space:]]*}"}"
  text="${text%"${text##*[![:space:]]}"}"
  printf '%s' "$text"
}

# split_cells ROW: sets `cells` to the trimmed cells of a table row.
split_cells() {
  local row cell
  local -a parts
  row="$(trimmed "$1")"
  row="${row#|}"
  row="${row%|}"
  cells=()
  IFS='|' read -r -a parts <<<"$row"
  for cell in "${parts[@]}"; do
    cells+=("$(trimmed "$cell")")
  done
}

# spans_of TEXT: sets `spans` to each backticked span of TEXT, in order.
spans_of() {
  # shellcheck disable=SC2016 # the backticks are markdown's code spans, not a substitution
  local rest="$1" pattern='`([^`]*)`'
  spans=()
  while [[ "$rest" =~ $pattern ]]; do
    spans+=("${BASH_REMATCH[1]}")
    rest="${rest#*"${BASH_REMATCH[0]}"}"
  done
}

# is_census_header CELL...: whether the cells are the census table's header.
is_census_header() {
  (($# == ${#census_header[@]})) || return 1
  local i=0 cell
  for cell in "$@"; do
    [[ "$cell" == "${census_header[i]}" ]] || return 1
    i=$((i + 1))
  done
}

# The census rows, each with its line in the body. A table is the census table when its header row
# is census_header; its separator row is skipped, and every other table of the section is not read.
rows=()
row_lines=()
headings=0
section=0
fence=0
table=none
line_number=0
while IFS= read -r line || [[ -n "$line" ]]; do
  line_number=$((line_number + 1))
  if [[ "$line" =~ $fence_pattern ]]; then
    fence=$((1 - fence))
    table=none
    continue
  fi
  ((fence == 0)) || continue
  if [[ "$line" =~ ^(#+)[[:space:]] ]]; then
    level=${#BASH_REMATCH[1]}
    if [[ "$line" =~ ^###[[:space:]]+Census ]]; then
      headings=$((headings + 1))
      if ((section == 0)); then
        section=1
      else
        section=2
      fi
    elif ((section == 1 && level <= 3)); then
      section=2
    fi
    table=none
    continue
  fi
  ((section == 1)) || continue
  if [[ "$line" != \|* ]]; then
    table=none
    continue
  fi
  case "$table" in
  none)
    split_cells "$line"
    if is_census_header "${cells[@]}"; then
      table=separator
    else
      table=other
    fi
    ;;
  separator)
    table=census
    [[ "$line" =~ ^\|[[:space:]:|-]+$ ]] || {
      rows+=("$line")
      row_lines+=("$line_number")
    }
    ;;
  census)
    rows+=("$line")
    row_lines+=("$line_number")
    ;;
  esac
done <"$body"

printf '\ncensus: %s holds %d census sections and %d rows under the census header\n' \
  "$body_source" "$headings" "${#rows[@]}"
((headings > 0)) ||
  refuse census "section is in the body" "$body_source holds no heading starting '### Census'"
((headings == 1)) ||
  refuse census "section is in the body once" "$body_source holds $headings headings starting '### Census'"
((${#rows[@]} > 0)) ||
  refuse census "table has a row" "the census section holds no row under the header | $(printf '%s | ' "${census_header[@]}")"

# Each row: the files its Site names, and, where it says it sends keys more than once, each key on
# each route it names, held to the committed document.
declare -A named=()
repeating_rows=0
list_pairs=0
for index in "${!rows[@]}"; do
  row="${rows[index]}"
  where="row at body line ${row_lines[index]}"
  split_cells "$row"
  if ((${#cells[@]} != ${#census_header[@]})); then
    fail census "$where has ${#census_header[@]} cells" "it has ${#cells[@]}: $row"
    continue
  fi
  site="${cells[1]}"
  route_cell="${cells[2]}"
  answer="${cells[3]}"
  where+=" (Site: $site)"

  spans_of "$site"
  for span in "${spans[@]}"; do
    if [[ "$span" == packages/* ]]; then
      named[${span%%:*}]=1
    fi
  done

  answer_words="${answer#"${answer%%[!*_[:space:]]*}"}"
  if [[ "$answer_words" =~ ^No([^[:alnum:]]|$) ]]; then
    continue
  fi
  spans_of "$answer"
  keys=("${spans[@]}")
  if ((${#keys[@]} == 0)); then
    fail census "$where says No or names in backticks each key it sends more than once" "its last cell is: $answer"
    continue
  fi
  spans_of "$route_cell"
  routes=()
  for span in "${spans[@]}"; do
    if [[ "$span" =~ ^(GET|HEAD|POST|PUT|PATCH|DELETE)\ /[^[:space:]]*$ ]]; then
      routes+=("$span")
    fi
  done
  if ((${#routes[@]} == 0)); then
    # shellcheck disable=SC2016 # the backticks are markdown's code spans, not a substitution
    fail census "$where names in backticks each route it sends its keys to" \
      "its last cell names $(printf '`%s` ' "${keys[@]}")and its Where the query goes cell names no route: $route_cell"
    continue
  fi
  repeating_rows=$((repeating_rows + 1))
  for route in "${routes[@]}"; do
    if [[ -z "${operations[$route]:-}" ]]; then
      fail census "$where sends to $route" "the committed OpenAPI document holds no operation $route"
      continue
    fi
    for key in "${keys[@]}"; do
      if [[ -z "${query_types["$route $key"]+declared}" ]]; then
        fail census "$where sends only list keys more than once" \
          "it says it sends \`$key\` more than once to $route, which declares no query parameter $key in the committed OpenAPI document, so a repeat of it is refused"
      elif [[ "${query_types["$route $key"]}" != array ]]; then
        fail census "$where sends only list keys more than once" \
          "it says it sends \`$key\` more than once to $route, where the committed OpenAPI document declares $key one value (${query_types["$route $key"]})"
      else
        list_pairs=$((list_pairs + 1))
      fi
    done
  done
done

# Each file a command found, named by a row or failed.
for path in "${found[@]}"; do
  [[ -n "${named[$path]:-}" ]] && continue
  finders="command ${found_by[$path]}"
  [[ "${found_by[$path]}" != *,* ]] || finders="commands ${found_by[$path]}"
  fail census "names $path" "no row of the census table names it, and $finders found it"
done

if ((failures > 0)); then
  printf '\ncensus checks failed: %d\n' "$failures" >&2
  exit 1
fi
pass census "at $commit: each of the ${#found[@]} files the four commands found (command 1: ${counts[0]}, command 2: ${counts[1]}, command 3: ${counts[2]}, command 4: ${counts[3]}) is named by a row of the ${#rows[@]} rows of the table, and the $repeating_rows rows that send a key more than once send only keys the committed OpenAPI document declares lists on the routes they name ($list_pairs keys on routes)"
