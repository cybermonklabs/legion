"use strict";
// The held removal of e2e/__tests__/playwright-worker-exit.test.ts, loaded into the Playwright
// runner with NODE_OPTIONS=--require. Each fs.promises.rm or fs.promises.rmdir of a worker's
// artifacts directory (`<outputDir>/.playwright-artifacts-<n>`) waits, before it runs, until the
// runner has sent that worker's 'exit', the event its stop() waits for: the 'exit' a WorkerHost
// emits whose artifactsDir is that directory. A runner that sends 'exit' when the process exits,
// as patches/playwright@1.64.1.patch makes it, sends it before the removal, which then runs. A
// runner that sends 'exit' only once the removal has ended, as Playwright 1.64.1 does, never
// sends it, so its stop waits until its deadline and the run until the test's bound. No time
// stands for the removal: on a contended disk a removal of thousands of trace files takes minutes,
// and the order of the two events is what the row reads. Both calls are held because Playwright
// 1.64.1 removes the directory with rm and the patched runner finishes it with rmdir. Each held
// call is said on stderr, so the test can tell a run that held nothing from one that held a
// removal. With WORKER_EXIT_HOLD_REMOVAL unset it does nothing, nor in a worker or any other
// process.
const events = require("node:events");
const fs = require("node:fs");
const path = require("node:path");

const entry = process.argv[1] ?? "";
const runner = !entry.endsWith("workerProcessEntry.js") && process.argv.includes("test");
if (runner && process.env.WORKER_EXIT_HOLD_REMOVAL === "1") {
  // The artifacts directories whose worker has sent 'exit', and the removals waiting on each.
  const exited = new Set();
  const waiting = new Map();
  const emit = events.EventEmitter.prototype.emit;
  events.EventEmitter.prototype.emit = function (event, ...args) {
    const dir = this?._params?.artifactsDir;
    if (event === "exit" && typeof dir === "string") {
      const name = path.basename(dir);
      exited.add(name);
      for (const release of waiting.get(name) ?? []) release();
      waiting.delete(name);
    }
    return emit.call(this, event, ...args);
  };
  for (const name of ["rm", "rmdir"]) {
    const original = fs.promises[name];
    fs.promises[name] = async function (target, ...rest) {
      const folder = path.basename(String(target));
      if (/^\.playwright-artifacts-\d+$/.test(folder)) {
        const started = Date.now();
        process.stderr.write(`worker-exit hold: ${name} ${folder} waits for its worker's exit\n`);
        if (!exited.has(folder)) {
          await new Promise((release) =>
            waiting.set(folder, [...(waiting.get(folder) ?? []), release])
          );
        }
        process.stderr.write(
          `worker-exit hold: ${name} ${folder} ran ${Date.now() - started} ms after it waited\n`
        );
      }
      return original.call(this, target, ...rest);
    };
  }
}
