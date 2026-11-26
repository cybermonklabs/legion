import { type QueryClient, type QueryKey, useQueryClient } from "@tanstack/react-query";
import { createContext, useMemo } from "react";

import { api, type WriteMethod } from "../../api/client";
import { inboxQuery, whoAmIQuery } from "../../api/queries";
import type { Ask, InboxRow } from "../../api/types";
import { askWritten } from "../../api/write-events";
import { type KeyedWriteOptions, stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { type AskPress, pressAsk, useAskWrites } from "./ask-writes";
import { invalidateOwnerReads } from "./owner-reads";

export interface AskHandOnWrite {
  /** The server's reason after a refused hand-on, or what one the reader stopped may have done
   *  and what Retry does with it, until the next attempt. */
  error: string | undefined;
  /** True after a hand-on failed until the next attempt starts. */
  failed: boolean;
  /** True while the hand-on is out, until Dispatch decides it or the reader stops it. */
  pending: boolean;
  /** Whether the hand-on is out past its deadline (`KeyedWriteResult.late`): still `pending`,
   *  but the control it holds is let go. */
  late: boolean;
  /** Repeats the failed hand-on. */
  retry: () => void;
  /** Hands the ask to `askedOf`, the email of somebody who has signed in. */
  submit: (askedOf: string) => void;
}

/** One press of a hand-on: the ask it hands on (`AskPress`), and the email of the person it goes
 *  to. */
interface HandOnPress extends AskPress {
  readonly askedOf: string;
}

/** A hand-on is a `PUT`. */
const HAND_ON_METHOD: WriteMethod = "PUT";

/** What a stopped hand-on may have done and what Retry does with it: said of the hand-on, since
 *  the error's own words are a composer's, of a message. */
const STOPPED_HAND_ON = "the hand-on of this question";

/**
 * What every hand-on is written with, by a card's own write (`useAskHandOn`) and by an Inbox's
 * (`useAskHandOnWrites`) alike, over `PUT /api/v1/asks/{id}/asked-of`. The new person lands on
 * every inbox cache at once, so the row leaves the list it was in and joins theirs on the press,
 * rolls back if the server refuses, and every read carrying the ask - the Inbox and its owner's
 * lists (`invalidateOwnerReads`), and the ask's own thread - refetches once the hand-on settles.
 *
 * What is patched is what the server will answer with: `waiting_on_person`, which is the person
 * named either way, and the three stored fields behind it - `asked_of`, empty when the ask was
 * handed to the assignee of its own issue, since an empty field means the ask follows that
 * issue again; `asked_of_by`, the reader; and `asked_of_at`, now. The reader is taken from the
 * `/auth/whoami` read the shell already holds rather than read again: it is in the cache before
 * any card can be pressed, and a hand-on made without it leaves the pair unnamed until the
 * refetch answers.
 *
 * Each press carries the ask it hands on: a Retry runs the newest render's `write` and callbacks
 * with the press's own values (`useKeyedWrite`), and a host may draw one card for one ask after
 * another, so a Retry hands the stopped press's ask to the person picked for it, and what it
 * patches and reads again are that ask's, whichever ask the card is drawn for by then.
 */
function handOnWrite(
  queryClient: QueryClient
): KeyedWriteOptions<unknown, HandOnPress, [QueryKey, InboxRow[] | undefined][]> {
  const inboxKey = inboxQuery().queryKey;
  // The card says why its hand-on was refused, where the reader pressed it. A read of the Inbox
  // asked while the hand-on is out waits until Dispatch has taken it (its answer or the ask's
  // `ask.asked_of` event): Dispatch can answer it before, with the person from before the press.
  return {
    method: HAND_ON_METHOD,
    write: (press, bounds) => api.handAskTo(press.ask.id, press.askedOf, bounds),
    holds: (press) => ({ endsOn: askWritten(press.ask.id, ["ask.asked_of"]), keys: [inboxKey] }),
    inline: true,
    label: ({ askedOf }) => `Ask handed to ${askedOf}`,
    onMutate: async (press) => {
      await queryClient.cancelQueries({ queryKey: inboxKey });
      const previous = queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey });
      const by = queryClient.getQueryData(whoAmIQuery().queryKey)?.login ?? null;
      const at = new Date().toISOString();
      queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
        current?.map((row) =>
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
            : row
        )
      );
      return previous;
    },
    onError: (_error, _press, previous) => {
      for (const [key, data] of previous ?? []) {
        queryClient.setQueryData(key, data);
      }
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
  };
}

/**
 * The hand-on of a card drawn outside an Inbox - the issue page's Conversation, a document's
 * margin and its ask block, the Answers page - as the card's own write (`handOnWrite`), which
 * lives as long as the card. Nothing on those surfaces takes the ask out from under the card, so
 * nothing has to outlive it. Inside an Inbox the card shows the Inbox's write instead
 * (`useAskHandOnWrites`).
 */
