import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { api, askSnoozeRequest } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import { holdBackReads } from "../../api/query-client";
import type { InboxRow } from "../../api/types";
import { snoozeWritten } from "../../api/write-events";
import { refusedByDispatch, SendStoppedError } from "../writes/keyed-write";
import { livePress, stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { plainWrite, useWrites } from "../writes/write-store";

export interface AskSnoozeWrite {
  /** The server's reason after a refused save, or what one the reader stopped may have done and
   *  what Retry does with it, until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  /** The press the failed save was while `failed` holds - its ask, and the moment of a snooze or
   *  `null` for an un-snooze - which `retry` makes again; `undefined` otherwise. */
  failedPress: AskSnoozePress | undefined;
  /** The ask the newest save is of while that save is in flight or failed, which the list around
   *  the row holds (`useLiveRegistration`); `undefined` while none is. */
  liveAskId: string | undefined;
  pending: boolean;
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

/**
 * The one way the SPA writes a row's snooze. The new moment lands on every inbox cache at once,
 * so the row drops into `Later` (or climbs back out) on the click, rolls back if the server
 * refuses, and the list refetches once the save settles - the same shape as the issue field
 * writes, without their cross-surface caches: a snooze is on the ask and shows nowhere else.
 *
 * Each press carries the ask it is of: `retry()` runs the newest render's `write` with the
 * press's own values (`useKeyedWrite`), so every byte a Retry sends rides in the press. A host
 * may draw the row's control for one ask after another, so the Retry of a press the reader
 * stopped is of that press's ask, and so is the ask the save holds in the list while it is live
 * (`liveAskId`), whichever ask the hook is rendered for by then.
 *
 * The caller of `submit` and `retry` hears whether the server took that press (`onSettled`): of
 * the newest press only, and only while the hook's host is mounted, as TanStack calls a
 * `mutate`'s own callbacks. A Retry takes no callbacks of its own (`useKeyedWrite`'s `retry`),
 * so the hook's own callbacks tell the listener held beside the newest press, found by that
 * press: a Retry makes that very press again.
 */
export function useAskSnooze(askId: string): AskSnoozeWrite {
  const queryClient = useQueryClient();
  const inboxKey = inboxQuery().queryKey;
  /** The newest press's listener, set by each `submit` and `retry` and let go once that press is
   *  heard or the host unmounts. */
  const listener = useRef<SnoozeListener | undefined>(undefined);
  useEffect(
    () => () => {
      listener.current = undefined;
    },
    []
  );
  /** Tells the newest press's listener whether the server took `press`; a press before it, or one
   *  that settles after the host went, tells nobody. */
  const heard = (press: AskSnoozePress, landed: boolean): void => {
    const listening = listener.current;
    if (listening === undefined || listening.press !== press) return;
    listener.current = undefined;
    listening.onSettled(landed);
  };
  const listen = (press: AskSnoozePress | undefined, onSettled: SnoozeSettled | undefined) => {
    listener.current =
      press === undefined || onSettled === undefined ? undefined : { onSettled, press };
  };
  // The row says why its snooze was refused, where the reader pressed it. A read of the Inbox
  // asked while the snooze is out waits until Dispatch has taken it (its answer or the viewer's
  // snooze event): Dispatch can answer it before, with the row's moment from before the press.
  const mutation = useKeyedWrite({
    holds: () => ({ endsOn: snoozeWritten, keys: [inboxKey] }),
    // A snooze is a `PUT`, and bringing the row back a `DELETE`.
    method: ({ until }: AskSnoozePress) => (until === null ? "DELETE" : "PUT"),
    write: (press: AskSnoozePress, bounds) =>
      press.until === null
        ? api.unsnoozeAsk(press.askId, bounds)
        : api.snoozeAsk(press.askId, press.until, bounds).then(() => undefined),
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
  return {
    // A save the reader stopped may have been made, and Retry makes it again (`retry`): the
    // error's own words are a composer's, of a message.
    error:
      stoppedNote(
        mutation,
        mutation.variables?.until === null ? "the un-snooze" : "the snooze",
        "saved"
      ) ?? mutation.error?.message,
    failed: mutation.isError,
    failedPress: mutation.isError ? mutation.variables : undefined,
    liveAskId: livePress(mutation)?.askId,
    pending: mutation.isPending,
    pendingUntil: mutation.isPending ? mutation.variables?.until : undefined,
    retry: (onSettled) => {
      listen(mutation.variables, onSettled);
      mutation.retry();
    },
    submit: (until, onSettled) => {
      const press = { askId, until };
      listen(press, onSettled);
      mutation.mutate(press);
    },
  };
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
  /** Snoozes every id until `until` (RFC3339); resolves with the asks the server refused. */
  submit: (
    ids: readonly string[],
    until: string
  ) => Promise<{ failed: readonly AskSnoozeFailure[] }>;
}

/**
 * The bulk form of `useAskSnooze`, over the same route once per ask: the marked rows drop into
 * `Later` on the pick through the same optimistic write on every inbox cache, and the list
 * refetches once the whole set has settled. Each ask is a write of its own, under a key of its own
 * and tried until Dispatch decides it, so a transport failure is tried again rather than counted a
 * refusal, and each settles on its own: a refusal rolls back that row alone - to the moment it had
 * before, not to null, which would un-snooze a row that was already deferred - and the ones the
 * server took stay where they landed. A read of the Inbox asked while the set is out waits until
 * the last of them is decided, or until as many snooze events as asks have come, as the single
 * snooze's does; an ask Dispatch refused with no earlier attempt of its snooze that may have
 * landed unseen publishes no event, and the hold waits for none of it.
 */
export function useAskSnoozeMany(): AskSnoozeManyWrite {
  const queryClient = useQueryClient();
  const writes = useWrites();
  const inboxKey = inboxQuery().queryKey;
  const [pending, setPending] = useState(0);
  return {
    pending: pending > 0,
    submit: async (ids, until) => {
      // Built before anything is written down: the builder refuses an id the route's pattern has
      // no value for, and a refusal here, inside `ids.map` below, would escape `submit` after the
      // optimistic write with `pending` raised and no row rolled back. No caller sends an empty
      // id, so this is where that stays true rather than a case to handle.
      const writeOf = ids.map((id) => ({ id, request: askSnoozeRequest(id, until) }));
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
          writeOf.map(({ id, request }) =>
            writes.run({
              detail: null,
              inline: true,
              kind: plainWrite,
              label: "Ask snoozed",
              presentation: "indicator",
              request,
              target: `ask-snooze:${id}`,
            })
          )
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
        // Whatever happens above: a hold left on would keep every read of the Inbox waiting.
        release(refusals);
      }
      void queryClient.invalidateQueries({ queryKey: inboxKey });
      setPending((count) => count - 1);
      return { failed };
    },
  };
}
