import type { ReactNode } from "react";

import type { StatusGlyphText } from "./issue-status";

/** A status's shape, as `statusGlyph` names it, drawn one em square so it takes the size of its
 *  slot's text. It is a shape and not text because the page's typeface holds only the characters
 *  its subset lists, ✓ and ○ among them and ◐ not: as text, ◐ is drawn in a face of the reader's
 *  own machine, at that face's width. Drawn as a shape, all three are the same on every machine,
 *  and before the typeface loads too. The status word beside it says the same, so it is hidden
 *  from assistive technology, and its colour is the text colour of its slot. */
export function StatusGlyph({ glyph }: { glyph: StatusGlyphText }): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="inline-block size-[1em] align-middle"
      data-status-glyph={glyph}
      viewBox="0 0 16 16"
    >
      {glyph === "✓" ? (
        <path
          d="M3.5 8.5l3 3 6-7"
          fill="none"
          stroke="currentColor"
          strokeLinecap="round"
          strokeLinejoin="round"
          strokeWidth="1.75"
        />
      ) : (
        <>
          <circle cx="8" cy="8" fill="none" r="5.25" stroke="currentColor" strokeWidth="1.5" />
          {glyph === "◐" ? <path d="M8 2.75a5.25 5.25 0 0 0 0 10.5z" fill="currentColor" /> : null}
        </>
      )}
    </svg>
  );
}
