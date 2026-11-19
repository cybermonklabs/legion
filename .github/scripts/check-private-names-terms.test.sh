#!/usr/bin/env bash
# Tests for the fifth rule of `check-private-names.sh`, the deployment's own term list
# (PRIVATE_NAME_TERMS), and for the masking and event handling that came with it: a planted term
# refused in a line and in a path, in any case and by grep's own case folding, its absence passing;
# a path, a named target and another command's message masked so the log never prints a term or a
# fragment of one, overlapping terms and a traced run included; several files a mask prints alike
# counted one by one; the allowed hosts of the committed rules never excusing a term; an unset
# list warning on a pull request's event and failing on the merge queue's, a push's, a dispatched
# run's and a review's; and a pull request's text through check-pr-text.sh, a stranger's login
# never printed.
#
# The terms are placeholders this file and check-private-names-test-lib.sh set, never a real
# term, and every case is named by a neutral label, since this script's own output is the CI log;
# the last case runs the guard over that captured log.
#
# Run from anywhere: .github/scripts/check-private-names-terms.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit
# No tracing: a traced run would print every term the cases below expand.
set +x

# shellcheck source=.github/scripts/check-private-names-test-lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-private-names-test-lib.sh"

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

echo "case: a term of the deployment's own list is refused, in any case, and the log names only its place"
root=$(fixture term)
printf 'line one\nSee the %s runbook.\n' "${planted^^}" > "$root/docs/notes.md"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "annotates the file and line" \
  "$(contains "$out" "^::error file=docs/notes.md,line=2::docs/notes.md:2: names a term of the deployment's own list (PRIVATE_NAME_TERMS)\$")"
check "counts the finding" "$(contains "$out" '^check-private-names: 1 finding(s)')"
check "does not print the term" "$(is "$(names_planted "$out")" false)"

echo "case: a run with the list says how many terms it read, and never a term"
# Each job that maps the secret runs the check once, so its log holds this line once: the proof,
# on a pull request's own run, that the job read the list. An unset list prints the warning
# instead.
terms_read='^check-private-names: PRIVATE_NAME_TERMS: 3 terms read$'
root=$(fixture term-count-line)
run_check "$root"
check "a clean scan names the count of the three placeholder terms" "$(contains "$out" "$terms_read")"
check "once" "$(is "$(grep -c 'terms read' <<< "$out")" 1)"
printf 'the %s notes\n' "$planted" > "$root/docs/notes.md"
run_check "$root"
check "a scan with a hit names the count too" "$(contains "$out" "$terms_read")"
check "prints no spelling of a term" "$(is "$(names_planted "$out")" false)"
run_check_as pull_request "$planted" "$root"
check "one term reads as one term" "$(contains "$out" '^check-private-names: PRIVATE_NAME_TERMS: 1 term read$')"
run_check_as pull_request - "$root"
check "an unset list prints the warning instead" \
  "$(contains "$out" '^::warning::check-private-names: PRIVATE_NAME_TERMS is unset or empty')"
check "and no count" "$(is "$(contains "$out" 'terms\? read')" false)"
clean_pr
run_pr_text
check "the pull request's text check names the count once" \
  "$(is "$(grep -c "$terms_read" <<< "$out")" 1)"

echo "case: with no term in the tree, the list passes and says it ran"
root=$(fixture term-absent)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "names the list and its size" "$(contains "$out" 'or a term of PRIVATE_NAME_TERMS (3 terms)')"
check "warns of nothing" "$(is "$(contains "$out" '::warning::')" false)"

echo "case: a term is a fixed string, whose regex characters match only themselves"
root=$(fixture term-fixed)
printf '%s\n' "${planted_dotted/./X}" > "$root/docs/notes.md"
run_check "$root"
check "a line the term read as a regex would match passes" "$(is "$status" 0)"
printf '%s\n' "${planted_dotted^}" > "$root/docs/notes.md"
run_check "$root"
check "the term itself, in another case, fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" "docs/notes.md:1: names a term of the deployment's own list")"

echo "case: blank lines, a carriage return and the blanks around a term are dropped"
root=$(fixture term-blanks)
padded=$(printf '\n  %s  \r\n\n\t\n' "$planted")
run_check_as pull_request "$padded" "$root"
check "a clean tree passes, so no empty pattern matched every line" "$(is "$status" 0)"
check "counts the one term" "$(contains "$out" '(1 terms)')"
printf 'the %s notes\n' "$planted" > "$root/docs/notes.md"
run_check_as pull_request "$padded" "$root"
check "the trimmed term still fails" "$(is "$status" 1)"

echo "case: a path holding a term is refused, and the log masks it"
root=$(fixture term-path)
mkdir -p "$root/docs/runbooks"
printf 'clean text\n' > "$root/docs/runbooks/$planted-steps.md"
git -C "$root" add docs/runbooks
run_check "$root"
check "a file name holding a term fails" "$(is "$status" 1)"
check "names the directory it is in" \
  "$(contains "$out" "^::error::docs/runbooks/<name>: names a term of the deployment's own list (PRIVATE_NAME_TERMS) in its path\$")"
