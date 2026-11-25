import type { CaretAt } from "./node-caret";
import { FOCUSABLE } from "./RowText";

/**
 * Where a press on a row's drawn words puts the caret in its editor (R18, decisions 446 and 455):
 * the one point the Spec view's statements (`spec-filter/SpecFilterBody.tsx`) and the outline's
 * rows (`IssueTree.tsx`) both hand `useNodeEditing.ts`' `edit`, which `node-caret.ts` places the
 * caret at (`caretInBlock`) and holds the editor's box at (`holdEditorAtBlock`). Each caller says
 * which element holds its rows' words; everything else of the press is read the same way here.
 */

/** Which row's words a press hit - the element `selector` names, the Spec view's statement's
 *  words or an outline row's - excluding their links, controls and chips. */
export function statementWordsAt(target: EventTarget, selector: string): HTMLElement | null {
  if (!(target instanceof Element)) return null;
  const words = target.closest<HTMLElement>(selector);
  const control = target.closest(
    `${FOCUSABLE}, summary, [role="button"], [role="link"], [data-node-trail]`
  );
  return words !== null && !words.contains(control) ? words : null;
}

/** The elements of a row's words the editor holds as atoms, which no character of the count
 *  stands for (`HELD_OTHERWISE`): inline HTML, a footnote's reference, and a picture from a source
 *  that is no artifact, which the row and the editor both draw as an image. */
const ATOMS = 'img, [data-type="html"], [data-type="footnote_reference"]';

/** Whether a press on `target` landed on an atom of `words` (`ATOMS`), whose caret goes just past
 *  it (`CaretAt`'s `afterAtom`). */
export function pressedAtom(target: EventTarget, words: HTMLElement): boolean {
  return target instanceof Element && words.contains(target.closest(ATOMS));
}

/** The mark of a reference's link in a node's drawn words, which holds the reference as its text
 *  wrote it (`row-references.ts`'s `ROW_WRITTEN`). */
const REFERENCE_WRITTEN = "data-row-written";

/**
 * What a row's drawn words draw in place of text the row's editor holds otherwise, each counted
 * as the editor holds it: its `REFERENCE_WRITTEN` text where it carries one, and nothing where it
 * does not. These are the inline kinds whose drawing the two differ in:
 *
 * - A reference, and a mention of a document, a block or a post: the row's link pass
 *   (`row-references.ts`'s `linkRowReferences`) draws it as a link by its target's name - an
 *   issue's key and title, a chip's words - marked with the words it was written as; the editor
 *   holds those words as text, hidden under a title it draws as a widget the caret cannot enter
 *   (`@legion/proof-editor`'s `reference-decorations.ts`).
 * - Inline HTML (`<name>`) and a footnote's reference: both draw them by the schema's own
 *   `toDOM` (Milkdown's `span[data-type="html"]` holding its value, and `sup` holding its
 *   label), and the editor holds each as an atom ProseMirror draws `contenteditable="false"`.
 * - A picture of an artifact: the row draws it as a link (`markdown-serializer.ts`'s
 *   `pictureSerializer`) holding the picture, or its caption's link until it loads or where it
 *   fails (`DispatchPicture`); the editor holds it as an atom (`doc/picture-view.ts`), its
 *   picture or its fallback link with no text the caret can enter.
 *
 * Every other text counts as drawn: a hard break is a `br` in both, holding no text, and what
 * the editor draws as a widget - a reference's title, a collaborator's cursor - stands outside
 * its editable text and is not drawn by the row. What the row draws differently with no mark
 * of it - words drawn as stored, with no reading of them (`RowText`'s `StoredText`) - is the
 * pressed word's to find (`CaretAt`).
 */
const HELD_OTHERWISE = [
  `[${REFERENCE_WRITTEN}]`,
  '[data-type="html"]',
  '[data-type="footnote_reference"]',
  "[data-dispatch-picture]",
].join(", ");

/** Reads the press before the editor replaces the words, as the count of the editor's text
 *  characters before it: text, not markup or lines, so a different wrap in the editor does not
 *  change the caret's place. What the row draws in place of the editor's text counts as the
 *  editor holds it (`HELD_OTHERWISE`), and a press inside one counts as at its start. With the
 *  count it reads the word pressed, from the words read the same way, for the editor to find
 *  where the count lands in another word (`pressedWord`). */
export function pressInWords(words: HTMLElement, x: number, y: number): CaretAt {
  const doc = words.ownerDocument;
  const position = doc.caretPositionFromPoint?.(x, y);
  const range = position == null ? doc.caretRangeFromPoint?.(x, y) : undefined;
  const node = position?.offsetNode ?? range?.startContainer;
  if (node === undefined || !words.contains(node)) return { x, y, offset: 0 };
  const end = doc.createRange();
  end.setStart(node, position?.offset ?? range?.startOffset ?? 0);
  const walker = doc.createTreeWalker(words, NodeFilter.SHOW_TEXT);
  // The words as the editor holds them, and the press's place in them once it is passed.
  let held = "";
  let offset: number | undefined;
  // The element drawn in place of the editor's text the last text counted belongs to, whose
  // other texts count nothing more.
  let counted: Element | null = null;
  for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
    if (text.parentElement?.closest("[data-node-trail]") != null) continue;
    const other = text.parentElement?.closest(HELD_OTHERWISE) ?? null;
    if (other !== null && other === counted) continue;
    if (offset === undefined) {
      if (other?.contains(node) === true || end.comparePoint(text, 0) >= 0) offset = held.length;
      else if (text === node) offset = held.length + end.startOffset;
    }
    counted = other;
    held += (other === null ? text.textContent : other.getAttribute(REFERENCE_WRITTEN)) ?? "";
  }
  const at = offset ?? held.length;
  return { x, y, offset: at, word: pressedWord(held, at) };
}

/** The word at `offset` in `text` - the run of characters that are no space around it - with
 *  the text from the start of the word before it and the text to the end of the word after it,
 *  and how far into the word `offset` is (`CaretAt`); none where `offset` stands in no word. */
function pressedWord(text: string, offset: number): CaretAt["word"] {
  // Where the run of spaces, or of characters that are no space, beside `from` ends: before it
  // where `step` is -1, after it where `step` is 1. One step a character, so a long run of
  // characters that are no space costs no more than its length.
  const across = (from: number, step: -1 | 1, space: boolean): number => {
    let at = from;
    for (;;) {
      const next = step < 0 ? at - 1 : at;
      if (next < 0 || next >= text.length || /\s/.test(text.charAt(next)) !== space) return at;
      at += step;
    }
  };
  const start = across(offset, -1, false);
  const end = across(offset, 1, false);
  if (start === end) return undefined;
  const before = text.slice(across(across(start, -1, true), -1, false), start);
  const after = text.slice(end, across(across(end, 1, true), 1, false));
  return [before, text.slice(start, end), after, offset - start];
}
