import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  readlinkSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { socketPathRefusal, socketRoot } from "@legion/test-fixtures/socket-root";
import {
  temporaryDirectories,
  temporaryDirectory,
} from "@legion/test-fixtures/temporary-directories";
import { descendantsOf, notInRoot, procRootOf, unrealPid } from "./proc-root";
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
// test is about is tmux kill-server's, so a host with no tmux runs nothing here and says what is
// missing. CI is not such a host: its `test` job runs `bun test scripts/e2e/lib` before its
// `Install tmux` step (.github/workflows/ci.yaml), and the row runs there anyway, because the
// ubuntu-24.04 image the job runs on already carries tmux.
const e2e = join(import.meta.dir, "..");
const root = join(e2e, "..", "..");
const scriptPath = join(e2e, "parked-decision-devbox.sh");
const fn = scriptFunctions(scriptPath);
const set = readFileSync(scriptPath, "utf8").match(/^set -.*$/m)?.[0];
if (set === undefined) throw new Error(`${scriptPath} has no top-level set line`);

// Read once, at module scope: a call inside the test body would print the warning per row.
const skip = skipWithout("tmux");

// stop_agent_daemons (parked-decision-devbox.sh) reads a stat and an environment for every process
// of `${proc_root:-/proc}`, and the row hands it a proc root of its own processes (lib/proc-root.ts),
// so its walk reads those alone. What is left of the row's work is a few process starts (a tmux
// server and pane, the stand-in broker, three shells running the code under test) and waits on its
// processes' events. 30 s is the file's default, set above every hook, since Bun 1.4.4 gives a hook
// the default in force where it is registered, and the hooks end what the row started.
setDefaultTimeout(30_000);

