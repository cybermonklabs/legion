import { hashKey, queryOptions, useQueries, useQuery } from "@tanstack/react-query";
import { useMemo, useRef } from "react";

import { api } from "../../api/client";
import {
  issueAskThreadsQuery,
  issueCoverageQuery,
  issueResultsQuery,
  referenceTargetsQuery,
  TREE_FILTERS,
  type TreeFilter,
  whoAmIQuery,
} from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { type PublishedReferences, usePublishReferenceNames } from "../refs/reference-names";
import { EVENT_FRESH } from "../refs/reference-target";
import { buildDispatchReference, parseNodeReference } from "../refs/routes";
import {
  buildIssueTree,
  type IssueTreeReads,
  nodeWords,
  type TreeAsk,
  type TreeNode,
  type TreeViews,
  targetNaming,
} from "./issue-tree";
import { citedReferences } from "./row-references";
import { askState } from "./tree-drafts";
import { holdsPlanTask } from "./tree-read/tree-nodes";
import { type TreeAnswer, type TreeRead, useTreeRead, viewQuery } from "./tree-read/tree-read";
import { useLineRefusals } from "./useLineRefusals";

/** The issue's tree, and how far the tree read it is built from has got (`TreeRead`): `root` is
 *  built from what that read holds, and keeps standing on it through a refetch that fails. */
export interface IssueTreeView
  extends Pick<TreeRead, "blocksHeld" | "blocksOf" | "blocksRead" | "failed" | "pending"> {
  /** The names line's state while the names stand failed (`useLineRefusals`), and none while
   *  they do not: what a view reads to draw the page's line that the names failed. The names
   *  are the batched names read's (`useIssueTree`); they stand failed from a failure of that read
   *  while it holds no answer under its set of addresses until it holds one under its set. The
   *  names in hand still stand meanwhile - the tree and what the page publishes are built from
   *  the last answer the read gave - so a view keeps every row's words and every chip's name
   *  beside that line. A first read that fails holds none; a read under a new set of addresses
   *  that fails stands failed while the tree keeps the last names. The line stays while a read
   *  that failed runs again; while another read the tree is built from runs again after it
   *  failed, which holds the names read back - it keeps the set it was last given meanwhile
   *  (`useIssueTree`); and while the names read is out under a new set of addresses - that other
   *  read answered with words that cite an address the tree did not hold, say. It says a failure
   *  again only for a new one (`NamesFailure.failure`). A refresh that fails over an answer the
   *  read holds counts nothing (`standsFailed`). */
  readonly namesFailure: NamesFailure | undefined;
  /** Whether the names the tree is built from are held: the batched names read has answered
   *  (`useIssueTree`). That read is always made: the root's words write the issue's own key
   *  (`issue-tree.ts`'s `assemble`), and a key written bare is an address the read names
   *  (`row-references.ts`'s `citedReferences`). It names what a chip points at and what a node's
   *  words cite, which a view that reads the names waits on; it gives no node its words, which
   *  the tree read holds. Its last answer stands while its read of a new set of addresses is out
   *  or has failed (`namesFailure`), so the names stay held then; where its very first read fails
   *  it holds none, and this is false until it answers. */
  readonly namesHeld: boolean;
  /** What the page publishes of its batched read (`usePublishReferenceNames`), which the view
   *  that draws the tree's rows hands them as well, in the render that draws them
   *  (`PublishedReferenceNames`), so no row reads a reference the page is asking for. */
  readonly published: PublishedReferences;
  readonly root: TreeNode;
}

/** The names line's state while the tree's names stand failed (`IssueTreeView.namesFailure`). */
export interface NamesFailure {
  /** How many times the names read has failed since it last held an answer under its set of
   *  addresses (`useLineRefusals`): one for each of its failures while it holds no answer under
   *  its set, as TanStack Query's `errorUpdateCount` rises under that set. The read moved to
   *  another set adds the failures that set's query holds, new to the line, so the count changes
   *  only with a new failure, and the line draws its alert again only then. It is not the read's
   *  own `errorUpdateCount`. */
  readonly failure: number;
  /** Asks the names read again (its own `refetch`), and no other read. */
  readonly retry: () => void;
  /** Whether the names read runs, again or under a new set of addresses (TanStack Query's
   *  `isFetching`). */
  readonly retrying: boolean;
}

