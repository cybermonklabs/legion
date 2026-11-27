// What a run's daemon may answer a completion with, and the one reader of the answers a run got.
// Three programs read it: the daemon stand-in, which answers each completion as the daemon of the
// run's label's checkout answers it (`LabelDaemon`); score.ts's `unscored`, which makes a
// completion refused with any other answer a rig error; and health.ts (`rig.sh health`), which
// calls such a run unhealthy and also holds a run to the completions its world must take
// (`ANSWERS`). Each reads the run's label (`labelOf`), its log (`daemonLines`) and the refusal
// (`refusedCompletion`) here, so the three cannot drift; how the label's daemon takes a completion
// each reads out of the label's own checkout (daemon-pane.ts's `labelDaemon`). It imports nothing
// of the scorer: the stand-in loads it at every boot.
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import { identityOf, recordFile } from "./run-names";
import { Opened, once } from "./run-records";

/** The daemon stand-in's log, under the run directory, which every stand-in of the run appends
 * to (rig.sh's worker_boot). */
export const DAEMON_LOG = "daemon.log";
/** The route a phase worker's `handoff_complete` posts to. */
export const COMPLETE_PATH = "/legion/v1/handoff/complete";
/** The daemon's refusal of a completion from a role that does not run the issue's phase. */
export const NOT_CURRENT_PHASE = "HANDOFF_NOT_CURRENT_PHASE";
/** The suffix of an entry of `ANSWERS` that allows its answer only as a repeat: after a
 * completion of the same role earlier in the same run was answered 200 (`allows`). */
const AFTER_TAKEN = " after 200";
/** The suffix of an answer allowed only after a completion of the same role earlier in the same
 * run was handed to the workflow: one it took (200) or one it refused (409, but the route's own
 * HANDOFF_RECORD_MISMATCH, which is refused before the workflow is handed anything). */
const AFTER_HANDED = " after one";
/** The answers a daemon gives a role's repeat of its own completion, in any world and of any
 * role: 409 HANDOFF_ALREADY_RECORDED for a call that repeats one the workflow was already
 * handed, whose fact is marked processed whether the workflow took it or refused it (intake's
 * `ApplyFact`; handoff.go's `eventID`), and 409 HANDOFF_NOT_NEW for a completion naming the
 * record the role's last taken completion named (engine.go's `handoff`). Each is production's
 * answer to what the agent sent again and no refusal of a world that should have taken it. */
const REPEATS = [
  `409 HANDOFF_ALREADY_RECORDED${AFTER_HANDED}`,
  `409 HANDOFF_NOT_NEW${AFTER_TAKEN}`,
];

/** One line of the daemon stand-in's log (daemon-standin.ts): what reached it, when, and what it
 * answered, with the refusal's code where the answer carried one; the role of the agent the
 * stand-in served; the request's method, which a log written before the stand-in logged one does
 * not name; a completion's line names the handoff record it reported, a backward move's the
 * phase it asked for, and a turn outcome's whether its answer failed the claim. The stand-in
 * writes beside them the reason that move gave and the phase a
 * completion was answered on, which no reader uses, so this names neither. The stand-in writes
 * `role` on every line; a reader keys a completion by it and by nothing else, so a completion
 * line without one is unkeyable (`unkeyed`) and fails closed, never given the role of the agent
 * booted in its place. */
export const DaemonLine = z.looseObject({
  at: z.string(),
  method: z.string().optional(),
  path: z.string(),
  status: z.number(),
  code: z.string().optional(),
  role: z.string().optional(),
  handoffId: z.string().optional(),
  handoffVersion: z.number().optional(),
  to: z.string().optional(),
  failed: z.boolean().optional(),
});
export type DaemonLine = z.infer<typeof DaemonLine>;

/** Every line of `<run>/daemon.log` but the opening lines, parsed; no line at all for a log that
 * is not there (score.ts's `unscored` makes a missing log a rig error where the score reads one).
 * A line that does not parse is the rig's fault. */
