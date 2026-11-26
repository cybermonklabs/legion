import { referenceSpans } from "@legion/contracts";

import type { DisplayTree } from "../../../api/types";
import { storeAddressReference } from "../../refs/store-address";
import { wordsOf } from "../../refs/tree-fragment";
import { NODE_TYPE_NAMES, sentenceCase } from "../page-words";
import { storedReading } from "../stored-reading";
import { blank, type Draft, titleReading } from "../tree-drafts";
import type { TreeDraftContext } from "../tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";
import { type StoreKind, storeAddressOf, storeKindOf } from "./store-link";

/**
 * What a row of a lens, a finding, a risk or a constraint carries beyond a `TreeNode`'s own
 * fields, and what a statement a constraint names carries. Every fact comes from the tree read;
 * nothing here sends a request.
 *
 * `lensDrafts` is the step of `tree-read/tree-nodes.ts`'s `treeDraftSteps` that drafts these
 * four kinds, and `lensChips` the mark of its `treeDraftMarks` that puts the chips on the drafts
 * once every node has been drafted; `store-words.ts`'s mark, after it, names these nodes in
 * every row's words by their addresses (`addressOf`) and their words. The step and the chips read
 * the answer in its own order and order nothing: the read's order is the outline's. This module
 * imports only types from `tree-read/` and `issue-tree.ts`, which import it, so no import cycle
 * closes (`biome.json`'s `noImportCycles`).
 */

/** One chip that names another node of the tree: a lens, a check or a constraint, and in a fact
 *  line's parts (`FactParts`) a lens, a risk, a constraint or a finding. */
export interface LensChip {
  /** The target's `dispatch://` address, as its own row carries it, but a check's id in the tree
   *  read where the read gives the check no address (`TreeDraftContext.referenceOf`) or holds no
   *  node of it yet (`unheldChip`). */
  readonly address: string;
  /** The target's words, whole; in a fact line, the words the line names it by. */
  readonly words: string;
  /** The reading `words` are read from, where a run of them keeps a mark the plain-text
   *  projection reads (`namedBy`): code, or a link. `NodeLink.tsx` draws the chip's words through
   *  the projection from it, so an address in code reads as written, and from `words` without it. */
  readonly reading?: DisplayTree;
  /** Set only where `words` are the page's and not the author's: the page's word for a lens,
   *  which names a lens whose kind has no words (`lensChipWords`), and the page's word for the
   *  kind of a node the read so far holds none of (`unheldChip`). */
  readonly chrome?: true;
  /** The target's id in the tree read, where a view draws its row: where a step drafted it; for a
   *  constraint of the issue, which Graph draws, where none did; and for a node the read so far
   *  holds none of, whose row Graph's address reads Graph's pages to reach (both `graphOnly`).
   *  The link's `?node=`. */
  readonly id?: string;
  /** Set where the chip links Graph's row of its target, from every view (`NodeLink.tsx`, R7):
   *  where Graph alone draws that row, for a constraint of the issue that no step drafted from the
   *  answer, as from Conversation's, which holds the issue's constraints as context (R5); and
   *  where the read so far holds no node of the target, since no view's row of it is known until
   *  its page is read, and Graph's address reads Graph's pages until it holds the node
   *  (`unheldChip`). */
  readonly graphOnly?: true;
  /** The target's state and visibility, as the tree read gives them, for the node marks. */
  readonly state?: string;
  readonly visibility?: string;
}

/** One lens of the tree, as the chain a lens draws names it. */
export interface LensPlace {
  readonly kindWords: string;
  /** The whole words of the node it stands under (`wholeWords`), where a step drafted that
   *  node; `LensChain.tsx` draws them through the plain-text projection, from `onReading` where
   *  they have one. */
  readonly onWords?: string;
  /** The reading `onWords` are read from, where a run of them keeps a mark the projection reads
   *  (`namedBy`). */
  readonly onReading?: DisplayTree;
  /** Its id in the tree read, where a step drafted it. */
  readonly id?: string;
}

/** The lens facts of one node. A list with nothing in it draws no line. A fact line - a risk's
 *  early warning and prevention, a standard's reference, where a measurement was taken - is held
 *  once, as `storedFacts` stores it: its words, or its parts (`FactParts`) where a run of them
 *  keeps a mark the plain-text projection reads (`factLine`) or they name a lens, a risk, a
 *  constraint or a finding the tree read holds, a chip of which links that node's row. Either
 *  way `factText` gives its words. */
export interface LensFacts {
  /** A lens: its kind's words, as the `lens_kind` node holds them. */
  readonly kind?: string;
  readonly triggeredBy: readonly LensChip[];
  readonly triggered: readonly LensChip[];
  readonly earlyWarning?: string | FactParts;
  readonly prevention?: string | FactParts;
  readonly preventedBy: readonly LensChip[];
  readonly constrainedBy: readonly LensChip[];
  /** A standard's reference. */
  readonly reference?: string | FactParts;
  /** Where a measurement was taken. */
  readonly takenWhere?: string | FactParts;
  /** A lens: every lens of the tree, by its address. */
  readonly lenses?: ReadonlyMap<string, LensPlace>;
}

