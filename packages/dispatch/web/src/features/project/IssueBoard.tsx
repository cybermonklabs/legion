import {
  type CollisionDetection,
  closestCorners,
  DndContext,
  type DragEndEvent,
  MouseSensor,
  pointerWithin,
  TouchSensor,
  useDroppable,
  useSensor,
  useSensors,
} from "@dnd-kit/core";
import { SortableContext, useSortable, verticalListSortingStrategy } from "@dnd-kit/sortable";
import { type QueryKey, useQuery } from "@tanstack/react-query";
import type { CSSProperties, ReactNode } from "react";
import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { userStateQuery } from "../../api/queries";
import type { IssueSummary, UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { LabelPill, Pill, StatusPill } from "../../components/Pill";
import { copyText } from "../../lib/clipboard";
import {
  borderDefault,
  borderStrong,
  card,
  cardHoverBorder,
  focusVisibleRing,
  linkHoverText,
  linkText,
  selectedCardBorder,
  surfaceMutedBg,
  textMutedOnCanvas,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ClaimChip } from "../issue/ClaimChip";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { ProgressChips } from "../issue/ProgressChips";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import { closeRefPreview, referenceTriggerProps } from "../refs/RefPreview";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching } from "../shell/roving";
import {
  announceMove,
  type BoardFocus,
  focusTarget,
  keyboardMove,
  type MoveDirection,
  type RoveKey,
} from "./board-keys";
import {
  type BoardColumn,
  dropTarget,
  groupItemsByLane,
  groupItemsByStage,
  type IssueStatus,
  laneColumnId,
  laneDropTarget,
  laneFromKey,
  laneKey,
  laneLabel,
  moveIssue,
  type PriorityLane,
  type StageColumn,
  type StageLane,
  statusLabel,
} from "./board-model";
import { useBoardMoves } from "./board-moves";
import { CollapsedColumn } from "./CollapsedColumn";
import { LegionPhaseCell } from "./issues-table-columns";
import { type Item, legionRolesText, legionSlotText, stageText } from "./issues-table-model";

import { issueIsUnread, UnreadDot } from "./UnreadDot";

const boardCollisionDetection: CollisionDetection = (args) => {
  const pointerCollisions = pointerWithin(args);
  return pointerCollisions.length > 0 ? pointerCollisions : closestCorners(args);
};

const CARD_SELECTOR = "[data-board-card]";
/** A card's title link, which the open keys follow: the card holds other links (a claim's
 *  agent), so "the first link" is not the issue's. */
const ISSUE_LINK = "[data-issue-link]";
const COLUMN_SELECTOR = "[data-board-column]";

/** The focus ring every board focus target draws: cards, columns and rails alike. */
const boardFocusRing = `outline-none focus-visible:ring-2 ${focusVisibleRing}`;

function cardAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, CARD_SELECTOR);
}

function columnAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, COLUMN_SELECTOR);
}

/**
 * The whole card is the drag activator: press and move 8 px (mouse) or hold 150 ms (touch)
 * anywhere on it to lift it, so a plain click on the title still follows the link and a
 * vertical swipe still scrolls. Mouse and touch are separate sensors on purpose: a pointer
 * sensor would also see a finger's first pointermove and lift the card for an instant before
 * the browser's pan cancels it. The keyboard never lifts a card: the board's registry rows
 * (`j`/`k`/`h`/`l` rove focus, `Shift+J/K/H/L` move through `moveCard`) are the accessible path,
 * so the card is `tabIndex={-1}` - reached by those keys, not by Tab - and `data-board-card`
 * names it for them. dnd-kit's default drag attributes would make the card a `button`; it stays
 * an `article` so nothing interactive is nested in a control.
 */
/** A card's Legion line: its phase as the table writes it, its slot and the roles in a turn. */
function CardLegion({ item }: { item: Item }): ReactNode {
  if (item.legion === undefined) return null;
  return (
    <div
      className="mt-2 flex flex-wrap items-start gap-x-3 gap-y-1 text-xs"
      data-board-legion={item.issue.key}
    >
      <LegionPhaseCell phase={item.legion} />
      <span data-legion-fact="slot">{legionSlotText(item.legion.card)}</span>
      <span data-legion-fact="roles">{legionRolesText(item.legion.card)}</span>
    </div>
  );
}

