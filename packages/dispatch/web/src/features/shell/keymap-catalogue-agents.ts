import type { ScopeTable } from "./keymap-catalogue";

/** The `agents` scope's actions, by id (`keymap-catalogue.ts` names each `agents.<id>`): the
 *  Agents page, `agents/keyboard.ts`. Its `/` and `i` take global keys on purpose, which the
 *  refusal check accepts with a notice that the global action does not work there. */
export const AGENTS_ACTIONS = {
  search: {
    label: "Search agents",
    keys: ["/"],
    palette: true,
    surface: "agents",
  },
  next: {
    label: "Next agent",
    keys: ["j"],
    palette: false,
    surface: "agents",
  },
  previous: {
    label: "Previous agent",
    keys: ["k"],
    palette: false,
    surface: "agents",
  },
  compose: {
    label: "Message the focused agent",
    keys: [],
    fixedKeys: ["Enter"],
    palette: true,
    surface: "agents",
  },
  "issue-picker": {
    label: "Pick an issue for the message",
    keys: ["i"],
    palette: true,
    surface: "agents",
  },
  pin: {
    label: "Pin or unpin the focused agent",
    keys: ["Shift+P"],
    palette: true,
    writes: true,
    surface: "agents",
  },
  select: {
    label: "Select or deselect the focused agent",
    keys: ["x"],
    palette: false,
    surface: "agents",
  },
  back: {
    label: "Close the issue picker, or go back to the agent row",
    keys: [],
    fixedKeys: ["Escape"],
    palette: false,
    inEditable: true,
    surface: "agents",
  },
  filter: {
    label: "Open the filter panel",
    keys: ["Shift+F"],
    palette: true,
    surface: "agents",
  },
} as const satisfies ScopeTable;
