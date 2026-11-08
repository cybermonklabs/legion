/** Every row of the browser suite, read from the source of the `*.e2e.ts` files themselves, so a
 *  row that drives no browser can be named.
 *
 * `packages/dispatch/AGENTS.md`'s rule is that a row which drives no browser does not belong in
 * this suite: it takes no page fixture, it runs in one project because every other would repeat
 * it, and it makes the browser run wait for work no browser does. Its home is the layer that owns
 * the claim. This module is that rule's machine check, and
 * `e2e/__tests__/no-browser-rows.test.ts` is what runs it over the real suite.
 *
 * A row is read from its source rather than from `playwright test --list` because the listing
 * names no fixtures: it reports a file and a title, and whether the callback destructures `page`
 * is the whole of what the rule asks. The test still reads the listings, to prove this walk sees
 * the suite Playwright runs, and what it compares against them is the FILE SET, never a row
 * count: a row generated in a loop is one `test(` call in this module's reading and as many rows
 * in the listing as the loop has turns, and one `test(` call is as many listed rows again as
 * there are projects that run the file. The set of files is the one thing the two agree on
 * exactly.
 *
 * Every function here reads text and nothing else: no environment, no network, no database and no
 * server, so any process may call it.
 */

import { readdirSync } from "node:fs";
import { join } from "node:path";
import * as ts from "typescript";

/** One `test(` call of one spec file. */
export type SuiteRow = {
  /** The spec file, relative to the `e2e` directory, as a Playwright listing reports it:
   *  `search.e2e.ts`, and `sub/dir/x.e2e.ts` for a spec in a subdirectory. */
  file: string;
  /** The 1-based line the `test(` call starts on, for a reader of a failure. Nothing matches on
   *  it: the old rule's `:723` citation rotted in four places in six days. */
  line: number;
  /** The title's source text as written, quotes and all: `"a row title"` for the usual string
   *  literal, and a template literal keeps its backticks and its `${…}`, which is a title no
   *  reader of the source alone can evaluate. */
  title: string;
  /** The fixtures the callback's first parameter destructures, by the name Playwright knows them
   *  by: the property name where one is renamed, so `{ page: p }` names `page`. A callback whose
   *  first parameter is no object binding pattern, or that takes none, binds nothing. */
  fixtures: string[];
};

/** A row that drives no browser and is allowed to stay, named by `file` and `title` exactly as
 *  `SuiteRow` reports them — the title including its quotes or backticks. Never by line: a line
 *  moves under every edit above it. */
export type Exemption = {
  file: string;
  title: string;
  /** Why this row stays in a browser suite, for the reader who finds it here later. */
  reason: string;
};

/** An exemption that has outlived what it excuses, with the sentence that says which way. */
export type StaleExemption = {
  exemption: Exemption;
  /** Why this entry excuses nothing: it names a row the suite no longer holds, or a row that
   *  drives a browser now. */
  why: string;
};

/** The rows that drive no browser and are allowed to stay.
 *
 * An entry is for a row whose claim no other layer can hold yet, and it is a debt, not a pattern:
 * the rule's answer is to move the row to the layer that owns its claim (a bun test under
 * `e2e/__tests__/` for a harness module of this directory, that package's own tests for a
 * command, a Go test for a claim about the server), not to list it here. Write one as
 *
 *     { file: "<the spec>.e2e.ts", title: '"<the title as the source writes it>"', reason: "…" }
 *
 * where `title` is the title's source text, quotes included, since that is what `rowsOfSource`
 * reads. `staleExemptions` refuses an entry that has outlived what it excuses — one naming a row
 * the suite no longer holds, or one naming a row that now drives a browser — so this list cannot
 * quietly grow stale.
 *
 * It ships empty: every row of the suite drives a browser.
 */
export const exemptions: readonly Exemption[] = [];

/** The callees that declare a row. `test.describe`, `test.step` and `test.use` declare none, and
 *  `test.skip(condition, reason)` inside a row body is a skip, not a row: it is left out by the
 *  first-argument and callback tests in `rowOfCall` rather than by its callee. */
const rowCallees = new Set([
  "test",
  "test.skip",
  "test.only",
  "test.fixme",
  "test.fail",
  "test.slow",
]);

/** The fixtures that are a browser. Playwright's `request` is not one of them: it is
 *  Playwright's HTTP client, which opens no page. */
const browserFixtures = new Set(["browser", "context", "page"]);

/** `test` or `test.skip` for a callee written that way, and nothing for any other expression. */
function calleeName(expression: ts.Expression): string | undefined {
  if (ts.isIdentifier(expression)) return expression.text;
  if (
    ts.isPropertyAccessExpression(expression) &&
    ts.isIdentifier(expression.expression) &&
    ts.isIdentifier(expression.name)
  ) {
    return `${expression.expression.text}.${expression.name.text}`;
  }
  return undefined;
}

/** The row's callback: the first argument that is a function, which both an arrow function and a
 *  function expression carry a body for. A row may take a details object between its title and
 *  its callback (`test(title, { tag }, callback)`), so this reads the arguments rather than one
 *  position. */
function rowCallback(
  call: ts.CallExpression
): ts.ArrowFunction | ts.FunctionExpression | undefined {
  for (const argument of call.arguments) {
    if (ts.isArrowFunction(argument) || ts.isFunctionExpression(argument)) return argument;
  }
  return undefined;
}

