import {
  buildReferencePath,
  type DispatchReferenceRoute,
  parseNodeReference,
  shortForm,
} from "../refs/routes";
import { readTextReferences } from "../refs/text-references";
import { textHead } from "../refs/tree-fragment";

/** How many characters of a target's words a link to it draws: a link's title elsewhere is cut
 *  at the same count (`TITLE_HEADLINE_MAX`, `refs/reference-target.ts`). It is written again
 *  here because that module keeps the count to itself and loads the markdown engine with it
 *  (`markdown-engine.ts`), which nothing this module draws needs; `useIssueTree.ts` reads this
 *  one for the citations of every node's words. */
const TITLE_MAX = 60;

/**
 * What a node's words cite, and how a row of the issue page draws it.
 *
 * A row names every reference in its words from the page's one batched read of what the tree's
 * references point at (`GET /api/v1/reference-targets`, `useIssueTree.ts`), which it reads
 * through `useKnownReferenceNames`: an issue by its key and its title, anything else by its
 * target's own words, as every other surface of Dispatch draws a reference. It asks the server
 * nothing of its own. The link pass every other body runs reads one issue summary for each key
 * and one item for each comment or ask a text cites (`RefLink.tsx`, `useTextReferences.ts`),
 * which on an issue of 73 comments was 19 requests of a first load R21 holds to 40.
 *
 * The references are found by the one reader of what a text cites (`text-references.ts`), so a
 * row links exactly what the server indexed for the same words. An issue key links only where
 * the batched read answered that its issue exists: `UTF-16` and `SHA-256` are read as keys by
 * the grammar the web shares with the server, name no issue, and are drawn as the words they
 * are. A short reference the read answered ambiguous links nothing either and is drawn as
 * written (`useKnownAmbiguousReferences`).
 */

/** One reference in a text, where it stands and what it names. */
export interface RowReference {
  readonly end: number;
  /** The reference as the batched read is asked for it. */
  readonly reference: string;
  readonly route: DispatchReferenceRoute;
  readonly start: number;
}

/**
 * Every reference `text` cites, as the batched read is asked for it: each `dispatch://`
 * reference and dashboard address it writes, and each issue key it writes bare, which is asked
 * as that issue's own reference so the read answers whether the issue exists. An address that
 * writes an item id or a block id short is asked as written too: the read resolves the short id
 * in the owner the reference names and answers under the reference as the request wrote it,
 * which is the reference a row links it by (`rowReferences`); where no item or two of them carry
 * the prefix it answers the reference gone, and `ambiguous` where two do.
 */
export function citedReferences(text: string): string[] {
  if (text === "") return [];
  const { needs, references } = readTextReferences(text);
  const cited = new Set<string>();
  for (const found of references) {
    if (found.form === "address" && found.reference !== undefined) cited.add(found.reference);
  }
  for (const key of needs.keys) cited.add(`dispatch://${key}`);
  return [...cited];
}

/** An issue's own reference, which is the form the batched read answers a bare key under. */
const ISSUE_REFERENCE = /^dispatch:\/\/([A-Z][A-Z0-9]*-[1-9][0-9]*)$/u;

/** The issue keys `names` holds an issue for: the keys a row may link where they stand bare. */
export function knownIssueKeys(names: ReadonlyMap<string, string>): string[] {
  const keys: string[] = [];
  for (const reference of names.keys()) {
    const key = ISSUE_REFERENCE.exec(reference)?.[1];
    if (key !== undefined) keys.push(key);
  }
  return keys;
}

/** No reference read as ambiguous: what a caller that hands no set of them reads by. */
const NO_AMBIGUOUS: ReadonlySet<string> = new Set();

/** The references of `text` a row links, in the order it holds them: every address that names
 *  something, and each bare key among `issues`. A `#id` is left as written: its question map is
 *  a read of its own, one per document, which a row does not make. A reference in `ambiguous` -
 *  one the batched read answered that two or more targets carry the short id of, which the page
 *  publishes (`useKnownAmbiguousReferences`) - is left out too, so the row draws it as the text
 *  it is and links nothing, as the tree's chip draws it (`NodeChip.ambiguous`): a link would draw
 *  it as its short form, which names no one target. */
export function rowReferences(
  text: string,
  issues: readonly string[],
  ambiguous: ReadonlySet<string> = NO_AMBIGUOUS
): RowReference[] {
  if (text === "") return [];
  const found: RowReference[] = [];
  for (const one of readTextReferences(text, { issues }).references) {
    if (one.form === "question" || one.reference === undefined || one.route === undefined) continue;
    if (ambiguous.has(one.reference)) continue;
    found.push({ end: one.end, reference: one.reference, route: one.route, start: one.start });
  }
  return found;
}

