import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  useId,
  useLayoutEffect,
  useState,
} from "react";
import { Link } from "react-router-dom";

import {
  AGENT_LIST_REFILLING,
  api,
  apiErrorCodeIn,
  apiErrorMessage,
  issueClaimReleaseRequest,
  issuePatchRequest,
  issueStateRequest,
  issueUnsubscribeRequest,
} from "../../api/client";
import { mergeIssue } from "../../api/issue-cache";
import { issueQuery, pinnedIssuesKey, projectsQuery, userStateQuery } from "../../api/queries";
import type { Artifact, IssueDetails, UserIssueState, UserState } from "../../api/types";
import { InPlaceEditBox, type InPlaceReadProps } from "../../components/InPlaceEditBox";
import { PinButton } from "../../components/PinButton";
import { QueryError } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import {
  badgePrimary,
  bgTransparent,
  borderDefault,
  borderTransparent,
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  card,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  scrollFadeTrailing,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  statusPill,
  statusPillDot,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ApprovalChip } from "../doc/ApprovalChip";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { issueStatuses, openIssueStatuses, statusLabel } from "../project/board-model";
import { AgentName, useAgentText } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
import { ReferencedBy, ReferencedByToggle } from "../refs/ReferencedBy";
import { buildDispatchReference, buildIssuePath, sessionOfAddress } from "../refs/routes";
import { useKeymap } from "../shell/keymap";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { ApprovalSteps } from "./ApprovalSteps";
import { AssigneeControl } from "./AssigneeControl";
import { ClaimChip } from "./ClaimChip";
import { GitHubLink } from "./GitHubLink";
import { gatedStepEntries } from "./gated-steps";
import { IssueComponentsLine } from "./IssueComponentsLine";
import { IssueLabels } from "./IssueLabels";
import { issuePriority } from "./issue-priority";
import { PriorityEditor } from "./PriorityControl";
import { ProgressChips } from "./ProgressChips";
import { stateForIssue } from "./pins";
import { RepoControl } from "./RepoControl";
import {
  RouteReachUnknownMarker,
  UnreachableRouteMarker,
  UnresponsiveRouteMarker,
} from "./RouteReach";
import { SubscribedAgents } from "./SubscribedAgents";
import { type IssueUpdateInput, useIssueDrafts } from "./useIssueDrafts";
import type { IssuePriorityWrite } from "./useIssuePriority";

