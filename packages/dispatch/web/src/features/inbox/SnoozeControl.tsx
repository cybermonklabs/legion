import { createContext, type ReactNode, useContext, useState } from "react";
import { heldPress, useHeldThroughAnswer } from "../../components/held-press";
import {
  type ListMarks,
  type ListMenu,
  type MenuDigit,
  MultiSelect,
} from "../../components/MultiSelect";
import { QueryError } from "../../components/QueryError";
import { badgeLow, secondaryButtonCompact, textMutedOnCanvas } from "../../theme/classes";
import { badgeSelectBadge, badgeSelectTrigger, badgeSelectWrapper } from "../issue/badge-select";
import { Timestamp } from "../refs/Timestamp";
import { ControlKeyHint, keyshortcutsOf } from "../shell/ShortcutHelp";
import { inboxActionMark } from "./inbox-action-mark";
import { InboxHandOffContext, RowAsideContext } from "./pane-selection";
import { isIndefinite, snoozeChoices, snoozeOf } from "./snooze";
import { type AskSnoozePress, type SnoozeSettled, useAskSnooze } from "./useAskSnooze";

/** The selection bar's own Snooze write as every Snooze of an Inbox reads it
 *  (`BulkSnoozingContext`). */
export interface BulkSnoozing {
  /** The asks the write is over while it is out (`useInboxSelection`'s `snoozing`), which the bar
   *  empties once the write settles - refused or stopped included. */
  readonly asks: ReadonlySet<string>;
  /** Whether the write is past its deadline (`useInboxSelection`'s `snoozeLate`): still out over
   *  `asks`, and still tried under its keys, but the controls it holds are let go. */
  readonly late: boolean;
}

/** No ask: a control drawn outside an Inbox has no selection bar over it. */
const NO_BULK_SNOOZING: BulkSnoozing = { asks: new Set(), late: false };

/**
 * The selection bar's own Snooze write (`BulkSnoozing`). The Inbox provides it over every control
 * it draws, a row's and the reading column's head alike. It is a context, as
 * `AskHandOnWritesContext` is, so that neither the row nor the column's bodies and head pass down
 * one more prop of the list's. With no provider no ask is under the bar.
 */
export const BulkSnoozingContext = createContext<BulkSnoozing>(NO_BULK_SNOOZING);

/** Every choice the list can offer - the presets, and Un-snooze on a snoozed item - which the
 *  list's digits and words are read from. */
const ALL_CHOICES = snoozeChoices(true);

/** The list's options, in the order it offers them: on an item not snoozed, and on one that is. */
const OPTIONS = {
  plain: snoozeChoices(false).map(({ value }) => value),
  snoozed: ALL_CHOICES.map(({ value }) => value),
} as const;

/** The value each digit writes: the presets' `1` to `4`, and Un-snooze's `0`. */
function digitKeys(): Partial<Record<MenuDigit, string>> {
  const keys: Partial<Record<MenuDigit, string>> = {};
  for (const { digit, value } of ALL_CHOICES) keys[digit] = value;
  return keys;
}

/** D5's rules (`ListMenu`): the list opens on its first choice, the presets are its `1` to `4`
 *  and Un-snooze its `0`, a digit the list does not offer - Un-snooze on an item not snoozed -
 *  writing nothing. Every close gives the focus back where it was as the list opened: the row or
 *  the checkbox a key pressed the trigger from, or the trigger a press opened it from. */
const SNOOZE_MENU: ListMenu = { keys: digitKeys(), opensOn: "first" };

const ROW_SNOOZE_ATTRIBUTE = "data-inbox-snooze";
const ROW_SNOOZE_MARKS: ListMarks = { trigger: { [ROW_SNOOZE_ATTRIBUTE]: "" } };
/** The trigger of each Snooze list this control draws, which the Inbox's `b` and `h` find by it
 *  and press - the row's own, or, with the reading column on, the column head's: the shared list
 *  hands out no element of its own. */
export const ROW_SNOOZE_SELECTOR = `[${ROW_SNOOZE_ATTRIBUTE}]`;

const BULK_PICKER_ATTRIBUTE = "data-inbox-bulk-snooze";
/** The marks of the selection bar's Snooze list's trigger (`Inbox.tsx`), which the Inbox's keys
 *  find it by (`BULK_PICKER_SELECTOR`). */
export const BULK_PICKER_MARKS: ListMarks = { trigger: { [BULK_PICKER_ATTRIBUTE]: "" } };
/** The bar's Snooze list's trigger: `b` and `h` press it while rows are marked, and Escape from it
 *  takes the focus back to the list. */
export const BULK_PICKER_SELECTOR = `[${BULK_PICKER_ATTRIBUTE}]`;

