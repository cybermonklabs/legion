import type { DisplayTree } from "../../../api/types";
import { referenceSpans } from "../../refs/reference-spans";
import { storeAddressReference } from "../../refs/store-address";
import { wordsOf } from "../../refs/tree-fragment";
import { NODE_TYPE_NAMES } from "../page-words";
import { storedReading } from "../stored-reading";
import { addressesAsShortForms, type Draft, plainWords, storeKindWord } from "../tree-drafts";
import type { TreeDraftContext } from "../tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import {
  addressOf,
  constraintRuns,
  constraintWords,
  type FactParts,
  factText,
  kindOf,
  type LensChip,
  lensChipWords,
  lensStoreAddress,
  lensTypeOf,
  plainRuns,
  storedFacts,
  storedRowWords,
} from "./lens-drafts";
import { outlineRowPath, type StoreKind } from "./store-link";

/**
 * A store address in a row's words on the issue page (decision 388): `dispatch://lens/<uuid>`,
 * `dispatch://risk/<uuid>`, `dispatch://constraint/<uuid>`, and a finding's
 * `dispatch://lens/<uuid>#b-<id>`. No route names one (`refs/store-address.ts`), and a uuid is
 * no word a reader reads, so every one-line words write one as its kind's word
 * (`tree-drafts.ts`'s `addressesAsShortForms`). Where the tree read holds the node and a step
 * drafted its row, a row's words read it by the node's name instead, linked to that row
 * (`?node=`). `storeWords` is the mark of `tree-read/tree-nodes.ts`'s `treeDraftMarks` that
 * writes them so, and `store-link.ts`'s `storeLinkPath` the check a row makes before it follows
 * such a link.
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
   *  mark a row's words name it in (`namedRuns`). */
  readonly path: string;
  /** Its id in the tree read, which that path names (`?node=`): the id of a fact line's chip of
   *  it (`factWords`). */
  readonly id: string;
}

/** Every node a row's words can name by its store address, by that address as
 *  `storeAddressReference` writes it. */
type StoreIndex = ReadonlyMap<string, StoreNode>;

/** The marks of a run that stands in no mark. */
const NO_MARKS: readonly DisplayTree[] = [];

/**
 * The mark of `tree-read/tree-nodes.ts`'s `treeDraftMarks` that names each store address in
 * `draft`'s words. It runs once every node has been offered to the steps, so it knows every row
 * and its line.
 *
 * - A draft with a reading, the server's or an ask's question's: that reading, named
 *   (`storeAddressesIn`).
 * - A draft with none whose text holds a store address: one paragraph of its text's plain words,
 *   named (`lineRuns`), which its row then draws its words from, the links with them. Its other
 *   addresses stay as written, which the row's link pass links as it links a text drawn as
 *   stored (`RowText.tsx`'s `StoredText`), and its line writes short as every line does.
 * - A lens's, a risk's and a constraint's own draft, whose words `lensDrafts` read from their
 *   attributes (`storedRowWords`, `constraintRuns`, `storedFacts`): each attribute that holds a
 *   store address read again with its names (`nameStored`), an author's link to a node among
 *   them, and each fact line that names a node with its parts, which link the node's row.
 *
 * A child issue's title is no markdown and its row draws it as written, so it is left as it is;
 * its line reads a store address as its kind's word, as every line does.
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
      const runs = lineRuns(draft.text, index, (words) => words);
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
 *  (`lensChipWords`), a risk by its heading, a constraint by its words (`constraintWords`), a
 *  finding by its row's line (`TreeDraftContext.lineOf`), and the page's word for its kind where
 *  those hold none. Each is read as the one-line words read it, so an address in it reads as its
 *  kind's word or its short form, and no name holds another node's. */
function nameOf(node: TreeAnswerNode, type: StoreKind, context: TreeDraftContext): string {
  if (type === "lens") return lensChipWords(addressesAsShortForms(kindOf(node, context))).words;
  const words =
    type === "risk"
      ? plainWords(storedRowWords(node, type))
      : type === "constraint"
        ? constraintWords(node)
        : (context.lineOf(node.id) ?? "");
  return words === "" ? NODE_TYPE_NAMES[type] : words;
}

/** Whether the stored string `raw` can hold a store address: only a `dispatch://` span is one. */
function cites(raw: string | undefined): raw is string {
  return raw?.includes("dispatch://") === true;
}

