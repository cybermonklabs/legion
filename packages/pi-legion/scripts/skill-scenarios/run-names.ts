// run-names.ts: the grammar of a run's name, the one place every reader of a run's name takes it
// from: score.ts, visible-world.ts and owner-walk.ts. rig.sh's cmd_run names each
// run `<scenario>[-<case>]-<label>-<n>`, and its link in the map carries that name (rig.sh's
// header, "the map"). A scenario, a case and a label may each hold a hyphen, so a name is read
// from the front: the longest scenario it starts with, then, for a scenario of RUN_CASES, the
// longest of its cases, and the rest up to the number is the label. The cases come from the
// catalogs rig.sh reads too: askgate-cases.txt and heads-scenarios.txt.
import { ASKGATE_CASES } from "./askgate-cases";
import { HEADS_SCENARIOS, headsCases } from "./heads-scenarios";
import { FAMILIES, OWNER_WALK_FORMS, OWNER_WALK_KINDS } from "./scored-runs-contracts";

/** Every scenario score.ts scores from a batch (`rig.sh batch`, rig_scenario's list). */
export const BATCH_SCENARIOS = [
  "ask-on-message",
  "measure-before-ask",
  "tester-proof",
  "brainstorm",
  "askgate",
  "spec-writing",
  "spec-rewrite",
  "reviewer-own-head",
  "reviewer-pass-and-fail",
  "architect-pass-and-fail",
  "ci-pass-and-fail",
  "tester-reads-no-ci",
] as const;
export type BatchScenario = (typeof BATCH_SCENARIOS)[number];
/** Whether `scenario` is one score.ts scores from a batch. */
export function isBatchScenario(scenario: string): scenario is BatchScenario {
  return (BATCH_SCENARIOS as readonly string[]).includes(scenario);
}

/** The scored scenarios (`rig.sh batch-scored`): score.ts scores each run of one alone (`score.ts
 * runs <runs dir> --run <name>`), through scored-runs.ts. */
export const SCORED_SCENARIOS: readonly string[] = ["owner-walk"];

/** The cases of owner-walk, `<kind>-<form>-<family>` (owner-walk.ts parseOwnerWalkCase). */
const OWNER_WALK_CASES = OWNER_WALK_KINDS.flatMap((kind) =>
  OWNER_WALK_FORMS.flatMap((form) => FAMILIES.map((family) => `${kind}-${form}-${family}`))
);

/** The scenarios whose run's name carries a case before its label, each with its cases: askgate's
 * (askgate-cases.txt), each heads scenario's whose catalog worlds name one (heads-scenarios.txt,
 * with "" for the scenario's own world, whose run names none), and owner-walk's. */
export const RUN_CASES: Readonly<Record<string, readonly string[]>> = {
  askgate: ASKGATE_CASES,
  ...Object.fromEntries(
    HEADS_SCENARIOS.flatMap((scenario) => {
      const cases = headsCases(scenario);
      return cases.some((which) => which !== "") ? [[scenario, cases]] : [];
    })
  ),
  "owner-walk": OWNER_WALK_CASES,
};

/** A run's name read into its fields. */
export interface RunFields {
  readonly scenario: string;
  /** Its case: one of RUN_CASES[scenario], "" for a scenario whose runs carry none and for a heads
   * scenario's own world, and undefined for a name of a scenario of RUN_CASES that names none of
   * its cases and has no own world. */
  readonly case: string | undefined;
  readonly label: string;
  readonly n: number;
}

const longestFirst = (names: readonly string[]) => names.toSorted((a, b) => b.length - a.length);
const SCENARIOS_LONGEST_FIRST = longestFirst([...BATCH_SCENARIOS, ...SCORED_SCENARIOS]);

/** The fields of a run's name, `<scenario>[-<case>]-<label>-<n>`, or undefined for a name of no
 * scenario rig.sh runs, or one with no label or no number. A label that starts like a case's tail
 * is read wrong, as rig.sh's names cannot tell them apart either. */
export function runFields(name: string): RunFields | undefined {
  const numbered = /^(.+)-(\d+)$/.exec(name);
  if (numbered === null) return undefined;
  const [, stem = "", number = ""] = numbered;
  const n = Number(number);
  const scenario = SCENARIOS_LONGEST_FIRST.find((known) => stem.startsWith(`${known}-`));
  if (scenario === undefined) return undefined;
  const rest = stem.slice(scenario.length + 1);
  const cases = RUN_CASES[scenario];
  if (cases === undefined) return { scenario, case: "", label: rest, n };
  const runCase = longestFirst(cases.filter((known) => known !== "")).find(
    (known) => rest.startsWith(`${known}-`) && rest.length > known.length + 1
  );
  if (runCase !== undefined)
    return { scenario, case: runCase, label: rest.slice(runCase.length + 1), n };
  return { scenario, case: cases.includes("") ? "" : undefined, label: rest, n };
}

/** The scenario a run's row is counted under: `<scenario>:<case>` for a run with a case, so the
 * counts are per case and label, and the scenario alone for one with none. */
export function rowScenario(fields: RunFields): string {
  return fields.case ? `${fields.scenario}:${fields.case}` : fields.scenario;
}
