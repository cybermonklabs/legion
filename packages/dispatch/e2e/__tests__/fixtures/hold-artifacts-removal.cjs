"use strict";
// The held removal of e2e/__tests__/playwright-worker-exit.test.ts, loaded into the Playwright
// runner with NODE_OPTIONS=--require. Each fs.promises.rm or fs.promises.rmdir of a worker's
// artifacts directory (`<outputDir>/.playwright-artifacts-<n>`) waits, before it runs, for two
// events of the runner, in turn:
//
// 1. That worker's 'exit', the event its stop() waits for: the 'exit' a WorkerHost emits whose
//    artifactsDir is that directory. A runner that sends 'exit' when the process exits, as
//    patches/playwright@1.64.1.patch makes it, sends it before the removal. A runner that sends
//    'exit' only once the removal has ended, as Playwright 1.64.1 does, never sends it, so its stop
//    waits until its deadline and the run until the test's bound.
// 2. The run waiting for the removals: the patched Dispatcher.stop() awaits Promise.all of the
//    set it keeps them in (artifactsRemovals) before the run may end, and this is the one
//    Promise.all of the runner over a Set. A runner that does not wait for them ends the run with
//    the removal still held, and the directory is still there when it exits.
//
// So the row reads the order of these events and no time: the removal runs only after the
// worker's exit, and the run ends only after the removal, however fast the removal would be. On
// a contended disk a removal of thousands of trace files takes minutes. Both calls are held
// because Playwright 1.64.1 removes the directory with rm and the patched runner finishes it with
// rmdir. Each held call is said on stderr, so the test can tell a run that held nothing from one
// that held a removal. With WORKER_EXIT_HOLD_REMOVAL unset it does nothing, nor in a worker or any
// other process.
const events = require("node:events");
const fs = require("node:fs");
const path = require("node:path");

const entry = process.argv[1] ?? "";
const runner = !entry.endsWith("workerProcessEntry.js") && process.argv.includes("test");
if (runner && process.env.WORKER_EXIT_HOLD_REMOVAL === "1") {
  // The artifacts directories whose worker has sent 'exit', and the removals waiting on each.
  const exited = new Set();
  const waitingForExit = new Map();
  const emit = events.EventEmitter.prototype.emit;
  events.EventEmitter.prototype.emit = function (event, ...args) {
    const dir = this?._params?.artifactsDir;
    if (event === "exit" && typeof dir === "string") {
      const name = path.basename(dir);
      exited.add(name);
      for (const release of waitingForExit.get(name) ?? []) release();
      waitingForExit.delete(name);
    }
    return emit.call(this, event, ...args);
  };
  // Whether the run has begun to wait for the removals, and the removals waiting for that.
  let runWaits = false;
  let waitingForRun = [];
  const all = Promise.all;
  Promise.all = function (iterable) {
    if (iterable instanceof Set && !runWaits) {
      runWaits = true;
      process.stderr.write("worker-exit hold: the run waits for the removals\n");
      for (const release of waitingForRun) release();
      waitingForRun = [];
    }
    return all.call(this, iterable);
  };
  for (const name of ["rm", "rmdir"]) {
    const original = fs.promises[name];
    fs.promises[name] = async function (target, ...rest) {
      const folder = path.basename(String(target));
      if (/^\.playwright-artifacts-\d+$/.test(folder)) {
        const started = Date.now();
        process.stderr.write(
          `worker-exit hold: ${name} ${folder} waits for its worker's exit and for the run to wait for it\n`
        );
        if (!exited.has(folder)) {
          await new Promise((release) =>
            waitingForExit.set(folder, [...(waitingForExit.get(folder) ?? []), release])
          );
        }
        if (!runWaits) await new Promise((release) => waitingForRun.push(release));
        process.stderr.write(
          `worker-exit hold: ${name} ${folder} ran ${Date.now() - started} ms after it waited\n`
        );
      }
      return original.call(this, target, ...rest);
    };
  }
}
