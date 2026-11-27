/** The layout rule's breaches the layout layer accepts on main: none, since the three fix pull
 *  requests of dispatch://INFRA-5200 landed, so every breach a reading finds fails its row.
 *  `e2e/layout-check.ts` judges every reading of `e2e/layout-routes.ts`'s routes against
 *  `layoutBreaches`, in every layout row.
 *
 *  A breach the layer lets through is an entry here, never a skipped route or a narrowed width.
 *  An entry accepts exactly `count` breaches (1 unless given) of its `kind`, on `route` - in its
 *  named `state`, or on the page as it loads when it names none - at `width`, whose locator is
 *  `element`: the `data-testid`, `data-layout-group` or `aria-label` nearest the breach, or its
 *  path, as the test prints it beside each breach no entry accepts. It matches only a breach of
 *  that locator, never one of an element inside it that carries a name of its own: a page-scroll
 *  breach's locator is that of the deepest element past the viewport. A `group-missing` entry
 *  names a group the route must mark (`requiredGroups`) and does not.
 *
 *  A reading fails on a breach no entry accepts, a breach past its entry's count, and an entry
 *  whose place it measured and which matched fewer breaches than its count. The count is the
 *  bound of what an entry lets through: one breach fixed and another of the same kind and locator
 *  added passes, and more overflow inside a box an entry already names does too, since that box
 *  is still one breach. A fixed breach must leave this list in the change that fixes it. */
import type { LayoutBreach, LayoutWidth } from "./layout-measure";

/** What a reading can break: the four kinds the measure reports, and a group the route must mark
 *  and the page does not. */
export type LayoutBreachKind = LayoutBreach["kind"] | "group-missing";

export interface KnownLayoutBreach {
  /** The `name` of the route in `e2e/layout-routes.ts`. */
  readonly route: string;
  /** The `name` of one of the route's states; absent for the page as it loads. */
  readonly state?: string;
  readonly width: LayoutWidth;
  readonly kind: LayoutBreachKind;
  /** The breach's locator; for `group-missing`, the group's name. */
  readonly element: string;
  /** How many breaches the entry accepts, exactly; 1 unless given. */
  readonly count?: number;
}

/** Every breach the layout rows accept. */
export const layoutBreaches: readonly KnownLayoutBreach[] = [];
