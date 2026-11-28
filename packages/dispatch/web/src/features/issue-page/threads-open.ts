import { useLocation } from "react-router-dom";

import { parseIssuePath } from "../refs/routes";
import { issueAddress } from "./issue-address";

/**
 * Whether Threads is the issue page's open view (`IssueAddress.preset`), read off the address
 * as the foot reads it (`slots/foot.tsx`); false off the issue page. The preset reads nothing of
 * the issue's spec, so no spec slug is given, as the page's keys read it (`useIssueKeymap.ts`).
 *
 * Threads mounts no box at the foot (dispatch://INFRA-5704/spec#b-r-joins, R16), so under it
 * Details draws neither control that aims that box: a turn of its history draws no Reply
 * (`history-entries.tsx`), and the facts of the reader's own comment no Edit
 * (`FocusedNodeFacts.tsx`). Threads' own record carries the reply and the edit of a thread it
 * lists. Under every other view both are drawn.
 */
export function useThreadsOpen(): boolean {
  const { hash, pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  return route !== undefined && issueAddress(route, search, hash, undefined).preset === "threads";
}
