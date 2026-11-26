import { isIdPrefix, itemFromSearch } from "@legion/contracts";
import { notifyManager, type QueryKey, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useMemo, useState, useSyncExternalStore } from "react";
import { useLocation } from "react-router-dom";

import {
  ambiguousItems,
  REFERENCE_TARGETS_MAX,
  REFERENCE_TARGETS_MAX_QUERY_CHARS,
} from "../../api/client";
import { primarySpec, roleDocument } from "../../api/issue-cache";
import { referenceTargetsQuery, TREE_FILTERS } from "../../api/queries";
import type {
  Artifact,
  Ask,
  Comment,
  IssueDetails,
  MessageRead,
  ReferenceTarget,
} from "../../api/types";
import {
  type ItemLanding,
  type ItemRoute,
  isItemRoute,
  useItemLanding,
} from "../issue/useItemLanding";
import { askQuery, commentQuery, messageQuery, referencesQuery } from "../refs/reference-target";
import { buildDispatchReference, type IssueRoute, parseIssuePath } from "../refs/routes";
import { prefixReference } from "../refs/text-references";
import { lastHeldPath } from "./graph-edit/last-held";
import { useKnownLastHeldReferences } from "./graph-edit/last-held-store";
import {
  type AddressTarget,
  agentsOnlyItem,
  type IssueAddress,
  pathToTarget,
} from "./issue-address";
import type { TreeNode } from "./issue-tree";
import { answerHolds } from "./presets";
import { useFollowing } from "./slots/following";
import { lastHeldOf } from "./tree-drafts";
import { nextTreeCursor } from "./tree-read/tree-nodes";
import { type TreeAnswer, viewQuery } from "./tree-read/tree-read";

/** The node an address names, read against the tree (`useAddressTarget`). Each field is one
 *  value for as long as the address and the tree say the same; the object holding them is new
 *  on every render. */
export interface AddressedNode {
  /** The id of the node the address names, or nothing: what a view keeps whatever its filter
   *  or its words match (`presets.ts`'s `presetView`). */
  readonly named: ReadonlySet<string>;
  /** The chain down to the node the address names, the issue first, which the tree unfolds and
   *  puts the focus on the last node of; undefined where the address names no node or the
   *  tree holds none it names. */
  readonly reveal: readonly TreeNode[] | undefined;
  /** The node the address names, as the address says it. */
  readonly target: AddressTarget | undefined;
}

/**
 * The node `address` names, resolved against the whole tree `root` - the page's and every
 * filter's - so a view keeps it even where its filter or its words would not have matched it.
 * Every target an address names is one a tree unfolds and focuses, but the children's
 * (`/children`), which lands on their section above the outline (`children-section.tsx`).
 *
 * The target is read by what it says rather than by the value `issueAddress` built, which is a
 * new one on every render: the tree is walked again only when the address or the tree changes.
 */
export function useAddressTarget(
  root: TreeNode,
  address: Pick<IssueAddress, "target">
): AddressedNode {
  const targetKind = address.target?.kind;
  const targetId = address.target?.id;
  // Which document a block target belongs to, where its address names one: a block id alone
  // names a block of any document of the issue (`pathToTarget`).
  const targetDocument = address.target?.kind === "block" ? address.target.document : undefined;
  const target = useMemo<AddressTarget | undefined>(() => {
    if (targetKind === undefined || targetId === undefined) return undefined;
    if (targetKind === "block") return { document: targetDocument, id: targetId, kind: "block" };
    return { id: targetId, kind: targetKind } as AddressTarget;
  }, [targetDocument, targetId, targetKind]);
  const reveal = useMemo(() => pathToTarget(root, target), [root, target]);
  const named = useMemo(() => namedBy(reveal), [reveal]);
  return { named, reveal, target };
}

/** The id of the last node of the chain `reveal`, the node an address lands on, or nothing where
 *  there is no chain: what a view keeps whatever its filter or its words match (`presetView`). */
export function namedBy(reveal: readonly TreeNode[] | undefined): ReadonlySet<string> {
  const node = reveal?.at(-1);
  return new Set(node === undefined ? [] : [node.id]);
}

/**
 * The ask, comment or message `address` names as a node of the outline, by its route, which is
 * how the old page's landing reads an item (`issue/useItemLanding.ts`): an item's own route
 * (`/asks/<id>`, `/comments/<id>`, `/messages/<id>`), or the item a preset's address names
 * beside it (`?ask=`, `?comment=`) under the issue's key. Undefined for every other address,
 * and for an item the address names beside the document it opens (`documentItem`: `?mark=`, or
 * an item at a pinned version), whose page is that document's and not the outline.
 */
