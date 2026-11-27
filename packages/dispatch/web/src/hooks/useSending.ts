import {
  type MutationFilters,
  type MutationKey,
  type QueryClient,
  useIsMutating,
  useQueryClient,
} from "@tanstack/react-query";
import { useCallback, useMemo } from "react";

/** The last segment of a key a send moves to once it is past its deadline (`pastDeadlineKey`). */
const PAST_DEADLINE = "past-deadline";

/** Where a send's request waits for its answer once it is past its deadline: beneath the send's
 *  own name, so a hold on that name, or on any name above it, still counts it until the server
 *  answers, and a hold `untilDeadline` lets go of it. */
export function pastDeadlineKey(mutationKey: MutationKey): MutationKey {
  return [...mutationKey, PAST_DEADLINE];
}

/** Holds `mutationKey` in TanStack's mutation cache, with `variables`, while `answer` is out, so a
 *  hold on the name (`useSending`, `useIsMutating`) counts it from the task that calls this on,
 *  whether or not what started it is still drawn: a composer's send (`held-sends.ts`), an issues
 *  table's action and each write it waits on (`issues-table-actions.ts`). Runs whether or not the
 *  browser says it is online (`networkMode: "always"`): a mutation TanStack paused offline would
 *  hold nothing. Answers once the cache counts it no longer, and never rejects. The cache then
 *  drops it at once rather than at TanStack's cleanup five minutes on, since every reader of these
 *  names counts the pending ones alone, and each settled one kept is scanned by every
 *  `useMutationState` and `useIsMutating` until then. */
export function holdName(
  client: QueryClient,
  mutationKey: MutationKey,
  answer: () => Promise<unknown>,
  variables?: unknown
): Promise<unknown> {
  const cache = client.getMutationCache();
  const mutation = cache.build(client, { mutationFn: answer, mutationKey, networkMode: "always" });
  return mutation
    .execute(variables)
    .catch(() => undefined)
    .finally(() => cache.remove(mutation));
}

/** The sends named `mutationKey`, or a key beneath it (TanStack matches a key's prefix), as the
 *  host of the composers that make them reads them. */
export interface Sending {
  /** Whether one is out, as of the last render: what the host's own controls show. */
  readonly sending: boolean;
  /** Whether one is out now, for an event handler. The mutation cache counts a send pending from
   *  the task that starts it - `mutate` marks it before its first await - so a control pressed in
   *  Send's own task, before React has rendered the send, still reads it; and a send whose
   *  composer has unmounted still counts until the server answers. */
  readonly sendingNow: () => boolean;
}

/**
 * A host's hold on the sends named `mutationKey`. By default it lasts until the server answers.
 * `untilDeadline` ends it at the send's client deadline (`SEND_DEADLINE_MS`) instead, for a control
 * that takes the reader away from the composer - Back, Escape, Collapse thread - so a request the
 * server never answers cannot keep them there; the request still goes on, and its answer is still
 * the send's outcome.
 */
export function useSending(
  mutationKey: MutationKey,
  { untilDeadline = false }: { readonly untilDeadline?: boolean } = {}
): Sending {
  const queryClient = useQueryClient();
  const filters = useMemo<MutationFilters>(
    () =>
      untilDeadline
        ? {
            mutationKey,
            predicate: (mutation) => mutation.options.mutationKey?.at(-1) !== PAST_DEADLINE,
          }
        : { mutationKey },
    [mutationKey, untilDeadline]
  );
  const sending = useIsMutating(filters) > 0;
  const sendingNow = useCallback(() => queryClient.isMutating(filters) > 0, [filters, queryClient]);
  return { sending, sendingNow };
}
