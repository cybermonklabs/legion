#!/usr/bin/env bash
# Tests for `dispatch-browser-reading-rows.sh`, reading one of dispatch-browser.yaml's merge job.
# The cases cover what it says about the legs' blob reports and row counts for each way its two
# input steps can end. The download ("Download every leg's blob report") succeeded, failed, was
# cancelled, was skipped, or ended in a conclusion the script does not know. The selection after
# a download that succeeded ("Take the newest attempt of each leg's blob report") succeeded,
# failed, was skipped, or was cancelled part of the way or after it had copied every leg. Where
# both succeeded, the cases cover a leg's blob or count absent, every leg's blob absent, a count
# that holds no number, a merged report that is absent, not JSON, JSON whose stats jq cannot sum,
# or stats that sum to no whole number, and a merged total equal to, under or over the legs'.
#
# The case it was written for is run 44754426131, attempt 1. There the job's time limit
# cancelled the merge job's `bun install`, both steps were skipped, and this reading said "no
# blob report arrived from: shard-1 shard-2 shard-3 shard-4", though each shard had uploaded its
# blob before the merge job started. The first case drives that shape.
#
# Each case builds a root holding what the merge job's earlier steps leave in $RUNNER_TEMP: the
# downloaded artifacts, the selected blob reports and row counts, and the merged report. The
# root also holds a copy of the script under test, the real one beside this file, and a
# stand-in for .github/scripts/dispatch-browser-run-reads.sh, which the script calls relative
# to its working directory. The stand-in writes a fixture's jobs instead of reading GitHub; the
# real read is dispatch-browser-run-reads.test.sh's. The script is run from that root through
# the merge job's own call: the step "Reading one, the rows" as dispatch-browser.yaml beside
# this directory writes it, with each `${{ steps.<id>.conclusion }}` replaced by the case's
# conclusion. So a workflow that passes the script the wrong conclusion fails here too.
#
# NO CASE REACHES THE NETWORK OR PLAYWRIGHT: each one is directories, files and the script.
#
# Run from anywhere: .github/scripts/dispatch-browser-reading-rows.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
# Every fixture is built inside `root=$(fixture …)`, and bash does not carry errexit into a
# command substitution on its own: without this, a tree that failed to build is used anyway and
# the case reports on whatever is there.
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
rows_script="$script_dir/dispatch-browser-reading-rows.sh"
workflow="$script_dir/../workflows/dispatch-browser.yaml"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# The merge job's call of the script, from the workflow's one `run:` line that names it.
step_run=$(grep -m 1 -E '^ +run: \.github/scripts/dispatch-browser-reading-rows\.sh( |$)' "$workflow" |
  sed -E 's/^ +run: //') || true
if [ -z "$step_run" ]; then
  echo "dispatch-browser-reading-rows.test.sh: $workflow holds no one-line" \
    "'run: .github/scripts/dispatch-browser-reading-rows.sh …' step to drive" >&2
  exit 1
fi

# fixture <name>: one case's root, with its $RUNNER_TEMP, its step summary, the script under
# test and the stand-in run-reads script. The stand-in writes the jobs in `jobs.fixture.json` to
# the out-file it is given, in the shape the real read writes: an array of {id, name,
# conclusion, leg}. The four shards ran and the alone leg was skipped, so it owes no blob. It
# carries a leg name, so only the script's filter on the conclusion keeps it out of the legs that
# owe one. Echoes the root. A repeated name is refused here rather than surfacing as one case
# reading another's files.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "two cases named $1" >&2
    return 1
  fi
  mkdir -p "$root/temp" "$root/.github/scripts"
  : > "$root/summary.md"
  cp "$rows_script" "$root/.github/scripts/dispatch-browser-reading-rows.sh"
  cat > "$root/.github/scripts/dispatch-browser-run-reads.sh" <<'STANDIN'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = jobs ] || { echo "stand-in run-reads: mode $1" >&2; exit 2; }
