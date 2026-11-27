import { expect, jest, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import * as ts from "typescript";

import { afterOnFakeClock, onFakeClock } from "./fake-clock";

/**
 * R6 of dispatch://INFRA-983: no unit test synchronises on the real clock. A row that waits for a
 * product timer moves the fake clock (`web/src/__tests__/fake-clock.ts`); a wait that must stay on
 * the real clock says why above it, in the words the kept ones use ("This one stays on the real
 * clock", then the reason), and docs/testing.md, "The clock of a unit test", is the rule.
 *
 * This parses unit tests and named test helpers under `web/src` - `*.test.ts(x)`, files in
 * `__tests__`, `*-fixture(s).ts(x)` and `*.coverage.ts(x)` - plus the helpers a text scan of
 * relative references finds (`loadedByTestsAlone`). It looks for calls that reach a wait of real
 * time by a name, whatever the delay and callback:
 *
 * `setTimeout`, a `setInterval`, a `setImmediate` or a `requestAnimationFrame`, global or a member
 * of any object (`window.setTimeout`, `globalThis.setInterval`, `window.requestAnimationFrame`),
 * by its name or by a key, a string literal or a `const` of the file bound to one
 * (`globalThis[key]`); the `setTimeout`, `setInterval`, `setImmediate` and `scheduler.wait` of
 * `node:timers` and `node:timers/promises` (or `timers` and `timers/promises`), named, aliased or
 * reached through a namespace or default import (`t.scheduler.wait`), through the `promises` of
 * `node:timers` (`timers.promises.scheduler.wait`), or through the namespace a dynamic import's
 * `then` hands its callback (`import("bun").then((m) => m.sleep(50))`); a `Bun.sleep` or a
 * `Bun.sleepSync`; and every local name an import gives one of those (`import { setTimeout as
 * delay }`, `import { setImmediate as nextTurn }`, `import { sleep as pause } from "bun"`). By the
 * architect's k11 ruling (dispatch://INFRA-983), the scan finds every spelling that reaches a wait
 * by a name, and states one exact boundary, below. So a wait is found under every name,
 * destructure or `bind` it is given, and where it is handed on: a declaration or an assignment
 * whose value is one of those, a member of one, another such name, or a choice of one (`const
 * later = setTimeout`, `options.wait ?? setTimeout`), a parameter's default, the default of a
 * name an object destructure binds, of a parameter, a `for … of` or a `catch` too (`async
 * function settle({ sleep = Bun.sleep }: Options)`), and of one that assigns, in a `for … of`, a
 * `for … in` or an array too (`[{ wait = setTimeout }] = [options]`), and a destructure, renamed,
 * nested or with a default, from `Bun`, from "bun" or a timers module bound whole, by an import,
 * an `import(…)` or a `require(…)` (`const { sleep } = Bun`, `const { setTimeout: delay } = await
 * import("node:timers/promises")`), and of a `setTimeout` from any object; such a module bound
 * whole by `import(…)` or `require(…)`, whose members count as a namespace import's do; the `call`
 * and the `apply` of a wait, and its `bind`, whose answer is such a name; and every reference to a
 * wait that the file hands on out of its view, which whatever it reaches may start: passed to
 * another call or a `new` (`[50].map(Bun.sleep)`, `act(setTimeout)`), stored in an object, an
 * array, a member, a class's field, by a parameter property's default too (`constructor(private
 * wait = setTimeout)`), or a JSX prop, the default of a name an array destructure binds, which
 * the scan does not follow (`const [wait = setTimeout] = delays`), returned, thrown, yielded, or
 * exported (`export const pause = Bun.sleep`, `export { setTimeout as later }`), each found at
 * the reference as a call is (`placeOf`). The scan does not follow a holder of waits handed to
 * another function or stored inside a value, and a wait reached only through one: a holder is
 * `Bun`, "bun" or a timers module bound whole, or a timers module's `scheduler`, as in `nap(Bun)`
 * and `{ bun: Bun }`. An exported holder is found where the file exports it, since an export is no
 * value handed to a function. The names are read in a fixed-point pass over the whole file, so an
 * alias of an alias is one, and so is one bound below its call; a name is an alias in the whole
 * file once any binding makes it one. A wait the scan sees all of starts none: one called (found
 * as the call), a member of it read (`setTimeout.length`), one read or compared (`typeof
 * setTimeout`, `globalThis.setTimeout === window.setTimeout`, a condition, the object `spyOn`
 * takes, a wait handed to one of `readers` such as `Object.hasOwn(setTimeout, "clock")`), one
 * written to, one named in a type, and one bound to a name the scan follows. It follows no
 * resolver, and no callback but the one a dynamic import's `then` hands its namespace: a trace of
 * a resolver through the data flow always misses a form, such as a resolver handed to a helper
 * that calls `setTimeout`, so the call is found where it starts, and so is the timer of a row that
 * stands in for a product's. A call, and a reference handed on, passes in two
 * ways alone. One is `afterOnFakeClock` (`fake-clock.ts`), which refuses the real clock itself: a
 * row that stands in for an answer that takes time to come sets its timer there, and the
 * `setTimeout` inside that helper's own body is passed by its file and its function's name. The
 * other is a marker: the comment block directly above the statement that holds the wait, with no
 * blank line between, holds the words "stays on the real clock" and after them, anywhere in that
 * block, a reason of at least `REASON_WORDS` words of letters. The statement is the first one, the
 * declaration or the class member that starts after the block, as the parser gives it, and the
 * waits it covers are those it holds, however deep: the wait's own statement, a `test(...)` call
 * whose body holds the wait, or a helper whose doc comment holds the marker and whose body holds
 * the wait (`/** … stays on the real clock … *\/ function nextFrame() {…}`). One marker covers
 * one wait, as a jest-dom marker of `check-fireevent-reasons.sh` covers one read, by the
 * architect's ruling: a statement that holds two waits or more passes none of them under its
 * marker, so each carries a marker of its own, and a comment that only mentions the real clock
 * passes nothing. The
 * file is read by the TypeScript parser, so a call is found however it is spelled or wrapped
 * (inside `act(...)`, across lines), and a comment or a string that spells one is none. A zero-delay
 * timer is a wait of real time too: what has run by its turn - a timer of the product's, a render,
 * a promise of the library's - depends on how loaded the machine is, so a row waits for the
 * condition its step stands for, or moves the fake clock over the product's own zero-delay timer
 * with `tick(0)`. A frame and a turn of the loop are of the same kind (dispatch://INFRA-983, the
 * architect's ruling): a row that awaits a `requestAnimationFrame` or a `setImmediate` waits one
 * turn for a thing to have happened, instead of waiting for the thing. Bun's fake clock holds
 * neither: happy-dom runs a frame as a `setImmediate` of the real loop, and
 * `jest.advanceTimersByTime` runs neither, so a row waits for what the frame or the turn brings,
 * and a wait that has nothing else to wait for says why it stays. The scan reads a test's own
 * calls: a product function the row tests that sets a timer itself is the product's code, not a
 * wait of the row's. The act-rule inputs (`*.fixture.ts` and `*.fixture.tsx`, which the scan's
 * `sourceFiles` skips by name) are left out, since they are written to break rules. A separate
 * naming check flags files outside the naming rules that match its double-quoted `from` patterns
 * for `bun:test`, Testing Library or a `__tests__` module. To find other helpers, such as
 * `features/issue-page/chromium-focus-fixup.ts`, `loadedByTestsAlone` scans source text for
 * single- or double-quoted `./` or `../` paths in imports, re-exports, `require(…)` and
 * `new URL(…)`. It tries only the path without its query, with `.ts` or `.tsx` added, or with
 * `index.ts` or `index.tsx` appended. Comments and strings can count; computed paths and aliases
 * do not. A helper must have at least one matched loader, and all its matched loaders must
 * already qualify. A cycle of helpers not recognized by name cannot qualify this way.
 */

const webSource = fileURLToPath(new URL("..", import.meta.url));

/** The words a marker says a wait stays on the real clock in, and how many words of letters, at
 *  least, the reason after them holds. */
const MARKER = "stays on the real clock";
const REASON_WORDS = 3;

/** A blank line: two line ends with nothing but spaces between. */
const blankLine = /\n[ \t]*\r?\n/;

/** A word of letters: `why`, `can't` and `fake-clock` are one each, `(INFRA-1505)` and `500`
 *  none. */
const wordOfLetters = /(?<![\w-])[A-Za-z]+(?:['’-][A-Za-z]+)*(?![\w-])/g;

/** The calls that start a wait of real time by their name alone, global, a member of any object,
 *  or under the name an import gives them: the timers, the turn of `setImmediate` and the frame
 *  of `requestAnimationFrame`. */
const waitCalls: ReadonlySet<string> = new Set([
  "setTimeout",
  "setInterval",
  "setImmediate",
  "requestAnimationFrame",
]);

/** The modules whose `setTimeout`, `setInterval`, `setImmediate` and `scheduler.wait` wait real
 *  time. */
const timerModules = ["node:timers", "node:timers/promises", "timers", "timers/promises"];

/** The calls that only read what they are handed and never call it, by the object they are of: a
 *  wait passed to one is read, as `typeof` reads it (`Object.hasOwn(setTimeout, "clock")`), where
 *  `Reflect.apply` and any other call may start it. `spyOn`, of any object or none, is one too: it
 *  takes the object whose member it replaces, and a call of that member is found as any is. */
const readers: Record<string, readonly string[]> = {
  Object: ["getOwnPropertyDescriptor", "getOwnPropertyNames", "hasOwn", "is", "keys"],
  Reflect: ["getOwnPropertyDescriptor", "has", "ownKeys"],
};

/** The one timer passed by its place: the `setTimeout` in the body of `afterOnFakeClock`, which
 *  refuses the real clock before it sets it. */
const fakeClockFile = "__tests__/fake-clock.ts";
const fakeClockHelper = "afterOnFakeClock";

/** Every `.ts(x)` file under `directory` but the act-rule inputs (`*.fixture.ts` and
 *  `*.fixture.tsx`). */
function sourceFiles(directory: string): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) return entry.name === "node_modules" ? [] : sourceFiles(path);
    if (!/\.tsx?$/.test(entry.name) || /\.fixture\.tsx?$/.test(entry.name)) return [];
    return [path];
  });
}

