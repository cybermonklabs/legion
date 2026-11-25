import { type QueryClient, type QueryKey, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, type RefCallback, useEffect, useId, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api, apiErrorMessage } from "../../api/client";
import {
  projectsQuery,
  type ReferenceTargetsAnswer,
  referenceTargetsQuery,
} from "../../api/queries";
import type { Artifact, IssueDetails, UserState } from "../../api/types";
import {
  pageAvatar,
  pageFailedText,
  pageHeaderControl,
  pageInkText,
  pageLabelChip,
  pageQuietText,
  pageRowHoverBg,
} from "../../theme/classes";
import { ApprovalSteps } from "../issue/ApprovalSteps";
import { GitHubLink } from "../issue/GitHubLink";
import { gatedStepEntries, STEP_LABELS } from "../issue/gated-steps";
import {
  COMPONENT_CHIP,
  componentChipIds,
  IssueComponentsLine,
} from "../issue/IssueComponentsLine";
import { IssueLabels } from "../issue/IssueLabels";
import { stateForIssue } from "../issue/pins";
import { RepoControl, type RepoControlFailure } from "../issue/RepoControl";
import { IssuePriorityLine } from "../priorities/IssuePriorityLine";
import { AgentName } from "../refs/AgentName";
import { listedEdges, ReferencedBy } from "../refs/ReferencedBy";
import { EVENT_FRESH, referencesQuery, standsFailed } from "../refs/reference-target";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { focusOnDocument } from "../shell/roving";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { BlockedByControl } from "./BlockedByControl";
import {
  DetailsErrorLine,
  retryRead,
  useRetryHandOff,
  useSaidFailure,
  useTitlesRetry,
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
import {
  IssueList,
  PropertySection,
  Row,
  SMALL_GLYPH,
  STATUS_ROW_SHAPE,
  useListPages,
} from "./details-leaves";
import { DetailsFocusedNode, DetailsHistory, DetailsPeople } from "./details-more";
import { blockedByControlName, referencedByControlName } from "./details-names";
import { detailsEditor, detailsOpener } from "./details-rows";
import { IssueAddMenu } from "./IssueAddMenu";
import { IssueClaimControl } from "./IssueClaimControl";
import { type IssueField, IssueFieldForms } from "./IssueFieldForms";
import { IssuePins } from "./IssuePins";
import { IssueRouteControl } from "./IssueRouteControl";
import { statusGlyph, statusTone } from "./issue-status";
import { useMarginDrawer, useMarginDrawsSlot } from "./margin-slot";
import { NoTooltips } from "./NoTooltips";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS, FILE_WORDS, issueStatusWord, PROGRESS_NOUNS } from "./page-words";
import { ROW_LINK } from "./row-references";
import { StatusGlyph } from "./StatusGlyph";
import { firstUsable } from "./usable-control";
import { useIssueBlockedBy } from "./useIssueBlockedBy";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";
import { useWhoIsReading } from "./useIssueTree";
import { useMarginSections } from "./useMarginSections";
import { usePriorityHandOffs } from "./usePriorityHandOffs";
import { waitingOnReader } from "./waiting";

/** The id of the Parent row's line that says why Dispatch refused a parent. */
export const PARENT_ERROR_ID = "issue-parent-error";

/** What the line of a write the reader stopped says on a closed issue, whose row of Details draws
 *  no Retry and no control that would save `field` again, since Dispatch refuses that field of a
 *  closed issue (`ISSUE_CLOSED`); `undefined` where the issue is open or the write was not
 *  stopped, so the line says its own words instead. `what` is what Dispatch has not confirmed. */
function closedStoppedNote(
  write: { readonly isClosed: boolean; readonly stopped: boolean },
  what: string,
  field: string
): string | undefined {
  return write.isClosed && write.stopped
    ? stoppedWriteNote(what, "saved", {
        anew: `The issue is closed, so the ${field} cannot be saved again.`,
      })
    : undefined;
}

/** What a row of Details says where a names read of the issues it lists failed, as it draws those
 *  issues by their keys alone: Blocked by's and Blocks' alike. */
export const TITLES_READ_FAILED = "Could not read the titles of these issues.";

