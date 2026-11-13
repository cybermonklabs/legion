import { decodeBlockId, itemFromSearch } from "@legion/contracts";

import { buildIssuePath, type IssueRoute } from "../refs/routes";
import type { TreeNode } from "./issue-tree";
import type { ConversationState, Preset } from "./page-words";
import { blockNodeId } from "./tree-drafts";

/**
 * What one address of the issue page says (dispatch://INFRA-1196 R19,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t9`).
 *
 * Every address the old page's nine tabs answered still resolves, and this module is the one
 * place that says what each one opens: which preset, which node the page unfolds the path to
 * and puts the focus on, which state the conversation is in, whether the node menu is open, and
 * which version of the preset's document is pinned. `refs/routes.ts` still parses the address;
 * this reads the route it answers.
 *
 * It is pure and holds no React, so the page, the margin's panel and the unit tests all read an
 * address the same way.
 */

/** The node an address names, before the tree is there to resolve it against. */
export type AddressTarget =
  /** One block, by the block id a `#b-` fragment or a route's block target holds, and the slug
   *  of the document holding it where the address names one: a block id is unique inside its
   *  document alone, and an issue carries several (`tree-drafts.ts`'s `blockNodeId`). An
   *  address that names no document - a `#b-` fragment on the issue's own address - leaves it
   *  undefined, and the first node carrying that block id answers. */
  | { readonly kind: "block"; readonly id: string; readonly document?: string }
  /** One ask, comment or message a preset's own address names beside it (`?ask=`, `?comment=`),
   *  by its own id: the tree is what that address opens, and the item is a node of it. */
  | { readonly kind: "item"; readonly id: string }
  /** One ask, comment or message a conversation address names by its own route
   *  (`/asks/<id>`, `/comments/<id>`, `/messages/<id>`). It is the same node of the tree, and
   *  the page unfolds the path to it as it does for any target; what differs is the landing.
   *  That address opens the conversation (`refs/routes.ts`'s `issueTabForRoute`), which places
   *  the reader on the entry's own turn and marks it `aria-current`, so the tree reveals its
   *  row and leaves the focus alone: a row focused there scrolls the page back off that turn,
   *  and on a phone it is what the comment's thread restores the focus to when the reader
   *  closes it. */
  | { readonly kind: "turn"; readonly id: string }
  /** One node by its own id inside the issue, which is how a document is named by its slug. */
  | { readonly kind: "node"; readonly id: string };

/** What one address of the issue page opens. */
export interface IssueAddress {
  /** The conversation's state the address names, or undefined for one that names none, where
   *  the panel opens in the state the reader kept (`IssuePage`, R13). */
  readonly conversation: ConversationState | undefined;
  /** What `?q=` narrows the preset to; `""` for an address that narrows nothing. */
  readonly filter: string;
  /** Whether the address opens the node menu, which `/artifacts` does (R16). */
  readonly nodeMenu: boolean;
  readonly preset: Preset;
  /** The node the address names, which the page unfolds the path to and focuses (R19). */
  readonly target: AddressTarget | undefined;
  /** The version of the preset's document the address pins (`?v=`). */
  readonly version: number | undefined;
}

/** Which preset each kind of route opens. Everything not named here is the Tree preset, which
 *  is the issue's own address. */
const ROUTE_PRESETS: Readonly<Record<string, Preset>> = {
  decisions: "decisions",
  plan: "plan",
  spec: "spec",
  verification: "checks",
};

/** The address of `preset` on `key`, which is the address its own tab carries (R19): the Checks
 *  preset keeps `/verification` and the Tree preset is the issue's own address. */
export function presetPath(key: string, preset: Preset): string {
  if (preset === "tree") return buildIssuePath({ key, kind: "issue" });
  if (preset === "checks") return buildIssuePath({ key, kind: "verification" });
  return buildIssuePath({ key, kind: preset });
}

/** The conversation's state each address names. `/activity` and `/conversation` are a state of
 *  their own, and an address naming one entry of the conversation names what people said,
 *  because the entry is in that list whatever the reader kept: a kept Questions or Pinned drew
 *  the conversation without the comment or message a link was sent to (R19). Every other
 *  address names none. */
const ROUTE_CONVERSATION_STATES: Readonly<Partial<Record<IssueRoute["kind"], ConversationState>>> =
  {
    activity: "everything",
    ask: "words",
    comment: "words",
    conversation: "words",
    message: "words",
  };

/**
 * The address picking `state` opens from `route`, or undefined where picking it opens no
 * address and the page holds the state for the address the reader is on.
 *
 * Two of the four states are addresses of their own, and the page draws more at them than the
 * panel does: `/activity` puts the issue's whole history in the page's column. Picking one of
 * those is navigation only from the other of the two, which is all either can stand in for:
 * every other address opens something besides the conversation - a preset's document, the
 * spec the issue's own address draws beside the tree, the children, the collaborators, one
 * entry. Picking a state there would drop the preset, the `?q=`, the `#b-` and the document the
 * reader has open, so there the state is the panel's alone (R19; picking Questions used to send
 * a reader of the Spec preset to the bare `/conversation`, and picking Words or Everything on
 * the issue's own address still closed its spec).
 */
