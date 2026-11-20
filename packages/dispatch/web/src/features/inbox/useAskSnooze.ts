import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { api, askSnoozeRequest } from "../../api/client";
import { inboxQuery, meInboxQuery } from "../../api/queries";
import { holdBackReads } from "../../api/query-client";
import type { InboxRow } from "../../api/types";
import { snoozeWritten } from "../../api/write-events";
import { refusedByDispatch, SendStoppedError } from "../writes/keyed-write";
import { livePress, stoppedNote, useKeyedWrite } from "../writes/useKeyedWrite";
import { plainWrite, useWrites } from "../writes/write-store";
import {
  cancelMeInbox,
  invalidateMeInbox,
  meInboxSnapshot,
  patchMeInboxAsks,
  restoreMeInbox,
} from "./one-read-cache";

export interface AskSnoozeWrite {
  /** The server's reason after a refused save, or what one the reader stopped may have done and
   *  what Retry does with it, until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
  /** The ask the newest save is of while that save is in flight or failed, which the list around
   *  the row holds (`useLiveRegistration`); `undefined` while none is. */
  liveAskId: string | undefined;
  pending: boolean;
  /** What the in-flight write is doing, and `undefined` when none is: the moment a snooze is
   *  saving, or `null` for an un-snooze. The optimistic update has already moved the row by
   *  the time it renders, so this - not the row's own `snoozed_until` - is what the reader
   *  just asked for. */
  pendingUntil: string | null | undefined;
  /** Repeats the failed save. */
  retry: () => void;
  /** Snoozes the row until `until` (RFC3339), or brings it back with null. */
  submit: (until: string | null) => void;
}

/** One press of a row's snooze: the ask it is of, and the moment, or `null` to bring it back. */
interface AskSnoozePress {
  readonly askId: string;
  readonly until: string | null;
}

/**
 * The one way the SPA writes a row's snooze. The new moment lands on every inbox cache at once -
 * the Inbox read and, behind its opt-in, the one read (`one-read-cache.ts`) - so the row drops
 * into `Later` (or climbs back out) on the click, rolls back if the server refuses, and the list
 * refetches once the save settles - the same shape as the issue field writes, without their
 * cross-surface caches: a snooze is on the ask and shows nowhere else.
 *
 * Each press carries the ask it is of: `retry()` runs the newest render's `write` with the
 * press's own values (`useKeyedWrite`), so every byte a Retry sends rides in the press. A host
 * may draw the row's control for one ask after another, so the Retry of a press the reader
 * stopped is of that press's ask, and so is the ask the save holds in the list while it is live
 * (`liveAskId`), whichever ask the hook is rendered for by then.
 */
export function useAskSnooze(askId: string): AskSnoozeWrite {
  const queryClient = useQueryClient();
  const inboxKey = inboxQuery().queryKey;
  // The row says why its snooze was refused, where the reader pressed it. A read of the Inbox,
  // or behind its opt-in of the one read, asked while the snooze is out waits until Dispatch has
  // taken it (its answer or the viewer's snooze event): Dispatch can answer it before, with the
  // row's moment from before the press.
  const mutation = useKeyedWrite({
    holds: () => ({ endsOn: snoozeWritten, keys: [inboxKey, meInboxQuery().queryKey] }),
    // A snooze is a `PUT`, and bringing the row back a `DELETE`.
    method: ({ until }: AskSnoozePress) => (until === null ? "DELETE" : "PUT"),
    write: (press: AskSnoozePress, bounds) =>
      press.until === null
        ? api.unsnoozeAsk(press.askId, bounds)
        : api.snoozeAsk(press.askId, press.until, bounds).then(() => undefined),
    inline: true,
    label: ({ until }) => (until === null ? "Ask brought back now" : "Ask snoozed"),
    onMutate: async (press) => {
      await Promise.all([
        queryClient.cancelQueries({ queryKey: inboxKey }),
        cancelMeInbox(queryClient),
      ]);
      const previous = queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey });
      const oneRead = meInboxSnapshot(queryClient);
      const snoozed = (row: InboxRow): InboxRow =>
        row.id === press.askId ? { ...row, snoozed_until: press.until } : row;
      queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
        current?.map(snoozed)
      );
      patchMeInboxAsks(queryClient, snoozed);
      return { inbox: previous, oneRead };
    },
    onError: (_error, _press, previous) => {
      for (const [key, data] of previous?.inbox ?? []) {
        queryClient.setQueryData(key, data);
      }
      restoreMeInbox(queryClient, previous?.oneRead);
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: inboxKey });
      invalidateMeInbox(queryClient);
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
    liveAskId: livePress(mutation)?.askId,
    pending: mutation.isPending,
    pendingUntil: mutation.isPending ? mutation.variables?.until : undefined,
    retry: mutation.retry,
    submit: (until) => mutation.mutate({ askId, until }),
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
 * server took stay where they landed. A read of the Inbox, or behind its opt-in of the one read,
 * asked while the set is out waits until the last of them is decided, or until as many snooze
 * events as asks have come, as the single snooze's does; an ask Dispatch refused with no earlier
 * attempt of its snooze that may have landed unseen publishes no event, and the hold waits for
 * none of it.
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
        keys: [inboxKey, meInboxQuery().queryKey],
      });
      let failed: AskSnoozeFailure[];
      // How many of the asks Dispatch refused with no earlier attempt of their snooze that may
      // have landed unseen (`refusedByDispatch`), which publish no event.
      let refusals = 0;
      try {
        await Promise.all([
          queryClient.cancelQueries({ queryKey: inboxKey }),
          cancelMeInbox(queryClient),
        ]);
        const previous = queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey });
        // What each marked row of the one read held before, as each Inbox read's rows are kept.
        const oneReadBefore = new Map<string, string | null | undefined>();
        const marked = new Set(ids);
        queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
          current?.map((row) => (marked.has(row.id) ? { ...row, snoozed_until: until } : row))
        );
        patchMeInboxAsks(queryClient, (row) => {
          if (!marked.has(row.id)) return row;
          oneReadBefore.set(row.id, row.snoozed_until);
          return { ...row, snoozed_until: until };
        });
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
        if (refused.size > 0) {
          patchMeInboxAsks(queryClient, (row) =>
            refused.has(row.id) ? { ...row, snoozed_until: oneReadBefore.get(row.id) ?? null } : row
          );
        }
      } finally {
        // Whatever happens above: a hold left on would keep every read of the Inbox waiting.
        release(refusals);
      }
      void queryClient.invalidateQueries({ queryKey: inboxKey });
      invalidateMeInbox(queryClient);
      setPending((count) => count - 1);
      return { failed };
    },
  };
}
