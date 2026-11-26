import { referenceSpans } from "@legion/contracts";

import type { DisplayTree } from "../../../api/types";
import { storeAddressReference } from "../../refs/store-address";
import { NODE_TYPE_NAMES } from "../page-words";
import { storedReading } from "../stored-reading";
import type { Draft } from "../tree-drafts";
import type { TreeDraftContext } from "../tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import {
  addressOf,
  codeOnly,
  codeOnlyRuns,
  constraintRuns,
  type FactParts,
  factText,
  inCodeSpan,
  kindOf,
  type LensChip,
  lensChipWords,
  lensStoreAddress,
  lensTypeOf,
  lineOf,
  markedRuns,
  marksOf,
  namedBy,
  oneLineRuns,
  type PlainWords,
  plainWords,
  setRowLine,
  storeAddressSpans,
  storedFacts,
  storedRowWords,
  storeKindText,
  wholeWords,
  writeRuns,
} from "./lens-drafts";
import { outlineRowPath, type StoreKind } from "./store-link";

/**
 * A store address in a row's words on the issue page (decision 388): `dispatch://lens/<uuid>`,
 * `dispatch://risk/<uuid>`, `dispatch://constraint/<uuid>`, and a finding's
 * `dispatch://lens/<uuid>#b-<id>`. No route names one (`refs/store-address.ts`), so the row's
 * renderer, which names every other reference, draws a bare one as its author wrote it, and a
 * uuid is no word a reader reads. Where the tree read holds the node and a step drafted its row,
 * a row's words read it by the node's name, linked to that row (`?node=`), and by its kind's word
 * otherwise (`lens-drafts.ts`' `storeAddressSpans`); the row's one line, read from those words, reads it alike.
 * `storeWords` is the mark of `tree-read/tree-nodes.ts`'s `treeDraftMarks` that writes them so,
 * and `store-link.ts`'s `placeStoreLinks` makes each name's link one to that row as the row paints
 * its words, once `storeLinkPath` has checked it. Every other address stays as its author wrote
 * it, for the renderer to name (`RowText.tsx`'s `DrawnText`, `node-line.tsx`'s `useNodeLine`),
 * and in a fact line for the plain-text projection (`LensLines.tsx`'s `FactString`).
 *
 * Every fact comes from the tree read; nothing here sends a request. This module imports only
 * types from `tree-read/`, which imports it, so no import cycle closes (`biome.json`'s
 * `noImportCycles`).
 */

/** A node a store address in a row's words names. */
interface StoreNode {
  /** The words a row reads it by (`nameOf`). */
  readonly name: string;
  /** The address of its row (`outlineRowPath`), as a lens chip links it: the `title` of the link
   *  mark a row's words name it in (`nameRuns`). */
  readonly path: string;
  /** Its id in the tree read, which that path names (`?node=`): the id of a fact line's chip of
   *  it (`factWords`). */
  readonly id: string;
}

/** Every node a row's words can name by its store address, by that address as
 *  `storeAddressReference` writes it. */
type StoreIndex = ReadonlyMap<string, StoreNode>;

/**
 * The mark of `tree-read/tree-nodes.ts`'s `treeDraftMarks` that names each store address in
 * `draft`'s words. It runs once every node has been offered to the steps, so it knows every row
 * and its line.
 *
 * - A draft with a reading, the server's or an ask's question's: that reading, named
 *   (`storeAddressesIn`).
 * - A draft with none whose text holds a store address: one paragraph of its text's plain words,
 *   named (`lineRuns`), which its row then draws its words from, the links with them. Its other
 *   addresses stay as written, which the row's renderer names in its words and in its line, as it
 *   names them in a text drawn as stored.
 * - A lens's, a risk's and a constraint's own draft, whose words `lensDrafts` read from their
 *   attributes (`storedRowWords`, `constraintRuns`, `storedFacts`): each attribute that holds a
 *   store address read again with its names (`nameStored`), an author's link to a node among
 *   them, and each fact line that names a node with its parts, which link the node's row.
 *
 * A child issue's title is no markdown and its row draws it as written, so it is left as it is,
 * and its line reads it as written too (`issue-tree.ts`'s `lineWordsRaw`).
 */
