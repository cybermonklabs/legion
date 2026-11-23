import { expect, test } from "bun:test";

import { isQuestionShapedAnswer } from "./question-shaped-answer";

test("detects punctuation- and interrogative-led clarification requests", () => {
  for (const answer of [
    "Maybe?",
    "   Who owns this release",
    "Can we ship after the audit",
    "Does the artifact already exist",
  ]) {
    expect(isQuestionShapedAnswer(answer), answer).toBe(true);
  }
});

test("does not treat declarative Other responses as clarification requests", () => {
  for (const answer of ["Ship after the audit.", "However, I would wait.", "This is approved."]) {
    expect(isQuestionShapedAnswer(answer), answer).toBe(false);
  }
});

test("reads the reader's own lines, never a quote's", () => {
  // The quote is the asker's question; the line the reader typed is no question.
  expect(
    isQuestionShapedAnswer("Flip. One thing I can't read here:\n\n> go with B per the train?\n\n")
  ).toBe(false);
  // A quote alone is nothing the reader wrote, and a quoted line that opens with a question word
  // is not the reader asking.
  expect(isQuestionShapedAnswer("> Which service moves first?\n\n")).toBe(false);
  expect(isQuestionShapedAnswer("> why not both\n\nBecause the mirror reads the ledger.")).toBe(
    false
  );
  // What the reader asks under a quote is still a question.
  expect(isQuestionShapedAnswer("> go with B per the train?\n\nWhat is the train?")).toBe(true);
  expect(isQuestionShapedAnswer("> The mirror goes first.\n\nwhy")).toBe(true);
});
