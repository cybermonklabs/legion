#!/usr/bin/env bash
# Tests for `artifact_download.py`: the artifact arrives whole however the storage holds back its
# bytes, each byte is kept once, and a storage that sends nothing fails the step by name at the
# time the job's budget leaves it, not before and not after.
#
# Each case starts artifact_download_fake.py, a stand-in for the REST API and the storage that
# misbehaves in one way, points the script at it through GITHUB_API_URL, and asserts on the
# script's status, its log, the files it extracted and the requests the fake logged. The
# artifact is a zip of 8 MB of random bytes, and the job's budget is a fixture workflow's
# `timeout-minutes: 1`, so --after sets the deadline: 60 - after seconds after the job's start,
# which the fake puts 2 s before the script's first request. `--silence 2` stands for the
# script's 30 s, so a case that waits out silent connections takes seconds, not minutes; one case
# runs the default and holds it.
#
# No check reads this test's own clock, which a busy machine moves. Each decides by what the
# script and the fake report on the one clock both run on: the deadline the script computed and
# the moment it gave up, against the times the fake gave. Each run of the script is held by
# `timeout` to twice the most its deadline can be, so a script that loses its deadline fails a
# named check instead of holding the test job until its budget.
#
# Run from anywhere: .github/scripts/artifact_download.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
download="$script_dir/artifact_download.py"
fake="$script_dir/artifact_download_fake.py"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"
# Each check reads $out, the script's output in its case; a check that fails prints it, so the log
# of a red run says what the script printed and not only which check it missed.
eval "lib_$(declare -f check)"
check() { lib_check "$@"; [ "$2" = true ] || sed 's/^/      | /' <<<"${out:-}"; }

work=$(mktemp -d)
fake_pid=
stop_fake() {
  if [ -n "$fake_pid" ]; then
    kill "$fake_pid" 2>/dev/null || true
    wait "$fake_pid" 2>/dev/null || true
    fake_pid=
  fi
}
trap 'stop_fake; rm -rf "$work"' EXIT
export PYTHONDONTWRITEBYTECODE=1

# 8 MB, so the script's eight ranges are each about a mebibyte: wide enough for an idle connection
# to take half of one (MIN_SPLIT is a quarter of a mebibyte).
mkdir "$work/src"
head -c 7000000 /dev/urandom > "$work/src/dispatch"
head -c 1000000 /dev/urandom > "$work/src/broker"
python3 - "$work/src" "$work/artifact.zip" <<'PY'
import os, sys, zipfile
src, out = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    for name in ("dispatch", "broker"):
        z.write(os.path.join(src, name), name)
PY
size=$(stat -c %s "$work/artifact.zip")
printf 'jobs:\n  shard:\n    timeout-minutes: 1\n' > "$work/workflow.yaml"

# alive <pid>: the process runs (kill -0 also answers for a child that ended and is not reaped).
alive() { kill -0 "$1" 2>/dev/null && ! grep -qs '^State:[[:space:]]*Z' "/proc/$1/status"; }

# start_fake <fake options...>: starts the stand-in and leaves its address in $api. The stand-in
# writes its port once it listens, so this waits for that and not for a time.
start_fake() {
  stop_fake
  rm -f "$work/port" "$work/fake.log"
  python3 "$fake" --zip "$work/artifact.zip" --port-file "$work/port" --name dispatch-e2e-binaries \
    "$@" 2> "$work/fake.log" &
  fake_pid=$!
  until [ -s "$work/port" ]; do
    alive "$fake_pid" || { echo "the fake ended before it wrote its port:"; cat "$work/fake.log"; exit 1; }
    sleep 0.05
  done
  api="http://127.0.0.1:$(cat "$work/port")"
}

