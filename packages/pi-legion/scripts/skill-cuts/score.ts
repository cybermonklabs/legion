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
 *
 * A dropped case is never taken out of the score (CaseFacts, scoreSkill): one dropped at screen is
 * not informative; one dropped at confirm or control keeps its screen, so a flagged case that
 * cannot be confirmed leaves its skill no `keeps` (the architect's rule of 2026-11-22), and a
 * dropped control case gives its place to the next case the control rule chooses. A case's
 * confirms on one runner at one set of texts pool when two disagree (confirmScore). A case or a
 * load task whose rebuilt listing misses more than MAX_LISTING_CHANGE_PERCENT of its session's
 * recorded names is dropped, and so is one whose capture builds its listing from the rebuilt
 * profile and whose listing adds more names than that share of them (listingStanding); one whose
 * capture sends its session's recorded system prompt whole shows the model the recorded listing
 * whatever the profile installs, so it is not dropped for added names, and the runs that read a
 * skill outside that listing are counted, never scored (outsideListing).
 */
import { z } from "zod";
import { counted, parseJson } from "./common";

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
/** The confirms of one case the pooled rule takes (the architect's decision "The behaviour test's
 *  pass rule", 2026-11-22): where two confirms at the same texts on one runner disagree, one a
 *  loss and one not, a third runs and the three are pooled, a loss at LOSS_GAP fewer passes of
 *  each FRESH_RUNS (30 of 120). No confirm runs after the third. */
export const POOLED_CONFIRMS = 3;
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
/** The most a case's or a load task's rebuilt skill listing may miss of the names its session's
 *  recorded listing holds, and, where its capture builds its listing from the rebuilt profile,
 *  the most names it may add to them, each counted alone, in percent of the recorded names (the
 *  architect, 2026-11-24T23:52Z; 2026-11-25T00:52:35Z: a case or task whose capture sends the
 *  session's recorded system prompt whole is not counted for added names): one that misses more,
 *  or adds more, is dropped (listingStanding). */
export const MAX_LISTING_CHANGE_PERCENT = 10;
/** The evidence item of a case or a load task that holds its listing measure as JSON, under which
 *  build.ts writes its ListingMeasure. */
export const LISTING_MEASURE_ITEM = "listing measure";
/** How the drop of a case or a task whose listing misses too many names begins, also where it
 *  adds too many names as well. */
export const LISTING_MISSES = "listing-misses";
/** How the drop of a case or a task whose capture builds its listing from the rebuilt profile
 *  (a measure of kind `subagent listing`, or any measure where `capture.recordedPromptSent` is
 *  false, as for a Claude Code capture), and whose listing adds too many names and misses no more
 *  than MAX_LISTING_CHANGE_PERCENT of them, begins (listingStanding). A case or task whose capture
 *  sends its session's recorded system prompt whole is never dropped for added names. */
export const LISTING_ADDS = "listing-adds";

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
 *  passes); some arm can still start the runs it lacks, or holds a run that waits
 *  (FURTHER_ROUND_LIMIT), with how many to start per arm (`running`; none to start anywhere is a
 *  stage that only waits); or some arm lacks its runs and has used its extra runs
 *  (`unscreened`). */
export type StageOutcome<S extends Stage> =
  | { state: "counted"; passes: Passes<S> }
  | { state: "running"; toStart: Record<StageArm<S>, number> }
  | { state: "unscreened" };

/** How the reason begins of a run that waits for the next decision (cli.ts's `behaviour
 *  continue-mixed --further-rounds`, replay.ts's FurtherRoundLimit): a saved run continued as far
 *  as its further rounds allowed, whose reply would be continued again. It has no verdict, and no
 *  fresh run takes its place: it holds its slot until that decision, and its stage is not counted
 *  while it waits (`stageOutcome`). */
export const FURTHER_ROUND_LIMIT = "further-round-limit";

