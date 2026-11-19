import { parseBlockPointerValue } from "@legion/contracts";

import type { Actor, Ask, CheckBlock, Decision, DisplayTree, IssueChild } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { referenceSpans } from "../refs/reference-spans";
import { buildDispatchReference, parseNodeReference } from "../refs/routes";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { agentsOnlyIds } from "./agents-only";
import { checkFacts, resultFacts } from "./check-facts";
import { type ChangeComment, decisionChanges, inReviewList } from "./decision-review";
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
  type CheckState,
  coverageGapKind,
  type DecisionState,
  type GapKind,
  NODE_STATE_WORDS,
  NODE_TYPES,
  type NodeState,
  type NodeType,
} from "./page-words";

/**
 * One draft node per read of the issue page, for `buildIssueTree` to place
 * (dispatch://INFRA-1196 R4, `dispatch://INFRA-1196/artifact/plan-md#b-t13`).
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
 * words beside them - the decisions read and the conversation do - the draft keeps it with the
 * record the words are, which is what the row draws (`TreeNode.tree`).
 *
 * The tree is the issue's conversation and what it revealed (dispatch://INFRA-1196 R1, R3): a
 * document, an artifact and a handoff are nodes of the issue and not of its conversation, so no
 * builder here drafts one. A document's typed blocks are drafted without it, under the issue
 * unless the model gives a block a parent of its own (R4). The issue's children are rows of the
 * tree, one group of them where the model places them, which today is under the issue: the page
 * draws no section of them above the outline (the architect's ruling 5 on R12).
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
  counts?: NodeCounts;
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
  state?: NodeState;
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
  child: 3000,
  /** A question of the issue's own and a turn: `orderConversation` counts up from here. */
  conversation: 4000,
  decision: 1000,
  requirement: 2000,
} as const;

/** What every builder needs of the reads: how this issue addresses a node, and what a reference
 *  points at. Held as one value so the placement and the names are read the same way everywhere. */