/** A reading of one paragraph that holds `runs`. */
function lineOf(runs: DisplayTree[]): DisplayTree {
  return { content: [{ content: runs, type: "paragraph" }], type: "doc" };
}

/**
 * The runs of the one paragraph the stored string `raw` reads as with each store address in it
 * named (`lineOf` makes them a reading), of the very words `plainWords` reads from it: its
 * reading (`storedReading`) named (`storeAddressesIn`), its text runs in order and spaced as
 * `wordsOf` joins them, white space collapsed. A name's run keeps one mark, its link: a bare
 * address's, an autolink's, and an author's own words linked to a node the index holds, words
 * in a code span among them but for code words that are the link's own address, which link
 * nothing (`linkedRuns`). Every other run is plain text, so an author's link to a node the
 * index does not hold, or to anything else, reads as its words. A run in code - marked as inline
 * code, or inside a code block - reads as written, a name's run in a code span too, and every
 * other run's words are written by `others`. A name's run joins the one before it where both
 * carry the same link - the same `href` and the same `title` - with no words between them, so an
 * author's link whose words carry marks, `[the **HTTP** rule](…)`, is one run. A row draws a
 * verdict, a heading, a fact line and a constraint's words that cite nothing as `plainWords`
 * reads them, each other address written short (`addressesAsShortForms`), so the same words that
 * cite a node are drawn as the same one line, the cited words alone changed, with none of the
 * lists and emphasis a parse made in the page would draw (R20); a text drawn with no reading
 * keeps its other addresses as written, for the row's link pass. Undefined where the walk names
 * nothing, so a draft keeps the words it has.
 */
function lineRuns(
  raw: string | undefined,
  index: () => StoreIndex,
  others: (words: string) => string
): DisplayTree[] | undefined {
  if (!cites(raw)) return undefined;
  const reading = storedReading(raw);
  const named = storeAddressesIn(reading, index);
  if (named === reading) return undefined;
  // The plain words since the last name, each name's run, and whether the next run stands apart
  // from the words before it, as `wordsOf` spaces them.
  const parts: DisplayTree[] = [];
  let words = "";
  let apart = false;
  const read = (node: DisplayTree, inCode: boolean): void => {
    if (typeof node.text === "string") {
      if (apart && (words !== "" || parts.length > 0)) words += " ";
      apart = false;
      if (inCode) {
        words += node.text;
        return;
      }
      const marks = Array.isArray(node.marks) ? (node.marks as DisplayTree[]) : NO_MARKS;
      const text = marks.some((mark) => mark.type === "inlineCode") ? node.text : others(node.text);
      // Outside a code block the walk leaves no link to a store address but a name's, in a code
      // span too: `linkedRuns` names or drops each other one.
      const link = marks.find((mark) => linkedAddress(mark) !== undefined);
      if (link === undefined) {
        words += text;
        return;
      }
      if (words !== "") parts.push({ text: words, type: "text" });
      words = "";
      // A name's run with the same link as the one just before it, no words between, joins it.
      const before = parts.at(-1);
      if (before?.marks !== undefined && sameLink((before.marks as DisplayTree[])[0], link)) {
        parts[parts.length - 1] = { ...before, text: `${before.text as string}${text}` };
        return;
      }
      parts.push({ marks: [link], text, type: "text" });
      return;
    }
    apart = true;
    if (!Array.isArray(node.content)) return;
    const code = inCode || node.type === "code_block";
    for (const child of node.content as DisplayTree[]) read(child, code);
    apart = true;
  };
  read(named, false);
  if (words !== "") parts.push({ text: words, type: "text" });
  // One space wherever white space stands, across the runs' edges too, and none at either end.
  const runs: DisplayTree[] = [];
  let spaced = true;
  for (const part of parts) {
    let text = (part.text as string).replace(/\s+/gu, " ");
    if (spaced && text.startsWith(" ")) text = text.slice(1);
    if (text === "") continue;
    spaced = text.endsWith(" ");
    runs.push({ ...part, text });
  }
  const last = runs.pop();
  const end = typeof last?.text === "string" ? last.text.trimEnd() : "";
  if (last !== undefined && end !== "") runs.push({ ...last, text: end });
  return runs;
}

/** Whether the link marks `one` and `other` link alike: to the same `href`, with the same
 *  `title`. */
