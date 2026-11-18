import { beforeAll, describe, expect, test } from "bun:test";
import { copyFileSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
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
// lib/rig.sh), its children's children included. start_of PID prints PID's start time, stat field 22.
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
program=$PROGRAM other=$OTHER
${body}`;
  const result = Bun.spawnSync(["bash", "-c", script], {
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
