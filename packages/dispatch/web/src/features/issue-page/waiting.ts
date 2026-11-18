import type { IssueDetails } from "../../api/types";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { decisionsOfAnswer } from "./decision-nodes/decision-nodes";
import { decisionNodeChanges, inReviewList } from "./decision-review";
import { isOvertaken } from "./issue-address";
import { statesOfRead } from "./tree-read/tree-nodes";
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
 * one still is: no node took its place yet.
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
  isStatement: (reference: string) => boolean,
  tree: TreeAnswer | undefined
): WaitingOnReader {
  // Each open ask and node of the tree at or under an overtaken node taken out, the decisions
  // among those nodes with them, and the count made of what is left: that tree holds no
  // overtaken node, so the count is made once more and no further.
  const closed = closedKeys(tree);
  if (tree !== undefined && closed.size > 0) {
    return waitingOnReader(
      { ...issue, open_asks: (issue.open_asks ?? []).filter((ask) => !askIn(closed, ask)) },
      viewer,
      isStatement,
      { ...tree, nodes: tree.nodes.filter((node) => !closed.has(nodeKey(node))) }
    );
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
  // The decision nodes a Change waits on, by the node's id: from the tree read alone, as the
  // outline finds them, so the two agree while the comments read is out or older.
  const changes = tree === undefined ? undefined : decisionNodeChanges(tree);
  const decisions = tree === undefined ? [] : [...decisionsOfAnswer(tree, issue.key).values()];
  const reviewing =
    viewer === undefined
      ? 0
      : decisions.filter((decision) =>
          inReviewList(decision, isStatement, changes?.has(decision.node.id) === true)
        ).length;
  return { approvals, asks, decisions: reviewing, total: asks + reviewing + approvals.length };
}

/** One key for block `blockId` of the document `documentId`. */
function blockKey(documentId: string, blockId: string): string {
  return `${documentId}#${blockId}`;
}

/** A node of the tree read by where it stands: a typed block by its document and its id, since
 *  the read names a block by its id within its document, and any other node by its own id. */
function nodeKey(node: TreeAnswer["nodes"][number]): string {
  return node.document === undefined ? node.id : blockKey(node.document.artifact_id, node.id);
}

/**
 * The key (`nodeKey`) of each node of `tree` at or under an overtaken one: an overtaken node, and
 * each node whose `parent` is one of these. The read answers a parent before its children
 * (`TreeAnswer.nodes`), so one pass in its order finds them all. None while the tree read has not
 * answered.
 */
function closedKeys(tree: TreeAnswer | undefined): ReadonlySet<string> {
  const closed = new Set<string>();
  // The read's own id of each, which is what a child's `parent` names.
  const parents = new Set<string>();
  for (const node of tree?.nodes ?? []) {
    const under = node.parent !== null && parents.has(node.parent);
    if (!under && !isOvertaken(statesOfRead(node))) continue;
    parents.add(node.id);
    closed.add(nodeKey(node));
  }
  return closed;
}

/** Whether the open `ask` is a node `closed` holds: an ask node of the conversation by the ask's
 *  own id, or the block it was indexed from, in the document that holds that block, as the
 *  tree read answers an ask either way (`decision-nodes.ts`' `askOf`). */
function askIn(
  closed: ReadonlySet<string>,
  ask: NonNullable<IssueDetails["open_asks"]>[number]
): boolean {
  if (closed.has(ask.id)) return true;
  const document = ask.block_artifact?.id ?? ask.artifact_id;
  return document !== null && ask.block_id !== null && closed.has(blockKey(document, ask.block_id));
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
