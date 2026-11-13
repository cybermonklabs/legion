/** Every wall-clock limit of `e2e/__tests__` - every number a row or a hook of that directory
 *  stops at - read from the source of its test files, so a limit that says nothing about where
 *  it came from can be named, and so can a row that waits for a bounded child and states none.
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
 * `minimumReasonWords` words. A row or hook that waits for a bounded child states its limit,
 * and states it as a call of `rowLimit`: WHAT A ROW WAITS FOR below.
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
 * - The measure of a reason is `e2e/fixed-waits.ts`'s: `reasonEndingAbove`, which reads the
 *   comments that end above a line, and `shortReason`, which counts their words and says so when
 *   they fall short, are imported from there, so this directory holds one measure of a reason and
 *   not two. What that measure is for, and what no scan can judge about a sentence, is THE RULE
 *   of that module.
 *
 * WHAT A LIMIT IS. Five places, which is what this rule reads. In each of them the limit is the
 * argument itself, or the `timeout` of an options object written there:
 *
 * - the third argument of a row call - `test(...)`, `it(...)`, `xtest(...)`, `xit(...)`, and
 *   every spelling reached from one of those: `test.only(...)`, `test.skip(...)`,
 *   `test.skipIf(<x>)(...)`, `test.each(<x>)(...)` and any modifier Bun adds later, each of which
 *   takes a limit there;
 * - the second argument of a row call. Bun reads `test(title, options, body)` as well as
 *   `test(title, body, options)`, although `@types/bun` 1.4.2 declares only the second, so the
 *   second argument is a limit wherever it is no function: a number, an options object, or a name
 *   bound to either. A function in either place is the row's body: an arrow, a function
 *   expression, or the name of a function the file declares;
 * - the second argument of `beforeAll`, `afterAll`, `beforeEach`, `afterEach` and
 *   `onTestFinished`, which are every export of `bun:test` that `test.d.ts` declares with a
 *   `HookOptions` limit, in the bun-types 1.4.2 this package installs and in 1.4.4 alike;
 * - the argument of `setDefaultTimeout`, which no file of this directory writes today. It is
 *   read because Bun accepts it, and a limit this rule did not read is a limit outside it;
 * - the argument of `jest.setTimeout`, which is that same file-wide limit under Jest's
 *   spelling. Bun honours it, and the bun suites of this repository write `jest.` in dozens of
 *   files, so it is a spelling a writer here reaches for.
 *
 * WHAT A NAME IS. The names above are the export names of `bun:test`, and a file reaches an
 * export under whatever local name it bound it to. So each file is read under its own bindings
 * AS WELL AS the bare spellings:
 *
 * - its import declarations: `import { test as row }` makes `row(...)` a row,
 *   `setDefaultTimeout as cap` makes `cap(...)` the file's own limit, and `import * as bun from
 *   "bun:test"` and a default import of the module reach each export through its own name;
 * - its variables, `const`, `let` and `var` at any scope, whose initializer names a read export:
 *   `const row = test`, `const only = test.only`, `const cap = bun.setDefaultTimeout`, or a name
 *   bound the same way;
 * - its variables bound to the module itself - `const bun = await import("bun:test")`,
 *   `require("bun:test")` - which reach each export as an import namespace does, and the names it
 *   destructures off the module or off a name bound to it: `const { test: t2 } = await
 *   import("bun:test")`.
 *
 * The variables are read to a fixpoint, so a name bound to a name bound to `test` is a row. The
 * scan follows no scope: a name bound anywhere in the file is read everywhere in it, and the first
 * binding read wins, which reads a call as a row more often rather than less. The bare spellings
 * are read whatever a file binds, because `bun test` declares them as globals and a file that
 * imports nothing still writes them. This is the shape `waitNames` of `e2e/fixed-waits.ts` reads
 * `node:timers/promises` by. `importedNames` below reads `rowLimit`, `runBounded` and `listing`
 * the same two ways: from import declarations, and from a variable at any scope bound to an
 * `import()` or a `require` of their module, as a namespace or destructured -
 * `const { listing } = await import("./playwright-listing")`. Those modules are this
 * directory's own, so a bare spelling of their exports stands for nothing.
 *
 * WHAT A ROW WAITS FOR. A row or hook waits for a bounded child when its body calls `runBounded`
 * of `e2e/__tests__/bounded-subprocess.ts` or `listing` of `e2e/__tests__/playwright-listing.ts`,
 * each read from the file's imports as `rowLimit` is, or calls a function of the file's own that
 * waits: a function declaration, or a variable bound to an arrow or a function expression, at any
 * scope, whose body calls one of those two or another such function. It waits as well when its
 * body refers to a name the file binds or assigns, at any scope, to such a call: the
 * `const lane = retained(listing(...))` a listing file starts at module load and awaits in a row,
 * or the `lane = retained(listing(...))` a hook assigns to a `let lane` of the file's, referred to
 * in an `await lane`, a `lane.then(...)`, an argument or a destructuring alike; and a function
 * whose body refers to such a name waits too. A name the body declares itself is its own and not
 * that one. The name an assignment writes to is written there rather than read, so the hook that
 * assigns `lane` waits for the `listing` it calls and not for `lane`. Such a row states a limit:
 * its own, or the file's through `setDefaultTimeout` or `jest.setTimeout`, which Bun applies to
 * every row and hook of the file. A row that states none runs under the lane's
 * `--timeout 60000`, which ties with the `short` bound and falls below every other one, so the
 * row's clock fires first and the failure names no child. The limit it states is a call of
 * `rowLimit` naming the children it waits for, never a number under a reason, because the
 * reason is the way out for a row that drives a process and this row waits for one; a file's own
 * limit counts for its rows only where it is such a call.
 *
 * What that reading leaves out is said here, because a row it leaves out is a row no refusal
 * names. It reads a name BOUND OR ASSIGNED TO a call that waits, and references to that name,
 * and nothing broader. A row that waits through any of these shapes is read as waiting for
 * nothing, and no refusal names it:
 *
 * - a name bound to a waiting name - `const rows = lane.then(rowsOf)` - though a row that refers
 *   to `lane` itself is read;
 * - a name a destructuring declaration binds - `const [lane] = [listing(...)]`,
 *   `const { lane } = { lane: listing(...) }` - because only a declaration whose name is one
 *   identifier binds a name the scan reads;
 * - a property assigned - `state.lane = listing(...)` - or a destructuring assignment -
 *   `[lane] = [listing(...)]`, `({ lane } = ...)` - because only an identifier is read as
 *   assigned, by `=`, `??=`, `||=` or `&&=`, so a row that awaits `state.lane`, or that `lane`,
 *   waits for nothing here;
 * - a method of a `class`, static or not, that calls `runBounded` or `listing`: the file's own
 *   functions are its function declarations and its variables bound to an arrow or a function
 *   expression, and a call through a member - `Helpers.run()` - reaches none of them;
 * - a bounded function passed as a value - `.map(runBounded)`, `.map(listing)`, a waiting
 *   function of the file's own handed to another call - which is no call the scan sees.
 *
 * A row that waits through one of them still states `rowLimit`, because its writer knows it
 * waits. A bare reference to `runBounded` or `listing` that calls neither - the `void runBounded`
 * that `e2e/__tests__/fixtures/row-limits.fixture.ts` plants - is no wait at all, because only a
 * call starts a child.
 *
 * A name bound or assigned to an awaited call - `const options = await helpOptions()` - holds
 * what the call answered, so a reference to it waits for nothing and is not read; the binding's
 * own `await` is the wait. That holds only where the `await` is outermost in what the name is
 * bound to, under parentheses and `as` alone. Where the `await` stands deeper -
 * `const code = (await runBounded(...)).code` - the name is read as bound to the call, so it is
 * a waiting name although it holds a number, and a row that refers to `code` and states no
 * `rowLimit` is refused as a row that waits for a bounded child: a false refusal. No test file
 * of this directory writes that shape.
 *
 * WHAT A ROWLIMIT COVERS. A `rowLimit` call is checked against the bounds of what its row waits
 * for wherever the scan reads them: a `listing` call is the `listing` bound, a `runBounded` call
 * whose options object writes `bound:` as a string literal is that bound, a function of the
 * file's own is the largest bound read among the calls it waits for, and a name bound to any of
 * those is the same. The total the call states is computed by `rowLimit` itself from its
 * arguments as written, and it must reach `rowLimit` of the largest of those bounds: the largest
 * single child the row waits for, plus the headroom every row gets. That is the reading taken
 * rather than a sum of the calls, because the scan cannot tell calls one after another from
 * calls started together or from one call in a loop, and a sum would refuse
 * `rowLimit({ atOnce: [...] })` over listings started at once. So `rowLimit("short")` over a
 * `listing` is refused, and so is a file's `setDefaultTimeout(rowLimit("short"))` for each of
 * its rows that states no limit of its own and waits for a `listing`. Where the scan reads no
 * bound - a `bound:` the call computes, options passed by name - or no total - an argument that
 * is no bound's name and no `{ atOnce: [...] }` of names, such as
 * `rowLimit(...list.map(...))` - the names are not checked and the limit is taken as stated.
 *
 * WHAT IS NOT A LIMIT. A number that is not a row's or a hook's own. `check-slow-rows.test.ts`
 * hands `2_000` to the judge it tests and `local-instance-support.test.ts` hands `30_000` to a
 * supervisor's stop; both are arguments of ordinary calls, which none of the five places above
 * reaches. The scan parses rather than matching text, so a `test(` written inside a template
 * literal - `check-slow-rows.test.ts` holds two files' worth of them - is a string and no call
 * at all, and a row of `e2e/__tests__/row-limits.test.ts` holds that over those two files.
 *
 * WHAT IS READ. Every file under `e2e/__tests__`, at any depth, that the lane's
 * `bun test e2e/__tests__` collects there, and no other: `collectedTestFile` and
 * `collectedDirectory` below say which names those are and how that was measured. That is more
 * than `*.test.ts`: a `*.spec.ts`, a `*_test.ts`, a `*_spec.ts` and a `*.test.tsx` run on the
 * lane like any other test file, so each is read like one, and each is parsed as what its
 * extension says - JSX in a `.tsx` or a `.jsx`, JavaScript in a `.js` - which
 * `ts.createSourceFile` reads off the file's name. Every directory below is read but the ones
 * `bun test` skips itself, `fixtures/` included: a test file planted there runs like any other.
 * The controls of `fixtures/` are left out because their names carry no `test` or `spec` token:
 * they are `*.fixture.ts` and its kin. The walk begins at `e2e/__tests__` itself, so nothing
 * beside that directory is read: no file of those is a test file of this rule's. `suiteFiles` of
 * `e2e/fixed-waits.ts` cannot serve, because it skips `__tests__` by name and walks the whole
 * browser suite below it.
 *
 * A call is read as a row by the NAME its callee is reached from, unlike `e2e/no-browser-rows.ts`,
 * which asks TypeScript what a callee's type is. The question there is which calls declare a row
 * of the browser suite, where a call it fails to recognise is a row that escapes the rule; the
 * question here is whether a number was derived or explained, and a name is the whole of what a
 * reader of the source sees. Reading the name also fails the safe way: anything reached from
 * a name the file bound `test` or `it` to, and anything that names a hook, is asked for its
 * limit rather than skipped. No type checker is needed for that, so the scan parses files and
 * builds no program.
 *
 * `testFiles` and `limitsOfFiles` read the file system and everything else here is pure over its
 * arguments: no environment, no network, no database and no server, so any process may call it.
 * The bounds and `rowLimit` are imported from `e2e/__tests__/bounded-subprocess.ts` itself, so a
 * total is computed by the one rule that states it; loading that module starts no child, and
 * the exit and signal listeners it adds stop only children it started.
 */

