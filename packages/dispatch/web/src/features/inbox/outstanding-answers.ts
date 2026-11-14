import { useEffect, useRef } from "react";
import { useCache } from "../../api/cache/cache";

import type { WriteRequest } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import type { InboxRow } from "../../api/types";
import { type KeyedWriteResult, useKeyedWrite } from "../writes/useKeyedWrite";

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
 * What it is for: `useOutstandingAnswerWrite` below drops the answered row from the `["inbox"]`
 * read the moment the answer goes out, so without this hold the column would hand the selection
 * on at once and unmount the card that wrote - and that card is the only surface a refused
 * answer is shown on, since `useKeyedWrite({inline: true})` lets a refused write go from the
 * store. The hold is the rule a deferred row already follows (`snoozing` in `Inbox.tsx`): the
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

/** What an answer or a decline is written with, beyond what the hold itself does. */
export interface OutstandingAnswerWriteOptions<TData, TVariables> {
  readonly askId: string;
  /** What the shell's pending-writes indicator names this write, in the reader's words. */
  readonly label: (variables: TVariables) => string;
  /** The request this write makes (`askAnswerRequest`, `askDeclineRequest`): what goes out, and
   *  what a write waiting its turn in the ask's line is kept as for a reload. */
  readonly request: (variables: TVariables) => WriteRequest<TData>;
  /** Whether this write takes the ask's row out of the Inbox and holds the ask: false for one
   *  that leaves both alone. Read once, as the write goes, so the change it shows is exactly
   *  what that press took out. Every write does by default. */
  readonly removesRow?: () => boolean;
  /** Called once the removal has stopped being composed, so a refusal the surface reads is read
   *  on the list Dispatch last gave. */
  readonly onError?: (error: Error, variables: TVariables) => void;
  readonly onSettled?: () => void;
  readonly onSuccess?: (data: TData | null) => void;
}

/**
 * The reader's own answer to an ask, or their decline of it, as one write: it takes the ask's
 * row out of the Inbox the moment it goes, and holds the ask here for as long as the answer is
 * theirs to finish.
 *
 * The order is the point, and it is why the two writes share this rather than each keeping its
 * own copy. The hold is registered before the row goes, never a commit after it: the reading
 * column reads the hold at the commit the removal below makes, and a hold arriving later would
 * reach it once the card about to show a refusal had been unmounted with the question.
 *
 * The removal is the store's to show (`WriteSpec.shows`), so it is shown again over each answer
 * that lands on the Inbox's read while the write is live (R7 of dispatch://INFRA-299): the
 * answer arrives whole and the question the reader has answered does not come back under their
 * own answer. Whether this press removes the row at all is `removesRow`, read once as the write
 * goes and held with the change it shows. A refused one stops being composed: the Inbox's read
 * is what Dispatch last gave it with every other live write's change over it, and is read again
 * (R10).
 *
 * The write is inline (`useKeyedWrite`): the surface that wrote shows the refusal itself, over
 * what the reader wrote, so the shell's indicator carries the write only while it is out. The
 * hold lasts the same `pending || failed` liveness the snooze reports its own write by
 * (`SnoozeControl`), let go when the server decides the answer, when the form is reset, and when
 * the card goes.
 */
export function useOutstandingAnswerWrite<TData, TVariables>({
  askId,
  label,
  onError,
  onSettled,
  onSuccess,
  removesRow,
  request,
}: OutstandingAnswerWriteOptions<TData, TVariables>): KeyedWriteResult<TData, TVariables, unknown> {
  const cache = useCache();
  /** Whether the press now going takes the ask's row out: `removesRow` read once, where it is
   *  read, and held for the change that press shows. */
  const removing = useRef(false);
  const mutation = useKeyedWrite<TData, TVariables>({
    inline: true,
    label,
    request,
    target: () => `ask:${askId}`,
    // A function of the Inbox's read as it stands - the ask's row is not in it - so showing it
    // again over an answer that lands there gives the same list.
    shows: () => {
      const removes = removing.current;
      return (shown) => {
        if (!removes) return;
        shown.on<InboxRow[]>(inboxQuery().queryKey, (current) =>
          current.filter((row) => row.id !== askId)
        );
      };
    },
    onMutate: async () => {
      removing.current = removesRow === undefined || removesRow();
      if (!removing.current) return;
      markAnswerOutstanding(askId, true);
      // A read already out was asked before this press.
      await cache.cancel({ queryKey: inboxQuery().queryKey });
    },
    onError,
    onSettled,
    onSuccess,
  });
  const live = mutation.isPending || mutation.isError;
  useEffect(() => {
    if (!live) return;
    markAnswerOutstanding(askId, true);
    return () => markAnswerOutstanding(askId, false);
  }, [askId, live]);
  return mutation;
}
