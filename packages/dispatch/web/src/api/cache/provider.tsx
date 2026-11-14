import { QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement, ReactNode } from "react";

import { type Cache, cacheOf } from "./cache";
import { clientOf } from "./client-handle";
import { DispatchQueryClient, queryRetry, queryRetryDelay } from "./query-client";

/** The page's one cache, as the app root makes it (`main.tsx`). Never two requests in flight for
 *  one read: an invalidation during a fetch fetches again once it settles (`query-client.ts`). */
export function createAppCache(): Cache {
  return cacheOf(
    new DispatchQueryClient({
      defaultOptions: {
        queries: {
          // Two quick retries for a transient failure, none for a 401, and the sign-in check's
          // EVENT_HEAD_UNAVAILABLE for as long as it lasts (`queryRetry`), each after the wait a
          // server refusing excess load asks for (`Retry-After`, `queryRetryDelay`), or 500 ms.
          retry: queryRetry,
          retryDelay: queryRetryDelay,
          // The live socket's event feed is the source of freshness; a stale query still gets
          // invalidated the moment a relevant event arrives. This just avoids an unconditional
          // refetch-on-mount for data the feed keeps current.
          refetchOnWindowFocus: true,
          staleTime: 30_000,
        },
      },
    })
  );
}

/** Gives everything under it `cache` (`useCache`, and every read hook of `shown-query.ts`). */
export function CacheProvider({
  cache,
  children,
}: {
  readonly cache: Cache;
  readonly children: ReactNode;
}): ReactElement {
  return <QueryClientProvider client={clientOf(cache)}>{children}</QueryClientProvider>;
}
