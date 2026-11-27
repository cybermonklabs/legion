#!/usr/bin/env bash
# Sourced by the three harnesses of the private-name check: check-private-names.test.sh (the rules
# the script spells, its modes and a pull request's text), check-private-names-terms.test.sh (the
# deployment's own term list, PRIVATE_NAME_TERMS) and check-private-names-keys.test.sh (the
# issue-key rule): what more than one reads and nothing one alone does. It creates the run's
# temporary directory, tees the harness's own output to a log that
# finish_self_log scans with the guard at the end, sources test-lib.sh, and holds the name pieces,
# the placeholder terms, the fixture repository and the stand-in GitHub API the cases share.
#
# Each case builds a miniature git repository under that directory and symlinks the real script
# into it, so the script under test is the file CI runs: it resolves its root from its own path,
# which the symlink puts at the fixture root. The names it hunts are assembled from pieces below,
# so neither this file nor a harness holds one and the check scans them like any other file.
#
# Not executable: a harness sources it after its own `set -euo pipefail` and `set +x`.
# shellcheck disable=SC2034 # the pieces, terms and paths below are the harnesses' to read

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-private-names.sh"
pr_text_script="$script_dir/check-pr-text.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Every case's stdout (echoed "case:" lines and `check`'s PASS/FAIL lines) is the harness's own
# output — the log of ci.yaml's test job — so it is also teed to self_log, which finish_self_log,
# the last case of each harness, scans with the guard itself: a future case that prints an
# assembled form or a placeholder term turns red there, in the suite, before it ever reaches a
# real job log. `exec 3>&1 4>&2` keeps the real terminal fds reachable so the redirection can be
# undone before reading the file: a process substitution's write end must close (and its process
# be waited on) before its output is guaranteed to be flushed to disk.
self_log="$work/suite-output.log"
exec 3>&1 4>&2
exec > >(tee "$self_log") 2>&1
tee_pid=$!

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

command -v git > /dev/null || { echo "  FAIL: git is required" >&2; exit 1; }

repo="agent""-c"
repo_snake="agent""_c"
repo_joined="agent""c"
repo_spaced="agent"" c"
repo_en_dash="agent""–c"
repo_nonbreaking_hyphen="agent""‑c"
repo_percent_hyphen="agent""%2Dc"
nbsp=$'\xc2\xa0'
en_dash=$'\xe2\x80\x93'
key="AGENT""C"
company="traj""ectory"
company_proper="T""rajectory"
labs="labs"
internal="internal"
private_host_suffix="private.example"
example_host_suffix="example"
vendored_nats="packages/claude-envoy/dist/envoy-channel.js"
tailnet_suffix="ts.net"
tailnet_label="realtailnet"
tailnet_example="example-tailnet"

# The term list every case runs the guard under, unless a case sets its own: placeholder terms
# standing in for the deployment's own list (PRIVATE_NAME_TERMS, an Actions secret), on a pull
# request's event. One holds regex characters, to show a term is a fixed string, and one a "/",
# standing in for an owner/repository form. Whatever list or event the job's own environment
# holds is replaced, never read: the test job holds no secret.
planted="zephyr-orchard"
planted_dotted="copper.kettle"
planted_owner="lanternfish/harbor-map"
planted_terms=$(printf '%s\n' "$planted" "$planted_dotted" "$planted_owner")
export GITHUB_EVENT_NAME=pull_request
export PRIVATE_NAME_TERMS=$planted_terms

# names_planted <text>: true/false for check; whether text holds a planted term, or either half of
# the owner/repository one, in any case.
names_planted() {
  local term
  for term in "$planted" "$planted_dotted" "$planted_owner" "${planted_owner%%/*}" "${planted_owner#*/}"; do
    if grep -q -i -F -- "$term" <<< "$1"; then
      echo true
      return
    fi
  done
  echo false
}

# fixture <name>: a clean repository with one tracked file. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/docs"
  ln -s "$check_script" "$root/.github/scripts/check-private-names.sh"
  printf '# Notes\n\nNothing private here.\n' > "$root/docs/notes.md"
  git -C "$root" init -q
  git -C "$root" add docs/notes.md
  echo "$root"
}

# run_check <root> [path...]: sets `out` to the combined output and `status` to the exit code.
run_check() {
  local root=$1
  shift
  set +e
  out=$(cd "$root" && .github/scripts/check-private-names.sh "$@" 2>&1)
  status=$?
  set -e
}

