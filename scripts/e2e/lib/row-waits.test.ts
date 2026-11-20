/** The rows of the test files below start processes and wait for them, and each states its limit
 *  as the sum of what it waits for (`rowMs` of `packages/test-fixtures/src/row-waits.ts`), and
 *  counts those waits as it makes them (`rowOf`). A row that states no limit runs under Bun's own
 *  5 s, which load runs past: the `--cli` row of `dispatch-keyed-comment.test.ts` did, in 4 of 20
 *  runs on a shared Linux machine on 2026-11-19. So this file reads their source, and refuses:
 *
 *  - a row or a hook that starts a process and states no limit of its own. A process is started
 *    by `Bun.spawn`, `Bun.spawnSync` or `Bun.$`, or by an export of `node:child_process` under the
 *    name the file imports it by, in the body itself or in a function of the file's own that the
 *    body calls, at any depth. A row's limit is its third argument, or its second where the third
 *    is its body, as Bun reads both; a hook's is its second;
 *  - a row that counts `rowOf(<waits>)` and states anything but `rowMs(<waits>)`, and a row that
 *    states `rowMs(<waits>)` and counts no `rowOf(<waits>)`, compared as the file writes them.
 *    The rows and this rule are read by `packages/test-fixtures/src/row-sums.ts`, the one reading
 *    that the source rows of `packages/dispatch/e2e/__tests__` hold their files to as well.
 *
 *  What it does not read: a process started by a function another module exports (a fixture's
 *  helper), and a file limit (`setDefaultTimeout`), which no file below writes. The files are those
 *  of this repository whose rows started processes with no limit of their own when the rule came
 *  in; `packages/dispatch/e2e/row-limits.ts` holds the rows of `packages/dispatch/e2e/__tests__`
 *  that wait for a bounded child to its own rule. */

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { functionsOf, sumRefusal, testCalls } from "@legion/test-fixtures/row-sums";
import * as ts from "typescript";

const root = join(import.meta.dir, "..", "..", "..");

/** The test files this file reads, from the root of the repository: the files whose rows started
 *  processes with no limit of their own, and now state one. `scripts/e2e/lib/leftovers.test.ts` is
 *  not here: its rows, main's reads of run-marked processes, state no limit of their own; the rows
 *  of refuse_leftovers's reading of containers, and of the removal, are in
 *  `leftovers-containers.test.ts`, which is. */
const files = [
  "scripts/e2e/lib/dispatch-keyed-comment.test.ts",
  "scripts/e2e/lib/dispatch-live-events.test.ts",
  "scripts/e2e/lib/leftovers-containers.test.ts",
  "scripts/e2e/lib/nats-image.test.ts",
  "scripts/e2e/lib/released-port-probe.test.ts",
  "scripts/e2e/lib/smoke-cleanup.test.ts",
  "scripts/e2e/lib/sweep-order.test.ts",
  "packages/pi-envoy/extensions/envoy.test.ts",
  "packages/dispatch/e2e/__tests__/harness-postgres-removal.test.ts",
  "packages/dispatch/e2e/__tests__/harness-postgres-elsewhere.test.ts",
  "packages/dispatch/e2e/__tests__/harness-postgres-container-removal.test.ts",
  "packages/dispatch/e2e/__tests__/harness-postgres-waits.test.ts",
];

/** Bun's own calls that start a process, as a file writes them. */
const bunStarts = new Set(["Bun.spawn", "Bun.spawnSync", "Bun.$"]);

/** The exports of `node:child_process` that start a process. */
const childProcessStarts = new Set([
  "exec",
  "execFile",
  "execFileSync",
  "execSync",
  "fork",
  "spawn",
  "spawnSync",
]);

/** What `limitsOfRows` read of one file: how many rows and hooks, how many of those start a
 *  process, and each that the rule refuses, one line each with its line in the file. */
interface Read {
  readonly rows: number;
  readonly starting: number;
  readonly refused: string[];
}

/** The rows and hooks of `source`, a test file named `file`, as the rule above reads them: each
 *  row and hook as `testCalls` reads it, refused where it starts a process and states no limit,
 *  and otherwise where its limit is not the sum it counts (`sumRefusal`). */