import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import * as ts from "typescript";

import { type BoundName, bounds, rowLimit } from "./__tests__/bounded-subprocess";
import { reasonEndingAbove, shortReason } from "./fixed-waits";
import { sourceFiles } from "./source-files";

/** What states a limit: a row, a hook, or the file itself through `setDefaultTimeout`. */
export type LimitKind = "row" | "hook" | "file";

/** A limit as a row, a hook or a file writes it. */
export type StatedLimit = {
  /** The limit as it is written - `rowLimit("short")`, `90_000`, `ms` - for a refusal to quote,
   *  so the reader knows which of a file's limits is meant without opening it. */
  text: string;
  /** The reason comment that ends on the line directly above the limit, its markers stripped and
   *  its lines joined by a space; the empty string when no comment ends there. */
  reason: string;
  /** Whether the limit is a call of the `rowLimit` the file imports from
   *  `e2e/__tests__/bounded-subprocess.ts`, which is a limit read from the bounds of the children
   *  the row waits for and needs no reason of its own. */
  fromBounds: boolean;
  /** The milliseconds such a `rowLimit` call states, computed by `rowLimit` from its arguments as
   *  written. Undefined for a limit of any other kind, and for a `rowLimit` call with an argument
   *  that is no bound's name and no `{ atOnce: [...] }` of names, whose value a parse cannot
   *  read. */
  total: number | undefined;
};

/** One wall-clock limit of one test file of `e2e/__tests__`, or one row or hook of it that waits
 *  for a bounded child and states no limit of its own. */
