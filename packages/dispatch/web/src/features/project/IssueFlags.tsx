import type { ReactNode } from "react";

import { AGENT_LIST_REFILLING } from "../../api/client";
import { Chip } from "../../components/Chip";
import { textSecondaryOnCanvas } from "../../theme/shell-classes";
import { useIssueFilters } from "./issue-filters";

/** The flag chips both issue views share: Needs you and Unread, each toggling the URL-backed
 *  filter it names. The component reads `useIssueFilters` itself, so a view only places it.
 *  `inToolbar` is the List's table toolbar's, where from 1280 px up the chips take the height
 *  of the controls beside them (`Chip`). */
export function IssueFlagChips({ inToolbar = false }: { inToolbar?: boolean }): ReactNode {
  const filters = useIssueFilters();
  return (
    <>
      <Chip
        inToolbar={inToolbar}
        onClick={() => filters.setNeedsYou(!filters.needsYou)}
        selected={filters.needsYou}
      >
        Needs you
      </Chip>
      <Chip
        inToolbar={inToolbar}
        onClick={() => filters.setUnread(!filters.unread)}
        selected={filters.unread}
      >
        Unread
      </Chip>
    </>
  );
}

/** Why the Unclaimed filter cannot tell which claims are takeable, said once for both views;
 *  nothing while `reason` is undefined - the list is current, loading, or the filter is off. */
export function UnclaimedUnchecked({
  reason,
}: {
  reason: "refilling" | "unreachable" | undefined;
}): ReactNode {
  if (reason === undefined) return null;
  return (
    <p
      className={`text-sm ${textSecondaryOnCanvas}`}
      data-testid="unclaimed-unchecked"
      role="status"
    >
      Couldn't check which claims have lapsed, so every claim counts as held:{" "}
      {reason === "refilling" ? AGENT_LIST_REFILLING : "Envoy listener unreachable."}
    </p>
  );
}
