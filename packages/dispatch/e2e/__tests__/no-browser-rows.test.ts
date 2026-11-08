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
import { listing, retained } from "./playwright-listing";

// Unlike e2e/__tests__/row-configurations.test.ts, this file imports no Playwright config: it
// reads the suite from the specs' own source and the listings from subprocesses. So it needs no
// `--list` on `process.argv` to keep an imported config from refusing a run with no harness port
// set (`isListMode` and the unset-ports refusal after it, e2e/playwright.config.ts), and leaves
// the command line alone for whatever test file this worker runs next.
const e2eDirectory = fileURLToPath(new URL("..", import.meta.url));

/** The rows a file under the `e2e` directory declares, under the path this test reports them
 *  by. */
function rowsOfFile(file: string): SuiteRow[] {
  return rowsOfSource(file, readFileSync(join(e2eDirectory, file), "utf8"));
}

// All three listings are read-only subprocesses, so they start here rather than in the test that
// awaits them: the second and third collect while the first does, which is most of this file's
// cost. All three are needed, and the third is the one easily missed. The default run ignores
// every `*.multiprocess.e2e.ts`; the two-process run takes those and ignores
// `signin.multiprocess.e2e.ts`; and only DISPATCH_E2E_SIGNIN=oidc with DISPATCH_E2E_PROCESSES=2
// lists that last spec, whose servers sign people in through the dev OIDC issuer alone
// (e2e/playwright.config.ts's project lists, e2e/harness-signin.ts:20-40). Each listing names
// its own DISPATCH_E2E_PROCESSES and DISPATCH_E2E_SIGNIN and never the ambient ones, so this
// file reads the same suite in every lane.
const defaultListing = retained(listing("e2e/playwright.config.ts", {}));
const multiprocessListing = retained(
  listing("e2e/playwright.config.ts", { DISPATCH_E2E_PROCESSES: "2" })
);
const signInListing = retained(
  listing("e2e/playwright.config.ts", {
    DISPATCH_E2E_PROCESSES: "2",
    DISPATCH_E2E_SIGNIN: "oidc",
  })
);

const scannedFiles = specFiles(e2eDirectory);
const realRows = scannedFiles.flatMap(rowsOfFile);
// Each rule is run over the suite once, here, because two tests below read each result: the one
// that asserts it and the one that prints the evidence line only when every rule held.
const realUnexplained = unexplainedRows(realRows, exemptions);
const realStale = staleExemptions(realRows, exemptions);

// The three listings run at once and cost about twelve seconds together on this machine, half as
// much again as the two of e2e/__tests__/row-configurations.test.ts, which allows them 90 s. This
// file allows three minutes rather than that: a third subprocess is a third more collecting over
// the same cores, and the ceiling is the point at which something is wrong rather than slow.
const listingTimeout = 180_000;

/** The four planted controls, read by name rather than by pointing `specFiles` at their
 *  directory: they are no part of the suite, and a file that was would run in it. Each is read
 *  and parsed once, here, for the tests below that read its rows. */
const noBrowserFixture = "__tests__/fixtures/no-browser-row.fixture.ts";
const browserFixture = "__tests__/fixtures/browser-row.fixture.ts";
const unreadShapesFixture = "__tests__/fixtures/unread-row-shapes.fixture.ts";
const unresolvedShapeFixture = "__tests__/fixtures/unresolved-row-shape.fixture.ts";
const noBrowserFixtureRows = rowsOfFile(noBrowserFixture);
const browserFixtureRows = rowsOfFile(browserFixture);
const unreadShapesFixtureRows = rowsOfFile(unreadShapesFixture);
const unresolvedShapeFixtureRows = rowsOfFile(unresolvedShapeFixture);

// A scan that found nothing would satisfy every rule below without reading a row, so this is
// asserted once here rather than again in each test that reads `realRows`.
test("the scan reads files and rows of the real suite", () => {
  expect(scannedFiles.length).toBeGreaterThan(0);
  expect(realRows.length).toBeGreaterThan(0);
});

test("every row of the suite drives a browser or stands on an exemption with a reason", () => {
  expect(realUnexplained).toEqual([]);
});

test("no exemption names a row the suite lost or a row that drives a browser", () => {
  expect(realStale).toEqual([]);
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

    // The evidence a reader of CI records for this check. It prints the two results the tests
    // above assert, so the line is there only when all three rules held in this run.
    if (realUnexplained.length === 0 && realStale.length === 0) {
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
  expect(browserFixtureRows).toEqual([
    {
      file: browserFixture,
      line: 16,
      title: '"a planted row that opens a page drives a browser"',
      fixtures: ["page"],
    },
  ]);

  // The check names the planted row and leaves the one beside it that opens a page, so a pass on
  // the real suite is a pass of a check that looks.
  expect(unexplainedRows([...noBrowserFixtureRows, ...browserFixtureRows], exemptions)).toEqual([
    {
      file: noBrowserFixture,
      line: 15,
      title: '"a planted row that takes no fixture drives no browser"',
      fixtures: [],
    },
  ]);
});

test("a planted row with a computed title, and one with a callback by name, are both named", () => {
  // Both rows are read: the first by its inline callback, whatever its title is, the second by
  // its literal title, with no fixtures because its callback is out of this module's reach.
  expect(unreadShapesFixtureRows).toEqual([
    {
      file: unreadShapesFixture,
      line: 19,
      title: "computedTitle",
      fixtures: [],
    },
    {
      file: unreadShapesFixture,
      line: 29,
      title: '"a planted row whose callback is declared beside it"',
      fixtures: [],
    },
  ]);

  // Neither shape gets past the rule: a reading that dropped them would report no row for a file
  // that holds two, and the file-set test could not see it, because a real spec keeps its other
  // rows whatever one row is written like.
  expect(unexplainedRows(unreadShapesFixtureRows, exemptions)).toEqual(unreadShapesFixtureRows);
});

// A row whose title and whose callback are BOTH passed by name. Playwright runs it like any
// other row and it binds no browser fixture, so the check must report it and name it. It reads
// nothing of it today, which is the gate hole this test pins: a row of this shape passes in
// silence, and neither the file-set comparison nor the two controls above can see it, since the
// file keeps whatever other rows it holds. `packages/dispatch/AGENTS.md` promises otherwise -
// the check "reads every `test(` call" and "refuses … a row whose callback it cannot read …
// since fixtures it did not read are not fixtures it saw". Reading the second argument's
// declaration in the same file settles it with no type checker, and leaves the four
// `test.skip(<condition>, <reason>)` calls of this suite that name neither by a literal alone,
// since what they name is no function.
test("a planted row whose title and whose callback are both passed by name is named too", () => {
  expect(unresolvedShapeFixtureRows.map((row) => row.fixtures)).toEqual([[]]);
  expect(unexplainedRows(unresolvedShapeFixtureRows, exemptions)).toEqual(
    unresolvedShapeFixtureRows
  );
});

test("an exemption for a row that drives a browser, and one for a row no file holds, are stale", () => {
  const nowDriving: Exemption = {
    file: browserFixture,
    title: browserFixtureRows[0].title,
    reason: "the row drove no browser when this entry was written",
  };
  const departed: Exemption = {
    file: "a-spec-that-left.e2e.ts",
    title: '"a row the suite no longer holds"',
    reason: "the row waited on something no browser did",
  };

  const stale = staleExemptions(browserFixtureRows, [nowDriving, departed]);
  expect(stale.map((entry) => entry.exemption)).toEqual([nowDriving, departed]);
  expect(stale[0].why).toContain("drives a browser now");
  expect(stale[1].why).toContain("no longer holds");
});
