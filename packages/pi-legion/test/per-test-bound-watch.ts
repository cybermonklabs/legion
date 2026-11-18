import { readdirSync, readFileSync } from "node:fs";
import { createInterface } from "node:readline";

/**
 * The watch `per-test-bound.ts` starts beside `bun test`: a process of its own, so a test that
 * blocks the thread Bun's own timers run on cannot hold it too.
 *
 * Arguments: the pid of `bun test` and the bound in ms. On its standard input, one event a line:
 * `file\t<path>` before the first test of a file begins, `start\t<place>` and `end\t<place>` when
 * that file's test at that place begins and ends. Each event restarts one timer of the bound. When the timer fires
 * and `bun test` is still the process it was, the watch writes what was running, kills every
 * process under `bun test` and then `bun test` itself, and exits 1. When its input closes, the run
 * is over: it writes the slowest test it saw and exits.
 */

const [pidArgument = "", boundArgument = ""] = process.argv.slice(2);
const bunPid = Number(pidArgument);
const boundMs = Number(boundArgument);
if (!Number.isSafeInteger(bunPid) || !Number.isSafeInteger(boundMs)) {
  throw new Error(
    `usage: per-test-bound-watch.ts <bun test pid> <bound ms>, not ${process.argv.slice(2).join(" ")}`
  );
}

/** Field `index` (1-based, as proc(5) numbers them) of `/proc/<pid>/stat`, or undefined when the
 * process is gone or there is no /proc. The command name, field 2, may hold spaces and
 * parentheses, so the fields after it are read from its last `)`. */
function statField(pid: number, index: number): string | undefined {
  let stat: string;
  try {
    stat = readFileSync(`/proc/${pid}/stat`, "utf8");
  } catch {
    return undefined;
  }
  return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[index - 3];
}

/** `bun test`'s start time (field 22), so a pid the kernel gave to another process since is not
 * taken for it. */
const bunStart = statField(bunPid, 22);

/** Every process under `pid`, parents before their children, with its command line. */
function descendants(pid: number): { pid: number; command: string }[] {
  let entries: string[];
  try {
    entries = readdirSync("/proc").filter((entry) => /^\d+$/.test(entry));
  } catch {
    return [];
  }
  const children = new Map<number, number[]>();
  for (const entry of entries) {
    const parent = Number(statField(Number(entry), 4));
    children.set(parent, [...(children.get(parent) ?? []), Number(entry)]);
  }
  const found: { pid: number; command: string }[] = [];
  const queue = [...(children.get(pid) ?? [])];
  for (let next = queue.shift(); next !== undefined; next = queue.shift()) {
    if (next === process.pid) continue;
    let command = "";
    try {
      command = readFileSync(`/proc/${next}/cmdline`, "utf8").replaceAll("\0", " ").trim();
    } catch {
      continue;
    }
    found.push({
      pid: next,
      command: command.length > 200 ? `${command.slice(0, 200)}…` : command,
    });
    queue.push(...(children.get(next) ?? []));
  }
  return found;
}

/** Where the run is: the file, the place of the last test that began in it, whether that test is
 * still running, and when the last event came, by this process's own clock. */
let file = "";
let place = 0;
let running = false;
let since = performance.now();
let slowest = { file: "", place: 0, ms: -1 };

const seconds = (ms: number) => `${Math.round(ms / 1000)} s`;

function fire(): void {
  if (statField(bunPid, 22) !== bunStart) process.exit(0);
  const waited = performance.now() - since;
  const what = running
    ? `${file}: test ${place} of the file (counting from 1 the tests that ran in it) is still running ${seconds(waited)} after it began`
    : place > 0
      ? `${file}: no test began ${seconds(waited)} after test ${place} of the file ended (its hooks, or the next file's module body and beforeAll)`
      : `no test began ${seconds(waited)} after bun test started (the first file's module body and beforeAll)`;
  const under = descendants(bunPid);
  const lines = [
    `${process.env.GITHUB_ACTIONS === "true" ? "::error title=per-test bound::" : "error: "}${what}; the per-test bound is ${seconds(boundMs)} (test/per-test-bound.ts), and bun test is killed.`,
    `The processes under bun test (pid ${bunPid}):`,
    ...(under.length === 0 ? ["  none"] : under.map(({ pid, command }) => `  ${pid} ${command}`)),
  ];
  process.stderr.write(`${lines.join("\n")}\n`);
  for (const { pid } of under.reverse()) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
  try {
    process.kill(bunPid, "SIGKILL");
  } catch {}
  process.exit(1);
}

let timer = setTimeout(fire, boundMs);

const input = createInterface({ input: process.stdin });
input.on("line", (line) => {
  const now = performance.now();
  const [event = "", value = ""] = line.split("\t");
  if (event === "file") {
    file = value;
    place = 0;
    running = false;
  } else if (event === "start") {
    place = Number(value);
    running = true;
  } else if (event === "end") {
    const ms = now - since;
    if (ms > slowest.ms) slowest = { file, place, ms };
    running = false;
  }
  since = now;
  clearTimeout(timer);
  timer = setTimeout(fire, boundMs);
});
input.on("close", () => {
  clearTimeout(timer);
  if (slowest.ms >= 0) {
    process.stderr.write(
      `per-test bound: the slowest test was test ${slowest.place} of ${slowest.file}, ${seconds(slowest.ms)} of the ${seconds(boundMs)} bound\n`
    );
  }
});