/** Where case `caseId` stands in `stage`, from the run records so far (StageOutcome). A run that
 *  waits (its reason begins FURTHER_ROUND_LIMIT) is counted among its arm's runs, as any run, but
 *  is not a run the extra-run rule replaces: it holds a slot of its arm's judged runs, and keeps
 *  its stage running. An arm with more judged runs than the stage runs is the runner's fault, and
 *  throws. */
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
    let waiting = 0;
    for (const record of records) {
      if (record.caseId !== caseId || record.stage !== stage || record.arm !== arm) continue;
      attempts++;
      if (record.verdict !== null) judged++;
      else if (record.reason.startsWith(FURTHER_ROUND_LIMIT)) waiting++;
      if (record.verdict === "pass") passed++;
    }
    if (judged > required)
      throw new Error(
        `case ${caseId}: the ${stage} arm ${arm} holds ${judged} judged runs, more than its ${required}`
      );
    const lacking = required - judged - waiting;
    const left = required + MAX_EXTRA_RUNS - attempts;
    if (lacking > 0 && left <= 0) return { state: "unscreened" };
    passes[arm] = passed;
    toStart[arm] = lacking > 0 ? Math.min(lacking, left) : 0;
    running ||= lacking > 0 || waiting > 0;
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

/** Whether a case's POOLED_CONFIRMS confirms, pooled, are a loss: the cut passes at least LOSS_GAP
 *  fewer of each FRESH_RUNS than the old text, so at least 30 fewer of the 120 pooled runs. */
export function isPooledLoss(passes: Passes<"confirm">): boolean {
  assertCounts(passes, FRESH_RUNS * POOLED_CONFIRMS);
  return passes.old - passes.new >= LOSS_GAP * POOLED_CONFIRMS;
}

/** One confirm of a case: the runner that made it (cli.ts's `runnerDigest`, a digest of its code;
 *  null for a confirm made before the runner was recorded), the texts it ran (the old and the cut
 *  text's digests in one string), and where its runs stand. */
export interface MadeConfirm {
  readonly runner: string | null;
  readonly texts: string;
  readonly outcome: StageOutcome<"confirm">;
}

/** Where a flagged case's confirm stands: runs to make, of its last confirm or of a third one that
 *  two disagreeing confirms call for (`running`, `third`); its last confirm unscreened; dropped
 *  before a confirm that counts, so it can never be confirmed (`dropped`, with the drop's
 *  reason); or counted, with the passes of each confirm that counts, their pooled passes when
 *  there are three, and whether it is a loss. */
export type ConfirmScore =
  | { state: "running"; third: boolean }
  | { state: "unscreened" }
  | { state: "dropped"; reason: string }
  | {
      state: "counted";
      loss: boolean;
      counted: Passes<"confirm">[];
      pooled: Passes<"confirm"> | undefined;
    };

/**
 * Where the confirms of case `caseId` stand, `confirms` oldest first. Only the confirms on the
 * runner and at the texts of the last one count: one made before a fix of the runner, or at other
 * texts, is not pooled with it, and the last one counts as it is. One confirm that counts is a
 * loss by isLoss, and two that agree are what both say; two that disagree call for a third
 * (`running`, `third`), and three are pooled (isPooledLoss). A last confirm still running, or
 * unscreened, is that. More than POOLED_CONFIRMS confirms that count throw, naming the case: the
 * rule runs no fourth.
 */
