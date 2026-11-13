import { parseBlockPointerValue } from "@legion/contracts";

import type { Actor, Ask, CheckBlock, Decision, DisplayTree } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { buildDispatchReference, parseNodeReference } from "../refs/routes";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { checkFacts, resultFacts } from "./check-facts";
import type {
  IssueTreeReads,
  NodeAnswer,
  NodeChip,
  NodeCounts,
  NodeFact,
  TreeAsk,
} from "./issue-tree";
import {
  type AskState,
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
 * unless the model gives a block a parent of its own (R4). A child issue is a node of the tree
 * where the model places it, which today is under the issue, as well as a line of the children
 * section above it (`IssueChildren.tsx`, R12).
 */

/** The id of the row of one block of one document, which is the block's own address inside the
 *  issue: a block id alone is unique only inside its document, and an issue carries several. */
export function blockNodeId(slug: string, blockId: string): string {
  return `${slug}#b-${blockId}`;
}

/** One node as it is being built: a `TreeNode`'s own fields, plus where the model placed it. */
export interface Draft {
  author?: Actor;
  /** A person's answer to this ask, which the ask's own row and body draw (`TreeNode.answer`). */
  answer?: NodeAnswer;
  chips: NodeChip[];
  counts?: NodeCounts;
  /** What the node's body draws under its text (`check-facts.ts`). */
  facts: NodeFact[];
  gaps: GapKind[];
  readonly id: string;
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
  /** Whether this decision waits for the reader's review (`Decision.waiting_review`). */
  waiting: boolean;
}

/**
 * Where a child of the issue sits among the issue's own children: the typed blocks of its
 * documents first, the spec's before the rest, each in its own document's order, so the
 * questions inside the spec open at the top of the tree; then the decisions no question holds,
 * then the requirements, then the questions asked of the issue itself, then its child issues,
 * and the conversation last. A node's order under any other parent is its own place in what it
 * came from.
 */
export const ROOT_ORDER = {
  ask: 3000,
  /** The first block of the issue's spec; every other document's blocks follow, one whole
   *  number per document (`documentRanks`), which keeps them under the decisions' `1000`. */
  block: 0,
  child: 4000,
  decision: 1000,
  requirement: 2000,
  turn: 5000,
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
        name: gone ? "" : (target?.name ?? ""),
        ref: reference,
      };
    },
    key,
    nodeIdOf,
    specSlug,
    slugOf: (artifactId: string) => slugs.get(artifactId),
  };
}

/** Every draft the reads hold, by its node id. */
export function treeDrafts(reads: IssueTreeReads, context: TreeContext): Map<string, Draft> {
  const drafts = new Map<string, Draft>();
  addDocumentBlocks(drafts, reads, context);
  addChildren(drafts, reads);
  const asks = addAsks(drafts, reads, context);
  addTurns(drafts, reads, context);
  addDecisions(drafts, reads, context, asks);
  addRequirements(drafts, reads, context);
  addChecks(drafts, reads, context);
  servePlacements(drafts, context);
  return drafts;
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
      // A check with no result recorded against it is in no state the results read would name,
      // and `no-result` is that state (`NODE_STATE_WORDS.check`), so a check drawn before the
      // results read answers says so rather than drawing a state nothing recorded.
      if (block.type === "check") draft.state = "no-result";
      if (block.type === "ask") draft.state = "open";
    });
  }
}

/** The issue's children, each one node under the issue, where the model places every child
 *  today (R4, R12): a child's own tree is its own page. */