export function conversationStatePath(
  route: IssueRoute,
  state: ConversationState
): string | undefined {
  const kind = state === "everything" ? "activity" : state === "words" ? "conversation" : undefined;
  if (kind === undefined) return undefined;
  if (route.kind !== "activity" && route.kind !== "conversation") return undefined;
  return buildIssuePath({ key: route.key, kind });
}

/** A positive integer `?v=` holds, or undefined for anything else. */
function pinnedVersion(search: string): number | undefined {
  const value = new URLSearchParams(search).get("v") ?? "";
  return /^[1-9]\d*$/.test(value) ? Number(value) : undefined;
}

/** The node `route` and `search` name, outside of a `#b-` fragment. */
function routeTarget(route: IssueRoute): AddressTarget | undefined {
  if (route.kind === "ask" || route.kind === "comment" || route.kind === "message") {
    return { id: route.id, kind: "turn" };
  }
  if (route.kind === "artifact") {
    const block = "block" in route ? route.block : undefined;
    return block === undefined
      ? { id: route.slug, kind: "node" }
      : { document: route.slug, id: block.id, kind: "block" };
  }
  if (route.kind === "spec") {
    const block = "block" in route ? route.block : undefined;
    return block === undefined ? undefined : { id: block.id, kind: "block" };
  }
  return undefined;
}

/**
 * What the address `route` + `search` + `hash` opens. `isPrimaryDocument` says the route names
 * the issue's own spec by its slug (`/artifacts/<spec>`), which is the Spec preset's address as
 * much as `/spec` is. The fragment outranks the route's own
 * target, because a reader who wrote `#b-` meant that block; `?ask=` and `?comment=` outrank
 * both, because they are what a deep link into a question or a comment carries.
 */
export function issueAddress(
  route: IssueRoute,
  search: string,
  hash: string,
  isPrimaryDocument: boolean
): IssueAddress {
  const query = new URLSearchParams(search);
  const item = itemFromSearch(search);
  // `decodeBlockId`, not `decodeURIComponent`: a fragment holding a stray percent (`#b-50%`)
  // throws a `URIError`, and this runs in a render, so a bare decode would put the whole page
  // in its error boundary over one unreadable address. A fragment that reads as no block id
  // names no block, and the address falls back to what its route names, which for a document's
  // address is that document. The block it does name belongs to the document the route names,
  // where it names one: a block id is unique inside its own document alone.
  const block = hash.startsWith("#b-") ? decodeBlockId(hash.slice(3)) : undefined;
  const named: AddressTarget | undefined =
    block === undefined
      ? routeTarget(route)
      : {
          document: route.kind === "artifact" ? route.slug : undefined,
          id: block,
          kind: "block",
        };
  return {
    conversation: ROUTE_CONVERSATION_STATES[route.kind],
    filter: query.get("q") ?? "",
    nodeMenu: route.kind === "artifacts",
    preset: isPrimaryDocument ? "spec" : (ROUTE_PRESETS[route.kind] ?? "tree"),
    target: item === null || item === undefined ? named : { id: item.id, kind: "item" },
    version: pinnedVersion(search) ?? (route.kind === "artifact" ? route.version : undefined),
  };
}

/** The chain from `root` down to the first node `wanted` answers for, the root first, or
 *  undefined when the tree holds no such node. */
function pathToNode(
  root: TreeNode,
  wanted: (node: TreeNode) => boolean
): readonly TreeNode[] | undefined {
  if (wanted(root)) return [root];
  for (const child of root.children) {
    const below = pathToNode(child, wanted);
    if (below !== undefined) return [root, ...below];
  }
  return undefined;
}

/**
 * The chain from the issue down to the node `target` names, the issue first. An ask is the
 * block the server indexed it from where it has one, so an item is looked up by its node id
 * first and by the `dispatch://` address of that item second.
 *
 * A block is looked up inside the document its address names, since a node's id is
 * `<slug>#b-<id>` (`tree-drafts.ts`'s `blockNodeId`) and a block id is the author's, unique
 * inside one document alone: `/issues/KEY/artifacts/plan-md#b-t1` would otherwise land on the
 * spec's own `#b-t1`. An address naming no document - a `#b-` fragment on the issue's own
 * address - has only the block id to go on, and the first node carrying it answers.
 */
export function pathToTarget(
  root: TreeNode,
  target: AddressTarget | undefined
): readonly TreeNode[] | undefined {
  if (target === undefined) return undefined;
  if (target.kind === "block") {
    if (target.document !== undefined) {
      const id = blockNodeId(target.document, target.id);
      return pathToNode(root, (node) => node.id === id);
    }
    const suffix = `#b-${target.id}`;
    return pathToNode(root, (node) => node.id.endsWith(suffix));
  }
  const byId = pathToNode(root, (node) => node.id === target.id);
  if (byId !== undefined || target.kind === "node") return byId;
  const address = `/${target.id}`;
  return pathToNode(root, (node) => node.reference?.endsWith(address) === true);
}
