import { join } from "node:path";

// What approved-version-news-verdicts.test.ts and approved-version-news-runs.test.ts both read
// and write of approved-version-news.sh: the script, part one's driver test, part two's steps, the
// plan part two's driver prints of them, its run lines, and the measurements that retired a base
// arm.

export const script = join(import.meta.dir, "..", "approved-version-news.sh");

export const driverTest = "TestAnApprovedVersionReachesItsHolderLive";

export const a4 = "a4-the-architect-tells-the-child-reviewer";
export const off = "off-the-architect-tells-the-holders";
export const answers = "answers-a-stale-reviewer-moves-back-a-current-one-comments";

/** A plan as part two's driver prints it (sessionsPick.plan): a `plan step` line for each step it
 * makes, its word, its name and the arms it makes the step in, then `plan runs`. */
export const plan = (steps: [word: string, step: string, arms: string][], runs: number) =>
  [
    ...steps.map(([word, step, arms]) => `plan step ${word} ${step} ${arms}`),
    `plan runs ${runs}`,
  ].join("\n");
// The driver's plan of a full run at this change: a4 and answers in the head arm alone, their base
// arms retired, and off in both, three runs of each arm.
export const fullPlan = plan(
  [
    ["a4", a4, "head"],
    ["off", off, "head base"],
    ["answers", answers, "head"],
  ],
  3
);
// One run's line as part two's driver prints it at the run's end.
export const runLine = (step: string, arm: string, n: number, outcome: string) =>
  `run ${step} ${arm} ${n}: ${outcome} in 140s, 31 model calls`;
/** Each run line of STEP in ARM, numbered from 1, one for each outcome. */
export const runsOf = (step: string, arm: string, ...outcomes: string[]) =>
  outcomes.map((outcome, i) => runLine(step, arm, i + 1, outcome));
export const passed = "PASS";
export const failed =
  "FAIL: the reviewer of AS1-2 was sent no Envoy message naming version 2 and the change";
// Each step whose base arm is retired, its word, and what the last line says in its place, the
// measurement that retired it.
export const retired = [
  {
    step: a4,
    word: "a4",
    headOnly: "(head only: the base arm passed 10 of 10 at 578c6abb, main meets R4 since #4203)",
  },
  {
    step: answers,
    word: "answers",
    headOnly:
      "(head only: the base arm passed 10 of 10 at 578c6abb, the daemon's news task states the rule in both arms)",
  },
];
