import type { ReactNode } from "react";

import type { IssuePriority } from "../../api/types";
import { priorityBadge } from "../../theme/classes";
import { badgeSelectBadge } from "../issue/badge-select";
import { laneLabel } from "../project/board-model";

/**
 * A priority in the one form the issue page draws it (design v3, rule 6; review a5), wherever the
 * command bar shows one - a plan row's value now and its new value, a new issue's line, a table's
 * cell: P0 to P3 as the page's tinted badge, the badge the page's priority menu draws
 * (`PriorityEditor`), and no priority in the page's words, "No priority".
 */
export function PriorityValue({
  priority,
}: {
  readonly priority: IssuePriority | null;
}): ReactNode {
  if (priority === null) return laneLabel(null);
  const tone = priorityBadge[priority];
  return (
    <span className={`${badgeSelectBadge} ${tone.bg} ${tone.text}`}>{laneLabel(priority)}</span>
  );
}
