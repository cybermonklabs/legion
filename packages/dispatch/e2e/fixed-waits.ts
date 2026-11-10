/** Every fixed wait of the browser suite - every call of `waitForTimeout`, and every call of the
 *  name a file binds `node:timers/promises`' `setTimeout` to - read from the source
 *  of this suite's own files, so one that says nothing about why it is there can be named.
 *
 * A fixed wait is two different things wearing one face. It is the measurement when the row's
 * claim IS a span of time: a quiet period the page must stay quiet through, an absence window in
 * which something must not happen, a pause that is part of the gesture under test. It is the
 * flake when it stands in for an assertion: the row sleeps a number someone guessed and then
 * reads a state that was going to arrive anyway, so a loaded machine reads it too early and the
 * suite fails for no defect. The two are told apart by one thing only - whether the writer said
 * which it is - and this module is that rule's machine check.
 * `e2e/__tests__/fixed-waits.test.ts` is what runs it over the suite.
 *
 * THE RULE. A fixed wait carries a reason when a comment ends on the line directly
 * above it and runs to at least `minimumReasonWords` words.
 *
 * - The line directly above, and nowhere else. A sentence further up the file, or beside the
 *   constant the call's argument is named by, is a sentence the next reader of the call does not
 *   see; the scan reads the one line a writer cannot miss. A blank line between the comment and
 *   the call breaks it, since that comment belongs to whatever stands above the gap.
 * - A comment trailing the call on its own line does not count. One form, read one way, leaves
 *   room for a sentence rather than for the half a line that is left after the call.
 * - The word count is what a bare `// wait` runs into: it, `// settle`, `// give it a moment` and
 *   `// 300` are refused by the same measure, because none of them says what is being waited for
 *   or why that long is enough. Eight words is not a test of prose - no scan can judge that - it
 *   is the length at which a writer has to name something outside the call itself. A reason that
 *   passes it and still says nothing is a review's to catch, and a reason worth writing runs well
 *   past it: every one in this suite does.
 * - The line is the CALL's, not its statement's. A wait on a later line of a bigger expression
 *   carries whatever stands above that line and nothing of the statement's own comment, so a
 *   wait written where no reason can stand above it is refused until it is given a line.
 *
 * THE TWO SPELLINGS. A row waits on a number two ways in this suite. `page.waitForTimeout(ms)`
 * is Playwright's, and `await sleep(ms)` is `node:timers/promises`' `setTimeout` under the alias
 * fourteen spec files import it by. They are the same wait - a number someone chose, spent -
 * and a gate that refused one and allowed the other would teach its readers which to write. So
 * both are read, and the second is read from the IMPORT rather than from the name: the scan
 * takes whatever local name a file binds that export to, so a file that calls it something else
 * is read too, and a `sleep` of its own that comes from anywhere else is not. Nothing matched is
 * text, so the SQL `pg_sleep` of `failed-room.e2e.ts` and `keyed-writes.e2e.ts` - strings handed
 * to Postgres, which no row waits on in this process - is no wait of this rule's, and a row of
 * `e2e/__tests__/fixed-waits.test.ts` holds that.
 *
 * WHAT IS READ. Every `.ts` file under the `e2e` directory except two sets. `__tests__` holds
 * this layer's own tests and the controls they plant, and a fixture there is written to be
 * refused. `harness-*.ts` is the run's starter: its sleeps are a process starter's backoff
 * between attempts to reach a port it has just launched, paced against a deadline it owns and
 * answerable to no page, where a row's wait stands between a gesture and a reading of the
 * product. The two part there: a wait this rule is about is one a row spends while the thing
 * under test is deciding. What is left is the specs and the helpers they import alike
 * (`e2e/touch.ts` holds a wait of its own), since a fixed wait is a fixed wait wherever a row
 * reaches it.
 *
 * A call is read as a wait by the NAME in its callee position, unlike `e2e/no-browser-rows.ts`,
 * which asks TypeScript what a callee's type is. The question there is which calls declare a row,
 * and a call it fails to recognise is a row that escapes the rule; the question here is whether a
 * line of prose stands above a call, and a name is the whole of what a reader of the source sees.
 * Reading the name also fails the safe way: anything spelled `waitForTimeout` - a `Page`, a
 * `Frame`, a helper of this suite's own - is asked for its reason rather than skipped. No type
 * checker is needed for that, so the scan parses each file and builds no program.
 *
 * What it does NOT read: `Bun.sleep`, which `e2e/fake-envoy.ts` and `e2e/local-instance.ts` use
 * for a fixture's configured delay and a dev server's shutdown, neither of them a row's wait;
 * and a page clock jumped forward, which spends no time at all. A pass here is a claim about the
 * two spellings above and about nothing else.
 *
 * `suiteFiles` and `waitsOfFiles` read the file system and everything else here is pure over its
 * arguments: no environment, no network, no database and no server, so any process may call it.
 */

import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import * as ts from "typescript";

