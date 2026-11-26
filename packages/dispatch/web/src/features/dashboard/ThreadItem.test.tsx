import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type {
  AskRead,
  AskWithTrees,
  CommentRead,
  CommentWithTrees,
  MessageRead,
  MessageWithTrees,
  MyReplyRow,
} from "../../api/types";
import type { SentDraft } from "../conversation/send-request";
import { unsentDraftsOf } from "../conversation/unsent-drafts";
import { composerNameOf } from "../conversation/useKeptDrafts";
import { NoTooltips } from "../issue-page/NoTooltips";
import { DASHBOARD_WORDS } from "../issue-page/page-words";
import { AppProviders } from "../issue-page/slots/stage-fixtures";
import { AgentRegistryPoller, agentsQuery } from "../refs/agent-registry";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import asksOpenBody from "./fixtures/asks-open.json";
import changesBody from "./fixtures/changes.json";
import { replySendKey, ThreadItem } from "./ThreadItem";

// A thread item of what waits (dispatch://INFRA-5702/spec#b-r-in-place): it reads its thread's
// own route, draws the first entry and each write the reader has not read, the reply control the
// Inbox draws for that kind of thread, and a link to the thread at its own address.

beforeAll(async () => {
  await warmMarkdownRenderer();
});

const READER = "alice";
const KEY = "ACME-1";
const PLANNER = { id: "dashboard-planner", kind: "session" } as const;
const MARK = "2026-11-25T10:00:00Z";
const BEFORE = "2026-11-25T09:00:00Z";
const AFTER = "2026-11-25T11:00:00Z";
const COMMENT_BASE = changesBody[0] as unknown as CommentWithTrees;

function comment(id: string, body: string, at: string, author: CommentWithTrees["author"]) {
  return {
    ...COMMENT_BASE,
    anchor: null,
    author,
    body,
    body_tree: undefined,
    created_at: at,
    deleted_at: null,
    deleted_by: null,
    id,
    issue_key: KEY,
    reply_to: null,
  } as CommentWithTrees;
}

function message(id: string, body: string, at: string, author: MessageWithTrees["author"]) {
  return {
    author,
    body,
    created_at: at,
    deleted_at: null,
    edited_at: null,
    id,
    issue_key: KEY,
    target: null,
  } as unknown as MessageWithTrees;
}

function row(kind: MyReplyRow["kind"], threadId: string): MyReplyRow {
  return {
    kind,
    read_through: MARK,
    ref: `dispatch://${KEY}/${kind}/${threadId}`,
    reply: { author: PLANNER, created_at: AFTER },
    subject: "",
    thread_id: threadId,
    unread: true,
    unread_count: 1,
  } as unknown as MyReplyRow;
}

/** Draws `thread`'s item; `kept` is a reply the reader typed in the thread's reply box and never
 *  sent, which the box kept under the thread's send key. */
function draw(thread: MyReplyRow, kept?: SentDraft) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // The agents list as the shell's poll leaves it in the cache, read a while ago, and the shell's
  // poller (`app.tsx`), which reads it again as soon as anything drawn asks for live agents: the
  // names a thread draws come from the cache, and nothing on the item reads it again until the
  // reader reaches the reply control (R8).
  client.setQueryData(agentsQuery().queryKey, [], { updatedAt: 1 });
  if (kept !== undefined) {
    unsentDraftsOf(client).write(composerNameOf(replySendKey(thread.thread_id)), kept);
  }
  return render(
    <MemoryRouter>
      <AppProviders client={client}>
        <AgentRegistryPoller />
        <NoTooltips>
          <ThreadItem closed={false} issueKey={KEY} row={thread} viewer={READER} />
        </NoTooltips>
      </AppProviders>
    </MemoryRouter>
  );
}

const spies: { mockRestore: () => void }[] = [];
afterEach(() => {
  for (const spy of spies.splice(0)) spy.mockRestore();
});

