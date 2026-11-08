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
 * Which calls are rows is TypeScript's answer too, and it is read off a type rather than off
 * the text of the callee. A callee whose type is @playwright/test's `TestType` declares a row;
 * so does a callee whose type is the type of one of the five members of `TestType` that carry
 * a title-first form — `only`, `skip`, `fixme`, `fail` and `fail.only` — which the scan keys by
 * the symbol of each member's own declared type. So every spelling of a `test` declares its
 * rows — `test`, an import renamed to `it`, `pw.test` through a namespace, a `const` bound to
 * one, `test["skip"]`, `(test)(…)`, and the `test.extend(…)` Playwright documents for adding a
 * fixture — and so does a member of one however it was reached: `test.only`, `const only =
 * test.only`, `const { skip } = test`. `test.describe`, `test.step`, `test.use` and `test.slow`
 * declare none of them, and neither do `describe.only` and `step.skip`, which are declarations
 * of their own and so other symbols than `test.only` and `test.skip`.
 *
 * What a name in a row's callback position stands for is TypeScript's own answer, not this
 * module's: `rowsOfFiles` builds one `ts.Program` over the files it scans and asks
 * `checker.getSymbolAtLocation` what each name is bound to. So a name bound by a `for`-of
 * `const`, a `let`, a parameter, a destructured binding or an import is read as that binding and
 * never as a same-named function declared elsewhere in the file, which is the one way a reading
 * of this kind can report fixtures a row does not take. A name bound to a function declaration or
 * to a function-valued `const` — in the spec, or through `getAliasedSymbol` in the module it is
 * imported from — gives the row the fixtures that function's first parameter binds.
 *
 * Every other callback is reported with no fixtures, and the row with it: a `let`, a parameter,
 * `helpers.body`, `makeBody()`, a name whose declaration the program does not hold. The check
 * fails closed there, because fixtures it did not read are not fixtures it saw, and the row
 * stands in `unexplainedRows` until someone moves it or writes it an exemption.
 *
 * `specFiles` and `rowsOfFiles` read the file system — the program reads every module its root
 * files import — and everything else here is pure over its arguments: no environment, no
 * network, no database and no server, so any process may call it.
 */

import { readdirSync } from "node:fs";
import { dirname, join } from "node:path";
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
   *  first parameter is no object binding pattern, or that takes none, binds nothing, and so does
   *  a callback this module could not read. */
  fixtures: string[];
};

/** A row that drives no browser and is allowed to stay, named by `file` and `title` exactly as
 *  `SuiteRow` reports them — the title including its quotes or backticks. Never by line: a line
 *  moves under every edit above it.
 *
 * `file` and `title` are the whole of the key, and a title is source text rather than the string
 * Playwright prints, so rows titled from one expression are one key between them: two calls in a
 * file that both pass `computedTitle`, and a call in a loop whose title is `` `…${landed}…` ``,
 * which Playwright runs once a turn. One entry excuses every row that shares its key, and
 * `staleExemptions` calls the entry stale only when they all drive a browser. Where that is more
 * than the row deserves, give the rows titles the source tells apart and write one entry each. */
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
 * where `title` is the title's source text, quotes included, since that is what `rowsOfFiles`
 * reads. `staleExemptions` refuses an entry that has outlived what it excuses — one naming a row
 * the suite no longer holds, or one naming a row that now drives a browser — so this list cannot
 * quietly grow stale.
 *
 * It ships empty: every row of the suite drives a browser.
 */
export const exemptions: readonly Exemption[] = [];

/** The fixtures that are a browser. Playwright's `request` is not one of them: it is
 *  Playwright's HTTP client, which opens no page. */
const browserFixtures = new Set(["browser", "context", "page"]);

/** What a callee of a call declares, by the forms @playwright/test 1.64.1 (`types/test.d.ts`)
 *  gives `TestType`. */
