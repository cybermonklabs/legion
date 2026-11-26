import { primarySpec } from "../../../api/issue-cache";
import type { Actor, Decision, DecisionLink, DisplayTree, IssueDetails } from "../../../api/types";
import { buildDispatchReference, parseDispatchReference } from "../../refs/routes";
import { wordsOf } from "../../refs/tree-fragment";
import {
  blockIdOfNode,
  type DecisionContent,
  decisionContent,
  firstOfEachId,
} from "../decision-review";
import type { IssueTreeReads, TreeAsk } from "../issue-tree";
import { storedReading } from "../stored-reading";
import { blockNodeId, deciderOf } from "../tree-drafts";
import type { TreeAnswer, TreeAnswerNode, TreeTuple } from "../tree-read/tree-read";

/**
 * The decisions of a tree read's answer, or of one of its views, in the shape the
 * decisions read answers them (`Decision`), so every reader of that read - the Decisions filter's
 * parts, the review list, the Spec filter's column, the header's count, Keep and Change - reads
 * them with no change of its own.
 *
 * A decision is a node of supertag `decision` that carries the model's decision (`decision`, the
 * contract's `Decision`), whose members the read answers there by the decisions read's names and
 * values, or a person's: a question they answered that carries `shaped` ties. The read answers a
 * person's decision on the answered ask's own `decision` too (`issue_tree_read.go`'s
 * `readDecisions`), and where it does, its members are read there; where it answers none, they
 * are the ones today's decisions read gives a person's decision (`api/decisions.go`'s
 * `readDecisions`).
 *
 * Beside the answer this reads the issue's key, which a node of this issue is addressed in, its
 * documents and its open asks, and the asks read: the answer holds no ask's answer, and it names
 * an ask block by its block, so the issue's asks are what say which ask a block is, and whether
 * an ask is answered where the read carries no person's decision on it (`askOfNode`), for the
 * outline (`tree-read/tree-nodes.ts`) as for every decision.
 *
 * This module is pure, and it imports no code of `issue-tree.ts` or of `tree-read/`, only their
 * types: the outline's steps (`tree-read/tree-nodes.ts`) are to read the decisions from here, and
 * `issue-tree.ts` is to build the outline from those steps (that module's own comment), so either
 * import would close a cycle (`biome.json`'s `noImportCycles`). So the rules a node is looked up
 * and addressed by live here, and the outline reads them from here (`tuplesByFrom`,
 * `nodeReference`, `tieReference`, `askIdOf`, `askOfNode`), beside the first node of each read
 * id, which both read from `decision-review.ts` (`firstOfEachId`); the heading is cut by
 * `decisionQuote` rather than `issue-tree.ts`'s `lineWords`. What `decisionsOfAnswer` keeps of
 * each answer it has read changes nothing it answers.
 */

/** One decision of the answer, as the decisions read answers a decision. */
export interface DecisionNode extends Decision {
  /** Who decided it: the person or the session its `decided_by` tuple names, which the read
   *  answers as context (`tree-drafts.ts`' `deciderOf`, the outline's one rule); absent where
   *  the read holds no such tuple. */
  readonly decidedBy?: Actor;
  /** The id of the document the node is a block of (its `document` member), which Keep and a
   *  Change's anchor are written against; undefined for an ask of the conversation, which is a
   *  block of no document. */
  readonly document: string | undefined;
  /** The tree node the decision is. */
  readonly node: TreeAnswerNode;
}

/** What `decisionNodes` reads beside the answer: the issue, for its key and, where the caller
 *  holds them, its documents and its open asks, and the asks read, which together say which ask
 *  an ask node is (`askOfNode`). The page's reads (`IssueTreeReads`) pass as they are. */
export type DecisionReads = Pick<IssueTreeReads, "asks"> & {
  readonly issue: Pick<IssueDetails, "key"> &
    Partial<Pick<IssueDetails, "artifacts" | "open_asks" | "primary_artifact_id">>;
};

/** The answer and the reads, looked up by the read's own ids: what `decisionNodes` reads them
 *  by, which the outline passes where it built them already (`tree-read/tree-nodes.ts`'
 *  `treeDraftsFrom`). */
