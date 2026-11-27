import {
  type CollisionDetection,
  closestCorners,
  DndContext,
  type DragEndEvent,
  MouseSensor,
  pointerWithin,
  TouchSensor,
  useDraggable,
  useDroppable,
  useSensor,
  useSensors,
} from "@dnd-kit/core";
import {
  type CSSProperties,
  memo,
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { Link } from "react-router-dom";

import type { StatusChangeAnswer, UserState } from "../../api/types";
import { AttentionBadge } from "../../components/Badge";
import { Chip } from "../../components/Chip";
import { ControlRow } from "../../components/ControlRow";
import { DisplayMenu } from "../../components/DisplayMenu";
import { type FilterChip, filterChips } from "../../components/filter-chips";
import { MultiSelect } from "../../components/MultiSelect";
import { PanelTrigger } from "../../components/PanelTrigger";
import { LabelPill, Pill, StatusPill } from "../../components/Pill";
import { SearchBox } from "../../components/SearchBox";
import { Tooltip, useTooltip } from "../../components/Tooltip";
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
  textMutedOnSurface,
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
import { useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching, focusOnDocument } from "../shell/roving";
import { useUserPreference } from "../shell/userPreference";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import {
  announceMove,
  type BoardFocus,
  focusTarget,
  type KeyColumn,
  keyboardMove,
  type MoveDirection,
  type RoveKey,
} from "./board-keys";
import {
  type BoardBy,
  type BoardCell,
  type BoardChange,
  type BoardColumn,
  type BoardLine,
  type BoardPlace,
  boardLines,
  cardDropId,
  cardIn,
  cellId,
  columnTitle,
  dropPlace,
  keyRefusal,
  laneKey,
  laneLabel,
  type MovePlan,
  type MoveRules,
  planMove,
} from "./board-model";
import { asChanged, othersMoved, useBoardMoves } from "./board-moves";
import { IssueFlagChips, NarrowingList, UnclaimedUnchecked } from "./IssueFlags";
import { useIssueFilters } from "./issue-filters";
import { formatPhaseTime, formatWorkTime, sinceMs } from "./issues-legion";
import { type Item, MEASURE_METHODS, workerName, workHoursOf } from "./issues-table-model";
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

// The sensors' options and the card's drag attributes, made once: `useSensor` keeps its sensor
// while its options are the same object, and a new object on each render would hand dnd-kit new
// activators, whose context then draws every card's drag hooks again.
const MOUSE_OPTIONS = { activationConstraint: { distance: 8 } };
const TOUCH_OPTIONS = { activationConstraint: { delay: 150, tolerance: 5 } };
const CARD_ATTRIBUTES = { role: "article", roleDescription: "card", tabIndex: -1 };
const BOARD_ACCESSIBILITY = {
  screenReaderInstructions: {
    draggable:
      "Focus a card and press Shift with J, K, H or L to move it; press ? for every shortcut.",
  },
};

function cardAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, CARD_SELECTOR);
}

function columnAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, COLUMN_SELECTOR);
}

/** One Legion fact of a card: its name, then its value. A measure's fact says how its value is
 *  taken (`method`, its column's sentence) in the shared tooltip, on hover and on keyboard focus,
 *  as the measure's table header does, so it is a tab stop. */
function Fact({
  hook,
  label,
  method,
  value,
}: {
  hook: string;
  label: string;
  method?: string;
  value: string;
}): ReactNode {
  const anchor = useRef<HTMLSpanElement>(null);
  const tooltip = useTooltip();
  return (
    <span
      {...(method === undefined ? {} : { ...tooltip.props, tabIndex: 0 })}
      className={`inline-flex gap-1 rounded-sm ${textSecondaryOnSurface} ${method === undefined ? "" : boardFocusRing}`}
      data-legion-fact={hook}
      ref={anchor}
    >
      <span className={textMutedOnSurface}>{label}</span>
      <span>{value}</span>
      {method === undefined ? null : (
        <Tooltip anchorRef={anchor} trigger={tooltip}>
          {method}
        </Tooltip>
      )}
    </span>
  );
}

/** A card's open asks: the count, which says what it counts in the shared tooltip, on hover and
 *  on keyboard focus, so it is a tab stop. It carries no native title. */
function OpenAsks({ count }: { count: number }): ReactNode {
  const anchor = useRef<HTMLSpanElement>(null);
  const tooltip = useTooltip();
  const noun = count === 1 ? "ask" : "asks";
  return (
    <span
      {...tooltip.props}
      className={`inline-flex rounded-sm ${boardFocusRing}`}
      data-open-asks={count}
      ref={anchor}
      // biome-ignore lint/a11y/noNoninteractiveTabindex: the tab stop opens the tooltip that says what the count is.
      tabIndex={0}
    >
      <AttentionBadge count={count} />
      <span className="sr-only"> open {noun}</span>
      <Tooltip anchorRef={anchor} trigger={tooltip}>
        {count} open {noun}
      </Tooltip>
    </span>
  );
}

/** The Time in Legion phase a card draws at its row's clock (`Item.at`), the clock its Work time
 *  is closed at (`workTimeOf`), or undefined for a card with none. Those two are the texts on a
 *  card that count from that clock. A claim's age keeps itself current through `Timestamp`'s own
 *  tick, and a wait's since is a date. */
function phaseTimeOf(item: Item): string | undefined {
  const since = item.card?.card.phase_since ?? null;
  return sinceMs(since) === undefined ? undefined : formatPhaseTime(since, item.at);
}

/** The Work time a card draws, its facts closed at its row's clock (`boardFactsAt`), or undefined
 *  for a card with none: one with no Legion card, or whose facts hold no measured work. */