cp "$(dirname "$0")/jobs.fixture.json" "$2"
STANDIN
  chmod +x "$root/.github/scripts/dispatch-browser-run-reads.sh"
  cat > "$root/.github/scripts/jobs.fixture.json" <<'JOBS'
[
  {"id": 10, "name": "dispatch-browser / binaries", "conclusion": "success", "leg": null},
  {"id": 11, "name": "dispatch-browser / shard (1)", "conclusion": "success", "leg": "shard-1"},
  {"id": 12, "name": "dispatch-browser / shard (2)", "conclusion": "success", "leg": "shard-2"},
  {"id": 13, "name": "dispatch-browser / shard (3)", "conclusion": "success", "leg": "shard-3"},
  {"id": 14, "name": "dispatch-browser / shard (4)", "conclusion": "success", "leg": "shard-4"},
  {"id": 15, "name": "dispatch-browser / alone (webkit-alone)", "conclusion": "skipped", "leg": "alone-webkit-alone"},
  {"id": 16, "name": "dispatch-browser / merge", "conclusion": null, "leg": null}
]
JOBS
  echo "$root"
}

# leg <root> <leg> <rows>: one leg's blob report and row count as the selection step leaves
# them in selected-blobs. The script tests for the zip; it does not read it, so a line of text
# stands in for Playwright's blob.
leg() {
  mkdir -p "$1/temp/selected-blobs/blob-report" "$1/temp/selected-blobs/rows"
  printf '%s\n' "$2" > "$1/temp/selected-blobs/blob-report/report-$2.zip"
  printf '%s\n' "$3" > "$1/temp/selected-blobs/rows/$2.txt"
}

# downloaded <root> <leg> <rows>: one leg's artifact as the download step leaves it, in a
# directory named for the artifact under $RUNNER_TEMP/blobs, before any selection.
downloaded() {
  local dir="$1/temp/blobs/dispatch-browser-blob-$2-1"
  mkdir -p "$dir/blob-report" "$dir/rows"
  printf '%s\n' "$2" > "$dir/blob-report/report-$2.zip"
  printf '%s\n' "$3" > "$dir/rows/$2.txt"
}

# merged <root> <expected> <unexpected> <flaky> <skipped>: the merged report's stats, which is
# all of report.json this script reads.
merged() {
  jq -n --argjson e "$2" --argjson u "$3" --argjson f "$4" --argjson s "$5" \
    '{stats: {expected: $e, unexpected: $u, flaky: $f, skipped: $s}}' > "$1/temp/report.json"
}

# run_rows <root> <download conclusion> <selection conclusion>: the merge job's call of the
# script, with its two conclusions filled in, run from that root with the five variables the
# runner sets and the token the step gives it. A call holding an expression this harness does
# not fill is refused rather than run with the expression's text as an argument. Sets `out` to
# the combined output and `status` to the exit code. Not a subshell, so both survive the call.
run_rows() {
  local cmd=$step_run
  cmd=${cmd//'${{ steps.download-blobs.conclusion }}'/$2}
  cmd=${cmd//'${{ steps.select-blobs.conclusion }}'/$3}
  case "$cmd" in
    *'${{'*)
      echo "the step's call holds an expression this harness does not fill: $cmd" >&2
      return 1
      ;;
  esac
  set +e
  out=$(cd "$1" && env RUNNER_TEMP="$1/temp" GITHUB_STEP_SUMMARY="$1/summary.md" GH_TOKEN=t \
    GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 GITHUB_RUN_ATTEMPT=1 bash -c "$cmd" 2>&1)
  status=$?
  set -e
}

errors() { grep -c '::error::' <<<"$out" || true; }
error_line() { grep '::error::' <<<"$out" || true; }
notice_line() { grep '::notice title=dispatch-browser rows::' <<<"$out" || true; }
table_rows() { grep -c -- "$2" "$1/summary.md" || true; }

echo "case: the download step never ran, as in run 44754426131 attempt 1"
# The job's time limit cancelled `bun install`, so the download and the selection were skipped
# and the steps that read their directories found nothing: no blob, no count, no merged report.
root=$(fixture skipped)
run_rows "$root" skipped skipped
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the download step and that it did not run" \
  "$(contains "$(error_line)" "the step \"Download every leg's blob report\" did not run (it concluded skipped)")"
check "and that this job made no merged report, since the merge step never ran" \
  "$(contains "$(error_line)" "so this job made no merged report: the step \"Merge the legs' blob reports into one report\" runs only after the selection succeeds\.")"
