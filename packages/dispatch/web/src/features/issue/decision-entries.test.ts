import { expect, test } from "bun:test";

import { decisionBlockId } from "../issue-page/decision-review";
import { isDecisionEntry } from "./decision-entries";

test("an agent's decision and an answered ask are decisions, and an open ask is none", () => {
  expect(isDecisionEntry({ answer_state: "open", kind: "person" })).toBe(false);
  expect(isDecisionEntry({ answer_state: null, kind: "agent" })).toBe(true);
  expect(isDecisionEntry({ answer_state: "answered", kind: "person" })).toBe(true);
});

// The tree read names a document only on a typed block of a role document, so the entry of a
// decision of any other document names none: its own address names the block a Keep is written
// against.
test("a decision the read names no document for names its block by its own address", () => {
  expect(
    decisionBlockId({ document: null, ref: "dispatch://CORE-1/artifact/decisions-md#b-h-store" })
  ).toBe("h-store");
});
