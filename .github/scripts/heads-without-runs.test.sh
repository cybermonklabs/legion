#!/usr/bin/env bash
# Tests for `heads-without-runs.sh`: the scheduled check that names each open pull request whose
# head no workflow ran, and warns on each entry of main's merge queue that has come to conflict
# with main.
#
# Each case writes the pages of GitHub's GraphQL answers and the REST answers of the pull requests,
# and runs the script with a stand-in `gh` first on PATH. The stand-in answers the list of open pull
# requests and the list of main's merge queue (`gh api graphql --input <file>`, told apart by the
# request's operation) with the page the request's cursor names; a pull request
# (`gh api repos/o/r/pulls/<n>`) with its fixture, or with the fixture of that read's turn where a
# case gives the reads of one pull request different answers, and a pull request with no fixture
# is a 404; and main's activity (`gh api --paginate repos/o/r/activity?... --jq <filter>`) with
# its fixture under that filter. It records each call, so a case can show what was read and what
# was not. The fixtures carry the shapes GitHub answered for this repository: two check suites
# with no check run on every head (two GitHub Apps' queued suites), and a check run of any
# conclusion as a node of a suite.
#
# The repository the queue's part fetches is a fixture origin served over file://, which the
# script reaches as GITHUB_SERVER_URL/GITHUB_REPOSITORY.git, told to serve the filter and any
# commit by its sha as GitHub serves them, so the script's partial fetch and git merge-tree's
# fetch of the contents it merges both run as they do in CI. Main's commits are squash commits
# whose subjects end with (#<number>), but one, and the queued heads fork from main's root.
#
# Run from anywhere: .github/scripts/heads-without-runs.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

# The fixtures are judged on git's defaults wherever the harness runs, so every git variable the
# environment sets goes before any git runs, as priority-conflicts.test.sh does.
while read -r name; do
  unset "$name"
done < <(compgen -e GIT_)

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# has_line <text> <line>: true when <line> is a whole line of <text>, compared as a fixed string.
has_line() { grep -qxF -- "$2" <<<"$1" && echo true || echo false; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/fixtures"

cat > "$work/bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
fixtures=$STAND_IN/fixtures
if [ "$#" -eq 4 ] && [ "$1 $2 $3" = "api graphql --input" ]; then
  after=$(jq -r '.variables.after // "first"' "$4")
  operation=$(jq -r '.operationName' "$4")
  echo "$operation $after" >> "$STAND_IN/gh-calls"
  case $operation in
    OpenPulls) refusal=graphql-refuses-$after answer=page-$after.json ;;
    MainQueue) refusal=queue-refuses-$after answer=queue-$after.json ;;
    *) echo "unexpected operation $operation" >&2; exit 2 ;;
  esac
  if [ -e "$fixtures/$refusal" ]; then
    echo 'HTTP 502: Bad Gateway (https://api.github.com/graphql)' >&2
    exit 1
  fi
  cat "$fixtures/$answer"
elif [ "$#" -eq 5 ] && [ "$1 $2" = "api --paginate" ] && [[ $3 == repos/o/r/activity\?* ]] && [ "$4" = --jq ]; then
  echo "activity ${3#*\?}" >> "$STAND_IN/gh-calls"
  if [ -e "$fixtures/activity-refuses" ] || [ ! -e "$fixtures/activity.json" ]; then
    echo 'HTTP 502: Bad Gateway (https://api.github.com/repos/o/r/activity)' >&2
    exit 1
  fi
  jq -c "$5" "$fixtures/activity.json"
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

# queue_entry <number> <head> <position> <queued, seconds ago>: one entry of main's merge queue.
queue_entry() {
  jq -nc --argjson number "$1" --arg head "$2" --argjson position "$3" --arg queued "$(ago "$4")" \
    '{position: $position, state: "QUEUED", enqueuedAt: $queued, pullRequest: {number: $number, headRefOid: $head}}'
}

