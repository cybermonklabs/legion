import { parseBlockPointerValue } from "@legion/contracts";

import type { Actor, ArtifactBlock, Ask, Decision } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { buildDispatchReference, parseNodeReference } from "../refs/routes";
import { checkFacts, resultFacts } from "./check-facts";
import type { IssueTreeReads, NodeChip, NodeCounts, NodeFact } from "./issue-tree";
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
 * read, and no request is made per node.
 */

/** The id of the row of one block of one document, which is the block's own address inside the
 *  issue: a block id alone is unique only inside its document, and an issue carries several. */
export function blockNodeId(slug: string, blockId: string): string {
  return `${slug}#b-${blockId}`;
}

/** One node as it is being built: a `TreeNode`'s own fields, plus where the model placed it. */
export interface Draft {
  author?: Actor;
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
  state?: NodeState;
  text: string;
  type: NodeType;
  /** Whether this decision waits for the reader's review (`Decision.waiting_review`). */
  waiting: boolean;
}

/**
 * Where a child of the issue sits among the issue's own children: its documents first, so the
 * spec and the path to the questions inside it open at the top of the tree, then the decisions,
 * then the requirements, then the questions asked of the issue itself, then its children, and
 * the conversation last. A node's order inside a document is the block's own place in it.
 */
export const ROOT_ORDER = {
  ask: 3000,
  child: 4000,
  decision: 1000,
  document: 0,
  other: 6000,
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
  // Where each document of the issue stands among its own, read once: it decides both which
  // citation of a ring is cut (`openRings`) and the order the documents are drawn in, and a
  // second reading of it could open a ring at a document other than the one drawn first.
  const ranks = documentRanks(reads);
  const cited = citations(reads, context, ranks);
  addDocuments(drafts, reads, context, cited, ranks);
  addDocumentBlocks(drafts, reads, context);
  addChildren(drafts, reads);
  addAsks(drafts, reads, context);
  addTurns(drafts, reads, context);
  addDecisions(drafts, reads, context);
  addRequirements(drafts, reads, context);
  addChecks(drafts, reads, context);
  settleCitations(drafts, cited);
  servePlacements(drafts, context);
  return drafts;
}

/** Where one artifact of the issue is cited: the block whose own words hold its address, and
 *  the document that block belongs to. */
interface Citation {
  /** The slug of the document the citing block belongs to, which is where the cited document
   *  sits when the citation stands in prose that is no node of its own. */
  readonly document: string;
  /** The node id of the citing block, `<slug>#b-<block id>`. */
  readonly id: string;
}

/**
 * Which block of which document cites each artifact of the issue, by the cited artifact's slug:
 * the first of the citing blocks in document order, because a document cited twice is drawn
 * once, under the first block that names it.
 *
 * The citation is the server's: `GET /issues/{key}/references?own=1` answers each document the
 * issue reaches, its own included, with the blocks of its citer (`via_blocks`,
 * `packages/envoy/internal/dispatch/refs/closure.go`). Reading it here rather than scanning
 * each document's markdown is what keeps the first load from reading the stored text of a
 * document the reader has not opened (dispatch://INFRA-1196 R20).
 *
 * A member whose via is not a document of this issue, or whose citer the page holds no blocks
 * for, places nothing: the citation is drawn as a node under a block, and there is no block to
 * draw it under. Neither does a citation that would place a document inside itself, which
 * `openRings` takes out.
 */
function citations(
  reads: IssueTreeReads,
  context: TreeContext,
  ranks: ReadonlyMap<string, number>
): Map<string, Citation> {
  const found = new Map<string, Citation>();
  const listings = new Map<string, readonly ArtifactBlock[]>(
    (reads.documents ?? []).map((document) => [document.artifactId, document.blocks])
  );
  for (const member of reads.references?.members ?? []) {
    // The cited artifact is this issue's by its id and never by its slug: a slug is unique
    // inside one issue only (`unique (issue_key, slug)`), so another issue's `plan-md` read as
    // this issue's would draw this issue's `plan-md` under the block citing that one.
    const slug = context.slugOf(member.artifact.id);
    if (slug === undefined || found.has(slug)) continue;
    if (member.via.kind !== "artifact") continue;
    const document = context.slugOf(member.via.id);
    const cites = member.via_blocks ?? [];
    if (document === undefined || cites.length === 0 || document === slug) continue;
    const id = citingRow(listings.get(member.via.id), cites);
    if (id === undefined) continue;
    found.set(slug, { document, id: blockNodeId(document, id) });
  }
  openRings(found, ranks);
  return found;
}

