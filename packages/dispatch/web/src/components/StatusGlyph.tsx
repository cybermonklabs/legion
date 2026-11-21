import type { ReactNode } from "react";

import {
  GLYPH_SHAPES,
  GLYPH_VIEW_BOX,
  type GlyphShape,
  type StatusGlyphText,
  statusGlyph,
  statusGlyphClassName,
  statusGlyphLabelling,
} from "./status-glyph";

/** A shape's attributes by the names React takes them by: `stroke-width` as `strokeWidth`. */
function reactShapes(shapes: readonly GlyphShape[]): readonly GlyphShape[] {
  return shapes.map(({ attributes, tag }) => ({
    attributes: Object.fromEntries(
      Object.entries(attributes).map(([name, value]) => [
        name.replace(/-([a-z])/gu, (_, letter: string) => letter.toUpperCase()),
        value,
      ])
    ),
    tag,
  }));
}

/** Each mark's shapes as React draws them, read once. */
const REACT_SHAPES = new Map<StatusGlyphText, readonly GlyphShape[]>(
  (Object.keys(GLYPH_SHAPES) as StatusGlyphText[]).map((glyph) => [
    glyph,
    reactShapes(GLYPH_SHAPES[glyph]),
  ])
);

/** `status`'s mark (`status-glyph.ts`): its group's shape in its group's colour, one em square,
 *  an image named by the status's words unless `decorative` says the words beside it already do,
 *  as the issue page's status pill does. */
export function StatusGlyph({
  decorative = false,
  status,
}: {
  decorative?: boolean;
  status: string;
}): ReactNode {
  const glyph = statusGlyph(status);
  return (
    <svg
      {...statusGlyphLabelling(status, decorative)}
      className={statusGlyphClassName(status)}
      data-status-glyph={glyph}
      viewBox={GLYPH_VIEW_BOX}
    >
      {(REACT_SHAPES.get(glyph) ?? []).map(({ attributes, tag: Shape }) => (
        <Shape key={Shape} {...attributes} />
      ))}
    </svg>
  );
}
