import type {
  CommentWithTrees,
  DisplayTree,
  GraphEdge,
  GraphReferences,
  IssueAskRow,
  MessageWithTrees,
} from "../../../api/types";
import { isBlockItemKind } from "../../refs/block-reference";
import { isPartyRoute } from "../../refs/party-references";
import type { KnownReferences } from "../../refs/reference-names";
import type { DispatchReferenceRoute, PartyRoute } from "../../refs/routes";
import { firstWordedBlock, wordsOf } from "../../refs/tree-fragment";
import { drawnLine, firstLineOf } from "../issue-tree";
import { NODE_REFERENCES_WORDS } from "../page-words";
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
 * `dispatch://` address of it, where the page draws a node by the words of the server's reading
 * of it. The page holds those words for the comments, messages and questions of the
 * focused node's own issue, which the outline reads (`issueItemWords`), and names such an item
 * by all of them; and the read sends the server's reading of every other citing node's excerpt
 * (`text_tree`), by whose first line, cut as the outline cuts a row's one line, a block - a task,
 * a statement, a check - and an item of another issue are named (`excerptLine`, decision 560).
 * A reference in those words is named as the outline's row names it, from the page's one
 * batched read of what its references point at, and by the page's word for what it points at
 * where that read names nothing (`namedWords`); a node with no words is named by the word of
 * its tie or of its kind, with its issue and its time.
 *
 * Pure, so the rows are read without a browser: the component draws them and holds the reads.
 */

/** The edge kind that is no tuple of the model: one node's text names another. */
const MENTIONS = "mentions";

/** One edge of the references read as the page holds it: its excerpt carries the server's
 *  reading of its text (`text_tree`), which every request of the page asks for
 *  (`Dispatch-Trees`, `api/client.ts`). */
type ReadEdge = NonNullable<GraphReferences["edges"]>[number];

/** One node that references the focused one. */
export interface ReferenceRow {
  /** When the reference was made (`GraphEdge.created_at`), which the old panel drew on each
   *  edge. */
  readonly at: string;
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
   *  of them, with no cut; every other node whose excerpt the read sent a reading of that holds a
   *  word, by the first line of those words, cut as the outline cuts a row's one line
   *  (`excerptLine`); each reference in either named (`namedWords`). `""` for a node with no
   *  words, which the component names by its word. Never the read's own text.
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
 * was edited from `known.edited` (`KnownReferences.edited`), as the outline's row marks them;
 * every other node carries the first line of its excerpt's reading, named the same way
 * (`excerptLine`), and a node with neither carries none. `issue` is the focused node's own
 * issue, which those items are of, or `undefined` where the focused node belongs to none.
 */
export function referenceRows(
  itemWords: ReadonlyMap<string, string>,
  edges: readonly ReadEdge[],
  known: Pick<KnownReferences, "edited" | "names">,
  issue: string | undefined
): ReferenceRow[] {
  const rows = new Map<string, ReferenceRow>();
  // The issues the page's names know, which a citing node's words tell their references by: read
  // once for the whole list.
  const issues = knownIssueKeys(known.names);
  for (const edge of edges) {
    const item = blockItem(edge.node);
    const nodeKind = item?.kind ?? edge.node.kind;
    const id = item?.id ?? edge.node.id;
    const key = `${nodeKind}:${id}`;
    const held = rows.get(key);
    if (held !== undefined && (held.kind !== MENTIONS || edge.kind === MENTIONS)) continue;
    const words = isBlockItemKind(nodeKind) ? itemWords.get(id) : undefined;
    rows.set(key, {
      at: held?.at ?? edge.created_at,
      key,
      kind: edge.kind,
      nodeKind,
      ref: item?.ref ?? edge.node.ref,
      source: edge.node.issue_key ?? edge.node.project,
      words:
        words === undefined
          ? excerptLine(edge, known, issues)
          : namedWords(words, known, issues, issue),
    });
  }
  return [...rows.values()];
}

/**
 * The first line of a citing node's own words (decision 560), from the server's reading of the
 * read's excerpt of them (`text_tree`): the head of a block's text, a task's, a statement's or a
 * check's, and of an item's, as the graph cuts each (`refs.BlockExcerpt`, `itemExcerpt`). It is
 * the line the outline draws for a node (`lineReading`, `useNodeLine`): the first block of the
 * reading that holds any words (`firstWordedBlock`), cut by R1's first-line rule (`firstLineOf`)
 * with its ellipsis (`drawnLine`), each reference in it named first (`namedWords`), as the
 * outline's line names its references before it cuts, by the citing node's own issue. `""` for a
 * node whose excerpt is no words of its own - an issue's title, a document's name, which is its
 * file's, and a component's title, whose reading the server sends and no page reads
 * (`api/display_trees.go`'s `graphExcerptRead`) - for an excerpt the read sent no reading of, and
 * for a reading none of whose blocks holds a word: such a node is named by its word. Never the
 * excerpt's own text, which is the markdown its author typed.
 */
function excerptLine(
  edge: ReadEdge,
  known: Pick<KnownReferences, "edited" | "names">,
  issues: readonly string[]
): string {
  if (edge.node.kind !== "block" && !isBlockItemKind(edge.node.kind)) return "";
  const reading = edge.excerpt?.text_tree;
  const block = reading === undefined ? undefined : firstWordedBlock(reading);
  if (block === undefined) return "";
  return drawnLine(firstLineOf(namedWords(wordsOf(block), known, issues, edge.node.issue_key)));
}

/**
 * The words a citing node is named by, before any cut: an item's all of them, since a row of a
 * node's references draws an item of the focused node's issue whole, and another node's the
 * first block of them, which its caller cuts to one line (`excerptLine`). Their runs of white
 * space are one space each, and each reference they write is named as the outline's row names it
 * (`rowReferenceWords`), `issue` being the issue the citing node is of: an issue by its key and
 * its title, a party by the name its own read gives it, anything else by its target's words, then
 * ` · edited` where its post's author edited it (`known.edited`). The server's reading of a bare
 * `dispatch://` reference is a link whose words are the address itself, so the words write it as
 * typed; a reference the read has not named is drawn by the page's word for what it names
 * (`unnamedWords`), since an address is no name, and the rest of the words stay. The batched read
 * names no party, so a party nothing names reads as its author wrote it, as the outline's row
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
