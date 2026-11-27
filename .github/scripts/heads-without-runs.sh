#!/usr/bin/env bash
# Names each open pull request of this repository whose head no workflow ran: a pull request that
# is no draft, whose head is 20 minutes old or more, and whose head carries no check run at all.
# .github/workflows/heads-without-runs.yaml runs it every half hour.
#
# Why. ci-ok, the check main's ruleset requires, comes from a ci.yaml run. A lane job that a pull
# request's labels hold back still leaves a skipped check run on the head, and the ci-ok gate reads
# it. A head that no workflow ran carries no check run at all, so it has no ci-ok, and nothing on
# the pull request says why. This check names such heads.
# (.github/ci-notes/heads-without-runs.md#why-the-check-exists)
#
# What it reads (.github/ci-notes/heads-without-runs.md#what-the-check-reads says why each part):
#   1. Every open pull request, 25 a page, through GitHub's GraphQL API: its number, whether it is
#      a draft, when it was opened, its head, and of the head commit its committer date and, for
#      each check suite on it, whether the suite holds a check run. A check run of any status or
#      conclusion counts, `skipped` among them.
#   2. For each head it names, that pull request through the REST API: `mergeable` says why the
#      head has no run. GitHub starts no pull_request run for a pull request that conflicts with
#      its base. A `mergeable` of null means GitHub is still computing it, and GitHub's REST
#      documentation asks the caller to read again, so a null is read once more after
#      MERGEABLE_RETRY_SECONDS.
#
# The age of a head is measured from the later of its commit's committer date and the opening of
# its pull request. GitHub keeps no time a commit was pushed (GraphQL's Commit.pushedDate reads
# null), so a commit made long ago and pushed just now reads as old. Such a head can be named in
# the first 20 minutes after its push, before its run has posted a check run, and is cleared on
# the next run once one has.
#
# What it prints. On stdout, one line a named head, `#<number> <head sha> <age> <why it has no
# run>`, where the reason is one of:
#   conflicts with its base branch: no merge ref, so no run   (mergeable false)
#   no run and no conflict                                     (mergeable true)
#   mergeability not yet computed                              (mergeable null twice)
#   mergeability not read: <what failed>                       (the REST read failed)
# and a last line that counts them, or one line that says it named none and how many pull requests
# it read. The base branch is never named: a branch's name is not this check's to print in a public
# log. A head whose check runs could not be decided is printed as `#<number> <head sha> UNKNOWN:
# <why>`. Each head that fails the check gets one ::error:: line, at most 10 in all, the number
# GitHub keeps for a step; the tenth says how many more the log and the summary hold. With
# GITHUB_STEP_SUMMARY set, it writes the same list as a table there.
#
# Its exit status:
#   0  it named no head, or every head it named conflicts with its base;
#   1  it named a head with no run and no conflict, or one whose mergeability it could not tell;
#   2  UNKNOWN: it could not read the list of open pull requests, the list was empty, or it could
#      not decide whether some head carries a check run. It never says it named none then.
# A conflict alone does not fail the check
# (.github/ci-notes/heads-without-runs.md#why-a-conflict-alone-does-not-fail-the-check).
#
# Its inputs, from the step's env:
#   GH_TOKEN                 the job's github.token, with `pull-requests: read`, `checks: read`
#                            and `contents: read`. Every call goes through gh-read.sh's gh_read,
#                            which waits out a spent budget within the job's JOB_TIMEOUT_MINUTES
#                            and tries twice more after any other refusal.
#   GITHUB_REPOSITORY        owner/repo, as Actions sets it.
#   GITHUB_STEP_SUMMARY      optional: where the table goes.
#   MERGEABLE_RETRY_SECONDS  optional: the wait before a null `mergeable` is read again (10 by
#                            default; the tests set 0).
#
# Run from anywhere: GITHUB_REPOSITORY=<owner>/<repo> .github/scripts/heads-without-runs.sh
# Its tests are in heads-without-runs.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit
# shellcheck source=.github/scripts/gh-read.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/gh-read.sh"

# A head younger than this may still be waiting for its run to post a check run.
MIN_AGE_SECONDS=1200
# GitHub keeps at most 10 error annotations a step.
MAX_ERRORS=10

summary_file=${GITHUB_STEP_SUMMARY:-/dev/null}

