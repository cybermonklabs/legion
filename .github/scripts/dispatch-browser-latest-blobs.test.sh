#!/usr/bin/env bash
# Tests for `dispatch-browser-latest-blobs.sh`: the merge job of dispatch-browser.yaml must judge
# a re-run on the attempt that ran, so each case builds a download directory of real directories
# and files - the two shapes `actions/download-artifact` leaves without `merge-multiple`, a
# directory named for each artifact when several match and the one artifact's own files when
# one does - and reads what the script wrote.
#
# The file contents stand in for the Playwright blob zips: this script picks files, it does not
# read them, so a line of text per file is what tells one attempt's copy from another's. That a
# real merge of two attempts' blobs turns on which copy won is proved against Playwright itself,
# in the pull request that added this script.
#
# NO CASE REACHES THE NETWORK OR PLAYWRIGHT: each one is directories, files and the script.
#
# Run from anywhere: .github/scripts/dispatch-browser-latest-blobs.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
select_script="$script_dir/dispatch-browser-latest-blobs.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: an empty download directory and an empty output directory for one case. Echoes
# the case's root, which holds `down` and `out`. A repeated name is refused here rather than
# surfacing as one case reading another's files.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "two cases named $1" >&2
    return 1
  fi
  mkdir -p "$root/down" "$root/out"
  echo "$root"
}

# leg <root> <leg> <attempt> <mark>: the artifact one leg's upload leaves at one attempt - a
# directory named for the artifact, holding the leg's blob report and the row count beside it.
# <mark> is written into both files, so the output says which attempt's copy arrived. The
# artifact's name starts with $caller, the id of the job that calls dispatch-browser.yaml.
caller=dispatch-browser
leg() {
  local dir="$1/down/$caller-blob-$2-$3"
  mkdir -p "$dir/blob-report" "$dir/rows"
  printf '%s\n' "$4" > "$dir/blob-report/report-$2.zip"
  printf '%s\n' "$4" > "$dir/rows/$2.txt"
}

# lone_leg <root> <leg> <mark>: the same artifact when it is the only one the download matched,
# which actions/download-artifact extracts into the download directory itself and not into a
# directory named for it (the script's head names the note that cites the action's source), so
# the leg's blob report and row count sit at the top.
lone_leg() {
  mkdir -p "$1/down/blob-report" "$1/down/rows"
  printf '%s\n' "$3" > "$1/down/blob-report/report-$2.zip"
  printf '%s\n' "$3" > "$1/down/rows/$2.txt"
}

# run_select <root>: runs the script under test over that case's directories for $caller, setting
# `out` to its combined output and `status` to its exit code. Not a subshell, so both survive the
# call.
run_select() {
  set +e
  out=$("$select_script" "$1/down" "$1/out" "$caller" 2>&1)
  status=$?
  set -e
}

# held <root> <path>: the one line of a file the script wrote, or `absent`.
held() {
  if [ -f "$1/out/$2" ]; then cat "$1/out/$2"; else echo absent; fi
}

echo "case: the newest attempt of a leg wins"
root=$(fixture newest-wins)
leg "$root" shard-1 1 first-attempt
leg "$root" shard-1 2 second-attempt
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "the output holds attempt 2's blob" "$(is "$(held "$root" blob-report/report-shard-1.zip)" second-attempt)"
check "and attempt 2's rows" "$(is "$(held "$root" rows/shard-1.txt)" second-attempt)"
check "and nothing of attempt 1" "$(is "$(grep -rl first-attempt "$root/out" | wc -l)" 0)"
check "says which attempt it took" "$(contains "$out" '^shard-1: attempt 2, passing over attempt(s) 1$')"

echo "case: a leg the re-run did not run keeps its earlier attempt"
# `gh run rerun --failed` re-runs the failed legs alone, so a leg that was green has no artifact
# at the new attempt. Dropping it would merge a report short of that leg's rows.
root=$(fixture rerun-failed-only)
leg "$root" shard-1 1 shard-1-first
leg "$root" shard-1 2 shard-1-second
leg "$root" shard-2 1 shard-2-first
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "the re-run leg is at attempt 2" "$(is "$(held "$root" blob-report/report-shard-1.zip)" shard-1-second)"
check "the leg not re-run keeps attempt 1's blob" "$(is "$(held "$root" blob-report/report-shard-2.zip)" shard-2-first)"
check "and attempt 1's rows" "$(is "$(held "$root" rows/shard-2.txt)" shard-2-first)"
check "which it says it took" "$(contains "$out" '^shard-2: attempt 1$')"