function sameLink(one: DisplayTree, other: DisplayTree): boolean {
  const attrs = (link: DisplayTree) =>
    link.attrs as { readonly href?: unknown; readonly title?: unknown } | undefined;
  return attrs(one)?.href === attrs(other)?.href && attrs(one)?.title === attrs(other)?.title;
}

/** `draft`'s words and their reading from `runs`, one paragraph of a lens's verdict, a risk's
 *  heading or a constraint's words read with their names (`lineRuns`), where there are any. */
function nameLine(draft: Draft, runs: DisplayTree[] | undefined): void {
  if (runs === undefined) return;
  const named = lineOf(runs);
  draft.text = wordsOf(named);
  draft.tree = named;
}

/**
 * `draft`'s words that `lensDrafts` read from the attributes of `node`, of kind `type`, read
 * again with each store address in them named (`lineRuns`): a lens's verdict and a risk's
 * heading (`storedRowWords`) with their named reading (`nameLine`), so the row links the names,
 * and a constraint's words (`constraintRuns`) so too, one paragraph of each stored string's runs,
 * those of a string that names nothing its plain words (`plainRuns`), each only where it names
 * one; and each fact line by the rule `lensDrafts` reads them by (`storedFacts`), as `factWords`
 * reads it: its words, which for a line that names nothing are the plain words `lensDrafts`
 * wrote, or its parts where it names a node the index holds, so the line links the names. A
 * node none of whose attributes holds a `dispatch://` span (`cites`) names nothing, so it is
 * left as `lensDrafts` drafted it.
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
      const line = lineRuns(raw, index, addressesAsShortForms);
      if (line === undefined) return plainRuns(raw);
      named = true;
      return line;
    });
    if (named) nameLine(draft, runs);
  } else {
    nameLine(draft, lineRuns(storedRowWords(node, type), index, addressesAsShortForms));
  }
  if (draft.lens !== undefined) {
    draft.lens = { ...draft.lens, ...storedFacts(node, type, (raw) => factWords(raw, index)) };
  }
}

/**
 * A fact line read from the stored string `raw` with each store address in it named, as
 * `LensFacts` holds it: its plain words (`plainWords`) where it names nothing, and else its named
 * reading's words (`lineRuns`), its runs joined (`factText`). Where that reading names a node the
 * index holds, the line is answered as its runs instead, its parts: each run with a name's link a
 * chip of the node - its store address, the run's words and its read id, so the chip links its
 * row - and every other run its text.
 */
function factWords(raw: string, index: () => StoreIndex): string | FactParts {
  const runs = lineRuns(raw, index, addressesAsShortForms);
  if (runs === undefined) return plainWords(raw);
  const parts = runs.map((run): string | LensChip => {
    const text = run.text as string;
    const link = Array.isArray(run.marks) ? (run.marks as DisplayTree[])[0] : undefined;
    const address = link === undefined ? undefined : linkedAddress(link);
    const named = address === undefined ? undefined : index().get(address);
    return address === undefined || named === undefined
      ? text
      : { address, id: named.id, words: text };
  });
  return parts.some((part) => typeof part !== "string") ? parts : factText(parts);
}

/**
 * `node`, a reading or a node inside one, with each store address in its text runs written as
 * the name of the node it names, in a run that carries a link mark beside the run's own marks,
 * where `index` holds the node, and as its kind's word (`storeKindWord`) with the run's own marks
 * otherwise. The link mark holds the store address as its `href`, as `storeAddressReference`
 * writes it, and the path of the node's row as its `title`
 * (`{ type: "link", attrs: { href, title } }`): the body's reference pass (`refs/RefLink.tsx`'s
 * `collectReferenceAnchors`) reads a link to a path of an issue's page as a reference to the
 * issue, which it draws by the issue's title at the issue's own path, while it leaves alone a
 * link to a store address, which no route names. The row's link pass (`RowText.tsx`'s
 * `DrawnText`) then makes the path the link's address (`storeLinkPath`). An address is found as
 * `addressesAsShortForms` finds one (`referenceSpans`), and what `trimReference` cuts off its
 * end stays where it stood. A run that already links to a store address is written as
 * `linkedRuns` writes it. One that links anywhere else keeps its marks, its link among them, and
 * each store address in its words is named as plain words, since a link holds no second link. A
 * run in code - marked as inline code, or inside a code block - keeps its words as its author
 * wrote them, as `withShortAddresses` keeps them, and only an inline code run's link to a store
 * address is written as `linkedRuns` writes it; `inCode` says `node` stands inside a code block,
 * so none of its runs is named.
 *
 * `node` itself is never changed, since a reading is shared (`storedReading`). A run that
 * names an address is copied, and so is each node above it; a node that holds none is answered
 * as it is, so a reading with none is answered whole, the very object: a row's reading keeps its
 * identity across reads, which `RowText.tsx`'s `useSameValue` and the paint rely on.
 */