/**
 * Every `dispatch://` address the tree holds a chip or a citation of: each chip's, so a chip can
 * name its target, and each one a node's words or its answer cite, so a row links what its words
 * name from this read and makes none per mention (`row-references.ts`). This is what the batched
 * reference read is given, which is why the page draws every chip and every link in one request
 * rather than one per node (R21). No node's own address is given for the node itself: the tree
 * read holds every node's words, its stored source among them, so a node's address is given only
 * where a chip names that node or a text cites it. It is here, beside the reads it is the key
 * of, and not beside the model, which makes no read; what a text cites is read by
 * `citedReferences` (`row-references.ts`), over the one reader of a text's citations
 * (`text-references.ts`).
 *
 * Beside an issue's whole spec goes the issue's own address. A link titles the spec by the
 * issue's title (`issueOrDocumentTitle`), which the read answers only under that address, so
 * without it a plain line the page feeds would name another issue's spec by the page's word for
 * a document, where a link and a line's own read name it by that title (`tree-drafts.ts`'s
 * `treeContext`).
 *
 * The note a person wrote as they declined an ask is drawn under the ask's row in the Decisions
 * view (`DecisionsRow.tsx`'s `Declined`) from the asks read, not from a node, so what each note
 * cites (`asks`) is asked for here too. An ask is declined by the tree's own rule (`askState`):
 * the server stores a decline as a `resolved` ask whose resolution is of kind `declined`.
 */
function treeReferences(root: TreeNode, asks: readonly TreeAsk[] = []): string[] {
  const refs = new Set<string>();
  const add = (reference: string): void => {
    refs.add(reference);
    const route = parseNodeReference(reference);
    if (route?.kind === "spec" && route.block === undefined) {
      refs.add(buildDispatchReference({ key: route.key, kind: "issue" }));
    }
  };
  const walk = (node: TreeNode): void => {
    for (const chip of [...node.chips, ...node.alsoUnder]) add(chip.ref);
    for (const reference of citedReferences(nodeWords(node))) add(reference);
    for (const reference of citedReferences(node.answer?.text ?? "")) add(reference);
    for (const child of node.children) walk(child);
  };
  walk(root);
  for (const ask of asks) {
    if (askState(ask) !== "declined") continue;
    for (const reference of citedReferences(ask.resolution?.reason ?? "")) add(reference);
  }
  return [...refs];
}

/** What the page publishes of the batched name read until it answers: no target read, and the
 *  same empty value on every render, so no surface beside the tree draws again for a read that
 *  names nothing. */
const NO_TARGET_NAMING: Omit<PublishedReferences, "asked"> = {
  ambiguous: new Set(),
  deleted: new Map(),
  edited: new Set(),
  gone: new Set(),
  readings: new Map(),
  unnamed: new Set(),
};

/** The set of addresses the batched names read is given until the gate first opens: none, and
 *  the same empty set on every render, so the read's options are made once meanwhile. */
const NO_REFS: readonly string[] = [];

/**
 * The last value given other than undefined: the one given while there is one, and the last one
 * given before while undefined is. The batched names read keeps by it the set of addresses it
 * was last given while the gate that holds it back is shut (`useIssueTree`'s `askedFor`). It
 * keeps a set of addresses and never a names answer: a correction's marks are written into the
 * answer the cache holds (`markCorrectedPost`), and a ref holding the answer itself would go on
 * drawing it as it was kept, a deleted post's words among them, so the answer is read from the
 * cache under the set it was last answered for (`useLastAnswered`). The ref is written during
 * render only with the value given in that render, so it never stands over a newer one.
 */
function useLastAnswer<T>(value: T | undefined): T | undefined {
  const last = useRef(value);
  if (value !== undefined) last.current = value;
  return last.current;
}

/**
 * The set of addresses a names read answered last: `refs` while its read under them holds an
 * answer (`answered`), and the set it last held one for while its read under a new set is out or
 * has failed. The page draws that set's answer as the cache holds it now, through an observer
 * that never fetches (`enabled: false`): a write into the cached answer, as a correction's marks
 * are (`markCorrectedPost`), draws again, and the observer keeps the query from garbage
 * collection, where a ref holding the answer itself would go on drawing it as it was kept.
 * TanStack's `placeholderData` would hold the last answer only while the read is out: a read
 * under a new key that fails holds no data, where a refetch that fails under the same key keeps
 * its own. The ref is written during render only with the set the read answers in that render,
 * so it never stands over a newer one.
 */
