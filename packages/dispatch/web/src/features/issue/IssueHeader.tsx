import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  useEffect,
  useId,
  useRef,
  useState,
} from "react";
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
import { projectsQuery, userStateQuery, whoAmIQuery } from "../../api/queries";
import { replaceQueryData } from "../../api/query-client";
import type { Artifact, IssueDetails, UserIssueState, UserState } from "../../api/types";
import { focusOnMount } from "../../components/focus-place";
import { heldPress } from "../../components/held-press";
import { MultiSelect } from "../../components/MultiSelect";
import { PinButton } from "../../components/PinButton";
import { QueryError } from "../../components/QueryError";
import { TruncatedText } from "../../components/TruncatedText";
import { useScrollEnds } from "../../components/useScrollEnds";
import { useReleaseWhenLate, useSubmitGuard } from "../../hooks/useSubmitGuard";
import { inputMethodKey } from "../../lib/input-method-key";
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
  hasFocusVisibleRing,
  inputClasses,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  statusPill,
  statusPillDot,
  surfaceMutedStrongBg,
  textMutedHoverToSecondary,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { TURN_ROW } from "../conversation/turn-keys";
import { ApprovalChip } from "../doc/ApprovalChip";
import { outsideAskCard } from "../inbox/ask-card";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { IssuePriorityLine } from "../priorities/IssuePriorityLine";
import {
  isIssueStatus,
  issueStatuses,
  openIssueStatuses,
  statusText,
} from "../project/board-model";
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
import { usableControl } from "./header-control";
import { IssueComponentsLine } from "./IssueComponentsLine";
import { IssueLabels } from "./IssueLabels";
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
import { SubscribedAgents } from "./SubscribedAgents";
import { STATUS_MENU, statusMarksShowing } from "./status-pick";
import { type IssueUpdateInput, useIssueDrafts } from "./useIssueDrafts";
import type { IssuePriorityWrite } from "./useIssuePriority";

const routeHint =
  "New asks, comments, and messages on this issue wake this agent or role; replies inside a thread reach their participants directly. It is where messages go, not who is working the issue — that is the claim.";

const titleRefusals = new Set(["CAP_EXCEEDED", "ISSUE_CLOSED"]);

/** A turn of the Conversation tab, or a reply inside one (`turn-keys.ts`). While the focus is on
 *  one the reader is on that turn, not on the header, so the header's Approve and Request
 *  changes keys stand down. */
const CONVERSATION_TURN = `#issue-conversation-panel :is(${TURN_ROW})`;

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

/** The control that opened one of the header's editors - Edit title, the message route's, the
 *  parent's - and what gives it the focus back, as the new issue page's title row does
 *  (`IssueTitleRow`). A key or a button inside the editor that ends the edit calls `handBack`
 *  before it ends it: the editor leaves the page with the focus it held, and once the commit
 *  that ends the edit has drawn `opener` again, the focus goes to it with no scroll. An edit the
 *  reader ended by moving the focus elsewhere - a click, Tab - calls no `handBack`, so the focus
 *  stays where they put it, and a control the header does not draw, as a closed issue draws no
 *  Edit title, takes nothing. */
