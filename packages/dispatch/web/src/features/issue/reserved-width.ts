/**
 * The width a control of the issue header whose text is its value reserves, so the value
 * landing — a pick shown before the server answers it, or a save putting its own word in the
 * control — never changes the control's box and never moves what sits beside it
 * (dispatch://INFRA-299/spec#b-r5). The rule for open text names a floor and a cap that are the
 * same number, and the rule for a closed set draws every value of the set in the one box, which
 * is how a reader sees that no value can change the box. The reservation is the header's alone:
 * the priority badge every other surface draws (`PriorityControl`) keeps its value's own width,
 * and reserves only where the header passes `PriorityEditor`'s `reserve`.
 *
 * The open-text rule bounds the content box (`box-content`), so the characters it names are the
 * whole of the bound whatever padding the control draws around them: the repository badge's
 * fourteen are fourteen of its monospace characters, and the labels trigger's are fourteen
 * whether it draws the phone's padding or, from `sm`, none — its word centred in the fourteen,
 * which hold the widest of them with room on both sides, so a bare issue's details still fit
 * the desktop rail.
 *
 * Both start at `sm`, because below it the header has no room for them: measured in this pod
 * at 390 px, the state row holds Status, the priority, the assignee and Close in 316 px with
 * 42 px to spare, and the assignee's fourteen characters alone want more than that (85 px past
 * an unset assignee's badge in DejaVu Sans), which would push Close issue onto a row of its own
 * — the one thing dispatch://INFRA-299/spec#b-r3 says that width must not do. Below `sm` these
 * controls keep the boxes they have today, so a pick there still moves what follows it; from
 * `sm` up, where the row has the width, none of them does.
 */

/**
 * A value that is open text — the assignee's login, the repository's `owner/name`, the labels
 * trigger's own word — has no bound of its own, so it takes the fourteen characters the
 * header's rail items are already cut at (`inPlaceBoxRules.rail`) as its floor as well as its
 * cap: one box at every value, with a longer one cut. A control that cuts a value keeps the
 * whole of it in its `title`, as the rail's items do.
 */
export const reservedOpenTextWidth = "sm:box-content sm:min-w-[14ch] sm:max-w-[14ch]";

/**
 * A value from a closed set takes the width of the widest of them by drawing all of them: the
 * control lays every value it can show in one grid cell, the one it shows visible and the rest
 * `invisible`, so the cell is as wide as the widest value in the type that draws it, and a value
 * added to the set widens the cell with it instead of outgrowing a count of characters. The
 * priority badge (`PriorityEditor`) is that control: `Priority`, the tag it wears while unset,
 * and `P0` to `P3`, the shorter of them in the middle of the cell. `cell` goes on the control's
 * box, `value` on every value and `other` on each value it does not show; below `sm` the box is
 * no grid and those values are not drawn, so there it keeps its value's own width.
 */
export const reservedClosedSet = {
  cell: "sm:inline-grid sm:justify-items-center",
  other: "invisible max-sm:hidden",
  value: "col-start-1 row-start-1",
} as const;