export function confirmScore(caseId: string, confirms: readonly MadeConfirm[]): ConfirmScore {
  const last = confirms.at(-1);
  if (last === undefined || last.outcome.state === "running") {
    return { state: "running", third: false };
  }
  if (last.outcome.state === "unscreened") return { state: "unscreened" };
  const counted = confirms.flatMap(({ runner, texts, outcome }) =>
    runner === last.runner && texts === last.texts && outcome.state === "counted"
      ? [outcome.passes]
      : []
  );
  if (counted.length > POOLED_CONFIRMS) {
    throw new Error(
      `case ${caseId} holds ${counted.length} confirms at the same texts on one runner: the pooled rule takes ${POOLED_CONFIRMS}, and no confirm runs after them`
    );
  }
  if (counted.length === POOLED_CONFIRMS) {
    const pooled = {
      old: counted.reduce((sum, each) => sum + each.old, 0),
      new: counted.reduce((sum, each) => sum + each.new, 0),
    };
    return { state: "counted", loss: isPooledLoss(pooled), counted, pooled };
  }
  const [first, second] = counted;
  if (first !== undefined && second !== undefined && isLoss(first) !== isLoss(second)) {
    return { state: "running", third: true };
  }
  return { state: "counted", loss: isLoss(last.outcome.passes), counted, pooled: undefined };
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

/** The informative cases of `cases` in the control rule's order: the one whose old text passes
 *  most more often than no skill first, a tie going to the lower id in string order. A skill's
 *  control case is the first of them that was not dropped (scoreSkill). */
export function controlOrder(
  cases: readonly { caseId: string; passes: Passes<"screen"> }[]
): string[] {
  return cases
    .flatMap(({ caseId, passes }) =>
      isInformative(passes) ? [{ caseId, gap: passes.old - passes.none }] : []
    )
    .sort((a, b) => b.gap - a.gap || (a.caseId < b.caseId ? -1 : a.caseId > b.caseId ? 1 : 0))
    .map((each) => each.caseId);
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

/** A case dropped at `stage`, and why. cli.ts writes why: a judge control failed, the gateway
 *  serves its model no more, a directory of its context could not be read, its listing misses
 *  too many names, a block of its skill its arms cannot each rebuild, a file it reads by path an
 *  arm's text holds in a form the arms do not rebuild, an arm's runs read skill files past the
 *  last answered round, or the provider refused, on a content policy, every replay with no verdict
 *  of each arm of a stage that lacks its judged runs. */
export interface CaseDrop {
  readonly stage: Stage;
  readonly reason: string;
}

/** What the run records of a case do not show: its drop, when it was dropped; and its confirms with
 *  the runner and texts of each (MadeConfirm), when it has any, which are then read in place of
 *  the confirm runs of the records. */
export interface CaseFacts {
  readonly drop?: CaseDrop;
  readonly confirms?: readonly MadeConfirm[];
}

/** One case of a skill: its drop, its screen stage, whether it is informative and flagged (both
 *  false unless its screen runs are counted and it was not dropped at screen), its confirms when
 *  flagged (confirmScore), and whether they are a counted loss. */
export interface CaseScore {
  caseId: string;
  drop: CaseDrop | undefined;
  screen: StageOutcome<"screen">;
  informative: boolean;
  flagged: boolean;
  confirm: ConfirmScore | undefined;
  loss: boolean;
}
/** A skill's cases and verdict, from the run records of its cases. `screened` counts the cases
 *  whose screen runs are counted and that were not dropped at screen (the case cap's count);
 *  `controlCase` is the first case of the control rule's order that was not dropped, and
 *  `replacedControls` the dropped ones before it in that order; `noControl` says why no case is
 *  left to be the control, when each informative case was dropped; `control` is the control
 *  case's control stage. `outcome` is undefined while a stage the verdict waits on can still start
 *  runs. */
export interface SkillScore {
  cases: CaseScore[];
  screened: number;
  informative: number;
  controlCase: string | undefined;
  replacedControls: string[];
  noControl: string | undefined;
  control: StageOutcome<"control"> | undefined;
  outcome: SkillOutcome | undefined;
}

/**
 * A skill's score from the run records of its cases and their `facts`, in the order their first
 * screen record comes, then each dropped case with no screen record. Unscreened cases count for
 * nothing. A case dropped at screen is not informative; a case dropped at confirm or control keeps
 * its screen, and its confirms made before the drop; a case recorded as dropped at confirm that
 * its screen did not flag cannot be, since only flagged cases are confirmed, and throws, naming
 * it. The control case is the first case of the control rule's order that was not dropped, so a
 * dropped one gives its place to the next; with none left, the skill gets no verdict, and the
 * reason names each dropped one.
 *
 * The verdict waits for every case's screen runs; with enough informative cases, for the control's
 * runs (its planted-loss and false-loss controls are one stage); and when the planted-loss control
 * is a loss and the false-loss control is not, for the confirms of every flagged case. An
 * unscreened control is no control loss. A flagged case whose confirm runs stay unscreened gives
 * no verdict, since its loss can be neither seen nor ruled out, and a `keeps` over it would hide
 * one. A flagged case dropped before a confirm that counts cannot be confirmed: the skill then
 * gets no `keeps` but no verdict naming that case, unless another case is a confirmed loss, which
 * stands as the skill's loss (the architect, 2026-11-22T01:28:33Z).
 */
export function scoreSkill(
  records: readonly RunRecord[],
  facts: ReadonlyMap<string, CaseFacts> = new Map()
): SkillScore {
  const ids = [
    ...new Set([
      ...records.filter((record) => record.stage === "screen").map((r) => r.caseId),
      ...[...facts].flatMap(([caseId, each]) => (each.drop === undefined ? [] : [caseId])),
    ]),
  ];
  const cases: CaseScore[] = ids.map((caseId) => {
    const drop = facts.get(caseId)?.drop;
    const screen = stageOutcome(records, caseId, "screen");
    const passes =
      screen.state === "counted" && drop?.stage !== "screen" ? screen.passes : undefined;
    const informative = passes !== undefined && isInformative(passes);
    const flagged = passes !== undefined && isFlagged(passes);
    if (drop?.stage === "confirm" && !flagged) {
      throw new Error(
        `case ${caseId} is recorded as dropped at confirm, but its screen did not flag it, and only flagged cases are confirmed: its records are refused, not scored (its drop: ${drop.reason})`
      );
    }
    let confirm: ConfirmScore | undefined;
    if (flagged) {
      const made = facts.get(caseId)?.confirms ?? [];
      confirm = confirmScore(
        caseId,
        made.length > 0
          ? made
          : [{ runner: null, texts: "", outcome: stageOutcome(records, caseId, "confirm") }]
      );
      if (drop !== undefined && confirm.state !== "counted") {
        confirm = { state: "dropped", reason: drop.reason };
      }
    }
    const loss = confirm?.state === "counted" && confirm.loss;
    return { caseId, drop, screen, informative, flagged, confirm, loss };
  });
  const screenedCases = cases.flatMap(({ caseId, screen, drop }) =>
    screen.state === "counted" && drop?.stage !== "screen"
      ? [{ caseId, passes: screen.passes }]
      : []
  );
  const informative = cases.filter((c) => c.informative).length;
  const replacedControls: string[] = [];
  let chosen: string | undefined;
  for (const caseId of controlOrder(screenedCases)) {
    if (facts.get(caseId)?.drop === undefined) {
      chosen = caseId;
      break;
    }
    replacedControls.push(caseId);
  }
  const noControl =
    chosen === undefined && replacedControls.length > 0
      ? `no informative case is left to be the control case: each is dropped (${replacedControls
          .map((caseId) => `${caseId} at ${facts.get(caseId)?.drop?.stage}`)
          .join(", ")})`
      : undefined;
  const control = chosen === undefined ? undefined : stageOutcome(records, chosen, "control");
  const score = {
    cases,
    screened: screenedCases.length,
    informative,
    controlCase: chosen,
    replacedControls,
    noControl,
    control,
  };
  const waiting = (outcome: { state: StageState | ConfirmScore["state"] } | undefined) =>
    outcome?.state === "running";
  if (cases.some((c) => c.drop?.stage !== "screen" && waiting(c.screen)))
    return { ...score, outcome: undefined };
  if (informative >= MIN_INFORMATIVE_CASES && noControl !== undefined)
    return { ...score, outcome: { verdict: null, reason: noControl } };
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
  const outcome = skillVerdict({ informative, controlLoss, falseLoss, losses });
  // A flagged case that could not be confirmed may hide a loss: no `keeps` over it.
  const unconfirmable = cases.flatMap((c) =>
    c.confirm?.state === "dropped"
      ? [`flagged case ${c.caseId} could not be confirmed (${c.confirm.reason})`]
      : []
  );
  if (outcome.verdict === "keeps" && unconfirmable.length > 0)
    return { ...score, outcome: { verdict: null, reason: unconfirmable.join("; ") } };
  return { ...score, outcome };
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

/** What the listing rule reads of a listing measure: build.ts's ListingMeasure, which writes it.
 *  build.ts always writes `added`; a measure with no `added` (this schema is loose) reads as one
 *  whose rebuilt listing adds no name. Its `kind` says whose listing the recorded one is: the
 *  session's own recorded system prompt (`session listing`), which the capture sends whole, or a
 *  subagent's (`subagent listing`), where the capture builds its listing from the profile. */
const ListingRead = z.looseObject({
  kind: z.enum(["session listing", "subagent listing", "not measurable"]),
  recorded: z.number().nullable(),
  missing: z.array(z.string()),
  added: z.array(z.string()).default([]),
  why: z.string(),
});

/** Where a case or a load task stands by the listing measure of its evidence: measured, with the
 *  names its session's recorded listing holds, how many of them its rebuilt listing misses and how
 *  many names it adds to them; not measurable, and why; or dropped, with the drop's reason. */
export type ListingStanding =
  | {
      readonly state: "measured";
      readonly recorded: number;
      readonly missing: number;
      readonly added: number;
    }
  | { readonly state: "not measurable"; readonly why: string }
  | { readonly state: "dropped"; readonly reason: string };

/** What a caller knows of a case's or task's capture beyond its listing measure. Claude Code's
 *  capture (a load test's `claude` host) lists the skills its plugins hold and sends no recorded
 *  system prompt, so for it `recordedPromptSent` is false whatever the measure's kind says; it is
 *  true, as an Oh My Pi capture's, when left out. */
export interface ListingCapture {
  readonly recordedPromptSent?: boolean;
}

/**
 * Where a case or a load task stands by the listing measure its `evidence` holds (the item
 * LISTING_MEASURE_ITEM). The rule counts each side of the listing alone, as a share of the names
 * its session's recorded listing holds, and keeps a share of exactly MAX_LISTING_CHANGE_PERCENT:
 *
 * - Missing names count for every case and task. A recorded name the rebuilt profile does not
 *   install is answered with Oh My Pi's unknown-skill reply when a run reads it, where the session
 *   got the skill's text. Over the share, the drop begins LISTING_MISSES.
 * - Added names count only where the capture builds its listing from the rebuilt profile: a
 *   measure of kind `subagent listing` (a top-level session, set against a subagent's recorded
 *   listing), or any measure when `capture.recordedPromptSent` is false. Over the share, the drop
 *   begins LISTING_ADDS; where the case or task is over the share on both sides, the reason
 *   begins LISTING_MISSES and goes on with the LISTING_ADDS part, so the names of both are on
 *   record.
 *
 * A measure of kind `session listing` is of a session that recorded its system prompt, and its
 * capture sends that prompt whole: the model in every arm sees exactly the recorded listing,
 * whatever the rebuilt profile installs, since the capture changes only the tested skill's line
 * of it per arm, however many names the profile lists. What the profile changes there is only how
 * a read of a skill is answered, from an installed copy or with the unknown-skill reply (replay.ts's
 * AnsweredRead `from`), which `outsideListing` counts and which drops nothing. The kind is the
 * capture's own choice, read from the measure: build.ts's `listingMeasure` writes `session
 * listing` exactly when it was given the session's recorded prompt (`input.recorded`, which each
 * caller that measures a listing takes from the profile's `systemPrompt`), and capture.ts's
 * `captureBody` sends the recorded blocks exactly when the profile holds a `systemPrompt`;
 * build.test.ts checks that over the cases a build writes. A task that keeps an old profile is
 * `not measurable` whatever the profile holds, and drops nothing.
 *
 * Not measurable when the measure says so, or when the evidence holds none or one this rule
 * cannot read; else measured.
 */
export function listingStanding(
  evidence: readonly { readonly item: string; readonly source: string }[] | undefined,
  capture: ListingCapture = {}
): ListingStanding {
  const item = evidence?.find((each) => each.item === LISTING_MEASURE_ITEM);
  if (item === undefined) {
    return { state: "not measurable", why: "its evidence holds no listing measure" };
  }
  const measure = ListingRead.safeParse(parseJson(item.source));
  if (!measure.success) {
    return { state: "not measurable", why: "its listing measure is not one build.ts writes" };
  }
  const { kind, missing, added } = measure.data;
  if (kind === "not measurable") {
    return { state: "not measurable", why: measure.data.why };
  }
  const recorded = measure.data.recorded ?? 0;
  // Each side counted alone, in one form; the share is of the recorded names, 100% where the
  // recording holds none. The added names count only where the listing is built from the profile.
  const sendsRecordedPrompt = kind === "session listing" && capture.recordedPromptSent !== false;
  const sides: (readonly [string, readonly string[], string])[] = [
    [LISTING_MISSES, missing, `misses ${missing.length} of`],
  ];
  if (!sendsRecordedPrompt) sides.push([LISTING_ADDS, added, `adds ${added.length} to`]);
  const parts = sides.flatMap(([start, names, says]) =>
    names.length * 100 <= MAX_LISTING_CHANGE_PERCENT * recorded
      ? []
      : [
          `${start}: its rebuilt listing ${says} the ${recorded} names of the recorded ${kind} (${(recorded === 0 ? 100 : (100 * names.length) / recorded).toFixed(1)}%), more than ${MAX_LISTING_CHANGE_PERCENT}%: ${names.join(", ")}`,
        ]
  );
  if (parts.length === 0) {
    return { state: "measured", recorded, missing: missing.length, added: added.length };
  }
  return { state: "dropped", reason: parts.join("; ") };
}

/** One read a run's answered rounds made, as `outsideListing` reads it: the skill (the name its
 *  profile lists it under, or the name a `skill://` URL gave) and where the answer came from
 *  (replay.ts's AnsweredRead: `arm` the arm's own text, `copy` an installed copy, `absent` Oh My
 *  Pi's unknown-skill reply). */
export interface AnsweredSkill {
  readonly skill: string;
  readonly from: "arm" | "copy" | "absent";
}

/** The runs of the cases or tasks whose capture sends their session's recorded system prompt
 *  whole, by what they read outside that prompt's listing (`outsideListing`): how many such cases
 *  or tasks (`groups`), their runs, those of them that record their reads (`examined`), those
 *  that read a skill the recorded listing lacks (`outside`), and for each such skill, by name,
 *  the runs that read it and were answered from an installed copy (`copy`) and with the
 *  unknown-skill reply (`absent`). A run that reads a skill in several rounds counts once for it. */
export interface OutsideListing {
  readonly groups: number;
  readonly runs: number;
  readonly examined: number;
  readonly outside: number;
  readonly bySkill: Readonly<Record<string, { readonly copy: number; readonly absent: number }>>;
}

/**
 * Counts, for `groups`, each a case or task whose capture sends its session's recorded system
 * prompt whole, the names `recorded` its listing holds and its `runs` (each run's reads, or
 * undefined for a run that records none: one saved before runs recorded their reads, or whose
 * replay failed), the runs that read a skill outside `recorded`. A read answered from the arm's
 * own text (`arm`: the tested skill) is no count, since the profile does not change its answer.
 * The count drops no case and changes no verdict (README.md, "behaviour"): it shows how far the
 * rebuilt profile's installed skills decided what the runs read, which the model's listing, the
 * recorded one, does not.
 */
export function outsideListing(
  groups: readonly {
    readonly recorded: ReadonlySet<string>;
    readonly runs: readonly (readonly AnsweredSkill[] | undefined)[];
  }[]
): OutsideListing {
  const bySkill = new Map<string, { copy: number; absent: number }>();
  let runs = 0;
  let examined = 0;
  let outside = 0;
  for (const group of groups) {
    for (const reads of group.runs) {
      runs += 1;
      if (reads === undefined) continue;
      examined += 1;
      const kinds = new Map<string, Set<"copy" | "absent">>();
      for (const { skill, from } of reads) {
        if (from === "arm" || group.recorded.has(skill)) continue;
        kinds.set(skill, (kinds.get(skill) ?? new Set<"copy" | "absent">()).add(from));
      }
      if (kinds.size > 0) outside += 1;
      for (const [skill, answers] of kinds) {
        const count = bySkill.get(skill) ?? { copy: 0, absent: 0 };
        for (const answer of answers) count[answer] += 1;
        bySkill.set(skill, count);
      }
    }
  }
  return {
    groups: groups.length,
    runs,
    examined,
    outside,
    bySkill: Object.fromEntries([...bySkill].toSorted(([a], [b]) => (a < b ? -1 : 1))),
  };
}

/** The line that reports an OutsideListing for `unit`s (`case` or `task`): how many of the runs
 *  that record their reads read a skill the recorded listing lacks, how many runs record none,
 *  and each such skill with its runs by answer kind. */
export function outsideListingWords(found: OutsideListing, unit: "case" | "task"): string {
  const skills = Object.entries(found.bySkill).map(
    ([skill, { copy, absent }]) => `${skill} (${copy} copy, ${absent} absent)`
  );
  const unread = found.runs - found.examined;
  return `outside recorded listing: ${found.outside} of ${found.examined} runs of ${counted(found.groups, `recorded-prompt ${unit}`)} (the capture sends the session's recorded system prompt whole) read a skill the recorded listing lacks${unread === 0 ? "" : `, not counting ${counted(unread, "run")} with no recorded reads`}; by skill and answer (copy: an installed copy; absent: the unknown-skill reply): ${skills.join(", ") || "none"}`;
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
