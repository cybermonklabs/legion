#!/usr/bin/env bash
# Tests for the issue-key rule: `check-private-names.sh`'s rule 6 (an issue key of the
# deployment's own projects, named by the `key:` lines of PRIVATE_NAME_TERMS), which
# `check-pr-text.sh` runs over a pull request's new text from a cutoff it derives at each
# run. The cases: the rule inactive until a release carries its marker commit; the cutoff, from
# the release history; a pull request and a commit before the cutoff not read; the title, body,
# comments, reviews, commits and the paths and lines they add read after it; a Dispatch link,
# which is no private name, passing where a key beside it or inside it is refused; a diff GitHub's
# REST answer does not hold whole read from git, and one neither holds whole refused as UNKNOWN; a
# key the same diff also removes carried over; a merge's own diff passed over for the pull
# request's net diff; a key beside a "_"; a list with terms and no `key:` line refused; a keys
# run under no list at all saying it checked nothing; and the count line of a `names` run,
# which reads the `key:` lines but checks none, saying so.
#
# The terms and the project key are placeholders this file and check-private-names-test-lib.sh
# set, never a real one, and every case is named by a neutral label, since this script's own
# output is the CI log; the last case runs the guard over that captured log.
#
# Run from anywhere: .github/scripts/check-private-names-keys.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit
# No tracing: a traced run would print every term the cases below expand.
set +x

# shellcheck source=.github/scripts/check-private-names-test-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-private-names-test-lib.sh"

# THE ISSUE-KEY RULE (check-private-names.sh's rule 6, from check-pr-text.sh's cutoff).
# QUILL is a placeholder project key, given as a `key:` line beside the placeholder terms; its
# issue keys are spelled from pieces, so this file holds none whole, and no case label prints one.
key_project=QUILL
planted_key="$key_project-""123"
key_terms=$(printf '%s\n' "$planted_terms" "key:$key_project" key:LEGION)
link='dispatch://'
cutoff_at=2026-11-20T00:00:00Z
# git_fixture <dir> <git args...>: git in a fixture repository, with no user or system
# configuration of this machine (a signing setting, say) in the way.
git_fixture() {
  local dir=$1
  shift
  GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git -C "$dir" -c user.name=Dev \
    -c user.email=dev@example.com "$@"
}
# history_remote: the repository's history as the rule reads it with git, built once: a bare
# repository at $work/remote/example/repo whose main holds a commit, the commit that adds the
# cutoff marker and three later commits, with lightweight tags legion-v8.9.9 on the first,
# legion-v9.0.0, legion-v9.1.0 and legion-v10.0.0 on the later three, and another package's
# pi-envoy-v20.0.0 beside legion-v9.0.0. The marker commit's id is kept beside the fixture, in
# $work/history-marker, as it was made; sets marker_sha to it.
history_remote() {
  local src="$work/history-src" bare="$work/remote/example/repo"
  if [ ! -d "$bare" ]; then
    mkdir -p "$src/.github"
    git_fixture "$src" init -q -b main
    git_fixture "$src" commit -q --allow-empty -m "before the marker"
    git_fixture "$src" tag legion-v8.9.9
    : > "$src/.github/issue-key-cutoff"
    git_fixture "$src" add .github/issue-key-cutoff
    git_fixture "$src" commit -q -m "add the marker"
    git_fixture "$src" rev-parse HEAD > "$work/history-marker"
    for tags in "legion-v9.0.0 pi-envoy-v20.0.0" legion-v9.1.0 legion-v10.0.0; do
      git_fixture "$src" commit -q --allow-empty -m "after the marker"
      for tag in $tags; do git_fixture "$src" tag "$tag"; done
    done
    mkdir -p "$work/remote/example"
    GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git clone -q --bare "$src" "$bare"
    git_fixture "$bare" config uploadpack.allowFilter true
  fi
  marker_sha=$(< "$work/history-marker")
}
# release <tag> <published at>: the release route's answer for that tag.
release() { jq -nc --arg tag "$1" --arg at "$2" '{tag_name: $tag, published_at: $at, draft: false}' > "$work/release.$1"; }
# marker_on_main: the commits route names the marker commit, dated before every release.
marker_on_main() {
  history_remote
  jq -nc --arg sha "$marker_sha" '[{sha: $sha, commit: {committer: {date: "2026-11-18T00:00:00Z"}}}]' \
    > "$work/cutoff-commits"
}
# key_rule_on: the fixtures of an active rule: the marker commit on main, and legion-v9.0.0, the
# lowest version that contains it, published at cutoff_at; legion-v8.9.9, which does not, and
# legion-v9.1.0, which does, published later.
key_rule_on() {
  marker_on_main
  release legion-v8.9.9 2026-11-19T00:00:00Z
  release legion-v9.0.0 "$cutoff_at"
  release legion-v9.1.0 2026-11-20T06:00:00Z
}
# release_calls: the tags whose release the run asked for, in order, one a line.
release_calls() { sed -n 's#^api repos/example/repo/releases/tags/##p' "$work/calls"; }
# opened_at <time>: the pull request was opened then.
opened_at() { jq --arg at "$1" '.created_at = $at' "$work/pull" > "$work/pull.next" && mv "$work/pull.next" "$work/pull"; }
# commits <sha> <date> <message> [...]: the compare route's one page, those commits in order.
commits() {
  local list='[]'
  while [ $# -gt 0 ]; do
    list=$(jq -c --arg sha "$1" --arg date "$2" --arg msg "$3" '. + [{sha: $sha, commit: {message: $msg,
      author: {name: "Dev", email: "dev@example.com"},
      committer: {name: "Dev", email: "dev@example.com", date: $date}}}]' <<< "$list")
    shift 3
  done
  jq -c '{total_commits: length, commits: .}' <<< "$list" > "$work/compare"
}
# commit_file <sha> <status> <path> <patch> [total]: the commit route's answer, that one file,
# its stats.total the file's changes unless a total is given; a patch of "-" is none at all.
commit_file() {
  local additions
  additions=$(grep -c '^+' <<< "$4" || true)
  jq -nc --arg status "$2" --arg path "$3" --arg patch "$4" --argjson additions "$additions" \
    --argjson total "${5:-$additions}" \
    '{stats: {total: $total}, files: [{filename: $path, status: $status, additions: $additions,
      changes: $additions, patch: (if $patch == "-" then null else $patch end)}]}' > "$work/commit.$1"
}
# as_merge <sha>: marks that commit of the compare page a merge, with two parents.
as_merge() {
  jq -c --arg sha "$1" '.commits |= map(if .sha == $sha then .parents = [{sha: "p1"}, {sha: "p2"}] else . end)' \
    "$work/compare" > "$work/compare.next"
  mv "$work/compare.next" "$work/compare"
}
# pr_files <status> <path> <patch> [...]: the pull request's files route, one page holding those
# files (a patch of "-" is none at all), and the pull request's totals to match them.
pr_files() {
  local list='[]' additions
  while [ $# -gt 0 ]; do
    additions=$(grep -c '^+' <<< "$3" || true)
    list=$(jq -c --arg status "$1" --arg path "$2" --arg patch "$3" --argjson additions "$additions" \
      '. + [{filename: $path, status: $status, additions: $additions, changes: $additions,
             patch: (if $patch == "-" then null else $patch end)}]' <<< "$list")
    shift 3
  done
  printf '%s\n' "$list" > "$work/pr-files"
  jq --argjson files "$(jq length <<< "$list")" --argjson lines "$(jq '[.[].changes] | add // 0' <<< "$list")" \
    '.changed_files = $files | .additions = $lines | .deletions = 0' "$work/pull" > "$work/pull.next"
  mv "$work/pull.next" "$work/pull"
}
key_run() { PRIVATE_NAME_TERMS=$key_terms run_pr_text; }
key_refused="names an issue key of the deployment's own projects"
# The commit ids the fixtures use. check-pr-text.sh prints a commit by the first 12 characters of
# its id, and holds the id GitHub answers to the 40 hex digits GitHub writes: a fixture id of any
# other shape is refused as a read that failed, so each one here is 40 hex digits and each
# assertion names its first 12.
new_sha=1111111111111111111111111111111111111111
old_sha=2222222222222222222222222222222222222222
west_sha=3333333333333333333333333333333333333333
east_sha=4444444444444444444444444444444444444444
merge_sha=5555555555555555555555555555555555555555
first_sha=6666666666666666666666666666666666666666
second_sha=7777777777777777777777777777777777777777
new_short=${new_sha:0:12} merge_short=${merge_sha:0:12} west_short=${west_sha:0:12}
second_short=${second_sha:0:12}

