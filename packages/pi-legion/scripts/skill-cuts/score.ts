/**
 * The rules of the behaviour test (README.md, "behaviour"), its load test (load.ts) and its name
 * test (a load test under a proposed name), as pure functions: no file, network or clock. The
 * runner reads the constants below to decide how many runs to start, and `report` reads the same
 * functions to print each case's passes and each skill's verdict, so both apply one rule.
 *
 * A run record is one JSON line per run (RunRecord). Counts are of judged runs only: a run whose
 * verdict is `pass` or `fail`. A run that errored, timed out or whose judgement did not parse has
 * a null verdict; it is no fail, and it is run again, at most MAX_EXTRA_RUNS extra times per
 * case, stage and arm. A case whose arm still lacks its judged runs is "unscreened" and is not
 * counted (stageOutcome).
 *
 * The control stage holds a skill's two controls on its control case, their arms' runs sent in
 * one interleaved order: the planted-loss control (`old` against `minus`, the old text less the
 * case's line), which must be a loss, so the test can see a loss; and the false-loss control
 * (`old` against `twin`, the old text again under another arm's name, its own capture and runs),
 * which must be no loss, so the test does not call a loss where the texts are the same. Both are
 * scored by the confirm rule, LOSS_GAP of FRESH_RUNS; a skill whose planted-loss control is no
 * loss, or whose false-loss control is a loss, gets no verdict (skillVerdict). The false-loss
 * control is the architect's check: the runs of one arm once shared one request id, and the same
 * body sent with two fixed ids passed 8 and 26 of 30 runs.
 */
import { z } from "zod";

/** Runs of each arm of a case in the screen stage. */
export const SCREEN_RUNS = 10;
/** Fresh runs of each arm of a case in the confirm and control stages. */
export const FRESH_RUNS = 40;
/** Runs, beyond a stage's own, that may replace runs with no judged verdict, per case, stage and
 *  arm. */
export const MAX_EXTRA_RUNS = 5;
/** A case is informative when the old text passes at least this many more of its screen runs than
 *  no skill. */
export const INFORMATIVE_GAP = 2;
/** An informative case is flagged for the confirm stage when the cut text passes at least this
 *  many fewer of its screen runs than the old text. */
export const FLAG_GAP = 2;
/** A confirmed case is a loss, and a control case a control loss, when the other arm passes at
 *  least this many fewer of its fresh runs than the old text. */
export const LOSS_GAP = 10;
/** The informative cases a skill needs for a verdict; screening stops when it has them. */
export const MIN_INFORMATIVE_CASES = 6;
/** The screened cases after which screening of a skill stops, informative or not. */
export const MAX_SCREENED_CASES = 15;
/** The sessions in the window that must read a skill before it is screened (one case per
 *  session). */
export const MIN_READING_SESSIONS = 6;
/** The verdicts the walk of a cut's ranking stops at. */
export const VERDICTS_PER_CUT = 3;
/** Runs of each task of a load test. */
export const LOAD_RUNS = 3;
/** A task that needs the skill is a fault when it loads on fewer of its runs than this. */
export const NEED_MIN_LOADS = 2;
/** A near-miss task is a fault when it loads on more of its runs than this. */
export const NEAR_MISS_MAX_LOADS = 1;

/** The texts a case runs under: the cut's base, the cut text, no skill, the base without the
 *  case's line (the planted-loss control), and the base again under another arm's name (`twin`,
 *  the false-loss control). */
export const Arm = z.enum(["old", "new", "none", "minus", "twin"]);
export type Arm = z.infer<typeof Arm>;
export const Stage = z.enum(["screen", "confirm", "control"]);
export type Stage = z.infer<typeof Stage>;
/** Where a case stands in a stage (StageOutcome's `state`). */
export const StageState = z.enum(["running", "counted", "unscreened"]);
export type StageState = z.infer<typeof StageState>;
/** One run of one arm of a case: its judge's verdict, or null for a run that errored, timed out or
 *  whose judgement did not parse, and the judge's reason or what went wrong. */
