import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import {
  temporaryDirectories,
  temporaryDirectory,
} from "@legion/test-fixtures/temporary-directories";
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

// The code under test reads `${proc_root:-/proc}`: run_processes (lib/rig.sh) a cwd link and a
// command line for every process there, stop_agent_daemons (parked-decision-devbox.sh) a stat and
// an environment for every process and run_processes once more. The row runs four such walks
// (run_processes before and after, and stop_agent_daemons's two), so over the host's /proc its work
// grew with the machine's processes, which no test owns: CI's runner has a few hundred, a devbox
// tens of thousands, and with the row held to a ninth of one CPU on a devbox of about 20,000 the
// fixed 30 s it had ran out inside stop_agent_daemons in 20 runs of 20. The row hands both a proc
// root of its own processes instead (ownProc, below), so each walk reads those alone. Its bound is
// then the code's own, with no term the host sets: stop_agent_daemons's wait for its TERM to land,
// read from the function (50 polls 0.2 s apart, 10 s), and the row's own five waits on its
// processes' events, each held to `until`'s 100 polls 0.1 s apart (10 s). The row's other steps (a
// tmux server and pane, the stand-in broker, three shells running the code under test) are a few
// process starts, which those waits' 50 s exceed many times over; no wait is ever meant to use its
// bound. It is set as the file's default above every hook, since Bun 1.4.4 gives a hook the default
// in force where it is registered, and the hooks end what the row started.
const stopAgentDaemons = fn("stop_agent_daemons");
const termWait = /for i in \$\(seq 1 (\d+)\); do[\s\S]*?\bsleep ([\d.]+)\n/.exec(stopAgentDaemons);
if (termWait === null) {
  throw new Error(
    `stop_agent_daemons in ${scriptPath} has no TERM wait of the form \`for i in $(seq 1 N); do … sleep S\``
  );
}
const termWaitMs = Number(termWait[1]) * Number(termWait[2]) * 1000;
const untilPolls = 100;
const untilIntervalMs = 100;
const rowWaits = 5;
setDefaultTimeout(termWaitMs + rowWaits * untilPolls * untilIntervalMs);

const dir = temporaryDirectory("parked-decision-teardown-test");
const started: number[] = [];
// What the test started that no pid of its own names: a tmux server to kill, a session a process
// writes its own pid file for. Each is registered the moment the test starts it, before the waits
// that can time out, so an unfinished test body still leaves nothing of the run alive. The hook is
// an afterEach registered above the tmux server's directory below: Bun 1.4.4 runs a scope's
// afterEach hooks in the order they were registered, a timed-out row's too, and all of them before
// an afterAll, so the server is stopped through its socket before that directory or `dir` goes.
const teardowns: (() => void)[] = [];
// The row's proc root once it has made one (ownProc), which the teardown reads too.
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

// The tmux server's socket is TMUX_TMPDIR/tmux-<uid>/teardown, and a unix socket's path holds at
// most 107 bytes (docs/testing.md, "Unix sockets in Go tests"): under `dir` it came to TMPDIR's
// length plus 68, so a TMPDIR over 39 bytes, such as a devbox's 45-byte one, had tmux refuse it
// (`error connecting to … (Filename too long)`) before the pane existed. Its directory is made
// under TMPDIR when the socket fits there and under /tmp otherwise, which says so, as the Go
// modules' internal/testtmp does. The maker names a directory <prefix>-XXXXXX.
const tmuxPrefix = "parked-decision-teardown-tmux";
const tmuxSocket = join(`tmux-${process.getuid?.() ?? 0}`, "teardown");
const fitsSocket = (root: string) =>
  Buffer.byteLength(join(root, `${tmuxPrefix}-XXXXXX`, tmuxSocket)) <= 107;
