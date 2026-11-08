import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { type Listing, rowsOfListing } from "../configurations";
import {
  type Exemption,
  exemptions,
  rowsOfSource,
  type SuiteRow,
  specFiles,
  staleExemptions,
  unexplainedRows,
} from "../no-browser-rows";

// Unlike e2e/__tests__/row-configurations.test.ts, this file imports no Playwright config: it
// reads the suite from the specs' own source and the listings from subprocesses. So it needs no
// `--list` on `process.argv` to keep an imported config from refusing a run with no harness port
// set (`isListMode` and the unset-ports refusal after it, e2e/playwright.config.ts), and leaves
// the command line alone for whatever test file this worker runs next.

// The preload (web/src/__tests__/setup.ts) puts happy-dom's Response on globalThis, which cannot
// read a subprocess's pipe; the listings below read one with Bun's own, as
// e2e/__tests__/row-configurations.test.ts:30-33 does.
const NativeResponse = (await Bun.fetch("data:,")).constructor as typeof Response;
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));

/** What `playwright test --list` reports for the real config under `environment`, which holds
 *  that listing's own `DISPATCH_E2E_PROCESSES` and `DISPATCH_E2E_SIGNIN` and nothing else: a
 *  listing never takes the ambient ones, so this file reads the same suite in every lane. The run
 *  starts no server and no browser; it costs about five seconds. */
async function listing(environment: Record<string, string>): Promise<Listing> {
  const child = Bun.spawn(
    [
      "bunx",
      "playwright",
      "test",
      "--config",
      "e2e/playwright.config.ts",
      "--list",
      "--reporter=json",
    ],
    {
      cwd: dispatchPackage,
      // No harness port or lane reaches the listing, so it is the plain one the config exempts
      // from its unset-port refusal whatever the shell that started this test holds.
      env: { HOME: process.env.HOME ?? "", PATH: process.env.PATH ?? "", ...environment },
      stderr: "pipe",
      stdout: "pipe",
    }
  );
  const [code, stderr, stdout] = await Promise.all([
    child.exited,
    new NativeResponse(child.stderr).text(),
    new NativeResponse(child.stdout).text(),
  ]);
  if (code !== 0) {
    const named =
      Object.entries(environment)
        .map(([key, value]) => `${key}=${value}`)
        .join(" ") || "the default run";
    throw new Error(`listing ${named} exited ${code}:\n${stderr}\n${stdout}`);
  }
  return JSON.parse(stdout) as Listing;
}

/** Keeps a rejection of `promise` from being reported as unhandled before the test that awaits
 *  it runs: the handler is attached the moment the promise is made, and the promise itself, which
 *  still rejects for whoever awaits it, is what this answers. */
function retained<T>(promise: Promise<T>): Promise<T> {
  promise.catch(() => {});
  return promise;
}

// All three listings are read-only subprocesses, so they start here rather than in the test that
// awaits them: the second and third collect while the first does, which is most of this file's
// cost. All three are needed, and the third is the one easily missed. The default run ignores
// every `*.multiprocess.e2e.ts`; the two-process run takes those and ignores
// `signin.multiprocess.e2e.ts`; and only DISPATCH_E2E_SIGNIN=oidc with DISPATCH_E2E_PROCESSES=2
// lists that last spec, whose servers sign people in through the dev OIDC issuer alone
// (e2e/playwright.config.ts's project lists, e2e/harness-signin.ts:20-40).
const defaultListing = retained(listing({}));
const multiprocessListing = retained(listing({ DISPATCH_E2E_PROCESSES: "2" }));
const signInListing = retained(
  listing({ DISPATCH_E2E_PROCESSES: "2", DISPATCH_E2E_SIGNIN: "oidc" })
);

const scannedFiles = specFiles(e2eDirectory);
const realRows = scannedFiles.flatMap((file) =>
  rowsOfSource(file, readFileSync(join(e2eDirectory, file), "utf8"))
);

// The three listings run at once and cost about twelve seconds together on this machine, half as
// much again as the two of e2e/__tests__/row-configurations.test.ts, which allows them 90 s. This
// file allows three minutes rather than that: a third subprocess is a third more collecting over
// the same cores, and the ceiling is the point at which something is wrong rather than slow.
const listingTimeout = 180_000;

