/** The layout rule's breaches the layout layer accepts on main, until the slice of
 *  dispatch://INFRA-5200 that removes them lands. `e2e/layout-check.ts` judges every reading of
 *  `e2e/layout-routes.ts`'s routes against it, for the stubbed rows and the real-stack ones alike.
 *
 *  A breach the layer lets through is an entry here, never a skipped route or a narrowed width.
 *  An entry accepts exactly `count` breaches (1 unless given) of its `kind`, on `route` - in its
 *  named `state`, or on the page as it loads when it names none - at `width`, whose locator is
 *  `element`: the `data-testid`, `data-layout-group` or `aria-label` nearest the breach, or its
 *  path, as the test prints it beside each breach no entry accepts. A page-scroll breach also
 *  matches an entry naming any element past the viewport inside it, its own locator first. A
 *  `group-missing` entry names a group the route must mark (`requiredGroups`) and does not.
 *
 *  A reading fails on a breach no entry accepts, a breach past its entry's count, and an entry
 *  whose place it measured and which matched fewer breaches than its count. So a new breach fails
 *  even inside a region an entry names, and a fixed breach must leave this list in the change that
 *  fixes it. A slice proves its fix by deleting its entries: with them gone, the test fails at the
 *  slice's base and passes at its head. The list is empty once the last slice lands, and this file
 *  and its use go then. */
import type { LayoutBreach, LayoutWidth } from "./layout-measure";

/** The slice of dispatch://INFRA-5200 that removes a breach: B owns today's issue page
 *  (`features/issue/**`, `components/Tabs.tsx`, the Verification, Children and Artifacts tabs), C
 *  the Inbox, the Peek drawer and the Answers page (`features/inbox/**`, `features/answers/**`),
 *  D every other surface. */
export type LayoutSlice = "B issue page" | "C inbox" | "D surfaces";

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
  readonly reason: LayoutSlice;
}

export const layoutBreaches: readonly KnownLayoutBreach[] = [];
