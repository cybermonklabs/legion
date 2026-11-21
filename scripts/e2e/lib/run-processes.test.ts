import { afterEach, beforeAll, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { hostTool, skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { argMaxForceHolds } from "./arg-max-force";
import { ownGroup, untilRunning } from "./own-group";
import { procRootOf } from "./proc-root";

// run_processes (lib/rig.sh) names the processes of a run still alive, and stop_marked_processes
// (lib/process-tree.sh) ends them: each that carries the run's mark, LEGION_E2E_RUN=$work, which the
// run exports, or a daemon pane's XDG_CONFIG_HOME under $work (run_marks), and no process that only
// works under $work or names it on its command line, as a person's shell or `tail -f` watching the
// run does. Both read every process of `${proc_root:-/proc}`, since a process that left every tree
// the run recorded is one of them, with one grep per batch of entries and no command for each; each
// row points proc_root at a proc root of its own processes (harness).
const rig = join(import.meta.dir, "rig.sh");
const force = join(import.meta.dir, "arg-max-force.sh");
const root = join(import.meta.dir, "..", "..", "..");
const dir = temporaryDirectory("run-processes-test");
// perl: every process a row starts, and every harness, runs through ownGroup (lib/own-group.ts).
const skip = skipWithout("perl");
// python3: the newline-name row renames its process through prctl (ctypes), and the not-dumpable
// rows make processes not dumpable through it (unreadablePy).
const withoutPython = skipWithout("python3");
const bin = join(dir, "bin");
const calls = join(dir, "calls");
beforeAll(() => {
  mkdirSync(bin);
  // A readlink that records each call and answers nothing, and a grep that records each call and
  // then runs the real one: a walk that ran one of them for each /proc entry says so in the log.
  writeFileSync(
    join(bin, "readlink"),
    `#!/bin/sh\nprintf 'readlink %s\\n' "$*" >>"${calls}"\nexit 1\n`,
    {
      mode: 0o755,
    }
  );
  writeFileSync(
    join(bin, "grep"),
    `#!/bin/sh\nprintf 'grep\\n' >>"${calls}"\nexec /usr/bin/grep "$@"\n`,
    { mode: 0o755 }
  );
  writeFileSync(unreadablePy, unreadableScript);
});

// Every process a row starts, with its start time read when it is recorded, killed when the row ends
// whatever it ended on, and only while its pid is still the process that started then: a sweep has
// reaped most of them by then, and a pid the kernel gave to another process since is left alone.
// Each, and each harness shell, runs in a process group of its own (ownGroup): the sweeps stop what
// they end, and a stopped member of the test runner's own group hangs the runner up where that group
// is orphaned. A sweep row waits until what it spawned runs its program (untilRunning), and so leads
// its own group, before it sweeps.
// startOf is PID's start time, stat field 22, read past the last `) ` of the whole file, or
// undefined once PID is gone.
function startOf(pid: number): string | undefined {
  try {
    return readFileSync(`/proc/${pid}/stat`, "utf8")
      .replace(/^[\s\S]*\) /, "")
      .split(" ")[19];
  } catch {
    return undefined;
  }
}
const started = {
  held: [] as { pid: number; start: string }[],
  push(pid: number) {
    const start = startOf(pid);
    if (start !== undefined) this.held.push({ pid, start });
  },
};
afterEach(() => {
  for (const { pid, start } of started.held.splice(0)) {
    if (startOf(pid) !== start) continue;
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
});
function spawn(argv: string[], options: { cwd: string; env?: Record<string, string> }) {
  const child = Bun.spawn(ownGroup(argv), {
    cwd: options.cwd,
    env: { PATH: process.env.PATH ?? "", ...(options.env ?? {}) },
    stdout: "ignore",
    stderr: "ignore",
  });
  started.push(child.pid);
  return child.pid;
}
// state is PID's state letter (stat field 3), or "gone".
function state(pid: number): string {
  try {
    return readFileSync(`/proc/${pid}/stat`, "utf8")
      .replace(/^[\s\S]*\) /, "")
      .slice(0, 1);
  } catch {
    return "gone";
  }
}
// gone says whether PID has ended, a zombie counting as ended, within five seconds.
async function gone(pid: number): Promise<boolean> {
  for (let i = 0; i < 100; i++) {
    if (["gone", "Z", "X"].includes(state(pid))) return true;
    await Bun.sleep(50);
  }
  return false;
}

// harness runs BODY in a bash with lib/rig.sh and lib/arg-max-force.sh sourced for the run
// directory WORK, marked as a run marks itself (LEGION_E2E_RUN), under the drivers' `set -Eeuo
// pipefail` and an ERR trap that prints, and answers its exit code and output. WORK comes in through
// the environment, and the mark is exported only once the shell runs, so the harness's own process
// carries no mark. PATH, when given, comes first. The walks read `proc_root`, a proc root of the
// row's own processes (lib/proc-root.ts): every process the row recorded so far and this test
// process, so a row's time is its own and not the host's process count. ROOT, when given, is that
// root instead.
function harness(work: string, body: string, path = "", procRoot = rowRoot()) {
  const ran = Bun.spawnSync(
    ownGroup([
      "bash",
      "-c",
      `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(root)}
work=$WORK
. ${JSON.stringify(rig)}
. ${JSON.stringify(force)}
export LEGION_E2E_RUN=$work
${body}`,
    ]),
    {
      env: {
        PATH: `${path}${path ? ":" : ""}${process.env.PATH ?? ""}`,
        WORK: work,
        proc_root: procRoot,
      },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  return { code: ran.exitCode, out: ran.stdout.toString(), err: ran.stderr.toString() };
}
// rowRoot is a fresh proc root holding every process the row recorded and this test process.
function rowRoot(): string {
  return procRootOf(dir, [...started.held.map(({ pid }) => pid), process.pid]);
}
const pids = (out: string) => out.split("\n").filter(Boolean);

test.skipIf(skip)(
  "names each process that carries the run's mark and none that only works under the run's directory or names it, running no command for each /proc entry",
  () => {
    const work = join(dir, "work");
    mkdirSync(join(work, "state", "home", ".config"), { recursive: true });
    writeFileSync(join(work, "transcript.log"), "");
    const marked = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    // A daemon pane: its environment is the daemon's allow-list, with the XDG directories moved under
    // the daemon's state directory, which the run puts under its own.
    const pane = spawn(["sleep", "60"], {
      cwd: dir,
      env: { XDG_CONFIG_HOME: join(work, "state", "home", ".config") },
    });
    // A person's shell working in the run's directory, and a `tail -f` of its transcript: neither
    // carries the mark, and neither is the run's.
    const shell = spawn(["sleep", "60"], { cwd: work });
    const tail = spawn(["tail", "-f", join(work, "transcript.log")], { cwd: dir });
    const other = spawn(["sleep", "60"], { cwd: dir });
    // A marked process outside the row's proc root: a walk that read the host's /proc and not the
    // root it was given names it, so the row fails rather than only slowing.
    const root = rowRoot();
    const outside = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    writeFileSync(calls, "");
    // The drivers call it under `set -E` with an ERR trap that prints, and in the bare word split a
    // stage's checks use, where a status of its own would reach that trap.
    const r = harness(work, `for p in $(run_processes); do echo "$p"; done`, bin, root);
    expect(r.err).toBe("");
    expect(r.code).toBe(0);
    const named = pids(r.out);
    expect(named).toContain(String(marked));
    expect(named).toContain(String(pane));
    expect({
      shell,
      tail,
      other,
      outside,
      named: named.filter((p) => [shell, tail, other, outside].map(String).includes(p)),
    }).toEqual({
      shell,
      tail,
      other,
      outside,
      named: [],
    });
    const log = readFileSync(calls, "utf8").split("\n").filter(Boolean);
    // One grep for each batch xargs makes of the machine's entries, a handful, never one an entry.
    expect(log.filter((line) => line.startsWith("readlink"))).toEqual([]);
    expect(log.length).toBeLessThan(20);
  }
);

test.skipIf(skip)(
  "names a marked process of the run that left every tree the run started, double-forked to pid 1 or a subreaper",
  () => {
    // run_processes is the run's search for its leftovers: a daemon that forked twice and whose
    // parent ended belongs to no tree the run recorded, and only a walk of every process finds it.
    const work = join(dir, "escaped");
    mkdirSync(work);
    const pidFile = join(dir, "escaped.pid");
    const forked = Bun.spawnSync(
      ownGroup([
        "bash",
        "-c",
        `( (exec sleep 60 </dev/null >/dev/null 2>&1) & echo $! >"$1" ) </dev/null >/dev/null 2>&1`,
        "fork",
        pidFile,
      ]),
      {
        env: { PATH: process.env.PATH ?? "", LEGION_E2E_RUN: work },
        stdout: "ignore",
        stderr: "pipe",
      }
    );
    expect(forked.exitCode).toBe(0);
    const pid = Number(readFileSync(pidFile, "utf8").trim());
    started.push(pid);
    // Its parent, the subshell, has ended, so nothing the test started is its parent any more.
    const parent = readFileSync(`/proc/${pid}/stat`, "utf8")
      .replace(/^[\s\S]*\) /, "")
      .split(" ")[1];
    expect(parent).not.toBe(String(process.pid));
    const r = harness(work, "run_processes");
    expect(r.err).toBe("");
    expect(r.code).toBe(0);
    expect(pids(r.out)).toEqual([String(pid)]);
  }
);

// The force cannot hold under gVisor (a Legion pod's kernel), which starts a program past ARG_MAX:
// there these two rows are skipped, by this name (argMaxForceHolds), rather than failed or passed.
test.skipIf(skip || !argMaxForceHolds())(
  "names every marked process of the run where argv and the environment past ARG_MAX would stop a program handed every /proc entry",
  () => {
    // The force (lib/arg-max-force.sh): an exec handed every <pid> entry of the row's proc root as an
    // argument fails with E2BIG, as `find /proc/[0-9]*/cwd` did unforced at about 80,000 processes,
    // while an exec of a few arguments still starts; the root's own synthetic entries, not the host's
    // processes, take it past the limit. The walk must still name the run's process, or fail naming
    // why; an empty answer is the defect.
    const work = join(dir, "e2big");
    mkdirSync(work);
    const inside = spawn(["sleep", "60"], { cwd: work, env: { LEGION_E2E_RUN: work } });
    const r = harness(
      work,
      `arg_max_force environ 6144 "$work" ${JSON.stringify(String.raw`find "$proc_root"/[0-9]*/cwd -maxdepth 0 -printf '%h %l\0'`)}
run_processes`
    );
    expect(r.err).toBe("");
    expect(r.code).toBe(0);
    expect(pids(r.out)).toContain(String(inside));
  }
);

test.skipIf(skip || !argMaxForceHolds())(
  "listening_holders names the holder of a listening socket where a program handed every /proc/<pid>/fd would not start",
  () => {
    // listening_holders (lib/rig.sh) walks every process for the one holding a socket, held_only_by's
    // reading of a port. Under the same force, over the row's proc root and its synthetic entries,
    // its walk must still name this test process, which holds the listener, or fail naming why; no
    // holder at all is the defect.
    const work = join(dir, "holders");
    mkdirSync(work);
    const listener = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
    try {
      const r = harness(
        work,
        `arg_max_force fd 1024 "$work" ${JSON.stringify(String.raw`find "$proc_root"/[0-9]*/fd -maxdepth 1 -lname 'socket:\[0\]'`)}
listening_holders ${listener.port}`
      );
      expect(r.err).toBe("");
      expect(r.code).toBe(0);
      expect(pids(r.out)).toContain(String(process.pid));
    } finally {
      listener.stop(true);
    }
  }
);

test.skipIf(skip)(
  "a walk whose reader a signal ended after part of its output is no answer: listening_holders and run_processes exit 2",
  () => {
    // A find that writes 2000 bytes and is then SIGKILLed prints no complaint of its own; neither does
    // a grep that a signal ends. Read as a clean walk, either names what it printed before and nothing
    // after, and a teardown's wait then reads the run as ended.
    const work = join(dir, "killed");
    mkdirSync(work);
    const killed = join(dir, "killed-bin");
    mkdirSync(killed);
    writeFileSync(
      join(killed, "find"),
      `#!/bin/bash\n/usr/bin/find "$@" | head -c 2000\nkill -KILL $$\n`,
      {
        mode: 0o755,
      }
    );
    writeFileSync(
      join(killed, "grep"),
      `#!/bin/bash\n/usr/bin/grep "$@" | head -c 0\nkill -KILL $$\n`,
      {
        mode: 0o755,
      }
    );
    spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    const listener = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
    try {
      const r = harness(
        work,
        `st=0; listening_holders ${listener.port} >/dev/null 2>"$work/lh.err" || st=$?; echo "listening_holders $st"
st=0; run_processes >/dev/null 2>"$work/rp.err" || st=$?; echo "run_processes $st"`,
        killed
      );
      expect(r.err).toBe("");
      expect(r.out).toBe("listening_holders 2\nrun_processes 2\n");
      expect(readFileSync(join(work, "lh.err"), "utf8")).toMatch(
        /^listening_holders: find exited 137 reading \/\S+: /m
      );
      expect(readFileSync(join(work, "rp.err"), "utf8")).toMatch(
        /^procs_naming: xargs exited 125 reading /m
      );
    } finally {
      listener.stop(true);
    }
  }
);

test.skipIf(skip)(
  "a walk whose grep could not read an environment is no answer: run_processes and stop_marked_processes exit 2 and leave the marked process running, and a grep that met only unreadable or ended entries still answers",
  async () => {
    // xargs exits 123 when a batch's grep exits 1 (no match in it, as most batches of a busy machine
    // have) and when one exits 2 (an error). Read as one, a grep that could not read the marked
    // process's environment named nothing, and the sweep returned 0 and ended nothing while that
    // process ran on. One stub fails as GNU grep does, saying why on stderr and exiting 2, and one
    // exits 2 and says nothing, alone and beside batches that each report an ended entry (an xargs
    // of one entry per batch), whose lines are no word of the silent one's. The control stub runs
    // the real grep and then reports, with exit 2, what a walk of a shared machine meets and is no
    // error: another user's environment, and one whose process ended before the open (ENOENT) or
    // during it (ESRCH).
    const work = join(dir, "grep-failed");
    mkdirSync(work);
    const grepStub = (name: string, body: string) => {
      const stubBin = join(dir, `grep-${name}`);
      mkdirSync(stubBin);
      writeFileSync(join(stubBin, "grep"), `#!/bin/bash\n${body}\n`, { mode: 0o755 });
      return stubBin;
    };
    const marked = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    await untilRunning(marked, "sleep");
    const sweep = (path: string) => {
      const r = harness(
        work,
        `st=0; run_processes >"$work/rp.out" 2>"$work/rp.err" || st=$?; echo "run_processes $st"
st=0; stop_marked_processes "$work" 2>"$work/stop.err" || st=$?; echo "stop_marked_processes $st"`,
        path
      );
      return {
        ...r,
        named: pids(readFileSync(join(work, "rp.out"), "utf8")),
        said: readFileSync(join(work, "rp.err"), "utf8"),
      };
    };
    const reported = sweep(
      grepStub("reported", `for f; do :; done\necho "grep: $f: Input/output error" >&2\nexit 2`)
    );
    expect(reported.err).toBe("");
    expect(reported.out).toBe("run_processes 2\nstop_marked_processes 2\n");
    expect(reported.said).toMatch(
      /^procs_naming: xargs exited 123 reading \S+: grep: \S+\/environ: Input\/output error, so /m
    );
    const silent = sweep(grepStub("silent", "exit 2"));
    expect(silent.err).toBe("");
    expect(silent.out).toBe("run_processes 2\nstop_marked_processes 2\n");
    expect(silent.said).toMatch(
      /^procs_naming: xargs exited 123 reading \S+: grep exited 2 and printed nothing, so /m
    );
    const mixed = grepStub(
      "mixed",
      `for f; do :; done
[ "$f" != "$proc_root/${marked}/environ" ] || exit 2
/usr/bin/grep "$@"
echo "grep: $proc_root/2/environ: No such file or directory" >&2
exit 2`
    );
    writeFileSync(join(mixed, "xargs"), `#!/bin/bash\nexec /usr/bin/xargs -n 1 "$@"\n`, {
      mode: 0o755,
    });
    const split = sweep(mixed);
    expect(split.err).toBe("");
    expect(split.out).toBe("run_processes 2\nstop_marked_processes 2\n");
    expect(split.said).toMatch(
      /^procs_naming: xargs exited 123 reading \S+: grep exited 2 and printed nothing, so /m
    );
    expect(state(marked)).toBe("S");
    const unreadable = sweep(
      grepStub(
        "unreadable",
        `/usr/bin/grep "$@"
echo "grep: $proc_root/1/environ: Permission denied" >&2
echo "/usr/bin/grep: $proc_root/2/environ: No such file or directory" >&2
echo "grep: $proc_root/3/environ: No such process" >&2
exit 2`
      )
    );
    expect(unreadable.err).toBe("");
    expect(unreadable.said).toBe("");
    expect(unreadable.out).toBe("run_processes 0\nstop_marked_processes 0\n");
    expect(unreadable.named).toContain(String(marked));
    expect(await gone(marked)).toBe(true);
  }
);

// unreadablePy READY MODE [GO] is a python3 whose /proc/<pid>/environ is root's, so this user's
// test of it fails: it makes itself not dumpable (prctl PR_SET_DUMPABLE 0, as ssh-agent and
// gpg-agent do at their start), checks that PR_GET_DUMPABLE reads 0, writes its pid to READY
// through a rename, and sleeps. MODE `orphan` first forks and leaves its session (setsid), and the
// parent exits once READY is there, as ssh-agent backgrounds: the kernel gives the process to a
// reaper above the shell that ran it. MODE `wait` first waits for the file GO, then forks the
// process that does so, and itself sleeps on. MODE `zombie` forks a child that exits at once and is
// never waited for, and writes the child's pid: a zombie's environ is root's as well.
const unreadablePy = join(dir, "unreadable.py");
const unreadableScript = `import ctypes, os, sys, time
ready, mode = sys.argv[1], sys.argv[2]
def wait_for(path):
    for _ in range(400):
        if os.path.exists(path):
            return
        time.sleep(0.05)
    sys.exit("no " + path)
def say(pid):
    with open(ready + ".tmp", "w") as f:
        f.write("%d\\n" % pid)
    os.rename(ready + ".tmp", ready)
if mode == "zombie":
    child = os.fork()
    if child == 0:
        os._exit(0)
    say(child)
    time.sleep(60)
    sys.exit(0)
if mode == "wait":
    wait_for(sys.argv[3])
    if os.fork() != 0:
        time.sleep(60)
        sys.exit(0)
elif mode == "orphan":
    if os.fork() != 0:
        wait_for(ready)
        os._exit(0)
    os.setsid()
libc = ctypes.CDLL(None, use_errno=True)
if libc.prctl(4, 0, 0, 0, 0) != 0 or libc.prctl(3, 0, 0, 0, 0) != 0:
    sys.exit("prctl(PR_SET_DUMPABLE, 0) did not hold")
say(os.getpid())
time.sleep(60)
`;
// unreadableSh holds the shell functions of the not-dumpable rows' harnesses: `unreadable READY
// MODE` runs unreadablePy as the run runs a process, MODE `child` and `zombie` in the background
// and `orphan` in the foreground, which returns once its parent has exited; `linked READY` waits for
// READY, sets `pid` to the pid it names and adds that process to the row's proc root unless it is
// there already; `state PID` prints PID's state letter, or `gone`; and `readable PID` whether this
// user may read its environ.
const unreadableSh = `unreadable() {
  case $2 in
    orphan) python3 -I ${JSON.stringify(unreadablePy)} "$@" </dev/null >/dev/null 2>"$1.err" ;;
    *) python3 -I ${JSON.stringify(unreadablePy)} "$@" </dev/null >/dev/null 2>"$1.err" & ;;
  esac
  linked "$1"
}
linked() {
  local i
  for i in $(seq 1 200); do [ ! -s "$1" ] || break; sleep 0.05; done
  [ -s "$1" ] || { echo "no $1: $(cat "$1.err" 2>/dev/null)" >&2; return 1; }
  read -r pid <"$1"
  [ -e "$proc_root/$pid" ] || ln -s "/proc/$pid" "$proc_root/$pid"
}
state() {
  local s=
  { IFS= read -r -d '' s <"/proc/$1/stat"; } 2>/dev/null || :
  [ -n "$s" ] || { echo gone; return 0; }
  s=\${s##*) }
  echo "\${s%% *}"
}
readable() { if { : <"/proc/$1/environ"; } 2>/dev/null; then echo readable; else echo unreadable; fi; }`;

// pastStarts waits until the kernel's boot clock, in the clock ticks of a process's start (stat
// field 22), has passed the start of each of PIDS: the walk tells a process older than the run's
// shell by its start, on a clock of 10 ms ticks, so one that started in the same tick as the
// harness's shell would read as one the run started. It waits on the platform's clock, which no
// fake timer moves, and gives up after 2 s.
async function pastStarts(pids: number[]): Promise<void> {
  const tck = Number(Bun.spawnSync(["getconf", "CLK_TCK"]).stdout.toString().trim());
  const latest = Math.max(...pids.map((pid) => Number(startOf(pid))));
  for (let i = 0; i < 200; i++) {
    const uptime = Number(readFileSync("/proc/uptime", "utf8").split(" ")[0]);
    if (Math.floor(uptime * tck) > latest) return;
    await Bun.sleep(10);
  }
  throw new Error(`the boot clock did not pass tick ${latest} within 2 s`);
}

test.skipIf(skip || withoutPython)(
  "a walk that cannot read the environment of a process of this user started during the run is no answer: a child of the run that made itself not dumpable, one that did so once it had left for a reaper as ssh-agent does, and one grep could not read after the test passed it make run_processes and stop_marked_processes exit 2 naming it, and leave it running",
  async () => {
    // The filter that keeps grep to the environments this user may read took a not-dumpable process
    // of this user for another user's, so the walk read clean and the sweep ended nothing while a
    // process the run started ran on, its mark unread: an ssh-agent the run started, or any process
    // that sets PR_SET_DUMPABLE to 0. Each harness starts its process after its own shell, as a run
    // starts its processes, and adds it to the row's proc root. The third is a sleep that grep says
    // it could not read, as for a process that made itself not dumpable between this user's test
    // and grep's open: its grep runs the real one and then says so, with exit 2.
    const work = join(dir, "not-dumpable");
    mkdirSync(work);
    const raced = join(dir, "grep-raced");
    mkdirSync(raced);
    writeFileSync(
      join(raced, "grep"),
      `#!/bin/bash
/usr/bin/grep "$@"
read -r p <"$WORK/raced.pid"
echo "grep: $proc_root/$p/environ: Permission denied" >&2
exit 2
`,
      { mode: 0o755 }
    );
    const marked = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    await untilRunning(marked, "sleep");
    const sweep = (start: string, path = "") => {
      const r = harness(
        work,
        `${unreadableSh}
${start}
echo "pid $pid"
echo "environ $(readable "$pid")"
[ "$(proc_parent "$pid")" = "$$" ] && echo "parent shell" || echo "parent reaper"
st=0; run_processes >/dev/null 2>"$work/rp.err" || st=$?; echo "run_processes $st"
st=0; stop_marked_processes "$work" 2>"$work/stop.err" || st=$?; echo "stop_marked_processes $st"
echo "after $(state "$pid")"`,
        path
      );
      const pid = /^pid (\d+)$/m.exec(r.out)?.[1] ?? "";
      if (pid !== "") started.push(Number(pid));
      return {
        err: r.err,
        out: r.out.replace(/^pid \d+\n/m, ""),
        pid,
        said: readFileSync(join(work, "rp.err"), "utf8"),
      };
    };
    const child = sweep(`unreadable "$work/child.ready" child`);
    const orphan = sweep(`unreadable "$work/orphan.ready" orphan`);
    const read = sweep(
      `sleep 60 </dev/null >/dev/null 2>&1 &
pid=$!
echo "$pid" >"$work/raced.pid"
ln -s "/proc/$pid" "$proc_root/$pid"`,
      raced
    );
    for (const [name, r, environ, parent] of [
      ["child", child, "unreadable", "shell"],
      ["orphan", orphan, "unreadable", "reaper"],
      ["raced", read, "readable", "shell"],
    ] as const) {
      expect({ name, err: r.err, out: r.out }).toEqual({
        name,
        err: "",
        out: `environ ${environ}\nparent ${parent}\nrun_processes 2\nstop_marked_processes 2\nafter S\n`,
      });
      // One line, naming this process alone: the processes earlier sweeps of the row started, before
      // this harness's shell, are in its proc root too and are no word of it.
      expect({ name, said: r.said.trimEnd() }).toEqual({
        name,
        said: expect.stringMatching(
          new RegExp(
            `^procs_naming: process ${r.pid} \\([^)\\n]+\\) is this user's, started after this shell, and its environment cannot be read, so it may carry the run's mark and the processes naming \\S+ were not checked$`
          )
        ),
      });
    }
    expect(state(marked)).toBe("S");
  }
);

test.skipIf(skip || withoutPython)(
  "a process of this user whose environment cannot be read and that cannot carry the run's mark does not fail the walk: one not dumpable from before the run, as a devbox's own ssh-agent is, one another session starts during the run below a process older than the run, and a zombie of the run's",
  async () => {
    // A devbox runs its user's ssh-agent, gpg-agent and forward, none dumpable; other sessions of
    // the same user start more during a run (a tailscaled login session's process, another agent's
    // tool); and a zombie's environ is root's too. A walk that failed on each would fail every
    // sweep there. The first started before the harness's shell; the second is the child that a
    // process older than that shell, the row's stand-in for another session, forks once the
    // harness has started; the third is the zombie child of a process the harness started. The
    // marked sleep is still named and ended.
    const work = join(dir, "cannot-carry");
    mkdirSync(work);
    const marked = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    const before = spawn(["python3", "-I", unreadablePy, join(work, "before.ready"), "child"], {
      cwd: dir,
    });
    const session = spawn(
      ["python3", "-I", unreadablePy, join(work, "session.ready"), "wait", join(work, "go")],
      { cwd: dir }
    );
    await untilRunning(marked, "sleep");
    await pastStarts([before, session]);
    const r = harness(
      work,
      `${unreadableSh}
linked "$work/before.ready"; before=$pid
: >"$work/go"
linked "$work/session.ready"; session=$pid
unreadable "$work/zombie.ready" zombie; zombie=$pid
echo "pids $session $!"
for i in $(seq 1 200); do [ "$(state "$zombie")" != Z ] || break; sleep 0.05; done
echo "before $(state "$before") $(readable "$before"), session $(state "$session") $(readable "$session"), zombie $(state "$zombie")"
st=0; run_processes >"$work/rp.out" 2>"$work/rp.err" || st=$?; echo "run_processes $st"
st=0; stop_marked_processes "$work" 2>"$work/stop.err" || st=$?; echo "stop_marked_processes $st"
echo "after: before $(state "$before"), session $(state "$session"), zombie $(state "$zombie")"`
    );
    for (const pid of /^pids (\d+) (\d+)$/m.exec(r.out)?.slice(1) ?? []) started.push(Number(pid));
    expect(r.err).toBe("");
    expect({
      out: r.out.replace(/^pids .*\n/m, ""),
      said: readFileSync(join(work, "rp.err"), "utf8"),
      stop: readFileSync(join(work, "stop.err"), "utf8"),
    }).toEqual({
      out: "before S unreadable, session S unreadable, zombie Z\nrun_processes 0\nstop_marked_processes 0\nafter: before S, session S, zombie Z\n",
      said: "",
      stop: "",
    });
    expect(pids(readFileSync(join(work, "rp.out"), "utf8"))).toEqual([String(marked)]);
    expect(await gone(marked)).toBe(true);
  }
);

test.skipIf(skip || withoutPython)(
  "a walk that could not read this shell's line of parents to its end is no answer: an unreadable orphan of the run, under a reaper above the cut, is not taken for another session's process",
  () => {
    // unread_may_carry passes a process whose first parent older than the walking shell is none of
    // that shell's own parents, so it needs every one of them. The review forced it so: the shell's
    // parent ends and is reaped right after the walk reads its pid, before the read of that
    // parent's own parent. A walk that stopped there kept only the dead parent, took the reaper
    // above, which had adopted a not-dumpable orphan of the run, for a stranger, and read 0 with the
    // orphan alive. G, the row's process, runs P0 and waits for it; P0 runs the harness shell H and
    // loops; H starts the orphan (unreadablePy's `orphan`), and its proc_parent, the first time it
    // reads P0 as H's parent, kills P0 and waits until G has reaped it.
    const work = join(dir, "cut-line");
    mkdirSync(work);
    const h = `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
trap ': >"$WORK/h.done"' EXIT
root=${JSON.stringify(root)}
work=$WORK
. ${JSON.stringify(rig)}
export LEGION_E2E_RUN=$work
${unreadableSh}
unreadable "$work/orphan.ready" orphan
orphan=$pid
for i in $(seq 1 200); do [ ! -s "$work/p0.pid" ] || break; sleep 0.05; done
read -r p0 <"$work/p0.pid"
eval "real_$(declare -f proc_parent)"
proc_parent() {
  local p i
  p=$(real_proc_parent "$1")
  if [ "$1" = "$$" ] && [ "$p" = "$p0" ] && [ ! -e "$work/p0.killed" ]; then
    : >"$work/p0.killed"
    kill -KILL "$p0"
    for i in $(seq 1 200); do [ -e "/proc/$p0" ] || break; sleep 0.05; done
  fi
  printf '%s\\n' "$p"
}
st=0; marked_processes "$work" >"$work/mp.out" 2>"$work/mp.err" || st=$?
[ "$(real_proc_parent "$orphan")" != "$$" ] && adopted=reaper || adopted=shell
echo "orphan $orphan adopted by a $adopted, $(state "$orphan"), P0 $( [ -e "/proc/$p0" ] && echo alive || echo reaped), marked_processes $st named [$(tr '\\n' ' ' <"$work/mp.out")]"`;
    const g = `bash -c 'bash -c "$H" >"$WORK/h.out" 2>"$WORK/h.err" & while :; do sleep 0.05; done' &
p0=$!
echo "$p0" >"$WORK/p0.tmp" && mv "$WORK/p0.tmp" "$WORK/p0.pid"
wait "$p0" 2>/dev/null || :
for i in $(seq 1 600); do [ ! -e "$WORK/h.done" ] || break; sleep 0.05; done
cat "$WORK/h.out"
cat "$WORK/h.err" >&2`;
    const ran = Bun.spawnSync(ownGroup(["bash", "-c", g]), {
      env: { PATH: process.env.PATH ?? "", WORK: work, H: h, proc_root: rowRoot() },
      stdout: "pipe",
      stderr: "pipe",
    });
    const out = ran.stdout.toString();
    const orphan = /^orphan (\d+) /m.exec(out)?.[1];
    if (orphan !== undefined) started.push(Number(orphan));
    expect(ran.stderr.toString()).toBe("");
    expect(out.replace(/^orphan \d+ /m, "orphan ")).toBe(
      "orphan adopted by a reaper, S, P0 reaped, marked_processes 2 named []\n"
    );
    expect(readFileSync(join(work, "mp.err"), "utf8")).toMatch(
      /^procs_naming: this shell's line of parents could not be read to its end, at the parent \d+ of \d+, which ended or whose pid another process took, so /m
    );
  }
);

// namespacesHold says whether this user may make a private user and pid namespace (util-linux
// unshare, a user namespace that maps this user to itself) and so run the namespace row; a host that
// refuses unprivileged user namespaces skips it, by this name.
const namespacesHold =
  hostTool("unshare") !== null &&
  hostTool("nsenter") !== null &&
  Bun.spawnSync(
    ["unshare", "--user", "--map-current-user", "--pid", "--fork", "--mount-proc", "true"],
    { stdout: "ignore", stderr: "ignore" }
  ).exitCode === 0;
if (!namespacesHold) {
  console.warn(
    `Skipped the pid-namespace row of ${import.meta.path}: it needs unshare and nsenter and an unprivileged user and pid namespace.`
  );
}

test.skipIf(skip || withoutPython || !namespacesHold)(
  "a walk from a shell that entered a pid namespace from outside is no answer for an unreadable orphan of the run that the namespace's init adopted",
  async () => {
    // A shell that `docker exec` or `kubectl exec` starts, or a step of a GitHub Actions
    // `container:` job, sits in a pid namespace whose init is no parent of its own: its parent reads
    // 0 there, and the run's orphans go to that init, older than the shell and none of its parents.
    // A walk that took every older parent outside the shell's line for a stranger read 0 with the
    // orphan alive. The row makes such a namespace (unshare: a user namespace mapping this user to
    // itself, a pid namespace whose init is a sh that says it runs and then becomes a sleep, a /proc
    // of its own), enters it from outside (nsenter) and runs the harness shell there; the shell
    // starts a not-dumpable orphan of the run (unreadablePy's `orphan`), which the namespace's init
    // adopts, and walks the namespace's /proc.
    const work = join(dir, "pid-namespace");
    mkdirSync(work);
    const ns = Bun.spawn(
      ownGroup([
        "unshare",
        "--user",
        "--map-current-user",
        "--pid",
        "--fork",
        "--kill-child",
        "--mount-proc",
        "sh",
        "-c",
        "echo runs; exec sleep 60",
      ]),
      { stdout: "pipe", stderr: "ignore" }
    );
    started.push(ns.pid);
    const said = await ns.stdout.getReader().read();
    expect(new TextDecoder().decode(said.value)).toBe("runs\n");
    const init = Number(
      readFileSync(`/proc/${ns.pid}/task/${ns.pid}/children`, "utf8").trim().split(" ")[0]
    );
    await pastStarts([init]);
    const h = `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(root)}
work=$WORK
. ${JSON.stringify(rig)}
export LEGION_E2E_RUN=$work
${unreadableSh}
python3 -I ${JSON.stringify(unreadablePy)} "$work/orphan.ready" orphan </dev/null >/dev/null 2>"$work/orphan.err"
read -r orphan <"$work/orphan.ready"
st=0; marked_processes "$work" >"$work/mp.out" 2>"$work/mp.err" || st=$?
echo "shell's parent $(proc_parent $$), orphan's parent $(proc_parent "$orphan"), orphan $(state "$orphan") $(readable "$orphan"), marked_processes $st named [$(tr '\\n' ' ' <"$work/mp.out")]"`;
    const ran = Bun.spawnSync(
      [
        "nsenter",
        "--preserve-credentials",
        "--user",
        "--pid",
        "--mount",
        "--target",
        String(init),
        "bash",
        "-c",
        h,
      ],
      { env: { PATH: process.env.PATH ?? "", WORK: work }, stdout: "pipe", stderr: "pipe" }
    );
    expect(ran.stderr.toString()).toBe("");
    expect(ran.stdout.toString()).toBe(
      "shell's parent 0, orphan's parent 1, orphan S unreadable, marked_processes 2 named []\n"
    );
    expect(readFileSync(join(work, "mp.err"), "utf8")).toMatch(
      /^procs_naming: process \d+ \([^)]+\) is this user's, started after this shell, and its environment cannot be read, so it may carry the run's mark and the processes naming \S+ were not checked$/m
    );
  }
);

test.skipIf(skip)(
  "a run that has not marked itself has no processes to read: run_processes and stop_marked_processes exit 2",
  () => {
    const work = join(dir, "unmarked");
    mkdirSync(work);
    const r = harness(
      work,
      `export LEGION_E2E_RUN=/elsewhere
st=0; run_processes >/dev/null 2>"$work/rp.err" || st=$?; echo "run_processes $st"
st=0; stop_marked_processes "$work" 2>"$work/stop.err" || st=$?; echo "stop_marked_processes $st"`
    );
    expect(r.err).toBe("");
    expect(r.out).toBe("run_processes 2\nstop_marked_processes 2\n");
    expect(readFileSync(join(work, "rp.err"), "utf8")).toMatch(/has not marked its processes/);
  }
);

test.skipIf(skip)(
  "an empty run directory names no process: marked_processes, procs_naming and names_dir refuse it with 2",
  () => {
    // With DIR empty and the mark unset, `LEGION_E2E_RUN=` and `XDG_CONFIG_HOME=` followed by any
    // absolute path matched, so the walk named every process of this user that carries an
    // XDG_CONFIG_HOME, and a sweep given it would have ended them all. The stand-in is one such
    // process. The row runs no sweep: against scripts that do not refuse, that would end them.
    const work = join(dir, "empty-dir");
    mkdirSync(join(work, "cfg"), { recursive: true });
    const bystander = spawn(["sleep", "60"], {
      cwd: dir,
      env: { XDG_CONFIG_HOME: join(work, "cfg") },
    });
    const r = harness(
      work,
      `unset LEGION_E2E_RUN
st=0; marked_processes "" >"$work/mp.out" 2>"$work/mp.err" || st=$?; echo "marked_processes $st, named $(wc -l <"$work/mp.out")"
st=0; procs_naming "" "\${run_marks[@]}" >"$work/pn.out" 2>"$work/pn.err" || st=$?; echo "procs_naming $st, named $(wc -l <"$work/pn.out")"
st=0; names_dir "" XDG_CONFIG_HOME ${bystander} 2>"$work/nd.err" || st=$?; echo "names_dir $st"`
    );
    expect(r.err).toBe("");
    expect(r.out).toBe("marked_processes 2, named 0\nprocs_naming 2, named 0\nnames_dir 2\n");
    expect(readFileSync(join(work, "mp.err"), "utf8")).toMatch(/no run directory/);
    expect(readFileSync(join(work, "pn.err"), "utf8")).toMatch(/no run directory/);
    expect(readFileSync(join(work, "nd.err"), "utf8")).toMatch(/no run directory/);
  }
);

test.skipIf(skip)(
  "stop_marked_processes ends every marked process of the run, an escaped one too, and leaves a person's shell and tail -f under the run's directory running",
  async () => {
    const work = join(dir, "sweep");
    mkdirSync(work);
    writeFileSync(join(work, "transcript.log"), "");
    const marked = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    await untilRunning(marked, "sleep");
    const pidFile = join(dir, "sweep-escaped.pid");
    Bun.spawnSync(
      ownGroup([
        "bash",
        "-c",
        `( (exec sleep 60 </dev/null >/dev/null 2>&1) & echo $! >"$1" ) </dev/null >/dev/null 2>&1`,
        "fork",
        pidFile,
      ]),
      { env: { PATH: process.env.PATH ?? "", LEGION_E2E_RUN: work } }
    );
    const escaped = Number(readFileSync(pidFile, "utf8").trim());
    started.push(escaped);
    const shell = spawn(["sleep", "60"], { cwd: work });
    const tail = spawn(["tail", "-f", join(work, "transcript.log")], { cwd: work });
    const r = harness(work, `stop_marked_processes "$work"; echo "status $?"`);
    expect(r.err).toBe("");
    expect(r.out).toBe("status 0\n");
    expect(await gone(marked)).toBe(true);
    expect(await gone(escaped)).toBe(true);
    // Running, and not left stopped either.
    expect({ shell: state(shell), tail: state(tail) }).toEqual({ shell: "S", tail: "S" });
  }
);

test.skipIf(skip)(
  "stop_marked_processes with a grace gives each marked process SIGTERM and the grace to exit, then SIGKILLs only the survivors",
  async () => {
    const work = join(dir, "grace");
    mkdirSync(work);
    const graceful = join(dir, "graceful-exited");
    const stubborn = join(dir, "stubborn-killed");
    // One exits on SIGTERM after a short write (a token refresh finishing), one ignores SIGTERM. Each
    // says "ready" once its trap is set, and the stop starts only then.
    const env = { PATH: process.env.PATH ?? "", LEGION_E2E_RUN: work };
    const a = Bun.spawn(
      ownGroup([
        "bash",
        "-c",
        `trap 'sleep 0.3; touch ${JSON.stringify(graceful)}; exit 0' TERM; echo ready; while :; do sleep 0.1; done`,
      ]),
      { env, stdout: "pipe" }
    );
    started.push(a.pid);
    const b = Bun.spawn(
      ownGroup([
        "bash",
        "-c",
        `trap '' TERM; echo ready; while :; do sleep 0.1; done; touch ${JSON.stringify(stubborn)}`,
      ]),
      { env, stdout: "pipe" }
    );
    started.push(b.pid);
    await a.stdout.getReader().read();
    await b.stdout.getReader().read();
    const r = harness(work, `stop_marked_processes "$work" 1; echo "status $?"`);
    expect(r.err).toBe("");
    expect(r.out).toBe("status 0\n");
    expect(await a.exited).toBe(0);
    expect(existsSync(graceful)).toBe(true);
    await b.exited;
    expect(b.signalCode).toBe("SIGKILL");
    expect(existsSync(stubborn)).toBe(false);
  }
);

test.skipIf(skip)(
  "stop_marked_processes signals no pid whose start time is not the one its walk read, and leaves it running",
  async () => {
    // The walk's pairs stand in for a pid that ended and passed to another marked process between the
    // walk and the signal: the start time is not that process's own, so freeze_if sends it nothing.
    const work = join(dir, "reused");
    mkdirSync(work);
    const pid = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    await untilRunning(pid, "sleep");
    const r = harness(
      work,
      `marked_processes() { printf '%s %s\\n' ${pid} 1; }
stop_marked_processes "$work"; echo "status $?"`
    );
    expect(r.err).toBe("");
    expect(r.out).toBe("status 0\n");
    await Bun.sleep(200);
    expect(state(pid)).toBe("S");
  }
);

test.skipIf(skip)(
  "stop_marked_processes ends a child that cleared its environment with the marked process that started it",
  async () => {
    // A marked bash starts a child under `env -i`, which carries no mark and no other variable of the
    // run's: the sweep takes it with the tree of the marked process that started it (kill_frozen).
    const work = join(dir, "cleared");
    mkdirSync(join(work, "ws"), { recursive: true });
    const childFile = join(dir, "cleared-child.pid");
    const parent = Bun.spawn(
      ownGroup([
        "bash",
        "-c",
        `env -i sleep 300 </dev/null >/dev/null 2>&1 & echo $! >"$1"; echo ready; wait`,
        "marked",
        childFile,
      ]),
      {
        cwd: join(work, "ws"),
        env: { PATH: process.env.PATH ?? "", LEGION_E2E_RUN: work },
        stdout: "pipe",
      }
    );
    started.push(parent.pid);
    await parent.stdout.getReader().read();
    const child = Number(readFileSync(childFile, "utf8").trim());
    started.push(child);
    const r = harness(work, `stop_marked_processes "$work"; echo "status $?"`);
    expect(r.err).toBe("");
    expect(r.out).toBe("status 0\n");
    expect(await gone(parent.pid)).toBe(true);
    expect(await gone(child)).toBe(true);
  }
);

test.skipIf(skip)(
  "stop_marked_processes with a grace ends a child that cleared its environment when the marked process that started it exits on its SIGTERM",
  async () => {
    // The marked process exits on the TERM and leaves its child behind; its children were read while
    // it was stopped for the TERM, so the child is still ended, by its own start time.
    const work = join(dir, "cleared-grace");
    mkdirSync(work);
    const childFile = join(dir, "cleared-grace-child.pid");
    const parent = Bun.spawn(
      ownGroup([
        "bash",
        "-c",
        `env -i sleep 300 </dev/null >/dev/null 2>&1 & echo $! >"$1"; trap 'exit 0' TERM; echo ready; while :; do sleep 0.1; done`,
        "marked",
        childFile,
      ]),
      { cwd: dir, env: { PATH: process.env.PATH ?? "", LEGION_E2E_RUN: work }, stdout: "pipe" }
    );
    started.push(parent.pid);
    await parent.stdout.getReader().read();
    const child = Number(readFileSync(childFile, "utf8").trim());
    started.push(child);
    const r = harness(work, `stop_marked_processes "$work" 1; echo "status $?"`);
    expect(r.err).toBe("");
    expect(r.out).toBe("status 0\n");
    expect(await parent.exited).toBe(0);
    expect(await gone(child)).toBe(true);
  }
);

test.skipIf(skip)(
  "stop_marked_processes ends a marked process it froze even where its parent reads otherwise the second time, as when the parent exits after the freeze",
  async () => {
    // proc_parent answers the real parent on its first read of the process and pid 1 on every read
    // after, which is what a parent that exits between two reads, leaving the process to pid 1, looks
    // like. A stop held to the parent read first then refused the frozen process and left it stopped.
    const work = join(dir, "reparented");
    mkdirSync(work);
    const pid = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    await untilRunning(pid, "sleep");
    const r = harness(
      work,
      `eval "real_$(declare -f proc_parent)"
proc_parent() {
  if [ "$1" != ${pid} ]; then real_proc_parent "$1"; elif [ -e "$work/read-once" ]; then echo 1; else : >"$work/read-once"; real_proc_parent "$1"; fi
}
stop_marked_processes "$work"; echo "status $?"`
    );
    expect(r.err).toBe("");
    expect(r.out).toBe("status 0\n");
    expect(await gone(pid)).toBe(true);
  }
);

test.skipIf(skip)(
  "stop_marked_processes with a grace leaves running a pid below a marked process whose parent is not the process it was read under, as a pid a stranger took",
  async () => {
    // proc_children answers, for the marked bash's child, the pid of a stranger: an unmarked process
    // of this user that no process of the run started, which is what a grandchild's pid reused under
    // a running parent looks like. The sweep ends the bash and its child, and not the stranger.
    const work = join(dir, "reused-below");
    mkdirSync(work);
    const stranger = spawn(["sleep", "60"], { cwd: dir });
    await untilRunning(stranger, "sleep");
    const parent = Bun.spawn(
      ownGroup(["bash", "-c", "sleep 300 </dev/null >/dev/null 2>&1 & echo $!; wait"]),
      { cwd: dir, env: { PATH: process.env.PATH ?? "", LEGION_E2E_RUN: work }, stdout: "pipe" }
    );
    started.push(parent.pid);
    const said = await parent.stdout.getReader().read();
    const child = Number(new TextDecoder().decode(said.value).trim());
    started.push(child);
    const r = harness(
      work,
      `eval "real_$(declare -f proc_children)"
proc_children() { if [ "$1" = ${child} ]; then echo ${stranger}; else real_proc_children "$1"; fi; }
stop_marked_processes "$work" 1; echo "status $?"`
    );
    expect(r.err).toBe("");
    expect(r.out).toBe("status 0\n");
    expect(await gone(parent.pid)).toBe(true);
    expect(await gone(child)).toBe(true);
    expect(state(stranger)).toBe("S");
  }
);

test.skipIf(skip)(
  "stop_marked_processes with a grace leaves running the children of a stranger that took the pid of a process below a marked one",
  async () => {
    // The marked bash's child, which carries no mark, is read and printed; then its pid passes to
    // a stranger before its own children are read: proc_children answers, for that pid, an
    // unmarked process of this user that no process of the run started, which names the pid as its
    // parent, and from then on the pid's start reads one more, the stranger's. A walk that held the
    // child only to its pid number took the stranger's child and SIGKILLed it after the grace.
    const work = join(dir, "reused-parent");
    mkdirSync(work);
    const strangers = spawn(["sleep", "60"], { cwd: dir });
    await untilRunning(strangers, "sleep");
    const marked = Bun.spawn(
      ownGroup([
        "bash",
        "-c",
        "env -u LEGION_E2E_RUN sleep 300 </dev/null >/dev/null 2>&1 & echo $!; wait",
      ]),
      { cwd: dir, env: { PATH: process.env.PATH ?? "", LEGION_E2E_RUN: work }, stdout: "pipe" }
    );
    started.push(marked.pid);
    const said = await marked.stdout.getReader().read();
    const child = Number(new TextDecoder().decode(said.value).trim());
    started.push(child);
    const r = harness(
      work,
      `eval "real_$(declare -f proc_children)"
eval "real_$(declare -f proc_start)"
eval "real_$(declare -f proc_parent)"
proc_children() { if [ "$1" = ${child} ]; then : >"$work/reused"; echo ${strangers}; else real_proc_children "$1"; fi; }
proc_start() { if [ "$1" = ${child} ] && [ -e "$work/reused" ]; then echo "$(($(real_proc_start "$1") + 1))"; else real_proc_start "$1"; fi; }
proc_parent() { if [ "$1" = ${strangers} ]; then echo ${child}; else real_proc_parent "$1"; fi; }
stop_marked_processes "$work" 1; echo "status $?"`
    );
    expect(r.err).toBe("");
    expect(r.out).toBe("status 0\n");
    expect(await gone(marked.pid)).toBe(true);
    expect(state(strangers)).toBe("S");
  }
);

test.skipIf(skip || withoutPython)(
  "names and ends a marked process whose name holds a newline and a `) `, which a read of the stat's first line took for another parent and no start",
  async () => {
    // The process renames itself to `) S 1` and a newline (prctl PR_SET_NAME, through python3's
    // ctypes: perl's $0 would write over the environment the walk reads the mark from): the first
    // line of its /proc/<pid>/stat then reads as a process whose parent is pid 1, with nothing
    // after, and a walk held to its start found none and left it out of every sweep. It says
    // "ready" once renamed, and the walk starts only then.
    const work = join(dir, "newline-name");
    mkdirSync(work);
    const renamed = Bun.spawn(
      ownGroup([
        "python3",
        "-I",
        "-c",
        'import ctypes, time; ctypes.CDLL(None).prctl(15, b") S 1\\n", 0, 0, 0); print("ready", flush=True); time.sleep(60)',
      ]),
      { cwd: dir, env: { PATH: process.env.PATH ?? "", LEGION_E2E_RUN: work }, stdout: "pipe" }
    );
    started.push(renamed.pid);
    await renamed.stdout.getReader().read();
    const pid = renamed.pid;
    const named = harness(
      work,
      `run_processes; printf 'parent %s start %s\\n' "$(proc_parent ${pid})" "$(proc_start ${pid})"`
    );
    expect(named.err).toBe("");
    expect(named.out).toBe(`${pid}\nparent ${process.pid} start ${startOf(pid)}\n`);
    const r = harness(work, `stop_marked_processes "$work"; echo "status $?"`);
    expect(r.err).toBe("");
    expect(r.out).toBe("status 0\n");
    expect(await gone(pid)).toBe(true);
  }
);
