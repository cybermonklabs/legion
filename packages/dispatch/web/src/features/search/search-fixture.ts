import type { SearchResponse, SearchResult, SearchResultsPage } from "../../api/types";

/**
 * The server's answer when `hits` are every match: one page holding the whole list, read as a
 * plain query - no chip narrowed it and no operator form was written in it - which is what every
 * spec that builds an answer here is about. A row that is about a filter or about an operator
 * spreads this and names its own `filters` or `query`, so the default each of the others asserts
 * nothing about is written once.
 */
export function searchAnswer(hits: SearchResult[]): SearchResponse & SearchResultsPage {
  return {
    filters: {},
    limit: 20,
    offset: 0,
    query: { broadened: false, operators: [] },
    reachable: hits.length,
    results: hits,
    took_ms: 1,
    total: hits.length,
  };
}
