import type { CreateMessageInput } from "@legion/contracts";
import type { MutationKey } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef } from "react";

import { ApiError, api } from "../../../api/client";
import type { Agent, DeliveryCapability, Message } from "../../../api/types";
import { submitOnModifiedEnter } from "../../../hooks/submitOnModifiedEnter";
import { useSending } from "../../../hooks/useSending";
import {
  bgTransparent,
  pageComposer,
  pageFailedText,
  pageInkText,
  pageMutedText,
  pageSend,
} from "../../../theme/classes";
import { deliveryModesOf } from "../../conversation/delivery";
import { deliveryPlan } from "../../conversation/send-request";
import { useUnsentDrafts } from "../../conversation/unsent-drafts";
import { useAgents } from "../../conversation/useAgents";
import { composerNameOf } from "../../conversation/useKeptDrafts";
import { registeredAgent } from "../../refs/agent-registry";
import { roleHolders } from "../../refs/role-holders";
import { partyOfAddress } from "../../refs/routes";
import { type ThreadMessage, threadMessageWrite } from "../../threads/thread-message-write";
import { useKeyedWrite } from "../../writes/useKeyedWrite";
import { authorText, pageChrome } from "../page-structure";
import { ANSWER_LINES, CONTROL_LABELS, THREADS_WORDS } from "../page-words";

/** Where a box's message goes: the message it answers, and the thread's target with the mode a
 *  message to it goes in. A thread whose root names no target sends neither. */
export interface MessageAddress {
  readonly inReplyTo?: string;
  readonly thread?: { readonly target: string; readonly delivery: DeliveryCapability };
}

/** One send of the box: the words as the reader wrote them, and the message they went out as. */
interface MessagePress {
  readonly body: string;
  readonly input: CreateMessageInput;
}

/** The message `body` goes out as to `address`: a delivery command at its start (`/btw`, `/aside`)
 *  picks the mode where the thread has a target, as `conversation/send-request.ts` builds a
 *  message reply, and the body goes as written where it has none. */
function messageInput(body: string, address: MessageAddress): CreateMessageInput {
  const reply = address.inReplyTo === undefined ? {} : { in_reply_to: address.inReplyTo };
  const { thread } = address;
  if (thread === undefined) return { body, ...reply };
  const plan = deliveryPlan(body, thread.delivery);
  return { body: plan.body, delivery: plan.delivery, target: thread.target, ...reply };
}

/** The session the registry response `agents` lists for the delivery address `target`: the
 *  session a `session:` address names, or the holder of the role a `role:` address names, which
 *  is the session Dispatch delivers to (`role-holders.ts`). Undefined for a session it does not
 *  list, a role no session it lists holds, and an address of no party (`partyOfAddress`). */
function listedRecipient(agents: readonly Agent[], target: string): Agent | undefined {
  const party = partyOfAddress(target);
  if (party?.kind === "agent-session") return registeredAgent(agents, party.session);
  if (party?.kind === "role") return roleHolders(agents).get(party.role);
  return undefined;
}

/**
 * A box of Threads that writes a message (dispatch://INFRA-5704/spec#b-r-mentions, R9, R11): the
 * box under an entry of a message thread, the one at its foot, and New thread's. It is this
 * part's own field and no `MentionField`, so `@` offers nothing and stores no mention: a message
 * names its agent by its target (R11). Its row of the writing list (`threads-message`
 * in `DISPATCH_WRITING_BOXES`) is not covered for the same reason, so the box draws no hint.
 *
 * Send writes `POST /api/v1/issues/{key}/messages` through the write store, under this part's own
 * write kind (`threadMessageWrite`), with the words, the message it answers and the thread's
 * target and delivery. The words are kept under the box's send name in the composers' store of
 * unsent drafts (`useUnsentDrafts`) as the reader types them, so a box that goes - another
 * entry's Reply, Escape - and comes back shows them again, as a `MentionComposer` box does. They
 * leave the store once Dispatch took the message. While a send is out the field is held, and a
 * refusal says why beside Retry with the words kept. A box with no address yet, New thread's
 * before the reader chose its agent, offers no Send (R10). A box whose message answers none, New
 * thread's, writes a new thread: its write is named New thread and its refusal says the message
 * was not sent, where a reply's says the reply was not.
 *
 * Where the session the registry lists for the thread's target advertises no delivery mode
 * (`deliveryModesOf`), the box says before the send that a send to it records a failed attempt,
 * as `plainDelivery`'s contract and a message box on `main` (`MentionComposer`) say (R10), and
 * the send goes as it would without the warning. The box reads the registry the cache holds and
 * sends no read of it (R15): New thread's chooser reads it, and an opened record sends no read but
 * its read mark's. Only a current list says a party takes no message: a registry not read, a
 * failed read and a role no listed session holds say nothing of the modes a party takes.
 */
