// The record of each run of a step of a world's issue, as the Legion daemon writes it onto the
// issue's Dispatch (main's since #4400, 53980e0e32): one write at the run's start, one at its end,
// and the report alone of a completion that lands after that end, each an outbox `step_record`
// row its executor sends as `PUT /api/v1/issues/{key}/steps/{id}` under the daemon's own session
// (packages/daemon/internal/daemon/outbox.go's `publishStepRecord`, workflow/steps.go's
// `startStep`, `endStep` and `reportEndedStep`). Dispatch writes each field once and keeps what a
// write leaves out, so the writes of one run merge in any order. `dispatch steps` and the route's
// GET are answered by the run's own Dispatch server, the label's own build, so the world fakes the
// write alone: the world's seeds write the history's runs (seed-handoffs.ts's `stepsSeed`), and
// the daemon stand-in each run it starts or ends (daemon-standin.ts's `queueStep`). Every table
// here restates the label daemon's source, which daemon-pane.ts's `labelShapes` holds it to by
// marker (`writesSteps`), so a daemon that writes otherwise is of no shape the rig reads.
import { createHash } from "node:crypto";
import { existsSync, readdirSync } from "node:fs";
import path from "node:path";
import { STEP_AGENT_TEXT_PATTERN } from "@legion/contracts/dispatch-tools";
import { sessionId, transcriptEntries } from "./transcript";

/** The task every step of today's fixed phases is a step of (workflow/steps.go's `defaultTask`). */
export const STEP_TASK = "default";

/** The steps of that task, in order, each a role or `role:key` (workflow/plan.go's
 * `DefaultSteps`, main's since #4488): a working phase's step is at the place of the first step
 * whose role works it (`placeOf`). */
export const DEFAULT_STEPS = ["planner", "implementer", "tester", "reviewer", "acceptance:owner"];

/** The place in the task of the step `role` works: one plus the index of the first of
 * DEFAULT_STEPS whose role, the part before its `:key`, is `role` (workflow/plan.go's `stepIn`).
 * Throws for a role no step names. */
function placeOf(role: string): number {
  const at = DEFAULT_STEPS.findIndex((named) => named.split(":")[0] === role);
  if (at < 0) throw new Error(`no step of DEFAULT_STEPS is the ${role}'s`);
  return at + 1;
}

/** The step each phase runs (workflow/steps.go's `stepOf`, workflow/plan.go's `stepIn` over
 * `DefaultSteps`): its phase's Go name, the kind of the run (`step` at its place from 1, from
 * `placeOf`; `retro` and `merge` at place 0, which no plan names), the role and the type its
 * record names (plan.go's `stepWork` for a step). A phase not here runs no step. */
export const STEPS = {
  planning: {
    go: "Planning",
    kind: "step",
    place: placeOf("planner"),
    role: "planner",
    type: "planning",
  },
  implementing: {
    go: "Implementing",
    kind: "step",
    place: placeOf("implementer"),
    role: "implementer",
    type: "implementation",
  },
  testing: {
    go: "Testing",
    kind: "step",
    place: placeOf("tester"),
    role: "tester",
    type: "testing",
  },
  reviewing: {
    go: "Reviewing",
    kind: "step",
    place: placeOf("reviewer"),
    role: "reviewer",
    type: "review",
  },
  accepting: {
    go: "Accepting",
    kind: "step",
    place: placeOf("acceptance"),
    role: "acceptance",
    type: "review",
  },
  retro: { go: "Retro", kind: "retro", place: 0, role: "implementer", type: "" },
  merging: { go: "Merging", kind: "merge", place: 0, role: "merger", type: "" },
} as const;
export type StepPhase = keyof typeof STEPS;

/** Whether `phase` runs a step. */
export function runsStep(phase: string): phase is StepPhase {
  return Object.hasOwn(STEPS, phase);
}

/** The triggers whose move ends the step it leaves failed, returning the work to the step it
 * moves into (workflow/steps.go's `endOf`); every other move ends it passed. A head that
 * conflicts with its base moves no issue in main's daemon since #4464, so no move ends a step
 * stopped: a run is stopped only where no move ended it (`stepStopped`), which no run of the rig
 * reaches. */
