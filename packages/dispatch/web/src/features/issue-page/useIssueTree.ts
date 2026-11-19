import { queryOptions, useQueries, useQuery } from "@tanstack/react-query";
import { useMemo, useRef } from "react";

import { api } from "../../api/client";
import {
  issueCoverageQuery,
  issueResultsQuery,
  referenceTargetsQuery,
  whoAmIQuery,
} from "../../api/queries";
import type { IssueDetails, ReferenceTarget } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import {
  buildIssueTree,
  type IssueTreeReads,
  nodeWords,
  type TreeNode,
  type TreeViews,
  targetNaming,
} from "./issue-tree";
import { usePublishReferenceNames } from "./reference-names";
import { citedReferences } from "./row-references";
import {
  REFRESHED_BY_EVENTS,
  TREE_FILTERS,
  type TreeAnswer,
  type TreeRead,
  useTreeRead,
  viewQuery,
} from "./tree-read/tree-read";

/** The issue's tree, and how far the tree read it is built from has got (`TreeRead`): `root` is
 *  built from what that read holds, and keeps standing on it through a refetch that fails. */
export interface IssueTreeView extends Pick<TreeRead, "blocksHeld" | "blocksOf" | "blocksRead"> {
  /** Whether a read of the tree's names - the batched one, or the one more that names what a
   *  block's stored source cites (`useIssueTree`) - stands in error: its newest attempt failed,
   *  under a new set of addresses or as a refetch of the set it had answered. It is what a view
   *  reads to draw the page's line that the names failed. The names in hand stand meanwhile -
   *  the tree and what the page publishes are built from the last answer each read gave - so a
   *  view keeps every row's words and every chip's name beside that line; a first read that
   *  fails holds no names, and turns it true all the same. It is false while a read is out that
   *  has not failed yet, and once both reads have answered with success. */
  readonly namesFailed: boolean;
  readonly root: TreeNode;
}

/**
 * Every `dispatch://` address the tree holds: each node's own, so a block's words can be read,
 * each chip's, so a chip can name its target, and each one a node's words or its answer cite, so
 * a row links what its words name from this read and makes none per mention
 * (`row-references.ts`). This is what the batched reference read is given, which is why the
 * page draws every chip and every link in one request rather than one per node (R21); a block
 * whose words come from that read's own answer cites what nothing else cites, which one more
 * read names (`useIssueTree`). It is here, beside the reads it is the key of, and not beside
 * the model, which makes no read; what a text cites is read by `citedReferences`
 * (`row-references.ts`), over the one reader of a text's citations (`text-references.ts`).
 */
function treeReferences(root: TreeNode): string[] {
  const refs = new Set<string>();
  const walk = (node: TreeNode): void => {
    if (node.reference !== undefined) refs.add(node.reference);
    for (const chip of [...node.chips, ...node.alsoUnder]) refs.add(chip.ref);
    for (const reference of citedReferences(nodeWords(node))) refs.add(reference);
    for (const reference of citedReferences(node.answer?.text ?? "")) refs.add(reference);
    for (const child of node.children) walk(child);
  };
  walk(root);
  return [...refs];
}

/** The names the page publishes until the batched name read answers: none, and the same empty
 *  map on every render, so no link beside the tree draws again for a read that names nothing. */
const NO_TARGET_NAMES: ReadonlyMap<string, string> = new Map();

/** The references the page publishes as ambiguous until the batched name read answers: none,
 *  and the same empty set on every render, as `NO_TARGET_NAMES` is the same empty map. */
const NO_AMBIGUOUS_TARGETS: ReadonlySet<string> = new Set();

/** What the one more names read holds where nothing is left for it to name: no names, and the
 *  same empty answer on every render, so the answer it gave before stands over nothing. */
const NO_MORE_TARGETS: { readonly targets: readonly ReferenceTarget[] } = { targets: [] };

