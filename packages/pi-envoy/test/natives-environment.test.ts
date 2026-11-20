// nativesEnvironment (@legion/test-fixtures/natives-environment) holds each thread pool a process a
// test starts gives @oh-my-pi/pi-natives to one worker. This row starts such a process, as
// extensions/envoy.test.ts's entry row and extensions/envoy-dial-rows-concurrent.test.ts do, and
// the process counts its Tokio workers once the addon has loaded, then its Rayon workers once
// countTokens has started the pool. Without either variable it holds one worker for each CPU of the
// machine: 288 on a shared Linux machine on 2026-11-19, where a second such process could not
// start its threads under a limit of 512 tasks. The counts are read from /proc, so the row runs on
// Linux alone.
//
// A thread takes its name only once it first runs, and until then carries the name of the thread
// that started it, this process's main thread: in 1 of 20 runs on that machine, under a load of
// 10,768, the Tokio count read at once was 0. The runtime starts its workers as the addon loads,
// so the process counts once a worker holds its name and no thread but the main one still holds the
// main one's, which is the moment every Tokio worker it started has its name. Rayon workers keep
// the main thread's name, so the process remembers that count before countTokens and counts the
// new main-named threads afterward. No event says that a thread of the kernel took its name, so
// the process reads /proc again every 10 ms until the count stays the same ten times.
import { expect, test } from "bun:test";
import { nativesEnvironment } from "@legion/test-fixtures/natives-environment";
import { command, rowMs, rowOf } from "@legion/test-fixtures/row-waits";

/** What the row waits for, which its limit is the sum of: one process of its own that loads the
 *  addon and exits. */
const theChild = command("the process that loads @oh-my-pi/pi-natives");
const oneChild = [theChild];
/** How long the process waits for its threads to take their names, inside `theChild`'s bound. */
const namedMs = theChild.ms / 2;

const marker = "pi-natives workers: ";
const script = `
import "@oh-my-pi/pi-utils";
import { createRequire } from "node:module";
import { readdirSync, readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";
const main = readFileSync("/proc/self/comm", "utf8").trim();
const names = () =>
  readdirSync("/proc/self/task").map((task) => {
    try {
      return readFileSync("/proc/self/task/" + task + "/comm", "utf8").trim();
    } catch {
      return main;
    }
  });
const mainNamed = () => names().filter((name) => name === main).length;
const beforeRayon = mainNamed();
const nativeRequire = createRequire(import.meta.resolve("@oh-my-pi/pi-utils"));
const { countTokens } = await import(pathToFileURL(nativeRequire.resolve("@oh-my-pi/pi-natives")).href);
countTokens(Array.from({ length: 2_000 }, (_, index) => "word " + index));
const deadline = Date.now() + ${namedMs};
let lastRayon = -1;
let stableRayon = 0;
for (;;) {
  const now = names();
  const tokioWorkers = now.filter((name) => name === "tokio-rt-worker").length;
  const rayonWorkers = now.filter((name) => name === main).length - beforeRayon;
  stableRayon = rayonWorkers > 0 && rayonWorkers === lastRayon ? stableRayon + 1 : 1;
  lastRayon = rayonWorkers;
  if (tokioWorkers > 0 && rayonWorkers > 0 && stableRayon >= 10) {
    console.log(${JSON.stringify(marker)} + JSON.stringify({ tokioWorkers, rayonWorkers }));
    break;
  }
  if (Date.now() > deadline) {
    console.error(
      tokioWorkers + " tokio-rt-worker threads and " + rayonWorkers + " Rayon workers after ${namedMs / 1000} s"
    );
    process.exit(2);
  }
  await Bun.sleep(10);
}
`;

test.skipIf(process.platform !== "linux")(
  "a process a test starts with nativesEnvironment, which loads pi-natives, holds one Tokio and one Rayon worker",
  () => {
    // The caller's own counts, where the shell running this file sets them, would be kept: the row
    // reads the counts the helper gives where the caller gives neither.
    const {
      TOKIO_WORKER_THREADS: _tokioGiven,
      RAYON_NUM_THREADS: _rayonGiven,
      ...rest
    } = process.env;
    rowOf(oneChild).charge(theChild);
    const run = Bun.spawnSync([process.execPath, "-e", script], {
      cwd: import.meta.dir,
      env: nativesEnvironment(rest),
      stdout: "pipe",
      stderr: "pipe",
      timeout: theChild.ms,
    });
    const said = run.stdout
      .toString()
      .split("\n")
      .find((line) => line.startsWith(marker));
    if (run.exitCode !== 0 || said === undefined) {
      throw new Error(
        `the process that loaded pi-natives exited ${run.exitCode}${run.signalCode ? ` on ${run.signalCode}` : ""} and counted nothing: ${run.stderr.toString()}`
      );
    }
    expect(JSON.parse(said.slice(marker.length))).toEqual({ tokioWorkers: 1, rayonWorkers: 1 });
  },
  rowMs(oneChild)
);
