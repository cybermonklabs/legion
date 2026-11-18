/** Every zero-wait probe of the browser suite whose answer decides what a row or a helper does
 *  next - an `isVisible()`, a `count()` or a `getAttribute()` read as the condition of a branch,
 *  or an `all()` whose answer a loop walks - read from the source of this suite's own files, so
 *  one that says nothing about why one instant's answer is safe can be named.
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
 * `e2e/__tests__/zero-wait-probes.test.ts` is what runs it over the suite, and
 * `e2e/__tests__/zero-wait-loops.test.ts`, `e2e/__tests__/zero-wait-cases.test.ts` and
 * `e2e/__tests__/zero-wait-calls.test.ts` hold it to the loops, to a `switch`'s cases and to a
 * call on an answer whose result decides.
 *
 * THE RULE. A probe whose answer decides a path reads a state the page has settled on, or carries
 * a reason. The settled read is `settledState` of `e2e/settled.ts`: it reads its probes inside one
 * `expect.poll` until one of the states the branch chooses between shows, so it is no probe of
 * this rule's. The reason is a comment that ends on the line directly above the probe's line and
 * runs to at least `minimumReasonWords` words, saying why one instant's answer is safe there.
 *
 * - The measure of a reason is `e2e/fixed-waits.ts`'s: `reasonAboveCall` and `shortReason` are
 *   imported from there, so the suite holds one measure of a reason and not two. What that
 *   measure is for, and what no scan can judge about a sentence, is THE RULE of that module.
 * - The line is the probe CALL's, the line its first token stands on, and the comments read are
 *   the ones above the call's statement. So a probe on a later line of its statement stands under
 *   no reason, and its refusal tells the writer to give it a line of its own.
 *
 * WHAT A PROBE IS. A call `x.<name>(...)` whose member name is one of `probeMethods`. The list
 * follows one rule over Playwright's `Locator` methods: a probe is a method whose answer is the
 * page's state at one instant, waiting for nothing or at most for an element to attach, so none
 * of them waits for the state a branch chooses between. By that rule `isVisible`, `count`,
 * `all`, `getAttribute`, `evaluate`, `evaluateAll`, `boundingBox` and the rest `probeMethods`
 * holds are probes, and these are not: the actions, which change the page rather than answer its
 * state, most of them after waiting for the element to be actionable (`click`, `fill`, `press`,
 * `selectOption` and the rest), and `screenshot`, which waits for that too; the waits
 * themselves, `waitFor` and `waitForFunction`; the builders, whose answer is a locator and not a
 * state (`filter`, `nth`, `locator`, the `getBy` family, `normalize` and the rest); and what
 * reads no page (`page`, `description`, `toString`, `highlight` and `hideHighlight`). A probe
 * is read by its member NAME, so a call of that name on another object is one too:
 * `page.evaluate(...)` answers the page's state at one instant just as `locator.evaluate(...)`
 * does. A call whose receiver is the plain identifier `Promise` is no probe, though its member
 * is named like one: `Promise.all(...)` waits for the promises it is handed and reads no page.
 * A call of a wrapper is a probe as well. A wrapper is a function declaration, or a
 * `const name =` arrow or function expression, one of whose own `return` statements - a nested
 * function's do not count - returns a probe or `await` of one, or an arrow whose expression body
 * is one, parentheses read through: a helper that returns `page.evaluate(...)` or
 * `rows.boundingBox()` is one. The wrappers are collected by name in one pass over every file
 * read, before any probe is judged, because a helper declares one and a spec calls it: a spec's
 * `if (await menuShows(page))` branches on the `isVisible` that a helper's `menuShows` returns.
 *
 * WHAT DECIDES A PATH. The positions below are the places TypeScript's grammar gives a value to
 * decide what runs next, and `conditionKind` enumerates them by the `SyntaxKind` of the node that
 * holds the value, so a reader checks them against the grammar in one place. The list is the
 * grammar's, not the forms found so far. A probe decides a path where its answer stands in one of
 * them, in place or by the climb below, or is bound to a name that one of them reads. Each is
 * named in brackets as `DecidingProbe.position` names it:
 *
 * - the condition of an `if`, a `?:`, a `while`, a `do...while` or a `for` [`if`, `ternary`,
 *   `while`, `do-while`, `for`]. An `if` whose only branch is a `throw` is one too: a zero-wait
 *   read that throws fails the row on one instant's answer, the same flake as a wrong branch;
 * - the iterable of a `for...of` and the object of a `for...in` [`for-of`, `for-in`], whose answer
 *   decides how many turns the loop runs: `for (const row of await rows.all())`, read before the
 *   page has drawn the rows, runs no turn, and a row whose assertions stand inside the loop passes
 *   without one of them run;
 * - the discriminant of a `switch` and the expression of each of its `case`s [`switch`, `case`],
 *   which pick the case that runs;
 * - the left operand of an `&&`, an `||` or a `??` whose right operand runs code [`&&`, `||`,
 *   `??`], whatever the operator's own value goes on to - a statement, a `return`, an arrow's body
 *   or an argument - since the left operand decides whether the right one runs:
 *   `(await menu.isVisible()) && (await menu.click())` clicks or not on one instant's answer. An
 *   operand runs code where it holds, outside any function or class written in it, a call, a
 *   `new`, an `await`, a tagged template, a `delete`, a `++` or `--`, a `yield` or an assignment.
 *   A right operand of literals, names and property reads runs nothing and only picks a value, as
 *   in `(await head.getAttribute("href")) ?? ""`, so there the climb carries the value on;
 * - the target of an `&&=`, an `||=` or a `??=` [`&&=`, `||=`, `??=`], whose value decides whether
 *   the right side runs. No probe call can stand there, but a name bound to an answer can, as in
 *   `shown ||= await menu.click()`;
 * - what an optional chain reads with `?.` where the rest of the chain runs code [`?.`]: a call
 *   anywhere in it, or code in an element access's key, as in
 *   `(await menu.getAttribute("href"))?.startsWith("/")`;
 * - the source of a destructuring whose pattern holds a default that runs code [`default`]: in
 *   `const [first = fallback()] = await rows.all()` a missing element decides whether the default
 *   runs;
 * - the first argument of `test.skip`, `test.fixme`, `test.slow` or `test.fail` [`test.skip` and
 *   its kin], where it is no string, template or function. No grammar names it: Playwright's API
 *   decides by it how the row runs.
 *
 * The climb goes up from the probe through every parent, and stops at the first position it
 * reaches or at a stop. The stops are the places where the value leaves what the check reads,
 * which WHAT IT DOES NOT SEE names, is thrown away, or is bound to a name, and nothing else ends
 * the climb: a function or a class written in place; a `return`, a `yield` or a `throw`; an
 * element of an array literal, a spread among them, and a property of an object literal; the left
 * operand of a comma and a `void`, whose value is thrown away; the target of a plain `=`, where
 * the value goes rather than a value read there, while a compound operator such as `+=` reads its
 * target, so in `while ((n -= 1) >= 0)` the `n` reaches the `while`; an initializer that binds a
 * name, below; and a statement or a declaration, since the climb is over expressions. Every other
 * parent carries the value on: `await`, parentheses, `as`, `satisfies`, a non-null `!`, every
 * operand of an operator but those two left sides, either branch of a `?:`, a template literal, a
 * property or an element read on the answer, the key of an element access, and a call, a `new`,
 * a spread argument and a tagged template, the answer the receiver or an argument alike. So
 * `(await rows.all()).length > 0`, `Boolean(await menu.isVisible())`,
 * `(await menu.inputValue()).includes("B")` and `handlers[await menu.inputValue()]` each reach
 * the `if` they stand in. `carrier` enumerates the stops by `SyntaxKind`, as `conditionKind`
 * does the positions. The list is of stops and not of carriers, so a parent no one listed
 * carries the value on rather than ending the climb unread.
 *
 * A name is bound to the answer where the climb reaches the initializer of a declaration
 * (`const`, `let`, `var` or `using`) of a plain name or of a pattern, every name in the pattern
 * counting, as in `const [first] = await rows.all()` or `const { length } = await rows.all()`; a
 * parameter's default, as in `async function open(menu: Locator, shown = menu.isVisible())`; a
 * pattern element's default; or the right side of an assignment of any operator (`=`, `+=`, `&&=`,
 * `||=`, `??=` and the rest) to a name or to an array or object pattern. A target that is a
 * property or an element binds no name. A position reads the name where both of these hold:
 *
 * - it stands in the name's scope: the function or the file that holds a declaration, or, for an
 *   assignment, the outermost function around it that declares the name, or the file where the
 *   file does or no function does, so a closure that assigns a name of the function around it
 *   is read against that function; or in any function nested in that scope. It counts wherever
 *   the scope writes it, above the binding or below it, because the check reads no order of
 *   running: a later turn of a loop, a call of a function declared further down and a closure
 *   called later each read the name after a binding written below them;
 * - the name's value reaches the position as the answer written in the name's place would: the
 *   climb, run from a reference to the name, ends at that position. So `if (shown)`,
 *   `if (!shown)`, `if (shown && open)`, `if (rows.length > 0)`, `for (const row of rows)`,
 *   `switch (label)`, `shown ?? (await menu.click())`, `shown ||= await menu.click()`,
 *   `if (text.includes("B"))`, `if (Boolean(shown))`, `if (label?.trim())`,
 *   `for (const row of rows.slice(1))` and `if (handlers[label])` read the name. A reference is
 *   an identifier that reads the name's value: a property's name (`obj.shown`'s `shown`), a
 *   key, a declaration's own name, a label and a name written in a type are none. A second name
 *   copied or computed from the first, `const ok = shown` or `const label = text.trim()`, is
 *   another alias, which WHAT IT DOES NOT SEE names: the climb from `shown` stops at the
 *   initializer of `ok`, and a position that reads only `ok` reads no answer.
 *
 * Such a probe's position is written `const shown -> ternary@42`: how the name is bound (`const`,
 * `let`, `var`, `using`, `param`, `default`, or the assignment's operator), the names read, and
 * the kind and the line of each position that reads one.
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
 *   moment the row chose. What runs in the page is no probe, but the call's answer is: the
 *   answer of `evaluate`, `evaluateAll` and `evaluateHandle` is the page's state at the instant
 *   they ran, so a branch on it is read as a branch on any probe is.
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
 * WHAT IT DOES NOT SEE. One class: a decision made where the check does not read. The check reads
 * the expression an answer stands in, up to the first stop of the climb, and the positions of a
 * bound name's scope; it does not know what a function does with a value, or where a value is
 * read again under another name. A probe whose answer reaches a position only through one of the
 * three kinds below passes unread, and a pass here is a claim about everything else. A call's
 * RESULT that decides is no limit: `t.includes("B") ? a : b`, `if (Boolean(t))` and
 * `if (t?.trim())` are refused in both forms, `t` the answer in place or a name bound to it.
 *
 * - inside a function the answer is passed into: the callee's own body branches on its argument
 *   or on its `this`, as `await clickWhen(menu, await menu.isVisible())` written as a statement
 *   does, or `expect(await rows.count()).toBe(3)`, which checks one instant's count and never
 *   reads it again; a callback that `(await rows.all()).map(...)` or `.forEach(...)` runs once
 *   per element, so the answer decides inside the method how many times it runs; and a getter
 *   that a property read on the answer runs;
 * - after the answer is returned, yielded or thrown from a function, or is an arrow's expression
 *   body: the caller branches on what it gets. A function that returns a probe is a wrapper whose
 *   calls are probes (WHAT A PROBE IS), but one that returns the answer through a variable -
 *   `const shown = await menu.isVisible(); return shown;` - or a value computed from it, a
 *   wrapper that is a method, a `let` or a `var`, and one that returns another wrapper's call are
 *   not collected;
 * - through an object property, an array element or another alias: a store into a property or
 *   an element, `state.shown = await menu.isVisible()`, `answers[0] = await menu.isEnabled()`,
 *   `{ shown: await menu.isVisible() }`, `[await rows.count()]` or a spread into an array, and a
 *   key that names one, `{ [await menu.textContent()]: true }`; a name copied or computed from a
 *   bound name, `const ok = shown` or `const label = text.trim()`; a name bound to a probe or a
 *   wrapper rather than to its answer, `const shows = menuShows` or `menu.isVisible.bind(menu)`;
 *   a method destructured off a locator, `const { isVisible } = menu`; a member written in
 *   brackets, `menu["isVisible"]()`, or any other computed one; and a name another file imports
 *   from the file that binds it.
 *
 * WHERE IT READS TOO MUCH. These refuse a probe that may decide nothing, and no probe passes
 * through them; a reason or a rename answers each:
 *
 * - shadowing, and every other name read by its spelling rather than by its binding, since the
 *   scan resolves no reference to its declaration: a function of one file named like a wrapper
 *   of another is read as that wrapper, a position that refers to an inner binding named like a
 *   name an answer is bound to is read as reading that name, and an assignment is read against
 *   the outermost function that declares its name;
 * - a call whose result decides, which is read as deciding on its receiver and on every
 *   argument, though the callee may read only some of them: in
 *   `if (isReady(page, await rows.count()))` the count is refused where `isReady` never reads it;
 * - the order things run in, which the check does not read: every read in a name's scope counts,
 *   whatever runs first, so a read that runs before the answer exists is refused too, as in an
 *   `if` written above the binding that no loop and no call brings back to;
 * - a function passed to `expect.poll` by its name rather than written there, which is read as
 *   any function is.
 *
 * `probesOfFiles` reads the file system and everything else here is pure over its arguments: no
 * environment, no network, no database and no server, so any process may call it.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import * as ts from "typescript";

import { minimumReasonWords, reasonAboveCall, shortReason } from "./fixed-waits";

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
  /** What the answer decides, as WHAT DECIDES A PATH names it: the position the climb reaches,
   *  `if`, `ternary`, `while`, `do-while`, `for`, `for-of`, `for-in`, `switch`, `case`, `&&`,
   *  `||`, `??`, `&&=`, `||=`, `??=`, `?.`, `default` or `test.skip` and its kin; or a name bound
   *  to the answer, `const shown -> ternary@42`, with how it is bound, the names read and the kind
   *  and the line of each position that reads one, bindings the climb passes joined by `; `. */
  position: string;
  /** The reason comment that ends on the line directly above the call, its markers stripped and
   *  its lines joined by a space; the empty string when no comment ends there. */
  reason: string;
};

