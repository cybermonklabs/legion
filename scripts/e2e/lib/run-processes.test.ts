import { afterEach, beforeAll, expect } from "bun:test";
import {
  existsSync,
  mkdirSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  writeFileSync,
} from "node:fs";
import { endianness } from "node:os";
import { join } from "node:path";
import { hostGate, hostTool, requireHostTools } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { argMaxForceHolds } from "./arg-max-force";
import { ownGroup, untilRunning } from "./own-group";
import { procRootOf } from "./proc-root";
import { libRowBoundMs, rowWaitMarginMs, rowWaits, stateOf, untilGone } from "./row-waits";

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

// The bound of every row of this file (rowWaits sets it with setDefaultTimeout), whatever the
// runner's command line, is a hang guard that decides no verdict: every wait a row makes on the
// kernel ends rowWaitMarginMs before it (waits.end()), naming what it waited for and what it saw, so
// the walk and the cleanup after a wait still fit in the row, and a wait that runs out fails by its
// own message rather than by bun's timeout. No span shorter than the row decides a wait: a forced
// run on a devbox at a load near 450 (2026-11-21) held an escaped process inside its exec for more
// than 3 s, and the fixed 3 s wait this replaced failed the row. Bun gives a hook the default bound
// in force when the hook is registered, so this comes before every hook of the file,
// temporaryDirectory's afterAll that removes `dir` among them: on that devbox's shared scratch disk
// that removal ran 14.5 s to 20.6 s in three whole-file runs, past bun's 5 s for a hook, and failed
// the file as "(unnamed) … a beforeEach/afterEach hook timed out" while every row passed.
const waits = rowWaits(libRowBoundMs);
const dir = temporaryDirectory("run-processes-test");
// perl: every process a row starts, and every harness, runs through ownGroup (lib/own-group.ts).
const host = hostGate({ programs: ["perl"] });
// python3: the newline-name row renames its process through prctl (ctypes), and fails by name
// without it rather than skipping (requireHostTools).
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
// untilExecDone resolves once PID has finished its exec of PROGRAM as far as a walk of the marks
// reads it: its exe link (/proc/<pid>/exe) names PROGRAM and its /proc/<pid>/environ reads
// non-empty. Inside an exec Linux gives the process its new memory, and the exe link with it,
// before it sets where that memory's arguments and environment end (create_elf_tables in
// fs/binfmt_elf.c): first the arguments' end, then a placeholder end of the environment, which lets
// /proc/<pid>/cmdline read in full (get_mm_cmdline in fs/proc/base.c reads once that end is not 0),
// and only after it has counted each variable the environment's real end. Until then environ reads
// empty and a walk of the marks names nothing, so a wait on the exe link or on the command line can
// end inside the exec. Nothing signals the end of another process's exec, so it reads both every
// 50 ms, as untilGone and untilRunning do, until the row's waits end (waits.end()), and reads once
// more then; past that it rejects naming the pid, what its exe link read and how many bytes its
// environ held.
async function untilExecDone(pid: number, program: string): Promise<void> {
  const want = realpathSync(hostTool(program) ?? program);
  const end = waits.end();
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
    `pid ${pid} did not finish its exec of ${want} by the end of the row's waits (${(libRowBoundMs - rowWaitMarginMs) / 1000} s after the row began): its exe link read ${exe || "nothing"} and its environ ${environ} bytes`
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
// and the row takes a fresh port from a new process until the row's waits end (waits.end()), then
// fails naming every port 127.0.0.1 refused, a failure of its setup and not of listening_holders.
// Both LISTEN rows are read from /proc/net/tcp before the walk, which must name this test process
// alone.
async function onlyTheLoopbackHolder(name: string, program: string[], other: string) {
  const work = join(dir, name);
  mkdirSync(work);
  let listener: { port: number; stop(closeActiveConnections?: boolean): void } | undefined;
  const refused: string[] = [];
  const deadline = waits.end();
  try {
    while (listener === undefined) {
      if (performance.now() >= deadline) {
        throw new Error(
          `no port free on both ${other} and 127.0.0.1 by the end of the row's waits (${(libRowBoundMs - rowWaitMarginMs) / 1000} s after the row began): ${refused.length} ports tried, each refused on 127.0.0.1, so listening_holders was not run (${refused.join("; ")})`
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
  "a walk an exit trap runs reads its reader's own status: after a failed run's exit it names the run's process, and after a clean end a reader a signal ended still makes it exit 2",
  async () => {
    // A run's exit handler is a trap, and there bash's bare `exit` in a process substitution exits
    // with the status the trap was entered with, not its last command's. The walks hand their
    // reader's status out of one that way, so after a run that failed (`exit 1`) every walk read as
    // xargs exiting 1 and named nothing, and the sweep stopped nothing; after a clean end a grep or
    // a find that a signal ended read as 0, a whole walk.
    const work = join(dir, "trapped");
    mkdirSync(work);
    const marked = spawn(["sleep", "60"], { cwd: dir, env: { LEGION_E2E_RUN: work } });
    await untilExecDone(marked, "sleep");
    // A walk of a busy machine always meets a batch of entries with no match, which xargs reports
    // as 123, and only then does the walk hand a status out through its `exit`. The row's proc root
    // fits one batch that matches, so a grep that prints what the real one prints and then answers
    // as such a batch does stands in for the rest of the machine.
    const grep = hostTool("grep") ?? "grep";
    const find = hostTool("find") ?? "find";
    const busy = join(dir, "trapped-busy-bin");
    mkdirSync(busy);
    writeFileSync(join(busy, "grep"), `#!/bin/sh\n${grep} "$@"\nexit 1\n`, { mode: 0o755 });
    const killed = join(dir, "trapped-bin");
    mkdirSync(killed);
    writeFileSync(
      join(killed, "find"),
      `#!/bin/bash\n${find} "$@" | head -c 2000\nkill -KILL $$\n`,
      {
        mode: 0o755,
      }
    );
    writeFileSync(join(killed, "grep"), `#!/bin/bash\n${grep} "$@" | head -c 0\nkill -KILL $$\n`, {
      mode: 0o755,
    });
    const listener = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });
    const walks = `walks() {
  st=0; run_processes >"$work/rp.out" 2>"$work/rp.err" || st=$?; echo "run_processes $st"
  st=0; listening_holders ${listener.port} >"$work/lh.out" 2>"$work/lh.err" || st=$?; echo "listening_holders $st"
}
trap walks EXIT`;
    try {
      const failed = harness(work, `${walks}\nexit 1`, busy);
      expect(failed).toEqual({
        code: 1,
        out: "run_processes 0\nlistening_holders 0\n",
        err: "",
      });
      expect(pids(readFileSync(join(work, "rp.out"), "utf8"))).toEqual([String(marked)]);
      expect(readFileSync(join(work, "lh.out"), "utf8")).toBe(`${process.pid}\n`);
      const clean = harness(work, walks, killed);
      expect(clean).toEqual({
        code: 0,
        out: "run_processes 2\nlistening_holders 2\n",
        err: "",
      });
      expect(readFileSync(join(work, "rp.err"), "utf8")).toMatch(
        /^procs_naming: xargs exited 125 reading /m
      );
      expect(readFileSync(join(work, "lh.err"), "utf8")).toMatch(
        /^listening_holders: find exited 137 reading \/\S+: /m
      );
    } finally {
      listener.stop(true);
    }
  }
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
    await untilRunning(marked, "sleep", waits.end());
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
    await untilGone(marked, "the marked process", waits.end());
    await untilGone(escaped, "the escaped marked process", waits.end());
    // Running, and not left stopped either.
    expect({ shell: stateOf(shell), tail: stateOf(tail) }).toEqual({ shell: "S", tail: "S" });
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
    await untilRunning(pid, "sleep", waits.end());
    // Every signal the sweep sends goes through `kill`, here a function that records it and sends
    // it, so what reached the pid is read from the record rather than from a state that a signal
    // still in flight has not changed yet. A sweep given the pid's own start must then fill the
    // record, so an empty one before it is a record that could see a signal.
    const r = harness(
      work,
      `kill() { printf '%s\\n' "$*" >>"$work/kills"; builtin kill "$@"; }
marked_processes() { printf '%s %s\\n' ${pid} 1; }
stop_marked_processes "$work"; echo "status $?"
sent=$(grep -cw -- ${pid} "$work/kills" 2>/dev/null || :)
echo "sent to the pid: \${sent:-0}"
read -r s </proc/${pid}/stat; s=\${s##*) }; echo "state \${s%% *}"
marked_processes() { printf '%s %s\\n' ${pid} "$(proc_start ${pid})"; }
stop_marked_processes "$work" >/dev/null 2>&1 || :
grep -qw -- ${pid} "$work/kills" && echo "its own start: the record holds its stop" || echo "its own start: nothing in the record"`
    );
    expect(r.err).toBe("");
    expect(r.out).toBe(
      "status 0\nsent to the pid: 0\nstate S\nits own start: the record holds its stop\n"
    );
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
    await untilGone(parent.pid, "the marked process", waits.end());
    await untilGone(child, "the child it started under env -i", waits.end());
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
    await untilGone(child, "the child the marked process started under env -i", waits.end());
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
    await untilRunning(pid, "sleep", waits.end());
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
    await untilGone(pid, "the marked process the sweep froze", waits.end());
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
    await untilRunning(stranger, "sleep", waits.end());
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
    await untilGone(parent.pid, "the marked process", waits.end());
    await untilGone(child, "its child", waits.end());
    expect(stateOf(stranger)).toBe("S");
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
    await untilRunning(strangers, "sleep", waits.end());
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
    await untilGone(marked.pid, "the marked process", waits.end());
    expect(stateOf(strangers)).toBe("S");
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
    await untilGone(pid, "the renamed marked process", waits.end());
  }
);
