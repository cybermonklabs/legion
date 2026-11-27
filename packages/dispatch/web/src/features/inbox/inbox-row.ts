/** The attribute every Inbox ask row carries, valued with its ask id: the list's keyboard, viewport
 *  and jump target. Group headers and band labels never carry it. */
export const ROW_ATTRIBUTE = "data-inbox-row";

/** Every Inbox ask row. */
export const ROW_SELECTOR = `[${ROW_ATTRIBUTE}]`;

/** The attribute the Inbox's list column carries, with rows or with none: where the reader's own
 *  Send takes a row away, the focus goes to a control of this list (`landAfterOwnSend`). */
export const LIST_ATTRIBUTE = "data-inbox-list";

/** The attribute the Inbox's empty message carries: what a press of the selection bar that leaves
 *  no row hands the focus to (`usePaneSelection`). */
export const EMPTY_ATTRIBUTE = "data-inbox-empty";
