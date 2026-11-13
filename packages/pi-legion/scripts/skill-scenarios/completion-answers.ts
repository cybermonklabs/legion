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

/** One line of the daemon stand-in's log (daemon-standin.ts): what reached it, when, and what it
 * answered, with the refusal's code where the answer carried one; the role of the agent the
 * stand-in served; a completion's line names the handoff record it reported, and a backward
 * move's the phase it asked for. The stand-in writes the reason that move gave beside it, which
 * no reader uses, so this does not name it. A log written before the stand-in named the role has
 * no `role` (`roleOf`). */
export const DaemonLine = z.looseObject({
  at: z.string(),
  path: z.string(),
  status: z.number(),
  code: z.string().optional(),
  role: z.string().optional(),
  handoffId: z.string().optional(),
  handoffVersion: z.number().optional(),
  to: z.string().optional(),
});
export type DaemonLine = z.infer<typeof DaemonLine>;
/** A line of the log with the agent whose stand-in wrote it: `boot` counts the stand-ins that
 * opened the log before this line, from 0. Each stand-in of a run opens the log with its own line
 * (run-records.ts's `openingLine`) when it starts, and rig.sh boots a run's agents one after the
 * other, so the n-th opening line starts the n-th agent's lines. */
export type LoggedLine = DaemonLine & { readonly boot: number };

/** Every line of `<run>/daemon.log` but the opening lines, parsed, each with the boot it belongs
 * to; no line at all for a log that is not there (score.ts's `unscored` makes a missing log a
 * rig error where the score reads one). A line that does not parse is the rig's fault. */