check "rather than a merged report short of rows" \
  "$(is "$(contains "$(error_line)" 'merged report is short')" false)"
check "naming the legs whose blob it did not read, without saying they sent none" \
  "$(contains "$(error_line)" 'read no blob report of: shard-1, shard-2, shard-3, shard-4, which does not say that those legs sent none\.')"
check "and, apart, the legs whose count it did not read" \
  "$(contains "$(error_line)" 'It read no row count of: shard-1, shard-2, shard-3, shard-4\.')"
check "while it does not say those legs sent no blob" \
  "$(is "$(contains "$(error_line)" 'no blob report arrived')" false)"
check "sending the reader to the job's log" \
  "$(contains "$(error_line)" "This job's log shows what stopped that step\.$")"
check "and naming no cause, which this job did not see" \
  "$(is "$(contains "$(error_line)" 'cancellation\|install\|limit\|failed before')" false)"
check "nor that no row failed, which this job did not read" \
  "$(is "$(contains "$(error_line)" 'no row of the suite failed')" false)"
check "nor a step that would say whether a row failed" \
  "$(is "$(contains "$(error_line)" 'verdict step')" false)"
check "the notice does not say the legs printed 0 rows" \
  "$(is "$(contains "$(notice_line)" 'against 0 printed')" false)"
check "the notice says this job has no merged report, rather than one holding unknown rows" \
  "$(contains "$(notice_line)" "rows::this job has no merged report to hold against the legs' counts: ")"
check "and does not say a merged report holds rows" \
  "$(is "$(contains "$(notice_line)" 'merged report holds')" false)"
check "and names each leg's count as not read" \
  "$(contains "$(notice_line)" 'counts: shard-1 not read, shard-2 not read, shard-3 not read, shard-4 not read$')"
check "the summary says the same" \
  "$(contains "$(cat "$root/summary.md")" "^This job has no merged report to hold against the legs' counts in the table above\.$")"
check "the summary table says neither each leg's count nor its blob was read" \
  "$(is "$(table_rows "$root" '| not read | not read |$')" 4)"
check "and holds no MISSING, which is the word for a blob the selection did not find" \
  "$(is "$(table_rows "$root" 'MISSING')" 0)"
check "the leg the lane skipped owes nothing and is not named" \
  "$(is "$(contains "$out" 'alone')" false)"

echo "case: the merge job's step passes the script both conclusions"
# The script is only as right as the call: a step that passed the download's conclusion alone
# could not tell a selection that never completed from a leg that sent nothing.
check "the call passes the download step's conclusion" \
  "$(contains "$step_run" '"${{ steps.download-blobs.conclusion }}"')"
check "and the selection step's" "$(contains "$step_run" '"${{ steps.select-blobs.conclusion }}"')"
check "and the selection step carries that id" \
  "$(contains "$(grep -A 1 "      - name: Take the newest attempt of each leg's blob report" "$workflow")" \
    'id: select-blobs')"

