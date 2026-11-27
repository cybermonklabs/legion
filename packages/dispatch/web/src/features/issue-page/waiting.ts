import type { IssueDetails } from "../../api/types";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import type { DecisionReads } from "./decision-nodes/decision-nodes";
import { decisionsOfAnswer } from "./decision-nodes/decision-nodes";
import { decisionNodeChanges, deletedPostIds, inReviewList, treeComments } from "./decision-review";
import { closedReadIds } from "./node-states/node-states";
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
 * would otherwise go uncounted - and Conversation's read until then. The page reads the
 * decisions view the first time the Decisions filter is on in a load (`ViewReadProvider`), so
 * until it holds an answer in that load, and where that read fails, Conversation's read stands
 * in (`IssuePropertiesRow.tsx`); on the Decisions view, while a Keep or a Change has dropped the
 * view, the answer the drop took stands until the next lands, as the Decisions control counts it
 * (`useDecisionsView`).
 * The decisions are read once per answer (`decision-nodes.ts`' `decisionsOfAnswer`) with no asks:
 * the read answers a person's decision on every ask a person answered, which says, with no asks,
 * which question is a person's decision, which never waits for review, and which agent's
 * decision restates that answer and is no decision at all (`restatesAnswer`), so the count is
 * the same without them. One the reader asked to change waits on its agent and is not counted,
 * by the outline's one rule (`decisionNodeChanges`): its Change is a comment the read the
 * decisions are counted from hangs under its node - the decisions view read, which carries each
 * comment on a node it holds, wherever the page holds it, and Conversation's read only until
 * then - so the count matches no quote, reads no block of a document and no comment list of its
 * own. A Change on a decision past the pages of Conversation's read held so far is so read with
 * its decision. A Change its author deleted asks nothing any more, as in the outline: the same
 * answer's comment nodes carry their author's marks, which say which were (`treeComments`,
 * `deletedPostIds`).
 *
 * A decision a person decided, whose `decided_by` names a person, is that person's decision and
 * never waits for review (`decision-review.ts`' `decidedByPerson`, INFRA-1196's decision 516),
 * which the review list's own rule reads (`inReviewList`), so it is not counted, whatever the
 * read's `waiting_review` says, as the outline marks it waiting on nobody (`draftDecision`). Who
 * decided it is the answer's `decided_by` tuple, so the decisions read with no asks say it too.
 *
 * Nothing at or under an overtaken node waits on anybody: a later node took its place, and the
 * tree draws none of them in the waiting tone. The nodes are read from Conversation's read (`tree`)
 * and the read the count is read from (`answer`) together, the latter's node where both hold one id
 * (`joinedReads`): the decisions view read holds each question by its ask's own node, each decision
 * and each reading, and never the block a question is written as, which Conversation's read holds.
 * A node is under another where its `parent` chain reaches it, so an open ask or a decision there
 * is not counted. A proposed one still is: no node took its place yet. A question written as a
 * block of a document is overtaken where its ask's node is, and never by a state its block's node
 * holds (`ownStatesOf`), as the tree draws it: the tree draws the block and its ask as one row,
 * which takes the ask's state, so where the ask's node is overtaken the block is closed too, and
 * every node either read hangs under it. Which block a question is written as is read as the tree
 * reads it, through the page's one pairing rule (`decision-nodes.ts`' `askOfNode`) over the issue's
 * open asks and the asks read (`asksRead`, the page's), so a question closes its block with its
 * ask's node whether it is open or answered (`node-states.ts`' `closedReadIds`), and an open ask
 * is not counted where its ask's node or its block is closed. The Changes are read from the read
 * the decisions are counted from, as above (`decisionNodeChanges`): a Change under a closed
 * decision moves no count, since that decision is not counted.
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
}

