/** Every zero-wait probe of the browser suite whose answer decides what a row or a helper does
 *  next - an `isVisible()`, a `count()` or a `getAttribute()` read as the condition of a branch -
 *  read from the source of this suite's own files, so one that says nothing about why one
 *  instant's answer is safe can be named.
 *
 * A probe is two different things wearing one face. It is the measurement when the row's claim
 * IS what one instant shows: a sampler that logs whether a span is in the page at each tick, where
 * a read that waited would move the sample. It is the flake when it stands in for waiting: the row
 * asks a page that has not drawn its layout yet, which answers no to every question, so the
 * helper takes the wrong path and the row fails later, at a line that has nothing to do with the
 * branch. That is how `openRail` of `e2e/rail.ts` failed: it read `isVisible()` on the Menu
 * control right after `page.goto`, got `false` while the shell still drew its loading skeleton,
 * and skipped opening the rail. The two are told apart by one thing only - whether the writer
 * said which it is - and this module is that rule's machine check.
 * `e2e/__tests__/zero-wait-probes.test.ts` is what runs it over the suite.
 *
 * THE RULE. A probe whose answer decides a path reads a state the page has settled on, or carries
 * a reason. The settled read is `settledState` of `e2e/settled.ts`: it reads its probes inside one
 * `expect.poll` until one of the states the branch chooses between shows, so it is no probe of
 * this rule's. The reason is a comment that ends on the line directly above the probe's line and
 * runs to at least `minimumReasonWords` words, saying why one instant's answer is safe there.
 *
 * - The measure of a reason is `e2e/fixed-waits.ts`'s: `statementOf`, `reasonEndingAbove` and
 *   `shortReason` are imported from there, so the suite holds one measure of a reason and not two.
 *   What that measure is for, and what no scan can judge about a sentence, is THE RULE of that
 *   module.
 * - The line is the probe CALL's, the line its first token stands on, and the comments read are
 *   the ones above the call's statement. So a probe on a later line of its statement stands under
 *   no reason, and its refusal tells the writer to give it a line of its own.
 *
 * WHAT A PROBE IS. A call `x.<name>(...)` whose member name is one of `probeMethods`: the reads
 * Playwright answers for the page as it stands when they are read. `isVisible`, `isHidden`,
 * `count`, `allTextContents` and `allInnerTexts` wait for nothing, and the rest wait at most for
 * an element to match and then answer for that instant, so none of them waits for the state a
 * branch chooses between. A call of a wrapper is a probe as well. A wrapper is a function
 * declaration, or a `const name =` arrow or function expression, one of whose own `return`
 * statements - a nested function's do not count - returns a probe or `await` of one, or an arrow
 * whose expression body is one, parentheses read through. The wrappers are collected by name in
 * one pass over every file read, before any probe is judged, because a helper declares one and a
 * spec calls it: a spec's `if (await menuShows(page))` branches on the `isVisible` that a
 * helper's `menuShows` returns.
 *
 * WHAT DECIDES A PATH. From the probe the scan climbs through `await`, parentheses, `!`, the
 * comparisons `===`, `!==`, `==`, `!=`, `<`, `>`, `<=` and `>=`, and the logical `&&`, `||` and
 * `??`. The probe decides a path when what the climb reaches is:
 *
 * - the condition of an `if`, a `?:`, a `while`, a `do...while` or a `for`, so a loop bound such
 *   as `index < (await rows.count())` is reached by the same climb. An `if` whose only branch is a
 *   `throw`, with no `else`, is no decision: it picks no path but fails the row, as an assertion
 *   does;
 * - the first argument of `test.skip`, `test.fixme`, `test.slow` or `test.fail`, which decides
 *   how the row runs, where that argument is no string, template or function;
 * - the initializer of a `const` or a `let` with a plain name that a later condition of either
 *   kind above refers to, in the same function as the declaration: `const shown = await
 *   menu.textContent()` read by `shown === "Menu" ? ... : ...` below it.
 *
 * Anything else decides no path of this rule's: a probe returned, stored as data, or handed to
 * `expect(...)`, which asserts what the row reads rather than branching on it.
 *
 * WHAT IS SKIPPED. A probe inside a function literal that something else calls at a moment of its
 * own, because its answer is not taken at an instant the row picked:
 *
 * - a function literal passed to `expect.poll`, and the subject of `expect(fn).toPass`, which
 *   Playwright calls again until it holds. That is the waiting shape itself, `settledState`'s
 *   among them;
 * - a function literal passed to a call of a member named in `pageSideCalls`: `evaluate`,
 *   `evaluateAll`, `evaluateHandle`, `waitForFunction`, `addInitScript`, `$eval`, `$$eval`,
 *   `exposeFunction`, `exposeBinding`, `route` and `routeWebSocket`. The first seven run it in
 *   the page, where a `getAttribute` is the DOM's own read and no Playwright probe; the last four
 *   run it when the page calls it or sends a request, so it reads at that event rather than at a
 *   moment the row chose.
 *
 * WHAT IS READ. Every file `suiteFiles` of `e2e/fixed-waits.ts` reads, for the reasons that
 * module's WHAT IS READ gives: every `.ts` under the `e2e` directory but `__tests__`, whose
 * controls are written to be refused, and `harness-*.ts`. Every one is parsed, with no filter of
 * its text first, because a wrapper written in one file is called in another and any file may
 * hold either. A probe is read by the NAME it is called under, as a wait is in
 * `e2e/fixed-waits.ts`, and that fails the same safe way: anything spelled `x.count(...)` or
 * `x.isVisible(...)`, a locator or another object, is asked for its reason rather than skipped. No
 * type checker is needed for that, so the scan parses files and builds no program.
 *
 * What it does NOT see. Each of these but the last is a probe that decides a path and passes
 * unread:
 *
 * - a probe reached through a name it does not know: a method destructured off a locator,
 *   `const { isVisible } = menu`, or a member written in brackets, `menu["isVisible"]()`, or any
 *   other computed property;
 * - an alias: a name bound to a probe or a wrapper rather than to its answer, `const shows =
 *   menuShows` or `const visible = menu.isVisible.bind(menu)`;
 * - a wrapper that returns a probe through a variable - `const shown = await menu.isVisible();
 *   return shown;` - and a wrapper that is a method, a `let` or a `var`, or that returns a call
 *   of another wrapper, since a call of a wrapper is a probe for judging and not for collecting;
 * - an answer that leaves the climb: `Boolean(await menu.isVisible())`, `(await rows.count()) - 1`,
 *   an answer passed to a function that branches on it, a name copied into another name, or a
 *   name tested in a condition of a function nested inside the declaration's own;
 * - shadowing of a name, and any other name read by its spelling rather than by its binding,
 *   since the scan models no scope: a function of one file named like a wrapper of another is
 *   read as that wrapper, a condition that refers to an inner binding named like a probe's
 *   `const` is read as referring to the `const`, and a function passed to `expect.poll` by its
 *   name rather than written there is read as any function is. These read too much rather than
 *   too little, and a reason or a rename answers each.
 *
 * A pass here is a claim about the shapes above and about nothing else.
 *
 * `probesOfFiles` reads the file system and everything else here is pure over its arguments: no
 * environment, no network, no database and no server, so any process may call it.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import * as ts from "typescript";

import { minimumReasonWords, reasonEndingAbove, shortReason, statementOf } from "./fixed-waits";

/** One probe of one file of the suite whose answer decides a path. */
export type DecidingProbe = {
  /** The file, relative to the `e2e` directory: `rail.ts`, and `inbox.e2e.ts` for a spec. */
  file: string;
  /** The 1-based line the probe call begins on: where a reader of a refusal looks, and the line a
   *  reason ends directly above. */
  line: number;
  /** The 0-based column the call begins at, which orders two probes of one line. */
  column: number;
  /** What is called: the method, `isVisible`, or the name of a wrapper, `menuShows`. */
  name: string;
  /** What the answer decides, as WHAT DECIDES A PATH reads it: `if`, `ternary`, `while`,
   *  `do-while`, `for`, `test.skip` and its kin, or `const shown -> ternary@42` for a name bound to
   *  the answer, with the kind and the line of each condition that refers to it. */
  position: string;
  /** The reason comment that ends on the line directly above the call, its markers stripped and
   *  its lines joined by a space; the empty string when no comment ends there. */
  reason: string;
};

