import {
  type EditorState,
  Plugin,
  PluginKey,
  TextSelection,
  type Transaction,
} from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";

/**
 * The document's side of Dispatch's reference picker: the editor reads what stands before the
 * caret and writes the pick, and the host owns the rows and the keyboard, the way the host owns
 * the grammar in `./reference-decorations.ts` and the editor owns the positions.
 *
 * What crosses the boundary is text and offsets into that text, never a document position: the
 * editor reports the caret's own text block up to the caret (`ReferencePickerCaret`), the host
 * reads its trigger and its query from those characters exactly as a `<textarea>` host reads
 * them from its value, and a pick comes back as the replacement text and the stretch of that
 * same text it replaces (`ReferenceInsertion`). So the host needs no knowledge of ProseMirror,
 * and a stale offset is refused here rather than writing over whatever has since moved under it.
 *
 * The write is one transaction, built before anything is dispatched, as `./typed-block-commands`
 * builds its commands: that is what carries the pick to every other reader of the document
 * through collaboration and puts the reference in the stored text. A DOM write or a text replace
 * outside a transaction would do neither.
 */

/** Stands in for an inline node that is not text - an image, a hard break - so one character of
 *  the reported text is one position of the document and an offset the host answers with is a
 *  position again. */
const LEAF_CHAR = "\ufffc";

/** What stands before the caret, as the host's picker reads a draft. */
export interface ReferencePickerCaret {
  /** The text of the block the caret stands in, up to the caret. The host reads its open door
   *  and its query from these characters, and answers with offsets into them. */
  readonly text: string;
  /** Where the caret is on the page, for the list the host draws by it: the viewport
   *  coordinates `EditorView.coordsAtPos` gives. */
  readonly left: number;
  readonly top: number;
  readonly bottom: number;
}

/** The host's side of the picker, the whole of it in one value, as `EditorReferences` is:
 *  `ProofEditorHandle.setReferencePicker` replaces the last one. */
export interface EditorReferencePicker {
  /** Called whenever what stands before the caret changes, and with `undefined` whenever there
   *  is nothing to pick for: no focus, a selection that is not a caret, or a caret in code,
   *  where a trigger character is code the author typed. */
  onCaret(caret: ReferencePickerCaret | undefined): void;
  /** Offered every key the document takes, which is every key the reader types in it. A key the
   *  host's picker took - a move through its rows, a pick, a dismissal - reaches the document no
   *  further. */
  pressKey(key: string): boolean;
}

/** A pick, in the text the editor last reported: the words to write, and the stretch of that
 *  text they replace - the trigger the reader typed through the caret. */
export interface ReferenceInsertion {
  readonly text: string;
  readonly start: number;
  readonly end: number;
}

interface ReferencePickerState {
  /** What the host last set, or `null` while it has set none. */
  readonly picker: EditorReferencePicker | null;
  readonly focused: boolean;
}

type PickerChange =
  | { readonly kind: "picker"; readonly picker: EditorReferencePicker | null }
  | { readonly kind: "focus"; readonly focused: boolean };

const pickerKey = new PluginKey<ReferencePickerState>("dispatch-reference-picker");

/** Where the caret's own text block's text starts in the document, and what it holds up to the
 *  caret. */
interface CaretText {
  readonly start: number;
  readonly text: string;
}

/** The caret's text block read, or undefined when the selection is not a caret standing in text
 *  a reference may be written in. */
function caretText(state: EditorState): CaretText | undefined {
  const { empty, $head } = state.selection;
  if (!empty) return undefined;
  const parent = $head.parent;
  if (!parent.isTextblock || parent.type.spec.code === true) return undefined;
  return {
    start: $head.start(),
    text: parent.textBetween(0, $head.parentOffset, undefined, LEAF_CHAR),
  };
}

/** The transaction a pick makes, or undefined when the offsets no longer name a stretch of the
 *  caret's own text: the reader typed, or the document moved under the row they clicked. */