export function waitingOnReader(
  issue: Pick<IssueDetails, "artifacts" | "key" | "open_asks" | "primary_artifact_id">,
  viewer: string | undefined,
  isCoverageStatement: (reference: string) => boolean,
  {
    answer,
    asksRead,
    tree,
  }: {
    /** The read the decisions, the questions and the Changes are counted from: the decisions
     *  view read where the page holds it, which hangs each Change under its decision, and
     *  Conversation's read until then, as the caller selects it; none for a count of the
     *  approvals alone. */
    readonly answer?: TreeAnswer;
    /** The page's asks read, open and answered, which pairs a question's block with its ask
     *  (`askOfNode`), so an overtaken question closes its block with its ask's node. */
    readonly asksRead?: DecisionReads["asks"];
    /** Conversation's read, which holds the block of a document each question is written as and
     *  the decisions view read never does: the nodes are closed over it and `answer` together
     *  (`joinedReads`). */
    readonly tree?: TreeAnswer;
  } = {}
): WaitingOnReader {
  // Each open ask and node at or under an overtaken node taken out, the decisions among those
  // nodes with them, and the count made of what is left. The nodes are closed over both reads
  // (`joinedReads`), so a question's block, which only Conversation's read holds, closes with its
  // ask's node, and so does each node the decisions view hangs under that block. The reads are
  // joined only where one of them holds a node whose `state` is overtaken (`holdsOvertaken`):
  // `closedReadIds` closes nothing of a read with no node overtaken by its own state, the same as
  // of no read, so without one the join is not built.
  const { askIds: closedAsks, readIds: closed } = closedReadIds(
    viewer !== undefined && (holdsOvertaken(tree) || holdsOvertaken(answer))
      ? joinedReads(tree, answer)
      : undefined,
    { asks: asksRead, issue }
  );
  if (closed.size > 0) {
    issue = {
      ...issue,
      open_asks: (issue.open_asks ?? []).filter((ask) => !closedAsks.has(ask.id)),
    };
    if (answer !== undefined) {
      answer = { ...answer, nodes: (answer.nodes ?? []).filter((node) => !closed.has(node.id)) };
    }
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
  // one who has. The decision nodes a Change waits on, by the node's id, are from the read the
  // decisions are counted from alone, by the one rule every reader of a Change reads, and so is
  // which Changes their author deleted, which ask nothing any more, as in the outline.
  let reviewing = 0;
  if (viewer !== undefined && answer !== undefined) {
    const changes = decisionNodeChanges(answer, undefined, deletedPostIds(treeComments(answer)));
    reviewing = [...decisionsOfAnswer(answer, issue.key).values()].filter(
      (decision) =>
        decision.node.state !== "overtaken" &&
        inReviewList(decision, isCoverageStatement, changes.has(decision.node.id))
    ).length;
  }
  const more = viewer !== undefined && answer !== undefined && nextTreeCursor(answer) !== undefined;
  return {
    approvals,
    asks,
    decisions: reviewing,
    ...(more ? { more: true as const } : {}),
    total: asks + reviewing + approvals.length,
  };
}

/** The nodes of Conversation's read `tree` and of the read the count is read from, `answer`, as
 *  one read, `answer`'s node where both hold one id, which the header's count closes nodes and
 *  asks over (`closedReadIds`) where either read holds an overtaken node (`holdsOvertaken`):
 *  `answer` alone where it is Conversation's read itself, either read alone where the count is
 *  handed only that one, and none while it is handed neither. */
function joinedReads(
  tree: TreeAnswer | undefined,
  answer: TreeAnswer | undefined
): TreeAnswer | undefined {
  if (tree === undefined || answer === undefined || tree === answer) return answer ?? tree;
  const answerNodes = answer.nodes ?? [];
  const held = new Set(answerNodes.map((node) => node.id));
  return {
    ...answer,
    nodes: [...(tree.nodes ?? []).filter((node) => !held.has(node.id)), ...answerNodes],
  };
}

/** Whether `read` holds a node whose `state` is overtaken. `closedReadIds` closes nothing of a
 *  read with no node overtaken by its own state (`ownStatesOf`), and a node's own state is
 *  overtaken only where its `state` is, so while neither read the count is handed holds one, the
 *  reads are not joined. It stops at the first such node and allocates nothing. */
function holdsOvertaken(read: TreeAnswer | undefined): boolean {
  if (!read?.nodes) return false;
  for (const node of read.nodes) {
    if (node.state === "overtaken") return true;
  }
  return false;
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
