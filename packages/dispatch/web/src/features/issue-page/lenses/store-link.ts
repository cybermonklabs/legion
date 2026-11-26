import type { DisplayTree } from "../../../api/types";
import { buildIssuePath, parseIssuePath } from "../../refs/routes";
import { storeAddressReference } from "../../refs/store-address";

/**
 * The link a row's words make of a store address they name by its node: `dispatch://lens/<uuid>`,
 * `dispatch://risk/<uuid>`, `dispatch://constraint/<uuid>`, and a finding's
 * `dispatch://lens/<uuid>#b-<id>`. No route names one (`refs/store-address.ts`). Where the tree
 * read holds the node and a step drafted its row, the words read it by the node's name, in a link
 * mark that holds the store address as its `href` and the path of the node's row as its `title`.
 * The body's reference pass reads a `dispatch://` address no route names as the words its author
 * wrote, and a link to the row's path as a link to its issue, so before the words are painted
 * `placeStoreLinks` writes each such link `storeLinkPath` can place as a link to nothing that
 * holds the path as its title, and the row's link pass (`RowText.tsx`'s `DrawnText`) takes that
 * path as the link's address (`placedStorePath`). A link it cannot place is left to the pass,
 * which draws it as its words.
 *
 * `RowText.tsx` loads on today's pages, so this file imports nothing from the issue page's model
 * or its word lists: of the files outside `lenses/`, only `refs/routes.ts` and
 * `refs/store-address.ts`, which today's pages load already. `RowText.tsx` takes
 * `placeStoreLinks` and `placedStorePath` from here; `StoreKind`, `storeKindOf` and
 * `storeAddressOf` are exported for the step that drafts a store address's link in a row's words,
 * and no file imports them yet.
 */

/** The kinds of node a store address in a row's words names, as the tree names them. */
export type StoreKind = "constraint" | "finding" | "lens" | "risk";

/** Where the page stands, as the router holds it (`useLocation`). */
interface PageAddress {
  readonly pathname: string;
  readonly search: string;
}

/**
 * The path a link in a row's words to the store address `href` opens (`RowText.tsx`'s link
 * pass): its link mark's `title`, `title`, only where that is the very path a name's link holds -
 * the outline of the issue the page's address `page` names, read as the page's slots read it
 * (`parseIssuePath`), at the `?node=` of the node `href` names (`outlineRowPath`). Undefined for
 * any other title, so an author's own title on a link to a store address opens nothing, and for
 * any other address.
 */
export function storeLinkPath(href: string, title: string, page: PageAddress): string | undefined {
  const address = storeAddressReference(href);
  const query = title.indexOf("?");
  if (address === undefined || query === -1) return undefined;
  const issueKey = parseIssuePath(page.pathname, page.search)?.key;
  if (issueKey === undefined) return undefined;
  const readId = new URLSearchParams(title.slice(query + 1)).get("node");
  return readId !== null && namesNode(address, readId) && title === outlineRowPath(issueKey, readId)
    ? title
    : undefined;
}

/** A mark of a display tree, as far as a link's two attributes are read. */
interface TreeMark {
  readonly type: string;
  readonly attrs?: { readonly href?: unknown; readonly title?: unknown };
}

/** `mark`, a link to a store address that `storeLinkPath` places on `page`, written as a link to
 *  nothing that holds the node's row path as its title; any other mark as it is. */
function placedMark(mark: TreeMark, page: PageAddress): TreeMark {
  const { href, title } = mark.attrs ?? {};
  if (mark.type !== "link" || typeof href !== "string" || typeof title !== "string") return mark;
  const path = storeLinkPath(href, title, page);
  return path === undefined ? mark : { ...mark, attrs: { ...mark.attrs, href: "", title: path } };
}

/**
 * `tree` with each link to a store address that `storeLinkPath` places on `page` written as a
 * link to nothing whose title is the node's row path (`placedStorePath`), so the body's reference
 * pass, which would draw the address as its words or a link to the path as one to its issue,
 * leaves it to `RowText.tsx`'s link pass. `tree` itself, the same object, where it holds none: a
 * reading drawn again over the same words is painted once, and a reading with no store link, as
 * nearly every row's is, is copied nowhere.
 */
export function placeStoreLinks(tree: DisplayTree, page: PageAddress): DisplayTree {
  const marks = placedItems(tree.marks as readonly TreeMark[] | undefined, (mark) =>
    placedMark(mark, page)
  );
  const content = placedItems(tree.content as readonly DisplayTree[] | undefined, (child) =>
    placeStoreLinks(child, page)
  );
  if (marks === undefined && content === undefined) return tree;
  return {
    ...tree,
    ...(marks === undefined ? {} : { marks }),
    ...(content === undefined ? {} : { content }),
  };
}