/** A probe the rule refuses, with the sentence that names it and says what to do about it. */
export type RefusedProbe = {
  probe: DecidingProbe;
  why: string;
};

/** The reads WHAT A PROBE IS names, by the member name they are called under. */
const probeMethods: ReadonlySet<string> = new Set([
  "isVisible",
  "isHidden",
  "isEnabled",
  "isDisabled",
  "isChecked",
  "isEditable",
  "count",
  "allTextContents",
  "allInnerTexts",
  "textContent",
  "innerText",
  "innerHTML",
  "getAttribute",
  "inputValue",
]);

/** The calls WHAT IS SKIPPED names, by the member name of the callee: each runs a function literal
 *  passed to it in the page, or when the page calls it or sends a request. */
const pageSideCalls: ReadonlySet<string> = new Set([
  "evaluate",
  "evaluateAll",
  "evaluateHandle",
  "waitForFunction",
  "addInitScript",
  "$eval",
  "$$eval",
  "exposeFunction",
  "exposeBinding",
  "route",
  "routeWebSocket",
]);

/** The members of `test` whose first argument decides how the row runs. */
const testModifiers: ReadonlySet<string> = new Set(["skip", "fixme", "slow", "fail"]);

/** The operators the climb goes through: the comparisons and the logical operators, each of which
 *  hands the answer on to whatever decides above it. */
