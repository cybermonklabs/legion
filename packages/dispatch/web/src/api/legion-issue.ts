import { queryOptions } from "@tanstack/react-query";

import { LIVE, subscribeLive } from "./queries";

/** The shapes the query answers, for the parts that draw an issue's Legion state. */
export type { LegionIssueResponse, LegionIssueState, LegionRoleClaim } from "@legion/contracts";

/** One issue's Legion state, live: the query subscribes the live socket's `legion_issue` view,
 *  whose snapshot is the body of `GET /api/v1/issues/{key}/legion` and whose deltas the socket
 *  writes into the cache, so it never reads that route itself. Its key stands apart from
 *  `["issue", key]`, so a refresh of the issue's reads never subscribes it again, and every part
 *  that draws the state reads this one query and holds one subscription. A project that runs no
 *  Legion daemon answers `configured` false, never a refusal. */
export const legionIssueQuery = (key: string) =>
  queryOptions({
    ...LIVE,
    queryKey: ["legion-issue", key],
    queryFn: (context) => subscribeLive(context, { kind: "legion_issue", issue: key }),
  });
