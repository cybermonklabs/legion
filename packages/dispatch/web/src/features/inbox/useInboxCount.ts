import { useQuery } from "@tanstack/react-query";

import { meInboxCountQuery } from "../../api/queries";
import { useIsHuman } from "../../hooks/useIsHuman";
import { useNeedsYouCount } from "./BlockedOnYou";

/** How many of the Inbox's thread rows are unread: the `total` of
 *  `GET /api/v1/me/inbox?state=unread&limit=1`, which the sidebar's and the compact header's one
 *  Inbox number adds to the work waiting on the reader. A one-entry read of its own rather than a
 *  count over the Inbox's unpaged read, which the number's other half reads (`useNeedsYouCount`):
 *  the unread threads show without waiting on that read. The route needs the `person` scope,
 *  which a signed-in browser holds, so nothing without it asks. Its own module, so the shell
 *  reads it without loading the Inbox. */
export function useUnreadThreadCount(): number {
  const human = useIsHuman();
  return useQuery({ ...meInboxCountQuery(), enabled: human }).data ?? 0;
}

/** The one number the Inbox entry carries, wherever it is drawn - the sidebar's rail and the
 *  compact header's link: the work waiting on the reader (open asks whose turn is theirs and
 *  every pending credential request) plus the threads they have not read, both kinds of row
 *  being in the one list now. Each site draws its own badge; the number is this one. */
export function useInboxCount(): number {
  return useNeedsYouCount() + useUnreadThreadCount();
}