check "does not print the term" "$(is "$(names_planted "$out")" false)"

root=$(fixture term-owner-path)
mkdir -p "$root/docs/${planted_owner^^}"
printf 'see %s\n' "$planted_owner" > "$root/docs/${planted_owner^^}/notes.md"
git -C "$root" add docs
run_check "$root"
check "a term that spans two directories fails" "$(is "$status" 1)"
check "masks it where it stands in a line's annotation" \
  "$(contains "$out" "^::error file=docs/<name>/notes.md,line=1::docs/<name>/notes.md:1: names a term")"
check "and in the path's own report" "$(contains "$out" "^::error::docs/<name>/notes.md: names a term")"
check "prints neither half of it" "$(is "$(names_planted "$out")" false)"

echo "case: a path that grep's case folding matches to a term is masked by that same folding"
# Three spellings grep -F -i folds to a term in a UTF-8 locale and Bash's own lowercasing does not:
# a long s for an s and a dotless i for an i, against ASCII terms, and a final sigma for a medial
# one, against a Greek term. Each file is clean inside; only its name names a term.
root=$(fixture term-folding)
folding_terms=$(printf '%s\n' moss lift κοσμοσ)
mkdir -p "$root/docs/a" "$root/docs/b" "$root/docs/c"
printf 'clean text\n' > "$root/docs/a/moſs.md"
printf 'clean text\n' > "$root/docs/b/lıft.md"
printf 'clean text\n' > "$root/docs/c/κοσμος.md"
git -C "$root" add docs
LC_ALL=C.UTF-8 run_check_as pull_request "$folding_terms" "$root"
check "fails, so grep folded each name to its term" "$(is "$status" 1)"
for dir in a b c; do
  check "masks the name in docs/$dir" \
    "$(contains "$out" "^::error::docs/$dir/<name>: names a term of the deployment's own list (PRIVATE_NAME_TERMS) in its path\$")"
done
check "counts the three" "$(contains "$out" '^check-private-names: 3 finding(s)')"
folded=$(LC_ALL=C.UTF-8 grep -i -F -e moss -e lift -e κοσμοσ <<< "$out" || true)
check "prints no spelling grep folds to a term" "$(is "$folded" "")"

echo "case: a Kelvin sign and a dotless i fold to a term the same way in both scan modes"
# The repository scan and a named path's scan, and the masking of each, run one grep with one
# pattern per term, so a spelling one of them folds to a term, the others fold too. A file named
# with a Kelvin sign holds the term in ASCII, so its line is a hit whose path must be masked; a
# file named with a dotless i is clean; a third file holds the dotless spelling in its text.
kelvin=$'\xe2\x84\xaa'
dotless_i=$'\xc4\xb1'
fold_terms=$(printf '%s\n' kelp lift)
for mode in repository named; do
  root=$(fixture "term-fold-$mode")
  mkdir -p "$root/docs/fold"
  printf 'kelp\n' > "$root/docs/fold/${kelvin}ELP.md"
  printf 'clean text\n' > "$root/docs/fold/l${dotless_i}ft.md"
  printf 'about l%sft\n' "$dotless_i" > "$root/docs/fold/plain.md"
  git -C "$root" add docs
  if [ "$mode" = repository ]; then
    LC_ALL=C.UTF-8 run_check_as pull_request "$fold_terms" "$root"
  else
    LC_ALL=C.UTF-8 run_check_as pull_request "$fold_terms" "$root" docs
  fi
  check "$mode: fails" "$(is "$status" 1)"
  check "$mode: masks the Kelvin-sign file's line" \
    "$(contains "$out" '^::error file=docs/fold/<name>,line=1::docs/fold/<name>:1: names a term')"
  check "$mode: masks both folded file names as paths" \
    "$(is "$(grep -c -F -x "::error::docs/fold/<name>: names a term of the deployment's own list (PRIVATE_NAME_TERMS) in its path" <<< "$out")" 2)"
  check "$mode: finds the dotless spelling in a file's text" \
    "$(contains "$out" '^::error file=docs/fold/plain.md,line=1::docs/fold/plain.md:1: names a term')"
  check "$mode: counts the four findings" "$(contains "$out" '^check-private-names: 4 finding(s)')"
  folded=$(LC_ALL=C.UTF-8 grep -i -F -e kelp -e lift -e "$kelvin" -e "l${dotless_i}ft" <<< "$out" || true)
  check "$mode: prints no spelling of either term" "$(is "$folded" "")"
done

