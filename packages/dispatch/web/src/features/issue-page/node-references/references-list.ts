import type {
  CommentWithTrees,
  DisplayTree,
  GraphEdge,
  GraphReferences,
  IssueAskRow,
  MessageWithTrees,
  ReferenceTarget,
} from "../../../api/types";
import { isBlockItemKind } from "../../refs/block-reference";
import { isPartyRoute } from "../../refs/party-references";
import type { KnownReferences } from "../../refs/reference-names";
import type { DispatchReferenceRoute, PartyRoute } from "../../refs/routes";
import { wordsOf } from "../../refs/tree-fragment";
import { drawnLine, firstLineOf, lineReading } from "../issue-tree";
import { NODE_REFERENCES_WORDS, type NodeType } from "../page-words";
import { knownIssueKeys, rowReferences, rowReferenceWords } from "../row-references";

/**
 * What references one node, as Details draws it: one row per node that points at the focused
 * one, in the order the graph read answers them, which is newest first.
 *
 * The old page drew this per block of a document, as a disclosure on the Spec, Verification and
 * Plan tabs (`features/refs/ReferencedBy.tsx`'s `BlockReferences`): what serves the block, what
 * needs it, and every mention of it. The new page draws `serves` as the spine - a check and a
 * plan task sit under the requirement they serve - and `needs` as a chip
 * (`tree-drafts.ts`'s `servePlacements`), so those two reach the reader from the outline; a
 * mention of a node reaches them nowhere else, which is what this list is for.
 *
 * A row is named by words the page holds for the citing node, and never by the read's own
 * text: the read's excerpt is the stored text, the markdown its author typed with every
 * `dispatch://` address of it, and a block's excerpt is its first 480 runes, its opening line
 * included (`refs.BlockExcerpt`), so a task that serves many statements has an excerpt of markup
 * alone, and one whose first sentence crosses the 480th rune an excerpt cut inside it (decision
 * 565). The page holds words for the comments, messages and questions of the focused node's own
 * issue, which the outline reads (`issueItemWords`), and names such an item by all of them. Every
 * other citing block - a task, a statement, a check, a code block, and the block of an item of
 * another issue that cites the row - is named from the batched names read of the list's citing
 * blocks (`citingBlockRefs`, the read the page names its references by), whose member for a block
 * is the server's reading of the block's own content (`ReferenceTarget.text_tree`), the reading
 * the outline names a block by (`tree-drafts.ts`'s `TreeContext.treeOf`): by the first line of
 * it, read and cut as the outline reads and cuts a row's one line, and a code block by the first
 * line of its code, as typed (`blockLine`). A reference in those words is named as the outline's
 * row names it, from the page's one batched read of what its references point at, and by the
 * page's word for what it points at where that read names nothing (`namedWords`); a node with no
 * words is named by the word of its tie or of its kind, with its issue and its time.
 *
 * Pure, so the rows are read without a browser: the component draws them and holds the reads.
 */

/** The edge kind that is no tuple of the model: one node's text names another. */
const MENTIONS = "mentions";

/** One edge of the references read as the page holds it. */
type ReadEdge = NonNullable<GraphReferences["edges"]>[number];

/** What a row draws of its node's words: the words, and whether they are a code block's line,
 *  which the component draws as code (`ReferenceRow.code`). */
type RowLine = Pick<ReferenceRow, "code" | "words">;

/** The line of a node the page holds no words of, which the component names by its word. */
const NO_LINE: RowLine = { code: false, words: "" };

