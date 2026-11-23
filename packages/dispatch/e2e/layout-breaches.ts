/** The layout rule's breaches the layout layer accepts on main, until the fix pull request of
 *  dispatch://INFRA-5200 that removes them lands. `e2e/layout-check.ts` judges every reading of
 *  `e2e/layout-routes.ts`'s routes against `layoutBreaches`, in every layout row.
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
 *  is still one breach. A fixed breach must leave this list in the change that fixes it.
 *
 *  The entries stand in one array per fix pull request, and each pull request deletes every entry
 *  of its own array and leaves the array empty: the three changes then share no line and touch no
 *  line another changes, so they merge in any order. (Deleting the arrays themselves, with their
 *  lines in `layoutBreaches`, conflicts in every order: those three lines stand side by side, and
 *  each array takes a blank line with it.) A pull request proves its fix by that change: with its
 *  array empty, the test fails at its base and passes at its head. The list is empty once the last
 *  of them lands, and this file and its use go then. */
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

/** The breaches the issue page's fix pull request removes (slice B of dispatch://INFRA-5200):
 *  today's issue page - `features/issue/**`, `components/Tabs.tsx`, the Verification, Children
 *  and Artifacts tabs. That pull request deletes every entry here and leaves the array empty. */
export const issuePageBreaches: readonly KnownLayoutBreach[] = [];

/** The breaches the Inbox's fix pull request removes (slice C of dispatch://INFRA-5200): the
 *  Inbox, the Peek drawer and the Answers page - `features/inbox/**`, `features/answers/**`.
 *  That pull request deletes every entry here and leaves the array empty. */
export const inboxBreaches: readonly KnownLayoutBreach[] = [];

/** The breaches the other pages' fix pull request removes (slice D of dispatch://INFRA-5200):
 *  every page and part of one the two arrays above leave out. That pull request deletes every
 *  entry here and leaves the array empty. */
export const otherPagesBreaches: readonly KnownLayoutBreach[] = [];

/** Every breach the layout rows accept: the three arrays above, one per fix pull request. */
export const layoutBreaches: readonly KnownLayoutBreach[] = [
  ...issuePageBreaches,
  ...inboxBreaches,
  ...otherPagesBreaches,
];