export function outlineItemRoute(
  route: IssueRoute,
  search: string,
  address: Pick<IssueAddress, "documentItem" | "target">
): ItemRoute | undefined {
  if (address.target?.kind !== "item" || address.documentItem !== undefined) return undefined;
  if (isItemRoute(route)) return route;
  const item = itemFromSearch(search);
  return item === null || item === undefined
    ? undefined
    : { id: item.id, key: route.key, kind: item.kind };
}

/**
 * Where this page goes for the old page's landing `to` (`useItemLanding`'s `redirect` and
 * `rewrite`): `to` itself, unless it is an address of the issue page naming the item beside the
 * document it is the address of - `?ask=` or `?comment=`, as `documentItemPath` writes it -
 * which this page reads as the item's node of the outline again (`issueAddress`): the node the
 * tree does not hold, so the landing sent the reader there again and again. That one is the
 * document's address with the item marked (`?mark=`), the address "View in document" builds,
 * which opens the document with the item beside it (`documentItem`) and asks for no node.
 */
export function landingPath(to: string): string {
  const url = new URL(to, "http://dispatch.invalid");
  const item = itemFromSearch(url.search);
  if (item === null || item === undefined || parseIssuePath(url.pathname) === undefined) return to;
  url.searchParams.delete(item.kind);
  url.searchParams.set("mark", item.id);
  return `${url.pathname}${url.search}${url.hash}`;
}

/**
 * The chain from the issue down to the nearest node the tree `root` holds above `id`, a turn of
 * a thread only agents wrote in (`agentsOnlyItem`), which the outline draws no node for: the
 * entry the turn was written under - the comment its write answered (`answers`, else the head of
 * its thread, `reply_to`), the ask it replies under (`ask_id`), the message it answers
 * (`in_reply_to`) - where the tree holds it, else that entry's own, as `tree-read/tree-nodes.ts`
 * places each turn, by its `parent`. Where the tree holds none of them - the head of such a thread
 * sits under the issue, and an approval request is no node (`draftAsk`) - it is the issue alone.
 */
function agentsOnlyAncestor(
  root: TreeNode,
  id: string,
  comments: readonly Pick<Comment, "answers" | "ask_id" | "id" | "reply_to">[] | undefined,
  messages: readonly MessageRead[] | undefined
): readonly TreeNode[] {
  const under = new Map<string, string | null>();
  for (const comment of comments ?? []) {
    under.set(comment.id, comment.answers ?? comment.reply_to ?? comment.ask_id);
  }
  for (const thread of messages ?? []) {
    for (const message of [thread.message, ...(thread.replies ?? [])]) {
      under.set(message.id, message.in_reply_to);
    }
  }
  // Each entry is walked once, so reads whose entries answer each other end at the issue.
  const walked = new Set([id]);
  for (let above = under.get(id); typeof above === "string"; above = under.get(above)) {
    if (walked.has(above)) break;
    walked.add(above);
    const chain = pathToTarget(root, { id: above, kind: "item" });
    if (chain !== undefined) return chain;
  }
  return [root];
}

/** Where an address naming a node lands while the tree holds no node of it, once every read
 *  the tree is built from has answered (`useAddressFallback`). */
export type AddressFallback =
  /**
   * A turn of a thread only agents wrote in, which the outline leaves out on purpose
   * (`agents-only.ts`): `ancestor` is the chain down to the turn's nearest node the tree holds
   * (`agentsOnlyAncestor`), the issue alone where it holds none, which the outline lands on and
   * says at its row that the thread is the agents' alone.
   *
   * It is a kind of its own, never `missing`: the turn is there, in the panel's history. Opening
   * Details at that history with the turn `id` marked is the panel's to do, and the part that
   * builds it takes this kind over from the outline's landing on the ancestor.
   */
  | {
      readonly ancestor: readonly TreeNode[];
      readonly id: string;
      readonly kind: "agents-only";
    }
  /**
   * The item as the old page's landing reads it (`issue/useItemLanding.ts`), once that read has
   * answered: an item of another issue, or one that stands beside a document (`redirect`); an id
   * written short, which names one item (`rewrite`) or several (`ambiguous`); or none this issue
   * holds (`missing`), or a read that failed (`unavailable`). `route` is the item's route, which
   * the match list and the failure name the item by. `retrying` is whether the read the landing
   * answers from runs again, as a Retry of a failed one asks, and `failure` how many times that
   * read has failed since the address was followed (its `errorUpdateCount`, less the count it
   * held as the following first read it), which tells one failure's line from the next.
   */
  | {
      readonly failure: number;
      readonly kind: "landing";
      readonly landing: Exclude<ItemLanding, { kind: "pending" | "turn" }>;
      readonly retrying: boolean;
      readonly route: ItemRoute;
    }
  /**
   * A block or the issue's children, named by an address that opens no document - a `#b-`
   * fragment on the issue's own address or on a view's, and `/children` - where the tree holds
   * no node of it: a block id no node carries, or an issue with no children. No read is left
   * that would draw it, so the page says in the outline's place that the link names nothing on
   * it, rather than leave the reader on the page's body.
   *
   * A node that `?node=` names and the read holds none of is one too, but the page draws no line
   * for it: on Conversation's own address it opens Graph at the node where Graph's read holds it,
   * and else, on Conversation's address or Graph's, that outline's own address without it
   * (`slots/body.tsx`' `addressView`). On Conversation's own address a block goes first to a view
   * that draws it. On the issue's own address, Graph's, and the Plan and Verification filters'
   * the page draws no line for a block either once one `reference-targets` read of it has
   * answered: it opens the version that last held the block, or the address without it
   * (`useUnqualifiedLastHeld`).
   */
  | {
      readonly kind: "absent";
      readonly target: Extract<AddressTarget, { kind: "block" | "children" | "node" }>;
    };

