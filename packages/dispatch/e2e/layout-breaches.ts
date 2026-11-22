/** The layout rule's breaches the layout layer accepts on main, one entry each, until the slice
 *  of dispatch://INFRA-5200 that removes it lands. `e2e/layout-check.ts` judges every reading of
 *  `e2e/layout-routes.ts`'s routes against it, for the stubbed rows and the real-stack ones alike.
 *
 *  A breach the layer lets through is an entry here, never a skipped route or a narrowed width:
 *  a row fails on any breach no entry covers, and on an entry whose route, state and width it
 *  measured and which matched no breach, so a fixed breach must leave this list in the change
 *  that fixes it. A slice proves its fix by deleting its entries: with them gone, the test fails
 *  at the slice's base and passes at its head. The list is empty once the last slice lands, and
 *  this file and its use go then.
 *
 *  An entry matches a breach of its `kind` measured on `route` - in its named `state`, or on the
 *  page as it loads when it names none - at `width`, whose element sits in `element` (`closest`):
 *  a `data-testid`, an `aria-label` or a short CSS selector, the locator the test prints beside
 *  each breach it does not cover. A page-scroll breach matches through any element past the
 *  viewport inside it, so its entry names the element that causes it. */
import type { LayoutBreach, LayoutWidth } from "./layout-measure";

/** The slice of dispatch://INFRA-5200 that removes a breach: B owns the issue page
 *  (`features/issue/**`, `components/Tabs.tsx`, the Verification, Children and Artifacts tabs), C
 *  the Inbox and the Answers page (`features/inbox/**`, `features/answers/**`), D every other
 *  surface. */
export type LayoutSlice = "B issue page" | "C inbox" | "D surfaces";

export interface KnownLayoutBreach {
  /** The `name` of the route in `e2e/layout-routes.ts`. */
  readonly route: string;
  /** The `name` of one of the route's states; absent for the page as it loads. */
  readonly state?: string;
  readonly width: LayoutWidth;
  readonly kind: LayoutBreach["kind"];
  readonly element: string;
  readonly reason: LayoutSlice;
}

export const layoutBreaches: readonly KnownLayoutBreach[] = [];
