import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";

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
  pageWaitingBg,
  pageWaitingChip,
} from "../../theme/classes";
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
import { APPROVAL_CHIP } from "./approval-chip-box";
import { presetPath } from "./issue-address";
import { statusGlyph, statusTone } from "./issue-status";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { StatusGlyph } from "./StatusGlyph";
import { coverageStatements } from "./tree-drafts";
import { treeRead, useTreeRead, viewQuery } from "./tree-read/tree-read";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";
import { type WaitingPress, waitingOnReader } from "./waiting";

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
  const navigate = useNavigate();
  const { pathname } = useLocation();
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
  // What the count counts, read from what the outline already reads under the same keys - the
  // reader's login, the coverage's statements, and the decisions the tree read answers as nodes,
  // with each Change it hangs under one, whose node says whether its author deleted it - and
  // kept fresh by the outline's own rule
  // (`reference-target.ts`'s `EVENT_FRESH`), but for the tree read, which keeps its own
  // (`TREE_FRESH`): events refresh them, never their age (decision 180). Staleness is per
  // observer, so each read here takes that rule too, and the count costs no request of its own
  // (`waiting.ts`). A statement is one by the rule the outline's review list reads
  // (`coverageStatements`), so the count counts the very decisions the outline marks as waiting.
  // The decisions, and each Change that read hangs under its decision, are the decisions view
  // read's wherever the page holds it, which holds each of them however many pages of the tree
  // read the page holds, watched under the key the page reads it by and never sent (`treeRead`),
  // so the count sends no view read of its own; Conversation's read stands in until then.
  const viewer = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login;
  const coverage = useQuery({ ...issueCoverageQuery(issue.key), ...EVENT_FRESH });
  const tree = useTreeRead(issue).answer;
  const decisions = useQuery(treeRead(viewQuery(issue.key, "decisions"))).data;
  const answer = decisions ?? tree;
  const isCoverageStatement = useMemo(
    () => coverageStatements(issue, coverage.data),
    [issue, coverage.data]
  );
  const waiting = useMemo(
    () => waitingOnReader(issue, viewer, isCoverageStatement, { answer }),
    [issue, viewer, isCoverageStatement, answer]
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
  const line = useRef<HTMLDivElement>(null);

  // The count's press, where an ask or a decision waits: it sets the Decisions filter's address
  // and leaves the press in that history entry as its `WaitingPress` mark, one number per press.
  // This part draws no view at that address; a later part's Decisions view reads the mark to put
  // the focus on the next item there that waits, each press on the one after the last. An
  // approval request alone puts the focus on the first chip or entry on this line of a document
  // whose request waits. Where the header draws no chip or entry of the waiting document -
  // another document of a child, or a document no step of a gated root names - the request is
  // counted and the press does nothing in the header, rather than put the focus on another
  // document's chip. A press made on the filter already replaces its entry, so Back leaves the
  // filter rather than walking back through the presses.
  const goToWaiting = () => {
    if (waiting.asks + waiting.decisions > 0) {
      const press: WaitingPress = { waitingPress: performance.now() };
      const path = presetPath(issue.key, "decisions");
      navigate(path, { replace: pathname === path, state: press });
      return;
    }
    const control = [...(line.current?.querySelectorAll<HTMLElement>(APPROVAL_CHIP) ?? [])].find(
      (found) => waiting.approvals.includes(found.dataset.approvalArtifact ?? "")
    );
    control?.focus();
  };

  return (
    <div
      className={`flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-1.5 border-b px-3.5 pt-1 pb-2.5 text-sm leading-5 min-[761px]:gap-x-3 min-[761px]:px-7 min-[761px]:pt-0.5 min-[761px]:pb-[7px] ${pageLineBorder} ${pageMutedText}`}
      data-testid="issue-state-actions"
      ref={line}
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
          {/* The words are one item, so the flex gap is the dot's alone: as items of their own,
              the brackets and the number each took the gap and the count drew "Waiting on
              you ( 2 )". The number carries "+" while the read its decisions are counted from
              names a next page (R15), as each count of the view row does. */}
          <span>
            {PROPERTY_LABELS.waiting_on_you} (
            <span {...authorText}>
              {waiting.total}
              {waiting.more === true ? "+" : ""}
            </span>
            )
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
        <span className={approvalPillClassName("draft", "compact")} {...pageChrome}>
          {CONTROL_LABELS.not_requested}
        </span>
      ) : (
        <ApprovalChip
          artifact={documentArtifact}
          keepActionsTogether
          layout="contents"
          showActions={waiting.approvals.includes(documentArtifact.id)}
          size="compact"
          variant="header"
        />
      )}
      {entries.length === 0 ? null : (
        <ApprovalSteps
          actionsOn={waiting.approvals}
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
export function ChildrenProgress({
  childIssues,
}: {
  childIssues: readonly IssueChild[];
}): ReactNode {
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
 * What fits is read off the page, never guessed at a width the typeface decides: every chip is
 * laid out, first with no count drawn, and where they all fit on the box's first line the line
 * draws them all and no count; otherwise they are laid out again with the count drawn at its
 * widest, and the chips past the end of the box's first line are then left out, so the line
 * holds no word it hides. The count keeps the width it had in that reading, so the line breaks
 * around it as it did then and the count names every chip left out. That reading is taken in
 * the commit that lays the chips out whenever the labels change, again on the next frame
 * whenever the line's width does, and again whenever the document's fonts finish loading: the
 * typeface can arrive after the first reading (`font-display: swap`) and widen the chips while
 * the line keeps its width. The frame between draws the last reading's chips at the new
 * widths: the box wraps and draws its first line only, so a chip that no longer fits goes onto a
 * line the box clips away rather than past its edge, and each frame draws a chip whole or not
 * at all.
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
  const [fit, setFit] = useState<{ readonly key: string; readonly shown: number } | null>(null);
  // Null while the line is being read: every chip laid out, and the count at its widest.
  const shown = fit?.key === key ? fit.shown : null;
  useLayoutEffect(() => {
    const element = box.current;
    if (shown !== null || element === null) return;
    element.style.minWidth = "";
    const counter = count.current;
    // First with no count, and from its own width again after: labels that all fit on the line
    // alone draw no count, where a reading with the count beside them would leave one out
    // for the count's room.
    if (counter !== null) {
      counter.style.minWidth = "";
      counter.style.display = "none";
    }
    const all = labelsThatFit(element) === labels.length;
    if (counter !== null) counter.style.display = "";
    if (all) {
      setFit({ key, shown: labels.length });
      return;
    }
    let fits = labelsThatFit(element);
    // Where what is left of a line holds not even the first chip and the count has gone on to
    // the next line anyway, the box takes that chip's width, or the line's where the chip is
    // wider, and so goes on with the count: that costs the header no line and draws the labels
    // the next line has room for (a property with a value is drawn). Every reading is
    // taken before the commit is drawn.
    const first = element.querySelector<HTMLElement>("[data-header-label]");
    const countTop = counter?.getBoundingClientRect().top;
    if (
      fits === 0 &&
      first !== null &&
      countTop !== undefined &&
      countTop >= element.getBoundingClientRect().bottom
    ) {
      element.style.minWidth = `min(${Math.ceil(first.getBoundingClientRect().width)}px, 100%)`;
      fits = labelsThatFit(element);
    }
    // The count drawn next names fewer labels and can be narrower: at its own width it would
    // leave room the line breaks differently around - onto the line it had wrapped from - and
    // the box, narrower then, would hide chips the count does not name. Held at the width it
    // has in this reading, the line breaks as it was read.
    if (counter !== null) counter.style.minWidth = `${counter.getBoundingClientRect().width}px`;
    setFit({ key, shown: fits });
  });
  // A width the line changes to is read again on the next frame, so the reading is a commit
  // of its own rather than one inside the observer's notification. A typeface that loads after
  // the reading can widen the chips while the box keeps its width, which the observer does not
  // see, so the line is read again on each `loadingdone` of the document's fonts too. Both are
  // added in a layout effect, which runs in the same commit right after the reading above, so no
  // event falls between the two. Added after the page is drawn, they would miss a typeface that
  // finished loading in that gap: its `loadingdone` would fire with no listener, and the
  // observer's first notification would find the width unchanged, so the line would keep the
  // fit it read with the old typeface.
  const hasLabels = labels.length > 0;
  useLayoutEffect(() => {
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
    // The unit suite's DOM has no font set, and so no typeface to wait for.
    const fonts = "fonts" in document ? document.fonts : undefined;
    const readAgain = () => setFit(null);
    fonts?.addEventListener("loadingdone", readAgain);
    return () => {
      cancelAnimationFrame(frame);
      observer.disconnect();
      fonts?.removeEventListener("loadingdone", readAgain);
    };
  }, [hasLabels]);
  if (labels.length === 0) return null;
  const left = labels.length - (shown ?? 0);
  return (
    <>
      {/* `relative`: the chips' offsets are read against this box (`labelsThatFit`). One chip
          high - 20 px, the chip's 18 px line and its two borders, at any root size - wrapping
          and clipped: a chip that does not fit goes onto a line that is not drawn. A line always
          holds its first item, so that item is an empty one the height of a chip, which takes no
          width and, by its margin, no gap: the first chip wraps too when it does not fit. */}
      <div
        className="relative flex h-[20px] min-w-0 flex-[1_1_0] flex-wrap gap-1.5 overflow-hidden"
        data-testid="issue-header-labels"
        ref={box}
      >
        <span aria-hidden="true" className="-mr-1.5 h-[20px] w-0 shrink-0" />
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
        // 24 px tall at every width (WCAG 2.5.8). Tabular digits, so a count is as wide as any
        // other of as many digits.
        <button
          className={`inline-flex min-h-6 shrink-0 items-center text-xs leading-5 font-semibold tabular-nums underline underline-offset-[3px] ${pageInkText} ${focusVisibleRing}`}
          data-testid="issue-labels-left"
          onClick={onOpenAll}
          ref={count}
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

/** How many of the header's label chips stand on the box's first line and wholly inside it, in
 *  order: the first that wrapped below it or reaches past its end ends the run, as the line
 *  reads left to right. The box is the chips' offset parent, so its top is 0. */
function labelsThatFit(box: HTMLElement): number {
  const width = box.clientWidth;
  let fit = 0;
  for (const chip of box.querySelectorAll<HTMLElement>("[data-header-label]")) {
    if (chip.offsetTop > 0 || chip.offsetLeft + chip.offsetWidth > width + 0.5) break;
    fit += 1;
  }
  return fit;
}
