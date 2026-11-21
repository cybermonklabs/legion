import { awaitsDecision, INBOX_DISMISSED_PAGE } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import {
  type FocusEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useRef,
  useState,
} from "react";
import { Link } from "react-router-dom";

import { inboxQuery, usersQuery, whoAmIQuery } from "../../api/queries";
import type { InboxRow, MyReplyRow } from "../../api/types";
import { ChoiceMenu } from "../../components/ChoiceMenu";
import { ControlRow } from "../../components/ControlRow";
import { controlPadding } from "../../components/control-names";
import { DisplayMenu, type DisplayMenuSection } from "../../components/DisplayMenu";
import { EmptyState } from "../../components/EmptyState";
import { type FilterChip, filterChips } from "../../components/filter-chips";
import { heldPress } from "../../components/held-press";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { MultiSelect } from "../../components/MultiSelect";
import { PanelTrigger } from "../../components/PanelTrigger";
import { LabelPill } from "../../components/Pill";
import { SearchBox } from "../../components/SearchBox";
import { SelectionBar } from "../../components/SelectionBar";
import { useNestedPanels } from "../../components/useNestedPanels";
import { capSearchQuery } from "../../lib/page-address";
import {
  badgeLow,
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
import { badgeMenuTrigger, badgeSelectBadge } from "../issue/badge-select";
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
import { InboxOwnerLink, ThreadOwnerLine } from "./AskOwnerLink";
import { AssignToMe } from "./AssignToMe";
import { formatAskAge } from "./ask-age";
import { type AskOrdinal, askOrdinals, controlName } from "./ask-name";
import { askUrgency } from "./ask-urgency";
import { blockedOnYouText, needsYou, waitingOnHuman } from "./BlockedOnYou";
import { DismissControl, RowDismissContext } from "./DismissControl";
import {
  ALL_GROUP_KEY,
  DIMENSION_LABELS,
  dimensionValue,
  dimensionValueLabel,
  filterCounts,
  group,
  type InboxFilters,
  inFieldFilter,
  LATER_GROUP_KEY,
  sortRows,
  withBand,
} from "./grouping";
import { AskHandOnLiveContext } from "./hand-on-live";
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
import { PaneDivider, usePaneWidth } from "./PaneDivider";
import { InboxHandOffContext, type PlacedRow, usePaneSelection } from "./pane-selection";
import { useFoldedGroups } from "./preferences";
import { questionLine } from "./question-line";
import { ReadingPane } from "./ReadingPane";
import { RowKindIcon, RowPriorityTag, RowUrgencyMark } from "./RowMarks";
import { SnoozeControl } from "./SnoozeControl";
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
import { snoozeChoices, snoozeOf } from "./snooze";
import { ThreadRowCard } from "./ThreadRowCard";
import { ThreadTitle } from "./ThreadTitle";
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

/** One action of the selection bar, as the bar's own line draws it. */
const BAR_ACTION_CLASS = `min-h-11 shrink-0 rounded-lg px-2 py-1 text-xs font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`;

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
 * what shortens and neither the marks nor the key are ever cut.
 */
function ShortRowFrame({
  age,
  check,
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
      </div>
    </InboxRowShell>
  );
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
}: InboxRowProps & ShortRowProps & { ask: InboxRow }): ReactNode {
  return (
    <ShortRowFrame
      age={formatAskAge(rowLastActivityAt(row))}
      check={
        <MarkCheckbox
          id={ask.id}
          marked={marked}
          name={controlName(ask, ordinal)}
          onMark={onMark}
        />
      }
      group={groupKey}
      onRelease={onRelease}
      onSelect={onSelect}
      owner={<InboxOwnerLink ask={ask} rowOwner />}
      row={row}
      section={section}
      selected={selected}
      title={<span className="min-w-0 grow truncate text-sm">{questionLine(ask.question)}</span>}
    />
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
  return (
    <ShortRowFrame
      age={<Timestamp at={rowLastActivityAt(row)} />}
      check={
        <MarkCheckbox
          id={rowId(row)}
          marked={marked}
          name={questionLine(thread.subject)}
          onMark={onMark}
        />
      }
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
    />
  );
}

/** The row without a reading column beside it: the question's whole card, under the controls
 *  that defer or reroute it. */
function InboxCardRow({
  ask,
  assignLive,
  group: groupKey,
  marked,
  onAnswered,
  onAssignLive,
  onMark,
  onRelease,
  onSnoozeLive,
  ordinal,
  row,
  section,
  threadUpdatedAt,
  viewer,
}: InboxRowProps & {
  ask: InboxRow;
  /** True while this row's "Assign to me" write is in flight or failed, whatever band the
   *  optimistic update put the row in. */
  assignLive: boolean;
  onAnswered: (id: string) => void;
  onAssignLive: (askId: string, live: boolean) => void;
  /** The same registration for this row's snooze write; the Inbox keeps a live row rendered
   *  even when its band is folded, so `Snoozing…` and a refusal survive the fold. */
  onSnoozeLive: (askId: string, live: boolean) => void;
  /** Timestamp of the Inbox snapshot that supplied this row's initial thread. */
  threadUpdatedAt: number;
  /** The signed-in person's lowercase email; "Assign to me" writes it. */
  viewer: string;
}): ReactNode {
  const name = controlName(ask, ordinal);
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
          {ask.issue_key !== null && (isUnassigned(row) || assignLive) ? (
            <AssignToMe
              askId={ask.id}
              issueKey={ask.issue_key}
              onLive={onAssignLive}
              viewer={viewer}
            />
          ) : null}
          <SnoozeControl
            askId={ask.id}
            label={name}
            onLive={onSnoozeLive}
            snoozedUntil={section === "later" ? ask.snoozed_until : null}
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
        <MarkCheckbox
          id={rowId(row)}
          marked={marked}
          name={questionLine(thread.subject)}
          onMark={onMark}
        />
      </div>
      <ThreadRowCard thread={thread} />
    </InboxRowShell>
  );
}

