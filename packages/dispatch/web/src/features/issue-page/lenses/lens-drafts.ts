import { chipWords } from "../node-row-chip";
import { NODE_TYPE_NAMES, sentenceCase } from "../page-words";
import { blank, type Draft, plainWords } from "../tree-drafts";
import type { TreeDraftContext } from "../tree-read/tree-nodes";
import type { TreeAnswer, TreeAnswerNode } from "../tree-read/tree-read";

/**
 * What a row of a lens, a finding, a risk or a constraint carries beyond a `TreeNode`'s own
 * fields, and what a statement a constraint names carries. Every fact comes from the tree read;
 * nothing here sends a request.
 *
 * `lensDrafts` is the step of `tree-read/tree-nodes.ts`'s `treeDraftSteps` that drafts these
 * four kinds, and `lensChips` the mark of its `treeDraftMarks` that puts the chips on the drafts
 * once every node has been drafted. Both read the answer in its own order and order nothing: the
 * read's order is the outline's. This module imports only types from `tree-read/` and
 * `issue-tree.ts`, which import it, so no import cycle closes (`biome.json`'s `noImportCycles`).
 */

/** One chip that names another node of the tree: a lens, a check or a constraint. */
export interface LensChip {
  /** The target's `dispatch://` address, as its own row carries it, but a check's id in the tree
   *  read where the read gives the check no address (`TreeDraftContext.referenceOf`). */
  readonly address: string;
  /** The target's words, cut by `chipWords`. */
  readonly words: string;
  /** Set only where `words` are the page's and not the author's: the page's word for a lens,
   *  which names a lens whose kind has no words (`lensChipWords`). */
  readonly chrome?: true;
  /** The target's id in the tree read, only where a step drafted it: the link's `?node=`. */
  readonly id?: string;
  /** The target's state and visibility, as the tree read gives them, for the node marks. */
  readonly state?: string;
  readonly visibility?: string;
}

/** One lens of the tree, as the chain of a focused lens draws it. */
export interface LensPlace {
  readonly kindWords: string;
  /** The words the row it stands under draws its one line from, as that row's draft holds them
   *  (`TreeDraftContext.lineOf`); `LensChain.tsx` cuts them as that row does. */
  readonly onLine?: string;
  /** Its id in the tree read, where a step drafted it. */
  readonly id?: string;
}

/** The lens facts of one node. A list with nothing in it draws no line. */
export interface LensFacts {
  /** A lens: its kind's words, as the `lens_kind` node holds them. */
  readonly kind?: string;
  readonly triggeredBy: readonly LensChip[];
  readonly triggered: readonly LensChip[];
  readonly earlyWarning?: string;
  readonly prevention?: string;
  readonly preventedBy: readonly LensChip[];
  readonly constrainedBy: readonly LensChip[];
  /** A standard's reference. */
  readonly reference?: string;
  /** Where a measurement was taken. */
  readonly takenWhere?: string;
  /** A lens: every lens of the tree, by its address. */
  readonly lenses?: ReadonlyMap<string, LensPlace>;
}

/** A kind's words with their first letter in capitals, as a lens's tag and a chip that names a
 *  lens draw them. A chain entry and a menu item draw the words as the node holds them. */
export function kindWords(words: string): string {
  return sentenceCase(words);
}

/** The words of a chip that names a lens: its kind's words `kind`, capitalised and cut as
 *  its tag is; where `kind` holds none, the page's word for a lens, which the chip marks as the
 *  page's (`chrome`). The outline's trigger chips and the chain's last chip are named alike. */
export function lensChipWords(kind: string | undefined): Pick<LensChip, "chrome" | "words"> {
  return filled(kind)
    ? { words: chipWords(kindWords(kind)) }
    : { chrome: true, words: chipWords(kindWords(NODE_TYPE_NAMES.lens)) };
}

/** Whether a stored string has words to draw. */
export function filled(text: string | undefined): text is string {
  return text !== undefined && text !== "";
}

/** The four kinds `lensDrafts` draws, as the tree names them. */
type LensType = "constraint" | "finding" | "lens" | "risk";

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

