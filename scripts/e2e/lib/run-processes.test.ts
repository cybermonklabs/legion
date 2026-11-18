import { afterEach, beforeAll, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { argMaxForceHolds } from "./arg-max-force";
import { ownGroup } from "./own-group";

// run_processes (lib/rig.sh) names the processes of a run still alive, and stop_marked_processes
// (lib/process-tree.sh) ends them: each that carries the run's mark, LEGION_E2E_RUN=$work, which the
// run exports, or a daemon pane's XDG_CONFIG_HOME under $work (run_marks), and no process that only
// works under $work or names it on its command line, as a person's shell or `tail -f` watching the
// run does. Both read every process of the machine, since a process that left every tree the run
// recorded is one of them, with one grep per batch of entries and no command for each.
const rig = join(import.meta.dir, "rig.sh");
const force = join(import.meta.dir, "arg-max-force.sh");
const root = join(import.meta.dir, "..", "..", "..");
const dir = temporaryDirectory("run-processes-test");
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

// Every process a row starts, killed when the row ends whatever it ended on. Each, and each harness
// shell, runs in a process group of its own (ownGroup): the sweeps stop what they end, and a stopped
// member of the test runner's own group hangs the runner up where that group is orphaned.
const started: number[] = [];
afterEach(() => {
  for (const pid of started.splice(0)) {
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
      .replace(/^.*\) /, "")
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
// carries no mark. PATH, when given, comes first.
function harness(work: string, body: string, path = "") {
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
      env: { PATH: `${path}${path ? ":" : ""}${process.env.PATH ?? ""}`, WORK: work },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  return { code: ran.exitCode, out: ran.stdout.toString(), err: ran.stderr.toString() };
}
const pids = (out: string) => out.split("\n").filter(Boolean);

test("names each process that carries the run's mark and none that only works under the run's directory or names it, running no command for each /proc entry", () => {
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
  writeFileSync(calls, "");
  // The drivers call it under `set -E` with an ERR trap that prints, and in the bare word split a
  // stage's checks use, where a status of its own would reach that trap.
  const r = harness(work, `for p in $(run_processes); do echo "$p"; done`, bin);
  expect(r.err).toBe("");
  expect(r.code).toBe(0);
  const named = pids(r.out);
  expect(named).toContain(String(marked));
  expect(named).toContain(String(pane));
  expect({
    shell,
    tail,
    other,
    named: named.filter((p) => [shell, tail, other].map(String).includes(p)),
  }).toEqual({
    shell,
    tail,
    other,
    named: [],
  });
  const log = readFileSync(calls, "utf8").split("\n").filter(Boolean);
  // One grep for each batch xargs makes of the machine's entries, a handful, never one an entry.
  expect(log.filter((line) => line.startsWith("readlink"))).toEqual([]);
  expect(log.length).toBeLessThan(20);
});

test("names a marked process of the run that left every tree the run started, double-forked to pid 1 or a subreaper", () => {
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
    .replace(/^.*\) /, "")
    .split(" ")[1];
  expect(parent).not.toBe(String(process.pid));
  const r = harness(work, "run_processes");
  expect(r.err).toBe("");
  expect(r.code).toBe(0);
  expect(pids(r.out)).toEqual([String(pid)]);
});

// The force cannot hold under gVisor (a Legion pod's kernel), which starts a program past ARG_MAX:
// there these two rows are skipped, by this name (argMaxForceHolds), rather than failed or passed.
test.skipIf(!argMaxForceHolds())(
  "names every marked process of the run where argv and the environment past ARG_MAX would stop a program handed every /proc entry",
  () => {
    // The force (lib/arg-max-force.sh): an exec handed every /proc/<pid> entry as an argument fails
    // with E2BIG, as `find /proc/[0-9]*/cwd` did unforced at about 80,000 processes, while an exec of a
    // few arguments still starts. The walk must still name the run's process, or fail naming why; an
    // empty answer is the defect.
    const work = join(dir, "e2big");
    mkdirSync(work);
    const inside = spawn(["sleep", "60"], { cwd: work, env: { LEGION_E2E_RUN: work } });
    const r = harness(
      work,
      `arg_max_force environ 6144 "$work" ${JSON.stringify(String.raw`find /proc/[0-9]*/cwd -maxdepth 0 -printf '%h %l\0'`)}
run_processes`
    );
    expect(r.err).toBe("");
    expect(r.code).toBe(0);
    expect(pids(r.out)).toContain(String(inside));
  }
);

test.skipIf(!argMaxForceHolds())(
  "listening_holders names the holder of a listening socket where a program handed every /proc/<pid>/fd would not start",
  () => {
    // listening_holders (lib/rig.sh) walks every process for the one holding a socket, held_only_by's
    // reading of a port. Under the same force its walk must still name this test process, which
    // holds the listener, or fail naming why; no holder at all is the defect.
    const work = join(dir, "holders");
    mkdirSync(work);
    const listener = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
    try {
      const r = harness(
        work,
        `arg_max_force fd 1024 "$work" ${JSON.stringify(String.raw`find /proc/[0-9]*/fd -maxdepth 1 -lname 'socket:\[0\]'`)}
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

test("a walk whose reader a signal ended after part of its output is no answer: listening_holders and run_processes exit 2", () => {
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
      /^listening_holders: find exited 137 reading \/proc: /m
    );
    expect(readFileSync(join(work, "rp.err"), "utf8")).toMatch(
      /^procs_naming: xargs exited 125 reading /m
    );
  } finally {
    listener.stop(true);
  }
});

test("a run that has not marked itself has no processes to read: run_processes and stop_marked_processes exit 2", () => {
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
});

test("stop_marked_processes ends every marked process of the run, an escaped one too, and leaves a person's shell and tail -f under the run's directory running", async () => {
  const work = join(dir, "sweep");
  mkdirSync(work);
  writeFileSync(join(work, "transcript.log"), "");
  const marked = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
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
});

test("stop_marked_processes with a grace gives each marked process SIGTERM and the grace to exit, then SIGKILLs only the survivors", async () => {
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
});

test("stop_marked_processes signals no pid whose start time is not the one its walk read, and leaves it running", async () => {
  // The walk's pairs stand in for a pid that ended and passed to another marked process between the
  // walk and the signal: the start time is not that process's own, so freeze_if sends it nothing.
  const work = join(dir, "reused");
  mkdirSync(work);
  const pid = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
  const r = harness(
    work,
    `marked_processes() { printf '%s %s\\n' ${pid} 1; }
stop_marked_processes "$work"; echo "status $?"`
  );
  expect(r.err).toBe("");
  expect(r.out).toBe("status 0\n");
  await Bun.sleep(200);
  expect(state(pid)).toBe("S");
});

test("stop_marked_processes ends a child that cleared its environment with the marked process that started it", async () => {
  // A marked bash starts a child under `env -i`, which carries no mark and no other variable of the
  // run's: the sweep takes it with the tree of the marked process that started it (stop_tree).
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
});

test("stop_marked_processes with a grace ends a child that cleared its environment when the marked process that started it exits on its SIGTERM", async () => {
  // The marked process exits on the TERM and leaves its child behind; what it had started was read
  // while it was stopped for the TERM, so the child is still ended, by its own start time.
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
});
