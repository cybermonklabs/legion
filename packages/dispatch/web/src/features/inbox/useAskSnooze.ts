import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { api, askSnoozeRequest } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import type { InboxRow } from "../../api/types";
import { SendStoppedError, stoppedWriteNote } from "../writes/keyed-write";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { plainWrite, useWrites } from "../writes/write-store";

export interface AskSnoozeWrite {
  /** The server's reason after a refused save, or what one the reader stopped may have done and
   *  what Retry does with it, until the next attempt. */
  error: string | undefined;
  /** True after a save failed until the next attempt starts. */
  failed: boolean;
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

/**
 * The one way the SPA writes a row's snooze. The new moment lands on every inbox cache at once,
 * so the row drops into `Later` (or climbs back out) on the click, rolls back if the server
 * refuses, and the list refetches once the save settles - the same shape as the issue field
 * writes, without their cross-surface caches: a snooze is on the ask and shows nowhere else.
 */
export function useAskSnooze(askId: string): AskSnoozeWrite {
  const queryClient = useQueryClient();
  const inboxKey = inboxQuery().queryKey;
  // The row says why its snooze was refused, where the reader pressed it.
  const mutation = useKeyedWrite({
    // A snooze is a `PUT`, and bringing the row back a `DELETE`.
    method: (until: string | null) => (until === null ? "DELETE" : "PUT"),
    write: (until: string | null, bounds) =>
      until === null
        ? api.unsnoozeAsk(askId, bounds)
        : api.snoozeAsk(askId, until, bounds).then(() => undefined),
    inline: true,
    label: (until) => (until === null ? "Ask brought back now" : "Ask snoozed"),
    onMutate: async (until) => {
      await queryClient.cancelQueries({ queryKey: inboxKey });
      const previous = queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey });
      queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
        current?.map((row) => (row.id === askId ? { ...row, snoozed_until: until } : row))
      );
      return previous;
    },
    onError: (_error, _until, previous) => {
      for (const [key, data] of previous ?? []) {
        queryClient.setQueryData(key, data);
      }
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: inboxKey });
    },
  });
  return {
    // A save the reader stopped may have been made, and Retry makes it again (`retry`): the
    // error's own words are a composer's, of a message. A snooze is a `PUT` and bringing the row
    // back a `DELETE`.
    error:
      mutation.error instanceof SendStoppedError
        ? stoppedWriteNote(mutation.variables === null ? "the un-snooze" : "the snooze", "saved", {
            again: "Retry",
            method: mutation.variables === null ? "DELETE" : "PUT",
            stopped: mutation.error,
          })
        : mutation.error?.message,
    failed: mutation.isError,
    pending: mutation.isPending,
    pendingUntil: mutation.isPending ? mutation.variables : undefined,
    retry: mutation.retry,
    submit: (until) => mutation.mutate(until),
  };
}

/** One ask the server refused, with the reason it gave - the same message the per-row control
 *  shows, so a sign-out never reads like a validation refusal - or one whose snooze the reader
 *  stopped (`stopped`), which may have been saved and is no refusal: its `reason` is then the
 *  error's own words, a composer's, which nothing shows. */
export interface AskSnoozeFailure {
  askId: string;
  reason: string;
  stopped: boolean;
}

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
 * server took stay where they landed.
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
      const failed = ids.flatMap((askId, index) => {
        const result = results[index];
        if (result === undefined || result.status !== "rejected") return [];
        const reason: unknown = result.reason;
        return [
          {
            askId,
            reason: reason instanceof Error ? reason.message : String(reason),
            stopped: reason instanceof SendStoppedError,
          },
        ];
      });
      const refused = new Set(failed.map(({ askId }) => askId));
      for (const [key, data] of refused.size === 0 ? [] : previous) {
        const before = new Map((data ?? []).map((row) => [row.id, row.snoozed_until]));
        queryClient.setQueryData<InboxRow[]>(key, (current) =>
          current?.map((row) =>
            refused.has(row.id) ? { ...row, snoozed_until: before.get(row.id) ?? null } : row
          )
        );
      }
      void queryClient.invalidateQueries({ queryKey: inboxKey });
      setPending((count) => count - 1);
      return { failed };
    },
  };
}
