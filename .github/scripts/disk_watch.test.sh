#!/usr/bin/env bash
# Tests for `disk_watch.py`: `start` writes the state file and leaves a sampler that outlives it,
# the sampler's least value falls when the watched file system fills, it writes a tick of the
# series every second and keeps the least memory and the greatest load it has seen, `report`
# stops the sampler and prints the series, the three worst readings and the four numbers in that
# order, and it exits 0 whether the margin is ok or short, when a reading was never made, when
# there is no series file and when there is no state file at all.
#
# `start` runs here with a PATH holding python3 alone, so neither `go` nor `docker` is on it:
# that is the runner this script must work on too (no Go toolchain, no Docker daemon, no
# database), and it proves the two tools are named and skipped rather than failing the step. The
# two margins are driven from state files written here by hand, so the numbers in the line are
# chosen rather than measured; the falling-least case is measured, against 64 MiB of real writes.
# Everything is written under $TMPDIR (mktemp -d honours it), never /tmp by name.
#
# Run from anywhere: .github/scripts/disk_watch.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
watch="$script_dir/disk_watch.py"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
pid=""
cleanup() {
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

# A PATH with python3 and nothing else on it: no go, no docker. The script's own shebang finds
# python3 through it, and the sampler it starts is started by sys.executable, an absolute path.
mkdir "$work/bin"
ln -s "$(command -v python3)" "$work/bin/python3"

# run_watch <arguments...>: runs the script under test with that bare PATH, leaving its merged
# output in $out and its status in $status.
run_watch() {
  set +e
  out=$(env -i PATH="$work/bin" HOME="$work" TMPDIR="$work" "$watch" "$@" 2>&1)
  status=$?
  set -e
}

# field <state file> <name>: one value of the state file, or '' when it has no such name.
field() {
  python3 -c '
import json, sys
print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))
' "$1" "$2"
}

# alive <pid>: true/false for check.
alive() { kill -0 "$1" 2>/dev/null && echo true || echo false; }

# line_of <text> <regex>: the number of the first line that matches, or '' when none does. The
# cases that read it are about the order the report prints its lines in, not their wording. It
# is `check-release-credentials.test.sh`'s helper of the same name, over text rather than a file.
line_of() { grep -n -m1 -- "$2" <<<"$1" | cut -d: -f1 || true; }

# The shapes the sampler writes and the report prints back: a time is RFC 3339 in UTC to the
# second, and a tick carries three readings, each a number or the `unknown` of a reading this
# kernel would not give.
when_shape='[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
tick_shape="^machine: $when_shape free \([0-9][0-9]*\|unknown\) kB, available memory \([0-9][0-9]*\|unknown\) kB, load \([0-9][0-9]*\.[0-9][0-9]*\|unknown\)$"

watched="$work/watched"
mkdir "$watched"
state="$work/state.json"

echo "case: start writes the state file, with no go and no docker on PATH"
run_watch start --path "$watched" --state "$state"
check "exits 0" "$(is "$status" 0)"
check "names the file system it watches" "$(contains "$out" "^disk: $watched on .*total [0-9][0-9]* kB, free [0-9][0-9]* kB$")"
check "names the missing go rather than failing" "$(contains "$out" '^disk: no go on PATH, so `go env GOCACHE` names no directory$')"
check "names the missing docker rather than failing" "$(contains "$out" '^disk: no docker on PATH, so `docker info')"
check "says what it is watching and from how much" "$(contains "$out" "^disk: watching $watched, free [0-9][0-9]* kB, sampler pid [0-9][0-9]*$")"
check "leaves a state file" "$([ -f "$state" ] && echo true || echo false)"
before=$(field "$state" before_kb)
check "the state carries the before number" "$(contains "$before" '^[0-9][0-9]*$')"
pid=$(field "$state" sampler_pid)
check "the state carries a sampler pid" "$(contains "$pid" '^[0-9][0-9]*$')"
check "the state carries the watched path" "$(is "$(field "$state" path)" "$watched")"