echo "case: the issue-key rule is inactive until a release carries its marker commit"
write_pr_text "fix(dispatch): keep the $planted_key order" "Refs LEGION-7" "fix: keep the order"
key_run
check "with no marker commit on main, a planted key in the title passes" "$(is "$status" 0)"
check "says the rule is inactive and why" \
  "$(contains "$out" '^check-pr-text: the issue-key rule is inactive: no commit on main adds')"
write_pr_text "fix(dispatch): keep the $planted_key order" "Refs LEGION-7" "fix: keep the order"
marker_on_main
release legion-v8.9.9 2026-11-19T00:00:00Z
key_run
check "with a marker commit and no release of a version that contains it, the key still passes" "$(is "$status" 0)"
check "says no release contains it" "$(contains "$out" 'no legion-v\* release contains the commit')"
check "asked for the three versions that contain it, a tag with no release yet being skipped" \
  "$(is "$(release_calls | paste -sd' ')" 'legion-v9.0.0 legion-v9.1.0 legion-v10.0.0')"

echo "case: the cutoff is the lowest legion-v version that contains the marker and has a release"
# Versions in version order, not text order (legion-v10.0.0 comes last), from git's answer to
# which tags contain the marker: legion-v8.9.9 does not, and another package's tag is never read.
# legion-v9.0.0's tag has no release yet, so legion-v9.1.0's is the first, and legion-v10.0.0's is
# read to check that it was published no earlier.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
marker_on_main
release legion-v8.9.9 2026-11-19T00:00:00Z
release legion-v9.1.0 2026-11-20T10:30:00Z
release legion-v10.0.0 2026-11-20T12:00:00Z
key_run
check "active since legion-v9.1.0, the first version with a release" \
  "$(contains "$out" '^check-pr-text: the issue-key rule is active since legion-v9.1.0, published 2026-11-20T10:30:00Z$')"
check "asks for the releases in version order, and stops after the one past the first" \
  "$(is "$(release_calls | paste -sd' ')" 'legion-v9.0.0 legion-v9.1.0 legion-v10.0.0')"

echo "case: the API calls stay the same however many releases there are"
# Every pr-title run derives the cutoff, so its cost must not grow with each release. Sixty more
# legion-v versions, each with a release, leave the run's calls as they were.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
key_run
few_calls=$(wc -l < "$work/calls")
check "two release calls with three versions after the marker" \
  "$(is "$(release_calls | paste -sd' ')" 'legion-v9.0.0 legion-v9.1.0')"
for n in $(seq 0 59); do
  git_fixture "$work/remote/example/repo" tag "legion-v11.$n.0" main
  release "legion-v11.$n.0" "2026-11-2$((1 + n / 10))T00:00:00Z"
done
key_run
check "the same cutoff with sixty more versions" \
  "$(contains "$out" "^check-pr-text: the issue-key rule is active since legion-v9.0.0, published $cutoff_at\$")"
check "the same two release calls" "$(is "$(release_calls | paste -sd' ')" 'legion-v9.0.0 legion-v9.1.0')"
check "and as many calls in all" "$(is "$(wc -l < "$work/calls")" "$few_calls")"
for n in $(seq 0 59); do git_fixture "$work/remote/example/repo" tag -d "legion-v11.$n.0" > /dev/null; done

echo "case: a later version published before a lower one is refused, not trusted"
# The lowest version is the earliest release only while versions rise in publish order, which the
# release workflow keeps; the next version's release is the check that it did.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
marker_on_main
release legion-v9.0.0 2026-11-20T00:00:00Z
release legion-v9.1.0 2026-11-19T12:00:00Z
key_run
check "exits 2" "$(is "$status" 2)"
check "says the versions are not in publish order" \
  "$(contains "$out" '^::error::UNKNOWN: legion-v9.1.0 was published at 2026-11-19T12:00:00Z, before legion-v9.0.0 .* not in publish order')"

echo "case: a release or a history GitHub does not answer is UNKNOWN, never a pass"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
marker_on_main
echo 'HTTP 500' > "$work/release.legion-v9.0.0"
key_run
check "a server error on a release exits 2" "$(is "$status" 2)"
check "names the release it could not read, and the file that keeps gh's words" \
  "$(contains "$out" '^::error::UNKNOWN: GitHub did not answer for the release of legion-v9\.0\.0 (gh (HTTP 500); its own words are in ')"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
mv "$work/remote" "$work/remote.held"
key_run
mv "$work/remote.held" "$work/remote"
check "a history git cannot read exits 2" "$(is "$status" 2)"
check "says git could not read it" "$(contains "$out" "^::error::UNKNOWN: git could not read main's history (git; its own words are in ")"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
git_fixture "$work/remote/example/repo" for-each-ref --format='create %(refname) %(objectname)' 'refs/tags/legion-v*' \
  > "$work/legion-tags.held"
awk '{ print "delete " $2 }' "$work/legion-tags.held" | git_fixture "$work/remote/example/repo" update-ref --stdin
key_run
git_fixture "$work/remote/example/repo" update-ref --stdin < "$work/legion-tags.held"
check "a remote that lists no legion-v tag exits 2, not inactive" "$(is "$status" 2)"
check "says the remote lists none" "$(contains "$out" '^::error::UNKNOWN: the remote lists no legion-v tag')"
check "and does not call the rule inactive" "$(is "$(contains "$out" 'issue-key rule is inactive')" false)"

echo "case: once a release carries the marker commit, a pull request opened before it is not read"
write_pr_text "fix(dispatch): keep the $planted_key order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-19T12:00:00Z
key_run
check "a key in the title of a pull request opened before the cutoff passes" "$(is "$status" 0)"
check "names the release the rule is active since" \
  "$(contains "$out" "^check-pr-text: the issue-key rule is active since legion-v9.0.0, published $cutoff_at")"
check "says the pull request is not read" "$(contains "$out" 'before the issue-key rule.s cutoff')"

