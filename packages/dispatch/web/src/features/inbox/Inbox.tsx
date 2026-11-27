import { awaitsDecision, INBOX_DISMISSED_PAGE, INBOX_THREAD_KINDS } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import {
  type FocusEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";

import { inboxQuery, usersQuery, whoAmIQuery } from "../../api/queries";
import type { InboxRow, MyReplyRow } from "../../api/types";
import { Chip } from "../../components/Chip";
import { ControlRow } from "../../components/ControlRow";
import { type ControlSize, controlPadding } from "../../components/control-names";
import { DisplayMenu, type DisplayMenuSection } from "../../components/DisplayMenu";
import { EmptyState } from "../../components/EmptyState";
import { type FilterChip, filterChips } from "../../components/filter-chips";
import { focusLost } from "../../components/focus-place";
import { heldPress, useHeldThroughAnswer } from "../../components/held-press";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { type ListMarks, MultiSelect } from "../../components/MultiSelect";
import { PanelTrigger } from "../../components/PanelTrigger";
import { pillClassName } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import { SearchBox } from "../../components/SearchBox";
import { focusListStop, SelectionBar, useLastMarkedPlace } from "../../components/SelectionBar";
import { TruncatedText } from "../../components/TruncatedText";
import { useNestedPanels } from "../../components/useNestedPanels";
import {
  capSearchQuery,
  loadWholePage,
  useAddressNavigate,
  useNavigationState,
} from "../../lib/page-address";
import {
  borderDefault,
  dangerText,
  focusVisibleRing,
  linkHoverText,
  linkText,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedBg,
  textMutedOnCanvas,
  textMutedOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { searchWords } from "../agents/agent-search";
import { CredentialRequestsSection } from "../credentials/CredentialRequestsSection";
import { type CredentialRequests, useCredentialRequests } from "../credentials/pending";
import { WaitingPlans } from "../going/WaitingPlans";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { AgentName, useAgentText } from "../refs/AgentName";
import { COPY_REF_SELECTOR, CopyRefButton } from "../refs/CopyRefButton";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import {
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
import { type KeymapScope, takesText, useKeymap, useKeymapScope } from "../shell/keymap";
import { closestMatching, focusedMatching, roveFocus } from "../shell/roving";
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
import { MarkCheckbox, ThreadMarkCheckbox } from "./MarkCheckbox";
import { MARK_ALL_REFUSED_TEXT, MARK_ALL_STOPPED_TEXT, useMarkAllRead } from "./mark-all";
import { useMotionHold } from "./motion-hold";
import {
  AskAnswerWritesContext,
  AskClarificationWritesContext,
  AskDeclineWritesContext,
  AskReplyWritesContext,
  useOutstandingAnswerWrites,
} from "./outstanding-answers";
import { PaneDivider, usePaneWidth } from "./PaneDivider";
import { inboxEntryState, type PlacedRow, usePaneSelection } from "./pane-selection";
import { useFoldedGroups } from "./preferences";
import { ReadingPane } from "./ReadingPane";
import {
  PANE_SCROLLER_SELECTOR,
  ReadingView,
  type ReadingViewTarget,
  useReadingViewLanding,
} from "./ReadingView";
import { RowKindIcon, RowPriorityTag, RowUrgencyMark } from "./RowMarks";
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
import { ThreadTitle } from "./ThreadTitle";
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

/** The chrome of the page's own controls - **Mark all read** and the needs-you control - so
 *  each keeps the row's 44 px line and nothing in the row can grow it. */
const PAGE_CONTROL_CLASS = `rounded-lg border text-sm font-semibold whitespace-nowrap outline-none focus-visible:ring-2 ${controlPadding.comfortable} ${focusVisibleRing} ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnCanvas}`;

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
 *  <agent>") is the card's own status line. The chip is a label pill's colours in a shape that
 *  may be narrower than its words: it takes at most two fifths of the card head's first line,
 *  so the issue's key and title keep the rest, and a long agent name ends in an ellipsis
 *  inside it, the whole of it in the title. */
function InboxRowChip({ ask }: { ask: InboxRow }): ReactNode {
  const agentText = useAgentText();
  if (ask.waiting_on === "human" && ask.last_reply?.author.kind === "session") {
    return (
      <span className={`${pillClassName("label")} min-w-0 max-w-[40%]`} data-inbox-reply-chip="">
        <TruncatedText title={`${agentText.label(ask.last_reply.author)} replied`}>
          <AgentName actor={ask.last_reply.author} inline /> replied
        </TruncatedText>
      </span>
    );
  }
  return null;
}

const BULK_PICKER_ATTRIBUTE = "data-inbox-bulk-snooze";
const BULK_PICKER_MARKS: ListMarks = { trigger: { [BULK_PICKER_ATTRIBUTE]: "" } };
/** The bulk bar's snooze picker, its list's trigger: `h` hands it focus and Escape takes it back
 *  to the list. */
const BULK_PICKER_SELECTOR = `[${BULK_PICKER_ATTRIBUTE}]`;

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
   *  ways in. */
  onMark: (id: string) => void;
  /** Set on the held row - one the server dropped or moved to another band that stays where
   *  the reader last saw it while they are still on it; called when their focus or pointer
   *  leaves it. */
  onRelease?: () => void;
  /** This ask's place among its owner's rows, when the owner has more than one listed. */
  ordinal: AskOrdinal | undefined;
  row: InboxListRow;
  section: InboxSection;
}

/** The checkbox a row draws for its mark (`MarkCheckbox`), the one checkbox every row has. */
const MARK_SELECTOR = 'input[type="checkbox"]';

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
      className={`group rounded-xl outline-none focus-visible:ring-2 ${focusVisibleRing} ${
        selected ? surfaceMutedBg : ""
      }`}
      data-inbox-group={groupKey}
      data-inbox-row={id}
      data-inbox-section={section}
      data-inbox-selected={selected ? "true" : undefined}
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
      tabIndex={-1}
    >
      {children}
    </li>
  );
}

/** What a short row adds to every row layout's props: on the Inbox page the row is selectable,
 *  and the one the reading column shows is drawn as selected. */
interface ShortRowProps {
  /** The reader's pick of this row: beside the reading column it selects the row, and below
   *  that width it opens the row's reading view in place of the list. */
  onSelect: (id: string) => void;
  /** Whether this is the row the reading column is showing. */
  selected: boolean;
}

/**
 * The frame both short rows draw in: R6's "title line, then a line of marks", in three named
 * groups in this order:
 *
 * - Line one, **Title**: the mark checkbox, the kind's icon, what the row is about, and the time
 *   it was last written in. It is one group that never wraps: at any width the title is the one
 *   element that shortens, ending in an ellipsis with its whole text as the title.
 * - Line two, **Marks**: the priority tag, an ask's urgency, and the row's owner, which
 *   truncates; then **Actions**, the row's own controls (a thread's Copy reference and
 *   Dismiss), at the right edge.
 *
 * Every mark and every control is `shrink-0`, so the title and the owner are what shorten and
 * neither the marks, the key nor a control is ever cut. A second line too narrow for Marks at
 * their smallest and the Actions beside them gives way a whole group at a time: the Actions move
 * to a line of their own. `children` is drawn under those lines.
 */