/** The reads of the issue's conversation the tree's turns come from, as the page holds them:
 *  each undefined until it has answered. */
export interface ConversationReads {
  readonly asks: readonly Pick<Ask, "id" | "kind">[] | undefined;
  readonly comments:
    | readonly Pick<Comment, "answers" | "ask_id" | "author" | "id" | "reply_to">[]
    | undefined;
  readonly messages: readonly MessageRead[] | undefined;
}

/**
 * The key of the one read the old page's landing answers `route` from (`useItemLanding`), and
 * which its `unavailable` landing's `retry` asks again: the graph read that resolves an id
 * written short (`useWholeRouteRead`), else the item's own read under its whole id.
 */
function landingReadKey(route: ItemRoute): QueryKey {
  if (isIdPrefix(route.id)) return referencesQuery(prefixReference(route)).queryKey;
  if (route.kind === "ask") return askQuery(route.id).queryKey;
  if (route.kind === "comment") return commentQuery(route.id).queryKey;
  return messageQuery(route.key, route.id).queryKey;
}

/** What one following of an address - `useFollowing`, a new one at each navigation, a
 *  link followed again to the same address among them - has seen so far
 *  (`useAddressFallback`). */
interface Following {
  /** Whether the following draws the line that the tree holds no node of the block or the
   *  children its address names (`absent`), as last stored here. A document added to the issue
   *  may hold a block but brings no child (each child is an issue, `tree-nodes.ts`' `draftChild`),
   *  so the children's line, once drawn, stays while the tree's reads are out again; a block's
   *  line goes then. */
  readonly absent: boolean;
  /** How many times the landing's read had failed as this following first read it (its
   *  `errorUpdateCount`), from which the following counts its own failures; undefined until the
   *  following reads it. */
  readonly failedBefore: number | undefined;
  /** Whether the tree has held the address's node, or the nearest drawn ancestor of a turn only
   *  agents wrote in, at any time during this following. */
  readonly found: boolean;
  /** The following: the location's key with its address (`useFollowing`). */
  readonly key: string;
  /** The landing the old page's landing settled on during this following, as last stored here,
   *  which the following draws again while the read runs again, and while the tree's reads are
   *  out again (`readsHeld`). A new one is stored when its kind changes (or another field of the
   *  following does), and a list of matches whenever the matches it lists do (`sameMatches`): a
   *  later list, or the list the cache holds once a correction is written into it
   *  (`markCorrectedPost`). So a list drawn while its read runs again is the refusal the cache
   *  held last, with every correction since, where the cache holds none then. */
  readonly settled: Exclude<ItemLanding, { kind: "pending" }> | undefined;
}

