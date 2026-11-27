/**
 * The mark of each control a command-bar action of the Inbox presses (R11): the action finds the
 * control the page draws for it by this mark, inside the row, the card or the column's head in
 * hand (`useInboxKeymap.ts`). The mark's value is the action's own id in the keymap's catalogue
 * (`assign-to-me` for `inbox.assign-to-me`), so the control, the key that presses it and the
 * browser suite's selectors name one action, and a control and its key cannot name two.
 */

import { catalogueEntry, type KeymapActionId } from "../shell/keymap-catalogue";

/** An action of the Inbox's scope in the keymap's catalogue. */
export type InboxActionId = Extract<KeymapActionId, `inbox.${string}`>;

/** The attribute marking such a control. It is written through `inboxActionMark` and read
 *  through `inboxActionSelector`, so neither end spells the other's string. */
const INBOX_ACTION_ATTRIBUTE = "data-inbox-action";

/** The mark the control `action` presses carries: spread onto the control itself, or handed to a
 *  shared component through its `marks` (`CopyButton`, `DisclosureToggle`, `MultiSelect`'s
 *  trigger, `EarlierReplies`, `ReferencedByToggle`, `ApprovalChip`'s state button). */
export function inboxActionMark(
  action: InboxActionId
): Readonly<Record<typeof INBOX_ACTION_ATTRIBUTE, string>> {
  return { [INBOX_ACTION_ATTRIBUTE]: catalogueEntry(action).id };
}

/** The selector that finds the control `action` marks. */
export function inboxActionSelector(action: InboxActionId): string {
  return `[${INBOX_ACTION_ATTRIBUTE}="${inboxActionMark(action)[INBOX_ACTION_ATTRIBUTE]}"]`;
}