echo "case: the series begins in the second start ran in, not a second later"
# The sampler reads before it sleeps, so its first tick falls inside the start step rather than
# after it. Without that the series would begin after the first step being measured had begun,
# and the boundary a reader most wants to line up would have no tick of the machine beside it.
after_start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
series_file="$state.series"
for _ in $(seq 30); do
  [ -s "$series_file" ] && break
  sleep 0.1
done
check "the sampler wrote a tick without waiting a second" \
  "$([ -s "$series_file" ] && echo true || echo false)"
first_tick=$(head -1 "$series_file")
check "that tick is a reading of the machine" "$(contains "$first_tick" "$tick_shape")"
check "it was read no later than the second start returned in" \
  "$([ "$(cut -d' ' -f2 <<<"$first_tick")" \< "$after_start" ] ||
    [ "$(cut -d' ' -f2 <<<"$first_tick")" = "$after_start" ] && echo true || echo false)"
check "start seeded all three worst readings itself, each with its time" \
  "$([ -n "$(field "$state" least_kb_at)" ] && [ -n "$(field "$state" least_memory_kb_at)" ] &&
     [ -n "$(field "$state" greatest_load_at)" ] && echo true || echo false)"

echo "case: the detached sampler outlives the process that started it"
check "the sampler is alive after start returned" "$(alive "$pid")"

echo "case: the least value falls when the watched file system fills"
dd if=/dev/zero of="$watched/filler" bs=1M count=64 status=none
least=$before
for _ in $(seq 20); do
  sleep 0.5
  least=$(field "$state" least_kb)
  [ "$least" -lt "$before" ] && break
done
check "least_kb fell below before_kb after 64 MiB was written" \
  "$([ "$least" -lt "$before" ] && echo true || echo false)"
check "before_kb is unchanged" "$(is "$(field "$state" before_kb)" "$before")"
rm -f "$watched/filler"

echo "case: the sampler writes the machine to a series file beside the state"
series="$state.series"
check "the sampler left a series file" "$([ -f "$series" ] && echo true || echo false)"
ticks=$(grep -c '' "$series" || true)
shaped=$(grep -c -- "$tick_shape" "$series" || true)
check "it has ticks in it" "$([ "$ticks" -ge 1 ] && echo true || echo false)"
check "every line of it is a tick of the machine" "$(is "$shaped" "$ticks")"

echo "case: the state carries the least memory and the greatest load, each with its time"
check "the least free disk has the time it was read beside it" \
  "$(contains "$(field "$state" least_kb_at)" "^$when_shape$")"
check "the state carries a least available memory in kB" \
  "$(contains "$(field "$state" least_memory_kb)" '^[0-9][0-9]*$')"
check "that memory has the time it was read beside it" \
  "$(contains "$(field "$state" least_memory_kb_at)" "^$when_shape$")"
check "the state carries a greatest one-minute load" \
  "$(contains "$(field "$state" greatest_load)" '^[0-9][0-9]*\.[0-9][0-9]*$')"
check "that load has the time it was read beside it" \
  "$(contains "$(field "$state" greatest_load_at)" "^$when_shape$")"

echo "case: report stops the sampler"
run_watch report --state "$state"
check "exits 0" "$(is "$status" 0)"
check "prints the four numbers" "$(contains "$out" '^disk: before [0-9][0-9]* kB, least [0-9][0-9]* kB, used [0-9][0-9]* kB, margin \(ok\|short\)$')"
check "the sampler is gone" "$(is "$(alive "$pid")" false)"
pid=""

echo "case: report prints the series, then the three worst readings, then the disk line"
# The sampler the case above stopped leaves its state and its series behind, so this second
# report reads the same run and prints it in the order a person reads the log in.
run_watch report --state "$state"
check "exits 0" "$(is "$status" 0)"
check "prints the ticks of the series" "$(contains "$out" "$tick_shape")"
check "names the least free disk and when it was read" \
  "$(contains "$out" "^machine: least free disk [0-9][0-9]* kB at $when_shape$")"
