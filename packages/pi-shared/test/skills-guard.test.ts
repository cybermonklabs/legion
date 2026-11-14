// The heading parse behind `anchors`, which decides whether a `skill://<name>/<path>#anchor` link
// resolves. It stands in for `/^#{1,6}\s+(.+?)\s*#*$/`, an expression quadratic in a line's
// trailing whitespace (dispatch://INFRA-1639), and each row below is that expression's own answer,
// oddities included: it is the meaning every skill's anchors were written against.
import { expect, test } from "bun:test";
import { anchors, headingText } from "./skills-guard";

const headings: readonly (readonly [line: string, text: string | undefined])[] = [
  ["# One", "One"],
  ["###### six", "six"],
  ["##\ttab", "tab"],
  // Seven hashes is no heading, and neither is a hash with no whitespace after it.
  ["####### seven", undefined],
  ["#no space", undefined],
  ["   ## indented", undefined],
  // The text must be a character, so a line of hashes and whitespace alone is no heading until
  // there is a character for the group to take: `##   ` gives it a space, whose anchor is `-`.
  ["##", undefined],
  ["######", undefined],
  ["## ", undefined],
  ["##   ", " "],
  ["## ###", "#"],
  // Trailing hashes are dropped with the whitespace before them, but only where they end the
  // line: after `##  ` stands whitespace, so the hashes are part of the text.
  ["## a##", "a"],
  ["## a ##", "a"],
  ["## a  ##  ", "a  ##"],
  ["## a#b", "a#b"],
  // A file read with CRLF endings and split on "\n" leaves the carriage return on the line, where
  // it is whitespace to drop; a carriage return inside the text is no heading, since `.` matches
  // no line terminator.
  ["## a\r", "a"],
  ["## a\rb", undefined],
];

test.each(headings)("the heading text of %j is %j", (line, text) => {
  expect(headingText(line)).toBe(text);
});

test("an anchor is the heading lower-cased, its punctuation dropped and its spaces hyphens", () => {
  expect([...anchors(["# One", "## a  ##  ", "## 💡 Tip ##", "#no space"].join("\n"))]).toEqual([
    "one",
    "a--",
    "-tip",
  ]);
});

// A heading line long enough to matter: the old expression took two minutes over this one, and a
// parse that reads it a bounded number of times takes about a millisecond. The bound is Bun's own
// per-test timeout rather than a wall-clock assertion of our own, which a loaded runner would
// make flaky: a parse quadratic in the trailing whitespace cannot finish inside it.
test("a heading line of 200,000 trailing spaces is parsed, not scanned once per character", () => {
  const spaces = " ".repeat(200_000);
  expect(headingText(`## a${spaces}x`)).toBe(`a${spaces}x`);
  expect(headingText(`## a${spaces}###`)).toBe("a");
  expect(headingText(`##${spaces}`)).toBe(" ");
  // The control the measurement used: one hash, so `\s+` takes the whole run in one pass.
  expect(headingText(`#${spaces}x`)).toBe("x");
});