echo "case: a file name holding a colon is reported and masked whole"
root=$(fixture term-colon)
printf 'clean text\n' > "$root/docs/prefix:${planted^^}.md"
printf 'the %s notes\n' "$planted" > "$root/docs/a:b,c.md"
git -C "$root" add docs
run_check "$root"
check "fails" "$(is "$status" 1)"
check "masks the whole name that holds the term" \
  "$(contains "$out" "^::error::docs/<name>: names a term of the deployment's own list (PRIVATE_NAME_TERMS) in its path\$")"
check "escapes a clean name's colon and comma in the annotation's file property" \
  "$(contains "$out" '^::error file=docs/a%3Ab%2Cc.md,line=1::docs/a:b,c.md:1: names a term')"
check "prints no spelling of the term" "$(is "$(names_planted "$out")" false)"

echo "case: two terms that overlap across a path's parts leave no fragment of either"
# sorrel/wicket and wicket/tamarind share their middle part. Masking the first occurrence alone
# would leave the tail of the second in the log.
root=$(fixture term-overlap)
overlap_terms=$(printf '%s\n' sorrel/wicket wicket/tamarind)
mkdir -p "$root/docs/sorrel/wicket/tamarind"
printf 'clean text\n' > "$root/docs/sorrel/wicket/tamarind/notes.md"
git -C "$root" add docs
run_check_as pull_request "$overlap_terms" "$root"
check "fails" "$(is "$status" 1)"
check "masks every part either term touches as one <name>" \
  "$(contains "$out" "^::error::docs/<name>/notes.md: names a term of the deployment's own list (PRIVATE_NAME_TERMS) in its path\$")"
fragments=$(grep -i -E 'sorrel|wicket|tamarind' <<< "$out" || true)
check "prints no part of either term" "$(is "$fragments" "")"

echo "case: several files a mask prints alike count as several findings"
root=$(fixture term-count)
mkdir -p "$root/docs/runbooks"
printf 'clean text\n' > "$root/docs/runbooks/$planted-one.md"
printf 'clean text\n' > "$root/docs/runbooks/$planted-two.md"
printf '%s\n' "$planted" > "$root/docs/runbooks/plain.md"
git -C "$root" add docs
run_check "$root"
check "fails" "$(is "$status" 1)"
check "counts two path findings and one line finding" "$(contains "$out" '^check-private-names: 3 finding(s)')"
check "reports each named file" \
  "$(is "$(grep -c "^::error::docs/runbooks/<name>: names a term" <<< "$out")" 2)"
check "prints no spelling of the term" "$(is "$(names_planted "$out")" false)"

echo "case: a named target that holds a term is masked in the summary and in the missing-target error"
root=$(fixture term-target)
mkdir -p "$root/docs/${planted^^}"
printf 'clean text\n' > "$root/docs/${planted^^}/notes.md"
run_check "$root" "docs/${planted^^}"
check "fails on the target's path" "$(is "$status" 1)"
check "the summary names the target masked" "$(contains "$out" '(1 files under docs/<name>)')"
check "prints no spelling of the term" "$(is "$(names_planted "$out")" false)"
run_check "$root" "docs/${planted^^}/missing.md"
check "a missing target exits 2" "$(is "$status" 2)"
check "names it masked" "$(contains "$out" '^check-private-names: docs/<name>/missing.md does not exist')"
check "prints no spelling of the term" "$(is "$(names_planted "$out")" false)"

echo "case: another command's message about a path that holds a term is masked"
# find cannot read the locked directory, and says so with its path. Root reads it anyway, so the
# case needs an unprivileged user, as CI's runner is.
if [ "$(id -u)" = 0 ]; then
  check "runs as an unprivileged user, which this case needs" false
else
  root=$(fixture term-diagnostic)
  mkdir -p "$root/docs/${planted^^}/locked"
  chmod 000 "$root/docs/${planted^^}/locked"
  run_check "$root" docs
  chmod 700 "$root/docs/${planted^^}/locked"
  check "a search that could not read every file exits 2" "$(is "$status" 2)"
  check "passes on the command's message" "$(contains "$out" 'docs/<name>/locked')"
  check "prints no spelling of the term" "$(is "$(names_planted "$out")" false)"
fi

echo "case: a traced run prints no term"
# A committed rule's hit and a term in a path, so the trace would pass through the matching and the
# masking of both.
root=$(fixture term-traced)
printf 'https://listener.%s.%s\n' "$internal" "$private_host_suffix" > "$root/docs/notes.md"
mkdir -p "$root/docs/${planted^^}"
printf 'clean text\n' > "$root/docs/${planted^^}/notes.md"
git -C "$root" add docs
set +e
out=$(cd "$root" && bash -x .github/scripts/check-private-names.sh 2>&1)
status=$?
set -e
check "bash -x: fails on its hits" "$(is "$status" 1)"
check "bash -x: the trace and the report together print no spelling of a term" \
  "$(is "$(names_planted "$out")" false)"
