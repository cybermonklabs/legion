import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useLayoutEffect, useRef, useState } from "react";
import { flushSync } from "react-dom";

import { projectsQuery } from "../../api/queries";
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
  pageWaitingBg,
  pageWaitingChip,
} from "../../theme/classes";
import { useWaitingItems } from "../dashboard/useWaitingItems";
import { ApprovalChip } from "../doc/ApprovalChip";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { ApprovalSteps } from "../issue/ApprovalSteps";
import { AssigneeControl } from "../issue/AssigneeControl";
import { gatedStepEntries, isProjectRoot } from "../issue/gated-steps";
import { PriorityEditor } from "../issue/PriorityControl";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import type { IssuePriorityWrite } from "../issue/useIssuePriority";
import { issueStatuses, openIssueStatuses, statusLabel } from "../project/board-model";
import { EVENT_FRESH } from "../refs/reference-target";
import { statusGlyph, statusTone } from "./issue-status";
import { PullRequestLink } from "./PullRequestLink";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { StatusGlyph } from "./StatusGlyph";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";
import { useWaitingPress } from "./waiting";

/**
 * The header's one properties line (the prototype's `#hdr-props`): the status, the priority,
 * the assignee, the children's progress as a bar with its two counts, the count of what waits
 * on the reader - or, while nothing does, whose turn the issue is - the approval control, the
 * one marker where the issue's message route reaches nobody, and then the labels, as many as fit
 * on the line, which ends with a count of those left over that opens Details at its Labels row.
 * Nothing else: every other property, and every control of one, is in Details
 * (`IssueMargin.tsx`).
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
  // The count's press (dispatch://INFRA-3940 R5, the plan's ST-3): the focus goes to the next
  // item of Dashboard's list, and Dashboard opens first where another control of the row or a
  // document is drawn (`waiting.ts`).
  const pressCount = useWaitingPress(issue.key);
  const selectableStatuses = isClosed ? issueStatuses : openIssueStatuses;
  // A status picked while a write of the issue is in flight - the title saved by the blur that
  // the press on this control caused, or an earlier pick of the status still being saved - is
  // held by the header's writes (`pickStatus`: a keyed write in the issue's line, sent after the
  // write in flight), so the press lands on the control as it does with nothing edited, and the
  // header and Details read one status, the newest one held. No write disables the select, not
  // even its own: only a closed issue does. The browser moves the focus off a control that turns
  // disabled, so disabling the select while its pick saves would leave a keyboard pick's focus on
  // the page's body.
  const status = writes.pendingStatus ?? issue.status;
  const openAsks = (issue.open_asks ?? []).filter((ask) => ask.state === "open");
  // What waits on the reader: Dashboard's items, from the one rule and the reads Dashboard reads
  // (`features/dashboard/useWaitingItems.ts`), so the count is the number of Dashboard's items on
  // every view (dispatch://INFRA-5702/spec#b-r-header-count). Each read is kept fresh by the
  // events that name its key, never by its age (`EVENT_FRESH`), and none is the issue's whole
  // comments list: a Change is matched to its decision by its anchor, with no blocks read.
  const waiting = useWaitingItems(issue.key).items;
  // The document each approval request that waits on the reader is on, by artifact id: the
  // ask's own, else the one its approval names by slug, else the issue's spec. The header's
  // approval control for each carries Approve and Request changes. A stale approval is no
  // request and moves no control.
  const requested = waiting.flatMap((item) =>
    item.kind === "approval"
      ? [
          item.ask.artifact_id ??
            issue.artifacts?.find((artifact) => artifact.slug === item.ask.approval_artifact?.slug)
              ?.id ??
            issue.primary_artifact_id,
        ]
      : []
  );
  // The detail carries waiting_on on every open ask; a response without it cannot say whose
  // turn it is, so the indicator stays off rather than shown wrong.
  const knowsWhoseTurn = openAsks.every((ask) => ask.waiting_on !== undefined);
  const agentsTurn = openAsks.length - waitingOnHuman(openAsks).length;
  // The one shared read of the projects, under the key the shell's sidebar and the search
  // palette already hold, kept fresh by the feed's events as the count's reads are
  // (`EVENT_FRESH`), so a header mounted again does not send it a second time. Only a root of the
  // issue's project has gated steps at all (`isProjectRoot`: no parent, or a parent of another
  // project), so a child does not ask for the list, and while the read is pending or failed the
  // issue draws its one chip rather than claiming a gate it has not read.
  const projects = useQuery({ ...projectsQuery(), ...EVENT_FRESH, enabled: isProjectRoot(issue) });
  const projectRow = projects.data?.find((candidate) => candidate.key === issue.project);
  const entries = gatedStepEntries(projectRow, issue);
  // Which document a step reviews is `gated-steps.ts`'s to say, so the chip asks the drawn
  // entries rather than the gate's words: a gate that names no step for the primary document
  // leaves that document's own state and its two controls to the chip.
  const stepHoldsPrimaryDocument = entries.some(
    (entry) => entry.document?.id === issue.primary_artifact_id
  );
  const approval = documentArtifact?.approval;

  return (
    <div
      className={`flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-1.5 border-b px-3.5 pt-1 pb-2.5 text-sm leading-5 min-[761px]:gap-x-3 min-[761px]:px-7 min-[761px]:pt-0.5 min-[761px]:pb-[7px] ${pageLineBorder} ${pageMutedText}`}
      data-testid="issue-state-actions"
    >
      {/* The prototype's `.pill`: the status's shape and word inside a rule of its colour. The
          rule is drawn inside the pill's box, so the select can fill that box: 24 px tall at
          every width, the least a target may be (WCAG 2.5.8). The shape is an svg and not text,
          since the page's typeface lacks ◐; it hides itself from assistive technology and holds
          no words, so its span only centres it on the pill while the select stretches. */}
      <label
        className={`inline-flex min-h-6 shrink-0 items-stretch gap-1 rounded-full px-2 text-xs leading-[18px] font-semibold shadow-[inset_0_0_0_1px_currentColor] ${statusTone(status)}`}
      >
        <span className="self-center">
          <StatusGlyph glyph={statusGlyph(status)} />
        </span>
        {/* Sized to the status it shows, as the prototype's pill is; a browser with no
            `field-sizing` draws the select at its longest option, which is its own width. */}
        <select
          aria-label={PROPERTY_LABELS.status}
          className={`min-w-0 cursor-pointer appearance-none font-inherit outline-none [field-sizing:content] disabled:cursor-not-allowed disabled:opacity-50 ${bgTransparent} ${focusVisibleRing}`}
          disabled={isClosed}
          onChange={(event) => writes.pickStatus(event.target.value)}
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
          pick behind it, as the status does and as their own controls say: a press straight out
          of the title's editor lands on them. */}
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
      <PullRequestLink pullRequest={issue.pull_request} />
      <ChildrenProgress childIssues={issue.children ?? []} />
      {waiting.length > 0 ? (
        // The prototype's `.waitchip`: a control, because the reader acts on what it counts.
        <button
          className={`inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2.5 text-xs leading-[22px] font-semibold ${pageWaitingChip} ${focusVisibleRing}`}
          data-testid="issue-whose-turn"
          onClick={pressCount}
          type="button"
          {...pageChrome}
        >
          <span aria-hidden="true" className={`size-2 shrink-0 rounded-full ${pageWaitingBg}`} />
          {/* The words are one item, so the flex gap is the dot's alone: as items of their own,
              the brackets and the number each took the gap and the count drew "Waiting on
              you ( 2 )". */}
          <span>
            {PROPERTY_LABELS.waiting_on_you} (<span {...authorText}>{waiting.length}</span>)
          </span>
        </button>
      ) : agentsTurn > 0 && knowsWhoseTurn ? (
        <span
          className={`inline-flex shrink-0 items-center text-xs leading-[22px] font-semibold ${pageQuietText}`}
          data-testid="issue-whose-turn"
          {...pageChrome}
        >
          {/* One item, as the count's words are above, so they are laid out as a line of words
              and not as flex items side by side. */}
          <span>
            {PROPERTY_LABELS.waiting_on_agents} (<span {...authorText}>{agentsTurn}</span>)
          </span>
        </span>
      ) : null}
      {/* The approval control: a root issue of a gated project reads its gate step by
          step, and the one chip stays beside those steps whenever no step reviews the primary
          document, so an open request on that document can always be answered here. Every
          other issue keeps the one chip, which says "Not requested" while nobody has asked.
          Each document whose own request waits on the reader carries its own Approve and
          Request changes here, the chip and each entry alike, and no other does; they are
          Details' otherwise, with the steps' reasons. The chip, and each step's name, chip and
          two actions (`layout="contents"`), are items of this line, the two actions kept
          together as one: the approval control stands beside the count, and where the line has
          no room left the two actions take the line under it together. */}
      {documentArtifact === undefined || stepHoldsPrimaryDocument ? null : approval === undefined ||
        approval.state === "draft" ? (
        <span className={approvalPillClassName("draft", "compact", true)} {...pageChrome}>
          {CONTROL_LABELS.not_requested}
        </span>
      ) : (
        <ApprovalChip
          artifact={documentArtifact}
          keepActionsTogether
          layout="contents"
          showActions={requested.includes(documentArtifact.id)}
          size="compact"
          variant="header"
        />
      )}
      {entries.length === 0 ? null : (
        <ApprovalSteps
          actionsOn={requested}
          entries={entries}
          layout="contents"
          primaryArtifactId={issue.primary_artifact_id}
          reasons={false}
          size="compact"
        />
      )}
      {/* One marker, where the route reaches nobody; whether it reaches anyone otherwise, and
          the route itself, are Details'. It speaks for the saved route only. */}
      {writes.drafts.route === (issue.route ?? "") ? (
        <UnreachableRouteMarker issue={issue} showRoute />
      ) : null}
      <HeaderLabels labels={issue.labels ?? []} onOpenAll={onOpenLabels} />
    </div>
  );
}