export const RunRecord = z.strictObject({
  caseId: z.string().min(1),
  arm: Arm,
  stage: Stage,
  verdict: z.enum(["pass", "fail"]).nullable(),
  reason: z.string(),
});
export type RunRecord = z.infer<typeof RunRecord>;

/** The arms each stage runs: screen the old text, the cut and no skill; confirm the old text and
 *  the cut; control the old text, the old text less the case's line, and the old text again. */
export const STAGE_ARMS = {
  screen: ["old", "new", "none"],
  confirm: ["old", "new"],
  control: ["old", "minus", "twin"],
} as const satisfies Record<Stage, readonly Arm[]>;
/** The judged runs each arm of a stage needs. */
export const STAGE_RUNS: Record<Stage, number> = {
  screen: SCREEN_RUNS,
  confirm: FRESH_RUNS,
  control: FRESH_RUNS,
};
export type StageArm<S extends Stage> = (typeof STAGE_ARMS)[S][number];
/** Passes of each arm of a stage, each of that stage's runs. */
export type Passes<S extends Stage> = Record<StageArm<S>, number>;
/** Where a case stands in a stage: every arm has its judged runs (`counted`, with each arm's
 *  passes); some arm can still start the runs it lacks (`running`, with how many to start per
 *  arm); or some arm lacks its runs and has used its extra runs (`unscreened`). */
export type StageOutcome<S extends Stage> =
  | { state: "counted"; passes: Passes<S> }
  | { state: "running"; toStart: Record<StageArm<S>, number> }
  | { state: "unscreened" };

/** Where case `caseId` stands in `stage`, from the run records so far (StageOutcome). An arm with
 *  more judged runs than the stage runs is the runner's fault, and throws. */
export function stageOutcome<S extends Stage>(
  records: readonly RunRecord[],
  caseId: string,
  stage: S
): StageOutcome<S> {
  const required = STAGE_RUNS[stage];
  const arms: readonly StageArm<S>[] = STAGE_ARMS[stage];
  const passes = {} as Passes<S>;
  const toStart = {} as Record<StageArm<S>, number>;
  let running = false;
  for (const arm of arms) {
    let attempts = 0;
    let judged = 0;
    let passed = 0;
    for (const record of records) {
      if (record.caseId !== caseId || record.stage !== stage || record.arm !== arm) continue;
      attempts++;
      if (record.verdict !== null) judged++;
      if (record.verdict === "pass") passed++;
    }
    if (judged > required)
      throw new Error(
        `case ${caseId}: the ${stage} arm ${arm} holds ${judged} judged runs, more than its ${required}`
      );
    const lacking = required - judged;
    const left = required + MAX_EXTRA_RUNS - attempts;
    if (lacking > 0 && left <= 0) return { state: "unscreened" };
    passes[arm] = passed;
    toStart[arm] = lacking > 0 ? Math.min(lacking, left) : 0;
    running ||= lacking > 0;
  }
  return running ? { state: "running", toStart } : { state: "counted", passes };
}

/** Refuses a count that is not a count of `runs` runs, so a rule of 10 runs never reads the passes
 *  of 40, or the reverse. */
function assertCounts(counts: Record<string, number>, runs: number): void {
  for (const [name, count] of Object.entries(counts))
    if (!Number.isInteger(count) || count < 0 || count > runs)
      throw new RangeError(`${name} ${count} is no count of ${runs} runs`);
}

/** Whether a screened case is informative: the old text passes at least INFORMATIVE_GAP more of
 *  its 10 runs than no skill, so the case can tell the skill's text from its absence. */
export function isInformative(passes: Pick<Passes<"screen">, "old" | "none">): boolean {
  assertCounts(passes, SCREEN_RUNS);
  return passes.old - passes.none >= INFORMATIVE_GAP;
}

/** Whether a screened case goes to the confirm stage: it is informative, and the cut passes at
 *  least FLAG_GAP fewer of its 10 runs than the old text. */
