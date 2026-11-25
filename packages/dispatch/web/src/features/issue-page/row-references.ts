import { isPartyRoute } from "../refs/party-references";
import { editedName } from "../refs/post-marks";
import { type DispatchReferenceRoute, parseNodeReference, referenceHref } from "../refs/routes";
import { readTextReferences, type TextReading } from "../refs/text-references";
import { chipName } from "./issue-tree";
import { documentWord } from "./node-row-chip";

/**
 * What a node's words cite, and how a row of the issue page draws it.
 *
 * A row names every reference in its words by the one rule every plain line names one by
 * (`refs/plain-text.tsx`'s `useReferenceNames`): from the page's one batched read of what the
 * tree's references point at (`GET /api/v1/reference-targets`, `useIssueTree.ts`), which the
 * page publishes (`refs/reference-names.tsx`) - an issue by its key and its title, anything else
 * by its target's own words, as every other surface of Dispatch draws a reference - and a party,
 * which that read is never asked for, by its own read. It asks the server nothing for a
 * reference the page reads. The link pass every other body runs reads one issue summary for
 * each key and one item for each comment or ask a text cites (`RefLink.tsx`,
 * `useTextReferences.ts`), which on an issue of 73 comments was 19 requests of a first load R21
 * holds to 40.
 *
 * The references are found by the one reader of what a text cites (`text-references.ts`), so a
 * row links exactly what the server indexed for the same words. An issue key links only where
 * the batched read answered that its issue exists: `UTF-16` and `SHA-256` are read as keys by
 * the grammar the web shares with the server, name no issue, and are drawn as the words they
 * are. A short reference the read answered ambiguous links nothing either and is drawn as
 * written (`KnownReferences.ambiguous`). A reference with no name yet - its target has not
 * answered, or Dispatch answered it has none - reads as the words its author wrote (R8,
 * INFRA-1196 decision 350).
 *
 * A party - an agent's session, a role or a person - is never asked of the batched read: the
 * graph holds no node for one, so the batched read refuses the whole request that names one
 * (`INVALID_REFERENCE`). A row links one from its address alone, as every other sink of a
 * reference does (`referenceHref`): an agent to its live view and a role to its agents list,
 * and a person to nothing, since no page shows one (R5); and it names one as the party's own
 * read names it: an agent by its session's title, a role by its token and a person by their
 * login, each once Dispatch knows it (R5).
 */

/** One reference in a text, where it stands and what it names. */
export interface RowReference {
  readonly end: number;
  /** The address a link to it goes to (`referenceHref`); absent for a person, whom no page
   *  shows (R5), and whom a row names with no link. */
  readonly path?: string;
  /** The reference as the batched read is asked for it. */
  readonly reference: string;
  /** What it names: anything a page shows, and a person. */
  readonly route: DispatchReferenceRoute;
  readonly start: number;
  /** The reference as the text wrote it, which it reads as until it has a name. */
  readonly written: string;
}

/** How many texts' readings the page keeps. A link pass reads one text per painted text node,
 *  so a row reads several: its stored text, each paragraph, list item and inline run of its
 *  drawn words, its answer. The largest issue the page draws, with 86 comments, 11 messages and
 *  2,106 blocks over 56 documents, holds 11,907 distinct such texts, counted from its stored
 *  texts through the API by paragraph, list item and inline span; that is every text of every
 *  document, more than one outline and a second view draw. The cap is about twice that, so a
 *  draw that goes over the rows in the same order each time finds every text kept. */
const READINGS_KEPT = 24_576;

/** How many lists of issue keys one text keeps a reading for: the tree's citations read against
 *  none, and a row against the keys the batched read names, which change as names arrive. */
const KEY_LISTS_KEPT = 4;

/** One text's reading, and the references of it a row links, read once it is first asked. */
interface Reading {
  linked?: readonly RowReference[];
  readonly reading: TextReading;
}

/** Each text's readings, the least recently asked text first, by the small id of the list of
 *  issue keys it was read against (`keyListId`), the least recently asked list first. The text
 *  itself is the key, so an entry holds no copy of it or of the keys. Nothing else a reading
 *  depends on changes while the page is open: the origin is the page's, and the references read
 *  ambiguous are left out after the reading (`rowReferences`), so a new set of them draws no
 *  stale link. */
const readings = new Map<string, Map<number, Reading>>();

/** The id of each list of issue keys a caller has passed, by the list itself, and by its keys
 *  for a new list of the same keys. `0` is no list. */
let listIds = new WeakMap<readonly string[], number>();
const keySetIds = new Map<string, number>();
let lastKeySetId = 0;

/** How many distinct sets of keys are kept an id before they are forgotten: a set the batched
 *  read has moved on from is asked no more, and a new one takes a new id. */
const KEY_SETS_KEPT = 64;

/** Forgets every reading and every id of a list of keys: the unit-test preload, around each test.
 *  No caller passes one array in two tests: the page makes its list of keys anew for each draw,
 *  and each test makes its own. `listIds` starts afresh for a caller that would: its array would
 *  keep the id of a set the reset forgot, while a new array of the same keys took another. */
