import { expect, jest, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import * as ts from "typescript";

import { afterOnFakeClock, onFakeClock } from "./fake-clock";

/**
 * R6 of dispatch://INFRA-983: no unit test synchronises on the real clock. A row that waits for a
 * product timer moves the fake clock (`web/src/__tests__/fake-clock.ts`); a wait that must stay on
 * the real clock says why beside it, in the form the kept ones use ("This one stays on the real
 * clock (INFRA-1505)"), and docs/testing.md, "The clock of a unit test", is the rule.
 *
 * This reads every unit test file and test helper under `web/src` - a `*.test.ts(x)`, a file of a
 * `__tests__` folder, a `*-fixture.ts(x)` or `*-fixtures.ts(x)` helper - for a wait of real time,
 * whatever its delay: a `Bun.sleep`, or a timer that settles a promise. That timer is a
 * `setTimeout`, global or a member (`window.setTimeout`), whose callback is a promise's resolver or
 * a function that calls one, and a resolver is the first parameter of a `new Promise` executor
 * whatever it is called (`r`, `done`), a `resolve` however it is reached (`resolve`,
 * `waited.resolve`), the name a `Promise.withResolvers()` result's `resolve` is taken under, or a
 * name one of those is stored under (`release = r`). The file is read by the TypeScript parser,
 * so a timer is found however its call is spelled or wrapped (inside `act(...)`, across lines),
 * and a comment or a string that spells one is none. A zero-delay timer is a wait of real time
 * too: what has run by its turn - a timer of the product's, a render, a promise of the library's -
 * depends on how loaded the machine is, so a row waits for the condition its step stands for, or
 * moves the fake clock over the product's own zero-delay timer with `tick(0)`. A row that stands
 * in for an answer that takes time to come sets its timer with `afterOnFakeClock`
 * (`fake-clock.ts`), which refuses the real clock itself. The act-rule inputs (`*.fixture.tsx`)
 * are written to break rules and are left out. A file that imports `bun:test` or Testing Library
 * is a test file or a helper whatever its name, and the scan holds itself to reading every such
 * file.
 */

const webSource = fileURLToPath(new URL("..", import.meta.url));

/** A comment within this many lines above a wait may carry its reason: the kept waits carry it in
 *  the doc comment of the helper that holds them, a few lines above the timer. */
const REASON_WINDOW = 12;

const comment = /^\s*(?:\/\/|\/\*|\*|\{\/\*)/;

/** Every `.ts(x)` file under `directory` but the act-rule inputs (`*.fixture.tsx`). */
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
    path.split("/").includes("__tests__")
  );
}

interface RealWait {
  readonly site: string;
  readonly reasoned: boolean;
}

/** The names `source` binds a promise's resolver to: `resolve`, the first parameter of every
 *  `new Promise` executor whatever it is called, every name a `Promise.withResolvers()` result's
 *  `resolve` is destructured to (`{ resolve: land }`), and every name one of those is stored under
 *  (`release = r`), read again until no name is added. */
function resolverNames(source: ts.SourceFile): ReadonlySet<string> {
  const names = new Set(["resolve"]);
  const visit = (node: ts.Node): void => {
    if (
      ts.isNewExpression(node) &&
      ts.isIdentifier(node.expression) &&
      node.expression.text === "Promise"
    ) {
      const executor = node.arguments?.[0];
      const first =
        executor !== undefined &&
        (ts.isArrowFunction(executor) || ts.isFunctionExpression(executor))
          ? executor.parameters[0]?.name
          : undefined;
      if (first !== undefined && ts.isIdentifier(first)) names.add(first.text);
    }
    if (
      ts.isVariableDeclaration(node) &&
      ts.isObjectBindingPattern(node.name) &&
      node.initializer !== undefined &&
      ts.isCallExpression(node.initializer) &&
      ts.isPropertyAccessExpression(node.initializer.expression) &&
      node.initializer.expression.name.text === "withResolvers"
    ) {
      for (const element of node.name.elements) {
        const property = element.propertyName ?? element.name;
        if (
          ts.isIdentifier(property) &&
          property.text === "resolve" &&
          ts.isIdentifier(element.name)
        ) {
          names.add(element.name.text);
        }
      }
    }
    const stored = ts.isVariableDeclaration(node)
      ? { name: node.name, value: node.initializer }
      : ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.EqualsToken
        ? { name: node.left, value: node.right }
        : undefined;
    if (
      stored?.value !== undefined &&
      ts.isIdentifier(stored.name) &&
      isResolver(stored.value, names)
    ) {
      names.add(stored.name.text);
    }
    ts.forEachChild(node, visit);
  };
  let read = 0;
  while (read !== names.size) {
    read = names.size;
    visit(source);
  }
  return names;
}

