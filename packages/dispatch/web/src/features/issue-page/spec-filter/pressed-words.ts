import type { CaretAt } from "../node-caret";
import { FOCUSABLE, ROW_WRITTEN } from "../RowText";

/** The item containing the focus, if it belongs to this tree. */
export function focusedTreeItem(tree: HTMLElement): HTMLElement | null {
  const item = document.activeElement?.closest<HTMLElement>('[role="treeitem"]') ?? null;
  return tree.contains(item) ? item : null;
}

/** Which statement's words a press hit, excluding their links, controls and chips. */
export function statementWordsAt(target: EventTarget): HTMLElement | null {
  if (!(target instanceof Element)) return null;
  const words = target.closest<HTMLElement>("[data-spec-statement] [data-node-words]");
  const control = target.closest(
    `${FOCUSABLE}, summary, [role="button"], [role="link"], [data-node-trail]`
  );
  return words !== null && !words.contains(control) ? words : null;
}

/**
 * What a statement's drawn words draw in place of the text its editor holds otherwise, each
 * marked by the words' renderer with the editor's text for it (`RowText.tsx`'s `markDrawnAtoms`)
 * and counted as that text. So the count reads one mark and no list of shapes, and a kind the
 * words draw in a new way needs only its mark. A statement holds paragraphs alone (the
 * `requirement` block's content), so these are the inline kinds the two draw differently:
 *
 * - A reference, and a mention of a document, a block or a post: the view's link pass
 *   (`row-references.ts`'s `linkRowReferences`) draws it as a link by its target's name - an
 *   issue's key and title, a chip's words - marked with the words it was written as; the editor
 *   holds those words as text, hidden under a title it draws as a widget the caret cannot enter
 *   (`@legion/proof-editor`'s `reference-decorations.ts`).
 * - Inline HTML (`<name>`): the view draws it by the schema's own `toDOM`, Milkdown's
 *   `span[data-type="html"]` holding its value; a footnote's reference the view draws as the text
 *   its author wrote (`[^label]`), since the statement's own words define no note, and as the
 *   schema's `sup` where a note is defined in them. The editor holds each as an atom ProseMirror
 *   draws `contenteditable="false"`, marked with no text.
 * - A picture of an artifact: the view draws it as a link (`markdown-serializer.ts`'s
 *   `pictureSerializer`) holding the picture, or its caption's link until it loads or where it
 *   fails (`DispatchPicture`); the editor holds it as an atom (`doc/picture-view.ts`), its
 *   picture or its fallback link with no text the caret can enter, marked with no text.
 *
 * Every other text counts as drawn: a hard break is a `br` in both, holding no text, and what
 * the editor draws as a widget - a reference's title, a collaborator's cursor - stands outside
 * its editable text and is not drawn by the view. A mark with no text is an atom: a press inside
 * it counts as at its start, and a press past it goes after it.
 */
const HELD_OTHERWISE = `[${ROW_WRITTEN}]`;

/** Whether `element`, one of `HELD_OTHERWISE`, stands for an atom of the editor's: its mark holds
 *  no text of the editor's. */
function isAtom(element: Element): boolean {
  return element.getAttribute(ROW_WRITTEN) === "";
}

/**
 * Reads the press before the editor replaces the words, as the count of the editor's text
 * characters before it: text, not markup or lines, so a different wrap in the editor does not
 * change the caret's place. What the view draws in place of the editor's text counts as the
 * editor holds it (`HELD_OTHERWISE`), and a press inside one counts as at its start. With the
 * count it reads the word pressed, from the words read the same way, for the editor to find
 * where the count lands in another word (`pressedWord`).
 *
 * A hard break and an atom hold no text, so the count at the start of the text after one is also
 * the end of the text before it. A press at the start of a drawn text with text counted before it
 * says so (`CaretAt`'s `next`), and the editor puts the caret in that text, where the press was:
 * on the line after the break, or after the atom. A press past an atom that no text follows says
 * so too, and the editor puts the caret after that atom, where its block ends; a press inside the
 * atom still counts as at its start. The browser can answer a point past the end of the line a
 * marked element ends with a place inside it - Chromium answers the end of its text - so a place
 * inside one reads as right after it where the point stands past the end of its last line
 * (`pastAtomEnd`): after an atom, and after a reference's words as the editor holds them. An atom
 * drawn with no text - a picture once it has loaded - counts as one too, so the walk reads each
 * element drawn in place of the editor's text before its own texts.
 */
