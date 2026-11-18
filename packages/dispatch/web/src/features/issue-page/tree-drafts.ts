import { primarySpec, roleDocument } from "../../api/issue-cache";
import type {
  Actor,
  Artifact,
  Ask,
  CheckBlock,
  CheckResult,
  DisplayTree,
  IssueChild,
  IssueDetails,
  ReferenceTarget,
} from "../../api/types";
import { compareTimestamps, isTimestamp } from "../../lib/timestamps";
import { referenceSpans } from "../refs/reference-spans";
import {
  buildDispatchReference,
  parseDispatchReference,
  parseNodeReference,
  shortForm,
} from "../refs/routes";
import type { TextSource } from "../refs/text-references";
import { artifactsBySlug } from "./issue-artifacts";
import type {
  IssueTreeReads,
  NodeAnswer,
  NodeChip,
  NodeCounts,
  NodeFact,
  NodeOption,
} from "./issue-tree";
import type { LensFacts } from "./lenses/lens-drafts";
import {
  type AskState,
  type CheckState,
  type GapKind,
  NODE_STATE_WORDS,
  NODE_TYPE_NAMES,
  NODE_TYPES,
  type NodeState,
  type NodeType,
  PRESET_NAMES,
} from "./page-words";

/**
 * The outline's rules for the drafts `buildIssueTree` places (root R4), which the tree read
 * keeps: how this issue addresses a node and names what a reference points
 * at (`treeContext`), a node's words read to plain words, and how a check stands
 * (`checkStanding`).
 *
 * Every node is drafted from the tree read (`tree-read/tree-nodes.ts`'s `treeDraftsFrom`),
 * which places and orders each one. Every function here is pure and takes the reads as its
 * input, and no request is made per node. Where a read sends the server's reading of a node's
 * words beside them, the draft keeps it with the record the words are, which is what the row
 * draws (`TreeNode.tree`).
 *
 * The tree is the issue's conversation and what it revealed: a document, an artifact and a
 * handoff are nodes of the issue and not of its conversation, so nothing here drafts one.
 */

/** The id of the row of one block of one document, which is the block's own address inside the
 *  issue: a block id alone is unique only inside its document, and an issue carries several. */
export function blockNodeId(slug: string, blockId: string): string {
  return `${slug}#b-${blockId}`;
}

/** One node as it is being built: a `TreeNode`'s own fields, plus where the model placed it. */
export interface Draft {
  /** When it was made, where its read records one (`TreeNode.at`). */
  at?: string;
  author?: Actor;
  /** A person's answer to this ask, which the ask's own row and body draw (`TreeNode.answer`). */
  answer?: NodeAnswer;
  chips: NodeChip[];
  /** What the coverage read counts for a requirement it holds (`tree-read/tree-nodes.ts`);
   *  absent on every other draft, a statement of the spec drawn before that read answers among
   *  them. */
  counts?: NodeCounts;
  /** What the node's body draws under its text (`check-facts.ts`). */
  facts: NodeFact[];
  gaps: GapKind[];
  readonly id: string;
  /** The child issue this node is, as the issue read carries it (`TreeNode.child`). */
  child?: IssueChild;
  /** The person who kept this decision as it reads now (`TreeNode.keptBy`). */
  keptBy?: Actor;
  /** The lens facts of a lens, a finding, a risk, a constraint or a statement a constraint names
   *  (`TreeNode.lens`). */
  lens?: LensFacts;
  /** Whether this ask takes more than one of its options (`TreeNode.multiple`). */
  multiple?: true;
  /** The choices an ask offers (`TreeNode.options`). */
  options?: NodeOption[];
  order: number;
  /** The nodes this one sits under, in the order the model placed them: the first is where it is
   *  edited, every other a transclusion. `[]` is "under the issue". */
  parents: string[];
  question: boolean;
  /** The id the tree read (`tree-read/tree-nodes.ts`'s `treeDraftsFrom`) gives the node this
   *  draft was made from (`TreeNode.readId`); absent on a draft made from another read. */
  readId?: string;
  reference?: string;
  replaced: boolean;
  /** The record `text` is, where the draft's read names one (`TreeNode.source`). */
  source?: TextSource;
  /** How it stands, for a type the model gives a state (`TreeNode.state`); absent on a check
   *  block of any document but the issue's Verification document (`tree-read/tree-nodes.ts`'
   *  `draftServing`). */
  state?: NodeState;
  /** Whether this requirement is a statement (`TreeNode.statement`): a `requirement` block of the
   *  issue's spec or a requirement the coverage read holds (`tree-read/tree-nodes.ts`'
   *  `draftRequirement`). Absent on every other draft. */
  statement?: true;
  text: string;
  /** The server's reading of `text`, where the draft's read sends one (`TreeNode.tree`). */
  tree?: DisplayTree;
  type: NodeType;
  /** Whether the model marks this decision unsure (`Decision.unsure`). */
  unsure?: true;
  /** Whether this node waits on the reader: a decision in their review list (`inReviewList`),
   *  or an open question whose turn is the reader's own (`waiting_on_person`). */
  waiting: boolean;
}

