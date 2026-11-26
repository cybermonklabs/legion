import { expect, test } from "bun:test";
import {
  type Arm,
  type CaseFacts,
  confirmScore,
  controlOrder,
  descriptionFault,
  FURTHER_ROUND_LIMIT,
  isControlLoss,
  isFalseLoss,
  isFlagged,
  isInformative,
  isLoss,
  isPooledLoss,
  LISTING_MEASURE_ITEM,
  type LoadTask,
  listingStanding,
  type MadeConfirm,
  outsideListing,
  outsideListingWords,
  poorName,
  RunRecord,
  rankSkills,
  type SkillOutcome,
  type Stage,
  scoreSkill,
  screeningDone,
  screeningRefusal,
  servedScreeningRefusal,
  skillVerdict,
  stageOutcome,
  taskFault,
  walk,
} from "./score";

// The rules of the behaviour test (score.ts's header), the load test's fault rule and the name
// test's poor-name rule, one row on each side of each boundary. Every row is built here: this
// file reads no fixture. A
// row is a name, the call it checks and the value that call must give.

/** `total` run records of one case, stage and arm: `passes` passes, the rest fails. */
function runs(caseId: string, stage: Stage, arm: Arm, passes: number, total: number): RunRecord[] {
  return Array.from({ length: total }, (_, n) => ({
    caseId,
    stage,
    arm,
    verdict: n < passes ? "pass" : "fail",
    reason: "judged",
  }));
}
/** `count` run records that got no judged verdict: an error, a timeout or an unparsed judgement. */
function unjudged(caseId: string, stage: Stage, arm: Arm, count: number): RunRecord[] {
  return Array.from({ length: count }, () => ({
    caseId,
    stage,
    arm,
    verdict: null,
    reason: "timed out",
  }));
}
/** A run of `arm` that waits at continue-mixed's further-round limit for the next decision. */
function waiting(caseId: string, stage: Stage, arm: Arm): RunRecord {
  return { caseId, stage, arm, verdict: null, reason: `${FURTHER_ROUND_LIMIT}: planted` };
}
/** A case's 10 screen runs in each arm, with these passes. */
const screen = (caseId: string, old: number, cut: number, none: number): RunRecord[] => [
  ...runs(caseId, "screen", "old", old, 10),
  ...runs(caseId, "screen", "new", cut, 10),
  ...runs(caseId, "screen", "none", none, 10),
];
/** A case's 40 fresh runs of `old` and of `other` (`new` to confirm, `minus` for the control), and
 *  for the control the false-loss control's `twin` too, passing `twin` (old's passes unless
 *  given). */
const fresh = (
  caseId: string,
  stage: "confirm" | "control",
  old: number,
  other: number,
  twin = old
): RunRecord[] => [
  ...runs(caseId, stage, "old", old, 40),
  ...runs(caseId, stage, stage === "confirm" ? "new" : "minus", other, 40),
  ...(stage === "control" ? runs(caseId, stage, "twin", twin, 40) : []),
];
/** Six informative cases (old 8, none 5 of 10), none flagged (new 8), and the control's runs on
 * c1, the lowest id of the six equal gaps: old 30, minus `minus` and twin `twin` (30) of 40. */
const sixCases = (minus: number, twin = 30): RunRecord[] => [
  ...["c1", "c2", "c3", "c4", "c5", "c6"].flatMap((id) => screen(id, 8, 8, 5)),
  ...fresh("c1", "control", 30, minus, twin),
];
/** The facts of cases `ids`, each dropped at `stage` for a made-up unreadable directory. */
const droppedAt = (stage: Stage, ...ids: string[]): Map<string, CaseFacts> =>
  new Map(
    ids.map((id) => [id, { drop: { stage, reason: `unreadable-directory: acme/skills ${id}` } }])
  );
/** One confirm made on `runner` at the made-up texts "t1 t2", counted: old and new passes of 40. */
const confirmOn = (runner: string, old: number, cut: number): MadeConfirm => ({
  runner,
  texts: "t1 t2",
  outcome: { state: "counted", passes: { old, new: cut } },
});
/** Confirms on the one runner `r1`, the old text passing 40 of 40 in each, the cut text `cuts`. */
const onOneRunner = (...cuts: number[]): MadeConfirm[] =>
  cuts.map((cut) => confirmOn("r1", 40, cut));
/** A case's evidence holding one listing measure as build.ts writes it: of `kind`, its recorded
 *  listing holding `recorded` names, of which the rebuilt one misses `missing`, and to which it
 *  adds `added`. */
