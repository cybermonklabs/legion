import { parseBlockPointerValue, referenceSpans } from "@legion/contracts";

import { primarySpec, roleDocument } from "../../api/issue-cache";
import type { Actor, Ask, Decision, DisplayTree, IssueChild } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { decisionsDocumentId } from "../decision-card/change-anchor";
import { deletedLineText, type PostDeletion, postDeletion } from "../refs/post-marks";
import { buildDispatchReference, issueOrDocumentTitle, parseNodeReference } from "../refs/routes";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { agentsOnlyIds } from "./agents-only";
import { checkFacts, resultFacts } from "./check-facts";
import {
  type ChangeComment,
  decisionChangeComments,
  decisionChanges,
  inReviewList,
} from "./decision-review";
import { artifactsBySlug } from "./issue-artifacts";
import type {
  IssueTreeReads,
  NodeAnswer,
  NodeChip,
  NodeCounts,
  NodeFact,
  NodeOption,
  TreeAsk,
} from "./issue-tree";
import {
  type AskState,
  blockAttributeLabel,
  coverageGapKind,
  type DecisionState,
  type GapKind,
  NODE_STATE_WORDS,
  NODE_TYPES,
  type NodeState,
  type NodeType,
} from "./page-words";
import { blockNodeId, checkStanding, coverageStatements } from "./statement-rules";
import { ACTOR, documentReading, type TreeContext } from "./target-naming";

/**
 * One draft node per read of the issue page, for `buildIssueTree` to place.
 *
 * Every function here is pure and takes the reads as its input. A builder may enrich a draft an
 * earlier one made - the decisions read names a block of `decisions.md` the document's own blocks
 * already drew, the coverage read names a block of the spec - so the order they run in is the
 * order `treeDrafts` calls them in, and each says what it adds.
 *
 * A node's own words come from the read that carries them: the coverage read for a requirement,
 * the results read for a check and its results, the decisions read for a decision, the
 * conversation for an ask, a comment and a message, and the one batched reference read for every
 * other block, whose `name` for a block is that block's stored excerpt. No document's text is
 * read, and no request is made per node. Where the read sends the server's reading of those
 * words beside them - every one of these reads does, a result's (`observed_tree`) included,
 * though a server of the build before sends none for a result - the draft keeps it with the
 * record the words are, which is what the row draws (`TreeNode.tree`).
 *
 * The tree is the issue's conversation and what it revealed: a document, an artifact and a
 * handoff are nodes of the issue and not of its conversation, so no builder here drafts one. A
 * document's typed blocks are drafted without it, under the issue unless the model gives a block
 * a parent of its own (R4). The issue's children are rows of the tree, one group of them where
 * the model places them, which today is under the issue: the page draws no section of them above
 * the outline.
 */