/** What every builder needs of the reads: how this issue addresses a node, and what a reference
 *  points at. Held as one value so the placement and the names are read the same way everywhere. */
export interface TreeContext {
  /** A chip naming what `reference` points at, by the one batched reference read and the
   *  issue's own documents (`treeContext`): one chip per reference, the same value on every
   *  call. */
  readonly chipOf: (reference: string) => NodeChip;
  /** The words of the stored excerpt the batched reference read names `reference`'s target by
   *  (`ReferenceTarget.name`), with a typed block's directive left out (`blockWords`); `""` for
   *  a target the read has not answered for and for one nothing answers to now. It is a block's
   *  source, cut at 480 runes and unread, so no chip draws it: it is where a node's citations
   *  are read from beside its words, and a block's words where the read sent no reading. */
  readonly excerptOf: (reference: string) => string;
  readonly key: string;
  /** The node `reference` names, as this issue addresses it; undefined for anything else. */
  readonly nodeIdOf: (reference: string) => string | undefined;
  /** The slug of the issue's spec (`primarySpec`), `""` for an issue with none. */
  readonly specSlug: string;
  /** The slug of one document of the issue, by its artifact id. */
  readonly slugOf: (artifactId: string) => string | undefined;
  /** The server's reading of what `reference` points at, where the batched reference read sent
   *  one: a typed block's own inner content, whole, references kept (`ReferenceTarget.text_tree`). */
  readonly treeOf: (reference: string) => DisplayTree | undefined;
  /** The reader's login, which an open question waits on where its turn is theirs; undefined
   *  until the login resolves, and nothing waits on nobody. */
  readonly viewer: string | undefined;
}

/**
 * How `issue` addresses a node: the node a `dispatch://` reference names, as the tree's node ids
 * write it - the spec, a document, a block of either, an ask, a comment, a message, the issue
 * itself - and undefined for a reference to another issue or to anything else. It reads only the
 * issue's key and its spec's slug (`primarySpec`), so a caller that needs a node id and no chip
 * calls it without building a whole `treeContext`, and `treeContext` addresses nodes by it too,
 * so the two never differ.
 */
export function nodeIdOf(
  issue: Pick<IssueTreeReads["issue"], "artifacts" | "key" | "primary_artifact_id">
): (reference: string) => string | undefined {
  const key = issue.key;
  const specSlug = primarySpec(issue)?.slug ?? "";
  return (reference: string): string | undefined => {
    const route = parseNodeReference(reference);
    if (route === undefined || !("key" in route) || route.key !== key) return undefined;
    const block = "block" in route ? route.block : undefined;
    if (route.kind === "spec") {
      return block === undefined ? specSlug : blockNodeId(specSlug, block.id);
    }
    if (route.kind === "artifact") {
      return block === undefined ? route.slug : blockNodeId(route.slug, block.id);
    }
    if (route.kind === "ask" || route.kind === "comment" || route.kind === "message") {
      return route.id;
    }
    return route.kind === "issue" ? key : undefined;
  };
}

/**
 * The reads as every builder reads them. `lineOf` is the one line a place that draws one line
 * for a node draws, from the server's reading of the node's words: `buildIssueTree` and
 * `targetNames` give R1's rule over the words of the reading's first block (`chipLine`, which
 * is `drawnLine(firstLineOf(…))` of what `lineWords` reads), which this module cannot import,
 * since `issue-tree.ts` imports it. Every caller hands one in, so no chip names a block by
 * anything but the line its caller chose: a default would name it by its whole reading.
 *
 * A chip names its target by the words a reader reads it by, and never by markup:
 *
 * - **A target nothing answers to now** by nothing, `""`, which the chip draws as gone. A short
 *   reference two or more targets carry the id of is such a target, which the read answers
 *   `ambiguous` as well, and its chip carries `ambiguous` so it can draw the reference as
 *   written rather than as a target deleted.
 * - **A block** - the reference names one, or the read answers that its target is one - by
 *   `lineOf` of the server's reading of it (`ReferenceTarget.text_tree`). Where the read
 *   answered and sent no reading, or one that holds no words, a block of this issue whose
 *   document the tree read is named by the page's word for its kind (`NODE_TYPE_NAMES`), never
 *   by its id (R22); another issue's block, whose kind no read of this page gives, by `""`. A
 *   block the read has not answered for yet is `""` too, which the chip draws as not yet named
 *   (`NodeChip.answered`). The read's `name` for a block is its stored excerpt, which is the
 *   block's source: a typed block's directive and the markdown its words are written in are
 *   markup and no word of it (R21).
 * - **A whole document** - a `spec` or an `artifact` reference naming no block - by the page's
 *   word for it (`documentWord`), and never by its file name.
 * - **Anything else** by the read's `name` for it. An ask's, a comment's and a message's name is
 *   the question or the body as stored, which is markdown whose marks are no word of it (R21),
 *   so it is read to plain words (`plainWords`); an issue's and a component's name is a title,
 *   which is no markdown, and is left as the read gives it.
 */