set +e
out=$(cd "$root" && env SHELLOPTS=xtrace .github/scripts/check-private-names.sh 2>&1)
status=$?
set -e
check "xtrace in SHELLOPTS: fails on its hits" "$(is "$status" 1)"
check "xtrace in SHELLOPTS: prints no spelling of a term" "$(is "$(names_planted "$out")" false)"

echo "case: an allowed host of the committed rules is no exception for a term"
# Each line passes the committed rules: a reserved example host, a host under the reserved example
# tailnet, and the NATS client's property in the bundle that vendors it.
root=$(fixture term-allowed-hosts)
allowed_example="flax.$internal.$example_host_suffix"
allowed_tailnet="mill.$tailnet_example.$tailnet_suffix"
allowed_property="this.$internal.drain"
printf 'Point it at %s.\nOr at %s.\n' "$allowed_example" "$allowed_tailnet" > "$root/docs/notes.md"
mkdir -p "$root/$(dirname "$vendored_nats")"
printf '        %s(sv);\n' "$allowed_property" > "$root/$vendored_nats"
git -C "$root" add docs "$vendored_nats"
run_check "$root"
check "under the placeholder list, all three pass" "$(is "$status" 0)"
run_check_as pull_request "$(printf '%s\n' "$allowed_example" "$allowed_tailnet" "$allowed_property")" "$root"
check "listed as terms, they fail" "$(is "$status" 1)"
check "the reserved example host is named" "$(contains "$out" "docs/notes.md:1: names a term of the deployment's own list")"
check "the example tailnet host is named" "$(contains "$out" "docs/notes.md:2: names a term of the deployment's own list")"
check "the vendored property is named" "$(contains "$out" "$vendored_nats:1: names a term of the deployment's own list")"

echo "case: the built docs site, scanned as docs-site.yaml's private-names job scans it"
# That job downloads the built site to docs/site/dist and runs the check over that path from the
# checkout's root, on a pull request, a merge group or (docs-pages.yaml) a push.
root=$(fixture term-site)
mkdir -p "$root/docs/site/dist/reference"
printf '<p>clean</p>\n' > "$root/docs/site/dist/index.html"
run_check_as push "$planted_terms" "$root" docs/site/dist
check "a clean built site passes on a push" "$(is "$status" 0)"
printf '<pre>The %s tool.</pre>\n' "$planted" > "$root/docs/site/dist/reference/tools.html"
run_check_as push "$planted_terms" "$root" docs/site/dist
check "a built page naming a term fails" "$(is "$status" 1)"
check "names the page and its line" \
  "$(contains "$out" '^::error file=docs/site/dist/reference/tools.html,line=1::docs/site/dist/reference/tools.html:1: names a term')"
check "prints no spelling of the term" "$(is "$(names_planted "$out")" false)"

echo "case: the packed plugin's dist/ and legion/, scanned as the private-names-packed-plugin action scans them"
# The action lays the tarball's two directories out at packages/claude-envoy and runs the check
# over them from the checkout's root.
root=$(fixture term-plugin)
mkdir -p "$root/packages/claude-envoy/dist" "$root/packages/claude-envoy/legion/skills"
printf 'export {};\n' > "$root/packages/claude-envoy/dist/envoy-channel.js"
printf 'Nothing private here.\n' > "$root/packages/claude-envoy/legion/skills/notes.md"
run_check_as merge_group "$planted_terms" "$root" packages/claude-envoy/dist packages/claude-envoy/legion
check "a clean packed plugin passes on a merge group" "$(is "$status" 0)"
printf 'Ask the %s team.\n' "${planted^}" > "$root/packages/claude-envoy/legion/skills/notes.md"
run_check_as merge_group "$planted_terms" "$root" packages/claude-envoy/dist packages/claude-envoy/legion
check "a packed file naming a term fails" "$(is "$status" 1)"
check "names the file and its line" \
  "$(contains "$out" 'packages/claude-envoy/legion/skills/notes.md:1: names a term')"
check "prints no spelling of the term" "$(is "$(names_planted "$out")" false)"

echo "case: a release's rendered changelog entries, scanned as the private-names-release-notes action scans them"
# The action downloads the entries a release's build job rendered (release-push.sh fold-notes) to
# $RUNNER_TEMP/private-names-release-notes, outside the checkout, and runs the check over that one
# file from the checkout's root, on a push or a dispatched run; the publish job posts that file.
root=$(fixture term-release-notes)
notes_dir="$work/term-release-notes-runner-temp/private-names-release-notes"
mkdir -p "$notes_dir"
notes="$notes_dir/release-notes.md"
printf '### Fixed\n\n* Keep the inbox order.\n' > "$notes"
for event in push workflow_dispatch; do
  run_check_as "$event" "$planted_terms" "$root" "$notes"
  check "$event: clean entries pass" "$(is "$status" 0)"