type RowCallee =
  /** `test` itself, its `only` and its `fail.only` — the one modifier with an `only` of its own.
   *  Each takes `(title, body)` and `(title, details, body)` and nothing else, so a call of one
   *  of them with a title and an argument after it declares a row. */
  | "row"
  /** `skip`, `fixme` and `fail`, which take those two title forms as well as three that declare
   *  no row — `()`, `(condition, description?)` and `(callback, description?)` — and mark the
   *  row the call stands in instead. A call of one of these with three arguments or more is a
   *  row whatever its arguments say, since the three marking forms take two at most; of a
   *  two-argument call, a row is the one whose second argument can be a body at all. */
  | "conditional";

/** The symbol of @playwright/test's `TestType`, the interface it gives `test` and everything
 *  `test.extend` returns, resolved out of the module a spec of `directory` imports.
 *
 * This is what a row callee is recognised by, rather than the text of the call: `test` under
 * another name declares its rows as plainly as `test` does, and a reading that matched the text
 * saw none of them. A scan that could not find the interface would read no row of any file and
 * so pass every rule, which is an error rather than an empty result. */
function testTypeSymbol(program: ts.Program, directory: string): ts.Symbol {
  const checker = program.getTypeChecker();
  // Only the directory of the containing path is read, so this names no file that must exist.
  const resolved = ts.resolveModuleName(
    "@playwright/test",
    join(directory, "index.ts"),
    program.getCompilerOptions(),
    ts.sys
  );
  const path = resolved.resolvedModule?.resolvedFileName;
  const moduleFile = path === undefined ? undefined : program.getSourceFile(path);
  const moduleSymbol =
    moduleFile === undefined ? undefined : checker.getSymbolAtLocation(moduleFile);
  const exported =
    moduleSymbol === undefined
      ? undefined
      : checker.getExportsOfModule(moduleSymbol).find((symbol) => symbol.name === "TestType");
  if (exported === undefined) {
    throw new Error(
      "@playwright/test's TestType is no type of this scan's program, so no row could be read"
    );
  }
  return (exported.flags & ts.SymbolFlags.Alias) !== 0
    ? checker.getAliasedSymbol(exported)
    : exported;
}

/** What each member of a `TestType` that carries a title-first form declares, keyed by the
 *  symbol of that member's own type: `only`, `skip`, `fixme` and `fail` of `TestType` itself,
 *  and the `only` of the type of its `fail`.
 *
 * A member is keyed by its type rather than by the name it is written under because a member
 * bound to another name is the member it came from, which a reading of the written name cannot
 * see: `const only = test.only` and `const { skip } = test` declare rows under names that say
 * nothing, and Playwright binds those functions, so the calls run. The symbol of a member's
 * type is the declaration it was written as, so `test.fail.only`, `describe.only` and
 * `step.skip` are three symbols rather than the two names they share, and only the first is a
 * key here.
 *
 * A scan that keyed fewer than five would let a row declared by a missing member pass unread,
 * which is an error rather than a smaller map. */
function rowCalleeMembers(checker: ts.TypeChecker, testType: ts.Symbol): Map<ts.Symbol, RowCallee> {
  const members = new Map<ts.Symbol, RowCallee>();
  // The key is built the way `rowCallee` looks one up — the symbol of the type the member has —
  // so that the two agree.
  const record = (owner: ts.Type, name: string, callee: RowCallee): ts.Type | undefined => {
    const member = checker.getPropertyOfType(owner, name);
    if (member === undefined) return undefined;
    const type = checker.getTypeOfSymbol(member);
    const key = type.getSymbol();
    if (key !== undefined) members.set(key, callee);
    return type;
  };
  const declared = checker.getDeclaredTypeOfSymbol(testType);
  record(declared, "only", "row");
  record(declared, "skip", "conditional");
  record(declared, "fixme", "conditional");
  const fail = record(declared, "fail", "conditional");
  if (fail !== undefined) record(fail, "only", "row");
  if (members.size !== 5) {
    throw new Error(
      "@playwright/test's TestType gives this scan no distinct type for each of only, skip, " +
        `fixme, fail and fail.only (${members.size} of 5), so a row declared by a member of a ` +
        "test could go unread"
    );
  }
  return members;
}

