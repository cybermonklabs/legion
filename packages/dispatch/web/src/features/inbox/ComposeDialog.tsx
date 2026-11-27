import { type MutationKey, useQuery } from "@tanstack/react-query";
import { type ReactNode, type RefObject, useLayoutEffect, useRef, useState } from "react";

import { api } from "../../api/client";
import { agentConversationQuery, broadcastQuery } from "../../api/queries";
import type {
  Agent,
  BroadcastCreated,
  Comment,
  CommentDelivery,
  Message,
  MessageDelivery,
} from "../../api/types";
import { ARM_ON_PRESS, causedByReader, focusLost } from "../../components/focus-place";
import { useHeldThroughAnswer } from "../../components/held-press";
import { MultiSelect } from "../../components/MultiSelect";
import { QueryError } from "../../components/QueryError";
import { useSending } from "../../hooks/useSending";
import {
  backdrop50,
  borderDefault,
  card,
  dangerText,
  primaryButtonBg,
  primaryButtonEnabledHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentMessageComposer } from "../agents/AgentMessageComposer";
import { BroadcastComposer } from "../agents/BroadcastComposer";
import { compositionDelivery, useBroadcastComposition } from "../agents/broadcast-composition";
import { type BroadcastRequest, composedBroadcast } from "../agents/broadcast-plan";
import {
  type BroadcastSend,
  broadcastWrite,
  defineBroadcastKind,
  keptBroadcastInput,
} from "../agents/broadcast-queue";
import { deliveryOutcomeState, queuedWait } from "../conversation/delivery";
import { DiscardDraftPrompt } from "../conversation/MentionComposer";
import { KEYED_SEND_LATE_TEXT } from "../conversation/send-request";
import { useAgents } from "../conversation/useAgents";
import {
  AgentName,
  AgentUnresponsiveNote,
  UNRESPONSIVE_KEEPS_MESSAGES,
  useAgentText,
} from "../refs/AgentName";
import { commentQuery } from "../refs/reference-target";
import { useDialog } from "../shell/useDialog";
import { StopTrying } from "../writes/StopTrying";
import { type Shape, shape } from "../writes/tab-storage";
import { ofKind, useWriteSelection, useWrites } from "../writes/write-store";

/** The name of the dialog's send to one session: `agentSendKey`'s shape with the dialog named in
 *  it, so a send from here never takes or clears the Agents row's box (`agentSendKey`) or the
 *  Inbox reading pane's Agent reply box (`threadSendKey`), and no hold of theirs reads it. */
export function composeSendKey(sessionID: string): MutationKey {
  return ["agent-composer", "inbox-compose", sessionID];
}

/** A broadcast the dialog sends, as the write store keeps it across a reload. */
interface ComposedSend {
  readonly input: BroadcastRequest;
}

/**
 * The dialog's broadcast: the Agents page's broadcast write (`broadcastWrite`), on the same line,
 * under a kind of the dialog's own (`defineBroadcastKind`), so the Agents page's strip
 * (`broadcastRows`) never lists it and its Retry, Restore draft and move to the broadcast's page
 * stay that page's alone. The open dialog shows the write itself - Send held while it is out, its
 * refusal beside Send - so a refusal of a write it ran leaves the store at once (`inline`). A write
 * a reload took up is no longer inline, since the dialog that ran it has gone: a refusal of it
 * stays in the store, with Retry and Discard, until the reader takes one. A reload makes it again
 * under its key, and Dispatch answers the repeat with what the first made.
 */
const composeBroadcast = defineBroadcastKind<ComposedSend>({
  kept: shape.record({ input: keptBroadcastInput }) satisfies Shape<ComposedSend>,
  name: "inbox-compose-broadcast",
});

/** What became of the message to one recipient, as the dialog says it. */
type Outcome =
  | { readonly kind: "sent" }
  | { readonly kind: "stored" }
  | { readonly kind: "on its way" }
  | { readonly kind: "failed"; readonly error: string }
  | { readonly kind: "left out"; readonly reason: string };

/** The words of an outcome. A session that is not running gets the message if it runs again
 *  while the stream still keeps it, which is what Dispatch stores it for: said in the words the
 *  Agents page gives a queued attempt (`queuedWait`), with their time limit. */