function addChildren(drafts: Map<string, Draft>, reads: IssueTreeReads): void {
  (reads.issue.children ?? []).forEach((child, index) => {
    const draft = add(drafts, blank(child.key, "child_issue", ROOT_ORDER.child + index));
    draft.text = child.title;
    // Named by the issue read itself rather than by the batched reference read, so there is
    // nothing left to wait for: a blocking issue's key is both its name here and its whole
    // address (`answered`, `NodeChip`).
    draft.chips = (child.blocked_by ?? []).map((blocked) => ({
      answered: true,
      gone: false,
      name: blocked,
      ref: `dispatch://${blocked}`,
    }));
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
    const draft = drafts.get(id) ?? add(drafts, blank(id, "ask", ROOT_ORDER.ask));
    placed.set(ask.id, id);
    draft.author = ask.author;
    if (ask.answer !== null) draft.answer = answerOf(ask.answer);
    draft.reference = buildDispatchReference({ id: ask.id, key: context.key, kind: "ask" });
    draft.source = textSourceOf("ask", ask.id);
    draft.state = askState(ask);
    draft.text = ask.question;
    draft.tree = ask.question_tree;
    // An open question is what the fold unfolds the path to, whatever its depth (R6).
    draft.question = ask.state === "open";
  }
  return placed;
}

/** An ask's answer as the tree holds it: the person it is theirs, with the agent they answered
 *  through where they did, and their choices and words as stored. */
function answerOf(answer: NonNullable<Ask["answer"]>): NodeAnswer {
  return {
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
  /** The turn it answers, `null` for a root turn. */
  readonly replyTo: string | null;
}

/**
 * The conversation's own turns: each comment and each message a node of this tree, a root turn
 * under the issue and a reply under the turn it answers (R1, R19). A thread head a page never
 * read is no node, so a reply whose parent is missing falls back to the issue, as every node
 * whose parent is absent does (`assemble`).
 *
 * One conversation is one order, so the turns are drawn by the time they were made and not by
 * their place in the read that carried them: the comments and the messages are two lists, each
 * counting from zero, and a message sent today drawn among yesterday's comments is the reader
 * reading the conversation out of order. Two turns made in the same instant are drawn by id, so
 * the same reads always draw the same tree.
 *
 * A comment is drawn under the entry it answers (R14): `answers`, the comment its write named,
 * and never `reply_to`, which that same write climbed to the head of the thread before it
 * stored (`packages/envoy/internal/dispatch/api/comment_create.go`'s `threadHeadOf`), so a
 * reply to a reply parented by `reply_to` would be drawn under the thread's head. A comment
 * stored before `comments.answers` existed (migration 20261105014808) carries none, and its
 * thread's head is then the one entry it names. A message carries no such column, so its
 * thread stays as `in_reply_to` holds it.
 *
 * The page draws these rows *and* the panel draws the same conversation (R12): the tree is the
 * issue's one ordered body, and the panel is where a reader answers. An address naming a
 * comment or a message (`/comments/<id>`, `/messages/<id>`, `?comment=`) is therefore an
 * address of a row of this tree.
 */
function addTurns(drafts: Map<string, Draft>, reads: IssueTreeReads, context: TreeContext): void {
  const turns: Turn[] = [
    ...(reads.comments ?? []).map((comment) => ({
      at: comment.created_at,
      author: comment.author,
      body: comment.body,
      bodyTree: comment.body_tree,
      id: comment.id,
      kind: "comment" as const,
      replyTo: comment.answers ?? comment.reply_to,
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
  ];
  turns.sort((one, other) => compareTimestamps(one.at, other.at) || one.id.localeCompare(other.id));
  turns.forEach((turn, index) => {
    const draft = add(drafts, blank(turn.id, turn.kind, ROOT_ORDER.turn + index));
    draft.author = turn.author;
    draft.parents = turn.replyTo === null ? [] : [turn.replyTo];
    draft.reference = buildDispatchReference({ id: turn.id, key: context.key, kind: turn.kind });
    draft.source = textSourceOf(turn.kind, turn.id);
    draft.text = turn.body;
    draft.tree = turn.bodyTree;
  });
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
  asks: ReadonlyMap<string, string>
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
    draft.pointers = held?.pointers ?? {};
    draft.reference = decision.ref;
    draft.replaced = decision.replaced;
    // The words are a block of the decisions document, which is what their references resolve
    // against (`TreeNode.source`).
    draft.source = textSourceOf("artifact", reads.decisions?.document);
    draft.text = decision.text;
    draft.tree = decision.text_tree;
    // How the decision stands with the reader, drawn as this node's own state word and shape
    // the way every other state is (R21). It is what the old Decisions tab's four sections
    // told a reader by grouping, and the one a reader looks for - what waits for their review
    // - wins where a decision would be in two (`NODE_STATE_WORDS.decision`).
    draft.state = decisionState(decision);
    // What waits for the reader: its node is where Keep and Change are drawn, and the one the
    // Decisions preset opens on (R7, `DecisionActions.tsx`).
    draft.waiting = decision.waiting_review;
    if (decision.unsure) draft.unsure = true;
  });
}

/**
 * How a decision stands with the reader, in the three states the page draws
 * (`NODE_STATE_WORDS.decision`). The reading is the one the old Decisions tab grouped by, in
 * the order a reader reads it: what waits for their review first, because that is what they
 * come to the preset for; then a decision replaced by a later one; then one that is theirs - a
 * person's own, or an agent's they kept. A decision in none of the three is an agent's that
 * nobody has reviewed and nothing replaced, which stood in no section of that tab, so it is in
 * no state here and the row draws none.
 */
function decisionState(decision: Decision): DecisionState | undefined {
  if (decision.waiting_review) return "waiting_review";
  if (decision.replaced) return "replaced";
  if (decision.kind === "person" || decision.kept !== null) return "yours";
  return undefined;
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
    (check.results ?? []).forEach((result, index) => {
      const row = add(drafts, blank(result.id, "result", index));
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
 * It is the one reading of a check's state the page has: the Checks preset draws the node, the
 * head's Checks line draws the same states, and each is told apart by a word and a shape (R15,
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
    draft.chips = [...draft.chips, ...(draft.pointers.needs ?? []).map(context.chipOf)];
  }
}
