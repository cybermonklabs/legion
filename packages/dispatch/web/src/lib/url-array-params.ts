import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router-dom";

/** One `update` callback that merges a mutation into the live search params and writes it with
 *  `replace: true`, so repeated filter changes never pile up history entries. Shared by every
 *  URL-backed filter state (`useIssueFilters`, the delivery page's `useDeliveryUrlState`) and by
 *  the Priorities page's `?tab=`, whose tab click is a place the reader went and so asks for an
 *  entry of its own (`replace: false`), as a tab of the project page or the issue page adds one. */
export function useSearchParamsUpdate(): (
  mutate: (params: URLSearchParams) => void,
  options?: { readonly replace?: boolean }
) => void {
  const [, setSearchParams] = useSearchParams();
  return useCallback(
    (mutate: (params: URLSearchParams) => void, { replace = true } = {}) => {
      setSearchParams(
        (current) => {
          const next = new URLSearchParams(current.toString());
          mutate(next);
          return next;
        },
        { replace }
      );
    },
    [setSearchParams]
  );
}

/** Every value of each repeatable `?<name>=` parameter in `keys`, memoized on the live
 *  `URLSearchParams` object's identity so two renders of the same URL return the exact same
 *  arrays — without this, `searchParams.getAll` mints a fresh array every render, and a consumer
 *  that keys a `useMemo` or a query off the result (the Delivery page's live view) never sees
 *  two renders agree, recomputing or refetching on every render instead of only when the URL
 *  changes. `keys` is expected to be a module-level constant, since its own identity is a
 *  dependency of the memo. */
export function useRepeatableSearchParams<K extends string>(
  keys: readonly K[]
): Record<K, string[]> {
  const [searchParams] = useSearchParams();
  return useMemo(() => {
    const result = {} as Record<K, string[]>;
    for (const key of keys) result[key] = searchParams.getAll(key);
    return result;
  }, [searchParams, keys]);
}
