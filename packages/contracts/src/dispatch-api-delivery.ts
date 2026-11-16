/**
 * The delivery timeline's facets. Every body `GET /api/v1/delivery/*` answers is generated from
 * the Go route table, so what stays here is the two closed sets a page selects and colours by:
 * the table carries them as query parameter names, not as a schema, so no generated type spells
 * them.
 */

/** The timeline's facets, as the query parameters that select them and the `facet_counts` keys
 *  that count them. */
export type DeliveryFacet =
  | "repo"
  | "parent_agent"
  | "session"
  | "issue"
  | "priority"
  | "component"
  | "author"
  | "rework"
  | "deployed";

/** The facets the page colours merges by, as the `color_counts` keys that count them. */
export type DeliveryColorFacet = "repo" | "author" | "priority" | "component" | "parent_agent";
