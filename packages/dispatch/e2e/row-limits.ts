/** Every wall-clock limit of `e2e/__tests__` - every number a row or a hook of that directory
 *  stops at - read from the source of its test files, so a limit that says nothing about where
 *  it came from can be named.
 *
 * A wall-clock limit is two different things wearing one face. It is the bound of the work when
 * the row waits for the subprocesses this directory starts: the limit is then read from the
 * bounds of those children, so the row's own clock fires after theirs and the failure a reader
 * gets names the child that hung. It is a guess when it is a number someone measured once, from
 * one file run alone: the suite grows a slower shape, the number fires first, and the run fails
 * for no defect of the product. The two are told apart by one thing only - whether the limit was
 * derived or chosen - and this module is that rule's machine check.
 * `e2e/__tests__/row-limits.test.ts` is what runs it over the directory.
 *
 * THE RULE. A wall-clock limit is a call of `rowLimit` from
 * `e2e/__tests__/bounded-subprocess.ts`, or any other expression under a reason: a comment that
 * ends on the line directly above the line the limit begins on and runs to at least
 * `minimumReasonWords` words.
 *
 * - `rowLimit(...)` is the way out for a row that WAITS for bounded children. It takes the names
 *   of the children, never a number, so the limit moves when a bound moves and a name that is
 *   not a bound is a compiler error rather than a red run.
 * - The reason is the way out for the few rows and hooks that DRIVE a process rather than wait
 *   for one: a row that starts a server and stops it itself, a hook that calls a helper whose
 *   child is bounded nowhere. Nothing can derive those, so the writer says what the row drives
 *   and why that long is enough to see it.
 * - It is the shared module's `rowLimit` and no other. The scan reads the file's import
 *   declarations, so a `rowLimit` a file declares for itself does not pass, and an import under
 *   another name does.
 * - The line directly above, and nowhere else. A sentence further up the file, or beside the
 *   constant the limit is named by, is a sentence the next reader of the limit does not see; the
 *   scan reads the one line a writer cannot miss. A blank line between the comment and the limit
 *   breaks it, since that comment belongs to whatever stands above the gap, and a comment
 *   trailing the limit is the half a line that is left after it rather than a reason.
 * - The measure of a reason is `e2e/fixed-waits.ts`'s: `commentText`, `minimumReasonWords` and
 *   `reasonWords` are imported from there - stripping a comment's markers is part of that
 *   measure - so this directory holds one measure of a reason and not two. What that measure is
 *   for, and what no scan can judge about a sentence, is THE RULE of that module.
 *
 * WHAT A LIMIT IS. Four places, which is every place `bun test` reads one:
 *
 * - the third argument of a row call - `test(...)`, `it(...)`, and every spelling reached from
 *   one of those two: `test.only(...)`, `test.skip(...)`, `test.skipIf(<x>)(...)`,
 *   `test.each(<x>)(...)` and any modifier Bun adds later, each of which takes a limit there;
 * - the `timeout` of an options object, in a row's own arguments or a hook's second. Bun reads
 *   `test(title, options, body)` as well as `test(title, body, options)`, and both are read
 *   here, although `@types/bun` 1.4.2 declares only the second;
 * - the second argument of `beforeAll`, `afterAll`, `beforeEach` and `afterEach`;
 * - the argument of `setDefaultTimeout`, which no file of this directory writes today. It is
 *   read because Bun accepts it, and a limit this rule did not read is a limit outside it.
 *
 * WHAT IS NOT A LIMIT. A number that is not a row's or a hook's own. `check-slow-rows.test.ts`
 * hands `2_000` to the judge it tests and `local-instance-support.test.ts` hands `30_000` to a
 * supervisor's stop; both are arguments of ordinary calls, which none of the four places above
 * reaches. The scan parses rather than matching text, so a `test(` written inside a template
 * literal - `check-slow-rows.test.ts` holds two files' worth of them - is a string and no call
 * at all, and a row of `e2e/__tests__/row-limits.test.ts` holds that over those two files.
 *
 * WHAT IS READ. Every `*.test.ts` directly under `e2e/__tests__`, which is what `bun test`
 * collects there, with `fixtures/` left out: the controls planted there are written to be
 * refused. `suiteFiles` of `e2e/fixed-waits.ts` cannot serve, because it skips `__tests__` by
 * name and walks the whole browser suite below it.
 *
 * A call is read as a row by the NAME its callee is reached from, unlike `e2e/no-browser-rows.ts`,
 * which asks TypeScript what a callee's type is. The question there is which calls declare a row
 * of the browser suite, where a call it fails to recognise is a row that escapes the rule; the
 * question here is whether a number was derived or explained, and a name is the whole of what a
 * reader of the source sees. Reading the name also fails the safe way: anything reached from
 * `test` or `it`, and anything spelled `beforeAll`, is asked for its limit rather than skipped.
 * No type checker is needed for that, so the scan parses files and builds no program.
 *
 * `testFiles` and `limitsOfFiles` read the file system and everything else here is pure over its
 * arguments: no environment, no network, no database and no server, so any process may call it.
 */

