import type { IssueDetails } from "../../api/types";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { decisionsOfAnswer } from "./decision-nodes/decision-nodes";
import { decisionNodeChanges, inReviewList } from "./decision-review";
import { isOvertaken } from "./issue-address";
import { ownStatesOf } from "./tree-read/tree-nodes";
import type { TreeAnswer } from "./tree-read/tree-read";

/**
 * What waits on the reader, as the header's count counts it: each open ask of the issue whose
 * turn is the reader's, each decision in the Decisions filter's review list (R35,
 * `decision-review.ts`), and each open approval request whose turn is the reader's - three
 * things and no other.
 *
 * An ask's turn is the reader's by the one rule every count of the reader's work reads
 * (`waitingOnYou`, `features/inbox/BlockedOnYou.tsx`): it waits on a person (`waitingOnHuman`),
 * and the person the server resolved it to (`waiting_on_person`: whoever it was handed to, else
 * the issue's assignee, else the project's default assignee) is the reader. So a question the
 * reader asked of somebody else does not raise the count, and handing the issue to another
 * assignee moves its questions to that person's count. Nothing waits on a reader whose login has
 * not resolved.
 *
 * A decision is on the review list by its tie to a statement, and `isCoverageStatement` answers
 * which addresses name one: the header's count (`IssuePropertiesRow.tsx`) passes
 * `coverageStatements(issue, coverage)` (`tree-drafts.ts`), the rule the outline's
 * `draftDecision` (`tree-read/tree-nodes.ts`) marks a decision waiting by, so the count counts
 * the decisions the outline marks as waiting and no other.
 *
 * A decision is a decision node of the tree read (`tree`), read once per answer
 * (`decision-nodes.ts`' `decisionsOfAnswer`) with no asks: they only make an answered question a
 * person's decision, which never waits for review, and an agent's decision reads none, so the
 * count is the same without them. One the reader asked to change waits on its agent and is not
 * counted, by the outline's one rule (`decisionNodeChanges`): its Change is a comment the tree
 * read hangs under its node, whether or not the comments read holds that comment yet, so the
 * count matches no quote and reads no block of a document.
 *
 * Nothing at or under an overtaken node of the tree read waits on anybody: a later node took its
 * place, and the tree draws none of them in the waiting tone. A node is under another where the
 * read's `parent` chain reaches it, so an open ask or a decision there is not counted. A proposed
 * one still is: no node took its place yet. A question written as a block of a document is
 * overtaken where its ask's node is, and never by a state its block's node holds (`ownStatesOf`),
 * as the tree draws it: the tree draws the block and its ask as one row, which takes the ask's
 * state, so where the ask's node is overtaken the block is closed too, and every node under it.
 * Which block a question is written as is read from the issue's open asks (`open_asks`), so a
 * question that is no longer open closes its block only through its own node's parent chain.
 */
export interface WaitingOnReader {
  /** The open asks other than an approval request whose turn is the reader's. */
  readonly asks: number;
  /** The decisions of the review list that wait for the reader's review. */
  readonly decisions: number;
  /** The documents an open approval request whose turn is the reader's covers, by artifact id:
   *  the header's approval control for each is where the count puts the focus when nothing
   *  else waits. */
  readonly approvals: readonly string[];
  /** All three, which is the number the count draws. */
  readonly total: number;
}

export function waitingOnReader(
  issue: Pick<IssueDetails, "artifacts" | "key" | "open_asks" | "primary_artifact_id">,
  viewer: string | undefined,
  isCoverageStatement: (reference: string) => boolean,
  tree: TreeAnswer | undefined
): WaitingOnReader {
  // Each open ask and node of the tree at or under an overtaken node taken out, the decisions
  // among those nodes with them, and the count made of what is left.
  const closed = closedKeys(tree, issue.open_asks ?? []);
  if (tree !== undefined && closed.size > 0) {
    issue = { ...issue, open_asks: (issue.open_asks ?? []).filter((ask) => !askIn(closed, ask)) };
    tree = { ...tree, nodes: (tree.nodes ?? []).filter((node) => !closed.has(nodeKey(node))) };
  }
  const open =
    viewer === undefined
      ? []
      : waitingOnHuman((issue.open_asks ?? []).filter((ask) => ask.state === "open")).filter(
          (ask) => ask.waiting_on_person === viewer
        );
  const approvals = open
    .filter((ask) => ask.kind === "approval")
    .map(
      (ask) =>
        ask.artifact_id ??
        issue.artifacts?.find((artifact) => artifact.slug === ask.approval_artifact?.slug)?.id ??
        issue.primary_artifact_id
    );
  const asks = open.length - approvals.length;
  // Nothing waits on a reader whose login has not resolved, so the decisions are read only for
  // one who has. The decision nodes a Change waits on, by the node's id, are from the tree read
  // alone, as the outline finds them, so the two agree while the comments read is out or older.
  let reviewing = 0;
  if (viewer !== undefined && tree !== undefined) {
    const changes = decisionNodeChanges(tree);
    reviewing = [...decisionsOfAnswer(tree, issue.key).values()].filter((decision) =>
      inReviewList(decision, isCoverageStatement, changes.has(decision.node.id))
    ).length;
  }
  return { approvals, asks, decisions: reviewing, total: asks + reviewing + approvals.length };
}