export type WallClockLimit = {
  /** The file, relative to the `e2e` directory: `__tests__/fake-envoy.test.ts`. */
  file: string;
  /** The 1-based line the limit expression begins on, which is where a reader of a refusal looks
   *  and the line a reason must end directly above. For a row that states no limit, the line its
   *  call begins on. */
  line: number;
  /** The limit the row, the hook or the file writes itself. Undefined for a row or hook that
   *  waits for a bounded child and states no limit of its own, the one call that states none and
   *  is recorded at all: its limit is then the file's own `rowLimit`, where the file states one,
   *  and the lane's `--timeout` where it does not. */
  stated: StatedLimit | undefined;
  /** Whether a row, a hook or the file states it. */
  kind: LimitKind;
  /** The row's title as written, quotes and all, and the hook's or `setDefaultTimeout`'s own
   *  name where one of those states the limit. A row called with no argument at all states the
   *  empty string. */
  title: string;
  /** The bounded calls the row's or hook's body waits for, each as the file names it, in the
   *  order they are written: `runBounded`, `listing`, or a function of the file's own or a name
   *  the file binds to a waiting call, followed by the bounded export it reaches. Empty where the
   *  body waits for none, and for a file's limit. */
  waitsFor: readonly string[];
  /** The largest bound the scan reads among the children the row or hook waits for, which its
   *  limit must cover: WHAT A ROWLIMIT COVERS. Undefined where it reads none, and for a file's
   *  limit. */
  bound: BoundName | undefined;
};

/** A limit the rule refuses, with the sentence that names it and says what to do about it. */
export type RefusedLimit = {
  limit: WallClockLimit;
  why: string;
};

/** The directory this rule is about, under the `e2e` directory every path here is read against. */
const testsDirectory = "__tests__";

/** The names `bun test <directory>` collects as test files: a name that ends `.test`, `_test`,
 *  `.spec` or `_spec` and then one of the extensions `ts`, `tsx`, `js`, `jsx`, `mjs`, `cjs`, `mts`
 *  and `cts`, in any case.
 *
 *  Measured with bun 1.4.4 on 2026-11-12: a scratch directory held one trivial row per name, and
 *  `bun test <directory> --reporter=junit` listed the files that ran. All 32 of those four
 *  endings crossed with those eight extensions ran; so did `m.TEST.ts`, `m.Test.ts`,
 *  `m.SPEC.ts`, `m_TEST.ts`, `m.Spec.Ts` and `m.test.TS`, and `.test.ts` and `_test.ts`, whose
 *  name is the token alone. None of `m.fixture.ts`, `test.ts`, `spec.ts`, `xtest.ts`,
 *  `m20test.ts`, `m.tests.ts`, `m.specs.ts`, `m-test.ts`, `m.testx.ts`, `test.m.ts`,
 *  `spec_m.ts`, `m.test.foo.ts`, `m.test.d.ts`, `m.test.ts.bak`, `m.test`, `m.test.json` or
 *  `m.test.mtsx` ran. */
const collectedTestFile = /[._](?:test|spec)\.(?:ts|tsx|js|jsx|mjs|cjs|mts|cts)$/i;

/** Whether `bun test <directory>` goes down a directory of this name. The measurement of
 *  `collectedTestFile` ran a `*.test.ts` in `fixtures/`, `sub/deeper/`, `dist/`, `build/` and
 *  `__snapshots__/`, and none in `node_modules/`, `.git/` or `.hidden/`. */
function collectedDirectory(name: string): boolean {
  return name !== "node_modules" && !name.startsWith(".");
}

/** Every file under `e2e/__tests__` that `bun test` collects, at any depth, relative to
 *  `directory` and sorted: what the lane's `bun test e2e/__tests__` runs there. Every directory
 *  below it that `bun test` goes down is walked, `fixtures/` included, because a test file
 *  planted there runs like any other. The walk begins at that one directory, so a test file
 *  beside the browser suite, were one ever written, is read nowhere, and the paths it answers
 *  begin `__tests__/` as the rest of this module reads them. */
export function testFiles(directory: string): string[] {
  return sourceFiles(join(directory, testsDirectory), {
    directory: collectedDirectory,
    file: (name) => collectedTestFile.test(name),
  }).map((file) => `${testsDirectory}/${file}`);
}

/** The module that holds `rowLimit` and `runBounded`, relative to the `e2e` directory. A test
 *  file reaches it by `./bounded-subprocess` and a fixture by `../bounded-subprocess`, so the scan
 *  compares the resolved path rather than the specifier's text. */
const boundsModule = `${testsDirectory}/bounded-subprocess`;

/** The module that holds `listing`, which awaits a `runBounded` child of its own. */
const listingModule = `${testsDirectory}/playwright-listing`;

/** The export a limit read from the bounds is a call of. */
const boundsExport = "rowLimit";

/** Whether `text` is the name of a bound. */
function isBoundName(text: string): text is BoundName {
  return Object.hasOwn(bounds, text);
}

/** The bound `node` names, where it is a string literal holding the name of one. */
function boundNamed(node: ts.Expression): BoundName | undefined {
  const inner = unwrapped(node);
  return ts.isStringLiteralLike(inner) && isBoundName(inner.text) ? inner.text : undefined;
}

/** The bound a call of `runBounded` stops its child at, where the options object written in the
 *  call names it as a string literal: `runBounded({ ..., bound: "short" })`. A `bound:` the call
 *  computes, and options passed by name, are read as no bound. */
function boundOption(call: ts.CallExpression): BoundName | undefined {
  const options = call.arguments.at(0);
  if (options === undefined) return undefined;
  const inner = unwrapped(options);
  if (!ts.isObjectLiteralExpression(inner)) return undefined;
  for (const property of inner.properties) {
    if (!ts.isPropertyAssignment(property) || !ts.isIdentifier(property.name)) continue;
    if (property.name.text === "bound") return boundNamed(property.initializer);
  }
  return undefined;
}

/** The larger of two bounds, either of which may be unread. */
function larger(
  first: BoundName | undefined,
  second: BoundName | undefined
): BoundName | undefined {
  if (first === undefined) return second;
  if (second === undefined) return first;
  return bounds[second] > bounds[first] ? second : first;
}

/** An export a call of which waits for a bounded child: the module it is read from, and the
 *  bound a call of it stops its child at, where the scan can read it. */
type BoundedCall = {
  readonly module: string;
  readonly exported: string;
  readonly boundOf: (call: ts.CallExpression) => BoundName | undefined;
};

/** The exports a call of which waits for a bounded child. `listing` runs its one child under the
 *  `listing` bound whatever its caller passes, and `runBounded` under the bound its options
 *  name. */
const boundedCalls: readonly BoundedCall[] = [
  { module: boundsModule, exported: "runBounded", boundOf: boundOption },
  { module: listingModule, exported: "listing", boundOf: () => "listing" },
];

/** The names a file calls one export of this directory's own modules by, read from its imports:
 *  the local name of the export however it was renamed, and the name of a namespace a member of
 *  which under the export's own name is the same export. A file that imports neither calls it
 *  nowhere, whatever it declares. */
type ImportedNames = {
  /** Bare names a call of which is a call of the export. */
  readonly bare: ReadonlySet<string>;
  /** Names a member of which is that export, reached through `import * as` or bound to the
   *  module an `import()` or a `require` loads. */
  readonly namespaces: ReadonlySet<string>;
};

/** The path a relative module specifier resolves to from `sourceFile`, with no extension, which
 *  is how `importedNames` compares modules; undefined for any other specifier. */
function relativeModule(
  sourceFile: ts.SourceFile,
  specifier: ts.Expression | undefined
): string | undefined {
  if (specifier === undefined || !ts.isStringLiteralLike(specifier)) return undefined;
  if (!specifier.text.startsWith(".")) return undefined;
  return resolve(dirname(sourceFile.fileName), specifier.text.replace(/\.ts$/, ""));
}

