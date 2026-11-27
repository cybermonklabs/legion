#!/usr/bin/env bash
# One reading of this repository's open pull requests and of main's merge queue, with three cases
# (.github/ci-notes/heads-without-runs.md#one-check-three-cases):
#   1. a pull request's head that no workflow ran because it conflicts with its base branch;
#   2. a pull request's head that no workflow ran for no reason anyone can see, the one case that
#      fails the check;
#   3. an entry of main's merge queue whose head has come to conflict with main, a warning.
# .github/workflows/heads-without-runs.yaml runs it every half hour. It writes nothing to GitHub:
# no comment, no check run, and it never removes an entry from the queue.
#
# Why cases 1 and 2. ci-ok, the check main's ruleset requires, comes from a ci.yaml run. A lane job
# that a pull request's labels hold back still leaves a skipped check run on the head, and the ci-ok
# gate reads it. A head that no workflow ran carries no check run at all, so it has no ci-ok, and
# nothing on the pull request says why (.github/ci-notes/heads-without-runs.md#why-the-check-exists).
#
# What cases 1 and 2 read (.github/ci-notes/heads-without-runs.md#what-the-check-reads):
#   1. Every open pull request, 25 a page, through GitHub's GraphQL API: its number, whether it is
#      a draft, when it was opened, its head, and of the head commit its committer date and, for
#      each check suite on it, whether the suite holds a check run. A check run of any status or
#      conclusion counts, `skipped` among them. A pull request that is no draft, whose head is 20
#      minutes old or more and carries no check run, is named.
#   2. For each head it names, that pull request through the REST API: `mergeable` false is case 1,
#      true is case 2. A `mergeable` of null means GitHub is still computing it, and GitHub's REST
#      documentation asks the caller to read again, so a null is read once more after
#      MERGEABLE_RETRY_SECONDS.
# The age of a head is measured from the later of its commit's committer date and the opening of
# its pull request. GitHub keeps no time a commit was pushed (GraphQL's Commit.pushedDate reads
# null), so a commit made long ago and pushed just now reads as old. Such a head can be named in
# the first 20 minutes after its push, before its run has posted a check run, and is cleared on
# the next run once one has.
#
# What case 3 reads
# (.github/ci-notes/heads-without-runs.md#what-the-queue-case-reads-and-why-it-only-warns):
#   1. Every entry of main's merge queue through GitHub's GraphQL API: its position, its state,
#      when it was queued, and its pull request's number and head.
#   2. One fetch, of this public repository with no credential, into a bare repository of its own:
#      main and every queued head, with their history and no file contents (--filter=blob:none).
#      `git merge-tree --write-tree --name-only` of main's tip and each head: exit 1 is a conflict
#      and names the conflicting files, exit 0 is clean. git merge-tree fetches the contents it
#      merges as it needs them.
#   3. For each entry that conflicts, the pull request whose landing made it conflict: main as it
#      was when the entry was queued (the `after` of the last move of main before that time, from
#      the REST route repos/<repo>/activity, read once), then each commit of main after it, first
#      parent first, until one conflicts with the head; the subject of that commit ends with the
#      pull request's number in brackets, as the queue's squash writes it. Main's own commit dates
#      are not read: the queue
#      writes the commit when it builds the group.
#
# What it prints, on stdout:
#   #<number> <head sha> <age> <why it has no run>   a named head, where the reason is one of
#       conflicts with its base branch: no merge ref, so no run   (mergeable false: case 1)
#       no run and no conflict                                     (mergeable true: case 2)
#       mergeability not yet computed                              (mergeable null twice)
#       mergeability not read: <what failed>                       (the REST read failed)
#   #<number> <head sha> UNKNOWN: <why>              a head whose check runs could not be decided
#   a line that counts the named heads, or says it named none and how many pull requests it read;
#   queued #<number> <head sha> position <n> conflicts with main in <files>; caused by <#n, or
#       UNKNOWN: why>                                an entry of case 3
#   queued #<number> <head sha> UNKNOWN: <why>       an entry git could not merge with main
#   a last line that counts the queue's entries and those that conflict.
# The base branch of a pull request is never named: a branch's name is not this check's to print in
# a public log. Each case-2 head, each head whose mergeability it could not tell and each UNKNOWN
# gets one ::error:: line; each case-3 entry one ::warning:: line. GitHub keeps at most 10 of each
# kind a step, so the tenth says how many more the log and the summary hold. With
# GITHUB_STEP_SUMMARY set, it writes both lists as tables there.
#
# Its exit status:
#   0  it named no case-2 head, whatever it named of cases 1 and 3;
#   1  it named a head with no run and no conflict, or one whose mergeability it could not tell;
#   2  UNKNOWN: it could not read the list of open pull requests or the queue, the list was empty,
#      git could not fetch or merge, or it could not decide whether some head carries a check run.
#      It never says it named none of a part it could not read, and a part it could read is still
#      reported.
# Neither a conflict nor a queued entry that conflicts fails the check
# (.github/ci-notes/heads-without-runs.md#why-a-conflict-alone-does-not-fail-the-check).
#
# Its inputs, from the step's env:
#   GH_TOKEN                 the job's github.token, with `pull-requests: read`, `checks: read`
#                            and `contents: read`. Every GitHub call and the fetch go through
#                            gh-read.sh's gh_read, which waits out a spent budget within the job's
#                            JOB_TIMEOUT_MINUTES and tries twice more after any other refusal.
#   GITHUB_REPOSITORY, GITHUB_SERVER_URL  as Actions sets them: the repository read and fetched.
#   GITHUB_STEP_SUMMARY      optional: where the tables go.
#   MERGEABLE_RETRY_SECONDS  optional: the wait before a null `mergeable` is read again (10 by
#                            default; the tests set 0).
#
# Run from anywhere:
#   GITHUB_REPOSITORY=<owner>/<repo> GITHUB_SERVER_URL=https://github.com .github/scripts/heads-without-runs.sh
# Its tests are in heads-without-runs.test.sh, which ci.yaml's test job runs.
set -euo pipefail
shopt -s inherit_errexit
# shellcheck source=.github/scripts/gh-read.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/gh-read.sh"

