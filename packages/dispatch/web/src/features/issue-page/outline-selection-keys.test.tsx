import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useCallback } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type { Artifact, Comment } from "../../api/types";
import { issueAsk, mockAskReads, selectWords, stateCardBox } from "../inbox/inbox-fixture";
import { MarginSheet, type MarginSheetModel } from "../margin/MarginSheet";
import { MarginProvider } from "../margin/margin-context";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { appKeymap } from "../shell/keymap";
import { KEY, node } from "./decisions-filter/decisions-fixtures";
import { DocumentAdmissionProvider } from "./document-admission";
import { DocumentStages } from "./document-stage";
import { FocusedNodeProvider } from "./focused-node";
import { IssueMarginCards } from "./IssueMargin";
import { IssueTree } from "./IssueTree";
import { DETAILS_ATTRIBUTE, MarginSlotProvider, useFillMarginSlot } from "./margin-slot";
import { useIssueKeymap } from "./useIssueKeymap";

/**
 * The selection bar's `q` and `d` on the new issue page (INFRA-3231's R7): the page's scope is
 * `issue` (`useIssueKeymap.ts`), and its Details, drawn beside the page from the phone's width up,
 * is no dialog, so the words a reader selects in a card of Details are quoted and defined by the
 * keys of that scope, whether the focus is on the page or in Details. The outline's own `q`, which
 * moves to the next open question, stands down while such words are selected, and acts as before
 * while none are.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

const originalMatchMedia = window.matchMedia;
afterEach(() => {
  window.matchMedia = originalMatchMedia;
  window.getSelection()?.removeAllRanges();
});

/** Answers every media query as a 1440 px window does: a `max-width` in px, a `min-width` in
 *  rem, and `not all and` either (`IssueTree.test.tsx`'s `setViewport`). */
function desktopWidth(): void {
  const width = 1440;
  window.matchMedia = ((query: string) => {
    const max = /max-width:\s*([\d.]+)px/.exec(query)?.[1];
    const min = /min-width:\s*([\d.]+)rem/.exec(query)?.[1];
    const matched =
      max !== undefined ? width <= Number(max) : min !== undefined && width >= Number(min) * 16;
    return {
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query.startsWith("not all and ") ? !matched : matched,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    } as MediaQueryList;
  }) as typeof window.matchMedia;
}

/** The outline: an issue holding a comment, then an open question, which `q` moves to. */
const OUTLINE = node({
  children: [
    node({ id: "comment-1", text: "A comment first.", type: "comment" }),
    node({ id: "question-1", question: true, state: "open", text: "Ship it?", type: "ask" }),
  ],
  held: 2,
  id: KEY,
  type: "issue",
});

const spec: Artifact = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: KEY,
  kind: "doc",
  name: "Spec",
  primary: true,
  project: "CORE",
  ref_key: `${KEY}/spec`,
  session_id: null,
  slug: "spec",
  versions: [],
};

/** The thread Details draws open, whose reply box a quote lands in. */
const root: Comment = {
  anchor: null,
  ask_id: null,
  author: { id: "bob", kind: "user" },
  body: "Review this paragraph.",
  created_at: "2026-09-10T00:00:00Z",
  edited_at: null,
  id: "comment-details",
  issue_key: KEY,
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
  ...commentDeliveryFields(),
};

/** The open question Details draws as a card, whose question's words Define reads. */
const ask = issueAsk({ id: "ask-details", question: "Which release path?" });

/** Details' model: the open thread and the question's card, as the drawer draws them. */
function detailsModel(): MarginSheetModel {
  return {
    actions: {
      closeComposer: () => {},
      discardKeptDraft: () => {},
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
      keptDrafts: [],
      marginRef: { current: null },
      needsYou: [ask],
      onSelectCard: () => {},
      openAskCount: 1,
      owner: { key: KEY, kind: "issue" },
      pendingActionIds: new Set<string>(),
      pinned: [],
      pinnedConversations: [],
      pinnedIds: [],
      resolvedThreads: [],
      threads: [
        {
          anchor: root.anchor,
          key: root.id,
          lastReplyAt: undefined,
          replies: [],
          resolved: false,
          root: { comment: root, kind: "comment" },
        },
      ],
      viewerLogin: "alice",
      visibleArtifact: spec,
    },
    placement: { blockPlacements: new Map(), markPlacements: new Map() },
    selection: {
      editingCommentId: undefined,
      expandedThreadKey: root.id,
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
      replySending: false,
      thread: undefined,
      toggle: () => {},
    },
    tab: { set: () => {}, value: "comments" },
  };
}

/** The page's own keys: its scope, its view row's chords and the selection bar's two keys. */
function PageKeys(): ReactNode {
  useIssueKeymap(KEY);
  return null;
}

/** The host of Details' panel, which these cases draw none of. */
const HOLDS_NO_PANEL = () => {};

/** Details' fill of the margin's slot while a document's editor is on the page, as
 *  `slots/details.tsx`'s `DetailsFill` fills it then: the margin's own cards, the threads and
 *  questions of the open document, above the panel, which these keys do not read. */
function DetailsFill(): null {
  const fill = useCallback(
    (conversation: ReactNode) => (
      <IssueMarginCards conversation={conversation} panelHost={HOLDS_NO_PANEL} />
    ),
    []
  );
  useFillMarginSlot(fill);
  return null;
}

/** The page as far as these keys go: its keys, the outline with its keys, and Details open beside
 *  it, which takes the focus on its Close as it opens (`Margin.tsx`'s `DetailsDrawer`). */