export function treeContext(
  reads: IssueTreeReads,
  lineOf: (reading: DisplayTree) => string
): TreeContext {
  const key = reads.issue.key;
  const artifacts = reads.issue.artifacts ?? [];
  const slugs = new Map(artifacts.map((artifact) => [artifact.id, artifact.slug]));
  const documents = artifactsBySlug(reads.issue);
  const spec = primarySpec(reads.issue);
  const specSlug = spec?.slug ?? "";
  const names = new Map((reads.targets ?? []).map((target) => [target.ref, target]));
  // The kind of each typed block of this issue the tree read answers, by its node id, which is
  // all a chip can say of a block the reference read sent no words for.
  const types: readonly string[] = NODE_TYPES;
  const kinds = new Map<string, NodeType>();
  for (const node of reads.tree?.nodes ?? []) {
    if (node.document !== undefined && node.issue === undefined && types.includes(node.supertag)) {
      kinds.set(blockNodeId(node.document.slug, node.id), node.supertag as NodeType);
    }
  }
  const nodeIdOfReference = nodeIdOf(reads.issue);
  const nameOf = (reference: string, target: ReferenceTarget | undefined): string => {
    const route = parseNodeReference(reference);
    const block = route !== undefined && "block" in route ? route.block : undefined;
    if (block !== undefined || target?.kind === "block") {
      const reading = target?.text_tree;
      const line = reading === undefined ? "" : lineOf(reading);
      if (line !== "" || target === undefined) return line;
      const kind = kinds.get(nodeIdOfReference(reference) ?? "");
      return kind === undefined ? "" : NODE_TYPE_NAMES[kind];
    }
    // Another issue's document is named by no role of this issue's.
    if (route?.kind === "spec") {
      return documentWord(reads.issue, route.key === key ? spec : undefined);
    }
    if (route?.kind === "artifact") {
      return documentWord(reads.issue, route.key === key ? documents.get(route.slug) : undefined);
    }
    if (target?.kind === "ask" || target?.kind === "comment" || target?.kind === "message") {
      return plainWords(target.name);
    }
    return blockWords(target?.name ?? "");
  };
  // One chip per reference, made the first time it is asked for: a tree asks once for every
  // citation of a target, and no caller changes a chip - its fields are read-only (`NodeChip`),
  // and a caller that adds one, a `needs` label or a link's `stale`, spreads it into a chip of
  // its own - so every place that cites the target holds the same one.
  const chips = new Map<string, NodeChip>();
  return {
    chipOf: (reference: string): NodeChip => {
      const held = chips.get(reference);
      if (held !== undefined) return held;
      const target = names.get(reference);
      const gone = target?.gone ?? false;
      const chip: NodeChip = {
        ...(target?.ambiguous === true ? { ambiguous: true } : {}),
        answered: target !== undefined,
        gone,
        name: gone ? "" : nameOf(reference, target),
        ref: reference,
      };
      chips.set(reference, chip);
      return chip;
    },
    excerptOf: (reference: string): string => {
      const target = names.get(reference);
      return target === undefined || target.gone ? "" : blockWords(target.name);
    },
    key,
    nodeIdOf: nodeIdOfReference,
    specSlug,
    slugOf: (artifactId: string) => slugs.get(artifactId),
    treeOf: (reference: string) => names.get(reference)?.text_tree,
    viewer: reads.viewer,
  };
}

/**
 * The page's word for one document of the issue, which a chip naming the whole document draws:
 * the name of the filter that draws what it holds (`PRESET_NAMES`) for the spec and for the
 * plan, Verification and decisions documents, and the page's word for a document
 * (`NODE_TYPE_NAMES.document`) for any other, another issue's included (`undefined`). A
 * document's own name is its file name, `spec.md`, which is no word a reader reads it by (R21).
 *
 * Which document plays which part is the issue read's answer and nothing else: the id it names
 * for each (`primary_artifact_id`, `plan_artifact_id`, `verification_artifact_id`), read by
 * `primarySpec` and `roleDocument`, the rule the tree read's blocks of each part are read by
 * (`tree-read/tree-nodes.ts`' `blocksOfRole`). Where the issue names no id for a part - none
 * (`""`), or a Dispatch older than that part (absent) - no document plays it, and a document
 * flagged `primary` or carrying that part's `role` is named by the page's word for a document: a
 * chip naming "Spec" opens a document whose blocks the tree reads as that part.
 */
function documentWord(issue: IssueDetails, document: Pick<Artifact, "id"> | undefined): string {
  if (document === undefined) return NODE_TYPE_NAMES.document;
  if (document.id === primarySpec(issue)?.id) return PRESET_NAMES.spec;
  for (const role of ["plan", "verification"] as const) {
    if (document.id === roleDocument(issue, role)?.id) return PRESET_NAMES[role];
  }
  return NODE_TYPE_NAMES.document;
}

/** A directive's own opening line, `:::plan_task{#t1 serves="…"}`, and its closing fence. */
const DIRECTIVE_OPENER = /^:{3,}[a-z_]/;
const DIRECTIVE_FENCE = /^:{3,}\s*$/;

