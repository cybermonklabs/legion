import { DETAILS_ATTRIBUTE } from "./margin-slot";

/** An element that hides what is under it, or takes it out of reach of a press and the focus. */
const SHUT = '[hidden], [inert], [aria-hidden="true"]';

/** Every attribute whose change can change what `usableControl` answers: the ones its rule reads,
 *  and `class` and `style`, which `checkVisibility()` reads. A watch that must see each such
 *  change (Add's, `IssueAddMenu.tsx`) watches these, so a condition added to the rule is
 *  watched with it. */
export const USABLE_CONTROL_ATTRIBUTES = [
  "disabled",
  "aria-disabled",
  "hidden",
  "inert",
  "aria-hidden",
  "class",
  "style",
] as const;

/**
 * Whether `control` takes a press now: the one rule Add's entries and the key `l` read before
 * they list, offer or press a control another component draws, so what is offered is exactly
 * what a press opens. Details reads it in the keeper that moves a focus off a control a close
 * disables, and wherever it hands a focus on through `firstUsable` (`IssueMargin.tsx`,
 * `BlockedByControl.tsx`), as does the opener of a row (`details-rows.ts`, through
 * `firstUsableIn`) for the control it focuses or presses. A control is usable while it is on
 * the page; neither `disabled` nor busy (`aria-disabled="true"`, as the page's controls are
 * while they save: the `refused:` variant of `styles.css` reads the same pair); under no
 * element, itself included, that is `hidden`, `inert` or `aria-hidden="true"`; and drawn
 * (`checkVisibility()`, with `visibility: hidden` read too), which also catches a box the
 * stylesheet hides.
 *
 * A closed Details is no refusal. The elements around the control are read up to the Details
 * drawer's own `aside` only, which is `hidden` while Details is closed
 * (`features/margin/Margin.tsx`), and while that `aside` is hidden the control is not asked
 * whether it is drawn, since nothing under a hidden `aside` is. Two readers need this: a reach
 * that opens Details itself before it presses (the key `l`, through `useOpenDetailsAt`) must
 * find its control while Details is closed, or `l` would never open Details at Labels; and Add's
 * list must stay right while Details is closed, so Details does not open on an Add without the
 * entries whose controls are in Details, which the read its opening sets off must then put back.
 */
export function usableControl(control: Element | null): boolean {
  if (control === null || !control.isConnected) return false;
  if (control.matches(':disabled, [aria-disabled="true"]')) return false;
  const details = control.closest(`aside[${DETAILS_ATTRIBUTE}]`);
  for (
    let node: Element | null = control;
    node !== null && node !== details;
    node = node.parentElement
  ) {
    if (node.matches(SHUT)) return false;
  }
  if (details?.hasAttribute("hidden") === true) return true;
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
