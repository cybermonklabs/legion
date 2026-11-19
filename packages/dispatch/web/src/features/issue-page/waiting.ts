import type { IssueDecisions, IssueDetails } from "../../api/types";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { type ChangeComment, decisionChangeComments, inReviewList } from "./decision-review";
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
 * which addresses name one: a caller passes `coverageStatements(issue, coverage)`
 * (`tree-drafts.ts`), the rule the tree's review list reads, so the count counts the decisions
 * the tree marks as waiting and no other.
 *
 * A decision the reader asked to change waits on its agent and is not counted, by the outline's
 * one rule (`decisionChangeComments`): a decision node's Change is a comment the tree read
 * (`tree`) hangs under it, whether or not the comments read holds that comment yet, and the
 * Change of a decision the read holds no node for is a comment of the issue's (`comments`) whose
 * quote its words hold, so the count reads no block of the decisions document.
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
  issue: Pick<IssueDetails, "artifacts" | "open_asks" | "primary_artifact_id">,
  viewer: string | undefined,
  decisions: IssueDecisions | undefined,
  isCoverageStatement: (reference: string) => boolean,
  comments: readonly ChangeComment[] | undefined,
  tree: TreeAnswer | undefined
): WaitingOnReader {
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
  // The decisions a Change waits on, each by its address: a decision node's from the tree read
  // alone, as the outline finds it, so the two agree while the comments read is out or older.
  const changed = new Set(
    decisionChangeComments(decisions, comments, issue.artifacts, undefined, tree).values()
  );
  const reviewing =
    viewer === undefined
      ? 0
      : (decisions?.decisions ?? []).filter((decision) =>
          inReviewList(decision, isCoverageStatement, changed.has(decision.ref))
        ).length;
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