/** One node as it is being built: a `TreeNode`'s own fields, plus where the model placed it. */
export interface Draft {
  /** When it was made, where its read records one (`TreeNode.at`). */
  at?: string;
  author?: Actor;
  /** A person's answer to this ask, which the ask's own row and body draw (`TreeNode.answer`). */
  answer?: NodeAnswer;
  chips: NodeChip[];
  /** What the coverage read counts for a requirement it holds (`addRequirements`); absent on
   *  every other draft, a statement of the spec drawn before that read answers among them. */
  counts?: NodeCounts;
  /** When its author deleted this comment or message (`TreeNode.deletedAt`). */
  deletedAt?: string;
  /** Who deleted it, where the read names them (`TreeNode.deletedBy`). */
  deletedBy?: Actor;
  /** When its author last replaced this comment's or message's body (`TreeNode.editedAt`). */
  editedAt?: string;
  /** What the node's body draws under its text (`check-facts.ts`). */
  facts: NodeFact[];
  gaps: GapKind[];
  readonly id: string;
  /** The child issue this node is, as the issue read carries it (`TreeNode.child`). */
  child?: IssueChild;
  /** The person who kept this decision as it reads now (`TreeNode.keptBy`). */
  keptBy?: Actor;
  /** Whether this ask takes more than one of its options (`TreeNode.multiple`). */
  multiple?: true;
  /** The choices an ask offers (`TreeNode.options`). */
  options?: NodeOption[];
  order: number;
  /** The nodes this one sits under, in the order the model placed them: the first is where it is
   *  edited, every other a transclusion. `[]` is "under the issue". */
  parents: string[];
  /** What each `block[]` attribute of the block this node is points at, by the attribute's own
   *  name; empty for a node that is no block. */
  pointers: Readonly<Record<string, readonly string[]>>;
  question: boolean;
  reference?: string;
  replaced: boolean;
  /** The record `text` is, where the draft's read names one (`TreeNode.source`). */
  source?: TextSource;
  /** How it stands, for a type the model gives a state (`TreeNode.state`); absent on a check
   *  block of any document but the issue's Verification document (`addDocumentBlocks`). */
  state?: NodeState;
  /** Whether this requirement is a statement (`TreeNode.statement`): a `requirement` block of the
   *  issue's spec (`addDocumentBlocks`) or a requirement the coverage read holds
   *  (`addRequirements`). Absent on every other draft. */
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

/**
 * Where a child of the issue sits among the issue's own children: the typed blocks of its
 * documents first, the spec's before the rest, each in its own document's order, so the
 * questions inside the spec open at the top of the tree; then the decisions no question holds,
 * then the requirements, then the group of its child issues, and the conversation last: the
 * questions asked of the issue itself and the turns, together, in the order they were made
 * (`orderConversation`), so a question and a comment the reader writes from the line at the
 * foot both land at the outline's end, where the reader is taken once it is sent. A node's order
 * under any other parent is its own place in what it came from.
 */
export const ROOT_ORDER = {
  /** The first block of the issue's spec; every other document's blocks follow, one whole
   *  number per document (`documentRanks`), which keeps them under the decisions' `1000`. */
  block: 0,
  /** The group of child issues, each a fraction of one past it in the issue read's order, so
   *  however many children an issue has none reaches the conversation's orders. */
  child: 3000,
  /** A question of the issue's own and a turn: `orderConversation` counts up from here. */
  conversation: 4000,
  decision: 1000,
  requirement: 2000,
} as const;

/** Every draft the reads hold, by its node id. */
export function treeDrafts(reads: IssueTreeReads, context: TreeContext): Map<string, Draft> {
  const drafts = new Map<string, Draft>();
  addDocumentBlocks(drafts, reads, context);
  addChildren(drafts, reads);
  const asks = addAsks(drafts, reads, context);
  const placed = decisionChangeComments(
    reads.decisions,
    reads.comments,
    reads.issue.artifacts,
    reads.documents
  );
  const changes = decisionChanges(
    reads.decisions,
    reads.comments,
    reads.issue.artifacts,
    reads.documents,
    placed
  );
  addTurns(drafts, reads, context, asks, placed);
  orderConversation(drafts);
  addDecisions(drafts, reads, context, asks, changes);
  addRequirements(drafts, reads, context);
  addChecks(drafts, reads, context);
  servePlacements(drafts, reads, context);
  addDocumentChips(drafts, reads, context);
  return drafts;
}

/**
 * The conversation's own nodes under the issue - a question asked of the issue itself and each
 * turn - in the one order they were made, a question among the comments it was asked between
 * (`ROOT_ORDER.conversation`): the questions and the reader's threads are the Contents list's
 * entries in that order, as the approved pictures draw them, and a write from the line at the
 * foot is the newest, so it lands at the end, where the page takes the reader once it is sent.
 * Two made in the same instant are drawn by id, so the same reads always draw the same tree.
 */
function orderConversation(drafts: Map<string, Draft>): void {
  const conversation = [...drafts.values()].filter(
    (draft) => draft.order >= ROOT_ORDER.conversation
  );
  conversation.sort(
    (one, other) =>
      compareTimestamps(one.at ?? "", other.at ?? "") || one.id.localeCompare(other.id)
  );
  conversation.forEach((draft, index) => {
    draft.order = ROOT_ORDER.conversation + index;
  });
}

/**
 * A chip that opens each document of the issue a node's own words cite (R3): a document is no
 * node of the outline, so a block that names one carries a chip of its own for it, which opens
 * the document at its own address in place of the outline (`NodeRow.tsx`'s `Chip`). The chip
 * draws the page's word for the document, as every chip naming a whole document does
 * (`documentReading`): the filter's name for the spec and for the plan, Verification and
 * decisions documents, and "document" for any other, never its file name (decision 39). It
 * addresses the document as `artifact/<slug>` - the spec too, whose `spec` reference is the Spec
 * filter's address - and carries the name a link to that address reads, the document's file
 * name. A citation of one block of a document names that block, which the text's own link opens,
 * and no chip.
 */
function addDocumentChips(
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
      const address = { key: context.key, kind: "artifact", slug } as const;
      const ref = buildDispatchReference(address);
      if (draft.chips.some((chip) => chip.ref === ref)) continue;
      draft.chips = [
        ...draft.chips,
        {
          answered: true,
          gone: false,
          reading: documentReading(
            reads.issue,
            artifact,
            issueOrDocumentTitle(address, reads.issue, artifact)
          ),
          ref,
        },
      ];
    }
  }
}

/** Where each document of the issue stands among its own, by its artifact id: the spec
 *  (`primarySpec`) first and the rest in the order the issue carries them, which is the order
 *  their blocks are drawn in under the issue (`ROOT_ORDER.block`). */
function documentRanks(reads: IssueTreeReads): ReadonlyMap<string, number> {
  const specId = primarySpec(reads.issue)?.id;
  const ranks = new Map<string, number>();
  (reads.issue.artifacts ?? []).forEach((artifact, index) => {
    ranks.set(artifact.id, artifact.id === specId ? 0 : index + 1);
  });
  return ranks;
}

/** Puts `draft` in, over whatever draft held its id. */
function add(drafts: Map<string, Draft>, draft: Draft): Draft {
  drafts.set(draft.id, draft);
  return draft;
}

/** A draft with nothing on it but its id and its type, for a builder to fill in. */
function blank(id: string, type: NodeType, order: number): Draft {
  return {
    chips: [],
    facts: [],
    gaps: [],
    id,
    order,
    parents: [],
    pointers: {},
    question: false,
    replaced: false,
    text: "",
    type,
    waiting: false,
  };
}