function outcomeWords(outcome: Outcome): string {
  switch (outcome.kind) {
    case "sent":
      return "Sent";
    case "stored":
      return `Stored for later: the session ${queuedWait}`;
    case "on its way":
      return "On its way";
    case "failed":
      return `Failed: ${outcome.error}`;
    case "left out":
      return `Left out: ${outcome.reason}`;
  }
}

/** The outcome of the newest attempt of a delivery, none yet being a message still on its way. */
function attemptOutcome(
  attempt: Pick<MessageDelivery | CommentDelivery, "error" | "state"> | undefined
): Outcome {
  if (attempt === undefined) return { kind: "on its way" };
  switch (deliveryOutcomeState(attempt.state)) {
    case "sent":
      return { kind: "sent" };
    case "queued":
      return { kind: "stored" };
    case "failed":
      return { error: attempt.error ?? "no reason was recorded", kind: "failed" };
    case "pending":
      return { kind: "on its way" };
  }
}

/** A send the server took: to one session, as a message or as the comment on the issue picked
 *  that mentions it, or to several, as one broadcast. `null` is a landing with no answer. */
type Sent =
  | { readonly kind: "one"; readonly sessionId: string; readonly answer: Message | Comment | null }
  | {
      readonly kind: "broadcast";
      readonly answer: BroadcastCreated | null;
      /** The recipients as the dialog listed them at Send, in that order. */
      readonly order: readonly string[];
      /** Each recipient the send's own plan left out, with its reason: one that does not
       *  advertise the mode the broadcast went in. */
      readonly planned: ReadonlyMap<string, string>;
    };

/** Whether a send to one session answered with the comment that mentions it, rather than with the
 *  message to it: only a comment carries its mentions. */
function isComment(answer: Message | Comment): answer is Comment {
  return "mentions" in answer;
}

/** One recipient's outcome, with the session it is about. */
interface OutcomeRow {
  readonly outcome: Outcome;
  readonly sessionId: string;
}

/**
 * Each recipient's outcome, drawn where the form was, with Done first (`doneRef`, which the
 * dialog's hand-off focuses). It is read from the answer and then from the send's own read, which
 * delivery events refresh: `GET /api/v1/broadcasts/{id}` for a broadcast,
 * `GET /api/v1/messages/{id}` for a message to one session, and `GET /api/v1/comments/{id}` for
 * the comment that mentions it where an issue was picked, that session's delivery of it. A send
 * to one session is no broadcast, so it never reads left out. A read that fails keeps the
 * answer's outcomes and says so beside them.
 */