/**
 * The Snooze badge and the list it opens: the badge is the trigger of the shared list's single
 * pick (`MultiSelect`) of `snooze.ts`'s choices, and the list's text box narrows them as the
 * reader types. A row's own Snooze, the reading column head's and the short row's (each a
 * `SnoozeControl`), and the selection bar's (`Inbox.tsx`) are this control, and the Inbox's `b`
 * and `h` press its trigger, which the host finds by `marks`.
 * It takes D5's rules (`SNOOZE_MENU`): it opens on Later today, each preset is picked by its
 * digit as well as by Enter or a click, and every close hands the focus back where it was as the
 * list opened - so a snooze whose write takes the item away takes the focus from the row the key
 * was pressed on, which the Inbox's hand-off reads (R8).
 * The badge shows `badge` for the eye alone (`aria-hidden`), so the trigger's name is `label`
 * alone, whatever the badge says. A pick hands `onPick` the moment its preset names, computed at
 * the pick, not when the page rendered, and Un-snooze, offered where `onUnsnooze` is given, calls
 * that. While `held` - a write is out that the pick would race - the trigger is `aria-disabled`,
 * never `disabled`, so it keeps the reader's focus: a press of it opens no list, and a pick from
 * a list that stood open as the hold began writes nothing.
 */
export function SnoozePick({
  badge,
  held,
  label,
  marks,
  onPick,
  onTriggerFocus,
  onUnsnooze,
  shortcutKeys,
}: {
  badge: string;
  held: boolean;
  label: string;
  /** The marks the host's keys find the trigger by. */
  marks: ListMarks;
  onPick: (until: string) => void;
  /** The focus arriving on the trigger from outside the list (`MultiSelect`'s). */
  onTriggerFocus?: (from: Element | null) => void;
  /** Ends the snooze of an item already snoozed: given, the list offers Un-snooze, on `0`. The
   *  selection bar's list, and a row not snoozed, offer none. */
  onUnsnooze?: () => void;
  /** The keys that open the list, where the host draws them on the trigger (the reading pane's
   *  head: `b` and `h`, or the person's own). */
  shortcutKeys?: readonly string[];
}): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <span className={badgeSelectWrapper}>
      <MultiSelect
        ariaDisabled={held}
        chevron={false}
        emptyMessage="No snooze time matches that."
        label={label}
        marks={marks}
        menu={SNOOZE_MENU}
        mode="single"
        onOpenChange={(next) => setOpen(next && !held)}
        onPick={(choice) => {
          if (held) return;
          const until = snoozeOf(choice, new Date());
          if (until === null) onUnsnooze?.();
          else onPick(until);
        }}
        onTriggerFocus={onTriggerFocus}
        open={open}
        optionLabel={(choice) => ALL_CHOICES.find(({ value }) => value === choice)?.label}
        options={onUnsnooze === undefined ? OPTIONS.plain : OPTIONS.snoozed}
        searchLabel="Search snooze times"
        shortcutKeys={shortcutKeys}
        triggerClassName={badgeSelectTrigger}
      >
        <span aria-hidden="true" className={`${badgeSelectBadge} ${badgeLow.bg} ${badgeLow.text}`}>
          {badge}
        </span>
      </MultiSelect>
    </span>
  );
}

