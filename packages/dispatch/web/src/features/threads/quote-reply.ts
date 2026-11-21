// Quote reply (INFRA-3231's R7): the reader selects words of an ask, a
// comment, a reply or a message and presses Quote reply on the selection bar
// (`SelectionBar.tsx`), or `q`. The reply box of that same thread opens, or is used where it is
// open, and takes the words as a Markdown quote of the Markdown that draws them
// (`selection-markdown.ts`), at the end of what it holds, with the cursor on the empty line under
// it.
import { hashKey, type MutationKey } from "@tanstack/react-query";
import { type RefObject, useCallback, useLayoutEffect, useRef } from "react";

import { type KeyBinding, type KeymapScope, useKeymap } from "../shell/keymap";
import { selectionMarkdown } from "./selection-markdown";

/**
 * The Markdown blocks of a selection (`selectionMarkdown`) as one Markdown quote: each line of a
 * block after `> ` (a bare `>` for a line a code block leaves empty), and a bare `>` line between
 * two blocks, so each block the page draws apart stays its own where the quote is drawn. No block
 * quotes nothing (`""`).
 */
export function quoteBlocks(blocks: readonly string[]): string {
  return blocks
    .filter((block) => block.trim() !== "")
    .map((block) =>
      block
        .split("\n")
        .map((line) => (line === "" ? ">" : `> ${line}`))
        .join("\n")
    )
    .join("\n>\n");
}

/** A box's text with a quote at its end, and where the cursor goes: the empty line under it. */
export interface Quoted {
  readonly caret: number;
  readonly text: string;
}

/**
 * What a reply box holds once `blocks` are quoted into it: `current`, then a blank line where it
 * holds any text, then the quote and a blank line, and where the cursor goes, the empty line
 * under that. Null where the selection quotes nothing.
 */
export function withQuote(current: string, blocks: readonly string[]): Quoted | null {
  const quote = quoteBlocks(blocks);
  if (quote === "") return null;
  const kept = current.trimEnd();
  const text = `${kept === "" ? "" : `${kept}\n\n`}${quote}\n\n`;
  return { caret: text.length, text };
}

/** The lines of a box's text that are the reader's own words: every line but a quote's (`>`,
 *  after at most three spaces, as Markdown reads one). What a check of what the reader wrote
 *  reads - an empty answer, an answer shaped as a question - so a quote is never taken for it. */
export function ownWords(text: string): string {
  return text
    .split("\n")
    .filter((line) => !/^ {0,3}>/.test(line))
    .join("\n");
}

/**
 * The box's part of a Quote reply, for the three boxes that take one (an ask's answer box, a
 * thread's reply box, a composer). The box writes the quoted text into its own state and hands
 * it to the function this answers; once a render has committed that text - `text` is the box's
 * state as rendered - the box grows to show it, takes the focus with the cursor on the empty line
 * under the quote, and comes into view with the form around it where that fits the window, so
 * the button that sends it is on screen too. A box emptied afterwards goes back to its own size.
 *
 * It waits for the render that holds the quote rather than for the next commit: a box the press
 * itself opened runs its effects before that render, and would measure and focus an empty box.
 */
export function useQuotePlacement(
  field: RefObject<HTMLTextAreaElement | null>,
  text: string,
  placed?: () => void
): (quoted: Quoted) => void {
  const pending = useRef<Quoted | null>(null);
  const grew = useRef(false);
  useLayoutEffect(() => {
    const element = field.current;
    if (element === null) return;
    if (text === "" && grew.current) {
      element.style.height = "";
      grew.current = false;
    }
    const quoted = pending.current;
    if (quoted === null || quoted.text !== text) return;
    pending.current = null;
    grew.current = true;
    const frame = element.offsetHeight - element.clientHeight;
    element.style.height = "auto";
    element.style.height = `${Math.min(element.scrollHeight + frame, Math.round(window.innerHeight / 2))}px`;
    element.focus({ preventScroll: true });
    element.setSelectionRange(quoted.caret, quoted.caret);
    const form = element.form;
    const whole =
      form !== null && form.getBoundingClientRect().height <= window.innerHeight ? form : element;
    whole.scrollIntoView({ block: "nearest" });
    element.scrollIntoView({ block: "nearest" });
    placed?.();
  });
  return useCallback((quoted) => {
    pending.current = quoted;
  }, []);
}