function storeAddressesIn(node: DisplayTree, index: () => StoreIndex, inCode = false): DisplayTree {
  const content = node.content;
  if (!Array.isArray(content)) return node;
  const code = inCode || node.type === "code_block";
  const children = content as DisplayTree[];
  // The children up to the first one named are the node's own; from there on they are copied,
  // each as the walk answered it, a run that names a node as the runs it was written as.
  let changed: DisplayTree[] | undefined;
  for (const [at, child] of children.entries()) {
    let written: readonly DisplayTree[] | undefined;
    if (typeof child.text === "string") {
      written = code ? undefined : namedRuns(child, child.text, index);
    } else {
      const walked = storeAddressesIn(child, index, code);
      written = walked === child ? undefined : [walked];
    }
    if (written === undefined) {
      changed?.push(child);
      continue;
    }
    changed ??= children.slice(0, at);
    changed.push(...written);
  }
  return changed === undefined ? node : { ...node, content: changed };
}

/** The runs the text run `run`, of words `text`, is written as with each store address of a
 *  lens, a risk, a constraint or a finding in its words named: the node's name where `index`
 *  holds it, and its kind's word otherwise, written into the run's own words. Each name is a run
 *  of its own linked to the node's row beside the run's own marks, but where the words stand in
 *  a link (`inLink`), which holds no second link: there it is plain words among them. A run with
 *  a link mark is written as `linkedRuns` writes it. A run in inline code names nothing in its
 *  words, which stay as written. Undefined where it names none: it holds no such address, or
 *  stands in inline code with no link to one. The index (`index`) is asked for only at such an
 *  address, here and at a link's (`linkedRuns`). */
function namedRuns(
  run: DisplayTree,
  text: string,
  index: () => StoreIndex,
  inLink = false
): DisplayTree[] | undefined {
  const marks = Array.isArray(run.marks) ? (run.marks as DisplayTree[]) : NO_MARKS;
  const code = marks.some((mark) => mark.type === "inlineCode");
  const link = inLink ? undefined : marks.find((mark) => mark.type === "link");
  if (link !== undefined) return linkedRuns(run, text, link, index);
  if (code || !cites(text)) return undefined;
  const runs: DisplayTree[] = [];
  // The run's own words since the last name, the kind's words written into them included.
  let words = "";
  let cursor = 0;
  for (const { start, value } of referenceSpans(text, "dispatch://")) {
    const address = storeAddressReference(value);
    if (address === undefined) continue;
    const word = storeKindWord(address);
    if (word === undefined) continue;
    words += text.slice(cursor, start);
    cursor = start + value.length;
    const named = index().get(address);
    if (named === undefined || inLink) {
      words += named?.name ?? word;
      continue;
    }
    if (words !== "") runs.push({ ...run, text: words });
    runs.push({
      ...run,
      marks: [...marks, { attrs: { href: address, title: named.path }, type: "link" }],
      text: named.name,
    });
    words = "";
  }
  if (cursor === 0) return undefined;
  words += text.slice(cursor);
  if (words !== "") runs.push({ ...run, text: words });
  return runs;
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
 * (several anchors) is decision 407's, part N's; a one-line row joins them (`lineRuns`).
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
  const { marks: _, ...bare } = run;
  const marks = (run.marks as DisplayTree[]).filter((mark) => mark !== link);
  const unlinked = marks.length === 0 ? bare : { ...bare, marks };
  const code = marks.some((mark) => mark.type === "inlineCode");
  if (storeAddressReference(text) === address) {
    return code ? [unlinked] : namedRuns(unlinked, text, index);
  }
  const named = index().get(address);
  const linked =
    named === undefined
      ? unlinked
      : {
          ...unlinked,
          marks: [...marks, { attrs: { href: address, title: named.path }, type: "link" }],
        };
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
