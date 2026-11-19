import type {
  Actor,
  ArtifactBlock,
  Ask,
  AskWithTrees,
  Comment,
  CommentWithTrees,
  DisplayTree,
  IssueCheckResults,
  IssueChild,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
  ReferenceTarget,
} from "../../api/types";
import type { TextSource } from "../refs/text-references";
import { wordsOf } from "../refs/tree-fragment";
import type { LensFacts } from "./lenses/lens-drafts";
import type { GapKind, NodeState, NodeType } from "./page-words";
import { storedReading } from "./stored-reading";
import {
  addDecisions,
  type Draft,
  type TreeContext,
  titleReading,
  treeContext,
  withShortAddresses,
} from "./tree-drafts";
import { askNodeIds, type TreeWords, treeDraftsFrom } from "./tree-read/tree-nodes";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * The issue's node tree, built from the page's reads and nothing else.
 *
 * `buildIssueTree` is pure: it takes one value holding the page's reads and answers the issue's
 * root node. It sends no request, reads no document's text and holds no React, so the fold
 * (`tree-fold.ts`), the rows (`IssueTree.tsx`) and the unit tests all read the same model.
 *
 * ## The tree read
 *
 * The outline draws the issue's tree read (`GET /api/v1/issues/{key}/tree`): each node under
 * the `parent` the read answers, in the read's order, its words from its `text_tree`
 * (`tree-read/tree-nodes.ts`'s `treeDraftsFrom`). The page places and orders no node itself and
 * gives none a second parent, but for the one transcluded
 * placement: the read gives every node one parent, and a check and a plan task stand again under
 * each other requirement their `serves` tuples name (R4). A node whose parent is a kind the page
 * does not draw stands under its nearest drawn ancestor, and under the issue where it has none,
 * as a node like any other and never as an error. Every tuple that places nothing is a chip
 * naming its target.
 *
 * The tree is the issue's conversation (R1, R3): a document, an artifact and a handoff are nodes
 * of the issue and no node of this tree, and a typed block of a document stands where the read
 * places it, its `document` member naming the document it is written in. The issue's children
 * are rows of it, each a node, where the read places them (R12).
 *
 * ## The reads
 *
 * The tree read (`IssueTreeReads.tree`), whose answer the page keeps whole, and the reads that
 * tell what the answer does not, each matched to its node by its address: the issue read, the
 * coverage read, the decisions read, the results read, the asks, and the one batched reference
 * read, which names what a chip points at outside the answer. While decisions are sections of
 * `decisions.md`, the decisions read also draws each decision no node of the answer holds, as
 * the outline drew it from that read (`tree-drafts.ts`'s `addDecisions`). No document's blocks
 * are read.
 */

/** One tie of a node that places nothing, drawn beside the node as its target's name. */
export interface NodeChip {
  /** Whether the batched reference read could not tell which of several targets a short
   *  reference names: two or more carry the id it writes short, and the read answers it gone and
   *  ambiguous (`ReferenceTarget.ambiguous`). The chip draws the reference as written, and the
   *  page publishes the reference as ambiguous for every link beside the tree to draw so too
   *  (`targetNaming`). Absent for every other reference. */
  readonly ambiguous?: true;
  /** Whether the batched reference read has answered for this reference. A target it answered
   *  with no text of its own (`name` empty, or `gone`) is told apart from one whose answer is
   *  still out by this and by nothing else, and the two are drawn differently: the first is
   *  named by its address's short form, the second by the address itself (`NodeRow.tsx`'s
   *  `Chip`). */
  readonly answered: boolean;
  /** Whether nothing answers to this reference now. */
  readonly gone: boolean;
  /** The page's word for the edge this chip is, where the edge has one: a plan task's `needs`
   *  (`BLOCK_ATTRIBUTE_LABELS`). Absent for an edge the chip's own name says enough about. */
  readonly label?: string;
  /** The words shown for the target (`treeContext`): a block's first line, as a row draws one
   *  (R1), cut from the first block of the server's reading of its words (`lineWords`); where the
   *  read answered with no reading or one that holds no words, the page's word for the block's
   *  kind (`NODE_TYPE_NAMES`) for a block of this issue whose document the tree read, and `""`
   *  for another issue's block; a whole document's word on the page, `Spec` and not `spec.md`;
   *  an ask's, a comment's and a message's stored text read to plain words (`plainWords`); an
   *  issue's title as its own row's line reads it (`titleWords`); and anything else's name as
   *  that read answers it. `""` for a block and for anything else until the read answers, and
   *  for a target that is gone. The page publishes this same name for every link beside the tree
   *  to draw (`targetNaming`). */
  readonly name: string;
  /** The target's own `dispatch://` address. */
  readonly ref: string;
  /** Whether the decision holding this chip changed after it first named the target and the
   *  target has not changed since (`DecisionLink.stale`); absent for every other edge. */
  readonly stale?: boolean;
}

/** What the coverage read counts for one requirement, with no identity behind either number. */
export interface NodeCounts {
  readonly checks: number;
  readonly planTasks: number;
}

/** A person's answer to an ask, which is part of the ask and no node of its own: the ask's own
 *  row and body draw it, word for word as the store holds it and never restated (R9). */
export interface NodeAnswer {
  /** When it was given, which the outline's thread read mark compares with the replies'. */
  readonly at: string;
  /** The person it is the answer of, with the agent they answered through where they did. */
  readonly by: Actor;
  /** The options they chose, each as the ask's own words name it. */
  readonly selected: readonly string[];
  /** What they wrote, as stored; `""` where they wrote nothing beside their choice. */
  readonly text: string;
}

/**
 * How a fact's value is drawn in a node's body: a command in a monospace block with its own
 * copy control (R15), a picture loaded through the bytes route the page already serves, one of
 * the page's own words, or the author's own text.
 */
export type FactDrawing = "command" | "picture" | "text" | "word";

/** One name/value fact of a node, which its body draws under its text: what the model holds
 *  about the node that a row has no line for, such as how a check is run and what a result
 *  recorded. The name is the key its label is declared under (`nodeFactLabel`). */
export interface NodeFact {
  readonly draw: FactDrawing;
  readonly name: string;
  readonly value: string;
}

/** One choice an ask offers, as the ask's own words name it (`AskOption`). */
export interface NodeOption {
  readonly description?: string;
  readonly label: string;
}

/** The one line a filter's row draws for its node's parent (R2, R1's first-line rule). */
export interface ParentLine {
  readonly id: string;
  /** The parent's own first line, cut by R1's rule (`firstLineOf`) from the words of its
   *  reading's first block (`lineWords`) and drawn as `drawnLine` reads it: an ellipsis only
   *  where the cut ended inside a sentence. */
  readonly line: string;
  /** The parent's own `dispatch://` address, where the model gives it one: the link a reader
   *  follows to that node, as a chip's is (R18). Absent for a parent that has none. */
  readonly reference?: string;
  readonly type: NodeType;
}

/** One node of the issue's tree. A node drawn under more than one parent is one node per
 *  placement, each naming the others in `alsoUnder`. */
export interface TreeNode {
  /** The other parents this node is also drawn under (R1, R4); empty for a node drawn once. */
  readonly alsoUnder: readonly NodeChip[];
  /** A person's answer to this ask, where it has one. */
  readonly answer?: NodeAnswer;
  /** When the node was made, as the model records it, where it records one: a turn's, an ask's
   *  and a result's creation, a decision's last change, the issue's own creation. A block of a
   *  document carries none, and its row draws no time (R9). */
  readonly at?: string;
  /** Who made it, where the model holds an author; a row draws "Author unknown" where it holds
   *  none (R9). */
  readonly author?: Actor;
  readonly children: readonly TreeNode[];
  readonly chips: readonly NodeChip[];
  /** What the coverage read counts for a requirement. */
  readonly counts?: NodeCounts;
  /** What the model holds about the node that a row has no line for, drawn in its body. */
  readonly facts: readonly NodeFact[];
  readonly gaps: readonly GapKind[];
  /** How many nodes this one holds, at any depth: what a folded row says it holds. */
  readonly held: number;
  /** The node's own address inside the issue: the issue key, `<slug>#b-<block id>`, else the id
   *  the model gives it. */
  readonly id: string;
  /** The child issue a `child_issue` node is, as the issue read carries it: its row draws its
   *  title as its words, how it stands, what blocks it and who holds it, and its key as the tag
   *  (R12, `IssueChildren.tsx`). Absent for every other node. */
  readonly child?: IssueChild;
  /** The person who kept this decision as it reads now (`DecisionKeep.kept_by`), whose name the
   *  row draws after "Kept by" (R2); absent for every other node. */
  readonly keptBy?: Actor;
  /** What a lens, a finding, a risk or a constraint carries beyond these fields, and the chips of
   *  a statement a constraint names; absent on every other node. */
  readonly lens?: LensFacts;
  /** The choices an ask offers, in its own order; absent for every other node and for an ask
   *  that offers none. */
  readonly options?: readonly NodeOption[];
  /** Whether this ask takes more than one of its options, which its answer's picks follow. */
  readonly multiple?: true;
  /** Whether this node is an open question, whoever's turn it is - the reader's, another
   *  person's or an agent's - which the fold unfolds the path to whatever its depth (R6). */
  readonly question: boolean;
  /** The id the tree read gives the node this one was drawn from (`tree-read/tree-nodes.ts`), on
   *  each of its placements, so a later part finds a node by the read's own id; absent on a node
   *  drawn from another read. */
  readonly readId?: string;
  /** The node's own `dispatch://` address, where it has one. */
  readonly reference?: string;
  /** The parent a filter draws as no row, where it draws this node as one (R2): every parent
   *  that is not a kind the filter draws - an ask, a comment, a message, a requirement under
   *  the Plan filter - is one line on its child's row, read from the parent's id, kind and
   *  first line (`presets.ts`). Absent wherever the node's parent is drawn as a row, or is the
   *  issue itself, and on every node of the page with no filter on. */
  readonly parentLine?: ParentLine;
  /** Whether the decision this node holds was replaced, which folds its branch to one line
   *  (R1). */
  readonly replaced: boolean;
  /** The record `text` is, where it is one: an ask, a comment, a message, or the document a
   *  decision is a block of. A body drawn from `tree` links what the text cites against it, as
   *  every other surface drawing the same text does (`MarkdownBody`'s `source`). */
  readonly source?: TextSource;
  /** How it stands, for a type the model gives a state: one of the states the page has a word
   *  for, so a row draws that word and never the token (`NodeState`, R21). A check reads it as
   *  the server reads its results (`tree-drafts.ts`'s `checkStanding`); a `check` block of any
   *  document but the issue's Verification document carries none, since it is no check and the
   *  results read never answers for it (`isCheck`). */
  readonly state?: NodeState;
  /** Whether this requirement is a statement of the issue's spec (`isStatement`). Absent on
   *  every other node, a `requirement` block of any other document among them. */
  readonly statement?: true;
  /** The node's words as the read that carried them holds them: the tree read's `text` for every
   *  node it answers, and a decision's section of `decisions.md` (`Decision.text`) for a decision
   *  of the decisions read, as stored; the issue's key and title for the issue itself. `""` where
   *  the model holds none. */
  readonly text: string;
  /** The server's reading of `text`, where the read that carried the text sends one: the tree
   *  read's `text_tree` for every node it answers, and a decision's (`Decision.text_tree`) for a
   *  decision of the decisions read. The row draws the node's words from it, so stored markdown
   *  is drawn as what it says and the browser parses none of it (R20). A node whose read sends
   *  none draws `text` as stored. Neither the issue nor a child issue carries one: its `text`
   *  names it by its title, which is no markdown, so such a node draws `text` as written, and its
   *  one line is that text with each run of white space one space and each bare address written
   *  short (`lineWords`). */
  readonly tree?: DisplayTree;
  /** Whether this placement is a transclusion rather than the node's edited place. */
  readonly transcluded: boolean;
  readonly type: NodeType;
  /** Whether the model marks this decision unsure, which its row says in one word (R9). */
  readonly unsure?: true;
  /** The view reads the page holds (`?view=`), with the decisions read, on the issue's own node
   *  alone: what each filter draws once its view has answered (`presets.ts`). Absent on every
   *  other node, and on the issue's until a view is held. */
  readonly views?: TreeViews;
  /** Whether this node waits on the reader: a decision waiting for their review, which is where
   *  Keep and Change are drawn and what the Decisions preset opens on (R7), or an open question
   *  whose turn is the reader's own - it waits on a person, and that person
   *  (`Ask.waiting_on_person`) is the reader's login. Nothing waits on a reader whose login has
   *  not resolved. The header's count of what waits puts the focus on the first of them (R13,
   *  `waiting.ts`). */
  readonly waiting: boolean;
}

/** The view answers the page holds, by filter, and the decisions read the Decisions filter
 *  draws (`TreeNode.views`). */
export interface TreeViews {
  readonly decisions?: IssueDecisions;
  readonly plan?: TreeAnswer;
  readonly spec?: TreeAnswer;
  readonly verification?: TreeAnswer;
}

/**
 * Whether `node` is a statement: a `requirement` block of the issue's own spec, the document the
 * issue read names by `primary_artifact_id`, drawn before the coverage read answers as after it,
 * or a requirement that read holds. A `requirement` block of the plan, Verification or
 * decisions document keeps its type and is a block and nothing more: no row of the Spec filter
 * and none of its count, no context row of the Verification filter and none of its statements,
 * and nothing placed under it (`tree-read/tree-nodes.ts`' `draftServing`, which reads the same
 * rule on the tree read's nodes). The review list is no reader of this rule: it reads a
 * requirement the coverage read holds (`inReviewList`'s callers).
 */
export function isStatement(node: Pick<TreeNode, "statement">): boolean {
  return node.statement === true;
}

/**
 * Whether `node` is a check: a `check` block of the issue's Verification document, the one the
 * issue read names by `verification_artifact_id`, or a check the results read answers for. Each
 * draws a state - `no-result` until the results read answers (`tree-read/tree-nodes.ts`'
 * `draftServing`) - and a `check` block of any other document draws none and is a block and
 * nothing more: no row and no count of the Verification filter, no check of a statement, and no
 * open check of the fold's frontier (`tree-fold.ts`).
 */
export function isCheck(node: Pick<TreeNode, "state" | "type">): boolean {
  return node.type === "check" && node.state !== undefined;
}

/** One document's blocks, as `GET /api/v1/artifacts/{id}/blocks` answers them: what the
 *  Decisions filter reads its Changes' sections from (`decision-review.ts`), while decisions are
 *  sections of `decisions.md`. */
export interface DocumentBlocks {
  readonly artifactId: string;
  readonly blocks: readonly ArtifactBlock[];
}

/** An ask as the tree reads it: the issue read's open asks and the asks read both send the
 *  server's reading of the question beside it, which `Ask` leaves out. */
export type TreeAsk = Ask & Pick<AskWithTrees, "question_tree">;

/** The page's reads, as one value. Every field but the issue is optional, because the tree draws
 *  from the reads that have answered rather than waiting for the last of them. */
export interface IssueTreeReads {
  /** The issue's asks, from the conversation; merged with the issue read's open asks by id. */
  readonly asks?: readonly TreeAsk[];
  readonly coverage?: IssueCoverage;
  readonly decisions?: IssueDecisions;
  /** The issue's comments with each body's reading: what a Change of a decision the tree read
   *  holds no node for is found among (`addDecisions`). */
  readonly comments?: readonly (Comment & Pick<CommentWithTrees, "body_tree">)[];
  readonly issue: IssueDetails;
  /** Each check of the Verification document with the results recorded against it. */
  readonly results?: IssueCheckResults;
  /** What every `dispatch://` address the tree holds points at, in one read. */
  readonly targets?: readonly ReferenceTarget[];
  /** The tree read's answer, every page merged (`tree-read/tree-read.ts`), kept whole: every node
   *  and tuple, those of a kind the page draws no row for among them, which later parts read
   *  from here. Undefined until the last page answers, and while it is, the tree is the issue
   *  alone. */
  readonly tree?: TreeAnswer;
  /** The reader's login, which an open question waits on where its turn is theirs
   *  (`TreeNode.waiting`); undefined until the login resolves, and while it is, nothing waits. */
  readonly viewer?: string;
}

/** How many characters a row's first line may hold (R1). */
export const FIRST_LINE_LIMIT = 100;

/**
 * A leading label: a first word of at most five characters that ends in `.` and holds a digit,
 * as `R1.`, `12.` or `1.2.`. Its dot is read as part of the line and no sentence end, so a line
 * that opens with one runs on to the first sentence end after it (`firstLineOf`). Every place
 * that reads where a line's first sentence ends reads its label by this one rule. It carries no
 * `g` or `y` flag, so `exec` keeps no state between the places that share it.
 */
export const LEADING_LABEL = /^(?=\S*\d)\S{0,4}\.(?=\s|$)/u;

/**
 * The node's own text to the first sentence end or `FIRST_LINE_LIMIT` characters, whichever
 * comes first, cut at a word wherever the limit falls near one. A leading label - a first word
 * of at most five characters that ends in `.` and holds a digit, as `R1.`, `12.` or `1.2.` - is
 * part of the line and no sentence end, so a statement's line runs on to the first sentence end
 * after its label. A line held to the limit that holds no space past its label -
 * one unbroken token, a URL or an identifier - is cut at the limit itself, because there is no
 * word boundary to cut at and the limit is the row's whole budget, and one unit short of it
 * where the limit falls inside a surrogate pair, as `textHead` cuts. `cut` says whether the
 * text carries on past the line, and `sentence` whether the line ends at a sentence end, the
 * first one after any label: the two are what the row draws its ellipsis by (`drawnLine`). A
 * line the limit cut, at a word or at the limit itself, ends at none, whatever its last
 * character: a token cut at the limit may end on a `.`, `!` or `?` of its own, as `x.yyyy` cut
 * after its dot does.
 */
export function firstLineOf(text: string): {
  readonly cut: boolean;
  readonly line: string;
  readonly sentence: boolean;
} {
  const whole = text.replace(/\s+/gu, " ").trim();
  const label = LEADING_LABEL.exec(whole)?.[0] ?? "";
  const rest = /^.*?[.!?](?=\s|$)/u.exec(whole.slice(label.length))?.[0];
  const first = rest === undefined ? undefined : `${label}${rest}`;
  if (first !== undefined && first.length <= FIRST_LINE_LIMIT) {
    return { cut: first.length < whole.length, line: first, sentence: true };
  }
  if (whole.length <= FIRST_LINE_LIMIT) {
    return { cut: false, line: whole, sentence: false };
  }
  const window = whole.slice(0, FIRST_LINE_LIMIT + 1);
  const lastSpace = window.lastIndexOf(" ");
  if (lastSpace > label.length) {
    return { cut: true, line: window.slice(0, lastSpace), sentence: false };
  }
  const last = whole.charCodeAt(FIRST_LINE_LIMIT - 1);
  const end = last >= 0xd800 && last <= 0xdbff ? FIRST_LINE_LIMIT - 1 : FIRST_LINE_LIMIT;
  return { cut: true, line: whole.slice(0, end), sentence: false };
}

/**
 * A first line as a place that draws one line for a node draws it (R1): an ellipsis where the
 * text carries on past a line the cut ended inside a sentence, and none
 * where the line is a whole sentence, whose own mark already ends it - "Thread 1 opens here.…"
 * reads as four dots. What follows a whole sentence is the node's next sentence, which its row
 * or its body holds. Whether the line ends at a sentence end is `firstLineOf`'s `sentence`, and
 * every caller hands it in with the line, since a line cut inside a sentence can end on a `.`,
 * `!` or `?` that ends none: nothing here reads the line's last character to decide it. A line
 * cut just after an ellipsis of the author's own (`wait…`), which `firstLineOf` reads as no
 * sentence end since it often stands inside one, draws no second: `wait……` reads as six dots.
 */
export function drawnLine({
  cut,
  line,
  sentence,
}: {
  readonly cut: boolean;
  readonly line: string;
  readonly sentence: boolean;
}): string {
  return cut && !sentence && !line.endsWith("…") ? `${line}…` : line;
}

/** The node's own words as a reader reads them: the words of the server's reading of its text
 *  where its read sent one (`tree`), so its first line holds none of the symbols its body draws
 *  as formatting, and its text as stored otherwise. A bare address stays in them as written,
 *  never as the short form a line reads (`lineWords`): the body draws these words, the words
 *  filter matches them (`presets.ts`), and the citations the batched reference read is asked to
 *  name are read from them (`useIssueTree.ts`), which a short form would drop. */
export function nodeWords(node: TreeNode): string {
  return node.tree === undefined ? node.text : wordsOf(node.tree);
}

/**
 * The words a node's one line is cut from (R1's first-line rule, `firstLineOf`): the words of
 * the first top-level block of its reading that holds any, a heading or a paragraph, and where
 * the node has no reading the same of its stored text's reading (`storedReading`), in the shape
 * of the server's reading. A heading so ends at its own line and its marker is no word of it, so
 * `## # 1 rule` reads `# 1 rule`, and a list that follows a paragraph with no blank line ends
 * that paragraph, so the paragraph alone is the first block. A block that holds no words - a
 * horizontal rule, a paragraph that is only an image or a piece of HTML - is passed over, or the
 * line would be empty while the node has words. A decision's reading opens with its own `##`
 * heading, and `wordsOf` joins a heading and the paragraph after it with one space, so a line cut
 * from the whole reading ran the heading into its body: "Keep it Keep the model pure.".
 *
 * A stored text is markdown, and its marks are no word of it, so its first block reads as that
 * parse holds it: a plan task stored as `**1\. Stand the harness up.** Run it.` reads
 * `1. Stand the harness up. Run it.`, whose label `firstLineOf` reads as no sentence end, and
 * whose sentence end is no longer hidden behind the `**` that followed it. Those are the words
 * the server's reading holds for every text that holds none of three things: a container
 * directive, a Proof mark span and a bare address holding markup (`STORED_TEXT_PARSE`). Its
 * white space - a code block's line feeds among it - collapses to single spaces.
 *
 * A bare `dispatch://` address in these words reads as its target's short form, as a link to it
 * draws it (`addressesAsShortForms`): the server's reading keeps one as a plain
 * text run, which is no link, a stored text and a title hold it as written, and no one-line view
 * draws a raw address (R22). An address in code stays as its author wrote it, as a rendered body
 * draws it. The first block is written short text run by text run (`firstBlockWords`), from a
 * reading, from a stored text's reading and from a title's alike.
 *
 * A title is read as written, but for its addresses. The `text` of the issue, its key and
 * title, and of a child issue, its title, is no markdown and carries no reading, and the places
 * that draw a whole title draw it as written: the header (`slots/header.tsx`) and the list of an
 * issue's children (`ChildrenTab.tsx`). Its line is read from that text as a reading of one
 * paragraph holding one text run (`tree-drafts.ts`'s `titleReading`), so nothing in it is
 * parsed, each run of white space is one space and the ends are trimmed, and a bare address in
 * it is written short as on every other node's line. So `1. Migrate the table` keeps its label,
 * ``Add `x` flag`` its backticks and `Fix <script> handling` its tag, where a parse would read
 * the first as a list item with no label and leave out the marks of the other two, and the
 * address of ``Fix `x` for dispatch://CORE-3`` is written short beside the backticks it keeps:
 * ``Fix `x` for CORE-3``. A chip naming a child of the issue, and the name the page publishes
 * for it, are these words too (`titleWords`), read from the whole title the issue read holds, so
 * a link to a child draws the title its row draws. A chip naming the issue itself is its title's
 * words by the same rule, without the key its own line opens with; another issue is named by the
 * head of its title the names read gives, its first 240 characters.
 *
 * Every place that draws one line for a node cuts it from these words: a parent's line under a
 * filter (`presets.ts`) and a chip naming a block (`buildIssueTree`) here, and a row's first
 * line, a folded branch, a Contents entry and a breadcrumb once they draw (R2). `nodeWords`
 * stays the whole reading, which is what a body draws and what a node's citations are read
 * from.
 */
export function lineWords(node: Pick<TreeNode, "text" | "tree">): string {
  return lineWordsBy(node, firstBlockWords);
}

/** The words a node's one line is cut from, as `lineWords` walks them but with no address
 *  written short: the first block of a reading, or of a stored text's reading, read by
 *  `firstBlockWordsRaw`, so a bare address stays as written. A caller that names each reference
 *  itself reads these, since a short form leaves it no address to name. A `TreeNode` passes as
 *  it is, and a decision as `{ text, tree: text_tree }`; a title node's line is its text as
 *  `lineWords` reads it, its addresses as written. */
export function lineWordsRaw(node: {
  readonly text: string;
  readonly tree?: DisplayTree;
  readonly type?: NodeType;
}): string {
  return lineWordsBy(node, firstBlockWordsRaw);
}

/** `lineWords`' walk, the one both `lineWords` and `lineWordsRaw` read by: a title node's text
 *  read as a reading of one paragraph holding one text run (`titleReading`; `lineWords`' doc
 *  says why), else the first block of the node's reading, and where the node has none the first
 *  block of its stored text's reading (`storedReading`), each read to words by `firstBlock`, the
 *  white space of a title and of a stored text collapsed. The node holds its stored text's
 *  reading, so it is remembered while the node lives. */
function lineWordsBy(
  node: { readonly text: string; readonly tree?: DisplayTree; readonly type?: NodeType },
  firstBlock: (reading: DisplayTree) => string
): string {
  if (node.type === "issue" || node.type === "child_issue") {
    return firstBlock(titleReading(node.text)).replace(/\s+/gu, " ").trim();
  }
  if (node.tree !== undefined) return firstBlock(node.tree);
  return firstBlock(storedReading(node.text, node)).replace(/\s+/gu, " ").trim();
}

/** The words of the first top-level block of a reading that holds any (`lineWords`), each bare
 *  address outside code in them written as its target's short form (`withShortAddresses`); `""`
 *  for a reading none of whose blocks holds a word. */
function firstBlockWords(reading: DisplayTree): string {
  return firstBlockRead(reading, (block) => wordsOf(withShortAddresses(block)));
}

/** The words of the first top-level block of a reading that holds any, as `firstBlockWords`
 *  reads them but with no address written short (`wordsOf`): a bare address stays as written.
 *  A caller that names each reference itself reads these, since a short form leaves it no
 *  address to name. `""` for a reading none of whose blocks holds a word. */
export function firstBlockWordsRaw(reading: DisplayTree): string {
  return firstBlockRead(reading, wordsOf);
}

/** The words of the first top-level block of `reading` that holds any, each block read to words
 *  by `read`; `""` for a reading none of whose blocks holds a word. */
function firstBlockRead(reading: DisplayTree, read: (block: DisplayTree) => string): string {
  const content = reading.content;
  for (const block of Array.isArray(content) ? (content as DisplayTree[]) : []) {
    const words = read(block);
    if (words.trim() !== "") return words;
  }
  return "";
}

/** The line a chip names a block by, and the page publishes as the block's name: the line a row
 *  draws for it (`drawnLine`), cut from the first block of the block's reading as `lineWords`
 *  cuts a node's, which `tree-drafts.ts` and `tree-read/tree-nodes.ts` are handed rather than
 *  import, since this module imports them. */
function chipLine(reading: DisplayTree): string {
  return drawnLine(firstLineOf(firstBlockWords(reading)));
}

/** The reads as the tree and the page's published names both read them (`treeContext`), with a
 *  block named by `chipLine`: the one call both make, so a chip and a link name one target by
 *  one rule and cannot drift apart. */
function namingContext(reads: IssueTreeReads): TreeContext {
  return treeContext(reads, chipLine);
}

/** What the tree's drafts are read by (`treeDraftsFrom`): the reads' names (`namingContext`)
 *  with the word rules of this module a draft's line and a chip's name are cut by. */
export function treeWordsOf(reads: IssueTreeReads): TreeWords {
  return { ...namingContext(reads), chipLine, lineWords };
}

/**
 * The issue's whole tree, from the reads it was given: the drafts of the tree read
 * (`treeDraftsFrom`), each where the read places it, with the outline's drafts of each decision
 * of today's decisions read that no node of the answer holds (`addDecisions`, while decisions are
 * sections of `decisions.md`). Until the tree read has answered every page
 * (`IssueTreeReads.tree`), the tree is the issue alone, since a node the read places under one
 * not yet read would otherwise be drawn in the wrong place.
 */
export function buildIssueTree(reads: IssueTreeReads): TreeNode {
  const words = treeWordsOf(reads);
  const tree = reads.tree;
  if (tree === undefined) return assemble(reads.issue, [], words.chipOf);
  const drafts = treeDraftsFrom(tree, reads, words);
  addDecisions(drafts, reads, words, () => askNodeIds(drafts, tree, reads));
  return assemble(reads.issue, [...drafts.values()], words.chipOf);
}

/**
 * What the page publishes from the targets of the batched reference read (`reads.targets`) for
 * every link beside the tree to draw (`reference-names.tsx`), each read by the rule a chip naming
 * the target draws by (`namingContext`), so a link and a chip naming one target never disagree.
 * Both are read from one context in one walk over the targets, so they cannot disagree either.
 *
 * - **`names`**: the name of each target that something answers to now, by its address: the
 *   name a chip naming it draws (`NodeChip.name`). A target whose name is `""` is left out: a
 *   target that is gone, which its chip names by `""` (`treeContext`), and another issue's block
 *   the read sent no reading for, where a link that finds no name reads its own
 *   (`useKnownReferenceName`), and an ambiguous short reference, which its chip names by `""`
 *   too and the page publishes in `ambiguous` instead.
 * - **`ambiguous`**: the references the read answered ambiguous: each writes short an id two or
 *   more targets carry, so it names none of them, and a chip naming it says so
 *   (`NodeChip.ambiguous`). The page publishes them beside its names, so a link tells an
 *   ambiguous reference from a gone one, which has no name either: it draws the first as its
 *   author wrote it and links nothing, where it reads the second's own.
 *
 * No reference is in both: an ambiguous chip's name is `""`.
 */
export function targetNaming(reads: IssueTreeReads): {
  readonly ambiguous: ReadonlySet<string>;
  readonly names: ReadonlyMap<string, string>;
} {
  const context = namingContext(reads);
  const ambiguous = new Set<string>();
  const names = new Map<string, string>();
  for (const target of reads.targets ?? []) {
    const chip = context.chipOf(target.ref);
    if (chip.ambiguous === true) ambiguous.add(target.ref);
    if (chip.name !== "") names.set(target.ref, chip.name);
  }
  return { ambiguous, names };
}

/** How many nodes a branch holds below `children`, at any depth. */
export function heldBy(children: readonly TreeNode[]): number {
  return children.reduce((held, child) => held + child.held + 1, 0);
}

/** The order the tree draws siblings in: a draft's own order, then its id. */
function drawOrder(one: Draft, other: Draft): number {
  return one.order - other.order || one.id.localeCompare(other.id);
}

/**
 * The drafts as one tree under the issue: one node per placement, each naming the parents it is
 * also drawn under and counting the nodes it holds. A placement that would sit under itself, or
 * under a node it already holds, is dropped, so placements that ever point in a circle still
 * draw a tree.
 *
 * A parent no draft answers to is no placement at all: a decision naming a question the tree
 * holds no draft for. Rather than dropping the node with its missing parent, it sits under the
 * issue, which is where every node whose parent is absent goes (R4).
 */
export function assemble(
  issue: IssueDetails,
  drafts: readonly Draft[],
  chipOf: (reference: string) => NodeChip
): TreeNode {
  const key = issue.key;
  const byParent = new Map<string, Draft[]>();
  const references = new Map<string, string>();
  const known = new Set(drafts.map((draft) => draft.id));
  /** Where each draft is actually drawn: the parents a draft of its own answers for, and the
   *  issue itself for a draft whose every named parent is absent. */
  const placements = new Map<string, readonly string[]>();
  for (const draft of drafts) {
    references.set(draft.id, draft.reference ?? draft.id);
    // A parent named twice is one placement: a decision whose paragraphs each cite the same
    // question arrives naming it once per paragraph (`api/decisions.go`), and drawing it twice
    // there would count it twice in `held` and give both rows one key (`tree-keys.ts`).
    const named = [...new Set(draft.parents)].filter((parent) => known.has(parent));
    placements.set(draft.id, named.length === 0 ? [key] : named);
  }
  for (const draft of drafts) {
    for (const parent of placements.get(draft.id) ?? []) {
      const siblings = byParent.get(parent);
      if (siblings === undefined) byParent.set(parent, [draft]);
      else siblings.push(draft);
    }
  }
  const childrenOf = (id: string, above: readonly string[]): readonly TreeNode[] =>
    (byParent.get(id) ?? [])
      .filter((child) => child.id !== id && !above.includes(child.id))
      .sort(drawOrder)
      .map((child) => build(child, id, [...above, id]));
  const build = (draft: Draft, parent: string, above: readonly string[]): TreeNode => {
    const children = childrenOf(draft.id, above);
    const parents = placements.get(draft.id) ?? [];
    const others = parents.filter((one) => one !== parent);
    return {
      alsoUnder: others.map((one) => chipOf(references.get(one) ?? one)),
      ...(draft.answer === undefined ? {} : { answer: draft.answer }),
      ...(draft.at === undefined ? {} : { at: draft.at }),
      ...(draft.author === undefined ? {} : { author: draft.author }),
      children,
      chips: draft.chips,
      ...(draft.counts === undefined ? {} : { counts: draft.counts }),
      facts: draft.facts,
      gaps: draft.gaps,
      held: heldBy(children),
      id: draft.id,
      ...(draft.child === undefined ? {} : { child: draft.child }),
      ...(draft.keptBy === undefined ? {} : { keptBy: draft.keptBy }),
      ...(draft.lens === undefined ? {} : { lens: draft.lens }),
      ...(draft.multiple === undefined ? {} : { multiple: draft.multiple }),
      ...(draft.options === undefined ? {} : { options: draft.options }),
      question: draft.question,
      ...(draft.readId === undefined ? {} : { readId: draft.readId }),
      ...(draft.reference === undefined ? {} : { reference: draft.reference }),
      replaced: draft.replaced,
      ...(draft.source === undefined ? {} : { source: draft.source }),
      ...(draft.state === undefined ? {} : { state: draft.state }),
      ...(draft.statement === undefined ? {} : { statement: draft.statement }),
      text: draft.text,
      transcluded: parents.indexOf(parent) > 0,
      ...(draft.tree === undefined ? {} : { tree: draft.tree }),
      type: draft.type,
      ...(draft.unsure === undefined ? {} : { unsure: draft.unsure }),
      waiting: draft.waiting,
    };
  };
  const children = childrenOf(key, [key]);
  return {
    alsoUnder: [],
    at: issue.created_at,
    ...(issue.created_by === null ? {} : { author: issue.created_by }),
    children,
    chips: [],
    facts: [],
    gaps: [],
    held: heldBy(children),
    id: key,
    question: false,
    replaced: false,
    // How Dispatch names an issue everywhere: its key and then its title. The outline draws no
    // row of it - the header is the issue - and a line that names the issue, the breadcrumb's
    // first crumb, names it by its key alone.
    text: `${key} ${issue.title}`,
    transcluded: false,
    type: "issue",
    waiting: false,
  };
}
