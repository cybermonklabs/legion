import type { ScopeTable } from "./keymap-catalogue";

/** The `document` scope's actions, by id (`keymap-catalogue.ts` names each `document.<id>`): a
 *  document page, `doc/ProofDocument.tsx`. */
export const DOCUMENT_ACTIONS = {
  "comment-on-selection": {
    label: "Comment on selection",
    keys: ["$mod+Alt+m"],
    palette: false,
    inEditable: true,
    surface: "document",
  },
  "open-link": {
    label: "Open the link at the caret",
    keys: ["Alt+Enter"],
    palette: false,
    inEditable: true,
    surface: "document",
  },
  "leave-editor": {
    label: "Take the focus out of the editor",
    keys: [],
    fixedKeys: ["Escape"],
    palette: false,
    inEditable: true,
    surface: "document",
  },
} as const satisfies ScopeTable;