/**
 * What a row draws a reference as: an issue by its key, then its title once the batched read
 * has brought it; anything else by its target's own words, cut to at most `TITLE_MAX`
 * characters by `textHead` - at a word where one ends in the budget's second half - and drawn
 * with no ellipsis; and until the read names it, or where nothing answers to it, by its short
 * form, as a link of any other body reads until its title resolves (`shortForm`).
 */
export function rowReferenceWords(
  one: Pick<RowReference, "reference" | "route">,
  names: ReadonlyMap<string, string>
): { readonly key: string | undefined; readonly words: string } {
  const name = (names.get(one.reference) ?? "").replace(/\s+/gu, " ").trim();
  if (one.route.kind === "issue") return { key: one.route.key, words: name };
  return {
    key: undefined,
    words: name === "" ? shortForm(one.route) : textHead(name, TITLE_MAX),
  };
}

/** The mark a row's own link carries: the reference it names, by which a later pass gives it
 *  the name the batched read has brought since. */
const ROW_REFERENCE = "data-row-reference";

/** The words a row's own link was made over, as the text wrote them, which a later pass puts
 *  back where the reference is read as ambiguous since (`linkRowReferences`). */
const ROW_WRITTEN = "data-row-written";

/** A link in a node's words that are drawn as stored: the words' own ink over the bullets'
 *  grey, as `styles.css` draws one in a drawn text. */
export const ROW_LINK = "underline decoration-page-bullet underline-offset-[3px]";

/** Writes `anchor`'s words: an issue's key set apart from its title, as a link to an issue reads
 *  everywhere (`RefLink.tsx`). An anchor that already reads them is left as it is: only this
 *  writes a row's link, so its words tell what it holds, and a pass whose names changed nothing
 *  writes nothing. */
function nameAnchor(
  anchor: HTMLAnchorElement,
  one: Pick<RowReference, "reference" | "route">,
  names: ReadonlyMap<string, string>
): void {
  const { key, words } = rowReferenceWords(one, names);
  if (key === undefined) {
    if (anchor.textContent !== words) anchor.textContent = words;
    return;
  }
  const title = words === "" ? "" : ` ${words}`;
  if (anchor.textContent === `${key}${title}`) return;
  const strong = document.createElement("span");
  strong.className = "font-semibold whitespace-nowrap";
  strong.textContent = key;
  anchor.replaceChildren(strong, title);
}

/**
 * The row's own link pass over words a reading painted into `root` (`RowText.tsx`): every
 * reference its text holds outside a link and outside code becomes a link to its target, named
 * from `names`, and a link an earlier pass made takes the name the read has brought since. A
 * reference in `ambiguous` links nothing (`rowReferences`), and a link an earlier pass made for
 * one the read has answered ambiguous since is put back as the words it was made over, so it is
 * drawn as written. Out of the tab order, as everything inside a row is (R9). Run again after
 * every paint and whenever `names` or `ambiguous` changes; it finds nothing to do twice.
 */
export function linkRowReferences(
  root: HTMLElement,
  names: ReadonlyMap<string, string>,
  ambiguous: ReadonlySet<string> = NO_AMBIGUOUS
): void {
  for (const anchor of root.querySelectorAll<HTMLAnchorElement>(`a[${ROW_REFERENCE}]`)) {
    const reference = anchor.getAttribute(ROW_REFERENCE) ?? "";
    if (ambiguous.has(reference)) {
      anchor.replaceWith(anchor.getAttribute(ROW_WRITTEN) ?? reference);
      continue;
    }
    const route = parseNodeReference(reference);
    if (route !== undefined) nameAnchor(anchor, { reference, route }, names);
  }
  const issues = knownIssueKeys(names);
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const texts: Text[] = [];
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node instanceof Text && node.parentElement?.closest("a, code, pre") == null) {
      texts.push(node);
    }
  }
  for (const text of texts) {
    const value = text.data;
    const found = rowReferences(value, issues, ambiguous);
    if (found.length === 0) continue;
    const pieces: (HTMLAnchorElement | string)[] = [];
    let at = 0;
    for (const one of found) {
      pieces.push(value.slice(at, one.start));
      const anchor = document.createElement("a");
      anchor.setAttribute("href", buildReferencePath(one.route));
      anchor.setAttribute(ROW_REFERENCE, one.reference);
      anchor.setAttribute(ROW_WRITTEN, value.slice(one.start, one.end));
      anchor.tabIndex = -1;
      nameAnchor(anchor, one, names);
      pieces.push(anchor);
      at = one.end;
    }
    pieces.push(value.slice(at));
    text.replaceWith(...pieces);
  }
}