done
printf '### Fixed\n\n* Move the %s runbook.\n' "${planted^}" > "$notes"
run_check_as push "$planted_terms" "$root" "$notes"
check "an entry naming a term fails" "$(is "$status" 1)"
check "names the file and its line" "$(contains "$out" 'release-notes\.md:3: names a term')"
check "prints no spelling of the term" "$(is "$(names_planted "$out")" false)"
run_check_as push "$planted_dotted" "$root" "$notes"
check "the same entry passes under a list without that term, so the finding is the list's" "$(is "$status" 0)"
run_check_as push - "$root" "$notes"
check "a push with no list exits 2, so an unmapped secret releases nothing" "$(is "$status" 2)"
rm "$notes"
run_check_as push "$planted_terms" "$root" "$notes"
check "a missing file exits 2, so an artifact without the entries never passes as clean" "$(is "$status" 2)"

echo "case: an unset or empty list on a pull request's event warns, and keeps the other rules' exit"
root=$(fixture terms-unset-pr)
unset_warning='^::warning::check-private-names: PRIVATE_NAME_TERMS is unset or empty, so only the'
run_check_as pull_request - "$root"
check "pull_request, unset: exits 0" "$(is "$status" 0)"
check "pull_request, unset: warns that only the rules the script spells ran" "$(contains "$out" "$unset_warning")"
run_check_as pull_request "" "$root"
check "an empty list exits 0 and warns the same way" \
  "$(is "$status:$(contains "$out" "$unset_warning")" 0:true)"
run_check_as pull_request "$(printf '\n \n\t')" "$root"
check "a list of blank lines is an empty one" "$(is "$status:$(contains "$out" "$unset_warning")" 0:true)"
run_check_as - - "$root"
check "outside Actions, an unset list warns too" "$(is "$status:$(contains "$out" "$unset_warning")" 0:true)"
printf '%s\n' "$key-7" > "$root/docs/notes.md"
run_check_as pull_request - "$root"
check "a hit of the rules the script spells still fails" "$(is "$status" 1)"
check "and still warns" "$(contains "$out" "$unset_warning")"
check "names the hit" "$(contains "$out" 'docs/notes.md:1: names the private deployment repository')"

echo "case: an unset list on the merge queue, a push, a dispatched run or a review fails before it scans"
root=$(fixture terms-unset-queue)
# A review starts no run of any workflow, so a pull_request_review run is no fork's pull request
# to warn for: it fails like the other events.
for event in merge_group push workflow_dispatch pull_request_review; do
  run_check_as "$event" - "$root"
  check "$event, unset: exits 2" "$(is "$status" 2)"
  check "$event, unset: says the list is missing" \
    "$(contains "$out" "^::error::check-private-names: PRIVATE_NAME_TERMS is unset or empty on a $event run")"
done
run_check_as merge_group "" "$root"
check "merge_group, empty: exits 2" "$(is "$status" 2)"
run_check_as merge_group "$planted_terms" "$root"
check "merge_group with the list exits 0, so the failure above is the missing list" "$(is "$status" 0)"
printf '%s\n' "$key-7" > "$root/docs/notes.md"
run_check_as merge_group - "$root"
check "merge_group, unset, over a tree with a hit: still exits 2" "$(is "$status" 2)"
check "and reports no finding, having scanned nothing" "$(is "$(contains "$out" 'finding(s)')" false)"

echo "case: a pull request's clean text passes with the list set"
run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
check "a clean title, body, branch, commit and no comment pass" "$(is "$status" 0)"

echo "case: a pull request's title naming a term"
run_pr_text "fix(dispatch): keep the $planted order" "Refs LEGION-7" "fix: keep the order"
check "a title naming a term fails" "$(is "$status" 1)"
check "names the title" "$(contains "$out" 'title:1: names a term')"
check "does not print the term" "$(is "$(names_planted "$out")" false)"

echo "case: a pull request's commit message naming a term"
run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" \
  "$(printf 'fix: keep the order\n\nAs the %s runbook says.\n' "$planted_dotted")"
check "a commit message naming a term fails" "$(is "$status" 1)"
check "names the commit text's line" "$(contains "$out" 'commits:3: names a term')"
check "does not print the term" "$(is "$(names_planted "$out")" false)"

echo "case: a pull request's text naming a term of the deployment's own list"
run_pr_text "fix(dispatch): keep the inbox order" "$(printf 'Line one.\nThe %s runbook says so.\n' "$planted")" \
  "fix: keep the order"
check "a body naming a term fails" "$(is "$status" 1)"
check "names the body's line" "$(contains "$out" "body:2: names a term of the deployment's own list (PRIVATE_NAME_TERMS)")"
check "does not print the term" "$(is "$(names_planted "$out")" false)"

run_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order" "fix/$planted-steps"
check "a branch naming a term fails" "$(is "$status" 1)"
check "names the branch" "$(contains "$out" 'branch:1: names a term')"
check "does not print the term" "$(is "$(names_planted "$out")" false)"