/** Whether `path` is a unit test file or a test helper, by its name or its folder. */
function isTestFile(path: string): boolean {
  return (
    /\.test\.tsx?$/.test(path) ||
    /-fixtures?\.tsx?$/.test(path) ||
    /\.coverage\.tsx?$/.test(path) ||
    path.split("/").includes("__tests__")
  );
}

/** A single- or double-quoted `./` or `../` path without its query, in a static import or
 *  re-export, an `import(…)`, a `require(…)`, or a `new URL(…)` such as a worker's. A matching
 *  spelling in a comment or string counts too. */
const loadedModule =
  /\b(?:from|import|require|new URL)\s*\(?\s*(?:"(\.{1,2}\/[^"?]+)|'(\.{1,2}\/[^'?]+))/g;

/** Helpers inferred from matched relative references (`loadedModule`). Try the path as written
 *  with its query removed, then `.ts`, `.tsx`, `index.ts` and `index.tsx`, within `texts` only.
 *  Add a file once it has at least one matched loader and all its matched loaders are tests or
 *  named helpers (`isTestFile`) or have already been added. A cycle of otherwise unrecognized
 *  helpers cannot start that process. */
function loadedByTestsAlone(texts: ReadonlyMap<string, string>): Set<string> {
  const loaders = new Map<string, string[]>();
  for (const [path, text] of texts) {
    for (const [, doubleQuoted, singleQuoted] of text.matchAll(loadedModule)) {
      const base = join(dirname(path), doubleQuoted ?? singleQuoted);
      const loaded = [
        base,
        `${base}.ts`,
        `${base}.tsx`,
        join(base, "index.ts"),
        join(base, "index.tsx"),
      ].find((each) => texts.has(each));
      if (loaded === undefined) continue;
      const by = loaders.get(loaded);
      if (by === undefined) loaders.set(loaded, [path]);
      else by.push(path);
    }
  }
  const helpers = new Set<string>();
  const testOrHelper = (path: string): boolean => isTestFile(path) || helpers.has(path);
  let grew = true;
  while (grew) {
    grew = false;
    for (const [path, by] of loaders) {
      if (testOrHelper(path) || !by.every(testOrHelper)) continue;
      helpers.add(path);
      grew = true;
    }
  }
  return helpers;
}

interface RealWait {
  readonly site: string;
  readonly reasoned: boolean;
}

/** What a name or an expression stands for, as far as a wait goes: a function whose call starts
 *  one (`wait`); "bun" whole, `Bun` among it, whose `sleep` and `sleepSync` are waits (`bun`); a
 *  timers module whole, whose `scheduler` is one and whose `promises` is another (`timers`); and a
 *  timers module's `scheduler`, whose `wait` is a wait (`scheduler`). */
type Kind = "wait" | "bun" | "timers" | "scheduler";

const kinds: readonly Kind[] = ["wait", "bun", "timers", "scheduler"];

/** Whether something stands for a kind. */
type Is = (kind: Kind) => boolean;

/** The strings each `const` of a file is bound to, by its name (`constantStrings`), which a
 *  member's key may be. */
type ConstantStrings = ReadonlyMap<string, readonly string[]>;

/** The names that stand for each kind in a file: the global ones (`setTimeout`, `Bun`), and every
 *  name a binding anywhere in the file gives one of those (`waitNames`); and `strings`, the
 *  file's `ConstantStrings`. */
type WaitNames = Record<Kind, Set<string>> & { readonly strings: ConstantStrings };

/** The operators whose value is one of their operands: `??`, `||` and `&&`. */
const choices: ReadonlySet<ts.SyntaxKind> = new Set([
  ts.SyntaxKind.QuestionQuestionToken,
  ts.SyntaxKind.BarBarToken,
  ts.SyntaxKind.AmpersandAmpersandToken,
]);

/** The operators that bind their left side to their right: `=`, `??=`, `||=` and `&&=`. */
const assignments: ReadonlySet<ts.SyntaxKind> = new Set([
  ts.SyntaxKind.EqualsToken,
  ts.SyntaxKind.QuestionQuestionEqualsToken,
  ts.SyntaxKind.BarBarEqualsToken,
  ts.SyntaxKind.AmpersandAmpersandEqualsToken,
]);

const noStrings: ConstantStrings = new Map();

/** The names a key may be: a string literal's text, or each string a `const` of the file named
 *  `key` is bound to (`strings`). */
function keysOf(key: ts.Expression, strings: ConstantStrings): readonly string[] {
  if (ts.isStringLiteralLike(key)) return [key.text];
  return ts.isIdentifier(key) ? (strings.get(key.text) ?? []) : [];
}

/** The object of a member and each name it may have: `window.setTimeout`, `window["setTimeout"]`,
 *  and `globalThis[key]` with a name for each string a `const key` of the file is bound to
 *  (`keysOf`), so a member reached by a constant key is read as the one it names. */
function member(
  expression: ts.Expression,
  strings: ConstantStrings = noStrings
): { object: ts.Expression; keys: readonly string[] } | undefined {
  if (ts.isPropertyAccessExpression(expression)) {
    return { keys: [expression.name.text], object: expression.expression };
  }
  if (!ts.isElementAccessExpression(expression)) return undefined;
  const keys = keysOf(expression.argumentExpression, strings);
  return keys.length === 0 ? undefined : { keys, object: expression.expression };
}

/** What the module `from` is whole: a timers module, "bun", or neither. */
function moduleIs(from: string): Is {
  return (kind) =>
    kind === "timers" ? timerModules.includes(from) : kind === "bun" && from === "bun";
}

/** What the member `name` of something `object` says stands for: a member of `waitCalls` of any
 *  object is a wait, and so is `sleep` or `sleepSync` of "bun" and `wait` of a scheduler; `Bun`
 *  of any object is "bun", `scheduler` of a timers module is a scheduler, and `promises` of one
 *  (`node:timers`' own `promises`) is a timers module. */
function memberIs(object: Is, name: string): Is {
  return (kind) => {
    if (kind === "bun") return name === "Bun";
    if (kind === "scheduler") return name === "scheduler" && object("timers");
    if (kind === "timers") return name === "promises" && object("timers");
    return (
      waitCalls.has(name) ||
      ((name === "sleep" || name === "sleepSync") && object("bun")) ||
      (name === "wait" && object("scheduler"))
    );
  };
}

/** The module `call` loads, where it is `import("…")` or `require("…")`. */
function loaded(call: ts.CallExpression): string | undefined {
  const [first] = call.arguments;
  const loads =
    call.expression.kind === ts.SyntaxKind.ImportKeyword ||
    (ts.isIdentifier(call.expression) && call.expression.text === "require");
  return loads && first !== undefined && ts.isStringLiteralLike(first) ? first.text : undefined;
}

/** `expression` without what hands its value on unchanged: parentheses, `await`, and a type
 *  assertion or a non-null one. */
function unwrapped(expression: ts.Expression): ts.Expression {
  let bare = expression;
  while (
    ts.isParenthesizedExpression(bare) ||
    ts.isAwaitExpression(bare) ||
    ts.isAsExpression(bare) ||
    ts.isSatisfiesExpression(bare) ||
    ts.isTypeAssertionExpression(bare) ||
    ts.isNonNullExpression(bare)
  ) {
    bare = bare.expression;
  }
  return bare;
}

/** Whether `expression` stands for `kind`, by the names `names` holds: a name, a member, the
 *  module an `import(…)` or a `require(…)` loads, a `bind` of a wait, or a choice of such (`??`,
 *  `||`, `&&`, `?:`, and the last of a comma, as in `(0, setTimeout)`). */
function is(expression: ts.Expression, kind: Kind, names: WaitNames): boolean {
  const bare = unwrapped(expression);
  if (ts.isIdentifier(bare)) return names[kind].has(bare.text);
  if (ts.isConditionalExpression(bare)) {
    return is(bare.whenTrue, kind, names) || is(bare.whenFalse, kind, names);
  }
  if (ts.isBinaryExpression(bare)) {
    if (bare.operatorToken.kind === ts.SyntaxKind.CommaToken) return is(bare.right, kind, names);
    return (
      choices.has(bare.operatorToken.kind) &&
      (is(bare.left, kind, names) || is(bare.right, kind, names))
    );
  }
  if (ts.isCallExpression(bare)) {
    const from = loaded(bare);
    if (from !== undefined) return moduleIs(from)(kind);
    return kind === "wait" && methodOfWait(bare.expression, ["bind"], names);
  }
  const reached = member(bare, names.strings);
  if (reached === undefined) return false;
  const object: Is = (each) => is(reached.object, each, names);
  return reached.keys.some((key) => memberIs(object, key)(kind));
}

