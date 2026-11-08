import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";

import { type Listing, rowsOfListing } from "../configurations";
import {
  type Exemption,
  exemptions,
  rowsOfFiles,
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

// All three listings are read-only subprocesses, so they start here rather than in the test that
// awaits them: the second and third collect while the first does, and the two scans below run
// while all three do, which is most of this file's cost. All three are needed, and the third is
// the one easily missed. The default run ignores every `*.multiprocess.e2e.ts`; the two-process
// run takes those and ignores `signin.multiprocess.e2e.ts`; and only DISPATCH_E2E_SIGNIN=oidc
// with DISPATCH_E2E_PROCESSES=2 lists that last spec, whose servers sign people in through the
// dev OIDC issuer alone (e2e/playwright.config.ts's project lists, e2e/harness-signin.ts:20-40).
// Each listing names its own DISPATCH_E2E_PROCESSES and DISPATCH_E2E_SIGNIN and never the
// ambient ones, so this file reads the same suite in every lane.
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
const realRows = rowsOfFiles(e2eDirectory, scannedFiles);
// Each rule is run over the suite once, here, because two tests below read each result: the one
// that asserts it and the one that prints the evidence line only when every rule held.
const realUnexplained = unexplainedRows(realRows, exemptions);
const realStale = staleExemptions(realRows, exemptions);

// The three listings run at once and cost about twelve seconds together on this machine, half as
// much again as the two of e2e/__tests__/row-configurations.test.ts, which allows them 90 s. This
// file allows three minutes rather than that: a third subprocess is a third more collecting over
// the same cores, and the ceiling is the point at which something is wrong rather than slow.
const listingTimeout = 180_000;

/** The planted controls, read by name rather than by pointing `specFiles` at their directory:
 *  they are no part of the suite, and a file that was would run in it. */
const noBrowserFixture = "__tests__/fixtures/no-browser-row.fixture.ts";
const browserFixture = "__tests__/fixtures/browser-row.fixture.ts";
const unreadShapesFixture = "__tests__/fixtures/unread-row-shapes.fixture.ts";
const unresolvedShapeFixture = "__tests__/fixtures/unresolved-row-shape.fixture.ts";
const crossFileShapeFixture = "__tests__/fixtures/cross-file-row-shape.fixture.ts";
const namedCallbackFixture = "__tests__/fixtures/named-callback-fixtures.fixture.ts";
const shadowedNamesFixture = "__tests__/fixtures/shadowed-names.fixture.ts";
const rowCalleesFixture = "__tests__/fixtures/row-callees.fixture.ts";
const calleeSpellingsFixture = "__tests__/fixtures/row-callee-spellings.fixture.ts";
const compilerOptionsFixture = "__tests__/fixtures/compiler-options.fixture.ts";

// One scan over all ten, because a scan is a program: read one file at a time it would build
// ten of them, and each fixture's imports — the callbacks four of them pass by name out of a
// module beside them — are resolved the same way either way.
const fixtureRows = rowsOfFiles(e2eDirectory, [
  noBrowserFixture,
  browserFixture,
  unreadShapesFixture,
  unresolvedShapeFixture,
  crossFileShapeFixture,
  namedCallbackFixture,
  shadowedNamesFixture,
  rowCalleesFixture,
  calleeSpellingsFixture,
  compilerOptionsFixture,
]);

/** The rows one planted control declares, in the order the scan read them. */
function rowsOf(file: string): SuiteRow[] {
  return fixtureRows.filter((row) => row.file === file);
}

const noBrowserFixtureRows = rowsOf(noBrowserFixture);
const browserFixtureRows = rowsOf(browserFixture);
const unreadShapesFixtureRows = rowsOf(unreadShapesFixture);
const unresolvedShapeFixtureRows = rowsOf(unresolvedShapeFixture);
const crossFileShapeFixtureRows = rowsOf(crossFileShapeFixture);
const namedCallbackFixtureRows = rowsOf(namedCallbackFixture);
const shadowedNamesFixtureRows = rowsOf(shadowedNamesFixture);
const rowCalleesFixtureRows = rowsOf(rowCalleesFixture);
const calleeSpellingsFixtureRows = rowsOf(calleeSpellingsFixture);
const compilerOptionsFixtureRows = rowsOf(compilerOptionsFixture);

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
  // the function its second argument names, which takes no parameter and so binds nothing.
  expect(unreadShapesFixtureRows).toEqual([
    {
      file: unreadShapesFixture,
      line: 20,
      title: "computedTitle",
      fixtures: [],
    },
    {
      file: unreadShapesFixture,
      line: 31,
      title: '"a planted row whose callback is declared beside it"',
      fixtures: [],
    },
  ]);

  // Neither shape gets past the rule: a reading that dropped them would report no row for a file
  // that holds two, and the file-set test could not see it, because a real spec keeps its other
  // rows whatever one row is written like.
  expect(unexplainedRows(unreadShapesFixtureRows, exemptions)).toEqual(unreadShapesFixtureRows);
});

