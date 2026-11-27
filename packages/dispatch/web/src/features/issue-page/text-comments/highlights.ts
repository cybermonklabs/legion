/**
 * The mark of each thread's quoted words in a row drawn read-only (R2): an underline and a tint,
 * and a stronger tint for the thread in focus, so no fact rests on colour alone. The column
 * (`TextMargin.tsx`) puts the words in the CSS Custom Highlight API's registry (`CSS.highlights`),
 * which styles `text-decoration` and `background-color` and changes no element of the row
 * (`RowText.tsx` changes by no line). A row in its editor draws its own marks, and takes none
 * here. Where the browser has no `CSS.highlights` nothing is marked, and the threads still stand
 * at their words (the plan's Hazards).
 *
 * The page's global stylesheet (`web/src/styles.css`) is outside `issue-page/`, so the rules that
 * style the two highlights are this module's: they are put in the document once, at the first
 * mark (`HIGHLIGHT_RULES`). The base rule takes the colours the document editor marks a comment's
 * words with (`.dispatch-doc .mark-comment` in `styles.css`: a 30% amber-300 tint and an amber-300
 * line). The focus rule's colours are its own: a 55% amber-300 tint and an amber-500 underline, a
 * little stronger than the editor's `.mark-active` (a 50% tint and an amber-400 line).
 */

/** The highlight that holds the quoted words of every thread the column draws. */
export const TEXT_COMMENT_HIGHLIGHT = "text-comment";

/** The highlight that holds, besides, the quoted words of the thread in focus. */
export const TEXT_COMMENT_FOCUS_HIGHLIGHT = "text-comment-focus";

/** The attribute of the style element that holds the two highlights' rules. */
export const HIGHLIGHT_STYLE_ATTRIBUTE = "data-text-comment-highlights";

/** The rules of the two highlights: an underline and a tint, and the focus's stronger tint and
 *  darker underline, which is registered after the first and so wins where both hold a word. */
const HIGHLIGHT_RULES = `::highlight(${TEXT_COMMENT_HIGHLIGHT}) {
  background-color: color-mix(in oklab, var(--color-amber-300) 30%, transparent);
  text-decoration: underline 2px var(--color-amber-300);
}
::highlight(${TEXT_COMMENT_FOCUS_HIGHLIGHT}) {
  background-color: color-mix(in oklab, var(--color-amber-300) 55%, transparent);
  text-decoration: underline 2px var(--color-amber-500);
}`;

/** The words of one thread in a row, and whether that thread is the one in focus. */
export interface ThreadWords {
  readonly focused: boolean;
  readonly range: Range;
}

/** The browser's registry of highlights and its `Highlight`, or undefined where it lacks either:
 *  the DOM's types declare both, and a browser without the API has neither. */
function highlightApi():
  | { readonly make: typeof Highlight; readonly registry: HighlightRegistry }
  | undefined {
  if (typeof CSS === "undefined" || !("highlights" in CSS) || typeof Highlight !== "function") {
    return undefined;
  }
  return { make: Highlight, registry: CSS.highlights };
}

/** Puts the rules of the two highlights in `document`'s head, once. */
function holdRules(document: Document): void {
  if (document.head.querySelector(`style[${HIGHLIGHT_STYLE_ATTRIBUTE}]`) !== null) return;
  const style = document.createElement("style");
  style.setAttribute(HIGHLIGHT_STYLE_ATTRIBUTE, "");
  style.textContent = HIGHLIGHT_RULES;
  document.head.append(style);
}

/**
 * Marks each of `words` with the margin's highlight, and the words of the thread in focus with
 * the focus highlight as well, in place of what either held; answers what takes both marks out
 * again, which leaves a mark a later call set. Does nothing where the browser has no
 * `CSS.highlights`.
 */
export function highlightThreadWords(
  words: readonly ThreadWords[],
  document: Document = globalThis.document
): () => void {
  const api = highlightApi();
  if (api === undefined) return () => {};
  holdRules(document);
  const { make, registry } = api;
  const all = new make(...words.map(({ range }) => range));
  const focus = new make(...words.filter(({ focused }) => focused).map(({ range }) => range));
  registry.set(TEXT_COMMENT_HIGHLIGHT, all);
  registry.set(TEXT_COMMENT_FOCUS_HIGHLIGHT, focus);
  return () => {
    if (registry.get(TEXT_COMMENT_HIGHLIGHT) === all) registry.delete(TEXT_COMMENT_HIGHLIGHT);
    if (registry.get(TEXT_COMMENT_FOCUS_HIGHLIGHT) === focus) {
      registry.delete(TEXT_COMMENT_FOCUS_HIGHLIGHT);
    }
  };
}
