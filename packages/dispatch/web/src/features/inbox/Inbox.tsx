import { awaitsDecision, INBOX_DISMISSED_PAGE, INBOX_THREAD_KINDS } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import {
  type FocusEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useLayoutEffect,
  useMemo,
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
import { heldPress, useHeldThroughAnswer } from "../../components/held-press";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { MultiSelect } from "../../components/MultiSelect";
import { PanelTrigger } from "../../components/PanelTrigger";
import { LabelPill } from "../../components/Pill";
import { QueryError } from "../../components/QueryError";
import { SearchBox } from "../../components/SearchBox";
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
  textMutedOnSurfaceMuted,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { searchWords } from "../agents/agent-search";
import { CredentialRequestsSection } from "../credentials/CredentialRequestsSection";
import { type CredentialRequests, useCredentialRequests } from "../credentials/pending";
import { issuePriority } from "../issue/issue-priority";
import { PriorityControl } from "../issue/PriorityControl";
import { AgentName, useAgentText } from "../refs/AgentName";
import { CopyRefButton } from "../refs/CopyRefButton";
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
import { DIALOG_SCOPE, type KeymapScope } from "../shell/keymap";
import { closestMatching } from "../shell/roving";
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
import { type AskOrdinal, askOrdinals, controlName } from "./ask-name";
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
import { ROW_ATTRIBUTE, rowIn, rowsIn } from "./inbox-row";
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
import { OptionDigitsContext } from "./option-digits";
import {
  AskAnswerWritesContext,
  AskClarificationWritesContext,
  AskDeclineWritesContext,
  AskReplyWritesContext,
  useOutstandingAnswerWrites,
} from "./outstanding-answers";
import { PaneDivider, usePaneWidth } from "./PaneDivider";
import {
  InboxHandOffContext,
  type PlacedRow,
  RowAsideContext,
  usePaneSelection,
} from "./pane-selection";
import { useFoldedGroups } from "./preferences";
import { questionLine } from "./question-line";
import { ReadingPane } from "./ReadingPane";
import { RowKindIcon, RowPriorityTag, RowUrgencyMark } from "./RowMarks";
import {
  BULK_PICKER_MARKS,
  type BulkSnoozing,
  BulkSnoozingContext,
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
import { INBOX_NOT_LOADED, ThreadRowCard } from "./ThreadRowCard";
import { ThreadTitle, useThreadLine } from "./ThreadTitle";
import { answerFailure, clarificationRefusal } from "./useAskAnswerForm";
import { AskHandOnWritesContext, useAskHandOnWrites } from "./useAskHandOn";
import { AskSnoozeContext, AskSnoozeWritesProvider, useAskSnooze } from "./useAskSnooze";
import type { InboxDismissRow } from "./useInboxDismiss";
import { useInboxKeymap } from "./useInboxKeymap";
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
 *  <agent>") is the card's own status line. */
function InboxRowChip({ ask }: { ask: InboxRow }): ReactNode {
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

/** The row's own checkbox, the pointer's way to the mark `x` makes. Every row takes one: with
 *  rows marked the bulk bar offers **Snooze** for the asks among them and **Dismiss** for the
 *  threads. */
function MarkCheckbox({
  id,
  marked,
  name,
  onMark,
}: {
  id: string;
  marked: boolean;
  name: string;
  onMark: (id: string) => void;
}): ReactNode {
  return (
    <input
      aria-label={`Select ${name}`}
      checked={marked}
      className={`size-4 shrink-0 ${checkboxAccent}`}
      onChange={() => onMark(id)}
      type="checkbox"
    />
  );
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

/** What a short row adds to every row layout's props: beside the reading column the row is
 *  selectable, and the one the column shows is drawn as selected. */
interface ShortRowProps {
  onSelect: (id: string) => void;
  /** Whether this is the row the reading column is showing. */
  selected: boolean;
}

/**
 * The frame both short rows draw in, two lines deep: the kind's icon at the left edge, then
 * what the row is about and when it was last written in; under them the priority tag, an ask's
 * urgency, and what the row says about its owner. Every mark is `shrink-0`, so the title is
 * what shortens and neither the marks nor the key are ever cut. `children` is drawn under the
 * two lines.
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
  /** The second line below the marks: the row's owner, and whatever it carries beside it. */
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
        <div className="flex min-w-0 items-baseline gap-2 pl-6">
          <RowPriorityTag priority={rowPriority(row)} />
          <RowUrgencyMark urgency={rowUrgency(row)} />
          {owner}
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
  onAside,
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
    /** A write pressed on this row while it is not the item in hand (`RowAsideContext`). */
    onAside: (id: string) => void;
    /** The signed-in person's lowercase email; "Assign to me" writes it. */
    viewer: string;
  }): ReactNode {
  const name = controlName(ask, ordinal);
  const snooze = useAskSnooze(ask.id);
  const assign = useAskAssign(ask.id);
  const handOn = useInboxWrites(AskHandOnWritesContext).of(ask);
  const answer = useInboxWrites(AskAnswerWritesContext).of(ask.id);
  const decline = useInboxWrites(AskDeclineWritesContext).of(ask.id);
  const clarification = useInboxWrites(AskClarificationWritesContext).of(ask.id);
  return (
    <ShortRowFrame
      age={formatAskAge(rowLastActivityAt(row))}
      check={<MarkCheckbox id={ask.id} marked={marked} name={name} onMark={onMark} />}
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      owner={<InboxOwnerLink ask={ask} rowOwner />}
      row={row}
      section={section}
      selected={selected}
      title={<span className="min-w-0 grow truncate text-sm">{questionLine(ask.question)}</span>}
    >
      {/* The column's head draws the Snooze and the Assign to me of the question the column
          shows, and a short row draws neither of its own. A snooze or an Assign to me of this
          ask can be out or failed while the column shows another question: the reader moved the
          selection after pressing it in the head. The row draws the control then, so its
          `Snoozing…` or `Assigning…`, its refusal and its Retry stay on the row they are
          about. The same for the column's card, which draws the hand-on, the answer, the
          decline and the question back of the question the column shows: each can be out or
          failed while the column shows another question. The row draws the hand-on control
          then, with its `Handing on…`, its refusal and its Retry, and a line for an answer, a
          decline or a question back, which says it is out or why it failed. That Retry is the
          card's, once the column shows the question again. The row is not the item in hand
          while it draws them, so a press of any of them hands nobody on (R8 is the item in
          hand's) and takes a focus it left in the row to the item being read first. */}
      {selected ? null : (
        <RowAsideContext value={() => onAside(ask.id)}>
          {!(snooze.pending || snooze.failed) ? null : (
            <div className="flex max-w-full flex-wrap items-center gap-1 pl-6">
              <SnoozeControl
                askId={ask.id}
                label={name}
                snoozedUntil={isSnoozed(ask) ? ask.snoozed_until : null}
              />
            </div>
          )}
          {ask.issue_key === null || !(assign.pending || assign.failed) ? null : (
            <div className="flex max-w-full flex-wrap items-center gap-1 pl-6">
              <AssignToMe askId={ask.id} issueKey={ask.issue_key} viewer={viewer} />
            </div>
          )}
          {!(handOn.pending || handOn.failed) ? null : (
            <div className="pl-6">
              <AskHandOn ask={ask} assignee={ask.issue?.assignee ?? null} />
            </div>
          )}
        </RowAsideContext>
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
  const line = useThreadLine(thread);
  return (
    <ShortRowFrame
      age={<Timestamp at={rowLastActivityAt(row)} />}
      check={<MarkCheckbox id={rowId(row)} marked={marked} name={line} onMark={onMark} />}
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      owner={
        <>
          <ThreadOwnerLine className={`truncate text-sm ${textMutedOnCanvas}`} thread={thread} />
          {reference === undefined ? null : <CopyRefButton item route={reference} />}
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
  ask: InboxRow;
  onAnswered: (id: string) => void;
  /** Timestamp of the Inbox snapshot that supplied this row's initial thread. */
  threadUpdatedAt: number;
  /** The signed-in person's lowercase email; "Assign to me" writes it. */
  viewer: string;
}): ReactNode {
  const name = controlName(ask, ordinal);
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
          issue key. */}
      <div className="mb-1.5 flex flex-wrap items-center gap-x-2 gap-y-1">
        <MarkCheckbox id={ask.id} marked={marked} name={name} onMark={onMark} />
        <RowKindIcon type={rowType(row)} />
        <div className="flex min-w-0 grow basis-48 items-baseline gap-2">
          <InboxOwnerLink ask={ask} rowOwner />
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
  const line = useThreadLine(thread);
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
        <MarkCheckbox id={rowId(row)} marked={marked} name={line} onMark={onMark} />
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
 * `filter`/`onFilterChange`/`keymapScope` exist for `InboxDrawer` (LEGION-547): a peek
 * embedded on another route must not read or write the real URL for its filters or the text
 * typed in its search box (either would navigate the page it sits over), and its
 * Escape/j/k/etc. must resolve under the drawer's own `dialog` scope - the modal scope a dialog
 * anywhere on the stack exclusively consults - rather than the standalone page's `inbox` scope,
 * which a dialog's keydown handling never reaches. `pane` is the third: the drawer is a peek at
 * any width, never a two-column reader. `InboxPage`'s unmodified `<Inbox/>` call keeps today's
 * URL-backed behaviour untouched.
 */
export interface InboxProps {
  filter?: InboxRoute;
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
 *  that ask's one write of its kind, which the Inbox holds, whichever control pressed it. */
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
  const navigate = useAddressNavigate();
  const filter = filterOverride ?? addressRoute;
  // The reading column shows from 1280 px, and only where the host allows one: below that, and
  // in the drawer, the Inbox is the list of cards it has always been.
  const wideEnoughForPane = useMediaQuery(READING_PANE_QUERY);
  const paneShown = pane && wideEnoughForPane;
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
   *  as well, and it is the list's own stop for the selection bar's hand-off
   *  (`useLastMarkedPlace`). */
  const listScroller = useRef<HTMLElement>(null);
  /** Everything this Inbox draws below the bar of selected rows - the header, the list or its
   *  empty message, and the column - where the row a hand-off sends the reader to is looked up
   *  (`usePaneSelection`), so the drawer's list never reaches into the page under it. */
  const regionRef = useRef<HTMLDivElement>(null);
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
    dismissPick,
    dismissRows,
    dismissWrite,
    lastMarked,
    marked,
    onMark,
    selectAll,
    selectedAsks,
    selectedThreads,
    snoozeLate,
    snoozePick,
    snoozeRefusal,
    snoozing: bulkSnoozing,
  } = useInboxSelection({
    asOf: narrowingMark,
    candidates,
    drawnIds,
    filters,
    rows: byId,
    showDismissed,
  });
  const paneSelection = usePaneSelection({
    candidates,
    filter,
    listRef,
    openFoldedBand: useCallback(() => openFoldedBand(LATER_GROUP_KEY), [openFoldedBand]),
    paneShown,
    presented,
    region: regionRef,
    rowsPending: threads.pending,
  });
  // The refetch a recorded answer triggers returns the list the optimistic removal already
  // produced, so it re-renders nothing on its own; release explicitly. The reader's own answer -
  // an answer, an approval, a request for changes, a decline - hands the reader on to the item
  // after it (R8) once the server has recorded it, not at the press: the list keeps the row where
  // the reader saw it until the server decides their answer (`heldRow`), so a refusal shows on
  // its own card. A reader who moved on meanwhile - an approval reviewed from the head, a row
  // they walked off - is not pulled back (`landHandOff`).
  const recordAnswered = (id: string) => {
    answered.current = id;
    paneSelection.landHandOff([id]);
    release();
  };
  /** One dismiss, or Move to Inbox, of the rows `named` - the bar's own press is `dismissPick`:
   *  its optimistic removal is immediate and a refusal shows above the list, so the reader is
   *  handed on to the item after them at once (R8). */
  const dismissNow = (
    named: readonly InboxDismissRow[],
    dismiss: () => void = () => dismissRows(named)
  ) => {
    const ids = named.map(({ id }) => id);
    paneSelection.beginHandOff(ids);
    dismiss();
    paneSelection.landHandOff(ids);
  };
  // Every key the Inbox binds, the record of the row `l` was pressed on, which the column's
  // wrapper below holds and drops, the column's scroller, where `l` puts the focus, and the row
  // the bar's Snooze was reached from, where Escape from it goes back to (`useInboxKeymap.ts`).
  const { forgetPaneReturn, holdPane, notePickerOrigin, paneScroller } = useInboxKeymap({
    barOpen,
    bulkBarRef,
    bulkBarShown,
    bulkSnoozing,
    clearSelection,
    dismiss: dismissNow,
    listRef,
    onMark,
    paneSelection,
    paneShown,
    presented,
    rowsById: byId,
    scope: keymapScope,
    searchRef,
    selectedAsks,
  });
  /** A row's own Dismiss (`DismissControl`, through `RowDismissContext`). Beside the column only
   *  the selected row is the item in hand, so the Dismiss the pointer reaches on another row
   *  takes that row out and leaves the selection, the address and the focus on the item being
   *  read; the selected row's Dismiss, the column head's, and every card's below the column's
   *  width hand the reader on to the item after it (R8). A press that left the focus in the row
   *  it takes out - the Dismiss takes it as the pointer presses it - hands it first, with no
   *  scroll, to the item being read: to its row, and where the list draws no row for it, to the
   *  column's scroller, the stop `l` hands the focus to. The list draws none while the column
   *  keeps a question past its row (`paneRow`): the reader's unsent answer keeps it there once a
   *  search or a filter hides its row or the server takes it away, and their sent answer keeps
   *  it until the server decides it. Left in the row, the focus would go with it and fall onto
   *  the page, away from the item being read. */
  const dismissFromRow = (named: InboxDismissRow) => {
    if (paneShown && named.id !== paneSelection.id) {
      asideFromRow(named.id);
      dismissRows([named]);
    } else {
      dismissNow([named]);
    }
  };
  /** A press on the row `id` beside the column while it is not the item in hand, which hands
   *  nobody on: a row's own Dismiss, and the Snooze, the Assign to me and the hand-on a short row
   *  draws while that write is out or refused (`RowAsideContext`). A focus the press left in the
   *  row, which the write may take out of the list, goes first, with no scroll, to the item being
   *  read - its row, else the column's scroller, as `dismissFromRow` says - and not to a control of
   *  the row that moves up into its place. */
  const asideFromRow = (id: string) => {
    if (rowIn(listRef.current, id)?.contains(document.activeElement) === true) {
      (paneSelection.selectedRow() ?? paneScroller())?.focus({ preventScroll: true });
    }
  };

  // The bar's hand-off (`useLastMarkedPlace`): the mark of the row the reader marked last while
  // the list draws it and no press is taking it away, out of sight or not. After a press that took
  // that row away: a row the reader can see at its place, else before it. With no row marked: the
  // first row the reader can see below whatever the page draws over the list. Each is shown whole
  // at its nearest edge. Else the list column's own stop, with no scroll. A row a Dismiss or a
  // Move to Inbox press is taking off this view is passed over: it is still drawn until the
  // write's optimistic apply lands, and a focus handed to it would drop with it. `dismissedHere`
  // names every such row - each id the dismiss write's `pending` holds, and the ids of a press
  // made under the narrowing, which `pending` leaves out. Each row after such rows is judged where
  // it will stand once they have gone, as the list's anchor lets them go (`leaving` below); where
  // their going leaves the row at the place wholly above the view, the nearest row after it in
  // sight takes the focus. A mark that will move up into sight then takes the focus with no
  // scroll, and the anchor shows it whole once those rows have gone.
  const landAtPlace = useLastMarkedPlace({
    control: (id) => rowIn(listRef.current, id)?.querySelector<HTMLElement>(MARK_SELECTOR) ?? null,
    lastMarked,
    leaving: dismissedHere,
    list: () => listScroller.current,
    order: () => rowsIn(listRef.current).map((row) => row.dataset.inboxRow ?? ""),
    shown: bulkBarShown,
  });

  // The bar's Snooze is held from its pick through the commit after its write's answer
  // (`useHeldThroughAnswer`), as a row's own Snooze is, and the hold keeps the armed hand-off of
  // the pick that made the write live (`holdHandOff`). So the bar's going when the write lands,
  // with the rows it snoozed, is the reader's doing, and its hand-off lands the focus: at the
  // reading column's width the short rows draw no Snooze of their own to hold the pick. Its
  // writes are tried under their keys until Dispatch decides them, so once every one still out is
  // past its deadline (`snoozeLate`) the bar's Snooze, and each row's under it, is let go: offline
  // the pick holds them no longer than that.
  const bulkSnoozeHeld = useHeldThroughAnswer(bulkSnoozing.size !== 0 && !snoozeLate);
  const bulkSnoozingShown = useMemo<BulkSnoozing>(
    () => ({ asks: bulkSnoozing, late: snoozeLate }),
    [bulkSnoozing, snoozeLate]
  );

  if (inbox.isPending || whoAmI.isPending) {
    return <LoadingSkeleton label="Loading your inbox" />;
  }
  if (inbox.isError || viewer === undefined || place === undefined) {
    return <p className={dangerText}>{INBOX_NOT_LOADED}</p>;
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
      // The bar gone with the focus, or the control in it that held the focus: the row a press
      // of the bar hands the reader on to (R8), which the hand-off focuses itself, with no scroll
      // while the rows the press takes away still stand above it (`focusHandOff`): the bar's own
      // focus would scroll the view to that row where it stands below them, and their going then
      // carries it past the place the reader saw. Else the hand-off above (`landAtPlace`) puts the
      // focus itself. The list column's own stop is the last place the focus can go: a Move to
      // Inbox that empties the Dismissed view leaves no other row, and the view's empty message
      // takes the focus from there once it is drawn.
      returnFocus={() =>
        paneSelection.focusHandOff() || landAtPlace() ? null : focusListStop(listScroller.current)
      }
      size={BAR_SIZE}
      state={barState}
    >
      {/* The picker stays while its own write is out. While Later is folded, the optimistic
          moment takes every marked ask out of the asks the selection counts (`selectedAsks`),
          and under the Blocked on you narrowing out of the list. A picker unmounted under the
          reader's hand drops their focus. It snoozes only, so it offers no Un-snooze. */}
      {selectedAsks.length === 0 && bulkSnoozing.size === 0 ? null : (
        // One pick at a time: a second while the first is in the air re-sends every id, and its
        // answer can settle after the live one's and overwrite what the reader was told. It is
        // held by `aria-disabled` rather than `disabled` because the browser takes focus off a
        // control that disables itself under it - dropping the reader on the document mid-write,
        // with no Escape to leave and no way back to the bar. The focus arriving on it, or `b`
        // and `h` pressing it, records the row the reader came from, for Escape from it to go
        // back to; the list handing the focus back records nothing, so the row outlives a pick
        // (`onTriggerFocus`, `useInboxKeymap`).
        <SnoozePick
          badge="Snooze"
          held={bulkSnoozeHeld}
          label="Snooze selected asks"
          marks={BULK_PICKER_MARKS}
          onPick={(until) => {
            // The reader is handed on to the item after the last of the asks once the server
            // has taken them (R8); a pick it refused whole sends nobody anywhere.
            const ids = [...selectedAsks];
            paneSelection.beginHandOff(ids);
            void snoozePick(until).then((taken) =>
              taken.length === 0 ? paneSelection.dropHandOff(ids) : paneSelection.landHandOff(taken)
            );
          }}
          onTriggerFocus={notePickerOrigin}
        />
      )}
      {selectedThreads.length === 0 ? null : (
        <button
          aria-label={`${dismissLabel} selected threads`}
          className={BAR_CONTROL_CLASS}
          data-inbox-bulk-dismiss=""
          onClick={() => dismissNow(selectedThreads, dismissPick)}
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
      <ControlRow
        chips={chips}
        display={displayMenu}
        // Shift+F and Shift+V under the scope the rest of this list's keys take: the page's own,
        // or the drawer's `dialog`.
        keyScope={keymapScope === DIALOG_SCOPE ? "dialog" : "inbox"}
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
    // Dismiss from here (`DismissControl`), which hands the reader on only from the item in hand
    // (`dismissFromRow`).
    <RowDismissContext
      value={{
        dismiss: dismissFromRow,
        held: dismissWrite.held,
        pending: dismissWrite.pending,
        show,
      }}
    >
      {/* Every Snooze this list draws - a row's, and the column head's - is held while the
            bar's own Snooze write is out over its ask within its deadline (`SnoozeControl`). */}
      <BulkSnoozingContext value={bulkSnoozingShown}>
        {/* Every Snooze this list draws - on a row, and in the column's head - hands the reader
            on to the item after the one it snoozed once the server has taken it (R8). */}
        <InboxHandOffContext value={paneSelection}>
          {/* Every card this list draws shows the digits that pick its options while it is the
            item in hand (R17, `option-digits.ts`): beside the column, the selected question's
            card; below its width, the card of the row the reader holds. */}
          <OptionDigitsContext
            value={
              paneShown ? { askId: paneSelection.id, kind: "selected" } : { kind: "focused-row" }
            }
          >
            {/* Outside the layout box above, because it is `fixed`: the bar is drawn over the list
              rather than in it, so the first mark moves no row (INFRA-3135, R4). */}
            {selectionBar}
            <div
              className={paneShown ? "flex min-h-0 flex-1 flex-col gap-4" : "space-y-4"}
              ref={regionRef}
            >
              {header}
              {paneShown ? (
                // The two columns are a grid of two fractions, so the share the reader dragged the
                // divider to is exactly the share the pane takes, at any width; the divider is the
                // column between them and takes its own.
                //
                // One row, `minmax(0, 1fr)`: an implicit `auto` row is as tall as the taller
                // column, so a long document in the pane would make the row - and with it both
                // columns' boxes - taller than the page, and neither column would scroll in itself.
                // The page would scroll instead, which is how the pane came to show a document's
                // top however far down it the reader's section sat.
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
          </OptionDigitsContext>
        </InboxHandOffContext>
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
    // its column here too: the reader's text is in it. The message takes the keyboard focus a
    // hand-off gives it (R8), so the Inbox's keys still work from there; where there is no
    // message to say yet, the empty list itself does.
    presented.current = [];
    presentedBands.current = { headings: [], layout };
    const empty = (
      // The list column is the list's own stop, so it is named: a labelled `<section>` is a region.
      // Where there is no message to say yet, it is what a hand-off gives the focus to (R8).
      <section
        aria-label="Inbox list"
        data-inbox-empty={emptyMessage === undefined ? "" : undefined}
        data-testid="inbox-list"
        ref={listScroller}
        tabIndex={emptyMessage === undefined ? -1 : undefined}
      >
        {emptyMessage === undefined ? null : (
          <EmptyState
            label="Inbox empty state"
            marks={{ "data-inbox-empty": "" }}
            message={emptyMessage}
            tabIndex={-1}
          />
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
    if (paneShown) {
      return row.kind === "ask" ? (
        <AskShortRow
          {...shared}
          ask={row.ask}
          key={id}
          onAside={asideFromRow}
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
  // are the rows a Dismiss or a Move to Inbox is taking off the view (`dismissedHere`), the rows
  // a snooze write, a row's own or the bar's, moves to another band in the commit that lets go of
  // them, the rows an Assign to me, a hand-on, an answer, a decline, a question back or a reply
  // lets go of, which then leave the list where the read no longer lists them there, and the rows
  // of a band the reader's press on its toggle folds shut (`folding`), in the commit that folds it.
  //
  // The list draws no row while every row it holds is in a folded band: a snooze of the only row
  // shown, with the Later band folded, leaves it so. A row is still left - the snoozed one,
  // counted on the band's toggle - so the list draws no empty message, whose words say it holds
  // nothing, and the list itself is what a hand-off gives the focus to then (R8,
  // `pane-selection.ts`), as the empty list is where it has no message to say yet.
  const drawsNoRow = presented.current.length === 0;
  const list = (
    // With the reading column beside it the list scrolls in itself rather than in the window,
    // so the two columns move independently; the anchor is given that same box, so a refetch
    // keeps the reader's row where it was in it. Without the pane the rows scroll in whatever
    // the host gives (the drawer's own `aside`), and in the window when it gives nothing. The
    // column is the list's own stop for the selection bar's hand-off, so it is named.
    <section
      aria-label="Inbox list"
      className={paneShown ? "min-w-0 flex-1 overflow-y-auto pr-1" : undefined}
      data-inbox-empty={drawsNoRow ? "" : undefined}
      data-testid="inbox-list"
      ref={listScroller}
      tabIndex={drawsNoRow ? -1 : undefined}
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
  // below the reading column's width. The one question that outlives every row is the one the
  // column is keeping for an unsent answer, and `showing` hands that one back from an empty
  // list too, so it keeps its column there: the reader's text is in it.
  return page(list, selectedRow);
}
