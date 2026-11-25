import type { IssueDetails } from "../../api/types";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { askOfNode, type DecisionReads, decisionsOfAnswer } from "./decision-nodes/decision-nodes";
import {
  type ChangeComment,
  decisionNodeChanges,
  deletedPostIds,
  inReviewList,
} from "./decision-review";
import { askIdsOf, closedReadIds } from "./node-states/node-states";
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
 * count matches no quote and reads no block of a document. A Change its author deleted asks
 * nothing any more, as in the outline: `comments`, the page's comments read, says which were
 * (`deletedPostIds`), since the tree read answers a deleted comment with no mark of the delete.
 *
 * Nothing at or under an overtaken node of the tree read waits on anybody: a later node took its
 * place, and the tree draws none of them in the waiting tone. A node is under another where the
 * read's `parent` chain reaches it, so an open ask or a decision there is not counted. A proposed
 * one still is: no node took its place yet. A question written as a block of a document is
 * overtaken where its ask's node is, and never by a state its block's node holds (`ownStatesOf`),
 * as the tree draws it: the tree draws the block and its ask as one row, which takes the ask's
 * state, so where the ask's node is overtaken the block is closed too, and every node under it.
 * Which block a question is written as is read as the tree reads it, through the page's one
 * pairing rule (`decision-nodes.ts`' `askOfNode`) over the issue's open asks and the asks read
 * (`asksRead`, the page's), so a question closes its block with its ask's node whether it is
 * open or answered (`node-states.ts`' `closedReadIds`), and an open ask is not counted where its
 * ask's node or its block is closed.
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
  tree: TreeAnswer | undefined,
  comments?: readonly Pick<ChangeComment, "deleted_at" | "id">[],
  asksRead?: DecisionReads["asks"]
): WaitingOnReader {
  // Each open ask and node of the tree at or under an overtaken node taken out, the decisions
  // among those nodes with them, and the count made of what is left.
  const reads: DecisionReads = { asks: asksRead, issue };
  const closed = closedReadIds(tree, reads);
  if (tree !== undefined && closed.size > 0) {
    // The ask of each closed ask node, its conversation node or its block.
    const closedAsks = askIdsOf(
      (tree.nodes ?? []).filter((node) => closed.has(node.id)),
      askOfNode(reads)
    );
    issue = {
      ...issue,
      open_asks: (issue.open_asks ?? []).filter((ask) => !closedAsks.has(ask.id)),
    };
    tree = { ...tree, nodes: (tree.nodes ?? []).filter((node) => !closed.has(node.id)) };
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
  // A Change its author deleted asks nothing any more, as in the outline.
  let reviewing = 0;
  if (viewer !== undefined && tree !== undefined) {
    const changes = decisionNodeChanges(tree, undefined, deletedPostIds(comments));
    reviewing = [...decisionsOfAnswer(tree, issue.key).values()].filter((decision) =>
      inReviewList(decision, isCoverageStatement, changes.has(decision.node.id))
    ).length;
  }
  return { approvals, asks, decisions: reviewing, total: asks + reviewing + approvals.length };
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