export interface Lookup {
  /** The row of the issue's asks that the ask node is (`askOfNode`). */
  readonly askOf: (node: TreeAnswerNode) => TreeAsk | undefined;
  readonly key: string;
  /** The slug of the issue's spec (`primarySpec`), `""` where the reads name none: a tie to a
   *  block of it is named as the spec's own (`tieTargetReference`). */
  readonly specSlug: string;
  readonly nodeOf: (readId: string) => TreeAnswerNode | undefined;
  /** The tuples of the answer that start at the node `readId` names, in the answer's order. */
  readonly tuplesFrom: (readId: string) => readonly TreeTuple[];
}

/**
 * Every decision of `answer`, in the answer's own order: one for each node of supertag `decision`
 * that carries the model's decision, and one for each answered question that carries `shaped`
 * ties, and none for any other node. A decision node that carries no `decision` is no decision,
 * and neither is one the read places in no document: it has no address (`referenceOf`), which is
 * how every reader finds a decision, and no document Keep could be written against. `lookup` is
 * the answer and `reads` looked up by the read's own ids (`lookupOf`), which a caller that holds
 * them already passes.
 */
export function decisionNodes(
  answer: TreeAnswer,
  reads: DecisionReads,
  lookup: Lookup = lookupOf(answer, reads)
): DecisionNode[] {
  return (answer.nodes ?? []).flatMap((node) => {
    const decided =
      node.supertag === "decision"
        ? fromDecision(node, lookup)
        : node.supertag === "ask"
          ? fromAnsweredAsk(node, lookup)
          : undefined;
    return decided === undefined ? [] : [decided];
  });
}

/** What `decisionsOfAnswer` has read: each answer's decisions, by the key of the issue they were
 *  read for. An answer the page no longer holds is let go with what it kept. */
const decisionsByAnswer = new WeakMap<TreeAnswer, Map<string, ReadonlyMap<string, DecisionNode>>>();

/**
 * The decisions of `answer` of issue `key` (`decisionNodes`), each by its address, the first
 * where two share one, in the order `decisionNodes` gives: read once per answer and issue, so
 * every Keep and Change control on the page and the header's count read one answer in one pass.
 * It reads no asks, which only make an answered question a person's decision: Keep and Change
 * are drawn only on a decision waiting for review, which is an agent's, and a person's decision
 * never waits for review, so neither reader finds another decision with them.
 */
export function decisionsOfAnswer(
  answer: TreeAnswer,
  key: string
): ReadonlyMap<string, DecisionNode> {
  let byIssue = decisionsByAnswer.get(answer);
  if (byIssue === undefined) {
    byIssue = new Map();
    decisionsByAnswer.set(answer, byIssue);
  }
  const held = byIssue.get(key);
  if (held !== undefined) return held;
  const decisions = new Map<string, DecisionNode>();
  for (const decision of decisionNodes(answer, { issue: { key } })) {
    if (!decisions.has(decision.ref)) decisions.set(decision.ref, decision);
  }
  byIssue.set(key, decisions);
  return decisions;
}

/**
 * The decision's first paragraph, as a reading of that block alone: the block a Change quotes and
 * is anchored to (`decisionQuote`), and the one whose plain-text projection the Change draws and
 * names Keep and Change after (`DecisionActions.tsx`, decision 448). It is read from `own`, the
 * reading of the decision's own content (`decision-review.ts`' `decisionContent`): the tree read
 * answers a decision node's text as its whole stored block, the lines that open and close its
 * `decision` directive included, and any reading it sends of that text as one `decision` node,
 * so its own words are that text without those two lines (`ownWords`) and its own reading that
 * node's content at the top level (`ownTree`). Of that reading, whose top-level blocks are its
 * paragraphs, the first that is no heading is taken: from the server's reading of the decision's
 * words (`text_tree`), and where the read carried none that holds blocks - as the tree read
 * answers a decision node today, and a pod of the build before during a rolling deploy - from the
 * reading of its own stored words (`storedReading`), which `holder`, the decision's node, holds,
 * so it is remembered while the node lives. Never its heading, its directive's lines or another
 * paragraph. Undefined for a decision that holds no paragraph.
 */