# run_download <after> [script options...]: runs the script under test against the fake and
# leaves its output in $out and its status in $status. Its deadline is at most 60 - after seconds
# after its first request, so `timeout` holds it to twice that and 10 s, and a run that bound ends
# fails the case's first check by name. $attempt sets GITHUB_RUN_ATTEMPT (1 when unset), and
# $silence the --silence option (empty for the script's default).
run_download() {
  local after=$1 bound
  shift
  bound=$((2 * (60 - after) + 10))
  rm -rf "$work/dest"
  set +e
  # shellcheck disable=SC2086 # $silence is an option and its value, or nothing.
  out=$(env GH_TOKEN=fake-token GITHUB_REPOSITORY=owner/repo GITHUB_RUN_ID=11 \
    GITHUB_RUN_ATTEMPT="${attempt:-1}" RUNNER_NAME='GitHub Actions 7' RUNNER_TEMP="$work" GITHUB_API_URL="$api" \
    timeout -k 5 "$bound" python3 "$download" --name dispatch-e2e-binaries --dest "$work/dest" \
    --budget-of "$work/workflow.yaml:shard" --after "$after" ${silence---silence 2} "$@" 2>&1)
  status=$?
  set -e
  check "ends by itself, inside the test's ${bound} s bound (twice the deadline's most, and 10 s)" \
    "$( [ "$status" != 124 ] && [ "$status" != 137 ] && echo true || echo false)"
}

whole() { cmp -s "$work/dest/dispatch" "$work/src/dispatch" && cmp -s "$work/dest/broker" "$work/src/broker" && echo true || echo false; }
nothing_placed() { [ -e "$work/dest" ] && echo false || echo true; }
# stopped_before_limit: true when the script, failing, says it stopped before the job's own limit,
# the job's start the fake gave plus the 60 s budget.
stopped_before_limit() {
  awk -v s="$(sed -n 's/^artifact_download: stopped at [^ ]* (epoch \([0-9.]*\))$/\1/p' <<<"$out")" \
    -v m="$(($(fake_time job) + 60))" 'BEGIN { exit !(s != "" && s + 0 < m) }' && echo true || echo false
}
blob_requests() { grep -c 'blob request [0-9]* range' "$work/fake.log" || true; }
sent_total() { awk '/blob request [0-9]+ sent [0-9]+ bytes/ { s += $6 } END { print s + 0 }' "$work/fake.log"; }

# fake_time job|run|artifact: a time the fake gave, as an epoch on the runner's clock.
fake_time() {
  local field
  case $1 in
    job) field='job 99 started at epoch' ;;
    run) field='the run attempt at epoch' ;;
    artifact) field='the artifact at epoch' ;;
  esac
  sed -n "s/^fake: times on the runner's clock: .*$field \([0-9]*\).*/\1/p" "$work/fake.log" | head -n 1
}
# The epochs the script logged: the deadline it computed, the moment it gave up, and how long the
# API answer it took its start from took to arrive.
deadline_epoch() { sed -n 's/.*must have arrived by [^ ]* (epoch \([0-9.]*\)).*/\1/p' <<<"$out" | head -n 1; }
gave_up_epoch() { sed -n 's/.*gave up at [^ ]* (epoch \([0-9.]*\)).*/\1/p' <<<"$out" | head -n 1; }
answer_took() { sed -n 's/.*(the answer took \([0-9.]*\) s).*/\1/p' <<<"$out" | head -n 1; }

# deadline_from <job|run|artifact> <after>: true when the deadline the script computed is no later
# than that time of the fake's plus the 60 s budget less after, and no earlier than that less 3 s
# (the 2 s the script allows for two whole-second times, and the 1 s the Date header can trail
# the moment) and less the time the answer took.
deadline_from() {
  local base d l
  base=$(($(fake_time "$1") + 60 - $2))
  d=$(deadline_epoch)
  l=$(answer_took)
  awk -v d="$d" -v b="$base" -v l="${l:-x}" \
    'BEGIN { exit !(d != "" && l != "x" && d + 0 <= b && d + 0 >= b - 3 - l) }' && echo true || echo false
}
# gave_up_at_deadline: true when the script gave up no earlier than the deadline it computed, and
# before the job's own limit, the job's start the fake gave plus the 60 s budget.
gave_up_at_deadline() {
  local g d limit
  g=$(gave_up_epoch)
  d=$(deadline_epoch)
  limit=$(($(fake_time job) + 60))
  awk -v g="$g" -v d="$d" -v m="$limit" 'BEGIN { exit !(g != "" && d != "" && g + 0 >= d + 0 && g + 0 <= m) }' \
    && echo true || echo false
}
gave_up_before_deadline() {
  awk -v g="$(gave_up_epoch)" -v d="$(deadline_epoch)" 'BEGIN { exit !(g != "" && d != "" && g + 0 < d + 0) }' \
    && echo true || echo false
}

