import type { ScopeTable } from "./keymap-catalogue";

/** The `architecture` scope's actions, by id (`keymap-catalogue.ts` names each
 *  `architecture.<id>`): a project's Architecture page, `architecture/ArchitecturePage.tsx`. */
export const ARCHITECTURE_ACTIONS = {
  next: {
    label: "Next component",
    keys: ["j"],
    palette: false,
    surface: "architecture",
  },
  previous: {
    label: "Previous component",
    keys: ["k"],
    palette: false,
    surface: "architecture",
  },
  arrows: {
    label: "Next / previous component while one is focused",
    keys: [],
    fixedKeys: ["ArrowDown", "ArrowUp"],
    palette: false,
    surface: "architecture",
  },
  open: {
    label: "Open the focused component: its children, else its details",
    keys: ["o"],
    fixedKeys: ["Enter"],
    palette: true,
    surface: "architecture",
  },
  descend: {
    label: "Into the focused component's children",
    keys: ["l"],
    fixedKeys: ["ArrowRight"],
    palette: false,
    surface: "architecture",
  },
  ascend: {
    label: "Up one level",
    keys: ["h"],
    fixedKeys: ["ArrowLeft"],
    palette: true,
    surface: "architecture",
  },
  back: {
    label: "Back to the component row, then out",
    keys: [],
    fixedKeys: ["Escape"],
    palette: false,
    inEditable: true,
    surface: "architecture",
  },
  "open-new-tab": {
    label: "Open in a new tab",
    keys: ["Shift+O"],
    palette: true,
    surface: "architecture",
  },
} as const satisfies ScopeTable;