/**
 * The draft of a lens, a finding, a risk or a constraint of the issue; undefined for any other
 * node, which the tree read's other steps are asked for. A catalogue node drafts no row: a
 * `lens_kind`, and a constraint the read marks as context or whose parent is not the issue.
 *
 * It stands under its parent's row (`parentsOf`), with its address as its reference
 * (`addressOf`), and its author and time as the other steps give a turn's. Its words: a lens's
 * verdict, or none, which its row draws as `LENS_WORDS.no_verdict`; a finding's text and its
 * reading, as the other steps read a node's; a risk's heading; a constraint's words
 * (`constraintWords`). Each stored string is read through `plainWords`, but a kind's words,
 * which are drawn as the `lens_kind` node holds them. Its lens facts carry no chip yet:
 * `lensChips` adds them.
 */
export function lensDrafts(
  node: TreeAnswerNode,
  _answer: TreeAnswer,
  context: TreeDraftContext
): Draft | undefined {
  const type = lensTypeOf(node, context);
  if (type === undefined || (type === "constraint" && !issueConstraint(node, context))) {
    return undefined;
  }
  const parents = parentsOf(node, context);
  if (parents === undefined) return undefined;
  const draft = blank(context.treeIdOf(node.id), type, 0);
  draft.parents = parents;
  draft.at = node.made_at;
  // The read lists every author of the node's first version, the one who made it first ahead of
  // the rest, and a row draws the first.
  const author = node.author[0];
  if (author !== undefined) draft.author = author;
  const address = addressOf(node, context);
  if (address !== undefined) draft.reference = address;
  const attributes = node.attributes;
  switch (type) {
    case "lens": {
      draft.text = plainWords(stringOf(attributes.verdict));
      const kind = kindOf(node, context);
      draft.lens = kind === "" ? { ...NO_CHIPS } : { ...NO_CHIPS, kind };
      break;
    }
    case "finding":
      draft.text = node.text;
      if (node.text_tree !== undefined) draft.tree = node.text_tree;
      draft.lens = { ...NO_CHIPS };
      break;
    case "risk": {
      draft.text = plainWords(stringOf(attributes.heading));
      const earlyWarning = plainWords(stringOf(attributes.early_warning));
      const prevention = plainWords(stringOf(attributes.prevention));
      draft.lens = {
        ...NO_CHIPS,
        ...(earlyWarning === "" ? {} : { earlyWarning }),
        ...(prevention === "" ? {} : { prevention }),
      };
      break;
    }
    case "constraint": {
      draft.text = constraintWords(node);
      // A standard names its reference; a measurement says where it was taken.
      const standard = stringOf(attributes.standard) !== "";
      const reference = plainWords(stringOf(attributes.reference));
      const takenWhere = plainWords(stringOf(attributes.taken_where));
      draft.lens = standard
        ? { ...NO_CHIPS, ...(reference === "" ? {} : { reference }) }
        : { ...NO_CHIPS, ...(takenWhere === "" ? {} : { takenWhere }) };
      break;
    }
  }
  return draft;
}

/**
 * The chips of `draft`, once every node has been offered to the steps, from the answer's tuples
 * in the answer's order. Each chip carries its target's address, its state and its visibility,
 * and its id in the tree read only where a step drafted the target, which makes it a link to
 * that row (`?node=`).
 *
 * - A lens: a chip for each lens its `triggered_by` tuples name (`triggeredBy`) and for each lens
 *   whose `triggered_by` names it (`triggered`), only for a lens the read holds, each named by its
 *   kind's words as its tag is; and `lenses`, every lens of the answer by its address, one map
 *   shared by every lens.
 * - A risk: a chip for each check its `prevented_by` tuples name, named by that check's line
 *   (`chipWords(lineWords(node))`).
 * - Any draft with a `constrained_by` tuple, a statement above all: a chip for each constraint it
 *   names, by the constraint's words. A catalogue constraint is no row, so its chip is no link. A
 *   draft of another step gains lens facts only where it has such a chip.
 */