/** Where each document of the issue stands among its own: the spec first and the rest in the
 *  order the issue carries them, which is the order their rows are drawn in (`ROOT_ORDER`). A
 *  slug the issue does not carry ranks last. */
function documentRanks(reads: IssueTreeReads): ReadonlyMap<string, number> {
  const ranks = new Map<string, number>();
  (reads.issue.artifacts ?? []).forEach((artifact, index) => {
    ranks.set(artifact.slug, artifact.primary ? 0 : index + 1);
  });
  return ranks;
}

/**
 * Takes out every citation that would place a document inside a document it already holds. A
 * spec that cites the plan while the plan cites the spec is ordinary data, and the server
 * answers each one via the other (`?own=1`), so each would be placed under the other and
 * neither would be drawn at all: the tree is walked downwards from the issue, and a ring
 * hangs off nothing.
 *
 * The ring is opened at the citation whose citing document comes last among the issue's own,
 * so the spec keeps the documents it cites under it and the document citing the spec back
 * leaves the spec where it was, under the issue. The decision is made in that same order and
 * never in the order the references read answered in, so the same documents always draw the
 * same tree.
 */
function openRings(found: Map<string, Citation>, ranks: ReadonlyMap<string, number>): void {
  const rank = (slug: string): number => ranks.get(slug) ?? Number.MAX_SAFE_INTEGER;
  const holder = new Map<string, string>();
  const decided = [...found].sort(
    ([slug, one], [otherSlug, other]) =>
      rank(one.document) - rank(other.document) ||
      rank(slug) - rank(otherSlug) ||
      slug.localeCompare(otherSlug)
  );
  for (const [slug, citation] of decided) {
    let above: string | undefined = citation.document;
    while (above !== undefined && above !== slug) above = holder.get(above);
    if (above === slug) found.delete(slug);
    else holder.set(slug, citation.document);
  }
}

/**
 * Which block of `blocks` the citation is drawn on. The server names the block its index holds
 * the link in, which is the innermost one - the paragraph inside a `requirement`, not the
 * requirement - and the tree draws a row for the listed block that holds that paragraph. So the
 * row is the first listed block, in document order, whose byte span covers a citing block's,
 * which is the outermost of them; a listing the page has not read yet, or one holding none of
 * the citing blocks, falls back to the first id the server named.
 */
function citingRow(
  blocks: readonly ArtifactBlock[] | undefined,
  cites: readonly string[]
): string | undefined {
  if (blocks === undefined) return cites[0];
  const cited = blocks.filter((block) => cites.includes(block.id));
  if (cited.length === 0) return cites[0];
  const holder = blocks.find((block) =>
    cited.some((inner) => block.from <= inner.from && inner.to <= block.to)
  );
  return holder?.id ?? cites[0];
}

/**
 * Where each cited document finally sits. A document a block cites is a node under that block
 * (R16), which is what `addDocuments` wrote; where the citation stands in prose that is no node
 * of its own - a paragraph is a document's own text and no node (`addDocumentBlocks`) - the
 * document is a node under the document that cites it instead, never under an invented parent.
 */
