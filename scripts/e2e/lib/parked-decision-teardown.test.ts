import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard";
import { hostGate, requireHostTools } from "@legion/test-fixtures/host-tools";
import { socketPathRefusal, socketRoot } from "@legion/test-fixtures/socket-root";
import {
  temporaryDirectories,
  temporaryDirectory,
} from "@legion/test-fixtures/temporary-directories";
import { descendantsOf, notInRoot, procRootOf, unrealPid } from "./proc-root";
import { libRowBoundMs, rowWaits, untilTrue } from "./row-waits";
import { scriptFunctions } from "./script-functions";

// stop_agent_daemons (parked-decision-devbox.sh), the teardown of what the run's Oh My Pi sessions
// start detached: a per-project daemon broker, a session leader of its own that writes broker.pid
// in its runtime directory, and the services it supervises. On the pinned build each service runs
// in a session of its own and ends when its broker ends; the stand-in instead keeps its two members
// in its own session, so the test also holds the function to every member of a broker's session.
// The stand-in is started from a tmux pane as Oh My Pi starts its broker, `setsid` into a session
// of its own with OMP_DAEMON_RUNTIME_DIR set, and runs two members: a service working outside the
// run's directory, found by that variable, and a helper working inside it. tmux kill-server ends
// the pane and leaves all three alive, as the brokers outlived cleanup-is-complete in the devbox
// run at 6512c7fc. The pane the stand-in broker starts is a real tmux pane, and the teardown this
// test is about is tmux kill-server's; stop_agent_daemons reads each broker.pid with jq. So a host
// with no tmux or no jq skips the tmux row naming what it lacks, and under GITHUB_ACTIONS fails it
// by its own name, naming it (hostGate); the newline row, which starts no tmux, is gated on jq
// alone. CI's `test` job (.github/workflows/ci-job-test.yaml) installs neither: the
// ubuntu-24.04-arm image it runs on already carries both.
const e2e = join(import.meta.dir, "..");
const root = join(e2e, "..", "..");
const scriptPath = join(e2e, "parked-decision-devbox.sh");
const fn = scriptFunctions(scriptPath);
const set = readFileSync(scriptPath, "utf8").match(/^set -.*$/m)?.[0];
if (set === undefined) throw new Error(`${scriptPath} has no top-level set line`);

// Read once, at module scope: a call inside the test body would print the warning per row.
const host = hostGate({ programs: ["tmux", "jq"] });
const jq = hostGate({ programs: ["jq"] });

// stop_agent_daemons (parked-decision-devbox.sh) reads a stat and an environment for every process
// of `${proc_root:-/proc}`, and each row hands it a proc root of its own processes
// (lib/proc-root.ts), so its walk reads those alone, not the host's process count. 30 s is the
// default of the hooks registered above rowWaits below, the scratch directories' among them, which
// name no bound of their own (Bun 1.4.4 gives a hook the default in force where it is
// registered); the rows take the directory's row bound from rowWaits, and the teardown hook
// HANG_GUARD_MS.
setDefaultTimeout(30_000);

