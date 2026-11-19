import { spawn } from "node:child_process";
import { readdirSync, readFileSync, writeSync } from "node:fs";
import path from "node:path";

/**
 * The per-test bound of this suite: `bun run test` runs this script, which runs `bun test` with
 * the given arguments as its child and holds every test to {@link PER_TEST_BOUND_MS} by its own
 * clock.
 *
 * Bun's own per-test timeout is a timer on the thread that runs the test, so it fires only while
 * that thread is free. Measured on Bun 1.4.4: a test blocked in a synchronous read of a FIFO, in
 * `Atomics.wait` or in a loop, and a test file whose module body awaits a promise that never
 * settles, run until something outside kills them, and a `bun test` that is sent SIGINT or SIGTERM
 * prints nothing of the test it was running. Where Bun's timer does fire, a test's own timeout
 * argument wins over `--timeout`, and this suite gives its real-Oh-My-Pi tests up to 600 s each. A
 * test that never ends in CI therefore holds the step until the job's own limit cancels the job:
 * nothing in the log fails, and a job cancelled at its limit can lose its log altogether.
 *
 * `per-test-bound-preload.ts`, which this script passes to `bun test` with `--preload`, writes a
 * line to this script for each test, on a pipe at the child's file descriptor 3: `start`, with the
 * time and the test's file, before the test's own `beforeEach` hooks, and `end`, with the time,
 * after its own `afterEach` hooks. When a test is still running the bound after it began, or no
 * test begins the bound after the last one ended, this script writes the file, the test's place in
 * it and the processes of the run, kills them and `bun test`, and exits 1. A test, or a wait for
 * the next test, that ended past the bound before this script could act fails the run too.
 *
 * Every process of the run carries {@link RUN_VARIABLE} in its environment, unless a test gave the
 * process an environment of its own. A process of the run is one under `bun test` by its parents
 * or one that carries the variable, which finds a process whose parent has exited. When `bun test`
 * exits, any process of the run still running is named and killed, since it can hold the step's
 * output open. Before it exits, this script writes the slowest test and the longest wait for a
 * test to begin, so a run's own log says how near the bound it came.
 */

/**
 * Twice the slowest correct test, taken up to 80 s from its 77.4 s. The slowest is
 * `extensions/legion-stall-watchdog-omp.test.ts`'s third, "a call the abort does not end", which
 * took 73.2 s to 77.4 s in the pi-legion CI job's own runs of this suite at nine commits of
 * 2026-11-18; the next is the same file's eighth, at 41.4 s. The longest wait between one test's
 * end and the next one's start in those runs was 0.83 s. With the suite's own 12 to 14 minutes, a
 * run that one test holds up ends inside the job's 28-minute limit, failed and with its log.
 */
const PER_TEST_BOUND_MS = 160_000;

/** The time the bound is given to read the events already sent before it is judged expired: a
 * timer whose time has come can run before the pipe's pending lines are read. */
const GRACE_MS = 1_000;

/** The variable every process of the run inherits, holding this run's own token. */
const RUN_VARIABLE = "LEGION_TEST_BOUND_RUN";

const boundSetting = process.env.LEGION_TEST_BOUND_MS;
const boundMs = boundSetting === undefined ? PER_TEST_BOUND_MS : Number(boundSetting);
if (!Number.isSafeInteger(boundMs) || boundMs <= 0 || boundMs > 2_147_483_647) {
  throw new Error(
    `LEGION_TEST_BOUND_MS must be a whole number of ms from 1 to 2147483647, not "${boundSetting}"`
  );
}

const directory = process.cwd();
const runToken = crypto.randomUUID();

/** Field `index` (1-based, as proc(5) numbers them) of `/proc/<pid>/stat`, or undefined when the
 * process is gone or there is no /proc. The command name, field 2, may hold spaces and
 * parentheses, so the fields after it are read from its last `)`. */
function statField(pid: number, index: number): string | undefined {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
    return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[index - 3];
  } catch {
    return undefined;
  }
}

/** A process of the run: its pid, its start time (field 22), so a pid the kernel gives to another
 * process later is not taken for it, and its command line. */
type RunProcess = { pid: number; start: string; command: string };

/** The processes of the run other than `bun test` (`root`) and this script, read from /proc;
 * where there is none, none. */