function paragraphOf(own: DecisionContent, holder: object): DisplayTree | undefined {
  const reading =
    own.tree !== undefined && Array.isArray(own.tree.content)
      ? own.tree
      : storedReading(own.text, holder);
  const blocks = reading.content;
  const block = Array.isArray(blocks)
    ? (blocks as DisplayTree[]).find((node) => node.type !== "heading")
    : undefined;
  return block === undefined ? undefined : { ...reading, content: [block] };
}

/** The first paragraph of decision node `decision` (`paragraphOf`), read from its own content,
 *  which the node holds as `text` and `text_tree` (`blockAndWords`), by its tree node: the block a
 *  Change quotes, whose words are the node's `heading`. */
export function anchorBlock(
  decision: Pick<DecisionNode, "node" | "text" | "text_tree">
): DisplayTree | undefined {
  const tree = decision.text_tree;
  return paragraphOf(
    tree === undefined ? { text: decision.text } : { text: decision.text, tree },
    decision.node
  );
}

/**
 * The words of the decision's first paragraph (`paragraphOf`), which a Change is anchored to: the
 * server matches an anchor's quote against the document's own text (`pmdoc.FindQuote`), never its
 * markdown, so they are the words of that block's reading, with a list's items one space apart
 * and a hard break as one space (`wordsOf`). So a decision's quote is the words of its first
 * paragraph alone, never its markdown. Undefined for a decision that holds no paragraph.
 */
function decisionQuote(own: DecisionContent, holder: object): string | undefined {
  const paragraph = paragraphOf(own, holder);
  return paragraph === undefined ? undefined : wordsOf(paragraph);
}

/** The members of the model's decision every reader of a decision reads as the read answers
 *  them: who it waits on, how it stands, when its words last changed, what it settles and what
 *  it shaped, and the sum of its words that a Keep sends. */
type ModelMembers = Pick<
  Decision,
  | "asks"
  | "changed_at"
  | "changed_version"
  | "kept"
  | "kind"
  | "links"
  | "replaced"
  | "text_sha256"
  | "waiting_review"
>;

/** The model's members of `decision`, the node's own `decision` as the read answers it. */
function modelMembers(decision: Decision): ModelMembers {
  return {
    asks: decision.asks,
    changed_at: decision.changed_at,
    changed_version: decision.changed_version,
    kept: decision.kept,
    kind: decision.kind,
    links: decision.links,
    replaced: decision.replaced,
    text_sha256: decision.text_sha256,
    waiting_review: decision.waiting_review,
  };
}

/**
 * A decision node, with the members of the model's decision the read answers on it (its
 * `decision`) as they are. A decision node that carries none is no decision: no reader could say
 * how it stands or what it shaped, and a Keep would carry no sum.
 */
function fromDecision(node: TreeAnswerNode, lookup: Lookup): DecisionNode | undefined {
  const decision = node.decision;
  const ref = referenceOf(node, lookup);
  if (decision === undefined || node.document === undefined || ref === undefined) return undefined;
  return {
    ...blockAndWords(node),
    ...modelMembers(decision),
    ...deciderMember(node, lookup),
    document: node.document.artifact_id,
    ref,
  };
}

/**
 * A person's decision: an answered ask that shaped something. Where the read answers the person's
 * decision on the ask's node (`decision`), which it does for an ask a person answered alone, the
 * ask is answered, it shaped what the decision's links name, and its members are the read's: the
 * links are its `shaped` ties as the decisions read answers them, so a person's decision is read
 * alike from an answer that holds no `shaped` tuple from it. The Decisions view is such an answer:
 * it holds no statement, and the read sends a tuple only where the answer holds its far end
 * (`issue_tree_read.go`'s `answer`). Where the read answers no person's decision on the ask, it is
 * answered where the asks read says so, it shaped something where the answer holds a `shaped`
 * tuple from it, and the decision reads as today's decisions read reads a person's decision, a
 * section of `decisions.md` that names the ask: of the kind `person`, naming the ask itself,
 * waiting for nobody's review, never kept nor replaced, made when it was answered and at no
 * version of a document, with no sum, since only a decision that waits is kept; and its links are
 * those tuples (`shapedLinks`). The read's word that the ask is answered stands over the asks
 * read's, which may be older or newer than it. Undefined for any other ask.
 */
