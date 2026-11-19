import { useQuery } from "@tanstack/react-query";
import {
  type ReactNode,
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
import { RepoControl } from "../issue/RepoControl";
import { IssuePriorityLine } from "../priorities/IssuePriorityLine";
import { AgentName } from "../refs/AgentName";
import { listedEdges, ReferencedBy } from "../refs/ReferencedBy";
import { referencesQuery } from "../refs/reference-target";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { focusOnDocument } from "../shell/roving";
import { BlockedByControl, IssueLine } from "./BlockedByControl";
import { DetailsErrorLine } from "./details-error";
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
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  FILE_WORDS,
  issueStatusWord,
  PROGRESS_NOUNS,
} from "./page-words";
import { ROW_LINK } from "./row-references";
import { useRunRows } from "./run-references/run-references";
import { useIssueBlockedBy } from "./useIssueBlockedBy";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";
import { type MarginSections, useMarginSections } from "./useMarginSections";
import { waitingOnReader } from "./waiting";

/** How many entries a list of unbounded length draws before it folds behind a count that opens
 *  the rest (dispatch://INFRA-1196 R13). */
const FOLD_AFTER = 10;

/** The rows of each section that a control elsewhere on the page opens Details at
 *  (`DetailsRow`), each where it is drawn: the section opens for a request of one of them.
 *  People holds the issue's collaborators, which part M draws there (`DetailsPeople`). */
const SECTION_ROWS: Readonly<Record<MarginSection, readonly DetailsRow[]>> = {
  development: ["labels", "parent", "route"],
  people: ["collaborators"],
  references: [],
};

/**
 * The margin's own side of the issue page's panel (dispatch://INFRA-1196 R12): the margin's
 * cards, and - where the margin keeps a column for that panel - the host the panel is moved
 * into, under them.
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
            authors' own, and not the page's declared vocabulary (dispatch://INFRA-1196 R21,
            `page-structure.ts`). They are first because the margin places an anchored card at
            a `top` it measures down from the top of its own content (`margin/ThreadList.tsx`'s
            `regionOrigin`), so anything drawn over them moves that anchor and a card can no
            longer reach the line its mark is on. */}
        <div className="contents" {...authorText}>
          {conversation}
        </div>
      </div>
    </NoTooltips>
  );
}