function renderPage() {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  return render(
    <MemoryRouter initialEntries={[`/issues/${KEY}`]}>
      <QueryClientProvider client={client}>
        <KeymapProvider>
          <MarginSlotProvider>
            <MarginProvider>
              <FocusedNodeProvider>
                <DocumentAdmissionProvider>
                  <DocumentStages>
                    <PageKeys />
                    <IssueTree
                      artifacts={new Map()}
                      context={new Set()}
                      filtering={false}
                      focusReveal={false}
                      holdsEscape={false}
                      isClosed={false}
                      onEditing={() => {}}
                      readsHeld
                      readsSettled
                      reveal={undefined}
                      root={OUTLINE}
                      rootRow
                    />
                  </DocumentStages>
                </DocumentAdmissionProvider>
              </FocusedNodeProvider>
              <aside aria-label="Details" {...{ [DETAILS_ATTRIBUTE]: "" }}>
                <button type="button">Close</button>
                <DetailsFill />
                <MarginSheet drawer model={detailsModel()} />
              </aside>
            </MarginProvider>
          </MarginSlotProvider>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Where the reader's focus stands as they select words with the mouse: on the page itself, where
 *  a press on words that hold no control leaves it, or on Details' Close, which takes it as
 *  Details opens. */
const PLACES = {
  details: () => {
    const details = screen.getByRole("complementary", { name: "Details" });
    act(() => within(details).getByRole("button", { name: "Close" }).focus());
  },
  page: () => {
    act(() => (document.activeElement as HTMLElement | null)?.blur());
    expect(document.activeElement).toBe(document.body);
  },
} as const;

/** The Details card of the open thread, once its words are drawn. */
async function threadCard(): Promise<HTMLElement> {
  const card = await screen.findByTestId(`margin-comment-${root.id}`);
  await waitFor(() => {
    if (card.querySelector(".dispatch-markdown")?.textContent !== root.body) {
      throw new Error("the comment's words have not rendered");
    }
  });
  return card;
}

/** The Details card of the open question, once its words are drawn. */
async function askCard(): Promise<HTMLElement> {
  const card = await waitFor(() => {
    const found = document.querySelector<HTMLElement>(`[data-ask-card="${ask.id}"]`);
    if (found === null) throw new Error("Details draws no card for the question");
    return found;
  });
  await waitFor(() => {
    const question = card.querySelector("[data-ask-question] .dispatch-markdown");
    if (question?.textContent !== ask.question) {
      throw new Error("the question's words have not rendered");
    }
  });
  stateCardBox(card);
  return card;
}

/** The outline's row that holds the focus, if one does. */
function focusedRowId(): string | undefined {
  return (
    document.activeElement?.closest("[data-node-row]")?.getAttribute("data-node-id") ?? undefined
  );
}

for (const [place, focus] of Object.entries(PLACES)) {
  test(`with the focus ${place === "page" ? "on the page" : "in Details"}, q quotes the words selected in a Details thread`, async () => {
    desktopWidth();
    const user = userEvent.setup();
    const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
      event_head: "0",
      kind: "user",
      login: "alice",
    });
    const getAsk = mockAskReads([ask]);
    const view = renderPage();
    try {
      const card = await threadCard();
      focus();
      selectWords(card, ".dispatch-markdown");
      expect(appKeymap.conflicts()).toEqual([]);
      await user.keyboard("q");
      const reply = within(card).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
      await waitFor(() => expect(reply.value).toBe(`> ${root.body}\n\n`));
      expect(focusedRowId(), "q moved to the next open question").not.toBe("question-1");
    } finally {
      view.unmount();
      getAsk.mockRestore();
      whoAmI.mockRestore();
    }
  });

  test(`with the focus ${place === "page" ? "on the page" : "in Details"}, d defines the words selected in a Details question`, async () => {
    desktopWidth();
    const user = userEvent.setup();
    const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
      event_head: "0",
      kind: "user",
      login: "alice",
    });
    const getAsk = mockAskReads([ask]);
    const defineWords = spyOn(api, "defineWords").mockImplementation(() => new Promise(() => {}));
    const view = renderPage();
    try {
      const card = await askCard();
      focus();
      selectWords(card, "[data-ask-question] .dispatch-markdown");
      expect(appKeymap.conflicts()).toEqual([]);
      await user.keyboard("d");
      await waitFor(() =>
        expect(defineWords).toHaveBeenCalledWith(
          { ask_id: ask.id, words: [ask.question] },
          expect.anything()
        )
      );
    } finally {
      view.unmount();
      defineWords.mockRestore();
      getAsk.mockRestore();
      whoAmI.mockRestore();
    }
  });
}

// With no words selected the two keys are what the page had: `q` moves to the next open question,
// and `d` does nothing here.
test("with no words selected, q moves to the next open question and d defines nothing", async () => {
  desktopWidth();
  const user = userEvent.setup();
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const getAsk = mockAskReads([ask]);
  const defineWords = spyOn(api, "defineWords").mockImplementation(() => new Promise(() => {}));
  const view = renderPage();
  try {
    await threadCard();
    PLACES.page();
    expect(appKeymap.conflicts()).toEqual([]);
    await user.keyboard("d");
    expect(defineWords).not.toHaveBeenCalled();
    await user.keyboard("q");
    await waitFor(() => expect(focusedRowId()).toBe("question-1"));
  } finally {
    view.unmount();
    defineWords.mockRestore();
    getAsk.mockRestore();
    whoAmI.mockRestore();
  }
});
