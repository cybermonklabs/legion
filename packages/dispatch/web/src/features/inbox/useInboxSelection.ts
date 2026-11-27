/**
 * What the reader marked in the Inbox, and the two bulk writes the bar presses on it: the one
 * Snooze over the asks among the marks, and the one Dismiss over the threads.
 *
 * The state lives here rather than in the bar because either write can take every marked row
 * off the view at once: a bar that owned the write would be unmounted before its own refusal
 * arrived. The bar (`SelectionBar`) reports one press downward and renders what it is told.
 */

import { type RefObject, useCallback, useRef, useState } from "react";
import { flushSync } from "react-dom";

import { useMarkedKeys } from "../../hooks/useMarkedKeys";
import { stoppedWriteNote } from "../writes/keyed-write";
import type { InboxFilters } from "./grouping";
import { type InboxListRow, rowId } from "./list-row";
import { rangeOf } from "./selection-range";
import { type AskSnoozeFailure, useAskSnoozeMany } from "./useAskSnooze";
import {
  dismissRow,
  type InboxDismiss,
  type InboxDismissRow,
  useInboxDismiss,
} from "./useInboxDismiss";

/**
 * What the bar says after a pick the server refused in part. The count alone cannot tell a
 * sign-out from `snoozed_until must be in the future`, so the reason travels with it: one reason
 * when every refusal gave the same one, otherwise the first and how many others there are. A
 * snooze the reader stopped is no refusal - it may have been saved - so those are said apart
 * (`snoozeStoppedText`).
 */
function snoozeRefusalText(failed: readonly AskSnoozeFailure[], of: number): string | undefined {
  const refused = failed.filter(
    (failure): failure is Extract<AskSnoozeFailure, { stopped: false }> => !failure.stopped
  );
  const unconfirmed = snoozeStoppedText(failed.length - refused.length, of);
  const first = refused[0];
  if (first === undefined) return unconfirmed;
  const reasons = new Set(refused.map(({ reason }) => reason));
  // The reason is the server's own words for why it refused the snooze.
  const head = `Could not snooze ${refused.length} of ${of}: ${first.reason}`;
  const others = reasons.size - 1;
  const said =
    others === 0 ? `${head}.` : `${head}, and ${others} other reason${others === 1 ? "" : "s"}.`;
  return unconfirmed === undefined ? said : `${said} ${unconfirmed}`;
}

/** What the bar says of the `stopped` snoozes of a pick over `of` asks that the reader stopped,
 *  or undefined for none, in the words of a stopped write (`stoppedWriteNote`): each may have
 *  been saved and stays marked, so the next pick is a press of its own over it. */
function snoozeStoppedText(stopped: number, of: number): string | undefined {
  if (stopped === 0) return undefined;
  return stoppedWriteNote(`the snooze of ${stopped} of ${of}`, "saved", {
    anew:
      stopped === 1
        ? "It stays marked, and the next pick snoozes it as a new write."
        : "They stay marked, and the next pick snoozes them as a new write.",
  });
}

/** Where the next range runs from: the anchor, the row the reader last marked or unmarked
 *  without the shift key, by its checkbox or by `x`; the marks as they stood just after that
 *  mark; and the run the last range took from it. */
interface RangeStart {
  readonly anchor: string | null;
  readonly atAnchor: ReadonlySet<string>;
  readonly last: readonly string[];
}

/** No anchor, as Select all and Clear selection leave the list. */
const NO_ANCHOR: RangeStart = { anchor: null, atAnchor: new Set(), last: [] };

/** `marked` with the run `run` taken from an anchor whose marks were `atAnchor`: each row of the
 *  run marked, or unmarked where `marking` is false, and each row of the last run (`last`) that
 *  this run does not hold back as it stood at the anchor, which unmarks a row that arrived since.
 *  Every other row keeps its mark. */
function withRange(
  marked: ReadonlySet<string>,
  run: readonly string[],
  marking: boolean,
  { atAnchor, last }: RangeStart
): ReadonlySet<string> {
  const next = new Set(marked);
  const inRun = new Set(run);
  for (const id of last) {
    if (inRun.has(id)) continue;
    if (atAnchor.has(id)) next.add(id);
    else next.delete(id);
  }
  for (const id of run) {
    if (marking) next.add(id);
    else next.delete(id);
  }
  return next;
}

