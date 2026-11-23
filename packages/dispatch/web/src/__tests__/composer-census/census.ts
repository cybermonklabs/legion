/**
 * The composer census (INFRA-1503): every mount in the Dispatch web app outside tests of a field
 * the draft stores keep the reader's typing for (`DRAFT_FIELDS` in `trees.ts`: `MentionComposer`,
 * the ask card's answer field and `AskReplyComposer`), and every item that changes where its
 * draft lives or ends its mount. `trees.ts` reads the address props, the conditionals, the keys,
 * the routes and the early returns from each mount up to `app.tsx`'s routes, or, for an ask's
 * field, up to its card, walking up from each mount and each place above one once (`walk.ts`);
 * `forms.ts` reads the forms out of the same walks, where the layout draws a field otherwise;
 * `lifetime.ts` adds one lifetime per mount, the tree ended and drawn again by a reload.
 * `composer-census.test.ts` holds every item to an entry of a draft matrix file's coverage list
 * (`coverage.ts`).
 *
 * The census fails closed: what it meets and cannot read, a field drawn in no component it reads
 * or a component element between a mount and the routes whose drawing it cannot follow, is
 * listed as unread (`Census.unread`), and the test fails on each, since a mount or an item may be
 * behind it.
 *
 * Each item is named by a key that no line number enters: the component whose code holds it,
 * the place it is about (a mount, by its host and the element it is, at the mount itself and in a
 * component the mount sits in a slot of; the component drawn there at a place above mounts),
 * its kind, and the expression's own text with its whitespace run together. The same
 * expression at two places of one component, about the same mount or the same component, is
 * one item; above two mounts of one host it is an item of each.
 */
import { readForms } from "./forms";
import { readLifetimes } from "./lifetime";
import { readProgram } from "./program";
import { readTrees } from "./trees";
import type { TreeKind } from "./walk";

export type ItemKind = TreeKind | "form" | "lifetime";

export interface CensusItem {
  readonly key: string;
  /** The component (or, for a form, the hook) whose code holds the item. */
  readonly host: string;
  /** The place the item is about: a mount, by its host and the element it is, with its send name
   *  or its handle (`ThreadCard <MentionComposer mutationKey={replyMutationKey}>`,
   *  `AskCard <textarea data-ask-answer="">`), the component drawn at a place above mounts, or
   *  `-` for a form of a body that wraps no mount. */
  readonly mount: string;
  readonly kind: ItemKind;
  readonly expression: string;
  /** Where the item is, `<path under web/src>:<line>`, for a reader of a failure. */
  readonly at: readonly string[];
  /** The mounts it stands above, each by its host and its send name. */
  readonly above: readonly string[];
  /** What else a reader of a failure needs: how a form reads the layout, what reloads a tree. */
  readonly detail: string;
}

export interface Census {
  readonly items: readonly CensusItem[];
  /** What the census met and cannot read, each with where it is (`Trees.unread`). */
  readonly unread: readonly string[];
}

const SEPARATOR = " | ";

export function itemKey(host: string, mount: string, kind: ItemKind, expression: string): string {
  const parts = [host, mount, kind];
  if (parts.some((part) => part.includes(SEPARATOR))) {
    throw new Error(`the census cannot key "${parts.join('", "')}": a part holds "${SEPARATOR}"`);
  }
  return [...parts, expression.replace(/\s+/g, " ").trim()].join(SEPARATOR);
}

/** A key read back into its parts. No part but the expression holds the separator (`itemKey`),
 *  so the expression is all that follows the third. */
export function keyParts(key: string): {
  host: string;
  mount: string;
  kind: string;
  expression: string;
} {
  const [host = "", mount = "", kind = "", ...expression] = key.split(SEPARATOR);
  return { expression: expression.join(SEPARATOR), host, kind, mount };
}

/** One item as a part of the census finds it, before the same key found twice is one item. */
interface Found {
  readonly host: string;
  readonly mount: string;
  readonly kind: ItemKind;
  readonly expression: string;
  readonly at: string;
  readonly mounts: readonly string[];
  readonly detail: string;
}

/** The census of the tree under `root`, a `web/src` or a copy of it. */
export function composerCensus(root: string): Census {
  const trees = readTrees(readProgram(root));
  const found: Found[] = [
    ...trees.items.map((item) => ({ ...item, detail: "" })),
    ...readForms(trees).map((form) => ({
      ...form,
      detail: `layout: ${form.why}`,
      kind: "form" as const,
    })),
    ...readLifetimes(trees).map((item) => ({
      ...item,
      detail: `ended by ${item.reloads.join("; ")}`,
      kind: "lifetime" as const,
    })),
  ];
  const byKey = new Map<string, CensusItem & { at: string[]; above: string[] }>();
  for (const item of found) {
    const key = itemKey(item.host, item.mount, item.kind, item.expression);
    const known = byKey.get(key);
    if (known === undefined) {
      byKey.set(key, {
        above: [...item.mounts],
        at: [item.at],
        detail: item.detail,
        expression: keyParts(key).expression,
        host: item.host,
        key,
        kind: item.kind,
        mount: item.mount,
      });
      continue;
    }
    if (!known.at.includes(item.at)) known.at.push(item.at);
    for (const mount of item.mounts) if (!known.above.includes(mount)) known.above.push(mount);
  }
  return { items: [...byKey.values()], unread: trees.unread };
}
