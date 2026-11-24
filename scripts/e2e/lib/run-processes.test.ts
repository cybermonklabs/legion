import { afterEach, beforeAll, beforeEach, expect, setDefaultTimeout, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { endianness } from "node:os";
import { join } from "node:path";
import { hostGate, hostTool, requireHostTools } from "@legion/test-fixtures/host-tools";
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
const subreaperPy = join(import.meta.dir, "subreaper.py");
const root = join(import.meta.dir, "..", "..", "..");

// rowBoundMs is the bound of every row of this file (setDefaultTimeout), whatever the runner's
// command line, and a hang guard that decides no verdict: every wait a row makes on the kernel ends
// rowTeardownMs before it (rowWaitsEnd), naming what it waited for and what it saw, so the walk and
// the cleanup after a wait still fit in the row, and a wait that runs out fails by its own message
// rather than by bun's timeout. No span shorter than the row decides a wait: a forced run on a
// devbox at a load near 450 (2026-11-21) held an escaped process inside its exec for more than 3 s,
// and the fixed 3 s wait this replaced failed the row. Bun gives a hook the default bound in force
// when the hook is registered, so this comes before every hook of the file, temporaryDirectory's
// afterAll that removes `dir` among them: on that devbox's shared scratch disk that removal ran
// 14.5 s to 20.6 s in three whole-file runs, past bun's 5 s for a hook, and failed the file as
// "(unnamed) … a beforeEach/afterEach hook timed out" while every row passed.
const rowBoundMs = 60_000;
const rowTeardownMs = 15_000;
setDefaultTimeout(rowBoundMs);
let rowStart = 0;
beforeEach(() => {
  rowStart = performance.now();
});
// rowWaitsEnd is when the running row's waits end, in performance.now() time.
function rowWaitsEnd(): number {
  return rowStart + rowBoundMs - rowTeardownMs;
}
const dir = temporaryDirectory("run-processes-test");
// perl: every process a row starts, and every harness, runs through ownGroup (lib/own-group.ts).
const host = hostGate({ programs: ["perl"] });
// python3: the newline-name row renames its process through prctl (ctypes), and the not-dumpable
// rows make processes not dumpable through it (unreadablePy); each fails by name without it
// rather than skipping (requireHostTools).
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
// untilEnded resolves once PID has ended, a zombie counting as ended, reading its state every 50 ms
// as gone does, until the row's waits end (rowWaitsEnd), and reads once more then; past that it
// rejects naming the pid, WHAT it is, and the state it last read. gone's fixed 5 s is no bound on
// a loaded node, where the file's rule (rowBoundMs) has a row's own waits decide.
async function untilEnded(pid: number, what: string): Promise<void> {
  const end = rowWaitsEnd();
  let last = "";
  for (;;) {
    last = state(pid);
    if (["gone", "Z", "X"].includes(last)) return;
    const now = performance.now();
    if (now >= end) break;
    await Bun.sleep(Math.min(50, end - now));
  }
  throw new Error(
    `${what} (pid ${pid}) had not ended by the end of the row's waits (${(rowBoundMs - rowTeardownMs) / 1000} s after the row began): its state read ${last}`
  );
}
// untilExecDone resolves once PID has finished its exec of PROGRAM as far as a walk of the marks
// reads it: its exe link (/proc/<pid>/exe) names PROGRAM and its /proc/<pid>/environ reads
// non-empty. Inside an exec Linux gives the process its new memory, and the exe link with it,
// before it sets where that memory's arguments and environment end (create_elf_tables in
// fs/binfmt_elf.c): first the arguments' end, then a placeholder end of the environment, which lets
// /proc/<pid>/cmdline read in full (get_mm_cmdline in fs/proc/base.c reads once that end is not 0),
// and only after it has counted each variable the environment's real end. Until then environ reads
// empty and a walk of the marks names nothing, so a wait on the exe link or on the command line can
// end inside the exec. Nothing signals the end of another process's exec, so it reads both every
// 50 ms, as gone and untilRunning (lib/own-group.ts) do, until the row's waits end (rowWaitsEnd),
// and reads once more then; past that it rejects naming the pid, what its exe link read and how many
// bytes its environ held.
async function untilExecDone(pid: number, program: string): Promise<void> {
  const want = realpathSync(hostTool(program) ?? program);
  const end = rowWaitsEnd();
  let exe = "";
  let environ = 0;
  for (;;) {
    try {
      exe = readlinkSync(`/proc/${pid}/exe`);
      environ = exe === want ? readFileSync(`/proc/${pid}/environ`).length : 0;
    } catch {
      exe = "";
      environ = 0;
    }
    if (exe === want && environ > 0) return;
    const now = performance.now();
    if (now >= end) break;
    await Bun.sleep(Math.min(50, end - now));
  }
  throw new Error(
    `pid ${pid} did not finish its exec of ${want} by the end of the row's waits (${(rowBoundMs - rowTeardownMs) / 1000} s after the row began): its exe link read ${exe || "nothing"} and its environ ${environ} bytes`
  );
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

host.test(
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

host.test(
  "names a marked process of the run that left every tree the run started, double-forked to pid 1 or a subreaper",
  async () => {
    // run_processes is the run's search for its leftovers: a daemon that forked twice and whose
    // parent ended belongs to no tree the run recorded, and only a walk of every process finds it.
    // The walk starts once the escaped subshell's exec of sleep is done (untilExecDone): inside the
    // exec its /proc/<pid>/environ reads empty, so the walk names nothing, while its exe link already
    // reads sleep. On a loaded devbox the row read [] where it wants the pid in 1 to 2 of 10 runs.
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
    await untilExecDone(pid, "sleep");
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
// there these two rows are skipped outside CI, naming why, and failed by name under GITHUB_ACTIONS,
// whose runner holds it (hostGate).
const argMax = hostGate({
  programs: ["perl"],
  reason: argMaxForceHolds()
    ? ""
    : "This kernel starts a program past ARG_MAX under a 512 KiB stack (gVisor does), so the force lib/arg-max-force.sh puts these rows under cannot be put in force.",
});
argMax.test(
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

argMax.test(
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

// otherLoopback says whether this host lets a socket listen on 127.0.0.2, an address of the
// loopback network other than 127.0.0.1. Linux routes all of 127.0.0.0/8 to lo; a host that does
// not skips the row that needs it outside CI, naming why, and under GITHUB_ACTIONS fails it by its
// own name (hostGate's reason).
const otherLoopback = (() => {
  try {
    Bun.listen({ hostname: "127.0.0.2", port: 0, socket: { data() {} } }).stop(true);
    return true;
  } catch {
    return false;
  }
})();
const sameAddress = hostGate({
  programs: ["perl"],
  reason: otherLoopback ? "" : "This host lets no socket listen on 127.0.0.2.",
});
// A python3 that listens on 1.0.0.127 at a port the kernel chooses, prints the port, and stays: an
// address no interface holds, which IP_FREEBIND (15 on Linux) lets a socket bind.
const freeBound = `import socket, time
s = socket.socket()
s.setsockopt(socket.SOL_IP, getattr(socket, "IP_FREEBIND", 15), 1)
s.bind(("1.0.0.127", 0))
s.listen()
print(s.getsockname()[1], flush=True)
time.sleep(60)`;
// freeBind says whether this host lets that socket listen; a kernel that refuses IP_FREEBIND skips
// the row that needs it outside CI, naming why, and under GITHUB_ACTIONS fails it by its own name
// (hostGate's reason). A host without python3 is not read as such a kernel: the row needs python3
// and fails by name without it on any host (requireHostTools).
const freeBind =
  hostTool("python3") === null ||
  Bun.spawnSync(["python3", "-c", freeBound.replace("time.sleep(60)", "")], {
    stdout: "ignore",
    stderr: "ignore",
  }).exitCode === 0;
const byteOrder = hostGate({
  programs: ["perl"],
  reason: freeBind ? "" : "This host lets no python3 socket listen on 1.0.0.127 (IP_FREEBIND).",
});
// spelled is how /proc/net/tcp writes the IPv4 address ADDRESS: the hexadecimal of its 32 bits in
// this host's byte order, so 127.0.0.1 is 0100007F on a little-endian host.
function spelled(address: string): string {
  const bytes = address
    .split(".")
    .map((b) => Number(b).toString(16).toUpperCase().padStart(2, "0"));
  return (endianness() === "LE" ? bytes.reverse() : bytes).join("");
}
// onlyTheLoopbackHolder is a row's whole check that listening_holders counts only the socket on
// 127.0.0.1:PORT: PROGRAM, another process, listens on OTHER at a port the kernel chooses and
// prints it, and this test process then listens on 127.0.0.1 with that port. That order cannot
// meet the collision the row is about: the other order, 127.0.0.1 first, failed when a third
// process already held OTHER at the port the kernel then gave 127.0.0.1. No order removes the
// reverse, a port the kernel gives OTHER that some process already listens on at 127.0.0.1 (or on
// every address), since the box's processes share one network namespace: that refuses this listen,
// and the row takes a fresh port from a new process until the row's waits end (rowWaitsEnd), then
// fails naming every port 127.0.0.1 refused, a failure of its setup and not of listening_holders.
// Both LISTEN rows are read from /proc/net/tcp before the walk, which must name this test process
// alone.
async function onlyTheLoopbackHolder(name: string, program: string[], other: string) {
  const work = join(dir, name);
  mkdirSync(work);
  let listener: { port: number; stop(closeActiveConnections?: boolean): void } | undefined;
  const refused: string[] = [];
  const deadline = rowWaitsEnd();
  try {
    while (listener === undefined) {
      if (performance.now() >= deadline) {
        throw new Error(
          `no port free on both ${other} and 127.0.0.1 by the end of the row's waits (${(rowBoundMs - rowTeardownMs) / 1000} s after the row began): ${refused.length} ports tried, each refused on 127.0.0.1, so listening_holders was not run (${refused.join("; ")})`
        );
      }
      const holder = Bun.spawn(ownGroup(program), {
        cwd: dir,
        env: { PATH: process.env.PATH ?? "" },
        stdout: "pipe",
        stderr: "pipe",
      });
      started.push(holder.pid);
      const said = new TextDecoder().decode((await holder.stdout.getReader().read()).value);
      const port = Number(said.trim());
      if (!Number.isInteger(port) || port <= 0) {
        holder.kill("SIGKILL");
        throw new Error(
          `the process listening on ${other} printed ${JSON.stringify(said)}, not its port: ${await new Response(holder.stderr).text()}`
        );
      }
      try {
        listener = Bun.listen({ hostname: "127.0.0.1", port, socket: { data() {} } });
      } catch (error) {
        refused.push(`${port}: ${(error as Error).message}`);
        holder.kill("SIGKILL");
      }
    }
    const port = listener?.port ?? 0;
    const hex = port.toString(16).toUpperCase().padStart(4, "0");
    const listening = readFileSync("/proc/net/tcp", "utf8")
      .split("\n")
      .map((line) => line.trim().split(/\s+/))
      .filter((fields) => fields[3] === "0A" && fields[1]?.endsWith(`:${hex}`))
      .map((fields) => fields[1]);
    expect(listening).toEqual(
      expect.arrayContaining([`${spelled("127.0.0.1")}:${hex}`, `${spelled(other)}:${hex}`])
    );
    const r = harness(work, `listening_holders ${port}`);
    expect({ code: r.code, out: r.out, err: r.err }).toEqual({
      code: 0,
      out: `${process.pid}\n`,
      err: "",
    });
  } finally {
    listener?.stop(true);
  }
}

sameAddress.test(
  "listening_holders counts only the socket on 127.0.0.1:PORT: one another process listens with on the same port of another loopback address is none of it",
  async () => {
    // The kernel's bind of port 0 on 127.0.0.1 can hand out a port another process already listens
    // on at another loopback address. On a devbox a Go test (buckets.test) listened on
    // 127.162.56.110:41869 while the killed-reader row's own listener had 127.0.0.1:41869, and a
    // count of every LISTEN row with that port read "2 sockets listen" and failed the row. Here the
    // other address is 127.0.0.2, held by a bun of its own.
    await onlyTheLoopbackHolder(
      "same-port",
      [
        process.execPath,
        "-e",
        'console.log(Bun.listen({ hostname: "127.0.0.2", port: 0, socket: { data() {} } }).port); setTimeout(() => {}, 60000);',
      ],
      "127.0.0.2"
    );
  }
);

byteOrder.test(
  "listening_holders reads 127.0.0.1 in this host's byte order: a socket on 1.0.0.127, the other order's spelling of it, is none of it",
  async () => {
    requireHostTools("python3");
    // /proc/net/tcp writes 127.0.0.1 as 0100007F on a little-endian host, and 7F000001 there is
    // 1.0.0.127; a read that took both spellings counted a socket on 1.0.0.127:PORT beside the one
    // on 127.0.0.1:PORT and read "2 sockets listen".
    await onlyTheLoopbackHolder("byte-order", ["python3", "-c", freeBound], "1.0.0.127");
  }
);

host.test(
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

host.test(
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
  },
  // A guard on a hang only: its four sweeps walk eight times, which overran Bun's 5 s default at
  // load 1,751 (13.1 s), and every wait of its harness has a bound of its own.
  60_000
);

host.test(
  "a walk whose read of the environments' bounds failed is no answer: run_processes and stop_marked_processes exit 2, say which awk failed, and leave the marked process running",
  async () => {
    // Around grep each batch reads every entry's environment bound twice (NAMING_SNAP) and compares
    // the reads (NAMING_MOVED), and the entries whose bound read 0 or moved are read again whole: a
    // read an exec cut short. An awk that failed left that list empty, and the walk took it for
    // "none moved" and answered clean (a reviewer's forcing at 49fafd2027: status 0, named []).
    // One stub fails the two reads (its arguments are the -e PATTERN pairs and the entries), and
    // one the comparison (`-F TAB PROGRAM`); every other awk runs the real one.
    const work = join(dir, "awk-failed");
    mkdirSync(work);
    const awkStub = (name: string, test: string) => {
      const stubBin = join(dir, `awk-${name}`);
      mkdirSync(stubBin);
      writeFileSync(
        join(stubBin, "awk"),
        `#!/bin/bash\n${test} && exit 1\nexec /usr/bin/awk "$@"\n`,
        {
          mode: 0o755,
        }
      );
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
      return { ...r, said: readFileSync(join(work, "rp.err"), "utf8") };
    };
    const reads = sweep(awkStub("reads", `[ "\${2:-}" = -e ]`));
    expect(reads.err).toBe("");
    expect(reads.out).toBe("run_processes 2\nstop_marked_processes 2\n");
    expect(reads.said).toMatch(
      /^procs_naming: xargs exited 123 reading \S+: awk failed at the bounds read before grep \(awk exited 1\), the bounds read after grep \(awk exited 1\), so /m
    );
    const compared = sweep(awkStub("compared", `[ "\${1:-}" = -F ] && [[ \${3:-} == *past* ]]`));
    expect(compared.err).toBe("");
    expect(compared.out).toBe("run_processes 2\nstop_marked_processes 2\n");
    expect(compared.said).toMatch(
      /^procs_naming: xargs exited 123 reading \S+: awk failed at the bounds compared \(awk exited 1\), so /m
    );
    expect(state(marked)).toBe("S");
    process.kill(marked, "SIGKILL");
    expect(await gone(marked)).toBe(true);
  },
  // A guard on a hang only: two sweeps, each walking twice, every wait with a bound of its own.
  60_000
);

host.test(
  "a walk inside a teardown's EXIT trap answers by its own xargs's status, after a run that passed and after one that failed: a grep that failed is no answer, and a grep that met only unreadable or ended entries still answers",
  async () => {
    // The walk passed xargs's status out of its substitution with a bare `exit`, and a bare `exit` in
    // a process or command substitution that a trap runs exits with the status from before the trap
    // (bash 5.2; a `( … )` subshell keeps its own): inside a teardown's EXIT
    // trap a grep that failed read 0 after a run that passed, so the walk took the batch it lost for
    // one with no match, and a grep that met only an ended entry read 1 after a run that failed, so
    // the teardown failed (Tester4784-3: `xargs exited 1 … xargs printed nothing`, 1 of 104 real
    // teardown sweeps). The stubs are the grep-error row's: one fails as GNU grep does, and one runs
    // the real grep and then reports entries that are no error, with exit 2.
    const work = join(dir, "walk-in-trap");
    mkdirSync(work);
    const grepStub = (name: string, body: string) => {
      const stubBin = join(dir, `trap-grep-${name}`);
      mkdirSync(stubBin);
      writeFileSync(join(stubBin, "grep"), `#!/bin/bash\n${body}\n`, { mode: 0o755 });
      return stubBin;
    };
    const failed = grepStub(
      "failed",
      `for f; do :; done\necho "grep: $f: Input/output error" >&2\nexit 2`
    );
    const benign = grepStub(
      "benign",
      `/usr/bin/grep "$@"
echo "grep: $proc_root/1/environ: Permission denied" >&2
echo "grep: $proc_root/2/environ: No such file or directory" >&2
exit 2`
    );
    const marked = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    await untilRunning(marked, "sleep");
    const teardown = (path: string, runStatus: number) => {
      const r = harness(
        work,
        `walk() { st=0; run_processes >"$work/rp.out" 2>"$work/rp.err" || st=$?; echo "run_processes $st"; }
trap walk EXIT
exit ${runStatus}`,
        path
      );
      return {
        code: r.code,
        out: r.out,
        err: r.err,
        named: pids(readFileSync(join(work, "rp.out"), "utf8")),
        said: readFileSync(join(work, "rp.err"), "utf8"),
      };
    };
    for (const runStatus of [0, 1]) {
      const lost = teardown(failed, runStatus);
      expect({ runStatus, code: lost.code, out: lost.out, err: lost.err }).toEqual({
        runStatus,
        code: runStatus,
        out: "run_processes 2\n",
        err: "",
      });
      expect(lost.said).toMatch(
        /^procs_naming: xargs exited 123 reading \S+: grep: \S+\/environ: Input\/output error, so /m
      );
      const answered = teardown(benign, runStatus);
      expect({ runStatus, code: answered.code, out: answered.out, said: answered.said }).toEqual({
        runStatus,
        code: runStatus,
        out: "run_processes 0\n",
        said: "",
      });
      expect(answered.named).toContain(String(marked));
    }
  },
  // A guard on a hang only: four walks, every wait of the harness with a bound of its own.
  60_000
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

// orphanPy READY is the python3 the namespace rows start inside a pid namespace, under a sh that
// exits at once so the namespace's init adopts it: it makes itself not dumpable (prctl
// PR_SET_DUMPABLE 0), writes its pid to READY through a rename, and sleeps. The pid it writes is
// what /proc/self names, not getpid(): a process in a pid namespace below the one its /proc shows
// (one a shell entered with nsenter --pid, the /proc left the host's) has from getpid() its pid in
// its own namespace, and the walk reads it by the pid /proc numbers it with.
const orphanPy = `import ctypes, os, sys, time
ctypes.CDLL(None).prctl(4, 0, 0, 0, 0)
open(sys.argv[1] + ".tmp", "w").write(os.readlink("/proc/self") + "\\n")
os.rename(sys.argv[1] + ".tmp", sys.argv[1])
time.sleep(60)`;

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

host.test(
  "a walk that cannot read the environment of a process of this user started during the run is no answer: a child of the run that made itself not dumpable, one that did so once it had left for a reaper as ssh-agent does, and one grep could not read after the test passed it make run_processes and stop_marked_processes exit 2 naming it, and leave it running",
  async () => {
    requireHostTools("python3");
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
  },
  // A guard on a hang only: its three sweeps walk six times, which took past Bun's 5 s default on a
  // devbox at load 350, and every wait of its harness has a bound of its own.
  60_000
);

host.test(
  "a process of this user whose environment cannot be read and that cannot carry the run's mark does not fail the walk: one not dumpable from before the run, as a devbox's own ssh-agent is, one another session starts during the run below a process older than the run, and a zombie of the run's",
  async () => {
    requireHostTools("python3");
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
  },
  // A guard on a hang only, as for the row above.
  60_000
);

host.test(
  "a walk from a shell that is a child subreaper judges an unreadable process by its line of parents: one another session leaves under pid 1 during the run does not fail it, and the run's own, which the shell adopted, still does",
  async () => {
    requireHostTools("python3");
    // Another session's not-dumpable process that double-forks away during the run goes to pid 1,
    // or to a reaper above the walking shell, as the run's own does when the shell is no subreaper,
    // so unread_may_carry cannot tell the two apart and a devbox's long run failed its teardown on
    // such processes (13 of 60 walks of aec1fb77ff's tester exited 2). A shell that lib/subreaper.py
    // started keeps every process of the run below it, its own orphans included, and run_descendant
    // passes any process whose line of parents meets an older process before the shell. The harness
    // here starts under subreaper.py and waits; this test process, which is no descendant of it,
    // then starts a not-dumpable python3 that leaves its parent and is adopted above the harness,
    // and lets the harness walk only once that parent has ended, so the walk never meets the orphan
    // while its parent exits (a line of parents the walk cannot read to its end, on which it fails
    // closed with 2). The harness then starts its own such orphan, whose parent `unreadable` waits
    // out, and walks again. The first walk's stderr is part of the row's first assertion, so a
    // failure says why: one forced run of 3 at 2f41f2bcb6, in a builds pod of 64 CPUs, read "walk
    // beside the foreign orphan: 2, named []" and kept no reason.
    const work = join(dir, "subreaper");
    mkdirSync(work);
    const marked = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    await untilRunning(marked, "sleep");
    const procRoot = rowRoot();
    const body = `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(root)}
work=$WORK
. ${JSON.stringify(rig)}
export LEGION_E2E_RUN=$work
${unreadableSh}
[ "\${LEGION_E2E_SUBREAPER:-}" = "$$ $(proc_start $$)" ] && echo "the shell is the subreaper" || echo "the shell is no subreaper"
: >"$work/shell.ready"
for i in $(seq 1 400); do [ ! -e "$work/go" ] || break; sleep 0.05; done
linked "$work/foreign.ready"; foreign=$pid
echo "foreign $(readable "$foreign"), parent $([ "$(proc_parent "$foreign")" = "$$" ] && echo shell || echo elsewhere)"
st=0; run_processes >"$work/rp1.out" 2>"$work/rp1.err" || st=$?
echo "walk beside the foreign orphan: $st, named [$(tr '\\n' ' ' <"$work/rp1.out")]"
unreadable "$work/own.ready" orphan; own=$pid
echo "own $(readable "$own"), parent $([ "$(proc_parent "$own")" = "$$" ] && echo shell || echo elsewhere)"
st=0; run_processes >"$work/rp2.out" 2>"$work/rp2.err" || st=$?
echo "walk beside the run's own orphan too: $st"
echo "pids $foreign $own"`;
    const shell = Bun.spawn(ownGroup(["python3", "-I", subreaperPy, "bash", "-c", body]), {
      env: { PATH: process.env.PATH ?? "", WORK: work, proc_root: procRoot },
      stdout: "pipe",
      stderr: "pipe",
    });
    started.push(shell.pid);
    for (let i = 0; i < 200 && !existsSync(join(work, "shell.ready")); i++) await Bun.sleep(50);
    const starter = spawn(["python3", "-I", unreadablePy, join(work, "foreign.ready"), "orphan"], {
      cwd: dir,
    });
    await untilEnded(starter, "the foreign orphan's parent");
    writeFileSync(join(work, "go"), "");
    const [out, err] = await Promise.all([
      new Response(shell.stdout).text(),
      new Response(shell.stderr).text(),
    ]);
    await shell.exited;
    const ids = /^pids (\d+) (\d+)$/m.exec(out)?.slice(1) ?? [];
    for (const pid of ids) started.push(Number(pid));
    const walk1 = join(work, "rp1.err");
    expect({
      err,
      out: out.replace(/^pids .*\n/m, ""),
      walk1: existsSync(walk1) ? readFileSync(walk1, "utf8") : "(no rp1.err)",
    }).toEqual({
      err: "",
      out: `the shell is the subreaper\nforeign unreadable, parent elsewhere\nwalk beside the foreign orphan: 0, named [${marked} ]\nown unreadable, parent shell\nwalk beside the run's own orphan too: 2\n`,
      walk1: "",
    });
    expect(readFileSync(join(work, "rp2.err"), "utf8").trimEnd()).toMatch(
      new RegExp(
        `^procs_naming: process ${ids[1]} \\([^)\\n]+\\) is this user's, started after this shell, and its environment cannot be read, so it may carry the run's mark and the processes naming \\S+ were not checked$`
      )
    );
  },
  // A guard on a hang only: the row took 2.2 to 2.6 s on a loaded devbox, and its harness's waits
  // for its files have bounds of their own.
  60_000
);

host.test(
  "a walk that could not read this shell's line of parents to its end is no answer: an unreadable orphan of the run, under a reaper above the cut, is not taken for another session's process",
  () => {
    requireHostTools("python3");
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
  },
  // A guard on a hang only, as for the rows above.
  60_000
);

host.test(
  "a marked process that re-execs itself is named by every walk: a read that falls inside its exec is read again, not taken for an answer",
  async () => {
    // A read of /proc/<pid>/environ reads the memory the process had when the file was opened, and
    // inside an exec the kernel gives the process new memory before the bounds of its arguments and
    // environment: a read across an exec comes back empty, or cut short between pages, where a mark
    // further on is lost. grep found no mark in it, names_dir read it as gone or unmarked, and the
    // walk named nothing of a marked process, which a sweep then left running. The harness's shell
    // starts a process that re-execs itself without end, as a run starts its processes, each exec
    // keeping its pid and its environment, a 100,000-byte variable first and the run's mark after it
    // (env -i keeps that order), so a read of the 25 pages before the mark that an exec meets is
    // often cut; it sleeps 10 ms between its execs, so it holds no CPU and most reads fall within
    // one exec on any machine (back to back, its execs left a two-CPU runner no such read in 10 s).
    // The shell adds it to the row's proc root, walks 30 times over it and this test process, and
    // every walk must name it. A walk that read only an empty read again (aec1fb77ff) missed such a
    // process; one that took every read for an answer missed it more.
    const work = join(dir, "exec-window");
    mkdirSync(work);
    const r = harness(
      work,
      `loop='sleep 0.01; exec "$0" -c "$1" "$0" "$1"'
pad=$(head -c 100000 /dev/zero | tr '\\0' x)
env -i PATH="$PATH" PAD="$pad" LEGION_E2E_RUN="$work" sh -c "$loop" sh "$loop" </dev/null >/dev/null 2>&1 &
looping=$!
echo "looping $looping"
ln -s "/proc/$looping" "$proc_root/$looping"
comm=
for i in $(seq 1 200); do { read -r comm <"/proc/$looping/comm"; } 2>/dev/null; [ "$comm" != sh ] || break; sleep 0.05; done
named=0 failed=0
for i in $(seq 1 30); do
  st=0
  out=$(procs_naming "$work" "\${run_marks[@]}") || st=$?
  [ "$st" = 0 ] || failed=$((failed + 1))
  case " $out " in *" $looping "*) named=$((named + 1)) ;; esac
done
kill -KILL "$looping"
# Reaped here, so bash prints no job notice for it before the next command (the cut-line row's G
# does the same): one that died before the echo ran made bash say "Killed" on stderr.
wait "$looping" 2>/dev/null || :
echo "named in $named of 30 walks, $failed failed"`
    );
    const looping = /^looping (\d+)$/m.exec(r.out)?.[1];
    if (looping !== undefined) started.push(Number(looping));
    expect(r.err).toBe("");
    expect(r.out.replace(/^looping \d+\n/m, "")).toBe("named in 30 of 30 walks, 0 failed\n");
  },
  // A guard on a hang only: 20 walks of a 100,000-byte environment took 18 s at load 620, each
  // reading the looping process again every 10 ms until a read falls within one exec.
  120_000
);

host.test(
  "a process that cannot carry the run's mark holds no walk while it re-execs: past the walk's bound, one whose read grep did not match is judged as an unreadable environment is, and passed, and the run's marked process is still named",
  async () => {
    // A process of this user whose read around grep may have fallen inside an exec is read again
    // until a read falls within one exec, up to the walk's bound, and past it the walk failed with
    // 2. On a devbox at load 960 one walk of 100 of the real /proc failed so on a process the run
    // did not start, in an exec the load held: another session's, which cannot carry the run's mark.
    // So past the bound one whose read grep did not match is judged (naming_may_carry), and one that
    // cannot carry the mark is passed. Here an unmarked process that re-execs itself without end, a
    // 100,000-byte variable in its environment so that a read seldom falls within one exec, starts
    // before the harness's shell, and the walk's bound is 0 s (procs_naming_exec_seconds), so the
    // first read that does not decide reaches it; 30 walks must each name the marked sleep alone and
    // exit 0.
    const work = join(dir, "cannot-carry-exec");
    mkdirSync(work);
    const loop = 'exec "$0" -c "$1" "$0" "$1"';
    const marked = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    const looping = spawn(["sh", "-c", loop, "sh", loop], {
      cwd: dir,
      env: { PAD: "x".repeat(100_000) },
    });
    await untilRunning(marked, "sleep");
    await untilRunning(looping, "sh");
    await pastStarts([marked, looping]);
    const r = harness(
      work,
      `procs_naming_exec_seconds=0
named=0 loop=0 failed=0
for i in $(seq 1 30); do
  st=0
  out=$(procs_naming "$work" "\${run_marks[@]}" 2>"$work/pn.err") || st=$?
  [ "$st" = 0 ] || { failed=$((failed + 1)); [ -e "$work/first.err" ] || cp "$work/pn.err" "$work/first.err"; }
  case " $out " in *" ${marked} "*) named=$((named + 1)) ;; esac
  case " $out " in *" ${looping} "*) loop=$((loop + 1)) ;; esac
done
echo "named the marked sleep in $named of 30 walks, the loop in $loop, $failed failed"`
    );
    expect({
      err: r.err,
      out: r.out,
      first: existsSync(join(work, "first.err"))
        ? readFileSync(join(work, "first.err"), "utf8")
        : "",
    }).toEqual({
      err: "",
      out: "named the marked sleep in 30 of 30 walks, the loop in 0, 0 failed\n",
      first: "",
    });
  },
  // A guard on a hang only: the walks read the loop not at all, and the marked sleep once each.
  60_000
);

// execEntry ROOT PID makes PID's entry in the proc root ROOT one the walk takes for a read that an
// exec may have met: a directory in place of the link to /proc/PID, whose environ holds a
// 9,000-byte variable and no mark of the run (what a read an exec cut short leaves) and whose stat
// is PID's own with start_code, env_start and env_end (fields 26, 50 and 51) 0, so the batch's
// read of the bound around grep flags it. The rest of the walk reads /proc/PID itself.
function execEntry(root: string, pid: number): void {
  const entry = join(root, String(pid));
  rmSync(entry, { force: true, recursive: true });
  mkdirSync(entry);
  writeFileSync(join(entry, "environ"), `PAD=${"x".repeat(9000)}\0PATH=/usr/bin\0`);
  const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
  const cut = stat.lastIndexOf(") ") + 2;
  const fields = stat.slice(cut).trim().split(" ");
  for (const at of [23, 47, 48]) fields[at] = "0";
  writeFileSync(join(entry, "stat"), `${stat.slice(0, cut)}${fields.join(" ")}\n`);
}

// execScript is the shell of the two rows below, sourced after lib/rig.sh: exec_entry PID does in
// the shell what execEntry does, and exec_state and names_dir answer for the processes in the
// space-separated $targets as each row scripts them (exec_answer, names_answer), from counters in
// $work (exec_state's Nth call for PID is es.PID, names_dir's nd.PID), and pass every other process
// to the walk's own. exec_state assigns its answer to its third argument, or prints it where it is
// given none (the form of aec1fb77ff and before); those before it have no exec_state.
const execScript = `exec_entry() {
  local entry=$proc_root/$1 stat= pad
  local -a f
  rm -rf "$entry"
  mkdir "$entry"
  pad=$(head -c 9000 /dev/zero | tr '\\0' x)
  printf 'PAD=%s\\0PATH=/usr/bin\\0' "$pad" >"$entry/environ"
  { IFS= read -r -d '' stat <"/proc/$1/stat"; } 2>/dev/null || :
  read -r -a f <<<"\${stat##*) }"
  f[23]=0 f[47]=0 f[48]=0
  printf '%s) %s\\n' "\${stat%) *}" "\${f[*]}" >"$entry/stat"
}
eval "real_$(declare -f names_dir)"
! declare -F exec_state >/dev/null || eval "real_$(declare -f exec_state)"
count() { local n=0; { read -r n <"$work/$1"; } 2>/dev/null || n=0; n=$((n + 1)); echo "$n" >"$work/$1"; echo "$n"; }
exec_state() {
  case " $targets " in *" $1 "*) ;; *) real_exec_state "$@"; return ;; esac
  local s
  s=$(exec_answer "$1" "$(count "es.$1")")
  if [ -n "\${3:-}" ]; then printf -v "$3" '%s' "$s"; else printf '%s\\n' "$s"; fi
}
names_dir() {
  case " $targets " in *" \${!#} "*) ;; *) real_names_dir "$@"; return ;; esac
  return "$(names_answer "\${!#}" "$(count "nd.\${!#}")")"
}`;

// bounds is the gate on whether this kernel shows a process's memory bounds in /proc/<pid>/stat, as
// the walk's exec_state reads them: this test process runs and is in no exec, so its start_code
// (field 26) and env_start (field 50) read 0 only where the kernel shows them all as 0, as gVisor (a
// Legion pod's kernel) does. There the walk takes each read as it comes (bounds_shown,
// lib/process-tree.sh), and the scripted rows below, which answer for exec_state, prove nothing.
// Outside CI such a kernel skips them, naming why; under GITHUB_ACTIONS, whose runner's kernel
// shows the bounds, it fails them by name (hostGate).
const bounds = (() => {
  const fields = readFileSync("/proc/self/stat", "utf8")
    .replace(/^[\s\S]*\) /, "")
    .split(" ");
  const shown = fields[23] !== "0" && fields[47] !== undefined && fields[47] !== "0";
  return hostGate({
    programs: ["perl"],
    reason: shown
      ? ""
      : "This kernel shows no memory bounds in /proc/<pid>/stat (start_code, field 26, and env_start, field 50, read 0 for a running process), so the walk cannot tell a read inside an exec.",
  });
})();

bounds.test(
  "a read of an environment that an exec cut short is read again, never taken for an unmarked process: the bounds moved between the two reads around it",
  () => {
    // A read of /proc/<pid>/environ across an exec comes back cut short at a page, and a mark past
    // it is lost: aec1fb77ff read again only an empty read, so it took a cut one for an environment
    // without the mark and named nothing of a marked process, exit 0. Here the process is a sleep
    // the harness starts, as a run starts its processes; its entry in the row's proc root is a read
    // grep finds no mark in and whose bound the batch reads as 0 (execEntry), and the walk's two
    // stat reads and its read of the environment answer as an exec in the middle of the first read
    // would: the bounds 1000-2000, the read with no mark (names_dir 1), the bounds 3000-4000; then
    // 3000-4000, the read with the mark, 3000-4000. The walk must read again and name it. No clock
    // decides it: the answers come in that order however fast the machine is.
    const work = join(dir, "cut-short");
    mkdirSync(work);
    const procRoot = rowRoot();
    const r = harness(
      work,
      `${execScript}
sleep 60 </dev/null >/dev/null 2>&1 &
targets=$!
echo "target $targets"
exec_entry "$targets"
exec_answer() { case $2 in 1) echo "set 1000-2000" ;; *) echo "set 3000-4000" ;; esac; }
names_answer() { case $2 in 1) echo 1 ;; *) echo 0 ;; esac; }
st=0; out=$(procs_naming "$work" "\${run_marks[@]}" 2>"$work/pn.err") || st=$?
read -r reads <"$work/nd.$targets" || reads=0
echo "status $st, named [$(echo $out | cut -d' ' -f1)], reads of the environment $reads"
kill -KILL "$targets"`,
      "",
      procRoot
    );
    const target = /^target (\d+)$/m.exec(r.out)?.[1];
    if (target !== undefined) started.push(Number(target));
    const said = join(work, "pn.err");
    expect({ err: r.err, said: existsSync(said) ? readFileSync(said, "utf8") : "(none)" }).toEqual({
      err: "",
      said: "",
    });
    expect(r.out.replace(/^target \d+\n/m, "")).toBe(
      `status 0, named [${target}], reads of the environment 2\n`
    );
  },
  // A guard on a hang only: the walk reads the target twice, 10 ms apart.
  60_000
);

bounds.test(
  "a process held inside one exec is waited for, whatever the walk's bound: one that started outside the run's shell is named once its exec ends, never passed, and so is the run's own",
  async () => {
    // A process a loaded machine holds inside one exec (the exec-window forcing held one 10.8 s)
    // shows its bounds as 0 until the exec ends. A walk that counted those reads against its 10 s
    // bound judged it there by its line of parents and passed one that started outside the run's
    // shell, as the double-forked row's escaped process does, unnamed, and failed the run's own
    // with 2. Here exec_state answers `exec` 20 times for each of two sleeps, one started before the
    // harness's shell and one the harness starts, and then their bounds, each read with the mark;
    // the walk's bound is 0 s (procs_naming_exec_seconds), so only the wait decides. Both must be
    // named.
    const work = join(dir, "held-exec");
    mkdirSync(work);
    const outside = spawn(["sleep", "60"], { cwd: dir });
    await untilRunning(outside, "sleep");
    await pastStarts([outside]);
    const procRoot = rowRoot();
    execEntry(procRoot, outside);
    const r = harness(
      work,
      `${execScript}
procs_naming_exec_seconds=0
sleep 60 </dev/null >/dev/null 2>&1 &
own=$!
echo "own $own"
exec_entry "$own"
targets="${outside} $own"
exec_answer() { if [ "$2" -le 20 ]; then echo exec; else echo "set 1000-2000"; fi; }
names_answer() { echo 0; }
st=0; out=$(procs_naming "$work" "\${run_marks[@]}" 2>"$work/pn.err") || st=$?
named=
for p in $targets; do case " $(echo $out) " in *" $p "*) named="$named $p" ;; esac; done
echo "status $st, named [\${named# }]"
kill -KILL "$own"`,
      "",
      procRoot
    );
    const own = /^own (\d+)$/m.exec(r.out)?.[1];
    if (own !== undefined) started.push(Number(own));
    const said = join(work, "pn.err");
    expect({ err: r.err, said: existsSync(said) ? readFileSync(said, "utf8") : "(none)" }).toEqual({
      err: "",
      said: "",
    });
    expect(r.out.replace(/^own \d+\n/m, "")).toBe(`status 0, named [${outside} ${own}]\n`);
  },
  // A guard on a hang only: the walk waits 20 times 10 ms for each.
  60_000
);

bounds.test(
  "the sweep's check of a process it is about to stop reads within one exec: one an exec cut short is read again and the process ended, never left running as unmarked",
  () => {
    // stop_marked_processes stops each process the walk named only while it still carries the
    // run's mark (freeze_if's check), and the check read the environment once: a read across an
    // exec, cut short before the mark, was taken for an unmarked process, which the sweep passed
    // over, exit 0, and left running (a reviewer's forcing: 3 of 25 sweeps). Here exec_state and
    // names_dir answer for a sleep the harness starts: the walk's read decides (the bounds
    // 1000-2000 both times, the mark found), then the check's first read is one an exec met (the
    // bounds 1000-2000, no mark, the bounds 3000-4000), and every read after it holds (3000-4000,
    // the mark found). The sweep must read again and end it. The sleep is started from a subshell
    // that exits at once, so it is no job of the harness's shell, which would otherwise print a job
    // notice ("Killed") on stderr at its next command once the sweep had killed it.
    const work = join(dir, "sweep-cut-short");
    mkdirSync(work);
    const r = harness(
      work,
      `${execScript}
(sleep 60 </dev/null >/dev/null 2>&1 & echo $! >"$work/target.pid")
read -r targets <"$work/target.pid"
echo "target $targets"
ln -s "/proc/$targets" "$proc_root/$targets"
state() { local s; s=$(proc_stat_field "$1" 3 state); echo "\${s:-gone}"; }
exec_answer() { if [ "$2" -le 3 ]; then echo "set 1000-2000"; else echo "set 3000-4000"; fi; }
names_answer() { if [ "$2" = 2 ]; then echo 1; else echo 0; fi; }
st=0; stop_marked_processes "$work" 2>"$work/stop.err" || st=$?
for i in $(seq 1 200); do [ "$(state "$targets")" != gone ] || break; sleep 0.05; done
echo "stop_marked_processes $st, the sleep $(state "$targets")"`
    );
    const target = /^target (\d+)$/m.exec(r.out)?.[1];
    if (target !== undefined) started.push(Number(target));
    expect({
      err: r.err,
      out: r.out.replace(/^target \d+\n/m, ""),
      said: readFileSync(join(work, "stop.err"), "utf8"),
    }).toEqual({ err: "", out: "stop_marked_processes 0, the sleep gone\n", said: "" });
  },
  // A guard on a hang only: one walk and two checks of one process.
  60_000
);

bounds.test(
  "the sweep's check that cannot read a process within one exec fails the sweep with 2, names the process and leaves it running, with a grace and without",
  () => {
    // A check that could not read the process was refused like a check that found no mark, and the
    // sweep passed over it with 0. Here exec_state answers for a sleep the harness starts as for a
    // process held inside one exec from the check's first read on, and the hold is 0 s
    // (procs_naming_hold_seconds), so the check cannot read it: the sweep must say so and fail
    // with 2, both through its grace loop (a grace of 1 s) and through kill_held (none).
    const work = join(dir, "sweep-unread");
    mkdirSync(work);
    const r = harness(
      work,
      `${execScript}
sleep 60 </dev/null >/dev/null 2>&1 &
targets=$!
echo "target $targets"
ln -s "/proc/$targets" "$proc_root/$targets"
state() { local s; s=$(proc_stat_field "$1" 3 state); echo "\${s:-gone}"; }
exec_answer() { if [ "$2" -le 2 ]; then echo "set 1000-2000"; else echo exec; fi; }
names_answer() { echo 0; }
procs_naming_hold_seconds=0
st=0; stop_marked_processes "$work" 1 2>"$work/grace.err" || st=$?
echo "with a grace: $st, the sleep $(state "$targets")"
rm -f "$work/es.$targets" "$work/nd.$targets"
st=0; stop_marked_processes "$work" 2>"$work/none.err" || st=$?
echo "without: $st, the sleep $(state "$targets")"
kill -KILL "$targets"
wait "$targets" 2>/dev/null || :`
    );
    const target = /^target (\d+)$/m.exec(r.out)?.[1];
    if (target !== undefined) started.push(Number(target));
    expect({ err: r.err, out: r.out.replace(/^target \d+\n/m, "") }).toEqual({
      err: "",
      out: "with a grace: 2, the sleep S\nwithout: 2, the sleep S\n",
    });
    for (const [name, by] of [
      ["grace.err", "stop_marked_processes"],
      ["none.err", "kill_held"],
    ]) {
      expect({ name, said: readFileSync(join(work, name), "utf8") }).toEqual({
        name,
        said: `held_names_dir: process ${target} stayed inside one exec for 0 s, so whether it carries the run's mark was not checked\n${by}: process ${target} of the run under ${work} could not be checked, so it was left running\n`,
      });
    }
  },
  // A guard on a hang only: two sweeps of one process.
  60_000
);

// subreaperSignals is the shell the two rows below run under lib/subreaper.py: the status of a
// pipeline whose reader exits first (141 where SIGPIPE ends the writer, 1 and a "Broken pipe" where
// the writer ignores it), the status of a write past `ulimit -f` (153 where SIGXFSZ ends it, 1 and
// "File too large" where it is ignored), and the signals the shell ignores (SigIgn, a mask), which
// gVisor's /proc/<pid>/status does not show: there the mask reads empty for both shells, and the row
// holds the shell under subreaper.py to the two statuses alone.
const subreaperSignals = `seq 1 100000 | head -n 1 >/dev/null 2>&1; pipe=\${PIPESTATUS[0]}
d=$(mktemp -d); { (ulimit -f 1; head -c 8192 /dev/zero >"$d/f") 2>/dev/null; } 2>/dev/null; xfsz=$?; rm -rf "$d"
while read -r k v; do [ "$k" != SigIgn: ] || ign=$v; done </proc/$$/status
echo "pipe $pipe, xfsz $xfsz, ignored $ign, subreaper [\${LEGION_E2E_SUBREAPER:-}] of $$ $(cut -d' ' -f22 </proc/$$/stat)"`;

test("a proof under lib/subreaper.py has SIGPIPE and SIGXFSZ at their defaults, as a proof started without it has: a pipeline whose reader exits first ends 141, and the shell is named by its pid and its start", () => {
  requireHostTools("python3");
  // Python ignores SIGPIPE and SIGXFSZ at its start, and an exec keeps an ignored signal ignored,
  // which the bash it runs cannot undo: under 14fc06e9e8's subreaper.py `seq | head -n 1` ended 1
  // with "Broken pipe", and github-apps.sh:522's over-limit token command answered 1 instead of
  // its documented 126. The same shell runs plain beside it as the control: the signals it
  // ignores are the ones the test runner gave it (bun:test's children ignore SIGQUIT), and the
  // shell under subreaper.py must ignore those and no more.
  const plain = Bun.spawnSync(["bash", "-c", subreaperSignals], {
    env: { PATH: process.env.PATH ?? "" },
  });
  const under = Bun.spawnSync(["python3", "-I", subreaperPy, "bash", "-c", subreaperSignals], {
    env: { PATH: process.env.PATH ?? "" },
  });
  const plainSaid = plain.stdout.toString().replace(/, subreaper .*$/m, "");
  expect(plainSaid).toMatch(/^pipe 141, xfsz 153, ignored ([0-9a-f]{16})?\n$/);
  expect({
    under: under.stdout.toString().replace(/, subreaper .*$/m, ""),
    err: under.stderr.toString(),
  }).toEqual({ under: plainSaid, err: "" });
  const named = /\[(\d+) (\d+)\] of (\d+) (\d+)$/m.exec(under.stdout.toString());
  expect(named?.slice(1, 3)).toEqual(named?.slice(3, 5));
}, 60_000); // A guard on a hang only: two shells of a few commands each.

test("lib/subreaper.py where the mark cannot be set, its Python without ctypes, says why by name, names the shell refused, and runs the proof all the same", () => {
  requireHostTools("python3");
  // A Python with no ctypes, or a libc with no prctl, raised before the fallback a kernel refusal
  // took, so the proof exited 1 with a traceback before it started. Here a ctypes module that
  // fails to import stands first on the path (python3 without -I reads PYTHONPATH).
  const fake = join(dir, "no-ctypes");
  mkdirSync(fake);
  writeFileSync(join(fake, "ctypes.py"), 'raise ImportError("no ctypes on this box")\n');
  const r = Bun.spawnSync(
    ["python3", subreaperPy, "bash", "-c", `echo "ran, [\${LEGION_E2E_SUBREAPER:-}]"`],
    { env: { PATH: process.env.PATH ?? "", PYTHONPATH: fake } }
  );
  expect({
    code: r.exitCode,
    out: r.stdout.toString().replace(/\[\d+ \d+ refused\]/, "[PID START refused]"),
    err: r.stderr.toString(),
  }).toEqual({
    code: 0,
    out: "ran, [PID START refused]\n",
    err: "subreaper.py: no prctl to call (ImportError: no ctypes on this box); running bash without it\n",
  });
}, 60_000); // A guard on a hang only: one Python and one shell.

test("lib/subreaper.py with no /proc to read its own start from says why, names the shell refused with no start, and runs the proof all the same, and lib/subreaper.sh runs that proof once", () => {
  requireHostTools("python3");
  // start_of read /proc/<pid>/stat unguarded, so on a host with no /proc subreaper.py ended with a
  // traceback before the proof started (a reviewer's reading at 49fafd2027). Here a sitecustomize
  // that refuses every open under /proc/ stands first on the path (python3 without -I reads
  // PYTHONPATH). Through lib/subreaper.sh, a python3 stand-in that drops -I, and so reads it,
  // gives the shell a value of its own pid with no start, which the shell's own start does not
  // match: a subreaper.sh that took only `PID START` for done ran the proof under subreaper.py
  // again, without end. The stand-in counts its calls: the shell's check of python3, and the run.
  const fake = join(dir, "no-proc");
  mkdirSync(join(fake, "bin"), { recursive: true });
  writeFileSync(
    join(fake, "sitecustomize.py"),
    `import builtins
_open = builtins.open
def _refused(path, *args, **kwargs):
    if str(path).startswith("/proc/"):
        raise FileNotFoundError(2, "No such file or directory", str(path))
    return _open(path, *args, **kwargs)
builtins.open = _refused
`
  );
  const said = (r: { exitCode: number | null; stdout: Buffer; stderr: Buffer }) => ({
    code: r.exitCode,
    out: r.stdout.toString().replace(/\[\d+ {2}refused\]/, "[PID  refused]"),
    err: r.stderr.toString().replace(/\b\d+\b/g, "PID"),
  });
  const direct = Bun.spawnSync(
    ["python3", subreaperPy, "bash", "-c", `echo "ran, [\${LEGION_E2E_SUBREAPER:-}]"`],
    { env: { PATH: process.env.PATH ?? "", PYTHONPATH: fake } }
  );
  expect(said(direct)).toEqual({
    code: 0,
    out: "ran, [PID  refused]\n",
    err: "subreaper.py: no start to read for process PID in /proc/PID/stat; running bash without it\n",
  });
  // The interpreter itself (sys.executable), not the first python3 on PATH: a shim there (mise's)
  // finds python3 through PATH again, which would be this stand-in.
  const python = Bun.spawnSync(["python3", "-c", "import sys; print(sys.executable)"], {
    env: { PATH: process.env.PATH ?? "" },
  })
    .stdout.toString()
    .trim();
  const counted = join(fake, "calls");
  writeFileSync(
    join(fake, "bin", "python3"),
    `#!/bin/bash
echo call >>${JSON.stringify(counted)}
args=()
for a; do [ "$a" = -I ] || args+=("$a"); done
PYTHONPATH=${JSON.stringify(fake)} exec ${JSON.stringify(python)} "\${args[@]}"
`,
    { mode: 0o755 }
  );
  const proof = join(fake, "proof.sh");
  writeFileSync(
    proof,
    `set -eu\n. ${JSON.stringify(join(import.meta.dir, "subreaper.sh"))}\necho "proof ran, [\${LEGION_E2E_SUBREAPER:-}]"\n`
  );
  const viaSh = Bun.spawnSync(["bash", proof], {
    env: { PATH: `${join(fake, "bin")}:${process.env.PATH ?? ""}` },
    timeout: 20_000,
  });
  expect({
    ...said(viaSh),
    err: said(viaSh).err.replace(/running \S+ without/, "running BASH without"),
    calls: readFileSync(counted, "utf8"),
  }).toEqual({
    code: 0,
    out: "proof ran, [PID  refused]\n",
    err: "subreaper.py: no start to read for process PID in /proc/PID/stat; running BASH without it\n",
    calls: "call\ncall\n",
  });
}, 60_000); // A guard on a hang only: two Pythons and two shells, the second bounded at 20 s on its own.

// namespaces is the gate on whether this user may make a private user and pid namespace
// (util-linux unshare, a user namespace that maps this user to itself) and enter one (nsenter), as
// the pid-namespace rows do; the GitHub Actions test job allows them (its step "Allow unprivileged
// user namespaces").
const namespacesHold =
  hostTool("unshare") !== null &&
  hostTool("nsenter") !== null &&
  Bun.spawnSync(
    ["unshare", "--user", "--map-current-user", "--pid", "--fork", "--mount-proc", "true"],
    { stdout: "ignore", stderr: "ignore" }
  ).exitCode === 0;
const namespacesRefused = namespacesHold
  ? ""
  : "This host refused an unprivileged user and pid namespace (unshare --user --map-current-user --pid --fork --mount-proc true), or has no unshare or nsenter.";
const namespaces = hostGate({ programs: ["perl"], reason: namespacesRefused });

// adopted is the gate on whether this kernel gives the orphan that a child subreaper's shell leaves
// in an older pid namespace to that namespace's init, as Linux does (a subreaper adopts within its
// own namespace only), and so whether the older-namespace row below can run: gVisor gives such an
// orphan to the subreaper shell itself (the reviewer's probe: orphan 23779, parent 23776, the
// shell), where the walk judges it by its parent, the shell, and the row has no init to read. The
// probe makes a namespace (unshare), then from a shell under lib/subreaper.py starts orphanPy there
// (nsenter) under a sh that exits at once, and reads the python3's parent. The GitHub Actions
// runner's Linux kernel gives it to the init. The row needs the pid-namespace rows' namespace too,
// so its gate carries their reason as well.
const adopterProbe = `set -u
unshare --user --map-current-user --pid --fork --kill-child --mount-proc sh -c 'exec sleep 30' </dev/null >/dev/null 2>&1 &
u=$!
init=
for _ in $(seq 1 200); do init=$(cut -d' ' -f1 <"/proc/$u/task/$u/children" 2>/dev/null); [ -n "$init" ] && break; sleep 0.05; done
d=$(mktemp -d)
INIT=$init D=$d python3 -I "$SUBREAPER" bash -c "$INNER"
kill -KILL "$u" 2>/dev/null; wait "$u" 2>/dev/null; rm -rf "$d"`;
const adopterInner = `nsenter --preserve-credentials --user --pid --target "$INIT" -- sh -c 'python3 -I -c "$1" "$2" </dev/null >/dev/null 2>&1 & exit 0' sh "$PY" "$D/orphan"
o=
for _ in $(seq 1 200); do { read -r o <"$D/orphan"; } 2>/dev/null && break; sleep 0.05; done
p=
for _ in $(seq 1 100); do p=$(cut -d' ' -f4 <"/proc/$o/stat" 2>/dev/null); [ "$p" = "$INIT" ] || [ "$p" = $$ ] && break; sleep 0.05; done
if [ "$p" = "$INIT" ]; then echo init; elif [ "$p" = $$ ]; then echo "the subreaper shell"; else echo "process $p"; fi
kill -KILL "$o" 2>/dev/null`;
const adopter =
  namespacesHold && hostTool("python3") !== null
    ? Bun.spawnSync(["bash", "-c", adopterProbe], {
        env: {
          PATH: process.env.PATH ?? "",
          SUBREAPER: subreaperPy,
          INNER: adopterInner,
          PY: orphanPy,
        },
        stdout: "pipe",
        stderr: "ignore",
        timeout: 20_000,
      })
        .stdout.toString()
        .trim()
    : "";
const adopted = hostGate({
  programs: ["perl"],
  reason: [
    namespacesRefused,
    !namespacesHold || hostTool("python3") === null || adopter === "init"
      ? ""
      : `This kernel gave an orphan left in an older pid namespace to ${adopter || "no process the probe could read"}, not to that namespace's init.`,
  ]
    .filter((said) => said !== "")
    .join(" "),
});

namespaces.test(
  "a walk from a shell that entered a pid namespace from outside is no answer for an unreadable orphan of the run that the namespace's init adopted",
  async () => {
    requireHostTools("python3");
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
  },
  // A guard on a hang only, as for the rows above.
  60_000
);

adopted.test(
  "a walk from a subreaper shell is no answer for an unreadable orphan of the run that the init of an older pid namespace adopted",
  async () => {
    requireHostTools("python3");
    // A child subreaper adopts the run's orphans in its own pid namespace only: a process the run
    // starts in another one goes to that namespace's init when its parent ends, however far above
    // the subreaper is. A namespace older than the walking shell has an init older than it, so the
    // walk from a subreaper shell, which passed every process whose first older parent was not the
    // shell, passed such an orphan, 0 and nothing named (a reviewer's forcing). The row makes a
    // namespace before the harness's shell starts (unshare, whose init is a sh that becomes a
    // sleep); the shell, under lib/subreaper.py, enters it (nsenter) to start a marked
    // not-dumpable python3 there, whose sh exits at once, so the namespace's init adopts it; and
    // walks.
    const work = join(dir, "subreaper-older-ns");
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
    const body = `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(root)}
work=$WORK
. ${JSON.stringify(rig)}
export LEGION_E2E_RUN=$work
${unreadableSh}
[ "\${LEGION_E2E_SUBREAPER:-}" = "$$ $(proc_start $$)" ] && echo "the shell is the subreaper" || echo "the shell is no subreaper"
nsenter --preserve-credentials --user --pid --target ${init} -- sh -c 'python3 -I -c "$1" "$2" </dev/null >/dev/null 2>&1 & exit 0' sh "$PY" "$work/orphan.ready"
linked "$work/orphan.ready"; orphan=$pid
for i in $(seq 1 200); do [ "$(proc_parent "$orphan")" != ${init} ] || break; sleep 0.05; done
echo "orphan $(readable "$orphan"), parent $([ "$(proc_parent "$orphan")" = ${init} ] && echo "the namespace's init" || echo elsewhere)"
st=0; run_processes >"$work/rp.out" 2>"$work/rp.err" || st=$?
echo "run_processes $st, named [$(tr '\\n' ' ' <"$work/rp.out")]"
echo "pid $orphan"`;
    const shell = Bun.spawnSync(ownGroup(["python3", "-I", subreaperPy, "bash", "-c", body]), {
      env: { PATH: process.env.PATH ?? "", WORK: work, PY: orphanPy, proc_root: rowRoot() },
      stdout: "pipe",
      stderr: "pipe",
    });
    const out = shell.stdout.toString();
    const orphan = /^pid (\d+)$/m.exec(out)?.[1];
    if (orphan !== undefined) started.push(Number(orphan));
    expect({ err: shell.stderr.toString(), out: out.replace(/^pid \d+\n/m, "") }).toEqual({
      err: "",
      out: "the shell is the subreaper\norphan unreadable, parent the namespace's init\nrun_processes 2, named []\n",
    });
    expect(readFileSync(join(work, "rp.err"), "utf8").trimEnd()).toMatch(
      new RegExp(
        `^procs_naming: process ${orphan} \\([^)\\n]+\\) is this user's, started after this shell, and its environment cannot be read, so it may carry the run's mark and the processes naming \\S+ were not checked$`
      )
    );
  },
  // A guard on a hang only, as for the rows above.
  60_000
);

// docker is the gate on whether this host runs a rootful Docker daemon on its own kernel for this
// user, as the docker-init row below needs: a `docker` CLI whose `docker info` answers, with nothing
// on stderr, no `name=rootless` among its security options and an operating system that is no
// Docker Desktop. Rootless Docker runs `docker exec -u <uid>:<gid>` as a subuid, so the row's
// readable control reads unreadable; Docker Desktop runs its daemon in a VM, whose pids
// (`.State.Pid`) this /proc does not show; and podman's docker shim writes a notice on stderr and
// runs catatonit, no docker-init. A Legion pod has no Docker. The GitHub Actions runner has a
// rootful Docker. The row's image is the skill scenarios' pinned judge image (ubuntu, which
// carries perl), a row of .github/ci-images.yaml.
const dockerImage =
  "ghcr.io/cybermonklabs/legion/ci@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55";
const docker = hostGate({
  programs: ["perl", "docker"],
  reason: (() => {
    if (hostTool("docker") === null) return "";
    const info = Bun.spawnSync(
      ["docker", "info", "--format", "{{json .SecurityOptions}}|{{.OperatingSystem}}"],
      { stdout: "pipe", stderr: "pipe", timeout: 30_000 }
    );
    const out = info.stdout.toString().trim();
    const err = info.stderr.toString().trim();
    if (info.exitCode !== 0)
      return `This host's docker info exited ${info.exitCode}: ${err.split("\n")[0]}.`;
    if (err !== "")
      return `This host's docker info wrote on stderr, as podman's docker shim does: ${err.split("\n")[0]}.`;
    if (out.includes("name=rootless"))
      return "This host's Docker daemon is rootless (docker info: name=rootless).";
    if (out.includes("Docker Desktop"))
      return "This host's Docker daemon is Docker Desktop's, in a VM.";
    return "";
  })(),
});

// pulledImage makes sure this host holds IMAGE as the skill-scenario rig's judge-omp does
// (packages/pi-legion/scripts/skill-scenarios/rig.sh): only where `docker image inspect` does not
// find it, quietly, three attempts 2 s apart, since the registry answers one request of a batch
// badly now and then and serves the next. It answers "" once the image is held, else why not, with
// the last attempt's own words.
function pulledImage(image: string): string {
  const held = Bun.spawnSync(["docker", "image", "inspect", image], {
    stdout: "ignore",
    stderr: "ignore",
    timeout: 60_000,
  });
  if (held.exitCode === 0) return "";
  let said = "";
  for (let attempt = 1; attempt <= 3; attempt++) {
    const pull = Bun.spawnSync(["docker", "pull", "--quiet", image], {
      stdout: "ignore",
      stderr: "pipe",
      timeout: 120_000,
    });
    if (pull.exitCode === 0) return "";
    said = pull.stderr.toString().trim() || `docker pull exited ${pull.exitCode}`;
    if (attempt < 3) Bun.sleepSync(2000);
  }
  return `docker could not pull ${image} in 3 attempts: ${said}`;
}

docker.test(
  "a not-dumpable process that `docker exec` left to a rootful container's init, older than the walking shell, cannot carry the run's mark: the walk passes it from a subreaper shell and from a plain one",
  async () => {
    requireHostTools("python3");
    // An init older than the walking shell that is the init of another pid namespace counted as an
    // adopter of the run's orphans, since the run's own orphan in a namespace it entered goes there.
    // A rootful container's docker-init is such an init, but the run cannot enter its namespace (it
    // takes privilege over root's), and `docker exec` gives what it starts there the environment the
    // runtime gives it, which holds no mark of the run unless a caller copies it in by name (`-e`):
    // so another session's not-dumpable process under it failed every walk with 2 (Tester4784-3 at
    // fe4ead44bb, from a subreaper shell and from a plain one). The row starts such a container,
    // then each walking shell, which has `docker exec -u <this uid>:<this gid>` start a perl whose sh
    // exits at once, so docker-init adopts it, links it into the row's proc root (rowRoot,
    // unreadableSh's `linked`) and walks; the perl makes itself not dumpable (prctl
    // PR_SET_DUMPABLE 0), or stays dumpable, the control, which the walk reads. The gid is given
    // too: `-u <uid>` alone runs the perl with gid 0 where the image's /etc/passwd has no such uid
    // (the GitHub Actions runner's 1001), and the kernel then keeps even a dumpable perl's
    // environment from this user, whose fsgid differs.
    const work = join(dir, "docker-init");
    mkdirSync(work);
    const name = `legion-e2e-docker-init-${process.pid}-${Date.now()}`;
    // The image is held before `docker run`, so on a host that does not hold it yet (the GitHub
    // Actions runner) `docker run` writes nothing on stderr; a pull that failed fails the row with
    // its own words.
    expect(pulledImage(dockerImage)).toBe("");
    const run = Bun.spawnSync(
      ["docker", "run", "-d", "--init", "--name", name, dockerImage, "sleep", "120"],
      {
        stdout: "pipe",
        stderr: "pipe",
        timeout: 90_000,
      }
    );
    try {
      expect({ code: run.exitCode, err: run.stderr.toString() }).toEqual({ code: 0, err: "" });
      const init = Number(
        Bun.spawnSync(["docker", "inspect", "-f", "{{.State.Pid}}", name]).stdout.toString().trim()
      );
      expect(readFileSync(`/proc/${init}/comm`, "utf8")).toBe("docker-init\n");
      await pastStarts([init]);
      const body = `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(root)}
work=$WORK
. ${JSON.stringify(rig)}
export LEGION_E2E_RUN=$work
${unreadableSh}
[ "\${LEGION_E2E_SUBREAPER:-}" = "$$ $(proc_start $$)" ] && echo "the shell is the subreaper" || echo "the shell is no subreaper"
case $(docker exec "$C" uname -m) in x86_64) nr=157 ;; aarch64) nr=167 ;; *) echo "no prctl number for this machine"; exit 3 ;; esac
pre=
[ "$KIND" != unread ] || pre="syscall($nr, 4, 0, 0, 0, 0) == 0 or die; "
# The perl names itself (\\$0, its command line, which /proc shows to every user) only once its prctl
# is done, so the row reads it after it has made itself not dumpable, never between its exec and
# that call.
docker exec -u "$(id -u):$(id -g)" "$C" sh -c "perl -e '\${pre}\\$0 = q(walk-row-perl); sleep 60' </dev/null >/dev/null 2>&1 & exit 0"
perl=
for i in $(seq 1 200); do
  kids=()
  { read -r -a kids <"/proc/$INIT/task/$INIT/children"; } 2>/dev/null || :
  for p in \${kids[@]+"\${kids[@]}"}; do
    cmd=
    { IFS= read -r -d '' cmd <"/proc/$p/cmdline"; } 2>/dev/null || :
    [ "$cmd" != walk-row-perl ] || perl=$p
  done
  [ -z "$perl" ] || break
  sleep 0.05
done
[ -n "$perl" ] || { echo "no perl under the container's init" >&2; exit 3; }
printf '%s\\n' "$perl" >"$work/perl.$KIND.ready"
linked "$work/perl.$KIND.ready"
echo "pid $pid"
echo "perl $(readable "$pid"), parent $([ "$(proc_parent "$pid")" = "$INIT" ] && echo "the container's init" || echo elsewhere)"
st=0; run_processes >"$work/rp.out" 2>"$work/rp.err" || st=$?
echo "run_processes $st, named [$(tr '\\n' ' ' <"$work/rp.out")], said [$(tr '\\n' ' ' <"$work/rp.err")]"`;
      for (const kind of ["unread", "readable"]) {
        for (const shellKind of ["subreaper", "plain"]) {
          const argv =
            shellKind === "subreaper"
              ? ["python3", "-I", subreaperPy, "bash", "-c", body]
              : ["bash", "-c", body];
          const shell = Bun.spawnSync(ownGroup(argv), {
            env: {
              PATH: process.env.PATH ?? "",
              WORK: work,
              C: name,
              INIT: String(init),
              KIND: kind,
              proc_root: rowRoot(),
            },
            stdout: "pipe",
            stderr: "pipe",
            timeout: 60_000,
          });
          const out = shell.stdout.toString();
          const perl = /^pid (\d+)$/m.exec(out)?.[1];
          if (perl) started.push(Number(perl));
          expect({
            kind,
            shellKind,
            err: shell.stderr.toString(),
            out: out.replace(/^pid \d+\n/m, ""),
          }).toEqual({
            kind,
            shellKind,
            err: "",
            out: `the shell is ${shellKind === "subreaper" ? "the subreaper" : "no subreaper"}\nperl ${kind === "unread" ? "unreadable" : "readable"}, parent the container's init\nrun_processes 0, named [], said []\n`,
          });
          if (perl) process.kill(Number(perl), "SIGKILL");
        }
      }
    } finally {
      Bun.spawnSync(["docker", "rm", "-f", name], {
        stdout: "ignore",
        stderr: "ignore",
        timeout: 60_000,
      });
    }
  },
  // A guard on a hang only: one container, a pull of its image the first time, and four walks.
  180_000
);

// rootAccess is the gate on whether this host gives this user root with no password (`sudo -n`) and
// has unshare, nsenter and setpriv, as the root-namespace row below needs; the GitHub Actions runner
// has them.
const rootAccess = hostGate({
  programs: ["perl", "sudo", "unshare", "nsenter", "setpriv"],
  reason:
    hostTool("sudo") === null ||
    Bun.spawnSync(["sudo", "-n", "true"], { stdout: "ignore", stderr: "ignore", timeout: 30_000 })
      .exitCode === 0
      ? ""
      : "This host gives this user no root without a password (sudo -n true failed).",
});

rootAccess.test(
  "a walk from a shell in a pid namespace below the one its /proc shows is no answer: it says so and exits 2, never reading another process as itself, beside an unreadable orphan of the run that the namespace's root-owned init adopted",
  () => {
    requireHostTools("python3");
    // ns_init_adopts takes an init whose links this user cannot read for one whose namespace the run
    // could not enter, and the init of the walking shell's own namespace for pid 1. Both hold only
    // for a shell in the pid namespace /proc shows: a shell that entered a namespace without
    // mounting its /proc has a $$ that /proc numbers as another process, and its own namespace's init
    // shows there by its outer pid, root's or anyone's, with the run's orphans under it. So the walk
    // refuses such a shell by name. Here root makes a pid namespace (its init a sleep of root's), and
    // the shell enters it as this user with /proc left the host's (sudo nsenter --pid, setpriv); it
    // starts a not-dumpable python3 whose sh exits at once, so the namespace's init adopts it, and
    // walks.
    const work = join(dir, "root-namespace");
    mkdirSync(work);
    const body = `set -Eeuo pipefail
trap 'echo "ERR trap: $BASH_COMMAND" >&2' ERR
root=${JSON.stringify(root)}
work=$WORK
. ${JSON.stringify(rig)}
export LEGION_E2E_RUN=$work
${unreadableSh}
echo "the init's links $([ -e "/proc/$INIT/ns/pid" ] && echo readable || echo unreadable), this shell's pid in /proc's numbering: $([ /proc/self -ef "/proc/$$" ] && echo "$$" || echo "not $$")"
sh -c 'python3 -I -c "$1" "$2" </dev/null >/dev/null 2>&1 & exit 0' sh "$PY" "$work/orphan.ready"
linked "$work/orphan.ready"; orphan=$pid
for i in $(seq 1 200); do [ "$(proc_parent "$orphan")" != "$INIT" ] || break; sleep 0.05; done
echo "orphan $(readable "$orphan"), parent $([ "$(proc_parent "$orphan")" = "$INIT" ] && echo "the namespace's init" || echo elsewhere)"
st=0; run_processes >"$work/rp.out" 2>"$work/rp.err" || st=$?
echo "run_processes $st, named [$(tr '\\n' ' ' <"$work/rp.out")]"
echo "pid $orphan"`;
    // Root's part: the namespace, a wait past its init's start tick, the shell, and the namespace's
    // end (its init killed, which kills every process in it) on any exit.
    const asRoot = `set -u
unshare --pid --fork --kill-child sh -c 'exec sleep 60' </dev/null >/dev/null 2>&1 &
u=$!
trap 'kill -KILL "$u" 2>/dev/null; wait "$u" 2>/dev/null' EXIT
init=
for _ in $(seq 1 200); do init=$(cut -d' ' -f1 "/proc/$u/task/$u/children" 2>/dev/null); [ -n "$init" ] && break; sleep 0.05; done
[ -n "$init" ] || { echo "no init in the namespace" >&2; exit 3; }
sleep 1
nsenter --target "$init" --pid -- setpriv --reuid="$1" --regid="$2" --init-groups env -i PATH="$3" WORK="$4" PY="$5" proc_root="$6" INIT="$init" bash -c "$7"`;
    const ran = Bun.spawnSync(
      [
        "sudo",
        "-n",
        "bash",
        "-c",
        asRoot,
        "-",
        String(process.getuid?.() ?? 0),
        String(process.getgid?.() ?? 0),
        process.env.PATH ?? "",
        work,
        orphanPy,
        rowRoot(),
        body,
      ],
      { stdout: "pipe", stderr: "pipe", timeout: 60_000 }
    );
    const out = ran.stdout.toString();
    const orphan = /^pid (\d+)$/m.exec(out)?.[1];
    expect({ err: ran.stderr.toString(), out: out.replace(/^pid \d+\n/m, "") }).toEqual({
      err: "",
      out: "the init's links unreadable, this shell's pid in /proc's numbering: not 2\norphan unreadable, parent the namespace's init\nrun_processes 2, named []\n",
    });
    expect(readFileSync(join(work, "rp.err"), "utf8").trimEnd()).toMatch(
      /^procs_naming: this shell is pid 2 in a pid namespace below the one \/proc shows \(NSpid \d+ \d+\), so its reads of \/proc are of other processes and the processes naming \S+ were not read$/m
    );
    expect(orphan).toMatch(/^\d+$/);
  },
  // A guard on a hang only, as for the rows above.
  60_000
);

host.test(
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

host.test(
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

host.test(
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
    // A sweep made inside the escaped process's exec of sleep reads no mark in its environment and
    // leaves it running, as the double-forked row's walk names nothing then (untilExecDone).
    await untilExecDone(escaped, "sleep");
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

host.test(
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

host.test(
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

host.test(
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

host.test(
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

host.test(
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

host.test(
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

host.test(
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

host.test(
  "names and ends a marked process whose name holds a newline and a `) `, which a read of the stat's first line took for another parent and no start",
  async () => {
    requireHostTools("python3");
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