export function storeWords(
  draft: Draft,
  node: TreeAnswerNode,
  answer: TreeAnswer,
  context: TreeDraftContext
): void {
  const index = () => storeIndex(answer, context);
  switch (draft.type) {
    case "child_issue":
      return;
    case "constraint":
    case "lens":
    case "risk":
      nameStored(draft, node, draft.type, index);
      return;
    default: {
      if (draft.tree !== undefined) {
        draft.tree = storeAddressesIn(draft.tree, index);
        return;
      }
      const runs = lineRuns(draft.text, index);
      if (runs !== undefined) draft.tree = lineOf(runs);
    }
  }
}

/** The index of each build, one per draft context, shared by every draft: it reads which nodes
 *  were drafted and their lines, which the page's other reads change while the answer stands, as
 *  `lens-drafts.ts`'s `lensPlaces` reads them. */
const storeIndexes = new WeakMap<TreeDraftContext, StoreIndex>();

/** Every node of the answer that `lensDrafts` drafts as a lens, a risk, a constraint or a finding
 *  and that has a row, by its store address (`addressOf`): its name (`nameOf`), the path of its
 *  row and its read id. It is made at the first store address of such a node a run's words name
 *  (`namedRuns`), so a build whose words name none makes none; no draft's words are named before
 *  then, so a name is read from the words as drafted. */
function storeIndex(answer: TreeAnswer, context: TreeDraftContext): StoreIndex {
  const held = storeIndexes.get(context);
  if (held !== undefined) return held;
  const index = new Map<string, StoreNode>();
  for (const node of answer.nodes ?? []) {
    const type = lensTypeOf(node, context);
    if (type === undefined || !context.drafted(node.id)) continue;
    const address = storeAddressReference(addressOf(node, context) ?? "");
    if (address === undefined || index.has(address)) continue;
    index.set(address, {
      id: node.id,
      name: nameOf(node, type, context),
      path: outlineRowPath(context.key, node.id),
    });
  }
  storeIndexes.set(context, index);
  return index;
}

/** The name a row's words read `node` by, whole: a lens by its kind's words as its chip names it
 *  (`lensChipWords`), a risk by its heading's words and a constraint by its words, each as a chip
 *  names it (`wholeWords`), a finding by its row's line (`TreeDraftContext.lineOf`), and the
 *  page's word for its kind where those hold none. A store address in it reads as its kind's word
 *  (`storeKindText`), so no name holds another node's; every other address stays as written, out
 *  of the name's link (`nameRuns`). */
function nameOf(node: TreeAnswerNode, type: StoreKind, context: TreeDraftContext): string {
  if (type === "lens") return lensChipWords(storeKindText(kindOf(node, context))).words;
  const words =
    type === "finding" ? storeKindText(context.lineOf(node.id) ?? "") : wholeWords(node, context);
  return words === "" ? NODE_TYPE_NAMES[type] : words;
}

/** Whether the stored string `raw` can hold a store address: only a `dispatch://` span is one. */
function cites(raw: string | undefined): raw is string {
  return raw?.includes("dispatch://") === true;
}

/**
 * The runs of the one paragraph the stored string `raw` reads as with each store address in it
 * named (`lineOf` makes them a reading), of the very runs `codeOnlyRuns` reads from it: its
 * reading (`storedReading`) named (`storeAddressesIn`), read onto one line (`oneLineRuns`). A run
 * keeps two marks at most. One is code: a run marked as inline code, or inside a code block,
 * reads as written, as a rendered body draws it, so another address in code is not named by its
 * target. The other is a name's link: a bare address's, an autolink's, and an author's own words
 * linked to a node the index holds, words in a code span among them, which keep both marks, but
 * for code words that are the link's own address, which link nothing (`linkedRuns`). Every other
 * mark is left out, so an author's link to a node the index does not hold, or to anything else,
 * reads as its words. Each run of an author's link whose words carry marks,
 * `[the **HTTP** rule](…)`, stays a run of its own with the same link, the same `href` and the
 * same `title`: the row's renderer draws runs that carry one link as one link. A row draws a
 * verdict, a heading and a constraint's words that cite nothing from `codeOnlyRuns`, so the same
 * words that cite a node are drawn as the same one line, the cited words alone changed, with none
 * of the lists and emphasis a parse made in the page would draw (R20). Every other address stays
 * as its author wrote it, for the row's renderer to name. Undefined where the walk names nothing,
 * so a draft keeps the words it has.
 */
