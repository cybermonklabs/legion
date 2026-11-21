import type { ReactNode } from "react";

import {
  GLYPH_SHAPES,
  GLYPH_VIEW_BOX,
  type StatusGlyphText,
  statusGlyph,
  statusGlyphClassName,
  statusGlyphLabelling,
} from "./status-glyph";

/** Each mark's shapes with their attributes by the names React takes them by: `stroke-width` as
 *  `strokeWidth`. */
const REACT_SHAPES = Object.fromEntries(
  Object.entries(GLYPH_SHAPES).map(([glyph, shapes]) => [
    glyph,
    shapes.map(({ attributes, tag }) => ({
      attributes: Object.fromEntries(
        Object.entries(attributes).map(([name, value]) => [
          name.replace(/-([a-z])/gu, (_, letter: string) => letter.toUpperCase()),
          value,
        ])
      ),
      tag,
    })),
  ])
) as Record<StatusGlyphText, readonly { attributes: Record<string, string>; tag: "circle" | "path" }[]>;

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
      {REACT_SHAPES[glyph].map(({ attributes, tag: Shape }) => (
        <Shape key={Shape} {...attributes} />
      ))}
    </svg>
  );
}