/**
 * Each document's typed blocks, in document order. A block of no declared node type - a
 * paragraph, a heading, a table - is the document's own text and no node of the tree. A block's
 * `block[]` attributes are kept as its pointers, which is where `serves` and `needs` are read
 * from; its words come from the batched reference read, since nothing reads the document's text.
 *
 * Every block sits under the issue, and never under its document, which is no node of the tree
 * (R3, R4): a builder after this one places a block the model gives a parent of its own - a
 * requirement by its `from`, a check and a plan task by their `serves`. Among the issue's own
 * children a document's blocks keep the order they have in it, one document after another.
 *
 * A block plays the part its type names only in the document that plays that part, which the
 * issue read names by its id (`primarySpec`, `roleDocument`): a `requirement` block of the spec
 * is a statement (`Draft.statement`) before the
 * coverage read answers as after, and a `check` block of the Verification document is a check,
 * which draws a state. The same block of any other document - a `requirement` in the plan, a
 * `check` in the decisions document - keeps its type and is a block and nothing more: no
 * filter draws or counts it as its kind, nothing is placed under it, and a check of it draws no
 * state word, since the results read never answers for it.
 */
function addDocumentBlocks(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  const types: readonly string[] = NODE_TYPES;
  const ranks = documentRanks(reads);
  const specId = primarySpec(reads.issue)?.id;
  const verificationId = roleDocument(reads.issue, "verification")?.id;
  for (const document of reads.documents ?? []) {
    const slug = context.slugOf(document.artifactId);
    if (slug === undefined) continue;
    const rank = ranks.get(document.artifactId) ?? 0;
    document.blocks.forEach((block, index) => {
      if (!types.includes(block.type)) return;
      const reference = buildDispatchReference({
        block: { id: block.id },
        key: context.key,
        kind: "artifact",
        slug,
      });
      const pointers: Record<string, readonly string[]> = {};
      for (const attribute of block.attributes ?? []) {
        if (attribute.kind === "block[]") {
          pointers[attribute.name] = parseBlockPointerValue(attribute.value) ?? [];
        }
      }
      // The document's rank, and the block's place inside it as a fraction of one, so no two
      // documents' blocks interleave.
      const order = ROOT_ORDER.block + rank + index / document.blocks.length;
      const draft = add(drafts, blank(blockNodeId(slug, block.id), block.type as NodeType, order));
      draft.pointers = pointers;
      draft.reference = reference;
      draft.text = context.excerptOf(reference);
      // The block's own words whole, as the server reads them, where the reference read sent
      // them: the excerpt above is the block's stored source, cut at 480 runes and unread.
      const tree = context.treeOf(reference);
      if (tree !== undefined) {
        draft.tree = tree;
        draft.source = textSourceOf("artifact", document.artifactId, context.key);
      }
      if (block.type === "requirement" && document.artifactId === specId) draft.statement = true;
      // A check with no result recorded against it is in no state the results read would name,
      // and `no-result` is that state (`NODE_STATE_WORDS.check`), so a check drawn before the
      // results read answers says so rather than drawing a state nothing recorded.
      if (block.type === "check" && document.artifactId === verificationId) {
        draft.state = "no-result";
      }
      if (block.type === "ask") draft.state = "open";
    });
  }
}

/**
 * The issue's children, each a node of the tree and a row of the outline, one group of them in
 * the issue read's order: its title as its words, and how it stands, what blocks it, who holds it
 * and its key as the tag drawn from the child itself (`IssueChildren.tsx`). The model places a
 * child under the words that filed it once it holds that edge; today's reads hold no such edge,
 * so the group sits under the issue. A child's own tree is its own page, so a child holds no
 * node below it.
 */
function addChildren(drafts: Map<string, Draft>, reads: IssueTreeReads): void {
  const children = reads.issue.children ?? [];
  children.forEach((child, index) => {
    const order = ROOT_ORDER.child + index / children.length;
    const draft = add(drafts, blank(child.key, "child_issue", order));
    draft.child = child;
    draft.text = child.title;
  });
}

/**
 * Every question asked of the issue: the block the server indexed it from, where it has one,
 * else a node of its own under the issue. An approval request belongs to the document it reviews
 * rather than to the reasoning, so the tree draws none.
 *
 * A person's answer is part of the ask and no node of its own: it is kept on the ask's draft as
 * the store holds it, which the ask's own row and body draw.
 *
 * Answers the node id each ask is drawn under, by the ask's own id, which is how a decision
 * names the question it answers (`Decision.asks`).
 */