# A head younger than this may still be waiting for its run to post a check run.
MIN_AGE_SECONDS=1200
# GitHub keeps at most 10 error annotations and 10 warning annotations a step.
MAX_ANNOTATIONS=10
# The branch whose merge queue case 3 reads.
QUEUE_BRANCH=main

summary_file=${GITHUB_STEP_SUMMARY:-/dev/null}

# annotate <error|warning> <text>: one workflow command line, its data escaped as GitHub asks.
annotate() {
  local text=$2
  text=${text//'%'/%25}
  text=${text//$'\r'/%0D}
  text=${text//$'\n'/%0A}
  echo "::$1::$text" >&2
}

# A bad input reads nothing at all.
refuse() {
  annotate error "UNKNOWN: $1, so this check read nothing: it names none and clears none"
  exit 2
}
repository=${GITHUB_REPOSITORY:-}
[[ $repository =~ ^[^/]+/[^/]+$ ]] || refuse "GITHUB_REPOSITORY '$repository' names no repository as owner/repo"
server=${GITHUB_SERVER_URL:-}
[ -n "$server" ] || refuse "GITHUB_SERVER_URL names no server to fetch $repository from"
retry_seconds=${MERGEABLE_RETRY_SECONDS:-10}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

read_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
now=$(date -u +%s)

# What the two parts leave for the end: the lines to annotate, and whether a part could not read.
errors=() warnings=() unknown=0 failing=0

# graphql <operation> <query> <variables as JSON> <answer file>: one GraphQL read through gh_read.
graphql() {
  jq -n --arg operation "$1" --arg query "$2" --argjson variables "$3" \
    '{operationName: $operation, query: $query, variables: $variables}' > "$work/request.json"
  gh_read gh api graphql --input "$work/request.json" > "$4"
}

# ------------------------------------------------------------------- cases 1 and 2: open heads

# heads_unknown <what failed>: the heads part read nothing it can name or clear.
heads_unknown() {
  local text="UNKNOWN: $1, so this check read no head: it names none and clears none"
  errors+=("$text")
  echo "$text"
  printf '%s\n' "$text" >> "$summary_file"
  unknown=1
}

# judge <number> <head>: why the head has no run, from the pull request's REST answer, in
# cause[<number>]; left unset where `mergeable` reads null. `moved` where the pull request closed
# or took a new head since the list was read: the next run judges what it is now.
declare -A cause head_of age_of why_unread
judge() {
  local number=$1 head=$2 state mergeable head_now
  if ! gh_read gh api "repos/$repository/pulls/$number" > "$work/pull.json"; then
    cause[$number]="mergeability not read: GitHub's REST API refused the pull request at every attempt"
    return 0
  fi
  if ! jq -e '(.state | type == "string") and (.mergeable == null or (.mergeable | type == "boolean"))
      and (.head.sha | type == "string")' "$work/pull.json" > /dev/null 2>&1; then
    cause[$number]="mergeability not read: GitHub's REST API answered the pull request in a shape this check does not read"
    return 0
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

heads_part() {
  local query shape after next open drafts rows kind number head detail named=() unread=() computing=()
  local clean=0 unsure=0 conflicts=0 listed=0 age why line error_line
  printf '## Open pull requests whose head no workflow ran\n\n' >> "$summary_file"

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
    if ! graphql OpenPulls "$query" \
        "$(jq -nc --arg o "${repository%%/*}" --arg r "${repository#*/}" --argjson a "$after" '{owner: $o, repo: $r, after: $a}')" \
        "$work/answer.json"; then
      heads_unknown "GitHub's GraphQL API refused the list of open pull requests at every attempt"
      return 0
    fi
    if ! jq -e "$shape" "$work/answer.json" > /dev/null 2>&1; then
      heads_unknown "GitHub's GraphQL API answered the list of open pull requests in a shape this check does not read"
      return 0
    fi
    jq -c '.data.repository.pullRequests.nodes[]' "$work/answer.json" >> "$work/nodes"
    [ "$(jq -r '.data.repository.pullRequests.pageInfo.hasNextPage' "$work/answer.json")" = true ] || break
    next=$(jq -c '.data.repository.pullRequests.pageInfo.endCursor' "$work/answer.json")
    if [ "$next" = null ] || [ "$next" = "$after" ]; then
      heads_unknown "GitHub's GraphQL API said another page of open pull requests follows, and named no new cursor for it"
      return 0
    fi
    after=$next
  done

  # Each pull request once, though a page boundary that moved between two reads lists it twice.
  jq -s 'unique_by(.number)' "$work/nodes" > "$work/pulls.json"
  open=$(jq 'length' "$work/pulls.json")
  drafts=$(jq 'map(select(.isDraft)) | length' "$work/pulls.json")
  if [ "$open" -eq 0 ]; then
    heads_unknown "GitHub's GraphQL API listed no open pull request at all, which reads the same as a list it could not see"
    return 0
  fi

  # One row a head, tab-separated: `named <number> <head> <age>` for a head with no check run that
  # is old enough, or `unread <number> <head> <why>` for a head this read cannot decide. A draft, a
  # head with a check run and a young head print nothing.
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

  printf 'Read at %s: %s open pull requests, %s of them drafts. A head is named when its pull request is no draft, the head is 20 minutes old or more, and no check suite on it holds a check run.\n\n' \
    "$read_at" "$open" "$drafts" >> "$summary_file"
  for number in "${named[@]}" "${unread[@]}"; do
    head=${head_of[$number]}
    if [ -n "${why_unread[$number]:-}" ]; then
      age=- why="UNKNOWN: ${why_unread[$number]}"
    else
      age=${age_of[$number]} why=${cause[$number]}
    fi
    if [ "$why" = moved ]; then
      echo "#$number changed while it was read (it closed, or took a new head): the next run judges it"
      continue
    fi
    if [ "$listed" -eq 0 ]; then
      printf '| Pull request | Head | Age | Why it has no run |\n|---|---|---|---|\n' >> "$summary_file"
    fi
    listed=$((listed + 1))
    # shellcheck disable=SC2016 # the backticks are Markdown's
    printf '| #%s | `%s` | %s | %s |\n' "$number" "$head" "$age" "$why" >> "$summary_file"
    error_line="#$number: its head $head is $age old and carries no check run: $why"
    case $why in
      UNKNOWN:\ *)
        echo "#$number $head $why"
        errors+=("UNKNOWN: #$number: ${why_unread[$number]}")
        unknown=1
        continue
        ;;
    esac
    echo "#$number $head $age $why"
    case $why in
      conflicts\ with\ *) conflicts=$((conflicts + 1)) ;;
      no\ run\ and\ no\ conflict) clean=$((clean + 1)) errors+=("$error_line") ;;
      *) unsure=$((unsure + 1)) errors+=("$error_line") ;;
    esac
  done
  failing=$((clean + unsure))

  if [ "$listed" -eq 0 ]; then
    line="Named none: no open pull request that is no draft has a head 20 minutes old or more with no check run ($open open pull requests read, $drafts of them drafts)."
  else
    line="Named $listed head(s) of $open open pull requests: $clean with no run and no conflict, $unsure whose mergeability is not known, $conflicts that conflict with their base branch, ${#unread[@]} UNKNOWN."
  fi
  echo "$line"
  printf '\n%s\n' "$line" >> "$summary_file"
}