/** A probe the rule refuses, with the sentence that names it and says what to do about it. */
type RefusedProbe = {
  probe: DecidingProbe;
  why: string;
};

/** The reads WHAT A PROBE IS names, by the member name they are called under: every method of
 *  Playwright's `Locator` whose answer is the page's state at one instant, waiting for nothing or
 *  at most for an element to attach, as Playwright 1.64.1's `Locator` documents each. */
const probeMethods: ReadonlySet<string> = new Set([
  "all",
  "allInnerTexts",
  "allTextContents",
  "ariaSnapshot",
  "ariaSnapshotJSON",
  "boundingBox",
  "count",
  "elementHandle",
  "elementHandles",
  "evaluate",
  "evaluateAll",
  "evaluateHandle",
  "getAttribute",
  "innerHTML",
  "innerText",
  "inputValue",
  "isChecked",
  "isDisabled",
  "isEditable",
  "isEnabled",
  "isHidden",
  "isVisible",
  "textContent",
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

/** The logical operators: each decides by its left operand whether its right operand runs. */
const logicalOperators: ReadonlySet<ts.SyntaxKind> = new Set([
  ts.SyntaxKind.AmpersandAmpersandToken,
  ts.SyntaxKind.BarBarToken,
  ts.SyntaxKind.QuestionQuestionToken,
]);

/** The logical assignments: each decides by its target's value whether its right side runs. */
const logicalAssignments: ReadonlySet<ts.SyntaxKind> = new Set([
  ts.SyntaxKind.AmpersandAmpersandEqualsToken,
  ts.SyntaxKind.BarBarEqualsToken,
  ts.SyntaxKind.QuestionQuestionEqualsToken,
]);

/** One file of the scan, parsed. */
type ParsedFile = { readonly file: string; readonly sourceFile: ts.SourceFile };

/** The member name of `call` where it is a call `x.<name>(...)` of a probe, read by that name,
 *  and nothing where it is not. A call on the plain identifier `Promise` is none: `Promise.all`
 *  waits for the promises it is handed and reads no page. */
function probeMethodOf(call: ts.CallExpression): string | undefined {
  const callee = call.expression;
  if (!ts.isPropertyAccessExpression(callee) || !probeMethods.has(callee.name.text)) {
    return undefined;
  }
  const receiver = callee.expression;
  return ts.isIdentifier(receiver) && receiver.text === "Promise" ? undefined : callee.name.text;
}

/** Whether `node` is a call `x.<name>(...)` of a probe, as `probeMethodOf` reads one. */
function isProbeCall(node: ts.Node): node is ts.CallExpression {
  return ts.isCallExpression(node) && probeMethodOf(node) !== undefined;
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
 *  name it declares belongs to its own scope and not its parent's. */
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
  if (ts.isPropertyAccessExpression(callee)) return probeMethodOf(call);
  return ts.isIdentifier(callee) && wrappers.has(callee.text) ? callee.text : undefined;
}

/** Whether `operator` is an assignment's: `=`, a compound one such as `+=`, or `&&=`, `||=` or
 *  `??=`, the range the grammar's `SyntaxKind` gives them. */
function isAssignment(operator: ts.SyntaxKind): boolean {
  return operator >= ts.SyntaxKind.FirstAssignment && operator <= ts.SyntaxKind.LastAssignment;
}

/** Whether `root` runs code, as WHAT DECIDES A PATH reads it: it holds, outside any function or
 *  class written in it, a call, a `new`, an `await`, a tagged template, a `delete`, a `++` or a
 *  `--`, a `yield`, or an assignment of any operator. Literals, names and property reads run
 *  nothing the scan can see: the getter a property read may run is a function, the first of
 *  WHAT IT DOES NOT SEE. */
function runsCode(root: ts.Node): boolean {
  let found = false;
  const visit = (node: ts.Node): void => {
    if (found || isFunctionLike(node) || ts.isClassLike(node)) return;
    switch (node.kind) {
      case ts.SyntaxKind.CallExpression:
      case ts.SyntaxKind.NewExpression:
      case ts.SyntaxKind.AwaitExpression:
      case ts.SyntaxKind.TaggedTemplateExpression:
      case ts.SyntaxKind.DeleteExpression:
      case ts.SyntaxKind.YieldExpression:
        found = true;
        return;
      case ts.SyntaxKind.PrefixUnaryExpression:
      case ts.SyntaxKind.PostfixUnaryExpression: {
        const { operator } = node as ts.PrefixUnaryExpression | ts.PostfixUnaryExpression;
        if (
          operator === ts.SyntaxKind.PlusPlusToken ||
          operator === ts.SyntaxKind.MinusMinusToken
        ) {
          found = true;
          return;
        }
        break;
      }
      case ts.SyntaxKind.BinaryExpression:
        if (isAssignment((node as ts.BinaryExpression).operatorToken.kind)) {
          found = true;
          return;
        }
        break;
    }
    ts.forEachChild(node, visit);
  };
  visit(root);
  return found;
}

/** Whether the optional chain `link` begins, an access or a call written with `?.`, runs code past
 *  its `?.`: a call anywhere in the chain from `link` up, or code in an element access's key. */
function chainRunsCode(link: ts.Node): boolean {
  let current = link;
  for (;;) {
    if (ts.isCallExpression(current)) return true;
    if (ts.isElementAccessExpression(current) && runsCode(current.argumentExpression)) return true;
    const next = current.parent;
    if (!ts.isOptionalChain(next) || next.expression !== current) return false;
    current = next;
  }
}

/** `test.skip`, `test.fixme`, `test.slow` or `test.fail`, where `node` is the first argument of a
 *  call of one and is no string, template or function. Nothing where it is not. */
function testModifierKind(node: ts.Node): string | undefined {
  const call = node.parent;
  if (!ts.isCallExpression(call)) return undefined;
  const callee = call.expression;
  if (
    call.arguments[0] !== node ||
    !ts.isPropertyAccessExpression(callee) ||
    !ts.isIdentifier(callee.expression) ||
    callee.expression.text !== "test" ||
    !testModifiers.has(callee.name.text) ||
    ts.isStringLiteralLike(node) ||
    ts.isTemplateExpression(node) ||
    isFunctionLiteral(node)
  ) {
    return undefined;
  }
  return `test.${callee.name.text}`;
}

/** Whether the pattern a declaration or a parameter binds holds a default that runs code, nested
 *  patterns included. A plain name holds none. */
function bindingDefaultRunsCode(name: ts.BindingName): boolean {
  if (ts.isIdentifier(name)) return false;
  const elements: readonly ts.ArrayBindingElement[] = name.elements;
  return elements.some(
    (element) =>
      !ts.isOmittedExpression(element) &&
      ((element.initializer !== undefined && runsCode(element.initializer)) ||
        bindingDefaultRunsCode(element.name))
  );
}

/** Whether `target`, the left side of an `=` written as an array or object pattern, holds a
 *  default that runs code, nested patterns included. Any other target holds none. */
function assignedDefaultRunsCode(target: ts.Expression): boolean {
  const node = unparenthesized(target);
  if (ts.isArrayLiteralExpression(node)) {
    return node.elements.some((element) => assignedDefaultRunsCode(element));
  }
  if (ts.isObjectLiteralExpression(node)) {
    return node.properties.some((property) => {
      if (ts.isShorthandPropertyAssignment(property)) {
        const initializer = property.objectAssignmentInitializer;
        return initializer !== undefined && runsCode(initializer);
      }
      if (ts.isPropertyAssignment(property)) return assignedDefaultRunsCode(property.initializer);
      return ts.isSpreadAssignment(property) && assignedDefaultRunsCode(property.expression);
    });
  }
  if (ts.isSpreadElement(node)) return assignedDefaultRunsCode(node.expression);
  if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.EqualsToken) {
    return runsCode(node.right) || assignedDefaultRunsCode(node.left);
  }
  return false;
}

/** What `node` decides as WHAT DECIDES A PATH names it, by the `SyntaxKind` of the node that holds
 *  it, the grammar's places where a value picks what runs next: `if`, `ternary`, `while`,
 *  `do-while`, `for`, `for-of`, `for-in`, `switch`, `case`, `&&`, `||` or `??` for a left operand
 *  whose right one runs code, `&&=`, `||=` or `??=` for a logical assignment's target, `?.` for
 *  what an optional chain that runs code reads, `default` for the source of a destructuring whose
 *  pattern holds a default that runs code, or a `test` modifier. Nothing where it decides none. */
function conditionKind(node: ts.Node): string | undefined {
  const parent = node.parent;
  switch (parent.kind) {
    case ts.SyntaxKind.IfStatement:
      return (parent as ts.IfStatement).expression === node ? "if" : undefined;
    case ts.SyntaxKind.ConditionalExpression:
      return (parent as ts.ConditionalExpression).condition === node ? "ternary" : undefined;
    case ts.SyntaxKind.WhileStatement:
      return (parent as ts.WhileStatement).expression === node ? "while" : undefined;
    case ts.SyntaxKind.DoStatement:
      return (parent as ts.DoStatement).expression === node ? "do-while" : undefined;
    case ts.SyntaxKind.ForStatement:
      return (parent as ts.ForStatement).condition === node ? "for" : undefined;
    case ts.SyntaxKind.ForOfStatement:
      return (parent as ts.ForOfStatement).expression === node ? "for-of" : undefined;
    case ts.SyntaxKind.ForInStatement:
      return (parent as ts.ForInStatement).expression === node ? "for-in" : undefined;
    case ts.SyntaxKind.SwitchStatement:
      return (parent as ts.SwitchStatement).expression === node ? "switch" : undefined;
    case ts.SyntaxKind.CaseClause:
      return (parent as ts.CaseClause).expression === node ? "case" : undefined;
    case ts.SyntaxKind.BinaryExpression: {
      const binary = parent as ts.BinaryExpression;
      const operator = binary.operatorToken.kind;
      if (binary.left === node) {
        const decides =
          logicalAssignments.has(operator) ||
          (logicalOperators.has(operator) && runsCode(binary.right));
        return decides ? ts.tokenToString(operator) : undefined;
      }
      const destructures =
        binary.right === node &&
        operator === ts.SyntaxKind.EqualsToken &&
        assignedDefaultRunsCode(binary.left);
      return destructures ? "default" : undefined;
    }
    case ts.SyntaxKind.PropertyAccessExpression:
    case ts.SyntaxKind.ElementAccessExpression:
    case ts.SyntaxKind.CallExpression: {
      const link = parent as
        | ts.PropertyAccessExpression
        | ts.ElementAccessExpression
        | ts.CallExpression;
      if (link.expression === node && link.questionDotToken !== undefined && chainRunsCode(link)) {
        return "?.";
      }
      return testModifierKind(node);
    }
    case ts.SyntaxKind.VariableDeclaration:
    case ts.SyntaxKind.Parameter:
    case ts.SyntaxKind.BindingElement: {
      const declaration = parent as
        | ts.VariableDeclaration
        | ts.ParameterDeclaration
        | ts.BindingElement;
      const destructures =
        declaration.initializer === node && bindingDefaultRunsCode(declaration.name);
      return destructures ? "default" : undefined;
    }
    default:
      return undefined;
  }
}

/** The parent of `node` the climb of WHAT DECIDES A PATH goes on to: every parent but the stops
 *  below, enumerated by `SyntaxKind`, each a place where the value leaves what the check reads
 *  (WHAT IT DOES NOT SEE), is thrown away, is bound to a name `bindingOf` reads, or leaves the
 *  expression. So a call carries the value, the answer its receiver or an argument alike: a
 *  decision on a call's result is read, and only one made inside the callee is not. */
function carrier(node: ts.Node): ts.Node | undefined {
  const parent = node.parent;
  switch (parent.kind) {
    // The answer leaves the function: a decision on it is made by the caller.
    case ts.SyntaxKind.YieldExpression:
      return undefined;
    // An element of an array literal, a spread among them, or a property of an object literal: a
    // decision on it reads the element or the property. A shorthand's default in a pattern,
    // `({ open = await menu.isChecked() } = options)`, is a binding `bindingOf` reads. A spread
    // in a call's or a `new`'s arguments is no element, and carries.
    case ts.SyntaxKind.ArrayLiteralExpression:
    case ts.SyntaxKind.PropertyAssignment:
    case ts.SyntaxKind.ShorthandPropertyAssignment:
    case ts.SyntaxKind.SpreadAssignment:
      return undefined;
    // The value is thrown away.
    case ts.SyntaxKind.VoidExpression:
      return undefined;
    case ts.SyntaxKind.BinaryExpression: {
      // The comma throws its left operand's value away, and the target of a plain `=` is where
      // the value goes, not a value read there. A compound operator such as `+=` reads its
      // target, so its left side carries; that of an `&&=`, an `||=` or a `??=` is a position
      // `conditionKind` names, where the climb has ended before it asks here.
      const binary = parent as ts.BinaryExpression;
      const operator = binary.operatorToken.kind;
      const leftDropped =
        operator === ts.SyntaxKind.CommaToken || operator === ts.SyntaxKind.EqualsToken;
      return leftDropped && binary.left === node ? undefined : parent;
    }
    // An initializer, where `bindingOf` reads the name it binds.
    case ts.SyntaxKind.VariableDeclaration:
    case ts.SyntaxKind.Parameter:
    case ts.SyntaxKind.BindingElement:
      return undefined;
    // A class field's or an enum member's initializer: the climb is over expressions.
    case ts.SyntaxKind.PropertyDeclaration:
    case ts.SyntaxKind.EnumMember:
      return undefined;
    default:
      // A function or a class written in place: a decision in it is made inside a function the
      // answer is passed into, or after it is returned. A statement, a declaration's among them:
      // the climb is over expressions. A `return` and a `throw` are statements, so the answer
      // they hand on leaves the function, and its caller decides on it.
      return isFunctionLike(parent) || ts.isClassLike(parent) || ts.isStatement(parent)
        ? undefined
        : parent;
  }
}

/** The nodes the climb of WHAT DECIDES A PATH passes from `start`, `start` first: up through each
 *  parent `carrier` names, to the first node `conditionKind` names, or else to the outermost node
 *  the value reaches. `start` is a probe call, or a reference to a name bound to its answer. */
function climb(start: ts.Expression): ts.Node[] {
  const path: ts.Node[] = [start];
  let node: ts.Node = start;
  while (conditionKind(node) === undefined) {
    const parent = carrier(node);
    if (parent === undefined) break;
    path.push(parent);
    node = parent;
  }
  return path;
}

/** The function `node` stands in, or its file where it stands in none. */
function enclosingFunction(node: ts.Node): ts.Node {
  let current = node.parent;
  while (current !== undefined && !isFunctionLike(current) && !ts.isSourceFile(current)) {
    current = current.parent;
  }
  return current ?? node.getSourceFile();
}

/** Whether `identifier` reads the value of the name it spells, by what it is: no reference is
 *  its parent's `name`, `propertyName` or `label`, which is a property's name (`obj.shown`'s
 *  `shown`, `{ shown: 1 }`'s key, `{ shown: s }`'s in a pattern), a declaration's own name or a
 *  label. A shorthand property's name is one: `{ shown }` reads `shown`. */
function isReference(identifier: ts.Identifier): boolean {
  const parent = identifier.parent;
  if (ts.isShorthandPropertyAssignment(parent)) return true;
  const { name, propertyName, label } = parent as {
    name?: ts.Node;
    propertyName?: ts.Node;
    label?: ts.Node;
  };
  return identifier !== name && identifier !== propertyName && identifier !== label;
}

/** The names `name` binds: itself where it is a plain name, else every name of the pattern,
 *  nested patterns included. */
function boundNames(name: ts.BindingName): string[] {
  if (ts.isIdentifier(name)) return [name.text];
  const elements: readonly ts.ArrayBindingElement[] = name.elements;
  return elements.flatMap((element) =>
    ts.isOmittedExpression(element) ? [] : boundNames(element.name)
  );
}

/** The names an assignment to `target` binds: the name it is, or every name an array or object
 *  pattern assigns, shorthand, spread and defaults included. A property or an element binds
 *  none. */
function assignedNames(target: ts.Expression): string[] {
  const node = unparenthesized(target);
  if (ts.isIdentifier(node)) return [node.text];
  if (ts.isArrayLiteralExpression(node)) {
    return node.elements.flatMap((element) => assignedNames(element));
  }
  if (ts.isObjectLiteralExpression(node)) {
    return node.properties.flatMap((property) => {
      if (ts.isShorthandPropertyAssignment(property)) return [property.name.text];
      if (ts.isPropertyAssignment(property)) return assignedNames(property.initializer);
      return ts.isSpreadAssignment(property) ? assignedNames(property.expression) : [];
    });
  }
  if (ts.isSpreadElement(node)) return assignedNames(node.expression);
  if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.EqualsToken) {
    return assignedNames(node.left);
  }
  return [];
}

