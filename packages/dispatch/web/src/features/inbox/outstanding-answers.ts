import { useQueryClient } from "@tanstack/react-query";

import type { WriteBounds, WriteMethod } from "../../api/client";
import type { Ask } from "../../api/types";
import { askWritten } from "../../api/write-events";
import {
  type KeyedWriteResult,
  livePress,
  useKeyedWrite,
  useLiveRegistration,
} from "../writes/useKeyedWrite";
import { invalidateMeInbox } from "./one-read-cache";
import {
  cancelInboxAsks,
  type InboxAsksSnapshot,
  inboxAsksKeys,
  inboxAsksSnapshot,
  invalidateOpenAsks,
  patchInboxAsks,
  restoreInboxAsks,
} from "./open-asks-cache";

/**
 * The asks whose answer the reader has sent and the server has not decided, by ask id: the write
 * is in flight, or it was refused and the card still holds that refusal under their answer.
 *
 * It lives beside the drafts (`answer-drafts.ts`) for the same reason they do: what the reader
 * has put into an ask belongs to the ask, not to the form holding it, and the reading column
 * reads it from outside every card (`pane-selection.ts`, `paneAsk`). The column's card can be the
 * document's own - a question written as a decision block, drawn by `ProofDocument` - which takes
 * no prop of the Inbox's, so the form registers itself here rather than reporting upward as
 * `SnoozeControl` and `AssignToMe` do.
 *
 * What it is for: `useOutstandingAnswerWrite` below drops the answered row from the reads the
 * Inbox draws it from the moment the answer goes out, so without this hold the column would hand
 * the selection on at once and unmount the card that wrote - and that card is the only surface a
 * refused answer is shown on, since `useKeyedWrite({inline: true})` lets a refused write go from
 * the store. The hold is the rule a deferred row already follows (`snoozing` in `Inbox.tsx`): the
 * write's own liveness keeps the question where the reader pressed it, released once the server
 * decides it.
 *
 * No listeners: the column reads this at the commit the row leaves, which is a commit the
 * optimistic removal itself causes, and the write's own state re-renders every surface showing it.
 */
const outstanding = new Set<string>();

/** Holds the ask while the reader's own answer to it is outstanding, or lets it go. */
export function markAnswerOutstanding(askId: string, held: boolean): void {
  if (held) outstanding.add(askId);
  else outstanding.delete(askId);
}

/** Whether the reader's own answer to `askId` is still the server's to decide. */
export function answerOutstanding(askId: string): boolean {
  return outstanding.has(askId);
}

/** Forgets every hold. The unit-test preload calls it after each test, since the store outlives
 *  any one render. */
export function resetOutstandingAnswers(): void {
  outstanding.clear();
}

/** Every cached read of the open asks and of the one read before the write took its ask's row out
 *  (`open-asks-cache.ts`, `one-read-cache.ts`), kept so a refused write puts them back. Undefined
 *  where the write left the Inbox alone - a change to an answer the server has already recorded,
 *  whose ask has no open row there - and so kept nothing. */
type InboxRowsBefore = InboxAsksSnapshot | undefined;

/** What every press of an answer or a decline carries: the ask it is of. `retry()` runs the
 *  newest render's `write` and callbacks with the press's own values (`useKeyedWrite`), and a
 *  host may draw one card for one ask after another, so the ask a write is sent to, the row it
 *  takes out of the Inbox and the ask it holds are the press's, whichever ask the card shows. */
export interface AskPress {
  readonly ask: Ask;
}