# The ranges the fake was asked for, and the ranges the script logged a whole line for (the
# range, the status and any Content-Range, the bytes kept, the time and how it ended): one line
# per request is when the two lists are the same.
fake_ranges() { sed -n "s/^fake: blob request [0-9]* range 'bytes=\([0-9]*-[0-9]*\)'$/\1/p" "$work/fake.log" | sort; }
logged_ranges() {
  sed -n 's/^artifact_download: request [0-9]*: bytes=\([0-9]*-[0-9]*\) answered [0-9]\{3\}\( bytes [0-9]*-[0-9]*\/[0-9]*\)\{0,1\}, [0-9,]* bytes in [0-9.]* s ([0-9.]* MB\/s): ..*$/\1/p' <<<"$out" | sort
}
every_request_logged() {
  local f l
  f=$(fake_ranges)
  l=$(logged_ranges)
  [ -n "$f" ] && [ "$f" = "$l" ] && echo true || echo false
}
lines_answered() { grep -c "^artifact_download: request [0-9]*: bytes=[0-9]*-[0-9]* answered $1[ ,]" <<<"$out" || true; }

echo "case: a storage that answers at once"
start_fake
run_download 20
check "exits 0" "$(is "$status" 0)"
check "extracts both files byte for byte" "$(whole)"
check "says the zip matches the digest its upload recorded" "$(contains "$out" 'matches the digest its upload recorded')"
check "matches job 99 on its runner, not another runner's job listed first nor a completed job on its runner" \
  "$(contains "$out" "job 99 'shard (1)', whose runner_name 'GitHub Actions 7' is this step's RUNNER_NAME")"
check "takes its deadline from that job's start on GitHub's clock and the job's budget: $(deadline_epoch) for $(fake_time job) + 40" \
  "$(deadline_from job 20)"
check "logs every range the storage was asked for, with its status, bytes and time: $(blob_requests) requests" \
  "$(every_request_logged)"
check "logs the whole transfer's rate" "$(contains "$out" "bytes in [0-9.]* s over [0-9]* requests on 8 connections ([0-9.]* MB/s)")"

echo "case: a storage that sends every byte, slowly"
# 8 MB at 400 kB/s over every connection together: about 20 s, all of it inside the 40 s left.
start_fake --rate 400000
run_download 20
check "exits 0" "$(is "$status" 0)"
check "extracts both files byte for byte" "$(whole)"
transfer=$(sed -n 's/.*bytes in \([0-9.]*\) s over [0-9]* requests on 8 connections.*/\1/p' <<<"$out")
check "waits the bytes out rather than a clock: its transfer took ${transfer:-no} s by its own count, at least 15" \
  "$(awk -v t="${transfer:-0}" 'BEGIN { exit !(t + 0 >= 15) }' && echo true || echo false)"

echo "case: one slow connection beside fast ones"
# The fake's first request sends 50 kB/s and every other one full speed. Read alone, its megabyte
# takes 20 s; an idle connection takes half of what it has left, and again while what is left is at
# least twice MIN_SPLIT, so the slow request ends short of the range it asked for.
start_fake --slow-first 50000
run_download 20
check "exits 0" "$(is "$status" 0)"
check "extracts both files byte for byte" "$(whole)"
asked=$(sed -n "s/^fake: blob request 1 range 'bytes=\([0-9]*-[0-9]*\)'$/\1/p" "$work/fake.log")
kept=$(sed -n "s/^artifact_download: request [0-9]*: bytes=${asked:-none} answered 206 [^,]*, \([0-9,]*\) bytes in .*: its range is whole$/\1/p" <<<"$out" | tr -d ,)
check "the slow connection does not set the pace: of its range ${asked:-none}, it kept ${kept:-no} bytes and others read the rest" \
  "$(awk -v r="${asked:-}" -v k="${kept:-}" 'BEGIN { split(r, a, "-"); exit !(r != "" && k != "" && k + 0 < a[2] - a[1] + 1) }' && echo true || echo false)"
check "splits the slow range: more requests than the 8 ranges" "$( [ "$(blob_requests)" -gt 8 ] && echo true || echo false)"

