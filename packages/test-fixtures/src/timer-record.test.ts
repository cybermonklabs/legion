import { expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "./temporary-directories";
import {
  placeArming,
  recordTimers,
  stepsOf,
  TIMER_RECORD_FILE,
  TIMER_RECORD_ROOTS,
  type TimerStep,
} from "./timer-record";
import { timeLeft } from "./wait";

const makeDirectory = temporaryDirectories("test-fixtures-timer-record");
const PRELOAD = join(import.meta.dir, "timer-record-preload.ts");
const WAIT = JSON.stringify(join(import.meta.dir, "wait.ts"));

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