function lineRuns(raw: string | undefined, index: () => StoreIndex): DisplayTree[] | undefined {
  if (!cites(raw)) return undefined;
  const reading = storedReading(raw);
  const named = storeAddressesIn(reading, index);
  if (named === reading) return undefined;
  return oneLineRuns(named, (run, inCode) => {
    // Outside a code block the walk leaves no link to a store address but a name's, in a code
    // span too: `linkedRuns` names or drops each other one.
    const link = inCode ? undefined : nameLink(run);
    const code = codeOnly(run, inCode);
    return link === undefined ? code : [...code, link];
  });
}

/** Whether the link marks `one` and `other` link alike: to the same `href`, with the same
 *  `title`. */
function sameLink(one: DisplayTree, other: DisplayTree): boolean {
  const attrs = (link: DisplayTree) =>
    link.attrs as { readonly href?: unknown; readonly title?: unknown } | undefined;
  return attrs(one)?.href === attrs(other)?.href && attrs(one)?.title === attrs(other)?.title;
}

/**
 * `draft`'s words that `lensDrafts` read from the attributes of `node`, of kind `type`, read
 * again with each store address in them named (`lineRuns`): a lens's verdict and a risk's
 * heading (`storedRowWords`) with their named reading (`setRowLine`), so the row links the names,
 * and a constraint's words (`constraintRuns`) so too, one paragraph of each stored string's runs,
 * those of a string that names nothing as `lensDrafts` read them (`codeOnlyRuns`), each only
 * where it names one; and each fact line by the rule `lensDrafts` reads them by (`storedFacts`),
 * as `factWords` reads it: its words, or its parts where they keep a mark or name a node the
 * index holds, so the line links the names. A node none of whose attributes holds a
 * `dispatch://` span (`cites`) names nothing, so it is left as `lensDrafts` drafted it.
 */
function nameStored(
  draft: Draft,
  node: TreeAnswerNode,
  type: "constraint" | "lens" | "risk",
  index: () => StoreIndex
): void {
  if (!Object.values(node.attributes ?? {}).some(cites)) return;
  if (type === "constraint") {
    let named = false;
    const runs = constraintRuns(node, (raw) => {
      const line = lineRuns(raw, index);
      if (line === undefined) return codeOnlyRuns(raw);
      named = true;
      return line;
    });
    if (named) setRowLine(draft, runs);
  } else {
    const runs = lineRuns(storedRowWords(node, type), index);
    if (runs !== undefined) setRowLine(draft, runs);
  }
  if (draft.lens !== undefined) {
    draft.lens = { ...draft.lens, ...storedFacts(node, type, (raw) => factWords(raw, index)) };
  }
}

/**
 * A fact line read from the stored string `raw` with each store address in it named, as
 * `LensFacts` holds it: its reading (`storedReading`) named (`storeAddressesIn`) and read onto
 * one line, each run keeping the marks the plain-text projection reads (`markedRuns`). A run with
 * a name's link, with each run right after it that carries the same link (`sameLink`), is one
 * chip of the node - its store address, its read id, so the chip links its row, and the runs'
 * words with their reading where they keep another mark (`namedBy`) - so an author's link whose
 * words carry marks is one chip; and the runs between are one part of plain words
 * (`plainWords`). A line of no chip and no mark is its words (`factText`), the plain words
 * `lensDrafts` reads where it names nothing.
 */
function factWords(raw: string, index: () => StoreIndex): string | FactParts {
  const runs = markedRuns(storeAddressesIn(storedReading(raw), index));
  // The runs in order: a name's runs that carry one link as one group, and the runs between
  // names as one.
  const groups: DisplayTree[][] = [];
  for (const run of runs) {
    const group = groups.at(-1);
    const link = nameLink(run);
    const before = group === undefined ? undefined : nameLink(group[0]);
    const joins =
      link === undefined || before === undefined ? link === before : sameLink(before, link);
    if (group !== undefined && joins) {
      group.push(run);
    } else {
      groups.push([run]);
    }
  }
  const parts = groups.map((group): PlainWords | LensChip => {
    const link = nameLink(group[0]);
    const address = link === undefined ? undefined : linkedAddress(link);
    const named = address === undefined ? undefined : index().get(address);
    if (address === undefined || named === undefined) return plainWords(group);
    return {
      address,
      id: named.id,
      ...namedBy(group.map((run) => withoutLink(run, nameLink(run)))),
    };
  });
  return parts.some((part) => typeof part !== "string") ? parts : factText(parts);
}

