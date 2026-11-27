import { setSystemTime } from "bun:test";
import { focusManager, type QueryClient } from "@tanstack/react-query";
import { act } from "@testing-library/react";

import { DispatchQueryClient } from "../../api/query-client";

/**
 * For unit tests only: the person comes back to the tab. Resolves once the client has handled the
 * focus: its focus listener awaits `resumePausedMutations` before it refetches, so awaiting the
 * same call after it finds any refetch the focus starts already at its read.
 */
export async function refocus(queryClient: QueryClient): Promise<void> {
  focusManager.setFocused(false);
  focusManager.setFocused(true);
  await queryClient.resumePausedMutations();
}

/** How long a read of `appClient` stays fresh after it answered: the `staleTime` `main.tsx`
 *  gives every read. */
const STALE_AFTER_MS = 30_000;

/** For unit tests only: the app's client with the defaults `main.tsx` gives every read: stale
 *  `STALE_AFTER_MS` after it answered, and fetched again on window focus once stale. No retry, so
 *  a stub's answer is the read's. */
export function appClient(): QueryClient {
  return new DispatchQueryClient({
    defaultOptions: {
      queries: { refetchOnWindowFocus: true, retry: false, staleTime: STALE_AFTER_MS },
    },
  });
}

/** For unit tests only: the reader comes back to the tab a second past the app's staleness
 *  (`appClient`) after the reads answered. The caller undoes it with `restoreWindowFocus`. */
export async function focusPastStaleness(client: QueryClient): Promise<void> {
  setSystemTime(Date.now() + STALE_AFTER_MS + 1_000);
  await act(() => refocus(client));
}

/** For unit tests only: puts the clock back and leaves the window's focus to the browser again,
 *  undoing `focusPastStaleness`. */
export function restoreWindowFocus(): void {
  setSystemTime();
  focusManager.setFocused(undefined);
}
