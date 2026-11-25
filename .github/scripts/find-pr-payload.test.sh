#!/usr/bin/env bash
# Tests for `find-pr-payload.sh`: the payload file `.github/workflows/ci.yaml#pr-title` job hands to
# check-pr-text.sh, which is the event's own payload at every event that carries a pull request
# and, on a merge group, the queued pull request read from the queue branch the group runs on.
#
# Each case writes the payload Actions would write and runs the script with a stand-in `gh` first
# on PATH: it answers the script's GraphQL read of the pull request (`gh api graphql --input <file>`)
# with a fixture in GraphQL's own shape and records each read, its operation and variables, so a
# case can show that a refused head_ref reads nothing. The script under test is the file CI runs,
# and the fixture carries the real shapes: a `gh-readonly-queue/<base>/pr-<n>-<40 hex>` branch,
# and a pull request whose head is the pull request's own, not the group's commit.
#
# Run from anywhere: .github/scripts/find-pr-payload.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin" "$work/temp"

# The head of the queued pull request, and the group's own commit, which the script never reads:
# the queue builds the group on top of the base, so the two differ.
pull_head=365e2766d4bd1f3a2e9c0b7a84f15d6c3e9a2b10
group_head=114285918850b1d4c6e8f0a2b4c6d8e0f2a4b6c8

pull_base=7f3b9c2d5e8a1b4c6d9e0f2a3b5c7d8e9f0a1b2c
jq -n --arg head "$pull_head" --arg base "$pull_base" \
  '{data: {repository: {pullRequest: {number: 2847, baseRefOid: $base, headRefOid: $head}}}}' \
  > "$work/pull-request.answer"