/**
 * Where `address` lands where it names a node the tree holds none of (`reveal` undefined), once
 * every read the tree is built from has answered (`readsHeld`): before that the node may not
 * have arrived yet. Undefined while the tree holds the node, while the reads are out before the
 * following has found the node or the ancestor a turn only agents wrote in lands on (`found`),
 * settled on a landing or drawn the line that the issue has no children (below), for an address
 * naming no node or opening a document, and while the old page's landing reads the item
 * (`pending`) with no landing settled since the address was followed.
 *
 * Each following of the address (`useFollowing`) is decided on its own, once: a
 * following whose tree has held the address's node, or the nearest drawn ancestor of a turn only
 * agents wrote in, draws no fallback for the rest of it, whatever the tree holds later - a block
 * deleted, the last child unlinked, the comment deleted while the reader is on its row leaves
 * the outline drawn, and the reader in it.
 *
 * An ask, a comment or a message of the outline (`outlineItemRoute`) is told from the
 * conversation's reads where it is a turn of a thread only agents wrote in, as the tree leaves
 * it out (`agentsOnlyItem`, with the asks the tree draws its nodes from: the issue's open asks
 * and the asks read), and lands on its nearest drawn ancestor (`agents-only`). Every other item
 * is read as the old page reads one (`useItemLanding`), and nothing is read for an item the tree
 * holds, or held earlier in the following. Its `turn` - an item of this issue beside no
 * document, whose page is the outline - draws the outline as it stands, which lands on the node
 * once a read draws it. A read that runs again holds no answer, so its landing is pending again
 * until that run ends - a Retry's, or window focus reading a read in error again - and the
 * following keeps drawing the kind of landing it last settled on meanwhile (`Following.settled`):
 * an item found missing keeps its line, a failed read its line, busy, with its Retry and the
 * focus, and several items their list: the refusal the cache holds for the read, where a
 * correction asked the read again (`correctRefusal` in `api/corrected-post.ts`), and else the one
 * it held last, so a match deleted since is drawn deleted. The failures are counted from the
 * following's start, so an item whose read failed before the address was followed again draws no
 * failure of its own until its read fails again.
 * A block or the children is `absent`, as no read is left to draw it.
 *
 * The reads going out again after the following has settled on a landing - a document added to
 * the issue, whose blocks are then read - leave that landing drawn, with its failures counted as
 * before: the line, its alert and the focus it holds stay, where the line leaving the page and
 * coming back once the reads answered said its failure again and took the focus again. So do the
 * landing on the nearest drawn ancestor of a turn only agents wrote in - the issue, for every
 * such turn the server stores - which the conversation's reads still tell while the blocks are
 * read: dropped then, the tree had no address to land on, and once the reads answered the
 * landing's key came back as a new landing's would, which left a zoom the reader had made after
 * the landing; and the line that the issue has no children, as a document added brings no
 * child (`Following.absent`). A block's line goes while the reads are out, as a document added
 * to the issue may hold the block. Each goes as before where the tree draws the address's node
 * (`found`), and with the following.
 */
export function useAddressFallback({
  address,
  issue,
  reads,
  readsHeld,
  reveal,
  root,
  route,
  search,
}: {
  readonly address: Pick<IssueAddress, "document" | "documentItem" | "target">;
  readonly issue: IssueDetails;
  readonly reads: ConversationReads;
  readonly readsHeld: boolean;
  readonly reveal: readonly TreeNode[] | undefined;
  /** The whole tree, which the address's node was looked for in. */
  readonly root: TreeNode;
  readonly route: IssueRoute;
  readonly search: string;
}): AddressFallback | undefined {
  const item = outlineItemRoute(route, search, address);
  // What this following has seen (`Following`), held from one render to the next, and anew for
  // each following.
  const following = useFollowing();
  const [followed, setFollowed] = useState<Following | undefined>(undefined);
  const before = followed?.key === following ? followed : undefined;
  // While the reads are out again, a following that has settled on a landing keeps its landing's
  // read, so the landing stays drawn (above); one that has found the address's node or the
  // ancestor a turn only agents wrote in lands on reads nothing (below), and only checks the item
  // against the conversation reads the page holds (`agentsOnly`), so a landing on that ancestor
  // stays drawn too.
  const unheld =
    reveal === undefined && (readsHeld || before?.settled !== undefined || before?.found === true)
      ? item
      : undefined;
  const unheldId = unheld?.id;
  const { asks, comments, messages } = reads;
  const openAsks = issue.open_asks;
  const agentsOnly = useMemo(
    () =>
      unheldId === undefined
        ? undefined
        : agentsOnlyItem({ id: unheldId, kind: "item" }, comments, messages, [
            ...(openAsks ?? []),
            ...(asks ?? []),
          ]),
    [asks, comments, messages, openAsks, unheldId]
  );
  const ancestor = useMemo(
    () =>
      agentsOnly === undefined
        ? undefined
        : agentsOnlyAncestor(root, agentsOnly, comments, messages),
    [agentsOnly, comments, messages, root]
  );
  const found = before?.found === true || reveal !== undefined || agentsOnly !== undefined;
  // The issue's own route reads nothing: the landing reads an item only where it falls back.
  const read = found ? undefined : unheld;
  const landing = useItemLanding(read ?? { key: route.key, kind: "issue" }, issue);
  const client = useQueryClient();
  const readKey = read === undefined ? undefined : landingReadKey(read);
  // The state of the one read the landing answers from, which draws the page again as it
  // changes: whether it runs again, and how many times it has failed, by the cache's own count.
  const subscribe = useCallback(
    (onChange: () => void) => client.getQueryCache().subscribe(notifyManager.batchCalls(onChange)),
    [client]
  );
  const state = useSyncExternalStore(subscribe, () =>
    readKey === undefined ? undefined : client.getQueryState(readKey)
  );
  const retrying = state?.fetchStatus === "fetching";
  const failures = state?.errorUpdateCount ?? 0;
  const failedBefore = before?.failedBefore ?? (readKey === undefined ? undefined : failures);
  const refused = landing?.kind === "pending" ? ambiguousItems(state?.error) : undefined;
  const settled: Following["settled"] =
    refused !== undefined
      ? { candidates: refused, kind: "ambiguous" }
      : landing?.kind === "pending"
        ? before?.settled
        : landing;
  const target = address.target;
  const named =
    address.document === undefined &&
    (target?.kind === "block" || target?.kind === "children" || target?.kind === "node")
      ? target
      : undefined;
  // A block or the children the tree holds no node of, once the reads are held; the children's
  // line, once drawn, stays while they are out again (`Following.absent`).
  const absent =
    named !== undefined &&
    !found &&
    (readsHeld || (named.kind === "children" && before?.absent === true));
  if (
    before === undefined ||
    before.absent !== absent ||
    before.found !== found ||
    before.failedBefore !== failedBefore ||
    before.settled?.kind !== settled?.kind ||
    !sameMatches(before.settled, settled)
  ) {
    setFollowed({ absent, failedBefore, found, key: following, settled });
  }
  if (agentsOnly !== undefined && ancestor !== undefined) {
    return { ancestor, id: agentsOnly, kind: "agents-only" };
  }
  if (found) return undefined;
  if (absent) return { kind: "absent", target: named };
  if (read === undefined || settled === undefined || settled.kind === "turn") return undefined;
  return {
    failure: failures - (failedBefore ?? failures),
    kind: "landing",
    landing: settled,
    retrying,
    route: read,
  };
}