/** The rows of one band, with the held row - one the server no longer lists here but the
 *  reader is still on - kept after the nearest row above it that is still listed, so it does not
 *  move while they finish. */
function withHeld(
  rows: readonly InboxListRow[],
  held: InboxListRow | undefined,
  previous: readonly PlacedRow[]
): readonly InboxListRow[] {
  if (held === undefined) return rows;
  const heldId = rowId(held);
  const ids = rows.map((row) => rowId(row));
  for (
    let index = previous.findIndex((placed) => rowId(placed.row) === heldId) - 1;
    index >= 0;
    index -= 1
  ) {
    const above = previous[index];
    const at = above === undefined ? -1 : ids.indexOf(rowId(above.row));
    if (at !== -1) return [...rows.slice(0, at + 1), held, ...rows.slice(at + 1)];
  }
  return [held, ...rows];
}

/** The row to keep where the reader last saw it, if any. Dropped from the list: kept where last
 *  seen, drawn as `current`, or as last seen while `current` is undefined. Moved to another band:
 *  what the list says about it changed under the reader's hand, so the listed row is kept in the
 *  band they saw it in. A move into or out of Later is a snooze - the reader's own click, shown
 *  at once. */
function heldRow(
  seen: PlacedRow | undefined,
  listed: InboxListRow | undefined,
  current: InboxListRow | undefined,
  now: string | undefined
): PlacedRow | undefined {
  if (seen === undefined) return undefined;
  if (listed === undefined) return current === undefined ? seen : { ...seen, row: current };
  const movedBand =
    now !== undefined &&
    now !== seen.group &&
    now !== LATER_GROUP_KEY &&
    seen.group !== LATER_GROUP_KEY;
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

export function Inbox({
  filter: filterOverride,
  keymapScope = "inbox",
  onFilterChange,
  pane = true,
  scroller,
}: InboxProps = {}): ReactNode {
  const [addressRoute, goToRoute] = useInboxRoute();
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
  /** The list column, which scrolls in itself beside the reading column. */
  const listScroller = useRef<HTMLDivElement>(null);
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
  // Rows (by ask id) whose "Assign to me" write is in flight or failed: their control stays
  // mounted through the optimistic assignment and the rollback out of it (see `AssignToMe`).
  const [assigning, setAssigning] = useState<ReadonlySet<string>>(() => new Set());
  const onAssignLive = useCallback((askId: string, live: boolean) => {
    setAssigning((current) => {
      if (current.has(askId) === live) return current;
      const next = new Set(current);
      if (live) next.add(askId);
      else next.delete(askId);
      return next;
    });
  }, []);
  // The same registration for snooze writes: a row whose write is live stays rendered even
  // when Later is folded, so `Snoozing…` and a refusal are not unmounted by the fold the
  // optimistic update triggers (see `SnoozeControl`).
  const [snoozing, setSnoozing] = useState<ReadonlySet<string>>(() => new Set());
  const onSnoozeLive = useCallback((askId: string, live: boolean) => {
    setSnoozing((current) => {
      if (current.has(askId) === live) return current;
      const next = new Set(current);
      if (live) next.add(askId);
      else next.delete(askId);
      return next;
    });
  }, []);
  /** The rows the reader can see, in the order they see them, as this list last rendered them:
   *  what the motion hold is read against, what the selection steps through, and which asks a
   *  hand-on may hold. Written on every render below, and read here as the record of the
   *  render a registration arrives after. */
  const presented = useRef<readonly PlacedRow[]>([]);
  /** The row the list held at its last render, by id, and the typed words it was held under
   *  (`searchWords`, joined): it stays held while the reader's words are those, whatever the
   *  server does to the text they search. Written on every render below. */
  const heldUnder = useRef<{ readonly id: string; readonly words: string } | undefined>(undefined);
  // And the same for a hand-on, whose control sits in the card rather than beside it
  // (`AskHandOn`): the optimistic `waiting_on_person` takes the row out of this list as the
  // write starts, so without this the card holding the control would be unmounted before the
  // server answered and a refusal would come back to a fresh card that never failed. The cards
  // register through the context this list provides below (`hand-on-live.ts`), since one of
  // them is the document page's own and takes no prop of this list's.
  //
  // Only an ask this list was drawing when the write began is held. That context reaches every
  // card under the Inbox, and many of them are not a listed row's own: the card of a Reply
  // row's thread (`ThreadBody`), whose ask is often one the reader's filters hide, and every
  // ask block of a document open in the column, not only the selected one. Holding one of
  // those would pull a question the filters hide into the list, and a refusal would leave it
  // there, since the write stays failed until it is tried again and no card unmounts to
  // release it. So a registration is held to `presented`, the rows this list last drew: its
  // own cards are those rows, and the column shows one of them (`pane-selection.ts`,
  // `showing`) - all but a question whose row has already gone, which it keeps for an unsent
  // answer and which is no row to hold either. Letting go is never refused, so a hold always
  // ends with the write that took it.
  const [handingOn, setHandingOn] = useState<ReadonlySet<string>>(() => new Set());
  const onHandOnLive = useCallback((askId: string, live: boolean) => {
    if (
      live &&
      !presented.current.some((placed) => placed.row.kind === "ask" && placed.row.ask.id === askId)
    ) {
      return;
    }
    setHandingOn((current) => {
      if (current.has(askId) === live) return current;
      const next = new Set(current);
      if (live) next.add(askId);
      else next.delete(askId);
      return next;
    });
  }, []);
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
  // The filters and the typed text both decide what is listed, with two exceptions that
  // outrank them all: a row whose "Assign to me" write is live stays, and so does one whose
  // hand-on is. Taking a question under the Unassigned filter is exactly the first case - the
  // row stops matching the moment the optimistic assignment lands - and handing a question on
  // in the reader's own list is the second, since the optimistic `waiting_on_person` is read
  // by this very filter. Unmounting either control would take `Assigning…` or `Handing on…`, a
  // refusal and its Retry off the screen with it.
  const narrowed = candidates.filter(
    (row) =>
      (row.kind === "ask" && (assigning.has(row.ask.id) || handingOn.has(row.ask.id))) ||
      (inAssigneeFilter(row, selection, viewer ?? "") &&
        matchesInboxSearch(row, words) &&
        INBOX_FILTER_FIELDS.every((field) =>
          inFieldFilter(row, field, filters[field], viewer ?? "")
        ))
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
  // after it (R8) once the server has recorded it, not at the press: the column holds the
  // question here until the server decides their answer, so a refusal shows on its own card. A
  // reader who moved on meanwhile - an approval reviewed from the head while they walked off -
  // is not pulled back (`landHandOff`).
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
  /** A row's own Dismiss (`DismissControl`, through `RowDismissContext`). Beside the column only
   *  the selected row is the item in hand, so the Dismiss the pointer reaches on another row
   *  takes that row out and leaves the selection, the address and the focus on the item being
   *  read; the selected row's Dismiss, the column head's, and every card's below the column's
   *  width hand the reader on to the item after it (R8). */
  const dismissFromRow = (named: InboxDismissRow) => {
    if (paneShown && named.id !== paneSelection.id) dismissRows([named]);
    else dismissNow([named]);
  };
  // Every key the Inbox binds, and the record of the row `l` was pressed on, which the column's
  // wrapper below holds and drops (`useInboxKeymap.ts`).
  const { forgetPaneReturn, holdPane } = useInboxKeymap({
    barOpen,
    bulkBarRef,
    bulkBarShown,
    clearSelection,
    dismiss: dismissNow,
    listRef,
    onMark,
    paneSelection,
    paneShown,
    rowsById: byId,
    scope: keymapScope,
    searchRef,
    selectedAsks,
  });

  if (inbox.isPending || whoAmI.isPending) {
    return <LoadingSkeleton label="Loading your inbox" />;
  }
  if (inbox.isError || viewer === undefined || place === undefined) {
    return <p className={dangerText}>Could not load your inbox.</p>;
  }

  // Which row the list holds in place while the server moves the list under it: the row the
  // reader's hand is on (focus or pointer), with or without the reading column. A row the
  // column is merely showing is not held - the Inbox selects the first row for the reader the
  // moment it opens, and a question the server withdraws, resolves or answers elsewhere must
  // not stay in the list on the strength of a selection nobody made. What the column does with
  // a selected question the server takes away is `paneRow` (`pane-selection.ts`). A row the
  // reader dismissed under their own hand is not held either: they asked for it to go, as they
  // do by answering one, so it goes at once and the next row takes its place.
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
  const listed = seen === undefined ? undefined : byId.get(rowId(seen.row));
  const nowBand =
    listed === undefined
      ? undefined
      : bands.find(({ rows: banded }) => banded.includes(listed))?.key;
  // A held row dropped from the list is drawn as its candidate, the row as the Inbox holds it
  // now, so a pick in it shows there.
  const held = heldRow(
    seen,
    listed,
    seen === undefined || listed !== undefined
      ? undefined
      : candidates.find((row) => rowId(row) === handId),
    nowBand
  );
  heldUnder.current = held === undefined ? undefined : { id: rowId(held.row), words: typed };

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
  // The clear-filter link keeps its own text and name; the agent's live link sits beside it.
  // `whitespace-pre` keeps the space before an attribution suffix, which a flex item would drop.
  const agentChip =
    agentSubject === undefined ? undefined : (
      <AgentName
        {...agentSubject}
        className="flex items-center whitespace-pre"
        control={(name) => (
          <Link
            aria-label="Clear agent filter"
            className="inline-flex min-h-11 items-center rounded-full"
            to={buildInboxPath({ assignees: filter.assignees, item: filter.item, q: filter.q })}
          >
            <LabelPill selected>
              Asks from {name}
              {filter.section === "needs-you" ? " waiting on you" : ""} · clear
            </LabelPill>
          </Link>
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
    // What each filter control counts against: a row that survives every narrowing but that
    // control's own, so a reader opening Priority is told what each priority holds inside the
    // list they have already narrowed, and ticking a value never empties the list by surprise.
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
          return (
            <MultiSelect
              {...filterPanel(field)}
              emptyMessage={`No matching ${DIMENSION_LABELS[field].toLowerCase()}.`}
              key={field}
              label={DIMENSION_LABELS[field]}
              onChange={(next) => chooseField(field, next)}
              optionDetail={(value) => `${counts.get(value) ?? 0}`}
              optionDetailLabel={(value) => `${counts.get(value) ?? 0} rows`}
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
            what the filter saying so is for. */}
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
          optionDetail={(value) =>
            `${value === viewer ? "you · " : ""}${assigneeCounts.get(value) ?? 0}`
          }
          optionDetailLabel={(value) =>
            `${value === viewer ? "you, " : ""}${assigneeCounts.get(value) ?? 0} rows`
          }
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
  // the list rather than in it, so marking a row moves no row (INFRA-3135, R4). It is drawn
  // always, and given no count and no state while it is not shown, so its going is the bar's
  // own to see and the focus it held is handed on (`SelectionBar`).
  const dismissLabel = showDismissed ? "Move to Inbox" : "Dismiss";
  const selectedCount = selectedAsks.length + selectedThreads.length;
  // In flight the count is the press's, since the rows it moved have already left this view;
  // settled, it is what a second press would act on - the rows still marked. Said here rather
  // than left to the bar's own count so the bar still stands over an emptied selection, which is
  // where a refusal of that press lands.
  let barState: string | undefined;
  if (!bulkBarShown) barState = undefined;
  else if (bulkSnoozing !== 0) barState = `Snoozing ${bulkSnoozing}…`;
  else if (allRows) barState = "Every row selected";
  else barState = `${selectedCount} selected`;
  const selectionBar = (
    <SelectionBar
      barRef={bulkBarRef}
      count={bulkBarShown ? selectedCount : 0}
      label="Selected rows"
      notice={snoozeRefusal}
      // The bar gone with the focus, or the control in it that held the focus: the row a press
      // of the bar hands the reader on to (R8), else the checkbox of the row the reader marked
      // last, else of the first row in list order, else the search box. A row a Dismiss or a
      // Move to Inbox press is taking off this view is passed over: it is still drawn until the
      // write's optimistic apply lands, and a focus handed to it would drop with it.
      // `dismissedHere` names every such row - each id the dismiss write's `pending` holds, and
      // the ids of a press made under the narrowing, which `pending` leaves out. A Move to Inbox
      // that empties the Dismissed view leaves no other row, so the focus goes to the search box
      // until the view's empty message is drawn.
      returnFocus={() =>
        paneSelection.handOffTarget() ??
        [rowIn(listRef.current, lastMarked()), ...rowsIn(listRef.current)]
          .find((row) => row !== null && !dismissedHere(row.dataset.inboxRow ?? ""))
          ?.querySelector<HTMLElement>(MARK_SELECTOR) ??
        searchRef.current
      }
      state={barState}
    >
      {/* The menu stays while its own write is out: the optimistic fold takes every marked ask
          off this view, and a trigger unmounted under the reader's hand drops their focus. One
          pick at a time: a second while the first is in the air re-sends every id, and its
          answer can settle after the live one's and overwrite what the reader was told. It is
          held (`aria-disabled`) rather than disabled because the browser takes focus off a
          control that disables itself under it - dropping the reader on the document
          mid-write, with no Escape to leave and no way back to the bar. It snoozes only, so it
          offers no Un-snooze. */}
      {selectedAsks.length === 0 && bulkSnoozing === 0 ? null : (
        <ChoiceMenu
          choices={snoozeChoices(false)}
          face="Snooze"
          held={bulkSnoozing !== 0}
          label="Snooze selected asks"
          marks={{ "data-inbox-bulk-snooze": "" }}
          onChoose={(choice) => {
            const until = snoozeOf(choice, new Date());
            if (until === null) return;
            // The reader is handed on to the item after the last of the asks once the server
            // has taken them (R8); a pick it refused whole sends nobody anywhere.
            const ids = [...selectedAsks];
            paneSelection.beginHandOff(ids);
            void snoozePick(until).then((taken) =>
              taken.length === 0 ? paneSelection.dropHandOff(ids) : paneSelection.landHandOff(taken)
            );
          }}
          triggerClassName={badgeMenuTrigger}
        >
          <span className={`${badgeSelectBadge} ${badgeLow.bg} ${badgeLow.text}`}>Snooze</span>
        </ChoiceMenu>
      )}
      {selectedThreads.length === 0 ? null : (
        <button
          aria-label={`${dismissLabel} selected threads`}
          className={BAR_ACTION_CLASS}
          data-inbox-bulk-dismiss=""
          onClick={() => dismissNow(selectedThreads, dismissPick)}
          type="button"
        >
          {dismissLabel}
        </button>
      )}
      {allRows ? null : (
        <button
          className={BAR_ACTION_CLASS}
          data-inbox-select-all=""
          onClick={selectAll}
          type="button"
        >
          Select all
        </button>
      )}
      <button className={BAR_ACTION_CLASS} onClick={clearSelection} type="button">
        Clear selection
      </button>
    </SelectionBar>
  );

  // Everything above the list, the same whether or not any band has a row, and a sibling of the
  // scrolling list rather than an item in it: the controls stay put while the rows move.
  //
  // One row holds the page's search, sorting and filtering, and nothing else between the title
  // and the first row draws a search box, a panel trigger or a chip (INFRA-3135, R1). Nothing
  // the reader can press here makes the header taller: the filter panel and the display menu
  // are portalled over the page (`AnchoredPanel`), the selection bar is drawn over the list,
  // and the chips are the one part of the row that may take a second line.
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
    // Every card this list draws - in the column, and in a row below its width - holds its own
    // hand-on here while the server decides it (`hand-on-live.ts`), the decision block the
    // column draws through the document page included, which no prop of this list reaches.
    <AskHandOnLiveContext value={onHandOnLive}>
      {/* Every thread this list draws - in the column, and in a card below its width - draws its
          own Dismiss from here (`DismissControl`), which hands the reader on only from the item
          in hand (`dismissFromRow`). */}
      <RowDismissContext
        value={{
          dismiss: dismissFromRow,
          pending: dismissWrite.pending,
          show,
        }}
      >
        {/* Every Snooze this list draws - on a row, and in the column's head - hands the reader on
            to the item after the one it snoozed once the server has taken it (R8). */}
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
                        <ReadingPane
                          onAnswered={recordAnswered}
                          onSnoozeLive={onSnoozeLive}
                          row={subject}
                          viewer={viewer}
                        />
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
      </RowDismissContext>
    </AskHandOnLiveContext>
  );

  if (shown.length === 0 && held === undefined) {
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
    const empty =
      emptyMessage === undefined ? (
        <div data-inbox-empty="" data-testid="inbox-list" tabIndex={-1} />
      ) : (
        <div data-testid="inbox-list">
          <EmptyState
            label="Inbox empty state"
            marks={{ "data-inbox-empty": "" }}
            message={emptyMessage}
            tabIndex={-1}
          />
        </div>
      );
    return page(empty, paneSelection.showing([]));
  }

  // The held row is drawn in the band the reader saw it in, after the row it then followed.
  // A band the held row has left is empty, and an empty band has nothing to head - but the band a
  // row still listed is held in is drawn even where the change that moved it to another band took
  // its last other row away, or the row would be drawn nowhere for that commit (`withBand`).
  const drawn = (
    held === undefined || listed === undefined
      ? bands
      : withBand(bands, held.group, groupBy, titles)
  )
    .map((band) => {
      const banded = withHeld(
        band.rows.filter((row) => rowId(row) !== (held === undefined ? "" : rowId(held.row))),
        held?.group === band.key ? held.row : undefined,
        presented.current
      );
      return { band: { ...band, rows: banded }, rows: drawnIn(band.key, banded) };
    })
    .filter(({ band }) => band.rows.length > 0);
  /** Which turn band a row is drawn in: the held row's is the one the reader saw it in. */
  const sectionDrawn = (row: InboxListRow): InboxSection =>
    held !== undefined && rowId(held.row) === rowId(row)
      ? held.section
      : (sectionOf(row, place) ?? "human");
  presented.current = drawn.flatMap(({ band, rows: visible }) =>
    visible.map((row) => ({ group: band.key, row, section: sectionDrawn(row) }))
  );
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
    const onRelease = held !== undefined && rowId(held.row) === id ? release : undefined;
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
          onSelect={paneSelection.select}
          selected={id === paneSelection.id}
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
        assignLive={assigning.has(row.ask.id)}
        key={id}
        onAnswered={recordAnswered}
        onAssignLive={onAssignLive}
        onSnoozeLive={onSnoozeLive}
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
  const list = (
    // With the reading column beside it the list scrolls in itself rather than in the window,
    // so the two columns move independently; the anchor is given that same box, so a refetch
    // keeps the reader's row where it was in it. Without the pane the rows scroll in whatever
    // the host gives (the drawer's own `aside`), and in the window when it gives nothing.
    <div
      className={paneShown ? "min-w-0 flex-1 overflow-y-auto pr-1" : undefined}
      data-testid="inbox-list"
      ref={listScroller}
    >
      <ViewportAnchor
        className="space-y-4"
        group="data-inbox-section"
        item={ROW_ATTRIBUTE}
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
              onToggleFold: toggleFold,
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
    </div>
  );

  // Nothing to read is nothing to select, so an empty list is the whole Inbox above, as it is
  // below the reading column's width. The one question that outlives every row is the one the
  // column is keeping for an unsent answer, and `showing` hands that one back from an empty
  // list too, so it keeps its column there: the reader's text is in it.
  return page(list, selectedRow);
}
