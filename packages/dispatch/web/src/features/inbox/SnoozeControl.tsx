import { createContext, type ReactNode, useContext } from "react";
import { heldPress, useHeldThroughAnswer } from "../../components/held-press";
import { QueryError } from "../../components/QueryError";
import { badgeLow, secondaryButtonCompact, textMutedOnCanvas } from "../../theme/classes";
import { badgeSelectBadge, badgeSelectOverlay, badgeSelectWrapper } from "../issue/badge-select";
import { Timestamp } from "../refs/Timestamp";
import { isIndefinite, SNOOZE_PRESETS } from "./snooze";
import { useAskSnooze } from "./useAskSnooze";

/** No ask: a control drawn outside an Inbox has no selection bar over it. */
const NO_BULK_SNOOZING: ReadonlySet<string> = new Set();

/**
 * The asks the selection bar's own Snooze write is over while that write is out
 * (`useInboxSelection`'s `snoozing`), which the bar empties once the write settles - refused or
 * stopped included. The Inbox provides it over every control it draws, a row's and the reading
 * column's head alike. It is a context, as `AskHandOnWritesContext` is, so that neither the row
 * nor the column's bodies and head pass down one more prop of the list's. With no provider no
 * ask is under the bar.
 */
export const BulkSnoozingContext = createContext<ReadonlySet<string>>(NO_BULK_SNOOZING);

/**
 * The per-row defer control. On an ordinary row it is the "Snooze" badge with the same
 * invisible native `<select>` over it that the priority editor uses, so the menu is one tap on
 * a phone and one keyboard interaction on a desktop; the picks are `snooze.ts`'s presets and
 * the moment is computed when the reader picks, not when the page rendered. On a row already
 * inside its window it is the return time and the one button that ends the snooze early -
 * nothing else does, which is the point: an agent answering a deferred ask leaves it deferred.
 *
 * A snooze moves its row into Later, which starts folded, and an un-snooze moves it out, either
 * of which would unmount this control. So the write is not this control's: the Inbox holds it,
 * by ask (`useAskSnoozeWrites`), and keeps the row in the band the reader saw it in while the
 * write is out (`heldRow`). Every control drawn for the ask shows that one write - its
 * `Snoozing…`, its refusal and its Retry - whichever of them the reader pressed: the row's own,
 * the reading column's head, and the short row beside the column while the column shows another
 * question. The column's head goes whenever the reader moves the selection, and the write, the
 * held row and a refusal stay. Under the Blocked on you narrowing, which lists no snoozed ask,
 * the row leaves the list at the pick instead, kept only while the reader's hand is on it. The
 * move is the feedback for a snooze that lands, made in the commit in which the Inbox lets go of
 * the row. A snooze the server refuses, or one the reader stops, rolls the row's moment back and
 * holds the row no longer, so the next read of the Inbox places it; the reason stays with the
 * ask, shown wherever the row is drawn. The Snooze select and Un-snooze are held from the press
 * until the commit after their write's answer (`useHeldThroughAnswer`): the select by
 * `aria-disabled`, with a pick then writing nothing, and Un-snooze by `heldPress`. The Inbox lets
 * go of the row in the answer's own commit, so a row that moves takes its control away while that
 * control is still held.
 * The optimistic moment puts the row inside its window at once, where Un-snooze would be drawn in
 * the select's place; the select stays drawn instead, held, with the return time being saved
 * before it, while its pick's write is out and through the commit after its answer. An
 * un-snooze takes the row out of its window at once the same way, where the select would be drawn
 * in Un-snooze's place, so Un-snooze stays drawn, held, while its write is out and through the
 * commit after its answer. The press is the reader's, and the row's going after the answer is
 * that press's doing (`causedByReader`) only while the control it landed on is still in the page
 * and held: the row goes with its held control, so the page's rescue hands the focus on as it
 * does for Dismiss and Assign to me, to the same control in the row that moves up into the row's
 * place. That is a row's own Snooze. In the reading column's head (`ReadingPaneHead`) the control
 * goes with the question the column shows, and the Inbox's own hand-off sends the focus to the
 * column's focus stop instead (`Inbox.tsx`). A control taken away at the press would leave the
 * row's going to drop the focus onto the page.
 *
 * The selection bar's Snooze over the rows the reader marked holds the control the same way while
 * that write is out over this ask (`BulkSnoozingContext`): the optimistic moment would draw
 * Un-snooze there too, and a press on it would send a write racing the bar's. The select stays
 * drawn instead, held, with the return time being saved before it, through the commit in which
 * the bar lets go of the row, which is the commit the Inbox moves the row in; a refused or
 * stopped bar write leaves the row where it was, with the reason on the bar, and its ordinary
 * control back in the commit after. One hold covers both writes, so no pick and no press of
 * Snooze or Un-snooze writes while either is out.
 *
 * The badge's `Snoozing…` is drawn for the eye alone (`aria-hidden`) and the select keeps its
 * name, so the write is said to a screen reader by a polite status of its own while it is out,
 * and that status says nothing once the write is decided.
 *
 * `label` names the row for assistive tech; the Inbox passes the issue key or the document.
 */