const routeHint =
  "New asks, comments, and messages on this issue wake this agent or role; replies inside a thread reach their participants directly. It is where messages go, not who is working the issue — that is the claim.";

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
  const [editingTitle, setEditingTitle] = useState(false);
  // The route and the parent editors open over their rail item in a panel, which takes the
  // focus into itself (`AnchoredPanel`); the item they hang off stays where it is.
  const [routeEditing, setRouteEditing] = useState(false);
  const [parentEditing, setParentEditing] = useState(false);
  // Whether the details rail has anywhere left to scroll, which is what its trailing fade says.
  // The rail element is state rather than a ref so this runs once per rail, not once per render:
  // three things change the answer and none of them is a render of this component - the reader
  // scrolling the rail, the rail's box changing with the viewport, and its content arriving (the
  // labels, the subscriber count, a GitHub title). One listener and two observers cover them,
  // and the mutation observer re-observes children a render added.
  const [rail, setRail] = useState<HTMLDivElement | null>(null);
  const [railScrollable, setRailScrollable] = useState(false);
  useLayoutEffect(() => {
    if (rail === null) {
      return;
    }
    // A fractional layout leaves scrollLeft a hair short of the end; one pixel of slack keeps a
    // rail scrolled to its end from wearing the fade.
    const measure = () =>
      setRailScrollable(rail.scrollWidth - rail.clientWidth - rail.scrollLeft > 1);
    const sizes = new ResizeObserver(measure);
    const observeAll = () => {
      sizes.observe(rail);
      for (const child of rail.children) {
        sizes.observe(child);
      }
    };
    observeAll();
    const contents = new MutationObserver(() => {
      observeAll();
      measure();
    });
    contents.observe(rail, { characterData: true, childList: true, subtree: true });
    rail.addEventListener("scroll", measure, { passive: true });
    measure();
    return () => {
      rail.removeEventListener("scroll", measure);
      sizes.disconnect();
      contents.disconnect();
    };
  }, [rail]);
  const [subscribersOpen, setSubscribersOpen] = useState(false);
  const [referencesOpen, setReferencesOpen] = useState(false);
  const agentText = useAgentText();
  const subscribers = useQuery({
    queryKey: ["subscribers", issue.key],
    queryFn: () => api.getIssueSubscribers(issue.key),
  });
  const subscriberList = subscribers.isError ? [] : (subscribers.data ?? []);
  // The count is a claim about who is subscribed, so only a read that answered makes it.
  const subscribersCounted = !subscribers.isError && subscribers.data !== undefined;
  // The unsubscribe and the claim release go in the issue's line (`issue:<key>`), as its fields
  // do, each made from its request alone; a reload keeps their order and drops them unsent, since
  // a `DELETE` carries no idempotency key and a repeat could undo what another person did
  // meanwhile (`Writes.resume`).
  const unsubscribe = useKeyedWrite({
    request: (sessionId: string) => issueUnsubscribeRequest(issue.key, sessionId),
    target: () => `issue:${issue.key}`,
    inline: true,
    label: (sessionId) =>
      `${agentText.sessionText(sessionId, subscriberList.find((row) => row.session_id === sessionId)?.title)} unsubscribed from ${issue.key}`,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["subscribers", issue.key] });
    },
  });
  const releaseClaim = useKeyedWrite({
    request: () => issueClaimReleaseRequest(issue.key),
    target: () => `issue:${issue.key}`,
    inline: true,
    label: () => `Claim on ${issue.key} released`,
    onSuccess: (next) => {
      if (next !== null) mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
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
    onSuccess: (next) => {
      if (next === null) {
        void queryClient.invalidateQueries({ queryKey: issueQuery(issue.key).queryKey });
      } else {
        mergeIssue(queryClient, next);
      }
      drafts.onIssueSuccess(next ?? issue);
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
  // Palette-only: closing and reopening an issue are too consequential for a single key, so they
  // are reachable from `$mod+k` alone, through the same handlers and the same guards as the
  // Close and Reopen buttons below. `IssuePage` pushes the `issue` scope these are offered under.
  useKeymap("issue", [
    {
      id: "close",
      keys: [],
      label: "Close issue",
      run: () => drafts.requestStatusSubmit("done"),
      // `disabled={updateIssue.isPending}` on the button, and `isClosed` decides whether it is
      // rendered at all; a row offered while the write is in flight would submit a second one.
      when: () => !isClosed && !updateIssue.isPending,
    },
    {
      id: "reopen",
      keys: [],
      label: "Reopen issue",
      run: () => drafts.requestStatusSubmit("backlog"),
      when: () => isClosed && !updateIssue.isPending,
    },
    ...([0, 1, 2, 3] as const).map((level) => ({
      id: `set-p${level}`,
      keys: [],
      label: `Set priority P${level}`,
      run: () => priorityWrite.submit(level),
      // The same condition as `PriorityEditor`'s `disabled` below.
      when: () => !(isClosed || updateIssue.isPending),
    })),
  ]);
  const statusSaving = updateIssue.isPending && updateIssue.variables?.status !== undefined;
  const pendingStatus = statusSaving ? updateIssue.variables?.status : undefined;
  const selectableStatuses = isClosed ? issueStatuses : openIssueStatuses;
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
  const approvalRequestOpen = openAsks.some((ask) => ask.kind === "approval");
  // The one shared read of the projects, under the key the shell's sidebar and the search
  // palette already hold, so the daemon's `project.updated` publish refreshes these steps with
  // no reload and the header makes no second, per-issue read. While that read is pending or
  // failed there is no row, so the steps are empty and the header draws its one chip below
  // rather than claiming a gate it has not read. Only a root issue has steps at all
  // (`gatedStepEntries`), so a child issue does not ask for the list: a disabled query still
  // reads whatever the shared cache holds, and a child draws the single chip either way.
  const projects = useQuery({ ...projectsQuery(), enabled: issue.parent === null });
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
  // The header's pin writes the same row as the Conversation's and the Activity tab's pins, so it
  // goes in that row's line (`pin:<key>`, `features/issue/pin-write.ts`): one row, one line. Its
  // change is the issue's `pinned` alone, over the row as it stands (`shows`), so the store shows
  // it again over each answer that lands there while it is live, and the store shows a pin
  // waiting behind it again over the row Dispatch answers it with (R7 of dispatch://INFRA-299).
  const updateState = useKeyedWrite({
    label: (pinned: boolean) => `${issue.key} ${pinned ? "pinned" : "unpinned"}`,
    request: (pinned: boolean) => issueStateRequest(issue.key, { pinned }),
    target: () => `pin:${issue.key}`,
    shows: (pinned) => (shown) => {
      shown.on<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issue.key]: { ...stateForIssue(current, issue.key), pinned },
      }));
    },
    onSettled: () => {
      pinGuard.release();
      void queryClient.invalidateQueries({ queryKey: userStateQuery().queryKey });
      // The sidebar lists the pinned issues from their own read, which follows the pin however
      // its write settled: with an answer, landed with none (`null`), or refused.
      void queryClient.invalidateQueries({ exact: true, queryKey: pinnedIssuesKey });
    },
    onMutate: async () => {
      await queryClient.cancelQueries({ queryKey: userStateQuery().queryKey });
      const before = queryClient.getQueryData<UserState>(userStateQuery().queryKey);
      return { pinned: stateForIssue(before, issue.key).pinned };
    },
    // A refusal puts back the issue's `pinned` as it stood before the press, and only that, over
    // the row as it stands: every other change on it is another write's.
    onError: (_error, _pinned, context) => {
      if (context === undefined) return;
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issue.key]: { ...stateForIssue(current, issue.key), pinned: context.pinned },
      }));
    },
    onSuccess: (next) => {
      if (next === null) return;
      queryClient.setQueryData<UserState>(userStateQuery().queryKey, (current) => ({
        ...current,
        [issue.key]: next,
      }));
    },
  });
  const saveTitleOnEnter = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Enter") {
      event.currentTarget.blur();
    }
  };
  const saveRoute = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestRouteSubmit()) {
      setRouteEditing(false);
    }
  };
  const saveParent = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestParentSubmit()) {
      setParentEditing(false);
    }
  };
  // One way out of either panel editor, for Cancel, for Escape and for a press outside it:
  // `AnchoredPanel` takes those two at the document's capture phase, so the discard cannot sit
  // on the field's own `onKeyDown` where the press never arrives.
  const cancelRoute = () => {
    drafts.discardRoute();
    setRouteEditing(false);
  };
  const cancelParent = () => {
    drafts.discardParent();
    setParentEditing(false);
  };
  // The save Dispatch refused, as the header shows it beside the draft it was sent with: the
  // drafts keep it, because every press is a write of its own in the issue's line and the
  // mutation's own state names the newest press rather than the one that failed. A status pick
  // is a control, so its refusal is the shell's (`PendingWritesIndicator`).
  const refusal =
    drafts.refusal === undefined || statusPickOnly(drafts.refusal.input)
      ? undefined
      : drafts.refusal;
  // A refused parent save (PARENT_INPUT, ISSUE_CLOSED) renders the server's reason inline,
  // like a route validation error, instead of the generic update failure line.
  const parentSaveFailed = refusal?.input.parent !== undefined;
  const parentError = apiErrorMessage(refusal?.error, "Could not save parent.");
  // A title Dispatch refused (CAP_EXCEEDED past ISSUE_TITLE_MAX, ISSUE_CLOSED) shows its reason
  // with no Retry, since the same title is refused every time; any other failure keeps the generic
  // line and its Retry.
  const titleRefusal =
    refusal?.input.title !== undefined && apiErrorCodeIn(refusal.error, titleRefusals)
      ? refusal.error.message
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
  // The pill's box is the shared editor's (`InPlaceEditBox`), handed in by its read slot; what
  // is left here is what names this control and what it says.
  const routeButton = (box: InPlaceReadProps) => (name: ReactNode) => (
    <button
      {...box}
      aria-label={routeLabel}
      disabled={isClosed}
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
      className={`mb-3 min-w-0 rounded-xl border p-3 ${card}`}
      data-issue-header=""
      data-testid="issue-header"
    >
      {isClosed ? (
        <div
          className={`mb-2 flex flex-wrap items-center justify-between gap-2 rounded-lg p-3 text-sm ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
        >
          <span>This issue is closed.</span>
          <button
            aria-label="Reopen issue"
            className={`min-h-11 rounded-lg px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
            disabled={updateIssue.isPending}
            onClick={() => drafts.requestStatusSubmit("backlog")}
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
              className={`rounded-md px-2.5 py-1.5 font-mono text-sm font-semibold ${badgePrimary.bg} ${badgePrimary.text}`}
            >
              {issue.key}
            </p>
            <CopyRefButton
              primary={{ label: `issue key ${issue.key}`, value: issue.key }}
              route={{ key: issue.key, kind: "issue" }}
            />
          </div>
          <div className="flex min-w-0 flex-1 items-start gap-2">
            {/* The heading and the editor share one box and one grid cell, drawn by
                `components/InPlaceEditBox`: the heading, rendered in both states over the live
                draft, is what sizes that cell, so the box is the same before, during and after
                an edit, whichever of the two is in it. A heading wrapped onto two lines giving
                way to a one-line editor (and, from 2xl where the slot is content-sized, to an
                editor of its own width) moved every control beside it between the mousedown
                that ends the edit and the mouseup, and the browser gave that click to neither.
                The rules that hold the box still — the editor out of the flow, the floor of
                twenty characters, the heading kept mounted and out of the way while the edit is
                open — are that component's, and it gives the route and the parent editors the
                same box. What stays here is this editor's own content: its draft, its save and
                its discard. */}
            <InPlaceEditBox
              box="title"
              className="flex-1"
              disabled={isClosed}
              editing={editingTitle}
              input={{
                "aria-describedby": drafts.titleError === null ? undefined : "issue-title-help",
                "aria-label": "Issue title",
                disabled: isClosed,
                maxLength: ISSUE_TITLE_MAX,
                onBlur: () => {
                  drafts.requestTitleSubmit();
                  setEditingTitle(false);
                },
                onChange: (event) => drafts.writeTitle(event.target.value),
                onKeyDown: (event) => {
                  if (event.key === "Escape") {
                    drafts.discardTitle();
                    setEditingTitle(false);
                    return;
                  }
                  saveTitleOnEnter(event);
                },
                ref: (node) => {
                  node?.focus();
                },
                value: drafts.title,
              }}
              name="title"
              onOpen={() => setEditingTitle(true)}
              overlay="cover"
              read={(box) => (
                // The header's one `h1`, which is what the title is read and measured by.
                <h1 {...box}>{drafts.title}</h1>
              )}
            />
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
            className={`flex min-h-11 shrink-0 items-center gap-2 rounded-full border px-2 text-sm font-semibold uppercase tracking-wide sm:px-3 md:min-h-8 xl:px-2 ${borderDefault} ${statusPill.bg} ${statusPill.text}`}
          >
            <span
              aria-hidden="true"
              className={`hidden h-2 w-2 rounded-full sm:block ${statusPillDot}`}
            />
            <select
              aria-label="Status"
              className={`min-w-0 appearance-none font-inherit outline-none disabled:cursor-not-allowed disabled:opacity-50 ${bgTransparent} ${statusPill.text}`}
              disabled={isClosed || updateIssue.isPending}
              onChange={(event) => drafts.requestStatusSubmit(event.target.value)}
              value={pendingStatus ?? issue.status}
            >
              {selectableStatuses.map((status) => (
                <option key={status} value={status}>
                  {statusLabel(status)}
                </option>
              ))}
            </select>
          </label>
          <PriorityEditor
            disabled={isClosed || updateIssue.isPending}
            issueKey={issue.key}
            priority={issuePriority(issue.priority)}
            write={priorityWrite}
          />
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
                className={`min-h-11 shrink-0 rounded-lg px-2 py-2 text-sm font-medium sm:px-3 md:min-h-8 md:py-1 xl:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
                disabled={releaseClaim.isPending}
                onClick={() => releaseClaim.mutate()}
                title="Release this claim so another agent can take the issue"
                type="button"
              >
                {releaseClaim.isPending ? "Releasing…" : "Release"}
              </button>
            </>
          )}
          {isClosed ? null : (
            <button
              aria-label="Close issue"
              className={`min-h-11 shrink-0 rounded-lg px-2 py-2 text-sm font-medium sm:px-3 md:min-h-8 md:py-1 xl:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
              disabled={updateIssue.isPending}
              onClick={() => drafts.requestStatusSubmit("done")}
              type="button"
            >
              {pendingStatus === "done" ? (
                "Closing…"
              ) : (
                <>
                  <span className="sm:hidden">Close</span>
                  <span className="hidden sm:inline">Close issue</span>
                </>
              )}
            </button>
          )}
        </div>
        {/* The details line scrolls sideways rather than widening the page, and at a phone width
            its cut lands inside an item - the Labels control alone is wider than the rail. The
            fade says the cut is a scroll, and it is off at the end of the scroll, where a fade
            would hide the last item instead. */}
        <div
          className={`flex min-w-0 flex-nowrap items-center gap-2 overflow-x-auto [scrollbar-width:thin] md:[&_button]:min-h-7 md:[&_button]:py-0 ${railScrollable ? scrollFadeTrailing : ""}`}
          data-testid="issue-metadata-rail"
          ref={setRail}
        >
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
          {/* The rail item stays where it is while its editor is open: the editor is a panel
              over it (`InPlaceEditBox`, `overlay="panel"`), because a label, a field, Save,
              Cancel and an error line cannot fit the pill's box. Unmounting the item and
              adding a form row under the header moved every item to its right and dropped
              everything below the header by about 52 px. */}
          <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
            <span aria-describedby="issue-route-hint" className="font-medium" title={routeHint}>
              Message route:
            </span>
            <InPlaceEditBox
              box="rail"
              disabled={isClosed}
              editing={routeEditing}
              name="route"
              onDismiss={cancelRoute}
              onOpen={() => setRouteEditing(true)}
              onSubmit={saveRoute}
              overlay="panel"
              read={(box) =>
                routeSession === undefined ? (
                  routeButton(box)(drafts.route === "" ? "No route" : drafts.route)
                ) : (
                  <AgentName
                    className="inline-flex shrink-0 items-center"
                    control={routeButton(box)}
                    kind="control"
                    sessionId={routeSession}
                    storedTitle={undefined}
                  />
                )
              }
            >
              <label
                aria-describedby="issue-route-hint"
                className="sr-only"
                htmlFor="issue-route"
                title={routeHint}
              >
                Message route:
              </label>
              <input
                aria-label="Message route"
                aria-describedby="issue-route-hint issue-route-help"
                className={`w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`}
                disabled={isClosed}
                id="issue-route"
                onChange={(event) => drafts.writeRoute(event.target.value)}
                placeholder="role:legion-controller-core"
                value={drafts.route}
              />
              <button
                className={`min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
                disabled={isClosed || !drafts.routeIsValid || updateIssue.isPending}
                type="submit"
              >
                Save route
              </button>
              <button
                className={`min-h-11 rounded border px-2 py-1 text-sm font-medium md:min-h-8 ${borderTransparent} ${textMutedHoverToSecondary}`}
                onClick={cancelRoute}
                type="button"
              >
                Cancel
              </button>
              <span
                className={drafts.routeIsValid ? "sr-only" : `text-sm ${dangerText}`}
                id="issue-route-help"
              >
                A message route must be role:[a-z0-9-]+ or session:[0-9a-f-]{`{16,}`}.
              </span>
            </InPlaceEditBox>
          </div>
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
            <InPlaceEditBox
              box="rail"
              disabled={isClosed}
              editing={parentEditing}
              name="parent"
              onDismiss={cancelParent}
              onOpen={() => setParentEditing(true)}
              onSubmit={saveParent}
              overlay="panel"
              read={(box) => (
                <button
                  {...box}
                  // The name starts with the word on the button - "None" or "Edit" - because a
                  // name that does not contain the visible text leaves the control out of reach
                  // of a voice user (WCAG 2.5.3, axe's `label-content-name-mismatch`).
                  aria-label={
                    issue.parent === null ? "None, set parent issue" : "Edit parent issue"
                  }
                  disabled={isClosed}
                  type="button"
                >
                  {issue.parent === null ? "None" : "Edit"}
                </button>
              )}
            >
              <label className="sr-only" htmlFor="issue-parent">
                Parent
              </label>
              <input
                aria-describedby="issue-parent-help"
                className={`w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`}
                disabled={isClosed}
                id="issue-parent"
                onChange={(event) => drafts.writeParent(event.target.value)}
                placeholder={`${issue.project}-12`}
                value={drafts.parent}
              />
              <button
                className={`min-h-11 rounded px-2 py-1 text-sm font-medium disabled:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
                disabled={isClosed || updateIssue.isPending}
                type="submit"
              >
                Save parent
              </button>
              <button
                className={`min-h-11 rounded border px-2 py-1 text-sm font-medium md:min-h-8 ${borderTransparent} ${textMutedHoverToSecondary}`}
                onClick={cancelParent}
                type="button"
              >
                Cancel
              </button>
              <span className="sr-only" id="issue-parent-help">
                Parent issue key in {issue.project}; leave empty to clear the parent.
              </span>
            </InPlaceEditBox>
          </div>
          {/* The repository is where the work goes, like the parent, so it sits in this rail,
              which scrolls rather than wrapping: a repository name can be long, and the state
              row above must keep every control on one row at a phone width. An issue whose
              project maps no repository has no item here. */}
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
          <ReferencedBy
            className="mt-1 w-full"
            reference={issueReference}
            toggle={{ id: referencesPanelId }}
          />
        ) : null}
      </div>
      <span className="sr-only" id="issue-route-hint">
        {routeHint}
      </span>
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
      {subscribersOpen ? (
        <SubscribedAgents
          onUnsubscribe={(sessionId) => unsubscribe.mutate(sessionId)}
          ownerLabel={issue.key}
          subscribers={subscriberList}
        />
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
          message="Could not save pin status."
          onRetry={() => pinGuard.retryLast(updateState)}
          retrying={updateState.isPending}
        />
      ) : null}
      {unsubscribe.isError ? (
        <QueryError
          message="Could not unsubscribe this agent."
          onRetry={() => unsubscribe.mutate(unsubscribe.variables as string)}
          retrying={unsubscribe.isPending}
        />
      ) : null}
      {releaseClaim.isError ? (
        <QueryError
          message="Could not release this claim."
          onRetry={() => releaseClaim.mutate()}
          retrying={releaseClaim.isPending}
        />
      ) : null}
      {refusal !== undefined ? (
        parentSaveFailed ? (
          <p className={`mt-1 text-sm ${dangerText}`} id="issue-parent-error">
            {parentError}
          </p>
        ) : titleRefusal !== null ? (
          <p className={`mt-1 text-sm ${dangerText}`} id="issue-title-error" role="alert">
            {titleRefusal}
          </p>
        ) : (
          <QueryError
            message="Could not update this issue."
            onRetry={drafts.retry}
            retrying={updateIssue.isPending}
          />
        )
      ) : null}
    </header>
  );
}
