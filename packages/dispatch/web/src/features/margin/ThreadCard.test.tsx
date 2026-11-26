import { afterEach, beforeAll, expect, jest, spyOn, test } from "bun:test";
import { hashKey, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ComponentProps, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { graphNode } from "../../__tests__/graph-fixture";
import { dwellOn, installIntersectionObserver } from "../../__tests__/intersection-observer";
import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { expectSameElements } from "../../__tests__/same-elements";
import { answerThreadReads } from "../../__tests__/send-answers";
import { heldDraftWords, stopReplySend } from "../../__tests__/stop-late-write";
import { ApiError, api } from "../../api/client";
import type { Comment } from "../../api/types";
import { heldSends } from "../conversation/held-sends";
import { ConversationTurns } from "../conversation/turn-keys";
import { unsentDraftsOf } from "../conversation/unsent-drafts";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { userPreferenceStorageKey } from "../shell/userPreference";
import { seedUnread } from "../threads/replies-fixture";
import { SendStoppedError } from "../writes/keyed-write";
import { marginReplySendKey } from "./margin-context";
import { ThreadCard } from "./ThreadCard";
import type { Thread } from "./useMarginItems";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

function comment(
  id: string,
  body: string,
  createdAt: string,
  overrides: Partial<Comment> = {}
): Comment {
  return {
    anchor: {
      artifact_id: "artifact-1",
      block_id: null,
      mark_id: "mark-1",
      orphaned: false,
      quote: "selected text",
      version: 1,
    },
    ask_id: null,
    turn: null,
    author: { id: "alice", kind: "user" },
    body,
    created_at: createdAt,
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    ...commentDeliveryFields(),
    ...overrides,
  };
}

const root = comment("root-1", "Root comment", "2026-09-10T00:00:00Z");
const reply = comment("reply-1", "First reply", "2026-09-10T00:01:00Z", {
  anchor: null,
  reply_to: root.id,
});

function thread(overrides: Partial<Thread> = {}): Thread {
  return {
    anchor: root.anchor,
    key: root.id,
    lastReplyAt: reply.created_at,
    replies: [reply],
    resolved: false,
    root: { comment: root, kind: "comment" },
    ...overrides,
  };
}

function listThread(): Thread {
  const listRoot = comment("root-1", "- first\n- second", "2026-09-10T00:00:00Z");
  return thread({ replies: [], root: { comment: listRoot, kind: "comment" } });
}

type EditableCardProps = Omit<
  ComponentProps<typeof ThreadCard>,
  "editingCommentId" | "onEditingChange" | "savingCommentEditId"
> &
  Partial<
    Pick<
      ComponentProps<typeof ThreadCard>,
      "editingCommentId" | "onEditingChange" | "savingCommentEditId"
    >
  >;

/** Owns the edit-in-progress id the way the margin does, so these tests drive the card alone. */
function EditableCard(props: EditableCardProps) {
  const [editingCommentId, setEditingCommentId] = useState<string>();
  return (
    <ThreadCard
      {...props}
      savingCommentEditId={props.savingCommentEditId}
      editingCommentId={props.editingCommentId ?? editingCommentId}
      onEditingChange={props.onEditingChange ?? setEditingCommentId}
    />
  );
}

function renderCard(
  currentThread = thread(),
  overrides: Partial<ComponentProps<typeof ThreadCard>> = {},
  queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  })
) {
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <EditableCard
          actionFailure={undefined}
          artifactSlug="spec"
          expanded
          hovered={false}
          isClosed={false}
          onAction={() => {}}
          onEdit={async () => undefined}
          onRetryAction={() => {}}
          onToggle={() => {}}
          owner={{ key: "CORE-1", kind: "issue" }}
          pendingAction={false}
          thread={currentThread}
          viewerLogin="alice"
          {...overrides}
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("an expanded comment thread renders flat replies and posts an inline reply to its root", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockResolvedValue({
    ...reply,
    body: "Second reply",
    id: "reply-2",
  });

  let view: RenderResult | undefined;
  try {
    view = renderCard();
    const card = screen.getByTestId(`margin-comment-${root.id}`);
    expect(card).toHaveAttribute("aria-expanded", "true");
    expect(card.querySelector(`[data-margin-item="${reply.id}"]`)).toBeNull();
    expect((await screen.findByText("First reply")).closest("li")?.style.marginLeft).toBe("0px");

    const composer = screen.getByRole("form", { name: "Comment composer" });
    await user.click(screen.getByLabelText("Reply"));
    await user.paste("Second reply");
    await user.keyboard("{Control>}{Enter}{/Control}");

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "Second reply",
          reply_to: root.id,
        },
        keyedAttempt
      )
    );
    expect(composer.querySelector("blockquote")).toBeNull();
  } finally {
    view?.unmount();
    createComment.mockRestore();
  }
});

test("a margin reply that names a passage quotes it as its author's, or says it was removed", async () => {
  const bob = { id: "bob", kind: "user" } as const;
  const passage = {
    block_id: "b2",
    node: "comment/reply-1#b2",
    ref: "dispatch://CORE-1/comment/reply-1#b-b2",
    removed: false,
    text: "Beta.",
  };
  const nested = comment("reply-1", "Alpha.\n\nBeta.", "2026-09-10T00:01:00Z", {
    anchor: null,
    author: bob,
    reply_to: root.id,
  });
  // A reply's `reply_to` is the thread's root, while the passage it names is another reply's.
  const toNested = comment("reply-2", "Yes to that.", "2026-09-10T00:02:00Z", {
    anchor: null,
    reply_block: passage,
    reply_to: root.id,
  });
  const toRemoved = comment("reply-3", "And that?", "2026-09-10T00:03:00Z", {
    anchor: null,
    author: bob,
    reply_block: { ...passage, block_id: "b1", node: `comment/${root.id}#b1`, removed: true },
    reply_to: root.id,
  });
  const view = renderCard(thread({ replies: [nested, toNested, toRemoved] }));

  try {
    const row = async (body: string) => {
      const element = (await screen.findByText(body)).closest("li");
      if (element === null) throw new Error(`${body} has no reply row`);
      return element;
    };
    const yes = await row("Yes to that.");
    await waitFor(() => expect(yes).toHaveTextContent("Replying to bob's passage — Beta."));
    const removed = await row("And that?");
    expect(removed).toHaveTextContent("Replying to alice's passage — This passage was removed.");
    // A reply that names no passage carries no quote: the thread is its parent.
    expect((await row("Beta.")).textContent).not.toContain("Replying to");
  } finally {
    view.unmount();
  }
});

test("a collapsed preview flattens a list-only body onto one line with no nested <ul>", async () => {
  const view = renderCard(listThread(), { expanded: false });

  try {
    const preview = await within(view.container).findByText("first second");
    expect(preview.querySelector("ul")).toBeNull();
  } finally {
    view.unmount();
  }
});
test("a resolved comment thread names its resolver and exposes Reopen", async () => {
  const user = userEvent.setup();
  const onAction = spyOn({ call: () => {} }, "call");
  const resolvedRoot = comment("root-1", "Root comment", "2026-09-10T00:00:00Z", {
    resolved: true,
    resolved_at: "2026-09-10T00:02:00Z",
    resolved_by: { id: "bob", kind: "user" },
  });

  const view = renderCard(
    thread({ resolved: true, root: { comment: resolvedRoot, kind: "comment" } }),
    {
      onAction: onAction,
    }
  );
  try {
    expect(screen.getByText(/Resolved by bob/)).not.toBeNull();
    await user.click(screen.getByRole("button", { name: "Reopen" }));
    expect(onAction).toHaveBeenCalledWith("root-1", "reopen");
  } finally {
    view.unmount();
    onAction.mockRestore();
  }
});

