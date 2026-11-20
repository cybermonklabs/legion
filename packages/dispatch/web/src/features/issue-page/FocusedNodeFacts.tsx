import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useLayoutEffect, useRef } from "react";

import { api, type WriteBounds } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { UserState } from "../../api/types";
import {
  pageHeaderControl,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pageQuietText,
} from "../../theme/classes";
import { storedMessages } from "../agent-view/stored-messages";
import {
  type DeliveryOutcome,
  deliveryMode,
  deliveryOutcomeState,
  duplicateText,
  failureGuidance,
  isSafeRetry,
  queuedParts,
  rowAnsweredWithError,
  withGuidance,
} from "../conversation/delivery";
import { AskHandOn } from "../inbox/AskHandOn";
import { writePin } from "../issue/pin-write";
import { conversationItemId, isPinnedConversation, stateForIssue } from "../issue/pins";
import { useCommentActionQueue } from "../margin/useCommentActionQueue";
import { type AgentPart, AgentParts, useAgentText } from "../refs/AgentName";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { DetailsErrorLine } from "./details-error";
import type { FocusedNode } from "./focused-node";
import { recipient } from "./history-model";
import { NodeReferences } from "./node-references/NodeReferences";
import { useOutlineWriter } from "./outline-writer";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import {
  CONTROL_LABELS,
  DELIVERY_STATE_WORDS,
  isReader,
  NODE_TYPE_NAMES,
  sentenceCase,
} from "./page-words";

/**
 * The facts of whatever the reader's focus is on, in the panel:
 * what it is, how it stands, who made it and what it says. They change as the focus moves,
 * because they are read from the focused node itself (`focused-node.tsx`); until the tree lands
 * the focus is the issue, so this draws the issue's own facts rather than nothing.
 *
 * Under them, what the reader can do to the node that is no reply - the old conversation's card
 * controls, which the panel's list took with it when the outline became the conversation:
 * the pin of an ask, a comment or a message, which the panel's list of pins then
 * holds; on an open question, who it is asked of and the control that hands it to someone
 * else, with who handed it on and when; on a comment that heads a thread its Resolve or Reopen,
 * and Accept and Reject on a suggestion not yet decided. They are ordinary controls of Details,
 * so Tab reaches each from the row's own facts; Reply and Answer are the row's own
 * (`NodeRow.tsx`).
 */
export function FocusedNodeFacts({
  issueKey,
  node,
  state,
}: {
  issueKey: string;
  node: FocusedNode;
  state: UserState | undefined;
}): ReactNode {
  const agentText = useAgentText();
  return (
    <section
      className={`border-b pb-2 ${pageLineBorder}`}
      data-focused-node={node.id}
      data-testid="focused-node-facts"
    >
      <h2 className={`text-sm font-semibold ${pageInkText}`} {...pageChrome}>
        {PROPERTY_LABELS.focused_node}
      </h2>
      <dl className="mt-1 flex min-w-0 flex-col gap-1 text-sm">
        <Fact label={sentenceCase(NODE_TYPE_NAMES[node.type])} value={node.title} />
        {node.state === undefined ? null : (
          <Fact chrome label={PROPERTY_LABELS.status} value={node.state} />
        )}
        {node.author === undefined ? null : (
          <Fact label={CONTROL_LABELS.written_by} value={agentText.author(node.author)} />
        )}
      </dl>
      {node.item === undefined ? null : (
        <ItemControls issueKey={issueKey} item={node.item} state={state} />
      )}
      {/* What references the row the reader is on, mentions included: read only when
          the reader opens it, so it costs nothing on load and nothing per row. */}
      <NodeReferences node={node} reference={node.reference} />
    </section>
  );
}

/** One attempt to deliver a comment's mention, or a message, to an agent: who it went to, as
 *  the history names them (`recipient`), how it stands, how a Retry sends it again in its own
 *  mode, and whose guidance a failure reads (`failureGuidance`): a mention's, or a message's
 *  own card's. */
interface Delivery extends DeliveryOutcome {
  readonly error: string | null;
  readonly key: string;
  readonly recipient: AgentPart;
  readonly retry: (bounds: WriteBounds) => Promise<unknown>;
  readonly surface: "card" | "mention";
}

/** The pin of an ask, a comment or a message; an open question's hand-on; a thread head's Resolve
 *  or Reopen, Accept and Reject; the Edit of the reader's own comment; and each delivery to an
 *  agent with its Retry. */
