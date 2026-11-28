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
# The cases of `go-cpu-runner.py`, R1-R5, go below the W cases, before the summary.
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

# gone <pid>: succeeds when no process <pid> runs: there is none, or it is a zombie, which has
# ended and waits only for whoever adopted it to reap it. It forks nothing, so a loop of it
# measures the process rather than itself.
gone() {
  local stat
  { read -r stat < "/proc/$1/stat"; } 2> /dev/null || return 0
  stat=${stat##*) }
  [ "${stat%% *}" = Z ]
}

# parent_of <pid>: the pid of <pid>'s parent, or nothing when there is no process <pid>.
parent_of() {
  local stat state ppid rest
  { read -r stat < "/proc/$1/stat"; } 2> /dev/null || return 0
  read -r state ppid rest <<< "${stat##*) }"
  echo "$ppid"
}

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

# R1-R5, the cases of go-cpu-runner.py, go here, after the wrapper's cases and before the summary.

summary "go-cpu-exec.py"
