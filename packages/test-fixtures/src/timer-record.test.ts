import { afterEach, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories, temporaryDirectory } from "./temporary-directories";
import {
  placeArming,
  recordTimers,
  stepsOf,
  TIMER_RECORD_FILE,
  TIMER_RECORD_ROOTS,
  type TimerStep,
  timersBetween,
} from "./timer-record";
import { settled, timeLeft, until } from "./wait";

const makeDirectory = temporaryDirectories("test-fixtures-timer-record");
const PRELOAD = join(import.meta.dir, "timer-record-preload.ts");
const WAIT = JSON.stringify(join(import.meta.dir, "wait.ts"));

/** The root of the program modules the rows below write, whose timers are the program's own. */
const programRoot = temporaryDirectory("timer-record");
let uninstall: (() => void) | undefined;
afterEach(() => {
  uninstall?.();
  uninstall = undefined;
});

/** A program module under `programRoot`, written for the row, whose timers are the program's own.
 *  It is imported by its path, since the row writes it into a directory made at run time. */
async function programModule(name: string, source: string): Promise<Record<string, unknown>> {
  const file = join(programRoot, name);
  await Bun.write(file, source);
  return (await import(file)) as Record<string, unknown>;
}

/**
 * Runs `bun` on a program that waits, through the shared wait, for the record's second stamp,
 * with the record preloaded after `before` (preloads of the child's own, in order), and returns
 * the canary of the last stamp the record holds. The child is no `bun test`, so its wait ends at
 * this row's deadline, which `LEGION_PROOF_DEADLINE_MS` hands it.
 */
function lastCanary(before: readonly string[]): readonly string[] {
  const root = makeDirectory("canary");
  const program = join(root, "program.ts");
  const record = join(root, "record.jsonl");
  writeFileSync(
    program,
    `import { readFileSync } from "node:fs";
import { until } from ${WAIT};
await until("the record's second stamp", () => {
  try {
    return readFileSync(${JSON.stringify(record)}, "utf8").split('"alive"').length > 2;
  } catch {
    return false;
  }
});
`
  );
  const run = Bun.spawnSync(
    [
      process.execPath,
      ...before.flatMap((file) => ["--preload", file]),
      "--preload",
      PRELOAD,
      program,
    ],
    {
      cwd: root,
      env: {
        ...process.env,
        [TIMER_RECORD_FILE]: record,
        [TIMER_RECORD_ROOTS]: root,
        LEGION_PROOF_DEADLINE_MS: String(Date.now() + timeLeft()),
      },
    }
  );
  expect(run.exitCode, `${run.stdout.toString()}${run.stderr.toString()}`).toBe(0);
  const stamps = readFileSync(record, "utf8")
    .split("\n")
    .filter((line) => line.includes('"alive"'))
    .map((line) => JSON.parse(line) as { readonly canary: readonly string[] });
  expect(stamps.length, "the record's stamps").toBeGreaterThan(1);
  return stamps.at(-1)?.canary ?? [];
}

// `node:timers/promises` keeps the global `setTimeout` it finds when it first loads, so its timers
// reach the record's wrap only where the record was installed first. The canary says which: a
// record preloaded first names it beside the four wrapped calls, and one that a preload loading
// the module came before does not, so a row that requires the canary fails by name there.
test("the canary names node:timers/promises only where the record came before that module", () => {
  expect(lastCanary([])).toEqual([
    "setTimeout",
    "setInterval",
    "Bun.sleep",
    "AbortSignal.timeout",
    "node:timers/promises",
  ]);
  const early = join(makeDirectory("early"), "early.ts");
  writeFileSync(early, 'import "node:timers/promises";\n');
  expect(lastCanary([early])).toEqual([
    "setTimeout",
    "setInterval",
    "Bun.sleep",
    "AbortSignal.timeout",
  ]);
});

// A recorded process hands the record to no child: once its preload has read them, the two
// variables and its own `--preload` are gone from its environment, so a child it starts writes
// nothing into the record. And every step its writer writes carries its process id, so a reader
// that takes one process's steps (`stepsOf`) leaves out a child that was handed them anyway.
test("a recorded process hands the record to no child, and its steps carry its process id", () => {
  const root = makeDirectory("child");
  const program = join(root, "program.ts");
  const record = join(root, "record.jsonl");
  writeFileSync(
    program,
    `const child = Bun.spawnSync([process.execPath, "-e", ${JSON.stringify(
      "console.log(JSON.stringify([process.env.LEGION_TIMER_RECORD_FILE, process.env.LEGION_TIMER_RECORD_ROOTS, process.env.BUN_OPTIONS]))"
    )}]);
console.log(JSON.stringify({ pid: process.pid, child: child.stdout.toString().trim() }));
setTimeout(() => undefined, 5);
`
  );
  const run = Bun.spawnSync([process.execPath, program], {
    cwd: root,
    env: {
      ...process.env,
      [TIMER_RECORD_FILE]: record,
      [TIMER_RECORD_ROOTS]: root,
      BUN_OPTIONS: `--preload ${PRELOAD} --smol`,
    },
  });
  expect(run.exitCode, `${run.stdout.toString()}${run.stderr.toString()}`).toBe(0);
  const said = JSON.parse(run.stdout.toString().trim().split("\n").at(-1) ?? "{}") as {
    readonly pid: number;
    readonly child: string;
  };
  // The child sees neither variable, and BUN_OPTIONS without the preload but with what else it held.
  expect(JSON.parse(said.child)).toEqual([null, null, "--smol"]);
  const steps = readFileSync(record, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as { readonly pid?: number });
  expect(steps.length, "the record's steps").toBeGreaterThan(0);
  expect(steps.filter((step) => step.pid !== said.pid)).toEqual([]);
  expect(stepsOf([...steps, { alive: 1, pid: said.pid + 1 }, { mark: "m" }], said.pid)).toEqual([
    ...steps,
    { mark: "m" },
  ]);
});

