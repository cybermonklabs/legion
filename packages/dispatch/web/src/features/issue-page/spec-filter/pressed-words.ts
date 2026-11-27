import type { CaretAt } from "../node-caret";
import { FOCUSABLE } from "../RowText";
import { ROW_WRITTEN } from "../row-references";

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
 * marked by the words' renderer with the editor's text for it (`ROW_WRITTEN`) and counted as that
 * text. So the count reads one mark and no list of shapes, and a kind the words draw in a new way
 * needs only its mark. Which kinds are marked, and with what, is the renderer's to say:
 * `row-references.ts`'s `linkRowReferences` marks each reference it draws by its target's name,
 * and its `markDrawnAtoms` everything else the words draw in place of the editor's text.
 *
 * Every other text counts as drawn: a hard break is a `br` in both, holding no text, and each
 * reading counts it as one character, "\n" (`node-caret.ts`'s `placeAt`); and what the editor
 * draws as a widget - a reference's title, a collaborator's cursor - stands outside its editable
 * text and is not drawn by the view. A mark with no text is an atom: a press inside it counts as
 * at its start, and a press past it goes after it.
 */
const HELD_OTHERWISE = `[${ROW_WRITTEN}]`;

/** Whether `element`, one of `HELD_OTHERWISE`, stands for an atom of the editor's: its mark holds
 *  no text of the editor's. */
function isAtom(element: Element): boolean {
  return element.getAttribute(ROW_WRITTEN) === "";
}

/**
 * Reads the press before the editor replaces the words, as the count of the editor's characters
 * before it: its text, and each hard break as one "\n", not markup or the lines a wrap makes, so a
 * different wrap in the editor does not change the caret's place. What the view draws in place of
 * the editor's text counts as the editor holds it (`HELD_OTHERWISE`), and a press inside one
 * counts as at its start. With the count it reads the word pressed, from the words read the same
 * way, for the editor to find where the count lands in another word (`pressedWord`).
 *
 * An atom holds no text, so the count at the start of the text after one is also the end of the
 * text before it. A press at the start of a drawn text with text counted before it says so
 * (`CaretAt`'s `next`), and the editor puts the caret in that text, where the press was: after
 * the atom, or on the line after a hard break. A hard break's "\n" keeps the count after the
 * break from the count before it, and a hard break at or after the press's place, met before that
 * place is counted, ends the line the press was on: the count is the end of that line, before the
 * break's "\n", with no `next`, and the editor puts the caret there, before the break. Where an
 * atom is the last thing counted before a press there, or before a press past everything counted,
 * the press says it fell past that atom (`CaretAt`'s `pastAtom`), and the editor puts the caret
 * after the atom, on its line, rather than before it; a press inside the atom still counts as at
 * its start. The browser can answer a point past the end of the line a marked element ends with a
 * place inside it - Chromium answers the end of its text - so a place inside one reads as right
 * after it where the point stands past the end of its last line (`pastAtomEnd`): after an atom,
 * and after a reference's words as the editor holds them. An atom drawn with no text - a picture
 * once it has loaded - counts as one too, so the walk reads each element drawn in place of the
 * editor's text before its own texts. A text with no characters - the link pass leaves one where
 * a reference starts or ends a text (`row-references.ts`'s `linkRowReferences`) - holds nothing
 * to press or count, so the walk passes it: no drawn text begins there, and an atom before it is
 * still the last thing counted.
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
  // Whether that place is the start of a drawn text with text counted before it.
  let next = false;
  // Whether that place is past an atom, with no text counted after it.
  let pastAtom = false;
  // The element drawn in place of the editor's text the last thing counted belongs to, whose
  // texts count nothing more.
  let counted: Element | null = null;
  for (let drawn = walker.nextNode(); drawn !== null; drawn = walker.nextNode()) {
    // An element counts only where it is drawn in place of the editor's text, which is where it
    // is one of `HELD_OTHERWISE` itself, so every other element is passed unread, but for a hard
    // break: one "\n", the last thing counted, and the end of the line the press was on where it
    // stands at or after the press's place, met before that place is counted.
    if (drawn instanceof Element && !drawn.matches(HELD_OTHERWISE)) {
      if (drawn.localName === "br") {
        if (offset === undefined && end.comparePoint(drawn, 0) >= 0) {
          offset = held.length;
          pastAtom = counted !== null && isAtom(counted);
        }
        counted = null;
        held += "\n";
      }
      continue;
    }
    // A text with no characters holds nothing to press or count.
    if (drawn instanceof Text && drawn.data === "") continue;
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
  if (offset === undefined && counted !== null && isAtom(counted)) pastAtom = true;
  const at = offset ?? held.length;
  return {
    x,
    y,
    offset: at,
    word: pressedWord(held, at),
    next: next || undefined,
    pastAtom: pastAtom || undefined,
  };
}

/** Whether the point (`x`, `y`) stands past the end of `atom`'s last line: right of its greatest
 *  right where the atom runs left to right, and left of its least left where it runs right to
 *  left. Chromium draws an atom in one box for each run of one direction and gives the boxes of
 *  one line left to right on the screen, so the line's end need not be the last box's. A box is on
 *  the last line where its middle stands at or below the last box's top; a padded atom's boxes
 *  can be taller than its line and overlap the line above, as a link under `.prose` is, so no
 *  edge tells the two lines apart. An atom's box is as tall as its font, not as its line, so
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