function IssueCard({ item, unread }: { item: Item; unread: boolean }): ReactNode {
  const { issue } = item;
  const {
    attributes,
    isDragging,
    listeners,
    setActivatorNodeRef,
    setNodeRef,
    transform,
    transition,
  } = useSortable({
    attributes: { role: "article", roleDescription: "card", tabIndex: -1 },
    id: issue.key,
  });
  const setCardRef = useCallback(
    (node: HTMLElement | null) => {
      setNodeRef(node);
      setActivatorNodeRef(node);
    },
    [setActivatorNodeRef, setNodeRef]
  );
  const style: CSSProperties = {
    transform:
      transform === null ? undefined : `translate3d(${transform.x}px, ${transform.y}px, 0)`,
    transition,
  };

  return (
    <article
      aria-label={`${issue.key} ${issue.title}`}
      className={`cursor-grab rounded-xl border p-3 ${card} ${cardHoverBorder} ${boardFocusRing} ${
        isDragging ? "opacity-50" : ""
      }`}
      data-board-card={issue.key}
      ref={setCardRef}
      style={style}
      {...attributes}
      {...listeners}
    >
      {/* Below `xl` every anchor is `inline-flex` with centred items (the 44 px touch rule in
          styles.css), so the key and title are stacked and start-aligned explicitly. */}
      <Link
        className={`flex flex-col text-sm ${linkText} ${linkHoverText}`}
        data-issue-link
        to={buildIssuePath({ key: issue.key, kind: "issue" })}
        {...referenceTriggerProps({ key: issue.key, kind: "issue" })}
      >
        <span className="self-start font-semibold">{issue.key}</span>
        <span className={`mt-1 self-start ${textPrimaryOnSurface}`}>{issue.title}</span>
      </Link>
      <CardLegion item={item} />
      <div className="mt-3 flex flex-wrap items-center gap-2">
        {unread ? <UnreadDot /> : null}
        <PriorityControl
          disabled={issue.status === "done"}
          issueKey={issue.key}
          priority={issuePriority(issue.priority)}
        />
        <ClaimChip claim={issue.claim} />
        <ProgressChips progress={issue.progress} />
        <UnreachableRouteMarker issue={issue} />
        {(issue.labels ?? []).map((label) => (
          <LabelPill key={label}>{label}</LabelPill>
        ))}
        {issue.open_asks === 0 ? null : (
          <span title={`${issue.open_asks} open asks`}>
            <AttentionBadge count={issue.open_asks} />
          </span>
        )}
      </div>
    </article>
  );
}

/**
 * One lifecycle column. Dispatch statuses accept drops; Legion phases report where the table's
 * current rows stand and deliberately have no drop target.
 */
const BoardColumnView = memo(function BoardColumnView({
  column,
  lane,
  userState,
}: {
  column: StageColumn;
  /** The swimlane this column sits inside, when it does; the flat board omits it. */
  lane?: PriorityLane;
  userState: UserState | undefined;
}): ReactNode {
  const status = column.droppable
    ? (column.stage.slice("status:".length) as IssueStatus)
    : undefined;
  const { isOver, setNodeRef } = useDroppable({
    disabled: !column.droppable,
    id:
      status === undefined
        ? `stage:${column.stage}`
        : lane === undefined
          ? `status:${status}`
          : laneColumnId(status, lane),
  });

  return (
    <section
      aria-label={stageText(column.stage)}
      className={`w-72 shrink-0 snap-start ${boardFocusRing}`}
      data-board-column={column.stage}
      data-board-lane={lane === undefined ? undefined : laneKey(lane)}
      ref={setNodeRef}
      tabIndex={-1}
    >
      <header
        className={`min-h-[52px] rounded-xl border px-3 py-2 ${borderDefault} ${surfaceMutedBg} ${
          isOver ? selectedCardBorder : ""
        }`}
        data-testid="board-column-header"
      >
        <div
          className={`flex items-center justify-between gap-2 text-base font-semibold ${textPrimaryOnSurface}`}
        >
          {status === undefined ? (
            <span>{stageText(column.stage)}</span>
          ) : (
            <StatusPill>{statusLabel(status)}</StatusPill>
          )}
          <Pill>{column.items.length}</Pill>
        </div>
      </header>
      {column.items.length === 0 ? (
        <div
          className={`mt-3 min-h-40 rounded-xl border border-dashed px-3 py-6 text-center text-xs ${textMutedOnCanvas} ${
            isOver ? selectedCardBorder : borderStrong
          }`}
        >
          Nothing here
        </div>
      ) : (
        <SortableContext
          items={column.items.map((item) => item.issue.key)}
          strategy={verticalListSortingStrategy}
        >
          <div className="mt-3 space-y-3">
            {column.items.map((item) => (
              <IssueCard
                item={item}
                key={item.issue.key}
                unread={issueIsUnread(item.issue, userState?.[item.issue.key]?.last_read_seq ?? 0)}
              />
            ))}
          </div>
        </SortableContext>
      )}
    </section>
  );
});

