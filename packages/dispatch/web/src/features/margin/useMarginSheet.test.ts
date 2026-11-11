import { expect, test } from "bun:test";

import type { AskResolution } from "../../api/types";
import { isFoldedAsk } from "./useMarginSheet";

const retracted: AskResolution = {
  actor: { id: "0123456789abcdef", kind: "session" },
  at: "2026-10-25T00:00:00Z",
  kind: "retracted",
  reason: "Duplicate.",
};
const withdrawn: AskResolution = {
  actor: { id: "alice", kind: "user" },
  at: "2026-10-25T00:00:00Z",
  kind: "withdrawn",
  reason: "Withdrawn by the close of CORE-1.",
};

/** A decline the person wrote no note with: the reason is empty, and Dispatch's own words sit
 *  apart from it. */
const declined: AskResolution = {
  actor: { id: "alice", kind: "user" },
  at: "2026-10-25T00:00:00Z",
  dispatch_words: "The person chose not to answer this question.",
  kind: "declined",
  reason: "",
};

test("the margin folds a withdrawn ask away like a retracted one, and never an open one", () => {
  expect(isFoldedAsk({ resolution: retracted, state: "resolved" })).toBe(true);
  expect(isFoldedAsk({ resolution: withdrawn, state: "resolved" })).toBe(true);
  expect(isFoldedAsk({ resolution: undefined, state: "open" })).toBe(false);
  // A resolution with an answer found is a decision to read, not withdrawn history.
  expect(
    isFoldedAsk({
      resolution: { ...retracted, kind: "resolved", reason: "Done." },
      state: "resolved",
    })
  ).toBe(false);
});

// A declined question is no decision: among the answered ones it would read as a decision
// nobody made.
test("the margin folds a declined ask away with the retracted and the withdrawn", () => {
  expect(isFoldedAsk({ resolution: declined, state: "resolved" })).toBe(true);
});