/** What an answer or a decline is written with, beyond what the hold itself does. */
export interface OutstandingAnswerWriteOptions<TData, TVariables extends AskPress> {
  /** What the shell's pending-writes indicator names this write, in the reader's words. */
  readonly label: (variables: TVariables) => string;
  /** The method of the request `write` makes (`KeyedWriteMade`). */
  readonly method: WriteMethod;
  readonly write: (variables: TVariables, bounds: WriteBounds) => Promise<TData>;
  /** Whether a press takes its ask's row out of the Inbox and holds the ask: false for one that
   *  leaves both alone. Read as each press goes - for its hold on the Inbox's reads, then for the
   *  removal, in the one step - so a refusal restores exactly what that press took out. Every
   *  press does by default. */
  readonly removesRow?: (variables: TVariables) => boolean;
  /** Called after the row is back, so a refusal the surface reads is read on the restored list. */
  readonly onError?: (error: Error, variables: TVariables, before: InboxRowsBefore) => void;
  readonly onSettled?: () => void;
  /** Reads again what the write moved, the one read among them (`invalidateOwnerReads`): this
   *  write reads the one read again itself only where the server refuses it. */
  readonly onSuccess?: (data: TData | null, variables: TVariables) => void;
}

/**
 * The reader's own answer to an ask, or their decline of it, as one write: it takes the ask's
 * row out of the Inbox - the open asks and the one read - the moment it goes,
 * puts it back where the server refuses it, holds the ask here for as long as the answer is theirs
 * to finish, and reads the one read again once the server has decided: on a refusal here, after
 * the surface's own `onError`, and on a landing in the surface's `onSuccess`, which reads it again
 * with the ask's other reads. Once, either way: a second invalidation made while the first read is
 * out would only queue the same read behind it (`DispatchQueryClient`).
 *
 * The order is the point, and it is why the two writes share this rather than each keeping its
 * own copy. The hold is registered before the row goes, never a commit after it: the reading
 * column reads the hold at the commit the removal below makes, and a hold arriving later would
 * reach it once the card about to show a refusal had been unmounted with the question.
 *
 * The write is inline (`useKeyedWrite`): the surface that wrote shows the refusal itself, over
 * what the reader wrote, so the shell's indicator carries the write only while it is out. The
 * hold lasts the same `pending || failed` liveness the snooze reports its own write by
 * (`SnoozeControl`), let go when the server decides the answer, when the form is reset, and when
 * the card goes. A write that takes the row out holds the reads of the open asks and the one
 * read until Dispatch has taken it (`holds`: its answer, or the ask's `ask.answered`
 * or `ask.resolved` event, and at most `SEND_DEADLINE_MS`): one asked before could be answered
 * with the row still in it. The Inbox lists the asks other people open, so the hold ends on the
 * event rather than wait out an answer that is late.
 */
export function useOutstandingAnswerWrite<TData, TVariables extends AskPress>({
  label,
  method,
  onError,
  onSettled,
  onSuccess,
  removesRow,
  write,
}: OutstandingAnswerWriteOptions<TData, TVariables>): KeyedWriteResult<
  TData,
  TVariables,
  InboxRowsBefore
> {
  const queryClient = useQueryClient();
  const mutation = useKeyedWrite<TData, TVariables, InboxRowsBefore>({
    holds: (press) => ({
      endsOn: askWritten(press.ask.id, ["ask.answered", "ask.resolved"]),
      keys: removesRow === undefined || removesRow(press) ? inboxAsksKeys(queryClient) : [],
    }),
    inline: true,
    label,
    method,
    write,
    onMutate: async (press) => {
      if (removesRow !== undefined && !removesRow(press)) return undefined;
      markAnswerOutstanding(press.ask.id, true);
      await cancelInboxAsks(queryClient);
      const before = inboxAsksSnapshot(queryClient);
      patchInboxAsks(queryClient, (row) => (row.id === press.ask.id ? undefined : row));
      return before;
    },
    onError: (error, variables, before) => {
      if (before !== undefined) {
        restoreInboxAsks(queryClient, before);
        void invalidateOpenAsks(queryClient);
      }
      onError?.(error, variables, before);
      void invalidateMeInbox(queryClient);
    },
    onSettled,
    onSuccess,
  });
  // The ask held is the newest press's, the one whose answer is still out or refused.
  useLiveRegistration(livePress(mutation)?.ask.id, markAnswerOutstanding);
  return mutation;
}