# ------------------------------------------------------------- case 3: queued entries gone dirty

repo="$work/queue.git"

# queue_unknown <what failed>: the queue part read nothing it can name or clear.
queue_unknown() {
  local text="UNKNOWN: $1, so this check read no entry of $QUEUE_BRANCH's merge queue: it names none and clears none"
  errors+=("$text")
  echo "$text"
  printf '%s\n' "$text" >> "$summary_file"
  unknown=1
}

# conflicts <out> <commit of main> <head>: git merge-tree of the two. Returns 0 for a clean merge;
# 1 for a conflict, with the conflicting files, NUL-separated, in <out>; 2 when git could not merge
# or its answer could not be read, with git's words on stderr. git answers 1 for a revision it
# cannot read too, so a merge counts only when its first field is a tree. Called where `set -e`
# does not reach inside it, so each step returns 2 on its own failure.
conflicts() {
  local out=$1 status=0 fields=() i
  git -C "$repo" merge-tree --write-tree --name-only -z "$2" "$3" > "$out.raw" 2> "$out.err" || status=$?
  mapfile -d '' -t fields < "$out.raw" || return 2
  if [ "$status" -gt 1 ] || ! [[ ${fields[0]:-} =~ ^[0-9a-f]{40}$ ]]; then
    cat "$out.err" >&2
    return 2
  fi
  : > "$out" || return 2
  [ "$status" -eq 1 ] || return 0
  for ((i = 1; i < ${#fields[@]} && ${#fields[i]} > 0; i++)); do
    printf '%s\0' "${fields[i]}" >> "$out" || return 2
  done
  [ -s "$out" ] || return 2
  return 1
}

# main_moves <oldest queue time, epoch>: reads every move of main back past that time from GitHub's
# activity route into $work/moves.json, [{timestamp, after}], or prints why it could not and fails.
# The route's time_period is the shortest of its periods that reaches that far back.
main_moves() {
  local age=$((now - $1)) period=year
  if [ "$age" -lt $((20 * 3600)) ]; then period=day
  elif [ "$age" -lt $((6 * 86400)) ]; then period=week
  elif [ "$age" -lt $((29 * 86400)) ]; then period=month
  elif [ "$age" -lt $((89 * 86400)) ]; then period=quarter
  fi
  if ! gh_read gh api --paginate "repos/$repository/activity?ref=refs/heads/$QUEUE_BRANCH&time_period=$period&per_page=100" \
      --jq '.[] | {timestamp, after}' > "$work/moves.jsonl"; then
    echo "GitHub's REST API refused main's activity at every attempt"
    return 1
  fi
  if ! jq -se 'all(.[]; (.timestamp | type == "string") and (.after | type == "string" and test("^[0-9a-f]{40}$")))' \
      "$work/moves.jsonl" > /dev/null 2>&1; then
    echo "GitHub's REST API answered main's activity in a shape this check does not read"
    return 1
  fi
  jq -s '.' "$work/moves.jsonl" > "$work/moves.json"
}

# culprit <head> <queued at>: prints the pull request whose landing on main made <head> conflict,
# `#<n>`, or `UNKNOWN: <why>`.
culprit() {
  local head=$1 queued=$2 base commit subject steps=() status
  [ -z "$moves_unread" ] || { echo "UNKNOWN: $moves_unread"; return 0; }
  # shellcheck disable=SC2016 # $at is jq's
  base=$(jq -r --arg at "$queued" 'def t: sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601;
    map(select((.timestamp | t) <= ($at | t))) | max_by(.timestamp | t) | .after // empty' "$work/moves.json")
  if [ -z "$base" ]; then
    echo "UNKNOWN: main's activity names no move of main before it was queued at $queued"
    return 0
  fi
  if ! git -C "$repo" merge-base --is-ancestor "$base" "$tip" 2> /dev/null; then
    echo "UNKNOWN: main when it was queued, $base, is not below main's tip"
    return 0
  fi
  status=0
  conflicts "$work/step" "$base" "$head" || status=$?
  case $status in
    1) echo "UNKNOWN: its head conflicted with main already when it was queued, at $base"; return 0 ;;
    2) echo "UNKNOWN: git could not merge its head with main as it was when it was queued, $base"; return 0 ;;
  esac
  mapfile -t steps < <(git -C "$repo" rev-list --first-parent --reverse "$base..$tip")
  for commit in "${steps[@]}"; do
    status=0
    conflicts "$work/step" "$commit" "$head" || status=$?
    case $status in
      0) continue ;;
      2) echo "UNKNOWN: git could not merge its head with main's commit $commit"; return 0 ;;
    esac
    subject=$(git -C "$repo" log -1 --format=%s "$commit")
    if [[ $subject =~ \(#([0-9]+)\)$ ]]; then
      echo "#${BASH_REMATCH[1]}"
    else
      echo "UNKNOWN: main's commit $commit made it conflict, and its subject names no pull request"
    fi
    return 0
  done
  echo "UNKNOWN: no commit of main since it was queued makes its head conflict"
}

moves_unread='' tip=''
queue_part() {
  local query shape after next entries count order=() dirty=() undecided=0 number head position queued
  local status files oldest cause_of line
  local -a paths=()
  local -A entry_head entry_position entry_queued entry_files
  printf '\n## Entries of %s'"'"'s merge queue that conflict with %s\n\n' "$QUEUE_BRANCH" "$QUEUE_BRANCH" >> "$summary_file"

  read -r -d '' query <<'GRAPHQL' || true
query MainQueue($owner: String!, $repo: String!, $branch: String!, $after: String) {
  repository(owner: $owner, name: $repo) {
    mergeQueue(branch: $branch) {
      entries(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes { position state enqueuedAt pullRequest { number headRefOid } }
      }
    }
  }
}
GRAPHQL
  # shellcheck disable=SC2016 # nothing here is the shell's
  shape='(.errors == null) and (.data.repository.mergeQueue.entries
    | (.pageInfo.hasNextPage | type == "boolean") and (.nodes | type == "array")
      and all(.nodes[]; (.position | type == "number") and (.state | type == "string")
        and (.enqueuedAt | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
        and (.pullRequest.number | type == "number")
        and (.pullRequest.headRefOid | type == "string" and test("^[0-9a-f]{40}$"))))'
  : > "$work/entries"
  after=null
  while :; do
    if ! graphql MainQueue "$query" \
        "$(jq -nc --arg o "${repository%%/*}" --arg r "${repository#*/}" --arg b "$QUEUE_BRANCH" --argjson a "$after" '{owner: $o, repo: $r, branch: $b, after: $a}')" \
        "$work/queue.json"; then
      queue_unknown "GitHub's GraphQL API refused the list of $QUEUE_BRANCH's merge queue at every attempt"
      return 0
    fi
    if ! jq -e "$shape" "$work/queue.json" > /dev/null 2>&1; then
      queue_unknown "GitHub's GraphQL API answered the list of $QUEUE_BRANCH's merge queue in a shape this check does not read, or named no merge queue for $QUEUE_BRANCH"
      return 0
    fi
    jq -c '.data.repository.mergeQueue.entries.nodes[]' "$work/queue.json" >> "$work/entries"
    [ "$(jq -r '.data.repository.mergeQueue.entries.pageInfo.hasNextPage' "$work/queue.json")" = true ] || break
    next=$(jq -c '.data.repository.mergeQueue.entries.pageInfo.endCursor' "$work/queue.json")
    if [ "$next" = null ] || [ "$next" = "$after" ]; then
      queue_unknown "GitHub's GraphQL API said another page of $QUEUE_BRANCH's merge queue follows, and named no new cursor for it"
      return 0
    fi
    after=$next
  done
  entries=$(jq -sr 'unique_by(.pullRequest.number) | sort_by(.position)[]
    | [.pullRequest.number, .pullRequest.headRefOid, .position, .enqueuedAt] | @tsv' "$work/entries")
  count=0
  while IFS=$'\t' read -r number head position queued; do
    [ -n "$number" ] || continue
    count=$((count + 1))
    order+=("$number")
    entry_head[$number]=$head entry_position[$number]=$position entry_queued[$number]=$queued
  done <<<"$entries"
  printf 'Read at %s: %s entries.\n\n' "$read_at" "$count" >> "$summary_file"
  if [ "$count" -eq 0 ]; then
    line="$QUEUE_BRANCH's merge queue holds no entry: nothing to merge with $QUEUE_BRANCH."
    echo "$line"
    printf '%s\n' "$line" >> "$summary_file"
    return 0
  fi

  # A bare repository of its own, whose only refs are main's and FETCH_HEAD: no automatic gc runs
  # in it between the fetch and the merges.
  git init -q --bare "$repo"
  git -C "$repo" config gc.auto 0
  git -C "$repo" config maintenance.auto false
  git -C "$repo" remote add origin "$server/$repository.git"
  if ! gh_read git -C "$repo" fetch --quiet --no-tags --filter=blob:none origin \
      "+refs/heads/$QUEUE_BRANCH:refs/remotes/origin/$QUEUE_BRANCH" "${entry_head[@]}"; then
    queue_unknown "git could not fetch $QUEUE_BRANCH and the queued heads from $server/$repository.git"
    return 0
  fi
  tip=$(git -C "$repo" rev-parse --verify "refs/remotes/origin/$QUEUE_BRANCH^{commit}")

  for number in "${order[@]}"; do
    head=${entry_head[$number]}
    status=0
    conflicts "$work/files" "$tip" "$head" || status=$?
    case $status in
      0) ;;
      1)
        mapfile -d '' -t paths < "$work/files"
        files=$(printf '%s, ' "${paths[@]}")
        entry_files[$number]=${files%, }
        dirty+=("$number")
        ;;
      *)
        echo "queued #$number $head UNKNOWN: git could not merge its head with $QUEUE_BRANCH's tip $tip"
        errors+=("UNKNOWN: queued #$number: git could not merge its head $head with $QUEUE_BRANCH's tip $tip")
        undecided=$((undecided + 1))
        unknown=1
        ;;
    esac
  done

  if [ "${#dirty[@]}" -gt 0 ]; then
    oldest=$now
    for number in "${dirty[@]}"; do
      queued=$(date -u -d "${entry_queued[$number]}" +%s)
      [ "$queued" -ge "$oldest" ] || oldest=$queued
    done
    moves_unread=$(main_moves "$oldest") || true
    printf '| Pull request | Head | Position | Conflicting files | Caused by |\n|---|---|---|---|---|\n' >> "$summary_file"
  fi
  for number in "${dirty[@]}"; do
    head=${entry_head[$number]} position=${entry_position[$number]} files=${entry_files[$number]}
    cause_of=$(culprit "$head" "${entry_queued[$number]}")
    echo "queued #$number $head position $position conflicts with $QUEUE_BRANCH in $files; caused by $cause_of"
    # shellcheck disable=SC2016 # the backticks are Markdown's
    printf '| #%s | `%s` | %s | %s | %s |\n' "$number" "$head" "$position" "$files" "$cause_of" >> "$summary_file"
    warnings+=("#$number is in $QUEUE_BRANCH's merge queue at position $position, and its head $head conflicts with $QUEUE_BRANCH in $files; caused by $cause_of")
  done

  if [ "${#dirty[@]}" -eq 0 ] && [ "$undecided" -eq 0 ]; then
    line="No entry of $QUEUE_BRANCH's merge queue conflicts with $QUEUE_BRANCH's tip $tip ($count read)."
  else
    line="Queue: ${#dirty[@]} of $count entries of $QUEUE_BRANCH's merge queue conflict with $QUEUE_BRANCH's tip $tip (warnings; they fail nothing), $undecided UNKNOWN."
  fi
  echo "$line"
  printf '\n%s\n' "$line" >> "$summary_file"
}

heads_part
queue_part

# annotate_all <error|warning> <line>...: each line, at most MAX_ANNOTATIONS, the last of them
# counting the rest.
annotate_all() {
  local kind=$1 i
  shift
  for ((i = 1; i <= $#; i++)); do
    if [ "$#" -gt "$MAX_ANNOTATIONS" ] && [ "$i" -eq "$MAX_ANNOTATIONS" ]; then
      annotate "$kind" "and $(($# - MAX_ANNOTATIONS + 1)) more: the job log and the step summary list every one"
      break
    fi
    annotate "$kind" "${!i}"
  done
}
annotate_all error "${errors[@]}"
annotate_all warning "${warnings[@]}"

[ "$unknown" -eq 0 ] || exit 2
[ "$failing" -eq 0 ] || exit 1