/** Whether the landing stored and the one settled on list the same matches, item for item as the
 *  cache holds each: a correction written into the refusal they come from gives each item it marks
 *  anew (`markCorrectedPost`), as a later read gives every item. Landings that list no matches are
 *  the same here. */
function sameMatches(stored: Following["settled"], settled: Following["settled"]): boolean {
  if (stored?.kind !== "ambiguous" || settled?.kind !== "ambiguous") return true;
  const { candidates } = settled;
  return (
    stored.candidates.length === candidates.length &&
    stored.candidates.every((item, at) => item === candidates[at])
  );
}

/** The reference a `reference-targets` read is asked for the block `address` names by the
 *  explicit `#b-` fragment of the address (`hash`), pinning no version: on the Spec view's own
 *  address the spec's block, and on a document's own address that document's
 *  (`buildDispatchReference`). Undefined for every other address: one that names no block, or
 *  names it by no fragment, one that pins a version, as the address `last_held` opens does, and
 *  one whose route names no document, Graph's among them (`/graph/<node id>`, decision 456). */
export function lastHeldReference(
  route: IssueRoute,
  address: Pick<IssueAddress, "target" | "version">,
  hash: string
): string | undefined {
  const target = address.target;
  if (!hash.startsWith("#b-") || target?.kind !== "block" || address.version !== undefined) {
    return undefined;
  }
  const block = { id: target.id };
  if (route.kind === "spec") return buildDispatchReference({ block, key: route.key, kind: "spec" });
  if (route.kind !== "artifact" || route.version !== undefined) return undefined;
  return buildDispatchReference({ block, key: route.key, kind: "artifact", slug: route.slug });
}

/**
 * The reference of the block an address names by its explicit `#b-` fragment
 * (`lastHeldReference`) where the read its view draws from conclusively lacks the block (R9 and
 * R12 of INFRA-5705): the one the page then asks `reference-targets` for, to open the version
 * that last held the block (`lastHeldOpening`). Undefined for every other address, and while
 * the read does not show the block absent.
 *
 * `held` is the read the address's view draws from, with every page of it read so far: the Spec
 * view's own read once held, else the outline read, as `presetView` draws that view
 * (`useIssueTree`), and the outline read for a document's address, at which the outline is drawn
 * (`slots/body.tsx`). It shows the block absent only once it holds an answer that names no page
 * more (`nextTreeCursor`) - until then the block may stand on a page not read yet, or in an
 * answer still out - and where neither that answer (`answerHolds`) nor the tree the page draws
 * from the reads (`reveal`, `pathToTarget`) holds it, so an address of a block a read holds
 * lands as it does today.
 */
