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
  MessageRead,
  ReferenceTarget,
} from "../../api/types";
import { type PostDeletion, postDeletion } from "../refs/post-marks";
import type { PublishedReferences, ReferenceNameReading } from "../refs/reference-names";
import type { DispatchReferenceRoute } from "../refs/routes";
import { referenceTargetKind } from "../refs/target-id";
import type { TextSource } from "../refs/text-references";
import { firstWordedBlock, wordsOf } from "../refs/tree-fragment";
import { chipWords } from "./node-row-chip";
import type { GapKind, NodeState, NodeType } from "./page-words";
import { type Draft, titleReading, treeContext, treeDrafts } from "./tree-drafts";

/**
 * The issue's node tree, built from the page's reads and nothing else.
 *
 * `buildIssueTree` is pure: it takes one value holding the page's reads and answers the issue's
 * root node. It sends no request, reads no document's text and holds no React, so the fold
 * (`tree-fold.ts`), the rows (`IssueTree.tsx`) and the unit tests all read the same model. The
 * placement and the names are that one function with that one input, so the day the chain read
 * lands it is the only thing that changes.
 *
 * ## The spine
 *
 * The `from` edge is the single placement authority: a decision sits under the question it
 * settles (its entry's `from_ask`), a reading under the ask or the decision it reads (its
 * `part_of`), a requirement under the node its `from` names, a check and a plan task under each
 * requirement their `serves` names (edited under the first, transcluded under the rest), and a
 * result under its check. A decision's `shaped` edge - the decisions read's links
 * (`DecisionEntry.shaped`) - is that decision's chip and places nothing; where a requirement's
 * `from` names one node and a decision that is not that node says it shaped the requirement, the
 * two disagree, the node is placed by `from` and the disagreement is drawn as a gap. A node whose
 * spine parent is absent sits under the issue, as a node like any other and never as an error:
 * no gap says so, because under today's reads every node of its kind lacks one, and a lack every
 * sibling shares tells none of them apart (R10). Every edge that is not the spine is a chip
 * naming its target, and a node is drawn again only under another parent its own edge names.
 *
 * The tree is the issue's conversation (R1, R3): a document, an artifact and a handoff are nodes
 * of the issue and no node of this tree, and a typed block of a document sits under the issue
 * unless the model gives it a parent of its own (R4). The issue's children are one group of rows,
 * each child a node, under the issue (R12).
 *
 * ## What today's reads hold, and what they do not
 *
 * Today's input forces two departures from the rules above, each read at the file and line
 * named, and each one `tree-drafts.ts` says again where it acts:
 *
 * - **No `from` on a requirement.** The `requirement` block carries no attributes at all
 *   (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:70-75`, read 2026-11-05), so
 *   every requirement of the issue's spec sits under the issue. A `requirement` block of any
 *   other document sits there too: the model holds no `from` for it and no coverage row either,
 *   so it is a block and nothing more, and no statement (`isStatement`); so is a `check` block of
 *   any document but the Verification document, which is no check (`isCheck`). With no `from`,
 *   no `shaped_disagreement` is found in today's data either.
 * - **No identities behind a requirement's coverage.** The coverage read counts the serving
 *   checks and plan tasks without naming them
 *   (`packages/contracts/src/dispatch-api.ts:805-816`), so a requirement draws those two counts
 *   and coverage's own gaps; what serves it is placed from each check's and each plan task's own
 *   `serves` pointer instead, which is the same edge read from the other end.
 *
 * ## The reads
 *
 * The issue read, the coverage read, the decisions read, the results read, the conversation,
 * the blocks of the spec and of the plan and Verification documents, and the one
 * batched reference read. A document's blocks answer block ids, types and attributes and never
 * text (`ArtifactBlock`), which is how the tree holds a `serves` edge without reading a document
 * the reader has not opened; a block's own words come from the batched reference read, whose
 * `name` for a block is that block's stored excerpt and whose `text_tree` is the server's
 * reading of the block's whole text. It is one read per document, never one per node.
 */