function settleCitations(drafts: Map<string, Draft>, cited: ReadonlyMap<string, Citation>): void {
  for (const [slug, citation] of cited) {
    const draft = drafts.get(slug);
    if (draft === undefined || drafts.has(citation.id)) continue;
    draft.parents = [citation.document];
  }
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
 * The documents and images the issue carries. A document a block cites sits under that block
 * (R16) - the block `citations` found, reading the references read by the cited artifact's id;
 * one nothing cites sits under the issue, which is where a node whose spine parent is absent
 * goes.
 */
function addDocuments(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext,
  cited: ReadonlyMap<string, Citation>,
  ranks: ReadonlyMap<string, number>
): void {
  (reads.issue.artifacts ?? []).forEach((artifact, index) => {
    if (artifact.kind !== "doc" && artifact.kind !== "image") return;
    const cites = cited.get(artifact.slug);
    const draft = add(
      drafts,
      blank(
        artifact.slug,
        artifact.kind === "doc" ? "document" : "image",
        // `documentRanks` is the one reading of where a document stands; every document of the
        // issue is in it, since it is built from this same list.
        ROOT_ORDER.document + (ranks.get(artifact.slug) ?? index + 1)
      )
    );
    draft.author = artifact.created_by;
    draft.parents = cites === undefined ? [] : [cites.id];
    draft.reference = buildDispatchReference({
      key: context.key,
      kind: "artifact",
      slug: artifact.slug,
    });
    draft.text = artifact.name;
  });
}

/**
 * Each document's typed blocks, in document order. A block of no declared node type - a
 * paragraph, a heading, a table - is the document's own text and no node of the tree. A block's
 * `block[]` attributes are kept as its pointers, which is where `serves` and `needs` are read
 * from; its words come from the batched reference read, since nothing reads the document's text.
 */
function addDocumentBlocks(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  const types: readonly string[] = NODE_TYPES;
  for (const document of reads.documents ?? []) {
    const slug = context.slugOf(document.artifactId);
    if (slug === undefined) continue;
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
      const draft = add(drafts, blank(blockNodeId(slug, block.id), block.type as NodeType, index));
      draft.parents = [slug];
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

/** The issue's children, each one node under the issue: a child's own tree is its own page. */
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
 */
function addAsks(drafts: Map<string, Draft>, reads: IssueTreeReads, context: TreeContext): void {
  const asks = new Map<string, Ask>();
  for (const ask of [...(reads.issue.open_asks ?? []), ...(reads.asks ?? [])]) {
    if (ask.kind === "question") asks.set(ask.id, ask);
  }
  for (const ask of asks.values()) {
    // The document holding the typed `ask` block is `block_artifact`; `artifact_id` is the
    // document an anchored ask is anchored in, which a block ask does not carry.
    const slug = ask.block_artifact?.slug ?? context.slugOf(ask.artifact_id ?? "");
    const block = ask.block_id ?? undefined;
    const id = slug !== undefined && block !== undefined ? blockNodeId(slug, block) : ask.id;
    const draft = drafts.get(id) ?? add(drafts, blank(id, "ask", ROOT_ORDER.ask));
    draft.author = ask.author;
    draft.reference = buildDispatchReference({ id: ask.id, key: context.key, kind: "ask" });
    draft.state = askState(ask);
    draft.text = ask.question;
    // An open question is what the fold unfolds the path to, whatever its depth (R6).
    draft.question = ask.state === "open";
  }
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
    draft.text = turn.body;
  });
}

/**
 * Each decision of the issue's `decisions.md`. A decision sits under the question it settles;
 * the model holds no such edge today (`packages/contracts/src/dispatch-api.ts:2519-2534`: the
 * read answers `kind` and a boolean `unsure`, and no `shaped`), so every decision's spine parent
 * is absent and it sits under the issue with the gap that says so, as any other node whose spine
 * parent is absent does (R4) - never parked inside `decisions.md`, where the fold would close
 * the reasoning away behind a document. It names every spec block it links and every ask its
 * blocks mention as its chips, and the day the edge lands the question it settles is read from
 * the decision itself.
 */
function addDecisions(
  drafts: Map<string, Draft>,
  reads: IssueTreeReads,
  context: TreeContext
): void {
  (reads.decisions?.decisions ?? []).forEach((decision, index) => {
    const id = context.nodeIdOf(decision.ref);
    if (id === undefined) return;
    const held = drafts.get(id);
    const draft = add(drafts, blank(id, "decision", held?.order ?? index));
    // A target with no address of its own is named by nothing, so there is no chip to draw for
    // it (`GraphNode.ref` is optional).
    draft.chips = [
      ...(decision.links ?? []).flatMap((link) =>
        link.to.ref === undefined ? [] : [{ ...context.chipOf(link.to.ref), stale: link.stale }]
      ),
      ...(decision.asks ?? []).map((ask) =>
        context.chipOf(buildDispatchReference({ id: ask, key: context.key, kind: "ask" }))
      ),
    ];
    // Under the issue: the question it settles is what would place it, and the model holds none.
    draft.gaps = ["no_origin"];
    draft.order = ROOT_ORDER.decision + index;
    draft.parents = [];
    draft.pointers = held?.pointers ?? {};
    draft.reference = decision.ref;
    draft.replaced = decision.replaced;
    draft.text = decision.text;
    // How the decision stands with the reader, drawn as this node's own state word and shape
    // the way every other state is (R21). It is what the old Decisions tab's four sections
    // told a reader by grouping, and the one a reader looks for - what waits for their review
    // - wins where a decision would be in two (`NODE_STATE_WORDS.decision`).
    draft.state = decisionState(decision);
    // What waits for the reader: its node is where Keep and Change are drawn, and the one the
    // Decisions preset opens on (R7, `DecisionActions.tsx`).
    draft.waiting = decision.waiting_review;
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
 * sits under the issue with the gap that says so, beside the two counts the coverage read holds
 * for it and the gaps that read already carries. Where a decision claims to have shaped a
 * requirement whose own origin names another node, the requirement is placed by its `from` and
 * the disagreement is drawn as a gap (R4), never placed under the decision.
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
    draft.parents = origin === undefined ? [] : [origin];
    draft.pointers = held?.pointers ?? {};
    draft.reference = requirement.ref;
    draft.text = requirement.text;
  });
}

/** The gaps on one requirement: its own origin, the two the coverage read already carries, and a
 *  decision claiming to have shaped a requirement that came from elsewhere. */
function requirementGaps(
  coverage: readonly string[],
  origin: string | undefined,
  shaped: number
): GapKind[] {
  const gaps: GapKind[] = [];
  if (origin === undefined) gaps.push("no_origin");
  for (const gap of coverage) {
    gaps.push(coverageGapKind(gap));
  }
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
    const draft = add(drafts, blank(id, "check", held?.order ?? 0));
    draft.facts = checkFacts(check);
    draft.parents = held?.parents ?? [slug];
    draft.pointers = held?.pointers ?? {};
    draft.reference = check.ref;
    draft.state = checkState(check.results ?? []);
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
 * How a check stands, from the newest result recorded against it, in the states
 * `NODE_STATE_WORDS.check` names: `older` and `unmarked` are about the result's currency rather
 * than its verdict, and a currency the server left out reads as `unmarked` because nothing
 * judged it. It is the one reading of a check's state the page has: the Checks preset draws
 * the node, and the five states are told apart by a word and a shape (R15, R21).
 */
function checkState(results: readonly { currency?: string; verdict: string }[]): CheckState {
  const latest = results[0];
  if (latest === undefined) return "no-result";
  if (latest.currency === undefined || latest.currency === "unknown") return "unmarked";
  if (latest.currency === "older") return "older";
  return latest.verdict === "pass" ? "passed" : "failed";
}

/**
 * A check and a plan task sit under each requirement their `serves` names, edited under the
 * first and transcluded under the rest (R4); what their `needs` names is a chip, as every edge
 * that is not the spine is. A `serves` naming no requirement of this issue places nothing, so
 * the block keeps its place under its own document.
 */
function servePlacements(drafts: Map<string, Draft>, context: TreeContext): void {
  for (const draft of drafts.values()) {
    if (draft.type !== "check" && draft.type !== "plan_task") continue;
    const serves = (draft.pointers.serves ?? [])
      .map(context.nodeIdOf)
      .filter((id): id is string => id !== undefined && drafts.get(id)?.type === "requirement");
    if (serves.length > 0) draft.parents = serves;
    draft.chips = [...draft.chips, ...(draft.pointers.needs ?? []).map(context.chipOf)];
  }
}