/**
 * The answer a names read gave last: its own while it holds one, and the last one it held while
 * its read under a new set of addresses is out or has failed. TanStack's `placeholderData` would
 * hold that answer only while the read is out: a read under a new key that fails holds no data,
 * where a refetch that fails under the same key keeps its own. The ref is written during render
 * only with the answer the read holds in that render, so it never stands over a newer one: a read
 * that holds an answer is drawn from it.
 */
function useLastAnswer<T>(answer: T | undefined): T | undefined {
  const last = useRef(answer);
  if (answer !== undefined) last.current = answer;
  return last.current;
}

/** The issue's asks as the tree reads them: every ask, open or closed (`listIssueAsks` with
 *  `all`), under the key the margin, a document's ask blocks and the feed's ask rules read and
 *  refresh (`["asks", key]`). One definition, so a view that observes the same read takes the
 *  key from here and cannot drift from it; such a view, like the tree, adds how it is kept fresh
 *  (`EVENT_FRESH`). */
export const issueAsksQuery = (key: string) =>
  queryOptions({
    queryKey: ["asks", key],
    queryFn: () => api.listIssueAsks(key, "all"),
  });

/**
 * What every read of the tree takes: it is kept fresh by events, never by its age. The page's
 * stage mounts the tree's slot again on every press of a view, so under the app's 30 s
 * `staleTime` (`main.tsx`) a press more than 30 s after the load would read every one of the
 * tree's reads again, though nothing they hold has changed.
 * The feed's rules (`api/event-refresh.ts`) refresh each read when an event can change it; among
 * them, `["comments", key]` on a comment event and on a document's upload or new version;
 * `["asks", key]` on an ask event, a reply on an ask, a review of a document and a block
 * repaired or found invalid; `["decisions", key]` on an ask event, a Keep and a document's new
 * version; `["issue-messages", key]` on a message event; `["issue", key, "coverage"]` on a
 * document's upload or new version; `["issue", key, "results"]` on those too, on a result
 * recorded or retracted and on a pull request moved to another issue; and a names read
 * (`["reference-targets", …]`) on an event that changes the words or the name under an address
 * its key holds, on the issue or project document that address is of: a comment's edit, an
 * ask's edit, a document's new version, rename or removal, and an issue's write that changes its
 * title (`referenceNameTargets`). A names read's key is the set of addresses it names, so a tree
 * that comes to hold another set reads again under a key of its own, and only a change under an
 * address the tree already names needs a rule. No rule there names `["whoami"]`: the reader's
 * login does not change while the page is open. A gap in the feed refreshes every read the app
 * holds (`refreshQueries`), these among them, and window focus fetches again only a read in
 * error (`refetchFailedOnFocus`). The tree read and its views keep the same rule but for a
 * focus, which reads them again only while the live feed is down (`TREE_FRESH`), since their
 * events already reach them while it is up.
 */
const EVENT_FRESH = REFRESHED_BY_EVENTS;

/** The reads a tree is built from (`IssueTreeReads`), each compared by identity. */
const TREE_INPUTS = [
  "asks",
  "comments",
  "coverage",
  "decisions",
  "issue",
  "results",
  "targets",
  "viewer",
] as const satisfies readonly (keyof IssueTreeReads)[];

/** How many trees are kept for one answer of the tree read: the tree before the names answer,
 *  with the first read's names and with both reads' names, and one more. */
const TREES_PER_ANSWER = 4;

/** The trees built for each answer of the tree read, newest last, each with its reads. */
const builtTrees = new WeakMap<
  TreeAnswer,
  readonly { readonly reads: IssueTreeReads; readonly root: TreeNode }[]
>();

/**
 * `buildIssueTree` of `reads`, built once for one set of reads: a tree already built from the
 * same answer of the tree read and the same other reads, each the same object, is that tree.
 * Every read keeps its answer's object until it answers anew, so a slot the stage mounts again
 * on the same reads - the outline drawn again as a view is pressed off - builds nothing, and a
 * read that changed builds a tree of its own. The trees are held by the answer, so they go with
 * it.
 */
