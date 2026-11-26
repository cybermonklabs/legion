/** The reader's own texts a field keeps beside itself while it shows another person's: none. */
export const NOTHING_KEPT: readonly string[] = [];

/** What the page answers of a field's write that ended with no verdict of Dispatch's - the
 *  reader stopped it, say: whether it landed is not known, so its text may be on the server or
 *  not. */
export const NOT_KNOWN = "not known";

/** What the page answers of a write a field made (`onSave`): that it landed (`true`), that it did
 *  not (`false`), or that it is not known (`NOT_KNOWN`). */
export type WriteAnswer = boolean | typeof NOT_KNOWN;

/** A text a field wrote that no read of the server's has shown it yet, and what the page said of
 *  its write (`answered`): nothing yet (`false`), while the write is out; that it landed (`true`),
 *  which makes the text the server's, and no write of the reader's still out; or that it is not
 *  known (`NOT_KNOWN`): no write still out either, but a read that shows the text is the reader's
 *  own write coming back, not another person's text. */
export interface SentText {
  readonly text: string;
  readonly landed: WriteAnswer;
}

/** Where a field of a priority stands against the server's text. */
export interface FieldStand {
  /** The text the field compares the reader's with: the last text it wrote, or the last text of
   *  the server's it took in, whichever came last. Only a text that differs from it is written. */
  readonly base: string;
  /** The texts the field wrote that the server has not answered with yet, oldest first: the
   *  server's answer with one of them is the reader's own write coming back. A text whose write
   *  did not land leaves it (`answered`): no answer brings it back. One whose landing is not known
   *  stays, since a read may still show it. */
  readonly sent: readonly SentText[];
  /** The reader's own texts kept beside the field while it shows another person's
   *  (`ChangedMeanwhile`), newest first, none twice. */
  readonly kept: readonly string[];
  /** The text the reader's next edit is made on: the last text the field wrote, or the last of
   *  the server's it came to hold. Typing not yet written began on it (`arrival`). */
  readonly made: string;
}

/** Where a field stands once a new text of the server's arrived, and whether it takes that text
 *  in: shows it in place of what it holds, and stops the write waiting for the typing to rest,
 *  which would write over it. */
export interface Arrival extends FieldStand {
  readonly take: boolean;
}

/** `kept` without `text`, and `kept` itself where it holds no such text. */
function without(kept: readonly string[], text: string): readonly string[] {
  return kept.includes(text) ? kept.filter((one) => one !== text) : kept;
}

/**
 * What a field of a priority makes of `server`, a new text of the server's, while the field holds
 * `held`: the reader's text as the field compares it, trimmed in a one-line field
 * (`InPlaceField`) and the editor's own markdown in the description's (`PriorityDescription`).
 * Both fields decide it here, so they never read one arrival two ways. Whatever they make of it,
 * each takes `server` as the server's text its writes go over from then on (its `from`): a read
 * is the one thing that moves that, and a write never does.
 *
 * - The answer to one of the field's own writes is the reader's text coming back: it answers
 *   that write and each one sent before it, and no later one. That is so of a write whose landing
 *   is not known too: its text arriving is the reader's, landed after all, and no other person's.
 *   The field then compares the reader's text with the last write after it still out or landed,
 *   so a write waiting for the typing to rest writes the reader's text again where it differs
 *   from that. With none, it compares with the answer, unless the reader still holds the text it
 *   compared with: one that differs from the answer is the text of a write that ended without
 *   landing (refused, stopped or discarded), which no save writes again on its own, since the
 *   reader discarded it, or the shell's line holds it, with its Retry. Nothing is taken in, so
 *   the reader never sees their text undone while it travels.
 * - The text the field holds is no change: nothing is left to write. Nor is the text the field
 *   compares with, with no write of the field's out over it: the reader's typing over it is still
 *   theirs to write.
 * - Any other text is another person's, and is taken in. Where the reader holds typing not yet
 *   written, or a write of theirs is still out - one the page has said nothing of yet - their
 *   text is kept beside the field, ahead of any kept there already. One that landed was the
 *   server's text until theirs replaced it, as one a read had shown would have been, and one
 *   whose landing is not known is no write still out.
 *
 * A kept text the server now holds is no longer the reader's to put back, and goes. The reader's
 * next edit is made on the text the field takes in, or on the text it compares with where the
 * reader holds that very text (`made`). Typing not yet written that survives the arrival began on
 * the text the edit was made on until then, which stays.
 */
export function arrival(server: string, held: string, stand: FieldStand): Arrival {
  const kept = without(stand.kept, server);
  const own = stand.sent.findIndex((one) => one.text === server);
  if (own !== -1) {
    const sent = stand.sent.slice(own + 1);
    const last = sent.filter((one) => one.landed !== NOT_KNOWN).at(-1);
    const base = last?.text ?? (held === stand.base ? stand.base : server);
    return { base, kept, made: held === base ? base : stand.made, sent, take: false };
  }
  if (server === held) return { base: server, kept, made: server, sent: [], take: false };
  // Here the reader holds typing not yet written over `base`: it began on `made`, which stays.
  if (server === stand.base) return { ...stand, kept, take: false };
  const holding = held !== stand.base || stand.sent.some((one) => one.landed === false);
  return {
    base: server,
    kept: holding ? [held, ...without(kept, held)] : kept,
    made: server,
    sent: [],
    take: true,
  };
}

/** `sent` once the page answered the write that pushed `entry` - that very entry, never another of
 *  the same text: landed, the text stays until a read shows it, and is no write still out; not
 *  known, it stays too, as no write still out, since a read may still show it; and not landed,
 *  it goes, since no answer of the server's brings it back. An entry `sent` no longer holds - a
 *  read answered it, or another person's text ended it - changes nothing. */
export function answered(
  sent: readonly SentText[],
  entry: SentText,
  landed: WriteAnswer
): readonly SentText[] {
  const at = sent.indexOf(entry);
  if (at === -1) return sent;
  if (landed === false) return [...sent.slice(0, at), ...sent.slice(at + 1)];
  return sent.map((one, place) => (place === at ? { ...one, landed } : one));
}

/** The text Use my text of `text`, one of the reader's kept texts `kept`, is made on, where the
 *  click lets every one of `kept` go: what `text`'s own write was made on (`madeOn`), or, where
 *  that is itself one of `kept`, which goes with the click, what that one's write was made on, and
 *  so on down the run. Undefined where no write of `text` is known. */
export function usedOn(
  text: string,
  kept: readonly string[],
  madeOn: (text: string) => string | undefined
): string | undefined {
  const passed = new Set([text]);
  let on = madeOn(text);
  while (on !== undefined && kept.includes(on) && !passed.has(on)) {
    passed.add(on);
    const below = madeOn(on);
    if (below === undefined) break;
    on = below;
  }
  return on;
}
