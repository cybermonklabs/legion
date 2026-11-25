import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  type ReactNode,
  type SyntheticEvent,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { flushSync } from "react-dom";

import { api, type WriteBounds } from "../../api/client";
import { askThreadQuery } from "../../api/queries";
import type { Actor, UserState } from "../../api/types";
import { CopyButton } from "../../components/CopyButton";
import {
  pageHeaderControl,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pageQuietText,
  pageRowAct,
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
import { AskEditHistory } from "../inbox/AskCompletionCard";
import { AskHandOn } from "../inbox/AskHandOn";
import { AskRecipients } from "../inbox/AskRecipients";
import { writePin } from "../issue/pin-write";
import { conversationItemId, isPinnedConversation, stateForIssue } from "../issue/pins";
import { type PressWatch, watchPresses } from "../margin/details-focus-keeper";
import { useCommentActionQueue } from "../margin/useCommentActionQueue";
import { type AgentPart, AgentParts, useAgentText } from "../refs/AgentName";
import { standsFailed } from "../refs/reference-target";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { DetailsErrorLine, retryRead, useRetryHandOff } from "./details-error";
import type { FocusedNode } from "./focused-node";
import { recipient } from "./history-model";
import { useMarginDrawer } from "./margin-slot";
import { NodeReferences } from "./node-references/NodeReferences";
import { useOutlineWriter } from "./outline-writer";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  DELIVERY_STATE_WORDS,
  isReader,
  NODE_TYPE_NAMES,
  sentenceCase,
} from "./page-words";
import { useWhoIsReading } from "./useIssueTree";

/**
 * The facts of whatever the reader's focus is on, in the panel: what it is, how it stands and
 * who made it. They draw nothing of what it says, since the row itself is drawn whole beside
 * them; the issue's own facts name its title. They change as the focus moves, because they are
 * read from the focused node itself (`focused-node.tsx`); until the tree lands the focus is the
 * issue, so this draws the issue's own facts rather than nothing.
 *
 * A question holds what the ask card draws around it, each as that card draws it: behind the
 * name of an agent session that asked it, the session's copy handles, which a press on that
 * name opens; its edit history; and who a person's answer and replies reach
 * (`AskThreadFacts`).
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
  const section = useRef<HTMLElement>(null);
  const askId = node.item?.kind === "ask" ? node.item.id : undefined;
  // The session that asked the question the reader is on, whose copy handles stand behind its
  // name; a person who asked one has none, as on the ask card.
  const asker = askId !== undefined && node.author?.kind === "session" ? node.author : undefined;
  // Which row the reader opened the asker's handles on. The focus moving to another row closes
  // them, and a reader coming back to the row finds them closed, as the row's references are.
  const [handlesOn, setHandlesOn] = useState<string | undefined>(undefined);
  if (handlesOn !== undefined && handlesOn !== node.id) setHandlesOn(undefined);
  const handlesOpen = handlesOn === node.id;
  return (
    <section
      className={`border-b pb-2 ${pageLineBorder}`}
      data-focused-node={node.id}
      data-testid="focused-node-facts"
      ref={section}
    >
      <h2 className={`text-sm font-semibold ${pageInkText}`} {...pageChrome}>
        {PROPERTY_LABELS.focused_node}
      </h2>
      <dl className="mt-1 flex min-w-0 flex-col gap-1 text-sm">
        {node.type === "issue" ? (
          <Fact label={sentenceCase(NODE_TYPE_NAMES.issue)} value={node.title} />
        ) : (
          <Fact
            chrome
            label={CONTROL_LABELS.node_kind}
            value={sentenceCase(NODE_TYPE_NAMES[node.type])}
          />
        )}
        {node.state === undefined ? null : (
          <Fact chrome label={PROPERTY_LABELS.status} value={node.state} />
        )}
        {node.author === undefined ? null : asker === undefined ? (
          <Fact label={CONTROL_LABELS.written_by} value={agentText.author(node.author)} />
        ) : (
          <div className={FACT}>
            <FactLabel label={CONTROL_LABELS.written_by} />
            <dd className="flex min-w-0 flex-col items-start gap-1">
              <button
                aria-expanded={handlesOpen}
                className={`dispatch-inline-link min-w-0 break-words text-left ${pageInkText} ${pageRowAct}`}
                onClick={() => setHandlesOn(handlesOpen ? undefined : node.id)}
                type="button"
                {...authorText}
              >
                {agentText.author(asker)}
              </button>
              {handlesOpen ? <AskerHandles asker={asker} /> : null}
            </dd>
          </div>
        )}
      </dl>
      {askId === undefined ? null : (
        <AskThreadFacts
          askId={askId}
          // A Retry whose read answered goes with its line: the focus it held goes to the facts'
          // first control, rather than to the page.
          firstControl={() => section.current?.querySelector<HTMLElement>("button, summary")}
          issueKey={issueKey}
          key={askId}
        />
      )}
      {node.item === undefined ? null : (
        <ItemControls issueKey={issueKey} item={node.item} state={state} />
      )}
      {/* What references the row the reader is on, mentions included: read only when
          the reader opens it, so it costs nothing on load and nothing per row. */}
      <NodeReferences node={node} reference={node.reference} />
    </section>
  );
}