/** The planted rows of a fixture, under the path this test reports them by. The fixtures are read
 *  by name rather than by pointing `specFiles` at their directory: they are no part of the suite,
 *  and a file that was would run in it. */
function fixtureRows(file: string): SuiteRow[] {
  return rowsOfSource(file, readFileSync(join(e2eDirectory, file), "utf8"));
}

const noBrowserFixture = "__tests__/fixtures/no-browser-row.fixture.ts";
const browserFixture = "__tests__/fixtures/browser-row.fixture.ts";

test("every row of the suite drives a browser or stands on an exemption with a reason", () => {
  // A scan that found nothing would satisfy the assertion below without reading a row.
  expect(scannedFiles.length).toBeGreaterThan(0);
  expect(realRows.length).toBeGreaterThan(0);

  expect(unexplainedRows(realRows, exemptions)).toEqual([]);
});

test("no exemption names a row the suite lost or a row that drives a browser", () => {
  expect(realRows.length).toBeGreaterThan(0);

  expect(staleExemptions(realRows, exemptions)).toEqual([]);
});

test(
  "the scanned files are exactly the files the three Playwright listings run",
  async () => {
    const [plain, multiprocess, signIn] = await Promise.all([
      defaultListing,
      multiprocessListing,
      signInListing,
    ]);
    const filesOf = (report: Listing): string[] =>
      [...new Set(rowsOfListing(report).map((row) => row.file))].sort();
    const plainFiles = filesOf(plain);
    const multiprocessFiles = filesOf(multiprocess);
    const signInFiles = filesOf(signIn);

    // A listing that collected nothing would leave the union to the other two and hide that this
    // run reads a different suite from the one Playwright runs.
    expect(plainFiles.length).toBeGreaterThan(0);
    expect(multiprocessFiles.length).toBeGreaterThan(0);
    expect(signInFiles.length).toBeGreaterThan(0);

    const listed = [...new Set([...plainFiles, ...multiprocessFiles, ...signInFiles])].sort();
    expect(listed).toEqual(scannedFiles);

    // The evidence a reader of CI records for this check. It recomputes the two assertions above
    // rather than remembering whether they ran, so the line is printed only when all three hold.
    if (
      unexplainedRows(realRows, exemptions).length === 0 &&
      staleExemptions(realRows, exemptions).length === 0
    ) {
      console.log(
        "no-browser rows: read the default, DISPATCH_E2E_PROCESSES=2 and DISPATCH_E2E_SIGNIN=oidc " +
          `DISPATCH_E2E_PROCESSES=2 listings (${plainFiles.length}, ${multiprocessFiles.length} ` +
          `and ${signInFiles.length} files, ${listed.length} together); scanned ` +
          `${scannedFiles.length} files and ${realRows.length} rows; found no unexplained ` +
          "no-browser row and no stale exemption."
      );
    }
  },
  listingTimeout
);

test("a planted row that drives no browser is named, and one that drives a browser is not", () => {
  const planted = fixtureRows(noBrowserFixture);
  const driving = fixtureRows(browserFixture);
  expect(driving).toEqual([
    {
      file: browserFixture,
      line: 16,
      title: '"a planted row that opens a page drives a browser"',
      fixtures: ["page"],
    },
  ]);

  // The check names the planted row and leaves the one beside it that opens a page, so a pass on
  // the real suite is a pass of a check that looks.
  expect(unexplainedRows([...planted, ...driving], exemptions)).toEqual([
    {
      file: noBrowserFixture,
      line: 15,
      title: '"a planted row that takes no fixture drives no browser"',
      fixtures: [],
    },
  ]);
});

test("an exemption for a row that drives a browser, and one for a row no file holds, are stale", () => {
  const driving = fixtureRows(browserFixture);
  const nowDriving: Exemption = {
    file: browserFixture,
    title: driving[0].title,
    reason: "the row drove no browser when this entry was written",
  };
  const departed: Exemption = {
    file: "a-spec-that-left.e2e.ts",
    title: '"a row the suite no longer holds"',
    reason: "the row waited on something no browser did",
  };

  const stale = staleExemptions(driving, [nowDriving, departed]);
  expect(stale.map((entry) => entry.exemption)).toEqual([nowDriving, departed]);
  expect(stale[0].why).toContain("drives a browser now");
  expect(stale[1].why).toContain("no longer holds");
});
