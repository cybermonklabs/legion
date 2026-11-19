import { useQuery } from "@tanstack/react-query";
import { type ReactNode, type RefCallback, useEffect, useId, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { projectsQuery, referenceTargetsQuery, whoAmIQuery } from "../../api/queries";
import type { Artifact, IssueDetails, UserState } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  dangerText,
  linkHoverText,
  linkText,
  pageAvatar,
  pageInkText,
  pageLabelChip,
  pageQuietText,
  pageRowBorder,
  pageRowHoverBg,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { useMarginDrawer, useMarginDrawsSlot } from "../margin/margin-slot";
import { IssuePriorityLine } from "../priorities/IssuePriorityLine";
import { AgentName } from "../refs/AgentName";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { ApprovalSteps } from "./ApprovalSteps";
import { BlockedByControl, IssueLine } from "./BlockedByControl";
import { DETAILS_ROW_ATTRIBUTE, type DetailsRow } from "./details-rows";
import { FocusedNodeFacts } from "./FocusedNodeFacts";
import type { FocusedNode } from "./focused-node";
import { GitHubLink } from "./GitHubLink";
import { gatedStepEntries, STEP_LABELS } from "./gated-steps";
import { IssueAddMenu } from "./IssueAddMenu";
import { IssueClaimControl } from "./IssueClaimControl";
import { IssueCollaboratorList } from "./IssueCollaboratorList";
import { IssueComponentsLine } from "./IssueComponentsLine";
import { type IssueField, IssueFieldForms } from "./IssueFieldForms";
import { IssueHistory } from "./IssueHistory";
import { IssueLabels } from "./IssueLabels";
import { IssuePins } from "./IssuePins";
import { statusGlyph, statusTone } from "./IssuePropertiesRow";
import { RouteControl } from "./IssueRouteControl";
import {
  authorText,
  MARGIN_SECTION_NAMES,
  type MarginSection,
  PROPERTY_LABELS,
  pageChrome,
} from "./page-structure";
import { CONTROL_LABELS, FILE_WORDS, issueStatusWord, PROGRESS_NOUNS } from "./page-words";
import { stateForIssue } from "./pins";
import { RepoControl } from "./RepoControl";
import { SubscribedAgents } from "./SubscribedAgents";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";
import { useIssueSubscribers } from "./useIssueSubscribers";
import { type MarginSections, useMarginSections } from "./useMarginSections";
import { waitingOnReader } from "./waiting";

/** How many entries a list of unbounded length draws before it folds behind a count that opens
 *  the rest (dispatch://INFRA-1196 R13). */
const FOLD_AFTER = 10;

/**
 * The margin's own side of the issue page's panel (dispatch://INFRA-1196 R12): the margin's
 * cards, and - where the margin keeps a column for that panel - the host the panel is moved
 * into, under them.
 *
 * The panel is drawn once, by the page, and moved between this host and the page's own column
 * rather than drawn afresh in each (`IssuePage`'s `panelStage`): a reader who has typed a
 * message, opened a blocked-by edit or opened one of the panel's lists still has it when the
 * margin is collapsed and shown again. Whether the margin keeps that column is read here as
 * the page reads it (`features/margin/margin-slot.tsx`), so both hosts answer one change of
 * it in one commit and the panel is never between hosts for a frame.
 */
export function IssueMarginCards({
  conversation,
  panelHost,
}: {
  /** The margin's own content, which this wraps where the panel sits inside the shell's
   *  margin. */
  conversation?: ReactNode;
  /** Takes the host the page's panel is drawn in, where the margin keeps a column for it; it
   *  is handed no host wherever the page draws the panel in its own column instead, and the
   *  margin draws its own cards alone. The wrapper is the same element in both states on
   *  purpose: the margin holds the open composer inside it, and a wrapper that changed shape
   *  as the margin collapsed would remount that composer and lose an unsent comment
   *  (`features/margin/MarginSheet.tsx`). */
  panelHost: RefCallback<HTMLElement>;
}): ReactNode {
  const drawsPanel = useMarginDrawsSlot();
  return (
    <div
      className={drawsPanel ? "flex min-w-0 flex-col gap-3" : "contents"}
      data-testid={drawsPanel ? "issue-margin" : undefined}
      ref={drawsPanel ? panelHost : undefined}
    >
      {/* The margin's own cards first, and the panel's own sections under them, because the
          page's panel is appended to this wrapper (`features/margin/margin-slot.tsx`): the
          issue's asks, comments, messages and decisions, in the margin's words and the
          authors' own, and not the page's declared vocabulary (dispatch://INFRA-1196 R21,
          `page-structure.ts`). They are first because the margin places an anchored card at
          a `top` it measures down from the top of its own content (`margin/ThreadList.tsx`'s
          `regionOrigin`), so anything drawn over them moves that anchor and a card can no
          longer reach the line its mark is on. */}
      <div className="contents" {...authorText}>
        {conversation}
      </div>
    </div>
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
 * Under the properties, the facts of whatever the reader's focus is on with what the reader can
 * do to it, the reader's pins as links to their rows, and the issue's history (R14, R15). It
 * holds no list of the conversation: the outline is the conversation.
 *
 * The page draws it once and moves it into whichever host is on the screen - the shell's
 * drawer, or the page's own column where no shell draws one - so nothing in it is thrown away as
 * the reader moves between them. What it reads that the header does not - the titles of the
 * issues around this one, the issue's event log - it reads only while it is open, so a load that
 * leaves Details shut costs no request for it.
 */
export function IssueMarginPanel({
  documentArtifact,
  editingField,
  focused,
  history,
  isClosed,
  issue,
  onEditingField,
  openCollaborators = false,
  state,
  writes,
}: {
  /** Whether the address opens the panel at the history, and the entry of it the address names
   *  (R15, R20). */
  history: { readonly item: string | undefined; readonly open: boolean };
  /** The issue's primary document, whose approval the Approval row draws. */
  documentArtifact: Artifact | undefined;
  /** The editor open in its row - the parent's or the route's - or `null`. */
  editingField: IssueField | null;
  /** What the reader's focus is on, which the page reads and hands over (`focused-node.tsx`). */
  focused: FocusedNode;
  isClosed: boolean;
  issue: IssueDetails;
  onEditingField: (field: IssueField | null) => void;
  /** Whether the People section opens with the issue's collaborators drawn, which is what
   *  `/issues/<key>/collaborators` - the address the Collaborators tab answered - names
   *  (R19). Every other address leaves the list closed and reads nothing. */
  openCollaborators?: boolean;
  /** The reader's own state of every issue, whose pins the panel lists (R14). */
  state: UserState | undefined;
  /** The page's one set of the issue's writes, which the header draws too. */
  writes: IssueHeaderWrites;
}): ReactNode {
  const sections = useMarginSections();
  const subscribers = useIssueSubscribers(issue.key);
  const [subscribersOpen, setSubscribersOpen] = useState(false);
  const [collaboratorsOpen, setCollaboratorsOpen] = useState(openCollaborators);
  const [referencesOpen, setReferencesOpen] = useState(false);
  const [filesOpen, setFilesOpen] = useState(false);
  const referencesPanelId = useId();
  const issueReference = buildDispatchReference({ key: issue.key, kind: "issue" });
  // The address opens the list; closing it again is the reader's, so this only ever opens.
  useEffect(() => {
    if (openCollaborators) setCollaboratorsOpen(true);
  }, [openCollaborators]);
  // What Details reads beyond the issue - the titles of the issues around this one, and the
  // issue's event log - only while the drawer is open, so a load that leaves it shut costs no
  // request for it. Outside the shell, where no drawer holds the panel, it is always open.
  const drawer = useMarginDrawer();
  const reading = drawer?.open === true;
  const historyShown = drawer === undefined || reading || history.open;
  const blockers = issue.blocked_by ?? [];
  const blockerTitles = useQuery({
    ...referenceTargetsQuery(blockers.map((key) => buildDispatchReference({ key, kind: "issue" }))),
    enabled: reading && blockers.length > 0,
  });
  const titleOf = (key: string) =>
    blockerTitles.data?.targets.find(
      (target) => target.ref === buildDispatchReference({ key, kind: "issue" })
    )?.name;
  // What waits on this issue is the other direction of `blocked_by`: the references of that
  // kind that point at it.
  const blocking = useQuery({
    enabled: reading,
    queryFn: () => api.getReferences(issueReference, { kind: "blocked_by" }),
    queryKey: ["references", issueReference, "blocked_by"],
  });
  const blocks = (blocking.data?.edges ?? [])
    .filter((edge) => edge.kind === "blocked_by" && edge.node.issue_key !== undefined)
    .map((edge) => ({ key: edge.node.issue_key ?? "", title: edge.node.text ?? "" }));
  // Approve and Request changes are here wherever no approval request waits on the reader,
  // and in the header while one does (R31): the approvals are all this reads of the count, so it
  // hands it no decisions.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const approvalWaits =
    waitingOnReader(issue, viewer, undefined, () => false, undefined).approvals.length > 0;
  // The status as the header draws it: the one being saved while its write is in flight, so the
  // page never says two statuses at once.
  const status = writes.pendingStatus ?? issue.status;
  const projects = useQuery({ ...projectsQuery(), enabled: issue.parent === null });
  const gated = gatedStepEntries(
    projects.data?.find((candidate) => candidate.key === issue.project),
    issue
  );
  const approvalEntries =
    gated.length > 0 && gated.some((entry) => entry.document?.id === issue.primary_artifact_id)
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
      <PropertySection name="development" sections={sections}>
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
            <IssuePriorityLine disabled={isClosed} issue={issue} />
          </Row>
        )}
        <Row glyph="↑" label={PROPERTY_LABELS.parent} row="parent">
          {issue.parent === null ? (
            none
          ) : (
            <Link
              className={`dispatch-inline-link underline ${linkText} ${linkHoverText}`}
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
            onClose={() => onEditingField(null)}
            writes={writes}
          />
          {writes.parentSaveFailed ? (
            <p className={`w-full text-sm ${dangerText}`} id="issue-parent-error">
              {writes.parentError}
            </p>
          ) : null}
        </Row>
        <Row glyph="⊘" label={PROPERTY_LABELS.blocked_by}>
          <BlockedByControl issue={issue} titleOf={titleOf} />
          {/* The blockers are drawn by their keys where their titles could not be read, and the
              row says so rather than drawing keys alone as though they had none. */}
          {blockerTitles.isError ? (
            <QueryError
              error={blockerTitles.error}
              message="Could not read the titles of these issues."
              onRetry={() => void blockerTitles.refetch()}
              retrying={blockerTitles.isFetching}
            />
          ) : null}
        </Row>
        {/* What this issue blocks: drawn whenever the panel is, with None where nothing waits
            on it and the failure where the read failed, as every other row of the panel says
            what it holds. */}
        <Row glyph="⊙" label={PROPERTY_LABELS.blocks}>
          {blocking.isError ? (
            <QueryError
              error={blocking.error}
              message="Could not read what this issue blocks."
              onRetry={() => void blocking.refetch()}
              retrying={blocking.isFetching}
            />
          ) : blocks.length === 0 ? (
            none
          ) : (
            <IssueList issues={blocks} />
          )}
        </Row>
        <Row glyph="⌂" label={PROPERTY_LABELS.repository}>
          <RepoControl
            disabled={isClosed}
            issueKey={issue.key}
            project={issue.project}
            repo={issue.repo}
            resolvedRepo={issue.resolved_repo}
          />
        </Row>
        <Row glyph="⇄" label={PROPERTY_LABELS.pull_request}>
          {pullRequests.length === 0
            ? none
            : pullRequests.map((link) => <GitHubLink key={link.url} link={link} />)}
        </Row>
        {otherLinks.length === 0 ? null : (
          <Row glyph="↗" label={PROPERTY_LABELS.external_links}>
            {otherLinks.map((link) => (
              <GitHubLink key={link.url} link={link} />
            ))}
          </Row>
        )}
        <Row glyph="⚑" label={PROPERTY_LABELS.claim}>
          <IssueClaimControl issue={issue} writes={writes} />
        </Row>
        <Row glyph="→" label={PROPERTY_LABELS.route} row="route">
          <RouteControl
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
            onClose={() => onEditingField(null)}
            writes={writes}
          />
        </Row>
        <Row glyph="▦" label={PROPERTY_LABELS.components}>
          <IssueComponentsLine issue={issue} />
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
              <IssuePriorityLine disabled={isClosed} issue={issue} />
            </div>
          ) : null}
        </div>
      </PropertySection>
      <PropertySection name="people" sections={sections}>
        <Row glyph="☺" label={PROPERTY_LABELS.opened_by}>
          {issue.created_by == null ? (
            none
          ) : (
            <span {...authorText}>
              <AgentName actor={issue.created_by} author inline />
            </span>
          )}
        </Row>
        <Row wide>
          <button
            aria-expanded={subscribersOpen}
            className={`flex min-h-11 items-center gap-2 rounded-lg text-sm font-medium md:min-h-8 ${textSecondaryOnSurface}`}
            onClick={() => setSubscribersOpen((open) => !open)}
            type="button"
            {...pageChrome}
          >
            <span>
              {subscribers.counted
                ? `${PROPERTY_LABELS.subscribers}:`
                : PROPERTY_LABELS.subscribers}
            </span>
            {subscribers.counted ? <span {...authorText}>{subscribers.rows.length}</span> : null}
          </button>
          {subscribersOpen ? (
            <SubscribedAgents
              onUnsubscribe={(sessionId) => subscribers.unsubscribe.mutate(sessionId)}
              ownerLabel={issue.key}
              subscribers={subscribers.rows}
            />
          ) : null}
          {subscribers.error ? (
            <QueryError
              error={subscribers.failure}
              message="Subscribed agents unavailable — Envoy listener unreachable."
              onRetry={subscribers.refetch}
              retrying={subscribers.fetching}
            />
          ) : null}
          {subscribers.unsubscribe.isError ? (
            <QueryError
              message="Could not unsubscribe this agent."
              onRetry={() =>
                subscribers.unsubscribe.mutate(subscribers.unsubscribe.variables ?? "")
              }
              retrying={subscribers.unsubscribe.isPending}
            />
          ) : null}
        </Row>
        {/* Everyone who worked the issue, running or not, which the nine-tab page gave a tab
            of its own. It is read only once a reader opens it, so the panel costs the page no
            request on an address that does not name it (R20). */}
        <Row row="collaborators" wide>
          <button
            aria-expanded={collaboratorsOpen}
            className={`flex min-h-11 items-center gap-2 rounded-lg text-sm font-medium md:min-h-8 ${textSecondaryOnSurface}`}
            onClick={() => setCollaboratorsOpen((open) => !open)}
            type="button"
            {...pageChrome}
          >
            <span>{PROPERTY_LABELS.collaborators}</span>
          </button>
          {collaboratorsOpen ? (
            <div className="w-full" {...authorText}>
              <IssueCollaboratorList issueKey={issue.key} />
            </div>
          ) : null}
        </Row>
      </PropertySection>
      <PropertySection name="references" sections={sections}>
        {/* The approval steps with the reasons a reviewer gave, and Approve and Request changes
            wherever no approval request waits on the reader (R13, R31). */}
        <Row glyph="▤" label={PROPERTY_LABELS.approval}>
          <ApprovalSteps
            actions={!approvalWaits}
            entries={approvalEntries}
            primaryArtifactId={issue.primary_artifact_id}
            reasons
            testId="issue-details-approval-steps"
            versions={{ asks: issue.open_asks ?? [], issueKey: issue.key }}
          />
        </Row>
        <Row wide>
          <ReferencedByToggle
            controls={referencesPanelId}
            count={issue.referenced_by_count}
            expanded={referencesOpen}
            onToggle={() => setReferencesOpen((open) => !open)}
          />
          {referencesOpen ? (
            <ReferencedBy
              className="w-full"
              reference={issueReference}
              toggle={{ id: referencesPanelId }}
            />
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
            <ul className="flex w-full min-w-0 flex-col">
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
            <button
              className={`pt-1.5 text-xs font-semibold underline underline-offset-[3px] ${pageInkText}`}
              onClick={() => setFilesOpen(true)}
              type="button"
              {...pageChrome}
            >
              {CONTROL_LABELS.show_all} <span {...authorText}>{artifacts.length}</span>
            </button>
          ) : null}
        </Row>
      </PropertySection>
      <FocusedNodeFacts issueKey={issue.key} node={focused} state={state} />
      <IssuePins dismissed={stateForIssue(state, issue.key).dismissed ?? []} issueKey={issue.key} />
      {historyShown ? <IssueHistory issue={issue} item={history.item} state={state} /> : null}
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
  const shown = open ? issues : issues.slice(0, FOLD_AFTER);
  return (
    <div className="flex w-full min-w-0 flex-col gap-1">
      {shown.map((one) => (
        <IssueLine issueKey={one.key} key={one.key} title={one.title} />
      ))}
      {issues.length > FOLD_AFTER && !open ? (
        <button
          className={`self-start text-xs font-semibold underline underline-offset-[3px] ${pageInkText}`}
          onClick={() => setOpen(true)}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.show_all} <span {...authorText}>{issues.length}</span>
        </button>
      ) : null}
    </div>
  );
}

/** One named section of the panel's properties, open or closed by its own one control. A closed
 *  section draws none of its values, so what a reader sees is what the page holds. */
function PropertySection({
  children,
  name,
  sections,
}: {
  children: ReactNode;
  name: MarginSection;
  sections: MarginSections;
}): ReactNode {
  const open = sections.isOpen(name);
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
