#!/usr/bin/env bash
# Tests for `harness_case_times.py`, the reader of the per-line times harness-pool.sh records: a
# harness's case lines are one row each, in log order, each from the harness's case line before
# it, or the harness's start, to its own; a `case:` head is a row, and a failing check under it
# takes its name; the `ok N - name`, `not ok` and `<name>: PASS` forms are cases too; a harness
# that exited other than 0 always has a failing harness row with its exit status, from its last
# case line to its end, however its cases went, and one that exited 0 has a passing one when it
# ended more than a poll after its last case line; a log with no case line is one harness row for
# the whole harness; a job that ran no pool wait step prints nothing, while every other job is
# still read, and one still running, or cancelled with its cancelled-job step run, counts as one
# that ran; a job whose pool held no harness, its `judged` 0, prints nothing either; and whatever
# the record cannot tell is an UNKNOWN row and exit 1, never a row left out: a job whose artifact
# is missing, holds no `judged` that is a whole number, or holds a number of entries other than
# its `judged`, a harness that never took a slot or has no exit status, records missing,
# unfinished by the stamper, torn or not adding up to the log, a record that is not a time and a
# line, and times that are no finite number or run backwards; one such row alone is enough for
# exit 1. With no UNKNOWN row, a failing row is exit 3, and only a run whose every row passed is
# exit 0. With --run it reads through gh, every page of 100 of the jobs and of the artifacts, and
# leaves no download behind.
#
# Every fixture is written here by hand, record by record, so each time and byte count a case
# checks is one it chose. The reader's `--run` path is driven through a stand-in `gh` on PATH
# that answers from files, so nothing here reaches the network.
# Everything is written under $TMPDIR (mktemp -d honours it), never /tmp by name.
#
# Run from anywhere: .github/scripts/harness_case_times.test.sh
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
reader="$script_dir/harness_case_times.py"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# entry <artifact dir> <NNN> <harness> <started> <exit> <wall> [<time> <line>]...: a harness's
# complete record. The stamper read the whole log, so .lines.end and .logbytes both hold the
# bytes its lines stand for, each line's bytes and the newline after it.
entry() {
  local d=$1 n=$2 bytes
  mkdir -p "$d"
  printf '%s\n' "$3" > "$d/$n.name"
  printf '%s\n' "$4" > "$d/$n.started"
  printf '%s\n' "$5" > "$d/$n.exit"
  printf '%s 0.1 0.0\n' "$6" > "$d/$n.seconds"
  shift 6
  : > "$d/$n.lines"
  while (($# >= 2)); do
    printf '%s\t%s\n' "$1" "$2" >> "$d/$n.lines"
    shift 2
  done
  bytes=$(cut -f2- "$d/$n.lines" | wc -c)
  printf '%s\n' "$((bytes))" > "$d/$n.lines.end"
  printf '%s\n' "$((bytes))" > "$d/$n.logbytes"
}

# sizes <artifact dir> <NNN> <lines.end> <logbytes>: sets the two byte counts of an entry.
sizes() {
  printf '%s\n' "$3" > "$1/$2.lines.end"
  printf '%s\n' "$4" > "$1/$2.logbytes"
}

# judged <artifact dir> <text>: the pool's judged file, the number of entries its wait found.
judged() {
  mkdir -p "$1"
  printf '%s\n' "$2" > "$1/judged"
}

# job <id> <name> [<step name> <conclusion>]...: one job of a jobs payload, its steps in order. A
# conclusion of `null` is written as JSON's null, which GitHub gives a step of a job still running.
job() {
  local id=$1 name=$2 steps="" conclusion
  shift 2
  while (($# >= 2)); do
    conclusion="\"$2\""
    if [[ $2 == null ]]; then
      conclusion=null
    fi
    steps+="${steps:+,}{\"name\":\"$1\",\"conclusion\":$conclusion}"
    shift 2
  done
  printf '{"id":%s,"name":"%s","steps":[%s]}' "$id" "$name" "$steps"
}

wait_step="Wait for the script harnesses and judge them"
cancel_step="Name the script harnesses a cancelled job left"

# read_run <argument>...: runs the reader; sets out (its stdout), err (its stderr) and status.
read_run() {
  status=0
  out=$("$reader" "$@" 2> "$work/err") || status=$?
  err=$(< "$work/err")
}

# pick <key>=<value> <key>...: the rows of $out whose first key has that value, each as its values
# of the other keys, tab-separated, a null as `null`. A line of $out that is not JSON prints as
# such, so it fails the check that reads it rather than ending this script.
pick() {
  python3 -c '
import json, sys

def show(value):
    return "null" if value is None else str(value)

key, value = sys.argv[1].split("=", 1)
for line in sys.stdin:
    if not line.strip():
        continue
    try:
        row = json.loads(line)
    except ValueError:
        print("not JSON: " + line.rstrip("\n"))
        continue
    if show(row.get(key)) == value:
        print("\t".join(show(row.get(k)) for k in sys.argv[2:]))
' "$@" <<< "$out"
}

# count_rows: the number of lines of $out.
count_rows() { grep -c . <<< "$out" || true; }

# strict_json: `ok` when every line of $out is JSON with no Infinity or NaN in it, which most
# readers of JSON refuse, else the first line that is not.
strict_json() {
  python3 -c '
import json, sys

def refuse(constant):
    raise ValueError(constant)

for line in sys.stdin:
    if not line.strip():
        continue
    try:
        json.loads(line, parse_constant=refuse)
    except ValueError:
        print(line.rstrip("\n"))
        sys.exit(0)
print("ok")
' <<< "$out"
}

# A run whose every record is complete. Its jobs payload is a list of two pages, as a reader of
# the API saves one.
m="$work/complete"
a="$m/artifacts"
mkdir -p "$a"
printf '[{"total_count":5,"jobs":[%s,%s]},{"total_count":5,"jobs":[%s,%s,%s]}]\n' \
  "$(job 101 'test (script harnesses)' 'Set up job' success "$wait_step" failure)" \
  "$(job 102 lint 'Set up job' success 'Run biome' success)" \
  "$(job 105 'test (no harness reached)' "$wait_step" success)" \
  "$(job 103 'test (cancelled)' "$wait_step" skipped "$cancel_step" null)" \
  "$(job 104 docs "$wait_step" skipped)" > "$m/jobs.json"
# The indented PASS and FAIL lines are test-lib.sh's, the third with the carriage return of a
# CRLF line.
entry "$a/harness-times-101" 000 .github/scripts/plain.test.sh 1764000000.250000000 1 4.0 \
  1764000001.000000 '  PASS: the first check' \
  1764000001.500000 'some output that is no case' \
  1764000003.250000 '  FAIL: the second check' \
  1764000003.500000 $'  PASS: the third check\r'
entry "$a/harness-times-101" 001 .github/scripts/blocks.test.sh 1764000000.500000000 1 3.0 \
  1764000001.000000 'case: the first block' \
  1764000001.250000 '  PASS: a check under it' \
  1764000002.000000 '  FAIL: a failing check' \
  1764000002.500000 'case: the second block'
# Lines read in one poll share its time, and a first line can be stamped in the microsecond its
# harness started: tap.test.sh's last line has the time of the line before it, and quiet.test.sh's
# first the time of its start. Neither is a time that runs backwards.
entry "$a/harness-times-101" 002 .github/scripts/tap.test.sh 1764000000.750000000 1 2.0 \
  1764000001.000000 'ok 1 - the tap pass' \
  1764000001.500000 'not ok 2 - the tap fail' \
  1764000001.750000 'the tail form: PASS' \
  1764000002.000000 'the failing tail: FAIL' \
  1764000002.000000 'ok'
# Its log's last line had no newline after it, so the log is one byte shorter than its records.
entry "$a/harness-times-101" 003 .github/scripts/quiet.test.sh 1764000000.000000000 0 2.5 \
  1764000000.000000 'building' \
  1764000002.000000 'done'
sizes "$a/harness-times-101" 003 13 13
entry "$a/harness-times-101" 004 .github/scripts/quiet-fail.test.sh 1764000000.000000000 3 1.5 \
  1764000001.000000 'it broke'
# It printed nothing and ended in the tenth of a second it started in.
entry "$a/harness-times-101" 005 .github/scripts/instant.test.sh 1764000000.000000000 0 0.0
entry "$a/harness-times-103" 000 .github/scripts/cancelled.test.sh 1764000010.000000000 143 1.0 \
  1764000010.500000 '  PASS: it ran before the cancel'
judged "$a/harness-times-101" 6
judged "$a/harness-times-103" 1
# Job 105's change reached none of its harness steps: its wait found no entry, and said so.
judged "$a/harness-times-105" 0
read_run --from-dir "$a" --jobs-json "$m/jobs.json"

echo "case: a harness's PASS: and FAIL: lines are one row each, in order, each from the case line before it to its own"
check "one row per case line, in log order, with its name and status, then the harness's failing row" "$(is "$(pick harness=.github/scripts/plain.test.sh case status kind)" $'the first check\tpass\tcase\nthe second check\tfail\tcase\nthe third check\tpass\tcase\nnull\tfail\tharness')"
check "the first starts at the harness's start and ends at its own line" "$(is "$(pick case='the first check' start end seconds)" $'1764000000.25\t1764000001.0\t0.75')"
check "the next starts at the case line before it, past the line that is no case" "$(is "$(pick case='the second check' start end seconds)" $'1764000001.0\t1764000003.25\t2.25')"
check "and the last at the one before it" "$(is "$(pick case='the third check' start end seconds)" $'1764000003.25\t1764000003.5\t0.25')"
check "a row is compact JSON with its keys in the schema's order" "$(is "$(sed -n 1p <<< "$out")" '{"job":101,"job_name":"test (script harnesses)","harness":".github/scripts/plain.test.sh","case":"the first check","status":"pass","start":1764000000.25,"end":1764000001.0,"seconds":0.75,"kind":"case"}')"
check "and a harness row has the harness's exit status after them" "$(is "$(sed -n 4p <<< "$out")" '{"job":101,"job_name":"test (script harnesses)","harness":".github/scripts/plain.test.sh","case":null,"status":"fail","start":1764000003.5,"end":1764000004.25,"seconds":0.75,"kind":"harness","exit":1}')"

echo "case: a case: head is a row of its own, and a failing check under it takes the head's name"
check "the head, a check that passed under it, the failing check by the head's name, the next head, then the harness's failing row" "$(is "$(pick harness=.github/scripts/blocks.test.sh case status start end)" $'the first block\tpass\t1764000000.5\t1764000001.0\na check under it\tpass\t1764000001.0\t1764000001.25\nthe first block\tfail\t1764000001.25\t1764000002.0\nthe second block\tpass\t1764000002.0\t1764000002.5\nnull\tfail\t1764000002.5\t1764000003.5')"

echo "case: the ok N - name, not ok N - name and <name>: PASS|FAIL forms are cases, and a bare ok is none"
check "each form is a row with its name and status, and the bare ok starts no harness row" "$(is "$(pick harness=.github/scripts/tap.test.sh case status seconds)" $'the tap pass\tpass\t0.25\nthe tap fail\tfail\t0.5\nthe tail form\tpass\t0.25\nthe failing tail\tfail\t0.25\nnull\tfail\t0.75')"

echo "case: a harness whose log has no case line is one row for the whole harness, with its exit status"
check "one harness row, from its start for its wall seconds, passed by its exit 0" "$(is "$(pick harness=.github/scripts/quiet.test.sh case status start end seconds kind exit)" $'null\tpass\t1764000000.0\t1764000002.5\t2.5\tharness\t0')"
check "and one that exited 3 failed" "$(is "$(pick harness=.github/scripts/quiet-fail.test.sh case status seconds kind exit)" $'null\tfail\t1.5\tharness\t3')"
check "and one that ended in no time still has its row, of no seconds" "$(is "$(pick harness=.github/scripts/instant.test.sh case status start end seconds kind exit)" $'null\tpass\t1764000000.0\t1764000000.0\t0.0\tharness\t0')"

echo "case: a job that ran no pool wait step prints no row, and the run's other jobs are still read"
check "no row for the job with no wait step" "$(is "$(pick job=102 kind)" "")"
check "nor for the one whose wait step was skipped" "$(is "$(pick job=104 kind)" "")"
check "the job whose cancelled-job step ran, its conclusion still null, is read after them" "$(is "$(pick job=103 harness case status exit)" $'.github/scripts/cancelled.test.sh\tit ran before the cancel\tpass\tnull\n.github/scripts/cancelled.test.sh\tnull\tfail\t143')"

echo "case: when every record is complete and a harness failed, it prints no UNKNOWN row and exits 3"
check "it exits 3" "$(is "$status" 3)"
check "no row is UNKNOWN" "$(is "$(pick kind=unknown kind)" "")"
check "and it printed all 19 case and harness rows" "$(is "$(count_rows)" 19)"

echo "case: a job whose pool held no harness, its judged 0 and no entry, prints no row, and the run's other jobs are still read"
check "no row for job 105" "$(is "$(pick job=105 kind)" "")"
check "job 103, after it in the payload, is still read" "$(is "$(pick job=103 case)" $'it ran before the cancel\nnull')"
read_run --from-dir "$a" --jobs-json "$m/jobs.json" --job 105
check "--job 105 alone prints no row and exits 0" "$(is "$status:$(count_rows)" 0:0)"

echo "case: --job reads only the jobs it names, a job with nothing to read says so, and a job the run lacks is a usage error"
read_run --from-dir "$a" --jobs-json "$m/jobs.json" --job 103
check "--job 103 prints that job's case and its failing harness row" "$(is "$(pick job=103 harness case)" $'.github/scripts/cancelled.test.sh\tit ran before the cancel\n.github/scripts/cancelled.test.sh\tnull')"
check "and no other" "$(is "$(count_rows)" 2)"
read_run --from-dir "$a" --jobs-json "$m/jobs.json" --job 102
check "--job 102, which ran no pool, prints no row and exits 0" "$(is "$status:$(count_rows)" 0:0)"
check "and says on stderr that there was no case to print" "$(contains "$err" 'no case to print')"
read_run --from-dir "$a" --jobs-json "$m/jobs.json" --job 999
check "--job 999, which the run has not, exits 2" "$(is "$status" 2)"
check "and names it" "$(contains "$err" 'the run has no job 999$')"

# A run whose records are missing or incomplete in every way the reader must name.
m="$work/broken"
a="$m/artifacts"
mkdir -p "$a/harness-times-203"
# Job 204 is still running: its wait step has no conclusion yet, and it has uploaded nothing. Job
# 209 was cancelled: its wait step never ran, its cancelled-job step did, and it uploaded nothing.
printf '{"total_count":9,"jobs":[%s,%s,%s,%s,%s,%s,%s,%s,%s]}\n' \
  "$(job 201 'test (lost artifact)' "$wait_step" success)" \
  "$(job 202 'test (torn records)' "$wait_step" failure)" \
  "$(job 203 'test (empty artifact)' "$wait_step" success)" \
  "$(job 204 'test (still running)' 'Set up job' success "$wait_step" null)" \
  "$(job 205 'test (lost part)' "$wait_step" success)" \
  "$(job 206 'test (judged not a number)' "$wait_step" success)" \
  "$(job 207 'test (no judged)' "$wait_step" success)" \
  "$(job 208 'test (times that are no times)' "$wait_step" success)" \
  "$(job 209 'test (cancelled, nothing uploaded)' "$wait_step" skipped "$cancel_step" success)" > "$m/jobs.json"
d="$a/harness-times-202"
entry "$d" 000 .github/scripts/before.test.sh 1764000100.000000000 0 1.0 \
  1764000100.500000 '  PASS: before the short read'
# Its records stand for 32 bytes of the log; the stamper says it read 60, the recorder that the
# log held 90.
entry "$d" 001 .github/scripts/short-read.test.sh 1764000100.000000000 1 5.0 \
  1764000100.500000 '  PASS: a line the stamper kept'
sizes "$d" 001 60 90
entry "$d" 002 .github/scripts/no-exit.test.sh 1764000100.000000000 0 1.0 \
  1764000100.500000 '  PASS: a case of a harness with no status'
rm "$d/002.exit"
entry "$d" 003 .github/scripts/after.test.sh 1764000100.000000000 0 1.0 \
  1764000100.750000 '  PASS: after the short read'
# The stamper and the recorder agree on a log of 99 bytes, but its records stand for 38.
entry "$d" 004 .github/scripts/lost-middle.test.sh 1764000100.000000000 0 1.0 \
  1764000100.250000 '  PASS: one record' \
  1764000100.500000 '  PASS: two record'
sizes "$d" 004 99 99
entry "$d" 005 .github/scripts/no-tab.test.sh 1764000100.000000000 0 1.0 \
  1764000100.250000 '  PASS: a record before it'
printf 'a record with no tab\n' >> "$d/005.lines"
entry "$d" 006 .github/scripts/no-end.test.sh 1764000100.000000000 0 1.0 \
  1764000100.250000 '  PASS: a case the stamper was killed after'
rm "$d/006.lines.end"
entry "$d" 007 .github/scripts/no-slot.test.sh 1764000100.000000000 0 1.0 \
  1764000100.250000 '  PASS: a case of a harness that never started'
rm "$d/007.started"
entry "$d" 008 .github/scripts/no-time.test.sh 1764000100.000000000 0 1.0
printf 'soon\t  PASS: a case with no time\n' > "$d/008.lines"
sizes "$d" 008 29 29
# The stamper and the recorder agree on the log's 27 bytes, its one line and the newline after
# it, but the record of that line in its .lines has no newline after it: the write of that record
# stopped part of the way.
entry "$d" 009 .github/scripts/torn.test.sh 1764000100.000000000 0 1.0
printf '1764000100.250000\t  PASS: a record cut short' > "$d/009.lines"
sizes "$d" 009 27 27
judged "$d" 10
# Its wait found three harnesses; two arrived, each complete.
entry "$a/harness-times-205" 000 .github/scripts/part-one.test.sh 1764000100.000000000 0 1.0 \
  1764000100.250000 '  PASS: a case of a part that arrived'
entry "$a/harness-times-205" 001 .github/scripts/part-two.test.sh 1764000100.000000000 0 1.0 \
  1764000100.250000 '  PASS: a case of the other part'
judged "$a/harness-times-205" 3
entry "$a/harness-times-206" 000 .github/scripts/judged-two.test.sh 1764000100.000000000 0 1.0 \
  1764000100.250000 '  PASS: a case beside a judged that is no number'
judged "$a/harness-times-206" two
entry "$a/harness-times-207" 000 .github/scripts/unjudged.test.sh 1764000100.000000000 0 1.0 \
  1764000100.250000 '  PASS: a case beside no judged'
# Each of job 208's harnesses is complete but for its times. The first's second record is earlier
# than its first, the second's first record earlier than its start, the third's time 400 digits
# long, past any float; the fourth's .started and the fifth's wall seconds are 400 digits too. The
# fifth's one case line is whole, but when the harness ended, and so what its last row holds, is
# not known.
d="$a/harness-times-208"
huge=$(printf '1%0399d' 0)
entry "$d" 000 .github/scripts/backwards.test.sh 1764000100.000000000 0 3.0 \
  1764000102.500000 '  PASS: a case stamped first' \
  1764000101.250000 '  PASS: a case stamped before it'
entry "$d" 001 .github/scripts/before-start.test.sh 1764000100.000000000 0 1.0 \
  1764000099.500000 '  PASS: a case stamped before its harness started'
entry "$d" 002 .github/scripts/infinite-stamp.test.sh 1764000100.000000000 0 1.0 \
  "$huge" '  PASS: a case stamped past any float'
entry "$d" 003 .github/scripts/infinite-start.test.sh "$huge" 0 1.0
entry "$d" 004 .github/scripts/infinite-wall.test.sh 1764000100.000000000 0 "$huge" \
  1764000100.250000 '  PASS: a case of a harness whose end is not known'
judged "$d" 5
read_run --from-dir "$a" --jobs-json "$m/jobs.json"

echo "case: a pool job whose artifact is absent is one UNKNOWN row for the job, and the exit status is 1"
check "one row for job 201, of no harness, UNKNOWN, with no times" "$(is "$(pick job=201 harness case status start end seconds kind)" $'null\tnull\tunknown\tnull\tnull\tnull\tunknown')"
check "its why names the artifact it looked for" "$(contains "$(pick job=201 why)" 'holds no directory harness-times-201')"
check "it exits 1" "$(is "$status" 1)"

echo "case: a harness whose stamper read fewer bytes than its log held is one UNKNOWN row naming both counts, and the job's other harnesses still print their cases"
check "one UNKNOWN row for short-read.test.sh and no case of it" "$(is "$(pick harness=.github/scripts/short-read.test.sh case status start kind)" $'null\tunknown\tnull\tunknown')"
check "its why names the bytes the stamper read and the bytes the log held" "$(contains "$(pick harness=.github/scripts/short-read.test.sh why)" 'read 60 bytes of its log, but its 001.logbytes says the log held 90$')"
check "the harness before it still prints its case, and the time after it" "$(is "$(pick harness=.github/scripts/before.test.sh case status kind)" $'before the short read\tpass\tcase\nnull\tpass\tharness')"
check "and the one after it too" "$(is "$(pick harness=.github/scripts/after.test.sh case status kind)" $'after the short read\tpass\tcase\nnull\tpass\tharness')"

echo "case: a harness whose .lines is torn, its last record with no newline after it, is one UNKNOWN row saying so"
check "one UNKNOWN row for torn.test.sh and no case of it" "$(is "$(pick harness=.github/scripts/torn.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why says its .lines ends in the middle of a record" "$(is "$(pick harness=.github/scripts/torn.test.sh why)" 'its 009.lines ends in the middle of a record, with no newline after its last one')"

echo "case: records that do not add up to the bytes the stamper read are one UNKNOWN row naming both counts"
check "one UNKNOWN row for lost-middle.test.sh" "$(is "$(pick harness=.github/scripts/lost-middle.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why names the bytes of the records and the bytes read" "$(contains "$(pick harness=.github/scripts/lost-middle.test.sh why)" 'stand for 38 bytes of its log, but its 004.lines.end says the stamper read 99$')"

echo "case: a harness with no .exit is one UNKNOWN row, and its cases are not printed"
check "one UNKNOWN row for no-exit.test.sh" "$(is "$(pick harness=.github/scripts/no-exit.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why names the missing file" "$(contains "$(pick harness=.github/scripts/no-exit.test.sh why)" 'no 002.exit')"

echo "case: a harness with no .started, one that never took a slot, is one UNKNOWN row"
check "one UNKNOWN row for no-slot.test.sh" "$(is "$(pick harness=.github/scripts/no-slot.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why names the missing file" "$(contains "$(pick harness=.github/scripts/no-slot.test.sh why)" 'no 007.started')"

echo "case: a harness whose stamper never wrote .lines.end is one UNKNOWN row"
check "one UNKNOWN row for no-end.test.sh" "$(is "$(pick harness=.github/scripts/no-end.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why names the missing file" "$(contains "$(pick harness=.github/scripts/no-end.test.sh why)" '006.lines.end is missing')"

echo "case: a record with no tab, or whose first field is not a time, makes its harness one UNKNOWN row"
check "one UNKNOWN row for no-tab.test.sh" "$(is "$(pick harness=.github/scripts/no-tab.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why names the record" "$(contains "$(pick harness=.github/scripts/no-tab.test.sh why)" 'record 2 of its 005.lines has no tab')"
check "one UNKNOWN row for no-time.test.sh" "$(is "$(pick harness=.github/scripts/no-time.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why names the field that is not a time" "$(contains "$(pick harness=.github/scripts/no-time.test.sh why)" "record 1 of its 008.lines begins 'soon', which is not a time")"

echo "case: a pool job whose artifact holds no entry and no judged is one UNKNOWN row for the job"
check "one row for job 203, of no harness, UNKNOWN" "$(is "$(pick job=203 harness case status kind)" $'null\tnull\tunknown\tunknown')"
check "its why says the artifact holds no entry" "$(contains "$(pick job=203 why)" 'harness-times-203 holds no entry of the pool and no judged file')"

echo "case: a job still running, whose wait step has no conclusion yet, and a cancelled job whose cancelled-job step ran each count as one that ran a pool"
check "one UNKNOWN row for job 204, which has no artifact yet" "$(is "$(pick job=204 harness status kind)" $'null\tunknown\tunknown')"
check "and one for job 209, whose wait step was skipped and which uploaded nothing" "$(is "$(pick job=209 harness status kind)" $'null\tunknown\tunknown')"

echo "case: an artifact whose judged says 3 and holds 2 entries is one UNKNOWN row for the job, and neither entry prints a case"
check "one row for job 205, of no harness, UNKNOWN" "$(is "$(pick job=205 harness case status kind)" $'null\tnull\tunknown\tunknown')"
check "its why names the 2 entries it holds and the 3 the wait found" "$(contains "$(pick job=205 why)" 'holds 2 entries of the pool, but its judged says the wait found 3$')"

echo "case: an artifact with entries and a judged that is not a whole number, or none, is one UNKNOWN row for the job"
check "one row for job 206, whose judged is 'two', of no harness, UNKNOWN" "$(is "$(pick job=206 harness case status kind)" $'null\tnull\tunknown\tunknown')"
check "its why says its judged is not a whole number" "$(contains "$(pick job=206 why)" 'harness-times-206 holds 1 entry of the pool and a judged file that is not a whole number')"
check "one row for job 207, which has no judged, of no harness, UNKNOWN" "$(is "$(pick job=207 harness case status kind)" $'null\tnull\tunknown\tunknown')"
check "its why says it has no judged" "$(contains "$(pick job=207 why)" 'harness-times-207 holds 1 entry of the pool and no judged file')"

echo "case: a record whose time is no finite number, is earlier than its harness's start or is earlier than the record before it makes its harness one UNKNOWN row"
check "one UNKNOWN row for backwards.test.sh, and no case of it" "$(is "$(pick harness=.github/scripts/backwards.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why names record 2, its time and the time of record 1" "$(contains "$(pick harness=.github/scripts/backwards.test.sh why)" 'record 2 of its 000.lines has the time 1764000101.250000, earlier than the time 1764000102.500000 of record 1 before it')"
check "one UNKNOWN row for before-start.test.sh" "$(is "$(pick harness=.github/scripts/before-start.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why names record 1, its time and the harness's start" "$(contains "$(pick harness=.github/scripts/before-start.test.sh why)" "record 1 of its 001.lines has the time 1764000099.500000, earlier than the harness's start 1764000100.000000 in its 001.started")"
check "one UNKNOWN row for infinite-stamp.test.sh" "$(is "$(pick harness=.github/scripts/infinite-stamp.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why names record 1 and says its time is no finite number" "$(contains "$(pick harness=.github/scripts/infinite-stamp.test.sh why)" 'record 1 of its 002.lines has a time of 400 digits, 10000000000000000000..., which is no finite number of seconds')"
check "one UNKNOWN row for infinite-start.test.sh, whose .started is no finite number" "$(is "$(pick harness=.github/scripts/infinite-start.test.sh case status kind why)" $'null\tunknown\tunknown\tits 003.started is not a finite number of seconds')"
check "one UNKNOWN row for infinite-wall.test.sh, whose wall seconds are no finite number, with its exit status and no case" "$(is "$(pick harness=.github/scripts/infinite-wall.test.sh case status kind exit why)" $'null\tunknown\tunknown\t0\tits 004.seconds gives no finite number of wall seconds, so when it ended is not known')"
check "every line it printed is JSON with no Infinity or NaN in it" "$(is "$(strict_json)" ok)"
check "and the run printed one row for each of the seven jobs whose record is lost or unreadable, each of the fifteen harnesses of jobs 202 and 208, and the time after the last case of the two of them that are whole" "$(is "$(count_rows)" 24)"

echo "case: a usage error exits 2 and says what is wrong"
read_run
check "with no argument it exits 2" "$(is "$status" 2)"
check "and prints its usage" "$(contains "$err" '^usage: harness_case_times.py ')"
read_run --run 7 --jobs-json "$m/jobs.json"
check "--jobs-json with --run exits 2" "$(is "$status" 2)"
check "and says --jobs-json goes with --from-dir" "$(contains "$err" 'jobs-json goes with --from-dir')"
read_run --from-dir "$a"
check "--from-dir with no --jobs-json exits 2" "$(is "$status" 2)"
read_run --run 7 --from-dir "$a" --jobs-json "$m/jobs.json"
check "--run with --from-dir exits 2" "$(is "$status" 2)"
check "and says they do not go together" "$(contains "$err" 'not allowed with argument')"

# A run of one job whose one harness lost its .lines.
m="$work/single"
a="$m/artifacts"
mkdir -p "$a"
printf '{"total_count":1,"jobs":[%s]}\n' "$(job 401 'test (one harness)' "$wait_step" success)" > "$m/jobs.json"
entry "$a/harness-times-401" 000 .github/scripts/no-lines.test.sh 1764000300.000000000 0 1.0 \
  1764000300.500000 '  PASS: a case whose record was lost'
rm "$a/harness-times-401/000.lines"
judged "$a/harness-times-401" 1
read_run --from-dir "$a" --jobs-json "$m/jobs.json"

echo "case: a harness whose .lines is missing is one UNKNOWN row naming it"
check "one UNKNOWN row for no-lines.test.sh, and no case of it" "$(is "$(pick harness=.github/scripts/no-lines.test.sh case status kind)" $'null\tunknown\tunknown')"
check "its why names the missing file" "$(contains "$(pick harness=.github/scripts/no-lines.test.sh why)" 'its 000.lines is missing or cannot be read')"

echo "case: a run whose one job's one harness is UNKNOWN exits 1, its one row printed"
check "it exits 1" "$(is "$status" 1)"
check "and printed exactly one row" "$(is "$(count_rows)" 1)"

# A run whose every record is complete, to read a harness's exit status and its end. Job 501's one
# harness passed two cases and was then stopped by its bound, as `timeout` stops one, so all it
# says of its failure and of the 898 seconds after its last case is its exit status and its wall
# seconds. Job 502's harness passed its last check and then exited 1, in the poll in which it
# ended: the recorder rounds its wall seconds to a tenth, so that line is past its end. Job 503's
# harnesses exited 0: the first ended two seconds after its last case line, the second 0.06 s
# after it, the third 0.05 s after it, one poll, and the fourth in the poll of its last line. Job
# 504 ran a pool and has no artifact.
m="$work/exits"
a="$m/artifacts"
mkdir -p "$a"
printf '{"total_count":4,"jobs":[%s,%s,%s,%s]}\n' \
  "$(job 501 'test (stopped by its bound)' "$wait_step" failure)" \
  "$(job 502 'test (failed at its end)' "$wait_step" failure)" \
  "$(job 503 'test (passed)' "$wait_step" success)" \
  "$(job 504 'test (lost artifact)' "$wait_step" success)" > "$m/jobs.json"
entry "$a/harness-times-501" 000 .github/scripts/bound.test.sh 1764000400.000000000 124 900.0 \
  1764000401.000000 '  PASS: the first check' \
  1764000402.000000 '  PASS: the second check'
judged "$a/harness-times-501" 1
entry "$a/harness-times-502" 000 .github/scripts/failed-at-end.test.sh 1764000410.000000000 1 2.0 \
  1764000412.030000 '  PASS: its last check'
judged "$a/harness-times-502" 1
entry "$a/harness-times-503" 000 .github/scripts/tail.test.sh 1764000500.000000000 0 3.0 \
  1764000501.000000 '  PASS: the only check'
entry "$a/harness-times-503" 001 .github/scripts/poll-past.test.sh 1764000530.000000000 0 2.0 \
  1764000531.940000 '  PASS: a check 0.06 s before its end'
entry "$a/harness-times-503" 002 .github/scripts/poll-end.test.sh 1764000510.000000000 0 2.0 \
  1764000511.950000 '  PASS: a check one poll before its end'
entry "$a/harness-times-503" 003 .github/scripts/line-end.test.sh 1764000520.000000000 0 2.0 \
  1764000522.040000 '  PASS: a check in the poll of its end'
judged "$a/harness-times-503" 4

echo "case: a harness its bound stopped after two passing cases prints both, then a failing row from its last case line to its end with its exit status, and the reader exits 3"
read_run --from-dir "$a" --jobs-json "$m/jobs.json" --job 501
check "two passing case rows, then a harness row of the 898 s after the second, failed by its exit 124" "$(is "$(pick harness=.github/scripts/bound.test.sh case status start end seconds kind exit)" $'the first check\tpass\t1764000400.0\t1764000401.0\t1.0\tcase\tnull\nthe second check\tpass\t1764000401.0\t1764000402.0\t1.0\tcase\tnull\nnull\tfail\t1764000402.0\t1764001300.0\t898.0\tharness\t124')"
check "it exits 3, though no case line failed" "$(is "$status" 3)"

echo "case: a harness that failed in the poll of its last case line still has its failing row, which ends no earlier than it starts"
read_run --from-dir "$a" --jobs-json "$m/jobs.json" --job 502
check "its passing case, then a harness row of no seconds at that line, failed by its exit 1" "$(is "$(pick harness=.github/scripts/failed-at-end.test.sh case status start end seconds kind exit)" $'its last check\tpass\t1764000410.0\t1764000412.03\t2.03\tcase\tnull\nnull\tfail\t1764000412.03\t1764000412.03\t0.0\tharness\t1')"
check "it exits 3" "$(is "$status" 3)"

read_run --from-dir "$a" --jobs-json "$m/jobs.json" --job 503
echo "case: a harness that exited 0 and ended more than a poll after its last case line has a passing row for that time, with its exit status"
check "two seconds after it, a row of those two seconds" "$(is "$(pick harness=.github/scripts/tail.test.sh case status start end seconds kind exit)" $'the only check\tpass\t1764000500.0\t1764000501.0\t1.0\tcase\tnull\nnull\tpass\t1764000501.0\t1764000503.0\t2.0\tharness\t0')"
check "0.06 s after it, a row of those 0.06 s" "$(is "$(pick harness=.github/scripts/poll-past.test.sh case status seconds kind exit)" $'a check 0.06 s before its end\tpass\t1.94\tcase\tnull\nnull\tpass\t0.06\tharness\t0')"

echo "case: a harness that exited 0 and ended within a poll of its last case line, or before it, has no row after it, and a run whose every row passed exits 0"
check "one poll after it, its case row alone" "$(is "$(pick harness=.github/scripts/poll-end.test.sh case status kind)" $'a check one poll before its end\tpass\tcase')"
check "before it, its case row alone" "$(is "$(pick harness=.github/scripts/line-end.test.sh case status kind)" $'a check in the poll of its end\tpass\tcase')"
check "the job printed its 4 case rows and the 2 harness rows, and exits 0" "$(is "$status:$(count_rows)" 0:6)"

echo "case: a run with an UNKNOWN row exits 1 though another row failed"
read_run --from-dir "$a" --jobs-json "$m/jobs.json"
check "it printed the failing rows of jobs 501 and 502" "$(is "$(pick status=fail job harness exit)" $'501\t.github/scripts/bound.test.sh\t124\n502\t.github/scripts/failed-at-end.test.sh\t1')"
check "and the UNKNOWN row of job 504" "$(is "$(pick job=504 kind)" unknown)"
check "it exits 1, not 3" "$(is "$status" 1)"

echo "case: --run reads through gh: every page of the jobs, the artifacts, one download a job, and leaves none behind"
g="$work/gh"
mkdir -p "$g/bin" "$g/fixture" "$g/tmp"
cat > "$g/bin/gh" << 'EOF'
#!/usr/bin/env bash
# A stand-in for gh: answers the reader's listings from files and its downloads from directories,
# and logs each call.
set -euo pipefail
printf '%s\n' "$*" >> "$GH_FIXTURE/calls"
case "$1 ${2:-}" in
  "api repos/o/r/actions/runs/7/jobs?filter=all&per_page=100&page="*) cat "$GH_FIXTURE/jobs-${2##*page=}.json" ;;
  "api repos/o/r/actions/runs/7/artifacts?per_page=100&page="*) cat "$GH_FIXTURE/artifacts-${2##*page=}.json" ;;
  "run download")
    if [[ $3 != 7 || $4 != -R || $5 != o/r || $6 != -n || $8 != -D || ! -d $GH_FIXTURE/$7 ]]; then
      echo "no valid artifacts found to download" >&2
      exit 1
    fi
    mkdir -p "$9"
    cp "$GH_FIXTURE/$7"/* "$9"/
    printf '%s\n' "$9" >> "$GH_FIXTURE/downloads"
    ;;
  *)
    echo "HTTP 404: Not Found (${2:-})" >&2
    exit 1
    ;;
esac
EOF
chmod +x "$g/bin/gh"
# A full first page of jobs that ran no pool, so the pool jobs are on the second.
jobs=""
for i in $(seq 1 100); do
  jobs+="${jobs:+,}$(job "$i" "job $i" 'Set up job' success)"
done
printf '{"total_count":103,"jobs":[%s]}\n' "$jobs" > "$g/fixture/jobs-1.json"
printf '{"total_count":103,"jobs":[%s,%s,%s]}\n' \
  "$(job 301 'test (read)' "$wait_step" success)" \
  "$(job 302 'test (expired)' "$wait_step" success)" \
  "$(job 303 'test (download fails)' "$wait_step" success)" > "$g/fixture/jobs-2.json"
# Artifacts on two pages: a full first page of the job's other artifacts and 301's, then the rest.
artifacts=""
for i in $(seq 1 99); do
  artifacts+="${artifacts:+,}{\"name\":\"playwright-report-$i\",\"expired\":false}"
done
printf '{"total_count":102,"artifacts":[%s,%s]}\n' "$artifacts" \
  '{"name":"harness-times-301","expired":false}' > "$g/fixture/artifacts-1.json"
printf '{"total_count":102,"artifacts":[%s,%s]}\n' \
  '{"name":"harness-times-302","expired":true}' \
  '{"name":"harness-times-303","expired":false}' > "$g/fixture/artifacts-2.json"
# Job 303's artifact is listed but has no directory here, so its download fails.
entry "$g/fixture/harness-times-301" 000 .github/scripts/remote.test.sh 1764000200.000000000 0 1.0 \
  1764000200.500000 '  PASS: read through gh'
judged "$g/fixture/harness-times-301" 1
TMPDIR="$g/tmp" GH_FIXTURE="$g/fixture" PATH="$g/bin:$PATH" read_run --run 7 --repo o/r
check "the job on the second page is read from its download" "$(is "$(pick job=301 harness case status)" $'.github/scripts/remote.test.sh\tread through gh\tpass\n.github/scripts/remote.test.sh\tnull\tpass')"
check "the job whose artifact expired is one UNKNOWN row saying so" "$(contains "$(pick job=302 kind why)" $'^unknown\tits artifact harness-times-302 has expired')"
check "the job whose download failed is one UNKNOWN row with gh's reason" "$(contains "$(pick job=303 kind why)" $'^unknown\tgh run download of its artifact harness-times-303 exited 1: no valid artifacts found to download$')"
check "it exits 1" "$(is "$status" 1)"
# A reader that never called gh left no log of calls; that fails the checks below rather than
# ending this script before its tally.
calls=$(cat "$g/fixture/calls" 2> /dev/null || true)
check "it asked for two pages of jobs, two of artifacts, then downloaded 301 and 303 and not the expired 302" "$(is "$(sed 's/ -D .*//' <<< "$calls")" $'api repos/o/r/actions/runs/7/jobs?filter=all&per_page=100&page=1\napi repos/o/r/actions/runs/7/jobs?filter=all&per_page=100&page=2\napi repos/o/r/actions/runs/7/artifacts?per_page=100&page=1\napi repos/o/r/actions/runs/7/artifacts?per_page=100&page=2\nrun download 7 -R o/r -n harness-times-301\nrun download 7 -R o/r -n harness-times-303')"
check "each download went to a directory of its own under the reader's temporary directory" "$(contains "$calls" "^run download 7 -R o/r -n harness-times-301 -D $g/tmp/harness-case-times\.[^/]*/harness-times-301\$")"
check "which it removed at its end" "$(is "$(ls -A "$g/tmp")" "")"
TMPDIR="$g/tmp" GH_FIXTURE="$g/fixture" PATH="$g/bin:$PATH" read_run --run 8 --repo o/r
check "a gh call that fails outright exits 2" "$(is "$status" 2)"
check "and says gh's reason" "$(contains "$err" 'exited 1: HTTP 404: Not Found')"
check "and prints no row" "$(is "$(count_rows)" 0)"
check "and leaves no temporary directory either" "$(is "$(ls -A "$g/tmp")" "")"

summary "harness_case_times.py"