// An agent's comment and an agent's resolution name it through `AgentName`: a link to its live
// conversation, with the service attribution as text after it. Following that link is not a
// click on the card, so it does not select the thread.
test("an agent author and resolver link to their live conversations", async () => {
  const user = userEvent.setup();
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  const onSelect = jest.fn();
  const agent = {
    id: "legion-worker-session",
    kind: "session" as const,
    origin: { session_title: "Implementer" },
    service: "system:serviceaccount:legion:legion-worker",
  };
  const agentRoot = comment("root-1", "Root comment", "2026-09-10T00:00:00Z", {
    author: agent,
    resolved: true,
    resolved_at: "2026-09-10T00:02:00Z",
    resolved_by: agent,
  });
  const view = renderCard(
    thread({ replies: [], resolved: true, root: { comment: agentRoot, kind: "comment" } }),
    { onSelect }
  );
  try {
    const card = screen.getByTestId(`margin-comment-${root.id}`);
    expect(card).toHaveTextContent("Implementer (as legion/legion-worker) ·");
    expect(card).toHaveTextContent("Resolved by Implementer (as legion/legion-worker) ·");
    // The author, the resolver, and the thread composer's `Replying to <root author>`.
    expect(card).toHaveTextContent("Replying to Implementer (as legion/legion-worker)");
    const names = within(card).getAllByRole("link", { name: "Implementer" });
    expect(names.map((name) => name.getAttribute("href"))).toEqual([
      "/agents/legion-worker-session/live",
      "/agents/legion-worker-session/live",
      "/agents/legion-worker-session/live",
    ]);
    await user.click(names[0] as HTMLElement);
    expect(onSelect).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    listAgents.mockRestore();
  }
});

test("the author can edit a comment and an edited comment carries its marker", async () => {
  const user = userEvent.setup();
  // The margin's edit reads the comment's blocks; this comment is not indexed, so the save
  // carries no block ids.
  const commentBlocks = spyOn(api, "commentBlocks").mockResolvedValue({
    blocks: [],
    indexed: false,
    item: graphNode({ id: root.id, issue_key: "CORE-1", kind: "comment" }),
  });
  const onEdit = spyOn({ call: async () => undefined }, "call");
  const view = renderCard(undefined, { onEdit: onEdit });

  try {
    const rootEdit = within(screen.getByTestId(`margin-comment-${root.id}`)).getAllByRole(
      "button",
      { name: "Edit" }
    )[0];
    if (rootEdit === undefined) {
      throw new Error("The root comment has no Edit button.");
    }
    await user.click(rootEdit);
    expect(screen.getByRole("button", { name: "Save" })).not.toBeNull();
    const field = screen.getByLabelText("Edit comment");
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Updated root");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() =>
      expect(onEdit).toHaveBeenCalledWith(root.id, "Updated root", undefined, keyedAttempt)
    );

    view.rerender(
      <QueryClientProvider client={new QueryClient()}>
        <EditableCard
          actionFailure={undefined}
          artifactSlug="spec"
          expanded
          hovered={false}
          isClosed={false}
          onAction={() => {}}
          onEdit={async () => undefined}
          onRetryAction={() => {}}
          onToggle={() => {}}
          owner={{ key: "CORE-1", kind: "issue" }}
          pendingAction={false}
          thread={thread({
            root: {
              comment: { ...root, body: "Updated root", edited_at: "2026-09-10T00:02:00Z" },
              kind: "comment",
            },
          })}
          viewerLogin="alice"
        />
      </QueryClientProvider>
    );
    expect(await screen.findByText("Updated root")).not.toBeNull();
    expect(screen.getByTestId(`margin-comment-${root.id}`)).toHaveTextContent("edited");
  } finally {
    view.unmount();
    onEdit.mockRestore();
    commentBlocks.mockRestore();
  }
});

test("a saving comment edit hides every Edit control across the card", () => {
  const idle = renderCard(undefined, { savingCommentEditId: undefined });
  try {
    expect(screen.getAllByRole("button", { name: "Edit" })).toHaveLength(2);
  } finally {
    idle.unmount();
  }

  const saving = renderCard(undefined, { savingCommentEditId: root.id });
  try {
    expect(screen.queryAllByRole("button", { name: "Edit" })).toHaveLength(0);
  } finally {
    saving.unmount();
  }
});