export function SnoozeControl({
  askId,
  label,
  snoozedUntil,
}: {
  askId: string;
  label: string;
  /** The row's own snooze while its moment is still ahead; null on an ordinary row. */
  snoozedUntil: string | null;
}): ReactNode {
  const write = useAskSnooze(askId);
  const bulkSnoozing = useContext(BulkSnoozingContext).has(askId);
  // A snooze out over this row: its own pick's write, whose variable is a moment where an
  // un-snooze's is null, or the bar's.
  const snoozing = typeof write.pendingUntil === "string" || bulkSnoozing;
  const held = useHeldThroughAnswer(write.pending || bulkSnoozing);
  const pickHeld = useHeldThroughAnswer(snoozing);
  const unsnoozeHeld = useHeldThroughAnswer(write.pendingUntil === null);
  // `basis-full` inside the header's wrapping row: a refusal is wider than the badge it
  // replaces, so it takes a line of its own rather than drawing over the issue key. Its Retry is
  // held while either write is out, the bar's included, so a Retry cannot race the bar's snooze.
  const failure = write.failed ? (
    <div className="basis-full">
      <QueryError
        message={write.error ?? `Could not snooze ${label}.`}
        onRetry={write.retry}
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
  // to it.
  const going = write.pendingUntil === null ? "Un-snoozing" : snoozing ? "Snoozing" : undefined;
  const pendingLabel = going === undefined ? undefined : `${going}…`;
  // Drawn first in both branches below and empty while no write is out, so the row keeps its own
  // one status, the same node, across a press: a screen reader reads a status already there when
  // its words change.
  const status = (
    <span className="sr-only" role="status">
      {going === undefined ? null : `${going} ${label}…`}
    </span>
  );

  // The row's return time, which a snooze in flight already shows: it is the moment being saved.
  const returnTime =
    snoozedUntil === null ? null : (
      <span className={`text-xs ${textMutedOnCanvas}`} data-inbox-snoozed-until={snoozedUntil}>
        {isIndefinite(snoozedUntil) ? (
          "Snoozed until you clear it"
        ) : (
          <>
            Back <Timestamp at={snoozedUntil} />
          </>
        )}
      </span>
    );

  if (unsnoozeHeld || (snoozedUntil !== null && !pickHeld)) {
    return (
      <>
        {status}
        {returnTime}
        <button
          aria-label={`Un-snooze ${label}`}
          className={secondaryButtonCompact}
          {...heldPress(held, () => write.submit(null))}
          type="button"
        >
          {pendingLabel ?? "Un-snooze"}
        </button>
        {failure}
      </>
    );
  }

  return (
    <>
      {status}
      {returnTime}
      <span className={badgeSelectWrapper}>
        <span aria-hidden="true" className={`${badgeSelectBadge} ${badgeLow.bg} ${badgeLow.text}`}>
          {pendingLabel ?? "Snooze"}
        </span>
        <select
          aria-disabled={held || undefined}
          aria-label={`Snooze ${label}`}
          className={badgeSelectOverlay}
          data-inbox-snooze=""
          onChange={(event) => {
            // Held, a pick writes nothing, and the select, controlled at `""`, shows no pick.
            if (held) return;
            const preset = SNOOZE_PRESETS.find((option) => option.id === event.target.value);
            if (preset !== undefined) write.submit(preset.until(new Date()));
          }}
          value=""
        >
          <option value="">Snooze…</option>
          {SNOOZE_PRESETS.map((preset) => (
            <option key={preset.id} value={preset.id}>
              {preset.label}
            </option>
          ))}
        </select>
      </span>
      {failure}
    </>
  );
}
