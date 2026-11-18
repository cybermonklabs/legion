import { beforeAll, describe, expect, test } from "bun:test";
import { copyFileSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { ownGroup } from "./own-group";
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
const skip = skipWithout("bash", "sleep", "sh");
const program = join(dir, "program");
// esc is TEXT as a regular expression that matches it alone.
const esc = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const other = join(dir, "other");
beforeAll(() => {
  if (skip) return;
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
function run(body: string) {
  const script = `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(root)}
work=$(mktemp -d "$DIR/work.XXXXXX") evidence=$work timeout_hook=
note() { :; }
fail() { echo "fail: $*" >&2; exit 1; }
. ${JSON.stringify(rig)}
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
program=$PROGRAM other=$OTHER
${body}`;
  const result = Bun.spawnSync(ownGroup(["bash", "-c", script]), {
    env: { PATH: process.env.PATH ?? "", DIR: dir, PROGRAM: program, OTHER: other },
  });
  return { code: result.exitCode, out: result.stdout.toString(), err: result.stderr.toString() };
}

// gone says whether PID has ended, a zombie counting as ended, within five seconds: what a row
// started must not outlive it. A SIGKILL's delivery and the end of the process it hits are the
// kernel's, and no fake clock drives them, so this polls the process itself, as the role-kill rows do.
async function gone(pid: number): Promise<boolean> {
  for (let i = 0; i < 100; i++) {
    let state: string;
    try {
      state = readFileSync(`/proc/${pid}/stat`, "utf8")
        .replace(/^.*\) /, "")
        .slice(0, 1);
    } catch {
      return true;
    }
    if (state === "Z" || state === "X") return true;
    await Bun.sleep(50);
  }
  return false;
}

const slow = 60_000;

describe.skipIf(skip)("record_pid_sh and recorded_pid", () => {
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

describe.skipIf(skip)("launched", () => {
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
      expect(await gone(Number(found?.[1]))).toBe(true);
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
      expect(await gone(Number(found?.[1]))).toBe(true);
      expect(await gone(Number(two?.[1]))).toBe(true);
      expect(await gone(Number(two?.[2]))).toBe(true);
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
        new RegExp(`^none: status 1: no process below \\d+ runs ${esc(program)}$`)
      );
      expect(lines[2]).toMatch(
        new RegExp(
          `^line: status 1: no process below \\d+ runs ${esc(program)}; below it: \\d+ ${esc(other)}$`
        )
      );
      expect(await gone(Number(two?.[2]))).toBe(true);
      expect(await gone(Number(two?.[3]))).toBe(true);
    },
    slow
  );
});

describe.skipIf(skip)(
  "child_of, child_running, native_program, first_native and omp_program",
  () => {
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
          new RegExp(`^not a child of it: status 1: 0 children of \\d+ run ${esc(program)}$`)
        );
        const kid = /^its child: the other program (\d+)$/.exec(lines[1] ?? "");
        expect(kid).not.toBeNull();
        expect(lines.slice(2)).toEqual([
          "a script: status 1",
          "a script as LEGION_OMP_PATH: status 1",
          "a mise shim: status 1",
          "mise itself: named",
        ]);
        expect(await gone(Number(kid?.[1]))).toBe(true);
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
  }
);

// The rig helpers' process-scan rows: signalable and stop_tree (lib/rig.sh), the proc_* reads they
// use (lib/process-tree.sh) and the failure note of parked-decision-devbox.sh's
// wait_for_run_processes each read only the /proc entries of the processes they ask about. Each row
// puts a stand-in for ps, pgrep and pkill first on PATH that logs every call and runs nothing: those
// read every process on the machine (procps-ng's `ps -p` included), so a helper that still calls one
// is named in the log, and the row fails. The traced row, where strace is on the host, also holds
// that the helpers list no directory but the task directory of a process they ask about and open no
// other process's entry.
const waitForRunProcesses = scriptFunctions(
  join(import.meta.dir, "..", "parked-decision-devbox.sh")
)("wait_for_run_processes");
const noStrace = skipWithout("strace");
// standIns is the shell that puts the logging ps, pgrep and pkill first on PATH, logging to
// $scratch/calls.
const standIns = `mkdir -p "$scratch/bin"
for n in ps pgrep pkill; do printf '#!/bin/sh\\necho "$0 $*" >>%s/calls\\nexit 1\\n' "$scratch" >"$scratch/bin/$n"; chmod +x "$scratch/bin/$n"; done
PATH="$scratch/bin:$PATH"`;
// tree starts a parent with two children, and sets g and kids once both children run.
const tree = `(sleep 300 & sleep 301 & wait) </dev/null >/dev/null 2>&1 &
g=$!
started+=("$g")
until_true 10 "two children of $g" sh -c '[ "$(cat /proc/$1/task/*/children | wc -w)" = 2 ]' sh "$g"
kids=$(cat /proc/"$g"/task/*/children)`;