/**
 * One swimlane: a heading naming the priority band and its issue count, then the current view's
 * stage columns. Only status columns are droppable.
 */
const BoardLaneRow = memo(function BoardLaneRow({
  boardLane,
  dragging,
  isCollapsed,
  userState,
}: {
  boardLane: StageLane;
  dragging: boolean;
  isCollapsed: (column: StageColumn) => boolean;
  userState: UserState | undefined;
}): ReactNode {
  const count = boardLane.columns.reduce((total, column) => total + column.items.length, 0);
  return (
    <section aria-label={`${laneLabel(boardLane.lane)} lane`}>
      <h2
        className={`mb-2 flex items-center gap-2 text-xs font-semibold uppercase tracking-wide ${textSecondaryOnSurface}`}
      >
        {laneLabel(boardLane.lane)}
        <Pill>{count}</Pill>
      </h2>
      <div
        className={`overflow-x-auto pb-3 ${dragging ? "" : "snap-x snap-mandatory"}`}
        data-testid="board-lane-scroll-container"
      >
        <div className="flex w-max items-stretch gap-4">
          {boardLane.columns.map((column) =>
            isCollapsed(column) ? (
              <CollapsedColumn column={column} key={column.stage} lane={boardLane.lane} />
            ) : (
              <BoardColumnView
                column={column}
                key={column.stage}
                lane={boardLane.lane}
                userState={userState}
              />
            )
          )}
        </div>
      </div>
    </section>
  );
});

