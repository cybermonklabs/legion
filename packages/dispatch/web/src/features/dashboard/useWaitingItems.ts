import { queryOptions, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import { issueCoverageQuery, issueQuery, myRepliesKey, whoAmIQuery } from "../../api/queries";
import type {
  AskApproval,
  Decision,
  IssueAskRow,
  IssueCoverage,
  IssueDecisions,
  IssueDetails,
  MyRepliesResponse,
  MyReplyRow,
} from "../../api/types";
import { compareTimestamps, isTimestamp } from "../../lib/timestamps";
import { decisionsDocumentId } from "../decision-card/change-anchor";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { waitingOnHuman } from "../inbox/BlockedOnYou";
import { type ChangeComment, decisionChanges, inReviewList } from "../issue-page/decision-review";
import { coverageStatements } from "../issue-page/statement-rules";
import { EVENT_FRESH } from "../refs/reference-target";

/**
 * What waits on the reader (dispatch://INFRA-5702/spec#b-r-waiting): R4's items over R8's reads,
 * each listed once, newest first. Dashboard's list and the new page's header count read this one
 * rule (R10), so the count is the number of items and never the `total` of a read.
 *
 * - The questions: each open ask of `asks?state=open` other than an approval request whose turn
 *   is the reader's, by the turn rule `issue-page/waiting.ts` reads: it waits on a person
 *   (`waitingOnHuman`) and that person (`waiting_on_person`) is the reader.
 * - The decisions: each decision of the review list (`inReviewList`, over the statements of the
 *   coverage read, `coverageStatements`), less one a person asked to change since it last changed
 *   (`decisionChanges`, a Change matched to its decision by its anchor, with no blocks read).
 * - The approval requests: one item for each open approval request whose turn is the reader's,
 *   naming each document it covers with its version, so one request on the spec and the
 *   Verification document together is one item.
 * - The stale approvals: one item for the spec and one for the Verification document whose
 *   approval in the issue read reads `stale` by the reader, where no such request covers the
 *   document. An approval the server carried to the newest version reads `approved`.
 * - The threads: each unread row of the reader's `me/replies` of the issue, less a row whose root
 *   the Legion daemon of the issue's project wrote (`legion-daemon:<PROJECT>`, R11), and less a
 *   row of an ask the questions or the approval requests hold, so an item is listed once.
 *
 * Where a read gives a node's state (R9), an ask or a decision whose node reads `overtaken` is no
 * item. No read of this `main` gives one, so the hook hands none.
 */

/** The fields of the issue read the rule reads. */
export type WaitingIssue = Pick<
  IssueDetails,
  "artifacts" | "key" | "primary_artifact_id" | "project" | "verification_artifact_id"
>;

/** A node's state as a read that gives one names the node: by its id, and by its block's id for
 *  a block (`GET /api/v1/issues/{key}/tree`'s nodes, dispatch://INFRA-1253). */
export interface NodeStateRead {
  readonly id: string;
  readonly block_id?: string;
  readonly state: string;
}

/** The reads the rule reads, each undefined while it has not answered. */
export interface WaitingReads {
  /** The reader's login (`/auth/whoami`): nothing waits on a reader whose login has not
   *  resolved. */
  readonly viewer: string | undefined;
  readonly issue: WaitingIssue | undefined;
  /** The issue's open asks (`asks?state=open`). */
  readonly asks: readonly IssueAskRow[] | undefined;
  readonly decisions: IssueDecisions | undefined;
  /** The comments of the decisions document (`comments?artifact=`), which hold the Changes. No
   *  decision is an item until they answer, where the issue holds a decisions document, so no
   *  item is drawn that leaves as they answer. */
  readonly changes: readonly ChangeComment[] | undefined;
  readonly coverage: IssueCoverage | undefined;
  /** The reader's unread threads of the issue (`me/replies?issue=<KEY>&state=unread`). */
  readonly replies: Pick<MyRepliesResponse, "rows"> | undefined;
  /** The node states a read gives (R9), where one does. */
  readonly nodes?: readonly NodeStateRead[];
}

/** One document an approval request covers, with the version it asks the reader to approve. */
export type ApprovalDocument = NonNullable<AskApproval["documents"]>[number];

/** A document of the issue read. */
type IssueDocument = NonNullable<IssueDetails["artifacts"]>[number];

/** What every item carries: the id the item is drawn under (an ask's id for a question and an
 *  approval request, the document's id for a stale approval, the decision's block id, the
 *  thread's root id), which with its kind is its test id, `dashboard-item-<kind>-<id>`, and its
 *  time, the newest moment its records give, which orders the list. */
interface ItemBase {
  readonly id: string;
  readonly at: string;
}

export type WaitingItem =
  | (ItemBase & { readonly kind: "question"; readonly ask: IssueAskRow })
  | (ItemBase & {
      readonly kind: "approval";
      readonly ask: IssueAskRow;
      readonly documents: readonly ApprovalDocument[];
    })
  | (ItemBase & { readonly kind: "stale"; readonly document: IssueDocument })
  | (ItemBase & { readonly kind: "decision"; readonly decision: Decision })
  | (ItemBase & { readonly kind: "thread"; readonly row: MyReplyRow });

export type WaitingKind = WaitingItem["kind"];

/** The newest of `moments` that is a time, or "" where none is. */
function newest(moments: readonly (string | null | undefined)[]): string {
  return moments
    .filter((moment): moment is string => typeof moment === "string" && isTimestamp(moment))
    .reduce(
      (held, moment) => (held === "" || compareTimestamps(moment, held) > 0 ? moment : held),
      ""
    );
}

/** Orders two items newest first. An item whose time cannot be read is the oldest. */
function newerFirst(first: WaitingItem, second: WaitingItem): number {
  const firstRead = isTimestamp(first.at);
  const secondRead = isTimestamp(second.at);
  if (firstRead && secondRead) return compareTimestamps(second.at, first.at);
  return Number(secondRead) - Number(firstRead);
}

/** The documents an approval request covers: each of `approval.documents`, or the one document
 *  of a request that lists none. */
function approvalDocuments(ask: IssueAskRow): readonly ApprovalDocument[] {
  const approval = ask.approval;
  if (approval === undefined) return [];
  const documents = approval.documents ?? [];
  return documents.length > 0
    ? documents
    : [
        {
          artifact_id: approval.artifact_id,
          name: approval.name,
          requested_version: approval.requested_version,
          version: approval.version,
        },
      ];
}

/** Whether `author` is the Legion daemon's session of `project` (R11). */
function isDaemon(author: MyReplyRow["root_author"], project: string): boolean {
  return author?.kind === "session" && author.id === `legion-daemon:${project}`;
}

/** R4's items over `reads`, newest first. */
export function waitingItems(reads: WaitingReads): readonly WaitingItem[] {
  const { viewer, issue } = reads;
  if (viewer === undefined) return [];
  // A node by its id, and a block's node by its block's id too, which a decision is drawn under.
  const overtaken = new Set(
    (reads.nodes ?? [])
      .filter((node) => node.state === "overtaken")
      .flatMap((node) => (node.block_id === undefined ? [node.id] : [node.id, node.block_id]))
  );

  const readersAsks = waitingOnHuman(
    (reads.asks ?? []).filter((ask) => ask.state === "open" && !overtaken.has(ask.id))
  ).filter((ask) => ask.waiting_on_person === viewer);
  const askTime = (ask: IssueAskRow) =>
    newest([ask.created_at, ask.edited_at, ask.asked_of_at, ask.last_reply?.created_at]);
  const questions: WaitingItem[] = readersAsks
    .filter((ask) => ask.kind !== "approval")
    .map((ask) => ({ ask, at: askTime(ask), id: ask.id, kind: "question" }));
  const approvals: Extract<WaitingItem, { kind: "approval" }>[] = readersAsks
    .filter((ask) => ask.kind === "approval")
    .map((ask) => ({
      ask,
      at: askTime(ask),
      documents: approvalDocuments(ask),
      id: ask.id,
      kind: "approval",
    }));

  const covered = new Set(
    approvals.flatMap((item) => item.documents.map((one) => one.artifact_id))
  );
  const documents = issue?.artifacts ?? [];
  const stale: WaitingItem[] = [
    ...new Set([issue?.primary_artifact_id, issue?.verification_artifact_id]),
  ].flatMap((id) => {
    const document = documents.find((one) => id !== "" && one.id === id);
    const approval = document?.approval;
    if (
      document === undefined ||
      approval?.state !== "stale" ||
      approval.by?.id !== viewer ||
      covered.has(document.id)
    ) {
      return [];
    }
    const latest = document.versions?.find((version) => version.number === approval.latest_version);
    return [
      {
        at: newest([latest?.created_at, approval.at, document.created_at]),
        document,
        id: document.id,
        kind: "stale",
      },
    ];
  });

  const decisionsDocument = decisionsDocumentId(documents, reads.decisions);
  const changesRead = decisionsDocument === undefined || reads.changes !== undefined;
  const changed = decisionChanges(reads.decisions, reads.changes, documents, undefined);
  const isStatement = issue === undefined ? () => false : coverageStatements(issue, reads.coverage);
  const decisions: WaitingItem[] = changesRead
    ? (reads.decisions?.decisions ?? [])
        .filter(
          (decision) =>
            inReviewList(decision, isStatement, changed.has(decision.ref)) &&
            !overtaken.has(decision.block_id)
        )
        .map((decision) => ({
          at: newest([
            decision.changed_at,
            ...(decision.links ?? []).map((link) => link.created_at),
          ]),
          decision,
          id: decision.block_id,
          kind: "decision",
        }))
    : [];

  const asked = new Set(readersAsks.map((ask) => ask.id));
  const threads: WaitingItem[] =
    issue === undefined
      ? []
      : (reads.replies?.rows ?? [])
          .filter((row) => !isDaemon(row.root_author, issue.project) && !asked.has(row.thread_id))
          .map((row) => ({ at: row.reply.created_at, id: row.thread_id, kind: "thread", row }));

  return [...questions, ...approvals, ...stale, ...decisions, ...threads].sort(newerFirst);
}

/** The issue's open asks, under the key R12 names, so an ask event's refresh of the prefix
 *  `["asks", key]` (`api/event-refresh.ts`) reaches it. */
export const openAsksQuery = (key: string) =>
  queryOptions({
    queryKey: ["asks", key, "open"],
    queryFn: () => api.listIssueAsks(key, "open"),
  });

/** The comments of the issue's decisions document, which hold the Changes: one document's
 *  comments, never the issue's whole list, read through the issue's route, since the document's
 *  own refuses an issue's document (`400 ARTIFACT_LINKED`). A comment event's refresh of the prefix
 *  `["comments", key]` reaches it. */
export const decisionChangesQuery = (key: string, artifact: string) =>
  queryOptions({
    queryKey: ["comments", key, { artifact }],
    queryFn: () => api.listComments(key, artifact),
  });

/** The reader's unread threads of the issue, every one, with no `limit`, under a key of its own
 *  beneath `myRepliesKey`, so each refresh of that prefix reaches it and it replaces no other
 *  reader's data (`myRepliesOwnerQuery` reads 200 under `[...myRepliesKey, {issue}]`). */
export const dashboardRepliesQuery = (key: string) =>
  queryOptions({
    queryKey: [...myRepliesKey, "dashboard", key],
    queryFn: () => api.listMyReplies({ issue: key, state: "unread" }),
  });

/**
 * What waits on the reader on issue `issueKey`, newest first: `waitingItems` over R8's reads. The
 * count of what waits is the length of what it answers. Each read is kept fresh by the events
 * that name its key, never by its age (`EVENT_FRESH`), so a part that mounts again sends none of
 * them a second time.
 */
export function useWaitingItems(issueKey: string): readonly WaitingItem[] {
  const viewer = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).data?.login;
  const issue = useQuery({ ...issueQuery(issueKey), ...EVENT_FRESH }).data;
  const asks = useQuery({ ...openAsksQuery(issueKey), ...EVENT_FRESH }).data;
  const decisions = useQuery({ ...issueDecisionsQuery(issueKey, true), ...EVENT_FRESH }).data;
  const decisionsDocument = decisionsDocumentId(issue?.artifacts, decisions);
  const changes = useQuery({
    ...decisionChangesQuery(issueKey, decisionsDocument ?? ""),
    ...EVENT_FRESH,
    enabled: decisionsDocument !== undefined,
  }).data;
  const coverage = useQuery({ ...issueCoverageQuery(issueKey), ...EVENT_FRESH }).data;
  const replies = useQuery({ ...dashboardRepliesQuery(issueKey), ...EVENT_FRESH }).data;
  return useMemo(
    () => waitingItems({ asks, changes, coverage, decisions, issue, replies, viewer }),
    [asks, changes, coverage, decisions, issue, replies, viewer]
  );
}
