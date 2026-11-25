import { afterEach, beforeEach, expect, test } from "bun:test";
import { realpathSync } from "node:fs";
import { hostGate, hostTool } from "@legion/test-fixtures/host-tools";
import {
  libRowBoundMs,
  rowWaitMarginMs,
  rowWaits,
  stateOf,
  untilGone,
  untilTrue,
} from "./row-waits";

// The waits run against the real clock, since what they bound in the rows that use them, a process
// starting or ending, is the kernel's: each row here gives them a short end of its own, so the
// failure is read soon.
// bash and sleep: both untilGone rows start a sleep, and the first a bash; a host without them
// skips those rows by name outside CI and fails them by name under GITHUB_ACTIONS (hostGate).
const host = hostGate({ programs: ["bash", "sleep"] });
// Each row's start lies between these two marks, which run just before and just after rowWaits'
// own, in the order they were registered.
let hooksBegan = 0;
let hooksEnded = 0;
beforeEach(() => {
  hooksBegan = performance.now();
});
const waits = rowWaits(libRowBoundMs);
beforeEach(() => {
  hooksEnded = performance.now();
});

const started: number[] = [];
afterEach(() => {
  for (const pid of started.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
});
const failureOf = async (wait: Promise<void>) => {
  try {
    await wait;
  } catch (error) {
    return (error as Error).message.replace(/^waited \d+\.\d s,/, "waited N s,");
  }
  return "no failure";
};

test("rowWaits ends a row's waits its margin before the row's bound, from the row's start, and gives a shell the whole seconds left to that end", () => {
  expect(waits.end()).toBeGreaterThanOrEqual(hooksBegan + libRowBoundMs - rowWaitMarginMs);
  expect(waits.end()).toBeLessThanOrEqual(hooksEnded + libRowBoundMs - rowWaitMarginMs);
  const before = waits.end() - performance.now();
  const seconds = waits.secondsLeft();
  const after = waits.end() - performance.now();
  expect(Number.isInteger(seconds)).toBe(true);
  expect(seconds * 1000).toBeLessThanOrEqual(before);
  expect(seconds * 1000).toBeGreaterThan(after - 1000);
});

test("untilTrue resolves once its look answers true", async () => {
  let looks = 0;
  await untilTrue("the third look", waits.end(), () => ++looks === 3 || `look ${looks}`);
  expect(looks).toBe(3);
});

test("untilTrue fails at the end it is given, not before, naming what it waited for and what it saw", async () => {
  const end = performance.now() + 300;
  let looks = 0;
  const failure = await failureOf(untilTrue("nothing", end, () => `look ${++looks}`));
  expect(performance.now()).toBeGreaterThanOrEqual(end);
  expect(failure).toBe(`waited N s, to the end of the row's waits, for nothing: look ${looks}`);
});

test("untilTrue looks once at an end that has passed: it succeeds on a condition that holds, and fails on one that does not", async () => {
  let looks = 0;
  await untilTrue("a condition that holds", performance.now() - 1, () => {
    looks++;
    return true;
  });
  const failure = await failureOf(
    untilTrue("a condition that does not", performance.now() - 1, () => `look ${++looks}`)
  );
  expect(failure).toBe(
    "waited N s, to the end of the row's waits, for a condition that does not: look 2"
  );
  expect(looks).toBe(2);
});

test("a look that throws ends untilTrue with its error", async () => {
  const failure = await failureOf(
    untilTrue("a look that throws", waits.end(), () => {
      throw new Error("the look's own error");
    })
  );
  expect(failure).toBe("the look's own error");
});

host.test(
  "untilGone resolves for a process that ended and was reaped, and for a zombie",
  async () => {
    const ended = Bun.spawn(["sleep", "60"], { stdout: "ignore", stderr: "ignore" });
    ended.kill("SIGKILL");
    await ended.exited;
    await untilGone(ended.pid, "a sleep that was killed and reaped", waits.end());
    // The child ends only once its parent runs sleep, which never reaps it: one that ended before
    // the exec would be reaped by the parent's bash and never be a zombie.
    const sleep = realpathSync(hostTool("sleep") ?? "sleep");
    const parent = Bun.spawn(
      [
        "bash",
        "-c",
        `bash -c 'end=$((SECONDS + $2)); until [ "$(readlink "/proc/$PPID/exe")" = "$1" ]; do [ "$SECONDS" -lt "$end" ] || exit 1; sleep 0.05; done' child "$1" "$2" </dev/null >/dev/null 2>&1 & echo $!; exec sleep 60`,
        "parent",
        sleep,
        String(waits.secondsLeft()),
      ],
      { stderr: "ignore" }
    );
    started.push(parent.pid);
    // The first line alone: the sleep holds the pipe open for its whole minute.
    const said = await parent.stdout.getReader().read();
    const zombie = Number(new TextDecoder().decode(said.value).trim());
    expect(Number.isSafeInteger(zombie)).toBe(true);
    await untilGone(zombie, "the child no one reaps", waits.end());
    expect(stateOf(zombie)).toBe("Z");
  }
);

host.test(
  "untilGone fails at its end naming the pid, what it is and its state, for a process that runs on",
  async () => {
    const running = Bun.spawn(["sleep", "60"], { stdout: "ignore", stderr: "ignore" });
    started.push(running.pid);
    const failure = await failureOf(
      untilGone(running.pid, "a sleep that runs on", performance.now() + 300)
    );
    expect(failure).toMatch(
      new RegExp(
        `^waited N s, to the end of the row's waits, for pid ${running.pid}, a sleep that runs on, to end: its state is [RS]$`
      )
    );
  }
);

test("untilGone refuses at once a pid that names no process at all, which would read as gone", async () => {
  for (const pid of [Number.NaN, 0, -1]) {
    expect(await failureOf(untilGone(pid, "a pid read from nothing", waits.end()))).toBe(
      `a pid read from nothing: ${pid} is no pid`
    );
  }
});
