import type { Selection, TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";

import { inputMethodKey } from "../../lib/input-method-key";
import { BLOCK_EDITOR_ATTRIBUTE } from "./block-editor";
import { type CaretAt, EDITOR_BOX_ATTRIBUTE, textOffsetAt } from "./node-caret";
import { AUTHOR_TEXT_ATTRIBUTE, KEY_REPORTS } from "./page-structure";
import { pressedWord, wordUnits } from "./spec-filter/pressed-words";

type PositionMap = (position: number) => number;

interface ReplayEditor {
  positionAt(offset: number): number | undefined;
  replace(from: number, to: number, text: string): void;
  select(head: number, anchor?: number): void;
  keydown(event: KeyboardEvent): void;
  /** The position mapping of the last edit or command, where the editor supplies one. */
  mapping?(): PositionMap | undefined;
  end(): void;
}

interface Snapshot {
  readonly text: string;
  readonly head: number;
  readonly anchor: number;
}

interface DrawnWords {
  readonly text: string;
  readonly held: string;
  readonly offsets: readonly (number | undefined)[];
}

/** These keys already have a ProseMirror selection mapped inside the bound block. */
export const replayedKeys = new WeakSet<KeyboardEvent>();

interface EarlyCaretOptions {
  readonly row: HTMLElement;
  readonly words: HTMLElement;
  readonly offset: number;
  readonly pressed?: CaretAt;
  readonly editable: boolean;
  readonly start?: () => void;
  readonly report: (line: string) => void;
  readonly onEscape?: () => void;
}

export interface EarlyCaretHold {
  bind(editor: ReplayEditor): Promise<void>;
  replay(view: EditorView, end: () => void): void;
  /** Wait for the browser's composition commit before moving the kept editor into the row. */
  handover(bind: (box?: HTMLElement) => void, reuseBox?: boolean): void;
  owns(target: EventTarget | null): boolean;
  leave(): boolean;
  /** Ends the stand-in with no bind: the row's words show again, and where it took typing, that
   *  typing is reported lost. Answers whether it took any. */
  refuse(): boolean;
  destroy(): void;
}

/** A browser-owned plaintext editor for the time between the press and the room's bind. */
export function openEarlyCaret({
  row,
  words,
  offset,
  pressed,
  editable,
  start,
  report,
  onEscape,
}: EarlyCaretOptions): EarlyCaretHold | undefined {
  if (!editable) {
    start?.();
    return undefined;
  }

  const drawn = readDrawnWords(words);
  // The click counts a reference by its written spelling. The stand-in shows its drawn label.
  const pressedOffset =
    pressed === undefined
      ? offset
      : pressed.next
        ? drawn.offsets.lastIndexOf(pressed.offset)
        : drawn.offsets.indexOf(pressed.offset);
  const caret = Math.max(0, Math.min(pressedOffset, drawn.text.length));
  const doc = row.ownerDocument;
  const box = doc.createElement("div");
  box.className = words.className;
  box.setAttribute(EDITOR_BOX_ATTRIBUTE, "");
  // The box the kept editor moves into at the bind, laid out as that editor's is from the start
  // (`styles.css`'s `[data-block-editor]`), so nothing moves as the editor takes its place.
  box.setAttribute(BLOCK_EDITOR_ATTRIBUTE, "");
  box.setAttribute(AUTHOR_TEXT_ATTRIBUTE, "");
  const input = doc.createElement("div");
  input.setAttribute("contenteditable", "plaintext-only");
  Object.assign(input.style, {
    outline: "none",
    whiteSpace: "pre-wrap",
    overflowWrap: "anywhere",
    minHeight: "1lh",
  });
  const text = input.appendChild(doc.createTextNode(drawn.text));
  box.append(input);
  words.before(box);
  // Keep the stand-in's hiding separate from the bound editor's `hidden` attribute.
  const display = words.style.getPropertyValue("display");
  const priority = words.style.getPropertyPriority("display");
  words.style.setProperty("display", "none");
  input.focus({ preventScroll: true });
  doc.getSelection()?.collapse(text, caret);

  const held: { snapshot: Snapshot; key: KeyboardEventInit }[] = [];
  const listeners = new AbortController();
  const options = { signal: listeners.signal };
  let snapshot = { text: drawn.text, head: caret, anchor: caret };
  let drawnHere = true;
  let composing = false;
  let left = false;
  let finished = false;
  let takeFocus = false;
  let waiting: (() => void) | undefined;
  let cancelReplay: (() => void) | undefined;

  function readInput() {
    if (!drawnHere) return;
    const selection = doc.getSelection();
    snapshot = {
      text: input.textContent ?? "",
      head: offsetIn(input, selection?.focusNode, selection?.focusOffset) ?? snapshot.head,
      anchor: offsetIn(input, selection?.anchorNode, selection?.anchorOffset) ?? snapshot.anchor,
    };
  }

  function detach(reuseBox = false) {
    if (!drawnHere) return;
    readInput();
    drawnHere = false;
    listeners.abort();
    if (reuseBox) input.remove();
    else box.remove();
    words.style.setProperty("display", display, priority);
  }

  function resume() {
    const bind = waiting;
    waiting = undefined;
    bind?.();
  }

  function leave() {
    left = true;
    composing = false;
    detach();
    resume();
    return held.length !== 0 || snapshot.text !== drawn.text;
  }

  function handover(bind: (box?: HTMLElement) => void, reuseBox = true) {
    if (finished) return;
    if (composing) {
      waiting = () => handover(bind, reuseBox);
      return;
    }
    takeFocus = !left && (input === doc.activeElement || row === doc.activeElement);
    const reuse = reuseBox && drawnHere && box.isConnected;
    detach(reuse);
    bind(reuse ? box : undefined);
    if (reuse && box.childNodes.length === 0) box.remove();
  }

  function apply(editor: ReplayEditor) {
    if (finished) return;
    let previous = drawn.text;
    let positionAt: PositionMap = (offset) => {
      for (let at = offset; at >= 0; at--) {
        const position = editor.positionAt(at);
        if (position !== undefined) return position;
      }
      return 0;
    };
    const applySnapshot = (next: Snapshot) => {
      if (previous !== next.text) {
        const { start, end, insert } = difference(previous, next.text);
        const from = positionAt(start);
        const to = Math.max(from, positionAt(end));
        editor.replace(from, to, insert);
        const mapped = editor.mapping?.();
        const before = positionAt;
        positionAt = (offset) => {
          if (offset >= start && offset <= start + insert.length) return from + offset - start;
          const old = before(offset < start ? offset : offset + end - start - insert.length);
          if (mapped !== undefined) return mapped(old);
          return old + (offset < start ? 0 : insert.length - (to - from));
        };
        previous = next.text;
      }
      editor.select(positionAt(next.head), positionAt(next.anchor));
    };
    for (const { snapshot: before, key } of held) {
      applySnapshot(before);
      const event = new KeyboardEvent("keydown", key);
      replayedKeys.add(event);
      editor.keydown(event);
      const map = editor.mapping?.();
      if (map !== undefined) {
        const previousMap = positionAt;
        positionAt = (offset) => map(previousMap(offset));
      }
    }
    applySnapshot(snapshot);
    finished = true;
    if (left) editor.end();
  }

  input.addEventListener(
    "compositionstart",
    () => {
      composing = true;
    },
    options
  );
  input.addEventListener(
    "compositionend",
    () => {
      // Read after the input method's last DOM edit, not from the composition event's data.
      composing = false;
      queueMicrotask(resume);
    },
    options
  );
  input.addEventListener("input", readInput, options);
  input.addEventListener("keyup", readInput, options);
  input.addEventListener("focusout", readInput, options);
  input.addEventListener(
    "keydown",
    (event) => {
      if (event.defaultPrevented || composing || inputMethodKey(event)) return;
      if (event.key === "Escape") {
        event.preventDefault();
        leave();
        row.focus({ preventScroll: true });
        onEscape?.();
        return;
      }
      readInput();
      const key = event.key.toLowerCase();
      const mod = event.metaKey || event.ctrlKey;
      const comment =
        mod && event.altKey && !event.shiftKey && (key === "m" || event.code === "KeyM");
      if (
        key !== "enter" &&
        key !== "tab" &&
        !(mod && !event.altKey && key === "z") &&
        !comment &&
        !(key === "backspace" && snapshot.head === 0 && snapshot.anchor === 0)
      )
        return;
      event.preventDefault();
      const { code, ctrlKey, metaKey, altKey, shiftKey, repeat, location } = event;
      held.push({
        snapshot,
        key: {
          key: event.key,
          code,
          ctrlKey,
          metaKey,
          altKey,
          shiftKey,
          repeat,
          location,
          bubbles: true,
          cancelable: true,
        },
      });
    },
    options
  );

  start?.();
  return {
    handover,
    owns: (target) => target === input,
    leave,
    bind(editor) {
      if (finished) return Promise.resolve();
      const result = Promise.withResolvers<void>();
      cancelReplay = result.resolve;
      handover(() => {
        cancelReplay = undefined;
        try {
          apply(editor);
          result.resolve();
        } catch (error) {
          result.reject(error);
        }
      }, false);
      return result.promise;
    },
    replay(view, end) {
      apply(editorFor(view, drawn, pressed, caret, takeFocus, end));
    },
    refuse() {
      if (finished) return false;
      const focused = takeFocus || input === doc.activeElement;
      finished = true;
      waiting = undefined;
      cancelReplay?.();
      detach();
      if (focused) row.focus({ preventScroll: true });
      // Only typing the stand-in took is lost: one that took none has nothing to say.
      const typed =
        snapshot.text !== drawn.text || held.some(({ snapshot }) => snapshot.text !== drawn.text);
      if (typed) report(KEY_REPORTS.editor_text_not_saved);
      return typed;
    },
    destroy() {
      finished = true;
      waiting = undefined;
      cancelReplay?.();
      detach();
    },
  };
}

function offsetIn(input: HTMLElement, node?: Node | null, offset = 0): number | undefined {
  if (node == null || !input.contains(node)) return undefined;
  if (node === input.firstChild && node === input.lastChild && node instanceof Text) return offset;
  const range = input.ownerDocument.createRange();
  range.selectNodeContents(input);
  range.setEnd(node, offset);
  return range.toString().length;
}

/** One replacement, with no overlap between the common start and end. Keep surrogate pairs whole. */
function difference(before: string, after: string) {
  const length = Math.min(before.length, after.length);
  let start = 0;
  while (start < length && before[start] === after[start]) start++;
  if (/[\uD800-\uDBFF]/.test(before.charAt(start - 1))) start--;
  let suffix = 0;
  while (
    suffix < length - start &&
    before[before.length - 1 - suffix] === after[after.length - 1 - suffix]
  )
    suffix++;
  if (/[\uDC00-\uDFFF]/.test(before.charAt(before.length - suffix))) suffix--;
  return { start, end: before.length - suffix, insert: after.slice(start, after.length - suffix) };
}

/** The same units as `pressInWords`. There is no editor position inside a drawn chip label. */
function readDrawnWords(words: HTMLElement): DrawnWords {
  let text = "";
  let held = "";
  const offsets: (number | undefined)[] = [0];
  for (const unit of wordUnits(words)) {
    const piece = unit.drawn.textContent ?? "";
    if (unit.other === null) {
      for (let at = 1; at <= piece.length; at++) offsets.push(held.length + at);
    } else {
      for (let at = 1; at < piece.length; at++) offsets.push(undefined);
      offsets[text.length + piece.length] = held.length + unit.text.length;
    }
    text += piece;
    held += unit.text;
  }
  return { text, held, offsets };
}

function editorFor(
  view: EditorView,
  drawn: DrawnWords,
  pressed: CaretAt | undefined,
  pressedOffset: number,
  focus: boolean,
  end: () => void
): ReplayEditor {
  const initial = view.state.doc;
  const SelectionClass = view.state.selection.constructor as typeof Selection;
  const first = SelectionClass.atStart(initial).from;
  const runs: { text: string; position: number }[] = [];
  const walker = view.dom.ownerDocument.createTreeWalker(view.dom, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node instanceof Text && node.parentElement?.isContentEditable === true && node.length > 0) {
      runs.push({ text: node.data, position: view.posAtDOM(node, 0) });
    }
  }
  const editorText = runs.map((run) => run.text).join("");
  let mapping: PositionMap | undefined;
  let takeFocus = focus;
  return {
    end,
    positionAt(at) {
      const offset = drawn.offsets[at];
      if (offset === undefined) return undefined;
      const click = at === pressedOffset ? pressed : undefined;
      let remaining = textOffsetAt(
        editorText,
        click ?? {
          offset,
          word: editorText === drawn.held ? undefined : pressedWord(drawn.held, offset),
        }
      );
      let last = first;
      for (const run of runs) {
        if (remaining < run.text.length + (click?.next ? 0 : 1)) return run.position + remaining;
        remaining -= run.text.length;
        last = run.position + run.text.length;
      }
      return last;
    },
    replace(from, to, text) {
      const tr = view.state.tr.insertText(text, from, to);
      view.dispatch(tr);
      mapping = (position) => tr.mapping.map(position);
    },
    select(head, anchor = head) {
      const doc = view.state.doc;
      const near = SelectionClass.near(doc.resolve(head));
      const selection =
        head === anchor || !near.$head.parent.inlineContent
          ? near
          : (near.constructor as typeof TextSelection).between(doc.resolve(anchor), near.$head);
      if (!view.state.selection.eq(selection)) view.dispatch(view.state.tr.setSelection(selection));
      if (takeFocus) {
        takeFocus = false;
        view.dom.focus({ preventScroll: true });
        view.focus();
      }
    },
    mapping: () => mapping,
    keydown(event) {
      mapping = undefined;
      const before = view.state.doc.content;
      view.dom.dispatchEvent(event);
      const after = view.state.doc.content;
      const start = before.findDiffStart(after);
      if (start === null) return;
      const end = before.findDiffEnd(after);
      if (end === null) return;
      const oldEnd = Math.max(start, end.a);
      const newEnd = oldEnd + after.size - before.size;
      mapping = (position) =>
        position < start ? position : position <= oldEnd ? newEnd : position + newEnd - oldEnd;
    },
  };
}