/** Words a lens line draws through the plain-text projection: their text where no run of them
 *  keeps a mark the projection reads, and else their reading, one paragraph of those runs
 *  (`plainWords`), so the projection reads an address in code as written, as a row's body does. */
export type PlainWords = string | DisplayTree;

/** A fact line's words in parts: plain words (`PlainWords`), and a chip for each node it names. */
export type FactParts = readonly (PlainWords | LensChip)[];

/** Whether `part`, one of a fact line's parts, is a chip of a node the line names rather than
 *  its plain words. */
export function isChip(part: PlainWords | LensChip): part is LensChip {
  return typeof part !== "string" && "address" in part;
}

/** The text of plain words (`PlainWords`): the words themselves, or their reading's words. */
export function plainText(words: PlainWords): string {
  return typeof words === "string" ? words : wordsOf(words);
}

/** The reading the plain-text projection reads plain words (`PlainWords`) from: their own, and
 *  for words that have none, their text as one text run (`titleReading`). */
export function wordsReading(words: PlainWords): DisplayTree {
  return typeof words === "string" ? titleReading(words) : words;
}

/** A fact line's words (`LensFacts`), the one rule that joins them: a string is its own words,
 *  and parts are joined with nothing between them, each part of plain words as their text
 *  (`plainText`) and each chip as its `words`, as `LensLines.tsx`'s `FactWords` draws them. */
export function factText(line: string | FactParts): string {
  return typeof line === "string"
    ? line
    : line.map((part) => (isChip(part) ? part.words : plainText(part))).join("");
}

/** A kind's words with their first letter in capitals, as a lens's tag and a chip that names a
 *  lens draw them. A chain entry and a menu item draw the words as the node holds them. */
export function kindWords(words: string): string {
  return sentenceCase(words);
}

/** The words of a chip that names a lens: its kind's words `kind`, capitalised and whole, as its
 *  tag draws them; where `kind` holds none, the page's word for a lens, which the chip marks as
 *  the page's (`chrome`). The outline's trigger chips and the chain's last chip are named alike. */
export function lensChipWords(kind: string | undefined): Pick<LensChip, "chrome" | "words"> {
  return filled(kind)
    ? { words: kindWords(kind) }
    : { chrome: true, words: kindWords(NODE_TYPE_NAMES.lens) };
}

/** Whether a stored string, or a fact line, has words to draw (`factText`). */
export function filled<Line extends string | FactParts>(line: Line | undefined): line is Line {
  return line !== undefined && factText(line) !== "";
}

/** The chip lists of a draft before `lensChips` fills them. */
const NO_CHIPS: Pick<LensFacts, "constrainedBy" | "preventedBy" | "triggered" | "triggeredBy"> = {
  constrainedBy: [],
  preventedBy: [],
  triggered: [],
  triggeredBy: [],
};

/** The kinds of node whose words a lens on one of their blocks reads: the server stores such a
 *  lens under the item's block, which no step drafts. */
const ITEMS: Readonly<Record<string, true>> = { ask: true, comment: true, message: true };

/** A fact line of a lens row whose words one stored attribute holds (`STORED_WORDS`). */
type StoredFact = "earlyWarning" | "prevention" | "reference" | "takenWhere";

/** One fact line of a lens row whose words one stored attribute holds. */
interface StoredWords {
  /** The kind of node that holds it. */
  readonly type: StoreKind;
  /** The attribute's name, as the tree read gives it. */
  readonly attribute: string;
  /** The fact line its words are, by its `LensFacts` member. */
  readonly fact: StoredFact;
  /** Whether a node of `type` draws the line; every one does where this is absent. */
  readonly drawn?: (node: TreeAnswerNode) => boolean;
}

/**
 * The fact lines whose words one stored attribute holds, each under the kind of node that holds
 * it, by its `LensFacts` member and the rule a node draws it by: a risk's early warning and
 * prevention; a standard's reference, and where a measurement was taken. `lensDrafts` reads each
 * through `factLine` (`storedFacts`), and `store-words.ts` through its reader that names each
 * store address, so an attribute is named here alone, as a row's own words' is (`ROW_WORDS`).
 */
const STORED_WORDS: readonly StoredWords[] = [
  { attribute: "early_warning", fact: "earlyWarning", type: "risk" },
  { attribute: "prevention", fact: "prevention", type: "risk" },
  {
    attribute: "reference",
    drawn: (constraint) => (constraint.attributes?.standard ?? "") !== "",
    fact: "reference",
    type: "constraint",
  },
  {
    attribute: "taken_where",
    drawn: (constraint) => (constraint.attributes?.standard ?? "") === "",
    fact: "takenWhere",
    type: "constraint",
  },
];

