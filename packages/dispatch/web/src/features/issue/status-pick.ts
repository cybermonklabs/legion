import type { ListMarks, ListMenu } from "../../components/MultiSelect";

/** D5's rules for the status (`ListMenu`): the list opens on the status held and takes no digit,
 *  and every close gives the focus back where it was as the list opened - the page or the
 *  element `s` was pressed on, or the trigger a press opened it from. */
export const STATUS_MENU: ListMenu = { keys: {}, opensOn: "value" };

const STATUS_TRIGGER_ATTRIBUTE = "data-status-menu";
/** The status list's trigger mark, which the issue page's and the header's `s` find it by: the
 *  shared list hands out no element of its own. */
export const STATUS_MARKS: ListMarks = { trigger: { [STATUS_TRIGGER_ATTRIBUTE]: "" } };
/** The status list's trigger (`STATUS_MARKS`). */
export const STATUS_TRIGGER_SELECTOR = `[${STATUS_TRIGGER_ATTRIBUTE}]`;

/** The issue header's Status list's trigger carries this attribute, valued with the status it
 *  shows (`Todo` shows `todo`): a reader outside the page (the docs' screenshots) reads the status
 *  off it as it read a select's value. */
const STATUS_SHOWN_ATTRIBUTE = "data-issue-status";

/** The issue header's Status list's marks: `STATUS_MARKS`, and the status it shows
 *  (`STATUS_SHOWN_ATTRIBUTE`). */
export function statusMarksShowing(shown: string): ListMarks {
  return { trigger: { ...STATUS_MARKS.trigger, [STATUS_SHOWN_ATTRIBUTE]: shown } };
}
