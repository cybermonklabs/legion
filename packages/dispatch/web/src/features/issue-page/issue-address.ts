import { type DispatchHrefItem, decodeBlockId, itemFromSearch } from "@legion/contracts";

import type { Ask, Comment, MessageRead } from "../../api/types";
import { buildIssuePath, type IssueRoute } from "../refs/routes";
import { agentsOnlyIds } from "./agents-only";
import type { TreeNode } from "./issue-tree";
import type { Preset } from "./page-words";
import { blockNodeId } from "./tree-drafts";

/**
 * What one address of the issue page says.
 *
 * Every address the old page's nine tabs answered still resolves, and this module is the one
 * place that says what each one opens: which preset, which document is opened in place of the
 * outline, which node the page unfolds the path to and puts the focus on, whether Documents' list
 * is drawn in its place, and which version is pinned. `refs/routes.ts` still parses the address;
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
  /** One ask, comment or message, by its own id: the one a preset's own address names beside
   *  it (`?ask=`, `?comment=`), and the one its own route names (`/asks/<id>`, `/comments/<id>`,
   *  `/messages/<id>`). It is a node of the tree, which the page unfolds the path to and puts
   *  the focus on, wherever the item is anchored (R20), or, for a comment or message agents
   *  or a person wrote that is a turn of a thread the history holds (`agents-only.ts`), an entry
   *  of the panel's history (`agentsOnlyItem`). */
  | { readonly kind: "item"; readonly id: string }
  /** The issue's children, by the issue's own key: the children's own address (`/children`,
   *  `t h`) names the first row of their group in the outline, which the page unfolds the path
   *  to and puts the focus on. */
  | { readonly kind: "children"; readonly id: string };

/** What one address of the issue page opens. */
export interface IssueAddress {
  /** The slug of the document or file the address opens in place of the outline: its own
   *  address, `/artifacts/<slug>`, and a `/spec` address naming a block of the spec (R3, R20).
   *  The issue's spec is opened at its slug like every other document; `/spec` is the Spec
   *  filter. An address naming an ask or a comment (`?ask=`, `?comment=`) opens none, whichever
   *  document it is the address of: the item is a node of the outline, which it opens on that
   *  node - unless it also pins a version (`?v=`), or names the item with `?mark=`, which
   *  `documentItem` answers. Undefined for every other address. */
  readonly document: string | undefined;
  /** The ask or comment the document the address opens lands on, marked in its text: the one
   *  `?ask=` or `?comment=` names beside a version the address pins (`?v=`), since the item's
   *  quote as it stood at that version is content only the document at that version shows
   *  (R20), and the ask or comment `?mark=` names, which is the address "View in document"
   *  builds (`refs/routes.ts`'s `openDocumentPath`). `?mark=` names it by its id alone, so its
   *  kind is `mark` until the issue's comments tell which it is (`IssuePage`). Undefined for
   *  every other address. */
  readonly documentItem:
    | { readonly kind: "ask" | "comment"; readonly id: string }
    | { readonly kind: "mark"; readonly id: string }
    | undefined;
  /** Whether the address names Documents, whose list is drawn in place of the outline and of
   *  every view: `/documents` and `/artifacts`, the old page's Artifacts tab (part N's R1, R2). */
  readonly documents: boolean;
  /** What `?q=` narrows the preset to; `""` for an address that narrows nothing. */
  readonly filter: string;
  readonly preset: Preset;
  /** The node the address names, which the page unfolds the path to and focuses (R19). */
  readonly target: AddressTarget | undefined;
  /** The version the address pins (`?v=`): of the document it opens, and otherwise of the
   *  document the filter's version control reads. */
  readonly version: number | undefined;
}

/** Which filter each kind of route opens (R20). Every route not named here is the page with no
 *  filter on, Conversation, whose own address is the issue's. */
const ROUTE_PRESETS: Readonly<Record<string, Preset>> = {
  decisions: "decisions",
  plan: "plan",
  spec: "spec",
  verification: "verification",
};

/** The address of `preset` on `key`, which is the address its own control carries (R2, R20):
 *  each filter its old tab's, and Conversation, the page with no filter on, the issue's own. */
export function presetPath(key: string, preset: Preset): string {
  return buildIssuePath(preset === "conversation" ? { key, kind: "issue" } : { key, kind: preset });
}

/** A positive integer `?v=` holds, or undefined for anything else. */
function pinnedVersion(search: string): number | undefined {
  const value = new URLSearchParams(search).get("v") ?? "";
  return /^[1-9]\d*$/.test(value) ? Number(value) : undefined;
}

/** The document a route names, by its slug: an `artifact` route's own, and the issue's spec for
 *  a `spec` route, which names the primary document as much as `/artifacts/<spec>` does. Every
 *  other route names none. */
function routeDocument(route: IssueRoute, specSlug: string | undefined): string | undefined {
  if (route.kind === "artifact") return route.slug;
  return route.kind === "spec" ? specSlug : undefined;
}

/** The node `route` and `search` name, outside of a `#b-` fragment. */
function routeTarget(route: IssueRoute, specSlug: string | undefined): AddressTarget | undefined {
  if (route.kind === "ask" || route.kind === "comment" || route.kind === "message") {
    return { id: route.id, kind: "item" };
  }
  if (route.kind === "children") return { id: route.key, kind: "children" };
  if (route.kind === "artifact" || route.kind === "spec") {
    const block = "block" in route ? route.block : undefined;
    if (block !== undefined) {
      return { document: routeDocument(route, specSlug), id: block.id, kind: "block" };
    }
  }
  return undefined;
}

/** The item the document an address opens lands on (`IssueAddress.documentItem`): none where
 *  the address opens no document (`addressed`), the ask or comment `?ask=` or `?comment=` names
 *  (`item`) where it also pins a `version`, and otherwise the one `?mark=` names (`marked`). */
