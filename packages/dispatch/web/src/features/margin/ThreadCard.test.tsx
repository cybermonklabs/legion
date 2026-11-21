import { afterEach, beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type RenderResult, render, screen, waitFor, within } from "@testing-library/react";
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

/** A card its host opens and closes as the margin's list does: its toggle flips `expanded`. */
function ToggledCard(props: Omit<EditableCardProps, "expanded" | "onToggle">) {
  const [expanded, setExpanded] = useState(true);
  return (
    <EditableCard {...props} expanded={expanded} onToggle={() => setExpanded((open) => !open)} />
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