// ---------------------------------------------------------------------------------------------
// The texts a reader may quote
// ---------------------------------------------------------------------------------------------

/** What one text a reader may quote does with a selection of its words. */
export interface QuoteTarget {
  /** Quotes the selection's Markdown blocks into the reply box of the text's own thread: opens
   *  it, or takes the one open, and gives it the focus. */
  readonly quote: (blocks: readonly string[]) => void;
  /** Present where the text offers Define (an ask's words): opens it on the selection. */
  readonly define?: () => void;
}

/** Each text's target; `null` for a text whose thread takes no reply now, which no text around
 *  it answers for either. */
const targets = new WeakMap<Element, QuoteTarget | null>();

/** Records `target` for the text drawn under `ref`, again on every commit so it reaches that
 *  render's state: `null` while its thread takes no reply (a decided suggestion's, one whose
 *  send is out). A selection is the innermost recorded text's that holds it. */
export function useQuoteTarget(ref: RefObject<Element | null>, target: QuoteTarget | null): void {
  useLayoutEffect(() => {
    const element = ref.current;
    if (element === null) return undefined;
    targets.set(element, target);
    return () => {
      if (targets.get(element) === target) targets.delete(element);
    };
  });
}

/** The callback ref `useQuoteTargetRef` hands out: it records the element it is given, and the
 *  function it answers lets the element go. */
export type QuoteRef = (element: Element | null) => () => void;

/** `useQuoteTarget` for a text drawn in more than one element - an answered ask's record and the
 *  thread under it, each another component's - as the callback ref each of them takes. */
export function useQuoteTargetRef(target: QuoteTarget | null): QuoteRef {
  const elements = useRef(new Set<Element>());
  const current = useRef(target);
  current.current = target;
  useLayoutEffect(() => {
    for (const element of elements.current) targets.set(element, target);
  });
  return useCallback((element: Element | null) => {
    const held = elements.current;
    if (element !== null) {
      held.add(element);
      targets.set(element, current.current);
    }
    return () => {
      if (element === null) return;
      held.delete(element);
      targets.delete(element);
    };
  }, []);
}

/** The `<input>` types that take no typed text: while one of them has the focus, a selection on
 *  the page is still the reader's (ticking an option must not hide the bar). */
const NON_TEXT_INPUTS: Record<string, true> = {
  button: true,
  checkbox: true,
  color: true,
  file: true,
  hidden: true,
  image: true,
  radio: true,
  range: true,
  reset: true,
  submit: true,
};

function typingIn(element: Element | null): boolean {
  if (element instanceof HTMLTextAreaElement || element instanceof HTMLSelectElement) return true;
  if (element instanceof HTMLInputElement) return NON_TEXT_INPUTS[element.type] !== true;
  return element instanceof HTMLElement && element.isContentEditable;
}

/** A selection of words the reader may quote: the text that holds them, their Markdown blocks,
 *  and where they and that text stand on screen: the whole selection's box, its first line's,
 *  and the text's own column. */
export interface QuotableSelection {
  readonly target: QuoteTarget;
  readonly blocks: readonly string[];
  readonly rect: DOMRect;
  readonly firstLine: DOMRect;
  readonly column: DOMRect;
}

/** The selection, where it lies inside one text a reader may quote and no field has the focus.
 *  A field's words are the reader's own, and a document's text stays out, edited or only read
 *  (its editor's root, `.ProseMirror`): the document draws its own bar, and a decision block's
 *  words are the document's. */