function fromAnsweredAsk(node: TreeAnswerNode, lookup: Lookup): DecisionNode | undefined {
  const ask = lookup.askOf(node);
  const ref = nodeReference(node, lookup.key, ask);
  if (ref === undefined) return undefined;
  const shaped =
    node.decision === undefined
      ? ask?.state === "answered" &&
        lookup.tuplesFrom(node.id).some((tuple) => tuple.kind === "shaped")
      : (node.decision.links ?? []).length > 0;
  if (!shaped) return undefined;
  const members: ModelMembers =
    node.decision === undefined
      ? {
          asks: ask === undefined ? [] : [ask.id],
          changed_at: ask?.answer?.at ?? null,
          changed_version: 0,
          kept: null,
          kind: "person",
          links: shapedLinks(node, lookup),
          replaced: false,
          text_sha256: "",
          waiting_review: false,
        }
      : modelMembers(node.decision);
  return {
    ...blockAndWords(node),
    ...deciderMember(node, lookup),
    ...members,
    document: node.document?.artifact_id,
    ref,
  };
}

/** Who decided `node` (`DecisionNode.decidedBy`): the far end of its `decided_by` tuple, read by
 *  the outline's one rule (`deciderOf`); none where the read answers no such tuple, or one that
 *  names neither a person nor a session. */
function deciderMember(node: TreeAnswerNode, lookup: Lookup): Pick<DecisionNode, "decidedBy"> {
  const decidedBy = lookup.tuplesFrom(node.id).find((tuple) => tuple.kind === "decided_by");
  const decider = deciderOf(decidedBy?.to ?? null);
  return decider === undefined ? {} : { decidedBy: decider };
}

/** The members every decision reads from its node alike: its block, its words and the heading
 *  read from them, and whether its agent was unsure. */
function blockAndWords(
  node: TreeAnswerNode
): Pick<DecisionNode, "block_id" | "heading" | "node" | "text" | "text_tree" | "unsure"> {
  const own = decisionContent(node);
  return {
    block_id: blockIdOfNode(node),
    // A converted decision's old `##` heading is its first paragraph, so the
    // heading every accessible name reads ("Keep <heading>") is that paragraph's words.
    heading: decisionQuote(own, node) ?? "",
    node,
    // The read answers a decision node's text as its whole stored block, the lines that open and
    // close its `decision` directive included, and its reading as that block's: one `decision`
    // node that carries the block's attributes. Every reader of a decision draws its own content,
    // as the outline draws the node's (`tree-read/tree-nodes.ts`' `readingOf`), so its words and
    // its reading are that content (`decisionContent`), and no attribute of the block is drawn.
    text: own.text,
    ...(own.tree === undefined ? {} : { text_tree: own.tree }),
    // The conversion writes an `Unsure:` paragraph as the attribute `uncertainty="high"`.
    unsure: node.decision?.unsure === true || node.attributes?.uncertainty === "high",
  };
}

/**
 * An answered ask's `shaped` ties as the decisions read answers a decision's links: each naming
 * the node the tie reaches by the address the outline's chip names it by (`tieReference` with
 * `tieTargetReference`, a block of the spec as `spec#b-<id>`), and
 * not stale, since the answer says no tie is. A tie reaches a block - `shaped` is a `block[]` -
 * and the members of the link no reader reads are the ones the tuple holds no value for: the
 * ask's own time and no version.
 */
function shapedLinks(node: TreeAnswerNode, lookup: Lookup): DecisionLink[] {
  return lookup.tuplesFrom(node.id).flatMap((tuple) => {
    if (tuple.kind !== "shaped") return [];
    const ref = tieReference(tuple.to.id, lookup.nodeOf, (target) =>
      tieTargetReference(target, lookup.key, lookup.askOf(target), lookup.specSlug)
    );
    if (ref === undefined) return [];
    return [
      {
        changed_since_tie: false,
        created_at: node.made_at ?? "",
        created_version: 0,
        from_block: blockIdOfNode(node),
        stale: false,
        to: {
          changed_at: null,
          changed_by: null,
          id: tuple.to.id,
          kind: "block",
          made_at: null,
          made_by: null,
          parent: null,
          parent_version: null,
          ref,
        },
      },
    ];
  });
}