/** One fixed wait of one file of the suite: a `waitForTimeout` or a timers `setTimeout` call. */
export type FixedWait = {
  /** The file, relative to the `e2e` directory: `inbox.e2e.ts`, and `touch.ts` for a helper. */
  file: string;
  /** The 1-based line the call stands on, which is where a reader of a refusal looks. */
  line: number;
  /** The call as it is written - `page.waitForTimeout(300)` - for a refusal to quote, so the
   *  reader knows which of a file's waits is meant without opening it. */
  call: string;
  /** The reason comment that ends on the line directly above the call, its markers stripped and
   *  its lines joined by a space; the empty string when no comment ends there. */
  reason: string;
};

/** A wait the rule refuses, with the sentence that names it and says what to do about it. */
export type RefusedWait = {
  wait: FixedWait;
  why: string;
};

/** How many words a reason must run to. See THE RULE above for what this measure is for and what
 *  it cannot do. A word is a run of non-space characters holding a letter, so `// 300` is none. */
export const minimumReasonWords = 8;

/** The files the walk below never reads, as their directory or their name says: `node_modules`
 *  as Playwright's own default ignores it, `test-results` because that is the config's
 *  `outputDir` and holds traces and videos rather than source, `__tests__` because this layer's
 *  own tests and their planted controls are not the suite, and `harness-*.ts` for the reason
 *  WHAT IS READ gives - a starter's backoff is no row's wait. */
const skippedDirectories = new Set(["__tests__", "node_modules", "test-results"]);
const skippedFile = /^harness-[^/]*\.ts$/;

/** Every `.ts` file of the suite under `directory`, relative to it and sorted: the specs and the
 *  helpers they import alike, since a fixed wait is one wherever it is written. */
export function suiteFiles(directory: string): string[] {
  const files: string[] = [];
  const walk = (relative: string): void => {
    for (const entry of readdirSync(join(directory, relative), { withFileTypes: true })) {
      const path = relative === "" ? entry.name : `${relative}/${entry.name}`;
      if (entry.isDirectory()) {
        if (!skippedDirectories.has(entry.name)) walk(path);
      } else if (entry.isFile() && entry.name.endsWith(".ts") && !skippedFile.test(entry.name)) {
        files.push(path);
      }
    }
  };
  walk("");
  return files.sort();
}

/** The module whose `setTimeout` is the suite's other spelling of a fixed wait. */
const timersModule = "node:timers/promises";

/** The names a file calls a fixed wait by, read from its imports: `waitForTimeout` however it is
 *  reached, and whatever local name the file bound `node:timers/promises`' `setTimeout` to -
 *  `sleep` in every spec that imports it today, but the import is what the scan reads, so a file
 *  that picks another name is read too and a same-named function of its own is not. */
type WaitNames = {
  /** Bare names a call of which is a wait: `sleep`, and `waitForTimeout` where a file
   *  destructured one off a page. */
  readonly bare: ReadonlySet<string>;
  /** Names a `setTimeout` member of which is a wait: `import * as timers` and a default import,
   *  both of which reach the same export through a member rather than a binding of their own. */
  readonly namespaces: ReadonlySet<string>;
};

/** What `sourceFile` calls a fixed wait by, read from its import declarations. */
function waitNames(sourceFile: ts.SourceFile): WaitNames {
  const bare = new Set(["waitForTimeout"]);
  const namespaces = new Set<string>();
  for (const statement of sourceFile.statements) {
    if (!ts.isImportDeclaration(statement)) continue;
    const specifier = statement.moduleSpecifier;
    if (!ts.isStringLiteralLike(specifier) || specifier.text !== timersModule) continue;
    const clause = statement.importClause;
    if (clause === undefined) continue;
    // `import timers from "node:timers/promises"`, whose default is the module's own object.
    if (clause.name !== undefined) namespaces.add(clause.name.text);
    const bindings = clause.namedBindings;
    if (bindings === undefined) continue;
    if (ts.isNamespaceImport(bindings)) {
      namespaces.add(bindings.name.text);
      continue;
    }
    for (const element of bindings.elements) {
      // `setTimeout as sleep` carries its export name in `propertyName`; `setTimeout` alone
      // carries it in `name`, which is then the local name as well.
      const exported = element.propertyName?.text ?? element.name.text;
      if (exported === "setTimeout") bare.add(element.name.text);
    }
  }
  return { bare, namespaces };
}

/** Whether `call` is a call of a wait, however it was reached: `page.waitForTimeout(…)`,
 *  `board.waitForTimeout(…)`, a name destructured off a page, `sleep(…)` under the import above,
 *  or `timers.setTimeout(…)` through a namespace. A string is never read, so the SQL `pg_sleep`
 *  this suite hands to Postgres is no call of anything here. */
function isFixedWait(call: ts.CallExpression, names: WaitNames): boolean {
  const callee = call.expression;
  if (ts.isPropertyAccessExpression(callee)) {
    if (callee.name.text === "waitForTimeout") return true;
    return (
      callee.name.text === "setTimeout" &&
      ts.isIdentifier(callee.expression) &&
      names.namespaces.has(callee.expression.text)
    );
  }
  if (ts.isElementAccessExpression(callee)) {
    const argument = callee.argumentExpression;
    return ts.isStringLiteralLike(argument) && argument.text === "waitForTimeout";
  }
  return ts.isIdentifier(callee) && names.bare.has(callee.text);
}