export function daemonLines(runDir: string): DaemonLine[] {
  const file = path.join(runDir, DAEMON_LOG);
  if (!existsSync(file)) return [];
  const records: DaemonLine[] = [];
  for (const [index, line] of readFileSync(file, "utf8")
    .split("\n")
    .filter((text) => text.trim() !== "")
    .entries()) {
    const value: unknown = JSON.parse(line);
    if (Opened.safeParse(value).success) continue;
    const result = DaemonLine.safeParse(value);
    if (!result.success)
      throw new Error(`${file}:${index + 1} is not a record: ${result.error.message}`);
    records.push(result.data);
  }
  return records;
}

/** Whether the agent output `file` is there and ends with the `exit=` line rig.sh's launch
 * appends once the agent has exited (`exit=timeout` once it waited no longer): its last line that
 * is not blank. */
export function exited(file: string): boolean {
  if (!existsSync(file)) return false;
  const last = readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .at(-1);
  return last?.startsWith("exit=") === true;
}

/** Whether the run finished as score.ts's `unscored` reads it: its out.txt, the last agent's
 * output, ended (`exited`). Health holds each role the run booted to its own output and
 * transcript instead (health.ts's `unfinished`). */
export function finished(runDir: string): boolean {
  return exited(path.join(runDir, "out.txt"));
}

/** How a daemon answers a completion from a role that does not run the issue's current phase (a
 * late role), in the two answers an engine (packages/daemon/internal/workflow) has had:
 *   - `recorded`: an engine whose `handoff` asks `leftItsPhase`, this checkout's (main's since
 *     #4611 asks it through `pastCompletion` and `pastReportOf`, daemon-completion.go's
 *     `recordingChains`). It records the completion on the role's own row and moves nothing when
 *     the daemon started the role on the issue and the role's row holds no completion
 *     (`row.Completed()`: no handoff commit and no handoff record a taken completion wrote). It
 *     takes the completion as one of the phase the
 *     role left (`phase.Of`), and that phase's record is required (`phase.Handoff(reported)`, a
 *     409 HANDOFF_RECORD_REQUIRED through `refused`). Any other late completion is refused 409
 *     HANDOFF_NOT_CURRENT_PHASE, a second one after the role's first was taken among them.
 *   - `refused`: an older daemon's engine, main `ff2a10ef`'s among them (`handoff`'s
 *     `if RoleFor(issue.Phase) != fact.Role { return refused("HANDOFF_NOT_CURRENT_PHASE", …) }`).
 *     It refuses every late completion 409 HANDOFF_NOT_CURRENT_PHASE, because it has no
 *     `leftItsPhase`.
 * Both write the late refusal through the same `refused()`. */
export const LATE_ROLES = ["recorded", "refused"] as const;
export type LateRole = (typeof LATE_ROLES)[number];

/** Which phase a daemon's completion route (packages/daemon/internal/api/handoff.go) holds a
 * completion's handoff record to, before the engine reads the completion, in the two shapes a
 * route has had:
 *   - `left`: this checkout's route (`of := phase.Of(grant.Role, issue.Phase)`), the phase the
 *     completion is of: the issue's when the role works it, and otherwise the phase the role
 *     left. A completion is record-backed when that phase ends with a record this role writes and
 *     it is the issue's phase or the completion names a record (`recordBacked`), so a late role
 *     naming the record of the phase it left is read back, and one naming none goes on to the
 *     engine. A completion naming a record of a phase that writes none is refused 409
 *     HANDOFF_RECORD_MISMATCH;
 *   - `issue`: an older daemon's route, main `ff2a10ef`'s among them (`phase.Handoff(issue.Phase)`,
 *     `recordBacked := handoffPhase && writer == grant.Role`), which holds a completion to the
 *     record of the phase the issue stands in: record-backed only in the role's own phase, and a
 *     completion naming a record while the issue stands in a phase that writes none is refused
 *     409 HANDOFF_RECORD_MISMATCH, a late reviewer's in `accepting` among them. */
export const RECORD_ROUTES = ["left", "issue"] as const;
export type RecordRoute = (typeof RECORD_ROUTES)[number];

/** How the daemon of a label takes a completion: its engine's `LateRole` and its route's
 * `RecordRoute`, read out of the checkout `rig.sh profile <label> <checkout>` built the label from
 * (the run's built-from; daemon-pane.ts's `labelDaemon`, daemon-completion.go), never from the
 * label's name. What a label is built from moves: the base label was main `ff2a10ef`, and is main
 * at the commit the head's newest forward merge brought in, which moves with every later forward
 * merge, so a table keyed by the name models a daemon its checkout may not have. */
