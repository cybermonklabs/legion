/**
 * The one reading of a test file's rows that the audits of row limits share, and the rule they
 * hold each row to: a row that counts its waits (`rowOf(<waits>)` of `row-waits.ts`, or of the
 * fixtures of `packages/dispatch/e2e/__tests__`) states its limit as `rowMs` of those same waits,
 * compared as the file writes them, and a row that states `rowMs(<waits>)` counts them.
 *
 * A row is a call of `test` or `it`, or of what either reaches through dotted members and calls
 * (`test.skipIf(<x>)`); its limit is its third argument, or its second where the third is its body,
 * as Bun reads both. A hook of `bun:test` (`hooks`) is read the same way, its limit its second
 * argument. Each audit reads its own files with it: `scripts/e2e/lib/row-waits.test.ts`, which also
 * refuses a row that starts a process and states no limit, and the source rows of
 * `packages/dispatch/e2e/__tests__`, over the files whose rows count their waits with that
 * package's fixtures.
 */

import * as ts from "typescript";

/** The hooks of `bun:test` that take a limit as their second argument. */
export const hooks = new Set(["beforeAll", "afterAll", "beforeEach", "afterEach"]);

/** One row or hook of a file: its line, the hook's name (none for a row), and its body and its
 *  limit as Bun reads them, the body read through the name of a function of the file's own. */
export interface TestCall {
  readonly line: number;
  readonly hook: string | undefined;
  readonly body: ts.Node | undefined;
  readonly limit: ts.Expression | undefined;
}

/** The functions of the file's own, at any scope, by name: each declared one, and each arrow or
 *  function expression a variable holds. */
export function functionsOf(parsed: ts.SourceFile): Map<string, ts.Node> {
  const functions = new Map<string, ts.Node>();
  const collect = (node: ts.Node): void => {
    if (ts.isFunctionDeclaration(node) && node.name !== undefined) {
      functions.set(node.name.text, node);
    } else if (
      ts.isVariableDeclaration(node) &&
      ts.isIdentifier(node.name) &&
      node.initializer !== undefined &&
      (ts.isArrowFunction(node.initializer) || ts.isFunctionExpression(node.initializer))
    ) {
      functions.set(node.name.text, node.initializer);
    }
    ts.forEachChild(node, collect);
  };
  collect(parsed);
  return functions;
}

/** Each row and hook of `parsed`, in the order the file writes them. */
export function testCalls(
  parsed: ts.SourceFile,
  functions: ReadonlyMap<string, ts.Node> = functionsOf(parsed)
): TestCall[] {
  const isRow = (callee: ts.Expression): boolean =>
    ts.isIdentifier(callee)
      ? callee.text === "test" || callee.text === "it"
      : (ts.isPropertyAccessExpression(callee) || ts.isCallExpression(callee)) &&
        isRow(callee.expression);
  // Whether `node` is a body rather than a limit: a function written there, or the name of one of
  // the file's own.
  const isBody = (node: ts.Expression | undefined): boolean =>
    node !== undefined &&
    (ts.isArrowFunction(node) ||
      ts.isFunctionExpression(node) ||
      (ts.isIdentifier(node) && functions.has(node.text)));
  const calls: TestCall[] = [];
  const visit = (node: ts.Node): void => {
    if (ts.isCallExpression(node)) {
      const hook =
        ts.isIdentifier(node.expression) && hooks.has(node.expression.text)
          ? node.expression.text
          : undefined;
      if (hook !== undefined || (isRow(node.expression) && node.arguments.length >= 2)) {
        const [first, second, third] = node.arguments;
        const [body, limit] =
          hook !== undefined ? [first, second] : isBody(second) ? [second, third] : [third, second];
        calls.push({
          line: parsed.getLineAndCharacterOfPosition(node.getStart(parsed)).line + 1,
          hook,
          body: body !== undefined && ts.isIdentifier(body) ? functions.get(body.text) : body,
          limit,
        });
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(parsed);
  return calls;
}

/** The argument of a call of `name`, as the file writes it, where `node` is one. */
function argumentOf(
  parsed: ts.SourceFile,
  node: ts.Node | undefined,
  name: string
): string | undefined {
  return node !== undefined &&
    ts.isCallExpression(node) &&
    ts.isIdentifier(node.expression) &&
    node.expression.text === name
    ? (node.arguments[0]?.getText(parsed) ?? "")
    : undefined;
}

/** What the rule refuses of `call`, a row or a hook of `parsed` (the file `file`), one line with
 *  its line in the file: a row that counts `rowOf(<waits>)` and states anything but `rowMs` of
 *  those same waits, a number above all, and a row that states `rowMs(<waits>)` and counts no
 *  `rowOf` of them. Undefined where it holds. */
export function sumRefusal(
  file: string,
  parsed: ts.SourceFile,
  call: TestCall
): string | undefined {
  const counted: string[] = [];
  const count = (inner: ts.Node): void => {
    const waits = argumentOf(parsed, inner, "rowOf");
    if (waits !== undefined) counted.push(waits);
    ts.forEachChild(inner, count);
  };
  if (call.body !== undefined) count(call.body);
  const summed = argumentOf(parsed, call.limit, "rowMs");
  const stated = call.limit?.getText(parsed) ?? "no limit";
  if (counted.length > 0 && counted.some((waits) => waits !== summed)) {
    return `${file}:${call.line}: counts rowOf(${counted.join("), rowOf(")}), states ${stated}`;
  }
  if (counted.length === 0 && summed !== undefined) {
    return `${file}:${call.line}: states ${stated}, counts no rowOf(${summed})`;
  }
  return undefined;
}

/** The rule's refusals of every row and hook of `source`, a test file named `file`, and how many
 *  rows and hooks it read: a file read as holding none is one the read could not see into. */
export function limitsNotSummed(
  file: string,
  source: string
): { readonly calls: number; readonly refused: string[] } {
  const parsed = ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true);
  const calls = testCalls(parsed);
  return {
    calls: calls.length,
    refused: calls.flatMap((call) => sumRefusal(file, parsed, call) ?? []),
  };
}
