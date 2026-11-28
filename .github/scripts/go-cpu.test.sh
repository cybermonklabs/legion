#!/usr/bin/env bash
# Tests for `go-cpu-exec.py`, the wrapper `go test -exec` and `go run -exec` run each Go binary
# through, so that a CI job records the CPU each test binary took. The wrapper runs the binary as
# it was given: its arguments, its environment (but for the GO_CPU_PARENT it adds for a `.test`
# binary), its stdin, stdout and stderr byte for byte, the signals its caller left ignored, its
# exit status and the signal that ended it all reach the caller as they would without it, and a
# SIGINT, SIGTERM, SIGQUIT or SIGHUP sent to the wrapper reaches the binary, while a SIGKILL sent
# to it ends the binary too. It writes one whole JSON row for each binary, into GO_CPU_DIR, else
# into $RUNNER_TEMP/go-test-events/cpu; with neither set it runs the binary in its own process and
# writes nothing. The row holds the binary's CPU, its own and that of the children it waited for,
# its directory, relative to GITHUB_WORKSPACE when inside it, and, for a wrapper under a test
# binary, the id of the row of the wrapper above it. A row it cannot write is dropped in silence,
# and twenty wrappers at once in one directory write twenty whole rows. W12 holds the same of the
# real `go test`, `go test -list`, `go test -json` and `go run`, and that `go vet` and `go list`
# run nothing through it.
#
# The binaries of every case but W12 are small bash and Python programs written here, so those
# cases need no Go. W12 builds a scratch module with the go on PATH, with a build cache, a module
# cache, a config directory and a GOFLAGS of its own, so it neither reads nor writes the machine's.
# Every case names its record directory itself, GO_CPU_DIR set to a fresh directory or both it and
# RUNNER_TEMP unset, because CI's test job sets RUNNER_TEMP and a case that left it would write
# under the job's own.
# Everything is written under $TMPDIR (mktemp -d honours it), never /tmp by name.
#
# The cases of `go-cpu-runner.py`, R1-R6, go below the W cases, before the summary. `start` prints
# the runner's facts and the raw text it read them from, and leaves a detached sampler of
# /proc/stat behind it, or with --no-sampler no process at all; `report` stops that sampler, sums
# the rows it finds by class, reads each Postgres container's cpu.stat through docker and prints
# the CPU /proc/stat counted between the two. These cases point RUNNER_TEMP and HOME at scratch
# directories, so they neither read nor write the CI job's own, and put a stand-in docker on PATH.
# `start` hashes 256 MiB on one CPU and again on each, so they run it twice in all.
#
# R1, R3 and R4 hold the runner's parsers to text captured from real runners, through the
# script's hidden `parse` mode, which reads files where `start` and `report` read the machine. The
# files are under testdata/go-cpu/, beside go_cpu_record.test.sh's fixtures, captured on 2026-11-28:
#
#   lscpu-140585658657.txt
#       gh api --allow-escape-sequences repos/cybermonklabs/legion/actions/jobs/140585658657/logs
#       | tr -d '\033' | cut -c30-
#     the output lines of that probe job's `nproc; lscpu | sed -n '1,20p'` step on
#     ubuntu-24.04-arm, nproc's 4 and lscpu's first 20 lines, without their timestamps.
#   raw-runner-facts-140938293854.txt
#     the same read of the log of envoy-go-checks (ubuntu-24.04, x64) of run 46131638614: the lines
#     between `##[group]go-cpu raw runner facts` and the next `##[endgroup]`, as the runner shows
#     the ::group:: and ::endgroup:: that `start` prints, its lscpu output and /proc/cpuinfo.
#   go-test-events-140938293835/cpu/runner/
#     leg 5's lscpu.txt, cpuinfo.txt, stat-start.txt, stat-end.txt and
#     postgres-service.cpu.stat, from its artifact as go_cpu_record.test.sh's header says.
#   cpu-stat-140938293848/
#       gh run download 46131638614 -R cybermonklabs/legion -n go-test-events-140938293848 -D <dir>
#     race-impossible's two cpu.stat files, of its service and its Postgres that preloads
#     pg_stat_statements, from that artifact's cpu/runner/.
#   log-140938293835.excerpt.txt, log-140938293848.excerpt.txt, log-140938293854.excerpt.txt
#     the `go-cpu` lines the three jobs' logs printed, the excerpts go_cpu_record.test.sh's header
#     names, whose runner and usage lines are as captured.
#
# Run from anywhere: .github/scripts/go-cpu.test.sh
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
wrapper="$script_dir/go-cpu-exec.py"

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
# W8 leaves a directory of mode 0500 and go leaves its module cache read-only, and rm -rf alone
# empties neither.
trap 'chmod -R u+rwx "$work" 2> /dev/null || true; rm -rf "$work"' EXIT
# The wrapper reads its directory from the kernel, which names it by its physical path, so the
# paths the cases expect are physical too.
work=$(cd "$work" && pwd -P)
cd "$work"

# program <path>: writes this shell's stdin to <path> and makes it executable.
program() {
  cat > "$1"
  chmod +x "$1"
}

# run <command...>: runs <command> in this shell, so a wrapper it starts is a child of $$, with
# no stdin; its stdout goes to $work/stdout and its stderr to $work/stderr, byte for byte, and its
# status to $status. The braces' own stderr is dropped: that is where bash reports a command a
# signal ended, which W3's do.
run() {
  { "$@" < /dev/null > "$work/stdout" 2> "$work/stderr" && status=0 || status=$?; } 2> /dev/null
}

# run_in <dir> <command...>: run, from <dir>.
run_in() {
  local dir=$1
  shift
  cd "$dir"
  run "$@"
  cd "$work"
}

# same <file> <file>: true when the two files hold the same bytes.
same() { cmp -s -- "$1" "$2" && echo true || echo false; }

# holds <file> <printf format>: true when <file> holds exactly the bytes the format prints.
holds() {
  # shellcheck disable=SC2059 # The format is the expected bytes, escapes and all.
  cmp -s -- "$1" <(printf "$2") && echo true || echo false
}

# row <dir> <expression> [<arg>...]: the value of a Python <expression> over the rows in <dir>:
# `rows`, the objects of its `*.json` files in name order; `r`, the one row when there is exactly
# one, else None; `names`, the name of every file in it, dot files too; and `a`, the <arg>s. A
# string prints as itself and anything else as JSON. A row that is not JSON, or an expression that
# cannot be read, prints `unreadable: <why>`, so it fails the check that reads it rather than
# ending this script.
row_reader='
import json, os, sys
try:
    where, a = sys.argv[1], sys.argv[3:]
    names = sorted(os.listdir(where)) if os.path.isdir(where) else []
    rows = []
    for name in names:
        if name.endswith(".json") and not name.startswith("."):
            with open(os.path.join(where, name), encoding="utf-8") as f:
                rows.append(json.load(f))
    r = rows[0] if len(rows) == 1 else None
    value = eval(sys.argv[2])
except Exception as e:
    value = "unreadable: %r" % (e,)
print(value if isinstance(value, str) else json.dumps(value))
'
row() { python3 -c "$row_reader" "$@"; }

