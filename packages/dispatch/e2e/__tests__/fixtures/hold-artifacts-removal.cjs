"use strict";
// The slow removal of e2e/__tests__/playwright-worker-exit.test.ts, loaded into the Playwright
// runner with NODE_OPTIONS=--require. Each fs.promises.rm or fs.promises.rmdir of a worker's
// artifacts directory (`<outputDir>/.playwright-artifacts-<n>`) waits WORKER_EXIT_HOLD_REMOVAL_MS
// before it runs, as a removal of thousands of trace files does on a contended disk. Both calls
// are held because Playwright 1.64.1 removes the directory with rm and the patched runner
// (patches/playwright@1.64.1.patch) finishes it with rmdir. Each held call is said on stderr, so
// the test can tell a run that held nothing from one that held a removal. It does nothing in a
// worker or any other process.
const fs = require("node:fs");
const path = require("node:path");
const { setTimeout: sleep } = require("node:timers/promises");

const entry = process.argv[1] ?? "";
const runner = !entry.endsWith("workerProcessEntry.js") && process.argv.includes("test");
const ms = Number(process.env.WORKER_EXIT_HOLD_REMOVAL_MS ?? 0);
if (runner && ms > 0) {
  for (const name of ["rm", "rmdir"]) {
    const original = fs.promises[name];
    fs.promises[name] = async function (target, ...rest) {
      if (/^\.playwright-artifacts-\d+$/.test(path.basename(String(target)))) {
        process.stderr.write(
          `worker-exit hold: ${name} ${path.basename(String(target))} held ${ms} ms\n`
        );
        await sleep(ms);
      }
      return original.call(this, target, ...rest);
    };
  }
}
