import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import {
  agentConversationQuery,
  openAsksQuery,
  userAgentStateQuery,
  whoAmIQuery,
} from "../../api/queries";
import { invalidateWaitingForPaused } from "../../api/query-client";
import { useReadBound } from "../../api/read-bound";
import type {
  Agent,
  MessageDeliveryMode,
  MessageRead,
  MessageWithTrees,
  UserAgentStates,
} from "../../api/types";
import { ControlRow } from "../../components/ControlRow";
import { CopyButton } from "../../components/CopyButton";
import { ChevronIcon, DisclosureToggle } from "../../components/DisclosureToggle";
import { EmptyState } from "../../components/EmptyState";
import type { FilterChip } from "../../components/filter-chips";
import {
  ARM_ON_PRESS,
  controlsLikeRow,
  focusNoScroll,
  inSightWhole,
  useFocusPlace,
} from "../../components/focus-place";
import { heldPress } from "../../components/held-press";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { MultiSelect } from "../../components/MultiSelect";
import { LabelPill } from "../../components/Pill";
import { PinButton } from "../../components/PinButton";
import { AgentListRefilling } from "../../components/QueryError";
import { useNestedPanels } from "../../components/useNestedPanels";
import { useSending } from "../../hooks/useSending";
import { compareTimestamps } from "../../lib/timestamps";
import {
  borderDefault,
  card,
  checkboxAccent,
  dangerText,
  disclosureButtonText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedHoverToSecondary,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { replyParentOf } from "../conversation/composer-model";
import { listedReplies } from "../conversation/conversation-model";
import { deliveryModesOf, MODE_LABELS } from "../conversation/delivery";
import { useHeldSends } from "../conversation/held-sends";
import { ReplyTurn, ThreadReplies } from "../conversation/ReplyTurn";
import { capabilitiesForTarget, TargetedMessageCard } from "../conversation/TargetedMessageCard";
import { useAgents } from "../conversation/useAgents";
import { type MeasuredHeights, rowWindow, withMeasuredHeights } from "../delivery/lib/rowWindow";
import { waitingOnYou } from "../inbox/BlockedOnYou";
import { buildInboxPath } from "../inbox/inbox-route";
import { AgentName, type AgentPart, AgentUnresponsiveNote, useAgentText } from "../refs/AgentName";
import { PostBody } from "../refs/PostBody";
import { EditedMark } from "../refs/PostMarks";
import { postDeletion } from "../refs/post-marks";
import { buildIssuePath, buildMachinePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { closestMatching, focusOnDocument } from "../shell/roving";
import { MD_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { useDocumentTitle } from "../shell/useDocumentTitle";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { AgentMessageComposer, type AgentReply, agentSendKey } from "./AgentMessageComposer";
import { AGENT_GROUP_LABELS, AGENT_GROUPS, type AgentGroup, agentGroup } from "./agent-groups";
import {
  type AgentFilters,
  filterOptions,
  matchesFilters,
  searchWords,
  sessionIssueKeys,
  useAgentFilters,
} from "./agent-search";
import { type NeedsYouBySession, tallyBySession } from "./ask-tally";
import { deliveryAttempts } from "./attempts";
import { BroadcastComposer } from "./BroadcastComposer";
import { BroadcastSends, useBroadcastQueue } from "./BroadcastSends";
import { compositionDelivery, useBroadcastComposition } from "./broadcast-composition";
import { composedBroadcast } from "./broadcast-plan";
import { EndedAgentsWithReplies } from "./EndedAgentsWithReplies";
import { agentReplyTo } from "./exchange-delivery";
import { AGENT_ROW_SELECTOR, foldToggleOf, leaveAgentComposer, useAgentsKeymap } from "./keyboard";
import { useAgentPinWrite, usePinnedAgents } from "./pins";
import { groupCountLabel, matchingSelection, selectionSummary, toggleMatching } from "./selection";
import { storeAgentState, unreadRepliesLabel, useMarkRepliesRead, useUnreadAtOpen } from "./unread";

/**
 * Pinned sessions first, in pin order; then whoever needs the viewer, then the most open asks,
 * then the newest Dispatch activity (sessions Dispatch never heard from last), then the title.
 */
export function orderAgents(
  agents: readonly Agent[],
  pinned: readonly string[],
  needsYou: NeedsYouBySession
): Agent[] {
  const pinOrder = new Map(pinned.map((sessionID, index) => [sessionID, index]));
  return [...agents].sort((left, right) => {
    const leftPin = pinOrder.get(left.session_id);
    const rightPin = pinOrder.get(right.session_id);
    if (leftPin !== undefined || rightPin !== undefined) {
      if (leftPin === undefined) return 1;
      if (rightPin === undefined) return -1;
      return leftPin - rightPin;
    }
    const byNeedsYou = (needsYou.get(right.session_id) ?? 0) - (needsYou.get(left.session_id) ?? 0);
    if (byNeedsYou !== 0) return byNeedsYou;
    if (left.open_asks !== right.open_asks) return right.open_asks - left.open_asks;
    if (left.last_activity === null || right.last_activity === null) {
      if (left.last_activity === null && right.last_activity !== null) return 1;
      if (left.last_activity !== null && right.last_activity === null) return -1;
    } else {
      const byActivity = compareTimestamps(right.last_activity, left.last_activity);
      if (byActivity !== 0) return byActivity;
    }
    return left.title.localeCompare(right.title) || left.session_id.localeCompare(right.session_id);
  });
}

/**
 * Every listed session in its group (`agent-groups.ts`), each group in `orderAgents` order. The
 * pin orders a group's rows and no longer moves a row between groups (INFRA-812's D3): a pinned
 * session Dispatch has not heard from for ten minutes is first among the Quiet rows, not lifted
 * out of them.
 */
function groupAgents(
  agents: readonly Agent[],
  pinned: readonly string[],
  needsYou: NeedsYouBySession,
  states: UserAgentStates,
  now: number
): Record<AgentGroup, Agent[]> {
  const grouped: Record<AgentGroup, Agent[]> = {
    active: [],
    idle: [],
    "needs-you": [],
    quiet: [],
  };
  for (const agent of agents) {
    const unread = states[agent.session_id]?.unread_replies ?? 0;
    grouped[agentGroup(agent, needsYou.get(agent.session_id) ?? 0, unread, now)].push(agent);
  }
  for (const group of AGENT_GROUPS) {
    grouped[group] = orderAgents(grouped[group], pinned, needsYou);
  }
  return grouped;
}

/** Every group open, which is how each load finds them and what a search goes back to. A fresh
 *  object each time, because it becomes state the toggles then change. */
function everyGroupOpen(): Record<AgentGroup, boolean> {
  return { active: true, idle: true, "needs-you": true, quiet: true };
}

/** Who a row's messages go to, as the delivery lines draw it: the row's own session, through
 *  `AgentName`, which names it by its current registry title. */
function rowTarget(agent: Agent): AgentPart {
  return { sessionId: agent.session_id, storedTitle: agent.title };
}

function AgentExchangeReply({
  agent,
  holdThroughRead,
  liveAgents,
  onReply,
  read,
  reply,
  replyDisabled,
}: {
  agent: Agent;
  /** Holds through the conversation's re-read (`useReadBound`). */
  holdThroughRead: () => Promise<void>;
  liveAgents: readonly Agent[] | undefined;
  onReply: (reply: AgentReply) => void;
  read: MessageRead;
  reply: MessageWithTrees;
  replyDisabled: boolean;
}): ReactNode {
  const text = useAgentText();
  const label = text.sessionText(agent.session_id, agent.title);
  const retry = useKeyedWrite({
    label: (delivery: MessageDeliveryMode) =>
      `Delivery to ${label} again, as ${MODE_LABELS[delivery]}`,
    // Pending through the read that draws the new attempt: Retry and the mode changes stay held
    // (`DeliveryRetry`), so the pressed one keeps the focus until that read takes them away. The
    // read settles either way, and a failed one lets them go. A read that has not settled by the
    // request deadline (`REQUEST_DEADLINE_MS`) - one paused while the browser is offline - lets
    // them go then, and the conversation says why it is not read yet (`useReadBound`).
    onSuccess: holdThroughRead,
    method: "POST",
    write: (delivery, bounds) => api.createMessageDelivery(reply.id, delivery, bounds),
  });
  const replies = read.replies ?? [];
  const parent = [read.message, ...replies].find((item) => item.id === reply.in_reply_to);
  const answer = replies.find(
    (candidate) => candidate.in_reply_to === reply.id && candidate.author.kind === "session"
  );
  const capabilities = capabilitiesForTarget(read.message.target, liveAgents);
  return (
    <ReplyTurn
      at={reply.created_at}
      author={reply.author}
      body={
        <div className={textPrimaryOnCanvas}>
          <PostBody
            deleted={postDeletion(reply)}
            markdown={reply.body}
            source={textSourceOf("message", reply.id, reply.issue_key)}
            surface="agents-row-reply"
            tree={reply.body_tree}
          />
        </div>
      }
      delivery={
        (reply.deliveries ?? []).length === 0
          ? undefined
          : {
              answeredBy: answer?.author,
              attempts: deliveryAttempts(reply.deliveries ?? [], rowTarget(agent)),
              retry: {
                canAside: capabilities?.includes("aside") !== false,
                canBtw: capabilities?.includes("btw") !== false,
                canSteer: capabilities?.includes("steer") !== false,
                onRetry: retry.mutate,
                retrying: retry.isPending,
              },
              targetName: rowTarget(agent),
            }
      }
      deleted={postDeletion(reply)}
      edited={reply.edited_at}
      onReply={() => onReply(agentReplyTo(agent, read, reply))}
      quote={
        parent === undefined
          ? undefined
          : {
              author: parent.author,
              block: reply.reply_block,
              deleted: postDeletion(parent),
              edited: parent.edited_at,
              excerpt: parent.body,
              excerptTree: parent.body_tree,
              source: textSourceOf("message", parent.id, parent.issue_key),
              ...(parent.issue_key === null
                ? {}
                : {
                    to: buildIssuePath({ id: parent.id, key: parent.issue_key, kind: "message" }),
                  }),
            }
      }
      replyDisabled={replyDisabled}
      turnID={`message:${reply.id}`}
    />
  );
}

function AgentTargetedMessage({
  agent,
  holdThroughRead,
  liveAgents,
  onReply,
  read,
  replyDisabled,
}: {
  agent: Agent;
  /** Holds through the conversation's re-read (`useReadBound`). */
  holdThroughRead: () => Promise<void>;
  liveAgents: readonly Agent[] | undefined;
  onReply: (reply: AgentReply) => void;
  read: MessageRead;
  replyDisabled: boolean;
}): ReactNode {
  const text = useAgentText();
  const label = text.sessionText(agent.session_id, agent.title);
  const retry = useKeyedWrite({
    label: (delivery: MessageDeliveryMode) =>
      `Delivery to ${label} again, as ${MODE_LABELS[delivery]}`,
    // Pending through the read that draws the new attempt, as a reply's retry is, and bounded
    // the same way.
    onSuccess: holdThroughRead,
    method: "POST",
    write: (delivery, bounds) => api.createMessageDelivery(read.message.id, delivery, bounds),
  });
  const replies = read.replies ?? [];
  const answer = replies.find((reply) => reply.author.kind === "session");
  return (
    <TargetedMessageCard
      answeredBy={answer?.author}
      body={
        <>
          <p className={`flex items-baseline gap-2 text-sm ${textSecondaryOnCanvas}`}>
            <AgentName actor={read.message.author} className="font-semibold" inline />
            <span>
              <Timestamp at={read.message.created_at} />
              <EditedMark at={read.message.edited_at} deleted={postDeletion(read.message)} />
            </span>
          </p>
          <div className={textPrimaryOnCanvas}>
            <PostBody
              deleted={postDeletion(read.message)}
              markdown={read.message.body}
              source={textSourceOf("message", read.message.id, read.message.issue_key)}
              surface="agents-row-message"
              tree={read.message.body_tree}
            />
          </div>
        </>
      }
      canAside={capabilitiesForTarget(read.message.target, liveAgents)?.includes("aside") !== false}
      canBtw={capabilitiesForTarget(read.message.target, liveAgents)?.includes("btw") !== false}
      canSteer={capabilitiesForTarget(read.message.target, liveAgents)?.includes("steer") !== false}
      deleted={postDeletion(read.message)}
      deliveries={deliveryAttempts(read.message.deliveries ?? [], rowTarget(agent))}
      header={null}
      onReply={() => onReply(agentReplyTo(agent, read, read.message))}
      onRetry={retry.mutate}
      replyDisabled={replyDisabled}
      retrying={retry.isPending}
      targetName={rowTarget(agent)}
      thread={
        replies.length === 0 ? null : (
          <ThreadReplies>
            {replies.map((reply) => (
              <AgentExchangeReply
                agent={agent}
                holdThroughRead={holdThroughRead}
                key={reply.id}
                liveAgents={liveAgents}
                onReply={onReply}
                read={read}
                reply={reply}
                replyDisabled={replyDisabled}
              />
            ))}
          </ThreadReplies>
        )
      }
      turnID={`message:${read.message.id}`}
    />
  );
}

/** When an exchange last moved: its newest reply, else the root message. */
function exchangeActivityAt(read: MessageRead): string {
  return ((read.replies ?? []).at(-1) ?? read.message).created_at;
}

/**
 * The exchanges a viewer still sees after a Clear: those whose newest message is after
 * `clearedBefore`. A message asked before the Clear but answered after it stays, because the
 * answer is news.
 */
function exchangesAfter(
  exchanges: readonly MessageRead[],
  clearedBefore: string | undefined
): readonly MessageRead[] {
  if (clearedBefore === undefined) return exchanges;
  return exchanges.filter((read) => compareTimestamps(exchangeActivityAt(read), clearedBefore) > 0);
}

/** What a conversation says when its read failed, and before why its read has not settled. */
const CONVERSATION_NOT_LOADED = "Could not load this conversation.";

function AgentMessageList({
  agent,
  liveAgents,
  onReply,
  open,
  replyDisabled,
}: {
  agent: Agent;
  liveAgents: readonly Agent[] | undefined;
  onReply: (reply: AgentReply) => void;
  /** Whether the row is open. Its list stays mounted while it is collapsed, and marks nothing read
   *  there: a reply is read once it is on screen. */
  open: boolean;
  /** Holds every Reply while the row's send is in flight, as the picker is held. */
  replyDisabled: boolean;
}): ReactNode {
  const queryClient = useQueryClient();
  const messages = useQuery(agentConversationQuery(agent.session_id));
  const agentState = useQuery(userAgentStateQuery());
  const [showOlder, setShowOlder] = useState(false);
  const [showCleared, setShowCleared] = useState(false);
  // A retry's hold through this conversation's read, and why that read has not settled once the
  // hold ran out (`useReadBound`).
  const [unsettled, holdThrough] = useReadBound();
  const holdThroughRead = () =>
    holdThrough(
      invalidateWaitingForPaused(queryClient, agentConversationQuery(agent.session_id).queryKey)
    );
  const clearedBefore = agentState.data?.[agent.session_id]?.cleared_before;
  const all = messages.data ?? [];
  const unread = exchangesAfter(all, clearedBefore);
  const visible = showCleared ? all : unread;
  const [newest, ...older] = visible;
  // Each of the viewer's own exchanges with a reply newer than how far they had read when the row
  // opened is shown, not left behind "Show N older". The watermark stays where it was while the
  // row is open, so marking those replies read does not fold them away from the viewer reading
  // them.
  const unreadAtOpen = useUnreadAtOpen(messages.data, open);
  const olderShown = older.filter((read) => unreadAtOpen?.has(read.message.id) === true);
  const olderFolded = older.filter((read) => !olderShown.includes(read));
  const rendered =
    newest === undefined ? [] : [newest, ...olderShown, ...(showOlder ? olderFolded : [])];
  useMarkRepliesRead(
    agent.session_id,
    !open || messages.isPending || agentState.isPending ? undefined : rendered
  );
  const text = useAgentText();
  const label = text.sessionText(agent.session_id, agent.title);
  // The cutoff is the newest visible message's own timestamp, not the browser clock: both are
  // compared against `created_at` (the server's clock), so a slow browser clock would otherwise
  // make Clear a silent no-op. This hides exactly what the viewer saw.
  const clear = useKeyedWrite({
    method: "PUT",
    write: (cutoff: string, bounds) =>
      api.putAgentState(agent.session_id, { cleared_before: cutoff }, bounds),
    label: () => `Conversation with ${label} cleared`,
    // A landing with no answer to give has the store read every query again.
    onSuccess: (next) => {
      setShowCleared(false);
      if (next !== null) storeAgentState(queryClient, agent.session_id, next);
    },
  });
  if (messages.isPending || agentState.isPending) return null;
  if (messages.isError || agentState.isError) {
    return <p className={`mt-3 text-sm ${dangerText}`}>{CONVERSATION_NOT_LOADED}</p>;
  }
  if (all.length === 0) return null;
  const row = (read: MessageRead) => (
    <AgentTargetedMessage
      agent={agent}
      holdThroughRead={holdThroughRead}
      key={read.message.id}
      liveAgents={liveAgents}
      onReply={onReply}
      read={read}
      replyDisabled={replyDisabled}
    />
  );
  return (
    <div className={`mt-3 border-t pt-3 ${borderDefault}`}>
      {/* A read of the conversation that has not settled is said as this list says one that
          failed, in the same line. */}
      {unsettled === undefined ? null : (
        <p className={`mt-3 text-sm ${dangerText}`}>
          {CONVERSATION_NOT_LOADED} {unsettled}.
        </p>
      )}
      {newest === undefined ? null : (
        <ol aria-label={`Conversation with ${label}`} className="space-y-2">
          {row(newest)}
          {olderShown.map(row)}
          {olderFolded.length === 0 ? null : (
            <li>
              <DisclosureToggle
                expanded={showOlder}
                label={`Show ${olderFolded.length} older`}
                onToggle={() => setShowOlder((open) => !open)}
              />
            </li>
          )}
          {showOlder ? olderFolded.map(row) : null}
        </ol>
      )}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        {clearedBefore === undefined || unread.length === all.length ? null : (
          <p className={`flex flex-wrap items-center gap-x-1 text-sm ${textMutedOnCanvas}`}>
            <span>
              Cleared <Timestamp at={clearedBefore} />
            </span>
            <span aria-hidden>·</span>
            <button
              aria-pressed={showCleared}
              className={`inline-flex min-h-11 items-center underline-offset-2 hover:underline md:min-h-8 ${textMutedHoverToSecondary}`}
              onClick={() => setShowCleared((open) => !open)}
              type="button"
            >
              {showCleared ? "Hide again" : "Show anyway"}
            </button>
          </p>
        )}
        {/* Held while its write is out (`heldPress`), not disabled: the button keeps the focus
            the reader's press left on it, and a press of it then sends nothing. */}
        {visible.length === 0 ? null : (
          <button
            className={`ml-auto inline-flex min-h-11 items-center text-sm md:min-h-8 ${textMutedHoverToSecondary}`}
            {...heldPress(clear.isPending, () =>
              clear.mutate(
                visible
                  .map(exchangeActivityAt)
                  .reduce((latest, at) => (compareTimestamps(at, latest) > 0 ? at : latest))
              )
            )}
            type="button"
          >
            Clear conversation
          </button>
        )}
      </div>
    </div>
  );
}

/** A whose-turn pill: a non-zero ask count linking to the Inbox narrowed to this agent's asks.
 *  `assignees` is the list's own filter: absent leaves the Inbox's default, the reader's own,
 *  which is what the `Needs you` pill counts and opens, the agent's asks on the reader's work;
 *  an empty array clears it, which is what the `Waiting on agent` pill needs, since the questions
 *  it counts sit on anybody's issue - that list is this session's whole open list, both turns,
 *  rather than the counted part alone. Neither link names the Inbox's `section`: an agent
 *  narrowing is drawn beside no Blocked on you switch, and the Inbox drops `section` from an
 *  address that holds an agent (`inbox-route.ts`). */
function AskCountPill({
  agent,
  assignees,
  count,
  selected = false,
  title,
  wording,
}: {
  agent: Agent;
  assignees?: readonly string[];
  count: number;
  selected?: boolean;
  title: string;
  wording: string;
}): ReactNode {
  return (
    <Link
      className="inline-flex min-h-11 items-center rounded-full md:min-h-8"
      title={title}
      to={buildInboxPath({ agent: agent.session_id, assignees })}
    >
      <LabelPill selected={selected}>
        {wording} {count}
      </LabelPill>
    </Link>
  );
}

/** The last segment of a session's directory, which is what the row shows in place of the whole
 *  path. A trailing slash names no segment, and a path of nothing but slashes has none at all,
 *  so both keep the path itself rather than show an empty directory. */
function lastPathSegment(dir: string): string {
  const segments = dir.split("/").filter((segment) => segment !== "");
  return segments.at(-1) ?? dir;
}

/** A row's machine name, the one link in its directory line. It sits inside that line's
 *  sentence, so it keeps its line box (`dispatch-inline-link`, `styles.css`) rather than taking
 *  the 44 px touch floor every other link below 1280 px takes, which would make every row on a
 *  phone a line taller; its padding reaches the hit size instead. */
const machineLink = `dispatch-inline-link rounded underline outline-none focus-visible:ring-2 ${focusVisibleRing} ${linkText} ${linkHoverText}`;

function AgentRow({
  agent,
  expanded,
  hidden,
  liveAgents,
  mdViewport,
  needsYou,
  onExpand,
  onPin,
  onSelect,
  opened,
  pinned,
  section,
  selected,
  unreadReplies,
  waitingOnAgent,
}: {
  agent: Agent;
  /** Whether the row shows its conversation and its composer. The page holds this, not the row:
   *  the list is windowed, and the rows it keeps drawn wherever they have scrolled to are the
   *  ones a reader has opened (`rowWindow.ts`, D1). */
  expanded: boolean;
  /** A row a closed fold or the filters hide, which the page draws only while the reader has it
   *  open: hidden, so what that row holds survives the fold and the filter, as it survives a
   *  scroll. */
  hidden: boolean;
  /** The roster a reply's delivery capabilities are judged against; `undefined` while the agent
   *  list's read is not current, when every capability is unknown (`capabilitiesForTarget`). */
  liveAgents: readonly Agent[] | undefined;
  /** Which side of `md` the page is on, read once for the whole list (`MD_VIEWPORT_QUERY` in
   *  `AgentsPage`): the directory below it shows the whole path, and from it the last segment
   *  with the path one hover away. One listener for two hundred rows, not two hundred. */
  mdViewport: boolean;
  /** This session's open questions that wait on the reader themselves: whose next move is a
   *  person's, which wait on them, and not snoozed. */
  needsYou: number;
  /** Opens the row, or closes it: the page keeps the reader's answer. */
  onExpand: (expanded: boolean) => void;
  onPin: () => void;
  onSelect: (selected: boolean) => void;
  /** Whether the reader has ever opened this row, which is what mounts its conversation and its
   *  composer. They stay mounted, hidden, while the row is collapsed: an unsent draft, an
   *  upload, the unread set and what the reader unfolded each have one owner, which a collapse
   *  no more discards than a pin does. A send and its refusal are the held-send store's. */
  opened: boolean;
  pinned: boolean;
  /** The group the row is listed under, which `keyboard.ts` reads to find that group's
   *  heading (`data-agent-section`). */
  section: AgentGroup;
  selected: boolean;
  /** Replies to this session the reader has not read, from the viewer's agent state. The page
   *  reads that state once for its grouping and hands each row its own number: a query of the
   *  row's own would be one more subscription to the same key per mounted row. */
  unreadReplies: number;
  /** This session's open questions whose next move is its own, counted in their own right
   *  rather than as what is left of `open_asks`. */
  waitingOnAgent: number;
}): ReactNode {
  const text = useAgentText();
  // The row's attributes name the session as its `AgentName` does: by its current registry title.
  const label = text.sessionText(agent.session_id, agent.title);
  const machineAndDir = `${agent.host} · ${agent.dir}`;
  // From `md` the row is one line, so the directory keeps the segment that names the session's
  // work and the whole path is the hover its `title` is. Below `md` it has a line of its own
  // under the name and a phone has no hover to ask that `title` with, so it shows the path
  // whole. The machine is drawn before it as a link of its own, so it is no part of this.
  const shownDir = mdViewport ? lastPathSegment(agent.dir) : agent.dir;
  // What the row holds itself is the reply the composer is answering; the page holds the rest.
  // A send the row's composer holds - out, or refused - still answers its reply when the row
  // mounts again, so the row restores the reply it was sent as.
  const heldSends = useHeldSends();
  const [replyTo, setReplyTo] = useState<AgentReply | null>(() => {
    const held = heldSends.get(agentSendKey(agent.session_id))?.request;
    return held === undefined || held.replyTo === null
      ? null
      : {
          issueKey: held.owner.kind === "issue" ? held.owner.issueKey : null,
          target: held.replyTo,
        };
  });
  const detailsId = useId();

  return (
    <article
      className={`rounded-xl border outline-none focus-visible:ring-2 ${card} ${borderDefault} ${focusVisibleRing}`}
      data-agent-row={agent.session_id}
      data-agent-section={section}
      // The issue picker renders only while this row is not answering a message, and a collapsed
      // row's picker is not on screen, so the row itself carries whether `i` can act.
      data-agent-can-pick-issue={replyTo === null ? "" : undefined}
      hidden={hidden}
      tabIndex={-1}
    >
      {/* One line from `md`: with six hundred rows a second line is six hundred lines, and the
          window's saving (R7) is paid back in paint. The name and the directory are what give
          way, each to an ellipsis with its whole text one hover away. Below `md` the directory
          wraps to a line of its own (the span below), which is the shape the row had before
          this change and what keeps the name a box wide enough to tap on a phone. */}
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 overflow-hidden px-3 py-1.5 md:flex-nowrap">
        <input
          aria-label={`Select ${label} for broadcast`}
          checked={selected}
          className={`size-4 shrink-0 ${checkboxAccent}`}
          data-agent-select=""
          onChange={(event) => onSelect(event.target.checked)}
          type="checkbox"
        />
        {/* The row's toggle keeps the name; `AgentName` draws the running state before it and
            the live link after it, outside the heading, so the heading's text is the name
            alone. `contents` keeps each of them a flex item of the row, as the heading is. The
            name takes what it needs up to a ceiling, so a long one shortens rather than
            squeezing the directory beside it to nothing. */}
        <AgentName
          className="contents"
          control={(name) => (
            <h2 className={`min-w-0 max-w-64 shrink text-sm font-semibold ${textPrimaryOnCanvas}`}>
              <button
                aria-controls={detailsId}
                aria-expanded={expanded}
                className={`flex min-h-11 w-full items-center gap-1 text-left md:min-h-8 ${textPrimaryOnCanvas}`}
                data-agent-toggle=""
                onClick={() => onExpand(!expanded)}
                title={label}
                type="button"
              >
                <span className="truncate">{name}</span>
                <span className={`shrink-0 ${disclosureButtonText}`}>
                  <ChevronIcon expanded={expanded} />
                </span>
              </button>
            </h2>
          )}
          kind="control"
          sessionId={agent.session_id}
          storedTitle={agent.title}
        />
        <CopyButton value={agent.session_id} what="session ID">
          ID
        </CopyButton>
        {agent.title.trim() === "" ? null : (
          <CopyButton value={agent.title} what="session title">
            title
          </CopyButton>
        )}
        {/* The directory. From `md` it takes the width the rest of the line leaves and keeps
            the segment that names the session's work, with the machine before it and the whole
            path one hover away. Below `md` it takes a line of its own under the name
            (`order-last basis-full`, the shape the page drew before INFRA-812) and shows the
            whole path: a phone has no hover to ask with, and on one line a 390 px row squeezed
            both this and the name button to nothing, which left the name untappable - the live
            link beside it took the press - and the directory unpainted. It is the row's one
            stretching item from `md`, so a row with little else in it still reads its directory
            in full. */}
        <span
          className={`order-last basis-full text-xs [overflow-wrap:anywhere] md:order-none md:min-w-0 md:flex-1 md:basis-auto md:truncate ${textMutedOnCanvas}`}
          data-agent-dir=""
          title={machineAndDir}
        >
          {/* Only the machine is a link: the span still reads `<machine> · <directory>` whole,
              and the machine's own page is one click from a row that names it. */}
          <Link
            className={machineLink}
            data-agent-machine={agent.host}
            to={buildMachinePath(agent.host)}
          >
            {agent.host}
          </Link>
          {` · ${shownDir}`}
        </span>
        {(agent.roles ?? []).map((role) => (
          <span className="shrink-0" key={role}>
            <LabelPill>{role}</LabelPill>
          </span>
        ))}
        {/* A session that has missed its heartbeats is still listed and still worth
            messaging, so the row says which it is rather than looking like any other: its
            name's mark alone is a ring a reader would have to hover. */}
        <AgentUnresponsiveNote
          lastSeen={agent.last_seen}
          state={agent.state === "unresponsive" ? "unresponsive" : "running"}
        />
        <div className="flex shrink-0 items-center gap-2">
          <span className={`whitespace-nowrap text-xs ${textMutedOnCanvas}`}>
            {agent.last_activity === null ? (
              "No Dispatch activity"
            ) : (
              <Timestamp at={agent.last_activity} />
            )}
          </span>
          {unreadReplies === 0 ? null : (
            // Opening the conversation is what reads it, so the badge opens it. It is held while
            // the row is open, where its press, which only opens the row, does nothing.
            <button
              // The pill's own words first, so the name contains the visible text and a voice
              // user can press what they read (WCAG 2.5.3, axe's
              // `label-content-name-mismatch`); the session's name says whose replies they are.
              aria-label={`${unreadRepliesLabel(unreadReplies)} from ${label}`}
              className="inline-flex min-h-11 items-center rounded-full md:min-h-8"
              {...heldPress(expanded, () => onExpand(true))}
              title={`Replies from ${label} you have not read`}
              type="button"
            >
              <LabelPill selected>{unreadRepliesLabel(unreadReplies)}</LabelPill>
            </button>
          )}
          {needsYou === 0 ? null : (
            <AskCountPill
              agent={agent}
              count={needsYou}
              selected
              title={`Open asks from ${label} waiting on your answer`}
              wording="Needs you"
            />
          )}
          {waitingOnAgent === 0 ? null : (
            <AskCountPill
              agent={agent}
              // These questions are on anybody's issue, while the Inbox opens on the reader's
              // own, so the link clears the filter: left at the default it would hide the ones
              // on issues the reader does not hold, and show none at all when no question of
              // this session's sits on one of theirs. What it opens is this session's whole
              // open list, both turns, of which this number is the agent-turn part.
              assignees={[]}
              count={waitingOnAgent}
              title={`Open asks from ${label} whose next move is ${label}'s`}
              wording="Waiting on agent"
            />
          )}
          {/* `contents` so the keymap has a handle on the pin without a box in the flex row. */}
          <span className="contents" data-agent-pin="">
            <PinButton
              label={pinned ? `Unpin ${label}` : `Pin ${label}`}
              onClick={onPin}
              pinned={pinned}
              title={pinned ? "Unpin agent" : "Pin agent"}
            />
          </span>
        </div>
      </div>
      {opened ? (
        <AgentRowDetails
          agent={agent}
          expanded={expanded}
          hidden={hidden}
          id={detailsId}
          liveAgents={liveAgents}
          replyTo={replyTo}
          setReplyTo={setReplyTo}
        />
      ) : null}
    </article>
  );
}

/** What a row shows once opened: its conversation and its composer. It reads the row's send, so
 *  a row never opened watches no send, and holds every Reply while that send is out, from Send's
 *  own task on. */
function AgentRowDetails({
  agent,
  expanded,
  hidden,
  id,
  liveAgents,
  replyTo,
  setReplyTo,
}: {
  agent: Agent;
  expanded: boolean;
  hidden: boolean;
  id: string;
  liveAgents: readonly Agent[] | undefined;
  replyTo: AgentReply | null;
  setReplyTo: (reply: AgentReply | null) => void;
}): ReactNode {
  const sendKey = useMemo(() => agentSendKey(agent.session_id), [agent.session_id]);
  const hold = useSending(sendKey);
  // The delivery modes the session lists, and only those: a capability that is no mode carries no
  // message, so a list of those alone takes none, as an empty one does.
  const modes = deliveryModesOf(agent.capabilities ?? []);
  // The session's conversation as the row's list reads it (`AgentMessageList`), the same read,
  // whatever a fold or a Clear shows of it: a draft the composer keeps for one of its messages
  // goes back by that message's Reply, and one kept for a message the read no longer holds is
  // drawn itself (`ListedReplies`). Until the read answers, every kept reply is as it was. The
  // message a reply quotes is looked up there too, so a draft open when its author deletes or
  // edits that message quotes what became of it.
  const messages = useQuery(agentConversationQuery(agent.session_id));
  const listed = useMemo(
    () => (messages.data === undefined ? undefined : listedReplies(messages.data)),
    [messages.data]
  );
  return (
    <div className={`border-t px-4 pb-4 ${borderDefault}`} hidden={!expanded} id={id}>
      <div
        className={`mt-3 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs ${textMutedOnCanvas}`}
      >
        {modes.length === 0 ? (
          // A session that advertises no mode refuses every message: an empty list says its plugin
          // cannot reach its conversation, and a list of unknown capabilities names no mode to use.
          <span>Takes no messages</span>
        ) : (
          modes.map((mode) => <span key={mode}>{MODE_LABELS[mode]}</span>)
        )}
        <span>
          Seen <Timestamp at={new Date(agent.last_seen).toISOString()} />
        </span>
      </div>
      <AgentMessageList
        agent={agent}
        liveAgents={liveAgents}
        onReply={(next) => {
          if (!hold.sendingNow()) setReplyTo(next);
        }}
        // Open means on screen: an expanded row a closed fold or a filter hides is as unseen as
        // a collapsed one, so it marks nothing read, and showing it again is an open that
        // freezes its unread set afresh.
        open={expanded && !hidden}
        replyDisabled={hold.sending}
      />
      <div data-agent-composer="">
        <AgentMessageComposer
          agent={agent}
          hold={hold}
          listed={listed}
          onCancelReply={() => setReplyTo(null)}
          onClose={leaveAgentComposer}
          replyParent={replyParentOf(replyTo?.target ?? null, [], messages.data ?? [])}
          replyTo={replyTo}
          sendKey={sendKey}
        />
      </div>
    </div>
  );
}

/**
 * A group's heading: an item in the one list, between the rows above it and its own, saying
 * what the group is and how many rows it holds, and collapsing them. Every group has one,
 * empty or not, so the four states are always on the page and the browser check can read each
 * one's count. A block item rather than a bare button, so the heading keeps its own width, and
 * `keyboard.ts` has a handle on it (`data-agent-fold`): the pin's focus hand-off lands on it,
 * and `j`/`k` step on from it. The count is drawn only once the reads that place a row in its
 * group have answered (`counted`); until then the heading names its group alone.
 */
function GroupHeading({
  agents,
  counted,
  expanded,
  group,
  onToggle,
  selected,
}: {
  /** The group's rows the filters match: what the count counts, and what the heading hides. */
  agents: readonly Agent[];
  /** Whether the grouping is the answered one: the open asks, the sign-in and the reader's agent
   *  state have each answered, so Needs you holds every session it should. */
  counted: boolean;
  expanded: boolean;
  group: AgentGroup;
  onToggle: () => void;
  selected: ReadonlySet<string>;
}): ReactNode {
  return (
    <div data-agent-fold={group} data-testid={`agents-group-${group}`}>
      <button
        aria-expanded={expanded}
        className={`flex min-h-11 items-center gap-1 text-sm font-medium ${textSecondaryOnCanvas}`}
        data-testid={`agents-group-toggle-${group}`}
        onClick={onToggle}
        type="button"
      >
        <span data-testid={`agents-group-name-${group}`}>{AGENT_GROUP_LABELS[group]}</span>
        {counted ? (
          <span className="tabular-nums" data-testid={`agents-group-count-${group}`}>
            {groupCountLabel(agents, selected)}
          </span>
        ) : null}
        <span className={disclosureButtonText}>
          <ChevronIcon expanded={expanded} />
        </span>
      </button>
    </div>
  );
}

/** Which of the three narrowings has its popover open, so the panel keeps one at a time rather
 *  than a row of them; each is portalled over the page, and two would cover each other. */
type OpenFilter = "group" | "machine" | "role";

/** The group filter's option labels: the control hands its values over as strings, and a string
 *  that names no group has no label. */
const GROUP_FILTER_LABELS: ReadonlyMap<string, string> = new Map(
  Object.entries(AGENT_GROUP_LABELS)
);

/**
 * The control bar the page's controls sit in (`ControlRow`), handed each control as its props:
 * the search box, **Filter** over the three narrowings that are no column of a table here -
 * group, machine and role - and the chips of the values that are on, which the trigger's panel
 * lists at its top too. Each narrowing is still a multi-select whose panel holds a text box that
 * narrows its own options, because forty machines cannot be read in a list that does not search;
 * all three sit behind one trigger rather than three of their own. The page has no **Clear
 * filters**: each chip takes its own value off.
 *
 * The placeholder is the box's name, `Search agents`, which still says what it searches where the
 * bar gives the box its least room, at the phone's width (the measurement of 2026-11-24,
 * INFRA-4948's `r8-bar-measurement-md`).
 */
function AgentFilterBar({
  agents,
  filters,
  onFilters,
  searchInputRef,
}: {
  agents: readonly Agent[];
  filters: AgentFilters;
  /** Writes the fields it is handed and no other (`useAgentFilters`). */
  onFilters: (changed: Partial<AgentFilters>) => void;
  searchInputRef: RefObject<HTMLInputElement | null>;
}): ReactNode {
  // The options carry every machine and every role the filter names even when no live session
  // reports them, so the control shows the filter in force and its row can be unticked
  // (`filterOptions`).
  const { machines, roles } = filterOptions(agents, filters.machines, filters.roles);
  /** Whether the one **Filter** panel stands open. */
  const [filterOpen, setFilterOpen] = useState(false);
  /** Which of its three controls has its popover open: one at a time, and none once it shuts. */
  const filterPanel = useNestedPanels<OpenFilter>(filterOpen);
  // One chip per value the reader has ticked, in the order the panel lists the three controls;
  // a press takes that one value off and leaves the rest of the narrowing alone.
  const chosen: FilterChip[] = [
    ...filters.groups.map((value) => ({
      key: `group:${value}`,
      label: `Group: ${AGENT_GROUP_LABELS[value]}`,
      remove: () => onFilters({ groups: filters.groups.filter((one) => one !== value) }),
    })),
    ...filters.machines.map((value) => ({
      key: `machine:${value}`,
      label: `Machine: ${value}`,
      remove: () => onFilters({ machines: filters.machines.filter((one) => one !== value) }),
    })),
    ...filters.roles.map((value) => ({
      key: `role:${value}`,
      label: `Role: ${value}`,
      remove: () => onFilters({ roles: filters.roles.filter((one) => one !== value) }),
    })),
  ];
  return (
    <ControlRow
      chips={chosen}
      className="mb-3"
      search={{
        "data-testid": "agents-filter-search",
        label: "Search agents",
        onChange: (value) => onFilters({ search: value }),
        placeholder: "Search agents",
        ref: searchInputRef,
        value: filters.search,
      }}
      trigger={{
        children: () => (
          <>
            <MultiSelect
              {...filterPanel("group")}
              emptyMessage="No group matches that."
              label="Group"
              // The control picks strings; these are the four group keys, read back in the one
              // order the page draws them in, whatever order they were ticked in.
              onChange={(next) =>
                onFilters({ groups: AGENT_GROUPS.filter((group) => next.includes(group)) })
              }
              optionLabel={(value) => GROUP_FILTER_LABELS.get(value)}
              options={AGENT_GROUPS}
              searchLabel="Search groups"
              selected={filters.groups}
              triggerAriaLabel="Filter by group"
            />
            <MultiSelect
              {...filterPanel("machine")}
              emptyMessage="No machine matches that."
              label="Machine"
              onChange={(next) => onFilters({ machines: next })}
              options={machines}
              searchLabel="Search machines"
              selected={filters.machines}
              triggerAriaLabel="Filter by machine"
            />
            <MultiSelect
              {...filterPanel("role")}
              emptyMessage="No role matches that."
              label="Role"
              onChange={(next) => onFilters({ roles: next })}
              options={roles}
              searchLabel="Search roles"
              selected={filters.roles}
              triggerAriaLabel="Filter by role"
            />
          </>
        ),
        count: chosen.length,
        label: "Filter",
        onOpenChange: setFilterOpen,
        open: filterOpen,
        stacked: true,
      }}
    />
  );
}

/**
 * The list's select-all checkbox. Its 13 px inset lines it up with the rows' checkboxes (their
 * 1 px border plus 12 px padding). Matching means every row the filters match, folded sections
 * included, and `Clear selection` also empties the selected rows the filters hide. A selected
 * session missing from a list that is not `current` is counted as of unknown state, not as gone,
 * as the composer's chip names it. Clear selection goes with the selection it empties, and the
 * focus it held goes to the select-all checkbox before it while the reader can see it
 * (`useFocusPlace`). While no row matches, that checkbox is disabled and the header holds no
 * other control, so the header's own hand-off walks back over the main content to the nearest
 * control before the header that the reader can see and that takes no typed text: the Filter
 * button. The header arms its own hand-off at a press inside it (`armOnPress`).
 */
function SelectionHeader({
  current,
  listed,
  matching,
  onClear,
  onToggle,
  selected,
}: {
  /** Whether the latest read of `listed` succeeded, so that it says who is absent (`useAgents`). */
  current: boolean;
  listed: readonly Agent[];
  matching: readonly Agent[];
  onClear: () => void;
  onToggle: () => void;
  selected: ReadonlySet<string>;
}): ReactNode {
  const selection = matchingSelection(listed, matching, selected, current);
  const { state } = selection;
  const checkbox = useRef<HTMLInputElement>(null);
  const header = useRef<HTMLDivElement>(null);
  const countId = useId();
  // `indeterminate` is a DOM property with no attribute, so React cannot render it.
  useLayoutEffect(() => {
    if (checkbox.current !== null) checkbox.current.indeterminate = state === "some";
  }, [state]);
  const noteFocus = useFocusPlace(
    () => header.current,
    () => null
  );
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the focus handler only notes which control of the header took the focus, for the hand-off when a commit takes that control away (`useFocusPlace`), and the capture handlers only arm that hand-off at a press inside the header (`armOnPress`), so the header needs no role of its own.
    <div
      className="mb-2 flex min-h-11 items-center gap-x-2 px-[13px] md:min-h-8"
      {...ARM_ON_PRESS}
      onFocus={noteFocus}
      ref={header}
    >
      <input
        aria-describedby={countId}
        aria-label="Select all matching agents"
        checked={state === "all"}
        className={`size-4 shrink-0 ${checkboxAccent}`}
        disabled={matching.length === 0}
        onChange={onToggle}
        ref={checkbox}
        type="checkbox"
      />
      <span className={`min-w-0 flex-1 text-xs ${textMutedOnCanvas}`} id={countId}>
        {selectionSummary(selection)}
      </span>
      {selected.size === 0 ? null : (
        <button
          className={`min-h-11 shrink-0 rounded-lg border px-2 text-xs font-medium md:min-h-7 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          onClick={onClear}
          type="button"
        >
          Clear selection
        </button>
      )}
    </div>
  );
}

/** One item of the page's list: a group's heading, or a session's row under it. */
type ListItem =
  | { kind: "group"; key: string; group: AgentGroup }
  | { kind: "row"; key: string; group: AgentGroup; agent: Agent };

/** How tall a row nothing has measured is taken to be: a collapsed row at `md`. It gives the
 *  list a scrollbar before a row is drawn and decides how many rows the first paint draws;
 *  every drawn row then replaces it with what it measured. */
const ROW_ESTIMATE_PX = 44;

/** A drawn item of the windowed list, which is what `useListWindow` measures. */
const LIST_ITEM_SELECTOR = "[data-row-key]";

/** The row `sessionID` draws, drawn or kept hidden, inside `list`. */
function rowNode(list: HTMLElement | null, sessionID: string): HTMLElement | null {
  return (
    [...(list?.querySelectorAll<HTMLElement>(AGENT_ROW_SELECTOR) ?? [])].find(
      (node) => node.dataset.agentRow === sessionID
    ) ?? null
  );
}

/** The rows `list` draws now, in the order the page shows them: a row kept mounted but hidden in
 *  a collapsed group is none of them. */
function drawnRows(list: HTMLElement | null): HTMLElement[] {
  return [...(list?.querySelectorAll<HTMLElement>(AGENT_ROW_SELECTOR) ?? [])].filter(
    (node) => !node.hidden
  );
}

/** What the page draws its list from: which items, where each sits, and how tall the list is. */
interface ListWindow {
  containerRef: RefObject<HTMLUListElement | null>;
  /** Every item the list draws, as indices into the keys it was given, ascending. */
  drawn: readonly number[];
  /** Where each item starts, in pixels from the list's own top. */
  tops: readonly number[];
  /** The whole list's height, drawn items and undrawn alike. */
  height: number;
}

/**
 * The window over `keys` (`features/delivery/lib/rowWindow.ts`), measured as it is drawn, with
 * `kept` the items drawn wherever they have scrolled to.
 *
 * The page scrolls in the document rather than in a box of its own, so what the window reads is
 * the list's own top edge against the viewport: everything above the list has scrolled past when
 * that edge is negative. The scroll listener is a capturing one, which hears an ancestor box
 * scroll as well as the document. Every drawn item is measured after each render, and a
 * `ResizeObserver` catches a height that changes with no render of the page's - a conversation
 * arriving inside an open row - with a height that has not changed costing no render.
 */
function useListWindow(keys: readonly string[], kept: ReadonlySet<string>): ListWindow {
  const containerRef = useRef<HTMLUListElement>(null);
  const [view, setView] = useState(() => ({ scrollTop: 0, viewportHeight: window.innerHeight }));
  const [measured, setMeasured] = useState<MeasuredHeights>(() => new Map());
  /** Every item the list has drawn, each carrying the key its height is cached under. Queried
   *  once per render and handed to both the measuring and the observer, rather than twice. */
  const drawnItems = useCallback(
    (): readonly HTMLElement[] => [
      ...(containerRef.current?.querySelectorAll<HTMLElement>(LIST_ITEM_SELECTOR) ?? []),
    ],
    []
  );
  const measure = useCallback((items: readonly HTMLElement[]) => {
    const heights = items.flatMap((item) => {
      const key = item.dataset.rowKey;
      return key === undefined ? [] : [[key, item.getBoundingClientRect().height] as const];
    });
    setMeasured((held) => withMeasuredHeights(held, heights));
  }, []);
  // An arrow rather than `measure` itself: `ResizeObserver` hands its callback the entries that
  // changed as its first argument, and the list measures every drawn item rather than those.
  const [observer] = useState(() =>
    typeof ResizeObserver === "undefined" ? null : new ResizeObserver(() => measure(drawnItems()))
  );
  const readView = useCallback(() => {
    const top = containerRef.current?.getBoundingClientRect().top ?? 0;
    setView((held) => {
      const next = { scrollTop: Math.max(0, -top), viewportHeight: window.innerHeight };
      return held.scrollTop === next.scrollTop && held.viewportHeight === next.viewportHeight
        ? held
        : next;
    });
  }, []);
  useEffect(() => {
    readView();
    window.addEventListener("scroll", readView, { capture: true, passive: true });
    window.addEventListener("resize", readView);
    return () => {
      window.removeEventListener("scroll", readView, { capture: true });
      window.removeEventListener("resize", readView);
    };
  }, [readView]);
  /** The items the observer is watching. Each render moves it to the items drawn now - what
   *  left is unobserved, what arrived is observed - rather than disconnecting it and arming it
   *  again over every one: `observe` delivers an entry for each newly observed element, so
   *  re-observing them all would measure the whole list a second time after every render. */
  const observed = useRef<readonly HTMLElement[]>([]);
  // After every render, because every render can change what is drawn and how tall it is.
  useLayoutEffect(() => {
    const items = drawnItems();
    measure(items);
    if (observer === null) return;
    const drawn = new Set(items);
    for (const item of observed.current) {
      if (!drawn.has(item)) observer.unobserve(item);
    }
    const held = new Set(observed.current);
    for (const item of items) {
      if (!held.has(item)) observer.observe(item);
    }
    observed.current = items;
  });
  // The observer belongs to the list rather than to a render, and goes once, with the list.
  useEffect(() => () => observer?.disconnect(), [observer]);
  const list = rowWindow({
    estimate: ROW_ESTIMATE_PX,
    kept,
    keys,
    measured,
    scrollTop: view.scrollTop,
    viewportHeight: view.viewportHeight,
  });
  return { containerRef, drawn: list.drawn, height: list.height, tops: list.tops };
}

/**
 * The list's own box: as tall as every item, drawn or not, so the scrollbar measures the list
 * rather than the part of it on screen, and each drawn item sits at the place the window
 * measured for it.
 *
 * The box owns the window, which is why it is a component of its own rather than part of the
 * page. The window's state moves with every scroll event the page hears, and the page groups,
 * sorts and filters every listed session on each of its own renders; here a scroll re-renders
 * this box alone, and what the page worked out - `items`, `kept`, `keptAside` and `renderItem` -
 * is untouched by it.
 *
 * The box is a list, and each drawn item - a group's heading or a session's card - is one of its
 * items that says how many the whole list holds (`aria-setsize`) and its own place among them
 * (`aria-posinset`), so a screen reader hears the whole list and where it is in it rather than
 * the drawn items alone. A kept row it no longer holds is a hidden item, which it does not count.
 */
function WindowedList({
  items,
  kept,
  keptAside,
  renderItem,
}: {
  /** The page's list in the order it draws it, each item under the key it is drawn with. */
  items: readonly ListItem[];
  /** The items drawn wherever they have scrolled to (D1). */
  kept: ReadonlySet<string>;
  /** The kept rows the list no longer holds, which the page draws hidden. They ride after the
   *  drawn items in the one array: two arrays would be two slots, and a row moving between
   *  them would remount. */
  keptAside: readonly ReactNode[];
  /** A heading or a row, as the page draws it. */
  renderItem: (item: ListItem) => ReactNode;
}): ReactNode {
  const list = useListWindow(
    items.map((item) => item.key),
    kept
  );
  return (
    <ul
      className="relative"
      data-agents-list=""
      ref={list.containerRef}
      style={{ height: `${list.height}px` }}
    >
      {[
        ...list.drawn.flatMap((index) => {
          const item = items[index];
          if (item === undefined) return [];
          return [
            <li
              aria-posinset={index + 1}
              aria-setsize={items.length}
              className="absolute inset-x-0 pb-3"
              data-row-key={item.key}
              key={item.key}
              style={{ top: `${list.tops[index] ?? 0}px` }}
            >
              {renderItem(item)}
            </li>,
          ];
        }),
        ...keptAside,
      ]}
    </ul>
  );
}

export function AgentsPage(): ReactNode {
  useDocumentTitle("Agents · Dispatch");
  const { agents, current, error, isError, isPending, refilling } = useAgents(true, true);
  // Which side of Tailwind's `md` the page is on, read once for the list rather than once per
  // row: it decides what each row's directory shows, and a list of two hundred rows is two
  // hundred listeners otherwise.
  const mdViewport = useMediaQuery(MD_VIEWPORT_QUERY);
  // Every open ask the viewer can see, which the cards count per session.
  const openAsks = useQuery(openAsksQuery());
  // Until `/auth/whoami` answers, no question is counted as the reader's.
  const viewer = useQuery(whoAmIQuery()).data?.login;
  // The pins are the person's, not this browser's (INFRA-812): the Agents page reads them from
  // the viewer's agent state and writes them to Dispatch, so a pin made here is in the rail and
  // in the person's other browsers at once.
  const pinned = usePinnedAgents();
  const writePin = useAgentPinWrite();
  // The viewer's own state for each session, which the rows already read for their unread
  // badges: one query, one answer, and the replies they have not read are what put a session in
  // Needs you beside the asks that wait on them.
  const agentStateRead = useQuery(userAgentStateQuery());
  const agentStates = agentStateRead.data ?? {};
  // Whether the grouping is the answered one. Until the open asks, the sign-in and the agent state
  // have each answered, no session can be put in Needs you, so each group heading is drawn
  // without its count rather than with one taken from a read that has not come in (or whose
  // first read failed). Every row pill is absent at zero, so none of them draws a number from
  // such a read either.
  const groupingAnswered =
    openAsks.data !== undefined && viewer !== undefined && agentStateRead.data !== undefined;
  // The first card number: the reader's own work with this session - a question whose next move
  // is a person's and which waits on them. One meaning for every count of what waits on
  // the reader, wherever it is shown.
  const needsYouBySession = useMemo(
    () => tallyBySession(waitingOnYou(openAsks.data ?? [], viewer)),
    [openAsks.data, viewer]
  );
  // The second card number, counted in its own right rather than as what is left of `open_asks`:
  // questions whose next move is the session's, on anybody's issue, snoozes ignored. A snooze is
  // the reader deferring a question, not a change of whose move it is, so a snoozed agent-turn
  // question is counted here and the list this pill opens holds it folded under `Later`. What
  // the subtraction this replaces got wrong is the other direction: it charged this session with
  // every question of the reader's they had snoozed, and with every other person's.
  const waitingOnAgentBySession = useMemo(
    () => tallyBySession(openAsks.data ?? [], (ask) => ask.waiting_on === "agent"),
    [openAsks.data]
  );
  // Every issue key an OPEN ask by each session names (`sessionIssueKeys`), for the search box's
  // "issue key" field.
  const issueKeysBySession = useMemo(() => sessionIssueKeys(openAsks.data ?? []), [openAsks.data]);
  // The whole narrowing is the page's address, owned by `agent-search.ts`: the free-text query
  // (`?q=`), the groups (`?group=`), the machines (`?machine=`) and the roles (`?role=`), each
  // absent until the reader picks something. A link in from the machines page names one machine
  // in `?machine=`, which is this filter with that one host ticked, and `/agents?role=<token>` is
  // the page a `dispatch://role/<token>` reference opens, which is that filter with one role
  // ticked.
  const [filters, setFilters] = useAgentFilters();
  const search = filters.search;
  // The picked roles no live session carries, which the empty state names rather than letting
  // the reader read this list as every role there is: a role is here only while a
  // session holds it.
  const unheldRoles = filters.roles.filter(
    (role) => !agents.some((agent) => (agent.roles ?? []).includes(role))
  );
  const searchInputRef = useRef<HTMLInputElement>(null);
  // Selection is what the human ticked, not what the filters currently show: narrowing the
  // list after ticking a row must not quietly drop that row from the send. Every selected
  // session is named in the composer, so nothing is hidden either way.
  // The draft (message and mode), the selection and a restored request are page state, in one
  // owner (`useBroadcastComposition`). Restore draft, on a send the server refused, puts them
  // back; any message or selection here was started after Send cleared both, so Restore draft
  // refuses rather than replace it - the same test the queue uses to hold off opening a broadcast
  // while another is begun.
  const composition = useBroadcastComposition();
  const { draft, selected, setSelected } = composition;
  // The mode the page shows and sends (`compositionDelivery`).
  const delivery = compositionDelivery(composition, agents);
  const composing = draft.trim() !== "" || selected.size > 0;
  const queue = useBroadcastQueue(composing);
  const restoreRefusal = composing
    ? "Restore draft would replace the broadcast you have started. Send it, or clear its message and selection, first."
    : null;
  const showComposer = selected.size > 0 && agents.length > 0;
  // Not memoised: the grouping is a function of the clock, and is recomputed on every render of
  // this page. Every listed session is grouped, the ones the filters exclude included: their
  // rows stay mounted, hidden, like a collapsed group's.
  const grouped = groupAgents(agents, pinned, needsYouBySession, agentStates, Date.now());
  const words = searchWords(filters.search);
  const matches = (agent: Agent, group: AgentGroup) =>
    matchesFilters(agent, group, filters, words, issueKeysBySession.get(agent.session_id) ?? []);
  // Each group's rows that the filters match: the heading's count, and what it hides.
  const matchingIn: Record<AgentGroup, Agent[]> = {
    active: grouped.active.filter((agent) => matches(agent, "active")),
    idle: grouped.idle.filter((agent) => matches(agent, "idle")),
    "needs-you": grouped["needs-you"].filter((agent) => matches(agent, "needs-you")),
    quiet: grouped.quiet.filter((agent) => matches(agent, "quiet")),
  };
  // The rows the filters match, in the order the page shows them - group by group. Select-all
  // ticks this set in order and the composer names the selection in tick order, so a set ordered
  // any other way (the registry's own, say) would name the recipients in an order the reader
  // never sees, and send them in it.
  const matching = AGENT_GROUPS.flatMap((group) => matchingIn[group]);
  /** The row a pin was made from while it held focus, and where that row stood among the rows the
   *  list drew, until the render that moves it. A pin no longer moves a row out of its group
   *  (D3), but it does move it within one - pinned rows come first - and React moves its node,
   *  which can drop focus to the document. Focus follows the row to where it lands, or to its
   *  group's heading when the row is hidden there, as `IssueBoard`'s `focusAfterMove` lands on a
   *  collapsed rail, so the next `j`/`k` go on from the reader's place rather than from the top
   *  (`useAgentsKeymap` steps from a heading too). The place is the one the row after it moves
   *  into, which takes the focus where the moved row's own control ends out of the reader's
   *  sight. Null while no pin is waiting. */
  const focusAfterPin = useRef<{ readonly place: number; readonly sessionID: string } | null>(null);
  const togglePin = (sessionID: string) => {
    if (
      closestMatching(document.activeElement, AGENT_ROW_SELECTOR)?.dataset.agentRow === sessionID
    ) {
      focusAfterPin.current = {
        place: drawnRows(listRef.current).findIndex((node) => node.dataset.agentRow === sessionID),
        sessionID,
      };
    }
    writePin(sessionID, !pinned.includes(sessionID));
  };
  const select = (sessionID: string, next: boolean) => {
    setSelected((current) => {
      const updated = new Set(current);
      if (next) updated.add(sessionID);
      else updated.delete(sessionID);
      return updated;
    });
  };
  // Every group is open on every load and collapses only while this page stays mounted. A
  // search opens them all, since searching is how a reader finds a session they cannot see:
  // while a query is present the groups read `searchGroups`, reset to open each time a search
  // starts, and clearing the query hands them back to the reader's own state, untouched by the
  // search.
  const [openGroups, setOpenGroups] = useState(everyGroupOpen);
  const searching = search.trim() !== "";
  const [searchGroups, setSearchGroups] = useState(everyGroupOpen);
  const [wasSearching, setWasSearching] = useState(searching);
  if (searching !== wasSearching) {
    setWasSearching(searching);
    if (searching) setSearchGroups(everyGroupOpen);
  }
  const shown = searching ? searchGroups : openGroups;
  const toggleGroup = (group: AgentGroup) => {
    const set = searching ? setSearchGroups : setOpenGroups;
    set((open) => ({ ...open, [group]: !open[group] }));
  };
  const listRef = useRef<HTMLElement>(null);
  // Which rows are open, and which the reader has ever opened. The page holds both rather than
  // each row: the window draws every opened row wherever it has scrolled to (D1), and a row
  // that kept its own answer would have nothing to tell the window.
  const [expandedRows, setExpandedRows] = useState<ReadonlySet<string>>(() => new Set());
  const [openedRows, setOpenedRows] = useState<ReadonlySet<string>>(() => new Set());
  const expandRow = (sessionID: string, open: boolean) => {
    setExpandedRows((current) => {
      const next = new Set(current);
      if (open) next.add(sessionID);
      else next.delete(sessionID);
      return next;
    });
    if (open) {
      setOpenedRows((current) =>
        current.has(sessionID) ? current : new Set(current).add(sessionID)
      );
    }
  };
  /** The row that holds keyboard focus, which the window draws however far it has scrolled: a
   *  row unmounted under the reader drops focus to the document body, which silently turns off
   *  every binding that acts on the row in hand (`keyboard.ts`). */
  const [focusedRow, setFocusedRow] = useState<string | null>(null);
  /** The row `j` or `k` last asked for, until the render that draws it puts focus there: a fresh
   *  object each ask, so the ask renders and the focus lands even when the row it names is the
   *  one `focusedRow` already holds - a focus inside that row named it, and a focus outside the
   *  list since left the name standing. */
  const [focusAsked, setFocusAsked] = useState<{ readonly sessionID: string } | null>(null);
  // One list, keyed by session, with each group's heading as an item between the rows - the
  // Inbox's pattern (`Inbox.tsx`) - of which the window draws a part. A row that moves from one
  // group to another moves within the one parent, so React moves its node instead of remounting
  // it: nothing a row holds is ever handed from one instance to another. One flat array, not one
  // per group: each array in the children is a slot of its own, and a row changing slots would
  // remount.
  const items: ListItem[] = AGENT_GROUPS.flatMap((group) => [
    { group, key: `group:${group}`, kind: "group" as const },
    ...(shown[group] ? matchingIn[group] : []).map((agent) => ({
      agent,
      group,
      key: agent.session_id,
      kind: "row" as const,
    })),
  ]);
  const kept = new Set(openedRows);
  if (focusedRow !== null) kept.add(focusedRow);
  useAgentsKeymap(
    {
      focusRow: (sessionID) => {
        setFocusedRow(sessionID);
        setFocusAsked({ sessionID });
      },
      items: items.map((item) =>
        item.kind === "group"
          ? { group: item.group, kind: "fold" as const }
          : { id: item.key, kind: "row" as const }
      ),
    },
    searchInputRef
  );
  // A layout effect, so the frame that paints the row the keys moved to already has focus on it.
  // The row is drawn because `kept` holds it, and it stays drawn because `focusedRow` does.
  useLayoutEffect(() => {
    if (focusAsked === null) return;
    const row = rowNode(listRef.current, focusAsked.sessionID);
    row?.scrollIntoView({ block: "nearest" });
    row?.focus();
  }, [focusAsked]);
  // A layout effect, so the frame the move paints already has focus where the row went.
  // biome-ignore lint/correctness/useExhaustiveDependencies: `pinned` is the trigger, not a read
  useLayoutEffect(() => {
    const pin = focusAfterPin.current;
    if (pin === null) return;
    focusAfterPin.current = null;
    const { place, sessionID } = pin;
    const row = rowNode(listRef.current, sessionID);
    if (row === null) return;
    // A row the pin leaves hidden is in a collapsed group: it held focus, so the filters match
    // it, and a pin changes neither what they read nor which group the row is in.
    const target = row.hidden ? foldToggleOf(row) : row;
    // Only the focus the move took is the page's to give back: still on the row, now hidden, or
    // dropped to the document.
    const focused = document.activeElement;
    // The move can leave the focus on a control of the row itself, which the browser puts back
    // where the row now stands: measured in WebKit at 390x664, Unpin on the first of three
    // pinned agents left the focus on that row's Pin at 816 px in a 664 px window. A landing is
    // one the reader can see, and nothing scrolls to reach it, so the focus goes with no scroll
    // to the control standing where this one does in the row now at the moved row's place
    // (`controlsLikeRow`), else that row's first control. A control the reader can see whole
    // keeps the focus, so the page still follows the row wherever it stays in sight.
    if (focused instanceof HTMLElement && focused !== row && row.contains(focused)) {
      if (inSightWhole(focused, new Map())) return;
      const at = drawnRows(listRef.current)[place];
      if (at === undefined || at === row) return;
      for (const control of controlsLikeRow(at, focused, row)) {
        if (focusNoScroll(control)) return;
      }
      return;
    }
    if (focused !== row && !focusOnDocument()) return;
    if (focused !== target) target?.focus();
  }, [pinned]);
  const agentRow = (agent: Agent, group: AgentGroup, hidden: boolean) => (
    <AgentRow
      agent={agent}
      expanded={expandedRows.has(agent.session_id)}
      hidden={hidden}
      key={agent.session_id}
      liveAgents={current ? agents : undefined}
      mdViewport={mdViewport}
      needsYou={needsYouBySession.get(agent.session_id) ?? 0}
      onExpand={(open) => expandRow(agent.session_id, open)}
      onPin={() => togglePin(agent.session_id)}
      onSelect={(next) => select(agent.session_id, next)}
      opened={openedRows.has(agent.session_id)}
      pinned={pinned.includes(agent.session_id)}
      section={group}
      selected={selected.has(agent.session_id)}
      unreadReplies={agentStates[agent.session_id]?.unread_replies ?? 0}
      waitingOnAgent={waitingOnAgentBySession.get(agent.session_id) ?? 0}
    />
  );
  // A row the reader has opened that the list no longer holds - its group is closed, or the
  // filters exclude it - stays mounted and hidden among the drawn items, in the same parent and
  // under the same key, so a fold and a filter discard no draft, as a scroll discards none. Its
  // item is the same element a drawn one is, so a row moving between the two is moved rather
  // than remounted, and hidden with its row, so it is no item a screen reader counts.
  const keptAside = AGENT_GROUPS.flatMap((group) =>
    grouped[group]
      .filter((agent) => kept.has(agent.session_id) && !(shown[group] && matches(agent, group)))
      .map((agent) => (
        <li className="absolute inset-x-0" hidden key={agent.session_id}>
          {agentRow(agent, group, true)}
        </li>
      ))
  );
  // A heading or a row, which the windowed box draws at the place it measured for it.
  const renderItem = (item: ListItem) =>
    item.kind === "group" ? (
      <GroupHeading
        agents={matchingIn[item.group]}
        counted={groupingAnswered}
        expanded={shown[item.group]}
        group={item.group}
        onToggle={() => toggleGroup(item.group)}
        selected={selected}
      />
    ) : (
      agentRow(item.agent, item.group, false)
    );

  if (isPending) return <LoadingSkeleton label="Loading agents" />;
  // A refresh that fails keeps the list it has: unmounting every row would throw away what each
  // holds - a draft, a send's refusal, an upload. With no rows there is nothing to keep. A list
  // that is refilling is no failure to report, and the page says so in its place.
  if (isError && agents.length === 0 && !refilling) {
    return <p className={dangerText}>Could not load agents: {error}</p>;
  }

  return (
    <section
      aria-label="Agents"
      onFocus={(event) =>
        setFocusedRow(
          closestMatching(event.target, AGENT_ROW_SELECTOR)?.dataset.agentRow ?? focusedRow
        )
      }
      ref={listRef}
    >
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <h1 className={`text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Agents
        </h1>
        <p className={`mt-1 flex flex-wrap items-center gap-2 text-sm ${textMutedOnCanvas}`}>
          <span>Live Envoy sessions and their Dispatch activity.</span>
          <Link className={`${linkText} ${linkHoverText}`} to="/agents/broadcasts">
            Broadcasts
          </Link>
          <Link className={`${linkText} ${linkHoverText}`} to="/machines">
            Machines
          </Link>
        </p>
      </header>
      {refilling ? (
        <AgentListRefilling className={`mb-3 text-sm ${textSecondaryOnCanvas}`} />
      ) : isError ? (
        <p className={`mb-3 text-sm ${dangerText}`} role="alert">
          Could not refresh agents: {error}
        </p>
      ) : null}
      {/* Only a current list says nobody is connected: one that failed or is refilling keeps
          the rows it had, and an empty one it had says nothing about now. */}
      {agents.length === 0 ? (
        current ? (
          <EmptyState label="Agents empty state" message="No agents are connected." />
        ) : null
      ) : (
        <>
          <AgentFilterBar
            agents={agents}
            filters={filters}
            onFilters={setFilters}
            searchInputRef={searchInputRef}
          />
          <SelectionHeader
            current={current}
            listed={agents}
            matching={matching}
            onClear={() => setSelected(() => new Set())}
            onToggle={() => setSelected((current) => toggleMatching(matching, current))}
            selected={selected}
          />
          {matching.length === 0 && current ? (
            <EmptyState
              label="Agents filtered empty state"
              // A role nothing live holds is not a role Dispatch can show as stopped: the
              // registry lists sessions, and a role is here only while a session carries it.
              // The words say so, rather than letting the reader read the list as every role
              // there is.
              message={
                unheldRoles.length > 0 && unheldRoles.length === filters.roles.length
                  ? `No live session holds ${unheldRoles.join(", ")}. Outside an issue, this list holds only the roles a live session carries; Dispatch keeps no list of every role.`
                  : "No agent matches these filters."
              }
            />
          ) : null}
          <WindowedList items={items} kept={kept} keptAside={keptAside} renderItem={renderItem} />
        </>
      )}
      {/* The sends sit outside the composer and outside the list, so a send's row outlives both:
          the composer goes at every press, and the list empties when no agent is connected. On a
          narrow or short screen only the composer sticks (the wrapper is `contents`), and the
          strip stays at the end of the list: the composer alone fills the screen's budget. */}
      {queue.rows.length === 0 && !showComposer ? null : (
        <div className="sticky bottom-0 z-10 mt-3 space-y-2 narrow-or-short:contents">
          {queue.rows.length === 0 ? null : (
            <BroadcastSends
              className="narrow-or-short:mt-3"
              onDiscard={queue.dismiss}
              onRestore={(row) => {
                queue.dismiss(row);
                if (row.status === "failed") composition.restore(row.send, row.error);
              }}
              onRetry={queue.retry}
              onStop={queue.stop}
              restoreRefusal={restoreRefusal}
              rows={queue.rows}
            />
          )}
          {showComposer ? (
            <BroadcastComposer
              agents={agents}
              body={draft}
              composed={composedBroadcast(
                { ...composition, delivery },
                { agents, current, refilling }
              )}
              defaulted={composition.delivery === null}
              delivery={delivery}
              onBody={composition.setDraft}
              onDelivery={composition.setDelivery}
              onDeselect={(sessionID) => select(sessionID, false)}
              onSend={(send) => {
                queue.enqueue(send);
                composition.sent();
              }}
              selected={selected}
            />
          ) : null}
        </div>
      )}
      {/* A session the list does not hold has ended only when the list is current. */}
      {current ? <EndedAgentsWithReplies live={agents} /> : null}
    </section>
  );
}