/** The statement a call stands in: the ancestor whose own parent holds a list of statements. A
 *  reason is written above that statement's first line, which is the call's own line whenever the
 *  wait is a statement of its own and some earlier line otherwise. */
function statementOf(call: ts.CallExpression): ts.Node {
  let node: ts.Node = call;
  while (
    node.parent !== undefined &&
    !ts.isBlock(node.parent) &&
    !ts.isSourceFile(node.parent) &&
    !ts.isModuleBlock(node.parent) &&
    !ts.isCaseClause(node.parent) &&
    !ts.isDefaultClause(node.parent)
  ) {
    node = node.parent;
  }
  return node;
}

/** One comment's words, its markers taken off: `//`, `/*`, the `*` a block comment's later lines
 *  begin with, and the closing `*\/`. */
function commentText(source: string): string {
  return source
    .split("\n")
    .map((line) =>
      line
        .replace(/^\s*(\/\/+|\/\*+|\*+)/, "")
        .replace(/\*+\/\s*$/, "")
        .trim()
    )
    .join(" ")
    .trim();
}

/** The words of a reason: the runs of non-space characters that hold a letter, so a line that is
 *  only a number or an arrow counts for nothing. */
export function reasonWords(reason: string): string[] {
  return reason.split(/\s+/).filter((word) => /[A-Za-z]/.test(word));
}

/** The reason standing above a call: every comment of an unbroken run that ends on the line
 *  directly above `callLine`, read as one sentence. A run is unbroken when each comment ends on
 *  the line before the next one begins, so `//` lines written together are one reason and a blank
 *  line between them ends it. */
function reasonAbove(sourceFile: ts.SourceFile, statement: ts.Node, callLine: number): string {
  const text = sourceFile.getFullText();
  const ranges = ts.getLeadingCommentRanges(text, statement.getFullStart()) ?? [];
  const lineOf = (position: number): number =>
    sourceFile.getLineAndCharacterOfPosition(position).line;
  const reason: string[] = [];
  let endsOn = callLine - 1;
  for (let index = ranges.length - 1; index >= 0; index -= 1) {
    const range = ranges[index];
    if (lineOf(range.end - 1) !== endsOn) break;
    reason.unshift(commentText(text.slice(range.pos, range.end)));
    endsOn = lineOf(range.pos) - 1;
  }
  return reason.join(" ").trim();
}

/** Every fixed wait the given files hold, each reported under the path it was named by:
 *  `suiteFiles`' path relative to `directory`.
 *
 * The walk is over the syntax tree rather than the text because the text cannot tell a call from
 * the word in a comment or a string that names one - this module's own header holds a dozen - and
 * because the comment above a call is trivia the parser already has. */
export function waitsOfFiles(directory: string, files: readonly string[]): FixedWait[] {
  const waits: FixedWait[] = [];
  for (const file of files) {
    const path = join(directory, file);
    const sourceFile = ts.createSourceFile(
      path,
      readFileSync(path, "utf8"),
      ts.ScriptTarget.Latest,
      true
    );
    const names = waitNames(sourceFile);
    const visit = (node: ts.Node): void => {
      if (ts.isCallExpression(node) && isFixedWait(node, names)) {
        const callLine = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line;
        waits.push({
          file,
          line: callLine + 1,
          call: node.getText(sourceFile),
          reason: reasonAbove(sourceFile, statementOf(node), callLine),
        });
      }
      ts.forEachChild(node, visit);
    };
    ts.forEachChild(sourceFile, visit);
  }
  return waits;
}

/** The waits the rule refuses, each with the sentence a reader of the failure acts on: which file
 *  and line, which call, what is wrong with what stands above it, and the two ways out - write
 *  the reason, or wait for the thing itself instead of for a number. */
export function refusedWaits(waits: readonly FixedWait[]): RefusedWait[] {
  const refused: RefusedWait[] = [];
  for (const wait of waits) {
    const at = `${wait.file}:${wait.line} ${wait.call}`;
    const remedy =
      "write the sentence that says what this wait measures, or what must not happen during it " +
      "and why that long is enough to see it, as a comment on the line directly above the call, " +
      "giving the call a line of its own where it has none; or, where the row is really waiting " +
      "for a state, replace the wait with a web-first assertion or an `expect.poll` for that " +
      "state, which retries until it holds instead of guessing how long it takes.";
    if (wait.reason === "") {
      refused.push({ wait, why: `${at} has no reason on the line above it: ${remedy}` });
      continue;
    }
    const words = reasonWords(wait.reason);
    if (words.length < minimumReasonWords) {
      refused.push({
        wait,
        why:
          `${at} stands under "${wait.reason}", which is ${words.length} ` +
          `${words.length === 1 ? "word" : "words"} where the rule asks for ` +
          `${minimumReasonWords}: a line that short names the wait rather than saying why it is ` +
          `here, so ${remedy}`,
      });
    }
  }
  return refused;
}
