import type { IssueDetails } from "../../api/types";
import type { IssueRoute } from "../refs/routes";

/** The one object the root hands to every slot: the issue as `issueQuery` loaded it, and the
 *  address as `parseIssuePath` read it. Every slot takes the same object, so a part that fills a
 *  slot reads what it needs from here and does not change the root. */
export interface SlotProps {
  readonly issue: IssueDetails;
  readonly route: IssueRoute;
}
