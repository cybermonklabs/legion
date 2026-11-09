import type { SearchResponse, SearchResult } from "../api/types";

/**
 * What `GET /api/v1/search` answers for `hits`: one whole page holding all of them, which is
 * what a fixture that writes out its own hits stands for - `total` and `reachable` are the hits
 * themselves, the page starts at the first, and `limit` is the route's own default (20). Name
 * `took_ms`, `degraded` or a paging field on top for a test about paging, about a degraded
 * search, or about how long the search took.
 */
export function searchResponse(
  hits: readonly SearchResult[],
  fields: Partial<SearchResponse> = {}
): SearchResponse {
  return {
    limit: 20,
    offset: 0,
    reachable: hits.length,
    results: [...hits],
    took_ms: 1,
    total: hits.length,
    ...fields,
  };
}
