/** The attribute the engine's serializer puts on the `<mark>` it draws over what a search
 *  matched (`markdown-serializer.ts`, `displaySchemaOf`), from the highlight mark the server puts
 *  in a snippet's tree (`pmdoc.DisplayHighlightMark`). */
export const SEARCH_HIGHLIGHT_ATTRIBUTE = "data-search-highlight";

/** Every run the server marked as matching a search, as the serializer draws one. */
export const SEARCH_HIGHLIGHT_SELECTOR = `mark[${SEARCH_HIGHLIGHT_ATTRIBUTE}]`;

/** A search mark the browser draws itself, holding `children`: the same element the serializer
 *  draws, so a surface styles every mark of a snippet alike. */
export function searchMark(...children: Node[]): HTMLElement {
  const mark = document.createElement("mark");
  mark.setAttribute(SEARCH_HIGHLIGHT_ATTRIBUTE, "");
  mark.append(...children);
  return mark;
}