function SentOutcomes({
  doneRef,
  onDone,
  sent,
}: {
  doneRef: RefObject<HTMLButtonElement | null>;
  onDone: () => void;
  sent: Sent;
}): ReactNode {
  const one = sent.kind === "one" ? sent : undefined;
  const oneAnswer = one?.answer ?? null;
  const messageId = oneAnswer !== null && !isComment(oneAnswer) ? oneAnswer.id : undefined;
  const commentId = oneAnswer !== null && isComment(oneAnswer) ? oneAnswer.id : undefined;
  const broadcastId = sent.kind === "broadcast" ? sent.answer?.id : undefined;
  // Keyed under the session's conversation, which every event of an issue-less message to that
  // session refreshes, so a delivery receipt redraws the outcome with no poll.
  const message = useQuery({
    enabled: messageId !== undefined,
    queryFn: () => api.getMessageThread(messageId ?? ""),
    queryKey: [...agentConversationQuery(one?.sessionId ?? "").queryKey, "message", messageId],
  });
  const comment = useQuery({ ...commentQuery(commentId), enabled: commentId !== undefined });
  // The Agents page's broadcast page reads the same key (`broadcastQuery`).
  const broadcast = useQuery({
    ...broadcastQuery(broadcastId ?? ""),
    enabled: broadcastId !== undefined,
  });

  let rows: OutcomeRow[];
  if (sent.kind === "broadcast") {
    const excluded = new Map(
      (sent.answer?.excluded ?? []).map((item) => [item.session_id, item.reason])
    );
    const recipients = broadcast.data?.recipients ?? sent.answer?.recipients ?? [];
    rows = sent.order.map((sessionId) => {
      const reason = excluded.get(sessionId) ?? sent.planned.get(sessionId);
      if (reason !== undefined) return { outcome: { kind: "left out", reason }, sessionId };
      const recipient = recipients.find((each) => each.session_id === sessionId);
      return { outcome: attemptOutcome(recipient?.message.deliveries?.at(-1)), sessionId };
    });
  } else if (oneAnswer === null) {
    rows = [{ outcome: { kind: "on its way" }, sessionId: sent.sessionId }];
  } else if (isComment(oneAnswer)) {
    const deliveries = (comment.data?.comment.deliveries ?? oneAnswer.deliveries ?? []).filter(
      (delivery) => delivery.session_id === sent.sessionId
    );
    rows = [{ outcome: attemptOutcome(deliveries.at(-1)), sessionId: sent.sessionId }];
  } else {
    const deliveries = message.data?.message.deliveries ?? oneAnswer.deliveries ?? [];
    rows = [{ outcome: attemptOutcome(deliveries.at(-1)), sessionId: sent.sessionId }];
  }
  const read = [message, comment, broadcast].find((each) => each.isError);

  return (
    <section aria-label="Where your message went" className="space-y-3">
      <button
        className={`min-h-11 rounded-lg px-3 py-2 text-sm font-semibold ${primaryButtonBg} ${primaryButtonEnabledHoverBg}`}
        onClick={onDone}
        ref={doneRef}
        type="button"
      >
        Done
      </button>
      <ul aria-label="Each recipient's outcome" className="space-y-2">
        {rows.map(({ outcome, sessionId }) => (
          <li
            className={`text-sm ${textSecondaryOnSurface}`}
            data-compose-outcome={sessionId}
            key={sessionId}
          >
            <AgentName kind="static" sessionId={sessionId} storedTitle={undefined} />{" "}
            <span className={`font-mono text-xs ${textMutedOnSurface}`}>{sessionId}</span>
            <span
              className={
                outcome.kind === "failed" || outcome.kind === "left out" ? dangerText : undefined
              }
            >
              {" · "}
              {outcomeWords(outcome)}
            </span>
          </li>
        ))}
      </ul>
      {read === undefined ? null : (
        <QueryError
          error={read.error}
          message={`Could not read where it went since: ${read.error instanceof Error ? read.error.message : "network error"}. These are the outcomes Dispatch answered the send with.`}
          onRetry={() => void read.refetch()}
          retrying={read.isFetching}
        />
      )}
    </section>
  );
}

/** A recipient's state before Send, as the registry's newest answer reads it. A session it does
 *  not list is `unlisted`, in a send to two or more, or `unlisted alone`, in a send to one. */
type RecipientState = "running" | "unresponsive" | "unlisted" | "unlisted alone" | "unknown";

/** The words of a recipient's state before Send. The registry's own mark says an unresponsive
 *  session's state only in a tooltip, so what the server does with the message is said here, in
 *  the Agents page's words for such a session (`AgentUnresponsiveNote`): it has not ended, and the
 *  message is stored until it answers. A session the registry does not list may be stopped, and
 *  then the Envoy listener knows it and the message is stored for it, or the listener may not
 *  know it at all; only the server can tell the two apart. So its words name both outcomes, and
 *  the outcome after Send says which it was. A send to one is no broadcast and leaves nobody out,
 *  so only the first applies to it. */
const STATE_WORDS: Record<RecipientState, string> = {
  running: "Running",
  unknown: "Running state unknown",
  unlisted:
    "Not listed: the message is stored for it if the Envoy listener knows it; otherwise the send leaves it out",
  "unlisted alone": "Not listed: the message is stored for it",
  unresponsive: `It has not ended: ${UNRESPONSIVE_KEEPS_MESSAGES}`,
};

