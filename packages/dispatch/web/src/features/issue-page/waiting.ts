import type { IssueDetails } from "../../api/types";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { askOfNode, decisionsOfAnswer } from "./decision-nodes/decision-nodes";
import { decisionNodeChanges, deletedPostIds, inReviewList, treeComments } from "./decision-review";
import { changeUnread } from "./tree-drafts";
import { nextTreeCursor } from "./tree-read/tree-nodes";
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
 * A decision is a decision node of the answer the count is read from (`answer`), which its
 * caller selects: the decisions view read (`?view=decisions`) wherever the page holds it, which
 * holds every decision and every question of the issue, however many pages of Conversation's
 * read the page holds - that read is paged (R15), and a decision past its pages held so far
 * would otherwise go uncounted - and Conversation's read until then. The header takes that
 * answer through `useDecisionsView` (`IssuePropertiesRow.tsx`), so its count and the Decisions
 * control count the same answer, and Conversation's read stands in wherever it holds none.
 * The decisions are read once per answer (`decision-nodes.ts`' `decisionsOfAnswer`) with no asks:
 * the read answers a person's decision on every ask a person answered, which says, with no asks,
 * which question is a person's decision, which never waits for review, and which agent's
 * decision restates that answer and is no decision at all (`restatesAnswer`), so the count is
 * the same without them. One the reader asked to change waits on its agent and is not counted,
 * by the outline's one rule (`decisionNodeChanges`): its Change is a comment the read the
 * decisions are counted from hangs under its node. The count matches no quote, reads no block
 * of a document and no comment list of its own. A Change may be older than the comment page of
 * either read; a marked decision (`changeUnread`) is not counted until the older pages resolve
 * it. A Change on a decision past Conversation's pages can still be read with its decision.
 * A Change its author deleted asks nothing any more, as in the outline: the same
 * answer's comment nodes carry their author's marks, which say which were (`treeComments`,
 * `deletedPostIds`).
 *
 * A decision a person decided, whose `decided_by` names a person, is that person's decision and
 * never waits for review (`decision-review.ts`' `decidedByPerson`, INFRA-1196's decision 516),
 * which the review list's own rule reads (`inReviewList`), so it is not counted, whatever the
 * read's `waiting_review` says, as the outline marks it waiting on nobody (`draftDecision`). Who
 * decided it is the answer's `decided_by` tuple, so the decisions read with no asks say it too.
 *
 * An overtaken decision or question, a node the read answers `overtaken`, counts in no
 * count of what waits (R2), as the outline marks it waiting on nobody (`draftDecision`,
 * `draftAsk`): neither a decision whose node is, nor an open ask whose node is, each ask found by
 * the one rule the outline finds an ask node's row by (`askOfNode`), in the read the decisions
 * are counted from.
 *
 * A count read from a read that names a next page counts the nodes loaded (R15), so it says so
 * (`WaitingOnReader.more`), and the header draws it with "+", as the view row draws each count
 * (`presets.ts`' `filterSignals`): Conversation's read while it names one, and the decisions
 * view read, once the page holds it, only where it names one itself. So the count never claims a
 * number the reads it is read from cannot hold.
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
  /** Whether the read the decisions are counted from names a next page (R15), so `total` counts
   *  the decisions of the pages loaded and the header draws it with "+"; absent where it names
   *  none. */
  readonly more?: true;
  /** Decisions that may have a Change on an older comment page. The header reads older pages
   *  of the answer it counts while any remain marked (`useReadOlderDecisionComments`). */
  readonly unread: number;
}

export function waitingOnReader(
  issue: Pick<IssueDetails, "artifacts" | "key" | "open_asks" | "primary_artifact_id">,
  viewer: string | undefined,
  isCoverageStatement: (reference: string) => boolean,
  {
    answer,
  }: {
    /** The read the decisions, the questions and the Changes are counted from: the decisions
     *  view read where the page holds it, which hangs each Change under its decision, and
     *  Conversation's read until then, as the caller selects it; none for a count of the
     *  approvals alone. */
    readonly answer?: TreeAnswer;
  } = {}
): WaitingOnReader {
  const overtaken = overtakenAsks(issue, viewer === undefined ? undefined : answer);
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
  // one who has. The decision nodes a Change waits on, by the node's id, are from the read the
  // decisions are counted from alone, by the one rule every reader of a Change reads, and so is
  // which Changes their author deleted, which ask nothing any more, as in the outline.
  let reviewing = 0;
  let unread = 0;
  if (viewer !== undefined && answer !== undefined) {
    const changes = decisionNodeChanges(answer, undefined, deletedPostIds(treeComments(answer)));
    for (const decision of decisionsOfAnswer(answer, issue.key).values()) {
      const changed = changes.has(decision.node.id);
      const marked = changeUnread(decision, changed, answer);
      if (marked) unread += 1;
      if (
        decision.node.state !== "overtaken" &&
        inReviewList(decision, isCoverageStatement, changed || marked)
      ) {
        reviewing += 1;
      }
    }
  }
  const more = viewer !== undefined && answer !== undefined && nextTreeCursor(answer) !== undefined;
  return {
    approvals,
    asks,
    decisions: reviewing,
    ...(more ? { more: true as const } : {}),
    total: asks + reviewing + approvals.length,
    unread,
  };
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