/** The link of a name the run `run` carries: its link to a store address (`linkedAddress`), which
 *  the walk leaves on a name's run alone outside a code block (`storeAddressesIn`); undefined for
 *  any other run. */
function nameLink(run: DisplayTree): DisplayTree | undefined {
  return marksOf(run).find((mark) => linkedAddress(mark) !== undefined);
}

/** The link mark of a name of the node at the store address `address`, whose row's path is
 *  `path`: the address as its `href` and the path as its `title`, the shape `store-link.ts`'s
 *  `storeLinkPath` checks. */
function nameLinkMark(address: string, path: string): DisplayTree {
  return { attrs: { href: address, title: path }, type: "link" };
}

/** `run` with no `link` among its marks: its other marks, or none. */
function withoutLink(run: DisplayTree, link: DisplayTree | undefined): DisplayTree {
  const { marks: _, ...bare } = run;
  const marks = marksOf(run).filter((mark) => mark !== link);
  return marks.length === 0 ? bare : { ...bare, marks };
}

/**
 * `node`, a reading, with each store address in its text runs written as the name of the node it
 * names, in a run that carries a link mark beside the run's own marks (`nameLinkMark`), where
 * `index` holds the node, and as its kind's word with the run's own marks otherwise. The link
 * mark holds the store address as its `href`, as `storeAddressReference` writes it, and the path
 * of the node's row as its `title` (`{ type: "link", attrs: { href, title } }`): the body's
 * reference pass (`refs/RefLink.tsx`'s `collectReferenceAnchors`) reads a link to a path of an
 * issue's page as a reference to the issue, which it draws by the issue's title at the issue's
 * own path, while it leaves alone a link to a store address, which no route names. The row's link
 * pass (`RowText.tsx`'s `DrawnText`) then makes the path the link's address (`store-link.ts`'s
 * `placeStoreLinks`). An address is found as the page finds a reference in text
 * (`storeAddressSpans`). A run that already links to a store address is written as `linkedRuns`
 * writes it. One that links anywhere else keeps its marks, its link among them, and each store
 * address in its words is named as plain words, since a link holds no second link. A run in code
 * - marked as inline code, or inside a code block - keeps its words as its author wrote them, as
 * a rendered body draws them, and only an inline code run's link to a store address is written
 * as `linkedRuns` writes it; a run inside a code block names nothing.
 *
 * The reading is walked run by run (`lens-drafts.ts`'s `writeRuns`), so `node` itself is never
 * changed, and a reading that names no address is answered whole, the very object: a row's
 * reading keeps its identity across reads, which `RowText.tsx`'s `useSameValue` and the paint
 * rely on.
 */
function storeAddressesIn(node: DisplayTree, index: () => StoreIndex): DisplayTree {
  return writeRuns(node, (run, text, inCode) => (inCode ? undefined : namedRuns(run, text, index)));
}

/** The runs the text run `run`, of words `text`, is written as with each store address of a
 *  lens, a risk, a constraint or a finding in its words named: the node's name where `index`
 *  holds it, and its kind's word otherwise, written into the run's own words. Each name is runs
 *  of its own linked to the node's row beside the run's own marks (`nameRuns`), but where the
 *  words stand in a link (`inLink`), which holds no second link: there it is plain words among
 *  them. A run with a link mark is written as `linkedRuns` writes it. A run in inline code names
 *  nothing in its words, which stay as written. Undefined where it names none: it holds no such
 *  address, or stands in inline code with no link to one. The index (`index`) is asked for only
 *  at such an address, here and at a link's (`linkedRuns`). */
function namedRuns(
  run: DisplayTree,
  text: string,
  index: () => StoreIndex,
  inLink = false
): DisplayTree[] | undefined {
  const marks = marksOf(run);
  const link = inLink ? undefined : marks.find((mark) => mark.type === "link");
  if (link !== undefined) return linkedRuns(run, text, link, index);
  if (inCodeSpan(marks) || !cites(text)) return undefined;
  const runs: DisplayTree[] = [];
  // The run's own words since the last name, the kind's words written into them included.
  let words = "";
  let cursor = 0;
  for (const { address, end, start, word } of storeAddressSpans(text)) {
    words += text.slice(cursor, start);
    cursor = end;
    const named = index().get(address);
    if (named === undefined || inLink) {
      words += named?.name ?? word;
      continue;
    }
    if (words !== "") runs.push({ ...run, text: words });
    runs.push(...nameRuns(run, marks, address, named));
    words = "";
  }
  if (cursor === 0) return undefined;
  words += text.slice(cursor);
  if (words !== "") runs.push({ ...run, text: words });
  return runs;
}

