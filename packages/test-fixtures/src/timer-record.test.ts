import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { check } from "recheck";
import { temporaryDirectory } from "./temporary-directories";
import {
  FRAME_PATTERNS,
  placeArming,
  recordTimers,
  type TimerStep,
  timersBetween,
} from "./timer-record";
import { settled, until } from "./wait";

const root = temporaryDirectory("timer-record");
let uninstall: (() => void) | undefined;
afterEach(() => {
  uninstall?.();
  uninstall = undefined;
});

/** A program module under `root`, written for the row, whose timers are the program's own. */
async function programModule(name: string, source: string): Promise<Record<string, unknown>> {
  const file = join(root, name);
  await Bun.write(file, source);
  return (await import(file)) as Record<string, unknown>;
}

test("records the program's timers with the delays it chose, in the order they ran", async () => {
  const steps: TimerStep[] = [];
  uninstall = recordTimers([root], (step) => steps.push(step));
  const program = await programModule(
    "waits.ts",
    [
      "export function later(done: () => void): void { setTimeout(done, 5); }",
      "export function cleared(): void { clearTimeout(setTimeout(() => {}, 60_000)); }",
      "export async function slept(): Promise<void> { await Bun.sleep(3); }",
    ].join("\n")
  );
  const fired = Promise.withResolvers<void>();
  (program["later"] as (done: () => void) => void)(fired.resolve);
  (program["cleared"] as () => void)();
  await (program["slept"] as () => Promise<void>)();
  await fired.promise;
  // The test's own timers are not the program's: the shared wait polls on one of them.
  await until("a poll of the test's own", () => true);
  expect(
    steps.filter((step) => "timer" in step).map((step) => "timer" in step && step.delayMs)
  ).toEqual([5, 60_000, 3]);
  const arming = steps.find((step) => "timer" in step);
  expect(arming !== undefined && "from" in arming ? arming.from : "").toStartWith(
    `${join(root, "waits.ts")}:1`
  );
  expect(
    timersBetween(steps, -1, steps.length)
      .map((timer) => timer.delayMs)
      .sort()
  ).toEqual([3, 5]);
});

test("reads a module loaded with a query after its path as the file it is", async () => {
  // Oh My Pi loads an extension as `file.ts?mtime=…`, and its stack frames name it so.
  const steps: TimerStep[] = [];
  uninstall = recordTimers([root], (step) => steps.push(step));
  const file = join(root, "queried.ts");
  await Bun.write(
    file,
    "export function later(): void { clearTimeout(setTimeout(() => {}, 9)); }\n"
  );
  const program = (await import(`${file}?mtime=1795072600396`)) as { later: () => void };
  program.later();
  expect(steps).toEqual([{ timer: 1, delayMs: 9, from: `${file}:1` }]);
});

test("leaves out the timers of code under no root, a library's or the test's", () => {
  const steps: TimerStep[] = [];
  uninstall = recordTimers([join(root, "elsewhere")], (step) => steps.push(step));
  clearTimeout(setTimeout(() => {}, 10));
  expect(steps).toEqual([]);
});

test("reads between two steps of a record it shares: only timers armed after the first and fired before the second", () => {
  const steps: unknown[] = [
    { timer: 1, delayMs: 1_000, from: "a.ts:1" },
    { reconnected: true },
    { timer: 2, delayMs: 40_000, from: "b.ts:2" },
    { timer: 3, delayMs: 60_000, from: "c.ts:3" },
    { fired: 1 },
    { fired: 2 },
    { shown: "the message" },
    { fired: 3 },
  ];
  expect(timersBetween(steps, 1, 6)).toEqual([{ delayMs: 40_000, from: "b.ts:2" }]);
});

test("reads a checkout under a directory named test as the program's, by its path below the root", async () => {
  const steps: TimerStep[] = [];
  const checkout = join(root, "test", "checkout");
  uninstall = recordTimers([checkout], (step) => steps.push(step));
  const file = join(checkout, "src", "waits.ts");
  await Bun.write(
    file,
    "export function later(): void { clearTimeout(setTimeout(() => {}, 11)); }\n"
  );
  const program = (await import(file)) as { later: () => void };
  program.later();
  expect(steps).toEqual([{ timer: 1, delayMs: 11, from: `${file}:1` }]);
});

test("reads a timer a compiled host armed for the plugin that called it as the plugin's, and the host's own as none", () => {
  const prefixes = ["/work/plugins/"];
  const throughHost = [
    "Error",
    "    at setTimeout (/$bunfs/root/omp:4021:17)",
    "    at <anonymous> (/$bunfs/root/omp:9120:5)",
    "    at deliver (/work/plugins/envoy.ts:1386:7)",
  ].join("\n");
  expect(placeArming(throughHost, prefixes)).toEqual({ from: "/work/plugins/envoy.ts:1386" });
  const hostsOwn = ["Error", "    at render (/$bunfs/root/omp:51:3)", "    at native"].join("\n");
  expect(placeArming(hostsOwn, prefixes)).toBe("foreign");
  const library = ["Error", "    at ping (/work/node_modules/nats/lib/protocol.js:88:5)"].join(
    "\n"
  );
  expect(placeArming(library, prefixes)).toBe("foreign");
});

