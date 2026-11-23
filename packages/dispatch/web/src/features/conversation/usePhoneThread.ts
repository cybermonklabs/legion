import type { MutationKey } from "@tanstack/react-query";
import { type RefObject, useCallback, useEffect, useMemo, useRef, useState } from "react";

import { useSending } from "../../hooks/useSending";
import {
  type ComposerOwner,
  type DraftIntake,
  hasUnsavedInput,
  type ReplyTarget,
} from "./composer-model";
import { redirectedReplyTarget, useHeldReplyTo, useHeldSends } from "./held-sends";
import type { CarriedAddress, SentDraft } from "./send-request";

/** The name every comment thread card's own reply send in a tab sits beneath. */
export function threadRepliesSendKey(sendKey: MutationKey): MutationKey {
  return [...sendKey, "thread-card"];
}

/** The name of a comment thread card's own reply composer's send: beneath the tab's `sendKey`, so
 *  the tab's Replies hold while it is out as they do for the tab's own composers, and its own, so
 *  the thread can tell that its reply is the one out. */
export function threadReplySendKey(sendKey: MutationKey, commentId: string): MutationKey {
  return [...threadRepliesSendKey(sendKey), commentId];
}

/** The Conversation's full-screen thread view on a phone, and the composer of its own that a
 *  comment's Reply answers in there. */
export interface PhoneThread {
  /** The comment whose thread shows, or undefined. It shows on a phone, and at any width while a
   *  send from the view keeps it open, until Back. */
  readonly id: string | undefined;
  /** Whether the reader has `commentId` in hand here - its thread open, or the thread composer
   *  answering it - so the tab keeps it listed whoever resolves it. */
  readonly holds: (commentId: string) => boolean;
  /** The thread composer's reply, so the docked composer's own reply and draft stay as they were.
   *  The view stays mounted, hidden, while it is set, so the composer, an unsent draft and a send
   *  still out outlive Back; a send it holds is the held-send store's, and comes back with its
   *  reply. */
  readonly replyTo: ReplyTarget | null;
  /** The draft the thread composer was put away with, its reply and its send as they stand
   *  (`cancelReply`), or none while it is not put away: hidden in its own thread too, and said so
   *  beside the docked composer, until that comment's Reply shows it again or its reply ends. The
   *  field cannot change while it is hidden, so this is what it holds; the docked composer draws
   *  it once the comment it answers is no longer listed, or is deleted, when no Reply goes on
   *  with it. */
  readonly parked: SentDraft | undefined;
  /** Discard my draft beside a reply put away (`parked`) whose comment is no longer listed, or is
   *  deleted: drops the thread composer's draft and its send as its own Discard does, which ends
   *  its reply (`endReply`) and keeps nothing for the comment as it goes. */
  readonly dropParked: () => void;
  /** The thread composer's send name: beneath the tab's, so every Reply holds for it as for the
   *  docked composer's, and its own, so the view can tell its send from the docked one's. */
  readonly composerKey: MutationKey;
  /** Whether the open thread's card has its own reply out: the view's Back holds for it. */
  readonly cardReplySending: boolean;
  readonly open: (commentId: string) => void;
  /** Answers `target` in its thread, and says whether it did: a send the tab has out refuses it,
   *  and a refusal the thread composer holds for another comment opens that comment's thread
   *  instead, where the reader retries or drops it. A thread composer put away shows again. */
  readonly beginReply: (target: ReplyTarget) => boolean;
  /** The docked composer's field (`MentionComposer`'s `intake`), where the thread composer's
   *  Cancel reply puts its draft. */
  readonly dockedIntake: RefObject<DraftIntake | null>;
  /** The thread composer's field (`MentionComposer`'s `intake`), which `dropParked` drops. */
  readonly threadIntake: RefObject<DraftIntake | null>;
  /** The reader's own Cancel reply in the thread composer, or Escape in its field: a move to the
   *  issue's own address, where the docked composer is, so the draft goes there, as the docked
   *  composer's own Cancel reply takes its draft along. The docked composer's field takes
   *  `draft`, and a refused or stopped send goes with it (`HeldSends.carryInto`), so its Retry
   *  and its Send go from there, under a key of their own; nothing is sent until the reader
   *  presses. A docked composer holding a draft of its own - typed, a send out or refused, or a
   *  reply of its own - is never written over: the thread composer is put away as it stands
   *  instead (`parked`), and that comment's Reply brings it back. */
  readonly cancelReply: (draft: SentDraft) => void;
  /** Ends the thread composer's reply, and the composer with it, once nothing of it is left to
   *  keep - its send landed, or the reader discarded its draft (`MentionComposer`'s `onSent` and
   *  `onClose`): a refusal it still holds goes too. */
  readonly endReply: () => void;
  /** The reader leaves the thread - Back, Escape, Collapse thread: refused while the open
   *  thread's card has its reply out, until the send's deadline. Leaving is the host's move, so
   *  it carries nothing and drops nothing: the card's reply and the thread composer's stay held
   *  for their thread, typed, out, refused or stopped, and opening the thread again, or that
   *  comment's Reply, brings each back where it was. */
  readonly leave: () => void;
  /** Hides the thread once a decision on its comment lands, and leaves every send the store holds
   *  where it is: a refused reply and the comment it answers stay, as above the phone layout. */
  readonly hide: () => void;
}

