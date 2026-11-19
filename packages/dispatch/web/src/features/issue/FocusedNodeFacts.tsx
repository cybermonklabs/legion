import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api, type WriteBounds } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { UserState } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  pageHeaderControl,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
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
import { AskHandOn } from "../inbox/AskCard";
import { useCommentActionQueue } from "../margin/useCommentActionQueue";
import { AgentParts, useAgentText } from "../refs/AgentName";
import { sessionOfAddress } from "../refs/routes";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import type { FocusedNode } from "./focused-node";
import { NodeReferences } from "./node-references/NodeReferences";
import { useOutlineWriter } from "./outline-writer";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS, DELIVERY_STATE_WORDS, NODE_TYPE_NAMES, sentenceCase } from "./page-words";
import { writePin } from "./pin-write";
import { conversationItemId, isPinnedConversation, stateForIssue } from "./pins";

/**
 * The facts of whatever the reader's focus is on, in the panel (dispatch://INFRA-1196 R12, R14):
 * what it is, how it stands, who made it and what it says. They change as the focus moves,
 * because they are read from the focused node itself (`focused-node.tsx`); until the tree lands
 * the focus is the issue, so this draws the issue's own facts rather than nothing.
 *
 * Under them, what the reader can do to the node that is no reply - the old conversation's card
 * controls, which the panel's list took with it when the outline became the conversation
 * (R14, R19): the pin of an ask, a comment or a message, which the panel's list of pins then
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
      className={`border-b pb-2 ${borderDefault}`}
      data-focused-node={node.id}
      data-testid="focused-node-facts"
    >
      <h2 className={`text-sm font-semibold ${textPrimaryOnSurface}`} {...pageChrome}>
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
      {/* What references the row the reader is on, mentions included (R14, R19): read only when
          the reader opens it, so it costs nothing on load and nothing per row (R21). */}
      <NodeReferences node={node} reference={node.reference} />
    </section>
  );
}

/** One attempt to deliver a comment's mention, or a message, to an agent: who it went to, how it
 *  stands, and how a Retry sends it again in its own mode. */
interface Delivery extends DeliveryOutcome {
  readonly error: string | null;
  readonly key: string;
  readonly retry: (bounds: WriteBounds) => Promise<unknown>;
  readonly target: string | null;
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
  // The reads the outline is drawn from, under its keys, so the controls cost no request.
  const comments = useQuery({
    enabled: item.kind === "comment",
    queryFn: () => api.listComments(issueKey),
    queryKey: ["comments", issueKey],
  });
  const messages = useQuery({
    enabled: item.kind === "message",
    queryFn: () => api.listIssueMessages(issueKey),
    queryKey: ["issue-messages", issueKey],
  });
  const asks = useQuery({
    enabled: item.kind === "ask",
    queryFn: () => api.listIssueAsks(issueKey, "all"),
    queryKey: ["asks", issueKey],
  });
  const actions = useCommentActionQueue({
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] }),
  });
  const retry = useKeyedWrite({
    label: () => "Delivery to an agent again",
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
      ? (messages.data ?? [])
          .flatMap((thread) => [thread.message, ...(thread.replies ?? [])])
          .find((one) => one.id === item.id)
      : undefined;
  const openAsk =
    item.kind === "ask"
      ? asks.data?.find((one) => one.id === item.id && one.state === "open")
      : undefined;
  const head = comment !== undefined && comment.reply_to === null;
  const undecided = comment?.suggestion !== null && comment?.suggestion?.accepted === null;
  const own =
    comment !== undefined && comment.author.kind === "user" && comment.author.id === viewer;
  const pending = actions.pendingActionIds.has(item.id);
  const control = `${pageHeaderControl} disabled:opacity-50`;
  const deliveries: Delivery[] = [
    ...(comment?.deliveries ?? []).map((row) => {
      const mode = deliveryMode(row.delivery) ?? "steer";
      return {
        answeredWithError: rowAnsweredWithError(row),
        createdAt: row.created_at,
        duplicate: row.duplicate,
        error: row.error,
        key: `${row.target}:${row.attempt}`,
        retry: (bounds: WriteBounds) =>
          api.createCommentDelivery(row.comment_id, row.target, mode, bounds),
        state: deliveryOutcomeState(row.state),
        target: row.target,
      };
    }),
    ...(message?.deliveries ?? []).map((row, index) => {
      const mode = deliveryMode(row.delivery) ?? "steer";
      return {
        answeredWithError: rowAnsweredWithError(row),
        createdAt: row.created_at,
        duplicate: row.duplicate,
        error: row.error,
        key: `${message?.id ?? ""}:${index}`,
        retry: (bounds: WriteBounds) => api.createMessageDelivery(message?.id ?? "", mode, bounds),
        state: deliveryOutcomeState(row.state),
        target: row.session_id === "" ? (message?.target ?? null) : `session:${row.session_id}`,
      };
    }),
  ];
  return (
    <div className="mt-2 flex min-w-0 flex-col gap-1">
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
          type="button"
          {...pageChrome}
        >
          {pinned ? CONTROL_LABELS.unpin : CONTROL_LABELS.pin}
        </button>
        {head && comment !== undefined ? (
          <button
            className={control}
            disabled={pending}
            onClick={() =>
              actions.mutateItem({ id: item.id, kind: comment.resolved ? "reopen" : "resolve" })
            }
            type="button"
            {...pageChrome}
          >
            {comment.resolved ? CONTROL_LABELS.reopen_thread : CONTROL_LABELS.resolve}
          </button>
        ) : null}
        {head && undecided ? (
          <>
            <button
              className={control}
              disabled={pending}
              onClick={() => actions.mutateItem({ id: item.id, kind: "accept" })}
              type="button"
              {...pageChrome}
            >
              {CONTROL_LABELS.accept}
            </button>
            <button
              className={control}
              disabled={pending}
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
        <ul className={`flex flex-col gap-0.5 text-xs ${textMutedOnSurface}`} data-deliveries="">
          {deliveries.map((delivery) => {
            const guidance = failureGuidance(delivery, "mention");
            const recipient =
              delivery.target === null
                ? "agent"
                : (() => {
                    const sessionId = sessionOfAddress(delivery.target);
                    return sessionId === undefined
                      ? delivery.target
                      : { sessionId, storedTitle: undefined };
                  })();
            return (
              <li
                className="flex flex-wrap items-center gap-x-2"
                key={delivery.key}
                {...authorText}
              >
                <span>
                  <AgentParts parts={[recipient]} /> ·{" "}
                  {/* A queued attempt waits for a session that is not running, which is what
                      the sender acts on, so it is read before a duplicate. */}
                  {delivery.state === "queued" ? (
                    <AgentParts parts={queuedParts(recipient)} />
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
                    className={control}
                    disabled={retry.isPending}
                    onClick={() => retry.mutate(delivery)}
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
          <QueryError
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
      <dt className={`font-medium ${textMutedOnSurface}`} {...pageChrome}>
        {label}:
      </dt>
      <dd className="min-w-0 break-words" {...(chrome ? pageChrome : authorText)}>
        {value}
      </dd>
    </div>
  );
}