/**
 * The Inbox's compose dialog: a message to one or more agents, opened by the control row's **New
 * message** and by `n`, through the same composers and requests as the Agents page. The
 * recipients come from the live registry (`agentsQuery()`) in the shared list with a text box. One
 * recipient is `AgentMessageComposer`, its Issue picker above its message box: no issue picked is
 * one `POST /api/v1/agents/{session_id}/messages`, an issue picked is the comment that mentions the
 * session. Two or more are `BroadcastComposer`, one `POST /api/v1/broadcasts`, refused past
 * `MAX_BROADCAST_RECIPIENTS` before any request. The dialog keeps the message, so it stays when a
 * recipient is added or taken off, across one and two and back, and Escape, Cancel or the backdrop
 * ask before they discard it.
 *
 * Send is held by `aria-disabled` until the server answers; on an error it comes back with the
 * error. The answer draws each recipient's outcome in the place of the form, with Done first, and
 * the dialog hands the focus the form held to Done: the reader's Send is armed at the dialog's
 * root (`armOnPress`) and its hold keeps it live through the answer (`useHeldThroughAnswer`), so
 * the form's going is their press's doing (`causedByReader`). Done closes the dialog, and
 * `useDialog` gives the focus back to what opened it: the New message control, the row that held
 * the focus, or the page's body where nothing had it. Mounted only while open, so it starts fresh
 * each time.
 */
