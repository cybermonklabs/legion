import type { DispatchWritingBoxRow } from "@legion/contracts";
import { type ReactNode, useEffect, useRef, useState } from "react";
import {
  borderDefault,
  focusVisibleRing,
  surfaceBg,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { MentionField, type MentionFieldHandle } from "../conversation/MentionField";

/** How long the reader may stop typing before what they wrote is saved. Nothing else in
 *  Dispatch asks a person to press Save, so a priority's title and description are saved as
 *  they are written: once the typing rests, and at once when the field is left. */
const SAVE_AFTER_MS = 500;

/**
 * A field edited where it is read: no edit mode, no Save button. What the reader types is kept
 * here until it rests, then written once; leaving the field writes it immediately, and Enter in
 * a one-line field does both. The reader's own text stands until `value` itself changes, so a
 * refresh of the row underneath never pulls a half-typed word out from under them and the
 * round trip of their own write never shows them the old text again.
 *
 * Given its row of the writing list, the field holds markdown a person writes - a priority's
 * description - and is the shared text field with the reference picker in it, as every box a
 * person writes prose in is (`DISPATCH_WRITING_BOXES`, `priority-description`). Without one it
 * is a single line: a title, or an owner picked from what other rows hold.
 */
export function InPlaceField({
  autoFocus = false,
  id,
  label,
  onDone,
  onSave,
  options,
  placeholder,
  rows = 8,
  value,
  writing,
}: {
  /** Takes the focus as it is drawn: a field the reader opened by clicking what it edits. */
  autoFocus?: boolean;
  /** The one-line field's own id, where a visible label points at it rather than wrapping it.
   *  The markdown field's label is its accessible name alone. */
  id?: string;
  label: string;
  /** Told when the reader leaves the field, after what they wrote has been sent. */
  onDone?: () => void;
  onSave: (next: string) => void;
  /** What other rows already hold, offered beside the field: a reader picks one of the owners
   *  the deployment uses, or writes a new one, since nothing bounds what an owner may be. */
  options?: readonly string[];
  placeholder?: string;
  /** How tall the markdown field starts, in lines. */
  rows?: number;
  value: string;
  /** The writing list's row of the markdown this field holds, which makes it the shared text
   *  field: the line under it is the row's hint. */
  writing?: DispatchWritingBoxRow;
}): ReactNode {
  const [draft, setDraft] = useState(value);
  const [editing, setEditing] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  const latest = useRef({ draft, value, onSave });
  latest.current = { draft, value, onSave };
  const seen = useRef(value);

  // The server's text wins when it changes under a reader who is not holding the field:
  // another writer's rename, or this writer's own write coming back. The draft stands until
  // then, because leaving the field sends a write and the write is a round trip: resetting
  // the draft while it is in flight would show the reader their own text being undone, and
  // then written again when the answer lands.
  useEffect(() => {
    if (value === seen.current) return;
    seen.current = value;
    if (!editing) setDraft(value);
  }, [editing, value]);

  const stop = () => {
    if (timer.current !== undefined) window.clearTimeout(timer.current);
    timer.current = undefined;
  };
  const save = () => {
    stop();
    const { draft: typed, value: saved, onSave: write } = latest.current;
    if (typed !== saved) write(typed);
  };
  // The timer is cleared through the ref the render that set it wrote to, so the unmount takes
  // no dependency on this render's own `stop`.
  useEffect(
    () => () => {
      if (timer.current !== undefined) window.clearTimeout(timer.current);
    },
    []
  );
  // A field opened by a click on what it edits takes the focus once, with the caret at the end
  // of the text, so the reader writes on from where the text stands. The one-line field is an
  // input of this file's own; the markdown field's textarea is the shared field's, reached
  // through the handle it gives its host.
  const input = useRef<HTMLInputElement>(null);
  const markdown = useRef<MentionFieldHandle>(null);
  useEffect(() => {
    const node = input.current ?? markdown.current?.textarea() ?? null;
    if (!autoFocus || node === null) return;
    node.focus();
    node.setSelectionRange(node.value.length, node.value.length);
  }, [autoFocus]);

  const leave = () => {
    setEditing(false);
    save();
    onDone?.();
  };
  const type = (next: string) => {
    setDraft(next);
    stop();
    timer.current = window.setTimeout(save, SAVE_AFTER_MS);
  };
  if (writing !== undefined)
    return (
      <MentionField
        body={draft}
        className="font-mono text-sm"
        fieldRef={markdown}
        label={label}
        labelHidden
        onBlur={leave}
        onChange={(change) => type(change.body)}
        onFocus={() => setEditing(true)}
        placeholder={placeholder}
        rows={rows}
        writing={writing}
      />
    );
  // A plain bordered field: it is the text's one reading, written where it is read, so nothing
  // hides it until the reader hovers it.
  const listId = options === undefined ? undefined : `${label.replace(/\W+/g, "-")}-options`;
  return (
    <>
      <input
        aria-label={label}
        className={`w-full rounded-md border px-2 py-1 text-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas} ${focusVisibleRing}`}
        id={id}
        list={listId}
        onBlur={leave}
        onChange={(event) => type(event.target.value)}
        onFocus={() => setEditing(true)}
        onKeyDown={(event) => {
          if (event.key !== "Enter") return;
          event.preventDefault();
          save();
          event.currentTarget.blur();
        }}
        placeholder={placeholder}
        ref={input}
        type="text"
        value={draft}
      />
      {listId === undefined ? null : (
        <datalist id={listId}>
          {(options ?? []).map((option) => (
            <option key={option} value={option} />
          ))}
        </datalist>
      )}
    </>
  );
}