cat > "$work/bin/gh" <<GH
#!/usr/bin/env bash
set -euo pipefail
[ "\$1 \$2 \$3" = "api graphql --input" ] && [ \$# -eq 4 ] || { echo "unexpected gh \$*" >&2; exit 2; }
request=\$(cat "\$4")
echo "graphql \$(jq -r '.operationName' <<< "\$request") \$(jq -c '.variables' <<< "\$request")" >> "$work/gh-calls"
cat "$work/pull-request.answer"
GH
chmod +x "$work/bin/gh"

# merge_group_event <head_ref>: the payload Actions writes for a queued group.
merge_group_event() {
  jq -n --arg ref "$1" --arg sha "$group_head" '{merge_group: {head_ref: $ref, head_sha: $sha}}' \
    > "$work/event.json"
}

# find_payload <event name>: runs the script over $work/event.json; sets out (stdout, the path),
# err (stderr) and status. Every case runs it under `timeout`, so a lookup that hung would fail
# its case instead of hanging the job.
find_payload() {
  rm -f "$work/gh-calls" "$work/temp/pull-request.json" "$work/temp/pull-request-event.json"
  set +e
  out=$(PATH="$work/bin:$PATH" GH_TOKEN=t GITHUB_REPOSITORY=o/r RUNNER_TEMP="$work/temp" \
    GITHUB_EVENT_NAME="$1" GITHUB_EVENT_PATH="$work/event.json" \
    timeout 60 "$script_dir/find-pr-payload.sh" 2> "$work/err")
  status=$?
  set -e
  err=$(cat "$work/err")
}

gh_calls() { cat "$work/gh-calls" 2> /dev/null || true; }

echo "case: a pull_request event carries the pull request, so its own payload comes back"
jq -n '{action: "opened", number: 2847, pull_request: {number: 2847}}' > "$work/event.json"
find_payload pull_request
check "it answers" "$(is "$status" 0)"
check "the path is the event payload Actions wrote" "$(is "$out" "$work/event.json")"
check "it reads no route" "$(is "$(gh_calls)" "")"
check "it says nothing else" "$(is "$err" "")"

echo "case: a well-formed queue ref gives the pull request it names, in check-pr-text.sh's shape"
merge_group_event "refs/heads/gh-readonly-queue/main/pr-2847-$pull_head"
find_payload merge_group
check "it answers" "$(is "$status" 0)"
check "the path is the payload it wrote under RUNNER_TEMP" \
  "$(is "$out" "$work/temp/pull-request-event.json")"
check "it read the pull request the branch names" \
  "$(is "$(gh_calls)" 'graphql QueuedPull {"owner":"o","repo":"r","number":2847}')"
check "the file holds the pull request" \
  "$(is "$(jq -r '.pull_request.head.sha' "$work/temp/pull-request-event.json")" "$pull_head")"
check "and its base" "$(is "$(jq -r '.pull_request.base.sha' "$work/temp/pull-request-event.json")" "$pull_base")"
check "and its number, as a number" \
  "$(is "$(jq -r '.number | tostring + " " + type' "$work/temp/pull-request-event.json")" "2847 number")"
check "the number is the branch's, not the group's commit" \
  "$(is "$(contains "$(cat "$work/temp/pull-request-event.json")" "$group_head")" false)"
# GitHub has sent merge_group.head_ref both with and without the refs/heads/ prefix.
merge_group_event "gh-readonly-queue/main/pr-2847-$pull_head"
find_payload merge_group
check "a branch name without the refs/heads/ prefix is the same ref" "$(is "$status" 0)"
check "it reads the same pull request" \
  "$(is "$(gh_calls)" 'graphql QueuedPull {"owner":"o","repo":"r","number":2847}')"

echo "case: an answer that names no base and head sha is refused and named"
jq '.data.repository.pullRequest.headRefOid = null' "$work/pull-request.answer" > "$work/pull-request.held"
mv "$work/pull-request.answer" "$work/pull-request.good"
mv "$work/pull-request.held" "$work/pull-request.answer"
merge_group_event "refs/heads/gh-readonly-queue/main/pr-2847-$pull_head"
find_payload merge_group
mv "$work/pull-request.good" "$work/pull-request.answer"
check "it refuses" "$(is "$status" 1)"
check "it names the pull request" \
  "$(contains "$err" "^::error::GitHub's GraphQL API answered no base and head sha for #2847, the pull request refs/heads/gh-readonly-queue/main/pr-2847-$pull_head names\$")"
check "it writes no payload" "$(is "$(test -s "$work/temp/pull-request-event.json" && echo yes || echo no)" no)"

echo "case: a ref that names no pull request is refused and named"
merge_group_event refs/heads/main
find_payload merge_group
check "it refuses" "$(is "$status" 1)"
check "it names the ref and the shape it wanted" \
  "$(contains "$err" "^::error::merge_group.head_ref 'refs/heads/main' names no queued pull request (want refs/heads/gh-readonly-queue/<base>/pr-<number>-<40 hex>)\$")"
check "it reads no route" "$(is "$(gh_calls)" "")"
check "it writes no payload" "$(is "$(test -e "$work/temp/pull-request-event.json" && echo yes || echo no)" no)"

echo "case: a ref whose sha is not 40 hex characters is refused, not trimmed to a number"
short=${pull_head:0:39}
merge_group_event "refs/heads/gh-readonly-queue/main/pr-2847-$short"
find_payload merge_group
check "it refuses" "$(is "$status" 1)"
check "it names the ref" \
  "$(contains "$err" "^::error::merge_group.head_ref 'refs/heads/gh-readonly-queue/main/pr-2847-$short' names no queued pull request ")"
check "it reads no route" "$(is "$(gh_calls)" "")"

echo "case: the payload holds no word of the answer but the pull request's number and two shas"
# Whatever answers the read in GitHub's place holds gh's token and writes the answer, and
# check-pr-text.sh prints the number and both shas of the payload this script writes. Each answer
# below holds the marker not-a-secret-token in one of them, as a string, which the script read as
# a sha (or took as the number); no run of eight of its characters (redacttest.Piece's run) may
# be in what the script prints or in the payload it hands on. A refused answer stays in its file.
marker=not-a-secret-token
# prints_no_marker <text>: true when the text holds no run of eight characters of the marker.
prints_no_marker() {
  local at
  for ((at = 0; at + 8 <= ${#marker}; at++)); do
    if [[ $1 == *"${marker:at:8}"* ]]; then
      echo false
      return
    fi
  done
  echo true
}
cp "$work/pull-request.answer" "$work/pull-request.good"
for field in headRefOid baseRefOid number; do
  jq --arg field "$field" --arg marker "$marker" '.data.repository.pullRequest[$field] = $marker' \
    "$work/pull-request.good" > "$work/pull-request.answer"
  merge_group_event "refs/heads/gh-readonly-queue/main/pr-2847-$pull_head"
  find_payload merge_group
  payload=$(cat "$work/temp/pull-request-event.json" 2> /dev/null || true)
  while IFS= read -r line; do printf '    printed: %s\n' "$line"; done <<<"$out"$'\n'"$err"
  printf '    payload: %s\n' "$payload"
  check "$field: the stand-in gh is the one called (control)" \
    "$(is "$(gh_calls)" 'graphql QueuedPull {"owner":"o","repo":"r","number":2847}')"
  check "$field, twin: the stand-in's answer holds the marker" \
    "$(is "$(grep -cF -- "$marker" "$work/pull-request.answer")" 1)"
  check "$field: refused" "$(is "$status" 1)"
  check "$field: prints no run of the marker the answer holds" "$(prints_no_marker "$out$err")"
  check "$field: hands on no run of it in the payload" "$(prints_no_marker "$payload")"
  check "$field: names the file that keeps the answer" \
    "$(contains "$err" "^find-pr-payload.sh: it answered no pull request #2847 with a base and a head sha of 40 lowercase hex digits; its answer is in $work/temp/pull-request.json\$")"
  check "$field: that file holds the marker, where the operator finds it" \
    "$(is "$(grep -cF -- "$marker" "$work/temp/pull-request.json")" 1)"
done
cp "$work/pull-request.good" "$work/pull-request.answer"
find_payload merge_group
check "GitHub's own answer is still handed on (control)" \
  "$(is "$status $(jq -r '.pull_request.head.sha' "$work/temp/pull-request-event.json")" "0 $pull_head")"

summary "the pull request payload the pr-title job checks a pull request's text against"