/** What one scan reads its rows with: the program's checker, the `TestType` symbol every row
 *  callee of that program is recognised by, and the members of that `TestType` which declare a
 *  row, keyed as `rowCalleeMembers` keys them. */
type Scan = {
  checker: ts.TypeChecker;
  testType: ts.Symbol;
  members: ReadonlyMap<ts.Symbol, RowCallee>;
};

/** Whether a type is `TestType`. A union or an intersection holding it is one as well, since a
 *  value of that type carries `TestType`'s call signatures and its members. */
function holdsTestType(type: ts.Type, testType: ts.Symbol): boolean {
  if (type.getSymbol() === testType) return true;
  return type.isUnionOrIntersection() && type.types.some((part) => holdsTestType(part, testType));
}

/** Whether `expression` is a `test`: a `TestType` however it is spelled and wherever it came
 *  from — `test`, an import renamed to `it`, `pw.test`, a `const` bound to one, or what
 *  `test.extend` returned. */
function isTestType(expression: ts.Expression, scan: Scan): boolean {
  return holdsTestType(scan.checker.getTypeAtLocation(expression), scan.testType);
}

/** What `expression`, standing in a call's callee position, declares: a row where it is a `test`
 *  itself, and a row or a marker where its type is the type of one of the five members of a
 *  `test` that carry a title-first form. Nothing for every other callee, whatever it computes.
 *
 * The member is decided by the symbol of the callee's type, so a member reached under another
 * name — `const only = test.only`, `const { skip } = test` — or written another way —
 * `test["skip"]` — is the member it was bound to, and a member of another object that shares
 * its name is not. No chain of `describe` or `step` is a row callee, however it is spelled:
 * `describe` and its `only`, `skip`, `fixme`, `serial` and `parallel` take a `() => void` block,
 * which declares rows rather than being one, and `step` and its `skip` take a step body inside a
 * row already running. Neither is a titled hook — `beforeEach`, `afterEach`, `beforeAll` and
 * `afterAll` take `(title, inner)` as well, and each runs around a row rather than declaring
 * one — nor `use`, which takes a fixture object, nor `slow`, whose three forms all mark a row
 * already declared, nor `extend`, which returns the `test` whose calls are the rows. Each of
 * those carries a type of its own and so is no key of the map. */
function rowCallee(expression: ts.Expression, scan: Scan): RowCallee | undefined {
  if (isTestType(expression, scan)) return "row";
  const member = scan.checker.getTypeAtLocation(expression).getSymbol();
  return member === undefined ? undefined : scan.members.get(member);
}

/** A function this module reads a row's fixtures from: one written in the call, or one a name in
 *  the call is bound to. */
type RowCallback = ts.ArrowFunction | ts.FunctionExpression | ts.FunctionDeclaration;

/** What one argument of a call is, as far as this module reads it. */
type RowArgument =
  /** A function, whose first parameter binds the row's fixtures. */
  | { kind: "callback"; declaration: RowCallback }
  /** Something no row can run: a title, a details object, a reason string. */
  | { kind: "value" }
  /** Something that can be a body and whose declaration this module could not read. */
  | { kind: "unreadable" };

/** The function a name is bound to, or nothing when it is bound to anything else.
 *
 * The symbol comes from the checker, so the name resolves in its own scope: a `for`-of `const`, a
 * parameter, a catch variable or a destructured binding is itself, and a name declared in a
 * `describe` callback is that declaration rather than a same-named one at the top of the file. An
 * import is a symbol of its own too, which `getAliasedSymbol` follows to what it imports, so a
 * callback declared in a module beside the spec is read there.
 *
 * A `let` or `var` is not read even where its initializer is a function: that binding can hold
 * another function by the time Playwright runs the row, so reading it would be a guess, and this
 * module reports what it cannot read rather than guessing at it. */
