import { useQueryClient } from "@tanstack/react-query";
import { createContext, type ReactNode, useMemo, useState } from "react";

import { askSnoozeRequest, askUnsnoozeRequest, type WriteMethod } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import { holdBackReads } from "../../api/query-client";
import type { InboxRow } from "../../api/types";
import { snoozeWritten } from "../../api/write-events";
import { newSendKey } from "../../lib/send-key";
import { refusedByDispatch, SendStoppedError } from "../writes/keyed-write";
import { stoppedNote, useLateWrites } from "../writes/useKeyedWrite";
import { plainWrite, useWrites } from "../writes/write-store";
import { useAskWrites, useInboxWrites } from "./ask-writes";

export interface AskSnoozeWrite {
  /** The server's reason after a refused save, or what one the reader stopped may have done and
   *  what Retry does with it, until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  /** The press the failed save was while `failed` holds - its ask, and the moment of a snooze or
   *  `null` for an un-snooze - which `retry` makes again; `undefined` otherwise. */
  failedPress: AskSnoozePress | undefined;
  /** True while the newest save is out, until Dispatch decides it or the reader stops it. */
  pending: boolean;
  /** Whether the newest save is out past its deadline, or waits its turn on its ask's snooze line
   *  behind a write that is (`AskWrites.late`): still `pending`, but the controls it holds are
   *  let go. */
  late: boolean;
  /** What the in-flight write is doing, and `undefined` when none is: the moment a snooze is
   *  saving, or `null` for an un-snooze. The optimistic update has already moved the row by
   *  the time it renders, so this - not the row's own `snoozed_until` - is what the reader
   *  just asked for. */
  pendingUntil: string | null | undefined;
  /** Repeats the failed save; `onSettled` hears whether the server took it. */
  retry: (onSettled?: SnoozeSettled) => void;
  /** Snoozes the row until `until` (RFC3339), or brings it back with null; `onSettled` hears
   *  whether the server took this press. */
  submit: (until: string | null, onSettled?: SnoozeSettled) => void;
}

/** Told once a press settles: true where the server took it, false where it refused it. */
export type SnoozeSettled = (landed: boolean) => void;

/** One press of a row's snooze: the ask it is of, and the moment, or `null` to bring it back. */
export interface AskSnoozePress {
  readonly askId: string;
  readonly until: string | null;
}

/** Who hears one press settle, with the press it is of. */
interface SnoozeListener {
  readonly onSettled: SnoozeSettled;
  readonly press: AskSnoozePress;
}

/** A snooze is a `PUT`, and bringing the row back a `DELETE`. */
function snoozeMethod({ until }: AskSnoozePress): WriteMethod {
  return until === null ? "DELETE" : "PUT";
}

/** The ask a press of a snooze is of. */
function snoozeAsk({ askId }: AskSnoozePress): string {
  return askId;
}

/** The line every snooze write of `askId` goes out on (`WriteSpec.target`): a row's own
 *  (`useAskSnoozeWrites`) and the bar's (`useAskSnoozeMany`) alike. Writes to one target go out
 *  in press order, so of the snooze writes of one ask an older one never lands after a newer one
 *  and overwrites the reader's newer choice. */
function askSnoozeTarget(askId: string): string {
  return `ask-snooze:${askId}`;
}

/** Every row's snooze write in one Inbox (`useAskSnoozeWrites`). */
export interface AskSnoozeWrites {
  /** The asks whose snooze write is out or failed, which the Inbox marks as the reader's own
   *  writes (`AskWrites.live`). */
  readonly live: ReadonlySet<string>;
  /** The asks whose snooze write is out. The Inbox keeps each one's row in the band the reader
   *  saw it in until the write lands (`heldRow`). A refused or stopped write is not out, so the
   *  next read of the Inbox places its row. */
  readonly out: ReadonlySet<string>;
  /** The snooze write of `askId`. Every control drawn for that ask shows this one write. */
  readonly of: (askId: string) => AskSnoozeWrite;
}

/**
 * The snooze writes of the Inbox around a control (`useAskSnoozeWrites`). Each Inbox provides its
 * own (`AskSnoozeWritesProvider`): the page's and the drawer's are separate. A row's Snooze is
 * drawn only inside an Inbox, so a control with no provider is a mistake in the code, and
 * `useInboxWrites` throws for it.
 */