# ended <command...>: how <command> ended, `exit <status>` or `signal <number>`. bash spells a
# signal N as the status 128+N, which a process that exits 128+N gives too.
ended() {
  python3 -c '
import subprocess, sys
try:
    code = subprocess.run(sys.argv[1:], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode
except OSError as e:
    print("not started: %s" % e)
else:
    print("signal %d" % -code if code < 0 else "exit %d" % code)
' "$@"
}

# wait_for <path>: waits up to 10 s for <path> to exist, and fails when it never does.
wait_for() {
  local i
  for i in $(seq 200); do
    [ -e "$1" ] && return 0
    sleep 0.05
  done
  return 1
}

# stat_field <pid> <n>: field <n> of /proc/<pid>/stat after its comm field, which a process's own
# name cannot shift (1 is the state, 2 the parent's pid, 4 the session), or nothing when there is
# no process <pid>. It forks nothing.
stat_field() {
  local stat fields
  { read -r stat < "/proc/$1/stat"; } 2> /dev/null || return 0
  read -r -a fields <<< "${stat##*) }"
  echo "${fields[$2 - 1]}"
}

# gone <pid>: succeeds when no process <pid> runs: there is none, or it is a zombie, which has
# ended and waits only for whoever adopted it to reap it. It starts no program, so a loop of it
# measures the process rather than itself.
gone() {
  [[ $(stat_field "$1" 1) =~ ^Z?$ ]]
}

# parent_of <pid>: the pid of <pid>'s parent, or nothing when there is no process <pid>.
parent_of() { stat_field "$1" 2; }

# now_us: the time in microseconds, from bash itself, so reading it forks nothing.
now_us() {
  local now=$EPOCHREALTIME
  echo "${now/[.,]/}"
}

# fresh <command...>: runs <command> with SIGINT, SIGQUIT, SIGHUP, SIGTERM, SIGPIPE and SIGXFSZ at
# their default, as go starts a binary. This shell starts a background command with SIGINT and
# SIGQUIT ignored, the shell that runs it may have left others ignored, and the wrapper rightly
# keeps a signal its caller ignored ignored, so a case of the wrapper's signals starts it through
# this. Python ignores SIGPIPE and SIGXFSZ at its start, which an exec would keep.
fresh="$work/fresh"
program "$fresh" << 'EOF'
#!/usr/bin/env python3
import os, signal, sys
for number in (signal.SIGINT, signal.SIGQUIT, signal.SIGHUP, signal.SIGTERM, signal.SIGPIPE, signal.SIGXFSZ):
    signal.signal(number, signal.SIG_DFL)
os.execvp(sys.argv[1], sys.argv[1:])
EOF

# quick: a binary that exits 0 at once.
quick="$work/quick"
program "$quick" << 'EOF'
#!/usr/bin/env bash
exit 0
EOF

echo "case W1: a binary that burns a second of CPU has one row, of that CPU, its exit 0 and its directory"
d="$work/w1"
mkdir -p "$d/ws/pkg/sub"
program "$d/ws/pkg/sub/burn" << 'EOF'
#!/usr/bin/env python3
# Writes its parent's pid to the file its first argument names, then burns a second of CPU of its
# own.
import os, sys, time
with open(sys.argv[1], "w") as f:
    f.write(str(os.getppid()))
while time.process_time() < 1.0:
    pass
EOF
before=$(date +%s.%N)
run_in "$d/ws/pkg/sub" env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows" GITHUB_WORKSPACE="$d/ws" \
  GITHUB_JOB=envoy-go GITHUB_ACTION=__run_7 "$wrapper" ./burn "$d/ppid"
after=$(date +%s.%N)
ppid=$(cat "$d/ppid" 2> /dev/null || true)
check "the wrapper exits 0, the binary's status" "$(is "$status" 0)"
check "and prints nothing of its own" "$(is "$(cat "$work/stdout" "$work/stderr")" "")"
check "it writes exactly one row" "$(is "$(row "$d/rows" 'len(rows)')" 1)"
check "named for its id, with no other file beside it" "$(row "$d/rows" 'names == [r["id"] + ".json"]')"
check "its fields are in the schema's order" "$(is "$(row "$d/rows" '",".join(r)')" v,id,parent,go_pid,job,step,cwd,binary,args,start,wall_s,user_s,sys_s,self_user_s,self_sys_s,children_user_s,children_sys_s,maxrss_kib,exit,signal)"
check "its v is 1" "$(is "$(row "$d/rows" 'r["v"]')" 1)"
check "its id ends with the wrapper's pid, the binary's parent" "$(is "$(row "$d/rows" 'r["id"].split("-")[1]')" "$ppid")"
check "and begins with its start in nanoseconds" "$(row "$d/rows" 'abs(int(r["id"].split("-")[0]) / 1e9 - r["start"]) < 1e-5')"
check "its start is between the run's start and end" "$(row "$d/rows" "$before <= r['start'] <= $after")"
check "its user and system CPU add up to the second it burned" "$(row "$d/rows" 'r["user_s"] + r["sys_s"] >= 0.8')"
check "and so do its own, read from /proc, with next to none for children" "$(row "$d/rows" 'r["self_user_s"] + r["self_sys_s"] >= 0.8 and r["children_user_s"] + r["children_sys_s"] < 0.2')"
check "its wall time is at least that second" "$(row "$d/rows" 'r["wall_s"] >= 0.8')"
check "its exit is 0 and its signal null" "$(is "$(row "$d/rows" '[r["exit"], r["signal"]]')" '[0, null]')"
check "its cwd is the binary's directory, relative to GITHUB_WORKSPACE" "$(is "$(row "$d/rows" 'r["cwd"]')" pkg/sub)"
check "its job and step are GITHUB_JOB and GITHUB_ACTION" "$(is "$(row "$d/rows" '[r["job"], r["step"]]')" '["envoy-go", "__run_7"]')"
check "its binary is the basename of the binary's path, its args the arguments after it" "$(row "$d/rows" 'r["binary"] == "burn" and r["args"] == a' "$d/ppid")"
check "its parent is null and its go_pid the wrapper's parent, this shell" "$(is "$(row "$d/rows" '[r["parent"], r["go_pid"]]')" "[null, $$]")"
check "its maxrss is a whole number of KiB above 0" "$(row "$d/rows" 'type(r["maxrss_kib"]) is int and r["maxrss_kib"] > 0')"
run_in "$d/ws/pkg/sub" env -u GO_CPU_PARENT -u GITHUB_WORKSPACE -u GITHUB_JOB -u GITHUB_ACTION \
  GO_CPU_DIR="$d/rows-absolute" "$wrapper" "$quick"
check "with no GITHUB_WORKSPACE the wrapper exits 0" "$(is "$status" 0)"
check "and its cwd is the absolute directory" "$(is "$(row "$d/rows-absolute" 'r["cwd"]')" "$d/ws/pkg/sub")"
check "and with no GITHUB_JOB and GITHUB_ACTION its job and step are null" "$(is "$(row "$d/rows-absolute" '[r["job"], r["step"]]')" '[null, null]')"
run_in "$d/ws/pkg/sub" env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows-prefix" GITHUB_WORKSPACE="$d/ws/pk" "$wrapper" "$quick"
check "a GITHUB_WORKSPACE that only begins the directory's path, no directory above it, leaves the cwd absolute" "$(is "$(row "$d/rows-prefix" 'r["cwd"]')" "$d/ws/pkg/sub")"
program "$d/ws/pkg/sub/burn-below" << 'EOF'
#!/usr/bin/env bash
# Runs the burner as a child of its own and waits for it.
"$(dirname "$0")/burn" "$1"
EOF
run_in "$d/ws/pkg/sub" env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows-below" "$wrapper" ./burn-below "$d/ppid-below"
check "a binary whose child burns the second exits 0 through the wrapper" "$(is "$status" 0)"
check "its user and system CPU still hold that second, the child's it waited for" "$(row "$d/rows-below" 'r["user_s"] + r["sys_s"] >= 0.8')"
check "which /proc gives as its children's, not its own" "$(row "$d/rows-below" 'r["children_user_s"] + r["children_sys_s"] >= 0.8 and r["self_user_s"] + r["self_sys_s"] < 0.5')"

echo "case W2: the binary's exit status is the wrapper's and the row's"
d="$work/w2"
mkdir -p "$d"
program "$d/three" << 'EOF'
#!/usr/bin/env bash
exit 3
EOF
run env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows" "$wrapper" "$d/three"
check "the wrapper exits 3" "$(is "$status" 3)"
check "and prints nothing" "$(is "$(cat "$work/stdout" "$work/stderr")" "")"
check "the row's exit is 3, its signal null" "$(is "$(row "$d/rows" '[r["exit"], r["signal"]]')" '[3, null]')"

echo "case W3: a binary a signal ended ends the wrapper by that same signal, and the row names it"
d="$work/w3"
mkdir -p "$d"
program "$d/kill9" << 'EOF'
#!/usr/bin/env bash
kill -KILL $$
EOF
program "$d/term" << 'EOF'
#!/usr/bin/env bash
kill -TERM $$
EOF
run env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows" "$wrapper" "$d/kill9"
check "bash sees the wrapper end with 137" "$(is "$status" 137)"
check "and it prints nothing" "$(is "$(cat "$work/stdout" "$work/stderr")" "")"
check "the row's signal is 9, its exit null" "$(is "$(row "$d/rows" '[r["exit"], r["signal"]]')" '[null, 9]')"
check "the wrapper was ended by SIGKILL itself, not an exit of 137" "$(is "$(ended env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows-again" "$wrapper" "$d/kill9")" 'signal 9')"
check "a binary SIGTERM ended ends the wrapper by SIGTERM, a signal the wrapper itself catches to forward" "$(is "$(ended env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows-term" "$wrapper" "$d/term")" 'signal 15')"
check "and that row's signal is 15, its exit null" "$(is "$(row "$d/rows-term" '[r["exit"], r["signal"]]')" '[null, 15]')"

echo "case W4: SIGQUIT, SIGINT, SIGTERM and SIGHUP sent to the wrapper reach the binary, and the binary starts with the signals its caller ignored ignored, no more"
d="$work/w4"
mkdir -p "$d"
program "$d/trapper" << 'EOF'
#!/usr/bin/env python3
# Catches the signal its first argument names, and on it writes that name to the file its second
# names and exits 0. It writes the file its third names once its handler is in place, then waits
# up to 30 s and exits 1.
import signal, sys, time
def caught(number, frame):
    with open(sys.argv[2], "w") as f:
        f.write(signal.Signals(number).name)
    sys.exit(0)
signal.signal(getattr(signal, sys.argv[1]), caught)
open(sys.argv[3], "w").close()
time.sleep(30)
sys.exit(1)
EOF
for sig in QUIT INT TERM HUP; do
  rm -f "$d/caught" "$d/ready"
  "$fresh" env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows-$sig" "$wrapper" "$d/trapper" "SIG$sig" "$d/caught" "$d/ready" \
    < /dev/null > "$d/stdout" 2> "$d/stderr" &
  pid=$!
  wait_for "$d/ready" || true
  kill -"$sig" "$pid" 2> /dev/null || true
  { wait "$pid" && status=0 || status=$?; } 2> /dev/null
  check "SIG$sig sent to the wrapper reaches the binary, which catches it" "$(is "$(cat "$d/caught" 2> /dev/null || true)" "SIG$sig")"
  check "and the wrapper exits 0, the binary's status, printing nothing" "$(is "$status:$(cat "$d/stdout" "$d/stderr")" 0:)"
  check "and the row's exit is 0, its signal null" "$(is "$(row "$d/rows-$sig" '[r["exit"], r["signal"]]')" '[0, null]')"
done
# bash lists, as `trap -- '' SIG<NAME>`, each signal it found ignored when it started.
traps=(bash -c 'trap -p')
ignoring=(bash -c 'trap "" HUP QUIT; exec "$@"' ignoring)
run "$fresh" "${traps[@]}"
cp "$work/stdout" "$d/traps-direct"
check "the control: a binary started with no signal ignored lists none" "$(holds "$d/traps-direct" '')"
run "$fresh" env GO_CPU_DIR="$d/rows-traps" "$wrapper" "${traps[@]}"
check "through a wrapper that records, it starts with none ignored either" "$(is "$status:$(same "$work/stdout" "$d/traps-direct")" 0:true)"
run "$fresh" env -u GO_CPU_DIR -u RUNNER_TEMP "$wrapper" "${traps[@]}"
check "nor through one that records nothing, which runs it in its own process" "$(is "$status:$(same "$work/stdout" "$d/traps-direct")" 0:true)"
run "$fresh" "${ignoring[@]}" "${traps[@]}"
cp "$work/stdout" "$d/ignoring-direct"
check "the control: a binary whose caller ignored SIGHUP and SIGQUIT starts with both ignored" "$(holds "$d/ignoring-direct" "trap -- '' SIGHUP\ntrap -- '' SIGQUIT\n")"
run "$fresh" "${ignoring[@]}" env GO_CPU_DIR="$d/rows-ignoring" "$wrapper" "${traps[@]}"
check "through a wrapper that records, it starts with both still ignored, and no other" "$(is "$status:$(same "$work/stdout" "$d/ignoring-direct")" 0:true)"
run "$fresh" "${ignoring[@]}" env -u GO_CPU_DIR -u RUNNER_TEMP "$wrapper" "${traps[@]}"
check "and through one that records nothing" "$(is "$status:$(same "$work/stdout" "$d/ignoring-direct")" 0:true)"

echo "case W5: SIGKILL sent to the wrapper ends the binary within 1 s"
d="$work/w5"
mkdir -p "$d"
program "$d/sleeper" << 'EOF'
#!/usr/bin/env bash
# Writes its pid, whole, to the file its first argument names, then becomes a 300 s sleep.
echo $$ > "$1.part"
mv "$1.part" "$1"
exec sleep 300
EOF
env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows" "$wrapper" "$d/sleeper" "$d/pid" < /dev/null > "$d/stdout" 2> "$d/stderr" &
pid=$!
# bash reports a background job that a signal ended in a line on its own stderr, at a moment of
# its own choosing, which no redirection here reaches. This one is SIGKILLed on purpose and its
# status is not needed, so it is made no job of this shell's; bash still reaps it.
disown "$pid"
wait_for "$d/pid" || true
child=$(cat "$d/pid" 2> /dev/null || true)
check "the control: the binary runs as the wrapper's child" "$(is "$(parent_of "${child:-0}")" "$pid")"
kill -KILL "$pid" 2> /dev/null || true
killed=$(now_us)
while ! gone "${child:-0}" && [ "$(($(now_us) - killed))" -lt 1000000 ]; do
  sleep 0.02
done
check "the binary is gone within 1 s of the wrapper's SIGKILL" "$([ -n "$child" ] && gone "$child" && echo true || echo false)"
if [ -n "$child" ]; then kill -KILL "$child" 2> /dev/null || true; fi

echo "case W6: the binary's stdin, stdout and stderr pass byte for byte, and the wrapper adds no byte of its own"
d="$work/w6"
mkdir -p "$d"
program "$d/speaker" << 'EOF'
#!/usr/bin/env bash
# Copies its stdin to its stdout, then writes to stdout and to stderr a line each and then bytes
# with no newline after them, a NUL and bytes that are no UTF-8 among them.
cat
printf 'to stdout\n'
printf 'to stderr\n' >&2
printf 'a NUL \000, a \377 and no newline'
printf 'a \001, a \376 and no newline' >&2
EOF
printf 'stdin with a NUL \000, a \377 and no newline' > "$d/stdin"
"$d/speaker" < "$d/stdin" > "$d/direct.out" 2> "$d/direct.err"
check "the control: run alone, the binary writes the bytes it was given and its own" "$(holds "$d/direct.out" 'stdin with a NUL \000, a \377 and no newlineto stdout\na NUL \000, a \377 and no newline')"
env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows" "$wrapper" "$d/speaker" < "$d/stdin" > "$d/record.out" 2> "$d/record.err" && status=0 || status=$?
check "through a wrapper that records, it exits 0 and writes one row" "$(is "$status:$(row "$d/rows" 'len(rows)')" 0:1)"
check "its stdout is the same bytes as run alone" "$(same "$d/record.out" "$d/direct.out")"
check "and so is its stderr" "$(same "$d/record.err" "$d/direct.err")"
env -u GO_CPU_DIR -u RUNNER_TEMP "$wrapper" "$d/speaker" < "$d/stdin" > "$d/exec.out" 2> "$d/exec.err" && status=0 || status=$?
check "through a wrapper that records nothing, its stdout and stderr are the same bytes too" "$(is "$status:$(same "$d/exec.out" "$d/direct.out"):$(same "$d/exec.err" "$d/direct.err")" 0:true:true)"

echo "case W7: the binary's arguments and environment reach it unchanged, but for the GO_CPU_PARENT a .test binary is given"
d="$work/w7"
mkdir -p "$d"
program "$d/x.test" << 'EOF'
#!/usr/bin/env python3
# Copies its command line and its environment, as the kernel gave them at its exec, to
# cmdline and environ beside it. Python's own os.environ is no copy: at its start Python sets
# LC_CTYPE in it when the locale is C.
import os, sys
here = os.path.dirname(os.path.abspath(sys.argv[0]))
for name in ("cmdline", "environ"):
    with open("/proc/self/" + name, "rb") as source, open(os.path.join(here, name), "wb") as target:
        target.write(source.read())
EOF
cp "$d/x.test" "$d/xtool"
# compare <a's cmdline> <a's environ> <b's cmdline> <b's environ> <GO_CPU_PARENT b should hold>:
# `same` when the two command lines are the same bytes, and the two environments the same
# entries in the same order once b's GO_CPU_PARENT is taken out of it, which must hold the last
# argument, or be absent when that is empty; else what differs.
compare() {
  python3 -c '
import sys
def read(path):
    with open(path, "rb") as f:
        return f.read()
a_cmd, a_env, b_cmd, b_env = (read(p) for p in sys.argv[1:5])
want = sys.argv[5].encode()
a_env = a_env.split(b"\0")
b_env = b_env.split(b"\0")
parent = [e for e in b_env if e.startswith(b"GO_CPU_PARENT=")]
b_env = [e for e in b_env if not e.startswith(b"GO_CPU_PARENT=")]
if a_cmd != b_cmd:
    print("the command lines differ: %r and %r" % (a_cmd, b_cmd))
elif a_env != b_env:
    print("the environments differ: %r" % (set(a_env) ^ set(b_env) or "in order",))
elif parent != ([b"GO_CPU_PARENT=" + want] if want else []):
    print("GO_CPU_PARENT is %r" % (parent,))
else:
    print("same")
' "$@" 2>&1 || true
}
# capture <name> <command...>: runs <command>, which runs x.test or xtool, and keeps the command
# line and the environment the binary copied as <name>.cmdline and <name>.environ.
capture() {
  local name=$1
  shift
  rm -f "$d/cmdline" "$d/environ"
  run "$@"
  mv "$d/cmdline" "$d/$name.cmdline" 2> /dev/null || true
  mv "$d/environ" "$d/$name.environ" 2> /dev/null || true
}
args=('' 'two words' $'a\nnewline' ' ' '-test.run=^TestA$')
# The locale is unset, so Python's start sets LC_CTYPE in the wrapper's own os.environ: a wrapper
# that passed that on would add it to the binary's environment.
environment=(-u GO_CPU_PARENT -u LANG -u LC_ALL -u LC_CTYPE W7_VALUE=$'spaces and\na newline' W7_EMPTY=)
for binary in x.test xtool; do
  capture "$binary-direct" env "${environment[@]}" GO_CPU_DIR="$d/rows-$binary" "$d/$binary" "${args[@]}"
  capture "$binary-wrapped" env "${environment[@]}" GO_CPU_DIR="$d/rows-$binary" "$wrapper" "$d/$binary" "${args[@]}"
  check "$binary through the wrapper exits 0 and writes one row" "$(is "$status:$(row "$d/rows-$binary" 'len(rows)')" 0:1)"
  check "whose args are the arguments, the empty one, the spaces and the newline among them" "$(row "$d/rows-$binary" 'r["args"] == a' "${args[@]}")"
done
check "x.test gets the command line it gets alone, and the same environment in the same order but for a GO_CPU_PARENT of its row's id" "$(is "$(compare "$d/x.test-direct.cmdline" "$d/x.test-direct.environ" "$d/x.test-wrapped.cmdline" "$d/x.test-wrapped.environ" "$(row "$d/rows-x.test" 'r["id"]')")" same)"
check "xtool, no .test binary, gets the same command line and the same environment, with no GO_CPU_PARENT" "$(is "$(compare "$d/xtool-direct.cmdline" "$d/xtool-direct.environ" "$d/xtool-wrapped.cmdline" "$d/xtool-wrapped.environ" '')" same)"
capture inherited-direct env "${environment[@]}" GO_CPU_PARENT=the-row-above GO_CPU_DIR="$d/rows-inherited" "$d/xtool" "${args[@]}"
capture inherited-wrapped env "${environment[@]}" GO_CPU_PARENT=the-row-above GO_CPU_DIR="$d/rows-inherited" "$wrapper" "$d/xtool" "${args[@]}"
check "xtool under a wrapper that inherited a GO_CPU_PARENT gets the same environment, that GO_CPU_PARENT in it" "$(is "$status:$(same "$d/inherited-direct.environ" "$d/inherited-wrapped.environ"):$(same "$d/inherited-direct.cmdline" "$d/inherited-wrapped.cmdline")" 0:true:true)"
check "and the row's parent is that GO_CPU_PARENT" "$(is "$(row "$d/rows-inherited" 'r["parent"]')" the-row-above)"

echo "case W8: a record directory that cannot be written leaves the binary's run as it is, and no row"
d="$work/w8"
mkdir -p "$d"
program "$d/talker" << 'EOF'
#!/usr/bin/env bash
# Leaves the file its first argument names as a mark that it ran, writes a line to stdout and one
# to stderr, and exits 5.
touch "$1"
printf 'talker out\n'
printf 'talker err\n' >&2
exit 5
EOF
printf 'a regular file\n' > "$d/file"
mkdir "$d/locked"
chmod 0500 "$d/locked"
targets=("$d/file/rows" "$d/file" "$d/locked")
labels=("a path under a regular file" "a regular file" "a directory of mode 0500")
for i in 0 1 2; do
  rm -f "$d/ran"
  run env -u GO_CPU_PARENT GO_CPU_DIR="${targets[$i]}" "$wrapper" "$d/talker" "$d/ran"
  check "with GO_CPU_DIR ${labels[$i]}, the binary runs" "$([ -e "$d/ran" ] && echo true || echo false)"
  check "and the wrapper exits 5, the binary's status" "$(is "$status" 5)"
  check "and prints the binary's bytes and no byte of its own" "$(is "$(holds "$work/stdout" 'talker out\n'):$(holds "$work/stderr" 'talker err\n')" true:true)"
done
check "the regular file still holds what it held" "$(holds "$d/file" 'a regular file\n')"
check "the control: this user cannot write in the directory of mode 0500 either" "$(touch "$d/locked/probe" 2> /dev/null && echo false || echo true)"
check "which holds no row and no temporary file" "$(is "$(ls -A "$d/locked")" "")"

echo "case W9: with neither GO_CPU_DIR nor RUNNER_TEMP set, the wrapper becomes the binary and writes nothing"
d="$work/w9"
mkdir -p "$d/pkg"
program "$d/plain" << 'EOF'
#!/usr/bin/env bash
# Writes its parent's pid to the file its first argument names, writes bytes with no newline after
# them to stdout and to stderr, and exits 4.
echo "$PPID" > "$1"
printf 'plain out'
printf 'plain err' >&2
exit 4
EOF
touch "$d/before"
run_in "$d/pkg" env -u GO_CPU_DIR -u RUNNER_TEMP -u GO_CPU_PARENT "$wrapper" "$d/plain" "$d/ppid"
check "the wrapper exits 4, the binary's status" "$(is "$status" 4)"
check "its stdout and stderr are the binary's bytes alone" "$(is "$(holds "$work/stdout" 'plain out'):$(holds "$work/stderr" 'plain err')" true:true)"
check "the binary ran in the wrapper's own process, whose parent is this shell" "$(is "$(cat "$d/ppid" 2> /dev/null || true)" "$$")"
check "and no file but the binary's own was written anywhere in the scratch tree" "$(is "$(find "$work" -newer "$d/before" -type f ! -path "$d/ppid" ! -path "$work/stdout" ! -path "$work/stderr")" "")"
rm -f "$d/ppid"
run_in "$d/pkg" env -u GO_CPU_PARENT GO_CPU_DIR= RUNNER_TEMP= "$wrapper" "$d/plain" "$d/ppid"
check "both set but empty count as unset: the binary runs in the wrapper's own process" "$(is "$status:$(cat "$d/ppid" 2> /dev/null || true)" "4:$$")"

echo "case W10: a wrapper inside a wrapper under a test binary names the outer row as its parent"
d="$work/w10"
mkdir -p "$d"
program "$d/x.test" << 'EOF'
#!/usr/bin/env bash
# A test binary that writes its pid to the file its third argument names, then runs the wrapper
# its first argument names on the binary its second names.
echo "$$" > "$3"
"$1" "$2"
EOF
run env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows" "$wrapper" "$d/x.test" "$wrapper" "$quick" "$d/pid"
check "the outer wrapper exits 0 and two rows are written" "$(is "$status:$(row "$d/rows" 'len(rows)')" 0:2)"
check "the outer row's parent is null" "$(is "$(row "$d/rows" '[x["parent"] for x in rows if x["binary"] == "x.test"]')" '[null]')"
check "the inner row's parent is the outer row's id" "$(row "$d/rows" 'len(rows) == 2 and [x["parent"] for x in rows if x["binary"] == "quick"] == [x["id"] for x in rows if x["binary"] == "x.test"]')"
check "and the inner row's go_pid is the test binary that ran it" "$(is "$(row "$d/rows" '[x["go_pid"] for x in rows if x["binary"] == "quick"]')" "[$(cat "$d/pid" 2> /dev/null || echo 0)]")"

echo "case W11: twenty wrappers at once in one directory write twenty whole rows"
d="$work/w11"
mkdir -p "$d"
program "$d/nap" << 'EOF'
#!/usr/bin/env bash
# Sleeps long enough that the twenty runs overlap.
sleep 0.3
EOF
pids=()
for i in $(seq 20); do
  env -u GO_CPU_PARENT GO_CPU_DIR="$d/rows" "$wrapper" "$d/nap" < /dev/null > /dev/null 2>&1 &
  pids+=("$!")
done
failed=0
for pid in "${pids[@]}"; do
  wait "$pid" || failed=$((failed + 1))
done
check "all twenty wrappers exit 0" "$(is "$failed" 0)"
check "there are twenty row files" "$(is "$(row "$d/rows" 'len(rows)')" 20)"
check "each is one whole JSON object of a row" "$(row "$d/rows" 'len(rows) == 20 and all(type(x) is dict and x["v"] == 1 and x["exit"] == 0 for x in rows)')"
check "their ids are twenty, each its file's name" "$(row "$d/rows" 'len({x["id"] for x in rows}) == 20 and sorted(x["id"] + ".json" for x in rows) == [n for n in names if not n.startswith(".")]')"
check "and no temporary file is left" "$(is "$(row "$d/rows" '[n for n in names if n.startswith(".") or not n.endswith(".json")]')" '[]')"

echo "case W13: GO_CPU_DIR wins over RUNNER_TEMP, and RUNNER_TEMP alone puts the row under go-test-events/cpu"
d="$work/w13"
mkdir -p "$d/runner" "$d/runner-alone" "$d/runner-empty"
run env -u GO_CPU_PARENT GO_CPU_DIR="$d/cpu" RUNNER_TEMP="$d/runner" "$wrapper" "$quick"
check "with both set, the row is under GO_CPU_DIR" "$(is "$status:$(row "$d/cpu" 'len(rows)')" 0:1)"
check "and nothing is under RUNNER_TEMP" "$(is "$(find "$d/runner" -mindepth 1)" "")"
run env -u GO_CPU_PARENT -u GO_CPU_DIR RUNNER_TEMP="$d/runner-alone" "$wrapper" "$quick"
check "with RUNNER_TEMP alone, the row is under \$RUNNER_TEMP/go-test-events/cpu" "$(is "$status:$(row "$d/runner-alone/go-test-events/cpu" 'len(rows)')" 0:1)"
check "and nothing else is under RUNNER_TEMP" "$(is "$(find "$d/runner-alone" -mindepth 1 ! -path "$d/runner-alone/go-test-events" ! -path "$d/runner-alone/go-test-events/cpu" ! -path "$d/runner-alone/go-test-events/cpu/*.json")" "")"
run env -u GO_CPU_PARENT GO_CPU_DIR= RUNNER_TEMP="$d/runner-empty" "$wrapper" "$quick"
check "an empty GO_CPU_DIR counts as unset, so the row is under RUNNER_TEMP's" "$(is "$status:$(row "$d/runner-empty/go-test-events/cpu" 'len(rows)')" 0:1)"

echo "case W12: the real go test, go test -list, go test -json and go run run each binary through the wrapper, and go vet and go list run none"
d="$work/w12"
m="$d/mod"
mkdir -p "$m/a" "$m/b" "$m/cmd/x" "$d/fail"
printf 'module example.com/m\n\ngo 1.21\n' > "$m/go.mod"
for p in a b; do
  printf 'package %s\n\nfunc Value() int { return 1 }\n' "$p" > "$m/$p/$p.go"
  printf 'package %s\n\nimport "testing"\n\nfunc TestValue(t *testing.T) {\n\tif Value() != 1 {\n\t\tt.Fatal("Value is not 1")\n\t}\n}\n' "$p" > "$m/$p/${p}_test.go"
done
printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("x ran") }\n' > "$m/cmd/x/main.go"
printf 'module example.com/f\n\ngo 1.21\n' > "$d/fail/go.mod"
printf 'package f\n\nimport "testing"\n\nfunc TestFails(t *testing.T) { t.Fatal("this test fails") }\n' > "$d/fail/f_test.go"
# go_in <dir> <rows dir> <GOFLAGS> <go argument...>: runs go from <dir>, through run, with a build
# cache, module cache, config directory and GOFLAGS of this case's own, no proxy, no toolchain but
# the local one, and the wrapper's rows going to <rows dir>. GITHUB_WORKSPACE is the module, so a
# row's cwd is its package's directory within it.
go_in() {
  local dir=$1 rows=$2 flags=$3
  shift 3
  run_in "$dir" env -u GO_CPU_PARENT -u RUNNER_TEMP GO_CPU_DIR="$rows" GITHUB_WORKSPACE="$m" \
    GOCACHE="$d/gocache" GOPATH="$d/gopath" GOMODCACHE="$d/gopath/pkg/mod" GOFLAGS="$flags" \
    GOENV=off GOWORK=off GOPROXY=off GOTOOLCHAIN=local CGO_ENABLED=0 XDG_CONFIG_HOME="$d/config" \
    go "$@"
}
exec_flag="-exec=$wrapper"
go_in "$m" "$d/rows-test" "$exec_flag" test -count=1 ./...
check "go test ./... exits 0" "$(is "$status" 0)"
check "and reports both packages ok" "$(is "$(contains "$(cat "$work/stdout")" '^ok[[:space:]]*example.com/m/a')$(contains "$(cat "$work/stdout")" '^ok[[:space:]]*example.com/m/b')" truetrue)"
check "it wrote one row for each test binary, a.test and b.test, each with its package's directory" "$(is "$(row "$d/rows-test" 'sorted([x["binary"], x["cwd"]] for x in rows)')" '[["a.test", "a"], ["b.test", "b"]]')"
check "each row's parent is null and its exit 0" "$(row "$d/rows-test" 'len(rows) == 2 and all(x["parent"] is None and x["exit"] == 0 for x in rows)')"
check "and neither ran with -test.list" "$(row "$d/rows-test" 'len(rows) == 2 and not any(y.startswith("-test.list") for x in rows for y in x["args"])')"
go_in "$m" "$d/rows-list" "$exec_flag" test -count=1 -list . ./a
check "go test -list . ./a exits 0 and lists TestValue" "$(is "$status:$(contains "$(cat "$work/stdout")" '^TestValue$')" 0:true)"
check "it wrote one row, of a.test, whose args hold -test.list" "$(row "$d/rows-list" 'r["binary"] == "a.test" and any(y.startswith("-test.list") for y in r["args"])')"
go_in "$m" "$d/rows-run" "$exec_flag" run ./cmd/x
check "go run ./cmd/x exits 0 and the program's line reaches stdout, alone" "$(is "$status:$(holds "$work/stdout" 'x ran\n')" 0:true)"
check "it wrote one row, of the binary x, no .test" "$(is "$(row "$d/rows-run" 'r["binary"]')" x)"
go_in "$m" "$d/rows-vet" "$exec_flag" vet ./...
check "go vet ./... with the same GOFLAGS exits 0 and writes no row" "$(is "$status:$(row "$d/rows-vet" 'names')" '0:[]')"
go_in "$m" "$d/rows-golist" "$exec_flag" list ./...
check "go list ./... with the same GOFLAGS exits 0, lists the three packages and writes no row" "$(is "$status:$(tr '\n' ' ' < "$work/stdout"):$(row "$d/rows-golist" 'names')" '0:example.com/m/a example.com/m/b example.com/m/cmd/x :[]')"
go_in "$m" "$d/rows-json" "$exec_flag" test -json -count=1 ./...
json_status=$status
cp "$work/stdout" "$d/json-wrapped"
go_in "$m" "$d/rows-json-alone" "" test -json -count=1 ./...
cp "$work/stdout" "$d/json-alone"
check "go test -json exits 0 with the wrapper and without it" "$(is "$json_status:$status" 0:0)"
# Each event's Time and Elapsed, and the seconds the output lines print, differ between any two
# runs. go test prints the packages' events package by package, so they are compared in order
# within each package.
check "and gives the same events, each package's in the same order, once their times are dropped" "$(python3 -c '
import json, re, sys
def events(path):
    by_package = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            event = json.loads(line)
            event.pop("Time", None)
            event.pop("Elapsed", None)
            if "Output" in event:
                event["Output"] = re.sub(r"\d+\.\d+s\b", "<seconds>", event["Output"])
            by_package.setdefault(event.get("Package") or event.get("ImportPath"), []).append(event)
    return by_package