/** Whether `expression` is one of the methods `methods` names of a wait (`setTimeout.bind`,
 *  `window["setTimeout"]["call"]`). */
function methodOfWait(
  expression: ts.Expression,
  methods: readonly string[],
  names: WaitNames
): boolean {
  const reached = member(unwrapped(expression), names.strings);
  if (reached === undefined) return false;
  return reached.keys.some((key) => methods.includes(key)) && is(reached.object, "wait", names);
}

/** A place a file binds a name, or the names of a destructure, to what `source` says. */
interface Binding {
  readonly target: ts.BindingName | ts.Expression;
  readonly source: Is;
}

/** The names a property's name may be: its text where it is written out (`setTimeout`,
 *  `"setTimeout"`), and a computed one's keys as `member()` reads them (`keysOf`: `["setTimeout"]`,
 *  or `[key]` of a `const key`). */
function propertyKeys(name: ts.PropertyName, strings: ConstantStrings): readonly string[] {
  return ts.isComputedPropertyName(name) ? keysOf(name.expression, strings) : [name.text];
}

/** What a destructure `target` binds of `source`: each of its names, or nested patterns, with the
 *  member of `source` it takes or the default it falls back to, and a rest with `source`'s module
 *  or `Bun` whole. */
function parts(target: ts.BindingName | ts.Expression, source: Is, names: WaitNames): Binding[] {
  const taken =
    (keys: readonly string[], fallback: ts.Expression | undefined): Is =>
    (kind) =>
      keys.some((key) => memberIs(source, key)(kind)) ||
      (fallback !== undefined && is(fallback, kind, names));
  const rest: Is = (kind) => kind !== "wait" && source(kind);
  if (ts.isObjectBindingPattern(target)) {
    return target.elements.map((element) => {
      if (element.dotDotDotToken !== undefined) return { source: rest, target: element.name };
      const keys =
        element.propertyName !== undefined
          ? propertyKeys(element.propertyName, names.strings)
          : ts.isIdentifier(element.name)
            ? [element.name.text]
            : [];
      return { source: taken(keys, element.initializer), target: element.name };
    });
  }
  if (!ts.isObjectLiteralExpression(target)) return [];
  return target.properties.flatMap((property): Binding[] => {
    if (ts.isSpreadAssignment(property)) return [{ source: rest, target: property.expression }];
    if (ts.isShorthandPropertyAssignment(property)) {
      const fallback = property.objectAssignmentInitializer;
      return [{ source: taken([property.name.text], fallback), target: property.name }];
    }
    if (!ts.isPropertyAssignment(property)) return [];
    const value = property.initializer;
    const defaulted =
      ts.isBinaryExpression(value) && value.operatorToken.kind === ts.SyntaxKind.EqualsToken;
    return [
      {
        source: taken(
          propertyKeys(property.name, names.strings),
          defaulted ? value.right : undefined
        ),
        target: defaulted ? value.left : value,
      },
    ];
  });
}

/** Binds each name `target` holds to what it takes of `source`; answers whether a name now stands
 *  for a kind it did not before. */
function bindTo(target: ts.BindingName | ts.Expression, source: Is, names: WaitNames): boolean {
  if (!ts.isIdentifier(target)) {
    return parts(target, source, names)
      .map((part) => bindTo(part.target, part.source, names))
      .includes(true);
  }
  let grew = false;
  for (const kind of kinds) {
    if (names[kind].has(target.text) || !source(kind)) continue;
    names[kind].add(target.text);
    grew = true;
  }
  return grew;
}

/** Every place `file` binds a name: an import, `import … = require(…)`, a declaration, a
 *  parameter's default, the default of a name a destructure binds, a shorthand default
 *  (`{ wait = setTimeout }`) wherever it stands, an assignment, and the first parameter of a
 *  callback that the `then` of a module's promise hands the module, each with what its value
 *  stands for, read from `names` as they stand when it is asked. */