export const AskSnoozeContext = createContext<AskSnoozeWrites | undefined>(undefined);

/** Makes the snooze writes of one Inbox (`useAskSnoozeWrites`) and provides them to `children`.
 *  The writes' own steps re-render this component alone: what it draws re-renders only when the
 *  writes it provides change. */
export function AskSnoozeWritesProvider({ children }: { children: ReactNode }): ReactNode {
  return <AskSnoozeContext value={useAskSnoozeWrites()}>{children}</AskSnoozeContext>;
}

/** The snooze write of `askId` in the Inbox around the control (`AskSnoozeContext`). */
export function useAskSnooze(askId: string): AskSnoozeWrite {
  return useInboxWrites(AskSnoozeContext).of(askId);
}

/**
 * The one way the SPA writes a row's snooze, for every row of one Inbox. The new moment lands on
 * every inbox cache at once, rolls back if the server refuses, and the list refetches once the
 * save settles - the same shape as the issue field writes, without their cross-surface caches: a
 * snooze is on the ask and shows nowhere else.
 *
 * The Inbox holds these writes by ask (`useAskWrites`), and the control that pressed holds none
 * of them. A control can go while its write is out: the reading column's head goes when the
 * reader moves the selection. So the row stays in the band the reader saw it in until its write
 * lands (`out`), and moves into `Later` (or back out) then - save under the Blocked on you
 * narrowing, which lists no snoozed ask and keeps the row only while the reader's hand is on it.
 * A refusal stays with the ask until its next press, and every control drawn for the ask shows
 * it, with its Retry. A refused or stopped write holds no row: the rollback has put the row's
 * moment back, and the next read of the Inbox places it.
 *
 * Every snooze write of one ask, a row's and the bar's (`useAskSnoozeMany`), goes out on that
 * ask's one line (`askSnoozeTarget`), in press order: a press waits its turn while an earlier
 * write of the ask is still being tried. The bar lets its controls go once its writes are past
 * their deadline (offline, say), so the reader can pick a moment on a row whose bulk snooze is
 * still out; sent at once, that pick could land first and the bulk's write after it, putting
 * back the moment the reader has just replaced. On the line it waits, and lands last. A write on
 * a line is made from its request alone (`askSnoozeRequest`, `askUnsnoozeRequest`): what goes out
 * once its turn comes, and what a press waiting its turn is kept as for a reload.
 *
 * Each press carries the ask it is of: a Retry runs the newest render's `request` with the
 * press's own values (`useKeyedWrite`), so every byte a Retry sends rides in the press. The Retry
 * of a press the reader stopped makes that press again under its key; the Retry of a press
 * Dispatch refused is a new press of the same values (`useAskWrites`).
 *
 * The caller of `submit` and `retry` hears whether the server took that press (`onSettled`),
 * which is how a row's Snooze hands the reader on once its snooze lands (R8): of each ask's
 * newest press only, since a newer press of the ask takes its place, and whichever control made
 * it, since the write outlives the control. A Retry makes that very press again, so the listener
 * held beside it is found by that press.
 */