/** The attribute that holds the stored words the row of a lens and of a risk draws
 *  (`storedRowWords`): a lens's verdict and a risk's heading. A finding's words are its text, and
 *  a constraint's `constraintRuns`'. */
const ROW_WORDS: Readonly<Record<"lens" | "risk", string>> = { lens: "verdict", risk: "heading" };

/** The stored string the row of `node`, a lens or a risk as `type` says, draws its words from
 *  (`ROW_WORDS`): a lens's verdict and a risk's heading, `""` where the node holds none. */
export function storedRowWords(node: TreeAnswerNode, type: "lens" | "risk"): string {
  return node.attributes?.[ROW_WORDS[type]] ?? "";
}

/** The fact lines of `node`, of kind `type`, whose words one stored attribute holds
 *  (`STORED_WORDS`): each one its rule draws, as `read` answers it - its words, or its parts where
 *  they keep a mark or name a node - where it has words to draw (`filled`). */
export function storedFacts(
  node: TreeAnswerNode,
  type: StoreKind,
  read: (markdown: string) => string | FactParts = factLine
): Partial<Pick<LensFacts, StoredFact>> {
  const facts: { [fact in StoredFact]?: string | FactParts } = {};
  for (const stored of STORED_WORDS) {
    if (stored.type !== type || stored.drawn?.(node) === false) continue;
    const line = read(node.attributes?.[stored.attribute] ?? "");
    if (filled(line)) facts[stored.fact] = line;
  }
  return facts;
}

/**
 * The draft of a lens, a finding, a risk or a constraint of the issue; undefined for any other
 * node, which the tree read's other steps are asked for. A catalogue node drafts no row: a
 * `lens_kind`, and a constraint whose parent is not the issue. Nor does a constraint the read
 * marks as context, as Conversation's answer holds each of the issue's (R5), but where a lens of
 * the answer stands under it: that answer holds it as the lens's ancestor (`issueConstraint`).
 *
 * It stands under its parent's row (`parentsOf`), with its address as its reference
 * (`addressOf`), and its author and time as the other steps give a turn's. Its words: a lens's
 * verdict, or none, which its row draws as `LENS_WORDS.no_verdict`; a finding's text and its
 * reading, as the other steps read a node's; a risk's heading; a constraint's words
 * (`constraintWords`); and its fact lines (`storedFacts`). Each stored string is read through
 * `storedWords`, and a fact line through `factLine`, but a kind's words, which are drawn as the
 * `lens_kind` node holds them. Its lens facts carry no chip yet: `lensChips` adds them.
 */
export function lensDrafts(
  node: TreeAnswerNode,
  answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  const type = lensTypeOf(node, context);
  if (type === undefined || (type === "constraint" && !issueConstraint(node, answer, context))) {
    return undefined;
  }
  const parents = parentsOf(node, context);
  if (parents === undefined) return undefined;
  const draft = blank(context.treeIdOf(node.id), type, 0);
  draft.parents = parents;
  if (node.made_at !== null) draft.at = node.made_at;
  // The read lists every author of the node's first version, the one who made it first ahead of
  // the rest, and a row draws the first; none where the list is empty or null.
  const author = node.author?.[0];
  if (author !== undefined) draft.author = author;
  const address = addressOf(node, context);
  if (address !== undefined) draft.reference = address;
  switch (type) {
    case "lens": {
      draft.text = storedWords(storedRowWords(node, type));
      const kind = kindOf(node, context);
      draft.lens = kind === "" ? { ...NO_CHIPS } : { ...NO_CHIPS, kind };
      break;
    }
    case "finding":
      draft.text = node.text;
      if (node.text_tree !== undefined) draft.tree = node.text_tree;
      draft.lens = { ...NO_CHIPS };
      break;
    case "risk":
      draft.text = storedWords(storedRowWords(node, type));
      draft.lens = { ...NO_CHIPS, ...storedFacts(node, type) };
      break;
    case "constraint":
      draft.text = constraintWords(node);
      draft.lens = { ...NO_CHIPS, ...storedFacts(node, type) };
      break;
  }
  return draft;
}