const dir = temporaryDirectory("parked-decision-teardown-test");
const started: number[] = [];
// What the test started that no pid of its own names: a tmux server to kill, a session a process
// writes its own pid file for. Each is registered the moment the test starts it, before the waits
// that can time out, so an unfinished test body still leaves nothing of the run alive. The hook is
// an afterEach registered above the tmux server's directory below: Bun 1.4.4 runs a scope's
// afterEach hooks in the order they were registered, a timed-out row's too, and all of them before
// an afterAll, so the server is stopped through its socket before that directory or `dir` goes.
const teardowns: (() => void | Promise<void>)[] = [];
// The bound of the teardown hook is HANG_GUARD_MS: a hang guard, not a verdict. Its work, a tmux
// server's kill and a broker's end (endBroker), is process ends and waits on them, and how long
// those take under load is the machine's.
// The row's proc root once it has made one, which the teardown reads too.
let procRoot: string | undefined;
afterEach(async () => {
  for (const teardown of teardowns.splice(0).reverse()) {
    try {
      await teardown();
    } catch {}
  }
  for (const pid of started.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
  procRoot = undefined;
}, HANG_GUARD_MS);

// tmux's Unix socket must fit below this directory, even when TMPDIR is long.
const tmuxPrefix = "parked-decision-teardown-tmux";
const tmuxSocket = join(`tmux-${process.getuid?.() ?? 0}`, "teardown");
const tmuxDirectory = temporaryDirectories(tmuxPrefix, {
  root: socketRoot("parked-decision teardown tmux socket", (root) =>
    socketPathRefusal(join(root, `${tmuxPrefix}-XXXXXX`, tmuxSocket))
  ),
});

/** The tmux binary behind PATH's command, resolved while the caller's own HOME still applies. It
 *  uses the budgeted socket name, so it fits wherever the teardown row's tmux socket fits. */
function realTmux(directory = tmuxDirectory()): string {
  const env = { ...process.env, TMUX_TMPDIR: directory };
  const name = "teardown";
  const made = Bun.spawnSync(
    ["tmux", "-L", name, "-f", "/dev/null", "new-session", "-d", "sleep 60"],
    {
      env,
    }
  );
  if (made.exitCode !== 0) throw new Error(`tmux resolution failed: ${made.stderr.toString()}`);
  try {
    const server = Bun.spawnSync(["tmux", "-L", name, "display-message", "-p", "#{pid}"], { env });
    if (server.exitCode !== 0)
      throw new Error(`tmux server lookup failed: ${server.stderr.toString()}`);
    return readlinkSync(`/proc/${server.stdout.toString().trim()}/exe`);
  } finally {
    Bun.spawnSync(["tmux", "-L", name, "kill-server"], { env });
  }
}

// The boundary row needs tmux alone, so it is gated on tmux alone.
const tmuxHost = hostGate({ programs: ["tmux"] });
tmuxHost.test("resolves tmux at the socket-path boundary the teardown row budgets", () => {
  const base = mkdtempSync("/tmp/pdt-");
  try {
    const padding = 107 - Buffer.byteLength(base) - 2 - Buffer.byteLength(tmuxSocket);
    if (padding < 1) throw new Error(`${base} leaves no room for the boundary directory`);
    const directory = join(base, "x".repeat(padding));
    mkdirSync(directory);
    expect(socketPathRefusal(join(directory, tmuxSocket))).toBeUndefined();
    expect(
      socketPathRefusal(
        join(directory, `tmux-${process.getuid?.() ?? 0}`, `resolve-${process.pid}`)
      )
    ).toBeDefined();
    expect(realTmux(directory)).toStartWith("/");
  } finally {
    rmSync(base, { recursive: true, force: true });
  }
});

const alive = (pid: number) => {
  try {
    process.kill(pid, 0);
  } catch {
    return false;
  }
  // A zombie still answers signal 0; it is gone for every purpose here.
  return !readFileSync(`/proc/${pid}/stat`, "utf8")
    .replace(/^.*\) /, "")
    .startsWith("Z");
};
const sessionOf = (pid: number) =>
  Number(
    readFileSync(`/proc/${pid}/stat`, "utf8")
      .replace(/^.*\) /, "")
      .split(" ")[3]
  );
// The broker LEADER and every live process it started that is still in its session, by the
// kernel's children lists (descendantsOf): the row's waits on the broker, which a scan of every
// process on the host would hold to the host's size. The stand-in's members are its own children.
const familyOf = (leader: number) =>
  [leader, ...descendantsOf(leader, true)].filter((pid) => {
    try {
      return sessionOf(pid) === leader && alive(pid);
    } catch {
      return false;
    }
  });