/** Names a probe's answer is bound to: how (`const`, `let`, `var`, `using`, `param`, `default`, or
 *  the assignment's operator), the names, the node that binds them, and whether it is an
 *  assignment rather than a declaration. */
type Binding = {
  readonly how: string;
  readonly names: readonly string[];
  readonly node: ts.Node;
  readonly assigns: boolean;
};

/** The binding `node` is the whole value of, by the `SyntaxKind` of its parent: the initializer of
 *  a variable declaration, of a parameter or of a pattern element, the right side of an assignment
 *  of any operator whose target is a name or a pattern, or the default of a shorthand property in
 *  an assignment pattern. Nothing where it is none, or binds no name. */
function bindingOf(node: ts.Node): Binding | undefined {
  const parent = node.parent;
  switch (parent.kind) {
    case ts.SyntaxKind.VariableDeclaration: {
      const declaration = parent as ts.VariableDeclaration;
      if (declaration.initializer !== node) return undefined;
      // `await using` sets the `Using` flag with `Const`, so `Using` is read first.
      const flags = declaration.parent.flags;
      let how = "var";
      if ((flags & ts.NodeFlags.Using) !== 0) how = "using";
      else if ((flags & ts.NodeFlags.Const) !== 0) how = "const";
      else if ((flags & ts.NodeFlags.Let) !== 0) how = "let";
      return { how, names: boundNames(declaration.name), node: declaration, assigns: false };
    }
    case ts.SyntaxKind.Parameter:
    case ts.SyntaxKind.BindingElement: {
      const declaration = parent as ts.ParameterDeclaration | ts.BindingElement;
      if (declaration.initializer !== node) return undefined;
      const how = ts.isParameter(declaration) ? "param" : "default";
      return { how, names: boundNames(declaration.name), node: declaration, assigns: false };
    }
    case ts.SyntaxKind.ShorthandPropertyAssignment: {
      const shorthand = parent as ts.ShorthandPropertyAssignment;
      if (shorthand.objectAssignmentInitializer !== node) return undefined;
      return { how: "default", names: [shorthand.name.text], node: shorthand, assigns: true };
    }
    case ts.SyntaxKind.BinaryExpression: {
      const assignment = parent as ts.BinaryExpression;
      const operator = assignment.operatorToken.kind;
      if (assignment.right !== node || !isAssignment(operator)) return undefined;
      const names = assignedNames(assignment.left);
      const how = ts.tokenToString(operator);
      if (names.length === 0 || how === undefined) return undefined;
      return { how, names, node: assignment, assigns: true };
    }
    default:
      return undefined;
  }
}