/**
 * The chips of `draft`, once every node has been offered to the steps, from the answer's tuples
 * in the answer's order (`endChips`). Each chip of a node the answer holds carries its address,
 * its state and its visibility, and its id in the tree read only where a view draws the target's
 * row, which makes it a link to that row (`?node=`): where a step drafted the target, and for a
 * constraint of the issue, which Graph draws (`constraintChip`). Where the read so far holds no
 * node at a chip's far end, the chip is drawn at once by its kind's word, linked by the far end's
 * read id to Graph's row (`unheldChip`): the server answers a tuple with the page that holds its
 * `from` node, wherever the whole answer holds its `to` node, so that node stands on a page not
 * yet read. A far end is missing only at a tuple's `to`, so a chip of `triggered`, whose far end
 * is a tuple's `from`, always names a node the read holds. Once that page is read, the tree is
 * built again from the answer that holds the node, and the chip is that node's.
 *
 * - A lens: a chip for each lens its `triggered_by` tuples name (`triggeredBy`) and for each lens
 *   whose `triggered_by` names it (`triggered`), each named by its kind's words as its tag is
 *   (`lensChip`); and `lenses`, every lens of the answer by its address, one map shared by every
 *   lens.
 * - A risk: a chip for each check its `prevented_by` tuples name, named by that check's whole
 *   words and, where they keep a mark, their reading (`wholeRuns`, `namedBy`).
 * - Any draft with a `constrained_by` tuple, a statement above all: a chip for each constraint it
 *   names, by the constraint's whole words (`constraintChip`). One of the issue's links its row,
 *   in Graph where only Graph draws it; a catalogue constraint is no row, so its chip is no link.
 *   A draft of another step gains lens facts only where it has such a chip.
 */
export function lensChips(
  draft: Draft,
  node: TreeAnswerNode,
  answer: TreeAnswer,
  context: TreeDraftContext
): void {
  const constrainedBy = endChips(node, "constrained_by", "to", context, (target) => {
    const address = target.supertag === "constraint" ? addressOf(target, context) : undefined;
    return address === undefined ? undefined : constraintChip(target, address, context);
  });
  if (draft.lens === undefined && constrainedBy.length === 0) return;
  const facts: LensFacts = { ...(draft.lens ?? NO_CHIPS), constrainedBy };
  if (draft.type === "lens") {
    draft.lens = {
      ...facts,
      lenses: lensPlaces(answer, context),
      triggered: endChips(node, "triggered_by", "from", context, lensChip),
      triggeredBy: endChips(node, "triggered_by", "to", context, lensChip),
    };
  } else if (draft.type === "risk") {
    draft.lens = {
      ...facts,
      preventedBy: endChips(node, "prevented_by", "to", context, (check) =>
        chipTo(
          check,
          context.referenceOf(check) ?? check.id,
          namedBy(wholeRuns(check, context)),
          context
        )
      ),
    };
  } else {
    draft.lens = facts;
  }
}

/** Which of the four kinds `node` is: a lens, a risk and a constraint by their supertags, and a
 *  finding a block whose read parent is a lens, since the server gives a finding no kind of its
 *  own (`graphstore/reasoning.go`). Its supertag is read as `block` (`8bdd1621fb`) and as
 *  `lens_block` (`5c85ffdc1b`), the two spellings of its kind in the model. */
export function lensTypeOf(node: TreeAnswerNode, context: TreeDraftContext): StoreKind | undefined {
  switch (node.supertag) {
    case "constraint":
    case "lens":
    case "risk":
      return node.supertag;
    case "block":
    case "lens_block":
      return node.parent !== null && context.nodeOf(node.parent.id)?.supertag === "lens"
        ? "finding"
        : undefined;
    default:
      return undefined;
  }
}

/** Whether `constraint` is one of the issue's own that draws a row: under the issue
 *  (`underIssue`), and not context the read brings in, but where a lens of `answer` stands under
 *  it (`lensParents`): Conversation's answer holds the issue's constraints as context, and one a
 *  lens stands under as that lens's ancestor, which it draws whole (R5). A catalogue constraint
 *  a tuple names is context too, and stands under no issue. */
function issueConstraint(
  constraint: TreeAnswerNode,
  answer: TreeAnswer,
  context: TreeDraftContext
): boolean {
  return (
    underIssue(constraint, context) &&
    (constraint.role !== "context" || lensParents(answer).has(constraint.id))
  );
}

/** Whether the read parent of `node` is the issue. */
function underIssue(node: TreeAnswerNode, context: TreeDraftContext): boolean {
  return node.parent !== null && context.treeIdOf(node.parent.id) === context.key;
}

/** The read id of each node a lens of the answer stands under, one set per answer. */
const lensParentSets = new WeakMap<TreeAnswer, ReadonlySet<string>>();

function lensParents(answer: TreeAnswer): ReadonlySet<string> {
  const held = lensParentSets.get(answer);
  if (held !== undefined) return held;
  const parents = new Set<string>();
  for (const node of answer.nodes ?? []) {
    if (node.supertag === "lens" && node.parent !== null) parents.add(node.parent.id);
  }
  lensParentSets.set(answer, parents);
  return parents;
}

/**
 * The `dispatch://` address of a lens, a finding, a risk or a constraint of the tree read, which
 * its row's Copy reference copies and a chip naming it carries: `dispatch://lens/<uuid>`, a
 * finding's its lens's with its block id (`dispatch://lens/<uuid>#b-<block id>`),
 * `dispatch://risk/<uuid>` and `dispatch://constraint/<uuid>`; undefined for any other node.
 *
 * The tree read answers no address per node, so this builds one from the node's read id
 * (`store-link.ts`'s `storeAddressOf`) until the server serves that read: a node's read id is its
 * store id, bare or after its kind (`lens/<uuid>`), and a finding's is its lens's and its block
 * id after a `#` (`lens/<uuid>#<block id>`, as the server writes one), or its block id alone.
 */