echo "case: the download step was cancelled with one leg's artifact already on the runner"
# The limit reached the download itself. The selection after it was skipped, so shard 2's
# artifact sits under blobs/ and no leg reached selected-blobs/. The table must not call it
# missing: this job did not read it.
root=$(fixture cancelled)
downloaded "$root" shard-2 40
run_rows "$root" cancelled skipped
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the download as cancelled before it completed, and no merged report" \
  "$(contains "$(error_line)" 'was cancelled before it completed (it concluded cancelled), so this job made no merged report')"
check "and every leg, since none reached this reading" \
  "$(contains "$(error_line)" 'read no blob report of: shard-1, shard-2, shard-3, shard-4,')"
check "while it does not say those legs sent no blob" \
  "$(is "$(contains "$(error_line)" 'no blob report arrived')" false)"
check "the table calls shard 2's blob not read, which it was not" \
  "$(is "$(table_rows "$root" '^| shard-2 | not read | not read |$')" 1)"

echo "case: the download succeeded and the selection failed"
# This reading reads only what the selection wrote, so a selection that failed leaves it
# nothing, though every artifact is on the runner.
root=$(fixture selection-failed)
for each in 1 2 3 4; do downloaded "$root" "shard-$each" 30; done
run_rows "$root" success failure
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the selection step, that it failed after the download, and no merged report" \
  "$(contains "$(error_line)" "the step \"Take the newest attempt of each leg's blob report\" failed after the download succeeded, so this job made no merged report")"
check "and the four legs, without saying they sent no blob" \
  "$(contains "$(error_line)" 'read no blob report of: shard-1, shard-2, shard-3, shard-4, which does not say that those legs sent none\.')"
check "while it does not say those legs sent no blob" \
  "$(is "$(contains "$(error_line)" 'no blob report arrived')" false)"
check "the table says neither each leg's count nor its blob was read" \
  "$(is "$(table_rows "$root" '| not read | not read |$')" 4)"
check "and holds no MISSING" "$(is "$(table_rows "$root" 'MISSING')" 0)"

echo "case: the download succeeded and the selection was skipped"
# A skipped step writes no line of its own, so the message sends the reader to the job's log.
root=$(fixture selection-skipped)
for each in 1 2 3 4; do downloaded "$root" "shard-$each" 30; done
run_rows "$root" success skipped
check "fails the step" "$(is "$status" 1)"
check "naming the selection step and that it did not run" \
  "$(contains "$(error_line)" "blob report\" did not run (it concluded skipped) after the download succeeded")"
check "sending the reader to the job's log" \
  "$(contains "$(error_line)" "This job's log shows what stopped that step\.$")"
check "the table says neither each leg's count nor its blob was read" \
  "$(is "$(table_rows "$root" '| not read | not read |$')" 4)"
check "and holds no MISSING" "$(is "$(table_rows "$root" 'MISSING')" 0)"

echo "case: the selection was cancelled after it had copied part of the legs"
# The selection copies each leg's blob, then its count. Cancelled after shard 1 whole and shard
# 2's blob, it leaves shard 2 with no count and shards 3 and 4 with neither.
root=$(fixture selection-cancelled-part)
for each in 1 2 3 4; do downloaded "$root" "shard-$each" 30; done
leg "$root" shard-1 30
leg "$root" shard-2 40
rm "$root/temp/selected-blobs/rows/shard-2.txt"
run_rows "$root" success cancelled
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the selection as cancelled after the download" \
  "$(contains "$(error_line)" 'was cancelled before it completed (it concluded cancelled) after the download succeeded')"
check "naming only the legs whose blob it did not read as such" \
  "$(contains "$(error_line)" 'read no blob report of: shard-3, shard-4, which')"
check "and, apart, the legs whose count it did not read" \
  "$(contains "$(error_line)" 'It read no row count of: shard-2, shard-3, shard-4\.')"
check "the table holds shard 1's count and blob" "$(is "$(table_rows "$root" '^| shard-1 | 30 | yes |$')" 1)"
check "and shard 2's blob without its count" \
  "$(is "$(table_rows "$root" '^| shard-2 | not read | yes |$')" 1)"