/**
 * The per-row defer control. It is the "Snooze" badge and the list of `snooze.ts`'s choices it
 * opens (`SnoozePick`), whose text box narrows them as the reader types; the Inbox's `b` and `h`
 * press its trigger (`ROW_SNOOZE_SELECTOR`). On a row already inside its window it also shows the
 * return time and, before the list, the one button that ends the snooze early, which the list
 * offers too, as Un-snooze on `0` - nothing else does, which is the point: an agent answering a
 * deferred ask leaves it deferred. The list stays at its one place in both states, so `b` and `h`
 * open it on a snoozed item too.
 *
 * A snooze moves its row into Later, which starts folded, and an un-snooze moves it out, either
 * of which would unmount this control. So the write is not this control's: the Inbox holds it,
 * by ask (`useAskSnoozeWrites`), and keeps the row in the band the reader saw it in while the
 * write is out (`heldRow`). Every control drawn for the ask shows that one write - its
 * `Snoozing…`, its refusal and its Retry - whichever of them the reader pressed: the row's own,
 * the reading column's head, the same head in the reading view below 1280 px, and the short row
 * beside the column while the column shows another question. The column's head goes whenever the
 * reader moves the selection, and the write, the held row and a refusal stay. Under the Blocked
 * on you narrowing, which lists no snoozed ask, the row leaves the list at the pick instead: a
 * row a press names as leaving is never kept for the reader's hand in it (`Inbox.tsx`'s
 * `leavingNow`). The move is the feedback for a snooze that lands, made in the commit in which
 * the Inbox lets go of the row. A snooze the server
 * refuses, or one the reader stops, rolls the row's moment back and holds the row no longer, so
 * the next read of the Inbox places it; the reason stays with the ask, shown wherever the row is
 * drawn. The Snooze list and Un-snooze are held from the press
 * until the commit after their write's answer (`useHeldThroughAnswer`): the list's trigger by
 * `aria-disabled`, with a pick then writing nothing, and Un-snooze by `heldPress`. The Inbox lets
 * go of the row in the answer's own commit, so a row that moves takes its control away while that
 * control is still held.
 * The optimistic moment puts the row inside its window at once, where Un-snooze would be drawn
 * before the list; it is not drawn while the pick's write is out and through the commit after its
 * answer, so the list stays the one control there, held, with the return time being saved before
 * it. An un-snooze takes the row out of its window at once the same way, where Un-snooze would
 * go, so Un-snooze stays drawn, held, while its write is out and through the commit after its
 * answer. The press is the reader's, and its control - for a pick, the trigger of the list it was
 * made in - stays in the page and held until the row goes with it after the answer, so the focus
 * stays on that control until then, and a second press on it does nothing. That is a row's own
 * Un-snooze. In the reading column's head
 * (`ReadingPaneHead`) the control goes with the question the column shows, and the Inbox's own
 * hand-off sends the focus to the column's focus stop instead (`Inbox.tsx`). A control taken away
 * at the press would leave the row's going to drop the focus onto the page.
 *
 * A snooze that lands hands the reader on to the item after the row (`InboxHandOffContext`, R8):
 * the Inbox selects it beside the column and gives its row the focus, over the column's own
 * hand-off. The hand-off begins at the press, off the list as the reader saw it, and a snooze the
 * server refuses or the reader stops drops it and leaves the reader on the row.
 * Un-snooze moves nobody, and neither does a snooze pressed on a short row beside the column that
 * is not the item in hand (`RowAsideContext`): R8 is the item in hand's, so a snooze or an
 * un-snooze pressed there - Un-snooze, the list's Un-snooze or a Retry - only takes a focus it
 * left in the row to the item being read first.
 *
 * The selection bar's Snooze over the rows the reader marked holds the control the same way while
 * that write is out over this ask (`BulkSnoozingContext`): the optimistic moment would draw
 * Un-snooze there too, and a press on it would send a write racing the bar's, so it is not drawn:
 * the list stays drawn, held, with the return time being saved before it, through the commit in
 * which the bar lets go of the row, which is the commit the Inbox moves the row in; a refused or
 * stopped bar write leaves the row where it was, with the reason on the bar, and its ordinary
 * control back in the commit after. One hold covers both writes, so no pick and no press of
 * Snooze or Un-snooze writes while either is out within its deadline. Each write is tried under
 * its key until Dispatch decides it, so past its deadline (`late`, `BulkSnoozing.late`) the
 * hold lets go, the list's Un-snooze among its choices, and a press then sends a save of its own:
 * a write that never answers - offline - holds the row no longer than that.
 *
 * The badge's `Snoozing…` is drawn for the eye alone (`aria-hidden`) and the list's trigger keeps
 * its name, so the write is said to a screen reader by a polite status of its own while it is
 * out, and that status says nothing once the write is decided.
 *
 * `label` names the row for assistive tech; the Inbox passes the issue key or the document.
 */
