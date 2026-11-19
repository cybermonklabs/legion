import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

import { api } from "../../api/client";
import { issueCoverageQuery, projectsQuery, whoAmIQuery } from "../../api/queries";
import type { Artifact, IssueChild, IssueDetails } from "../../api/types";
import { approvalPillClassName } from "../../components/Pill";
import {
  bgTransparent,
  focusVisibleRing,
  pageInkText,
  pageLabelChip,
  pageLineBorder,
  pageMutedText,
  pageQuietText,
  pageSegmentBg,
  pageSegmentDoneBg,
  pageSegmentProgressBg,
  pageStatusDoneText,
  pageStatusIdleText,
  pageStatusProgressText,
  pageWaitingBg,
  pageWaitingChip,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { issueStatuses, openIssueStatuses, statusLabel } from "../project/board-model";
import { ApprovalSteps } from "./ApprovalSteps";
import { AssigneeControl } from "./AssigneeControl";
import { gatedStepEntries } from "./gated-steps";
import { presetPath } from "./issue-address";
import { PriorityEditor } from "./PriorityControl";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { UnreachableRouteMarker } from "./RouteReach";
import { treeContext } from "./tree-drafts";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";
import type { IssuePriorityWrite } from "./useIssuePriority";
import { type WaitingPress, waitingOnReader } from "./waiting";

/**
 * The header's one properties line (dispatch://INFRA-1196 R13, the prototype's `#hdr-props`):
 * the status, the priority, the assignee, the children's progress as a bar with its two counts,
 * the count of what waits on the reader - or, while nothing does, whose turn the issue is - the
 * approval control, the one marker where the issue's message route reaches nobody, and then the
 * labels, as many as fit on the line, which ends with a count of those left over that opens
 * Details at its Labels row (the ruling of
 * dispatch://INFRA-1196/comment/63b73642-8fc5-4cc4-8f5d-a3d8989d3bf4). Nothing else: every other
 * property, and every control of one, is in Details (`IssueMargin.tsx`).
 *
 * The line wraps rather than scrolling: nothing here is reached by dragging a row sideways.
 */
export function IssuePropertiesRow({
  documentArtifact,
  isClosed,
  issue,
  onOpenLabels,
  priorityWrite,
  writes,
}: {
  documentArtifact: Artifact | undefined;
  isClosed: boolean;
  issue: IssueDetails;
  /** Opens Details at its Labels row, which lists every label with its control. */
  onOpenLabels: () => void;
  priorityWrite: IssuePriorityWrite;
  writes: IssueHeaderWrites;
}): ReactNode {
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const selectableStatuses = isClosed ? issueStatuses : openIssueStatuses;
  // A status picked while another write of the issue is in flight - the title saved by the blur
  // that the press on this control caused - is held and sent once that write has settled, so the
  // press lands on the control as it does with nothing edited (the issue's writes go one at a
  // time, `useIssueDrafts`'s guard), rather than finding it refusing input. Only the status's own
  // write holds the control.
  const [heldStatus, setHeldStatus] = useState<string | undefined>(undefined);
  const saving = writes.updateIssue.isPending;
  const pickStatus = (next: string) => {
    if (!writes.drafts.requestStatusSubmit(next)) setHeldStatus(next);
  };
  useEffect(() => {
    if (heldStatus === undefined || saving) return;
    setHeldStatus(undefined);
    if (!writes.drafts.requestStatusSubmit(heldStatus)) setHeldStatus(heldStatus);
  }, [heldStatus, saving, writes.drafts]);
  const status = heldStatus ?? writes.pendingStatus ?? issue.status;
  const openAsks = (issue.open_asks ?? []).filter((ask) => ask.state === "open");
  // What the count counts, read from what the outline already reads under the same keys - the
  // decisions, the coverage's statements, the comments a Change is written as - so it costs no
  // request of its own (`waiting.ts`).
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const decisions = useQuery(issueDecisionsQuery(issue.key, true));
  const coverage = useQuery(issueCoverageQuery(issue.key));
  const comments = useQuery({
    queryFn: () => api.listComments(issue.key),
    queryKey: ["comments", issue.key],
  });
  const nodeIdOf = useMemo(() => treeContext({ issue }).nodeIdOf, [issue]);
  const statements = useMemo(
    () => new Set((coverage.data?.requirements ?? []).map((one) => nodeIdOf(one.ref))),
    [coverage.data, nodeIdOf]
  );
  const waiting = waitingOnReader(
    issue,
    viewer,
    decisions.data,
    (reference) => statements.has(nodeIdOf(reference)),
    comments.data
  );
  // The detail carries waiting_on on every open ask; a response without it cannot say whose
  // turn it is, so the indicator stays off rather than shown wrong.
  const knowsWhoseTurn = openAsks.every((ask) => ask.waiting_on !== undefined);
  const agentsTurn = openAsks.length - waitingOnHuman(openAsks).length;
  // The one shared read of the projects, under the key the shell's sidebar and the search
  // palette already hold: only a root issue has gated steps at all (`gatedStepEntries`), so a
  // child does not ask for the list, and while the read is pending or failed the issue draws
  // its one chip rather than claiming a gate it has not read.
  const projects = useQuery({ ...projectsQuery(), enabled: issue.parent === null });
  const projectRow = projects.data?.find((candidate) => candidate.key === issue.project);
  const entries = gatedStepEntries(projectRow, issue);
  // Which document a step reviews is `gated-steps.ts`'s to say, so the chip asks the drawn
  // entries rather than the gate's words: a gate that names no step for the primary document
  // leaves that document's own state and its two controls to the chip.
  const stepHoldsPrimaryDocument = entries.some(
    (entry) => entry.document?.id === issue.primary_artifact_id
  );
  // Approve and Request changes are here while an approval request waits on the reader, and in
  // Details otherwise, with the steps and their reasons (R31).
  const approvalWaits = waiting.approvals.length > 0;
  const approval = documentArtifact?.approval;

  // The count's press: the Decisions filter, with the outline drawn in place of any document,
  // and the focus on the next item there that waits, in the filter's order - the first press on
  // the first, each press after on the one after it (the Decisions filter's body takes the press
  // from the history entry); an approval request alone puts the focus on the approval control on
  // this line (R13, dispatch://INFRA-3678). A press made on the filter already replaces its entry,
  // so Back leaves the filter rather than walking back through the presses.
  const goToWaiting = () => {
    if (waiting.asks + waiting.decisions > 0) {
      const press: WaitingPress = { waitingPress: performance.now() };
      const path = presetPath(issue.key, "decisions");
      navigate(path, { replace: pathname === path, state: press });
      return;
    }
    const header = document.querySelector("[data-issue-header]");
    const control =
      waiting.approvals
        .map((id) =>
          header?.querySelector<HTMLElement>(`[data-approval-artifact="${CSS.escape(id)}"]`)
        )
        .find((found) => found !== null && found !== undefined) ??
      header?.querySelector<HTMLElement>("[data-approval-artifact]");
    control?.focus();
  };

  return (
    <div
      className={`flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-1.5 border-b px-3.5 pt-1 pb-2.5 text-sm leading-5 md:gap-x-3.5 md:px-7 md:pt-0.5 md:pb-[7px] ${pageLineBorder} ${pageMutedText}`}
      data-testid="issue-state-actions"
    >
      {/* The prototype's `.pill`: the status's glyph and word inside a rule of its colour. The
          rule is drawn inside the pill's box, so the select can fill that box: 24 px tall on a
          phone, the least a touch target may be (WCAG 2.5.8), and the prototype's 20 px from
          the phone's width up. */}
      <label
        className={`inline-flex min-h-6 shrink-0 items-stretch gap-1 rounded-full px-2 text-xs leading-[18px] font-semibold shadow-[inset_0_0_0_1px_currentColor] md:min-h-5 ${statusTone(status)}`}
      >
        <span aria-hidden="true" className="self-center" {...pageChrome}>
          {statusGlyph(status)}
        </span>
        {/* Sized to the status it shows, as the prototype's pill is; a browser with no
            `field-sizing` draws the select at its longest option, which is its own width. */}
        <select
          aria-label={PROPERTY_LABELS.status}
          className={`min-w-0 cursor-pointer appearance-none font-inherit outline-none [field-sizing:content] disabled:cursor-not-allowed disabled:opacity-50 ${bgTransparent} ${focusVisibleRing}`}
          disabled={isClosed || writes.statusSaving}
          onChange={(event) => pickStatus(event.target.value)}
          value={status}
        >
          {selectableStatuses.map((choice) => (
            <option key={choice} value={choice}>
              {statusLabel(choice)}
            </option>
          ))}
        </select>
      </label>
      {/* The priority and the assignee stay enabled while another write is in flight and queue a
          pick behind it, as their own controls say: a press straight out of the title's editor
          lands on them. */}
      <PriorityEditor
        disabled={isClosed}
        issueKey={issue.key}
        variant="header"
        write={priorityWrite}
      />
      <AssigneeControl
        assignee={issue.assignee}
        disabled={isClosed}
        issueKey={issue.key}
        variant="header"
      />
      <ChildrenProgress childIssues={issue.children ?? []} />
      {waiting.total > 0 ? (
        // The prototype's `.waitchip`: a control, because the reader acts on what it counts.
        <button
          className={`inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2.5 text-xs leading-[22px] font-semibold ${pageWaitingChip} ${focusVisibleRing}`}
          data-testid="issue-whose-turn"
          onClick={goToWaiting}
          type="button"
          {...pageChrome}
        >
          <span aria-hidden="true" className={`size-2 shrink-0 rounded-full ${pageWaitingBg}`} />
          {PROPERTY_LABELS.waiting_on_you} (<span {...authorText}>{waiting.total}</span>)
        </button>
      ) : agentsTurn > 0 && knowsWhoseTurn ? (
        <span
          className={`inline-flex shrink-0 items-center text-xs leading-[22px] font-semibold ${pageQuietText}`}
          data-testid="issue-whose-turn"
          {...pageChrome}
        >
          {PROPERTY_LABELS.waiting_on_agents} (<span {...authorText}>{agentsTurn}</span>)
        </span>
      ) : null}
      {/* The approval control (R31): a root issue of a gated project reads its gate step by
          step, and the one chip stays beside those steps whenever no step reviews the primary
          document, so an open request on that document can always be answered here. Every
          other issue keeps the one chip, which says "Not requested" while nobody has asked. */}
      {documentArtifact === undefined || stepHoldsPrimaryDocument ? null : approval === undefined ||
        approval.state === "draft" ? (
        <span className={approvalPillClassName("draft", "compact")} {...pageChrome}>
          {CONTROL_LABELS.not_requested}
        </span>
      ) : (
        <ApprovalChip
          artifact={documentArtifact}
          layout="contents"
          showActions={approvalWaits}
          size="compact"
          variant="header"
        />
      )}
      {entries.length === 0 ? null : (
        <ApprovalSteps
          actions={approvalWaits}
          entries={entries}
          primaryArtifactId={issue.primary_artifact_id}
          reasons={false}
          size="compact"
        />
      )}
      {/* One marker, where the route reaches nobody; whether it reaches anyone otherwise, and
          the route itself, are Details' (R13). It speaks for the saved route only. */}
      {writes.drafts.route === (issue.route ?? "") ? (
        <UnreachableRouteMarker issue={issue} showRoute />
      ) : null}
      <HeaderLabels labels={issue.labels ?? []} onOpenAll={onOpenLabels} />
    </div>
  );
}

/** The status's colour, as the prototype's `.st-*` gives it. */
export function statusTone(status: string): string {
  if (status === "done") return pageStatusDoneText;
  if (status === "in_progress" || status === "testing" || status === "needs_review") {
    return pageStatusProgressText;
  }
  return pageStatusIdleText;
}

/** The status's shape, the prototype's: a half-filled circle while the work moves, a tick once
 *  it is done, an empty circle before it starts. The word beside it says the same. */
export function statusGlyph(status: string): string {
  if (status === "done") return "✓";
  if (status === "in_progress" || status === "testing" || status === "needs_review") return "◐";
  return "○";
}

/** How many segments the bar draws one child each before it is one bar instead: past this a
 *  row of segments is wider than the counts beside it. */
const MOST_SEGMENTS = 12;

/**
 * The children's progress (R13, the prototype's `.kids-prog`): the direct children done of
 * total, the bar, one segment per child, and the whole subtree done of total, each count with
 * its label, because the two numbers differ and a reader cannot tell which is which from the
 * numbers alone. An issue with no children draws none of it.
 */
function ChildrenProgress({ childIssues }: { childIssues: readonly IssueChild[] }): ReactNode {
  if (childIssues.length === 0) return null;
  const done = childIssues.filter((child) => child.status === "done").length;
  const subtree = childIssues.reduce(
    (sum, child) => ({
      done: sum.done + child.subtree_done,
      total: sum.total + child.subtree_total,
    }),
    { done: 0, total: 0 }
  );
  const moving = childIssues.filter(
    (child) =>
      child.status === "in_progress" ||
      child.status === "testing" ||
      child.status === "needs_review"
  ).length;
  return (
    // Two items from the phone's width up; below it one item, the direct count, the bar, then
    // the whole tree's count, so the line wraps around it rather than between the two
    // (dispatch://INFRA-1196/artifact/build-rulings-md, ruling 3), and the item wraps inside
    // itself only where a line is narrower than it, as at 320 px.
    <span className="inline-flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1.5 md:contents">
      <span
        className={`inline-flex shrink-0 items-center gap-2 font-medium ${pageInkText}`}
        data-testid="issue-progress-children"
        {...pageChrome}
      >
        <span>
          {CONTROL_LABELS.children} <span {...authorText}>{done}</span> {CONTROL_LABELS.count_of}{" "}
          <span {...authorText}>{childIssues.length}</span>
          <span className="hidden md:inline"> {CONTROL_LABELS.count_done}</span>
        </span>
        <span aria-hidden="true" className="inline-flex gap-0.5">
          {childIssues.length > MOST_SEGMENTS ? (
            <span
              className={`relative block h-2 w-16 overflow-hidden rounded-sm md:w-24 ${pageSegmentBg}`}
            >
              <span
                className={`absolute inset-y-0 left-0 block ${pageSegmentDoneBg}`}
                style={{ width: `${(100 * done) / childIssues.length}%` }}
              />
            </span>
          ) : (
            childIssues.map((child, index) => (
              <span
                className={`block h-2 w-2 rounded-sm md:w-3 ${
                  index < done
                    ? pageSegmentDoneBg
                    : index < done + moving
                      ? pageSegmentProgressBg
                      : pageSegmentBg
                }`}
                key={child.key}
              />
            ))
          )}
        </span>
      </span>
      <span
        className={`shrink-0 text-xs leading-5 ${pageMutedText}`}
        data-testid="issue-progress-subtree"
        {...pageChrome}
      >
        {CONTROL_LABELS.whole_tree} <span {...authorText}>{subtree.done}</span>{" "}
        {CONTROL_LABELS.count_of} <span {...authorText}>{subtree.total}</span>{" "}
        {CONTROL_LABELS.count_done}
      </span>
    </span>
  );
}

/**
 * The labels, in Dispatch's order, as many as fit on what is left of the line once every other
 * property is drawn, and then the count of those left over, which opens Details at its Labels
 * row, where every label is listed with its control (R13, the ruling of 2026-11-15). An issue
 * whose labels fit draws them all and no count.
 *
 * What fits is read off the page, never guessed at a width the typeface decides: every chip is
 * laid out once, with the count drawn at its widest, and the chips past the line's end are then
 * left out, so the line holds no word it hides. That reading is taken again whenever the labels
 * change or the line's width does, inside one commit, so no frame draws the chips that do not
 * fit.
 */
function HeaderLabels({
  labels,
  onOpenAll,
}: {
  labels: readonly string[];
  onOpenAll: () => void;
}): ReactNode {
  const box = useRef<HTMLDivElement>(null);
  const key = labels.join("\n");
  const [fit, setFit] = useState<{ readonly key: string; readonly shown: number } | null>(null);
  // Null while the line is being read: every chip laid out, and the count at its widest.
  const shown = fit?.key === key ? fit.shown : null;
  useLayoutEffect(() => {
    if (shown === null && box.current !== null) {
      setFit({ key, shown: labelsThatFit(box.current) });
    }
  });
  // A width the line changes to is read again on the next frame, so the reading is a commit
  // of its own rather than one inside the observer's notification.
  const hasLabels = labels.length > 0;
  useEffect(() => {
    const element = box.current;
    if (!hasLabels || element === null || typeof ResizeObserver === "undefined") return;
    let frame = 0;
    let width = element.clientWidth;
    const observer = new ResizeObserver(() => {
      if (element.clientWidth === width) return;
      width = element.clientWidth;
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => setFit(null));
    });
    observer.observe(element);
    return () => {
      cancelAnimationFrame(frame);
      observer.disconnect();
    };
  }, [hasLabels]);
  if (labels.length === 0) return null;
  const left = labels.length - (shown ?? 0);
  return (
    <>
      {/* `relative`: the chips' offsets are read against this box (`labelsThatFit`). */}
      <div
        className="relative flex min-w-0 flex-[1_1_0] gap-1.5 overflow-hidden"
        data-testid="issue-header-labels"
        ref={box}
      >
        {labels.slice(0, shown ?? labels.length).map((label) => (
          <span
            className={`inline-flex shrink-0 items-center rounded-md border px-[7px] text-xs leading-[18px] font-medium whitespace-nowrap ${pageLabelChip}`}
            data-header-label=""
            key={label}
            {...authorText}
          >
            {label}
          </span>
        ))}
      </div>
      {left === 0 ? null : (
        // Tabular digits, so the count read at its widest is as wide as any count of as many
        // digits it then draws.
        <button
          className={`shrink-0 text-xs leading-5 font-semibold tabular-nums underline underline-offset-[3px] ${pageInkText} ${focusVisibleRing}`}
          data-testid="issue-labels-left"
          onClick={onOpenAll}
          type="button"
          {...pageChrome}
        >
          +<span {...authorText}>{left}</span>{" "}
          {left === 1 ? CONTROL_LABELS.labels_left_one : CONTROL_LABELS.labels_left_many}
        </button>
      )}
    </>
  );
}

/** How many of the header's label chips stand wholly inside `box`, in order: the first that
 *  reaches past its end ends the run, as the line reads left to right. */
function labelsThatFit(box: HTMLElement): number {
  const width = box.clientWidth;
  let fit = 0;
  for (const chip of box.querySelectorAll<HTMLElement>("[data-header-label]")) {
    if (chip.offsetLeft + chip.offsetWidth > width + 0.5) break;
    fit += 1;
  }
  return fit;
}