echo "case: connections that go silent part way through their range"
# Each connection sends 300,000 bytes of its range and then nothing: every range is read again
# from where it stopped, on a new connection, after 2 s of silence.
start_fake --stall-after 300000
run_download 20
check "exits 0" "$(is "$status" 0)"
check "extracts both files byte for byte" "$(whole)"
check "reads ranges again on new connections: more requests than the 8 ranges" \
  "$( [ "$(blob_requests)" -gt 8 ] && echo true || echo false)"
check "asks for each byte once: the storage sent $(sent_total) for $size" "$(is "$(sent_total)" "$size")"
check "counts the reads that ended early" "$(contains "$out" '[1-9][0-9]* ended early')"

echo "case: connections that go silent, with the script's default silence bound"
# Each connection sends 900,000 of its range's 1,000,038 bytes and then nothing. With the default
# 30 s, every one ends 30 s into its silence and its last 100,038 bytes are read on a new one,
# inside the 50 s left.
start_fake --stall-after 900000
silence='' run_download 10
check "exits 0" "$(is "$status" 0)"
check "extracts both files byte for byte" "$(whole)"
silent=$(sed -n 's/^artifact_download: request [0-9]*: .* bytes in \([0-9.]*\) s (.*): TimeoutError: timed out$/\1/p' <<<"$out" | sort -n)
check "ends a silent connection after the default 30 s of silence: $(wc -w <<<"$silent") ended so, after $(head -n 1 <<<"$silent") to $(tail -n 1 <<<"$silent") s" \
  "$(awk -v first="$(head -n 1 <<<"$silent")" -v last="$(tail -n 1 <<<"$silent")" 'BEGIN { exit !(first != "" && first + 0 >= 29 && last + 0 < 40) }' && echo true || echo false)"

echo "case: a storage that reads every request and never answers"
# The deadline is 60 - 50 = 10 s after the job's start.
start_fake --silent
run_download 50
check "exits 1" "$(is "$status" 1)"
check "fails by name: no usable byte arrived, and no request had an answer" \
  "$(contains "$out" '::error::artifact_download: the artifact service sent no usable byte of dispatch-e2e-binaries .* of which .*\. The service did not answer; nothing the change under test did is read here')"
check "takes its deadline from the job's start: $(deadline_epoch) for $(fake_time job) + 10" "$(deadline_from job 50)"
check "gives up at its deadline, $(gave_up_epoch) for $(deadline_epoch), and before the job's own limit" "$(gave_up_at_deadline)"
check "extracts nothing" "$(nothing_placed)"

echo "case: a storage nothing listens on"
start_fake --refuse
run_download 50
check "exits 1" "$(is "$status" 1)"
check "fails by name and names the refusal" "$(contains "$out" 'sent no usable byte of dispatch-e2e-binaries.* had no answer (the last: ConnectionRefusedError')"
check "gives up at its deadline, $(gave_up_epoch) for $(deadline_epoch), and before the job's own limit" "$(gave_up_at_deadline)"

echo "case: a storage too slow for the time the job's budget leaves"
# 100 kB/s for the 10 s left: about a tenth of the zip arrives.
start_fake --rate 100000
run_download 50
check "exits 1" "$(is "$status" 1)"
check "fails by name with the bytes that did arrive" "$(contains "$out" 'dispatch-e2e-binaries did not arrive in the [0-9]* s this job.s budget left it: [0-9,]* of [0-9,]* bytes')"
check "gives up at its deadline, $(gave_up_epoch) for $(deadline_epoch), and before the job's own limit" "$(gave_up_at_deadline)"

echo "case: a storage that answers a range with the whole zip"
# A 200 to a ranged request: no range is read, every request is logged with its 200, and the
# step fails at the deadline by name.
start_fake --ignore-range
run_download 50
check "exits 1" "$(is "$status" 1)"
check "logs every request, each with its 200: $(lines_answered 200) of $(blob_requests)" \
  "$(every_request_logged)"
check "each such line says the 200 is refused" \
  "$(is "$(grep -c 'answered 200, 0 bytes in .*: ConnectionError: the storage answered a range request with HTTP 200, the whole zip and not the range$' <<<"$out" || true)" "$(blob_requests)")"
