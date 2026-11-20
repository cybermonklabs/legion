import { readFileSync } from "node:fs";

/** The scenarios whose runs get the heads fixture and may hide Dispatch's computed state. */
export const HEADS_SCENARIOS = Object.freeze(
  readFileSync(new URL("./heads-scenarios.txt", import.meta.url), "utf8")
    .split("\n")
    .filter(Boolean)
);

/** Whether `scenario` gets the heads fixture. */
export function isHeadsScenario(scenario: string): boolean {
  return HEADS_SCENARIOS.includes(scenario);
}