import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import * as ts from "typescript";

import { commentText, minimumReasonWords, reasonWords } from "./fixed-waits";
import { sourceFiles } from "./source-files";

/** What states a limit: a row, a hook, or the file itself through `setDefaultTimeout`. */
export type LimitKind = "row" | "hook" | "file";

/** One wall-clock limit of one test file of `e2e/__tests__`. */
export type WallClockLimit = {
  /** The file, relative to the `e2e` directory: `__tests__/fake-envoy.test.ts`. */
  file: string;
  /** The 1-based line the limit expression begins on, which is where a reader of a refusal looks
   *  and the line a reason must end directly above. */
  line: number;
  /** The limit as it is written - `rowLimit("short")`, `90_000`, `ms` - for a refusal to quote,
   *  so the reader knows which of a file's limits is meant without opening it. */
  limit: string;
  /** Whether a row, a hook or the file states it. */
  kind: LimitKind;
  /** The row's title as written, quotes and all, and the hook's or `setDefaultTimeout`'s own
   *  name where one of those states the limit. A row called with no argument at all states the
   *  empty string. */
  title: string;
  /** The reason comment that ends on the line directly above the limit, its markers stripped and
   *  its lines joined by a space; the empty string when no comment ends there. */
  reason: string;
  /** Whether the limit is a call of the `rowLimit` the file imports from
   *  `e2e/__tests__/bounded-subprocess.ts`, which is a limit read from the bounds of the children
   *  the row waits for and needs no reason of its own. */
  fromBounds: boolean;
};

/** A limit the rule refuses, with the sentence that names it and says what to do about it. */
export type RefusedLimit = {
  limit: WallClockLimit;
  why: string;
};

/** The directory this rule is about, under the `e2e` directory every path here is read against. */
const testsDirectory = "__tests__";

/** Every `*.test.ts` directly under `e2e/__tests__`, relative to `directory` and sorted. The walk
 *  goes down that one directory and no other, so `fixtures/` is left where it stands, and the
 *  filter at the end keeps what is under it: a `*.test.ts` beside the browser suite, were one
 *  ever written, is no test of this directory's. */
export function testFiles(directory: string): string[] {
  return sourceFiles(directory, {
    directory: (name) => name === testsDirectory,
    file: (name) => name.endsWith(".test.ts"),
  }).filter((file) => file.startsWith(`${testsDirectory}/`));
}

/** The module that holds `rowLimit`, relative to the `e2e` directory. A test file reaches it by
 *  `./bounded-subprocess` and a fixture by `../bounded-subprocess`, so the scan compares the
 *  resolved path rather than the specifier's text. */
const boundsModule = `${testsDirectory}/bounded-subprocess`;

/** The export a limit read from the bounds is a call of. */
const boundsExport = "rowLimit";