# error <text>: one ::error:: line, escaped as a workflow command's data must be.
error() {
  local text=$1
  text=${text//'%'/%25}
  text=${text//$'\r'/%0D}
  text=${text//$'\n'/%0A}
  echo "::error::$text" >&2
}

# unknown <what failed>: the refusal of a run that could not read the list. It names no head and
# clears none.
unknown() {
  local text="UNKNOWN: $1, so this check read no head: it names none and clears none"
  error "$text"
  printf '## Open pull requests whose head no workflow ran\n\n%s\n' "$text" >> "$summary_file"
  exit 2
}

repository=${GITHUB_REPOSITORY:-}
[[ $repository =~ ^[^/]+/[^/]+$ ]] || unknown "GITHUB_REPOSITORY '$repository' names no repository as owner/repo"
retry_seconds=${MERGEABLE_RETRY_SECONDS:-10}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

read_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
now=$(date -u +%s)

# The list, a page at a time: each page's nodes are appended to $work/nodes, one JSON object a
# line. 25 a page, because GitHub answered a page of 50 with RESOURCE_LIMITS_EXCEEDED on the
# check suites of this repository's newer pull requests (the notes say when).
read -r -d '' query <<'GRAPHQL' || true
query OpenPulls($owner: String!, $repo: String!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequests(states: OPEN, first: 25, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number isDraft createdAt headRefOid
        commits(last: 1) { nodes { commit { oid committedDate
          checkSuites(first: 100) { pageInfo { hasNextPage } nodes { checkRuns(first: 1) { nodes { databaseId } } } }
        } } }
      }
    }
  }
}
GRAPHQL
# shellcheck disable=SC2016 # $date is jq's
shape='def date: type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$");
  (.errors == null) and (.data.repository.pullRequests
  | (.pageInfo.hasNextPage | type == "boolean") and (.nodes | type == "array")
    and all(.nodes[]; (.number | type == "number") and (.isDraft | type == "boolean")
      and (.createdAt | date) and (.headRefOid | type == "string" and test("^[0-9a-f]{40}$"))
      and (.commits.nodes | type == "array")
      and all(.commits.nodes[]; (.commit.oid | type == "string") and (.commit.committedDate | date)
        and (.commit.checkSuites.pageInfo.hasNextPage | type == "boolean")
        and all(.commit.checkSuites.nodes[]; .checkRuns.nodes | type == "array"))))'
: > "$work/nodes"
after=null
while :; do
  jq -n --arg query "$query" --arg owner "${repository%%/*}" --arg repo "${repository#*/}" \
    --argjson after "$after" \
    '{operationName: "OpenPulls", query: $query, variables: {owner: $owner, repo: $repo, after: $after}}' \
    > "$work/request.json"
  if ! gh_read gh api graphql --input "$work/request.json" > "$work/answer.json"; then
    unknown "GitHub's GraphQL API refused the list of open pull requests at every attempt"
  fi
  if ! jq -e "$shape" "$work/answer.json" > /dev/null 2>&1; then
    unknown "GitHub's GraphQL API answered the list of open pull requests in a shape this check does not read"
  fi
  jq -c '.data.repository.pullRequests.nodes[]' "$work/answer.json" >> "$work/nodes"
  [ "$(jq -r '.data.repository.pullRequests.pageInfo.hasNextPage' "$work/answer.json")" = true ] || break
  next=$(jq -c '.data.repository.pullRequests.pageInfo.endCursor' "$work/answer.json")
  if [ "$next" = null ] || [ "$next" = "$after" ]; then
    unknown "GitHub's GraphQL API said another page of open pull requests follows, and named no new cursor for it"
  fi
  after=$next
done

# Each pull request once, though a page boundary that moved between two reads lists it twice.
jq -s 'unique_by(.number)' "$work/nodes" > "$work/pulls.json"
open=$(jq 'length' "$work/pulls.json")
drafts=$(jq 'map(select(.isDraft)) | length' "$work/pulls.json")
[ "$open" -gt 0 ] || unknown "GitHub's GraphQL API listed no open pull request at all, which reads the same as a list it could not see"

# One row a head, tab-separated: `named <number> <head> <age>` for a head with no check run that is
# old enough, or `unread <number> <head> <why>` for a head this read cannot decide. A draft, a head
# with a check run and a young head print nothing.
# shellcheck disable=SC2016 # $now, $min and $m are jq's
rows=$(jq -r --argjson now "$now" --argjson min "$MIN_AGE_SECONDS" '
  def age: (. / 60 | floor) as $m
    | if $m >= 1440 then "\($m / 1440 | floor)d\($m % 1440 / 60 | floor)h\($m % 60)m"
      elif $m >= 60 then "\($m / 60 | floor)h\($m % 60)m" else "\($m)m" end;
  .[] | select(.isDraft | not) | .headRefOid as $head | (.commits.nodes[0].commit // null) as $commit
  | if $commit == null or $commit.oid != $head then
      ["unread", .number, $head, "the last commit GitHub lists for it is not its head, so its check runs were not read"]
    elif any($commit.checkSuites.nodes[]; .checkRuns.nodes | length > 0) then empty
    elif $commit.checkSuites.pageInfo.hasNextPage then
      ["unread", .number, $head, "none of the first 100 check suites on its head holds a check run, and more suites follow"]
    else
      ([($commit.committedDate | fromdateiso8601), (.createdAt | fromdateiso8601)] | max) as $since
      | select($now - $since >= $min)
      | ["named", .number, $head, ($now - $since | age)]
    end
  | @tsv' "$work/pulls.json")

# judge <number> <head>: why the head has no run, from the pull request's REST answer, in
# cause[<number>]; left unset where `mergeable` reads null. `moved` where the pull request closed
# or took a new head since the list was read: the next run judges what it is now.
declare -A cause
judge() {
  local number=$1 head=$2 state mergeable head_now
  if ! gh_read gh api "repos/$repository/pulls/$number" > "$work/pull.json"; then
    cause[$number]="mergeability not read: GitHub's REST API refused the pull request at every attempt"
    return
  fi
  if ! jq -e '(.state | type == "string") and (.mergeable == null or (.mergeable | type == "boolean"))
      and (.head.sha | type == "string")' "$work/pull.json" > /dev/null 2>&1; then
    cause[$number]="mergeability not read: GitHub's REST API answered the pull request in a shape this check does not read"
    return
  fi
  IFS=$'\t' read -r state mergeable head_now \
    <<<"$(jq -r '[.state, (.mergeable | tostring), .head.sha] | @tsv' "$work/pull.json")"
  if [ "$state" != open ] || [ "$head_now" != "$head" ]; then
    cause[$number]=moved
  elif [ "$mergeable" = false ]; then
    cause[$number]="conflicts with its base branch: no merge ref, so no run"
  elif [ "$mergeable" = true ]; then
    cause[$number]="no run and no conflict"
  fi
  return 0
}

named=() unread=()
declare -A head_of age_of why_unread
while IFS=$'\t' read -r kind number head detail; do
  [ -n "$kind" ] || continue
  head_of[$number]=$head
  if [ "$kind" = named ]; then
    named+=("$number")
    age_of[$number]=$detail
  else
    unread+=("$number")
    why_unread[$number]=$detail
  fi
done <<<"$rows"

for number in "${named[@]}"; do
  judge "$number" "${head_of[$number]}"
done
computing=()
for number in "${named[@]}"; do
  [ -n "${cause[$number]:-}" ] || computing+=("$number")
done
if [ "${#computing[@]}" -gt 0 ]; then
  echo "GitHub was still computing the mergeability of ${#computing[@]} pull request(s); reading them again in $retry_seconds s."
  sleep "$retry_seconds"
  for number in "${computing[@]}"; do
    judge "$number" "${head_of[$number]}"
    [ -n "${cause[$number]:-}" ] || cause[$number]="mergeability not yet computed"
  done
fi

# The report: stdout, the annotations, the summary.
errors=() clean=0 unsure=0 conflicts=0 listed=0
{
  echo "## Open pull requests whose head no workflow ran"
  echo
  echo "Read at $read_at: $open open pull requests, $drafts of them drafts. A head is named when its pull request is no draft, the head is 20 minutes old or more, and no check suite on it holds a check run."
  echo
} >> "$summary_file"
table() { # table <number> <head> <age> <why>
  if [ "$listed" -eq 0 ]; then
    printf '| Pull request | Head | Age | Why it has no run |\n|---|---|---|---|\n' >> "$summary_file"
  fi
  listed=$((listed + 1))
  # shellcheck disable=SC2016 # the backticks are Markdown's
  printf '| #%s | `%s` | %s | %s |\n' "$1" "$2" "$3" "$4" >> "$summary_file"
}
for number in "${named[@]}"; do
  head=${head_of[$number]} age=${age_of[$number]} why=${cause[$number]}
  if [ "$why" = moved ]; then
    echo "#$number changed while it was read (it closed, or took a new head): the next run judges it"
    continue
  fi
  echo "#$number $head $age $why"
  table "$number" "$head" "$age" "$why"
  error_line="#$number: its head $head is $age old and carries no check run: $why"
  case $why in
    conflicts\ with\ *) conflicts=$((conflicts + 1)) ;;
    no\ run\ and\ no\ conflict) clean=$((clean + 1)) errors+=("$error_line") ;;
    *) unsure=$((unsure + 1)) errors+=("$error_line") ;;
  esac
done
for number in "${unread[@]}"; do
  head=${head_of[$number]} why=${why_unread[$number]}
  echo "#$number $head UNKNOWN: $why"
  table "$number" "$head" "-" "UNKNOWN: $why"
  errors+=("UNKNOWN: #$number: $why")
done

for i in "${!errors[@]}"; do
  if [ "${#errors[@]}" -gt "$MAX_ERRORS" ] && [ "$i" -eq $((MAX_ERRORS - 1)) ]; then
    error "and $((${#errors[@]} - MAX_ERRORS + 1)) more: the job log and the step summary list every one"
    break
  fi
  error "${errors[$i]}"
done

if [ "$listed" -eq 0 ]; then
  line="Named none: no open pull request that is no draft has a head 20 minutes old or more with no check run ($open open pull requests read, $drafts of them drafts)."
else
  line="Named $listed head(s) of $open open pull requests: $clean with no run and no conflict, $unsure whose mergeability is not known, $conflicts that conflict with their base branch, ${#unread[@]} UNKNOWN."
fi
echo "$line"
printf '\n%s\n' "$line" >> "$summary_file"

[ "${#unread[@]}" -eq 0 ] || exit 2
[ $((clean + unsure)) -eq 0 ] || exit 1
