import type { QueryClient } from "@tanstack/react-query";

import type { Cache } from "./cache";

/**
 * The TanStack client under each `Cache`, for the cache layer's own modules alone: a feature holds
 * the `Cache` and never the client, and lint refuses this module to anything outside
 * `api/cache/` (`biome.json`). Kept apart from `cache.ts` so the store (`write-store.ts`), which
 * `cache.ts` builds each cache's store from, reaches its client without importing `cache.ts`.
 */
const clients = new WeakMap<Cache, QueryClient>();

/** Records `client` as the one under `cache`, as `cacheOf` makes it. */
export function bindClient(cache: Cache, client: QueryClient): void {
  clients.set(cache, client);
}

/** The client under `cache`. Every `Cache` is made by `cacheOf`, which binds one. */
export function clientOf(cache: Cache): QueryClient {
  const client = clients.get(cache);
  if (client === undefined) throw new Error("A Cache was made without cacheOf.");
  return client;
}