clean_pr
items 101 "Looks good." 102 "Owned by ${planted_owner^^}." > "$work/comments.1"
run_pr_text
check "an issue comment naming a term fails" "$(is "$status" 1)"
check "names the comment by its id and line" "$(contains "$out" 'comment:102:1: names a term')"
check "prints neither half of the term" "$(is "$(names_planted "$out")" false)"

echo "case: a stranger whose login holds a term is skipped without the login"
clean_pr
items 407 "fine" > "$work/comments.1"
by "${planted}-fan" User NONE "$work/comments.1"
run_pr_text
check "a stranger's clean comment passes" "$(is "$status" 0)"
check "names the skipped comment by its author's association" \
  "$(contains "$out" '^skipped comment:407: its author (NONE) is not trusted$')"
check "prints no spelling of the term" "$(is "$(names_planted "$out")" false)"

echo "case: a pull request's text with the list unset, on a pull request and on the merge queue"
clean_pr
GITHUB_EVENT_NAME=pull_request PRIVATE_NAME_TERMS='' run_pr_text
check "a pull request's run passes clean text" "$(is "$status" 0)"
check "and warns that the list did not run" "$(contains "$out" "$unset_warning")"
clean_pr
GITHUB_EVENT_NAME=merge_group PRIVATE_NAME_TERMS='' run_pr_text
check "a merge group's run fails with exit 2, the pr-title job's failure" "$(is "$status" 2)"
check "says the list is missing" \
  "$(contains "$out" '::error::check-private-names: PRIVATE_NAME_TERMS is unset or empty on a merge_group run')"

# THE ISSUE-KEY RULE (check-private-names.sh's rules 6 and 7, from check-pr-text.sh's cutoff).
# QUILL is a placeholder project key, given as a `key:` line beside the placeholder terms; its
# issue keys are spelled from pieces, so this file holds none whole, and no case label prints one.
key_project=QUILL
planted_key="$key_project-""123"
key_terms=$(printf '%s\n' "$planted_terms" "key:$key_project" key:LEGION)
link="dispatch"':''//'
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
# files (a patch of "-" is none at all), and the pull request route's totals to match them.
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
check "names the release it could not read" \
  "$(contains "$out" '^::error::UNKNOWN: GitHub did not answer for the release of legion-v9.0.0 (gh: Server Error (HTTP 500))')"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
mv "$work/remote" "$work/remote.held"
key_run
mv "$work/remote.held" "$work/remote"
check "a history git cannot read exits 2" "$(is "$status" 2)"
check "says git could not read it" "$(contains "$out" "^::error::UNKNOWN: git could not read main's history")"
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
opened_after() { key_rule_on; opened_at 2026-11-21T00:00:00Z; commits new-sha 2026-11-21T01:00:00Z "fix: keep the order"; commit_file new-sha modified docs/notes.md $'@@ -1,1 +1,1 @@\n+clean text'; }
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
items 601 "See ${link}x/spec." > "$work/reviews.1"
key_run
check "a key in a comment and a link in a review fail" "$(is "$status" 1)"
check "names the comment" "$(contains "$out" "comment:502:1: $key_refused")"
check "names the review's link" "$(contains "$out" 'review:601:1: holds a Dispatch link')"

echo "case: a commit is read after the cutoff and not before it"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits old-sha 2026-11-19T00:00:00Z "fix: as $planted_key said" new-sha 2026-11-21T01:00:00Z "fix: as $planted_key said"
commit_file old-sha modified docs/notes.md $'@@ -0,0 +1,2 @@\n+line one\n+old '"$planted_key"' line'
commit_file new-sha modified docs/notes.md $'@@ -3,0 +4,2 @@\n+clean\n+new '"$planted_key"' line'
key_run
check "fails on the new commit" "$(is "$status" 1)"
check "names the new commit's message" "$(contains "$out" "commit:new-sha:1: $key_refused")"
check "names the line the new commit adds, at its line in the file" \
  "$(contains "$out" "commit:new-sha:docs/notes.md:5: $key_refused")"
check "reads nothing of the commit dated before the cutoff" "$(is "$(contains "$out" 'old-sha')" false)"
check "and never asks GitHub for its files" "$(is "$(contains "$(< "$work/calls")" 'commits/old-sha')" false)"

echo "case: a path a new commit adds is new text, and a path it only changes is not"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits new-sha 2026-11-21T01:00:00Z "fix: keep the order"
commit_file new-sha added "docs/notes-$planted_key.md" $'@@ -0,0 +1,1 @@\n+clean text'
key_run
check "an added file whose path holds a key fails" "$(is "$status" 1)"
check "names the path, masked" \
  "$(contains "$out" "^::error::commit:new-sha:docs/<name>: $key_refused .* in its path\$")"
