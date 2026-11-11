import type { Item } from "./issues-table-model";

export const LIFECYCLE_ORDER: readonly string[] = [
  "status:triage",
  "status:icebox",
  "status:backlog",
  "status:todo",
  "phase:queue",
  "status:in_progress",
  "phase:design",
  "phase:planning",
  "phase:implementing",
  "phase:testing",
  "status:needs_review",
  "phase:reviewing",
  "phase:accepting",
  "phase:merging",
  "phase:awaiting_person",
  "phase:held",
  "phase:waiting_on_children",
  "phase:other",
  "status:retro",
  "status:done",
];

/** The lifecycle stage a Dispatch status names when no Legion card supplies a phase. */
export function statusStage(status: string): string {
  return status === "testing" ? "phase:testing" : `status:${status}`;
}

/** Which lifecycle stage an issue stands in: its Legion phase where the board holds it, else its
 * Dispatch status. A Legion-labelled issue without a card reads as the phase its status names. */
export function stageOf(item: Pick<Item, "issue" | "legion">): string {
  if (item.legion !== undefined) {
    const stage = `phase:${item.legion.id}`;
    return LIFECYCLE_ORDER.includes(stage) ? stage : "phase:other";
  }
  if (item.issue.labels?.some((label) => label.toLowerCase() === "legion")) {
    if (item.issue.status === "in_progress") return "phase:implementing";
    if (item.issue.status === "needs_review") return "phase:reviewing";
    if (item.issue.status === "todo") return "phase:queue";
  }
  return statusStage(item.issue.status);
}