export function SnoozeControl({
  askId,
  label,
  shortcutKeys,
  snoozedUntil,
  unsnoozeKeys,
}: {
  askId: string;
  label: string;
  /** The keys that open the list, where the host draws them on the trigger (the reading pane's
   *  head: `b` and `h`, or the person's own). */
  shortcutKeys?: readonly string[];
  /** The row's own snooze while its moment is still ahead; null on an ordinary row. */
  snoozedUntil: string | null;
  /** The keys that press Un-snooze, where the host draws them on it (the reading pane's head,
   *  with a key a person gave it). */
  unsnoozeKeys?: readonly string[];
}): ReactNode {
  const write = useAskSnooze(askId);
  const handOff = useContext(InboxHandOffContext);
  const aside = useContext(RowAsideContext);
  const bulk = useContext(BulkSnoozingContext);
  const bulkSnoozing = bulk.asks.has(askId);
  // A snooze out over this row: its own pick's write, whose variable is a moment where an
  // un-snooze's is null, or the bar's.
  const snoozing = typeof write.pendingUntil === "string" || bulkSnoozing;
  // Either write holds the controls until its deadline, past which a pick or Un-snooze sends a
  // save of its own (`late`, `BulkSnoozing.late`).
  const held = useHeldThroughAnswer((write.pending && !write.late) || (bulkSnoozing && !bulk.late));
  const pickHeld = useHeldThroughAnswer(snoozing);
  const unsnoozeHeld = useHeldThroughAnswer(write.pendingUntil === null);
  /** Begins the hand-off a snooze press makes, at the press, before its optimistic write moves
   *  the row, and says what its settling tells the Inbox: a landing hands the reader on, a
   *  refusal or a stop drops it. The hand-off is of the press's own ask, which a Retry keeps
   *  whichever ask the control is drawn for by then. The Inbox holds the write, so it hears the
   *  settling whether or not this control is still drawn. An un-snooze, and a control outside an
   *  Inbox, hand nothing on; a snooze or an un-snooze on a row that is not the item in hand hands
   *  nobody on and moves a focus it left in the row first (`RowAsideContext`). */
  const handingOn = (press: AskSnoozePress | undefined): SnoozeSettled | undefined => {
    if (press === undefined) return undefined;
    if (aside !== undefined) {
      aside();
      return undefined;
    }
    if (press.until === null) return undefined;
    if (handOff === undefined) return undefined;
    const acted = press.askId;
    handOff.beginHandOff([acted]);
    return (landed) => {
      if (landed) handOff.landHandOff([acted]);
      else handOff.dropHandOff([acted]);
    };
  };
  /** An un-snooze press: it hands nobody on, and on a row that is not the item in hand it moves a
   *  focus it left in the row first (`RowAsideContext`). */
  const unsnooze = () => {
    aside?.();
    write.submit(null);
  };
  // `basis-full` inside the header's wrapping row: a refusal is wider than the badge it
  // replaces, so it takes a line of its own rather than drawing over the issue key. Its Retry is
  // held while either write is out within its deadline, the bar's included, as the controls are.
  const failure = write.failed ? (
    <div className="basis-full">
      <QueryError
        message={write.error ?? `Could not snooze ${label}.`}
        onRetry={() => write.retry(handingOn(write.failedPress))}
        retrying={held}
      />
    </div>
  ) : null;
  // Outside the Blocked on you narrowing the row stays in the band the reader saw it in while
  // the write is out, and a snooze in flight already shows the return time it is saving. What
  // the row cannot say is which way the reader is going, so the in-flight label comes from the
  // write that is out - its own, by its variable (a moment is a snooze, null is an un-snooze), or
  // the bar's, which is a snooze - and never from `snoozedUntil`, which by then describes the
  // destination. `pendingUntil` is undefined once the row's own write settles, and the bar's set
  // lets go of the ask once its write does, so a settled row says nothing of the last thing done
  // to it. A snooze's label is the list's badge, and an un-snooze's is Un-snooze's own.
  const going = write.pendingUntil === null ? "Un-snoozing" : snoozing ? "Snoozing" : undefined;

  // One tree for both states, the list at the same place in it: a snooze picked from the trigger
  // hands the focus back to the trigger, then makes the row a snoozed one, and a trigger drawn
  // anew there would take that focus off the page with the one it replaced. The status is drawn
  // first and empty while no write is out, so the row keeps its own one status, the same node,
  // across a press: a screen reader reads a status already there when its words change.
  return (
    <>
      <span className="sr-only" role="status">
        {going === undefined ? null : `${going} ${label}…`}
      </span>
      {snoozedUntil === null ? null : (
        <span className={`text-xs ${textMutedOnCanvas}`} data-inbox-snoozed-until={snoozedUntil}>
          {isIndefinite(snoozedUntil) ? (
            "Snoozed until you clear it"
          ) : (
            <>
              Back <Timestamp at={snoozedUntil} />
            </>
          )}
        </span>
      )}
      {unsnoozeHeld || (snoozedUntil !== null && !pickHeld) ? (
        <button
          aria-keyshortcuts={keyshortcutsOf(unsnoozeKeys)}
          aria-label={`Un-snooze ${label}`}
          className={secondaryButtonCompact}
          {...inboxActionMark("inbox.unsnooze")}
          {...heldPress(held, unsnooze)}
          type="button"
        >
          {going === "Un-snoozing" ? "Un-snoozing…" : "Un-snooze"}
          <ControlKeyHint keys={unsnoozeKeys} />
        </button>
      ) : null}
      <SnoozePick
        badge={going === "Snoozing" ? "Snoozing…" : "Snooze"}
        held={held}
        label={`Snooze ${label}`}
        marks={ROW_SNOOZE_MARKS}
        onPick={(until) => write.submit(until, handingOn({ askId, until }))}
        onUnsnooze={snoozedUntil === null ? undefined : unsnooze}
        shortcutKeys={shortcutKeys}
      />
      {failure}
    </>
  );
}
