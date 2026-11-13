import { ID_AMBIGUOUS, isIdPrefix } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";

import { ApiError, type ErrorCandidate } from "../../api/client";
import type { IDCandidate } from "../../api/types";
import { askQuery, commentQuery, messageQuery, useWholeRouteRead } from "../refs/reference-target";
import { buildIssuePath, type DispatchReferenceRoute, type IssueRoute } from "../refs/routes";

/** The item routes: `/issues/KEY/asks|comments|messages/<id>`. */
export type ItemRoute = Extract<IssueRoute, { id: string }>;

/**
 * Where an item deep link lands.
 *
 * - `node` — the item is this issue's: the page opens the outline on its node, focused,
 *   wherever the item is anchored, and never the document it sits in (dispatch://INFRA-1196
 *   R20). What the address opens is the address's own to say (`issue-address.ts`).
 * - `approval` — the item is an approval request, which the outline draws no node for: the
 *   identity block draws the approval of the document it reviews, and the page puts the focus
 *   on that entry there (`artifactId`).
 * - `redirect` — the item belongs to another issue, whose own address lands it.
 * - `rewrite` — the address wrote the id short and it named one item: the page replaces the
 *   address with the whole id, and reads the item under that (`useItemLanding`).
 * - `ambiguous` — the address wrote the id short and it names several items, which the server
 *   listed in its refusal; the page shows them for the reader to choose between.
 * - `missing` — the id names nothing openable: the server rejected it as malformed (400) or has
 *   no such item (404). Either way retrying cannot help; the link is dead.
 * - `unavailable` — the lookup itself failed; the reader can retry it.
 */
export type ItemLanding =
  | { kind: "pending" }
  | { kind: "node" }
  | { kind: "approval"; artifactId: string }
  | { kind: "redirect"; to: string }
  | { kind: "rewrite"; to: string }
  | { kind: "ambiguous"; candidates: readonly IDCandidate[] }
  | { kind: "missing" }
  | { kind: "unavailable"; retry: () => void };

export function isItemRoute(route: IssueRoute): route is ItemRoute {
  return route.kind === "ask" || route.kind === "comment" || route.kind === "message";
}

/** The items an `ID_AMBIGUOUS` refusal listed, or undefined for any other failure and for a
 *  refusal that listed none, which there is no list to show for. The code is what says the
 *  entries are these and not a duplicate check's issues (`ErrorCandidate`). */
export function ambiguousItems(error: unknown): readonly IDCandidate[] | undefined {
  if (!(error instanceof ApiError) || error.code !== ID_AMBIGUOUS) {
    return undefined;
  }
  const items = error.candidates?.filter(isIDCandidate) ?? [];
  return items.length === 0 ? undefined : items;
}

function isIDCandidate(candidate: ErrorCandidate): candidate is IDCandidate {
  return "id" in candidate && "kind" in candidate;
}

/**
 * Resolves an item deep link against the item itself. `undefined` for every other issue route.
 *
 * An address whose id is a whole uuid is read exactly as it always was: the item's own read
 * under that id, and nothing else. Only an id written short (`isIdPrefix`) is resolved first, by
 * the one graph read every surface resolves a short id through (`useWholeRouteRead`), which the
 * server resolves within the issue the address names; the item is then read under the whole id
 * the answer carries, so nothing is ever read or cached under a prefix. One match replaces the
 * address with the whole id (`rewrite`), a prefix the server refused as ambiguous lists its
 * matches, and one that names nothing is the 404 a whole id that names nothing already is.
 *
 * The gate is on the item's own id, not on `prefixReference`, because an item route also carries
 * a block: an item whose id is whole and whose `#b-` id is short is this item's landing, read
 * under its own whole id, and resolving it as an item prefix would rewrite - or 404 - a link
 * that already names the item exactly.
 */
export function useItemLanding(route: IssueRoute): ItemLanding | undefined {
  const address = isItemRoute(route) ? route : undefined;
  const short = address !== undefined && isIdPrefix(address.id) ? address : undefined;
  const read = useWholeRouteRead(short);
  const { prefix, resolution } = read;
  // The route the address names with its id written whole: the address's own route where it
  // wrote it whole, and the graph read's answer where it wrote it short. An answer naming a
  // record of another kind, or none, leaves it undefined, and the landing below is the 404.
  const answered: DispatchReferenceRoute | undefined = prefix === undefined ? address : read.route;
  const whole =
    answered !== undefined && "key" in answered && isItemRoute(answered) ? answered : undefined;
  // The item's own read waits for the whole id: a prefix is never a key anything is cached
  // under.
  const id = whole?.id;
  const comment = useQuery({
    ...commentQuery(id),
    enabled: route.kind === "comment" && id !== undefined,
  });
  const ask = useQuery({ ...askQuery(id), enabled: route.kind === "ask" && id !== undefined });
  const message = useQuery({
    ...messageQuery(route.key, id),
    enabled: route.kind === "message" && id !== undefined,
  });

  if (!isItemRoute(route)) {
    return undefined;
  }
  if (prefix !== undefined) {
    if (resolution.isPending) {
      return { kind: "pending" };
    }
    if (resolution.isError) {
      const candidates = ambiguousItems(resolution.error);
      if (candidates !== undefined) {
        return { candidates, kind: "ambiguous" };
      }
      const dead =
        resolution.error instanceof ApiError &&
        (resolution.error.status === 404 || resolution.error.status === 400);
      return dead
        ? { kind: "missing" }
        : { kind: "unavailable", retry: () => void resolution.refetch() };
    }
    // The answer named one item: the address is rewritten to its whole id, and the page lands
    // again on the route that id makes, which reads the item as any whole-id address does.
    return whole === undefined
      ? { kind: "missing" }
      : { kind: "rewrite", to: buildIssuePath(whole) };
  }
  // One query per route kind, and the item it returns.
  const { item, query } =
    route.kind === "comment"
      ? { item: comment.data?.comment, query: comment }
      : route.kind === "ask"
        ? { item: ask.data?.ask, query: ask }
        : { item: message.data?.message, query: message };
  if (query.isPending) {
    return { kind: "pending" };
  }
  if (query.isError) {
    const dead =
      query.error instanceof ApiError && (query.error.status === 404 || query.error.status === 400);
    return dead ? { kind: "missing" } : { kind: "unavailable", retry: () => void query.refetch() };
  }

  if (item === undefined) {
    return { kind: "missing" };
  }
  if (item.issue_key !== route.key) {
    // A pasted or stale link under the wrong issue: send the reader to the issue that owns the
    // item, which resolves the item again from there. An item with no issue is not reachable
    // under an issue route at all.
    return item.issue_key === null
      ? { kind: "missing" }
      : { kind: "redirect", to: buildIssuePath({ ...route, key: item.issue_key }) };
  }
  const reviewed = route.kind === "ask" ? ask.data?.ask.approval?.artifact_id : undefined;
  return reviewed === undefined ? { kind: "node" } : { artifactId: reviewed, kind: "approval" };
}
