/**
 * The composer census (INFRA-1503): every `MentionComposer` mount in the Dispatch web app outside
 * tests, and every item that changes where its draft lives or ends its mount. `trees.ts` reads
 * the address props, the conditionals, the keys, the routes and the early returns from each
 * mount up to `app.tsx`'s routes; `forms.ts` reads the forms, where the layout draws a composer
 * otherwise; `lifetime.ts` adds one lifetime per mount, the tree ended and drawn again by a
 * reload. `composer-census.test.ts` holds every item to an entry of a draft matrix file's
 * coverage list (`coverage.ts`).
 *
 * Each item is named by a key that no line number enters: the component whose code holds it,
 * the component drawn at the place it is about (`MentionComposer` at a mount itself), its kind,
 * and the expression's own text with its whitespace run together. The same expression at two
 * places of one component, about the same component, is one item.
 */
import { readForms } from "./forms";
import { readLifetimes } from "./lifetime";
import { readProgram } from "./program";
import { readTrees, type TreeKind } from "./trees";

export type ItemKind = TreeKind | "form" | "lifetime";

export interface CensusItem {
  readonly key: string;
  /** The component (or, for a form, the hook) whose code holds the item. */
  readonly host: string;
  /** The component drawn at the place the item is about. */
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

const SEPARATOR = " | ";

export function itemKey(host: string, mount: string, kind: ItemKind, expression: string): string {
  return [host, mount, kind, expression.replace(/\s+/g, " ").trim()].join(SEPARATOR);
}

/** A key read back into its parts. Neither a component's name nor a kind holds the separator,
 *  so the expression is all that follows the third. */
export function keyParts(key: string): { host: string; kind: string; expression: string } {
  const [host = "", , kind = "", ...expression] = key.split(SEPARATOR);
  return { expression: expression.join(SEPARATOR), host, kind };
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
export function composerCensus(root: string): CensusItem[] {
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
  return [...byKey.values()];
}
