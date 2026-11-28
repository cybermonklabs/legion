import { afterEach, beforeAll, expect, test } from "bun:test";
import { cleanup, render } from "@testing-library/react";
import type { ComponentProps } from "react";

import type { Artifact } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { MarginSheet } from "./MarginSheet";

/**
 * Below 1280 px, `styles.css` docks the editor's selection bar above the expanded review sheet by
 * `data-margin-sheet-expanded`, which the sheet sets on `<body>` while it stands expanded - not by
 * `body:has()` of the sheet's own `data-expanded`. Chromium checks a `:has()` anchored at `<body>`
 * against every element under it, and the rule's `:has()` held at every width below 1280 px, so on
 * the new issue page at 390 px each row a fold took away, or an unfold added, restyled each
 * button, paragraph and text of the page: 3,103 elements of a fold of INFRA-1196's size, where the
 * same fold restyles 12 at 1440 px (INFRA-5939, decision 601). Each case reads `<body>` as the
 * docking rule reads it.
 */

// The sheet's cards draw through the shared renderer, whose Markdown engine loads lazily.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(() => {
  cleanup();
});

const specArtifact: Artifact = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  project: "CORE",
  kind: "doc",
  name: "Spec",
  primary: true,
  ref_key: "CORE-1/spec",
  session_id: null,
  slug: "spec",
  versions: [],
};

/** An issue's margin, empty, its sheet expanded or not. */
function model(expanded: boolean): ComponentProps<typeof MarginSheet>["model"] {
  return {
    actions: {
      closeComposer: () => {},
      discardKeptDraft: () => {},
      onAction: () => {},
      onComposerKindChange: () => undefined,
      onEdit: async () => undefined,
      onRetryAction: () => {},
      onUnpin: () => {},
      onUnpinConversation: () => {},
      onRetryAnsweredAsk: undefined,
      onRetryComments: () => {},
      onRetryIssue: () => {},
      onToggleResolved: () => {},
      onToggleThread: () => {},
      onEditingChange: () => {},
    },
    composer: undefined,
    items: {
      actionFailure: undefined,
      answeredAsksPending: false,
      asksPending: false,
      commentsPending: false,
      commentsError: false,
      decisions: [],
      historicalAsks: [],
      keptDrafts: [],
      marginRef: { current: null },
      isClosed: false,
      owner: { key: "CORE-1", kind: "issue" },
      issuePending: false,
      openAskCount: 0,
      onSelectCard: () => {},
      needsYou: [],
      issueError: false,
      pinned: [],
      pinnedConversations: [],
      pendingActionIds: new Set<string>(),
      pinnedIds: [],
      resolvedThreads: [],
      foldedAskCount: 0,
      threads: [],
      viewerLogin: "alice",
      visibleArtifact: specArtifact,
    },
    placement: { blockPlacements: new Map(), markPlacements: new Map() },
    selection: {
      expandedThreadKey: undefined,
      editingCommentId: undefined,
      savingCommentEditId: undefined,
      hoveredItemId: undefined,
      hoveredMarkId: undefined,
      landedCard: undefined,
      routeMarkedItemId: undefined,
      selectedItemId: undefined,
      showResolved: false,
    },
    sheet: {
      closeThread: () => {},
      expanded,
      replySending: false,
      thread: undefined,
      toggle: () => {},
    },
    filter: { blockId: undefined, clear: () => {} },
    tab: { set: () => {}, value: "comments" },
  };
}

/** What the sheet says of itself, and whether `<body>` carries the mark the docking rule reads. */
function state(): [string | null | undefined, boolean] {
  return [
    document.querySelector('[data-testid="margin-sheet"]')?.getAttribute("data-expanded"),
    document.body.hasAttribute("data-margin-sheet-expanded"),
  ];
}

test("<body> carries data-margin-sheet-expanded while the review sheet stands expanded", () => {
  const view = render(<MarginSheet model={model(false)} />);
  expect(state(), "a collapsed sheet marked <body>").toEqual(["false", false]);
  view.rerender(<MarginSheet model={model(true)} />);
  expect(state(), "the expanded sheet left <body> unmarked").toEqual(["true", true]);
});

test("closing the review sheet takes data-margin-sheet-expanded off <body>", () => {
  const view = render(<MarginSheet model={model(true)} />);
  expect(state()).toEqual(["true", true]);
  view.rerender(<MarginSheet model={model(false)} />);
  expect(state(), "<body> kept the mark of a closed sheet").toEqual(["false", false]);
});

test("unmounting an expanded review sheet takes data-margin-sheet-expanded off <body>", () => {
  const view = render(<MarginSheet model={model(true)} />);
  expect(state()).toEqual(["true", true]);
  view.unmount();
  expect(
    document.body.hasAttribute("data-margin-sheet-expanded"),
    "<body> kept the mark of a sheet that is gone"
  ).toBe(false);
});
