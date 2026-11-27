import type { IssueDecisions, IssueDetails } from "../../api/types";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { type ChangeComment, decisionChanges, inReviewList } from "./decision-review";
import type { DocumentBlocks } from "./issue-tree";

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
 * A decision the reader asked to change waits on its agent and is not counted
 * (`decisionChanges`): `comments` are the issue's comments, and `documents` the blocks of its
 * documents the page has read (`undefined` where it has read none), which name the decision whose
 * block holds the block a Change was written in, as the tree reads them. A person's decision - the
 * answered ask, or a `decision` block whose `decided_by` names a person - waits for no review
 * (the server's `waiting_review` is false for it) and is never counted here.
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
  decisions: IssueDecisions | undefined,
  isCoverageStatement: (reference: string) => boolean,
  comments: readonly ChangeComment[] | undefined,
  documents: readonly DocumentBlocks[] | undefined
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
  const changes = decisionChanges(decisions, comments, issue, documents);
  // A person's decision never waits for review (`waiting_review`), so only an agent's counts.
  const reviewing =
    viewer === undefined
      ? 0
      : (decisions?.entries ?? []).filter((decision) =>
          inReviewList(decision, isCoverageStatement, changes.has(decision.ref))
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