/** One node of the tree read. */
type AnswerNode = NonNullable<TreeAnswer["nodes"]>[number];

/** One open ask of the issue, as the issue read carries it. */
type OpenAsk = NonNullable<IssueDetails["open_asks"]>[number];

/** One key for block `blockId` of the document `documentId`. */
function blockKey(documentId: string, blockId: string): string {
  return `${documentId}#${blockId}`;
}

/** A node of the tree read by where it stands: a typed block by its document and its block id
 *  (`block_id`), since the read names a block by its id within its document, and any other node
 *  by its own id. */
function nodeKey(node: AnswerNode): string {
  return node.document === undefined
    ? node.id
    : blockKey(node.document.artifact_id, node.block_id ?? node.id);
}

/**
 * The key (`nodeKey`) of each node of `tree` at or under an overtaken one: an overtaken node, the
 * block an open question of `asks` is written as where that question's ask's node is overtaken,
 * and each node whose `parent` is one of these. A question's block takes no state of its own here
 * (`ownStatesOf`): the tree draws the block and its ask as one row, which takes the ask's state
 * (`node-states.ts`' `stateIdOf`), so the block is closed with its ask's node. The read need not
 * answer an ask's node before its block, so the overtaken nodes are all found before any node is
 * closed, and each closed node's children are then closed under it, wherever the read answers
 * them. None while the tree read has not answered.
 */
function closedKeys(tree: TreeAnswer | undefined, asks: readonly OpenAsk[]): ReadonlySet<string> {
  const nodes = tree?.nodes ?? [];
  const overtaken = nodes.filter((node) => isOvertaken(ownStatesOf(node)));
  // The read's own id of each, which an ask's node of the conversation shares with its ask.
  const overtakenIds = new Set(overtaken.map((node) => node.id));
  // The key of each block an open question is written as whose ask's node is overtaken.
  const blocks = new Set(
    asks.flatMap((ask) => {
      const block = askBlockKey(ask);
      return block !== undefined && overtakenIds.has(ask.id) ? [block] : [];
    })
  );
  // Each node's children, by the read's own id of the parent, which is what `parent` names.
  const children = new Map<string, AnswerNode[]>();
  for (const node of nodes) {
    if (node.parent === null) continue;
    const named = children.get(node.parent.id);
    if (named === undefined) children.set(node.parent.id, [node]);
    else named.push(node);
  }
  const closing = [
    ...overtaken,
    ...nodes.filter(
      (node) => node.supertag === "ask" && node.document !== undefined && blocks.has(nodeKey(node))
    ),
  ];
  const closed = new Set<string>();
  // The read's own id of each node closed so far, whose children are closed with it.
  const parents = new Set<string>();
  for (let node = closing.pop(); node !== undefined; node = closing.pop()) {
    closed.add(nodeKey(node));
    if (parents.has(node.id)) continue;
    parents.add(node.id);
    closing.push(...(children.get(node.id) ?? []));
  }
  return closed;
}

/** Whether the open `ask` is a node `closed` holds: an ask node of the conversation by the ask's
 *  own id, or the block it was indexed from (`askBlockKey`), as the tree read answers an ask
 *  either way (`decision-nodes.ts`' `askOfNode`). */
function askIn(closed: ReadonlySet<string>, ask: OpenAsk): boolean {
  const block = askBlockKey(ask);
  return closed.has(ask.id) || (block !== undefined && closed.has(block));
}

/** The key (`nodeKey`) of the block the open `ask` was indexed from, in the document that holds
 *  that block; undefined for an ask that names no block. */
function askBlockKey(ask: OpenAsk): string | undefined {
  const document = ask.block_artifact?.id ?? ask.artifact_id;
  return document === null || ask.block_id === null ? undefined : blockKey(document, ask.block_id);
}

/** What the count leaves in the address's history entry, which the Decisions filter reads to put
 *  the focus on what waits once it has drawn it: one press, told apart from the next by
 *  `waitingPress`, so each press goes on to the next item that waits. */
export interface WaitingPress {
  readonly waitingPress: number;
}

/** The press `state` carries, where the history entry is the count's own. */
export function waitingPressOf(state: unknown): number | undefined {
  if (typeof state !== "object" || state === null || !("waitingPress" in state)) return undefined;
  const press = state.waitingPress;
  return typeof press === "number" ? press : undefined;
}