try:
    wrapped, alone = events(sys.argv[1]), events(sys.argv[2])
    passed = {e.get("Package") for events in wrapped.values() for e in events if e["Action"] == "pass" and "Test" not in e}
    print("true" if wrapped == alone and {"example.com/m/a", "example.com/m/b"} <= passed else "false")
except Exception as e:
    print("unreadable: %r" % (e,))
' "$d/json-wrapped" "$d/json-alone")"
check "and it wrote a row for each of the two test binaries" "$(is "$(row "$d/rows-json" 'sorted(x["binary"] for x in rows)')" '["a.test", "b.test"]')"
go_in "$d/fail" "$d/rows-fail" "$exec_flag" test -count=1 ./...
check "go test of a package whose test fails still exits 1 with the wrapper" "$(is "$status" 1)"
check "and prints the test's FAIL and the package's" "$(is "$(contains "$(cat "$work/stdout")" '^--- FAIL: TestFails')$(contains "$(cat "$work/stdout")" '^FAIL'$'\t''example.com/f')" truetrue)"
check "and the row of f.test has exit 1" "$(is "$(row "$d/rows-fail" '[r["binary"], r["exit"]]')" '["f.test", 1]')"

# The cases of go-cpu-runner.py.
runner="$script_dir/go-cpu-runner.py"

