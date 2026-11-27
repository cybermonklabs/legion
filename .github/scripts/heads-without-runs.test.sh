#!/usr/bin/env bash
# Tests for `heads-without-runs.sh`: the scheduled check that names each open pull request whose
# head no workflow ran.
#
# Each case writes the pages of GitHub's GraphQL answer and the REST answers of the pull requests,
# and runs the script with a stand-in `gh` first on PATH. The stand-in answers the list
# (`gh api graphql --input <file>`) with the page the request's cursor names, and a pull request
# (`gh api repos/o/r/pulls/<n>`) with its fixture, or with the fixture of that read's turn where a
# case gives the reads of one pull request different answers; a pull request with no fixture is a
# 404. It records each call, so a case can show what was read and what was not. The fixtures carry
# the shapes GitHub answered for this repository: two check suites with no check run on every head
# (two GitHub Apps' queued suites), and a check run of any conclusion as a node of a suite.
#
# Run from anywhere: .github/scripts/heads-without-runs.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/fixtures"

cat > "$work/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
fixtures=$STAND_IN/fixtures
if [ "$#" -eq 4 ] && [ "$1 $2 $3" = "api graphql --input" ]; then
  after=$(jq -r '.variables.after // "first"' "$4")
  echo "graphql $after $(jq -r '.operationName' "$4")" >> "$STAND_IN/gh-calls"
  if [ -e "$fixtures/graphql-refuses-$after" ]; then
    echo 'HTTP 502: Bad Gateway (https://api.github.com/graphql)' >&2
    exit 1
  fi
  cat "$fixtures/page-$after.json"
elif [ "$#" -eq 2 ] && [ "$1" = api ] && [[ $2 =~ ^repos/o/r/pulls/([0-9]+)$ ]]; then
  number=${BASH_REMATCH[1]}
  echo "pull $number" >> "$STAND_IN/gh-calls"
  turn=$(grep -c "^pull $number\$" "$STAND_IN/gh-calls")
  if [ -e "$fixtures/pull-$number.$turn.json" ]; then
    cat "$fixtures/pull-$number.$turn.json"
  elif [ -e "$fixtures/pull-$number.json" ]; then
    cat "$fixtures/pull-$number.json"
  else
    echo "HTTP 404: Not Found (https://api.github.com/repos/o/r/pulls/$number)" >&2
    exit 1
  fi
else
  echo "unexpected gh $*" >&2
  exit 2
fi
GH
chmod +x "$work/bin/gh"

# ago <seconds>: the time that many seconds before now, as GitHub writes a date.
ago() { date -u -d "@$(($(date -u +%s) - $1))" +%Y-%m-%dT%H:%M:%SZ; }
# sha <number>: a head of 40 hex characters that names its pull request.
sha() { printf '%040x' "$1"; }

# node <number> <draft> <opened> <committed> <runs a suite> [<more suites>] [<commit oid>]: one
# pull request of the list. <runs a suite> is a JSON array, one count of check runs for each check
# suite on the head; <more suites> says GitHub has more suites past these; <commit oid> is the last
# commit GitHub lists for the pull request, its head unless a case says otherwise.
node() {
  jq -nc --argjson number "$1" --argjson draft "$2" --arg opened "$3" --arg committed "$4" \
    --argjson suites "$5" --argjson more "${6:-false}" --arg head "$(sha "$1")" --arg oid "${7:-$(sha "$1")}" '
    {number: $number, isDraft: $draft, createdAt: $opened, headRefOid: $head,
     commits: {nodes: [{commit: {oid: $oid, committedDate: $committed,
       checkSuites: {pageInfo: {hasNextPage: $more},
         nodes: [$suites[] | {checkRuns: {nodes: [range(.) | {databaseId: (. + 1)}]}}]}}}]}}'
}

# page <cursor of the request, or first> <hasNextPage> <endCursor as JSON> <node>...: one page.
page() {
  local file=$1 next=$2 cursor=$3
  shift 3
  printf '%s\n' "$@" | jq -s --argjson next "$next" --argjson cursor "$cursor" \
    '{data: {repository: {pullRequests: {pageInfo: {hasNextPage: $next, endCursor: $cursor}, nodes: .}}}}' \
    > "$work/fixtures/page-$file.json"
}

# pull <number> <mergeable: true, false or null> [<turn>] [<head>] [<state>]: the REST answer of
# one pull request, for every read of it or for its <turn>th read alone.
pull() {
  local mergeable_state
  case $2 in true) mergeable_state=clean ;; false) mergeable_state=dirty ;; *) mergeable_state=unknown ;; esac
  jq -n --argjson mergeable "$2" --arg state "${5:-open}" --arg head "${4:-$(sha "$1")}" \
    --arg mergeable_state "$mergeable_state" \
    '{state: $state, mergeable: $mergeable, mergeable_state: $mergeable_state, head: {sha: $head}, base: {ref: "named-base-branch"}}' \
    > "$work/fixtures/pull-$1${3:+.$3}.json"
}

