import type { ScopeTable } from "./keymap-catalogue";

/** The `board` scope's actions, by id (`keymap-catalogue.ts` names each `board.<id>`): the Board,
 *  `project/IssueBoard.tsx`. */
export const BOARD_ACTIONS = {
  // Moving the cursor, by card or by column, is no row, for the Inbox's `j` and `k`'s reason.
  next: {
    label: "Next card",
    keys: ["j"],
    palette: false,
    surface: "board",
  },
  previous: {
    label: "Previous card",
    keys: ["k"],
    palette: false,
    surface: "board",
  },
  column: {
    label: "Next column",
    keys: ["l"],
    palette: false,
    surface: "board",
  },
  "previous-column": {
    label: "Previous column",
    keys: ["h"],
    palette: false,
    surface: "board",
  },
  "arrows-vertical": {
    label: "Next / previous card while one is focused",
    keys: [],
    fixedKeys: ["ArrowDown", "ArrowUp"],
    palette: false,
    surface: "board",
  },
  "arrows-horizontal": {
    label: "Next / previous column while one is focused",
    keys: [],
    fixedKeys: ["ArrowRight", "ArrowLeft"],
    palette: false,
    surface: "board",
  },
  "move-down": {
    label: "Move card down",
    keys: ["Shift+J"],
    palette: true,
    surface: "board",
  },
  "move-up": {
    label: "Move card up",
    keys: ["Shift+K"],
    palette: true,
    surface: "board",
  },
  "move-next": {
    label: "Move card to the next status",
    keys: ["Shift+L"],
    palette: true,
    surface: "board",
  },
  "move-previous": {
    label: "Move card to the previous status",
    keys: ["Shift+H"],
    palette: true,
    surface: "board",
  },
  open: {
    label: "Open issue",
    keys: ["o"],
    palette: true,
    surface: "board",
  },
  "open-enter": {
    label: "Open the focused card's issue",
    keys: [],
    fixedKeys: ["Enter"],
    palette: false,
    surface: "board",
  },
  "open-new-tab": {
    label: "Open in a new tab",
    keys: ["Shift+O"],
    palette: true,
    surface: "board",
  },
  priority: {
    label: "Open the card's priority menu",
    keys: ["p"],
    palette: true,
    surface: "board",
  },
  "copy-ref": {
    label: "Copy the card's issue reference",
    keys: ["y"],
    palette: true,
    surface: "board",
  },
  "copy-key": {
    label: "Copy the card's issue key",
    keys: ["Shift+Y"],
    palette: true,
    surface: "board",
  },
  back: {
    label: "Back to the card, then out",
    keys: [],
    fixedKeys: ["Escape"],
    palette: false,
    inEditable: true,
    surface: "board",
  },
  filter: {
    label: "Open the filter panel",
    keys: ["Shift+F"],
    palette: true,
    surface: "board",
  },
} as const satisfies ScopeTable;