/**
 * The words of the name the batched reference read gives a target (`ReferenceTarget.name`).
 * The read names a document's block by its stored excerpt, which is the block's source: a typed
 * block is a directive, so its excerpt opens on the directive's line - its type and its
 * attributes - and, where the excerpt holds the whole block, ends on its closing fence. Both are
 * markup and no word of the block (R21), so they are left out. Any other name is left as the
 * read gives it.
 */
export function blockWords(name: string): string {
  const lines = name.trimEnd().split("\n");
  if (!DIRECTIVE_OPENER.test(lines[0] ?? "")) return name;
  lines.shift();
  if (DIRECTIVE_FENCE.test(lines.at(-1) ?? "")) lines.pop();
  return lines.join("\n").trim();
}

/** A blank line, which ends a paragraph: no mark and no code span reaches past one. A stored
 *  text's blocks are read one at a time between them (`plainWords`, `lineWords`). */
export const BLANK_LINE = /\n\s*\n/u;

/** One ASCII punctuation character: what a backslash escapes, and what a code span and an
 *  autolink hold that the marks below would otherwise read as marks. */
const PUNCTUATION = /[!-/:-@[-`{-~]/gu;

/**
 * What markdown reads literally, each where it starts, left to right: a backslash before an
 * ASCII punctuation character, which is that character; a code span, a run of backticks closed
 * by the next run of the same count, which is its content verbatim and inside which a backslash
 * escapes nothing; an autolink, `<https://…>`, which is its URL; and a run of backticks no run
 * of its count closes, which is that run as written. They are one pattern because each decides
 * the others: a backtick a backslash escapes opens no code span, and a backslash inside a code
 * span escapes nothing.
 */
const LITERAL =
  /\\([!-/:-@[-`{-~])|(`+)(?!`)([\s\S]*?[^`])\2(?!`)|<([A-Za-z][A-Za-z0-9+.-]{1,31}:[^\s<>]*)>|`+/gu;

/**
 * The markers a line of a blockquote or a list item opens on, at the start of the line, each
 * container's in turn as they nest: a blockquote's `>` and the one space or tab after it, which
 * markdown allows up to three spaces of indent before, once per quote (`> > deep`); and a
 * bullet (`-`, `+` or `*`) or an ordered marker (one to nine digits and `.` or `)`), which must
 * be followed by a space or a tab, after any indent, since a nested item carries its parent's,
 * with a task box (`[ ]`, `[x]`) after it, which the server's reading holds as the item's state
 * and no word of it. `**bold**` at a line's start opens on no marker, since its first `*` is
 * followed by another and not by white space; nor does what `LITERAL` read, which is held as a
 * stand-in by then, so `1\.` and `\-` stay words.
 */
const BLOCK_MARKER =
  /^(?: {0,3}>[ \t]?|[ \t]*(?:[-+*]|\d{1,9}[.)])[ \t]+(?:\[[ \txX]\][ \t]+)?)+/gmu;

/** A heading marker at the start of a line, `#` to `######` and a space, which markdown allows
 *  up to three spaces of indent before, and which a blockquote's or a list item's markers
 *  (`BLOCK_MARKER`) may stand before. */
const HEADING_MARKER = /^ {0,3}#{1,6}[ \t]+/gmu;

/** An image, `![alt](src)`, whose words the server's reading of it holds none of. */
const IMAGE = /!\[[^\]]*\]\([^)]*\)/gu;

/** A link, `[words](target)`, which reads as its words; its target is no word of the text. */
const LINK = /\[([^\]]*)\]\([^)]*\)/gu;

/** `**strong**`, which stands wherever it is written. */
const STAR_STRONG = /\*\*(?=\S)([\s\S]*?\S)\*\*/gu;

/** `__strong__`, which markdown reads only where no letter or digit stands either side of it. */
const UNDERSCORE_STRONG = /(?<![\p{L}\p{N}])__(?=\S)([\s\S]*?\S)__(?![\p{L}\p{N}])/gu;

/** `~~struck~~`, which stands wherever it is written. */
const STRUCK = /~~(?=\S)([\s\S]*?\S)~~/gu;

/** `*emphasis*`, which stands wherever it is written. */
const STAR_EMPHASIS = /\*(?=\S)([^*]*?\S)\*/gu;

/** `_emphasis_`, which markdown reads only between characters that are no letter or digit, so
 *  `snake_case_name` is one word. */
const UNDERSCORE_EMPHASIS = /(?<![\p{L}\p{N}])_(?=\S)([^_]*?\S)_(?![\p{L}\p{N}])/gu;

/** Each ASCII punctuation character, in code order, as `PUNCTUATION` picks them out of the
 *  printable ASCII characters but the space: the 32 each hold of `Holds` has a stand-in for. */
const PUNCTUATION_CHARACTERS: readonly string[] =
  String.fromCharCode(...Array.from({ length: 0x7f - 0x21 }, (_, at) => 0x21 + at)).match(
    PUNCTUATION
  ) ?? [];

/** The private-use code points, in the order a text's stand-ins are taken from them: the Basic
 *  Multilingual Plane's area, U+E000 to U+F8FF, then plane 15's and plane 16's. */
const PRIVATE_USE: readonly (readonly [first: number, last: number])[] = [
  [0xe000, 0xf8ff],
  [0xf0000, 0xffffd],
  [0x100000, 0x10fffd],
];