function builtTree(reads: IssueTreeReads): TreeNode {
  const answer = reads.tree;
  if (answer === undefined) return buildIssueTree(reads);
  const held = builtTrees.get(answer) ?? [];
  const found = held.find((one) => TREE_INPUTS.every((input) => one.reads[input] === reads[input]));
  if (found !== undefined) return found.root;
  const root = buildIssueTree(reads);
  builtTrees.set(answer, [...held.slice(1 - TREES_PER_ANSWER), { reads, root }]);
  return root;
}

/**
 * The issue's tree, from the page's reads (root R20). This hook is where the reads are;
 * `buildIssueTree` is pure and holds none of them, so the model is testable without a server and
 * the page makes no request per node.
 *
 * The tree is the issue's tree read (`useTreeRead`), every page of it, which places, orders and
 * words every node; no document's blocks are read. Beside it stand the reads that tell what the
 * answer does not, each under the key another surface of this issue reads too, so opening
 * Details, the history or the Contents pane after the tree finds them answered rather than
 * fetching again: the coverage, the decisions, the results, the issue's asks, its comments -
 * which also say which decision of the decisions read a person asked to change
 * (`decision-review.ts`) - its messages, and the reader's login, which says which open questions
 * wait on the reader (`IssueTreeReads.viewer`).
 *
 * Each filter's view read (`viewQuery`) is watched here and never sent: `ViewReadProvider`
 * (`tree-read/view-read.tsx`) reads a view the first time its filter is on, and whatever it holds,
 * with the decisions read, is set on the issue's node (`TreeNode.views`), which `presetView` and
 * `filterSignals` draw a filter from (R4). The root keeps its identity while neither the tree nor
 * a held view changes.
 *
 * The names come last and all at once: the tree is built, every address it holds is read in one
 * batched request (`referenceTargetsQuery`; a tree holding more addresses than the server reads
 * at once costs one request per batch of them, which no ordinary issue reaches), and the tree is
 * built again with the names in it. A block's stored source, which that read names it by, may
 * cite a whole document of the issue no word of the tree cites, which a chip on the block opens
 * (`tree-drafts.ts`'s `addDocumentChips`), so that address is in no address the read was given:
 * one more read names those addresses alone, made only where a source cites one, and the tree is
 * built with both answers. That is the last read of a round: the second's answer adds no chip.
 * Until the names answer the tree draws, with a chip naming its target's address; on every read
 * after the first the names already in hand stand while the new set loads, and after its read
 * fails (`IssueTreeView.namesFailed`).
 */