function workTimeOf(item: Item): string | undefined {
  const hours = item.card === undefined ? undefined : workHoursOf(item);
  return hours === undefined ? undefined : formatWorkTime(hours);
}

/** What a card on the Legion board shows beside today's card, from the board route, each fact
 *  left out where the board gives it no value, never drawn as 0 or a dash. */
function LegionFacts({
  item,
  phaseTime,
  workTime,
}: {
  item: Item;
  phaseTime?: string;
  workTime?: string;
}): ReactNode {
  const legion = item.card?.card;
  if (legion === undefined) return null;
  // Workers are the roles in a turn: a card whose issue has none running leaves the fact out
  // rather than say None, which every queued card would carry.
  const roles = (legion.roles ?? []).map(workerName);
  const own = legion.blocked_by ?? [];
  const blockedBy = own.length > 0 ? own : (legion.waiting?.blocked_by ?? []);
  const waitingOn = legion.waiting_on;
  return (
    <>
      {phaseTime === undefined && workTime === undefined && roles.length === 0 ? null : (
        <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
          {phaseTime === undefined ? null : (
            <Fact
              hook="phase-time"
              label="Time in Legion phase"
              method={MEASURE_METHODS.time_in_phase}
              value={phaseTime}
            />
          )}
          {workTime === undefined ? null : (
            <Fact
              hook="work-time"
              label="Work time"
              method={MEASURE_METHODS.work_hours}
              value={workTime}
            />
          )}
          {roles.length === 0 ? null : (
            <Fact hook="roles" label="Workers" value={roles.join(", ")} />
          )}
        </div>
      )}
      {blockedBy.length === 0 ? null : (
        <p
          className={`mt-2 border-t pt-2 text-xs ${borderDefault} ${textSecondaryOnSurface}`}
          data-legion-fact="blocked-by"
        >
          Blocked by {blockedBy.join(", ")}
        </p>
      )}
      {waitingOn === null ? null : (
        <p
          className={`mt-2 border-t pt-2 text-xs ${borderDefault} ${textSecondaryOnSurface}`}
          data-legion-fact="waiting-on"
        >
          Waiting on {waitingOn.kind}
          {waitingOn.since === null ? "" : ` since ${new Date(waitingOn.since).toLocaleString()}`}
          {waitingOn.answered_at === null ? "" : ", answered"}
        </p>
      )}
      {legion.children === 0 ? null : (
        <p className={`mt-2 text-xs ${textMutedOnSurface}`} data-legion-fact="children">
          {legion.children} {legion.children === 1 ? "issue" : "issues"} of this tree not started
        </p>
      )}
    </>
  );
}

/** What a card is drawn from. A card draws again only when one of these changes (`sameCard`):
 *  the issue, its Legion card and its Place keep their objects or values across reads that did
 *  not change them (the listings pair a read's issues with the cached ones by key,
 *  `api/queries.ts`), and its times are the texts it draws, not the clock or its facts, which
 *  each build closes at its own clock (`boardFactsAt`) in a new object. So a build of the rows
 *  draws again only the card of an issue a live change moved, and a card whose Time in Legion
 *  phase or Work time reads differently at the new clock. */
interface CardProps {
  readonly item: Item;
  /** The Time in Legion phase the card draws (`phaseTimeOf`); undefined where it draws none. */
  readonly phaseTime?: string;
  /** The cell the card stands in, by its drop id (`cellId`). */
  readonly place: string;
  readonly unread: boolean;
  /** The Work time the card draws (`workTimeOf`); undefined where it draws none. */
  readonly workTime?: string;
}

const sameCard = (a: CardProps, b: CardProps): boolean =>
  a.item.issue === b.item.issue &&
  a.item.place === b.item.place &&
  a.item.card?.card === b.item.card?.card &&
  a.phaseTime === b.phaseTime &&
  a.workTime === b.workTime &&
  a.place === b.place &&
  a.unread === b.unread;

/**
 * One card. The whole card is the drag activator: press and move 8 px (mouse) or hold 150 ms
 * (touch) anywhere on it to lift it, so a plain click on the title still follows the link and a
 * vertical swipe still scrolls. Mouse and touch are separate sensors on purpose: a pointer
 * sensor would also see a finger's first pointermove and lift the card for an instant before the
 * browser's pan cancels it. The keyboard never lifts a card: the Board's registry rows
 * (`j`/`k`/`h`/`l` rove focus, `Shift+J/K/H/L` move it by the Board's rules) are the accessible
 * path, so the card is `tabIndex={-1}` - reached by those keys, not by Tab - and
 * `data-board-card` names it for them. dnd-kit's default drag attributes would make the card a
 * `button`; it stays an `article` so nothing interactive is nested in a control. The card is a
 * drop target too, so a drop onto it lands at its place. Its drag hooks draw it again whenever
 * the drag context changes, so what it shows is a body of its own (`CardBody`), drawn again only
 * when its data changes.
 */
const BoardCard = memo(function BoardCard({
  item,
  phaseTime,
  place,
  unread,
  workTime,
}: CardProps): ReactNode {
  const { issue } = item;
  const { attributes, isDragging, listeners, setNodeRef, transform } = useDraggable({
    attributes: CARD_ATTRIBUTES,
    id: issue.key,
  });
  const drop = useDroppable({ id: cardDropId(issue.key) });
  const setDropRef = drop.setNodeRef;
  const setCardRef = useCallback(
    (node: HTMLElement | null) => {
      setNodeRef(node);
      setDropRef(node);
    },
    [setDropRef, setNodeRef]
  );
  const style: CSSProperties = {
    transform:
      transform === null ? undefined : `translate3d(${transform.x}px, ${transform.y}px, 0)`,
  };
  return (
    <article
      aria-label={`${issue.key} ${issue.title}`}
      className={`cursor-grab rounded-xl border p-3 ${card} ${cardHoverBorder} ${boardFocusRing} ${
        isDragging ? "relative z-50 opacity-50" : ""
      } ${drop.isOver && !isDragging ? selectedCardBorder : ""}`}
      data-board-card={issue.key}
      data-board-place={place}
      ref={setCardRef}
      style={style}
      {...attributes}
      {...listeners}
    >
      <CardBody
        item={item}
        phaseTime={phaseTime}
        place={place}
        unread={unread}
        workTime={workTime}
      />
    </article>
  );
}, sameCard);