function runProcesses(root: number): RunProcess[] {
  let pids: number[];
  try {
    pids = readdirSync("/proc")
      .filter((entry) => /^\d+$/.test(entry))
      .map(Number);
  } catch {
    return [];
  }
  const children = new Map<number, number[]>();
  for (const pid of pids) {
    const parent = Number(statField(pid, 4));
    if (Number.isSafeInteger(parent)) children.set(parent, [...(children.get(parent) ?? []), pid]);
  }
  const found = new Set<number>();
  const walk = (pid: number): void => {
    for (const child of children.get(pid) ?? []) {
      if (found.has(child)) continue;
      found.add(child);
      walk(child);
    }
  };
  walk(root);
  const carried = `${RUN_VARIABLE}=${runToken}`;
  for (const pid of pids) {
    if (found.has(pid) || pid === root || pid === process.pid) continue;
    try {
      if (readFileSync(`/proc/${pid}/environ`, "utf8").split("\0").includes(carried)) {
        found.add(pid);
        walk(pid);
      }
    } catch {}
  }
  found.delete(process.pid);
  return [...found].flatMap((pid) => {
    const start = statField(pid, 22);
    if (start === undefined) return [];
    let command = "";
    try {
      command = readFileSync(`/proc/${pid}/cmdline`, "utf8").replaceAll("\0", " ").trim();
    } catch {}
    return [{ pid, start, command: command.length > 200 ? `${command.slice(0, 200)}…` : command }];
  });
}

/** Reports `processes` under a heading, one line each, then kills each that is still the
 * process listed: the same pid with the same start time. */
