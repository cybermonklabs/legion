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
# (.github/ci-notes/ci.md#why-the-router-runs-on-these-events-and-no-others says why it runs on no
# other); its tests are in check-private-names.test.sh, those of the term list in
# check-private-names-terms.test.sh, and those of the issue-key rule in
# check-private-names-keys.test.sh.
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
# anyway. A key or a link that the new commits also remove is carried over, not new: a line edited
# around a link it already held (whatever punctuation now follows it), a file moved with its keys,
# or a line one commit removes and a later one puts back, passes (THE DIFF READING below). A diff
# GitHub's REST answer does not hold whole (a file that adds lines with no patch, which GitHub
# leaves out for a large file, or a list short of the diff's totals) is read from git instead, and
# only a diff neither holds whole is UNKNOWN, and refused. The branch name is not read by this
# rule, neither on its own nor where a merge commit's message names the pull request's branch or
# its base branch, as a forward merge's does; any other message is read as written. The daemon
# names an issue's branch after its key today, and the change that names branches another way adds
# the branch to this rule.
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
  # THE DIFF READING. A diff (one new commit's, or the net diff) is staged as one entry for each
  # path it touches: <stage>/<i>.patch, its patch, with <i>.status (added, renamed, copied, removed
  # or modified), <i>.path and <i>.old (the path before a rename or a copy, else empty), and
  # <stage>/count. GitHub's REST answer is read first. Where it does not hold the whole diff, git
  # reads the same diff from the pull request's head, which it never truncates. Then carry.awk
  # (written below) writes each entry's added lines at their line numbers in the new file, every
  # other line empty, with each issue-key-shaped word or Dispatch link that a removed line also
  # holds blanked as <carried>: one removed occurrence carries one added one. The pool is every new
  # commit's removed lines together, across their files and commits, so a key moved, a line
  # re-worded around its link, or a line one commit removes and a later one puts back (as `jj split`
  # divides a move) is not new text, and a second copy of it is; the net diff, read for a merge, is
  # its own pool. A link's token ends before a sentence's trailing `.`, `,`, `;` or `:`, so changing
  # the punctuation after it adds nothing. carry.awk runs under LC_ALL=C, so it reads bytes and
  # folds the case of ASCII letters alone, as its words and its link scheme are defined: in a UTF-8
  # locale gawk's tolower folds a dotted capital I to an i, where rule 7's grep -i does not.
  link_scheme='dispatch'':''//'
  : > "$work/git-error"
  cat > "$work/carry.awk" <<'AWK'
