import { beforeAll, expect, test } from "bun:test";
import { copyFileSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { hostGate, hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { ownGroup } from "./own-group";
import { rowWaits, untilGone } from "./row-waits";
import { scriptFunctions } from "./script-functions";

// lib/process-tree.sh (sourced through lib/rig.sh): how a proof knows a process it started by the
// pid it started, never by a search of the machine's /proc — record_pid_sh and recorded_pid,
// launched, child_of, the proc_* reads of one process and omp_program, and the rig helpers that use
// them. Each row runs under the stage scripts' own `set -Eeuo pipefail`, with an ERR trap that
// prints, against processes it starts: the stand-in programs are copies of sleep under names of
// their own, so the kernel's record of what a process runs (/proc/<pid>/exe) tells them apart from
// every sleep the machine runs.
const rig = join(import.meta.dir, "rig.sh");
const root = join(import.meta.dir, "..", "..", "..");
const dir = temporaryDirectory("process-tree-test");
// perl: run starts every harness through ownGroup (lib/own-group.ts).
const host = hostGate({ programs: ["bash", "sleep", "sh", "perl"] });
const program = join(dir, "program");
const literalPattern = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const other = join(dir, "other");
beforeAll(() => {
  if (host.missing) return;
  const sleep = hostTool("sleep");
  if (sleep === null) throw new Error("sleep is not on PATH");
  copyFileSync(sleep, program);
  copyFileSync(sleep, other);
});

// run runs BODY with lib/rig.sh sourced and a scratch directory of its own in $scratch; every
// process BODY records in `started` is stopped with everything under it when BODY ends (stop_tree,
// lib/process-tree.sh), its children's children included. start_of PID prints PID's start time,
// stat field 22. ended_pid prints the pid of a program this shell started, killed and reaped: a pid
// that names no process, whose environment no one can read, where pid 1's can be read (a sandbox
// whose init runs as this user). The harness shell, and so everything it starts with `&`, runs in
// a process group of its own (ownGroup): the rows stop what they end, and a stopped member of the
// test runner's own group hangs the runner up where that group is orphaned, as in a Legion pod.
//
// Each wait of a row, its shell's (until_true) and the TypeScript side's (untilGone,
// lib/row-waits.ts), ends on the event it waits for, and is bounded only by the row's bound, 20 s
// before it, never by the wait's own seconds: a loaded host took more than 10 s to deliver a zombie
// (2026-11-20, main d36289cb84, 8 busy loops on two CPUs). Every row's bound is `slow`, which
// every runner reads whatever its command line; Bun ends a row at its bound and kills what the
// row's shell left running, so a wait gives up first, to name what it waited for.
const slow = 60_000;
const waits = rowWaits(slow);
function run(body: string) {
  const script = `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(root)}
work=$(mktemp -d "$DIR/work.XXXXXX") evidence=$work timeout_hook=
note() { :; }
fail() { echo "fail: $*" >&2; exit 1; }
. ${JSON.stringify(rig)}
until_true_deadline=$((SECONDS + ${waits.secondsLeft()}))
scratch=$(mktemp -d "$DIR/run.XXXXXX")
started=()
# The shell's own "Killed" notice of each job the trap stops goes nowhere: it is no output of a row.
trap '{ for p in \${started[@]+"\${started[@]}"}; do stop_tree "$p"; done; } 2>/dev/null' EXIT
start_of() {
  local stat
  local -a fields
  read -r stat <"/proc/$1/stat"
  read -r -a fields <<<"\${stat##*) }"
  printf '%s\\n' "\${fields[19]}"
}
ended_pid() {
  local pid
  "$program" 600 </dev/null >/dev/null 2>&1 &
  pid=$!
  builtin kill -KILL "$pid"
  { wait "$pid" || true; } 2>/dev/null
  printf '%s\\n' "$pid"
}
# laid_out PROGRAM PID… holds once each PID runs PROGRAM by the kernel's record and its environment
# is laid out (environ_laid_out, lib/process-tree.sh): the kernel names PROGRAM in /proc/<pid>/exe
# before it lays out PROGRAM's environment, and a row that reads the environment in between reads
# none.
laid_out() {
  local want=$1 pid
  shift
  for pid; do
    [ "$(readlink "/proc/$pid/exe" 2>/dev/null || :)" = "$want" ] && environ_laid_out "$pid" || return 1
  done
}
program=$PROGRAM other=$OTHER
${body}`;
  const result = Bun.spawnSync(ownGroup(["bash", "-c", script]), {
    env: { PATH: process.env.PATH ?? "", DIR: dir, PROGRAM: program, OTHER: other },
  });
  return { code: result.exitCode, out: result.stdout.toString(), err: result.stderr.toString() };
}

host.describe("record_pid_sh and recorded_pid", () => {
  test(
    "name the process the command execs into while it runs, and nothing once it has ended",
    () => {
      const r = run(`
sh -c "$record_pid_sh" record-pid "$scratch/pid" "$program" 600 </dev/null >/dev/null 2>&1 &
pid=$!
started+=("$pid")
until_true 10 "the pid file" recorded_pid "$scratch/pid" "$program"
found=$(recorded_pid "$scratch/pid" "$program")
[ "$found" = "$pid" ] && echo "running: the started pid" || echo "running: $found, want $pid"
kill "$pid"
wait "$pid" 2>/dev/null || true
if recorded_pid "$scratch/pid" "$program" >/dev/null; then echo "ended: still named"; else echo "ended: refused"; fi
`);
      expect(r.err).toBe("");
      expect(r.out).toBe("running: the started pid\nended: refused\n");
    },
    slow
  );

  test(
    "refuse a file not written yet, one that holds no start time, a pid of the same program started at another time, and a pid that runs another program",
    () => {
      const r = run(`
if recorded_pid "$scratch/absent" "$program" >/dev/null; then echo "absent: named"; else echo "absent: refused"; fi
printf 'x\\n' >"$scratch/word"
if recorded_pid "$scratch/word" "$program" >/dev/null; then echo "word: named"; else echo "word: refused"; fi
"$program" 600 </dev/null >/dev/null 2>&1 &
same=$!
started+=("$same")
until_true 10 "the program to run" sh -c '[ "$(readlink /proc/$1/exe)" = "$2" ]' sh "$same" "$program"
printf '%s\\n' "$same" >"$scratch/bare"
if recorded_pid "$scratch/bare" "$program" >/dev/null; then echo "no start time: named"; else echo "no start time: refused"; fi
printf '%s %s\\n' "$same" "$(($(start_of "$same") - 1))" >"$scratch/reused"
if recorded_pid "$scratch/reused" "$program" >/dev/null; then echo "another start: named"; else echo "another start: refused"; fi
printf '%s %s\\n' "$same" "$(start_of "$same")" >"$scratch/same"
if recorded_pid "$scratch/same" "$program" >/dev/null; then echo "its own start: named"; else echo "its own start: refused"; fi
"$other" 600 </dev/null >/dev/null 2>&1 &
other_pid=$!
started+=("$other_pid")
until_true 10 "the other program to run" sh -c '[ "$(readlink /proc/$1/exe)" = "$2" ]' sh "$other_pid" "$other"
printf '%s %s\\n' "$other_pid" "$(start_of "$other_pid")" >"$scratch/other"
if recorded_pid "$scratch/other" "$program" >/dev/null; then echo "other program: named"; else echo "other program: refused"; fi
`);
      expect(r.err).toBe("");
      expect(r.out).toBe(
        "absent: refused\nword: refused\nno start time: refused\nanother start: refused\n" +
          "its own start: named\nother program: refused\n"
      );
    },
    slow
  );
});

host.describe("launched", () => {
  // A bash that runs `sh -c` of the program as its one child, as controller.go's exec.Command("sh",
  // "-c", …) runs Oh My Pi: bash is a line of one child, sh another, and the program the end of it.
  test(
    "finds the program below a line of single children",
    async () => {
      const r = run(`
bash -c 'sh -c "\\"\\$0\\" 600; :" "$0"; :' "$program" </dev/null >/dev/null 2>&1 &
top=$!
started+=("$top")
until_true 10 "the program to run below $top" launched "$top" "$program"
found=$(launched "$top" "$program")
[ "$(readlink "/proc/$found/exe")" = "$program" ] && echo "found the program $found" || echo "found $found"
`);
      expect(r.err).toBe("");
      const found = /^found the program (\d+)$/.exec(r.out.trim());
      expect(found).not.toBeNull();
      // The row's own end stops the whole line: the program two levels below what it started too.
      await untilGone(Number(found?.[1]), "the program below the line", waits.end());
    },
    slow
  );

  // A pane's `legion worker-shim` runs agent-secrets' keygen and renew beside its agent, so the
  // agent is not the only child of its parent there: a walk that took single children alone would
  // stop at the shim.
  test(
    "finds the program beside a helper its parent runs too",
    async () => {
      const r = run(`
bash -c '"$1" 600 & sh -c "\\"\\$0\\" 600; :" "$0" & wait' "$program" "$other" </dev/null >/dev/null 2>&1 &
top=$!
started+=("$top")
until_true 10 "the program to run below $top" launched "$top" "$program"
found=$(launched "$top" "$program")
[ "$(readlink "/proc/$found/exe")" = "$program" ] && echo "found the program $found" || echo "found $found"
st=0
why=$(child_of "$top") || st=$?
echo "one child: status $st: $why"
`);
      expect(r.err).toBe("");
      const lines = r.out.trimEnd().split("\n");
      expect(lines).toHaveLength(2);
      const found = /^found the program (\d+)$/.exec(lines[0] ?? "");
      expect(found).not.toBeNull();
      const two = /^one child: status 1: process \d+ has 2 children: (\d+) (\d+)$/.exec(
        lines[1] ?? ""
      );
      expect(two).not.toBeNull();
      await untilGone(Number(found?.[1]), "the program beside the helper", waits.end());
      await untilGone(Number(two?.[1]), "a child of the program's parent", waits.end());
      await untilGone(Number(two?.[2]), "a child of the program's parent", waits.end());
    },
    slow
  );

  test(
    "refuses two processes that run the program, none, and a tree where only another program runs",
    async () => {
      const r = run(`
bash -c '"$0" 600 & "$0" 600 & wait' "$program" </dev/null >/dev/null 2>&1 &
two=$!
started+=("$two")
until_true 10 "two children of $two" sh -c '[ "$(cat /proc/$1/task/*/children | wc -w)" = 2 ]' sh "$two"
until_true 10 "both children of $two to run the program" sh -c 'for c in $(cat /proc/$1/task/*/children); do [ "$(readlink /proc/$c/exe)" = "$2" ] || exit 1; done' sh "$two" "$program"
st=0
why=$(launched "$two" "$program") || st=$?
echo "two: status $st: $why"
"$other" 600 </dev/null >/dev/null 2>&1 &
none=$!
started+=("$none")
st=0
why=$(launched "$none" "$program") || st=$?
echo "none: status $st: $why"
bash -c '"$0" 600; :' "$other" </dev/null >/dev/null 2>&1 &
line=$!
started+=("$line")
until_true 10 "the other program below $line" sh -c 'c=$(cat /proc/$1/task/*/children); c=\${c%% *}; [ "$(readlink /proc/$c/exe)" = "$2" ]' sh "$line" "$other"
st=0
why=$(launched "$line" "$program") || st=$?
echo "line: status $st: $why"
`);
      expect(r.err).toBe("");
      const lines = r.out.trimEnd().split("\n");
      expect(lines).toHaveLength(3);
      const two = /^two: status 1: 2 processes below \d+ run (\S+): (\d+) (\d+)$/.exec(
        lines[0] ?? ""
      );
      expect(two).not.toBeNull();
      expect(two?.[1]).toBe(program);
      expect(lines[1]).toMatch(
        new RegExp(`^none: status 1: no process below \\d+ runs ${literalPattern(program)}$`)
      );
      expect(lines[2]).toMatch(
        new RegExp(
          `^line: status 1: no process below \\d+ runs ${literalPattern(program)}; below it: \\d+ ${literalPattern(other)}$`
        )
      );
      await untilGone(Number(two?.[2]), "one of the two programs", waits.end());
      await untilGone(Number(two?.[3]), "one of the two programs", waits.end());
    },
    slow
  );
});

host.describe("child_of, child_running, native_program, first_native and omp_program", () => {
  test(
    "child_of names the one child and refuses none; omp_program takes LEGION_OMP_PATH's real path when it is set",
    () => {
      const r = run(`
bash -c '"$0" 600; :' "$program" </dev/null >/dev/null 2>&1 &
one=$!
started+=("$one")
until_true 10 "the child of $one to run the program" sh -c 'c=$(cat /proc/$1/task/*/children); c=\${c%% *}; [ "$(readlink /proc/$c/exe)" = "$2" ]' sh "$one" "$program"
kid=$(child_of "$one")
[ "$(readlink "/proc/$kid/exe")" = "$program" ] && echo "one child: the program" || echo "one child: $kid"
st=0
why=$(child_of "$kid") || st=$?
echo "no child: status $st: \${why//"$kid"/KID}"
ln -s "$program" "$scratch/omp-link"
echo "omp_program: $(LEGION_OMP_PATH=$scratch/omp-link omp_program unused-pin)"
`);
      expect(r.err).toBe("");
      expect(r.out).toBe(
        `one child: the program\nno child: status 1: process KID has no child\nomp_program: ${program}\n`
      );
    },
    slow
  );
  test(
    "child_running takes the child of the pid itself that runs the program; a script and a mise shim are no program",
    async () => {
      const r = run(`
bash -c '"$1" 600 & sh -c "\\"\\$0\\" 600; :" "$0" & wait' "$program" "$other" </dev/null >/dev/null 2>&1 &
top=$!
started+=("$top")
until_true 10 "the program below $top" launched "$top" "$program"
st=0
why=$(child_running "$top" "$program") || st=$?
echo "not a child of it: status $st: \${why%%;*}"
kid=$(child_running "$top" "$other")
[ "$(readlink "/proc/$kid/exe")" = "$other" ] && echo "its child: the other program $kid" || echo "its child: $kid"
printf '#!/bin/sh\\nexec sleep 600\\n' >"$scratch/wrapper" && chmod +x "$scratch/wrapper"
st=0
why=$(native_program "$scratch/wrapper") || st=$?
echo "a script: status $st"
st=0
why=$(LEGION_OMP_PATH=$scratch/wrapper omp_program unused-pin) || st=$?
echo "a script as LEGION_OMP_PATH: status $st"
cp "$program" "$scratch/mise" && ln -s "$scratch/mise" "$scratch/omp"
st=0
why=$(native_program "$scratch/omp") || st=$?
echo "a mise shim: status $st"
if native_program "$scratch/mise" >/dev/null; then echo "mise itself: named"; else echo "mise itself: refused"; fi
`);
      expect(r.err).toBe("");
      const lines = r.out.trimEnd().split("\n");
      expect(lines[0]).toMatch(
        new RegExp(
          `^not a child of it: status 1: 0 children of \\d+ run ${literalPattern(program)}$`
        )
      );
      const kid = /^its child: the other program (\d+)$/.exec(lines[1] ?? "");
      expect(kid).not.toBeNull();
      expect(lines.slice(2)).toEqual([
        "a script: status 1",
        "a script as LEGION_OMP_PATH: status 1",
        "a mise shim: status 1",
        "mise itself: named",
      ]);
      await untilGone(Number(kid?.[1]), "the other program, the child", waits.end());
    },
    slow
  );
  test(
    "first_native takes the first program itself on PATH, in a directory whose name holds a space, past a script and a mise shim",
    () => {
      const r = run(`
mkdir -p "$scratch/a" "$scratch/b" "$scratch/c with space" "$scratch/d"
printf '#!/bin/sh\\nexec sleep 600\\n' >"$scratch/a/tool" && chmod +x "$scratch/a/tool"
cp "$program" "$scratch/b/mise" && ln -s "$scratch/b/mise" "$scratch/b/tool"
cp "$program" "$scratch/c with space/tool"
cp "$other" "$scratch/d/tool"
got=$(PATH="$scratch/a:$scratch/b:$scratch/c with space:$scratch/d:$PATH" first_native tool)
[ "$got" = "$scratch/c with space/tool" ] && echo "first: the one in the spaced directory" || echo "first: $got"
st=0
why=$(PATH="$scratch/a:$scratch/b:$PATH" first_native tool) || st=$?
echo "none: status $st: \${why%%:*}"
`);
      expect(r.err).toBe("");
      expect(r.out).toBe(
        "first: the one in the spaced directory\nnone: status 1: no tool on PATH is the program itself\n"
      );
    },
    slow
  );
});

// The rig helpers' process-scan rows: signalable and stop_tree (lib/process-tree.sh) and the proc_*
// reads they use each read only the /proc entries of the processes they ask about (the failure note
// of parked-decision-devbox.sh's wait_for_run_processes is parked-decision-cleanup.test.ts's). Each
// row puts a stand-in for ps, pgrep and pkill first on PATH that logs every call and runs nothing:
// those read every process on the machine (procps-ng's `ps -p` included), so a helper that still
// calls one is named in the log, and the row fails. The traced row, where strace is on the host,
// also holds that the helpers list no directory but the task directory of a process they ask about
// and open no other process's entry.
const strace = hostGate({ programs: ["strace"] });
const logPsPgrepPkillCalls = `mkdir -p "$scratch/bin"
for n in ps pgrep pkill; do printf '#!/bin/sh\\necho "$0 $*" >>%s/calls\\nexit 1\\n' "$scratch" >"$scratch/bin/$n"; chmod +x "$scratch/bin/$n"; done
PATH="$scratch/bin:$PATH"`;
// The tree leads a process group of its own (monitor mode for its start alone), as start_process
// starts a service: stop_tree stops it with SIGSTOP first, and a stopped member of the shell's own
// group has the kernel hang up the shell itself once that group counts as orphaned, which in a
// gVisor pod it does, so the row's shell was killed there at the stop and printed nothing more.
const startParentWithTwoSleeps = `set -m
(sleep 300 & sleep 301 & wait) </dev/null >/dev/null 2>&1 &
g=$!
set +m
started+=("$g")
until_true 10 "two children of $g" sh -c '[ "$(cat /proc/$1/task/*/children | wc -w)" = 2 ]' sh "$g"
kids=$(cat /proc/"$g"/task/*/children)`;

host.describe("the rig helpers' process-scan rows", () => {
  test(
    "signalable refuses this shell and its group's leader, stop_tree refuses a wrong parent and then ends a tree, and none starts ps, pgrep or pkill",
    async () => {
      const r = run(`${logPsPgrepPkillCalls}
${startParentWithTwoSleeps}
if signalable $$ 2>/dev/null; then echo "signalable took this shell"; else echo "this shell: refused"; fi
read -r stat </proc/$$/stat
read -r -a fields <<<"\${stat##*) }"
leader=\${fields[2]}
# The harness leads its own group (ownGroup), so signalable is asked from a child bash, which runs in
# the harness's group without leading it, as a driver runs in the group of the shell that started
# it, a CI step's or a pod's bash tool's. The child says so when it leads its group itself, which
# would make the question vacuous.
bash -c '. "$1"; g=$(proc_group $$); if [ "$g" = $$ ]; then echo "the child leads its own group"; elif signalable "$g" 2>/dev/null; then echo "signalable took the group leader $g"; else echo "its group'"'"'s leader: refused"; fi' - "$root/scripts/e2e/lib/process-tree.sh"
# Every signal stop_tree sends goes through kill, here a function that records it and sends it,
# so what the wrong parent's stop sent is read from the record, never from a state that a signal
# still in flight has not changed yet. The right parent's stop then must fill the record, so an
# empty one after the wrong parent's is a record that could see a signal.
kill() { printf '%s\\n' "$*" >>"$scratch/kills"; builtin kill "$@"; }
stop_tree "$g" 1
echo "a wrong parent: sent [$(paste -sd, "$scratch/kills" 2>/dev/null || :)]"
# The shell's own "Killed" notice of the tree it reaps goes nowhere: it is no output of the row.
exec 3>&2 2>/dev/null
stop_tree "$g"
wait "$g" || :
exec 2>&3 3>&-
unset -f kill
echo "stopped: $g" $kids
if [ -s "$scratch/kills" ]; then echo "the record: holds the right parent's stop"; else echo "the record: empty after the right parent's stop"; fi
[ "$(proc_parent $$) $(proc_group $$)" = "$PPID $leader" ] && echo "proc_parent and proc_group: this shell's" || echo "proc_parent and proc_group: $(proc_parent $$) $(proc_group $$)"
echo "a pid that runs nothing: [$(proc_parent 999999999)$(proc_group 999999999)$(proc_children 999999999)$(proc_describe 999999999)]"
calls=$(cat "$scratch/calls" 2>/dev/null || :)
echo "calls: [\${calls//$'\\n'/;}]"
`);
      expect(r.err).toBe("");
      const lines = r.out.trimEnd().split("\n");
      const stopped = /^stopped: (\d+) (\d+)\s+(\d+)$/.exec(lines[3] ?? "");
      expect(stopped).not.toBeNull();
      expect([lines[0], lines[1], lines[2], ...lines.slice(4)]).toEqual([
        "this shell: refused",
        "its group's leader: refused",
        "a wrong parent: sent []",
        "the record: holds the right parent's stop",
        "proc_parent and proc_group: this shell's",
        "a pid that runs nothing: []",
        "calls: []",
      ]);
      for (const pid of stopped?.slice(1) ?? []) {
        await untilGone(Number(pid), "a process of the tree stop_tree ended", waits.end());
      }
    },
    slow
  );

  strace.test(
    "the helpers open no other process's /proc entry and list no directory but the task directories of the processes they ask about",
    async () => {
      // strace follows the traced shell and every process it starts. The traced shell records the
      // pids it asks about first (the tree's three and its own, which signalable reads), so each
      // /proc path the trace names can be held to them.
      const r = run(`${startParentWithTwoSleeps}
st=0
export ASKED="$scratch/asked"
# The shell's own "Killed" notice of the tree the traced shell stops goes to a file of its own, with
# the traced shell's stderr, which the row prints when the traced shell fails.
exec 3>&2 2>"$scratch/traced.err"
strace -f -qq -z -y -e trace=openat,getdents64 -e signal=none -o "$scratch/trace" bash -c '
  set -Eeuo pipefail
  root=$1 work=/nonexistent evidence=/nonexistent timeout_hook=
  . "$root/scripts/e2e/lib/rig.sh"
  g=$2 asker=$3
  shift 3
  echo "$g $* $$" >"$ASKED"
  proc_parent "$g" >/dev/null
  proc_group "$g" >/dev/null
  proc_children "$g" >/dev/null
  proc_describe "$g" "$@" 999999999 >/dev/null
  signalable "$g"
  stop_tree "$g" "$asker"' traced "$root" "$g" $$ $kids || st=$?
{ wait "$g"; } 2>/dev/null || :
exec 2>&3 3>&-
echo "traced: exit $st"
[ "$st" = 0 ] || cat "$scratch/traced.err"
echo "scratch: $scratch"
`);
      expect(r.err).toBe("");
      expect(r.out).toMatch(/^traced: exit 0\n/);
      const scratch = /^scratch: (.+)$/m.exec(r.out)?.[1] ?? "";
      const asked = readFileSync(join(scratch, "asked"), "utf8").trim().split(/\s+/);
      expect(asked.length).toBe(4);
      const trace = readFileSync(join(scratch, "trace"), "utf8");
      const paths = [
        ...new Set(
          [...trace.matchAll(/"(\/proc\/[^"]*)"/g)].map((m) => (m[1] ?? "").replace(/\/$/, ""))
        ),
      ];
      const others = [
        ...new Set(paths.flatMap((p) => /^\/proc\/(\d+)(\/|$)/.exec(p)?.[1] ?? [])),
      ].filter((pid) => !asked.includes(pid));
      const globals = paths.filter(
        (p) => !/^\/proc\/\d+\//.test(p) && p !== "/proc/stat" && p !== "/proc/self/maps"
      );
      const listed = [
        ...new Set([...trace.matchAll(/getdents64\(\d+<([^>]*)>/g)].map((m) => m[1] ?? "")),
      ].filter((p) => !asked.some((pid) => p === `/proc/${pid}/task`));
      expect({ others, globals, listed, readsStat: paths.includes("/proc/stat") }).toEqual({
        others: [],
        globals: [],
        listed: [],
        readsStat: true,
      });
      for (const pid of asked.slice(0, 3)) {
        await untilGone(Number(pid), "a process of the tree the traced shell stopped", waits.end());
      }
    },
    slow
  );
});

host.describe("incarnation_pid, env_has and freeze_if", () => {
  test(
    "incarnation_pid names a pid only with its own start ticks, and refuses a reused, an ended and a malformed incarnation",
    () => {
      const r = run(`
"$program" 600 </dev/null >/dev/null 2>&1 &
pid=$!
started+=("$pid")
until_true 10 "the program to run" sh -c '[ "$(readlink /proc/$1/exe)" = "$2" ]' sh "$pid" "$program"
start=$(start_of "$pid")
[ "$(incarnation_pid "$pid:$start")" = "$pid" ] && echo "its own ticks: named" || echo "its own ticks: refused"
for inc in "$pid:$((start + 1))" "$pid" ":$start" "0$pid:$start" "$pid:x" "x:$start"; do
  if incarnation_pid "$inc" >/dev/null; then echo "$inc: named"; else echo "refused"; fi
done
true &
ended=$!
wait "$ended"
if incarnation_pid "$ended:$start" >/dev/null; then echo "ended: named"; else echo "ended: refused"; fi
`);
      expect(r.err).toBe("");
      expect(r.out).toBe(`its own ticks: named\n${"refused\n".repeat(6)}ended: refused\n`);
    },
    slow
  );

  test(
    "env_has says 0 for a name the environment holds even past the pipe buffer, 1 for one it does not, and 2 for an environment it cannot read",
    () => {
      // The name stands first in an environment of 200 KB: a `tr | grep -q` stops reading at it,
      // tr dies of SIGPIPE, and pipefail made a negated check pass while the name was there.
      const r = run(`
pad=$(printf '%0100000d' 0)
env -i LEGION_GRANT_FILE=/grant PADDING1="$pad" PADDING2="$pad" "$program" 600 </dev/null >/dev/null 2>&1 &
pid=$!
started+=("$pid")
until_true 40 "the program's environment" laid_out "$program" "$pid"
for name in LEGION_GRANT_FILE LEGION_GRANT; do st=0; env_has "$pid" "$name" || st=$?; echo "$name $st"; done
echo "value $(env_of "$pid" LEGION_GRANT_FILE)"
st=0; env_has "$(ended_pid)" HOME 2>/dev/null || st=$?; echo "unreadable $st"
`);
      expect(r.err).toBe("");
      expect(r.out).toBe("LEGION_GRANT_FILE 0\nLEGION_GRANT 1\nvalue /grant\nunreadable 2\n");
    },
    slow
  );

  test(
    "freeze_if signals a process only while it is the one that started then and its check holds, sends nothing on a mismatch, and leaves a stopped process stopped",
    async () => {
      // `kill` is shadowed by a function that records each signal freeze_if sends and then sends it,
      // so what a call sent is read from its record, whether or not the target's state shows it: a
      // SIGSTOP and SIGCONT pair leaves a running process as it was. The row's own signals go
      // through `builtin kill`. `once` holds on its first call only, so a call can pass the read
      // before the stop and fail the one after it.
      const r = run(`
"$program" 600 </dev/null >/dev/null 2>&1 &
pid=$!
started+=("$pid")
until_true 10 "the program to run" sh -c '[ "$(readlink /proc/$1/exe)" = "$2" ]' sh "$pid" "$program"
start=$(start_of "$pid")
state() { local s; read -r s <"/proc/$1/stat"; s=\${s##*) }; case \${s%% *} in T) echo stopped ;; *) echo running ;; esac; }
kills=$scratch/kills
: >"$kills"
kill() { [ "$1" = -0 ] || printf '%s\\n' "$1" >>"$kills"; builtin kill "$@"; }
sent() { if [ -s "$kills" ]; then paste -sd, "$kills"; else echo nothing; fi; : >"$kills"; }
once() { [ ! -e "$scratch/once" ] && touch "$scratch/once"; }
try() { local label=$1 st=0; shift; freeze_if "$@" || st=$?; rm -f "$scratch/once"; echo "$label: $st, sent $(sent), $(state "$pid")"; }
try "another start" "$pid" "$((start + 1))" KILL
try "its check fails" "$pid" "$start" KILL false
try "its check fails once stopped" "$pid" "$start" KILL once
builtin kill -STOP "$pid"
until_true 10 "the program to stop" sh -c 'read -r s <"/proc/$1/stat"; s=\${s##*) }; [ "\${s%% *}" = T ]' sh "$pid"
try "stopped already, another start" "$pid" "$((start + 1))" KILL
try "stopped already, its check fails" "$pid" "$start" KILL false
try "stopped already, its check fails once stopped" "$pid" "$start" KILL once
builtin kill -CONT "$pid"
st=0; freeze_if "$pid" "$start" TERM true || st=$?; echo "its check holds: $st, sent $(sent)"
echo "pid $pid"
`);
      expect(r.err).toBe("");
      const pid = Number(/^pid (\d+)$/m.exec(r.out)?.[1]);
      expect(r.out).toBe(
        "another start: 1, sent nothing, running\n" +
          "its check fails: 1, sent nothing, running\n" +
          "its check fails once stopped: 1, sent -STOP,-CONT, running\n" +
          "stopped already, another start: 1, sent nothing, stopped\n" +
          "stopped already, its check fails: 1, sent nothing, stopped\n" +
          "stopped already, its check fails once stopped: 1, sent -STOP, stopped\n" +
          `its check holds: 0, sent -STOP,-TERM,-CONT\npid ${pid}\n`
      );
      await untilGone(pid, "the program freeze_if sent TERM", waits.end());
    },
    slow
  );
});

// stage3-4b13b-acceptance.sh sources ACCEPT_ROOT's lib/rig.sh, which can be another checkout's. Its
// require_process_tree fails before the run starts anything, naming each lib/process-tree.sh
// function that lib lacks and ACCEPT_ROOT, where a lib from before that file passed every other
// guard and died later on "command not found". An older ACCEPT_ROOT is a shell that sourced no
// process-tree.sh; this checkout's lib is the run() harness, which sources it through rig.sh.
const requireProcessTree = scriptFunctions(
  join(import.meta.dir, "..", "stage3-4b13b-acceptance.sh")
)("require_process_tree");
host.describe("require_process_tree", () => {
  test("names each missing function and ACCEPT_ROOT for a lib from before lib/process-tree.sh, and passes this checkout's", () => {
    const older = Bun.spawnSync(
      [
        "bash",
        "-c",
        `set -Eeuo pipefail\nroot=/older/checkout\nfail() { echo "FAIL: $*"; exit 1; }\n${requireProcessTree}\nrequire_process_tree\necho "went on"`,
      ],
      { env: { PATH: process.env.PATH ?? "" } }
    );
    expect({
      code: older.exitCode,
      out: older.stdout.toString(),
      err: older.stderr.toString(),
    }).toEqual({
      code: 1,
      out: "FAIL: ACCEPT_ROOT's scripts/e2e/lib/rig.sh (/older/checkout) predates lib/process-tree.sh and has no omp_program launched incarnation_pid env_of env_has stop_marked_processes: point ACCEPT_ROOT at a checkout that has it\n",
      err: "",
    });
    const current = run(`${requireProcessTree}\nrequire_process_tree\necho "went on"`);
    expect(current).toEqual({ code: 0, out: "went on\n", err: "" });
  });
});

// stage3-4b13b-acceptance.sh's merger-pane-mints-no-grant reads the merger pane's environment for
// LEGION_GRANT_FILE and LEGION_GRANT through env_has, and holds an absence only on its status 1.
const carriesNoGrant = scriptFunctions(join(import.meta.dir, "..", "stage3-4b13b-acceptance.sh"))(
  "carries_no_grant"
);
host.describe("carries_no_grant", () => {
  test(
    "fails while a grant stands first in a 200 KB environment and when the environment cannot be read, and holds only for an environment without one",
    () => {
      const r = run(`${carriesNoGrant}
pad=$(printf '%0100000d' 0)
env -i LEGION_GRANT_FILE=/grant PADDING1="$pad" PADDING2="$pad" "$program" 600 </dev/null >/dev/null 2>&1 &
granted=$!
started+=("$granted")
env -i PADDING1="$pad" PADDING2="$pad" "$program" 600 </dev/null >/dev/null 2>&1 &
clean=$!
started+=("$clean")
until_true 40 "both programs' environments" laid_out "$program" "$granted" "$clean"
for p in "$granted" "$clean" "$(ended_pid)"; do
  st=0
  why=$(carries_no_grant "$p") || st=$?
  echo "status $st: $why"
done
`);
      expect(r.err).toBe("");
      expect(r.out).toBe(
        "status 1: still carries LEGION_GRANT_FILE\nstatus 0: \nstatus 1: has an environment that could not be read (env_has status 2)\n"
      );
    },
    slow
  );
});

// stage3-4b13b-acceptance.sh's and stage3-devbox-workflow.sh's endpoint_mismatch read an Oh My Pi's
// environment for the rig's endpoints (env_of) and then for the ANTHROPIC_API_KEY it must not carry
// (env_has): the key's absence holds only on env_has's status 1, and an environment either read
// could not read is a mismatch, never a clean one.
host.describe("endpoint_mismatch", () => {
  test.each(["stage3-4b13b-acceptance.sh", "stage3-devbox-workflow.sh"])(
    "%s's holds only for the rig's endpoints and no key, and fails closed on an environment either read could not read",
    (script) => {
      const endpointMismatch = scriptFunctions(join(import.meta.dir, "..", script))(
        "endpoint_mismatch"
      );
      // The third program ends between the two reads: env_has, shadowed, kills and reaps it before
      // its own read, after env_of has read every endpoint, so env_has finds no environment. check
      // runs endpoint_mismatch in this shell, not a substitution, so the wait reaps the harness's
      // own child and the pid is gone, not a zombie, when env_has reads it.
      const r = run(`${endpointMismatch}
port_dispatch=1 port_listener=2 port_nats=3 state=/rig
rig=(DISPATCH_URL=http://127.0.0.1:1 DISPATCH_TOKEN_FILE=/rig/secrets/dispatch-token ENVOY_URL=http://127.0.0.1:2 ENVOY_NATS_URL=nats://127.0.0.1:3)
env -i "\${rig[@]}" "$program" 600 </dev/null >/dev/null 2>&1 &
clean=$!
env -i "\${rig[@]}" ANTHROPIC_API_KEY=k "$program" 600 </dev/null >/dev/null 2>&1 &
keyed=$!
env -i "\${rig[@]}" "$program" 600 </dev/null >/dev/null 2>&1 &
ending=$!
started+=("$clean" "$keyed" "$ending")
until_true 40 "the three programs' environments" laid_out "$program" "$clean" "$keyed" "$ending"
check() { local st=0; endpoint_mismatch "$2" >"$scratch/found" || st=$?; echo "$1: status $st: $(cat "$scratch/found")"; }
check clean "$clean"
check keyed "$keyed"
check unreadable "$(ended_pid)"
eval "real_$(declare -f env_has)"
env_has() {
  if [ "$1" = "$ending" ]; then builtin kill -KILL "$1"; { wait "$1" || true; } 2>/dev/null; fi
  real_env_has "$@"
}
check "ended between the reads" "$ending"
`);
      expect(r.err).toBe("");
      expect(r.out).toBe(
        "clean: status 1: \n" +
          "keyed: status 0: ANTHROPIC_API_KEY set, want unset\n" +
          "unreadable: status 0: an environment that could not be read (env_of status 2)\n" +
          "ended between the reads: status 0: an environment that could not be read (env_has status 2)\n"
      );
    },
    slow
  );
});

// stage3-4b13b-acceptance.sh's and stage3-devbox-workflow.sh's pane_watcher check each pane's Oh My
// Pi once it runs and its environment is laid out (environ_laid_out): the kernel names the program
// an exec starts before it lays out that program's environment, and a read in between finds none.
// An Oh My Pi found there is read again at the next poll and never counted; a mismatch the watcher
// reads is a violation. The stand-in for one inside that window is an Oh My Pi started with an empty
// environment, which reads as one inside the window reads, and stays so: the row waits until it runs
// the program, and counts the watcher's environ_laid_out calls on it (a wrapper around the lib's),
// so a watcher that skipped it, or read it once and never again, does not pass. A pane is a bash
// whose child runs the Oh My Pi, a copy of sleep; `legion claims list` is a stub that prints the
// claims a row writes. The first watcher's evidence is the harness's own, so a violation it records
// aborts the row's next wait (until_true), as it aborts the stage scripts' waits.
// jq: the watcher reads the claims `legion claims list` prints with it.
const paneWatcher = hostGate({ programs: ["bash", "sleep", "sh", "perl", "jq"] });
paneWatcher.describe("pane_watcher", () => {
  test.each(["stage3-4b13b-acceptance.sh", "stage3-devbox-workflow.sh"])(
    "%s's reads an Oh My Pi whose environment is not laid out again at the next poll, and records a mismatch it reads",
    (script) => {
      const watcher = ["omp_descendant", "endpoint_mismatch", "pane_watcher"]
        .map(scriptFunctions(join(import.meta.dir, "..", script)))
        .join("\n");
      const r = run(`${watcher}
port_dispatch=1 port_listener=2 port_nats=3 state=/rig ptoken=row omp_exe=$program
rig=(DISPATCH_URL=http://127.0.0.1:1 DISPATCH_TOKEN_FILE=/rig/secrets/dispatch-token ENVOY_URL=http://127.0.0.1:2 ENVOY_NATS_URL=nats://127.0.0.1:3)
# pane VAR=VALUE… starts a pane whose Oh My Pi runs with exactly those variables, and sets pane_pid
# to the pane's pid and inc to its incarnation.
pane() {
  bash -c 'env -i "$@" 600 & wait' pane "$@" "$program" </dev/null >/dev/null 2>&1 &
  started+=("$!")
  pane_pid=$!
  inc=$!:$(start_of "$!")
}
# claims DIR ROLE INC… gives DIR's stub one claim on I-1 for each ROLE and incarnation.
claims() {
  local dir=$1 sep=
  shift
  printf '#!/bin/sh\\ncat "%s/claims.json"\\n' "$dir" >"$dir/legion"
  chmod +x "$dir/legion"
  { printf '{"claims":['; while [ "$#" -gt 0 ]; do printf '%s{"issue":"I-1","role":"%s","locator":{"incarnation":"%s"}}' "$sep" "$1" "$2"; sep=,; shift 2; done; printf ']}\\n'; } >"$dir/claims.json"
}
pane
empty=$inc
until_true 15 "the empty pane's Oh My Pi to run" launched "$pane_pid" "$program"
empty_omp=$(launched "$pane_pid" "$program")
eval "real_$(declare -f environ_laid_out)"
environ_laid_out() { [ "$1" != "$empty_omp" ] || echo read >>"$scratch/empty-reads"; real_environ_laid_out "$@"; }
pane "\${rig[@]}"
clean=$inc
claims "$work" empty "$empty" clean "$clean"
pane_watcher &
started+=("$!")
until_true 15 "the clean pane checked and the empty one read at two polls" sh -c 'grep -qF "$1 " "$2" && [ -s "$3" ] && [ "$(wc -l <"$3")" -ge 2 ]' sh "$clean" "$work/pane-endpoints-checked.txt" "$scratch/empty-reads"
echo "checked: $(cut -d' ' -f3 "$work/pane-endpoints-checked.txt" | paste -sd, -)"
pane "\${rig[@]}" ANTHROPIC_API_KEY=k
keyed=$inc
mkdir "$scratch/keyed"
claims "$scratch/keyed" keyed "$keyed"
(work=$scratch/keyed evidence=$scratch/keyed; pane_watcher) &
started+=("$!")
until_true 15 "the keyed pane's violation" test -s "$scratch/keyed/pane-endpoint-violation.txt"
sed -e "s/$keyed/INC/" -e 's/OMP pid [0-9]*/OMP pid PID/' "$scratch/keyed/pane-endpoint-violation.txt"
`);
      expect(r.err).toBe("");
      expect(r.out).toBe(
        "checked: clean\n" +
          "keyed pane on I-1 (incarnation INC, OMP pid PID) has ANTHROPIC_API_KEY set, want unset\n"
      );
    },
    slow
  );
});

// clean-session-stop-tmux.sh's kill_the_agent SIGKILLs the agent under a claim's pane, the pane
// read from the claim's locator incarnation `<pid>:<start ticks>`. A pane is a copy of bash running
// a `worker-shim` script, as `legion worker-shim` runs its agent, whose agent is the run's Oh My Pi,
// a copy of sleep; claim_field, the daemon's answer, is a stub that applies the caller's own jq
// filter to a locator holding the incarnation a row gives it.
const cleanStop = scriptFunctions(join(import.meta.dir, "..", "clean-session-stop-tmux.sh"));
const killTheAgent = [
  "claim_pane_pid",
  "kill_the_agent",
  "agent_descendant",
  "parent_pid",
  "shimmed",
]
  .map(cleanStop)
  .join("\n");
host.describe("kill_the_agent", () => {
  test(
    "kills the agent below the pane its incarnation names, and nothing when the incarnation's start ticks are not the pane's own",
    async () => {
      const r = run(`${killTheAgent}
printf '"$1" 600\\n:\\n' >"$scratch/worker-shim"
(cd "$scratch" && exec bash worker-shim "$program") </dev/null >/dev/null 2>&1 &
pane=$!
started+=("$pane")
until_true 10 "the agent below the pane" launched "$pane" "$program"
agent=$(launched "$pane" "$program")
omp_bin=$program
start=$(start_of "$pane")
# The daemon's answer for the claim, read through the caller's own filter, as claim_json's is.
claim_field() { jq -rn --arg inc "$INCARNATION" '{locator: {incarnation: $inc}}'" | $2"; }
st=0; INCARNATION="$pane:$((start + 1))" kill_the_agent token omp >/dev/null 2>&1 || st=$?
read -r s <"/proc/$agent/stat"; s=\${s##*) }
echo "another pane's ticks: status $st, agent \${s%% *}"
INCARNATION="$pane:$start" kill_the_agent token omp 2>/dev/null | grep -qE '^[0-9]+$' && echo "its own ticks: killed"
echo "agent $agent"
`);
      expect(r.err).toBe("");
      const agent = Number(/^agent (\d+)$/m.exec(r.out)?.[1]);
      expect(r.out).toBe(
        `another pane's ticks: status 1, agent S\nits own ticks: killed\nagent ${agent}\n`
      );
      await untilGone(agent, "the agent kill_the_agent killed", waits.end());
    },
    slow
  );
});

// ps: the row of the block below that answers through ps where there is no /proc.
const ps = hostGate({ programs: ["ps"] });

// proc_start gives a zombie no start, since a zombie runs nothing any more and a signal held to its
// start would reach no process; and the environment readers (env_of, env_has, env_length,
// names_dir) answer 2 for an environment that is empty, a process that has gone or one with no
// variable at all, never 1, the answer of an environment that lacks the name.
host.describe("proc_start and the environment readers", () => {
  // The zombie is a child of a bash that execs sleep, which never reaps it. The child ends only once
  // its parent runs sleep: one that ended before the exec was reaped by the parent's bash and never
  // became a zombie, which happened once in 60 runs and every time with a second's sleep before the
  // exec. It gives up at the end of the row's waits, and the wait for the zombie then names it.
  test(
    "proc_start gives a zombie no start and a live process its own",
    () => {
      const r = run(`
printf '%s\\n' 'end=$((SECONDS + $2))' 'until [ "$(readlink "/proc/$PPID/exe")" = "$1" ]; do [ "$SECONDS" -lt "$end" ] || exit 1; sleep 0.05; done' >"$scratch/child"
bash -c 'sleep 600 & bash "$1" "$2" "$3" </dev/null >/dev/null 2>&1 & echo $!; exec sleep 600' parent "$scratch/child" "$(realpath -e -- "$(command -v sleep)")" "$((until_true_deadline - SECONDS))" </dev/null >"$scratch/zombie" 2>/dev/null &
parent=$!
started+=("$parent")
until_true 10 "the child to be a zombie" sh -c 'read -r z <"$1" && read -r s </proc/$z/stat && s=\${s##*) } && [ "\${s%% *}" = Z ]' sh "$scratch/zombie"
read -r zombie <"$scratch/zombie"
echo "zombie: [$(proc_start "$zombie")]"
echo "live: [$(proc_start "$parent")] [$(start_of "$parent")]"
`);
      expect(r.err).toBe("");
      const live = /^live: \[(\d+)\] \[(\d+)\]$/m.exec(r.out);
      expect(r.out).toStartWith("zombie: []\n");
      expect(live?.[1]).toBe(live?.[2]);
    },
    slow
  );
  // A host with no /proc (macOS) answers proc_parent, proc_group and proc_start through ps, which
  // exits 1 for a pid that names no process. Each must still print nothing and succeed there, as it
  // does on /proc: a caller under `set -e` assigns the answer bare (replace-blocks.sh reads the group
  // of an instance that may have exited already), and a failure ends that caller with no word of
  // why. The row makes the helper's own test of /proc/self read false, and the host's ps answers
  // through a wrapper that counts its calls, so the row shows the fallback was the path taken.
  ps.test(
    "proc_parent, proc_group and proc_start answer through ps where there is no /proc: nothing, and success, for a process that is gone, and a live one's own",
    () => {
      const r = run(`mkdir -p "$scratch/bin"
printf '#!/bin/sh\\necho "$*" >>%s/calls\\nexec %s "$@"\\n' "$scratch" "$(command -v ps)" >"$scratch/bin/ps"
chmod +x "$scratch/bin/ps"
PATH="$scratch/bin:$PATH"
[() {
  case "$*" in
    "-d /proc/self ]") return 1 ;;
    "! -d /proc/self ]") return 0 ;;
  esac
  builtin [ "$@"
}
gone=$(ended_pid)
g=$(proc_group "$gone")
p=$(proc_parent "$gone")
s=$(proc_start "$gone")
echo "gone: [$g$p$s]"
read -r stat </proc/$$/stat
read -r -a fields <<<"\${stat##*) }"
echo "live: $(proc_parent $$) $(proc_group $$) / \${fields[1]} \${fields[2]}"
echo "ps calls: $(wc -l <"$scratch/calls")"
`);
      expect(r.err).toBe("");
      expect(r.out).toStartWith("gone: []\n");
      const live = /^live: (\d+) (\d+) \/ (\d+) (\d+)$/m.exec(r.out);
      expect([live?.[1], live?.[2]]).toEqual([live?.[3], live?.[4]]);
      // proc_group, proc_parent and proc_start's two reads of the gone pid, then the two live reads.
      expect(r.out).toEndWith("ps calls: 6\n");
    },
    slow
  );
  test(
    "each environment reader answers 2 for an empty environment, and 1 or the value for one it reads",
    () => {
      const r = run(`
env -i "$program" 600 </dev/null >/dev/null 2>&1 &
empty=$!
env -i NAME=value "$program" 600 </dev/null >/dev/null 2>&1 &
full=$!
started+=("$empty" "$full")
until_true 20 "the full one's environment" laid_out "$program" "$full"
# The empty one has no variable to wait for. Its command line reads once its exec has laid out its
# arguments and set where its environment starts and ends (fs/proc/base.c reads neither before),
# which for an empty environment is all of it.
args_laid_out() { local -a words; [ "$(readlink "/proc/$1/exe" 2>/dev/null || :)" = "$program" ] && process_args "$1" words; }
until_true 20 "the empty one's arguments" args_laid_out "$empty"
for pid in "$empty" "$full"; do
  st=0; v=$(env_of "$pid" NAME) || st=$?; printf 'env_of %s [%s]; ' "$st" "$v"
  st=0; env_has "$pid" OTHER || st=$?; printf 'env_has %s; ' "$st"
  st=0; v=$(env_length "$pid" NAME) || st=$?; printf 'env_length %s [%s]; ' "$st" "$v"
  st=0; names_dir /elsewhere NAME "$pid" || st=$?; printf 'names_dir %s\\n' "$st"
done
`);
      expect(r.err).toBe("");
      expect(r.out).toBe(
        "env_of 2 []; env_has 2; env_length 2 []; names_dir 2\n" +
          "env_of 0 [value]; env_has 1; env_length 0 [5]; names_dir 1\n"
      );
    },
    slow
  );
});

// A row that fails in until_true (lib/rig.sh) exits inside it, through the harness's `fail`, so the
// harness's EXIT trap runs where until_true's locals are in scope. One of them was named `started`,
// and the trap then read that number in place of the row's `started` pids and stopped none of them.
// The subshell clears the row's until_true_deadline, so its until_true fails at its own 1 s, not at
// the end of the row's waits.
host.describe("the row harness", () => {
  test(
    "an EXIT trap that runs when until_true fails reads the row's own started pids",
    () => {
      const r = run(`
"$program" 600 </dev/null >/dev/null 2>&1 &
started+=("$!")
(trap 'echo "the trap reads [\${started[*]}]"' EXIT; until_true_deadline=; until_true 1 "nothing" false) 2>/dev/null || :
echo "the row started [\${started[*]}]"
`);
      expect(r.err).toBe("");
      const lines = r.out.trim().split("\n");
      expect(lines).toHaveLength(2);
      expect(lines[0].replace("the trap reads", "")).toBe(lines[1].replace("the row started", ""));
    },
    slow
  );
});
