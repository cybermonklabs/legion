#!/usr/bin/env bash
# Tests for `priority-conflicts.sh`: the check `.github/workflows/ci.yaml#priority-conflicts` job
# runs on every pull request and merge group, which fails a change that would land a conflict
# with an approved or queued pull request labelled priority.
#
# Each case runs the script with a stand-in `gh` first on PATH, which answers its GraphQL read of
# the priority pull requests with a fixture page in GraphQL's own shape (the page its `after`
# cursor asks for), refuses it when a case asks, and records each read. The repository it fetches
# is a fixture origin served over file://, which the script reaches as
# GITHUB_SERVER_URL/GITHUB_REPOSITORY.git, told to serve the filter and any commit by its sha as
# GitHub serves them, so the script's partial fetch and git merge-tree's fetch of the file contents
# it merges both run as they do in CI. The fixture's commits have the real shapes: a pull request's
# run commit is a merge of its head into main's tip with main first, a merge group is squash
# commits whose subjects end with (#<number>) on the group's base, and one priority pull request
# forked from main before a later commit of main that conflicts with it. The titles hold a percent
# sign, which an ::error:: line must escape.
#
# Run from anywhere: .github/scripts/priority-conflicts.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

# The fixtures are judged on git's defaults wherever the harness runs, so every git variable the
# environment sets goes before any git runs (edited-migrations-base.test.sh says which ones a
# Legion pod sets).
while read -r name; do
  unset "$name"
