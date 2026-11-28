import { isIdPrefix, itemFromSearch } from "@legion/contracts";
import { notifyManager, type QueryKey, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { useNavigate } from "react-router-dom";

import { ambiguousItems } from "../../api/client";
import type { Ask, Comment, IssueDetails, MessageRead } from "../../api/types";
import {
  type ItemLanding,
  type ItemRoute,
  isItemRoute,
  useItemLanding,
} from "../issue/useItemLanding";
import { askQuery, commentQuery, messageQuery, referencesQuery } from "../refs/reference-target";
import { buildIssuePath, type IssueRoute, parseIssuePath } from "../refs/routes";
import { prefixReference } from "../refs/text-references";
import {
  type AddressTarget,
  agentsOnlyItem,
  type IssueAddress,
  pathToTarget,
} from "./issue-address";
import type { TreeNode } from "./issue-tree";
import { useFollowing } from "./slots/following";
import { type EntryFocus, messageRecord } from "./threads/thread-address";

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
 * Every target an address names, `/children` among them, is one a tree unfolds and focuses.
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
 * (`in_reply_to`) - where the tree holds it, else that entry's own, as `tree-drafts.ts`'s
 * `addTurns` places each turn. Where the tree holds none of them - the head of such a thread
 * sits under the issue, and an approval request is no node (`addAsks`) - it is the issue alone.
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
   */
  | {
      readonly kind: "absent";
      readonly target: Extract<AddressTarget, { kind: "block" | "children" }>;
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
   *  children its address names (`absent`), as last stored here. The children come with the
   *  issue's own read alone (`tree-drafts.ts`'s `addChildren`), so the children's line, once
   *  drawn, stays while the tree's reads are out again; a block's line goes then, since a
   *  document added to the issue may hold the block. */
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
 * A message of a thread between agents or of an agent's note, which no view draws on this part's
 * base, opens in Threads instead, with its record open and the message focused
 * (dispatch://INFRA-5704/spec#b-r-addresses, R12, `messageRecord`): the address moves to the
 * record's own, in place of the message's in the history, with the message to focus in its state
 * (`EntryFocus`), and the outline draws no fallback for it meanwhile. A message of a thread the
 * Legion daemon began lands as on `main`.
 *
 * The reads going out again after the following has settled on a landing - a document added to
 * the issue, whose blocks are then read - leave that landing drawn, with its failures counted as
 * before: the line, its alert and the focus it holds stay, where the line leaving the page and
 * coming back once the reads answered said its failure again and took the focus again. So do the
 * landing on the nearest drawn ancestor of a turn only agents wrote in - the issue, for every
 * such turn the server stores - which the conversation's reads still tell while the blocks are
 * read: dropped then, the tree had no address to land on, and once the reads answered the
 * landing's key came back as a new landing's would, which left a zoom the reader had made after
 * the landing; and the line that the issue has no children, which come with the issue's own read
 * alone (`Following.absent`). A block's line goes while the reads are out, as a document added
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
  // The conversation's messages alone tell whether Threads opens the message, so the address moves
  // as soon as they answer, before the rest of the tree's reads have.
  const threadsMessage = item?.kind === "message" && reveal === undefined ? item.id : undefined;
  const threadsRecord = useMemo(
    () =>
      threadsMessage === undefined
        ? undefined
        : messageRecord(threadsMessage, messages, issue.project),
    [issue.project, messages, threadsMessage]
  );
  const navigate = useNavigate();
  useEffect(() => {
    if (threadsMessage === undefined || threadsRecord === undefined) return;
    const state: EntryFocus = { threadEntry: threadsMessage };
    navigate(buildIssuePath({ key: issue.key, kind: "threads", record: threadsRecord }), {
      replace: true,
      state,
    });
  }, [issue.key, navigate, threadsMessage, threadsRecord]);
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
    address.document === undefined && (target?.kind === "block" || target?.kind === "children")
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
  if (threadsRecord !== undefined) return undefined;
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
