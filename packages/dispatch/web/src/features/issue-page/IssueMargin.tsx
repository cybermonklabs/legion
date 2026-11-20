import { useQuery } from "@tanstack/react-query";
import {
  type FocusEventHandler,
  type ReactNode,
  type Ref,
  type RefCallback,
  type RefObject,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { flushSync } from "react-dom";
import { Link } from "react-router-dom";

import { api, apiErrorMessage } from "../../api/client";
import { projectsQuery, referenceTargetsQuery, whoAmIQuery } from "../../api/queries";
import type { Artifact, IssueDetails, UserState } from "../../api/types";
import {
  pageAvatar,
  pageFailedText,
  pageHeaderControl,
  pageInkText,
  pageLabelChip,
  pageQuietText,
  pageRowBorder,
  pageRowHoverBg,
} from "../../theme/classes";
import { ApprovalSteps } from "../issue/ApprovalSteps";
import { GitHubLink } from "../issue/GitHubLink";
import { gatedStepEntries, STEP_LABELS } from "../issue/gated-steps";
import { IssueComponentsLine } from "../issue/IssueComponentsLine";
import { IssueLabels } from "../issue/IssueLabels";
import { stateForIssue } from "../issue/pins";
import { RepoControl, type RepoControlFailure } from "../issue/RepoControl";
import { addPriorityControlName, IssuePriorityLine } from "../priorities/IssuePriorityLine";
import { AgentName } from "../refs/AgentName";
import { ReferencedBy } from "../refs/ReferencedBy";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { focusOnDocument } from "../shell/roving";
import {
  BLOCKED_BY_INPUT_ID,
  BlockedByControl,
  blockedByControlName,
  IssueLine,
} from "./BlockedByControl";
import {
  DetailsErrorLine,
  readFailed,
  retryRead,
  useFocusHandOff,
  useSaidFailure,
} from "./details-error";
import {
  CIRCLED_DOT,
  FLAG,
  GRID_SQUARE,
  HOUSE,
  LINED_SQUARE,
  RIGHT_OVER_LEFT_ARROWS,
  SMILING_FACE,
  SQUARE_IN_SQUARE,
  TICKED_BOX,
  UPRIGHT_BAR,
} from "./details-icons";
import { DetailsFocusedNode, DetailsHistory, DetailsPeople } from "./details-more";
import { DETAILS_ROW_ATTRIBUTE, DETAILS_ROW_REQUEST_EVENT, type DetailsRow } from "./details-rows";
import { IssueAddMenu } from "./IssueAddMenu";
import { IssueClaimControl } from "./IssueClaimControl";
import { type IssueField, IssueFieldForms } from "./IssueFieldForms";
import { IssuePins } from "./IssuePins";
import { IssueRouteControl } from "./IssueRouteControl";
import { statusGlyph, statusTone } from "./issue-status";
import { useMarginDrawer, useMarginDrawsSlot } from "./margin-slot";
import { NoTooltips } from "./NoTooltips";
import {
  authorText,
  MARGIN_SECTION_NAMES,
  type MarginSection,
  PROPERTY_LABELS,
  pageChrome,
} from "./page-structure";
import { CONTROL_LABELS, FILE_WORDS, issueStatusWord, PROGRESS_NOUNS } from "./page-words";
import { ROW_LINK } from "./row-references";
import { StatusGlyph } from "./StatusGlyph";
import { firstUsable, usableControl } from "./usable-control";
import { useIssueBlockedBy } from "./useIssueBlockedBy";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";
import { type MarginSections, useMarginSections } from "./useMarginSections";
import { waitingOnReader } from "./waiting";

/** How many entries a list of unbounded length draws before it folds behind a count that opens
 *  the rest. */
const FOLD_AFTER = 10;

/** The id of the Parent row's line that says why Dispatch refused a parent. */
export const PARENT_ERROR_ID = "issue-parent-error";

/** The rows of each section that a control elsewhere on the page opens Details at
 *  (`DetailsRow`), each where it is drawn: the section opens for a request of one of them.
 *  People holds the issue's collaborators, whose place there is `DetailsPeople`. */
const SECTION_ROWS: Readonly<Record<MarginSection, readonly DetailsRow[]>> = {
  development: ["labels", "parent", "route"],
  people: ["collaborators"],
  references: [],
};

/**
 * The margin's own side of the issue page's panel: the margin's cards, and - where the margin
 * keeps a column for that panel - the host the panel is moved into, under them.
 *
 * The panel is drawn once, by the details slot, and appended to this host rather than drawn
 * afresh in it (`slots/details.tsx`): a reader who has typed a message, opened a blocked-by edit
 * or opened one of the panel's lists still has it when Details is closed and opened again.
 * Whether the margin keeps that column is read here as the slot reads it (`margin-slot.tsx`),
 * so the host answers one change of it in one commit.
 */
export function IssueMarginCards({
  conversation,
  panelHost,
}: {
  /** The margin's own content, which this wraps where the panel sits inside the shell's
   *  margin. */
  conversation?: ReactNode;
  /** Takes the host the page's panel is drawn in, where the margin keeps a column for it; it
   *  is handed no host wherever the margin draws its own cards alone. The wrapper is the same
   *  element in both states on purpose: the margin holds the open composer inside it, and a
   *  wrapper that changed shape as the margin collapsed would remount that composer and lose an
   *  unsent comment (`features/margin/MarginSheet.tsx`). */
  panelHost: RefCallback<HTMLElement>;
}): ReactNode {
  const drawsPanel = useMarginDrawsSlot();
  return (
    // The panel is the page's, wherever the shell mounts it, and the page draws no tooltips
    // (`NoTooltips.tsx`): the margin's own cards reach that reading through this wrapper,
    // because they are rendered inside it (`margin-slot.tsx`).
    <NoTooltips>
      <div
        className={drawsPanel ? "flex min-w-0 flex-col gap-3" : "contents"}
        data-testid={drawsPanel ? "issue-margin" : undefined}
        ref={drawsPanel ? panelHost : undefined}
      >
        {/* The margin's own cards first, and the panel's own sections under them, because the
            page's panel is appended to this wrapper (`slots/details.tsx`'s `holdPanel`): the
            threads anchored in an open document and its composer, in the margin's words and the
            authors' own, and not the page's declared vocabulary (`page-structure.ts`). They are
            first because the margin places an anchored card at a `top` it measures down from
            the top of its own content (`margin/ThreadList.tsx`'s `regionOrigin`), so anything
            drawn over them moves that anchor and a card can no longer reach the line its mark
            is on. */}
        <div className="contents" {...authorText}>
          {conversation}
        </div>
      </div>
    </NoTooltips>
  );
}

/**
 * Details, the issue page's one panel of properties (the prototype's `#details` and the
 * picture page-v3-details-1440): every property the header does not hold, each a row of its
 * own - a label column with a shape for the property and its name, and the value beside it,
 * `None` where the issue has none - with every control the page had for it, and nothing
 * read-only. A field with no value is reachable from the one add control, and only a list of
 * unbounded length folds, after its first ten entries, behind a count that opens the rest. The
 * rows sit in three named sections the reader can collapse, kept for the next issue.
 *
 * Under the properties, the reader's pins as links to their rows. It holds no list of the
 * conversation: the outline is the conversation. The people, the facts of the focused row and
 * the history each have one of the three places `details-more.tsx` exports.
 *
 * The details slot draws it once and appends it to the shell's drawer, so nothing in it is
 * thrown away as the reader closes and opens Details. The titles of the issues this one waits
 * on, what waits on it and its links on GitHub it reads only while Details and its Development
 * are open, and what references the issue only while Details and its References are open with
 * its Referenced by list, so a load that leaves Details shut costs no request for any of them.
 */
export function IssueMarginPanel({
  documentArtifact,
  drawerBox = null,
  editingField,
  isClosed,
  issue,
  onEditingField,
  state,
  writes,
}: {
  /** The issue's primary document, whose approval the Approval row draws. */
  documentArtifact: Artifact | undefined;
  /** The Details drawer's `aside` the panel is placed in (`slots/details.tsx`), which a control
   *  elsewhere on the page sends its request for a row to, and which Add watches in the panel's
   *  place (`IssueAddMenu`); `null` while the panel is in none. */
  drawerBox?: HTMLElement | null;
  /** The editor open in its row - the parent's or the route's - or `null`. */
  editingField: IssueField | null;
  isClosed: boolean;
  issue: IssueDetails;
  onEditingField: (field: IssueField | null) => void;
  /** The reader's own state of every issue, whose pins the panel lists. */
  state: UserState | undefined;
  /** The details slot's set of the issue's writes (`useIssueHeaderWrites.ts`). */
  writes: IssueHeaderWrites;
}): ReactNode {
  const sections = useMarginSections();
  const [referencesOpen, setReferencesOpen] = useState(false);
  const [filesOpen, setFilesOpen] = useState(false);
  const filesList = useRef<HTMLUListElement>(null);
  // The panel's own element, under which every control of Details that Add reaches is drawn.
  const panel = useRef<HTMLDivElement>(null);
  const referencesPanelId = useId();
  // The blocked-by editor's state is the panel's, which stays drawn for the life of the page
  // (`slots/details.tsx`), as a closed section keeps its rows: a list typed before the reader
  // closed Development, and a refusal that lands after, are still there when it opens again
  // (`BlockedByControl`).
  const [blockedByEditing, setBlockedByEditing] = useState(false);
  const [blockedByDraft, setBlockedByDraft] = useState("");
  const blockedByWrite = useIssueBlockedBy(issue.key, () => setBlockedByEditing(false));
  // The row's own control takes the focus back as the reader leaves its editor - by a save, by
  // Cancel or by Escape - since the box that held it goes with the editor; only where the focus
  // fell to the document, the one focus the page takes back for the reader (`shell/roving.ts`).
  // It passes over a control that takes no press (`usableControl`). On a closed issue the Parent
  // row draws no control, so the focus goes to the parent's link where the issue has a parent,
  // and the route's control is disabled; where the row holds nothing that takes the focus, it
  // goes to Development's heading, the button that opens and closes the section these rows are in.
  const parentControl = useRef<HTMLButtonElement>(null);
  const parentValue = useRef<HTMLDivElement>(null);
  const routeControl = useRef<HTMLButtonElement>(null);
  const routeValue = useRef<HTMLDivElement>(null);
  const developmentHeading = useRef<HTMLButtonElement>(null);
  const leftEditor = useRef<IssueField | null>(null);
  const closeEditor = () => {
    leftEditor.current = editingField;
    onEditingField(null);
  };
  useEffect(() => {
    const left = leftEditor.current;
    leftEditor.current = null;
    if (left === null || editingField !== null || !focusOnDocument()) return;
    const rowControls =
      left === "parent"
        ? [parentControl.current, parentValue.current?.querySelector<HTMLElement>("a[href]")]
        : [routeControl.current];
    firstUsable(...rowControls, developmentHeading.current)?.focus();
  }, [editingField]);
  // The failure of Details' last field save, which the route's row says when it is a route's.
  // The focus of that line's Retry goes on in the row as the line goes: to the row's control, and
  // where that takes no press - not drawn while the route's editor is open, disabled on a closed
  // issue - to the editor's box, the one box of the row; and where that takes none either, on a
  // closed issue, to Development's heading.
  const fieldFailure = useSaidFailure(writes.updateIssue);
  const routeRetryLeft = useFocusHandOff(() =>
    firstUsable(
      routeControl.current,
      routeValue.current?.querySelector<HTMLElement>("input"),
      developmentHeading.current
    )
  );
  const issueReference = buildDispatchReference({ key: issue.key, kind: "issue" });
  // Four reads Details makes beyond the issue - the titles of the issues this one waits on, what
  // waits on it, its links on GitHub, and what references it - are made only while the drawer is
  // open, so a load that leaves it shut costs no request for them.
  const reading = useMarginDrawer()?.open === true;
  // A closed section keeps its rows, hidden (`PropertySection`), so each of those reads is made
  // only while its own section is open too - the first three are of rows in Development, where a
  // link reads GitHub as it is drawn, and the fourth is the References row's list: a reader who
  // keeps a section closed costs none of its reads, nor any of them again on an event. The
  // section's other rows make their own reads, as they do while Details itself is closed.
  const readingDevelopment = reading && sections.isOpen("development");
  const readingReferences = reading && sections.isOpen("references");
  const blockers = issue.blocked_by ?? [];
  const blockerTitles = useQuery({
    ...referenceTargetsQuery(blockers.map((key) => buildDispatchReference({ key, kind: "issue" }))),
    enabled: readingDevelopment && blockers.length > 0,
  });
  const titleOf = (key: string) => {
    const reference = buildDispatchReference({ key, kind: "issue" });
    return blockerTitles.data?.targets.find((target) => target.ref === reference)?.name;
  };
  // What waits on this issue is the other direction of `blocked_by`: the references of that
  // kind that point at it, each issue's title the head of its text that the edge carries as its
  // excerpt (the issue's node itself carries no text). It is kept under the backlink panels'
  // `["references", "to", <reference>]`, so the event that starts or stops an issue waiting on
  // this one reads it again (`api/event-refresh.ts`'s rule for `blocked_by`).
  const blocking = useQuery({
    enabled: readingDevelopment,
    queryFn: () => api.getReferences(issueReference, { kind: "blocked_by" }),
    queryKey: ["references", "to", issueReference, "blocked_by"],
  });
  const blocks = (blocking.data?.edges ?? []).flatMap((edge) =>
    edge.kind !== "blocked_by" || edge.node.issue_key === undefined
      ? []
      : [{ key: edge.node.issue_key, title: edge.excerpt?.text ?? "" }]
  );
  // The line of a read that failed goes once the read answers, and the focus its Retry held goes
  // on in the row rather than to the page. In Blocked by it goes to the row's control, found by
  // its name as the add menu finds it (`blockedByControlName`); where the row draws none - on a
  // closed issue, or while its editor is open - to the first issue the row waits on; and where
  // it waits on none while the editor is open, as another session's write can leave it, to the
  // editor's box. Each passes over a control that takes no press (`usableControl`), and where the
  // row holds none that takes it, the focus goes to Development's heading, the button that opens
  // and closes the section the row is in. In Blocks, which has no control, it goes to the first
  // issue the answer drew, or else to the None it says.
  const blockedByValue = useRef<HTMLDivElement>(null);
  const titlesRetryLeft = useFocusHandOff(() =>
    firstUsable(
      blockedByValue.current?.querySelector<HTMLElement>(
        `[aria-label="${CSS.escape(blockedByControlName(issue.key, blockers.length))}"]`
      ),
      blockedByValue.current?.querySelector<HTMLElement>("a[href]"),
      blockedByValue.current?.querySelector<HTMLElement>(`#${BLOCKED_BY_INPUT_ID}`),
      developmentHeading.current
    )
  );
  const blocksValue = useRef<HTMLDivElement>(null);
  const blocksNone = useRef<HTMLSpanElement>(null);
  const blocksRetryLeft = useFocusHandOff(
    () => blocksValue.current?.querySelector<HTMLElement>("a[href]") ?? blocksNone.current
  );
  // Details draws the priority's line in one of two places: beside Add while the issue advances
  // none, and in the Advances row once it advances one. The first priority added and the last one
  // removed take the line out of the place that held the focus, and the focus goes on to the line
  // in its other place rather than to the page. Out of the place beside Add it goes to the
  // Advances row's add control: the add control beside Add is where the focus is once an add
  // lands, so the reader stays on the control of that name, whose next press adds another, with
  // Remove of what they added just before it; a line at the cap draws no add control, and there
  // it goes to the row's first priority. Out of the Advances row it goes to the add control
  // beside Add, the one control the line draws there. Each passes over a control that takes no
  // press (`usableControl`), and where none is left the focus goes to Development's heading, the
  // button that opens and closes the section these rows are in. On a closed issue each add
  // control is disabled and holds no focus, so out of the place beside Add the focus goes to the
  // row's first priority, and out of the Advances row to that heading.
  const advancesValue = useRef<HTMLDivElement>(null);
  const addRow = useRef<HTMLDivElement>(null);
  const priorityAddSelector = `[aria-label="${CSS.escape(addPriorityControlName(issue.key))}"]`;
  const priorityBesideAddLeft = useFocusHandOff(() =>
    firstUsable(
      advancesValue.current?.querySelector<HTMLElement>(priorityAddSelector),
      advancesValue.current?.querySelector<HTMLElement>("a[href]"),
      developmentHeading.current
    )
  );
  const priorityRowLeft = useFocusHandOff(() =>
    firstUsable(
      addRow.current?.querySelector<HTMLElement>(priorityAddSelector),
      developmentHeading.current
    )
  );
  // The Repository row's control draws its own failed read of the mappings and its own refused
  // write, and hands each to the row to say (`sayFailure`), as Details says its own. A read's
  // line keeps its words while its Retry reads again, as Details' own read lines do: the read
  // that is out holds no error, so words taken from the error would change as Retry is pressed. A
  // press of a write's Retry clears the refusal as it sends the pick again, taking the line with
  // it, so that Retry never waits on the write it sent and reads no busy. As a line goes with a
  // reader's focus on its Retry - a write's at the press of that Retry, a read's once the
  // mappings answer - the focus goes on to the row's own control: the box, drawn while a write is
  // out, else the text or None the row draws in its place; and where the row holds nothing that
  // takes it, on a closed issue, to Development's heading. On a closed issue a write's line has no
  // Retry: Dispatch refuses every repository of a closed issue (`ISSUE_CLOSED`), so the pick sent
  // again would be refused again.
  const repoValue = useRef<HTMLDivElement>(null);
  const repoRetryLeft = useFocusHandOff(() =>
    firstUsable(
      repoValue.current?.querySelector<HTMLElement>("input"),
      ...(repoValue.current?.querySelectorAll<HTMLElement>("button") ?? []),
      developmentHeading.current
    )
  );
  const sayRepoFailure = (failure: RepoControlFailure): ReactNode =>
    failure.kind === "write" ? (
      <DetailsErrorLine
        message={failure.message}
        onRetry={isClosed ? undefined : failure.retry}
        retryRef={repoRetryLeft}
      />
    ) : readFailed(failure.read) ? (
      <DetailsErrorLine
        failure={failure.read.errorUpdateCount}
        message={failure.message}
        onRetry={retryRead(failure.read)}
        retryRef={repoRetryLeft}
        retrying={failure.read.isFetching}
      />
    ) : null;
  // Another session's close disables some of Development's controls - the route's control and
  // its editor's box, the labels' control, the priority's line, the Repository row, the parent's
  // editor's box - and takes others off the page - Set or Edit parent issue, Blocked by's control,
  // Add and its menu - and each takes the focus it held with it to the page's body. No hand-off
  // above answers that, since each acts only as an element it holds leaves the page or as an
  // editor closes. So the panel keeps the element the focus last went to in Development, which a
  // focus anywhere else forgets, as does the reader's leaving it while it still takes a press;
  // and as the issue closes, where that element takes no press now (`usableControl`) and the
  // focus is still on it or on the body, the focus goes to Development's heading, where every
  // other hand-off of the section ends. A focus a hand-off already moved to a control that takes
  // it stays there. An issue closed as the panel is drawn moves nothing.
  const developmentFocus = useRef<HTMLElement | null>(null);
  const noteDevelopmentFocus: FocusEventHandler<HTMLElement> = (event) => {
    developmentFocus.current = event.target;
  };
  // A press on Details' plain text moves the focus to the body and sends no `focusin`, so the
  // listener below never hears the reader leave, and a later close would move the focus to the
  // heading of a section the reader had already left. An element that still takes a press as the
  // focus leaves it was left by the reader, so it is forgotten. The elements a close takes the
  // focus from stay noted: a control a close disables takes no press as Chromium blurs it, and
  // React sends no `onBlur` for an element it takes off the page. A switch to another window or
  // tab blurs the control too, but leaves it the document's active element, where the focus comes
  // back on return, so a blur that leaves the element active forgets nothing.
  const forgetLeftDevelopmentFocus: FocusEventHandler<HTMLElement> = (event) => {
    if (event.target !== document.activeElement && usableControl(event.target)) {
      developmentFocus.current = null;
    }
  };
  useEffect(() => {
    // In the capture phase of the document, so it runs before the section's `onFocus`, which
    // React calls from its root and which notes the focus again where it went to Development. It
    // also forgets what the section's `onBlur` keeps: an element that was busy as the reader left
    // it for another.
    const forget = () => {
      developmentFocus.current = null;
    };
    document.addEventListener("focusin", forget, true);
    return () => document.removeEventListener("focusin", forget, true);
  }, []);
  const wasClosed = useRef(isClosed);
  // A layout effect, so it runs in the commit that closes the issue, after the hand-offs above,
  // and before a browser that moves the focus off a disabled control only later does so.
  useLayoutEffect(() => {
    const closing = isClosed && !wasClosed.current;
    wasClosed.current = isClosed;
    const left = developmentFocus.current;
    if (!closing || left === null || usableControl(left)) return;
    if (document.activeElement !== left && !focusOnDocument()) return;
    developmentHeading.current?.focus({ preventScroll: true });
  }, [isClosed]);
  // Approve and Request changes are here wherever no approval request waits on the reader,
  // and in the header while one does: the approvals are all this reads of the count, so it
  // hands it no decisions.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const approvalWaits =
    waitingOnReader(issue, viewer, undefined, () => false, undefined, undefined).approvals.length >
    0;
  // The status as the header draws it: the one being saved while any caller's write of it is
  // in flight, so the page never says two statuses at once.
  const status = writes.pendingStatus ?? issue.status;
  const projects = useQuery({ ...projectsQuery(), enabled: issue.parent === null });
  const gated = gatedStepEntries(
    projects.data?.find((candidate) => candidate.key === issue.project),
    issue
  );
  const approvalEntries = gated.some((entry) => entry.document?.id === issue.primary_artifact_id)
    ? gated
    : [{ document: documentArtifact, label: STEP_LABELS.spec, step: "spec" as const }, ...gated];
  const pullRequests = (issue.external_links ?? []).filter((link) => link.kind === "github_pr");
  const otherLinks = (issue.external_links ?? []).filter((link) => link.kind !== "github_pr");
  const labels = issue.labels ?? [];
  const advances = issue.advances ?? [];
  const artifacts = issue.artifacts ?? [];
  const files = filesOpen ? artifacts : artifacts.slice(0, FOLD_AFTER);
  const none = (
    <span className={pageQuietText} {...pageChrome}>
      {CONTROL_LABELS.none}
    </span>
  );

  return (
    <div className="flex min-w-0 flex-col gap-3" ref={panel}>
      <PropertySection
        drawerBox={drawerBox}
        headingRef={developmentHeading}
        name="development"
        onBlur={forgetLeftDevelopmentFocus}
        onFocus={noteDevelopmentFocus}
        sections={sections}
      >
        <Row glyph={<StatusGlyph glyph={STATUS_ROW_SHAPE} />} label={PROPERTY_LABELS.status}>
          <span
            className={`inline-flex items-center gap-1 rounded-full border border-current px-2 text-xs leading-[18px] font-semibold ${statusTone(status)}`}
            {...pageChrome}
          >
            <StatusGlyph glyph={statusGlyph(status)} />
            {issueStatusWord(status)}
          </span>
        </Row>
        <Row glyph={UPRIGHT_BAR} label={PROPERTY_LABELS.priority}>
          {issue.priority === null ? (
            none
          ) : (
            <span className="font-semibold" {...authorText}>
              P{issue.priority}
            </span>
          )}
        </Row>
        <Row glyph="●" label={PROPERTY_LABELS.assignee}>
          {issue.assignee === null ? (
            none
          ) : (
            <span className="inline-flex min-w-0 items-center gap-1.5" {...authorText}>
              <span
                aria-hidden="true"
                className={`inline-grid size-5 shrink-0 place-items-center rounded-full text-xs leading-5 font-semibold before:content-[attr(data-initial)] ${pageAvatar}`}
                data-initial={issue.assignee.charAt(0).toUpperCase()}
              />
              <span className="min-w-0 [overflow-wrap:anywhere]">{issue.assignee}</span>
            </span>
          )}
        </Row>
        {/* Every label, with the control that adds and removes one: the header draws those
            that fit, and its count opens this row. */}
        <Row glyph="#" label={PROPERTY_LABELS.labels} row="labels">
          {labels.length === 0 ? none : null}
          <IssueLabels
            chipClassName={`rounded-md border px-[7px] text-xs leading-[18px] font-medium ${pageLabelChip}`}
            disabled={isClosed}
            issueKey={issue.key}
            labels={labels}
            onSave={(next) => writes.labels.mutateAsync(next)}
            project={issue.project}
            saving={writes.labels.isPending}
            variant="rail"
          />
        </Row>
        {/* The priorities the issue advances, with the control that adds and removes one: an
            issue that advances none draws no such row, and its control stands beside the add
            control below, where a focus in the row moves on as the last one goes
            (`useFocusHandOff`). */}
        {advances.length === 0 ? null : (
          <Row
            glyph="↗"
            label={PROPERTY_LABELS.advances}
            testId="issue-advances"
            valueRef={advancesValue}
          >
            <div className="contents" ref={priorityRowLeft}>
              <IssuePriorityLine disabled={isClosed} inRow issue={issue} />
            </div>
          </Row>
        )}
        {/* The parent's own control first, which opens its editor in this row: the header's Set
            parent issue presses it (`useOpenDetailsAt`, `details-rows.ts`), as the add menu's
            Parent does. */}
        <Row glyph="↑" label={PROPERTY_LABELS.parent} row="parent" valueRef={parentValue}>
          {isClosed || editingField === "parent" ? null : (
            <button
              className={pageHeaderControl}
              onClick={() => onEditingField("parent")}
              ref={parentControl}
              type="button"
              {...pageChrome}
            >
              {issue.parent === null ? CONTROL_LABELS.set_parent : CONTROL_LABELS.edit_parent}
            </button>
          )}
          {issue.parent === null ? (
            none
          ) : (
            <Link
              className={`dispatch-inline-link ${ROW_LINK} ${pageInkText}`}
              to={buildIssuePath({ key: issue.parent, kind: "issue" })}
              {...authorText}
            >
              {issue.parent}
            </Link>
          )}
          {editingField === "parent" ? (
            <IssueFieldForms
              field="parent"
              isClosed={isClosed}
              issue={issue}
              onClose={closeEditor}
              writes={writes}
            />
          ) : null}
          {writes.parentSaveFailed ? (
            <p className={`w-full text-sm ${pageFailedText}`} id={PARENT_ERROR_ID} role="alert">
              {writes.parentError}
            </p>
          ) : null}
        </Row>
        <Row glyph={SMALL_GLYPH} label={PROPERTY_LABELS.blocked_by} valueRef={blockedByValue}>
          <BlockedByControl
            draft={blockedByDraft}
            editing={blockedByEditing}
            focusFallback={developmentHeading}
            issue={issue}
            onDraft={setBlockedByDraft}
            onEditing={setBlockedByEditing}
            titleOf={titleOf}
            write={blockedByWrite}
          />
          {/* The blockers are drawn by their keys where their titles could not be read, and the
              row says so rather than drawing keys alone as though they had none. */}
          {readFailed(blockerTitles) ? (
            <DetailsErrorLine
              error={blockerTitles.error}
              failure={blockerTitles.errorUpdateCount}
              message="Could not read the titles of these issues."
              onRetry={retryRead(blockerTitles)}
              retryRef={titlesRetryLeft}
              retrying={blockerTitles.isFetching}
            />
          ) : null}
        </Row>
        {/* What this issue blocks: drawn whenever the panel is, with None where the answer holds
            nothing that waits on it, nothing while that answer is still out, and the failure
            where the read failed, as every other row of the panel says what it holds. This
            row's None takes the focus of the failure's Retry when it replaces that line. */}
        <Row glyph={CIRCLED_DOT} label={PROPERTY_LABELS.blocks} valueRef={blocksValue}>
          {readFailed(blocking) ? (
            <DetailsErrorLine
              error={blocking.error}
              failure={blocking.errorUpdateCount}
              message="Could not read what this issue blocks."
              onRetry={retryRead(blocking)}
              retryRef={blocksRetryLeft}
              retrying={blocking.isFetching}
            />
          ) : blocking.isPending ? null : blocks.length === 0 ? (
            <span
              className={`outline-none ${pageQuietText}`}
              ref={blocksNone}
              tabIndex={-1}
              {...pageChrome}
            >
              {CONTROL_LABELS.none}
            </span>
          ) : (
            <IssueList issues={blocks} />
          )}
        </Row>
        <Row glyph={HOUSE} label={PROPERTY_LABELS.repository} valueRef={repoValue}>
          <RepoControl
            disabled={isClosed}
            inRow
            issueKey={issue.key}
            project={issue.project}
            repo={issue.repo}
            resolvedRepo={issue.resolved_repo}
            sayFailure={sayRepoFailure}
          />
        </Row>
        {/* Each link reads GitHub as it is drawn (`GitHubLink`), so the links are drawn only
            while the reader can see them: Details open, and Development with it. */}
        <Row glyph={RIGHT_OVER_LEFT_ARROWS} label={PROPERTY_LABELS.pull_request}>
          {pullRequests.length === 0 ? none : null}
          {readingDevelopment
            ? pullRequests.map((link) => <GitHubLink key={link.url} link={link} />)
            : null}
        </Row>
        {otherLinks.length === 0 ? null : (
          <Row glyph="↗" label={PROPERTY_LABELS.external_links}>
            {readingDevelopment
              ? otherLinks.map((link) => <GitHubLink key={link.url} link={link} />)
              : null}
          </Row>
        )}
        <Row glyph={FLAG} label={PROPERTY_LABELS.claim}>
          <IssueClaimControl issue={issue} writes={writes} />
        </Row>
        <Row glyph="→" label={PROPERTY_LABELS.route} row="route" valueRef={routeValue}>
          <IssueRouteControl
            controlRef={routeControl}
            editing={editingField === "route"}
            isClosed={isClosed}
            issue={issue}
            onEdit={() => onEditingField("route")}
            writes={writes}
          />
          {editingField === "route" ? (
            <IssueFieldForms
              field="route"
              isClosed={isClosed}
              issue={issue}
              onClose={closeEditor}
              writes={writes}
            />
          ) : null}
          {/* A route Dispatch refused, said in the row it was saved from with Dispatch's reason
              and Retry, as the Parent row says a refused parent, for as long as the draft still
              holds a route the issue does not: a reader who leaves the editor puts the saved
              route back, and Retry would then send nothing. On a closed issue the line has no
              Retry: Dispatch refuses every route of a closed issue (`ISSUE_CLOSED`, a save out as
              the issue closed), and the drafts send none of it again (`useIssueDrafts`). The save
              is Details' own write (the details slot's `useIssueHeaderWrites`), and inline, so
              the write store leaves its refusal to this row, and the header, which says the
              page's other refusals, never sees it. The line stays while the save its Retry sent
              again is out (`useSaidFailure`), and says Dispatch's new reason if that is refused
              too; its Retry takes no press while that save is out (`refused`), since the drafts
              send nothing while a save is out (`useSubmitGuard`), and is never disabled. Once the
              route lands the line goes, and the row's control takes the focus Retry held
              (`useFocusHandOff`). */}
          {fieldFailure !== null &&
          fieldFailure.variables?.route !== undefined &&
          writes.drafts.route !== (issue.route ?? "") ? (
            <DetailsErrorLine
              error={fieldFailure.error}
              failure={fieldFailure.count}
              message={apiErrorMessage(fieldFailure.error, "Could not update this issue.")}
              onRetry={isClosed ? undefined : writes.drafts.retry}
              refused={writes.updateIssue.isPending}
              retryRef={routeRetryLeft}
            />
          ) : null}
        </Row>
        <Row glyph={GRID_SQUARE} label={PROPERTY_LABELS.components}>
          <IssueComponentsLine inRow issue={issue} />
        </Row>
        {issue.progress?.tasks === null || issue.progress?.tasks === undefined ? null : (
          <Row glyph={TICKED_BOX} label={PROPERTY_LABELS.tasks}>
            <span data-testid="issue-progress-tasks" {...pageChrome}>
              <span {...authorText}>
                {issue.progress.tasks.done}/{issue.progress.tasks.total}
              </span>{" "}
              {issue.progress.tasks.total === 1 ? PROGRESS_NOUNS.task : PROGRESS_NOUNS.tasks}
            </span>
          </Row>
        )}
        {/* The one add control, which lists every field this issue has no value for and opens
            its control; and, while the issue advances no priority, the control that adds one,
            which is the priority row (`IssuePriorityLine`) with nothing in it yet. The focus it
            holds as the first priority moves the line goes on to the Advances row
            (`useFocusHandOff`). */}
        <div className="flex min-w-0 flex-wrap items-center gap-2 py-2" ref={addRow}>
          <IssueAddMenu
            drawerBox={drawerBox}
            isClosed={isClosed}
            issue={issue}
            onOpenField={onEditingField}
            panel={panel}
          />
          {advances.length === 0 ? (
            <div className="contents" data-testid="issue-advances" ref={priorityBesideAddLeft}>
              <IssuePriorityLine disabled={isClosed} inRow issue={issue} />
            </div>
          ) : null}
        </div>
      </PropertySection>
      <PropertySection drawerBox={drawerBox} name="people" sections={sections}>
        <Row glyph={SMILING_FACE} label={PROPERTY_LABELS.opened_by}>
          {issue.created_by == null ? (
            none
          ) : (
            <span {...authorText}>
              <AgentName actor={issue.created_by} author inline />
            </span>
          )}
        </Row>
        <DetailsPeople issue={issue} state={state} />
      </PropertySection>
      <PropertySection drawerBox={drawerBox} name="references" sections={sections}>
        {/* The approval steps with the reasons a reviewer gave, and Approve and Request changes
            wherever no approval request waits on the reader. */}
        <Row glyph={LINED_SQUARE} label={PROPERTY_LABELS.approval}>
          <ApprovalSteps
            actions={!approvalWaits}
            entries={approvalEntries}
            primaryArtifactId={issue.primary_artifact_id}
            reasons
            size="compact"
            testId="issue-details-approval-steps"
            versions={{ asks: issue.open_asks ?? [], issueKey: issue.key }}
          />
        </Row>
        <Row wide>
          {/* Details' own control of the list, with the name, the role and the states of the
              application's `ReferencedByToggle`, in the page's ink rather than its link blue. A
              count of zero is a fact and not a control, as there: opening it would only say
              that nothing references the issue. */}
          {issue.referenced_by_count === 0 ? (
            <span
              className={`inline-flex min-h-11 items-center text-sm md:min-h-0 ${pageQuietText}`}
            >
              Referenced by (0)
            </span>
          ) : (
            <button
              aria-controls={referencesPanelId}
              aria-expanded={referencesOpen}
              className={`inline-flex min-h-11 items-center text-left text-sm font-medium md:min-h-0 ${ROW_LINK} ${pageInkText}`}
              onClick={() => setReferencesOpen((open) => !open)}
              type="button"
            >
              {`Referenced by (${issue.referenced_by_count})`}
            </button>
          )}
          {/* The list reads what references the issue, and reads it again on each event about
              those references, so it is drawn only while the reader can see it: open, in an open
              References of an open Details. A closed section keeps its rows on the page, hidden
              (`PropertySection`), as a closed Details keeps the panel (`slots/details.tsx`), and
              either would keep the list's read live with them. */}
          {referencesOpen && readingReferences ? (
            <ReferencedBy
              className="w-full"
              reference={issueReference}
              toggle={{ id: referencesPanelId }}
            />
          ) : null}
        </Row>
        {/* Every document and file of the issue, each opening its own address, folded after ten
            behind a count that opens the rest. */}
        <Row
          glyph={LINED_SQUARE}
          label={
            <>
              {PROPERTY_LABELS.files} · <span {...authorText}>{artifacts.length}</span>
            </>
          }
          wide
        >
          {artifacts.length === 0 ? (
            none
          ) : (
            <ul className="flex w-full min-w-0 flex-col" data-testid="issue-files" ref={filesList}>
              {files.map((artifact) => (
                <li key={artifact.id}>
                  <Link
                    className={`grid w-full grid-cols-[16px_minmax(0,1fr)_auto] items-baseline gap-2 rounded-md p-1 text-sm ${pageInkText} ${pageRowHoverBg}`}
                    to={buildIssuePath({ key: issue.key, kind: "artifact", slug: artifact.slug })}
                  >
                    <span aria-hidden="true" className={pageQuietText}>
                      {artifact.kind === "image" ? SQUARE_IN_SQUARE : LINED_SQUARE}
                    </span>
                    <span className="min-w-0 [overflow-wrap:anywhere]" {...authorText}>
                      {artifact.name}
                    </span>
                    <span className={`text-xs whitespace-nowrap ${pageQuietText}`} {...pageChrome}>
                      {artifact.kind === "doc"
                        ? FILE_WORDS.doc
                        : artifact.kind === "image"
                          ? FILE_WORDS.image
                          : FILE_WORDS.file}
                      {artifact.versions === null || artifact.versions.length === 0 ? null : (
                        <span {...authorText}> v{artifact.versions.length}</span>
                      )}
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          )}
          {artifacts.length > FOLD_AFTER && !filesOpen ? (
            <ShowAll
              className="pt-1.5"
              count={artifacts.length}
              list={filesList}
              onShow={() => setFilesOpen(true)}
            />
          ) : null}
        </Row>
      </PropertySection>
      <DetailsFocusedNode issue={issue} state={state} />
      <IssuePins
        dismissed={
          state === undefined ? undefined : (stateForIssue(state, issue.key).dismissed ?? [])
        }
        issueKey={issue.key}
      />
      <DetailsHistory issue={issue} state={state} />
    </div>
  );
}

/** The issues of a list of them, each its key as a link and its title beside it, folded after
 *  ten behind a count that opens the rest. */
function IssueList({
  issues,
}: {
  issues: readonly { readonly key: string; readonly title: string }[];
}): ReactNode {
  const [open, setOpen] = useState(false);
  const list = useRef<HTMLDivElement>(null);
  const shown = open ? issues : issues.slice(0, FOLD_AFTER);
  return (
    <div className="flex w-full min-w-0 flex-col gap-1" ref={list}>
      {shown.map((one) => (
        <IssueLine issueKey={one.key} key={one.key} title={one.title} />
      ))}
      {issues.length > FOLD_AFTER && !open ? (
        <ShowAll
          className="self-start"
          count={issues.length}
          list={list}
          onShow={() => setOpen(true)}
        />
      ) : null}
    </div>
  );
}

/** The count a folded list is drawn behind, which opens the rest of it. `className` places it
 *  in the list that draws it, and `list` is that list, each of its children one entry. The
 *  count goes with its press, so the focus goes on to the link of the first entry it opened,
 *  the list's entry `FOLD_AFTER`, rather than falling to the page. */
function ShowAll({
  className,
  count,
  list,
  onShow,
}: {
  className: string;
  count: number;
  list: RefObject<HTMLElement | null>;
  onShow: () => void;
}): ReactNode {
  return (
    <button
      className={`${className} text-xs font-semibold underline underline-offset-[3px] ${pageInkText}`}
      onClick={() => {
        flushSync(onShow);
        list.current?.children[FOLD_AFTER]?.querySelector<HTMLElement>("a[href]")?.focus();
      }}
      type="button"
      {...pageChrome}
    >
      {/* One child, so the space before the count holds where every button is a flex box
          (`styles.css`, below 1280 px), which drops a space between two of its children. */}
      <span>
        {CONTROL_LABELS.show_all} <span {...authorText}>{count}</span>
      </span>
    </button>
  );
}

/** One named section of the panel's properties, open or closed by its own one control. A closed
 *  section keeps its rows on the page and hides them (`hidden`: out of view, out of the tab order
 *  and out of the accessibility tree), so what a reader sees is what the page holds, and what a
 *  row holds of its own - a draft, an open form, a refusal said in the row - is still there when
 *  the section opens again, as the panel's is when Details does (`slots/details.tsx`). */
function PropertySection({
  children,
  drawerBox,
  headingRef,
  name,
  onBlur,
  onFocus,
  sections,
}: {
  children: ReactNode;
  /** The drawer's `aside` the panel is placed in, which a request for a row is sent to. */
  drawerBox: HTMLElement | null;
  /** Takes the section's heading, its own control, so a focus can be handed to it. */
  headingRef?: Ref<HTMLButtonElement>;
  name: MarginSection;
  /** Hears each focus that leaves an element of the section, one its rows draw in a portal
   *  included. */
  onBlur?: FocusEventHandler<HTMLElement>;
  /** Hears each focus that goes to an element of the section, one its rows draw in a portal
   *  included. */
  onFocus?: FocusEventHandler<HTMLElement>;
  sections: MarginSections;
}): ReactNode {
  const open = sections.isOpen(name);
  // A control elsewhere on the page that opens Details at one of this section's rows asks the
  // drawer for that row before it looks for it (`useOpenDetailsAt` sends
  // `DETAILS_ROW_REQUEST_EVENT`). A closed section opens for it as its heading opens it, so the
  // reader's kept choice follows, and in the same task, so the row is no longer hidden when the
  // control looks for it and its control takes the focus.
  const latest = useRef(sections);
  latest.current = sections;
  useEffect(() => {
    if (drawerBox === null) return;
    const openForRow = (event: Event) => {
      if (
        !(event instanceof CustomEvent) ||
        !SECTION_ROWS[name].includes(event.detail) ||
        latest.current.isOpen(name)
      ) {
        return;
      }
      flushSync(() => latest.current.toggle(name));
    };
    drawerBox.addEventListener(DETAILS_ROW_REQUEST_EVENT, openForRow);
    return () => drawerBox.removeEventListener(DETAILS_ROW_REQUEST_EVENT, openForRow);
  }, [drawerBox, name]);
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handlers only read where the focus has gone and what it has left, so the section needs no role of its own.
    <section data-margin-section={name} onBlur={onBlur} onFocus={onFocus}>
      <h2>
        <button
          aria-expanded={open}
          className={`flex min-h-11 w-full items-center justify-between gap-2 text-xs leading-4 font-semibold tracking-[0.08em] uppercase md:min-h-8 ${pageQuietText}`}
          onClick={() => sections.toggle(name)}
          ref={headingRef}
          type="button"
          {...pageChrome}
        >
          <span>{MARGIN_SECTION_NAMES[name]}</span>
          <span aria-hidden="true">{open ? "−" : "+"}</span>
        </button>
      </h2>
      <div className="flex min-w-0 flex-col" hidden={!open}>
        {children}
      </div>
    </section>
  );
}

/** The one text glyph of Details' rows drawn smaller than the row's text, so that it fits its
 *  14 px column (`Row`'s `w-3.5`): the page's typeface (`Dispatch Inter`, `styles.css`) draws ⊘
 *  19 px wide at the row's 14 px, to 2 px from the row's name, and 13 px wide at 9 px. Every
 *  other text glyph fits at the row's size, and each icon is drawn to the column. */
const SMALL_GLYPH = "⊘";

/** The Status row's shape: that of a status in motion, as the prototype draws the row, which the
 *  page's one status icon draws (`StatusGlyph`). It is named through `statusGlyph`, since the
 *  character that names it is no character the page draws as text. */
const STATUS_ROW_SHAPE = statusGlyph("in_progress");

/**
 * One row of Details (the prototype's `.frow`): a column of 112 px with the property's shape
 * and its name, and the value beside it; a `wide` row is one column, for a property whose own
 * control names it. The shape is an icon of `details-icons.tsx` where the page's typeface does
 * not hold the character, and otherwise that character as text, one the typeface holds and that
 * no font draws as an emoji unless asked to. `row` names the row a control elsewhere opens Details
 * at (`details-rows.ts`), and `valueRef` takes the box of the row's value.
 */
function Row({
  children,
  glyph,
  label,
  row,
  testId,
  valueRef,
  wide = false,
}: {
  children: ReactNode;
  glyph?: ReactNode;
  label?: ReactNode;
  row?: DetailsRow;
  testId?: string;
  valueRef?: Ref<HTMLDivElement>;
  wide?: boolean;
}): ReactNode {
  return (
    <div
      className={`grid min-w-0 gap-2.5 border-b py-2 text-sm leading-5 ${
        wide ? "grid-cols-1" : "grid-cols-[112px_minmax(0,1fr)]"
      } ${pageRowBorder}`}
      data-testid={testId}
      {...(row === undefined ? {} : { [DETAILS_ROW_ATTRIBUTE]: row })}
    >
      {label === undefined ? null : (
        <div className={`flex min-w-0 items-start gap-[7px] ${pageQuietText}`}>
          <span
            aria-hidden="true"
            className={`w-3.5 shrink-0 text-center ${glyph === SMALL_GLYPH ? "text-[9px]" : ""}`}
          >
            {glyph}
          </span>
          <span className="min-w-0" {...pageChrome}>
            {label}
          </span>
        </div>
      )}
      <div
        className={`flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1 [overflow-wrap:break-word] ${pageInkText}`}
        ref={valueRef}
      >
        {children}
      </div>
    </div>
  );
}
