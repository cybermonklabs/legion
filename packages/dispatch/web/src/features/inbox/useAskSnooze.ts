import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { askSnoozeRequest, askUnsnoozeRequest } from "../../api/client";
import { inboxQuery } from "../../api/queries";
import type { InboxRow } from "../../api/types";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { plainWrite, useWrites } from "../writes/write-store";

export interface AskSnoozeWrite {
  /** The server's reason after a refused save, until the next attempt. */
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
 * so the row drops into `Later` (or climbs back out) on the click, and the list refetches once
 * the save settles - the same shape as the issue field writes, without their cross-surface
 * caches: a snooze is on the ask and shows nowhere else.
 *
 * The moment is shown by the store (`WriteSpec.shows`), so it is shown again over each answer
 * that lands on an inbox cache while the write is live (R7 of dispatch://INFRA-299): an answer
 * composed before the snooze arrives whole - every other row of it with it - and leaves the
 * row where the reader put it. A refused snooze is the store's to put back: the lists as
 * Dispatch last gave them, every other live write's change over them (R10).
 */
export function useAskSnooze(askId: string): AskSnoozeWrite {
  const queryClient = useQueryClient();
  const inboxKey = inboxQuery().queryKey;
  // The row says why its snooze was refused, where the reader pressed it. A snooze and an
  // un-snooze are writes of the ask, so they stand in that row's line (`ask:<id>`) with its
  // answer, its decline, its hand-on and its clarification, rather than on a line of their own.
  const mutation = useKeyedWrite({
    request: (until: string | null) =>
      until === null ? askUnsnoozeRequest(askId) : askSnoozeRequest(askId, until),
    target: () => `ask:${askId}`,
    inline: true,
    label: (until) => (until === null ? "Ask brought back now" : "Ask snoozed"),
    // The moment is a function of each inbox cache as it stands, so showing it again over an
    // answer that lands there gives the same row.
    shows: (until) => (shown) => {
      shown.under<InboxRow[]>(inboxKey, (current) =>
        current.map((row) => (row.id === askId ? { ...row, snoozed_until: until } : row))
      );
    },
    // A read already out was asked before this press.
    onMutate: () => queryClient.cancelQueries({ queryKey: inboxKey }),
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: inboxKey });
    },
  });
  return {
    error: mutation.error?.message,
    failed: mutation.isError,
    pending: mutation.isPending,
    pendingUntil: mutation.isPending ? mutation.variables : undefined,
    retry: () => {
      if (mutation.variables !== undefined) mutation.mutate(mutation.variables);
    },
    submit: (until) => mutation.mutate(until),
  };
}

/** One ask the server refused, with the reason it gave - the same message the per-row control
 *  shows, so a sign-out never reads like a validation refusal. */
export interface AskSnoozeFailure {
  askId: string;
  reason: string;
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
 * `Later` on the pick, each through its own write's change, and the list refetches once the
 * whole set has settled. Each ask is a write of its own, under a key of its own and tried until
 * Dispatch decides it, so a transport failure is tried again rather than counted a refusal, and
 * each settles on its own: the store puts a refused one's row back as Dispatch last gave it -
 * to the moment it had before, not to null, which would un-snooze a row that was already
 * deferred - and the ones the server took stay where they landed.
 *
 * Each write shows its own row's moment through the store (`WriteSpec.shows`), so an answer
 * that lands on an inbox cache while the set is out carries every row of it and still leaves
 * each marked row deferred (R7 of dispatch://INFRA-299).
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
      // no value for, and a refusal here, inside `ids.map` below, would escape `submit` with
      // `pending` raised. No caller sends an empty id, so this is where that stays true rather
      // than a case to handle.
      const writeOf = ids.map((id) => ({ id, request: askSnoozeRequest(id, until) }));
      setPending((count) => count + 1);
      await queryClient.cancelQueries({ queryKey: inboxKey });
      const results = await Promise.allSettled(
        writeOf.map(({ id, request }) =>
          writes.run({
            detail: null,
            inline: true,
            kind: plainWrite,
            label: "Ask snoozed",
            presentation: "indicator",
            request,
            // Each marked row is deferred by the write that defers it, as a function of the
            // cache as it stands, so the same showing over an answer gives the same row.
            shows: (shown) => {
              shown.under<InboxRow[]>(inboxKey, (current) =>
                current.map((row) => (row.id === id ? { ...row, snoozed_until: until } : row))
              );
            },
            target: `ask:${id}`,
          })
        )
      );
      const failed = ids.flatMap((askId, index) => {
        const result = results[index];
        if (result === undefined || result.status !== "rejected") return [];
        const reason: unknown = result.reason;
        return [{ askId, reason: reason instanceof Error ? reason.message : String(reason) }];
      });
      void queryClient.invalidateQueries({ queryKey: inboxKey });
      setPending((count) => count - 1);
      return { failed };
    },
  };
}