export interface TreeContext {
  /** A chip naming what `reference` points at, by the one batched reference read. */
  readonly chipOf: (reference: string) => NodeChip;
  readonly key: string;
  /** The node `reference` names, as this issue addresses it; undefined for anything else. */
  readonly nodeIdOf: (reference: string) => string | undefined;
  /** The slug of the issue's spec, `""` for an issue with none. */
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

export function treeContext(reads: IssueTreeReads): TreeContext {
  const key = reads.issue.key;
  const artifacts = reads.issue.artifacts ?? [];
  const slugs = new Map(artifacts.map((artifact) => [artifact.id, artifact.slug]));
  const specSlug = artifacts.find((artifact) => artifact.primary)?.slug ?? "";
  const names = new Map((reads.targets ?? []).map((target) => [target.ref, target]));
  const nodeIdOf = (reference: string): string | undefined => {
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
  return {
    chipOf: (reference: string): NodeChip => {
      const target = names.get(reference);
      const gone = target?.gone ?? false;
      return {
        answered: target !== undefined,
        gone,
        name: gone ? "" : blockWords(target?.name ?? ""),
        ref: reference,
      };
    },
    key,
    nodeIdOf,
    specSlug,
    slugOf: (artifactId: string) => slugs.get(artifactId),
    treeOf: (reference: string) => names.get(reference)?.text_tree,
    viewer: reads.viewer,
  };
}

/** A directive's own opening line, `:::plan_task{#t1 serves="…"}`, and its closing fence. */
const DIRECTIVE_OPENER = /^:{3,}[a-z_]/;
const DIRECTIVE_FENCE = /^:{3,}\s*$/;

/**
 * The words a target is named by. The reference read names a document's block by its stored
 * excerpt, which is the block's source: a typed block is a directive, so its excerpt opens on
 * the directive's line - its type and its attributes - and, where the excerpt holds the whole
 * block, ends on its closing fence. Both are markup and no word of the block (R21), so they are
 * left out, as the head's three sentences leave them out of the callout they read
 * (`IssueThreeThings.tsx`). Any other name is its own words already.
 */
export function blockWords(name: string): string {
  const lines = name.trimEnd().split("\n");
  if (!DIRECTIVE_OPENER.test(lines[0] ?? "")) return name;
  lines.shift();
  if (DIRECTIVE_FENCE.test(lines.at(-1) ?? "")) lines.pop();
  return lines.join("\n").trim();
}

/** Every draft the reads hold, by its node id. */
export function treeDrafts(reads: IssueTreeReads, context: TreeContext): Map<string, Draft> {
  const drafts = new Map<string, Draft>();
  addDocumentBlocks(drafts, reads, context);
  addChildren(drafts, reads);
  const asks = addAsks(drafts, reads, context);
  const changes = decisionChanges(reads.decisions, reads.comments);
  addTurns(drafts, reads, context, asks, changes);
  orderConversation(drafts);
  addDecisions(drafts, reads, context, asks, changes);
  addRequirements(drafts, reads, context);
  addChecks(drafts, reads, context);
  // What waits for the reader's review is the review list's one rule (R35): the tie to a
  // statement decides it, and a decision the reader asked to change waits on its agent, not on
  // the reader (`decision-review.ts`). It is read once every statement has its draft.
  for (const decision of reads.decisions?.decisions ?? []) {
    const draft = drafts.get(context.nodeIdOf(decision.ref) ?? "");
    if (draft === undefined) continue;
    draft.waiting = inReviewList(
      decision,
      (reference) => drafts.get(context.nodeIdOf(reference) ?? "")?.type === "requirement",
      changes.has(decision.ref)
    );
  }
  servePlacements(drafts, context);
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
 * the document at its own address in place of the outline (`NodeRow.tsx`'s `Chip`). The chip is
 * named by the document's name, which the issue's own read carries, and addresses the document
 * as `artifact/<slug>` - the spec too, whose `spec` reference is the Spec filter's address. A
 * citation of one block of a document names that block, which the text's own link opens, and
 * no chip.
 */
function addDocumentChips(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  const artifacts = reads.issue.artifacts ?? [];
  if (artifacts.length === 0) return;
  const bySlug = new Map(artifacts.map((artifact) => [artifact.slug, artifact]));
  for (const draft of drafts.values()) {
    // The words a node is drawn by can leave a citation out - the coverage read drops every
    // `://` word of a requirement (`blockWords`, `api/results.go`) - so the node's own source,
    // which the reference read names it by, is read as well.
    const source = draft.reference === undefined ? "" : context.chipOf(draft.reference).name;
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
      draft.chips = [...draft.chips, { answered: true, gone: false, name: artifact.name, ref }];
    }
  }
}

/** Where each document of the issue stands among its own, by its artifact id: the spec first
 *  and the rest in the order the issue carries them, which is the order their blocks are drawn
 *  in under the issue (`ROOT_ORDER.block`). */
function documentRanks(reads: IssueTreeReads): ReadonlyMap<string, number> {
  const ranks = new Map<string, number>();
  (reads.issue.artifacts ?? []).forEach((artifact, index) => {
    ranks.set(artifact.id, artifact.primary ? 0 : index + 1);
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
 */
function addDocumentBlocks(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  const types: readonly string[] = NODE_TYPES;
  const ranks = documentRanks(reads);
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
      draft.text = context.chipOf(reference).name;
      // The block's own words whole, as the server reads them, where the reference read sent
      // them: the name above is the stored excerpt, cut at 480 runes and unread.
      const tree = context.treeOf(reference);
      if (tree !== undefined) {
        draft.tree = tree;
        draft.source = textSourceOf("artifact", document.artifactId);
      }
      // A check with no result recorded against it is in no state the results read would name,
      // and `no-result` is that state (`NODE_STATE_WORDS.check`), so a check drawn before the
      // results read answers says so rather than drawing a state nothing recorded.
      if (block.type === "check") draft.state = "no-result";
      if (block.type === "ask") draft.state = "open";
    });
  }
}

/**
 * The issue's children, each a node of the tree and a row of the outline, one group of them in
 * the issue read's order: its title as its words, and how it stands, what blocks it, who holds it
 * and its key as the tag drawn from the child itself (the architect's ruling 5 on R12,
 * `IssueChildren.tsx`). The model places a child under the words that filed it once it holds
 * that edge (dispatch://INFRA-1253); today it holds none, so the group sits under the issue. A
 * child's own tree is its own page, so a child holds no node below it.
 */
function addChildren(drafts: Map<string, Draft>, reads: IssueTreeReads): void {
  (reads.issue.children ?? []).forEach((child, index) => {
    const draft = add(drafts, blank(child.key, "child_issue", ROOT_ORDER.child + index));
    draft.child = child;
    draft.text = child.title;
  });
}

/**
 * Every question asked of the issue: the block the server indexed it from, where it has one,
 * else a node of its own under the issue. An approval request belongs to the document it reviews
 * rather than to the reasoning, so the tree draws none.
 *
 * A person's answer is part of the ask and no node of its own (dispatch://INFRA-1150 R3): it is
 * kept on the ask's draft as the store holds it, which the ask's own row and body draw.
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
          : { description: option.description }),
        label: option.label,
      }));
    }
    if (ask.multiple) draft.multiple = true;
    draft.reference = buildDispatchReference({ id: ask.id, key: context.key, kind: "ask" });
    draft.source = textSourceOf("ask", ask.id);
    draft.state = askState(ask);
    draft.text = ask.question;
    draft.tree = ask.question_tree;
    // An open question is what the fold unfolds the path to, whatever its depth (R6).
    draft.question = ask.state === "open";
    // An open question whose turn is the reader's own waits on the reader, and is what the
    // header's count of what waits puts the focus on (R13, `waiting.ts`): the person the server
    // resolved it waits on (`waiting_on_person`), as every other count of what waits on the
    // reader reads it (`inbox/BlockedOnYou.tsx`'s `waitingOnYou`). A question waiting on
    // somebody else is open all the same, and the fold still unfolds the path to it.
    draft.waiting =
      ask.state === "open" &&
      ask.waiting_on === "human" &&
      context.viewer !== undefined &&
      ask.waiting_on_person === context.viewer;
  }
  return placed;
}

/** An ask's answer as the tree holds it: the person it is theirs, with the agent they answered
 *  through where they did, and their choices and words as stored. */
function answerOf(answer: NonNullable<Ask["answer"]>): NodeAnswer {
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
function askState(ask: Ask): AskState | undefined {
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
  readonly id: string;
  readonly kind: "comment" | "message";
  /** The node it answers, `null` for a root turn under the issue. */
  readonly replyTo: string | null;
}

/**
 * The conversation's own turns: each comment and each message a person wrote, or that sits in a
 * thread a person wrote in, a node of this tree, a root turn under the issue and a reply under
 * the entry it answers (R1, R16). A thread head a page never read is no node, so a reply whose
 * parent is missing falls back to the issue, as every node whose parent is absent does
 * (`assemble`). A thread no person wrote in is no node at all: it is the history's, in the panel
 * (`agents-only.ts`, R15), until a person replies in it.
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
 * under the ask's node, whatever state the ask is in, a resolved one included
 * (dispatch://INFRA-3677). A message carries no such column, so its thread stays as
 * `in_reply_to` holds it. A comment that is a change a person asked of a decision, and answers
 * nothing else, is drawn under that decision (`decision-review.ts`'s `decisionChanges`), so the
 * change stands tied to the decision it changes.
 *
 * An address naming a comment or a message of the outline (`/comments/<id>`, `/messages/<id>`,
 * `?comment=`) is therefore an address of a row of this tree.
 */
function addTurns(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext,
  asks: ReadonlyMap<string, string>,
  changes: ReadonlyMap<string, ChangeComment>
): void {
  const history = agentsOnlyIds(reads.comments, reads.messages);
  const changeOf = new Map([...changes].map(([ref, comment]) => [comment.id, ref]));
  const turns: Turn[] = [
    ...(reads.comments ?? []).map((comment) => ({
      at: comment.created_at,
      author: comment.author,
      body: comment.body,
      bodyTree: comment.body_tree,
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
    draft.source = textSourceOf(turn.kind, turn.id);
    draft.text = turn.body;
    draft.tree = turn.bodyTree;
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
 * names every spec block it links as a chip, since a link places nothing (`shaped` is
 * dispatch://INFRA-1150's, `packages/contracts/src/dispatch-api.ts:2519-2534`).
 */
function addDecisions(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext,
  asks: ReadonlyMap<string, string>,
  changes: ReadonlyMap<string, ChangeComment>
): void {
  (reads.decisions?.decisions ?? []).forEach((decision, index) => {
    const id = context.nodeIdOf(decision.ref);
    if (id === undefined) return;
    const held = drafts.get(id);
    const draft = add(drafts, blank(id, "decision", ROOT_ORDER.decision + index));
    const answers = decision.asks ?? [];
    // A target with no address of its own is named by nothing, so there is no chip to draw for
    // it (`GraphNode.ref` is optional).
    draft.chips = [
      ...(decision.links ?? []).flatMap((link) =>
        link.to.ref === undefined ? [] : [{ ...context.chipOf(link.to.ref), stale: link.stale }]
      ),
      ...answers
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
    // The words are a block of the decisions document, which is what their references resolve
    // against (`TreeNode.source`).
    draft.source = textSourceOf("artifact", reads.decisions?.document);
    draft.text = decision.text;
    draft.tree = decision.text_tree;
    // How the decision stands with the reader, drawn as this node's own state word and shape
    // the way every other state is (R21). Whether it waits for the reader's review is the
    // review list's rule, read once every statement has its draft (`treeDrafts`).
    draft.state = decisionState(decision, changes.has(decision.ref));
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
 * tells none of them apart (R10). Where a decision claims to have shaped a requirement whose own
 * origin names another node, the requirement is placed by its `from` and the disagreement is
 * drawn as a gap, and a decision is never a requirement's parent (R4): a `from` that names one
 * places nothing, and the requirement sits under the issue.
 */
function addRequirements(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  const shapedBy = new Map<string, number>();
  for (const decision of reads.decisions?.decisions ?? []) {
    for (const link of decision.links ?? []) {
      const shaped = link.to.ref === undefined ? undefined : context.nodeIdOf(link.to.ref);
      if (shaped === undefined) continue;
      shapedBy.set(shaped, (shapedBy.get(shaped) ?? 0) + 1);
    }
  }
  // The spec, which a statement's words are a block of and its references resolve against.
  const specId = (reads.issue.artifacts ?? []).find((artifact) => artifact.primary)?.id;
  (reads.coverage?.requirements ?? []).forEach((requirement, index) => {
    const id = blockNodeId(context.specSlug, requirement.block_id);
    const held = drafts.get(id);
    // The `from` edge is the single placement authority (R4): the requirement's own pointer,
    // never the decision that claims to have shaped it.
    const origin = context.nodeIdOf(held?.pointers.from?.[0] ?? "");
    const draft = add(drafts, blank(id, "requirement", ROOT_ORDER.requirement + index));
    draft.chips = held?.chips ?? [];
    draft.counts = { checks: requirement.checks, planTasks: requirement.plan_tasks };
    draft.gaps = requirementGaps(requirement.gaps ?? [], origin, shapedBy.get(id) ?? 0);
    draft.parents = origin === undefined || drafts.get(origin)?.type === "decision" ? [] : [origin];
    draft.pointers = held?.pointers ?? {};
    draft.reference = requirement.ref;
    draft.text = requirement.text;
    // The server's reading of the statement's own words, with the references the coverage
    // read's plain words leave out: the read's own, else the one its block carried.
    const tree = requirement.text_tree ?? held?.tree;
    if (tree !== undefined) {
      draft.tree = tree;
      draft.source = held?.source ?? textSourceOf("artifact", specId);
    }
  });
}

/** The gaps on one requirement: the one the coverage read carries that the page draws, and a
 *  decision claiming to have shaped a requirement that came from elsewhere. */
function requirementGaps(
  coverage: readonly string[],
  origin: string | undefined,
  shaped: number
): GapKind[] {
  const gaps = coverage.flatMap((line): GapKind[] => {
    const kind = coverageGapKind(line);
    return kind === undefined ? [] : [kind];
  });
  if (origin !== undefined && shaped > 0) gaps.push("shaped_disagreement");
  return gaps;
}

/**
 * Each check of the issue's Verification document with the results recorded against it: the
 * check's own words, how it stands and the four attributes its body draws, and one node per
 * result under the check it was recorded against (R4, "a result under its check"), each with
 * what that result recorded (`check-facts.ts`). The check's `serves` is no part of this read
 * (`CheckBlock` carries none), so its placement is the block's own pointer, below.
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
    draft.state = checkState(check);
    draft.text = check.text;
    const tree = check.text_tree ?? held?.tree;
    if (tree !== undefined) {
      draft.tree = tree;
      draft.source = held?.source ?? textSourceOf("artifact", check.artifact_id);
    }
    (check.results ?? []).forEach((result, index) => {
      const row = add(drafts, blank(result.id, "result", index));
      row.at = result.created_at;
      row.author = result.actor;
      row.facts = resultFacts(result);
      row.parents = [id];
      row.text = result.observed;
    });
  }
}

/**
 * How a check stands, in the states `NODE_STATE_WORDS.check` names. Whether it passed is the
 * server's own answer over every result of the check (`pass_stands`, main's e98fc56ef8): a pass
 * recorded before a later fail still stands, while a retracted pass, a pass recorded against an
 * older wording of the check and a pass at a commit the check no longer compares with stand for
 * nothing. The other states read what no pass standing is:
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
 * - **`failed`,** a newest result that stands, at the work the check compares with.
 * - **`unmarked`,** where nothing could be judged: a repository Dispatch cannot read, or a
 *   commit it cannot place.
 *
 * A retracted result is no evidence, as the server counts it (`results_comparison.go`'s
 * `passStandsOf` skips it), so `older` and `failed` are read from the newest result that still
 * stands and never from a withdrawn one: a pass taken back at the compared commit over an older
 * pass at another commit is `older`, and a pass taken back over a fail that stands is `failed`,
 * whatever commit the withdrawn pass ran at.
 *
 * It is the one reading of a check's state the page has: the Verification filter draws the
 * node, the head's checks line draws the same states, and each is told apart by a word and a shape (R15,
 * R21). `e2e/issue-layout-seed-support.ts` waits on seeded checks by this function, so a seed
 * waits for the states the page draws rather than for a copy of this rule.
 */
export function checkState(check: CheckStateReads): CheckState {
  const results = check.results ?? [];
  if (results.length === 0) return "no-result";
  if (check.pass_stands === undefined) return "unreadable";
  if (check.pass_stands.answer === "yes") return "passed";
  if (check.pass_stands.answer === "unknown") return "unmarked";
  // The results arrive newest first, so this is the newest one a retraction left standing.
  const standing = results.find((result) => result.retracted === undefined);
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

/**
 * A check and a plan task sit under each requirement their `serves` names, edited under the
 * first and transcluded under the rest (R4); what their `needs` names is a chip, as every edge
 * that is not the spine is. A `serves` naming no requirement of this issue places nothing, so
 * the block keeps its place under the issue.
 *
 * Two of the chain's gaps are read here, from the pointers this pass already resolves (R10):
 *
 * - **A citation whose target is no longer a requirement.** `serves` points only at a
 *   requirement (`packages/envoy/internal/dispatch/pmdoc/schema/blocks.json:81-84`), and the
 *   write that introduces one is refused where it does not (`pmdoc.CheckBlockPointers`); a
 *   target retyped after that write leaves the citation naming a node of another kind, which
 *   the drafts hold the type of. A reference this issue does not address, or one no draft
 *   answers, is no such citation: the page cannot say what it is not holding, and a target
 *   nothing answers to at all is already drawn as a chip that says so.
 * - **A plan task nobody asked for.** A task naming no requirement it serves and no
 *   declaration it attempts is work no reason stands behind. `attempts` is the declarations
 *   route of dispatch://INFRA-1278, read from the same `block[]` attributes as every other
 *   pointer, so the day it lands this pass reads it and nothing else changes. A check is held
 *   to neither: a check is there to prove a requirement, and it attempts no declaration at
 *   all.
 */
function servePlacements(drafts: Map<string, Draft>, context: TreeContext): void {
  for (const draft of drafts.values()) {
    if (draft.type !== "check" && draft.type !== "plan_task") continue;
    const named = (draft.pointers.serves ?? []).map(context.nodeIdOf);
    const serves = named.filter(
      (id): id is string => id !== undefined && drafts.get(id)?.type === "requirement"
    );
    if (serves.length > 0) draft.parents = serves;
    const elsewhere = named.some((id) => {
      const target = id === undefined ? undefined : drafts.get(id);
      return target !== undefined && target.type !== "requirement";
    });
    if (elsewhere) draft.gaps = [...draft.gaps, "serves_not_requirement"];
    if (
      draft.type === "plan_task" &&
      (draft.pointers.serves ?? []).length === 0 &&
      (draft.pointers.attempts ?? []).length === 0
    ) {
      draft.gaps = [...draft.gaps, "nobody_asked"];
    }
    draft.chips = [
      ...draft.chips,
      ...(draft.pointers.needs ?? []).map((reference) => ({
        ...context.chipOf(reference),
        label: blockAttributeLabel("needs"),
      })),
    ];
  }
}
