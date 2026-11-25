import { ISSUE_TITLE_MAX } from "@legion/contracts/dispatch-constants";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type KeyboardEvent, type ReactNode, useId, useLayoutEffect, useState } from "react";
import { Link } from "react-router-dom";

import {
  AGENT_LIST_REFILLING,
  api,
  apiErrorCodeIn,
  apiErrorMessage,
  isSubscriberNotFound,
  issuePatchRequest,
  removedEvenIfGone,
} from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { projectsQuery, userStateQuery } from "../../api/queries";
import { replaceQueryData } from "../../api/query-client";
import type { Artifact, IssueDetails, UserIssueState, UserState } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import { QueryError } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import { inputMethodKey } from "../../hooks/inputMethodKey";
import { useScrollFadeTrailing } from "../../hooks/useScrollFadeTrailing";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  badgePrimary,
  bgTransparent,
  borderDefault,
  borderStrongHover,
  borderTransparent,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  card,
  dangerText,
  focusBorder,
  focusVisibleRing,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  statusPill,
  statusPillDot,
  surfaceMutedStrongBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { ReaderIsHere, useAfterTheFirstBatch } from "../doc/first-batch";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { IssuePriorityLine } from "../priorities/IssuePriorityLine";
import { statusLabel } from "../project/board-model";
import { AgentName, useAgentText } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, buildIssuePath, sessionOfAddress } from "../refs/routes";
import { useKeymap } from "../shell/keymap";
import { PHONE_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { ApprovalSteps } from "./ApprovalSteps";
import { AssigneeControl } from "./AssigneeControl";
import { ClaimChip } from "./ClaimChip";
import { GitHubLink } from "./GitHubLink";
import { gatedStepEntries, isProjectRoot } from "./gated-steps";
import { IssueComponentsLine } from "./IssueComponentsLine";
import { IssueParentForm, IssueRouteForm, routeHint } from "./IssueHeaderForms";
import { IssueLabels } from "./IssueLabels";
import {
  issueClosedCalloutClassName,
  issueHeaderFrameClassName,
  issueKeyBadgeClassName,
  issueKeyTextClassName,
  issueStateButtonClassName,
  issueStatusPillClassName,
  issueStatusPillDotClassName,
  issueStatusSelectClassName,
  issueTitleBoxClassName,
  issueTitleTextClassName,
} from "./issue-header-classes";
import { closeIssueWords, releaseClaimWords, selectableStatuses } from "./issue-header-words";
import { PriorityEditor } from "./PriorityControl";
import { ProgressChips } from "./ProgressChips";
import { IssueStatusNotes } from "./parent-status/IssueStatusNotes";
import { useIssueStatusMove } from "./parent-status/useIssueStatusMove";
import { stateForIssue } from "./pins";
import { RepoControl } from "./RepoControl";
import {
  RouteReachUnknownMarker,
  UnreachableRouteMarker,
  UnresponsiveRouteMarker,
} from "./RouteReach";
import { theChromeIsHere } from "./reaching-the-chrome";
import { SubscribedAgents } from "./SubscribedAgents";
import { type IssueUpdateInput, useIssueDrafts } from "./useIssueDrafts";
import type { IssuePriorityWrite } from "./useIssuePriority";

const titleRefusals = new Set(["CAP_EXCEEDED", "ISSUE_CLOSED"]);

/** Whether `input` is the Status pill's pick alone: that control shows its write in the shell's
 *  indicator and rolls back on a refusal, where every other header field holds its draft and
 *  says why itself. */
function statusPickOnly(input: IssueUpdateInput | undefined): boolean {
  return input !== undefined && Object.keys(input).length === 1 && input.status !== undefined;
}

/** What the shell's indicator names one header write of `key`. */
function issueUpdateLabel(key: string, input: IssueUpdateInput): string {
  const field = Object.keys(input)[0] ?? "field";
  return input.status === undefined ? `${key}: ${field} saved` : `${key} set to ${input.status}`;
}

export function IssueHeader({
  documentArtifact,
  isClosed,
  issue,
  priorityWrite,
  state,
}: {
  documentArtifact: Artifact | undefined;
  isClosed: boolean;
  issue: IssueDetails;
  /** The page's one priority write, shared with the keyboard's `0`–`3` and the palette's Set
   *  priority rows, so a refusal of any of them is reported once and by this header. */
  priorityWrite: IssuePriorityWrite;
  state: UserIssueState;
}): ReactNode {
  const queryClient = useQueryClient();
  // A key the reader pressed while the page was still holding this header is done here, in the
  // commit that mounts the controls it was reaching for (`reaching-the-chrome.ts`). The layout
  // phase, so the control it focuses or presses is theirs before this header is painted.
  useLayoutEffect(() => theChromeIsHere("header"), []);
  const [editingTitle, setEditingTitle] = useState(false);
  const [routeEditing, setRouteEditing] = useState(false);
  const [parentEditing, setParentEditing] = useState(false);
  // The details rail's trailing fade, worn while the rail has anywhere left to scroll.
  const railFade = useScrollFadeTrailing();
  const [subscribersOpen, setSubscribersOpen] = useState(false);
  const [referencesOpen, setReferencesOpen] = useState(false);
  const agentText = useAgentText();
  // Who is subscribed is a panel of its own, shut until the reader opens it, and its count is
  // a number beside that toggle: nothing the reader is waiting for while a document opens
  // beneath it. So the read waits for the editor's first batch, and goes out at once for a
  // reader who opens the panel first (`features/doc/first-batch.ts`).
  const subscribers = useQuery({
    enabled: useAfterTheFirstBatch(subscribersOpen),
    queryKey: ["subscribers", issue.key],
    queryFn: () => api.getIssueSubscribers(issue.key),
  });
  const subscriberList = subscribers.isError ? [] : (subscribers.data ?? []);
  // The count is a claim about who is subscribed, so only a read that answered makes it.
  const subscribersCounted = !subscribers.isError && subscribers.data !== undefined;
  // Every write here names `write` before its callbacks: its answer and its variables are what
  // the callbacks below read.
  const unsubscribe = useKeyedWrite({
    // An unsubscribe made again after one that landed finds the session no longer subscribed
    // (`404 SUBSCRIBER_NOT_FOUND`), which is the unsubscribe done (`removedEvenIfGone`).
    method: "DELETE",
    write: (sessionId: string, bounds) =>
      removedEvenIfGone(
        api.unsubscribeIssueSession(issue.key, sessionId, bounds),
        isSubscriberNotFound
      ),
    inline: true,
    label: (sessionId) =>
      `${agentText.sessionText(sessionId, subscriberList.find((row) => row.session_id === sessionId)?.title)} unsubscribed from ${issue.key}`,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["subscribers", issue.key] });
    },
  });
  const releaseClaim = useKeyedWrite({
    // A release made again after one that landed needs no `removedEvenIfGone`: Dispatch answers a
    // release of an issue nobody holds with the issue, as it answers the first.
    method: "DELETE",
    write: (_variables, bounds) => api.releaseIssueClaim(issue.key, bounds),
    inline: true,
    label: () => `Claim on ${issue.key} released`,
    onSuccess: (next) => {
      if (next !== null) mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  // Every status write of the header - the select, Close, Reopen and the palette's rows - goes
  // through the rule's question (`useIssueStatusMove`): a dry run first for an issue with a
  // parent or an open child, then the write below, or the question where Dispatch's rule would
  // refuse it.
  const statusMove = useIssueStatusMove(issue);
  // A title, route, parent or close the header shows refused itself, beside its draft; a status
  // pick is a control, so its refusal is the shell's (`PendingWritesIndicator`), and the pick
  // rolls back. Every field write goes in the issue's line (`issue:<key>`), so an older one still
  // being tried never lands after a newer one, whatever left the page meanwhile. A landing with no
  // answer to give is merged by reading the issue again.
  const updateIssue = useKeyedWrite({
    inline: (input) => !statusPickOnly(input),
    label: (input) => issueUpdateLabel(issue.key, input),
    target: () => `issue:${issue.key}`,
    request: (input: IssueUpdateInput) => issuePatchRequest(issue.key, input),
    // A status the page took for one the rule could not refuse, which Dispatch refused under it
    // all the same, asks as a refused dry run does (`statusMove.onError`).
    onError: (error, input) => {
      statusMove.onError(error, issue.key, input);
    },
    onSettled: (_data, error) => {
      drafts.onIssueSettled(error);
    },
    onSuccess: (next, input) => {
      if (next === null) {
        void queryClient.invalidateQueries({ queryKey: ["issue", issue.key] });
      } else {
        mergeIssue(queryClient, next);
      }
      drafts.onIssueSuccess(next ?? issue, input);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const labelsMutation = useKeyedWrite({
    label: () => `Labels of ${issue.key}`,
    target: () => `issue:${issue.key}`,
    request: (labels: string[]) => issuePatchRequest(issue.key, { labels }),
    onSuccess: (next) => {
      if (next !== null) mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const drafts = useIssueDrafts(issue, updateIssue);
  const requestStatus = (status: string) => {
    statusMove.move(status, () => drafts.requestStatusSubmit(status));
  };
  // A status write is out, or the rule's question is checking or asking about one: no second
  // status write can start.
  const statusBusy = updateIssue.isPending || statusMove.pending !== undefined;
  // Palette-only: closing and reopening an issue are too consequential for a single key, so they
  // are reachable from `$mod+k` alone, through the same handlers and the same guards as the
  // Close and Reopen buttons below. `IssuePage` pushes the `issue` scope these are offered under.
  // Each is a write of the issue and moves nobody: the palette that offers it took the focus.
  useKeymap("issue", [
    {
      effect: "moves nothing",
      id: "close",
      keys: [],
      label: "Close issue",
      run: () => requestStatus("done"),
      // `disabled={statusBusy}` on the button, and `isClosed` decides whether it is rendered at
      // all; a row offered while a status write is in flight would submit a second one.
      when: () => !isClosed && !statusBusy,
    },
    {
      effect: "moves nothing",
      id: "reopen",
      keys: [],
      label: "Reopen issue",
      run: () => requestStatus("backlog"),
      when: () => isClosed && !statusBusy,
    },
    ...([0, 1, 2, 3] as const).map((level) => ({
      effect: "moves nothing" as const,
      id: `set-p${level}`,
      keys: [],
      label: `Set priority P${level}`,
      run: () => priorityWrite.submit(level),
      // The same condition as `PriorityEditor`'s `disabled` below.
      when: () => !(isClosed || updateIssue.isPending),
    })),
  ]);
  // The status the select and the closed line show while it is out: the one the rule's question
  // is checking or asking about, else the one the write is saving.
  const checkingStatus = statusMove.pending;
  const statusSaving =
    checkingStatus !== undefined ||
    (updateIssue.isPending && updateIssue.variables?.status !== undefined);
  const pendingStatus =
    checkingStatus ?? (statusSaving ? updateIssue.variables?.status : undefined);
  const openAsks = (issue.open_asks ?? []).filter((ask) => ask.state === "open");
  // The detail carries waiting_on on every open ask; a response without it cannot say whose
  // turn it is, so the indicator stays off rather than shown wrong.
  const knowsWhoseTurn = openAsks.every((ask) => ask.waiting_on !== undefined);
  const waitingForHuman = waitingOnHuman(openAsks);
  const waitingOnAgents = openAsks.length - waitingForHuman.length;
  const whoseTurn =
    openAsks.length === 0 || !knowsWhoseTurn
      ? null
      : waitingForHuman.length > 0
        ? `Waiting on you (${waitingForHuman.length})`
        : `Waiting on agents (${waitingOnAgents})`;
  // The priorities the issue advances, each the chip of its place in the order, and the picker's
  // "+". From 768 px up they stand in the state row right after the P0-P3 tag: that row wraps,
  // where the details row scrolls sideways and is long enough without them. Below it the state
  // row has no room left on its first line, so there they stand first in the details row, in view
  // before it is scrolled: a line of their own in the state row would push the page down under
  // every sheet and pill a phone pins to the screen's foot. One line is drawn, in one place.
  const phone = useMediaQuery(PHONE_VIEWPORT_QUERY);
  const priorityLine = (
    <IssuePriorityLine
      className={phone ? "shrink-0" : undefined}
      disabled={isClosed}
      issue={issue}
    />
  );
  const approvalRequestOpen = openAsks.some((ask) => ask.kind === "approval");
  // The one shared read of the projects, under the key the shell's sidebar and the search
  // palette already hold, so the daemon's `project.updated` publish refreshes these steps with
  // no reload and the header makes no second, per-issue read. While that read is pending or
  // failed there is no row, so the steps are empty and the header draws its one chip below
  // rather than claiming a gate it has not read. Only a root of the issue's project has steps at
  // all (`isProjectRoot`: no parent, or a parent of another project), so a child issue does not
  // ask for the list: a disabled query still reads whatever the shared cache holds, and a child
  // draws the single chip either way.
  const projects = useQuery({ ...projectsQuery(), enabled: isProjectRoot(issue) });
  const projectRow = projects.data?.find((candidate) => candidate.key === issue.project);
  const entries = gatedStepEntries(projectRow, issue);
  // Which document a step reviews is `gated-steps.ts`'s to say, so the chip asks the drawn
  // entries rather than the gate's words: a gate that names no step for the primary document
  // leaves that document's own state and its two controls to the chip.
  const stepHoldsPrimaryDocument = entries.some(
    (entry) => entry.document?.id === issue.primary_artifact_id
  );
  const showApprovalActions =
    documentArtifact?.approval !== undefined &&
    (documentArtifact.approval.state !== "draft" || approvalRequestOpen);
  const pinGuard = useSubmitGuard();
  const updateState = useKeyedWrite({
    // A read of the state asked while the pin is out could be answered before Dispatch takes it,
    // with the pin as it was: it waits for the answer, for at most `SEND_DEADLINE_MS`. The state
    // is the viewer's alone, so the wait keeps back nobody else's change.
    holds: () => ({ keys: [userStateQuery().queryKey] }),
    label: (pinned: boolean) => `${issue.key} ${pinned ? "pinned" : "unpinned"}`,
    method: "PUT",
    write: (pinned: boolean, bounds) => api.putIssueState(issue.key, { pinned }, bounds),
    onSettled: () => {
      pinGuard.release();
      void queryClient.invalidateQueries({ queryKey: userStateQuery().queryKey });
      // The sidebar lists the pinned issues from their own read, which follows the pin however
      // its write settled: with an answer, landed with none (`null`), or refused.
      void queryClient.invalidateQueries({ exact: true, queryKey: ["issues", "pinned"] });
    },
    onMutate: async (pinned) => {
      await queryClient.cancelQueries({ queryKey: userStateQuery().queryKey });
      const previous = queryClient.getQueryData<UserState>(userStateQuery().queryKey);
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issue.key]: { ...state, pinned },
      }));
      return { previous };
    },
    onError: (_error, _pinned, context) => {
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issue.key]: stateForIssue(context?.previous, issue.key),
      }));
    },
    onSuccess: (next) => {
      if (next === null) return;
      // Over any read of the state still out, asked before the answer (`replaceQueryData`).
      replaceQueryData<UserState>(queryClient, userStateQuery().queryKey, (current) => ({
        ...current,
        [issue.key]: next,
      }));
    },
  });
  const saveTitleOnEnter = (event: KeyboardEvent<HTMLInputElement>) => {
    // The Enter that confirms an input method's word is the input method's, not the save.
    if (event.key === "Enter" && !inputMethodKey(event)) {
      event.currentTarget.blur();
    }
  };
  // A refused parent save (PARENT_INPUT, ISSUE_CLOSED) renders the server's reason inline,
  // like a route validation error, instead of the generic update failure line. One the reader
  // stopped may have been saved; the editor has closed, and saving it again is a press of its own
  // (`requestParentSubmit`).
  const parentSaveFailed = updateIssue.isError && updateIssue.variables?.parent !== undefined;
  const parentError =
    updateIssue.error instanceof SendStoppedError
      ? stoppedWriteNote("the new parent", "saved", {
          anew: "Saving it again sends it as a new write.",
        })
      : apiErrorMessage(updateIssue.error, "Could not save parent.");
  // A title Dispatch refused (CAP_EXCEEDED past ISSUE_TITLE_MAX, ISSUE_CLOSED) shows its reason
  // with no Retry, since the same title is refused every time; any other failure keeps the generic
  // line and its Retry.
  const titleRefusal =
    updateIssue.isError &&
    updateIssue.variables?.title !== undefined &&
    apiErrorCodeIn(updateIssue.error, titleRefusals)
      ? updateIssue.error.message
      : null;
  // A `session:` route is an agent: its button names it as `AgentName` does, with the agent's own
  // live link beside it. A role route is its address.
  const routeSession = sessionOfAddress(drafts.route);
  const routeLabel = `Messages default to ${
    drafts.route === ""
      ? "no route"
      : routeSession === undefined
        ? drafts.route
        : agentText.sessionText(routeSession, undefined)
  }`;
  const routeButton = (name: ReactNode) => (
    <button
      aria-label={routeLabel}
      className={`inline-flex min-h-11 max-w-[14ch] shrink-0 items-center truncate rounded-full px-3 py-2 text-left text-sm outline-none focus-visible:ring-2 xl:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
      disabled={isClosed}
      onClick={() => setRouteEditing(true)}
      title={drafts.route === "" ? undefined : drafts.route}
      type="button"
    >
      <TruncatedText>{name}</TruncatedText>
    </button>
  );
  const issueReference = buildDispatchReference({ key: issue.key, kind: "issue" });
  const referencesPanelId = useId();
  // The title slot has one flex-basis whether it shows the heading or the editor: below 2xl the
  // title always takes its own row (basis-full) and the state controls and details line share
  // the row beneath it; from 2xl the slot is content-sized (basis-auto) so they join the title's
  // row when they fit. A basis that changed between view and edit mode would move every other
  // control on the blur a mousedown causes, and Chromium would deliver the mouseup — and so the
  // click — to a different element, swallowing the first click after a title edit.

  return (
    // `data-issue-header` is load-bearing for the keyboard: the `issue` scope's bindings find
    // the Status, priority, labels, title and pin controls through it, and scoping to it is
    // what keeps a child issue's row in the Children tab, which carries the same accessible
    // names, out of their reach.
    <header
      className={`${issueHeaderFrameClassName} ${card}`}
      data-issue-header=""
      data-testid="issue-header"
    >
      {/* The closed block, from the one place the space the page keeps for this header builds
          it too (`issue-header-classes.ts`, `issue-page-skeleton.tsx`), so what stood there and
          what lands here are the same block. */}
      {isClosed ? (
        <div
          className={`${issueClosedCalloutClassName} ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
        >
          <span>This issue is closed.</span>
          <button
            aria-label="Reopen issue"
            className={`min-h-11 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
            disabled={statusBusy}
            onClick={() => requestStatus("backlog")}
            title="Reopen into Backlog"
            type="button"
          >
            {pendingStatus === "backlog" ? "Reopening…" : "Reopen"}
          </button>
        </div>
      ) : null}
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex min-w-0 grow basis-full flex-col gap-2 sm:flex-row sm:items-start 2xl:basis-auto">
          <div className="flex shrink-0 items-center gap-1 self-start">
            <p
              className={`${issueKeyBadgeClassName} ${issueKeyTextClassName} ${badgePrimary.bg} ${badgePrimary.text}`}
            >
              {issue.key}
            </p>
            <CopyRefButton
              primary={{ label: `issue key ${issue.key}`, value: issue.key }}
              route={{ key: issue.key, kind: "issue" }}
            />
          </div>
          <div className="flex min-w-0 flex-1 items-start gap-2">
            {editingTitle ? (
              <input
                aria-describedby={drafts.titleError === null ? undefined : "issue-title-help"}
                aria-label="Issue title"
                className={`${issueTitleBoxClassName} text-[22px] leading-7 font-semibold tracking-tight outline-none ${borderTransparent} ${bgTransparent} ${textPrimaryOnSurface} ${borderStrongHover} ${focusBorder}`}
                disabled={isClosed}
                maxLength={ISSUE_TITLE_MAX}
                onBlur={() => {
                  drafts.requestTitleSubmit();
                  setEditingTitle(false);
                }}
                onChange={(event) => drafts.writeTitle(event.target.value)}
                onKeyDown={(event) => {
                  // The Escape that ends an input method's composition is the input method's,
                  // not the reader dropping the title (`inputMethodKey`).
                  if (event.key === "Escape" && !inputMethodKey(event)) {
                    drafts.discardTitle();
                    setEditingTitle(false);
                    return;
                  }
                  saveTitleOnEnter(event);
                }}
                ref={(node) => node?.focus()}
                value={drafts.title}
              />
            ) : (
              <h1
                className={`${issueTitleBoxClassName} ${issueTitleTextClassName} ${borderTransparent} ${textPrimaryOnSurface} ${
                  isClosed ? "" : `cursor-text ${borderStrongHover}`
                }`}
                onClick={() => {
                  if (!isClosed) {
                    setEditingTitle(true);
                  }
                }}
                onFocus={() => {
                  if (!isClosed) {
                    setEditingTitle(true);
                  }
                }}
                onKeyDown={(event) => {
                  if (!isClosed && (event.key === "Enter" || event.key === " ")) {
                    event.preventDefault();
                    setEditingTitle(true);
                  }
                }}
                tabIndex={isClosed ? -1 : 0}
              >
                {drafts.title}
              </h1>
            )}
            <PinButton
              disabled={updateState.isPending}
              label={state.pinned ? "Unpin issue" : "Pin issue"}
              onClick={() => pinGuard.guard(() => updateState.mutate(!state.pinned))}
              pinned={state.pinned}
            />
          </div>
        </div>
        <div
          className="flex w-full min-w-0 flex-wrap items-center gap-2 md:w-auto"
          data-testid="issue-state-actions"
        >
          <label
            className={`${issueStatusPillClassName} ${borderDefault} ${statusPill.bg} ${statusPill.text}`}
          >
            <span
              aria-hidden="true"
              className={`${issueStatusPillDotClassName} ${statusPillDot}`}
            />
            <select
              aria-label="Status"
              className={`${issueStatusSelectClassName} ${bgTransparent} ${statusPill.text}`}
              disabled={isClosed || statusBusy}
              onChange={(event) => requestStatus(event.target.value)}
              value={pendingStatus ?? issue.status}
            >
              {selectableStatuses(isClosed).map((status) => (
                <option key={status} value={status}>
                  {statusLabel(status)}
                </option>
              ))}
            </select>
          </label>
          <PriorityEditor
            disabled={isClosed || updateIssue.isPending}
            issueKey={issue.key}
            write={priorityWrite}
          />
          {phone ? null : priorityLine}
          <AssigneeControl
            assignee={issue.assignee}
            disabled={isClosed || updateIssue.isPending}
            issueKey={issue.key}
          />
          {/* Progress is state about how far the work has got, like the status and the claim,
              so it sits in this wrapping row with them; the metadata rail below clips. */}
          <ProgressChips bar progress={issue.progress} />
          {/* A root issue of a gated project reads its gate step by step, after the chip's
              place; the chip itself stays beside those steps whenever no step drawn reviews the
              primary document, since otherwise an open request on that document could not be
              answered on this page at all. Every other issue - and any issue whose project is
              still unread - keeps that chip for the primary document alone, unchanged. */}
          {documentArtifact === undefined || stepHoldsPrimaryDocument ? null : (
            <ApprovalChip
              artifact={documentArtifact}
              layout="contents"
              showActions={showApprovalActions}
              variant="header"
            />
          )}
          {entries.length === 0 ? null : (
            <ApprovalSteps entries={entries} primaryArtifactId={issue.primary_artifact_id} />
          )}
          {/* Whether the route reaches anyone is state about who is working this, like the
              claim, and it sits in this wrapping row for the same reason: the metadata rail
              clips at every width, and a warning past its edge is no warning. It speaks for the
              saved route only, so a route being saved hides it until the next read judges it. */}
          {drafts.route === (issue.route ?? "") ? (
            <>
              <UnreachableRouteMarker issue={issue} showRoute />
              <UnresponsiveRouteMarker issue={issue} showRoute />
              {issue.route !== null && issue.route_status === "unknown" ? (
                issue.route_status_reason === "refilling" ? (
                  <span
                    className={`shrink-0 text-xs ${textMutedOnSurface}`}
                    data-testid="issue-route-refilling"
                    title="The Envoy listener is refilling its agent list, so Dispatch cannot tell yet whether anyone receives this route."
                  >
                    Route reach unknown: {AGENT_LIST_REFILLING}
                  </span>
                ) : (
                  <RouteReachUnknownMarker className="shrink-0" testId="issue-route-unknown" />
                )
              ) : null}
            </>
          ) : null}
          {issue.claim === null ? null : (
            <>
              {/* A claim is state — who is working this, since when — so it sits with the
                  status, priority and assignee, and Release is an action beside Close. This
                  row wraps, so both stay visible and tappable at 390px; the metadata rail
                  below never wraps (#1211) and would clip them. */}
              <ClaimChip claim={issue.claim} liveness={issue} />
              <button
                aria-label={`Release the claim on ${issue.key}`}
                className={`${issueStateButtonClassName} ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
                disabled={releaseClaim.isPending}
                onClick={() => releaseClaim.mutate()}
                title="Release this claim so another agent can take the issue"
                type="button"
              >
                {releaseClaim.isPending ? "Releasing…" : releaseClaimWords}
              </button>
            </>
          )}
          {isClosed ? null : (
            <button
              aria-label="Close issue"
              className={`${issueStateButtonClassName} ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
              disabled={statusBusy}
              onClick={() => requestStatus("done")}
              type="button"
            >
              {pendingStatus === "done"
                ? "Closing…"
                : closeIssueWords.map(({ className, words }) => (
                    <span className={className} key={words}>
                      {words}
                    </span>
                  ))}
            </button>
          )}
        </div>
        {/* The details line scrolls sideways rather than widening the page, and at a phone width
            its cut lands inside an item - the Labels control alone is wider than the rail. The
            fade says the cut is a scroll, and it is off at the end of the scroll, where a fade
            would hide the last item instead. */}
        <div
          className={`flex min-w-0 flex-nowrap items-center gap-2 overflow-x-auto [scrollbar-width:thin] md:[&_button]:min-h-7 md:[&_button]:py-0 ${railFade.fade}`}
          data-testid="issue-metadata-rail"
          ref={railFade.ref}
        >
          {phone ? priorityLine : null}
          {whoseTurn === null ? null : (
            <span
              className={`inline-flex shrink-0 items-center rounded-full border px-3 py-1 text-xs font-semibold ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
              data-testid="issue-whose-turn"
            >
              {whoseTurn}
            </span>
          )}
          {issue.created_by == null ? null : (
            <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
              <span className="font-medium">Opened by:</span>
              <span
                className="shrink-0 whitespace-nowrap"
                title={agentText.label(issue.created_by)}
              >
                <AgentName actor={issue.created_by} inline />
              </span>
            </div>
          )}
          <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
            <span className="font-medium">Labels:</span>
            <IssueLabels
              disabled={isClosed}
              labels={issue.labels ?? []}
              onSave={(labels) => labelsMutation.mutateAsync(labels)}
              project={issue.project}
              saving={labelsMutation.isPending}
              variant="rail"
            />
          </div>
          {routeEditing ? null : (
            <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
              <span aria-describedby="issue-route-hint" className="font-medium" title={routeHint}>
                Message route:
              </span>
              {routeSession === undefined ? (
                routeButton(drafts.route === "" ? "No route" : drafts.route)
              ) : (
                <AgentName
                  className="inline-flex shrink-0 items-center"
                  control={routeButton}
                  kind="control"
                  sessionId={routeSession}
                  storedTitle={undefined}
                />
              )}
            </div>
          )}
          <button
            aria-expanded={subscribersOpen}
            className={`flex min-h-11 shrink-0 items-center gap-2 rounded-lg px-2 py-1 text-sm font-medium ${textSecondaryHoverToPrimary}`}
            onClick={() => setSubscribersOpen((open) => !open)}
            type="button"
          >
            <span>{subscribersCounted ? "Subscribers:" : "Subscribers"}</span>
            {subscribersCounted ? <span>{subscriberList.length}</span> : null}
          </button>
          <ReferencedByToggle
            controls={referencesPanelId}
            count={issue.referenced_by_count}
            expanded={referencesOpen}
            onToggle={() => setReferencesOpen((open) => !open)}
          />
          {parentEditing ? null : (
            <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
              <span className="font-medium">Parent:</span>
              {issue.parent === null ? null : (
                <Link
                  className={`dispatch-inline-link shrink-0 underline ${linkText} ${linkHoverText}`}
                  to={buildIssuePath({ key: issue.parent, kind: "issue" })}
                >
                  {issue.parent}
                </Link>
              )}
              <button
                // The name starts with the word on the button - "None" or "Edit" - because a
                // name that does not contain the visible text leaves the control out of reach
                // of a voice user (WCAG 2.5.3, axe's `label-content-name-mismatch`).
                aria-label={issue.parent === null ? "None, set parent issue" : "Edit parent issue"}
                className={`inline-flex min-h-11 max-w-[14ch] shrink-0 items-center rounded-full px-3 py-2 text-left text-sm outline-none focus-visible:ring-2 xl:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
                disabled={isClosed}
                onClick={() => setParentEditing(true)}
                type="button"
              >
                {issue.parent === null ? "None" : "Edit"}
              </button>
            </div>
          )}
          {/* The repository is where the work goes, like the parent, so it sits in this rail,
              which scrolls rather than wrapping: a repository name can be long, and the state
              row above must keep every control on one row at a phone width. Every issue has an
              item here: an issue resolves to its own repository, an ancestor's, or its
              project's default repository, and any issue can take a repository of its own. */}
          <RepoControl
            disabled={isClosed || updateIssue.isPending}
            issueKey={issue.key}
            project={issue.project}
            repo={issue.repo}
            resolvedRepo={issue.resolved_repo}
          />
          <IssueComponentsLine issue={issue} />
          {(issue.external_links ?? []).map((link) => (
            <div className="flex shrink-0 items-center" key={link.url}>
              <GitHubLink link={link} />
            </div>
          ))}
        </div>
        {referencesOpen ? (
          // The reader opened this one, so its graph read goes out now rather than waiting for
          // the editor's first batch (`features/doc/first-batch.ts`).
          <ReaderIsHere value={true}>
            <ReferencedBy
              className="mt-1 w-full"
              reference={issueReference}
              toggle={{ id: referencesPanelId }}
            />
          </ReaderIsHere>
        ) : null}
      </div>
      <span className="sr-only" id="issue-route-hint">
        {routeHint}
      </span>
      {routeEditing ? (
        <IssueRouteForm
          drafts={drafts}
          isClosed={isClosed}
          onClose={() => setRouteEditing(false)}
          saving={updateIssue.isPending}
        />
      ) : null}
      {parentEditing ? (
        <IssueParentForm
          drafts={drafts}
          isClosed={isClosed}
          onClose={() => setParentEditing(false)}
          project={issue.project}
          saving={updateIssue.isPending}
        />
      ) : null}
      {drafts.titleError === null ? null : (
        <p className={`mt-1 text-sm ${dangerText}`} id="issue-title-help">
          {drafts.titleError}
        </p>
      )}
      {statusSaving ? (
        <span className={`mt-1 block text-xs ${textMutedOnSurface}`} role="status">
          {pendingStatus === "done"
            ? "Closing…"
            : pendingStatus === "backlog" && isClosed
              ? "Reopening…"
              : "Saving…"}
        </span>
      ) : null}
      <IssueStatusNotes
        issueKey={issue.key}
        noteClassName={`mt-1 text-sm ${dangerText}`}
        statusMove={statusMove}
      />
      {subscribersOpen ? (
        // The reader is in this panel, so every read inside it - each chip's agent registry
        // among them - goes out now rather than waiting for the editor's first batch
        // (`features/doc/first-batch.ts`).
        <ReaderIsHere value={true}>
          <SubscribedAgents
            onUnsubscribe={(sessionId) => unsubscribe.mutate(sessionId)}
            ownerLabel={issue.key}
            subscribers={subscriberList}
          />
        </ReaderIsHere>
      ) : null}
      {subscribers.isError ? (
        <QueryError
          error={subscribers.error}
          message="Subscribed agents unavailable — Envoy listener unreachable."
          onRetry={() => subscribers.refetch()}
          retrying={subscribers.isFetching}
        />
      ) : null}
      {updateState.isError ? (
        <QueryError
          message={
            stoppedNote(updateState, "the pin status", "saved") ?? "Could not save pin status."
          }
          onRetry={() => pinGuard.retryLast(updateState)}
          retrying={updateState.isPending}
        />
      ) : null}
      {unsubscribe.isError ? (
        <QueryError
          message={
            stoppedNote(unsubscribe, "this agent's unsubscribe", "made") ??
            "Could not unsubscribe this agent."
          }
          onRetry={unsubscribe.retry}
          retrying={unsubscribe.isPending}
        />
      ) : null}
      {releaseClaim.isError ? (
        <QueryError
          message={
            stoppedNote(releaseClaim, "the release of this claim", "made") ??
            "Could not release this claim."
          }
          onRetry={releaseClaim.retry}
          retrying={releaseClaim.isPending}
        />
      ) : null}
      {updateIssue.isError && !statusPickOnly(updateIssue.variables) ? (
        parentSaveFailed ? (
          <p className={`mt-1 text-sm ${dangerText}`} id="issue-parent-error">
            {parentError}
          </p>
        ) : titleRefusal !== null ? (
          <p className={`mt-1 text-sm ${dangerText}`} id="issue-title-error" role="alert">
            {titleRefusal}
          </p>
        ) : (
          // A title or a route save. Its Retry is the field's own submit (`drafts.retry`): a press
          // of its own, of what the field holds now, so a stopped save is never made again under
          // its key from here.
          <QueryError
            message={
              updateIssue.error instanceof SendStoppedError
                ? stoppedWriteNote("this change to the issue", "saved", {
                    anew: "Retry sends what the field holds now as a new write.",
                  })
                : "Could not update this issue."
            }
            onRetry={drafts.retry}
            retrying={updateIssue.isPending}
          />
        )
      ) : null}
    </header>
  );
}