function addAsks(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): ReadonlyMap<string, string> {
  const asks = new Map<string, TreeAsk>();
  for (const ask of [...(reads.issue.open_asks ?? []), ...(reads.asks ?? [])]) {
    if (ask.kind === "question") asks.set(ask.id, ask);
  }
  const placed = new Map<string, string>();
  for (const ask of asks.values()) {
    // The document holding the typed `ask` block is `block_artifact`; `artifact_id` is the
    // document an anchored ask is anchored in, which a block ask does not carry.
    const slug = ask.block_artifact?.slug ?? context.slugOf(ask.artifact_id ?? "");
    const block = ask.block_id ?? undefined;
    const id = slug !== undefined && block !== undefined ? blockNodeId(slug, block) : ask.id;
    const draft = drafts.get(id) ?? add(drafts, blank(id, "ask", ROOT_ORDER.conversation));
    placed.set(ask.id, id);
    draft.at = ask.created_at;
    draft.author = ask.author;
    if (ask.answer !== null) draft.answer = answerOf(ask.answer);
    if ((ask.options ?? []).length > 0) {
      draft.options = (ask.options ?? []).map((option) => ({
        ...(option.description === undefined || option.description === ""
          ? {}
          : {
              description: option.description,
              ...(option.description_tree === undefined
                ? {}
                : { descriptionTree: option.description_tree }),
            }),
        label: option.label,
        ...(option.label_tree === undefined ? {} : { labelTree: option.label_tree }),
      }));
    }
    if (ask.multiple) draft.multiple = true;
    draft.reference = buildDispatchReference({ id: ask.id, key: context.key, kind: "ask" });
    draft.source = textSourceOf("ask", ask.id, context.key);
    draft.state = askState(ask);
    draft.text = ask.question;
    draft.tree = ask.question_tree;
    // An open question is what the fold unfolds the path to, whatever its depth (R6).
    draft.question = ask.state === "open";
    // An open question whose turn is the reader's own waits on the reader, and is what the
    // header's count of what waits puts the focus on (R13, `dashboard/useWaitingItems.ts`'s
    // `waitingItems`): the person the server resolved it waits on (`waiting_on_person`), as every
    // other count of what waits on the reader reads it (`inbox/BlockedOnYou.tsx`'s
    // `waitingOnYou`). A question waiting on somebody else is open all the same, and the fold
    // still unfolds the path to it.
    draft.waiting =
      ask.state === "open" &&
      ask.waiting_on === "human" &&
      context.viewer !== undefined &&
      ask.waiting_on_person === context.viewer;
  }
  return placed;
}

/** An ask's answer as the tree holds it: the person it is theirs, with the agent they answered
 *  through where they did, and their choices and words as stored, with the server's reading of
 *  each where the read sent one. */