echo "case: attempt 10 beats attempt 9"
# The attempts are compared as numbers. Read as text, attempt 10 would sort below attempt 9 and
# the tenth run of a lane would be judged on the ninth's rows.
root=$(fixture attempt-ten)
leg "$root" shard-1 9 ninth
leg "$root" shard-1 10 tenth
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "the output holds attempt 10's blob" "$(is "$(held "$root" blob-report/report-shard-1.zip)" tenth)"
check "says it passed over attempt 9" "$(contains "$out" '^shard-1: attempt 10, passing over attempt(s) 9$')"

echo "case: every leg shape reads"
# A shard leg's name ends with its number, and an alone leg named for its project, as each was
# while the alone job ran a matrix leg per project, ends with the project, so the digits at the
# end of the artifact name are the attempt in both. The alone job's one leg is the bare word
# `alone` now (dispatch-browser.yaml uploads dispatch-browser-blob-alone-<attempt>), which every
# full run's merge job reads as a leg of its own beside the two shards.
root=$(fixture leg-shapes)
leg "$root" shard-3 1 shard-3-blob
leg "$root" alone-webkit-alone 1 webkit-blob
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "the shard leg is selected" "$(is "$(held "$root" blob-report/report-shard-3.zip)" shard-3-blob)"
check "the alone leg is selected" "$(is "$(held "$root" blob-report/report-alone-webkit-alone.zip)" webkit-blob)"
check "and named with its attempt" "$(contains "$out" '^alone-webkit-alone: attempt 1$')"

echo "case: a directory whose name does not read fails"
# A name ending in anything but digits carries no attempt, so which of two copies of a leg is
# the newer cannot be read from it. The whole step fails rather than merge a set it cannot
# order: a blob dropped quietly is a merged report short of a leg's rows, which reads as a
# shorter suite and not as an error.
root=$(fixture name-unreadable)
leg "$root" shard-1 1 shard-1-first
mkdir -p "$root/down/dispatch-browser-blob-shard-1-retry/blob-report"
run_select "$root"
check "fails" "$(is "$status" 1)"
check "names the directory" "$(contains "$out" 'down/dispatch-browser-blob-shard-1-retry is not a leg')"
check "says what the name must be" "$(contains "$out" 'dispatch-browser-blob-<leg>-<attempt>')"
check "and what to do about it" "$(contains "$out" 'narrow the download step.s pattern')"
check "and merges nothing" "$(is "$(grep -rl shard-1-first "$root/out" | wc -l)" 0)"

root=$(fixture name-no-leg)
mkdir -p "$root/down/dispatch-browser-blob-2/blob-report"
run_select "$root"
check "a name with an attempt and no leg fails too" "$(is "$status" 1)"
check "naming it" "$(contains "$out" 'down/dispatch-browser-blob-2 is not a leg')"

root=$(fixture name-not-a-directory)
printf 'stray\n' > "$root/down/dispatch-browser-blob-shard-1-1"
run_select "$root"
check "a file where a directory was expected fails too" "$(is "$status" 1)"
check "naming it" "$(contains "$out" 'down/dispatch-browser-blob-shard-1-1 is not a leg')"

echo "case: one artifact, which the download extracts into the download directory itself"
# How many artifacts the download's pattern matches, over every attempt of the run, is the
# run's: one leg that uploaded a blob on one attempt makes one, and with one match
# actions/download-artifact leaves no directory named for it. Read as an entry's name,
# blob-report/ is no leg's artifact, and the step failed a merge job that had a blob to merge.
root=$(fixture one-artifact)
lone_leg "$root" shard-1 lone
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "prints no error" "$(is "$(contains "$out" '::error::')" false)"
check "the output holds that artifact's blob" "$(is "$(held "$root" blob-report/report-shard-1.zip)" lone)"
check "and its rows" "$(is "$(held "$root" rows/shard-1.txt)" lone)"
check "and no other file" "$(is "$(find "$root/out" -type f | wc -l)" 2)"
check "says which layout it read" \
  "$(contains "$out" 'holds blob-report/ and rows/ at its top, which is how actions/download-artifact extracts an artifact when exactly one matched')"
check "and names the files it took" \
  "$(contains "$out" 'taken whole: blob-report/report-shard-1.zip, rows/shard-1.txt\.$')"
lone_out=$root/out
root=$(fixture one-artifact-named)
leg "$root" shard-1 1 lone
run_select "$root"
check "the same artifact in a directory named for it writes the same files" \
  "$(diff -r "$lone_out" "$root/out" > /dev/null && echo true || echo false)"