check "and neither for shards 3 and 4" "$(is "$(table_rows "$root" '| not read | not read |$')" 2)"
check "and no MISSING" "$(is "$(table_rows "$root" 'MISSING')" 0)"
check "the notice says this job has no merged report, with each leg's count as read" \
  "$(contains "$(notice_line)" "rows::this job has no merged report to hold against the legs' counts: shard-1 30, shard-2 not read, shard-3 not read, shard-4 not read$")"

echo "case: the selection was cancelled after it had copied every leg"
# Every blob and count is in front of this reading, but the merge step after the selection never
# ran, so there is no merged report to hold them against.
root=$(fixture selection-cancelled-whole)
for each in 1 2 3 4; do downloaded "$root" "shard-$each" 30; done
for each in 1 2 3 4; do leg "$root" "shard-$each" 30; done
run_rows "$root" success cancelled
check "fails the step" "$(is "$status" 1)"
check "naming the selection and that this job made no merged report" \
  "$(contains "$(error_line)" "blob report\" was cancelled before it completed (it concluded cancelled) after the download succeeded, so this job made no merged report")"
check "and no leg, since each leg's blob and count was read" \
  "$(is "$(contains "$(error_line)" 'blob report of\|row count of')" false)"

echo "case: an unknown conclusion is named as it was given"
root=$(fixture empty-conclusion)
run_rows "$root" "" ""
check "fails the step" "$(is "$status" 1)"
check "names the conclusion it was given" \
  "$(contains "$(error_line)" "did not run to its end (it concluded '')")"
check "and says nothing of blobs that did not arrive" \
  "$(is "$(contains "$(error_line)" 'no blob report arrived')" false)"

echo "case: the download and the selection succeeded and one leg uploaded no blob"
# The control for the cases above: where both steps ran and found no blob for a leg, the line
# that names the leg as having sent none is the true one, and it stays.
root=$(fixture success-one-missing)
leg "$root" shard-1 30
leg "$root" shard-2 40
leg "$root" shard-3 50
merged "$root" 118 0 0 2
run_rows "$root" success success
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the leg that sent no blob" \
  "$(contains "$(error_line)" "no blob report arrived from: shard-4; the merged report is short of those legs' rows$")"
check "and neither input step, both of which ran" \
  "$(is "$(contains "$(error_line)" 'the step ')" false)"
check "the table marks that leg's blob MISSING" \
  "$(is "$(table_rows "$root" '^| shard-4 | not read | MISSING |$')" 1)"
check "the notice counts the three legs read" \
  "$(contains "$(notice_line)" 'holds 120 rows, against 120 printed by the 3 legs whose count this job read')"

echo "case: the download and the selection succeeded and no leg uploaded a blob"
# The download succeeds with no artifact, the selection writes empty directories and exits 0, and
# the merge step fails with nothing to merge. There is no merged report to be short of rows.
root=$(fixture success-none)
run_rows "$root" success success
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming every leg as having sent no blob, and that this job has no merged report" \
  "$(contains "$(error_line)" '::error::no blob report arrived from: shard-1, shard-2, shard-3, shard-4; this job has no merged report$')"
check "rather than a merged report short of rows" \
  "$(is "$(contains "$(error_line)" 'short')" false)"
check "the table marks each leg's blob MISSING" "$(is "$(table_rows "$root" '| not read | MISSING |$')" 4)"
check "the notice says this job has no merged report" \
  "$(contains "$(notice_line)" "rows::this job has no merged report to hold against the legs' counts: shard-1 not read, shard-2 not read, shard-3 not read, shard-4 not read$")"

echo "case: the download and the selection succeeded, one leg uploaded no blob, and there is no merged report"
# The merge step failed or the job's limit stopped it. The leg that sent no blob is named; there
# is no merged report to be short of its rows.
root=$(fixture success-one-missing-no-report)
leg "$root" shard-1 30
leg "$root" shard-2 40
leg "$root" shard-3 50
run_rows "$root" success success
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the leg that sent no blob, and that this job has no merged report" \
  "$(contains "$(error_line)" '::error::no blob report arrived from: shard-4; this job has no merged report$')"