check "fails by name: the storage answered with the whole zip, not the range" \
  "$(contains "$out" "::error::artifact_download: the artifact service sent no usable byte of dispatch-e2e-binaries .* of which [0-9]* were answered HTTP 200 with the whole zip, not the range asked for .*No answer the storage gave could be used")"
check "does not say the service did not answer" "$(is "$(contains "$out" 'The service did not answer')" false)"

echo "case: a storage that answers each range with other bytes than those asked for"
# Every 206 starts one byte past the start asked for: the script refuses it, so no byte of it is
# kept, and the step fails at the deadline by name.
start_fake --misalign
run_download 50
check "exits 1" "$(is "$status" 1)"
check "fails by name: the storage answered other bytes than those asked for" \
  "$(contains "$out" "::error::artifact_download: the artifact service sent no usable byte of dispatch-e2e-binaries .* of which [0-9]* were answered 206 with other bytes than those asked for (the last: ConnectionError: the storage answered bytes [0-9]*- with 'bytes [0-9]*-[0-9]*/[0-9]*').*No answer the storage gave could be used")"
check "gives up at its deadline, $(gave_up_epoch) for $(deadline_epoch), and before the job's own limit" "$(gave_up_at_deadline)"
check "extracts nothing" "$(nothing_placed)"

echo "case: a storage that answers its first range with other bytes, then as asked"
# The first request's 206 starts one byte late: it is refused, nothing of it is kept, and its range
# is read again on a new connection.
start_fake --misalign-first 1
run_download 20
check "exits 0" "$(is "$status" 0)"
check "extracts both files byte for byte" "$(whole)"
check "refuses that one answer by name: $(grep -c ': ConnectionError: the storage answered bytes [0-9]*- with ' <<<"$out" || true)" \
  "$(is "$(grep -c ': ConnectionError: the storage answered bytes [0-9]*- with ' <<<"$out" || true)" 1)"

echo "case: a storage that answers its first two ranges with 503"
# The shape run 44758704517's storage showed: two 503s, each range read again and answered 206.
start_fake --fail-first 2 --fail-status 503
run_download 20
check "exits 0" "$(is "$status" 0)"
check "extracts both files byte for byte" "$(whole)"
check "logs the two 503s: $(lines_answered 503)" "$(is "$(lines_answered 503)" 2)"

echo "case: a storage that answers every range with 503"
start_fake --blob-status 503
run_download 50
check "exits 1" "$(is "$status" 1)"
check "fails by name: the storage answered HTTP 503" \
  "$(contains "$out" "::error::artifact_download: the artifact service sent no usable byte of dispatch-e2e-binaries .* of which [0-9]* were answered HTTP 503 .*No answer the storage gave could be used")"
check "waits the 503s out to its deadline, $(gave_up_epoch) for $(deadline_epoch), and before the job's own limit" "$(gave_up_at_deadline)"

echo "case: a signed URL the storage refuses twice"
start_fake --refuse-urls 2
run_download 20
check "exits 0" "$(is "$status" 0)"
check "extracts both files byte for byte" "$(whole)"
check "asks the API for a fresh URL after a refusal" "$( [ "$(grep -c 'artifacts/4242/zip' "$work/fake.log")" -ge 2 ] && echo true || echo false)"

echo "case: a storage that refuses every fresh URL"
start_fake --blob-status 403
run_download 20
check "exits 1" "$(is "$status" 1)"
check "fails at once, by name" "$(contains "$out" 'the storage refused 3 freshly signed URLs in a row with HTTP 403')"
check "does not wait for the deadline: gave up at $(gave_up_epoch), before its deadline $(deadline_epoch)" "$(gave_up_before_deadline)"

echo "case: signed URLs that live 3 s"
# Each connection sends 300,000 bytes and then nothing, so a range is read again every 2 s or so
# and the read outlives the URL it began on. --url-life 3 has the script sign again after 1.5 s.
start_fake --stall-after 300000 --url-life 3
run_download 20 --url-life 3
check "exits 0" "$(is "$status" 0)"
check "extracts both files byte for byte" "$(whole)"
check "starts no request on an expired URL" "$(is "$(grep -c 'refused: its URL is expired' "$work/fake.log" || true)" 0)"
check "signs the URL again by its age: $(grep -c 'artifacts/4242/zip' "$work/fake.log") signings" \
  "$( [ "$(grep -c 'artifacts/4242/zip' "$work/fake.log")" -ge 3 ] && echo true || echo false)"