// membersOf SID: every live member of session SID the teardown can reach with no scan of the host:
// the leader's family while the leader lives, and every process of the row's proc root once the row
// has made it, which holds a member a dead broker left behind.
const membersOf = (sid: number) => {
  const rooted =
    procRoot === undefined
      ? []
      : readdirSync(procRoot)
          .filter((name) => /^\d+$/.test(name))
          .map(Number);
  return [...new Set([...familyOf(sid), ...rooted])].filter((pid) => {
    try {
      return sessionOf(pid) === sid && alive(pid);
    } catch {
      return false;
    }
  });
};
// The stand-in broker, written at PATH: `broker RUNTIME HOME [HOLD]`. Its first act names it, its
// own pid in RUNTIME/broker.started, before it starts its two members: a broker that ends its row
// before it writes broker.pid is then found by that file, and nothing has to look for it among the
// host's processes (a walk of every process's command line, synchronous in the test's own process,
// held Bun's event loop on a command line that never answered). It then starts a service working
// outside the run's directory and a helper working inside it, waits on HOLD when given (a FIFO the
// row opens, or never does), and names itself in broker.pid as the pinned broker does.
function writeBroker(path: string) {
  writeFileSync(
    path,
    `#!/usr/bin/env bash
runtime=$1 home=$2 hold=\${3:-}
printf '%s\\n' "$$" >"$runtime/broker.started.next"
mv "$runtime/broker.started.next" "$runtime/broker.started"
(cd / && exec sleep 300) &
(cd "$home" && exec sleep 301) &
[ -z "$hold" ] || read -r _ <"$hold"
printf '{"pid":%s,"domain":"test"}\\n' "$$" >"$runtime/broker.pid.next"
mv "$runtime/broker.pid.next" "$runtime/broker.pid"
wait
`,
    { mode: 0o755 }
  );
}
// endBroker RUNTIME STARTED: ends the whole session of the stand-in broker that runs on RUNTIME, a
// teardown's work. A broker leads its session, so the pid it names is the session's id even once
// the broker has ended, when only the row's proc root still holds the members it left. It reads
// broker.pid, else broker.started; when STARTED says the broker was started and neither is there
// yet, it waits up to 5 s for broker.started, the broker's first act, reading that file alone.
async function endBroker(runtime: string, started: () => boolean) {
  const named = () =>
    [join(runtime, "broker.pid"), join(runtime, "broker.started")].find((file) => existsSync(file));
  for (let i = 0; i < 50 && started() && named() === undefined; i++) await Bun.sleep(100);
  const file = named();
  if (file === undefined) return;
  const text = readFileSync(file, "utf8");
  killSession(file.endsWith(".pid") ? Number(JSON.parse(text).pid) : Number(text.trim()));
}
// killSession ends every member of the session SID leads, the whole of a stand-in broker. This
// test's own session is never one of them: a teardown that lands on a pid of the runner leaves it.
const killSession = (sid: number) => {
  if (sid === sessionOf(process.pid)) return;
  for (const pid of membersOf(sid)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
};
// until polls real processes, which no fake clock moves, for the condition it names, until the end
// of the row's waits, 20 s before the row's bound (lib/row-waits.ts), or until END where a row
// gives one: how soon a process starts, writes or ends is the kernel's, and a loaded host's, so a
// row decides on the condition itself and the time it allows is the row's, not a count's.
const waits = rowWaits(libRowBoundMs);
async function until(what: string, ok: () => boolean, end = waits.end()) {
  await untilTrue(what, end, () => ok() || "it does not hold yet");
}

// detached starts ARGV in a session of its own as a child of a shell that exits at once, and returns
// its pid. The process is no child of the test, so Bun's cleanup of a row that times out ("killed N
// dangling processes") does not end it; the row's own teardown does.
function detached(argv: string[], cwd: string, env: Record<string, string>) {
  const ran = Bun.spawnSync(
    ["bash", "-c", 'setsid "$@" </dev/null >/dev/null 2>&1 & echo "$!"', "detached", ...argv],
    { cwd, env, stdout: "pipe", stderr: "pipe" }
  );
  const pid = Number(ran.stdout.toString().trim());
  if (ran.exitCode !== 0 || !Number.isInteger(pid) || pid <= 0) {
    throw new Error(`could not start ${argv.join(" ")} detached: ${ran.stderr.toString()}`);
  }
  return pid;
}

// pathWithout NAME is a PATH of one directory holding a link to the first of each program on this
// PATH but NAME: the PATH of a host that lacks NAME and has everything else this one has.
function pathWithout(name: string): string {
  const bin = mkdtempSync(join(dir, `no-${name}-`));
  const linked = new Set([name]);
  for (const entry of (process.env.PATH ?? "").split(":")) {
    if (entry === "" || !existsSync(entry) || !statSync(entry).isDirectory()) continue;
    for (const program of readdirSync(entry)) {
      if (linked.has(program)) continue;
      linked.add(program);
      symlinkSync(join(entry, program), join(bin, program));
    }
  }
  return bin;
}

// harness runs BODY with the script's `set` line, lib/rig.sh sourced as the script sources it (for
// run_processes) and stop_agent_daemons taken from the script by name, its run marked as the script
// marks it (LEGION_E2E_RUN). $work comes in through the environment, and the mark is exported only
// once the shell runs, so the harness's own process carries no mark and run_processes does not name
// it. PROCROOT, when given, is the proc root the walks read (`proc_root`, lib/proc-root.ts).
// DEADLINE, when given (a performance.now() value, as waits.end() is), bounds the run by the row's
// remaining time: a run still going then is ended and the row fails naming BODY, inside its own
// time, since Bun cannot time out a row while a synchronous spawn holds it, and a row it times out
// after runs on to an unhandled error. PATH, when given, is the run's PATH in place of this
// process's.
function harness(
  work: string,
  body: string,
  { procRoot, deadline, path }: { procRoot?: string; deadline?: number; path?: string } = {}
) {
  // Whole milliseconds: Bun.spawnSync's timeout takes an integer, and performance.now() has fractions.
  const remaining = deadline === undefined ? undefined : Math.ceil(deadline - performance.now());
  if (remaining !== undefined && remaining <= 0) {
    throw new Error(`the row's time ran out before ${body}`);
  }
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `${set}
root=${JSON.stringify(root)}
work=$WORK
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL: %s\\n' "$*" >&2; exit 1; }
. "$root/scripts/e2e/lib/rig.sh"
export LEGION_E2E_RUN=$work
${fn("stop_agent_daemons")}
${body}`,
    ],
    {
      env: {
        PATH: path ?? process.env.PATH ?? "",
        WORK: work,
        ...(procRoot === undefined ? {} : { proc_root: procRoot }),
      },
      stdout: "pipe",
      stderr: "pipe",
      ...(remaining === undefined ? {} : { timeout: remaining }),
    }
  );
  if (ran.exitedDueToTimeout) {
    throw new Error(`the row's time ran out during ${body}`);
  }
  return { code: ran.exitCode, stdout: ran.stdout.toString(), stderr: ran.stderr.toString() };
}

