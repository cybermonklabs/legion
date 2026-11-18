import { createDraftStore } from "./draft-store";

/**
 * Unsent ask-reply drafts, by ask id. A draft belongs to the ask, not to the composer showing it:
 * every composer for one ask reads and writes the same draft (the margin's inline composer and a
 * Conversation or decision-block composer can be on one page at once), and a composer that
 * remounts (switching margin tabs away and back, closing and reopening Write a reply) picks the
 * draft back up. The tab keeps each for a reload of the page too, under `reply`
 * (`draft-store.ts`). A sent reply or an emptied field drops it. The ask resolving does not: a
 * resolved ask's thread still takes the reply (`takesReply` in `AskThread.tsx`). Nor does the
 * ask opening again, whose thread shows the draft read-only (`useUnsentReply`).
 */
const store = createDraftStore("reply");

/** Drops an ask's draft once its reply is sent, only while the draft is still the text sent. */
export function clearReplyDraft(askId: string, sent: string): void {
  if (store.read(askId).trim() === sent) store.set(askId, "");
}

/** The ask's draft and its setter; the component re-renders whenever any composer changes it. */
export const useReplyDraft = store.useDraft;

/** Writes an ask's draft by the ask's id, from outside every composer for it: where an upload
 *  pasted into a composer adds its reference once a host draws the composer for another ask
 *  (`AskReplyComposer`). */
export const setReplyDraft = store.set;

/** Who is signed in now: another person's sign-in forgets every reply draft, since each is the
 *  person's who typed it (`DraftStore.signedIn`). */
export const replyDraftsSignedIn = store.signedIn;

/** Forgets every reply draft, for the unit-test preload. */
export const resetReplyDrafts = store.reset;
