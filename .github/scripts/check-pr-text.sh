#!/usr/bin/env bash
# Runs check-private-names.sh over a pull request's title, body, branch name and commits (each
# commit's message, author name and email, and committer name and email), and over every issue
# comment, review and review comment on it that a trusted author posted (see "Who counts" below).
# The first four are as public as the tree: with this
# repository's squash settings (the pull request title, then a blank message), the title becomes
# main's commit subject; the release workflows' generated notes list merged titles; the branch
# name keeps showing on the pull request's page after the branch is deleted; and each commit's
# message, author and committer stay public on the pull request's page. A comment, review or review
# comment is public on the pull request's page from the moment it is posted. A hit is reported as
# `title:<line>`, `body:<line>`, `branch:<line>`, `commits:<line>`, `comment:<id>:<line>`,
# `review:<id>:<line>` or `review-comment:<id>:<line>` (the id GitHub gives that issue comment,
# review or review comment), never its text.
#
# Then it holds the pull request's new text to check-private-names.sh's rules 6 and 7 (an issue key
# of the deployment's own projects, and a Dispatch link), from a cutoff it derives at each run: the
# section "THE ISSUE-KEY RULE" below says how, and which text. A hit there is reported as
# `title:<line>`, `body:<line>`, `<kind>:<id>:<line>`, `commit:<sha>:<line>` for a
# commit's message, `commit:<sha>:<path>:<line>` for a line a commit adds, or
# `files:<path>:<line>` for a line of the pull request's net diff, which is read in place of a
# merge commit's own diff, each path masked where it holds a name.
#
# It also requires the title to be a conventional commit subject (`type(scope): summary`, the
# scope and a `!` optional): squash merges use the title as main's commit subject, and the release
# workflows version from conventional subjects; this repository's squash merges write an empty
# body, so the title is the only text they read. That failure prints no title text
# either.
#
# It reads the event payload GitHub writes to GITHUB_EVENT_PATH, never the step's environment,
# which the runner prints in the job log. A pull_request payload carries
# the pull request: its number, and the base and head sha the run checks, which is the head the
# event names rather than whatever head the pull request has by the time a route is read, since a
# push can land while the job runs and gets a run of its own. A payload without one is refused.
# The rest it reads with gh (GH_TOKEN, GITHUB_REPOSITORY). The title, body and branch name come
# from the pull request route as the pull request has them now, not from the payload: ci.yaml's
# `pr-title` job checks at a push and can finish long after a later edit's pr-title.yaml run, and
# both carry pr-title on the head, so a check of the push's own text would report a pass over
# the edit's refusal. The commits come from the compare route between the event's base and head,
# which pages through every commit and reports their total, so a list that comes back shorter
# than that total is refused loudly here instead of silently scanning part of it; and the issue
# comments, reviews and review comments from their list routes, which report no total, so each
# is read 100 at a time until a page comes back short. There is no total to cross-check those
# lists against, and none is
# needed: a list that changes while a run reads it is read again by the next push or edit run and
# by ci.yaml's `pr-title` job's next run (ci.yaml says when it runs), and those runs'
# checks are the ones the merge reads. A NUL byte in any of them, which would make `grep -I`
# treat the file as binary and skip it, is stripped first.
#
# Usage: GITHUB_EVENT_PATH=<payload> GITHUB_REPOSITORY=<owner>/<repo> .github/scripts/check-pr-text.sh
# CI runs it in ci.yaml's `pr-title` job at every push to a pull request that is not a draft, when
# a draft is marked ready, and on the merge queue's group, where that job finds the queued pull
# request with find-pr-payload.sh; that job's own check run carries the verdict as pr-title on
# each. It runs in pr-title.yaml when a pull request is edited, the event ci.yaml does not run on
# (that file's header says why it runs on no other); its tests are in check-private-names.test.sh,
# and those of the term list and the issue-key rule in check-private-names-terms.test.sh.
# check-private-names.sh takes PRIVATE_NAME_TERMS, the deployment's own list, and
# GITHUB_EVENT_NAME from this script's environment, so both of those steps map that secret: on
# the merge queue's group the list is required (check-private-names.sh's header), and this script
# then exits 2 without it.
set -euo pipefail