/** Whether `scope`, a function or the file, declares one of `names` itself, by spelling: a variable
 *  or a parameter of its own, outside any function nested in it. */
function declares(scope: ts.Node, names: readonly string[]): boolean {
  let found = false;
  const visit = (node: ts.Node): void => {
    if (found || (node !== scope && isFunctionLike(node))) return;
    if (
      (ts.isVariableDeclaration(node) || ts.isParameter(node)) &&
      boundNames(node.name).some((name) => names.includes(name))
    ) {
      found = true;
      return;
    }
    ts.forEachChild(node, visit);
  };
  visit(scope);
  return found;
}

/** The function or the file whose positions read a name `binding` binds, by spelling: for a
 *  declaration, the one that holds it; for an assignment, the outermost function around it that
 *  declares one of its names, or the file where the file declares one or no function does. The
 *  outermost, so a name declared again in between reads more positions rather than fewer. */
function scopeOf(binding: Binding): ts.Node {
  if (!binding.assigns) return enclosingFunction(binding.node);
  let declaring: ts.Node | undefined;
  for (let scope = enclosingFunction(binding.node); ; scope = enclosingFunction(scope)) {
    if (ts.isSourceFile(scope)) {
      return declaring === undefined || declares(scope, binding.names) ? scope : declaring;
    }
    if (declares(scope, binding.names)) declaring = scope;
  }
}