export function useAskSnoozeWrites(): AskSnoozeWrites {
  const queryClient = useQueryClient();
  const inboxKey = inboxQuery().queryKey;
  /** Each ask's newest press's listener, set by each `submit` and `retry` and let go once that
   *  press is heard or a newer press of the ask takes its place. */
  const [listeners] = useState(() => new Map<string, SnoozeListener>());
  /** Tells the listener of the newest press of `press`'s ask whether the server took `press`; a
   *  press before it tells nobody. */
  const heard = (press: AskSnoozePress, landed: boolean): void => {
    const listening = listeners.get(press.askId);
    if (listening === undefined || listening.press !== press) return;
    listeners.delete(press.askId);
    listening.onSettled(landed);
  };
  // A read of the Inbox asked while a snooze is out waits until Dispatch has taken it (its answer
  // or the viewer's snooze event): Dispatch can answer it before, with the row's moment from
  // before the press.
  const writes = useAskWrites(snoozeAsk, {
    holds: () => ({ endsOn: snoozeWritten, keys: [inboxKey] }),
    // On the ask's snooze line, with the bar's writes of it, in press order (`askSnoozeTarget`).
    target: ({ askId }: AskSnoozePress) => askSnoozeTarget(askId),
    request: ({ askId, until }: AskSnoozePress) =>
      until === null ? askUnsnoozeRequest(askId) : askSnoozeRequest(askId, until),
    // The row says why its snooze was refused, not the shell's pending-writes notice.
    inline: true,
    label: ({ until }) => (until === null ? "Ask brought back now" : "Ask snoozed"),
    onMutate: async (press) => {
      await queryClient.cancelQueries({ queryKey: inboxKey });
      const previous = queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey });
      queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
        current?.map((row) =>
          row.id === press.askId ? { ...row, snoozed_until: press.until } : row
        )
      );
      return previous;
    },
    onError: (_error, press, previous) => {
      for (const [key, data] of previous ?? []) {
        queryClient.setQueryData(key, data);
      }
      heard(press, false);
    },
    onSuccess: (_answer, press) => heard(press, true),
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: inboxKey });
    },
  });
  return useMemo(() => {
    const listen = (press: AskSnoozePress, onSettled: SnoozeSettled | undefined) => {
      if (onSettled === undefined) listeners.delete(press.askId);
      else listeners.set(press.askId, { onSettled, press });
    };
    return {
      live: writes.live,
      of: (askId: string): AskSnoozeWrite => {
        const entry = writes.newest.get(askId);
        return {
          // A save the reader stopped may have been made, and Retry makes it again: the stop is
          // said of the snooze, not in a composer's words of a message.
          error:
            entry?.state !== "failed"
              ? undefined
              : (stoppedNote(
                  { error: entry.failure, method: snoozeMethod(entry.press) },
                  entry.press.until === null ? "the un-snooze" : "the snooze",
                  "saved"
                ) ?? entry.failure.message),
          failed: entry?.state === "failed",
          failedPress: entry?.state === "failed" ? entry.press : undefined,
          late: writes.late.has(askId),
          pending: entry?.state === "out",
          pendingUntil: entry?.state === "out" ? entry.press.until : undefined,
          retry: (onSettled) => {
            if (entry?.state === "failed") listen(entry.press, onSettled);
            writes.retry(askId);
          },
          submit: (until, onSettled) => {
            const press = { askId, until };
            listen(press, onSettled);
            writes.press(press);
          },
        };
      },
      out: writes.out,
    };
  }, [listeners, writes]);
}

/** One ask whose snooze Dispatch did not confirm: one it refused, with the reason it gave - the
 *  same message the per-row control shows, so a sign-out never reads like a validation refusal -
 *  or one the reader stopped (`stopped`), which may have been saved and is no refusal, so it
 *  carries no reason. */
export type AskSnoozeFailure =
  | { readonly askId: string; readonly reason: string; readonly stopped: false }
  | { readonly askId: string; readonly stopped: true };

export interface AskSnoozeManyWrite {
  /** True while a bulk save is in flight. */
  pending: boolean;
  /** Whether every ask's write of the bulk saves out is past its deadline, or waits its turn
   *  behind one that is (`useLateWrites`): still tried under its key until Dispatch decides it,
   *  so the saves are still `pending`, but the controls they hold are let go. False while no
   *  write of theirs is out. */
  late: boolean;
  /** Snoozes every id until `until` (RFC3339); resolves with the asks the server refused. */
  submit: (
    ids: readonly string[],
    until: string
  ) => Promise<{ failed: readonly AskSnoozeFailure[] }>;
}

/**
 * The bulk form of a row's snooze (`useAskSnoozeWrites`), over the same route once per ask: the
 * marked rows take the moment on the pick through the same optimistic write on every inbox
 * cache, and the list refetches once the whole set has settled. Outside the Blocked on you
 * narrowing, which lists no snoozed ask and keeps a row only while the reader's hand is on it,
 * the Inbox keeps the rows where the reader saw them until `submit`'s promise settles
 * (`useInboxSelection`'s `snoozing`), so they move into `Later` once the whole set has landed.
 * Each ask is a write of its own, under a key of its own and tried until Dispatch decides it, so
 * a transport failure is tried again rather than counted a refusal, and each settles on its own:
 * a refusal rolls back that row alone - to the moment it had before, not to null, which would
 * un-snooze a row that was already deferred - and the ones the server took stay where they
 * landed. A read of the Inbox asked while the set is out waits until the last of them is decided,
 * or until as many snooze events as asks have come, as a row's own snooze's does; an ask
 * Dispatch refused with no earlier attempt of its snooze that may have landed unseen publishes no
 * event, and the hold waits for none of it. Since a write that never answers - offline - is
 * tried until the reader stops it, the set's writes are read in the store under the keys they
 * were made with (`late`), so the controls the set holds let go once every one of them still out
 * is past its deadline. That lets the reader pick a moment on a row while the bar's write of its
 * ask is still out: each ask's write goes out on that ask's snooze line (`askSnoozeTarget`), the
 * line a row's own snooze of it goes out on (`useAskSnoozeWrites`), so the row's pick waits its
 * turn behind the bar's write and lands after it, rather than being overwritten by it.
 */