/**
 * The runs the name of `named`, the node the store address `address` names, is written as in the
 * run `run`, of marks `marks`: its words linked to the node's row beside those marks - the
 * address as the link's `href`, the path of the row as its `title` - and each other `dispatch://`
 * reference in it a run of its own with those marks alone, since the row's renderer names no
 * reference inside a link's words and names one outside it. A store address stays in the link:
 * the name reads each one of a node as its kind's word (`nameOf`), and no route names any other.
 * Words that are white space alone link nothing, and a name with no words outside its references
 * is one link, so the node's row is still linked.
 */
function nameRuns(
  run: DisplayTree,
  marks: readonly DisplayTree[],
  address: string,
  named: StoreNode
): DisplayTree[] {
  const name = named.name;
  const linked: DisplayTree = {
    ...run,
    marks: [...marks, nameLinkMark(address, named.path)],
    text: name,
  };
  const runs: DisplayTree[] = [];
  let cursor = 0;
  let worded = false;
  // The name's words from the cursor up to `end`, linked where they hold any but white space.
  const wordsUpTo = (end: number): void => {
    const text = name.slice(cursor, end);
    if (text === "") return;
    const blank = text.trim() === "";
    if (!blank) worded = true;
    runs.push(blank ? { ...run, text } : { ...linked, text });
  };
  for (const { start, value } of referenceSpans(name, "dispatch://")) {
    if (storeAddressReference(value) !== undefined) continue;
    wordsUpTo(start);
    runs.push({ ...run, text: value });
    cursor = start + value.length;
  }
  wordsUpTo(name.length);
  return worded ? runs : [linked];
}

/**
 * The runs the text run `run`, of words `text`, is written as where its link mark `link` links.
 * A link to a store address of a lens, a risk, a constraint or a finding (`linkedAddress`): an
 * autolink, whose words are that address, is named as a bare address is (`namedRuns`); any other
 * words are the author's, each store address in them named as plain words, linked as a name is -
 * the address as the link's `href`, the path of the node's row as its `title` - where `index`
 * holds the node, and with no link otherwise, which would go nowhere. Words in code - the run's
 * own marks hold inline code - are linked or unlinked so, and stay as written: no store address
 * in them is named, the link's own address included. Code whose words are that very address
 * links nothing, so every row draws it as written, as the body's reference pass draws an anchor
 * whose words are its `dispatch://` address (`refs/RefLink.tsx`'s `collectReferenceAnchors`). A
 * link to anything else keeps the run's marks, the author's link among them, and only each store
 * address in its words is named, as plain words; undefined where its words hold none or are code,
 * so it stays as its author wrote it. In a reading drawn as a body, each run of one author link
 * keeps its own link beside its other marks, and how the shared renderer draws a marked link
 * (several anchors) is decision 407's, part N's; a one-line row keeps the link alone on each run,
 * which its renderer draws as one link, and a fact line reads them as one chip (`lineRuns`,
 * `factWords`).
 */
function linkedRuns(
  run: DisplayTree,
  text: string,
  link: DisplayTree,
  index: () => StoreIndex
): DisplayTree[] | undefined {
  const address = linkedAddress(link);
  if (address === undefined) return namedRuns(run, text, index, true);
  // The run as it stands with no link: its other marks, or none.
  const unlinked = withoutLink(run, link);
  const marks = marksOf(unlinked);
  if (storeAddressReference(text) === address) {
    return inCodeSpan(marks) ? [unlinked] : namedRuns(unlinked, text, index);
  }
  const named = index().get(address);
  const linked =
    named === undefined
      ? unlinked
      : { ...unlinked, marks: [...marks, nameLinkMark(address, named.path)] };
  return namedRuns(linked, text, index, true) ?? [linked];
}

/** The store address of a lens, a risk, a constraint or a finding the link mark `link` links to,
 *  as `storeAddressReference` writes it; undefined for a mark that is no link, and for a link to
 *  anything else. */
function linkedAddress(link: DisplayTree): string | undefined {
  if (link.type !== "link") return undefined;
  const href = (link.attrs as { readonly href?: unknown } | undefined)?.href;
  return typeof href === "string" ? lensStoreAddress(href) : undefined;
}
