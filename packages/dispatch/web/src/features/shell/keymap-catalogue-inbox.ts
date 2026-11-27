import type { ScopeTable } from "./keymap-catalogue";

/** The `inbox` scope's actions, by id (`keymap-catalogue.ts` names each `inbox.<id>`): the Inbox's
 *  keys, `inbox/useInboxKeymap.ts`, on its page and, under the `dialog` scope, in its drawer. */
export const INBOX_ACTIONS = {
  // Movement is what the arrow keys are for: a palette row that moves the cursor helps nobody,
  // so neither `j`, `k` nor the arrows is a row.
  next: {
    label: "Next row",
    keys: ["j"],
    palette: false,
    repeats: true,
    surface: "inbox",
  },
  previous: {
    label: "Previous row",
    keys: ["k"],
    palette: false,
    repeats: true,
    surface: "inbox",
  },
  arrows: {
    label: "Next / previous row from the list",
    keys: [],
    fixedKeys: ["ArrowDown", "ArrowUp"],
    palette: false,
    repeats: true,
    surface: "inbox",
  },
  // Scrolling the open document is a key pressed with the row in hand, as `j` and `k` are, so
  // none of the four scroll keys is a row either.
  "scroll-down": {
    label: "Scroll the open document down",
    keys: [],
    fixedKeys: ["Space"],
    palette: false,
    repeats: true,
    surface: "inbox",
  },
  "scroll-up": {
    label: "Scroll the open document up",
    keys: [],
    fixedKeys: ["Shift+Space"],
    palette: false,
    repeats: true,
    surface: "inbox",
  },
  "scroll-top": {
    label: "Scroll the open document to its top",
    keys: ["g g"],
    palette: false,
    surface: "inbox",
  },
  "scroll-end": {
    label: "Scroll the open document to its end",
    keys: ["Shift+G"],
    palette: false,
    surface: "inbox",
  },
  "read-pane": {
    label: "Move the focus into the reading pane",
    keys: ["l"],
    palette: true,
    surface: "inbox",
  },
  back: {
    label: "Back to the row, then clear the focused row",
    keys: [],
    fixedKeys: ["Escape"],
    palette: false,
    inEditable: true,
    surface: "inbox",
  },
  // Below 1280 px, from a row's reading view back to the list, at that row. Not a palette row,
  // as `back` is not.
  "leave-reading": {
    label: "Back to the list",
    keys: [],
    fixedKeys: ["Escape"],
    palette: false,
    inEditable: true,
    surface: "inbox",
  },
  answer: {
    label: "Answer the ask in hand",
    keys: [],
    fixedKeys: ["Enter"],
    palette: true,
    surface: "inbox",
  },
  option: {
    label: "Pick an option of the ask in hand",
    keys: [],
    fixedKeys: ["1", "2", "3", "4", "5", "6", "7", "8", "9", "0"],
    palette: false,
    surface: "inbox",
  },
  approve: {
    label: "Approve",
    keys: ["a"],
    palette: true,
    surface: "inbox",
  },
  "request-changes": {
    label: "Request changes",
    keys: ["Shift+R"],
    palette: true,
    surface: "inbox",
  },
  reply: {
    label: "Reply in the thread of the item in hand",
    keys: ["r"],
    palette: true,
    surface: "inbox",
  },
  snooze: {
    label: "Open the snooze menu",
    keys: ["b", "h"],
    palette: true,
    surface: "inbox",
  },
  priority: {
    label: "Open the priority menu",
    keys: ["p"],
    palette: true,
    surface: "inbox",
  },
  dismiss: {
    label: "Dismiss, or Move to Inbox",
    keys: ["e"],
    palette: true,
    surface: "inbox",
  },
  // Marking walks the list with the row in hand, as `j`/`k` do, so it is no palette row.
  select: {
    label: "Select or deselect the row",
    keys: ["x"],
    palette: false,
    surface: "inbox",
  },
  open: {
    label: "Open the row's issue, document or thread",
    keys: ["o"],
    palette: true,
    surface: "inbox",
  },
  "open-new-tab": {
    label: "Open in a new tab",
    keys: ["Shift+O"],
    palette: true,
    surface: "inbox",
  },
  "copy-ref": {
    label: "Copy the row's reference",
    keys: ["y"],
    palette: true,
    surface: "inbox",
  },
  // Not `inEditable`: inside the box itself a `search` input takes typed text (`keymap.ts`'s
  // `TEXT_INPUT_TYPES`), so the registry passes the key through and the reader types an `f`
  // rather than re-focusing what they are already in.
  search: {
    label: "Search asks",
    keys: ["f"],
    palette: true,
    surface: "inbox",
  },
  "leave-search": {
    label: "Back to the list from the search box",
    keys: [],
    fixedKeys: ["Escape"],
    palette: false,
    inEditable: true,
    surface: "inbox",
  },
  // From the bar's Snooze, where its list hands the focus back as it closes, back to the row the
  // bar was reached from. Not a palette row, as `back` is not.
  "back-from-bulk": {
    label: "Back to the list from the bulk snooze picker",
    keys: [],
    fixedKeys: ["Escape"],
    palette: false,
    inEditable: true,
    surface: "inbox",
  },
  "clear-selection": {
    label: "Clear the selection",
    keys: [],
    fixedKeys: ["Escape"],
    // The bar's Clear selection, a row of the command bar too (R11), which shows its Escape.
    palette: true,
    surface: "inbox",
  },
  filter: {
    label: "Open the filter panel",
    keys: ["Shift+F"],
    palette: true,
    surface: "inbox",
  },
  display: {
    label: "Open the display menu",
    keys: ["Shift+V"],
    palette: true,
    surface: "inbox",
  },
  // The command bar's actions for the item in hand, with no default key (R11).
  "assign-to-me": {
    label: "Assign to me",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  unsnooze: {
    label: "Un-snooze",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "mark-read": {
    label: "Mark read",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "earlier-replies": {
    label: "Show earlier replies",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "asker-handles": {
    label: "Show the asker's handles",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "copy-session-id": {
    label: "Copy session ID",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "copy-session-title": {
    label: "Copy session title",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "copy-tmux-target": {
    label: "Copy tmux target",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "hand-on": {
    label: "Hand on",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "referenced-by": {
    label: "Referenced by",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "earlier-approval-diff": {
    label: "Show the earlier approval's diff",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "ask-back": {
    label: "Ask back",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "ask-back-instead": {
    label: "Ask back instead",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "answer-anyway": {
    label: "Answer with it anyway",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  cancel: {
    label: "Cancel",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  decline: {
    label: "Decline",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  recipients: {
    label: "Recipients",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "change-answer": {
    label: "Change answer",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "previous-versions": {
    label: "Show previous versions",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "earlier-answers": {
    label: "Show earlier answers",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "open-at-section": {
    label: "Open page at the section",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "skip-to-answer": {
    label: "Skip to the answer",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "next-change": {
    label: "Next change",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "show-reviews": {
    label: "Show the reviews",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "select-all": {
    label: "Select all",
    keys: [],
    palette: true,
    surface: "inbox",
  },
  "bulk-dismiss": {
    label: "Dismiss, or Move to Inbox, the selected items",
    keys: [],
    palette: true,
    surface: "inbox",
  },
} as const satisfies ScopeTable;
