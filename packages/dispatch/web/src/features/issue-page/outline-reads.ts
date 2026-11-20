import { type QueryObserverResult, useQueries } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import { primarySpec, roleDocument } from "../../api/issue-cache";
import { artifactBlocksQuery, issueCoverageQuery, issueResultsQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import type { IssueTreeReads, TreeNode } from "./issue-tree";
import { STATE_LINES } from "./page-words";
import {
  EVENT_FRESH,
  issueAsksQuery,
  standsFailed,
  treeDocumentIds,
  useIssueTree,
} from "./useIssueTree";

/** One read the outline's rows or their names come from that stands failed (`standsFailed`), as
 *  the outline draws it: one line over the tree (`FailedReadLine`), with a Retry that asks that
 *  read alone again. */
export interface FailedRead {
  /** Whether the questions come from the read, as of the reads here they come from the asks':
   *  the issue's own read brings its open asks too (`tree-drafts.ts`'s `addAsks`), so a question
   *  that waits on the reader is drawn while this read stands failed where the issue's read holds
   *  it, and the page's first focus goes to that question (`IssueTree`'s opening). Where none is
   *  drawn as the reads settle, it goes to this line's Retry before any other's, since a question
   *  the issue's read does not hold comes from this read alone. */
  readonly bringsQuestions: boolean;
  /** Which failure of the read the line says, which keys the line's alert, so a second failure
   *  is said again: a read's own `errorUpdateCount` (TanStack Query's), and for the names the
   *  count of their reads' failures since every one of them last answered
   *  (`NamesFailure.failure`), which is no read's own. */
  readonly failure: number;
  /** Tells the read from every other read of the outline: its line's React key. */
  readonly key: string;
  /** What the outline says of the read, in the page's words (`STATE_LINES`). */
  readonly message: string;
  /** Whether rows come from the read, as they come from every read here but the names': where
   *  no question that waits on the reader is drawn as the reads settle, the page's first focus
   *  goes to the first such line, the questions' before it (`bringsQuestions`), where one
   *  stands failed (`IssueTree`'s opening), and the names place no row. */
  readonly placesRows: boolean;
  /** Asks that read alone again. */
  readonly retry: () => void;
  /** Whether the read runs again (TanStack Query's `isFetching`). */
  readonly retrying: boolean;
}

/** What the outline's reads give (`useOutlineReads`). Each field is one value for as long as the
 *  reads say the same; the object holding them is new on every render. */
export interface OutlineReads {
  /** Each read the tree's rows or their names come from that stands failed (`standsFailed`), in
   *  the order the reads are given: the six reads (`reads`), each document's blocks
   *  (`treeDocumentIds`), and the names last, as the tree reads them last (`useIssueTree`).
   *  Empty while none does. */
  readonly failed: readonly FailedRead[];
  /** Whether the names the tree is built from are held (`IssueTreeView.namesHeld`): a plan
   *  task's or a callout's words come from them alone, so what a words filter matches waits on
   *  them, as well as on the reads the nodes come from (`readsHeld`). */
  readonly namesHeld: boolean;
  /** The reads of the issue's conversation the tree's turns come from, each undefined until it
   *  has answered, and whether each of the six reads the tree's other nodes come from - the
   *  coverage, the decisions, the results, the asks, the comments and the messages - holds its
   *  answer (`held`). */
  readonly reads: {
    readonly asks: IssueTreeReads["asks"];
    readonly comments: IssueTreeReads["comments"];
    readonly held: boolean;
    readonly messages: IssueTreeReads["messages"];
  };
  /** Whether every read the tree's nodes come from holds its answer: `reads.held`, and every
   *  document the tree reads holding its blocks (`IssueTreeView.blocksHeld`). */
  readonly readsHeld: boolean;
  /** Whether every read the tree's rows come from has answered, with its data or with its
   *  failure: the six reads here and every document's blocks. It is what the page's first focus
   *  waits on, since until then a read still out can draw rows above the row it would land on,
   *  and a failed read answers too, where `readsHeld` would wait for ever. */
  readonly readsSettled: boolean;
  /** The whole tree (`useIssueTree`). */
  readonly root: TreeNode;
}

/** No read stands failed: the same empty list on every render, so the tree's lines and its
 *  opening, which read it, see one value; the tree's own where the page gives it none. */
export const NO_FAILED_READS: readonly FailedRead[] = [];

/** The six reads the tree's nodes other than the issue's children and the documents' blocks come
 *  from, in the order they are given, each under the key and with the freshness `useIssueTree`
 *  reads it by, so they cost no request of their own; and the key each one's line is drawn under,
 *  what it says, and whether the questions come from it (`failedLines`). */
function sixReads(key: string) {
  return [
    {
      bringsQuestions: false,
      line: "coverage",
      message: STATE_LINES.coverage_unreadable,
      query: { ...issueCoverageQuery(key), ...EVENT_FRESH },
    },
    {
      bringsQuestions: false,
      line: "decisions",
      message: STATE_LINES.issue_decisions_unreadable,
      query: { ...issueDecisionsQuery(key, true), ...EVENT_FRESH },
    },
    {
      bringsQuestions: false,
      line: "results",
      message: STATE_LINES.results_unreadable,
      query: { ...issueResultsQuery(key), ...EVENT_FRESH },
    },
    {
      bringsQuestions: true,
      line: "asks",
      message: STATE_LINES.asks_unreadable,
      query: { ...issueAsksQuery(key), ...EVENT_FRESH },
    },
    {
      bringsQuestions: false,
      line: "comments",
      message: STATE_LINES.comments_unreadable,
      query: { queryFn: () => api.listComments(key), queryKey: ["comments", key], ...EVENT_FRESH },
    },
    {
      bringsQuestions: false,
      line: "messages",
      message: STATE_LINES.messages_unreadable,
      query: {
        queryFn: () => api.listIssueMessages(key),
        queryKey: ["issue-messages", key],
        ...EVENT_FRESH,
      },
    },
  ] as const;
}

/** Each of `reads`' queries, in their order: a tuple, since `useQueries` types each answer by
 *  its query's place. */
function queriesOf<T extends readonly { readonly query: object }[]>(
  reads: T
): { [K in keyof T]: T[K] extends { readonly query: infer Q } ? Q : never } {
  return reads.map((read) => read.query) as {
    [K in keyof T]: T[K] extends { readonly query: infer Q } ? Q : never;
  };
}

/** The line of each of `answers`' reads that stands failed (`standsFailed`), in their order, and
 *  none for a read that does not: while its read runs again the line stays, busy (`retrying`).
 *  The read at `index` draws its line under the key and in the words `lineOf(index)` gives, which
 *  says too whether the questions come from it, and is asked only for a read that stands failed.
 *  Its Retry is the read's own `refetch`, which asks that read alone again and is one function
 *  for as long as the read is observed, so a line that says the same is the same value. */
function failedLines(
  answers: readonly QueryObserverResult<unknown>[],
  lineOf: (index: number) => Pick<FailedRead, "bringsQuestions" | "key" | "message">
): FailedRead[] {
  return answers.flatMap((answer, index) => {
    if (!standsFailed(answer)) return [];
    return [
      {
        ...lineOf(index),
        failure: answer.errorUpdateCount,
        placesRows: true,
        retry: answer.refetch,
        retrying: answer.isFetching,
      },
    ];
  });
}

/** What the outline says where the blocks of the document `id` could not be read: the document
 *  named by the part the tree reads it for (`treeDocumentIds`), the first of them where one
 *  document plays two. */
function blocksUnreadable(issue: IssueDetails, id: string): string {
  if (primarySpec(issue)?.id === id) return STATE_LINES.spec_unreadable;
  if (roleDocument(issue, "plan")?.id === id) return STATE_LINES.plan_unreadable;
  if (roleDocument(issue, "verification")?.id === id) return STATE_LINES.verification_unreadable;
  return STATE_LINES.decisions_document_unreadable;
}

/**
 * The issue's tree, the reads the outline waits on, and the line of each that stands failed
 * (`failed`): for whatever draws or lands on the outline, and for the view row, which counts each
 * view's nodes once every read holds its answer (`readsHeld`), and under a words filter once the
 * names are held too (`namesHeld`), and draws no line. One place for those reads and how they
 * are kept fresh, so every caller reads each of them under the one key and with the one
 * freshness, and two callers on one page cost no request more than one. A caller that draws no
 * line costs no render for them while no read stands failed: `useQueries` shares what each
 * `combine` gives structurally, and the list is the one empty list (`NO_FAILED_READS`) while
 * none does.
 *
 * Whether every read the tree's nodes come from holds its answer (`readsHeld`): the issue's
 * children come with the issue, the documents' blocks are `blocksHeld`, and every other node
 * comes from one of the six reads here, read under the keys `useIssueTree` reads them by, so
 * they cost no request of their own. A read that stands failed holds nothing either
 * (`standsFailed`): the page cannot say what it would have held, so the outline says that the
 * read failed (`failed`). One whose refresh failed holds the answer it had, which its rows are
 * drawn from, and says nothing. The names' line says the tree's names reads that stand failed
 * (`IssueTreeView.namesFailure`) - the tree draws the names an older set's read gave meanwhile -
 * and its Retry asks those reads again and no other. Whether the names are held is the tree's
 * own answer (`IssueTreeView.namesHeld`). The conversation's reads are kept too, which tell a
 * turn the outline leaves out (`useAddressFallback`).
 */
export function useOutlineReads(issue: IssueDetails): OutlineReads {
  const { blocksHeld, namesFailure, namesHeld, root } = useIssueTree(issue);
  const key = issue.key;
  const documentIds = treeDocumentIds(issue);
  const documents = useQueries({
    combine: (answers): { failed: readonly FailedRead[]; settled: boolean } => ({
      failed: failedLines(answers, (index) => {
        const id = documentIds[index] ?? "";
        return {
          bringsQuestions: false,
          key: `blocks:${id}`,
          message: blocksUnreadable(issue, id),
        };
      }),
      settled: answers.every((answer) => !answer.isPending),
    }),
    // The keys and the freshness `useIssueTree` reads each document's blocks under, so these
    // cost no request of their own.
    queries: documentIds.map((id) => ({ ...artifactBlocksQuery(id), ...EVENT_FRESH })),
  });
  const six = sixReads(key);
  const own = useQueries({
    combine: (
      answers
    ): { failed: readonly FailedRead[]; reads: OutlineReads["reads"]; settled: boolean } => {
      const [, , , asks, comments, messages] = answers;
      return {
        failed: failedLines(answers, (index) => ({
          bringsQuestions: six[index].bringsQuestions,
          key: six[index].line,
          message: six[index].message,
        })),
        reads: {
          asks: asks.data,
          comments: comments.data,
          held: answers.every((answer) => answer.data !== undefined),
          messages: messages.data,
        },
        settled: answers.every((answer) => !answer.isPending),
      };
    },
    queries: queriesOf(six),
  });
  const names = useMemo(
    (): readonly FailedRead[] =>
      namesFailure === undefined
        ? NO_FAILED_READS
        : [
            {
              ...namesFailure,
              bringsQuestions: false,
              key: "names",
              message: STATE_LINES.names_unreadable,
              placesRows: false,
            },
          ],
    [namesFailure]
  );
  const failed = useMemo(
    () =>
      own.failed.length + documents.failed.length + names.length === 0
        ? NO_FAILED_READS
        : [...own.failed, ...documents.failed, ...names],
    [documents.failed, names, own.failed]
  );
  return {
    failed,
    namesHeld,
    reads: own.reads,
    readsHeld: own.reads.held && blocksHeld,
    readsSettled: own.settled && documents.settled,
    root,
  };
}
