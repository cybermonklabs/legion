import { beforeAll, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";
import { createPortal } from "react-dom";
import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import type { Artifact, Comment } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { KeymapProvider } from "../shell/KeymapProvider";
import { MarginSheet } from "./MarginSheet";
import { useReviewSheetSlots } from "./review-sheet-slots";

/** What the document draws into a phone's sheet (`ArtifactHeader`, `ProofDocument`), as words
 *  the row can find. */
function SheetSlotProbe(): ReactNode {
  const slots = useReviewSheetSlots();
  if (slots === null) return null;
  return (
    <>
      {createPortal("probe controls", slots.controls)}
      {createPortal("probe decisions", slots.decisions)}
      {createPortal("probe version", slots.version)}
      {createPortal("probe count", slots.count)}
    </>
  );
}

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
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
        // Open, as the other rows here render it: below `xl` the margin is a bottom sheet, and
        // a shut one draws neither tab nor card (`MarginSheet`'s `covered`).
        sheet: {
          closeThread: () => {},
          expanded: true,
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
    expect(screen.getByRole("button", { name: "Close review panel (3 open asks)" })).not.toBeNull();
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
        <SheetSlotProbe />
      </KeymapProvider>
    </QueryClientProvider>
  );

  try {
    const thread = screen.getByRole("dialog", { name: "Thread" });
    expect(thread.className).toContain("inset-0");
    expect(within(thread).getByTestId(`margin-comment-${root.id}`)).not.toBeNull();
    // A phone's sheet takes in the document's chrome (`review-sheet-slots.ts`): what the page
    // draws into its boxes stands inside the sheet, the version and the count in its bar.
    const sheetBox = screen.getByTestId("margin-sheet");
    expect(within(sheetBox).getByText("probe controls")).not.toBeNull();
    expect(within(sheetBox).getByText("probe decisions")).not.toBeNull();
    const bar = sheetBox.querySelector("button[aria-expanded]");
    expect(bar?.textContent).toContain("probe version");
    expect(bar?.textContent).toContain("probe count");
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

/** An issue's phone sheet whose toggle opens and shuts it, as `useMarginSheet`'s does. */
function PhoneIssueSheet({ children }: { children: ReactNode }): ReactNode {
  const [expanded, setExpanded] = useState(true);
  return (
    <>
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
            openAskCount: 1,
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
            toggle: (open?: boolean) => setExpanded((current) => open ?? !current),
          },
          filter: { blockId: undefined, clear: () => {} },
          tab: { set: () => {}, value: "comments" },
        }}
      />
      {children}
    </>
  );
}

// A control in a phone's sheet that takes the reader into the document - the open-decisions
// bar's link - shuts the sheet first, and its move runs once the sheet is shut and has given the
// focus back (`leave`, `review-sheet-slots.ts`): a focus the move gives the reader is theirs, not
// taken back to the sheet's toggle by the dialog closing after it.
test("a move a phone sheet's control asks for runs once the sheet is shut, and keeps the focus it gives", async () => {
  const user = userEvent.setup();
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
  const seen: string[] = [];
  function LeavingControl(): ReactNode {
    const slots = useReviewSheetSlots();
    if (slots === null) return null;
    return createPortal(
      <button
        onClick={() =>
          slots.leave(() => {
            seen.push(screen.getByTestId("margin-sheet").getAttribute("data-expanded") ?? "");
            screen.getByTestId("the-decision").focus();
          })
        }
        type="button"
      >
        Go to the decision
      </button>,
      slots.decisions
    );
  }
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <KeymapProvider>
        <PhoneIssueSheet>
          <LeavingControl />
          <button data-testid="the-decision" type="button">
            The decision
          </button>
        </PhoneIssueSheet>
      </KeymapProvider>
    </QueryClientProvider>
  );
  try {
    await user.click(await screen.findByRole("button", { name: "Go to the decision" }));
    expect(screen.getByTestId("margin-sheet")).toHaveAttribute("data-expanded", "false");
    expect(seen).toEqual(["false"]);
    // A frame later the focus is still where the move put it.
    const frame = Promise.withResolvers<void>();
    requestAnimationFrame(() => frame.resolve());
    await frame.promise;
    expect(document.activeElement).toBe(screen.getByTestId("the-decision"));
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
  }
});