export interface LabelDaemon {
  readonly lateRole: LateRole;
  readonly recordRoute: RecordRoute;
}

/** A daemon's `LabelDaemon` as one word pair, the key of `ANSWERS`' `byShape`. */
export type DaemonShape = `${LateRole} ${RecordRoute}`;

/** The labels a run with no record is named by: rig.sh's cmd_run named a run
 * `<scenario>[-<case>]-<label>-<n>` before the names left the world, and the rig's comparison
 * labels were these two (`rig.sh batch <scenario> <n> head base`). Names alone, which `labelOf`
 * reads such a name by and health.ts counts among the words of the rig's vocabulary: what each
 * label's daemon answers is read out of its checkout (`LabelDaemon`), never out of this list. */
export const LABELS: readonly string[] = ["head", "base"];

/** The label of the run under `runDir`: the label its record names (run-names.ts's
 * `runIdentity`). A run recorded before the names left the world has no record, and its label is
 * the one of `LABELS` its directory's name ends with before its number, which is how cmd_run
 * named a run then (`<scenario>[-<case>]-<label>-<n>`). Health and the scorer read it here, to
 * name the label; how its daemon takes a completion they read out of its checkout. Throws for a
 * record that is no run's, and for a name that ends with no label of `LABELS`. */
export function labelOf(runDir: string): string {
  const identity = existsSync(recordFile(runDir)) ? identityOf(runDir) : undefined;
  if (identity !== undefined) return identity.label;
  const name = path.basename(runDir);
  const label = LABELS.filter((named) => new RegExp(`-${named}-\\d+$`).test(name)).sort(
    (a, b) => b.length - a.length
  )[0];
  if (label === undefined)
    throw new Error(
      `${name} names no label a run with no record is named by (completion-answers.ts's LABELS: ${LABELS.join(", ")})`
    );
  return label;
}

/** An answer as a log line carries it: the status, then the code when the answer has one. */
export function answerOf(line: Pick<DaemonLine, "status" | "code">): string {
  return line.code === undefined ? String(line.status) : `${line.status} ${line.code}`;
}

/** Whether `allowed` (an entry of `ANSWERS`, or `REPEATS`) allows the answer of `lines[index]`,
 * `lines` being the run's log in the order it was written: the answer alone; the answer followed
 * by `AFTER_TAKEN` when a completion of the same role on an earlier line was answered 200; or
 * the answer followed by `AFTER_HANDED` when one on an earlier line was handed to the workflow. */
function allows(allowed: readonly string[], lines: readonly DaemonLine[], index: number): boolean {
  const line = lines[index];
  if (line === undefined) return false;
  const answer = answerOf(line);
  if (allowed.includes(answer)) return true;
  const after = [AFTER_TAKEN, AFTER_HANDED].find((suffix) =>
    allowed.includes(`${answer}${suffix}`)
  );
  if (after === undefined) return false;
  return lines
    .slice(0, index)
    .some(
      (earlier) =>
        earlier.path === COMPLETE_PATH &&
        earlier.role === line.role &&
        (earlier.status === 200 ||
          (after === AFTER_HANDED &&
            earlier.status === 409 &&
            earlier.code !== "HANDOFF_RECORD_MISMATCH"))
    );
}