function bindings(file: ts.SourceFile, names: WaitNames): Binding[] {
  const found: Binding[] = [];
  const standsFor =
    (expression: ts.Expression): Is =>
    (kind) =>
      is(expression, kind, names);
  const visit = (node: ts.Node): void => {
    if (ts.isImportDeclaration(node) && ts.isStringLiteral(node.moduleSpecifier)) {
      const whole = moduleIs(node.moduleSpecifier.text);
      const clause = node.importClause;
      const named = clause?.namedBindings;
      if (clause?.name !== undefined) found.push({ source: whole, target: clause.name });
      if (named !== undefined && ts.isNamespaceImport(named)) {
        found.push({ source: whole, target: named.name });
      } else if (named !== undefined) {
        for (const element of named.elements) {
          const imported = (element.propertyName ?? element.name).text;
          found.push({ source: memberIs(whole, imported), target: element.name });
        }
      }
    } else if (
      ts.isImportEqualsDeclaration(node) &&
      ts.isExternalModuleReference(node.moduleReference) &&
      ts.isStringLiteral(node.moduleReference.expression)
    ) {
      found.push({ source: moduleIs(node.moduleReference.expression.text), target: node.name });
    } else if (
      (ts.isVariableDeclaration(node) ||
        ts.isParameter(node) ||
        (ts.isBindingElement(node) && ts.isObjectBindingPattern(node.parent))) &&
      node.initializer !== undefined
    ) {
      // An object binding element's default binds its name or nested pattern even when the
      // scan does not read the destructure's input, as in a parameter, `for … of` or `catch`.
      found.push({ source: standsFor(node.initializer), target: node.name });
    } else if (
      // A shorthand default stands only in a destructure that assigns, which TypeScript parses as
      // an object literal: on the left of a `=`, at the head of a `for … of` or a `for … in`, or
      // nested in either, inside an array literal too. `parts` reads it only on the left of a
      // `=`, so it binds its name here, wherever it stands.
      ts.isShorthandPropertyAssignment(node) &&
      node.objectAssignmentInitializer !== undefined
    ) {
      found.push({ source: standsFor(node.objectAssignmentInitializer), target: node.name });
    } else if (ts.isBinaryExpression(node) && assignments.has(node.operatorToken.kind)) {
      found.push({ source: standsFor(node.right), target: node.left });
    } else if (ts.isCallExpression(node)) {
      // A `then` of a promise of a module, `import("bun").then((m) => …)`, hands its callback's
      // first parameter that module whole.
      const reached = member(unwrapped(node.expression), names.strings);
      const [handler] = node.arguments;
      const parameter =
        handler !== undefined && (ts.isArrowFunction(handler) || ts.isFunctionExpression(handler))
          ? handler.parameters[0]
          : undefined;
      if (reached?.keys.includes("then") && parameter !== undefined) {
        const module: Is = (kind) => kind !== "wait" && is(reached.object, kind, names);
        found.push({ source: module, target: parameter.name });
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  return found;
}

/** The strings each `const` of `file` is bound to, by its name (`const key = "setTimeout"`): a
 *  name bound to two in two scopes has both. */
function constantStrings(file: ts.SourceFile): ConstantStrings {
  const strings = new Map<string, string[]>();
  const visit = (node: ts.Node): void => {
    if (
      ts.isVariableDeclaration(node) &&
      ts.isIdentifier(node.name) &&
      node.initializer !== undefined &&
      (ts.getCombinedNodeFlags(node) & ts.NodeFlags.Const) !== 0
    ) {
      const value = unwrapped(node.initializer);
      if (ts.isStringLiteralLike(value)) {
        strings.set(node.name.text, [...(strings.get(node.name.text) ?? []), value.text]);
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  return strings;
}

/** The names that stand for each kind in `file`: the global ones, and every name a binding gives
 *  one of those; and the file's `ConstantStrings`. The bindings are read again, over the whole
 *  file, until none adds a name, so an alias of an alias is found, and so is one bound above the
 *  binding it names; a name is an alias in the whole file once any binding makes it one, and no
 *  function's data flow is followed but the module the `then` of its promise hands a callback. */
function waitNames(file: ts.SourceFile): WaitNames {
  const names: WaitNames = {
    bun: new Set(["Bun"]),
    scheduler: new Set(),
    strings: constantStrings(file),
    timers: new Set(),
    wait: new Set(waitCalls),
  };
  const found = bindings(file, names);
  let grew = true;
  while (grew) {
    grew = false;
    for (const { source, target } of found) if (bindTo(target, source, names)) grew = true;
  }
  return names;
}

/** Whether a call of `callee` starts a wait of real time: a wait, or the `call` or `apply` of one
 *  (`window.setTimeout.call(window, run, 50)`). */
function startsWait(callee: ts.Expression, names: WaitNames): boolean {
  return is(callee, "wait", names) || methodOfWait(callee, ["call", "apply"], names);
}

/** Whether `call` only reads its arguments: a call of one of `readers`, or of `spyOn`. */
function readsArguments(call: ts.CallExpression): boolean {
  const callee = unwrapped(call.expression);
  const reached = member(callee);
  if ((ts.isIdentifier(callee) ? callee.text : reached?.keys[0]) === "spyOn") return true;
  if (reached === undefined || !ts.isIdentifier(reached.object)) return false;
  const methods = readers[reached.object.text] ?? [];
  return reached.keys.some((key) => methods.includes(key));
}

/** Whether `node` is written to: the left side of an assignment, or of a `for … of` or a
 *  `for … in`, or a name or a nested pattern inside a destructure that is one (`nap` of
 *  `({ sleep: nap } = Bun)`). */
function isWritten(node: ts.Node): boolean {
  const parent = node.parent;
  if (ts.isBinaryExpression(parent)) {
    return assignments.has(parent.operatorToken.kind) && parent.left === node;
  }
  if (ts.isForOfStatement(parent) || ts.isForInStatement(parent)) {
    return parent.initializer === node;
  }
  if (ts.isArrayLiteralExpression(parent)) return isWritten(parent);
  const inPattern =
    (ts.isPropertyAssignment(parent) && parent.initializer === node) ||
    ts.isShorthandPropertyAssignment(parent) ||
    ts.isSpreadAssignment(parent) ||
    ts.isSpreadElement(parent);
  return inPattern && isWritten(parent.parent);
}

/** The kinds the value `node` reads stands for, where it is a name, a member, or a call that
 *  answers one (`bind`, `import(…)`, `require(…)`); none where `node` reads no value: a name that
 *  a declaration, a property or a member gives, a part of a type (the `setTimeout` of `typeof
 *  window.setTimeout` there), or a place an assignment writes. A name in `{ setTimeout }` is a
 *  value the object stores. */
function kindsRead(node: ts.Node, names: WaitNames): Kind[] {
  const candidate =
    ts.isIdentifier(node) ||
    ts.isPropertyAccessExpression(node) ||
    ts.isElementAccessExpression(node) ||
    ts.isCallExpression(node);
  if (!candidate) return [];
  const stands = kinds.filter((kind) => is(node, kind, names));
  if (stands.length === 0 || ts.findAncestor(node, ts.isTypeNode) !== undefined) return [];
  const parent = node.parent;
  const named =
    ts.isIdentifier(node) &&
    !ts.isShorthandPropertyAssignment(parent) &&
    (("name" in parent && parent.name === node) ||
      ("propertyName" in parent && parent.propertyName === node));
  return named || isWritten(node) ? [] : stands;
}

/** Where a value goes. `kept`: where the scan sees all that is done with it. `exported`: out of
 *  the file. `handed`: anywhere else out of the scan's view. */
type Place = "kept" | "handed" | "exported";

/** The places that only read or compare the value they hold, or drop it: `typeof`, `void`,
 *  `delete`, `!` and the other unary operators, the condition of `?:`, a statement of its own, and
 *  the condition of an `if`, a `while`, a `do` and a `switch`, and a `case`. */
const reads: ReadonlySet<ts.SyntaxKind> = new Set([
  ts.SyntaxKind.TypeOfExpression,
  ts.SyntaxKind.VoidExpression,
  ts.SyntaxKind.DeleteExpression,
  ts.SyntaxKind.PrefixUnaryExpression,
  ts.SyntaxKind.PostfixUnaryExpression,
  ts.SyntaxKind.ConditionalExpression,
  ts.SyntaxKind.ExpressionStatement,
  ts.SyntaxKind.IfStatement,
  ts.SyntaxKind.WhileStatement,
  ts.SyntaxKind.DoStatement,
  ts.SyntaxKind.SwitchStatement,
  ts.SyntaxKind.CaseClause,
]);

/** Whether `parent` hands on the value of its operand `at` as its own: parentheses, `await`, a
 *  type assertion or a non-null one, a branch of `?:`, either side of `??`, `||` and `&&`, and the
 *  last of a comma. */
function passesOn(parent: ts.Node, at: ts.Node): boolean {
  if (ts.isConditionalExpression(parent)) return parent.condition !== at;
  if (ts.isBinaryExpression(parent)) {
    const operator = parent.operatorToken.kind;
    return choices.has(operator) || (operator === ts.SyntaxKind.CommaToken && parent.right === at);
  }
  return ts.isExpression(parent) && unwrapped(parent) !== parent;
}

/** Where the value `reference` reads goes, past what hands it on unchanged (`passesOn`). It is
 *  kept where it is called, a member of it is read, it is read or compared (`reads`, a comparison,
 *  one of `readers`), an assignment writes to it, or a declaration, an assignment, a parameter's
 *  default or a destructure binds it to a name the scan follows (`waitNames`) - unless that
 *  declaration is exported, or that parameter is a parameter property, whose default goes into a
 *  field. It is handed on anywhere else: passed to a call or a `new`, stored in an object, an
 *  array, a member, a class's field or a JSX prop, returned, thrown or yielded. */
function placeOf(reference: ts.Node): Place {
  let at = reference;
  while (passesOn(at.parent, at)) at = at.parent;
  const parent = at.parent;
  const followed = (name: ts.BindingName): boolean =>
    ts.isIdentifier(name) || ts.isObjectBindingPattern(name);
  if (ts.isCallExpression(parent)) {
    return parent.expression === at || readsArguments(parent) ? "kept" : "handed";
  }
  if (
    (ts.isPropertyAccessExpression(parent) || ts.isElementAccessExpression(parent)) &&
    parent.expression === at
  ) {
    return "kept";
  }
  if (ts.isBinaryExpression(parent)) {
    if (!assignments.has(parent.operatorToken.kind) || parent.left === at) return "kept";
    return ts.isIdentifier(parent.left) || ts.isObjectLiteralExpression(parent.left)
      ? "kept"
      : "handed";
  }
  if (ts.isVariableDeclaration(parent)) {
    if (!followed(parent.name)) return "handed";
    return (ts.getCombinedModifierFlags(parent) & ts.ModifierFlags.Export) !== 0
      ? "exported"
      : "kept";
  }
  if (ts.isParameter(parent)) {
    if (ts.isParameterPropertyDeclaration(parent, parent.parent)) return "handed";
    return followed(parent.name) ? "kept" : "handed";
  }
  if (ts.isBindingElement(parent)) {
    return ts.isObjectBindingPattern(parent.parent) && followed(parent.name) ? "kept" : "handed";
  }
  if (ts.isShorthandPropertyAssignment(parent)) {
    return parent.objectAssignmentInitializer === at ? "kept" : "handed";
  }
  if (ts.isExportAssignment(parent)) return "exported";
  return reads.has(parent.kind) ? "kept" : "handed";
}

/** What `declaration` exports that stands for a wait or for what holds one: each name it exports
 *  that does, of the file's own or of the module it names, or the whole of a timers module or
 *  "bun" (`export * from "node:timers/promises"`). */
function exportedWaits(declaration: ts.ExportDeclaration, names: WaitNames): ts.Node[] {
  if (declaration.isTypeOnly) return [];
  const specifier = declaration.moduleSpecifier;
  const from =
    specifier !== undefined && ts.isStringLiteral(specifier) ? specifier.text : undefined;
  const clause = declaration.exportClause;
  if (clause === undefined || ts.isNamespaceExport(clause)) {
    return from !== undefined && kinds.some(moduleIs(from)) ? [declaration] : [];
  }
  return clause.elements.filter((element) => {
    const local = (element.propertyName ?? element.name).text;
    const source: Is =
      from === undefined ? (kind) => names[kind].has(local) : memberIs(moduleIs(from), local);
    return !element.isTypeOnly && kinds.some(source);
  });
}

/** Whether `wait` stands in the body of `fake-clock.ts`'s `afterOnFakeClock`, and in no function
 *  nested in it: the `setTimeout` there. */
function isFakeClockTimer(file: string, wait: ts.Node): boolean {
  const enclosing = ts.findAncestor(wait.parent, ts.isFunctionLike);
  return (
    file === fakeClockFile &&
    enclosing !== undefined &&
    ts.isFunctionDeclaration(enclosing) &&
    enclosing.name?.text === fakeClockHelper
  );
}

/** The comment block directly above `statement`: the comments before it, back from the last one,
 *  while no blank line parts one from the next or the last from the statement. Empty where a
 *  blank line parts the last comment from it, or no comment stands above it on a line of its own
 *  (a comment after the statement before is that one's). */
function commentAbove(text: string, statement: ts.Node, source: ts.SourceFile): string {
  const block: string[] = [];
  let end = statement.getStart(source);
  for (const range of (ts.getLeadingCommentRanges(text, statement.pos) ?? []).reverse()) {
    if (blankLine.test(text.slice(range.end, end))) break;
    block.unshift(text.slice(range.pos, range.end));
    end = range.pos;
  }
  return block.join("\n");
}

/** Whether `block` says why a wait stays on the real clock: it holds `MARKER`, and the rest of the
 *  block after it holds `REASON_WORDS` words of letters or more. */
function saysWhy(block: string): boolean {
  const marker = block.indexOf(MARKER);
  if (marker === -1) return false;
  const reason = block.slice(marker + MARKER.length);
  return (reason.match(wordOfLetters) ?? []).length >= REASON_WORDS;
}

/** Whether a marker covers `wait`, one of the file's `waits`: a statement, a declaration or a
 *  class member that holds it - its own, or a helper around it - has a marker directly above it
 *  (`commentAbove`, `saysWhy`), and holds no other of `waits`. */
function isMarked(
  wait: ts.Node,
  waits: readonly ts.Node[],
  text: string,
  source: ts.SourceFile
): boolean {
  for (let at: ts.Node | undefined = wait; at !== undefined; at = at.parent) {
    if (!ts.isStatement(at) && !ts.isClassElement(at)) continue;
    const statement = at;
    const held = waits.filter((each) => each.pos >= statement.pos && each.end <= statement.end);
    if (held.length === 1 && saysWhy(commentAbove(text, statement, source))) return true;
  }
  return false;
}

/** The waits of real time `text` starts, each with whether a marker directly above it says why it
 *  stays on the real clock (`isMarked`): every call of a wait, and every reference to a wait that
 *  the file hands on out of the scan's view (`placeOf`), which whatever it reaches may start. A
 *  reference to what holds waits - `Bun`, a timers module, a scheduler - is found where the file
 *  exports it, since the file that imports it calls its waits under a name the scan does not
 *  know. */
export function realWaits(file: string, text: string): RealWait[] {
  const source = ts.createSourceFile(
    file,
    text,
    ts.ScriptTarget.Latest,
    true,
    file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS
  );
  const names = waitNames(source);
  const lines = text.split("\n");
  const waits: ts.Node[] = [];
  const visit = (node: ts.Node): void => {
    if (ts.isCallExpression(node) && startsWait(node.expression, names)) waits.push(node);
    if (ts.isExportDeclaration(node)) {
      waits.push(...exportedWaits(node, names));
    } else {
      const stands = kindsRead(node, names);
      const place = stands.length === 0 ? "kept" : placeOf(node);
      if (stands.includes("wait") ? place !== "kept" : place === "exported") waits.push(node);
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  const real = waits.filter((wait) => !isFakeClockTimer(file, wait));
  return real.map((wait) => {
    const index = source.getLineAndCharacterOfPosition(wait.getStart(source)).line;
    return {
      reasoned: isMarked(wait, real, text, source),
      site: `${file}:${index + 1}: ${(lines[index] ?? "").trim()}`,
    };
  });
}

/** Every source file under `web/src` with its text, read once for the scan and its self-checks. */
const texts = new Map(sourceFiles(webSource).map((path) => [path, readFileSync(path, "utf8")]));
const helpers = loadedByTestsAlone(texts);
const files = [...texts].filter(
  ([path]) => (isTestFile(path) || helpers.has(path)) && !path.endsWith("real-clock-waits.test.ts")
);
const waits = files.flatMap(([path, text]) => realWaits(relative(webSource, path), text));

test("the scan reads the unit suite and finds the waits it keeps", () => {
  expect(files.length).toBeGreaterThan(100);
  // Margin.landing.test.tsx's `quiet` stays on the real clock with its reason.
  expect(waits.some((wait) => wait.reasoned)).toBe(true);
});

// A file that imports `bun:test`, Testing Library or a module of a `__tests__` folder is a test
// file or a test helper, as the `*.coverage.ts` lists are, which import
// `__tests__/composer-census/coverage.ts`; the scan reads each by its name.
test("the scan reads every file that imports bun:test, Testing Library or a __tests__ module", () => {
  const unread = [...texts]
    .filter(
      ([path, text]) =>
        !isTestFile(path) &&
        /from "(?:bun:test|@testing-library\/[\w-]+|[^"]*\/__tests__\/[^"]*)"/.test(text)
    )
    .map(([path]) => relative(webSource, path));
  expect(unread, "a test helper the scan does not read by its name").toEqual([]);
});

// Five test files import `features/issue-page/chromium-focus-fixup.ts` to make happy-dom drop
// focus as Chromium does, and three test files import `fonts/inter/subset-script.ts` to read
// subset.sh. The sample exercises a chain of test-only helpers and a worker that both a test
// and a product file load.
test("the scan follows a test-only import chain and leaves out a worker the page loads", () => {
  const sample = new Map([
    ["row.test.tsx", 'import { emulate } from "./focus";\nimport { Page } from "./page";'],
    ["focus.ts", "import { nextFrame } from './frames';"],
    ["frames/index.ts", "export const nextFrame = () => {};"],
    ["page.tsx", 'const worker = new Worker(new URL("./parse.worker.ts", import.meta.url));'],
    ["parse.worker.ts", "export function parse() {}"],
    ["parse.test.ts", 'import { parse } from "./parse.worker";'],
    ["main.tsx", 'import { Page } from "./page";'],
  ]);
  expect([...loadedByTestsAlone(sample)].sort()).toEqual(["focus.ts", "frames/index.ts"]);
  const read = files.map(([path]) => relative(webSource, path));
  expect(read).toEqual(
    expect.arrayContaining([
      "features/issue-page/chromium-focus-fixup.ts",
      "fonts/inter/subset-script.ts",
    ])
  );
});

/** A marker a kept wait could carry: the words, and a reason after them. */
const MARKED =
  "// This one stays on the real clock (INFRA-1505): a real long task that no fake clock can stand in for.";

// The reason after the marker words is a sentence: the one word "why." these rows once carried
// says nothing, and the marker's own row below refuses it.
test("a wait of real time is found, a zero-delay one too, and its reason is read from the comment above it", () => {
  const bare = ["await act(async () => {", "  setTimeout(waited.resolve, 50);", "});"].join("\n");
  const kept = [MARKED, bare].join("\n");
  const zero = "setTimeout(resolve, 0);";
  const zeroKept = [MARKED, zero].join("\n");
  expect(realWaits("bare.test.ts", bare).map((wait) => wait.reasoned)).toEqual([false]);
  expect(realWaits("kept.test.ts", kept).map((wait) => wait.reasoned)).toEqual([true]);
  expect(realWaits("zero.test.ts", zero).map((wait) => wait.reasoned)).toEqual([false]);
  expect(realWaits("zero-kept.test.ts", zeroKept).map((wait) => wait.reasoned)).toEqual([true]);
  expect(realWaits("sleep.test.ts", "await Bun.sleep(0);").map((wait) => wait.reasoned)).toEqual([
    false,
  ]);
});

// docs/testing.md, "The clock of a unit test", says this scan fails every timer whatever its delay
// and whatever its callback. These are the spellings of an awaited timer the scan once found by
// tracing the resolver a `new Promise` hands its executor, `r` or `done` as often as `resolve`,
// called from an arrow too; it traces no resolver now, and each is still found once. Each of these
// waits 50 ms of real time.
test("an awaited timer is found whatever its resolver is called and however the timer calls it", () => {
  const spellings = [
    "await new Promise((r) => setTimeout(r, 50));",
    "await new Promise<void>((done) => {\n  setTimeout(done, 50);\n});",
    "await new Promise((resolve) => setTimeout(() => resolve(), 50));",
  ];
  expect(spellings.map((text) => realWaits("spelling.test.ts", text).length)).toEqual([1, 1, 1]);
});

// A timer is found however it is reached and whatever its callback does, each once: with no delay,
// a resolver taken from `Promise.withResolvers` whole or under a name of its own, a member
// `setTimeout`, inside `act(...)`, a resolver stored under another name, a function that calls one,
// and the two this row once listed as found not at all because they settle no promise of the row's:
// a timer that stands in for a product's (`setShown`) and a timer whose callback calls
// `Promise.resolve`. Each waits real time all the same, and a row that waits for one waits on how
// loaded the machine is. A row that stands in for an answer sets its timer with `afterOnFakeClock`,
// which refuses the real clock and is passed; and only a comment or a string that spells a timer is
// found not at all.
test("a timer is found however it is reached and whatever its callback does, and one spelled in a comment or a string is not", () => {
  const found = [
    "await new Promise((resolve) => setTimeout(resolve));",
    "const w = Promise.withResolvers<void>();\nsetTimeout(w.resolve, 50);\nawait w.promise;",
    "const { promise, resolve: land } = Promise.withResolvers<void>();\nsetTimeout(land, 50);\nawait promise;",
    "await new Promise((r) => window.setTimeout(r, 50));",
    "await act(() => new Promise((r) => setTimeout(r, 50)));",
    "let release = () => {};\nconst held = new Promise<void>((r) => {\n  release = r;\n});\nsetTimeout(release, 50);\nawait held;",
    "setTimeout(function () {\n  landed = true;\n  older.resolve(value);\n}, 50);",
    "const timer = setTimeout(() => setShown(true), 50);",
    "setTimeout(() => void Promise.resolve(), 0);",
  ];
  expect(found.map((text) => realWaits("found.test.tsx", text).length)).toEqual(found.map(() => 1));
  expect(realWaits("answer.test.tsx", "afterOnFakeClock(50, () => setShown(true));")).toEqual([]);
  const none = [
    "// await new Promise((r) => setTimeout(r, 50));",
    'const source = "await new Promise((r) => setTimeout(r, 50));";',
  ];
  expect(none.map((text) => realWaits("none.test.tsx", text).length)).toEqual(none.map(() => 0));
});

// docs/testing.md says this scan fails a `Bun.sleep` and an awaited timer. Bun's sleep is also
// imported by name from "bun", and the standard way to wait in Node, which Bun implements, is the
// promise `setTimeout` of "node:timers/promises", often imported under another name. Each of these
// waits 50 ms of real time.
test("Bun's sleep imported by name and the promise setTimeout of node:timers/promises are found", () => {
  const imported = [
    'import { sleep } from "bun";\nawait sleep(50);',
    'import { sleep as pause } from "bun";\nawait pause(50);',
    'import { setTimeout } from "node:timers/promises";\nawait setTimeout(50);',
    'import { setTimeout as delay } from "node:timers/promises";\nawait delay(50);',
  ];
  expect(imported.map((text) => realWaits("imported.test.ts", text).length)).toEqual(
    imported.map(() => 1)
  );
});

// The scan follows no callback, because a trace of a resolver through the data flow always misses
// a form: here the resolver `r` is handed to a helper, whose own `setTimeout` waits 50 ms of real
// time for it. The wait is found where it starts, in the helper, and refused for want of a reason.
test("a timer is found in the helper a resolver is handed to", () => {
  const helper = [
    "function after(ms: number, done: () => void) { setTimeout(done, ms); }",
    "await new Promise<void>((r) => after(50, r));",
  ].join("\n");
  expect(realWaits("helper.test.ts", helper)).toEqual([
    {
      reasoned: false,
      site: "helper.test.ts:1: function after(ms: number, done: () => void) { setTimeout(done, ms); }",
    },
  ]);
});

// Every other call that starts a wait of real time is found once and refused without a reason:
// the timers of node:timers and its promises, under the bare specifiers too, named, aliased, or
// reached through a namespace or a default import; Bun's sleeps, the blocking one included, by
// `Bun` or by an import from "bun"; a `setInterval`; and a `setTimeout` of `globalThis`, of `self`
// or of a window reached by its key. An import alone binds a name and starts no wait.
test("every timer of node:timers, its promises and Bun is found, however an import names it", () => {
  const refused = [
    'import { setTimeout as delay } from "node:timers/promises";\nawait delay(50);',
    'import { scheduler } from "node:timers/promises";\nawait scheduler.wait(50);',
    'import { scheduler as later } from "timers/promises";\nawait later.wait(50);',
    'import * as timers from "node:timers/promises";\nawait timers.setTimeout(50);',
    'import * as timers from "node:timers/promises";\nawait timers.scheduler.wait(50);',
    'import timers from "node:timers";\ntimers.setInterval(run, 50);',
    'import { setTimeout as later } from "timers";\nlater(run, 50);',
    'import { setInterval as every } from "node:timers";\nevery(run, 50);',
    "Bun.sleepSync(50);",
    'import { sleepSync } from "bun";\nsleepSync(50);',
    'import { sleepSync as block } from "bun";\nblock(50);',
    'import * as bun from "bun";\nawait bun.sleep(50);',
    "setInterval(run, 50);",
    "globalThis.setTimeout(run, 50);",
    "self.setInterval(run, 50);",
    'window["setTimeout"](run, 50);',
  ];
  expect(
    refused.map((text) => realWaits("refused.test.ts", text).map((wait) => wait.reasoned))
  ).toEqual(refused.map(() => [false]));
  const imports = [
    'import { setTimeout as delay } from "node:timers/promises";',
    'import { scheduler } from "node:timers/promises";',
    'import { sleep, sleepSync } from "bun";',
  ];
  expect(imports.map((text) => realWaits("imports.test.ts", text).length)).toEqual(
    imports.map(() => 0)
  );
});

// A frame and a turn of the loop are waits of the kind a zero-delay timer is, by the architect's
// ruling on dispatch://INFRA-983: a row that awaits one waits one turn for a thing to have
// happened, instead of waiting for the thing. Each of these is found once and refused without a
// reason: a `requestAnimationFrame` and a `setImmediate`, global, of `window` or of `globalThis`,
// awaited or not, inside `act(...)` too; and the `setImmediate` of node:timers and its promises,
// aliased, or reached through a namespace or a default import. Under the marker words and a
// reason each passes, as a timer does; and a spy on the frame, a cancel of a frame or a turn, and
// an import alone start no wait.
test("a frame and a turn are found, global or a member, and the setImmediate of node:timers however an import names it", () => {
  const refused = [
    "requestAnimationFrame(run);",
    "window.requestAnimationFrame(run);",
    "globalThis.requestAnimationFrame(run);",
    "await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()));",
    "await act(() => new Promise<void>((done) => window.requestAnimationFrame(() => done())));",
    "setImmediate(run);",
    "window.setImmediate(run);",
    "globalThis.setImmediate(run);",
    "await new Promise<void>((resolve) => setImmediate(resolve));",
    'import { setImmediate as nextTurn } from "node:timers";\nnextTurn(run);',
    'import { setImmediate as nextTurn } from "node:timers/promises";\nawait nextTurn();',
    'import * as timers from "node:timers";\ntimers.setImmediate(run);',
    'import timers from "timers";\ntimers.setImmediate(run);',
  ];
  expect(
    refused.map((text) => realWaits("refused.test.tsx", text).map((wait) => wait.reasoned))
  ).toEqual(refused.map(() => [false]));
  const kept = [MARKED, "requestAnimationFrame(run);", MARKED, "setImmediate(run);"].join("\n");
  expect(realWaits("kept.test.tsx", kept).map((wait) => wait.reasoned)).toEqual([true, true]);
  const none = [
    'const frame = spyOn(window, "requestAnimationFrame").mockImplementation(() => 0);',
    "cancelAnimationFrame(frame);",
    "clearImmediate(turn);",
    'import { setImmediate as nextTurn } from "node:timers/promises";',
  ];
  expect(none.map((text) => realWaits("none.test.tsx", text).length)).toEqual(none.map(() => 0));
});

/** Expects each of `texts` to start one wait, refused without a reason, and to pass under
 *  `MARKED` put on a line of its own directly above the line that wait starts on. */
function refusedAndKept(texts: readonly string[]): void {
  const bare = texts.map((text) => realWaits("alias.test.tsx", text));
  expect(bare.map((found) => found.map((wait) => wait.reasoned))).toEqual(texts.map(() => [false]));
  const marked = texts.map((text, at) => {
    const line = Number(/^[^:]+:(\d+):/.exec(bare[at]?.[0]?.site ?? "")?.[1]) - 1;
    const lines = text.split("\n");
    return [...lines.slice(0, line), MARKED, ...lines.slice(line)].join("\n");
  });
  expect(
    marked.map((text) => realWaits("alias.test.tsx", text).map((wait) => wait.reasoned))
  ).toEqual(texts.map(() => [true]));
}

// docs/testing.md, "The clock of a unit test", lists the calls this scan finds by a name, among
// them every name and destructure a wait is given and every `call` of one. A reader found five
// spellings, each waiting 50 ms, that the scan did not find: Bun's sleep destructured from `Bun`,
// a `setTimeout` under a name of the row's own, the promise `setTimeout` destructured from a
// dynamic `import` and from a `require` of node:timers/promises, and a `setTimeout` called
// through its `call`. Each is found once, at the line of its call, and passes under the marker.
test("the five spellings a reader found are each found at the line of their call", () => {
  const spellings = [
    "const { sleep } = Bun;\nawait sleep(50);",
    "const later = setTimeout;\nlater(run, 50);",
    'const { setTimeout: delay } = await import("node:timers/promises");\nawait delay(50);',
    'const { setTimeout: wait } = require("node:timers/promises");\nawait wait(50);',
    "window.setTimeout.call(window, run, 50);",
  ];
  expect(spellings.map((text) => realWaits("spelled.test.ts", text))).toEqual(
    spellings.map((text) => {
      const lines = text.split("\n");
      return [{ reasoned: false, site: `spelled.test.ts:${lines.length}: ${lines.at(-1)}` }];
    })
  );
  refusedAndKept(spellings);
});

// A name bound to a wait is a wait wherever the file calls it: a declaration or an assignment
// whose value is a wait, a member such as `Bun.sleep`, another alias, or a choice of one (`??`,
// `?:`, the last of a comma); a parameter's default; and a destructure, renamed or not, nested or
// with a default, from `Bun`, from "bun" imported whole, `import`ed or `require`d, from a timers
// module the same ways, and of a `setTimeout` from any object. A name is an alias in the whole
// file once any binding makes it one, above or below its call and through any number of others.
// The id a timer answers is no alias: the timer is the one wait.
test("a name or a destructure that one of the waits is given is found where it is called", () => {
  refusedAndKept([
    "let later;\nlater = window.setTimeout;\nlater(run, 50);",
    "var nap = Bun.sleep;\nawait nap(50);",
    "const a = setImmediate;\nconst b = a;\nb(run);",
    "function go() {\n  pause(50);\n}\nconst pause = nap;\nconst nap = Bun.sleepSync;",
    "const wait = options.wait ?? setTimeout;\nwait(run, 50);",
    "const frame = ready ? requestAnimationFrame : run;\nframe(run);",
    "(0, setTimeout)(run, 50);",
    "function after(ms: number, wait = setTimeout) {\n  wait(run, ms);\n}",
    "const { sleep: nap } = Bun;\nawait nap(50);",
    "const { sleepSync } = globalThis.Bun;\nsleepSync(50);",
    'import * as bun from "bun";\nconst { sleep } = bun;\nawait sleep(50);',
    'const { sleep } = await import("bun");\nawait sleep(50);',
    'const { sleepSync: block } = require("bun");\nblock(50);',
    'const {\n  scheduler: { wait },\n} = await import("node:timers/promises");\nawait wait(50);',
    'const { setImmediate: turn } = await import("node:timers");\nturn(run);',
    "const { setTimeout: later } = window;\nlater(run, 50);",
    "const { later = setTimeout } = options;\nlater(run, 50);",
    "let nap;\n({ sleep: nap } = Bun);\nawait nap(50);",
    "const timer = setTimeout(run, 50);\nclearTimeout(timer);",
  ]);
});

// docs/testing.md, "The clock of a unit test", says the scan finds any name or destructure a wait
// is given, and a reference to one that the file stores. The first review of
// cybermonklabs/legion#4523 found five spellings, each waiting 50 ms, of which the scan found
// none: the default of a name destructured from a parameter, an arrow's too, from a `for … of`
// and from a `catch`, none of which hands the destructure a value the scan reads; and a parameter
// property's default, which the constructor stores in a field and the scan read as a parameter it
// follows. Each is found once, the field's at the reference as any value stored is, and passes
// under the marker; so is the default of a nested destructure of a parameter.
test("a destructure's default in a parameter, a for of or a catch, and a parameter property's default, are found", () => {
  refusedAndKept([
    "async function settle({ ms = 50, sleep = Bun.sleep }: Options) {\n  await sleep(ms);\n}",
    "const settle = ({ nap = Bun.sleep }) => nap(50);",
    "for (const { wait = setTimeout } of options) {\n  wait(run, 50);\n}",
    "try {\n  run();\n} catch ({ wait = setTimeout }) {\n  wait(run, 50);\n}",
    "class Clock {\n  constructor(private wait = setTimeout) {}\n  start() {\n    this.wait(run, 50);\n  }\n}",
    "async function settle({ timers: { sleep } = Bun }: Options) {\n  await sleep(50);\n}",
  ]);
});

// The class search of that review found three more spellings, each waiting 50 ms, of which the
// scan found none: a shorthand default (`{ wait = setTimeout }`) in a destructure that assigns,
// at the head of a `for … of`, at the head of a `for … in`, and inside an array that is the left
// of a `=`. Each is found once, at its call, and passes under the marker. The default of a name an
// array destructure binds, in a declaration or a parameter, binds a name the scan does not
// follow: it is found once, at the reference, as a value handed on, and passes under the marker
// too.
test("a shorthand default in a for of, a for in or an array that assigns, and an array destructure's default, are found", () => {
  refusedAndKept([
    "for ({ wait = setTimeout } of options) {\n  wait(run, 50);\n}",
    "for ({ wait = setTimeout } in options) {\n  wait(run, 50);\n}",
    "[{ wait = setTimeout }] = [options];\nwait(run, 50);",
    "const [wait = setTimeout] = delays;\nwait(run, 50);",
    "const settle = ([wait = setTimeout]) => wait(run, 50);",
  ]);
});

// A timers module or "bun" bound whole by a dynamic `import`, a `require` or an `import … =
// require` is what a namespace import binds, and its members start the waits that one's do; so do
// the members of one loaded in place.
test("a timers module or bun bound by import() or require() starts a wait through its members", () => {
  refusedAndKept([
    'const t = await import("node:timers/promises");\nawait t.scheduler.wait(50);',
    'const t = require("timers/promises");\nconst { scheduler } = t;\nawait scheduler.wait(50);',
    'const b = require("bun");\nawait b.sleep(50);',
    'const b = await import("bun");\nb.sleepSync(50);',
    'import b = require("bun");\nawait b.sleep(50);',
    'const timers = require("node:timers");\ntimers.setInterval(run, 50);',
    '(await import("bun")).sleep(50);',
    '(await import("node:timers/promises")).scheduler.wait(50);',
  ]);
});

// docs/testing.md, "The clock of a unit test", says the scan fails the `scheduler.wait` of
// node:timers "reached through a namespace or default import", and every wait "imported ... under
// another name" or given a destructure. In Bun, as in Node, node:timers holds its scheduler under
// `promises`, and a dynamic import's `then` hands its callback the namespace. Each of these waits
// 50 ms of real time, and the scan found none of them at 54a60643 (INFRA-983, tester round 4).
test("the scheduler of node:timers' promises and a namespace a dynamic import hands on start waits", () => {
  refusedAndKept([
    'import * as timers from "node:timers";\nawait timers.promises.scheduler.wait(50);',
    'import { promises } from "node:timers";\nawait promises.scheduler.wait(50);',
    'await import("node:timers/promises").then(({ setTimeout: d }) => d(50));',
    'await import("bun").then((m) => m.sleep(50));',
  ]);
});

// The same section says the scan finds a timer that is a member of any object by its key, a
// string literal or a `const` of the file bound to one (`globalThis[key]`). A timer of
// `globalThis` reached by a key held in a constant waits 50 ms of real time, and the scan did not
// find it at 54a60643 (INFRA-983, tester round 4).
test("a timer of globalThis reached by a constant key starts a wait", () => {
  refusedAndKept(['const key = "setTimeout";\nawait new Promise((r) => globalThis[key](r, 50));']);
});

// A constant key reads the member it names of `window`, of `self` and of any other object, as a
// string literal does: a `const` bound to a string, one bound `as const` or to a template with no
// substitution, and a name that two scopes bind to two strings, which may be either. A method of a
// wait is read by one too, and so is a destructure's computed key (`{ [key]: later }`).
test("a constant key of window, self or any object reads the member it names", () => {
  refusedAndKept([
    'const key = "requestAnimationFrame";\nwindow[key](run);',
    'const key = "setImmediate" as const;\nself[key](run);',
    "const key = `setInterval`;\nglobalThis[key](run, 50);",
    'const method = "sleep";\nawait Bun[method](50);',
    'function focusOn() {\n  const key = "focus";\n}\nconst key = "setTimeout";\nwindow[key](run, 50);',
    'const call = "call";\nsetTimeout[call](window, run, 50);',
    'const key = "setTimeout";\nconst { [key]: later } = window;\nlater(run, 50);',
  ]);
});

// The `call` and the `apply` of a wait start it, by a member's key too, and so does a call of its
// `bind`, at once or under a name: what `bind` answers is an alias, and its own `call` is a call.
test("the call, the apply and the bind of a wait are found where the wait is started", () => {
  refusedAndKept([
    "setTimeout.call(window, run, 50);",
    "requestAnimationFrame.apply(window, [run]);",
    "await Bun.sleep.call(Bun, 50);",
    'window["setImmediate"]["call"](window, run);',
    "const later = setTimeout;\nlater.apply(null, [run, 50]);",
    "const later = window.setTimeout.bind(window);\nlater(run, 50);",
    "setImmediate.bind(null, run)();",
    "const bound = setTimeout.bind(window, run);\nbound.call(null, 50);",
    'import { scheduler } from "node:timers/promises";\nconst wait = scheduler.wait.bind(scheduler);\nawait wait(50);',
  ]);
});

// A wait passed to another call or a `new` as an argument is handed to code the scan does not
// read, which may start it, as `Reflect.apply` does: it is found as a call, at the argument, and
// passes as one does, under the marker or in `afterOnFakeClock`'s own body. A reference that is
// only read or compared - the name `spyOn` takes, `typeof`, a comparison passed to `expect`, a
// wait handed to `Object.hasOwn` or `Reflect.has`, which read it and call nothing - starts none,
// and neither does an alias or a `bind` nobody calls or passes on, nor a `sleep` of the row's own
// that is not Bun's.
test("a wait passed to another call is found, and one only read or compared is not", () => {
  refusedAndKept([
    "await Promise.all([50].map(Bun.sleep));",
    "await act(setTimeout);",
    "await new Promise(setImmediate);",
    "await promise.then(requestAnimationFrame);",
    "Reflect.apply(setTimeout, window, [run, 50]);",
    "const later = setTimeout;\nschedule(later);",
    "act(setTimeout.bind(window, run, 50));",
    'import { setTimeout as delay } from "node:timers/promises";\nawait retry(delay);',
  ]);
  const helper =
    "export function afterOnFakeClock(ms: number, fn: () => void) {\n  return Reflect.apply(setTimeout, globalThis, [fn, ms]);\n}";
  expect(realWaits("__tests__/fake-clock.ts", helper)).toEqual([]);
  expect(realWaits("__tests__/other-clock.ts", helper)).toHaveLength(1);
  const none = [
    'spyOn(window, "setTimeout").mockImplementation(run);',
    'expect(typeof setTimeout).toBe("function");',
    "expect(globalThis.setTimeout === window.setTimeout).toBe(true);",
    'if (typeof requestAnimationFrame === "function") run();',
    "const later = setTimeout;",
    "const later = setTimeout.bind(window);",
    "const { sleep } = helpers;\nawait sleep(50);",
    'expect(Object.hasOwn(setTimeout, "clock")).toBe(true);',
    'Reflect.has(window.requestAnimationFrame, "clock");',
  ];
  expect(none.map((text) => realWaits("none.test.ts", text).length)).toEqual(none.map(() => 0));
});

// Every reference to a wait that leaves the scan's view is a wait, found at the reference as an
// argument is, passing under the marker or in `afterOnFakeClock`'s own body: one stored in an
// object, under a key or under its own name, in an array, a member, a class's field or a JSX prop;
// one returned, by an arrow too, thrown or yielded; and one exported, by a declaration, by a name
// of the file's own or of a module, or as the default. `Bun`, a timers module and a scheduler hold
// waits that a file importing one calls under names the scan does not know, so each is found where
// it is exported, and so is `export *` of such a module.
test("a wait stored, given as a JSX prop, returned, thrown, yielded or exported is found at the reference", () => {
  refusedAndKept([
    "const timers = { wait: setTimeout };",
    "const fakes = { setTimeout, clearTimeout };",
    "const steps = [run, Bun.sleep];",
    "options.wait = setTimeout;",
    "const original = window.requestAnimationFrame;\nwindow.requestAnimationFrame = original;",
    "class Clock {\n  wait = setImmediate;\n}",
    "render(<Clock schedule={requestAnimationFrame} />);",
    "function nextTurn() {\n  return setImmediate;\n}",
    "const pick = () => window.setTimeout;",
    "throw Bun.sleep;",
    "function* waits() {\n  yield setTimeout;\n}",
    "export const pause = Bun.sleep;",
    "export { setTimeout as later };",
    "const later = setTimeout;\nexport { later };",
    "export default requestAnimationFrame;",
    'export { setTimeout as delay } from "node:timers/promises";',
    'export { sleep } from "bun";',
    'export * from "node:timers/promises";',
    'export * as clock from "bun";',
    "export const bun = Bun;",
    'export const timers = await import("node:timers/promises");',
    'import * as timers from "node:timers/promises";\nexport { timers };',
    "export default Bun;",
  ]);
  const helper =
    "export function afterOnFakeClock(ms: number, fn: () => void) {\n  const timers = { wait: setTimeout };\n  return timers.wait(fn, ms);\n}";
  expect(realWaits("__tests__/fake-clock.ts", helper)).toEqual([]);
  expect(realWaits("__tests__/other-clock.ts", helper)).toHaveLength(1);
});

// What the scan sees all of hands nothing on: a member of a wait read (`setTimeout.length`), an
// assignment that writes to one, a declaration or a destructure the scan follows, a condition, a
// `void`, a `!`, a comparison and a `case`, a type (`typeof setTimeout` there), a value of the
// row's own stored, returned or exported, and `Bun` handed to `spyOn`, which reads the object
// whose member it replaces.
test("a wait read, compared, written to, bound or named in a type hands nothing on", () => {
  const none = [
    "const arity = setTimeout.length;",
    "window.setTimeout = run;",
    "const restore = window.requestAnimationFrame;",
    "const { sleep } = Bun;",
    "if (window.requestAnimationFrame) run();",
    "void setTimeout;",
    "const missing = !setImmediate;",
    "const same = schedule === setTimeout;",
    "switch (schedule) {\n  case setTimeout:\n    run();\n}",
    "type Timer = ReturnType<typeof setTimeout>;",
    "const held = spy as unknown as typeof window.setTimeout;",
    "const timers = { wait: run };",
    "render(<Clock schedule={run} />);",
    "function version() {\n  return Bun.version;\n}",
    'const spawned = spyOn(Bun, "spawn");',
    "export const later = run;",
  ];
  expect(none.map((text) => realWaits("none.test.tsx", text).length)).toEqual(none.map(() => 0));
});

// A wait passes when the comment block directly above the statement that holds it, with no blank
// line between, holds the words "stays on the real clock" and after them, anywhere in the block, a
// reason of `REASON_WORDS` words of letters or more: one comment, the comment lines that continue
// it, a doc comment, or a block of several comments. Nothing less passes: a comment that only
// mentions the real clock, the marker words with no reason or a one-word one, a reason in a
// comment of its own below a blank line, the words in a string, a marker a blank line parts from
// the statement, and a marker over a statement before the wait's.
test("a wait passes under the marker words and a reason after them, and under nothing less", () => {
  const reasoned = (above: string): boolean[] =>
    realWaits("marked.test.ts", `${above}\nsetTimeout(run, 50);`).map((wait) => wait.reasoned);
  const marked = [
    MARKED,
    "// This one stays on the real clock (INFRA-1505)\n// for a long task that no fake clock models.",
    "/** This one stays on the real clock\n *  (INFRA-1505): the margin corrects a card in a frame. */",
    "// The editor is reached on the next frame.\n// This one stays on the real clock (INFRA-983): the row reads\n// what no state marks but the frame.",
  ];
  const unmarked = [
    "// This waits on the real clock for the long task to end.",
    "// Real clock: a real long task that no fake clock can stand in for.",
    "// This one stays on the real clock (INFRA-1505).",
    "// This one stays on the real clock (INFRA-1505): why.",
    "// This one stays on the real clock (INFRA-1505).\n\n// A real long task that no fake clock can stand in for.",
    'const note = "This one stays on the real clock: a real long task no fake clock stands in for.";',
    `${MARKED}\n`,
    `${MARKED}\nrun();`,
  ];
  expect(marked.map(reasoned)).toEqual(marked.map(() => [true]));
  expect(unmarked.map(reasoned)).toEqual(unmarked.map(() => [false]));
});

// The architect ruled that one marker covers one wait, as a jest-dom marker covers one read.
// It covers the statement directly below it, or the helper whose doc comment holds it, and passes
// the one wait that statement holds; a statement that holds two passes neither, and each carries a
// marker of its own, on the statement that holds it. A marker over the statement before the
// wait's, or one a blank line parts from it, covers nothing.
test("one marker covers one wait: its own statement's, or the one wait of the helper it documents", () => {
  const reasoned = (text: string): boolean[] =>
    realWaits("one.test.ts", text).map((wait) => wait.reasoned);
  const doc =
    "/** Lets a frame pass. This one stays on the real clock (INFRA-983): the rows read what the frame did not do. */";
  const frame = "  await new Promise((done) => requestAnimationFrame(done));";
  const turn = "  await new Promise((done) => setImmediate(done));";
  expect(reasoned(`${MARKED}\nsetTimeout(run, 50);`)).toEqual([true]);
  expect(reasoned(`${doc}\nasync function nextFrame() {\n  run();\n${frame}\n}`)).toEqual([true]);
  expect(
    reasoned(`async function settle() {\n  ${MARKED}\n${frame}\n  ${MARKED}\n${turn}\n}`)
  ).toEqual([true, true]);
  expect(reasoned(`${MARKED}\nawait Promise.all([Bun.sleep(50), Bun.sleep(50)]);`)).toEqual([
    false,
    false,
  ]);
  expect(reasoned(`${doc}\nasync function settle() {\n${frame}\n${turn}\n}`)).toEqual([
    false,
    false,
  ]);
  expect(
    reasoned(
      `${MARKED}\nconst { promise, resolve } = Promise.withResolvers<void>();\nsetImmediate(resolve);`
    )
  ).toEqual([false]);
  expect(reasoned(`${MARKED}\n\nsetTimeout(run, 50);`)).toEqual([false]);
});

// A row that stands in for an answer sets its timer with `afterOnFakeClock`, which refuses the real
// clock, so the scan passes the call with no reason. The `setTimeout` inside that helper passes by
// its place alone, `afterOnFakeClock`'s own body in `fake-clock.ts`: the same function in another
// file, another function of that file, and a function nested inside the helper are each found.
test("afterOnFakeClock passes, and so does the timer in its own body in fake-clock.ts, and no other", () => {
  expect(realWaits("answer.test.tsx", "afterOnFakeClock(50, run);")).toEqual([]);
  const helper =
    "export function afterOnFakeClock(ms: number, fn: () => void) {\n  return setTimeout(fn, ms);\n}";
  const other =
    "export function later(ms: number, fn: () => void) {\n  return setTimeout(fn, ms);\n}";
  const nested =
    "export function afterOnFakeClock(ms: number, fn: () => void) {\n  queueMicrotask(() => setTimeout(fn, ms));\n}";
  expect(realWaits("__tests__/fake-clock.ts", helper)).toEqual([]);
  expect(realWaits("__tests__/other-clock.ts", helper)).toHaveLength(1);
  expect(realWaits("__tests__/fake-clock.ts", other)).toHaveLength(1);
  expect(realWaits("__tests__/fake-clock.ts", nested)).toHaveLength(1);
});

// The timer a row sets for an answer it stands in for (`afterOnFakeClock`) is no wait of real
// time: it refuses the real clock, and on the fake one it runs once the clock has moved its delay.
test("afterOnFakeClock refuses the real clock and runs once the fake one has moved its delay", async () => {
  let ran = 0;
  const run = () => {
    ran += 1;
  };
  expect(() => afterOnFakeClock(50, run)).toThrow("runs on the fake clock alone");
  await onFakeClock(async () => {
    afterOnFakeClock(50, run);
    jest.advanceTimersByTime(49);
    expect(ran).toBe(0);
    jest.advanceTimersByTime(1);
    expect(ran).toBe(1);
  });
});

test("no unit test waits on the real clock without saying why", () => {
  expect(waits.filter((wait) => !wait.reasoned).map((wait) => wait.site)).toEqual([]);
});
