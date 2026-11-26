import { type QueryObserverResult, useQueries, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import {
  issueAskThreadsQuery,
  issueCoverageQuery,
  issueResultsQuery,
  whoAmIQuery,
} from "../../api/queries";
import type { IssueDetails, MessageRead } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import type { PublishedReferences } from "../refs/reference-names";
import { EVENT_FRESH, standsFailed } from "../refs/reference-target";
import type { IssueTreeReads, TreeNode } from "./issue-tree";
import { useOwnedIssueTree } from "./issue-tree-owner";
import { STATE_LINES } from "./page-words";
import {
  type IssueTreeView,
  issueCommentsQuery,
  issueMessagesQuery,
  type NamesFailure,
} from "./useIssueTree";

/** One line the outline draws over the tree (`FailedReadLine`), with a Retry. A read the rows
 *  come from has one while it stands failed (`standsFailed`), and its Retry asks that read alone
 *  again. The names read has one line, which stands while its count of failures is above 0
 *  (`IssueTreeView.namesFailure`): while its read under a new set of addresses is out as well,
 *  though it does not stand failed then. Its Retry asks the names read again while it holds no
 *  answer under its set, a first read of a new set included. */
export interface FailedRead {
  /** Whether the questions come from the read, as of the reads here they come from the asks':
   *  the issue's own read brings its open asks too (`tree-read/tree-nodes.ts`'s `asksOf`), so a
   *  question that waits on the reader is drawn while this read stands failed where the issue's
   *  read holds it, and the page's first focus goes to that question (`IssueTree`'s opening).
   *  Where none is drawn as the reads settle, it goes to this line's Retry before any other's,
   *  since a question the issue's read does not hold comes from this read alone. */
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
  /** Asks that read alone again; the names line's asks the names read again while it holds no
   *  answer under its set of addresses (`NamesFailure.retry`). */
  readonly retry: () => void;
  /** Whether the read runs again (TanStack Query's `isFetching`); for the names line, whether
   *  the names read runs while it holds no answer under its set, again or as a first read of a
   *  new set (`NamesFailure.retrying`). */
  readonly retrying: boolean;
}

/** What the outline's reads give (`useOutlineReads`). Each field is one value for as long as the
 *  reads say the same; the object holding them is new on every render. */
export interface OutlineReads {
  /** Each role document's blocks, from the read that holds them (`IssueTreeView.blocksOf`):
   *  under that role's own filter its view read, and else the outline read; so a view that
   *  draws from them reads them here, through the observers the outline's reads hold. */
  readonly blocksOf: IssueTreeView["blocksOf"];
  /** Whether the coverage read has neither answered nor been refused yet: until it has, each
   *  statement's row holds the line its counts will be drawn in (`TreeRows.tsx`), so the counts
   *  arriving move nothing (the plan's "The places held", row 6). A read refused before it
   *  answered holds no place again while it runs again (a Retry, an event's refresh): its row
   *  draws no counts until a read of it answers. */
  readonly coverageOut: boolean;
  /** The line of each read the tree's rows come from that stands failed (`standsFailed`), and
   *  the names' line while it stands (`IssueTreeView.namesFailure`), in the order the reads are
   *  given: the six reads (`reads`), and the names last, as the tree reads them last
   *  (`useIssueTree`). A tree read refused before it first answered draws a line of its own
   *  (`tree-read/view-read.tsx`'s `TreeReadFailed`). Empty while no line stands. */
  readonly failed: readonly FailedRead[];
  /** The names' line alone while it stands (`IssueTreeView.namesFailure`), the last of `failed`:
   *  one line in the outline's words with its Retry, which a view the stage draws in the
   *  outline's place draws over its own lines, and the one empty list (`NO_FAILED_READS`) while
   *  none stands. */
  readonly names: readonly FailedRead[];
  /** Whether the names the tree is built from are held (`IssueTreeView.namesHeld`), which the
   *  line that nothing holds a words filter's words waits on (`slots/body.tsx`), as it waits on
   *  the reads the tree is built from (`readsHeld`). */
  readonly namesHeld: boolean;
  /** Whether every read that can place a row of the tree, or say which row the page's first
   *  focus goes to, has answered, with its data or with its failure: the tree read
   *  (`TreeRead.pending`), the decisions, the asks and the comments, each of which can draw a
   *  node or move one - a decision no node holds, an ask's answer, a Change under its decision -
   *  and the reader's login, which says which open question waits on the reader
   *  (`IssueTreeReads.viewer`) and so where the page opens. The outline first draws its rows once
   *  it holds (`slots/body.tsx`), so that first drawing holds every row where it stays. The other
   *  three of the six only mark rows those reads placed, so the rows do not wait on them: the
   *  coverage a statement's counts, in a place the row holds for them from its first drawing
   *  (`coverageOut`); the results a check's state, which a check of the Verification document
   *  draws as `no-result` until they answer; and the messages what an author did to a message,
   *  since the tree read answers every message as a node (`useIssueTree`). A failed read
   *  answers too, where `readsHeld` would wait for ever. */
  readonly placesSettled: boolean;
  /** What the page publishes of its batched names read (`IssueTreeView.published`), which the
   *  outline hands its rows in the render that draws them (`IssueTree`'s `published`). */
  readonly published: PublishedReferences;
  /** The reads of the issue's conversation, each undefined until it has answered, which tell a
   *  turn the outline leaves out (`useAddressFallback`), and whether each of the six reads the
   *  tree is built from beside the tree read - the coverage, the decisions, the results, the
   *  asks, the comments and the messages - holds its answer (`held`). */
  readonly reads: {
    readonly asks: IssueTreeReads["asks"];
    readonly comments: IssueTreeReads["comments"];
    readonly held: boolean;
    readonly messages: readonly MessageRead[] | undefined;
  };
  /** Whether every read the tree is built from holds its answer: `reads.held`, and the tree read
   *  holding its answer (`IssueTreeView.blocksHeld`). */
  readonly readsHeld: boolean;
  /** Whether every read the tree's rows come from has answered, with its data or with its
   *  failure: the six reads here and the tree read (`TreeRead.pending`). It is what the page's
   *  first focus waits on, since until then a read still out can draw rows above the row it would
   *  land on, and a failed read answers too, where `readsHeld` would wait for ever. */
  readonly readsSettled: boolean;
  /** The lines a view tells the stage by (`slots/refused-reads.ts`'s `useTellsAddressDrawn`), by
   *  the key each is drawn under and whether rows come from its read: `failed`, and while the tree
   *  read stands refused (`TreeRead.failed`) the tree read's own line, under the key a view
   *  tells its blocks' line by (`blocksKey`). The outline draws that line itself
   *  (`TreeReadFailed`) and never among `failed`, so it is here alone. */
  readonly refused: readonly RefusedLine[];
  /** The whole tree (`useIssueTree`). */
  readonly root: TreeNode;
}

/** A line as a view tells the stage by it (`OutlineReads.refused`): its key, and whether rows come
 *  from its read. */
export type RefusedLine = Pick<FailedRead, "key" | "placesRows">;

/** No read stands failed: the same empty list on every render, so the tree's lines and its
 *  opening, which read it, see one value; the tree's own where the page gives it none. */
export const NO_FAILED_READS: readonly FailedRead[] = [];

/** The key of the tree read's line, the outline read's, whose refusal draws one line
 *  (`tree-read/view-read.tsx`'s `TreeReadFailed`), and the key a view tells its blocks' line by. */
const TREE_READ_LINE = "tree";

/** The key the line of a read rows come from is drawn under (`FailedRead.key`). */
export type ReadKey = ReturnType<typeof sixReads>[number]["line"] | typeof TREE_READ_LINE;

/** The key a view tells the line of the document `id`'s blocks by (`useTellsAddressDrawn`): the
 *  tree read's line for every document, whichever read holds its blocks (`TreeRead.blocksOf`). */
export const blocksKey = (_id: string): ReadKey => TREE_READ_LINE;

/** The six reads the tree is built from beside the tree read, in the order they are given, each
 *  under the key and with the freshness `useIssueTree` reads it by, so they cost no request of
 *  their own; and the key each one's line is drawn under, what it says, and whether the questions
 *  come from it (`failedLines`). */
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
      query: { ...issueAskThreadsQuery(key), ...EVENT_FRESH },
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

/** The names' line while it stands (`IssueTreeView.namesFailure`), in the outline's words: the
 *  outline's and a view's, both through `useOutlineReads`, drawn by one rule. One value for
 *  as long as the names' failure says the same, and the one empty list while none stands. */
function useNamesLine(namesFailure: NamesFailure | undefined): readonly FailedRead[] {
  return useMemo(
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
 * Whether every read the tree is built from holds its answer (`readsHeld`): the tree read, which
 * answers every node (`blocksHeld`), and the six reads here, read under the keys `useIssueTree`
 * reads them by, so they cost no request of their own; no document's blocks are read. A read that
 * stands failed holds nothing either (`standsFailed`): the page cannot say what it would have
 * held, so the outline says that the read failed (`failed`). One whose refresh failed holds the
 * answer it had, which its rows are drawn from, and says nothing. The names' line stands while
 * the one names read's count of failures is above 0 (`IssueTreeView.namesFailure`): from a
 * failure while it holds no answer under its set of addresses until it holds one, so it stays
 * while its read under a new set is out - the tree draws the names an older set's answer gave
 * meanwhile. Its Retry asks that read again, and no other read, and the line is busy while it
 * runs (`retrying`). Whether the names are held is the tree's own answer
 * (`IssueTreeView.namesHeld`). The conversation's reads are kept too, which tell a turn the
 * outline leaves out (`useAddressFallback`).
 */
export function useOutlineReads(issue: IssueDetails): OutlineReads {
  // The tree is the page's one, its owner's where one stands above (`useOwnedIssueTree`). The
  // tree read's `pending` and `failed` are `useIssueTree`'s, under its key, so they cost no
  // request of their own: until it settles a node still out can draw rows above the row the page
  // would land on.
  const {
    blocksHeld,
    blocksOf,
    failed: treeFailed,
    namesFailure,
    namesHeld,
    pending,
    published,
    root,
  } = useOwnedIssueTree(issue);
  const key = issue.key;
  const six = sixReads(key);
  const own = useQueries({
    combine: (
      answers
    ): {
      coverageOut: boolean;
      failed: readonly FailedRead[];
      placed: boolean;
      reads: OutlineReads["reads"];
      settled: boolean;
    } => {
      const [coverage, decisions, , asks, comments, messages] = answers;
      return {
        coverageOut: coverage.isPending && coverage.errorUpdateCount === 0,
        failed: failedLines(answers, (index) => ({
          bringsQuestions: six[index].bringsQuestions,
          key: six[index].line,
          message: six[index].message,
        })),
        placed: [decisions, asks, comments].every((answer) => !answer.isPending),
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
  // The reader's login, under the key and with the freshness the tree reads it by
  // (`useIssueTree`), so it costs no request of its own: it says which open question waits on the
  // reader, so the first drawing of the rows waits on it (`placesSettled`).
  const viewerOut = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).isPending;
  const names = useNamesLine(namesFailure);
  const failed = useMemo(
    () => (own.failed.length + names.length === 0 ? NO_FAILED_READS : [...own.failed, ...names]),
    [names, own.failed]
  );
  // The tree read's line while the read stands refused, which `TreeReadFailed` draws over the
  // outline: rows come from it (`OutlineReads.refused`).
  const refused = useMemo(
    (): readonly RefusedLine[] =>
      treeFailed ? [...failed, { key: TREE_READ_LINE, placesRows: true }] : failed,
    [failed, treeFailed]
  );
  return {
    blocksOf,
    coverageOut: own.coverageOut,
    failed,
    names,
    namesHeld,
    placesSettled: own.placed && !pending && !viewerOut,
    published,
    reads: own.reads,
    readsHeld: own.reads.held && blocksHeld,
    readsSettled: own.settled && !pending,
    refused,
    root,
  };
}
