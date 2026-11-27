/**
 * The mark of the control that sets one property of one issue: the attribute
 * `data-property-control`, whose value is the property and then the issue's key
 * (`data-property-control="assignee CORE-1"`). The page finds a property's control by this mark,
 * never by the control's accessible name: a name changes with the control's state ("Saving labels
 * of CORE-1", Blocked by's Add or Edit as the issue waits on none or some) and its surface, and a
 * shared list's single pick names its trigger through `aria-labelledby`, by its list's name and
 * then what it shows (`MultiSelect`), so its trigger carries no `aria-label` to find. The issue's
 * key in the mark keeps another issue's control out of reach, as the key in the control's name
 * did. Each control writes its mark with `propertyControl` and each reader finds it with
 * `propertyControlSelector`, so the two sides cannot drift apart. Its readers:
 *
 * - the new issue page's add menu (`IssueAddMenu.tsx`), which presses the control of each
 *   property it lists, and watches the mark for a change;
 * - the key `l`, the catalogue's Edit labels (`issue-page/slots/details.tsx`), which presses the
 *   labels' control in Details' Labels row;
 * - the Retry of Details' Blocked by titles (`IssueMargin.tsx`), which hands the focus it held on
 *   to the row's control;
 * - the hand-offs of Details' priority line (`usePriorityHandOffs.ts`), which hand the focus on
 *   to the line's add control in the place the line moved to.
 *
 * This module imports nothing, so the shared controls that write the mark load none of the issue
 * page's word list (`components/tooltips.tsx`).
 */

/** The attribute the mark is written in, which the add menu's watch reads for a change. */
export const PROPERTY_CONTROL_ATTRIBUTE = "data-property-control";

/** The properties whose control carries the mark: each the add menu reaches through a control
 *  drawn elsewhere on the page, by the key `PROPERTY_LABELS` names it by
 *  (`issue-page/page-structure.ts`). */
export type MarkedProperty =
  | "advances"
  | "assignee"
  | "blocked_by"
  | "components"
  | "labels"
  | "repository";

/** The mark's value: the property, then the issue's key. */
function markOf(property: MarkedProperty, issueKey: string): string {
  return `${property} ${issueKey}`;
}

/** The mark of the control that sets `property` of the issue `issueKey`, spread onto the control
 *  (`<button {...propertyControl("blocked_by", key)}>`) or handed to a shared list as its
 *  trigger's mark (`MultiSelect`'s `marks.trigger`). */
export function propertyControl(
  property: MarkedProperty,
  issueKey: string
): Readonly<Record<`data-${string}`, string>> {
  return { [PROPERTY_CONTROL_ATTRIBUTE]: markOf(property, issueKey) };
}

/** The selector of the element that carries `propertyControl(property, issueKey)`. */
export function propertyControlSelector(property: MarkedProperty, issueKey: string): string {
  return `[${PROPERTY_CONTROL_ATTRIBUTE}="${CSS.escape(markOf(property, issueKey))}"]`;
}
