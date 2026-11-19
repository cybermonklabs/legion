import { queryOptions, useQueries, useQuery } from "@tanstack/react-query";
import { useMemo, useRef } from "react";

import { api } from "../../api/client";
import { primarySpec, roleDocument } from "../../api/issue-cache";
import {
  artifactBlocksQuery,
  issueCoverageQuery,
  issueResultsQuery,
  referenceTargetsQuery,
  whoAmIQuery,
} from "../../api/queries";
import type { IssueDetails, ReferenceTarget } from "../../api/types";
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
  /** Whether a read of the tree's names - the batched one, or the one more that names what a
   *  typed block's words cite (`useIssueTree`) - stands in error: its newest attempt failed,
   *  under a new set of addresses or as a refetch of the set it had answered. It is what a view
   *  reads to draw the page's line that the names failed. The names in hand stand meanwhile -
   *  the tree and what the page publishes are built from the last answer each read gave - so a
   *  view keeps every row's words and every chip's name beside that line. It is false before
   *  the first answer and once both reads have answered with success. */
  readonly namesFailed: boolean;
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
 * recorded or retracted and on a pull request moved to another issue; `["artifact", id]`, a
 * prefix of the document's blocks read (`["artifact", id, "blocks"]`), on its new version, its
 * rename, removal or review, and an ask or comment in it; and a names read
 * (`["reference-targets", …]`) on an event that changes the words or the name under an address
 * its key holds, on the issue or project document that address is of: a comment's edit, an
 * ask's edit, a document's new version, rename or removal, and an issue's write that changes its
 * title (`referenceNameTargets`). A names read's key is the set of addresses it names, so a tree
 * that comes to hold another set reads again under a key of its own, and only a change under an
 * address the tree already names needs a rule. No rule there names `["whoami"]`: the reader's
 * login does not change while the page is open. A gap in the feed refreshes every read the app
 * holds (`refreshQueries`), these among them, and window focus fetches again only a read in
 * error (`refetchFailedOnFocus`).
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
 * yet; on every read after the first the names already in hand stand while the new set loads,
 * and after its read fails (`IssueTreeView.namesFailed`).
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
  // The last answer stands while the next one loads and after it fails, because the key is the
  // set of addresses itself: one comment, message, ask or block arriving changes that set, and
  // without this the whole page would fall back to no names at all - every block row drawn with
  // empty text and every chip with its raw address - until the new read lands, and for as long
  // as it keeps failing (`useLastAnswer`). Merging the two answers by `ref` would say no more
  // than this does: the new read names every address the tree now holds, so the only names it
  // lacks are the ones the previous answer carries, which is what is kept here. A name that
  // moved is a stale word until a read under the new key succeeds, where a dropped one is the
  // whole page's words at once; while that read stands failed - until window focus or an event
  // reads it again (`EVENT_FRESH`) - the view says so (`IssueTreeView.namesFailed`).
  const targets = useQuery({
    ...referenceTargetsQuery(treeRead ? refs : []),
    ...EVENT_FRESH,
  });
  const firstAnswer = useLastAnswer(targets.data);
  const first = useMemo(
    () => (firstAnswer === undefined ? undefined : (firstAnswer.targets ?? [])),
    [firstAnswer]
  );
  const firstTree = useMemo(
    () => (first === undefined ? unnamed : buildIssueTree({ ...reads, targets: first })),
    [first, reads, unnamed]
  );
  // What the named tree's words cite that the first read was not given: a typed block's words
  // come from that read's answer, so what they cite is known only now (above). An empty set is
  // no request, and the names already in hand stand while a new non-empty set loads and after
  // its read fails, as above.
  const extra = useMemo(() => {
    if (first === undefined) return [];
    const asked = new Set(refs);
    return treeReferences(firstTree).filter((reference) => !asked.has(reference));
  }, [first, firstTree, refs]);
  const more = useQuery({
    ...referenceTargetsQuery(extra),
    ...EVENT_FRESH,
  });
  // Both answers, the second's standing for an address both name. Where nothing is left for the
  // second read to name, it has no answer, and the one it gave before is dropped: its query is
  // disabled, so that answer would stand for good over the first read's newer names for the
  // same addresses, and again while a later set loads or after its read fails.
  const moreAnswer = useLastAnswer(extra.length === 0 ? NO_MORE_TARGETS : more.data);
  const named = useMemo(() => {
    const added = moreAnswer?.targets ?? [];
    if (first === undefined || added.length === 0) return first;
    return [...new Map([...first, ...added].map((target) => [target.ref, target])).values()];
  }, [first, moreAnswer]);
  // A failure of either read, which the names in hand stand through (above). The second read
  // fails only where something is left for it to name: where nothing is, its query is disabled.
  const namesFailed = targets.isError || more.isError;
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
    () => ({ blocksHeld: documents.held, blocksRead: documents.read, namesFailed, root }),
    [documents.held, documents.read, namesFailed, root]
  );
}
