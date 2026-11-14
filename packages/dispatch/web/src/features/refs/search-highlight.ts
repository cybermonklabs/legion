import { createContext } from "react";

/** The attribute the engine's serializer puts on the `<mark>` it draws over what a search
 *  matched (`markdown-serializer.ts`, `displaySchemaOf`), from the highlight mark the server puts
 *  in a snippet's tree (`pmdoc.DisplayHighlightMark`). */
export const SEARCH_HIGHLIGHT_ATTRIBUTE = "data-search-highlight";

/** Every run the server marked as matching a search, as the serializer draws one. */
export const SEARCH_HIGHLIGHT_SELECTOR = `mark[${SEARCH_HIGHLIGHT_ATTRIBUTE}]`;

/** The class a surface styles its search marks with (`MarkdownPreview`'s `highlightClassName`),
 *  for the marks React draws inside a reference (`RefLink`). The surface's own pass over the
 *  painted element sets it on the marks the paint drew (`decorate`); a mark a reference's portal
 *  draws is none of those, so it reads the class here. `""` draws a mark with no class. */
export const SearchHighlightClassName = createContext("");
