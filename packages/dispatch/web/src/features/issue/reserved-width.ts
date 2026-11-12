/**
 * The width a control whose text is its value reserves, so the value landing — a pick shown
 * before the server answers it, or a save putting its own word in the control — never changes
 * the control's box and never moves what sits beside it
 * (dispatch://INFRA-299/spec#b-r5). Each rule below names a floor and a cap that are the same
 * number, which is how a reader sees that no value can change the box.
 *
 * Both bound the content box (`box-content`), so the characters they name are the whole of the
 * bound whatever padding the control draws around them: the repository badge's fourteen are
 * fourteen of its monospace characters, and the labels trigger's are fourteen whether its
 * padding is the phone's or the desk's.
 *
 * Both start at `sm`, because below it the header has no room for them: measured in this pod
 * at 390 px, the state row holds Status, the priority, the assignee and Close in 316 px with
 * 42 px to spare, and reserving the two of them that sit in that row wants 72 px more, which
 * would push Close issue onto a row of its own — the one thing
 * dispatch://INFRA-299/spec#b-r3 says that width must not do. Below `sm` these controls keep
 * the boxes they have today, so a pick there still moves what follows it; from `sm` up, where
 * the row has the width, none of them does.
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
 * A value from a closed set takes the width of the widest of them: the priority's values are
 * `Priority` — the tag it wears while unset — and `P0` to `P3`, and eight characters hold the
 * eight letters of the widest, because a `ch` is the advance of a digit and no letter of this
 * type is as wide as a digit. The shorter values sit in the middle of that box.
 */
export const reservedPriorityWidth = "sm:box-content sm:min-w-[8ch] sm:max-w-[8ch] sm:text-center";
