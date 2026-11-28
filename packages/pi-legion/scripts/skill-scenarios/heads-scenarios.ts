import { catalogLines } from "./catalog-file";

/** The heads scenarios' worlds, each line of heads-scenarios.txt one, `<scenario> [<case>]`, in the
 * order a batch runs them: the one catalog that rig.sh, heads-world.ts, seed.ts, score.ts and
 * hidden-proof.ts read. A world with no case is its scenario's own, whose run names none. */
const WORLDS = catalogLines(
  new URL("./heads-scenarios.txt", import.meta.url),
  2,
  "<scenario> [<case>]"
).map(([scenario = "", which = ""]) => [scenario, which] as const);

/** The scenarios whose runs get the heads fixture and may hide Dispatch's computed state. */
export const HEADS_SCENARIOS = Object.freeze([...new Set(WORLDS.map(([scenario]) => scenario))]);

/** Whether `scenario` gets the heads fixture. */
export function isHeadsScenario(scenario: string): boolean {
  return HEADS_SCENARIOS.includes(scenario);
}

/** The case of each world of `scenario`, in the catalog's order: "" for the scenario's own world.
 * None for a scenario the catalog does not list. */
export function headsCases(scenario: string): readonly string[] {
  return WORLDS.filter(([known]) => known === scenario).map(([, which]) => which);
}
