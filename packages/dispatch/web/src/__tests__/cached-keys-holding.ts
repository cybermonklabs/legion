import type { QueryClient, QueryKey } from "@tanstack/react-query";

/** The keys of the reads in `client`'s cache whose answer holds `words` anywhere in it: a
 *  pending insert's delete leaves no read that still holds the insert's words. */
export function cachedKeysHolding(client: QueryClient, words: string): QueryKey[] {
  return client
    .getQueryCache()
    .getAll()
    .filter((query) => JSON.stringify(query.state.data ?? null).includes(words))
    .map((query) => query.queryKey);
}