/** One private-use character a text holds: an author's own, or a stand-in `Holds` chose. */
const PRIVATE_USE_CHARACTER = /\p{Co}/gu;

/** One hold's stand-ins: each ASCII punctuation character's (`held`), and the character each
 *  stand-in is held for (`back`). */
interface Hold {
  readonly back: ReadonlyMap<string, string>;
  readonly held: ReadonlyMap<string, string>;
}

/**
 * The private-use stand-ins one text's `LITERAL` characters are held as while the marks are
 * taken off, in two holds that `plainWords` puts back at two times. An escaped character
 * (`escaped`) is put back before bare addresses are written short, so `see dispatch://CORE-2\.`
 * reads `see CORE-2.`. A code span's content and an autolink's URL (`verbatim`) are put back
 * after, so an address in code stays as its author wrote it, and an autolink's, written short as
 * it is read, is not read again.
 */
interface Holds {
  readonly escaped: Hold;
  readonly verbatim: Hold;
}

/** The holds of a text `LITERAL` reads nothing of, which hold nothing and put nothing back. */
const NO_HOLDS: Holds = {
  escaped: { back: new Map(), held: new Map() },
  verbatim: { back: new Map(), held: new Map() },
};

/**
 * The holds of `text`, their stand-ins chosen for it alone: the private-use code points in order
 * (`PRIVATE_USE`), skipping each one the text holds, the escaped hold's 32 first and the verbatim
 * hold's 32 next. A stand-in is so never a character the author wrote, and an author's own
 * private-use character - an icon font's, a pasted terminal glyph - reads as written. Undefined
 * for a text that holds so many private-use characters that fewer than 64 are left free.
 */
function holdsFor(text: string): Holds | undefined {
  const free = freePrivateUse(new Set(text.match(PRIVATE_USE_CHARACTER)));
  const hold = (): Hold | undefined => {
    const back = new Map<string, string>();
    const held = new Map<string, string>();
    for (const character of PUNCTUATION_CHARACTERS) {
      const standIn = free.next();
      if (standIn.done === true) return undefined;
      back.set(standIn.value, character);
      held.set(character, standIn.value);
    }
    return { back, held };
  };
  const escaped = hold();
  const verbatim = hold();
  return escaped === undefined || verbatim === undefined ? undefined : { escaped, verbatim };
}

/** Each private-use character, in `PRIVATE_USE`'s order, that is none of `taken`. */
function* freePrivateUse(taken: ReadonlySet<string>): Generator<string, void> {
  for (const [first, last] of PRIVATE_USE) {
    for (let point = first; point <= last; point += 1) {
      const character = String.fromCodePoint(point);
      if (!taken.has(character)) yield character;
    }
  }
}

/** `text` with each stand-in of `hold` the ASCII character it is held for, and every other
 *  character, an author's own private-use one included, as written. */
function putBack(text: string, hold: Hold): string {
  return hold.back.size === 0
    ? text
    : text.replace(PRIVATE_USE_CHARACTER, (character) => hold.back.get(character) ?? character);
}

/**
 * The words of a stored markdown text as a reader reads them, on one line: what a place that
 * draws a stored text as plain words draws, since its marks are markup and no word of it (R21),
 * and a mark after a sentence's end (`today.**`) would hide that end from `firstLineOf`.
 *
 * Each paragraph is read on its own, since no mark pairs across a blank line. What markdown
 * reads literally (`LITERAL`) - an escaped punctuation character (`1\.` is `1.`), a code span's
 * content, an autolink's URL, which reads as a link's words read, its address written short - is
 * read first, and each ASCII punctuation character of it is held as a private-use stand-in while
 * the marks are taken off, so no mark is read inside it. The stand-ins are chosen for each text
 * from the private-use code points it does not hold (`holdsFor`) and read back by what each was
 * chosen for, so an author's own private-use character - an icon font's, a pasted terminal
 * glyph - is no stand-in and reads as written. Then a blockquote's and a list item's markers at
 * the start of any line go (`BLOCK_MARKER`), and a heading marker after them, an image goes
 * whole, a link reads as its words, and strong, emphasis and struck marks go until none is left,
 * so nested marks (`***x***`) go too. The escaped characters are put back, and white space
 * collapses to single spaces.
 *
 * Then a bare `dispatch://` address reads as its target's short form (`addressesAsShortForms`):
 * every place that reads a stored text to plain words draws it on one line - a
 * node's line where the node has no reading (`lineWords`), and the name of a chip naming an
 * ask, a comment or a message (`treeContext`) - and no one-line view draws a raw address (R22).
 * A link already reads as its words, so `[the spec](dispatch://…)` reads `the spec`. Last, the
 * characters of code spans and autolinks are put back, so an address in code reads as its
 * author wrote it, as a rendered body draws it: a code span holding
 * `dispatch read dispatch://CORE-2` keeps its address.
 *
 * A text `LITERAL` reads something of that holds so many private-use characters that fewer than
 * the 64 stand-ins it needs are left free - more than 137,404 of the 137,468 - has nothing to
 * hold its literal characters as, and reads as its own words with white space collapsed and no
 * mark taken off: markup shown is less wrong than characters changed.
 */
