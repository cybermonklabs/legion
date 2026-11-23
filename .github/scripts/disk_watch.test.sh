#!/usr/bin/env bash
# Tests for `disk_watch.py`: `start` writes the state file and leaves a sampler that outlives it,
# the sampler's least value falls when the watched file system fills, it writes a tick of the
# series every second it is given and keeps the least memory and the greatest load it has seen,
# a sampler held off the CPU leaves the seconds it never ran in out of the series rather than
# writing them afterwards, `report` stops the sampler and prints the series, the three worst
# readings and the four numbers in that order, and it exits 0 whether the margin is ok or short,
# when a reading was never made, when there is no series file and when there is no state file at
# all.
#
# `start` runs here with a PATH holding python3 alone, so neither `go` nor `docker` is on it:
# that is the runner this script must work on too (no Go toolchain, no Docker daemon, no
# database), and it proves the two tools are named and skipped rather than failing the step. The
# two margins are driven from state files written here by hand, so the numbers in the line are
# chosen rather than measured; the falling-least case is measured, against 64 MiB of real writes
# to a tmpfs only it can see (it says SKIP where the kernel refuses an unprivileged mount), and the
# stall case is measured against a real sampler stopped with SIGSTOP for four seconds.
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

# Every process this script starts runs in a PID namespace whose first process is this script, so
# no sampler that `start` detaches can outlive it: when the first process of a PID namespace exits,
# by any path and a SIGKILL included, the kernel kills every other process in it. --mount-proc
# gives the namespace its own /proc, so the pids `start` writes, the ones `report` signals and the
# ones this script reads in /proc are one numbering. Inside, the script is that first process, so
# `$$` is 1 there and nowhere else.
# The process the caller started stays this bash, waiting on the unshare it started: unshare
# --fork blocks SIGTERM and SIGINT in its waiting parent and passes neither on, so a caller that
# signals the pid it started (kill <pid>, a runtime's cancel) would otherwise not stop the test.
# This bash turns a SIGTERM, SIGINT or SIGHUP into a SIGKILL of that unshare. unshare's
# --kill-child then sends the first process a SIGKILL, and setpriv's --pdeathsig does the same to
# the unshare when this bash itself is SIGKILLed, so every way to stop the started pid ends the
# namespace and the samplers in it. Where the kernel refuses an unprivileged namespace, a run by
# hand goes on without one and says so, and CI, whose job allows them first (ci-job-test.yaml),
# fails instead.
if [ "$$" -ne 1 ]; then
  namespace=(setpriv --pdeathsig KILL unshare --user --map-root-user --mount --pid --fork --kill-child --mount-proc)
  if namespace_out=$("${namespace[@]}" true 2>&1); then
    "${namespace[@]}" "$BASH" "${BASH_SOURCE[0]}" "$@" &
    namespace_pid=$!
    # `|| true`: errexit applies inside a trap, and the wait on the unshare just killed returns 137.
    trap 'kill -KILL "$namespace_pid" 2>/dev/null || true; wait "$namespace_pid" 2>/dev/null || true; exit 143' TERM
    trap 'kill -KILL "$namespace_pid" 2>/dev/null || true; wait "$namespace_pid" 2>/dev/null || true; exit 130' INT
    trap 'kill -KILL "$namespace_pid" 2>/dev/null || true; wait "$namespace_pid" 2>/dev/null || true; exit 129' HUP
    namespace_status=0
    wait "$namespace_pid" || namespace_status=$?
    exit "$namespace_status"
  elif [ "${GITHUB_ACTIONS:-}" = true ]; then
    check "the kernel granted this script a PID namespace, which the job's step before this one allows: ${namespace_out%%$'\n'*}" false
    summary "disk_watch.py"
  else
    echo "  NOTE: no PID namespace for this script (${namespace_out%%$'\n'*}), so a sampler it starts can outlive a SIGKILL of it"
  fi
fi