function reportAndKill(heading: string, processes: RunProcess[]): string[] {
  const lines = [
    heading,
    ...(processes.length === 0
      ? ["  none"]
      : processes.map(({ pid, command }) => `  ${pid} ${command}`)),
  ];
  for (const { pid, start } of processes) {
    if (statField(pid, 22) !== start) continue;
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
  return lines;
}

const seconds = (ms: number) => `${(ms / 1000).toFixed(1)} s`;

/** An error line: a GitHub annotation, on the test file when there is one, under GitHub Actions;
 * plain otherwise. `file` is relative to the run's directory; an annotation names it from the
 * workspace, the root GitHub reads its paths from. */
function errorLine(file: string | undefined, message: string): string {
  if (process.env.GITHUB_ACTIONS !== "true") return `error: ${message}`;
  const escapeData = (text: string) =>
    text.replaceAll("%", "%25").replaceAll("\r", "%0D").replaceAll("\n", "%0A");
  let where = "";
  if (file !== undefined) {
    const fromWorkspace = path.relative(
      process.env.GITHUB_WORKSPACE ?? directory,
      path.resolve(directory, file)
    );
    where = `file=${escapeData(fromWorkspace).replaceAll(":", "%3A").replaceAll(",", "%2C")},`;
  }
  return `::error ${where}title=per-test bound::${escapeData(message)}`;
}

/** Where the run is, from the events read so far: the file and place of the last test that
 * began, whether it is still running, and the time of the last event, by the sender's clock. */
let file: string | undefined;
let place = 0;
let running = false;
let last = Date.now();
let slowestTest: { file: string; place: number; ms: number } | undefined;
let longestWait: { after: string; ms: number } | undefined;
/** Each test, or wait for a test, that ended past the bound before the timer could act. */
const past: { file: string | undefined; what: string }[] = [];

/** What the last event left the run doing, for a report. */
function doing(): string {
  if (file === undefined) return "bun test started (the first file's module body and beforeAll)";
  if (running)
    return `${file}: test ${place} of the file (counting from 1 the tests that ran in it)`;
  return `${file}: test ${place} of the file ended (its hooks, or the next file's module body and beforeAll)`;
}

function onEvent(line: string): void {
  const [event = "", at = "", startedFile = ""] = line.split("\t");
  const time = Number(at);
  if (!Number.isFinite(time) || (event !== "start" && event !== "end")) {
    throw new Error(`per-test bound: an event this script does not read: ${JSON.stringify(line)}`);
  }
  const ms = time - last;
  if (event === "start") {
    if (ms > (longestWait?.ms ?? -1)) longestWait = { after: doing(), ms };
    if (ms > boundMs) past.push({ file, what: `no test began ${seconds(ms)} after ${doing()}` });
    place = startedFile === file ? place + 1 : 1;
    file = startedFile;
    running = true;
  } else if (file !== undefined && running) {
    if (ms > (slowestTest?.ms ?? -1)) slowestTest = { file, place, ms };
    if (ms > boundMs) past.push({ file, what: `${doing()} ran ${seconds(ms)}` });
    running = false;
  }
  last = time;
}

// This script is the test command's main process, so its exit status is the step's: a child that
// cannot start fails the run here.
const child = spawn(
  process.execPath,
  [
    "test",
    "--preload",
    path.join(import.meta.dir, "per-test-bound-preload.ts"),
    ...process.argv.slice(2),
  ],
  {
    stdio: ["inherit", "inherit", "inherit", "pipe"],
    env: { ...process.env, [RUN_VARIABLE]: runToken, LEGION_TEST_BOUND_FD: "3" },
  }
);
child.on("error", (error) => {
  writeSync(2, `${errorLine(undefined, `bun test could not start: ${error.message}`)}\n`);
  process.exit(1);
});

let fired = false;
let exited = false;

/** Judges the bound once the grace has let the pending events in: a run whose last event is
 * still older than the bound is reported and killed; one that moved on is checked again when its
 * own bound would end. */
function confirm(): void {
  if (fired || exited) return;
  const waited = Date.now() - last;
  if (waited < boundMs) {
    setTimeout(confirm, boundMs - waited);
    return;
  }
  fired = true;
  const message = running
    ? `${doing()} is still running ${seconds(waited)} after it began`
    : `no test began ${seconds(waited)} after ${doing()}`;
  // Stopped before anything else, so it cannot finish while the processes are read, and its
  // children stay its children until they are listed: reading /proc can take seconds on a
  // crowded host.
  if (child.pid !== undefined) {
    try {
      process.kill(child.pid, "SIGSTOP");
    } catch {}
  }
  const processes = child.pid === undefined ? [] : runProcesses(child.pid);
  child.kill("SIGKILL");
  const lines = [
    errorLine(
      file,
      `${message}; the per-test bound is ${seconds(boundMs)} (test/per-test-bound.ts), and bun test is killed.`
    ),
    ...reportAndKill(`The processes of the run under bun test (pid ${child.pid}):`, processes),
  ];
  writeSync(2, `${lines.join("\n")}\n`);
}

const timer = setTimeout(() => setTimeout(confirm, GRACE_MS), boundMs);

let partial = "";
let eventsEnded = false;
let exitCode: number | null | undefined;
child.stdio[3]?.on("data", (chunk: Buffer) => {
  const lines = (partial + chunk.toString()).split("\n");
  partial = lines.pop() ?? "";
  try {
    for (const line of lines) onEvent(line);
  } catch (error) {
    // An event this script cannot read leaves no bound it can hold: the run fails here, with
    // bun test ended rather than left running unbounded.
    child.kill("SIGKILL");
    writeSync(
      2,
      `${errorLine(undefined, error instanceof Error ? error.message : String(error))}\n`
    );
    process.exit(1);
  }
  timer.refresh();
});

// The run is accounted for once bun test has exited and its last events are read: the exit can be
// reported before the pipe's last lines are. A pipe that a process of the run keeps open is given
// a few seconds.
child.stdio[3]?.on("end", () => {
  eventsEnded = true;
  if (exitCode !== undefined) finish();
});
child.on("exit", (code) => {
  exited = true;
  exitCode = code;
  clearTimeout(timer);
  if (eventsEnded) finish();
  else setTimeout(finish, 5_000);
});

let finished = false;
function finish(): void {
  if (finished) return;
  finished = true;
  const lines: string[] = [];
  const left = child.pid === undefined ? [] : runProcesses(child.pid);
  if (left.length > 0) {
    lines.push(
      ...reportAndKill(
        errorLine(
          undefined,
          "processes of the run still running after bun test exited, now killed:"
        ),
        left
      )
    );
  }
  for (const { file: pastFile, what } of past) {
    lines.push(
      errorLine(
        pastFile,
        `${what}, past the per-test bound of ${seconds(boundMs)} (test/per-test-bound.ts).`
      )
    );
  }
  const slowest =
    slowestTest === undefined
      ? "none"
      : `test ${slowestTest.place} of ${slowestTest.file}, ${seconds(slowestTest.ms)}`;
  const wait =
    longestWait === undefined ? "none" : `${seconds(longestWait.ms)}, after ${longestWait.after}`;
  lines.push(
    `per-test bound: the slowest test was ${slowest}; the longest wait for a test to begin was ${wait}; the bound is ${seconds(boundMs)}.`
  );
  writeSync(2, `${lines.join("\n")}\n`);
  process.exit(fired || past.length > 0 ? 1 : (exitCode ?? 1));
}
