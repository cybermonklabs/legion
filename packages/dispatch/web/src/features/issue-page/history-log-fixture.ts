import type { QueryClient } from "@tanstack/react-query";

import type { EventPages } from "../../api/event-feed";
import type { DispatchEvent } from "../../api/types";
import { historyLogKey } from "./history-log";

/** An event arriving in the history's log, as the read that the feed's refresh starts answers it
 *  (`eventRefreshTargets` invalidates `["events", <key>]`, a prefix of the history's key): page 0
 *  with the event at its head, as `prependEventToLog` writes it into Activity's log. */
export function arriveInHistoryLog(
  queryClient: QueryClient,
  issueKey: string,
  event: DispatchEvent
): void {
  queryClient.setQueryData<EventPages>(historyLogKey(issueKey), (current) => {
    if (current === undefined) return current;
    return {
      ...current,
      pages: [[event, ...(current.pages[0] ?? [])], ...current.pages.slice(1)],
    };
  });
}