/** The names a file calls `rowLimit` by, read from its imports: the local name of the export
 *  however it was renamed, and the name of a namespace a `rowLimit` member of which is the same
 *  export. A file that imports neither calls `rowLimit` nowhere, whatever it declares. */
type LimitNames = {
  /** Bare names a call of which is a limit read from the bounds. */
  readonly bare: ReadonlySet<string>;
  /** Names a `rowLimit` member of which is that export, reached through `import * as`. */
  readonly namespaces: ReadonlySet<string>;
};

/** What `sourceFile` calls `rowLimit` by, read from its import declarations. `modulePath` is the
 *  bounds module's path with no extension, which is what a relative specifier resolves to. */
function limitNames(sourceFile: ts.SourceFile, modulePath: string): LimitNames {
  const bare = new Set<string>();
  const namespaces = new Set<string>();
  for (const statement of sourceFile.statements) {
    if (!ts.isImportDeclaration(statement)) continue;
    const specifier = statement.moduleSpecifier;
    if (!ts.isStringLiteralLike(specifier) || !specifier.text.startsWith(".")) continue;
    const imported = resolve(dirname(sourceFile.fileName), specifier.text.replace(/\.ts$/, ""));
    if (imported !== modulePath) continue;
    const bindings = statement.importClause?.namedBindings;
    if (bindings === undefined) continue;
    if (ts.isNamespaceImport(bindings)) {
      namespaces.add(bindings.name.text);
      continue;
    }
    for (const element of bindings.elements) {
      // `rowLimit as bound` carries its export name in `propertyName`; `rowLimit` alone carries
      // it in `name`, which is then the local name as well.
      const exported = element.propertyName?.text ?? element.name.text;
      if (exported === boundsExport) bare.add(element.name.text);
    }
  }
  return { bare, namespaces };
}

/** Whether `limit` is a call of the shared `rowLimit`, however it was reached: under the name it
 *  was imported by, or through a namespace. A function of the file's own that happens to be
 *  called `rowLimit` is no call of this export and is asked for a reason like any other
 *  expression. */
function fromBounds(limit: ts.Expression, names: LimitNames): boolean {
  if (!ts.isCallExpression(limit)) return false;
  const callee = limit.expression;
  if (ts.isPropertyAccessExpression(callee)) {
    return (
      callee.name.text === boundsExport &&
      ts.isIdentifier(callee.expression) &&
      names.namespaces.has(callee.expression.text)
    );
  }
  return ts.isIdentifier(callee) && names.bare.has(callee.text);
}

/** The names a row is declared by here. `bun test` exports one function under two of them. */
const rowNames = new Set(["test", "it"]);

/** The hooks that take a limit as their second argument. */
const hookNames = new Set(["beforeAll", "afterAll", "beforeEach", "afterEach"]);

/** The call that gives every row of a file a limit at once. */
const fileLimitName = "setDefaultTimeout";

/** Whether `callee` names a row: `test` and `it`, and everything reached from one of them -
 *  `test.only(...)`, `test.skip(...)`, `test.todo(...)`, `test.failing(...)`, and the functions
 *  `test.skipIf(<x>)` and `test.each(<x>)` answer - since Bun reads a limit in the same place in
 *  all of them.
 *
 *  The callee is walked down to its root identifier: a call expression to its own callee, a
 *  property access to its own object, until an identifier is left. That reads every modifier at
 *  once, the ones Bun adds after this is written included, and it keeps the reading failing the
 *  safe way - anything reached from `test` or `it` is asked for a reason rather than skipped. */
function isRowCallee(callee: ts.Expression): boolean {
  let node: ts.Expression = callee;
  while (!ts.isIdentifier(node)) {
    if (ts.isCallExpression(node)) node = node.expression;
    else if (ts.isPropertyAccessExpression(node)) node = node.expression;
    else return false;
  }
  return rowNames.has(node.text);
}

/** The `timeout` of an options object, however it is written: `{ timeout: 90_000 }`, and the
 *  shorthand `{ timeout }`, whose limit is the name itself. */
