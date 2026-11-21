import type { ScopeTable } from "./keymap-catalogue";

/** The `issue` scope's actions, by id (`keymap-catalogue.ts` names each `issue.<id>`): today's
 *  issue page's, then the new issue page's outline's. */
export const ISSUE_ACTIONS = {
  // Today's issue page: `issue/IssuePage.tsx`, `issue/IssueHeader.tsx` and the Conversation tab.
  // The new issue page registers the actions of `issue-both` too: the header's keys
  // (`issue-page/useHeaderKeys.ts`), its command bar rows (`issue-page/useIssuePalette.ts`) and
  // the tab chords.
  status: {
    label: "Open the status menu",
    keys: ["s"],
    palette: true,
    surface: "issue-both",
  },
  priority: {
    label: "Open the priority menu",
    keys: ["p"],
    palette: true,
    surface: "issue-both",
  },
  "set-priority": {
    label: "Set priority P0–P3",
    keys: [],
    fixedKeys: ["0", "1", "2", "3"],
    palette: false,
    writes: true,
    surface: "issue-both",
  },
  labels: {
    label: "Edit labels",
    keys: ["l"],
    palette: true,
    surface: "issue",
  },
  title: {
    label: "Edit the title",
    keys: ["e"],
    palette: true,
    surface: "issue-both",
  },
  pin: {
    label: "Pin or unpin the issue",
    keys: ["Shift+P"],
    palette: true,
    writes: true,
    surface: "issue-both",
  },
  // The tab chords. The new issue page registers six of them too: the five of its filter row
  // (`issue-page/useIssueKeymap.ts`), where `t c` turns every filter off, and `t h`, which moves
  // to the issue's children in its outline (`issue-page/useTreeKeymap.ts`). Each is one action on
  // both pages, so a key a person gives it works on both, and its label names its purpose first
  // and says in brackets what it does on the new page.
  "tab-spec": {
    label: "Spec: open its tab (on the new page, its filter)",
    keys: ["t s"],
    palette: true,
    surface: "issue-both",
  },
  "tab-verification": {
    label: "Verification: open its tab (on the new page, its filter)",
    keys: ["t k"],
    palette: true,
    surface: "issue-both",
  },
  "tab-plan": {
    label: "Plan: open its tab (on the new page, its filter)",
    keys: ["t p"],
    palette: true,
    surface: "issue-both",
  },
  "tab-steps": {
    label: "Steps tab",
    keys: ["t e"],
    palette: true,
    surface: "issue",
  },
  "tab-conversation": {
    label: "Conversation: open its tab (on the new page, every filter off)",
    keys: ["t c"],
    palette: true,
    surface: "issue-both",
  },
  "tab-children": {
    label: "Children: open its tab (on the new page, move to them)",
    keys: ["t h"],
    palette: true,
    surface: "issue-both",
  },
  "tab-artifacts": {
    label: "Artifacts tab",
    keys: ["t a"],
    palette: true,
    surface: "issue",
  },
  "tab-activity": {
    label: "Activity tab",
    keys: ["t v"],
    palette: true,
    surface: "issue",
  },
  "tab-decisions": {
    label: "Decisions: open its tab (on the new page, its filter)",
    keys: ["t d"],
    palette: true,
    surface: "issue-both",
  },
  "tab-collaborators": {
    label: "Collaborators tab",
    keys: ["t o"],
    palette: true,
    surface: "issue",
  },
  "previous-tab": {
    label: "Previous tab",
    keys: ["["],
    palette: true,
    surface: "issue",
  },
  "next-tab": {
    label: "Next tab",
    keys: ["]"],
    palette: true,
    surface: "issue",
  },
  close: {
    label: "Close issue",
    keys: [],
    palette: true,
    writes: true,
    surface: "issue-both",
  },
  reopen: {
    label: "Reopen issue",
    keys: [],
    palette: true,
    writes: true,
    surface: "issue-both",
  },
  "set-p0": {
    label: "Set priority P0",
    keys: [],
    palette: true,
    writes: true,
    surface: "issue-both",
  },
  "set-p1": {
    label: "Set priority P1",
    keys: [],
    palette: true,
    writes: true,
    surface: "issue-both",
  },
  "set-p2": {
    label: "Set priority P2",
    keys: [],
    palette: true,
    writes: true,
    surface: "issue-both",
  },
  "set-p3": {
    label: "Set priority P3",
    keys: [],
    palette: true,
    writes: true,
    surface: "issue-both",
  },
  approve: {
    label: "Approve",
    keys: ["a"],
    palette: true,
    writes: true,
    surface: "issue-both",
  },
  "request-changes": {
    label: "Request changes",
    keys: ["Shift+R"],
    palette: true,
    surface: "issue-both",
  },
  "next-turn": {
    label: "Next turn",
    keys: ["j"],
    palette: false,
    surface: "issue",
  },
  "previous-turn": {
    label: "Previous turn",
    keys: ["k"],
    palette: false,
    surface: "issue",
  },
  // Expanding a turn opens no link, so the Conversation tab has no new-tab key.
  "toggle-turn": {
    label: "Expand or collapse the turn's thread",
    keys: ["o"],
    fixedKeys: ["Enter"],
    palette: true,
    surface: "issue",
  },
  "reply-turn": {
    label: "Reply in the turn's thread",
    keys: ["r"],
    palette: true,
    surface: "issue",
  },
  "copy-turn-ref": {
    label: "Copy the turn's reference",
    keys: ["y"],
    palette: true,
    surface: "issue",
  },
  "copy-turn-address": {
    label: "Copy the turn's web address",
    keys: ["Shift+Y"],
    palette: true,
    surface: "issue",
  },

  // The new issue page's outline: `issue-page/useTreeKeymap.ts`, in the `issue` scope, where
  // today's issue page is never drawn beside it (`KeySurface`). Moving through the rows, folding
  // them and stepping through a row's controls is what those keys are for, so none of them is a
  // row of the command bar; nor are `o` and `.`, which act on the row in hand, nor Tab, Enter and
  // Escape, which move the focus into the outline, into a row's text and out again. Home and End
  // are keys a person can change: no run reads which key was pressed, and neither is a key that
  // keeps its own action (`keymap-refusal.ts`).
  "tree-enter": {
    label: "Move into the tree",
    keys: [],
    fixedKeys: ["Tab", "Shift+Tab"],
    palette: false,
    surface: "issue-outline",
  },
  "tree-next": {
    label: "Move to the next row",
    keys: ["j"],
    fixedKeys: ["ArrowDown"],
    palette: false,
    surface: "issue-outline",
  },
  "tree-previous": {
    label: "Move to the previous row",
    keys: ["k"],
    fixedKeys: ["ArrowUp"],
    palette: false,
    surface: "issue-outline",
  },
  "tree-first": {
    label: "Move to the first row",
    keys: ["Home"],
    palette: false,
    surface: "issue-outline",
  },
  "tree-last": {
    label: "Move to the last row",
    keys: ["End"],
    palette: false,
    surface: "issue-outline",
  },
  "tree-unfold": {
    label: "Unfold the focused row, or move into it",
    keys: [],
    fixedKeys: ["ArrowRight"],
    palette: false,
    surface: "issue-outline",
  },
  "tree-fold": {
    label: "Fold the focused row, or move to its parent",
    keys: [],
    fixedKeys: ["ArrowLeft"],
    palette: false,
    surface: "issue-outline",
  },
  "tree-pin": {
    label: "Keep the focused row's body open",
    keys: ["o"],
    palette: false,
    surface: "issue-outline",
  },
  "tree-keep": {
    label: "Keep the focused decision",
    keys: ["Shift+K"],
    palette: true,
    writes: true,
    surface: "issue-outline",
  },
  "tree-change": {
    label: "Change the focused decision",
    keys: ["Shift+C"],
    palette: true,
    surface: "issue-outline",
  },
  "tree-link": {
    label: "Move to the next link or control on the focused row",
    keys: ["f"],
    palette: false,
    surface: "issue-outline",
  },
  "tree-zoom-in": {
    label: "Zoom into the focused row",
    keys: ["."],
    palette: false,
    surface: "issue-outline",
  },
  "tree-zoom-out": {
    label: "Zoom back out one level",
    keys: [","],
    palette: true,
    surface: "issue-outline",
  },
  "tree-question": {
    label: "Move to the next open question",
    keys: ["q"],
    palette: true,
    surface: "issue-outline",
  },
  "tree-decision": {
    label: "Move into the decision behind the focused row",
    keys: ["w"],
    palette: true,
    surface: "issue-outline",
  },
  "tree-reply": {
    label: "Reply to the focused row",
    keys: ["r"],
    palette: true,
    surface: "issue-outline",
  },
  "tree-answer": {
    label: "Answer the focused question",
    keys: ["a"],
    palette: true,
    surface: "issue-outline",
  },
  "tree-cycle": {
    label: "Cycle how much of the tree is drawn",
    keys: ["z"],
    palette: true,
    surface: "issue-outline",
  },
  "tree-edit": {
    label: "Put the caret in the focused row",
    keys: [],
    fixedKeys: ["Enter"],
    palette: false,
    surface: "issue-outline",
  },
  // Fires from the row's own text, the editor the caret is in, which is what it leaves.
  "tree-leave-caret": {
    label: "Leave the row's text",
    keys: [],
    fixedKeys: ["Escape"],
    palette: false,
    inEditable: true,
    surface: "issue-outline",
  },
  "tree-leave": {
    label: "Leave the control, then the tree",
    keys: [],
    fixedKeys: ["Escape"],
    palette: false,
    surface: "issue-outline",
  },
} as const satisfies ScopeTable;
