/** The preload e2e/tools/unit-repeat.ts adds to the lane it runs: it writes where each test file
 *  is, so that a leg stopped at its deadline can name what was running. It has a second mode,
 *  for the run that reads the rows' names after a stop.
 *
 *  Where `DISPATCH_UNIT_REPEAT_PROGRESS` names a file, each line it appends there is one JSON
 *  object: an `event`, the `load` it belongs to, and `at`, the time in milliseconds since the
 *  epoch. The `load` event also carries `file`, the test file's path from the directory Bun runs
 *  in, as the junit file writes it; `pid`, the worker that runs it; and `atOnce`, whether the
 *  file's source names `.concurrent`, so that it may run its rows at once. Bun 1.4.4 runs this
 *  file once for each test file a worker loads, and not again for a rerun of it
 *  (web/src/__tests__/hold-lazy-loads.ts measured the same of its own preload), so one `load`
 *  covers every rerun of its file:
 *
 *  - `load`: the worker began the file;
 *  - `run`: a run of the file began, before the file's own `beforeAll` hooks (this file's hooks
 *    are registered before the test file's own, and Bun runs them first);
 *  - `start`: a row began, before the file's own `beforeEach` hooks;
 *  - `end`: a row ended, after the file's own `afterEach` hooks. A throwing `afterEach` of the
 *    file skips it (both measured on Bun 1.4.4 on 2026-11-20), so a missing `end` does not show
 *    that a row is still running: the reader in e2e/tools/unit-repeat.ts counts a row of a file
 *    that runs one row at a time as ended when the next one starts;
 *  - `done`: the run ended, after the file's own `afterAll` hooks.
 *
 *  Every line is one `appendFileSync` of under 200 bytes, so lines the workers write at once do
 *  not interleave.
 *
 *  Where `DISPATCH_UNIT_REPEAT_NAMES_ONLY` is `1`, its `beforeEach` throws instead, before the
 *  file's own `beforeEach` hooks and the row's body: every row fails at once and runs no code of
 *  its own, so a row that hung runs here too without hanging, and Bun prints each row's name, in
 *  the order it runs the rows (measured on Bun 1.4.4 against the order the bodies ran in, under
 *  five seeds). Where neither variable is set, as in a lane run without the tool, it writes
 *  nothing and registers no hook.
 */
import { afterAll, afterEach, beforeAll, beforeEach } from "bun:test";
import { appendFileSync, readFileSync } from "node:fs";
import { relative } from "node:path";

const progress = process.env.DISPATCH_UNIT_REPEAT_PROGRESS;

if (process.env.DISPATCH_UNIT_REPEAT_NAMES_ONLY === "1") {
  beforeEach(() => {
    throw new Error("unit-repeat: a run for the rows' names alone, so no row's body runs");
  });
} else if (progress !== undefined && progress !== "") {
  const load = crypto.randomUUID();
  const write = (event: "run" | "start" | "end" | "done") =>
    appendFileSync(progress, `${JSON.stringify({ event, load, at: Date.now() })}\n`);
  appendFileSync(
    progress,
    `${JSON.stringify({
      event: "load",
      load,
      at: Date.now(),
      file: relative(process.cwd(), Bun.main),
      pid: process.pid,
      atOnce: /\.concurrent\b/.test(readFileSync(Bun.main, "utf8")),
    })}\n`
  );
  beforeAll(() => write("run"));
  beforeEach(() => write("start"));
  afterEach(() => write("end"));
  afterAll(() => write("done"));
}
