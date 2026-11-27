import type { QueryClient, QueryKey } from "@tanstack/react-query";

/** Drops every settled write under `mutationKey` from the mutation cache, leaving the pending
 *  ones. A surface that keeps its writes past unmount (`gcTime: Infinity`, so a control mounted
 *  later can still show its newest refusal) calls it from `onSettled`: a write settles in press
 *  order and is still pending while `onSettled` runs, so every settled write left under the key
 *  is older than it and no longer the one the surface reports, and nothing else clears them. */
export function dropSettledWrites(queryClient: QueryClient, mutationKey: QueryKey): void {
  const cache = queryClient.getMutationCache();
  for (const older of cache.findAll({ exact: true, mutationKey })) {
    if (older.state.status !== "pending") {
      cache.remove(older);
    }
  }
}