// Loaded after the wrap, as in this process, which loads it first in this row, `node:timers/promises`'
// `setTimeout` arms through the wrapped global, and the first frame past this module is the
// runtime's own (`node:timers/promises:…`), before the frame of the code that called it. The pane's
// own code waits that way (envoy-client's nats-connection.ts waits between dials with it), so a
// timer it arms there is the code's, and the record keeps it with the code's line.
test("a timer armed through node:timers/promises from a rooted file is that file's", async () => {
  const root = makeDirectory("root");
  const waiter = join(root, "waiter.ts");
  writeFileSync(
    waiter,
    `import { setTimeout as sleep } from "node:timers/promises";
export async function wait(): Promise<void> {
  await sleep(25);
}
`
  );
  const steps: TimerStep[] = [];
  const restore = recordTimers([root], (step) => {
    steps.push(step);
  });
  try {
    // The waiter is written to a directory this row makes, so its path is known only now.
    const module = (await import(waiter)) as { wait: () => Promise<void> };
    await module.wait();
  } finally {
    restore();
  }
  const armed = steps.filter(
    (step): step is Extract<TimerStep, { timer: number }> => "timer" in step
  );
  expect(
    armed.map(({ delayMs, from }) => ({ delayMs, file: from.slice(0, from.lastIndexOf(":")) })),
    `the record: ${JSON.stringify(steps)}`
  ).toEqual([{ delayMs: 25, file: waiter }]);
  expect(steps).toContainEqual({ fired: armed[0]?.timer ?? -1 });
});

// The same reading on the stack the runtime prints for that arming, so the row above cannot pass
// for want of the `node:` frame: it is the first frame past this module, and the frame after it is
// the code's.
test("a node: frame names no file, and the frame after it is the arming's", () => {
  const stack = [
    "Error",
    "    at <anonymous> (node:timers/promises:40:35)",
    "    at wait (/srv/pane/waiter.ts:3:9)",
  ].join("\n");
  expect(placeArming(stack, ["/srv/pane/"])).toEqual({ from: "/srv/pane/waiter.ts:3" });
  expect(placeArming(stack, ["/elsewhere/"])).toBe("foreign");
});

test("records the program's timers with the delays it chose, in the order they ran", async () => {
  const steps: TimerStep[] = [];
  uninstall = recordTimers([programRoot], (step) => steps.push(step));
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
    `${join(programRoot, "waits.ts")}:1`
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
  uninstall = recordTimers([programRoot], (step) => steps.push(step));
  const file = join(programRoot, "queried.ts");
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
  uninstall = recordTimers([join(programRoot, "elsewhere")], (step) => steps.push(step));
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
  const checkout = join(programRoot, "test", "checkout");
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

test("records a deadline armed with AbortSignal.timeout, fired as its signal aborts", async () => {
  const steps: TimerStep[] = [];
  uninstall = recordTimers([programRoot], (step) => steps.push(step));
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
    { timer: 1, delayMs: 4, from: `${join(programRoot, "deadline.ts")}:2` },
    { fired: 1 },
  ]);
});

// The preload's writer also stamps the record as alive every `TIMER_RECORD_ALIVE_MS` while the
// child lives, so how many stamps a run holds is the machine's; the row reads the timer steps
// alone, each carrying the child's process id.
test("the preload appends each step to the file it is given, in a process of its own", async () => {
  const file = join(programRoot, "preloaded.jsonl");
  const program = join(programRoot, "program.ts");
  await Bun.write(program, "setTimeout(() => {}, 7);\n");
  const child = Bun.spawn([process.execPath, "--preload", PRELOAD, program], {
    env: { ...process.env, [TIMER_RECORD_FILE]: file, [TIMER_RECORD_ROOTS]: programRoot },
    stdout: "ignore",
    stderr: "inherit",
  });
  expect(await settled("the recorded program's exit", child.exited)).toBe(0);
  const steps = readFileSync(file, "utf8")
    .split("\n")
    .filter(Boolean)
    .map((line) => JSON.parse(line) as Record<string, unknown>)
    .filter((step) => !("alive" in step));
  expect(steps).toEqual([
    { timer: 1, delayMs: 7, from: `${program}:1`, pid: child.pid },
    { fired: 1, pid: child.pid },
  ]);
});
