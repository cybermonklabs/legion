import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { type UseQueryResult, useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { api } from "../../api/client";
import type { SearchResponse } from "../../api/types";
import type { SearchRequest } from "./search-filters";

/** How long the bar waits after a keystroke before it searches the words alone. Short enough
 *  that the list answers the letter just typed, long enough that a fast typist sends one search
 *  for a word rather than one for each of its letters. */
const KEYSTROKE_DEBOUNCE_MS = 150;
/** How long the bar waits after the last keystroke before it searches by meaning as well as by
 *  words. It has to be clearly longer than the keystroke's wait, or every burst of typing would
 *  pay for an embedding; and short enough that the re-ranked list still reads as part of typing
 *  rather than as something that happened later. Half a second is both: more than three times
 *  the keystroke's wait, and longer than the gap between the letters of a word typed at any
 *  speed a person types at, so a reader who is still typing never pays for it. */
export const MEANING_PAUSE_MS = 500;

/** The one search the list reads, and where the words in the box stand against it. */
export interface TieredSearch {
  /** The words the answer in hand was asked for: the keystroke tier's query, which the pause's
   *  tier only ever replaces for the same words. The list's hits answer these words. */
  readonly asked: string;
  /** The one answer the list, what it says about itself and the highlight all read. */
  readonly search: UseQueryResult<SearchResponse>;
  /** Whether the box has moved on from the words the answer in hand was asked for, so the hits
   *  the list would show are of a query the reader has already typed past. */
  readonly waiting: boolean;
}

/**
 * The bar's search in two tiers, which answer as one. The keystroke's tier asks the route for
 * the keyword leg alone (`keywords_only`), which embeds nothing: an embedding behind each
 * keystroke is a second of waiting on a deployment whose embedder is live, and the words answer
 * in tens of milliseconds. The pause's tier is made once the reader stops typing, and asks for
 * the whole search - meaning as well as words - whose fused order then replaces the list the
 * keystrokes painted. Every keystroke starts both waits again, so a reader still typing pays
 * for no embedding.
 *
 * Each tier is its own answer under its own key. Keeping them apart is what lets the keyword
 * list stay painted while the meaning search is still out, and what keeps an answer the reader
 * has moved on from off the list: it lands under the key it was asked for, which the list no
 * longer reads.
 */
export function useTieredSearch({
  enabled,
  request,
  words,
}: {
  /** Whether the words in the box are a query at all: too short or too long, and the bar says
   *  so in place of searching. */
  readonly enabled: boolean;
  /** What narrows every leg of the search, the page's project included. The chips belong to the
   *  request and to the key the answer is kept under: a chip added or dropped is another
   *  search, not another reading of this one. */
  readonly request: SearchRequest;
  /** The words in the box, trimmed, as the search is asked for them. */
  readonly words: string;
}): TieredSearch {
  // The two queries the bar searches, one per tier: `debouncedQuery` is the keystroke's and
  // `restedQuery` the pause's.
  const [debouncedQuery, setDebouncedQuery] = useState("");
  const [restedQuery, setRestedQuery] = useState("");

  useEffect(() => {
    // The words in the box have just changed, so the meaning search the keystroke before it was
    // heading for is withdrawn: the timer below is cleared when it has not fired, and where it
    // had, dropping its query here leaves that search with no reader, which aborts the request
    // through the signal React Query hands the search below.
    setRestedQuery("");
    const keystroke = window.setTimeout(() => setDebouncedQuery(words), KEYSTROKE_DEBOUNCE_MS);
    const pause = window.setTimeout(() => setRestedQuery(words), MEANING_PAUSE_MS);
    return () => {
      window.clearTimeout(keystroke);
      window.clearTimeout(pause);
    };
  }, [words]);

  const keywordRequest: SearchRequest = { ...request, keywords_only: true };
  const keywords = useQuery({
    enabled: debouncedQuery.length >= 2 && debouncedQuery.length <= SEARCH_QUERY_MAX,
    queryFn: ({ signal }) => api.search(debouncedQuery, keywordRequest, { signal }),
    queryKey: ["search", debouncedQuery, keywordRequest],
  });
  const meaning = useQuery({
    enabled: restedQuery.length >= 2 && restedQuery.length <= SEARCH_QUERY_MAX,
    queryFn: ({ signal }) => api.search(restedQuery, request, { signal }),
    queryKey: ["search", restedQuery, request],
  });
  return {
    asked: debouncedQuery,
    // The fused order once the pause's search of the query in hand has answered, and the
    // keyword order until then.
    search: restedQuery === debouncedQuery && meaning.data !== undefined ? meaning : keywords,
    waiting: enabled && debouncedQuery !== words,
  };
}