check "prints no key" "$(is "$(contains "$out" "$planted_key")" false)"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits new-sha 2026-11-21T01:00:00Z "fix: keep the order"
commit_file new-sha modified "docs/notes-$planted_key.md" $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "a clean line added to a file whose old path holds a key passes" "$(is "$status" 0)"

echo "case: a commit neither GitHub nor git returns whole is UNKNOWN, never passed"
# The commit ids here are no commit git can fetch, so git cannot read the commit either.
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits new-sha 2026-11-21T01:00:00Z "fix: keep the order"
commit_file new-sha modified docs/big.md - 
jq -c '.files[0].additions = 5 | .files[0].changes = 5 | .stats.total = 5' "$work/commit.new-sha" > "$work/commit.next"
mv "$work/commit.next" "$work/commit.new-sha"
key_run
check "a file with added lines and no patch, which git cannot read either, exits 2" "$(is "$status" 2)"
check "says UNKNOWN for that file" "$(contains "$out" 'UNKNOWN: GitHub returned no patch for file 1 of commit new-sha, and git could not read it either')"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits new-sha 2026-11-21T01:00:00Z "fix: keep the order"
commit_file new-sha modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text' 9
key_run
check "a commit whose listed files fall short of its total exits 2" "$(is "$status" 2)"
check "says UNKNOWN for that commit" "$(contains "$out" 'UNKNOWN: commit new-sha changes 9 lines')"

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

echo "case: a key or a link the same diff also removes is carried over, not new"
# carry_commit <status> <path> <patch>: a pull request opened after the cutoff whose one commit is
# that one file, as GitHub answers it.
carry_commit() {
  write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
  key_rule_on
  opened_at 2026-11-21T00:00:00Z
  commits new-sha 2026-11-21T01:00:00Z "fix: keep the order"
  commit_file new-sha "$1" "$2" "$3"
}
carry_commit modified docs/notes.md \
  "$(printf '@@ -2,1 +2,1 @@\n-Tracked as %s, see %sexample/1.\n+Now tracked as %s, see %sexample/1.' \
    "$planted_key" "$link" "$planted_key" "$link")"
key_run
check "a line re-worded around the key and the link it held passes" "$(is "$status" 0)"
carry_commit modified docs/notes.md \
  "$(printf '@@ -2,1 +2,2 @@\n-Tracked as %s.\n+Now tracked as %s.\n+And again as %s.' \
    "$planted_key" "$planted_key" "$planted_key")"
key_run
check "a second copy of the key, which no removed line carries, fails" "$(is "$status" 1)"
check "names the second copy's line" "$(contains "$out" "commit:new-sha:docs/notes.md:3: $key_refused")"
check "and not the carried one's" "$(is "$(contains "$out" "commit:new-sha:docs/notes.md:2:")" false)"
carry_commit modified docs/notes.md "$(printf '@@ -2,1 +2,1 @@\n-See %sexample/1.\n+See %sexample/2.' "$link" "$link")"
key_run
check "a link to another place is new: fails" "$(is "$status" 1)"
check "names its line" "$(contains "$out" 'commit:new-sha:docs/notes.md:2: holds a Dispatch link')"
carry_commit added docs/new.md "$(printf '@@ -0,0 +1,1 @@\n+Tracked as %s.' "$planted_key")"
jq -c --arg old "$(printf '@@ -1,1 +0,0 @@\n-Tracked as %s.' "$planted_key")" \
  '.files = [{filename: "docs/old.md", status: "removed", additions: 0, changes: 1, patch: $old}] + .files
   | .stats.total = 2' "$work/commit.new-sha" > "$work/commit.next"
mv "$work/commit.next" "$work/commit.new-sha"
key_run
check "a key a commit moves from a removed file into an added one passes" "$(is "$status" 0)"
carry_commit renamed "docs/$planted_key-notes.md" $'@@ -1,0 +2,1 @@\n+clean text'
jq -c --arg old "docs/old/$planted_key-notes.md" '.files[0].previous_filename = $old' "$work/commit.new-sha" \
  > "$work/commit.next"
