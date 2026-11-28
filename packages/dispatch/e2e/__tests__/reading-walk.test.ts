import { expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { DISPATCH_READING_SURFACES, type DispatchReadingProof } from "@legion/contracts";
import * as ts from "typescript";

import { type ListedRow, rowsOfListing } from "../configurations";
import { referencesLaneFiles } from "../reference-lane";
import { rowLimit } from "./bounded-subprocess";
import { listing, projectArguments, retained } from "./playwright-listing";

/**
 * The gate of the reading list's walk (`e2e/reading-rows.ts`): every rendered
 * row of `DISPATCH_READING_SURFACES` has exactly one driver among the `reading-*.e2e.ts` specs,
 * a row titled with the surface's id followed by a colon, and so does every arm of one (the
 * surface's id, a `/` and the arm's, then the colon). Playwright runs every driver rather than
 * skipping it, and no driver names a row or an arm the list does not hold or does not render. A
 * row the list gains with no driver fails here, by name, before any browser runs.
 *
 * The drivers are the rows Playwright itself lists, not titles read out of the specs' text: a
 * title in a comment declares no row, and a row marked `test.fixme` or `test.skip` declares one
 * Playwright never runs, so neither can stand in for a driver here.
 *
 * Two more rules hold what a driver reads and where it runs. A driver stores every form its
 * row's surface carries (`cannotCarry`), so a rendered row whose text is no record must say it
 * carries no question id, or its driver would hold it to a `#id` that names nothing there; and
 * a row that names a form it cannot carry must say why, or the list would let a surface off a
 * form with nothing a reader could check. And the references lane names its spec files one by
 * one, so a reading spec it does not name would pass every rule above and run on no pull
 * request.
 *
 * A row drawn through the plain-text projection has no driver here, since the walk drives the
 * rendered rows alone: it names the test that proves it instead (`proof`), and that test must be
 * one its file declares and runs. The source test is what says which rows draw the projection
 * and fails one that names no proof (`web/src/features/refs/surface-lists.test.ts`).
 */

/** The project the references lane runs the reading specs in (the `references)` arm of
 *  `.github/scripts/dispatch-browser-shard-suite.sh`), and the file filter that names those specs
 *  and no other: Playwright matches it against each spec's path, where `inbox-reading-pane.e2e.ts`
 *  holds `reading-` but not `e2e/reading-`. */
const READING_LISTING_ARGUMENTS = [...projectArguments(["chromium"]), "e2e/reading-"];

/** What a driver drives: the id its title starts with, before the colon - a row's surface, or a
 *  surface, a `/` and one of its arms (`DispatchReadingArm`). A row of a reading spec whose title
 *  starts with no id is no driver of any row, so it is answered by its whole title, which names
 *  nothing the list drives and is reported as such. */
const DRIVEN = /^([a-z0-9-]+(?:\/[a-z0-9-]+)?):/u;

/** Everything the walk drives, each by the id a driver's title opens with: every rendered row,
 *  and every arm of one. */
const DRIVEN_IDS: readonly string[] = DISPATCH_READING_SURFACES.filter(
  (row) => row.rendered
).flatMap((row) => [row.surface, ...(row.arms ?? []).map((arm) => `${row.surface}/${arm.arm}`)]);

/** One row Playwright lists in the reading specs: what it drives, where it is declared, and
 *  whether Playwright runs it. */
interface Driver {
  readonly drives: string;
  readonly file: string;
  readonly title: string;
  /** What Playwright expects of the row: `passed` for a row it runs, `skipped` for one marked
   *  `fixme` or `skip`, `failed` for one marked `fail`. */
  readonly expectedStatus: string;
}

/** Every row of the reading specs, read from the listing. A spec's rows are one level under its
 *  file, so a row's title is its own; a row inside a `describe` block reads as the block's title
 *  first and is answered by its own title, the last part. */
function driversOf(rows: readonly ListedRow[]): Driver[] {
  return rows
    .filter((row) => /^reading-[^/]*\.e2e\.ts$/u.test(row.file))
    .map((row) => {
      const title = row.title.split(" › ").at(-1) ?? row.title;
      return {
        drives: DRIVEN.exec(title)?.[1] ?? title,
        expectedStatus: row.expectedStatus,
        file: row.file,
        title,
      };
    });
}

// The listing is a read-only subprocess that starts no server and no browser, so it starts here
// rather than in the first test that awaits it, and its rejection waits for that test rather
// than being reported as unhandled before it runs (`retained`).
const drivers = retained(
  listing(import.meta.url, "e2e/playwright.config.ts", {}, READING_LISTING_ARGUMENTS).then(
    (report) => driversOf(rowsOfListing(report))
  )
);

// A listing that read no reading spec would leave every rule below over nothing: the rendered
// rows would each report no driver, but the rules that read the drivers would pass.
test(
  "the listing reads the reading specs",
  async () => {
    expect((await drivers).length).toBeGreaterThan(0);
  },
  rowLimit("listing")
);

test.each(DRIVEN_IDS.map((driven) => [driven] as const))(
  "the rendered surface %s has one driver",
  async (driven) => {
    const files = (await drivers)
      .filter((driver) => driver.drives === driven)
      .map((driver) => driver.file);
    expect(
      files.length === 1
        ? driven
        : `${driven} has ${files.length} drivers in the reading specs: ${files.join(", ") || "none"}`
    ).toBe(driven);
  },
  rowLimit("listing")
);

test(
  "every driver names a rendered row of the reading list",
  async () => {
    const stray = (await drivers)
      .filter((driver) => !DRIVEN_IDS.includes(driver.drives))
      .map((driver) => `${driver.file}: ${driver.title}`);
    expect(stray).toEqual([]);
  },
  rowLimit("listing")
);

test(
  "Playwright runs every driver",
  async () => {
    // A driver marked `fixme` or `skip` is listed and never run, and one marked `fail` passes
    // only while its surface is wrong: none of them reads its surface.
    const unrun = (await drivers)
      .filter((driver) => driver.expectedStatus !== "passed")
      .map((driver) => `${driver.file}: ${driver.title} (expected ${driver.expectedStatus})`);
    expect(unrun).toEqual([]);
  },
  rowLimit("listing")
);

test("every row the list does not render says why", () => {
  const silent = DISPATCH_READING_SURFACES.filter(
    (row) => !row.rendered && (row.reason ?? "").trim() === ""
  ).map((row) => row.surface);
  expect(silent).toEqual([]);
});

test.each(
  DISPATCH_READING_SURFACES.filter((row) => row.rendered && !row.record).map(
    (row) => [row.surface, row] as const
  )
)(
  "the rendered surface %s, whose text is no record, says it carries no question id",
  (surface, row) => {
    expect(
      row.cannotCarry?.forms.includes("question-id") === true
        ? surface
        : `${surface} draws no record and does not say it cannot carry a question id (cannotCarry)`
    ).toBe(surface);
  }
);

test("every row that names a form it cannot carry says why", () => {
  const silent = DISPATCH_READING_SURFACES.flatMap((row) =>
    [
      { at: row.surface, cannotCarry: row.cannotCarry },
      ...(row.arms ?? []).map((arm) => ({
        at: `${row.surface}/${arm.arm}`,
        cannotCarry: arm.cannotCarry,
      })),
    ].filter(
      ({ cannotCarry }) =>
        cannotCarry !== undefined &&
        (cannotCarry.forms.length === 0 || cannotCarry.why.trim() === "")
    )
  ).map(({ at }) => at);
  expect(silent).toEqual([]);
});

// An arm is a kind of record a rendered surface shows, held by a driver of its own: on a row the
// list does not render, nothing would drive it.
test("every arm is an arm of a rendered row", () => {
  const undriven = DISPATCH_READING_SURFACES.filter(
    (row) => !row.rendered && (row.arms ?? []).length > 0
  ).map((row) => row.surface);
  expect(undriven).toEqual([]);
});

/** The repository's root, which a proof's file is named from. */
const REPOSITORY = fileURLToPath(new URL("../../../../", import.meta.url));

/** The calls that declare a test that runs: bun's and Playwright's `test`, and bun's `it`. A
 *  `test.skip`, `test.fixme` or `test.todo` declares one that never runs, and a `test.each` one
 *  whose title is a pattern rather than a test's own, so neither proves a row. */
const TEST_CALLS: readonly string[] = ["test", "it"];

/** The titles of the tests `source` declares: the first argument of each call of a
 *  `TEST_CALLS` name, written as a string. A title in a comment, or in a string no such call
 *  takes, declares none. */
function declaredTests(fileName: string, source: string): string[] {
  const tree = ts.createSourceFile(fileName, source, ts.ScriptTarget.Latest, true);
  const titles: string[] = [];
  const visit = (node: ts.Node): void => {
    if (
      ts.isCallExpression(node) &&
      ts.isIdentifier(node.expression) &&
      TEST_CALLS.includes(node.expression.text)
    ) {
      const [title] = node.arguments;
      if (title !== undefined && ts.isStringLiteralLike(title)) titles.push(title.text);
    }
    ts.forEachChild(node, visit);
  };
  visit(tree);
  return titles;
}

/** What is wrong with `proof` as the test of a row, or undefined where its file is a test file
 *  of the repository that declares a test of its title. `read` answers a file's text, or
 *  undefined where there is none. */
function proofComplaint(
  surface: string,
  proof: DispatchReadingProof,
  read: (file: string) => string | undefined
): string | undefined {
  if (!/\.(test\.tsx?|e2e\.ts)$/u.test(proof.file)) {
    return `${surface}: its proof ${proof.file} is no unit test or browser spec`;
  }
  const source = read(proof.file);
  if (source === undefined) return `${surface}: its proof ${proof.file} does not exist`;
  return declaredTests(proof.file, source).includes(proof.test)
    ? undefined
    : `${surface}: ${proof.file} declares no test "${proof.test}"`;
}

/** A proof file's text, read from the repository, or undefined where there is no such file. */
function readRepositoryFile(file: string): string | undefined {
  const path = `${REPOSITORY}${file}`;
  return existsSync(path) ? readFileSync(path, "utf8") : undefined;
}

test("every proof a row names is a test its file declares", () => {
  const proved = DISPATCH_READING_SURFACES.flatMap((row) =>
    row.proof === undefined ? [] : [row.proof].flat().map((proof) => [row.surface, proof] as const)
  );
  // A guard on the list itself: were no row to name a proof, this rule would hold nothing.
  expect(proved.length).toBeGreaterThan(5);
  expect(
    proved.flatMap(([surface, proof]) => proofComplaint(surface, proof, readRepositoryFile) ?? [])
  ).toEqual([]);
});

test("a proof is held to a test its file declares and runs", () => {
  const file = "packages/dispatch/web/src/features/x/x.test.tsx";
  const source = [
    '// test("in a comment", () => {});',
    'describe("a block", () => {',
    '  test("a test", () => {});',
    '  it("an it", () => {});',
    "});",
    'test.skip("skipped", () => {});',
    'test.fixme("fixme", () => {});',
    'const title = "a string no call takes";',
  ].join("\n");
  const read = (asked: string) => (asked === file ? source : undefined);
  const complaint = (title: string, at = file) =>
    proofComplaint("row", { file: at, test: title }, read);
  expect(complaint("a test")).toBeUndefined();
  expect(complaint("an it")).toBeUndefined();
  for (const title of ["in a comment", "skipped", "fixme", "a string no call takes", "a block"]) {
    expect(complaint(title)).toBe(`row: ${file} declares no test "${title}"`);
  }
  expect(complaint("a test", "packages/dispatch/web/src/features/x/gone.test.tsx")).toBe(
    "row: its proof packages/dispatch/web/src/features/x/gone.test.tsx does not exist"
  );
  expect(complaint("a test", "packages/dispatch/web/src/features/x/x.tsx")).toBe(
    "row: its proof packages/dispatch/web/src/features/x/x.tsx is no unit test or browser spec"
  );
});

test(
  "the references lane runs every reading spec",
  async () => {
    const lane = new Set(referencesLaneFiles());
    const unrun = [...new Set((await drivers).map((driver) => `e2e/${driver.file}`))].filter(
      (file) => !lane.has(file)
    );
    expect(unrun).toEqual([]);
  },
  rowLimit("listing")
);