// A row whose title and whose callback are BOTH passed by name: neither half is written in the
// call, so a reading that wanted a literal title or an inline function had nothing of it at all.
// The scan reports it from its callee and its arguments, and reads the callback's parameters
// where it stands - there are none, so the row drives no browser and the check names it.
test("a planted row whose title and whose callback are both passed by name is named too", () => {
  expect(unresolvedShapeFixtureRows.map((row) => row.fixtures)).toEqual([[]]);
  expect(unexplainedRows(unresolvedShapeFixtureRows, exemptions)).toEqual(
    unresolvedShapeFixtureRows
  );
});

// The same two halves with the callback declared in a module beside the fixture, which is what
// the program the scan builds is for: the import is resolved to the function it names and read
// there. It takes no fixture, so the row drives no browser and the check names it. The fixtures
// of an imported callback that does bind one are pinned in the shadowed-names fixture below.
test("a planted row whose callback is imported from another module is read there", () => {
  expect(crossFileShapeFixtureRows).toEqual([
    {
      file: crossFileShapeFixture,
      line: 35,
      title: "computedTitle",
      fixtures: [],
    },
  ]);
  expect(unexplainedRows(crossFileShapeFixtureRows, exemptions)).toEqual(crossFileShapeFixtureRows);
});

// What a resolved callback is read FOR: its parameters. Every other planted shape would pass on
// a reading that reported each unreadable row with no fixtures and never looked at a
// declaration, since each of their callbacks takes none. These two take one each, by the two
// ways a file declares a callback a call then names, and one of them is a browser: the row that
// opens a page is left alone and the row that only asks over HTTP is named, which is the rule
// deciding on fixtures it read out of the file rather than on what it could not see.
test("the fixtures of a callback passed by name are read, whatever the row's title is", () => {
  expect(namedCallbackFixtureRows).toEqual([
    {
      file: namedCallbackFixture,
      line: 32,
      title: "computedTitle",
      fixtures: ["page"],
    },
    {
      file: namedCallbackFixture,
      line: 40,
      title: '"a planted row whose callback is a const declared beside it"',
      fixtures: ["request"],
    },
  ]);
  expect(unexplainedRows(namedCallbackFixtureRows, exemptions)).toEqual([
    namedCallbackFixtureRows[1],
  ]);
});