event=${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH must name the pull request event payload}
repository=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must name the repository as owner/repo}
script_dir=$(cd "$(dirname "$0")" && pwd)
text=$(mktemp -d)
work=$(mktemp -d)
trap 'rm -rf "$text" "$work"' EXIT

pull="$work/pull.json"
if ! jq -e '.pull_request' "$event" > "$pull"; then
  echo "::error::the event payload carries no pull request" >&2
  exit 1
fi
number=$(jq -er '.number' "$pull")
base=$(jq -er '.base.sha' "$pull")
head=$(jq -er '.head.sha' "$pull")
live="$work/live.json"
gh api "repos/$repository/pulls/$number" > "$live"
if ! jq -e '(.title | type == "string") and (.head.ref | type == "string")' "$live" > /dev/null 2>&1; then
  echo "::error::the pull request route answered no title and head branch for #$number" >&2
  exit 1
fi
jq -r '.title' "$live" | tr -d '\000' > "$text/title"
jq -r '.body // ""' "$live" | tr -d '\000' > "$text/body"
jq -r '.head.ref' "$live" | tr -d '\000' > "$text/branch"
title_status=0
if ! grep -qE '^[a-z]+(\([^)]+\))?!?: .+' "$text/title"; then
  echo "::error::PR title must be a conventional commit subject (type(scope): summary). Squash merges use the PR title as the commit subject, and the release workflows version from conventional subjects — this repository's squash merges write an empty body, so the title is the only text they read."
  title_status=1
fi

# GitHub limits this route to 250 commits for a call with no paging parameter: `?per_page=100` is
# that parameter, and asks for the largest page GitHub serves; `--paginate` then reads every page
# it creates, printing one JSON object per page, each repeating total_commits.
compare_json="$work/compare.json"
gh api "repos/$repository/compare/$base...$head?per_page=100" --paginate > "$compare_json"
total_commits=$(jq -s '.[0].total_commits // empty' "$compare_json")
if ! [[ $total_commits =~ ^[0-9]+$ ]]; then
  echo "::error::the compare route's answer for $base...$head names no whole-number" \
    "total_commits, so the commit list cannot be checked against it" >&2
  exit 1
fi
listed_commits=$(jq -s '[.[].commits[]] | length' "$compare_json")
if [ "$listed_commits" -ne "$total_commits" ]; then
  echo "::error::the compare route reports $total_commits commits from the pull request's base to" \
    "$head but listed $listed_commits, so not every commit can be checked" >&2
  exit 1
fi
jq -r '.commits[] | .commit.message, .commit.author.name, .commit.author.email, .commit.committer.name, .commit.committer.email' \
  "$compare_json" | tr -d '\000' > "$text/commits"

# Who counts: an issue comment, review or review comment is scanned only when an App installed on
# this repository posted it (it posts as a Bot, which only such an App or github-actions can do;
# Legion's role Apps carry the CONTRIBUTOR association here, so association alone would drop them)
# or its author is the repository's owner, a member of its organization or a collaborator. On a
# public repository anyone signed in can comment or review, and every run reports the required
# check; a stranger's text skipped this way can neither fail the check nor clear one that failed.
# Each skipped item is printed as `skipped <kind>:<id>: its author (<association>) is not
# trusted`, with no text and no login: a stranger chooses the login, and a login can spell a
# private name, which nothing here would mask.
trusted='.user.type == "Bot" or (.author_association // "" | IN("OWNER", "MEMBER", "COLLABORATOR"))'