test("a comment thread draws its root, the unread write and the reply box, reading its own route", async () => {
  const read: CommentRead = {
    comment: comment("c-root", "Does the count drop?", BEFORE, { id: READER, kind: "user" }),
    replies: [
      comment("c-old", "An old reply.", BEFORE, PLANNER),
      comment("c-new", "It drops when the write lands.", AFTER, PLANNER),
      comment("c-own", "My own later word.", AFTER, { id: READER, kind: "user" }),
    ],
  };
  const getComment = spyOn(api, "getComment").mockResolvedValue(read);
  const listComments = spyOn(api, "listComments");
  spies.push(getComment, listComments);
  const view = draw(row("comment", "c-root"));
  try {
    await screen.findByText("It drops when the write lands.");
    expect(screen.getByText("Does the count drop?")).toBeTruthy();
    expect(screen.queryByText("An old reply.")).toBeNull();
    expect(screen.queryByText("My own later word.")).toBeNull();
    expect(screen.getByRole("textbox", { name: "Reply" })).toBeTruthy();
    expect(
      screen.getByRole("link", { name: DASHBOARD_WORDS.thread_link }).getAttribute("href")
    ).toBe(`/issues/${KEY}/comments/c-root`);
    expect(getComment).toHaveBeenCalledWith("c-root");
    expect(listComments).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

test("a message thread draws its root and the unread write, and links the message", async () => {
  const read = {
    message: message("m-root", "The tiles read the results.", BEFORE, PLANNER),
    replies: [message("m-new", "The children from the issue read.", AFTER, PLANNER)],
  } as unknown as MessageRead;
  const getMessage = spyOn(api, "getMessage").mockResolvedValue(read);
  const listIssueMessages = spyOn(api, "listIssueMessages");
  spies.push(getMessage, listIssueMessages);
  const view = draw(row("message", "m-root"));
  try {
    await screen.findByText("The children from the issue read.");
    expect(screen.getByText("The tiles read the results.")).toBeTruthy();
    expect(
      screen.getByRole("link", { name: DASHBOARD_WORDS.thread_link }).getAttribute("href")
    ).toBe(`/issues/${KEY}/messages/m-root`);
    expect(getMessage).toHaveBeenCalledWith(KEY, "m-root");
    expect(listIssueMessages).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

test("an ask thread draws the ask body and the unread reply, and links the ask", async () => {
  const ask = { ...asksOpenBody[0], issue_key: KEY } as unknown as AskWithTrees;
  const read = {
    ask,
    replies: [comment("r-new", "Which thread do you mean?", AFTER, PLANNER)],
  } as unknown as AskRead;
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(read);
  spies.push(getAsk);
  const view = draw(row("ask", ask.id));
  try {
    await screen.findByText("Which thread do you mean?");
    expect(view.container.querySelector("[data-ask-body]")).not.toBeNull();
    expect(
      screen.getByRole("link", { name: DASHBOARD_WORDS.thread_link }).getAttribute("href")
    ).toBe(`/issues/${KEY}/asks/${ask.id}`);
    expect(getAsk).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

test("a thread whose root its author deleted draws no reply box, only a kept draft for it", async () => {
  const root = {
    ...comment("c-gone", "", BEFORE, PLANNER),
    deleted_at: BEFORE,
    deleted_by: PLANNER,
  } as CommentWithTrees;
  const read: CommentRead = {
    comment: root,
    replies: [comment("c-after", "A reply before the delete.", AFTER, PLANNER)],
  };
  spies.push(spyOn(api, "getComment").mockResolvedValue(read));
  const view = draw(row("comment", "c-gone"));
  try {
    await screen.findByText("A reply before the delete.");
    // `KeptReplyToDeleted` stands where the box would: it draws a draft kept for the root, and
    // nothing while none is kept.
    expect(screen.queryByRole("textbox", { name: "Reply" })).toBeNull();
  } finally {
    view.unmount();
  }
});

// A reply the reader kept for a deleted root stands in the box's place, naming the root's author
// from what the cache holds (`agentNames="cache"`): drawing it sends no agents read (R8), where
// `AgentName`'s live form would read the cache's stale list again.
test("a reply kept for a deleted root names its author from the cache and reads no agents", async () => {
  const author = {
    id: "dashboard-planner-titled",
    kind: "session",
    origin: { session_title: "Planner (dashboard)" },
  } as const;
  const root = {
    ...comment("c-kept", "", BEFORE, author),
    deleted_at: BEFORE,
    deleted_by: author,
  } as CommentWithTrees;
  spies.push(spyOn(api, "getComment").mockResolvedValue({ comment: root, replies: [] }));
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  spies.push(listAgents);
  const typed = "Raise it to five.";
  const view = draw(row("comment", "c-kept"), { body: typed, replacement: "" });
  try {
    const kept = await screen.findByRole("status");
    expect(kept.textContent).toBe(
      `The comment by Planner (dashboard) that your reply answered was deleted, so it takes no reply. Your reply is kept here:${typed}`
    );
    expect(screen.getByRole("button", { name: "Discard my draft" })).toBeTruthy();
    expect(listAgents).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

test("the reply control sends no agents read until the reader reaches it", async () => {
  const read: CommentRead = {
    comment: comment("c-reach", "Root words.", BEFORE, PLANNER),
    replies: [comment("c-reach-1", "A write to answer.", AFTER, PLANNER)],
  };
  spies.push(spyOn(api, "getComment").mockResolvedValue(read));
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([]);
  spies.push(listAgents);
  const view = draw(row("comment", "c-reach"));
  try {
    const box = await screen.findByRole("textbox", { name: "Reply" });
    expect(listAgents).not.toHaveBeenCalled();
    act(() => box.focus());
    await waitFor(() => expect(listAgents).toHaveBeenCalledTimes(1));
  } finally {
    view.unmount();
  }
});