# matches <text> <extended regex>: true/false for check; the regex is bash's own.
matches() { [[ $1 =~ $2 ]] && echo true || echo false; }

# lines_in <file>: the number of lines in <file>, 0 when there is none.
lines_in() { { wc -l < "$1"; } 2> /dev/null || echo 0; }

# session_of <pid>: the session of <pid>, or nothing when there is no process <pid>.
session_of() { stat_field "$1" 4; }

# The files of real runners R1, R3 and R4 parse, as the header says.
fixtures="$script_dir/testdata/go-cpu"
leg5_runner="$fixtures/go-test-events-140938293835/cpu/runner"
leg5_log="$fixtures/log-140938293835.excerpt.txt"

echo "case R1: the parse of lscpu and /proc/cpuinfo, on the text of real runners"
run "$runner" parse lscpu "$fixtures/lscpu-140585658657.txt"
check "the lines job 140585658657's nproc; lscpu step printed, on ubuntu-24.04-arm, give the model Neoverse-N2" "$(is "$status:$(cat "$work/stdout")" '0:model="Neoverse-N2"')"
run "$runner" parse lscpu "$leg5_runner/lscpu.txt"
leg5_model=$(cat "$work/stdout")
run "$runner" parse cpuinfo "$leg5_runner/cpuinfo.txt"
check "leg 5's lscpu.txt and cpuinfo.txt give Neoverse-N2, and four CPUs of part 0xd49" "$(is "$leg5_model|$status:$(cat "$work/stdout")" 'model="Neoverse-N2"|0:part=0xd49:4')"
check "as leg 5's log line printed them" "$(contains "$(< "$leg5_log")" 'go-cpu runner: part=0xd49:4 cpus=4 .* model="Neoverse-N2"$')"
run "$runner" parse lscpu "$fixtures/raw-runner-facts-140938293854.txt"
checks_model=$(cat "$work/stdout")
run "$runner" parse cpuinfo "$fixtures/raw-runner-facts-140938293854.txt"
check "envoy-go-checks' raw group, of ubuntu-24.04 (x64), gives the x64 model, spaces and all, and no part: its /proc/cpuinfo names the model in lower case, and no CPU part" "$(is "$checks_model|$status:$(cat "$work/stdout")" 'model="INTEL(R) XEON(R) PLATINUM 8573C"|0:part=')"
check "as envoy-go-checks' log line printed them" "$(contains "$(< "$fixtures/log-140938293854.excerpt.txt")" 'go-cpu runner: part= cpus=4 .* model="INTEL(R) XEON(R) PLATINUM 8573C"$')"