host.test(
  "stops a detached broker's session that outlives tmux kill-server, and nothing it cannot tie to one",
  async () => {
    // The row decides on the broker session's stop, bounded only by the row's own bound: its stop
    // and check read the row's own proc root, and what is left (a tmux server, the broker, the
    // shells, their events) takes the machine's time (a run at load ~1,000 passed main's 30 s,
    // another at ~1,400 did not). Its waits end at the end of the row's waits (waits.end(), 20 s
    // before the file's row bound), each harness run is ended at that deadline (harness's
    // DEADLINE), and the tmux step checks the time first, so a row out of time fails by the name of
    // what it was doing, inside its own time, and Bun's own timeout, which would end the row's
    // children and leave the body running on to an unhandled expect, does not come first.
    const deadline = waits.end();
    const inTime = (step: string) => {
      if (performance.now() >= deadline) throw new Error(`the row's time ran out before ${step}`);
    };
    const work = join(dir, "work");
    const ws = join(work, "ws");
    const home = join(work, "home");
    const runtime = join(home, ".local/state/omp/profiles/p/run/daemons/0123456789abcdef");
    const stale = join(work, "state/omp/profiles/p/run/daemons/fedcba9876543210");
    for (const d of [ws, home, runtime, stale]) mkdirSync(d, { recursive: true });
    // The stand-in broker lives outside $work; its environment, the pane's, carries a daemon pane's
    // mark.
    const broker = join(dir, "fake-broker.sh");
    writeBroker(broker);
    const pane = `setsid -f env OMP_DAEMON_RUNTIME_DIR=${runtime} ${broker} ${runtime} ${home}; exec sleep 600`;

    // A process of the run that no broker started, in a session of its own: the teardown must leave it
    // for cleanup-is-complete's wait to name.
    const bystander = detached(["sleep", "303"], ws, {
      PATH: process.env.PATH ?? "",
      LEGION_E2E_RUN: work,
    });
    // A broker.pid naming a pid whose session holds nothing of the run: a pid the kernel gave to
    // another session leader after the broker ended.
    const stranger = detached(["sleep", "304"], dir, { PATH: process.env.PATH ?? "" });
    started.push(bystander, stranger);
    writeFileSync(join(stale, "broker.pid"), JSON.stringify({ pid: stranger, domain: "test" }));
    await until(
      "the bystander and the stranger to lead sessions of their own",
      () => [bystander, stranger].every((pid) => alive(pid) && sessionOf(pid) === pid),
      deadline
    );

    // The run's tmux server, and so every pane, carries what a daemon pane carries and no variable of
    // the run's own: the daemon builds a pane's environment from its allow-list, with the XDG
    // directories moved under its state directory, which the run keeps under $work
    // (packages/daemon/internal/runtime/tmux/environment.go). That is the one mark the sweep and
    // cleanup-is-complete's wait find a pane's broker by.
    const tmux = realTmux();
    const env = {
      PATH: process.env.PATH ?? "",
      HOME: home,
      TMUX_TMPDIR: tmuxDirectory(),
      XDG_CONFIG_HOME: join(work, "state", "home", ".config"),
    };
    const pidFile = join(runtime, "broker.pid");
    // The stand-in broker the pane starts leads a session of its own and names it in broker.started
    // and then broker.pid. Ending that session is registered before the pane that starts it, so a
    // wait below that times out before the test has read the pid still leaves no member of the
    // broker's session running (endBroker). Each teardown does nothing once the body has seen the
    // thing it ends gone, so a passing row's hook spawns no tmux and signals nothing.
    let paneMade = false;
    let brokerEnded = false;
    let serverKilled = false;
    teardowns.push(() => (brokerEnded ? undefined : endBroker(runtime, () => paneMade)));
    const made = Bun.spawnSync(
      [tmux, "-L", "teardown", "-f", "/dev/null", "new-session", "-d", "-c", ws, pane],
      { env }
    );
    paneMade = made.exitCode === 0;
    // The server this started holds the pane; it outlives a test body that never reaches its own
    // kill-server, so the kill is registered here, where the server exists.
    teardowns.push(() => {
      if (!serverKilled) Bun.spawnSync([tmux, "-L", "teardown", "kill-server"], { env });
    });
    expect(made.stderr.toString()).toBe("");
    const [server, panePid] = Bun.spawnSync(
      [tmux, "-L", "teardown", "display-message", "-p", "#{pid} #{pane_pid}"],
      { env }
    )
      .stdout.toString()
      .split(" ")
      .map(Number);
    await until("the stand-in broker to write broker.pid", () => existsSync(pidFile), deadline);
    const leader: number = JSON.parse(readFileSync(pidFile, "utf8")).pid;
    await until("the broker and its two members", () => familyOf(leader).length === 3, deadline);
    const members = familyOf(leader);
    // The row's proc root, which the code under test reads below: the tmux server and what runs
    // under it, the broker and its members, the bystander and the stranger, each with every
    // process the kernel lists under it, to the bottom. A process that has already ended is left
    // out. Each process the code judges must be in it, or the code would pass the row by never
    // seeing that process.
    const proc = procRootOf(
      dir,
      [server, leader, bystander, stranger].flatMap((pid) => [pid, ...descendantsOf(pid, true)])
    );
    procRoot = proc;
    expect(
      notInRoot(proc, [
        ["the tmux pane", panePid],
        ...members.map((pid): [string, number] => ["a process of the broker's session", pid]),
        ["the bystander", bystander],
        ["the stranger", stranger],
      ])
    ).toEqual([]);
    // A member no host /proc can hold. It fails the row if a walk escapes this private root.
    const unreal = unrealPid(proc);
    const unrealStat = ["S", "1", String(leader), String(leader), ...Array(15).fill("0"), "1"];
    writeFileSync(join(proc, unreal, "stat"), `${unreal} (sleep) ${unrealStat.join(" ")}\n`);
    writeFileSync(join(proc, unreal, "environ"), `OMP_DAEMON_RUNTIME_DIR=${runtime}\0`);

    // The pane goes, as cleanup-is-complete's kill-server takes it.
    inTime("tmux kill-server");
    Bun.spawnSync([tmux, "-L", "teardown", "kill-server"], { env });
    await until("the pane to end", () => !alive(panePid), deadline);
    serverKilled = true;
    // The red half: the broker's whole session outlives the pane, and run_processes, the check's own
    // reading, still names the members, which carry the run's mark.
    expect(members.every(alive)).toBe(true);
    const before = harness(work, "run_processes || true", { procRoot: proc, deadline })
      .stdout.split("\n")
      .filter(Boolean)
      .map(Number);
    expect(before).toContain(leader);

    const stopped = harness(work, "stop_agent_daemons", { procRoot: proc, deadline });
    expect({ code: stopped.code, stderr: stopped.stderr }).toEqual({ code: 0, stderr: "" });
    expect(stopped.stdout.trim().split(/\s+/)).toContain(unreal);
    await until(
      "every member of the broker's session to end",
      () => !members.some(alive),
      deadline
    );
    brokerEnded = true;
    // What no broker of the run ties to the run is left alone, and the check still names the one that
    // carries the run's mark.
    expect(alive(bystander)).toBe(true);
    expect(alive(stranger)).toBe(true);
    const after = harness(work, "run_processes || true", { procRoot: proc, deadline })
      .stdout.split("\n")
      .filter(Boolean)
      .map(Number);
    expect(after).toEqual([bystander]);
  }
);