const climbedOperators: ReadonlySet<ts.SyntaxKind> = new Set([
  ts.SyntaxKind.EqualsEqualsEqualsToken,
  ts.SyntaxKind.ExclamationEqualsEqualsToken,
  ts.SyntaxKind.EqualsEqualsToken,
  ts.SyntaxKind.ExclamationEqualsToken,
  ts.SyntaxKind.LessThanToken,
  ts.SyntaxKind.GreaterThanToken,
  ts.SyntaxKind.LessThanEqualsToken,
  ts.SyntaxKind.GreaterThanEqualsToken,
  ts.SyntaxKind.AmpersandAmpersandToken,
  ts.SyntaxKind.BarBarToken,
  ts.SyntaxKind.QuestionQuestionToken,
]);

/** One file of the scan, parsed. */
type ParsedFile = { readonly file: string; readonly sourceFile: ts.SourceFile };

/** Whether `node` is a call `x.<name>(...)` of a probe, read by the member name. */
function isProbeCall(node: ts.Node): node is ts.CallExpression {
  return (
    ts.isCallExpression(node) &&
    ts.isPropertyAccessExpression(node.expression) &&
    probeMethods.has(node.expression.name.text)
  );
}

/** Whether `node` is a function written in place: an arrow or a function expression. */
function isFunctionLiteral(node: ts.Node): node is ts.ArrowFunction | ts.FunctionExpression {
  return ts.isArrowFunction(node) || ts.isFunctionExpression(node);
}

/** Whether the function literals among `call`'s arguments are skipped: a call of a member named in
 *  `pageSideCalls`, or `expect.poll`. */
function skipsArguments(call: ts.CallExpression): boolean {
  const callee = call.expression;
  if (!ts.isPropertyAccessExpression(callee)) return false;
  const name = callee.name.text;
  return (
    pageSideCalls.has(name) ||
    (name === "poll" && ts.isIdentifier(callee.expression) && callee.expression.text === "expect")
  );
}

/** The subject of `expect(fn).toPass(...)`: `fn`, the argument of the inner `expect`. */
function toPassSubject(call: ts.CallExpression): ts.Expression | undefined {
  const callee = call.expression;
  if (!ts.isPropertyAccessExpression(callee) || callee.name.text !== "toPass") return undefined;
  const inner = callee.expression;
  if (
    ts.isCallExpression(inner) &&
    ts.isIdentifier(inner.expression) &&
    inner.expression.text === "expect"
  ) {
    return inner.arguments[0];
  }
  return undefined;
}

/** Calls `visit` on every node of `root`, with whether it stands in a function literal WHAT IS
 *  SKIPPED names. A call is visited before its arguments, so it marks the literals it skips
 *  before the walk goes down into them. */