echo "case R2: report sums the rows of each class, reads each Postgres container's cpu.stat and counts the new cache entries"
d="$work/r2"
cpu="$d/temp/go-test-events/cpu"
mkdir -p "$cpu/runner" "$d/home/.cache/go-build/00" "$d/home/.cache/go-build/01" "$d/bin" "$d/empty-temp"
# Five entries in the Go build cache now, of which start counted two.
for entry in 00/a-d 00/b-d 01/c-d 01/d-a README; do
  printf 'x' > "$d/home/.cache/go-build/$entry"
done
printf '{"cache_entries": 2}\n' > "$cpu/runner/runner.json"
# Each field differs from every other, so a sum that reads a wrong field, or adds guest and
# guest_nice, comes out different from the one this case works out.
printf 'cpu  1 2 3 4 5 6 7 8 9 10\ncpu0 1 2 3 4 5 6 7 8 9 10\n' > "$cpu/runner/stat-start.txt"
# put_row <id> <parent, as JSON> <cwd> <binary> <args, as JSON> <user_s> <sys_s> <wall_s>: writes a
# row of the wrapper's schema by hand into the record directory.
put_row() {
  printf '{"v": 1, "id": "%s", "parent": %s, "go_pid": 100, "job": "envoy-go", "step": "__run_3", "cwd": "%s", "binary": "%s", "args": %s, "start": 1.5, "wall_s": %s, "user_s": %s, "sys_s": %s, "self_user_s": 0.0, "self_sys_s": 0.0, "children_user_s": 0.0, "children_sys_s": 0.0, "maxrss_kib": 1024, "exit": 0, "signal": null}\n' \
    "$1" "$2" "$3" "$4" "$5" "$8" "$6" "$7" > "$cpu/$1.json"
}
# Two test rows of one package, a list row, an outside row and a run row, whose sums report adds,
# but for the run row's; and a nested row under the first test row, which its parent's numbers
# already hold. The outside row's binary is no .test and the nested row's cwd is absolute and its
# args list, so a report that tests the classes in another order counts them in another class.
put_row 1-101 null pkg/a a.test '["-test.paniconexit0", "-test.timeout=10m0s"]' 1.2 0.3 2.0
put_row 2-102 null pkg/a a.test '["-test.paniconexit0", "-test.run=^TestB$"]' 2.4 0.6 3.5
put_row 3-103 null pkg/a a.test '["-test.list=^Test"]' 0.1 0.05 0.25
put_row 4-104 null /elsewhere/cmd/tool tool '[]' 3.0 1.0 4.0
put_row 5-105 null cmd/x x '["serve"]' 50.0 50.0 50.0
put_row 6-106 '"1-101"' /tmp/inner c.test '["-test.list=."]' 100.0 100.0 100.0
job_line='go-cpu job: envoy-go binaries=4 nested=1 runs=1 user=6.70 sys=1.95 cpu=8.65 wall=9.75'
# A docker that appends its arguments to docker.calls, one call to a line, and prints the
# cpu.stat beside that file.
program "$d/bin/docker" << 'EOF'
#!/usr/bin/env bash
here=$(dirname "$0")/..
echo "$*" >> "$here/docker.calls"
cat "$here/cpu.stat"
EOF
printf 'usage_usec 1234567\nuser_usec 1000000\nsystem_usec 234567\nnr_periods 0\nnr_throttled 0\nthrottled_usec 0\n' > "$d/cpu.stat"
cp "$d/cpu.stat" "$d/cpu.stat.with-usage"
in_r2=(env -u GITHUB_JOB RUNNER_TEMP="$d/temp" HOME="$d/home" PATH="$d/bin:$PATH")
number='-?[0-9]+\.[0-9]{2}'
usage_sums="busy=$number user=$number sys=$number iowait=$number steal=$number idle=$number"

