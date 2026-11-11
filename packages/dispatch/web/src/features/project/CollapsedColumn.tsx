import { useDroppable } from "@dnd-kit/core";
import type { ReactNode } from "react";

import { Pill } from "../../components/Pill";
import {
  borderDefault,
  focusVisibleRing,
  selectedCardBorder,
  surfaceMutedBg,
  textPrimaryOnSurface,
} from "../../theme/classes";
import {
  type IssueStatus,
  laneColumnId,
  laneKey,
  type PriorityLane,
  type StageColumn,
} from "./board-model";
import { stageText } from "./issues-table-model";

/**
 * Icebox and Done while the board's edges are hidden: a narrow rail with the status name and
 * its count, no cards. It is the same droppable (`status:<s>` flat, `lane:<l>|status:<s>` inside
 * a swimlane) a full column is, and like every column it spans the board's height, so a card
 * dragged onto it anywhere along the rail is appended to that cell - into Done that closes the
 * issue, exactly as dropping into the full column would. Like a column it is a `tabIndex={-1}`
 * focus target for the board's `h`/`l` keys, and a card moved into it by `Shift+H/L` hands focus
 * to the rail.
 */
export function CollapsedColumn({
  column,
  lane,
}: {
  column: StageColumn;
  /** The swimlane this rail sits inside, when it does; the flat board omits it. */
  lane?: PriorityLane;
}): ReactNode {
  const status = column.stage.slice("status:".length) as IssueStatus;
  const { isOver, setNodeRef } = useDroppable({
    id: lane === undefined ? `status:${status}` : laneColumnId(status, lane),
  });
  const label = stageText(column.stage);

  return (
    <section
      aria-label={`${label} (collapsed)`}
      className={`w-10 shrink-0 snap-start outline-none focus-visible:ring-2 ${focusVisibleRing}`}
      data-board-column={column.stage}
      data-board-lane={lane === undefined ? undefined : laneKey(lane)}
      ref={setNodeRef}
      tabIndex={-1}
    >
      <div
        className={`flex h-full min-h-[52px] flex-col items-center gap-3 rounded-xl border px-1 py-3 ${borderDefault} ${surfaceMutedBg} ${
          isOver ? selectedCardBorder : ""
        }`}
        data-testid="board-column-rail"
      >
        <Pill>{column.items.length}</Pill>
        <span
          className={`text-sm font-semibold tracking-wide ${textPrimaryOnSurface}`}
          style={{ writingMode: "vertical-rl" }}
        >
          {label}
        </span>
      </div>
    </section>
  );
}