function insertionTransaction(
  state: EditorState,
  insertion: ReferenceInsertion
): Transaction | undefined {
  const caret = caretText(state);
  if (caret === undefined) return undefined;
  const { end, start, text } = insertion;
  if (!Number.isInteger(start) || !Number.isInteger(end)) return undefined;
  if (start < 0 || start > end || end > caret.text.length) return undefined;
  const from = caret.start + start;
  const to = caret.start + end;
  const transaction = state.tr.insertText(text, from, to);
  // Where the caret lands is the write's own doing: the reader types on after the reference.
  const after = from + text.length;
  return transaction.setSelection(TextSelection.create(transaction.doc, after));
}

/**
 * Writes `insertion` into the document as one transaction and dispatches it, answering whether
 * it did. Nothing is dispatched for an insertion the document no longer fits, so a refused pick
 * leaves the document exactly as it was.
 */
export function insertReference(
  state: EditorState,
  dispatch: (transaction: Transaction) => void,
  insertion: ReferenceInsertion
): boolean {
  const transaction = insertionTransaction(state, insertion);
  if (transaction === undefined) return false;
  dispatch(transaction);
  return true;
}

function dispatchPickerChange(view: EditorView, change: PickerChange): void {
  if (view.isDestroyed) return;
  view.dispatch(view.state.tr.setMeta(pickerKey, change).setMeta("addToHistory", false));
}

/** Whether two readings say the same thing, so a caret that has not moved is reported once. */
function sameCaret(
  left: ReferencePickerCaret | undefined,
  right: ReferencePickerCaret | undefined
): boolean {
  if (left === undefined || right === undefined) return left === right;
  return (
    left.text === right.text &&
    left.left === right.left &&
    left.top === right.top &&
    left.bottom === right.bottom
  );
}

/** What the host is told about the caret as the view stands, with the place to draw by read
 *  from the view itself. */
function readCaret(view: EditorView): ReferencePickerCaret | undefined {
  const caret = caretText(view.state);
  if (caret === undefined) return undefined;
  const { bottom, left, top } = view.coordsAtPos(view.state.selection.head);
  return { bottom, left, text: caret.text, top };
}

/**
 * The editor's half of the reference picker. The plugin holds what the host set and whether the
 * document has focus, reports the caret's text whenever it changes, and hands the focused
 * document's keys to the host's picker first, which is what lets an open list own the arrows and
 * Enter while a closed one leaves every key to the document.
 */
export const referencePickerPlugin = $prose(
  () =>
    new Plugin<ReferencePickerState>({
      key: pickerKey,
      state: {
        init: () => ({ focused: false, picker: null }),
        apply(transaction, value) {
          const change = transaction.getMeta(pickerKey) as PickerChange | undefined;
          if (change === undefined) return value;
          if (change.kind === "focus") {
            return change.focused === value.focused ? value : { ...value, focused: change.focused };
          }
          return change.picker === value.picker ? value : { ...value, picker: change.picker };
        },
      },
      view() {
        let reported: ReferencePickerCaret | undefined;
        let told: EditorReferencePicker | null = null;
        return {
          update(updated) {
            const state = pickerKey.getState(updated.state);
            const picker = state?.picker ?? null;
            const focused = state?.focused === true;
            const caret = picker === null || !focused ? undefined : readCaret(updated);
            // A picker just set is told where the caret is even when it has not moved: it has
            // been told nothing yet.
            if (picker === told && sameCaret(caret, reported)) return;
            told = picker;
            reported = caret;
            picker?.onCaret(caret);
          },
        };
      },
      props: {
        // A keydown reaches the document only while the reader is typing in it, so the key is
        // offered whenever a picker is hosted: what the list does with it is the host's to say.
        handleKeyDown(view, event) {
          return pickerKey.getState(view.state)?.picker?.pressKey(event.key) === true;
        },
        handleDOMEvents: {
          focus(view) {
            dispatchPickerChange(view, { focused: true, kind: "focus" });
            return false;
          },
          blur(view) {
            dispatchPickerChange(view, { focused: false, kind: "focus" });
            return false;
          },
        },
      },
    })
);

/** Hosts `picker` in the document, in place of whatever was hosted before; `null` hosts none. */
export function setReferencePicker(view: EditorView, picker: EditorReferencePicker | null): void {
  const current = pickerKey.getState(view.state);
  if (current === undefined || current.picker === picker) return;
  dispatchPickerChange(view, { kind: "picker", picker });
}
