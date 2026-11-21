/** The reader's own texts a field keeps beside itself while it shows another person's: none. */
export const NOTHING_KEPT: readonly string[] = [];

/** Where a field of a priority stands against the server's text. */
export interface FieldStand {
  /** The text the reader's edit started from: the last of the field's writes still out, which
   *  the server holds once it lands, or else the last text of the server's the field took in.
   *  Only a text that differs from it is written. */
  readonly base: string;
  /** The texts the field wrote that the server has not answered with yet, oldest first: the
   *  server's answer with one of them is the reader's own write coming back. */
  readonly sent: readonly string[];
  /** The reader's own texts kept beside the field while it shows another person's
   *  (`ChangedMeanwhile`), newest first, none twice. */
  readonly kept: readonly string[];
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
 * Both fields decide it here, so they never read one arrival two ways.
 *
 * - The answer to one of the field's own writes is the reader's text coming back: it answers
 *   that write and each one sent before it, and no later one. The field's edit then starts from
 *   the last write still out, or from the answer where none is, so a write waiting for the typing
 *   to rest writes the reader's text again where it differs from that. Nothing is taken in, so
 *   the reader never sees their text undone while it travels.
 * - The text the field holds is no change: nothing is left to write. Nor is the text the edit
 *   started from, with no write of the field's out over it: the reader's typing over it is still
 *   theirs to write.
 * - Any other text is another person's, and is taken in. Where the reader holds typing not yet
 *   written, or a write of theirs is still out, their text is kept beside the field, ahead of
 *   any kept there already.
 *
 * A kept text the server now holds is no longer the reader's to put back, and goes.
 */
export function arrival(server: string, held: string, stand: FieldStand): Arrival {
  const kept = without(stand.kept, server);
  const own = stand.sent.indexOf(server);
  if (own !== -1) {
    const sent = stand.sent.slice(own + 1);
    return { base: sent.at(-1) ?? server, kept, sent, take: false };
  }
  if (server === held) return { base: server, kept, sent: [], take: false };
  if (server === stand.base) return { ...stand, kept, take: false };
  const holding = held !== stand.base || stand.sent.length > 0;
  return {
    base: server,
    kept: holding ? [held, ...without(kept, held)] : kept,
    sent: [],
    take: true,
  };
}
