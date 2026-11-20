import { canHoldFocus, OUT_OF_REACH } from "./focus-keeper";
import { DETAILS_ATTRIBUTE } from "./margin-slot";

/** A control that keeps the focus but refuses a press: the page's controls while they save. */
const BUSY = '[aria-disabled="true"]';

/** An element a screen reader passes over, with everything under it. */
const ARIA_HIDDEN = '[aria-hidden="true"]';

/** What takes a control out of reach inside the closed Details drawer, where `canHoldFocus`
 *  refuses everything: what takes it out of the focus's reach (`canHoldFocus`'s own selector),
 *  and `aria-hidden`. */
const SHUT = `${OUT_OF_REACH}, ${ARIA_HIDDEN}`;

/** Every attribute whose change can change what `usableControl` answers: the ones its rule
 *  reads, those `canHoldFocus` reads among them, and `class` and `style`, which
 *  `checkVisibility()` reads. A watch that must see each such change (Add's, `IssueAddMenu.tsx`)
 *  watches these, so a condition added to the rule is watched with it. */
export const USABLE_CONTROL_ATTRIBUTES = [
  "disabled",
  "aria-disabled",
  "hidden",
  "inert",
  "aria-hidden",
  "class",
  "style",
] as const;

/** Whether `control`, or an element around it short of `bound`, matches `selector`. With no
 *  `bound`, every element around it is read. */
function shutBelow(control: Element, bound: Element | null, selector: string): boolean {
  for (
    let node: Element | null = control;
    node !== null && node !== bound;
    node = node.parentElement
  ) {
    if (node.matches(selector)) return true;
  }
  return false;
}

/**
 * Whether `control` takes a press now: the one rule Add's entries and the key `l` read before
 * they list, offer or press a control another component draws, so what is offered is exactly
 * what a press opens. Details reads it wherever it hands a focus on through `firstUsable`
 * (`IssueMargin.tsx`, `BlockedByControl.tsx`), as does the opener of a row (`details-rows.ts`,
 * through `firstUsableIn`) for the control it focuses or presses.
 *
 * It builds on `canHoldFocus` (`focus-keeper.ts`), whether an element can take the focus: it is
 * on the page, not `disabled`, and neither it nor an element around it is `hidden` or `inert`.
 * A control that can take the focus can still refuse a press, and an entry or a key that offered
 * it would then open nothing, so this rule refuses three more. A busy control:
 * `aria-disabled="true"`, which the page's controls carry while they save so that they keep the
 * focus (the `refused:` variant of `styles.css` reads the same pair). A control under an element,
 * itself included, that is `aria-hidden="true"`, which a screen reader passes over; for a control
 * of Details these are read up to the Details drawer's `aside`. And a control that is not drawn
 * (`checkVisibility()`, with `visibility: hidden` read too), which catches a box the stylesheet
 * hides, since `canHoldFocus` reads no layout.
 *
 * The two rules answer differently in one place, on purpose: a control inside a closed Details.
 * Details' drawer is an `aside` that is `hidden` while Details is closed
 * (`features/margin/Margin.tsx`), so `canHoldFocus` refuses every control in it. This rule makes
 * `canHoldFocus`'s own tests on such a control, with its selector (`OUT_OF_REACH`), but reads the
 * elements around the control up to that `aside` only, which `canHoldFocus` cannot be told to do,
 * and does not ask whether the control is drawn, since nothing under a hidden `aside` is: it is
 * refused when it is off the page, disabled or busy, or when it or an element between it and the
 * `aside` is hidden, inert or `aria-hidden`, and taken otherwise. Two readers need this: a reach
 * that opens Details itself before it presses (the key `l`, through `useOpenDetailsAt`) must find
 * its control while Details is closed, or `l` would never open Details at Labels; and Add's list
 * must stay right while Details is closed, so Details does not open on an Add without the entries
 * whose controls are in Details, which the read its opening sets off must then put back.
 */
export function usableControl(control: Element | null): boolean {
  if (control === null || control.matches(BUSY)) return false;
  const details = control.closest(`aside[${DETAILS_ATTRIBUTE}]`);
  if (details?.hasAttribute("hidden") === true) {
    return (
      control.isConnected && !control.matches(":disabled") && !shutBelow(control, details, SHUT)
    );
  }
  if (!canHoldFocus(control) || shutBelow(control, details, ARIA_HIDDEN)) return false;
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

/** The first of `candidates` that takes a press (`usableControl`), to hand a focus to. */
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
