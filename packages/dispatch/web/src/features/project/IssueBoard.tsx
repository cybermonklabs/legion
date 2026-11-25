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
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { CSSProperties, ReactNode, RefObject } from "react";
import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { projectIssuesQuery, userStateQuery } from "../../api/queries";
import type { IssueSummary, UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { Chip } from "../../components/Chip";
import { ControlRow } from "../../components/ControlRow";
import { EmptyState } from "../../components/EmptyState";
import { filterChips } from "../../components/filter-chips";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { MultiSelect } from "../../components/MultiSelect";
import { PanelTrigger } from "../../components/PanelTrigger";
import { LabelPill, Pill, StatusPill } from "../../components/Pill";
import { SearchBox } from "../../components/SearchBox";
import { useNestedPanels } from "../../components/useNestedPanels";
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
import { PRIORITY_TRIGGER_SELECTOR, PriorityControl } from "../issue/PriorityControl";
import { ProgressChips } from "../issue/ProgressChips";
import { ParentStatusDialog } from "../issue/parent-status/ParentStatusDialog";
import { UnreachableRouteMarker } from "../issue/RouteReach";
import { closeRefPreview, referenceTriggerProps } from "../refs/RefPreview";
import { buildDispatchReference, buildIssuePath } from "../refs/routes";
import { keyText, listed } from "../shell/key-text";
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { useActionKeys } from "../shell/keymap-keys";
import { openLinkInNewTab } from "../shell/new-tab";
import { closestMatching, focusOnDocument } from "../shell/roving";
import { useUserPreference } from "../shell/userPreference";
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
  type BoardLane,
  dropTarget,
  groupIssuesByLane,
  groupIssuesByStatus,
  type IssueStatus,
  laneColumnId,
  laneDropTarget,
  laneFromKey,
  laneKey,
  laneLabel,
  type PriorityLane,
  statusLabel,
} from "./board-model";
import { useBoardMoves } from "./board-moves";
import { CollapsedColumn } from "./CollapsedColumn";
import { IssueFlagChips, UnclaimedUnchecked } from "./IssueFlags";
import { projectIssuesQueryKey, useIssueFilters } from "./issue-filters";
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
/** A card's priority trigger, where the card draws it unheld: `p` presses it. */
const CARD_PRIORITY = `${PRIORITY_TRIGGER_SELECTOR}:not([aria-disabled="true"])`;

/** The focus ring every board focus target draws: cards, columns and rails alike. */
const boardFocusRing = `outline-none focus-visible:ring-2 ${focusVisibleRing}`;

function cardAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, CARD_SELECTOR);
}

function columnAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, COLUMN_SELECTOR);
}

/** What a screen reader hears of a draggable card: the keys that move the focused card, read
 *  `Shift with J, K, H or L` (`⇧ with J, K, H or L` on a Mac) while each is Shift and a letter, as
 *  they are by default, else each as the platform draws it (`keyText`), and the key of the
 *  shortcut help. Both are the person's keys (`useActionKeys`). */