/** The copy handles of the session that asked the question the reader is on, as the ask card
 *  draws them behind its asker's name: the session's ID, the title the session gave itself
 *  where it gave one, and its tmux target where it has one, each with its copy control. Each
 *  control's name is the ask card's, "Copy session ID …". */
function AskerHandles({ asker }: { asker: Actor }): ReactNode {
  const title = asker.origin?.session_title?.trim();
  const tmux = asker.origin?.tmux;
  return (
    <span className="flex min-w-0 flex-wrap items-center gap-x-1.5">
      <CopyButton value={asker.id} what="session ID">
        <span {...pageChrome}>{CONTROL_LABELS.session_id}</span>
      </CopyButton>
      {title === undefined || title === "" ? null : (
        <CopyButton value={title} what="session title">
          <span {...pageChrome}>{CONTROL_LABELS.session_title}</span>
        </CopyButton>
      )}
      {tmux === undefined || tmux === "" ? null : (
        <CopyButton value={tmux} what="tmux target">
          <span {...authorText}>{tmux}</span>
        </CopyButton>
      )}
    </span>
  );
}

/**
 * What the question the reader is on holds beside its controls, each as the ask card draws it:
 * its edit history (`AskEditHistory`), and who a person's answer and replies reach
 * (`AskRecipients`): its followers, the session holding the issue's claim, and its route. They
 * are the card's own parts in the card's own words, so they are marked as text the page did not
 * write, as the hand-on control is (`ItemControls`).
 *
 * Both are drawn from the question's thread. Where the page holds the issue's questions read
 * with each one's thread (`GET /api/v1/issues/{key}/asks?thread=1`, under `["asks", key,
 * "thread"]`), the thread is that read's, watched and never asked for, so it costs no request.
 * Where it does not, the thread is the question's own read (`askThreadQuery`,
 * `GET /api/v1/asks/{id}`), sent only while Details is open, and once for each question: it
 * never goes stale on its own, and an event that changes the question reads it again
 * (`api/event-refresh.ts`). It is asked again as every read of the app is (`queryRetry`), and a
 * read that still failed is said with Retry, as Details says each of its failed reads.
 */