# run_check_as <event> <terms> <root> [path...]: run_check under that GITHUB_EVENT_NAME and
# PRIVATE_NAME_TERMS, where `-` leaves the variable unset (a run outside Actions, a job that maps
# no secret).
run_check_as() {
  local event=$1 list=$2 root=$3
  shift 3
  set +e
  out=$(
    if [ "$event" = - ]; then unset GITHUB_EVENT_NAME; else export GITHUB_EVENT_NAME=$event; fi
    if [ "$list" = - ]; then unset PRIVATE_NAME_TERMS; else export PRIVATE_NAME_TERMS=$list; fi
    cd "$root" && .github/scripts/check-private-names.sh "$@" 2>&1
  )
  status=$?
  set -e
}

# argv_grep <file> <log>: writes a grep for a case's PATH that appends to <log> each pattern it is
# handed on its command line (an -e or --regexp value, or the first operand when it has neither -e
# nor -f), never a file's path, then runs the grep that PATH named when argv_grep ran, with the
# same arguments. A call shaped as check-private-names.sh's search runs grep (-o and -Z) also
# appends one line, `<search grep -f>` when its patterns came from a file and `<search grep -e>`
# otherwise, so a case sees that the search itself reached this grep: the masking's own greps fill
# the log too. Both paths are written into the script as it is made, before the case puts it on
# PATH, so it never finds itself as the grep it runs.
argv_grep() {
  {
    printf '#!/usr/bin/env bash\nREAL_GREP=%q\nARGV_LOG=%q\n' "$(command -v grep)" "$2"
    cat <<'GREP'
skip=no explicit=no operand=no only=no null=no from=-e
for a in "$@"; do
  if [ "$skip" = pattern ]; then printf '%s\n' "$a" >> "$ARGV_LOG"; skip=no; continue; fi
  if [ "$skip" = yes ]; then skip=no; continue; fi
  case $a in
    -e | --regexp) skip=pattern; explicit=yes ;;
    --regexp=*) printf '%s\n' "${a#--regexp=}" >> "$ARGV_LOG"; explicit=yes ;;
    -f | --file) skip=yes; explicit=yes; from=-f ;;
    --file=*) explicit=yes; from=-f ;;
    -m | -A | -B | -C) skip=yes; explicit=yes ;;
    -o) only=yes ;;
    -Z) null=yes ;;
    -*) ;;
    *) if [ "$explicit" = no ] && [ "$operand" = no ]; then printf '%s\n' "$a" >> "$ARGV_LOG"; fi; operand=yes ;;
  esac
done
if [ "$only" = yes ] && [ "$null" = yes ]; then printf '<search grep %s>\n' "$from" >> "$ARGV_LOG"; fi
exec "$REAL_GREP" "$@"
GREP
  } > "$1"
  chmod +x "$1"
}