check "rather than a merged report short of rows" \
  "$(is "$(contains "$(error_line)" 'merged report is short')" false)"
check "the notice says the same of the report" \
  "$(contains "$(notice_line)" "rows::this job has no merged report to hold against the legs' counts: shard-1 30, shard-2 40, shard-3 50, shard-4 not read$")"

echo "case: the download failed"
root=$(fixture failure)
run_rows "$root" failure skipped
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the failed download" \
  "$(contains "$(error_line)" "the download of the legs' blob reports failed")"
check "and the four legs, and that this job made no merged report" \
  "$(contains "$(error_line)" 'read no blob report of: shard-1, shard-2, shard-3, shard-4, and made no merged report;')"
check "rather than a merged report short of rows" \
  "$(is "$(contains "$(error_line)" 'merged report is short')" false)"
check "saying this error is not a failed browser row" \
  "$(contains "$(error_line)" "This error is a failed read of the run's artifacts, not a failed browser row\.$")"
check "nor that no row failed, which this job did not read" \
  "$(is "$(contains "$(error_line)" 'no row of the suite failed')" false)"
check "the table says no blob was read, not that each is missing" \
  "$(is "$(table_rows "$root" '| not read | not read |$')" 4)"

echo "case: every owed blob arrived and the counts agree"
root=$(fixture all-in)
leg "$root" shard-1 30
leg "$root" shard-2 40
leg "$root" shard-3 50
leg "$root" shard-4 60
merged "$root" 170 3 4 3
run_rows "$root" success success
check "exits 0" "$(is "$status" 0)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"
check "the notice reads both counts" \
  "$(contains "$(notice_line)" 'holds 180 rows, against 180 printed by the legs (shard-1 30, shard-2 40, shard-3 50, shard-4 60)')"
check "and the summary says the same" \
  "$(contains "$(cat "$root/summary.md")" '^The merged report holds 180 rows, against 180 printed by the legs\.$')"
check "and marks each leg's blob held" "$(is "$(table_rows "$root" '| yes |$')" 4)"

echo "case: every owed blob arrived and the merge dropped rows"
root=$(fixture dropped)
leg "$root" shard-1 30
leg "$root" shard-2 40
leg "$root" shard-3 50
leg "$root" shard-4 60
merged "$root" 170 0 0 0
run_rows "$root" success success
check "fails the step" "$(is "$status" 1)"
check "naming the two counts" \
  "$(contains "$(error_line)" 'the merged report holds 170 rows and the legs printed 180')"
check "and the rows the merge dropped" "$(contains "$(error_line)" 'the merge dropped rows the legs ran$')"

echo "case: every owed blob arrived and the merged report holds more rows than the legs printed"
root=$(fixture more)
leg "$root" shard-1 30
leg "$root" shard-2 40
leg "$root" shard-3 50
leg "$root" shard-4 60
merged "$root" 190 0 0 0
run_rows "$root" success success
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the two counts" \
  "$(contains "$(error_line)" 'the merged report holds 190 rows and the legs printed 180')"
check "and the rows no leg counted" "$(contains "$(error_line)" "holds rows that no leg's log counted$")"
check "and not that the merge dropped rows" \
  "$(is "$(contains "$(error_line)" 'dropped')" false)"