mv "$work/commit.next" "$work/commit.new-sha"
key_run
check "a renamed file whose new path keeps its old path's key passes" "$(is "$status" 0)"
carry_commit renamed "docs/$planted_key-notes.md" $'@@ -1,0 +2,1 @@\n+clean text'
jq -c '.files[0].previous_filename = "docs/notes.md"' "$work/commit.new-sha" > "$work/commit.next"
mv "$work/commit.next" "$work/commit.new-sha"
key_run
check "a renamed file whose new path adds a key fails" "$(is "$status" 1)"
check "names the path, masked" "$(contains "$out" "^::error::commit:new-sha:docs/<name>: $key_refused .* in its path\$")"
carry_commit modified docs/notes.md "$(printf '@@ -2,1 +2,1 @@\n-Filed as \xc3\xa9%s.\n+Filed as %s.' "$planted_key" "$planted_key")"
key_run
check "a removed word beside a non-ASCII letter, which is no key, carries no key: fails" "$(is "$status" 1)"
check "names the added key's line" "$(contains "$out" "commit:new-sha:docs/notes.md:2: $key_refused")"
carry_commit modified docs/notes.md "$(printf '@@ -2,1 +2,1 @@\n-Filed as fix_%s.\n+Filed as %s.' "$planted_key" "$planted_key")"
key_run
check "a removed key beside a \"_\", which is a key, carries it: passes" "$(is "$status" 0)"

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
  commits new-sha 2026-11-21T01:00:00Z "fix: keep the order" merge-sha 2026-11-21T02:00:00Z "${1:-Merge main forward}"
  as_merge merge-sha
  commit_file new-sha modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
}
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits new-sha 2026-11-21T01:00:00Z "fix: keep the order"
commit_file new-sha modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "control 1, the branch's own clean commit alone, exits 0" "$(is "$status" 0)"
merge_branch
commit_file merge-sha modified docs/other-pr.md $'@@ -0,0 +1,1 @@\n+other '"$planted_key"' line'
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "control 2, a merge whose own diff adds another pull request's key, exits 0" "$(is "$status" 0)"
check "never asks GitHub for the merge's own files" "$(is "$(contains "$(< "$work/calls")" 'commits/merge-sha')" false)"
check "reads the pull request's files once" "$(is "$(grep -c 'pulls/7/files' "$work/calls")" 1)"
merge_branch
commit_file merge-sha modified docs/big.md -
jq -c '.files[0].additions = 5000 | .files[0].changes = 5000 | .stats.total = 5000' "$work/commit.merge-sha" > "$work/commit.next"
mv "$work/commit.next" "$work/commit.merge-sha"
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "control 3, a merge whose own diff holds a large file with no patch, exits 0" "$(is "$status" 0)"
check "and says nothing UNKNOWN" "$(is "$(contains "$out" 'UNKNOWN')" false)"
merge_branch
commit_file merge-sha modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
pr_files modified docs/notes.md $'@@ -1,0 +2,2 @@\n+clean text\n+resolved as '"$planted_key"' said'
key_run
check "a key typed while resolving a conflict, in the net diff, fails" "$(is "$status" 1)"
check "names the net diff's file and line" "$(contains "$out" "files:docs/notes.md:3: $key_refused")"
check "prints no key" "$(is "$(contains "$out" "$planted_key")" false)"
merge_branch "Merge main forward, as $planted_key asked"
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+clean text'
key_run
check "a merge's message is still read" "$(is "$status" 1)"
check "names the merge's message" "$(contains "$out" "commit:merge-sha:1: $key_refused")"
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
commits old-sha 2026-11-19T00:00:00Z "fix: keep the order"
commit_file old-sha modified docs/notes.md $'@@ -1,0 +2,1 @@\n+old '"$planted_key"' line'
key_run
check "D1, the old commit alone, is not read: exits 0" "$(is "$status" 0)"
write_pr_text "fix(dispatch): keep the inbox order" "Refs LEGION-7" "fix: keep the order"
key_rule_on
opened_at 2026-11-21T00:00:00Z
commits old-sha 2026-11-19T00:00:00Z "fix: keep the order" merge-sha 2026-11-21T02:00:00Z "Merge main forward"
as_merge merge-sha
commit_file old-sha modified docs/notes.md $'@@ -1,0 +2,1 @@\n+old '"$planted_key"' line'
pr_files modified docs/notes.md $'@@ -1,0 +2,1 @@\n+old '"$planted_key"' line'
key_run
check "D2, the same branch with a clean forward merge, is read through its net diff: exits 1" "$(is "$status" 1)"
check "names the net diff's line" "$(contains "$out" "files:docs/notes.md:2: $key_refused")"

echo "case: the tree outside a pull request's new text is not held to the issue-key rule"
root=$(fixture key-tree)
printf 'From before the rule: %s.\n' "$planted_key" > "$root/docs/notes.md"
run_check_as pull_request "$key_terms" "$root"
check "a key in a tree file passes the repository scan" "$(is "$status" 0)"
check "and the scan names the keys it read" "$(contains "$out" 'PRIVATE_NAME_TERMS: 3 terms and 1 issue key read')"
set +e
out=$(cd "$root" && PRIVATE_NAME_TERMS=$key_terms GITHUB_EVENT_NAME=pull_request PRIVATE_NAME_RULES=keys \
  .github/scripts/check-private-names.sh docs 2>&1)
status=$?
set -e
check "the same file fails a keys run" "$(is "$status" 1)"
check "names its line" "$(contains "$out" "docs/notes.md:1: $key_refused")"

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

finish_self_log
summary "check-private-names.sh's term list"