const measured = (
  kind: "session listing" | "subagent listing" | "not measurable",
  recorded: number | null,
  missing: string[],
  added: string[] = []
) => [
  {
    item: LISTING_MEASURE_ITEM,
    source: JSON.stringify({
      kind,
      recorded,
      listed: 9,
      missing,
      added,
      missingShare: recorded === null || recorded === 0 ? null : missing.length / recorded,
      sessionDigest: null,
      rebuiltDigest: null,
      subagent: null,
      why: "the made-up session records no system prompt",
    }),
  },
];
/** A load test that passes: 5 need tasks, each loaded on 2 or 3 of 3 runs, and 5 near-miss tasks,
 * each loaded on at most 1; and the same test with its first need task loaded once. */
const passing: LoadTask[] = [
  ...[3, 2, 2, 3, 2].map((loads) => ({ label: "need" as const, loads })),
  ...[0, 1, 0, 1, 0].map((loads) => ({ label: "near-miss" as const, loads })),
];
const failing: LoadTask[] = [{ label: "need", loads: 1 }, ...passing.slice(1)];

const keeps: SkillOutcome = { verdict: "keeps" };
const loss: SkillOutcome = { verdict: "loss" };
const tooFew: SkillOutcome = { verdict: null, reason: "fewer than 6 informative cases: 5" };
const ranking = ["alpha", "beta", "gamma", "delta", "epsilon"];

