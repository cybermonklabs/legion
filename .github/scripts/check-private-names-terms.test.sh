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

echo "case: a term the list holds beside a byte-order mark or a no-break space still matches"
# [[:space:]] holds neither, so a term kept with one matched nothing while the count line said it
# was read.
root=$(fixture terms-bom-nbsp)
printf 'Ask the %s team.\n' "${planted^}" > "$root/docs/notes.md"
run_check_as merge_group "$(printf '\xef\xbb\xbf%s\n%s\n' "$planted" "$planted_dotted")" "$root"
check "a byte-order mark before the term: fails" "$(is "$status" 1)"
check "names the line" "$(contains "$out" 'docs/notes.md:1: names a term')"
run_check_as merge_group "$(printf '%s\xc2\xa0\n%s\n' "$planted" "$planted_dotted")" "$root"
check "a no-break space after the term: fails" "$(is "$status" 1)"
run_check_as merge_group "$(printf '\xc2\xa0 %s \xc2\xa0\n%s\n' "$planted" "$planted_dotted")" "$root"
check "no-break spaces and blanks on both sides: fails" "$(is "$status" 1)"
check "prints no spelling of the term" "$(is "$(names_planted "$out")" false)"

echo "case: no term reaches a command's arguments, which any process of the job can read"
# argv_grep (check-private-names-test-lib.sh) logs each pattern a grep is handed on its command
# line, never a file's path, which is the tree's own text and not the list's. The term is in a path
# here as well as in a file, so the run masks the path, which runs every term's own pattern over it
# (term_spans).
root=$(fixture terms-argv)
printf 'Ask the %s team.\n' "${planted^}" > "$root/docs/notes.md"
mkdir -p "$root/docs/${planted^^}"
printf 'clean\n' > "$root/docs/${planted^^}/notes.md"
git -C "$root" add docs
argv_dir="$work/argv-grep"
argv_log="$work/argv-grep.log"
mkdir -p "$argv_dir"
: > "$argv_log"
argv_grep "$argv_dir/grep" "$argv_log"
PATH="$argv_dir:$PATH" run_check_as merge_group "$planted_terms" "$root"
check "the run still finds the term, in the file and in the path: fails" "$(is "$status" 1)"
check "the path's report is masked" "$(contains "$out" 'docs/<name>/notes.md: names a term')"
check "the run's term search, its patterns from a file, reached the fake grep" \
  "$(contains "$(< "$argv_log")" '^<search grep -f>$')"
check "no grep was handed a term in its arguments" "$(is "$(names_planted "$(< "$argv_log")")" false)"

finish_self_log
summary "check-private-names.sh's term list"