export function addressOf(node: TreeAnswerNode, context: TreeDraftContext): string | undefined {
  const type = lensTypeOf(node, context);
  return type === undefined ? undefined : storeAddressOf(type, node.id, node.parent?.id);
}

/** The store address `value` names, as `storeAddressReference` writes it, where that address is
 *  a lens's, a risk's, a constraint's or a finding's (`storeKindOf`); undefined otherwise. */
export function lensStoreAddress(value: string): string | undefined {
  const address = storeAddressReference(value);
  return address === undefined || storeKindOf(address) === undefined ? undefined : address;
}

/** The page's word for the kind of node the store address `address` names (`storeKindOf`): what
 *  a place reads such an address by where it knows no name for the node: `lens`, `risk`,
 *  `constraint`, and `finding` for a block of a lens. Undefined for an interaction's and a lens
 *  kind's address. */
export function storeKindWord(address: string): string | undefined {
  const kind = storeKindOf(address);
  return kind === undefined ? undefined : NODE_TYPE_NAMES[kind];
}

/** `words` with each store address of a lens, a risk, a constraint or a finding in them written
 *  as the page's word for its kind (`storeKindWord`), so no uuid is drawn: the words of one text
 *  run (`storeKindWords`), and a lens kind's words and a finding's line, which name a node in a
 *  row's words (`store-words.ts`) and hold no other node's. An address is found as the page finds
 *  a reference in text (`referenceSpans`), and what `trimReference` cuts off its end stays where
 *  it stood. Every other address stays as its author wrote it. */
export function storeKindText(words: string): string {
  let written = "";
  let cursor = 0;
  for (const { start, value } of referenceSpans(words, "dispatch://")) {
    const address = storeAddressReference(value);
    const word = address === undefined ? undefined : storeKindWord(address);
    if (word === undefined) continue;
    written += `${words.slice(cursor, start)}${word}`;
    cursor = start + value.length;
  }
  return written + words.slice(cursor);
}

/**
 * `node`, a reading or a node inside one, with each store address of a lens, a risk, a
 * constraint or a finding in its text runs written as the page's word for its kind
 * (`storeKindText`), run by run, before its words are read: the words of a chip and of a chain
 * entry (`wholeWords`), which are drawn as plain words that link nothing. A run in code - marked
 * as inline code, or inside a code block, which `inCode` says `node` stands in - keeps its
 * address as its author wrote it, as a rendered body draws it.
 *
 * `node` itself is never changed, since a reading is shared (`storedReading`): a run that names
 * an address is copied, and so is each node above it; a node that holds none is answered as it
 * is.
 */
function storeKindWords(node: DisplayTree, inCode = false): DisplayTree {
  if (typeof node.text === "string") {
    const marks = Array.isArray(node.marks) ? (node.marks as DisplayTree[]) : [];
    if (inCode || marks.some((mark) => mark.type === "inlineCode")) return node;
    const text = storeKindText(node.text);
    return text === node.text ? node : { ...node, text };
  }
  const content = node.content;
  if (!Array.isArray(content)) return node;
  const code = inCode || node.type === "code_block";
  const children = content as DisplayTree[];
  // The children up to the first one written anew are the node's own; from there on they are
  // copied, each as the walk answered it.
  let changed: DisplayTree[] | undefined;
  for (const [at, child] of children.entries()) {
    const written = storeKindWords(child, code);
    if (changed === undefined && written !== child) changed = children.slice(0, at);
    changed?.push(written);
  }
  return changed === undefined ? node : { ...node, content: changed };
}

/**
 * Where a draft of `node` stands, by its read parent: `[]` under the issue, where
 * `treeDraftsFrom` places it; under the parent's row where a step drafted the parent; under the
 * ask, the comment or the message the parent is a block of, where no step drafted that block;
 * and nowhere else - undefined, so neither the node nor anything under it is drawn.
 */
function parentsOf(node: TreeAnswerNode, context: TreeDraftContext): string[] | undefined {
  const parent = node.parent?.id;
  if (parent === undefined) return undefined;
  if (context.treeIdOf(parent) === context.key) return [];
  if (context.drafted(parent)) return [context.treeIdOf(parent)];
  const item = itemAbove(parent, context);
  return item === undefined ? undefined : [context.treeIdOf(item)];
}

/** The read id of the drafted ask, comment or message the undrafted node `readId` is a block
 *  of, walking up its parents; undefined where the walk reaches a drafted node, the issue, an
 *  undrafted item or nothing first. */