/** Where the positions of the scope of `binding` read a name it binds, as
 *  `DecidingProbe.position` spells it: `const shown -> ternary@42`. Nothing where none does.
 *
 * One walk of the scope `scopeOf` names, nested functions included, reads it: from each
 * reference to a bound name, an identifier `isReference` reads as one, the climb runs as it would
 * from an answer written in its place, and where it ends at a position, that position reads the
 * name, at the line of the first reference that reaches it. A name written in a type is none, so
 * the walk goes into no type: `x as shown` reads no `shown`. An instantiation expression, `f<T>`,
 * is a value, and only its type arguments are types. The positions are listed in the order the
 * file writes them: by where each starts, and one before a position it holds that starts with
 * it. */
function bindingReads(binding: Binding): string | undefined {
  const sourceFile = binding.node.getSourceFile();
  const read = new Set<string>();
  const uses = new Map<ts.Node, string>();
  const visit = (node: ts.Node): void => {
    if (ts.isTypeNode(node) && !ts.isExpressionWithTypeArguments(node)) return;
    if (ts.isIdentifier(node) && binding.names.includes(node.text) && isReference(node)) {
      const path = climb(node);
      const position = path[path.length - 1];
      const kind = conditionKind(position);
      if (kind !== undefined) {
        read.add(node.text);
        if (!uses.has(position)) {
          const line = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line;
          uses.set(position, `${kind}@${line + 1}`);
        }
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(scopeOf(binding));
  if (uses.size === 0) return undefined;
  const ordered = [...uses]
    .sort(
      ([first], [second]) =>
        first.getStart(sourceFile) - second.getStart(sourceFile) || second.getEnd() - first.getEnd()
    )
    .map(([, use]) => use);
  const names = binding.names.filter((name) => read.has(name));
  return `${binding.how} ${names.join(",")} -> ${ordered.join(",")}`;
}

/** What `probe` decides, as `DecidingProbe.position` spells it, or nothing where it decides no
 *  path: the position the climb reaches, or else every binding it passes that a position reads. */
function positionOf(probe: ts.CallExpression): string | undefined {
  const path = climb(probe);
  const kind = conditionKind(path[path.length - 1]);
  if (kind !== undefined) return kind;
  const reads: string[] = [];
  for (const node of path) {
    const binding = bindingOf(node);
    const read = binding === undefined ? undefined : bindingReads(binding);
    if (read !== undefined) reads.push(read);
  }
  return reads.length === 0 ? undefined : reads.join("; ");
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
    walkSkipped(sourceFile, (node, skipped) => {
      if (skipped || !ts.isCallExpression(node)) return;
      const name = probeName(node, wrappers);
      if (name === undefined) return;
      const position = positionOf(node);
      if (position === undefined) return;
      const start = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile));
      probes.push({
        file,
        line: start.line + 1,
        column: start.character,
        name,
        position,
        reason: reasonAboveCall(node),
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