/** The selection of one Inbox list, and the two presses it feeds. */
export interface InboxSelection {
  /** Whether the reader pressed **Select all**, which takes in every row the narrowing matches,
   *  the ones a fold or a page is holding back included. The bar says so instead of counting. */
  readonly allRows: boolean;
  /** The bar's own element, which the keymap reaches through this: Escape from the bulk picker
   *  focuses the bar itself when no drawn row is left that the bar's own Snooze write, while it
   *  is pending (`snoozing`), is not taking away. */
  readonly barRef: RefObject<HTMLElement | null>;
  /** Whether the bar shows: a mark is on the page, a pick is in the air, or a refusal is
   *  standing. Escape with no row focused is the keyboard's Clear and is offered on exactly this,
   *  so the two cannot drift. */
  readonly barShown: boolean;
  readonly clearSelection: () => void;
  /** Whether a **Dismiss** the reader pressed here is taking this row off the page: from the
   *  press until the page no longer draws the row after Dispatch took it, or until Dispatch
   *  refused it. The list's hand hold skips such a row, so a row they dismissed leaves the list
   *  even with their hand, or the bar's parked focus, still on it. */
  readonly dismissedHere: (id: string) => boolean;
  /** One press over the threads the bar is counting. */
  readonly dismissPick: () => void;
  /** One press over the rows given, from a row's own control or from `e`: each row with what
   *  the page drew in it (`InboxDismissRow`). */
  readonly dismissRows: (rows: readonly InboxDismissRow[]) => void;
  /** The one dismiss write this list shares, for what it is doing and what it was refused. */
  readonly dismissWrite: InboxDismiss;
  /** Ends the name of each row Dispatch took whose row `drawn` no longer holds: the page has let
   *  it go, so nothing of the press is left to wait for (`dismissedHere`). */
  readonly forgetDismissed: (drawn: (id: string) => boolean) => void;
  /** The row the reader marked last, by `rowId`, or null before their first mark: the row whose
   *  checkbox takes the focus when the bar goes with it (`SelectionBar`'s `returnFocus`). A range
   *  never moves it. */
  readonly lastMarked: () => string | null;
  /** Every row the reader has marked, by `rowId`, a folded one's included. */
  readonly marked: ReadonlySet<string>;
  /** The range from the anchor to row `id`, every row of it taking the state `id` moves to: a
   *  shift-click on its checkbox, and `Shift+X`, `Shift+J` and `Shift+K` on it. Where the list
   *  does not draw the anchor, or there is none, `startsAnchor` decides: a shift-click and
   *  `Shift+X` mark or unmark that row alone, which becomes the anchor; `Shift+J` and `Shift+K`,
   *  which set no anchor, change nothing. */
  readonly markRange: (id: string, startsAnchor: boolean) => void;
  /** A mark or an unmark of row `id` without the shift key, by its checkbox or by `x`, which
   *  makes it the anchor of the next range. */
  readonly onMark: (id: string) => void;
  readonly selectAll: () => void;
  /** The marked asks the view is drawing, which is what **Snooze** defers. */
  readonly selectedAsks: readonly string[];
  /** The thread rows a **Dismiss** press names, each with what the page drew in it, which is
   *  what the bar counts for that press. */
  readonly selectedThreads: readonly InboxDismissRow[];
  /** Snoozes the marked asks the view is drawing until `until`; settles once the server has
   *  answered for every one of them. */
  readonly snoozePick: (until: string) => Promise<void>;
  /** What the reader is told about the last bulk snooze the server refused in part. */
  readonly snoozeRefusal: string | undefined;
  /** Whether Dispatch refused this ask's snooze in the bar's newest pick: the rollback has put
   *  the row's moment back, so the row is staying where it is. */
  readonly snoozeRefused: (id: string) => boolean;
  /** The asks the snooze now in flight covers, none while none is: the list keeps each in the
   *  band the reader saw it in until the write lands - save under the Blocked on you narrowing,
   *  which lists no snoozed ask and keeps a row only while the reader's hand is on it - and the
   *  bar counts them while it is out, since the optimistic moment takes them out of the
   *  selection it would otherwise count. */
  readonly snoozing: ReadonlySet<string>;
}