echo "case: a run that holds no artifact of that name"
start_fake --missing
run_download 20
check "exits 1" "$(is "$status" 1)"
check "fails at once, by name" "$(contains "$out" "run 11 holds no unexpired artifact named 'dispatch-e2e-binaries'")"

echo "case: a zip whose digest is not the one its upload recorded"
start_fake --digest sha256:0000
run_download 20
check "exits 1" "$(is "$status" 1)"
check "fails by name" "$(contains "$out" 'not the sha256:0000 its upload recorded')"
check "extracts nothing" "$(nothing_placed)"

echo "case: a zip that matches its digest and cannot be extracted"
# Byte 1000, inside the first file's data, is changed in what the upload recorded: the digest
# matches, and the file's CRC-32 fails once its bytes have been written.
start_fake --corrupt-at 1000
run_download 20
check "exits 1" "$(is "$status" 1)"
check "fails by name" "$(contains "$out" 'the zip matched its digest but could not be extracted into .*, which holds nothing of it: BadZipFile: Bad CRC-32')"
check "places no file, not even the part of one" "$(nothing_placed)"
check "leaves nothing of the extraction beside --dest" "$(is "$(find "$work" -maxdepth 1 -name '.dest-*' | wc -l)" 0)"

echo "case: an artifact whose upload recorded no digest"
start_fake --no-digest
run_download 20
check "exits 1" "$(is "$status" 1)"
check "fails at once, by name" "$(contains "$out" 'the API records no digest for artifact 4242, so its bytes could not be checked')"
check "reads no byte of it: $(blob_requests) storage requests" "$(is "$(blob_requests)" 0)"
check "extracts nothing" "$(nothing_placed)"

echo "case: a jobs API that fails"
# The artifact was made 10 s before the job's start and the run attempt began 60 s before it, so
# the artifact's time stands for the start.
start_fake --jobs-status 500
run_download 20
check "exits 0" "$(is "$status" 0)"
check "says it could not read the job's start and takes the latest time GitHub records before it" \
  "$(contains "$out" "the job's start could not be read (.*HTTP 500.*), so the latest time GitHub records before it stands for it: artifact 4242's created_at")"
check "takes its deadline from the artifact's time: $(deadline_epoch) for $(fake_time artifact) + 40" "$(deadline_from artifact 20)"
check "extracts both files byte for byte" "$(whole)"

echo "case: a jobs API that fails, and a storage that never answers"
# The deadline is the artifact's time, 10 s before the job's start, plus 60 - 40 s: 10 s after
# the job's start.
start_fake --jobs-status 500 --silent
run_download 40
check "exits 1" "$(is "$status" 1)"
check "takes its deadline from the artifact's time: $(deadline_epoch) for $(fake_time artifact) + 20" "$(deadline_from artifact 40)"
check "gives up at that deadline, $(gave_up_epoch) for $(deadline_epoch), and before the job's own limit" "$(gave_up_at_deadline)"

echo "case: a re-run whose jobs API fails"
# Attempt 2 reuses an artifact attempt 1 made 900 s before; its own run attempt began 20 s before
# the job, so that time stands for the start.
start_fake --attempt 2 --jobs-status 500 --artifact-ago 900 --run-ago 20
attempt=2 run_download 20
check "exits 0" "$(is "$status" 0)"
check "takes the run attempt's start, the later of the two" \
  "$(contains "$out" "stands for it: run attempt 2's run_started_at")"
check "takes its deadline from the run attempt's start: $(deadline_epoch) for $(fake_time run) + 40" "$(deadline_from run 20)"
check "extracts both files byte for byte" "$(whole)"

echo "case: a re-run, whose jobs list for attempt 1 holds the job completed"
start_fake --attempt 2
attempt=2 run_download 20
check "exits 0" "$(is "$status" 0)"
check "reads attempt 2's jobs" "$(contains "$(cat "$work/fake.log")" 'GET /repos/owner/repo/actions/runs/11/attempts/2/jobs')"
check "matches job 99 in attempt 2" "$(contains "$out" "job 99 'shard (1)', whose runner_name 'GitHub Actions 7' is this step's RUNNER_NAME")"
check "takes its deadline from that job's start: $(deadline_epoch) for $(fake_time job) + 40" "$(deadline_from job 20)"

