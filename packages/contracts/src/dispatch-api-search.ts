import type { SearchResponse } from "./dispatch-api.generated";

/**
 * Search and the related-item check. Every body `GET /api/v1/search` and the related-check
 * routes answer is generated from the Go route table; what stays here is the closed list the
 * table widens to a plain string and the paging fields a caller reads a page by.
 */

/** The kinds a search result and a write suggestion are one of. The route table carries `kind`
 *  as a plain string, and this is the closed list it means. */
export const SEARCH_RESULT_KINDS = ["issue", "document", "comment", "ask", "message"] as const;

export type SearchResultKind = (typeof SEARCH_RESULT_KINDS)[number];

/** The members `GET /api/v1/search` carried before `GET /api/v1/search/results` was added.
 *  `session` is among them because `openapi.go` declares it on every operation of every build
 *  and `actingPerson` reads it on every route, so a request carrying it is one an older build
 *  serves correctly and must not be sent to a path that build refuses. */
export const SEARCH_MEMBERS_EVERY_BUILD_READS = [
  "q",
  "project",
  "limit",
  "offset",
  "session",
] as const;

/** Whether every member a search request carries is one of `SEARCH_MEMBERS_EVERY_BUILD_READS`,
 *  which is the one rule that picks a search's path: a request carrying anything beyond them
 *  goes to `GET /api/v1/search/results`. A Dispatch released before that route answers the
 *  new path `404 no route`, which is a refusal, where it answers `GET /api/v1/search` `200` with
 *  the member dropped, which is unnarrowed rows under the filters a person chose. A key whose
 *  value is `undefined` is not carried, which is how both clients build a request out of
 *  optional filters, and the keys are read from the request itself, so a member added later
 *  routes itself. */
export function everyBuildReadsSearch(request: object): boolean {
  const read: readonly string[] = SEARCH_MEMBERS_EVERY_BUILD_READS;
  return Object.entries(request).every(
    ([member, value]) => value === undefined || read.includes(member)
  );
}

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