/**
 * One list's selection: the marks, **Select all**, and the Snooze and Dismiss the bar presses.
 *
 * **Select all** and the marks differ in what a press covers. A mark the reader cannot see is
 * not acted on - a row a fold or a page is holding back is not on the page, and they may well
 * have set its own moment since marking it - so the marks a press writes are the ones this view
 * is drawing. **Select all** is the other promise: every row the narrowing matches. The Inbox's
 * own thread read is unpaged, so one Dismiss under it names every matching thread id, the rows
 * behind **Show more** and inside a folded band with them (A12); only the paged Dismissed view
 * has ids it has not loaded, and there the press names the list's own narrowing instead (`asOf`).
 * Snooze is unchanged by **Select all**: it defers the marked asks the view is drawing, never a
 * moment the reader set on a row they cannot see.
 */
export function useInboxSelection({
  asOf,
  candidates,
  drawnIds,
  filters,
  order,
  rows,
  showDismissed,
}: {
  /** The read mark a press that names the list's own narrowing carries, or undefined where no
   *  press may name it: the server's narrowing holds the five filter fields and knows nothing of
   *  the text typed in the search box. */
  readonly asOf: string | undefined;
  /** Every row the two reads answered, before the filters and the typed text narrow them: a mark
   *  whose row the list no longer holds at all drops against these. */
  readonly candidates: readonly InboxListRow[];
  /** The rows this view is drawing, by `rowId`: a folded band's rows and the ones behind **Show
   *  more** are not among them. */
  readonly drawnIds: ReadonlySet<string>;
  /** The list's own five fields, which a press under the narrowing sends. */
  readonly filters: InboxFilters;
  /** The rows a range runs over, by `rowId`, in the order the list draws them at the moment it
   *  is asked: what a shift-click and the range keys read. */
  readonly order: () => readonly string[];
  /** Every row the narrowing matches, by `rowId`, in the order the list holds them. */
  readonly rows: ReadonlyMap<string, InboxListRow>;
  /** Whether the list is showing what the reader dismissed, where a press puts rows back. */
  readonly showDismissed: boolean;
}): InboxSelection {
  // A mark whose row the list no longer holds at all - dismissed, answered, resolved elsewhere -
  // drops out, so a row that comes back is unmarked. Read against every row the two reads
  // answered, which is what the marks are keyed by.
  const candidateIds = candidates.map((row) => rowId(row)).join(" ");
  // The rows the reader has marked, by `rowId` - asks and threads alike: `x` and the row's own
  // checkbox are the two ways in, and the pruning above is the shared hook's own
  // (`hooks/useMarkedKeys.ts`; a project's issue table marks its rows through TanStack's own `rowSelection` instead). A mark
  // whose row this view does not list, or whose band is folded shut, drops out of `selected`
  // below, so the bar never counts - and `h` never writes - a row that is not on the page. The
  // mark itself outlives both: open the band, or switch back, and it is still made.
  const { marked, onMark: toggleMark, setMarked } = useMarkedKeys(candidateIds);
  // Whether the reader pressed **Select all**. Any change to the marks ends it.
  const [allRows, setAllRows] = useState(false);
  const bulkWrite = useAskSnoozeMany();
  const [snoozing, setSnoozing] = useState<ReadonlySet<string>>(() => new Set());
  const barRef = useRef<HTMLElement>(null);
  const [snoozeRefusal, setSnoozeRefusal] = useState<string | undefined>(undefined);
  /** The asks Dispatch refused in the bar's newest pick (`snoozeRefused`). */
  const refusedPick = useRef<ReadonlySet<string>>(new Set());
  /** The rows the reader has pressed Dismiss on here, by `rowId`, from the press until the page
   *  no longer draws them: the list's hand-hold skips them, so a row they dismissed leaves the
   *  list even with their hand still on it. An id a refusal settles leaves at once, since the
   *  rollback puts an ordinary row back; an id Dispatch took stays until the page lets its row go
   *  (`forgetDismissed`), since a read that has not dropped it yet still draws it, and a hold that
   *  read a settled press as over would keep that row for a hand in it. */
  const pressedHere = useRef<Set<string>>(new Set());
  /** The ids of `pressedHere` that Dispatch took, each waiting for the page to let its row go. */
  const takenHere = useRef<Set<string>>(new Set());
  // The one dismiss write every control on this list shares, the row's own and the bar's: the
  // write takes the row - and every control drawn on it - off the page, so its refusal is the
  // page's to show rather than the row's (`useInboxDismiss`).
  const dismissWrite = useInboxDismiss({
    onSettled: (ids, refused) => {
      for (const id of ids) {
        if (refused) pressedHere.current.delete(id);
        else takenHere.current.add(id);
      }
    },
  });
  // Where the next range runs from. Select all and Clear selection leave no anchor, so the
  // next shift-click marks or unmarks its own row alone and becomes the anchor.
  const range = useRef<RangeStart>(NO_ANCHOR);
  const clearSelection = useCallback(() => {
    range.current = NO_ANCHOR;
    setMarked(new Set());
    setAllRows(false);
    setSnoozeRefusal(undefined);
  }, [setMarked]);
  // The marks as this render drew them, so a mark tells a row being marked from one being
  // unmarked while `onMark` stays as stable as `useMarkedKeys` makes it.
  const markedNow = useRef(marked);
  markedNow.current = marked;
  // The order as this render reads it, so `markRange` stays stable too.
  const orderNow = useRef(order);
  orderNow.current = order;
  const lastMarkedId = useRef<string | null>(null);
  // A mark or an unmark of one row, which becomes the anchor, with the marks as they stand
  // after it. Any change to the marks ends Select all.
  const markAlone = useCallback(
    (id: string) => {
      const atAnchor = new Set(markedNow.current);
      if (atAnchor.has(id)) atAnchor.delete(id);
      else atAnchor.add(id);
      range.current = { anchor: id, atAnchor, last: [] };
      setAllRows(false);
      toggleMark(id);
    },
    [toggleMark]
  );
  const onMark = useCallback(
    (id: string) => {
      if (!markedNow.current.has(id)) lastMarkedId.current = id;
      markAlone(id);
    },
    [markAlone]
  );
  // The state the range takes is read from the marks at the anchor, not from the marks now, so a
  // second shift-click from one anchor reads as a first one does. It writes the marks directly
  // and never through `onMark`, so the row marked last stays the one the reader marked.
  const markRange = useCallback(
    (id: string, startsAnchor: boolean) => {
      const start = range.current;
      const drawn = orderNow.current();
      if (start.anchor === null || !drawn.includes(start.anchor)) {
        if (startsAnchor) markAlone(id);
        return;
      }
      const run = rangeOf(drawn, start.anchor, id);
      const marking = !start.atAnchor.has(id);
      range.current = { ...start, last: run };
      setAllRows(false);
      setMarked((current) => withRange(current, run, marking, start));
    },
    [markAlone, setMarked]
  );
  /** The marks the bar counts: the rows this view is drawing. */
  const selected = [...marked].filter((id) => drawnIds.has(id));
  const selectedAsks = selected.filter((id) => rows.get(id)?.kind === "ask");
  // Under **Select all** the press covers every thread row the narrowing matches, drawn or not:
  // the Inbox's read is unpaged, so these are all of them, and one request names them all, each
  // with the values its own row is drawn from (`dismissRow`).
  const selectedThreads: InboxDismissRow[] = [];
  for (const id of allRows ? [...rows.keys()] : selected) {
    const row = rows.get(id);
    const named = row === undefined ? undefined : dismissRow(row);
    if (named !== undefined) selectedThreads.push(named);
  }
  // One pick, over the ids the bar is counting: the same optimistic moment and rollback the
  // per-row control makes, once per ask, with the rows kept where the reader saw them while the
  // write is out (`snoozing`), save under the Blocked on you narrowing, which lists no snoozed
  // ask: there each row leaves at the pick, since a row a press names as leaving is never kept for
  // the reader's hand in it (`Inbox.tsx`'s `leavingNow`). The ids the server took leave the
  // selection, and their rows the band they were kept in, in one commit; the ones it refused are
  // marked again under its own reason, even if Clear ran while the write was in flight, so a
  // second pick retries exactly those.
  const snoozePick = (until: string): Promise<void> => {
    const ids = [...selectedAsks];
    setSnoozeRefusal(undefined);
    refusedPick.current = new Set();
    setSnoozing(new Set(ids));
    return bulkWrite.submit(ids, until).then(({ failed }) => {
      const refused = new Set(failed.map(({ askId }) => askId));
      refusedPick.current = refused;
      // The bar's going and its hand-off are one render. This runs outside any event of the
      // reader's, so React would commit it at normal priority and run the bar's hand-off, a
      // passive effect (`SelectionBar`), in a later task; in Firefox, which fires no `focusout`
      // for the removed Snooze trigger, the reader's next key could reach the page before the
      // bar's hand-off has placed the focus. `flushSync` commits the going here and runs the bar's
      // effects with it, as the project List's bar does (`issues-table-actions.ts`).
      flushSync(() => {
        setSnoozing(new Set());
        setSnoozeRefusal(snoozeRefusalText(failed, ids.length));
        const taken = new Set(ids.filter((id) => !refused.has(id)));
        setMarked((current) => {
          const next = new Set([...current].filter((id) => !taken.has(id)));
          for (const id of refused) next.add(id);
          return next;
        });
      });
    });
  };
  /** One press of the reader's, from a row's own control, from `e`, or from the bar: the ids go
   *  into the hold's exception list first, so the rows leave the list under their hand. */
  const dismissRows = (named: readonly InboxDismissRow[]) => {
    for (const { id } of named) pressedHere.current.add(id);
    dismissWrite.submit({ restore: showDismissed, rows: [...named] });
  };
  // One press over the threads the bar is counting, through the one dismiss write. The ids are
  // in hand wherever the list that holds them is unpaged, which the Inbox's own read is, so the
  // press names them - a **Select all** of a band wider than a page included. The Dismissed view
  // is the one list the server pages, and there a **Select all** has ids it never loaded: that
  // press names the list's own narrowing and the mark the list was read at instead, as **Mark
  // all read** does, which is offered only where nothing is typed in the search box (`asOf`).
  const dismissPick = () => {
    if (allRows && showDismissed && asOf !== undefined) {
      for (const { id } of selectedThreads) pressedHere.current.add(id);
      dismissWrite.submit({ filters, restore: true, through: asOf });
    } else {
      dismissRows(selectedThreads);
    }
    // The rows leave the view at once, so the marks on them go with them; a refusal puts them
    // back under the line the page shows for it, not under a bar the reader must look for.
    clearSelection();
  };

  return {
    allRows,
    barRef,
    // The bar outlives an empty list: a pick that empties this view leaves its write in the air,
    // and the refusal that comes back has to land somewhere the reader can see.
    barShown: selected.length > 0 || snoozing.size !== 0 || snoozeRefusal !== undefined,
    clearSelection,
    dismissedHere: (id) => pressedHere.current.has(id),
    dismissPick,
    dismissRows,
    dismissWrite,
    forgetDismissed: (drawn) => {
      for (const id of takenHere.current) {
        if (drawn(id)) continue;
        takenHere.current.delete(id);
        pressedHere.current.delete(id);
      }
    },
    lastMarked: () => lastMarkedId.current,
    markRange,
    marked,
    onMark,
    selectAll: () => {
      range.current = NO_ANCHOR;
      setMarked(new Set(rows.keys()));
      setAllRows(true);
    },
    selectedAsks,
    selectedThreads,
    snoozePick,
    snoozeRefusal,
    snoozeRefused: (id) => refusedPick.current.has(id),
    snoozing,
  };
}
