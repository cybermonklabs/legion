import type { SearchResponse } from "./dispatch-api.generated";
import type { SEARCH_RESULT_KINDS } from "./dispatch-api-constants";

/**
 * Search and the related-item check. Every body `GET /api/v1/search` and the related-check
 * routes answer is generated from the Go route table; what stays here is the type of the closed
 * list the table widens to a plain string and the paging fields a caller reads a page by. The
 * list itself and the rule that picks a search's path (`everyBuildReadsSearch`), which the table
 * cannot carry because it is about what a Dispatch released before `GET /api/v1/search/results`
 * does with a request, are in `dispatch-api-constants.ts`.
 */

export type SearchResultKind = (typeof SEARCH_RESULT_KINDS)[number];

/** The four paging fields a Dispatch that supports search paging always answers together:
 *  `total` is every match of every kind, and `reachable` is how many of `total` the pages can
 *  return — each kind lists only its best `SEARCH_KIND_DEPTH` (`dispatch-tools.ts`), so an
 *  offset at or past it returns no results. */
export type SearchResultsPage = Readonly<
  Pick<SearchResponse, "total" | "reachable" | "limit" | "offset">
>;

/** `SearchResultsPage` with every field retyped to `V`, always optional: the shape of "none of
 * these fields" for a sentinel `V` (`undefined` for the wire type below, `never` for
 * `fake-search-response.ts`'s stricter test-only override, which forbids supplying a concrete
 * value without forbidding an explicit `undefined` — TypeScript widens every optional property to
 * admit one regardless of its declared type unless the whole project turns on
 * `exactOptionalPropertyTypes`). */
export type SearchResultsPageAbsentAs<V> = { readonly [K in keyof SearchResultsPage]?: V };
