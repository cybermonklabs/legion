/**
 * The tabs of an issue's page whose whole route is the tab itself - no block, no slug, no id -
 * each the segment of its own name after the issue's key, in a reference
 * (`dispatch://CORE-1/plan`) and in the dashboard's path (`/issues/CORE-1/plan`) alike. Either
 * reads as the issue, as the dashboard draws it and the server's graph stores it (`text.issueTabs`,
 * which adds the conversation, written `conversation` in both and `log` in a reference too).
 * `DISPATCH_TEXT_REFERENCES` holds a row per tab, so a tab added here that the server does not
 * read fails its table.
 *
 * A module of its own that imports nothing: the dashboard's page addresses read it in the entry
 * chunk every page parses first (`packages/dispatch/web/src/features/refs/page-routes.ts`), and
 * the rest of the text rules (`dispatch-text-rules.ts`) stay out of that chunk.
 */
export const PLAIN_ISSUE_TABS = [
  "verification",
  "plan",
  "steps",
  "activity",
  "children",
  "artifacts",
  "collaborators",
] as const;