export function plainWords(markdown: string): string {
  return plainWordsWith(markdown, addressesAsShortForms);
}

/** The words of a stored markdown text as `plainWords` reads them, with no address written
 *  short: its marks, markers, links, images, escapes and code spans read as they read there, an
 *  author's own private-use character as written, and a bare address and an autolink's URL stay
 *  as written. A caller that names each reference itself reads these, since a short form leaves
 *  it no address to name. */
export function plainWordsRaw(markdown: string): string {
  return plainWordsWith(markdown, (words) => words);
}

/** The words `plainWords` reads, each bare address in them and each autolink's URL written by
 *  `shorten` where `plainWords` writes it short (`addressesAsShortForms`). A text `LITERAL` reads
 *  nothing of holds nothing, so no stand-in is chosen for it and none is put back. */
function plainWordsWith(markdown: string, shorten: (words: string) => string): string {
  const holds = markdown.search(LITERAL) === -1 ? NO_HOLDS : holdsFor(markdown);
  if (holds === undefined) return markdown.replace(/\s+/gu, " ").trim();
  const paragraphs = markdown.split(BLANK_LINE).map((paragraph) => {
    let words = paragraph
      .replace(LITERAL, (written, escaped?: string, _run?: string, code?: string, url?: string) => {
        const hold = escaped === undefined ? holds.verbatim : holds.escaped;
        const literal = escaped ?? code ?? (url === undefined ? undefined : shorten(url));
        return (
          literal?.replace(PUNCTUATION, (character) => hold.held.get(character) ?? character) ??
          written
        );
      })
      .replace(BLOCK_MARKER, "")
      .replace(HEADING_MARKER, "")
      .replace(IMAGE, "")
      .replace(LINK, "$1");
    for (let marked = ""; marked !== words; ) {
      marked = words;
      words = words
        .replace(STAR_STRONG, "$1")
        .replace(UNDERSCORE_STRONG, "$1")
        .replace(STRUCK, "$1")
        .replace(STAR_EMPHASIS, "$1")
        .replace(UNDERSCORE_EMPHASIS, "$1");
    }
    return putBack(words, holds.escaped);
  });
  const words = paragraphs.join(" ").replace(/\s+/gu, " ").trim();
  return putBack(shorten(words), holds.verbatim);
}

/**
 * `words` with each bare `dispatch://` address in them written as its target's short form, as a
 * link to it draws it (`shortForm`): `dispatch://CORE-2` reads `CORE-2`, and
 * `dispatch://ACME-1196/spec#b-r1` reads `ACME-1196 spec block`. It is for the words a place
 * that draws one line for a node cuts that line from, since no one-line view draws a raw
 * address: the server's reading keeps a bare address in a node's words as a plain text run,
 * which is no link, and a stored text holds it as written, so both meet it.
 *
 * An address is found as the page finds a reference in text (`referenceSpans`), and only the
 * address itself is written short: what `trimReference` cuts off its end stays where it stood, so
 * `see dispatch://CORE-2.` reads `see CORE-2.`. A span that names no target
 * (`parseDispatchReference`) is left as written.
 */
export function addressesAsShortForms(words: string): string {
  let shortened = "";
  let cursor = 0;
  for (const { start, value } of referenceSpans(words, "dispatch://")) {
    const route = parseDispatchReference(value);
    if (route === undefined) continue;
    shortened += `${words.slice(cursor, start)}${shortForm(route)}`;
    cursor = start + value.length;
  }
  return shortened + words.slice(cursor);
}

/**
 * A chip that opens each document of the issue a node's own words cite (R3): a document is no
 * node of the outline, so a block that names one carries a chip of its own for it, which opens
 * the document at its own address in place of the outline (`NodeRow.tsx`'s `Chip`). The chip is
 * named by the page's word for the document, as every chip naming a whole document is
 * (`documentWord`): the filter's name for the spec and for the plan, Verification and decisions
 * documents, and "document" for any other, never the file name the issue's own read carries. It
 * addresses the document as `artifact/<slug>` - the spec too, whose `spec` reference is the Spec
 * filter's address. A citation of one block of a document names that block, which the text's
 * own link opens, and no chip.
 */
export function addDocumentChips(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  const bySlug = artifactsBySlug(reads.issue);
  if (bySlug.size === 0) return;
  for (const draft of drafts.values()) {
    // The words a node is drawn by can leave a citation out - the coverage read drops every
    // `://` word of a requirement (`api/results.go`'s `blockWords`) - so the node's own source,
    // the excerpt the reference read names it by, is read as well.
    const source = draft.reference === undefined ? "" : context.excerptOf(draft.reference);
    for (const span of [
      ...referenceSpans(draft.text, "dispatch://"),
      ...referenceSpans(source, "dispatch://"),
    ]) {
      const route = parseNodeReference(span.value);
      if (route === undefined || !("key" in route) || route.key !== context.key) continue;
      if ("block" in route && route.block !== undefined) continue;
      const slug =
        route.kind === "artifact" ? route.slug : route.kind === "spec" ? context.specSlug : "";
      const artifact = bySlug.get(slug);
      if (artifact === undefined) continue;
      const ref = buildDispatchReference({ key: context.key, kind: "artifact", slug });
      if (draft.chips.some((chip) => chip.ref === ref)) continue;
      draft.chips = [
        ...draft.chips,
        { answered: true, gone: false, name: documentWord(reads.issue, artifact), ref },
      ];
    }
  }
}