/** What one scenario's world takes, for each role the scenario boots:
 *   - `boots`, the roles rig.sh boots, in the order it boots them: the roles a completion line of
 *     the scenario's log can name. A line naming another role, or none, is unkeyable (`unkeyed`);
 *   - `answers`, for each role, every answer a daemon of either shape (`LabelDaemon`) may give
 *     that role's completion in this world, and `byShape`, for each `DaemonShape` and role, the
 *     answers a daemon of that shape may give it beyond those: `<status> <code>`, or `<status>
 *     <code> after 200` (`AFTER_TAKEN`) for an answer the daemon gives a repeat alone, held to
 *     the run's log: a completion of the same role was answered 200 on an earlier line
 *     (`allows`). A 4xx answer in neither is a refusal the world should not have given
 *     (`refusedCompletion`). The run's daemon is read out of its label's checkout only for an
 *     answer outside `answers`, the one place its shape decides anything;
 *   - `takes`, by the run's case ("" for a scenario that takes none), the role whose completion
 *     the world must take, at least once, with an answer its entries allow (health's third rule).
 *     A case `takes` does not name, and a scenario with no `takes`, requires no completion.
 * Health judges what the world can accept, never what an agent should do: no entry counts the
 * completions an agent makes or forbids one.
 *
 * reviewer-text-findings boots the reviewer in `reviewing`, where its completion is of its own
 * phase and is taken 200. Its review moves the issue (the stand-in's `roundOutcome`), and a
 * completion after that is a late one: a daemon of the `refused` engine refuses it 409
 * HANDOFF_NOT_CURRENT_PHASE, as it refuses every late one, its route having let it through, since
 * that older checkout's plugin names no record for a phase the issue has left. A `left` route
 * reads back the reviewing record a late reviewer names, which this checkout's plugin names for a
 * late role: the record of the phase the role left (packages/pi-shared/src/handoff-record.ts,
 * `findHandoffReport`). A `recorded` engine then records that completion 200 when the reviewer's
 * row holds no completion, and refuses it 409 HANDOFF_NOT_CURRENT_PHASE when it does: a
 * completion of the reviewer's was taken before it, since every taken completion writes the row
 * (`row.Completed()`). So that daemon's 409 HANDOFF_NOT_CURRENT_PHASE to the reviewer is its own
 * answer after an earlier 200 alone; with none before it, the world refused the reviewer's first
 * completion, which no world of this rig should. A reviewer refused HANDOFF_RECORD_REQUIRED (a
 * plugin that names no record once the issue has left `reviewing`) is a refusal this table does
 * not allow either. The review reaches `implementing` on a change request, and the approval of
 * the head leaves it in `reviewing`: GitHub reports that head `BLOCKED` while a thread is open
 * (rig.sh's review_world), and the workflow's thread the review App cannot push a fix for stays
 * open. The reviewer's completion is required in defect alone, whose change request ends its
 * round. A text-only reviewer that pushed its own text fix records no pass at it, approves
 * nothing and waits for the implementer's last chance (spec v36's R1, steps 2 and 3, and
 * Verification v47's k1), so it completes nothing while it waits. A last-push reviewer completes
 * once it has approved the head as it stands, but one that posts a `COMMENT` review and waits
 * completes nothing: what it does there is k3's measurement, and a world that voided such a run
 * as unhealthy would leave only the runs that completed to be scored, so the world requires no
 * completion of it either.
 *
 * threads-closed boots the implementer in `reviewing` (accept, correct and crossing, whose
 * reviewer pushed its own text commit and has not approved yet) or `accepting` (code-defect, whose
 * reviewer approved and pushed nothing), and then the reviewer in that phase where the case has
 * a second half (rig.sh's run_threads_closed, `SKIPS`). The world's seeds stand for a completion
 * of each role (seed-handoffs.ts: the implement record, and the review record at the head the
 * reviewer left, its own commit in accept, correct and crossing, where it has not approved, and
 * the head it approved in code-defect, are on those roles' rows, daemon-standin.ts's `HANDOFFS`),
 * so a late completion of either is one from a role whose row holds a completion, which a daemon
 * of either shape refuses 409 HANDOFF_NOT_CURRENT_PHASE: the `refused` engine as it refuses every
 * late one, the `recorded` engine by `leftItsPhase`'s `row.Completed()`. That is the
 * implementer's in either phase, and the reviewer's once its review in correct has moved the
 * issue, or from its boot in `accepting` in code-defect. Until the stand-in read the seeded rows,
 * a `recorded` daemon's stand-in took such a reviewer's first completion 200 (3 of round 25's
 * runs, in `accepting`), a success production gives none of.
 * The reviewer's completion in `reviewing` is of its own phase and is taken 200. Either role
 * may also be refused 403 HANDOFF_SESSION_MISMATCH: its seeded record was written by another
 * session than the one the agent registered (the seed's), so a completion naming it is one
 * naming a record another session wrote, as the completion route of a `left` daemon reads the
 * record a late role names (this checkout's plugin names the newest record of the phase the
 * role left where the agent wrote none: packages/pi-shared/src/handoff-record.ts). The route
 * refuses no completion 409 HANDOFF_RECORD_MISMATCH in this rig: the phase a completion is of
 * writes a record, and the record of another issue, phase or version is one no world of this
 * rig gives. So that code is a broken world in any phase. HANDOFF_STALE_ASSIGNMENT is left out:
 * the stand-in models no assignment, so no run of this rig can get it. A role's repeat of its
 * own completion is answered as `REPEATS` says, in every scenario. A daemon of either shape
 * pairs this checkout's two (`recorded left`) or an older daemon's two (`refused issue`); one
 * that pairs either with the other's has no answers here, and a run of it is read as no world
 * this rig knows (`answered`). */