/**
 * Details, the issue page's one panel of properties (dispatch://INFRA-1196 R13, R14, the
 * prototype's `#details` and the picture page-v3-details-1440): every property the header does
 * not hold, each a row of its own - a label column with a shape for the property and its name,
 * and the value beside it, `None` where the issue has none - with every control the page had
 * for it, and nothing read-only. A field with no value is reachable from the one add control,
 * and only a list of unbounded length folds, after its first ten entries, behind a count that
 * opens the rest. The rows sit in three named sections the reader can collapse, kept for the
 * next issue.
 *
 * Under the properties, the reader's pins as links to their rows (R14). It holds no list of the
 * conversation: the outline is the conversation. The people, the facts of the focused row and
 * the history are part M's, in the three places `details-more.tsx` exports.
 *
 * The details slot draws it once and appends it to the shell's drawer, so nothing in it is
 * thrown away as the reader closes and opens Details. What it reads that the header does not -
 * the titles of the issues around this one, and its links on GitHub - it reads only while
 * Details is open, so a load that leaves Details shut costs no request for it.
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
   *  elsewhere on the page sends its request for a row to; `null` while the panel is in none. */
  drawerBox?: HTMLElement | null;
  /** The editor open in its row - the parent's or the route's - or `null`. */
  editingField: IssueField | null;
  isClosed: boolean;
  issue: IssueDetails;
  onEditingField: (field: IssueField | null) => void;
  /** The reader's own state of every issue, whose pins the panel lists (R14). */
  state: UserState | undefined;
  /** The details slot's set of the issue's writes (`useIssueHeaderWrites.ts`). */
  writes: IssueHeaderWrites;
}): ReactNode {
  const sections = useMarginSections();
  const [referencesOpen, setReferencesOpen] = useState(false);
  const [filesOpen, setFilesOpen] = useState(false);
  const filesList = useRef<HTMLUListElement>(null);
  const referencesPanelId = useId();
  // The blocked-by editor's state is the panel's and not its row's, because a closed section
  // unmounts its rows and the panel stays drawn for the life of the page (`slots/details.tsx`):
  // a list typed before the reader closed Development, and a refusal that lands after, are
  // still there when it opens again (`BlockedByControl`).
  const [blockedByEditing, setBlockedByEditing] = useState(false);
  const [blockedByDraft, setBlockedByDraft] = useState("");
  const blockedByWrite = useIssueBlockedBy(issue.key, () => setBlockedByEditing(false));
  // The row's own control takes the focus back as the reader leaves its editor - by a save, by
  // Cancel or by Escape - since the box that held it goes with the editor; only where the focus
  // fell to the document, the one focus the page takes back for the reader (`shell/roving.ts`).
  const parentControl = useRef<HTMLButtonElement>(null);
  const routeControl = useRef<HTMLButtonElement>(null);
  const leftEditor = useRef<IssueField | null>(null);
  const closeEditor = () => {
    leftEditor.current = editingField;
    onEditingField(null);
  };
  useEffect(() => {
    const left = leftEditor.current;
    leftEditor.current = null;
    if (left === null || editingField !== null || !focusOnDocument()) return;
    (left === "parent" ? parentControl : routeControl).current?.focus();
  }, [editingField]);
  const issueReference = buildDispatchReference({ key: issue.key, kind: "issue" });
  // What Details reads beyond the issue - the titles of the issues around this one, and its
  // links on GitHub - only while the drawer is open, so a load that leaves it shut costs no
  // request for it.
  const reading = useMarginDrawer()?.open === true;
  const blockers = issue.blocked_by ?? [];
  const blockerTitles = useQuery({
    ...referenceTargetsQuery(blockers.map((key) => buildDispatchReference({ key, kind: "issue" }))),
    enabled: reading && blockers.length > 0,
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
    enabled: reading,
    queryFn: () => api.getReferences(issueReference, { kind: "blocked_by" }),
    queryKey: ["references", "to", issueReference, "blocked_by"],
  });
  const blocks = (blocking.data?.edges ?? []).flatMap((edge) =>
    edge.kind !== "blocked_by" || edge.node.issue_key === undefined
      ? []
      : [{ key: edge.node.issue_key, title: edge.excerpt?.text ?? "" }]
  );
  // The list Referenced by opens, as the cache holds it: this never reads it itself, so the
  // control has a number only once the list has been read, and then it is the rows the list
  // shows, since the issue's count also holds its own asks, comments and messages, which the list
  // leaves out (`listedEdges`, dispatch://INFRA-3948 R10).
  const referencesRead = useQuery({ ...referencesQuery(issueReference), enabled: false });
  const referencesListed = listedEdges(referencesRead.data?.edges, issueReference, true)?.length;
  // The command bar runs the reader asked that changed the issue, from the outline's tree read:
  // this observes that read and sends no request of its own (`useRunRows`).
  const runs = useRunRows(issue);
  // A Retry of the tree read takes the focus off itself, and the focus would be left on the
  // page's body, where no keyboard action may leave it. Where the read holds no answer, the
  // failure's line goes as the read starts again; where it holds one, the line stays under the
  // rows with its Retry disabled until the read ends, and a browser moves the focus off a control
  // that becomes disabled. So once no Retry of the line waits on the read the reader's press sent,
  // a focus left on the body goes back to the line's Retry where that read failed again, and to
  // the list's own control where the line has gone, as a focused row's list hands it to its
  // control (`NodeReferences.tsx`).
  const referencesToggle = useRef<HTMLButtonElement>(null);
  const runsFailure = useRef<HTMLDivElement>(null);
  const runsRetryHeld = useRef(false);
  const retryRuns = () => {
    runsRetryHeld.current = true;
    runs.retry();
  };
  useLayoutEffect(() => {
    if (!runsRetryHeld.current || runs.retrying) return;
    runsRetryHeld.current = false;
    if (document.activeElement === null || document.activeElement === document.body) {
      (runs.failed
        ? runsFailure.current?.querySelector("button")
        : referencesToggle.current
      )?.focus({ preventScroll: true });
    }
  });
  // Approve and Request changes are here wherever no approval request waits on the reader,
  // and in the header while one does (R31): the approvals are all this reads of the count, so it
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
    <div className="flex min-w-0 flex-col gap-3">
      <PropertySection drawerBox={drawerBox} name="development" sections={sections}>
        <Row glyph="◐" label={PROPERTY_LABELS.status}>
          <span
            className={`inline-flex items-center gap-1 rounded-full border border-current px-2 text-xs leading-[18px] font-semibold ${statusTone(status)}`}
            {...pageChrome}
          >
            <span aria-hidden="true">{statusGlyph(status)}</span>
            {issueStatusWord(status)}
          </span>
        </Row>
        <Row glyph="▮" label={PROPERTY_LABELS.priority}>
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
        {/* Every label, with the control that adds and removes one (the ruling of
            dispatch://INFRA-1196/comment/63b73642-8fc5-4cc4-8f5d-a3d8989d3bf4): the header draws
            those that fit, and its count opens this row. */}
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
        {/* The priorities the issue advances, with the control that adds and removes one
            (dispatch://INFRA-2947 R3, R16): an issue that advances none draws no such row, and
            its control stands beside the add control below. */}
        {advances.length === 0 ? null : (
          <Row glyph="↗" label={PROPERTY_LABELS.advances} testId="issue-advances">
            <IssuePriorityLine disabled={isClosed} inRow issue={issue} />
          </Row>
        )}
        {/* The parent's own control first, which opens its editor in this row: the header's Set
            parent issue presses it (`useOpenDetailsAt`, `details-rows.ts`), as the add menu's
            Parent does. */}
        <Row glyph="↑" label={PROPERTY_LABELS.parent} row="parent">
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
          <IssueFieldForms
            field={editingField === "parent" ? "parent" : null}
            isClosed={isClosed}
            issue={issue}
            onClose={closeEditor}
            writes={writes}
          />
          {writes.parentSaveFailed ? (
            <p className={`w-full text-sm ${pageFailedText}`} id="issue-parent-error" role="alert">
              {writes.parentError}
            </p>
          ) : null}
        </Row>
        <Row glyph="⊘" label={PROPERTY_LABELS.blocked_by}>
          <BlockedByControl
            draft={blockedByDraft}
            editing={blockedByEditing}
            issue={issue}
            onDraft={setBlockedByDraft}
            onEditing={setBlockedByEditing}
            titleOf={titleOf}
            write={blockedByWrite}
          />
          {/* The blockers are drawn by their keys where their titles could not be read, and the
              row says so rather than drawing keys alone as though they had none. */}
          {blockerTitles.isError ? (
            <DetailsErrorLine
              error={blockerTitles.error}
              message="Could not read the titles of these issues."
              onRetry={() => void blockerTitles.refetch()}
              retrying={blockerTitles.isFetching}
            />
          ) : null}
        </Row>
        {/* What this issue blocks: drawn whenever the panel is, with None where the answer holds
            nothing that waits on it, nothing while that answer is still out, and the failure
            where the read failed, as every other row of the panel says what it holds. */}
        <Row glyph="⊙" label={PROPERTY_LABELS.blocks}>
          {blocking.isError ? (
            <DetailsErrorLine
              error={blocking.error}
              message="Could not read what this issue blocks."
              onRetry={() => void blocking.refetch()}
              retrying={blocking.isFetching}
            />
          ) : blocking.isPending ? null : blocks.length === 0 ? (
            none
          ) : (
            <IssueList issues={blocks} />
          )}
        </Row>
        <Row glyph="⌂" label={PROPERTY_LABELS.repository}>
          <RepoControl
            disabled={isClosed}
            inRow
            issueKey={issue.key}
            project={issue.project}
            repo={issue.repo}
            resolvedRepo={issue.resolved_repo}
          />
        </Row>
        {/* Each link reads GitHub as it is drawn (`GitHubLink`), so the links are drawn only
            while Details is open, as the panel's other reads are made. */}
        <Row glyph="⇄" label={PROPERTY_LABELS.pull_request}>
          {pullRequests.length === 0 ? none : null}
          {reading ? pullRequests.map((link) => <GitHubLink key={link.url} link={link} />) : null}
        </Row>
        {otherLinks.length === 0 ? null : (
          <Row glyph="↗" label={PROPERTY_LABELS.external_links}>
            {reading ? otherLinks.map((link) => <GitHubLink key={link.url} link={link} />) : null}
          </Row>
        )}
        <Row glyph="⚑" label={PROPERTY_LABELS.claim}>
          <IssueClaimControl issue={issue} writes={writes} />
        </Row>
        <Row glyph="→" label={PROPERTY_LABELS.route} row="route">
          <IssueRouteControl
            controlRef={routeControl}
            editing={editingField === "route"}
            isClosed={isClosed}
            issue={issue}
            onEdit={() => onEditingField("route")}
            writes={writes}
          />
          <IssueFieldForms
            field={editingField === "route" ? "route" : null}
            isClosed={isClosed}
            issue={issue}
            onClose={closeEditor}
            writes={writes}
          />
          {/* A route Dispatch refused, said in the row it was saved from with Dispatch's reason
              and Retry, as the Parent row says a refused parent, for as long as the draft still
              holds a route the issue does not: a reader who leaves the editor puts the saved
              route back, and Retry would then send nothing. On a closed issue the line has no
              Retry: Dispatch refuses every route of a closed issue (`ISSUE_CLOSED`, a save out as
              the issue closed), and the drafts send none of it again (`useIssueDrafts`). The save
              is Details' own write (the details slot's `useIssueHeaderWrites`), and inline, so
              the write store leaves its refusal to this row, and the header, which says the
              page's other refusals, never sees it. */}
          {writes.updateIssue.isError &&
          writes.updateIssue.variables?.route !== undefined &&
          writes.drafts.route !== (issue.route ?? "") ? (
            <DetailsErrorLine
              error={writes.updateIssue.error}
              message={apiErrorMessage(writes.updateIssue.error, "Could not update this issue.")}
              onRetry={isClosed ? undefined : writes.drafts.retry}
              retrying={writes.updateIssue.isPending}
            />
          ) : null}
        </Row>
        <Row glyph="▦" label={PROPERTY_LABELS.components}>
          <IssueComponentsLine inRow issue={issue} />
        </Row>
        {issue.progress?.tasks === null || issue.progress?.tasks === undefined ? null : (
          <Row glyph="☑" label={PROPERTY_LABELS.tasks}>
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
            which is INFRA-2947's priority row with nothing in it yet. */}
        <div className="flex min-w-0 flex-wrap items-center gap-2 py-2">
          <IssueAddMenu isClosed={isClosed} issue={issue} onOpenField={onEditingField} />
          {advances.length === 0 ? (
            <div className="contents" data-testid="issue-advances">
              <IssuePriorityLine disabled={isClosed} inRow issue={issue} />
            </div>
          ) : null}
        </div>
      </PropertySection>
      <PropertySection drawerBox={drawerBox} name="people" sections={sections}>
        <Row glyph="☺" label={PROPERTY_LABELS.opened_by}>
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
            wherever no approval request waits on the reader (R13, R31). */}
        <Row glyph="▤" label={PROPERTY_LABELS.approval}>
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
              application's `ReferencedByToggle`, in the page's ink rather than its link blue. It
              is a control at every count: a command bar run the reader asked is in no reader's
              `referenced_by_count`, so a zero is no reason to shut the list. It reads the rows
              the list shows once both its reads have answered, the list's own and the runs'. */}
          <button
            aria-controls={referencesPanelId}
            aria-expanded={referencesOpen}
            className={`inline-flex min-h-11 items-center text-left text-sm font-medium md:min-h-0 ${ROW_LINK} ${pageInkText}`}
            onClick={() => setReferencesOpen((open) => !open)}
            ref={referencesToggle}
            type="button"
          >
            {referencesListed === undefined || !runs.settled
              ? PROPERTY_LABELS.referenced_by
              : `${PROPERTY_LABELS.referenced_by} (${referencesListed + runs.rows.length})`}
          </button>
          {referencesOpen ? (
            <>
              <ReferencedBy
                className="w-full"
                extraRows={runs.rows}
                page
                reference={issueReference}
                toggle={{ id: referencesPanelId }}
              />
              {/* A tree read that failed leaves the list's own rows drawn, with the failure
                  under them and its Retry, which reads the tree again. */}
              {runs.failed ? (
                <div ref={runsFailure}>
                  <DetailsErrorLine
                    message={ANSWER_LINES.references_unreadable}
                    onRetry={retryRuns}
                    retrying={runs.retrying}
                  />
                </div>
              ) : null}
            </>
          ) : null}
        </Row>
        {/* Every document and file of the issue, each opening its own address, folded after ten
            behind a count that opens the rest (R13). */}
        <Row
          glyph="▤"
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
                      {artifact.kind === "image" ? "▣" : "▤"}
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
      <IssuePins dismissed={stateForIssue(state, issue.key).dismissed ?? []} issueKey={issue.key} />
      <DetailsHistory issue={issue} state={state} />
    </div>
  );
}

