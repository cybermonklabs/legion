import type { ReactNode } from "react";

import { statusText } from "../features/project/board-model";
import {
  GLYPH_BOX,
  GLYPH_SHAPES,
  GLYPH_VIEW_BOX,
  type GlyphShape,
  type StatusGlyphText,
  statusGlyph,
  statusGlyphClassName,
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

/** A status's shape (`status-glyph.ts`), one em square. Given a `status`, the mark a reference to
 *  an issue draws: its group's shape in its group's colour, an image named by the status's words,
 *  the same markup, attribute for attribute, as `statusGlyphElement` builds. Given a `glyph`
 *  alone, the shape it names in its slot's colour, hidden from assistive technology, for a place
 *  whose words beside it say the status already: the issue page's status pill, and the icon of
 *  its Status row. */
export function StatusGlyph(
  props: { readonly status: string } | { readonly glyph: StatusGlyphText }
): ReactNode {
  const glyph = "glyph" in props ? props.glyph : statusGlyph(props.status);
  const shapes = (REACT_SHAPES.get(glyph) ?? []).map(({ attributes, tag: Shape }) => (
    <Shape key={Shape} {...attributes} />
  ));
  return "glyph" in props ? (
    <svg
      aria-hidden="true"
      className={GLYPH_BOX}
      data-status-glyph={glyph}
      viewBox={GLYPH_VIEW_BOX}
    >
      {shapes}
    </svg>
  ) : (
    <svg
      aria-label={statusText(props.status)}
      className={statusGlyphClassName(props.status)}
      data-status-glyph={glyph}
      role="img"
      viewBox={GLYPH_VIEW_BOX}
    >
      {shapes}
    </svg>
  );
}
