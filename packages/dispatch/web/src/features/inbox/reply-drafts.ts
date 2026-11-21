import { type Dispatch, type SetStateAction, useCallback } from "react";

import { type Shape, shape } from "../writes/tab-storage";
import { createMentionDraftStore, type Draft, EMPTY_DRAFT, keptMentions } from "./draft-store";

/** A reply draft: the text and its mentions, and whether the ask took a reply as a settled ask
 *  (`takesReply` in `AskThread.tsx`) when the reader last changed them. */
export interface ReplyDraft extends Draft {
  readonly writtenSettled: boolean;
}

const EMPTY_REPLY_DRAFT: ReplyDraft = { ...EMPTY_DRAFT, writtenSettled: false };

/** A reply draft as the tab keeps it for a reload (`ReplyDraft`). */
const keptReplyDraft = shape.record({
  mentions: keptMentions,
  text: shape.string,
  writtenSettled: shape.boolean,
}) satisfies Shape<ReplyDraft>;

/**
 * Unsent ask-reply drafts, by ask id. A draft belongs to the ask, not to the composer showing it:
 * every composer for one ask reads and writes the same draft (the margin's inline composer and a
 * Conversation or decision-block composer can be on one page at once; the Inbox reading pane's
 * approval request and the card the same request is drawn as below the pane's width are one
 * ask's), and a composer that remounts (switching margin tabs away and back, closing and
 * reopening Write a reply) picks the draft back up - the mentions it holds with its text
 * (`draft-store.ts`). The tab keeps each for a reload of the page too, under `reply`, with what
 * it records. A sent reply or an emptied field drops it. The ask resolving does not: a resolved
 * ask's thread still takes the reply (`takesReply` in `AskThread.tsx`). Nor does the ask opening
 * again.
 *
 * Each write records whether the ask took a reply as a settled ask when the reader wrote it
 * (`writtenSettled`), because composers for one ask can stand on either side of that: an open ask
 * draws a reply box only where its host replies while it is open (`repliesWhileOpen` in
 * `AskThread.tsx`). A draft written while the ask was settled, on an ask that has since opened
 * again, is a reply the ask's other threads have no composer left to send, so they show it
 * read-only (`useUnsentReply`). A draft written while the ask was open is a reply the reader is
 * still writing in that host, which no other thread reads as a send that failed.
 */
const store = createMentionDraftStore("reply", EMPTY_REPLY_DRAFT, keptReplyDraft);

/** Drops an ask's draft once its reply is sent, only while its text is still the text sent. */
export function clearReplyDraft(askId: string, sent: string): void {
  if (store.read(askId).text.trim() === sent) store.set(askId, EMPTY_REPLY_DRAFT);
}

/**
 * The ask's draft and its setter, for a thread drawn while the ask takes a reply as a settled ask
 * (`settled`, `takesReply`) or while it is open; the component re-renders whenever any composer
 * changes the draft. Every write through the setter records `settled` with what it writes
 * (`writtenSettled`).
 */
export function useReplyDraft(
  askId: string,
  settled: boolean
): [ReplyDraft, Dispatch<SetStateAction<Draft>>] {
  const [draft, setDraft] = store.useDraft(askId);
  const write = useCallback(
    (next: SetStateAction<Draft>) =>
      setDraft((current) => ({
        ...(typeof next === "function" ? next(current) : next),
        writtenSettled: settled,
      })),
    [setDraft, settled]
  );
  return [draft, write];
}

/** Writes an ask's draft by the ask's id, from outside every composer for it: where an upload
 *  pasted into a composer adds its reference once a host draws the composer for another ask
 *  (`AskReplyComposer`). */
export const setReplyDraft = store.set;

/** Who is signed in now: another person's sign-in forgets every reply draft, since each is the
 *  person's who typed it (`DraftStore.signedIn`). */
export const replyDraftsSignedIn = store.signedIn;

/** The person whose sign-out landed: every reply draft of theirs is forgotten, in the page and
 *  in the tab, and none is taken until someone signs in (`DraftStore.signedOut`). */
export const replyDraftsSignedOut = store.signedOut;

/** What a reload of the page does to the reply drafts, for the draft matrix: the page forgets
 *  them, and the tab keeps them for the next sign-in (`DraftStore.reload`). */
export const reloadReplyDrafts = store.reload;

/** Forgets every reply draft, for the unit-test preload. */
export const resetReplyDrafts = store.reset;
