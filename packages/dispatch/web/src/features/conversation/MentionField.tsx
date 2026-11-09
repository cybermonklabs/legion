import type { DispatchWritingBoxRow } from "@legion/contracts";
import {
  type ClipboardEvent,
  type DragEvent,
  type KeyboardEvent,
  type ReactNode,
  type Ref,
  type SyntheticEvent,
  useCallback,
  useEffect,
  useId,
  useImperativeHandle,
  useMemo,
  useRef,
  useState,
} from "react";

import {
  inputClasses,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { RefPickerList } from "../refs/RefPickerList";
import { applyInsertion, type RefPickerInsertion } from "../refs/ref-picker";
import { useRefPicker } from "../refs/useRefPicker";
import type { TextEditRange } from "./block-spans";
import type { AcceptedMention } from "./composer-model";
import { survivingMentions } from "./send-request";

/** What a box that sends no mention passes: one value, so the memo over it never sees a new
 *  array. */
const NO_MENTIONS: readonly AcceptedMention[] = [];

/** The parties a pick can name - the live sessions, the roles their holders carry, and the
 *  issue's Legion roles nobody holds - are `features/refs/party-options.ts`, not this file:
 *  the picker builds its agent and role rows from them, and this field hosts that picker, so
 *  holding them here would make the two modules import each other (`noImportCycles`). */

/** Reconcile accepted mentions against the actual textarea edit range. */
export function reconcileMentions(
  before: string,
  after: string,
  mentions: readonly AcceptedMention[],
  edit?: TextEditRange
): AcceptedMention[] {
  if (edit !== undefined) {
    const delta = after.length - before.length;
    return mentions.flatMap((mention) => {
      if (mention.end <= edit.start) return [mention];
      if (mention.start >= edit.end) {
        return [{ ...mention, end: mention.end + delta, start: mention.start + delta }];
      }
      return [];
    });
  }
  const delta = after.length - before.length;
  if (delta < 0) {
    const deleted = -delta;
    const possibleStarts = Array.from({ length: after.length + 1 }, (_, start) => start).filter(
      (start) =>
        before.slice(0, start) === after.slice(0, start) &&
        before.slice(start + deleted) === after.slice(start)
    );
    return mentions.flatMap((mention) => {
      if (possibleStarts.some((start) => mention.start < start + deleted && mention.end > start)) {
        return [];
      }
      const shifts = new Set(possibleStarts.map((start) => (start < mention.start ? delta : 0)));
      if (shifts.size !== 1) return [];
      const shift = shifts.values().next().value;
      if (shift === undefined) return [];
      return [{ ...mention, end: mention.end + shift, start: mention.start + shift }];
    });
  }
  let prefix = 0;
  const common = Math.min(before.length, after.length);
  while (prefix < common && before[prefix] === after[prefix]) prefix += 1;
  return mentions.flatMap((mention) => {
    if (mention.end <= prefix) return [mention];
    if (mention.start >= prefix) {
      return [{ ...mention, end: mention.end + delta, start: mention.start + delta }];
    }
    return [];
  });
}

export function mentionDisplay(title: string, target: string): string {
  const trimmed = title.trim();
  if (trimmed !== "" && !/^(?:session|role):/i.test(trimmed) && !/[@\r\n]/.test(trimmed)) {
    return trimmed;
  }
  return target.replace(":", " ");
}

/** One edit of the field: the text now, the accepted mentions reconciled against it, and the
 *  edit the browser named, which a host following block spans maps them through (`followEdit`).
 *  Both arrive together because a mention's offsets are into that exact text. */
export interface MentionFieldChange {
  readonly body: string;
  readonly inputType?: string;
  readonly mentions: AcceptedMention[];
  readonly range?: TextEditRange;
}

/** What a host reaches the field it mounted by, through the `fieldRef` it passed. */
export interface MentionFieldHandle {
  /** Closes the picker, leaving the draft alone: a send does, since the words the picker stood
   *  over are on the wire. */
  readonly closePicker: () => void;
  /** The field's textarea as it stands, null while the field is unmounted. A host reads what the
   *  reader typed from here rather than from the value React last rendered - a keystroke can land
   *  between that render and a press of Send - and moves the focus into it. */
  readonly textarea: () => HTMLTextAreaElement | null;
}

/**
 * The body field of a draft and the reference picker in it: one textarea, the mentions accepted
 * in it, and the rows that `[[`, `@` and `#` open (`useRefPicker`). The host holds the draft -
 * the text and the accepted mentions reach it through `onChange`, and come back as props -
 * because the host is what sends it; the field holds only where the caret is and what the
 * picker is showing. Every box a person writes prose in mounts it, so each one has the same
 * picker with the same rules (`DISPATCH_WRITING_BOXES`).
 *
 * While the picker is open it owns the arrow keys, Enter, Tab and Escape, on a listener of the
 * field's own, and none of them reaches anything around the field: the Inbox binds the arrows
 * and Enter as its own hotkeys (`features/inbox/Inbox.tsx`), an ask's option field adds an
 * option on Enter, and a card or a dialog closes on Escape. With the picker closed every key is
 * the host's, as it always was.
 */
export function MentionField({
  body,
  className,
  disabled = false,
  fieldRef,
  handle,
  inline = false,
  issueKey,
  label,
  labelClassName,
  labelHidden = false,
  mentions = NO_MENTIONS,
  onChange,
  onDrop,
  onKeyDown,
  onPaste,
  onPickerOpenChange,
  placeholder,
  project,
  rows = 1,
  writing,
}: {
  readonly body: string;
  /** What this host's layout asks of the box itself, after the field's own classes: a height
   *  the host holds it to, or the text size its surface writes in. */
  readonly className?: string;
  /** Takes no typing: a draft whose own send is out, which the host shows the reader rather than
   *  letting them write over what is already on the wire. The picker is shut with it. */
  readonly disabled?: boolean;
  /** How the host reaches this field (`MentionFieldHandle`): a ref it holds, or, for a host
   *  with a field per row, a callback that files the handle under its row. */
  readonly fieldRef?: Ref<MentionFieldHandle | null>;
  /** A bare `data-` attribute written on the textarea itself, the one handle a host outside the
   *  draft reaches the box by: the Inbox's answer keys find the selected ask's answer box by
   *  `data-ask-answer` and move the focus into it (`features/inbox/Inbox.tsx`). */
  readonly handle?: string;
  /** A one-line field that grows with what is typed, as a reply's does. */
  readonly inline?: boolean;
  /** The issue this box writes on, where it has one: the picker offers that issue's people and
   *  the Legion roles it records that nobody is running. A box with none offers everyone who
   *  has signed in (`DISPATCH_WRITING_BOXES`'s `issueRows`). */
  readonly issueKey?: string;
  /** The field's accessible name, shown above it unless `labelHidden`. */
  readonly label: string;
  /** What this host's layout asks of the label, in place of the field's own classes: the label
   *  is the box's one element in the host's flow, so a host that lays its boxes out itself - a
   *  grid row, a flex form - dresses this rather than the textarea. */
  readonly labelClassName?: string;
  readonly labelHidden?: boolean;
  /** The mentions accepted in `body`, whose offsets are into that exact text. A box that sends
   *  none passes none. */
  readonly mentions?: readonly AcceptedMention[];
  readonly onChange: (change: MentionFieldChange) => void;
  readonly onDrop?: (event: DragEvent<HTMLTextAreaElement>) => void;
  /** The host's own keys - a submit on Ctrl/Cmd+Enter, an ask option on Enter - which the keys
   *  an open picker owns never reach. */
  readonly onKeyDown?: (event: KeyboardEvent<HTMLTextAreaElement>) => void;
  readonly onPaste?: (event: ClipboardEvent<HTMLTextAreaElement>) => void;
  /** Told whenever the picker opens or closes: a host lifts itself over whatever it floats
   *  above while the list is showing. */
  readonly onPickerOpenChange?: (open: boolean) => void;
  /** What the empty box says it takes, where the host has words for it (an ask's answer box
   *  names what the picked option asks of the reader). */
  readonly placeholder?: string;
  /** The project whose components the picker offers, where the box names one of its own; a box
   *  on an issue needs none, the issue's key holding it. */
  readonly project?: string;
  /** How tall the box starts, in lines. */
  readonly rows?: number;
  /** This box's row of the writing list, for the write it is making now: the line under the box
   *  is the row's `hint`, so the words a person reads and the list cannot disagree. */
  readonly writing: DispatchWritingBoxRow;
}): ReactNode {
  const element = useRef<HTMLTextAreaElement | null>(null);
  /** Where the caret is, which is what the picker reads the open door from. It follows the
   *  box's own selection rather than the end of the text: a trigger typed in the middle of a
   *  sentence opens the picker there, and a pick replaces that stretch alone. It starts at the
   *  front because nothing has been typed yet: a draft the host seeded (`@Planner`) is not a
   *  trigger the reader is standing on. */
  const [caret, setCaret] = useState(0);
  /** The reader is in this box. A picker over a box nobody is writing in has nothing to pick
   *  for, and would read the seeded text of every other box on the page as a trigger. */
  const [focused, setFocused] = useState(false);
  // What the field last showed - its text and the records in step with it - which the next edit
  // is measured from. It is the host's own state, which the render below brings back, plus
  // whatever an edit in this same task has already made of it, since the host's state is a
  // render behind that. A draft the host puts in the field (a held send's, an upload's text, a
  // reference) arrives the same way, so the next edit is measured from that instead.
  const previousBody = useRef(body);
  previousBody.current = body;
  const shownMentions = useRef(mentions);
  shownMentions.current = mentions;
  const pendingTextEdit = useRef<
    { before: string; inputType: string; range: TextEditRange } | undefined
  >(undefined);
  const listboxId = useId();
  const hintId = useId();
  const picker = useRefPicker({
    caret,
    enabled: !disabled && focused,
    issueKey,
    project,
    text: body,
  });
  const { open } = picker;
  const activeMentions = useMemo(() => survivingMentions(body, mentions), [body, mentions]);

  // The key handler and the handle below read the picker as it stands rather than as the render
  // that bound them saw it, so neither is bound again on a keystroke.
  const pickerNow = useRef(picker);
  pickerNow.current = picker;
  const closePicker = useCallback(() => {
    pickerNow.current.pressKey("Escape");
  }, []);
  useImperativeHandle(fieldRef, () => ({ closePicker, textarea: () => element.current }), [
    closePicker,
  ]);
  useEffect(() => {
    onPickerOpenChange?.(open);
  }, [onPickerOpenChange, open]);

  /** A row taken: the picker says what to write and which stretch of the draft it replaces, and
   *  the field writes it. The write is one edit like any other, so an accepted mention whose
   *  span it crosses goes with it; a party's pick adds none of its own - it writes the link
   *  that names the party, which is what the body is read for. */
  const takePick = (insertion: RefPickerInsertion) => {
    const current = element.current?.value ?? body;
    const after = applyInsertion(current, insertion);
    const accepted = reconcileMentions(current, after.text, shownMentions.current, {
      end: insertion.end,
      start: insertion.start,
    });
    previousBody.current = after.text;
    shownMentions.current = accepted;
    setCaret(after.caret);
    onChange({ body: after.text, mentions: accepted });
    requestAnimationFrame(() => {
      element.current?.focus();
      element.current?.setSelectionRange(after.caret, after.caret);
    });
  };
  const changed = (value: string, selectionStart: number | null) => {
    const before = previousBody.current;
    const pending = pendingTextEdit.current;
    pendingTextEdit.current = undefined;
    const accepted = reconcileMentions(
      before,
      value,
      shownMentions.current,
      pending?.before === before ? pending.range : undefined
    );
    previousBody.current = value;
    shownMentions.current = accepted;
    onChange({
      body: value,
      inputType: pending?.inputType,
      mentions: accepted,
      range: pending?.range,
    });
    setCaret(selectionStart ?? value.length);
  };
  /** The keys an open picker owns, on a listener of the field's own. React's handlers run when
   *  the event reaches the root, by which time it has passed every host between - the Inbox's
   *  window-bound hotkeys, an ask option's Enter, a card's or a dialog's Escape - so stopping it
   *  there would stop nothing: only a listener on the field itself is in front of them. It is
   *  written on every render and read through a ref, so it sees the picker as it stands without
   *  being bound again. */
  const pickerKey = useRef<(event: globalThis.KeyboardEvent) => void>(() => {});
  pickerKey.current = (event) => {
    if (!pickerNow.current.open) return;
    // A modified Enter is not the picker's: Ctrl/Cmd+Enter is the host's submit and Shift+Enter
    // the reader's newline, picker or no picker.
    if (event.key === "Enter" && (event.ctrlKey || event.metaKey || event.altKey || event.shiftKey))
      return;
    const result = pickerNow.current.pressKey(event.key);
    // A key the picker ignores is the host's, as it always was: Enter with no row under it
    // types its newline or adds the ask option, and an arrow the list has no answer for moves
    // the caret.
    if (result.action === "ignored") return;
    event.preventDefault();
    event.stopPropagation();
    if (result.action === "took") takePick(result.insertion);
  };
  // The body field records each edit from the browser's own `beforeinput`: its type, which tells
  // `mapSpans` a cut or a paste, and the range it replaces. React's `onBeforeInput` is the
  // `textInput` event, which carries no type and which no deletion or cut fires.
  const bodyFieldRef = useCallback((node: HTMLTextAreaElement | null) => {
    element.current = node;
    if (node === null) return;
    const recordEdit = (input: InputEvent) => {
      let start = node.selectionStart;
      let end = node.selectionEnd;
      if (start === end && input.inputType === "deleteContentBackward")
        start = Math.max(0, start - 1);
      if (start === end && input.inputType === "deleteContentForward")
        end = Math.min(node.value.length, end + 1);
      pendingTextEdit.current = {
        before: node.value,
        inputType: input.inputType,
        range: { end, start },
      };
    };
    const pickerKeyDown = (event: globalThis.KeyboardEvent) => pickerKey.current(event);
    node.addEventListener("beforeinput", recordEdit);
    node.addEventListener("keydown", pickerKeyDown);
    return () => {
      node.removeEventListener("beforeinput", recordEdit);
      node.removeEventListener("keydown", pickerKeyDown);
      element.current = null;
    };
  }, []);

  return (
    <>
      <label className={labelClassName ?? `block text-sm font-medium ${textSecondaryOnSurface}`}>
        {labelHidden ? null : label}
        <textarea
          aria-activedescendant={picker.activeId}
          aria-controls={open ? listboxId : undefined}
          aria-describedby={hintId}
          aria-label={label}
          className={`mt-1 block w-full rounded-lg border px-3 py-2 font-normal outline-none ${inline ? "min-h-11 resize-none" : "min-h-24"} ${inputClasses(true)} ${textPrimaryOnSurface} ${className ?? ""}`}
          disabled={disabled}
          onChange={(event) => changed(event.target.value, event.target.selectionStart)}
          onDrop={onDrop}
          onInput={
            inline
              ? (event: SyntheticEvent<HTMLTextAreaElement>) => {
                  event.currentTarget.style.height = "auto";
                  event.currentTarget.style.height = `${event.currentTarget.scrollHeight}px`;
                }
              : undefined
          }
          // The list closes with the box the reader leaves. A row of it keeps the focus here
          // while it is clicked (`RefPickerList` stops the mouse down), so a pick is no blur.
          onBlur={() => setFocused(false)}
          onFocus={(event: SyntheticEvent<HTMLTextAreaElement>) => {
            setCaret(event.currentTarget.selectionStart);
            setFocused(true);
          }}
          onKeyDown={onKeyDown}
          onPaste={onPaste}
          // Every caret move, not only an edit: the picker reads the door from the text before
          // the caret, so a reader who clicks or arrows back into a `[[` they typed earlier
          // opens it there.
          onSelect={(event: SyntheticEvent<HTMLTextAreaElement>) =>
            setCaret(event.currentTarget.selectionStart)
          }
          placeholder={placeholder}
          ref={bodyFieldRef}
          rows={rows}
          value={body}
          {...(handle === undefined ? {} : { [handle]: "" })}
        />
      </label>
      {activeMentions.length === 0 ? null : (
        <ul
          aria-label="Mention targets"
          className={`flex flex-wrap gap-2 text-xs ${textPrimaryOnSurface}`}
        >
          {activeMentions.map((mention) => (
            <li className="rounded-full border px-2 py-1" key={mention.target}>
              {mention.text} · <code>{mention.target}</code>
            </li>
          ))}
        </ul>
      )}
      <RefPickerList id={listboxId} onTake={takePick} picker={picker} />
      {/* What this box does with a pick, in the writing list's own words, so the line under the
          box and the list that drives the browser rows cannot drift apart. */}
      <p className={`text-xs ${textMutedOnSurface}`} id={hintId}>
        {writing.hint}
      </p>
    </>
  );
}