function itemAbove(readId: string, context: TreeDraftContext): string | undefined {
  const passed = new Set<string>();
  let above: string | null = readId;
  while (above !== null && !passed.has(above)) {
    passed.add(above);
    const node = context.nodeOf(above);
    if (node === undefined) return undefined;
    if (ITEMS[node.supertag] === true) return context.drafted(above) ? above : undefined;
    if (context.drafted(above) || context.treeIdOf(above) === context.key) return undefined;
    above = node.parent?.id ?? null;
  }
  return undefined;
}

/** The words of the lens kind `lens`'s `has_kind` tuple names, as that `lens_kind` node holds
 *  them: its `words`, else its text; `""` where the read holds none. */
export function kindOf(lens: TreeAnswerNode, context: TreeDraftContext): string {
  for (const tuple of context.tuplesFrom(lens.id)) {
    const kind = tuple.kind === "has_kind" ? context.nodeOf(tuple.to.id) : undefined;
    if (kind === undefined) continue;
    const words = kind.attributes?.words ?? "";
    return words === "" ? kind.text : words;
  }
  return "";
}

/** A constraint's words: the text of its runs (`constraintRuns`), each stored string read to its
 *  plain words. */
function constraintWords(constraint: TreeAnswerNode): string {
  return wordsOf({ content: constraintRuns(constraint), type: "paragraph" });
}

/** The whole words `node` has, which a chip that names it and the chain entry of the row a lens
 *  stands under draw, with nothing cut: the text of its runs (`wholeRuns`), `""` where the node
 *  has none. */
export function wholeWords(node: TreeAnswerNode, context: TreeDraftContext): string {
  return wordsOf({ content: wholeRuns(node, context), type: "paragraph" });
}

/**
 * The runs of the one paragraph the whole words `node` has read as (`wholeWords`): a lens's
 * verdict, a risk's heading and a constraint's words as their rows draw them, and any other
 * node's reading where the read sent one (`text_tree`), else its stored text. Each is read onto
 * one line with one space between words, each run keeping the marks the plain-text projection
 * reads (`markedRuns`), and each store address in them as its kind's word, run by run before the
 * words are read (`storeKindWords`), since a chip and the chain draw them as plain words that
 * link nothing; an address in code, and every other address, stays as written.
 */
function wholeRuns(node: TreeAnswerNode, context: TreeDraftContext): DisplayTree[] {
  const type = lensTypeOf(node, context);
  if (type === "constraint") {
    return constraintRuns(node, (raw) => markedRuns(storeKindWords(storedReading(raw))));
  }
  const reading =
    type === "lens" || type === "risk"
      ? storedReading(storedRowWords(node, type))
      : (node.text_tree ?? storedReading(node.text));
  return markedRuns(storeKindWords(reading));
}

/** The words of `runs`, one paragraph's of `markedRuns`, and their reading where a run of them
 *  keeps a mark (`plainWords`): what a chip names its node by (`LensChip`), and a chain entry the
 *  node its lens stands under (`LensPlace`). */
export function namedBy(runs: DisplayTree[]): Pick<LensChip, "reading" | "words"> {
  const words = plainWords(runs);
  return typeof words === "string" ? { words } : { reading: words, words: wordsOf(words) };
}

/** A constraint's words as the runs of one paragraph: a standard's `standard`, else a
 *  measurement's `<measurement>: <value>`, the `": "` a run of its own, and none where both are
 *  empty. `read` gives each stored string's runs, none where it holds no words: its plain words
 *  (`plainRuns`), in a row's words its reading with each store address named (`store-words.ts`),
 *  and in a chip's its runs with their marks (`wholeRuns`). */
export function constraintRuns(
  constraint: TreeAnswerNode,
  read: (markdown: string) => DisplayTree[] = plainRuns
): DisplayTree[] {
  const standard = constraint.attributes?.standard ?? "";
  if (standard !== "") return read(standard);
  const measurement = read(constraint.attributes?.measurement ?? "");
  const value = read(constraint.attributes?.value ?? "");
  return measurement.length === 0 && value.length === 0
    ? []
    : [...measurement, { text: ": ", type: "text" }, ...value];
}

/** The words of the stored markdown text `markdown` on one line: its reading (`storedReading`)
 *  read to plain words (`oneLine`). Each address stays as its author wrote it: `store-words.ts`
 *  names a store address in a row's words, and the row's renderer (`RowText.tsx`'s `DrawnText`)
 *  names every other reference. */
export function storedWords(markdown: string): string {
  return oneLine(storedReading(markdown));
}

/** The words of `reading` (`wordsOf`) on one line: each run of white space one space, and the
 *  ends trimmed. */
function oneLine(reading: DisplayTree): string {
  return wordsOf(reading).replace(/\s+/gu, " ").trim();
}

/** A fact line read from the stored string `markdown` (`LensFacts`): its words (`storedWords`)
 *  where no run of them keeps a mark the plain-text projection reads, and else their reading as
 *  its one part (`markedRuns`, `plainWords`), so the line reads an address in code as written.
 *  Every other address stays as its author wrote it, for the projection to name
 *  (`LensLines.tsx`'s `FactString`). */
