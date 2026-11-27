import type { ScopeTable } from "./keymap-catalogue";

/** The `global` scope's actions, by id (`keymap-catalogue.ts` names each `global.<id>`):
 *  `app.tsx`. */
export const GLOBAL_ACTIONS = {
  // Fires while the reader is writing, so the key reaches the bar from inside a document's
  // editor too.
  search: {
    label: "Search and actions",
    keys: ["$mod+k"],
    palette: false,
    inEditable: true,
    surface: "global",
  },
  // No row: chosen from the bar it would only reopen the bar. Not `inEditable`, so a slash typed
  // into a document stays a slash, and the Agents page shadows it with its own filter box.
  "search-only": {
    label: "Search",
    keys: ["/"],
    palette: false,
    surface: "global",
  },
  help: {
    label: "Keyboard shortcuts",
    keys: ["?"],
    palette: true,
    surface: "global",
  },
  create: {
    label: "Create issue",
    keys: ["c"],
    palette: true,
    surface: "global",
  },
  "go-inbox": {
    label: "Go to Inbox",
    keys: ["g i"],
    palette: true,
    surface: "global",
  },
  "go-agents": {
    label: "Go to Agents",
    keys: ["g a"],
    palette: true,
    surface: "global",
  },
  "go-settings": {
    label: "Go to Settings",
    keys: ["g s"],
    palette: true,
    surface: "global",
  },
  "go-documents": {
    label: "Go to Documents",
    keys: ["g d"],
    palette: true,
    surface: "global",
  },
  // No row, for `global.search-only`'s reason.
  "go-project": {
    label: "Go to project…",
    keys: ["g p"],
    palette: false,
    surface: "global",
  },
  "go-priorities": {
    label: "Go to Priorities",
    keys: [],
    palette: true,
    surface: "global",
  },
  "go-delivery": {
    label: "Go to Delivery",
    keys: [],
    palette: true,
    surface: "global",
  },
  "go-pipeline": {
    label: "Go to Pipeline",
    keys: [],
    palette: true,
    surface: "global",
  },
  "inbox-drawer": {
    label: "Open the inbox",
    keys: ["i"],
    palette: true,
    surface: "global",
  },
  "toggle-sidebar": {
    label: "Toggle sidebar",
    keys: ["Shift+S"],
    palette: true,
    surface: "global",
  },
  // Every page but the new issue page registers it: the new page's frame draws Details in place of
  // the margin, and registers Details' own action instead (`app.tsx`).
  "toggle-margin": {
    label: "Toggle margin",
    keys: ["Shift+M"],
    palette: true,
    surface: "margin",
  },
  // The new issue page's frame (`app.tsx`), and Details' sheet on a phone (`margin/Margin.tsx`),
  // which registers it under `dialog`: the sheet is a dialog, which masks every global key. It
  // opens and closes Details, not the margin, so it is an action apart from Toggle margin, and a
  // key a person gives one of the two leaves the other as it was.
  "toggle-details": {
    label: "Open or close Details",
    keys: ["Shift+M"],
    palette: true,
    surface: "issue-outline",
  },
} as const satisfies ScopeTable;
