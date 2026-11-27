import { canHoldFocus, FOCUS_ATTRIBUTES } from "./focus-keeper";
import { DETAILS_ATTRIBUTE } from "./margin-slot";

/** A control that keeps the focus but refuses a press: the page's controls while they save. */
const BUSY = '[aria-disabled="true"]';

/** An element a screen reader passes over, with everything under it. */
const ARIA_HIDDEN = '[aria-hidden="true"]';

/** Every attribute whose change can change what `usableControl` answers: the ones `canHoldFocus`
 *  reads (`FOCUS_ATTRIBUTES`), the ones this rule adds, and `class` and `style`, which
 *  `checkVisibility()` reads. A watch that must see each such change (Add's, `IssueAddMenu.tsx`)
 *  watches these, so a condition added to either rule is watched with it. */
export const USABLE_CONTROL_ATTRIBUTES = [
  ...FOCUS_ATTRIBUTES,
  "aria-disabled",
  "aria-hidden",
  "class",
  "style",
] as const;

/** Whether `control`, or an element around it short of `bound`, is `aria-hidden`. With no
 *  `bound`, every element around it is read. */
function ariaHiddenBelow(control: Element, bound: Element | null): boolean {
  for (
    let node: Element | null = control;
    node !== null && node !== bound;
    node = node.parentElement
  ) {
    if (node.matches(ARIA_HIDDEN)) return true;
  }
  return false;
}

/**
 * Whether `control` takes a press now: the one rule Add's entries, the key `l` and the header's
 * keys (`useHeaderKeys.ts`, the approval's `a` and `Shift+R` too) read before they list, offer or
 * press a control another component draws, so what is offered is exactly what a press opens.
 * Details reads it wherever it hands a focus on through `firstUsable` (`IssueMargin.tsx`,
 * `BlockedByControl.tsx`), as does the opener of a row (`details-rows.ts`), through `firstUsable`
 * for the row's marked opener it presses and `firstUsableIn` for the control it focuses or the
 * first control it presses in a row that marks none.
 *
 * It builds on `canHoldFocus` (`focus-keeper.ts`), whether an element can take the focus. A
 * control that can take the focus can still refuse a press, and an entry or a key that offered
 * it would then open nothing, so this rule refuses three more. A busy control:
 * `aria-disabled="true"`, which the page's controls carry while they save so that they keep the
 * focus (the `refused:` variant of `styles.css` reads it beside `:disabled`, which `canHoldFocus`
 * refuses). A control under an element, itself included, that is `aria-hidden="true"`, which a
 * screen reader passes over; for a control of Details these are read up to the Details drawer's
 * `aside`. And a control that is not drawn (`checkVisibility()`, with `visibility: hidden` read
 * too), which catches a box the stylesheet hides, since `canHoldFocus` reads no layout.
 *
 * The two rules answer differently in one place, on purpose: a control under a shut bound. The
 * bound is `within` where the caller names one, else the Details drawer's `aside` around the
 * control, and it is shut while it is `hidden`. `canHoldFocus` refuses every control under a
 * hidden element, so there this rule reads `canHoldFocus` bounded by it, which leaves the bound
 * and what is above it unread; it reads `aria-hidden` on the same elements, and does not ask
 * whether the control is drawn, since nothing under a hidden element is. Such a control is
 * refused when it is off the page, disabled or busy, or when it or an element between it and the
 * bound is hidden, inert or `aria-hidden`, and taken otherwise. Three readers need this. In a
 * closed Details, whose `aside` is `hidden` (`features/margin/Margin.tsx`), a reach that opens
 * Details itself before it presses (the key `l`, through `useOpenDetailsAt`) must find its
 * control, or `l` would never open Details at Labels; and Add's list must stay right, so Details
 * does not open on an Add without the entries whose controls are in Details, which the read its
 * opening sets off must then put back. In a shut group its caller names, the header's `Shift+P`
 * presses the pin, as a press on it in the open menu does: the header's keys name the actions
 * menu's group, hidden while the menu is shut, as the pin's bound (`useHeaderKeys.ts`).
 */
export function usableControl(control: Element | null, within?: Element | null): boolean {
  if (control === null || control.matches(BUSY)) return false;
  const details = control.closest(`aside[${DETAILS_ATTRIBUTE}]`);
  const bound = within ?? details;
  if (bound?.hasAttribute("hidden") === true) {
    return canHoldFocus(control, bound) && !ariaHiddenBelow(control, bound);
  }
  if (!canHoldFocus(control) || ariaHiddenBelow(control, details)) return false;
  // `checkVisibility()` needs Safari 17.4, Chrome 105 or Firefox 106, and Dispatch still runs on
  // browsers without it (`shell/roving.ts`): there the attributes above are the whole rule. With
  // no options it does not read `visibility: hidden`, so the options ask for that too, by both of
  // its names: a browser that shipped `checkVisibility()` before `visibilityProperty` knows it
  // only as `checkVisibilityCSS`.
  return (
    typeof control.checkVisibility !== "function" ||
    control.checkVisibility({ checkVisibilityCSS: true, visibilityProperty: true })
  );
}

/** The first of `candidates` that takes a press (`usableControl`), to hand a focus to or press. */
export function firstUsable(
  ...candidates: (HTMLElement | null | undefined)[]
): HTMLElement | undefined {
  return candidates.find(
    (candidate): candidate is HTMLElement => candidate != null && usableControl(candidate)
  );
}

/** A row's first control that takes a press, in the order the row draws them. */
export function firstUsableIn(container: ParentNode | null): HTMLElement | undefined {
  return firstUsable(
    ...(container?.querySelectorAll<HTMLElement>("input, textarea, select, button, a[href]") ?? [])
  );
}
