/**
 * The marks every Inbox row wears, drawn the same way in the short row beside the reading pane
 * and in the card below its width: what kind of row it is, how its issue is prioritised, how
 * urgent the question is, and whether its issue is approved. Each one is `shrink-0` and never
 * wraps, so none of them can grow over the title or over the issue key beside it - the row's own
 * text is what shortens.
 */

import type { ApprovalGateLine } from "@legion/contracts";
import type { ReactNode } from "react";

import type { ArtifactApproval, AskUrgency, IssuePriority } from "../../api/types";
import { approvalPillClassName } from "../../components/Pill";
import { badgeBlocking, badgeHigh, badgeLow, badgeMed, priorityBadge } from "../../theme/classes";
import { approvalLabel } from "../doc/ApprovalChip";
import { isDesignGateStep, STEP_LABELS } from "../issue/gated-steps";
import { CONTROL_LABELS } from "../issue-page/page-words";
import { URGENCY_LABELS } from "./ask-urgency";
import { TYPE_LABELS } from "./grouping";
import type { InboxRowType } from "./list-row";

/** One character per kind, in place of the word the row used to spend a badge on. */
const TYPE_GLYPHS: Record<InboxRowType, string> = {
  agent: "✦",
  approval: "✓",
  decision: "◆",
  question: "?",
  reply: "↩",
};

/** The badge tone of each urgency, on the canvas rather than on a decision block's tint, where
 *  `askBlockPill` lifts the same hues one step. */
const URGENCY_BADGES: Record<AskUrgency, { bg: string; text: string }> = {
  blocking: badgeBlocking,
  high: badgeHigh,
  low: badgeLow,
  med: badgeMed,
};

/** What the row is, at the left edge of its first line. The glyph is decoration; the name is
 *  the image's own label, so a reader who cannot see it is told the kind just the same. */
export function RowKindIcon({ type }: { type: InboxRowType }): ReactNode {
  return (
    <span
      aria-label={TYPE_LABELS[type]}
      className={`grid size-5 shrink-0 place-items-center rounded text-xs leading-none ${badgeMed.bg} ${badgeMed.text}`}
      data-inbox-kind={type}
      role="img"
    >
      <span aria-hidden="true">{TYPE_GLYPHS[type]}</span>
    </span>
  );
}

/** The owning issue's priority, as the coloured tag the board and the issue header use; a row
 *  whose issue has none, and every row with no issue at all, wears nothing. */
export function RowPriorityTag({ priority }: { priority: IssuePriority | null }): ReactNode {
  if (priority === null) return null;
  const tone = priorityBadge[priority];
  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold whitespace-nowrap ${tone.bg} ${tone.text}`}
      data-inbox-priority={`P${priority}`}
    >
      P{priority}
    </span>
  );
}

/** The ask's urgency, in its own colour and in its own word. A thread has no urgency, so its
 *  row wears no mark at all. */
export function RowUrgencyMark({ urgency }: { urgency: AskUrgency | null }): ReactNode {
  if (urgency === null) return null;
  const tone = URGENCY_BADGES[urgency];
  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-xs font-medium whitespace-nowrap ${tone.bg} ${tone.text}`}
      data-inbox-urgency={urgency}
    >
      {URGENCY_LABELS[urgency]}
    </span>
  );
}

/** The issue page's word for a gate line's step (`STEP_LABELS`): undefined for a line with no
 *  step, and for a step word this build does not know, whose line draws its state alone. */
export function gateStepWord(step: string | undefined): string | undefined {
  return step !== undefined && isDesignGateStep(step) ? STEP_LABELS[step] : undefined;
}

/** The state of a gate line's document in the issue page's words, as its step line draws it
 *  (`ApprovalSteps`): the chip's own words (`approvalLabel`), and `Not requested` for a document
 *  nobody has asked to review - one the issue does not hold, or a draft. The word `Draft` is
 *  drawn nowhere. */
export function gateLineWords(approval: ArtifactApproval | null): string {
  return approval === null || approval.state === "draft"
    ? CONTROL_LABELS.not_requested
    : approvalLabel(approval);
}

/** A gate line's state as its mark's `data-` attribute names it: its document's state, and
 *  `not_requested` for a draft, as its words say (`gateLineWords`). */
export function gateLineState(state: ArtifactApproval["state"]): string {
  return state === "draft" ? "not_requested" : state;
}

/** The line a row's mark speaks for: a gate's only line, and of more than one the first, in the
 *  gate's order, whose document is not approved at its latest version, else the last. A line
 *  whose step word this build does not know is still a line of the gate, so it is counted. */
function markedGateLine(gate: readonly ApprovalGateLine[]): ApprovalGateLine | undefined {
  if (gate.length < 2) return gate[0];
  return gate.find((line) => line.approval?.state !== "approved") ?? gate[gate.length - 1];
}

/** What a row's mark says for `line`, the gate's line it speaks for (`markedGateLine`): the state
 *  of its line's document in the issue page's words, a request as `Awaiting approval` alone - the
 *  pane's line names the documents it covers - and, where the gate has more than one line, the
 *  line's step word before it, as `Verification · Awaiting approval`. */
function approvalMarkText(gate: readonly ApprovalGateLine[], line: ApprovalGateLine): string {
  const approval = line.approval;
  const words =
    approval?.state === "awaiting"
      ? approvalLabel({ ...approval, documents: [] })
      : gateLineWords(approval);
  const step = gate.length > 1 ? gateStepWord(line.step) : undefined;
  return step === undefined ? words : `${step} · ${words}`;
}

/** The approval state of the row's issue, in the issue page's words and in its own text, so its
 *  colour states nothing alone (`approvalMarkText`). A row of no issue, and a row whose read
 *  carried no gate - a server older than the field - wears nothing. Each of the four renderers
 *  draws it first in its line - on an ask's card, first after the checkbox and the kind's icon -
 *  at one fixed place in the row, so the marks of rows one above another start at one x
 *  whatever the row's other text and the mark's own words. */
export function RowApprovalMark({
  gate,
}: {
  gate: readonly ApprovalGateLine[] | undefined;
}): ReactNode {
  const line = gate === undefined ? undefined : markedGateLine(gate);
  if (gate === undefined || line === undefined) return null;
  const state = line.approval?.state ?? "draft";
  return (
    <span
      className={`${approvalPillClassName(state, "compact")} shrink-0 whitespace-nowrap`}
      data-inbox-approval={gateLineState(state)}
    >
      {approvalMarkText(gate, line)}
    </span>
  );
}