export function useIssueTree(issue: IssueDetails): IssueTreeView {
  const key = issue.key;
  const read = useTreeRead(issue);
  // Every read here takes `EVENT_FRESH`: events refresh it, and a slot mounted again reads
  // nothing.
  const coverage = useQuery({ ...issueCoverageQuery(key), ...EVENT_FRESH });
  const decisions = useQuery({ ...issueDecisionsQuery(key, true), ...EVENT_FRESH });
  const results = useQuery({ ...issueResultsQuery(key), ...EVENT_FRESH });
  const asks = useQuery({ ...issueAsksQuery(key), ...EVENT_FRESH });
  // Under the keys the margin, the history and the focused row's facts read too, so a reader
  // opening the panel after the tree finds them answered rather than read a second time.
  const comments = useQuery({
    queryKey: ["comments", key],
    queryFn: () => api.listComments(key),
    ...EVENT_FRESH,
  });
  // Under the key the history and the margin read too, and one of the reads the names wait on
  // (`answered`); the model reads no message from it, since the tree read answers each as a node.
  const messages = useQuery({
    queryKey: ["issue-messages", key],
    queryFn: () => api.listIssueMessages(key),
    ...EVENT_FRESH,
  });
  // The reader, whose own turn an open question may be (`IssueTreeReads.viewer`): the shell's
  // one read of who is signed in, so it costs no request of its own.
  const who = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH });
  const viewer = who.data?.login;
  // Each filter's view read, watched by an observer that is never enabled, so it sends nothing
  // and holds whatever `ViewReadProvider` read for the load; the answers in `TREE_FILTERS`' order.
  const [specView, verificationView, planView] = useQueries({
    queries: TREE_FILTERS.map((filter) => ({ ...viewQuery(key, filter), enabled: false })),
  });
  const spec = specView?.data;
  const verification = verificationView?.data;
  const plan = planView?.data;
  const views = useMemo((): TreeViews | undefined => {
    const held: TreeViews = {
      ...(decisions.data === undefined ? {} : { decisions: decisions.data }),
      ...(plan === undefined ? {} : { plan }),
      ...(spec === undefined ? {} : { spec }),
      ...(verification === undefined ? {} : { verification }),
    };
    return Object.keys(held).length === 0 ? undefined : held;
  }, [decisions.data, plan, spec, verification]);
  // One value, so the model is given the reads and nothing else.
  const reads = useMemo<IssueTreeReads>(
    () => ({
      asks: asks.data,
      comments: comments.data,
      coverage: coverage.data,
      decisions: decisions.data,
      issue,
      results: results.data,
      tree: read.answer,
      viewer,
    }),
    [
      asks.data,
      comments.data,
      coverage.data,
      decisions.data,
      issue,
      read.answer,
      results.data,
      viewer,
    ]
  );
  const unnamed = useMemo(() => builtTree(reads), [reads]);
  const refs = useMemo(() => treeReferences(unnamed), [unnamed]);
  // One request for every name, not one per read that landed. Each of these reads adds to the
  // tree as it arrives - the tree read every node, the decisions read a decision no node holds,
  // the asks an answer, the comments a Change moved under its decision - so asking while any is
  // still out would ask for the addresses known so far and ask again for the rest: measured on
  // this pod, that cost the first load two reads and sometimes three, where the design is "the
  // names come last and all at once" (above) and the first load's budget is 40 requests. The
  // reader's login is one of the reads this gate waits on: it adds no node, but the tree is
  // built again when it answers, since it says which questions wait on the reader. Every one of
  // these reads is always enabled, so each ends in an answer or an error and none of them holds
  // the names for good. Nothing waits on the names: until they land the tree draws with each chip
  // naming its target's address.
  const answered =
    !read.pending &&
    !asks.isPending &&
    !comments.isPending &&
    !coverage.isPending &&
    !decisions.isPending &&
    !messages.isPending &&
    !results.isPending &&
    !who.isPending;
  // The last answer stands while the next one loads and after it fails, because the key is the
  // set of addresses itself: one comment, message, ask or block arriving changes that set, and
  // without this the whole page would fall back to no names at all - every chip with its raw
  // address - until the new read lands, and for as long as it keeps failing (`useLastAnswer`).
  // Merging the two answers by `ref` would say no more than this does: the new read names every
  // address the tree now holds, so the only names it lacks are the ones the previous answer
  // carries, which is what is kept here. A name that moved is a stale word until a read under the
  // new key succeeds, where a dropped one is the whole page's names at once; while that read
  // stands failed - until window focus or an event reads it again (`EVENT_FRESH`) - the view says
  // so (`IssueTreeView.namesFailed`).
  const targets = useQuery({
    ...referenceTargetsQuery(answered ? refs : []),
    ...EVENT_FRESH,
  });
  const firstAnswer = useLastAnswer(targets.data);
  const first = useMemo(
    () => (firstAnswer === undefined ? undefined : (firstAnswer.targets ?? [])),
    [firstAnswer]
  );
  const firstTree = useMemo(
    () => (first === undefined ? unnamed : builtTree({ ...reads, targets: first })),
    [first, reads, unnamed]
  );
  // What the named tree holds that the first read was not given: a block's stored source comes
  // from that read's answer, so a whole document it cites is known only now (above). An empty
  // set is no request, and the names already in hand stand while a new non-empty set loads and
  // after its read fails, as above.
  const extra = useMemo(() => {
    if (first === undefined) return [];
    const asked = new Set(refs);
    return treeReferences(firstTree).filter((reference) => !asked.has(reference));
  }, [first, firstTree, refs]);
  const more = useQuery({
    ...referenceTargetsQuery(extra),
    ...EVENT_FRESH,
  });
  // Both answers, the second's standing for an address both name that it is asked for now
  // (`extra`), and the first's for every other: an answer the second gave for an older set,
  // kept while its new set loads or after that read fails, may name an address the first read
  // has taken over since, whose name it holds from before. Where nothing is left for the second
  // read to name, the answer it gave before is dropped: its query is disabled and never answers
  // again, so that answer would have the page publish for good a name for an address no read
  // asks for any more. A link beside the tree draws a published name in place of the one it
  // reads of its own (`useKnownReferenceName`), and the feed refreshes a names read only for an
  // address its key holds (`referenceNameTargets`), so no event would ever move that name.
  const moreAnswer = useLastAnswer(extra.length === 0 ? NO_MORE_TARGETS : more.data);
  const named = useMemo(() => {
    const added = moreAnswer?.targets ?? [];
    if (first === undefined || added.length === 0) return first;
    const asked = new Set(extra);
    const ordered = [
      ...added.filter((target) => !asked.has(target.ref)),
      ...first,
      ...added.filter((target) => asked.has(target.ref)),
    ];
    return [...new Map(ordered.map((target) => [target.ref, target])).values()];
  }, [extra, first, moreAnswer]);
  // A failure of either read, which the names in hand stand through (above). The second read
  // fails only where something is left for it to name: where nothing is, its query is disabled.
  const namesFailed = targets.isError || more.isError;
  // The same answer every link of this page would otherwise read one pointer at a time, which a
  // link in a document, in a decision's card or in a comment's preview shows, and a row's
  // in-place chip too (`reference-names.tsx`, R20). Every one of those draws the name it is
  // given as it is, so the name published for a target is the name a chip of the tree draws
  // for it, by the one rule both read (`targetNaming`): a block's drawn first line, a whole
  // document's word on the page, a question's or a comment's words with no markup. Beside the
  // names go the references the read answered ambiguous, read with them by that same rule: a
  // short reference two targets carry has no name, as a gone one has none, and a link draws it
  // as written and links nothing where it reads a gone one's own, which the names alone would
  // not tell it. A chip's name is read from the issue, the tree read and the names alone
  // (`treeContext`), so the map and the set are built from those and nothing else: an answer of
  // any other read - an ask, a comment, the coverage, the reader's login - publishes nothing
  // new, which would draw every link beside the tree again. Until the names answer there is
  // nothing to publish.
  const { ambiguous, names } = useMemo(
    () =>
      named === undefined
        ? { ambiguous: NO_AMBIGUOUS_TARGETS, names: NO_TARGET_NAMES }
        : targetNaming({ issue, targets: named, tree: read.answer }),
    [issue, named, read.answer]
  );
  usePublishReferenceNames(names, ambiguous);
  const built = useMemo(
    () =>
      named === undefined || named === first ? firstTree : builtTree({ ...reads, targets: named }),
    [first, firstTree, named, reads]
  );
  const root = useMemo(() => (views === undefined ? built : { ...built, views }), [built, views]);
  return useMemo(
    () => ({
      blocksHeld: read.blocksHeld,
      blocksOf: read.blocksOf,
      blocksRead: read.blocksRead,
      namesFailed,
      root,
    }),
    [read.blocksHeld, read.blocksOf, read.blocksRead, namesFailed, root]
  );
}