# write_pr_text <title> <body> <commit message> [branch] [author email] [committer email]: writes
# the fixture check-pr-text.sh reads as CI runs it. A pull_request event payload carries the head
# the run checks (its base and head sha, and its one commit). The stand-in `gh` stands in for
# GitHub's GraphQL server for the script's two queries (`gh api graphql --input -`: PrText, and
# ThreadComments for a review thread's comments past its first 20). It answers only the fields a
# round asks for (each `@include` flag), in GraphQL's own shapes, so the real filters that feed the
# required check run for real, rather than filters the stub fakes; it refuses a read of another
# repository or pull request, or one not sent as --input -. The files it answers from keep the
# REST shapes the cases write. The pull request as it is now is `$work/pull` (the event's own
# unless a case rewrites it): title, body, head.ref, base.ref, created_at, changed_files,
# additions and deletions. The comparison of the base branch with the head the query names comes
# from `$work/compare` (one REST compare page a JSON object, as `gh api --paginate` prints them:
# each a GraphQL page, the first's total_commits the totalCount) when that head is the event's,
# and from `$work/pr-commits`, whatever head the pull request has by then, when it is not: a run
# that asked for the pull request's head rather than the event's would read the later commit the
# mid-run-push case appends there. Each commit's committedDate is its committer date as the case
# wrote it, turned to UTC, and its committer's date stays as written, offset and all, as GitHub's
# committer { date } does. The issue comments, reviews and review threads come a page of
# 100 at a time: page N of each is `$work/<list>.N` (`comments`, `reviews` or `review-comments`;
# each item in the REST shape, its id GraphQL's fullDatabaseId), the totalCount is the items of
# every page a case wrote, a next page follows while page N+1 exists, and page N's cursor is "N".
# An item of a `review-comments` page is a review thread holding that one comment or, when the
# item is a list, those comments: on the thread's first page as many as PrText asks for, then 100 a
# page. A page that is
# not a list is answered as it is. The commits on the base branch that touch the cutoff marker
# come with PrText too, 100 a page (`$work/cutoff-commits`, one list, else none, so the rule is
# inactive). `$work/cut.<list>` (commits, history, comments, reviews, threads, or thread for each
# thread's comments) holding N makes page N say no more follow while the totalCount still counts
# every page, as GitHub's comparison does at 1000 commits; `$work/base-gone` answers no base
# branch; `$work/compare-not-found` answers the comparison as GitHub answers one it cannot make,
# null with a NOT_FOUND error at its path, on which gh fails; `$work/graphql-fails` holding N
# fails the Nth GraphQL call as gh fails on an answer that carries an error; and
# `$work/graphql-rate-limited` holding `N SECONDS` answers the Nth GraphQL call as GraphQL answers
# a spent budget (HTTP 200, an error of type RATE_LIMITED, "API rate limit already exceeded", gh
# failing on it), the budget back SECONDS later, and with `N SECONDS type` the same error with
# words that name no rate limit (gh-read.sh reads neither: the rate_limit route decides). GET
# /rate_limit (`gh api rate_limit --jq <filter>`, as gh-read.sh reads it) answers that reset for
# the graphql budget at 0 until it passes, and both budgets whole otherwise. The REST routes it
# answers are the ones the script still reads, each from a file a case
# writes and otherwise as GitHub answers a repository with no such record: the compare route, for
# a comparison GraphQL answered short, matched whole for the event's own base...head and refused
# without --paginate (losing either paging token would pass on a list GitHub cut short at 250
# commits), from `$work/compare-rest` when a case wrote one, else `$work/compare`; and, for the
# issue-key rule, one release by its tag
# (`$work/release.<tag>`, else a 404 as gh prints it, and a file holding `HTTP 500` a server
# error), one commit's files (`$work/commit.<sha>`, else a refusal) and the pull request's files
# (`$work/pr-files`, one JSON list a page as `gh api --paginate` prints them, else a refusal; read
# without --paginate, also a refusal). Any other call is refused. A case rewrites those files, then
# calls run_pr_text, which runs the script and sets `out` and `status`; the stub logs each call it
# answers to `$work/calls`, a GraphQL call as `graphql <operation> <variables>`. `gh auth token`,
# which reads no GitHub, answers GH_TOKEN as gh does and is not logged: run_pr_text sets GH_TOKEN
# to $pr_text_token, the token check-pr-text.sh filters its output for. The repository's
# history and tags, which the rule reads with git, are a bare repository under `$work/remote`,
# which run_pr_text names as GITHUB_SERVER_URL.
mkdir -p "$work/bin"
cat > "$work/bin/gh" <<'GH'
#!/usr/bin/env bash
dir=$PR_TEXT_TEST_DIR
if [ "$1 $2" = "auth token" ]; then
  if [ -n "${GH_TOKEN:-}" ]; then printf '%s\n' "$GH_TOKEN"; exit 0; fi
  echo "no oauth token found for github.com" >&2
  exit 1