export function resetRowReferenceReadings(): void {
  readings.clear();
  listIds = new WeakMap();
  keySetIds.clear();
}

/** The small id of the list of keys `issues` holds: the same id for the same keys. */
function keyListId(issues: readonly string[] | undefined): number {
  if (issues === undefined) return 0;
  const known = listIds.get(issues);
  if (known !== undefined) return known;
  const joined = issues.join(" ");
  let id = keySetIds.get(joined);
  if (id === undefined) {
    if (keySetIds.size >= KEY_SETS_KEPT) keySetIds.clear();
    lastKeySetId += 1;
    id = lastKeySetId;
    keySetIds.set(joined, id);
  }
  listIds.set(issues, id);
  return id;
}

/**
 * `text` read by `readTextReferences` against `issues`, the bare keys it may link, once per
 * distinct text and set of keys while the reading is kept. The page read every row's words again
 * on each draw, and encoded each reference's block id again with them, which was about 650 ms of
 * a draw of the outline after a view change on a production-size issue.
 */
function readingOf(text: string, issues: readonly string[] | undefined): Reading {
  const id = keyListId(issues);
  let byKeys = readings.get(text);
  if (byKeys === undefined) {
    byKeys = new Map();
    if (readings.size >= READINGS_KEPT) {
      const oldest = readings.keys().next();
      if (oldest.done !== true) readings.delete(oldest.value);
    }
  } else {
    readings.delete(text);
  }
  readings.set(text, byKeys);
  const held = byKeys.get(id);
  if (held !== undefined) {
    // The list asked is its text's newest, so a fifth list evicts the one asked longest ago.
    byKeys.delete(id);
    byKeys.set(id, held);
    return held;
  }
  const read: Reading = {
    reading: readTextReferences(text, issues === undefined ? {} : { issues }),
  };
  if (byKeys.size >= KEY_LISTS_KEPT) {
    const oldest = byKeys.keys().next();
    if (oldest.done !== true) byKeys.delete(oldest.value);
  }
  byKeys.set(id, read);
  return read;
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
  const { needs, references } = readingOf(text, undefined).reading;
  const cited = new Set<string>();
  for (const found of references) {
    if (found.form !== "address" || found.reference === undefined) continue;
    if (found.route !== undefined && isPartyRoute(found.route)) continue;
    cited.add(found.reference);
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

/** No reference read as edited: what a caller that hands no set of them reads by. */
const NO_EDITED: ReadonlySet<string> = new Set();

/** The references of `text` a row draws by name, in the order it holds them: every address that
 *  names something a page shows (`referenceHref`), which is every node, an agent and a role, a
 *  person, whom no page shows and a row names with no link (R5), and each bare key among
 *  `issues`. A `#id` is left as written: its question map is a read of its own, one per
 *  document, which a row does not make. A reference in `ambiguous` - one the batched read
 *  answered that two or more targets carry the short id of, which the page publishes
 *  (`KnownReferences.ambiguous`) - is left out too, so the row draws it as the text it is and
 *  links nothing, as the tree's chip draws it (`NodeChip.ambiguous`): it names no one target. */
export function rowReferences(
  text: string,
  issues: readonly string[],
  ambiguous: ReadonlySet<string> = NO_AMBIGUOUS
): RowReference[] {
  if (text === "") return [];
  const read = readingOf(text, issues);
  read.linked ??= read.reading.references.flatMap((one) => {
    if (one.form === "question" || one.reference === undefined || one.route === undefined) {
      return [];
    }
    const path = referenceHref(one.route);
    return path === undefined && one.route.kind !== "person"
      ? []
      : [
          {
            end: one.end,
            ...(path === undefined ? {} : { path }),
            reference: one.reference,
            route: one.route,
            start: one.start,
            written: text.slice(one.start, one.end),
          },
        ];
  });
  return read.linked.filter((one) => !ambiguous.has(one.reference));
}

/**
 * What a row draws a reference as, from `names`, the name each reference has as a link reads it
 * (`RowText.tsx`'s names, by `useReferenceNames`): a whole document by the page's word for it,
 * which a chip draws in place of its name (decision 39) and which `names` holds for it instead -
 * the line the page's store projects beside its name (`KnownReferences.lines`, from
 * `tree-drafts.ts`'s `documentWord`) - or, until it has, the word the same rule gives it by its
 * address and `issue`, the issue the row belongs to (`node-row-chip.ts`'s `documentWord`); an
 * issue by its key, then its title; a party by the name its own read gives it; anything else as
 * a chip in place, by its target's own words cut as a tie's chip cuts them (`chipName`), then
 * ` · edited` where the read answered that the target's post was edited (`edited`,
 * `KnownReferences.edited`), after the cut as a link's title takes it (`editedName`), so no cut
 * takes the mark. A reference with no name - its target has not answered yet, or Dispatch
 * answered it has none - reads as the words its author wrote, as a link of any other body reads
 * until its title resolves (R8, INFRA-1196 decision 350, which replaces decision 95's short
 * form).
 */
export function rowReferenceWords(
  one: Pick<RowReference, "reference" | "route" | "written">,
  names: ReadonlyMap<string, string>,
  issue: string | undefined,
  edited: ReadonlySet<string> = NO_EDITED
): { readonly key: string | undefined; readonly words: string } {
  const name = (names.get(one.reference) ?? "").replace(/\s+/gu, " ").trim();
  const word = documentWord(one.route, issue);
  if (word !== undefined) return { key: undefined, words: name === "" ? word : name };
  if (name === "") return { key: undefined, words: one.written };
  if (one.route.kind === "issue") return { key: one.route.key, words: name };
  return {
    key: undefined,
    words: isPartyRoute(one.route)
      ? name
      : editedName(chipName(one.route, name), edited.has(one.reference)),
  };
}

/** The mark a row's own reference carries: the reference it names, by which a later pass gives
 *  it the name it has come to have since. */
const ROW_REFERENCE = "data-row-reference";

/** The words a row's own reference was made over, as the text wrote them, which a later pass
 *  puts back where the reference is read as ambiguous since (`linkRowReferences`). */
const ROW_WRITTEN = "data-row-written";

/** A link the page draws in its words' own ink: underlined in the bullets' grey, as `styles.css`
 *  draws a link in a node's drawn words. */
export const ROW_LINK = "underline decoration-page-bullet underline-offset-[3px]";

/** Writes `element`'s words: an issue's key set apart from its title, as a link to an issue
 *  reads everywhere (`RefLink.tsx`). An element that already reads them is left as it is: only
 *  this writes a row's reference, so its words tell what it holds, and a pass whose names
 *  changed nothing writes nothing. `issue` is the issue the row belongs to. */
function nameReference(
  element: HTMLElement,
  one: Pick<RowReference, "reference" | "route" | "written">,
  names: ReadonlyMap<string, string>,
  issue: string | undefined,
  edited: ReadonlySet<string>
): void {
  const { key, words } = rowReferenceWords(one, names, issue, edited);
  if (key === undefined) {
    if (element.textContent !== words) element.textContent = words;
    return;
  }
  const title = words === "" ? "" : ` ${words}`;
  if (element.textContent === `${key}${title}`) return;
  const strong = document.createElement("span");
  strong.className = "font-semibold whitespace-nowrap";
  strong.textContent = key;
  element.replaceChildren(strong, title);
}

/**
 * The row's own pass over words a reading painted into `root` (`RowText.tsx`): every reference
 * its text holds outside a link and outside code becomes a link to its target - a person, whom
 * no page shows, words with no link (R5) - named from `names` and marked from `edited`
 * (`rowReferenceWords`), and a reference an earlier pass made takes the name it has come to have
 * since. A bare key is read as one only where it is among `issues`, the keys the page's batched
 * read named an issue for. `issue` is the issue the row belongs to, whose spec a chip in place
 * names as the Spec. A reference in `ambiguous` links nothing (`rowReferences`), and one an
 * earlier pass made for a reference the read has answered ambiguous since is put back as the
 * words it was made over, so it is drawn as written. Out of the tab order, as everything inside
 * a row is (R9). Run again after every paint and whenever `names`, `issues`, `issue`,
 * `ambiguous` or `edited` changes; it finds nothing to do twice.
 */
export function linkRowReferences(
  root: HTMLElement,
  names: ReadonlyMap<string, string>,
  issues: readonly string[],
  issue: string | undefined,
  ambiguous: ReadonlySet<string> = NO_AMBIGUOUS,
  edited: ReadonlySet<string> = NO_EDITED
): void {
  for (const element of root.querySelectorAll<HTMLElement>(`[${ROW_REFERENCE}]`)) {
    const reference = element.getAttribute(ROW_REFERENCE) ?? "";
    const written = element.getAttribute(ROW_WRITTEN) ?? reference;
    if (ambiguous.has(reference)) {
      element.replaceWith(written);
      continue;
    }
    const route = parseNodeReference(reference);
    if (route !== undefined) {
      nameReference(element, { reference, route, written }, names, issue, edited);
    }
  }
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const texts: Text[] = [];
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (
      node instanceof Text &&
      node.parentElement?.closest(`a, code, pre, [${ROW_REFERENCE}]`) == null
    ) {
      texts.push(node);
    }
  }
  for (const text of texts) {
    const value = text.data;
    const found = rowReferences(value, issues, ambiguous);
    if (found.length === 0) continue;
    const pieces: (HTMLElement | string)[] = [];
    let at = 0;
    for (const one of found) {
      pieces.push(value.slice(at, one.start));
      const element = document.createElement(one.path === undefined ? "span" : "a");
      if (one.path !== undefined) {
        element.setAttribute("href", one.path);
        element.tabIndex = -1;
      }
      element.setAttribute(ROW_REFERENCE, one.reference);
      element.setAttribute(ROW_WRITTEN, one.written);
      nameReference(element, one, names, issue, edited);
      pieces.push(element);
      at = one.end;
    }
    pieces.push(value.slice(at));
    text.replaceWith(...pieces);
  }
}