/** The module `node` loads, where it is an `import()` or a `require` of a relative specifier,
 *  awaited or not. */
function loadedModule(sourceFile: ts.SourceFile, node: ts.Expression): string | undefined {
  const inner = unwrapped(node);
  if (!ts.isCallExpression(inner)) return undefined;
  const loader = inner.expression;
  const loads =
    loader.kind === ts.SyntaxKind.ImportKeyword ||
    (ts.isIdentifier(loader) && loader.text === "require");
  return loads ? relativeModule(sourceFile, inner.arguments.at(0)) : undefined;
}

/** What `sourceFile` calls `exported` of `modulePath` by, read from its import declarations and
 *  from its variables, at any scope, bound to an `import()` or a `require` of that module: a name
 *  bound to the module is a namespace, and a name destructured off it under the export's name is
 *  that export. `modulePath` is the module's path with no extension, which is what a relative
 *  specifier resolves to. */
function importedNames(
  sourceFile: ts.SourceFile,
  modulePath: string,
  exported: string
): ImportedNames {
  const bare = new Set<string>();
  const namespaces = new Set<string>();
  for (const statement of sourceFile.statements) {
    if (!ts.isImportDeclaration(statement)) continue;
    if (relativeModule(sourceFile, statement.moduleSpecifier) !== modulePath) continue;
    const bindings = statement.importClause?.namedBindings;
    if (bindings === undefined) continue;
    if (ts.isNamespaceImport(bindings)) {
      namespaces.add(bindings.name.text);
      continue;
    }
    for (const element of bindings.elements) {
      // `rowLimit as bound` carries its export name in `propertyName`; `rowLimit` alone carries
      // it in `name`, which is then the local name as well.
      const name = element.propertyName?.text ?? element.name.text;
      if (name === exported) bare.add(element.name.text);
    }
  }
  const collect = (node: ts.Node): void => {
    if (
      ts.isVariableDeclaration(node) &&
      node.initializer !== undefined &&
      loadedModule(sourceFile, node.initializer) === modulePath
    ) {
      if (ts.isIdentifier(node.name)) namespaces.add(node.name.text);
      if (ts.isObjectBindingPattern(node.name)) {
        for (const element of node.name.elements) {
          const key = element.propertyName ?? element.name;
          if (!ts.isIdentifier(element.name) || element.dotDotDotToken !== undefined) continue;
          if ((ts.isIdentifier(key) || ts.isStringLiteral(key)) && key.text === exported) {
            bare.add(element.name.text);
          }
        }
      }
    }
    ts.forEachChild(node, collect);
  };
  ts.forEachChild(sourceFile, collect);
  return { bare, namespaces };
}

/** Whether `call` calls `exported` under the names the file imported it by: a bare name, or a
 *  member of a namespace. A function of the file's own that happens to share the export's name
 *  is no call of it. */
function callsImport(call: ts.CallExpression, names: ImportedNames, exported: string): boolean {
  const callee = call.expression;
  if (ts.isPropertyAccessExpression(callee)) {
    return (
      callee.name.text === exported &&
      ts.isIdentifier(callee.expression) &&
      names.namespaces.has(callee.expression.text)
    );
  }
  return ts.isIdentifier(callee) && names.bare.has(callee.text);
}

/** The export names a row is declared by. `bun test` exports one function under `test` and `it`,
 *  and its skipped twin under `xtest` and `xit`, which take a limit in the same places. */
const rowNames = new Set(["test", "it", "xtest", "xit"]);

/** The hooks that take a limit as their second argument. */
const hookNames = new Set(["beforeAll", "afterAll", "beforeEach", "afterEach", "onTestFinished"]);

/** The export that gives every row of a file a limit at once. */
const fileLimitName = "setDefaultTimeout";

/** The export whose `setTimeout` is that same file-wide limit under Jest's spelling. */
const jestExport = "jest";

/** The member of `jest` that states it. */
const jestLimitName = "setTimeout";

/** What a name bound to `jest.setTimeout` itself stands for, which is no export of its own. */
const jestLimit = `${jestExport}.${jestLimitName}`;

/** The module all of those are exports of. */
const testModule = "bun:test";

/** The export names this rule reads, which are what a bare spelling and a member of the module
 *  stand for. */
const readExports = new Set([...rowNames, ...hookNames, fileLimitName, jestExport]);

/** The names a file reaches those exports by: the bare spellings `bun test` declares as globals,
 *  and whatever local name the file bound each export to. See WHAT A NAME IS above. */
type TestNames = {
  /** What each name stands for: the export's own name, under the bare spelling and under the
   *  local name the file bound that export to alike, and `jest.setTimeout` for a name bound to
   *  that member. */
  readonly exports: ReadonlyMap<string, string>;
  /** Names bound to the module itself, a member of which is the export of that member's name. */
  readonly namespaces: ReadonlySet<string>;
};

/** `node` with whatever wraps an expression without changing what it names taken off:
 *  parentheses, `await`, `!` and a type assertion. */
function unwrapped(node: ts.Expression): ts.Expression {
  let inner = node;
  while (
    ts.isParenthesizedExpression(inner) ||
    ts.isAwaitExpression(inner) ||
    ts.isNonNullExpression(inner) ||
    ts.isAsExpression(inner) ||
    ts.isSatisfiesExpression(inner) ||
    ts.isTypeAssertionExpression(inner)
  ) {
    inner = inner.expression;
  }
  return inner;
}

/** Whether `node` is `bun:test` itself: a name the file bound the module to, the module a
 *  `require` or an `import()` loads, or the `default` of any of those, which Bun answers with the
 *  same module. */
function isModule(node: ts.Expression, names: TestNames): boolean {
  const inner = unwrapped(node);
  if (ts.isIdentifier(inner)) return names.namespaces.has(inner.text);
  if (ts.isPropertyAccessExpression(inner)) {
    return inner.name.text === "default" && isModule(inner.expression, names);
  }
  if (!ts.isCallExpression(inner)) return false;
  const loader = inner.expression;
  const loads =
    loader.kind === ts.SyntaxKind.ImportKeyword ||
    (ts.isIdentifier(loader) && loader.text === "require");
  const specifier = inner.arguments.at(0);
  return (
    loads &&
    specifier !== undefined &&
    ts.isStringLiteralLike(specifier) &&
    specifier.text === testModule
  );
}

/** What the `member` of `object` stands for: the export of that name where `object` is the module,
 *  and `jest.setTimeout` where `object` is `jest` and the member is its `setTimeout`. */
function memberOf(object: ts.Expression, member: string, names: TestNames): string | undefined {
  if (isModule(object, names)) return readExports.has(member) ? member : undefined;
  return member === jestLimitName && exportOf(object, names) === jestExport ? jestLimit : undefined;
}

/** The export `node` names, where it names one: an identifier under the name the file reaches
 *  that export by, and a member of the module or of `jest` as `memberOf` reads it. Anything else
 *  names no export of that module, so a `setDefaultTimeout` method of some object of the file's
 *  own is read nowhere here. */
