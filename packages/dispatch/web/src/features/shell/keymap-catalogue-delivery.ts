import type { ScopeTable } from "./keymap-catalogue";

/** The `delivery` scope's actions, by id (`keymap-catalogue.ts` names each `delivery.<id>`): the
 *  Delivery page's list, `delivery/PRList.tsx`, through `components/table-keys.ts`. */
export const DELIVERY_ACTIONS = {
  next: {
    label: "Next pull request",
    keys: [],
    fixedKeys: ["ArrowDown"],
    palette: false,
    surface: "delivery",
  },
  previous: {
    label: "Previous pull request",
    keys: [],
    fixedKeys: ["ArrowUp"],
    palette: false,
    surface: "delivery",
  },
  open: {
    label: "Open the pull request's details",
    keys: ["o"],
    palette: true,
    surface: "delivery",
  },
  "open-enter": {
    label: "Open the focused pull request",
    keys: [],
    fixedKeys: ["Enter"],
    palette: false,
    surface: "delivery",
  },
  filter: {
    label: "Open the filter panel",
    keys: ["Shift+F"],
    palette: true,
    surface: "delivery",
  },
} as const satisfies ScopeTable;