export function isFlagged(passes: Passes<"screen">): boolean {
  assertCounts(passes, SCREEN_RUNS);
  return isInformative(passes) && passes.old - passes.new >= FLAG_GAP;
}

/** Whether a confirmed case is a loss: the cut passes at least LOSS_GAP fewer of 40 fresh runs than
 *  the old text. */
export function isLoss(passes: Passes<"confirm">): boolean {
  assertCounts(passes, FRESH_RUNS);
  return passes.old - passes.new >= LOSS_GAP;
}

/** Whether a skill's control case is a loss by the same rule as isLoss, with the old text less the
 *  case's line in the cut's place: the test can see a loss of that line. */
export function isControlLoss(passes: Passes<"control">): boolean {
  assertCounts(passes, FRESH_RUNS);
  return passes.old - passes.minus >= LOSS_GAP;
}

/** Whether a skill's false-loss control is a loss by the same rule as isLoss, with the old text
 *  again (`twin`) in the cut's place: the two arms' texts are the same, so a loss here is the
 *  procedure's own (its sends, the gateway, the judge), and it would call one in a cut too. */
export function isFalseLoss(passes: Passes<"control">): boolean {
  assertCounts(passes, FRESH_RUNS);
  return passes.old - passes.twin >= LOSS_GAP;
}

/** The id of a skill's control case: among its informative screened cases, the one whose old text
 *  passes most more often than no skill, a tie going to the lower id in string order. Undefined
 *  when no case is informative. */
export function controlCase(
  cases: readonly { caseId: string; passes: Passes<"screen"> }[]
): string | undefined {
  let best: { caseId: string; gap: number } | undefined;
  for (const { caseId, passes } of cases) {
    if (!isInformative(passes)) continue;
    const gap = passes.old - passes.none;
    if (best === undefined || gap > best.gap || (gap === best.gap && caseId < best.caseId))
      best = { caseId, gap };
  }
  return best?.caseId;
}

/** A skill that gets no verdict, and why. */
export type NoVerdict = { verdict: null; reason: string };
/** A skill's verdict: the cut lost what a case's line makes agents do, or kept it; or no
 *  verdict. */
export type SkillOutcome = { verdict: "loss" | "keeps" } | NoVerdict;

/** A skill's verdict from its counted cases: no verdict with fewer than MIN_INFORMATIVE_CASES
 *  informative cases, since too few cases see the skill at all; no verdict when its planted-loss
 *  control is no loss, since then the test could not see a loss, or when its false-loss control
 *  is a loss, since then the test calls one where none is (the reason names each control that
 *  failed); else `loss` when any confirmed case is a loss, else `keeps`. */
export function skillVerdict(counts: {
  informative: number;
  controlLoss: boolean;
  falseLoss: boolean;
  losses: number;
}): SkillOutcome {
  if (counts.informative < MIN_INFORMATIVE_CASES)
    return {
      verdict: null,
      reason: `fewer than ${MIN_INFORMATIVE_CASES} informative cases: ${counts.informative}`,
    };
  const failed = [
    ...(counts.controlLoss ? [] : ["control not a loss"]),
    ...(counts.falseLoss ? ["false-loss control a loss"] : []),
  ];
  if (failed.length > 0) return { verdict: null, reason: failed.join("; ") };
  return { verdict: counts.losses > 0 ? "loss" : "keeps" };
}

/** One case of a skill: its screen stage, whether it is informative and flagged (both false unless
 *  its screen runs are counted), its confirm stage when flagged, and whether that is a counted
 *  loss. */
export interface CaseScore {
  caseId: string;
  screen: StageOutcome<"screen">;
  informative: boolean;
  flagged: boolean;
  confirm: StageOutcome<"confirm"> | undefined;
  loss: boolean;
}
/** A skill's cases and verdict, from the run records of its cases. `screened` counts the cases
 *  whose screen runs are counted (the case cap's count); `control` is the control case's control
 *  stage. `outcome` is undefined while a stage the verdict waits on can still start runs. */