function exportOf(node: ts.Expression, names: TestNames): string | undefined {
  const inner = unwrapped(node);
  if (ts.isIdentifier(inner)) return names.exports.get(inner.text);
  if (ts.isPropertyAccessExpression(inner))
    return memberOf(inner.expression, inner.name.text, names);
  return undefined;
}

/** Whether `callee` names a row: `test` and `it` under the names this file reaches them by, and
 *  everything reached from one of those - `test.only(...)`, `test.skip(...)`, `test.todo(...)`,
 *  `test.failing(...)`, and the functions `test.skipIf(<x>)` and `test.each(<x>)` answer - since
 *  Bun reads a limit in the same place in all of them.
 *
 *  The callee is walked down towards its root: a call expression to its own callee, a property
 *  access to its own object, until a name of the module is left or nothing is. That reads every
 *  modifier at once, the ones Bun adds after this is written included, and it keeps the reading
 *  failing the safe way - anything reached from a row's name is asked for a reason rather than
 *  skipped. */
function isRowCallee(callee: ts.Expression, names: TestNames): boolean {
  let node = unwrapped(callee);
  let exported = exportOf(node, names);
  while (exported === undefined) {
    if (!ts.isCallExpression(node) && !ts.isPropertyAccessExpression(node)) return false;
    node = unwrapped(node.expression);
    exported = exportOf(node, names);
  }
  return rowNames.has(exported);
}

/** What `sourceFile` reaches `bun:test` by, read from its import declarations, from its
 *  variables to a fixpoint, and from the bare spellings, which stand whatever a file binds. */
function testNames(sourceFile: ts.SourceFile): TestNames {
  const exports = new Map([...readExports].map((name) => [name, name]));
  const namespaces = new Set<string>();
  const names: TestNames = { exports, namespaces };
  for (const statement of sourceFile.statements) {
    if (!ts.isImportDeclaration(statement)) continue;
    const specifier = statement.moduleSpecifier;
    if (!ts.isStringLiteralLike(specifier) || specifier.text !== testModule) continue;
    const clause = statement.importClause;
    if (clause === undefined) continue;
    if (clause.name !== undefined) namespaces.add(clause.name.text);
    const bindings = clause.namedBindings;
    if (bindings === undefined) continue;
    if (ts.isNamespaceImport(bindings)) {
      namespaces.add(bindings.name.text);
      continue;
    }
    for (const element of bindings.elements) {
      // `test as row` carries its export name in `propertyName`; `test` alone carries it in
      // `name`, which is then the local name as well.
      const exported = element.propertyName?.text ?? element.name.text;
      if (readExports.has(exported)) exports.set(element.name.text, exported);
    }
  }

  const declarations: { name: ts.BindingName; initializer: ts.Expression }[] = [];
  const collect = (node: ts.Node): void => {
    if (ts.isVariableDeclaration(node) && node.initializer !== undefined) {
      declarations.push({ name: node.name, initializer: node.initializer });
    }
    ts.forEachChild(node, collect);
  };
  ts.forEachChild(sourceFile, collect);
  // A name is bound once, by the first binding read, so every pass either binds a name no pass
  // bound before or ends the loop.
  let grew = true;
  const bind = (local: string, exported: string | undefined): void => {
    if (exported === undefined || exports.has(local) || namespaces.has(local)) return;
    exports.set(local, exported);
    grew = true;
  };
  const bindModule = (local: string): void => {
    if (exports.has(local) || namespaces.has(local)) return;
    namespaces.add(local);
    grew = true;
  };
  // A row reached from a row, `const only = test.only`, stands for a row like `test` itself.
  const rowOf = (node: ts.Expression): string | undefined =>
    isRowCallee(node, names) ? "test" : undefined;
  while (grew) {
    grew = false;
    for (const { name, initializer } of declarations) {
      if (ts.isIdentifier(name)) {
        if (isModule(initializer, names)) bindModule(name.text);
        else bind(name.text, exportOf(initializer, names) ?? rowOf(initializer));
        continue;
      }
      if (!ts.isObjectBindingPattern(name)) continue;
      for (const element of name.elements) {
        if (element.dotDotDotToken !== undefined || !ts.isIdentifier(element.name)) continue;
        const key = element.propertyName ?? element.name;
        if (!ts.isIdentifier(key) && !ts.isStringLiteral(key)) continue;
        if (key.text === "default" && isModule(initializer, names)) {
          bindModule(element.name.text);
        } else {
          bind(element.name.text, memberOf(initializer, key.text, names) ?? rowOf(initializer));
        }
      }
    }
  }
  return names;
}

/** What one call or one name waits for: the bounded export it reaches, and the largest bound the
 *  scan reads among the children it waits for, or undefined where it reads none. */
type Reach = { readonly reached: string; readonly bound: BoundName | undefined };

/** What a file waits for bounded children by: the names it imports each bounded export under,
 *  every function of its own, and every name it binds to a call that waits. */
type WaitingNames = {
  readonly imports: readonly (BoundedCall & { readonly names: ImportedNames })[];
  /** The file's own functions, by the name each is declared under, at any scope: a function
   *  declaration, an arrow or a function expression. */
  readonly functions: ReadonlyMap<string, readonly ts.Node[]>;
  /** The file's own functions that wait, each with what it reaches. */
  readonly helpers: ReadonlyMap<string, Reach>;
  /** The names the file binds or assigns, at any scope, to an expression that holds a call that
   *  waits - `const lane = retained(listing(...))`, `lane = retained(listing(...))` - each with
   *  what that call reaches. A name bound or assigned to an expression whose outermost operator,
   *  under parentheses and `as`, is `await` is left out: it holds what the call answered, the
   *  binding itself waited, and a reference to it waits for nothing. An `await` deeper in the
   *  expression leaves the name in: WHAT A ROW WAITS FOR. */
  readonly values: ReadonlyMap<string, Reach>;
};

/** What `call` reaches: the export itself, with the bound the call names, where it calls one
 *  under its imported name, and what a function of the file's own reaches where it calls one of
 *  those. */
function reachOf(call: ts.CallExpression, waiting: WaitingNames): Reach | undefined {
  for (const { exported, names, boundOf } of waiting.imports) {
    if (callsImport(call, names, exported)) return { reached: exported, bound: boundOf(call) };
  }
  return ts.isIdentifier(call.expression) ? waiting.helpers.get(call.expression.text) : undefined;
}

/** One place code waits: a call that reaches a bounded export, or a reference to a name bound to
 *  such a call. */
type Wait = { readonly node: ts.CallExpression | ts.Identifier; readonly reach: Reach };

/** Whether `name` is read as a value where it stands, rather than standing as a property's name,
 *  a key, the name a declaration binds or the name an assignment writes to. `{ lane }` is a
 *  reference to `lane`; the `lane` of `lane = listing(...)` is not. */