export function useAskSnoozeMany(): AskSnoozeManyWrite {
  const queryClient = useQueryClient();
  const writes = useWrites();
  const inboxKey = inboxQuery().queryKey;
  const [pending, setPending] = useState(0);
  /** The key of each ask's write of the bulk saves out, from the press until it is decided. */
  const [out, setOut] = useState<readonly string[]>([]);
  const lateKeys = useLateWrites(out);
  return {
    late: out.length > 0 && out.every((key) => lateKeys.has(key)),
    pending: pending > 0,
    submit: async (ids, until) => {
      // Built before anything is written down: the builder refuses an id the route's pattern has
      // no value for, and a refusal here, inside `ids.map` below, would escape `submit` after the
      // optimistic write with `pending` raised and no row rolled back. No caller sends an empty
      // id, so this is where that stays true rather than a case to handle.
      const writeOf = ids.map((id) => ({
        id,
        key: newSendKey(),
        request: askSnoozeRequest(id, until),
      }));
      const keys = new Set(writeOf.map(({ key }) => key));
      setOut((current) => [...current, ...keys]);
      setPending((count) => count + 1);
      const release = holdBackReads(queryClient, {
        endsOn: snoozeWritten,
        events: ids.length,
        keys: [inboxKey],
      });
      let failed: AskSnoozeFailure[];
      // How many of the asks Dispatch refused with no earlier attempt of their snooze that may
      // have landed unseen (`refusedByDispatch`), which publish no event.
      let refusals = 0;
      try {
        await queryClient.cancelQueries({ queryKey: inboxKey });
        const previous = queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey });
        const marked = new Set(ids);
        queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
          current?.map((row) => (marked.has(row.id) ? { ...row, snoozed_until: until } : row))
        );
        const results = await Promise.allSettled(
          writeOf.map(({ id, key, request }) => {
            const run = writes.run({
              detail: null,
              inline: true,
              key,
              kind: plainWrite,
              label: "Ask snoozed",
              presentation: "indicator",
              request,
              target: askSnoozeTarget(id),
            });
            // Each ask's write leaves `out` as it is decided, beside the set's own wait for all
            // of them, which this branch does not lengthen.
            const decided = () => setOut((current) => current.filter((each) => each !== key));
            run.then(decided, decided);
            return run;
          })
        );
        failed = ids.flatMap((askId, index): AskSnoozeFailure[] => {
          const result = results[index];
          if (result === undefined || result.status !== "rejected") return [];
          const reason: unknown = result.reason;
          if (reason instanceof SendStoppedError) return [{ askId, stopped: true }];
          return [
            {
              askId,
              reason: reason instanceof Error ? reason.message : String(reason),
              stopped: false,
            },
          ];
        });
        refusals = results.filter(
          (result) => result.status === "rejected" && refusedByDispatch(result.reason)
        ).length;
        const refused = new Set(failed.map(({ askId }) => askId));
        for (const [key, data] of refused.size === 0 ? [] : previous) {
          const before = new Map((data ?? []).map((row) => [row.id, row.snoozed_until]));
          queryClient.setQueryData<InboxRow[]>(key, (current) =>
            current?.map((row) =>
              refused.has(row.id) ? { ...row, snoozed_until: before.get(row.id) ?? null } : row
            )
          );
        }
      } finally {
        // Whatever happens above: a hold left on would keep every read of the Inbox waiting, and
        // a key left in `out` would keep the bulk's lateness read.
        release(refusals);
        setOut((current) => current.filter((each) => !keys.has(each)));
      }
      void queryClient.invalidateQueries({ queryKey: inboxKey });
      setPending((count) => count - 1);
      return { failed };
    },
  };
}
