import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import * as ts from "typescript";

/**
 * The census behind `heldPress` (`held-press.ts`): no control of this app is `disabled` while a
 * write of its own is out. The browser takes the focus off a control that disables itself under
 * the reader's press and drops it on the page, so the next key does nothing; a control held with
 * `aria-disabled` keeps the focus and its Tab stop. A control disabled for another reason - a
 * closed issue, an empty field, nothing to submit - stays `disabled`, and an expression that mixes
 * both keeps `disabled` for the other reason and holds the control for the write.
 *
 * Reads every non-test `.tsx` under `web/src`, takes each JSX `disabled={…}` attribute whole (the
 * parser balances its braces, over as many lines as it spans), and names an offender when its
 * expression names a pending state: an identifier with one of `PENDING_WORDS` among its words
 * (`isPending`, `save.isPending`, `replySending`, `isFetchingNextPage`), or a comparison with the
 * string `"pending"` (`mutation.status === "pending"`). A comment inside the expression names
 * nothing. Each offender is listed as `<path>:<line>: <expression>`.
 */
const PENDING_WORDS = [
  "busy",
  "pending",
  "sending",
  "saving",
  "retrying",
  "stopping",
  "submitting",
  "working",
  "writing",
  "in_flight",
  "fetching",
] as const;

const PENDING_WORD = new RegExp(`(?:^|_)(?:${PENDING_WORDS.join("|")})(?:_|$)`);

/** `isPending` → `is_pending`, `SEND_PENDING` → `send_pending`: an identifier as its words. */
function words(name: string): string {
  return name
    .replace(/([a-z0-9])([A-Z])/g, "$1_$2")
    .replace(/([A-Z]+)([A-Z][a-z])/g, "$1_$2")
    .toLowerCase();
}

const EQUALITY = new Set([
  ts.SyntaxKind.EqualsEqualsEqualsToken,
  ts.SyntaxKind.EqualsEqualsToken,
  ts.SyntaxKind.ExclamationEqualsEqualsToken,
  ts.SyntaxKind.ExclamationEqualsToken,
]);

/** Whether `expression` names a pending state, read from its tokens rather than its text. */
function namesPendingState(expression: ts.Node): boolean {
  let found = false;
  const visit = (node: ts.Node): void => {
    if (found) return;
    if (ts.isIdentifier(node) && PENDING_WORD.test(words(node.text))) {
      found = true;
      return;
    }
    if (
      ts.isBinaryExpression(node) &&
      EQUALITY.has(node.operatorToken.kind) &&
      [node.left, node.right].some(
        (side) =>
          (ts.isStringLiteral(side) || ts.isNoSubstitutionTemplateLiteral(side)) &&
          side.text === "pending"
      )
    ) {
      found = true;
      return;
    }
    ts.forEachChild(node, visit);
  };
  visit(expression);
  return found;
}

interface Offender {
  readonly expression: string;
  readonly line: number;
}

/** Every `disabled={…}` of `source` whose expression names a pending state. */
function pendingDisabled(source: string, fileName = "census.tsx"): Offender[] {
  const file = ts.createSourceFile(
    fileName,
    source,
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TSX
  );
  const offenders: Offender[] = [];
  const visit = (node: ts.Node): void => {
    if (
      ts.isJsxAttribute(node) &&
      node.name.getText(file) === "disabled" &&
      node.initializer !== undefined &&
      ts.isJsxExpression(node.initializer) &&
      node.initializer.expression !== undefined &&
      namesPendingState(node.initializer.expression)
    ) {
      offenders.push({
        expression: node.initializer.expression.getText(file).replace(/\s+/g, " "),
        line: file.getLineAndCharacterOfPosition(node.getStart(file)).line + 1,
      });
    }
    ts.forEachChild(node, visit);
  };
  visit(file);
  return offenders;
}

const WEB_SRC_ROOT = join(import.meta.dir, "..");

function collectComponentFiles(dir: string): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const entryPath = join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...collectComponentFiles(entryPath));
    } else if (entry.name.endsWith(".tsx") && !entry.name.endsWith(".test.tsx")) {
      files.push(entryPath);
    }
  }
  return files;
}

const SCANNED = collectComponentFiles(WEB_SRC_ROOT).map((file) => ({
  file,
  relative: join("web/src", file.slice(WEB_SRC_ROOT.length + 1)),
}));

const FOUND = SCANNED.flatMap(({ file, relative }) =>
  pendingDisabled(readFileSync(file, "utf8"), file).map((offender) => ({
    ...offender,
    file: relative,
  }))
);

describe("pendingDisabled", () => {
  test("names a pending state over several lines, through a property and in a status", () => {
    const source = [
      "<>",
      "  <button",
      "    disabled={",
      "      refresh.isPending ||",
      "      remove.isPending",
      "    }",
      "  />",
      '  <button disabled={mutation.status === "pending"} />',
      "  <input disabled={replySending} />",
      "  <button disabled={!text || inFlight} />",
      "  <button disabled={log.isFetchingNextPage} />",
      "</>",
    ].join("\n");
    expect(pendingDisabled(source)).toEqual([
      { expression: "refresh.isPending || remove.isPending", line: 3 },
      { expression: 'mutation.status === "pending"', line: 8 },
      { expression: "replySending", line: 9 },
      { expression: "!text || inFlight", line: 10 },
      { expression: "log.isFetchingNextPage", line: 11 },
    ]);
  });

  test("leaves a control disabled for another reason, and the words in a comment or a string", () => {
    const source = [
      "<>",
      "  <button disabled={isClosed || title.trim() === ''} />",
      "  <button disabled={/* not while pending */ isClosed} />",
      '  <button disabled={label === "saving"} />',
      "  <button aria-disabled={busy || undefined} />",
      "  <button disabled={appending} />",
      "</>",
    ].join("\n");
    expect(pendingDisabled(source)).toEqual([]);
  });
});

describe("no control is disabled while its write is out", () => {
  test("scans a non-trivial number of component files", () => {
    // If the scan collapsed to nothing, every check below would pass covering nothing.
    expect(SCANNED.length).toBeGreaterThan(100);
  });

  test("no disabled={…} names a pending state: hold the control (heldPress)", () => {
    expect(FOUND.map(({ expression, file, line }) => `${file}:${line}: ${expression}`)).toEqual([]);
  });
});