test("a different author never sees an Edit control", () => {
  const view = renderCard(undefined, { viewerLogin: "bob" });
  try {
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("a comment body with inline code renders it as <code>", async () => {
  const codeRoot = comment("root-1", "Run `npm install` first.", "2026-09-10T00:00:00Z");
  const view = renderCard(thread({ root: { comment: codeRoot, kind: "comment" } }));
  try {
    expect(await screen.findByText("npm install", { selector: "code" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("Escape with an empty inline reply collapses the thread", async () => {
  const user = userEvent.setup();
  let collapsed = false;
  const view = renderCard(undefined, {
    onToggle: () => {
      collapsed = true;
    },
  });
  try {
    await user.click(screen.getByLabelText("Reply"));
    await user.keyboard("{Escape}");
    expect(collapsed).toBe(true);
  } finally {
    view.unmount();
  }
});

test("a reply box a key opened stands, taking no focus, and its Escape goes where the host says, the thread staying open", async () => {
  const user = userEvent.setup();
  let collapsed = false;
  let cancelled = false;
  const view = renderCard(undefined, {
    onReplyCancel: () => {
      cancelled = true;
    },
    onToggle: () => {
      collapsed = true;
    },
    replyStanding: true,
  });
  try {
    const box = await screen.findByLabelText("Reply");
    expect(box).not.toHaveFocus();
    await user.click(box);
    await user.keyboard("{Escape}");
    expect(cancelled).toBe(true);
    expect(collapsed).toBe(false);
  } finally {
    view.unmount();
  }
});

test("every reply draws its own Copy reference, and the root's is drawn only where the host draws none", () => {
  const name = (id: string) => `Copy reference dispatch://CORE-1/comment/${id}`;
  const own = renderCard();
  try {
    expect(screen.getAllByRole("button", { name: name(root.id) })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: name(reply.id) })).toHaveLength(1);
  } finally {
    own.unmount();
  }
  const hosted = renderCard(undefined, { showRootReference: false });
  try {
    expect(screen.queryByRole("button", { name: name(root.id) })).toBeNull();
    expect(screen.getAllByRole("button", { name: name(reply.id) })).toHaveLength(1);
  } finally {
    hosted.unmount();
  }
});

test("a collapsed card draws its root's Copy reference beside its preview, outside the button that opens it", () => {
  const view = renderCard(undefined, { expanded: false });
  try {
    const copy = screen.getByRole("button", {
      name: `Copy reference dispatch://CORE-1/comment/${root.id}`,
    });
    const preview = view.container.querySelector('button[aria-expanded="false"]');
    expect(preview).not.toBeNull();
    expect(preview?.contains(copy)).toBe(false);
  } finally {
    view.unmount();
  }
});

/** A collapsed card, as its host draws it, whose root holds a web link, once its preview has
 *  drawn the link; `onToggle` hears each toggle the host makes. */
async function foldedLinkedCard() {
  const onToggle = jest.fn();
  const linked = comment(
    "root-1",
    "Read [a page elsewhere](https://elsewhere.test/page) first.",
    "2026-09-10T00:00:00Z"
  );
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={new QueryClient()}>
        <ToggledCard
          actionFailure={undefined}
          artifactSlug="spec"
          folded
          hovered={false}
          isClosed={false}
          onAction={() => {}}
          onEdit={async () => undefined}
          onRetryAction={() => {}}
          onToggle={onToggle}
          owner={{ key: "CORE-1", kind: "issue" }}
          pendingAction={false}
          thread={thread({ replies: [], root: { comment: linked, kind: "comment" } })}
          viewerLogin="alice"
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const card = screen.getByTestId(`margin-comment-${linked.id}`);
  const preview = card.querySelector<HTMLElement>('button[aria-expanded="false"]');
  if (preview === null) throw new Error("The collapsed card draws no preview button.");
  const link = await waitFor(() => {
    const drawn = preview.querySelector("a");
    if (drawn === null) throw new Error("The preview has not drawn its link yet.");
    return drawn;
  });
  expect(link.textContent).toBe("a page elsewhere");
  // The test document follows no link; the press itself is what reaches the card.
  link.addEventListener("click", (event) => event.preventDefault());
  return { card, link, onToggle, preview, view };
}

test("a click and a Ctrl+click on a link in a collapsed card's preview leave the card folded, and a click on its words opens it", async () => {
  // The preview is Markdown (`MarkdownPreview`), empty until the lazy Markdown engine loads.
  await warmMarkdownRenderer();
  const user = userEvent.setup();
  // What each press left, on a card of its own, in one comparison, so a row that fails says
  // which press opened the card.
  const left: Record<string, unknown> = {};
  for (const press of ["click", "Ctrl+click"] as const) {
    const { card, link, onToggle, view } = await foldedLinkedCard();
    try {
      if (press === "Ctrl+click") await user.keyboard("{Control>}");
      await user.click(link);
      if (press === "Ctrl+click") await user.keyboard("{/Control}");
      left[press] = {
        expanded: card.getAttribute("aria-expanded"),
        linkInPage: link.isConnected,
        replyBox: within(card).queryByRole("textbox", { name: "Reply" }) !== null,
        toggles: onToggle.mock.calls.length,
      };
    } finally {
      view.unmount();
    }
  }
  const folded = { expanded: "false", linkInPage: true, replyBox: false, toggles: 0 };
  expect(left).toEqual({ "Ctrl+click": folded, click: folded });

  const { card, onToggle, preview, view } = await foldedLinkedCard();
  try {
    await user.click(within(preview).getByText("first.", { exact: false }));
    expect(onToggle).toHaveBeenCalledTimes(1);
    expect(card).toHaveAttribute("aria-expanded", "true");
  } finally {
    view.unmount();
  }
});

test("on the Conversation tab each reply is a turn its keys reach, and elsewhere no reply takes the focus", async () => {
  const inTab = render(
    <ConversationTurns value>
      <MemoryRouter>
        <QueryClientProvider client={new QueryClient()}>
          <EditableCard
            actionFailure={undefined}
            artifactSlug="spec"
            expanded
            hovered={false}
            isClosed={false}
            onAction={() => {}}
            onEdit={async () => undefined}
            onRetryAction={() => {}}
            onToggle={() => {}}
            owner={{ key: "CORE-1", kind: "issue" }}
            pendingAction={false}
            thread={thread()}
            viewerLogin="alice"
          />
        </QueryClientProvider>
      </MemoryRouter>
    </ConversationTurns>
  );
  try {
    const row = (await screen.findByText("First reply")).closest("[data-reply]");
    expect(row).toHaveAttribute("tabindex", "-1");
    expect(row).toHaveAttribute("data-turn-ref", `dispatch://CORE-1/comment/${reply.id}`);
  } finally {
    inTab.unmount();
  }
  const margin = renderCard();
  try {
    const row = (await screen.findByText("First reply")).closest("[data-reply]");
    expect(row).not.toHaveAttribute("tabindex");
    expect(row).not.toHaveAttribute("data-turn-ref");
  } finally {
    margin.unmount();
  }
});

/** A card its host opens and closes as the margin's list does: its toggle flips `expanded`, which
 *  starts open unless `folded`, and `onToggle` hears each toggle. */
function ToggledCard({
  folded = false,
  onToggle,
  ...props
}: Omit<EditableCardProps, "expanded" | "onToggle"> & {
  folded?: boolean;
  onToggle?: () => void;
}) {
  const [expanded, setExpanded] = useState(!folded);
  return (
    <EditableCard
      {...props}
      expanded={expanded}
      onToggle={() => {
        onToggle?.();
        setExpanded((open) => !open);
      }}
    />
  );
}

/** The thread's reply send name, as the margin names it. */
const replyKey = marginReplySendKey(root.id);

/** What a refusal Dispatch gave says in the reply composer. */
const refusedText = "Couldn't send — the server is down";

/** What a send the reader stopped says in its composer at its own address: Dispatch has not
 *  confirmed it, and its Retry asks again under its own key. */
const stoppedText = new SendStoppedError().message;

/** The thread's reply, as a stopped send answers it (`stopReplySend`). */
const rootReply = { actor: root.author, excerpt: "", id: root.id, parentKind: "comment" } as const;

// Cancel reply is the reader's own move, and a margin thread has no other composer to take the
// reply to: the reply is kept under its own send name, the card says so until the reader goes on
// with it, and the card's Reply puts it back in the field - its text, a refusal's note, and a
// stopped send's note that it may have been delivered. Nothing is sent until the reader presses:
// a refusal's Retry then goes under a key of its own, and a stopped send's under the key it went
// out under, at the address it went to.
for (const kept of ["typed", "refused", "stopped"] as const) {
  test(`Cancel reply keeps a margin reply ${heldDraftWords[kept]}, the card says so, and its Reply brings it back`, async () => {
    const user = userEvent.setup();
    const queryClient = new QueryClient({
      defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
    });
    const createComment = spyOn(api, "createComment");
    const body = `Reply ${heldDraftWords[kept]}, then Cancel reply`;
    if (kept === "stopped") {
      await stopReplySend(queryClient, createComment, replyKey, body, rootReply);
    }
    createComment.mockResolvedValue({ ...reply, body, id: "reply-2" });
    if (kept === "refused") {
      createComment.mockRejectedValueOnce(
        new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" })
      );
    }
    const view = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <ToggledCard
            actionFailure={undefined}
            artifactSlug="spec"
            hovered={false}
            isClosed={false}
            onAction={() => {}}
            onEdit={async () => undefined}
            onRetryAction={() => {}}
            owner={{ key: "CORE-1", kind: "issue" }}
            pendingAction={false}
            replyMutationKey={replyKey}
            thread={thread()}
            viewerLogin="alice"
          />
        </QueryClientProvider>
      </MemoryRouter>
    );
    const card = screen.getByTestId(`margin-comment-${root.id}`);

    try {
      if (kept === "stopped") {
        expect(within(card).getByText(stoppedText)).toBeTruthy();
      } else {
        await user.click(within(card).getByRole("textbox", { name: "Reply" }));
        await user.paste(body);
        if (kept === "refused") {
          await user.click(within(card).getByRole("button", { name: "Send" }));
          await within(card).findByText(refusedText);
        }
      }
      const sent = createComment.mock.calls.length;

      await user.click(within(card).getByRole("button", { name: "Cancel reply" }));
      expect(card).toHaveAttribute("aria-expanded", "false");
      expect(within(card).queryByRole("textbox", { name: "Reply" })).not.toBeInTheDocument();
      expect(within(card).getByRole("status").textContent).toBe(
        "Your reply to the comment by alice is kept. Press Reply to go on with it."
      );
      expect(createComment).toHaveBeenCalledTimes(sent);

      await user.click(within(card).getByRole("button", { name: "Reply" }));
      expect(card).toHaveAttribute("aria-expanded", "true");
      // The text and the note in one comparison, so a row that fails says which of them is gone.
      expect({
        refusal: within(card).queryByText(refusedText) !== null,
        stopped: within(card).queryByText(stoppedText) !== null,
        text: within(card).getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" }).value,
      }).toEqual({ refusal: kept === "refused", stopped: kept === "stopped", text: body });
      expect(within(card).queryByText(/is kept/)).not.toBeInTheDocument();
      expect(createComment).toHaveBeenCalledTimes(sent);

      if (kept === "typed") return;
      await user.click(within(card).getByRole("button", { name: "Retry" }));
      await waitFor(() => expect(createComment).toHaveBeenCalledTimes(sent + 1));
      const keys = createComment.mock.calls.map(([, , bounds]) => bounds.key);
      if (kept === "refused") expect(keys[1]).not.toBe(keys[0]);
      if (kept === "stopped") expect(keys.at(-1)).toBe("stopped-send");
    } finally {
      view.unmount();
      createComment.mockRestore();
    }
  });
}

/** An open margin card its host folds and opens as the margin's list does (`ToggledCard`), under
 *  the thread's reply send name. */
function renderMarginCard() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ToggledCard
          actionFailure={undefined}
          artifactSlug="spec"
          hovered={false}
          isClosed={false}
          onAction={() => {}}
          onEdit={async () => undefined}
          onRetryAction={() => {}}
          owner={{ key: "CORE-1", kind: "issue" }}
          pendingAction={false}
          replyMutationKey={replyKey}
          thread={thread()}
          viewerLogin="alice"
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

// A key or a button in a margin card's reply box that ends the reply folds the card (`onToggle`),
// which takes the box away with the focus it holds: the preview drawn again in its place, which
// opened the thread, takes the focus back, never the page's body (`foldFromReply`;
// dispatch://INFRA-2160, E1). Escape and Cancel reply end the reply the box holds.
for (const ends of ["Escape", "Cancel reply"] as const) {
  test(`${ends} in a margin card's reply box folds the card with the focus on its preview`, async () => {
    const user = userEvent.setup();
    const view = renderMarginCard();
    const card = screen.getByTestId(`margin-comment-${root.id}`);
    try {
      await user.click(within(card).getByRole("textbox", { name: "Reply" }));
      if (ends === "Escape") {
        await user.keyboard("{Escape}");
      } else {
        await user.click(within(card).getByRole("button", { name: "Cancel reply" }));
      }
      expect(card).toHaveAttribute("aria-expanded", "false");
      expect(within(card).getByRole("button", { expanded: false })).toHaveFocus();
    } finally {
      view.unmount();
    }
  });
}

// Discard draft under a reply Dispatch refused drops the reply and ends it, as Cancel reply does:
// the card folds with the focus on its preview (`foldFromReply`).
test("Discard draft over a margin reply Dispatch refused folds the card with the focus on its preview", async () => {
  const user = userEvent.setup();
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" })
  );
  const view = renderMarginCard();
  const card = screen.getByTestId(`margin-comment-${root.id}`);
  try {
    await user.click(within(card).getByRole("textbox", { name: "Reply" }));
    await user.paste("A reply Dispatch refuses");
    await user.click(within(card).getByRole("button", { name: "Send" }));
    await within(card).findByText(refusedText);
    await user.click(within(card).getByRole("button", { name: "Discard draft" }));
    expect(card).toHaveAttribute("aria-expanded", "false");
    expect(within(card).getByRole("button", { expanded: false })).toHaveFocus();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// Two editors open in one card: the root's, held open by a save Dispatch refused, and a reply's.
// A second Escape over Discard draft? in the reply's editor ends that edit alone, and the reply's
// Edit takes the focus back while the root's editor stays open (`useFocusBack`, which reads the
// ended editor alone).
test("a second Escape over Discard draft? in one of a card's two open editors gives the focus back to that comment's Edit", async () => {
  const user = userEvent.setup();
  // These comments are not indexed, so a save carries no block ids.
  const commentBlocks = spyOn(api, "commentBlocks").mockImplementation(async (id) => ({
    blocks: [],
    indexed: false,
    item: graphNode({ id, issue_key: "CORE-1", kind: "comment" }),
  }));
  const onEdit = jest.fn(async () => {
    throw new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" });
  });
  const view = renderCard(undefined, { onEdit });
  const card = screen.getByTestId(`margin-comment-${root.id}`);
  try {
    const [rootEdit] = within(card).getAllByRole("button", { name: "Edit" });
    if (rootEdit === undefined) throw new Error("The root comment has no Edit button.");
    await user.click(rootEdit);
    await user.click(within(card).getByRole("textbox", { name: "Edit comment" }));
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Root comment, refused");
    await user.click(within(card).getByRole("button", { name: "Save" }));
    await within(card).findByText(refusedText);

    const replyRow = card.querySelector<HTMLElement>("[data-reply]");
    if (replyRow === null) throw new Error("The card draws no reply.");
    await user.click(within(replyRow).getByRole("button", { name: "Edit" }));
    await user.click(within(replyRow).getByRole("textbox", { name: "Edit comment" }));
    await user.keyboard(" And a second thought.");
    await user.keyboard("{Escape}");
    expect(within(replyRow).getByRole("alert")).toHaveTextContent("Discard draft?");
    await user.keyboard("{Escape}");
    expect(within(replyRow).queryByRole("textbox", { name: "Edit comment" })).toBeNull();
    expect(within(card).getAllByRole("textbox", { name: "Edit comment" })).toHaveLength(1);
    expect(within(replyRow).getByRole("button", { name: "Edit" })).toHaveFocus();
  } finally {
    view.unmount();
    commentBlocks.mockRestore();
  }
});

// A second Escape over Discard draft? asks the edit to end, but a save Dispatch refused holds the
// editor open. The reader presses Retry, then puts the focus in the thread's Reply box while the
// save is out. When it lands, the editor closes; the Edit takes nothing, since the focus was not
// lost with the editor (`useFocusBack`, `focusOnDocument`), and stays in the Reply box.
test("an edit held open by a refused save that ends after the reader moved the focus leaves the focus where they put it", async () => {
  const user = userEvent.setup();
  // This comment is not indexed, so a save carries no block ids.
  const commentBlocks = spyOn(api, "commentBlocks").mockResolvedValue({
    blocks: [],
    indexed: false,
    item: graphNode({ id: root.id, issue_key: "CORE-1", kind: "comment" }),
  });
  const retried = Promise.withResolvers<Comment>();
  const onEdit = jest
    .fn(async (): Promise<Comment> => retried.promise)
    .mockRejectedValueOnce(new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" }));
  const view = renderCard(undefined, { onEdit });
  const card = screen.getByTestId(`margin-comment-${root.id}`);
  try {
    const [rootEdit] = within(card).getAllByRole("button", { name: "Edit" });
    if (rootEdit === undefined) throw new Error("The root comment has no Edit button.");
    await user.click(rootEdit);
    await user.click(within(card).getByRole("textbox", { name: "Edit comment" }));
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Root comment, saved at last");
    await user.click(within(card).getByRole("button", { name: "Save" }));
    await within(card).findByText(refusedText);
    await user.keyboard("{Escape}");
    expect(within(card).getByText("Discard draft?")).toBeTruthy();
    await user.keyboard("{Escape}");
    // The refused save holds the editor open.
    expect(within(card).getByRole("textbox", { name: "Edit comment" })).toBeTruthy();

    await user.click(within(card).getByRole("button", { name: "Retry" }));
    const replyBox = within(card).getByRole("textbox", { name: "Reply" });
    await user.click(replyBox);
    await act(async () => {
      retried.resolve({ ...root, body: "Root comment, saved at last" });
      await retried.promise;
    });
    await waitFor(() =>
      expect(within(card).queryByRole("textbox", { name: "Edit comment" })).toBeNull()
    );
    expect(replyBox).toHaveFocus();
  } finally {
    retried.resolve(root);
    view.unmount();
    commentBlocks.mockRestore();
  }
});

test("a terminal suggestion shows its disposition and has no Reopen control", () => {
  const accepted = comment("root-1", "Suggested replacement.", "2026-09-10T00:00:00Z", {
    resolved: true,
    resolved_at: "2026-09-10T00:02:00Z",
    resolved_by: { id: "bob", kind: "user" },
    suggestion: { accepted: true, replace_with: "Replacement" },
  });
  const view = renderCard(
    thread({
      resolved: true,
      root: { comment: accepted, kind: "comment" },
    })
  );
  try {
    expect(screen.queryByRole("button", { name: "Reopen" })).not.toBeInTheDocument();
    expect(screen.getByText(/Accepted by bob/)).not.toBeNull();
  } finally {
    view.unmount();
  }
});

function suggestionThread(overrides: Partial<Comment> = {}): Thread {
  const suggested = comment("root-1", "Suggested replacement.", "2026-09-10T00:00:00Z", {
    suggestion: { accepted: null, replace_with: "Replacement" },
    ...overrides,
  });
  return thread({ replies: [], root: { comment: suggested, kind: "comment" } });
}

test("a collapsed suggestion card shows its diff and Accept/Reject, and Accept acts without selecting", async () => {
  const user = userEvent.setup();
  const onAction = spyOn({ call: () => {} }, "call");
  const onSelect = spyOn({ call: () => {} }, "call");
  const view = renderCard(suggestionThread(), { expanded: false, onAction, onSelect });

  try {
    const card = screen.getByTestId("margin-comment-root-1");
    expect(within(card).getByText("Suggestion")).not.toBeNull();
    expect(card.querySelector("del")?.textContent).toBe("selected text");
    expect(card.querySelector("ins")?.textContent).toBe("Replacement");
    await user.click(within(card).getByRole("button", { name: "Accept suggestion" }));
    expect(onAction).toHaveBeenCalledWith("root-1", "accept");
    await user.click(within(card).getByRole("button", { name: "Reject suggestion" }));
    expect(onAction).toHaveBeenCalledWith("root-1", "reject");
    expect(onSelect).not.toHaveBeenCalled();
    expect(card).toHaveAttribute("aria-expanded", "false");
  } finally {
    view.unmount();
    onAction.mockRestore();
    onSelect.mockRestore();
  }
});

test("an expanded suggestion card offers Accept and Reject exactly once", () => {
  const view = renderCard(suggestionThread());

  try {
    expect(screen.getAllByRole("button", { name: "Accept suggestion" })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "Reject suggestion" })).toHaveLength(1);
    expect(screen.queryByRole("button", { name: "Resolve" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("a collapsed comment card keeps its text preview and offers no suggestion controls", async () => {
  const view = renderCard(thread(), { expanded: false });

  try {
    const card = screen.getByTestId("margin-comment-root-1");
    expect(await within(card).findByText("Root comment")).not.toBeNull();
    expect(card.querySelector("del, ins")).toBeNull();
    expect(within(card).queryByText("Suggestion")).not.toBeInTheDocument();
    expect(
      within(card).queryByRole("button", { name: "Accept suggestion" })
    ).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("an orphaned suggestion shows its diff and Text changed but offers neither Accept nor Reject", () => {
  const orphaned = suggestionThread({
    anchor: {
      artifact_id: "artifact-1",
      block_id: "block-1",
      mark_id: "mark-1",
      orphaned: true,
      quote: "selected text",
      version: 1,
    },
  });

  for (const expanded of [false, true]) {
    const view = renderCard(orphaned, { expanded });
    try {
      const card = screen.getByTestId("margin-comment-root-1");
      expect(within(card).getByText("Suggestion")).not.toBeNull();
      expect(within(card).getByText(/Text changed\./)).not.toBeNull();
      expect(card.querySelector("ins")?.textContent).toBe("Replacement");
      expect(
        within(card).queryByRole("button", { name: "Accept suggestion" })
      ).not.toBeInTheDocument();
      expect(
        within(card).queryByRole("button", { name: "Reject suggestion" })
      ).not.toBeInTheDocument();
      if (expanded) {
        expect(within(card).getByRole("button", { name: "Resolve" })).not.toBeNull();
      }
    } finally {
      view.unmount();
    }
  }
});

test("a document-owned comment thread posts its inline reply to the artifact route", async () => {
  const user = userEvent.setup();
  const createArtifactComment = spyOn(api, "createArtifactComment").mockResolvedValue(
    undefined as never
  );
  const documentRoot = comment("document-root", "Document-owned comment", "2026-09-10T00:00:00Z", {
    issue_key: null,
  });
  const view = renderCard(
    thread({
      key: documentRoot.id,
      root: { comment: documentRoot, kind: "comment" },
    }),
    {
      owner: {
        artifactId: "artifact-1",
        kind: "document",
        project: "CORE",
        slug: "design-notes",
      },
    }
  );
  try {
    const reply = screen.getByLabelText("Reply");
    await user.click(reply);
    await user.paste("Document reply");
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() =>
      expect(createArtifactComment).toHaveBeenCalledWith(
        "artifact-1",
        {
          body: "Document reply",
          reply_to: documentRoot.id,
        },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    createArtifactComment.mockRestore();
  }
});

test("a collapsed card's delivery controls sit outside the toggle button and don't trigger it", async () => {
  const user = userEvent.setup();
  let toggled = false;
  let retried = false;
  const view = renderCard(thread(), {
    expanded: false,
    onToggle: () => {
      toggled = true;
    },
    renderDeliveries: (comment) =>
      comment.id === root.id ? (
        <button
          onClick={() => {
            retried = true;
          }}
          type="button"
        >
          Retry
        </button>
      ) : null,
  });

  try {
    const retryButton = screen.getByRole("button", { name: "Retry" });
    expect(retryButton.parentElement?.closest("button") ?? null).toBeNull();
    await user.click(retryButton);
    expect(retried).toBe(true);
    expect(toggled).toBe(false);
  } finally {
    view.unmount();
  }
});

// A failed action shows the reason the card was given; Retry is offered only when retrying can
// succeed.
test("a failed action shows its reason, with Retry only when a retry can succeed", () => {
  for (const retryable of [false, true]) {
    const view = renderCard(thread(), {
      actionFailure: {
        id: "comment-1",
        message: 'ask block "a1" holds a second bullet list',
        retryable,
      },
    });
    try {
      expect(screen.getByRole("alert")).toHaveTextContent(
        'ask block "a1" holds a second bullet list'
      );
      expect(screen.queryByRole("button", { name: "Retry" }) !== null).toBe(retryable);
    } finally {
      view.unmount();
    }
  }
});

// LEGION-540. A suggestion's note is Markdown its author wrote; the collapsed card previews it
// formatted on one line beside the diff, never as `**` and `-`.
test("a collapsed suggestion card previews its note formatted on one line", async () => {
  const view = renderCard(
    suggestionThread({ body: "**Tighter** wording:\n\n- drops `very`\n- keeps the claim" }),
    { expanded: false }
  );
  try {
    const card = screen.getByTestId("margin-comment-root-1");
    await waitFor(() => expect(card.querySelector("strong")?.textContent).toBe("Tighter"));
    const preview = card.querySelector<HTMLElement>("[data-markdown-preview]");
    if (preview === null) throw new Error("the suggestion's note is not a preview");
    expect(preview.querySelector("code")?.textContent).toBe("very");
    expect(preview.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "Tighter wording: drops very keeps the claim"
    );
    expect(preview.querySelector("ul, li, p")).toBeNull();
  } finally {
    view.unmount();
  }
});

afterEach(() => {
  jest.useRealTimers();
});

/** Renders the card on `CORE-1`, whose root-1 thread is unread for alice, with time and the
 *  screen in the test's hands. */
async function renderUnreadCard(expanded: boolean) {
  // A reply is Markdown (`MarkdownBody`), empty until the lazy Markdown engine loads in real time.
  await warmMarkdownRenderer();
  jest.useFakeTimers();
  const screen = installIntersectionObserver();
  const mark = answerThreadReads().mockResolvedValue(undefined);
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  seedUnread(queryClient, { issue: "CORE-1" }, [root.id]);
  const view = renderCard(thread(), { expanded }, queryClient);
  return {
    cleanup: () => {
      view.unmount();
      mark.mockRestore();
      screen.restore();
    },
    mark,
    screen,
    view,
  };
}

test("an expanded thread is marked read once its newest reply has been on screen", async () => {
  const card = await renderUnreadCard(true);
  try {
    const newest = (await card.view.findByText("First reply")).closest("li");
    if (newest === null) throw new Error("expected the newest reply's item");
    // The watched element is the newest reply's own, not the card around the whole thread.
    expectSameElements(card.screen.observed(), [newest]);
    await dwellOn(card.screen, newest);
    expect(card.mark.mock.calls).toEqual([
      [root.id, { read_through: reply.created_at }, keyedAttempt],
    ]);
  } finally {
    card.cleanup();
  }
});

test("a collapsed thread shows no reply and marks nothing", async () => {
  const card = await renderUnreadCard(false);
  try {
    expect(card.screen.observed().length).toBe(0);
    await dwellOn(card.screen, screen.getByTestId(`margin-comment-${root.id}`));
    expect(card.mark).not.toHaveBeenCalled();
  } finally {
    card.cleanup();
  }
});

// A margin thread folds nothing and offers no order control of its own: it follows the one
// choice the reader made in the Inbox's View menu, replies and reply box alike.
for (const order of ["oldest", "newest"] as const) {
  test(`a margin thread reads ${order} first, with its reply box beside the newest message`, async () => {
    const key = userPreferenceStorageKey("alice", "inbox.thread-order");
    window.localStorage.setItem(key, order);
    const second = comment("reply-2", "Second reply", "2026-09-10T00:02:00Z", {
      anchor: null,
      reply_to: root.id,
    });
    const queryClient = new QueryClient({
      defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
    });
    seedUnread(queryClient, { issue: "CORE-1" }, []);
    const view = renderCard(thread({ replies: [reply, second] }), {}, queryClient);
    try {
      const replies = await screen.findByRole("list", { name: "Replies" });
      await waitFor(() =>
        expect(Array.from(replies.querySelectorAll("li"), (item) => item.textContent)).toEqual(
          order === "oldest"
            ? [expect.stringContaining("First reply"), expect.stringContaining("Second reply")]
            : [expect.stringContaining("Second reply"), expect.stringContaining("First reply")]
        )
      );
      const composer = screen.getByRole("form", { name: "Comment composer" });
      const composerFirst =
        (replies.compareDocumentPosition(composer) & Node.DOCUMENT_POSITION_PRECEDING) !== 0;
      expect(composerFirst).toBe(order === "newest");
    } finally {
      view.unmount();
      window.localStorage.removeItem(key);
    }
  });
}

// INFRA-869: the margin keeps a card per reference for a body of several, now that the one
// predicate decides which bodies show cards.
test("a comment of several references shows a card for each of them", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockImplementation(async (key) => ({
    documents: [],
    key,
    project: "CORE",
    status: "in_progress",
    title: key === "CORE-2" ? "The other issue" : "The third issue",
  }));
  const references = comment(
    "root-1",
    "dispatch://CORE-2 dispatch://CORE-3",
    "2026-09-10T00:00:00Z"
  );
  const view = renderCard(thread({ replies: [], root: { comment: references, kind: "comment" } }));

  try {
    const card = screen.getByTestId(`margin-comment-${references.id}`);
    const cards = await within(card).findByRole("region", { name: "References" });
    const links = await waitFor(() => {
      const found = within(cards).getAllByRole("link");
      if (found.length < 2) throw new Error("the second card has not rendered");
      return found;
    });
    expect(links.map((link) => link.getAttribute("href"))).toEqual([
      "/issues/CORE-2",
      "/issues/CORE-3",
    ]);
    await within(cards).findByText("The other issue");
    await within(cards).findByText("The third issue");
  } finally {
    getIssueSummary.mockRestore();
    view.unmount();
  }
});

test("a deleted comment reads as deleted where its body was, and its reply keeps its own text", async () => {
  // The deleted root keeps its body, "Root comment", beside the marks, so only the card's own
  // reading of the marks keeps it off the page. The card draws a body after its first paint, so
  // the body's absence is read once the reply's body is on the page.
  const deletedRoot = comment("root-1", root.body, "2026-09-10T00:00:00Z", {
    deleted_at: "2026-09-10T00:05:00Z",
    deleted_by: { id: "alice", kind: "user" },
  });
  const view = renderCard(thread({ root: { comment: deletedRoot, kind: "comment" } }));

  try {
    const card = screen.getByTestId(`margin-comment-${root.id}`);
    expect(card).toHaveTextContent("Deleted by alice");
    // The thread keeps its shape: the reply below the deleted root still reads as its author
    // wrote it.
    expect(await screen.findByText("First reply")).not.toBeNull();
    expect(card).not.toHaveTextContent("Root comment");
  } finally {
    view.unmount();
  }
});

// A deleted post takes no new text: the server refuses an edit of one and a reply to one (409),
// so the card offers neither where the post is gone, and both where it stands.
test("a deleted comment offers no Edit and no reply, and its live reply keeps Edit", () => {
  const deleted = {
    deleted_at: "2026-09-10T00:05:00Z",
    deleted_by: { id: "alice", kind: "user" } as const,
  };

  const deletedRoot = renderCard(
    thread({ root: { comment: comment("root-1", "", root.created_at, deleted), kind: "comment" } })
  );
  try {
    // Only the reply below it, which alice wrote and nobody deleted, is hers to edit.
    expect(screen.getAllByRole("button", { name: "Edit" })).toHaveLength(1);
    expect(screen.queryByRole("form", { name: "Comment composer" })).toBeNull();
  } finally {
    deletedRoot.unmount();
  }

  const deletedReply = renderCard(
    thread({
      replies: [
        comment("reply-1", "", reply.created_at, { ...deleted, anchor: null, reply_to: root.id }),
      ],
    })
  );
  try {
    expect(screen.getAllByRole("button", { name: "Edit" })).toHaveLength(1);
    // The root stands, so the thread still takes a reply.
    expect(screen.getByRole("form", { name: "Comment composer" })).toBeTruthy();
  } finally {
    deletedReply.unmount();
  }
});

/** `root` as its author left it once they deleted it: the body cleared, the marks set. */
const deletedRoot = comment("root-1", "", root.created_at, {
  deleted_at: "2026-09-10T00:05:00Z",
  deleted_by: { id: "alice", kind: "user" },
});

/** The words every role="status" region of `card` holds, in order. */
function statusLines(card: HTMLElement): string[] {
  return within(card)
    .queryAllByRole("status")
    .map((status) => status.textContent ?? "");
}

// A root its author deleted takes no reply (the server refuses one, 409), so the card draws no
// reply composer under it, and the Reply its kept line names would open nothing. A reply the
// reader kept for the thread - typed, refused or stopped - is drawn itself instead, closed and
// open, saying the comment was deleted and giving no word of it, with no Reply, and Discard my
// draft drops it where it is kept. Nothing is sent.
for (const kept of ["typed", "refused", "stopped"] as const) {
  test(`a margin reply ${heldDraftWords[kept]} under a root deleted since is drawn itself, with no Reply to go on with it`, async () => {
    const user = userEvent.setup();
    const queryClient = new QueryClient({
      defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
    });
    const createComment = spyOn(api, "createComment");
    const body = `Reply ${heldDraftWords[kept]}, then the root's delete`;
    if (kept === "stopped") {
      await stopReplySend(queryClient, createComment, replyKey, body, rootReply);
    }
    createComment.mockResolvedValue({ ...reply, body, id: "reply-2" });
    if (kept === "refused") {
      createComment.mockRejectedValueOnce(
        new ApiError(409, { code: "UNAVAILABLE", error: "the server is down" })
      );
    }
    const tree = (current: Thread) => (
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <ToggledCard
            actionFailure={undefined}
            artifactSlug="spec"
            hovered={false}
            isClosed={false}
            onAction={() => {}}
            onEdit={async () => undefined}
            onRetryAction={() => {}}
            owner={{ key: "CORE-1", kind: "issue" }}
            pendingAction={false}
            replyMutationKey={replyKey}
            thread={current}
            viewerLogin="alice"
          />
        </QueryClientProvider>
      </MemoryRouter>
    );
    const view = render(tree(thread()));
    const card = () => screen.getByTestId(`margin-comment-${root.id}`);
    const gone =
      "The comment by alice that your reply answered was deleted, so it takes no reply. " +
      `Your reply is kept here:${body}` +
      (kept === "stopped"
        ? "Dispatch has not confirmed this reply: it may have been posted, and if it was, it is listed in this thread."
        : "");

    try {
      if (kept !== "stopped") {
        await user.click(within(card()).getByRole("textbox", { name: "Reply" }));
        await user.paste(body);
        if (kept === "refused") {
          await user.click(within(card()).getByRole("button", { name: "Send" }));
          await within(card()).findByText(refusedText);
        }
      }
      await user.click(within(card()).getByRole("button", { name: "Cancel reply" }));
      const sent = createComment.mock.calls.length;

      view.rerender(tree(thread({ root: { comment: deletedRoot, kind: "comment" } })));
      const closed = {
        lines: statusLines(card()),
        reply: within(card()).queryByRole("button", { name: "Reply" }) !== null,
      };
      await user.click(within(card()).getByRole("button", { expanded: false }));
      const open = {
        composer: within(card()).queryByRole("form", { name: "Comment composer" }) !== null,
        expanded: card().getAttribute("aria-expanded"),
        lines: statusLines(card()),
      };
      // Pressed where it is drawn, so a card that draws none fails in the comparison below, which
      // says what the card drew instead.
      const discard = within(card()).queryByRole("button", { name: "Discard my draft" });
      if (discard !== null) await user.click(discard);

      expect({
        closed,
        discarded: {
          held: heldSends(queryClient).get(replyKey) !== undefined,
          lines: statusLines(card()),
          typed: unsentDraftsOf(queryClient).read(hashKey(replyKey)) !== undefined,
        },
        open,
        sent: createComment.mock.calls.length - sent,
      }).toEqual({
        closed: { lines: [gone], reply: false },
        discarded: { held: false, lines: [], typed: false },
        open: { composer: false, expanded: "true", lines: [gone] },
        sent: 0,
      });
      expect(card()).not.toHaveTextContent("Root comment");
    } finally {
      view.unmount();
      createComment.mockRestore();
    }
  });
}

/** What holds the focus, by the name `named` gives it, else the page's body, else its markup. */
function focusedAs(named: ReadonlyMap<string, HTMLElement>): string {
  const focused = document.activeElement;
  return (
    [...named].find(([, control]) => control === focused)?.[0] ??
    (focused === document.body ? "the page's body" : (focused?.outerHTML ?? "nothing"))
  );
}

// Discard my draft takes the kept reply's offer away, and with it the button that held the focus.
// The focus goes on, with no scroll, to the control now at the offer's place in the card, else to
// the nearest one before it (`handFocusFrom`): on the folded card nothing follows the offer, so the
// root's Copy reference beside the preview takes it.
test("Discard my draft on a margin reply kept under a deleted root hands the focus to the root's Copy reference, with no scroll", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const tree = (current: Thread) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <ToggledCard
          actionFailure={undefined}
          artifactSlug="spec"
          hovered={false}
          isClosed={false}
          onAction={() => {}}
          onEdit={async () => undefined}
          onRetryAction={() => {}}
          owner={{ key: "CORE-1", kind: "issue" }}
          pendingAction={false}
          replyMutationKey={replyKey}
          thread={current}
          viewerLogin="alice"
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(thread()));
  const card = () => screen.getByTestId(`margin-comment-${root.id}`);

  try {
    await user.click(within(card()).getByRole("textbox", { name: "Reply" }));
    await user.paste("A reply the root's delete leaves kept");
    await user.click(within(card()).getByRole("button", { name: "Cancel reply" }));
    view.rerender(tree(thread({ root: { comment: deletedRoot, kind: "comment" } })));
    const target = within(card()).getByRole("button", { name: /^Copy reference / });
    const focuses = spyOn(target, "focus");
    try {
      await user.click(within(card()).getByRole("button", { name: "Discard my draft" }));
      expect({
        focused: focusedAs(new Map([["the root's Copy reference", target]])),
        focuses: focuses.mock.calls,
        offered: within(card()).queryByRole("button", { name: "Discard my draft" }) !== null,
      }).toEqual({
        focused: "the root's Copy reference",
        focuses: [[{ preventScroll: true }]],
        offered: false,
      });
    } finally {
      focuses.mockRestore();
    }
  } finally {
    view.unmount();
  }
});

/** The line a kept edit over a comment deleted since is announced by, which says it is the
 *  reader's own. */
const KEPT_EDIT_LINE =
  "This comment was deleted, and Dispatch saves no edit of a deleted comment. Your unsaved edit is kept in this browser.";

// Dispatch saves no edit of a deleted comment (409 POST_DELETED). An editor open over a comment
// its author then deletes - holding words of the reader's own, or a save Dispatch refused - is
// not left open on an empty body: the comment reads as deleted where its body was, and the edit
// waits as the reader's own until Discard my edit drops it where it is kept. By the architect's
// ruling of 2026-11-20 07:52Z the edit is announced and not drawn: an edit is mostly the words of
// the comment it was written over, so the card says under the deleted line that the unsaved edit
// is kept in this browser, offers Copy my edit and Discard my edit and nothing else - no editor,
// no Save - and draws no word of the edit, and so none of the comment the edit was written over.
for (const kept of ["typed", "refused"] as const) {
  test(`an edit ${heldDraftWords[kept]} over a comment deleted since closes, and waits under the deleted line until discarded`, async () => {
    const user = userEvent.setup();
    const queryClient = new QueryClient({
      defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
    });
    // This comment is not indexed, so the save carries no block ids.
    const commentBlocks = spyOn(api, "commentBlocks").mockResolvedValue({
      blocks: [],
      indexed: false,
      item: graphNode({ id: root.id, issue_key: "CORE-1", kind: "comment" }),
    });
    const onEdit = jest.fn(async () => {
      throw new ApiError(409, { code: "POST_DELETED", error: "the comment was deleted" });
    });
    const tree = (current: Thread) => (
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <EditableCard
            actionFailure={undefined}
            artifactSlug="spec"
            expanded
            hovered={false}
            isClosed={false}
            onAction={() => {}}
            onEdit={onEdit}
            onRetryAction={() => {}}
            onToggle={() => {}}
            owner={{ key: "CORE-1", kind: "issue" }}
            pendingAction={false}
            thread={current}
            viewerLogin="alice"
          />
        </QueryClientProvider>
      </MemoryRouter>
    );
    const view = render(tree(thread()));
    const card = () => screen.getByTestId(`margin-comment-${root.id}`);
    const editKey = ["comment-edit", root.id];
    const edit = `An edit ${heldDraftWords[kept]}`;

    try {
      const [rootEdit] = within(card()).getAllByRole("button", { name: "Edit" });
      if (rootEdit === undefined) throw new Error("The root comment has no Edit button.");
      await user.click(rootEdit);
      const field = screen.getByLabelText("Edit comment");
      // The body the editor opens on, which the edit is written over: the root's own words.
      const started = field instanceof HTMLTextAreaElement ? field.value : undefined;
      await user.click(field);
      await user.keyboard("{Control>}a{/Control}");
      await user.paste(edit);
      if (kept === "refused") {
        await user.click(screen.getByRole("button", { name: "Save" }));
        await waitFor(() => expect(heldSends(queryClient).get(editKey)?.status).toBe("refused"));
      }
      // The refused edit's one save is the reader's own press, before the delete.
      const saves = onEdit.mock.calls.length;

      view.rerender(tree(thread({ root: { comment: deletedRoot, kind: "comment" } })));
      await within(card()).findByRole("button", { name: "Discard my edit" });
      // The card's words outside its replies, its controls and its times: the words of the
      // document the comment is anchored to, the deleted line, the root's byline and the label,
      // and nothing of the edit or of the comment the edit was written over.
      const words = card().cloneNode(true);
      if (words instanceof Element) {
        for (const part of words.querySelectorAll('ol[aria-label="Replies"], button, time')) {
          part.remove();
        }
      }
      const offer = within(card()).getByRole("status").parentElement;
      const offered = {
        actions: [...(offer?.querySelectorAll("button") ?? [])].map((press) => press.textContent),
        body: card().textContent?.includes(root.body),
        deleted: card().textContent?.includes("Deleted by alice"),
        edit: card().textContent?.includes(edit),
        editor: within(card()).queryByLabelText("Edit comment") !== null,
        field: screen.queryByLabelText("Edit comment") !== null,
        lines: statusLines(card()),
        save: within(card()).queryByRole("button", { name: "Save" }) !== null,
        words: words.textContent,
      };
      await user.click(within(card()).getByRole("button", { name: "Discard my edit" }));

      expect({
        discarded: {
          held: heldSends(queryClient).get(editKey) !== undefined,
          kept: unsentDraftsOf(queryClient).readAway(hashKey(editKey), null) !== undefined,
          lines: statusLines(card()),
        },
        offered,
        saves: { after: onEdit.mock.calls.length - saves, before: saves },
        started,
      }).toEqual({
        discarded: { held: false, kept: false, lines: [] },
        offered: {
          actions: ["Copy my edit", "Discard my edit"],
          body: false,
          deleted: true,
          edit: false,
          editor: false,
          field: false,
          lines: [KEPT_EDIT_LINE],
          save: false,
          words: `${root.anchor?.quote}Deleted by alice · alice · ${KEPT_EDIT_LINE}`,
        },
        saves: { after: 0, before: kept === "refused" ? 1 : 0 },
        started: root.body,
      });
      expect(card()).not.toHaveTextContent("Root comment");
    } finally {
      view.unmount();
      commentBlocks.mockRestore();
    }
  });
}

/** A card whose root the reader was editing when its author deleted it, with `draft` typed over
 *  the root's whole body and never saved: the edit waits under the deleted line. */
async function editKeptOverDeletedRoot(draft: string) {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const commentBlocks = spyOn(api, "commentBlocks").mockResolvedValue({
    blocks: [],
    indexed: false,
    item: graphNode({ id: root.id, issue_key: "CORE-1", kind: "comment" }),
  });
  const tree = (current: Thread) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <EditableCard
          actionFailure={undefined}
          artifactSlug="spec"
          expanded
          hovered={false}
          isClosed={false}
          onAction={() => {}}
          onEdit={async () => {}}
          onRetryAction={() => {}}
          onToggle={() => {}}
          owner={{ key: "CORE-1", kind: "issue" }}
          pendingAction={false}
          thread={current}
          viewerLogin="alice"
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(thread()));
  const card = () => screen.getByTestId(`margin-comment-${root.id}`);
  const [rootEdit] = within(card()).getAllByRole("button", { name: "Edit" });
  if (rootEdit === undefined) throw new Error("The root comment has no Edit button.");
  await user.click(rootEdit);
  await user.click(screen.getByLabelText("Edit comment"));
  await user.keyboard("{Control>}a{/Control}");
  await user.paste(draft);
  view.rerender(tree(thread({ root: { comment: deletedRoot, kind: "comment" } })));
  await within(card()).findByRole("button", { name: "Copy my edit" });
  return {
    card,
    done: () => {
      view.unmount();
      commentBlocks.mockRestore();
    },
    kept: () =>
      unsentDraftsOf(queryClient).readAway(hashKey(["comment-edit", root.id]), null) !== undefined,
    user,
  };
}

// The kept edit is drawn nowhere, so Copy my edit is how the reader takes it with them: the whole
// edit, every line of it, goes to the clipboard, and still none of it is drawn.
test("Copy my edit puts the whole kept edit on the clipboard, and draws none of it", async () => {
  const draft = "Root comment, said again.\n\nAnd a second paragraph of mine.";
  const kept = await editKeptOverDeletedRoot(draft);
  const originalClipboard = navigator.clipboard;
  const writeText = spyOn({ writeText: async (_text: string) => undefined }, "writeText");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  try {
    await kept.user.click(within(kept.card()).getByRole("button", { name: "Copy my edit" }));
    await waitFor(() => expect(writeText.mock.calls).toEqual([[draft]]));
    await waitFor(() =>
      expect(statusLines(kept.card())).toEqual([`${KEPT_EDIT_LINE}Your edit is copied.`])
    );
    expect(kept.card()).not.toHaveTextContent("said again");
    expect(kept.kept()).toBe(true);
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    kept.done();
  }
});

// A browser can refuse the clipboard (its permission denied, and the older copy too): the card
// says the edit was not copied, and the edit stays kept, with both its presses.
test("a Copy my edit the browser refuses says so in words and keeps the edit", async () => {
  const kept = await editKeptOverDeletedRoot("Root comment, said again.");
  const originalClipboard = navigator.clipboard;
  const originalExecCommand = document.execCommand;
  const writeText = spyOn(
    {
      writeText: async (_text: string): Promise<void> => {
        throw new DOMException("Write permission denied.", "NotAllowedError");
      },
    },
    "writeText"
  );
  const execCommand = spyOn({ execCommand: (_command: string) => false }, "execCommand");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  Object.defineProperty(document, "execCommand", { configurable: true, value: execCommand });
  try {
    await kept.user.click(within(kept.card()).getByRole("button", { name: "Copy my edit" }));
    await waitFor(() =>
      expect(statusLines(kept.card())).toEqual([
        `${KEPT_EDIT_LINE}Your edit was not copied: this browser refused the clipboard. It is still kept here.`,
      ])
    );
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(kept.card()).not.toHaveTextContent("said again");
    expect(kept.kept()).toBe(true);
    expect(
      within(kept.card())
        .getAllByRole("button")
        .map((press) => press.textContent)
        .filter((label) => label?.includes("my edit"))
    ).toEqual(["Copy my edit", "Discard my edit"]);
  } finally {
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    kept.done();
  }
});

// Discard my edit takes the kept edit's offer away, and with it the button that held the focus.
// The focus goes on, with no scroll, to the control now at the offer's place in the card, else to
// the nearest one before it (`handFocusFrom`): the root's Resolve, which follows the offer.
test("Discard my edit on an edit kept over a deleted comment hands the focus to the control at its place, with no scroll", async () => {
  const kept = await editKeptOverDeletedRoot("Root comment, said again.");
  const target = within(kept.card()).getByRole("button", { name: "Resolve" });
  const focuses = spyOn(target, "focus");
  try {
    await kept.user.click(within(kept.card()).getByRole("button", { name: "Discard my edit" }));
    expect({
      focused: focusedAs(new Map([["Resolve", target]])),
      focuses: focuses.mock.calls,
      kept: kept.kept(),
    }).toEqual({ focused: "Resolve", focuses: [[{ preventScroll: true }]], kept: false });
  } finally {
    focuses.mockRestore();
    kept.done();
  }
});

// The card's reply box quotes the thread's root, so the root's edit is marked in that quote as on
// the root's own byline, as the Inbox's box quoting the same thread marks it: a quote of an
// edited root carries ` · edited`, a quote of a root nobody edited does not.
test("a margin thread's reply quote marks an edited root edited, and a standing root carries no mark", () => {
  const editedRoot = comment("root-1", "Root comment", root.created_at, {
    edited_at: "2026-09-10T00:04:00Z",
  });
  const edited = renderCard(thread({ root: { comment: editedRoot, kind: "comment" } }));
  try {
    const composer = screen.getByRole("form", { name: "Comment composer" });
    expect(composer).toHaveTextContent("Replying to alice · edited");
  } finally {
    edited.unmount();
  }

  const standing = renderCard(thread());
  try {
    const composer = screen.getByRole("form", { name: "Comment composer" });
    expect(composer).toHaveTextContent("Replying to alice");
    expect(composer).not.toHaveTextContent("edited");
  } finally {
    standing.unmount();
  }
});

// A collapsed card folds its byline away, so the root's ` · edited` sits beside the preview: an
// edited root carries it, a root nobody edited does not, and a deleted root reads as deleted.
test("a collapsed card marks an edited root beside its preview, and a standing root carries no mark", async () => {
  const editedRoot = comment("root-1", "Root comment", "2026-09-10T00:00:00Z", {
    edited_at: "2026-09-10T00:04:00Z",
  });
  const edited = renderCard(thread({ root: { comment: editedRoot, kind: "comment" } }), {
    expanded: false,
  });
  try {
    const card = screen.getByTestId("margin-comment-root-1");
    await within(card).findByText("Root comment");
    expect(card).toHaveTextContent("Root comment · edited");
  } finally {
    edited.unmount();
  }

  const standing = renderCard(thread(), { expanded: false });
  try {
    const card = screen.getByTestId("margin-comment-root-1");
    await within(card).findByText("Root comment");
    expect(card).not.toHaveTextContent("edited");
  } finally {
    standing.unmount();
  }
});

// The quote of a passage prints the post it is a passage of, so that post's edit is marked in
// the quote as on the post's own byline.
test("a margin reply's quote of a passage of an edited post marks the post as edited", async () => {
  const bob = { id: "bob", kind: "user" } as const;
  const passage = {
    block_id: "b2",
    node: "comment/reply-1#b2",
    ref: "dispatch://CORE-1/comment/reply-1#b-b2",
    removed: false,
    text: "Beta.",
  };
  const editedReply = comment("reply-1", "Alpha.\n\nBeta.", "2026-09-10T00:01:00Z", {
    anchor: null,
    author: bob,
    edited_at: "2026-09-10T00:01:30Z",
    reply_to: root.id,
  });
  const toEdited = comment("reply-2", "Yes to that.", "2026-09-10T00:02:00Z", {
    anchor: null,
    reply_block: passage,
    reply_to: root.id,
  });
  const toStanding = comment("reply-3", "And that?", "2026-09-10T00:03:00Z", {
    anchor: null,
    author: bob,
    reply_block: { ...passage, block_id: "b1", node: `comment/${root.id}#b1`, text: "Root." },
    reply_to: root.id,
  });
  const view = renderCard(thread({ replies: [editedReply, toEdited, toStanding] }));

  try {
    const row = async (body: string) => {
      const element = (await screen.findByText(body)).closest("li");
      if (element === null) throw new Error(`${body} has no reply row`);
      return element;
    };
    const yes = await row("Yes to that.");
    await waitFor(() =>
      expect(yes).toHaveTextContent("Replying to bob's passage · edited — Beta.")
    );
    const other = await row("And that?");
    await waitFor(() => expect(other).toHaveTextContent("Replying to alice's passage — Root."));
    expect(other).not.toHaveTextContent("edited");
  } finally {
    view.unmount();
  }
});

// R3 of dispatch://INFRA-1523 and the architect's ruling of 2026-11-20 05:13Z: an edit kept under a
// comment its author deleted holds no word of the comment the edit was written over. An edit that
// adds to the comment's own words, rather than replacing them, keeps those words in its draft, and
// the card drew them under the deleted line as "Your edit is kept here: Root comment Also Friday."
// (test round at 869d340e). The comment's words are not drawn anywhere in the card; Discard my edit
// stays its one action.
test("an edit that added to a comment deleted since draws none of the comment's words under the deleted line", async () => {
  const user = userEvent.setup();
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const commentBlocks = spyOn(api, "commentBlocks").mockResolvedValue({
    blocks: [],
    indexed: false,
    item: graphNode({ id: root.id, issue_key: "CORE-1", kind: "comment" }),
  });
  const tree = (current: Thread) => (
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <EditableCard
          actionFailure={undefined}
          artifactSlug="spec"
          expanded
          hovered={false}
          isClosed={false}
          onAction={() => {}}
          onEdit={async () => {}}
          onRetryAction={() => {}}
          onToggle={() => {}}
          owner={{ key: "CORE-1", kind: "issue" }}
          pendingAction={false}
          thread={current}
          viewerLogin="alice"
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree(thread()));
  const card = () => screen.getByTestId(`margin-comment-${root.id}`);
  try {
    const [rootEdit] = within(card()).getAllByRole("button", { name: "Edit" });
    if (rootEdit === undefined) throw new Error("The root comment has no Edit button.");
    await user.click(rootEdit);
    const field = screen.getByLabelText("Edit comment");
    // The positive control: the editor opens on the comment's own words, which the edit keeps.
    expect(field instanceof HTMLTextAreaElement ? field.value : undefined).toBe(root.body);
    await user.click(field);
    await user.keyboard("{Control>}{End}{/Control}");
    await user.paste(" Also Friday.");

    view.rerender(tree(thread({ root: { comment: deletedRoot, kind: "comment" } })));
    await within(card()).findByRole("button", { name: "Discard my edit" });
    expect(card()).toHaveTextContent("Deleted by alice");
    expect(card()).not.toHaveTextContent(root.body);
  } finally {
    view.unmount();
    commentBlocks.mockRestore();
  }
});
