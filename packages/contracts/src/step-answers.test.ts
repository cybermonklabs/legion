import { expect, test } from "bun:test";

import { answeredText, type StepRecordModels, setModelText } from "./step-answers";

const ENDED = "2026-11-16T09:40:00Z";

// The five records the route's test writes and reads back: one with only the fields a record held
// before the set model and the answers, one still running, and three set to the model A, one with
// no counts, one that A answered 41 times and B 3 times, and one with no call.
const fiveRecords: readonly StepRecordModels[] = [
  { ended_at: ENDED, set_model: "", answers: null },
  { ended_at: null, set_model: "", answers: null },
  { ended_at: ENDED, set_model: "A", answers: null },
  {
    ended_at: ENDED,
    set_model: "A",
    answers: [
      { model: "A", calls: 41 },
      { model: "B", calls: 3 },
    ],
  },
  { ended_at: ENDED, set_model: "A", answers: [] },
];

test("answeredText reads the five records as running, unknown, none or each model's calls", () => {
  expect(fiveRecords.map((record) => answeredText(record))).toEqual([
    "unknown",
    "running",
    "unknown",
    "A=41(93%),B=3(7%)",
    "none",
  ]);
});

test("setModelText names the set model of the five records, and null where a record holds none", () => {
  expect(fiveRecords.map((record) => setModelText(record))).toEqual([null, null, "A", "A", "A"]);
});

// A Dispatch older than the set model and the answers sends neither key. Such a record does not
// know its counts, which is not a step with no call.
test("a record with no answers key reads unknown, and one with no set_model key names none", () => {
  expect(answeredText({ ended_at: ENDED })).toBe("unknown");
  expect(setModelText({ ended_at: ENDED })).toBeNull();
});

// The daemon writes the answers most calls first; the text keeps the record's order whatever it
// is, and rounds each share alone, so three equal shares read 33% each.
test("answeredText keeps the record's order and rounds each share to a whole percent", () => {
  const ended = (answers: StepRecordModels["answers"]) =>
    answeredText({ ended_at: ENDED, answers });

  expect(
    ended([
      { model: "B", calls: 3 },
      { model: "A", calls: 41 },
    ])
  ).toBe("B=3(7%),A=41(93%)");
  expect(
    ended([
      { model: "A", calls: 1 },
      { model: "B", calls: 1 },
      { model: "C", calls: 1 },
    ])
  ).toBe("A=1(33%),B=1(33%),C=1(33%)");
  expect(ended([{ model: "anthropic/claude-opus-5-5", calls: 2 }])).toBe(
    "anthropic/claude-opus-5-5=2(100%)"
  );
});

// Only the end writes the answers, so a run that has not ended reads as running whatever it holds.
test("a run that has not ended reads running, whatever answers it holds", () => {
  expect(
    answeredText({ ended_at: null, set_model: "A", answers: [{ model: "A", calls: 1 }] })
  ).toBe("running");
});