/** One node that references the focused one. */
export interface ReferenceRow {
  /** When the reference was made (`GraphEdge.created_at`), which the old panel drew on each
   *  edge. */
  readonly at: string;
  /** Whether `words` is the first line of a code block's code (decision 565), which the
   *  component draws as code, as typed, with no address in it named or linked (`blockLine`). */
  readonly code: boolean;
  /** What the citing node does for the focused one, as the graph names the edge's kind:
   *  `serves`, `needs`, `answers`, `mentions` and the rest. The component draws the page's
   *  word for it, never this token. */
  readonly kind: string;
  /** What kind of node cites, as the graph names it: `block`, `comment`, `ask`, `message`,
   *  `artifact`, `issue`, `component`, `session`. A comment, a message or a question whose block
   *  holds the mention reads as that item. The component draws the page's word for it where the
   *  edge's own kind has none. */
  readonly nodeKind: string;
  /** The citing node's own `dispatch://` address, which the row's link opens; `undefined` for
   *  a node the read names no address for, and such a row is no link. */
  readonly ref: string | undefined;
  /** The citing node itself, which is what makes it one row however many edges it holds. */
  readonly key: string;
  /** The issue the citing node belongs to, or its project where it belongs to none, which the
   *  component draws where the citing node is not of the focused node's own issue (the old
   *  panel's "Comment · LAY-9"); `undefined` where the read names neither. */
  readonly source: string | undefined;
  /** What the row draws of the citing node's words. A comment, a message or a question of the
   *  focused node's own issue whose words the page holds (`issueItemWords`) is named by every one
   *  of them, with no cut; a block - a task, a statement, a check, and the block of an item of
   *  another issue - whose member of the page's batched names read holds a word, by the first line
   *  of the block's own content, read and cut as the outline reads and cuts a row's one line, and
   *  a code block by the first line of its code, as typed (`blockLine`); each reference in either
   *  but a code block's named (`namedWords`). `""` for an issue, a document, a component and a
   *  whole item of another issue, which that read gives no reading of, for a block whose member
   *  has not answered, is gone or holds no word, and for a node with no words: the component
   *  names each by its word. Never the read's own text, nor the member's `name`, which is the
   *  same stored excerpt.
   *  The component draws the whole string as text the page did not author. Three parts of it can
   *  be the page's own words all the same: the word for the kind of a reference the read has
   *  not named (`unnamedWords`, from `NODE_REFERENCES_WORDS.kinds`), ` · edited` after the
   *  name of a reference whose post its author edited (`editedName`, from `POST_MARK_WORDS`,
   *  declared as `CONTROL_LABELS.post_edited`), and the ellipsis of a first line cut inside a
   *  sentence (`drawnLine`), as every one line of a node draws it. Each comes from a rule the page
   *  declares, so none needs a mark of its own to be a declared word. */
  readonly words: string;
}

/** The reads of an issue that hold its comments, messages and questions, as the outline reads
 *  them (`useIssueTree.ts`); each `undefined` while it has no answer. */
export interface IssueItemReads {
  readonly asks: readonly Pick<IssueAskRow, "id" | "question_tree">[] | undefined;
  readonly comments: readonly Pick<CommentWithTrees, "body_tree" | "id">[] | undefined;
  readonly messages:
    | readonly {
        readonly message: Pick<MessageWithTrees, "body_tree" | "id">;
        readonly replies: readonly Pick<MessageWithTrees, "body_tree" | "id">[] | null;
      }[]
    | undefined;
}

/**
 * The words of each comment, message and question of an issue, by its id: the words of the
 * server's reading of its text (`wordsOf`), which its row in the outline draws, so the list
 * names the item as the outline does and with none of the symbols its author typed. An item
 * whose read sent no reading has no words here and is named by its word, as any node the page
 * holds no words of is: its stored text is markup.
 */
export function issueItemWords(reads: IssueItemReads): ReadonlyMap<string, string> {
  const words = new Map<string, string>();
  const add = (id: string, tree: DisplayTree | undefined): void => {
    if (tree !== undefined) words.set(id, wordsOf(tree));
  };
  for (const ask of reads.asks ?? []) add(ask.id, ask.question_tree);
  for (const comment of reads.comments ?? []) add(comment.id, comment.body_tree);
  for (const thread of reads.messages ?? []) {
    add(thread.message.id, thread.message.body_tree);
    for (const reply of thread.replies ?? []) add(reply.id, reply.body_tree);
  }
  return words;
}

/**
 * The ask, comment or message a graph node is a block of: its parent, for a block node naming
 * no document (`ask/<uuid>#b2`, `comment/<uuid>#b2`, `message/<uuid>#b1`); undefined for any
 * other node. The graph resolves a mention inside an item's words to that item's block
 * (`refs/item_block_nodes.go`), and hides the item's own mention whenever one of its blocks
 * holds the link, so this is how a comment that mentions the row reads as a comment and not as
 * a document (`features/refs/ReferencedBy.tsx`'s `blockItem`).
 */
function blockItem(node: GraphEdge["node"]): NonNullable<GraphEdge["node"]["parent"]> | undefined {
  return node.kind === "block" &&
    node.document === undefined &&
    node.parent !== null &&
    isBlockItemKind(node.parent.kind)
    ? node.parent
    : undefined;
}

/**
 * The addresses the page's batched names read is asked for, so the list names its citing blocks
 * (decision 565): the own address of every citing block - a block of a document, and a block of
 * a comment, a message or a question, which is how an item of another issue reaches the list -
 * each once. That read answers a block with the server's reading of its own content
 * (`ReferenceTarget.text_tree`); it gives an issue, a document, a component and a whole item no
 * reading, so none of them is asked for.
 */