/** A draft with nothing on it but its id and its type, for a builder to fill in. */
export function blank(id: string, type: NodeType, order: number): Draft {
  return {
    chips: [],
    facts: [],
    gaps: [],
    id,
    order,
    parents: [],
    question: false,
    replaced: false,
    text: "",
    type,
    waiting: false,
  };
}

/** An ask's answer as the tree holds it: the person it is theirs, with the agent they answered
 *  through where they did, and their choices and words as stored. */
export function answerOf(answer: NonNullable<Ask["answer"]>): NodeAnswer {
  return {
    at: answer.at,
    by: { id: answer.user, kind: "user", ...(answer.act === undefined ? {} : { act: answer.act }) },
    selected: answer.selected ?? [],
    text: answer.text ?? "",
  };
}

/**
 * How an ask stands, in one of the words the page declares for an ask (`NODE_STATE_WORDS.ask`):
 * the kind of its resolution for an ask that was closed rather than answered, so the tree says
 * "Declined" where the card and the margin beside it say "Declined", and the `state` the server
 * wrote for every other ask. The server types both as plain strings (`Ask.state`,
 * `AskResolution.kind`), and a token the page declares no word for is drawn as no state rather
 * than as the server's own token (R21).
 */
export function askState(ask: Ask): AskState | undefined {
  const words: Readonly<Record<string, string | undefined>> = NODE_STATE_WORDS.ask;
  const token =
    ask.state === "resolved" && ask.resolution !== undefined ? ask.resolution.kind : ask.state;
  return words[token] === undefined ? undefined : (token as AskState);
}

/**
 * How a check stands where no result of it stands at its basis, in the states
 * `NODE_STATE_WORDS.check` names: `checkStanding`'s answer for such a check, and for a check with
 * no result or no `pass_stands` at all, and read by nothing else, so no place of the page reads
 * a check other than `checkStanding` does. Whether it passed is the server's own answer over
 * every result of the check (`pass_stands`, main's e98fc56ef8): a pass recorded before a later
 * fail still stands, while a retracted pass, a pass recorded against an older wording of the
 * check and a pass at a commit the check no longer compares with stand for nothing. The other
 * states read what no pass standing is:
 *
 * - **`no-result`,** no result recorded at all, which the results say by themselves.
 * - **`unreadable`,** a payload carrying no `pass_stands` at all, which is a server that does
 *   not send the field - an older build behind the same load balancer as the one this page
 *   came from. It is read before everything the answer decides, because without it the page
 *   knows nothing of how the check stands; it is the page's own blindness and no verdict of
 *   the check's.
 * - **`retracted`,** every result of the check withdrawn (`CheckResultRetracted`, which is on
 *   the result and never on the check): nothing stands for it and nothing stands against it,
 *   so neither "Passed" nor "Failed" is true of it, and the withdrawal is what the reader is
 *   owed.
 * - **`older`,** a newest result that stands and no longer speaks for the work as it stands,
 *   whatever it found, because its commit is not one the check compared with or the check's
 *   own words have moved under it.
 * - **`failed`,** a newest result that stands and failed at a commit Dispatch could not place:
 *   a fail at the work the check compares with is `checkStanding`'s own answer, before this.
 * - **`unmarked`,** where nothing could be judged: a repository Dispatch cannot read, or a
 *   commit it cannot place.
 *
 * A retracted result is no evidence, as the server counts it (`results_comparison.go`'s
 * `passStandsOf` skips it), so `older` and `failed` are read from the newest result that still
 * stands and never from a withdrawn one: a pass taken back at the compared commit over an older
 * pass at another commit is `older`, and a pass taken back over a fail that stands is `failed`,
 * whatever commit the withdrawn pass ran at. The newest is read by the time each result was
 * recorded (`newestFirst`) and never by the order the read sent them in, as `checkStanding` and
 * the Verification filter's body read it, so the three name one result whatever that order is.
 *
 * It is how a check stands against the basis the check itself carries, which is the server's
 * own reading (`pass_stands`, `commit_stands`), where no result over the check's words now is at
 * that basis, so one check reads one state wherever it is drawn, each told apart by a word and a
 * shape (R15, R21).
 */
function checkState(check: CheckStateReads): CheckState {
  if ((check.results ?? []).length === 0) return "no-result";
  if (check.pass_stands === undefined) return "unreadable";
  if (check.pass_stands.answer === "yes") return "passed";
  if (check.pass_stands.answer === "unknown") return "unmarked";
  const standing = newestFirst(standingResults(check))[0];
  if (standing === undefined) return "retracted";
  if (standing.check_words_moved === true) return "older";
  return standing.commit_stands?.answer === "other_commit" ? "older" : "failed";
}

