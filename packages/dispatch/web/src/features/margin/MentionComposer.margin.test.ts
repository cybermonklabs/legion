import { expect, test } from "bun:test";
import { canSubmitComposer, hasUnsavedInput } from "../conversation/MentionComposer";

test("composer holds Save until every image upload settles", () => {
  // A draft nothing refuses (`draftRefusal` returned no reason) still waits on its upload.
  expect(canSubmitComposer(undefined, false, 1)).toBe(false);
  expect(canSubmitComposer(undefined, false, 0)).toBe(true);
});

test("hasUnsavedInput counts a suggestion's optional reason even with no replacement typed", () => {
  expect(hasUnsavedInput("because", "")).toBe(true);
});

test("hasUnsavedInput is false with every field blank", () => {
  expect(hasUnsavedInput("", "")).toBe(false);
  expect(hasUnsavedInput("   ", "  ")).toBe(false);
});
test("hasUnsavedInput counts typed option labels and descriptions", () => {
  expect(hasUnsavedInput("", "", [{ description: "", label: "Ship" }])).toBe(true);
  expect(hasUnsavedInput("", "", [{ description: "Explain why", label: "" }])).toBe(true);
});
