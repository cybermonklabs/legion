import {
  pageStatusDoneText,
  pageStatusIdleText,
  pageStatusProgressText,
} from "../../theme/classes";
import type { IssueStatus } from "../project/board-model";

/** The statuses of work that moves: started, and not done. Typed by the status names, so a
 *  misspelt one fails the type check rather than leave an issue idle. */
const MOVING_STATUSES: ReadonlySet<string> = new Set<IssueStatus>([
  "in_progress",
  "needs_review",
  "testing",
]);

/** The status's colour, as the prototype's `.st-*` gives it. */
export function statusTone(status: string): string {
  if (status === "done") return pageStatusDoneText;
  return MOVING_STATUSES.has(status) ? pageStatusProgressText : pageStatusIdleText;
}

/** The status's shape, the prototype's: a half-filled circle while the work moves, a tick once
 *  it is done, an empty circle before it starts. The word beside it says the same. */
export function statusGlyph(status: string): string {
  if (status === "done") return "✓";
  return MOVING_STATUSES.has(status) ? "◐" : "○";
}