echo "case: a lone artifact that holds no blob report keeps its rows"
# A leg whose browser step wrote its row count and no blob uploads rows/ alone. Read in either
# layout, the count is copied and the blob directory stays empty.
root=$(fixture one-artifact-rows-only)
mkdir -p "$root/down/rows"
printf 'rows-only\n' > "$root/down/rows/shard-2.txt"
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "copies its rows" "$(is "$(held "$root" rows/shard-2.txt)" rows-only)"
check "and no blob" "$(is "$(find "$root/out/blob-report" -mindepth 1 | wc -l)" 0)"
check "and says it found rows/ alone" "$(contains "$out" 'down holds rows/ at its top')"

echo "case: beside a lone artifact's two directories, any other entry fails"
# A leg's artifact holds blob-report/ and rows/ and nothing else, and when several artifacts
# match none of them is extracted at the top. So an entry beside those two is a layout this
# step does not know, and it fails rather than merge around it.
root=$(fixture one-artifact-stray)
lone_leg "$root" shard-1 lone
printf 'stray\n' > "$root/down/notes.txt"
mkdir -p "$root/down/dispatch-browser-blob-shard-2-1/blob-report"
run_select "$root"
check "fails" "$(is "$status" 1)"
check "names the stray file" "$(contains "$out" 'down/notes.txt is not part of a leg.s blob report')"
check "and the stray directory" \
  "$(contains "$out" 'down/dispatch-browser-blob-shard-2-1 is not part of a leg.s blob report')"
check "and merges nothing" "$(is "$(find "$root/out" -type f | wc -l)" 0)"

root=$(fixture one-artifact-blob-file)
printf 'stray\n' > "$root/down/blob-report"
run_select "$root"
check "a blob-report that is a file and not a directory fails too" "$(is "$status" 1)"
check "naming it" "$(contains "$out" 'down/blob-report is not part of a leg.s blob report')"

echo "case: an empty download directory writes empty output and does not fail"
# The merge step after this one fails when there is nothing to merge, and the merge job's first
# reading names the legs that owed a blob, which is what says why.
root=$(fixture download-empty)
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "says so in one line" "$(contains "$out" 'No leg.s blob artifact was downloaded into')"
check "writes the blob directory" "$(is "$(find "$root/out/blob-report" -mindepth 1 | wc -l)" 0)"
check "writes the rows directory" "$(is "$(find "$root/out/rows" -mindepth 1 | wc -l)" 0)"

root=$(fixture download-absent)
rmdir "$root/down"
run_select "$root"
check "an absent download directory is the same" "$(is "$status" 0)"
check "and the output directories are written" "$(is "$(find "$root/out" -type d | wc -l)" 3)"

echo "case: the rows file travels with its blob"
# A leg's rows are the count its log printed, which reading one holds against the merged report.
# Taking one attempt's blob beside another's rows would report a mismatch that never happened.
root=$(fixture rows-travel)
leg "$root" shard-1 1 first-attempt
leg "$root" shard-1 2 second-attempt
leg "$root" shard-2 2 shard-2-second
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "each selected leg's blob and rows are the one attempt's" \
  "$(is "$(held "$root" blob-report/report-shard-1.zip)" "$(held "$root" rows/shard-1.txt)")"
check "for every leg" \
  "$(is "$(held "$root" blob-report/report-shard-2.zip)" "$(held "$root" rows/shard-2.txt)")"
check "and the output holds one file per leg per directory" \
  "$(is "$(find "$root/out" -type f | wc -l)" 4)"

echo "case: the pull-request caller's artifacts read under its own id"
# ci.yaml calls dispatch-browser.yaml three times, and one run of a pull request can run several
# of those callers, so every artifact name starts with the caller's id: the pull-request lane's
# merge job reads dispatch-browser-pr-blob-<leg>-<attempt>.
caller=dispatch-browser-pr
root=$(fixture pr-caller)
leg "$root" real 1 first-attempt
leg "$root" real 2 second-attempt
leg "$root" stub 1 stub-first
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "the real leg's blob comes from attempt 2" "$(is "$(held "$root" blob-report/report-real.zip)" second-attempt)"
check "says which attempt it took" "$(contains "$out" '^real: attempt 2, passing over attempt(s) 1$')"
check "the stub leg keeps its earlier blob" "$(is "$(held "$root" blob-report/report-stub.zip)" stub-first)"
check "the stub count travels with its blob" "$(is "$(held "$root" rows/stub.txt)" stub-first)"
caller=dispatch-browser