export function useAskHandOn(ask: Ask): AskHandOnWrite {
  const mutation = useKeyedWrite({ ...handOnWrite(useQueryClient()), readsLate: true });
  return {
    // A hand-on the reader stopped may have been made, and Retry makes it again (`retry`).
    error: stoppedNote(mutation, STOPPED_HAND_ON, "made") ?? mutation.error?.message,
    failed: mutation.isError,
    late: mutation.late,
    pending: mutation.isPending,
    retry: mutation.retry,
    submit: (askedOf) => mutation.mutate({ ask, askedOf }),
  };
}

/** Every card's hand-on in one Inbox (`useAskHandOnWrites`). */
export interface AskHandOnWrites {
  /** The asks whose hand-on is out or failed, which the Inbox marks as the reader's own writes
   *  (`AskWrites.live`). */
  readonly live: ReadonlySet<string>;
  /** The asks whose hand-on is out. The Inbox keeps each one's row where the reader saw it, if
   *  it drew one, until the write lands (`heldRow`). A refused or stopped hand-on is not out, so
   *  the next read of the Inbox places its row. */
  readonly out: ReadonlySet<string>;
  /** The hand-on of `ask`. Every card drawn for that ask shows this one write. */
  readonly of: (ask: Ask) => AskHandOnWrite;
}

/**
 * Every card's hand-on in one Inbox (`handOnWrite`), held by ask in the Inbox's own state
 * (`useAskWrites`), as each row's snooze is. The optimistic `waiting_on_person` takes the row out
 * of the reader's own list the moment the write starts (`isMine`, `inAssigneeFilter`), and the
 * card that pressed can go before the server answers: the reading column's card goes whenever
 * the reader moves the selection. So the Inbox keeps the row where the reader saw it while the
 * write is out, and the write and its refusal outlive the card: every card drawn for the ask,
 * and the ask's short row beside the column, shows that one write, with its Retry.
 */
export function useAskHandOnWrites(): AskHandOnWrites {
  const writes = useAskWrites(pressAsk, handOnWrite(useQueryClient()));
  return useMemo(
    () => ({
      live: writes.live,
      of: (ask: Ask): AskHandOnWrite => {
        const entry = writes.newest.get(ask.id);
        return {
          error:
            entry?.state !== "failed"
              ? undefined
              : (stoppedNote(
                  { error: entry.failure, method: HAND_ON_METHOD },
                  STOPPED_HAND_ON,
                  "made"
                ) ?? entry.failure.message),
          failed: entry?.state === "failed",
          late: writes.late.has(ask.id),
          pending: entry?.state === "out",
          retry: () => writes.retry(ask.id),
          submit: (askedOf) => writes.press({ ask, askedOf }),
        };
      },
      out: writes.out,
    }),
    [writes]
  );
}

/**
 * The hand-on writes of the Inbox around a card (`useAskHandOnWrites`), as a context rather than
 * a prop.
 *
 * The hand-on's optimistic `waiting_on_person` takes the row out of the Inbox's own list the
 * moment the write starts (`isMine`, `inAssigneeFilter`), and the card that pressed can go before
 * the server answers: the reading column's card goes whenever the reader moves the selection. So
 * the Inbox holds every card's hand-on itself, by ask, and keeps the ask's row where the reader
 * saw it while that write is out (`heldRow`). The write and its refusal outlive the card: the
 * card drawn for the ask again, and the ask's short row beside the column, show that one write,
 * with its Retry. A refused or stopped hand-on holds no row: the rollback has put the person back,
 * and the next read of the Inbox places the row.
 *
 * It is a context because the card the reader pressed need not be one the Inbox rendered itself.
 * A question written as a decision block is drawn in the reading column by the document page's
 * own components - `ReadingPane` -> `DocumentPane` (which lives in `PaneDocument.tsx`) ->
 * `ProofDocument` -> `AskBlockCard` - which carry no prop of the Inbox's, and threading one
 * through them would leave the next host free to omit it exactly as that path did. `AskHandOn`
 * reads the writes from here instead, so every card under an Inbox has them and no host can be
 * written without them. The reader's answer, decline, question back and reply reach the Inbox's
 * writes the same way (`outstanding-answers.ts`), and so does every row's snooze
 * (`useAskSnoozeWrites`) and every Assign to me (`AskAssignWritesProvider`).
 *
 * Reaching every card is also why the Inbox holds only rows it drew. Many cards under it speak
 * for an ask it draws no row for: the card of a Reply row's thread, whose ask can be one the
 * reader's filters hide, and every ask block of a document open in the column beside the
 * selected one. The Inbox keeps a row for a hand-on out only where the row is among those it
 * last drew (`Inbox.tsx`, `heldRow`), so none of those can pull a hidden question into the list.
 *
 * Each Inbox provides its own - the page's and the drawer's are separate. With no provider a card
 * makes its own write and holds nothing (`useAskHandOn`), which is what a card drawn outside a
 * list does: the issue page's Conversation, a document's margin, its ask block, the Answers page.
 */
export const AskHandOnWritesContext = createContext<AskHandOnWrites | undefined>(undefined);
