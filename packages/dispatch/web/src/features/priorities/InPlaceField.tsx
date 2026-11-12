import { type ReactNode, useEffect, useRef, useState } from "react";
import {
  borderDefault,
  focusVisibleRing,
  surfaceBg,
  textPrimaryOnCanvas,
} from "../../theme/classes";

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
 */
export function InPlaceField({
  id,
  label,
  multiline = false,
  onSave,
  options,
  placeholder,
  rows = 8,
  value,
}: {
  /** The field's own id, where a visible label points at it rather than wrapping it. */
  id?: string;
  label: string;
  multiline?: boolean;
  onSave: (next: string) => void;
  /** What other rows already hold, offered beside the field: a reader picks one of the owners
   *  the deployment uses, or writes a new one, since nothing bounds what an owner may be. */
  options?: readonly string[];
  placeholder?: string;
  rows?: number;
  value: string;
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

  const common = {
    "aria-label": label,
    id,
    onBlur: () => {
      setEditing(false);
      save();
    },
    onChange: (event: { target: { value: string } }) => {
      setDraft(event.target.value);
      stop();
      timer.current = window.setTimeout(save, SAVE_AFTER_MS);
    },
    onFocus: () => setEditing(true),
    placeholder,
    value: draft,
  };
  // A plain bordered field: it is the text's one reading, written where it is read, so nothing
  // hides it until the reader hovers it.
  const className = `w-full rounded-md border px-2 py-1 text-sm ${borderDefault} ${surfaceBg} ${textPrimaryOnCanvas} ${focusVisibleRing}`;
  if (multiline) return <textarea {...common} className={`${className} font-mono`} rows={rows} />;
  const listId = options === undefined ? undefined : `${label.replace(/\W+/g, "-")}-options`;
  return (
    <>
      <input
        {...common}
        className={className}
        list={listId}
        onKeyDown={(event) => {
          if (event.key !== "Enter") return;
          event.preventDefault();
          save();
          event.currentTarget.blur();
        }}
        type="text"
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
