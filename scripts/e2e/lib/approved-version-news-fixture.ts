import { join } from "node:path";

// What approved-version-news-verdicts.test.ts and approved-version-news-runs.test.ts both read
// and write of approved-version-news.sh: the script, part one's driver test, the failure its
// negative control bites on, part two's steps, the plan part two's driver prints of them and its
// run lines.

export const script = join(import.meta.dir, "..", "approved-version-news.sh");

export const driverTest = "TestAnApprovedVersionReachesItsHolderLive";

/** The line part one's driver prints when STEP's one-approval check fails on a daemon from before
 * the change, as awaitQueued writes it: the failure the negative control bites on
 * (control_failure). */
export const controlFailure = (step: string) =>
  `FAIL ${step}: waited 2m0s for a task queued for the reviewer of AVQ1-1 behind its open turn; last seen: its claim holds no task`;

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
// The driver's plan of a full run at this change: each step in the head arm, the one arm, three
// runs each.
export const fullPlan = plan(
  [
    ["a4", a4, "head"],
    ["off", off, "head"],
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
