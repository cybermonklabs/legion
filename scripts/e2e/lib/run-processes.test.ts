import { afterEach, beforeAll, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { span, until } from "@legion/test-fixtures/wait";
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
// python3: the newline-name row renames its process through prctl (ctypes).
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
// gone waits until PID has ended, a zombie counting as ended, through @legion/test-fixtures/wait's
// `until`, which ends at the row's deadline naming the pid.
async function gone(pid: number): Promise<boolean> {
  return await until(`pid ${pid} to end`, () => ["gone", "Z", "X"].includes(state(pid)));
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
    await span(
      200,
      "a signal the sweep sent would reach the pid through the kernel at a moment no state orders, and no later action of the row's is ordered after its delivery"
    );
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
