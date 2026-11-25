import { expect, test } from "bun:test";
import { readerStep } from "@legion/contracts";
import { checkSync } from "recheck/lib/browser";

import { expectOnePass } from "../../__tests__/one-pass";
import { expressionsRunBy, readerExpressions } from "../../__tests__/reader-expressions";

/**
 * Whether `expression` can take more than one pass over a text, decided from the expression as
 * written rather than from a run of it: a regular expression that backtracks does it inside one
 * call, where neither a clock nor a count of calls can see it. `recheck`'s automaton checker
 * proves the answer from the expression's automaton. This is its pure JavaScript build
 * (`recheck/lib/browser`), which runs in this process: no worker, no Java and no native binary.
 * Its own deadline is off (`timeout: null`), so a loaded machine slows the answer and never
 * changes it. The check reads only the expression's source and flags, so a `RegExp` and an
 * expression given as those two alone are read the same.
 */
function verdict(expression: { readonly flags: string; readonly source: string }) {
  return checkSync(expression.source, expression.flags, { checker: "automaton", timeout: null });
}

// Every expression the one-pass rows' reads run over a text at least as long as their floor
// (`__tests__/reader-expressions.ts`), which `expectOnePass` holds each one-pass row's reader to
// (`Unfurl.test.tsx`, `party-references.test.ts`, `text-references.test.ts`).
test.each(
  readerExpressions
)("the expression of %s reads a text in one pass", (_name, expression) => {
  expect(verdict(expression)).toMatchObject({ checker: "automaton", status: "safe" });
});

// The control of the rows above: the expressions those readers replaced, which the same check
// calls vulnerable. The unfurl cards once matched a body against the first, which splits a body
// of repeated schemes every way it can and ran for minutes in V8; the trim once cut a
// reference's closing run with the second, and a mention's trailing `_` and `-` with the third
// (`trimmedMention`), each of which reads a run again from every position in it. Each is given as
// its source and flags, not as a `RegExp`: nothing here runs one, and CodeQL's js/redos reports a
// literal of the first wherever it stands, run or not.
test.each([
  [
    "a body of references",
    { flags: "", source: String.raw`^(?:(?:dispatch:\/\/|https?:\/\/)\S+\s*)+$` },
    { type: "exponential" },
  ],
  [
    "the end of a reference",
    { flags: "", source: "[),.;:!?]+$" },
    { degree: 2, type: "polynomial" },
  ],
  ["the end of a mention", { flags: "", source: "[_-]+$" }, { degree: 2, type: "polynomial" }],
] as const)("the check calls the old expression for %s vulnerable", (_name, expression, complexity) => {
  expect(verdict(expression)).toMatchObject({
    checker: "automaton",
    complexity,
    status: "vulnerable",
  });
});

// What holds a reader to the list sees every way a string or an expression runs a match, so a
// reader cannot run an expression past it by calling another method.
test.each([
  ["exec", (text: string) => /b+$/.exec(text)],
  ["test", (text: string) => /b+$/.test(text)],
  ["match", (text: string) => text.match(/b+$/)],
  ["matchAll", (text: string) => [...text.matchAll(/b+$/g)]],
  ["replace", (text: string) => text.replace(/b+$/, "")],
  ["replaceAll", (text: string) => text.replaceAll(/b+$/g, "")],
  ["search", (text: string) => text.search(/b+$/)],
  ["split", (text: string) => text.split(/b+$/)],
] as const)("an expression run by %s is recorded", (_name, run) => {
  expect([...expressionsRunBy(() => run("abb"))]).toEqual(["/b+$/"]);
});

// The floor a one-pass row records from (`n` characters, where `n` is the row's own count of the
// units its text repeats, so a quarter of its longer read's text or less): an expression run
// over a shorter text, such as the `/[A-Z]/g` happy-dom runs over a tag name it meets for the
// first time, is no reading of the body and is left out; one run over a text as long as the
// floor is recorded.
test("an expression run over a text shorter than the floor is not recorded, and one at it is", () => {
  expect([...expressionsRunBy(() => /b+$/.test("abb"), 4)]).toEqual([]);
  expect([...expressionsRunBy(() => /b+$/.test("abbb"), 4)]).toEqual(["/b+$/"]);
});

// The control of the one-pass rows' list: a reader that counts its steps in one pass but trims
// with the old mention expression, which no step count sees, fails the row and names it.
test("a reader that runs an expression the list does not hold fails its one-pass row", () => {
  const trimmed = (n: number) => {
    for (let index = 0; index < n; index += 1) readerStep();
    return `mention${"_".repeat(n)}`.replace(/[_-]+$/, "");
  };
  expect(() => expectOnePass(trimmed, 1 << 10)).toThrow("/[_-]+$/");
});