echo "case: the other caller's artifact is refused, never merged"
# The download step's pattern takes only this caller's artifacts. One of the other caller's that
# reached the directory anyway would merge that lane's rows into this one's report, so it fails
# by name, as any name that does not read does.
root=$(fixture other-caller)
leg "$root" shard-1 1 full-lane
caller=dispatch-browser-pr leg "$root" shard-1 1 pull-request-lane
run_select "$root"
check "fails" "$(is "$status" 1)"
check "names the other caller's directory" "$(contains "$out" 'down/dispatch-browser-pr-blob-shard-1-1 is not a leg')"
check "and merges nothing" "$(is "$(find "$root/out" -type f | wc -l)" 0)"

root=$(fixture other-caller-reversed)
leg "$root" shard-1 1 full-lane
caller=dispatch-browser-pr
run_select "$root"
check "the pull-request caller refuses the full lane's artifact too" "$(is "$status" 1)"
check "naming it" "$(contains "$out" 'down/dispatch-browser-blob-shard-1-1 is not a leg')"
caller=dispatch-browser

echo "case: a leg name holding a space is one leg"
# A defensive row, and no longer a live case. The alone job had a matrix leg per project and its
# artifact carried the project in its name (`dispatch-browser-blob-alone-<project>-<attempt>`),
# so a project named with a space would have arrived here as a leg holding one. The alone job
# runs the three projects on one machine now and uploads `dispatch-browser-blob-alone-<attempt>`,
# and the only other leg names are `shard-<N>`, so no name this workflow uploads holds a space
# (dispatch://INFRA-1109). The row stays because what a split name costs is silent: this script
# never sees the merge job's capture - a leg reaches it as the tail of an artifact directory
# name - and read with word splitting, as this loop once was, such a name becomes two legs,
# neither with a blob or a row count of its own, and the merged report comes out a leg short
# rather than red.
root=$(fixture leg-with-space)
leg "$root" "alone-chromium hidpi" 1 spaced
leg "$root" shard-1 1 plain
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "selects two legs, not three" "$(contains "$out" 'Selected 2 leg(s)')"
check "and names the spaced leg whole" "$(contains "$out" 'alone-chromium hidpi: attempt 1')"
check "its blob lands under its own name" \
  "$(is "$(held "$root" 'blob-report/report-alone-chromium hidpi.zip')" spaced)"
check "and its rows travel with it" \
  "$(is "$(held "$root" 'rows/alone-chromium hidpi.txt')" spaced)"

echo "case: the references caller reads its own blobs and refuses the pull-request caller's"
# ci.yaml's dispatch-browser-pr and dispatch-references jobs both call dispatch-browser.yaml on a
# pull request that changes what the reference rows test, and an artifact name is unique within a
# run, so the references lane's blobs are named under its caller's id and its merge job passes
# that id here. A blob of the pull-request caller in its download directory is a download pattern
# that took another lane's rows, which fails rather than merging them into this lane's report.
caller=dispatch-references
root=$(fixture references-caller)
leg "$root" shard-1 1 references-first
leg "$root" shard-1 2 references-second
leg "$root" shard-4 1 references-shard-4
run_select "$root"
check "exits 0" "$(is "$status" 0)"
check "the output holds attempt 2's blob" "$(is "$(held "$root" blob-report/report-shard-1.zip)" references-second)"
check "and the other leg's" "$(is "$(held "$root" blob-report/report-shard-4.zip)" references-shard-4)"
check "says which attempt it took" "$(contains "$out" '^shard-1: attempt 2, passing over attempt(s) 1$')"

root=$(fixture references-caller-pull-request-blob)
leg "$root" shard-1 1 references-blob
caller=dispatch-browser-pr leg "$root" shard-1 1 pull-request-blob
run_select "$root"
check "a blob of the pull-request caller fails" "$(is "$status" 1)"
check "naming it" "$(contains "$out" 'down/dispatch-browser-pr-blob-shard-1-1 is not a leg')"
check "and the name it must have" "$(contains "$out" 'dispatch-references-blob-<leg>-<attempt>')"
check "and merges nothing" "$(is "$(grep -rl references-blob "$root/out" | wc -l)" 0)"
caller=dispatch-browser

echo "case: a call that names no directories"
set +e
out=$("$select_script" 2>&1)
status=$?
set -e
check "fails" "$(is "$status" 1)"
check "says what it takes" "$(contains "$out" '<download-dir> <out-dir> <job>')"

echo "case: a call that names the directories and no caller"
root=$(fixture no-caller)
leg "$root" shard-1 1 lone
set +e
out=$("$select_script" "$root/down" "$root/out" 2>&1)
status=$?
set -e
check "fails" "$(is "$status" 1)"
check "says what it takes" "$(contains "$out" '<download-dir> <out-dir> <job>')"
check "and merges nothing" "$(is "$(find "$root/out" -type f 2>/dev/null | wc -l)" 0)"

summary "dispatch-browser-latest-blobs.sh"
