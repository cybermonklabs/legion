import { type QueryObserverResult, useQueries } from "@tanstack/react-query";
import { useMemo } from "react";

import { primarySpec, roleDocument } from "../../api/issue-cache";
import { artifactBlocksQuery, issueCoverageQuery, issueResultsQuery } from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { EVENT_FRESH, standsFailed } from "../refs/reference-target";
import type { IssueTreeReads, TreeNode } from "./issue-tree";
import { STATE_LINES } from "./page-words";
import {
  issueAsksQuery,
  issueCommentsQuery,
  issueMessagesQuery,
  treeDocumentIds,
  useIssueTree,
} from "./useIssueTree";

/** One line the outline draws over the tree (`FailedReadLine`), with a Retry. A read the rows
 *  come from has one while it stands failed (`standsFailed`), and its Retry asks that read alone
 *  again. The names reads have one line between them, which stands while their count of failures
 *  is above 0 (`IssueTreeView.namesFailure`): while a read under a new set of addresses is out as
 *  well, though no names read stands failed then. Its Retry asks again every names read that
 *  holds no answer under its set, a first read of a new set included. */
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
  /** Asks that read alone again; the names line's asks again every names read that holds no
   *  answer under its set of addresses (`NamesFailure.retry`). */
  readonly retry: () => void;
  /** Whether the read runs again (TanStack Query's `isFetching`); for the names line, whether
   *  one of the names reads that hold no answer under their set runs, again or as a first read
   *  of a new set (`NamesFailure.retrying`). */
  readonly retrying: boolean;
}

/** What the outline's reads give (`useOutlineReads`). Each field is one value for as long as the
 *  reads say the same; the object holding them is new on every render. */
export interface OutlineReads {
  /** The line of each read the tree's rows come from that stands failed (`standsFailed`), and
   *  the names' line while it stands (`IssueTreeView.namesFailure`), in the order the reads are
   *  given: the six reads (`reads`), each document's blocks (`treeDocumentIds`), and the names
   *  last, as the tree reads them last (`useIssueTree`). Empty while no line stands. */
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

/** The key the line of a read rows come from is drawn under (`FailedRead.key`). */
export type ReadKey = ReturnType<typeof sixReads>[number]["line"] | `blocks:${string}`;

/** The key the line of the blocks read of the document `id` is drawn under. */
export const blocksKey = (id: string): ReadKey => `blocks:${id}`;

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
      line: "issue-decisions",
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
      query: { ...issueCommentsQuery(key), ...EVENT_FRESH },
    },
    {
      bringsQuestions: false,
      line: "messages",
      message: STATE_LINES.messages_unreadable,
      query: {
        ...issueMessagesQuery(key),
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
 *  named by the part the tree reads it for (`treeDocumentIds`) - the spec, the plan or
 *  Verification - the first of them where one document plays two. No decisions document plays a
 *  part: a decision is a `decision` block of a document of the issue. */
function blocksUnreadable(issue: IssueDetails, id: string): string {
  if (primarySpec(issue)?.id === id) return STATE_LINES.spec_unreadable;
  if (roleDocument(issue, "plan")?.id === id) return STATE_LINES.plan_unreadable;
  return STATE_LINES.verification_unreadable;
}

/**
 * The issue's tree, the reads the outline waits on, and the line of each that stands failed, the
 * names' as the tree says it (`failed`): for whatever draws or lands on the outline, and for the
 * view row, which counts each view's nodes once every read holds its answer (`readsHeld`), and
 * under a words filter once the names are held too (`namesHeld`), and draws no line. One place
 * for those reads and how they are kept fresh, so every caller reads each of them under the one
 * key and with the one freshness, and two callers on one page cost no request more than one. A
 * caller that draws no line costs no render for them while no read stands failed: `useQueries`
 * shares what each `combine` gives structurally, and the list is the one empty list
 * (`NO_FAILED_READS`) while none does.
 *
 * Whether every read the tree's nodes come from holds its answer (`readsHeld`): the issue's
 * children come with the issue, the documents' blocks are `blocksHeld`, and every other node
 * comes from one of the six reads here, read under the keys `useIssueTree` reads them by, so
 * they cost no request of their own. A read that stands failed holds nothing either
 * (`standsFailed`): the page cannot say what it would have held, so the outline says that the
 * read failed (`failed`). One whose refresh failed holds the answer it had, which its rows are
 * drawn from, and says nothing. The names' line stands while the names reads' count of failures
 * is above 0 (`IssueTreeView.namesFailure`): from a failure of a names read that holds no answer
 * under its set of addresses until every one of them holds one, so it stays while a read under a
 * new set is out, though no names read stands failed then - the tree draws the names an older
 * set's read gave meanwhile. Its Retry asks again every names read that holds no answer under its
 * set, a first read of a new set included, and no other read, and the line is busy while one of
 * those runs (`retrying`). Whether the names are held is the tree's own answer
 * (`IssueTreeView.namesHeld`). The conversation's reads are kept too, which tell a turn the
 * outline leaves out (`useAddressFallback`).
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
          key: blocksKey(id),
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