export interface SkillScore {
  cases: CaseScore[];
  screened: number;
  informative: number;
  controlCase: string | undefined;
  control: StageOutcome<"control"> | undefined;
  outcome: SkillOutcome | undefined;
}

/** A skill's score from the run records of its cases, in the order their first screen record
 *  comes. Unscreened cases count for nothing. The verdict waits for every case's screen runs; with
 *  enough informative cases, for the control's runs (its planted-loss and false-loss controls are
 *  one stage); and when the planted-loss control is a loss and the false-loss control is not, for
 *  the confirm runs of every flagged case. An unscreened control is no control loss. A flagged
 *  case whose confirm runs stay unscreened gives no verdict, since its loss can be neither seen
 *  nor ruled out, and a `keeps` over it would hide one. */
export function scoreSkill(records: readonly RunRecord[]): SkillScore {
  const ids = [
    ...new Set(records.filter((record) => record.stage === "screen").map((r) => r.caseId)),
  ];
  const cases: CaseScore[] = ids.map((caseId) => {
    const screen = stageOutcome(records, caseId, "screen");
    const counted = screen.state === "counted";
    const informative = counted && isInformative(screen.passes);
    const flagged = counted && isFlagged(screen.passes);
    const confirm = flagged ? stageOutcome(records, caseId, "confirm") : undefined;
    const loss = confirm?.state === "counted" && isLoss(confirm.passes);
    return { caseId, screen, informative, flagged, confirm, loss };
  });
  const screenedCases = cases.flatMap(({ caseId, screen }) =>
    screen.state === "counted" ? [{ caseId, passes: screen.passes }] : []
  );
  const informative = cases.filter((c) => c.informative).length;
  const chosen = controlCase(screenedCases);
  const control = chosen === undefined ? undefined : stageOutcome(records, chosen, "control");
  const score = {
    cases,
    screened: screenedCases.length,
    informative,
    controlCase: chosen,
    control,
  };
  const waiting = (outcome: { state: StageState } | undefined) => outcome?.state === "running";
  if (cases.some((c) => waiting(c.screen))) return { ...score, outcome: undefined };
  if (informative >= MIN_INFORMATIVE_CASES && waiting(control))
    return { ...score, outcome: undefined };
  const controlLoss = control?.state === "counted" && isControlLoss(control.passes);
  const falseLoss = control?.state === "counted" && isFalseLoss(control.passes);
  const controlsHold = controlLoss && !falseLoss;
  if (controlsHold && cases.some((c) => waiting(c.confirm)))
    return { ...score, outcome: undefined };
  const unconfirmed = controlsHold
    ? cases.filter((c) => c.confirm?.state === "unscreened").map((c) => c.caseId)
    : [];
  if (informative >= MIN_INFORMATIVE_CASES && unconfirmed.length > 0)
    return {
      ...score,
      outcome: { verdict: null, reason: `confirm runs missing: ${unconfirmed.join(", ")}` },
    };
  const losses = cases.filter((c) => c.loss).length;
  return { ...score, outcome: skillVerdict({ informative, controlLoss, falseLoss, losses }) };
}

/** The no-verdict outcome of a skill that too few sessions in the window read to give it
 *  MIN_READING_SESSIONS cases (one per session), which is then not screened; undefined for a skill
 *  that is screened. */
export function screeningRefusal(sessions: number): NoVerdict | undefined {
  return sessions < MIN_READING_SESSIONS
    ? { verdict: null, reason: `fewer than ${MIN_READING_SESSIONS} sessions read it` }
    : undefined;
}

/** The no-verdict outcome of a skill whose reading sessions on a model the gateway serves
 *  (`served` of its `sessions`: attribute.ts's `ModelCount` of its sessions file) are fewer than
 *  MIN_READING_SESSIONS: a case runs only on a served model, so the skill is then not screened;
 *  undefined for a skill that is screened. */
