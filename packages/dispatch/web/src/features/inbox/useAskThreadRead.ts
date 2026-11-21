import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useLayoutEffect, useRef } from "react";

import { replaceQueryData } from "../../api/query-client";
import type { Ask, AskRead } from "../../api/types";
import type { AskThreadQuery } from "./AskThread";
import {
  clearPendingAskThreadInvalidation,
  hasPendingAskThreadInvalidation,
} from "./ask-thread-freshness";

interface UseAskThreadReadOptions {
  readonly ask: Pick<Ask, "id" | "issue_key">;
  readonly getAskThread: (id: string) => Promise<AskRead>;
  /** Data the Inbox response already hydrated for the first render. */
  readonly initialThread?: AskRead;
  /** Timestamp of the Inbox snapshot that supplied initialThread. */
  readonly initialThreadUpdatedAt?: number;
}

/**
 * The read of an ask's thread that an ask's card (`useAskAnswerForm`) and the Inbox reading
 * pane's approval request (`ApprovalExchange`) both draw from, seeded from the Inbox's own read
 * where the host has one: the replies, the ask as it now stands, and what the card shows beside
 * them. It is shared by every view of the ask's thread - the card, its edit-version history, its
 * collapsed disclosure (`AskThreadDisclosure`), and its inline thread (`AskThread`), which the
 * approval request draws alone.
 */
export function useAskThreadRead({
  ask,
  getAskThread,
  initialThread,
  initialThreadUpdatedAt,
}: UseAskThreadReadOptions): AskThreadQuery {
  const queryClient = useQueryClient();
  // A thread invalidated while its Inbox snapshot was in flight bypasses that snapshot, and so
  // does every thread on an issue whose claim or route moved while it was in flight.
  const refreshInitialThread =
    initialThread !== undefined && hasPendingAskThreadInvalidation(queryClient, ask);
  // A view that reads this thread with no seed (the reading column's thread, `ThreadPane`) may
  // already have a read of it out, asked at a time this host cannot know: perhaps before the
  // Inbox read the seed comes from, so that it answers an older thread - the ask still open, say,
  // after the seed showed it answered. TanStack puts the seed into that read's query, which holds
  // no data yet, and leaves the read running, and its answer then replaces the seed with no
  // compare of their ages. So the host notes on its first render, before `useQuery` puts the seed
  // in, whether it seeds over such a read, and if it does, puts the seed over that read as every
  // write goes over the reads still out (`replaceQueryData`).
  const threadBeforeSeed = queryClient.getQueryState(["ask-thread", ask.id]);
  const seedsOverRead = useRef(
    initialThread !== undefined &&
      !refreshInitialThread &&
      threadBeforeSeed !== undefined &&
      threadBeforeSeed.data === undefined &&
      threadBeforeSeed.fetchStatus !== "idle"
  );
  const threadQuery = useQuery<AskRead, Error>({
    initialData: refreshInitialThread ? undefined : initialThread,
    initialDataUpdatedAt: refreshInitialThread ? undefined : initialThreadUpdatedAt,
    queryKey: ["ask-thread", ask.id],
    queryFn: () => getAskThread(ask.id),
    staleTime: Number.POSITIVE_INFINITY,
  });
  // In a layout effect, so that no answer from the network lands between the commit that drew the
  // seed and this write. Cancelling the read puts its query back as it stood before that read,
  // with no data, so the seed is written after the cancel; and with a read cancelled,
  // `replaceQueryData` asks the thread again, so the next answer is of a read asked after the seed.
  useLayoutEffect(() => {
    if (!seedsOverRead.current) return;
    seedsOverRead.current = false;
    replaceQueryData<AskRead>(
      queryClient,
      ["ask-thread", ask.id],
      (current) => current ?? initialThread
    );
  }, [ask.id, initialThread, queryClient]);
  useEffect(() => {
    if (refreshInitialThread && threadQuery.data !== undefined) {
      clearPendingAskThreadInvalidation(queryClient, ask.id);
    }
  }, [ask.id, queryClient, refreshInitialThread, threadQuery.data]);
  return threadQuery;
}