export function pressInWords(words: HTMLElement, x: number, y: number): CaretAt {
  const doc = words.ownerDocument;
  const position = doc.caretPositionFromPoint?.(x, y);
  const range = position == null ? doc.caretRangeFromPoint?.(x, y) : undefined;
  const node = position?.offsetNode ?? range?.startContainer;
  if (node === undefined || !words.contains(node)) return { x, y, offset: 0 };
  const end = doc.createRange();
  end.setStart(node, position?.offset ?? range?.startOffset ?? 0);
  const atom = (node instanceof Element ? node : node.parentElement)?.closest(HELD_OTHERWISE);
  if (atom != null && pastAtomEnd(atom, x, y)) end.setStartAfter(atom);
  const walker = doc.createTreeWalker(words, NodeFilter.SHOW_TEXT | NodeFilter.SHOW_ELEMENT);
  // The words as the editor holds them, and the press's place in them once it is passed.
  let held = "";
  let offset: number | undefined;
  // Whether that place is the start of a drawn text with text counted before it, or past an atom.
  let next = false;
  // The element drawn in place of the editor's text the last thing counted belongs to, whose
  // texts count nothing more.
  let counted: Element | null = null;
  for (let drawn = walker.nextNode(); drawn !== null; drawn = walker.nextNode()) {
    // An element counts only where it is drawn in place of the editor's text, which is where it
    // is one of `HELD_OTHERWISE` itself, so every other element is passed unread.
    if (drawn instanceof Element && !drawn.matches(HELD_OTHERWISE)) continue;
    const element = drawn instanceof Element ? drawn : drawn.parentElement;
    if (element?.closest("[data-node-trail]") != null) continue;
    const other = element?.closest(HELD_OTHERWISE) ?? null;
    if (other !== null && other === counted) continue;
    if (offset === undefined) {
      if (other?.contains(end.startContainer) === true || end.comparePoint(drawn, 0) >= 0) {
        offset = held.length;
        next = other === null && held !== "";
      } else if (drawn === end.startContainer) offset = held.length + end.startOffset;
    }
    counted = other;
    held += (other === null ? drawn.textContent : other.getAttribute(ROW_WRITTEN)) ?? "";
  }
  // A press past everything counted, after an atom - an element drawn in place of no text of the
  // editor's, as a reference is not - goes after that atom.
  if (offset === undefined && counted !== null && isAtom(counted)) next = true;
  const at = offset ?? held.length;
  return { x, y, offset: at, word: pressedWord(held, at), next: next || undefined };
}

/** Whether the point (`x`, `y`) stands past the end of `atom`'s last line: right of its greatest
 *  right where the atom runs left to right, and left of its least left where it runs right to
 *  left. Chromium draws an atom in one box for each run of one direction and gives the boxes of
 *  one line left to right on the screen, so the line's end need not be the last box's. A box is on
 *  the last line where its middle stands between the last box's top and bottom; a padded atom's
 *  boxes can be taller than its line and overlap the line above, as a link under `.prose` is, so
 *  no edge tells the two lines apart. An atom's box is as tall as its font, not as its line, so
 *  `y` only tells the last line from the one before it, the last box above the last line: the two
 *  lines meet half way between that box's foot and the last box's top, and a point at or above
 *  that is on no last line. An atom whose boxes all stand on one line has no line before its
 *  last. Where the browser names the atom neither direction, the point is past no end of it. Read
 *  only for a place the browser answered inside an atom, so no other press lays the words out. */
function pastAtomEnd(atom: Element, x: number, y: number): boolean {
  const boxes = atom.getClientRects();
  const last = boxes[boxes.length - 1];
  if (last === undefined) return false;
  let earlier: DOMRect | undefined;
  let { left, right } = last;
  for (const box of boxes) {
    if (box.top + box.bottom < 2 * last.top) earlier = box;
    else {
      left = Math.min(left, box.left);
      right = Math.max(right, box.right);
    }
  }
  if (earlier !== undefined && y <= (earlier.bottom + last.top) / 2) return false;
  const direction = atom.ownerDocument.defaultView?.getComputedStyle(atom).direction;
  return direction === "rtl" ? x < left : direction === "ltr" && x > right;
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