fresh() { rm -rf "$work/fixtures" && mkdir "$work/fixtures"; }

# run_check [<script>]: runs the script (or a broken copy of it) over the fixtures; sets out
# (stdout), err (stderr), summary (the step summary) and status. Under `timeout`, so a read that
# hung fails its case instead of hanging the job.
run_check() {
  rm -f "$work/gh-calls" "$work/summary"
  set +e
  out=$(env -u JOB_TIMEOUT_MINUTES -u STEP_TIMEOUT_MINUTES PATH="$work/bin:$PATH" STAND_IN="$work" \
    GH_TOKEN=t GH_RETRY_SECONDS=0 MERGEABLE_RETRY_SECONDS=0 GITHUB_REPOSITORY=o/r \
    GITHUB_STEP_SUMMARY="$work/summary" timeout 60 "${1:-$script_dir/heads-without-runs.sh}" 2> "$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
  summary=$(cat "$work/summary" 2> /dev/null || true)
}

gh_calls() { cat "$work/gh-calls" 2> /dev/null || true; }
pull_reads() { grep -c "^pull $1\$" "$work/gh-calls" || true; }
named() { contains "$out" "^#$1 $(sha "$1") "; }
errors() { grep -c '^::error::' <<< "$err" || true; }

three_hours=$((3 * 3600))
five_days=$((5 * 86400))

# The listing most cases read: 104 open pull requests over two pages, 101 on the first, so the
# second page is read through the first page's cursor.
#   #100  an old head whose check suites hold check runs: CI ran.
#   #101  an old head whose two suites hold no check run: no workflow ran. REST: no conflict.
#   #102  an old head whose one check run concluded skipped, as a lane job a label held back
#         leaves it. The read asks for no conclusion: a check run is a check run.
#   #103  a draft whose old head holds no check run.
#   #104  a head committed five minutes ago on a pull request opened five days ago.
#   #105  a head committed three hours ago on a pull request opened five minutes ago.
#   #106  a head 26 hours old with no check run. REST: no conflict. Its age reads in days.
#   #300-#393  94 old heads whose suites hold check runs.
#   #250  (second page) an old head with no check run. REST: conflicts with its base branch.
#   #251  (second page) as #250. REST: null at the first read, no conflict at the second.
#   #252  (second page) as #250. REST: null at both reads.
listing() {
  fresh
  local fillers=() n
  for n in $(seq 300 393); do
    fillers+=("$(node "$n" false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0,1,12]')")
  done
  page first true '"c1"' \
    "$(node 100 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0,1,58,1]')" \
    "$(node 101 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')" \
    "$(node 102 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0,1]')" \
    "$(node 103 true "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')" \
    "$(node 104 false "$(ago "$five_days")" "$(ago 300)" '[0,0]')" \
    "$(node 105 false "$(ago 300)" "$(ago "$three_hours")" '[0,0]')" \
    "$(node 106 false "$(ago "$five_days")" "$(ago $((26 * 3600)))" '[0,0]')" \
    "${fillers[@]}"
  page c1 false null \
    "$(node 250 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')" \
    "$(node 251 false "$(ago "$five_days")" "$(ago "$three_hours")" '[]')" \
    "$(node 252 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0]')"
  pull 101 true
  pull 106 true
  pull 250 false
  pull 251 null 1
  pull 251 true 2
  pull 252 null
}

listing
run_check

echo "case: an open pull request's old head with no check run is named, by number and head"
check "it fails" "$(is "$status" 1)"
check "#101 is named with its head, its age and why it has no run" \
  "$(contains "$out" "^#101 $(sha 101) 3h[0-9]*m no run and no conflict\$")"
check "an ::error:: line names #101 and its head" \
  "$(contains "$err" "^::error::#101: its head $(sha 101) is 3h[0-9]*m old and carries no check run: no run and no conflict\$")"
check "a head whose suites hold check runs is not named" "$(is "$(named 100)" false)"
check "its pull request is read only for the heads the list names" "$(is "$(pull_reads 100)" 0)"
check "an age over a day reads in days" \
  "$(contains "$out" "^#106 $(sha 106) 1d2h[0-9]*m no run and no conflict\$")"
check "the summary holds the table row" \
  "$(contains "$summary" "^| #101 | \`$(sha 101)\` | 3h[0-9]*m | no run and no conflict |\$")"
check "the summary says when it read and how many" \
  "$(contains "$summary" "^Read at [0-9T:-]*Z: 104 open pull requests, 1 of them drafts\. ")"

echo "case: a head whose one check run concluded skipped, as a lane label leaves it, is not named"
check "#102 is not named" "$(is "$(named 102)" false)"
check "its pull request is not read" "$(is "$(pull_reads 102)" 0)"

echo "case: a draft whose head has no check run is not named"
check "#103 is not named" "$(is "$(named 103)" false)"
check "its pull request is not read" "$(is "$(pull_reads 103)" 0)"

echo "case: a head younger than 20 minutes is not named"
check "a commit five minutes old is not named" "$(is "$(named 104)" false)"
check "an old commit on a pull request opened five minutes ago is not named" "$(is "$(named 105)" false)"
check "neither pull request is read" "$(is "$(pull_reads 104)$(pull_reads 105)" 00)"

echo "case: more than 100 open pull requests: a head on the second page is named"
check "it read both pages, the second through the first's cursor" \
  "$(is "$(grep '^graphql' "$work/gh-calls")" $'graphql first OpenPulls\ngraphql c1 OpenPulls')"
check "#250, on the second page, is named" "$(named 250)"

echo "case: a head whose pull request conflicts with its base is named with that reason, and no branch name"
check "#250 is named as a conflict" \
  "$(contains "$out" "^#250 $(sha 250) 3h[0-9]*m conflicts with its base branch: no merge ref, so no run\$")"
check "it gets no ::error:: line" "$(is "$(contains "$err" '#250')" false)"
check "the base branch's name is printed nowhere" "$(is "$(contains "$out$err$summary" 'named-base-branch')" false)"

echo "case: a mergeability GitHub is still computing is read once more"
check "#251, null and then no conflict, is named with no conflict" \
  "$(contains "$out" "^#251 $(sha 251) 3h[0-9]*m no run and no conflict\$")"
check "#251 was read twice" "$(is "$(pull_reads 251)" 2)"
check "#252, null at both reads, is named as not yet computed" \
  "$(contains "$out" "^#252 $(sha 252) 3h[0-9]*m mergeability not yet computed\$")"
check "#252 gets an ::error:: line" \
  "$(contains "$err" "^::error::#252: its head $(sha 252) is 3h[0-9]*m old and carries no check run: mergeability not yet computed\$")"
check "a head with a mergeability at the first read is read once" "$(is "$(pull_reads 101)" 1)"
check "the last line counts each reason" \
  "$(contains "$out" '^Named 5 head(s) of 104 open pull requests: 3 with no run and no conflict, 1 whose mergeability is not known, 1 that conflict with their base branch, 0 UNKNOWN\.$')"
check "one ::error:: line for each head that fails the check" "$(is "$(errors)" 4)"

echo "case: a conflict alone does not fail the check"
fresh
page first false null \
  "$(node 250 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')" \
  "$(node 100 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0,1]')"
pull 250 false
run_check
check "it passes" "$(is "$status" 0)"
check "it still names the head" "$(named 250)"
check "it writes no ::error:: line" "$(is "$(errors)" 0)"

echo "case: a list GitHub refused is UNKNOWN, never none named"
fresh
page first false null "$(node 100 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0,1]')"
touch "$work/fixtures/graphql-refuses-first"
run_check
check "it exits 2" "$(is "$status" 2)"
check "it says UNKNOWN and why" \
  "$(contains "$err" "^::error::UNKNOWN: GitHub's GraphQL API refused the list of open pull requests at every attempt, so this check read no head: it names none and clears none\$")"
check "it tried three times" "$(is "$(grep -c '^graphql first' "$work/gh-calls")" 3)"
check "it never says it named none" "$(is "$(contains "$out$summary" 'Named none')" false)"
check "the summary says UNKNOWN" "$(contains "$summary" '^UNKNOWN: ')"

echo "case: a second page GitHub refused is UNKNOWN, and the first page's heads are not named"
fresh
page first true '"c1"' "$(node 101 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')"
touch "$work/fixtures/graphql-refuses-c1"
pull 101 true
run_check
check "it exits 2" "$(is "$status" 2)"
check "it names no head" "$(is "$out" "")"
check "it reads no pull request" "$(is "$(pull_reads 101)" 0)"

echo "case: an empty list is UNKNOWN"
fresh
page first false null
run_check
check "it exits 2" "$(is "$status" 2)"
check "it says the list held nothing" \
  "$(contains "$err" "^::error::UNKNOWN: GitHub's GraphQL API listed no open pull request at all, ")"
check "it never says it named none" "$(is "$(contains "$out" 'Named none')" false)"

echo "case: an answer whose check runs did not resolve is UNKNOWN"
# The shape GitHub answered a page of 50 with: RESOURCE_LIMITS_EXCEEDED, and checkRuns null.
fresh
page first false null "$(node 101 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')"
jq '.data.repository.pullRequests.nodes[0].commits.nodes[0].commit.checkSuites.nodes[1].checkRuns = null
  | .errors = [{type: "RESOURCE_LIMITS_EXCEEDED", message: "Resource limits for this query exceeded."}]' \
  "$work/fixtures/page-first.json" > "$work/fixtures/page" && mv "$work/fixtures/page" "$work/fixtures/page-first.json"
run_check
check "it exits 2" "$(is "$status" 2)"
check "it names the shape" \
  "$(contains "$err" "^::error::UNKNOWN: GitHub's GraphQL API answered the list of open pull requests in a shape this check does not read, ")"
check "it names no head" "$(is "$out" "")"

echo "case: a page that says another follows and names no cursor is UNKNOWN"
fresh
page first true null "$(node 100 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0,1]')"
run_check
check "it exits 2" "$(is "$status" 2)"
check "it names the missing cursor" \
  "$(contains "$err" '^::error::UNKNOWN: GitHub.s GraphQL API said another page of open pull requests follows, and named no new cursor for it, ')"

echo "case: a head this read cannot decide is UNKNOWN, and the others are still judged"
fresh
page first false null \
  "$(node 110 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]' true)" \
  "$(node 111 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,1]' true)" \
  "$(node 112 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]' false "$(sha 999)")" \
  "$(node 101 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')"
pull 101 true
run_check
check "it exits 2" "$(is "$status" 2)"
check "a head past the first 100 suites with no check run is UNKNOWN" \
  "$(contains "$out" "^#110 $(sha 110) UNKNOWN: none of the first 100 check suites on its head holds a check run, and more suites follow\$")"
check "a head with a check run among the first suites is not named" "$(is "$(named 111)" false)"
check "a head that is not the last commit GitHub lists is UNKNOWN" \
  "$(contains "$out" "^#112 $(sha 112) UNKNOWN: the last commit GitHub lists for it is not its head, so its check runs were not read\$")"
check "the head it can decide is still named" "$(contains "$out" "^#101 $(sha 101) 3h[0-9]*m no run and no conflict\$")"
check "each UNKNOWN head gets an ::error:: line" "$(contains "$err" '^::error::UNKNOWN: #110: ')"
check "it never says it named none" "$(is "$(contains "$out" 'Named none')" false)"

echo "case: a pull request that took a new head since the list was read is left to the next run"
fresh
page first false null "$(node 101 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')"
pull 101 true '' "$(sha 777)"
run_check
check "it passes" "$(is "$status" 0)"
check "it says the pull request changed" \
  "$(contains "$out" '^#101 changed while it was read (it closed, or took a new head): the next run judges it$')"
check "it does not name the old head" "$(is "$(named 101)" false)"

echo "case: a pull request GitHub's REST API refused is named, its mergeability not read"
fresh
page first false null "$(node 101 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')"
run_check
check "it fails" "$(is "$status" 1)"
check "it names the head and what it could not read" \
  "$(contains "$out" "^#101 $(sha 101) 3h[0-9]*m mergeability not read: GitHub's REST API refused the pull request at every attempt\$")"

echo "case: more heads than GitHub keeps annotations for: ten ::error:: lines, the tenth counting the rest"
fresh
nodes=()
for n in $(seq 120 131); do
  nodes+=("$(node "$n" false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')")
  pull "$n" true
done
page first false null "${nodes[@]}"
run_check
check "it fails" "$(is "$status" 1)"
check "it names all twelve" "$(is "$(grep -c ' no run and no conflict$' <<< "$out")" 12)"
check "it writes ten ::error:: lines" "$(is "$(errors)" 10)"
check "the tenth counts the three it leaves out" \
  "$(contains "$err" '^::error::and 3 more: the job log and the step summary list every one$')"

echo "case: no head without a check run names none, and says how many it read"
fresh
page first false null \
  "$(node 100 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0,1]')" \
  "$(node 103 true "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')" \
  "$(node 104 false "$(ago "$five_days")" "$(ago 300)" '[0,0]')"
run_check
check "it passes" "$(is "$status" 0)"
check "it says it named none, with the count it read" \
  "$(is "$out" 'Named none: no open pull request that is no draft has a head 20 minutes old or more with no check run (3 open pull requests read, 1 of them drafts).')"
check "it reads no pull request" "$(is "$(grep -c '^pull' "$work/gh-calls" || true)" 0)"

summary "the check that names each open pull request whose head no workflow ran"