// A broker that has not yet written broker.pid when its row ends is found by broker.started, the
// first act of the stand-in, and the teardown ends its whole session from that file alone. The
// stand-in holds on a FIFO no writer opens before it writes broker.pid, so the teardown runs, as a
// row's that failed early does, with broker.started and no broker.pid.
test("a teardown ends a broker that has not written broker.pid, by the pid it wrote first", async () => {
  const work = join(dir, "early-broker");
  const home = join(work, "home");
  const runtime = join(work, "run");
  for (const d of [home, runtime]) mkdirSync(d, { recursive: true });
  const hold = join(work, "hold");
  const made = Bun.spawnSync(["mkfifo", hold]);
  expect(made.exitCode).toBe(0);
  const broker = join(work, "fake-broker.sh");
  writeBroker(broker);
  let brokerStarted = false;
  teardowns.push(() => endBroker(runtime, () => brokerStarted));
  const setsid = Bun.spawnSync(["setsid", "-f", broker, runtime, home, hold], {
    env: { PATH: process.env.PATH ?? "" },
    stdout: "ignore",
    stderr: "ignore",
  });
  brokerStarted = setsid.exitCode === 0;
  expect(setsid.exitCode).toBe(0);
  const startedFile = join(runtime, "broker.started");
  await until("the broker to name itself in broker.started", () => existsSync(startedFile));
  const leader = Number(readFileSync(startedFile, "utf8").trim());
  await until("the broker and its two members", () => familyOf(leader).length === 3);
  const members = familyOf(leader);
  expect(existsSync(join(runtime, "broker.pid"))).toBe(false);
  await endBroker(runtime, () => brokerStarted);
  await until("every member of the broker's session to end", () => !members.some(alive));
});