const dir = temporaryDirectory("parked-decision-teardown-test");
const started: number[] = [];
// What the test started that no pid of its own names: a tmux server to kill, a session a process
// writes its own pid file for. Each is registered the moment the test starts it, before the waits
// that can time out, so an unfinished test body still leaves nothing of the run alive. The hook is
// an afterEach registered above the tmux server's directory below: Bun 1.4.4 runs a scope's
// afterEach hooks in the order they were registered, a timed-out row's too, and all of them before
// an afterAll, so the server is stopped through its socket before that directory or `dir` goes.
const teardowns: (() => void)[] = [];
// The row's proc root once it has made one, which the teardown reads too.
let procRoot: string | undefined;
afterEach(() => {
  for (const teardown of teardowns.splice(0).reverse()) {
    try {
      teardown();
    } catch {}
  }
  for (const pid of started.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
  procRoot = undefined;
});

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

test.skipIf(skip)("resolves tmux at the socket-path boundary the teardown row budgets", () => {
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
// Every live pid whose command line holds TEXT: how a teardown finds a process the test started
// in a session of its own before that process has named itself in a file. It scans the host's
// /proc, and runs only for a row that ended before the stand-in broker wrote its pid file: the
// broker is then in no file and, `setsid -f` having handed it to another parent, under no process
// of the row's.
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
// until polls real processes, which no fake clock moves, for the condition it names.
async function until(what: string, ok: () => boolean) {
  for (let i = 0; i < 100; i++) {
    if (ok()) return;
    await Bun.sleep(100);
  }
  throw new Error(`timed out waiting for ${what}`);
}

// harness runs BODY with the script's `set` line, lib/rig.sh sourced as the script sources it (for
// run_processes) and stop_agent_daemons taken from the script by name, its run marked as the script
// marks it (LEGION_E2E_RUN). $work comes in through the environment, and the mark is exported only
// once the shell runs, so the harness's own process carries no mark and run_processes does not name
// it. PROCROOT, when given, is the proc root the walks read (`proc_root`, lib/proc-root.ts).
function harness(work: string, body: string, procRoot?: string) {
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
        PATH: process.env.PATH ?? "",
        WORK: work,
        ...(procRoot === undefined ? {} : { proc_root: procRoot }),
      },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  return { code: ran.exitCode, stdout: ran.stdout.toString(), stderr: ran.stderr.toString() };
}

test.skipIf(skip)(
  "stops a detached broker's session that outlives tmux kill-server, and nothing it cannot tie to one",
  async () => {
    const work = join(dir, "work");
    const ws = join(work, "ws");
    const home = join(work, "home");
    const runtime = join(home, ".local/state/omp/profiles/p/run/daemons/0123456789abcdef");
    const stale = join(work, "state/omp/profiles/p/run/daemons/fedcba9876543210");
    for (const d of [ws, home, runtime, stale, join(work, "tmux")])
      mkdirSync(d, { recursive: true });
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
    const bystander = Bun.spawn(["setsid", "sleep", "303"], {
      cwd: ws,
      env: { PATH: process.env.PATH ?? "", LEGION_E2E_RUN: work },
      stdout: "ignore",
      stderr: "ignore",
    });
    // A broker.pid naming a pid whose session holds nothing of the run: a pid the kernel gave to
    // another session leader after the broker ended.
    const stranger = Bun.spawn(["setsid", "sleep", "304"], {
      cwd: dir,
      stdout: "ignore",
      stderr: "ignore",
    });
    started.push(bystander.pid, stranger.pid);
    writeFileSync(join(stale, "broker.pid"), JSON.stringify({ pid: stranger.pid, domain: "test" }));
    await until("the bystander and the stranger to lead sessions of their own", () =>
      [bystander.pid, stranger.pid].every((pid) => alive(pid) && sessionOf(pid) === pid)
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
    // The stand-in broker the pane starts leads a session of its own and names it in broker.pid.
    // Ending that session is registered before the pane that starts it, so a wait below that times
    // out before the test has read the pid still leaves no member of the broker's session running.
    // A broker leads its session, so the pid in its file is the session's id even once the broker
    // has ended, when only the row's proc root still holds the members it left. Before the file is
    // there the broker's command line, which names the run's runtime directory, is the only hold on
    // it, `setsid -f` having already taken it out of the pane's session.
    teardowns.push(() => {
      const sessions = existsSync(pidFile)
        ? [Number(JSON.parse(readFileSync(pidFile, "utf8")).pid)]
        : namingPids(runtime).flatMap((pid) => {
            try {
              return [sessionOf(pid)];
            } catch {
              return [];
            }
          });
      for (const sid of sessions) {
        try {
          killSession(sid);
        } catch {}
      }
    });
    const made = Bun.spawnSync(
      [tmux, "-L", "teardown", "-f", "/dev/null", "new-session", "-d", "-c", ws, pane],
      { env }
    );
    // The server this started holds the pane; it outlives a test body that never reaches its own
    // kill-server, so the kill is registered here, where the server exists.
    teardowns.push(() => {
      Bun.spawnSync([tmux, "-L", "teardown", "kill-server"], { env });
    });
    expect(made.stderr.toString()).toBe("");
    const panePid = Number(
      Bun.spawnSync([tmux, "-L", "teardown", "display-message", "-p", "#{pane_pid}"], {
        env,
      }).stdout.toString()
    );
    await until("the stand-in broker to write broker.pid", () => existsSync(pidFile));
    const leader: number = JSON.parse(readFileSync(pidFile, "utf8")).pid;
    await until("the broker and its two members", () => familyOf(leader).length === 3);
    const members = familyOf(leader);
    const proc = procRootOf(dir, [leader, ...members, bystander.pid, stranger.pid]);
    procRoot = proc;
    expect(
      notInRoot(proc, [
        ...members.map((pid): [string, number] => ["a process of the broker's session", pid]),
        ["the bystander", bystander.pid],
        ["the stranger", stranger.pid],
      ])
    ).toEqual([]);
    // A member no host /proc can hold. It fails the row if a walk escapes this private root.
    const unreal = unrealPid(proc);
    const unrealStat = ["S", "1", String(leader), String(leader), ...Array(15).fill("0"), "1"];
    writeFileSync(join(proc, unreal, "stat"), `${unreal} (sleep) ${unrealStat.join(" ")}\n`);
    writeFileSync(join(proc, unreal, "environ"), `OMP_DAEMON_RUNTIME_DIR=${runtime}\0`);

    // The pane goes, as cleanup-is-complete's kill-server takes it.
    Bun.spawnSync([tmux, "-L", "teardown", "kill-server"], { env });
    await until("the pane to end", () => !alive(panePid));
    // The red half: the broker's whole session outlives the pane, and run_processes, the check's own
    // reading, still names the members, which carry the run's mark.
    expect(members.every(alive)).toBe(true);
    const before = harness(work, "run_processes || true", proc)
      .stdout.split("\n")
      .filter(Boolean)
      .map(Number);
    expect(before).toContain(leader);

    const stopped = harness(work, "stop_agent_daemons", proc);
    expect({ code: stopped.code, stderr: stopped.stderr }).toEqual({ code: 0, stderr: "" });
    expect(stopped.stdout.trim().split(/\s+/)).toContain(unreal);
    await until("every member of the broker's session to end", () => !members.some(alive));
    // What no broker of the run ties to the run is left alone, and the check still names the one that
    // carries the run's mark.
    expect(alive(bystander.pid)).toBe(true);
    expect(alive(stranger.pid)).toBe(true);
    const after = harness(work, "run_processes || true", proc)
      .stdout.split("\n")
      .filter(Boolean)
      .map(Number);
    expect(after).toEqual([bystander.pid]);
  },
  30_000
);

// The walk reads every process's session from its whole /proc/<pid>/stat: a process names itself,
// and a name that holds a newline cut a first-line read short of the session field, so a broker so
// named was tied to no broker.pid and outlived the run. The walk reads a proc root of the broker
// alone (lib/proc-root.ts), so the row's time is its own and not the host's process count.
test.skipIf(skipWithout("python3"))(
  "stops a broker's session whose leader's name holds a newline and a `) `",
  async () => {
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
    const stopped = harness(work, "stop_agent_daemons", procRootOf(dir, [broker.pid]));
    expect({ code: stopped.code, stderr: stopped.stderr }).toEqual({ code: 0, stderr: "" });
    await until("the broker to end", () => !alive(broker.pid));
  },
  30_000
);