export const ANSWERS: Readonly<
  Record<
    string,
    {
      readonly boots: readonly string[];
      readonly answers: Readonly<Record<string, readonly string[]>>;
      readonly byShape: Readonly<
        Partial<Record<DaemonShape, Readonly<Record<string, readonly string[]>>>>
      >;
      readonly takes?: Readonly<Record<string, string>>;
    }
  >
> = {
  "reviewer-text-findings": {
    boots: ["reviewer"],
    answers: { reviewer: ["200"] },
    byShape: {
      "recorded left": { reviewer: [`409 ${NOT_CURRENT_PHASE}${AFTER_TAKEN}`] },
      "refused issue": { reviewer: [`409 ${NOT_CURRENT_PHASE}`] },
    },
    takes: { defect: "reviewer" },
  },
  "threads-closed": {
    boots: ["implementer", "reviewer"],
    answers: {
      implementer: [`409 ${NOT_CURRENT_PHASE}`, "403 HANDOFF_SESSION_MISMATCH"],
      reviewer: ["200", `409 ${NOT_CURRENT_PHASE}`, "403 HANDOFF_SESSION_MISMATCH"],
    },
    byShape: { "recorded left": {}, "refused issue": {} },
  },
};

/** The run's record of each role of its scenario's `boots` (ANSWERS), <run>/booted, which rig.sh
 * writes at the site that decides each one: `boot <role>` before the role's daemon stand-in
 * starts, once per attempt (a relaunch writes it again), and `skip <role> <reason>` where the rig
 * decides not to boot it, the reason one SKIPS declares for the run's scenario and case. Health's
 * finished rule reads it (`unfinished`). */
export const BOOTED = "booted";

/** Why a run may leave a role of its scenario's `boots` unbooted, keyed scenario → case → role:
 * the one reason rig.sh writes verbatim on that role's `skip` line of <run>/booted, and the one
 * health accepts there. threads-closed declares three: in accept the reviewer's commit is right
 * and it left no thread, and in crossing the same commit stands at a check its earlier pass does
 * not hold at, so in both the implementer's last chance is the run's one half and no reviewer is
 * booted; in code-defect the implementer tells the architect and the reviewer of the thread
 * (Verification v47's k2 negative control), so a wake of none is a run that has already failed
 * that message, and rig.sh boots no reviewer on it (run_threads_closed). */
export const SKIPS: Readonly<
  Record<string, Readonly<Record<string, Readonly<Record<string, string>>>>>
> = {
  "threads-closed": {
    accept: { reviewer: "the accept case scores the implementer's answer alone" },
    crossing: { reviewer: "the crossing case scores the implementer's answer alone" },
    "code-defect": {
      reviewer: "the implementer sent the reviewer nothing, which fails the message the case asks",
    },
  },
};

/** The scenario of `ANSWERS` the run is of: the one its record names (`runIdentity`), or, for a
 * run recorded before the names left the world, the one its directory's name starts with;
 * undefined for a run of a scenario the table does not name. */
export function scenarioOf(runDir: string): string | undefined {
  if (existsSync(recordFile(runDir))) {
    const { scenario } = identityOf(runDir);
    return ANSWERS[scenario] === undefined ? undefined : scenario;
  }
  const name = path.basename(runDir);
  return Object.keys(ANSWERS).find((scenario) => name.startsWith(`${scenario}-`));
}