/** One edge of a node that is not the spine, drawn beside the node as its target's name. */
export interface NodeChip {
  /** Whether the batched reference read could not tell which of several targets a short
   *  reference names: two or more carry the id it writes short, and the read answers it gone and
   *  ambiguous (`ReferenceTarget.ambiguous`). The chip draws the reference as written, and the
   *  page publishes the reference as ambiguous for every link beside the tree to draw so too
   *  (`targetNaming`). Absent for every other reference. */
  readonly ambiguous?: true;
  /** Whether the batched reference read has answered for this reference. A target it answered
   *  with no words of its own (no `reading`, or `gone`) is told apart from one whose answer is
   *  still out by this and by nothing else. */
  readonly answered: boolean;
  /** When the author of the comment or message this chip names, or of the post a block it names
   *  is of, deleted it, as the batched reference read answers it (`ReferenceTarget.deleted_at`);
   *  absent for every other target. The deleted line is the reading of a comment's or a
   *  message's chip only: a chip naming a block of a deleted post is `gone`, with no reading,
   *  though it carries the marks. A view that draws a comment's or a message's chip carrying this
   *  draws the deleted line in its italic, as a link to the post does (`RefLink`), and names the
   *  actor from `deletedBy` as the page names an author, as a row's chip does (`NodeRow.tsx`'s
   *  `Chip`, issue page decision 342). */
  readonly deletedAt?: string;
  /** Who deleted it (`ReferenceTarget.deleted_by`); absent where `deletedAt` is absent, and where
   *  the read names nobody. */
  readonly deletedBy?: Actor;
  /** When the author of the comment or message this chip names, or of the post a block it names
   *  is of, last replaced its body (`ReferenceTarget.edited_at`); absent for a post nobody
   *  edited, for a deleted one and for every other target. The chip's `reading` is of the words
   *  alone: a view that draws the name of a chip carrying this must cut the words to its own
   *  length first and then add ` · edited`, so no cut takes the mark: the row's chip draws the
   *  mark as an element of its own (`NodeRow.tsx`'s `Chip`, with `EditedMark`), and a place that
   *  names the post in one string writes it with `editedName`. */
  readonly editedAt?: string;
  /** Whether nothing answers to this reference now. */
  readonly gone: boolean;
  /** The page's word for the edge this chip is, where the edge has one: a plan task's `needs`
   *  (`BLOCK_ATTRIBUTE_LABELS`). Absent for an edge the chip's own name says enough about. */
  readonly label?: string;
  /** How the target is named, by the batched reference read and the issue's own documents
   *  (`treeContext`): the reading the page publishes for it, which the shared reader projects
   *  into the name every surface reads the target by, as a link to it reads it
   *  (`refs/reference-names.tsx`), and which a chip cuts again where it draws it (`chipName`). A
   *  block by the server's reading of its whole text, as a link to it titles it, which a chip
   *  names by its first block that holds any words (`chipLineReading`); where the read answered
   *  with no reading or one that holds no words, the page's word for the block's kind
   *  (`NODE_TYPE_NAMES`) for a block of this issue whose document the tree read; a whole
   *  document by the title a link to it reads, the issue's title for its spec and its file name
   *  for any other, with the page's word for it, which a chip draws in place of that title
   *  (`ReferenceNameReading.chipWord`); an ask's, a comment's and a message's stored text, and a
   *  deleted comment's or message's deleted line as words (`deletedLineText`), never its emptied
   *  body: `Deleted by <actor>`, the actor named as `AgentName` names one before the agent
   *  registry answers (`actorLabelText`), or `Deleted` where the read names nobody; an issue's
   *  title; and anything else's name as that read answers it. The reading is of the words alone,
   *  an edited post's too: its mark is `editedAt`'s. Absent for a block and for anything else
   *  until the read answers, for another issue's block the read sent no words for, and for a
   *  target that is gone. */
  readonly reading?: ReferenceNameReading;
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
  /** The server's reading of each of `selected`, at the same index, where the read sent one: the
   *  label as the answer took it, which the ask may have renamed since. */
  readonly selectedTrees?: readonly DisplayTree[];
  /** What they wrote, as stored; `""` where they wrote nothing beside their choice. */
  readonly text: string;
  /** The server's reading of `text`, where the read sent one. */
  readonly textTree?: DisplayTree;
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

/** One choice an ask offers, as the ask's own words name it (`AskOption`), with the server's
 *  reading of each of its texts where the read sent one. */
export interface NodeOption {
  readonly description?: string;
  readonly descriptionTree?: DisplayTree;
  readonly label: string;
  readonly labelTree?: DisplayTree;
}

/** The node a filter's row names on one line for its node's parent (R2, R1's first-line rule):
 *  what that line is projected from, the parent's own words, which the row draws as every one
 *  line of a node is drawn (`lineReading`). */
export interface ParentLine {
  /** When its author deleted the parent, a comment or a message (`TreeNode.deletedAt`); absent
   *  for every other parent. Its `text` is then the deleted line as the model writes it, and the
   *  child's row draws the line from the delete instead, as the parent's own row does
   *  (`node-line.tsx`'s `DeletedLine`, deleted-post R3). */
  readonly deletedAt?: string;
  /** Who deleted the parent (`TreeNode.deletedBy`); absent where `deletedAt` is, and where the
   *  read names nobody. */
  readonly deletedBy?: Actor;
  /** When its author last edited the parent, a comment or a message (`TreeNode.editedAt`),
   *  which the child's row says ` · edited` after the line for (`EditedMark`, INFRA-1523 R2);
   *  absent for every other parent. */
  readonly editedAt?: string;
  readonly id: string;
  /** The parent's own `dispatch://` address, where the model gives it one: the link a reader
   *  follows to that node, as a chip's is (R18). Absent for a parent that has none. */
  readonly reference?: string;
  /** The record the parent's words are (`TreeNode.source`). */
  readonly source?: TextSource;
  /** The parent's words as its read holds them (`TreeNode.text`). */
  readonly text: string;
  /** The server's reading of the parent's words (`TreeNode.tree`). */
  readonly tree?: DisplayTree;
  readonly type: NodeType;
}

/** The line a filter's row names `node` by on its child's row (`ParentLine`): its id, its kind,
 *  its address where it has one, the words a row projects its one line from, and a comment's or
 *  a message's marks, which the child's row draws with the line (deleted-post R2 and R3). */
export function parentLineOf(node: TreeNode): ParentLine {
  return {
    ...(node.deletedAt === undefined ? {} : { deletedAt: node.deletedAt }),
    ...(node.deletedBy === undefined ? {} : { deletedBy: node.deletedBy }),
    ...(node.editedAt === undefined ? {} : { editedAt: node.editedAt }),
    id: node.id,
    ...(node.reference === undefined ? {} : { reference: node.reference }),
    ...(node.source === undefined ? {} : { source: node.source }),
    text: node.text,
    ...(node.tree === undefined ? {} : { tree: node.tree }),
    type: node.type,
  };
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
  /** The coverage read's own lines a requirement draws in its body where its gaps are, each in the
   *  page's words (`coverageNotes`): while a document of the issue is still being indexed, that
   *  document's line, which the server writes in place of every missing check, so the requirement
   *  is drawn lacking none and says why. Absent where the read holds none. */
  readonly coverageNotes?: readonly string[];
  /** When its author deleted this comment or message, as its read stores the time
   *  (`deleted_at`); absent on every other node. A deleted post keeps its place in the tree and
   *  holds no part of its body: its `text` is the deleted line (`tree-drafts.ts`'s
   *  `deletedPostLine`) and it carries no `tree` (INFRA-1523, R3). */
  readonly deletedAt?: string;
  /** Who deleted this comment or message (`deleted_by`); absent on every other node, and on a
   *  delete whose read names nobody. */
  readonly deletedBy?: Actor;
  /** When its author last replaced this comment's or message's body (`edited_at`), which a view
   *  that draws the node's row must say as ` · edited` (`EditedMark`, INFRA-1523, R2); absent on
   *  a post nobody edited, on a deleted one, whose deleted line already says what became of the
   *  text, and on every other node. */
  readonly editedAt?: string;
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
  /** The choices an ask offers, in its own order; absent for every other node and for an ask
   *  that offers none. */
  readonly options?: readonly NodeOption[];
  /** Whether this ask takes more than one of its options, which its answer's picks follow. */
  readonly multiple?: true;
  /** Whether this node is an open question, whoever's turn it is - the reader's, another
   *  person's or an agent's - which the fold unfolds the path to whatever its depth (R6). */
  readonly question: boolean;
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
  /** The node's words as the read that carried them holds them, which is not the same for every
   *  kind: the issue's title, its key being its id (`assemble`); a child issue's title; an ask's
   *  question, a comment's and a message's body, and a decision's own words, its `decision`
   *  block's content (its entry's `text` with the lines that open and close the block left out),
   *  each as stored; a deleted comment's and message's deleted line in place of its body
   *  (`deletedAt`); a result's `observed`, as recorded; a statement's the coverage read's plain
   *  words of its block, and a check's the results read's, each block's text runs joined by single
   *  spaces with the words of every link and every `dispatch://` reference left out (`blockWords`,
   *  `packages/envoy/internal/dispatch/api/results.go`); and every other block's - a statement's
   *  and a check's too until their read answers - the batched reference read's stored excerpt of
   *  it (`ReferenceTarget.name`), its source cut at 480 runes with a typed block's directive left
   *  out (`TreeContext.excerptOf`), and `""` until that read answers for it or where its target is
   *  gone. `""` where the model holds none. */
  readonly text: string;
  /** The server's reading of the node's words, where the read that carried them sends one. For
   *  an ask, a comment, a message and a decision it is the reading of `text` itself: the question
   *  (`question_tree`), the body (`body_tree`), the decision's own content (its entry's
   *  `text_tree` with its block's content at the top level). For a block it is the reading of the
   *  block's whole stored text and never of `text`, which for a block is an excerpt or plain words
   *  with its references left out: a statement's on the coverage read and a check's on the results
   *  read (`text_tree`), else the one its block carried, and every other
   *  block's on the batched reference read (`ReferenceTarget.text_tree`). The row draws the node's
   *  words from it, so stored markdown is drawn as what it says and the browser parses none of it
   *  (R20). A result's is the reading of its observed text (`observed_tree`), which a server of
   *  the build before sends none of; a result whose read sent none draws its stored `text` read
   *  as markdown. A deleted comment or message carries none (`deletedAt`): its `text` is the
   *  deleted line, and no reading of its body. Neither the issue nor a child issue carries one:
   *  its `text` names it by its title, which is no markdown, so such a node draws `text` as
   *  written, and its one line is projected from that text read as one text run
   *  (`lineReading`). */
  readonly tree?: DisplayTree;
  /** Whether this placement is a transclusion rather than the node's edited place. */
  readonly transcluded: boolean;
  readonly type: NodeType;
  /** Whether the model marks this decision unsure, which its row says in one word (R9). */
  readonly unsure?: true;
  /** Whether this node waits on the reader: a decision waiting for their review, which is where
   *  Keep and Change are drawn and what the Decisions preset opens on (R7), or an open question
   *  whose turn is the reader's own - it waits on a person, and that person
   *  (`Ask.waiting_on_person`) is the reader's login. Nothing waits on a reader whose login has
   *  not resolved. The header's count of what waits puts the focus on the first of them (R13,
   *  `waiting.ts`). */
  readonly waiting: boolean;
}

/**
 * Whether `node` is a statement: a `requirement` block of the issue's own spec, the document the
 * issue read names by `primary_artifact_id`, drawn before the coverage read answers as after it,
 * or a requirement that read holds. A `requirement` block of the plan, of the Verification
 * document or of any other document keeps its type and is a block and nothing more: no row of
 * the Spec filter and none of its count, no context row of the Verification filter and none of
 * its statements, and nothing placed under it (`tree-drafts.ts`'s `servePlacements`, which
 * reads the same field on its drafts). The review list is no reader of this rule: it reads a
 * requirement the coverage read holds (`inReviewList`'s callers).
 */
export function isStatement(node: Pick<TreeNode, "statement">): boolean {
  return node.statement === true;
}

/**
 * Whether `node` is a check: a `check` block of the issue's Verification document, the one the
 * issue read names by `verification_artifact_id`, or a check the results read answers for. Each
 * draws a state - `no-result` until the results read answers (`tree-drafts.ts`'s
 * `addDocumentBlocks`) - and a `check` block of any other document draws none and is a block and
 * nothing more: no row and no count of the Verification filter, no check of a statement, and no
 * open check of the fold's frontier (`tree-fold.ts`).
 */
export function isCheck(node: Pick<TreeNode, "state" | "type">): boolean {
  return node.type === "check" && node.state !== undefined;
}

/** One document's blocks, as `GET /api/v1/artifacts/{id}/blocks` answers them. */
export interface DocumentBlocks {
  readonly artifactId: string;
  readonly blocks: readonly ArtifactBlock[];
}

/** An ask as the tree reads it: the issue read's open asks and the asks read both send the
 *  server's reading of the question, of each option's label and description, of the answer and
 *  of the reason it was closed with beside them, which `Ask` leaves out. */
export type TreeAsk = Omit<Ask, "answer" | "options" | "resolution"> &
  Pick<AskWithTrees, "answer" | "options" | "question_tree" | "resolution">;

/** The page's reads, as one value. Every field but the issue is optional, because the tree draws
 *  from the reads that have answered rather than waiting for the last of them. */
export interface IssueTreeReads {
  /** The issue's asks, from the conversation; merged with the issue read's open asks by id. */
  readonly asks?: readonly TreeAsk[];
  readonly coverage?: IssueCoverage;
  /** The decisions read. The tree reads its `entries` and nothing else of it: each agent's
   *  decision, a `decision` block of any document of the issue, and each answered ask, which is a
   *  person's decision (`tree-drafts.ts`'s `addDecisions`, `isDecisionEntry`), with who decided
   *  each agent's decision (`decided_by`), which is the decision's author, and the questions it
   *  settles (`from_ask`); and the readings every entry lists, which place a reading under the
   *  node its `part_of` reaches (`addReadingParents`). */
  readonly decisions?: IssueDecisions;
  /** The issue's comments, each a node of the tree (`addTurns`), with each body's reading. */
  readonly comments?: readonly (Comment & Pick<CommentWithTrees, "body_tree">)[];
  /** One entry per document of the issue whose blocks the page has read. */
  readonly documents?: readonly DocumentBlocks[];
  readonly issue: IssueDetails;
  /** The issue's messages with their replies, each a node of the tree (`addTurns`). */
  readonly messages?: readonly MessageRead[];
  /** Each check of the Verification document with the results recorded against it. */
  readonly results?: IssueCheckResults;
  /** What every `dispatch://` address the tree holds points at, in one read. */
  readonly targets?: readonly ReferenceTarget[];
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
 * after its label. `lead` is the page's own word the line opens with, before the node's own
 * words `text` and none of them - the issue's key before its title (`lineReading`) - which is
 * written before them here and counts towards the limit, and the label is read at the start of
 * `text`: `CORE-1 1. Migrate the table` keeps its `1.`. A line held to the limit that holds no
 * space past its label - one unbroken token, a URL or an identifier - is cut at the limit
 * itself, because there is no word boundary to cut at and the limit is the row's whole budget,
 * and one unit short of it where the limit falls inside a surrogate pair, as `textHead` cuts.
 * `cut` says whether the text carries on past the line, and `sentence` whether the line ends at
 * a sentence end, the first one after any label: the two are what the row draws its ellipsis by
 * (`drawnLine`). A line the limit cut, at a word or at the limit itself, ends at none, whatever
 * its last character: a token cut at the limit may end on a `.`, `!` or `?` of its own, as
 * `x.yyyy` cut after its dot does.
 */
export function firstLineOf(
  text: string,
  lead = ""
): {
  readonly cut: boolean;
  readonly line: string;
  readonly sentence: boolean;
} {
  const own = text.replace(/\s+/gu, " ").trim();
  const opening = lead === "" ? "" : `${lead} `;
  const whole = `${opening}${own}`.trimEnd();
  const head = `${opening}${LEADING_LABEL.exec(own)?.[0] ?? ""}`;
  const rest = /^.*?[.!?](?=\s|$)/u.exec(whole.slice(head.length))?.[0];
  const first = rest === undefined ? undefined : `${head}${rest}`;
  if (first !== undefined && first.length <= FIRST_LINE_LIMIT) {
    return { cut: first.length < whole.length, line: first, sentence: true };
  }
  if (whole.length <= FIRST_LINE_LIMIT) {
    return { cut: false, line: whole, sentence: false };
  }
  const window = whole.slice(0, FIRST_LINE_LIMIT + 1);
  const lastSpace = window.lastIndexOf(" ");
  if (lastSpace > head.length) {
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
 *  where its read sent one (`tree`), and its text as stored otherwise; the issue's, its key and
 *  then its title, as Dispatch names an issue everywhere (`assemble`). A bare address stays in
 *  them as written: nothing draws these words - the body draws the reading itself - and they
 *  are what the words filter matches (`presets.ts`) and what the citations the batched reference
 *  read is asked to name are read from (`useIssueTree.ts`), which a name in an address's place
 *  would drop. */
export function nodeWords(node: TreeNode): string {
  if (node.type === "issue") {
    // The issue's title, joined to its key for the words alone (`surface-lists.test.ts`'s list
    // of what holds no draw).
    const title = node.text;
    return `${node.id} ${title}`;
  }
  return node.tree === undefined ? node.text : wordsOf(node.tree);
}

/** What a node's one line is projected from (`lineReading`): a reading, with the issue's key
 *  the page writes before the words it projects to (`key`); the stored text, where the node
 *  carries no reading, which the line parses once (`useNodeLine`) and the projection reads the
 *  first line of (`usePlainHeadline`'s `firstLine`); or nothing, for a reading none of whose
 *  blocks holds a word. */
export type LineReading =
  | { readonly key?: string; readonly tree: DisplayTree }
  | { readonly markdown: string }
  | { readonly none: true };

/**
 * What a node's one line is projected from by the shared plain-text projection, which R1's
 * first-line rule (`firstLineOf`) then cuts and `drawnLine` draws: every place that draws one line
 * for a node draws it from here - a folded branch's line, a parent's line under a filter and a
 * crumb above a zoom (`node-line.tsx`). The projection reads every reference in it as a rendered
 * body does: as its target's current name, and as the words its author wrote until it resolves
 * (R8, issue page decision 350); and it reads no list or block marker as a word, but for an
 * ordered list's number (decisions 66 and 88).
 *
 * - **A node with a reading** by the first top-level block of it that holds any words, a heading
 *   or a paragraph (`firstWordedBlock`). A heading so ends at its own line and its marker is no
 *   word of it, so `## # 1 rule` reads `# 1 rule`. A block that holds no words - a horizontal
 *   rule, a paragraph that is only an image - is passed over, or the line would be empty while
 *   the node has words, and a reading none of whose blocks holds a word reads as none (`none`).
 *   A decision's content opens with its own heading where it has one, and a line cut from the
 *   whole reading ran the heading into its body: "Keep it Keep the model pure.". A decision
 *   passes as its own content (`decision-review.ts`'s `decisionContent`).
 * - **The issue and a child issue** by its title, which is no markdown and carries no reading:
 *   read as one text run (`titleReading`), so nothing in it is parsed and `1. Migrate the table`
 *   keeps its label, ``Add `x` flag`` its backticks and `Fix <script> handling` its tag. The
 *   issue's own node holds its title as its text and its key as its id (`assemble`); its key is
 *   the page's word for it and none of the title's words, so the title alone is projected and the
 *   key is written before its line (`key`), where R1's rule reads a label after it
 *   (`firstLineOf`'s `lead`) and no name of the issue takes its place.
 * - **A node whose read sent no reading** - a result from a server of the build before - by its
 *   stored text, which the line parses once for as long as it draws it (`node-line.tsx`'s
 *   `useNodeLine`), the projection reading the parse's first line.
 */
export function lineReading(node: {
  readonly id: string;
  readonly text: string;
  readonly tree?: DisplayTree | undefined;
  readonly type: NodeType;
}): LineReading {
  if (node.type === "issue") return { key: node.id, tree: titleReading(node.text) };
  if (node.type === "child_issue") return { tree: titleReading(node.text) };
  if (node.tree === undefined) return { markdown: node.text };
  const block = firstWordedBlock(node.tree);
  return block === undefined ? { none: true } : { tree: block };
}

/** The words a chip draws for a target `name` names (ruling 5): a block's first line, as a row
 *  draws one (R1, `firstLineOf`, `drawnLine`), and any other target's name, each cut to a
 *  chip's room (`chipWords`). The page publishes every name as a link to its target reads it
 *  (R8), so a tie's chip (`NodeRow.tsx`), a block or a document drawn as a chip in a node's words
 *  (`row-references.ts`), the Plan chart's needs and serves (`plan-filter/PlanFilterBody.tsx`)
 *  and a check's "Also under" line (`verification-filter/VerificationFilterBody.tsx`) each cut it
 *  again here, where they draw it, and draw one target alike. */
export function chipName(route: DispatchReferenceRoute | undefined, name: string): string {
  const block = route !== undefined && referenceTargetKind(route) === "block";
  return chipWords(block ? drawnLine(firstLineOf(name)) : name);
}

/** The issue's whole tree, from the reads it was given. */
export function buildIssueTree(reads: IssueTreeReads): TreeNode {
  const context = treeContext(reads);
  return assemble(reads.issue, [...treeDrafts(reads, context).values()], context.chipOf);
}

/**
 * What the page publishes from the targets of the batched reference read (`reads.targets`) for
 * every surface beside the tree to name them by (`refs/reference-names.tsx`), each read by the
 * rule a chip naming the target is named by (`treeContext`), so a link and a chip naming one
 * target never disagree, save that a chip draws the page's word for a whole document in place
 * of its name (`ReferenceNameReading.chipWord`): the chip of each answered target, named by
 * `chipNaming`.
 */
export function targetNaming(reads: IssueTreeReads): Omit<PublishedReferences, "asked"> {
  const context = treeContext(reads);
  return chipNaming((reads.targets ?? []).map((target) => context.chipOf(target.ref)));
}

/**
 * How the page names each target the batched reference read answered, by the chip naming it
 * (`targetNaming`), under the chip's own reference (`NodeChip.ref`). Each reference is in one of
 * the four - `readings`, `ambiguous`, `gone`, `unnamed` - read from its chip in one walk, so the
 * four cannot disagree, and the marks of a named post are read in the same walk.
 *
 * - **`readings`**: how each target that something answers to now is named, by its address: the
 *   reading of a chip naming it (`NodeChip.reading`), which the store projects.
 * - **`ambiguous`**: the references the read answered ambiguous: each writes short an id two or
 *   more targets carry, so it names none of them, and a chip naming it says so
 *   (`NodeChip.ambiguous`). A surface draws one as its author wrote it and links nothing.
 * - **`gone`**: the references the read answered that nothing answers to now, an ambiguous one
 *   apart, which a surface draws as the words its author wrote (R8) and reads no target for.
 * - **`unnamed`**: the references the read answered with nothing to name them by - another
 *   issue's block it sent no words for - which a surface reads for itself, as a link to the
 *   target does (R8).
 * - **`edited`**: the references among `readings` whose post its author edited, a comment, a
 *   message or a block of either (`NodeChip.editedAt`). A link that draws a published name cuts
 *   it to its own length and then marks it ` · edited`, so no cut takes the mark: a mark
 *   published inside the name would be cut off a long one. A row's link names the post in one
 *   string, marked with `editedName` (`row-references.ts`); the row's chip draws the mark as an
 *   element of its own with `EditedMark`.
 * - **`deleted`**: the references among `readings` whose comment or message its author deleted,
 *   by its delete (`NodeChip.deletedAt`, `deletedBy`), each named in `readings` by its deleted
 *   line. A row's words draw that line in the deleted line's italic and name its actor as the
 *   page names an author, where the published name holds the actor as no registry knows it
 *   (`RowText.tsx`, INFRA-1196's decision 342); every other link draws the published name.
 *
 * Every reference in `edited` and in `deleted` is in `readings`: a target with no name takes no
 * mark, and a block of a deleted post is gone. None is in both `edited` and `deleted`: a deleted
 * post's chip carries no edit.
 */
export function chipNaming(chips: Iterable<NodeChip>): Omit<PublishedReferences, "asked"> {
  const ambiguous = new Set<string>();
  const deleted = new Map<string, PostDeletion>();
  const edited = new Set<string>();
  const gone = new Set<string>();
  const unnamed = new Set<string>();
  const readings = new Map<string, ReferenceNameReading>();
  for (const chip of chips) {
    if (chip.ambiguous === true) ambiguous.add(chip.ref);
    else if (chip.gone) gone.add(chip.ref);
    else if (chip.reading === undefined) unnamed.add(chip.ref);
    else {
      readings.set(chip.ref, chip.reading);
      if (chip.editedAt !== undefined) edited.add(chip.ref);
      const deletion = postDeletion({ deleted_at: chip.deletedAt, deleted_by: chip.deletedBy });
      if (deletion !== null) deleted.set(chip.ref, deletion);
    }
  }
  return { ambiguous, deleted, edited, gone, readings, unnamed };
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
 * under a node it already holds, is dropped, so a `from` edge that ever points in a circle still
 * draws a tree.
 *
 * A parent no draft answers to is no placement at all: a reply to a turn the page never read,
 * or a `from` naming a node the tree holds no draft for. Rather than dropping the
 * node with its missing parent, it sits under the issue, which is where every node whose parent
 * is absent goes (R4). A draft the issue cannot reach is the same case one step further out,
 * and `bringHome` answers it the same way.
 */
function assemble(
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
    // A parent named twice is one placement: a decision may name the same question twice - the
    // read of the release before named it once for each paragraph of a section that cited it -
    // and drawing it twice there would count it twice in `held` and give both rows one key
    // (`tree-keys.ts`).
    const named = [...new Set(draft.parents)].filter((parent) => known.has(parent));
    placements.set(draft.id, named.length === 0 ? [key] : named);
  }
  bringHome(key, drafts, placements);
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
      ...(draft.coverageNotes === undefined ? {} : { coverageNotes: draft.coverageNotes }),
      ...(draft.deletedAt === undefined ? {} : { deletedAt: draft.deletedAt }),
      ...(draft.deletedBy === undefined ? {} : { deletedBy: draft.deletedBy }),
      ...(draft.editedAt === undefined ? {} : { editedAt: draft.editedAt }),
      facts: draft.facts,
      gaps: draft.gaps,
      held: heldBy(children),
      id: draft.id,
      ...(draft.child === undefined ? {} : { child: draft.child }),
      ...(draft.keptBy === undefined ? {} : { keptBy: draft.keptBy }),
      ...(draft.multiple === undefined ? {} : { multiple: draft.multiple }),
      ...(draft.options === undefined ? {} : { options: draft.options }),
      question: draft.question,
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
    // How Dispatch names an issue everywhere: its key and then its title, which the node holds
    // apart - its key as its id and its title as its text - so its words write the key before the
    // title (`nodeWords`) and its one line reads a label after the key (`lineReading`). The
    // outline draws no row of it - the header is the issue - and a line that names the issue,
    // the breadcrumb's first crumb, names it by its key alone.
    text: issue.title,
    transcluded: false,
    type: "issue",
    waiting: false,
  };
}

/**
 * Puts back under the issue every draft the walk from the issue cannot reach, so that nothing
 * the reads hold is lost (R1): the tree is built downwards from the issue, so a ring of
 * placements nothing above it holds would be drawn nowhere, and every node inside it with it.
 * A `from` edge that points in a circle makes one.
 *
 * The ring is opened at one draft and no more, and that draft is a member of the ring itself:
 * the first of the ring members the issue cannot reach, in the order the tree draws siblings
 * in - its own order, then its id - comes home to the issue, and whatever that lets the issue
 * reach keeps the place its edge gave it, so the same reads always draw the same tree.
 *
 * It is the ring's own member and never the lowest order among the drafts the issue cannot
 * reach, because a node hanging off a ring is unreachable too and may sort before every member
 * of it: a block's order is its place among the issue's documents, and a requirement's is
 * `ROOT_ORDER.requirement` and up, so a plan task serving a requirement of a ring is the
 * first of them. Re-rooting that task would tear it off the requirement it serves and leave
 * the ring to be opened anyway.
 */
function bringHome(
  key: string,
  drafts: readonly Draft[],
  placements: Map<string, readonly string[]>
): void {
  const below = new Map<string, string[]>();
  for (const draft of drafts) {
    for (const parent of placements.get(draft.id) ?? []) {
      const siblings = below.get(parent);
      if (siblings === undefined) below.set(parent, [draft.id]);
      else siblings.push(draft.id);
    }
  }
  const reached = new Set<string>([key]);
  const walk = (from: string): void => {
    const queue = [from];
    for (;;) {
      const id = queue.pop();
      if (id === undefined) return;
      for (const child of below.get(id) ?? []) {
        if (reached.has(child)) continue;
        reached.add(child);
        queue.push(child);
      }
    }
  };
  walk(key);
  const ordered = [...drafts].sort(drawOrder);
  /** Whether `id` is a member of a ring: a draft its own placements lead back to. */
  const inRing = (id: string): boolean => {
    const seen = new Set<string>();
    const queue = [...(placements.get(id) ?? [])];
    for (;;) {
      const above = queue.pop();
      if (above === undefined) return false;
      if (above === id) return true;
      if (seen.has(above)) continue;
      seen.add(above);
      queue.push(...(placements.get(above) ?? []));
    }
  };
  // Nothing the reads hold is lost (R1): every draft the issue cannot reach sits under a ring,
  // since a draft whose placements run out sits under the issue and is reached, so the rings
  // this opens answer for them all.
  for (;;) {
    const member = ordered.find((draft) => !reached.has(draft.id) && inRing(draft.id));
    if (member === undefined) break;
    placements.set(member.id, [key]);
    reached.add(member.id);
    walk(member.id);
  }
}