function ItemControls({
  issueKey,
  item,
  state,
}: {
  issueKey: string;
  item: NonNullable<FocusedNode["item"]>;
  state: UserState | undefined;
}): ReactNode {
  const queryClient = useQueryClient();
  const writer = useOutlineWriter();
  const viewer = useQuery(whoAmIQuery()).data?.login;
  // The reads the outline is drawn from, under its keys, so the controls cost no request: never
  // stale here, since the outline's own reads and every invalidation keep them current, so
  // the focus landing on a row reads the cache however long ago the outline read it.
  const comments = useQuery({
    enabled: item.kind === "comment",
    queryFn: () => api.listComments(issueKey),
    queryKey: ["comments", issueKey],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const messages = useQuery({
    enabled: item.kind === "message",
    queryFn: () => api.listIssueMessages(issueKey),
    queryKey: ["issue-messages", issueKey],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const asks = useQuery({
    enabled: item.kind === "ask",
    queryFn: () => api.listIssueAsks(issueKey, "all"),
    queryKey: ["asks", issueKey],
    staleTime: Number.POSITIVE_INFINITY,
  });
  const actions = useCommentActionQueue({
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] }),
  });
  const retry = useKeyedWrite({
    label: () => CONTROL_LABELS.retry_delivery,
    method: "POST",
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] });
      void queryClient.invalidateQueries({ queryKey: ["issue-messages", issueKey] });
    },
    write: (delivery: Delivery, bounds) => delivery.retry(bounds),
  });
  const pinId = conversationItemId(item.kind, item.id);
  const pinned = isPinnedConversation(stateForIssue(state, issueKey).dismissed ?? [], pinId);
  const comment =
    item.kind === "comment" ? comments.data?.find((one) => one.id === item.id) : undefined;
  const message =
    item.kind === "message"
      ? storedMessages(messages.data ?? []).find((one) => one.id === item.id)
      : undefined;
  const openAsk =
    item.kind === "ask"
      ? asks.data?.find((one) => one.id === item.id && one.state === "open")
      : undefined;
  const head = comment !== undefined && comment.reply_to === null;
  const undecided = comment?.suggestion !== null && comment?.suggestion?.accepted === null;
  // The reader's own words by the page's one rule, as the outline's row tells them.
  const own = isReader(comment?.author, viewer);
  const pending = actions.pendingActionIds.has(item.id);
  // A control whose write is out stays enabled, marked busy, as the margin's thread cards keep
  // theirs (`ThreadCard.tsx`): Chromium drops the focus of a focused button that turns disabled
  // to the page, and enabling it again does not bring it back. A repeat press does nothing:
  // the queue ignores a repeat of the action it is sending (`useCommentActionQueue`), and a
  // delivery's Retry returns while one is out.
  const control = `${pageHeaderControl} aria-busy:opacity-50`;
  const deliveries: Delivery[] = [
    ...(comment?.deliveries ?? []).map((row) => {
      const mode = deliveryMode(row.delivery) ?? "steer";
      return {
        answeredWithError: rowAnsweredWithError(row),
        createdAt: row.created_at,
        duplicate: row.duplicate,
        error: row.error,
        key: `${row.target}:${row.attempt}`,
        recipient: recipient(row.session_id, undefined, row.target),
        retry: (bounds: WriteBounds) =>
          api.createCommentDelivery(row.comment_id, row.target, mode, bounds),
        state: deliveryOutcomeState(row.state),
        surface: "mention" as const,
      };
    }),
    ...(message?.deliveries ?? []).map((row, index) => {
      const mode = deliveryMode(row.delivery) ?? "steer";
      return {
        answeredWithError: rowAnsweredWithError(row),
        createdAt: row.created_at,
        duplicate: row.duplicate,
        error: row.error,
        key: `${row.message_id}:${index}`,
        recipient: recipient(row.session_id, undefined, message?.target ?? undefined),
        retry: (bounds: WriteBounds) => api.createMessageDelivery(row.message_id, mode, bounds),
        state: deliveryOutcomeState(row.state),
        surface: "card" as const,
      };
    }),
  ];
  const pinControl = useRef<HTMLButtonElement>(null);
  const threadControl = useRef<HTMLButtonElement>(null);
  // The control of these the focus was last on (`onFocus` below). Accept and Reject go once the
  // suggestion is decided, a delivery's Retry once a Retry could deliver it twice, and a
  // failure's Retry once what it sent again is out, each under the reader's own press, and the
  // focus would fall to the page with them: it goes to the thread's Resolve or Reopen instead,
  // else to Pin, as Unpin in Pinned hands it on (`IssuePins.tsx`). Pin, Resolve and Reopen, and
  // the hand-on, stay drawn when pressed, and none of them is disabled.
  const held = useRef<Element | null>(null);
  useLayoutEffect(() => {
    if (held.current === null || held.current.isConnected) return;
    held.current = null;
    if (document.activeElement === null || document.activeElement === document.body) {
      (threadControl.current ?? pinControl.current)?.focus();
    }
  });
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handler only notes which of these controls the focus is on, so they need no role of their own.
    <div
      className="mt-2 flex min-w-0 flex-col gap-1"
      onFocus={(event) => {
        held.current = event.target;
      }}
    >
      <p className="flex flex-wrap items-center gap-2" data-focused-controls="">
        <button
          aria-pressed={pinned}
          className={control}
          onClick={() =>
            writePin(queryClient, issueKey, (dismissed) => ({
              id: pinId,
              op: isPinnedConversation(dismissed, pinId) ? "unpin" : "pin",
            }))
          }
          ref={pinControl}
          type="button"
          {...pageChrome}
        >
          {pinned ? CONTROL_LABELS.unpin : CONTROL_LABELS.pin}
        </button>
        {head && comment !== undefined ? (
          <button
            aria-busy={pending || undefined}
            className={control}
            onClick={() =>
              actions.mutateItem({ id: item.id, kind: comment.resolved ? "reopen" : "resolve" })
            }
            ref={threadControl}
            type="button"
            {...pageChrome}
          >
            {comment.resolved ? CONTROL_LABELS.reopen_thread : CONTROL_LABELS.resolve}
          </button>
        ) : null}
        {head && undecided ? (
          <>
            <button
              aria-busy={pending || undefined}
              className={control}
              onClick={() => actions.mutateItem({ id: item.id, kind: "accept" })}
              type="button"
              {...pageChrome}
            >
              {CONTROL_LABELS.accept}
            </button>
            <button
              aria-busy={pending || undefined}
              className={control}
              onClick={() => actions.mutateItem({ id: item.id, kind: "reject" })}
              type="button"
              {...pageChrome}
            >
              {CONTROL_LABELS.reject}
            </button>
          </>
        ) : null}
        {own && comment !== undefined && writer !== undefined ? (
          <button
            className={control}
            onClick={() => writer.edit(comment.id, comment.body)}
            type="button"
            {...pageChrome}
          >
            {CONTROL_LABELS.edit}
          </button>
        ) : null}
      </p>
      {/* The ask card's own control, as the old conversation drew it on this page, in the card's
          words: the issue's assignee is no name the panel holds, so it is offered under its
          login among everyone else, as on every surface but the Inbox. */}
      {openAsk === undefined ? null : (
        <div {...authorText}>
          <AskHandOn ask={openAsk} assignee={null} />
        </div>
      )}
      {deliveries.length === 0 ? null : (
        <ul className={`flex flex-col gap-0.5 text-xs ${pageMutedText}`} data-deliveries="">
          {deliveries.map((delivery) => {
            const guidance = failureGuidance(delivery, delivery.surface);
            return (
              <li
                className="flex flex-wrap items-center gap-x-2"
                key={delivery.key}
                {...authorText}
              >
                <span>
                  <AgentParts parts={[delivery.recipient]} /> ·{" "}
                  {/* A queued attempt waits for a session that is not running, which is what
                      the sender acts on, so it is read before a duplicate. */}
                  {delivery.state === "queued" ? (
                    <AgentParts parts={queuedParts(delivery.recipient)} />
                  ) : delivery.duplicate === true ? (
                    duplicateText
                  ) : (
                    <span {...pageChrome}>{DELIVERY_STATE_WORDS[delivery.state]}</span>
                  )}
                  {delivery.error === null
                    ? ""
                    : ` · ${guidance === undefined ? delivery.error : withGuidance(delivery.error, guidance)}`}
                </span>
                {isSafeRetry(delivery) ? (
                  <button
                    aria-busy={retry.isPending || undefined}
                    className={control}
                    onClick={() => {
                      if (!retry.isPending) retry.mutate(delivery);
                    }}
                    type="button"
                    {...pageChrome}
                  >
                    {CONTROL_LABELS.retry}
                  </button>
                ) : null}
              </li>
            );
          })}
        </ul>
      )}
      {actions.actionFailure?.id === item.id ? (
        <div {...authorText}>
          <DetailsErrorLine
            message={actions.actionFailure.message}
            onRetry={actions.actionFailure.retryable ? actions.retryItem : undefined}
          />
        </div>
      ) : null}
    </div>
  );
}

/** One fact: its name in the page's words, its value in whoever's words it came from. A state
 *  word is the page's own (`NODE_STATE_WORDS`); a title and an author's name are not. */
function Fact({
  chrome = false,
  label,
  value,
}: {
  chrome?: boolean;
  label: string;
  value: string;
}): ReactNode {
  return (
    <div className="flex min-w-0 flex-wrap items-baseline gap-2">
      <dt className={`font-medium ${pageQuietText}`} {...pageChrome}>
        {label}:
      </dt>
      <dd className="min-w-0 break-words" {...(chrome ? pageChrome : authorText)}>
        {value}
      </dd>
    </div>
  );
}