/**
 * The address a tie names the node `readId` names by, by the one rule a decision's links and the
 * outline's chips (`tree-read/tree-nodes.ts`' `chipTo`) both read: a node the answer does not
 * hold (`nodeOf`) by the tie's own `readId`, where that is an address; one it holds by that
 * node's address (`referenceOf`, which each reader passes as it addresses a node), else an
 * issue's by its own address. Undefined for a target with no address.
 */
export function tieReference(
  readId: string,
  nodeOf: (readId: string) => TreeAnswerNode | undefined,
  referenceOf: (target: TreeAnswerNode) => string | undefined
): string | undefined {
  const target = nodeOf(readId);
  if (target === undefined) {
    return parseDispatchReference(readId) === undefined ? undefined : readId;
  }
  return (
    referenceOf(target) ??
    (target.supertag === "issue"
      ? buildDispatchReference({ key: target.id, kind: "issue" })
      : undefined)
  );
}

/**
 * The node's own address (`buildDispatchReference`), by the one rule the outline
 * (`tree-read/tree-nodes.ts`'s `referenceOf`) and every reader of a decision address a node by: a
 * comment's and a message's own, an ask's by the ask it is (`askIdOf`, from `ask`, the row of the
 * issue's asks it is), and a typed block's, a decision's among them, in the document its
 * `document` member names, by its block id. A node of another issue is addressed in that issue;
 * `key` is this issue's. Anything else has none.
 */
export function nodeReference(
  node: TreeAnswerNode,
  key: string,
  ask: Pick<TreeAsk, "id"> | undefined
): string | undefined {
  const issue = node.issue ?? key;
  if (node.supertag === "comment" || node.supertag === "message") {
    return buildDispatchReference({ id: node.id, key: issue, kind: node.supertag });
  }
  const askId = node.supertag === "ask" ? askIdOf(node, ask) : undefined;
  if (askId !== undefined) return buildDispatchReference({ id: askId, key: issue, kind: "ask" });
  return node.document === undefined
    ? undefined
    : buildDispatchReference({
        block: { id: blockIdOfNode(node) },
        key: issue,
        kind: "artifact",
        slug: node.document.slug,
      });
}

/**
 * The address a tie names the node it reaches by, which a decision's links and the outline's
 * chips (`tree-read/tree-nodes.ts`' `chipTo`) both read, so the two name one node by one
 * address: a typed block of this issue's spec that is no ask as the spec's own
 * (`spec#b-<id>`), as the server's reads and the outline's chips name one, and every other node
 * by its own address (`nodeReference`). `specSlug` is the spec's slug, `""` where the caller
 * holds none, which names every block by its document's slug.
 */
export function tieTargetReference(
  node: TreeAnswerNode,
  key: string,
  ask: Pick<TreeAsk, "id"> | undefined,
  specSlug: string
): string | undefined {
  const asked = node.supertag === "ask" && askIdOf(node, ask) !== undefined;
  if (!asked && isSpecBlock(node, key, specSlug)) {
    return buildDispatchReference({ block: { id: blockIdOfNode(node) }, key, kind: "spec" });
  }
  return nodeReference(node, key, ask);
}

/** Whether `node` is a block of issue `key`'s spec, the document whose slug is `specSlug` (`""`
 *  where the caller holds none, which makes no block one), as the read places it (`document`):
 *  the one rule a tie's address (`tieTargetReference`) and the outline's statements
 *  (`tree-read/tree-nodes.ts`' `isStatementNode`) read. */
export function isSpecBlock(node: TreeAnswerNode, key: string, specSlug: string): boolean {
  return (
    specSlug !== "" &&
    (node.issue === undefined || node.issue === key) &&
    node.document?.slug === specSlug
  );
}

/** The id of the ask an ask node is: that of `ask`, the row of the issue's asks it is
 *  (`askOfNode`), else the node's own where it is no block of a document, whose id would be the
 *  block's. */
