import { type IssueStatus, statusText } from "../features/project/board-model";
import { pageStatusDoneText, pageStatusIdleText, pageStatusProgressText } from "../theme/classes";

/**
 * An issue's status mark: one shape and one colour for each of three groups of status, the
 * prototype's `.st-*` - a tick once the work is done, a half-filled circle while it moves, an
 * empty circle for every other status, a status Dispatch does not know among them. The shapes
 * are data here, so the React component (`StatusGlyph.tsx`) and the DOM builder below, which the
 * document editor's titles and the issue page's rows draw with, draw the same mark.
 *
 * The mark is a shape and not text because the page's typeface holds only the characters its
 * subset lists, ✓ and ○ among them and ◐ not: as text, ◐ is drawn in a face of the reader's own
 * machine, at that face's width. Drawn as a shape, all three are the same on every machine, and
 * before the typeface loads too. It is one em square, so it takes the size of the text around
 * it, and it is an image named by the status's words (`statusText`), unless the words beside it
 * already say the status (`decorative`).
 */

/** The statuses of work that moves: started, and not done. Typed by the status names, so a
 *  misspelt one fails the type check rather than leave an issue idle. */
const MOVING_STATUSES: ReadonlySet<string> = new Set<IssueStatus>([
  "in_progress",
  "needs_review",
  "testing",
]);

/** The status's colour, as the prototype's `.st-*` gives it. */
export function statusTone(status: string): string {
  if (status === "done") return pageStatusDoneText;
  return MOVING_STATUSES.has(status) ? pageStatusProgressText : pageStatusIdleText;
}

/** The shapes `statusGlyph` names, one per group of statuses. */
export type StatusGlyphText = "✓" | "◐" | "○";

/** The status's shape, by the character that names it: a half-filled circle while the work
 *  moves, a tick once it is done, an empty circle before it starts. Draw it with `StatusGlyph`
 *  or `statusGlyphElement`, never as text, since the page's typeface lacks ◐. */
export function statusGlyph(status: string): StatusGlyphText {
  if (status === "done") return "✓";
  return MOVING_STATUSES.has(status) ? "◐" : "○";
}

/** One shape of a mark: an svg element's tag and its attributes, by their names in the DOM. */
export interface GlyphShape {
  readonly tag: "circle" | "path";
  readonly attributes: Readonly<Record<string, string>>;
}

/** The ring the two circles are drawn on. */
const RING: GlyphShape = {
  tag: "circle",
  attributes: {
    cx: "8",
    cy: "8",
    fill: "none",
    r: "5.25",
    stroke: "currentColor",
    "stroke-width": "1.5",
  },
};

/** Each mark's shapes, in the 16 by 16 box `GLYPH_VIEW_BOX` names, drawn in the mark's colour. */
export const GLYPH_SHAPES: Readonly<Record<StatusGlyphText, readonly GlyphShape[]>> = {
  "✓": [
    {
      tag: "path",
      attributes: {
        d: "M3.5 8.5l3 3 6-7",
        fill: "none",
        stroke: "currentColor",
        "stroke-linecap": "round",
        "stroke-linejoin": "round",
        "stroke-width": "1.75",
      },
    },
  ],
  "◐": [
    RING,
    { tag: "path", attributes: { d: "M8 2.75a5.25 5.25 0 0 0 0 10.5z", fill: "currentColor" } },
  ],
  "○": [RING],
};

export const GLYPH_VIEW_BOX = "0 0 16 16";

/** The mark's box: one em square, on the middle of the line, never squeezed by a flex line. */
const GLYPH_BOX = "inline-block size-[1em] shrink-0 align-middle";

/** The classes of `status`'s mark: its box and its group's colour. */
export function statusGlyphClassName(status: string): string {
  return `${GLYPH_BOX} ${statusTone(status)}`;
}

const SVG = "http://www.w3.org/2000/svg";

/** `status`'s mark as an svg element of `document`, for a caller that draws DOM and not React:
 *  the document editor's reference titles and the issue page's rows. The same element, attribute
 *  for attribute, that `StatusGlyph` renders: an image named by the status's words, or, where the
 *  words beside it say the status already (`decorative`), hidden from assistive technology. */
export function statusGlyphElement(
  document: Document,
  status: string,
  { decorative = false }: { readonly decorative?: boolean } = {}
): SVGSVGElement {
  const glyph = statusGlyph(status);
  const svg = document.createElementNS(SVG, "svg");
  if (decorative) svg.setAttribute("aria-hidden", "true");
  else svg.setAttribute("aria-label", statusText(status));
  svg.setAttribute("class", statusGlyphClassName(status));
  svg.setAttribute("data-status-glyph", glyph);
  if (!decorative) svg.setAttribute("role", "img");
  svg.setAttribute("viewBox", GLYPH_VIEW_BOX);
  for (const shape of GLYPH_SHAPES[glyph]) {
    const drawn = document.createElementNS(SVG, shape.tag);
    for (const [name, value] of Object.entries(shape.attributes)) {
      drawn.setAttribute(name, value);
    }
    svg.append(drawn);
  }
  return svg;
}
