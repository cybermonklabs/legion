import { isIdPrefix } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";

import { ApiError, ambiguousItems } from "../../api/client";
import type { IDCandidate } from "../../api/types";
import { askQuery, useWholeRouteRead } from "../refs/reference-target";
import { buildIssuePath, type DispatchReferenceRoute, type IssueRoute } from "../refs/routes";

/** The item routes: `/issues/KEY/asks|comments|messages/<id>`. */
export type ItemRoute = Extract<IssueRoute, { id: string }>;

/**
 * Where an ask's own address (`/issues/KEY/asks/<id>`) lands.
 *
 * - `node` — the ask is this issue's: the page opens the outline on its node, focused,
 *   wherever the ask is anchored, and never the document it sits in (dispatch://INFRA-1196
 *   R20). What the address opens is the address's own to say (`issue-address.ts`).
 * - `approval` — the ask is an approval request, which the outline draws no node for: the
 *   identity block draws the approval of the document it reviews, and the page puts the focus
 *   on that entry there (`artifactId`).
 * - `redirect` — the ask belongs to another issue, whose own address lands it.
 * - `rewrite` — the address wrote the id short and it named one item: the page replaces the
 *   address with the whole id, and reads the ask under that (`useItemLanding`).
 * - `ambiguous` — the address wrote the id short and it names several items, which the server
 *   listed in its refusal; the page shows them for the reader to choose between.
 * - `missing` — the id names nothing openable: the server rejected it as malformed (400) or has
 *   no such ask (404). Either way retrying cannot help; the link is dead.
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

/**
 * Resolves an ask's own address against the ask itself, for the stage, which waits for the
 * ask's answer before it draws the outline: an approval request is drawn at the issue's own
 * address, under the header that holds its entry (`stage-landing.tsx`). `undefined` for every
 * other issue route. A comment's or a message's own address, its id whole or written short, is
 * the outline's own landing (`slots/body.tsx`'s `useAddressFallback`, which reads the item
 * through `issue/useItemLanding.ts`), so nothing here reads a comment or a message by its id, nor
 * resolves the short id of either.
 *
 * An address whose id is a whole uuid is read exactly as it always was: the ask's own read under
 * that id, and nothing else. Only an id written short (`isIdPrefix`) is resolved first, by the
 * one graph read every surface resolves a short id through (`useWholeRouteRead`), which the
 * server resolves within the issue the address names; the ask is then read under the whole id
 * the answer carries, so nothing is ever read or cached under a prefix. One match replaces the
 * address with the whole id (`rewrite`), a prefix the server refused as ambiguous lists its
 * matches, and one that names nothing is the 404 a whole id that names nothing already is.
 *
 * The gate is on the ask's own id, not on `prefixReference`, because an ask route also carries a
 * block: an ask whose id is whole and whose `#b-` id is short is this ask's landing, read under
 * its own whole id, and resolving it as an item prefix would rewrite - or 404 - a link that
 * already names the ask exactly.
 */
export function useItemLanding(route: IssueRoute): ItemLanding | undefined {
  const address = route.kind === "ask" ? route : undefined;
  const short = address !== undefined && isIdPrefix(address.id) ? address : undefined;
  const read = useWholeRouteRead(short);
  const { prefix, resolution } = read;
  // The route the address names with its id written whole: the address's own route where it
  // wrote it whole, and the graph read's answer where it wrote it short. An answer naming a
  // record of another kind, or none, leaves it undefined, and the landing below is the 404.
  const answered: DispatchReferenceRoute | undefined = prefix === undefined ? address : read.route;
  const whole =
    answered !== undefined && "key" in answered && isItemRoute(answered) ? answered : undefined;
  // The ask's own read waits for the whole id: a prefix is never a key anything is cached under.
  const id = whole?.id;
  const ask = useQuery({ ...askQuery(id), enabled: address !== undefined && id !== undefined });

  if (address === undefined) {
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
    // again on the route that id makes, which reads the ask as any whole-id address does.
    return whole === undefined
      ? { kind: "missing" }
      : { kind: "rewrite", to: buildIssuePath(whole) };
  }
  if (ask.isPending) {
    return { kind: "pending" };
  }
  if (ask.isError) {
    const dead =
      ask.error instanceof ApiError && (ask.error.status === 404 || ask.error.status === 400);
    return dead ? { kind: "missing" } : { kind: "unavailable", retry: () => void ask.refetch() };
  }

  const item = ask.data?.ask;
  if (item === undefined) {
    return { kind: "missing" };
  }
  if (item.issue_key !== address.key) {
    // A pasted or stale link under the wrong issue: send the reader to the issue that owns the
    // ask, which resolves the ask again from there. An ask with no issue is not reachable under
    // an issue route at all.
    return item.issue_key === null
      ? { kind: "missing" }
      : { kind: "redirect", to: buildIssuePath({ ...address, key: item.issue_key }) };
  }
  const reviewed = item.approval?.artifact_id;
  return reviewed === undefined ? { kind: "node" } : { artifactId: reviewed, kind: "approval" };
}