export function usePhoneThread({
  isPhoneViewport,
  owner,
  sendKey,
  sendingNow,
}: {
  readonly isPhoneViewport: boolean;
  /** The tab's issue, which the docked composer addresses with no reply. */
  readonly owner: ComposerOwner;
  /** The tab's send name; the view's composers name theirs beneath it. */
  readonly sendKey: MutationKey;
  /** Whether a send the tab names is out now, from Send's own task on. */
  readonly sendingNow: () => boolean;
}): PhoneThread {
  const store = useHeldSends();
  const [openId, setOpenId] = useState<string>();
  const composerKey = useMemo<MutationKey>(() => [...sendKey, "phone-thread"], [sendKey]);
  const [replyTo, setReplyTo] = useHeldReplyTo(composerKey);
  const [parked, setParked] = useState<SentDraft>();
  const dockedIntake = useRef<DraftIntake>(null);
  const threadIntake = useRef<DraftIntake>(null);
  // Set when the viewport widened past the phone layout while a send from the view was out - its
  // card's own reply, or the thread composer's: the view stays open, full-screen at any width,
  // until the reader leaves it with Back, so the send's draft and its refusal stay where the
  // reader was. A widening with nothing out closes the view.
  const [outlived, setOutlived] = useState(false);
  // No comment has the empty id, so with no thread open nothing matches the card's key.
  const cardReplyKey = useMemo(() => threadReplySendKey(sendKey, openId ?? ""), [openId, sendKey]);
  const { sending: cardReplySending, sendingNow: cardReplySendingNow } = useSending(cardReplyKey, {
    untilDeadline: true,
  });
  const { sending: composerSending } = useSending(composerKey, { untilDeadline: true });
  const replySending = composerSending && replyTo !== null;
  useEffect(() => {
    if (isPhoneViewport || outlived) return;
    if (openId !== undefined && cardReplySending) {
      setOutlived(true);
      return;
    }
    if (replySending && replyTo !== null) {
      // The view that stays is the thread the composer's send answers, even one the reader had
      // left with Back: on a wider screen nothing else would show that send's outcome.
      setOpenId(replyTo.id);
      setOutlived(true);
      return;
    }
    if (openId !== undefined) setOpenId(undefined);
  }, [cardReplySending, isPhoneViewport, openId, outlived, replySending, replyTo]);
  const holds = useCallback(
    (commentId: string) => commentId === openId || commentId === replyTo?.id,
    [openId, replyTo]
  );
  const endReply = () => {
    store.discard(composerKey);
    setReplyTo(null);
    setParked(undefined);
  };
  const cancelReply = (draft: SentDraft) => {
    const to: CarriedAddress = { owner, replyTo: null };
    // Nothing typed and no send held: there is no draft to keep anywhere.
    if (store.get(composerKey) === undefined && !hasUnsavedInput(draft.body, draft.replacement)) {
      setReplyTo(null);
      return;
    }
    // The docked composer takes the draft only while it holds nothing of its own, a send of its
    // own included (`DraftIntake`), so the refusal carried after it is the only send there.
    if (dockedIntake.current?.take(draft, to) === true) {
      store.carryInto(composerKey, sendKey, to);
      setReplyTo(null);
      return;
    }
    setParked(draft);
  };
  const hide = () => {
    setOpenId(undefined);
    setOutlived(false);
  };
  return {
    beginReply: (target) => {
      if (sendingNow()) return false;
      const reply = redirectedReplyTarget(store, composerKey, target);
      setReplyTo(reply);
      setParked(undefined);
      setOpenId(reply.id);
      return reply.id === target.id;
    },
    cancelReply,
    cardReplySending,
    composerKey,
    dockedIntake,
    dropParked: () => threadIntake.current?.drop(),
    endReply,
    hide,
    holds,
    id: isPhoneViewport || outlived || cardReplySending || replySending ? openId : undefined,
    leave: () => {
      if (cardReplySendingNow()) return;
      hide();
    },
    open: setOpenId,
    parked,
    replyTo,
    threadIntake,
  };
}
