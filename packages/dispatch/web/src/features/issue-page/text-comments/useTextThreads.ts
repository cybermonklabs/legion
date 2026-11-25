import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import {
  issueCoverageQuery,
  issueQuery,
  issueResultsQuery,
  TREE_FILTERS,
  whoAmIQuery,
} from "../../../api/queries";
import type { IssueDetails } from "../../../api/types";
import { issueDecisionsQuery } from "../../doc/spec-marks";
import { parseIssuePath } from "../../refs/routes";
import { issueAddress } from "../issue-address";
import type { IssueTreeReads } from "../issue-tree";
import type { Preset } from "../page-words";
import { treeIdOf } from "../tree-read/tree-nodes";
import {
  outlineOf,
  outlineQuery,
  type TreeAnswer,
  type TreeAnswerNode,
  viewQuery,
} from "../tree-read/tree-read";
import { issueAskThreadsQuery, issueCommentsQuery, issueMessagesQuery } from "../useIssueTree";
import { type NodeThreads, textThreadsOf } from "./text-threads";

/**
 * The threads on text of the view the stage draws (R2, R9): one `textThreadsOf` answer of the
 * read that view holds, which the column (`TextMargin.tsx`) draws, and each row site's sheet
 * below 1024 px with it (`NodeThreads.tsx`). Neither sends a read for it: every read here is
 * watched by an observer that is never enabled, as `useIssueTree` watches each view and
 * `ViewReadProvider` watches Conversation's read, so a load with threads sends the requests of a
 * load without them (c-reads). The reads are those the page already holds, under the keys their
 * own readers read them by: the view's tree read - Conversation's beside Conversation and
 * Decisions, Graph's in Graph, the filter's view read in Spec, Verification and Plan
 * (`outlineOf`, `viewQuery`) - and the comments, messages, asks, coverage, decisions and results
 * reads and the reader's login, which `useIssueTree` reads (`IssueTreeReads`).
 *
 * The answer is built once for one set of answers, whoever asks (`builtThreads`), so the column
 * and every sheet read one value, which keeps its identity while no read answers anew.
 */

/** The threads of the view, with what they were built from. */
export interface ViewThreads {
  /** The tree read the view holds, undefined while it holds none. */
  readonly answer: TreeAnswer | undefined;
  /** Each node of that answer by the tree id the view draws it by (part T's `treeIdOf`): the
   *  first node of each id. */
  readonly nodes: ReadonlyMap<string, TreeAnswerNode>;
  /** The threads of each node, by the node's tree id (`textThreadsOf`). */
  readonly threads: ReadonlyMap<string, NodeThreads>;
}

/** A view that holds no tree read, or whose read has not answered. */
const NO_VIEW_THREADS: ViewThreads = { answer: undefined, nodes: new Map(), threads: new Map() };

/** The reads the threads are built from beside the answer, each compared by identity. */
const READ_NAMES = [
  "asks",
  "comments",
  "coverage",
  "decisions",
  "issue",
  "messages",
  "results",
  "viewer",
] as const satisfies readonly (keyof IssueTreeReads)[];

/** How many answers are kept for one tree read's answer: as many as `useIssueTree` keeps trees. */
const BUILT_PER_ANSWER = 3;

/** The threads built for each answer of a tree read, newest last, each with its reads. */
const builtThreads = new WeakMap<
  TreeAnswer,
  readonly { readonly reads: IssueTreeReads; readonly view: ViewThreads }[]
>();

/**
 * The threads of `answer` over `reads`, built once for one set of reads: the column and every
 * sheet that asks with the same answer and the same reads, each the same object, get the value
 * built first. The values are held by the answer, so they go with it.
 */