/** The fixture names a callback's first parameter binds. */
function fixturesOfCallback(callback: ts.ArrowFunction | ts.FunctionExpression): string[] {
  const [parameter] = callback.parameters;
  if (parameter === undefined || !ts.isObjectBindingPattern(parameter.name)) return [];
  const fixtures: string[] = [];
  for (const element of parameter.name.elements) {
    // `...rest` names no fixture of its own: it is whatever is left.
    if (element.dotDotDotToken !== undefined) continue;
    const bound = element.propertyName ?? element.name;
    if (ts.isIdentifier(bound) || ts.isStringLiteral(bound)) fixtures.push(bound.text);
  }
  return fixtures;
}

/** The row `call` declares, or nothing when it declares none. */
function rowOfCall(
  call: ts.CallExpression,
  sourceFile: ts.SourceFile,
  file: string
): SuiteRow | undefined {
  const callee = calleeName(call.expression);
  if (callee === undefined || !rowCallees.has(callee)) return undefined;
  const [title] = call.arguments;
  // A title is a string literal or a template literal; `test.skip(test.info()…, "reason")` inside
  // a body leads with a condition and declares no row.
  if (title === undefined || !(ts.isStringLiteralLike(title) || ts.isTemplateExpression(title))) {
    return undefined;
  }
  const callback = rowCallback(call);
  if (callback === undefined) return undefined;
  return {
    file,
    line: sourceFile.getLineAndCharacterOfPosition(call.getStart(sourceFile)).line + 1,
    title: title.getText(sourceFile),
    fixtures: fixturesOfCallback(callback),
  };
}

/** Every row `source` declares, reported under `file`. The walk is over the TypeScript syntax
 *  tree because a line regex reads this suite wrong: the same patterns run over the text return
 *  25 candidates, of which 21 are multi-line `test.skip(` calls inside a row body and no rows at
 *  all. */
export function rowsOfSource(file: string, source: string): SuiteRow[] {
  // `setParentNodes` is false: nothing here reads a node's `.parent`. The walk is
  // `ts.forEachChild`, and both position reads pass the source file (`call.getStart(sourceFile)`,
  // `title.getText(sourceFile)`), so attaching a parent to every node of 149 files would roughly
  // double this parse for no reader.
  const sourceFile = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, false);
  const rows: SuiteRow[] = [];
  const visit = (node: ts.Node): void => {
    if (ts.isCallExpression(node)) {
      const row = rowOfCall(node, sourceFile, file);
      if (row !== undefined) rows.push(row);
    }
    ts.forEachChild(node, visit);
  };
  ts.forEachChild(sourceFile, visit);
  return rows;
}

/** Every `*.e2e.ts` file under `directory`, relative to it and sorted. The walk is recursive
 *  because Playwright's is: `testDir: "."` with `testMatch: /.*\.e2e\.ts/`
 *  (`e2e/playwright.config.ts`) takes every matching file below the `e2e` directory, so a spec in
 *  a subdirectory is in the suite as surely as one beside the config. None is today.
 *  `node_modules` is skipped, as Playwright's own default ignores it, and `test-results` with it:
 *  that is the `outputDir` the config sets (`outputDir: "test-results"`,
 *  e2e/playwright.config.ts:421), resolved beside the config, so after a browser run it is a tree
 *  of traces, videos and screenshots holding no spec. */
export function specFiles(directory: string): string[] {
  const files: string[] = [];
  const walk = (relative: string): void => {
    for (const entry of readdirSync(join(directory, relative), { withFileTypes: true })) {
      const path = relative === "" ? entry.name : `${relative}/${entry.name}`;
      if (entry.isDirectory()) {
        if (entry.name !== "node_modules" && entry.name !== "test-results") walk(path);
      } else if (entry.isFile() && entry.name.endsWith(".e2e.ts")) {
        files.push(path);
      }
    }
  };
  walk("");
  return files.sort();
}

/** Whether the row opens a browser: it takes `page`, `browser` or `context`. */
function drivesBrowser(row: SuiteRow): boolean {
  return row.fixtures.some((fixture) => browserFixtures.has(fixture));
}

/** What an exemption and a row are matched on. */
function rowKey(file: string, title: string): string {
  return `${file}\n${title}`;
}

/** The rows that drive no browser and stand on no exemption: the rows the rule refuses. */
export function unexplainedRows(
  rows: readonly SuiteRow[],
  exemptions: readonly Exemption[]
): SuiteRow[] {
  const exempted = new Set(exemptions.map((exemption) => rowKey(exemption.file, exemption.title)));
  return rows.filter((row) => !drivesBrowser(row) && !exempted.has(rowKey(row.file, row.title)));
}

/** The exemptions that have outlived what they excuse, each with the sentence that says which of
 *  the two it is: it names a row the suite no longer holds, or a row that now drives a browser.
 *  Either way the entry excuses nothing and belongs out of the list. */
export function staleExemptions(
  rows: readonly SuiteRow[],
  exemptions: readonly Exemption[]
): StaleExemption[] {
  const byKey = new Map<string, SuiteRow[]>();
  for (const row of rows) {
    const key = rowKey(row.file, row.title);
    const found = byKey.get(key);
    if (found === undefined) byKey.set(key, [row]);
    else found.push(row);
  }
  const stale: StaleExemption[] = [];
  for (const exemption of exemptions) {
    const matched = byKey.get(rowKey(exemption.file, exemption.title));
    if (matched === undefined) {
      stale.push({
        exemption,
        why:
          `no row of ${exemption.file} is titled ${exemption.title}, so this exemption names a ` +
          "row the suite no longer holds",
      });
    } else if (matched.every(drivesBrowser)) {
      stale.push({
        exemption,
        why:
          `${exemption.file} ${exemption.title} drives a browser now, so this exemption excuses ` +
          "a row that no longer needs it",
      });
    }
  }
  return stale;
}