function isReference(name: ts.Identifier): boolean {
  const parent = name.parent;
  if (ts.isShorthandPropertyAssignment(parent)) return true;
  if (ts.isPropertyAccessExpression(parent)) return parent.expression === name;
  if (ts.isBindingElement(parent) && parent.propertyName === name) return false;
  if (ts.isBinaryExpression(parent) && parent.left === name) {
    const operator = parent.operatorToken.kind;
    return operator < ts.SyntaxKind.FirstAssignment || operator > ts.SyntaxKind.LastAssignment;
  }
  return !("name" in parent && parent.name === name);
}

/** Every name declared under `node`, at any depth: its variables, its parameters and its
 *  function declarations. A name `node` declares is its own, and a reference to it inside `node`
 *  is a reference to that one. */
function declaredUnder(node: ts.Node): Set<string> {
  const declared = new Set<string>();
  const bind = (name: ts.BindingName): void => {
    if (ts.isIdentifier(name)) {
      declared.add(name.text);
      return;
    }
    for (const element of name.elements) {
      if (!ts.isOmittedExpression(element)) bind(element.name);
    }
  };
  const visit = (child: ts.Node): void => {
    if (ts.isVariableDeclaration(child) || ts.isParameter(child)) bind(child.name);
    if (ts.isFunctionDeclaration(child) && child.name !== undefined) declared.add(child.name.text);
    ts.forEachChild(child, visit);
  };
  visit(node);
  return declared;
}

/** Every place under `body` that waits, at any depth, in the order the file writes them: each
 *  call that reaches a bounded export, and, where `readsNames` holds, each reference to a name
 *  bound to a waiting call that `body` does not declare itself. A name passed as a value - the
 *  `runBounded` of `.map(runBounded)` - is no call and no such name, so it is no wait. */
function waitsUnder(body: ts.Node, waiting: WaitingNames, readsNames: boolean): Wait[] {
  const declared = readsNames ? declaredUnder(body) : new Set<string>();
  const waits: Wait[] = [];
  const visit = (node: ts.Node): void => {
    if (ts.isCallExpression(node)) {
      const reach = reachOf(node, waiting);
      if (reach !== undefined) waits.push({ node, reach });
    } else if (readsNames && ts.isIdentifier(node) && !declared.has(node.text)) {
      const reach = waiting.values.get(node.text);
      if (reach !== undefined && isReference(node)) waits.push({ node, reach });
    }
    ts.forEachChild(node, visit);
  };
  visit(body);
  return waits;
}

/** What `waits` together reach: the export the first of them reaches, and the largest bound read
 *  among them all. Nothing where there is no wait. */
function reachOfAll(waits: readonly Wait[]): Reach | undefined {
  const first = waits.at(0);
  if (first === undefined) return undefined;
  const bound = waits.reduce<BoundName | undefined>(
    (largest, wait) => larger(largest, wait.reach.bound),
    undefined
  );
  return { reached: first.reach.reached, bound };
}

/** The operators an assignment that binds a waiting name is written with: `lane = ...`, and
 *  `lane ??= ...`, `lane ||= ...` and `lane &&= ...`, each of which may leave the call's promise
 *  in the name. */
const assignments: ReadonlySet<ts.SyntaxKind> = new Set([
  ts.SyntaxKind.EqualsToken,
  ts.SyntaxKind.QuestionQuestionEqualsToken,
  ts.SyntaxKind.BarBarEqualsToken,
  ts.SyntaxKind.AmpersandAmpersandEqualsToken,
]);

/** What `sourceFile` waits for bounded children by. `directory` is the `e2e` directory the
 *  modules are read against. The functions that wait and the names bound or assigned to a
 *  waiting call are read together to a fixpoint, so a function that calls a function that calls
 *  `listing` waits, a name bound to a call of such a function is a waiting name, and a function
 *  that awaits such a name waits too. A name bound to a waiting name is not read: WHAT A ROW
 *  WAITS FOR. */
function waitingNames(sourceFile: ts.SourceFile, directory: string): WaitingNames {
  const imports = boundedCalls.map((call) => ({
    ...call,
    names: importedNames(sourceFile, join(directory, call.module), call.exported),
  }));
  const functions = new Map<string, ts.Node[]>();
  const declare = (name: string, body: ts.Node): void => {
    functions.set(name, [...(functions.get(name) ?? []), body]);
  };
  const bindings: { name: string; initializer: ts.Expression }[] = [];
  // A name is read the same way whether a declaration binds it or an assignment writes it.
  const bind = (name: string, initializer: ts.Expression): void => {
    const value = unwrapped(initializer);
    let outer = initializer;
    while (ts.isParenthesizedExpression(outer) || ts.isAsExpression(outer)) {
      outer = outer.expression;
    }
    if (ts.isArrowFunction(value) || ts.isFunctionExpression(value)) {
      declare(name, value);
    } else if (!ts.isAwaitExpression(outer)) {
      bindings.push({ name, initializer });
    }
  };
  const collect = (node: ts.Node): void => {
    if (ts.isFunctionDeclaration(node) && node.name !== undefined && node.body !== undefined) {
      declare(node.name.text, node);
    }
    if (
      ts.isVariableDeclaration(node) &&
      ts.isIdentifier(node.name) &&
      node.initializer !== undefined
    ) {
      bind(node.name.text, node.initializer);
    }
    if (
      ts.isBinaryExpression(node) &&
      ts.isIdentifier(node.left) &&
      assignments.has(node.operatorToken.kind)
    ) {
      bind(node.left.text, node.right);
    }
    ts.forEachChild(node, collect);
  };
  ts.forEachChild(sourceFile, collect);

  const helpers = new Map<string, Reach>();
  const values = new Map<string, Reach>();
  const waiting: WaitingNames = { imports, functions, helpers, values };
  // A name is settled once it waits and changes after that only to a larger bound, so every
  // pass either reads more than the pass before or ends the loop.
  let grew = true;
  const settle = (known: Map<string, Reach>, name: string, reach: Reach | undefined): void => {
    if (reach === undefined) return;
    const before = known.get(name);
    const bound = larger(before?.bound, reach.bound);
    if (before !== undefined && before.bound === bound) return;
    known.set(name, { reached: before?.reached ?? reach.reached, bound });
    grew = true;
  };
  while (grew) {
    grew = false;
    for (const [name, bodies] of functions) {
      settle(helpers, name, reachOfAll(bodies.flatMap((body) => waitsUnder(body, waiting, true))));
    }
    for (const { name, initializer } of bindings) {
      settle(values, name, reachOfAll(waitsUnder(initializer, waiting, false)));
    }
  }
  return waiting;
}

/** Whether `argument` is a body rather than a limit: an arrow, a function expression, or the name
 *  of a function the file declares. */
function isBody(argument: ts.Expression, waiting: WaitingNames): boolean {
  const inner = unwrapped(argument);
  if (ts.isArrowFunction(inner) || ts.isFunctionExpression(inner)) return true;
  return ts.isIdentifier(inner) && waiting.functions.has(inner.text);
}

/** What the bodies among `call`'s arguments wait for: each bounded call or waiting name as the
 *  file writes it, once each and in the order they are written, and the largest bound the scan
 *  reads among them. A body that is the name of a function of the file's own is read through
 *  that function. */