export function quotableSelection(): QuotableSelection | null {
  if (typingIn(document.activeElement)) return null;
  const selection = window.getSelection();
  if (selection === null || selection.rangeCount === 0 || selection.isCollapsed) return null;
  const range = selection.getRangeAt(0);
  for (let node: Node | null = range.commonAncestorContainer; node !== null; ) {
    if (
      node instanceof HTMLElement &&
      (node.isContentEditable || node.classList.contains("ProseMirror"))
    ) {
      return null;
    }
    if (node instanceof Element && targets.has(node)) {
      const target = targets.get(node);
      if (target == null) return null;
      const blocks = selectionMarkdown(range, node);
      if (quoteBlocks(blocks) === "") return null;
      // A text drawn without a box of its own (`QuotableText`) stands in its parent's column.
      const box = node.getBoundingClientRect();
      const column = box.width > 0 ? box : (node.parentElement?.getBoundingClientRect() ?? box);
      const rect = range.getBoundingClientRect();
      const firstLine = [...range.getClientRects()].find((line) => line.width > 0) ?? rect;
      return { blocks, column, firstLine, rect, target };
    }
    node = node.parentNode;
  }
  return null;
}

/** Quotes the selection into its thread's reply box, and says whether there was one to quote. */
export function quoteSelection(): boolean {
  const selected = quotableSelection();
  if (selected === null) return false;
  selected.target.quote(selected.blocks);
  return true;
}

/** The selection bar's two keys, in `scope`'s section of the shortcut help: `q` quotes the
 *  selected words of an ask, a comment, a reply or a message on the page in a reply, and `d`
 *  opens Define on the selected words of an ask, wherever the focus is. Off the Inbox an ask
 *  card's own `d` (`ask-card-keys.ts`) acts only while the focus is inside the card, and a
 *  selection made with the mouse leaves the focus where the press put it, outside the card. A
 *  page that draws no ask (`asks: false`, the Agents page) registers `q` alone, so its help
 *  lists no `d` that never applies there. */
export function useSelectionBarKeys(scope: KeymapScope, { asks = true } = {}): void {
  const quote = quoteReplyKey("Quote selected words in a reply", quoteSelection);
  useKeymap(
    scope,
    asks
      ? [
          quote,
          {
            id: "define-selected-ask-words",
            keys: "d",
            label: "Define selected words of an ask",
            // The palette's own field takes the selection, so a row of it would define nothing.
            palette: false,
            run: () => quotableSelection()?.target.define?.(),
            when: () => quotableSelection()?.target.define !== undefined,
          },
        ]
      : [quote]
  );
}

/** The binding `q` is, for a host that decides when it applies and what it quotes. */
export function quoteReplyKey(
  label: string,
  run: () => void,
  when: () => boolean = () => quotableSelection() !== null
): KeyBinding {
  return {
    id: "quote-reply",
    keys: "q",
    label,
    // The palette's own field takes the selection, so a row of it would quote nothing.
    palette: false,
    run,
    when,
  };
}

// ---------------------------------------------------------------------------------------------
// A composer's quote, on its way
// ---------------------------------------------------------------------------------------------

/** The selection each composer is to quote, by its send's name, until the composer takes it: one
 *  that is mounted at once, one the press opens as it mounts. */
const waiting = new Map<string, readonly string[]>();
const wakers = new Map<string, Set<() => void>>();

/** Hands `blocks` to the composer whose send is named `mutationKey`, mounted or about to be. */
export function quoteInto(mutationKey: MutationKey, blocks: readonly string[]): void {
  const name = hashKey(mutationKey);
  waiting.set(name, blocks);
  for (const wake of wakers.get(name) ?? []) wake();
}

/** The selection waiting for the composer named `name`, taken; undefined where none waits. */
export function takeQuote(name: string | undefined): readonly string[] | undefined {
  if (name === undefined) return undefined;
  const blocks = waiting.get(name);
  waiting.delete(name);
  return blocks;
}

/** Whether a selection waits for the composer named `name`, left where it is. */
export function quoteWaiting(name: string | undefined): boolean {
  return name !== undefined && waiting.has(name);
}

/** Calls `wake` whenever a selection is handed to the composer named `name`: the composer itself,
 *  and a control that opens it (`AskThreadDisclosure`'s Write a reply). */
export function onQuote(name: string, wake: () => void): () => void {
  const set = wakers.get(name) ?? new Set();
  set.add(wake);
  wakers.set(name, set);
  return () => {
    set.delete(wake);
    if (set.size === 0 && wakers.get(name) === set) wakers.delete(name);
  };
}
