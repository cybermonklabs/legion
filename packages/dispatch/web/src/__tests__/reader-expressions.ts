import { controlCharacterPattern, PROJECT_KEY_PATTERN, referencePattern } from "@legion/contracts";

import {
  dispatchReferencePattern,
  issueArtifactReferencePattern,
  issueKeyOnlyPattern,
  issuePathPattern,
} from "../features/refs/routes";
import { githubIssuePath, trailingRun, whitespaceOnly } from "../features/refs/Unfurl";

/**
 * Every regular expression that the one-pass rows' reads run over a text at least as long as the
 * row's floor (`stepsAtNAndFourN` in `one-pass.ts`), with the name
 * `features/refs/reader-expressions.test.ts` proves it reads a text in one pass under: the
 * reference grammar runs over a whole body, a `dispatch://` reference read from it is parsed with
 * the next five, a dashboard address with the seventh, and a GitHub URL's path is read with the
 * eighth. The last two are what the unfurl cards read a body with between its references and
 * after each one. The same reads reach more expressions of `routes.ts`, which those rows run over
 * no text that long and this list does not hold, among them the patterns `parseNodeReference`
 * runs over a reference's target for a project's or an agent's artifact, an item or the log, and
 * the other patterns that read an address's page path.
 *
 * `expectOnePass` (`one-pass.ts`) holds a reader to this list: a row fails when the read it
 * counts runs an expression the list does not hold, because a step count cannot see an expression
 * that backtracks, which does all its work inside one call.
 */
export const readerExpressions: readonly (readonly [string, RegExp])[] = [
  ["the reference grammar", referencePattern],
  ["a control character", controlCharacterPattern],
  ["a `dispatch://` reference", dispatchReferencePattern],
  ["an issue key", issueKeyOnlyPattern],
  ["a project key", PROJECT_KEY_PATTERN],
  ["an issue reference's document target", issueArtifactReferencePattern],
  ["a dashboard issue address", issuePathPattern],
  ["a GitHub issue's path", githubIssuePath],
  ["the whitespace between references", whitespaceOnly],
  ["the run after a reference", trailingRun],
];

/** An expression as this list compares it: its source and its flags, less `g`, `y` and `d`,
 *  which say where a match starts or what it reports and not how the expression reads. A
 *  `split` runs a copy of its expression with `y` added. It runs no expression itself, since
 *  `expressionsRunBy` calls it from inside the method it records. */
export function expressionKey(expression: RegExp): string {
  const flags = [...expression.flags].filter((flag) => !"dgy".includes(flag)).join("");
  return `/${expression.source}/${flags}`;
}

/** The expressions `read` runs over a text of at least `shortest` characters, as
 *  `expressionKey` writes them. Every match a string or an expression makes - `exec`, `test`,
 *  `match`, `matchAll`, `replace`, `search` and `split` - goes through `RegExp.prototype.exec`
 *  (ECMAScript's RegExpExec), so `read` runs with that one method recording, and the method is
 *  put back however `read` ends.
 *
 *  A shorter text is left out because an expression over a text that does not grow with the body
 *  costs the same on every body, however it is written. A library runs such expressions over a
 *  name: happy-dom's `asciiLowerCase` runs `/[A-Z]/g` over a tag or attribute name, the first
 *  time it meets that name, and property-information's `find` runs it over a `data` property's
 *  name. An expression that reads a body again has to run over a stretch of that body, and a
 *  one-pass row's stretch grows with the row's `n`. */
export function expressionsRunBy(read: () => void, shortest = 0): Set<string> {
  const run = new Set<string>();
  const exec = RegExp.prototype.exec;
  RegExp.prototype.exec = function recordingExec(this: RegExp, text: string) {
    if (String(text).length >= shortest) run.add(expressionKey(this));
    return exec.call(this, text);
  };
  try {
    read();
  } finally {
    RegExp.prototype.exec = exec;
  }
  return run;
}
