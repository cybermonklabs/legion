/** The serious and critical axe findings `e2e/accessibility.e2e.ts` accepts, one entry each.
 *
 *  A violation the gate lets through is an entry here, never a disabled rule or a quiet filter:
 *  the gate fails on any serious or critical finding no entry covers, and it also fails on an
 *  entry that matched nothing in the rows it ran, so a stale entry cannot hide a regression.
 *
 *  An entry is written only where the fix would change what another row of the suite claims -
 *  that is, where the accessible page and the behaviour the product was asked for disagree. That
 *  is a product judgement, so it goes to the architect with the rule, the DOM and the rows it
 *  would move, and an entry is written here only once the architect has answered. Every other
 *  finding is fixed in `web/src`, in the change that found it. */
export interface AccessibilityExclusion {
  /** The axe rule id, for example "list". */
  readonly rule: string;
  /** The entry of `e2e/review-routes.ts` this covers, by its `name`. */
  readonly route: string;
  /** The CSS selector axe reports for the node, as its `target` gives it. */
  readonly selector: string;
  /** Why this violation is accepted, in a sentence a reviewer can judge. */
  readonly reason: string;
  /** The Dispatch issue that owns it, `dispatch://KEY`. */
  readonly owner: string;
}

export const accessibilityExclusions: readonly AccessibilityExclusion[] = [];