export function askIdOf(
  node: TreeAnswerNode,
  ask: Pick<TreeAsk, "id"> | undefined
): string | undefined {
  return ask?.id ?? (node.document === undefined ? node.id : undefined);
}

/** The issue's asks by id: the issue read's open asks and the asks read, the asks read's where
 *  both hold one, since it is the newer read of the two. */
export function issueAsks(reads: DecisionReads): Map<string, TreeAsk> {
  const asks = new Map<string, TreeAsk>();
  for (const ask of [...(reads.issue.open_asks ?? []), ...(reads.asks ?? [])]) {
    asks.set(ask.id, ask);
  }
  return asks;
}

/** The node id an ask is drawn by: the block it was indexed from, in the document that holds that
 *  block (`block_artifact`), else the document it is anchored in, where it names a block; its own
 *  id otherwise. */
export function askNodeId(
  ask: TreeAsk,
  slugOf: (artifactId: string) => string | undefined
): string {
  const slug = ask.block_artifact?.slug ?? slugOf(ask.artifact_id ?? "");
  const block = ask.block_id ?? undefined;
  return slug !== undefined && block !== undefined ? blockNodeId(slug, block) : ask.id;
}

/**
 * The row of the issue's asks (`issueAsks`) each ask node is, by the node id the outline draws
 * either by (`askNodeId`, and `tree-read/tree-nodes.ts`'s `treeIdOfNode`): an ask block of this
 * issue by its block id and the slug of its document, which a row names by its
 * `block_artifact`, else by the issue's document its `artifact_id` names; an ask of the
 * conversation by its own row's; and a node of another issue by its block id. A later row of one
 * place stands over an earlier one. The outline and every reader of a decision find an ask's row
 * by this one rule.
 */
export function askOfNode(reads: DecisionReads): (node: TreeAnswerNode) => TreeAsk | undefined {
  const slugs = new Map((reads.issue.artifacts ?? []).map((one) => [one.id, one.slug]));
  const slugOf = (artifactId: string) => slugs.get(artifactId);
  const asks = issueAsks(reads);
  const placed = new Map([...asks.values()].map((ask) => [askNodeId(ask, slugOf), ask]));
  return (node) => {
    const blockId = blockIdOfNode(node);
    if (node.issue !== undefined && node.issue !== reads.issue.key) return placed.get(blockId);
    if (node.document !== undefined) return placed.get(blockNodeId(node.document.slug, blockId));
    const own = asks.get(node.id);
    return placed.get(own === undefined ? blockId : askNodeId(own, slugOf));
  };
}

/** The node's own address (`nodeReference`), with the row of the issue's asks an ask is: the
 *  ask is looked up for an ask node alone, the one kind `nodeReference` reads it for. */
function referenceOf(node: TreeAnswerNode, lookup: Lookup): string | undefined {
  return nodeReference(node, lookup.key, node.supertag === "ask" ? lookup.askOf(node) : undefined);
}

/** The answer's nodes and tuples, each looked up by the read's own ids (`decision-review.ts`'
 *  `firstOfEachId`, `tuplesByFrom`), and the issue's asks by the node each is (`askOfNode`). */
function lookupOf(answer: TreeAnswer, reads: DecisionReads): Lookup {
  const nodes = firstOfEachId(answer.nodes ?? []);
  const tuples = tuplesByFrom(answer);
  return {
    askOf: askOfNode(reads),
    key: reads.issue.key,
    nodeOf: (readId) => nodes.get(readId),
    specSlug:
      primarySpec({
        artifacts: reads.issue.artifacts ?? null,
        primary_artifact_id: reads.issue.primary_artifact_id ?? "",
      })?.slug ?? "",
    tuplesFrom: (readId) => tuples.get(readId) ?? [],
  };
}

/** The answer's tuples by the read id of the node each starts at (`from`), each node's in the
 *  answer's order. */
export function tuplesByFrom(answer: TreeAnswer): Map<string, TreeTuple[]> {
  const tuples = new Map<string, TreeTuple[]>();
  for (const tuple of answer.tuples ?? []) {
    const from = tuples.get(tuple.from.id);
    if (from === undefined) tuples.set(tuple.from.id, [tuple]);
    else from.push(tuple);
  }
  return tuples;
}
