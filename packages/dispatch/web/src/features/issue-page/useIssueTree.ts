import { keepPreviousData, useQueries, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import {
  issueCoverageQuery,
  issueResultsQuery,
  referenceTargetsQuery,
  whoAmIQuery,
} from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import {
  ambiguousTargets,
  buildIssueTree,
  type IssueTreeReads,
  nodeWords,
  type TreeNode,
  type TreeViews,
  targetNames,
} from "./issue-tree";
import { usePublishReferenceNames } from "./reference-names";
import { citedReferences } from "./row-references";
import {
  REFRESHED_BY_EVENTS,
  TREE_VIEWS,
  type TreeAnswer,
  type TreeRead,
  useTreeRead,
  viewQuery,
} from "./tree-read/tree-read";

/** The issue's tree, and how far the tree read it is built from has got (`TreeRead`): `root` is
 *  built from what that read holds, and keeps standing on it through a refetch that fails. */
export interface IssueTreeView extends Pick<TreeRead, "blocksHeld" | "blocksOf" | "blocksRead"> {
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

/**
 * What every read of the tree takes: it is kept fresh by events, never by its age. The page's
 * stage mounts the tree's slot again on every press of a view, so under the app's 30 s
 * `staleTime` (`main.tsx`) a press more than 30 s after the load would read every one of the
 * tree's reads again, though nothing they hold has changed.
 * The feed's rules (`api/event-refresh.ts`) refresh each read when an event can change it; among
 * them, `["comments", key]` on a comment event and on a document's upload or new version;
 * `["asks", key]` on an ask event, a reply on an ask, a review of a document and a block
 * repaired or found invalid; `["issue-messages", key]` on a message event;
 * `["issue", key, "coverage"]` on a document's upload or new version; `["issue", key, "results"]`
 * on those too, on a result recorded or retracted and on a pull request moved to another issue;
 * and the names reads
 * (`["reference-targets", …]`, by that prefix) on a comment's edit, an ask's edit and a
 * document's new version (`referenceNameTargets`). A names read's key is the set
 * of addresses it names, so a tree that comes to hold another set reads again under a key of its
 * own, and those three events are what change a target's words under an address the tree
 * already names: a cited comment edited, a cited ask's question edited, a block of a document's
 * new version. No rule there names `["whoami"]`: the reader's login does not change while the
 * page is open. A gap in the feed refreshes every read the app holds (`refreshQueries`), these
 * among them, and window focus fetches again only a read in error (`refetchFailedOnFocus`). The
 * tree read and its views keep the same rule but for a focus, which reads them again only while
 * the live feed is down (`TREE_FRESH`), since their events already reach them while it is up.
 */
const EVENT_FRESH = REFRESHED_BY_EVENTS;

/** The reads a tree is built from (`IssueTreeReads`), each compared by identity. */
const TREE_INPUTS = [
  "asks",
  "coverage",
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
 * words every node; no document's blocks are read. Every decision is one of its nodes, with its
 * review state, and a person's comment the read hangs under a decision is a change they asked of
 * it (`decision-review.ts`), so no decisions read is made. Beside it stand the reads that tell
 * what the answer does not, each under the key another surface of this issue reads too, so
 * opening Details, the history or the Contents pane after the tree finds them answered rather
 * than fetching again: the coverage, the results, the issue's asks, its comments, its messages,
 * and the reader's login, which says which open questions wait on the reader
 * (`IssueTreeReads.viewer`).
 *
 * Each filter's view read (`viewQuery`), the decisions view among them,
 * is watched here and never sent: `ViewReadProvider`
 * (`tree-read/view-read.tsx`) reads a view the first time its filter is on, and whatever it holds
 * is set on the issue's node (`TreeNode.views`), which `presetView` and `filterSignals` draw a
 * filter from, and the Decisions filter its decisions (`decisions-entries.ts`). The root
 * keeps its identity while neither the tree nor a held view changes.
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
 * after the first the names already in hand stand while the new set loads.
 */
export function useIssueTree(issue: IssueDetails): IssueTreeView {
  const key = issue.key;
  const read = useTreeRead(issue);
  // Every read here takes `EVENT_FRESH`: events refresh it, and a slot mounted again reads
  // nothing.
  const coverage = useQuery({ ...issueCoverageQuery(key), ...EVENT_FRESH });
  const results = useQuery({ ...issueResultsQuery(key), ...EVENT_FRESH });
  const asks = useQuery({
    queryKey: ["asks", key],
    queryFn: () => api.listIssueAsks(key, "all"),
    ...EVENT_FRESH,
  });
  // Under the keys the margin, the history and the focused row's facts read too, so a reader
  // opening the panel after the tree finds them answered rather than read a second time. The
  // tree read answers each comment as a node, so the comments build no part of the tree; the
  // read is held here so that a comment written, a Change among them, reads the tree again
  // (`subscribeTreeFreshness`).
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
  // and holds whatever `ViewReadProvider` read for the load; the answers in `TREE_VIEWS`' order.
  const [specView, verificationView, planView, decisionsView] = useQueries({
    queries: TREE_VIEWS.map((view) => ({ ...viewQuery(key, view), enabled: false })),
  });
  const spec = specView?.data;
  const verification = verificationView?.data;
  const plan = planView?.data;
  const decisions = decisionsView?.data;
  const views = useMemo((): TreeViews | undefined => {
    const held: TreeViews = {
      ...(decisions === undefined ? {} : { decisions }),
      ...(plan === undefined ? {} : { plan }),
      ...(spec === undefined ? {} : { spec }),
      ...(verification === undefined ? {} : { verification }),
    };
    return Object.keys(held).length === 0 ? undefined : held;
  }, [decisions, plan, spec, verification]);
  // One value, so the model is given the reads and nothing else.
  const reads = useMemo<IssueTreeReads>(
    () => ({
      asks: asks.data,
      coverage: coverage.data,
      issue,
      results: results.data,
      tree: read.answer,
      viewer,
    }),
    [asks.data, coverage.data, issue, read.answer, results.data, viewer]
  );
  const unnamed = useMemo(() => builtTree(reads), [reads]);
  const refs = useMemo(() => treeReferences(unnamed), [unnamed]);
  // One request for every name, not one per read that landed. Each of these reads adds to the
  // tree as it arrives - the tree read every node, the asks an answer - so asking while any is
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
    !messages.isPending &&
    !results.isPending &&
    !who.isPending;
  // The last answer stands while the next one loads, because the key is the set of addresses
  // itself: one comment, message, ask or block arriving changes that set, and without this the
  // whole page would fall back to no names at all - every chip with its raw address - until the
  // new read lands. Merging the two answers by `ref` would say no more than this does: the new
  // read names every address the tree now holds, so the only names it lacks are the ones the
  // previous answer carries, which is what is kept here. A name that moved is a stale word for as
  // long as one read, where a dropped one is the whole page's names at once.
  const targets = useQuery({
    ...referenceTargetsQuery(answered ? refs : []),
    ...EVENT_FRESH,
    placeholderData: keepPreviousData,
  });
  const first = useMemo(
    () => (targets.data === undefined ? undefined : (targets.data.targets ?? [])),
    [targets.data]
  );
  const firstTree = useMemo(
    () => (first === undefined ? unnamed : builtTree({ ...reads, targets: first })),
    [first, reads, unnamed]
  );
  // What the named tree holds that the first read was not given: a block's stored source comes
  // from that read's answer, so a whole document it cites is known only now (above). An empty
  // set is no request, and the names already in hand stand while a new non-empty set loads, as
  // above.
  const extra = useMemo(() => {
    if (first === undefined) return [];
    const asked = new Set(refs);
    return treeReferences(firstTree).filter((reference) => !asked.has(reference));
  }, [first, firstTree, refs]);
  const more = useQuery({
    ...referenceTargetsQuery(extra),
    ...EVENT_FRESH,
    placeholderData: keepPreviousData,
  });
  // Both answers, the second's standing for an address both name. Where nothing is left for the
  // second read to name, it has no answer: its query is disabled and would hold its last answer
  // as placeholder for good, over the first read's newer names for the same addresses.
  const named = useMemo(() => {
    const added = extra.length === 0 ? [] : (more.data?.targets ?? []);
    if (first === undefined || added.length === 0) return first;
    return [...new Map([...first, ...added].map((target) => [target.ref, target])).values()];
  }, [extra, first, more.data]);
  // The same answer every link of this page would otherwise read one pointer at a time, which a
  // link in a document, in a decision's card or in a comment's preview shows, and a row's
  // in-place chip too (`reference-names.tsx`, R20). Every one of those draws the name it is
  // given as it is, so the name published for a target is the name a chip of the tree draws
  // for it, by the one rule both read (`targetNames`): a block's drawn first line, a whole
  // document's word on the page, a question's or a comment's words with no markup. Beside the
  // names go the references the read answered ambiguous (`ambiguousTargets`), by that same
  // rule: a short reference two targets carry has no name, as a gone one has none, and a link
  // draws it as written and links nothing where it reads a gone one's own, which the names alone
  // would not tell it. A chip's name is read from the issue, the tree read and the names alone
  // (`treeContext`), so the map and the set are built from those and nothing else: an answer of
  // any other read - an ask, a comment, the coverage, the reader's login - publishes nothing new,
  // which would draw every link beside the tree again. Until the names answer there is nothing to
  // publish.
  const names = useMemo(
    () =>
      named === undefined
        ? NO_TARGET_NAMES
        : targetNames({ issue, targets: named, tree: read.answer }),
    [issue, named, read.answer]
  );
  const ambiguous = useMemo(
    () =>
      named === undefined
        ? NO_AMBIGUOUS_TARGETS
        : ambiguousTargets({ issue, targets: named, tree: read.answer }),
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
      root,
    }),
    [read.blocksHeld, read.blocksOf, read.blocksRead, root]
  );
}