/** How many segments the bar draws one child each before it is one bar instead: past this the
 *  segments, sharing the bar's 48 px at 760 px and below with a 2 px gap between each two, are
 *  each under 2 px wide, too narrow to tell one child from the next. */
const MOST_SEGMENTS = 12;

/**
 * The children's progress (the prototype's `.kids-prog`): the direct children done of
 * total, the bar, and the whole subtree done of total, each count with its label, because the
 * two numbers differ and a reader cannot tell which is which from the numbers alone. An issue
 * with no children draws none of it.
 *
 * Each count draws "Children 3 of 24" and "All issues under it 3 of 24", with no "done" at any
 * width, and carries its whole sentence as its name, "3 of 24 children done", for a reader who
 * hears it; the bar is 48 px wide at 760 px and below and 64 px above, whatever the number of
 * children: up to `MOST_SEGMENTS` children share it as one segment each, and past that it is one
 * bar filled to the share done. That keeps the line of a large root - 24 children, a gated step
 * waiting on the reader, 20 labels - inside the header's height limits.
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
  // The words a reader hears, in the order a sentence reads them; the drawn words lead with
  // the label, as the line does, and leave "done" to this name.
  const childrenName = [
    done,
    CONTROL_LABELS.count_of,
    childIssues.length,
    CONTROL_LABELS.children.toLowerCase(),
    CONTROL_LABELS.count_done,
  ].join(" ");
  const subtreeName = [
    subtree.done,
    CONTROL_LABELS.count_of,
    subtree.total,
    CONTROL_LABELS.count_of,
    CONTROL_LABELS.whole_tree.toLowerCase(),
    CONTROL_LABELS.count_done,
  ].join(" ");
  return (
    // Two items from 761 px up; at 760 px and below one item, the direct count, the bar, then
    // the whole tree's count, so the line wraps around it rather than between the two, and the
    // item wraps inside itself only where a line is narrower than it, as at 320 px.
    <span className="inline-flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1.5 min-[761px]:contents">
      {/* `role="img"`: a span's own name is not read, and an image's is, in place of what it
          draws, so the count is heard once, whole. */}
      <span
        aria-label={childrenName}
        className={`inline-flex shrink-0 items-center gap-2 font-medium ${pageInkText}`}
        data-testid="issue-progress-children"
        role="img"
        {...pageChrome}
      >
        <span>
          {CONTROL_LABELS.children} <span {...authorText}>{done}</span> {CONTROL_LABELS.count_of}{" "}
          <span {...authorText}>{childIssues.length}</span>
        </span>
        <span aria-hidden="true" className="inline-flex w-12 shrink-0 gap-0.5 min-[761px]:w-16">
          {childIssues.length > MOST_SEGMENTS ? (
            <span
              className={`relative block h-2 flex-1 overflow-hidden rounded-sm ${pageSegmentBg}`}
            >
              <span
                className={`absolute inset-y-0 left-0 block ${pageSegmentDoneBg}`}
                style={{ width: `${(100 * done) / childIssues.length}%` }}
              />
            </span>
          ) : (
            childIssues.map((child, index) => (
              <span
                className={`block h-2 flex-1 rounded-sm ${
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
        aria-label={subtreeName}
        className={`shrink-0 text-xs leading-5 ${pageMutedText}`}
        data-testid="issue-progress-subtree"
        role="img"
        {...pageChrome}
      >
        {CONTROL_LABELS.whole_tree} <span {...authorText}>{subtree.done}</span>{" "}
        {CONTROL_LABELS.count_of} <span {...authorText}>{subtree.total}</span>
      </span>
    </span>
  );
}

/**
 * The labels, in Dispatch's order, as many as fit on what is left of the line once every other
 * property is drawn, and then the count of those left over, which opens Details at its Labels
 * row, where every label is listed with its control. An issue whose labels fit draws them all
 * and no count. Where what is left of a line holds no label and the count has gone on to the
 * next line anyway, the labels go on with it.
 *
 * What fits is read off the page, never guessed at a width the typeface decides. For a reading
 * every chip is laid out with the count drawn at its widest, and that layout is read once
 * (`fitOf`): in a `ResizeObserver` notification, after the browser has laid the frame out itself
 * and before it paints it, with no write between the reads, so the reading forces no layout of
 * its own. The line draws the chips the box's first line holds beside the count, so it holds no
 * word it hides; where every chip would fit on the line with no count drawn, it draws them all
 * and no count. The count keeps the width it had in that reading, so the line breaks around it
 * as it did then and the count names every chip left out.
 *
 * A reading is taken whenever the labels change, again in the frame after the line's width
 * changes, and again whenever the document's fonts finish loading: the typeface can arrive after
 * the first reading (`font-display: swap`) and widen the chips while the line keeps its width.
 * The frame between draws the last reading's chips at the new widths: the box wraps and draws
 * its first line only, so a chip that no longer fits goes onto a line the box clips away rather
 * than past its edge, and each frame draws a chip whole or not at all. Each reading is one
 * `issue-header-labels` measure (`measured`), which the row of `c-header-labels` sums over a
 * load (`e2e/issue-page-n.e2e.ts`).
 */
function HeaderLabels({
  labels,
  onOpenAll,
}: {
  labels: readonly string[];
  onOpenAll: () => void;
}): ReactNode {
  const box = useRef<HTMLDivElement>(null);
  const count = useRef<HTMLButtonElement>(null);
  const key = labels.join("\n");
  const [fit, setFit] = useState<(LabelsFit & { readonly key: string }) | null>(null);
  // Null while the line is being read: every chip laid out, and the count at its widest.
  const drawn = fit?.key === key ? fit : null;
  // The labels a reading is owed for, and the observer that takes it.
  const owed = useRef<string | null>(null);
  const sizes = useRef<ResizeObserver | null>(null);
  // The observer watches the box's width and takes each reading. It is made in a layout effect,
  // in the commit that first lays the chips out, and so is the fonts' listener: added after the
  // page is drawn, the listener would miss a typeface that finished loading in that gap, and the
  // line would keep the fit it read with the old typeface.
  const hasLabels = labels.length > 0;
  useLayoutEffect(() => {
    const element = box.current;
    if (!hasLabels || element === null || typeof ResizeObserver === "undefined") return;
    const root = document.documentElement;
    // The box's width as last read, and, until the next frame begins, the frame of a reading,
    // whose own fit can change that width: a change the fit makes is none to read again for.
    let width = Number.NaN;
    let fitting = 0;
    const observer = new ResizeObserver(() => {
      const reading = owed.current;
      if (reading === null) {
        const now = element.getBoundingClientRect().width;
        if (now === width) return;
        width = now;
        // A width the line changes to is read in a commit of its own, laid out in the next frame.
        if (fitting === 0) setFit(null);
        return;
      }
      owed.current = null;
      observer.unobserve(root);
      const read = measured(() => fitOf(element, count.current));
      width = read.width;
      cancelAnimationFrame(fitting);
      fitting = requestAnimationFrame(() => {
        fitting = 0;
      });
      // Drawn before this frame is painted, so no frame draws the chips as they were read.
      flushSync(() => setFit({ ...read.fit, key: reading }));
    });
    sizes.current = observer;
    observer.observe(element);
    // The unit suite's DOM has no font set, and so no typeface to wait for.
    const fonts = "fonts" in document ? document.fonts : undefined;
    const readAgain = () => setFit(null);
    fonts?.addEventListener("loadingdone", readAgain);
    return () => {
      sizes.current = null;
      cancelAnimationFrame(fitting);
      observer.disconnect();
      fonts?.removeEventListener("loadingdone", readAgain);
    };
  }, [hasLabels]);
  // A commit that lays the chips out for a reading owes one, which the observer takes once the
  // browser has laid that commit out: the document's root, observed afresh, is notified after the
  // next layout and before the paint. It is the shallowest element there is, so a size the fit
  // drawn in that notification changes is notified in the same frame, the box's among them, and
  // none is left over for a "ResizeObserver loop" error.
  useLayoutEffect(() => {
    const observer = sizes.current;
    if (drawn !== null || observer === null) return;
    owed.current = key;
    observer.observe(document.documentElement);
  });
  if (labels.length === 0) return null;
  const left = labels.length - (drawn?.shown ?? 0);
  return (
    <>
      {/* One chip high - 20 px, the chip's 18 px line and its two borders, at any root size -
          wrapping and clipped: a chip that does not fit goes onto a line that is not drawn. A
          line always holds its first item, so that item is an empty one the height of a chip,
          which takes no width and, by its margin, no gap: the first chip wraps too when it does
          not fit. */}
      <div
        className="flex h-[20px] min-w-0 flex-[1_1_0] flex-wrap gap-1.5 overflow-hidden"
        data-testid="issue-header-labels"
        ref={box}
        style={drawn?.boxMinWidth === undefined ? undefined : { minWidth: drawn.boxMinWidth }}
      >
        <span aria-hidden="true" className="-mr-1.5 h-[20px] w-0 shrink-0" />
        {labels.slice(0, drawn?.shown ?? labels.length).map((label) => (
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
        // 24 px tall at every width (WCAG 2.5.8). Tabular digits, so a count is as wide as any
        // other of as many digits.
        <button
          className={`inline-flex min-h-6 shrink-0 items-center text-xs leading-5 font-semibold tabular-nums underline underline-offset-[3px] ${pageInkText} ${focusVisibleRing}`}
          data-testid="issue-labels-left"
          onClick={onOpenAll}
          ref={count}
          style={drawn?.countWidth === undefined ? undefined : { minWidth: drawn.countWidth }}
          type="button"
          {...pageChrome}
        >
          {/* One inline item for the words: a flex box drops the text that is only a space, and
              the count drew "+1label". */}
          <span>
            +<span {...authorText}>{left}</span>{" "}
            {left === 1 ? CONTROL_LABELS.labels_left_one : CONTROL_LABELS.labels_left_many}
          </span>
        </button>
      )}
    </>
  );
}

/** What the labels' line draws, as a reading decides it (`fitOf`): how many chips; the width the
 *  count keeps, the one it had in that reading; and, where the box goes on to the count's line
 *  with it, the least width that takes it there. */
interface LabelsFit {
  readonly boxMinWidth?: string;
  readonly countWidth?: number;
  readonly shown: number;
}

/** The measure each reading of the labels is (`measured`), which the row of `c-header-labels`
 *  reads off the page (`performance.getEntriesByName`) and the header's unit case reads too. */
export const LABELS_MEASURE = "issue-header-labels";

/** How far past the room a chip may end and still be read as fitting in it. A box is read as a
 *  float, off from the layout's own unit (1/64 px, a third of that at a device scale of 3) by a
 *  few ten-thousandths of a pixel at most: the slack is above that and below one unit, so a chip
 *  the browser wraps is never read as one that fits. */
const FIT_SLACK = 0.001;

/** `read`, wrapped whole in an `issue-header-labels` measure, so any layout it forces is inside
 *  the measure too. */
function measured<T>(read: () => T): T {
  const start = `${LABELS_MEASURE}:start`;
  performance.mark(start);
  try {
    return read();
  } finally {
    performance.measure(LABELS_MEASURE, start);
    performance.clearMarks(start);
  }
}

/**
 * Reads the labels' box as a reading lays it out - every chip, and the count at its widest - in
 * one run of reads with no write among them: the box's own box, each chip's, the count's and the
 * chips' gap, and the line's where the box goes on to the count's line. Taken after the browser's
 * own layout, it forces none. Answers the fit, and the box's width as read.
 *
 * The chips the box's first line holds whole beside the count, by the browser's own wrap, are the
 * ones the line draws. Drawn with no count, the box would take the count's room and the gap
 * before it as well, where the count stands on the box's line, and the line would break nowhere
 * else: every chip fits that room where the last would end within it were the chips one line,
 * each line of them going on one gap after the line above it ended. Where the count has gone on
 * to the next line and the box's first line holds no chip, the box takes the first chip's width,
 * or the line's where the chip is wider, and so goes on to the count's line, first on it. There
 * it is the line less the count and the gap before it where the count still fits beside it, and
 * the whole line where it does not, and it draws the chips that end within that.
 */
function fitOf(
  box: HTMLElement,
  count: HTMLElement | null
): { readonly fit: LabelsFit; readonly width: number } {
  const outer = box.getBoundingClientRect();
  const chips = Array.from(box.querySelectorAll<HTMLElement>("[data-header-label]"), (chip) =>
    chip.getBoundingClientRect()
  );
  const counter = count?.getBoundingClientRect();
  const gap = pixels(getComputedStyle(box).columnGap);
  const width = outer.width;
  // Where each chip would end, from the box's left, were the chips one line: against the first
  // chip of its own line it keeps its place, and its line starts one gap after the last ended.
  const ends: number[] = [];
  let top = 0;
  let start = 0;
  let from = 0;
  for (const chip of chips) {
    const last = ends.at(-1);
    if (last === undefined || Math.abs(chip.top - top) >= 0.5) {
      top = chip.top;
      start = chip.left;
      from = last === undefined ? 0 : last + gap;
    }
    ends.push(from + chip.right - start);
  }
  /** How many chips, from the first, end within `room`. */
  const within = (room: number): number => {
    const over = ends.findIndex((end) => end > room + FIT_SLACK);
    return over === -1 ? ends.length : over;
  };
  if (counter === undefined) return { fit: { shown: chips.length }, width };
  const beside = counter.top < outer.bottom;
  if (within(beside ? counter.right - outer.left : width) === ends.length) {
    return { fit: { shown: chips.length }, width };
  }
  const wrapped = chips.findIndex(
    (chip) => chip.top > outer.top + 0.5 || chip.right > outer.right + FIT_SLACK
  );
  let shown = wrapped === -1 ? chips.length : wrapped;
  let boxMinWidth: string | undefined;
  const line = box.parentElement;
  if (shown === 0 && !beside && line !== null) {
    const least = Math.ceil(ends[0] ?? 0);
    const style = getComputedStyle(line);
    const across =
      line.getBoundingClientRect().width -
      pixels(style.paddingLeft) -
      pixels(style.paddingRight) -
      pixels(style.borderLeftWidth) -
      pixels(style.borderRightWidth);
    const after = pixels(style.columnGap) + counter.width;
    const room = Math.min(least, across) + after <= across + FIT_SLACK ? across - after : across;
    shown = within(room);
    boxMinWidth = `min(${least}px, 100%)`;
  }
  return { fit: { boxMinWidth, countWidth: counter.width, shown }, width };
}

/** A computed length in px, and 0 where it is none. */
function pixels(value: string): number {
  const read = Number.parseFloat(value);
  return Number.isFinite(read) ? read : 0;
}