// The walk reads every process's session from its whole /proc/<pid>/stat: a process names itself,
// and a name that holds a newline cut a first-line read short of the session field, so a broker so
// named was tied to no broker.pid and outlived the run. The walk reads a proc root of the broker
// alone (lib/proc-root.ts), so the row's time is its own and not the host's process count.
jq.test("stops a broker's session whose leader's name holds a newline and a `) `", async () => {
  requireHostTools("python3");
  const work = join(dir, "newline-broker");
  const runtime = join(work, "state", "omp-daemon");
  mkdirSync(runtime, { recursive: true });
  const pidFile = join(runtime, "broker.pid");
  // The stand-in leads a session of its own, renames itself to `) S 1` and a newline (prctl
  // PR_SET_NAME), and only then writes broker.pid, so the walk starts after the rename.
  const broker = Bun.spawn(
    [
      "python3",
      "-I",
      "-c",
      'import ctypes, json, os, sys, time; os.setsid(); ctypes.CDLL(None).prctl(15, b") S 1\\n", 0, 0, 0); open(sys.argv[1] + ".tmp", "w").write(json.dumps({"pid": os.getpid()})); os.rename(sys.argv[1] + ".tmp", sys.argv[1]); time.sleep(60)',
      pidFile,
    ],
    {
      env: { PATH: process.env.PATH ?? "", OMP_DAEMON_RUNTIME_DIR: runtime },
      stdout: "ignore",
      stderr: "ignore",
    }
  );
  teardowns.push(() => {
    if (alive(broker.pid) && sessionOf(broker.pid) === broker.pid) killSession(broker.pid);
  });
  await until("the stand-in broker to write broker.pid", () => existsSync(pidFile));
  expect(readFileSync(`/proc/${broker.pid}/comm`, "utf8")).toBe(") S 1\n\n");
  const stopped = harness(work, "stop_agent_daemons", {
    procRoot: procRootOf(dir, [broker.pid]),
  });
  expect({ code: stopped.code, stderr: stopped.stderr }).toEqual({ code: 0, stderr: "" });
  await until("the broker to end", () => !alive(broker.pid));
});

