import { referenceSpans } from "@legion/contracts";

import type { DisplayTree } from "../../../api/types";
import { parseDispatchReference, shortForm } from "../../refs/routes";
import { storeAddressReference } from "../../refs/store-address";
import { wordsOf } from "../../refs/tree-fragment";
import { NODE_TYPE_NAMES } from "../page-words";
import { storedReading } from "../stored-reading";
import { storeKindOf } from "./store-link";

/**
 * The one-line words a lens's, a finding's, a risk's and a constraint's own strings are drawn in
 * (`lens-drafts.ts`, `store-words.ts`): a verdict, a heading, a fact line, a constraint's words,
 * a lens kind's words, and the name a row's words read such a node by. The model draws these as
 * strings - its chips, its chain entries and its fact lines - with no projection of the page's to
 * name an address in them, so a bare address is written here as its target's short form, and a
 * store address, which no route names, as the page's word for its node's kind (`storeKindWord`),
 * so no one-line view of a lens draws a raw address (R22). A row's own words are drawn from their
 * reading, whose references the row's link pass names (`RowText.tsx`), and read none of these.
 *
 * This module imports only `store-link.ts` among the lens modules, and no module of
 * `tree-read/`, so no import cycle closes (`biome.json`'s `noImportCycles`).
 */

/**
 * The words of a stored markdown text as a reader reads them, on one line: what a place that
 * draws a stored text as plain words draws, since its marks are markup and no word of it (R21),
 * and a mark after a sentence's end (`today.**`) would hide that end from `firstLineOf`.
 *
 * They are the words of the text's reading (`storedReading`), taken by the walk that takes the
 * words of the server's reading (`wordsOf`), so they are what the server's reading holds for
 * every text that holds none of three things: a container directive, a Proof mark span and a
 * bare address holding markup (`STORED_TEXT_PARSE`). An escape is the character it escapes
 * (`1\.` is `1.`), a code span's content reads as written, a link reads as its words and an
 * autolink in angle brackets as its address. An image, an HTML comment or tag, a table's pipes
 * and rule, a thematic break and the markers of a heading, a quote and a list are no word, and
 * each block's words stand apart from the next block's. White space collapses to single spaces.
 *
 * A bare `dispatch://` address reads as its target's short form, run by run of the reading
 * (`withShortAddresses`). A link already reads as its words, so `[the spec](dispatch://…)` reads
 * `the spec`, and an autolink's words, its address, read short as a bare address does. An address
 * in code reads as its author wrote it, as a rendered body draws it.
 *
 * `holder` is what holds the text, a read's answer: the text's reading is remembered while it
 * lives (`storedReading`). With none, a reading is remembered only where a holder holds it.
 */
export function plainWords(markdown: string, holder?: object): string {
  return wordsOf(withShortAddresses(storedReading(markdown, holder)))
    .replace(/\s+/gu, " ")
    .trim();
}

/**
 * `words` with each bare `dispatch://` address in them written as its target's short form, as a
 * link to it draws it (`shortForm`): `dispatch://CORE-2` reads `CORE-2`, and
 * `dispatch://ACME-1196/spec#b-r1` reads `ACME-1196 spec block`.
 *
 * An address is found as the page finds a reference in text (`referenceSpans`), and only the
 * address itself is written short: what `trimReference` cuts off its end stays where it stood, so
 * `see dispatch://CORE-2.` reads `see CORE-2.`. A store address, which no route names
 * (`refs/store-address.ts`), is written as the page's word for its node's kind
 * (`storeKindWord`): `dispatch://lens/<uuid>` reads `lens`, and a risk's, a constraint's and a
 * finding's read `risk`, `constraint` and `finding`, so no uuid is drawn. The node's own name, and
 * a link to its row, are the tree read's to give, which this pure rule does not read
 * (`store-words.ts`). Any other span that names no target (`parseDispatchReference`), an
 * interaction's or a lens kind's address among them, is left as written.
 */
export function addressesAsShortForms(words: string): string {
  let shortened = "";
  let cursor = 0;
  for (const { start, value } of referenceSpans(words, "dispatch://")) {
    const route = parseDispatchReference(value);
    const address = route === undefined ? storeAddressReference(value) : undefined;
    const word = address === undefined ? undefined : storeKindWord(address);
    if (word !== undefined) {
      shortened += `${words.slice(cursor, start)}${word}`;
      cursor = start + value.length;
      continue;
    }
    if (route === undefined) continue;
    shortened += `${words.slice(cursor, start)}${shortForm(route)}`;
    cursor = start + value.length;
  }
  return shortened + words.slice(cursor);
}

/** The page's word for the kind of node the store address `address` names
 *  (`store-link.ts`' `storeKindOf`): what a place reads such an address by where it knows no
 *  name for the node: `lens`, `risk`, `constraint`, and `finding` for a block of a lens.
 *  Undefined for an interaction's and a lens kind's address. */
export function storeKindWord(address: string): string | undefined {
  const kind = storeKindOf(address);
  return kind === undefined ? undefined : NODE_TYPE_NAMES[kind];
}

/**
 * `node` with each bare address in each of its text runs written as its target's short form
 * (`addressesAsShortForms`), one run at a time before their words are joined, so its words are
 * spaced as the node's are. A run marked as inline code, or inside a code block, stays as its
 * author wrote it, as a rendered body's one line keeps it. `node` itself is never changed, since
 * a reading is shared: the server's by the node that carries it, a stored text's by every caller
 * that reads that text (`storedReading`). A run whose address is written short is a copy, and so
 * is each node above it; a node that holds no address to write short is answered as it is, so a
 * text with none copies nothing.
 */
export function withShortAddresses(node: DisplayTree): DisplayTree {
  return shortAddressesIn(node, false);
}

/** `withShortAddresses`' walk over `node`, which stands inside a code block where `inCode`, so
 *  none of its runs is written short. */
function shortAddressesIn(node: DisplayTree, inCode: boolean): DisplayTree {
  if (typeof node.text === "string") {
    const marks = node.marks;
    const inlineCode =
      Array.isArray(marks) && (marks as DisplayTree[]).some((mark) => mark.type === "inlineCode");
    if (inCode || inlineCode) return node;
    const text = addressesAsShortForms(node.text);
    return text === node.text ? node : { ...node, text };
  }
  const content = node.content;
  if (!Array.isArray(content)) return node;
  const code = inCode || node.type === "code_block";
  const children = content as DisplayTree[];
  // The children up to the first one written short are the node's own; from there on they are
  // copied, each as the walk answered it.
  let changed: DisplayTree[] | undefined;
  for (const [at, child] of children.entries()) {
    const short = shortAddressesIn(child, code);
    if (changed === undefined && short !== child) changed = children.slice(0, at);
    changed?.push(short);
  }
  return changed === undefined ? node : { ...node, content: changed };
}