function factLine(markdown: string): string | FactParts {
  const words = plainWords(markedRuns(storedReading(markdown)));
  return typeof words === "string" ? words : [words];
}

/** The stored string `markdown`'s words as one text run, none where it holds no words: its plain
 *  words (`storedWords`). */
export function plainRuns(markdown: string): DisplayTree[] {
  const words = storedWords(markdown);
  return words === "" ? [] : [{ text: words, type: "text" }];
}

/** The marks of a run that stands in no mark. */
export const NO_MARKS: readonly DisplayTree[] = [];

/** The marks a code block's text keeps on one line (`markedRuns`), where no block holds it: code. */
const IN_CODE: readonly DisplayTree[] = [{ type: "inlineCode" }];

/** Whether the plain-text projection reads the mark `mark`: code, in whose words it names
 *  nothing, and a link, whose words it reads as a rendered link's. Formatting reads as the words
 *  it covers. */
function readMark(mark: DisplayTree): boolean {
  return mark.type === "inlineCode" || mark.type === "link";
}

/**
 * The runs of the one paragraph `reading` reads as on one line: its text runs in order and spaced
 * as `wordsOf` joins them, each run of white space one space, across the runs' edges too, and
 * none at either end, so their words are `oneLine`'s. Each run carries the marks `marksOf` gives
 * it, `inCode` saying it stands inside a code block, and the text between two runs that carry
 * marks is one run that carries none. `store-words.ts` reads a row's words onto one line so, each
 * name's run with its link alone, and a fact line's and a chip's words keep their marks
 * (`markedRuns`).
 */