// stop_agent_daemons reads each broker.pid with jq. Without jq it read no file, stopped nothing and
// exited 0, which its caller takes for a run that left no broker session; it fails with 2 instead,
// naming jq, and signals nothing. The stand-in broker is a sleep leading a session of its own with
// the runtime directory in its environment, and the same call with jq on PATH then ends it, so the
// refusal is the missing jq's and not a broker the walk could not have stopped. That second call
// needs the host's jq, so the row is gated on jq as the newline row is.
jq.test(
  "stop_agent_daemons on a PATH without jq fails with 2 naming jq and signals nothing",
  async () => {
    const work = join(dir, "no-jq");
    const runtime = join(work, "state", "omp-daemon");
    mkdirSync(runtime, { recursive: true });
    const broker = detached(["sleep", "305"], work, {
      PATH: process.env.PATH ?? "",
      OMP_DAEMON_RUNTIME_DIR: runtime,
    });
    started.push(broker);
    await until(
      "the stand-in broker to lead a session of its own",
      () => alive(broker) && sessionOf(broker) === broker
    );
    writeFileSync(join(runtime, "broker.pid"), JSON.stringify({ pid: broker, domain: "test" }));
    const procRoot = procRootOf(dir, [broker]);
    const refused = harness(work, "stop_agent_daemons", { procRoot, path: pathWithout("jq") });
    expect(refused).toEqual({
      code: 2,
      stdout: "",
      stderr:
        "stop_agent_daemons: jq is not on PATH, so no broker.pid was read and no broker session was stopped\n",
    });
    expect(alive(broker)).toBe(true);
    const stopped = harness(work, "stop_agent_daemons", { procRoot });
    expect({ code: stopped.code, stderr: stopped.stderr }).toEqual({ code: 0, stderr: "" });
    await until("the broker to end", () => !alive(broker));
  }
);