function waitsOf(
  call: ts.CallExpression,
  waiting: WaitingNames
): { readonly waitsFor: string[]; readonly bound: BoundName | undefined } {
  const bodies = call.arguments
    .filter((argument) => isBody(argument, waiting))
    .flatMap((argument) => {
      const inner = unwrapped(argument);
      return ts.isIdentifier(inner) ? (waiting.functions.get(inner.text) ?? []) : [inner];
    });
  const waits = bodies.flatMap((body) => waitsUnder(body, waiting, true));
  const named = new Set<string>();
  for (const { node, reach } of waits) {
    if (ts.isIdentifier(node)) {
      named.add(`\`${node.text}\` (which waits for \`${reach.reached}\`)`);
      continue;
    }
    const callee = `\`${node.expression.getText()}\``;
    const direct = waiting.imports.some(({ exported, names }) =>
      callsImport(node, names, exported)
    );
    named.add(direct ? callee : `${callee} (which waits for \`${reach.reached}\`)`);
  }
  return { waitsFor: [...named], bound: reachOfAll(waits)?.bound };
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
 *  of an options object, and the expression itself otherwise. A body states nothing, because Bun
 *  reads `test(title, body, options)` and `test(title, options, body)` alike. */
function limitOf(
  argument: ts.Expression | undefined,
  waiting: WaitingNames
): ts.Expression | undefined {
  if (argument === undefined || isBody(argument, waiting)) return undefined;
  if (ts.isObjectLiteralExpression(argument)) return timeoutOf(argument);
  return argument;
}

/** What one call states: nothing at all where it is no row, hook or `setDefaultTimeout`; the kind
 *  and the name a refusal uses, and the limit expressions, where it is one. */
type CallLimits = {
  readonly kind: LimitKind;
  readonly title: string;
  readonly limits: readonly ts.Expression[];
};

/** The limits `call` states, read from the places THE RULE names and from no other argument. The
 *  name a refusal quotes is the callee as the file writes it, which is what its reader looks
 *  for: `beforeAll`, `cap`, or `bun.setDefaultTimeout`. */
function limitsOfCall(
  call: ts.CallExpression,
  names: TestNames,
  waiting: WaitingNames
): CallLimits | undefined {
  const callee = call.expression;
  const limits: ts.Expression[] = [];
  const state = (argument: ts.Expression | undefined): void => {
    const limit = limitOf(argument, waiting);
    if (limit !== undefined) limits.push(limit);
  };
  if (isRowCallee(callee, names)) {
    state(call.arguments.at(1));
    state(call.arguments.at(2));
    return { kind: "row", title: call.arguments.at(0)?.getText() ?? "", limits };
  }
  const exported = exportOf(callee, names);
  if (exported === undefined) return undefined;
  if (hookNames.has(exported)) {
    state(call.arguments.at(1));
    return { kind: "hook", title: callee.getText(), limits };
  }
  if (exported === fileLimitName || exported === jestLimit) {
    state(call.arguments.at(0));
    return { kind: "file", title: callee.getText(), limits };
  }
  return undefined;
}

/** The reason standing above a limit, read by `reasonEndingAbove` against `limitLine`.
 *
 * Where a comment above a limit is written down is whatever begins the limit's own line: the
 * limit itself where it stands on a line of its own, the property where the limit is a
 * `timeout:`, and the row's whole statement where the row is written on one line. So the run is
 * read from the limit and from each ancestor that begins on the limit's line, and nothing that
 * stands above an earlier line of the same statement is read at all. */
function reasonAbove(sourceFile: ts.SourceFile, limit: ts.Expression, limitLine: number): string {
  const text = sourceFile.getFullText();
  const ranges = new Map<number, ts.CommentRange>();
  const read = (node: ts.Node): void => {
    for (const range of ts.getLeadingCommentRanges(text, node.getFullStart()) ?? []) {
      ranges.set(range.pos, range);
    }
  };
  read(limit);
  for (
    let node: ts.Node = limit.parent;
    !ts.isSourceFile(node) &&
    sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line === limitLine;
    node = node.parent
  ) {
    read(node);
  }
  const sorted = [...ranges.values()].sort((first, second) => first.pos - second.pos);
  return reasonEndingAbove(sourceFile, sorted, limitLine);
}

/** The bound names of an `{ atOnce: [...] }` argument of `rowLimit`, where it is written out as
 *  string literals that name bounds. */
function atOnceOf(argument: ts.Expression): BoundName[] | undefined {
  const inner = unwrapped(argument);
  if (!ts.isObjectLiteralExpression(inner) || inner.properties.length !== 1) return undefined;
  const [property] = inner.properties;
  if (!ts.isPropertyAssignment(property) || !ts.isIdentifier(property.name)) return undefined;
  const list = unwrapped(property.initializer);
  if (property.name.text !== "atOnce" || !ts.isArrayLiteralExpression(list)) return undefined;
  const names = list.elements.map((element) => boundNamed(element));
  return names.every((name): name is BoundName => name !== undefined) ? names : undefined;
}

/** The milliseconds a call of `rowLimit` states, computed by `rowLimit` itself from the arguments
 *  as written, where each is a bound's name or an `{ atOnce: [...] }` of names. Undefined where
 *  any argument is something else - a spread, a name, a call - whose value a parse cannot read. */
function totalOf(call: ts.CallExpression): number | undefined {
  const waits: (BoundName | { readonly atOnce: readonly BoundName[] })[] = [];
  for (const argument of call.arguments) {
    const name = boundNamed(argument);
    const group = name === undefined ? atOnceOf(argument) : undefined;
    if (name === undefined && group === undefined) return undefined;
    waits.push(name ?? { atOnce: group ?? [] });
  }
  return rowLimit(...waits);
}

/** Every wall-clock limit the given files state, each reported under the path it was named by:
 *  `testFiles`' path relative to `directory`, and every row and hook that waits for a bounded
 *  child and states no limit of its own, with no `stated` limit, as `WallClockLimit` describes.
 *  Whether the file's own `rowLimit` covers such a row is `refusedLimits`' question.
 *
 * The walk is over the syntax tree rather than the text, because the text cannot tell a row from
 * the word in a comment or in a string - `check-slow-rows.test.ts` holds whole spec files inside
 * template literals - and because the comment above a limit is trivia the parser already has. */
export function limitsOfFiles(directory: string, files: readonly string[]): WallClockLimit[] {
  const boundsPath = join(directory, boundsModule);
  const limits: WallClockLimit[] = [];
  for (const file of files) {
    const path = join(directory, file);
    // No script kind is passed, so TypeScript reads it off the path's extension in any case: TSX
    // for `.tsx`, JSX for `.jsx`, JS for `.js`, `.mjs` and `.cjs`, and TS for `.ts`, `.mts` and
    // `.cts` (`ensureScriptKind` of typescript 5.9.3), so a JSX element in a `*.test.tsx` row
    // parses as one.
    const sourceFile = ts.createSourceFile(
      path,
      readFileSync(path, "utf8"),
      ts.ScriptTarget.Latest,
      true
    );
    const derived = importedNames(sourceFile, boundsPath, boundsExport);
    const names = testNames(sourceFile);
    const waiting = waitingNames(sourceFile, directory);
    const lineOf = (node: ts.Node): number =>
      sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line;
    const record = (call: ts.CallExpression): void => {
      const ofCall = limitsOfCall(call, names, waiting);
      if (ofCall === undefined) return;
      const { kind, title } = ofCall;
      const { waitsFor, bound } =
        kind === "file" ? { waitsFor: [], bound: undefined } : waitsOf(call, waiting);
      for (const limit of ofCall.limits) {
        const line = lineOf(limit);
        const fromBounds = ts.isCallExpression(limit) && callsImport(limit, derived, boundsExport);
        const stated: StatedLimit = {
          text: limit.getText(sourceFile),
          reason: reasonAbove(sourceFile, limit, line),
          fromBounds,
          total: fromBounds && ts.isCallExpression(limit) ? totalOf(limit) : undefined,
        };
        limits.push({ file, line: line + 1, stated, kind, title, waitsFor, bound });
      }
      if (ofCall.limits.length === 0 && waitsFor.length > 0) {
        limits.push({
          file,
          line: lineOf(call) + 1,
          stated: undefined,
          kind,
          title,
          waitsFor,
          bound,
        });
      }
    };
    const visit = (node: ts.Node): void => {
      if (ts.isCallExpression(node)) record(node);
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

/** The two ways out, which every refusal of a limit whose body the scan sees wait for no bounded
 *  child ends with, written about whatever states the limit: a hook waits for children and
 *  drives processes as a row does, and `setDefaultTimeout` stands for every row of its file. */
function remedyFor(limit: WallClockLimit): string {
  const subject = limit.kind === "file" ? "every row of the file" : `the ${limit.kind}`;
  return (
    `call \`rowLimit(...)\` naming the bounded children ${subject} waits for, so the limit is ` +
    `read from their bounds and moves when they move; or, where ${subject} drives a process ` +
    "rather than waiting for one, give the limit a line of its own and write the sentence that " +
    "says what it drives and why that long is enough as a comment on the line directly above it."
  );
}

/** The one way out for a row or hook that waits for bounded children, which its refusal ends
 *  with. */
function waitingRemedy(limit: WallClockLimit): string {
  return (
    `state \`rowLimit(...)\` naming the bounded children the ${limit.kind} waits for - as its ` +
    "own limit, or as the file's `setDefaultTimeout` - so the limit is read from their bounds, " +
    "fires after theirs and moves when they move."
  );
}

/** Where `total`, the milliseconds a `rowLimit` call states for `limit`'s row or hook, falls short
 *  of `rowLimit` of the largest bound the scan reads among the children it waits for, the clause
 *  that says so and what to do about it. Nothing where it does not fall short, and nothing where
 *  the scan reads no bound or no total: WHAT A ROWLIMIT COVERS. */
function shortOfBound(limit: WallClockLimit, total: number | undefined): string | undefined {
  if (limit.bound === undefined || total === undefined) return undefined;
  const needed = rowLimit(limit.bound);
  if (total >= needed) return undefined;
  return (
    `${total} ms, and waits for ${limit.waitsFor.join(" and ")}, under the \`${limit.bound}\` ` +
    `bound of ${bounds[limit.bound]} ms: the ${limit.kind} needs \`rowLimit("${limit.bound}")\`, ` +
    `${needed} ms, at the least, or its own clock fires before that child's bound and the ` +
    `failure names no child. Name in \`rowLimit(...)\` every bounded child the ${limit.kind} ` +
    "waits for, so the limit is read from their bounds."
  );
}

/** Why `limit`, a row or hook that waits for bounded children and states no limit of its own, is
 *  refused, or nothing where its file's own `rowLimit` covers it. That limit is the row's where
 *  the file states one, and the largest of them is read where it states more than one; one whose
 *  total the scan cannot read covers the row as stated. */
function unstatedRefusal(
  limit: WallClockLimit,
  limits: readonly WallClockLimit[]
): string | undefined {
  const waits = limit.waitsFor.join(" and ");
  const at = `${limit.file}:${limit.line} ${statedBy(limit)}`;
  const fileLimits = limits.flatMap((other) =>
    other.file === limit.file && other.kind === "file" && other.stated?.fromBounds
      ? [{ other, total: other.stated.total }]
      : []
  );
  if (fileLimits.length === 0) {
    return (
      `${at} waits for ${waits} and states no limit, so the lane's \`--timeout\` stops it, and ` +
      "that clock can fire before the bound of the child it waits for and name no child: " +
      waitingRemedy(limit)
    );
  }
  let widest = fileLimits[0];
  for (const fileLimit of fileLimits) {
    if (fileLimit.total === undefined) return undefined;
    if (widest.total !== undefined && fileLimit.total > widest.total) widest = fileLimit;
  }
  const short = shortOfBound(limit, widest.total);
  if (short === undefined) return undefined;
  return (
    `${at} states no limit of its own, so the file's own ` +
    `${widest.other.title} on line ${widest.other.line} stops it at ` +
    `${widest.other.stated?.text}, ${short}`
  );
}

/** The limits the rule refuses, each with the sentence a reader of the failure acts on: which
 *  file and line, which row or hook, what the limit says or that it states none, what it waits
 *  for and the bound it needs, what stands above it, and the ways out. `limits` is read whole, so
 *  a file's own `rowLimit` is read for each of its rows that states none. */
export function refusedLimits(limits: readonly WallClockLimit[]): RefusedLimit[] {
  const refused: RefusedLimit[] = [];
  for (const limit of limits) {
    const stated = limit.stated;
    if (stated === undefined) {
      const why = unstatedRefusal(limit, limits);
      if (why !== undefined) refused.push({ limit, why });
      continue;
    }
    const at = `${limit.file}:${limit.line} ${statedBy(limit)} stops at ${stated.text}`;
    const waits = limit.waitsFor.join(" and ");
    if (stated.fromBounds) {
      const short = shortOfBound(limit, stated.total);
      if (short !== undefined) refused.push({ limit, why: `${at}, ${short}` });
      continue;
    }
    if (limit.waitsFor.length > 0) {
      refused.push({
        limit,
        why:
          `${at}, which is no call of \`rowLimit\`, and waits for ${waits}: ` +
          `a reason explains the limit of a ${limit.kind} that drives a process, and this ` +
          `${limit.kind} waits for bounded children, so ${waitingRemedy(limit)}`,
      });
      continue;
    }
    const remedy = remedyFor(limit);
    if (stated.reason === "") {
      refused.push({
        limit,
        why: `${at}, which is no call of \`rowLimit\` and has no reason on the line above it: ${remedy}`,
      });
      continue;
    }
    const short = shortReason(stated.reason, "the limit", "this long");
    if (short !== undefined) refused.push({ limit, why: `${at} ${short}, so ${remedy}` });
  }
  return refused;
}
