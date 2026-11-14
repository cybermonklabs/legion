import { displayTree, snippetTree } from "../../__tests__/display-trees";
import type { SearchResponse, SearchResult, SearchResultsPage } from "../../api/types";

/**
 * The server's answer when `hits` are every match: one page holding the whole list, read as a
 * plain query - no chip narrowed it and no operator form was written in it - which is what every
 * spec that builds an answer here is about. A row that is about a filter or about an operator
 * spreads this and names its own `filters` or `query`, so the default each of the others asserts
 * nothing about is written once.
 *
 * Each hit carries the reading the server made of its snippet and of an ask's answer text, which
 * is what the palette draws (`snippet_tree`, `answer.text_tree`). A hit that already names a tree
 * of its own keeps it: a snippet holding formatting is read from the Go fixture, which the plain
 * reading here would flatten.
 */
export function searchAnswer(hits: SearchResult[]): SearchResponse & SearchResultsPage {
  const read = hits.map((hit) => ({
    ...hit,
    // A hit's ask states its answer as optional, because the route does: an answered ask
    // carries one, and this says so rather than taking it for granted.
    ...(hit.ask?.state === "answered" &&
    hit.ask.answer !== undefined &&
    hit.ask.answer.text !== null
      ? {
          ask: {
            ...hit.ask,
            answer: { ...hit.ask.answer, text_tree: displayTree(hit.ask.answer.text) },
          },
        }
      : {}),
    snippet_tree: hit.snippet_tree ?? snippetTree(hit.snippet),
  }));
  return {
    filters: {},
    limit: 20,
    offset: 0,
    query: { broadened: false, operators: [] },
    reachable: hits.length,
    results: read,
    took_ms: 1,
    total: hits.length,
  };
}