check "names the least available memory and when it was read" \
  "$(contains "$out" "^machine: least available memory [0-9][0-9]* kB at $when_shape$")"
check "names the greatest one-minute load and when it was read" \
  "$(contains "$out" "^machine: greatest one-minute load [0-9][0-9]*\.[0-9][0-9]* at $when_shape$")"
first_tick=$(line_of "$out" "$tick_shape")
worst_disk=$(line_of "$out" '^machine: least free disk ')
worst_memory=$(line_of "$out" '^machine: least available memory ')
worst_load=$(line_of "$out" '^machine: greatest one-minute load ')
disk_line=$(line_of "$out" '^disk: before ')
check "the series comes before the worst readings" \
  "$([ "$first_tick" -lt "$worst_disk" ] && echo true || echo false)"
check "the three worst readings come in that order" \
  "$([ "$worst_disk" -lt "$worst_memory" ] && [ "$worst_memory" -lt "$worst_load" ] && echo true || echo false)"
check "the disk line comes after all of them" \
  "$([ "$worst_load" -lt "$disk_line" ] && echo true || echo false)"
check "the disk line is the one it has always printed" \
  "$(contains "$out" '^disk: before [0-9][0-9]* kB, least [0-9][0-9]* kB, used [0-9][0-9]* kB, margin \(ok\|short\)$')"

echo "case: report prints the four numbers and exits 0 for a margin that is ok"
ok_state="$work/ok.json"
printf '{"path": "%s", "before_kb": 1000, "least_kb": 600, "sampler_pid": null}\n' "$watched" > "$ok_state"
run_watch report --state "$ok_state"
check "exits 0" "$(is "$status" 0)"
check "prints before, least, used and an ok margin" \
  "$(contains "$out" '^disk: before 1000 kB, least 600 kB, used 400 kB, margin ok$')"

echo "case: report exits 0 for a short margin too"
short_state="$work/short.json"
printf '{"path": "%s", "before_kb": 1000, "least_kb": 300, "sampler_pid": null}\n' "$watched" > "$short_state"
run_watch report --state "$short_state"
check "exits 0" "$(is "$status" 0)"
check "prints the shortfall as a short margin" \
  "$(contains "$out" '^disk: before 1000 kB, least 300 kB, used 700 kB, margin short$')"

echo "case: report names the readings no tick ever made and exits 0"
bare_state="$work/bare.json"
printf '{"path": "%s", "before_kb": 1000, "least_kb": 600, "least_kb_at": "2026-01-01T00:00:00Z", "sampler_pid": null}\n' \
  "$watched" > "$bare_state"
run_watch report --state "$bare_state"
check "exits 0" "$(is "$status" 0)"
check "names the disk reading the state does carry" \
  "$(contains "$out" '^machine: least free disk 600 kB at 2026-01-01T00:00:00Z$')"
check "says no available memory was read" \
  "$(contains "$out" '^machine: no available memory was read$')"
check "says no one-minute load was read" \
  "$(contains "$out" '^machine: no one-minute load was read$')"
check "prints the disk line all the same" \
  "$(contains "$out" '^disk: before 1000 kB, least 600 kB, used 400 kB, margin ok$')"

echo "case: report with no series file beside the state says so and exits 0"
no_series_state="$work/no-series.json"
printf '{"path": "%s", "before_kb": 1000, "least_kb": 300, "sampler_pid": null}\n' "$watched" > "$no_series_state"
run_watch report --state "$no_series_state"
check "exits 0" "$(is "$status" 0)"
check "says there were no ticks to print" \
  "$(contains "$out" "^machine: no series file to read at $no_series_state.series, no ticks were written$")"
check "prints the disk line all the same" \
  "$(contains "$out" '^disk: before 1000 kB, least 300 kB, used 700 kB, margin short$')"

echo "case: report on a missing state file exits 0 and says so"
run_watch report --state "$work/never-written.json"
check "exits 0" "$(is "$status" 0)"
check "says there was no state file to read" \
  "$(contains "$out" "^disk: no state file to read at $work/never-written.json, nothing measured$")"

summary "disk_watch.py"