function useLastAnswered(refs: readonly string[], answered: boolean): readonly string[] {
  const last = useRef<readonly string[]>([]);
  if (answered) last.current = refs;
  return last.current;
}

/** The issue's comments as the tree reads them (`listComments`), under the key the feed's rules
 *  refresh (`["comments", key]`); a view of the same read takes its key from here. */
export const issueCommentsQuery = (key: string) =>
  queryOptions({
    queryKey: ["comments", key],
    queryFn: () => api.listComments(key),
  });

/** The issue's messages as the tree reads them (`listIssueMessages`), under the key the feed's
 *  rules refresh (`["issue-messages", key]`); a view of the same read takes its key from here. */
export const issueMessagesQuery = (key: string) =>
  queryOptions({
    queryKey: ["issue-messages", key],
    queryFn: () => api.listIssueMessages(key),
  });

/** The reads a tree is built from (`IssueTreeReads`) beside the tree read's answer, each compared
 *  by identity; the answer is not among them, since `builtTrees` holds the trees built from it
 *  keyed by it. Every other member is named here, so a member added to `IssueTreeReads` does not
 *  compile until it is named. */
const TREE_INPUTS: Readonly<Record<Exclude<keyof IssueTreeReads, "tree">, true>> = {
  asks: true,
  comments: true,
  coverage: true,
  decisions: true,
  holdsPlanTask: true,
  issue: true,
  messages: true,
  results: true,
  targets: true,
  viewer: true,
};

/** The members `TREE_INPUTS` names, which `builtTree` compares. */
const TREE_INPUT_NAMES = Object.keys(TREE_INPUTS) as (keyof typeof TREE_INPUTS)[];

/** How many trees are kept for one answer of the tree read: the tree before the names answer,
 *  the tree with them, and one more. */
const TREES_PER_ANSWER = 3;

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
  const found = held.find((one) =>
    TREE_INPUT_NAMES.every((input) => one.reads[input] === reads[input])
  );
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
 * The tree is the outline read the page holds (`useTreeRead`): Conversation's read, or Graph's
 * in Graph, as the view of the address the router is at chooses it (`tree-read/tree-read.ts`'
 * `outlineOf`), the pages of it read so far - the first on load, each next on the reader's press
 * or an address (R15) - which place, order and word every node; no document's blocks are read.
 * `IssueTreeReads.tree` is that read's whole answer as loaded (R7), so
 * `presetView`, `filterSignals` and `blocksOf` read the one tree the page holds. Beside it stand
 * the reads that tell what the answer does not: the coverage, decisions, results, comments,
 * messages and the reader's login. Details' pins and the new page's margin share the outline's
 * asks read with each ask's thread (`issueAskThreadsQuery`). The comments also say which
 * decision a person asked to change (`decision-review.ts`); the login says which open questions
 * wait on the reader (`IssueTreeReads.viewer`).
 *
 * Each filter's view read (`viewQuery`) is watched here and never sent: `ViewReadProvider`
 * (`tree-read/view-read.tsx`) reads a view the first time its filter is on, and whatever it holds,
 * with the decisions read, is set on the issue's node (`TreeNode.views`), which `presetView` and
 * `filterSignals` draw a filter from, its rows and its count. The root keeps its identity while
 * neither the tree nor a held view changes.
 *
 * The names come last and all at once: the tree is built, every address its chips name and its
 * words cite is read in one batched request (`referenceTargetsQuery`; a tree holding more
 * addresses than the server reads at once costs one request per batch of them, which no ordinary
 * issue reaches), and the tree is built again with the names in it. No node's own address is
 * read for the node: the tree read holds every node's words, its stored source among them
 * (`treeReferences`). That is the one names read of a round: the names add no chip and no
 * citation to the tree. Until the names answer the tree draws, with a chip naming its target's
 * address; on every read after the first the names already in hand stand while the new set
 * loads, and after its read fails (`IssueTreeView.namesFailure`).
 */