fi
if [ "$1 $2" = "api graphql" ]; then
  # gh-read.sh calls once more after a wait, so the script sends the request as a file; `-` (one
  # stdin, read once) is answered too, so a script from before that reads the same rows.
  [ "$3" = --input ] && [ $# -eq 4 ] || { echo "graphql read not sent as --input <file>: $*" >&2; exit 3; }
  if [ "$4" = - ]; then request=$(cat); else request=$(cat "$4"); fi
  operation=$(jq -r '.operationName' <<< "$request")
  variables=$(jq -c '.variables' <<< "$request")
  printf 'graphql %s %s\n' "$operation" "$variables" >> "$dir/calls"
  if [ -e "$dir/graphql-fails" ] && [ "$(grep -c '^graphql ' "$dir/calls")" -eq "$(< "$dir/graphql-fails")" ]; then
    says=$(cat "$dir/graphql-says" 2> /dev/null || echo "Something went wrong while executing your query.")
    jq -nc --arg message "$says" '{data: null, errors: [{message: $message}]}'
    echo "gh: $says" >&2
    exit 1
  fi
  if [ -e "$dir/graphql-rate-limited" ]; then
    read -r nth seconds words < "$dir/graphql-rate-limited"
    if [ "$(grep -c '^graphql ' "$dir/calls")" -eq "$nth" ]; then
      echo $(($(date +%s) + seconds)) > "$dir/rate-reset"
      message='API rate limit already exceeded for installation ID 1.'
      [ "${words:-}" != type ] || message='Something went wrong while executing your query.'
      jq -nc --arg message "$message" '{data: null, errors: [{type: "RATE_LIMITED", message: $message}]}'
      echo "gh: $message" >&2
      exit 1
    fi
  fi
  if [ -e "$dir/compare-not-found" ] && [ "$operation" = PrText ] && [ "$(jq -r '.commits' <<< "$variables")" = true ]; then
    echo '{"data": {"repository": {"pullRequest": {"baseRef": {"compare": null}}}}, "errors": [{"type": "NOT_FOUND", "path": ["repository", "pullRequest", "baseRef", "compare"], "message": "Could not resolve to a commit."}]}'
    echo 'gh: Could not resolve to a commit.' >&2
    exit 1
  fi
  if [ "$operation" = PrText ] && [ "$(jq -c '[.owner, .repo, .number]' <<< "$variables")" != '["example","repo",7]' ]; then
    echo '{"data": {"repository": null}, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to a PullRequest."}]}'
    echo 'gh: Could not resolve to a PullRequest.' >&2
    exit 1
  fi
  # The fixtures go to jq as files: a list of 1000 commits is longer than one argument may be.
  answer_dir=$(mktemp -d)
  trap 'rm -rf "$answer_dir"' EXIT
  # pages <list>: the pages a case wrote for that list, <list>.1, <list>.2, …, as one JSON list.
  pages() {
    local n=1
    while [ -e "$dir/$1.$n" ]; do cat "$dir/$1.$n"; n=$((n + 1)); done | jq -s '.'
  }
  for list in comments reviews review-comments; do pages "$list" > "$answer_dir/$list"; done
  cuts='{}'
  for list in commits history comments reviews threads thread; do
    if [ -e "$dir/cut.$list" ]; then
      cuts=$(jq -c --arg list "$list" --argjson n "$(< "$dir/cut.$list")" '.[$list] = $n' <<< "$cuts")
    fi
  done
  if [ "$(jq -r '.head' <<< "$variables")" = "$(jq -r '.pull_request.head.sha' "$dir/event.json")" ]; then
    jq -s '.' "$dir/compare" > "$answer_dir/compare"
  else
    jq -s '[{total_commits: (.[0] | length), commits: .[0]}]' "$dir/pr-commits" > "$answer_dir/compare"
  fi
  echo '[[]]' > "$answer_dir/history"
  if [ -e "$dir/cutoff-commits" ] && [ "$(jq -r '.marker' <<< "$variables")" = .github/issue-key-cutoff ]; then
    jq -c '[range(0; ([length, 1] | max); 100) as $i | .[$i:$i + 100]]' "$dir/cutoff-commits" > "$answer_dir/history"
  fi
  # The size of a review thread's first page of comments is the one PrText asks for, kept for the
  # ThreadComments reads that follow it.
  if [ "$operation" = PrText ]; then
    jq -r '[.query | capture("reviewThreads\\((.|\\n)*?comments\\(first: (?<n>[0-9]+)\\)")][0].n // "100"' \
      <<< "$request" > "$dir/thread-first"
  fi
  jq -n --argjson v "$variables" --arg operation "$operation" --slurpfile pull "$dir/pull" \
    --slurpfile compare "$answer_dir/compare" --slurpfile history "$answer_dir/history" \
    --slurpfile comments "$answer_dir/comments" --slurpfile reviews "$answer_dir/reviews" \
    --slurpfile threads "$answer_dir/review-comments" --argjson cut "$cuts" \
    --argjson first "$(cat "$dir/thread-first" 2> /dev/null || echo 100)" \
    --argjson base_gone "$([ -e "$dir/base-gone" ] && echo true || echo false)" '
    def item: {fullDatabaseId: (.id | if type == "number" then tostring else . end), body,
      authorAssociation: .author_association,
      author: (if .user == null then null else {__typename: .user.type, login: .user.login} end)};
    # utc: a date with an offset, as the same moment in UTC; any other value as it is.
    def utc: if type != "string" then . else
      [capture("^(?<t>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<z>Z|(?<s>[+-])(?<h>[0-9]{2}):(?<m>[0-9]{2}))$")][0] as $c
      | if $c == null or $c.z == "Z" then . else
          ($c.t + "Z" | fromdateiso8601) - (if $c.s == "+" then 1 else -1 end)
            * (($c.h | tonumber) * 3600 + ($c.m | tonumber) * 60) | todateiso8601 end end;
    def commit: {oid: .sha, message: .commit.message, author: (.commit.author | {name, email}),
      committedDate: (.commit.committer.date | utc), committer: (.commit.committer | {name, email, date}),
      parents: {totalCount: (.parents // [] | length)}};
    # page($pages; $after; $cut; $total; f): the page after cursor $after of $pages, each node
    # mapped by f.
    def page($pages; $after; $cut; $total; f):
      (if $after == null then 1 else ($after | tonumber) + 1 end) as $n
      | ($pages[$n - 1] // []) as $nodes
      | {totalCount: $total,
         pageInfo: {hasNextPage: ($n < ($pages | length) and ($cut == null or $n < $cut)), endCursor: "\($n)"},
         nodes: (if ($nodes | type) == "array" then [$nodes[] | f] else $nodes end)};
    def total($pages): [$pages[] | if type == "array" then length else 0 end] | add // 0;
    # thread_pages: the comments of a thread as GitHub pages them for the script: as many as PrText
    # asks for on its first page ($first), then 100 a page.
    def thread_pages: [.[0:$first]] + [range($first; length; 100) as $i | .[$i:$i + 100]];
    def thread_comments: if type == "array" then . else [.] end;
    $compare[0] as $compare | $history[0] as $history | $comments[0] as $comments
    | $reviews[0] as $reviews | $threads[0] as $threads
    | ($threads | to_entries | map((.key + 1) as $n | .value
      | if type == "array" then
          [to_entries[] | .key as $i | .value | thread_comments as $all
            | {id: "thread-\($n)-\($i)", comments: page($all | thread_pages; null; $cut.thread; $all | length; item)}]
        else . end)) as $thread_pages
    | if $operation == "ThreadComments" then
        ($v.thread | capture("^thread-(?<n>[0-9]+)-(?<i>[0-9]+)$")? // null) as $t
        | if $t == null then {data: {node: null}} else
            ($threads[($t.n | tonumber) - 1][$t.i | tonumber] | thread_comments) as $all
            | {data: {node: {comments: page($all | thread_pages; $v.after; $cut.thread; $all | length; item)}}}
          end
      else
        $pull[0] as $pull
        | {data: {repository: {pullRequest: (
            (if $v.full then {title: $pull.title, body: ($pull.body // ""), headRefName: $pull.head.ref,
                baseRefName: ($pull.base.ref // "main"), createdAt: $pull.created_at,
                changedFiles: $pull.changed_files, additions: $pull.additions, deletions: $pull.deletions}
              else {} end)
            + {baseRef: (if $base_gone then null else
                (if $v.commits then {compare:
                    {commits: page([$compare[] | .commits]; $v.commitsAfter; $cut.commits;
                      $compare[0].total_commits; commit)}} else {} end)
                + (if $v.history then {target: {history: page($history; $v.historyAfter; $cut.history;
                    total($history); {oid: .sha})}} else {} end)
              end)}
            + (if $v.comments then {comments: page($comments; $v.commentsAfter; $cut.comments; total($comments); item)}
              else {} end)
            + (if $v.reviews then {reviews: page($reviews; $v.reviewsAfter; $cut.reviews; total($reviews); item)}
              else {} end)
            + (if $v.threads then {reviewThreads: page($thread_pages; $v.threadsAfter; $cut.threads; total($threads); .)}
              else {} end)
          )}}}
      end'
  exit
fi
printf '%s\n' "$*" >> "$dir/calls"
# The compare route is the event's own base...head, matched whole: base-sha...event-head-sha as
# write_pr_text writes the event, or a real commit's id where a case sets one for git to read.
compare_route="api repos/example/repo/compare/$(jq -r '.pull_request.base.sha' "$dir/event.json")"
compare_route+="...$(jq -r '.pull_request.head.sha' "$dir/event.json")?per_page=100"
if [ "$1 $2" = "$compare_route" ]; then
  [[ " $* " == *" --paginate "* ]] || { echo "compare route read without --paginate: $*" >&2; exit 3; }
  if [ -e "$dir/compare-rest" ]; then cat "$dir/compare-rest"; else cat "$dir/compare"; fi
  exit 0
fi
case "$1 $2" in
  "api rate_limit")
    [ "$3" = --jq ] && [ $# -eq 4 ] || { echo "rate_limit read with no --jq filter: $*" >&2; exit 3; }
    now=$(date +%s) reset=$((now + 3600)) remaining=5000
    if [ -e "$dir/rate-reset" ] && [ "$(< "$dir/rate-reset")" -gt "$now" ]; then
      reset=$(< "$dir/rate-reset") remaining=0
    fi
    jq -n --argjson reset "$reset" --argjson remaining "$remaining" --argjson now "$now" \
      '{resources: {core: {limit: 15000, remaining: 15000, reset: ($now + 3600)},
        graphql: {limit: 10000, remaining: $remaining, reset: $reset}}}' | jq -r "$4" ;;
  "api repos/example/repo/releases/tags/"*)
    file="$dir/release.${2#repos/example/repo/releases/tags/}"
    if [ ! -e "$file" ]; then echo "gh: Not Found (HTTP 404)" >&2; exit 1; fi
    if grep -q -x 'HTTP 500' "$file"; then echo "gh: Server Error (HTTP 500)" >&2; exit 1; fi
    cat "$file" ;;
  "api repos/example/repo/pulls/7/files?per_page=100")
    [[ " $* " == *" --paginate "* ]] || { echo "files route read without --paginate: $*" >&2; exit 3; }
    if [ -e "$dir/pr-files" ]; then cat "$dir/pr-files"; else echo "no files answer" >&2; exit 3; fi ;;
  "api repos/example/repo/commits/"*"?per_page=100")
    sha=${2#repos/example/repo/commits/}
    sha=${sha%%\?*}
    if [ -e "$dir/commit.$sha" ]; then cat "$dir/commit.$sha"; else echo "no such commit: $sha" >&2; exit 3; fi ;;
  *) echo "unexpected gh call: $*" >&2; exit 3 ;;
esac
GH
chmod +x "$work/bin/gh"
# pr_commit <message> [author email] [committer email]: one commit as both routes list it.
pr_commit() {
  jq -nc --arg msg "$1" --arg author_email "${2:-dev@example.com}" \
    --arg committer_email "${3:-dev@example.com}" \
    '{commit: {message: $msg, author: {name: "Dev", email: $author_email},
               committer: {name: "Dev", email: $committer_email}}}'
}
write_pr_text() {
  local title=$1 body=$2 message=$3 branch=${4:-fix/keep-the-order} commit
  commit=$(pr_commit "$message" "${5:-}" "${6:-}")
  jq -nc --argjson commit "$commit" '{total_commits: 1, commits: [$commit]}' > "$work/compare"
  jq -nc --argjson commit "$commit" '[$commit]' > "$work/pr-commits"
  printf '%s' "$body" > "$work/pr-body"
  jq -n --arg title "$title" --arg branch "$branch" --rawfile body "$work/pr-body" \
    '{pull_request: {number: 7, title: $title, commits: 1,
                      head: {ref: $branch, sha: "event-head-sha"}, base: {sha: "base-sha"},
                      body: (if $body == "" then null else $body end)}}' \
    > "$work/event.json"
  jq '.pull_request' "$work/event.json" > "$work/pull"
  rm -f "$work"/comments.* "$work"/reviews.* "$work"/review-comments.* "$work/cutoff-commits" \
    "$work"/release.* "$work"/commit.* "$work/pr-files" "$work"/cut.* "$work/base-gone" \
    "$work/compare-not-found" "$work/graphql-fails" "$work/graphql-says" "$work/compare-rest" \
    "$work/graphql-rate-limited" "$work/rate-reset"
}
# The token every run holds as GH_TOKEN, which check-pr-text.sh filters its output for: no case's
# text holds it, so the filter changes no line but a case's own. A case sets pr_text_token to its
# own (the marker of a row whose stand-in answer holds the token), or to '' for a run with none.
pr_text_token=fixture-token-the-output-is-filtered-for
# The directory where check-pr-text.sh keeps gh's words (a job's RUNNER_TEMP), under $work.
mkdir -p "$work/runner-temp"
run_pr_text() {
  if [ $# -gt 0 ]; then write_pr_text "$@"; fi
  : > "$work/calls"
  set +e
  out=$(env -u GITHUB_TOKEN -u GH_ENTERPRISE_TOKEN -u GITHUB_ENTERPRISE_TOKEN GH_TOKEN="$pr_text_token" \
    RUNNER_TEMP="$work/runner-temp" GITHUB_EVENT_PATH="$work/event.json" GITHUB_REPOSITORY=example/repo \
    GITHUB_SERVER_URL="file://$work/remote" PR_TEXT_TEST_DIR="$work" PATH="$work/bin:$PATH" \
    "$pr_text_script" 2>&1)
  status=$?
  set -e
}
# run_pr_text_in_job <minutes>: run_pr_text as a job's step, under a job limit of that many
# minutes (JOB_TIMEOUT_MINUTES, no step limit) and a stand-in Runner.Worker (bash under the name
# GitHub's runner starts each job as), whose start gh-read.sh reads as the job's: the job starts
# when the case does. Also sets `seconds`, the run's wall time.
mkdir -p "$work/job"
ln -s "$(command -v bash)" "$work/job/Runner.Worker"
run_pr_text_in_job() {
  local started
  : > "$work/calls"
  started=$(date +%s)
  set +e
  # `; exit $?` keeps the worker from replacing itself with the script, which would take it out
  # of the process tree the helper reads.
  out=$(env -u STEP_TIMEOUT_MINUTES -u GITHUB_TOKEN -u GH_ENTERPRISE_TOKEN -u GITHUB_ENTERPRISE_TOKEN \
    GH_TOKEN="$pr_text_token" RUNNER_TEMP="$work/runner-temp" JOB_TIMEOUT_MINUTES="$1" \
    GITHUB_EVENT_PATH="$work/event.json" \
    GITHUB_REPOSITORY=example/repo GITHUB_SERVER_URL="file://$work/remote" PR_TEXT_TEST_DIR="$work" \
    PATH="$work/bin:$PATH" "$work/job/Runner.Worker" -c '"$@"; exit $?' worker "$pr_text_script" 2>&1)
  status=$?
  set -e
  seconds=$(($(date +%s) - started))
}
# app: who posted an item a case writes, unless the case says otherwise: an App installed on the
# repository, which posts as a Bot, with the association every Legion App's comments carry here.
app='{"user": {"login": "dev-app[bot]", "type": "Bot"}, "author_association": "CONTRIBUTOR"}'
# items <id> <body> [<id> <body>...]: one page of a comment route, each item its id, body and
# author (app) in the route's own JSON shape.
items() {
  local list='[]'
  while [ $# -gt 0 ]; do
    list=$(jq -c --argjson id "$1" --arg body "$2" --argjson author "$app" \
      '. + [{id: $id, body: $body} + $author]' <<< "$list")
    shift 2
  done
  printf '%s\n' "$list"
}
# by <login> <type> <association> <page> [index]: rewrites a page's items (only the one at index,
# when given) as posted by that author.
by() {
  local filter='map(author)'
  if [ $# -gt 4 ]; then filter=".[$5] |= author"; fi
  jq -c --arg login "$1" --arg type "$2" --arg association "$3" \
    "def author: .user = {login: \$login, type: \$type} | .author_association = \$association; $filter" \
    "$4" > "$4.next"
  mv "$4.next" "$4"
}

# clean_pr: a pull request whose title, body, branch and commit name nothing.
clean_pr() { write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"; }

# finish_self_log: the last case of a harness. It ends the tee and runs the guard over everything
# the harness printed, under the placeholder list.
finish_self_log() {
  local suite_status=0
  echo "case: the suite's own output never names a private thing"
  exec 1>&3 2>&4
  wait "$tee_pid" 2> /dev/null || true
  "$check_script" "$self_log" > /dev/null 2>&1 || suite_status=$?
  check "the suite's own PASS/FAIL log passes the guard, its placeholder terms included" \
    "$(is "$suite_status" 0)"
}