/** The title a names read (`referenceTargetsQuery`) answered for the issue `key`. While that read
 *  has not answered, an issue the read's last answer named keeps the title that answer gave
 *  (`keptUntilFailed`: the last answer that held data, of an earlier set), and any other is
 *  undefined; undefined too where the read failed holding no answer, and where it names nothing
 *  at the issue's address. */
function issueTitleIn(read: ReferenceTargetsAnswer | undefined, key: string): string | undefined {
  const reference = buildDispatchReference({ key, kind: "issue" });
  return read?.targets?.find((target) => target.ref === reference)?.name;
}

/** What a names read under `queryKey` draws while it holds no answer (`placeholderData`): the
 *  answer of the set of issues before (`before`), as a change to the set gives the read a new key,
 *  so a title drawn stays drawn while the new read is out - until that read first fails. A read
 *  that failed holding no answer stands failed (`standsFailed`) and draws the keys with its
 *  failure line, which stays while the read its Retry sent is out; the old answer drawn then would
 *  be an answer held, which would take the line, and its Retry with a reader's focus, away. */
function keptUntilFailed(queryClient: QueryClient, queryKey: QueryKey) {
  return <T,>(before: T | undefined): T | undefined =>
    (queryClient.getQueryState(queryKey)?.errorUpdateCount ?? 0) > 0 ? undefined : before;
}

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
 * read-only. A field with no value is reachable from the one add control, and a list of
 * unbounded length draws its first 20 entries and, under them, Show more, which says how many
 * are left and draws the next 20 (`useListPages`). The rows sit in three named sections the
 * reader can collapse, kept for the next issue.
 *
 * Under the properties, the reader's pins as links to their rows. It holds no list of the
 * conversation: the outline is the conversation. The people, the facts of the focused row and
 * the history each have one of the three places `details-more.tsx` exports.
 *
 * A focus Details loses goes on only where the reader's own press took its element away: a save
 * of theirs that lands, a Retry whose request answers, an editor they close, an Unpin, a Show
 * more. A Retry's line, the claim row's Release and the priority's line moved by the reader's own
 * write hand the focus on in their row as they go (`useFocusHandOff`); an editor hands it on as
 * it closes, a Show more moves it at its press, and an Unpin once its row has gone. A change the
 * reader did not make - another session's write, an event, a read that answers on its own, the
 * issue closed elsewhere - leaves the focus on the page's body, as on every page of Dispatch, and
 * nothing in Details takes it; but one that reaches what the reader's press asked for while that
 * press is still out - the route set to the reader's draft, the claim released, the read
 * answered - is taken for the press's answer, and hands the focus on as that answer would.
 *
 * The details slot draws it once and appends it to the shell's drawer, so nothing in it is
 * thrown away as the reader closes and opens Details. The titles of the issues this one waits
 * on, what waits on it and the titles of those, and its links on GitHub it reads only while
 * Details and its Development are open, and what references the issue only while Details and its
 * References are open with its Referenced by list, so a load that leaves Details shut costs no
 * request for any of them.
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
  // The pages of each list of unbounded length the panel's rows draw (`useListPages`), each with
  // the box its entries are drawn in, where a press of its Show more finds the first entry it
  // drew. The issues that wait on this one and the pins page their own lists.
  const pullRequestPages = useListPages<HTMLDivElement>();
  const linkPages = useListPages<HTMLDivElement>();
  const componentPages = useListPages<HTMLDivElement>();
  const referencePages = useListPages();
  const filePages = useListPages<HTMLUListElement>();
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
  // The line goes once the save lands, or once the draft holds the issue's own route. Where the
  // reader pressed its Retry and the line went, the focus Retry held goes on in the row: to the
  // row's control, and where that takes no press - not drawn while the route's editor is open,
  // disabled on a closed issue - to the editor's box, the one box of the row; and where that takes
  // none either, to Development's heading. The line going while the route Retry sent is still out,
  // as another session sets the issue's route to the draft, reached what the press asked for, so
  // the row takes it for the press's answer. Retry going while its line stays answers no press: a
  // close made in another session takes it, as a closed issue's line offers none, and the focus
  // stays on the page's body.
  const fieldFailure = useSaidFailure(writes.updateIssue);
  const routeStopped = fieldFailure?.error instanceof SendStoppedError;
  const routeFailureShown =
    fieldFailure !== null &&
    fieldFailure.variables?.input.route !== undefined &&
    writes.drafts.route !== (issue.route ?? "");
  const routeRetry = useRetryHandOff(() =>
    routeFailureShown
      ? null
      : firstUsable(
          routeControl.current,
          routeValue.current?.querySelector<HTMLElement>("input"),
          developmentHeading.current
        )
  );
  const issueReference = buildDispatchReference({ key: issue.key, kind: "issue" });
  // The reads Details makes beyond the issue - the titles of the issues this one waits on, what
  // waits on it and the titles of those, its links on GitHub, and what references it - are made
  // only while the drawer is open, so a load that leaves it shut costs no request for them.
  const reading = useMarginDrawer()?.open === true;
  // A closed section keeps its rows, hidden (`PropertySection`), so each of those reads is made
  // only while its own section is open too - all but the last are of rows in Development, where a
  // link reads GitHub as it is drawn, and the last is the References row's list: a reader who
  // keeps a section closed costs none of its reads, nor any of them again on an event. Each
  // section's other rows make their own reads, as they do while Details itself is closed.
  const readingDevelopment = reading && sections.isOpen("development");
  const readingReferences = reading && sections.isOpen("references");
  const blockers = issue.blocked_by ?? [];
  // The titles of the issues this one waits on, and below them those of the issues that wait on
  // it, are each a names read of the issues' addresses. The event of an issue whose title changes
  // (`issue.updated` naming `title` among its changes, or naming none, as an older server sends
  // it) reads again every names read whose key holds that issue's address
  // (`api/event-refresh.ts`'s `namesReadsNaming`), so neither is read again by its age, and a
  // window focus reads one again only while its last read failed (`EVENT_FRESH`). Each draws the
  // answer of the set before while the read of a changed set is out (`keptUntilFailed`).
  const queryClient = useQueryClient();
  const blockerRead = referenceTargetsQuery(
    blockers.map((key) => buildDispatchReference({ key, kind: "issue" }))
  );
  const blockerTitles = useQuery({
    ...blockerRead,
    ...EVENT_FRESH,
    enabled: readingDevelopment && blockers.length > 0,
    placeholderData: keptUntilFailed(queryClient, blockerRead.queryKey),
  });
  const titleOf = (key: string) => issueTitleIn(blockerTitles.data, key);
  // What waits on this issue is the other direction of `blocked_by`: the references of that kind
  // that point at it. It is kept under the backlink panels' `["references", "to", <reference>]`,
  // so the event that starts or stops an issue waiting on this one reads it again
  // (`api/event-refresh.ts`'s rule for a moved `child_of` or `blocked_by`, which reads again each
  // panel under that prefix whose read passes its test: those of the new parent and of each new
  // blocker the event names, this one where an issue starts waiting on it, and those that list an
  // edge from the issue, this one where it stops). Its age never reads it again, and a window
  // focus does only while its last read failed (`EVENT_FRESH`). It says which issues wait and not
  // what they are called: the rename of one of them moves no `blocked_by`, and the panel the
  // server names for it (`references_changed`) is read again by its exact key, which is not this
  // one, so the title an edge carries here would stay the old one.
  const blocking = useQuery({
    enabled: readingDevelopment,
    queryFn: () => api.getReferences(issueReference, { kind: "blocked_by" }),
    queryKey: ["references", "to", issueReference, "blocked_by"],
    ...EVENT_FRESH,
  });
  const waiting = (blocking.data?.edges ?? []).flatMap((edge) =>
    edge.kind !== "blocked_by" || edge.node.issue_key === undefined ? [] : [edge.node.issue_key]
  );
  // The list Referenced by opens, as the cache holds it: this never reads it itself, so the
  // control has a number only once the list has been read, and then it is the rows the list
  // shows, since the issue's count also holds its own asks, comments and messages, which the list
  // leaves out (`listedEdges`).
  const referencesRead = useQuery({ ...referencesQuery(issueReference), enabled: false });
  const referencesListed = listedEdges(referencesRead.data?.edges, issueReference, true)?.length;
  const waitingRead = referenceTargetsQuery(
    waiting.map((key) => buildDispatchReference({ key, kind: "issue" }))
  );
  const waitingTitles = useQuery({
    ...waitingRead,
    ...EVENT_FRESH,
    enabled: readingDevelopment && waiting.length > 0,
    placeholderData: keptUntilFailed(queryClient, waitingRead.queryKey),
  });
  // A waiting issue whose title is not read is drawn by its key alone, as Blocked by draws one.
  const blocks = waiting.map((key) => ({ key, title: issueTitleIn(waitingTitles.data, key) }));
  // The line of a read that stands failed (`standsFailed`) goes once the read answers; a refresh
  // that fails over an answer the read holds draws no line, and the row keeps what that answer
  // drew. Each titles' Retry notes its own press and the read it was pressed for
  // (`useTitlesRetry`), and hands the focus it held on in the row only once the read the row
  // draws now is that read and has answered after it failed: the read the press sent, or a read
  // of the same issues an event sent while it was out, which reached what the press asked for, so
  // the row takes it for the press's answer. The answer of another set is not: one changed in
  // another session to a set not read yet, whose read is out while the row draws the titles of
  // the set before, or emptied, whose read is not sent, or back to one already read, whose answer
  // is cached, even where that read failed before it answered. A line that goes with no press of
  // its Retry - a read answering on an event or a window focus - or before that answer - the set
  // changed or emptied in another session while the read is out - leaves the focus on the page's
  // body. In Blocked by the focus goes to the row's control, found by its name as the add menu
  // finds it (`blockedByControlName`), and where the row draws none - on a closed issue, or while
  // its editor is open - to the first issue the row waits on, each passing over a control that
  // takes no press (`usableControl`). The row draws that issue whenever the read has answered,
  // since the titles are read only for a set that holds one. In Blocks, which has no control, the
  // focus of the Retry of what waits on the issue goes to the first issue the row draws, or else
  // to the None it says, which the row draws only once that read has answered; and that of the
  // Retry of their titles goes to the first issue the row draws.
  const blockedByValue = useRef<HTMLDivElement>(null);
  const titlesRetry = useTitlesRetry(blockerTitles, blockerRead.queryKey, () =>
    firstUsable(
      blockedByValue.current?.querySelector<HTMLElement>(
        `[aria-label="${CSS.escape(blockedByControlName(issue.key, blockers.length))}"]`
      ),
      blockedByValue.current?.querySelector<HTMLElement>("a[href]")
    )
  );
  const blocksValue = useRef<HTMLDivElement>(null);
  const blocksNone = useRef<HTMLSpanElement>(null);
  const firstWaiting = () => blocksValue.current?.querySelector<HTMLElement>("a[href]");
  const blockingRetry = useRetryHandOff(() => firstWaiting() ?? blocksNone.current);
  const waitingRetry = useTitlesRetry(waitingTitles, waitingRead.queryKey, firstWaiting);
  const priority = usePriorityHandOffs(issue, developmentHeading);
  // The Repository row's control draws its own refused write, and hands it to the row to say
  // (`sayFailure`), as Details says its own. A failed read of the project list, which the box's
  // suggestion of the project's default repository comes from, costs the suggestion and says
  // nothing (`RepoControl`). A press of the line's Retry clears the refusal as it sends the pick
  // again, taking the line with it, so that Retry never waits on the write it sent and reads no
  // busy. The line notes the press of its Retry. As the line goes with a reader's focus on its
  // Retry at their press of it, the focus goes on to the row's own control: the box, drawn while
  // a write is out, else the text or None the row draws in its place; and where the row holds
  // nothing that takes it, on a closed issue, to Development's heading. A line that goes with no
  // press of its Retry - its Retry taken away by a close made in another session - hands nothing
  // on. On a closed issue the line has no Retry: Dispatch refuses every repository of a closed
  // issue (`ISSUE_CLOSED`), so the pick sent again would be refused again, and a pick the reader
  // stopped says the issue is closed in place of the Retry its line would offer; the control
  // says which pick the reader stopped (`RepoControlFailure`'s `stopped`).
  const repoValue = useRef<HTMLDivElement>(null);
  const repoControlLeft = () =>
    firstUsable(
      repoValue.current?.querySelector<HTMLElement>("input"),
      ...(repoValue.current?.querySelectorAll<HTMLElement>("button") ?? []),
      developmentHeading.current
    );
  const repoWriteRetry = useRetryHandOff(repoControlLeft);
  const repoWhat = `the repository of ${issue.key}`;
  const sayRepoFailure = (failure: RepoControlFailure): ReactNode => (
    <DetailsErrorLine
      message={
        closedStoppedNote({ isClosed, stopped: failure.stopped }, repoWhat, "repository") ??
        failure.message
      }
      onRetry={isClosed ? undefined : failure.retry}
      {...repoWriteRetry}
    />
  );
  // Referenced by's line stands while its read stands failed (`standsFailed`), as each line of
  // Details' own does - `ReferencedBy` draws it so where it is handed `retried` - so its Retry
  // goes with it once the read the press sent answers. Where the reader's press of Retry took it
  // away, the focus it held goes on to the list's first link; where the list draws none, with
  // nothing to list, to Referenced by's own control, which opens and closes the list; and where
  // that is not drawn - at a count of 0, while a list opened before stays open - to References'
  // heading, the button that opens and closes the section. Each passes over a control that takes
  // no press (`usableControl`). A line that goes with no press of its Retry - the list read again
  // on an event, or after a press whose read failed - leaves the focus on the page's body.
  const referencesValue = useRef<HTMLDivElement>(null);
  const referencesControl = useRef<HTMLButtonElement>(null);
  const referencesHeading = useRef<HTMLButtonElement>(null);
  const referencesRetry = useRetryHandOff(() =>
    firstUsable(
      referencesValue.current?.querySelector<HTMLElement>("a[href]"),
      referencesControl.current,
      referencesHeading.current
    )
  );
  // Approve and Request changes are here wherever no approval request waits on the reader,
  // and in the header while one does: the approvals are all this reads of the count, so it
  // hands it no decisions.
  const viewer = useWhoIsReading().data?.login;
  const approvalWaits =
    waitingOnReader(issue, viewer, undefined, () => false, undefined, undefined).approvals.length >
    0;
  // The status as the header draws it: the one being saved while any caller's write of it is
  // in flight, so the page never says two statuses at once.
  const status = writes.pendingStatus ?? issue.status;
  // The project's gated steps, for a top-level issue. The events that change a project refresh
  // its read (`api/event-refresh.ts`), so a window focus past the app's staleness reads it again
  // only while its last read failed.
  const projects = useQuery({
    ...projectsQuery(),
    ...EVENT_FRESH,
    enabled: issue.parent === null,
  });
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
  // The chips the Components row draws, as one list (`componentChipIds`).
  const componentChips = componentChipIds(issue.components).length;
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
            onSave={(next) => writes.labels.mutateAsync({ issueKey: issue.key, labels: next })}
            project={issue.project}
            saving={writes.labels.isPending}
            variant="rail"
          />
        </Row>
        {/* The priorities the issue advances, with the control that adds and removes one: an
            issue that advances none draws no such row, and its control stands beside the add
            control below, where a focus in the row moves on as the reader's own removal of the
            last one lands (`usePriorityHandOffs`). */}
        {advances.length === 0 ? null : (
          <Row
            glyph="↗"
            label={PROPERTY_LABELS.advances}
            testId="issue-advances"
            valueRef={priority.advancesValue}
          >
            <div className="contents" ref={priority.rowLeft}>
              <IssuePriorityLine disabled={isClosed} inRow issue={issue} />
            </div>
          </Row>
        )}
        {/* The parent's own control first, which opens its editor in this row and is marked as
            the row's opener: the header's Set parent issue presses it (`useOpenDetailsAt`,
            `details-rows.ts`), and, while the editor stands in its place, puts the focus in the
            editor, also marked, rather than press the parent's link. The add menu's Parent opens
            the editor by its state (`onEditingField`), and presses nothing. */}
        <Row glyph="↑" label={PROPERTY_LABELS.parent} row="parent" valueRef={parentValue}>
          {isClosed || editingField === "parent" ? null : (
            <button
              className={pageHeaderControl}
              onClick={() => onEditingField("parent")}
              ref={parentControl}
              type="button"
              {...pageChrome}
              {...detailsOpener}
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
            <div className="contents" {...detailsEditor}>
              <IssueFieldForms
                field="parent"
                isClosed={isClosed}
                issue={issue}
                onClose={closeEditor}
                writes={writes}
              />
            </div>
          ) : null}
          {/* A parent save the reader stopped offers to be saved again, for which a closed
              issue's row draws no control, so there its line says the issue is closed instead. */}
          {writes.parentSaveFailed ? (
            <p className={`w-full text-sm ${pageFailedText}`} id={PARENT_ERROR_ID} role="alert">
              {closedStoppedNote(
                { isClosed, stopped: writes.updateIssue.error instanceof SendStoppedError },
                "the new parent",
                "parent"
              ) ?? writes.parentError}
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
          {standsFailed(blockerTitles) ? (
            <DetailsErrorLine
              error={blockerTitles.error}
              failure={blockerTitles.errorUpdateCount}
              message={TITLES_READ_FAILED}
              {...titlesRetry}
              retrying={blockerTitles.isFetching}
            />
          ) : null}
        </Row>
        {/* What this issue blocks: drawn whenever the panel is, with None where the answer holds
            nothing that waits on it, nothing while that answer is still out, and the failure
            where the read stands failed (`standsFailed`), as every other row of the panel says
            what it holds. The issues are drawn by their keys where their titles could not be
            read, and the row says so, as Blocked by does. This row's None takes the focus of the
            failure's Retry when it replaces that line once the read the reader's press of Retry
            sent answers. */}
        <Row glyph={CIRCLED_DOT} label={PROPERTY_LABELS.blocks} valueRef={blocksValue}>
          {standsFailed(blocking) ? (
            <DetailsErrorLine
              error={blocking.error}
              failure={blocking.errorUpdateCount}
              message="Could not read what this issue blocks."
              onRetry={retryRead(blocking)}
              {...blockingRetry}
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
            <>
              <IssueList issues={blocks} />
              {standsFailed(waitingTitles) ? (
                <DetailsErrorLine
                  error={waitingTitles.error}
                  failure={waitingTitles.errorUpdateCount}
                  message={TITLES_READ_FAILED}
                  {...waitingRetry}
                  retrying={waitingTitles.isFetching}
                />
              ) : null}
            </>
          )}
        </Row>
        <Row glyph={HOUSE} label={PROPERTY_LABELS.repository} valueRef={repoValue}>
          <RepoControl
            disabled={isClosed}
            inRow
            issueKey={issue.key}
            project={issue.project}
            projectsFreshness={EVENT_FRESH}
            repo={issue.repo}
            resolvedRepo={issue.resolved_repo}
            sayFailure={sayRepoFailure}
          />
        </Row>
        {/* Each link reads GitHub as it is drawn (`GitHubLink`), so the links are drawn only
            while the reader can see them: Details open, and Development with it; and 20 at a
            time, each one link, a child of the row's value. */}
        <Row
          glyph={RIGHT_OVER_LEFT_ARROWS}
          label={PROPERTY_LABELS.pull_request}
          valueRef={pullRequestPages.list}
        >
          {pullRequests.length === 0 ? none : null}
          {readingDevelopment ? (
            <>
              {pullRequests.slice(0, pullRequestPages.drawn).map((link) => (
                <GitHubLink key={link.url} link={link} />
              ))}
              {pullRequestPages.more(pullRequests.length)}
            </>
          ) : null}
        </Row>
        {otherLinks.length === 0 ? null : (
          <Row glyph="↗" label={PROPERTY_LABELS.external_links} valueRef={linkPages.list}>
            {readingDevelopment ? (
              <>
                {otherLinks.slice(0, linkPages.drawn).map((link) => (
                  <GitHubLink key={link.url} link={link} />
                ))}
                {linkPages.more(otherLinks.length)}
              </>
            ) : null}
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
            <div className="contents" {...detailsEditor}>
              <IssueFieldForms
                field="route"
                isClosed={isClosed}
                issue={issue}
                onClose={closeEditor}
                writes={writes}
              />
            </div>
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
              send nothing while a save is out (`useSubmitGuard`), and is never disabled. A
              save the reader stopped is no refusal: it may have been saved, and the line says so
              in the header's words for a title or a route save, whose Retry is this one, a press
              of its own; on a closed issue, whose line draws no Retry, it says so in Details' own
              sentence, that the route cannot be saved again. Once the route the reader's Retry sent
              lands, or the issue is drawn holding it while that save is out, the line goes, and
              the row's control takes the focus Retry held (`useRetryHandOff`). */}
          {routeFailureShown ? (
            <DetailsErrorLine
              error={fieldFailure.error}
              failure={fieldFailure.count}
              message={
                closedStoppedNote(
                  { isClosed, stopped: routeStopped },
                  "this change to the issue",
                  "route"
                ) ??
                (routeStopped
                  ? stoppedWriteNote("this change to the issue", "saved", {
                      anew: "Retry sends what the field holds now as a new write.",
                    })
                  : apiErrorMessage(fieldFailure.error, "Could not update this issue."))
              }
              onRetry={isClosed ? undefined : writes.drafts.retry}
              refused={writes.updateIssue.isPending}
              {...routeRetry}
            />
          ) : null}
        </Row>
        {/* The issue's components, then its retired ones, as one list, 20 at a time: each chip
            carries the line's mark (`COMPONENT_CHIP`), in the list's order. */}
        <Row glyph={GRID_SQUARE} label={PROPERTY_LABELS.components} valueRef={componentPages.list}>
          <IssueComponentsLine inRow issue={issue} limit={componentPages.drawn} />
          {componentPages.more(
            componentChips,
            (index) => componentPages.list.current?.querySelectorAll(COMPONENT_CHIP)[index]
          )}
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
            holds as the reader's own first priority lands and moves the line goes on to the
            Advances row (`usePriorityHandOffs`). */}
        <div className="flex min-w-0 flex-wrap items-center gap-2 py-2" ref={priority.addRow}>
          <IssueAddMenu
            drawerBox={drawerBox}
            isClosed={isClosed}
            issue={issue}
            onOpenField={onEditingField}
            panel={panel}
          />
          {advances.length === 0 ? (
            <div className="contents" data-testid="issue-advances" ref={priority.besideAddLeft}>
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
      <PropertySection
        drawerBox={drawerBox}
        headingRef={referencesHeading}
        name="references"
        sections={sections}
      >
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
        <Row valueRef={referencesValue} wide>
          {/* Details' own control of the list, with the name, the role and the states of the
              application's `ReferencedByToggle`, in the page's ink rather than its link blue. A
              count of zero is a fact and not a control, as there: opening it would only say
              that nothing references the issue. Otherwise it reads the rows the list shows once
              the list has been read. */}
          {issue.referenced_by_count === 0 ? (
            <span
              className={`inline-flex min-h-11 items-center text-sm md:min-h-0 ${pageQuietText}`}
            >
              {referencedByControlName(0)}
            </span>
          ) : (
            <button
              aria-controls={referencesPanelId}
              aria-expanded={referencesOpen}
              className={`inline-flex min-h-11 items-center text-left text-sm font-medium md:min-h-0 ${ROW_LINK} ${pageInkText}`}
              onClick={() => setReferencesOpen((open) => !open)}
              ref={referencesControl}
              type="button"
            >
              {referencedByControlName(referencesListed)}
            </button>
          )}
          {/* The list reads what references the issue, and reads it again on each event about
              those references, so it is drawn only while the reader can see it: open, in an open
              References of an open Details. Its rows are drawn 20 at a time, as one list
              whatever source they cite from (`pages`). */}
          {referencesOpen && readingReferences ? (
            <ReferencedBy
              className="w-full"
              page
              pages={referencePages}
              reference={issueReference}
              {...referencesRetry}
              toggle={{ id: referencesPanelId }}
            />
          ) : null}
        </Row>
        {/* Every document and file of the issue, each opening its own address, 20 at a time;
            the count in the row's label is of them all. */}
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
            <ul
              className="flex w-full min-w-0 flex-col"
              data-testid="issue-files"
              ref={filePages.list}
            >
              {artifacts.slice(0, filePages.drawn).map((artifact) => (
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
          {filePages.more(artifacts.length)}
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