export function lensChips(
  draft: Draft,
  node: TreeAnswerNode,
  answer: TreeAnswer,
  context: TreeDraftContext
): void {
  const constrainedBy = uniqueChips(
    targetsOf(node, "constrained_by", context).flatMap((target) => {
      const address = target.supertag === "constraint" ? addressOf(target, context) : undefined;
      return address === undefined
        ? []
        : [chipTo(target, address, { words: chipWords(constraintWords(target)) }, context)];
    })
  );
  if (draft.lens === undefined && constrainedBy.length === 0) return;
  const facts: LensFacts = { ...(draft.lens ?? NO_CHIPS), constrainedBy };
  if (draft.type === "lens") {
    draft.lens = {
      ...facts,
      lenses: lensPlaces(answer, context),
      triggered: lensChipsTo(
        (triggersOf(answer).get(node.id) ?? []).flatMap((readId) => context.nodeOf(readId) ?? []),
        context
      ),
      triggeredBy: lensChipsTo(targetsOf(node, "triggered_by", context), context),
    };
  } else if (draft.type === "risk") {
    draft.lens = {
      ...facts,
      preventedBy: uniqueChips(
        targetsOf(node, "prevented_by", context).map((check) =>
          chipTo(
            check,
            context.referenceOf(check) ?? check.id,
            { words: chipWords(context.lineOf(check.id) ?? "") },
            context
          )
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
function lensTypeOf(node: TreeAnswerNode, context: TreeDraftContext): LensType | undefined {
  switch (node.supertag) {
    case "constraint":
    case "lens":
    case "risk":
      return node.supertag;
    case "block":
    case "lens_block":
      return node.parent !== null && context.nodeOf(node.parent)?.supertag === "lens"
        ? "finding"
        : undefined;
    default:
      return undefined;
  }
}

/** Whether `constraint` is one of the issue's own: under the issue, and not the context
 *  the read brings in for a catalogue constraint a tuple names. */
function issueConstraint(constraint: TreeAnswerNode, context: TreeDraftContext): boolean {
  return (
    constraint.mark !== "context" &&
    constraint.parent !== null &&
    context.treeIdOf(constraint.parent) === context.key
  );
}

/**
 * The `dispatch://` address of a lens, a finding, a risk or a constraint of the tree read, which
 * its row's Copy reference copies and a chip naming it carries: `dispatch://lens/<uuid>`, a
 * finding's its lens's with its block id (`dispatch://lens/<uuid>#b-<block id>`),
 * `dispatch://risk/<uuid>` and `dispatch://constraint/<uuid>`; undefined for any other node.
 *
 * The tree read answers no address per node, so this builds one from the node's read id until
 * the server serves that read: a node's read id is its store id, bare or after its kind
 * (`lens/<uuid>`), and a finding's is its lens's and its block id after a `#`
 * (`lens/<uuid>#<block id>`, as the server writes one), or its block id alone.
 */
function addressOf(node: TreeAnswerNode, context: TreeDraftContext): string | undefined {
  const type = lensTypeOf(node, context);
  if (type === undefined) return undefined;
  if (type !== "finding") return `dispatch://${type}/${storeId(node.id, type)}`;
  const block = node.id.slice(node.id.lastIndexOf("#") + 1);
  return `dispatch://lens/${storeId(node.parent ?? "", "lens")}#b-${block}`;
}

/** `id` without the kind its store address starts with (`lens/`), where it carries one. */
function storeId(id: string, kind: string): string {
  return id.startsWith(`${kind}/`) ? id.slice(kind.length + 1) : id;
}

/**
 * Where a draft of `node` stands, by its read parent: `[]` under the issue, where
 * `treeDraftsFrom` places it; under the parent's row where a step drafted the parent; under the
 * ask, the comment or the message the parent is a block of, where no step drafted that block;
 * and nowhere else - undefined, so neither the node nor anything under it is drawn.
 */
function parentsOf(node: TreeAnswerNode, context: TreeDraftContext): string[] | undefined {
  const parent = node.parent;
  if (parent === null) return undefined;
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
    above = node.parent;
  }
  return undefined;
}

/** The words of the lens kind `lens`'s `has_kind` tuple names, as that `lens_kind` node holds
 *  them: its `words`, else its text; `""` where the read holds none. */
function kindOf(lens: TreeAnswerNode, context: TreeDraftContext): string {
  for (const tuple of context.tuplesFrom(lens.id)) {
    const kind = tuple.kind === "has_kind" ? context.nodeOf(tuple.to) : undefined;
    if (kind === undefined) continue;
    const words = stringOf(kind.attributes.words);
    return words === "" ? kind.text : words;
  }
  return "";
}

/** A constraint's words: a standard's `standard`, else a measurement's
 *  `<measurement>: <value>`, each stored string through `plainWords`. */
function constraintWords(constraint: TreeAnswerNode): string {
  const standard = stringOf(constraint.attributes.standard);
  if (standard !== "") return plainWords(standard);
  const measurement = plainWords(stringOf(constraint.attributes.measurement));
  const value = plainWords(stringOf(constraint.attributes.value));
  return measurement === "" && value === "" ? "" : `${measurement}: ${value}`;
}

/** The nodes of the answer the tuples of `kind` from `node` point at, in the answer's order. */
function targetsOf(
  node: TreeAnswerNode,
  kind: string,
  context: TreeDraftContext
): TreeAnswerNode[] {
  return context.tuplesFrom(node.id).flatMap((tuple) => {
    const target = tuple.kind === kind ? context.nodeOf(tuple.to) : undefined;
    return target === undefined ? [] : [target];
  });
}

/** The chip of each of `lenses` that is a lens, named by its kind's words as its tag is. */
function lensChipsTo(lenses: readonly TreeAnswerNode[], context: TreeDraftContext): LensChip[] {
  return uniqueChips(
    lenses.flatMap((lens) => {
      const address = lens.supertag === "lens" ? addressOf(lens, context) : undefined;
      if (address === undefined) return [];
      return [chipTo(lens, address, lensChipWords(kindOf(lens, context)), context)];
    })
  );
}

/** The chip naming `target` at `address` by `named`'s words, linked by its read id where a step
 *  drafted it. */
function chipTo(
  target: TreeAnswerNode,
  address: string,
  named: Pick<LensChip, "chrome" | "words">,
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

/** `chips` with a later chip of an address an earlier one holds left out. */
function uniqueChips(chips: readonly LensChip[]): LensChip[] {
  const unique = new Map<string, LensChip>();
  for (const chip of chips) {
    if (!unique.has(chip.address)) unique.set(chip.address, chip);
  }
  return [...unique.values()];
}

/** For each lens, the lenses whose `triggered_by` tuples name it, in the answer's order: indexed
 *  once per answer, since it reads the answer alone. */
const triggerIndexes = new WeakMap<TreeAnswer, ReadonlyMap<string, readonly string[]>>();

function triggersOf(answer: TreeAnswer): ReadonlyMap<string, readonly string[]> {
  const held = triggerIndexes.get(answer);
  if (held !== undefined) return held;
  const index = new Map<string, string[]>();
  for (const tuple of answer.tuples) {
    if (tuple.kind !== "triggered_by") continue;
    const from = index.get(tuple.to);
    if (from === undefined) index.set(tuple.to, [tuple.from]);
    else from.push(tuple.from);
  }
  triggerIndexes.set(answer, index);
  return index;
}

/** Every lens of the answer by its address, for a focused lens's chain (`LensChain.tsx`): its
 *  kind's words as held, its id where a step drafted it, and its parent's line where a step
 *  drafted the parent. One map per build, shared by every lens: it reads which nodes were drafted
 *  and their lines, which the page's other reads change while the answer stands. */
const placeIndexes = new WeakMap<TreeDraftContext, ReadonlyMap<string, LensPlace>>();

function lensPlaces(answer: TreeAnswer, context: TreeDraftContext): ReadonlyMap<string, LensPlace> {
  const held = placeIndexes.get(context);
  if (held !== undefined) return held;
  const places = new Map<string, LensPlace>();
  for (const lens of answer.nodes) {
    const address = lens.supertag === "lens" ? addressOf(lens, context) : undefined;
    if (address === undefined) continue;
    const onLine = lens.parent === null ? undefined : context.lineOf(lens.parent);
    places.set(address, {
      ...(context.drafted(lens.id) ? { id: lens.id } : {}),
      kindWords: kindOf(lens, context),
      ...(onLine === undefined ? {} : { onLine }),
    });
  }
  placeIndexes.set(context, places);
  return places;
}

/** A stored attribute as a string, `""` where it is none. */
function stringOf(value: unknown): string {
  return typeof value === "string" ? value : "";
}