export const FAILED_TRIGGERS = [
  "TriggerTesterFailed",
  "TriggerReviewRejected",
  "TriggerBackward",
] as const;
export type StepResult = "passed" | "failed" | "stopped";

/** How a move on `trigger` ends the step it leaves, and whether its record names the step the work
 * returns to (workflow/steps.go's `endOf`). */
export function endOf(trigger: string): { result: StepResult; returns: boolean } {
  if ((FAILED_TRIGGERS as readonly string[]).includes(trigger))
    return { result: "failed", returns: true };
  return { result: "passed", returns: false };
}

/** The name the daemon derives every step record's namespace from (workflow/steps.go's
 * `stepRecordNamespace`, a version 5 uuid of it in the OID namespace). */
export const STEP_NAMESPACE_NAME = "legion-step-record";
/** RFC 4122's OID namespace (Go's `uuid.NameSpaceOID`). */
const NAMESPACE_OID = "6ba7b812-9dad-11d1-80b4-00c04fd430c8";

/** A version 5 uuid of `name` in `namespace` (Go's `uuid.NewSHA1`). */
function uuidV5(namespace: string, name: string): string {
  const hash = createHash("sha1")
    .update(Buffer.from(namespace.replaceAll("-", ""), "hex"))
    .update(name)
    .digest();
  hash[6] = ((hash[6] ?? 0) & 0x0f) | 0x50;
  hash[8] = ((hash[8] ?? 0) & 0x3f) | 0x80;
  const hex = hash.subarray(0, 16).toString("hex");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

/** The id of the record of one run of `phase`'s step on `issue` at `generation`, its `starts`th
 * start of that place (workflow/steps.go's `StepID`): the start and the end of a run compute one
 * id, and the next start of the place another. */
export function stepId(
  issue: string,
  generation: number,
  phase: StepPhase,
  starts: number
): string {
  const { kind, place } = STEPS[phase];
  return uuidV5(
    uuidV5(NAMESPACE_OID, STEP_NAMESPACE_NAME),
    `${issue}/${generation}/${STEP_TASK}/${kind}/${place}/${starts}`
  );
}

/** The fields of a step write as the daemon's executor sends them, in the order its body declares
 * them (daemon/internal/dispatch/types.go's `StepWrite`): the Go name, its type and its JSON key.
 * The first seven, the run's identity, every write carries; each other a write may leave out. */
export const STEP_WRITE_FIELDS = [
  ["Task", "string", "task"],
  ["Step", "int", "step"],
  ["Kind", "string", "kind"],
  ["Role", "string", "role"],
  ["Type", "string", "type"],
  ["Agent", "string", "agent"],
  ["Generation", "uint64", "generation"],
  ["Session", "*string", "session,omitempty"],
  ["Model", "*string", "model,omitempty"],
  ["StartedAt", "*time.Time", "started_at,omitempty"],
  ["EndedAt", "*time.Time", "ended_at,omitempty"],
  ["Result", "*string", "result,omitempty"],
  ["ReturnedTo", "*int", "returned_to,omitempty"],
  ["Report", "*string", "report,omitempty"],
] as const;

/** One write of a run's record, the body of `PUT /api/v1/issues/{key}/steps/{id}` but its actor:
 * the run's identity, and each field the write knows. */
export interface StepWrite {
  readonly task: string;
  readonly step: number;
  readonly kind: (typeof STEPS)[StepPhase]["kind"];
  readonly role: string;
  readonly type: string;
  readonly agent: string;
  readonly generation: number;
  readonly session?: string;
  readonly model?: string;
  readonly started_at?: string;
  readonly ended_at?: string;
  readonly result?: StepResult;
  readonly returned_to?: number;
  readonly report?: string;
}

/** One run of a step, as the writes of its record name it: its id and its identity. */
export interface StepRun {
  readonly id: string;
  readonly identity: StepWrite;
}

/** The run of `phase`'s step on `issue` at `generation`, its `starts`th start, run by the claim
 * `agent` (workflow/steps.go's `stepWrite`). */
export function stepRun(
  issue: string,
  generation: number,
  phase: StepPhase,
  agent: string,
  starts: number
): StepRun {
  const { kind, place, role, type } = STEPS[phase];
  return {
    id: stepId(issue, generation, phase, starts),
    identity: { task: STEP_TASK, step: place, kind, role, type, agent, generation },
  };
}

/** The start of `run` at `startedAt` (workflow/steps.go's `startStep`): its identity and the
 * moment, and no session, since only the end names the session and the model. */
export function stepStart(run: StepRun, startedAt: string): StepWrite {
  return { ...run.identity, started_at: startedAt };
}

/** The end of `run` (workflow/steps.go's `endStep`): its identity, the moment its start wrote where
 * it is known, the moment now, the result, the step the work returns to, the report its ending
 * completion named, and the session and the model of one read of its agent, each left out where
 * it is not known or outside the form Dispatch holds a step's agent text to (`agentText`). */
export function stepEnd(
  run: StepRun,
  end: {
    readonly startedAt?: string;
    readonly endedAt: string;
    readonly result: StepResult;
    readonly returnedTo?: number;
    readonly report?: string;
    readonly session?: string;
    readonly model?: string;
  }
): StepWrite {
  const session = agentText(end.session);
  const model = agentText(end.model);
  return {
    ...run.identity,
    ...(end.startedAt === undefined ? {} : { started_at: end.startedAt }),
    ended_at: end.endedAt,
    result: end.result,
    ...(end.returnedTo === undefined ? {} : { returned_to: end.returnedTo }),
    ...(end.report === undefined || end.report === "" ? {} : { report: end.report }),
    ...(session === undefined ? {} : { session }),
    ...(model === undefined ? {} : { model }),
  };
}

/** The report alone of a completion that landed after its run's end (workflow/steps.go's
 * `reportEndedStep`). */
export function stepReport(run: StepRun, report: string): StepWrite {
  return { ...run.identity, report };
}

/** `text` where it is in the form Dispatch holds a step's agent, session and model to
 * (contracts' STEP_AGENT_TEXT_PATTERN), and undefined otherwise: the daemon leaves such text out
 * of the record (internal/daemon's `agentText`). */
export function agentText(text: string | undefined): string | undefined {
  return text !== undefined && STEP_AGENT_TEXT_PATTERN.test(text) ? text : undefined;
}

/** The model that answered last in `session`, as the daemon reads it off the agent's stream
 * (internal/stream's `Answered`, supervise's `LastAnswer`): the `provider` and `model` of the last
 * assistant message of the session's transcript, `<provider>/<model>`, or the model alone where it
 * names no provider; undefined where the transcript names none, as no message a model wrote or
 * Claude Code's `<synthetic>` names none. The transcript is the one under `<run>/sessions`, the
 * run's first agent's, or `<run>/sessions/<role>`, a later one's (rig.sh's worker_boot), whose
 * header names `session`. */
export function answeredModel(runDir: string, session: string): string | undefined {
  const sessions = path.join(runDir, "sessions");
  const files: string[] = [];
  const collect = (dir: string, depth: number) => {
    if (!existsSync(dir)) return;
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      if (entry.isFile() && entry.name.endsWith(".jsonl")) files.push(path.join(dir, entry.name));
      else if (entry.isDirectory() && depth > 0) collect(path.join(dir, entry.name), depth - 1);
    }
  };
  collect(sessions, 1);
  for (const file of files) {
    const entries = transcriptEntries(file);
    if (sessionId(entries) !== session) continue;
    let model: string | undefined;
    for (const entry of entries) {
      const message = entry.message as Record<string, unknown> | undefined;
      if (message?.role !== "assistant") continue;
      const named = typeof message.model === "string" ? message.model : "";
      if (named === "" || named === "<synthetic>") continue;
      const provider = typeof message.provider === "string" ? message.provider : "";
      model = provider === "" ? named : `${provider}/${named}`;
    }
    return model;
  }
  return undefined;
}