/** Whether `expression` is a resolver: one of `names`, or a `resolve` member (`waited.resolve`)
 *  other than `Promise.resolve`, which settles no promise of the row's. */
function isResolver(expression: ts.Expression, names: ReadonlySet<string>): boolean {
  if (ts.isIdentifier(expression)) return names.has(expression.text);
  return (
    ts.isPropertyAccessExpression(expression) &&
    expression.name.text === "resolve" &&
    !(ts.isIdentifier(expression.expression) && expression.expression.text === "Promise")
  );
}

/** Whether a timer's `callback` settles a promise: it is a resolver, or a function whose body calls
 *  one (`() => resolve()`, `() => { landed = true; older.resolve(value); }`). */
function settles(callback: ts.Expression | undefined, names: ReadonlySet<string>): boolean {
  if (callback === undefined) return false;
  if (isResolver(callback, names)) return true;
  if (!ts.isArrowFunction(callback) && !ts.isFunctionExpression(callback)) return false;
  const calls = (node: ts.Node): boolean =>
    (ts.isCallExpression(node) && isResolver(node.expression, names)) ||
    (ts.forEachChild(node, calls) ?? false);
  return calls(callback.body);
}

/** The waits of real time in `text`, each with whether a comment above it names the real clock. */
export function realWaits(file: string, text: string): RealWait[] {
  const source = ts.createSourceFile(
    file,
    text,
    ts.ScriptTarget.Latest,
    false,
    file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS
  );
  const names = resolverNames(source);
  const lines = text.split("\n");
  const found: RealWait[] = [];
  const visit = (node: ts.Node): void => {
    if (ts.isCallExpression(node)) {
      const callee = node.expression;
      const sleep =
        ts.isPropertyAccessExpression(callee) &&
        callee.name.text === "sleep" &&
        ts.isIdentifier(callee.expression) &&
        callee.expression.text === "Bun";
      // A `setTimeout` is the global or a member of anything (`window.setTimeout`).
      const timerName = ts.isPropertyAccessExpression(callee) ? callee.name : callee;
      const timer = ts.isIdentifier(timerName) && timerName.text === "setTimeout";
      if (sleep || (timer && settles(node.arguments[0], names))) {
        const index = source.getLineAndCharacterOfPosition(node.getStart(source)).line;
        const above = lines.slice(Math.max(0, index - REASON_WINDOW), index);
        const reasoned = above.some((line) => comment.test(line) && /real clock/i.test(line));
        found.push({ reasoned, site: `${file}:${index + 1}: ${(lines[index] ?? "").trim()}` });
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return found;
}

const sources = sourceFiles(webSource);
const files = sources
  .filter(isTestFile)
  .filter((path) => !path.endsWith("real-clock-waits.test.ts"));
const waits = files.flatMap((path) =>
  realWaits(relative(webSource, path), readFileSync(path, "utf8"))
);

test("the scan reads the unit suite and finds the waits it keeps", () => {
  expect(files.length).toBeGreaterThan(100);
  // Margin.landing.test.tsx's `quiet` stays on the real clock with its reason.
  expect(waits.some((wait) => wait.reasoned)).toBe(true);
});

test("the scan reads every file that imports bun:test or Testing Library", () => {
  const unread = sources
    .filter((path) => !isTestFile(path))
    .filter((path) =>
      /from "(?:bun:test|@testing-library\/[\w-]+)"/.test(readFileSync(path, "utf8"))
    )
    .map((path) => relative(webSource, path));
  expect(unread, "a test helper the scan does not read by its name").toEqual([]);
});

test("a wait of real time is found, a zero-delay one too, and its reason is read from the comment above it", () => {
  const bare = ["await act(async () => {", "  setTimeout(waited.resolve, 50);", "});"].join("\n");
  const kept = ["// This one stays on the real clock (INFRA-1505): why.", bare].join("\n");
  const zero = "setTimeout(resolve, 0);";
  const zeroKept = ["// This one stays on the real clock (INFRA-1505): why.", zero].join("\n");
  expect(realWaits("bare.test.ts", bare).map((wait) => wait.reasoned)).toEqual([false]);
  expect(realWaits("kept.test.ts", kept).map((wait) => wait.reasoned)).toEqual([true]);
  expect(realWaits("zero.test.ts", zero).map((wait) => wait.reasoned)).toEqual([false]);
  expect(realWaits("zero-kept.test.ts", zeroKept).map((wait) => wait.reasoned)).toEqual([true]);
  expect(realWaits("sleep.test.ts", "await Bun.sleep(0);").map((wait) => wait.reasoned)).toEqual([
    false,
  ]);
});

// docs/testing.md, "The clock of a unit test", says this scan fails an awaited timer whatever its
// delay, and that is a claim about every spelling of one: the resolver a `new Promise` hands its
// executor is as often `r` or `done` as `resolve`, and a timer may call it from an arrow. Each of
// these waits 50 ms of real time.
test("an awaited timer is found whatever its resolver is called and however the timer calls it", () => {
  const spellings = [
    "await new Promise((r) => setTimeout(r, 50));",
    "await new Promise<void>((done) => {\n  setTimeout(done, 50);\n});",
    "await new Promise((resolve) => setTimeout(() => resolve(), 50));",
  ];
  expect(spellings.map((text) => realWaits("spelling.test.ts", text).length)).toEqual([1, 1, 1]);
});

// The other spellings of a timer that settles a promise, each found once - with no delay, a
// resolver taken from `Promise.withResolvers` whole or under a name of its own, a member
// `setTimeout`, inside `act(...)`, a resolver stored under another name, and a function that calls
// one - and what settles no promise, found not at all: a product timer a row stands in for, a timer
// whose callback calls `Promise.resolve`, and a timer spelled in a comment or in a string.
test("a timer that settles a promise is found however it is reached, and one that settles none is not", () => {
  const found = [
    "await new Promise((resolve) => setTimeout(resolve));",
    "const w = Promise.withResolvers<void>();\nsetTimeout(w.resolve, 50);\nawait w.promise;",
    "const { promise, resolve: land } = Promise.withResolvers<void>();\nsetTimeout(land, 50);\nawait promise;",
    "await new Promise((r) => window.setTimeout(r, 50));",
    "await act(() => new Promise((r) => setTimeout(r, 50)));",
    "let release = () => {};\nconst held = new Promise<void>((r) => {\n  release = r;\n});\nsetTimeout(release, 50);\nawait held;",
    "setTimeout(function () {\n  landed = true;\n  older.resolve(value);\n}, 50);",
  ];
  expect(found.map((text) => realWaits("found.test.tsx", text).length)).toEqual(found.map(() => 1));
  const none = [
    "const timer = setTimeout(() => setShown(true), 50);",
    "setTimeout(() => void Promise.resolve(), 0);",
    "// await new Promise((r) => setTimeout(r, 50));",
    'const source = "await new Promise((r) => setTimeout(r, 50));";',
  ];
  expect(none.map((text) => realWaits("none.test.tsx", text).length)).toEqual(none.map(() => 0));
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