function dragInstructions(
  moveKeys: readonly (string | undefined)[],
  helpKey: string | undefined
): string {
  const keys = moveKeys.filter((key): key is string => key !== undefined);
  const shifted = keys.every((key) => /^Shift\+[A-Za-z]$/.test(key));
  const named = shifted
    ? keys.map((key) => key.slice("Shift+".length).toUpperCase())
    : keys.map((key) => keyText(key));
  const move =
    keys.length === 0
      ? "Focus a card"
      : `Focus a card and press ${shifted ? `${keyText("Shift")} with ` : ""}${listed(named, "or")} to move it`;
  return helpKey === undefined
    ? `${move}.`
    : `${move}; press ${keyText(helpKey)} for every shortcut.`;
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
function IssueCard({ issue, unread }: { issue: IssueSummary; unread: boolean }): ReactNode {
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
 * One lifecycle column: the `status:<s>` droppable (`lane:<l>|status:<s>` inside a swimlane).
 * Every column stretches to the board's height, so a card can be dropped anywhere in a column's
 * body - below its last card, or into an empty column whose header has scrolled out of view on
 * a phone - not only on the header.
 */
const BoardColumnView = memo(function BoardColumnView({
  column,
  lane,
  userState,
}: {
  column: BoardColumn;
  /** The swimlane this column sits inside, when it does; the flat board omits it. */
  lane?: PriorityLane;
  userState: UserState | undefined;
}): ReactNode {
  const { isOver, setNodeRef } = useDroppable({
    id: lane === undefined ? `status:${column.status}` : laneColumnId(column.status, lane),
  });

  return (
    <section
      aria-label={statusLabel(column.status)}
      className={`w-72 shrink-0 snap-start ${boardFocusRing}`}
      data-board-column={column.status}
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
          <StatusPill>{statusLabel(column.status)}</StatusPill>
          <Pill>{column.issues.length}</Pill>
        </div>
      </header>
      {column.issues.length === 0 ? (
        // An empty column rendered as a bare header: the board lost its shape wherever a
        // status had no work, and the droppable - which is the whole section - was 52 px tall.
        // A dashed body keeps the column's width and its drop area visible.
        <div
          className={`mt-3 min-h-40 rounded-xl border border-dashed px-3 py-6 text-center text-xs ${textMutedOnCanvas} ${
            isOver ? selectedCardBorder : borderStrong
          }`}
        >
          Nothing here
        </div>
      ) : (
        <SortableContext
          items={column.issues.map((issue) => issue.key)}
          strategy={verticalListSortingStrategy}
        >
          <div className="mt-3 space-y-3">
            {column.issues.map((issue) => (
              <IssueCard
                issue={issue}
                key={issue.key}
                unread={issueIsUnread(issue, userState?.[issue.key]?.last_read_seq ?? 0)}
              />
            ))}
          </div>
        </SortableContext>
      )}
    </section>
  );
});

/**
 * One swimlane: a heading naming the priority band and its issue count, then the same
 * horizontally scrolling row of nine lifecycle columns a flat board renders - each column's
 * droppable id scoped to this lane (`laneColumnId`) so a card dropped in it takes on the lane's
 * priority along with its status.
 */
const BoardLaneRow = memo(function BoardLaneRow({
  boardLane,
  dragging,
  isCollapsed,
  userState,
}: {
  boardLane: BoardLane;
  dragging: boolean;
  isCollapsed: (status: IssueStatus) => boolean;
  userState: UserState | undefined;
}): ReactNode {
  const count = boardLane.columns.reduce((total, column) => total + column.issues.length, 0);
  return (
    <section aria-label={`${laneLabel(boardLane.lane)} lane`}>
      <h2
        className={`mb-2 flex items-center gap-2 text-xs font-semibold uppercase tracking-wide ${textSecondaryOnSurface}`}
      >
        {laneLabel(boardLane.lane)}
        <Pill>{count}</Pill>
      </h2>
      {/* No Tab stop of its own, which Firefox gives a box that scrolls, with no name, and every
          engine where a lane holds no card: `h`/`l` and `j`/`k` rove the focus over its columns,
          rails and cards, which brings each into view (`IssueBoard`'s registry rows). */}
      <div
        className={`overflow-x-auto pb-3 ${dragging ? "" : "snap-x snap-mandatory"}`}
        data-testid="board-lane-scroll-container"
        tabIndex={-1}
      >
        <div className="flex w-max items-stretch gap-4">
          {boardLane.columns.map((column) =>
            isCollapsed(column.status) ? (
              <CollapsedColumn column={column} key={column.status} lane={boardLane.lane} />
            ) : (
              <BoardColumnView
                column={column}
                key={column.status}
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

/**
 * Draws nothing. While the rule's question is open, `tookFocusFrom` holds the element the question
 * took the focus from as it opened, which the question gives the focus back to as it closes
 * (`useDialog`). Once the question has closed, it holds `null`. The board draws this just before
 * the question, in one fragment with it. A commit runs the layout effects of siblings in their
 * order, so this effect reads the focus before the question's own effect takes it. A development
 * build's StrictMode runs the effects of a new subtree a second time, the whole fragment at once:
 * the question has given the focus back by then, so the second read finds the same element.
 */
function QuestionOpening({ tookFocusFrom }: { tookFocusFrom: RefObject<Element | null> }): null {
  useLayoutEffect(() => {
    tookFocusFrom.current = document.activeElement;
    return () => {
      tookFocusFrom.current = null;
    };
  }, [tookFocusFrom]);
  return null;
}

export function IssueBoard({
  project,
  showEdges = false,
  lanes = false,
}: {
  project: string;
  /** Render Icebox and Done as full columns instead of collapsed rails. */
  showEdges?: boolean;
  /** Group the board into swimlanes by priority - rows across the nine status columns, one band
   *  per priority plus one for no priority - rank still ordering cards within a lane+column
   *  cell. */
  lanes?: boolean;
}): ReactNode {
  const queryClient = useQueryClient();
  const filters = useIssueFilters();
  const { activeFilterCount, activeFilters, labels, matches, setLabels } = filters;
  // The same query-key pair the List keeps: the plain project list, or the server-filtered
  // list while labels are active - shared keys, shared caches.
  const queryKey = projectIssuesQueryKey(project, labels);
  const allIssues = useQuery(projectIssuesQuery(project, []));
  const labelledIssues = useQuery({
    ...projectIssuesQuery(project, labels),
    enabled: labels.length > 0,
  });
  const issues = labels.length === 0 ? allIssues : labelledIssues;
  const userState = useQuery(userStateQuery());
  const availableLabels = useMemo(
    () =>
      [...new Set((allIssues.data ?? []).flatMap((issue) => issue.labels ?? []))].sort(
        (left, right) => left.localeCompare(right)
      ),
    [allIssues.data]
  );
  const noIdentityFiltersExpanded = labels.length > 0;
  // The saved preference is the only owner of the panel's open state: the Board reads it when it
  // mounts and when the signed-in identity resolves, never because the filter count moved.
  // Arriving from another page, tab or project mounts a new board, so that read covers the
  // filters the route brings. On a board that is up the count moves when the reader filters in
  // it, and when Back or Forward crosses entries of the Issues tab. A re-read would close the
  // panel the moment the last filter goes (clearing the search box), taking the control being
  // typed in with it, or open or close it against the reader's choice on the others, while its
  // trigger and chips already show every filter. So it stays as they left it until they close it
  // or leave the project or its Issues tab.
  const [filtersExpanded, setFiltersExpanded] = useUserPreference(
    "project.issue-filters",
    (stored) => activeFilterCount > 0 && stored !== "collapsed",
    (open) => (open ? "expanded" : "collapsed"),
    { failed: noIdentityFiltersExpanded, pending: noIdentityFiltersExpanded }
  );
  const filterPanel = useNestedPanels<"labels">(filtersExpanded);
  // Whether the panel stands open because the reader pressed it open. A panel the stored choice
  // opens - as the Board mounts, or as the signed-in identity resolves - moves no focus into
  // itself, so the focus starts where the page starts and the reader is not put in a panel they
  // did not open.
  const [pressedOpen, setPressedOpen] = useState(false);
  /** The reader's own open or close of the panel - the trigger's press either way, Escape, Tab
   *  past either end, a press outside it that closes it - which is the one thing that writes the
   *  stored choice, and only when it changes what the panel shows. A press while the panel is
   *  shut, a chip's removal while it is shut or from the keyboard beside a panel the stored
   *  choice opened, and the choice read again on a new identity write nothing. A pointer press on
   *  a chip while the panel is open is a press outside it - the chips sit in the control row, and
   *  the panel is portalled over the page - so it closes the panel and writes that close, as any
   *  press outside it does. */
  const openFilterPanel = (open: boolean) => {
    setPressedOpen(open);
    if (open !== filtersExpanded) setFiltersExpanded(open);
  };
  /** The control row's client-side filters against this viewer's read state: what the board renders,
   *  what drop indices count, and what the PATCH names as neighbours. */
  const isVisible = useCallback(
    (issue: IssueSummary) => matches(issue, userState.data?.[issue.key]?.last_read_seq ?? 0),
    [matches, userState.data]
  );
  const { error, moveCard, moveCardToLane, question } = useBoardMoves(project, labels, isVisible);
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
  const columns = useMemo(
    () => (issues.data === undefined ? [] : groupIssuesByStatus(issues.data.filter(isVisible))),
    [issues.data, isVisible]
  );
  const boardLanes = useMemo(
    () =>
      !lanes || issues.data === undefined ? [] : groupIssuesByLane(issues.data.filter(isVisible)),
    [lanes, issues.data, isVisible]
  );
  const onDragEnd = ({ active, over }: DragEndEvent) => {
    landCard();
    const current = queryClient.getQueryData<IssueSummary[]>(queryKey);
    if (over === null || current === undefined) {
      return;
    }
    // Indices count the rendered - visible - cards, so a move sends visible neighbours.
    const visible = current.filter(isVisible);
    if (lanes) {
      const target = laneDropTarget(visible, String(active.id), String(over.id));
      if (target !== undefined) {
        void moveCardToLane(String(active.id), target.status, target.lane, target.insertionIndex);
      }
      return;
    }
    const target = dropTarget(visible, String(active.id), String(over.id));
    if (target !== undefined) {
      void moveCard(String(active.id), target.status, target.insertionIndex);
    }
  };
  /** Icebox and Done are rails - no cards to focus, still a move target - while the edges are
   *  hidden. The render below and the roving keys read the same rule. Memoised so a toggle
   *  that does not change `showEdges` leaves `BoardLaneRow`'s memo intact. */
  const isCollapsed = useCallback(
    (status: IssueStatus) => !showEdges && (status === "icebox" || status === "done"),
    [showEdges]
  );

  // One `aria-live` sentence per keyboard move; `focusAfterMove` names the card focus follows
  // once the optimistic list has rendered (a status move remounts the article under its new
  // column, so the old node cannot keep focus; a card moved into a rail has no node, and focus
  // lands on the rail). A move sets it only once it has placed the card (its `onPlaced`), and only
  // while the focus then stands where the move's press left it: on the card the press moved
  // (`from`), or in the rule's question where the question took the focus from that card
  // (`questionTookFocusFrom`). The question's choice places the card, and the question gives the
  // focus back where it took it from as it closes (`useDialog`). Another dialog gives a move no
  // focus, and nor does a question that took the focus from the body or from another control. The
  // move reads this as it places the card, before the board draws the card in its new place and so
  // takes the focus off that card. So a move that ends without placing the card takes no focus
  // later, and nor does a move the rule's dry run checks first, placed once the dry run answers,
  // where the reader has moved the focus on by then - a key to another card, a Tab, a click, a
  // click on a blank spot that leaves the focus on the body too. A move that takes no focus leaves
  // the plan of an earlier move as it is: the effect checks that plan against its own press. The
  // effect runs on the `columns`/`boardLanes` render - the announcement's own render comes first,
  // before the query observer has notified, and would focus the doomed node. It moves the focus
  // only while the focus still stands on the press's card, or on the body where that card's going
  // left it, or where the question's hand-back left it once that card had gone.
  const [announcement, setAnnouncement] = useState("");
  const focusAfterMove = useRef<{
    key: string;
    status: IssueStatus;
    laneId: string | null;
    from: Element | null;
  } | null>(null);
  /** The element the rule's question took the focus from as it opened, while the question is
   *  open, else `null` (`QuestionOpening`). */
  const questionTookFocusFrom = useRef<Element | null>(null);
  const boardNode = (selector: string) => boardRef.current?.querySelector<HTMLElement>(selector);
  /** The `data-board-column` selector for `status`, scoped to `laneId` (`null` outside a
   *  swimlane). */
  const laneColumnSelector = (status: IssueStatus, laneId: string | null): string =>
    laneId === null
      ? `[data-board-column="${status}"]`
      : `[data-board-column="${status}"][data-board-lane="${laneId}"]`;
  // biome-ignore lint/correctness/useExhaustiveDependencies: `columns`/`boardLanes` are the trigger, not a read; `boardNode` reads a ref
  useEffect(() => {
    const pending = focusAfterMove.current;
    if (pending === null) {
      return;
    }
    focusAfterMove.current = null;
    const { from } = pending;
    const stands = document.activeElement === from || (!from?.isConnected && focusOnDocument());
    if (!stands) return;
    (
      boardNode(`[data-board-card="${pending.key}"]`) ??
      boardNode(laneColumnSelector(pending.status, pending.laneId))
    )?.focus();
  }, [columns, boardLanes]);
  /** The columns `j`/`k`/`h`/`l` and `Shift+J/K/H/L` act on: the swimlane holding the current
   *  focus (by its `data-board-lane` key) while lanes are on, the board's flat columns
   *  otherwise. */
  const focusedLaneColumns = (laneId: string | null): BoardColumn[] =>
    lanes && laneId !== null
      ? (boardLanes.find((entry) => laneKey(entry.lane) === laneId)?.columns ?? [])
      : columns;
  /** The focused card or column's lane key; outside lane mode, or with nothing focused yet,
   *  `null`. With lanes on and nothing focused, the first roving key lands in the first band
   *  (P0) rather than the flat board's cross-lane order. */
  const currentLaneId = (): string | null => {
    const explicit = columnAround(document.activeElement)?.dataset.boardLane;
    if (explicit !== undefined) {
      return explicit;
    }
    return lanes ? laneKey(boardLanes[0]?.lane ?? null) : null;
  };
  /** Where keyboard focus sits on the board right now, or `null` when it is elsewhere. */
  const currentFocus = (): BoardFocus | null => {
    const active = document.activeElement;
    const column = columnAround(active);
    if (column === null) {
      return null;
    }
    const status = column.dataset.boardColumn as IssueStatus;
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
    const target = focusTarget(laneColumns, isCollapsed, currentFocus(), key);
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
    if (key === undefined) {
      return;
    }
    const laneId = currentLaneId();
    const laneColumns = focusedLaneColumns(laneId);
    const target = keyboardMove(laneColumns, key, direction);
    if (target === undefined) {
      return;
    }
    // Each move function tells `placed` (its `onPlaced`) once it has placed the card in the cache:
    // in the call itself, before its request goes out, or, for a move the rule's question checks
    // first, after the dry run or a choice of the question. So the announcement reads the
    // optimistic position - the one the user sees - and, after a choice, each other issue it
    // moved is already in its cell, so the count holds them. A move that ends without placing
    // the card (kept, cancelled or refused) says nothing and takes no focus.
    const lane: PriorityLane | undefined = laneId === null ? undefined : laneFromKey(laneId);
    // Where the press left the focus: the card it moves (`focusAfterMove`).
    const from = document.activeElement;
    /** Says where the card stands now - its position and count in the visible cell, the one the
     *  user sees, or nothing while it is not in that cell - and, while the focus stands where the
     *  press left it, has focus follow the card once the board has drawn it there
     *  (`focusAfterMove`). */
    const placed = () => {
      const column = (queryClient.getQueryData<IssueSummary[]>(queryKey) ?? []).filter(
        (issue) =>
          issue.status === target.status &&
          isVisible(issue) &&
          (lane === undefined || issue.priority === lane)
      );
      const position = column.findIndex((issue) => issue.key === key);
      if (position !== -1) {
        setAnnouncement(announceMove(key, target.status, position + 1, column.length));
      }
      // Read now, before the query observer notifies the board: the draw that follows takes the
      // focus off the press's card, so after it a focus on the body cannot tell that draw from a
      // reader's click. A choice of the question places the card while the question is still
      // open, so `questionTookFocusFrom` still holds what the question took the focus from.
      if (document.activeElement === from || questionTookFocusFrom.current === from) {
        focusAfterMove.current = { from, key, laneId, status: target.status };
      }
    };
    if (lanes && lane !== undefined) {
      void moveCardToLane(key, target.status, lane, target.insertionIndex, placed);
    } else {
      void moveCard(key, target.status, target.insertionIndex, placed);
    }
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
    { action: "board.next", run: () => rove("j") },
    { action: "board.previous", run: () => rove("k") },
    { action: "board.column", run: () => rove("l") },
    { action: "board.previous-column", run: () => rove("h") },
    {
      action: "board.arrows-vertical",
      run: (event) => rove(event.key === "ArrowDown" ? "j" : "k"),
      when: focusedBoardNode,
    },
    {
      action: "board.arrows-horizontal",
      run: (event) => rove(event.key === "ArrowRight" ? "l" : "h"),
      when: focusedBoardNode,
    },
    { action: "board.move-down", run: () => move("down"), when: focusedCard },
    { action: "board.move-up", run: () => move("up"), when: focusedCard },
    { action: "board.move-next", run: () => move("next"), when: focusedCard },
    { action: "board.move-previous", run: () => move("prev"), when: focusedCard },
    {
      action: "board.open",
      run: () =>
        cardAround(document.activeElement)?.querySelector<HTMLElement>(ISSUE_LINK)?.click(),
      when: focusedCard,
    },
    {
      // Only from the card itself: Enter on the title link is the browser's own navigation. Out of
      // the palette, where `open` above is already the row for the same action.
      action: "board.open-enter",
      run: () =>
        cardAround(document.activeElement)?.querySelector<HTMLElement>(ISSUE_LINK)?.click(),
      when: () => document.activeElement?.matches(CARD_SELECTOR) === true,
    },
    {
      // R20: `o` in a new tab, from the card itself or a control of it; this page stays.
      action: "board.open-new-tab",
      run: () =>
        openLinkInNewTab(cardAround(document.activeElement)?.querySelector(ISSUE_LINK) ?? null),
      when: focusedCard,
    },
    {
      // A card in Done holds its priority (`aria-disabled`), so the key is offered on the cards
      // whose list would open.
      action: "board.priority",
      run: () =>
        cardAround(document.activeElement)?.querySelector<HTMLElement>(CARD_PRIORITY)?.click(),
      when: () => cardAround(document.activeElement)?.querySelector(CARD_PRIORITY) != null,
    },
    { action: "board.copy-ref", run: () => copyFocusedCard("reference"), when: focusedCard },
    { action: "board.copy-key", run: () => copyFocusedCard("key"), when: focusedCard },
    {
      action: "board.back",
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
  const instructions = dragInstructions(
    [
      useActionKeys("board.move-down")[0],
      useActionKeys("board.move-up")[0],
      useActionKeys("board.move-previous")[0],
      useActionKeys("board.move-next")[0],
    ],
    useActionKeys("global.help")[0]
  );
  // One row holds the Board's search and filtering, above whatever the board itself is showing
  // - its cards, the skeleton or a refusal - so a reader who has narrowed the list keeps the
  // controls that did it. Nothing they can press here makes the header taller:
  // the filter panel is portalled over the page (`AnchoredPanel`), and the chips are the one
  // part of the row that may take a second line.
  const controls = (
    <>
      <ControlRow
        chips={filterChips(activeFilters)}
        className="mb-4"
        keyScope="board"
        search={
          <SearchBox
            className="w-full"
            label="Search issues"
            onChange={filters.setSearch}
            placeholder="Issue key or title"
            value={filters.search}
          />
        }
        trigger={
          <PanelTrigger
            count={activeFilterCount}
            focusOnOpen={pressedOpen}
            label="Filters"
            onOpenChange={openFilterPanel}
            open={filtersExpanded}
            stacked
          >
            {() => (
              <>
                <MultiSelect
                  {...filterPanel("labels")}
                  emptyMessage="No labels yet."
                  label="Labels"
                  onChange={setLabels}
                  options={availableLabels}
                  searchLabel="Search labels"
                  selected={labels}
                />
                <IssueFlagChips />
                <Chip
                  onClick={() => filters.setUnclaimed(!filters.unclaimed)}
                  selected={filters.unclaimed}
                >
                  Unclaimed
                </Chip>
              </>
            )}
          </PanelTrigger>
        }
      />
      <UnclaimedUnchecked reason={filters.unclaimedUnchecked} />
    </>
  );
  // The question belongs to the board's moves (`useBoardMoves`), not to the list: it is drawn
  // whatever the list read shows, so a read that fails while it is open leaves it to be answered,
  // and its answer ends the move that holds every later read of the list. `QuestionOpening` comes
  // first, so it reads what the question takes the focus from (`focusAfterMove`).
  const questionDialog =
    question === undefined ? null : (
      <>
        <QuestionOpening tookFocusFrom={questionTookFocusFrom} />
        <ParentStatusDialog question={question} />
      </>
    );
  const body = issues.isPending ? (
    <LoadingSkeleton label="Loading board" />
  ) : issues.isError ? (
    <p className={textSecondaryOnSurface}>Could not load board.</p>
  ) : (
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
      {issues.data.length === 0 ? (
        // The active query is the label-filtered list, so an empty answer under a label filter
        // means "nothing matches", not "empty project" - the same copy the List shows.
        labels.length === 0 ? (
          <EmptyState label="Empty project board" message="No issues in this project." />
        ) : (
          <EmptyState label="No matching project issues" message="No issues match these filters." />
        )
      ) : (
        <DndContext
          accessibility={{
            screenReaderInstructions: {
              draggable: instructions,
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
            // No Tab stop of its own, as a lane's row has none (`BoardLaneRow`).
            <div
              className={`overflow-x-auto pb-3 ${dragging ? "" : "snap-x snap-mandatory"}`}
              data-testid="board-scroll-container"
              tabIndex={-1}
            >
              <div className="flex w-max items-stretch gap-4">
                {columns.map((column) =>
                  isCollapsed(column.status) ? (
                    <CollapsedColumn column={column} key={column.status} />
                  ) : (
                    <BoardColumnView
                      column={column}
                      key={column.status}
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
  return (
    <>
      {questionDialog}
      {controls}
      {body}
    </>
  );
}