function timeoutOf(options: ts.ObjectLiteralExpression): ts.Expression | undefined {
  for (const property of options.properties) {
    const name = property.name;
    if (name === undefined || !ts.isIdentifier(name) || name.text !== "timeout") continue;
    if (ts.isPropertyAssignment(property)) return property.initializer;
    if (ts.isShorthandPropertyAssignment(property)) return property.name;
  }
  return undefined;
}

/** The limit an argument states, where that argument is a place a limit is written: the `timeout`
 *  of an options object, and the expression itself otherwise. A function is the row's body and
 *  states nothing, because Bun reads `test(title, body, options)` and `test(title, options,
 *  body)` alike. */
function limitOf(argument: ts.Expression | undefined): ts.Expression | undefined {
  if (argument === undefined) return undefined;
  if (ts.isArrowFunction(argument) || ts.isFunctionExpression(argument)) return undefined;
  if (ts.isObjectLiteralExpression(argument)) return timeoutOf(argument);
  return argument;
}

/** What one call states: nothing at all where it is no row, hook or `setDefaultTimeout`; the kind
 *  and the name a refusal uses, and the limit expressions, where it is one. */
type StatedLimits = {
  readonly kind: LimitKind;
  readonly title: string;
  readonly limits: readonly ts.Expression[];
};

/** The limits `call` states, read from the places THE RULE names and from no other argument. */
function statedLimits(call: ts.CallExpression): StatedLimits | undefined {
  const callee = call.expression;
  const limits: ts.Expression[] = [];
  const state = (limit: ts.Expression | undefined): void => {
    if (limit !== undefined) limits.push(limit);
  };
  if (isRowCallee(callee)) {
    const options = call.arguments.at(1);
    if (options !== undefined && ts.isObjectLiteralExpression(options)) state(timeoutOf(options));
    state(limitOf(call.arguments.at(2)));
    return { kind: "row", title: call.arguments.at(0)?.getText() ?? "", limits };
  }
  if (!ts.isIdentifier(callee)) return undefined;
  if (hookNames.has(callee.text)) {
    state(limitOf(call.arguments.at(1)));
    return { kind: "hook", title: callee.text, limits };
  }
  if (callee.text === fileLimitName) {
    state(limitOf(call.arguments.at(0)));
    return { kind: "file", title: callee.text, limits };
  }
  return undefined;
}

/** The reason standing above a limit: every comment of an unbroken run that ends on the line
 *  directly above `limitLine`, read as one sentence. A run is unbroken when each comment ends on
 *  the line before the next one begins, so `//` lines written together are one reason and a blank
 *  line between them ends it.
 *
 * Where a comment above a limit is written down is whatever begins the limit's own line: the
 * limit itself where it stands on a line of its own, the property where the limit is a
 * `timeout:`, and the row's whole statement where the row is written on one line. So the run is
 * read from the limit and from each ancestor that begins on the limit's line, and nothing that
 * stands above an earlier line of the same statement is read at all. */
function reasonAbove(sourceFile: ts.SourceFile, limit: ts.Expression, limitLine: number): string {
  const text = sourceFile.getFullText();
  const lineOf = (position: number): number =>
    sourceFile.getLineAndCharacterOfPosition(position).line;
  const ranges = new Map<number, ts.CommentRange>();
  const read = (node: ts.Node): void => {
    for (const range of ts.getLeadingCommentRanges(text, node.getFullStart()) ?? []) {
      ranges.set(range.pos, range);
    }
  };
  read(limit);
  for (
    let node: ts.Node = limit.parent;
    !ts.isSourceFile(node) && lineOf(node.getStart(sourceFile)) === limitLine;
    node = node.parent
  ) {
    read(node);
  }
  const sorted = [...ranges.values()].sort((first, second) => first.pos - second.pos);
  const reason: string[] = [];
  let endsOn = limitLine - 1;
  for (let index = sorted.length - 1; index >= 0; index -= 1) {
    const range = sorted[index];
    if (lineOf(range.end - 1) !== endsOn) break;
    reason.unshift(commentText(text.slice(range.pos, range.end)));
    endsOn = lineOf(range.pos) - 1;
  }
  return reason.join(" ").trim();
}