export function daemonLines(runDir: string): LoggedLine[] {
  const file = path.join(runDir, DAEMON_LOG);
  if (!existsSync(file)) return [];
  const records: LoggedLine[] = [];
  let opened = 0;
  for (const [index, line] of readFileSync(file, "utf8")
    .split("\n")
    .filter((text) => text.trim() !== "")
    .entries()) {
    const value: unknown = JSON.parse(line);
    if (Opened.safeParse(value).success) {
      opened += 1;
      continue;
    }
    const result = DaemonLine.safeParse(value);
    if (!result.success)
      throw new Error(`${file}:${index + 1} is not a record: ${result.error.message}`);
    records.push({ ...result.data, boot: Math.max(0, opened - 1) });
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
 *     the daemon started the role on the issue and the role's row holds no handoff commit. It
 *     takes the completion as one of the phase the role left (`leftPhase`), and that phase's
 *     record is required (`phase.Handoff(reported)`, a 409 HANDOFF_RECORD_REQUIRED through
 *     `refused`). Any other late completion is refused 409 HANDOFF_NOT_CURRENT_PHASE.
 *   - `refused`: main `ff2a10ef`'s engine (`handoff`'s
 *     `if RoleFor(issue.Phase) != fact.Role { return refused("HANDOFF_NOT_CURRENT_PHASE", …) }`).
 *     It refuses every late completion 409 HANDOFF_NOT_CURRENT_PHASE, because it has no
 *     `leftItsPhase`.
 * The route in front of both is the same (`internal/api/handoff.go`, `recordBacked`, identical at
 * `ff2a10ef`), and both write the late refusal through the same `refused()`. */
export type LateRole = "recorded" | "refused";

/** The labels the rig builds and the daemon each one runs, keyed by the label's name as
 * `rig.sh profile <label> <checkout>` names it. A label not here has no daemon this rig can model:
 * the stand-in refuses to boot for it, and health and the scorer refuse to read its runs. A new
 * label whose daemon answers some other way states that here. */
export const LABEL_DAEMONS: Readonly<Record<string, { readonly lateRole: LateRole }>> = {
  /** This checkout. */
  head: { lateRole: "recorded" },
  /** Built from main `ff2a10ef` (its `built-from`). */
  base: { lateRole: "refused" },
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

/** What one scenario's world takes, for each label and each role the scenario boots:
 *   - `boots`, the roles rig.sh boots, in the order it boots them, which is the role of each
 *     stand-in's lines in a log written before the stand-in named the role (`roleOf`);
 *   - `answers`, for each label and role, every answer that label's daemon may give that role's
 *     completion in this world. A 4xx answer not in the entry is a refusal the world should not
 *     have given (`refusedCompletion`);
 *   - `takes`, the role whose completion the world must take, at least once, with an answer in
 *     its entry (health's third rule). A scenario with no `takes` requires no completion.
 * Health judges what the world can accept, never what an agent should do: no entry counts the
 * completions an agent makes or forbids one.
 *
 * reviewer-text-findings boots the reviewer in `reviewing`, where its completion is of its own
 * phase and is taken 200. Its review moves the issue (the stand-in's `roundOutcome`), and a
 * completion after that is a late one: `ff2a10ef` refuses it 409 HANDOFF_NOT_CURRENT_PHASE, which
 * is the base daemon taking the completion as it takes every late one. This checkout records it
 * 200 when it names the reviewing record. A head reviewer refused HANDOFF_RECORD_REQUIRED (the
 * plugin names no record once the issue has left `reviewing`) is a refusal this table does not
 * allow.
 *
 * threads-closed boots the implementer, then the reviewer, in `reviewing` (text-fixes) or
 * `accepting` (code-defect) (rig.sh's run_threads_closed). The implementer's implementing round
 * already stands on its row, so either daemon refuses its completion there 409
 * HANDOFF_NOT_CURRENT_PHASE, the one deliberate refusal. The reviewer's completion is of its own
 * phase in `reviewing` and is taken 200. Once the round has moved it is late, which the base
 * daemon refuses 409 HANDOFF_NOT_CURRENT_PHASE. HANDOFF_STALE_ASSIGNMENT,
 * HANDOFF_ALREADY_RECORDED and HANDOFF_NOT_NEW are left out: the stand-in models no assignment, no
 * duplicate key and no earlier record of a role, so no run of this rig can get them. A late role
 * naming a record in `accepting`, which the route refuses 409 HANDOFF_RECORD_MISMATCH, is also
 * left out: neither label's plugin names a record outside the role's own file-backed phase
 * (packages/pi-shared/src/handoff-record.ts, `findHandoffReport`). */
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
      head: { reviewer: ["200"] },
      base: { reviewer: ["200", `409 ${NOT_CURRENT_PHASE}`] },
    },
    takes: "reviewer",
  },
  "threads-closed": {
    boots: ["implementer", "reviewer"],
    answers: {
      head: { implementer: [`409 ${NOT_CURRENT_PHASE}`], reviewer: ["200"] },
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

/** The role whose stand-in wrote `line`: the role the line names, or, for a log written before
 * the stand-in named it, the role rig.sh booted `line.boot`-th in `scenario`. Undefined when
 * neither says. */
export function roleOf(line: LoggedLine, scenario: string): string | undefined {
  return line.role ?? ANSWERS[scenario]?.boots[line.boot];
}

/** Every answer `label`'s daemon may give `role`'s completion in `scenario`'s world. A scenario
 * `ANSWERS` does not name boots one role in that role's own phase, which either daemon answers
 * 200. Throws for a label `ANSWERS` gives no entry in a scenario it names. */
export function allowedAnswers(
  scenario: string,
  label: string,
  role: string | undefined
): readonly string[] {
  const entry = ANSWERS[scenario];
  if (entry === undefined) return ["200"];
  const byRole = entry.answers[label];
  if (byRole === undefined)
    throw new Error(
      `the ${label} label has no answers for ${scenario}'s completions (completion-answers.ts's ANSWERS)`
    );
  return role === undefined ? [] : (byRole[role] ?? []);
}

/** The first completion of the run that was refused 4xx with an answer its world's entry does not
 * allow, and the role it was the completion of. This one reader serves the scorer's
 * refused-completion rule (score.ts's `unscored`) and health's second rule (health.ts). */
export function refusedCompletion(
  lines: readonly LoggedLine[],
  scenario: string,
  label: string
): { readonly line: LoggedLine; readonly role: string | undefined } | undefined {
  for (const line of lines) {
    if (line.path !== COMPLETE_PATH || line.status < 400 || line.status >= 500) continue;
    const role = roleOf(line, scenario);
    if (!allowedAnswers(scenario, label, role).includes(answerOf(line))) return { line, role };
  }
  return undefined;
}

/** The completions of the role the scenario's world must take (`takes`) that were answered with
 * an answer its entry allows; undefined for a scenario that requires none. */
export function takenCompletions(
  lines: readonly LoggedLine[],
  scenario: string,
  label: string
): { readonly role: string; readonly count: number } | undefined {
  const role = ANSWERS[scenario]?.takes;
  if (role === undefined) return undefined;
  const allowed = allowedAnswers(scenario, label, role);
  const count = lines.filter(
    (line) =>
      line.path === COMPLETE_PATH &&
      roleOf(line, scenario) === role &&
      allowed.includes(answerOf(line))
  ).length;
  return { role, count };
}