function answerOf(answer: NonNullable<TreeAsk["answer"]>): NodeAnswer {
  return {
    at: answer.at,
    by: { id: answer.user, kind: "user", ...(answer.act === undefined ? {} : { act: answer.act }) },
    selected: answer.selected ?? [],
    ...(answer.selected_trees === undefined ? {} : { selectedTrees: answer.selected_trees }),
    text: answer.text ?? "",
    ...(answer.text_tree === undefined ? {} : { textTree: answer.text_tree }),
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

/** One turn of the conversation as the tree holds it: a comment and a message are the same row
 *  here, told apart by the kind they are drawn as and by the address they carry. */
interface Turn {
  /** When it was made, which is the order the conversation is read in. */
  readonly at: string;
  readonly author: Actor;
  readonly body: string;
  /** The server's reading of `body`. */
  readonly bodyTree?: DisplayTree;
  /** Its author's delete of it (`postDeletion`), null for a turn nobody deleted. */
  readonly deleted: PostDeletion | null;
  /** When its author last replaced its body, null for a turn nobody edited. */
  readonly editedAt: string | null;
  readonly id: string;
  readonly kind: "comment" | "message";
  /** The node it answers, `null` for a root turn under the issue. */
  readonly replyTo: string | null;
}

/**
 * The line a deleted comment's or message's node holds in place of its body: the words
 * `DeletedBody` (`refs/PostBody.tsx`) prints where the body was, `Deleted by <who> · <when>`, or
 * `Deleted · <when>` for a delete whose read names nobody (`deletedLineText`). The model is pure
 * and reads no agent registry and no clock, so `<who>` is the actor as `AgentName` names one
 * before the registry answers (`actorLabelText`: a person's login, a session's stamped title
 * else its short id), and `<when>` is the time the read stores (`deleted_at`) as it stores it,
 * never an age; a view that draws the line itself names the actor from `deletedBy` and draws
 * the time from `deletedAt`. It is made from the delete alone, so it holds no part of the body
 * even where a read still carries one.
 */
function deletedPostLine(deleted: PostDeletion): string {
  return `${deletedLineText(deleted, ACTOR)} · ${deleted.at}`;
}

/**
 * The conversation's own turns: each comment and each message that is no turn of a thread the
 * panel's history holds, a node of this tree, a root turn under the issue and a reply under
 * the entry it answers (R1, R16). A thread head a page never read is no node, so a reply whose
 * parent is missing falls back to the issue, as every node whose parent is absent does
 * (`assemble`). A thread the history holds is no node at all (`agents-only.ts`): one the Legion
 * daemon began, whoever wrote in it, and one tied to no text that no person wrote in.
 *
 * One conversation is one order, so the turns are drawn by the time they were made and not by
 * their place in the read that carried them: the comments and the messages are two lists, each
 * counting from zero, and a message sent today drawn among yesterday's comments is the reader
 * reading the conversation out of order (`orderConversation`).
 *
 * A comment is drawn under the entry it answers: `answers`, the comment its write named, and
 * never `reply_to`, which that same write climbed to the head of the thread before it stored
 * (`packages/envoy/internal/dispatch/api/comment_create.go`'s `threadHeadOf`), so a reply to a
 * reply parented by `reply_to` would be drawn under the thread's head. A comment stored before
 * `comments.answers` existed (migration 20261105014808) carries none, and its thread's head is
 * then the one entry it names. A reply to an ask names the ask alone (`ask_id`) and is drawn
 * under the ask's node, whatever state the ask is in, a resolved one included. An approval
 * request is no node (`addAsks`), so the replies under one
 * are no node either where only agents wrote them, and sit under the issue once a person has
 * written among them. A message carries no such column, so its thread stays as
 * `in_reply_to` holds it. A comment that is a change a person asked of a decision, and answers
 * nothing else, is drawn under that decision: every change newer than the decision's words, and
 * not only the newest, which `changeOf` holds by the comment's id (`decision-review.ts`'s
 * `decisionChangeComments`), so each change stands tied to the decision it changes.
 *
 * An address naming a comment or a message of the outline (`/comments/<id>`, `/messages/<id>`,
 * `?comment=`) is therefore an address of a row of this tree.
 *
 * A turn its author corrected keeps its place (INFRA-1523, R2 and R3). An edited one keeps its
 * words and carries when it was last edited (`editedAt`). A deleted one carries when and by whom
 * (`deletedAt`, `deletedBy`), and holds the deleted line as its text (`deletedPostLine`) and no
 * reading, so no line, body, filter or citation read from the node reads any part of its body.
 * It carries no `editedAt`: the deleted line already says what became of the text, as the
 * byline's ` · edited` says nothing of a deleted post (`EditedMark`).
 */
function addTurns(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext,
  asks: ReadonlyMap<string, string>,
  changeOf: ReadonlyMap<string, string>
): void {
  // The asks `addAsks` drew its nodes from, which is what tells a reply under a question the
  // tree draws from one under an approval request, which it does not.
  const history = agentsOnlyIds(reads.comments, reads.messages, [
    ...(reads.issue.open_asks ?? []),
    ...(reads.asks ?? []),
  ]);
  const turns: Turn[] = [
    ...(reads.comments ?? []).map((comment) => ({
      at: comment.created_at,
      author: comment.author,
      body: comment.body,
      bodyTree: comment.body_tree,
      deleted: postDeletion(comment),
      editedAt: comment.edited_at,
      id: comment.id,
      kind: "comment" as const,
      replyTo:
        comment.answers ??
        comment.reply_to ??
        (comment.ask_id === null
          ? (context.nodeIdOf(changeOf.get(comment.id) ?? "") ?? null)
          : (asks.get(comment.ask_id) ?? comment.ask_id)),
    })),
    ...(reads.messages ?? [])
      .flatMap((thread) => [thread.message, ...(thread.replies ?? [])])
      .map((message) => ({
        at: message.created_at,
        author: message.author,
        body: message.body,
        bodyTree: message.body_tree,
        deleted: postDeletion(message),
        editedAt: message.edited_at,
        id: message.id,
        kind: "message" as const,
        replyTo: message.in_reply_to,
      })),
  ].filter((turn) => !history.has(turn.id));
  for (const turn of turns) {
    const draft = add(drafts, blank(turn.id, turn.kind, ROOT_ORDER.conversation));
    draft.at = turn.at;
    draft.author = turn.author;
    draft.parents = turn.replyTo === null ? [] : [turn.replyTo];
    draft.reference = buildDispatchReference({ id: turn.id, key: context.key, kind: turn.kind });
    draft.source = textSourceOf(turn.kind, turn.id, context.key);
    if (turn.deleted === null) {
      draft.text = turn.body;
      draft.tree = turn.bodyTree;
      if (turn.editedAt !== null) draft.editedAt = turn.editedAt;
    } else {
      draft.deletedAt = turn.deleted.at;
      if (turn.deleted.by !== null) draft.deletedBy = turn.deleted.by;
      draft.text = deletedPostLine(turn.deleted);
    }
  }
}

/**
 * Each decision of the issue's `decisions.md`, under the question it answers (R4): the asks its
 * blocks mention (`Decision.asks`), the first of them where it is edited and every other one a
 * transclusion, as a check sits under each requirement it serves. An ask the tree draws no node
 * for - another issue's, or an approval request - places nothing and stays the decision's chip,
 * so a decision that names no question of this issue sits under the issue, as any node whose
 * spine parent is absent does, and never inside `decisions.md`, which is no node at all. It
 * carries no gap for that: a lack every such decision shares tells none of them apart (R10). It
 * names every spec block it links as a chip, since a link places nothing. The read carries no
 * `shaped` edge (`packages/contracts/src/dispatch-api.ts:2519-2534`), so
 * today a decision's links stand for it: they are its chips here, and what `addRequirements`
 * reads a `shaped_disagreement` from.
 */
function addDecisions(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext,
  asks: ReadonlyMap<string, string>,
  changes: ReadonlyMap<string, ChangeComment>
): void {
  // The words are a block of the decisions document, which is what their references resolve
  // against (`TreeNode.source`). The read names that document by its address, and a source
  // names a document by its id.
  const source = textSourceOf(
    "artifact",
    decisionsDocumentId(reads.issue.artifacts, reads.decisions),
    context.key
  );
  // A decision waits for the reader's review where it is on their review list (R35), which
  // reads a statement as a requirement the coverage read holds (`coverageStatements`), the rule
  // the header's count reads too (`features/dashboard/useWaitingItems.ts`), and not as the rows
  // read one (`Draft.statement`): a spec's `requirement` block that read does not hold - each
  // one, before it answers - is a statement on its row, and a tie to it puts no decision on the
  // list.
  const isCoverageStatement = coverageStatements(reads.issue, reads.coverage);
  (reads.decisions?.decisions ?? []).forEach((decision, index) => {
    const id = context.nodeIdOf(decision.ref);
    if (id === undefined) return;
    const held = drafts.get(id);
    const draft = add(drafts, blank(id, "decision", ROOT_ORDER.decision + index));
    const answers = decision.asks ?? [];
    // A target with no address of its own is named by nothing, so there is no chip to draw for
    // it (`GraphNode.ref` is optional). The section links a block once for each of its
    // paragraphs that links it, and names an ask once for each that cites it
    // (`api/decisions.go`), so each target is one chip, stale where any of its links is.
    const linked = new Map<string, NodeChip>();
    for (const link of decision.links ?? []) {
      const ref = link.to.ref;
      if (ref === undefined) continue;
      const stale = link.stale || linked.get(ref)?.stale === true;
      linked.set(ref, { ...context.chipOf(ref), stale });
    }
    draft.chips = [
      ...linked.values(),
      ...[...new Set(answers)]
        .filter((ask) => !asks.has(ask))
        .map((ask) =>
          context.chipOf(buildDispatchReference({ id: ask, key: context.key, kind: "ask" }))
        ),
    ];
    draft.parents = answers.flatMap((ask) => {
      const parent = asks.get(ask);
      return parent === undefined ? [] : [parent];
    });
    if (decision.changed_at !== null) draft.at = decision.changed_at;
    draft.pointers = held?.pointers ?? {};
    draft.reference = decision.ref;
    draft.replaced = decision.replaced;
    draft.source = source;
    draft.text = decision.text;
    draft.tree = decision.text_tree;
    // How the decision stands with the reader, drawn as this node's own state word and shape
    // the way every other state is (R21).
    draft.state = decisionState(decision, changes.has(decision.ref));
    // The tie to a statement decides whether it waits on the reader, and a decision the reader
    // asked to change waits on its agent, not on the reader (`decision-review.ts`). Nothing
    // waits on a reader whose login has not resolved, as `dashboard/useWaitingItems.ts`'s
    // `waitingItems` counts it.
    draft.waiting =
      context.viewer !== undefined &&
      inReviewList(decision, isCoverageStatement, changes.has(decision.ref));
    if (decision.kept !== null && !decision.waiting_review) {
      draft.keptBy = { id: decision.kept.kept_by, kind: "user" };
    }
    if (decision.unsure) draft.unsure = true;
  });
}

/**
 * How a decision stands with the reader, in the states the page draws
 * (`NODE_STATE_WORDS.decision`, R2): replaced by a later one first, since nothing waits on it
 * any longer; then a change the reader asked for, which waits on its agent and not on the
 * reader (`decision-review.ts`); then waiting for review; then kept, by the person whose name the
 * row draws after it (`TreeNode.keptBy`). A person's own decision is in none of them: it is
 * theirs, and the row draws no state.
 */
function decisionState(decision: Decision, changed: boolean): DecisionState | undefined {
  if (decision.replaced) return "replaced";
  if (changed) return "change_requested";
  if (decision.waiting_review) return "waiting_review";
  return decision.kept === null ? undefined : "kept";
}

/**
 * Every requirement of the issue's spec sits under the node its `from` names. No requirement
 * carries one - the `requirement` block declares no attributes at all
 * (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:70-75`, read 2026-11-05) - so each
 * sits under the issue, beside the two counts the coverage read holds for it and the gaps that
 * read already carries. An absent `from` is no gap of its own: every requirement lacks one, so it
 * tells none of them apart (R10). A decision is never a requirement's parent (R4): a `from` that
 * names one places nothing, and the requirement sits under the issue.
 *
 * The decisions read carries no `shaped` edge, so today a decision's links stand for it: they
 * are the decision's chips (`addDecisions`) and what the disagreement is read from. Where a
 * requirement has an origin and a decision that is not that origin links to it, the two edges
 * disagree: the requirement is placed by its `from` and the disagreement is drawn as a gap. The
 * decision its `from` names linking to it is the two edges agreeing, which draws none.
 *
 * Every requirement the coverage read holds is a statement of the spec (`Draft.statement`), as
 * the spec's own `requirement` block was before the read answered (`addDocumentBlocks`).
 */
function addRequirements(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  // The node of every decision whose links name each requirement, by the requirement's node id.
  // A decision the tree addresses no node for is one `addDecisions` draws nothing for either.
  const shapers = new Map<string, Set<string>>();
  for (const decision of reads.decisions?.decisions ?? []) {
    const shaper = context.nodeIdOf(decision.ref);
    if (shaper === undefined) continue;
    for (const link of decision.links ?? []) {
      const shaped = link.to.ref === undefined ? undefined : context.nodeIdOf(link.to.ref);
      if (shaped === undefined) continue;
      const held = shapers.get(shaped);
      if (held === undefined) shapers.set(shaped, new Set([shaper]));
      else held.add(shaper);
    }
  }
  // The spec, which a statement's words are a block of and its references resolve against.
  const specId = primarySpec(reads.issue)?.id;
  (reads.coverage?.requirements ?? []).forEach((requirement, index) => {
    const id = blockNodeId(context.specSlug, requirement.block_id);
    const held = drafts.get(id);
    // The `from` edge is the single placement authority (R4): the requirement's own pointer,
    // never the decision that claims to have shaped it.
    const origin = context.nodeIdOf(held?.pointers.from?.[0] ?? "");
    const draft = add(drafts, blank(id, "requirement", ROOT_ORDER.requirement + index));
    draft.chips = held?.chips ?? [];
    draft.counts = { checks: requirement.checks, planTasks: requirement.plan_tasks };
    draft.gaps = requirementGaps(requirement.gaps ?? [], origin, shapers.get(id));
    draft.parents = origin === undefined || drafts.get(origin)?.type === "decision" ? [] : [origin];
    draft.pointers = held?.pointers ?? {};
    draft.reference = requirement.ref;
    draft.statement = true;
    draft.text = requirement.text;
    // The server's reading of the statement's own words, with the references the coverage
    // read's plain words leave out: the read's own, else the one its block carried.
    const tree = requirement.text_tree ?? held?.tree;
    if (tree !== undefined) {
      draft.tree = tree;
      draft.source = held?.source ?? textSourceOf("artifact", specId, context.key);
    }
  });
}

/** The gaps on one requirement: the one the coverage read carries that the page draws, and
 *  `shaped_disagreement` where the requirement has an origin and a decision that links to it
 *  (`shapers`, each by its node id) is not that origin. */
function requirementGaps(
  coverage: readonly string[],
  origin: string | undefined,
  shapers: ReadonlySet<string> | undefined
): GapKind[] {
  const gaps = coverage.flatMap((line): GapKind[] => {
    const kind = coverageGapKind(line);
    return kind === undefined ? [] : [kind];
  });
  if (origin !== undefined && [...(shapers ?? [])].some((shaper) => shaper !== origin)) {
    gaps.push("shaped_disagreement");
  }
  return gaps;
}

/**
 * Each check of the issue's Verification document with the results recorded against it: the
 * check's own words, how it stands and the four attributes its body draws, and one node per
 * result under the check it was recorded against (R4, "a result under its check"), each with
 * what that result recorded (`check-facts.ts`). How it stands is the page's one reading of it,
 * as the server reads the commit each of its results ran at (`checkStanding`), so the row, the
 * fold's frontier, the Verification filter's failing count and its body read one state, and the
 * row reads what its result's own fact reads. The check's `serves` is no part of this read
 * (`CheckBlock` carries none), so its placement is the block's own pointer, below.
 *
 * A result is drawn from the server's reading of its observed text (`observed_tree`) where the
 * read carries one, so the page parses no result's text; only from a server that sends none, a
 * build before it, is that text parsed from its stored markdown (`storedReading`). Its node names
 * no `source`: a result is no record the reference graph indexes a text under (`TextSource`), so
 * a `#id` in its observed text declares nothing.
 */
function addChecks(drafts: Map<string, Draft>, reads: IssueTreeReads, context: TreeContext): void {
  for (const check of reads.results?.checks ?? []) {
    const slug = context.slugOf(check.artifact_id);
    if (slug === undefined) continue;
    const id = blockNodeId(slug, check.block_id);
    const held = drafts.get(id);
    const draft = add(drafts, blank(id, "check", held?.order ?? ROOT_ORDER.block));
    draft.facts = checkFacts(check);
    draft.parents = held?.parents ?? [];
    draft.pointers = held?.pointers ?? {};
    draft.reference = check.ref;
    draft.state = checkStanding(check).state;
    draft.text = check.text;
    const tree = check.text_tree ?? held?.tree;
    if (tree !== undefined) {
      draft.tree = tree;
      draft.source = held?.source ?? textSourceOf("artifact", check.artifact_id, context.key);
    }
    (check.results ?? []).forEach((result, index) => {
      const row = add(drafts, blank(result.id, "result", index));
      row.at = result.created_at;
      row.author = result.actor;
      row.facts = resultFacts(result);
      row.parents = [id];
      row.text = result.observed;
      if (result.observed_tree !== undefined) row.tree = result.observed_tree;
    });
  }
}

/**
 * A check and a plan task sit under each statement their `serves` names, edited under the
 * first and transcluded under the rest (R4); what their `needs` names is a chip, one per target
 * (`needsChips`), as every edge that is not the spine is. A `serves` naming no statement of
 * this issue places nothing, so the block keeps its place under the issue. A statement is a
 * requirement of the issue's spec (`Draft.statement`), and the `requirement` block of any other
 * document is a block and nothing more, which nothing is placed under.
 *
 * Three of the chain's gaps are read here, from the pointers this pass already resolves (R10):
 *
 * - **A check that names nothing it serves** (`serves_nothing`). A check is there to prove a
 *   statement, so a check whose `serves` names no target at all stands under the issue with
 *   nothing to prove. Only a check that draws a state carries it (`issue-tree.ts`'s `isCheck`),
 *   one of the Verification document or of the results read: a `check` block of any other
 *   document is a block and nothing more. The gap is read only where the page holds the check's
 *   own block, whose attributes its pointers are: a check the results read answers for before
 *   its block arrives holds no pointers yet, which is no `serves` left empty.
 * - **A citation whose target is no statement** (`serves_not_requirement`). `serves` points
 *   only at a requirement (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:81-84`),
 *   and the write that introduces one is refused where it does not
 *   (`pmdoc.CheckBlockPointers`). A target retyped after that write leaves the citation naming
 *   a node of another kind, which the drafts hold the type of, and a `requirement` block of a
 *   document other than the spec is a requirement and no statement. A target deleted after it,
 *   such as a requirement taken out of the spec, leaves the citation naming nothing, which the
 *   page can tell only of a document whose blocks it holds: the spec and the plan,
 *   Verification and decisions documents (`useIssueTree.ts`'s `treeDocumentIds`), whose blocks
 *   arrive in `IssueTreeReads.documents`. A target of this issue that no draft answers to is
 *   therefore no statement once the blocks of the document it names are held, and no such
 *   citation while they are out: `useIssueTree` hands each document in with no blocks until
 *   its read answers, and a refused read too, so a document is held where it holds a block. A
 *   target in any other document of this issue is no such citation either, since the page
 *   reads no blocks of it, and nor is a reference this issue does not address (`nodeIdOf`):
 *   the page cannot tell it from a statement of an issue above, which a check serves. The
 *   gap's words say what each of these is: not a statement.
 * - **A plan task nobody asked for** (`nobody_asked`). A task naming no requirement it serves
 *   and no declaration it attempts is work no reason stands behind. `attempts` is the route of
 *   the planned declarations read, read from the same `block[]` attributes as every other
 *   pointer, so the day that read lands this pass reads it and nothing else changes. A check is
 *   held to the first gap above and never to this one: it attempts no declaration at all.
 *
 * A check or a plan task whose every target is a statement carries none of these gaps.
 */
function servePlacements(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  // The slug of each document whose blocks the page holds, and the node id of each block it
  // holds, a block of no node type included.
  const heldDocuments = new Set<string>();
  const heldBlocks = new Set<string>();
  for (const document of reads.documents ?? []) {
    const slug = context.slugOf(document.artifactId);
    if (slug === undefined || document.blocks.length === 0) continue;
    heldDocuments.add(slug);
    for (const block of document.blocks) heldBlocks.add(blockNodeId(slug, block.id));
  }
  // Whether `reference`, which names the node `id` where this issue addresses it, names a target
  // of this issue that is no statement: a node of another kind, or nothing at all in a document
  // whose blocks the page holds.
  const namesNoStatement = (reference: string, id: string | undefined): boolean => {
    if (id === undefined) return false;
    const target = drafts.get(id);
    if (target !== undefined) return target.statement !== true;
    const route = parseNodeReference(reference);
    const slug =
      route?.kind === "spec" ? context.specSlug : route?.kind === "artifact" ? route.slug : "";
    return heldDocuments.has(slug);
  };
  for (const draft of drafts.values()) {
    if (draft.type !== "check" && draft.type !== "plan_task") continue;
    const cited = draft.pointers.serves ?? [];
    const named = cited.map(context.nodeIdOf);
    const serves = named.filter(
      (id): id is string => id !== undefined && drafts.get(id)?.statement === true
    );
    if (serves.length > 0) draft.parents = serves;
    if (cited.some((reference, at) => namesNoStatement(reference, named[at]))) {
      draft.gaps = [...draft.gaps, "serves_not_requirement"];
    }
    if (
      draft.type === "check" &&
      draft.state !== undefined &&
      cited.length === 0 &&
      heldBlocks.has(draft.id)
    ) {
      draft.gaps = [...draft.gaps, "serves_nothing"];
    }
    if (
      draft.type === "plan_task" &&
      cited.length === 0 &&
      (draft.pointers.attempts ?? []).length === 0
    ) {
      draft.gaps = [...draft.gaps, "nobody_asked"];
    }
    draft.chips = [...draft.chips, ...needsChips(draft.pointers.needs ?? [], context)];
  }
}

/**
 * The chips of what a block `needs`, one per target, in the
 * order its pointers first name each. The server accepts one task needed by a pinned and an
 * unpinned pointer at once (`#b-a1@v1 #b-a1`), and both need the one task, so the pointers are
 * told apart by what they name with any version pin dropped (`pinDropped`). A target named both
 * ways is drawn by its unpinned pointer's chip, which names the block's current words; one only
 * pinned pointers name, by the first one's.
 */
function needsChips(needs: readonly string[], context: TreeContext): NodeChip[] {
  const needed = new Map<string, { readonly pinned: boolean; readonly reference: string }>();
  for (const reference of needs) {
    const { pinned, target } = pinDropped(reference);
    const held = needed.get(target);
    if (held === undefined || (held.pinned && !pinned)) needed.set(target, { pinned, reference });
  }
  return [...needed.values()].map(({ reference }) => ({
    ...context.chipOf(reference),
    label: blockAttributeLabel("needs"),
  }));
}

/**
 * What `reference` names with any version pin dropped, a block's (`#b-a1@v1`) and a document's
 * (`artifact/plan-md@v1`), as the reference its route writes with none (`target`), and whether
 * it pinned one (`pinned`). A reference that does not parse is its own target, pinning nothing.
 */
function pinDropped(reference: string): { readonly pinned: boolean; readonly target: string } {
  const route = parseNodeReference(reference);
  if (route === undefined) return { pinned: false, target: reference };
  const block = "block" in route ? route.block : undefined;
  const version = "version" in route ? route.version : undefined;
  return {
    pinned: block?.pin !== undefined || version !== undefined,
    target: buildDispatchReference({
      ...route,
      ...(block === undefined ? {} : { block: { id: block.id } }),
      ...(version === undefined ? {} : { version: undefined }),
    }),
  };
}