function AskThreadFacts({
  askId,
  firstControl,
  issueKey,
}: {
  askId: string;
  /** The control the focus of a Retry that went with its answered read goes to. */
  firstControl: () => HTMLElement | null | undefined;
  issueKey: string;
}): ReactNode {
  // Outside the shell no drawer holds the panel and it is always open; on the issue page it is
  // the Details drawer, which opens shut.
  const drawer = useMarginDrawer();
  const open = drawer === undefined || drawer.open;
  const threads = useQuery({
    enabled: false,
    queryFn: () => api.listIssueAskThreads(issueKey, "all"),
    queryKey: ["asks", issueKey, "thread"],
  });
  const held = threads.data?.find((row) => row.id === askId);
  const own = useQuery({ ...askThreadQuery(askId), enabled: open && held === undefined });
  const ask = held ?? own.data?.ask;
  const thread = held?.thread ?? own.data;
  // Retry is never disabled while it reads again, since Chromium drops the focus of a focused
  // button that turns disabled to the page, so a read that fails again leaves the focus on it; a
  // press while the question is read joins that read (`retryRead`).
  const retry = useRetryHandOff(firstControl);
  return (
    <>
      {held === undefined && standsFailed(own) ? (
        <div className="mt-2">
          <DetailsErrorLine
            error={own.error}
            failure={own.errorUpdateCount}
            message={ANSWER_LINES.ask_thread_unreadable}
            onRetry={retryRead(own)}
            {...retry}
            retrying={own.isFetching}
          />
        </div>
      ) : null}
      {ask === undefined || thread === undefined ? null : (
        <div {...authorText}>
          <AskEditHistory ask={ask} edits={thread.edits ?? []} />
          <AskRecipients
            askId={ask.id}
            claimant={thread.claimant}
            followers={thread.followers ?? []}
            owner={ask}
            route={thread.route}
            routeHolder={thread.route_holder}
            routeStatus={thread.route_status}
          />
        </div>
      )}
    </>
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
  const viewer = useWhoIsReading().data?.login;
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
  // The items whose action Dispatch has taken while the read of the comments that action set off
  // is still out, once for each such read. A press is answered once the page shows what it did
  // (`watchPresses`), so the item's controls stay busy until that read has landed.
  const [reading, setReading] = useState<readonly string[]>([]);
  const actions = useCommentActionQueue({
    onSuccess: ({ id }) => {
      // Drawn before the queue draws the action as no longer out, so that the controls never
      // read idle between the two.
      flushSync(() => setReading((ids) => [...ids, id]));
      void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] }).finally(() =>
        setReading((ids) => {
          const landed = ids.indexOf(id);
          return ids.filter((_, index) => index !== landed);
        })
      );
    },
  });
  const retry = useKeyedWrite({
    label: () => CONTROL_LABELS.retry_delivery,
    method: "POST",
    // The write stays out until the reads it set off have landed, failed or not, so that its
    // Retry reads busy until the page shows what it did.
    onSuccess: () =>
      Promise.all([
        queryClient.invalidateQueries({ queryKey: ["comments", issueKey] }),
        queryClient.invalidateQueries({ queryKey: ["issue-messages", issueKey] }),
      ]),
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
  const pending = actions.pendingActionIds.has(item.id) || reading.includes(item.id);
  // A control whose write is out, or the read that write set off, stays enabled, marked busy, as
  // the margin's thread cards keep theirs (`ThreadCard.tsx`): Chromium drops the focus of a
  // focused button that turns disabled to the page, and enabling it again does not bring it
  // back. A repeat press while the write is out does nothing: the queue ignores a repeat of the
  // action it is sending (`useCommentActionQueue`), and a delivery's Retry returns while its
  // write, or the reads that write set off, are out.
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
  // The watch of the reader's presses (`watchPresses`, the rule of which loss is theirs) on the
  // control of these the focus is on (`onFocus` below). Accept and Reject go once the
  // suggestion is decided, a delivery's Retry once a Retry could deliver it twice, and a failure's
  // Retry once what it sent again is out. Where the reader's own press of one takes it away, the
  // focus would fall to the page with it: it goes to the thread's Resolve or Reopen instead, else
  // to Pin, as Unpin in Pinned hands it on (`IssuePins.tsx`). Pin, Resolve and Reopen, and the
  // hand-on, stay drawn when pressed, and none of them is disabled. A control that goes while no
  // press of the reader's on it stands - the suggestion decided in another session, a delivery's
  // Retry whose time ran out, the question answered elsewhere - leaves the focus on the page's
  // body, as on every page of Dispatch, and nothing here takes it.
  const held = useRef<PressWatch | null>(null);
  const hear = (event: SyntheticEvent) => held.current?.hear(event.nativeEvent);
  useEffect(() => () => held.current?.stop(), []);
  useLayoutEffect(() => {
    const lost = held.current;
    if (lost === null || lost.control.isConnected) return;
    held.current = null;
    const caused = lost.stands();
    lost.stop();
    if (caused && (document.activeElement === null || document.activeElement === document.body)) {
      (threadControl.current ?? pinControl.current)?.focus();
    }
  });
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handlers only note which of these controls the focus is on and whether the reader pressed it, so they need no role of their own.
    <div
      className="mt-2 flex min-w-0 flex-col gap-1"
      onClick={hear}
      onFocus={(event) => {
        // The focus come back to the control it was on, as the window is focused again, keeps the
        // reader's press of it.
        if (held.current?.control === event.target) return;
        held.current?.stop();
        held.current = watchPresses(event.target);
      }}
      onKeyDown={hear}
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

/** The line of one fact: its name, then its value. */
const FACT = "flex min-w-0 flex-wrap items-baseline gap-2";

/** One fact: its name in the page's words, its value in whoever's words it came from. A kind
 *  and a state word are the page's own (`NODE_TYPE_NAMES`, `NODE_STATE_WORDS`); a title and an
 *  author's name are not. */
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
    <div className={FACT}>
      <FactLabel label={label} />
      <dd className="min-w-0 break-words" {...(chrome ? pageChrome : authorText)}>
        {value}
      </dd>
    </div>
  );
}

/** A fact's name, in the page's words. */
function FactLabel({ label }: { label: string }): ReactNode {
  return (
    <dt className={`font-medium ${pageQuietText}`} {...pageChrome}>
      {label}:
    </dt>
  );
}