function boundFunction(name: ts.Identifier, checker: ts.TypeChecker): RowCallback | undefined {
  const symbol = checker.getSymbolAtLocation(name);
  if (symbol === undefined) return undefined;
  const bound =
    (symbol.flags & ts.SymbolFlags.Alias) !== 0 ? checker.getAliasedSymbol(symbol) : symbol;
  for (const declaration of bound.declarations ?? []) {
    // An overload signature declares no parameters the row runs with; its implementation does.
    if (ts.isFunctionDeclaration(declaration)) {
      if (declaration.body !== undefined) return declaration;
      continue;
    }
    if (!ts.isVariableDeclaration(declaration)) continue;
    if ((ts.getCombinedNodeFlags(declaration) & ts.NodeFlags.Const) === 0) continue;
    const initializer = declaration.initializer;
    if (
      initializer !== undefined &&
      (ts.isArrowFunction(initializer) || ts.isFunctionExpression(initializer))
    ) {
      return initializer;
    }
  }
  return undefined;
}

/** Whether an expression this module read no function out of can be a row's body at all. A title,
 *  a details object and a reason string cannot, which is what tells `test.skip(condition, reason)`
 *  from `test.skip(title, body)`; anything callable can, and so does anything the program could
 *  not work a type out for, which is read as a body this module failed to read rather than
 *  dismissed. */
function mayBeCallable(expression: ts.Expression, checker: ts.TypeChecker): boolean {
  const type = checker.getTypeAtLocation(expression);
  const constituents = type.isUnion() ? type.types : [type];
  return constituents.some(
    (part) =>
      (part.flags & (ts.TypeFlags.Any | ts.TypeFlags.Unknown)) !== 0 ||
      part.getCallSignatures().length > 0
  );
}

/** What `expression`, written as one argument of a row callee, is to this module. */
function rowArgument(expression: ts.Expression, checker: ts.TypeChecker): RowArgument {
  if (ts.isArrowFunction(expression) || ts.isFunctionExpression(expression)) {
    return { kind: "callback", declaration: expression };
  }
  if (ts.isIdentifier(expression)) {
    const declaration = boundFunction(expression, checker);
    if (declaration !== undefined) return { kind: "callback", declaration };
  }
  return mayBeCallable(expression, checker) ? { kind: "unreadable" } : { kind: "value" };
}