function ShortRowFrame({
  actions,
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
  /** The row's own controls, drawn at the right of line two; undefined for a row with none. */
  actions?: ReactNode;
  /** When the row was last written in, as its own kind reads a time. */
  age: ReactNode;
  /** The mark checkbox, or the space a row that takes no mark leaves where it would be. */
  check: ReactNode;
  children?: ReactNode;
  group: string;
  onRelease: (() => void) | undefined;
  /** The row's owner, the end of line two's Marks group. */
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
        <div className="flex min-w-0 flex-wrap items-center gap-x-2 pl-6">
          <div
            className="flex min-w-0 flex-1 basis-40 items-baseline gap-2"
            data-inbox-row-context=""
          >
            <RowPriorityTag priority={rowPriority(row)} />
            <RowUrgencyMark urgency={rowUrgency(row)} />
            {owner}
          </div>
          {actions === undefined ? null : (
            <div className="ml-auto flex shrink-0 items-center gap-2" data-inbox-row-actions="">
              {actions}
            </div>
          )}
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

/** An open question on the Inbox page: its first line, how long it has waited, and the issue or
 *  document it belongs to. */
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
    ask: InboxRow;
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
          <MarkCheckbox id={ask.id} marked={marked} name={name} onMark={onMark} pressSquare />
        </>
      }
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      owner={<InboxOwnerLink ask={ask} rowOwner />}
      row={row}
      section={section}
      selected={selected}
      title={
        // Main's title span, as main drew the question's words: its size, its place in the row
        // and the line it cuts (`truncate`). The preview is that span's inline text (`inline`),
        // so the ellipsis falls where main's did, and its type is the span's, since
        // `.dispatch-markdown` takes its size and its line from the element around it
        // (styles.css). `fullTitle` gives the line it cuts its whole text as a title.
        <span className="min-w-0 grow truncate text-sm">
          <MarkdownPreview
            className="inline"
            fullTitle
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

/** A Reply or an Agent row on the Inbox page: the thread's subject as a link to the
 *  thread itself, when it was last written in, whose it is, and - in the Actions group - its
 *  Copy reference and the **Dismiss** that takes it out of the list, drawn while the reader's
 *  hand is on the row, always on the selected one, and always where no pointer hovers. */
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
  titleOpensRow,
}: InboxRowProps &
  ShortRowProps & {
    thread: MyReplyRow;
    /** Below 1280 px a plain click on the title opens the row in place, as a click anywhere
     *  else on the row does; a modified or middle click still follows the link. */
    titleOpensRow: boolean;
  }): ReactNode {
  const reference = threadReferenceRoute(thread);
  // The column draws an ask's thread with the ask's card, whose reply the Inbox holds by ask
  // (`AskReplyWritesContext`): it can be out or failed while the column shows another row. The
  // row then says it is out, or why it failed; the Retry is the card's, once the column shows the
  // thread again.
  const reply = useInboxWrites(AskReplyWritesContext).of(thread.thread_id);
  return (
    <ShortRowFrame
      actions={
        <>
          {reference === undefined ? null : <CopyRefButton route={reference} />}
          <DismissControl shown={selected} thread={thread} />
        </>
      }
      age={<Timestamp at={rowLastActivityAt(row)} />}
      check={
        <ThreadMarkCheckbox marked={marked} onMark={onMark} pressSquare row={row} thread={thread} />
      }
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      owner={
        <ThreadOwnerLine className={`truncate text-sm ${textMutedOnCanvas}`} thread={thread} />
      }
      row={row}
      section={section}
      selected={selected}
      title={
        <ThreadTitle
          className="min-w-0 grow truncate text-sm"
          href={thread.ref}
          onOpen={titleOpensRow ? () => onSelect(rowId(row)) : undefined}
          thread={thread}
        />
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

/**
 * The drawer's row: the question's whole card, under a head of two lines, each a named group:
 *
 * - Line one, **Identity**: the mark checkbox, the kind's icon, the issue or document the
 *   question belongs to (its key whole, its title cut with an ellipsis), and the reply chip,
 *   which takes at most two fifths of the line and cuts its own words.
 * - Line two, **Actions**: the urgency, the priority control, "Assign to me" and Snooze, the
 *   controls that defer or reroute the question. Too narrow a line wraps them as whole
 *   controls, and a refusal from Snooze or Assign to me is a full-width line inside the group.
 */
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
  ask: InboxRow;
  onAnswered: (id: string) => void;
  /** Timestamp of the Inbox snapshot that supplied this row's initial thread. */
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
      <div className="mb-1.5 flex flex-col gap-1">
        <div className="flex min-w-0 items-center gap-2" data-inbox-card-identity="">
          {reads}
          <MarkCheckbox id={ask.id} marked={marked} name={name} onMark={onMark} />
          <RowKindIcon type={rowType(row)} />
          <InboxOwnerLink ask={ask} rowOwner />
          <InboxRowChip ask={ask} />
        </div>
        <div
          className="flex min-w-0 max-w-full flex-wrap items-center gap-1 pl-6"
          data-inbox-card-actions=""
        >
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

/** What the Inbox keeps of a row a Snooze of the reader's is taking (`snoozedHere`): the read the
 *  list drew while the write was out, and the write that named the row, the row's own or the
 *  bar's, whose refusal alone ends the name. */
interface SnoozeName {
  readonly from: "bar" | "row";
  readonly read: unknown;
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
 * `filter`/`onFilterChange`/`keymapScope` exist for `InboxDrawer` (LEGION-547): a peek
 * embedded on another route must not read or write the real URL for its filters or the text
 * typed in its search box (either would navigate the page it sits over), and its
 * Escape/j/k/etc. must resolve under the drawer's own `dialog` scope - the modal scope a dialog
 * anywhere on the stack exclusively consults - rather than the standalone page's `inbox` scope,
 * which a dialog's keydown handling never reaches. `pane` is the third: the drawer is a peek at
 * any width, a list of cards that selects nothing. `InboxPage`'s unmodified `<Inbox/>` call keeps
 * today's URL-backed behaviour untouched.
 */
export interface InboxProps {
  filter?: InboxRoute;
  keymapScope?: KeymapScope;
  /** A host that owns the filter itself: every change this list makes - a filter, the sort, the
   *  grouping, the view, the text typed in the search box - is reported here instead of being
   *  written to the address. */
  onFilterChange?: (next: InboxRoute) => void;
  /** Whether this Inbox reads one row at a time, which the page does at every width: its rows
   *  are short rows, and the row the reader picks is read in the reading column beside the list
   *  from `READING_PANE_QUERY` (1280 px), and below that width in a reading view that takes the
   *  list's place until the reader goes back. False keeps the card in the row, no selection and
   *  nothing written to `?item=`, at every width, which is what the drawer is. */
  pane?: boolean;
  /** The box the host scrolls these rows in, where that is neither the window nor the list's
   *  own column: the drawer scrolls them in its `aside`, so the viewport anchor must read and
   *  move that element's `scrollTop` - `window.scrollBy` moves nothing there, and the row the
   *  reader's hand is on jumps by the height of a row removed above it. Ignored while the
   *  reading column shows, where the list column is the box that scrolls. */
  scroller?: RefObject<HTMLElement | null>;
}

/**
 * The Inbox: the bar, the bands of rows, and the row the reader is reading - in the reading
 * column beside the list from 1280 px, and below that width in a reading view that takes the
 * list's place (`ReadingView`). It draws under its own ask writes (`InboxAskWrites`): every
 * control it draws for an ask shows that ask's one write of its kind, which the Inbox holds,
 * whichever control pressed it.
 */
export function Inbox({
  filter,
  keymapScope,
  onFilterChange,
  pane,
  scroller,
}: InboxProps = {}): ReactNode {
  return (
    <InboxAskWrites>
      <InboxBody
        filter={filter}
        keymapScope={keymapScope}
        onFilterChange={onFilterChange}
        pane={pane}
        scroller={scroller}
      />
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

function InboxBody({
  filter: filterOverride,
  keymapScope = "inbox",
  onFilterChange,
  pane = true,
  scroller,
}: InboxProps): ReactNode {
  const [addressRoute, goToRoute] = useInboxRoute();
  const entryState = useNavigationState();
  const navigate = useAddressNavigate();
  const filter = filterOverride ?? addressRoute;
  // The reading column stands beside the list from 1280 px, on the page alone (`columns`).
  // Below that width the page draws the same short rows, and a row the reader opens is read in
  // place of the list; the drawer is the list of cards at every width.
  const wideEnoughForPane = useMediaQuery(READING_PANE_QUERY);
  const columns = pane && wideEnoughForPane;
  // One inbox query, shared with the nav badge, the sidebar, the agents page and the margin; the
  // list below is a client-side partition of it, so an answered row leaves every surface at once.
  const inbox = useQuery(inboxQuery());
  const whoAmI = useQuery(whoAmIQuery());
  /** Which of the reader's two lists is showing: their Inbox, or what they have dismissed out of
   *  it. A thread stays in the Inbox, read or not, until it is dismissed. */
  const show: InboxShow = filter.show ?? "inbox";
  const showDismissed = show === "dismissed";
  const listRef = useRef<HTMLElement>(null);
  /** The list column, which scrolls in itself beside the reading column. It is drawn with no row
   *  as well, and it is the list's own stop for the selection bar's hand-off while the list is
   *  shown (`barStop`, `useLastMarkedPlace`). */
  const listScroller = useRef<HTMLElement>(null);
  /** The reading column, where `h`, `o`, `y`, Enter and `1`-`9` act while it is on. Written by
   *  `holdPane` below rather than handed to the wrapper, so the column's unmount is seen. */
  const paneRef = useRef<HTMLDivElement | null>(null);
  /** The grid the list, the divider and the reading column share, whose width a drag of the
   *  divider is measured against. */
  const columnsRef = useRef<HTMLDivElement>(null);
  /** How much of that grid the reading column takes, kept for the reader's browser. */
  const [paneWidth, setPaneWidth] = usePaneWidth();
  // A refetch can land while a pointer travels between rows; adopt it once that pointer settles.
  const adoptedInbox = useMotionHold(inbox.data, listRef);
  // The credential requests the Inbox lists above its asks: the section, the Blocked on you
  // control and the empty state all read this one answer.
  const credentials = useCredentialRequests();
  const viewer = whoAmI.data?.login;
  // Who the list is for: what `?assignee=` carries, and the reader alone when it carries
  // nothing. The address is the whole memory - a filter the reader can link, and nothing kept
  // behind their back.
  const selection = filter.assignees ?? (viewer === undefined ? [] : [viewer]);
  const sort = filter.sort ?? "newest";
  const groupBy = filter.group ?? "priority";
  const filters: InboxFilters = filter.filters ?? {};
  const [threadOrder, setThreadOrder] = useThreadOrder();
  const [folded, toggleFold] = useFoldedGroups(groupBy);
  // How many pages of rows each band is drawing; a band starts on its first.
  const [pages, setPages] = useState<ReadonlyMap<string, number>>(() => new Map());
  const showMore = useCallback((key: string) => {
    setPages((current) => new Map(current).set(key, (current.get(key) ?? 1) + 1));
  }, []);
  /** Whether the one **Filter** panel stands open. */
  const [filterOpen, setFilterOpen] = useState(false);
  /** Which of its five controls has its popover open: one at a time, and none once it shuts. */
  const filterPanel = useNestedPanels<InboxFilterPanel>(filterOpen);
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
      // rule every address-backed filter here holds to (`lib/page-address.ts`). A rewrite that
      // keeps the row the address names keeps what its history entry says of it too: who picked
      // the row, and whether opening it pushed over the list (`pane-selection.ts`).
      goToRoute(next, next.item === filter.item ? entryState : undefined);
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
  const agentText = useAgentText();
  // Every row's snooze write, held by ask around this list (`Inbox`) rather than by the control
  // that pressed, which can go while the write is out: the column's head goes when the reader
  // moves the selection. A row whose write is out (`snoozing`) stays in the band the reader saw
  // it in, though the optimistic moment puts it into Later or out of it, and is drawn even when
  // that band is folded (`heldRow`). Its move is made in the commit this lets go of it, once the
  // write lands (see `SnoozeControl`). A refused or stopped write holds no row, and its reason
  // stays with the ask for every control drawn for it. Under the Blocked on you narrowing
  // (`?section=needs-you`), which lists no snoozed ask, the optimistic moment drops the row at
  // once: the hand's own hold keeps no row a press of the reader's is taking away (`leavingNow`).
  const snoozeWrites = useInboxWrites(AskSnoozeContext);
  const snoozing = snoozeWrites.out;
  /** The asks the reader wrote to from this list - a snooze, Assign to me, a hand-on, a bulk
   *  snooze - by the render their write ended after, `null` while it is live: a snooze, an
   *  Assign to me or a hand-on while it is out or failed (`AskWrites.live`), so one Dispatch
   *  refused or the reader stopped is still theirs, and the bar's snooze until it settles,
   *  whatever Dispatch answers. A row such a write takes out of the list is the reader's own
   *  move, and it leaves no later than the render after the write ends: each write moves the
   *  Inbox read itself as it is made, and a row its write held in the list is let go as it ends.
   *  Once the selection has settled on that render the mark is dropped (the effect after
   *  `usePaneSelection`), so a row the write left in the list that later leaves on its own -
   *  withdrawn, resolved - is said to have left. The writes are this Inbox's own
   *  (`InboxAskWrites`), so the marks are read off them after each commit (below), not told by
   *  each control that pressed. */
  const writtenHere = useRef<Map<string, number | null>>(new Map());
  /** Which render this is: an ended mark is dropped after the first render made after it ended. */
  const renders = useRef(0);
  renders.current += 1;
  const thisRender = renders.current;
  // An ended write re-renders the list even where nothing else it holds changes, so the mark
  // never waits on a later read, which might be the one that takes the row away on its own.
  const [, setWritesEnded] = useState(0);
  /** Below 1280 px, whether the row the reader was reading left their Inbox on its own -
   *  withdrawn, resolved, answered elsewhere - so the list they are back at says so. */
  const [rowLeft, setRowLeft] = useState(false);
  // The line speaks for the moment the reader is brought back to the list: their next key or
  // press anywhere on the page ends it, and so does the window reaching the column's width,
  // where the list it spoke of is no longer the one they see.
  useEffect(() => {
    if (!rowLeft) return;
    if (columns) {
      setRowLeft(false);
      return;
    }
    const clear = () => setRowLeft(false);
    document.addEventListener("keydown", clear, true);
    document.addEventListener("pointerdown", clear, true);
    return () => {
      document.removeEventListener("keydown", clear, true);
      document.removeEventListener("pointerdown", clear, true);
    };
  }, [columns, rowLeft]);
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
  const { live: assignsLive, out: assignsOut } = useInboxWrites(AskAssignWritesContext);
  const { live: handOnsLive, out: handOnsOut } = useInboxWrites(AskHandOnWritesContext);
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
  const answered = useRef<string | null>(null);
  const [, setReleased] = useState(0);
  const release = () => setReleased((count) => count + 1);
  const openFoldedBand = useCallback(
    (key: string) => {
      if (folded.has(key)) toggleFold(key);
    },
    [folded, toggleFold]
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
  /** Whether the row with this id is still in the reader's Inbox, whatever this list's filters,
   *  section or agent narrow it to: the Inbox read itself, and the thread side of it. */
  const inInbox = (id: string): boolean =>
    (adoptedInbox ?? []).some((ask) => ask.id === id) ||
    threads.rows.some((row) => rowId(row) === id);
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
    // The Dismissed view pages at the server: every row a page answered is drawn, and **Show
    // more** reads the next 200 rather than revealing rows already here.
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
    forgetDismissed,
    dismissPick,
    dismissRows,
    dismissWrite,
    lastMarked,
    marked,
    onMark,
    selectAll,
    selectedAsks,
    selectedThreads,
    snoozePick,
    snoozeRefusal,
    snoozeRefused,
    snoozing: bulkSnoozing,
  } = useInboxSelection({
    asOf: narrowingMark,
    candidates,
    drawnIds,
    filters,
    rows: byId,
    showDismissed,
  });
  // The marks of the reader's own writes (`writtenHere`), read off this Inbox's writes after each
  // commit: an ask with a snooze, an Assign to me or a hand-on out or failed, or under the bar's
  // snooze while it is pending, is marked while that lasts, and a mark whose write no longer
  // marks it ends at this render. A layout effect declared before the selection's own, so a
  // commit that takes a row away has its marks before the selection reads them
  // (`usePaneSelection`).
  useLayoutEffect(() => {
    const live = new Set([...snoozeWrites.live, ...bulkSnoozing, ...assignsLive, ...handOnsLive]);
    for (const id of live) writtenHere.current.set(id, null);
    let ended = false;
    for (const [id, at] of writtenHere.current) {
      if (at !== null || live.has(id)) continue;
      writtenHere.current.set(id, thisRender);
      ended = true;
    }
    if (ended) setWritesEnded((count) => count + 1);
  });
  const paneSelection = usePaneSelection({
    bulkSnoozing: bulkSnoozing.size,
    candidates,
    columns,
    filter,
    leftByReader: (id) =>
      writtenHere.current.has(id) ||
      bulkSnoozing.size > 0 ||
      dismissedHere(id) ||
      dismissWrite.pending.has(id) ||
      answered.current === id,
    listRef,
    // The status line is for a row gone from the reader's Inbox, not one that only left this
    // list's filter or section - a priority changed elsewhere, say - which is still there.
    onOpenRowLeft: (id) => {
      if (!inInbox(id)) setRowLeft(true);
    },
    openFoldedBand: useCallback(() => openFoldedBand(LATER_GROUP_KEY), [openFoldedBand]),
    pane,
    presented,
    rowsReady: threads.ready,
    snoozing,
  });
  // After the selection has settled on this render (`usePaneSelection`'s effect runs first), a
  // write that had ended before this render was made has had its one render to take its row
  // out of the list as the reader's own move: its mark goes.
  useEffect(() => {
    for (const [askId, ended] of writtenHere.current) {
      if (ended !== null && ended < thisRender) writtenHere.current.delete(askId);
    }
  });
  /**
   * How this Inbox reads its rows, chosen once here for every binding and the layout below:
   *
   * - `cards`: the drawer (`pane` false), each row its own card, nothing selected;
   * - `list`: the page below 1280 px with no row open - the keys act on the focused row, and the
   *   four that act inside a row's reading view open it first (`openFocused`);
   * - `reading`: the page below 1280 px, a row's reading view in the list's place;
   * - `columns`: the page from 1280 px, the reading column beside the list.
   */
  const mode: "cards" | "columns" | "list" | "reading" = !pane
    ? "cards"
    : columns
      ? "columns"
      : paneSelection.id === undefined
        ? "list"
        : "reading";
  /** Whether the keys act on the row the reading column or view shows rather than on the focused
   *  row. */
  const keysOnShownRow = mode === "columns" || mode === "reading";
  /** The reader's pick of a row: beside the list it selects the row, and below that width it
   *  opens the row's reading view and takes the reader to a target in it (`ReadingView`). */
  const openRow = useReadingViewLanding({
    columns,
    open: (id) => {
      setRowLeft(false);
      paneSelection.open(id);
    },
    openId: paneSelection.id,
    viewRef: paneRef,
  });
  /** Below 1280 px with no row open, the pick of the focused row, landing on `target` in its
   *  reading view: what Enter, `1`-`9`, `y` and `h` do there. */
  const openFocused = (target?: ReadingViewTarget) => {
    const id = focusedRow()?.dataset.inboxRow;
    if (id !== undefined) openRow(id, target);
  };
  /** Below 1280 px with no row open, whether the focused short row is a question its view's head
   *  draws a Snooze for: one the reader has not snoozed already. */
  const focusedAskToSnooze = (): boolean => {
    const row = handRow();
    return row?.kind === "ask" && !isSnoozed(row.ask);
  };
  const focusedAskWithOptions = (): string | undefined => {
    const row = handRow();
    return row?.kind === "ask" && row.ask.kind !== "approval" && (row.ask.options?.length ?? 0) > 0
      ? row.ask.id
      : undefined;
  };
  // The refetch a recorded answer triggers returns the list the optimistic removal already
  // produced, so it re-renders nothing on its own; release explicitly. The reader's own answer
  // is one of the ways the selection is handed on, and only while it is still the selected
  // question: the list keeps the row where the reader saw it until the server decides their
  // answer (`heldRow`), but a selection that moved on meanwhile - an approval reviewed from the
  // head, a row they walked off - must not be pulled back off the row it moved to.
  const recordAnswered = (id: string) => {
    answered.current = id;
    if (keysOnShownRow && paneSelection.selectedNow.current === id) {
      paneSelection.select(paneSelection.nextAfterSelected());
    }
    release();
  };
  const rows = () => [...(listRef.current?.querySelectorAll<HTMLElement>(ROW_SELECTOR) ?? [])];
  const step = (delta: 1 | -1) => roveFocus(rows(), rowAround(document.activeElement), delta);
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
  /** One step of the reader's, in whichever mode the Inbox is in: the selection the column or
   *  the reading view follows, or the focus roving the rows. Chosen once, so no binding below
   *  asks again. */
  const stepInMode = keysOnShownRow ? paneSelection.step : step;
  /** That step, taken from wherever the reader is standing. From inside the column the focus
   *  comes back to the selected row first: `j` fires in there, a `tabindex=0` section being no
   *  editable target, and a step from an approval to a question the column draws as a card
   *  swaps `ApprovalBody` for `CardBody`, which unmounts the focused scroller and drops the
   *  focus to `document.body` - where neither of Escape's branches holds and the key would do
   *  nothing. The reader then stands exactly where a click and then `j` leaves them today. A
   *  reading view below 1280 px has no row in sight to come back to, and its Escape goes to the
   *  list rather than to a row, so the step alone is taken. From anywhere else the column's
   *  noted focus is forgotten: a control the column's own code took away under the focus, with
   *  no render of the Inbox's to read that loss, leaves the note on it, and the step's commit
   *  would hand the focus to the next question's stop (below `paneScroller`), which no loss of
   *  this key took. */
  const move = (delta: 1 | -1) => {
    if (mode === "columns" && focusInPane()) {
      paneSelection.selectedRow()?.focus();
    } else {
      paneFocus.current = null;
    }
    stepInMode(delta);
  };
  /** What the keys act on, for the bindings' labels: the row the column is showing, or the row
   *  holding the focus. */
  const acted = keysOnShownRow ? "selected" : "focused";
  /** When the arrows move, and what their label says of it: in the reading column the arrows
   *  are the column's - a document scrolls, an option list moves - so they move the selection
   *  only while the focus is in the list beside it. */
  const arrowsMove = keysOnShownRow ? focusInList : () => focusedRow() !== null;
  const arrowsWhen = keysOnShownRow ? "the focus is in the list" : "a row is focused";
  /** The row the keys act on: the one the column is showing, or the one holding the focus. */
  const handRow = (): InboxListRow | undefined => {
    const id = keysOnShownRow ? paneSelection.id : focusedRow()?.dataset.inboxRow;
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
  const hand = () => (keysOnShownRow ? paneRef.current : focusedRow());
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
    if (stop === null && lost && mode === "columns" && presented.current.length > 0) return;
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
    keysOnShownRow
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
  useKeymap(keymapScope, [
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
      // Below 1280 px with no row open, the focused row holds no answer box: Enter opens its
      // reading view, the keyboard's way to the pick a click makes, and the box is there.
      label: mode === "list" ? "Open the focused row" : `Answer the ${acted} ask`,
      run: () => {
        if (mode === "list") {
          openFocused();
          return;
        }
        // A decision block's compact card folds its answer box behind "Add a note or answer in
        // your own words"; Enter presses that disclosure, and the card lands the focus in the
        // box its own render reveals (`AskCard.tsx`) - the box does not exist in this tick, so
        // nothing here could focus it. Only the column draws that compact card: from a row, and
        // from a disclosure already open, the key goes straight to the box.
        const box = inAnswerHand("[data-ask-answer]");
        if (box === null && keysOnShownRow) {
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
      // inside an ask card for the same reason (`outsideAskCard`). Below 1280 px the row is
      // hidden with the list once its view is open, and the view's scroller, where the pick
      // left the focus, stands in for it: the Inbox control beside it keeps its own Enter.
      when: () =>
        mode === "list"
          ? focusedRow() !== null
          : keysOnShownRow
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
        const index = Number(event.key) - 1;
        if (mode === "list") {
          if (focusedAskWithOptions() !== undefined) openFocused({ index, kind: "option" });
          return;
        }
        const options = answerHand()?.querySelectorAll<HTMLElement>("[data-ask-option]");
        options?.[index]?.click();
      },
      when: () =>
        mode === "list"
          ? focusedAskWithOptions() !== undefined
          : inAnswerHand("[data-ask-option]") !== null,
    },
    {
      id: "open",
      keys: "o",
      label: "Open the row's issue, document or thread",
      run: () => {
        const owner = inHand("[data-inbox-owner]");
        // Below 1280 px with no row open, a thread row's title opens the row in place on a plain
        // click (`ThreadTitle`'s `onOpen`), and a script's click is one. `o` is the way to the
        // thread's own page, as it is beside the column, so it follows the link itself.
        if (mode === "list" && handRow()?.kind !== "ask" && owner instanceof HTMLAnchorElement) {
          loadWholePage(owner.href);
          return;
        }
        owner?.click();
      },
      when: () => inHand("[data-inbox-owner]") !== null,
    },
    {
      id: "copy-ref",
      keys: "y",
      // An Agent row has no reference of its own - its thread is a message to a session - so
      // it draws no copy control and this stands down on it.
      label: `Copy the ${acted} row's reference`,
      run: () => {
        if (mode === "list" && inHand(COPY_REF_SELECTOR) === null) {
          openFocused({ kind: "copy-ref" });
          return;
        }
        inHand(COPY_REF_SELECTOR)?.click();
      },
      when: () =>
        inHand(COPY_REF_SELECTOR) !== null || (mode === "list" && handRow()?.kind === "ask"),
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
        if (mode === "columns" && focusInPane()) {
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
      //
      // Below 1280 px a row's reading view hides the list, so there is no row to come back to:
      // `leave-reading` below is that width's Escape, and this one stands down there.
      when: () =>
        mode !== "reading" &&
        (rowAround(document.activeElement) !== null ||
          (mode === "columns" && focusInPane() && paneWayBack() !== null)),
    },
    {
      // Below 1280 px the reading view stands in the list's place, so one level out of it is
      // the list, at the row the reader left (`pane-selection.ts`). In the answer box, a text
      // input or the document's editor Escape is that field's own, and leaving the whole view
      // from there would be a bigger move than the reader pressed for; a closed pick in the view's
      // head - Snooze, Priority - holds no text of the reader's, so Escape there is the way back to
      // the row, as it was from the card's own Snooze. Inside the question's card - an option, a
      // button - one level out is the view itself, its scroller, where the next Escape is the
      // list: the options the reader picked are the card's own state, and closing the view would
      // drop them. Registered ahead of `clear-selection`, so the marks the bulk bar holds outlast it.
      // Not a palette row, for the reason `clear-selection` gives.
      id: "leave-reading",
      inEditable: true,
      keys: "Escape",
      label: "Back to the list",
      palette: false,
      run: () => {
        if (outsideAskCard()) {
          paneSelection.close();
          return;
        }
        paneScroller()?.focus();
      },
      when: () =>
        mode === "reading" &&
        !(document.activeElement !== null && takesText(document.activeElement)) &&
        document.activeElement?.matches(BULK_PICKER_SELECTOR) !== true &&
        (outsideAskCard() || paneScroller() !== null),
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
      // One test for all three cases the key does nothing in: in the drawer, and below 1280 px
      // with no row open, the keys act on no row shown (`mode` is `cards` or `list`), and with
      // no row selected no column is rendered at all, so `paneRef.current` is null and there is
      // no scroller under it.
      when: () => keysOnShownRow && paneScroller() !== null,
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
      when: () => dismissableRow() !== undefined && (!keysOnShownRow || outsideAskCard()),
    },
    {
      id: "snooze",
      keys: "h",
      // One key for both: with rows marked it is the whole selection, otherwise the row in hand.
      label: "Snooze the focused ask, or every selected ask",
      run: () => {
        if (selectedAsks.length > 0) {
          bulkPicker()?.focus();
          return;
        }
        if (mode === "list") {
          openFocused({ kind: "snooze" });
          return;
        }
        // The selected short row carries no Snooze of its own: the reading column's head does,
        // for the row it is showing.
        inHand(ROW_SNOOZE_SELECTOR)?.focus();
      },
      // The selection's key works from wherever marking leaves the reader - a row, the checkbox
      // they just ticked, the bar - but not from inside an ask they are answering: an option
      // radio takes no typed text, so the registry passes the key through, and the card's own
      // controls are not the list's. The issue page withholds its writing keys the same way.
      // Inside the column that rule covers the lone Snooze too, the card there being the
      // column's own.
      when: () =>
        (selectedAsks.length > 0 && outsideAskCard()) ||
        (mode === "list" && focusedAskToSnooze()) ||
        (inHand(ROW_SNOOZE_SELECTOR) !== null && (!keysOnShownRow || outsideAskCard())),
    },
    {
      id: "back-from-bulk",
      inEditable: true,
      keys: "Escape",
      label: "Back to the list from the bulk snooze picker",
      run: () => {
        // In the reading view the view's scroller, where the bar's actions came from. Else the
        // row `h` was pressed on, else the first row, passing over every row the bar's own
        // Snooze write is over while that write is pending (`bulkSnoozing`, which the bar empties
        // once the write settles, a refusal or a stop included). The list keeps such a row in its
        // band only until the write lands, and its move into a folded Later then is no press's
        // doing (`causedByReader`), so a focus put on it would fall to the body. With no drawn
        // row left that the bar's write is not taking away, the bar itself is the level out: it
        // holds the count and the way to clear it.
        (mode === "reading"
          ? (paneScroller() ?? bulkBarRef.current)
          : ([originRow(pickerOrigin.current), ...rows()].find(
              (row) => row !== null && !bulkSnoozing.has(row.dataset.inboxRow ?? "")
            ) ?? bulkBarRef.current)
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
      // what they are already in. A reading view below 1280 px hides the box with the list.
      id: "search",
      keys: "f",
      label: "Search asks",
      run: () => searchRef.current?.focus(),
      when: () => searchRef.current !== null && mode !== "reading",
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
  ]);

  /** The rows a Snooze of this view is taking, by id, each with the read the list drew while its
   *  write was out and the write that named it, the row's own or the bar's: from the pick until the
   *  first read the list adopts after that write, which places the row, or until Dispatch refuses
   *  that write. Kept after every commit (below). */
  const snoozedHere = useRef<Map<string, SnoozeName>>(new Map());
  // The bar's hand-off (`useLastMarkedPlace`): the mark of the row the reader marked last while
  // the list holds it, out of sight or not. After a press that took that row away: a row the
  // reader can see at its place, else before it, else after it. With no row marked: the first row
  // the reader can see below whatever the page draws over the list. Each is shown whole at its
  // nearest edge. Else the list's own stop, with no scroll (`barStop`). While a press is taking a
  // listed row off this view (`leavingNow`), the landing is owed and made by the commit that takes
  // those rows away: a Dismiss's or a Move to Inbox's rows (`dismissedHere`), and a Snooze's, the
  // bar's and a row's own (`snoozedHere`). The list keeps a snoozed row in its band only until
  // the write lands, and its move into a folded Later then is no press's doing, so a focus put on
  // it falls to the page's body - which is what the bar's Snooze left in WebKit, eight times in
  // eight repeats (builds Job `infra-3135-infra-3135-r29t-snp-vfht5`). So the name stands until
  // the read that places the row.
  /** Whether a press of the reader's is taking this row off the view: a Dismiss's or a Move to
   *  Inbox's (`dismissedHere`), and a Snooze's, the bar's or a row's own, from the pick until the
   *  first read the list adopts after its write (`snoozedHere`). The one answer the bar's hand-off
   *  is given, the one the hand's own hold reads (`seen`, below), and the one the list's anchor
   *  reads beside its own other reasons (`ViewportAnchor`'s `leaving`, below): the rows the
   *  hand-off waits for are the rows the anchor lets go, so once the landing is made, their going
   *  moves nothing. The anchor names the rows of a commit before that commit's layout effects
   *  run (`getSnapshotBeforeUpdate`), where the Snooze names below are still the last commit's,
   *  so a Snooze's write out (`snoozing`, `bulkSnoozing`) names its rows too: the pick's own
   *  commit is named for it. A row named here is never kept on the page for the hand in it: the
   *  hold that keeps a row under the reader's hand would keep this one for the focus the hand-off
   *  parked on it, and the write that takes it away would never land. */
  const leavingNow = (id: string): boolean =>
    dismissedHere(id) || snoozing.has(id) || bulkSnoozing.has(id) || snoozedHere.current.has(id);
  // The names the hand-off reads, kept after every commit and before it reads them in that commit
  // (`useLastMarkedPlace`, below).
  //
  // A Dismiss's name ends once the page no longer draws its row after Dispatch took it
  // (`forgetDismissed`), and not before: a read that has not dropped the row yet still draws it.
  //
  // A Snooze's name is made while its write is out, with the read the list draws then, and ends at
  // the first read the list adopts after that write. It outlives `snoozing` and `bulkSnoozing`,
  // which empty when the write settles, because a commit after that settle can still draw the
  // row in its band: in WebKit the bar's hand-off read the row marked last still listed there,
  // and named by nothing, in four of eight repeats when the name ended at the settle (builds Job
  // `infra-3135-infra-3135-r30f-s70p`). The row does not wait for that read to leave: under
  // `?group=none` it left in the commit the bar went in, with the read still held (builds Job
  // `infra-3135-infra-3135-r30t-heldp`, Chromium), and the hand-off lands once it has left
  // (`useLastMarkedPlace` waits only while a named row is listed). The first read adopted after
  // the write is the one that places the row out of its band wherever Dispatch took the snooze.
  // No read begun before Dispatch took it can be that read: each snooze write cancels the Inbox's
  // reads in flight at the pick and holds back every read asked while it is out until Dispatch
  // has taken it (`useAskSnooze.tsx`: the row's write through `holds` and `cancelQueries` in
  // `onMutate`, the bar's through `holdBackReads` and `cancelQueries` in `submit`). So a row that
  // read leaves in its band is staying, as a list with no Later band leaves it. The name ends at once where Dispatch refused the write that made
  // it - the row's own, whose refusal stays with the ask until its next press, or the bar's
  // newest pick - since the rollback has put the row's moment back and the row is staying.
  useLayoutEffect(() => {
    const drawn = new Set(rows().map((row) => row.dataset.inboxRow ?? ""));
    forgetDismissed((id) => drawn.has(id));
    const names = snoozedHere.current;
    for (const id of bulkSnoozing) names.set(id, { from: "bar", read: adoptedInbox });
    for (const id of snoozing) names.set(id, { from: "row", read: adoptedInbox });
    for (const [id, name] of names) {
      if (snoozing.has(id) || bulkSnoozing.has(id)) continue;
      const refused =
        name.from === "row" ? snoozeWrites.of(id).error !== undefined : snoozeRefused(id);
      if (refused || adoptedInbox !== name.read) names.delete(id);
    }
  });
  // Below 1280 px with a row open, the page draws that row's reading view in the list's place and
  // hides the list (`page`'s `readingView`), and a focus put in the hidden list falls to the
  // page's body. So there the hand-off reads the view: the view's row is the one row drawn, the
  // view's own checkbox is that row's mark, and the view's scroller is the list's own stop.
  /** The open row's reading view while it stands in the list's place, else null. */
  const barReadingView = (): HTMLElement | null => (mode === "reading" ? paneRef.current : null);
  /** The list's own stop for the bar's hand-off: the reading view's scroller while the view
   *  stands in the list's place, else the list column. */
  const barStop = (): HTMLElement | null =>
    barReadingView() === null ? listScroller.current : paneScroller();
  const landAtPlace = useLastMarkedPlace({
    control: (id) => {
      const view = barReadingView();
      // The view's own mark is the first checkbox in it: its first line, Back and Mark, comes
      // before the row's body (`ReadingView`).
      const row = view === null ? originRow(id) : view.dataset.inboxReadingRow === id ? view : null;
      return row?.querySelector<HTMLElement>(MARK_SELECTOR) ?? null;
    },
    lastMarked,
    leaving: leavingNow,
    list: barStop,
    order: () => {
      const view = barReadingView();
      return view === null
        ? rows().map((row) => row.dataset.inboxRow ?? "")
        : [view.dataset.inboxReadingRow ?? ""];
    },
    shown: bulkBarShown,
  });

  // The bar's Snooze is held from its pick through the commit after its write's answer
  // (`useHeldThroughAnswer`), as a row's own Snooze is, and the hold keeps the armed hand-off of
  // the pick that made the write live (`holdHandOff`). So the bar's going when the write lands,
  // with the rows it snoozed, is the reader's doing, and its hand-off lands the focus: at the
  // reading column's width the short rows draw no Snooze of their own to hold the pick.
  const bulkSnoozeHeld = useHeldThroughAnswer(bulkSnoozing.size !== 0);

  if (inbox.isPending || whoAmI.isPending) {
    return <LoadingSkeleton label="Loading your inbox" />;
  }
  if (inbox.isError || viewer === undefined || place === undefined) {
    return <p className={dangerText}>Could not load your inbox.</p>;
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
  // answered goes once the server has recorded the answer. A row a Snooze of theirs is over goes
  // the same way, press by press the same rows the bar's hand-off is told of (`leavingNow`): the
  // hand-off parks the focus on the mark of the row they marked last while its write is out, and a
  // hold that read the hand alone would keep that row for that focus and never let the write
  // land.
  //
  // The hold is told which rows the typed text excludes rather than being handed the narrowed
  // list: a row the reader's own typing drops must go at once, even with the pointer resting on
  // it, so it is not "seen" here at all and there is nothing to hold. The words are tested only
  // where they can have dropped it: on a row the list did not hold at its last render, or once
  // the reader's words have changed since. A row held under the words the reader still has stays
  // held though the server changes the text they search, since only the reader's own typing
  // drops a row at once. A row the server stopped sending is held exactly as it is today.
  const handId = anchor?.id;
  const typed = words.join(" ");
  const heldBefore = heldUnder.current;
  const heldUnderWords =
    heldBefore !== undefined && heldBefore.id === handId && heldBefore.words === typed;
  const seen =
    handId === undefined || handId === answered.current || leavingNow(handId)
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
  // and the reader's hand does not hold it (`leavingNow`). A row whose Assign to me, hand-on,
  // answer, decline, question back or reply is out is held as the row under the hand is, listed
  // or not, until that write lands.
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
  // gives the agent's asks back. The entry it pushes keeps whether the column picked the row by
  // itself, but it is no row's opening (`inboxOverList`): a row opened below 1280 px, the window
  // widened and the filter cleared, the view's Inbox control still leaves in one step. A host
  // that owns the filter (the drawer) is told of the move instead, as it is of every other
  // chip's removal.
  const clearAgent = () => {
    const next: InboxRoute = { assignees: filter.assignees, item: filter.item, q: filter.q };
    if (onFilterChange) onFilterChange(next);
    else {
      navigate(buildInboxPath(next), {
        replace: false,
        state:
          inboxEntryState(entryState).inboxDefaultPick === true
            ? { inboxDefaultPick: true }
            : undefined,
      });
    }
  };
  const waitingOnYou = filter.section === "needs-you" ? " waiting on you" : "";
  // The agent narrowing's chip is the row's shared removable chip, drawn as `filterChips` draws
  // the other narrowings' chips, and named for what its press does. The agent's live link sits
  // beside it, outside it. `whitespace-pre` keeps the space before an attribution suffix, which
  // a flex item would drop. The box is never wider than the chips line (`max-w-full`), and the
  // chip's own box gives way inside it (`min-w-0`), so a long agent title is cut short with an
  // ellipsis by the chip's own `max-w-full`, as every other chip is, and the live link stays in
  // the line.
  const agentChip =
    agentSubject === undefined ? undefined : (
      <AgentName
        {...agentSubject}
        className="flex max-w-full items-center whitespace-pre"
        control={(name) => (
          <span className="flex min-w-0">
            <Chip
              aria-label="Clear agent filter"
              onClick={clearAgent}
              removable
              selected
              title={`Asks from ${agentTitle}${waitingOnYou}`}
            >
              Asks from {name}
              {waitingOnYou}
            </Chip>
          </span>
        )}
        kind="control"
      />
    );

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
          // read. A Type value names the one read its rows come from: an ask kind's, the Inbox's
          // own read, which answered before this panel was drawn; a thread kind's, the thread read.
          // Every other field counts asks and threads together. So while the thread read is out or
          // refused (`threads.answered`), the ask kinds keep their count and every other value is
          // offered with no count, never a 0 or an asks-only count.
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
            drawn only once the thread read has answered, as every field's above is. */}
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
  // `?section=needs-you` is a narrowing like the rest, so it has a chip that takes it off. With
  // an agent filter on, the agent's own chip already reads "waiting on you" and this would say
  // the same thing twice.
  if (filter.section === "needs-you" && agent === undefined) {
    tokens.push({
      key: "section:needs-you",
      label: "Waiting on you",
      remove: () => changeFilter({ ...filter, section: undefined }),
    });
  }
  const chips =
    agentChip === undefined && tokens.length === 0 ? undefined : (
      <>
        {agentChip}
        {filterChips(tokens)}
      </>
    );
  // **Clear filters**, which the row draws at the end of the chips' line while there are chips:
  // every narrowing off at once - the four fields, who the list is for, the agent and the
  // needs-you section. What the reader typed, the order, the banding and which of the two lists
  // is showing are not narrowings of this panel and stay exactly as they are.
  const clearFilters =
    chips === undefined
      ? undefined
      : () =>
          changeFilter({
            ...filter,
            agent: undefined,
            assignees: undefined,
            filters: undefined,
            section: undefined,
          });

  // The row's Blocked on you control: the asks waiting on the reader plus the credential
  // requests they may decide, with the oldest one's age in its title and its accessible name. A
  // press narrows the list to exactly those asks and leaves the credential requests above it: it
  // takes off the four fields, the assignee and the search text with the section it writes, so
  // the list holds every ask the number counts, and keeps the sort, the grouping and the view. It
  // is absent at a count of zero, and beside an agent filter or in the Dismissed view, where a
  // count over the whole inbox would contradict the list under it. Its number and its oldest age
  // are drawn only once the credential requests and the sign-in have answered: until then, or
  // while the requests' first read has failed, the control is drawn without them wherever an ask
  // already waits, since a number that counted an unread list as empty would be no number of the
  // reader's.
  const waiting = needsYou(adoptedInbox ?? [], credentials.requests, viewer);
  const waitingCounted = credentials.status === "listed" && viewer !== undefined;
  const needsYouControl =
    agent !== undefined || showDismissed || waiting.count === 0 ? null : (
      <button
        aria-label={blockedOnYouText(waitingCounted ? waiting : undefined)}
        className={PAGE_CONTROL_CLASS}
        data-inbox-needs-you=""
        onClick={() =>
          changeFilter({
            ...filter,
            assignees: undefined,
            filters: undefined,
            q: undefined,
            section: "needs-you",
          })
        }
        title={blockedOnYouText(waitingCounted ? waiting : undefined)}
        type="button"
      >
        {waitingCounted ? `Blocked on you · ${waiting.count}` : "Blocked on you"}
      </button>
    );
  // The page's own controls, or nothing at all: an empty slot would still cost the row a gap.
  const pageControls =
    !markAll.offered && markAll.failure === undefined && needsYouControl === null ? undefined : (
      <>
        {/* Only while the list holds an unread Reply or Agent row. It asks nothing first: the
            rows go, and a refusal brings them back; a press the reader stopped may have marked
            them, so the list is read again rather than brought back. */}
        {markAll.offered ? (
          <button
            className={PAGE_CONTROL_CLASS}
            data-inbox-mark-all=""
            onClick={markAll.markAll}
            type="button"
          >
            Mark all read
          </button>
        ) : null}
        {markAll.failure === "refused" ? (
          <p
            className={`text-sm whitespace-nowrap ${dangerText}`}
            data-inbox-mark-all-refused=""
            role="alert"
          >
            {MARK_ALL_REFUSED_TEXT}
          </p>
        ) : markAll.failure === "stopped" ? (
          <p className={`text-sm ${dangerText}`} data-inbox-mark-all-stopped="" role="alert">
            {MARK_ALL_STOPPED_TEXT}
          </p>
        ) : null}
        {needsYouControl}
      </>
    );

  // How the list is ordered, how it is banded, which of the reader's two lists is showing, and
  // which way a thread's own replies read. The first three are the list's, so they ride in the
  // address and a link carries them; the thread order is the reader's own and is kept for their
  // browser (`threads/thread-order.ts`).
  const displayMenu = (
    <DisplayMenu
      label="View"
      onOpenChange={setViewOpen}
      open={viewOpen}
      sections={[
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
            changeFilter({ ...filter, show: next === "dismissed" ? "dismissed" : undefined }),
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
      ]}
    />
  );

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
  const selectionBar = (
    <SelectionBar
      barRef={bulkBarRef}
      count={bulkBarShown ? selectedCount : 0}
      label="Selected rows"
      notice={snoozeRefusal}
      // The bar gone with the focus, or the control in it that held the focus: the hand-off above
      // (`landAtPlace`) puts the focus itself. The list's own stop (`barStop`) is the last place
      // the focus can go.
      returnFocus={() => (landAtPlace.land() ? null : focusListStop(barStop()))}
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

  // Everything above the list, the same whether or not any band has a row, and a sibling of the
  // scrolling list rather than an item in it: the controls stay put while the rows move.
  //
  // One row holds the page's search, sorting and filtering, and nothing else between the title
  // and the first row draws a search box, a panel trigger or a chip. Nothing the reader can press
  // here makes the header taller: the filter panel and the display menu are portalled over the
  // page (`AnchoredPanel`), the selection bar is drawn over the list, and the chips are the one
  // part of the row that may take a second line.
  const header = (
    <div className="space-y-4">
      <CredentialRequestsSection credentials={credentials} />
      {/* The plans of the person's runs that wait for them, which they approve here. */}
      <WaitingPlans />
      <ControlRow
        chips={chips}
        display={displayMenu}
        onClear={clearFilters}
        search={
          <SearchBox
            className="w-full"
            label="Search asks"
            // `capSearchQuery` again here, over the box's own `maxLength`: a paste the box
            // takes whole is still cut to what the address and the list agree on (`typeQuery`).
            onChange={typeQuery}
            placeholder="Issue, title, question or author"
            ref={searchRef}
            value={filter.q ?? ""}
          />
        }
        trigger={
          <PanelTrigger label="Filter" onOpenChange={setFilterOpen} open={filterOpen} stacked>
            {renderFilterControls}
          </PanelTrigger>
        }
      >
        {pageControls}
      </ControlRow>
    </div>
  );
  // What the reader's own writes left to say: a dismiss the server refused - the rows are back
  // where they were, and the page says so here, because the control that sent it was on a row
  // this write had already taken off the page - and that the row they were reading left their
  // Inbox. A sibling of the header rather than part of it, so the reading view below 1280 px,
  // which hides the header with the list, still shows both where the reader is: a Dismiss
  // pressed in the view's head is refused there. The selection bar stands over the list on its
  // own (`selectionBar`), so `h` with rows marked reaches its Snooze from the view as well.
  const notices = (
    <>
      {dismissWrite.refusal === undefined ? null : (
        <p className={`text-sm ${dangerText}`} data-inbox-dismiss-refused="" role="alert">
          {dismissWrite.refusal}
        </p>
      )}
      {/* Always mounted, and empty while there is nothing to say, so a screen reader hears the
          line as it appears; empty, it takes no room. */}
      <p
        className={`text-sm empty:sr-only ${textMutedOnCanvas}`}
        data-inbox-row-left=""
        role="status"
      >
        {rowLeft ? "This item left your Inbox." : null}
      </p>
    </>
  );

  // Two columns from 1280 px, the reading column to the right of the list, each scrolling in
  // itself inside the height the page gives them (`app.tsx` makes the Inbox page fill the
  // viewport here). Below that width, a row the reader opened is read in place of the list
  // and its header, under the view's own Inbox control, in a box that scrolls in itself for the
  // same reason (`useInboxFillsViewport`); the list and the header stay mounted, hidden, so the
  // reader comes back to them as they left them.
  //
  // One root, selection or none: the column is what appears and disappears beside the list,
  // never the list itself. A selection empties for a commit whenever the selected row leaves
  // the rendered bands - the reader's own bulk snooze of that row does it, before the effect
  // above hands the selection on - and a root that changed shape there would have React
  // unmount the whole list, taking the keyboard's focus, the bulk bar and every row's state
  // with it and mounting a fresh one a commit later.
  const page = (listColumn: ReactNode, subject: InboxListRow | undefined): ReactNode => {
    const readingView = mode === "reading" && subject !== undefined;
    // The row's reading pane: the same one beside the list and in the reading view.
    const readingPane =
      subject === undefined ? null : (
        <ReadingPane onAnswered={recordAnswered} row={subject} viewer={viewer} />
      );
    return (
      // Every thread this list draws - in the column or the reading view, and in a card of the
      // drawer - draws its own Dismiss from here (`DismissControl`).
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
          {/* Outside the layout box below, because it is `fixed`: the bar is drawn over the list
              rather than in it, so the first mark moves no row. */}
          {selectionBar}
          <div
            className={
              mode === "columns" || readingView ? "flex min-h-0 flex-1 flex-col gap-4" : "space-y-4"
            }
          >
            <div hidden={readingView}>{header}</div>
            {notices}
            {mode === "columns" ? (
              // The two columns are a grid of two fractions, so the share the reader dragged the
              // divider to is exactly the share the pane takes, at any width; the divider is the
              // column between them and takes its own.
              //
              // One row, `minmax(0, 1fr)`: an implicit `auto` row is as tall as the taller
              // column, so a long document in the pane would make the row - and with it both
              // columns' boxes - taller than the page, and neither column would scroll in
              // itself. The page would scroll instead, which is how the pane came to show a
              // document's top however far down it the reader's section sat.
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
                      {readingPane}
                    </div>
                  </>
                )}
              </div>
            ) : (
              <>
                <div hidden={readingView}>{listColumn}</div>
                {readingView ? (
                  <ReadingView
                    marked={marked.has(rowId(subject))}
                    onBack={paneSelection.close}
                    onMark={onMark}
                    row={subject}
                    viewRef={holdPane}
                  >
                    {readingPane}
                  </ReadingView>
                ) : null}
              </>
            )}
          </div>
        </BulkSnoozingContext>
      </RowDismissContext>
    );
  };

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
    return page(empty, paneSelection.showing([]));
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
  // Every row the bands hold, a folded one's included: `?item=` naming a row the reader deferred
  // opens that band rather than being read as an address this list cannot answer.
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
      onMark,
      onRelease,
      ordinal: row.kind === "ask" ? ordinals.get(row.ask.id) : undefined,
      row,
      section,
    };
    if (mode !== "cards") {
      return row.kind === "ask" ? (
        <AskShortRow
          {...shared}
          ask={row.ask}
          key={id}
          onSelect={openRow}
          selected={id === paneSelection.id}
          viewer={viewer}
        />
      ) : (
        <ThreadShortRow
          {...shared}
          key={id}
          onSelect={openRow}
          selected={id === paneSelection.id}
          thread={row.thread}
          titleOpensRow={mode !== "columns"}
        />
      );
    }
    return row.kind === "ask" ? (
      <InboxCardRow
        {...shared}
        ask={row.ask}
        key={id}
        onAnswered={recordAnswered}
        threadUpdatedAt={inbox.dataUpdatedAt}
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
  // are the rows the bar's hand-off waits for (`leavingNow`), a Dismiss's, a Move to Inbox's and a
  // Snooze's, a row's own or the bar's, for as long as the hand-off waits for them: a snoozed row
  // the motion hold keeps drawn after its write lands is named until it goes, and its going after
  // the landing moves no row (the tester's red row of INFRA-3135's round 29, builds Job
  // `infra-3135-i-h5-g6tnd`, where the anchor named it only while the write was out and scrolled
  // the list back by its height). Beside them, the rows an Assign to me, a hand-on, an answer, a
  // decline, a question back or a reply lets go of, which then leave the list where the read no
  // longer lists them there, and the rows of a band the reader's press on its toggle folds shut
  // (`folding`), in the commit that folds it.
  const list = (
    // With the reading column beside it the list scrolls in itself rather than in the window,
    // so the two columns move independently; the anchor is given that same box, so a refetch
    // keeps the reader's row where it was in it. Without the column the rows scroll in whatever
    // the host gives (the drawer's own `aside`), and in the window when it gives nothing. The
    // scroller reaches 20 px left of the rows, into the page's own inset, so it does not clip
    // the press square of a row's checkbox (`MarkCheckbox`). The column is the list's own stop
    // for the selection bar's hand-off, so it is named.
    <section
      aria-label="Inbox list"
      className={mode === "columns" ? "-ml-5 min-w-0 flex-1 overflow-y-auto pr-1 pl-5" : undefined}
      data-testid="inbox-list"
      ref={listScroller}
    >
      <ViewportAnchor
        className="space-y-4"
        group="data-inbox-section"
        item={ROW_ATTRIBUTE}
        leaving={(id) =>
          leavingNow(id) || askWriteOut(id) || replying.has(id) || folding.current?.has(id) === true
        }
        ref={viewport}
        rootRef={listRef}
        scroller={mode === "columns" ? listScroller : scroller}
      >
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
  // in the drawer. The one question that outlives every row is the one the column or the
  // reading view is keeping for an unsent answer, and `showing` hands that one back from an
  // empty list too, so it keeps its column there: the reader's text is in it.
  return page(list, selectedRow);
}