echo "case: every owed blob arrived and one leg's count is absent"
# The merge holds every row; only shard 4's count is missing. Read as a short total, this would
# say the merge dropped rows it did not drop.
root=$(fixture count-absent)
leg "$root" shard-1 30
leg "$root" shard-2 40
leg "$root" shard-3 50
leg "$root" shard-4 60
rm "$root/temp/selected-blobs/rows/shard-4.txt"
merged "$root" 180 0 0 0
run_rows "$root" success success
check "fails the step" "$(is "$status" 1)"
check "prints exactly one ::error:: line" "$(is "$(errors)" 1)"
check "naming the leg whose count it did not read" \
  "$(contains "$(error_line)" 'but this job read no row count of: shard-4, so the merged report')"
check "and not saying the merge dropped rows" \
  "$(is "$(contains "$(error_line)" 'the merge dropped rows')" false)"

echo "case: every owed blob arrived and three legs' count files hold no number"
# The legs' suite scripts write the count file from the log's last "Running N tests" line, so a
# count file that holds no number is a log that printed no such line this job could read.
root=$(fixture count-empty)
leg "$root" shard-1 30
leg "$root" shard-2 40
leg "$root" shard-3 50
leg "$root" shard-4 60
: > "$root/temp/selected-blobs/rows/shard-2.txt"
: > "$root/temp/selected-blobs/rows/shard-3.txt"
: > "$root/temp/selected-blobs/rows/shard-4.txt"
merged "$root" 180 0 0 0
run_rows "$root" success success
check "fails the step" "$(is "$status" 1)"
check "naming those legs as unread" \
  "$(contains "$(error_line)" 'read no row count of: shard-2, shard-3, shard-4, so')"
check "and sending the reader to their own logs for the line the count comes from" \
  "$(contains "$(error_line)" "A leg's count is the N of the last \"Running N tests\" line its own log printed; the log of each leg named here shows whether it printed one\.$")"
check "and the table calls their counts not read, not 0" \
  "$(is "$(table_rows "$root" '^| shard-[234] | not read | yes |$')" 3)"
check "the notice counts the one leg read, as one leg" \
  "$(contains "$(notice_line)" 'holds 180 rows, against 30 printed by the 1 leg whose count this job read (shard-1 30, shard-2 not read, shard-3 not read, shard-4 not read)$')"

echo "case: every owed blob arrived and there is no merged report"
# The merge step failed, or the job's limit or cancellation skipped it; the job's log says which.
# This reading has nothing to hold the counts against, and says so in its notice.
root=$(fixture no-report)
leg "$root" shard-1 30
leg "$root" shard-2 40
leg "$root" shard-3 50
leg "$root" shard-4 60
run_rows "$root" success success
check "exits 0" "$(is "$status" 0)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"
check "the notice says this job has no merged report, with the legs' counts" \
  "$(contains "$(notice_line)" "rows::this job has no merged report to hold against the legs' counts: shard-1 30, shard-2 40, shard-3 50, shard-4 60$")"

echo "case: every owed blob arrived, one leg's count file holds no number, and there is no merged report"
# As in the case above, with nothing to hold the counts against. The unread count is then no
# failure of this step's, and the notice names it.
root=$(fixture no-report-count-empty)
leg "$root" shard-1 30
leg "$root" shard-2 40
leg "$root" shard-3 50
leg "$root" shard-4 60
: > "$root/temp/selected-blobs/rows/shard-4.txt"
run_rows "$root" success success
check "exits 0" "$(is "$status" 0)"
check "prints no ::error:: line" "$(is "$(errors)" 0)"
check "the notice says this job has no merged report, and names the count it did not read" \
  "$(contains "$(notice_line)" "rows::this job has no merged report to hold against the legs' counts: shard-1 30, shard-2 40, shard-3 50, shard-4 not read$")"

