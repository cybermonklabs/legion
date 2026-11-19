import { keepPreviousData, queryOptions, useQueries, useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import { primarySpec, roleDocument } from "../../api/issue-cache";
import {
  artifactBlocksQuery,
  issueCoverageQuery,
  issueResultsQuery,
  referenceTargetsQuery,
  whoAmIQuery,
} from "../../api/queries";
import type { IssueDetails } from "../../api/types";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { refetchFailedOnFocus } from "../refs/reference-target";
import {
  ambiguousTargets,
  buildIssueTree,
  type DocumentBlocks,
  type IssueTreeReads,
  nodeWords,
  type TreeNode,
  targetNames,
} from "./issue-tree";
import { usePublishReferenceNames } from "./reference-names";
import { citedReferences } from "./row-references";

/** The issue's tree, and how far the reads it is built from have got. */
export interface IssueTreeView {
  /** Whether every document the tree reads (`treeDocumentIds`) holds blocks, which `root` is
   *  built from: what a view that draws from the blocks it holds waits on. A refetch that fails
   *  over blocks already read keeps them held - TanStack keeps a query's data through a failed
   *  refetch, and `root` is still built from it - so the view keeps the text it drew, which
   *  waiting on `blocksRead` would blank, since that failure turns it false. That is the one
   *  case where the two differ: both are false before a document's first answer and after a
   *  first read that fails. */
  readonly blocksHeld: boolean;
  /** Whether every blocks read of the tree's documents (`treeDocumentIds`) has answered with
   *  success. A read that fails, a first one or a refetch, turns it false, even where a first
   *  read's blocks are still held. It is what a view reads that must tell "not read yet" from
   *  "holds none": until it is true a typed block - the spec's summary among them - may be
   *  missing from `root` because it has not arrived rather than because its document holds
   *  none. A view that draws from the blocks it holds reads `blocksHeld` instead. */
  readonly blocksRead: boolean;
  readonly root: TreeNode;
}

/**
 * The documents whose blocks the tree reads: the spec and the plan, Verification and decisions
 * documents, whose typed blocks are the ones the page draws (the root spec's R2), each the one
 * the issue read names by its id (`primarySpec`, `roleDocument`). That id is the server's own
 * answer to which document plays the part - the one in that role, or a role-less one under the
 * role's slug, which is how a row a rolling deploy left with no role still plays it
 * (`api/artifacts.go`'s `documentPlays`) - and the coverage and results reads find the
 * document by the same rule. The artifact's own `role` is no answer: the server omits it on
 * such a row, so a plan read by its role would leave that plan's tasks out of the tree while
 * the coverage counts them. A block read for every document of the issue was 25 of a first
 * load's 47 requests, so no other document's blocks are read: four reads at most, and fewer
 * where one document plays two parts or the issue holds none in a part.
 */
export function treeDocumentIds(issue: IssueDetails): string[] {
  const ids = new Set<string>();
  for (const document of [
    primarySpec(issue),
    roleDocument(issue, "plan"),
    roleDocument(issue, "verification"),
    roleDocument(issue, "decisions"),
  ]) {
    if (document !== undefined) ids.add(document.id);
  }
  return [...ids];
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
 * recorded or retracted and on a pull request moved to another issue; `["artifact", id]`, a
 * prefix of the document's blocks read (`["artifact", id, "blocks"]`), on its new version, its
 * rename, removal or review, and an ask or comment in it; and the names reads
 * (`["reference-targets", …]`, by that prefix) on a comment's edit, an ask's edit and a
 * document's new version (`referenceNameTargets`). A names read's key is the set
 * of addresses it names, so a tree that comes to hold another set reads again under a key of its
 * own, and those three events are what change a target's words under an address the tree
 * already names: a cited comment edited, a cited ask's question edited, a block of a document's
 * new version. No rule there names `["whoami"]`: the reader's login does not change while the
 * page is open. A gap in the feed refreshes every read the app holds (`refreshQueries`), these
 * among them, and window focus fetches again only a read in error (`refetchFailedOnFocus`).
 */
const EVENT_FRESH = {
  refetchOnWindowFocus: refetchFailedOnFocus,
  staleTime: Number.POSITIVE_INFINITY,
} as const;

/**
 * The issue's tree, from the page's reads. This hook is where the reads are; `buildIssueTree` is
 * pure and holds none of them, so the model is testable without a server and the page makes no
 * request per node.
 *
 * The reads are the issue's own (handed in, because the page already has it), the coverage, the
 * decisions, the results, the issue's asks, and the blocks of the spec and of the plan,
 * Verification and decisions documents (`treeDocumentIds`) - ids, types and
 * attributes, never text, which is how a `serves` edge is held without reading a document the
 * reader has not opened (R20). Every one of them is a key another surface of this issue already
 * reads, so opening the Conversation or the Decisions panel after the tree finds them answered
 * rather than fetching again. The conversation's comments and messages are nodes of this tree
 * too, its turns, and the comments also say which decisions a person asked to change
 * (`tree-drafts.ts`'s `treeDrafts`, `decision-review.ts`). No document is a node of the tree
 * (R3), so nothing here reads a document's own text. The reader's login says which open
 * questions wait on the reader (`IssueTreeReads.viewer`).
 *
 * The names come last and all at once: the tree is built, every address it holds is read in one
 * batched request (`referenceTargetsQuery`; a tree holding more addresses than the server reads
 * at once costs one request per batch of them, which no ordinary issue reaches), and the tree is
 * built again with the names in it. A typed block that is no statement, check, ask or decision -
 * a plan task, a callout - has no words until that read answers, so what its words cite is in
 * no address the read was given: one more read names those addresses alone, made only where
 * such words cite one, and the tree is built with both answers. That is the last read of a
 * round: a node's words come from the names of node addresses, every one of which the first
 * read holds, so the second's answer changes no node's words. Until the names answer the tree
 * draws, with a chip naming its target's address and a block of a document carrying no words
 * yet; on every read after the first the names already in hand stand while the new set loads.
 */
export function useIssueTree(issue: IssueDetails): IssueTreeView {
  const key = issue.key;
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
  const messages = useQuery({
    queryKey: ["issue-messages", key],
    queryFn: () => api.listIssueMessages(key),
    ...EVENT_FRESH,
  });
  // The reader, whose own turn an open question may be (`IssueTreeReads.viewer`): the shell's
  // one read of who is signed in, so it costs no request of its own.
  const who = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH });
  const viewer = who.data?.login;
  const documentIds = treeDocumentIds(issue);
  // `combine` is what keeps one value for every document's blocks: TanStack holds its answer and
  // replaces it only when a document's own read changes, where a map over the results would be a
  // new array every render and build the tree again with it. It answers whether any of them is
  // still waiting too, which is part of what holds the one batched name read back below, whether
  // every one holds blocks the tree draws (`IssueTreeView.blocksHeld`), and whether every one
  // has answered with its blocks (`IssueTreeView.blocksRead`).
  const documents = useQueries({
    combine: (
      answers
    ): { blocks: DocumentBlocks[]; held: boolean; pending: boolean; read: boolean } => ({
      blocks: answers.map((answer, index) => ({
        artifactId: documentIds[index] ?? "",
        blocks: answer.data ?? [],
      })),
      held: answers.every((answer) => answer.data !== undefined),
      pending: answers.some((answer) => answer.isPending),
      read: answers.every((answer) => answer.isSuccess),
    }),
    queries: documentIds.map((id) => ({ ...artifactBlocksQuery(id), ...EVENT_FRESH })),
  });
  // One value, so the model is given the reads and nothing else.
  const reads = useMemo<IssueTreeReads>(
    () => ({
      asks: asks.data,
      comments: comments.data,
      coverage: coverage.data,
      decisions: decisions.data,
      documents: documents.blocks,
      issue,
      messages: messages.data,
      results: results.data,
      viewer,
    }),
    [
      asks.data,
      comments.data,
      coverage.data,
      decisions.data,
      documents.blocks,
      issue,
      messages.data,
      results.data,
      viewer,
    ]
  );
  const unnamed = useMemo(() => buildIssueTree(reads), [reads]);
  const refs = useMemo(() => treeReferences(unnamed), [unnamed]);
  // One request for every name, not one per read that landed. Each of these reads adds nodes to
  // the tree as it arrives, so asking while any is still out would ask for the addresses known
  // so far and ask again for the rest: measured on this pod, that cost the first load two reads
  // and sometimes three, where the design is "the names come last and all at once" (above) and
  // the first load's budget is 40 requests. The reader's login
  // is one of the reads this gate waits on: it adds no node, but the tree is built again when
  // it answers, since it says which questions wait on the reader. Every one of these reads is
  // always enabled, so each ends in an answer or an error and none of them holds the names for
  // good. Nothing waits on the names: until they land the tree draws with each chip naming its
  // target's address.
  const treeRead =
    !asks.isPending &&
    !comments.isPending &&
    !coverage.isPending &&
    !decisions.isPending &&
    !messages.isPending &&
    !results.isPending &&
    !who.isPending &&
    !documents.pending;
  // The last answer stands while the next one loads, because the key is the set of addresses
  // itself: one comment, message, ask or block arriving changes that set, and without this the
  // whole page would fall back to no names at all - every block row drawn with empty text and
  // every chip with its raw address - until the new read lands. Merging the two answers by
  // `ref` would say no more than this does: the new read names every address the tree now
  // holds, so the only names it lacks are the ones the previous answer carries, which is what
  // is kept here. A name that moved is a stale word for as long as one read, where a dropped
  // one is the whole page's words at once.
  const targets = useQuery({
    ...referenceTargetsQuery(treeRead ? refs : []),
    ...EVENT_FRESH,
    placeholderData: keepPreviousData,
  });
  const first = useMemo(
    () => (targets.data === undefined ? undefined : (targets.data.targets ?? [])),
    [targets.data]
  );
  const firstTree = useMemo(
    () => (first === undefined ? unnamed : buildIssueTree({ ...reads, targets: first })),
    [first, reads, unnamed]
  );
  // What the named tree's words cite that the first read was not given: a typed block's words
  // come from that read's answer, so what they cite is known only now (above). An empty set is
  // no request, and the names already in hand stand while a new non-empty set loads, as above.
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
  // would not tell it. A chip's name is read from the issue, the documents'
  // blocks and the names alone (`treeContext`), so the map and the set are built from those and
  // nothing else: an answer of any other read - an ask, a comment, the coverage, the reader's
  // login - publishes nothing new, which would draw every link beside the tree again. Until the
  // names answer there is nothing to publish.
  const names = useMemo(
    () =>
      named === undefined
        ? NO_TARGET_NAMES
        : targetNames({ documents: documents.blocks, issue, targets: named }),
    [documents.blocks, issue, named]
  );
  const ambiguous = useMemo(
    () =>
      named === undefined
        ? NO_AMBIGUOUS_TARGETS
        : ambiguousTargets({ documents: documents.blocks, issue, targets: named }),
    [documents.blocks, issue, named]
  );
  usePublishReferenceNames(names, ambiguous);
  const root = useMemo(
    () =>
      named === undefined || named === first
        ? firstTree
        : buildIssueTree({ ...reads, targets: named }),
    [first, firstTree, named, reads]
  );
  return useMemo(
    () => ({ blocksHeld: documents.held, blocksRead: documents.read, root }),
    [documents.held, documents.read, root]
  );
}
