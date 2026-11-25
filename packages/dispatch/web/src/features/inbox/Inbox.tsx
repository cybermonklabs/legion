import { awaitsDecision, INBOX_DISMISSED_PAGE, INBOX_THREAD_KINDS } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { useVirtualizer, useWindowVirtualizer, type Virtualizer } from "@tanstack/react-virtual";
import {
  createContext,
  type FocusEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useContext,
  useEffect,
  useImperativeHandle,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { Link } from "react-router-dom";

import { usersQuery, whoAmIQuery } from "../../api/queries";
import type { MyReplyRow, OpenAskRow } from "../../api/types";
import type { BarActionProps } from "../../components/BarAction";
import { textGlyph } from "../../components/bar-look";
import { ControlRow } from "../../components/ControlRow";
import { type ControlSize, controlPadding } from "../../components/control-names";
import { WINDOW_OVERSCAN } from "../../components/DataTable";
import type { DisplayMenuProps, DisplayMenuSection } from "../../components/DisplayMenu";
import { EmptyState } from "../../components/EmptyState";
import type { FilterChip } from "../../components/filter-chips";
import { focusLost, focusOwnStop } from "../../components/focus-place";
import { heldPress, useHeldThroughAnswer } from "../../components/held-press";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { type ListMarks, MultiSelect } from "../../components/MultiSelect";
import { LabelPill } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import { SearchBox } from "../../components/SearchBox";
import type { SegmentedControlProps } from "../../components/SegmentedControl";
import { focusListStop, SelectionBar, useLastMarkedPlace } from "../../components/SelectionBar";
import { useNestedPanels } from "../../components/useNestedPanels";
import { capSearchQuery, useAddressNavigate } from "../../lib/page-address";
import {
  borderDefault,
  checkboxAccent,
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { searchWords } from "../agents/agent-search";
import { CredentialRequestsSection } from "../credentials/CredentialRequestsSection";
import { type CredentialRequests, useCredentialRequests } from "../credentials/pending";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { AgentName, useAgentText } from "../refs/AgentName";
import { COPY_REF_SELECTOR, CopyRefButton } from "../refs/CopyRefButton";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import {
  ANSWERS_PATH,
  buildReferencePath,
  INBOX_FILTER_FIELDS,
  INBOX_FILTER_VOCABULARY,
  INBOX_GROUP_BYS,
  INBOX_SHOWS,
  INBOX_SORTS,
  type InboxFilterField,
  type InboxGroupBy,
  type InboxShow,
  type InboxSort,
  NO_FILTER_VALUE,
} from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { textSourceOf } from "../refs/text-references";
import { type KeyBinding, type KeymapScope, useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching, focusedMatching, roveFocus, roveIndex } from "../shell/roving";
import { READING_PANE_QUERY, useMediaQuery } from "../shell/useDialog";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { THREAD_ORDER_LABELS, THREAD_ORDERS, useThreadOrder } from "../threads/thread-order";
import { AskCard } from "./AskCard";
import { AskHandOn } from "./AskHandOn";
import { InboxOwnerLink, ThreadOwnerLine } from "./AskOwnerLink";
import { replyRefusal } from "./AskThread";
import {
  AskAssignWritesContext,
  AskAssignWritesProvider,
  AssignToMe,
  useAskAssign,
} from "./AssignToMe";
import { formatAskAge } from "./ask-age";
import { outsideAskCard } from "./ask-card";
import { type AskOrdinal, askOrdinals } from "./ask-name";
import { askUrgency } from "./ask-urgency";
import { useInboxWrites } from "./ask-writes";
import { blockedOnYouText, needsYou, waitingOnHuman } from "./BlockedOnYou";
import { ComposeDialog } from "./ComposeDialog";
import { declineRefusal } from "./DeclineControl";
import { DismissControl, RowDismissContext } from "./DismissControl";
import {
  ALL_GROUP_KEY,
  DIMENSION_LABELS,
  dimensionValue,
  dimensionValueLabel,
  filterCounts,
  group,
  type InboxFilters,
  type InboxGroup,
  inFieldFilter,
  LATER_GROUP_KEY,
  sortRows,
} from "./grouping";
import { GROUP_PAGE, groupItems } from "./InboxGroups";
import { buildInboxPath, type InboxRoute, useInboxRoute } from "./inbox-route";
import { ROW_ATTRIBUTE, ROW_SELECTOR } from "./inbox-row";
import { matchesInboxSearch } from "./inbox-search";
import {
  type InboxListRow,
  rowId,
  rowLastActivityAt,
  rowPriority,
  rowType,
  rowUrgency,
  threadReferenceRoute,
} from "./list-row";
import { MARK_ALL_REFUSED_TEXT, MARK_ALL_STOPPED_TEXT, useMarkAllRead } from "./mark-all";
import { useMotionHold } from "./motion-hold";
import { useInboxAsks } from "./one-read";
import { rowApprovalGate } from "./one-read-rows";
import {
  AskAnswerWritesContext,
  AskClarificationWritesContext,
  AskDeclineWritesContext,
  AskReplyWritesContext,
  useOutstandingAnswerWrites,
} from "./outstanding-answers";
import { PaneDivider, usePaneWidth } from "./PaneDivider";
import { type PlacedRow, usePaneSelection } from "./pane-selection";
import { useFoldedGroups } from "./preferences";
import { ReadingPane } from "./ReadingPane";
import { RowApprovalMark, RowKindIcon, RowPriorityTag, RowUrgencyMark } from "./RowMarks";
import {
  BulkSnoozingContext,
  ROW_SNOOZE_SELECTOR,
  SnoozeControl,
  SnoozePick,
} from "./SnoozeControl";
import {
  EVERYONE_FILTER_VALUE,
  type InboxSection,
  inAssigneeFilter,
  isOwnList,
  isUnassigned,
  sectionOf,
  sectionTitles,
  UNASSIGNED_FILTER_VALUE,
} from "./sections";
import { isSnoozed } from "./snooze";
import { ThreadRowCard } from "./ThreadRowCard";
import { ThreadLine, ThreadTitle } from "./ThreadTitle";
import { answerFailure, clarificationRefusal } from "./useAskAnswerForm";
import { AskHandOnWritesContext, useAskHandOnWrites } from "./useAskHandOn";
import { AskSnoozeContext, AskSnoozeWritesProvider, useAskSnooze } from "./useAskSnooze";
import { useControlName } from "./useControlName";
import { dismissRow, type InboxDismissRow } from "./useInboxDismiss";
import { useInboxSelection } from "./useInboxSelection";
import { useInboxThreadRows } from "./useInboxThreadRows";

/** One of the **Filter** panel's five controls that opens a popover of its own: one of the four
 *  filter fields, or the assignee control beside them. */
type InboxFilterPanel = InboxFilterField | "assignee";

/** **New message**'s glyph, drawn before its words and alone below 640 px of the bar's width,
 *  where its words stay its name. */
const NEW_MESSAGE_GLYPH = textGlyph("✎");

/** **Mark all read**'s glyph, drawn before its words where **More** lists it. */
const MARK_ALL_GLYPH = textGlyph("✓");

/** The size the Inbox draws its selection bar at, the comfortable one: the one value the bar
 *  takes (`SelectionBar`'s `size`) and its actions' chrome reads, so the two cannot drift apart. */
const BAR_SIZE: ControlSize = "comfortable";

/** The chrome of the selection bar's actions - **Select all**, **Clear selection** and the
 *  thread's **Dismiss** - in the recipe of the size the bar is drawn at (`BAR_SIZE`). */
const BAR_CONTROL_CLASS = `shrink-0 rounded-lg text-sm font-medium ${controlPadding[BAR_SIZE]} ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`;

/** The values a field offers, in the order its control lists them: its vocabulary, the one the
 *  address keeps the field's values by (`INBOX_FILTER_VOCABULARY`). Project is the one field
 *  whose vocabulary is the workspace's, so it is read off the rows themselves. */
function fieldOptions(
  field: InboxFilterField,
  rows: readonly InboxListRow[],
  viewer: string,
  chosen: readonly string[]
): string[] {
  const vocabulary = INBOX_FILTER_VOCABULARY[field];
  let listed: readonly string[];
  if (vocabulary !== undefined) listed = vocabulary;
  else {
    const keys = new Set<string>();
    for (const row of rows) {
      const value = dimensionValue(row, field, viewer);
      if (value !== NO_FILTER_VALUE) keys.add(value);
    }
    listed = [...keys].sort((left, right) => left.localeCompare(right)).concat(NO_FILTER_VALUE);
  }
  // A value the address carries that this list has no row for is still a row of the control,
  // so the reader can always see what they are filtering by and switch it off.
  return [...listed, ...chosen.filter((value) => !listed.includes(value))];
}

/** What the assignee control's trigger says, in the reader's own words. */
function assigneeTriggerText(selected: readonly string[], viewer: string): string {
  if (selected.length === 0) return "Assignee · everyone";
  if (isOwnList(selected, viewer)) return "Assignee · you";
  if (selected.length > 1) return `Assignee · ${selected.length}`;
  const only = selected[0];
  return `Assignee · ${only === UNASSIGNED_FILTER_VALUE ? "Unassigned" : only}`;
}

/** How the list may be ordered, as the display menu names each choice. */
const SORT_LABELS: Record<InboxSort, string> = { ...DIMENSION_LABELS, newest: "Newest first" };

/** How the list may be banded, as the display menu names each choice. */
const GROUP_LABELS: Record<InboxGroupBy, string> = {
  ...DIMENSION_LABELS,
  none: "None",
  "waiting-on": "Waiting on",
};

/** Which list is showing, as the display menu names each choice. */
const SHOW_LABELS: Record<InboxShow, string> = { dismissed: "Dismissed", inbox: "Inbox" };

/** The choices of one display-menu section, in the order the menu lists them. */
function menuOptions<T extends string>(
  values: readonly T[],
  labels: Record<T, string>
): DisplayMenuSection["options"] {
  return values.map((value) => ({ label: labels[value], value }));
}

/** The value a display-menu section hands back, as one of the words that section lists, or
 *  undefined for anything else. A menu holds sections of unrelated unions at once, so its own
 *  value type is a plain `string`; this is where the page reads its own word back out of one. */
function pick<T extends string>(values: readonly T[], next: string): T | undefined {
  return values.find((value) => value === next);
}

/** When an agent replied last on a row that still needs the viewer, that reply is surfaced so
 *  the viewer knows to look before answering. Whose turn it is ("Waiting on you" / "Waiting on
 *  <agent>") is the card's own status line. */
function InboxRowChip({ ask }: { ask: OpenAskRow }): ReactNode {
  if (ask.waiting_on === "human" && ask.last_reply?.author.kind === "session") {
    // One span, so the pill's flex box keeps the space between the name and "replied".
    return (
      <LabelPill>
        <span>
          <AgentName actor={ask.last_reply.author} inline /> replied
        </span>
      </LabelPill>
    );
  }
  return null;
}

const BULK_PICKER_ATTRIBUTE = "data-inbox-bulk-snooze";
const BULK_PICKER_MARKS: ListMarks = { trigger: { [BULK_PICKER_ATTRIBUTE]: "" } };
/** The bulk bar's snooze picker, its list's trigger: `h` hands it focus and Escape takes it back
 *  to the list. */
const BULK_PICKER_SELECTOR = `[${BULK_PICKER_ATTRIBUTE}]`;

/** The reading column's own scroller: `l` hands it focus and Escape takes it back to the list. */
const PANE_SCROLLER_SELECTOR = '[data-testid="inbox-pane-scroller"]';

/** The "Add a note or answer in your own words" disclosure an `AskCard` folds its answer box
 *  behind in its compact variant - the card a decision block hosts, which is what the reading
 *  column shows for a question written as one - while it is still shut. The card carries the
 *  handle (`AskCard.tsx`), as it carries `data-ask-answer` and `data-ask-option`. */
const NOTE_DISCLOSURE_SELECTOR = '[data-ask-note-disclosure][aria-expanded="false"]';

/** The row that holds keyboard focus itself — not one merely containing a focused control. */
function focusedRow(): HTMLElement | null {
  return focusedMatching(ROW_SELECTOR);
}

function rowAround(node: Element | null): HTMLElement | null {
  return closestMatching(node, ROW_SELECTOR);
}

/** Everything inside a short row that answers a click itself: its links, its checkbox, and
 *  whatever control a row gains later. A click on one of those is that control's - the link
 *  opens the ask inside its issue or document, the checkbox marks the row - so the row's own
 *  selection stands down for it, the rule the margin's cards hold to (`margin/ThreadCard.tsx`).
 *  Everywhere else in the row, its title and its time included, the click is the row's and
 *  selects it. */
const ROW_CONTROL_SELECTOR = "a, button, input, label, select, textarea";

/** What every row layout is given: the row, where it sits, and the reader's marks on it. */
interface InboxRowProps {
  /** The band this row is drawn under, for the fold and the held-row rule. */
  group: string;
  /** Whether the reader has marked this row for a bulk action. */
  marked: boolean;
  /** Marks or unmarks this row, by its own `rowId`; `x` and the row's own checkbox are the two
   *  ways in. `range` is a shift-click on the checkbox, which takes the range from the anchor to
   *  this row (`useInboxSelection`'s `markRange`). */
  onMark: (id: string, range: boolean) => void;
  /** Set on the held row - one the server dropped or moved to another band that stays where
   *  the reader last saw it while they are still on it; called when their focus or pointer
   *  leaves it. */
  onRelease?: () => void;
  /** This ask's place among its owner's rows, when the owner has more than one listed. */
  ordinal: AskOrdinal | undefined;
  row: InboxListRow;
  section: InboxSection;
}

/** The row's own checkbox, the pointer's way to the mark `x` makes. Every row takes one: with
 *  rows marked the bulk bar offers **Snooze** for the asks among them and **Dismiss** for the
 *  threads. A shift-click takes the range from the anchor to this row. */
function MarkCheckbox({
  id,
  marked,
  name,
  onMark,
}: {
  id: string;
  marked: boolean;
  name: string;
  onMark: (id: string, range: boolean) => void;
}): ReactNode {
  return (
    <input
      aria-label={`Select ${name}`}
      checked={marked}
      className={`size-4 shrink-0 ${checkboxAccent}`}
      // React fires a checkbox's change from the browser's click on it, so the event under it is
      // that click, with its shift key: the pointer's, and the space bar's where the engine
      // passes it (Chromium does, Firefox does not).
      onChange={(event) =>
        onMark(id, event.nativeEvent instanceof MouseEvent && event.nativeEvent.shiftKey)
      }
      // On macOS, Safari and Firefox do not focus a checkbox the pointer clicks (MDN, "Clicking a
      // <button>", the focus table), so the shift-press puts the focus on it here: the range's
      // keys, the bar's return and the hold read the row that holds the focus. The press keeps
      // its default action: the three engines the browser suite runs select no text from it.
      onPointerDown={(event) => {
        if (event.shiftKey) event.currentTarget.focus({ preventScroll: true });
      }}
      type="checkbox"
    />
  );
}

/** A thread row's own checkbox, named after the thread it marks. The name is an `aria-label`,
 *  which can hold no markup: it is the row's one plain-text line (`ThreadLine`) - the
 *  projection of the subject by the same reader the row's own line is drawn by, with the record
 *  that line names (`ThreadTitle`), never a second stripper, or a deleted root's deleted line. */
function ThreadMarkCheckbox({
  marked,
  onMark,
  row,
  thread,
}: {
  marked: boolean;
  onMark: (id: string, range: boolean) => void;
  row: InboxListRow;
  thread: MyReplyRow;
}): ReactNode {
  return (
    <ThreadLine thread={thread}>
      {(line) => <MarkCheckbox id={rowId(row)} marked={marked} name={line ?? ""} onMark={onMark} />}
    </ThreadLine>
  );
}

/** The checkbox a row draws for its mark (`MarkCheckbox`), the one checkbox every row has. */
const MARK_SELECTOR = 'input[type="checkbox"]';

/** Where a row of the Dismissed window sits among the loaded rows, how many rows are loaded, and
 *  the virtualizer's measure of the row (`DismissedWindow`). The row's own `<li>` takes them, so
 *  it stays a direct child of the list, and a screen reader hears the row's place among every
 *  loaded row, drawn or not, as `DataTable` says it of its own window. Undefined outside that
 *  window, where every row is drawn. */
interface WindowSlot {
  readonly count: number;
  readonly index: number;
  readonly measure: (node: Element | null) => void;
}

const WindowSlotContext = createContext<WindowSlot | undefined>(undefined);

/** The `<li>` both layouts sit in: where the row is, what the reader's hand does to it, and the
 *  marks the selected row carries. Only the short row is selectable, so only it is ever drawn
 *  as the selected one. */
function InboxRowShell({
  children,
  group: groupKey,
  onRelease,
  onSelect,
  row,
  section,
  selected,
}: {
  children: ReactNode;
  group: string;
  onRelease: (() => void) | undefined;
  /** Selects this row, which is what the reading column shows; the short row alone has one. */
  onSelect: ((id: string) => void) | undefined;
  row: InboxListRow;
  section: InboxSection;
  /** Whether this is the row the reading column is showing. */
  selected: boolean;
}): ReactNode {
  const id = rowId(row);
  const slot = useContext(WindowSlotContext);
  const releaseOnFocusOut =
    onRelease === undefined
      ? undefined
      : (event: FocusEvent<HTMLLIElement>) => {
          if (!event.currentTarget.contains(event.relatedTarget)) onRelease();
        };
  return (
    /* The keyboard selects a row through the Inbox's own registry (`j`, `k` and the arrows),
       which needs no handler on the row itself; the click is the pointer's way to that same
       selection.
       biome-ignore lint/a11y/useKeyWithClickEvents: explained just above. */
    <li
      aria-current={selected ? "true" : undefined}
      aria-posinset={slot === undefined ? undefined : slot.index + 1}
      aria-setsize={slot?.count}
      className={`group rounded-xl outline-none focus-visible:ring-2 ${focusVisibleRing} ${
        selected ? surfaceMutedBg : ""
      }`}
      data-inbox-group={groupKey}
      data-inbox-row={id}
      data-inbox-section={section}
      data-inbox-selected={selected ? "true" : undefined}
      data-index={slot?.index}
      onBlur={releaseOnFocusOut}
      onClick={
        onSelect === undefined
          ? undefined
          : (event) => {
              if (
                event.target instanceof Element &&
                closestMatching(event.target, ROW_CONTROL_SELECTOR) !== null
              ) {
                return;
              }
              onSelect(id);
            }
      }
      onPointerLeave={onRelease}
      ref={slot?.measure}
      tabIndex={-1}
    >
      {children}
    </li>
  );
}

/** What a short row adds to every row layout's props: beside the reading column the row is
 *  selectable, and the one the column shows is drawn as selected. */
interface ShortRowProps {
  onSelect: (id: string) => void;
  /** Whether this is the row the reading column is showing. */
  selected: boolean;
}

/**
 * The frame both short rows draw in, two lines deep, or three where the second wraps: the kind's
 * icon at the left edge, then what the row is about and when it was last written in; under them
 * the approval state of the row's issue, the priority tag, an ask's urgency, and what the row
 * says about its owner. The approval mark stands first on that line, at its start, so the marks
 * of rows one above another start at one x whatever the other marks and the owner say. Every
 * mark is `shrink-0`, and so are the owner's key and the controls beside it, so the line wraps
 * rather than drawing one item over another: a line that cannot wrap squeezes the owner's link
 * below its key's width once a long mark and a priority fill it, and the key then runs out of its
 * link, under the copy control or past the row's edge. The owner and whatever it carries beside
 * it are one group, whose link asks the line for its key's width alone, and a document's link,
 * which has no key, for none (`sizedByKey`). So the group moves under the marks only when an
 * issue's key and its controls do not fit beside them, and otherwise stands there and the title
 * or the document's name takes the room the line leaves, and is cut there. `children` is drawn
 * under the lines.
 */
function ShortRowFrame({
  age,
  check,
  children,
  group: groupKey,
  onRelease,
  onSelect,
  owner,
  row,
  section,
  selected,
  title,
}: ShortRowProps & {
  /** When the row was last written in, as its own kind reads a time. */
  age: ReactNode;
  /** The mark checkbox, or the space a row that takes no mark leaves where it would be. */
  check: ReactNode;
  children?: ReactNode;
  group: string;
  onRelease: (() => void) | undefined;
  /** What the second line draws after the marks: the row's owner, and whatever it carries beside
   *  it. The frame draws it as one group, which wraps as one. */
  owner: ReactNode;
  row: InboxListRow;
  section: InboxSection;
  title: ReactNode;
}): ReactNode {
  return (
    <InboxRowShell
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      row={row}
      section={section}
      selected={selected}
    >
      <div className="flex flex-col gap-0.5 py-1.5">
        <div className="flex items-center gap-2">
          {check}
          <RowKindIcon type={rowType(row)} />
          {title}
          <time
            className={`shrink-0 text-xs ${textMutedOnSurfaceMuted}`}
            dateTime={rowLastActivityAt(row)}
          >
            {age}
          </time>
        </div>
        <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1 pl-6">
          <RowApprovalMark gate={rowApprovalGate(row)} />
          <RowPriorityTag priority={rowPriority(row)} />
          <RowUrgencyMark urgency={rowUrgency(row)} />
          <div className="flex min-w-0 grow items-baseline gap-2">{owner}</div>
        </div>
        {children}
      </div>
    </InboxRowShell>
  );
}

/** What a short row says of a write the column made for its row - an answer, a decline, a
 *  question back, a reply: why it was refused, or that the reader stopped it (`refusal`), else
 *  that it is out (`out`), else nothing. */
function RowWriteNote({
  out,
  refusal,
}: {
  out: string | undefined;
  refusal: string | undefined;
}): ReactNode {
  if (refusal !== undefined) {
    return (
      <div className="pl-6">
        <QueryError message={refusal} />
      </div>
    );
  }
  return out === undefined ? null : <p className={`pl-6 text-xs ${textMutedOnCanvas}`}>{out}</p>;
}

/** An open question beside the reading column: its first line, how long it has waited, and the
 *  issue or document it belongs to. */
function AskShortRow({
  ask,
  group: groupKey,
  marked,
  onMark,
  onRelease,
  onSelect,
  ordinal,
  row,
  section,
  selected,
  viewer,
}: InboxRowProps &
  ShortRowProps & {
    ask: OpenAskRow;
    /** The signed-in person's lowercase email; "Assign to me" writes it. */
    viewer: string;
  }): ReactNode {
  const { name, reads } = useControlName(ask, ordinal);
  const snooze = useAskSnooze(ask.id);
  const assign = useAskAssign(ask.id);
  const handOn = useInboxWrites(AskHandOnWritesContext).of(ask);
  const answer = useInboxWrites(AskAnswerWritesContext).of(ask.id);
  const decline = useInboxWrites(AskDeclineWritesContext).of(ask.id);
  const clarification = useInboxWrites(AskClarificationWritesContext).of(ask.id);
  return (
    <ShortRowFrame
      age={formatAskAge(rowLastActivityAt(row))}
      check={
        <>
          {reads}
          <MarkCheckbox id={ask.id} marked={marked} name={name} onMark={onMark} />
        </>
      }
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      owner={<InboxOwnerLink ask={ask} rowOwner sizedByKey />}
      row={row}
      section={section}
      selected={selected}
      title={
        // Main's title span, as main drew the question's words: its size, its place in the row
        // and the line it cuts (`truncate`). The preview is that span's inline text (`inline`),
        // so the ellipsis falls where main's did, and its type is the span's, since
        // `.dispatch-markdown` takes its size and its line from the element around it
        // (styles.css).
        <span className="min-w-0 grow truncate text-sm">
          <MarkdownPreview
            className="inline"
            lines={1}
            markdown={ask.question}
            source={textSourceOf("ask", ask.id, ask.issue_key)}
            surface="inbox-ask-row"
            tree={ask.question_tree}
          />
        </span>
      }
    >
      {/* The column's head draws the Snooze and the Assign to me of the question the column
          shows, and a short row draws neither of its own. A snooze or an Assign to me of this
          ask can be out or failed while the column shows another question: the reader moved the
          selection after pressing it in the head. The row draws the control then, so its
          `Snoozing…` or `Assigning…`, its refusal and its Retry stay on the row they are
          about. */}
      {selected || !(snooze.pending || snooze.failed) ? null : (
        <div className="flex max-w-full flex-wrap items-center gap-1 pl-6">
          <SnoozeControl
            askId={ask.id}
            label={name}
            snoozedUntil={isSnoozed(ask) ? ask.snoozed_until : null}
          />
        </div>
      )}
      {selected || ask.issue_key === null || !(assign.pending || assign.failed) ? null : (
        <div className="flex max-w-full flex-wrap items-center gap-1 pl-6">
          <AssignToMe askId={ask.id} issueKey={ask.issue_key} viewer={viewer} />
        </div>
      )}
      {/* The same for the column's card, which draws the hand-on, the answer, the decline and
          the question back of the question the column shows: each can be out or failed while
          the column shows another question. The row draws the hand-on control then, with its
          `Handing on…`, its refusal and its Retry, and a line for an answer, a decline or a
          question back, which says it is out or why it failed. That Retry is the card's, once
          the column shows the question again. */}
      {selected || !(handOn.pending || handOn.failed) ? null : (
        <div className="pl-6">
          <AskHandOn ask={ask} assignee={ask.issue?.assignee ?? null} />
        </div>
      )}
      {selected ? null : (
        <>
          <RowWriteNote
            out={answer.isPending ? "Answering…" : undefined}
            refusal={
              answer.error === null
                ? undefined
                : answerFailure({ error: answer.error, method: answer.method }).message
            }
          />
          <RowWriteNote
            out={decline.isPending ? "Declining…" : undefined}
            refusal={decline.error === null ? undefined : declineRefusal(decline.error)}
          />
          <RowWriteNote
            out={clarification.isPending ? "Asking back…" : undefined}
            refusal={clarification.error === null ? undefined : clarificationRefusal(clarification)}
          />
        </>
      )}
    </ShortRowFrame>
  );
}

/** A Reply or an Agent row beside the reading column: the thread's subject as a link to the
 *  thread itself, when it was last written in, whose it is, and the **Dismiss** that takes it out
 *  of the list - drawn while the reader's hand is on the row, and always on the selected one. */
function ThreadShortRow({
  group: groupKey,
  marked,
  onMark,
  onRelease,
  onSelect,
  row,
  section,
  selected,
  thread,
}: InboxRowProps & ShortRowProps & { thread: MyReplyRow }): ReactNode {
  const reference = threadReferenceRoute(thread);
  // The column draws an ask's thread with the ask's card, whose reply the Inbox holds by ask
  // (`AskReplyWritesContext`): it can be out or failed while the column shows another row. The
  // row then says it is out, or why it failed; the Retry is the card's, once the column shows the
  // thread again.
  const reply = useInboxWrites(AskReplyWritesContext).of(thread.thread_id);
  return (
    <ShortRowFrame
      age={<Timestamp at={rowLastActivityAt(row)} />}
      check={<ThreadMarkCheckbox marked={marked} onMark={onMark} row={row} thread={thread} />}
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      owner={
        <>
          <ThreadOwnerLine
            className={`truncate text-sm ${textMutedOnCanvas}`}
            sizedByKey
            thread={thread}
          />
          {reference === undefined ? null : <CopyRefButton route={reference} />}
          <DismissControl shown={selected} thread={thread} />
        </>
      }
      row={row}
      section={section}
      selected={selected}
      title={
        <ThreadTitle className="min-w-0 grow truncate text-sm" href={thread.ref} thread={thread} />
      }
    >
      {selected || thread.kind !== "ask" ? null : (
        <RowWriteNote
          out={reply.isPending ? "Replying…" : undefined}
          refusal={reply.error === null ? undefined : replyRefusal(reply)}
        />
      )}
    </ShortRowFrame>
  );
}

/** The row without a reading column beside it: the question's whole card, under the controls
 *  that defer or reroute it. */
function InboxCardRow({
  ask,
  group: groupKey,
  marked,
  onAnswered,
  onMark,
  onRelease,
  ordinal,
  row,
  section,
  threadUpdatedAt,
  viewer,
}: InboxRowProps & {
  ask: OpenAskRow;
  onAnswered: (id: string) => void;
  /** When the read that supplied this row's initial thread answered: the one read for the
   *  reader's own asks, the open asks for every other (`useInboxAsks`'s `updatedAtOf`). */
  threadUpdatedAt: number;
  /** The signed-in person's lowercase email; "Assign to me" writes it. */
  viewer: string;
}): ReactNode {
  const { name, reads } = useControlName(ask, ordinal);
  // The optimistic assignee takes the issue out of `isUnassigned` at the press, so the row draws
  // Assign to me while its write is out or refused as well: `Assigning…`, and a refusal with its
  // Retry.
  const assign = useAskAssign(ask.id);
  return (
    <InboxRowShell
      group={groupKey}
      onRelease={onRelease}
      onSelect={undefined}
      row={row}
      section={section}
      selected={false}
    >
      {/* One line: the issue this question belongs to on the left, the controls that defer or
          reroute it on the right. It wraps rather than overlapping - a refusal from Snooze or
          Assign to me is a full-width row inside the right group, so it never draws over the
          issue key. The approval mark stands first in the left group, right after the checkbox
          and the kind's icon, so the marks of cards one above another start at one x. The left
          group wraps too: neither the mark nor the issue key shrinks, so on a phone a long mark
          and a long key side by side would run past the card's edge. The owner's link asks the
          line for its key's width alone (`sizedByKey`), so it moves under the mark only when the
          key does not fit beside it, and otherwise stays beside it and cuts its title. */}
      <div className="mb-1.5 flex flex-wrap items-center gap-x-2 gap-y-1">
        {reads}
        <MarkCheckbox id={ask.id} marked={marked} name={name} onMark={onMark} />
        <RowKindIcon type={rowType(row)} />
        <div className="flex min-w-0 grow basis-48 flex-wrap items-baseline gap-x-2 gap-y-1">
          <RowApprovalMark gate={rowApprovalGate(row)} />
          <InboxOwnerLink ask={ask} rowOwner sizedByKey />
          {/* The reply chip truncates rather than squeezing the title away. */}
          <span className="min-w-0 max-w-[40%] shrink truncate">
            <InboxRowChip ask={ask} />
          </span>
        </div>
        {/* `max-w-full` bounds the group to the row: `shrink-0` alone lets a server's own
            refusal reason - `snoozed_until must be in the future`, or anything longer - set
            the group's width and widen the whole document past the viewport on a phone. */}
        <div className="flex max-w-full shrink-0 flex-wrap items-center gap-1">
          <RowUrgencyMark urgency={askUrgency(ask.urgency)} />
          {ask.issue_key === null ? null : (
            <PriorityControl issueKey={ask.issue_key} priority={issuePriority(ask.priority)} />
          )}
          {ask.issue_key !== null && (isUnassigned(row) || assign.pending || assign.failed) ? (
            <AssignToMe askId={ask.id} issueKey={ask.issue_key} viewer={viewer} />
          ) : null}
          <SnoozeControl
            askId={ask.id}
            label={name}
            snoozedUntil={isSnoozed(ask) ? ask.snoozed_until : null}
          />
        </div>
      </div>
      <AskCard
        ask={ask}
        initialThread={{ ask, ...ask.thread }}
        initialThreadUpdatedAt={threadUpdatedAt}
        onAnswered={onAnswered}
      />
    </InboxRowShell>
  );
}

/** A Reply or an Agent row where the question rows are cards: the row's mark over the card's
 *  shell, whose thread body - and whose **Dismiss** - is written elsewhere. */
function ThreadCardRow({
  group: groupKey,
  marked,
  onMark,
  onRelease,
  row,
  section,
  thread,
}: InboxRowProps & { thread: MyReplyRow }): ReactNode {
  return (
    <InboxRowShell
      group={groupKey}
      onRelease={onRelease}
      onSelect={undefined}
      row={row}
      section={section}
      selected={false}
    >
      {/* The mark sits where a question card's does, on a line of its own above the card. */}
      <div className="mb-1.5 flex items-center gap-2">
        <ThreadMarkCheckbox marked={marked} onMark={onMark} row={row} thread={thread} />
      </div>
      <ThreadRowCard thread={thread} />
    </InboxRowShell>
  );
}

/** The rows of one band, with each held row - one the server no longer lists here, or lists in
 *  another band, while the reader is still on it or a write of theirs on it is still out - kept
 *  after the nearest row above it that is still drawn here, so it does not move while they
 *  finish. `held` is in the order the reader last saw the rows, so of two held rows the lower is
 *  kept after the upper. */
function withHeld(
  rows: readonly InboxListRow[],
  held: readonly InboxListRow[],
  previous: readonly PlacedRow[]
): readonly InboxListRow[] {
  let banded = rows;
  for (const row of held) {
    const heldId = rowId(row);
    const ids = banded.map((each) => rowId(each));
    let at = -1;
    for (
      let index = previous.findIndex((placed) => rowId(placed.row) === heldId) - 1;
      index >= 0 && at === -1;
      index -= 1
    ) {
      const above = previous[index];
      at = above === undefined ? -1 : ids.indexOf(rowId(above.row));
    }
    banded = [...banded.slice(0, at + 1), row, ...banded.slice(at + 1)];
  }
  return banded;
}

/** `bands` with an empty band for each key of `kept` that none of them has, headed as the reader
 *  last saw it (`previous`, the bands as last drawn) and drawn after the nearest band before it
 *  there that is still drawn: the band a write out keeps its row in, or the reader's hand keeps
 *  a moved row in, once every other row the list holds has left it. */
function withHeldBands(
  bands: readonly InboxGroup[],
  kept: ReadonlySet<string>,
  previous: readonly Omit<InboxGroup, "rows">[]
): readonly InboxGroup[] {
  let drawn = bands;
  for (const [at, heading] of previous.entries()) {
    if (!kept.has(heading.key) || drawn.some(({ key }) => key === heading.key)) continue;
    let after = -1;
    for (let index = at - 1; index >= 0 && after === -1; index -= 1) {
      const before = previous[index];
      after = before === undefined ? -1 : drawn.findIndex(({ key }) => key === before.key);
    }
    drawn = [...drawn.slice(0, after + 1), { ...heading, rows: [] }, ...drawn.slice(after + 1)];
  }
  return drawn;
}

/** The row to keep where the reader last saw it (`seen`), if any: one kept for the reader
 *  (`kept`) - the row under their hand, or one whose Assign to me, hand-on, answer, decline,
 *  question back or reply is out, which the Inbox holds (`AskAssignWritesProvider`,
 *  `useAskHandOnWrites`, `useOutstandingAnswerWrites`) - or one whose snooze write is out
 *  (`snoozeOut`) - its own write, which the Inbox holds too (`useAskSnoozeWrites`), or the bar's
 *  over the rows the reader marked. Dropped from the list: a kept row is kept where last seen,
 *  drawn as `current`, or as last seen while `current` is undefined - an answer or a decline
 *  takes its row out of the Inbox's read, an Assign to me or a hand-on can take it out of the
 *  reader's list, and a read of the Inbox can take it out while a question back or a reply is
 *  out. Moved to another band: what the list says about it changed under the reader's hand,
 *  their write or their snooze, so the listed row is kept in the band they saw it in. A move into
 *  or out of Later is a snooze, the reader's own press, and it is made once its write lands: the
 *  row is kept in its band while that write is out, and moves in the commit in which the Inbox
 *  lets go of it (`snoozing`). Its control's hold lasts through that commit
 *  (`useHeldThroughAnswer`), so a press on its own control takes its own row away while the
 *  control is held. A refused or stopped write is not out, so the next read places its row. With
 *  no snooze of it out, a move into or out of Later is shown at once, for a kept row too. */
function heldRow(
  seen: PlacedRow,
  listed: InboxListRow | undefined,
  current: InboxListRow | undefined,
  now: string | undefined,
  { kept, snoozeOut }: { readonly kept: boolean; readonly snoozeOut: boolean }
): PlacedRow | undefined {
  if (listed === undefined) {
    if (!kept) return undefined;
    return current === undefined ? seen : { ...seen, row: current };
  }
  const movedBand =
    now !== undefined &&
    now !== seen.group &&
    (snoozeOut || (now !== LATER_GROUP_KEY && seen.group !== LATER_GROUP_KEY));
  return movedBand ? { ...seen, row: listed } : undefined;
}

/** What the Inbox says when no band has a row. Typed text comes first: a reader who has narrowed
 *  the list is told why it is empty before anything else is, whatever view, filter or agent the
 *  address also carries. It answers on the words the box searches by rather than on a non-empty
 *  `q`, so an address carrying only spaces narrows nothing and says nothing about it. The
 *  Dismissed view is about one thing only, so it says that and nothing else. An agent filter's
 *  line is about that agent's asks alone, naming it in plain text (`useAgentText`): the empty
 *  state is a message, not a byline. Otherwise the credential requests above the bands wait on
 *  the viewer as much as an ask whose turn is theirs, so it says nothing until their list has
 *  come back with none waiting: while the list loads, or after it fails, the Inbox cannot say that
 *  nothing needs them. An expired request waits on nobody (`awaitsDecision`), so a list holding
 *  only expired ones still lets it say so, above which the section lists them for dismissal. The
 *  three filtered lines differ because only the reader's own list is about them: a cleared filter
 *  is about everyone, and any other selection is about whoever it names. Nothing is drawn under
 *  the line: the Inbox view lists every thread the reader has not dismissed, read or not, so no
 *  thread waits anywhere else for a line to point at. */
function emptyStateMessage({
  agent,
  agentTitle,
  credentials,
  selection,
  show,
  viewer,
  words,
}: {
  agent: string | undefined;
  agentTitle: string | undefined;
  credentials: CredentialRequests;
  /** Who the list is filtered to, as `?assignee=` reads: empty is the cleared filter. */
  selection: readonly string[];
  show: InboxShow;
  viewer: string;
  /** What the search box narrows by, as the list reads it: `searchWords` of the capped `?q=`. */
  words: readonly string[];
}): string | undefined {
  if (words.length > 0) return "Nothing matches this text";
  if (show === "dismissed") return "Nothing dismissed";
  if (agent !== undefined) return `No open asks from ${agentTitle}`;
  if (credentials.status !== "listed" || credentials.requests.some(awaitsDecision)) {
    return undefined;
  }
  if (isOwnList(selection, viewer)) return "Nothing needs you";
  return selection.length === 0 ? "Nothing needs anyone" : "Nothing matches this filter";
}

/**
 * What the Inbox's header line holds around its search box, by where the Inbox stands. On the
 * page, the title is the page's `h1` and the line ends in the **Answered by you** link: only the
 * page links to the answers page, and the drawer's Inbox stays a peek. In the drawer, the title
 * heads the dialog and the line ends in its close button, which the drawer hands `useDialog` as
 * the dialog's first focus (`initialFocusRef`), so the ref is the drawer's and the button is
 * drawn here.
 */
export type InboxHeader =
  | { readonly kind: "page" }
  | {
      readonly closeRef: RefObject<HTMLButtonElement | null>;
      readonly kind: "drawer";
      readonly onClose: () => void;
    };

/** The Inbox page's header line, which an Inbox given none draws. */
const PAGE_HEADER: InboxHeader = { kind: "page" };

/**
 * `filter`/`onFilterChange`/`keymapScope` exist for `InboxDrawer` (LEGION-547): a peek
 * embedded on another route must not read or write the real URL for its filters or the text
 * typed in its search box (either would navigate the page it sits over), and its
 * Escape/j/k/etc. must resolve under the drawer's own `dialog` scope - the modal scope a dialog
 * anywhere on the stack exclusively consults - rather than the standalone page's `inbox` scope,
 * which a dialog's keydown handling never reaches. `pane` is the third: the drawer is a peek at
 * any width, never a two-column reader, and `header` the fourth: the drawer's title line ends in
 * its close button. Each defaults to the Inbox page's, so a host that passes none of them draws
 * the page's URL-backed Inbox.
 */
export interface InboxProps {
  filter?: InboxRoute;
  /** The header line's title and what ends it (`InboxHeader`); the page's where absent. The
   *  Inbox draws the line itself because the search box in it is the Inbox's own state. */
  header?: InboxHeader;
  keymapScope?: KeymapScope;
  /** A host that owns the filter itself: every change this list makes - a filter, the sort, the
   *  grouping, the view, the text typed in the search box - is reported here instead of being
   *  written to the address. */
  onFilterChange?: (next: InboxRoute) => void;
  /** Whether this Inbox may show its reading column beside the list, which it does from
   *  `READING_PANE_QUERY` (1280 px). False keeps the card in the row, no selection and nothing
   *  written to `?item=`, at every width, which is what the drawer is. */
  pane?: boolean;
  /** The box the host scrolls these rows in, where that is neither the window nor the list's
   *  own column: the drawer scrolls them in its `aside`, so the viewport anchor must read and
   *  move that element's `scrollTop` - `window.scrollBy` moves nothing there, and the row the
   *  reader's hand is on jumps by the height of a row removed above it. Ignored while the
   *  reading pane shows, where the list column is the box that scrolls. */
  scroller?: RefObject<HTMLElement | null>;
}

/** The Inbox under its own ask writes (`InboxAskWrites`). Every control it draws for an ask shows
 *  that ask's one write of its kind, which the Inbox holds, whichever control pressed it. The
 *  compose dialog that **New message** and `n` open stands here, over the list rather than in it,
 *  so neither the list's loading line nor its error takes the message the person is writing. */
export function Inbox({
  filter,
  header,
  keymapScope,
  onFilterChange,
  pane,
  scroller,
}: InboxProps = {}): ReactNode {
  const [composing, setComposing] = useState(false);
  return (
    <InboxAskWrites>
      <InboxBody
        composing={composing}
        filter={filter}
        header={header}
        keymapScope={keymapScope}
        onCompose={() => setComposing(true)}
        onFilterChange={onFilterChange}
        pane={pane}
        scroller={scroller}
      />
      {composing ? <ComposeDialog onClose={() => setComposing(false)} /> : null}
    </InboxAskWrites>
  );
}

/**
 * Every write one Inbox holds by ask (`ask-writes.ts`, `AskAssignWritesProvider`): each row's
 * snooze and Assign to me, and the hand-on, the answer, the decline, the question back and the
 * reply of every card the Inbox draws, the document page's own cards in its reading column
 * included. A control can go while its write is out - the reading column's head and card go
 * whenever the reader moves the selection - and the write, its row's hold and its refusal stay.
 * The page's Inbox and the drawer's hold their own. The writes' own steps re-render this
 * component alone: what it draws re-renders only when the writes it provides change.
 */
function InboxAskWrites({ children }: { children: ReactNode }): ReactNode {
  const handOns = useAskHandOnWrites();
  const answers = useOutstandingAnswerWrites();
  const declines = useOutstandingAnswerWrites();
  const clarifications = useOutstandingAnswerWrites();
  const replies = useOutstandingAnswerWrites();
  return (
    <AskSnoozeWritesProvider>
      <AskAssignWritesProvider>
        <AskHandOnWritesContext value={handOns}>
          <AskAnswerWritesContext value={answers}>
            <AskDeclineWritesContext value={declines}>
              <AskClarificationWritesContext value={clarifications}>
                <AskReplyWritesContext value={replies}>{children}</AskReplyWritesContext>
              </AskClarificationWritesContext>
            </AskDeclineWritesContext>
          </AskAnswerWritesContext>
        </AskHandOnWritesContext>
      </AskAssignWritesProvider>
    </AskSnoozeWritesProvider>
  );
}

/** The space between two rows of the Dismissed list, in pixels: its `gap-3`. */
const DISMISSED_ROW_GAP = 12;

/** How tall a row of the Dismissed view is before it is drawn and measured: a short row beside
 *  the reading column, and a card without it. Close to what each draws at, so the scroll box is
 *  about the right height from the first draw and the rows move little once measured. */
const DISMISSED_SHORT_ROW_PX = 58;
const DISMISSED_CARD_ROW_PX = 120;

/** What the Inbox asks of the Dismissed window. */
interface DismissedWindowHandle {
  /** Focuses row `id`, the loaded row at `index`: at once where the window draws it, and else
   *  once the window has scrolled to it and drawn it. */
  focusRow: (id: string, index: number) => void;
}

interface DismissedWindowProps {
  /** The box the rows scroll in: the list column beside the reading column, or the drawer's own
   *  box. Undefined where the window scrolls them. */
  readonly box: RefObject<HTMLElement | null> | undefined;
  /** The row in hand where the reading column is on: the selected row, which `j` and `k` move.
   *  Undefined without the column, where the keys move the focus instead (`focusRow`). */
  readonly hand: string | undefined;
  readonly handle: RefObject<DismissedWindowHandle | null>;
  /** Reads the next page of dismissals. Undefined where there is none, or while it loads. */
  readonly onNearEnd: (() => void) | undefined;
  readonly renderRow: (row: InboxListRow) => ReactNode;
  /** How tall a row is before it is measured. */
  readonly rowHeight: number;
  /** Every loaded row, in the order the view lists them: what the window draws a part of. */
  readonly rows: readonly InboxListRow[];
}

/**
 * The Dismissed view's rows, drawn as a window: the rows in sight and `WINDOW_OVERSCAN` more
 * above and below them, out of every row the view has loaded. A person can have thousands of
 * dismissed threads, and drawing every loaded row made the view slower with each page. The rows
 * stay the view's own `<li>`s in its own `<ul>`, with the virtualizer `DataTable` uses.
 *
 * The window scrolls the box the list's anchor scrolls: the list column beside the reading
 * column, the drawer's box, or the window itself. Each kind needs its own virtualizer hook, so
 * this picks one of two components.
 */
function DismissedWindow(props: DismissedWindowProps): ReactNode {
  return props.box === undefined ? (
    <DismissedPageWindow {...props} />
  ) : (
    <DismissedBoxWindow {...props} box={props.box} />
  );
}

/** What both windows ask of their virtualizer: the loaded rows, each keyed by its row, and
 *  `margin`, where the list starts in what the window scrolls (`DismissedRows`). */
function dismissedWindowOptions(props: DismissedWindowProps, margin: number) {
  return {
    count: props.rows.length,
    estimateSize: () => props.rowHeight,
    gap: DISMISSED_ROW_GAP,
    getItemKey: (index: number) =>
      props.rows[index] === undefined ? index : rowId(props.rows[index]),
    overscan: WINDOW_OVERSCAN,
    scrollMargin: margin,
  };
}

/** The window over rows the page itself scrolls. */
function DismissedPageWindow(props: DismissedWindowProps): ReactNode {
  const [margin, setMargin] = useState(0);
  const virtualizer = useWindowVirtualizer(dismissedWindowOptions(props, margin));
  return <DismissedRows {...props} onMargin={setMargin} virtualizer={virtualizer} />;
}

/** The window over rows a box scrolls. */
function DismissedBoxWindow(
  props: DismissedWindowProps & { readonly box: RefObject<HTMLElement | null> }
): ReactNode {
  const [margin, setMargin] = useState(0);
  const virtualizer = useVirtualizer({
    ...dismissedWindowOptions(props, margin),
    getScrollElement: () => props.box.current,
  });
  return <DismissedRows {...props} onMargin={setMargin} virtualizer={virtualizer} />;
}

/**
 * The drawn part of the window, and what keeps it right.
 *
 * The list starts below whatever the box or the page draws above it, so where it starts is read
 * after each commit and handed to the window (`onMargin`), whose virtualizer counts its rows from
 * it (`scrollMargin`). The row in hand is always drawn: a step of `j` or `k` to a row the window
 * does not draw scrolls the window to it first, and the row takes the focus, or the selection,
 * once it is drawn. The next page is read once the window reaches the last loaded row, one
 * request for each number of loaded rows; **Show more** still reads it by key.
 *
 * A focused row the person scrolls out of the window is no loss. The window takes the row away,
 * which is no press of the person's, so the focus goes to no row and nothing scrolls: on the page
 * it stays on the page. In the drawer, a modal dialog, a focus left on the page would reach the
 * page behind the dialog, so the window hands it to the drawer's own stop, with no scroll, as a
 * component hands a lost focus to its own region's stop whoever caused the loss (`focusOwnStop`).
 * Scrolled back, the row is drawn again. A row a live update takes away while it is the row
 * under the hand stays in the rows the Inbox hands here (`heldRow`), so it stays drawn in its
 * place.
 */
function DismissedRows({
  box,
  hand,
  handle,
  onMargin,
  onNearEnd,
  renderRow,
  rows,
  virtualizer,
}: DismissedWindowProps & {
  readonly onMargin: (margin: number) => void;
  readonly virtualizer: Virtualizer<HTMLElement, Element> | Virtualizer<Window, Element>;
}): ReactNode {
  const list = useRef<HTMLUListElement>(null);
  /** The row that takes the focus once the window draws it. */
  const pendingFocus = useRef<string | null>(null);
  /** How many rows were loaded when the window last asked for the next page. */
  const askedAt = useRef<number | null>(null);
  /** The focused element of a row this list draws, and that row's id, noted as it takes the
   *  focus, for the window's own hand-off below. */
  const focusedIn = useRef<{ readonly element: Element; readonly id: string } | null>(null);
  const drawnRow = (id: string) =>
    list.current?.querySelector<HTMLElement>(`[${ROW_ATTRIBUTE}="${CSS.escape(id)}"]`) ?? null;

  useImperativeHandle(handle, () => ({
    focusRow: (id, index) => {
      const row = drawnRow(id);
      if (row !== null) {
        pendingFocus.current = null;
        row.focus();
        return;
      }
      pendingFocus.current = id;
      virtualizer.scrollToIndex(index, { align: "auto" });
    },
  }));

  // Where the list starts in what the box or the page scrolls, which the virtualizer counts its
  // rows from. Read after every commit: a bar or a filter row above the list can come and go.
  useLayoutEffect(() => {
    const element = list.current;
    if (element === null) return;
    const top = element.getBoundingClientRect().top;
    const scroller = box?.current ?? null;
    const start =
      scroller === null
        ? top + window.scrollY
        : top - scroller.getBoundingClientRect().top + scroller.scrollTop;
    if (Math.abs(start - virtualizer.options.scrollMargin) >= 1) onMargin(start);
  });

  // A row a step asked for takes the focus once it is drawn, with no scroll of its own: the
  // window has already brought it into view.
  useLayoutEffect(() => {
    const id = pendingFocus.current;
    if (id === null) return;
    const row = drawnRow(id);
    if (row === null) return;
    pendingFocus.current = null;
    row.focus({ preventScroll: true });
  });

  // The selected row stays drawn and in view as `j` and `k` move it. A selected row the window
  // draws is brought into view by the selection itself (`pane-selection.ts`). On the passive
  // lane, as the selection's own scroll is: the list's anchor (`ViewportAnchor`) holds the row
  // under the hand in place at the end of the commit that moved the selection, and a scroll made
  // inside that commit would be undone by it.
  // biome-ignore lint/correctness/useExhaustiveDependencies: a change of the row in hand is what this answers, and nothing else.
  useEffect(() => {
    if (hand === undefined || drawnRow(hand) !== null) return;
    const index = rows.findIndex((row) => rowId(row) === hand);
    if (index !== -1) virtualizer.scrollToIndex(index, { align: "auto" });
  }, [hand]);

  // The window's own hand-off, after every commit: a focused row the window took away on the
  // person's scroll - still a loaded row, drawn no more - hands the focus to the modal dialog the
  // list stands in, its own stop with no scroll, and leaves it on the page anywhere else. A row
  // that left the loaded rows is not the window's doing, and a focus already placed moves nothing.
  useLayoutEffect(() => {
    const noted = focusedIn.current;
    if (noted === null || noted.element.isConnected) return;
    focusedIn.current = null;
    if (!focusLost() || !rows.some((row) => rowId(row) === noted.id)) return;
    const modal = list.current?.closest<HTMLElement>('[role="dialog"][aria-modal="true"]');
    if (modal != null) focusOwnStop(modal);
  });

  const items = virtualizer.getVirtualItems();
  const first = items[0];
  const last = items.at(-1);
  const lastIndex = last?.index;
  useEffect(() => {
    if (onNearEnd === undefined || lastIndex === undefined || lastIndex < rows.length - 1) return;
    if (askedAt.current === rows.length) return;
    askedAt.current = rows.length;
    onNearEnd();
  });

  const { scrollMargin } = virtualizer.options;
  return (
    <ul
      className="flex flex-col gap-3"
      onFocus={(event) => {
        const row = event.target.closest(`[${ROW_ATTRIBUTE}]`);
        const id = row?.getAttribute(ROW_ATTRIBUTE);
        focusedIn.current = id == null ? null : { element: event.target, id };
      }}
      ref={list}
      style={{
        paddingBottom:
          last === undefined ? 0 : virtualizer.getTotalSize() - (last.end - scrollMargin),
        paddingTop: first === undefined ? 0 : first.start - scrollMargin,
      }}
    >
      {items.map((item) => {
        const row = rows[item.index];
        if (row === undefined) return null;
        return (
          <WindowSlotContext
            key={rowId(row)}
            value={{ count: rows.length, index: item.index, measure: virtualizer.measureElement }}
          >
            {renderRow(row)}
          </WindowSlotContext>
        );
      })}
    </ul>
  );
}

function InboxBody({
  composing,
  filter: filterOverride,
  header = PAGE_HEADER,
  keymapScope = "inbox",
  onCompose,
  onFilterChange,
  pane = true,
  scroller,
}: InboxProps & {
  /** Whether the compose dialog stands open (`ComposeDialog`). */
  readonly composing: boolean;
  /** Opens the compose dialog: **New message** and `n`. */
  readonly onCompose: () => void;
}): ReactNode {
  const [addressRoute, goToRoute] = useInboxRoute();
  const navigate = useAddressNavigate();
  const filter = filterOverride ?? addressRoute;
  // The reading column shows from 1280 px, and only where the host allows one: below that, and
  // in the drawer, the Inbox is the list of cards it has always been.
  const wideEnoughForPane = useMediaQuery(READING_PANE_QUERY);
  const paneShown = pane && wideEnoughForPane;
  const whoAmI = useQuery(whoAmIQuery());
  const viewer = whoAmI.data?.login;
  // Who the list is for: what `?assignee=` carries, and the reader alone when it carries
  // nothing. The address is the whole memory - a filter the reader can link, and nothing kept
  // behind their back.
  const selection = filter.assignees ?? (viewer === undefined ? [] : [viewer]);
  /** Whether the one **Filter** panel stands open. */
  const [filterOpen, setFilterOpen] = useState(false);
  /** Which of its five controls has its popover open: one at a time, and none once it shuts. */
  const filterPanel = useNestedPanels<InboxFilterPanel>(filterOpen);
  // The asks this list holds: the reader's own from the one read, and every other person's from
  // the open asks while the selection shows them (`useInboxAsks`, `one-read.ts`, which
  // `useInboxThreadRows` below follows for the threads). The list is a client-side partition of
  // those reads, so an answered row leaves every surface that reads them at once. The assignee
  // picker counts every person's rows beside their name, so the open asks are read while it
  // stands open too, and its counts are what each person holds.
  const inboxAsks = useInboxAsks(selection, viewer, filterPanel("assignee").open);
  /** Which of the reader's two lists is showing: their Inbox, or what they have dismissed out of
   *  it. A thread stays in the Inbox, read or not, until it is dismissed, by **Dismiss** or by
   *  the reader's own answer or reply in it. */
  const show: InboxShow = filter.show ?? "inbox";
  const showDismissed = show === "dismissed";
  const listRef = useRef<HTMLElement>(null);
  /** The list column, which scrolls in itself beside the reading column. It is drawn with no row
   *  as well, and it is the list's own stop for the selection bar's hand-off
   *  (`useLastMarkedPlace`). */
  const listScroller = useRef<HTMLElement>(null);
  /** The Dismissed view's window over its loaded rows, which `j` and `k` scroll to the row they
   *  move to where it is not drawn (`DismissedWindow`). */
  const dismissedWindow = useRef<DismissedWindowHandle | null>(null);
  /** The reading column, where `h`, `o`, `y`, Enter and `1`-`9` act while it is on. Written by
   *  `holdPane` below rather than handed to the wrapper, so the column's unmount is seen. */
  const paneRef = useRef<HTMLDivElement | null>(null);
  /** The grid the list, the divider and the reading column share, whose width a drag of the
   *  divider is measured against. */
  const columnsRef = useRef<HTMLDivElement>(null);
  /** How much of that grid the reading column takes, kept for the reader's browser. */
  const [paneWidth, setPaneWidth] = usePaneWidth();
  // A refetch can land while a pointer travels between rows, so it is adopted once that pointer
  // settles.
  const adoptedInbox = useMotionHold(inboxAsks.asks, listRef);
  // The credential requests the Inbox lists above its asks: the section, the Blocked on you
  // control and the empty state all read this one answer.
  const credentials = useCredentialRequests();
  const sort = filter.sort ?? "newest";
  const groupBy = filter.group ?? "priority";
  const filters: InboxFilters = filter.filters ?? {};
  const [threadOrder, setThreadOrder] = useThreadOrder();
  const [storedFolds, setStoredFold] = useFoldedGroups(groupBy);
  // The bands an address opened for this view alone (`openFoldedBand`): a link is no press of the
  // reader's, so the folds they keep in their browser stay as they left them. A band is folded
  // where they keep it folded and no address opened it here.
  const [openedHere, setOpenedHere] = useState<ReadonlySet<string>>(() => new Set());
  const folded =
    openedHere.size === 0
      ? storedFolds
      : new Set([...storedFolds].filter((key) => !openedHere.has(key)));
  // The reader's own press on a band's toggle acts on the band as it is drawn and writes what it
  // leaves to their stored folds: a band an address opened here folds, as they keep it.
  const toggleFold = useCallback(
    (key: string) => {
      const shut = !folded.has(key);
      if (shut && openedHere.has(key)) {
        setOpenedHere(new Set([...openedHere].filter((opened) => opened !== key)));
      }
      setStoredFold(key, shut);
    },
    [folded, openedHere, setStoredFold]
  );
  // How many pages of rows each band is drawing; a band starts on its first.
  const [pages, setPages] = useState<ReadonlyMap<string, number>>(() => new Map());
  const showMore = useCallback((key: string) => {
    setPages((current) => new Map(current).set(key, (current.get(key) ?? 1) + 1));
  }, []);
  /** Whether the display menu stands open. */
  const [viewOpen, setViewOpen] = useState(false);
  // Escape is a panel's while one stands open, never the selection's. A field's popover stands
  // open only inside the open Filter panel.
  const barOpen = filterOpen || viewOpen;
  // Every Inbox reader draws the assignee control and few open it, so the people are fetched on
  // its first open and kept from then on: a reader who never filters costs no `/api/v1/users`.
  const [peopleWanted, setPeopleWanted] = useState(false);
  const people = useQuery({ ...usersQuery(), enabled: peopleWanted });
  const changeFilter = (next: InboxRoute) => {
    if (onFilterChange) {
      onFilterChange(next);
    } else {
      // To the address, which `useInboxRoute` writes with `replace` rather than pushing: a
      // reader trying filters out would otherwise have to walk Back through every pick, the
      // rule every address-backed filter here holds to (`lib/page-address.ts`).
      goToRoute(next);
    }
  };
  /** The search box, so `f` can land the reader's focus in it from anywhere on the page. */
  const searchRef = useRef<HTMLInputElement>(null);
  // What the box narrows by: the address's text, cut as the box itself cuts what it writes, and
  // split into words once per render rather than once per row. An address that arrived with a
  // longer `?q=` is left as it is: the list is filtered by the cut text, while the box shows
  // what the address holds.
  const words = searchWords(capSearchQuery(filter.q ?? ""));
  const typeQuery = (typed: string) => {
    // One cut, here as well as by the box's own `maxLength`: what the box writes and what the
    // list filters by are then the one rule, and the cut stays provable from a browser.
    const next = capSearchQuery(typed);
    // A keypress on a box already at the cap produces the text the filter holds, and writing it
    // would replace the history entry and re-render every row for nothing. React restores a
    // controlled input's DOM value when the state does not move, so the cap still holds in the
    // box.
    if (next === (filter.q ?? "")) return;
    // The typed text is a field of the route like any other, so it goes back the one way every
    // other narrowing does: to the host that owns the filter, or to the address, replacing
    // rather than pushing - a reader typing a word would otherwise have to walk Back through
    // every letter of it.
    changeFilter({ ...filter, q: next === "" ? undefined : next });
  };
  // The header line: the title, the search box and what ends the line - the page's link to the
  // answers page, or the drawer's close button - in one line that never wraps, the box filling
  // the room between and the others keeping their width. Each part takes the line's height, so
  // every part's box starts at the line's top. It stands 8 px over the control bar, and every
  // return below draws it first in one fragment, while the list loads and where it failed too.
  const headerLine = (
    <div className="mb-2 flex flex-nowrap gap-3" data-inbox-header-line="">
      {header.kind === "page" ? (
        <h1 className="flex shrink-0 items-center text-[22px] font-semibold tracking-tight">
          Inbox
        </h1>
      ) : (
        <h2
          className={`flex shrink-0 items-center text-base font-semibold ${textPrimaryOnSurface}`}
        >
          Inbox
        </h2>
      )}
      <SearchBox
        className="flex-1"
        // `f`, the key that puts the reader in the box (`inboxKeys` below).
        hint="f"
        label="Search asks"
        // `capSearchQuery` again here, over the box's own `maxLength`: a paste the box takes
        // whole is still cut to what the address and the list agree on (`typeQuery`).
        onChange={typeQuery}
        placeholder="Search"
        ref={searchRef}
        value={filter.q ?? ""}
      />
      {header.kind === "page" ? (
        <Link
          className={`flex shrink-0 items-center text-sm whitespace-nowrap ${linkText} ${linkHoverText}`}
          to={ANSWERS_PATH}
        >
          Answered by you
        </Link>
      ) : (
        <button
          className={`min-h-11 shrink-0 rounded-lg px-3 text-sm font-medium ${textMutedOnSurface}`}
          onClick={header.onClose}
          ref={header.closeRef}
          type="button"
        >
          Close
        </button>
      )}
    </div>
  );
  const agentText = useAgentText();
  // Every row's snooze write, held by ask around this list (`Inbox`) rather than by the control
  // that pressed, which can go while the write is out: the column's head goes when the reader
  // moves the selection. A row whose write is out (`snoozing`) stays in the band the reader saw
  // it in, though the optimistic moment puts it into Later or out of it, and is drawn even when
  // that band is folded (`heldRow`). Its move is made in the commit this lets go of it, once the
  // write lands (see `SnoozeControl`). A refused or stopped write holds no row, and its reason
  // stays with the ask for every control drawn for it. Under the Blocked on you narrowing
  // (`?section=needs-you`), which lists no snoozed ask, the optimistic moment drops the row at
  // once, and only the reader's hand keeps it.
  const snoozing = useInboxWrites(AskSnoozeContext).out;
  /** The rows the reader can see, in the order they see them, as this list last rendered them:
   *  what the motion hold is read against, what the selection steps through, and the rows a
   *  write out may keep. Written on every render below, and read before that as the record of
   *  the render before. */
  const presented = useRef<readonly PlacedRow[]>([]);
  /** The bands this list drew at its last render, in order, with their headings, and the layout
   *  it drew them under - the grouping, or the Dismissed view: the band a write out keeps its row
   *  in is drawn from this once no other row the list holds is in it, and only under that same
   *  layout. Written on every render below. */
  const presentedBands = useRef<{
    readonly headings: readonly Omit<InboxGroup, "rows">[];
    readonly layout: string | undefined;
  }>({ headings: [], layout: undefined });
  /** The row under the reader's hand the list held at its last render, by id, and the typed
   *  words it was held under (`searchWords`, joined): it stays held while the reader's words are
   *  those, whatever the server does to the text they search. Written on every render below. */
  const heldUnder = useRef<{ readonly id: string; readonly words: string } | undefined>(undefined);
  // Every row's Assign to me, and every card's hand-on, answer, decline, question back and reply,
  // held by ask around this list (`Inbox`) as each snooze is, since the control that pressed can
  // go while the write is out: the column's head and card go when the reader moves the
  // selection. An answer or a decline takes its row out of the Inbox's read the moment it goes,
  // an Assign to me's optimistic assignee takes it out of the Unassigned filter, a hand-on's
  // optimistic `waiting_on_person` can take it out of the reader's own list, and a read of the
  // Inbox can take it out or move it while a question back or a reply is out. So a row whose
  // write is out is kept where the reader saw it until the write lands (`heldRow`). A question
  // back keeps its ask's row, and a reply keeps the row of its ask's thread, the Reply row whose
  // card made it. A refused or stopped write holds no row: the rollback has put the row back,
  // and the next read places it, with the refusal shown on it.
  //
  // Only a row this list drew is kept. The cards reach these writes through the context this
  // list provides (`useAskHandOn.ts`, `outstanding-answers.ts`), since one of them is the
  // document page's own and takes no prop of this list's, and that context reaches every card
  // under the Inbox: the card of a Reply row's thread (`ThreadBody`), whose ask is often one the
  // reader's filters hide, and every ask block of a document open in the column, not only the
  // selected one. Keeping one of those would pull a question the filters hide into the list. So
  // a write keeps its ask's row only among `presented`, the rows this list last drew.
  const assignsOut = useInboxWrites(AskAssignWritesContext).out;
  const handOnsOut = useInboxWrites(AskHandOnWritesContext).out;
  const answersOut = useInboxWrites(AskAnswerWritesContext).out;
  const declinesOut = useInboxWrites(AskDeclineWritesContext).out;
  const clarificationsOut = useInboxWrites(AskClarificationWritesContext).out;
  const repliesOut = useInboxWrites(AskReplyWritesContext).out;
  /** Whether ask `id` has an Assign to me, a hand-on, an answer, a decline or a question back
   *  out: what keeps its row where the reader saw it (`heldRow`), and what names that row to the
   *  list's anchor as leaving, so the two always agree. */
  const askWriteOut = (id: string) =>
    assignsOut.has(id) ||
    handOnsOut.has(id) ||
    answersOut.has(id) ||
    declinesOut.has(id) ||
    clarificationsOut.has(id);
  // The row the reader's hand is on (focus or pointer), read from the DOM as last committed. When
  // the server has dropped it (answered or resolved elsewhere) or handed its turn the other way
  // (their own ask-back, an agent's note or reply), it is kept - `held` - where the reader last
  // saw it, until their focus or pointer leaves it; `release` re-renders so the row is chosen
  // afresh without it and takes its new place then.
  const viewport = useRef<ViewportAnchor>(null);
  const anchor = viewport.current?.interacted() ?? null;
  /** The ask whose answer or decline of the reader's the server has just recorded
   *  (`recordAnswered`): its row goes though their hand is on it, since their answer is what
   *  takes it. It names that ask only while that is so, and is cleared before the hold below. */
  const answered = useRef<string | null>(null);
  const [, setReleased] = useState(0);
  const release = () => setReleased((count) => count + 1);
  // An `?item=` naming a row in a folded band opens that band for this view, and writes nothing
  // to the reader's stored folds (`openedHere`).
  const openFoldedBand = useCallback(
    (key: string) => {
      if (folded.has(key)) setOpenedHere((current) => new Set(current).add(key));
    },
    [folded]
  );
  /** The rows a press of the reader's on a band's fold toggle is folding shut, for the commit that
   *  folds the band: the press takes them off the list, so the list's anchor frees their space
   *  (`leaving` below) rather than hold the row at the list's centre against their going. Only
   *  that press writes it - an address's `?item=` that opens a folded band (`openFoldedBand`)
   *  takes no row away - and it is let go once the commit is in. */
  const folding = useRef<ReadonlySet<string> | null>(null);
  useLayoutEffect(() => {
    folding.current = null;
  });
  // What this list holds, and computed here rather than after the early returns so the bindings
  // below speak for exactly the rows on the page.
  const agent = filter.agent;
  // An agent filter and `?section=needs-you` both narrow the list to asks, so neither lists a
  // thread: one is about what an agent asked, the other about what waits on the reader now. The
  // Dismissed view is the other way round - an ask is never dismissed, so it holds threads alone.
  const asksOnly = agent !== undefined || filter.section === "needs-you";
  const fromAgent =
    agent === undefined
      ? (adoptedInbox ?? [])
      : (adoptedInbox ?? []).filter(
          (ask) => ask.author.kind === "session" && ask.author.id === agent
        );
  const askRows: InboxListRow[] = showDismissed
    ? []
    : (filter.section === "needs-you" ? waitingOnHuman(fromAgent) : fromAgent).map((ask) => ({
        ask,
        kind: "ask",
      }));
  // The thread side of this list, in one hook: the reader's Inbox or what they dismissed out of
  // it, the read mark a write over that list carries, and the page behind **Show more**.
  const threads = useInboxThreadRows({ asksOnly, listRef, show });
  /** Every row this list could hold before its five fields narrow it: what the bar counts. */
  const candidates = [...askRows, ...threads.rows];
  // The filters and the typed text both decide what is listed. A row a write of the reader's
  // takes out of them while it is out - Assign to me under the Unassigned filter, whose
  // optimistic assignee stops the row matching at the press, or a hand-on the filters drop - is
  // kept where the reader saw it instead (`heldRow`), as an answer and a decline are.
  const narrowed = candidates.filter(
    (row) =>
      inAssigneeFilter(row, selection, viewer ?? "") &&
      matchesInboxSearch(row, words) &&
      INBOX_FILTER_FIELDS.every((field) => inFieldFilter(row, field, filters[field], viewer ?? ""))
  );
  // The Dismissed view keeps the server's own order, newest dismissal first, and bands nothing:
  // it is one list of what the reader has already dealt with.
  const shown = showDismissed ? narrowed : sortRows(narrowed, sort, viewer ?? "");
  const byId = new Map(shown.map((row) => [rowId(row), row]));
  // What a press that names the list's own narrowing rather than ids carries: the read mark of
  // the list read the reader is looking at, and undefined where no such press may be made at
  // all - the server knows the list's five fields and nothing of the text typed in the search
  // box, so a press that sent the narrowing under typed text would act on rows the text hid.
  // **Mark all read** and a **Select all** dismiss read this one name.
  const narrowingMark = words.length === 0 ? threads.asOf : undefined;
  // The bar's **Mark all read**, over exactly this list: its filters, the read mark of the list
  // read it is showing, and the rows it is showing. The Dismissed view offers none, so it passes
  // no read mark either: nothing there is left to mark.
  const markAll = useMarkAllRead({
    asOf: showDismissed ? undefined : narrowingMark,
    filters,
    rows: shown,
  });
  // Where each row sits, judged against the clock at render: a snoozed row rejoins its band on
  // the first render after its moment passes. Computed here, with the selection, rather than
  // after the early returns, because the bar's count and the bindings both read it.
  const place = viewer === undefined ? undefined : Date.now();
  // The bands as this list heads them: another person's question is not "Waiting on you".
  const titles = sectionTitles(selection, viewer ?? "");
  const bands =
    place === undefined
      ? []
      : showDismissed
        ? [{ key: ALL_GROUP_KEY, label: undefined, rows: shown }]
        : group(shown, groupBy, { now: place, titles, viewer: viewer ?? "" });
  // A row in a band the reader has not opened is not on the page: the band renders its heading
  // and count and none of its rows, so there is no checkbox to untick, and the reader may well
  // have set that row's own moment since marking it - a pick that wrote it would overwrite what
  // they chose. A row whose own snooze write is still in flight stays rendered inside the fold,
  // so it still counts. One rule, read by the selection here and by the render below.
  const drawnIn = (bandKey: string, rows: readonly InboxListRow[]): readonly InboxListRow[] => {
    if (folded.has(bandKey)) {
      return rows.filter((row) => row.kind === "ask" && snoozing.has(row.ask.id));
    }
    // The Dismissed view pages at the server: every row a page answered is listed, and **Show
    // more** reads the next 200 rather than revealing rows already here. Its window draws the
    // listed rows near sight (`DismissedWindow`).
    if (showDismissed) return rows;
    return rows.slice(0, (pages.get(bandKey) ?? 1) * GROUP_PAGE);
  };
  const drawnIds = new Set(
    bands.flatMap(({ key, rows }) => drawnIn(key, rows).map((row) => rowId(row)))
  );
  // What the reader marked, and the two presses the bar makes on it: the state and both writes
  // live in one hook of their own (`useInboxSelection`).
  const {
    allRows,
    barRef: bulkBarRef,
    barShown: bulkBarShown,
    clearSelection,
    dismissedHere,
    dismissPick,
    dismissRows,
    dismissWrite,
    lastMarked,
    markRange,
    marked,
    onMark,
    selectAll,
    selectedAsks,
    selectedThreads,
    snoozePick,
    snoozeRefusal,
    snoozing: bulkSnoozing,
  } = useInboxSelection({
    asOf: narrowingMark,
    candidates,
    drawnIds,
    filters,
    // The rows a range runs over, read at the click: the rows the list draws, in its order, less
    // a row it holds in place after the server dropped it (`heldRow`), which is in no range. The
    // Dismissed view's range runs over every row it has loaded, in its order, whether its window
    // draws the row or not.
    order: () =>
      showDismissed
        ? shown.map((row) => rowId(row))
        : presented.current.flatMap(({ row }) => (byId.has(rowId(row)) ? [rowId(row)] : [])),
    rows: byId,
    showDismissed,
  });
  // What a row's checkbox does: a click marks or unmarks its row, a shift-click takes the range.
  const markRow = useCallback(
    (id: string, range: boolean) => {
      if (range) markRange(id, true);
      else onMark(id);
    },
    [markRange, onMark]
  );
  const paneSelection = usePaneSelection({
    bulkSnoozing: bulkSnoozing.size,
    candidates,
    filter,
    goToRoute,
    listRef,
    openFoldedBand,
    paneShown,
    presented,
    snoozing,
  });
  // The refetch a recorded answer triggers returns the list the optimistic removal already
  // produced, so it re-renders nothing on its own; release explicitly. The reader's own answer
  // is one of the ways the selection is handed on, and only while it is still the selected
  // question: the list keeps the row where the reader saw it until the server decides their
  // answer (`heldRow`), but a selection that moved on meanwhile - an approval reviewed from the
  // head, a row they walked off - must not be pulled back off the row it moved to.
  const recordAnswered = (id: string) => {
    answered.current = id;
    if (paneShown && paneSelection.selectedNow.current === id) {
      paneSelection.select(paneSelection.nextAfterSelected());
    }
    release();
  };
  const rows = () => [...(listRef.current?.querySelectorAll<HTMLElement>(ROW_SELECTOR) ?? [])];
  /** One step of the focus over the rows. In the Dismissed view that is over every loaded row in
   *  the order the view lists them, since its window draws only those near sight: a row it does
   *  not draw is scrolled to and then focused (`DismissedWindow`). */
  const step = (delta: 1 | -1) => {
    if (!showDismissed) {
      roveFocus(rows(), rowAround(document.activeElement), delta);
      return;
    }
    const ids = presented.current.map(({ row }) => rowId(row));
    const current = rowAround(document.activeElement)?.dataset.inboxRow;
    const next = roveIndex(ids.length, current === undefined ? -1 : ids.indexOf(current), delta);
    const id = ids[next];
    if (id !== undefined) dismissedWindow.current?.focusRow(id, next);
  };
  const focusInPane = () => paneRef.current?.contains(document.activeElement) === true;
  const focusInList = () => listRef.current?.contains(document.activeElement) === true;
  /** The row a recorded origin names: null where nothing was recorded, and null where the row
   *  it names has since left the list. One lookup, for the ways back into the list below. */
  const originRow = (origin: string | null): HTMLElement | null =>
    origin === null
      ? null
      : (listRef.current?.querySelector<HTMLElement>(`[${ROW_ATTRIBUTE}="${origin}"]`) ?? null);
  /** The row `l` was pressed on: the row Escape returns the focus to from inside the column,
   *  which is another row than the selected one whenever `j` moved the selection while the
   *  focus stayed in the list. Every other way into the column leaves it null, or names the row
   *  of the question the column shows (below), and either way Escape returns to that row, as it
   *  always has.
   *
   *  It is written by one key - `read-pane`'s `run` - and only where no record stands: a second
   *  `l`, and the `l` a reader comes back in on after an unmount inside the column left the
   *  record standing, both leave it alone, where either would otherwise write the row the
   *  column is showing over the row the reader came from. So re-entry by key and re-entry by
   *  click, which writes nothing at all, leave a standing record alike. The test is the record
   *  alone, with no check of where the focus is, because none is needed: a press from inside
   *  the column with no record standing finds no focused row there (`focusedRow()` is null in
   *  the column), so it writes the row of the question the column shows, or null where that
   *  question has no listed row, and either sends Escape exactly where `paneWayBack`'s fallback
   *  already would - the stored value differs from a click's null, the row Escape reaches does
   *  not. It is read by one accessor, `paneWayBack`, which both halves of Escape ask, so the
   *  `when` and the `run` cannot disagree about where the key goes.
   *
   *  The record stands exactly while two facts hold of the reader, each with its own drops;
   *  between them no route can outlive the record.
   *
   *  (a) The column still shows the question the record was made on. The layout effect below
   *      drops it wherever `paneSelection.id` has changed under a standing column: a step of
   *      `j` or `k`, the reader's own answer. Where the commit has nothing shown at all the
   *      column itself goes and `holdPane` drops the record first - a snooze of theirs and a
   *      question the server withdraws both take the selected row off the list before the next
   *      one is handed the selection - and that is also the only drop for the Inbox falling
   *      below the column's width, where the selection stands and the column goes.
   *  (b) The reader's focus has not left the column. The document's `focusin` below drops the
   *      record wherever the focus lands outside the wrapper, which is every route out at once:
   *      `f` to the search box, Escape to the row, a Tab onto a row's checkbox, a click into
   *      the list, the row a step out of the column focuses on its way, the dialog a picture
   *      opens full size. `forgetPaneReturn`, the wrapper's `onBlur`, covers the one exit that
   *      lands nowhere and so fires no `focusin`: a focus leaving the column for no node at all
   *      - a `blur()` of the control holding it, a click on the page outside its focusable
   *      `main` - which leaves `document.body` holding the focus. A click on non-focusable
   *      content inside `main` focuses `main` itself, which is a `focusin` outside the wrapper.
   *
   *  Two cases look like a route out and keep the record, because neither breaks that pair. A
   *  window or tab blur moves no focus at all: nothing is focused anew, so no `focusin` fires,
   *  and the `onBlur` that does names no target while the scroller still holds the focus - the
   *  reader is still in the column, and back on the tab Escape owes them the row they came
   *  from. A focused node React removes from inside the column - one body component swapped
   *  for another - while the column still shows the question the record was made on leaves
   *  the reader on that question, so the record is still theirs: React fires no blur for a
   *  node it removes, and the `document.body` the focus falls to is focused by nothing, so
   *  neither drop of (b) reads it. Their next focus does. A control that disables itself is
   *  not that case: Chromium blurs it as it is disabled, before React removes it, so the
   *  wrapper's `onBlur` reads the focus leaving for the body and drops the record, and Escape
   *  then goes to the row of the question the column shows. A control held under the reader's
   *  press while its write is out - the head's Assign to me and Snooze, a thread's Reply and its
   *  box - is never disabled: it is held by `aria-disabled` (`held-press.ts`), the box read-only
   *  as well, so it keeps the focus, and the record stands. */
  const paneReturn = useRef<string | null>(null);
  /** The question the column was showing when this component last looked, which is how the
   *  layout effect below tells a commit that changed it from one that did not, and what the
   *  column's noted focus is told to be in (`paneFocus`). Nothing else writes it, so the record
   *  itself still names a row and nothing more. */
  const shownQuestion = useRef<string | undefined>(undefined);
  /** The element in the column that holds the focus, and the question the column showed as it
   *  took it: what the column's own hand-off reads once a commit has taken that element away
   *  (below `paneScroller`). Noted by the document's `focusin` below, and forgotten wherever the
   *  focus lands outside a column still drawn, wherever it leaves the column for nothing
   *  (`forgetPaneReturn`), and at a step of the selection taken with the focus outside the column
   *  (`move`), so a reader who left the column is owed no hand-off. */
  const paneFocus = useRef<{ element: Element; question: string | undefined } | null>(null);
  // The column shows another question, so the record is spent: the row it names belongs to a
  // question the reader has been carried off, whichever route carried them. Read after every
  // commit, as the selection's own settling is (`pane-selection.ts`), and dropped here rather
  // than tested against the question at Escape, because a record kept past its question would
  // come back the moment the reader selected that question again. On the layout lane, so that
  // the drop lands in the commit that changed the question: React flushes a passive effect
  // before the next key only for the changes it commits synchronously, and the question can
  // change on another lane - a transition, a `?item=` the router settles - which would leave the
  // reader one Escape with a record its question has already left.
  useLayoutEffect(() => {
    if (shownQuestion.current === paneSelection.id) return;
    shownQuestion.current = paneSelection.id;
    paneReturn.current = null;
  });
  /** The column wrapper: the node the keys above act in, and the drop its unmount makes. React
   *  calls a ref callback with null whenever the callback's identity changes, so this one is
   *  stable - an arrow written into the JSX would be a new callback every render, called with
   *  null each time, and would drop a record the reader is still owed. */
  const holdPane = useCallback((node: HTMLDivElement | null) => {
    paneRef.current = node;
    // The column has gone, taking the question the record was made on off the screen and the
    // focus the scroller held to `document.body` with it. Nothing else reports that: no blur
    // fires for a node React removes and `body` takes no `focusin`. So this is the drop that
    // covers the commit a snooze or a withdrawal of the selected question leaves with nothing
    // shown, before the next question is handed the selection - it is first there, ahead of the
    // layout effect above - and the only one that covers the Inbox falling below the column's
    // width, where the selection stands and the column goes.
    if (node === null) paneReturn.current = null;
  }, []);
  // Fact (b), stated as itself: wherever the focus lands, the record stands only while that
  // place is inside the column. A focus the column itself takes - the head's Snooze under `h`,
  // a link in the drawn document, the scroller `l` focuses as it writes the record - lands
  // inside the wrapper and changes nothing, so the key's own `focusin` cannot undo it, and
  // Escape's `run` reads the record before it focuses the row, so the drop that press makes
  // takes nothing it still needs. The same listener notes the element the focus lands on inside
  // the column for the column's own hand-off (`paneFocus`), and forgets it wherever the focus
  // lands outside a column still drawn. While the column is being let go, `paneRef` is already
  // null: React detaches the wrapper's ref before the cleanups of the refs inside it, and the
  // card's form hands the focus to the column's stop from its own (`AskCard`'s `handOffFocus`),
  // a stop that goes with the column. That focus is no reader leaving the column, so the note
  // stands for the column drawn next to hand the focus to its own stop.
  //
  // One listener for this component's life, not one put up and taken down around each record:
  // the record is a ref precisely so that `l` commits nothing, so there is no render to hang a
  // record-shaped subscription on, and making one would mean rendering the Inbox twice per `l`.
  // The handler reads refs alone and nothing of this render, so it never goes stale and the
  // effect has nothing to depend on: it binds once on mount, unbinds on unmount, and no render
  // in between churns it.
  useEffect(() => {
    const followFocus = () => {
      // `focusin` fires once the focus has landed, so the document already names where.
      const focused = document.activeElement;
      if (focused !== null && paneRef.current?.contains(focused) === true) {
        paneFocus.current = { element: focused, question: shownQuestion.current };
        return;
      }
      paneReturn.current = null;
      if (paneRef.current !== null) paneFocus.current = null;
    };
    document.addEventListener("focusin", followFocus);
    return () => document.removeEventListener("focusin", followFocus);
  }, []);
  /** The drop a focus that lands nowhere makes, which is the half of fact (b) no `focusin`
   *  reports: React's `onBlur` bubbles from the column's descendants, so a reader whose control
   *  blurs itself, or who clicks the page outside its focusable `main`, has left the column with
   *  `document.body` holding the focus, and nothing is focused for the listener above to read.
   *
   *  Where the focus went is `relatedTarget`, and `document.activeElement` where the event names
   *  none, because those two nulls mean opposite things: a window or tab blur moves no focus and
   *  keeps the record, while a focus that left for the body drops it. The routes out that land
   *  on a node are this handler's too, an event before the listener above reads them, so what
   *  needs it is only the focus that lands nowhere. The column's noted focus (`paneFocus`) is
   *  dropped with the record, here as in the listener above. */
  const forgetPaneReturn = (event: FocusEvent<HTMLDivElement>) => {
    if (!event.currentTarget.contains(event.relatedTarget ?? document.activeElement)) {
      paneReturn.current = null;
      paneFocus.current = null;
    }
  };
  /** The row Escape returns the focus to from inside the column: the row `l` recorded, else the
   *  row the column is showing, and null where the list holds neither. The one reader of the
   *  record - the binding's `when` tests it against null, its `run` focuses it, and there is no
   *  second spelling of the way back for the two of them to disagree over. */
  const paneWayBack = (): HTMLElement | null =>
    originRow(paneReturn.current) ?? paneSelection.selectedRow();
  /** One step of the reader's, in whichever mode the Inbox is in: the selection the column
   *  follows, or the focus roving the rows. Chosen once, so no binding below asks again. */
  const stepInMode = paneShown ? paneSelection.step : step;
  /** That step, taken from wherever the reader is standing. From inside the column the focus
   *  comes back to the selected row first: `j` fires in there, a `tabindex=0` section being no
   *  editable target, and a step from an approval to a question the column draws as a card
   *  swaps `ApprovalBody` for `CardBody`, which unmounts the focused scroller and drops the
   *  focus to `document.body` - where neither of Escape's branches holds and the key would do
   *  nothing. The reader then stands exactly where a click and then `j` leaves them today. From
   *  anywhere else the column's noted focus is forgotten: a control the column's own code took
   *  away under the focus, with no render of the Inbox's to read that loss, leaves the note on
   *  it, and the step's commit would hand the focus to the next question's stop (below
   *  `paneScroller`), which no loss of this key took. */
  const move = (delta: 1 | -1) => {
    if (paneShown && focusInPane()) {
      paneSelection.selectedRow()?.focus();
    } else {
      paneFocus.current = null;
    }
    stepInMode(delta);
  };
  /** `j` or `k` with the range carried from the anchor to the row the step reaches. It sets no
   *  anchor: with none the list draws, the step only moves. The step moves the selection or the
   *  focus at once, so the row it reached is read from there rather than from this render. */
  const moveInRange = (delta: 1 | -1) => {
    move(delta);
    const id = paneShown ? paneSelection.selectedNow.current : focusedRow()?.dataset.inboxRow;
    if (id !== undefined && byId.has(id)) markRange(id, false);
  };
  /** What the keys act on, for the bindings' labels: the row the column is showing, or the row
   *  holding the focus. */
  const acted = paneShown ? "selected" : "focused";
  /** When the arrows move, and what their label says of it: in the reading column the arrows
   *  are the column's - a document scrolls, an option list moves - so they move the selection
   *  only while the focus is in the list beside it. */
  const arrowsMove = paneShown ? focusInList : () => focusedRow() !== null;
  const arrowsWhen = paneShown ? "the focus is in the list" : "a row is focused";
  /** The row the keys act on: the one the column is showing, or the one holding the focus. */
  const handRow = (): InboxListRow | undefined => {
    const id = paneShown ? paneSelection.id : focusedRow()?.dataset.inboxRow;
    return id === undefined ? undefined : byId.get(id);
  };
  /** The row `x` marks, by its own `rowId`: asks and threads alike, since the bulk bar now offers
   *  each kind its own press. */
  const markableRow = (): string | undefined => {
    const row = handRow();
    return row === undefined ? undefined : rowId(row);
  };
  /** The row `e` dismisses: the thread row in hand, as the route names it - its id and what the
   *  page drew in it. An Ask or an Approval row has no Dismiss at all (`dismissRow`). */
  const dismissableRow = (): InboxDismissRow | undefined => {
    const row = handRow();
    return row === undefined ? undefined : dismissRow(row);
  };
  /** Where the Inbox's keys act: the reading column while it is on - its head's Snooze, owner
   *  link and Copy ref, and the answer form of whatever it is showing - else the row holding
   *  the focus. Null where there is neither, which is an Inbox with nothing selected (a wide
   *  but empty list, or the commit a snooze of the selected row leaves behind) and a list
   *  nothing in is focused. */
  const hand = () => (paneShown ? paneRef.current : focusedRow());
  /** The control `selector` names in whichever of the two that is, or null for none: every
   *  `when` below tests this against `null`, so the accessor never answers `undefined`. */
  const inHand = (selector: string) => hand()?.querySelector<HTMLElement>(selector) ?? null;
  /** The column's scroller, the focus stop `l` moves the reader into, or null where no column
   *  is rendered. */
  const paneScroller = () =>
    paneRef.current?.querySelector<HTMLElement>(PANE_SCROLLER_SELECTOR) ?? null;
  // The column's own hand-off, for the focus its question took with it. A press in the head that
  // takes away the question the column shows - its Snooze, Assign to me under the Unassigned
  // filter, a thread's Dismiss - and a question the server takes away under the reader's focus
  // both take the selected row off the list: the column goes with it, in a commit that draws
  // none, and the selection's settling hands the next row the selection on the layout lane, so
  // the column is drawn again for that row in the same task (`pane-selection.ts`). The focus the
  // column held has fallen to the body, and what now stands where it was is the next question,
  // not what the reader pressed for. So the focus goes to the column's own focus stop, with no
  // scroll, whoever caused the loss, and `j`, `k` and Enter answer from there as they do from the
  // body. It runs on the layout lane, in the commit that draws the column again. A focus the
  // column lost while it still shows the same question - an editor a press opened in place - is
  // none of this hand-off's, and nor is one that has gone somewhere by then. `focusLost`, not the
  // body alone, so an engine that names a removed element focused until its focus fixup is read
  // the same way.
  useLayoutEffect(() => {
    const noted = paneFocus.current;
    if (noted === null || noted.element.isConnected) return;
    const stop = paneScroller();
    const lost = focusLost();
    // The commit that takes the question off the list draws no column; while the list holds a
    // row to hand the selection to, the commit the selection's settling makes draws it.
    if (stop === null && lost && paneShown && presented.current.length > 0) return;
    paneFocus.current = null;
    if (stop !== null && lost && paneSelection.id !== noted.question) {
      stop.focus({ preventScroll: true });
    }
  });
  /** Where the answer keys - Enter and 1-9 - act: the selected question's own card, not the
   *  whole column. The column of a question written as a decision block draws the whole live
   *  document, and `ProofDocument` hosts an answerable card for every open block of it
   *  (`ProofDocument.tsx`), so the first `[data-ask-option]` under the column is the first open
   *  block's, which is another question than the one the reader selected. Null where the
   *  selected row has no card drawn for it: an approval, whose column is the document's
   *  version and whose answer is the head's own Approve, a document whose block has not
   *  mounted its card yet, and every thread row, which asks nothing. Outside the column the
   *  keys act on the focused row, as all of them do. */
  const answerHand = () =>
    paneShown
      ? paneSelection.id === undefined
        ? null
        : (paneRef.current?.querySelector<HTMLElement>(
            `[data-ask-card="${CSS.escape(paneSelection.id)}"]`
          ) ?? null)
      : focusedRow();
  const inAnswerHand = (selector: string) =>
    answerHand()?.querySelector<HTMLElement>(selector) ?? null;
  // The bar is reached through its own ref, which holds wherever the header draws it.
  const bulkPicker = () => bulkBarRef.current?.querySelector<HTMLElement>(BULK_PICKER_SELECTOR);
  // Which row `h` was pressed on, so Escape from the bulk picker - which sits above the bands and
  // has no row to fall back through - is one level out rather than a dead end.
  const pickerOrigin = useRef<string | null>(null);
  useKeymapScope(keymapScope);
  const inboxKeys: KeyBinding[] = [
    // Movement is what the arrow keys are for; a palette row that moves the cursor helps nobody.
    // With the reading column on, movement is the selection's: the column follows it, and the
    // focus stays where the reader left it in the list.
    {
      id: "next",
      keys: "j",
      label: "Next row",
      palette: false,
      run: () => move(1),
      when: () => rows().length > 0,
    },
    {
      id: "previous",
      keys: "k",
      label: "Previous row",
      palette: false,
      run: () => move(-1),
      when: () => rows().length > 0,
    },
    {
      id: "arrows",
      keys: ["ArrowDown", "ArrowUp"],
      label: `Next / previous row while ${arrowsWhen}`,
      run: (event) => move(event.key === "ArrowDown" ? 1 : -1),
      when: arrowsMove,
    },
    {
      id: "answer",
      keys: "Enter",
      label: `Answer the ${acted} ask`,
      run: () => {
        // A decision block's compact card folds its answer box behind "Add a note or answer in
        // your own words"; Enter presses that disclosure, and the card lands the focus in the
        // box its own render reveals (`AskCard.tsx`) - the box does not exist in this tick, so
        // nothing here could focus it. Only the column draws that compact card: from a row, and
        // from a disclosure already open, the key goes straight to the box.
        const box = inAnswerHand("[data-ask-answer]");
        if (box === null && paneShown) {
          inAnswerHand(NOTE_DISCLOSURE_SELECTOR)?.click();
          return;
        }
        box?.focus();
      },
      // Enter is the list's key: it carries the reader from the row they are on into the
      // column's answer box. Everywhere else the key belongs to the control that holds the
      // focus - the card's own `Ask back instead` or an option inside the column, a row's owner
      // link, a band's fold, the bulk-snooze bar, a filter control - and the registry's handler
      // calls `preventDefault` for every binding that fires, so a binding that claimed the key
      // there would press nothing. It fires from a row itself, with the focus on nothing
      // (`document.body`), where a reader who has pressed no key yet stands, and from the
      // column's own focus stop, where the column's hand-off puts a focus its question took with
      // it, so the key opens the question shown next as it would from the body. `h` stands down
      // inside an ask card for the same reason (`outsideAskCard`).
      when: () =>
        paneShown
          ? (focusedRow() !== null ||
              document.activeElement === document.body ||
              document.activeElement === paneScroller()) &&
            (inAnswerHand("[data-ask-answer]") !== null ||
              inAnswerHand(NOTE_DISCLOSURE_SELECTOR) !== null)
          : inAnswerHand("[data-ask-answer]") !== null,
    },
    {
      id: "option",
      keys: ["1", "2", "3", "4", "5", "6", "7", "8", "9"],
      label: `Select option 1–9 of the ${acted} ask`,
      run: (event) => {
        const options = answerHand()?.querySelectorAll<HTMLElement>("[data-ask-option]");
        options?.[Number(event.key) - 1]?.click();
      },
      when: () => inAnswerHand("[data-ask-option]") !== null,
    },
    {
      id: "open",
      keys: "o",
      label: "Open the row's issue, document or thread",
      run: () => inHand("[data-inbox-owner]")?.click(),
      when: () => inHand("[data-inbox-owner]") !== null,
    },
    {
      id: "copy-ref",
      keys: "y",
      // An Agent row has no reference of its own - its thread is a message to a session - so
      // it draws no copy control and this stands down on it.
      label: `Copy the ${acted} row's reference`,
      run: () => inHand(COPY_REF_SELECTOR)?.click(),
      when: () => inHand(COPY_REF_SELECTOR) !== null,
    },
    {
      id: "back",
      inEditable: true,
      keys: "Escape",
      label: "Back to the row, then clear the focused row",
      run: () => {
        // From the reading column, Escape is the way back to the row the reader came from: `l`
        // recorded that row, which is not the selected one whenever `j` moved the selection
        // without moving the focus. Every other way in records nothing, or the row the column
        // is showing, and returns, as it always has, to that row. `paneWayBack` is both of
        // those, and the `when` below asks it the same question, so the key never fires on a way
        // back the run then cannot find.
        if (paneShown && focusInPane()) {
          paneWayBack()?.focus();
          return;
        }
        const row = rowAround(document.activeElement);
        if (row === focusedRow()) {
          row?.blur();
        } else {
          row?.focus();
        }
      },
      // Both ways back are tested, because either can be the only one left. The column keeps a
      // question whose row the typed text hides while the reader has an unsent answer in it
      // (`pane-selection.ts`'s `paneAsk`), and there the row `l` was pressed on is the way out,
      // the selected row having none; every other way into the column records no row, and there
      // the selected row is the way out. With only the selected row tested, Escape in a column
      // held open over a hidden row would match no binding and do nothing.
      when: () =>
        rowAround(document.activeElement) !== null ||
        (paneShown && focusInPane() && paneWayBack() !== null),
    },
    {
      // The column is the other half of the page, and `l` is the step into it: the scroller
      // takes the focus, and the browser scrolls it on the arrow keys, Page Up, Page Down,
      // Space, Home and End from there.
      id: "read-pane",
      keys: "l",
      label: "Move the focus into the reading pane",
      run: () => {
        // The record Escape reads, written only where no record stands. It names the row the
        // reader came from - the one holding the focus, or the selected one where the focus is
        // on nothing (`document.body`, where a reader who has pressed no key yet stands) or on
        // something that is not a row, which includes a press from inside the column itself.
        //
        // A press with a record standing writes nothing, whether the reader is in the column
        // already or on the body an unmount inside the column dropped their focus to: both
        // would write the row the column is showing, or null, over the row the reader actually
        // came from, which is the row Escape owes them. So the key leaves a standing record
        // exactly as a click back into the column does, and the two ways back in agree.
        if (paneReturn.current === null) {
          paneReturn.current =
            focusedRow()?.dataset.inboxRow ?? paneSelection.selectedRow()?.dataset.inboxRow ?? null;
        }
        paneScroller()?.focus();
      },
      // One test for all three cases the key does nothing in: below 1280 px and in the drawer
      // `paneShown` is false (`pane && wideEnoughForPane`), and with no row selected no column
      // is rendered at all, so `paneRef.current` is null and there is no scroller under it.
      when: () => paneShown && paneScroller() !== null,
    },
    {
      // Marking walks the list with the row in hand, as `j`/`k` do, so it is no palette row.
      id: "select",
      keys: "x",
      label: `Select or deselect the ${acted} row`,
      palette: false,
      run: () => {
        const id = markableRow();
        if (id !== undefined) onMark(id);
      },
      when: () => markableRow() !== undefined,
    },
    {
      // The range from the anchor to the row in hand, as a shift-click on its checkbox takes it.
      // It walks the list with the row in hand as `x` does, so it is no palette row either.
      id: "select-range",
      keys: "Shift+X",
      label: `Select or deselect the rows from the last one marked or unmarked without Shift to the ${acted} row`,
      palette: false,
      run: () => {
        const id = markableRow();
        if (id !== undefined) markRange(id, true);
      },
      when: () => markableRow() !== undefined,
    },
    {
      id: "next-in-range",
      keys: "Shift+J",
      label: "Next row, taking the range to it",
      palette: false,
      run: () => moveInRange(1),
      when: () => markableRow() !== undefined,
    },
    {
      id: "previous-in-range",
      keys: "Shift+K",
      label: "Previous row, taking the range to it",
      palette: false,
      run: () => moveInRange(-1),
      when: () => markableRow() !== undefined,
    },
    {
      id: "dismiss",
      keys: "e",
      // The row goes out of the Inbox, and in the Dismissed view back into it; the selection then
      // moves to the next row, as it does after a snooze. An Ask or an Approval row has no
      // Dismiss, so the key does nothing on one.
      label: showDismissed
        ? `Move the ${acted} thread back to the Inbox`
        : `Dismiss the ${acted} thread`,
      run: () => {
        const named = dismissableRow();
        if (named !== undefined) dismissRows([named]);
      },
      // Not from inside an ask the reader is answering, the rule `h` holds to: the card's own
      // controls are not the list's, and a typed `e` belongs to whatever holds the focus.
      when: () => dismissableRow() !== undefined && (!paneShown || outsideAskCard()),
    },
    {
      id: "snooze",
      keys: "h",
      // One key for both: with rows marked it is the whole selection, otherwise the row in hand.
      label: "Snooze the focused ask, or every selected ask",
      run: () => {
        if (selectedAsks.length === 0) {
          // The selected short row carries no Snooze of its own: the reading column's head does,
          // for the row it is showing.
          inHand(ROW_SNOOZE_SELECTOR)?.focus();
          return;
        }
        bulkPicker()?.focus();
      },
      // The selection's key works from wherever marking leaves the reader - a row, the checkbox
      // they just ticked, the bar - but not from inside an ask they are answering: an option
      // radio takes no typed text, so the registry passes the key through, and the card's own
      // controls are not the list's. The issue page withholds its writing keys the same way.
      // Inside the column that rule covers the lone Snooze too, the card there being the
      // column's own.
      when: () =>
        (selectedAsks.length > 0 && outsideAskCard()) ||
        (inHand(ROW_SNOOZE_SELECTOR) !== null && (!paneShown || outsideAskCard())),
    },
    {
      id: "back-from-bulk",
      inEditable: true,
      keys: "Escape",
      label: "Back to the list from the bulk snooze picker",
      run: () => {
        // The row `h` was pressed on, else the first row, passing over every row the bar's own
        // Snooze write is over while that write is pending (`bulkSnoozing`, which the bar empties
        // once the write settles, a refusal or a stop included). The list keeps such a row in its
        // band only until the write lands, and its move into a folded Later then is no press's
        // doing (`causedByReader`), so a focus put on it would fall to the body. With no drawn
        // row left that the bar's write is not taking away, the bar itself is the level out: it
        // holds the count and the way to clear it.
        (
          [originRow(pickerOrigin.current), ...rows()].find(
            (row) => row !== null && !bulkSnoozing.has(row.dataset.inboxRow ?? "")
          ) ?? bulkBarRef.current
        )?.focus();
      },
      when: () => document.activeElement?.matches(BULK_PICKER_SELECTOR) === true,
    },
    {
      id: "clear-selection",
      keys: "Escape",
      // Escape is one level out, and the selection is the outermost thing a row press made:
      // `back` takes the reader off the row first, and this clears what they marked. It is
      // offered exactly while the bar's Clear is, a pick in flight and a refusal the reader has
      // since unticked the rows of included - the keyboard that raised them dismisses them. Not a
      // palette row: in the palette Escape closes the palette, so the row would name a key that
      // does something else there.
      label: "Clear the selection",
      palette: false,
      run: clearSelection,
      // A popover of the bar's takes Escape first: one press closes it, and dropping what the
      // reader marked at the same time would be two undos for one key.
      when: () => focusedRow() === null && bulkBarShown && !barOpen,
    },
    {
      // The box is one key away from anywhere on the page, and `f` is not `inEditable`: inside
      // the box itself a `search` input takes typed text (`keymap.ts`'s `TEXT_INPUT_TYPES`), so
      // the registry passes the key through and the reader types an `f` rather than re-focusing
      // what they are already in.
      id: "search",
      keys: "f",
      label: "Search asks",
      run: () => searchRef.current?.focus(),
      when: () => searchRef.current !== null,
    },
    {
      // A message to one or more agents, from the page and from the drawer alike. In a text field
      // the key types the letter, as every single-key binding does.
      id: "compose",
      keys: "n",
      label: "Write a message to one or more agents",
      run: onCompose,
    },
    {
      // Escape is one level out, and out of the box is the list: the row the column is showing
      // on the page, and the first row in the drawer, where nothing is selected. It is offered
      // only while the focus is in the box, so the three Escapes above are untouched - `back`
      // and `back-from-bulk` both answer false there, and `clear-selection` is not `inEditable`
      // at all - and the drawer's own Close waits its turn, the Inbox's registration being the
      // first of the scope to match (`keymap.ts`'s `resolve`).
      id: "leave-search",
      inEditable: true,
      keys: "Escape",
      label: "Back to the list from the search box",
      run: () => (paneSelection.selectedRow() ?? rows()[0])?.focus(),
      when: () => searchRef.current !== null && document.activeElement === searchRef.current,
    },
  ];
  // While the compose dialog is open, every key is the dialog's: in the drawer this list's keys
  // stand in the dialog scope beside the dialog's, where `j` or Escape would act on the list
  // under it, so the list lets its keys go until the dialog closes.
  useKeymap(keymapScope, composing ? [] : inboxKeys);

  // The bar's hand-off where the row marked last cannot take the focus, with no scroll. After a
  // press that took that row away: a row the reader can see at its place, else before it. With no
  // row marked: the first row the reader can see below whatever the page draws over the list.
  // Else the list column's own stop. A row a Dismiss or a Move to Inbox press is taking off this
  // view is passed over: it is still drawn until the write's optimistic apply lands, and a focus
  // handed to it would drop with it. `dismissedHere` names every such row - each id the dismiss
  // write's `pending` holds, and the ids of a press made under the narrowing, which `pending`
  // leaves out. Each row after such rows is judged where it will stand once they have gone, as
  // the list's anchor lets them go (`leaving` below); where their going leaves the row at the
  // place wholly above the view, the nearest row after it in sight takes the focus.
  const landAtPlace = useLastMarkedPlace({
    control: (id) => originRow(id)?.querySelector<HTMLElement>(MARK_SELECTOR) ?? null,
    lastMarked,
    leaving: dismissedHere,
    list: () => listScroller.current,
    order: () => rows().map((row) => row.dataset.inboxRow ?? ""),
    shown: bulkBarShown,
  });

  // The bar's Snooze is held from its pick through the commit after its write's answer
  // (`useHeldThroughAnswer`), as a row's own Snooze is, and the hold keeps the armed hand-off of
  // the pick that made the write live (`holdHandOff`). So the bar's going when the write lands,
  // with the rows it snoozed, is the reader's doing, and its hand-off lands the focus: at the
  // reading column's width the short rows draw no Snooze of their own to hold the pick.
  const bulkSnoozeHeld = useHeldThroughAnswer(bulkSnoozing.size !== 0);

  // The list waits for every read its asks come from, and says it could not load where one
  // failed, rather than drawing a list without the rows that read answers for. The header line
  // stands over it in every state, first in one fragment, so the line is the same element from
  // the first draw on: the drawer's close button keeps the focus the dialog gave it as the list
  // lands, and a reader who pressed `f` while the list loaded keeps typing in the same box.
  if (whoAmI.isPending || inboxAsks.pending) {
    return (
      <>
        {headerLine}
        <LoadingSkeleton label="Loading your inbox" />
      </>
    );
  }
  if (inboxAsks.failed || viewer === undefined || place === undefined) {
    return (
      <>
        {headerLine}
        <p className={dangerText}>Could not load your inbox.</p>
      </>
    );
  }

  // Which row the list holds in place while the server moves the list under it: the row the
  // reader's hand is on (focus or pointer), with or without the reading column, and a row whose
  // Assign to me, hand-on, answer, decline, question back or reply is out, whatever the selection
  // does. A row the column is merely showing is not held - the Inbox selects the first row for
  // the reader the moment it opens, and a question the server withdraws, resolves or answers
  // elsewhere must not stay in the list on the strength of a selection nobody made. What the
  // column does with a selected question the server takes away is `paneRow`
  // (`pane-selection.ts`). A row the reader dismissed under their own hand is not held either:
  // they asked for it to go, so it goes at once and the next row takes its place; and a row they
  // answered goes once the server has recorded the answer.
  //
  // The hold is told which rows the typed text excludes rather than being handed the narrowed
  // list: a row the reader's own typing drops must go at once, even with the pointer resting on
  // it, so it is not "seen" here at all and there is nothing to hold. The words are tested only
  // where they can have dropped it: on a row the list did not hold at its last render, or once
  // the reader's words have changed since. A row held under the words the reader still has stays
  // held though the server changes the text they search, since only the reader's own typing
  // drops a row at once. A row the server stopped sending is held exactly as it is today.
  //
  // The reader's own recorded answer lets its row go from under their hand once. The record of it
  // (`answered`) ends once the row has left the list, and once a further answer or decline of
  // theirs on the ask is out: a landing records it again, and a refusal or a failure leaves the
  // row where it is. A row of that ask the list holds after either is held as any other row is,
  // and does not leave from under the hand at once as if their answer took it.
  const answeredId = answered.current;
  if (
    answeredId !== null &&
    (answersOut.has(answeredId) ||
      declinesOut.has(answeredId) ||
      !presented.current.some((placed) => rowId(placed.row) === answeredId))
  ) {
    answered.current = null;
  }
  const handId = anchor?.id;
  const typed = words.join(" ");
  const heldBefore = heldUnder.current;
  const heldUnderWords =
    heldBefore !== undefined && heldBefore.id === handId && heldBefore.words === typed;
  const seen =
    handId === undefined || handId === answered.current || dismissedHere(handId)
      ? undefined
      : presented.current.find(
          (placed) =>
            rowId(placed.row) === handId &&
            (heldUnderWords || matchesInboxSearch(placed.row, words))
        );
  // A row a snooze of the reader's is out on - its own write, or the bar's over the rows they
  // marked - is held in the band they saw it in until the Inbox lets go of it once the write
  // lands (`heldRow`), while the list still lists it. A refused or stopped write holds nothing.
  // Under the Blocked on you narrowing, which lists no snoozed ask, the row is dropped at once,
  // and only the reader's hand holds it. A row whose Assign to me, hand-on, answer, decline,
  // question back or reply is out is held as the row under the hand is, listed or not, until
  // that write lands.
  // That band is a band of the layout the row was drawn under, so a snooze holds its row only
  // while the list is still drawn under it - the same grouping, and the same view. Under that
  // layout the band a write holds its row in is drawn again though no other row is left in it
  // (`keptBands`): a snooze takes its row out of the band whoever else is there, and an answer,
  // a decline, an Assign to me or a hand-on the filters drop takes it out of the list. So is the
  // band the reader's hand holds a listed row in that has moved to another band, as a pick of the
  // row's own priority moves it: drawn in no band, the row would leave the page with the
  // reader's focus in it.
  const layout = showDismissed ? "dismissed" : groupBy;
  const sameLayout = presentedBands.current.layout === layout;
  const held: PlacedRow[] = [];
  const keptBands = new Set<string>();
  // The Reply rows this list last drew whose reply is out, by id. The anchor is told they are
  // leaving, as it is told of the rows an ask write keeps.
  const replying = new Set<string>();
  let handHeld: PlacedRow | undefined;
  for (const placed of presented.current) {
    const id = rowId(placed.row);
    const inHand = seen !== undefined && rowId(seen.row) === id;
    const out = sameLayout && (snoozing.has(id) || bulkSnoozing.has(id));
    const written =
      placed.row.kind === "ask"
        ? askWriteOut(id)
        : placed.row.thread.kind === "ask" && repliesOut.has(placed.row.thread.thread_id);
    if (written && placed.row.kind !== "ask") replying.add(id);
    if (!inHand && !out && !written) continue;
    const listed = byId.get(id);
    const kept = heldRow(
      placed,
      listed,
      // A held row dropped from the list is drawn as its candidate, the row as the Inbox holds
      // it now, so a pick in it shows there.
      (inHand || written) && listed === undefined
        ? candidates.find((row) => rowId(row) === id)
        : undefined,
      listed === undefined
        ? undefined
        : bands.find(({ rows: banded }) => banded.includes(listed))?.key,
      { kept: inHand || written, snoozeOut: out }
    );
    if (kept === undefined) continue;
    held.push(kept);
    if (sameLayout && ((inHand && listed !== undefined) || out || written)) {
      keptBands.add(kept.group);
    }
    if (inHand) handHeld = kept;
  }
  heldUnder.current =
    handHeld === undefined ? undefined : { id: rowId(handHeld.row), words: typed };

  // The filtered agent as its asks wrote it (the title it stamped, its attribution), or only its
  // id when none of its asks is listed; either way its registry title names it when listed.
  // Without the filter, `fromAgent` is every row, so it names no agent.
  const agentAuthor = agent === undefined ? undefined : fromAgent[0]?.author;
  const agentSubject =
    agent === undefined
      ? undefined
      : agentAuthor === undefined
        ? { sessionId: agent, storedTitle: undefined }
        : { actor: agentAuthor };
  const agentTitle = agentSubject === undefined ? undefined : agentText.part(agentSubject);
  // Taking the agent narrowing off leaves for the Inbox of the same assignees, selection and
  // search text, with every other narrowing, the order, the banding and the view at their
  // defaults. On the page the move pushes a history entry, as following a link does, so Back
  // gives the agent's asks back. A host that owns the filter (the drawer) is told of the move
  // instead, as it is of every other chip's removal.
  const clearAgent = () => {
    const next: InboxRoute = { assignees: filter.assignees, item: filter.item, q: filter.q };
    if (onFilterChange) onFilterChange(next);
    else navigate(buildInboxPath(next), { replace: false });
  };
  // The agent narrowing's chip, the first of the bar's chips: the bar's shared removable chip,
  // named for what its press does as every chip is, with the agent's live link beside it and
  // outside it (`FilterChip.link`).
  const agentChip: FilterChip | undefined =
    agent === undefined || agentTitle === undefined
      ? undefined
      : {
          key: "agent",
          label: `Asks from ${agentTitle}`,
          link: {
            label: `Open live conversation with ${agentTitle}`,
            to: buildReferencePath({ kind: "agent-session", session: agent }),
          },
          remove: clearAgent,
        };

  const chooseField = (field: InboxFilterField, chosen: readonly string[]) => {
    const next: InboxFilters = { ...filters };
    if (chosen.length === 0) delete next[field];
    else next[field] = chosen;
    changeFilter({ ...filter, filters: Object.keys(next).length === 0 ? undefined : next });
  };
  const chooseAssignees = (next: readonly string[] | undefined) =>
    changeFilter({ ...filter, assignees: next });

  /**
   * The five controls the one **Filter** panel holds: the four fields the address carries as
   * comma lists, and who the list is for. Each row carries how many rows that value holds.
   *
   * Built here rather than above, and handed to `PanelTrigger` as the render prop it takes, so
   * that a shut panel costs nothing: every count below walks the candidate rows once per field,
   * and the reader scrolling a list with the panel shut must pay for none of it.
   */
  const renderFilterControls = (): ReactNode => {
    // What each filter control counts: the rows that every filter that is on keeps, but for that
    // control's own, so a reader opening Priority is told what each priority holds under the
    // filters they have already set. The typed text is no filter here, as on `main`: the counts
    // leave it out, as Clear filters leaves it standing, so a value whose rows the text hides
    // still counts them.
    const matchesOtherFields = (row: InboxListRow, field: InboxFilterField | "assignee"): boolean =>
      INBOX_FILTER_FIELDS.every(
        (other) => other === field || inFieldFilter(row, other, filters[other], viewer)
      ) &&
      (field === "assignee" || inAssigneeFilter(row, selection, viewer));
    // A login equal to either reserved word would be a row that means the filter rather than
    // the person, so neither is offered among the people.
    const reservedLogins: readonly string[] = [UNASSIGNED_FILTER_VALUE, EVERYONE_FILTER_VALUE];
    const listedPeople = [
      UNASSIGNED_FILTER_VALUE,
      ...(people.data ?? [])
        .map((person) => person.login)
        .filter((login) => !reservedLogins.includes(login)),
    ];
    // A selected value the list does not hold - a person who has not signed in, or anyone at all
    // while the request is out or refused - is still a row, so a filter read from the address
    // always shows what it is filtering by and the reader can switch it off.
    const assigneeOptions = [
      ...listedPeople,
      ...selection.filter((value) => !listedPeople.includes(value)),
    ];
    const assigneeCounts = filterCounts(
      candidates,
      "assignee",
      (row) => matchesOtherFields(row, "assignee"),
      viewer
    );
    return (
      <>
        {INBOX_FILTER_FIELDS.map((field) => {
          const chosen = filters[field] ?? [];
          const counts = filterCounts(
            candidates,
            field,
            (row) => matchesOtherFields(row, field),
            viewer
          );
          // A count is drawn only once the read of the rows it counts has answered. In the
          // Dismissed view those rows are the pages read so far, the rows the list holds and a
          // filter narrows, so a count there counts those pages while **Show more** has more to
          // read. An ask kind of Type counts the asks this list holds, whose reads the Inbox waits
          // for before it draws this bar at all (`useInboxAsks`), and the Dismissed view holds no
          // ask, so that count stands in either view. A thread kind counts the threads of the view
          // that is showing: the Dismissed view's come from the one read's `dismissed` state, which
          // the bar does not wait for. Every other field counts asks and threads together. So
          // while the threads' read is out or refused (`threads.answered`), the ask kinds keep
          // their count and every other value is offered with no count, never a 0 or an asks-only
          // count.
          const counted = (value: string) =>
            threads.answered ||
            (field === "type" && !(INBOX_THREAD_KINDS as readonly string[]).includes(value));
          return (
            <MultiSelect
              {...filterPanel(field)}
              emptyMessage={`No matching ${DIMENSION_LABELS[field].toLowerCase()}.`}
              key={field}
              label={DIMENSION_LABELS[field]}
              onChange={(next) => chooseField(field, next)}
              optionDetail={(value) => (counted(value) ? `${counts.get(value) ?? 0}` : undefined)}
              optionDetailLabel={(value) =>
                counted(value) ? `${counts.get(value) ?? 0} rows` : undefined
              }
              optionLabel={(value) => dimensionValueLabel(field, value)}
              options={fieldOptions(field, candidates, viewer, chosen)}
              searchLabel={`Search ${DIMENSION_LABELS[field].toLowerCase()}`}
              selected={chosen}
              triggerAriaLabel={`Filter by ${DIMENSION_LABELS[field].toLowerCase()}`}
            />
          );
        })}
        {/* Who the list is listed for (`?assignee=`): the trigger says the selection in the
            reader's own words, and the popover picks any number of people, with Unassigned
            first. Unassigned lists what the server resolved no person for, and every project
            naming a default assignee leaves nothing: it reads 0 and lists nothing, which is
            what the filter saying so is for. A person's count counts asks and threads, so it is
            drawn only once the threads' read has answered, as every field's above is; until
            then the reader's own row keeps its "you" alone. */}
        <MultiSelect
          emptyMessage={people.isPending ? "Loading people…" : "No matching person."}
          error={people.isError ? "Could not load the people list." : null}
          label="Assignee"
          onChange={chooseAssignees}
          onOpenChange={(open) => {
            if (open) setPeopleWanted(true);
            filterPanel("assignee").onOpenChange(open);
          }}
          open={filterPanel("assignee").open}
          optionDetail={(value) => {
            const you = value === viewer;
            if (!threads.answered) return you ? "you" : undefined;
            return `${you ? "you · " : ""}${assigneeCounts.get(value) ?? 0}`;
          }}
          optionDetailLabel={(value) => {
            // While the thread read is out, the detail as drawn names the row: "you" or none.
            if (!threads.answered) return undefined;
            return `${value === viewer ? "you, " : ""}${assigneeCounts.get(value) ?? 0} rows`;
          }}
          optionLabel={(value) => (value === UNASSIGNED_FILTER_VALUE ? "Unassigned" : value)}
          options={assigneeOptions}
          searchLabel="Search people"
          selected={selection}
          triggerAriaLabel="Filter by assignee"
        >
          {assigneeTriggerText(selection, viewer)}
        </MultiSelect>
      </>
    );
  };

  const tokens: FilterChip[] = [];
  for (const field of INBOX_FILTER_FIELDS) {
    for (const value of filters[field] ?? []) {
      tokens.push({
        key: `${field}:${value}`,
        label: `${DIMENSION_LABELS[field]}: ${dimensionValueLabel(field, value)}`,
        remove: () =>
          chooseField(
            field,
            (filters[field] ?? []).filter((current) => current !== value)
          ),
      });
    }
  }
  if (filter.assignees !== undefined) {
    if (filter.assignees.length === 0) {
      tokens.push({
        key: `assignee:${EVERYONE_FILTER_VALUE}`,
        label: "Assignee: everyone",
        remove: () => chooseAssignees(undefined),
      });
    }
    for (const value of filter.assignees) {
      tokens.push({
        key: `assignee:${value}`,
        label: `Assignee: ${value === UNASSIGNED_FILTER_VALUE ? "Unassigned" : value}`,
        remove: () => {
          const kept = (filter.assignees ?? []).filter((current) => current !== value);
          chooseAssignees(kept.length === 0 ? undefined : kept);
        },
      });
    }
  }
  // The bar's chips: the agent's first, then each field's values and who the list is for. The
  // switch's choice is no narrowing of the panel's, so it draws no chip and is not counted.
  const chips: readonly FilterChip[] = agentChip === undefined ? tokens : [agentChip, ...tokens];
  // **Clear filters**, given while a chip is on: every narrowing of the Filter panel off at once
  // - the four fields, who the list is for and the agent. What the reader typed, the order, the
  // banding, which of the two lists is showing and the switch's choice are not narrowings of
  // the panel and stay exactly as they are, so `section=needs-you` stays in the address.
  const clearFilters =
    chips.length === 0
      ? undefined
      : () =>
          changeFilter({ ...filter, agent: undefined, assignees: undefined, filters: undefined });

  // The switch's Blocked on you: the asks waiting on the reader plus the credential requests they
  // may decide, with the oldest one's age in its accessible name and its tooltip. Choosing it
  // narrows the list to exactly those asks and leaves the credential requests above it: it takes
  // off the four fields, the assignee and the search text with the section it writes, so the
  // list holds every ask the number counts, and keeps the sort, the grouping and the view;
  // choosing All takes the section off alone. Its number is drawn only once the credential
  // requests and the sign-in have answered, and never at zero: until then, or while the
  // requests' first read has failed, the part is drawn without it, since a number that counted
  // an unread list as empty would be no number of the reader's. No switch is drawn beside an
  // agent narrowing or in the Dismissed view, where a count over the whole inbox would
  // contradict the list under it.
  const waiting = needsYou(adoptedInbox ?? [], credentials.requests, viewer);
  const waitingCounted = credentials.status === "listed" && viewer !== undefined;
  const blockedOnYou = filter.section === "needs-you";
  const inboxSwitch: SegmentedControlProps | undefined =
    agent !== undefined || showDismissed
      ? undefined
      : {
          label: "Inbox section",
          onChange: (value) => {
            const chosen = value === "needs-you";
            if (chosen === blockedOnYou) return;
            changeFilter(
              chosen
                ? {
                    ...filter,
                    assignees: undefined,
                    filters: undefined,
                    q: undefined,
                    section: "needs-you",
                  }
                : { ...filter, section: undefined }
            );
          },
          parts: [
            { label: "All", value: "all" },
            {
              count: waitingCounted ? waiting.count : undefined,
              label: "Blocked on you",
              short: "You",
              title: blockedOnYouText(waitingCounted ? waiting : undefined),
              value: "needs-you",
            },
          ],
          value: blockedOnYou ? "needs-you" : "all",
        };

  // The bar's one action, kept in **More** at every width: only while the list holds an unread
  // Reply or Agent row. It asks nothing first: the rows go, and a refusal brings them back; a
  // press the reader stopped may have marked them, so the list is read again rather than brought
  // back. What a refused or stopped press says stands under the bar.
  const actions: readonly BarActionProps[] = markAll.offered
    ? [{ icon: MARK_ALL_GLYPH, label: "Mark all read", more: true, onPress: markAll.markAll }]
    : [];
  const markAllFailure =
    markAll.failure === "refused" ? (
      <p className={`text-sm ${dangerText}`} data-inbox-mark-all-refused="" role="alert">
        {MARK_ALL_REFUSED_TEXT}
      </p>
    ) : markAll.failure === "stopped" ? (
      <p className={`text-sm ${dangerText}`} data-inbox-mark-all-stopped="" role="alert">
        {MARK_ALL_STOPPED_TEXT}
      </p>
    ) : null;

  // How the list is ordered, how it is banded, which of the reader's two lists is showing, and
  // which way a thread's own replies read. The first three are the list's, so they ride in the
  // address and a link carries them; the thread order is the reader's own and is kept for their
  // browser (`threads/thread-order.ts`). Choosing the Dismissed view takes `section=needs-you`
  // off, since an ask is never dismissed and that list would hold nothing under it.
  const displayMenu: DisplayMenuProps = {
    label: "View",
    onOpenChange: setViewOpen,
    open: viewOpen,
    sections: [
      {
        id: "sort",
        label: "Sort by",
        onChange: (next) => {
          const picked = pick(INBOX_SORTS, next);
          if (picked !== undefined) changeFilter({ ...filter, sort: picked });
        },
        options: menuOptions(INBOX_SORTS, SORT_LABELS),
        value: sort,
      },
      {
        id: "group",
        label: "Group by",
        onChange: (next) => {
          const picked = pick(INBOX_GROUP_BYS, next);
          if (picked !== undefined) changeFilter({ ...filter, group: picked });
        },
        options: menuOptions(INBOX_GROUP_BYS, GROUP_LABELS),
        value: groupBy,
      },
      {
        id: "show",
        label: "Show",
        onChange: (next) =>
          changeFilter(
            next === "dismissed"
              ? { ...filter, section: undefined, show: "dismissed" }
              : { ...filter, show: undefined }
          ),
        options: menuOptions(INBOX_SHOWS, SHOW_LABELS),
        value: show,
      },
      {
        id: "order",
        label: "Thread order",
        onChange: (next) => {
          const picked = pick(THREAD_ORDERS, next);
          if (picked !== undefined) setThreadOrder(picked);
        },
        options: menuOptions(THREAD_ORDERS, THREAD_ORDER_LABELS),
        value: threadOrder,
      },
    ],
  };

  // The bar outlives an empty list: a pick that empties this view leaves its write in the air,
  // and the refusal that comes back has to land somewhere the reader can see. It is drawn over
  // the list rather than in it, so marking a row moves no row. It is drawn always, and given no
  // count and no state while it is not shown, so its going is the bar's own to see and the focus
  // it held is handed on (`SelectionBar`).
  const dismissLabel = showDismissed ? "Move to Inbox" : "Dismiss";
  const selectedCount = selectedAsks.length + selectedThreads.length;
  // In flight the count is the press's. While Later is folded the optimistic moment has taken
  // its rows out of the rows the selection counts as drawn (`drawnIds`), though the list keeps
  // each where the reader saw it until the write lands. Under the Blocked on you narrowing it
  // has taken them out of the list. Settled, the count is what a second press would act on: the
  // rows still marked. Said here rather than left to the bar's own count so the bar still stands
  // over an emptied selection, which is where a refusal of that press lands.
  let barState: string | undefined;
  if (!bulkBarShown) barState = undefined;
  else if (bulkSnoozing.size !== 0) barState = `Snoozing ${bulkSnoozing.size}…`;
  else if (allRows) barState = "Every row selected";
  else barState = `${selectedCount} selected`;
  // What a screen reader hears of the selection, a range's included: the count, in words of its
  // own rather than the bar's "N selected", from a region the page draws before the first mark,
  // so the first change is heard. `sr-only`, so it moves no pixel.
  const spokenCount = bulkBarShown ? selectedCount : 0;
  const selectionStatus = (
    <p className="sr-only" data-inbox-selection-count="" role="status">
      {spokenCount === 0 ? null : `${spokenCount} ${spokenCount === 1 ? "row" : "rows"} selected`}
    </p>
  );
  const selectionBar = (
    <SelectionBar
      barRef={bulkBarRef}
      count={bulkBarShown ? selectedCount : 0}
      label="Selected rows"
      notice={snoozeRefusal}
      // The bar gone with the focus, or the control in it that held the focus: the checkbox of
      // the row the reader marked last while the list draws it and no press is taking it away,
      // brought into view. Else the hand-off above (`landAtPlace`) has put the focus itself,
      // with no scroll, after a press that took that row away or with no row marked. The list
      // column's own stop is the last place the focus can go.
      returnFocus={() =>
        landAtPlace()
          ? null
          : (originRow(lastMarked())?.querySelector<HTMLElement>(MARK_SELECTOR) ??
            focusListStop(listScroller.current))
      }
      size={BAR_SIZE}
      state={barState}
    >
      {/* The picker stays while its own write is out. While Later is folded, the optimistic
          moment takes every marked ask out of the asks the selection counts (`selectedAsks`),
          and under the Blocked on you narrowing out of the list. A picker unmounted under the
          reader's hand drops their focus. */}
      {selectedAsks.length === 0 && bulkSnoozing.size === 0 ? null : (
        // One pick at a time: a second while the first is in the air re-sends every id, and its
        // answer can settle after the live one's and overwrite what the reader was told. It is
        // held by `aria-disabled` rather than `disabled` because the browser takes focus off a
        // control that disables itself under it - dropping the reader on the document mid-write,
        // with no Escape to leave and no way back to the bar. The focus arriving on it records
        // the row it came from, for Escape to go back to; the list handing the focus back to it
        // records nothing, so the row outlives a pick (`onTriggerFocus`).
        <SnoozePick
          badge="Snooze"
          held={bulkSnoozeHeld}
          label="Snooze selected asks"
          marks={BULK_PICKER_MARKS}
          onPick={snoozePick}
          onTriggerFocus={(from) => {
            pickerOrigin.current = rowAround(from)?.dataset.inboxRow ?? null;
          }}
        />
      )}
      {selectedThreads.length === 0 ? null : (
        <button
          aria-label={`${dismissLabel} selected threads`}
          className={BAR_CONTROL_CLASS}
          data-inbox-bulk-dismiss=""
          onClick={dismissPick}
          type="button"
        >
          {dismissLabel}
        </button>
      )}
      {allRows ? null : (
        <button
          className={BAR_CONTROL_CLASS}
          data-inbox-select-all=""
          onClick={selectAll}
          type="button"
        >
          Select all
        </button>
      )}
      <button className={BAR_CONTROL_CLASS} onClick={clearSelection} type="button">
        Clear selection
      </button>
    </SelectionBar>
  );

  // Everything above the list under the header line, the same whether or not any band has a row,
  // and a sibling of the scrolling list rather than an item in it: the controls stay put while
  // the rows move.
  //
  // The header line holds the search box and one bar holds the switch, the sorting and the
  // filtering, **Mark all read** in its **More** and **New message** as its main action; nothing
  // else between the title and the first row draws a search box, a panel trigger or a chip. The
  // credential requests stand under the bar, above the list they wait beside, and so does what a
  // refused or stopped **Mark all read** says. Nothing the reader can press here makes the
  // header taller: the bar is one line that never wraps, its chips in that line, and the filter
  // panel, the display menu and **More** are portalled over the page (`AnchoredPanel`), and the
  // selection bar is drawn over the list. The drawer is at most 28rem wide from 768 px, so in a
  // window 768 px wide or wider its bar takes the narrow layout by its own width, as a phone's
  // does.
  const aboveList = (
    <div className="space-y-4">
      <ControlRow
        actions={actions}
        chips={chips}
        clear={clearFilters}
        display={displayMenu}
        main={{ icon: NEW_MESSAGE_GLYPH, label: "New message", onPress: onCompose }}
        narrowDisplayInMore
        switch={inboxSwitch}
        trigger={{
          children: renderFilterControls,
          count: chips.length,
          label: "Filter",
          onOpenChange: setFilterOpen,
          open: filterOpen,
          stacked: true,
        }}
      />
      {markAllFailure}
      <CredentialRequestsSection credentials={credentials} />
      {/* A dismiss the server refused: the rows are back where they were, and the page says so
          here, above the list, because the control that sent it was on a row this write had
          already taken off the page. */}
      {dismissWrite.refusal === undefined ? null : (
        <p className={`text-sm ${dangerText}`} data-inbox-dismiss-refused="" role="alert">
          {dismissWrite.refusal}
        </p>
      )}
    </div>
  );

  // Two columns, the reading column to the right of the list, each scrolling in itself inside
  // the height the page gives them (`app.tsx` makes the Inbox page fill the viewport here).
  //
  // One root, selection or none: the column is what appears and disappears beside the list,
  // never the list itself. A selection empties for a commit whenever the selected row leaves
  // the rendered bands - the reader's own bulk snooze of that row does it, before the effect
  // above hands the selection on - and a root that changed shape there would have React
  // unmount the whole list, taking the keyboard's focus, the bulk bar and every row's state
  // with it and mounting a fresh one a commit later.
  const page = (listColumn: ReactNode, subject: InboxListRow | undefined): ReactNode => (
    // Every thread this list draws - in the column, and in a card below its width - draws its own
    // Dismiss from here (`DismissControl`).
    <RowDismissContext
      value={{
        dismiss: (named) => dismissRows([named]),
        pending: dismissWrite.pending,
        show,
      }}
    >
      {/* Every Snooze this list draws - a row's, and the column head's - is held while the
            bar's own Snooze write is out over its ask (`SnoozeControl`). */}
      <BulkSnoozingContext value={bulkSnoozing}>
        {selectionStatus}
        {/* Outside the layout box above, because it is `fixed`: the bar is drawn over the list
              rather than in it, so the first mark moves no row. */}
        {selectionBar}
        <div className={paneShown ? "flex min-h-0 flex-1 flex-col gap-4" : "space-y-4"}>
          {aboveList}
          {paneShown ? (
            // The two columns are a grid of two fractions, so the share the reader dragged the
            // divider to is exactly the share the pane takes, at any width; the divider is the
            // column between them and takes its own.
            //
            // One row, `minmax(0, 1fr)`: an implicit `auto` row is as tall as the taller column,
            // so a long document in the pane would make the row - and with it both columns'
            // boxes - taller than the page, and neither column would scroll in itself. The page
            // would scroll instead, which is how the pane came to show a document's top however
            // far down it the reader's section sat.
            <div
              className="grid min-h-0 flex-1"
              ref={columnsRef}
              style={{
                gridTemplateColumns:
                  subject === undefined
                    ? "minmax(0, 1fr)"
                    : `minmax(0, ${1 - paneWidth}fr) auto minmax(0, ${paneWidth}fr)`,
                gridTemplateRows: "minmax(0, 1fr)",
              }}
            >
              {listColumn}
              {subject === undefined ? null : (
                <>
                  <PaneDivider
                    columns={columnsRef}
                    onWidthChange={setPaneWidth}
                    width={paneWidth}
                  />
                  {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handler only reads where the focus has already gone, so the column needs no role of its own. */}
                  <div className="min-h-0 min-w-0" onBlur={forgetPaneReturn} ref={holdPane}>
                    <ReadingPane onAnswered={recordAnswered} row={subject} viewer={viewer} />
                  </div>
                </>
              )}
            </div>
          ) : (
            listColumn
          )}
        </div>
      </BulkSnoozingContext>
    </RowDismissContext>
  );

  if (shown.length === 0 && held.length === 0) {
    const emptyMessage = emptyStateMessage({
      agent,
      agentTitle,
      credentials,
      selection,
      show,
      viewer,
      words,
    });
    // Nothing to read, so nothing to select and no column beside the list. The one question that
    // outlives every row is the one the column is keeping for an unsent answer, and it keeps
    // its column here too: the reader's text is in it.
    presented.current = [];
    presentedBands.current = { headings: [], layout };
    const empty = (
      // The list column is the list's own stop, so it is named: a labelled `<section>` is a region.
      <section aria-label="Inbox list" data-testid="inbox-list" ref={listScroller}>
        {emptyMessage === undefined ? null : (
          <EmptyState label="Inbox empty state" message={emptyMessage} />
        )}
      </section>
    );
    return (
      <>
        {headerLine}
        {page(empty, paneSelection.showing([]))}
      </>
    );
  }

  // Each held row is drawn in the band the reader saw it in, after the row it then followed; a
  // band a write keeps its row in, or the reader's hand keeps a moved row in, is drawn where it
  // stood though no other row is left in it (`keptBands`). A band every row has left is empty,
  // and an empty band has nothing to head.
  const heldById = new Map(held.map((placed) => [rowId(placed.row), placed]));
  const drawn = withHeldBands(bands, keptBands, presentedBands.current.headings)
    .map((band) => {
      const banded = withHeld(
        band.rows.filter((row) => !heldById.has(rowId(row))),
        held.flatMap((placed) => (placed.group === band.key ? [placed.row] : [])),
        presented.current
      );
      return { band: { ...band, rows: banded }, rows: drawnIn(band.key, banded) };
    })
    .filter(({ band }) => band.rows.length > 0);
  /** Which turn band a row is drawn in: a held row's is the one the reader saw it in. */
  const sectionDrawn = (row: InboxListRow): InboxSection =>
    heldById.get(rowId(row))?.section ?? sectionOf(row, place) ?? "human";
  presented.current = drawn.flatMap(({ band, rows: visible }) =>
    visible.map((row) => ({ group: band.key, row, section: sectionDrawn(row) }))
  );
  presentedBands.current = {
    headings: drawn.map(({ band }) => ({ key: band.key, label: band.label })),
    layout,
  };
  // Every row the bands hold, a folded one's included: `?item=` naming a row in a folded band
  // opens that band for this view rather than being read as an address this list cannot answer.
  const listedRows: PlacedRow[] = drawn.flatMap(({ band }) =>
    band.rows.map((row) => ({ group: band.key, row, section: sectionDrawn(row) }))
  );
  // Names are read off the rows as they render - band by band, top to bottom - so "ask 2 of 2"
  // never sits above "ask 1 of 2", and a twin folded away leaves the row on screen with the
  // short name rather than a count of something the reader cannot see.
  const ordinals = askOrdinals(
    presented.current.flatMap(({ row }) => (row.kind === "ask" ? [row.ask] : []))
  );
  const selectedRow = paneSelection.showing(listedRows);

  const renderRow = (bandKey: string, row: InboxListRow): ReactNode => {
    const id = rowId(row);
    const section = sectionDrawn(row);
    const onRelease = handHeld !== undefined && rowId(handHeld.row) === id ? release : undefined;
    const shared = {
      group: bandKey,
      marked: marked.has(id),
      onMark: markRow,
      onRelease,
      ordinal: row.kind === "ask" ? ordinals.get(row.ask.id) : undefined,
      row,
      section,
    };
    if (paneShown) {
      return row.kind === "ask" ? (
        <AskShortRow
          {...shared}
          ask={row.ask}
          key={id}
          onSelect={paneSelection.select}
          selected={id === paneSelection.id}
          viewer={viewer}
        />
      ) : (
        <ThreadShortRow
          {...shared}
          key={id}
          onSelect={paneSelection.select}
          selected={id === paneSelection.id}
          thread={row.thread}
        />
      );
    }
    return row.kind === "ask" ? (
      <InboxCardRow
        {...shared}
        ask={row.ask}
        key={id}
        onAnswered={recordAnswered}
        threadUpdatedAt={inboxAsks.updatedAtOf(row.ask.id)}
        viewer={viewer}
      />
    ) : (
      <ThreadCardRow {...shared} key={id} thread={row.thread} />
    );
  };

  // One list, keyed by row id, with the band headings as items between the rows: a row that
  // changes band moves within the same parent, so React moves its node instead of remounting
  // it - its draft, selection, disclosures, and focus stay, and the viewport anchor can find it.
  //
  // One header, rows or none: what appears and disappears beside it is the list, never the
  // header itself, which `page` draws above the bands and above the empty state alike. That is
  // the rule the comment above `page` states for the columns root, and the header is under it
  // for the same reason - drawing the header inside each of those two trees would have React
  // unmount it on the letter that leaves nothing listed and mount a fresh one, taking the
  // search box out from under the reader's hand and their focus to `document.body` with it, so
  // the rest of the word, and the Backspace that would correct it, would reach the Inbox's own
  // single-key bindings instead of the box (INFRA-963).
  //
  // The anchor does not hold the reader's row in place against the rows a press of theirs takes
  // away (`leaving`): the rows after them move up into their place, as the reader asked. Those
  // are the rows a Dismiss or a Move to Inbox is taking off the view (`dismissedHere`), the rows
  // a snooze write, a row's own or the bar's, moves to another band in the commit that lets go of
  // them, the rows an Assign to me, a hand-on, an answer, a decline, a question back or a reply
  // lets go of, which then leave the list where the read no longer lists them there, and the rows
  // of a band the reader's press on its toggle folds shut (`folding`), in the commit that folds it.
  const list = (
    // With the reading column beside it the list scrolls in itself rather than in the window,
    // so the two columns move independently; the anchor is given that same box, so a refetch
    // keeps the reader's row where it was in it. Without the pane the rows scroll in whatever
    // the host gives (the drawer's own `aside`), and in the window when it gives nothing. The
    // column is the list's own stop for the selection bar's hand-off, so it is named.
    <section
      aria-label="Inbox list"
      className={paneShown ? "min-w-0 flex-1 overflow-y-auto pr-1" : undefined}
      data-testid="inbox-list"
      ref={listScroller}
    >
      <ViewportAnchor
        className="space-y-4"
        group="data-inbox-section"
        item={ROW_ATTRIBUTE}
        leaving={(id) =>
          dismissedHere(id) ||
          snoozing.has(id) ||
          bulkSnoozing.has(id) ||
          askWriteOut(id) ||
          replying.has(id) ||
          folding.current?.has(id) === true
        }
        ref={viewport}
        rootRef={listRef}
        scroller={paneShown ? listScroller : scroller}
      >
        {showDismissed ? (
          <DismissedWindow
            box={paneShown ? listScroller : scroller}
            hand={paneShown ? paneSelection.id : undefined}
            handle={dismissedWindow}
            onNearEnd={threads.showingMore ? undefined : threads.showMore}
            renderRow={(row) => renderRow(ALL_GROUP_KEY, row)}
            rowHeight={paneShown ? DISMISSED_SHORT_ROW_PX : DISMISSED_CARD_ROW_PX}
            rows={presented.current.map(({ row }) => row)}
          />
        ) : (
          <ul className="space-y-3">
            {drawn.flatMap(({ band, rows: visible }) =>
              groupItems({
                folded: folded.has(band.key),
                group: band,
                held: folded.has(band.key) ? 0 : band.rows.length - visible.length,
                onShowMore: showMore,
                // Folding a band shut takes the rows it draws off the list, as the reader asked:
                // they are named to the anchor as leaving for the commit that folds it.
                onToggleFold: (key) => {
                  if (!folded.has(key)) folding.current = new Set(visible.map((row) => rowId(row)));
                  toggleFold(key);
                },
                renderRow: (row) => renderRow(band.key, row),
                rows: visible,
              })
            )}
          </ul>
        )}
        {/* The Dismissed view's own **Show more**: the next page of dismissals, read from the
            cursor the last page named, rather than rows this page is holding back, held while
            that page loads (`heldPress`). */}
        {threads.showMore === undefined ? null : (
          <button
            className={`min-h-11 rounded-lg border px-3 py-1.5 text-sm font-medium ${borderDefault} ${linkText} ${linkHoverText}`}
            data-inbox-show-more="dismissed"
            {...heldPress(threads.showingMore, threads.showMore)}
            type="button"
          >
            {threads.showingMore ? "Loading…" : `Show ${INBOX_DISMISSED_PAGE} more`}
          </button>
        )}
      </ViewportAnchor>
    </section>
  );

  // Nothing to read is nothing to select, so an empty list is the whole Inbox above, as it is
  // below the reading column's width. The one question that outlives every row is the one the
  // column is keeping for an unsent answer, and `showing` hands that one back from an empty
  // list too, so it keeps its column there: the reader's text is in it.
  return (
    <>
      {headerLine}
      {page(list, selectedRow)}
    </>
  );
}
