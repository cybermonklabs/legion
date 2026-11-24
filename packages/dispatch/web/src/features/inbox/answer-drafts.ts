import { createDraftStore } from "./draft-store";

/**
 * Unsent ask-answer drafts, by ask id, for the reason the reply drafts beside them live in one
 * (`reply-drafts.ts`): a draft belongs to the ask, not to the form showing it, so every card for
 * one ask reads and writes the same draft and a card that remounts picks it back up. The tab
 * keeps each for a reload of the page too, under `answer` (`draft-store.ts`).
 *
 * It outlives the ask's Inbox row, which is what the Inbox reads it for. A question the server
 * withdraws, resolves or answers elsewhere leaves the list at once, except where the reader had
 * typed an answer to it: the reading column then keeps that question - its closed state, with
 * their text under it - until they select another row (`pane-selection.ts`, `paneAsk`;
 * `AskCard.tsx`, `unsentAnswer`). An answer the server records drops the draft, since it is no
 * longer unsent.
 */
const store = createDraftStore("answer");

/** What the reader has typed into the ask's answer box and not sent, read without subscribing to
 *  it: the Inbox asks at the commit that drops the row, not at every keystroke in the column. */
export const answerDraft = store.read;

/** The ask's draft and its setter; the component re-renders whenever any form changes it. */
export const useAnswerDraft = store.useDraft;

/** Writes an ask's draft by the ask's id, from outside every field for it: what a press of that
 *  ask's form addresses once a host draws the form for another ask (`useAskAnswerForm`). */
export const setAnswerDraft = store.set;

/** Who is signed in now: another person's sign-in forgets every answer draft, since each is the
 *  person's who typed it (`DraftStore.signedIn`). */
export const answerDraftsSignedIn = store.signedIn;

/** The person whose sign-out landed: every answer draft of theirs is forgotten, in the page and
 *  in the tab, and none is taken until someone signs in (`DraftStore.signedOut`). */
export const answerDraftsSignedOut = store.signedOut;

/** What a reload of the page does to the answer drafts, for the draft matrix: the page forgets
 *  them, and the tab keeps them for the next sign-in (`DraftStore.reload`). */
export const reloadAnswerDrafts = store.reload;

/** Forgets every answer draft, for the unit-test preload. */
export const resetAnswerDrafts = store.reset;