# queue_page <cursor of the request, or first> <hasNextPage> <endCursor as JSON> <entry>...: one
# page of the queue's answer.
queue_page() {
  local file=$1 next=$2 cursor=$3
  shift 3
  printf '%s\n' "$@" | jq -s --argjson next "$next" --argjson cursor "$cursor" \
    '{data: {repository: {mergeQueue: {entries: {pageInfo: {hasNextPage: $next, endCursor: $cursor}, nodes: .}}}}}' \
    > "$work/fixtures/queue-$file.json"
}

# fresh: no fixture but an empty queue, which a case that reads none of the queue answers with.
fresh() { rm -rf "$work/fixtures" && mkdir "$work/fixtures" && queue_page first false null; }

# git_fixture <args>: git with no configuration of the machine or of the user it runs as, and an
# identity for the fixture's commits.
git_fixture() {
  GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git -c user.name='CI test' \
    -c user.email=ci-test@example.com "$@"
}

# run_check [<script>]: runs the script (or a broken copy of it) over the fixtures; sets out
# (stdout), err (stderr), summary (the step summary) and status. Under `timeout`, so a read that
# hung fails its case instead of hanging the job. Git runs on its defaults here too, with the file
# protocol allowed to the fetches git merge-tree makes; server_url, when a case sets it, replaces
# the fixture origin's server.
run_check() {
  rm -f "$work/gh-calls" "$work/summary"
  set +e
  out=$(env -u JOB_TIMEOUT_MINUTES -u STEP_TIMEOUT_MINUTES PATH="$work/bin:$PATH" STAND_IN="$work" \
    GH_TOKEN=t GH_RETRY_SECONDS=0 MERGEABLE_RETRY_SECONDS=0 GITHUB_REPOSITORY=o/r \
    GITHUB_SERVER_URL="${server_url:-file://$work/server}" GIT_CONFIG_GLOBAL=/dev/null \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=protocol.file.allow \
    GIT_CONFIG_VALUE_0=always GITHUB_STEP_SUMMARY="$work/summary" \
    timeout 120 "${1:-$script_dir/heads-without-runs.sh}" 2> "$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
  summary=$(cat "$work/summary" 2> /dev/null || true)
}

gh_calls() { cat "$work/gh-calls" 2> /dev/null || true; }
pull_reads() { grep -c "^pull $1\$" "$work/gh-calls" || true; }
named() { contains "$out" "^#$1 $(sha "$1") "; }
errors() { grep -c '^::error::' <<< "$err" || true; }
warnings() { grep -c '^::warning::' <<< "$err" || true; }

# The fixture origin, at the path the script builds from GITHUB_SERVER_URL and GITHUB_REPOSITORY.
# A bare repository serves only its tips and ignores a filter unless told otherwise; GitHub serves
# both.
origin="$work/server/o/r.git"
mkdir -p "$work/server/o"
git_fixture init -q --bare "$origin"
git_fixture -C "$origin" config uploadpack.allowFilter true
git_fixture -C "$origin" config uploadpack.allowAnySHA1InWant true
tree="$work/tree"
git_fixture init -q -b main "$tree"

# body <prefix> [<line> <text>]...: nine lines, <prefix>1 to <prefix>9, each line named replaced by
# its text. Two edits with two unchanged lines between them merge cleanly.
body() {
  local prefix=$1 i lines=()
  shift
  for i in 1 2 3 4 5 6 7 8 9; do
    lines[i]="$prefix$i"
  done
  while [ $# -gt 0 ]; do
    lines[$1]=$2
    shift 2
  done
  printf '%s\n' "${lines[@]}"
}

# put <file> <text>: writes <text> and a newline to <file>.
put() { printf '%s\n' "$2" > "$1"; }

# on <parent> <message> <command>...: a commit on <parent> of what <command> does, run in the
# fixture's work tree; prints its sha.
on() {
  local parent=$1 message=$2
  shift 2
  git_fixture -C "$tree" checkout -q --detach "$parent"
  (cd "$tree" && "$@") > /dev/null
  git_fixture -C "$tree" add -A
  git_fixture -C "$tree" commit -q -m "$message"
  git_fixture -C "$tree" rev-parse HEAD
}

# merges <commit of main> <head>: git merge-tree's own exit status for the two, in the fixture.
merges() { git_fixture -C "$tree" merge-tree --write-tree "$1" "$2" > /dev/null 2>&1 && echo 0 || echo "$?"; }

for prefix in a b c e; do
  body "$prefix" > "$tree/$prefix.txt"
done
git_fixture -C "$tree" add -A
git_fixture -C "$tree" commit -q -m "the first commit"
root=$(git_fixture -C "$tree" rev-parse HEAD)
# Main after the root, one squash commit a landing. #701 sets a2, which #601, #605 and #606 set
# too; #702 sets a8, so the newest commit of main that touches a.txt is not the one that made the
# conflict; the e2 commit names no pull request.
m1=$(on "$root" "edit b2 (#700)" put b.txt "$(body b 2 'b2 main')")
m2=$(on "$m1" "edit a2 (#701)" put a.txt "$(body a 2 'a2 main')")
m3=$(on "$m2" "edit a8 (#702)" put a.txt "$(body a 2 'a2 main' 8 'a8 main')")
m4=$(on "$m3" "edit e2, a commit that names no pull request" put e.txt "$(body e 2 'e2 main')")
m5=$(on "$m4" "edit b8 (#703)" put b.txt "$(body b 2 'b2 main' 8 'b8 main')")
# The queued heads, each a branch of the root.
edit_a_and_e() { put a.txt "$(body a 2 'a2 six-oh-six')" && put e.txt "$(body e 2 'e2 six-oh-six')"; }
q601=$(on "$root" "edit a2" put a.txt "$(body a 2 'a2 six-oh-one')")
q602=$(on "$root" "edit c5" put c.txt "$(body c 5 'c5 six-oh-two')")
q603=$(on "$root" "edit e2" put e.txt "$(body e 2 'e2 six-oh-three')")
q605=$(on "$root" "edit a2 too" put a.txt "$(body a 2 'a2 six-oh-five')")
q606=$(on "$root" "edit a2 and e2" edit_a_and_e)
git_fixture -C "$tree" push -q "$origin" "$m5:refs/heads/main"
for pair in "601:$q601" "602:$q602" "603:$q603" "605:$q605" "606:$q606"; do
  git_fixture -C "$tree" push -q "$origin" "${pair#*:}:refs/pull/${pair%%:*}/head"
done

# main_activity: main's moves as the activity route lists them, newest first: the root five hours
# ago, then a commit an hour apart, the tip half an hour ago.
main_activity() {
  jq -n --arg m5 "$m5" --arg m4 "$m4" --arg m3 "$m3" --arg m2 "$m2" --arg m1 "$m1" --arg root "$root" \
    --arg t5 "$(ago 1800)" --arg t4 "$(ago 3600)" --arg t3 "$(ago 7200)" --arg t2 "$(ago 10800)" \
    --arg t1 "$(ago 14400)" --arg t0 "$(ago 18000)" '
    [[$m5, $m4, $t5], [$m4, $m3, $t4], [$m3, $m2, $t3], [$m2, $m1, $t2], [$m1, $root, $t1], [$root, null, $t0]]
    | map({after: .[0], before: .[1], timestamp: .[2], ref: "refs/heads/main", activity_type: "merge_queue_merge"})' \
    > "$work/fixtures/activity.json"
}

# dirty_queue: five entries, queued as main moved. #601 and #606 were queued between #700 and #701,
# #605 between #701 and #702, #603 between #702 and the e2 commit, #602 ten minutes ago.
dirty_queue() {
  queue_page first false null \
    "$(queue_entry 601 "$q601" 1 12600)" \
    "$(queue_entry 602 "$q602" 2 600)" \
    "$(queue_entry 603 "$q603" 3 5400)" \
    "$(queue_entry 605 "$q605" 4 9000)" \
    "$(queue_entry 606 "$q606" 5 12600)"
  main_activity
}

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
  "$(is "$(grep '^OpenPulls' "$work/gh-calls")" $'OpenPulls first\nOpenPulls c1')"
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
check "it tried three times" "$(is "$(grep -c '^OpenPulls first' "$work/gh-calls")" 3)"
check "it never says it named none" "$(is "$(contains "$out$summary" 'Named none')" false)"
check "the summary says UNKNOWN" "$(contains "$summary" '^UNKNOWN: ')"

echo "case: a second page GitHub refused is UNKNOWN, and the first page's heads are not named"
fresh
page first true '"c1"' "$(node 101 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')"
touch "$work/fixtures/graphql-refuses-c1"
pull 101 true
run_check
check "it exits 2" "$(is "$status" 2)"
check "it names no head" "$(is "$(contains "$out" '^#')" false)"
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
check "it names no head" "$(is "$(contains "$out" '^#')" false)"

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
  "$(has_line "$out" 'Named none: no open pull request that is no draft has a head 20 minutes old or more with no check run (3 open pull requests read, 1 of them drafts).')"
check "it reads no pull request" "$(is "$(grep -c '^pull' "$work/gh-calls" || true)" 0)"

echo "case: the fixture: each planted conflict changed a tree, and git itself finds it"
# The measuring method's own trap: a plant that left the tree as it was merged clean.
# tree_of <commit>: its tree, in the fixture.
tree_of() { git_fixture -C "$tree" rev-parse "$1^{tree}"; }
check "#601's head changed the tree it forked from" "$(is "$(is "$(tree_of "$q601")" "$(tree_of "$root")")" false)"
check "#701's commit changed main's tree" "$(is "$(is "$(tree_of "$m2")" "$(tree_of "$m1")")" false)"
check "git merge-tree finds #601's head conflicts with main's tip" "$(is "$(merges "$m5" "$q601")" 1)"
check "and merges it cleanly with main as it was when #601 was queued" "$(is "$(merges "$m1" "$q601")" 0)"
check "and finds #602's head clean with main's tip" "$(is "$(merges "$m5" "$q602")" 0)"

echo "case: a queued entry whose head has come to conflict with main is named, with its files and the pull request that caused it"
# conflict_heads: a listing whose one named head conflicts with its base, so the heads pass.
conflict_heads() {
  page first false null \
    "$(node 250 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')" \
    "$(node 100 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0,1]')"
  pull 250 false
}
fresh
conflict_heads
dirty_queue
run_check
check "#601 is named with its head, its position, its file and the pull request whose commit made it conflict" \
  "$(has_line "$out" "queued #601 $q601 position 1 conflicts with main in a.txt; caused by #701")"
check "a ::warning:: line says the same" \
  "$(has_line "$err" "::warning::#601 is in main's merge queue at position 1, and its head $q601 conflicts with main in a.txt; caused by #701")"
check "an entry that conflicts in two files names both, and the first commit of main that made it conflict" \
  "$(has_line "$out" "queued #606 $q606 position 5 conflicts with main in a.txt, e.txt; caused by #701")"
check "a commit whose subject names no pull request is UNKNOWN" \
  "$(has_line "$out" "queued #603 $q603 position 3 conflicts with main in e.txt; caused by UNKNOWN: main's commit $m4 made it conflict, and its subject names no pull request")"
check "an entry that conflicted already when it was queued is UNKNOWN, not laid on a later commit" \
  "$(has_line "$out" "queued #605 $q605 position 4 conflicts with main in a.txt; caused by UNKNOWN: its head conflicted with main already when it was queued, at $m2")"
check "main's activity is read once, over the past day" \
  "$(is "$(grep '^activity' "$work/gh-calls")" "activity ref=refs/heads/main&time_period=day&per_page=100")"
check "the summary holds #601's row" \
  "$(has_line "$summary" "| #601 | \`$q601\` | 1 | a.txt | #701 |")"
check "the last line counts the entries" \
  "$(has_line "$out" "Queue: 4 of 5 entries of main's merge queue conflict with main's tip $m5 (warnings; they fail nothing), 0 UNKNOWN.")"

echo "case: a clean queued entry is not named"
check "#602 is not named" "$(is "$(contains "$out$err" '#602')" false)"

echo "case: a queued entry that conflicts does not fail the job; a head with no run and no conflict still does"
check "it passes" "$(is "$status" 0)"
check "one ::warning:: line for each entry that conflicts" "$(is "$(warnings)" 4)"
check "no ::error:: line" "$(is "$(errors)" 0)"
fresh
page first false null \
  "$(node 101 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')" \
  "$(node 250 false "$(ago "$five_days")" "$(ago "$three_hours")" '[0,0]')"
pull 101 true
pull 250 false
dirty_queue
run_check
check "with a head with no run and no conflict, it fails" "$(is "$status" 1)"
check "the entries are still warnings" "$(is "$(warnings)" 4)"
check "the one ::error:: line is that head's" \
  "$(is "$(errors) $(contains "$err" '^::error::#101: ')" "1 true")"

echo "case: a queue read GitHub refused is UNKNOWN, never none, and the heads are still reported"
touch "$work/fixtures/queue-refuses-first"
run_check
check "it exits 2" "$(is "$status" 2)"
check "it says UNKNOWN and why" \
  "$(has_line "$err" "::error::UNKNOWN: GitHub's GraphQL API refused the list of main's merge queue at every attempt, so this check read no entry of main's merge queue: it names none and clears none")"
check "it tried three times" "$(is "$(grep -c '^MainQueue first' "$work/gh-calls")" 3)"
check "it never says no entry conflicts, or that the queue is empty" \
  "$(is "$(contains "$out$summary" "No entry of main\|holds no entry")" false)"
check "it names no entry and reads no activity" \
  "$(is "$(contains "$out" '^queued ') $(grep -c '^activity' "$work/gh-calls" || true)" "false 0")"
check "the heads are still named" "$(named 101)"

echo "case: an answer that names no merge queue for main is UNKNOWN"
fresh
conflict_heads
echo '{"data": {"repository": {"mergeQueue": null}}}' > "$work/fixtures/queue-first.json"
run_check
check "it exits 2" "$(is "$status" 2)"
check "it says why" \
  "$(contains "$err" "^::error::UNKNOWN: GitHub's GraphQL API answered the list of main's merge queue in a shape this check does not read, or named no merge queue for main, ")"

echo "case: a fetch git could not make is UNKNOWN"
fresh
conflict_heads
dirty_queue
server_url="file://$work/nowhere" run_check
check "it exits 2" "$(is "$status" 2)"
check "it says what it could not fetch" \
  "$(has_line "$err" "::error::UNKNOWN: git could not fetch main and the queued heads from file://$work/nowhere/o/r.git, so this check read no entry of main's merge queue: it names none and clears none")"
check "it names no entry" "$(is "$(contains "$out" '^queued ')" false)"

echo "case: main's activity GitHub refused leaves the cause UNKNOWN, and the entry is still named"
touch "$work/fixtures/activity-refuses"
run_check
check "it passes" "$(is "$status" 0)"
check "#601 is named, its cause UNKNOWN" \
  "$(has_line "$out" "queued #601 $q601 position 1 conflicts with main in a.txt; caused by UNKNOWN: GitHub's REST API refused main's activity at every attempt")"

echo "case: a queue whose entries are all clean says so, with how many it read"
fresh
conflict_heads
queue_page first false null "$(queue_entry 602 "$q602" 1 600)"
run_check
check "it passes" "$(is "$status" 0)"
check "it says no entry conflicts" \
  "$(has_line "$out" "No entry of main's merge queue conflicts with main's tip $m5 (1 read).")"
check "it reads no activity" "$(is "$(grep -c '^activity' "$work/gh-calls" || true)" 0)"

echo "case: an empty queue says so"
fresh
conflict_heads
run_check
check "it passes" "$(is "$status" 0)"
check "it says the queue holds no entry" \
  "$(has_line "$out" "main's merge queue holds no entry: nothing to merge with main.")"

summary "the check that names open pull request heads no workflow ran and queued entries that conflict with main"