export function IssueBoard({
  project,
  items,
  listingKeys,
  showEdges = false,
  lanes = false,
}: {
  project: string;
  /** The rows the active table view already filtered, searched and sorted. */
  items: readonly Item[];
  /** The open and, when loaded, closed listing caches that can hold one of these rows. */
  listingKeys: readonly QueryKey[];
  /** Render Icebox and Done as full columns instead of collapsed rails. */
  showEdges?: boolean;
  /** Group the board into swimlanes by priority. */
  lanes?: boolean;
}): ReactNode {
  const userState = useQuery(userStateQuery());
  const visibleKeys = useMemo(() => new Set(items.map((item) => item.issue.key)), [items]);
  const isVisible = useCallback((issue: IssueSummary) => visibleKeys.has(issue.key), [visibleKeys]);
  const { error, moveCard, moveCardToLane } = useBoardMoves(project, listingKeys, isVisible);
  const sensors = useSensors(
    useSensor(MouseSensor, { activationConstraint: { distance: 8 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 150, tolerance: 5 } })
  );
  // While a card is in flight the scroller must not snap: dnd-kit auto-scrolls the board when
  // the pointer nears an edge, and with mandatory snapping every scroll tick jumps a whole
  // column, so one drag near the edge flies to the far end. Snapping is for finger swipes at
  // rest, so it is switched off from lift to drop.
  const [dragging, setDragging] = useState(false);
  // The click that trails a drop (mouseup or touchend on the card) is stopped by dnd-kit before
  // React sees it, which is enough for buttons but not for the title link: the browser's own
  // navigation still runs and reloads the app on the issue page. The drop itself marks that
  // click as the drag's, rather than a window of milliseconds after it: the browser fires the
  // click in the gesture that was dropped, however long a busy main thread takes to deliver it,
  // and a window that has lapsed by then lets the drop navigate away from the board. The mark is
  // spent by the first click after the drop, and the press that begins the next gesture - of a
  // pointer or of a key, since `o` and Enter open the focused card by clicking its link - drops
  // it, so a drop that draws no click of its own (a touch drag, a drag the browser cancels)
  // never swallows a later one. Only the board's own clicks are cancelled - a link elsewhere on
  // the page must keep working meanwhile.
  const boardRef = useRef<HTMLElement>(null);
  const clickBelongsToDrop = useRef(false);
  useEffect(() => {
    // `capture: true` on `window` is load-bearing, not decoration: the keymap listens for keys on
    // window's bubble phase (`shell/KeymapProvider.tsx`), and `o` and Enter open the focused card
    // by clicking its link, so this has to have dropped the mark before that key is acted on. In
    // the bubble phase it would run after the click it is meant to let through.
    const forgetDrop = () => {
      clickBelongsToDrop.current = false;
    };
    const cancelTrailingClick = (event: MouseEvent) => {
      if (!clickBelongsToDrop.current) {
        return;
      }
      clickBelongsToDrop.current = false;
      if (event.target instanceof Node && boardRef.current?.contains(event.target)) {
        event.preventDefault();
      }
    };
    window.addEventListener("keydown", forgetDrop, { capture: true });
    window.addEventListener("pointerdown", forgetDrop, { capture: true });
    window.addEventListener("click", cancelTrailingClick, { capture: true });
    return () => {
      window.removeEventListener("keydown", forgetDrop, { capture: true });
      window.removeEventListener("pointerdown", forgetDrop, { capture: true });
      window.removeEventListener("click", cancelTrailingClick, { capture: true });
    };
  }, []);
  // The title link is both the hover-card trigger and part of the whole-card drag activator, and
  // the click that would dismiss an open card after the drop is the one swallowed above - so a
  // lift closes the card (open or still pending) itself. The lift marks the gesture's click as
  // well as the drop does: dnd-kit builds its drop event from a lift React has committed, so a
  // flick fast enough to release before that commit - which a stalled main thread makes of any
  // flick - ends with no drop event at all, and the mark has to be there already.
  const liftCard = () => {
    clickBelongsToDrop.current = true;
    closeRefPreview();
    setDragging(true);
  };
  const landCard = () => {
    clickBelongsToDrop.current = true;
    setDragging(false);
  };
  const columns = useMemo(() => groupItemsByStage(items), [items]);
  const boardLanes = useMemo(() => (lanes ? groupItemsByLane(items) : []), [items, lanes]);
  const onDragEnd = ({ active, over }: DragEndEvent) => {
    landCard();
    if (over === null) return;
    const activeKey = String(active.id);
    const overKey = String(over.id);
    const overColumn = columns.find((column) =>
      column.items.some((item) => item.issue.key === overKey)
    );
    // A phase card is read-only as a target. It is still draggable to any status column.
    if (overColumn !== undefined && !overColumn.droppable) return;
    const visible = items.map((item) => item.issue);
    if (lanes) {
      const target = laneDropTarget(visible, activeKey, overKey);
      if (target !== undefined)
        void moveCardToLane(activeKey, target.status, target.lane, target.insertionIndex);
      return;
    }
    const target = dropTarget(visible, activeKey, overKey);
    if (target !== undefined) void moveCard(activeKey, target.status, target.insertionIndex);
  };
  /** Icebox and Done are rails — only status columns collapse. */
  const isCollapsed = useCallback(
    (column: StageColumn) =>
      column.droppable &&
      !showEdges &&
      (column.stage === "status:icebox" || column.stage === "status:done"),
    [showEdges]
  );

  // One `aria-live` sentence per keyboard move; `focusAfterMove` names the card focus follows
  // once the optimistic list has rendered (a status move remounts the article under its new
  // column, so the old node cannot keep focus; a card moved into a rail has no node, and focus
  // lands on the rail). The effect runs on the `columns`/`boardLanes` render - the
  // announcement's own render comes first, before the query observer has notified, and would
  // focus the doomed node.
  const [announcement, setAnnouncement] = useState("");
  const focusAfterMove = useRef<{
    key: string;
    status: IssueStatus;
    laneId: string | null;
  } | null>(null);
  const boardNode = (selector: string) => boardRef.current?.querySelector<HTMLElement>(selector);
  const laneColumnSelector = (status: IssueStatus, laneId: string | null): string =>
    laneId === null
      ? `[data-board-column="status:${status}"]`
      : `[data-board-column="status:${status}"][data-board-lane="${laneId}"]`;
  // biome-ignore lint/correctness/useExhaustiveDependencies: `columns`/`boardLanes` are the trigger, not a read; `boardNode` reads a ref
  useEffect(() => {
    const pending = focusAfterMove.current;
    if (pending === null) return;
    focusAfterMove.current = null;
    (
      boardNode(`[data-board-card="${pending.key}"]`) ??
      boardNode(laneColumnSelector(pending.status, pending.laneId))
    )?.focus();
  }, [columns, boardLanes]);
  const statusColumns = (stageColumns: readonly StageColumn[]): BoardColumn[] =>
    stageColumns
      .filter((column) => column.droppable)
      .map((column) => ({
        status: column.stage.slice("status:".length) as IssueStatus,
        issues: column.items.map((item) => item.issue),
      }));
  /** The status columns keyboard actions can focus or write. Phase columns remain readable cards,
   * but a drag, rather than a keyboard status action, is their route to a Dispatch status. */
  const focusedLaneColumns = (laneId: string | null): BoardColumn[] =>
    statusColumns(
      lanes && laneId !== null
        ? (boardLanes.find((entry) => laneKey(entry.lane) === laneId)?.columns ?? [])
        : columns
    );
  const currentLaneId = (): string | null => {
    const explicit = columnAround(document.activeElement)?.dataset.boardLane;
    if (explicit !== undefined) return explicit;
    return lanes ? laneKey(boardLanes[0]?.lane ?? null) : null;
  };
  const currentFocus = (): BoardFocus | null => {
    const active = document.activeElement;
    const column = columnAround(active);
    const stage = column?.dataset.boardColumn;
    if (column === null || stage === undefined || !stage.startsWith("status:")) return null;
    const status = stage.slice("status:".length) as IssueStatus;
    const key = cardAround(active)?.dataset.boardCard;
    const index =
      key === undefined
        ? -1
        : (focusedLaneColumns(column.dataset.boardLane ?? null)
            .find((entry) => entry.status === status)
            ?.issues.findIndex((issue) => issue.key === key) ?? -1);
    return { status, index: index === -1 ? null : index };
  };
  const rove = (key: RoveKey) => {
    const laneId = currentLaneId();
    const laneColumns = focusedLaneColumns(laneId);
    const target = focusTarget(
      laneColumns,
      (status) => !showEdges && (status === "icebox" || status === "done"),
      currentFocus(),
      key
    );
    const issue =
      target.index === null
        ? undefined
        : laneColumns.find((entry) => entry.status === target.status)?.issues[target.index];
    (issue === undefined
      ? boardNode(laneColumnSelector(target.status, laneId))
      : boardNode(`[data-board-card="${issue.key}"]`)
    )?.focus();
  };
  const move = (direction: MoveDirection) => {
    const key = cardAround(document.activeElement)?.dataset.boardCard;
    if (key === undefined) return;
    const laneId = currentLaneId();
    const laneColumns = focusedLaneColumns(laneId);
    const target = keyboardMove(laneColumns, key, direction);
    if (target === undefined) return;
    const lane: PriorityLane | undefined = laneId === null ? undefined : laneFromKey(laneId);
    if (lanes && lane !== undefined) {
      void moveCardToLane(key, target.status, lane, target.insertionIndex);
    } else {
      void moveCard(key, target.status, target.insertionIndex);
    }
    const moved = moveIssue(
      items.map((item) => item.issue),
      key,
      target.status,
      target.insertionIndex
    );
    const column = (moved?.issues ?? []).filter(
      (issue) => issue.status === target.status && (lane === undefined || issue.priority === lane)
    );
    const position = column.findIndex((issue) => issue.key === key);
    setAnnouncement(announceMove(key, target.status, position + 1, column.length));
    focusAfterMove.current = { key, status: target.status, laneId };
  };
  const focusedCard = () => cardAround(document.activeElement) !== null;
  /** Copies the focused card's key or `dispatch://` reference and announces which. */
  const copyFocusedCard = (form: "key" | "reference") => {
    const key = cardAround(document.activeElement)?.dataset.boardCard;
    if (key === undefined) {
      return;
    }
    const text = form === "key" ? key : buildDispatchReference({ key, kind: "issue" });
    void copyText(text).then((copied) =>
      setAnnouncement(copied ? `Copied ${text}` : `Copy failed - ${text}`)
    );
  };
  const focusedBoardNode = () =>
    document.activeElement?.matches(`${CARD_SELECTOR}, ${COLUMN_SELECTOR}`) === true;
  useKeymapScope("board");
  useKeymap("board", [
    // Movement is what the arrow keys are for; a palette row that moves the cursor helps nobody.
    { id: "next", keys: "j", label: "Next card", palette: false, run: () => rove("j") },
    { id: "previous", keys: "k", label: "Previous card", palette: false, run: () => rove("k") },
    { id: "column", keys: "l", label: "Next column", palette: false, run: () => rove("l") },
    {
      id: "previous-column",
      keys: "h",
      label: "Previous column",
      palette: false,
      run: () => rove("h"),
    },
    {
      id: "arrows-vertical",
      keys: ["ArrowDown", "ArrowUp"],
      label: "Next / previous card while one is focused",
      run: (event) => rove(event.key === "ArrowDown" ? "j" : "k"),
      when: focusedBoardNode,
    },
    {
      id: "arrows-horizontal",
      keys: ["ArrowRight", "ArrowLeft"],
      label: "Next / previous column while one is focused",
      run: (event) => rove(event.key === "ArrowRight" ? "l" : "h"),
      when: focusedBoardNode,
    },
    {
      id: "move-down",
      keys: "Shift+J",
      label: "Move card down",
      run: () => move("down"),
      when: focusedCard,
    },
    {
      id: "move-up",
      keys: "Shift+K",
      label: "Move card up",
      run: () => move("up"),
      when: focusedCard,
    },
    {
      id: "move-next",
      keys: "Shift+L",
      label: "Move card to the next status",
      run: () => move("next"),
      when: focusedCard,
    },
    {
      id: "move-previous",
      keys: "Shift+H",
      label: "Move card to the previous status",
      run: () => move("prev"),
      when: focusedCard,
    },
    {
      id: "open",
      keys: "o",
      label: "Open issue",
      run: () =>
        cardAround(document.activeElement)?.querySelector<HTMLElement>(ISSUE_LINK)?.click(),
      when: focusedCard,
    },
    {
      // Only from the card itself: Enter on the title link is the browser's own navigation. Out of
      // the palette, where `open` above is already the row for the same action.
      id: "open-enter",
      keys: "Enter",
      label: "Open the focused card's issue",
      palette: false,
      run: () =>
        cardAround(document.activeElement)?.querySelector<HTMLElement>(ISSUE_LINK)?.click(),
      when: () => document.activeElement?.matches(CARD_SELECTOR) === true,
    },
    {
      id: "priority",
      keys: "p",
      label: "Focus the card's priority",
      run: () => cardAround(document.activeElement)?.querySelector("select")?.focus(),
      when: focusedCard,
    },
    {
      id: "copy-ref",
      keys: "y",
      label: "Copy the card's issue reference",
      run: () => copyFocusedCard("reference"),
      when: focusedCard,
    },
    {
      id: "copy-key",
      keys: "Shift+Y",
      label: "Copy the card's issue key",
      run: () => copyFocusedCard("key"),
      when: focusedCard,
    },
    {
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back to the card, then out",
      run: () => {
        const active = document.activeElement;
        const card = cardAround(active);
        if (card === active || (card === null && columnAround(active) === active)) {
          (active as HTMLElement).blur();
        } else {
          card?.focus();
        }
      },
      when: () =>
        cardAround(document.activeElement) !== null ||
        columnAround(document.activeElement) === document.activeElement,
    },
  ]);
  return (
    <section aria-label="Project board" className="min-w-0" ref={boardRef}>
      {error === undefined ? null : (
        <div
          aria-live="assertive"
          className={`mb-4 rounded-xl border px-4 py-3 text-sm ${card} ${textPrimaryOnSurface}`}
          role="alert"
        >
          {error}
        </div>
      )}
      <p aria-label="Board announcements" aria-live="polite" className="sr-only" role="status">
        {announcement}
      </p>
      {items.length === 0 ? (
        <EmptyState label="Empty project board" message="No issues in this project." />
      ) : (
        <DndContext
          accessibility={{
            screenReaderInstructions: {
              draggable:
                "Focus a card and press Shift with J, K, H or L to move it; press ? for every shortcut.",
            },
          }}
          collisionDetection={boardCollisionDetection}
          onDragCancel={landCard}
          onDragEnd={onDragEnd}
          onDragStart={liftCard}
          sensors={sensors}
        >
          {lanes ? (
            <div className="flex flex-col gap-6" data-testid="board-lanes-container">
              {boardLanes.map((boardLane) => (
                <BoardLaneRow
                  boardLane={boardLane}
                  dragging={dragging}
                  isCollapsed={isCollapsed}
                  key={laneKey(boardLane.lane)}
                  userState={userState.data}
                />
              ))}
            </div>
          ) : (
            <div
              className={`overflow-x-auto pb-3 ${dragging ? "" : "snap-x snap-mandatory"}`}
              data-testid="board-scroll-container"
            >
              <div className="flex w-max items-stretch gap-4">
                {columns.map((column) =>
                  isCollapsed(column) ? (
                    <CollapsedColumn column={column} key={column.stage} />
                  ) : (
                    <BoardColumnView
                      column={column}
                      key={column.stage}
                      userState={userState.data}
                    />
                  )
                )}
              </div>
            </div>
          )}
        </DndContext>
      )}
    </section>
  );
}