done < <(compgen -e GIT_)

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$script_dir/priority-conflicts.sh"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# has_line <text> <line>: true when <line> is a whole line of <text>, compared as a fixed string.
has_line() { grep -qxF -- "$2" <<<"$1" && echo true || echo false; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/temp"

# git_fixture <args>: git with no configuration of the machine or of the user it runs as, and an
# identity for the fixture's commits.
git_fixture() {
  GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git -c user.name='CI test' \
    -c user.email=ci-test@example.com "$@"
}

# The origin, at the path the script builds from GITHUB_SERVER_URL and GITHUB_REPOSITORY. A bare
# repository serves only its tips and ignores a filter unless told otherwise; GitHub serves both.
server="$work/server"
origin="$server/o/r.git"
mkdir -p "$server/o"
git_fixture init -q --bare "$origin"
git_fixture -C "$origin" config uploadpack.allowFilter true
git_fixture -C "$origin" config uploadpack.allowAnySHA1InWant true

fixture="$work/fixture"
git_fixture init -q -b main "$fixture"

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

# commit_on <parent> <message> <file> <text>: a commit on <parent> that writes <text> to <file>;
# prints its sha.
commit_on() {
  git_fixture -C "$fixture" checkout -q --detach "$1"
  mkdir -p "$(dirname "$fixture/$3")"
  printf '%s\n' "$4" > "$fixture/$3"
  git_fixture -C "$fixture" add -A
  git_fixture -C "$fixture" commit -q -m "$2"
  git_fixture -C "$fixture" rev-parse HEAD
}

# rename_on <parent> <message> <from> <to>: a commit on <parent> that renames <from> to <to>, its
# content as it was; prints its sha.
rename_on() {
  git_fixture -C "$fixture" checkout -q --detach "$1"
  git_fixture -C "$fixture" mv "$3" "$4"
  git_fixture -C "$fixture" commit -q -m "$2"
  git_fixture -C "$fixture" rev-parse HEAD
}

# merge_of <base> <head>: GitHub's test merge of a pull request's head into its base, the base
# first; prints its sha.
merge_of() {
  git_fixture -C "$fixture" checkout -q --detach "$1"
  git_fixture -C "$fixture" merge -q --no-ff --no-edit "$2" > /dev/null
  git_fixture -C "$fixture" rev-parse HEAD
}

for prefix in s t o q; do
  body "$prefix" > "$fixture/$prefix.txt"
done
git_fixture -C "$fixture" add -A
git_fixture -C "$fixture" commit -q -m "the first commit"
root=$(git_fixture -C "$fixture" rev-parse HEAD)
# main's tip: a later commit of main that edits t2, which a stale priority pull request edits too.
main=$(commit_on "$root" "main edits t2" t.txt "$(body t 2 't2 main')")

# The priority pull requests' heads. #12 forked before main's t2 and edits t2 and t5, so it
# conflicts with main at t2 and lands t5 cleanly.
p11=$(commit_on "$main" "priority edits s5" s.txt "$(body s 5 's5 priority')")
p12=$(commit_on "$root" "stale priority edits t2 and t5" t.txt "$(body t 2 't2 stale' 5 't5 stale')")
p13=$(commit_on "$main" "unapproved priority edits o5" o.txt "$(body o 5 'o5 unapproved')")
p14=$(commit_on "$main" "queued priority edits q5" q.txt "$(body q 5 'q5 queued')")
p15=$(commit_on "$main" "priority adds a directory d" d/inner.txt "inner")

# Pull request #20's heads, one per case, and the run commit of each: its merge into main.
m_conflict=$(merge_of "$main" "$(commit_on "$main" "edit s5" s.txt "$(body s 5 's5 mine')")")
x_conflict=$(git_fixture -C "$fixture" rev-parse "$m_conflict^2")
m_new=$(merge_of "$main" "$(commit_on "$main" "add a file" new.txt "new")")
m_far=$(merge_of "$main" "$(commit_on "$main" "edit s1" s.txt "$(body s 1 's1 mine')")")
m_apart=$(merge_of "$main" "$(commit_on "$main" "edit t8" t.txt "$(body t 2 't2 main' 8 't8 mine')")")
m_inside=$(merge_of "$main" "$(commit_on "$main" "edit t5" t.txt "$(body t 2 't2 main' 5 't5 mine')")")
m_rename=$(merge_of "$main" "$(rename_on "$main" "rename t to u" t.txt u.txt)")
m_file=$(merge_of "$main" "$(commit_on "$main" "add a file d" d "a file")")
m_unapproved=$(merge_of "$main" "$(commit_on "$main" "edit o5" o.txt "$(body o 5 'o5 mine')")")
m_queue=$(merge_of "$main" "$(commit_on "$main" "edit q5" q.txt "$(body q 5 'q5 mine')")")

# Merge groups: squash commits on the group's base. q11 is #11's squash in a group in front, and
# g30 and g31 are one group of two pull requests.
g20=$(commit_on "$main" "edit s5 (#20)" s.txt "$(body s 5 's5 mine')")
q11=$(commit_on "$main" "priority edits s5 (#11)" s.txt "$(body s 5 's5 priority')")
g21=$(commit_on "$q11" "edit s5 after the priority (#21)" s.txt "$(body s 5 's5 mine after')")
g22=$(commit_on "$main" "a commit that names no pull request" new.txt "new")
g30=$(commit_on "$main" "add a file (#30)" thirty.txt "thirty")
g31=$(commit_on "$g30" "edit s5 (#31)" s.txt "$(body s 5 's5 mine')")

for sha in "$p11" "$p12" "$p13" "$p14" "$p15" "$m_conflict" "$m_new" "$m_far" "$m_apart" "$m_inside" \
  "$m_rename" "$m_file" "$m_unapproved" "$m_queue" "$g20" "$g21" "$g22" "$g31"; do
  git_fixture -C "$fixture" push -q "$origin" "$sha:refs/fixture/$sha"
done

# The stand-in gh: the page its `after` cursor asks for, answer-first.json for the first.
cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
set -euo pipefail
[ "\$1 \$2 \$3" = "api graphql --input" ] && [ \$# -eq 4 ] || { echo "unexpected gh \$*" >&2; exit 2; }
echo "graphql \$(jq -r '.operationName' "\$4") \$(jq -c '.variables' "\$4")" >> "$work/gh-calls"
if [ -e "$work/gh-refuses" ]; then
  echo 'HTTP 502: Bad Gateway (https://api.github.com/graphql)' >&2
  exit 1
fi
cat "$work/answer-\$(jq -r '.variables.after // "first"' "\$4").json"
GH
chmod +x "$work/bin/gh"

# pull <number> <head> <reviewDecision as JSON> <isInMergeQueue>: one node of the label read.
pull() {
  jq -n --argjson number "$1" --arg head "$2" --argjson decision "$3" --argjson queued "$4" \
    '{number: $number, title: "priority work \($number) at 100%", headRefOid: $head,
      reviewDecision: $decision, isInMergeQueue: $queued}'
}

# page <file> <hasNextPage> <endCursor as JSON> <node>...: one page of the label read's answer.
page() {
  local file=$1 next=$2 cursor=$3
  shift 3
  printf '%s\n' "$@" | jq -s --argjson next "$next" --argjson cursor "$cursor" \
    '{data: {repository: {pullRequests: {pageInfo: {hasNextPage: $next, endCursor: $cursor}, nodes: .}}}}' \
    > "$work/$file"
}

# answer <node>...: the label read's answer, one page of those nodes.
answer() { page answer-first.json false null "$@"; }

# The priority pull requests: #11, #12 and #15 approved, #13 waiting for review, #14 queued.
standard=("$(pull 11 "$p11" '"APPROVED"' false)" "$(pull 12 "$p12" '"APPROVED"' false)"
  "$(pull 13 "$p13" '"REVIEW_REQUIRED"' false)" "$(pull 14 "$p14" null true)"
  "$(pull 15 "$p15" '"APPROVED"' false)")

pr_event() {
  jq -n --argjson number "$1" '{pull_request: {number: $number, base: {ref: "main"}}}' > "$work/event.json"
}

# group_event <number> <base sha> [<head_ref>]: the payload of a queued group of that pull request.
group_event() {
  jq -n --arg ref "${3:-refs/heads/gh-readonly-queue/main/pr-$1-$2}" --arg base "$2" \
    '{merge_group: {head_ref: $ref, base_sha: $base, base_ref: "refs/heads/main"}}' > "$work/event.json"
}

# run_check <event name> <sha>: runs the script over $work/event.json with GITHUB_SHA <sha>; sets
# out (stdout), err (stderr) and status. Every case runs it under `timeout`, so a fetch that hung
# would fail its case instead of hanging the job. Git runs on its defaults here too, with the file
# protocol allowed to the fetches git merge-tree makes.
run_check() {
  rm -rf "$work/gh-calls" "$work/temp"/*
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GH_RETRY_SECONDS=0 GITHUB_REPOSITORY=o/r \
    GITHUB_SERVER_URL="file://$server" RUNNER_TEMP="$work/temp" GITHUB_EVENT_NAME="$1" \
    GITHUB_EVENT_PATH="$work/event.json" GITHUB_SHA="$2" GIT_CONFIG_GLOBAL=/dev/null \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=protocol.file.allow \
    GIT_CONFIG_VALUE_0=always timeout 120 "$script" 2> "$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
}

gh_calls() { cat "$work/gh-calls" 2> /dev/null || true; }
read_once='graphql PriorityPulls {"owner":"o","repo":"r","after":null}'
fetched() { compgen -G "$work/temp/priority-conflicts.*/repo.git" > /dev/null && echo yes || echo no; }
# clean_line <number> <change>: the regex of the line naming a priority pull request that merges
# cleanly with the change; the seconds it took vary.
clean_line() { echo "^#$1 (priority work $1 at 100%): no conflict with $2 ([0-9]* s)\.\$"; }
remedy_pr="A priority pull request merges first: wait for #%s to merge, then merge main into this pull request's branch and resolve the conflict there."
# conflict_line <priority number> <files>: the error a conflict of pull request #20 writes.
conflict_line() {
  # shellcheck disable=SC2059 # the remedy is the format
  echo "::error::pull request #20 conflicts with priority pull request #$1 (priority work $1 at 100%25) in $2. $(printf "$remedy_pr" "$1")"
}

echo "case: a dispatched run lands nothing, so it reads nothing"
echo '{}' > "$work/event.json"
run_check workflow_dispatch "$main"
check "it passes" "$(is "$status" 0)"
check "it says why" "$(has_line "$out" "A workflow_dispatch run lands nothing on a branch, so it cannot land ahead of a priority pull request: nothing to check.")"
check "it reads no route" "$(is "$(gh_calls)" "")"

answer "${standard[@]}"
pr_event 20

echo "case: a pull request that edits the lines an approved priority pull request edits fails"
run_check pull_request "$m_conflict"
check "it fails" "$(is "$status" 1)"
check "it names the priority pull request, its title escaped, the file and the remedy" \
  "$(has_line "$err" "$(conflict_line 11 s.txt)")"
check "it names no other" "$(is "$(grep -c '^::error::' <<<"$err")" 1)"
check "it reads the priority pull requests once" "$(is "$(gh_calls)" "$read_once")"
check "it says the stale one already conflicts with the base" \
  "$(has_line "$out" "#12 (priority work 12 at 100%) already conflicts with the base of pull request #20 in 1 file(s): those are its own to resolve, and count against pull request #20 only where pull request #20 edits them too.")"
check "and that it is no conflict of the pull request's" "$(contains "$out" "$(clean_line 12 "pull request #20")")"
check "it does not check the one that is neither approved nor queued" "$(is "$(contains "$out$err" "#13")" false)"

echo "case: a pull request that edits no file a priority pull request edits passes"
run_check pull_request "$m_new"
check "it passes" "$(is "$status" 0)"
check "it merged each" "$(contains "$out" "$(clean_line 11 "pull request #20")")"
check "it counts the four it checked" \
  "$(has_line "$out" "pull request #20 lands no conflict on an approved or queued priority pull request: 4 checked.")"
check "it writes no error" "$(is "$err" "")"

echo "case: a pull request that edits a priority pull request's file away from its lines passes"
run_check pull_request "$m_far"
check "it passes" "$(is "$status" 0)"
check "it says so" "$(contains "$out" "$(clean_line 11 "pull request #20")")"

echo "case: an edit apart from a stale priority pull request's own conflict with the base passes"
# #12 conflicts with main at t2 and lands t5; the pull request edits t8 of that file, so its plain
# merge with #12 conflicts at t2, which is #12's own conflict with main and not the pull request's.
run_check pull_request "$m_apart"
check "it passes" "$(is "$status" 0)"
check "it says whose conflict it is" "$(contains "$out" "^#12 (priority work 12 at 100%) already conflicts with the base")"
check "and that the pull request adds none" "$(contains "$out" "$(clean_line 12 "pull request #20")")"

echo "case: a new conflict in a file a stale priority pull request already conflicts on fails"
# The pull request edits t5, which #12 edits too, in the file where #12 already conflicts at t2.
run_check pull_request "$m_inside"
check "it fails" "$(is "$status" 1)"
check "it names #12 and the file" "$(has_line "$err" "$(conflict_line 12 t.txt)")"

echo "case: a rename of the file a stale priority pull request conflicts on is not a conflict"
run_check pull_request "$m_rename"
check "it passes" "$(is "$status" 0)"
check "it merged #12 through the rename" "$(contains "$out" "$(clean_line 12 "pull request #20")")"

echo "case: a file where a priority pull request adds a directory fails"
run_check pull_request "$m_file"
check "it fails" "$(is "$status" 1)"
check "it names #15 and the path, without the name git moves the file aside to" \
  "$(has_line "$err" "$(conflict_line 15 d)")"

echo "case: a priority pull request that is neither approved nor queued is not checked against"
run_check pull_request "$m_unapproved"
check "it passes, though the two edit one line" "$(is "$status" 0)"
check "it names it nowhere" "$(is "$(contains "$out$err" "#13")" false)"

echo "case: a priority pull request in the merge queue counts as an approved one"
run_check pull_request "$m_queue"
check "it fails" "$(is "$status" 1)"
check "it names the queued one" "$(has_line "$err" "$(conflict_line 14 q.txt)")"

echo "case: a pull request labelled priority passes, and fetches nothing"
answer "${standard[@]}" "$(pull 20 "$x_conflict" null false)"
run_check pull_request "$m_conflict"
answer "${standard[@]}"
check "it passes" "$(is "$status" 0)"
check "it says why" "$(has_line "$out" "#20 carries the label priority, and priority pull requests are not held to each other: nothing to check.")"
check "it fetches nothing" "$(is "$(fetched)" no)"

echo "case: a label read GitHub refuses fails the check rather than pass it"
touch "$work/gh-refuses"
run_check pull_request "$m_new"
rm "$work/gh-refuses"
check "it fails" "$(is "$status" 1)"
check "it says it could not read" \
  "$(has_line "$err" "::error::GitHub's GraphQL API refused the read of the open pull requests labelled priority at every attempt, so this check cannot tell whether pull request #20 conflicts with a priority pull request, and fails rather than pass unread")"
check "it tried three times" "$(is "$(gh_calls | wc -l)" 3)"
check "it fetches nothing" "$(is "$(fetched)" no)"

echo "case: a priority pull request on the label read's second page is checked against"
page answer-first.json true '"c1"' "$(pull 11 "$p11" '"APPROVED"' false)"
page answer-c1.json false null "$(pull 14 "$p14" null true)"
run_check pull_request "$m_queue"
check "it fails" "$(is "$status" 1)"
check "it names the one on the second page" "$(has_line "$err" "$(conflict_line 14 q.txt)")"
check "it read both pages" \
  "$(is "$(gh_calls)" "$read_once"$'\n''graphql PriorityPulls {"owner":"o","repo":"r","after":"c1"}')"

echo "case: a page that says another follows and names no cursor fails the check"
page answer-first.json true null "$(pull 11 "$p11" '"APPROVED"' false)"
run_check pull_request "$m_new"
check "it fails" "$(is "$status" 1)"
check "it says so" "$(contains "$err" "^::error::GitHub's GraphQL API said another page of the open pull requests labelled priority follows, and named no new cursor for it, ")"
answer "${standard[@]}"

echo "case: an answer of another shape fails the check"
echo '{"data": null, "errors": [{"message": "Something went wrong"}]}' > "$work/answer-first.json"
run_check pull_request "$m_new"
answer "${standard[@]}"
check "it fails" "$(is "$status" 1)"
check "it says it could not read the answer" \
  "$(contains "$err" "^::error::GitHub's GraphQL API answered the read of the open pull requests labelled priority in a shape this check does not read, ")"

echo "case: a priority head git cannot fetch fails the check rather than pass it"
answer "$(pull 11 1111111111111111111111111111111111111111 '"APPROVED"' false)"
run_check pull_request "$m_new"
answer "${standard[@]}"
check "it fails" "$(is "$status" 1)"
check "it says it could not fetch" "$(contains "$err" "^::error::git could not fetch the commit of pull request #20 and the heads of the priority pull requests it is checked against from file://")"

echo "case: a pull request run whose commit is no merge fails"
run_check pull_request "$x_conflict"
check "it fails" "$(is "$status" 1)"
check "it says why" "$(contains "$err" "^::error::GITHUB_SHA $x_conflict is no merge commit")"

echo "case: a merge group that edits an approved priority pull request's lines fails"
group_event 20 "$main"
run_check merge_group "$g20"
check "it fails" "$(is "$status" 1)"
check "it names the group's pull request, the priority one, the file and the remedy" \
  "$(has_line "$err" "::error::the merge group of #20 conflicts with priority pull request #11 (priority work 11 at 100%25) in s.txt. A priority pull request merges first: wait for #11 to merge, then merge main into #20's branch, resolve the conflict and queue #20 again.")"

echo "case: a merge group whose one pull request is labelled priority passes"
answer "${standard[@]}" "$(pull 20 "$x_conflict" '"APPROVED"' true)"
run_check merge_group "$g20"
answer "${standard[@]}"
check "it passes" "$(is "$status" 0)"
check "it says why" "$(has_line "$out" "Every pull request of the merge group of #20 (#20) carries the label priority, and priority pull requests are not held to each other: nothing to check.")"

echo "case: a priority pull request whose squash commit is in the group's base is no conflict"
# #11 stands in the group in front; this group edits s5 again, which #11's head edits from main.
group_event 21 "$q11"
run_check merge_group "$g21"
check "it passes" "$(is "$status" 0)"
check "it merged #11 as it already stands in the base" "$(contains "$out" "$(clean_line 11 "the merge group of #21")")"

echo "case: a group of two pull requests, one of them labelled priority, is checked"
group_event 31 "$main"
answer "${standard[@]}" "$(pull 30 "$g30" null false)"
run_check merge_group "$g31"
check "it fails" "$(is "$status" 1)"
check "it names the group by the pull request its branch names" \
  "$(contains "$err" "^::error::the merge group of #31 conflicts with priority pull request #11 (priority work 11 at 100%25) in s.txt. ")"

echo "case: a group of two pull requests, both labelled priority, passes"
answer "${standard[@]}" "$(pull 30 "$g30" null false)" "$(pull 31 "$g31" null false)"
run_check merge_group "$g31"
answer "${standard[@]}"
check "it passes" "$(is "$status" 0)"
check "it names both" "$(has_line "$out" "Every pull request of the merge group of #31 (#31 #30) carries the label priority, and priority pull requests are not held to each other: nothing to check.")"

echo "case: a merge group commit that names no pull request fails"
group_event 22 "$main"
run_check merge_group "$g22"
check "it fails" "$(is "$status" 1)"
check "it names the commit" "$(contains "$err" "^::error::commit $g22 of the merge group of #22, 'a commit that names no pull request', names no pull request")"

echo "case: a merge group whose commits do not hold the pull request its branch names fails"
group_event 23 "$main"
run_check merge_group "$g20"
check "it fails" "$(is "$status" 1)"
check "it names both" "$(contains "$err" "^::error::the commits of the merge group of #23 name #20 and not #23, ")"

echo "case: a merge group branch that names no pull request fails before any read"
group_event 20 "$main" refs/heads/main
run_check merge_group "$g20"
check "it fails" "$(is "$status" 1)"
check "it names the ref" "$(contains "$err" "^::error::merge_group.head_ref 'refs/heads/main' names no queued pull request ")"
check "it reads no route" "$(is "$(gh_calls)" "")"

summary "the priority pull request conflict check"