/** What a check's state is read from: the server's answer over its results and the results
 *  themselves. `pass_stands` is declared required by the API types and is missing from what
 *  the deployed server sends, so it is optional here and the page reads a payload without it
 *  rather than throwing over the whole issue page. */
export type CheckStateReads = Pick<CheckBlock, "results"> & {
  readonly pass_stands?: CheckBlock["pass_stands"];
};

/** How one check stands, as the server reads the commit each of its results ran at. */
export interface CheckStanding {
  /** How it stands, in the states `NODE_STATE_WORDS.check` names. */
  readonly state: CheckState;
  /** The result that state stands on: the newest result over the check's words now at its
   *  basis, a pass or a fail, where one is there; else the newest result that stands at all,
   *  and none for a check the server says passed with no pass at its basis in the read. Absent
   *  where none stands. */
  readonly stands?: CheckResult;
}

/**
 * How a check stands: the page's one reading of a check's state (R10, R17), and the only one -
 * `checkState` is its own fallback and nothing else reads it. The tree's check node holds it
 * (`tree-read/tree-nodes.ts`' `draftServing`), so the outline's row, the fold's frontier
 * (`tree-fold.ts`) and the Verification filter's failing count (`presets.ts`'s `filterSignals`)
 * read it, and so does the Verification filter's body (`statement-state.ts`'s `checkReading`):
 * one check never reads Passed on its row and Failed in the filter.
 *
 * Each result is read against the commit the server names for its own check - the basis the
 * check compares with, which the result's `commit_stands` answers (`at_basis`, `other_commit`,
 * `unknown`) - as the server's `standingCommit` and `passStandsOf` read it
 * (`results_comparison.go`), and never against one commit the page names for every check: two
 * checks of one issue compare with different commits, a `before_merge` check with the pull
 * request's head and an `after_merge` one with the branch it merged into, so a pass at its own
 * check's basis reads Passed whatever newer result another check holds at another commit, as its
 * own fact reads "Up to date" (`check-facts.ts`'s `resultFacts`). A result stands for something
 * at the basis only where nobody withdrew it and its check's words have not moved since it was
 * recorded.
 *
 * **The latest evidence at the basis decides.** A check with no result reads No result, and one
 * whose payload carries no `pass_stands` reads Could not read, as `checkState` reads them. Else,
 * where a result stands for something at the basis, the newest of them is the check's state: a
 * fail there is Failed, whatever older pass the server still counts as standing (`pass_stands`
 * says yes for any pass at a basis, later fails or not), and a pass there is Passed. A newer
 * result at another commit, or at one Dispatch could not place, says nothing about the basis,
 * so it neither hides a fail there nor turns a pass there over; and a result recorded against
 * words the check no longer has stands for nothing, since it is evidence about a check that no
 * longer reads that way. The results read sends no verdict but `pass` and `fail`
 * (`api/results.go` refuses any other). Where nothing stands at the basis, it is `checkState`'s
 * answer: Out of date for a newest result at another commit or over moved words, Retracted,
 * Cannot tell, or Failed for a newest fail at a commit Dispatch could not place.
 */
export function checkStanding(check: CheckStateReads): CheckStanding {
  const standing = newestFirst(standingResults(check));
  // The newest result that stands for something at the commit the server names for the check:
  // over the check's words now and at its basis (`standingCommit`). A check with no result has
  // none, and one with no `pass_stands` is read before it.
  const here =
    check.pass_stands === undefined
      ? undefined
      : standing.find(
          (result) =>
            result.check_words_moved !== true && result.commit_stands?.answer === "at_basis"
        );
  if (here !== undefined) {
    return { stands: here, state: here.verdict === "pass" ? "passed" : "failed" };
  }
  const state = checkState(check);
  // No pass is at the basis in the read, so a check the server says passed stands on none of
  // its results here.
  const stands = state === "passed" ? undefined : standing[0];
  return { state, ...(stands === undefined ? {} : { stands }) };
}

/** Every result of `check` nobody withdrew, which is every result that stands as evidence: the
 *  server's `passStandsOf` skips a withdrawn one, and so does every reading of the page. */
export function standingResults(check: CheckStateReads): readonly CheckResult[] {
  return (check.results ?? []).filter((result) => result.retracted === undefined);
}

/** `results` newest first, by the time each was recorded. The server already answers newest
 *  first; this reads the times themselves, so the order is the model's own and no caller
 *  depends on the order a read happened to arrive in. */
export function newestFirst(results: readonly CheckResult[]): readonly CheckResult[] {
  return [...results].sort(newerFirst);
}

/** Orders two results newest first by the time each was recorded, to the microsecond the server
 *  writes (`compareTimestamps`), which `Date.parse` would cut to the millisecond and so tie. A
 *  time that cannot be read (`isTimestamp`) is the oldest possible, so a result with no readable
 *  time never outranks one that has one, two such tie, and no time throws. */
function newerFirst(first: CheckResult, second: CheckResult): number {
  const firstRead = isTimestamp(first.created_at);
  const secondRead = isTimestamp(second.created_at);
  if (firstRead && secondRead) return compareTimestamps(second.created_at, first.created_at);
  return Number(secondRead) - Number(firstRead);
}
