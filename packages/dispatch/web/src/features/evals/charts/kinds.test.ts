import { expect, test } from "bun:test";
import { chartKinds } from "./registry";

/** Every kind of declared chart the validator accepts (`internal/dispatch/evals/charts`, a port of
 *  the eval platform's `validate_charts`): its 15 kinds but `module`, which Dispatch refuses
 *  (dispatch://INFRA-2918/artifact/plan-md, D10 and decision 5a). */
const ACCEPTED_KINDS = [
  "xy",
  "band",
  "bar",
  "interval",
  "stacked-bar",
  "grid",
  "lanes",
  "network",
  "architecture",
  "number",
  "pending",
  "runs",
  "text",
  "prose",
] as const;

/** The registry the tab draws a declared chart through: each kind's pure model function (the spec
 *  and its readings to the chart's model) and its renderer, by kind (plan T1, T20). */
test("every accepted kind has a model and a renderer", () => {
  expect(ACCEPTED_KINDS).toHaveLength(14);
  expect(Object.keys(chartKinds).sort()).toEqual([...ACCEPTED_KINDS].sort());
  for (const kind of ACCEPTED_KINDS) {
    expect([kind, typeof chartKinds[kind]?.model]).toEqual([kind, "function"]);
    expect([kind, typeof chartKinds[kind]?.renderer]).toEqual([kind, "function"]);
  }
  expect(Object.hasOwn(chartKinds, "module")).toBe(false);
});
