import type { ReactNode } from "react";

/**
 * The shapes of Details' rows that the page's typeface does not hold, drawn rather than typed.
 * The page ships its own cut of Inter (`Dispatch Inter`, `styles.css`), and a character outside
 * that cut's `unicode-range`, or inside it where Inter has no glyph for it (⇄, in the Arrows
 * block the range takes whole), is drawn in whatever font the reader's machine holds for it, at
 * that font's width: ☺ is 13.7 px wide in DejaVu Sans and 15.3 px in Arial or Liberation Sans,
 * in a column of 14 px, and on a Mac an emoji face can draw it. Each icon is the shape of the
 * character it stands for, 12 px square, so it sits inside the rows' 14 px glyph column
 * (`IssueMargin.tsx`'s `Row`) and the files list's 16 px one whatever fonts the machine holds,
 * on the text's baseline and in the text's colour. Each is decoration (`aria-hidden`): the
 * row's name, or the file's kind in words, says what it is.
 */

/** One icon: a 12 px square on a 12-unit grid, drawn in `currentColor`, its foot 1 px under the
 *  baseline so that its shapes stand as tall as the row's capitals and level with them. */
function icon(shape: ReactNode): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="inline-block size-3 align-[-1px]"
      fill="none"
      stroke="currentColor"
      strokeLinecap="round"
      strokeLinejoin="round"
      strokeWidth="1.2"
      viewBox="0 0 12 12"
    >
      {shape}
    </svg>
  );
}

/** ▮, an upright filled bar: Priority. */
export const UPRIGHT_BAR = icon(
  <rect fill="currentColor" height="9" stroke="none" width="5" x="3.5" y="1.5" />
);

/** ⊙, a circle with a dot at its centre: Blocks. */
export const CIRCLED_DOT = icon(
  <>
    <circle cx="6" cy="6" r="4.4" />
    <circle cx="6" cy="6" fill="currentColor" r="1.5" stroke="none" />
  </>
);

/** ⌂, a house: Repository. */
export const HOUSE = icon(<path d="M2 10.4V5.6L6 1.8l4 3.8v4.8z" />);

/** ⚑, a filled flag on its pole: Claim. */
export const FLAG = icon(
  <>
    <path d="M2.8 10.6V1.6" />
    <path d="M2.8 1.9h6.6L7.7 4.5l1.7 2.6H2.8z" fill="currentColor" />
  </>
);

/** ⇄, an arrow to the right over an arrow to the left: Pull request. */
export const RIGHT_OVER_LEFT_ARROWS = icon(
  <path d="M2 4.2h8M7.8 2l2.2 2.2-2.2 2.2M10 7.8H2M4.2 5.6 2 7.8l2.2 2.2" />
);

/** ▦, a square ruled into a grid: Components. */
export const GRID_SQUARE = icon(
  <>
    <rect height="8.4" width="8.4" x="1.8" y="1.8" />
    <path d="M4.6 1.8v8.4M7.4 1.8v8.4M1.8 4.6h8.4M1.8 7.4h8.4" strokeWidth="0.8" />
  </>
);

/** ☑, a box with a tick in it: Tasks. */
export const TICKED_BOX = icon(
  <>
    <rect height="8.4" width="8.4" x="1.8" y="1.8" />
    <path d="M3.9 6.1l1.5 1.5 2.8-3.2" />
  </>
);

/** ☺, a smiling face: Opened by. */
export const SMILING_FACE = icon(
  <>
    <circle cx="6" cy="6" r="4.4" />
    <circle cx="4.5" cy="4.9" fill="currentColor" r="0.7" stroke="none" />
    <circle cx="7.5" cy="4.9" fill="currentColor" r="0.7" stroke="none" />
    <path d="M4 7.1a2.3 2.3 0 0 0 4 0" />
  </>
);

/** ▤, a square ruled across: Approval, Files and documents, and a document or file in that
 *  list. */
export const LINED_SQUARE = icon(
  <>
    <rect height="8.4" width="8.4" x="1.8" y="1.8" />
    <path d="M1.8 4.6h8.4M1.8 7.4h8.4" strokeWidth="0.8" />
  </>
);

/** ▣, a square with a filled square inside it: an image in the list of files. */
export const SQUARE_IN_SQUARE = icon(
  <>
    <rect height="8.4" width="8.4" x="1.8" y="1.8" />
    <rect fill="currentColor" height="3.6" stroke="none" width="3.6" x="4.2" y="4.2" />
  </>
);
