/** The merge job's join of the legs' blob reports (`.github/scripts/dispatch-browser-merge-reports.sh`),
 *  given legs that ran in two places.
 *
 *  The pull-request lane's two legs check the repository out in two places: `real` in the CI
 *  container, under `/__w/<repo>/<repo>`, and `stub` on the bare runner, under
 *  `/home/runner/work/<repo>/<repo>` (`.github/workflows/dispatch-browser.yaml`). Each blob report
 *  records the test directory it ran from, and Playwright 1.64.1's `merge-reports` refuses blobs
 *  from two test directories unless it is handed a merge configuration: "Blob reports being merged
 *  were recorded with different test directories, and merging cannot proceed". In pull-request run
 *  45913634925 (INFRA-1109, head 35b9456b) both legs passed and the merge job failed on that
 *  refusal, with no merged report for the readings after it.
 *
 *  The row records two legs' blob reports with real Playwright runs, from the same package layout
 *  under two roots, and runs the merge script over them as the merge job does. No row opens a
 *  browser: the spec's tests take no page. */
import { expect, test } from "bun:test";
import { copyFileSync, mkdirSync, readFileSync, symlinkSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { rowLimit, runBounded } from "./bounded-subprocess";

const directories = temporaryDirectories("merge-reports");

/** packages/dispatch, whose Playwright the legs and the merge run, as the merge job's step does. */
const PACKAGE = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const SCRIPT = resolve(PACKAGE, "../../.github/scripts/dispatch-browser-merge-reports.sh");

/** Records one leg's blob report: the package's e2e directory under `root`, holding a config and
 *  a spec of one test titled for the leg, run by the package's Playwright with the blob reporter
 *  writing `report-<leg>.zip` into a directory of the leg's own, as each CI leg writes it, and
 *  copies it into `blobs`, where the merge job's download puts every leg's. The blob reporter
 *  empties its directory before it writes, so two legs never share one. */
async function recordLeg(root: string, leg: string, blobs: string): Promise<void> {
  const e2e = join(root, "packages/dispatch/e2e");
  const own = join(root, "leg-report/blob-report");
  mkdirSync(e2e, { recursive: true });
  symlinkSync(join(PACKAGE, "node_modules"), join(root, "packages/dispatch/node_modules"));
  writeFileSync(join(e2e, "playwright.config.ts"), 'export default { testDir: "." };\n');
  writeFileSync(
    join(e2e, "leg.spec.ts"),
    `import { test } from "@playwright/test";\ntest("the ${leg} leg's row", () => {});\n`
  );
  const run = await runBounded({
    argv: [
      "bunx",
      "playwright",
      "test",
      "--config",
      join(e2e, "playwright.config.ts"),
      "--reporter=blob",
    ],
    bound: "listing",
    cwd: join(root, "packages/dispatch"),
    env: {
      ...process.env,
      // `bun test --parallel`, which the package's test script runs, sets JEST_WORKER_ID in its
      // workers, and Playwright refuses to declare a row while it is set ("Playwright Test needs
      // to be invoked via 'npx playwright test' and excluded from Jest test runs").
      JEST_WORKER_ID: undefined,
      PLAYWRIGHT_BLOB_OUTPUT_DIR: own,
      PLAYWRIGHT_BLOB_OUTPUT_NAME: `report-${leg}.zip`,
    },
    from: import.meta.url,
  });
  expect(run.code, `${run.named}\n${run.stdout}\n${run.stderr}`).toBe(0);
  copyFileSync(join(own, `report-${leg}.zip`), join(blobs, `report-${leg}.zip`));
}

test(
  "legs that ran under two checkout roots, the container's and the bare runner's, merge into one report holding each leg's rows",
  async () => {
    const work = directories("work");
    const blobs = join(work, "selected-blobs/blob-report");
    mkdirSync(blobs, { recursive: true });
    await recordLeg(join(work, "container/__w/legion/legion"), "real", blobs);
    const runner = join(work, "runner/home/runner/work/legion/legion");
    await recordLeg(runner, "stub", blobs);

    // The merge job runs the script from packages/dispatch of its own checkout on the bare
    // runner, as the stub leg did.
    const merged = await runBounded({
      argv: ["bash", SCRIPT],
      bound: "short",
      cwd: join(runner, "packages/dispatch"),
      env: { ...process.env, RUNNER_TEMP: work },
      from: import.meta.url,
    });
    expect(merged.code, `${merged.named}\n${merged.stdout}\n${merged.stderr}`).toBe(0);
    const report = JSON.parse(readFileSync(join(work, "report.json"), "utf8")) as {
      stats: { expected: number; unexpected: number };
    };
    expect(report.stats).toEqual(expect.objectContaining({ expected: 2, unexpected: 0 }));
  },
  rowLimit("listing", "listing", "short")
);