# scan <line> <collect>: in collect mode, counts each key-shaped word and link of the line as one
# the diff removes; else the line with each one a removed occurrence carries blanked, and the
# count of those none carries in `uncarried`. A key-shaped word is an upper-case run, a "-" and
# digits, with an ASCII character that is no letter or digit (or the line's edge) on either side.
# Every other character, a non-ASCII letter or mark among them, counts as a letter, so this finds
# a word only where the sixth rule's locale-aware [^[:alnum:]] boundary finds a key too: a removed
# word that rule would not read as a key never carries an added one it would. The cost runs the
# safe way: a key beside non-ASCII punctuation (a curly quote, a no-break space) is no word here,
# so it is never carried and rule 6 reads it as new. A link is the scheme, in ASCII letters of any
# case, and what follows it up to a blank, a quote or a bracket, less any `.`, `,`, `;` or `:` that
# ends it, which stays in the line as written.
function scan(line, collect,    out, i, n, c, prev, j, k, tok, sl) {
  out = ""; n = length(line); i = 1; sl = length(scheme)
  while (i <= n) {
    c = substr(line, i, 1)
    if (tolower(substr(line, i, sl)) == scheme) {
      j = i + sl
      while (j <= n && index(stops, substr(line, j, 1)) == 0) j++
      while (j > i + sl && index(".,;:", substr(line, j - 1, 1)) > 0) j--
      tok = scheme substr(line, i + sl, j - i - sl)
      if (take(tok, collect)) out = out "<carried>"; else out = out substr(line, i, j - i)
      i = j
      continue
    }
    prev = (i == 1) ? "" : substr(line, i - 1, 1)
    if (index(upper, c) > 0 && (prev == "" || index(apart, prev) > 0)) {
      j = i
      while (j <= n && index(upper digits, substr(line, j, 1)) > 0) j++
      if (j <= n && substr(line, j, 1) == "-") {
        k = j + 1
        while (k <= n && index(digits, substr(line, k, 1)) > 0) k++
        if (k > j + 1 && (k > n || index(apart, substr(line, k, 1)) > 0)) {
          tok = substr(line, i, k - i)
          if (take(tok, collect)) out = out "<carried>"; else out = out tok
          i = k
          continue
        }
      }
    }
    out = out c
    i++
  }
  return out
}
function take(tok, collect) {
  if (collect) { carried[tok]++; return 0 }
  if (carried[tok] > 0) { carried[tok]--; return 1 }
  uncarried++
  return 0
}
BEGIN {
  stops = " \t\r\"<>()[]`" sprintf("%c", 39)
  upper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"; digits = "0123456789"
  # apart: the ASCII characters that are no letter or digit, the one set that bounds a word.
  apart = ""
  for (code = 1; code < 128; code++) {
    ch = sprintf("%c", code)
    if (index(upper tolower(upper) digits, ch) == 0) apart = apart ch
  }
  # mode=path: whether a renamed or copied path's words are all carried by its old path's.
  if (mode == "path") {
    scan(ENVIRON["OLD_PATH"], 1)
    scan(ENVIRON["NEW_PATH"], 0)
    print (uncarried ? "new" : "carried")
    exit
  }
  # Every stage the list names (`<stage><TAB><count>` a line), as one pool: first every removed
  # line of every stage, then each stage's added lines.
  while ((getline entry < stages) > 0) {
    split(entry, field, "\t")
    nstages++; stage_dir[nstages] = field[1]; stage_count[nstages] = field[2] + 0
  }
  close(stages)
  for (s = 1; s <= nstages; s++) {
    for (i = 1; i <= stage_count[s]; i++) {
      file = stage_dir[s] "/" i ".patch"; started = 0
      while ((getline line < file) > 0) {
        if (line ~ /^@@ /) { started = 1; continue }
        if (started && substr(line, 1, 1) == "-") scan(substr(line, 2), 1)
      }
      close(file)
    }
  }
  for (s = 1; s <= nstages; s++) {
    for (i = 1; i <= stage_count[s]; i++) {
      file = stage_dir[s] "/" i ".patch"; out = stage_dir[s] "/" i ".added"; started = 0; n = 0; written = 0
      printf "" > out
      while ((getline line < file) > 0) {
        # A typechange's patch holds two sections; the second's header lines, its `+++ b/<path>`
        # among them, are no added line. (The removed-line pass above needs no reset: the only
        # header line of a second section that starts with "-" is `--- /dev/null`, which holds no
        # word.)
        if (line ~ /^diff --git /) { started = 0; continue }
        if (line ~ /^@@ /) {
          split(line, parts, " "); split(parts[3], plus, ",")
          n = substr(plus[1], 2) + 0; started = 1
          continue
        }
        if (!started) continue
        first = substr(line, 1, 1)
        if (first == "+") {
          while (written < n - 1) { print "" > out; written++ }
          print scan(substr(line, 2), 0) > out
          written++; n++
        } else if (first == " ") n++
      }
      close(file); close(out)
    }
  }
}
AWK
  # stage_rest <pages> <stage> <label>: stages each file GitHub listed in <pages> (one JSON object
  # a page, each with its .files), in one read of the pages. Fails at the first file that adds
  # lines GitHub returned no patch for, or when the pages do not read as that, setting `reason` to
  # name it within <label>.
  stage_rest() {
    local pages=$1 stage=$2 label=$3 i=0 file_status additions has_patch name old patch
    if ! jq -s -j '[.[].files[]][] | .status, "\u0000", (.additions | tostring), "\u0000",
        (.patch != null | tostring), "\u0000", .filename, "\u0000", (.previous_filename // ""),
        "\u0000", ((.patch // "") | @base64), "\u0000"' "$pages" > "$stage/fields" 2> /dev/null; then
      reason="GitHub's files of $label do not read as a list of files"
      return 1
    fi
    while IFS= read -r -d '' file_status && IFS= read -r -d '' additions &&
      IFS= read -r -d '' has_patch && IFS= read -r -d '' name && IFS= read -r -d '' old &&
      IFS= read -r -d '' patch; do
      i=$((i + 1))
      if [ "$additions" -gt 0 ] && [ "$has_patch" != true ]; then
        reason="GitHub returned no patch for file $i of $label"
        return 1
      fi
      printf '%s' "$file_status" > "$stage/$i.status"
      printf '%s' "$name" > "$stage/$i.path"
      printf '%s' "$old" > "$stage/$i.old"
      base64 -d <<< "$patch" | tr -d '\000' > "$stage/$i.patch"
    done < "$stage/fields"
    echo "$i" > "$stage/count"
  }
  # pr_commit <sha>: whether $history holds that commit of the pull request, having fetched the
  # pull request's head (refs/pull/<number>/head) without blobs the first time, and the commit by
  # its id when that ref has moved past it. Each blob a later diff reads comes from the same
  # anonymous remote, the partial clone's own. A failure leaves git's message in
  # $work/git-error.
  pr_fetched=''
  pr_commit() {
    if [ -z "$pr_fetched" ]; then
      pr_fetched=yes
      git -C "$history" fetch -q --filter=blob:none --no-tags origin \
        "+refs/pull/$number/head:refs/pull/$number/head" 2> "$work/git-error" || true
    fi
    git -C "$history" cat-file -e "$1^{commit}" 2> /dev/null ||
      git -C "$history" fetch -q --filter=blob:none --no-tags origin "$1" 2> "$work/git-error"
  }
  # stage_git <from> <to> <stage>: stages the diff between two commits as git reads it, renames
  # found as GitHub finds them (-M): one whole diff, which fetches every blob at once, split at its
  # `diff --git` headers into sections and paired, in order, with the entries --name-status lists:
  # one section an entry, but two for a typechange (T, a change between a regular file, a symlink
  # and a submodule entry), whose patch is the old one removed and the new one added. A count that
  # differs, or a split or a copy that fails, is refused rather than paired wrong.
  stage_git() {
    local from=$1 to=$2 stage=$3 i=0 s=0 take code old name file_status sections
    rm -rf "$stage" && mkdir -p "$stage/sections"
    # A NUL byte is dropped, as the REST path drops it: git reads a file as text when its first
    # NUL comes past byte 8000, and the check's grep -I would skip a staged file that kept it.
    git -C "$history" diff --no-color --no-ext-diff -M -U0 "$from" "$to" 2> "$work/git-error" |
      tr -d '\000' > "$work/whole.patch" || return 1
    git -C "$history" diff -M -z --name-status "$from" "$to" > "$work/name-status" 2> "$work/git-error" ||
      return 1
    # Inside a hunk every line starts with " ", "+", "-" or "\", so a line that starts with
    # "diff --git " is a header.
    if ! sections=$(awk -v dir="$stage/sections" '/^diff --git / { if (out != "") close(out); i++; out = dir "/" i }
      i { print > out } END { print i + 0 }' "$work/whole.patch") || ! [[ $sections =~ ^[0-9]+$ ]]; then
      echo "git's diff could not be split into its file sections" > "$work/git-error"
      return 1
    fi
    while IFS= read -r -d '' code; do
      old=''
      case $code in R* | C*) IFS= read -r -d '' old ;; esac
      IFS= read -r -d '' name
      i=$((i + 1))
      case $code in
        A) file_status=added ;;
        R*) file_status=renamed ;;
        C*) file_status=copied ;;
        D) file_status=removed ;;
        *) file_status=modified ;;
      esac
      printf '%s' "$file_status" > "$stage/$i.status"
      printf '%s' "$name" > "$stage/$i.path"
      printf '%s' "$old" > "$stage/$i.old"
      take=1
      [ "$code" != T ] || take=2
      : > "$stage/$i.patch"
      while [ "$take" -gt 0 ]; do
        s=$((s + 1))
        if [ -e "$stage/sections/$s" ] && ! cat "$stage/sections/$s" >> "$stage/$i.patch"; then
          echo "section $s of git's diff could not be copied into entry $i's patch" > "$work/git-error"
          return 1
        fi
        take=$((take - 1))
      done
    done < "$work/name-status"
    if [ "$sections" -ne "$s" ]; then
      echo "git's diff holds $sections file sections and its name list $i entries, which take $s" \
        > "$work/git-error"
      return 1
    fi
    echo "$i" > "$stage/count"
  }
  # unread <reason>: neither GitHub nor git holds the whole diff, so the lines it adds cannot all
  # be read: UNKNOWN, and refused.
  unread() {
    echo "::error::UNKNOWN: $1, and git could not read it either ($(head -n 1 "$work/git-error")), so" \
      "the lines it adds cannot all be read; it is refused, not passed" >&2
    key_status=2
  }
  # carry_stages <stage>...: carry.awk over those stages as one pool (THE DIFF READING above), which
  # writes each entry's added lines, carried words blanked, to <stage>/<i>.added.
  carry_stages() {
    local stage
    for stage in "$@"; do printf '%s\t%s\n' "$stage" "$(< "$stage/count")"; done > "$work/stages"
    LC_ALL=C awk -v stages="$work/stages" -v scheme="$link_scheme" -f "$work/carry.awk"
  }
  # place_stage <stage> <part>: each staged entry's added lines under <part>/<path>: in $keyed when
  # the path is new text (an added file, or a renamed or copied one whose path holds a word its old
  # path does not carry), and in $lines_only otherwise. A removed file adds nothing; its removed
  # lines still carry.
  place_stage() {
    local stage=$1 part=$2 count i file_status name old dest
    count=$(< "$stage/count")
    for ((i = 1; i <= count; i++)); do
      file_status=$(< "$stage/$i.status")
      [ "$file_status" != removed ] || continue
      name=$(< "$stage/$i.path")
      old=$(< "$stage/$i.old")
      case $file_status in
        added) dest="$keyed/$part/$name" ;;
        renamed | copied)
          if [ "$(OLD_PATH=$old NEW_PATH=$name LC_ALL=C awk -v mode=path -v scheme="$link_scheme" -f "$work/carry.awk")" = carried ]; then
            dest="$lines_only/$part/$name"
          else
            dest="$keyed/$part/$name"
          fi
          ;;
        *) dest="$lines_only/$part/$name" ;;
      esac
      mkdir -p "$(dirname "$dest")"
      mv "$stage/$i.added" "$dest"
    done
  }
  jq -r --arg cutoff "$cutoff" '.commits[] | select(.commit.committer.date >= $cutoff) | .sha' \
    "$compare_json" > "$work/new-commits"
  merges=0
  # A merge commit's message is read with the pull request's own branch and its base branch each
  # written <head branch> and <base branch>: the daemon names an issue's branch after its key, and
  # a forward merge's message names both (the worker skill's `Merge <base> into <branch>`, GitHub's
  # "Update branch" `Merge branch 'main' into <branch>`, git's `Merge branch 'x' into <branch>`),
  # so a branch name, which this rule does not read, is not read through a merge's message either.
  # Any other commit's message is read as written: a branch named after a key does not hide that
  # key in a message a person wrote. A ref is replaced only where it stands whole: no letter,
  # digit, `-`, `_` or `.` before it (a `/` may be, as in `origin/<branch>`), and none of those or
  # a `/` after it, so a longer branch that holds the ref (legion/<KEY>-123 beside
  # legion/<KEY>-12) keeps its key, and rule 6 reads it. The boundary characters are captured and
  # put back, since jq builds differ on look-around; each replace runs twice, for two refs one
  # character apart, whose shared boundary one pass consumes.
  head_ref=$(jq -r '.head.ref // ""' "$live")
  commit_stages=()
  commit_parts=()
  while IFS= read -r sha; do
    short=${sha:0:12}
    parents=$(jq -r --arg sha "$sha" '.commits[] | select(.sha == $sha) | .parents // [] | length' "$compare_json")
    merge=false
    if [ "$parents" -gt 1 ]; then merge=true; fi
    jq -r --arg sha "$sha" --arg head "$head_ref" --arg base "$base_ref" --argjson merge "$merge" '
        def literal: gsub("(?<c>[\\[\\].*+?^${}()|\\\\/-])"; "\\\(.c)");
        def unbranch($ref; $as):
          if $ref == "" then .
          else ("(?<pre>^|[^A-Za-z0-9_.-])" + ($ref | literal) + "(?<post>[^A-Za-z0-9_./-]|$)") as $re
            | gsub($re; "\(.pre)\($as)\(.post)") | gsub($re; "\(.pre)\($as)\(.post)") end;
        .commits[] | select(.sha == $sha) | .commit.message
        | if $merge then unbranch($head; "<head branch>") | unbranch($base; "<base branch>") else . end' \
      "$compare_json" | tr -d '\000' > "$keyed/commit/$short"
    if [ "$merge" = true ]; then
      merges=$((merges + 1))
      continue
    fi
    stage="$work/stage-$short"
    mkdir "$stage"
    gh api "repos/$repository/commits/$sha?per_page=100" --paginate > "$work/commit-$short"
    files=$(jq -s '[.[].files[]] | length' "$work/commit-$short")
    listed=$(jq -s '[.[].files[] | .changes] | add // 0' "$work/commit-$short")
    total=$(jq -s '.[0].stats.total // -1' "$work/commit-$short")
    reason=''
    if [ "$listed" -ne "$total" ]; then
      reason="commit $short changes $total lines, and GitHub listed $listed of them in $files files"
    else
      stage_rest "$work/commit-$short" "$stage" "commit $short" || true
    fi
    if [ -n "$reason" ]; then
      if pr_commit "$sha" && stage_git "$sha^" "$sha" "$stage"; then
        echo "check-pr-text: $reason, so git reads that commit instead"
      else
        unread "$reason"
        continue
      fi
    fi
    commit_stages+=("$stage")
    commit_parts+=("added/$short")
  done < "$work/new-commits"
  if [ ${#commit_stages[@]} -gt 0 ]; then
    carry_stages "${commit_stages[@]}"
    for i in "${!commit_stages[@]}"; do place_stage "${commit_stages[i]}" "${commit_parts[i]}"; done
  fi
  # The net diff, once, for the merges above: the files route lists 100 a page, up to GitHub's
  # limit of 3000 files, and the pull request route totals them. A list short of those totals, or
  # a file with no patch, is read from git instead: the diff from where the head leaves the base
  # branch to the head. Its hits are `files:<path>:<line>`.
  if [ "$merges" -gt 0 ]; then
    stage="$work/stage-net"
    mkdir "$stage"
    gh api "repos/$repository/pulls/$number/files?per_page=100" --paginate > "$work/pr-files-pages"
    jq -s '{files: [.[][]]}' "$work/pr-files-pages" > "$work/pr-files"
    files=$(jq '.files | length' "$work/pr-files")
    listed=$(jq '[.files[] | .changes] | add // 0' "$work/pr-files")
    want_files=$(jq '.changed_files // -1' "$live")
    want_lines=$(jq '(.additions // -1) + (.deletions // 0)' "$live")
    reason=''
    if [ "$files" -ne "$want_files" ] || [ "$listed" -ne "$want_lines" ]; then
      reason="#$number holds $merges merge commit(s), so its net diff is read, and GitHub listed $files"
      reason+=" of its $want_files files and $listed of its $want_lines changed lines"
    else
      stage_rest "$work/pr-files" "$stage" "#$number's net diff" || true
    fi
    if [ -n "$reason" ]; then
      if pr_commit "$head" &&
        fork=$(git -C "$history" merge-base "refs/heads/$base_ref" "$head" 2> "$work/git-error") &&
        stage_git "$fork" "$head" "$stage"; then
        echo "check-pr-text: $reason, so git reads the net diff instead"
      else
        unread "$reason"
        stage=''
      fi
    fi
    if [ -n "$stage" ]; then
      carry_stages "$stage"
      place_stage "$stage" net
    fi
  fi
  # Two runs of rules 6 and 7: one over the title, body, items, commit messages and the files the
  # new commits (or the net diff) add, or rename to a path with a word its old path lacks, whose
  # paths are new text too; one over the lines they add to files they only change, whose paths are
  # not.
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