echo "case: two in-progress jobs on this runner's name"
start_fake --twin-job
run_download 20
check "exits 0" "$(is "$status" 0)"
check "takes neither, by name" "$(contains "$out" "2 in-progress jobs of attempt 1 ran on runner 'GitHub Actions 7' (97, 99), so which one this is cannot be told")"
check "takes its deadline from the artifact's time: $(deadline_epoch) for $(fake_time artifact) + 40" "$(deadline_from artifact 20)"

echo "case: an API whose clock runs 60 s ahead of the runner's"
# Every time the API gives, its Date header included, is 60 s ahead: the job's age on GitHub's
# clock is the same, so the deadline on the runner's is too.
start_fake --clock-offset 60 --silent
run_download 50
check "exits 1" "$(is "$status" 1)"
check "takes its deadline from the job's age on GitHub's clock: $(deadline_epoch) for $(fake_time job) + 10" "$(deadline_from job 50)"
check "gives up at its deadline, $(gave_up_epoch) for $(deadline_epoch), and before the job's own limit" "$(gave_up_at_deadline)"

echo "case: a jobs API that gives a start after this step began"
# Job 99's start is 30 s after the script's first request, after the script began, which cannot be.
start_fake --started-ago -30
run_download 20
check "exits 0" "$(is "$status" 0)"
check "says the start falls after this step began" "$(contains "$out" "the jobs API gives job 99 'shard (1)' a start of .*, after this step began")"
check "takes its deadline from the artifact's time: $(deadline_epoch) for $(fake_time artifact) + 40" "$(deadline_from artifact 20)"
check "extracts both files byte for byte" "$(whole)"

echo "case: a job whose budget the earlier steps have spent"
start_fake --started-ago 50
run_download 20
check "exits 1" "$(is "$status" 1)"
check "fails at once, by name" "$(contains "$out" "leaves the download no time: the steps after it take 20 s")"

echo "case: a jobs API that sends its answer one byte a second"
# The read is held to EARLY_READ's 4 s, its body included, and the step goes on without the start.
start_fake --trickle jobs
run_download 20
check "exits 0" "$(is "$status" 0)"
check "ends the jobs read at its own bound, by name" \
  "$(contains "$out" "the job's start could not be read (.*the deadline passed while the answer was still arriving.*), so the latest time GitHub records before it stands for it")"
check "extracts both files byte for byte" "$(whole)"

echo "case: an artifacts API that sends its answer one byte a second"
# The listing is held to the download's deadline, 10 s after the job's start, its body included.
start_fake --trickle artifacts
run_download 50
check "exits 1" "$(is "$status" 1)"
check "fails by name at the deadline" \
  "$(contains "$out" "the API never listed the run's artifacts before the deadline (.*the deadline passed while the answer was still arriving)")"

echo "case: an API that sends every answer one byte a second"
# The job started 10 s before the script's first request, so its own limit is 50 s after it. The jobs
# read is held to 4 s, and the artifacts and run-attempt reads of the fallback to 4 s together.
start_fake --trickle all --started-ago 10
run_download 20
check "exits 1" "$(is "$status" 1)"
check "fails by name: neither the job's start nor the run's artifacts were read" \
  "$(contains "$out" "the job's start could not be read (.*the deadline passed while the answer was still arriving.*), and the API never listed the run's artifacts either")"
check "stops before the job's own limit, $(($(fake_time job) + 60)): $(sed -n 's/^artifact_download: stopped at [^ ]* (epoch \([0-9.]*\))$/\1/p' <<<"$out")" \
  "$(stopped_before_limit)"

echo "case: a step that passes no token"
start_fake
set +e
out=$(env -u GH_TOKEN GITHUB_REPOSITORY=owner/repo GITHUB_RUN_ID=11 GITHUB_API_URL="$api" \
  timeout -k 5 30 python3 "$download" --name dispatch-e2e-binaries --dest "$work/dest" \
  --budget-of "$work/workflow.yaml:shard" --after 20 2>&1)
status=$?
set -e
check "exits 2" "$(is "$status" 2)"
check "names what is missing" "$(contains "$out" 'GH_TOKEN not set')"

stop_fake
summary "artifact_download.py"