export function citingBlockRefs(edges: readonly ReadEdge[]): string[] {
  const refs = new Set<string>();
  for (const edge of edges) {
    if (edge.node.kind === "block" && edge.node.ref !== undefined) refs.add(edge.node.ref);
  }
  return [...refs];
}

/**
 * One row per citing node, in the read's own order.
 *
 * A node that cites the focused one twice - a comment that names it in two of its blocks, a
 * check that both serves it and names it in its words - is one row, because the row is a way
 * to that node and the node is one place: a block of a comment, a message or a question is
 * that item, keyed and named by it. Which of its edges names the row is the one that says the
 * most: a tuple of the model over a mention, since "served by" tells the reader what the node
 * does and a mention tells them only that it was named. The row keeps the place of the first
 * edge either way, so the list stays in the order the read answered.
 *
 * `itemWords` holds the words of the comments, messages and questions of the focused node's own
 * issue by their ids (`issueItemWords`). A citing item among them carries all of its words, with
 * no cut (`namedWords`), its references named from `known.names`, the page's one batched read of
 * what its references point at, by reference (`reference-names.tsx`), and marked where their post
 * was edited from `known.edited` (`KnownReferences.edited`), as the outline's row marks them.
 * Every other citing block carries the line of its member of `targets`, the batched names read's
 * answer for the list's citing blocks (`citingBlockRefs`), empty while that read has not
 * answered, its references named the same way (`blockLine`); and an issue, a document, a
 * component and a whole item of another issue carry none, as a node with no words does. `issue`
 * is the focused node's own issue, which those items are of, or `undefined` where the focused
 * node belongs to none.
 */
export function referenceRows(
  itemWords: ReadonlyMap<string, string>,
  edges: readonly ReadEdge[],
  targets: readonly ReferenceTarget[],
  known: Pick<KnownReferences, "edited" | "names">,
  issue: string | undefined
): ReferenceRow[] {
  const rows = new Map<string, ReferenceRow>();
  // The issues the page's names know, which a citing node's words tell their references by, and
  // the members of the names read by the address each answers: read once for the whole list.
  const issues = knownIssueKeys(known.names);
  const targetOf = new Map(targets.map((target) => [target.ref, target]));
  for (const edge of edges) {
    const item = blockItem(edge.node);
    const nodeKind = item?.kind ?? edge.node.kind;
    const id = item?.id ?? edge.node.id;
    const key = `${nodeKind}:${id}`;
    const held = rows.get(key);
    if (held !== undefined && (held.kind !== MENTIONS || edge.kind === MENTIONS)) continue;
    const words = isBlockItemKind(nodeKind) ? itemWords.get(id) : undefined;
    const target =
      edge.node.kind === "block" && edge.node.ref !== undefined
        ? targetOf.get(edge.node.ref)
        : undefined;
    const line =
      words === undefined
        ? blockLine(target, known, issues, edge.node.issue_key)
        : { code: false, words: namedWords(words, known, issues, issue) };
    rows.set(key, {
      at: held?.at ?? edge.created_at,
      code: line.code,
      key,
      kind: edge.kind,
      nodeKind,
      ref: item?.ref ?? edge.node.ref,
      source: edge.node.issue_key ?? edge.node.project,
      words: line.words,
    });
  }
  return [...rows.values()];
}

/** The type a citing block's line is read as (`lineReading`). The graph names no block's type,
 *  and its line needs none: `lineReading` reads a type only to read the issue's and a child
 *  issue's line from their titles, and reads every other node's line from its reading, as the
 *  outline's row of a block reads it. To this list's reader a block is a document's
 *  (`NODE_REFERENCES_WORDS.kinds.block`). */
const CITING_BLOCK: NodeType = "document";

/** The type the server's reading gives a code block, fenced or indented (`pmdoc`'s
 *  `code_block`), whose text runs are its code as typed. */
const CODE_BLOCK = "code_block";

/** A line ending in a code block's code, in any of the three forms its text can hold. */
const LINE_ENDING = /\r\n?|\n/u;

