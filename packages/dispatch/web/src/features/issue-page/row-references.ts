import { statusGlyphElement } from "../../components/status-glyph";
import { issueReferenceName } from "../refs/issue-reference-name";
import { isPartyRoute } from "../refs/party-references";
import {
  type DispatchReferenceRoute,
  issueOfRoute,
  parseNodeReference,
  referenceHref,
} from "../refs/routes";
import { readTextReferences, type TextReading } from "../refs/text-references";
import { chipName } from "./issue-tree";
import { documentWord } from "./node-row-chip";

/**
 * What a node's words cite, and how a row of the issue page draws it.
 *
 * A row names every reference in its words by the one rule every plain line names one by
 * (`refs/plain-text.tsx`'s `useReferenceNames`): from the page's one batched read of what the
 * tree's references point at (`GET /api/v1/reference-targets`, `useIssueTree.ts`), which the
 * page publishes (`refs/reference-names.tsx`) - an issue by its status mark and its key, with its
 * title in its accessible name, as a rendered body draws one (`RefLink.tsx`), anything else by
 * its target's own words - and a party, which that read is never asked for, by its own read. It
 * asks the server nothing for a reference the page reads. The link pass every other body runs
 * reads one issue summary for
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

/** What a row draws one reference as (`rowReferenceWords`): its words, or, for an issue whose
 *  name and status the page read, its key, drawn after the status mark, apart from its words,
 *  the title, which the row draws in the reference's accessible name alone. */
export type RowReferenceWords =
  | { readonly key: undefined; readonly words: string }
  | { readonly key: string; readonly status: string; readonly words: string };

/**
 * What a row draws a reference as, from `names`, the name each reference has as a link reads it
 * (`RowText.tsx`'s names, by `useReferenceNames`), and `statuses`, each issue's status the same
 * names answer (`ReferenceName.status`): a whole document by the page's word for it, the one the
 * page published (`tree-drafts.ts`'s `documentWord`) or, until it has, the word for a document
 * (`node-row-chip.ts`'s `documentWord`); an issue - its own address or a tab of its page
 * (`issueOfRoute`) - by its status mark and its key, with its key, its status's words and its
 * title its accessible name (`issueReferenceName`, INFRA-4009 R1 and R3), since a row opens no
 * peek; the key, the status and the title each held apart; a
 * party by the name its own read gives it; anything else as a chip in place, by its target's
 * own words cut as a tie's chip cuts them (`chipName`). A reference with no name - its target has
 * not answered yet, or Dispatch answered it has none - reads as the words its author wrote, as a
 * link of any other body reads until its title resolves (R8, INFRA-1196 decision 350, which
 * replaces decision 95's short form), and so does an issue whose status the read did not answer.
 */
export function rowReferenceWords(
  one: Pick<RowReference, "reference" | "route" | "written">,
  names: ReadonlyMap<string, string>,
  statuses: ReadonlyMap<string, string>
): RowReferenceWords {
  const name = (names.get(one.reference) ?? "").replace(/\s+/gu, " ").trim();
  const word = documentWord(one.route);
  if (word !== undefined) return { key: undefined, words: name === "" ? word : name };
  if (name === "") return { key: undefined, words: one.written };
  const key = issueOfRoute(one.route);
  if (key !== undefined) {
    const status = statuses.get(one.reference);
    return status === undefined
      ? { key: undefined, words: one.written }
      : { key, status, words: name };
  }
  return { key: undefined, words: isPartyRoute(one.route) ? name : chipName(one.route, name) };
}

/** The mark a row's own reference carries: the reference it names, by which a later pass gives
 *  it the name it has come to have since. */
const ROW_REFERENCE = "data-row-reference";

/** The words a row's own reference was made over, as the text wrote them, which a later pass
 *  puts back where the reference is read as ambiguous since (`linkRowReferences`). */
const ROW_WRITTEN = "data-row-written";

/** A link in a node's words that are drawn as stored: the words' own ink over the bullets'
 *  grey, as `styles.css` draws one in a drawn text. */
export const ROW_LINK = "underline decoration-page-bullet underline-offset-[3px]";

/** Writes `element`'s words, and for an issue its status mark before its key, on one line, with
 *  its key, its status's words and its title as its accessible name, as a link to an issue reads
 *  everywhere (`RefLink.tsx`). The mark is the one every surface draws (`statusGlyphElement`). An
 *  element that already reads them is left as it is: only this writes a row's reference, so a
 *  mark and a name built from the same key, status and title tell it holds them, and a pass
 *  whose names changed nothing writes nothing. */
function nameReference(
  element: HTMLElement,
  one: Pick<RowReference, "reference" | "route" | "written">,
  names: ReadonlyMap<string, string>,
  statuses: ReadonlyMap<string, string>
): void {
  const drawn = rowReferenceWords(one, names, statuses);
  const marked = element.querySelector("svg[data-status-glyph]") !== null;
  if (drawn.key === undefined) {
    if (marked || element.textContent !== drawn.words) element.textContent = drawn.words;
    element.removeAttribute("aria-label");
    return;
  }
  const label = issueReferenceName(drawn.key, drawn.status, drawn.words);
  if (marked && element.getAttribute("aria-label") === label) return;
  const line = document.createElement("span");
  line.className = "whitespace-nowrap";
  line.append(statusGlyphElement(document, drawn.status), " ", drawn.key);
  element.replaceChildren(line);
  element.setAttribute("aria-label", label);
}

/**
 * The row's own pass over words a reading painted into `root` (`RowText.tsx`): every reference
 * its text holds outside a link and outside code becomes a link to its target - a person, whom
 * no page shows, words with no link (R5) - named from `names` and `statuses`
 * (`rowReferenceWords`), and a reference an earlier pass made takes the name and the status it
 * has come to have since. A bare key is read
 * as one only where it is among `issues`, the keys the page's batched read named an issue for.
 * A reference in `ambiguous` links nothing (`rowReferences`), and one an earlier pass made for a
 * reference the read has answered ambiguous since is put back as the words it was made over, so
 * it is drawn as written. Out of the tab order, as everything inside a row is (R9). Run again
 * after every paint and whenever `names`, `statuses`, `issues` or `ambiguous` changes; it finds
 * nothing to do twice.
 */
export function linkRowReferences(
  root: HTMLElement,
  names: ReadonlyMap<string, string>,
  statuses: ReadonlyMap<string, string>,
  issues: readonly string[],
  ambiguous: ReadonlySet<string> = NO_AMBIGUOUS
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
      nameReference(element, { reference, route, written }, names, statuses);
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
      nameReference(element, one, names, statuses);
      pieces.push(element);
      at = one.end;
    }
    pieces.push(value.slice(at));
    text.replaceWith(...pieces);
  }
}