function useFocusBack(editing: boolean): {
  handBack: () => void;
  opener: RefObject<HTMLButtonElement | null>;
} {
  const opener = useRef<HTMLButtonElement>(null);
  const returning = useRef(false);
  useEffect(() => {
    if (editing || !returning.current) return;
    returning.current = false;
    opener.current?.focus({ preventScroll: true });
  }, [editing]);
  return {
    handBack: () => {
      returning.current = true;
    },
    opener,
  };
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
  const titleFocus = useFocusBack(editingTitle);
  const [routeEditing, setRouteEditing] = useState(false);
  const routeFocus = useFocusBack(routeEditing);
  const routeInputRef = useRef<HTMLInputElement>(null);
  // Focus the route input once when editing opens: the trigger button unmounts, so without this
  // focus would drop to the body and a keyboard user would have to find the form again.
  useEffect(() => {
    if (routeEditing) {
      routeInputRef.current?.focus();
    }
  }, [routeEditing]);
  const [parentEditing, setParentEditing] = useState(false);
  const parentFocus = useFocusBack(parentEditing);
  const parentInputRef = useRef<HTMLInputElement>(null);
  // Same focus handoff as the route editor: the trigger button unmounts when the form opens.
  useEffect(() => {
    if (parentEditing) {
      parentInputRef.current?.focus();
    }
  }, [parentEditing]);
  // The details rail's trailing fade, worn while the rail has anywhere left to scroll.
  const railFade = useScrollEnds();
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
    readsLate: true,
    write: (_variables, bounds) => api.releaseIssueClaim(issue.key, bounds),
    inline: true,
    label: () => `Claim on ${issue.key} released`,
    onSuccess: (next) => {
      if (next !== null) mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const releasing = releaseClaim.isPending && !releaseClaim.late;
  // Every status write of the header - the status list, Close, Reopen and the palette's rows -
  // goes through the rule's question (`useIssueStatusMove`): a dry run first for an issue with a
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
    readsLate: true,
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
  // One labels write goes out at a time: the labels' control is held while one is out within its
  // deadline. Past it (`late`) the control is let go, as every hold on a write is, and a set saved
  // then goes behind the late one in the issue's line.
  const labelsMutation = useKeyedWrite({
    label: () => `Labels of ${issue.key}`,
    readsLate: true,
    target: () => `issue:${issue.key}`,
    request: (labels: string[]) => issuePatchRequest(issue.key, { labels }),
    onSuccess: (next) => {
      if (next !== null) mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  // The labels the labels' control draws and starts from: past the deadline of a labels write
  // still tried, the set it carries, so the set saved next keeps that write's change rather than
  // undo it; else the issue's.
  const labelsShown =
    (labelsMutation.late ? labelsMutation.variables : undefined) ?? issue.labels ?? [];
  const drafts = useIssueDrafts(issue, updateIssue);
  const requestStatus = (status: string) => {
    statusMove.move(status, () => drafts.requestStatusSubmit(status));
  };
  // While a field write is out within its deadline every header control that writes the issue is
  // held rather than disabled: it keeps the focus the press gave it (`heldPress`). Past the
  // deadline (`KeyedWriteResult.late`) it holds none of them, and a status pressed then is a write
  // of its own, which waits behind it in the issue's line (`requestStatusSubmit`). The route and
  // parent forms' submits need no guard of their own: `useIssueDrafts` keeps a draft submitted
  // meanwhile and sends it once the writes out have landed, never beside one.
  const fieldHeld = updateIssue.isPending && !updateIssue.late;
  // A write of the header's fields (its status, title, route or parent) is out within its
  // deadline, or the rule's question is checking or asking about a status write: no second status
  // write can start. The check is a dry run held to the request deadline (`dryRunStatusChange`),
  // so it holds no longer than the write does.
  const statusBusy = fieldHeld || statusMove.pending !== undefined;
  // No default key: closing and reopening an issue are too consequential for a single key, so
  // they are rows of `$mod+k` until a person gives them a key, through the same handlers and the
  // same guards as the Close and Reopen buttons below. The four Set priority rows have none
  // either: the page's digits `0`–`3` set the priority (`issue.set-priority`), which is no row,
  // since each digit writes its own priority. `IssuePage` pushes the `issue` scope these are
  // offered under.
  useKeymap("issue", [
    {
      action: "issue.close",
      run: () => requestStatus("done"),
      // The button is held while `statusBusy`, and `isClosed` decides whether it is rendered at
      // all; a row offered while a status write is in flight would submit a second one.
      when: () => !isClosed && !statusBusy,
    },
    {
      action: "issue.reopen",
      run: () => requestStatus("backlog"),
      when: () => isClosed && !statusBusy,
    },
    ...([0, 1, 2, 3] as const).map((level) => ({
      action: `issue.set-p${level}` as const,
      run: () => priorityWrite.submit(level),
      // The same condition as `PriorityEditor`'s `disabled` and `held` below.
      when: () => !(isClosed || fieldHeld),
    })),
  ]);
  // The status the status list and the closed line show while it is out: the one the rule's
  // question is checking or asking about, else the one the write is saving.
  const checkingStatus = statusMove.pending;
  const statusSaving =
    checkingStatus !== undefined ||
    (updateIssue.isPending && updateIssue.variables?.status !== undefined);
  const pendingStatus =
    checkingStatus ?? (statusSaving ? updateIssue.variables?.status : undefined);
  // What the status list's trigger shows: the status a pick is checking or saving, else the
  // issue's.
  const statusValue = pendingStatus ?? issue.status;
  const statusShown = statusText(statusValue);
  const selectableStatuses = isClosed ? issueStatuses : openIssueStatuses;
  const [statusOpen, setStatusOpen] = useState(false);
  // A closed issue's Status takes no pick, and neither does it while a write of the header's
  // fields (its status, title, route or parent) is out within its deadline or the rule's question
  // is checking or asking about a status write (`statusBusy`). Either way it is held
  // (`aria-disabled`), never disabled: it keeps the focus the reader's pick handed back to it, and
  // a press of it or `s` opens nothing.
  const statusHeld = isClosed || statusBusy;
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
  // `a` and Shift+R press the header's Approve and Request changes (R7) while the primary
  // document's own approval request waits on the reader: its `ask_id` names an open ask of this
  // issue whose turn is a person's, and that person is the reader. `showApprovalActions` is not
  // that test: it draws Request changes on an approved spec while only another document of the
  // issue waits, whose request the reader reaches on the Artifacts tab. Inside an ask card the
  // card's controls own the keys, and on a turn of the Conversation tab the reader is on that
  // turn; a text field keeps both keys already, since neither fires while one has the focus.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  const approvalAskId = documentArtifact?.approval?.ask_id;
  const approvalWaitsOnReader =
    viewer !== undefined &&
    waitingForHuman.some((ask) => ask.id === approvalAskId && ask.waiting_on_person === viewer);
  /** The header's Approve or Request changes, while the key that presses it is the reader's:
   *  drawn, taking a press, and the request waiting on them. */
  const approvalControl = (action: "approve" | "request-changes"): HTMLElement | null =>
    approvalWaitsOnReader &&
    outsideAskCard() &&
    document.activeElement?.closest(CONVERSATION_TURN) == null
      ? usableControl(`[data-approval-action="${action}"]`)
      : null;
  useKeymap("issue", [
    {
      // Approve stays held from the press until the page reads the approval again
      // (`ApprovalChip`), so a second press meanwhile presses nothing.
      action: "issue.approve",
      run: () => approvalControl("approve")?.click(),
      when: () => approvalControl("approve") !== null,
    },
    {
      // Request changes opens its box with the focus in the reason, where Ctrl or Cmd+Enter
      // sends and Escape closes it sending nothing (`ApprovalChip`).
      action: "issue.request-changes",
      run: () => approvalControl("request-changes")?.click(),
      when: () => approvalControl("request-changes") !== null,
    },
  ]);
  const pinGuard = useSubmitGuard();
  const updateState = useKeyedWrite({
    // A read of the state asked while the pin is out could be answered before Dispatch takes it,
    // with the pin as it was: it waits for the answer, for at most `SEND_DEADLINE_MS`. The state
    // is the viewer's alone, so the wait keeps back nobody else's change.
    holds: () => ({ keys: [userStateQuery().queryKey] }),
    label: (pinned: boolean) => `${issue.key} ${pinned ? "pinned" : "unpinned"}`,
    method: "PUT",
    readsLate: true,
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
  useReleaseWhenLate(pinGuard, updateState.late);
  // Enter ends the edit as the editor's blur does, and gives the focus back to Edit title. The
  // press is marked taken: the browser sends the key's keypress to the element focused by then,
  // and on Edit title that keypress would press it and open the editor again.
  const saveTitleOnEnter = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Enter") {
      event.preventDefault();
      titleFocus.handBack();
      event.currentTarget.blur();
    }
  };
  // The route's and the parent's editors end by a save - Enter or the Save button - by Escape and
  // by Cancel, each a key or a button inside the editor: each closes it with the focus back on the
  // control that opened it (`useFocusBack`). Escape and Cancel drop the draft.
  const saveRoute = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestRouteSubmit()) {
      routeFocus.handBack();
      setRouteEditing(false);
    }
  };
  const cancelRoute = () => {
    routeFocus.handBack();
    drafts.discardRoute();
    setRouteEditing(false);
  };
  const saveParent = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (drafts.requestParentSubmit()) {
      parentFocus.handBack();
      setParentEditing(false);
    }
  };
  const cancelParent = () => {
    parentFocus.handBack();
    drafts.discardParent();
    setParentEditing(false);
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
      ref={routeFocus.opener}
      title={drafts.route === "" ? undefined : drafts.route}
      type="button"
    >
      <TruncatedText>{name}</TruncatedText>
    </button>
  );
  const issueReference = buildDispatchReference({ key: issue.key, kind: "issue" });
  const referencesPanelId = useId();
  const titleId = useId();
  const externalLinks = issue.external_links ?? [];
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
            // Held, not disabled, while a write of the header's fields within its deadline or the
            // rule's question is out (`statusBusy`): the pressed Reopen keeps the keyboard focus,
            // and a second press sends nothing.
            {...heldPress(statusBusy, () => requestStatus("backlog"))}
            title="Reopen into Backlog"
            type="button"
          >
            {statusBusy && pendingStatus === "backlog" ? "Reopening…" : "Reopen"}
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
            {editingTitle ? (
              <input
                aria-describedby={drafts.titleError === null ? undefined : "issue-title-help"}
                aria-label="Issue title"
                className={`min-w-0 flex-1 rounded-lg border px-2 py-1 text-[22px] leading-7 font-semibold tracking-tight outline-none md:py-0 ${borderTransparent} ${bgTransparent} ${textPrimaryOnSurface} ${borderStrongHover} ${focusBorder}`}
                disabled={isClosed}
                maxLength={ISSUE_TITLE_MAX}
                onBlur={() => {
                  drafts.requestTitleSubmit();
                  setEditingTitle(false);
                }}
                onChange={(event) => drafts.writeTitle(event.target.value)}
                onKeyDown={(event) => {
                  // A key that ends an input method's composition - the Enter that takes the word
                  // composed, the Escape that drops it - is no key of the reader's: it neither
                  // writes the title nor closes its editor. Safari sends it after its
                  // compositionend, with `isComposing` false and keyCode 229 (`inputMethodKey`).
                  if (inputMethodKey(event.nativeEvent)) return;
                  if (event.key === "Escape") {
                    // The press is the editor's: marked taken, so the key registry passes over it
                    // and a held Escape's repeats are kept from the page (`shell/keymap.ts`). The
                    // focus goes back to Edit title (`useFocusBack`).
                    event.preventDefault();
                    titleFocus.handBack();
                    drafts.discardTitle();
                    setEditingTitle(false);
                    return;
                  }
                  saveTitleOnEnter(event);
                }}
                ref={focusOnMount}
                value={drafts.title}
              />
            ) : (
              // The heading holds the title and takes no Tab stop and no press. The Edit title
              // button laid over the heading's box is the control: a screen reader reads it as
              // "Edit title, button", with the title as its description. The button shows no text
              // of its own, since a button that showed the title under the name "Edit title" would
              // leave its visible words out of its name (WCAG 2.5.3, Label in Name). It stands
              // beside the heading and not inside it, since a heading's name is computed from
              // what it holds, a button's `aria-label` included. It opens the editor when the
              // reader activates it - a click, Enter or Space - and never as it takes the focus:
              // an editor that opens on focus changes the page on focus (WCAG 2.2, 3.2.1 On
              // Focus), and a focus can come to the button with no act of the reader's. Escape and
              // Enter in the editor give the focus back to it (`useFocusBack`). A closed issue's
              // title edits nothing, so it is the heading alone.
              <div className="relative min-w-0 flex-1">
                <h1
                  className={`break-words rounded-lg border px-2 py-1 text-[22px] leading-7 font-semibold tracking-tight md:py-0 ${borderTransparent} ${textPrimaryOnSurface}`}
                  id={titleId}
                >
                  {drafts.title}
                </h1>
                {isClosed ? null : (
                  <button
                    aria-describedby={titleId}
                    aria-label="Edit title"
                    className={`absolute inset-0 cursor-text rounded-lg border outline-none focus-visible:ring-2 ${borderTransparent} ${borderStrongHover} ${focusVisibleRing}`}
                    onClick={() => setEditingTitle(true)}
                    ref={titleFocus.opener}
                    type="button"
                  />
                )}
              </div>
            )}
            <PinButton
              held={updateState.isPending && !updateState.late}
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
          {/* The Status is the shared list's single pick, whose text box narrows the statuses
              as the reader types. Its trigger fills the pill, which draws the keyboard focus
              ring round it as `badge-select.ts`'s wrappers do. On an open issue it lists every
              open status, since Close issue is an open issue's one way into Done; on a closed
              issue every status, its own Done among them, and it takes no pick, since its way
              back is Reopen. A pick goes through the rule's question (`requestStatus`), then
              writes through `updateIssueGuard` (`requestStatusSubmit`), and a pick of the status
              it shows writes nothing, as a select set to the value it holds fires nothing. Held
              on a closed issue and while a write of the header's fields within its deadline or
              the rule's question is out (`statusHeld`), so the focus a status pick hands back
              stays where it went, and a list that stood open as the hold began picks nothing. It
              takes D5's rules (`STATUS_MENU`): the issue page's `s` opens it on the status held,
              and every close gives the focus back where it was as the list opened. */}
          <span
            className={`flex min-h-11 shrink-0 items-center rounded-full border text-sm font-semibold uppercase tracking-wide md:min-h-8 has-focus-visible:ring-2 ${hasFocusVisibleRing} ${borderDefault} ${statusPill.bg} ${statusPill.text}`}
          >
            <MultiSelect
              ariaDisabled={statusHeld}
              chevron={false}
              emptyMessage="No status matches that."
              label="Status"
              marks={statusMarksShowing(statusValue)}
              menu={STATUS_MENU}
              mode="single"
              onOpenChange={(next) => setStatusOpen(next && !statusHeld)}
              onPick={(picked) => {
                if (statusHeld || picked === statusValue || !isIssueStatus(picked)) return;
                requestStatus(picked);
              }}
              open={statusOpen}
              optionLabel={statusText}
              options={selectableStatuses}
              searchLabel="Search statuses"
              triggerClassName="flex cursor-pointer items-center self-stretch rounded-full px-2 outline-none sm:px-3 xl:px-2 refused:cursor-not-allowed refused:opacity-50"
              value={statusValue}
            >
              <span className="flex items-center gap-2">
                <span
                  aria-hidden="true"
                  className={`hidden h-2 w-2 rounded-full sm:block ${statusPillDot}`}
                />
                {statusShown}
              </span>
            </MultiSelect>
          </span>
          <PriorityEditor
            disabled={isClosed}
            held={fieldHeld}
            issueKey={issue.key}
            write={priorityWrite}
          />
          {phone ? null : priorityLine}
          <AssigneeControl
            assignee={issue.assignee}
            disabled={isClosed}
            held={fieldHeld}
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
                {...heldPress(releasing, () => releaseClaim.mutate())}
                title="Release this claim so another agent can take the issue"
                type="button"
              >
                {releasing ? "Releasing…" : "Release"}
              </button>
            </>
          )}
          {isClosed ? null : (
            <button
              aria-label="Close issue"
              className={`min-h-11 shrink-0 rounded-lg px-2 py-2 text-sm font-medium sm:px-3 md:min-h-8 md:py-1 xl:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
              // Held, not disabled, while a write of the header's fields within its deadline or
              // the rule's question is out (`statusBusy`): the pressed Close keeps the keyboard
              // focus, and a second press sends nothing.
              {...heldPress(statusBusy, () => requestStatus("done"))}
              type="button"
            >
              {statusBusy && pendingStatus === "done" ? (
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
            would hide the last item instead. No Tab stop of its own, which Firefox gives a box
            that scrolls, with no name: its controls are the stops that bring each part of the
            line into view, and the arrow keys scroll it from any of them. */}
        <div
          className={`flex min-w-0 flex-nowrap items-center gap-2 overflow-x-auto [scrollbar-width:thin] md:[&_button]:min-h-7 md:[&_button]:py-0 ${railFade.fade}`}
          data-testid="issue-metadata-rail"
          ref={railFade.ref}
          tabIndex={-1}
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
              labels={labelsShown}
              onSave={(labels) => labelsMutation.mutateAsync(labels)}
              project={issue.project}
              saving={labelsMutation.isPending && !labelsMutation.late}
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
                ref={parentFocus.opener}
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
            disabled={isClosed}
            held={fieldHeld}
            issueKey={issue.key}
            project={issue.project}
            repo={issue.repo}
            resolvedRepo={issue.resolved_repo}
          />
          <IssueComponentsLine issue={issue} />
          {externalLinks.length === 0 ? null : (
            // The issue's external links as a list, a link to an item. The list keeps the rail's
            // gap between its items, so each link stands where it would as an item of the rail,
            // and it is drawn only while there is a link, so it adds no gap at the rail's end.
            <ul aria-label="External links" className="flex shrink-0 items-center gap-2">
              {externalLinks.map((link) => (
                <li className="flex shrink-0 items-center" key={link.url}>
                  <GitHubLink link={link} />
                </li>
              ))}
            </ul>
          )}
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
      {routeEditing ? (
        <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={saveRoute}>
          <label
            aria-describedby="issue-route-hint"
            className={`text-sm font-medium ${textSecondaryOnSurface}`}
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
            onKeyDown={(event) => {
              // An Escape that ends an input method's composition is the composition's
              // (`inputMethodKey`). Any other is the editor's, and marked taken
              // (`shell/keymap.ts`).
              if (event.key === "Escape" && !inputMethodKey(event.nativeEvent)) {
                event.preventDefault();
                cancelRoute();
              }
            }}
            placeholder="role:legion-controller-core"
            ref={routeInputRef}
            value={drafts.route}
          />
          <button
            aria-disabled={fieldHeld || undefined}
            className={`min-h-11 rounded px-2 py-1 text-sm font-medium refused:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
            disabled={isClosed || !drafts.routeIsValid}
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
        </form>
      ) : null}
      {parentEditing ? (
        <form className="mt-2 flex flex-wrap items-center gap-2" onSubmit={saveParent}>
          <label className="sr-only" htmlFor="issue-parent">
            Parent
          </label>
          <span aria-hidden="true" className={`text-sm font-medium ${textSecondaryOnSurface}`}>
            Parent:
          </span>
          <input
            aria-describedby="issue-parent-help"
            className={`w-full rounded px-2 py-1 text-sm outline-none md:w-64 ${inputClasses(true)}`}
            disabled={isClosed}
            id="issue-parent"
            onChange={(event) => drafts.writeParent(event.target.value)}
            onKeyDown={(event) => {
              // An Escape that ends an input method's composition is the composition's
              // (`inputMethodKey`). Any other is the editor's, and marked taken
              // (`shell/keymap.ts`).
              if (event.key === "Escape" && !inputMethodKey(event.nativeEvent)) {
                event.preventDefault();
                cancelParent();
              }
            }}
            placeholder={`${issue.project}-12`}
            ref={parentInputRef}
            value={drafts.parent}
          />
          <button
            aria-disabled={fieldHeld || undefined}
            className={`min-h-11 rounded px-2 py-1 text-sm font-medium refused:cursor-not-allowed md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonDisabledText}`}
            disabled={isClosed}
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
            Parent issue key, in {issue.project} or another project; leave empty to clear the
            parent.
          </span>
        </form>
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
            retrying={fieldHeld}
          />
        )
      ) : null}
    </header>
  );
}