export function servedScreeningRefusal(counts: {
  served: number;
  sessions: number;
}): NoVerdict | undefined {
  return counts.served < MIN_READING_SESSIONS
    ? {
        verdict: null,
        reason: `too few sessions on served models: ${counts.served} of its ${counts.sessions} reading sessions are on a model the gateway serves, fewer than ${MIN_READING_SESSIONS}`,
      }
    : undefined;
}

/** Whether screening of a skill stops: it has MIN_INFORMATIVE_CASES informative cases, or
 *  MAX_SCREENED_CASES screened (counted) cases. */
export function screeningDone(counts: { screened: number; informative: number }): boolean {
  return counts.informative >= MIN_INFORMATIVE_CASES || counts.screened >= MAX_SCREENED_CASES;
}

/** A cut's changed skills, loaded skills included, most read first by the counted sessions of the
 *  cut repository's copy, a tie going to the lower name in string order. */
export function rankSkills(rows: readonly { skill: string; sessions: number }[]): string[] {
  return rows
    .toSorted((a, b) =>
      a.sessions !== b.sessions ? b.sessions - a.sessions : a.skill < b.skill ? -1 : 1
    )
    .map((row) => row.skill);
}

/** The walk of a cut's ranking so far: each skill reached, in ranking order, with its outcome
 *  (undefined until it has one), and the skills to test now, which are the reached ones with no
 *  outcome. The walk is done when none is left to test: it has VERDICTS_PER_CUT verdicts, or it
 *  reached the end of the ranking and each tested skill has a verdict or its reason. */
export interface Walk {
  tested: { skill: string; outcome: SkillOutcome | undefined }[];
  next: string[];
  done: boolean;
}

/** Where the walk of `ranking` stands given the `outcomes` so far: the first VERDICTS_PER_CUT
 *  skills are reached, and each one that gets no verdict lets the walk reach the next, until the
 *  verdicts and the skills still to test make VERDICTS_PER_CUT, or the ranking ends. A skill's
 *  outcome is ignored until the walk reaches it. */
export function walk(
  ranking: readonly string[],
  outcomes: ReadonlyMap<string, SkillOutcome>
): Walk {
  const tested: Walk["tested"] = [];
  let verdicts = 0;
  let open = 0;
  for (const skill of ranking) {
    if (verdicts + open >= VERDICTS_PER_CUT) break;
    const outcome = outcomes.get(skill);
    tested.push({ skill, outcome });
    if (outcome === undefined) open++;
    else if (outcome.verdict !== null) verdicts++;
  }
  const next = tested.filter((entry) => entry.outcome === undefined).map((entry) => entry.skill);
  return { tested, next, done: next.length === 0 };
}

/** A load test task's label: the task needs the skill, or is a near miss that does not. */
export const TaskLabel = z.enum(["need", "near-miss"]);
export type TaskLabel = z.infer<typeof TaskLabel>;
/** One task of a load test: its label and the runs of its LOAD_RUNS whose first assistant message
 *  loaded the skill. */
export interface LoadTask {
  label: TaskLabel;
  loads: number;
}

/** Whether one task of a load test is a fault: a need task that loads on fewer than
 *  NEED_MIN_LOADS of its 3 runs, or a near-miss task that loads on more than
 *  NEAR_MISS_MAX_LOADS. */
export function taskFault(task: LoadTask): boolean {
  assertCounts({ [task.label]: task.loads }, LOAD_RUNS);
  return task.label === "need" ? task.loads < NEED_MIN_LOADS : task.loads > NEAR_MISS_MAX_LOADS;
}

/** Whether a load test finds a description fault: any of its tasks is a fault. A name that fails
 *  the load test is one whose test finds this fault. */
export function descriptionFault(tasks: readonly LoadTask[]): boolean {
  return tasks.some(taskFault);
}

/** Whether a name test shows a poor name: with the description unchanged, the load test fails
 *  under the current name and passes under the proposed one. */
export function poorName(current: readonly LoadTask[], proposed: readonly LoadTask[]): boolean {
  return descriptionFault(current) && !descriptionFault(proposed);
}
