import { afterAll, expect } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { hostGate, requireHostTools } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { procRootOf } from "./proc-root";
import { libRowBoundMs, rowWaits, untilTrue } from "./row-waits";
import { scriptFunctions } from "./script-functions";

// stop_agent_daemons (kept-decision-devbox.sh), the teardown of what the run's Oh My Pi sessions
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
const scriptPath = join(e2e, "kept-decision-devbox.sh");
const fn = scriptFunctions(scriptPath);
const set = readFileSync(scriptPath, "utf8").match(/^set -.*$/m)?.[0];
if (set === undefined) throw new Error(`${scriptPath} has no top-level set line`);

// Read once, at module scope: a call inside the test body would print the warning per row.
const host = hostGate({ programs: ["tmux", "jq"] });
const jq = hostGate({ programs: ["jq"] });

const dir = temporaryDirectory("kept-decision-teardown-test");
const started: number[] = [];
// What the test started that no pid of its own names: a tmux server to kill, a session a process
// writes its own pid file for. Each is registered the moment the test starts it, before the waits
// that can time out, so an unfinished test body still leaves nothing of the run alive.
const teardowns: (() => void)[] = [];
// The bound of the teardown hook: a hang guard, not a verdict. A teardown that
// ends a broker's session, like the stop the row waits on, walks every process of the host, and how
// long that takes is the machine's (two seconds over 17,000 processes at load ~830, past Bun's 5 s
// hook default on a busier box). #4318 adds this constant to @legion/test-fixtures as
// hang-guard.ts; once it merges, this local copy gives way to that import.
const HANG_GUARD_MS = 120_000;
afterAll(() => {
  for (const teardown of teardowns.reverse()) {
    try {
      teardown();
    } catch {}
  }
  for (const pid of started) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
}, HANG_GUARD_MS);
// The row's tmux server binds its socket at $TMUX_TMPDIR/tmux-<uid>/<label>, and the kernel refuses
// a unix socket path past 107 bytes (sun_path's 108 with its NUL): under the run's own scratch
// directory inside a caller's deep TMPDIR it passed that and tmux refused the server ("error
// connecting to …: Filename too long"). So the sockets go in a short directory of their own under
// /tmp, as the live proofs' do (codex-host.sh's `mktemp -d /tmp/ncs-codex-tmux.XXXXXX`). Its
// removal is registered after the teardowns above, so the server's kill-server reaches its socket
// before the directory goes (Bun runs a scope's afterAll hooks in the order they were registered).
const tmuxDir = temporaryDirectory("kdt-tmux", { root: "/tmp" });

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
const membersOf = (sid: number) =>
  [...new Bun.Glob("[0-9]*").scanSync({ cwd: "/proc", onlyFiles: false })]
    .map(Number)
    .filter((pid) => {
      try {
        return sessionOf(pid) === sid && alive(pid);
      } catch {
        return false;
      }
    });
// Every live pid whose command line holds TEXT: how a teardown finds a process the test started
// in a session of its own before that process has named itself in a file.
const namingPids = (text: string) =>
  [...new Bun.Glob("[0-9]*").scanSync({ cwd: "/proc", onlyFiles: false })]
    .map(Number)
    .filter((pid) => {
      try {
        return readFileSync(`/proc/${pid}/cmdline`, "utf8").includes(text) && alive(pid);
      } catch {
        return false;
      }
    });
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
    // The row decides on the broker session's stop, bounded only by the row's own bound: the stop
    // walks every process of the host and how long it takes is the machine's (a run at load ~1,000
    // passed main's 30 s). Its waits end at the end of the row's waits (waits.end(), 20 s before the
    // file's row bound), each harness run is ended at that deadline (harness's DEADLINE), and the
    // tmux step checks the time first, so a row out of time fails by the name of what it was doing,
    // inside its own time, and Bun's own timeout, which would end the row's children and leave the
    // body running on to an unhandled expect, does not come first.
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
    writeFileSync(
      broker,
      `#!/usr/bin/env bash
runtime=$1 home=$2
(cd / && exec sleep 300) &
(cd "$home" && exec sleep 301) &
printf '{"pid":%s,"domain":"test"}\\n' "$$" >"$runtime/broker.pid.next"
mv "$runtime/broker.pid.next" "$runtime/broker.pid"
wait
`,
      { mode: 0o755 }
    );
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
    const env = {
      PATH: process.env.PATH ?? "",
      HOME: home,
      TMUX_TMPDIR: tmuxDir,
      XDG_CONFIG_HOME: join(work, "state", "home", ".config"),
    };
    const pidFile = join(runtime, "broker.pid");
    // The stand-in broker the pane starts leads a session of its own and names it in broker.pid.
    // Ending that session is registered before the pane that starts it, so a wait below that times
    // out before the test has read the pid still leaves no member of the broker's session running.
    // Before the file is there the broker's command line, which names the run's runtime directory,
    // is the only hold on it, `setsid -f` having already taken it out of the pane's session.
    // Each teardown does nothing once the body has seen the thing it ends gone, so a passing row's
    // hook walks no process and spawns no tmux: the walk is for a body that stopped short of it.
    let brokerEnded = false;
    let serverKilled = false;
    teardowns.push(() => {
      if (brokerEnded) return;
      const leaders = existsSync(pidFile)
        ? [Number(JSON.parse(readFileSync(pidFile, "utf8")).pid)]
        : namingPids(runtime);
      for (const pid of leaders) {
        try {
          killSession(sessionOf(pid));
        } catch {}
      }
    });
    const made = Bun.spawnSync(
      ["tmux", "-L", "teardown", "-f", "/dev/null", "new-session", "-d", "-c", ws, pane],
      { env }
    );
    // The server this started holds the pane; it outlives a test body that never reaches its own
    // kill-server, so the kill is registered here, where the server exists.
    teardowns.push(() => {
      if (!serverKilled) Bun.spawnSync(["tmux", "-L", "teardown", "kill-server"], { env });
    });
    expect(made.stderr.toString()).toBe("");
    const panePid = Number(
      Bun.spawnSync(["tmux", "-L", "teardown", "display-message", "-p", "#{pane_pid}"], {
        env,
      }).stdout.toString()
    );
    await until("the stand-in broker to write broker.pid", () => existsSync(pidFile), deadline);
    const leader: number = JSON.parse(readFileSync(pidFile, "utf8")).pid;
    await until("the broker and its two members", () => membersOf(leader).length === 3, deadline);
    const members = membersOf(leader);

    // The pane goes, as cleanup-is-complete's kill-server takes it.
    inTime("tmux kill-server");
    Bun.spawnSync(["tmux", "-L", "teardown", "kill-server"], { env });
    await until("the pane to end", () => !alive(panePid), deadline);
    serverKilled = true;
    // The red half: the broker's whole session outlives the pane, and run_processes, the check's own
    // reading, still names the members, which carry the run's mark.
    expect(members.every(alive)).toBe(true);
    const before = harness(work, "run_processes || true", { deadline })
      .stdout.split("\n")
      .filter(Boolean)
      .map(Number);
    expect(before).toContain(leader);

    const stopped = harness(work, "stop_agent_daemons", { deadline });
    expect({ code: stopped.code, stderr: stopped.stderr }).toEqual({ code: 0, stderr: "" });
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
    const after = harness(work, "run_processes || true", { deadline })
      .stdout.split("\n")
      .filter(Boolean)
      .map(Number);
    expect(after).toEqual([bystander]);
  }
);

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
