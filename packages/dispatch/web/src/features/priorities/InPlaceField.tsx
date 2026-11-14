import { type ReactNode, useEffect, useRef } from "react";
import {
  borderDefault,
  focusVisibleRing,
  surfaceBg,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { useInPlaceDraft } from "./useInPlaceDraft";

/**
 * A one-line field edited where it is read - a title, or an owner picked from what other rows
 * hold - saved as it is written (`useInPlaceDraft`), and at once on Enter. A priority's
 * description, which is markdown a person writes, is the shared text field instead
 * (`PriorityDescription`).
 */
export function InPlaceField({
  autoFocus = false,
  id,
  label,
  onDone,
  onSave,
  options,
  placeholder,
  value,
}: {
  /** Takes the focus as it is drawn: a field the reader opened by clicking what it edits. */
  autoFocus?: boolean;
  /** The field's own id, where a visible label points at it rather than wrapping it. */
  id?: string;
  label: string;
  /** Told when the reader leaves the field, after what they wrote has been sent. */
  onDone?: () => void;
  onSave: (next: string) => void;
  /** What other rows already hold, offered beside the field: a reader picks one of the owners
   *  the deployment uses, or writes a new one, since nothing bounds what an owner may be. */
  options?: readonly string[];
  placeholder?: string;
  value: string;
}): ReactNode {
  const { draft, hold, leave, save, type } = useInPlaceDraft(value, onSave, onDone);
  // A field opened by a click on what it edits takes the focus once, with the caret at the end
  // of the text, so the reader writes on from where the text stands.
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => {
    const node = input.current;
    if (!autoFocus || node === null) return;
    node.focus();
    node.setSelectionRange(node.value.length, node.value.length);
  }, [autoFocus]);

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
        onFocus={hold}
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
