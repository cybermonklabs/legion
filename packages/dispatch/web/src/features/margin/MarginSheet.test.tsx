import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
import type { Artifact, Comment } from "../../api/types";
import { issueAsk, mockAskReads, selectWords, stateCardBox } from "../inbox/inbox-fixture";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { useKeymapScope } from "../shell/keymap";
import { COMPACT_VIEWPORT_QUERY } from "../shell/useDialog";
import { useSelectionBarKeys } from "../threads/quote-reply";
import { MarginSheet } from "./MarginSheet";

beforeAll(async () => {
  await warmMarkdownRenderer();
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

test("an issue margin offers Comments and Pinned, Comments first", () => {
  const view = render(
    <MarginSheet
      model={{
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
          openAskCount: 3,
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
        placement: {
          blockPlacements: new Map(),
          markPlacements: new Map(),
        },
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
          expanded: false,
          replySending: false,
          thread: undefined,
          toggle: () => {},
        },
        filter: { blockId: undefined, clear: () => {} },
        tab: {
          set: () => {},
          value: "comments",
        },
      }}
    />
  );

  try {
    expect(screen.getByRole("button", { name: "Open review panel (3 open asks)" })).not.toBeNull();
    expect(screen.getByRole("tab", { name: "Comments" })).toHaveAttribute("aria-selected", "true");
    expect(
      within(view.container)
        .getAllByRole("tab")
        .map((tab) => tab.textContent)
    ).toEqual(["Comments", "Pinned"]);
    expect(
      within(view.container).queryByRole("tab", { name: "Artifacts" })
    ).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});
test("a document owner shows the Comments tab and comment composer only", () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MarginSheet
        model={{
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
          composer: {
            anchor: { artifact: specArtifact.id, mark_id: "mark-1", quote: "selected" },
            kind: "comment",
            owner: { artifactId: specArtifact.id, kind: "artifact", project: "CORE" },
            seq: 1,
            turnedAway: false,
          },
          items: {
            actionFailure: undefined,
            answeredAsksPending: false,
            asksPending: false,
            commentsError: false,
            commentsPending: false,
            decisions: [],
            historicalAsks: [],
            keptDrafts: [],
            isClosed: false,
            issueError: false,
            issuePending: false,
            marginRef: { current: null },
            needsYou: [],
            onSelectCard: () => {},
            openAskCount: 0,
            owner: {
              artifactId: "artifact-1",
              kind: "document",
              project: "CORE",
              slug: "design-notes",
            },
            pendingActionIds: new Set<string>(),
            pinned: [],
            pinnedConversations: [],
            pinnedIds: [],
            resolvedThreads: [],
            foldedAskCount: 0,
            threads: [],
            viewerLogin: "alice",
            visibleArtifact: {
              ...specArtifact,
              issue_key: null,
              primary: false,
              slug: "design-notes",
            },
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
            expanded: true,
            replySending: false,
            thread: undefined,
            toggle: () => {},
          },
          filter: { blockId: undefined, clear: () => {} },
          tab: { set: () => {}, value: "comments" },
        }}
      />
    </QueryClientProvider>
  );

  try {
    expect(
      within(view.container)
        .getAllByRole("tab")
        .map((tab) => tab.textContent)
    ).toEqual(["Comments"]);
    expect(screen.getByRole("form", { name: "Comment composer" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("MarginSheet shows the selected phone thread in a full-height view with a Back button", async () => {
  const user = userEvent.setup();
  const root: Comment = {
    anchor: {
      artifact_id: specArtifact.id,
      block_id: null,
      mark_id: "mark-1",
      orphaned: false,
      quote: "selected text",
      version: 1,
    },
    ask_id: null,
    turn: null,
    author: { id: "alice", kind: "user" },
    body: "Review this paragraph.",
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id: "comment-1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    ...commentDeliveryFields(),
  };
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: true,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  let closed = false;
  let sheetClosed = false;
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <KeymapProvider>
        <MarginSheet
          model={{
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
              commentsError: false,
              commentsPending: false,
              decisions: [],
              historicalAsks: [],
              keptDrafts: [],
              isClosed: false,
              issueError: false,
              owner: { key: "CORE-1", kind: "issue" },
              issuePending: false,
              marginRef: { current: null },
              needsYou: [],
              onSelectCard: () => {},
              openAskCount: 0,
              pendingActionIds: new Set<string>(),
              pinned: [],
              pinnedConversations: [],
              pinnedIds: [],
              resolvedThreads: [],
              foldedAskCount: 0,
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
              visibleArtifact: specArtifact,
            },
            placement: { blockPlacements: new Map(), markPlacements: new Map() },
            selection: {
              expandedThreadKey: root.id,
              editingCommentId: undefined,
              savingCommentEditId: undefined,
              hoveredItemId: undefined,
              hoveredMarkId: undefined,
              landedCard: undefined,
              routeMarkedItemId: undefined,
              selectedItemId: root.id,
              showResolved: false,
            },
            sheet: {
              closeThread: () => {
                closed = true;
              },
              expanded: true,
              replySending: false,
              thread: {
                anchor: root.anchor,
                key: root.id,
                lastReplyAt: undefined,
                replies: [],
                resolved: false,
                root: { comment: root, kind: "comment" },
              },
              toggle: (expanded) => {
                sheetClosed = expanded === false;
              },
            },
            filter: { blockId: undefined, clear: () => {} },
            tab: { set: () => {}, value: "comments" },
          }}
        />
      </KeymapProvider>
    </QueryClientProvider>
  );

  try {
    const thread = screen.getByRole("dialog", { name: "Thread" });
    expect(thread.className).toContain("inset-0");
    expect(within(thread).getByTestId(`margin-comment-${root.id}`)).not.toBeNull();
    await user.keyboard("{Escape}");
    expect(closed).toBe(true);
    expect(sheetClosed).toBe(false);
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
  }
});

test("the standalone document phone sheet shows each suggestion's diff and Accept/Reject without opening the thread", async () => {
  const user = userEvent.setup();
  const suggestion = (id: string, replaceWith: string): Comment => ({
    anchor: {
      artifact_id: specArtifact.id,
      block_id: "block-1",
      mark_id: `${id}-mark`,
      orphaned: false,
      quote: "brown",
      version: 1,
    },
    ask_id: null,
    turn: null,
    author: { id: "bob", kind: "user" },
    body: "Suggested replacement.",
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: { accepted: null, replace_with: replaceWith },
    ...commentDeliveryFields(),
  });
  const first = suggestion("suggestion-1", "red");
  const second = suggestion("suggestion-2", "auburn");
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: true,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  const actions: Array<[string, string]> = [];
  const selections: string[] = [];
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <KeymapProvider>
        <MarginSheet
          model={{
            actions: {
              closeComposer: () => {},
              discardKeptDraft: () => {},
              onAction: (id, action) => {
                actions.push([id, action]);
              },
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
              commentsError: false,
              commentsPending: false,
              decisions: [],
              historicalAsks: [],
              keptDrafts: [],
              isClosed: false,
              issueError: false,
              owner: {
                artifactId: "artifact-1",
                kind: "document",
                project: "CORE",
                slug: "design-notes",
              },
              issuePending: false,
              marginRef: { current: null },
              needsYou: [],
              onSelectCard: (key) => {
                selections.push(key);
              },
              openAskCount: 0,
              pendingActionIds: new Set<string>(),
              pinned: [],
              pinnedConversations: [],
              pinnedIds: [],
              resolvedThreads: [],
              foldedAskCount: 0,
              threads: [first, second].map((root) => ({
                anchor: root.anchor,
                key: root.id,
                lastReplyAt: undefined,
                replies: [],
                resolved: false,
                root: { comment: root, kind: "comment" },
              })),
              viewerLogin: "alice",
              visibleArtifact: {
                ...specArtifact,
                issue_key: null,
                primary: false,
                slug: "design-notes",
              },
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
              expanded: true,
              replySending: false,
              thread: undefined,
              toggle: () => {},
            },
            filter: { blockId: "block-1", clear: () => {} },
            tab: { set: () => {}, value: "comments" },
          }}
        />
      </KeymapProvider>
    </QueryClientProvider>
  );

  try {
    expect(screen.queryByRole("dialog", { name: "Thread" })).not.toBeInTheDocument();
    const firstCard = screen.getByTestId(`margin-comment-${first.id}`);
    const secondCard = screen.getByTestId(`margin-comment-${second.id}`);
    expect(firstCard.querySelector("ins")?.textContent).toBe("red");
    expect(secondCard.querySelector("ins")?.textContent).toBe("auburn");
    await user.click(within(secondCard).getByRole("button", { name: "Accept suggestion" }));
    expect(actions).toEqual([[second.id, "accept"]]);
    expect(selections).toEqual([]);
    expect(within(firstCard).getByRole("button", { name: "Reject suggestion" })).not.toBeNull();
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
  }
});

/** The issue page around the margin, as far as its keys go: its scope, and the selection bar's
 *  `q` and `d` in it (`IssuePage`). */
function IssuePageKeys({ children }: { children: ReactNode }): ReactNode {
  useKeymapScope("issue");
  useSelectionBarKeys("issue");
  return children;
}

// Below `xl` the open review panel is a dialog, and a dialog masks the page's keys (`useDialog`,
// `keymap.ts`'s `resolve`): the selection bar over words in the panel shows Quote reply `q` and,
// over an ask's words, Define `d`, so each key does there what the bar's button does.
test("in the review panel below xl, q and d act on the words selected in it, as the selection bar shows them", async () => {
  const user = userEvent.setup();
  const root: Comment = {
    anchor: null,
    ask_id: null,
    turn: null,
    author: { id: "bob", kind: "user" },
    body: "Review this paragraph.",
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id: "comment-sheet",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    ...commentDeliveryFields(),
  };
  const ask = issueAsk({ id: "ask-sheet", question: "Which release path?" });
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = ((query: string) =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: query === COMPACT_VIEWPORT_QUERY,
      media: query,
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  const getAsk = mockAskReads([ask]);
  const defineWords = spyOn(api, "defineWords").mockImplementation(() => new Promise(() => {}));
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <KeymapProvider>
          <IssuePageKeys>
            <MarginSheet
              model={{
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
                  commentsError: false,
                  commentsPending: false,
                  decisions: [],
                  historicalAsks: [],
                  keptDrafts: [],
                  isClosed: false,
                  issueError: false,
                  owner: { key: "CORE-1", kind: "issue" },
                  issuePending: false,
                  marginRef: { current: null },
                  needsYou: [ask],
                  onSelectCard: () => {},
                  openAskCount: 1,
                  pendingActionIds: new Set<string>(),
                  pinned: [],
                  pinnedConversations: [],
                  pinnedIds: [],
                  resolvedThreads: [],
                  foldedAskCount: 0,
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
                  visibleArtifact: specArtifact,
                },
                placement: { blockPlacements: new Map(), markPlacements: new Map() },
                selection: {
                  expandedThreadKey: root.id,
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
                  expanded: true,
                  replySending: false,
                  thread: undefined,
                  toggle: () => {},
                },
                filter: { blockId: undefined, clear: () => {} },
                tab: { set: () => {}, value: "comments" },
              }}
            />
          </IssuePageKeys>
        </KeymapProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const panel = await screen.findByRole("dialog", { name: "Review panel" });
    const card = within(panel).getByTestId(`margin-comment-${root.id}`);
    await waitFor(() => {
      if (card.querySelector(".dispatch-markdown")?.textContent !== root.body) {
        throw new Error("the comment's words have not rendered");
      }
    });
    // The press that selects the words takes the focus out of the reply box the open thread
    // focused: here, to the panel's own control.
    const panelToggle = within(panel).getByRole("button", { name: /^Close review panel/ });
    act(() => panelToggle.focus());
    selectWords(card, ".dispatch-markdown");
    await user.keyboard("q");
    // The open thread's reply box takes the quote (`ThreadCard`).
    const reply = within(panel).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
    await waitFor(() => expect(reply.value).toBe(`> ${root.body}\n\n`));

    const askCard = panel.querySelector<HTMLElement>(`[data-ask-card="${ask.id}"]`);
    if (askCard === null) throw new Error("the panel draws no card for the ask");
    await waitFor(() => {
      const question = askCard.querySelector("[data-ask-question] .dispatch-markdown");
      if (question?.textContent !== ask.question) {
        throw new Error("the question's words have not rendered");
      }
    });
    stateCardBox(askCard);
    act(() => panelToggle.focus());
    selectWords(askCard, "[data-ask-question] .dispatch-markdown");
    await user.keyboard("d");
    await waitFor(() =>
      expect(defineWords).toHaveBeenCalledWith(
        { ask_id: ask.id, words: [ask.question] },
        expect.anything()
      )
    );
  } finally {
    window.getSelection()?.removeAllRanges();
    view.unmount();
    window.matchMedia = originalMatchMedia;
    defineWords.mockRestore();
    getAsk.mockRestore();
    whoAmI.mockRestore();
  }
});
