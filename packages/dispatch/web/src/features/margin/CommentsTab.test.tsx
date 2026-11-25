import { expect, spyOn, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import type { AwayDraft } from "../conversation/unsent-drafts";
import { MarginSheet } from "./MarginSheet";
import { DESKTOP_WIDTH, specArtifact, stubViewport } from "./margin-fixture";

/** The draft the margin's compose kept for the words `quote` of the spec. */
function keptFor(quote: string): AwayDraft {
  return {
    anchor: { artifact: specArtifact.id, mark_id: `mark-${quote}`, quote },
    draft: { body: `Draft for ${quote}`, replacement: "" },
    owner: { issueKey: "CORE-1", kind: "issue" },
    replyTo: null,
  };
}

/** An issue's margin over its spec with no compose open there, drawing the drafts its compose
 *  kept for words of the spec (`MarginKeptDrafts`) under the Comments tab, each until the
 *  reader's Discard my draft drops it. */
function KeptDraftsMargin({ drafts }: { drafts: readonly AwayDraft[] }): ReactNode {
  const [kept, setKept] = useState(drafts);
  return (
    <MarginSheet
      model={{
        actions: {
          closeComposer: () => {},
          discardKeptDraft: (away) => setKept((list) => list.filter((other) => other !== away)),
          onAction: () => {},
          onComposerKindChange: () => undefined,
          onEdit: async () => undefined,
          onEditingChange: () => {},
          onRetryAction: () => {},
          onRetryAnsweredAsk: undefined,
          onRetryComments: () => {},
          onRetryIssue: () => {},
          onToggleResolved: () => {},
          onToggleThread: () => {},
          onUnpin: () => {},
          onUnpinConversation: () => {},
        },
        composer: undefined,
        filter: { blockId: undefined, clear: () => {} },
        items: {
          actionFailure: undefined,
          answeredAsksPending: false,
          asksPending: false,
          commentsError: false,
          commentsPending: false,
          decisions: [],
          foldedAskCount: 0,
          historicalAsks: [],
          isClosed: false,
          issueError: false,
          issuePending: false,
          keptDrafts: kept,
          marginRef: { current: null },
          needsYou: [],
          onSelectCard: () => {},
          openAskCount: 0,
          owner: { key: "CORE-1", kind: "issue" },
          pendingActionIds: new Set<string>(),
          pinned: [],
          pinnedConversations: [],
          pinnedIds: [],
          resolvedThreads: [],
          threads: [],
          viewerLogin: "alice",
          visibleArtifact: specArtifact,
        },
        placement: { blockPlacements: new Map(), markPlacements: new Map() },
        selection: {
          editingCommentId: undefined,
          expandedThreadKey: undefined,
          hoveredItemId: undefined,
          hoveredMarkId: undefined,
          landedCard: undefined,
          routeMarkedItemId: undefined,
          savingCommentEditId: undefined,
          selectedItemId: undefined,
          showResolved: false,
        },
        sheet: {
          closeThread: () => {},
          expanded: true,
          keepFocusOnClose: () => false,
          replySending: false,
          thread: undefined,
          toggle: () => {},
        },
        tab: { set: () => {}, value: "comments" },
      }}
    />
  );
}

// Discard my draft takes its draft's offer away, and with it the button that held the focus. The
// focus goes on to the next draft's Discard my draft, which stands at its place now, else to the
// previous draft's, else, with the list gone, to the Comments tab the list was drawn under: each
// with no scroll (`MarginKeptDrafts`).
for (const row of [
  {
    drafts: ["first", "second", "third"],
    lands: "Discard my draft for “third”",
    press: "second",
    to: "the next draft's Discard my draft",
  },
  {
    drafts: ["first", "second"],
    lands: "Discard my draft for “first”",
    press: "second",
    to: "the previous draft's Discard my draft",
  },
  { drafts: ["first"], lands: "the Comments tab", press: "first", to: "the Comments tab" },
]) {
  test(`Discard my draft on one of ${row.drafts.length} kept drafts hands the focus to ${row.to}, with no scroll`, async () => {
    const restoreViewport = stubViewport(DESKTOP_WIDTH);
    const user = userEvent.setup();
    const view = render(<KeptDraftsMargin drafts={row.drafts.map(keptFor)} />);
    // The controls the focus can land on, by name: each draft's Discard my draft, by its words,
    // and the Comments tab.
    const buttons = screen.getAllByRole("button", { name: "Discard my draft" });
    const controls = new Map<string, HTMLElement>([
      ...row.drafts.map(
        (quote, index) => [`Discard my draft for “${quote}”`, buttons[index]] as const
      ),
      ["the Comments tab", screen.getByRole("tab", { name: "Comments" })] as const,
    ]);
    const pressed = controls.get(`Discard my draft for “${row.press}”`);
    const target = controls.get(row.lands);
    if (pressed === undefined || target === undefined) throw new Error("the row names no control");
    const focuses = spyOn(target, "focus");

    try {
      await user.click(pressed);
      const focused = document.activeElement;
      expect({
        focused:
          [...controls].find(([, control]) => control === focused)?.[0] ??
          (focused === document.body ? "the page's body" : focused?.outerHTML),
        focuses: focuses.mock.calls,
        kept: screen
          .queryAllByText(/^You wrote this draft for the words/)
          .map((line) => /“(.*)”/.exec(line.textContent ?? "")?.[1]),
      }).toEqual({
        focused: row.lands,
        focuses: [[{ preventScroll: true }]],
        kept: row.drafts.filter((quote) => quote !== row.press),
      });
    } finally {
      focuses.mockRestore();
      view.unmount();
      restoreViewport();
    }
  });
}
