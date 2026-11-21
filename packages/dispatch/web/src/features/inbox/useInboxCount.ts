import { useQuery } from "@tanstack/react-query";

import { myRepliesCountQuery } from "../../api/queries";
import { useIsHuman } from "../../hooks/useIsHuman";
import { useNeedsYouCount } from "./BlockedOnYou";

/** How many of the Inbox's thread rows are unread: the `total` of
 *  `GET /api/v1/me/replies?kind=reply,agent&limit=1`, which the sidebar's and the compact
 *  header's one Inbox number adds to the work waiting on the reader. One request per page load,
 *  never the Inbox's own unpaged read. The route needs the `person` scope, which a signed-in
 *  browser holds, so nothing without it asks. Undefined until that read answers, and after a
 *  first read that failed. Its own module, so the shell reads it without loading the Inbox. */
export function useUnreadThreadCount(): number | undefined {
  return useQuery({ ...myRepliesCountQuery(), enabled: useIsHuman() }).data;
}

/** The one number the Inbox entry carries, wherever it is drawn - the sidebar's rail and the
 *  compact header's link: the work waiting on the reader (open asks whose turn is theirs and
 *  every pending credential request) plus the threads they have not read, both kinds of row
 *  being in the one list now. Each site draws its own badge; the number is this one. It is
 *  undefined while any read it adds up has not answered or has failed (`useNeedsYouCount`,
 *  `useUnreadThreadCount`), and a site then draws the entry without a number: a sum that
 *  counted an unread list as empty would be a number that is not the reader's. */
export function useInboxCount(): number | undefined {
  const waiting = useNeedsYouCount();
  const unread = useUnreadThreadCount();
  return waiting === undefined || unread === undefined ? undefined : waiting + unread;
}
