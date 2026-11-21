import type { ScopeTable } from "./keymap-catalogue";

/** The `project` scope's actions, by id (`keymap-catalogue.ts` names each `project.<id>`): a
 *  project, `project/ProjectPage.tsx`, and the row keys every list of the `project` scope shares
 *  (`components/table-keys.ts`): the List, the Documents list, the Priorities page and a
 *  priority's page. One action each, whichever list draws it (R14). */
export const PROJECT_ACTIONS = {
  "toggle-view": {
    label: "Toggle List / Board",
    keys: ["v"],
    palette: true,
    surface: "project-page",
  },
  next: {
    label: "Next row",
    keys: ["j"],
    palette: false,
    surface: "project-list",
  },
  previous: {
    label: "Previous row",
    keys: ["k"],
    palette: false,
    surface: "project-list",
  },
  open: {
    label: "Open",
    keys: ["o"],
    palette: true,
    surface: "project-list",
  },
  "open-enter": {
    label: "Open the focused row",
    keys: [],
    fixedKeys: ["Enter"],
    palette: false,
    surface: "project-list",
  },
  "open-new-tab": {
    label: "Open in a new tab",
    keys: ["Shift+O"],
    palette: true,
    surface: "project-list",
  },
  select: {
    label: "Select or deselect the focused row",
    keys: ["x"],
    palette: false,
    surface: "project-list",
  },
  filter: {
    label: "Open the filter panel",
    keys: ["Shift+F"],
    palette: true,
    surface: "project-list",
  },
  display: {
    label: "Open the display menu",
    keys: ["Shift+V"],
    palette: true,
    surface: "project-list",
  },
} as const satisfies ScopeTable;
