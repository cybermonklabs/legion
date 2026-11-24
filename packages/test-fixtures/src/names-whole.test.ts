import { expect, test } from "bun:test";
import { namesMissing, namesWhole } from "./names-whole";

test("a name is found as a whole token and never inside a longer name", () => {
  const text = "closed carries merged, merge_commit_sha and merged_by; see dispatch issue-update.";

  expect(namesWhole(text, "merged")).toBe(true);
  expect(namesWhole(text, "merge_commit_sha")).toBe(true);
  expect(namesWhole("only merged_by here", "merged")).toBe(false);
  expect(namesWhole("only unmerged here", "merged")).toBe(false);
  expect(namesWhole(text, "dispatch issue")).toBe(false);
  expect(namesWhole(`${text} Then dispatch issue.`, "dispatch issue")).toBe(true);
});

test("a name that begins or ends with punctuation is bounded by that punctuation", () => {
  const text = "keep --issue whenever; prints `Text:` and {#id}, at dispatch://KEY/spec.";

  expect(namesWhole(text, "--issue")).toBe(true);
  expect(namesWhole(text, "`Text:`")).toBe(true);
  expect(namesWhole(text, "{#id}")).toBe(true);
  expect(namesWhole(text, "dispatch://")).toBe(true);
  expect(namesWhole("keep --issues", "--issue")).toBe(false);
});

// A name is matched as its own text: a character a pattern reads as syntax is escaped first, so a
// name holding one is found only where the text holds that character.
test("a | in a name matches only a |, never a choice between two names", () => {
  expect(namesWhole("pass|fail only", "pass|fail")).toBe(true);
  expect(namesWhole("pass only", "pass|fail")).toBe(false);
});

test("a . in a name matches only a ., never any character", () => {
  expect(namesWhole("<subject>.>", "<subject>.>")).toBe(true);
  expect(namesWhole("<subject>x>", "<subject>.>")).toBe(false);
});

test("namesMissing lists each name the text does not hold, in order", () => {
  expect(namesMissing("backlog and icebox", ["backlog", "triage", "icebox", "todo"])).toEqual([
    "triage",
    "todo",
  ]);
  expect(namesMissing("backlog and icebox", ["icebox", "backlog"])).toEqual([]);
});