/** What a card shows inside its drag handle: today's card (key, title, unread dot, priority,
 *  claim, progress, route reach, labels, open asks), its Place in the Legion queue beside its key
 *  where it has one (the Queue column's number), and, for an issue with a card on the Legion
 *  board, its Legion facts. It draws no slot number. */
const CardBody = memo(function CardBody({
  item,
  phaseTime,
  unread,
  workTime,
}: CardProps): ReactNode {
  const { issue } = item;
  return (
    <>
      {/* Below `xl` every anchor is `inline-flex` with centred items (the 44 px touch rule in
          styles.css), so the key and title are stacked and start-aligned explicitly. */}
      <Link
        className={`flex flex-col text-sm ${linkText} ${linkHoverText}`}
        data-issue-link
        to={buildIssuePath({ key: issue.key, kind: "issue" })}
        {...referenceTriggerProps({ key: issue.key, kind: "issue" })}
      >
        <span className="inline-flex items-center gap-1.5 self-start font-semibold">
          {issue.key}
          {item.place === undefined ? null : (
            <span className={`font-normal ${textMutedOnSurface}`} data-queue-place={item.place}>
              #{item.place}
            </span>
          )}
        </span>
        <span className={`mt-1 self-start ${textPrimaryOnSurface}`}>{issue.title}</span>
      </Link>
      <div className="mt-3 flex flex-wrap items-center gap-2">
        {unread ? <UnreadDot /> : null}
        <PriorityControl
          disabled={issue.status === "done"}
          issueKey={issue.key}
          priority={issuePriority(issue.priority)}
        />
        {/* A card is too narrow for the holder's name on one line: the chip writes it on as
            many lines as it needs, with the age and the state under it. */}
        <ClaimChip claim={issue.claim} stacked />
        <ProgressChips progress={issue.progress} />
        <UnreachableRouteMarker issue={issue} />
        {(issue.labels ?? []).map((label) => (
          <LabelPill key={label}>{label}</LabelPill>
        ))}
        {issue.open_asks === 0 ? null : <OpenAsks count={issue.open_asks} />}
      </div>
      <LegionFacts item={item} phaseTime={phaseTime} workTime={workTime} />
    </>
  );
}, sameCard);

/**
 * One column: its title, its count and its cards. The whole column is its cell's drop target
 * (`cellId`), and every column stretches to the Board's height, so a card can be dropped anywhere
 * in a column's body - below its last card, or into an empty column whose header has scrolled out
 * of view on a phone - not only on the header.
 */
function BoardColumnView({
  by,
  cell,
  userState,
}: {
  by: BoardBy;
  cell: BoardCell;
  userState: UserState | undefined;
}): ReactNode {
  const place = cellId(cell.column.id, cell.lane);
  const { isOver, setNodeRef } = useDroppable({ id: place });
  const title = cell.column.title;
  return (
    <section
      aria-label={title}
      className={`w-72 shrink-0 snap-start ${boardFocusRing}`}
      data-board-column={cell.column.id}
      data-board-lane={cell.lane === undefined ? undefined : laneKey(cell.lane)}
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
          className={`flex items-center justify-between gap-2 text-sm font-semibold ${textPrimaryOnSurface}`}
        >
          {by === "status" ? <StatusPill>{title}</StatusPill> : <span>{title}</span>}
          <Pill>{cell.items.length}</Pill>
        </div>
      </header>
      {cell.items.length === 0 ? (
        // An empty column keeps its width and its drop area in sight with a dashed body.
        <div
          className={`mt-3 min-h-40 rounded-xl border border-dashed px-3 py-6 text-center text-xs ${textMutedOnCanvas} ${
            isOver ? selectedCardBorder : borderStrong
          }`}
        >
          Nothing here
        </div>
      ) : (
        <div className="mt-3 space-y-3">
          {cell.items.map((item) => (
            <BoardCard
              item={item}
              key={item.issue.key}
              phaseTime={phaseTimeOf(item)}
              place={place}
              unread={issueIsUnread(item.issue, userState?.[item.issue.key]?.last_read_seq ?? 0)}
              workTime={workTimeOf(item)}
            />
          ))}
        </div>
      )}
    </section>
  );
}

/**
 * A rail: a narrow column with its name and, but on the Done rail, its count, and no card. Icebox
 * is one until Show Icebox is on, Done while the view lists no done issue, and None on a Board by
 * Legion phase. It is its cell's drop target as a column is, and spans the Board's height: a card
 * dropped on the Done rail closes its issue. Like a column it is a `tabIndex={-1}` focus target
 * for the Board's `h`/`l` keys, and a card moved into it by `Shift+H/L` hands the focus to it.
 */
