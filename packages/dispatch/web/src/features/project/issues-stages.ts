import type { LegionBoardColumnID } from "../../api/types";
import { type IssueStatus, isIssueStatus } from "./board-model";

/**
 * Where an issue stands: one of eighteen stages, in the one order the bar, the Stage sort and the
 * Board follow. An issue with a card on the Legion board is in the stage of its card's column; an
 * issue with no card is in the stage of its status, and `testing` holds both. A done issue is in
 * Done whatever card it has, so each issue is in exactly one stage and a Legion issue never
 * counts twice. The ids are the board's column ids and the status ids, with no prefix, so a link
 * names a stage the way the board and the listing name it.
 */
export const STAGES = [
  { id: "triage", label: "Triage", source: "status", group: "not_started" },
  { id: "icebox", label: "Icebox", source: "status", group: "not_started" },
  { id: "backlog", label: "Backlog", source: "status", group: "not_started" },
  { id: "todo", label: "Todo", source: "status", group: "not_started" },
  { id: "queue", label: "Queue", source: "card", group: "not_started" },
  { id: "design", label: "Design", source: "card", group: "in_flight" },
  { id: "planning", label: "Planning", source: "card", group: "in_flight" },
  { id: "implementing", label: "Implementing", source: "card", group: "in_flight" },
  { id: "in_progress", label: "In progress", source: "status", group: "in_flight" },
  { id: "testing", label: "Testing", source: "both", group: "in_flight" },
  { id: "reviewing", label: "Reviewing", source: "card", group: "in_flight" },
  { id: "needs_review", label: "Needs review", source: "status", group: "in_flight" },
  { id: "accepting", label: "Accepting", source: "card", group: "in_flight" },
  { id: "retro", label: "Retro", source: "status", group: "in_flight" },
  { id: "merging", label: "Merging", source: "card", group: "in_flight" },
  { id: "awaiting_person", label: "Waiting on a person", source: "card", group: "in_flight" },
  { id: "held", label: "Held", source: "card", group: "in_flight" },
  { id: "done", label: "Done", source: "status", group: "done" },
] as const;

export type StageId = (typeof STAGES)[number]["id"];
type StageSource = (typeof STAGES)[number]["source"];

export const stageIds: readonly StageId[] = STAGES.map((stage) => stage.id);

/** The five stages of work that has not started: the bar on All open and Created by me writes
 *  their count as one number at its left end. */
export const NOT_STARTED_STAGES: readonly StageId[] = STAGES.filter(
  (stage) => stage.group === "not_started"
).map((stage) => stage.id);

/** Stages 6 to 17, the In flight view's stages in a project with a Legion daemon. */
export const IN_FLIGHT_STAGES: readonly StageId[] = STAGES.filter(
  (stage) => stage.group === "in_flight"
).map((stage) => stage.id);

/** The In flight view's stages in a project with no Legion daemon: the status stages of work. */
export const STATUS_IN_FLIGHT_STAGES: readonly StageId[] = [
  "in_progress",
  "testing",
  "needs_review",
  "retro",
];

/** The stages only a card puts an issue in: the daemon owns them, so the Board refuses a drop
 *  on them (all but Queue, which takes a drop from inside itself as a rank write). */
export const CARD_STAGES: readonly StageId[] = STAGES.filter(
  (stage) => stage.source === "card"
).map((stage) => stage.id);

/** The status stages of work Legion does not run: the palette hatches them. */
export const UNRUN_STAGES: readonly StageId[] = ["in_progress", "needs_review", "retro"];

const byId: ReadonlyMap<string, (typeof STAGES)[number]> = new Map(
  STAGES.map((stage) => [stage.id, stage])
);

export function isStageId(value: string): value is StageId {
  return byId.has(value);
}

export function stageLabel(stage: string): string {
  return byId.get(stage)?.label ?? stage;
}

export function stageSource(stage: StageId): StageSource {
  return byId.get(stage)?.source ?? "status";
}

/** A stage a status puts an issue in, which is a status the Board can set by a drop. */
export function stageStatus(stage: StageId): IssueStatus | undefined {
  return isIssueStatus(stage) ? stage : undefined;
}

/** The stage of one issue: Done for a done issue, else its card's column, else its status. */
export function stageOf(status: string, column: LegionBoardColumnID | undefined): StageId {
  if (status === "done") return "done";
  if (column !== undefined) return column;
  return isStageId(status) ? status : "triage";
}

/** Why a card waits, in the order the Waiting bar draws them: it is ready to start, it waits for
 *  a slot, a blocker link holds it, or a person has not answered it. */
export const WAITING_REASONS = ["ready", "slot", "blocked", "person"] as const;
export type WaitingReason = (typeof WAITING_REASONS)[number];
const waitingLabels: Record<WaitingReason, string> = {
  ready: "Ready",
  slot: "Needs a slot",
  blocked: "Blocked",
  // The stage's own words: a person has not answered.
  person: "Waiting on a person",
};
export function waitingLabel(reason: string): string {
  return (waitingLabels as Record<string, string>)[reason] ?? reason;
}

/** What a slot holder does with its slot, in the order the Slot use bar draws them. */
export const SLOT_USES = [
  "working",
  "between_turns",
  "held_children",
  "held_person",
  "held_idle",
] as const;
export type SlotUse = (typeof SLOT_USES)[number];
const slotUseLabels: Record<SlotUse, string> = {
  working: "Working",
  between_turns: "Between turns",
  held_children: "Held for children",
  held_person: "Held for a person",
  held_idle: "Held, idle",
};
export function slotUseLabel(use: string): string {
  return (slotUseLabels as Record<string, string>)[use] ?? use;
}
