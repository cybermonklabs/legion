import { useQueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Ask, OpenAskRow } from "../../api/types";
import { askWritten } from "../../api/write-events";
import { livePress, stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import {
  cancelInboxAsks,
  inboxAsksKeys,
  inboxAsksSnapshot,
  type OpenAsksSnapshot,
  patchInboxAsks,
  restoreInboxAsks,
} from "./open-asks-cache";
import { invalidateOwnerReads } from "./owner-reads";

export interface AskHandOnWrite {
  /** The server's reason after a refused hand-on, or what one the reader stopped may have done
   *  and what Retry does with it, until the next attempt. */
  error: string | undefined;
  /** True after a hand-on failed until the next attempt starts. */
  failed: boolean;
  /** The ask the newest hand-on is of while that write is in flight or failed, which the list
   *  around the card holds (`hand-on-live.ts`, `useLiveRegistration`); `undefined` while none
   *  is. */
  liveAskId: string | undefined;
  pending: boolean;
  /** Repeats the failed hand-on. */
  retry: () => void;
  /** Hands the ask to `askedOf`, the email of somebody who has signed in. */
  submit: (askedOf: string) => void;
}

/**
 * The one way the SPA hands an ask to a person. The new person lands on every inbox cache at
 * once, so the row leaves the list it was in and joins theirs on the press, rolls back if the
 * server refuses, and every read carrying the ask - the open asks and its owner's lists
 * (`invalidateOwnerReads`), the one read (`one-read-cache.ts`), and the ask's own thread -
 * refetches once the hand-on settles, the same shape as `useAskSnooze`, over
 * `PUT /api/v1/asks/{id}/asked-of`. The reader's own asks are the one read's alone, so an ask
 * handed to the reader joins that read's Inbox answer as well, where it held no entry yet;
 * without it the row would leave the open asks' list and join no other.
 *
 * What is patched is what the server will answer with: `waiting_on_person`, which is the person
 * named either way, and the three stored fields behind it - `asked_of`, empty when the ask was
 * handed to the assignee of its own issue, since an empty field means the ask follows that
 * issue again; `asked_of_by`, the reader; and `asked_of_at`, now. The reader is taken from the
 * `/auth/whoami` read the shell already holds rather than read again: it is in the cache before
 * any card can be pressed, and a hand-on made without it leaves the pair unnamed until the
 * refetch answers.
 *
 * Each press carries the ask it hands on: `retry()` runs the newest render's `write` and
 * callbacks with the press's own values (`useKeyedWrite`), and a host may draw one card for one
 * ask after another, so a Retry hands the stopped press's ask to the person picked for it, and
 * what it patches, reads again and holds in the list while it is live (`liveAskId`) are that
 * ask's, whichever ask the hook is rendered for by then.
 */
export function useAskHandOn(ask: Ask): AskHandOnWrite {
  const queryClient = useQueryClient();
  // The card says why its hand-on was refused, where the reader pressed it. A read of the open
  // asks or of the one read asked while the hand-on is out waits until Dispatch has taken it (its
  // answer or the ask's `ask.asked_of` event): Dispatch can answer it before, with the person from
  // before the press.
  const mutation = useKeyedWrite({
    method: "PUT",
    write: (press: { ask: Ask; askedOf: string }, bounds) =>
      api.handAskTo(press.ask.id, press.askedOf, bounds),
    holds: (press) => ({
      endsOn: askWritten(press.ask.id, ["ask.asked_of"]),
      keys: inboxAsksKeys(queryClient),
    }),
    inline: true,
    label: ({ askedOf }) => `Ask handed to ${askedOf}`,
    onMutate: async (press) => {
      await cancelInboxAsks(queryClient);
      const previous = inboxAsksSnapshot(queryClient);
      const by = queryClient.getQueryData(whoAmIQuery().queryKey)?.login ?? null;
      const at = new Date().toISOString();
      const handedOn = (row: OpenAskRow): OpenAskRow =>
        row.id === press.ask.id
          ? {
              ...row,
              // Naming the assignee of the ask's own issue stores no person: the ask
              // follows that issue again, and whom it waits on is the person named either
              // way.
              asked_of: press.askedOf === (row.issue?.assignee ?? null) ? null : press.askedOf,
              asked_of_by: by,
              asked_of_at: at,
              waiting_on_person: press.askedOf,
            }
          : row;
      // The first row the open asks held of the ask, which joins the reader's own Inbox answer
      // where the ask is handed to them and it held no entry of it yet.
      const held = press.askedOf === by ? firstRowOf(previous.openAsks, press.ask.id) : undefined;
      patchInboxAsks(queryClient, handedOn, held === undefined ? [] : [handedOn(held)]);
      return previous;
    },
    onError: (_error, _press, previous) => {
      restoreInboxAsks(queryClient, previous);
    },
    onSettled: (_data, _error, press) => {
      // Every surface that draws this ask follows the write, not the `ask.asked_of` event that
      // lands after it: on the issue page, in a document's margin and in its ask block the card
      // reads the ask from its owner's list, where the control's own value comes from, so an
      // Inbox-only refresh would snap the picker back to the person before the hand-on.
      invalidateOwnerReads(queryClient, press.ask);
      // The Inbox's own Reply-thread card is the one surface that reads the ask from its thread
      // instead (`ThreadBody`, `askThreadQuery`), which is kept for the session and never goes
      // stale of itself. It is read again here rather than in `invalidateOwnerReads`, whose
      // other caller - `useAskAnswerForm` - already refreshes that thread in each branch that
      // moves it, and whose own subject is the reads that carry an ask besides its own thread.
      void queryClient.invalidateQueries({ queryKey: ["ask-thread", press.ask.id] });
    },
  });
  return {
    // A hand-on the reader stopped may have been made, and Retry makes it again (`retry`): the
    // error's own words are a composer's, of a message.
    error: stoppedNote(mutation, "the hand-on of this question", "made") ?? mutation.error?.message,
    failed: mutation.isError,
    liveAskId: livePress(mutation)?.ask.id,
    pending: mutation.isPending,
    retry: mutation.retry,
    submit: (askedOf) => mutation.mutate({ ask, askedOf }),
  };
}

/** The first row of `askId` that `reads`, each read of the open asks as a write kept it, holds. */
function firstRowOf(reads: OpenAsksSnapshot, askId: string): OpenAskRow | undefined {
  for (const [, rows] of reads) {
    const row = rows?.find((each) => each.id === askId);
    if (row !== undefined) return row;
  }
  return undefined;
}