function Rail({ cell }: { cell: BoardCell }): ReactNode {
  const { isOver, setNodeRef } = useDroppable({ id: cellId(cell.column.id, cell.lane) });
  const title = cell.column.title;
  return (
    <section
      aria-label={`${title} (collapsed)`}
      className={`w-10 shrink-0 snap-start ${boardFocusRing}`}
      data-board-column={cell.column.id}
      data-board-lane={cell.lane === undefined ? undefined : laneKey(cell.lane)}
      ref={setNodeRef}
      tabIndex={-1}
    >
      <div
        className={`flex h-full min-h-[52px] flex-col items-center gap-3 rounded-xl border px-1 py-3 ${borderDefault} ${surfaceMutedBg} ${
          isOver ? selectedCardBorder : ""
        }`}
        data-testid="board-column-rail"
      >
        {/* The Done rail stands for done issues the view does not list, so it has no count. */}
        {cell.column.id === "done" ? null : <Pill>{cell.items.length}</Pill>}
        <span
          className={`text-sm font-semibold tracking-wide ${textPrimaryOnSurface}`}
          style={{ writingMode: "vertical-rl" }}
        >
          {title}
        </span>
      </div>
    </section>
  );
}

/** One line of the Board's columns, in a box that scrolls sideways. While a card is in flight
 *  the box does not snap: dnd-kit scrolls the Board when the pointer nears an edge, and with
 *  mandatory snapping every scroll tick jumps a whole column. `relative` keeps a card's `sr-only`
 *  text, which is placed absolutely, inside the box rather than widening the page. The box turns
 *  the browser's own scroll anchoring off (`overflow-anchor: none`): the Board's anchor keeps the
 *  reader's card in place by scrolling the window, and the browser's would move it a second time.
 *  The row of columns cuts what runs past its ends sideways (`overflow-x: clip`): a card in flight
 *  follows the pointer by a transform, which takes in each step the box scrolls, so a card held
 *  at the Board's last column or rail would widen the box by each step dnd-kit scrolls it, and
 *  that scroll would never reach an end. */
function BoardLineView({
  by,
  dragging,
  line,
  testId,
  userState,
}: {
  by: BoardBy;
  dragging: boolean;
  line: BoardLine;
  testId: string;
  userState: UserState | undefined;
}): ReactNode {
  return (
    <div
      className={`relative overflow-x-auto pb-3 [overflow-anchor:none] ${dragging ? "" : "snap-x snap-mandatory"}`}
      data-testid={testId}
    >
      <div className="flex w-max items-stretch gap-4 overflow-x-clip">
        {line.cells.map((cell) =>
          cell.column.rail ? (
            <Rail cell={cell} key={cell.column.id} />
          ) : (
            <BoardColumnView by={by} cell={cell} key={cell.column.id} userState={userState} />
          )
        )}
      </div>
    </div>
  );
}

/**
 * The control row above the Board: the view's search, the Filters panel with the Labels picker
 * (which writes `label=`, as the table's Labels column filter does) and the three flags, a chip
 * for each narrowing that differs from the view's own (the page's `chips`, since the Board has no
 * header to show a column's filter), Clear filters while there is one, the page's own controls
 * and, in a project with a Legion daemon, the Group by menu that picks the Board's columns.
 *
 * The saved preference `project.issue-filters` is the only owner of the panel's open state: the
 * row reads it when it mounts and when the signed-in identity resolves, never because the count
 * moved. Arriving from another page, tab or project mounts a new row, so that read covers the
 * narrowings the route brings. On a row that is up the count moves when the reader narrows the
 * view, and when Back or Forward crosses entries of the Issues tab. A re-read would close the
 * panel the moment the last narrowing goes (clearing the search box), taking the control being
 * typed in with it, or open or close it against the reader's choice on the others, while its
 * trigger and chips already show every narrowing. So it stays as they left it until they close
 * it or leave the project or its Issues tab.
 */
export function BoardControlRow({
  chips,
  controls,
  groupBy,
  labelOptions,
  narrowings,
  onClear,
  onSearch,
  search,
  selection,
}: {
  /** Every narrowing of the view that differs from the view's own, the chip of the issues a
   *  link names aside, which the trigger does not count. */
  chips: readonly FilterChip[];
  /** The page's own controls: the layout switch, the Board's toggles and the view's controls. */
  controls: ReactNode;
  /** The Board's grouping and the way to change it, in a project with a Legion daemon; unset,
   *  no Group by menu draws, since Status would be its one choice. */
  groupBy?: { readonly value: BoardBy; readonly onChange: (by: BoardBy) => void };
  /** The labels the Labels picker offers. */
  labelOptions: readonly string[];
  /** The narrowings among `chips` that are no column and no flag (`exclude_tree`), which the
   *  Filters panel lists too, each with its remove. */
  narrowings: readonly FilterChip[];
  onClear: (() => void) | undefined;
  onSearch: (search: string) => void;
  /** The view's search. */
  search: string;
  /** The chip of the issues a link names (`only=`), drawn first. */
  selection?: FilterChip;
}): ReactNode {
  const filters = useIssueFilters();
  const { labels, setLabels } = filters;
  const count = chips.length;
  const noIdentityFiltersExpanded = labels.length > 0;
  const [filtersExpanded, setFiltersExpanded] = useUserPreference(
    "project.issue-filters",
    (stored) => count > 0 && stored !== "collapsed",
    (open) => (open ? "expanded" : "collapsed"),
    { failed: noIdentityFiltersExpanded, pending: noIdentityFiltersExpanded }
  );
  const filterPanel = useNestedPanels<"labels">(filtersExpanded);
  // Whether the panel stands open because the reader pressed it open. A panel the stored choice
  // opens - as the row mounts, or as the signed-in identity resolves - moves no focus into
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
  const [groupOpen, setGroupOpen] = useState(false);
  return (
    <>
      <ControlRow
        chips={filterChips(selection === undefined ? chips : [selection, ...chips])}
        className="mb-4"
        display={
          groupBy === undefined ? undefined : (
            <DisplayMenu
              label="Group by"
              onOpenChange={setGroupOpen}
              open={groupOpen}
              sections={[
                {
                  id: "group",
                  label: "Group by",
                  onChange: (value) =>
                    groupBy.onChange(value === "legion_phase" ? "legion_phase" : "status"),
                  options: [
                    { label: "Status", value: "status" },
                    { label: "Legion phase", value: "legion_phase" },
                  ],
                  value: groupBy.value,
                },
              ]}
            />
          )
        }
        onClear={onClear}
        search={
          <SearchBox
            className="w-full"
            label="Search issues"
            onChange={onSearch}
            placeholder="Search issues: key, title, assignee, author, labels"
            value={search}
          />
        }
        trigger={
          <PanelTrigger
            count={count}
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
                  options={labelOptions}
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
                <NarrowingList narrowings={narrowings} />
              </>
            )}
          </PanelTrigger>
        }
      >
        {controls}
      </ControlRow>
      <UnclaimedUnchecked reason={filters.unclaimedUnchecked} />
    </>
  );
}