export function oneLineRuns(
  reading: DisplayTree,
  marksOf: (run: DisplayTree, inCode: boolean) => readonly DisplayTree[]
): DisplayTree[] {
  // The words since the last run that carries marks, each such run, and whether the next run
  // stands apart from the words before it, as `wordsOf` spaces them.
  const parts: DisplayTree[] = [];
  let words = "";
  let apart = false;
  const read = (node: DisplayTree, inCode: boolean): void => {
    if (typeof node.text === "string") {
      if (apart && (words !== "" || parts.length > 0)) words += " ";
      apart = false;
      const marks = marksOf(node, inCode);
      if (marks.length === 0) {
        words += node.text;
        return;
      }
      if (words !== "") parts.push({ text: words, type: "text" });
      words = "";
      parts.push({ marks, text: node.text, type: "text" });
      return;
    }
    apart = true;
    if (!Array.isArray(node.content)) return;
    const code = inCode || node.type === "code_block";
    for (const child of node.content as DisplayTree[]) read(child, code);
    apart = true;
  };
  read(reading, false);
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

/** `reading`'s runs on one line (`oneLineRuns`), each keeping the marks the plain-text projection
 *  reads (`readMark`), and a code block's text marked as code, so the projection reads an address
 *  in code as written, as a rendered body draws it. */
export function markedRuns(reading: DisplayTree): DisplayTree[] {
  return oneLineRuns(reading, (run, inCode) => {
    if (inCode) return IN_CODE;
    const marks = Array.isArray(run.marks) ? (run.marks as DisplayTree[]) : NO_MARKS;
    return marks.every(readMark) ? marks : marks.filter(readMark);
  });
}

/** `runs`, one paragraph's of `markedRuns`, as plain words (`PlainWords`): their text where no
 *  run of them carries a mark, and else their reading (`lineOf`). */
export function plainWords(runs: DisplayTree[]): PlainWords {
  return runs.some((run) => Array.isArray(run.marks))
    ? lineOf(runs)
    : wordsOf({ content: runs, type: "paragraph" });
}

/** A reading of one paragraph that holds `runs`. */
export function lineOf(runs: DisplayTree[]): DisplayTree {
  return { content: [{ content: runs, type: "paragraph" }], type: "doc" };
}

/** A kind of tuple whose far end a chip names (`endChips`). */
type ChipTuple = "constrained_by" | "prevented_by" | "triggered_by";

/** The kind of node at the far end of each kind of tuple a chip names, by whose word a chip
 *  names a node the read so far holds none of (`unheldChip`). */
const FAR_KINDS: Readonly<Record<ChipTuple, "check" | "constraint" | "lens">> = {
  constrained_by: "constraint",
  prevented_by: "check",
  triggered_by: "lens",
};

/**
 * The chips of the nodes at the `end` of `node`'s tuples of `kind`, in the answer's order, with a
 * later chip of an address an earlier one holds left out (`uniqueChips`): `held`'s chip of a node
 * the answer holds, where it makes one, and the chip of its kind's word of a node the read so far
 * holds none of (`unheldChip`).
 */
function endChips(
  node: TreeAnswerNode,
  kind: ChipTuple,
  end: "from" | "to",
  context: TreeDraftContext,
  held: (target: TreeAnswerNode, context: TreeDraftContext) => LensChip | undefined
): LensChip[] {
  const tuples = end === "to" ? context.tuplesFrom(node.id) : context.tuplesTo(node.id);
  return uniqueChips(
    tuples.flatMap((tuple) => {
      if (tuple.kind !== kind) return [];
      const target = context.nodeOf(tuple[end].id);
      const chip = target === undefined ? unheldChip(kind, tuple[end].id) : held(target, context);
      return chip === undefined ? [] : [chip];
    })
  );
}

/**
 * The chip of the node `readId` at the far end of a tuple of `kind`, where the read so far holds
 * no node of it: the server answers a tuple wherever the whole answer holds its `to` node, so
 * that node stands on a page not yet read. It is drawn at once by the page's word for its kind
 * (`FAR_KINDS`, `chrome`), and linked by its read id to Graph's row (`graphOnly`): no view's row
 * of it is known until its page is read, and Graph's address reads Graph's pages until it holds
 * the node. A constraint and a lens are at their store address (`storeAddressOf`), and a check at
 * its read id, as a check the read gives no address is.
 */
function unheldChip(kind: ChipTuple, readId: string): LensChip {
  const type = FAR_KINDS[kind];
  return {
    address: type === "check" ? readId : storeAddressOf(type, readId),
    chrome: true,
    graphOnly: true,
    id: readId,
    words: kindWords(NODE_TYPE_NAMES[type]),
  };
}

/** The chip of `lens` where it is a lens, named by its kind's words as its tag is. */
function lensChip(lens: TreeAnswerNode, context: TreeDraftContext): LensChip | undefined {
  const address = lens.supertag === "lens" ? addressOf(lens, context) : undefined;
  return address === undefined
    ? undefined
    : chipTo(lens, address, lensChipWords(kindOf(lens, context)), context);
}

/** The chip naming `target` at `address` by `named`'s words, and their reading where they have
 *  one, linked by its read id where a step drafted it. */
function chipTo(
  target: TreeAnswerNode,
  address: string,
  named: Pick<LensChip, "chrome" | "reading" | "words">,
  context: TreeDraftContext
): LensChip {
  return {
    address,
    ...(context.drafted(target.id) ? { id: target.id } : {}),
    state: target.state,
    visibility: target.visibility,
    ...named,
  };
}

/** The chip naming the constraint `constraint` at `address` by its whole words, and their reading
 *  where they keep a mark (`wholeRuns`, `namedBy`): linked by its read id where a step drafted it
 *  (`chipTo`), and else, where it is one of the issue's (`underIssue`), linked by it to Graph's
 *  row, which only Graph draws then (`graphOnly`). A catalogue constraint is no row, so its chip
 *  is no link. */
function constraintChip(
  constraint: TreeAnswerNode,
  address: string,
  context: TreeDraftContext
): LensChip {
  const chip = chipTo(constraint, address, namedBy(wholeRuns(constraint, context)), context);
  return chip.id !== undefined || !underIssue(constraint, context)
    ? chip
    : { ...chip, graphOnly: true, id: constraint.id };
}

/** `chips` with a later chip of an address an earlier one holds left out. */
function uniqueChips(chips: readonly LensChip[]): LensChip[] {
  const unique = new Map<string, LensChip>();
  for (const chip of chips) {
    if (!unique.has(chip.address)) unique.set(chip.address, chip);
  }
  return [...unique.values()];
}

/** Every lens of the answer by its address, for the chain a lens draws (`LensChain.tsx`): its
 *  kind's words as held, its id where a step drafted it, and the whole words of its parent, with
 *  their reading where they keep a mark (`namedBy`), where a step drafted the parent. One map per
 *  build, shared by every lens: it reads which nodes were drafted, which the page's other reads
 *  change while the answer stands. */
const placeIndexes = new WeakMap<TreeDraftContext, ReadonlyMap<string, LensPlace>>();

function lensPlaces(answer: TreeAnswer, context: TreeDraftContext): ReadonlyMap<string, LensPlace> {
  const held = placeIndexes.get(context);
  if (held !== undefined) return held;
  const places = new Map<string, LensPlace>();
  for (const lens of answer.nodes ?? []) {
    const address = lens.supertag === "lens" ? addressOf(lens, context) : undefined;
    if (address === undefined) continue;
    const parent = lens.parent === null ? undefined : context.nodeOf(lens.parent.id);
    const on: Pick<LensChip, "reading" | "words"> =
      parent !== undefined && context.drafted(parent.id)
        ? namedBy(wholeRuns(parent, context))
        : { words: "" };
    places.set(address, {
      ...(context.drafted(lens.id) ? { id: lens.id } : {}),
      kindWords: kindOf(lens, context),
      ...(on.words === "" ? {} : { onWords: on.words }),
      ...(on.reading === undefined ? {} : { onReading: on.reading }),
    });
  }
  placeIndexes.set(context, places);
  return places;
}