/** The role the completion line `line` was the completion of, or why it cannot be keyed to one:
 * it names no role, or, in a scenario `ANSWERS` names, a role rig.sh boots none of there
 * (`boots`). The role the line names is the only key: a line that names none is never given the
 * role of the agent booted in its place, so it fails closed, where that guess read a 409
 * HANDOFF_NOT_CURRENT_PHASE with no role as the implementer's, the one role that answer is
 * allowed to. */
function keyOf(
  line: DaemonLine,
  scenario: string
): { readonly role: string } | { readonly unkeyed: string } {
  if (line.role === undefined) return { unkeyed: "a completion line names no role" };
  const boots = ANSWERS[scenario]?.boots;
  if (boots !== undefined && !boots.includes(line.role))
    return {
      unkeyed: `a completion line names the ${line.role}, a role ${scenario} boots none of`,
    };
  return { role: line.role };
}

/** Whether `scenario`'s world may answer `lines[index]`, a completion of `role`, as the line says
 * it was answered: with an answer a daemon of either shape gives it there (`ANSWERS`' `answers`),
 * or else with one the run's own daemon gives it (`byShape`), `daemon` (how the run's label's
 * daemon takes a completion) read only then. A scenario `ANSWERS` does not name boots one role in
 * that role's own phase, which a daemon of either shape answers 200. Throws what reading `daemon`
 * threw, and for a daemon of a shape `ANSWERS` gives no answers in a scenario it names. */
function answered(
  scenario: string,
  role: string,
  lines: readonly DaemonLine[],
  index: number,
  daemon: () => LabelDaemon
): boolean {
  if (allows(REPEATS, lines, index)) return true;
  const entry = ANSWERS[scenario];
  if (entry === undefined) return allows(["200"], lines, index);
  if (allows(entry.answers[role] ?? [], lines, index)) return true;
  const { lateRole, recordRoute } = daemon();
  const shape: DaemonShape = `${lateRole} ${recordRoute}`;
  const byRole = entry.byShape[shape];
  if (byRole === undefined)
    throw new Error(
      `a daemon of the shape \`${shape}\` (its engine's late role, then its route's record) has no answers for ${scenario}'s completions (completion-answers.ts's ANSWERS)`
    );
  return allows(byRole[role] ?? [], lines, index);
}

/** The first completion of the run that cannot be keyed to a role (`keyOf`), whatever it was
 * answered, with why; or the first refused 4xx with an answer its world does not allow the role it
 * names after the lines before it (`answered`), with that role. `lines` is the run's log in the
 * order it was written, and `daemon` how the run's label's daemon takes a completion, read once
 * and only for an answer that turns on it. This one reader serves the scorer's refused-completion
 * rule (score.ts's `unscored`) and health's second rule (health.ts). */
export function refusedCompletion(
  lines: readonly DaemonLine[],
  scenario: string,
  daemon: () => LabelDaemon
):
  | { readonly line: DaemonLine; readonly unkeyed: string }
  | { readonly line: DaemonLine; readonly role: string }
  | undefined {
  const shaped = once(daemon);
  for (const [index, line] of lines.entries()) {
    if (line.path !== COMPLETE_PATH) continue;
    const key = keyOf(line, scenario);
    if ("unkeyed" in key) return { line, unkeyed: key.unkeyed };
    if (line.status < 400 || line.status >= 500) continue;
    if (!answered(scenario, key.role, lines, index, shaped)) return { line, role: key.role };
  }
  return undefined;
}

/** The completions of the role the scenario's world must take in the run's case `runCase`
 * (`takes`) that were answered with an answer its world allows (`answered`, `daemon` read as
 * `refusedCompletion` reads it); undefined for a scenario and case that require none. */
export function takenCompletions(
  lines: readonly DaemonLine[],
  scenario: string,
  runCase: string,
  daemon: () => LabelDaemon
): { readonly role: string; readonly count: number } | undefined {
  const role = ANSWERS[scenario]?.takes?.[runCase];
  if (role === undefined) return undefined;
  const shaped = once(daemon);
  const count = lines.filter(
    (line, index) =>
      line.path === COMPLETE_PATH &&
      line.role === role &&
      answered(scenario, role, lines, index, shaped)
  ).length;
  return { role, count };
}