/** The fixture names a callback's first parameter binds. */
function fixturesOfCallback(callback: RowCallback): string[] {
  const [first, second] = callback.parameters;
  // A `this` parameter is a type annotation standing in the parameter list and no argument at
  // all: `function (this: void, { page })` binds its fixtures in the parameter after it.
  const parameter =
    first !== undefined && ts.isIdentifier(first.name) && first.name.text === "this"
      ? second
      : first;
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
 * A call of a row callee is a row whatever its title is written as, so a row titled from a
 * variable is read as surely as one titled by a literal, and its `title` is that argument's
 * source text — all a reader of the source alone can have. The body is the first argument after
 * the title that is not plainly a value, because a row may carry a details object between the two
 * (`test(title, { tag }, body)`), and the arguments after the first are what is read because a
 * function in first position is a condition rather than a body:
 * `test.skip(({ browserName }) => browserName === "webkit", "reason")` inside a row body declares
 * no row.
 *
 * That is also what leaves out the calls of `skip`, `fixme` and `fail` that mark the row they
 * stand in: `test.skip(<condition>, "<reason>")` and the four of this suite whose reason is the
 * imported `plainHttpSkipReason` of `e2e/plain-http-origin.ts` carry two arguments of which the
 * second is a string, and a string is no body.
 *
 * A row whose body this module could not read is reported with no fixtures rather than skipped,
 * because a gate a row can get past is a gap in the gate. */
function rowOfCall(
  call: ts.CallExpression,
  sourceFile: ts.SourceFile,
  file: string,
  scan: Scan
): SuiteRow | undefined {
  // Every form that declares a row gives it a title and a body, so a call of one argument or
  // none — `test.skip()`, `test.skip(<condition>)` — marks the row it stands in. Arity is read
  // before the callee because the callee costs a type and this costs a length: of the 44,242
  // calls this suite writes, 33,955 carry fewer than two arguments and ask the checker nothing.
  if (call.arguments.length < 2) return undefined;
  const callee = rowCallee(call.expression, scan);
  if (callee === undefined) return undefined;
  const [title, ...rest] = call.arguments;
  const body = rest
    .map((argument) => rowArgument(argument, scan.checker))
    .find((argument) => argument.kind !== "value");
  if (callee === "conditional" && rest.length === 1 && body === undefined) return undefined;
  return {
    file,
    line: sourceFile.getLineAndCharacterOfPosition(call.getStart(sourceFile)).line + 1,
    title: title.getText(sourceFile),
    fixtures: body?.kind === "callback" ? fixturesOfCallback(body.declaration) : [],
  };
}

/** The compiler options the scan's program is built with: the package's own `tsconfig.json`,
 *  read from the nearest one above `directory` and parsed as `tsc` parses it, so this program
 *  resolves what the package resolves.
 *
 * They are taken whole rather than picked, because an option left out is read here as a type
 * this module could not work out. Without `jsx` a `.tsx` import does not resolve (TS6142), and
 * `inbox-dismiss.e2e.ts` and `inbox-mark-all.e2e.ts` import one today, so a body declared in
 * such a module would be refused on valid source; where `bun` does not reach the program, as
 * under a `types` list that leaves it out, `process` is `any`, so
 * `test.skip(!process.env.CI, process.env.SKIP_REASON)` reads as a title and a body and is
 * reported as a row. Each of those costs a real row its fixtures or makes a marking call
 * a row, which is noise on correct code. The program is built to bind names rather than to
 * type-check the suite — no diagnostic of it is read, and `bunx tsc --noEmit` does the checking
 * from this same configuration. */
function scanOptions(directory: string): ts.CompilerOptions {
  const path = ts.findConfigFile(directory, ts.sys.fileExists);
  if (path === undefined) throw new Error(`no tsconfig.json stands above ${directory}`);
  const unusable = (failure: ts.Diagnostic): Error =>
    new Error(
      `${path} is no configuration this scan can be built from: ` +
        ts.flattenDiagnosticMessageText(failure.messageText, " ")
    );
  const read = ts.readConfigFile(path, ts.sys.readFile);
  if (read.error !== undefined) throw unusable(read.error);
  const parsed = ts.parseJsonConfigFileContent(read.config, ts.sys, dirname(path));
  const [failed] = parsed.errors;
  if (failed !== undefined) throw unusable(failed);
  return parsed.options;
}

/** Every row the given files declare, each reported under the path it was named by: `specFiles`'
 *  path relative to `directory`.
 *
 * One program is built for the whole scan. It is what resolves a name — including a name imported
 * from a module beside the spec, which is in the program because the spec imports it — and
 * building one per file would build it 150 times over this suite and resolve nothing more.
 *
 * The walk is over the syntax tree because a line regex reads this suite wrong: the same patterns
 * run over the text return 25 candidates, of which 21 are multi-line `test.skip(` calls inside a
 * row body and no rows at all. */
export function rowsOfFiles(directory: string, files: readonly string[]): SuiteRow[] {
  const program = ts.createProgram({
    rootNames: files.map((file) => join(directory, file)),
    options: scanOptions(directory),
  });
  const checker = program.getTypeChecker();
  const testType = testTypeSymbol(program, directory);
  const scan: Scan = { checker, testType, members: rowCalleeMembers(checker, testType) };
  const rows: SuiteRow[] = [];
  for (const file of files) {
    const path = join(directory, file);
    const sourceFile = program.getSourceFile(path);
    // A file the program does not hold is a file this scan did not read, and a scan that reads
    // nothing passes every rule: it is an error rather than an empty result.
    if (sourceFile === undefined) throw new Error(`${path} is no file of this scan's program`);
    const visit = (node: ts.Node): void => {
      if (ts.isCallExpression(node)) {
        const row = rowOfCall(node, sourceFile, file, scan);
        if (row !== undefined) rows.push(row);
      }
      ts.forEachChild(node, visit);
    };
    ts.forEachChild(sourceFile, visit);
  }
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
