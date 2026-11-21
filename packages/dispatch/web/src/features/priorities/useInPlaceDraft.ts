import { useCallback, useEffect, useRef, useState } from "react";

/** How long the reader may stop typing before what they wrote is saved. Nothing else in
 *  Dispatch asks a person to press Save, so a priority's title and description are saved as
 *  they are written: once the typing rests, and at once when the field is left. */
export const SAVE_AFTER_MS = 500;

/** A field edited where it is read, as its host wires it to whatever element it draws. */
export interface InPlaceDraft {
  /** What the field shows: the reader's own text, or the server's when it changed under a
   *  reader who is not holding the field. */
  readonly draft: string;
  /** The newest text this field sent that `value` has not reached and Dispatch has not refused:
   *  what the server is about to hold, which a reading of the field shows in place of `value`
   *  while the write is out or its answer has not been read. Undefined while none is. */
  readonly sending: string | undefined;
  /** The reader typed `next`: it is kept, and saved once the typing rests. */
  readonly type: (next: string) => void;
  /** The reader is holding the field, so the server's text no longer replaces the draft. */
  readonly hold: () => void;
  /** The reader left the field: the draft is written now and `onDone` is told. */
  readonly leave: () => void;
}

/** One text this field sent, as an object of its own: a refusal takes back this send, never a
 *  later send of the same text. `landed` is set once Dispatch has answered it. */
interface Sent {
  readonly text: string;
  landed: boolean;
}

/**
 * The draft of a field edited where it is read: no edit mode, no Save button. What the reader
 * types is kept here until it rests, then written once; leaving the field writes it at once.
 * `InPlaceField` (one line) and `PriorityDescription` (the shared text field) each draw their
 * own element over it.
 *
 * A write is a round trip: `value` is the old text until the answer lands and the row is read
 * again. So the hook holds three things - the draft, whether the reader holds the field, and the
 * texts it sent that `value` has not reached yet, oldest first (`out`), which is what the server
 * is about to hold - and moves between them only these ways:
 *
 * - Typing: the draft is the reader's text, and the rest's timer starts again. Nothing is sent.
 * - Sent and pending: a rest, or a leave, sends the draft and adds it to `out`, unless it is the
 *   text the server is about to hold - the newest text out, or `value` while none is. A rest and
 *   then a leave therefore send one write, and a reader who types back to the old text while a
 *   write is out has that text written.
 * - Refused: the send leaves `out`, wherever it stands in it. The draft stays the reader's, so
 *   the next rest or leave sends it again. A Retry of it from the pending-writes indicator is
 *   none of this field's: it is the reader's own new write of that text, made at the back of the
 *   line (`Writes.retry`), so its landing reads as another writer's text does (below).
 * - `value` reaches a text in `out`: that send's answer is in, and so is every older one's, so
 *   it and every send before it leave `out`. The earliest send of that text is the one matched:
 *   the sends in `out` land in press order, so a read can answer a later send of it only once
 *   every send between has landed, and those leave `out` as landed sends do.
 * - A landed send leaves `out` on the next move of `value`, whatever `value` moves to: Dispatch
 *   has answered it, so the read is what it holds, and no landed send is still about to be. A
 *   send of the text Dispatch already held, which moves nothing, therefore stays only until
 *   `value` moves, and two answers read out of order leave none of them behind.
 * - Once none is left, the draft becomes `value` unless the reader holds the field: a refresh
 *   never pulls a half-typed word out from under them.
 * - A stale read landing while a newer write is out: `value` reaches an older text in `out`, and
 *   the newer sends still out stay. The draft stands, since it already holds the newer text, so
 *   the field never shows the old text again and a leave sends nothing.
 * - `value` moves to a text this field never sent - another writer's, or the server's own
 *   reading of a text sent: `out` empties, since the server holds that text now, and the draft
 *   becomes `value` unless the reader holds the field. A draft that differs from it is written,
 *   even where it was sent before.
 *
 * A write that lands is marked so and moves nothing else: its answer is `value` moving.
 */
export function useInPlaceDraft(
  value: string,
  onSave: (next: string) => Promise<unknown>,
  onDone?: () => void
): InPlaceDraft {
  const [draft, setDraft] = useState(value);
  const [editing, setEditing] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  const latest = useRef({ draft, value, onSave });
  latest.current = { draft, value, onSave };
  const seen = useRef(value);
  const out = useRef<readonly Sent[]>([]);
  // The newest text out, as state as well: a reading of the field draws it, and a refusal that
  // takes it back has to draw again.
  const [sending, setSending] = useState<string | undefined>(undefined);
  const record = useCallback((next: readonly Sent[]) => {
    out.current = next;
    setSending(next.at(-1)?.text);
  }, []);

  useEffect(() => {
    if (value === seen.current) return;
    seen.current = value;
    const reached = out.current.findIndex((sent) => sent.text === value);
    const left = (reached === -1 ? [] : out.current.slice(reached + 1)).filter(
      (sent) => !sent.landed
    );
    record(left);
    if (!editing && left.length === 0) setDraft(value);
  }, [editing, record, value]);

  const stop = () => {
    if (timer.current !== undefined) window.clearTimeout(timer.current);
    timer.current = undefined;
  };
  const save = () => {
    stop();
    const { draft: typed, value: saved, onSave: write } = latest.current;
    if (typed === (out.current.at(-1)?.text ?? saved)) return;
    const sent: Sent = { landed: false, text: typed };
    record([...out.current, sent]);
    void write(typed).then(
      () => {
        sent.landed = true;
      },
      () => {
        if (out.current.includes(sent)) record(out.current.filter((entry) => entry !== sent));
      }
    );
  };
  // The timer is cleared through the ref the render that set it wrote to, so the unmount takes
  // no dependency on this render's own `stop`.
  useEffect(
    () => () => {
      if (timer.current !== undefined) window.clearTimeout(timer.current);
    },
    []
  );

  return {
    draft,
    hold: () => setEditing(true),
    leave: () => {
      setEditing(false);
      save();
      onDone?.();
    },
    sending,
    type: (next: string) => {
      setDraft(next);
      stop();
      timer.current = window.setTimeout(save, SAVE_AFTER_MS);
    },
  };
}