test("records an arming whose stack names no file as unplaced, and refuses a window that ran one", () => {
  expect(placeArming(["Error", "    at native", "    at <anonymous>"].join("\n"), ["/x/"])).toBe(
    "unplaced"
  );
  const steps: unknown[] = [
    { reconnected: true },
    { unplaced: 1, delayMs: 40_000 },
    { timer: 2, delayMs: 100, from: "b.ts:2" },
    { fired: 2 },
    { shown: "the message" },
    { fired: 1 },
  ];
  // Still pending when the message was shown: it delayed nothing on the way.
  expect(timersBetween(steps, 0, 4)).toEqual([{ delayMs: 100, from: "b.ts:2" }]);
  expect(() => timersBetween(steps, 0, 6)).toThrow(/could not place 1 timer/);
});

test("reads the file and line of each shape of frame", () => {
  const shapes: Record<string, { readonly from: string } | "unplaced"> = {
    "    at fn (/work/file.ts:12:34)": { from: "/work/file.ts:12" },
    "    at /work/file.ts:12:34": { from: "/work/file.ts:12" },
    "    at fn (file:///work/file.ts:12:34)": { from: "/work/file.ts:12" },
    "    at deliver (/work/envoy.ts?nats-stall:12:34)": { from: "/work/envoy.ts:12" },
    "    at /work/file.ts?mtime=123:4:5": { from: "/work/file.ts:4" },
    "    at fn (/work/file.ts:12)": { from: "/work/file.ts:12" },
    "    at fn (/work/file.ts:12:34) \t": { from: "/work/file.ts:12" },
    "    at native": "unplaced",
    "    at <anonymous>": "unplaced",
  };
  expect(
    Object.fromEntries(
      Object.keys(shapes).map((frame) => [
        frame,
        placeArming(["Error", frame].join("\n"), ["/work/"]),
      ])
    )
  ).toEqual(shapes);
});

// recheck's automaton checker decides from a pattern's automaton whether some text costs a
// backtracking engine more than linear work in its length, so the verdict holds for every text
// and not only the one a row builds. `timeout: null` takes the clock out of the analysis: the
// verdict is the same on every machine, however loaded.
for (const [name, pattern] of Object.entries(FRAME_PATTERNS)) {
  test(`the frame pattern ${name} is linear in the frame, as recheck's automaton checker reads it`, async () => {
    const diagnostics = await check(pattern.source, pattern.flags, {
      checker: "automaton",
      timeout: null,
    });
    const why =
      diagnostics.status === "vulnerable"
        ? `${diagnostics.complexity.summary}, attack ${diagnostics.attack.pattern}`
        : diagnostics.status === "unknown"
          ? JSON.stringify(diagnostics.error)
          : diagnostics.complexity.summary;
    const failure = `${name} ${pattern} is ${diagnostics.status}: ${why}`;
    expect(diagnostics.status, failure).toBe("safe");
  });
}

test("records a deadline armed with AbortSignal.timeout, fired as its signal aborts", async () => {
  const steps: TimerStep[] = [];
  uninstall = recordTimers([root], (step) => steps.push(step));
  // The signal is read before it is returned: a call in tail position of a strict-mode function
  // (`return AbortSignal.timeout(4)`, or a `const` the transpiler folds into one) leaves that
  // function's frame off the stack (JavaScriptCore's proper tail calls), and the arming would read
  // as the caller's, this test's.
  const program = await programModule(
    "deadline.ts",
    [
      "export function deadline(): AbortSignal {",
      "  const signal = AbortSignal.timeout(4);",
      '  if (signal.aborted) throw new Error("aborted at once");',
      "  return signal;",
      "}",
    ].join("\n")
  );
  const signal = (program["deadline"] as () => AbortSignal)();
  await until("the deadline's abort", () => signal.aborted);
  expect(steps).toEqual([
    { timer: 1, delayMs: 4, from: `${join(root, "deadline.ts")}:2` },
    { fired: 1 },
  ]);
});

test("the preload appends each step to the file it is given, in a process of its own", async () => {
  const file = join(root, "preloaded.jsonl");
  const program = join(root, "program.ts");
  await Bun.write(program, "setTimeout(() => {}, 7);\n");
  const child = Bun.spawn(
    ["bun", "--preload", join(import.meta.dir, "timer-record-preload.ts"), program],
    {
      env: { ...process.env, LEGION_TIMER_RECORD_FILE: file, LEGION_TIMER_RECORD_ROOTS: root },
      stdout: "ignore",
      stderr: "inherit",
    }
  );
  expect(await settled("the recorded program's exit", child.exited)).toBe(0);
  const steps = readFileSync(file, "utf8")
    .split("\n")
    .filter(Boolean)
    .map((line) => JSON.parse(line) as TimerStep);
  expect(steps).toEqual([{ timer: 1, delayMs: 7, from: `${program}:1` }, { fired: 1 }]);
});