/** The issues of a list of them, each its key as a link and its title beside it, folded after
 *  ten behind a count that opens the rest (R13). */
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

/** The count a folded list is drawn behind, which opens the rest of it (R13). `className`
 *  places it in the list that draws it, and `list` is that list, each of its children one
 *  entry. The count goes with its press, so the focus goes on to the link of the first entry
 *  it opened, the list's entry `FOLD_AFTER`, rather than falling to the page. */
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
 *  section draws none of its values, so what a reader sees is what the page holds. */
function PropertySection({
  children,
  drawerBox,
  name,
  sections,
}: {
  children: ReactNode;
  /** The drawer's `aside` the panel is placed in, which a request for a row is sent to. */
  drawerBox: HTMLElement | null;
  name: MarginSection;
  sections: MarginSections;
}): ReactNode {
  const open = sections.isOpen(name);
  // A control elsewhere on the page that opens Details at one of this section's rows asks the
  // drawer for that row before it looks for it (`DETAILS_ROW_REQUEST_EVENT`, part D's
  // `useOpenDetailsAt`). A closed section opens
  // for it as its heading opens it, so the reader's kept choice follows, and in the same task,
  // so the row is on the page when the control looks for it.
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
    <section data-margin-section={name}>
      <h2>
        <button
          aria-expanded={open}
          className={`flex min-h-11 w-full items-center justify-between gap-2 text-xs leading-4 font-semibold tracking-[0.08em] uppercase md:min-h-8 ${pageQuietText}`}
          onClick={() => sections.toggle(name)}
          type="button"
          {...pageChrome}
        >
          <span>{MARGIN_SECTION_NAMES[name]}</span>
          <span aria-hidden="true">{open ? "−" : "+"}</span>
        </button>
      </h2>
      {open ? <div className="flex min-w-0 flex-col">{children}</div> : null}
    </section>
  );
}

/**
 * One row of Details (the prototype's `.frow`): a column of 112 px with the property's shape
 * and its name, and the value beside it; a `wide` row is one column, for a property whose own
 * control names it. `row` names the row a control elsewhere opens Details at
 * (`details-rows.ts`).
 */
function Row({
  children,
  glyph,
  label,
  row,
  testId,
  wide = false,
}: {
  children: ReactNode;
  glyph?: string;
  label?: ReactNode;
  row?: DetailsRow;
  testId?: string;
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
          <span aria-hidden="true" className="w-3.5 shrink-0 text-center">
            {glyph}
          </span>
          <span className="min-w-0" {...pageChrome}>
            {label}
          </span>
        </div>
      )}
      <div
        className={`flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1 [overflow-wrap:break-word] ${pageInkText}`}
      >
        {children}
      </div>
    </div>
  );
}