# read_list <kind> <route>: writes the body of every trusted item the list route answers to
# $text/<kind>/<id>, reading 100 items a page until a page holds fewer. An answer that is not a
# list, or an item whose id is not a whole number, is refused rather than scanned in part.
read_list() {
  local kind=$1 route=$2 page=1 id trust association body
  mkdir "$text/$kind"
  while :; do
    gh api "repos/$repository/$route?per_page=100&page=$page" > "$work/answer"
    if ! jq -e 'type == "array"' "$work/answer" > /dev/null 2>&1; then
      echo "::error::page $page of the $route route is not a list, so its ${kind}s cannot be" \
        "checked" >&2
      exit 1
    fi
    jq -r ".[] | [(.id | tojson), ($trusted | tostring),
        \"\(.author_association // \"<none>\")\",
        (.body // \"\" | explode | map(select(. != 0)) | implode | @base64)] | @tsv" \
      "$work/answer" > "$work/items"
    while IFS=$'\t' read -r id trust association body; do
      if ! [[ $id =~ ^[0-9]+$ ]]; then
        echo "::error::page $page of the $route route lists a $kind whose id is not a whole" \
          "number, so it cannot be reported" >&2
        exit 1
      fi
      if [ "$trust" = true ]; then
        base64 -d <<< "$body" > "$text/$kind/$id"
      else
        echo "skipped $kind:$id: its author ($association) is not trusted"
      fi
    done < "$work/items"
    if [ "$(wc -l < "$work/items")" -lt 100 ]; then break; fi
    page=$((page + 1))
  done
}
# Each kind a hit is reported under, beside its route; the reads, the scan and the report below
# all take the kinds from here.
lists=(
  comment "issues/$number/comments"
  review "pulls/$number/reviews"
  review-comment "pulls/$number/comments"
)
kinds=()
for ((i = 0; i < ${#lists[@]}; i += 2)); do
  kinds+=("${lists[i]}")
  read_list "${lists[i]}" "${lists[i + 1]}"
done

# The guard scans these by their absolute names; the log names each by the part after the
# temporary directory, and an item as <kind>:<id>.
kind_pattern="($(IFS='|'; echo "${kinds[*]}"))"
status=0
"$script_dir/check-private-names.sh" "$text/title" "$text/body" "$text/branch" "$text/commits" \
  "${kinds[@]/#/$text/}" 2>&1 | sed -E -e "s|$text/||g" -e "s#(^|[:=])$kind_pattern/([0-9]+)#\\1\\2:\\3#g" \
  > "$work/report" || status=$?
cat "$work/report"
if [ "$status" -ne 0 ] && grep -q -E "(^|[:=])$kind_pattern:[0-9]+:" "$work/report"; then
  echo "  - a comment, review or review comment reported above is public on the pull request's page"
  echo "    now: delete each comment and review comment, since an edit keeps the old text in its"
  echo "    edit history, which anyone who can read the repository can open; a submitted review"
  echo "    cannot be deleted, so edit its body and delete the old revision from that history."
  echo "    Then re-run the newest failed run of each check that reported it on the current head:"
  echo "    lint, which also posts the push's pr-title, and pr-title.yaml's run once for each other"
  echo "    event that is red (\`gh run rerun <run-id> --failed\`)."
  echo "    Never re-run an older run: it checks the commits of the head its own event named, and"
  echo "    its check lands on that head, not the current one."
fi
# THE ISSUE-KEY RULE (check-private-names.sh's rules 6 and 7: an issue key of the deployment's own
# projects, and a Dispatch link), over new text only. Its cutoff is derived, never typed: the
# commit that added .github/issue-key-cutoff to the base branch is the one that told every worker
# to keep keys and links out of this repository's text (skills/legion-worker/references/
# pr-body.md), and the cutoff is the publish time of the earliest legion-v* release whose commit
# contains it, found at a cost that does not grow with the releases (below). No such commit, or no
# such release yet, and the rule is inactive; a history or a release GitHub does not answer is
# UNKNOWN, and refused. Once active, a
# pull request opened before the cutoff is not read for it; one opened after it is read in its
# title and body, every trusted comment, review and review comment (all posted after it), and
# each commit whose committer date is at or after the cutoff: its message, and the lines and paths
# it adds. A merge commit's own diff is against its first parent, which for a merge of the base
# branch forward is everything the base branch changed since the branch left it, other pull
# requests' text among it: so a merge's message is read, its own diff is not, and the pull
# request's net diff (the pulls files route, the lines the pull request adds to its base) is read
# once instead, which holds any line typed while resolving a conflict. The net diff is the whole
# pull request's, so it also holds the lines of its commits dated before the cutoff: a pull request
# opened after the cutoff that holds a forward merge is read whole, those older lines included.
# That exception errs toward reading more, and each of those lines lands on main as new text
# anyway. A commit or a net diff whose patch GitHub does not return whole is UNKNOWN, and
# refused. The branch name is not read by this rule: the daemon names an issue's branch after its
# key today, and the change that names branches another way adds the branch to this rule.
key_status=0
cutoff_file=.github/issue-key-cutoff
base_ref=$(jq -r '.base.ref // "main"' "$live")
gh api "repos/$repository/commits?path=$cutoff_file&sha=$base_ref&per_page=100" --paginate \
  > "$work/cutoff-commits"
skill_commit=$(jq -s -r '[.[][]] | last | .sha // empty' "$work/cutoff-commits")
cutoff=''
if [ -z "$skill_commit" ]; then
  echo "check-pr-text: the issue-key rule is inactive: no commit on $base_ref adds $cutoff_file"
else
  # The earliest release that contains the marker commit is the lowest legion-v version that
  # contains it and has a published release: release.yaml computes each legion version from the
  # highest existing tag (release-push.sh version) and runs one release at a time (its concurrency
  # group, never cancelled), so a higher version is tagged and published after every lower one.
  # That order is checked, not trusted: the next version's release, when it has one, must be
  # published no earlier, or the cutoff is refused as UNKNOWN. Which tags contain the marker is
  # git's answer, from a clone of the base branch's commits alone (--filter=tree:0: no tree, no
  # file), read anonymously, as the repository is public; the legion-v tags are lightweight tags on
  # those commits, so their refs come from one ref listing. A tag whose commit the clone lacks is
  # fetched by update-ref itself, that one commit from the partial clone's own remote, and the tags
  # are fetched only when that fails. So each run asks GitHub's API for a release or two, however
  # many releases there are; reading every release since the marker grew with each one.
  history="$work/history.git"
  remote="${GITHUB_SERVER_URL:-https://github.com}/$repository"
  # unknown <message...>: the cutoff cannot be derived, so the rule refuses rather than passes.
  unknown() {
    echo "::error::UNKNOWN: $*, so the issue-key rule's cutoff is unknown; it is refused, not passed" >&2
    key_status=2
  }
  if ! git clone -q --bare --filter=tree:0 --single-branch --no-tags --branch "$base_ref" "$remote" \
    "$history" 2> "$work/git-error"; then
    unknown "git could not read $base_ref's history ($(head -n 1 "$work/git-error"))"
  elif ! git -C "$history" cat-file -e "$skill_commit^{commit}" 2> /dev/null; then
    unknown "$base_ref's history does not hold the commit that added $cutoff_file"
  elif ! git -C "$history" ls-remote origin 'refs/tags/legion-v*' > "$work/tag-refs" 2> "$work/git-error"; then
    unknown "git could not list the legion-v tags ($(head -n 1 "$work/git-error"))"
  elif [ ! -s "$work/tag-refs" ]; then
    # The repository has hundreds of legion-v tags, and release-push.sh starts no release line
    # without one, so an empty listing is a read that went wrong (a renamed prefix, an empty
    # answer), never a repository with no release yet.
    unknown "the remote lists no legion-v tag"
  elif ! awk '$2 !~ /\^\{\}$/ { print "create " $2 " " $1 }' "$work/tag-refs" |
      git -C "$history" update-ref --stdin 2> /dev/null &&
    ! git -C "$history" fetch -q --no-tags origin '+refs/tags/legion-v*:refs/tags/legion-v*' 2> "$work/git-error"; then
    unknown "git could not fetch the legion-v tags ($(head -n 1 "$work/git-error"))"
  else
    git -C "$history" tag -l 'legion-v*' --contains "$skill_commit" --sort=v:refname |
      awk '/^legion-v[0-9]+\.[0-9]+\.[0-9]+$/' > "$work/containing"
    first_tag='' first_published=''
    while IFS= read -r tag; do
      # A tag can exist before its release does, and a draft is no release: either is skipped.
      if ! gh api "repos/$repository/releases/tags/$tag" > "$work/release" 2> "$work/gh-error"; then
        if grep -q -F '(HTTP 404)' "$work/gh-error"; then continue; fi
        unknown "GitHub did not answer for the release of $tag ($(head -n 1 "$work/gh-error"))"
        break
      fi
      published=$(jq -r 'select(.draft == false) | .published_at // empty' "$work/release")
      [ -n "$published" ] || continue
      if [ -z "$first_tag" ]; then
        first_tag=$tag first_published=$published
        continue
      fi
      if [[ $published < $first_published ]]; then
        unknown "$tag was published at $published, before $first_tag at $first_published, so the" \
          "legion-v versions are not in publish order and the lowest one is not the earliest"
      fi
      break
    done < "$work/containing"
    if [ "$key_status" -eq 0 ] && [ -n "$first_tag" ]; then
      cutoff=$first_published
      echo "check-pr-text: the issue-key rule is active since $first_tag, published $first_published"
    elif [ "$key_status" -eq 0 ]; then
      echo "check-pr-text: the issue-key rule is inactive: no legion-v* release contains the commit" \
        "that added $cutoff_file yet"
    fi
  fi
fi
opened=$(jq -r '.created_at // empty' "$live")
if [ -n "$cutoff" ] && [ -n "$opened" ] && [[ $opened < $cutoff ]]; then
  echo "check-pr-text: #$number was opened at $opened, before the issue-key rule's cutoff, so it" \
    "is not read for issue keys"
elif [ -n "$cutoff" ]; then
  keyed=$(mktemp -d)
  lines_only=$(mktemp -d)
  trap 'rm -rf "$text" "$work" "$keyed" "$lines_only"' EXIT
  cp "$text/title" "$text/body" "$keyed/"
  for kind in "${kinds[@]}"; do cp -r "$text/$kind" "$keyed/$kind"; done
  mkdir "$keyed/commit" "$keyed/added"
  # added_lines <patch file>: the patch's added lines at their line numbers in the new file, every
  # other line empty, so a hit names the line the file holds it at.
  added_lines() {
    awk '/^@@ / { split($3, plus, ","); n = substr(plus[1], 2) + 0; next }
         /^\+/ { while (out < n - 1) { print ""; out++ } print substr($0, 2); out++; n++; next }
         /^ / { n++; next }' "$1"
  }
  # added_files <pages> <label> <part>: the added lines of every file GitHub listed in <pages> (one
  # JSON object a page, each with its .files), each written by added_lines to <part>/<path>: under
  # $keyed when the file is new (added, renamed or copied), its path new text too, and under
  # $lines_only otherwise. A file that adds lines GitHub returned no patch for is UNKNOWN, named
  # by <label> and its place in the list, and refused.
  added_files() {
    local pages=$1 label=$2 part=$3 files i file_status additions has_patch name dest
    files=$(jq -s '[.[].files[]] | length' "$pages")
    for ((i = 0; i < files; i++)); do
      jq -s -r --argjson i "$i" '[.[].files[]][$i] | "\(.status)\t\(.additions)\t\(.patch != null)"' \
        "$pages" > "$work/file"
      IFS=$'\t' read -r file_status additions has_patch < "$work/file"
      if [ "$additions" -gt 0 ] && [ "$has_patch" != true ]; then
        echo "::error::UNKNOWN: GitHub returned no patch for file $((i + 1)) of $label, so the lines" \
          "it adds cannot be read; it is refused, not passed" >&2
        key_status=2
        continue
      fi
      jq -s -r --argjson i "$i" '[.[].files[]][$i] | .patch // ""' "$pages" | tr -d '\000' > "$work/patch"
      jq -s -r --argjson i "$i" '[.[].files[]][$i] | .filename' "$pages" > "$work/name"
      name=$(< "$work/name")
      case $file_status in
        added | renamed | copied) dest="$keyed/$part/$name" ;;
        *) dest="$lines_only/$part/$name" ;;
      esac
      mkdir -p "$(dirname "$dest")"
      added_lines "$work/patch" > "$dest"
    done
  }
  jq -r --arg cutoff "$cutoff" '.commits[] | select(.commit.committer.date >= $cutoff) | .sha' \
    "$compare_json" > "$work/new-commits"
  merges=0
  while IFS= read -r sha; do
    short=${sha:0:12}
    jq -r --arg sha "$sha" '.commits[] | select(.sha == $sha) | .commit.message' "$compare_json" |
      tr -d '\000' > "$keyed/commit/$short"
    parents=$(jq -r --arg sha "$sha" '.commits[] | select(.sha == $sha) | .parents // [] | length' "$compare_json")
    if [ "$parents" -gt 1 ]; then
      merges=$((merges + 1))
      continue
    fi
    gh api "repos/$repository/commits/$sha?per_page=100" --paginate > "$work/commit-$short"
    files=$(jq -s '[.[].files[]] | length' "$work/commit-$short")
    listed=$(jq -s '[.[].files[] | .changes] | add // 0' "$work/commit-$short")
    total=$(jq -s '.[0].stats.total // -1' "$work/commit-$short")
    if [ "$listed" -ne "$total" ]; then
      echo "::error::UNKNOWN: commit $short changes $total lines, and GitHub listed $listed of them" \
        "in $files files, so the lines it adds cannot all be read; it is refused, not passed" >&2
      key_status=2
      continue
    fi
    added_files "$work/commit-$short" "commit $short" "added/$short"
  done < "$work/new-commits"
  # The net diff, once, for the merges above: the files route lists 100 a page, up to GitHub's
  # limit of 3000 files, and the pull request route totals them, so a list that comes back short
  # of those totals is refused rather than read in part. Its hits are `files:<path>:<line>`.
  if [ "$merges" -gt 0 ]; then
    gh api "repos/$repository/pulls/$number/files?per_page=100" --paginate > "$work/pr-files-pages"
    jq -s '{files: [.[][]]}' "$work/pr-files-pages" > "$work/pr-files"
    files=$(jq '.files | length' "$work/pr-files")
    listed=$(jq '[.files[] | .changes] | add // 0' "$work/pr-files")
    want_files=$(jq '.changed_files // -1' "$live")
    want_lines=$(jq '(.additions // -1) + (.deletions // 0)' "$live")
    if [ "$files" -ne "$want_files" ] || [ "$listed" -ne "$want_lines" ]; then
      echo "::error::UNKNOWN: #$number holds $merges merge commit(s), so its net diff is read, and" \
        "GitHub listed $files of its $want_files files and $listed of its $want_lines changed lines;" \
        "the lines it adds cannot all be read, so it is refused, not passed" >&2
      key_status=2
    else
      added_files "$work/pr-files" "#$number's net diff" net
    fi
  fi
  # Two runs of rules 6 and 7: one over the title, body, branch, items, commit messages and the
  # files the new commits (or the net diff) add or rename, whose paths are new text too; one over
  # the lines they add to files they only change, whose paths are not.
  for run in keyed lines_only; do
    dir=${!run}
    [ "$run" = lines_only ] && [ -z "$(ls -A "$dir")" ] && continue
    if [ "$run" = keyed ]; then paths_flag=yes; else paths_flag=no; fi
    run_status=0
    PRIVATE_NAME_RULES=keys PRIVATE_NAME_CHECK_PATHS=$paths_flag "$script_dir/check-private-names.sh" "$dir" 2>&1 |
      sed -E -e "s|$dir/||g" -e "s#(^|[:=])$kind_pattern/([0-9]+)#\\1\\2:\\3#g" \
        -e "s#(^|[:=])commit/([0-9A-Za-z_-]+)#\\1commit:\\2#g" -e "s#(^|[:=])added/([0-9A-Za-z_-]+)/#\\1commit:\\2:#g" \
        -e "s#(^|[:=])net/#\\1files:#g" \
      || run_status=$?
    if [ "$run_status" -gt "$key_status" ]; then key_status=$run_status; fi
  done
fi

if [ "$key_status" -gt "$status" ]; then status=$key_status; fi
if [ "$status" -eq 0 ] && [ "$title_status" -ne 0 ]; then
  exit 1
fi
exit "$status"
