import { expect, test } from "bun:test";
import { rowScenario, runFields } from "./run-names";

// Catches: a reader of run names that knows some scenarios' cases and not others', that takes a
// case's prefix for the case (`nochange` of `nochange-block`), or that reads a label with a hyphen
// as two fields.
test("runFields reads each scenario's name, its case when it carries one, and a label with hyphens", () => {
  expect(runFields("tester-proof-ablate-head-12")).toEqual({
    scenario: "tester-proof",
    case: "",
    label: "ablate-head",
    n: 12,
  });
  expect(runFields("askgate-nochange-block-on-1")).toEqual({
    scenario: "askgate",
    case: "nochange-block",
    label: "on",
    n: 1,
  });
  expect(runFields("reviewer-own-head-no-tester-head-hidden-2")).toEqual({
    scenario: "reviewer-own-head",
    case: "no-tester",
    label: "head-hidden",
    n: 2,
  });
  expect(runFields("ci-pass-and-fail-architect-head-1")).toEqual({
    scenario: "ci-pass-and-fail",
    case: "architect",
    label: "head",
    n: 1,
  });
  expect(runFields("owner-walk-root-B-claude-ctl-d-3")).toEqual({
    scenario: "owner-walk",
    case: "root-B-claude",
    label: "ctl-d",
    n: 3,
  });
});

// Catches: a name of a scenario of cases that names none read as a run of the scenario
// `<scenario>-<label>` (score.ts then skips it with nothing said), and a name of no scenario read
// as one.
test("runFields says a cased scenario's name names no case, and reads no unknown scenario", () => {
  expect(runFields("reviewer-own-head-head-1")).toEqual({
    scenario: "reviewer-own-head",
    case: undefined,
    label: "head",
    n: 1,
  });
  expect(runFields("owner-walk-root-Z-claude-head-1")?.case).toBeUndefined();
  expect(runFields("frobnicate-head-1")).toBeUndefined();
  expect(runFields("tester-proof-head")).toBeUndefined();
  expect(runFields("askgate-facts--1")).toEqual({
    scenario: "askgate",
    case: undefined,
    label: "facts-",
    n: 1,
  });
});

// Catches: a row counted under one scenario and its rig errors under another.
test("rowScenario counts a run with a case under <scenario>:<case>, and one with none under its scenario", () => {
  const fields = (name: string) => {
    const read = runFields(name);
    if (read === undefined) throw new Error(`${name} is no run name`);
    return read;
  };
  expect(rowScenario(fields("ci-pass-and-fail-tester-head-1"))).toBe("ci-pass-and-fail:tester");
  expect(rowScenario(fields("askgate-facts-on-1"))).toBe("askgate:facts");
  expect(rowScenario(fields("reviewer-pass-and-fail-head-1"))).toBe("reviewer-pass-and-fail");
  expect(rowScenario(fields("reviewer-own-head-head-1"))).toBe("reviewer-own-head");
});