/** The card the focus follows after a key move, until the move is drawn and settled
 *  (`IssueBoard`'s `afterMove`). */
interface MoveFollow {
  readonly key: string;
  readonly column: string;
  /** The swimlane's `data-board-lane` key, or `null` outside a swimlane. */
  readonly laneId: string | null;
  /** The live region still has to read where the view's sort put the card. */
  announce: boolean;
  /** The element the focus stood on in the card the press moved, or the one the rule's question
   *  took it from, while the focus is to follow the card; `undefined` where it is not. */
  from: Element | undefined;
  /** The node the follow focused last; `undefined` until the move is drawn. */
  focused: HTMLElement | null | undefined;
  /** The move's write and the listings' reads after it are done. */
  settled: boolean;
}

/** Where the rank puts a card in its cell once a move has placed it, or undefined where the view's
 *  sort does (`MovePlan`). */
type MovePosition = Extract<MovePlan, { kind: "move" }>["position"];

/**
 * Draws nothing. While the rule's question is open, `tookFocusFrom` holds the element the question
 * took the focus from as it opened, which the question gives the focus back to as it closes
 * (`useDialog`). Once the question has closed, it holds `null`. The Board draws this just before
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

/**
 * The Board: a layout of the current view. It draws every row the view lists (`rows`, in the
 * view's sort, so each column's cards are in that sort), in columns by status, or, in a project
 * with a Legion daemon, by Legion phase, as Group by picks (`boardColumns`), in one line or in a
 * lane for each priority. It draws no bar: each column carries its count.
 *
 * A drop or a key move follows the Board's rules (`planMove`): by Status a move into another
 * column sets that status and into another lane that priority, and a move within a column writes
 * the rank only where the rank decides where the card lands, which is while the view sorts by
 * Place, first to last (`rankRefusal`); by Legion phase the daemon sets each card's column, and
 * the one move taken is one within Queue. A refused move says why over the Board and in its live
 * region, where every key move is read out too, with the place the card lands. A move of the
 * status of a card with a parent or an open child asks Dispatch's rule first, and the Board draws
 * the rule's question where the rule would refuse it (`useBoardMoves`).
 *
 * A live change to one issue draws that issue's card alone: the cards are memoised on what they
 * draw (`sameCard`), the sensors' options are made once, and a card compares the time texts it
 * draws, its Time in Legion phase and its Work time, which count to its row's clock (`Item.at`).
 *
 * The reader keeps their place, as in a table (R11): the card the reader's hand is on (focus,
 * then the pointer), else, once they have scrolled, the card at the middle of the Board, and at
 * the top the first card of the column nearest the middle (`holdAtTop`), keeps its top while
 * cards are added, removed or change height above it in its column, or a notice appears above
 * the Board. A card is the anchor's row and its column or lane (`data-board-place`) its group, so
 * a card that leaves its column is not followed. The page scrolls the Board, so the anchor keeps
 * that top by scrolling the window. It is off while a card is dragged, until the drop is drawn,
 * and for a change of the reader's own view (`viewKey`).
 */
