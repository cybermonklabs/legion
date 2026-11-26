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
# commits whose subjects end with (#<number>) on the group's base, and priority pull requests fork
# from main before later commits of main that conflict with them. The titles hold a percent sign,
# which an ::error:: line must escape.
#
# Every case writes the event it runs on (run_pr, run_group), and run_check sets the label read's
# answer back to the standard one after every run, so a case reads no other case's fixture.
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

# binary [<line> <text>]...: a binary file, a NUL byte and the numbers 1 to 400, a line each, each
# line named replaced by its text.
binary() {
  local script=''
  while [ $# -gt 0 ]; do
    script+="s/^$1\$/$2/;"
    shift 2
  done
  printf 'binary\0'
  seq 1 400 | sed "$script"
}

# put <file> <text>: writes <text> and a newline to <file>, making its directory.
put() {
  mkdir -p "$(dirname "$1")"
  printf '%s\n' "$2" > "$1"
}

# put_binary <file> [<line> <text>]...: writes the binary file `binary` makes to <file>.
put_binary() {
  local file=$1
  shift
  binary "$@" > "$file"
}

# on <parent> <message> <command>...: a commit on <parent> of what <command> does, run in the
# fixture's work tree (put, put_binary, `git_fixture mv` or `git_fixture rm`); prints its sha.
on() {
  local parent=$1 message=$2
  shift 2
  git_fixture -C "$fixture" checkout -q --detach "$parent"
  (cd "$fixture" && "$@") > /dev/null
  git_fixture -C "$fixture" add -A
  git_fixture -C "$fixture" commit -q -m "$message"
  git_fixture -C "$fixture" rev-parse HEAD
}

# merge_of <base> <head>: GitHub's test merge of a pull request's head into its base, the base
# first; prints its sha.
merge_of() {
  git_fixture -C "$fixture" checkout -q --detach "$1"
  git_fixture -C "$fixture" merge -q --no-ff --no-edit "$2" > /dev/null
  git_fixture -C "$fixture" rev-parse HEAD
}

# r.txt's lines are long, so git finds its rename after a merge has added conflict markers to it;
# n.txt's are short, so git finds that one only at the script's looser second try. k.txt starts
# with a line that reads like a conflict marker.
rp=r-a-line-of-the-file-two-sides-rename-long-enough-for-git-to-find-the-rename-
for prefix in s t o q w x y n; do
  body "$prefix" > "$fixture/$prefix.txt"
done
body "$rp" > "$fixture/r.txt"
body k 1 '<<<<<<< fixture' > "$fixture/k.txt"
binary > "$fixture/b.bin"
binary > "$fixture/v.bin"
git_fixture -C "$fixture" add -A
git_fixture -C "$fixture" commit -q -m "the first commit"
root=$(git_fixture -C "$fixture" rev-parse HEAD)
# main's tip: later commits of main that edit t2, which two stale priority pull requests edit too,
# set w5 as one of them, #16, also does, and edit r2, k3, y2, n2 and line 100 of b.bin and v.bin.
main=$(on "$root" "main edits t2" put t.txt "$(body t 2 't2 main')")
main=$(on "$main" "main sets w5 as #16 does" put w.txt "$(body w 5 'w5 shared')")
main=$(on "$main" "main edits r2" put r.txt "$(body "$rp" 2 "${rp}2 main")")
main=$(on "$main" "main edits k3" put k.txt "$(body k 1 '<<<<<<< fixture' 3 'k3 main')")
main=$(on "$main" "main edits y2" put y.txt "$(body y 2 'y2 main')")
main=$(on "$main" "main edits n2" put n.txt "$(body n 2 'n2 main')")
main=$(on "$main" "main edits b" put_binary b.bin 100 main)
main=$(on "$main" "main edits v" put_binary v.bin 100 main)

# The priority pull requests' heads.
p11=$(on "$main" "priority edits s5" put s.txt "$(body s 5 's5 priority')")
# #12 forked before main's t2 and edits t2 and t5: it conflicts with main at t2, lands t5 cleanly.
p12=$(on "$root" "stale priority edits t2 and t5" put t.txt "$(body t 2 't2 stale' 5 't5 stale')")
p13=$(on "$main" "unapproved priority edits o5" put o.txt "$(body o 5 'o5 unapproved')")
p14=$(on "$main" "queued priority edits q5" put q.txt "$(body q 5 'q5 queued')")
p15=$(on "$main" "priority adds a directory d" put d/inner.txt "inner")
# #16 forked there too, edits t2, and sets w5 as main does, which merges cleanly with main.
p16=$(on "$root" "stale priority edits t2" put t.txt "$(body t 2 't2 sixteen')")
p16=$(on "$p16" "stale priority sets w5" put w.txt "$(body w 5 'w5 shared')")
# #17 forked there too, renames r.txt to g.txt and edits r2: it conflicts with main at r2 of g.txt.
p17=$(on "$root" "stale priority renames r to g" git_fixture mv r.txt g.txt)
p17=$(on "$p17" "stale priority edits r2" put g.txt "$(body "$rp" 2 "${rp}2 seventeen")")
# #18 forked there too and deletes k.txt, which main edits: a modify/delete conflict.
p18=$(on "$root" "stale priority deletes k" git_fixture rm -q k.txt)
# #19 forked there too and edits line 300 of b.bin, which main edits too: a binary conflict.
p19=$(on "$root" "stale priority edits b" put_binary b.bin 300 nineteen)
# #40 and #50 are stacked on x_one, a branch of the root that a pull request of its own lands: each
# edits x2 on top of x_one's x2. #40 edits y2 too, which main edits: it is stale; #50 is not.
x_one=$(on "$root" "a change #40 and #50 are stacked on edits x2" put x.txt "$(body x 2 'x2 one')")
p40=$(on "$x_one" "stacked priority edits x2" put x.txt "$(body x 2 'x2 two')")
p40=$(on "$p40" "stacked priority edits y2" put y.txt "$(body y 2 'y2 forty')")
p50=$(on "$x_one" "fresh stacked priority edits x2" put x.txt "$(body x 2 'x2 fifty')")
# #41 forked there too, renames v.bin to z.bin and edits its line 300: a binary conflict with main
# at z.bin.
p41=$(on "$root" "stale priority renames v to z" git_fixture mv v.bin z.bin)
p41=$(on "$p41" "stale priority edits z" put_binary z.bin 300 forty-one)
# #60 forked there too, renames the short n.txt to p.txt and edits n2: it conflicts with main at n2
# of p.txt, and its copy of p.txt with conflict markers is too little like n.txt for git's 50%.
p60=$(on "$root" "stale priority renames n to p" git_fixture mv n.txt p.txt)
p60=$(on "$p60" "stale priority edits n2" put p.txt "$(body n 2 'n2 sixty')")

# Pull request #20's heads, one per case, and the run commit of each: its merge into main.
m_conflict=$(merge_of "$main" "$(on "$main" "edit s5" put s.txt "$(body s 5 's5 mine')")")
x_conflict=$(git_fixture -C "$fixture" rev-parse "$m_conflict^2")
m_new=$(merge_of "$main" "$(on "$main" "add a file" put new.txt "new")")
m_far=$(merge_of "$main" "$(on "$main" "edit s1" put s.txt "$(body s 1 's1 mine')")")
m_apart=$(merge_of "$main" "$(on "$main" "edit t8" put t.txt "$(body t 2 't2 main' 8 't8 mine')")")
m_inside=$(merge_of "$main" "$(on "$main" "edit t5" put t.txt "$(body t 2 't2 main' 5 't5 mine')")")
m_moved=$(merge_of "$main" "$(on "$main" "take #12's t2, edit t5" put t.txt "$(body t 2 't2 stale' 5 't5 mine')")")
m_shared=$(merge_of "$main" "$(on "$main" "edit w5" put w.txt "$(body w 5 'w5 mine')")")
m_rename=$(merge_of "$main" "$(on "$main" "rename t to u" git_fixture mv t.txt u.txt)")
m_file=$(merge_of "$main" "$(on "$main" "add a file d" put d "a file")")
m_unapproved=$(merge_of "$main" "$(on "$main" "edit o5" put o.txt "$(body o 5 'o5 mine')")")
m_queue=$(merge_of "$main" "$(on "$main" "edit q5" put q.txt "$(body q 5 'q5 mine')")")
m_samename=$(merge_of "$main" "$(on "$main" "rename r to g, as #17 does" git_fixture mv r.txt g.txt)")
m_literal=$(merge_of "$main" "$(on "$main" "edit k5" put k.txt "$(body k 1 '<<<<<<< fixture' 3 'k3 main' 5 'k5 mine')")")
m_binary=$(merge_of "$main" "$(on "$main" "rename b to c" git_fixture mv b.bin c.bin)")
m_binedit=$(merge_of "$main" "$(on "$main" "edit b" put_binary b.bin 100 main 200 mine)")
m_binsame=$(merge_of "$main" "$(on "$main" "rename v to z, as #41 does" git_fixture mv v.bin z.bin)")
m_dep=$(merge_of "$main" "$x_one")
m_shortedit=$(merge_of "$main" "$(on "$main" "edit n8" put n.txt "$(body n 2 'n2 main' 8 'n8 mine')")")
m_shortsame=$(merge_of "$main" "$(on "$main" "rename n to p, as #60 does" git_fixture mv n.txt p.txt)")
m_shortinside=$(merge_of "$main" "$(on "$main" "edit n2" put n.txt "$(body n 2 'n2 mine')")")

# Merge groups: squash commits on the group's base. q11 is #11's squash in a group in front, whose
# queue branch the origin holds; g30 and g31 are one group of two pull requests; g_dep lands
# x_one, which #40 and #50 are stacked on, as the queue squashes it.
g20=$(on "$main" "edit s5 (#20)" put s.txt "$(body s 5 's5 mine')")
q11=$(on "$main" "priority edits s5 (#11)" put s.txt "$(body s 5 's5 priority')")
g21=$(on "$q11" "edit s5 after the priority (#21)" put s.txt "$(body s 5 's5 mine after')")
g22=$(on "$main" "a commit that names no pull request" put new.txt "new")
g30=$(on "$main" "add a file (#30)" put thirty.txt "thirty")
g31=$(on "$g30" "edit s5 (#31)" put s.txt "$(body s 5 's5 mine')")
g_dep=$(on "$main" "a change #40 and #50 are stacked on edits x2 (#20)" put x.txt "$(body x 2 'x2 one')")

for sha in "$p11" "$p12" "$p13" "$p14" "$p15" "$p16" "$p17" "$p18" "$p19" "$p40" "$p41" "$p50" "$p60" \
  "$m_conflict" "$m_new" "$m_far" "$m_apart" "$m_inside" "$m_moved" "$m_shared" "$m_rename" \
  "$m_file" "$m_unapproved" "$m_queue" "$m_samename" "$m_literal" "$m_binary" "$m_binedit" \
  "$m_binsame" "$m_dep" "$m_shortedit" "$m_shortsame" "$m_shortinside" "$g20" "$g21" "$g22" \
  "$g31" "$g_dep"; do
  git_fixture -C "$fixture" push -q "$origin" "$sha:refs/fixture/$sha"
done
git_fixture -C "$fixture" push -q "$origin" "$q11:refs/heads/gh-readonly-queue/main/pr-11-$main"

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

# The priority pull requests: #11, #12, #15 to #19, #40, #41, #50 and #60 approved, #13 waiting
# for review, #14 queued.
standard=("$(pull 11 "$p11" '"APPROVED"' false)" "$(pull 12 "$p12" '"APPROVED"' false)"
  "$(pull 13 "$p13" '"REVIEW_REQUIRED"' false)" "$(pull 14 "$p14" null true)"
  "$(pull 15 "$p15" '"APPROVED"' false)" "$(pull 16 "$p16" '"APPROVED"' false)"
  "$(pull 17 "$p17" '"APPROVED"' false)" "$(pull 18 "$p18" '"APPROVED"' false)"
  "$(pull 19 "$p19" '"APPROVED"' false)" "$(pull 40 "$p40" '"APPROVED"' false)"
  "$(pull 41 "$p41" '"APPROVED"' false)" "$(pull 50 "$p50" '"APPROVED"' false)"
  "$(pull 60 "$p60" '"APPROVED"' false)")
answer "${standard[@]}"

# run_check <event name> <sha>: runs the script over $work/event.json with GITHUB_SHA <sha>; sets
# out (stdout), err (stderr) and status, then sets the label read's answer back to the standard
# one and lets gh answer again. Every case runs it under `timeout`, so a fetch that hung would fail
# its case instead of hanging the job. Git runs on its defaults here too, with the file protocol
# allowed to the fetches git merge-tree makes.
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
  answer "${standard[@]}"
  rm -f "$work"/answer-c*.json "$work/gh-refuses"
}

# run_pr <sha>: a pull_request run of pull request #20 at the run commit <sha>.
run_pr() {
  jq -n '{pull_request: {number: 20, base: {ref: "main"}}}' > "$work/event.json"
  run_check pull_request "$1"
}

# run_group <number> <base sha> <sha> [<head_ref>]: a merge_group run of the group of pull request
# <number> on <base sha> at the group's head <sha>.
run_group() {
  jq -n --arg ref "${4:-refs/heads/gh-readonly-queue/main/pr-$1-$2}" --arg base "$2" \
    '{merge_group: {head_ref: $ref, base_sha: $base, base_ref: "refs/heads/main"}}' > "$work/event.json"
  run_check merge_group "$3"
}

gh_calls() { cat "$work/gh-calls" 2> /dev/null || true; }
read_once='graphql PriorityPulls {"owner":"o","repo":"r","after":null}'
fetched() { compgen -G "$work/temp/priority-conflicts.*/repo.git" > /dev/null && echo yes || echo no; }
# clean_line <number> <change>: the regex of the line naming a priority pull request that merges
# cleanly with the change; the seconds it took vary.
clean_line() { echo "^#$1: no conflict with $2 ([0-9]* s)\.\$"; }
remedy_pr="A priority pull request merges first: wait for #%s to merge, then merge main into this pull request's branch and resolve the conflict there."
# conflict_line <priority number> <files>: the error a conflict of pull request #20 writes.
conflict_line() {
  # shellcheck disable=SC2059 # the remedy is the format
  echo "::error::pull request #20 conflicts with priority pull request #$1 (priority work $1 at 100%25) in $2. $(printf "$remedy_pr" "$1")"
}
# group_line <priority number> <files>: the error a conflict of the merge group of #20 writes.
group_line() {
  echo "::error::the merge group of #20 conflicts with priority pull request #$1 (priority work $1 at 100%25) in $2. A priority pull request merges first: wait for #$1 to merge, then merge main into #20's branch, resolve the conflict and queue #20 again."
}

echo "case: a dispatched run lands nothing, so it reads nothing"
echo '{}' > "$work/event.json"
run_check workflow_dispatch "$main"
check "it passes" "$(is "$status" 0)"
check "it says why" "$(has_line "$out" "A workflow_dispatch run lands nothing on a branch, so it cannot land ahead of a priority pull request: nothing to check.")"
check "it reads no route" "$(is "$(gh_calls)" "")"

echo "case: a pull request that edits the lines an approved priority pull request edits fails"
run_pr "$m_conflict"
check "it fails" "$(is "$status" 1)"
check "it names the priority pull request, its title escaped, the file and the remedy" \
  "$(has_line "$err" "$(conflict_line 11 s.txt)")"
check "it names no other" "$(is "$(grep -c '^::error::' <<<"$err")" 1)"
check "it reads the priority pull requests once" "$(is "$(gh_calls)" "$read_once")"
check "it says the stale one already conflicts with the base" \
  "$(has_line "$out" "#12 already conflicts with the base of pull request #20 in 1 file(s): those are its own to resolve, and count against pull request #20 only where pull request #20 adds a conflict.")"
check "and that it is no conflict of the pull request's" "$(contains "$out" "$(clean_line 12 "pull request #20")")"
check "it does not check the one that is neither approved nor queued" "$(is "$(contains "$out$err" "#13")" false)"
check "it writes no title on stdout" "$(is "$(contains "$out" "priority work")" false)"

echo "case: a pull request that edits no file a priority pull request edits passes"
run_pr "$m_new"
check "it passes" "$(is "$status" 0)"
check "it merged each" "$(contains "$out" "$(clean_line 11 "pull request #20")")"
check "it counts the twelve it checked" \
  "$(has_line "$out" "pull request #20 lands no conflict on an approved or queued priority pull request: 12 checked.")"
check "it writes no error" "$(is "$err" "")"

echo "case: a pull request that edits a priority pull request's file away from its lines passes"
run_pr "$m_far"
check "it passes" "$(is "$status" 0)"
check "it says so" "$(contains "$out" "$(clean_line 11 "pull request #20")")"

echo "case: an edit apart from a stale priority pull request's own conflict with the base passes"
# #12 conflicts with main at t2 and lands t5; the pull request edits t8 of that file, so its plain
# merge with #12 conflicts at t2, which is #12's own conflict with main and not the pull request's.
run_pr "$m_apart"
check "it passes" "$(is "$status" 0)"
check "it says whose conflict it is" "$(contains "$out" "^#12 already conflicts with the base")"
check "and that the pull request adds none" "$(contains "$out" "$(clean_line 12 "pull request #20")")"

echo "case: a new conflict in a file a stale priority pull request already conflicts on fails"
# The pull request edits t5, which #12 edits too, in the file where #12 already conflicts at t2.
run_pr "$m_inside"
check "it fails" "$(is "$status" 1)"
check "it names #12 and the file" "$(has_line "$err" "$(conflict_line 12 t.txt)")"

echo "case: a conflict moved to another place of a file a stale priority pull request conflicts on fails"
# The pull request takes #12's t2, which ends #12's conflict with main there, and edits t5, which
# #12 edits too: one conflict in t.txt in either merge, at another place.
run_pr "$m_moved"
check "it fails" "$(is "$status" 1)"
check "it names #12 and the file" "$(has_line "$err" "$(conflict_line 12 t.txt)")"

echo "case: an edit of text the base and a stale priority pull request each added alike fails"
# main and #16 both set w5 to the same text, which merges cleanly, while #16 conflicts with main
# elsewhere; the pull request edits w5, which #16 would then have to resolve.
run_pr "$m_shared"
check "it fails" "$(is "$status" 1)"
check "it names #16 and the file" "$(has_line "$err" "$(conflict_line 16 w.txt)")"
check "it names no other" "$(is "$(grep -c '^::error::' <<<"$err")" 1)"

echo "case: a rename of the file a stale priority pull request conflicts on is not a conflict"
run_pr "$m_rename"
check "it passes" "$(is "$status" 0)"
check "it merged #12 through the rename" "$(contains "$out" "$(clean_line 12 "pull request #20")")"

echo "case: a rename to where a stale priority pull request renamed the same file is not a conflict"
# #17 renamed r.txt to g.txt and conflicts with main at r2; the pull request renames it to g.txt too.
run_pr "$m_samename"
check "it passes" "$(is "$status" 0)"
check "it merged #17 through both renames" "$(contains "$out" "$(clean_line 17 "pull request #20")")"

echo "case: an edit of a small file a stale priority pull request renamed and conflicts in passes"
# #60 renamed the short n.txt to p.txt and conflicts with main at n2; the pull request edits n8.
# Step 2's p.txt holds conflict markers and is under git's 50% likeness to n.txt, so only the
# looser second try of step 3 finds the rename.
run_pr "$m_shortedit"
check "it passes" "$(is "$status" 0)"
check "it merged #60 through the rename" "$(contains "$out" "$(clean_line 60 "pull request #20")")"

echo "case: a rename of a small file to where a stale priority pull request renamed it is not a conflict"
run_pr "$m_shortsame"
check "it passes" "$(is "$status" 0)"
check "it merged #60 through both renames" "$(contains "$out" "$(clean_line 60 "pull request #20")")"

echo "case: an edit of a small renamed file inside a stale priority pull request's conflict fails"
# The pull request edits n2, the line #60 already conflicts with main on: the looser try finds the
# rename and git still cannot apply the edit.
run_pr "$m_shortinside"
check "it fails" "$(is "$status" 1)"
check "it names #60" "$(contains "$err" "^::error::pull request #20 conflicts with priority pull request #60 (priority work 60 at 100%25) in ")"
check "it names no other" "$(is "$(grep -c '^::error::' <<<"$err")" 1)"

echo "case: an edit of a file a stale priority pull request deletes fails, though the file reads like it holds a marker"
# #18 deletes k.txt, whose first line reads like a conflict marker, and main edits k3: an inherited
# modify/delete conflict. The pull request edits k5, which #18 would then have to resolve too.
run_pr "$m_literal"
check "it fails" "$(is "$status" 1)"
check "it names #18 and the file" "$(has_line "$err" "$(conflict_line 18 k.txt)")"

echo "case: an edit of a binary file a stale priority pull request conflicts on fails"
# #19 and main each edit b.bin, a binary conflict; the pull request edits b.bin again. git names a
# binary conflict as a contents one too, and writes no markers in it: only its stages tell the
# pull request's edit apart.
run_pr "$m_binedit"
check "it fails" "$(is "$status" 1)"
check "it names #19 and the file" "$(has_line "$err" "$(conflict_line 19 b.bin)")"

echo "case: a rename of a binary file a stale priority pull request conflicts on is not a conflict"
# #19 and main each edit b.bin, a binary conflict; the pull request only renames it to c.bin.
run_pr "$m_binary"
check "it passes" "$(is "$status" 0)"
check "it merged #19 through the rename" "$(contains "$out" "$(clean_line 19 "pull request #20")")"

echo "case: a rename of a binary file to where a stale priority pull request renamed it is not a conflict"
# #41 renames v.bin to z.bin and edits it, a binary conflict with main; the pull request renames it
# to z.bin too.
run_pr "$m_binsame"
check "it passes" "$(is "$status" 0)"
check "it merged #41 through both renames" "$(contains "$out" "$(clean_line 41 "pull request #20")")"

echo "case: a pull request priority pull requests are stacked on gets its merge group's verdict"
# #40 and #50 are stacked on x_one and edit x2 again; the pull request lands x_one, which the queue
# squashes, so each would then have to resolve x2. The test merge keeps x_one's own commit, which
# both share, so a merge of it with #50, which is not stale, is clean: the check judges the squash.
run_pr "$m_dep"
check "it fails" "$(is "$status" 1)"
check "it names #40, which is stale, and the file" "$(has_line "$err" "$(conflict_line 40 x.txt)")"
check "it names #50, which is not, and the file" "$(has_line "$err" "$(conflict_line 50 x.txt)")"
check "it names no other" "$(is "$(grep -c '^::error::' <<<"$err")" 2)"

echo "case: the merge group of a pull request priority pull requests are stacked on fails alike"
run_group 20 "$main" "$g_dep"
check "it fails" "$(is "$status" 1)"
check "it names #40 and the file" "$(has_line "$err" "$(group_line 40 x.txt)")"
check "and #50" "$(has_line "$err" "$(group_line 50 x.txt)")"

echo "case: a file where a priority pull request adds a directory fails"
run_pr "$m_file"
check "it fails" "$(is "$status" 1)"
check "it names #15 and the path, without the name git moves the file aside to" \
  "$(has_line "$err" "$(conflict_line 15 d)")"

echo "case: a priority pull request that is neither approved nor queued is not checked against"
run_pr "$m_unapproved"
check "it passes, though the two edit one line" "$(is "$status" 0)"
check "it names it nowhere" "$(is "$(contains "$out$err" "#13")" false)"

echo "case: a priority pull request in the merge queue counts as an approved one"
run_pr "$m_queue"
check "it fails" "$(is "$status" 1)"
check "it names the queued one" "$(has_line "$err" "$(conflict_line 14 q.txt)")"

echo "case: a pull request labelled priority passes, and fetches nothing"
answer "${standard[@]}" "$(pull 20 "$x_conflict" null false)"
run_pr "$m_conflict"
check "it passes" "$(is "$status" 0)"
check "it says why" "$(has_line "$out" "#20 carries the label priority, and priority pull requests are not held to each other: nothing to check.")"
check "it fetches nothing" "$(is "$(fetched)" no)"

echo "case: a label read GitHub refuses fails the check rather than pass it"
touch "$work/gh-refuses"
run_pr "$m_new"
check "it fails" "$(is "$status" 1)"
check "it says it could not read" \
  "$(has_line "$err" "::error::GitHub's GraphQL API refused the read of the open pull requests labelled priority at every attempt, so this check cannot tell whether pull request #20 conflicts with a priority pull request, and fails rather than pass unread")"
check "it tried three times" "$(is "$(gh_calls | wc -l)" 3)"
check "it fetches nothing" "$(is "$(fetched)" no)"

echo "case: a priority pull request on the label read's second page is checked against"
page answer-first.json true '"c1"' "$(pull 11 "$p11" '"APPROVED"' false)"
page answer-c1.json false null "$(pull 14 "$p14" null true)"
run_pr "$m_queue"
check "it fails" "$(is "$status" 1)"
check "it names the one on the second page" "$(has_line "$err" "$(conflict_line 14 q.txt)")"
check "it read both pages" \
  "$(is "$(gh_calls)" "$read_once"$'\n''graphql PriorityPulls {"owner":"o","repo":"r","after":"c1"}')"

echo "case: a page that says another follows and names no cursor fails the check"
page answer-first.json true null "$(pull 11 "$p11" '"APPROVED"' false)"
run_pr "$m_new"
check "it fails" "$(is "$status" 1)"
check "it says so" "$(contains "$err" "^::error::GitHub's GraphQL API said another page of the open pull requests labelled priority follows, and named no new cursor for it, ")"

echo "case: an answer of another shape fails the check"
echo '{"data": null, "errors": [{"message": "Something went wrong"}]}' > "$work/answer-first.json"
run_pr "$m_new"
check "it fails" "$(is "$status" 1)"
check "it says it could not read the answer" \
  "$(contains "$err" "^::error::GitHub's GraphQL API answered the read of the open pull requests labelled priority in a shape this check does not read, ")"

echo "case: a priority head git cannot fetch fails the check rather than pass it"
answer "$(pull 11 1111111111111111111111111111111111111111 '"APPROVED"' false)"
run_pr "$m_new"
check "it fails" "$(is "$status" 1)"
check "it says it could not fetch" "$(contains "$err" "^::error::git could not fetch the commit of pull request #20 and the heads of the priority pull requests it is checked against from file://")"

echo "case: a pull request run whose commit is no merge fails"
run_pr "$x_conflict"
check "it fails" "$(is "$status" 1)"
check "it says why" "$(contains "$err" "^::error::GITHUB_SHA $x_conflict is no merge commit")"

echo "case: a merge group that edits an approved priority pull request's lines fails"
run_group 20 "$main" "$g20"
check "it fails" "$(is "$status" 1)"
check "it names the group's pull request, the priority one, the file and the remedy" \
  "$(has_line "$err" "$(group_line 11 s.txt)")"

echo "case: a merge group whose one pull request is labelled priority passes"
answer "${standard[@]}" "$(pull 20 "$x_conflict" '"APPROVED"' true)"
run_group 20 "$main" "$g20"
check "it passes" "$(is "$status" 0)"
check "it says why" "$(has_line "$out" "Every pull request of the merge group of #20 (#20) carries the label priority, and priority pull requests are not held to each other: nothing to check.")"

echo "case: a queued priority pull request standing in a group in front lands first"
# #11's group is in front: its queue branch's tip, q11, is this group's base, and this group edits
# s5 again, which #11's head edits from main.
answer "$(pull 11 "$p11" '"APPROVED"' true)" "${standard[@]:1}"
run_group 21 "$q11" "$g21"
check "it passes" "$(is "$status" 0)"
check "it says why" "$(has_line "$out" "#11 stands in a group in front of the merge group of #21, so it lands first.")"

echo "case: an approved priority pull request that is not queued is held against, whatever queue branch names it"
# The same group, with #11 approved and out of the queue: the leftover branch decides nothing.
run_group 21 "$q11" "$g21"
check "it fails" "$(is "$status" 1)"
check "it names #11" "$(contains "$err" "^::error::the merge group of #21 conflicts with priority pull request #11 (priority work 11 at 100%25) in s.txt. ")"

echo "case: a group of two pull requests, one of them labelled priority, is checked"
answer "${standard[@]}" "$(pull 30 "$g30" null false)"
run_group 31 "$main" "$g31"
check "it fails" "$(is "$status" 1)"
check "it names the group by the pull request its branch names" \
  "$(contains "$err" "^::error::the merge group of #31 conflicts with priority pull request #11 (priority work 11 at 100%25) in s.txt. ")"

echo "case: a group of two pull requests, both labelled priority, passes"
answer "${standard[@]}" "$(pull 30 "$g30" null false)" "$(pull 31 "$g31" null false)"
run_group 31 "$main" "$g31"
check "it passes" "$(is "$status" 0)"
check "it names both" "$(has_line "$out" "Every pull request of the merge group of #31 (#31 #30) carries the label priority, and priority pull requests are not held to each other: nothing to check.")"

echo "case: a merge group commit that names no pull request fails"
run_group 22 "$main" "$g22"
check "it fails" "$(is "$status" 1)"
check "it names the commit" "$(contains "$err" "^::error::commit $g22 of the merge group of #22, 'a commit that names no pull request', names no pull request")"

echo "case: a merge group whose commits do not hold the pull request its branch names fails"
run_group 23 "$main" "$g20"
check "it fails" "$(is "$status" 1)"
check "it names both" "$(contains "$err" "^::error::the commits of the merge group of #23 name #20 and not #23, ")"

echo "case: a merge group branch that names no pull request fails before any read"
run_group 20 "$main" "$g20" refs/heads/main
check "it fails" "$(is "$status" 1)"
check "it names the ref" "$(contains "$err" "^::error::merge_group.head_ref 'refs/heads/main' names no queued pull request ")"
check "it reads no route" "$(is "$(gh_calls)" "")"

summary "the priority pull request conflict check"