export function ComposeDialog({ onClose }: { onClose: () => void }): ReactNode {
  const dialog = useDialog<HTMLDivElement>({ onClose: () => closeOrAsk(), open: true });
  const writes = useWrites();
  const text = useAgentText();
  const { agents, current, isError, isPending, refetch, refilling } = useAgents(true, true);
  // Every registry row a recipient was last listed with: a picked session the newest answer no
  // longer lists keeps its title and what it advertised.
  const seen = useRef(new Map<string, Agent>());
  for (const agent of agents) seen.current.set(agent.session_id, agent);
  const listed = new Map(agents.map((agent) => [agent.session_id, agent]));

  // The message, its mode, the recipients and a refused broadcast's request, with the Agents
  // page's rule for a broadcast's key (`useBroadcastComposition`): a new key at every press, and
  // the old one only for a refused request sent again unchanged.
  const composition = useBroadcastComposition();
  const recipients = [...composition.selected];
  const body = composition.draft;
  const [listOpen, setListOpen] = useState(false);
  // The key of the broadcast out, and what refused the last one.
  const [out, setOut] = useState<string | null>(null);
  const [refusal, setRefusal] = useState<Error | null>(null);
  const [sent, setSent] = useState<Sent | null>(null);
  // Whether Escape, Cancel or the backdrop asked to discard the message, and the control in the
  // dialog that held the focus then, which Keep gives it back to.
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  const focusedAtAsk = useRef<HTMLElement | null>(null);
  const setBody = (next: string) => {
    composition.setDraft(next);
    setConfirmingDiscard(false);
  };
  /** Escape, Cancel and a click on the backdrop. Over a message not yet sent they ask `Discard
   *  draft?`, as the one-recipient composer does over its own draft, and Keep leaves the message
   *  as it was. An empty message, or the outcomes in the form's place, closes at once, and so does
   *  a second ask. */
  const closeOrAsk = () => {
    if (sent !== null || body.trim() === "" || confirmingDiscard) {
      onClose();
      return;
    }
    const active = document.activeElement;
    focusedAtAsk.current =
      active instanceof HTMLElement && dialog.containerRef.current?.contains(active)
        ? active
        : null;
    setConfirmingDiscard(true);
  };
  // The broadcast's write as the store holds it, for its state past the write deadline.
  const outWrite = useWriteSelection(`inbox-compose:${out ?? ""}`, (rows) =>
    out === null
      ? undefined
      : rows.find((row) => ofKind(row, composeBroadcast) !== undefined && row.key === out)
  );
  // The press's hold on what answers it (`useHeldThroughAnswer`) ends at the write deadline from
  // the press at the latest, as a held Send's does (`MentionComposer`'s `heldSend`): a send still
  // out past it is still being tried under its key, which the dialog then says, with Stop trying.
  const pastDeadline = out !== null && outWrite?.status === "late";
  useHeldThroughAnswer(out !== null && !pastDeadline);
  // The control of the form that holds the focus, noted as it takes it, and Done, which the
  // outcomes draw first.
  const formFocus = useRef<Element | null>(null);
  const doneRef = useRef<HTMLButtonElement>(null);
  // The dialog's own hand-off, in the commit that draws the outcomes in the form's place: a focus
  // the form held, which the reader's own Send took away with the form - a loss their press made
  // (`causedByReader`) - goes to Done, with no scroll. A focus the reader moved elsewhere
  // meanwhile, or one already placed, stays where it is.
  useLayoutEffect(() => {
    const noted = formFocus.current;
    if (sent === null || noted === null) return;
    formFocus.current = null;
    if (noted.isConnected || !focusLost() || !causedByReader(noted)) return;
    doneRef.current?.focus({ preventScroll: true });
  }, [sent]);

  const one = recipients.length === 1 ? recipients[0] : undefined;
  const oneSendKey = composeSendKey(one ?? "");
  const oneHold = useSending(oneSendKey);
  const selected = composition.selected;
  // The mode the dialog shows and sends, by the Agents page's rule (`compositionDelivery`).
  const delivery = compositionDelivery(composition, agents);
  const composed = composedBroadcast({ ...composition, delivery }, { agents, current, refilling });

  // The Agents page's rule (`liveExclusion`): a session the registry does not list is never left
  // out before Send, since only the server can tell a stopped session from one the Envoy listener
  // does not know.
  const stateOf = (sessionId: string): RecipientState => {
    if (!current) return "unknown";
    const agent = listed.get(sessionId);
    if (agent === undefined) return recipients.length > 1 ? "unlisted" : "unlisted alone";
    return agent.state === "unresponsive" ? "unresponsive" : "running";
  };

  const sendBroadcast = (send: BroadcastSend) => {
    if (out !== null) return;
    setRefusal(null);
    setOut(send.key);
    composition.pressed();
    const planned = new Map(composed.plan.excluded.map((item) => [item.sessionID, item.reason]));
    const order = [...recipients];
    writes
      .run({
        ...broadcastWrite(send),
        detail: { input: send.input },
        inline: true,
        kind: composeBroadcast,
      })
      .then(
        (answer) => {
          setOut(null);
          setSent({ answer, kind: "broadcast", order, planned });
        },
        (error: unknown) => {
          const refused = error instanceof Error ? error : new Error(String(error));
          setOut(null);
          setRefusal(refused);
          // The request comes back as it went. Sent again unchanged, it goes under its own key
          // where it ended without Dispatch's verdict, and under a new one where Dispatch refused
          // it; any edit sends a new request under the key the press made.
          composition.restore(send, refused);
        }
      );
  };

  const options = [
    ...agents.map((agent) => agent.session_id),
    ...recipients.filter((sessionId) => !listed.has(sessionId)),
  ];
  const titleOf = (sessionId: string) => seen.current.get(sessionId)?.title;
  const oneAgent = one === undefined ? undefined : seen.current.get(one);

  let composer: ReactNode = null;
  if (recipients.length === 0) {
    composer = (
      <p className={`text-sm ${textMutedOnSurface}`}>Pick one or more agents to write to.</p>
    );
  } else if (one !== undefined && oneAgent !== undefined) {
    composer = (
      <AgentMessageComposer
        agent={oneAgent}
        heldSend
        hold={oneHold}
        hostDraft={{ onChange: setBody, starting: body }}
        // A recipient changed is a composer of its own, which starts from the message kept here.
        key={one}
        onCancelReply={() => {}}
        onClose={onClose}
        onSent={(answer) =>
          setSent({ answer: answer as Message | Comment | null, kind: "one", sessionId: one })
        }
        replyTo={null}
        sendKey={oneSendKey}
      />
    );
  } else {
    composer = (
      <>
        <BroadcastComposer
          agents={agents}
          body={body}
          composed={composed}
          defaulted={composition.delivery === null}
          delivery={delivery}
          held={out !== null}
          onBody={setBody}
          onDelivery={composition.setDelivery}
          onDeselect={(sessionId) =>
            composition.setSelected((current) => {
              const next = new Set(current);
              next.delete(sessionId);
              return next;
            })
          }
          onSend={sendBroadcast}
          selected={selected}
        />
        {pastDeadline && outWrite !== undefined && outWrite.status === "late" ? (
          <div className="flex flex-wrap items-center gap-3">
            <p className={`text-sm ${textMutedOnSurface}`} role="status">
              {KEYED_SEND_LATE_TEXT}
            </p>
            <StopTrying onStop={() => writes.stop(outWrite.id)} stopping={outWrite.stopping} />
          </div>
        ) : null}
        {refusal === null ? null : (
          <p className={`text-sm ${dangerText}`} role="alert">
            Couldn't send — {refusal.message}
          </p>
        )}
      </>
    );
  }

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={closeOrAsk} />
      <div className="pointer-events-none fixed inset-0 z-50 flex items-start justify-center xl:p-4 xl:pt-20">
        {/* The capture handlers only arm the dialog's hand-off at a press inside it (`armOnPress`),
            and the focus handler notes which control of the form holds the focus, for that
            hand-off when the outcomes take the form's place. */}
        <div
          aria-label="New message"
          aria-modal="true"
          className={`pointer-events-auto max-h-[100dvh] w-full space-y-3 overflow-y-auto border p-4 shadow-2xl xl:max-h-[calc(100dvh-6rem)] xl:max-w-2xl xl:rounded-lg ${card} ${borderDefault}`}
          data-testid="compose-dialog"
          {...ARM_ON_PRESS}
          onFocus={(event) => {
            formFocus.current = sent === null ? event.target : null;
          }}
          ref={dialog.containerRef}
          role="dialog"
        >
          <h2 className={`text-base font-semibold ${textPrimaryOnSurface}`}>New message</h2>
          {sent !== null ? (
            <SentOutcomes doneRef={doneRef} key="outcomes" onDone={onClose} sent={sent} />
          ) : (
            <div className="space-y-3" key="form">
              <MultiSelect
                // Held while a send is out, a broadcast or a send to one: a change of recipients
                // would take the composer whose Send is out off the page, and its outcome with it.
                disabled={out !== null || oneHold.sending}
                emptyMessage={
                  isPending
                    ? "Loading agents…"
                    : agents.length === 0
                      ? "No agent is connected."
                      : "No agent matches that."
                }
                error={isError ? "Could not load agents." : null}
                label="Recipients"
                onChange={(next) => composition.setSelected(() => new Set(next))}
                onOpenChange={(open) => {
                  setListOpen(open);
                  // The list reads the registry again as it opens, inside the shell poller's 15 s,
                  // so a session started or ended since the last poll is listed as it is now.
                  if (open) void refetch();
                }}
                open={listOpen}
                optionLabel={(sessionId) => text.sessionText(sessionId, titleOf(sessionId))}
                options={options}
                searchLabel="Search agents"
                selected={recipients}
                triggerClassName={`min-h-11 w-full rounded-lg border px-3 py-2 text-left text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
              />
              {recipients.length === 0 ? null : (
                <ul aria-label="Each recipient's state" className="space-y-1">
                  {recipients.map((sessionId) => {
                    const state = stateOf(sessionId);
                    return (
                      <li
                        className={`text-sm ${textSecondaryOnSurface}`}
                        data-compose-state={sessionId}
                        key={sessionId}
                      >
                        <AgentName
                          kind="static"
                          sessionId={sessionId}
                          storedTitle={titleOf(sessionId)}
                        />{" "}
                        <AgentUnresponsiveNote
                          lastSeen={listed.get(sessionId)?.last_seen}
                          state={
                            listed.get(sessionId)?.state === "unresponsive"
                              ? "unresponsive"
                              : "running"
                          }
                        />
                        <span>
                          {" · "}
                          {STATE_WORDS[state]}
                        </span>
                      </li>
                    );
                  })}
                </ul>
              )}
              {composer}
              {confirmingDiscard ? (
                <DiscardDraftPrompt
                  onDiscard={onClose}
                  onKeep={() => {
                    setConfirmingDiscard(false);
                    focusedAtAsk.current?.focus();
                  }}
                />
              ) : null}
              <div className="flex justify-end">
                <button
                  className={`min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
                  onClick={closeOrAsk}
                  type="button"
                >
                  Cancel
                </button>
              </div>
            </div>
          )}
        </div>
      </div>
    </>
  );
}