/** Every wall-clock limit the given files state, each reported under the path it was named by:
 *  `testFiles`' path relative to `directory`.
 *
 * The walk is over the syntax tree rather than the text, because the text cannot tell a row from
 * the word in a comment or in a string - `check-slow-rows.test.ts` holds whole spec files inside
 * template literals - and because the comment above a limit is trivia the parser already has. */
export function limitsOfFiles(directory: string, files: readonly string[]): WallClockLimit[] {
  const modulePath = join(directory, boundsModule);
  const limits: WallClockLimit[] = [];
  for (const file of files) {
    const path = join(directory, file);
    const sourceFile = ts.createSourceFile(
      path,
      readFileSync(path, "utf8"),
      ts.ScriptTarget.Latest,
      true
    );
    const names = limitNames(sourceFile, modulePath);
    const visit = (node: ts.Node): void => {
      const stated = ts.isCallExpression(node) ? statedLimits(node) : undefined;
      if (stated !== undefined) {
        for (const limit of stated.limits) {
          const line = sourceFile.getLineAndCharacterOfPosition(limit.getStart(sourceFile)).line;
          limits.push({
            file,
            line: line + 1,
            limit: limit.getText(sourceFile),
            kind: stated.kind,
            title: stated.title,
            reason: reasonAbove(sourceFile, limit, line),
            fromBounds: fromBounds(limit, names),
          });
        }
      }
      ts.forEachChild(node, visit);
    };
    ts.forEachChild(sourceFile, visit);
  }
  return limits;
}

/** How a refusal names what states the limit, which is what the reader opens the file for. */
function statedBy(limit: WallClockLimit): string {
  if (limit.kind === "row") return `the row ${limit.title}`;
  if (limit.kind === "hook") return `the hook ${limit.title}`;
  return `the file's own ${limit.title}`;
}

/** The two ways out, which every refusal ends with, written about whatever states the limit: a
 *  hook waits for children and drives processes as a row does, and `setDefaultTimeout` stands
 *  for every row of its file. */
function remedyFor(limit: WallClockLimit): string {
  const subject = limit.kind === "file" ? "every row of the file" : `the ${limit.kind}`;
  return (
    `call \`rowLimit(...)\` naming the bounded children ${subject} waits for, so the limit is ` +
    `read from their bounds and moves when they move; or, where ${subject} drives a process ` +
    "rather than waiting for one, give the limit a line of its own and write the sentence that " +
    "says what it drives and why that long is enough as a comment on the line directly above it."
  );
}

/** The limits the rule refuses, each with the sentence a reader of the failure acts on: which
 *  file and line, which row or hook, what the limit says, what stands above it, and the two ways
 *  out. */
export function refusedLimits(limits: readonly WallClockLimit[]): RefusedLimit[] {
  const refused: RefusedLimit[] = [];
  for (const limit of limits) {
    if (limit.fromBounds) continue;
    const remedy = remedyFor(limit);
    const at = `${limit.file}:${limit.line} ${statedBy(limit)} stops at ${limit.limit}`;
    if (limit.reason === "") {
      refused.push({
        limit,
        why: `${at}, which is no call of \`rowLimit\` and has no reason on the line above it: ${remedy}`,
      });
      continue;
    }
    const words = reasonWords(limit.reason);
    if (words.length < minimumReasonWords) {
      refused.push({
        limit,
        why:
          `${at} under "${limit.reason}", which is ${words.length} ` +
          `${words.length === 1 ? "word" : "words"} where the rule asks for ` +
          `${minimumReasonWords}: a line that short names the limit rather than saying why it is ` +
          `this long, so ${remedy}`,
      });
    }
  }
  return refused;
}
