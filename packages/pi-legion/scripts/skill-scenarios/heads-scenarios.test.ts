import { expect, test } from "bun:test";
import { HEADS_SCENARIOS, isHeadsScenario } from "./heads-scenarios";

test("the heads scenarios have one canonical membership", () => {
  expect(HEADS_SCENARIOS).toEqual([
    "reviewer-own-head",
    "reviewer-pass-and-fail",
    "architect-pass-and-fail",
    "ci-pass-and-fail",
  ]);
  expect(isHeadsScenario("reviewer-own-head")).toBe(true);
  expect(isHeadsScenario("measure-before-ask")).toBe(false);
});
