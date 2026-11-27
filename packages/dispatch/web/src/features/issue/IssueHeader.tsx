import { ISSUE_TITLE_MAX } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type KeyboardEvent, type ReactNode, useId, useRef, useState } from "react";

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
import type {
  Artifact,
  ExternalLink,
  IssueDetails,
  UserIssueState,
  UserState,
} from "../../api/types";
import { CappedGroup } from "../../components/CappedGroup";
import { focusableIn, focusOnMount } from "../../components/focus-place";
import { heldPress } from "../../components/held-press";
import { MultiSelect } from "../../components/MultiSelect";
import { PinButton } from "../../components/PinButton";
import { QueryError } from "../../components/QueryError";
import { useFocusBack } from "../../hooks/useFocusBack";
import { useFocusHandOff } from "../../hooks/useFocusHandOff";
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
import { ReferencedBy } from "../refs/ReferencedBy";
import { buildDispatchReference } from "../refs/routes";
import { useKeymap } from "../shell/keymap";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { ApprovalSteps } from "./ApprovalSteps";
import { AssigneeControl } from "./AssigneeControl";
import { ClaimChip } from "./ClaimChip";
import { GitHubLink, isPullRequestLink } from "./GitHubLink";
import { gatedStepEntries, isProjectRoot } from "./gated-steps";
import { HeaderDetails, type HeaderDetailsRow } from "./HeaderDetails";
import { usableControl } from "./header-control";
import { IssueComponentsLine } from "./IssueComponentsLine";
import { IssueParentDetails, IssueRouteDetails } from "./IssueHeaderForms";
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

const headerGroup = "flex min-w-0 max-w-full flex-wrap items-center gap-2";

