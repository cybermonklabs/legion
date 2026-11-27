import type { ReactNode } from "react";

import { AGENT_LIST_REFILLING } from "../../api/client";
import { Chip } from "../../components/Chip";
import type { ControlSize } from "../../components/control-names";
import type { FilterChip } from "../../components/filter-chips";
import {
  borderDefault,
  focusVisibleRing,
  surfaceMutedHoverBg,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useIssueFilters } from "./issue-filters";

/** The flag chips both issue views share: Needs you and Unread, each toggling the URL-backed
 *  filter it names. The component reads `useIssueFilters` itself, so a view only places it, with
 *  its own size (`ControlSize`): the Board's comfortable, the List's compact. */
export function IssueFlagChips({ size = "comfortable" }: { size?: ControlSize }): ReactNode {
  const filters = useIssueFilters();
  return (
    <>
      <Chip
        onClick={() => filters.setNeedsYou(!filters.needsYou)}
        selected={filters.needsYou}
        size={size}
      >
        Needs you
      </Chip>
      <Chip
        onClick={() => filters.setUnread(!filters.unread)}
        selected={filters.unread}
        size={size}
      >
        Unread
      </Chip>
    </>
  );
}

/** The narrowings an address carries that are no column and no flag (`exclude_tree`), as the
 *  Filters panel lists them under the flags, each with the remove its chip in the row has
 *  (INFRA-3135, R3). Nothing for none. */
export function NarrowingList({ narrowings }: { narrowings: readonly FilterChip[] }): ReactNode {
  if (narrowings.length === 0) return null;
  return (
    <ul
      aria-label="Other filters"
      className={`flex w-full flex-col gap-1 border-t pt-2 ${borderDefault}`}
    >
      {narrowings.map(({ key, label, remove }) => (
        <li
          className={`flex w-full items-center justify-between gap-2 text-xs ${textSecondaryOnCanvas}`}
          key={key ?? label}
        >
          <span className="min-w-0 break-words">{label}</span>
          <button
            aria-label={`Remove ${label}`}
            className={`inline-flex size-11 shrink-0 items-center justify-center rounded-md md:size-8 ${surfaceMutedHoverBg} ${focusVisibleRing}`}
            onClick={remove}
            type="button"
          >
            <span aria-hidden="true">✕</span>
          </button>
        </li>
      ))}
    </ul>
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