function documentItemOf(
  addressed: string | undefined,
  item: DispatchHrefItem | null | undefined,
  version: number | undefined,
  marked: string
): IssueAddress["documentItem"] {
  if (addressed === undefined) return undefined;
  if (item !== null && item !== undefined && version !== undefined) return item;
  return marked === "" ? undefined : { id: marked, kind: "mark" };
}

/**
 * What the address `route` + `search` + `hash` opens. `specSlug` is the slug of the issue's
 * spec, which a block a `/spec` address names is looked up in; undefined until the issue's
 * documents are read. The fragment outranks the route's own target, because a reader who wrote
 * `#b-` meant that block; `?ask=` and `?comment=` outrank both, because they are what a deep
 * link into a question or a comment carries.
 */
export function issueAddress(
  route: IssueRoute,
  search: string,
  hash: string,
  specSlug: string | undefined
): IssueAddress {
  const query = new URLSearchParams(search);
  const item = itemFromSearch(search);
  // `decodeBlockId`, not `decodeURIComponent`: a fragment holding a stray percent (`#b-50%`)
  // throws a `URIError`, and this runs in a render, so a bare decode would put the whole page
  // in its error boundary over one unreadable address. A fragment that reads as no block id
  // names no block, and the address falls back to what its route names, which for a document's
  // address is that document. The block it does name belongs to the document the route names,
  // where it names one: a block id is unique inside its own document alone. On an ask's, a
  // comment's or a message's own address the fragment names a part of that item - an option,
  // the answer, a block of a comment - which the outline draws inside the item's own row, so
  // the item's node is what the address names.
  const itemRoute = route.kind === "ask" || route.kind === "comment" || route.kind === "message";
  const block = hash.startsWith("#b-") && !itemRoute ? decodeBlockId(hash.slice(3)) : undefined;
  const named: AddressTarget | undefined =
    block === undefined
      ? routeTarget(route, specSlug)
      : { document: routeDocument(route, specSlug), id: block, kind: "block" };
  // An ask or a comment the address names beside it (`?ask=`, `?comment=`) is a node of the
  // outline, so the address opens the outline on that node, whichever document's address it is
  // - the spec's, `/spec`, or any other's - and wherever the item is anchored (R20). Two
  // addresses of a document name an item the document lands on instead: one that also pins a
  // version (`?v=`), whose quote as it stood then only that version of the document shows, and
  // `?mark=`, the address "View in document" builds. Otherwise a `/spec` address naming a block
  // of the spec (`#b-`) named content the old page opened the spec at, so it opens the spec, as
  // the spec's own address with the same fragment does; plain `/spec` is the Spec filter, and
  // which block opens the outline instead is the page's to say (`IssuePage.tsx`'s
  // `useOpensDocument`).
  const version = pinnedVersion(search) ?? (route.kind === "artifact" ? route.version : undefined);
  const addressed = routeDocument(route, specSlug);
  const marked = query.get("mark") ?? "";
  const documentItem = documentItemOf(addressed, item, version, marked);
  const outlineItem = documentItem === undefined && item !== null && item !== undefined;
  const namesSpecBlock = route.kind === "spec" && named?.kind === "block";
  return {
    document: outlineItem
      ? undefined
      : documentItem !== undefined || route.kind === "artifact" || namesSpecBlock
        ? addressed
        : undefined,
    documentItem,
    documents: route.kind === "issue-documents" || route.kind === "artifacts",
    filter: query.get("q") ?? "",
    preset: ROUTE_PRESETS[route.kind] ?? "conversation",
    target:
      documentItem === undefined
        ? outlineItem
          ? { id: item.id, kind: "item" }
          : named
        : { id: documentItem.id, kind: "item" },
    version,
  };
}

/**
 * The id `target` names where it is a turn of a thread the panel's history holds, which the page
 * opens in that history rather than in the outline: a message of a thread the Legion daemon
 * began, a person's reply in it included, or a comment of a thread tied to no text that no person
 * wrote in (`agents-only.ts`). Undefined for every other target, and while the reads that tell
 * are not in. `asks` is the asks read, which tells a reply under a question the outline draws
 * from one under an approval request, which it does not; a caller that has not read the asks
 * passes `undefined`, and a reply under any ask is then the outline's.
 */
export function agentsOnlyItem(
  target: AddressTarget | undefined,
  comments:
    | readonly Pick<Comment, "anchor" | "ask_id" | "author" | "id" | "reply_to">[]
    | undefined,
  messages: readonly MessageRead[] | undefined,
  asks: readonly Pick<Ask, "id" | "kind">[] | undefined
): string | undefined {
  if (target?.kind !== "item") return undefined;
  return agentsOnlyIds(comments, messages, asks).has(target.id) ? target.id : undefined;
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
 * address - has only the block id to go on, and the first node carrying it answers. The
 * children's address names the first child of their group.
 */
export function pathToTarget(
  root: TreeNode,
  target: AddressTarget | undefined
): readonly TreeNode[] | undefined {
  if (target === undefined) return undefined;
  if (target.kind === "children") return pathToNode(root, (node) => node.type === "child_issue");
  if (target.kind === "block") {
    if (target.document !== undefined) {
      const id = blockNodeId(target.document, target.id);
      return pathToNode(root, (node) => node.id === id);
    }
    const suffix = `#b-${target.id}`;
    return pathToNode(root, (node) => node.id.endsWith(suffix));
  }
  const byId = pathToNode(root, (node) => node.id === target.id);
  if (byId !== undefined) return byId;
  const address = `/${target.id}`;
  return pathToNode(root, (node) => node.reference?.endsWith(address) === true);
}