// The correctness the checker is here for. Each of the first five rows passes a name that a
// reading matching names against the functions a file declares resolved to a function of the
// fixture that binds `page`: run over this fixture, that reading reports `fixtures: ["page"]`
// for all five and the gate passes all five. Four of them drive nothing. The fifth is the
// `let`, which holds a callback that opens a page by the time Playwright would run its row;
// the scan names that row because no `let` can be read, not because nothing opens a page.
// Each name is bound somewhere else - a for-of `const`, a `let` in a describe, a destructured
// `const`, a parameter, an import beside a nested function of its name - and the scan reads
// each binding where it stands. Two more pass no name at all, and the last two are the other
// direction: the file's own `opensAPage` where nothing shadows it, and an import that binds
// `page` in the module it comes from, both read and both left.
test("a name in a row's callback position is read as what it is bound to", () => {
  expect(shadowedNamesFixtureRows).toEqual([
    {
      file: shadowedNamesFixture,
      line: 50,
      title: '"a planted row whose callback is a for-of const of a declared name"',
      fixtures: [],
    },
    // The `let` holds a callback that opens a page by the time Playwright would run this row.
    // It is reported with no fixtures because no `let` can be read, not because nothing opens
    // a page: the check fails closed and names the row.
    {
      file: shadowedNamesFixture,
      line: 62,
      title: '"a planted row whose callback is a let inside a describe"',
      fixtures: [],
    },
    {
      file: shadowedNamesFixture,
      line: 71,
      title: '"a planted row whose callback is a destructured const"',
      fixtures: [],
    },
    // The helper's own `test(title, body)` call, read once where it is written: the title is the
    // parameter's source text, since no reader of the source can say what a caller passes.
    {
      file: shadowedNamesFixture,
      line: 78,
      title: "title",
      fixtures: [],
    },
    {
      file: shadowedNamesFixture,
      line: 83,
      title: '"a planted row whose callback is an import beside a nested function of that name"',
      fixtures: [],
    },
    // Neither of these two is a name, so there is no declaration to read. The first binds
    // `page` all the same, and the check still names its row: fixtures it did not read are not
    // fixtures it saw, and failing closed is what keeps the residual honest.
    {
      file: shadowedNamesFixture,
      line: 98,
      title: '"a planted row whose callback is what a call returns"',
      fixtures: [],
    },
    {
      file: shadowedNamesFixture,
      line: 100,
      title: '"a planted row whose callback is a property of an imported object"',
      fixtures: [],
    },
    {
      file: shadowedNamesFixture,
      line: 102,
      title: '"a planted row whose callback is this file\'s own function"',
      fixtures: ["page"],
    },
    {
      file: shadowedNamesFixture,
      line: 104,
      title: '"a planted row whose callback is imported and opens a page"',
      fixtures: ["page"],
    },
  ]);

  // The seven whose fixtures the scan did not read are named and the two it read a page out of
  // are left, which is the whole of the difference between reading a name and matching it. Two
  // of the seven - the `let` and the call result - do take `page` when Playwright runs them,
  // and they are named because the check fails closed on a body it could not read, which is
  // what the rule asks of a residual.
  expect(unexplainedRows(shadowedNamesFixtureRows, exemptions)).toEqual(
    shadowedNamesFixtureRows.slice(0, 7)
  );
});

// Which member of a `test` declares a row, which is decided before any argument is read.
// `test.fail`, `test.skip` and `test.fixme` declare a row in their title-first forms and mark
// the row they stand in in their condition forms; `test.fail.only` is a member of a member and
// declares a row; `test.describe.only` and `test.step.skip` are members of a member and declare
// none. The fixture holds all of them, with a `test.skip(<condition>, <reason>)` whose reason is
// a `const` - the shape of the four real calls of this suite - and a `test.slow()` beside them.
// Three of the rows carry a computed title and a `let` body, so the callee is the whole of what
// says they are rows at all.
test("every member of a test that declares a row is read as one, and no other member is", () => {
  expect(rowCalleesFixtureRows).toEqual([
    {
      file: rowCalleesFixture,
      line: 63,
      title: '"a planted row declared by test.fail"',
      fixtures: [],
    },
    {
      file: rowCalleesFixture,
      line: 65,
      title: "skipTitle",
      fixtures: [],
    },
    {
      file: rowCalleesFixture,
      line: 67,
      title: "fixmeTitle",
      fixtures: [],
    },
    {
      file: rowCalleesFixture,
      line: 69,
      title: "failTitle",
      fixtures: [],
    },
    {
      file: rowCalleesFixture,
      line: 71,
      title: '"a planted row declared by test.fail.only"',
      fixtures: [],
    },
    // The row inside the `test.describe.only` block. The block's own call declares no row, and
    // neither does the `test.slow()`, the `test.skip(<condition>, <reason>)` or the
    // `test.step.skip(…)` inside this one, or there would be more rows here than six.
    {
      file: rowCalleesFixture,
      line: 77,
      title: '"a planted row inside a describe block"',
      fixtures: ["page"],
    },
  ]);

  expect(unexplainedRows(rowCalleesFixtureRows, exemptions)).toEqual(
    rowCalleesFixtureRows.slice(0, 5)
  );
});