const rows: [string, () => unknown, unknown][] = [
  // The run record: one JSON line per run.
  [
    "a run record with a null verdict parses",
    () =>
      RunRecord.safeParse({
        caseId: "c1",
        arm: "minus",
        stage: "control",
        verdict: null,
        reason: "judgement did not parse",
      }).success,
    true,
  ],
  [
    "a run record whose verdict is neither pass, fail nor null is refused",
    () =>
      RunRecord.safeParse({
        caseId: "c1",
        arm: "old",
        stage: "screen",
        verdict: "error",
        reason: "x",
      }).success,
    false,
  ],
  [
    "a run record with a key the shape does not name is refused",
    () =>
      RunRecord.safeParse({
        caseId: "c1",
        arm: "old",
        stage: "screen",
        verdict: "pass",
        reason: "x",
        skill: "alpha",
      }).success,
    false,
  ],

  // Judged runs: an error, a timeout or an unparsed judgement is no judged run.
  [
    "10 judged screen runs in each arm are counted",
    () => stageOutcome(screen("c1", 7, 6, 5), "c1", "screen"),
    { state: "counted", passes: { old: 7, new: 6, none: 5 } },
  ],
  [
    "a run with no judged verdict is no fail: 9 judged of 10 runs leave one run to start",
    () =>
      stageOutcome(
        [...screen("c1", 7, 6, 5).slice(1), ...unjudged("c1", "screen", "old", 1)],
        "c1",
        "screen"
      ),
    { state: "running", toStart: { old: 1, new: 0, none: 0 } },
  ],
  [
    "10 judged runs among 15 (5 extra runs) are counted",
    () =>
      stageOutcome(
        [...screen("c1", 7, 6, 5), ...unjudged("c1", "screen", "new", 5)],
        "c1",
        "screen"
      ),
    { state: "counted", passes: { old: 7, new: 6, none: 5 } },
  ],
  [
    "9 judged runs among 14 leave one extra run to start",
    () =>
      stageOutcome(
        [...screen("c1", 7, 6, 5).slice(1), ...unjudged("c1", "screen", "old", 5)],
        "c1",
        "screen"
      ),
    { state: "running", toStart: { old: 1, new: 0, none: 0 } },
  ],
  [
    "9 judged runs among 15 leave the case unscreened",
    () =>
      stageOutcome(
        [...screen("c1", 7, 6, 5).slice(1), ...unjudged("c1", "screen", "old", 6)],
        "c1",
        "screen"
      ),
    { state: "unscreened" },
  ],
  [
    "the confirm stage needs 40 judged runs in each arm",
    () => stageOutcome(fresh("c1", "confirm", 30, 20).slice(1), "c1", "confirm"),
    { state: "running", toStart: { old: 1, new: 0 } },
  ],
  [
    "40 judged control runs of old, minus and twin are counted",
    () => stageOutcome(fresh("c1", "control", 30, 20, 28), "c1", "control"),
    { state: "counted", passes: { old: 30, minus: 20, twin: 28 } },
  ],
  [
    "the control stage needs twin's 40 judged runs too",
    () =>
      stageOutcome(
        fresh("c1", "control", 30, 20).filter((run) => run.arm !== "twin"),
        "c1",
        "control"
      ),
    { state: "running", toStart: { old: 0, minus: 0, twin: 40 } },
  ],
  // Catches: a run that waits at continue-mixed's further-round limit replaced by a fresh run,
  // so the next round's decision finds its slot taken; or a stage counted while a run waits.
  [
    "a run that waits at the further-round limit holds its slot: 9 judged and 1 waiting start no run, and the stage stays running",
    () =>
      stageOutcome(
        [...screen("c1", 7, 6, 5).slice(1), waiting("c1", "screen", "old")],
        "c1",
        "screen"
      ),
    { state: "running", toStart: { old: 0, new: 0, none: 0 } },
  ],
  [
    "beside a waiting run, a run with no verdict is run again as ever",
    () =>
      stageOutcome(
        [
          ...screen("c1", 7, 6, 5).slice(2),
          waiting("c1", "screen", "old"),
          ...unjudged("c1", "screen", "old", 1),
        ],
        "c1",
        "screen"
      ),
    { state: "running", toStart: { old: 1, new: 0, none: 0 } },
  ],

  // informative: old passes - none passes >= 2, of 10 each.
  ["informative at 2 more passes than no skill", () => isInformative({ old: 7, none: 5 }), true],
  ["not informative at 1 more pass than no skill", () => isInformative({ old: 6, none: 5 }), false],

  // flagged: informative, and old - new >= 2, of 10 each.
  ["flagged at 2 fewer passes of 10", () => isFlagged({ old: 7, new: 5, none: 5 }), true],
  ["not flagged at 1 fewer pass of 10", () => isFlagged({ old: 7, new: 6, none: 5 }), false],
  ["not flagged when not informative", () => isFlagged({ old: 7, new: 5, none: 6 }), false],

  // loss: old - new >= 10, of 40 fresh runs each.
  ["a loss at 10 fewer passes of 40", () => isLoss({ old: 30, new: 20 }), true],
  ["no loss at 9 fewer passes of 40", () => isLoss({ old: 30, new: 21 }), false],

  // control loss: old - minus >= 10, of 40 fresh runs each.
  [
    "a control loss at 10 fewer passes of 40",
    () => isControlLoss({ old: 30, minus: 20, twin: 30 }),
    true,
  ],
  [
    "no control loss at 9 fewer passes of 40",
    () => isControlLoss({ old: 30, minus: 21, twin: 30 }),
    false,
  ],
  // false loss: old - twin >= 10, of 40 fresh runs each, the same text twice.
  [
    "a false loss at 10 fewer passes of 40 of the twin",
    () => isFalseLoss({ old: 30, minus: 20, twin: 20 }),
    true,
  ],
  [
    "no false loss at 9 fewer passes of 40 of the twin",
    () => isFalseLoss({ old: 30, minus: 20, twin: 21 }),
    false,
  ],
  [
    "no false loss when the twin passes more than old",
    () => isFalseLoss({ old: 20, minus: 10, twin: 35 }),
    false,
  ],

  // control case: the first of the control rule's order, the informative case with the largest
  // old - none gap; a tie to the lower id.
  [
    "the control case is the informative case with the largest gap",
    () =>
      controlOrder([
        { caseId: "a", passes: { old: 8, new: 8, none: 5 } },
        { caseId: "b", passes: { old: 9, new: 9, none: 5 } },
        { caseId: "c", passes: { old: 6, new: 6, none: 5 } },
      ])[0],
    "b",
  ],
  [
    "a tie for the largest gap goes to the lower case id",
    () =>
      controlOrder([
        { caseId: "b", passes: { old: 8, new: 8, none: 5 } },
        { caseId: "a", passes: { old: 9, new: 9, none: 6 } },
      ])[0],
    "a",
  ],
  [
    "the lower case id is the lower in string order",
    () =>
      controlOrder([
        { caseId: "c9", passes: { old: 8, new: 8, none: 5 } },
        { caseId: "c10", passes: { old: 8, new: 8, none: 5 } },
      ])[0],
    "c10",
  ],
  [
    "no informative case gives no control case",
    () => controlOrder([{ caseId: "a", passes: { old: 6, new: 6, none: 5 } }])[0],
    undefined,
  ],

  // skill verdict.
  [
    "6 informative cases and a control loss give a verdict",
    () => skillVerdict({ informative: 6, controlLoss: true, falseLoss: false, losses: 0 }),
    keeps,
  ],
  [
    "5 informative cases give no verdict",
    () => skillVerdict({ informative: 5, controlLoss: true, falseLoss: false, losses: 0 }),
    tooFew,
  ],
  [
    "a control that is no loss gives no verdict",
    () => skillVerdict({ informative: 6, controlLoss: false, falseLoss: false, losses: 1 }),
    { verdict: null, reason: "control not a loss" },
  ],
  [
    "a false-loss control that is a loss gives no verdict, whatever the confirmed cases say",
    () => skillVerdict({ informative: 6, controlLoss: true, falseLoss: true, losses: 1 }),
    { verdict: null, reason: "false-loss control a loss" },
  ],
  [
    "both controls failing give no verdict, and the reason names both",
    () => skillVerdict({ informative: 6, controlLoss: false, falseLoss: true, losses: 0 }),
    { verdict: null, reason: "control not a loss; false-loss control a loss" },
  ],
  [
    "one confirmed loss gives loss",
    () => skillVerdict({ informative: 6, controlLoss: true, falseLoss: false, losses: 1 }),
    loss,
  ],
  [
    "no confirmed loss gives keeps",
    () => skillVerdict({ informative: 7, controlLoss: true, falseLoss: false, losses: 0 }),
    keeps,
  ],

  // The skill's score from its run records: an unscreened case is not counted.
  [
    "six informative cases with a control loss on the lowest id keep",
    () => {
      const score = scoreSkill(sixCases(20));
      return [score.informative, score.controlCase, score.outcome];
    },
    [6, "c1", keeps],
  ],
  [
    "a planted-loss control at 9 fewer passes of 40 gives no verdict",
    () => scoreSkill(sixCases(21)).outcome,
    { verdict: null, reason: "control not a loss" },
  ],
  [
    "a false-loss control at 10 fewer passes of 40 gives no verdict",
    () => scoreSkill(sixCases(20, 20)).outcome,
    { verdict: null, reason: "false-loss control a loss" },
  ],
  [
    "a false-loss control at 9 fewer passes of 40 keeps the verdict",
    () => scoreSkill(sixCases(20, 21)).outcome,
    keeps,
  ],
  [
    "a control stage whose twin lacks its runs waits for them",
    () => scoreSkill(sixCases(20).filter((run) => run.arm !== "twin")).outcome,
    undefined,
  ],
  [
    "a flagged case's confirm runs are not waited for when the false-loss control is a loss",
    () => scoreSkill([...sixCases(20, 20), ...screen("c7", 8, 6, 5)]).outcome,
    { verdict: null, reason: "false-loss control a loss" },
  ],
  [
    "an unscreened case is not counted among the informative cases",
    () => {
      const records = sixCases(20).filter(
        (run) => !(run.caseId === "c6" && run.arm === "old" && run.verdict === "pass")
      );
      records.push(...runs("c6", "screen", "old", 7, 7), ...unjudged("c6", "screen", "old", 6));
      const score = scoreSkill(records);
      return [score.screened, score.informative, score.outcome];
    },
    [5, 5, tooFew],
  ],
  [
    "a flagged case waits for its confirm runs",
    () => scoreSkill([...sixCases(20), ...screen("c7", 8, 6, 5)]).outcome,
    undefined,
  ],
  [
    "a flagged case that is a loss on its confirm runs gives loss",
    () =>
      scoreSkill([...sixCases(20), ...screen("c7", 8, 6, 5), ...fresh("c7", "confirm", 30, 20)])
        .outcome,
    loss,
  ],
  [
    "a flagged case whose confirm runs stay unscreened gives no verdict, never keeps",
    () =>
      scoreSkill([
        ...sixCases(20),
        ...screen("c7", 8, 6, 5),
        ...runs("c7", "confirm", "old", 30, 40),
        ...runs("c7", "confirm", "new", 20, 34),
        ...unjudged("c7", "confirm", "new", 11),
      ]).outcome,
    { verdict: null, reason: "confirm runs missing: c7" },
  ],

  // Drops (the architect, 2026-11-22T01:28:33Z): no dropped case leaves the score.
  [
    "a case dropped at screen is not informative",
    () => {
      const score = scoreSkill(sixCases(20), droppedAt("screen", "c6"));
      return [score.screened, score.informative, score.outcome];
    },
    [5, 5, tooFew],
  ],
  [
    "a flagged case dropped at confirm gives no verdict that names it, never keeps",
    () =>
      scoreSkill([...sixCases(20), ...screen("c7", 8, 6, 5)], droppedAt("confirm", "c7")).outcome,
    {
      verdict: null,
      reason: "flagged case c7 could not be confirmed (unreadable-directory: acme/skills c7)",
    },
  ],
  [
    "a flagged case dropped at confirm leaves another case's confirmed loss standing as the skill's",
    () =>
      scoreSkill(
        [
          ...sixCases(20),
          ...screen("c7", 8, 6, 5),
          ...screen("c8", 8, 6, 5),
          ...fresh("c8", "confirm", 30, 20),
        ],
        droppedAt("confirm", "c7")
      ).outcome,
    loss,
  ],
  [
    "a dropped control case keeps its screen and gives its place to the next case of the control rule's order",
    () => {
      const score = scoreSkill(
        [
          ...screen("c1", 9, 9, 5),
          ...["c2", "c3", "c4", "c5", "c6"].flatMap((id) => screen(id, 8, 8, 5)),
          ...fresh("c2", "control", 30, 20),
        ],
        droppedAt("control", "c1")
      );
      return [score.informative, score.controlCase, score.replacedControls, score.outcome];
    },
    [6, "c2", ["c1"], keeps],
  ],
  [
    "a skill whose every informative case is dropped after its screen gets no verdict naming each",
    () =>
      scoreSkill(
        sixCases(20).filter((run) => run.stage === "screen"),
        droppedAt("control", "c1", "c2", "c3", "c4", "c5", "c6")
      ).outcome,
    {
      verdict: null,
      reason:
        "no informative case is left to be the control case: each is dropped (c1 at control, c2 at control, c3 at control, c4 at control, c5 at control, c6 at control)",
    },
  ],
  [
    "a case recorded as dropped at confirm that its screen did not flag is refused, naming it",
    () => {
      try {
        scoreSkill(sixCases(20), droppedAt("confirm", "c2"));
        return "scored";
      } catch (error) {
        return (error as Error).message;
      }
    },
    "case c2 is recorded as dropped at confirm, but its screen did not flag it, and only flagged cases are confirmed: its records are refused, not scored (its drop: unreadable-directory: acme/skills c2)",
  ],

  // Pooled confirms (the architect's decision "The behaviour test's pass rule", 2026-11-22).
  [
    "30 fewer passes of 120 pooled runs are a loss",
    () => isPooledLoss({ old: 120, new: 90 }),
    true,
  ],
  [
    "29 fewer passes of 120 pooled runs are no loss",
    () => isPooledLoss({ old: 120, new: 91 }),
    false,
  ],
  [
    "two confirms on one runner that disagree call for a third",
    () => confirmScore("c7", onOneRunner(30, 31)),
    { state: "running", third: true },
  ],
  [
    "three confirms on one runner pooled at 29 fewer of 120 are no loss",
    () => confirmScore("c7", onOneRunner(30, 31, 30)),
    {
      state: "counted",
      loss: false,
      counted: [
        { old: 40, new: 30 },
        { old: 40, new: 31 },
        { old: 40, new: 30 },
      ],
      pooled: { old: 120, new: 91 },
    },
  ],
  [
    "three confirms on one runner pooled at 30 fewer of 120 are a loss",
    () => confirmScore("c7", onOneRunner(30, 31, 29)),
    {
      state: "counted",
      loss: true,
      counted: [
        { old: 40, new: 30 },
        { old: 40, new: 31 },
        { old: 40, new: 29 },
      ],
      pooled: { old: 120, new: 90 },
    },
  ],
  [
    "the decision's first case: 40 against 40, 40 against 30, then 40 against 39 pool to 120 against 109, no loss",
    () => confirmScore("c7", onOneRunner(40, 30, 39)),
    {
      state: "counted",
      loss: false,
      counted: [
        { old: 40, new: 40 },
        { old: 40, new: 30 },
        { old: 40, new: 39 },
      ],
      pooled: { old: 120, new: 109 },
    },
  ],
  [
    "two confirms on one runner that agree are what both say, and call for no third",
    () => confirmScore("c7", onOneRunner(30, 25)),
    {
      state: "counted",
      loss: true,
      counted: [
        { old: 40, new: 30 },
        { old: 40, new: 25 },
      ],
      pooled: undefined,
    },
  ],
  [
    "confirms on two runners are not pooled: the last runner's confirm counts alone",
    () => confirmScore("c7", [confirmOn("r1", 40, 30), confirmOn("r2", 40, 31)]),
    { state: "counted", loss: false, counted: [{ old: 40, new: 31 }], pooled: undefined },
  ],
  [
    "a fourth confirm on one runner is refused, naming the case",
    () => {
      try {
        confirmScore("c7", onOneRunner(30, 31, 30, 29));
        return "scored";
      } catch (error) {
        return (error as Error).message;
      }
    },
    "case c7 holds 4 confirms at the same texts on one runner: the pooled rule takes 3, and no confirm runs after them",
  ],
  [
    "a flagged case whose three pooled confirms are 30 fewer of 120 gives loss",
    () =>
      scoreSkill(
        [...sixCases(20), ...screen("c7", 8, 6, 5)],
        new Map([["c7", { confirms: onOneRunner(30, 31, 29) }]])
      ).outcome,
    loss,
  ],

  // listing: a case or a load task whose rebuilt listing misses more than 10% of its session's
  // recorded names is dropped (the architect, 2026-11-22T03:04Z). One whose capture builds its
  // listing from the rebuilt profile (a measure of kind `subagent listing`) is dropped for adding
  // more than 10% of them too (the architect, 2026-11-24T23:52Z); one whose capture sends its
  // session's recorded system prompt whole (kind `session listing`) is not, since the model sees
  // the recorded listing whatever the profile installs (the architect, 2026-11-25T00:52:35Z).
  [
    "a listing that misses 2 of 20 recorded names (10%) is measured, and the case stays",
    () => listingStanding(measured("session listing", 20, ["a1", "a2"])),
    { state: "measured", recorded: 20, missing: 2, added: 0 },
  ],
  [
    "a listing that misses 3 of 20 recorded names (15%) drops the case, the reason naming them",
    () => listingStanding(measured("subagent listing", 20, ["a1", "a2", "a3"])),
    {
      state: "dropped",
      reason:
        "listing-misses: its rebuilt listing misses 3 of the 20 names of the recorded subagent listing (15.0%), more than 10%: a1, a2, a3",
    },
  ],
  [
    "a listing that misses 1 of 9 recorded names (11.1%) drops the case",
    () => listingStanding(measured("session listing", 9, ["a1"])).state,
    "dropped",
  ],
  [
    "a measure that says not measurable keeps the case in, with why",
    () => listingStanding(measured("not measurable", null, [])),
    { state: "not measurable", why: "the made-up session records no system prompt" },
  ],
  [
    "evidence that holds no listing measure is not measurable",
    () => listingStanding([{ item: "role", source: "made up" }]),
    { state: "not measurable", why: "its evidence holds no listing measure" },
  ],
  [
    "a profile-built measure that adds 3 names to 20 recorded names (15%) is still dropped as listing-adds, the reason naming them",
    () => listingStanding(measured("subagent listing", 20, [], ["b1", "b2", "b3"])),
    {
      state: "dropped",
      reason:
        "listing-adds: its rebuilt listing adds 3 to the 20 names of the recorded subagent listing (15.0%), more than 10%: b1, b2, b3",
    },
  ],
  [
    "a profile-built listing that adds 2 names to 20 recorded names (10%) is measured, and the case stays",
    () => listingStanding(measured("subagent listing", 20, [], ["b1", "b2"])),
    { state: "measured", recorded: 20, missing: 0, added: 2 },
  ],
  [
    "a listing that misses 3 and adds 3 of 20 recorded names drops the case as listing-misses, the reason naming the adds too",
    () => listingStanding(measured("subagent listing", 20, ["a1", "a2", "a3"], ["b1", "b2", "b3"])),
    {
      state: "dropped",
      reason:
        "listing-misses: its rebuilt listing misses 3 of the 20 names of the recorded subagent listing (15.0%), more than 10%: a1, a2, a3; listing-adds: its rebuilt listing adds 3 to the 20 names of the recorded subagent listing (15.0%), more than 10%: b1, b2, b3",
    },
  ],
  [
    "a measure with no added names array reads as adding none",
    () => {
      const [item] = measured("session listing", 20, ["a1"]);
      const { added: _, ...rest } = JSON.parse(item?.source ?? "{}");
      return listingStanding([{ item: LISTING_MEASURE_ITEM, source: JSON.stringify(rest) }]);
    },
    { state: "measured", recorded: 20, missing: 1, added: 0 },
  ],
  [
    "a recorded-prompt measure that adds 205 to 9 recorded names and misses none is measured (kept)",
    () =>
      listingStanding(
        measured(
          "session listing",
          9,
          [],
          Array.from({ length: 205 }, (_, n) => `extra-${n + 1}`)
        )
      ),
    { state: "measured", recorded: 9, missing: 0, added: 205 },
  ],
  [
    "a recorded-prompt measure that misses 2 of 9 recorded names (22.2%) and adds 205 is dropped as listing-misses alone",
    () =>
      listingStanding(
        measured(
          "session listing",
          9,
          ["a1", "a2"],
          Array.from({ length: 205 }, (_, n) => `extra-${n + 1}`)
        )
      ),
    {
      state: "dropped",
      reason:
        "listing-misses: its rebuilt listing misses 2 of the 9 names of the recorded session listing (22.2%), more than 10%: a1, a2",
    },
  ],
  [
    "a measure of kind session listing is dropped for the names it adds where the capture sends no recorded prompt (Claude Code's), as a profile-built one is",
    () =>
      listingStanding(measured("session listing", 20, [], ["b1", "b2", "b3"]), {
        recordedPromptSent: false,
      }),
    {
      state: "dropped",
      reason:
        "listing-adds: its rebuilt listing adds 3 to the 20 names of the recorded session listing (15.0%), more than 10%: b1, b2, b3",
    },
  ],

  // outsideListing: the runs of the cases or tasks whose capture sends the recorded prompt whole
  // that read a skill the recorded listing lacks, by skill and answer kind (the architect,
  // 2026-11-25T00:52:35Z).
  [
    "a read of a skill the recorded listing lacks counts its run once per skill and answer kind; a read the listing holds, one answered from the arm's own text, and a run that records no reads are no count; a run with no read is examined",
    () =>
      outsideListing([
        {
          recorded: new Set(["alpha", "beta"]),
          runs: [
            [
              { skill: "gamma", from: "copy" },
              { skill: "epsilon", from: "arm" },
            ],
            [
              { skill: "gamma", from: "copy" },
              { skill: "gamma", from: "copy" },
            ],
            [
              { skill: "delta", from: "absent" },
              { skill: "gamma", from: "copy" },
            ],
            undefined,
            [],
            [{ skill: "beta", from: "copy" }],
          ],
        },
        { recorded: new Set(["alpha"]), runs: [[{ skill: "beta", from: "absent" }]] },
      ]),
    {
      groups: 2,
      runs: 7,
      examined: 6,
      outside: 4,
      bySkill: {
        beta: { copy: 0, absent: 1 },
        delta: { copy: 0, absent: 1 },
        gamma: { copy: 3, absent: 0 },
      },
    },
  ],
  [
    "a skill read answered from the arm's own text outside the listing is no count",
    () =>
      outsideListing([{ recorded: new Set(["beta"]), runs: [[{ skill: "alpha", from: "arm" }]] }]),
    { groups: 1, runs: 1, examined: 1, outside: 0, bySkill: {} },
  ],
  [
    "the outside-listing line counts the examined runs, the runs with no recorded reads, and each skill by answer kind",
    () =>
      outsideListingWords(
        outsideListing([
          {
            recorded: new Set(["alpha"]),
            runs: [
              [{ skill: "gamma", from: "copy" }],
              [{ skill: "gamma", from: "absent" }],
              undefined,
            ],
          },
        ]),
        "task"
      ),
    "outside recorded listing: 2 of 2 runs of 1 recorded-prompt task (the capture sends the session's recorded system prompt whole) read a skill the recorded listing lacks, not counting 1 run with no recorded reads; by skill and answer (copy: an installed copy; absent: the unknown-skill reply): gamma (1 copy, 1 absent)",
  ],
  [
    "the outside-listing line of no recorded-prompt case says none",
    () => outsideListingWords(outsideListing([]), "case"),
    "outside recorded listing: 0 of 0 runs of 0 recorded-prompt cases (the capture sends the session's recorded system prompt whole) read a skill the recorded listing lacks; by skill and answer (copy: an installed copy; absent: the unknown-skill reply): none",
  ],

  // skills tested: the walk of the ranking.
  [
    "the ranking is by sessions, ties by name",
    () =>
      rankSkills([
        { skill: "beta", sessions: 5 },
        { skill: "alpha", sessions: 5 },
        { skill: "gamma", sessions: 9 },
      ]),
    ["gamma", "alpha", "beta"],
  ],
  [
    "the first three skills get tested",
    () => walk(ranking, new Map()),
    {
      tested: [
        { skill: "alpha", outcome: undefined },
        { skill: "beta", outcome: undefined },
        { skill: "gamma", outcome: undefined },
      ],
      next: ["alpha", "beta", "gamma"],
      done: false,
    },
  ],
  [
    "a no-verdict skill is replaced by the next",
    () =>
      walk(
        ranking,
        new Map<string, SkillOutcome>([
          ["alpha", keeps],
          ["beta", tooFew],
        ])
      ).next,
    ["gamma", "delta"],
  ],
  [
    "the walk stops at three verdicts",
    () =>
      walk(
        ranking,
        new Map<string, SkillOutcome>([
          ["alpha", keeps],
          ["beta", tooFew],
          ["gamma", loss],
          ["delta", keeps],
          ["epsilon", keeps],
        ])
      ),
    {
      tested: [
        { skill: "alpha", outcome: keeps },
        { skill: "beta", outcome: tooFew },
        { skill: "gamma", outcome: loss },
        { skill: "delta", outcome: keeps },
      ],
      next: [],
      done: true,
    },
  ],
  [
    "the walk stops at the end of the list, each tested skill with a verdict or its reason",
    () =>
      walk(
        ["alpha", "beta", "gamma", "delta"],
        new Map<string, SkillOutcome>([
          ["alpha", tooFew],
          ["beta", keeps],
          ["gamma", tooFew],
          ["delta", loss],
        ])
      ),
    {
      tested: [
        { skill: "alpha", outcome: tooFew },
        { skill: "beta", outcome: keeps },
        { skill: "gamma", outcome: tooFew },
        { skill: "delta", outcome: loss },
      ],
      next: [],
      done: true,
    },
  ],

  // case cap.
  [
    "a skill 5 sessions read gets no screening",
    () => screeningRefusal(5),
    { verdict: null, reason: "fewer than 6 sessions read it" },
  ],
  ["a skill 6 sessions read is screened", () => screeningRefusal(6), undefined],
  [
    "a skill 5 of whose 8 reading sessions are on served models gets no screening",
    () => servedScreeningRefusal({ served: 5, sessions: 8 }),
    {
      verdict: null,
      reason:
        "too few sessions on served models: 5 of its 8 reading sessions are on a model the gateway serves, fewer than 6",
    },
  ],
  [
    "a skill 6 of whose 8 reading sessions are on served models is screened",
    () => servedScreeningRefusal({ served: 6, sessions: 8 }),
    undefined,
  ],
  [
    "screening stops at 6 informative cases",
    () => screeningDone({ screened: 6, informative: 6 }),
    true,
  ],
  [
    "screening goes on at 5 informative of 14 screened cases",
    () => screeningDone({ screened: 14, informative: 5 }),
    false,
  ],
  [
    "screening stops at 15 screened cases",
    () => screeningDone({ screened: 15, informative: 5 }),
    true,
  ],

  // load fault, per task of 3 runs.
  [
    "a need task loaded on 1 of 3 runs is a fault",
    () => taskFault({ label: "need", loads: 1 }),
    true,
  ],
  [
    "a need task loaded on 2 of 3 runs is no fault",
    () => taskFault({ label: "need", loads: 2 }),
    false,
  ],
  [
    "a near-miss task loaded on 2 of 3 runs is a fault",
    () => taskFault({ label: "near-miss", loads: 2 }),
    true,
  ],
  [
    "a near-miss task loaded on 1 of 3 runs is no fault",
    () => taskFault({ label: "near-miss", loads: 1 }),
    false,
  ],
  ["a description with one faulty task is a fault", () => descriptionFault(failing), true],
  ["a description with no faulty task is no fault", () => descriptionFault(passing), false],

  // poor name: the current name fails the load test and a proposed name passes.
  ["current fails, proposed passes: a poor name", () => poorName(failing, passing), true],
  ["current fails, proposed fails: no poor name", () => poorName(failing, failing), false],
  ["current passes, proposed passes: no poor name", () => poorName(passing, passing), false],
  ["current passes, proposed fails: no poor name", () => poorName(passing, failing), false],
];

test.each(rows)("%s", (_name, got, want) => {
  expect(got()).toEqual(want);
});
