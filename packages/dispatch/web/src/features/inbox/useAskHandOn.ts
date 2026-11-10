import { useQueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import { inboxQuery, whoAmIQuery } from "../../api/queries";
import type { InboxRow } from "../../api/types";
import { useKeyedWrite } from "../writes/useKeyedWrite";

export interface AskHandOnWrite {
  /** The server's reason after a refused hand-on, until the next attempt. */
  error: string | undefined;
  /** True after a hand-on failed until the next attempt starts. */
  failed: boolean;
  pending: boolean;
  /** Repeats the failed hand-on. */
  retry: () => void;
  /** Hands the ask to `askedOf`, the email of somebody who has signed in. */
  submit: (askedOf: string) => void;
}

/**
 * The one way the SPA hands an ask to a person. The new person lands on every inbox cache at
 * once, so the row leaves the list it was in and joins theirs on the press, rolls back if the
 * server refuses, and the list refetches once the hand-on settles - the same shape as
 * `useAskSnooze`, over `PUT /api/v1/asks/{id}/asked-of`.
 *
 * What is patched is what the server will answer with: `waiting_on_person`, which is the person
 * named either way, and the three stored fields behind it - `asked_of`, empty when the ask was
 * handed to the assignee of its own issue, since an empty field means the ask follows that
 * issue again; `asked_of_by`, the reader; and `asked_of_at`, now. The reader is taken from the
 * `/auth/whoami` read the shell already holds rather than read again: it is in the cache before
 * any card can be pressed, and a hand-on made without it leaves the pair unnamed until the
 * refetch answers.
 */
export function useAskHandOn(askId: string): AskHandOnWrite {
  const queryClient = useQueryClient();
  const inboxKey = inboxQuery().queryKey;
  // The card says why its hand-on was refused, where the reader pressed it.
  const mutation = useKeyedWrite({
    write: (askedOf: string, bounds) => api.handAskTo(askId, askedOf, bounds),
    inline: true,
    label: (askedOf) => `Ask handed to ${askedOf}`,
    onMutate: async (askedOf) => {
      await queryClient.cancelQueries({ queryKey: inboxKey });
      const previous = queryClient.getQueriesData<InboxRow[]>({ queryKey: inboxKey });
      const by = queryClient.getQueryData(whoAmIQuery().queryKey)?.login ?? null;
      const at = new Date().toISOString();
      queryClient.setQueriesData<InboxRow[]>({ queryKey: inboxKey }, (current) =>
        current?.map((row) =>
          row.id === askId
            ? {
                ...row,
                // Naming the assignee of the ask's own issue stores no person: the ask
                // follows that issue again, and whom it waits on is the person named either
                // way.
                asked_of: askedOf === (row.issue?.assignee ?? null) ? null : askedOf,
                asked_of_by: by,
                asked_of_at: at,
                waiting_on_person: askedOf,
              }
            : row
        )
      );
      return previous;
    },
    onError: (_error, _askedOf, previous) => {
      for (const [key, data] of previous ?? []) {
        queryClient.setQueryData(key, data);
      }
    },
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: inboxKey });
    },
  });
  return {
    error: mutation.error?.message,
    failed: mutation.isError,
    pending: mutation.isPending,
    retry: () => {
      if (mutation.variables !== undefined) mutation.mutate(mutation.variables);
    },
    submit: (askedOf) => mutation.mutate(askedOf),
  };
}