run "${in_r2[@]}" GITHUB_JOB=envoy-go "$runner" report --postgres service=pg-one
usage=$(sed -n 2p "$work/stdout")
check "report exits 0, writes nothing to stderr and prints two lines" "$(is "$status:$(cat "$work/stderr"):$(lines_in "$work/stdout")" 0::2)"
check "the job line counts the test, list and outside rows as binaries and the nested and run rows apart, and sums the four" "$(is "$(sed -n 1p "$work/stdout")" "$job_line")"
check "the usage line names the /proc/stat sums, the 3 new cache entries and the 1.23 s of usage_usec, in that order" "$(matches "$usage" "^go-cpu usage: $usage_sums new_cache_entries=3 postgres_cpu=1\\.23\$")"
check "each sum is stat-end.txt's first cpu line less stat-start.txt's, in seconds, with guest and guest_nice in none" "$(python3 -c '
import os, sys
def ticks(path):
    with open(path, encoding="utf-8") as f:
        return [int(x) for x in f.readline().split()[1:9]]
t = [b - a for a, b in zip(ticks(sys.argv[2]), ticks(sys.argv[3]))]
s = lambda n: "%.2f" % (n / os.sysconf("SC_CLK_TCK"))
want = "busy=%s user=%s sys=%s iowait=%s steal=%s idle=%s" % (
    s(t[0] + t[1] + t[2] + t[5] + t[6] + t[7]), s(t[0] + t[1]), s(t[2] + t[5] + t[6]), s(t[4]), s(t[7]), s(t[3]))
print("true" if sys.argv[1].startswith("go-cpu usage: " + want + " ") else "false")
' "$usage" "$cpu/runner/stat-start.txt" "$cpu/runner/stat-end.txt" 2> /dev/null || echo false)"
check "stat-end.txt is a copy of /proc/stat, which begins with its cpu line" "$(matches "$(sed -n 1p "$cpu/runner/stat-end.txt" 2> /dev/null || true)" '^cpu +[0-9]')"
check "it ran docker exec <container> cat /sys/fs/cgroup/cpu.stat once" "$(holds "$d/docker.calls" 'exec pg-one cat /sys/fs/cgroup/cpu.stat\n')"
check "and kept what that printed as postgres-service.cpu.stat" "$(same "$cpu/runner/postgres-service.cpu.stat" "$d/cpu.stat")"

