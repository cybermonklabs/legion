/**
 * The constants of Dispatch's tool contract that are not a schema: the issue statuses, the ask
 * urgencies, the block units and the two limits a caller is held to.
 *
 * Why they are a module of their own, beside `dispatch-tools.ts` rather than inside it. That
 * module builds the tool schemas at its top level, and a bundler may drop none of them: a value
 * is used, so the module is kept and the schemas are kept with it, zod and all. The dashboard
 * reads these five values and validates nothing, and it was paying for the schema builder on
 * every page load to have them (dispatch://INFRA-1109, R13). A constant a reader needs must not
 * drag a schema builder into a browser, so the constants live here and `dispatch-tools.ts`
 * re-exports them: an import of that module, or of the package index, answers exactly what it
 * answered before.
 */

/** Ask urgency levels the Dispatch server accepts, in ascending order. */
export const ASK_URGENCIES = ["low", "med", "high", "blocking"] as const;

/**
 * Longest one block of a comment, comment edit, message, broadcast or ask part may be, in UTF-16
 * code units as the Dispatch server counts them. Generated into Go as
 * `contracts.DispatchBlockUnits`, which the server enforces and names in its refusal, and which
 * it publishes through `GET /api/v1/limits`, the route `dispatch limits` prints.
 */
export const DISPATCH_BLOCK_UNITS = 32000;

/**
 * Longest body an event carries whole, in UTF-16 code units, and the bound the Dispatch server
 * holds an ask's question and its options to together. Generated into Go as
 * `contracts.DispatchEventBodyMax`, which the server enforces and names in its refusal, and
 * which it publishes through `GET /api/v1/limits`, the route `dispatch limits` prints.
 */
export const DISPATCH_EVENT_BODY_MAX = 32000;

/**
 * Longest `dispatch_search` query (`GET /api/v1/search`'s `q`, trimmed), in UTF-16 units. The
 * query rides in the URL beside `project`, `limit` and `offset`, so this refusal and `project`'s
 * key rule are what keep the tool's URL under the load balancer's limit;
 * `packages/contracts/AGENTS.md` "Search limits" owns that budget. Generated into Go as
 * `contracts.SearchQueryMax`, which the server enforces.
 */
export const SEARCH_QUERY_MAX = 1000;

/** Issue lifecycle statuses the Dispatch server accepts (`model.IssueStatuses`), in lifecycle order. */
export const ISSUE_STATUSES = [
  "triage",
  "icebox",
  "backlog",
  "todo",
  "in_progress",
  "testing",
  "needs_review",
  "retro",
  "done",
] as const;

export function isIssueStatus(value: string): value is IssueStatus {
  return (ISSUE_STATUSES as readonly string[]).includes(value);
}

export type IssueStatus = (typeof ISSUE_STATUSES)[number];

/** A whole project key, as the Dispatch server creates them, generated into Go as
 *  `contracts.ProjectKeyPattern` (and held by the `projects.key` check constraint). */
export const PROJECT_KEY_PATTERN = /^[A-Z][A-Z0-9]{1,9}$/;