export function IssueBoard({
  by,
  columns,
  lanes,
  project,
  rankRefusal,
  rows,
  userState,
  viewKey,
}: {
  by: BoardBy;
  /** The Board's columns, in its order (`boardColumns`). */
  columns: readonly BoardColumn[];
  /** Group the Board into swimlanes by priority, one band per priority and one for none. */
  lanes: boolean;
  project: string;
  /** Why the view's sort lets no rank decide where a card lands within a column (`rankRefusal` in
   *  `board-model.ts`); undefined while the view sorts by Place, first to last. */
  rankRefusal: string | undefined;
  /** The view's rows, in the view's sort. */
  rows: readonly Item[];
  userState: UserState | undefined;
  /** Names the view the reader picked: its address, its lanes and its Icebox, and whether its
   *  first draw is whole (`viewKey` of `useTabReads`, `issues-tab-reads.ts`). A change of it is
   *  the reader's own move, which the anchor leaves alone, as a table does (`useDataTable`). */
  viewKey: string;
}): ReactNode {
  const { error, move, question } = useBoardMoves(project);
  const sensors = useSensors(
    useSensor(MouseSensor, MOUSE_OPTIONS),
    useSensor(TouchSensor, TOUCH_OPTIONS)
  );
  const lines = boardLines(rows, columns, by, lanes);
  const rules: MoveRules = { by, lanes, rankRefusal };
  // While a card is in flight the scroller must not snap (`BoardLineView`), so snapping is
  // switched off from lift to drop.
  const [dragging, setDragging] = useState(false);
  // While a card is dragged, and until the drop's own changes are drawn, the anchor leaves the
  // page alone: the dragged card moves under the reader's hand by a transform, and a commit then
  // would read that move as content shifting around it. Released two frames after the drop, when
  // the drop's write to the listing, which the query cache hands its readers after the event, has
  // been drawn.
  const inFlight = useRef(false);
  // The view the last commit drew: while it is the one the reader is in, a change of the cards is
  // someone else's and the anchor holds the reader's place; a new view is the reader's own move.
  const drawnView = useRef(viewKey);
  useLayoutEffect(() => {
    drawnView.current = viewKey;
  }, [viewKey]);
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
  // A refused move's text, over the Board, until the next move.
  const [refusal, setRefusal] = useState<string>();
  // One `aria-live` sentence per key move and per refusal. Each set takes a new count, which keys
  // the region's text anew, so the same sentence twice in a row is read out twice.
  const [announcement, setAnnouncementState] = useState({ count: 0, text: "" });
  const setAnnouncement = (text: string) =>
    setAnnouncementState((current) => ({ count: current.count + 1, text }));
  // The title link is both the hover-card trigger and part of the whole-card drag activator, and
  // the click that would dismiss an open card after the drop is the one swallowed above - so a
  // lift closes the card (open or still pending) itself. The lift marks the gesture's click as
  // well as the drop does: dnd-kit builds its drop event from a lift React has committed, so a
  // flick fast enough to release before that commit - which a stalled main thread makes of any
  // flick - ends with no drop event at all, and the mark has to be there already.
  const liftCard = () => {
    clickBelongsToDrop.current = true;
    inFlight.current = true;
    closeRefPreview();
    setRefusal(undefined);
    setDragging(true);
  };
  const landCard = () => {
    clickBelongsToDrop.current = true;
    setDragging(false);
    requestAnimationFrame(() =>
      requestAnimationFrame(() => {
        inFlight.current = false;
      })
    );
  };
  /** The Board as the last draw has it. A move the rule's question holds is planned again after
   *  a confirmed status change (`take`), and reads the rows as they stand then. */
  const latest = useRef({ columns, rows, rules });
  useLayoutEffect(() => {
    latest.current = { columns, rows, rules };
  });
  /** A refused move says why, over the Board and in its live region. */
  const refuse = (text: string) => {
    setRefusal(text);
    setAnnouncement(text);
  };
  /** Takes a planned move to `to`: a refusal says why, and a move is written. `placed` is told
   *  each time the move places its card, with the place the rank gives it in its cell, or
   *  undefined where the view's sort puts it. A move the Board's rules refuse once it is planned
   *  again, before it has placed or written anything, says why too. A move answers once its write
   *  and the listings' reads after it are done. */
  const take = (
    plan: MovePlan | undefined,
    to: BoardPlace,
    placed?: (position: MovePosition) => void
  ): Promise<void> | undefined => {
    if (plan === undefined) return;
    if (plan.kind === "refused") {
      refuse(plan.text);
      return;
    }
    setRefusal(undefined);
    const { key } = plan;
    let { position } = plan;
    // A status change that moves other issues with the card - its open children - moves them into
    // cells of the Board, so the move is planned again with those issues there: the rank then
    // names a neighbour Dispatch holds, and the place the card lands counts them.
    const planAgain = (
      answer: StatusChangeAnswer | null
    ): BoardChange | { readonly refused: string } | undefined => {
      const now = latest.current;
      const others = othersMoved(answer, key);
      const items =
        others.size === 0
          ? now.rows
          : now.rows.map((item) => {
              const entry = others.get(item.issue.key);
              return entry === undefined ? item : { ...item, issue: asChanged(item.issue, entry) };
            });
      const again = planMove(
        boardLines(items, now.columns, now.rules.by, now.rules.lanes),
        now.rules,
        key,
        to
      );
      if (again === undefined) return undefined;
      if (again.kind === "refused") return { refused: again.text };
      position = again.position;
      return again.change;
    };
    return move(key, planAgain, placed === undefined ? undefined : () => placed(position), refuse);
  };
  const onDragEnd = ({ active, over }: DragEndEvent) => {
    landCard();
    if (over === null) return;
    const key = String(active.id);
    const place = dropPlace(lines, key, String(over.id));
    if (place !== undefined) void take(planMove(lines, rules, key, place), place);
  };

  // The card the focus follows once a key move has been drawn: a move into another column draws
  // the card again under it, so the old node cannot keep the focus, and a card moved into a rail,
  // or out of the view, has no node, so the focus lands on the column or rail. A move sets it
  // only once it has placed the card (its `onPlaced`), and has the focus follow the card only
  // while the focus then stands in the card the press moved (`pressed`): on the card itself, or
  // on a control inside it, such as the card's link that a Tab reaches while the move's dry run is
  // out. The move draws the card in its new column, which takes that control away with the card,
  // so the focus goes with the card. A move has it follow too where the rule's question took the
  // focus from that card or from a control inside it (`questionTookFocusFrom`). The question's
  // choice places the card, and the question gives the focus back where it took it from as it
  // closes (`useDialog`). The follow's `from` is the element the focus stood on in that card, or
  // the one the question took it from. Another dialog gives a move no focus, and nor does a
  // question that took the focus from the body or from another control. The move reads this as it
  // places the card, before the Board draws the card in its new place and so takes the focus off
  // that card. So a move that ends without placing the card takes no focus later, and nor does a
  // move the rule's dry run checks first, placed once the dry run answers, where the reader has
  // moved the focus out of the card by then - a key to another card, a Tab out of the card, a
  // click, a click on a blank spot that leaves the focus on the body too. The effect moves the
  // focus only while the focus still stands on the follow's `from`, or on the body where that
  // element's going with the card left it, or where the question's hand-back left it once that
  // element had gone. Where the view's sort puts the card (`announce`), the live region reads its
  // place once the move is drawn. The follow holds until the write and the reads after it are
  // done (`settled`). A view that lists done issues reads its open and its done issues apart, so
  // a card moved out of Done can leave the page with one read and come back with the next. While
  // the follow holds, a card drawn again takes back the focus that its old node left on the
  // page's body, or that the follow put on its column while the card was not drawn.
  const afterMove = useRef<MoveFollow | null>(null);
  /** The element the rule's question took the focus from as it opened, while the question is
   *  open, else `null` (`QuestionOpening`). */
  const questionTookFocusFrom = useRef<Element | null>(null);
  const boardNode = (selector: string) => boardRef.current?.querySelector<HTMLElement>(selector);
  /** The `data-board-column` selector for `column`, scoped to `laneId` (`null` outside a
   *  swimlane). */
  const columnSelector = (column: string, laneId: string | null): string =>
    laneId === null
      ? `[data-board-column="${column}"]`
      : `[data-board-column="${column}"][data-board-lane="${laneId}"]`;
  // After every render, since the render that draws a move is one the listing's write starts.
  useEffect(() => {
    const pending = afterMove.current;
    if (pending === null) return;
    const found = cardIn(
      lines.flatMap((line) => line.cells),
      pending.key
    );
    const cell = found?.cell;
    // Not drawn yet: the card still stands in the column it left.
    if (cell !== undefined && cell.column.id !== pending.column) {
      if (pending.settled) afterMove.current = null;
      return;
    }
    if (pending.announce) {
      pending.announce = false;
      const index = found?.index ?? -1;
      setAnnouncement(
        announceMove(
          pending.key,
          pending.column,
          columnTitle(columns, pending.column),
          cell === undefined ? undefined : { at: index + 1, of: cell.items.length }
        )
      );
    }
    const { from } = pending;
    if (from !== undefined) {
      const target =
        boardNode(`[data-board-card="${pending.key}"]`) ??
        boardNode(columnSelector(pending.column, pending.laneId)) ??
        null;
      const active = document.activeElement;
      if (pending.focused === undefined) {
        // The first draw: the focus goes with the card only while it still stands where the
        // press left it, or on the body where that element's going with the card left it.
        if (active === from || (!from.isConnected && focusOnDocument())) {
          target?.focus();
          pending.focused = target;
        } else {
          pending.from = undefined;
        }
      } else if (
        target !== pending.focused &&
        (active === null || active === document.body || active === pending.focused)
      ) {
        target?.focus();
        pending.focused = target;
      }
    }
    if (pending.settled) afterMove.current = null;
  });
  /** The line the keys act on: the swimlane holding the focus (by its `data-board-lane` key)
   *  while lanes are on, the Board's one line otherwise. */
  const lineOf = (laneId: string | null): BoardLine | undefined =>
    lanes && laneId !== null
      ? lines.find((line) => line.lane !== undefined && laneKey(line.lane) === laneId)
      : lines[0];
  /** The focused card or column's lane key; outside lane mode `null`. With lanes on and nothing
   *  focused, the first band (P0), so the first roving key lands there rather than in the
   *  Board's order across lanes. */
  const currentLaneId = (): string | null => {
    if (!lanes) return null;
    const lane = lines[0]?.lane;
    return (
      columnAround(document.activeElement)?.dataset.boardLane ??
      (lane === undefined ? null : laneKey(lane))
    );
  };
  const keyColumns = (line: BoardLine | undefined): KeyColumn[] =>
    (line?.cells ?? []).map((cell) => ({
      id: cell.column.id,
      rail: cell.column.rail,
      size: cell.items.length,
    }));
  /** Where keyboard focus sits on the Board right now, or `null` when it is elsewhere. */
  const currentFocus = (line: BoardLine | undefined): BoardFocus | null => {
    const active = document.activeElement;
    const column = columnAround(active)?.dataset.boardColumn;
    if (column === undefined) return null;
    const key = cardAround(active)?.dataset.boardCard;
    const index =
      key === undefined
        ? -1
        : (line?.cells
            .find((cell) => cell.column.id === column)
            ?.items.findIndex((item) => item.issue.key === key) ?? -1);
    return { column, index: index === -1 ? null : index };
  };
  const rove = (key: RoveKey) => {
    const laneId = currentLaneId();
    const line = lineOf(laneId);
    const target = focusTarget(keyColumns(line), currentFocus(line), key);
    if (target === undefined) return;
    const item =
      target.index === null
        ? undefined
        : line?.cells.find((cell) => cell.column.id === target.column)?.items[target.index];
    (item === undefined
      ? boardNode(columnSelector(target.column, laneId))
      : boardNode(`[data-board-card="${item.issue.key}"]`)
    )?.focus();
  };
  const moveFocused = (direction: MoveDirection) => {
    // The card the press moves: the focus stands on it, or on a control inside it.
    const pressed = cardAround(document.activeElement);
    const key = pressed?.dataset.boardCard;
    if (pressed === null || key === undefined) return;
    const laneId = currentLaneId();
    const line = lineOf(laneId);
    const found = line === undefined ? undefined : cardIn(line.cells, key);
    if (line === undefined || found === undefined) return;
    const { cell, index } = found;
    // The refusal first, then the Board's edge: a key the rules refuse says so at the column's
    // end too.
    const refused = keyRefusal(
      rules,
      cell.column.id,
      direction === "down" || direction === "up" ? "within" : "across"
    );
    if (refused !== undefined) {
      refuse(refused);
      return;
    }
    const target = keyboardMove(keyColumns(line), { column: cell.column.id, index }, direction);
    if (target === undefined) return;
    const to: BoardPlace = { column: target.column, lane: cell.lane, index: target.index };
    // The move tells `placed` once it has placed the card in the listings: in the call itself,
    // before its request goes out, or, for a move the rule's question checks first, after the dry
    // run or a choice of the question. So where the rank decides the place the card lands, the
    // live region reads it at once, from the listings the move has just written - after a choice,
    // with each other issue it moved already in its cell, so the count holds them - and elsewhere
    // the view's sort puts the card, which the drawn move tells (`announce`). A move that ends
    // without placing the card (kept, cancelled or refused, or none once planned again after the
    // dry run) says no place and takes no focus.
    let follow: MoveFollow | undefined;
    const moved = take(planMove(lines, rules, key, to), to, (position) => {
      if (position !== undefined)
        setAnnouncement(
          announceMove(key, target.column, columnTitle(columns, target.column), position)
        );
      // Read now, before the Board draws the move: the draw that follows takes the focus off the
      // press's card, so after it a focus on the body cannot tell that draw from a reader's
      // click. A choice of the question places the card while the question is still open, so
      // `questionTookFocusFrom` still holds what the question took the focus from.
      const active = document.activeElement;
      const stood = pressed.contains(active) ? active : questionTookFocusFrom.current;
      const from = stood !== null && pressed.contains(stood) ? stood : undefined;
      // A move that takes no focus and has its place told leaves the follow of an earlier move
      // as it is: the effect checks that follow against its own press.
      if (position !== undefined && from === undefined) return;
      follow = {
        announce: position === undefined,
        column: target.column,
        focused: undefined,
        from,
        key,
        laneId,
        settled: false,
      };
      afterMove.current = follow;
    });
    void moved?.then(() => {
      if (follow !== undefined) follow.settled = true;
    });
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
      run: () => moveFocused("down"),
      when: focusedCard,
    },
    {
      id: "move-up",
      keys: "Shift+K",
      label: "Move card up",
      run: () => moveFocused("up"),
      when: focusedCard,
    },
    {
      id: "move-next",
      keys: "Shift+L",
      label: "Move card to the next status",
      run: () => moveFocused("next"),
      when: focusedCard,
    },
    {
      id: "move-previous",
      keys: "Shift+H",
      label: "Move card to the previous status",
      run: () => moveFocused("prev"),
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
      run: () =>
        cardAround(document.activeElement)
          ?.querySelector<HTMLElement>(PRIORITY_TRIGGER_SELECTOR)
          ?.focus(),
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
        const around = cardAround(active);
        if (around === active || (around === null && columnAround(active) === active)) {
          (active as HTMLElement).blur();
        } else {
          around?.focus();
        }
      },
      when: () =>
        cardAround(document.activeElement) !== null ||
        columnAround(document.activeElement) === document.activeElement,
    },
  ]);

  return (
    <>
      {/* The question belongs to the Board's moves (`useBoardMoves`), not to the listing's read:
          it is drawn whatever the listing's read shows, so a read that fails while it is open
          leaves it to be answered, and its answer ends the move that holds every later read of
          the listing. `QuestionOpening` comes first, so it reads what the question takes the
          focus from (`afterMove`). */}
      {question === undefined ? null : (
        <>
          <QuestionOpening tookFocusFrom={questionTookFocusFrom} />
          <ParentStatusDialog question={question} />
        </>
      )}
      <section aria-label="Project board" className="min-w-0" data-board-by={by} ref={boardRef}>
        {error === undefined ? null : (
          <div
            aria-live="assertive"
            className={`mb-4 rounded-xl border px-4 py-3 text-sm ${card} ${textPrimaryOnSurface}`}
            role="alert"
          >
            {error}
          </div>
        )}
        {/* A refusal is read out in the live region below, so this line is for the eye alone. */}
        {refusal === undefined ? null : (
          <p
            className={`mb-4 rounded-xl border px-4 py-3 text-sm ${card} ${textPrimaryOnSurface}`}
            data-testid="board-refusal"
          >
            {refusal}
          </p>
        )}
        <p aria-label="Board announcements" aria-live="polite" className="sr-only" role="status">
          <span key={announcement.count}>{announcement.text}</span>
        </p>
        <DndContext
          accessibility={BOARD_ACCESSIBILITY}
          collisionDetection={boardCollisionDetection}
          onDragCancel={landCard}
          onDragEnd={onDragEnd}
          onDragStart={liftCard}
          sensors={sensors}
        >
          {/* The reader's place (R11): a card is a row and its column or lane its group. */}
          <ViewportAnchor
            enabled={() => !inFlight.current && drawnView.current === viewKey}
            group="data-board-place"
            holdAtTop
            item="data-board-card"
          >
            {lanes ? (
              <div className="flex flex-col gap-6" data-testid="board-lanes-container">
                {lines.map((line) => {
                  const lane = line.lane ?? null;
                  const count = line.cells.reduce((total, cell) => total + cell.items.length, 0);
                  return (
                    <section aria-label={`${laneLabel(lane)} lane`} key={laneKey(lane)}>
                      <h2
                        className={`mb-2 flex items-center gap-2 text-xs font-semibold uppercase tracking-wide ${textSecondaryOnSurface}`}
                      >
                        {laneLabel(lane)}
                        <Pill>{count}</Pill>
                      </h2>
                      <BoardLineView
                        by={by}
                        dragging={dragging}
                        line={line}
                        testId="board-lane-scroll-container"
                        userState={userState}
                      />
                    </section>
                  );
                })}
              </div>
            ) : lines[0] === undefined ? null : (
              <BoardLineView
                by={by}
                dragging={dragging}
                line={lines[0]}
                testId="board-scroll-container"
                userState={userState}
              />
            )}
          </ViewportAnchor>
        </DndContext>
      </section>
    </>
  );
}