export function lastHeldAsk({
  address,
  hash,
  held,
  reveal,
  route,
}: {
  readonly address: Pick<IssueAddress, "target" | "version">;
  readonly hash: string;
  /** The read the address's view draws from; undefined while it holds no answer. */
  readonly held: TreeAnswer | undefined;
  /** The chain the tree holds down to the address's node (`pathToTarget`). */
  readonly reveal: readonly TreeNode[] | undefined;
  readonly route: IssueRoute;
}): string | undefined {
  const reference = lastHeldReference(route, address, hash);
  const target = address.target;
  if (reference === undefined || target?.kind !== "block" || reveal !== undefined) return undefined;
  if (held === undefined || nextTreeCursor(held) !== undefined) return undefined;
  return answerHolds(held, target) ? undefined : reference;
}

/** The page's own address of the version `lastHeld` names (`lastHeldPath`), else `unblocked`:
 *  the same address without its fragment, which draws no line. */
function openingOf(lastHeld: string | undefined, unblocked: string): string {
  return (lastHeld === undefined ? undefined : lastHeldPath(lastHeld)) ?? unblocked;
}

/**
 * Where an address opens that names a block the read its view draws from lacks
 * (`lastHeldAsk`), by the member `reference-targets` answered for the block: where it answers the
 * block gone, the version that last held it, at the page's own address of it (`lastHeldOf`), and
 * with no version the page can read, `unblocked` (`openingOf`). Undefined where it answers the
 * block live, or answers nothing of it: the address stands as it is, so a live block no tree read
 * holds - a plain paragraph of a document - opens as today.
 */
export function lastHeldOpening(
  member: ReferenceTarget | undefined,
  unblocked: string
): string | undefined {
  return member?.gone === true ? openingOf(lastHeldOf(member), unblocked) : undefined;
}

/**
 * Where a document's own address opens instead (`/issues/<KEY>/artifacts/<slug>#b-<id>`, at no
 * version) whose block the outline read lacks (`lastHeldAsk`), the outline being what the page
 * draws at that address (`slots/body.tsx`): the version that last held the block, or the address
 * without it, with no line; undefined while the address stands as it is.
 *
 * The page's names read (`useIssueTree`) adds a block to its set for the Spec view's own address
 * alone. Where the tree cites this one (`cites`), that read names it already, and its answer, as
 * the page publishes it (`useKnownLastHeldReferences`), opens the address: the block gone, with
 * the version that last held it or with none (`openingOf`); named live, or not answered yet, the
 * address stands, and no second read of it is sent. Where the tree cites it nowhere, this is the
 * one read of it: one `reference-targets` read of the block's reference alone
 * (`referenceTargetsQuery`), sent once and never again for that address, which its answer
 * replaces (`lastHeldOpening`). The address it opens pins a version and asks nothing. While the
 * read is out, or where it is refused, the address stands.
 */
export function useLastHeldAddress({
  address,
  cites,
  held,
  reveal,
  route,
}: {
  readonly address: Pick<IssueAddress, "target" | "version">;
  /** Whether the tree cites `reference`, which the page's names read then names. */
  readonly cites: (reference: string) => boolean;
  /** The outline read, every page of it read so far; undefined while it holds no answer, or
   *  while a read the tree is built from is out. */
  readonly held: TreeAnswer | undefined;
  /** The chain the tree holds down to the address's node (`useAddressTarget`). */
  readonly reveal: readonly TreeNode[] | undefined;
  readonly route: IssueRoute;
}): string | undefined {
  const { hash, pathname, search } = useLocation();
  const history = useKnownLastHeldReferences();
  const asked =
    route.kind === "artifact" ? lastHeldAsk({ address, hash, held, reveal, route }) : undefined;
  const cited = asked !== undefined && cites(asked);
  const answer = useQuery({
    ...referenceTargetsQuery(asked === undefined || cited ? [] : [asked]),
    enabled: asked !== undefined && !cited,
  }).data;
  const unblocked = `${pathname}${search}`;
  if (asked === undefined) return undefined;
  if (cited) return history.has(asked) ? openingOf(history.get(asked), unblocked) : undefined;
  if (answer === undefined) return undefined;
  return lastHeldOpening(
    answer.targets.find((one) => one.ref === asked),
    unblocked
  );
}

/** The routes whose `#b-` fragment names its block in no document (the INFRA-5705 architect's
 *  decision on plan task t12): the issue's own address, Graph's, zoomed to a node or not, and the
 *  Plan and Verification filters'. An address of any other route, the Decisions filter's among
 *  them, keeps the outline's line where no row stands for its block (`slots/body.tsx`). */
const UNQUALIFIED_ROUTES: Readonly<Partial<Record<IssueRoute["kind"], true>>> = {
  graph: true,
  issue: true,
  plan: true,
  verification: true,
};

