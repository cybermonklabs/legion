import type { Comment, IssueDetails } from "../../api/types";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { askOfNode, decisionsOfAnswer } from "./decision-nodes/decision-nodes";
import { decisionNodeChanges, deletedPostIds, inReviewList } from "./decision-review";
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
 * (`decision-nodes.ts`' `decisionsOfAnswer`) with no asks: the read answers a person's decision
 * on every ask a person answered, which says, with no asks, which question is a person's
 * decision, which never waits for review, and which agent's decision restates that answer and is
 * no decision at all (`restatesAnswer`), so the count is the same without them. One the reader
 * asked to change waits on its agent and is not counted, by the outline's one rule
 * (`decisionNodeChanges`): its Change is a comment the tree read hangs under its node, whether or
 * not the comments read holds that comment yet, so the count matches no quote and reads no block
 * of a document. A Change its author deleted asks nothing any more, as in the outline:
 * `comments`, the page's comments read, says which were (`deletedPostIds`), since the tree read
 * answers a deleted comment with no mark of the delete.
 *
 * An overtaken decision or question, a node the tree read answers `overtaken`, counts in no
 * count of what waits (R2), as the outline marks it waiting on nobody (`draftDecision`,
 * `draftAsk`): neither a decision whose node is, nor an open ask whose node is, each ask found by
 * the one rule the outline finds an ask node's row by (`askOfNode`).
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
  comments?: readonly Pick<Comment, "deleted_at" | "id">[]
): WaitingOnReader {
  const overtaken = overtakenAsks(issue, viewer === undefined ? undefined : tree);
  const open =
    viewer === undefined
      ? []
      : waitingOnHuman((issue.open_asks ?? []).filter((ask) => ask.state === "open")).filter(
          (ask) => ask.waiting_on_person === viewer && !overtaken.has(ask.id)
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
    reviewing = [...decisionsOfAnswer(tree, issue.key).values()].filter(
      (decision) =>
        decision.node.state !== "overtaken" &&
        inReviewList(decision, isCoverageStatement, changes.has(decision.node.id))
    ).length;
  }
  return { approvals, asks, decisions: reviewing, total: asks + reviewing + approvals.length };
}

/** The ids of the issue's open asks whose tree-read node is overtaken (R2), each ask found by the
 *  one rule the outline finds an ask node's row by (`askOfNode`); none without a tree. */
function overtakenAsks(
  issue: Pick<IssueDetails, "artifacts" | "key" | "open_asks" | "primary_artifact_id">,
  tree: TreeAnswer | undefined
): ReadonlySet<string> {
  if (tree === undefined) return new Set();
  const askOf = askOfNode({ issue });
  return new Set(
    (tree.nodes ?? []).flatMap((node) => {
      const ask = node.supertag === "ask" && node.state === "overtaken" ? askOf(node) : undefined;
      return ask === undefined ? [] : [ask.id];
    })
  );
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