function walkSkipped(root: ts.Node, visit: (node: ts.Node, skipped: boolean) => void): void {
  const skippedLiterals = new Set<ts.Node>();
  const walk = (node: ts.Node, inSkipped: boolean): void => {
    const skipped = inSkipped || skippedLiterals.has(node);
    visit(node, skipped);
    if (ts.isCallExpression(node)) {
      if (skipsArguments(node)) {
        for (const argument of node.arguments) {
          if (isFunctionLiteral(argument)) skippedLiterals.add(argument);
        }
      }
      const subject = toPassSubject(node);
      if (subject !== undefined && isFunctionLiteral(subject)) skippedLiterals.add(subject);
    }
    ts.forEachChild(node, (child) => walk(child, skipped));
  };
  walk(root, false);
}

/** `node` with its parentheses taken off. */
function unparenthesized(node: ts.Expression): ts.Expression {
  let current = node;
  while (ts.isParenthesizedExpression(current)) current = current.expression;
  return current;
}

/** Whether `node` begins a function of its own: a `return` inside it is not its parent's, and a
 *  name it declares is read in its own conditions and not its parent's. */
function isFunctionLike(node: ts.Node): boolean {
  return (
    ts.isFunctionDeclaration(node) ||
    ts.isFunctionExpression(node) ||
    ts.isArrowFunction(node) ||
    ts.isMethodDeclaration(node) ||
    ts.isConstructorDeclaration(node) ||
    ts.isGetAccessorDeclaration(node) ||
    ts.isSetAccessorDeclaration(node)
  );
}

/** Whether `fn` returns a probe from one of its own `return` statements, or is an arrow whose
 *  expression body is one: a probe call or `await` of one, parentheses read through, that stands
 *  in no skipped literal. */
function returnsProbe(fn: ts.FunctionLikeDeclaration, skipped: ReadonlySet<ts.Node>): boolean {
  const isProbe = (expression: ts.Expression): boolean => {
    let current = unparenthesized(expression);
    if (ts.isAwaitExpression(current)) current = unparenthesized(current.expression);
    return isProbeCall(current) && !skipped.has(current);
  };
  const body = fn.body;
  if (body === undefined) return false;
  if (!ts.isBlock(body)) return isProbe(body);
  let found = false;
  const visit = (node: ts.Node): void => {
    if (found || isFunctionLike(node)) return;
    if (ts.isReturnStatement(node) && node.expression !== undefined && isProbe(node.expression)) {
      found = true;
      return;
    }
    ts.forEachChild(node, visit);
  };
  ts.forEachChild(body, visit);
  return found;
}

/** The names of the wrappers `parsed` declares, collected in one pass over every file: WHAT A
 *  PROBE IS. */
