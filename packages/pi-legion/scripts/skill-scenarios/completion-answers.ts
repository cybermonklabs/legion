// What a run's daemon may answer a completion with, and the one reader of the answers a run got.
// Three programs read it: the daemon stand-in, which answers each completion as the run's label's
// daemon answers it (`LABEL_DAEMONS`); score.ts's `unscored`, which makes a completion refused
// with any other answer a rig error; and health.ts (`rig.sh health`), which calls such a run
// unhealthy and also holds a run to the completions its world must take (`ANSWERS`). Each reads
// the run's label (`labelOf`), its log (`daemonLines`) and the refusal (`refusedCompletion`)
// here, so the three cannot drift. It imports nothing of the scorer: the stand-in loads it at
// every boot.
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import { Opened } from "./run-records";

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

/** Whether the run finished: its out.txt ends with the `exit=` line rig.sh's launch writes when
 * the agent exits. */
export function finished(runDir: string): boolean {
  const out = path.join(runDir, "out.txt");
  if (!existsSync(out)) return false;
  const last = readFileSync(out, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .at(-1);
  return last?.startsWith("exit=") === true;
}

/** How a label's daemon answers a completion from a role that does not run the issue's current
 * phase (a late role):
 *   - `recorded`: this checkout's engine (packages/daemon/internal/workflow/engine.go, `handoff`
 *     and `leftItsPhase`). It records the completion on the role's own row and moves nothing when
 *     the daemon started the role on the issue and the role's row holds no completion
 *     (`row.Completed()`: no handoff commit and no handoff record a taken completion wrote). It
 *     takes the completion as one of the phase the role left (`phase.Of`), and that phase's
 *     record is required (`phase.Handoff(reported)`, a 409 HANDOFF_RECORD_REQUIRED through
 *     `refused`). Any other late completion is refused 409 HANDOFF_NOT_CURRENT_PHASE, a second
 *     one after the role's first was taken among them.
 *   - `refused`: main `ff2a10ef`'s engine (`handoff`'s
 *     `if RoleFor(issue.Phase) != fact.Role { return refused("HANDOFF_NOT_CURRENT_PHASE", …) }`).
 *     It refuses every late completion 409 HANDOFF_NOT_CURRENT_PHASE, because it has no
 *     `leftItsPhase`.
 * Both write the late refusal through the same `refused()`. */
export type LateRole = "recorded" | "refused";

/** Which phase a label's completion route (packages/daemon/internal/api/handoff.go) holds a
 * completion's handoff record to, before the engine reads the completion:
 *   - `left`: this checkout's route (`of := phase.Of(grant.Role, issue.Phase)`), the phase the
 *     completion is of: the issue's when the role works it, and otherwise the phase the role
 *     left. A completion is record-backed when that phase ends with a record this role writes and
 *     it is the issue's phase or the completion names a record (`recordBacked`), so a late role
 *     naming the record of the phase it left is read back, and one naming none goes on to the
 *     engine. A completion naming a record of a phase that writes none is refused 409
 *     HANDOFF_RECORD_MISMATCH;
 *   - `issue`: main `ff2a10ef`'s route (`phase.Handoff(issue.Phase)`, `recordBacked :=
 *     handoffPhase && writer == grant.Role`), which holds a completion to the record of the
 *     phase the issue stands in: record-backed only in the role's own phase, and a completion
 *     naming a record while the issue stands in a phase that writes none is refused 409
 *     HANDOFF_RECORD_MISMATCH, a late reviewer's in `accepting` among them. */
export type RecordRoute = "left" | "issue";

/** The labels the rig builds and the daemon each one runs, keyed by the label's name as
 * `rig.sh profile <label> <checkout>` names it. A label not here has no daemon this rig can model:
 * the stand-in refuses to boot for it, and health and the scorer refuse to read its runs. A new
 * label whose daemon answers some other way states that here. */
export const LABEL_DAEMONS: Readonly<
  Record<string, { readonly lateRole: LateRole; readonly recordRoute: RecordRoute }>
> = {
  /** This checkout. */
  head: { lateRole: "recorded", recordRoute: "left" },
  /** Built from main `ff2a10ef` (its `built-from`). */
  base: { lateRole: "refused", recordRoute: "issue" },
};

/** The label of the run under `runDir`: the label of `LABEL_DAEMONS` that the run directory's
 * name ends with before its number. That is how rig.sh's cmd_run names a run
 * (`<scenario>[-<case>]-<label>-<n>`) and how score.ts reads its label. The stand-in and health
 * both read it here. Throws for a name that ends with no such label. */
export function labelOf(runDir: string): string {
  const name = path.basename(runDir);
  const label = Object.keys(LABEL_DAEMONS)
    .filter((named) => new RegExp(`-${named}-\\d+$`).test(name))
    .sort((a, b) => b.length - a.length)[0];
  if (label === undefined)
    throw new Error(
      `${name} names no label this rig models a daemon for (completion-answers.ts's LABEL_DAEMONS: ${Object.keys(LABEL_DAEMONS).join(", ")})`
    );
  return label;
}

/** An answer as a log line carries it: the status, then the code when the answer has one. */
export function answerOf(line: Pick<DaemonLine, "status" | "code">): string {
  return line.code === undefined ? String(line.status) : `${line.status} ${line.code}`;
}

/** Whether `allowed` (an entry of `ANSWERS`) allows the answer of `lines[index]`, `lines` being
 * the run's log in the order it was written: the answer alone, or the answer followed by
 * `AFTER_TAKEN` when a completion of the same role on an earlier line was answered 200. */
function allows(allowed: readonly string[], lines: readonly DaemonLine[], index: number): boolean {
  const line = lines[index];
  if (line === undefined) return false;
  const answer = answerOf(line);
  if (allowed.includes(answer)) return true;
  if (!allowed.includes(`${answer}${AFTER_TAKEN}`)) return false;
  for (let at = 0; at < index; at += 1) {
    const earlier = lines[at];
    if (earlier?.path === COMPLETE_PATH && earlier.role === line.role && earlier.status === 200)
      return true;
  }
  return false;
}

/** What one scenario's world takes, for each label and each role the scenario boots:
 *   - `boots`, the roles rig.sh boots, in the order it boots them: the roles a completion line of
 *     the scenario's log can name. A line naming another role, or none, is unkeyable (`unkeyed`);
 *   - `answers`, for each label and role, every answer that label's daemon may give that role's
 *     completion in this world: `<status> <code>`, or `<status> <code> after 200` (`AFTER_TAKEN`)
 *     for an answer the daemon gives a repeat alone, held to the run's log: a completion of the
 *     same role was answered 200 on an earlier line (`allows`). A 4xx answer not in the entry is
 *     a refusal the world should not have given (`refusedCompletion`);
 *   - `takes`, the role whose completion the world must take, at least once, with an answer in
 *     its entry (health's third rule). A scenario with no `takes` requires no completion.
 * Health judges what the world can accept, never what an agent should do: no entry counts the
 * completions an agent makes or forbids one.
 *
 * reviewer-text-findings boots the reviewer in `reviewing`, where its completion is of its own
 * phase and is taken 200. Its review moves the issue (the stand-in's `roundOutcome`), and a
 * completion after that is a late one: `ff2a10ef` refuses it 409 HANDOFF_NOT_CURRENT_PHASE, which
 * is the base daemon taking the completion as it takes every late one. This checkout's route
 * reads back the reviewing record a late reviewer names, which the head label's plugin names for
 * a late role: the record of the phase the role left (packages/pi-shared/src/handoff-record.ts,
 * `findHandoffReport`). Its engine then records that completion 200 when the reviewer's row
 * holds no completion, and refuses it 409 HANDOFF_NOT_CURRENT_PHASE when it does: a completion
 * of the reviewer's was taken before it, since every taken completion writes the row
 * (`row.Completed()`). So the head reviewer's 409 HANDOFF_NOT_CURRENT_PHASE is the head daemon's
 * own answer after an earlier 200 alone; with none before it, the world refused the reviewer's
 * first completion, which no world of this rig should. A head reviewer refused
 * HANDOFF_RECORD_REQUIRED (a plugin that names no record once the issue has left `reviewing`) is
 * a refusal this table does not allow either. The review reaches `implementing` and never
 * `accepting`: GitHub reports the world's head `BLOCKED` (rig.sh's review_world), so only a
 * reviewer that pushes a head of its own, which is the world's own failure, and approves it could
 * move the issue there.
 *
 * threads-closed boots the implementer, then the reviewer, in `reviewing` (text-fixes) or
 * `accepting` (code-defect) (rig.sh's run_threads_closed). The implementer's implementing round
 * already stands on its row, so either daemon refuses its completion there 409
 * HANDOFF_NOT_CURRENT_PHASE, the one deliberate refusal of the implementer. The reviewer's
 * completion is of its own phase in `reviewing` and is taken 200. Once the round has moved it is
 * late, which the base daemon refuses 409 HANDOFF_NOT_CURRENT_PHASE. A late head reviewer stands
 * in `accepting`: one that completes after its approval of the fix head moved text-fixes there, or
 * a code-defect reviewer, which is booted there. Its completion names its review record, which
 * this checkout's route reads back (`LABEL_DAEMONS`' `left`) and its engine answers as in
 * reviewer-text-findings: 200, or 409 HANDOFF_NOT_CURRENT_PHASE after an earlier 200 of the
 * reviewer's. The head route refuses no reviewer 409 HANDOFF_RECORD_MISMATCH in this rig: the
 * phase a reviewer's completion is of is `reviewing`, which writes a record, and the record of
 * another issue, phase or version that the route also refuses so (`handoffRecord`) is one no
 * world of this rig gives. So that code is a broken world in any phase. HANDOFF_STALE_ASSIGNMENT,
 * HANDOFF_ALREADY_RECORDED and HANDOFF_NOT_NEW are left out: the stand-in models no assignment,
 * no duplicate key and no earlier record of a role, so no run of this rig can get them. */
export const ANSWERS: Readonly<
  Record<
    string,
    {
      readonly boots: readonly string[];
      readonly answers: Readonly<Record<string, Readonly<Record<string, readonly string[]>>>>;
      readonly takes?: string;
    }
  >
> = {
  "reviewer-text-findings": {
    boots: ["reviewer"],
    answers: {
      head: { reviewer: ["200", `409 ${NOT_CURRENT_PHASE}${AFTER_TAKEN}`] },
      base: { reviewer: ["200", `409 ${NOT_CURRENT_PHASE}`] },
    },
    takes: "reviewer",
  },
  "threads-closed": {
    boots: ["implementer", "reviewer"],
    answers: {
      head: {
        implementer: [`409 ${NOT_CURRENT_PHASE}`],
        reviewer: ["200", `409 ${NOT_CURRENT_PHASE}${AFTER_TAKEN}`],
      },
      base: {
        implementer: [`409 ${NOT_CURRENT_PHASE}`],
        reviewer: ["200", `409 ${NOT_CURRENT_PHASE}`],
      },
    },
  },
};

/** The scenario of `ANSWERS` the run directory's name starts with, which is how score.ts reads
 * a run's scenario; undefined for a run of a scenario the table does not name. */
export function scenarioOf(runDir: string): string | undefined {
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

/** Every answer `label`'s daemon may give `role`'s completion in `scenario`'s world. A scenario
 * `ANSWERS` does not name boots one role in that role's own phase, which either daemon answers
 * 200. Throws for a label `ANSWERS` gives no entry in a scenario it names. */
export function allowedAnswers(scenario: string, label: string, role: string): readonly string[] {
  const entry = ANSWERS[scenario];
  if (entry === undefined) return ["200"];
  const byRole = entry.answers[label];
  if (byRole === undefined)
    throw new Error(
      `the ${label} label has no answers for ${scenario}'s completions (completion-answers.ts's ANSWERS)`
    );
  return byRole[role] ?? [];
}

/** The first completion of the run that cannot be keyed to a role (`keyOf`), whatever it was
 * answered, with why; or the first refused 4xx with an answer its world's entry does not allow
 * the role it names after the lines before it, with that role. `lines` is the run's log in the
 * order it was written. This one reader serves the scorer's refused-completion rule (score.ts's
 * `unscored`) and health's second rule (health.ts). */
export function refusedCompletion(
  lines: readonly DaemonLine[],
  scenario: string,
  label: string
):
  | { readonly line: DaemonLine; readonly unkeyed: string }
  | { readonly line: DaemonLine; readonly role: string }
  | undefined {
  for (const [index, line] of lines.entries()) {
    if (line.path !== COMPLETE_PATH) continue;
    const key = keyOf(line, scenario);
    if ("unkeyed" in key) return { line, unkeyed: key.unkeyed };
    if (line.status < 400 || line.status >= 500) continue;
    if (!allows(allowedAnswers(scenario, label, key.role), lines, index))
      return { line, role: key.role };
  }
  return undefined;
}

/** The completions of the role the scenario's world must take (`takes`) that were answered with
 * an answer its entry allows; undefined for a scenario that requires none. */
export function takenCompletions(
  lines: readonly DaemonLine[],
  scenario: string,
  label: string
): { readonly role: string; readonly count: number } | undefined {
  const role = ANSWERS[scenario]?.takes;
  if (role === undefined) return undefined;
  const allowed = allowedAnswers(scenario, label, role);
  const count = lines.filter(
    (line, index) =>
      line.path === COMPLETE_PATH && line.role === role && allows(allowed, lines, index)
  ).length;
  return { role, count };
}