printf 'user_usec 1000000\nsystem_usec 234567\n' > "$d/cpu.stat"
printf '{"v": 1, "id": "7-107", ' > "$cpu/7-107.json"
run "${in_r2[@]}" GITHUB_JOB=envoy-go "$runner" report --postgres service=pg-one
check "with a cpu.stat that holds no usage_usec line, report exits 0 and postgres_cpu is UNKNOWN" "$(is "$status:$(matches "$(sed -n 2p "$work/stdout")" ' new_cache_entries=3 postgres_cpu=UNKNOWN$')" 0:true)"
check "a row file that is no JSON is counted as bad=1 at the end of the job line, the other rows as before" "$(is "$(sed -n 1p "$work/stdout")" "$job_line bad=1")"
rm -f "$cpu/7-107.json" "$d/docker.calls"

run "${in_r2[@]}" GITHUB_JOB=envoy-go "$runner" report
check "with no --postgres, report exits 0, postgres_cpu is NONE and docker is not run" "$(is "$status:$(matches "$(sed -n 2p "$work/stdout")" ' postgres_cpu=NONE$'):$([ -e "$d/docker.calls" ] && echo run || echo none)" 0:true:none)"

cp "$d/cpu.stat.with-usage" "$d/cpu.stat"
run "${in_r2[@]}" GITHUB_JOB=envoy-go "$runner" report --no-rows --postgres service=pg-one
check "report --no-rows exits 0 and prints the usage line alone, with no new_cache_entries and no postgres_cpu" "$(is "$status:$(lines_in "$work/stdout"):$(matches "$(cat "$work/stdout")" "^go-cpu usage: $usage_sums\$")" 0:1:true)"
check "and no job line" "$(is "$(contains "$(cat "$work/stdout")" '^go-cpu job:')" false)"
check "and reads no container" "$(is "$([ -e "$d/docker.calls" ] && echo run || echo none)" none)"

run env -u GITHUB_JOB RUNNER_TEMP="$d/empty-temp" HOME="$d/home" PATH="$d/bin:$PATH" "$runner" report
check "with no start before it, no row and no GITHUB_JOB, report exits 0" "$(is "$status" 0)"
check "and prints UNKNOWN for the job, zeros for the rows, UNKNOWN for each /proc/stat sum and the cache, and NONE" "$(holds "$work/stdout" 'go-cpu job: UNKNOWN binaries=0 nested=0 runs=0 user=0.00 sys=0.00 cpu=0.00 wall=0.00\ngo-cpu usage: busy=UNKNOWN user=UNKNOWN sys=UNKNOWN iowait=UNKNOWN steal=UNKNOWN idle=UNKNOWN new_cache_entries=UNKNOWN postgres_cpu=NONE\n')"

run "${in_r2[@]}" "$runner" report --postgres pg-one
check "a --postgres with no NAME= is a usage error: exit 2" "$(is "$status" 2)"

echo "case R3: the /proc/stat deltas of leg 5's two copies of /proc/stat"
run "$runner" parse stat "$leg5_runner/stat-start.txt" "$leg5_runner/stat-end.txt"
check "they are busy 267.43, user 180.49, sys 86.94, iowait 4.52, steal 0 and idle 233.64 s" "$(is "$status:$(cat "$work/stdout")" '0:busy=267.43 user=180.49 sys=86.94 iowait=4.52 steal=0.00 idle=233.64')"
check "the six leg 5's log printed" "$(is "$(cat "$work/stdout")" "$(sed -n 's/.* go-cpu usage: \(busy=.* idle=[^ ]*\) .*/\1/p' "$leg5_log")")"
# By hand: the first cpu line of each file, user nice system idle iowait irq softirq steal from
# its second field, the end's less the start's, over the clock's ticks a second.
check "and the sums by hand of the two files' first cpu lines" "$(is "$(cat "$work/stdout")" "$(awk -v tick="$(getconf CLK_TCK)" '
FNR == 1 { file++ }
$1 == "cpu" && !taken[file]++ { for (i = 2; i <= 9; i++) v[file, i] = $i }
END {
  for (i = 2; i <= 9; i++) d[i] = v[2, i] - v[1, i]
  printf "busy=%.2f user=%.2f sys=%.2f iowait=%.2f steal=%.2f idle=%.2f\n", (d[2] + d[3] + d[4] + d[7] + d[8] + d[9]) / tick, (d[2] + d[3]) / tick, (d[4] + d[7] + d[8]) / tick, d[6] / tick, d[9] / tick, d[5] / tick
}' "$leg5_runner/stat-start.txt" "$leg5_runner/stat-end.txt")")"

echo "case R4: usage_usec of real cpu.stat files, as the postgres_cpu their jobs' logs printed"
run "$runner" parse cpustat "$leg5_runner/postgres-service.cpu.stat"
check "leg 5's service: 45.31 s, as its log printed" "$(is "$status:$(cat "$work/stdout")|$(sed -n 's/.* \(postgres_cpu=[^ ]*\)$/\1/p' "$leg5_log")" '0:postgres_cpu=45.31|postgres_cpu=45.31')"
stats="$fixtures/cpu-stat-140938293848"
run "$runner" parse cpustat "$stats/postgres-service.cpu.stat" "$stats/postgres-pg-stat-statements.cpu.stat"
check "race-impossible's two containers: the sum of both, 116.16 s, as its log printed" "$(is "$status:$(cat "$work/stdout")|$(sed -n 's/.* \(postgres_cpu=[^ ]*\)$/\1/p' "$fixtures/log-140938293848.excerpt.txt")" '0:postgres_cpu=116.16|postgres_cpu=116.16')"
check "which is the two files' usage_usec added up by hand" "$(is "$(awk '$1 == "usage_usec" { sum += $2 } END { printf "postgres_cpu=%.2f\n", sum / 1e6 }' "$stats/postgres-service.cpu.stat" "$stats/postgres-pg-stat-statements.cpu.stat")" "$(cat "$work/stdout")")"
grep -v '^usage_usec ' "$leg5_runner/postgres-service.cpu.stat" > "$work/no-usage.cpu.stat"
run "$runner" parse cpustat "$work/no-usage.cpu.stat"
check "a cpu.stat with no usage_usec line gives UNKNOWN" "$(is "$status:$(cat "$work/stdout")" 0:postgres_cpu=UNKNOWN)"
run "$runner" parse cpustat "$leg5_runner/postgres-service.cpu.stat" "$work/no-usage.cpu.stat"
check "and so does any one of several" "$(is "$(cat "$work/stdout")" postgres_cpu=UNKNOWN)"

echo "case R5: start --no-sampler leaves no process running; start leaves one detached sampler, which report stops"
d="$work/r5"
mkdir -p "$d/home/.cache/go-build/00" "$d/alone" "$d/sampled"
for entry in 00/a-d 00/b-d README; do
  printf 'x' > "$d/home/.cache/go-build/$entry"
done
# reaper <left> <command...>: runs <command> as a child subreaper (prctl(2)'s
# PR_SET_CHILD_SUBREAPER), so each process <command> leaves running, in a session of its own or
# not, becomes the reaper's child when its parent ends, rather than init's. Once <command> has
# ended it writes the pids of its children that still run into <left>, nothing for none, and
# exits as <command> did.
reaper="$d/reaper"
program "$reaper" << 'EOF'
#!/usr/bin/env python3
import ctypes, os, subprocess, sys
PR_SET_CHILD_SUBREAPER = 36
left = sys.argv[1]
if ctypes.CDLL(None, use_errno=True).prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) != 0:
    with open(left, "w") as f:
        f.write("unreadable: no subreaper\n")
    sys.exit(125)
code = subprocess.run(sys.argv[2:]).returncode
running = []
for name in os.listdir("/proc"):
    if not name.isdigit():
        continue
    try:
        with open(f"/proc/{name}/stat", "rb") as f:
            stat = f.read()
    except OSError:
        continue
    state, ppid = stat[stat.rindex(b")") + 2 :].split()[:2]
    if int(ppid) == os.getpid() and state != b"Z":
        running.append(int(name))
with open(left, "w") as f:
    f.write(" ".join(str(pid) for pid in sorted(running)))
sys.exit(code if code >= 0 else 128 - code)
EOF

alone="$d/alone/go-test-events/cpu/runner"
before=$(date +%s.%N)
run "$reaper" "$d/left-alone" env RUNNER_TEMP="$d/alone" HOME="$d/home" GOFLAGS=-exec=/x/y "$runner" start --no-sampler
after=$(date +%s.%N)
# R6 reads this run's output.
cp "$work/stdout" "$d/alone.out"
check "start --no-sampler exits 0 and writes nothing to stderr" "$(is "$status:$(cat "$work/stderr")" 0:)"
check "it leaves no process running that it started" "$(is "$(cat "$d/left-alone" 2> /dev/null || echo missing)" "")"
check "it writes runner.json, stat-start.txt, lscpu.txt and cpuinfo.txt, and no sampler.pid and no proc-stat.tsv" "$(is "$(ls -A "$alone" 2> /dev/null | tr '\n' ' ')" 'cpuinfo.txt lscpu.txt runner.json stat-start.txt ')"
check "stat-start.txt is a copy of /proc/stat, which begins with its cpu line" "$(matches "$(sed -n 1p "$alone/stat-start.txt" 2> /dev/null || true)" '^cpu +[0-9]')"