/**
 * Today's issue header. It draws its elements in these groups, in this order, each element in one
 * group; when the header is too narrow a group moves to the next line whole (`headerGroup`), and
 * nothing in it scrolls sideways:
 *
 * 1. Identity: the issue's key and its copy control, the title, and the pin. A closed issue reads
 *    "This issue is closed." above it, with Reopen.
 * 2. State: the status, the priority, the assignee, the progress of its tasks and children, and the
 *    approval chip of the primary document or the gate's steps.
 * 3. Attention: whose turn it is (waiting on you, or on agents), a route that reaches nobody
 *    (unreachable, unresponsive, or unknown and why), and the claim with Release beside it.
 * 4. Labels: the labels, at most 10 and then "+N", and the control that edits them, at the left
 *    of the header's last line.
 * 5. Actions: Close issue, at the right end of that line.
 * 6. Details: one disclosure (`HeaderDetails`) beside Actions, whose panel holds every other
 *    property as a name and a value, each with the control that edits it.
 */
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
  // Whether the route's and the parent's editors in Details are open: the header holds them,
  // since Details closing closes them. The Advances picker saves its draft as it closes, which a
  // press outside it does, so it needs no such hold.
  const [routeEditing, setRouteEditing] = useState(false);
  const [parentEditing, setParentEditing] = useState(false);
  /** The route a save landed with when Dispatch kept no answer for it, and the issue's sequence
   *  then, until the issue is read again (`onSuccess`): the route's warnings would otherwise
   *  judge the route the issue held before the save. */
  const [routeUnread, setRouteUnread] = useState<{ route: string; seq: number } | null>(null);
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
  // The reader's press of Release, or of Retry after a refused release, noted as it sends the
  // release and forgotten as that release is refused, or as the claim's box leaves the page
  // (`claimLeft`).
  const released = useRef(false);
  const releaseClaim = useKeyedWrite({
    // A release made again after one that landed needs no `removedEvenIfGone`: Dispatch answers a
    // release of an issue nobody holds with the issue, as it answers the first.
    method: "DELETE",
    readsLate: true,
    write: (_variables, bounds) => api.releaseIssueClaim(issue.key, bounds),
    inline: true,
    label: () => `Claim on ${issue.key} released`,
    // The write's own callback, not the press's: Retry sends the release again with none of its
    // own (`retry`).
    onError: () => {
      released.current = false;
    },
    onSuccess: (next) => {
      if (next !== null) mergeIssue(queryClient, next);
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
  });
  const releasing = releaseClaim.isPending && !releaseClaim.late;
  // Release is drawn beside the claim only while the issue has one, so the release its press
  // makes takes the claim's box off the page as it lands, with the focus Release holds. The focus
  // goes on, with no scroll, to the control now at Release's place in the header's order
  // (`releasePlace`): the first after the State and Attention groups (`stateActions`), the first
  // of the header's last line - the labels' own, or Details where they take no press
  // (`useFocusHandOff`). Only a release of the reader's own hands it on (`released`, which a press
  // of Release or of Retry sets and the release's refusal clears): the box taken away by a release
  // made elsewhere leaves the focus on the page's body, as the new issue page's claim does
  // (`IssueClaimControl`). A refused release draws its line with Retry, and Retry's press takes the
  // line away, with the focus Retry holds, as it sends the release again: the focus goes on to
  // Release, held beside the claim while that release is out, and from there to Release's place
  // once it lands, as after a first release; or to Release's place at once where the release has
  // already taken the claim's box (`retryLeft`, which Retry's press arms, `retried`).
  const headerElement = useRef<HTMLElement>(null);
  const stateActions = useRef<HTMLDivElement>(null);
  const releaseControl = useRef<HTMLButtonElement>(null);
  const releasePlace = () => {
    const groups = stateActions.current;
    if (groups === null || headerElement.current === null) return null;
    return focusableIn(headerElement.current).find(
      (control) =>
        !groups.contains(control) &&
        (groups.compareDocumentPosition(control) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0
    );
  };
  const claimLeft = useFocusHandOff(releasePlace, released);
  const retried = useRef(false);
  const retryLeft = useFocusHandOff(() => releaseControl.current ?? releasePlace(), retried);
  const pressRelease = () => {
    released.current = true;
    releaseClaim.mutate();
  };
  const retryRelease = () => {
    retried.current = true;
    released.current = true;
    releaseClaim.retry();
  };
  // Every status write of the header - the status list, Close, Reopen and the palette's rows -
  // goes through the rule's question (`useIssueStatusMove`): a dry run first for an issue with a
  // parent or an open child, then the write below, or the question where Dispatch's rule would
  // refuse it. One that ends with nothing written takes back the focus that a press of Close issue
  // or Reopen asked for (`closeFocus`, `reopenFocus`, below).
  const statusMove = useIssueStatusMove(issue, () => {
    closeFocus.letGo();
    reopenFocus.letGo();
  });
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
        if (input.route !== undefined) {
          setRouteUnread({ route: input.route ?? "", seq: issue.last_seq });
        }
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
  // Close issue is drawn only while the issue is open and Reopen only while it is closed, so the
  // status write each one asks for takes it off the page as it lands, with the focus it holds.
  // Each press asks for the focus back (`useFocusBack`), and the commit that draws the other one
  // in its place gives it the focus with no scroll, from the page's body alone, where the going
  // left it: Reopen after a close (`closeFocus`), Close issue after a reopen (`reopenFocus`). The
  // command bar's rows of the two ask the same: the bar runs one once it has given the focus
  // back, to the page's body where nothing held it as the bar opened, and the focus goes on from
  // there. Where the rule's question asks first, its dialog holds the focus, and the page can draw
  // the issue closed or reopened before the dialog goes - its read of the change Dispatch made can
  // answer before the chosen write does - so the ask waits for the dialog too (`asking`): the
  // dialog gives the focus back to the control pressed, or to the page's body where that control
  // has gone, and the focus goes on from there. A status write refused, or a question closed
  // unanswered, takes the ask back (`letGo`): the issue closed or reopened later in another
  // session, which the page draws as it reads it, moves no focus, as a claim released elsewhere
  // moves none (`released`).
  const asking = statusMove.question !== undefined;
  const closeFocus = useFocusBack(!isClosed || asking);
  const reopenFocus = useFocusBack(isClosed || asking);
  const closeIssue = () => {
    closeFocus.handBack();
    requestStatus("done");
  };
  const reopenIssue = () => {
    reopenFocus.handBack();
    requestStatus("backlog");
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
      run: closeIssue,
      // The button is held while `statusBusy`, and `isClosed` decides whether it is rendered at
      // all; a row offered while a status write is in flight would submit a second one.
      when: () => !isClosed && !statusBusy,
    },
    {
      action: "issue.reopen",
      run: reopenIssue,
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
  const routeSaving = updateIssue.isPending && updateIssue.variables?.route !== undefined;
  // A route saved with no answer stands for the issue until the issue is read again: until then
  // the issue's own route, and the reach Dispatch judged for it, are the route before the save.
  const routeAwaitsRead =
    routeUnread !== null &&
    routeUnread.seq === issue.last_seq &&
    routeUnread.route !== (issue.route ?? "");
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
  // The one way each editor of Details gives up an edit: its own Cancel and Escape, and Details
  // closing over it. Its own Cancel and Escape also give the focus back to the control that
  // opened it (`IssueHeaderForms.tsx`); Details closing leaves the focus on Details.
  const cancelRouteEdit = () => {
    drafts.discardRoute();
    setRouteEditing(false);
  };
  const cancelParentEdit = () => {
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
  const issueReference = buildDispatchReference({ key: issue.key, kind: "issue" });
  const referencesPanelId = useId();
  const titleId = useId();
  const pullRequests: ExternalLink[] = [];
  const otherLinks: ExternalLink[] = [];
  for (const link of issue.external_links ?? []) {
    (isPullRequestLink(link) ? pullRequests : otherLinks).push(link);
  }
  const discardUnsavedDetailsEdits = () => {
    if (routeEditing) cancelRouteEdit();
    if (parentEditing) cancelParentEdit();
  };
  const detailsRows: HeaderDetailsRow[] = [
    {
      // The priorities the issue advances, each the chip of its place in the order, and the
      // picker's "+". Advances is one of the properties Details holds; the State group holds the
      // issue's own status and priority, not the priorities it advances (dispatch://INFRA-5200,
      // which moves them from the state row and the old details row #4316 placed them in).
      id: "advances",
      label: "Advances",
      value: <IssuePriorityLine disabled={isClosed} issue={issue} />,
    },
    ...(issue.created_by == null
      ? []
      : [
          {
            id: "opened-by",
            label: "Opened by",
            value: (
              <span title={agentText.label(issue.created_by)}>
                <AgentName actor={issue.created_by} inline />
              </span>
            ),
          },
        ]),
    {
      id: "route",
      label: "Message route",
      value: (
        <IssueRouteDetails
          drafts={drafts}
          editing={routeEditing}
          held={fieldHeld}
          isClosed={isClosed}
          onCancel={cancelRouteEdit}
          onEditingChange={setRouteEditing}
        />
      ),
    },
    {
      id: "subscribers",
      label: "Subscribers",
      value: (
        <button
          aria-expanded={subscribersOpen}
          aria-label={subscribersCounted ? `Subscribers: ${subscriberList.length}` : "Subscribers"}
          className={`inline-flex min-h-11 items-center rounded-full px-3 py-2 text-sm font-medium outline-none focus-visible:ring-2 md:min-h-8 md:py-1 xl:px-2 ${surfaceMutedStrongBg} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
          onClick={() => setSubscribersOpen((open) => !open)}
          type="button"
        >
          {subscribersCounted ? subscriberList.length : "Subscribers"}
        </button>
      ),
    },
    {
      id: "referenced-by",
      label: "Referenced by",
      value:
        issue.referenced_by_count === 0 ? (
          <span className={textMutedOnSurface}>0</span>
        ) : (
          <button
            aria-controls={referencesPanelId}
            aria-expanded={referencesOpen}
            aria-label={`Referenced by (${issue.referenced_by_count})`}
            className={`inline-flex min-h-11 items-center rounded-full px-3 py-2 text-sm font-medium outline-none focus-visible:ring-2 md:min-h-8 md:py-1 xl:px-2 ${surfaceMutedStrongBg} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
            onClick={() => setReferencesOpen((open) => !open)}
            type="button"
          >
            {issue.referenced_by_count}
          </button>
        ),
    },
    {
      id: "parent",
      label: "Parent",
      value: (
        <IssueParentDetails
          drafts={drafts}
          editing={parentEditing}
          held={fieldHeld}
          isClosed={isClosed}
          onCancel={cancelParentEdit}
          onEditingChange={setParentEditing}
          parent={issue.parent}
          project={issue.project}
        />
      ),
    },
    {
      id: "repository",
      label: "Repository",
      value: (
        <RepoControl
          disabled={isClosed}
          held={fieldHeld}
          issueKey={issue.key}
          project={issue.project}
          repo={issue.repo}
          resolvedRepo={issue.resolved_repo}
        />
      ),
    },
    {
      id: "components",
      label: "Components",
      value: <IssueComponentsLine issue={issue} />,
    },
    ...[
      { id: "pull-requests", label: "Pull requests", links: pullRequests },
      { id: "links", label: "Links", links: otherLinks },
    ]
      .filter((row) => row.links.length > 0)
      .map((row) => ({
        id: row.id,
        label: row.label,
        value: (
          <CappedGroup
            className="flex min-w-0 flex-col items-start gap-1"
            label={row.label.toLowerCase()}
            name={row.id}
          >
            {row.links.map((link) => (
              <span className="flex min-w-0 max-w-full items-center" key={link.url}>
                <GitHubLink link={link} />
              </span>
            ))}
          </CappedGroup>
        ),
      })),
  ];
  // The title slot has one flex-basis whether it shows the heading or the editor: below 2xl the
  // title always takes its own row (basis-full) and the State and Attention groups share the row
  // beneath it; from 2xl the slot is content-sized (basis-auto) so they join the title's row when
  // they fit. A basis that changed between view and edit mode would move every other control on
  // the blur a mousedown causes, and Chromium would deliver the mouseup — and so the click — to a
  // different element, swallowing the first click after a title edit.

  return (
    // `data-issue-header` is load-bearing for the keyboard: the `issue` scope's bindings find
    // the Status, priority, labels, title and pin controls through it, and scoping to it is
    // what keeps a child issue's row in the Children tab, which carries the same accessible
    // names, out of their reach.
    <header
      className={`mb-3 min-w-0 rounded-xl border p-3 ${card}`}
      data-issue-header=""
      data-testid="issue-header"
      ref={headerElement}
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
            // and a second press sends nothing. A close hands the focus on to it (`closeFocus`).
            {...heldPress(statusBusy, reopenIssue)}
            ref={closeFocus.opener}
            title="Reopen into Backlog"
            type="button"
          >
            {statusBusy && pendingStatus === "backlog" ? "Reopening…" : "Reopen"}
          </button>
        </div>
      ) : null}
      <div className="flex flex-wrap items-center gap-2">
        <div
          className="flex min-w-0 grow basis-full flex-col gap-2 sm:flex-row sm:items-start 2xl:basis-auto"
          data-testid="issue-identity"
        >
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
                // The heading's 22 px type at every width, so the title is written in the size it
                // is read in. Below 1280 px `styles.css` sets every input's type to 16 px
                // (`font-size: 1rem`, the floor that keeps a phone from zooming in), in no layer,
                // over `text-[22px]`: a set size, not a minimum. `data-own-type` takes it off this
                // box.
                data-own-type=""
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
                    // (`shell/keymap.ts`). The focus goes back to Edit title (`useFocusBack`).
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
          className="flex w-full min-w-0 flex-wrap items-center gap-x-4 gap-y-2 md:w-auto"
          data-testid="issue-state-actions"
          ref={stateActions}
        >
          <div className={headerGroup} data-testid="issue-state">
            {/* The Status is the shared list's single pick, whose text box narrows the statuses
                as the reader types. Its trigger fills the pill, which draws the keyboard focus
                ring round it as `badge-select.ts`'s wrappers do. On an open issue it lists every
                open status, since Close issue is an open issue's one way into Done; on a closed
                issue every status, its own Done among them, and it takes no pick, since its way
                back is Reopen. A pick goes through the rule's question (`requestStatus`), then
                writes through `updateIssueGuard` (`requestStatusSubmit`), and a pick of the
                status it shows writes nothing, as a select set to the value it holds fires
                nothing. Held on a closed issue and while a write of the header's fields within
                its deadline or the rule's question is out (`statusHeld`), so the focus a status
                pick hands back stays where it went, and a list that stood open as the hold began
                picks nothing. It takes D5's rules (`STATUS_MENU`): the issue page's `s` opens it
                on the status held, and every close gives the focus back where it was as the list
                opened. */}
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
            <AssigneeControl
              assignee={issue.assignee}
              disabled={isClosed}
              held={fieldHeld}
              issueKey={issue.key}
            />
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
          </div>
          <div className={`${headerGroup} empty:hidden`} data-testid="issue-attention">
            {whoseTurn === null ? null : (
              <span
                className={`inline-flex shrink-0 items-center rounded-full border px-3 py-1 text-xs font-semibold ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
                data-testid="issue-whose-turn"
              >
                {whoseTurn}
              </span>
            )}
            {!routeSaving && !routeAwaitsRead ? (
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
              <span
                className="flex min-w-0 max-w-full grow basis-64 items-center gap-2"
                ref={claimLeft}
              >
                <ClaimChip claim={issue.claim} liveness={issue} />
                <button
                  aria-label={`Release the claim on ${issue.key}`}
                  className={`min-h-11 shrink-0 rounded-lg px-2 py-2 text-sm font-medium sm:px-3 md:min-h-8 md:py-1 xl:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
                  {...heldPress(releasing, pressRelease)}
                  ref={releaseControl}
                  title="Release this claim so another agent can take the issue"
                  type="button"
                >
                  {releasing ? "Releasing…" : "Release"}
                </button>
              </span>
            )}
          </div>
        </div>
      </div>
      <HeaderDetails
        after={
          <>
            {subscribersOpen ? (
              <SubscribedAgents
                onUnsubscribe={(sessionId) => unsubscribe.mutate(sessionId)}
                ownerLabel={issue.key}
                subscribers={subscriberList}
              />
            ) : null}
            {referencesOpen ? (
              <ReferencedBy
                className="mt-3"
                reference={issueReference}
                toggle={{ id: referencesPanelId }}
              />
            ) : null}
          </>
        }
        leading={
          <div className="min-w-0 max-w-full" data-testid="issue-labels-group">
            <IssueLabels
              disabled={isClosed}
              labels={labelsShown}
              onSave={(labels) => labelsMutation.mutateAsync(labels)}
              project={issue.project}
              saving={labelsMutation.isPending && !labelsMutation.late}
            />
          </div>
        }
        onClose={discardUnsavedDetailsEdits}
        trailing={
          isClosed ? null : (
            <div className={headerGroup} data-testid="issue-actions">
              <button
                aria-label="Close issue"
                className={`min-h-11 shrink-0 rounded-lg px-2 py-2 text-sm font-medium sm:px-3 md:min-h-8 md:py-1 xl:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
                // Held, not disabled, while a write of the header's fields within its deadline
                // or the rule's question is out (`statusBusy`): the pressed Close keeps the
                // keyboard focus, and a second press sends nothing. A reopen hands the focus on
                // to it (`reopenFocus`).
                {...heldPress(statusBusy, closeIssue)}
                ref={reopenFocus.opener}
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
            </div>
          )
        }
        rows={detailsRows}
      />
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
        // Retry's press takes this line away as it sends the release again, and the focus Retry
        // holds goes on to Release, or to Release's place (`retryLeft`).
        <div className="contents" ref={retryLeft}>
          <QueryError
            message={
              stoppedNote(releaseClaim, "the release of this claim", "made") ??
              "Could not release this claim."
            }
            onRetry={retryRelease}
            retrying={releaseClaim.isPending}
          />
        </div>
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
      {/* The rule's question and the notes it leaves: a status the rule stops asks here, and the
          answer it gives is written from here (`parent-status/IssueStatusNotes`). */}
      <IssueStatusNotes
        issueKey={issue.key}
        noteClassName={`mt-1 text-sm ${dangerText}`}
        statusMove={statusMove}
      />
    </header>
  );
}