echo "case: every owed blob arrived and jq cannot sum the merged report's stats"
# Not JSON, JSON of another shape (an array), and a count that is a string each end jq with an
# error of its own. The step names the sum it could not make, and claims nothing of the report's
# syntax, which only the first of the three gets wrong.
for shape in not-json array string-count; do
  root=$(fixture "report-$shape")
  leg "$root" shard-1 30
  leg "$root" shard-2 40
  leg "$root" shard-3 50
  leg "$root" shard-4 60
  case "$shape" in
    not-json) printf 'not json\n' > "$root/temp/report.json" ;;
    array) printf '[]\n' > "$root/temp/report.json" ;;
    string-count)
      printf '{"stats": {"expected": "180", "unexpected": 0, "flaky": 0, "skipped": 0}}\n' \
        > "$root/temp/report.json"
      ;;
  esac
  run_rows "$root" success success
  check "$shape: fails the step" "$(is "$status" 1)"
  check "$shape: prints exactly one ::error:: line" "$(is "$(errors)" 1)"
  check "$shape: naming the stats jq could not sum, and the report" \
    "$(contains "$(error_line)" "::error::jq could not sum the stats of the merged report $root/temp/report.json (jq's own message is above)")"
  check "$shape: with jq's own message in the output" "$(contains "$out" '^jq: ')"
  check "$shape: and claiming nothing of the report's syntax" \
    "$(is "$(contains "$(error_line)" 'not JSON')" false)"
  check "$shape: the notice says this job could not sum the stats" \
    "$(contains "$(notice_line)" "rows::this job could not sum the merged report's stats to hold against the legs' counts: shard-1 30, ")"
done

echo "case: every owed blob arrived and the merged report's stats sum to no whole number"
# An empty report, a report with no stats and one whose stats sum to a fraction: jq sums each,
# to nothing, `null` and 179.5. A test on such a sum fails inside the comparison's `if`, where
# `set -e` does not reach, so the step passed having compared nothing. The notice and the summary
# are written before the errors, and give no row count for such a report.
for shape in empty null fraction; do
  root=$(fixture "stats-$shape")
  leg "$root" shard-1 30
  leg "$root" shard-2 40
  leg "$root" shard-3 50
  leg "$root" shard-4 60
  case "$shape" in
    empty)
      : > "$root/temp/report.json"
      sum_shown=''
      ;;
    null)
      printf '{}\n' > "$root/temp/report.json"
      sum_shown=null
      ;;
    fraction)
      merged "$root" 179.5 0 0 0
      sum_shown=179.5
      ;;
  esac
  run_rows "$root" success success
  check "$shape: fails the step" "$(is "$status" 1)"
  check "$shape: prints exactly one ::error:: line" "$(is "$(errors)" 1)"
  check "$shape: naming the sum that is not a whole number" \
    "$(contains "$(error_line)" "the merged report's stats sum to '$sum_shown', not a whole number of rows")"
  check "$shape: the notice gives no row count for the report" \
    "$(contains "$(notice_line)" "rows::the merged report's stats sum to '$sum_shown', so this job has no whole number of rows to hold against the legs' counts: shard-1 30, shard-2 40, shard-3 50, shard-4 60$")"
  check "$shape: nor says the report holds rows" \
    "$(is "$(contains "$(notice_line)" 'merged report holds')" false)"
  check "$shape: and the summary says the same" \
    "$(contains "$(cat "$root/summary.md")" "^The merged report's stats sum to '$sum_shown', so this job has no whole number of rows to hold against the legs' counts in the table above\.$")"
done

echo "case: the script given one conclusion"
root=$(fixture one-argument)
set +e
out=$(cd "$root" && env RUNNER_TEMP="$root/temp" GITHUB_STEP_SUMMARY="$root/summary.md" GH_TOKEN=t \
  GITHUB_REPOSITORY=o/r GITHUB_RUN_ID=41 GITHUB_RUN_ATTEMPT=1 \
  .github/scripts/dispatch-browser-reading-rows.sh success 2>&1)
status=$?
set -e
check "fails the step" "$(is "$status" 1)"
check "naming both conclusions it takes" \
  "$(contains "$out" '<download conclusion> <selection conclusion>. It was given: success$')"

summary "dispatch-browser-reading-rows.sh names what its inputs did"