/** `items` with each item replaced by what `place` answers for it, copied from the first item
 *  `place` changes; undefined where it changes none, or `items` is no array. */
function placedItems<T>(items: readonly T[] | undefined, place: (item: T) => T): T[] | undefined {
  if (!Array.isArray(items)) return undefined;
  let placed: T[] | undefined;
  for (const [at, item] of items.entries()) {
    const now = place(item);
    if (placed === undefined && now !== item) placed = items.slice(0, at);
    placed?.push(now);
  }
  return placed;
}

/**
 * The path a link `placeStoreLinks` wrote opens, read from its `title` back on the page: the
 * title where it is the outline of an issue at the `?node=` of a node (`outlineRowPath`), else
 * undefined. The row's link pass reads only links to nothing (`href=""`), so a title an author
 * wrote on a link of any address is never taken.
 */
export function placedStorePath(title: string): string | undefined {
  const query = title.indexOf("?");
  if (query === -1) return undefined;
  const issueKey = parseIssuePath(title.slice(0, query), title.slice(query))?.key;
  const readId = new URLSearchParams(title.slice(query + 1)).get("node");
  return issueKey !== undefined && readId !== null && title === outlineRowPath(issueKey, readId)
    ? title
    : undefined;
}

/**
 * Whether `readId`, a node's id in the tree read, is that of the node the store address `address`
 * names, as an address is built from a read id (`storeAddressOf`) for the kind the address names
 * (`storeKindOf`): with a leading `<kind>/` taken off, the address's id (`dispatch://<kind>/<id>`)
 * for a lens, a risk and a constraint. For a finding (`dispatch://lens/<id>#b-<block>`), the block
 * id is the part after the read id's last `#`, or the whole read id where it holds none, and where
 * it holds one the part before it is the lens's id; a read id of the block id alone names no lens,
 * since a finding's address takes its lens from the finding's parent.
 */
function namesNode(address: string, readId: string): boolean {
  const kind = storeKindOf(address);
  switch (kind) {
    case undefined:
      return false;
    case "finding": {
      const hash = readId.lastIndexOf("#");
      const lens =
        hash === -1
          ? address.slice("dispatch://lens/".length, address.indexOf("#"))
          : readId.slice(0, hash);
      return storeAddressReference(storeAddressOf(kind, readId, lens)) === address;
    }
    default:
      return storeAddressReference(storeAddressOf(kind, readId)) === address;
  }
}

/** The kind of node the store address `address` names, written as `storeAddressReference`
 *  answers it: `finding` for a block of a lens, else `lens`, `risk` or `constraint` as its kind
 *  segment names it. Undefined for an interaction's and a lens kind's address. */
export function storeKindOf(address: string): StoreKind | undefined {
  if (address.includes("#")) return "finding";
  const kind = address.slice("dispatch://".length, address.lastIndexOf("/"));
  return kind === "constraint" || kind === "lens" || kind === "risk" ? kind : undefined;
}

/**
 * The `dispatch://` address of the node of kind `type` whose read id is `readId`: its store id
 * after its kind, and for a finding its lens's, whose read id is `lens`, with the block id after
 * the last `#` of its own. The tree read answers no address per node, so the address is built
 * from the read id: a node's read id is its store id, bare or after its kind (`lens/<uuid>`), and
 * a finding's is its lens's and its block id after a `#` (`lens/<uuid>#<block id>`), or its block
 * id alone.
 */
export function storeAddressOf(type: StoreKind, readId: string, lens = ""): string {
  if (type !== "finding") return `dispatch://${type}/${storeId(readId, type)}`;
  const block = readId.slice(readId.lastIndexOf("#") + 1);
  return `dispatch://lens/${storeId(lens, "lens")}#b-${block}`;
}

/** `id` without the kind its store address starts with (`lens/`), where it carries one. */
function storeId(id: string, kind: string): string {
  return id.startsWith(`${kind}/`) ? id.slice(kind.length + 1) : id;
}

/** The address of the row of the node whose id in the tree read is `readId` on the page of the
 *  issue `key`: the issue's own path (`buildIssuePath`), which opens the outline, at its `?node=`,
 *  after the words filter `filter` (`?q=`) where one is given. A store address in a row's words
 *  links that row by it, and a store link's path is checked against it (`storeLinkPath`). */
export function outlineRowPath(key: string, readId: string, filter?: string): string {
  const query = new URLSearchParams(filter === undefined ? {} : { q: filter });
  query.set("node", readId);
  return `${buildIssuePath({ key, kind: "issue" })}?${query}`;
}