work=$(mktemp -d)
pid=""
cleanup() {
  if [ -n "$pid" ]; then
    # SIGCONT first: the stall case leaves a stopped sampler behind if it dies partway, and a
    # stopped process takes SIGTERM only once it is running again.
    kill -CONT "$pid" 2>/dev/null || true
    kill "$pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

# A PATH with python3 and nothing else on it: no go, no docker. The script's own shebang finds
# python3 through it, and the sampler it starts is started by sys.executable, an absolute path.
# The link names the interpreter python3 reports as sys.executable, asked before PATH and HOME
# change, not the first python3 on the caller's PATH: where that is a version manager's shim
# (mise's, with no python selected), the shim needs mise's settings, and under `env -i` with this
# PATH and HOME it refuses to run ("not a valid shim", or the user's mise config as untrusted when
# the checkout is under the user's home), so no case reached the script.
mkdir "$work/bin"
if ! python=$(python3 -I -c 'import sys; print(sys.executable)'); then
  echo "could not resolve the python3 interpreter" >&2
  exit 1
fi
if [[ "$python" != /* || ! -x "$python" ]]; then
  echo "python3 did not report an absolute executable path: $python" >&2
  exit 1
fi
ln -s "$python" "$work/bin/python3"

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

# state_of <pid>: the state the kernel gives the process (`T` for stopped), read straight from
# /proc/<pid>/stat by the shell, or '' once the process is gone. Not `ps`: it opens the stat file
# of every process on the machine, and on a dev box running 16,530 of them one `ps -o state= -p`
# took 0.66 to 0.71 s, so the stop's second, read after it, came out a second past the stop.
state_of() {
  local stat=""
  read -r stat <"/proc/$1/stat" 2>/dev/null || true
  stat=${stat##*) }
  echo "${stat%% *}"
}

# line_of <text> <regex>: the number of the first line that matches, or '' when none does. The
# cases that read it are about the order the report prints its lines in, not their wording. It
# is `check-release-credentials.test.sh`'s helper of the same name, over text rather than a file.
line_of() { grep -n -m1 -- "$2" <<<"$1" | cut -d: -f1 || true; }

# stamps <series file>: the second each tick of the series carries, as whole epoch seconds, one
# to a line and in the order they were written. The cases that read it are about which seconds
# the series holds and in what order, which is arithmetic rather than text.
stamps() {
  python3 -c '
import calendar, sys, time
for line in open(sys.argv[1]):
    said = line.split()
    if len(said) > 1:
        print(calendar.timegm(time.strptime(said[1], "%Y-%m-%dT%H:%M:%SZ")))
' "$1"
}

# The shapes the sampler writes and the report prints back: a time is RFC 3339 in UTC to the
# second, and a tick carries three readings, each a number or the `unknown` of a reading this
# kernel would not give.
when_shape='[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'
tick_shape="^machine: $when_shape free \([0-9][0-9]*\|unknown\) kB, available memory \([0-9][0-9]*\|unknown\) kB, load \([0-9][0-9]*\.[0-9][0-9]*\|unknown\)$"

watched="$work/watched"
mkdir "$watched"
state="$work/state.json"

echo "case: the bare PATH's python3 is an interpreter"
bare_status=0
bare=$(env -i PATH="$work/bin" HOME="$work" TMPDIR="$work" python3 -c 'print("ran")' 2>&1) || bare_status=$?
check "exits 0 under the bare PATH and HOME" "$(is "$bare_status" 0)"
check "runs under the bare PATH and HOME" "$(is "$bare" ran)"
if [ "$bare_status" -ne 0 ] || [ "$bare" != ran ]; then
  printf '%s\n' "$bare" >&2
  summary "disk_watch.py"
fi

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
# What this proves is that the sampler reads the free space of the file system it watches while
# that file system fills, and keeps the fall as the least it has seen. On a file system other
# writers share, the fall it sees is the sum of every writer's: on a shared dev box disk the free
# space moved by up to 1,300 MiB in 20 s, so another writer freeing more than 64 MiB hid the case's
# write and failed this case, and another writer filling - or the sampler's own state and series
# files beside the watched directory - let it pass with no reading of the case's write at all. So
# the case watches a tmpfs that only it can see: mounted in unprivileged user, mount and PID
# namespaces, with the sampler started inside them and the state file outside, on $work. The one
# write that tmpfs ever takes is the case's 64 MiB, so the least free must fall by all of it.
# Where the kernel refuses an unprivileged mount, there is no file system only this case writes:
# a run by hand says SKIP and counts nothing rather than measuring a shared one, and CI, whose job
# turns Ubuntu's AppArmor restriction off first (ci-job-test.yaml), fails the case instead.
private="$work/private"
private_state="$work/private.json"
private_ready="$work/private.status"
private_log="$work/private.log"
mkdir "$private"
# ended <pid>: true once the process is gone from this script's /proc or is a zombie. The script's
# own PID namespace has its own /proc, so the pid of a process it started is the one read there.
ended() {
  local stat=""
  read -r stat 2>/dev/null <"/proc/$1/stat" || return 0
  stat=${stat##*) }
  [ "${stat%% *}" = Z ]
}
if private_out=$(unshare --user --map-root-user --mount --pid --fork --mount-proc sh -c 'mount -t tmpfs -o size=1m tmpfs "$1"' sh "$private" 2>&1); then
  # The namespaces live exactly as long as this script holds the pipe below open. Their first
  # process waits on it, and when the first process of a PID namespace exits the kernel kills every
  # other process in it: the sampler with it, and the tmpfs with the mount namespace. Only this
  # script holds the write end (the sampler that `start` launches inherits none of its
  # descriptors), so the namespaces end when the case closes it, or when this script exits by any
  # path, a SIGKILL that no trap sees included; no sampler is left on its 5,400 s backstop. The pid
  # in the state file is the sampler's pid inside its own PID namespace, so nothing here signals
  # it: `report`, which would, is left to the cases of the shared sampler. What the namespace's
  # commands print goes to $private_log, and is printed below when the setup fails.
  # The write waits for the sampler's first sample: the sampler appends its first series line only
  # once that sample's readings are in, so the write comes after a reading taken before it, and a
  # watch that reads the disk once and repeats that reading fails the fall below whatever the
  # timing. Without the wait, a first reading that came after the write - one held 0.6 s did - let
  # such a watch pass. No wait in this case has a clock of its own, since a loaded machine can take
  # any time to start the sampler or to run it: the inner one ends on the first series line, on
  # the sampler leaving the process table (status 93; it is an orphan, so its exit code cannot be
  # read), or on the pipe closing (status 94), each read in the namespace's own /proc; the outer
  # one ends on the status file or on the namespace ending; below, the fall is read until it shows
  # or three passes have followed the write, and the close waits for the namespace to end. Each
  # wait out here has one bound, a hang guard of 300 s by the clock ($SECONDS), that only a hung
  # mount, start or sampler reaches, and reaching it fails the case by name. The pipe is read from
  # the shell's first line by a `cat` of its own, which ends when the pipe closes, and the shell
  # looks at that `cat` in two places only: the wait for the first series line and its last step.
  # So a closed pipe cuts no step short, a held mount or start included. The shell runs on to that
  # wait, which ends it with status 94 (93 if the sampler is gone first); or, if the first series
  # line is there by then, it passes the wait, writes, records status 0 and ends at its last step,
  # which returns at once. A mount, start or write that fails still ends it with that step's
  # status (90, 91, 92).
  exec {private_hold}> >(exec unshare --user --map-root-user --mount --pid --fork --mount-proc sh -c '
    exec >"$7" 2>&1 3<&0
    cat <&3 >/dev/null &
    reader=$!
    gone() { st=""; read -r st 2>/dev/null <"/proc/$1/stat" || return 0; st=${st##*) }; [ "${st%% *}" = Z ]; }
    mount -t tmpfs -o size=128m tmpfs "$1" || { echo 90 >"$6"; exit; }
    env -i PATH="$2" HOME="$3" TMPDIR="$3" "$4" start --path "$1" --state "$5" || { echo 91 >"$6"; exit; }
    sp=$(sed -n "s/.*\"sampler_pid\": *\([0-9][0-9]*\).*/\1/p" "$5")
    until [ -s "$5.series" ]; do
      if [ -z "$sp" ] || gone "$sp"; then echo "the sampler (pid ${sp:-unknown}) left before its first sample"; echo 93 >"$6"; exit; fi
      if gone "$reader"; then echo "the pipe closed before the sampler'"'"'s first sample"; echo 94 >"$6"; exit; fi
      sleep 0.05
    done
    dd if=/dev/zero of="$1/filler" bs=1M count=64 status=none || { echo 92 >"$6"; exit; }
    echo 0 >"$6"
    wait "$reader"
  ' sh "$private" "$work/bin" "$work" "$watch" "$private_state" "$private_ready" "$private_log")
  private_ns=$!
  private_guard=""
  deadline=$((SECONDS + 300))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ -s "$private_ready" ] && break
    ended "$private_ns" && break
    sleep 0.2
  done
  [ -s "$private_ready" ] || ended "$private_ns" || private_guard=", none within the 300 s hang guard"
  private_status=$(tr -d '\n' 2>/dev/null <"$private_ready" || true)
  check "start watched a tmpfs only this case writes, and the case wrote 64 MiB on it" \
    "$(is "$private_status" 0)"
  if [ "$private_status" = 0 ]; then
    private_before=$(field "$private_state" before_kb)
    # The fall is read until it shows, or until three passes of the sampler have appended a line
    # since the write: each pass reads the machine after the one before it appended, and writes the
    # state before the next one appends, so by the third line the state holds a reading taken after
    # the write. A sampler that stops passing meets the 300 s hang guard instead.
    private_lines=$(grep -c '' "$private_state.series" || true)
    fell=0
    deadline=$((SECONDS + 300))
    while [ "$SECONDS" -lt "$deadline" ]; do
      grown=$(($(grep -c '' "$private_state.series" || true) - private_lines))
      fell=$((private_before - $(field "$private_state" least_kb)))
      if [ "$fell" -ge 65536 ] || [ "$grown" -ge 3 ]; then break; fi
      sleep 0.2
    done
    check "least_kb fell by at least the 64 MiB written, 65536 kB below before_kb" \
      "$([ "$fell" -ge 65536 ] && echo true || echo false)"
    check "before_kb is unchanged" "$(is "$(field "$private_state" before_kb)" "$private_before")"
    # The sampler is seen from here by its command line, which names the case's own private path:
    # it must be seen while the namespaces live, or its absence after them would prove nothing.
    check "the sampler runs in the namespaces, seen by its command line" \
      "$([ -n "$(pgrep -f -- "disk_watch.py sample --path $private " || true)" ] && echo true || echo false)"
  else
    printf '    setup status %s%s (90: mount, 91: start, 92: dd, 93: the sampler left before its first sample, 94: the pipe closed before it, none: the namespace ended without a status); its output:\n' \
      "${private_status:-none}" "$private_guard" >&2
    sed 's/^/    /' "$private_log" >&2 || true
  fi
  exec {private_hold}>&-
  # Once its unshare has ended, the namespaces' first process has, and the kernel has taken every
  # other process in them with it, so the sampler is looked for once, after that.
  deadline=$((SECONDS + 300))
  while [ "$SECONDS" -lt "$deadline" ]; do
    ended "$private_ns" && break
    sleep 0.2
  done
  check "closing the pipe ended the namespaces, and the sampler in them" \
    "$(ended "$private_ns" && is "$(pgrep -f -- "disk_watch.py sample --path $private " || true)" "" || echo false)"
elif [ "${GITHUB_ACTIONS:-}" = true ]; then
  check "the kernel granted an unprivileged tmpfs mount, which the job's step before this one allows: ${private_out%%$'\n'*}" false
else
  echo "  SKIP: the falling-least case (no file system only it writes: the kernel refused an unprivileged tmpfs mount: ${private_out%%$'\n'*})"
fi

echo "case: the sampler writes the machine to a series file beside the state"
series="$state.series"
check "the sampler left a series file" "$([ -f "$series" ] && echo true || echo false)"
# The sampler is still running here, so the file grows between any two reads of it: counting the
# lines and then counting the ticks straight off the file reads two different files whenever a
# tick lands between them, and the second count is one higher. Measured on this pod: 8 of 400
# paired reads disagreed that way. So both counts are taken from one copy.
cp "$series" "$work/series.snapshot"
ticks=$(grep -c '' "$work/series.snapshot" || true)
shaped=$(grep -c -- "$tick_shape" "$work/series.snapshot" || true)
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

echo "case: the seconds a stopped sampler did not run in stay missing from the series"
# A sampler that cannot keep up is one of the things this series exists to show, and a hole in
# the stamps is how a reader sees it. SIGSTOP for four seconds is that starvation at its
# plainest: the sampler is held off the CPU for whole seconds, then let go. Those seconds must
# be absent from the series rather than written one after another when it comes back, each
# stamped with a second that had already gone by - a series with no hole in it and every stamp
# further behind its own reading is a starved machine reading as a healthy one. The tick the
# sampler was already waiting for when it was stopped is stamped from the clock once its
# readings are in, so it names the second the sampler came back in and not the second it was
# due in.
stall_state="$work/stall.json"
run_watch start --path "$watched" --state "$stall_state"
check "start left a sampler to stop" "$(is "$status" 0)"
pid=$(field "$stall_state" sampler_pid)
sleep 3
# The stop is sent just after the sampler has written the line of the second it is in, while it
# sleeps towards the next second: the one it was waiting for. A sampler that stamps that pending
# second instead of reading the clock carries it out of the stall and writes it on waking, and the
# window of seconds no line may name, read below, then begins at exactly that second. The wait is
# bounded, because a sampler too starved to write a line inside the second it names gives no such
# moment: the stop is then sent anyway, and the case says what it could not see.
for _ in $(seq 120); do
  [ "$(tail -n 1 "$stall_state.series" | cut -d' ' -f2)" = "$(date -u +%Y-%m-%dT%H:%M:%SZ)" ] && break
  sleep 0.025
done
kill -STOP "$pid"
# `kill -STOP` only sends the signal, and the series below may be read only once the process
# can no longer write. Measured in this pod, a sleeping sampler shows `T` within about a
# millisecond, so the wait is a guarantee rather than a delay.
for _ in $(seq 50); do
  [ "$(state_of "$pid")" = T ] && break
  sleep 0.1
done
check "the kernel has the sampler off the CPU" "$(is "$(state_of "$pid")" T)"
# The stop's second is read from the clock only after the check above has read `T`, so the stop
# landed in that second or an earlier one, and every second after it is one the sampler was off
# the CPU for the whole of. Not before that read: when the polls above all miss the stop and it
# becomes visible only at the check, a second read before the check can come before the stop, and
# the sampler's honest lines up to the stop then fall inside the window. When the check did not
# read `T`, the case has failed already. The window of seconds no line may name begins after this
# second, not at the second after the sampler's last line: a stop can land after the sampler read
# the clock and before its line reached the file, and that line - naming a second the sampler did
# run in, the one it read the clock in - reaches the file on waking. A window begun after the last
# line refused it.
stopped_in=$(date -u +%s)
# While the sampler is off the CPU it cannot write, so its series file is stable here and its last
# stamp is the last second it has a line for.
last_before_stall=$(stamps "$stall_state.series" | tail -1)
# When that line names the stop's own second, the stop landed while the sampler slept, and the
# window's first second is the one it was waiting for. When it does not, the stop landed before
# the sampler had written that second, and a line naming a second up to the stop's own may follow
# on waking whether its clock was read before the stop or the second was carried out pending: this
# run cannot tell the two apart, and says so rather than passing them as seen.
if [ "$last_before_stall" != "$stopped_in" ]; then
  echo "  UNKNOWN: the stop was seen in second $stopped_in, which the sampler had not written (its" \
    "last line names $last_before_stall), so this run cannot catch a pending second carried out of the stall"
fi
sleep 4
# resumed_at is read before the sampler is let go, so the second it is let go in - which may
# carry the line it writes on waking - is left out of the window. Reading it inside the stall
# also keeps the case off the second that can turn over between `kill -CONT` and a `date` run
# after it.
resumed_at=$(date -u +%s)
kill -CONT "$pid"
sleep 3
run_watch report --state "$stall_state"
check "report stopped it and exited 0" "$(is "$status" 0)"
pid=""
seconds=$(stamps "$stall_state.series")
ticks=$(grep -c '^[0-9]' <<<"$seconds")
after=$(awk -v resumed="$resumed_at" '$1 >= resumed' <<<"$seconds" | grep -c '^[0-9]' || true)
# The number of ticks whose second did not move on: a sampler that reads the clock at the
# wrong moment could stamp two ticks with one second.
out_of_order=$(awk 'NR > 1 && $1 <= last { n += 1 } { last = $1 } END { print n + 0 }' <<<"$seconds")
# The lines naming a second from the one after the stop to the one before the sampler was let go:
# seconds it was off the CPU for the whole of. A sampler that stamps the second it was waiting for
# instead of reading the clock writes exactly one when the stop landed while it slept: that second.
inside_stall=$(awk -v from="$((stopped_in + 1))" -v to="$((resumed_at - 1))" \
  '$1 >= from && $1 <= to { n += 1 } END { print n + 0 }' <<<"$seconds")
# What this proves is that the sampler was running before the stop and came back after it, so
# the hole below is the stop rather than a sampler that died in it. It cannot ask for a tick in
# every second it was given: on a loaded runner the sampler loses seconds of its own, and that
# hole is the very reading this case exists to keep. So the floor is low on purpose.
check "the sampler ticked either side of the stop" \
  "$([ "$ticks" -ge 4 ] && [ "$after" -ge 1 ] && echo true || echo false)"
check "the stamps follow the real clock: no second is written twice or out of order" \
  "$(is "$out_of_order" 0)"
# The window has to hold seconds for the check below it to mean anything, and the four-second
# stall is what puts them there.
check "the stall left whole seconds that no line may name" \
  "$([ "$((resumed_at - 1))" -ge "$((stopped_in + 1))" ] && echo true || echo false)"
check "no line names a second the sampler was off the CPU for the whole of" \
  "$(is "$inside_stall" 0)"
if [ "$inside_stall" != 0 ]; then
  echo "    the seconds from $((stopped_in + 1)) to $((resumed_at - 1)) are the window; the series:" >&2
  sed 's/^/    /' "$stall_state.series" >&2
fi

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
