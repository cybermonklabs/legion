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

/** The shapes `statusGlyph` names, one per group of statuses. */
export type StatusGlyphText = "✓" | "◐" | "○";

/** The status's shape, the prototype's: a half-filled circle while the work moves, a tick once
 *  it is done, an empty circle before it starts. The word beside it says the same. Draw it with
 *  `StatusGlyph`, never as text, since the page's typeface lacks ◐. */
export function statusGlyph(status: string): StatusGlyphText {
  if (status === "done") return "✓";
  return MOVING_STATUSES.has(status) ? "◐" : "○";
}