function wrapperNames(parsed: readonly ParsedFile[]): Set<string> {
  const names = new Set<string>();
  for (const { sourceFile } of parsed) {
    const skipped = new Set<ts.Node>();
    walkSkipped(sourceFile, (node, isSkipped) => {
      if (isSkipped && isProbeCall(node)) skipped.add(node);
    });
    const visit = (node: ts.Node): void => {
      if (ts.isFunctionDeclaration(node) && node.name !== undefined) {
        if (returnsProbe(node, skipped)) names.add(node.name.text);
      } else if (
        ts.isVariableDeclaration(node) &&
        ts.isIdentifier(node.name) &&
        node.initializer !== undefined &&
        (node.parent.flags & ts.NodeFlags.Const) !== 0
      ) {
        const initializer = unparenthesized(node.initializer);
        if (isFunctionLiteral(initializer) && returnsProbe(initializer, skipped)) {
          names.add(node.name.text);
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(sourceFile);
  }
  return names;
}

/** The name `call` is a probe under: the member of a probe call, or the name of a wrapper it
 *  calls. Nothing where it is neither. */
function probeName(call: ts.CallExpression, wrappers: ReadonlySet<string>): string | undefined {
  const callee = call.expression;
  if (ts.isPropertyAccessExpression(callee)) {
    return probeMethods.has(callee.name.text) ? callee.name.text : undefined;
  }
  return ts.isIdentifier(callee) && wrappers.has(callee.text) ? callee.text : undefined;
}

/** The outermost expression the climb of WHAT DECIDES A PATH reaches from `probe`. */
function climb(probe: ts.Expression): ts.Expression {
  let node = probe;
  for (;;) {
    const parent = node.parent;
    if (
      ts.isAwaitExpression(parent) ||
      ts.isParenthesizedExpression(parent) ||
      (ts.isPrefixUnaryExpression(parent) && parent.operator === ts.SyntaxKind.ExclamationToken) ||
      (ts.isBinaryExpression(parent) && climbedOperators.has(parent.operatorToken.kind))
    ) {
      node = parent;
    } else {
      return node;
    }
  }
}

/** `test.skip`, `test.fixme`, `test.slow` or `test.fail`, where `node` is a call of one. */
function testModifier(node: ts.Node): string | undefined {
  if (!ts.isCallExpression(node)) return undefined;
  const callee = node.expression;
  if (
    ts.isPropertyAccessExpression(callee) &&
    ts.isIdentifier(callee.expression) &&
    callee.expression.text === "test" &&
    testModifiers.has(callee.name.text)
  ) {
    return `test.${callee.name.text}`;
  }
  return undefined;
}

/** What `node` is the condition of, as the first two kinds of WHAT DECIDES A PATH read it: `if`,
 *  `ternary`, `while`, `do-while`, `for` or a `test` modifier. Nothing where it decides no path,
 *  an `if` whose only branch is a `throw` among them. */
function conditionKind(node: ts.Expression): string | undefined {
  const parent = node.parent;
  if (ts.isIfStatement(parent) && parent.expression === node) {
    const then = parent.thenStatement;
    const onlyThrows =
      ts.isThrowStatement(then) ||
      (ts.isBlock(then) && then.statements.length === 1 && ts.isThrowStatement(then.statements[0]));
    return onlyThrows && parent.elseStatement === undefined ? undefined : "if";
  }
  if (ts.isConditionalExpression(parent) && parent.condition === node) return "ternary";
  if (ts.isWhileStatement(parent) && parent.expression === node) return "while";
  if (ts.isDoStatement(parent) && parent.expression === node) return "do-while";
  if (ts.isForStatement(parent) && parent.condition === node) return "for";
  const modifier = testModifier(parent);
  if (
    modifier !== undefined &&
    (parent as ts.CallExpression).arguments[0] === node &&
    !ts.isStringLiteralLike(node) &&
    !ts.isTemplateExpression(node) &&
    !isFunctionLiteral(node)
  ) {
    return modifier;
  }
  return undefined;
}

/** The function `node` stands in, or its file where it stands in none. */
function enclosingFunction(node: ts.Node): ts.Node {
  let current = node.parent;
  while (current !== undefined && !isFunctionLike(current) && !ts.isSourceFile(current)) {
    current = current.parent;
  }
  return current ?? node.getSourceFile();
}

/** One condition of a file: the expression, what it is the condition of, and the function it
 *  stands in, which a name bound to a probe's answer is read against. */
type Condition = {
  readonly expression: ts.Expression;
  readonly kind: string;
  readonly scope: ts.Node;
};

/** Every condition of `sourceFile` that decides a path, in the order the file writes them. */
function conditionsOf(sourceFile: ts.SourceFile): Condition[] {
  const conditions: Condition[] = [];
  const visit = (node: ts.Node): void => {
    if (ts.isExpression(node)) {
      const kind = conditionKind(node);
      if (kind !== undefined) {
        conditions.push({ expression: node, kind, scope: enclosingFunction(node) });
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(sourceFile);
  return conditions;
}

/** Whether `node` is a reference to a binding spelled `name`, rather than a property's name. */
function isReferenceTo(node: ts.Node, name: string): boolean {
  if (!ts.isIdentifier(node) || node.text !== name) return false;
  const parent = node.parent;
  if (ts.isPropertyAccessExpression(parent) && parent.name === node) return false;
  if (ts.isPropertyAssignment(parent) && parent.name === node) return false;
  return true;
}

/** The first reference to `name` under `root` that begins at or after `after`. */
function referenceAfter(
  sourceFile: ts.SourceFile,
  root: ts.Node,
  name: string,
  after: number
): ts.Node | undefined {
  let found: ts.Node | undefined;
  const visit = (node: ts.Node): void => {
    if (found !== undefined) return;
    if (node.getStart(sourceFile) >= after && isReferenceTo(node, name)) {
      found = node;
      return;
    }
    ts.forEachChild(node, visit);
  };
  visit(root);
  return found;
}

/** What `probe` decides, as `DecidingProbe.position` spells it, or nothing where it decides no
 *  path. `conditions` are its file's. */
function positionOf(
  sourceFile: ts.SourceFile,
  probe: ts.CallExpression,
  conditions: readonly Condition[]
): string | undefined {
  const top = climb(probe);
  const kind = conditionKind(top);
  if (kind !== undefined) return kind;
  const declaration = top.parent;
  if (
    !ts.isVariableDeclaration(declaration) ||
    declaration.initializer !== top ||
    !ts.isIdentifier(declaration.name) ||
    (declaration.parent.flags & (ts.NodeFlags.Const | ts.NodeFlags.Let)) === 0
  ) {
    return undefined;
  }
  const name = declaration.name.text;
  const scope = enclosingFunction(declaration);
  const uses: string[] = [];
  for (const condition of conditions) {
    if (condition.scope !== scope) continue;
    const reference = referenceAfter(sourceFile, condition.expression, name, declaration.getEnd());
    if (reference === undefined) continue;
    const line = sourceFile.getLineAndCharacterOfPosition(reference.getStart(sourceFile)).line;
    uses.push(`${condition.kind}@${line + 1}`);
  }
  if (uses.length === 0) return undefined;
  const keyword = (declaration.parent.flags & ts.NodeFlags.Const) !== 0 ? "const" : "let";
  return `${keyword} ${name} -> ${uses.join(",")}`;
}

/** Every probe the given files hold whose answer decides a path, each reported under the path it
 *  was named by - `suiteFiles`' path relative to `directory` - and ordered by file, line and
 *  column. The wrappers are read from these files and no others, so a wrapper declared in a file
 *  left out is no probe here.
 *
 * The walk is over the syntax tree rather than the text because the text cannot tell a call from
 * the word in a comment or a string that names one - this module's own header holds dozens - and
 * because the comment above a call is trivia the parser already has. */
export function probesOfFiles(directory: string, files: readonly string[]): DecidingProbe[] {
  const parsed: ParsedFile[] = files.map((file) => {
    const path = join(directory, file);
    const text = readFileSync(path, "utf8");
    return { file, sourceFile: ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true) };
  });
  const wrappers = wrapperNames(parsed);
  const probes: DecidingProbe[] = [];
  for (const { file, sourceFile } of parsed) {
    const text = sourceFile.getFullText();
    const conditions = conditionsOf(sourceFile);
    walkSkipped(sourceFile, (node, skipped) => {
      if (skipped || !ts.isCallExpression(node)) return;
      const name = probeName(node, wrappers);
      if (name === undefined) return;
      const position = positionOf(sourceFile, node, conditions);
      if (position === undefined) return;
      const start = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile));
      // The reason stands above the probe's statement, and is read against the probe's own line.
      const ranges = ts.getLeadingCommentRanges(text, statementOf(node).getFullStart()) ?? [];
      probes.push({
        file,
        line: start.line + 1,
        column: start.character,
        name,
        position,
        reason: reasonEndingAbove(sourceFile, ranges, start.line),
      });
    });
  }
  return probes.sort((first, second) => {
    if (first.file !== second.file) return first.file < second.file ? -1 : 1;
    return first.line - second.line || first.column - second.column;
  });
}

/** The probes the rule refuses, each with the sentence a reader of the failure acts on: which file
 *  and line, which probe and what its answer decides, what is wrong with what stands above it,
 *  and the two ways out - read a settled state, or write the reason. */
export function refusedProbes(probes: readonly DecidingProbe[]): RefusedProbe[] {
  const refused: RefusedProbe[] = [];
  for (const probe of probes) {
    const at =
      `${probe.file}:${probe.line} ${probe.name} (${probe.position}) decides a path on one ` +
      "instant's answer";
    const remedy =
      "read the state with `settledState` of `e2e/settled.ts`, which waits until the page has " +
      "settled on one of the states the branch chooses between; or, where one instant's answer " +
      "is what the row means to read, write the sentence that says why that answer is safe here, " +
      `at least ${minimumReasonWords} words, as a comment on the line directly above the probe, ` +
      "giving the probe a line of its own where it has none.";
    if (probe.reason === "") {
      refused.push({ probe, why: `${at} and has no reason on the line above it: ${remedy}` });
      continue;
    }
    const short = shortReason(probe.reason, "the probe", "safe to read once here");
    if (short !== undefined) {
      refused.push({ probe, why: `${at} and stands ${short}, so ${remedy}` });
    }
  }
  return refused;
}