export function useIssueTree(issue: IssueDetails): IssueTreeView {
  const key = issue.key;
  const read = useTreeRead(issue);
  // Every read here takes `EVENT_FRESH`: events refresh it, and a slot mounted again reads
  // nothing.
  // The page's stage mounts the tree's slot again on every press of a view, so under the app's
  // 30 s `staleTime` (`main.tsx`) a press more than 30 s after the load would read each one
  // again, though nothing it holds had changed.
  //
  // Of the feed's rules (`api/event-refresh.ts`), these refresh the reads the tree is built from:
  // `["comments", key]` on a comment event and on a document's upload or new version;
  // `["asks", key]` on an ask event, a reply on an ask, a review of a document and a block
  // repaired or found invalid; `["decisions", key]` on an ask event, a Keep, a document's new
  // version and the block index catching up on documents a write left owing it
  // (`artifact.indexed`); `["issue-messages", key]` on a message event;
  // `["issue", key, "coverage"]` on a document's upload or new version and on `artifact.indexed`;
  // `["issue", key, "results"]` on those too, on a result recorded or retracted and on a pull
  // request moved to another issue; and a names read (`["reference-targets", …]`) on an event
  // that changes the words or the name under an address its key holds, on the issue or project
  // document that address is of: a comment's or an issue message's edit or delete, an ask's edit,
  // a document's creation, new version, rename or removal, an `artifact.indexed`, and an issue's
  // write that changes its title (`referenceNameTargets`). A post's edit or delete also reaches a
  // read that names the post by its whole id under another owner (`correctedPostAddresses`), and
  // `markCorrectedPost` writes the post's marks into the answers the cache holds. A names read's
  // key is the set of addresses it names, so a tree that comes to hold another set reads again
  // under a key of its own, and only a change under an address the tree already names needs a
  // rule. No rule names `["whoami"]`, which the tree reads with `EVENT_FRESH` too: the reader's
  // login does not change while a page is open. The tree read and its views keep the same rule
  // but for a focus, which reads them again only while the live feed is down (`TREE_FRESH`),
  // since their events already reach them while it is up.
  const coverage = useQuery({ ...issueCoverageQuery(key), ...EVENT_FRESH });
  const decisions = useQuery({ ...issueDecisionsQuery(key, true), ...EVENT_FRESH });
  const results = useQuery({ ...issueResultsQuery(key), ...EVENT_FRESH });
  const asks = useQuery({ ...issueAskThreadsQuery(key), ...EVENT_FRESH });
  // Under the keys the margin, the history and the focused row's facts read too, so a reader
  // opening the panel after the tree finds them answered rather than read a second time.
  const comments = useQuery({ ...issueCommentsQuery(key), ...EVENT_FRESH });
  // Under the key the history and the margin read too, and one of the reads the names wait on
  // (`answered`); the model reads from it only what its author did to a message - deleted it or
  // last edited it - since the tree read answers each message as a node with no such mark.
  const messages = useQuery({ ...issueMessagesQuery(key), ...EVENT_FRESH });
  // The reader, whose own turn an open question may be (`IssueTreeReads.viewer`): the shell's
  // one read of who is signed in, so it costs no request of its own.
  const who = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH });
  const viewer = who.data?.login;
  // Each filter's view read, watched by an observer that is never enabled, so it sends nothing
  // and holds whatever `ViewReadProvider` read for the load. Each answer is read by its filter, at
  // that filter's place in the list the reads were made from, so the order of `TREE_FILTERS`
  // hands no view another's answer.
  const viewReads = useQueries({
    queries: TREE_FILTERS.map((filter) => ({ ...viewQuery(key, filter), enabled: false })),
  });
  const viewOf = (filter: TreeFilter): TreeAnswer | undefined =>
    viewReads[TREE_FILTERS.indexOf(filter)].data;
  const spec = viewOf("spec");
  const verification = viewOf("verification");
  const plan = viewOf("plan");
  const views = useMemo((): TreeViews | undefined => {
    const held: { -readonly [K in keyof TreeViews]: TreeViews[K] } = {
      ...(decisions.data === undefined ? {} : { decisions: decisions.data }),
    };
    const answers = { plan, spec, verification };
    for (const filter of TREE_FILTERS) {
      const answer = answers[filter];
      if (answer !== undefined) held[filter] = answer;
    }
    return Object.keys(held).length === 0 ? undefined : held;
  }, [decisions.data, plan, spec, verification]);
  // One value, so the model is given the reads and nothing else. Of the Plan view's answer the
  // model reads only whether it holds a plan task (`IssueTreeReads.holdsPlanTask`), so a next page
  // or a refresh of that view that leaves it as it was builds no tree again. It is read from the
  // answer the page holds (`plan`): a later read of it that is refused keeps the answer before,
  // so a statement it marks keeps its mark.
  const planTaskHeld = useMemo(() => holdsPlanTask(plan), [plan]);
  const reads = useMemo<IssueTreeReads>(
    () => ({
      asks: asks.data,
      comments: comments.data,
      coverage: coverage.data,
      decisions: decisions.data,
      holdsPlanTask: planTaskHeld,
      issue,
      messages: messages.data,
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
      messages.data,
      planTaskHeld,
      read.answer,
      results.data,
      viewer,
    ]
  );
  const unnamed = useMemo(() => builtTree(reads), [reads]);
  const refs = useMemo(() => treeReferences(unnamed, asks.data), [unnamed, asks.data]);
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
  // address - until the new read lands, and for as long as it keeps failing. The answer that
  // stands is the one the cache holds now for the set the read answered last, read by an
  // observer that never fetches (`useLastAnswered`). Merging the two answers by `ref` would say
  // no more than this does: the new read names every address the tree now holds, so the only
  // names it lacks are the ones the previous answer carries, which is what is kept here. A name
  // that moved is a stale word until a read under the new key succeeds, where a dropped one is
  // the whole page's names at once; while that read stands failed - until its Retry, window
  // focus or an event reads it again (`EVENT_FRESH`) - the view says so
  // (`IssueTreeView.namesFailure`), and goes on saying so while a read under a new set is out,
  // until the names read holds an answer under its set (`useLineRefusals`).
  //
  // The gate shuts again while a read that holds no answer runs again - a failed read's Retry,
  // window focus or an event's refresh of it, which TanStack puts back in `pending` - and the
  // names read keeps the set it was last given until the gate opens, by `useLastAnswer`, which
  // holds the last value given other than undefined: it keeps the set, and the answer is read
  // from the cache, as above. Moved to the set of none, which is no read, the line's Retry would
  // ask the read of that empty set, which names nothing, and the set it failed under would be
  // read again each time the gate opened on it once more. Kept, the line says that read's state
  // while another read runs: whether it runs again, and a Retry that asks it again. Where the
  // read that ran answers with words that cite an address the tree did not hold, the gate opens
  // on a new set, and the line stays while the read under it is out, with the same alert and the
  // focus its Retry holds (`useLineRefusals`). Until the gate first opens the set is none.
  const askedFor = useLastAnswer(answered ? refs : undefined) ?? NO_REFS;
  const targetsQuery = useMemo(() => referenceTargetsQuery(askedFor), [askedFor]);
  const targetsHash = useMemo(() => hashKey(targetsQuery.queryKey), [targetsQuery]);
  const targets = useQuery({ ...targetsQuery, ...EVENT_FRESH });
  const targetsAnswered = targets.data !== undefined;
  const answeredSet = useLastAnswered(askedFor, targetsAnswered);
  const heldAnswer = useQuery({
    ...referenceTargetsQuery(answeredSet),
    ...EVENT_FRESH,
    enabled: false,
  });
  const answer = targets.data ?? heldAnswer.data;
  const named = useMemo(
    () => (answer === undefined ? undefined : (answer.targets ?? [])),
    [answer]
  );
  // Whether the names the tree is built from are held (`IssueTreeView.namesHeld`): the read
  // holds an answer. It is always made, since the root's words cite the issue's own key.
  const namesHeld = named !== undefined;
  // The names stand failed, the names in hand standing through it (above), from a failure of the
  // names read while it holds no answer under its set until it holds one, by one count of its
  // failures (`useLineRefusals`), so the line says a failure again only for a new one: the read
  // out under a set never read before does not change it. The Retry asks the read again, and the
  // line is busy while it runs.
  const [failure] = useLineRefusals([
    [{ errorUpdateCount: targets.errorUpdateCount, failing: !targetsAnswered, hash: targetsHash }],
  ]);
  const { isFetching: retrying, refetch: refetchTargets } = targets;
  const namesFailure = useMemo(
    (): NamesFailure | undefined =>
      failure > 0
        ? {
            failure,
            retry: () => {
              void refetchTargets();
            },
            retrying,
          }
        : undefined,
    [failure, refetchTargets, retrying]
  );
  // The same answer every surface of this page would otherwise read one pointer at a time, which
  // a link in a document, in a decision's card or in a comment's preview shows, a row's in-place
  // chip and every one line of a node too (`refs/reference-names.tsx`, R20). The page publishes
  // how each target is named, by the one rule a chip of the tree is named by (`targetNaming`),
  // and the store projects each once, so every one of them draws a target in the same words.
  // Beside the readings go the references the read answered ambiguous and those it answered
  // gone, read by that same rule, each of which a surface draws as its author wrote it rather
  // than reading it itself; those it answered with nothing to name them by, which a surface reads
  // itself, as a link to one does; the references whose post its author edited, which a link
  // marks ` · edited` after it cuts the name to its own length, so no cut takes the mark, and
  // those whose post its author deleted, by the delete, which a row's words draw as the deleted
  // line with its actor named as the page names an author; and every reference the page asks the
  // read for, which a surface waits on the page's answer for rather than reading it itself, from
  // before the first answer on. A chip's reading is read from the issue, the tree read and the
  // names alone (`treeContext`), so it is built from those and nothing else: an answer of any
  // other read - an ask, a comment, the coverage, the reader's login - names nothing new.
  const naming = useMemo(
    () =>
      named === undefined
        ? NO_TARGET_NAMING
        : targetNaming({ issue, targets: named, tree: read.answer }),
    [issue, named, read.answer]
  );
  // The tree is read anew whenever any of its reads answers, and so is each set of references
  // published from it; the store hands readers the set it already holds where a new one holds
  // the same references (`refs/reference-names.tsx`'s `merged`), so such a set draws nothing
  // again.
  const published = useMemo<PublishedReferences>(
    () => ({ ...naming, asked: new Set(refs) }),
    [naming, refs]
  );
  usePublishReferenceNames(published);
  const built = useMemo(
    () => (named === undefined ? unnamed : builtTree({ ...reads, targets: named })),
    [named, reads, unnamed]
  );
  // Each held view's tree (`TreeViews.trees`): a getter per filter whose view answer is held,
  // which builds the tree from that answer with the outline's context on its first read and
  // answers that one tree on every read after, so a filter no reader asks for builds nothing,
  // and callers of the same reads hold trees that compare equal.
  const trees = useMemo(() => {
    const held: Partial<Record<TreeFilter, TreeNode>> = {};
    for (const filter of TREE_FILTERS) {
      const answer = views?.[filter];
      if (answer === undefined) continue;
      let tree: TreeNode | undefined;
      Object.defineProperty(held, filter, {
        enumerable: true,
        get: (): TreeNode => {
          tree ??= builtTree({
            ...reads,
            tree: viewWithContext(answer, reads.tree),
            ...(named === undefined ? {} : { targets: named }),
          });
          return tree;
        },
      });
    }
    return held;
  }, [named, reads, views]);
  const root = useMemo(
    () => (views === undefined ? built : { ...built, views: { ...views, trees } }),
    [built, trees, views]
  );
  return useMemo(
    () => ({
      blocksHeld: read.blocksHeld,
      blocksOf: read.blocksOf,
      blocksRead: read.blocksRead,
      failed: read.failed,
      namesFailure,
      namesHeld,
      pending: read.pending,
      published,
      root,
    }),
    [
      read.blocksHeld,
      read.blocksOf,
      read.blocksRead,
      read.failed,
      namesFailure,
      namesHeld,
      read.pending,
      published,
      root,
    ]
  );
}

/** Keep the view's placement and metadata, adding only outline context the view omitted. */
function viewWithContext(answer: TreeAnswer, outline: TreeAnswer | undefined): TreeAnswer {
  if (outline === undefined) return answer;
  const nodes = new Set(answer.nodes?.map(({ id, kind }) => `${kind}:${id}`));
  const tupleKey = ({ kind, from, to }: NonNullable<TreeAnswer["tuples"]>[number]) =>
    `${kind} ${from.kind} ${from.id} ${to.kind} ${to.id}`;
  const tuples = new Set(answer.tuples?.map(tupleKey));
  const extraNodes = outline.nodes?.filter(({ id, kind }) => !nodes.has(`${kind}:${id}`));
  const extraTuples = outline.tuples?.filter((tuple) => !tuples.has(tupleKey(tuple)));
  if (!extraNodes?.length && !extraTuples?.length) return answer;
  return {
    ...answer,
    nodes: [...(answer.nodes ?? []), ...(extraNodes ?? [])],
    tuples: [...(answer.tuples ?? []), ...(extraTuples ?? [])],
  };
}