const tmuxRoot = fitsSocket(tmpdir()) ? tmpdir() : "/tmp";
if (tmuxRoot !== tmpdir()) {
  console.error(
    `parked-decision-teardown.test.ts: the tmux socket would pass 107 bytes under TMPDIR ${tmpdir()}, so its directory is under /tmp`
  );
}
const tmuxDirectory = temporaryDirectories(tmuxPrefix, { root: tmuxRoot });

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
// kernel's children lists (/proc/<pid>/task/<pid>/children): the row's waits on the broker, which
// a scan of every process on the host would hold to the host's size. The stand-in's members are its
// own children.
const familyOf = (leader: number) => {
  const family: number[] = [];
  const visit = (pid: number) => {
    let children: string[];
    try {
      if (sessionOf(pid) !== leader || !alive(pid)) return;
      children = readFileSync(`/proc/${pid}/task/${pid}/children`, "utf8").split(" ");
    } catch {
      return;
    }
    family.push(pid);
    for (const child of children.filter(Boolean)) visit(Number(child));
  };
  visit(leader);
  return family;
};
// ownProc ANCHORS makes the row's proc root in a fresh directory under `dir`: a link to the real
// /proc/<pid> of each anchor and of every process the kernel lists under it (each thread's
// /proc/<pid>/task/<tid>/children, to the bottom), so the code under test reads those processes' own
// files and signals their real pids, and a process a stand-in started is in view with no scan of
// the host. A process that has already ended is left out.
const ownProc = (anchors: number[]) => {
  const root = mkdtempSync(join(dir, "proc-"));
  const seen = new Set<number>();
  const visit = (pid: number) => {
    if (seen.has(pid)) return;
    seen.add(pid);
    let tasks: string[];
    try {
      tasks = readdirSync(`/proc/${pid}/task`);
    } catch {
      return;
    }
    symlinkSync(`/proc/${pid}`, join(root, String(pid)));
    for (const tid of tasks) {
      let children: string[];
      try {
        children = readFileSync(`/proc/${pid}/task/${tid}/children`, "utf8").split(/\s+/);
      } catch {
        continue; // A thread that ended since the listing has no children to list.
      }
      for (const child of children) if (child !== "") visit(Number(child));
    }
  };
  for (const pid of anchors) visit(pid);
  return root;
};
// notInRoot ROOT NAMED: the processes NAMED ([what, pid]) that ROOT holds no live entry for, each
// as "<what> <pid>". The row reads the code's verdict on a process as a pass only with that
// process in view, so a root without one fails the row, naming it.
const notInRoot = (root: string, named: [string, number][]) =>
  named
    .filter(([, pid]) => !existsSync(join(root, String(pid), "stat")))
    .map(([what, pid]) => `${what} ${pid}`);
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
  for (let i = 0; i < untilPolls; i++) {
    if (ok()) return;
    await Bun.sleep(untilIntervalMs);
  }
  throw new Error(`timed out waiting for ${what}`);
}

// harness runs BODY with the script's `set` line, lib/rig.sh sourced as the script sources it (for
// run_processes) and stop_agent_daemons taken from the script by name, both reading the row's proc
// root PROC. $work comes in through the environment, so the harness's own command line does not
// name it and run_processes does not name the harness.
function harness(work: string, proc: string, body: string) {
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
${stopAgentDaemons}
${body}`,
    ],
    {
      env: { PATH: process.env.PATH ?? "", WORK: work, proc_root: proc },
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
    for (const d of [ws, home, runtime, stale]) mkdirSync(d, { recursive: true });
    // The stand-in broker lives outside $work, so only its working directory ties it to the run.
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

    const env = { PATH: process.env.PATH ?? "", HOME: home, TMUX_TMPDIR: tmuxDirectory() };
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
      ["tmux", "-L", "teardown", "-f", "/dev/null", "new-session", "-d", "-c", ws, pane],
      { env }
    );
    // The server this started holds the pane; it outlives a test body that never reaches its own
    // kill-server, so the kill is registered here, where the server exists.
    teardowns.push(() => {
      Bun.spawnSync(["tmux", "-L", "teardown", "kill-server"], { env });
    });
    expect(made.stderr.toString()).toBe("");
    const [server, panePid] = Bun.spawnSync(
      ["tmux", "-L", "teardown", "display-message", "-p", "#{pid} #{pane_pid}"],
      { env }
    )
      .stdout.toString()
      .split(" ")
      .map(Number);
    await until("the stand-in broker to write broker.pid", () => existsSync(pidFile));
    const leader: number = JSON.parse(readFileSync(pidFile, "utf8")).pid;
    await until("the broker and its two members", () => familyOf(leader).length === 3);
    const members = familyOf(leader);
    // The row's proc root, which the code under test reads below: the tmux server and what runs
    // under it, the broker and its members, the bystander and the stranger. Each process the code
    // judges must be in it, or the code would pass the row by never seeing that process.
    procRoot = ownProc([server, leader, bystander.pid, stranger.pid]);
    const proc = procRoot;
    expect(
      notInRoot(proc, [
        ["the tmux pane", panePid],
        ...members.map((pid): [string, number] => ["a process of the broker's session", pid]),
        ["the bystander", bystander.pid],
        ["the stranger", stranger.pid],
      ])
    ).toEqual([]);

    // The pane goes, as cleanup-is-complete's kill-server takes it.
    Bun.spawnSync(["tmux", "-L", "teardown", "kill-server"], { env });
    await until("the pane to end", () => !alive(panePid));
    // The red half: the broker's whole session outlives the pane, and run_processes, the check's own
    // reading, still names the members that work inside $work.
    expect(members.every(alive)).toBe(true);
    const before = harness(work, proc, "run_processes || true")
      .stdout.split("\n")
      .filter(Boolean)
      .map(Number);
    expect(before).toContain(leader);

    const stopped = harness(work, proc, "stop_agent_daemons");
    expect({ code: stopped.code, stderr: stopped.stderr }).toEqual({ code: 0, stderr: "" });
    await until("every member of the broker's session to end", () => !members.some(alive));
    // What no broker of the run ties to the run is left alone, and the check still names the one that
    // works inside $work.
    expect(alive(bystander.pid)).toBe(true);
    expect(alive(stranger.pid)).toBe(true);
    const after = harness(work, proc, "run_processes || true")
      .stdout.split("\n")
      .filter(Boolean)
      .map(Number);
    expect(after).toEqual([bystander.pid]);
  }
);