// How a `test` and a member of one are spelled, which the callee's type answers and its text
// cannot. Every row below is declared by a `TestType`, or by a member of one, under another
// spelling, and a reading that matched the text found none of them: it reported no row for
// this file at all, so no file-set comparison and no row count could have caught them.
// `test.extend` is Playwright's documented way to add a fixture, so the first of them is the
// likely shape of a real row that drives no browser.
test("a row is read whatever the test that declares it is called", () => {
  expect(calleeSpellingsFixtureRows).toEqual([
    {
      file: calleeSpellingsFixture,
      line: 45,
      title: '"a planted row declared by an extended test"',
      fixtures: ["request"],
    },
    {
      file: calleeSpellingsFixture,
      line: 49,
      title: '"a planted row declared by a const bound to test"',
      fixtures: [],
    },
    {
      file: calleeSpellingsFixture,
      line: 53,
      title: '"a planted row declared by an import renamed to it"',
      fixtures: [],
    },
    {
      file: calleeSpellingsFixture,
      line: 57,
      title: '"a planted row declared through a namespace import"',
      fixtures: [],
    },
    {
      file: calleeSpellingsFixture,
      line: 62,
      title: '"a planted row declared by element access"',
      fixtures: [],
    },
    {
      file: calleeSpellingsFixture,
      line: 67,
      title: '"a planted row declared by a parenthesized test"',
      fixtures: [],
    },
    // Inside the `apiTest.describe` block, which declares no row of its own, and neither do the
    // `alias.slow()` and the `apiTest.skip(<condition>, <reason>)` in this row's body: a member
    // of a `TestType` is the member it is whatever the `TestType` is called. Its callback
    // declares a `this` parameter, which binds no fixture: `page` is read from the parameter
    // after it, and a reading that took the first would name a row that opens a page.
    {
      file: calleeSpellingsFixture,
      line: 76,
      title: '"a planted row inside an extended describe"',
      fixtures: ["page"],
    },
    // The five members bound to names of their own. Playwright binds `only`, `skip`, `fixme`,
    // `fail` and `fail.only` as functions, so each of these calls runs a row; the name says
    // nothing about which member it holds, and a reading of the written name read none of the
    // five. The scan keys a member by the symbol of its type, so each is the member it came
    // from.
    {
      file: calleeSpellingsFixture,
      line: 94,
      title: '"a planted row declared by a const bound to test.only"',
      fixtures: [],
    },
    {
      file: calleeSpellingsFixture,
      line: 98,
      title: '"a planted row declared by a destructured skip"',
      fixtures: [],
    },
    {
      file: calleeSpellingsFixture,
      line: 102,
      title: '"a planted row declared by a destructured fixme"',
      fixtures: [],
    },
    {
      file: calleeSpellingsFixture,
      line: 106,
      title: '"a planted row declared by a const bound to test.fail"',
      fixtures: [],
    },
    {
      file: calleeSpellingsFixture,
      line: 110,
      title: '"a planted row declared by the only of a const bound to test.fail"',
      fixtures: [],
    },
  ]);

  // Every row of this file but the one inside the describe block, which is the only one that
  // opens a page. Playwright's `request` is no browser, so the extended row is named with the
  // rest.
  expect(unexplainedRows(calleeSpellingsFixtureRows, exemptions)).toEqual([
    ...calleeSpellingsFixtureRows.slice(0, 6),
    ...calleeSpellingsFixtureRows.slice(7),
  ]);
});

// What taking the package's `tsconfig.json` whole is worth, which no other control measures:
// each of these two is read wrong under a picked set of options. The first row's callback is
// imported from a `.tsx` module, which resolves only with `jsx` among the options — without it
// the import is refused (TS6142) and the row is read with no fixtures, so a row that opens a
// page is refused on correct source. The `test.skip(<condition>, <reason>)` below it declares
// no row only while `bun` is among `types` and `process.env.SKIP_REASON` is therefore a
// `string | undefined`; without it `process` is `any`, the reason reads as a body, and the
// marking call is reported as a row that drives no browser.
test("the scan's options are the package's own, so a .tsx callback and process are read", () => {
  expect(compilerOptionsFixtureRows).toEqual([
    {
      file: compilerOptionsFixture,
      line: 30,
      title: '"a planted row whose callback is imported from a .tsx module"',
      fixtures: ["page"],
    },
  ]);

  // The row opens a page and the marking call is no row, so this file leaves the rule nothing.
  expect(unexplainedRows(compilerOptionsFixtureRows, exemptions)).toEqual([]);
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
