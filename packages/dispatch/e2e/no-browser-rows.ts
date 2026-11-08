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
 * Every function here but `specFiles`, which reads the file system, is pure over its arguments:
 * no environment, no network, no database and no server, so any process may call it.
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
 *  neither does `test.slow`: @playwright/test 1.64.1 (`types/test.d.ts`) gives it three forms,
 *  `slow(): void`, `slow(condition: boolean, description?: string): void` and
 *  `slow(callback: ConditionBody<TestArgs & WorkerArgs>, description?: string): void`, each of
 *  which marks a row already declared as slow rather than declaring one.
 *  `test.skip(condition, reason)` inside a row body is a skip, not a row: it is left out by the
 *  title and callback tests in `rowOfCall` rather than by its callee. */
const rowCallees = new Set(["test", "test.skip", "test.only", "test.fixme", "test.fail"]);

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

/** The row's callback among the arguments that follow the title: the first that is a function,
 *  which both an arrow function and a function expression carry a body for. A row may take a
 *  details object between its title and its callback (`test(title, { tag }, callback)`), so this
 *  reads the arguments rather than one position. It is passed the arguments after the first
 *  because a function in first position is a condition, not a callback:
 *  `test.skip(({ browserName }) => browserName === "webkit", "reason")` inside a row body
 *  declares no row. */
function rowCallback(
  argumentsAfterTitle: readonly ts.Expression[]
): ts.ArrowFunction | ts.FunctionExpression | undefined {
  for (const argument of argumentsAfterTitle) {
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

/** The row `call` declares, or nothing when it declares none.
 *
 * A call whose callback is written inline is a row whatever its first argument is, so a row
 * titled from a variable is read as surely as one titled by a literal, and its `title` is that
 * argument's source text — all a reader of the source alone can have.
 *
 * A call with a literal title whose callback this module cannot read, because it is declared
 * beside the row and passed by name, is reported too, with no fixtures. It is reported rather
 * than skipped because a gate a row can get past is a gap in the gate: fixtures this module did
 * not read are not fixtures it saw, so the check fails closed and `unexplainedRows` names the
 * row until someone moves it or writes it an exemption. A literal title is what tells such a row
 * from `test.skip(test.info()…, "reason")` inside a row body, whose first argument is a
 * condition and no title at all; and a call whose arguments are all strings after the title is
 * `test.fixme(condition, description)` and its kin, which declare nothing either.
 */
function rowOfCall(
  call: ts.CallExpression,
  sourceFile: ts.SourceFile,
  file: string
): SuiteRow | undefined {
  const callee = calleeName(call.expression);
  if (callee === undefined || !rowCallees.has(callee)) return undefined;
  const [title, ...rest] = call.arguments;
  // Every form that declares a row names it and gives it a body, so a call of one argument or
  // none — `test.skip()`, `test.fail("this is broken")` — marks the row it stands in.
  if (title === undefined || rest.length === 0) return undefined;
  const callback = rowCallback(rest);
  if (callback === undefined) {
    const titled = ts.isStringLiteralLike(title) || ts.isTemplateExpression(title);
    if (!titled || rest.every((argument) => ts.isStringLiteral(argument))) return undefined;
  }
  return {
    file,
    line: sourceFile.getLineAndCharacterOfPosition(call.getStart(sourceFile)).line + 1,
    title: title.getText(sourceFile),
    fixtures: callback === undefined ? [] : fixturesOfCallback(callback),
  };
}

/** Every row `source` declares, reported under `file`. The walk is over the TypeScript syntax
 *  tree because a line regex reads this suite wrong: the same patterns run over the text return
 *  25 candidates, of which 21 are multi-line `test.skip(` calls inside a row body and no rows at
 *  all. */
export function rowsOfSource(file: string, source: string): SuiteRow[] {
  // `setParentNodes` is false: nothing here reads a node's `.parent`. The walk is
  // `ts.forEachChild`, and both position reads pass the source file (`call.getStart(sourceFile)`,
  // `title.getText(sourceFile)`), so attaching a parent to every node of every spec of the
  // suite would roughly double this parse for no reader.
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
 *  that is the `outputDir` the config sets (`outputDir: "test-results"` in
 *  e2e/playwright.config.ts), resolved beside the config, so after a browser run it is a tree of
 *  traces, videos and screenshots holding no spec. */
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