sampled="$d/sampled/go-test-events/cpu/runner"
sampled_before=$(date +%s.%N)
run "$reaper" "$d/left-sampled" env RUNNER_TEMP="$d/sampled" HOME="$d/home" "$runner" start
started=$(now_us)
sampler=$(cat "$sampled/sampler.pid" 2> /dev/null || true)
check "start exits 0 and writes a pid to sampler.pid" "$(is "$status:$(matches "$sampler" '^[0-9]+$')" 0:true)"
check "the one process it leaves running is the sampler that sampler.pid names" "$(is "$(cat "$d/left-sampled" 2> /dev/null || echo missing)" "${sampler:-none}")"
check "which runs" "$([ -n "$sampler" ] && ! gone "$sampler" && echo true || echo false)"
check "with /dev/null as its stdin, stdout and stderr, so it holds none of the step's pipes" "$(is "$(readlink "/proc/${sampler:-0}/fd/0" "/proc/${sampler:-0}/fd/1" "/proc/${sampler:-0}/fd/2" 2> /dev/null | tr '\n' ' ' || true)" '/dev/null /dev/null /dev/null ')"
check "and leads a session of its own" "$(is "$(session_of "${sampler:-0}")" "${sampler:-none}")"
# Two ticks of the 2 s sampler, with room for a loaded runner: the harness pool runs this harness
# beside others, and a check that waits only one tick's slack would fail on load, not on a fault.
while [ "$(lines_in "$sampled/proc-stat.tsv")" -lt 2 ] && [ "$(($(now_us) - started))" -lt 10000000 ]; do
  sleep 0.1
done
check "within 10 s of start it has appended two lines to proc-stat.tsv" "$([ "$(lines_in "$sampled/proc-stat.tsv")" -ge 2 ] && echo true || echo false)"
check "each the epoch seconds of its tick, a tab and the first line of /proc/stat, the second 1.5 s or more after the first" "$(python3 -c '
import re, sys, time
with open(sys.argv[1], encoding="utf-8") as f:
    lines = f.read().splitlines()
stamps = [float(line.split("\t")[0]) for line in lines]
print("true" if len(lines) >= 2
      and all(re.fullmatch(r"[0-9]+\.[0-9]{3}\tcpu +[0-9]+( [0-9]+){6,}", line) for line in lines)
      and all(float(sys.argv[2]) <= stamp <= time.time() for stamp in stamps)
      and stamps[1] - stamps[0] >= 1.5 else "false")
' "$sampled/proc-stat.tsv" "$sampled_before" 2> /dev/null || echo false)"
run env RUNNER_TEMP="$d/sampled" HOME="$d/home" "$runner" report --no-rows
check "report exits 0" "$(is "$status" 0)"
check "and the sampler is gone when it returns" "$([ -n "$sampler" ] && gone "$sampler" && echo true || echo false)"
check "it wrote stat-end.txt beside stat-start.txt" "$([ -s "$sampled/stat-end.txt" ] && [ -s "$sampled/stat-start.txt" ] && echo true || echo false)"
# A process either start left, and report did not stop, must not outlive this script.
for pid in $(cat "$d/left-alone" "$d/left-sampled" 2> /dev/null || true) $sampler; do
  if [[ $pid =~ ^[0-9]+$ ]] && ! gone "$pid"; then
    kill -KILL "$pid" 2> /dev/null || true
  fi
done

echo "case R6: start prints its line in the documented order and quoting, then the raw facts it read in a group"
# This reads the output of R5's start --no-sampler, so the harness pays for the probe twice only.
out="$d/alone.out"
line=$(sed -n 1p "$out")
check "its first line is the go-cpu runner: line, and it prints no other" "$(is "$(matches "$line" '^go-cpu runner: '):$(grep -c '^go-cpu runner: ' "$out" || true)" true:1)"
check "the line holds part, cpus, mem_kib, kernel, probe_one, probe_all, cache_entries, goflags and model, in that order and quoting" "$(matches "$line" '^go-cpu runner: part=(0x[0-9a-f]+:[0-9]+(,0x[0-9a-f]+:[0-9]+)*)? cpus=[0-9]+ mem_kib=[0-9]+ kernel=[^ ]+ probe_one=[0-9]+\.[0-9] probe_all=[0-9]+\.[0-9] cache_entries=[0-9]+ goflags="[^"]*" model="[^"]+"$')"
# field <name>: the value <name>= holds in the line, up to the next space.
field() { sed -n "s/.* $1=\\([^ ]*\\).*/\\1/p" <<< "$line"; }
check "goflags is the environment's GOFLAGS, in quotes" "$(contains "$line" ' goflags="-exec=/x/y" ')"
check "cpus is what nproc prints" "$(is "$(field cpus)" "$(env -u OMP_NUM_THREADS -u OMP_THREAD_LIMIT nproc)")"
check "mem_kib is /proc/meminfo's MemTotal" "$(is "$(field mem_kib)" "$(awk '$1 == "MemTotal:" { print $2 }' /proc/meminfo)")"
check "kernel is what uname -r prints" "$(is "$(field kernel)" "$(uname -r)")"
check "cache_entries counts the three files under ~/.cache/go-build" "$(is "$(field cache_entries)" 3)"
check "part is each CPU part of /proc/cpuinfo with its count, in order of first appearance" "$(is "$(field part)" "$(awk -F: '
$1 ~ /^CPU part[ \t]*$/ { v = $2; gsub(/^[ \t]+|[ \t]+$/, "", v); if (!(v in n)) order[++k] = v; n[v]++ }
END { for (i = 1; i <= k; i++) printf "%s%s:%d", (i > 1 ? "," : ""), order[i], n[order[i]] }' /proc/cpuinfo)")"
check "model is lscpu's Model name, the last field" "$(is "${line#* model=\"}" "$(LC_ALL=C lscpu | sed -n 's/^Model name:[[:space:]]*//p' | sed 's/[[:space:]]*$//' | awk '!seen[$0]++ { printf "%s%s", (n++ ? ", " : ""), $0 }')\"")"
check "both probe rates are above 0 MB/s" "$(awk -v one="$(field probe_one)" -v all="$(field probe_all)" 'BEGIN { print (one + 0 > 0 && all + 0 > 0) ? "true" : "false" }')"
{
  printf '%s\n' "$line" '::group::go-cpu raw runner facts'
  cat "$alone/lscpu.txt" "$alone/cpuinfo.txt"
  printf '::endgroup::\n'
} > "$d/alone.expected" 2> /dev/null || true
check "after the line it prints lscpu's output and /proc/cpuinfo, as it kept them in lscpu.txt and cpuinfo.txt, between ::group::go-cpu raw runner facts and ::endgroup::" "$(same "$out" "$d/alone.expected")"
check "lscpu.txt is lscpu's output, with its Model name line" "$(contains "$(cat "$alone/lscpu.txt" 2> /dev/null || true)" '^Model name:')"
check "cpuinfo.txt is /proc/cpuinfo whole: as many lines, and the same processor lines" "$(is "$(lines_in "$alone/cpuinfo.txt"):$(grep '^processor' "$alone/cpuinfo.txt" 2> /dev/null | tr '\n' ' ' || true)" "$(lines_in /proc/cpuinfo):$(grep '^processor' /proc/cpuinfo | tr '\n' ' ')")"
check "runner.json holds the line's facts, and the start time, in epoch seconds, within the run" "$(python3 -c '
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    j = json.load(f)
shown = lambda v, spelled="%s": "UNKNOWN" if v is None else spelled % v
line = "go-cpu runner: part=%s cpus=%s mem_kib=%s kernel=%s probe_one=%s probe_all=%s cache_entries=%s goflags=\"%s\" model=\"%s\"" % (
    shown(j["part"]), shown(j["cpus"]), shown(j["mem_kib"]), shown(j["kernel"]), shown(j["probe_one"], "%.1f"),
    shown(j["probe_all"], "%.1f"), shown(j["cache_entries"]), j["goflags"], shown(j["model"]))
print("true" if sorted(j) == sorted(["part", "cpus", "mem_kib", "kernel", "probe_one", "probe_all", "cache_entries", "goflags", "model", "start_time"])
      and line == sys.argv[2] and float(sys.argv[3]) <= j["start_time"] <= float(sys.argv[4]) else "false")
' "$alone/runner.json" "$line" "$before" "$after" 2> /dev/null || echo false)"

summary "go-cpu-exec.py and go-cpu-runner.py"