export function viewThreadsOf(answer: TreeAnswer, reads: IssueTreeReads): ViewThreads {
  const held = builtThreads.get(answer) ?? [];
  const found = held.find((one) => READ_NAMES.every((name) => one.reads[name] === reads[name]));
  if (found !== undefined) return found.view;
  const idOf = treeIdOf(answer, reads);
  const nodes = new Map<string, TreeAnswerNode>();
  for (const node of answer.nodes ?? []) {
    const id = idOf(node.id);
    if (id !== reads.issue.key && !nodes.has(id)) nodes.set(id, node);
  }
  // Each node the view's read holds but the issue is one the view draws: a thread stands by the
  // node its words are in where the read holds it, else by the node the read hangs it under.
  const view: ViewThreads = {
    answer,
    nodes,
    threads: textThreadsOf(answer, reads, new Set(nodes.keys())),
  };
  builtThreads.set(answer, [...held.slice(1 - BUILT_PER_ANSWER), { reads, view }]);
  return view;
}

/**
 * The reads the threads of `issue` are built from beside a tree read's answer, each watched by an
 * observer that is never enabled (no request of its own): the asks, comments, coverage, decisions,
 * messages and results reads and the reader's login, which `useIssueTree` reads. One value while
 * no read answers anew; undefined while no issue is given, as before the page's issue read has
 * answered.
 */
export function useThreadReads(issue: IssueDetails | undefined): IssueTreeReads | undefined {
  const key = issue?.key ?? "";
  const asks = useQuery({ ...issueAskThreadsQuery(key), enabled: false }).data;
  const comments = useQuery({ ...issueCommentsQuery(key), enabled: false }).data;
  const coverage = useQuery({ ...issueCoverageQuery(key), enabled: false }).data;
  const decisions = useQuery({ ...issueDecisionsQuery(key, true), enabled: false }).data;
  const messages = useQuery({ ...issueMessagesQuery(key), enabled: false }).data;
  const results = useQuery({ ...issueResultsQuery(key), enabled: false }).data;
  const viewer = useQuery({ ...whoAmIQuery(), enabled: false }).data?.login;
  return useMemo(
    () =>
      issue === undefined
        ? undefined
        : { asks, comments, coverage, decisions, issue, messages, results, viewer },
    [asks, comments, coverage, decisions, issue, messages, results, viewer]
  );
}

/** The threads on text of the view `preset` of `issue` draws, from the reads the page holds. */
export function useTextThreads(issue: IssueDetails, preset: Preset): ViewThreads {
  const key = issue.key;
  const filter = TREE_FILTERS.find((one) => one === preset);
  const outline = outlineOf(preset);
  const outlineAnswer = useQuery({
    ...outlineQuery(key, outline ?? "conversation"),
    enabled: false,
  }).data;
  const viewAnswer = useQuery({
    ...viewQuery(key, filter ?? TREE_FILTERS[0]),
    enabled: false,
  }).data;
  const answer =
    filter !== undefined ? viewAnswer : outline !== undefined ? outlineAnswer : undefined;
  const reads = useThreadReads(issue);
  return useMemo(
    () =>
      answer === undefined || reads === undefined ? NO_VIEW_THREADS : viewThreadsOf(answer, reads),
    [answer, reads]
  );
}

/**
 * The issue the router is at, as the page reads it, and the view its address opens: what the
 * control (`SelectionComment.tsx`) and each node's sheet (`NodeThreads.tsx`), which are handed no
 * issue, read the view's threads by. The issue's read is the page's own (`IssueOutlinePage`),
 * which holds its answer before the stage draws a row, so this observer sends no request of its
 * own there; where nothing has read it - a part of the page drawn alone - it reads it once.
 * Undefined off an issue's address and until the read answers. With `read` false it only watches
 * the cache and sends no request, where nothing has read the issue: what the address target
 * (`thread-address.ts`) does, since every address of the page asks for it.
 */
export function useStageIssue(
  read = true
): { readonly issue: IssueDetails; readonly preset: Preset } | undefined {
  const { hash, pathname, search } = useLocation();
  const route = parseIssuePath(pathname, search);
  const issue = useQuery({
    ...issueQuery(route?.key),
    enabled: read && route !== undefined,
    refetchOnMount: false,
  }).data;
  if (route === undefined || issue === undefined) return undefined;
  return { issue, preset: issueAddress(route, search, hash, primarySpec(issue)?.slug).preset };
}