function limitsOfRows(file: string, source: string): Read {
  const parsed = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true);
  // The names the file reaches `node:child_process`'s starts by: each import of one, and each
  // namespace or default import of the module.
  const starters = new Set<string>();
  const modules = new Set<string>();
  for (const statement of parsed.statements) {
    if (
      !ts.isImportDeclaration(statement) ||
      !ts.isStringLiteral(statement.moduleSpecifier) ||
      !["node:child_process", "child_process"].includes(statement.moduleSpecifier.text)
    ) {
      continue;
    }
    const clause = statement.importClause;
    if (clause?.name !== undefined) modules.add(clause.name.text);
    const bindings = clause?.namedBindings;
    if (bindings !== undefined && ts.isNamespaceImport(bindings)) modules.add(bindings.name.text);
    if (bindings !== undefined && ts.isNamedImports(bindings)) {
      for (const element of bindings.elements) {
        if (childProcessStarts.has((element.propertyName ?? element.name).text)) {
          starters.add(element.name.text);
        }
      }
    }
  }
  const functions = functionsOf(parsed);
  // The start `node` makes, as the file writes its callee, where it makes one.
  const startOf = (node: ts.Node): string | undefined => {
    const callee = ts.isCallExpression(node)
      ? node.expression
      : ts.isTaggedTemplateExpression(node)
        ? node.tag
        : undefined;
    if (callee === undefined) return undefined;
    const text = callee.getText(parsed);
    if (bunStarts.has(text)) return text;
    if (ts.isIdentifier(callee) && starters.has(callee.text)) return text;
    if (
      ts.isPropertyAccessExpression(callee) &&
      ts.isIdentifier(callee.expression) &&
      modules.has(callee.expression.text) &&
      childProcessStarts.has(callee.name.text)
    ) {
      return text;
    }
    return undefined;
  };
  // The first start under `body`, itself or through a function of the file's own it calls, each
  // function read once.
  const startUnder = (body: ts.Node, seen: Set<string>): string | undefined => {
    let found: string | undefined;
    const visit = (node: ts.Node): void => {
      if (found !== undefined) return;
      found = startOf(node);
      if (found !== undefined) return;
      if (ts.isCallExpression(node) && ts.isIdentifier(node.expression)) {
        const name = node.expression.text;
        const called = functions.get(name);
        if (called !== undefined && !seen.has(name)) {
          seen.add(name);
          const inner = startUnder(called, seen);
          if (inner !== undefined) {
            found = `${inner} through ${name}`;
            return;
          }
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(body);
    return found;
  };
  const calls = testCalls(parsed, functions);
  let starting = 0;
  const refused: string[] = [];
  for (const call of calls) {
    const start = call.body === undefined ? undefined : startUnder(call.body, new Set());
    if (start !== undefined) starting += 1;
    const sum = sumRefusal(file, parsed, call);
    if (start !== undefined && call.limit === undefined) {
      refused.push(
        `${file}:${call.line}: ${call.hook ?? "a row"} starts ${start} and states no limit`
      );
    } else if (sum !== undefined) {
      refused.push(sum);
    }
  }
  return { rows: calls.length, starting, refused };
}

test("each row of these files that starts a process states its limit as the sum of what it waits for, and counts it", () => {
  let starting = 0;
  for (const file of files) {
    const read = limitsOfRows(file, readFileSync(join(root, file), "utf8"));
    // A file read as holding no row is a file the scan could not read, never one that passes.
    expect({ file, rows: read.rows > 0, refused: read.refused }).toEqual({
      file,
      rows: true,
      refused: [],
    });
    starting += read.starting;
  }
  // The rows of these files that start a process when the rule came in, which a scan that no longer
  // sees a start would not find: a floor under the scan.
  expect(starting).toBeGreaterThanOrEqual(36);
});

// The controls of the check above: a copy of a file above whose row states no limit, and planted
// rows and hooks of each shape the rule reads, which it must name, beside the shapes it must pass.
test("a row or a hook that starts a process and states no limit is named, and so is a limit that is not the sum its row counts", () => {
  const keyed = "scripts/e2e/lib/dispatch-keyed-comment.test.ts";
  const text = readFileSync(join(root, keyed), "utf8");
  const unlimited = text.replace("  },\n  rowMs(cliCheck)\n);", "  }\n);");
  expect(unlimited).not.toBe(text);
  expect(limitsOfRows(keyed, unlimited).refused).toEqual([
    expect.stringMatching(
      /^scripts\/e2e\/lib\/dispatch-keyed-comment\.test\.ts:\d+: a row starts Bun\.spawn through run and states no limit$/
    ),
  ]);
  const uncounted = text.replace("rowOf(cliCheck)", "rowOf(oneCheck)");
  expect(limitsOfRows(keyed, uncounted).refused).toEqual([
    expect.stringMatching(/:\d+: counts rowOf\(oneCheck\), states rowMs\(cliCheck\)$/),
  ]);

  const planted = [
    'import { execFileSync as git } from "node:child_process";',
    'import * as child from "node:child_process";',
    "function helper() {\n  return Bun.spawnSync(['true']);\n}",
    'test("a spawn", () => {\n  Bun.spawn(["true"]);\n});',
    'test("a helper", () => {\n  helper();\n});',
    'test.skipIf(false)("a renamed import", () => {\n  git("git", ["status"]);\n});',
    'test("a namespace", async () => {\n  child.spawn("true");\n});',
    'test("the shell", async () => {\n  await Bun.$`true`;\n});',
    "afterAll(() => {\n  helper();\n});",
    'test("a regular expression", () => {\n  /x/.exec("x");\n});',
    'test("a stated limit", () => {\n  helper();\n}, 5_000);',
    'test("a limit first", { timeout: 5_000 }, () => {\n  helper();\n});',
    "afterEach(() => helper(), 5_000);",
    'test("a number", () => {\n  run(rowOf(waits));\n}, 600_000);',
    'test("another sum", () => {\n  run(rowOf(waits));\n}, rowMs(other));',
    'test("a sum it does not count", () => {}, rowMs(waits));',
    'test("the sum", () => {\n  run(rowOf(waits));\n}, rowMs(waits));',
    'it("an it row", () => {\n  run(rowOf(waits));\n}, 600_000);',
    'test("a limit before its body", rowMs(other), () => {\n  run(rowOf(waits));\n});',
  ].join("\n");
  const read = limitsOfRows("planted.test.ts", planted);
  expect(read.refused).toEqual([
    "planted.test.ts:6: a row starts Bun.spawn and states no limit",
    "planted.test.ts:9: a row starts Bun.spawnSync through helper and states no limit",
    "planted.test.ts:12: a row starts git and states no limit",
    "planted.test.ts:15: a row starts child.spawn and states no limit",
    "planted.test.ts:18: a row starts Bun.$ and states no limit",
    "planted.test.ts:21: afterAll starts Bun.spawnSync through helper and states no limit",
    "planted.test.ts:34: counts rowOf(waits), states 600_000",
    "planted.test.ts:37: counts rowOf(waits), states rowMs(other)",
    "planted.test.ts:40: states rowMs(waits), counts no rowOf(waits)",
    "planted.test.ts:44: counts rowOf(waits), states 600_000",
    "planted.test.ts:47: counts rowOf(waits), states rowMs(other)",
  ]);
  expect(read.starting).toBe(9);
});