/**
 * The first of `references`, in their order, that one `reference-targets` request carries: at
 * most `REFERENCE_TARGETS_MAX`, the server's cap, and at most `REFERENCE_TARGETS_MAX_QUERY_CHARS`
 * characters of query string, each reference written as the client writes it (`ref=<encoded>`,
 * joined by `&`) - the two bounds `referenceTargetsQuery` splits a set into requests by. The first
 * is kept whatever its length, as that query sends one address past the budget alone. What a set
 * writes does not hang on its order, so the set that query sorts is one request too.
 */
export function withinOneRead(references: readonly string[]): readonly string[] {
  const kept: string[] = [];
  let written = 0;
  for (const ref of references) {
    const length = new URLSearchParams({ ref }).toString().length;
    if (
      kept.length > 0 &&
      (kept.length === REFERENCE_TARGETS_MAX ||
        written + 1 + length > REFERENCE_TARGETS_MAX_QUERY_CHARS)
    ) {
      break;
    }
    written += (kept.length > 0 ? 1 : 0) + length;
    kept.push(ref);
  }
  return kept;
}

/**
 * What the page asks `reference-targets` for a block an address names by a `#b-` fragment of no
 * document (`unqualifiedAsk`): the block in each document of the issue it can read, each once and
 * in the form the server reads (`buildDispatchReference`), the spec's as `spec`, any other's
 * under `artifact/<slug>`. The spec, the Verification document and the plan (`roleDocument`)
 * come first, then the issue's tree artifact, the one the tree read names (`tree`), then each
 * other document (an artifact of kind `doc`) in the issue's order, so the ones `withinOneRead`
 * cuts past the bounds of one read are the last.
 */
export function blockReferences({
  block,
  issue,
  key,
  tree,
}: {
  /** The block's id, as the fragment names it. */
  readonly block: string;
  readonly issue: Pick<
    IssueDetails,
    "artifacts" | "plan_artifact_id" | "primary_artifact_id" | "verification_artifact_id"
  >;
  readonly key: string;
  /** The id of the issue's tree artifact, as the tree read names it: null where the issue has
   *  not converted. */
  readonly tree: string | null | undefined;
}): readonly string[] {
  const target = { id: block };
  const artifacts = issue.artifacts ?? [];
  const spec = primarySpec(issue);
  const inDocument = (document: Pick<Artifact, "slug"> | undefined): string | undefined =>
    document === undefined
      ? undefined
      : buildDispatchReference({ block: target, key, kind: "artifact", slug: document.slug });
  const treeArtifact =
    tree === null || tree === undefined
      ? undefined
      : artifacts.find((artifact) => artifact.id === tree);
  const ordered = [
    spec === undefined ? undefined : buildDispatchReference({ block: target, key, kind: "spec" }),
    inDocument(roleDocument(issue, "verification")),
    inDocument(roleDocument(issue, "plan")),
    inDocument(treeArtifact),
    ...artifacts
      .filter((artifact) => artifact.kind === "doc" && artifact.id !== spec?.id)
      .map(inDocument),
  ];
  return withinOneRead([...new Set(ordered.filter((ref): ref is string => ref !== undefined))]);
}

/**
 * The references the page asks `reference-targets` for once where an address names, by a `#b-`
 * fragment of no document, a block no read of the page holds (R9 and R12 of INFRA-5705, the
 * architect's decision on plan task t12): its route is one of `UNQUALIFIED_ROUTES`, it pins no
 * version, each read the address's view draws from (`held`) holds an answer that names no page
 * more (`nextTreeCursor`), and neither those nor any other read the page holds (`others`) holds
 * the block (`answerHolds`). The references are the block in each document of the issue
 * (`blockReferences`), none where the issue has none. Undefined for every other address, and
 * while a read the view draws from is out, refused or has a page left, so a block a read holds,
 * or may hold, lands as it does today.
 */
export function unqualifiedAsk({
  address,
  hash,
  held,
  issue,
  others,
  route,
}: {
  readonly address: Pick<IssueAddress, "target" | "version">;
  readonly hash: string;
  /** The reads the address's view draws from, each with every page of it read so far, the
   *  outline read first: that read alone on the issue's own address and Graph's, and the
   *  filter's own view read beside it on the Plan and Verification filters'. Each undefined
   *  while it holds no answer. */
  readonly held: readonly [TreeAnswer | undefined, ...(TreeAnswer | undefined)[]];
  readonly issue: Parameters<typeof blockReferences>[0]["issue"];
  /** Every other read of the tree the page holds: each filter's view answer, and Graph's read. */
  readonly others: readonly (TreeAnswer | undefined)[];
  readonly route: IssueRoute;
}): readonly string[] | undefined {
  const target = address.target;
  if (
    UNQUALIFIED_ROUTES[route.kind] !== true ||
    !hash.startsWith("#b-") ||
    target?.kind !== "block" ||
    target.document !== undefined ||
    address.version !== undefined
  ) {
    return undefined;
  }
  if (held.some((answer) => answer === undefined || nextTreeCursor(answer) !== undefined)) {
    return undefined;
  }
  if ([...held, ...others].some((answer) => answerHolds(answer, target))) return undefined;
  return blockReferences({ block: target.id, issue, key: route.key, tree: held[0]?.tree });
}