describe.skipIf(skip)("the rig helpers' process-scan rows", () => {
  test(
    "signalable refuses this shell and its group's leader, stop_tree refuses a wrong parent and then ends a tree, and none starts ps, pgrep or pkill",
    async () => {
      const r = run(`${standIns}
${tree}
if signalable $$ 2>/dev/null; then echo "signalable took this shell"; else echo "this shell: refused"; fi
read -r stat </proc/$$/stat
read -r -a fields <<<"\${stat##*) }"
leader=\${fields[2]}
if [ "$leader" = $$ ] || ! signalable "$leader" 2>/dev/null; then echo "its group's leader: refused"; else echo "signalable took the group leader $leader"; fi
stop_tree "$g" 1
sleep 1
read -r stat </proc/"$g"/stat
case \${stat##*) } in S* | R*) echo "a wrong parent: left running" ;; *) echo "a wrong parent: acted on, state \${stat##*) }" ;; esac
# The shell's own "Killed" notice of the tree it reaps goes nowhere: it is no output of the row.
exec 3>&2 2>/dev/null
stop_tree "$g"
wait "$g" || :
exec 2>&3 3>&-
echo "stopped: $g" $kids
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
        "a wrong parent: left running",
        "proc_parent and proc_group: this shell's",
        "a pid that runs nothing: []",
        "calls: []",
      ]);
      for (const pid of stopped?.slice(1) ?? []) expect(await gone(Number(pid))).toBe(true);
    },
    slow
  );

  test(
    "wait_for_run_processes's failure names each survivor that runs with its command line, and none that has ended, starting no ps, pgrep or pkill",
    () => {
      const r = run(`${standIns}
true &
ended=$!
wait "$ended"
${waitForRunProcesses}
run_processes() { printf '%s\\n' "$$" "$PPID" "$ended"; }
since() { printf '0s'; }
(
  note() { printf '   %s\\n' "$*"; }
  fail() { printf 'FAIL: %s\\n' "$*"; exit 1; }
  wait_for_run_processes 0 3
) >"$scratch/note" 2>&1 || true
printf 'shell=%s test=%s ended=%s\\n' "$$" "$PPID" "$ended"
cat "$scratch/note"
calls=$(cat "$scratch/calls" 2>/dev/null || :)
echo "calls: [\${calls//$'\\n'/;}]"
`);
      expect(r.err).toBe("");
      const ids = /^shell=(\d+) test=(\d+) ended=(\d+)$/m.exec(r.out);
      expect(ids).not.toBeNull();
      const [, shell, testPid, ended] = ids ?? [];
      expect(r.out).toContain(
        `FAIL: timed out after 0s (3 polls) waiting for every process of the run to end; still running: ${shell} ${testPid} ${ended}\n`
      );
      expect(r.out).toMatch(
        new RegExp(
          `^ +${shell} ppid ${testPid} pgid \\d+ sid \\d+ started .+: bash -c set -Eeuo pipefail`,
          "m"
        )
      );
      expect(r.out).toMatch(new RegExp(`^ +${testPid} ppid \\d+ .*: \\S*bun`, "m"));
      expect(r.out).not.toMatch(new RegExp(`^ +${ended} `, "m"));
      expect(r.out).toEndWith("calls: []\n");
    },
    slow
  );

  test.skipIf(noStrace)(
    "the helpers open no other process's /proc entry and list no directory but the task directories of the processes they ask about",
    async () => {
      // strace follows the traced shell and every process it starts. The traced shell records the
      // pids it asks about first (the tree's three and its own, which signalable reads), so each
      // /proc path the trace names can be held to them.
      const r = run(`${tree}
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
      for (const pid of asked.slice(0, 3)) expect(await gone(Number(pid))).toBe(true);
    },
    slow
  );
});

describe.skipIf(skip)("incarnation_pid, env_has and freeze_if", () => {
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
until_true 10 "the program's environment" sh -c '[ "$(readlink /proc/$1/exe)" = "$2" ]' sh "$pid" "$program"
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
      expect(await gone(pid)).toBe(true);
    },
    slow
  );
});

// stage3-4b13b-acceptance.sh's merger-pane-mints-no-grant reads the merger pane's environment for
// LEGION_GRANT_FILE and LEGION_GRANT through env_has, and holds an absence only on its status 1.
const carriesNoGrant = scriptFunctions(join(import.meta.dir, "..", "stage3-4b13b-acceptance.sh"))(
  "carries_no_grant"
);
describe.skipIf(skip)("carries_no_grant", () => {
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
until_true 10 "both programs to run" sh -c '[ "$(readlink /proc/$1/exe)" = "$3" ] && [ "$(readlink /proc/$2/exe)" = "$3" ]' sh "$granted" "$clean" "$program"
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
describe.skipIf(skip)("endpoint_mismatch", () => {
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
until_true 10 "the three programs to run" sh -c 'p=$1; shift; for pid; do [ "$(readlink /proc/$pid/exe)" = "$p" ] || exit 1; done' sh "$program" "$clean" "$keyed" "$ending"
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
describe.skipIf(skip)("kill_the_agent", () => {
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
      expect(await gone(agent)).toBe(true);
    },
    slow
  );
});