echo "case: a pull request opened after the cutoff is read in its title and body, not its branch"
opened_after() { key_rule_on; opened_at 2026-11-21T00:00:00Z; commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order"; commit_file "$new_sha" modified docs/notes.md $'@@ -1,1 +1,1 @@\n+clean text'; }
write_pr_text "fix(dispatch): keep the $planted_key order" "Refs LEGION-7" "fix: keep the order"
opened_after
key_run
check "a key in the title fails" "$(is "$status" 1)"
check "names the title" "$(contains "$out" "title:1: $key_refused")"
check "prints no key" "$(is "$(contains "$out" "$planted_key")" false)"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-123, with the word $key_project alone." \
  "fix: keep the order"
opened_after
key_run
check "Legion's own key and the bare project word pass" "$(is "$status" 0)"
# A key is the upper-case form Dispatch prints; a lower-case string of the same shape, such as a
# job name, is not one. The same text with the key in upper case is refused.
lower_key="${key_project,,}-123-job"
write_pr_text "fix(dispatch): keep the inbox order" "Runs as the $lower_key job." "fix: keep the order"
opened_after
key_run
check "a lower-case job name of a key's shape passes" "$(is "$status" 0)"
write_pr_text "fix(dispatch): keep the inbox order" "Runs as the $planted_key job." "fix: keep the order"
opened_after
key_run
check "the same text with the key in upper case is refused" "$(is "$status" 1)"
write_pr_text "fix(dispatch): keep the inbox order" "$(printf 'Line one.\nTracked as %s.\n' "$planted_key")" \
  "fix: keep the order"
opened_after
key_run
check "a key in the body fails" "$(is "$status" 1)"
check "names the body's line" "$(contains "$out" "body:2: $key_refused")"
# The daemon names an issue's branch after its key today; the change that names branches another
# way adds the branch name to this rule.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order" \
  "fix/$planted_key-keep-order"
opened_after
key_run
check "a key in the branch name is not read by this rule" "$(is "$status" 0)"

echo "case: a pull request opened after the cutoff is read in its comments and reviews"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
opened_after
items 501 "Fine." 502 "Tracked as $planted_key." > "$work/comments.1"
items 601 "$(printf 'See %sx/spec.\nTracked as %s.\n' "$link" "$planted_key")" > "$work/reviews.1"
key_run
check "a key in a comment and one in a review fail" "$(is "$status" 1)"
check "names the comment" "$(contains "$out" "comment:502:1: $key_refused")"
check "names the review's key line" "$(contains "$out" "review:601:2: $key_refused")"
check "and not the review's link line" "$(is "$(contains "$out" 'review:601:1:')" false)"

echo "case: a Dispatch link in new text passes, and a key beside it or inside it is still refused"
write_pr_text "fix(dispatch): keep the inbox order" \
  "$(printf 'See %sLEGION-7/ask/x for the decision.\nTracked as %s.\n' "$link" "$planted_key")" \
  "fix: keep the order"
opened_after
commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order, as ${link}LEGION-7 says"
commit_file "$new_sha" modified docs/notes.md $'@@ -1,1 +1,1 @@\n+See '"$link"'example/1.'
items 701 "Answered at ${link}example/ask/1." > "$work/comments.1"
key_run
check "a body with a link and a key fails, on the key" "$(is "$status" 1)"
check "names the key's line" "$(contains "$out" "body:2: $key_refused")"
check "and not the link's line" "$(is "$(contains "$out" 'body:1:')" false)"
check "and not the commit's message, which holds only a link" "$(is "$(contains "$out" "commit:$new_short:1:")" false)"
check "and not the line it adds, which holds only a link" "$(is "$(contains "$out" 'docs/notes.md:1:')" false)"
check "and not the comment, which holds only a link" "$(is "$(contains "$out" 'comment:701')" false)"
check "and the run over the text, the first, reports one finding, the key" \
  "$(is "$(grep -m 1 -o '^check-private-names: [0-9]* finding(s)' <<< "$out" || true)" 'check-private-names: 1 finding(s)')"
check "and none over the added line, which holds only a link" \
  "$(contains "$out" "^check-private-names: no text names an issue key of the deployment's own projects")"
write_pr_text "fix(dispatch): keep the inbox order" "See ${link}LEGION-7/ask/x." "fix: keep the order"
opened_after
key_run
check "the same link with no key beside it passes" "$(is "$status" 0)"
write_pr_text "fix(dispatch): keep the inbox order" "See ${link}$planted_key/ask/x." "fix: keep the order"
opened_after
key_run
check "a link that holds a key fails" "$(is "$status" 1)"
check "names its line as a key" "$(contains "$out" "body:1: $key_refused")"
check "prints no key" "$(is "$(contains "$out" "$planted_key")" false)"

echo "case: a commit is read after the cutoff and not before it"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$old_sha" 2026-11-19T00:00:00Z "fix: as $planted_key said" "$new_sha" 2026-11-21T01:00:00Z "fix: as $planted_key said"
commit_file "$old_sha" modified docs/notes.md $'@@ -0,0 +1,2 @@\n+line one\n+old '"$planted_key"' line'
commit_file "$new_sha" modified docs/notes.md $'@@ -3,0 +4,2 @@\n+clean\n+new '"$planted_key"' line'
key_run
check "fails on the new commit" "$(is "$status" 1)"
check "names the new commit's message" "$(contains "$out" "commit:$new_short:1: $key_refused")"
check "names the line the new commit adds, at its line in the file" \
  "$(contains "$out" "commit:$new_short:docs/notes.md:5: $key_refused")"
check "reads nothing of the commit dated before the cutoff" "$(is "$(contains "$out" "${old_sha:0:12}")" false)"
check "and never asks GitHub for its files" "$(is "$(contains "$(< "$work/calls")" "commits/$old_sha")" false)"

echo "case: a commit's date is compared in UTC, whatever offset its committer's clock wrote"
# GitHub's GraphQL committer { date } keeps the committer's own offset, where the cutoff is in UTC;
# the script reads committedDate, which is in UTC. The first commit was made 4 hours after the
# cutoff on a clock 8 hours behind UTC, so its date as written sorts before the cutoff's; the
# second 2 hours before the cutoff on a clock 8 hours ahead, so its date as written sorts after.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$west_sha" 2026-11-19T20:00:00-08:00 "fix: as $planted_key said" \
  "$east_sha" 2026-11-20T06:00:00+08:00 "fix: as $planted_key said"
commit_file "$west_sha" modified docs/notes.md $'@@ -3,0 +4,1 @@\n+new '"$planted_key"' line'
key_run
check "fails on the commit made after the cutoff on a clock behind UTC" "$(is "$status" 1)"
check "names its message" "$(contains "$out" "commit:$west_short:1: $key_refused")"
check "names the line it adds" "$(contains "$out" "commit:$west_short:docs/notes.md:4: $key_refused")"
check "reads nothing of the commit made before the cutoff on a clock ahead of UTC" \
  "$(is "$(contains "$out" "${east_sha:0:12}")" false)"
check "and never asks GitHub for its files" "$(is "$(contains "$(< "$work/calls")" "commits/$east_sha")" false)"

echo "case: the marker commit is found past the first page of its history"
# The marker commit is the oldest that touches the marker, the last listed; here 100 later commits
# touch it too, so it is on the second page.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
jq -nc --arg sha "$marker_sha" '[range(100) | "\(.)" | {sha: ("c" * (40 - length) + .)}] + [{sha: $sha}]
  | map(. + {commit: {committer: {date: "2026-11-18T00:00:00Z"}}})' > "$work/cutoff-commits"
key_run
check "the rule is active since the release that holds it" \
  "$(contains "$out" "^check-pr-text: the issue-key rule is active since legion-v9.0.0, published $cutoff_at\$")"
check "reads the history's second page by its cursor" \
  "$(contains "$(< "$work/calls")" '^graphql PrText .*"history":true.*"historyAfter":"1"')"
echo 1 > "$work/cut.history"
key_run
check "a history GitHub lists short of its total exits 1" "$(is "$status" 1)"
check "names the list and both counts" \
  "$(contains "$out" "^::error::GitHub's GraphQL API reports 101 commits on main that touch .github/issue-key-cutoff but listed 100")"

echo "case: a path a new commit adds is new text, and a path it only changes is not"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order"
commit_file "$new_sha" added "docs/notes-$planted_key.md" $'@@ -0,0 +1,1 @@\n+clean text'
key_run
check "an added file whose path holds a key fails" "$(is "$status" 1)"
check "names the path, masked" \
  "$(contains "$out" "^::error::commit:$new_short:docs/<name>: $key_refused .* in its path\$")"
check "prints no key" "$(is "$(contains "$out" "$planted_key")" false)"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order"
commit_file "$new_sha" modified "docs/notes-$planted_key.md" $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "a clean line added to a file whose old path holds a key passes" "$(is "$status" 0)"

echo "case: a commit neither GitHub nor git returns whole is UNKNOWN, never passed"
# The commit ids here are no commit git can fetch, so git cannot read the commit either.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order"
commit_file "$new_sha" modified docs/big.md -
jq -c '.files[0].additions = 5 | .files[0].changes = 5 | .stats.total = 5' "$work/commit.$new_sha" > "$work/commit.next"
mv "$work/commit.next" "$work/commit.$new_sha"
key_run
check "a file with added lines and no patch, which git cannot read either, exits 2" "$(is "$status" 2)"
check "says UNKNOWN for that file" "$(contains "$out" "UNKNOWN: GitHub returned no patch for file 1 of commit $new_short, and git could not read it either")"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order"
commit_file "$new_sha" modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text' 9
key_run
check "a commit whose listed files fall short of its total exits 2" "$(is "$status" 2)"
check "says UNKNOWN for that commit" "$(contains "$out" "UNKNOWN: commit $new_short changes 9 lines")"

echo "case: a commit GitHub returns without a whole patch is read from git"
# GitHub leaves the patch out for a large file, and past a response's patch budget; git reads the
# same commit from the pull request's head (refs/pull/7/head of the stand-in remote) whole.
# pr_head <path> <content>: a commit on the fixture history's main that writes <content> to
# <path>, pushed to the stand-in remote as the pull request's head; sets pr_sha to its id.
pr_head() {
  local src="$work/history-src" bare="$work/remote/example/repo"
  history_remote
  git_fixture "$src" checkout -q -B pr main
  mkdir -p "$src/$(dirname "$1")"
  printf '%s\n' "$2" > "$src/$1"
  git_fixture "$src" add "$1"
  git_fixture "$src" commit -q -m "fix: write the file"
  pr_sha=$(git_fixture "$src" rev-parse HEAD)
  git_fixture "$src" push -q --force "$bare" HEAD:refs/pull/7/head
  git_fixture "$src" checkout -q main
}
# unpatched_head <content>: a pull request opened after the cutoff whose one commit adds
# docs/big.md with <content>, a commit GitHub answers with no patch for that file.
unpatched_head() {
  write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
  key_rule_on
  opened_at 2026-11-21T00:00:00Z
  pr_head docs/big.md "$1"
  commits "$pr_sha" 2026-11-21T01:00:00Z "fix: keep the order"
  commit_file "$pr_sha" added docs/big.md -
  jq -c '.files[0].additions = 2 | .files[0].changes = 2 | .stats.total = 2' "$work/commit.$pr_sha" \
    > "$work/commit.next"
  mv "$work/commit.next" "$work/commit.$pr_sha"
}
unpatched_head "$(printf 'line one\nline two, as %s said' "$planted_key")"
key_run
check "a key in the file GitHub gave no patch for fails" "$(is "$status" 1)"
check "says git read the commit instead" \
  "$(contains "$out" "GitHub returned no patch for file 1 of commit ${pr_sha:0:12}, so git reads that commit instead")"
check "names the line git read, at its line in the file" \
  "$(contains "$out" "commit:${pr_sha:0:12}:docs/big.md:2: $key_refused")"
check "prints no key" "$(is "$(contains "$out" "$planted_key")" false)"
unpatched_head "$(printf 'line one\nline two')"
key_run
check "the same file, clean, passes" "$(is "$status" 0)"
check "and nothing is UNKNOWN" "$(is "$(contains "$out" 'UNKNOWN')" false)"
# A section of git's diff that cannot be copied into its entry's patch: a `cat` ahead of the real
# one on PATH fails for the split's section files alone. The diff git read is then not held whole,
# so the commit is UNKNOWN and refused, with the cause named, never passed with a key unread.
fail_dir="$work/failing-cat"
mkdir -p "$fail_dir"
# The script's own "$@" and $a are for the fake cat to expand, not this harness.
# shellcheck disable=SC2016
printf '#!/usr/bin/env bash\nfor a in "$@"; do case $a in */sections/*) echo "cat: refused $a" >&2; exit 1 ;; esac; done\nexec %q "$@"\n' \
  "$(command -v cat)" > "$fail_dir/cat"
chmod +x "$fail_dir/cat"
unpatched_head "$(printf 'line one\nline two, as %s said' "$planted_key")"
PATH="$fail_dir:$PATH" key_run
check "a section git's diff cannot copy into its entry's patch: exits 2" "$(is "$status" 2)"
check "says UNKNOWN and names the section it could not copy" \
  "$(contains "$out" "UNKNOWN: GitHub returned no patch for file 1 of commit ${pr_sha:0:12}, and git could not read it either (section 1 of git's diff could not be copied into entry 1's patch)")"
# A split of git's diff into its sections that fails: an `awk` ahead of the real one on PATH fails
# for the split alone, the one call handed `dir=<stage>/sections`. The commit is UNKNOWN and
# refused, with the cause named.
split_dir="$work/failing-awk"
mkdir -p "$split_dir"
# The script's own "$@" and $a are for the fake awk to expand, not this harness.
# shellcheck disable=SC2016
printf '#!/usr/bin/env bash\nfor a in "$@"; do case $a in dir=*/sections) echo "awk: refused $a" >&2; exit 1 ;; esac; done\nexec %q "$@"\n' \
  "$(command -v awk)" > "$split_dir/awk"
chmod +x "$split_dir/awk"
unpatched_head "$(printf 'line one\nline two, as %s said' "$planted_key")"
PATH="$split_dir:$PATH" key_run
check "a split of git's diff that fails: exits 2" "$(is "$status" 2)"
check "says UNKNOWN and names the failed split" \
  "$(contains "$out" "UNKNOWN: GitHub returned no patch for file 1 of commit ${pr_sha:0:12}, and git could not read it either (git's diff could not be split into its file sections)")"
# A file that becomes a directory: git's diff of that commit lists the rename of the file and the
# file added under the directory of its old name, and each gets its own patch, so the key the
# commit moves into the new file is carried once, not counted twice.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
src="$work/history-src"
git_fixture "$src" checkout -q -B pr main
mkdir -p "$src/docs"
printf 'neutral line %s\n' 1 2 3 4 5 6 7 8 9 10 > "$src/docs/old"
printf 'Tracked as %s.\n' "$planted_key" >> "$src/docs/old"
git_fixture "$src" add docs/old
git_fixture "$src" commit -q -m "docs: a file"
git_fixture "$src" rm -q docs/old
mkdir -p "$src/docs/old"
printf 'neutral line %s\n' 1 2 3 4 5 6 7 8 9 10 > "$src/docs/moved"
printf 'Tracked as %s.\n' "$planted_key" > "$src/docs/old/notes.md"
git_fixture "$src" add docs/moved docs/old/notes.md
git_fixture "$src" commit -q -m "docs: the file becomes a directory"
pr_sha=$(git_fixture "$src" rev-parse HEAD)
git_fixture "$src" push -q --force "$work/remote/example/repo" HEAD:refs/pull/7/head
git_fixture "$src" checkout -q main
commits "$pr_sha" 2026-11-21T01:00:00Z "fix: keep the order"
commit_file "$pr_sha" added docs/old/notes.md -
jq -c '.files[0].additions = 1 | .files[0].changes = 1 | .stats.total = 1' "$work/commit.$pr_sha" \
  > "$work/commit.next"
mv "$work/commit.next" "$work/commit.$pr_sha"
key_run
check "a key moved from a file into the directory that replaces it, read from git, passes" "$(is "$status" 0)"
check "and git read the commit" "$(contains "$out" 'so git reads that commit instead')"
# A file whose first NUL byte comes past byte 8000, which git reads as text: the key on the line
# after the NUL is read, as the REST path reads it, and not skipped as binary.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
git_fixture "$src" checkout -q -B pr main
mkdir -p "$src/docs"
{
  for n in $(seq 1 200); do printf 'filler line %03d of neutral text, long enough\n' "$n"; done
  printf 'a line with a NUL \000 byte\n'
  printf 'Tracked as %s.\n' "$planted_key"
} > "$src/docs/nul.md"
git_fixture "$src" add docs/nul.md
git_fixture "$src" commit -q -m "docs: a file with a late NUL"
pr_sha=$(git_fixture "$src" rev-parse HEAD)
git_fixture "$src" push -q --force "$work/remote/example/repo" HEAD:refs/pull/7/head
git_fixture "$src" checkout -q main
commits "$pr_sha" 2026-11-21T01:00:00Z "fix: keep the order"
commit_file "$pr_sha" added docs/nul.md -
jq -c '.files[0].additions = 202 | .files[0].changes = 202 | .stats.total = 202' "$work/commit.$pr_sha" \
  > "$work/commit.next"
mv "$work/commit.next" "$work/commit.$pr_sha"
key_run
check "a key after a NUL past byte 8000, read from git, fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" "commit:${pr_sha:0:12}:docs/nul.md:202: $key_refused")"
# typechange_head <big.md content> [link path] [link target]: a commit that adds docs/big.md, which
# GitHub answers with no patch, and turns <link path> (docs/link.md by default) from a file into a
# symlink to <link target> (notes.md by default): git's diff gives that typechange two sections and
# its name list one entry.
typechange_head() {
  local link=${2:-docs/link.md}
  write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
  key_rule_on
  opened_at 2026-11-21T00:00:00Z
  git_fixture "$src" checkout -q -B pr main
  mkdir -p "$src/docs"
  printf 'a plain file\n' > "$src/$link"
  git_fixture "$src" add "$link"
  git_fixture "$src" commit -q -m "docs: a plain file"
  rm "$src/$link"
  ln -s "${3:-notes.md}" "$src/$link"
  printf '%s\n' "$1" > "$src/docs/big.md"
  git_fixture "$src" add "$link" docs/big.md
  git_fixture "$src" commit -q -m "docs: a link and a big file"
  pr_sha=$(git_fixture "$src" rev-parse HEAD)
  git_fixture "$src" push -q --force "$work/remote/example/repo" HEAD:refs/pull/7/head
  git_fixture "$src" checkout -q main
  commits "$pr_sha" 2026-11-21T01:00:00Z "fix: keep the order"
  commit_file "$pr_sha" added docs/big.md -
  jq -c '.files[0].additions = 2 | .files[0].changes = 2 | .stats.total = 4' "$work/commit.$pr_sha" \
    > "$work/commit.next"
  mv "$work/commit.next" "$work/commit.$pr_sha"
}
typechange_head "$(printf 'line one\nline two')"
key_run
check "a commit that also turns a file into a symlink, read from git, passes clean" "$(is "$status" 0)"
check "and nothing is UNKNOWN" "$(is "$(contains "$out" 'UNKNOWN')" false)"
typechange_head "$(printf 'line one\nline two, as %s said' "$planted_key")"
key_run
check "the same commit with a key in the big file fails" "$(is "$status" 1)"
check "names that line" "$(contains "$out" "commit:${pr_sha:0:12}:docs/big.md:2: $key_refused")"
# A path the typechange keeps is no new text: each section of its patch starts afresh, so the
# second section's `+++ b/<path>` header is no added line, though that path holds a key.
typechange_head "$(printf 'line one\nline two')" "docs/$planted_key-link.md"
key_run
check "a file whose path holds a key, turned into a symlink, passes: the path is no new text" "$(is "$status" 0)"
# The typechange's new side is read: a key in the symlink's target, its second section's one added
# line, is new text.
typechange_head "$(printf 'line one\nline two')" docs/link.md "$planted_key.md"
key_run
check "a symlink whose target holds a key, from a file, fails: its new side is read" "$(is "$status" 1)"
check "names the link's one line" "$(contains "$out" "commit:${pr_sha:0:12}:docs/link.md:1: $key_refused")"

echo "case: a key the same diff also removes is carried over, not new"
# carry_commit <status> <path> <patch>: a pull request opened after the cutoff whose one commit is
# that one file, as GitHub answers it.
carry_commit() {
  write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
  key_rule_on
  opened_at 2026-11-21T00:00:00Z
  commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order"
  commit_file "$new_sha" "$1" "$2" "$3"
}
carry_commit modified docs/notes.md \
  "$(printf '@@ -2,1 +2,1 @@\n-Tracked as %s, see %sexample/1.\n+Now tracked as %s, see %sexample/1.' \
    "$planted_key" "$link" "$planted_key" "$link")"
key_run
check "a line re-worded around the key it held, beside a link, passes" "$(is "$status" 0)"
carry_commit modified docs/notes.md \
  "$(printf '@@ -2,1 +2,2 @@\n-Tracked as %s.\n+Now tracked as %s.\n+And again as %s.' \
    "$planted_key" "$planted_key" "$planted_key")"
key_run
check "a second copy of the key, which no removed line carries, fails" "$(is "$status" 1)"
check "names the second copy's line" "$(contains "$out" "commit:$new_short:docs/notes.md:3: $key_refused")"
check "and not the carried one's" "$(is "$(contains "$out" "commit:$new_short:docs/notes.md:2:")" false)"
carry_commit modified docs/notes.md "$(printf '@@ -2,1 +2,1 @@\n-See %sexample/1.\n+See %sexample/2.' "$link" "$link")"
key_run
check "a link to another place passes: a link is no private name" "$(is "$status" 0)"
carry_commit modified docs/notes.md \
  "$(printf '@@ -2,1 +2,1 @@\n-See %s%s/ask/1.\n+See %s%s/comment/2.' "$link" "$planted_key" "$link" "$planted_key")"
key_run
check "a key a re-pointed link keeps is carried by the key alone: passes" "$(is "$status" 0)"
carry_commit modified docs/notes.md \
  "$(printf '@@ -2,1 +2,1 @@\n-See %sexample/1.\n+See %s%s/ask/1.' "$link" "$link" "$planted_key")"
key_run
check "a key a link gains, which no removed line carries, fails" "$(is "$status" 1)"
check "names its line as a key" "$(contains "$out" "commit:$new_short:docs/notes.md:2: $key_refused")"
carry_commit added docs/new.md "$(printf '@@ -0,0 +1,1 @@\n+Tracked as %s.' "$planted_key")"
jq -c --arg old "$(printf '@@ -1,1 +0,0 @@\n-Tracked as %s.' "$planted_key")" \
  '.files = [{filename: "docs/old.md", status: "removed", additions: 0, changes: 1, patch: $old}] + .files
   | .stats.total = 2' "$work/commit.$new_sha" > "$work/commit.next"
mv "$work/commit.next" "$work/commit.$new_sha"
key_run
check "a key a commit moves from a removed file into an added one passes" "$(is "$status" 0)"
carry_commit renamed "docs/$planted_key-notes.md" $'@@ -1,0 +2,1 @@\n+clean text'
jq -c --arg old "docs/old/$planted_key-notes.md" '.files[0].previous_filename = $old' "$work/commit.$new_sha" \
  > "$work/commit.next"
mv "$work/commit.next" "$work/commit.$new_sha"
key_run
check "a renamed file whose new path keeps its old path's key passes" "$(is "$status" 0)"
carry_commit renamed "docs/$planted_key-notes.md" $'@@ -1,0 +2,1 @@\n+clean text'
jq -c '.files[0].previous_filename = "docs/notes.md"' "$work/commit.$new_sha" > "$work/commit.next"
mv "$work/commit.next" "$work/commit.$new_sha"
key_run
check "a renamed file whose new path adds a key fails" "$(is "$status" 1)"
check "names the path, masked" "$(contains "$out" "^::error::commit:$new_short:docs/<name>: $key_refused .* in its path\$")"
carry_commit modified docs/notes.md "$(printf '@@ -2,1 +2,1 @@\n-Filed as \xc3\xa9%s.\n+Filed as %s.' "$planted_key" "$planted_key")"
key_run
check "a removed word beside a non-ASCII letter, which is no key, carries no key: fails" "$(is "$status" 1)"
check "names the added key's line" "$(contains "$out" "commit:$new_short:docs/notes.md:2: $key_refused")"
carry_commit modified docs/notes.md "$(printf '@@ -2,1 +2,1 @@\n-Filed as fix_%s.\n+Filed as %s.' "$planted_key" "$planted_key")"
key_run
check "a removed key beside a \"_\", which is a key, carries it: passes" "$(is "$status" 0)"
for mark in . ',' ';' :; do
  carry_commit modified docs/notes.md \
    "$(printf '@@ -2,1 +2,1 @@\n-Tracked at %s%s today.\n+Recorded at %s%s%s which the operator answered' \
      "$link" "$planted_key" "$link" "$planted_key" "$mark")"
  key_run
  check "a key in a link the line already held, now followed by '$mark', is carried: passes" "$(is "$status" 0)"
  carry_commit modified docs/notes.md \
    "$(printf '@@ -2,1 +2,1 @@\n-Tracked as %s today.\n+Now %s%s as before' "$planted_key" "$planted_key" "$mark")"
  key_run
  check "a key the line already held, now followed by '$mark', is carried: passes" "$(is "$status" 0)"
done

echo "case: a key one commit removes and a later one puts back is carried across the commits"
# two_commits <first message> <first file> <first patch> <second message> <second file> <second
# patch>: a pull request opened after the cutoff whose two commits change one file each.
two_commits() {
  write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
  key_rule_on
  opened_at 2026-11-21T00:00:00Z
  commits "$first_sha" 2026-11-21T01:00:00Z "$1" "$second_sha" 2026-11-21T02:00:00Z "$4"
  commit_file "$first_sha" modified "$2" "$3"
  commit_file "$second_sha" modified "$5" "$6"
}
removed_line=$(printf '@@ -2,1 +1,0 @@\n-Tracked as %s.' "$planted_key")
added_line=$(printf '@@ -0,0 +1,1 @@\n+Tracked as %s.' "$planted_key")
two_commits "refactor: take the note out" docs/a.md "$removed_line" "refactor: put the note back" docs/b.md "$added_line"
key_run
check "removed by the first commit, put back by the second: passes" "$(is "$status" 0)"
two_commits "refactor: put the note in" docs/b.md "$added_line" "refactor: take the old one out" docs/a.md "$removed_line"
key_run
check "put in by the first commit, its old copy removed by the second: passes" "$(is "$status" 0)"
two_commits "refactor: take the note out" docs/a.md "$removed_line" "refactor: put it back twice" docs/b.md \
  "$(printf '@@ -0,0 +1,2 @@\n+Tracked as %s.\n+Tracked as %s.' "$planted_key" "$planted_key")"
key_run
check "put back twice, one removal carries one copy: fails" "$(is "$status" 1)"
check "names the second copy's line" "$(contains "$out" "commit:$second_short:docs/b.md:2: $key_refused")"
check "and not the carried one's" "$(is "$(contains "$out" "commit:$second_short:docs/b.md:1:")" false)"

echo "case: a merge commit's own diff is not read, and the pull request's net diff is, once"
# A forward merge's diff against its first parent is everything the base branch changed since the
# branch left it: other pull requests' text, and often large files. The net diff is the lines the
# pull request adds to its base, a line typed while resolving a conflict among them. The three
# controls a review asked for: the branch's clean commit alone; plus a merge whose own diff adds
# another pull request's key; plus a merge whose own diff holds a large file with no patch.
merge_branch() {
  write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
  key_rule_on
  opened_at 2026-11-21T00:00:00Z
  commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order" "$merge_sha" 2026-11-21T02:00:00Z "${1:-Merge main forward}"
  as_merge "$merge_sha"
  commit_file "$new_sha" modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
}
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order"
commit_file "$new_sha" modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "control 1, the branch's own clean commit alone, exits 0" "$(is "$status" 0)"
merge_branch
commit_file "$merge_sha" modified docs/other-pr.md $'@@ -0,0 +1,1 @@\n+other '"$planted_key"' line'
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "control 2, a merge whose own diff adds another pull request's key, exits 0" "$(is "$status" 0)"
check "never asks GitHub for the merge's own files" "$(is "$(contains "$(< "$work/calls")" "commits/$merge_sha")" false)"
check "reads the pull request's files once" "$(is "$(grep -c 'pulls/7/files' "$work/calls")" 1)"
merge_branch
commit_file "$merge_sha" modified docs/big.md -
jq -c '.files[0].additions = 5000 | .files[0].changes = 5000 | .stats.total = 5000' "$work/commit.$merge_sha" > "$work/commit.next"
mv "$work/commit.next" "$work/commit.$merge_sha"
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "control 3, a merge whose own diff holds a large file with no patch, exits 0" "$(is "$status" 0)"
check "and says nothing UNKNOWN" "$(is "$(contains "$out" 'UNKNOWN')" false)"
merge_branch
commit_file "$merge_sha" modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
pr_files modified docs/notes.md $'@@ -1,0 +2,2 @@\n+clean text\n+resolved as '"$planted_key"' said'
key_run
check "a key typed while resolving a conflict, in the net diff, fails" "$(is "$status" 1)"
check "names the net diff's file and line" "$(contains "$out" "files:docs/notes.md:3: $key_refused")"
check "prints no key" "$(is "$(contains "$out" "$planted_key")" false)"
merge_branch "Merge main forward, as $planted_key asked"
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "a merge's message is still read" "$(is "$status" 1)"
check "names the merge's message" "$(contains "$out" "commit:$merge_short:1: $key_refused")"
# A forward merge's message names the pull request's branch, and an issue branch is named after its
# key: the branch is not read through the message. A key the message names some other way still is.
# forward_merge <message> [head branch]: a pull request from that branch (legion/<key> by default)
# whose second commit is a merge with that message.
forward_merge() {
  write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order" "${2:-legion/$planted_key}"
  key_rule_on
  opened_at 2026-11-21T00:00:00Z
  commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order" "$merge_sha" 2026-11-21T02:00:00Z "$1"
  as_merge "$merge_sha"
  commit_file "$new_sha" modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
  pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
}
forward_merge "Merge main into legion/$planted_key"
key_run
check "the worker skill's 'Merge main into <branch>': passes" "$(is "$status" 0)"
forward_merge "Merge branch 'main' into legion/$planted_key"
key_run
check "GitHub's update-branch message: passes" "$(is "$status" 0)"
forward_merge "Merge remote-tracking branch 'origin/main' into legion/$planted_key"
key_run
check "git's message for a merge of origin/main: passes" "$(is "$status" 0)"
forward_merge "Merge main into legion/$planted_key, as $key_project-""77 asked"
key_run
check "the same message naming another key: fails" "$(is "$status" 1)"
check "names the merge's message" "$(contains "$out" "commit:$merge_short:1: $key_refused")"
# A stacked pull request's base is another issue branch, which its forward merge names too.
forward_merge "Merge legion/$key_project-""77 into legion/$planted_key"
git_fixture "$work/remote/example/repo" branch -f "legion/$key_project-""77" main
jq --arg base "legion/$key_project-""77" '.base.ref = $base' "$work/pull" > "$work/pull.next"
mv "$work/pull.next" "$work/pull"
key_run
check "a stacked pull request's merge of its base issue branch: passes" "$(is "$status" 0)"
# A ref is replaced only where it stands whole: a longer branch that holds the head branch as its
# start keeps its own key.
forward_merge "Merge branch legion/$planted_key into legion/$key_project-""12" "legion/$key_project-""12"
key_run
check "head legion/<key>-12, a message naming the branch legion/<key>-123: fails" "$(is "$status" 1)"
check "names the merge's message" "$(contains "$out" "commit:$merge_short:1: $key_refused")"
forward_merge "Merge main into legion/$key_project-""12" "legion/$key_project-""12"
key_run
check "the same head's own forward merge still passes" "$(is "$status" 0)"
# Only a merge's message has its branches replaced: a branch named exactly a key does not hide that
# key in a message a person wrote.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order" "$planted_key"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$new_sha" 2026-11-21T01:00:00Z "fix: close the order, as $planted_key asked"
commit_file "$new_sha" modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "head named exactly the key, a commit (no merge) naming it: fails" "$(is "$status" 1)"
check "names that commit's message" "$(contains "$out" "commit:$new_short:1: $key_refused")"
merge_branch
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text' modified docs/big.md -
jq -c '.[1].additions = 5 | .[1].changes = 5' "$work/pr-files" > "$work/pr-files.next"
mv "$work/pr-files.next" "$work/pr-files"
jq '.additions = 6' "$work/pull" > "$work/pull.next" && mv "$work/pull.next" "$work/pull"
key_run
check "a net diff file that adds lines with no patch, at a head git cannot fetch, exits 2" "$(is "$status" 2)"
check "says UNKNOWN for that file of the net diff" "$(contains "$out" "UNKNOWN: GitHub returned no patch for file 2 of #7's net diff")"
merge_branch
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
jq '.changed_files = 3001' "$work/pull" > "$work/pull.next" && mv "$work/pull.next" "$work/pull"
key_run
check "a net diff listed short of the pull request's totals, at a head git cannot fetch, exits 2" "$(is "$status" 2)"
check "says UNKNOWN for the net diff" "$(contains "$out" 'UNKNOWN: #7 holds 1 merge commit(s), so its net diff is read')"
# The same short list at a head git can fetch: the net diff is read from git, from where the head
# leaves main to the head.
merge_branch
pr_head docs/notes.md "$(printf 'clean text\nresolved as %s said' "$planted_key")"
jq --arg head "$pr_sha" '.pull_request.head.sha = $head' "$work/event.json" > "$work/event.next"
mv "$work/event.next" "$work/event.json"
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
jq '.changed_files = 3001' "$work/pull" > "$work/pull.next" && mv "$work/pull.next" "$work/pull"
key_run
check "a net diff listed short, read from git at the head, fails on the key it adds" "$(is "$status" 1)"
check "says git read the net diff instead" "$(contains "$out" 'so git reads the net diff instead')"
check "names the line git read" "$(contains "$out" "files:docs/notes.md:2: $key_refused")"
check "and nothing is UNKNOWN" "$(is "$(contains "$out" 'UNKNOWN')" false)"

echo "case: a commit dated before the cutoff is read after all when the pull request holds a forward merge"
# The one exception to "a commit dated before the cutoff is not read": the net diff is the whole
# pull request's. D1 and D2 are the same branch, opened after the cutoff, whose commit dated
# before it adds a key line; D2 also holds a clean forward merge after the cutoff.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$old_sha" 2026-11-19T00:00:00Z "fix: keep the order"
commit_file "$old_sha" modified docs/notes.md $'@@ -1,0 +2,1 @@\n+old '"$planted_key"' line'
key_run
check "D1, the old commit alone, is not read: exits 0" "$(is "$status" 0)"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$old_sha" 2026-11-19T00:00:00Z "fix: keep the order" "$merge_sha" 2026-11-21T02:00:00Z "Merge main forward"
as_merge "$merge_sha"
commit_file "$old_sha" modified docs/notes.md $'@@ -1,0 +2,1 @@\n+old '"$planted_key"' line'
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+old '"$planted_key"' line'
key_run
check "D2, the same branch with a clean forward merge, is read through its net diff: exits 1" "$(is "$status" 1)"
check "names the net diff's line" "$(contains "$out" "files:docs/notes.md:2: $key_refused")"

echo "case: the tree outside a pull request's new text is not held to the issue-key rule"
root=$(fixture key-tree)
printf 'From before the rule: %s.\n' "$planted_key" > "$root/docs/notes.md"
run_check_as pull_request "$key_terms" "$root"
check "a key in a tree file passes the repository scan" "$(is "$status" 0)"
check "and its count line says the key is read but not checked" \
  "$(contains "$out" '^check-private-names: PRIVATE_NAME_TERMS: 3 terms read; 1 issue key not checked: rule 6 runs only with PRIVATE_NAME_RULES=keys$')"
check "and does not count the key beside the terms it checks" "$(is "$(contains "$out" 'and 1 issue key read')" false)"
set +e
out=$(cd "$root" && PRIVATE_NAME_TERMS=$key_terms GITHUB_EVENT_NAME=pull_request PRIVATE_NAME_RULES=keys \
  .github/scripts/check-private-names.sh docs 2>&1)
status=$?
set -e
check "the same file fails a keys run" "$(is "$status" 1)"
check "names its line" "$(contains "$out" "docs/notes.md:1: $key_refused")"
check "and the keys run counts the key it checks" \
  "$(contains "$out" '^check-private-names: PRIVATE_NAME_TERMS: 3 terms and 1 issue key read$')"

# keys_run_in <root> <terms> [path...]: a keys run of the check from <root> on a merge group,
# under <terms> as the list.
keys_run_in() {
  local root=$1 list=$2
  shift 2
  set +e
  out=$(cd "$root" && PRIVATE_NAME_TERMS=$list GITHUB_EVENT_NAME=merge_group PRIVATE_NAME_RULES=keys \
    .github/scripts/check-private-names.sh "$@" 2>&1)
  status=$?
  set -e
}

echo "case: a keys run passes a file that holds only a Dispatch link"
root=$(fixture key-link)
printf 'Decided at %sexample/ask/1.\n' "$link" > "$root/docs/notes.md"
keys_run_in "$root" "$key_terms" docs
check "exits 0" "$(is "$status" 0)"
check "says no text names a key" \
  "$(contains "$out" "^check-private-names: no text names an issue key of the deployment's own projects (1 keys;")"
printf 'Tracked as %s.\n' "$planted_key" >> "$root/docs/notes.md"
keys_run_in "$root" "$key_terms" docs
check "the same file with a key on its next line exits 1" "$(is "$status" 1)"
check "names the key's line alone" "$(contains "$out" '^check-private-names: 1 finding(s)')"
check "names line 2" "$(contains "$out" "docs/notes.md:2: $key_refused")"

echo "case: a key beside a \"_\" is a key, in a line and in a path"
# GNU grep's \b counts "_" as a word character, so a key glued to a "_" passed a \b pattern.
root=$(fixture key-underscore)
printf 'Moved under fix_%s now.\n' "$planted_key" > "$root/docs/notes.md"
mkdir -p "$root/docs/plans"
printf 'clean text\n' > "$root/docs/plans/${planted_key}_plan.md"
keys_run_in "$root" "$key_terms" docs
check "exits 1" "$(is "$status" 1)"
check "names the line where the key follows a \"_\"" "$(contains "$out" "docs/notes.md:1: $key_refused")"
check "names the path where the key comes before a \"_\", masked" \
  "$(contains "$out" "^::error::docs/plans/<name>: $key_refused .* in its path\$")"
check "prints no key" "$(is "$(contains "$out" "$planted_key")" false)"
root=$(fixture key-underscore-lower)
printf 'Runs as fix_%s now.\n' "${planted_key,,}" > "$root/docs/notes.md"
keys_run_in "$root" "$key_terms" docs
check "the same line in lower case still passes" "$(is "$status" 0)"

echo "case: a keys run under a list with terms and no key: line fails, rather than checking no project"
root=$(fixture key-no-key-line)
printf 'Tracked as %s.\n' "$planted_key" > "$root/docs/notes.md"
keys_run_in "$root" "$planted_terms" docs
check "exits 2" "$(is "$status" 2)"
check "says the list holds no key: line" \
  "$(contains "$out" '^::error::check-private-names: PRIVATE_NAME_TERMS holds no key: line')"
keys_run_in "$root" "$(printf '%s\n' "$planted_terms" key:LEGION)" docs
check "a key: line for Legion's own project alone counts as none: exits 2" "$(is "$status" 2)"
keys_run_in "$root" "$key_terms" docs
check "the same run with the project's key: line finds the key: exits 1" "$(is "$status" 1)"
run_check_as merge_group "$planted_terms" "$root"
check "a names run of the tree, which reads no key, still passes with no key: line" "$(is "$status" 0)"
# A fork's pull request runs with no list at all, which its event allows: a keys run then reads no
# project, and says that it checked nothing rather than that no text names a key.
PRIVATE_NAME_RULES=keys run_check_as pull_request '' "$root" docs
check "a keys run under no list on a pull_request event exits 0" "$(is "$status" 0)"
check "warns that no issue key was checked" \
  "$(contains "$out" '^::warning::check-private-names: PRIVATE_NAME_TERMS is unset or empty, so no issue key')"
check "and its last line says it checked none" \
  "$(contains "${out##*$'\n'}" '^check-private-names: no issue key was checked: PRIVATE_NAME_TERMS is unset or empty')"
check "and never that no text names a key" "$(is "$(contains "$out" 'no text names an issue key')" false)"
# A list of key: lines alone holds no term, so the script reads it as an empty list; but it names a
# project, so a keys run reads that project's keys, and warns of nothing.
PRIVATE_NAME_RULES=keys run_check_as pull_request "key:$key_project" "$root" docs
check "a keys run under a list of key: lines alone finds the key: exits 1" "$(is "$status" 1)"
check "and warns of nothing" "$(is "$(contains "$out" '^::warning::')" false)"

echo "case: no issue key's project reaches a command's arguments"
# argv_grep (check-private-names-test-lib.sh) logs each pattern a grep is handed on its command
# line. The key is in a path and in a file, so the keys run searches both and masks the path.
root=$(fixture key-argv)
printf 'Tracked as %s.\n' "$planted_key" > "$root/docs/notes.md"
mkdir -p "$root/docs/$planted_key"
printf 'clean\n' > "$root/docs/$planted_key/notes.md"
argv_dir="$work/key-argv-grep"
argv_log="$work/key-argv-grep.log"
mkdir -p "$argv_dir"
: > "$argv_log"
argv_grep "$argv_dir/grep" "$argv_log"
PATH="$argv_dir:$PATH" keys_run_in "$root" "$key_terms" docs
check "the keys run still finds the key: fails" "$(is "$status" 1)"
check "the path's report is masked" "$(contains "$out" "docs/<name>/notes.md: $key_refused")"
check "the run's search grep, its pattern from a file, reached the fake grep" \
  "$(contains "$(< "$argv_log")" '^<search grep -f>$')"
check "no grep was handed the project's key in its arguments" "$(is "$(contains "$(< "$argv_log")" "$key_project")" false)"

# --- What check-pr-text.sh prints of GitHub's. Whatever answers gh's calls in GitHub's place
# holds the token gh sends and writes gh's words and gh's answer, so each case below puts the
# marker not-a-secret-token in one of them: no run of eight of its characters (the run the
# redaction rule's gate reads) may be in what the script printed. The twin of each: the file the
# line names holds the marker, or (for the filter) the same answer reaches the output with the
# token set to something else. The control: the stub was asked for that read.
marker=not-a-secret-token
holds_marker() {
  local at
  for ((at = 0; at + 8 <= ${#marker}; at++)); do
    [[ "$1" == *"${marker:at:8}"* ]] && return 0
  done
  return 1
}
prints_no_marker() { holds_marker "$1" && echo false || echo true; }
holds() { holds_marker "$1" && echo true || echo false; }
# kept_in <pattern>: the file the output's last line holding <pattern> names after it.
kept_in() { sed -n "s/^.*$1\([^ ,;)]*\).*\$/\1/p" <<<"$out" | tail -n 1; }
kept_holds_marker() {
  local file
  file=$(kept_in "$1")
  [ -n "$file" ] && grep -q -F "$marker" -- "$file" && echo true || echo false
}

echo "case: a read GitHub's GraphQL API refuses with the marker in its words"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
printf 'Something went wrong: %s\n' "$marker" > "$work/graphql-says"
echo 1 > "$work/graphql-fails"
key_run
check "exits 1, as it did" "$(is "$status" 1)"
check "prints no run of the marker" "$(prints_no_marker "$out")"
check "names the read and the file that keeps gh's words" \
  "$(contains "$out" "^::error::GitHub's GraphQL API did not answer the PrText read of #7 (gh; its own words are in ")"
check "twin: that file holds the marker" "$(kept_holds_marker "its own words are in ")"
check "control: the stub answered the read" "$(contains "$(< "$work/calls")" '^graphql PrText ')"

echo "case: a base branch, a time, an association and a path GitHub answers that hold the marker"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
jq --arg ref "main $marker" '.base.ref = $ref' "$work/pull" > "$work/pull.next" && mv "$work/pull.next" "$work/pull"
key_run
check "a base branch that is no branch name: prints no run of the marker" "$(prints_no_marker "$out")"
check "names it as none and the file that keeps it" \
  "$(contains "$out" 'the base branch GitHub.s GraphQL API named (no branch name git accepts; it is in ')"
check "twin: that file holds the marker" "$(kept_holds_marker 'it is in ')"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at "$marker"
key_run
check "an opened-at time that is no timestamp: prints no run of the marker" "$(prints_no_marker "$out")"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
release legion-v9.0.0 "$marker"
key_run
check "a published time that is no timestamp: prints no run of the marker" "$(prints_no_marker "$out")"
check "names it as none and the file that keeps it" \
  "$(contains "$out" 'at a time that is no UTC timestamp (it is in ')"
check "twin: that file holds the marker" "$(kept_holds_marker 'it is in ')"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
items 801 "fine" > "$work/comments.1"
by stranger User "$marker" "$work/comments.1"
key_run
check "an author association that is none of GitHub's: prints no run of the marker" "$(prints_no_marker "$out")"
check "names it as none and the file that keeps it" \
  "$(contains "$out" "^skipped comment:801: its author (an association that is none of GitHub's, which is in ")"
check "twin: that file holds the marker" "$(kept_holds_marker 'which is in ')"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order"
commit_file "$new_sha" modified "../$marker-notes.md" "$(printf '@@ -1,0 +2,1 @@\n+Tracked as %s.' "$planted_key")"
key_run
check "a path that is no relative path: prints no run of the marker" "$(prints_no_marker "$out")"
check "and names the finding by the place the path has in the diff" \
  "$(contains "$out" "commit:$new_short:file 1:2: $key_refused")"
check "names it by its place and the file that keeps it" \
  "$(contains "$out" "^check-pr-text: file 1 of the diff of commit $new_short has a path that is no relative path; it is in ")"
check "twin: that file holds the marker" "$(kept_holds_marker 'it is in ')"

echo "case: a commit id that is no commit id, holding the marker"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$marker" 2026-11-21T01:00:00Z "fix: keep the order"
key_run
check "exits 2: UNKNOWN, never passed" "$(is "$status" 2)"
check "prints no run of the marker" "$(prints_no_marker "$out")"
check "says the id is no commit id and names the file that keeps it" \
  "$(contains "$out" '^::error::UNKNOWN: GitHub lists a commit of #7 from after the cutoff whose id is no 40-hex commit id (it is in ')"
check "twin: that file holds the marker" "$(kept_holds_marker 'it is in ')"

echo "case: every line passes the filter for the token gh sends"
# A path that holds to the grammar is printed as the check's reading of it, and a path is GitHub's
# answer: a path that holds the token is written [redacted] by the filter every line passes. The
# twin is the same run under another token, where that path prints whole.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits "$new_sha" 2026-11-21T01:00:00Z "fix: keep the order"
commit_file "$new_sha" modified "docs/$marker.md" "$(printf '@@ -1,0 +2,1 @@\n+Tracked as %s.' "$planted_key")"
pr_text_token=$marker key_run
check "a path holding the token: prints no run of it" "$(prints_no_marker "$out")"
check "and writes it as [redacted] in the line that names the path" \
  "$(contains "$out" "commit:$new_short:docs/\[redacted\]")"
check "control: the line is the finding over that file's added line" \
  "$(contains "$out" ":2: $key_refused")"
pr_text_token=another-token-entirely key_run
check "twin: the same run under another token prints the path whole" "$(holds "$out")"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
pr_text_token='' key_run
check "a run with no token says no line is filtered" \
  "$(contains "$out" '^check-pr-text: gh auth token answered no token, and no token variable is set, so no line below is filtered for one$')"

finish_self_log
summary "check-private-names.sh's issue-key rule"