/**
 * The one version the members `targets` of a `reference-targets` read last held the block in,
 * at the page's own address of it: each member of a reference in `asked` that answers its block
 * gone with a version the page opens (`lastHeldOf`, `lastHeldPath`). Undefined where none does,
 * and where they open more than one address: the page does not choose between them.
 */
export function lastHeldDestination(
  targets: readonly ReferenceTarget[],
  asked: readonly string[]
): string | undefined {
  const opened = new Set<string>();
  for (const member of targets) {
    const lastHeld = asked.includes(member.ref) ? lastHeldOf(member) : undefined;
    const path = lastHeld === undefined ? undefined : lastHeldPath(lastHeld);
    if (path !== undefined) opened.add(path);
  }
  return opened.size === 1 ? [...opened][0] : undefined;
}

/** Where an address opens instead that names, by a `#b-` fragment of no document, a block no read
 *  holds (`useUnqualifiedLastHeld`), and whether the read that tells is out. */
export interface UnqualifiedOpening {
  /** The address the page opens in place of the address; undefined where it stands. */
  readonly opened: string | undefined;
  /** Whether the one `reference-targets` read of the block is out, which the outline's landing
   *  and its line that no row stands for the block wait on. */
  readonly out: boolean;
}

/** The address stands, and no read of it is out. */
const STANDS: UnqualifiedOpening = { opened: undefined, out: false };

/** The address stands while its read is out. */
const ASKING: UnqualifiedOpening = { opened: undefined, out: true };

/**
 * Where an address on the issue's own address, Graph's, or the Plan or Verification filter's
 * opens whose `#b-` fragment names a block no read holds (R9 and R12 of INFRA-5705, the
 * architect's decision on plan task t12), once the outline would say in its place that no row
 * stands for it (`absent`): `useAddressFallback`'s `absent` for the block, with every page of its
 * reads read (`useReadUntilHeld`), and no view the page holds drawing the block (`addressView`).
 *
 * The page asks one `reference-targets` read of the block in each document of the issue
 * (`unqualifiedAsk`) and opens the one version its own answer names (`lastHeldDestination`),
 * read once that read has settled, never the names the page published from another read. Where
 * the answer names none, or versions of more than one address, and where the issue holds no
 * document to ask of, the address opens without its fragment, with no line. While the read is out
 * the address stands (`out`); where it is refused, the address stands as today, with the line.
 * The address it opens pins a version or names no block, and asks nothing more.
 */
export function useUnqualifiedLastHeld({
  absent,
  address,
  issue,
  others,
  outline,
  route,
}: {
  /** Whether the outline would say in its place that no row stands for the address's block. */
  readonly absent: boolean;
  readonly address: Pick<IssueAddress, "target" | "version">;
  readonly issue: Parameters<typeof blockReferences>[0]["issue"];
  /** Every other read of the tree the page holds (`unqualifiedAsk`). */
  readonly others: readonly (TreeAnswer | undefined)[];
  /** The outline read the body draws, every page of it read so far (`useTreeRead`). */
  readonly outline: TreeAnswer | undefined;
  readonly route: IssueRoute;
}): UnqualifiedOpening {
  const { hash, pathname, search } = useLocation();
  // On the Plan and Verification filters' addresses the filter's own view read is one the address
  // is looked up in too, watched under the key `ViewReadProvider` reads it by and never sent.
  const filter = route.kind === "plan" || route.kind === "verification" ? route.kind : undefined;
  const view = useQuery({
    ...viewQuery(route.key, filter ?? TREE_FILTERS[0]),
    enabled: false,
  }).data;
  const asked = absent
    ? unqualifiedAsk({
        address,
        hash,
        held: filter === undefined ? [outline] : [outline, view],
        issue,
        others,
        route,
      })
    : undefined;
  const lookup = useQuery({
    ...referenceTargetsQuery(asked ?? []),
    enabled: asked !== undefined && asked.length > 0,
  });
  if (asked === undefined) return STANDS;
  const unblocked = `${pathname}${search}`;
  if (asked.length === 0) return { opened: unblocked, out: false };
  if (lookup.isError) return STANDS;
  if (!lookup.isSuccess || lookup.fetchStatus !== "idle") return ASKING;
  return { opened: lastHeldDestination(lookup.data.targets, asked) ?? unblocked, out: false };
}