export function MessageBox({
  address,
  autoFocus = false,
  issueKey,
  label,
  onLeave,
  onSent,
  sendKey,
}: {
  /** Where the message goes, or undefined while the reader has not chosen it yet. */
  readonly address: MessageAddress | undefined;
  /** Puts the caret in the field as the box mounts: a box the reader opened with a Reply. */
  readonly autoFocus?: boolean;
  readonly issueKey: string;
  /** The field's name, which is its accessible name. */
  readonly label: string;
  /** The reader leaves the box with Escape; a box that stays, as the one at a record's foot
   *  does, takes none. */
  readonly onLeave?: () => void;
  readonly onSent?: (message: Message) => void;
  /** The box's send name: its words and its send are kept under it. */
  readonly sendKey: MutationKey;
}): ReactNode {
  const unsent = useUnsentDrafts();
  const name = composerNameOf(sendKey);
  const body = unsent.useUnsent(name)?.body ?? "";
  const field = useRef<HTMLTextAreaElement>(null);
  const { sending } = useSending(sendKey);
  // R10 and R15: the registry as the cache holds it, never read here, and only a current list.
  const { agents, current } = useAgents(false);
  const target = address?.thread?.target;
  const recipient = current && target !== undefined ? listedRecipient(agents, target) : undefined;
  const takesNoMessage =
    recipient !== undefined && deliveryModesOf(recipient.capabilities ?? []).length === 0;
  const write = useKeyedWrite<Message, MessagePress, unknown, ThreadMessage>({
    detail: (press) => ({ body: press.body, issueKey }),
    inline: true,
    kind: threadMessageWrite,
    label: (press) =>
      press.input.in_reply_to === undefined ? THREADS_WORDS.new_thread : CONTROL_LABELS.reply,
    method: "POST",
    mutationKey: sendKey,
    onSuccess: (message, press) => {
      // The words leave the store once Dispatch took them, unless the reader has written more
      // since, which are theirs to send.
      if (unsent.read(name)?.body === press.body) unsent.write(name, undefined);
      if (message !== null) onSent?.(message);
    },
    write: (press, bounds) => api.createMessage(issueKey, press.input, bounds),
  });
  useEffect(() => {
    if (autoFocus) field.current?.focus();
  }, [autoFocus]);
  const words = body.trim();
  const send = () => {
    if (words === "" || sending || address === undefined) return;
    write.mutate({ body, input: messageInput(words, address) });
  };
  return (
    <form
      className={pageComposer.box}
      onSubmit={(event) => {
        event.preventDefault();
        send();
      }}
    >
      {/* The line keeps its layout whether the field holds the caret or not: a box under an
          entry that changed its height as the caret left it would move the entries under it
          between the press of a control below and its release, so the press would miss. */}
      <div className="flex min-w-0 items-end gap-2">
        <textarea
          aria-label={label}
          className={`block max-h-[150px] min-h-[26px] min-w-0 flex-1 resize-none border-0 py-px text-[15px] leading-6 outline-none ${bgTransparent} ${pageInkText}`}
          disabled={sending}
          onChange={(event) => {
            const next = event.target.value;
            unsent.write(name, next === "" ? undefined : { body: next, replacement: "" });
          }}
          onKeyDown={(event) => {
            if (submitOnModifiedEnter(event)) return;
            if (event.key === "Escape" && onLeave !== undefined && !event.nativeEvent.isComposing) {
              event.preventDefault();
              event.stopPropagation();
              onLeave();
            }
          }}
          ref={field}
          rows={1}
          value={body}
        />
        <div className={pageComposer.controls}>
          <button
            className={`${pageSend} disabled:cursor-default disabled:opacity-50`}
            disabled={words === "" || sending || address === undefined}
            type="submit"
            {...pageChrome}
          >
            {sending ? ANSWER_LINES.sending : CONTROL_LABELS.send}
          </button>
        </div>
      </div>
      {takesNoMessage ? (
        <p className={`text-sm ${pageFailedText}`} {...pageChrome}>
          {ANSWER_LINES.threads_no_delivery}
        </p>
      ) : null}
      {write.isError && !sending ? (
        <p className={`flex flex-wrap items-center gap-2 text-sm ${pageFailedText}`} role="alert">
          {/* Dispatch's own words for the refusal are a value of the page. The page's line
              stands where Dispatch gave none, as for a lost connection. */}
          {write.error instanceof ApiError ? (
            <span {...authorText}>{write.error.message}</span>
          ) : (
            <span {...pageChrome}>
              {address?.inReplyTo === undefined
                ? ANSWER_LINES.threads_message_not_sent
                : ANSWER_LINES.threads_reply_not_sent}
            </span>
          )}
          <button
            className={`font-medium underline ${pageMutedText}`}
            onClick={() => write.retry()}
            type="button"
            {...pageChrome}
          >
            {CONTROL_LABELS.retry}
          </button>
        </p>
      ) : null}
    </form>
  );
}