/**
 * The line a citing block is named by (decisions 560 and 565), from `target`, its member of the
 * page's batched names read: the line the outline draws for the block (`tree-drafts.ts`'s
 * `addDocumentBlocks`, `useNodeLine`), read from the server's reading of the block's own content
 * (`ReferenceTarget.text_tree`), which a typed block's opening line is no part of, by the
 * outline's own rule (`lineReading`): the first top-level block of it that holds any words, so a
 * task that opens on a heading is named by the heading alone. Its words, each reference in them
 * named first (`namedWords`), as the outline's line names its references before it cuts, by the
 * citing block's own `issue`, are cut by R1's first-line rule (`firstLineOf`) with its ellipsis
 * (`drawnLine`). A code block is named by the first line of its code instead, the first that
 * holds any characters, as typed: code is literal, so no address in it is a reference and none
 * is named, and the component draws it as code (`ReferenceRow.code`).
 *
 * No line for a block whose member has not answered, is gone or holds no reading, and for a
 * reading none of whose blocks holds a word: such a block is named by its word. The member's
 * `name` is never read: it is the block's stored excerpt, cut at 480 runes with its opening line
 * (decision 46).
 */
function blockLine(
  target: ReferenceTarget | undefined,
  known: Pick<KnownReferences, "edited" | "names">,
  issues: readonly string[],
  issue: string | undefined
): RowLine {
  if (target === undefined || target.gone || target.text_tree === undefined) return NO_LINE;
  // The line is read from the reading alone: `lineReading` reads `text` only for a node that
  // holds none.
  const reading = lineReading({
    id: target.ref,
    text: "",
    tree: target.text_tree,
    type: CITING_BLOCK,
  });
  if (!("tree" in reading)) return NO_LINE;
  // `lineReading` answers the reading holding that first worded block and nothing else.
  const [block] = reading.tree.content as readonly [DisplayTree];
  if (block.type === CODE_BLOCK) {
    const code = wordsOf(block).split(LINE_ENDING);
    return { code: true, words: code.find((line) => line.trim() !== "")?.trim() ?? "" };
  }
  const words = namedWords(wordsOf(reading.tree), known, issues, issue);
  return { code: false, words: drawnLine(firstLineOf(words)) };
}

/**
 * The words a citing node is named by, before any cut: an item's all of them, since a row of a
 * node's references draws an item of the focused node's issue whole, and a block's the first
 * block of them that holds a word, which its caller cuts to one line (`blockLine`). Their runs of
 * white space are one space each, and each reference they write is named as the outline's row
 * names it (`rowReferenceWords`), `issue` being the issue the citing node is of: an issue by its
 * key and its title, a party by the name its own read gives it, anything else by its target's
 * words, then ` · edited` where its post's author edited it (`known.edited`). The server's reading
 * of a bare `dispatch://` reference is a link whose words are the address itself, so the words
 * write it as typed; a reference the read has not named is drawn by the page's word for what it
 * names (`unnamedWords`), since an address is no name, and the rest of the words stay. The batched
 * read names no party, so a party nothing names reads as its author wrote it, as the outline's row
 * reads it. `issues` is `knownIssueKeys(known.names)`.
 */
function namedWords(
  words: string,
  known: Pick<KnownReferences, "edited" | "names">,
  issues: readonly string[],
  issue: string | undefined
): string {
  const whole = words.replace(/\s+/gu, " ").trim();
  let written = "";
  let at = 0;
  for (const one of rowReferences(whole, issues)) {
    written += whole.slice(at, one.start);
    if ((known.names.get(one.reference) ?? "").trim() === "" && !isPartyRoute(one.route)) {
      written += unnamedWords(one.route);
    } else {
      const named = rowReferenceWords(one, known.names, issue, known.edited);
      written += named.key === undefined ? named.words : `${named.key} ${named.words}`;
    }
    at = one.end;
  }
  return `${written}${whole.slice(at)}`;
}

/**
 * What a citing node's words draw for a reference the page's batched read names nothing for:
 * an issue by its key alone, as the outline's row draws one whose title has not come
 * (`rowReferenceWords`), and anything else by the page's word for its kind, as the list names a
 * node it holds no words of (`NODE_REFERENCES_WORDS.kinds`): a question, a comment or a message
 * by its own word, a block of any of them by its item's, a document, a block of one or an
 * agent's file by the word for a document, and a component by its own. Never its address, its
 * slug, its id or a document's file name.
 */
function unnamedWords(route: Exclude<DispatchReferenceRoute, PartyRoute>): string {
  switch (route.kind) {
    case "ask":
    case "comment":
    case "message":
      return NODE_REFERENCES_WORDS.kinds[route.kind];
    case "document":
      return NODE_REFERENCES_WORDS.kinds[route.item?.kind ?? "artifact"];
    case "agent-artifact":
    case "artifact":
    case "plan":
    case "spec":
    case "verification":
      return NODE_REFERENCES_WORDS.kinds.artifact;
    case "component":
      return NODE_REFERENCES_WORDS.kinds.component;
    default:
      return route.key;
  }
}
