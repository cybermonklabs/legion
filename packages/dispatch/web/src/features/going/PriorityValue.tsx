import type { ReactNode } from "react";

import { priorityBadge } from "../../theme/classes";
import { badgeSelectBadge } from "../issue/badge-select";
import type { PriorityView } from "../issue/issue-priority";
import { PrioritySource } from "../issue/PrioritySource";
import { laneLabel } from "../project/board-model";

/**
 * A priority in the one form the issue page draws it (design v3, rule 6; review a5), wherever the
 * command bar shows one - a plan row's value now and its new value, a new issue's line, a table's
 * cell: the priority the issue works at (`issuePriority`), P0 to P3 as the page's tinted badge,
 * the badge the page's priority menu draws (`PriorityEditor`), and no priority in the page's
 * words, "No priority". An inherited priority names the issue it comes from after the badge, as
 * that menu does: "P0 · from ACME-1" (`PrioritySource`).
 */
export function PriorityValue({ priority }: { readonly priority: PriorityView }): ReactNode {
  const { from, level } = priority;
  if (level === null) return laneLabel(null);
  const tone = priorityBadge[level];
  const badge = (
    <span className={`${badgeSelectBadge} ${tone.bg} ${tone.text}`}>{laneLabel(level)}</span>
  );
  if (from === null) return badge;
  return (
    <span className="inline-flex flex-wrap items-baseline">
      {badge}
      <PrioritySource className="ml-1 text-xs" from={from} />
    </span>
  );
}
