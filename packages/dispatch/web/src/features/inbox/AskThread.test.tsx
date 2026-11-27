import { beforeAll, expect, jest, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: a paste below carries a file, which user-event has no form for
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { correction, correctionFeed, DELETION, EDITED_AT } from "../../__tests__/correction-feed";
import { realClock, tick } from "../../__tests__/fake-clock";
import { heldNotDisabled } from "../../__tests__/held-press";
import { heldUntilLate, pressesOf } from "../../__tests__/held-until-late";
import { untilAttempted } from "../../__tests__/stop-late-write";
import { userOnFakeClock } from "../../__tests__/user";
import { ApiError, api, NetworkError } from "../../api/client";
import { useEventFeed } from "../../api/event-feed";
import { askThreadQuery, whoAmIQuery } from "../../api/queries";
import type { Ask, AskRead, Comment, CreateCommentInput, IssueAskRow } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { SEND_DEADLINE_MS } from "../writes/keyed-write";
import { AskReplyComposer, AskThread, type AskThreadQuery, type CreateReply } from "./AskThread";
import { AskReplyWritesContext, useOutstandingAnswerWrites } from "./outstanding-answers";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The question block of CORE-1's spec, as the issue's ask list carries it: what a map built
 *  from that list would have named `#q2`, and what the reply below does not mean. */
const specQuestionAsk: IssueAskRow = {
  anchor: null,
  answer: null,
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  author: { id: "planner", kind: "session" },
  block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
  block_id: "q2",
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-2",
  issue_key: "CORE-1",
  kind: "question",
  last_reply: null,
  multiple: false,
  opened_event_id: 1,
  options: [],
  question: "Which way round?",
  state: "open",
  urgency: "med",
};

const answeredAsk: Ask = {
  anchor: null,
  answer: { at: "2026-09-09T01:00:00Z", selected: [], text: "Done.", user: "alice" },
  artifact_id: null,
  asked_of: null,
  asked_of_at: null,
  asked_of_by: null,
  author: { id: "session-1", kind: "session" },
  block_id: null,
  created_at: "2026-09-09T00:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [],
  question: "Should this ship?",
  state: "answered",
  urgency: "med",
};

test.each(["button", "field", "remount"] as const)(
  "%s lets go at the reply write deadline without posting twice",
  async (from) => {
    const create = spyOn(api, "createComment").mockRejectedValue(
      new NetworkError("/api/v1/issues/CORE-1/comments", new TypeError("Failed to fetch"))
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const composer = (key: string) => (
      <QueryClientProvider client={client}>
        <AskReplyComposer ask={answeredAsk} createReply={create} key={key} />
      </QueryClientProvider>
    );
    const view = render(composer("one"));
    try {
      const prepare = userEvent.setup();
      const box = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
      await prepare.click(box);
      await prepare.paste("The rollout is on Friday.");
      const button = screen.getByRole<HTMLButtonElement>("button", { name: "Reply" });
      jest.useFakeTimers();
      const user = userOnFakeClock();
      const sent = () => pressesOf([create]);
      const press = from === "field" ? "{Control>}{Enter}{/Control}" : "{Enter}";
      if (from === "remount") {
        await user.click(button);
        await tick(0);
        view.rerender(composer("two"));
        await tick(SEND_DEADLINE_MS - 1_000);
        expect(screen.getByRole("textbox", { name: "Reply" })).toHaveAttribute(
          "aria-disabled",
          "true"
        );
        await tick(1_000);
        view.rerender(composer("three"));
        expect(screen.getByRole("textbox", { name: "Reply" })).not.toHaveAttribute("aria-disabled");
        screen.getByRole("button", { name: /Reply/ }).focus();
      } else {
        await heldUntilLate(user, from === "field" ? box : button, {
          label: "Reply on “Should this ship?”",
          name: "Reply",
          press,
          queryClient: client,
          sent,
        });
      }
      await user.keyboard(press);
      await tick(1_000);
      const stillSending = /Reply sent nothing:.*reply is still being sent/;
      expect(screen.getByText(stillSending)).toBeInTheDocument();
      expect(sent()).toBe(1);
      create.mockRejectedValue(new ApiError(403, { error: "Cannot reply" }));
      await tick(30_000);
      expect(screen.queryByText(stillSending)).toBeNull();
      await user.keyboard(press);
      await tick(1_000);
      expect(sent()).toBe(2);
    } finally {
      realClock();
      view.unmount();
      client.clear();
      create.mockRestore();
    }
  }
);

test("Retry lets go at the reply write deadline but cannot send while another composer is sending", async () => {
  const refused = new ApiError(403, { error: "Cannot reply" });
  const dropped = new NetworkError(
    "/api/v1/issues/CORE-1/comments",
    new TypeError("Failed to fetch")
  );
  const create = spyOn(api, "createComment").mockRejectedValue(refused);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <section aria-label="First composer">
        <AskReplyComposer ask={answeredAsk} createReply={create} />
      </section>
      <section aria-label="Second composer">
        <AskReplyComposer ask={answeredAsk} createReply={create} />
      </section>
    </QueryClientProvider>
  );
  try {
    const first = within(screen.getByRole("region", { name: "First composer" }));
    const second = within(screen.getByRole("region", { name: "Second composer" }));
    const prepare = userEvent.setup();
    const box = first.getByRole("textbox", { name: "Reply" });
    await prepare.click(box);
    await prepare.paste("The rollout is on Friday.");
    await prepare.click(first.getByRole("button", { name: "Reply" }));
    const retry = await first.findByRole("button", { name: "Retry" });
    create.mockRejectedValue(dropped);
    jest.useFakeTimers();
    const user = userOnFakeClock();
    await user.click(second.getByRole("button", { name: "Reply" }));
    await tick(0);
    await tick(SEND_DEADLINE_MS - 1_000);
    expect(retry, "Retry, a second short of the deadline").toHaveAttribute("aria-disabled", "true");
    await tick(1_000);
    expect(retry, "Retry, past the deadline").not.toHaveAttribute("aria-disabled");
    await user.click(retry);
    await tick(1_000);
    expect(pressesOf([create]), "Retry must not post while the other reply is out").toBe(2);
    expect(first.getByRole("status")).toHaveTextContent("your reply is still being sent");
    create.mockRejectedValue(refused);
    await tick(30_000);
    await user.click(retry);
    await tick(1_000);
    expect(pressesOf([create])).toBe(3);
  } finally {
    realClock();
    view.unmount();
    client.clear();
    create.mockRestore();
  }
});

/** An Inbox's replies around `children`, as the Inbox provides them (`useOutstandingAnswerWrites`):
 *  a composer under it makes its press through the Inbox's write of its ask. */
function InboxReplies({ children }: { children: ReactNode }): ReactNode {
  return (
    <AskReplyWritesContext value={useOutstandingAnswerWrites()}>{children}</AskReplyWritesContext>
  );
}

// Inside an Inbox the reply is the Inbox's write of the ask, which holds Reply and the box as the
// composer's own does: until its deadline and no longer. Past it a press while the reply is still
// out posts nothing and says so, and once the reply fails a press posts one.
test("an Inbox's Reply lets go at the reply write deadline without posting twice", async () => {
  const create = spyOn(api, "createComment").mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1/comments", new TypeError("Failed to fetch"))
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <InboxReplies>
        <AskReplyComposer ask={answeredAsk} createReply={create} />
      </InboxReplies>
    </QueryClientProvider>
  );
  try {
    const prepare = userEvent.setup();
    const box = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
    await prepare.click(box);
    await prepare.paste("The rollout is on Friday.");
    const button = screen.getByRole<HTMLButtonElement>("button", { name: "Reply" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const sent = () => pressesOf([create]);
    await heldUntilLate(user, button, {
      label: "Reply on “Should this ship?”",
      name: "Reply",
      queryClient: client,
      sent,
    });
    expect(box, "the box, past the deadline").not.toHaveAttribute("aria-disabled");
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(sent(), "a press while the reply is out posts nothing").toBe(1);
    const stillSending = /Reply sent nothing:.*reply is still being sent/;
    expect(screen.getByText(stillSending)).toBeInTheDocument();
    create.mockRejectedValue(new ApiError(403, { error: "Cannot reply" }));
    await tick(30_000);
    expect(screen.queryByText(stillSending)).toBeNull();
    await user.keyboard("{Enter}");
    await tick(1_000);
    expect(sent(), "a press once the reply failed posts one").toBe(2);
  } finally {
    realClock();
    view.unmount();
    client.clear();
    create.mockRestore();
  }
});

// A picture still uploading is not in the draft yet, so Reply waits for it - but only until the
// upload's deadline: an upload that never answers is still tried, its text joins the draft if it
// lands, and Reply posts what the box holds.
test("Reply lets go of a pasted picture's upload at its deadline and posts", async () => {
  const upload = spyOn(api, "uploadArtifact").mockRejectedValue(
    new NetworkError("/api/v1/issues/CORE-1/artifacts", new TypeError("Failed to fetch"))
  );
  const create = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(403, { error: "Cannot reply" })
  );
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const view = render(
    <QueryClientProvider client={client}>
      <AskReplyComposer ask={answeredAsk} createReply={create} uploadOwner={{ issue: "CORE-1" }} />
    </QueryClientProvider>
  );
  try {
    const prepare = userEvent.setup();
    const box = screen.getByRole<HTMLTextAreaElement>("textbox", { name: "Reply" });
    await prepare.click(box);
    await prepare.paste("The rollout is on Friday.");
    const button = screen.getByRole<HTMLButtonElement>("button", { name: "Reply" });
    jest.useFakeTimers();
    const user = userOnFakeClock();
    const file = new File([new Uint8Array([137, 80, 78, 71])], "shot.png", { type: "image/png" });
    // user-event's paste carries text, never files.
    fireEvent.paste(box, { clipboardData: { files: [file] } });
    await untilAttempted(upload, 1);

    await tick(SEND_DEADLINE_MS - 1_000);
    expect(heldNotDisabled(button, "Reply"), "Reply, a second short of the deadline").toBe(true);
    await user.click(button);
    expect(create, "a press while the upload holds Reply").not.toHaveBeenCalled();

    await tick(1_000);
    expect(screen.getByText("Uploading file…"), "the upload, past its deadline").toBeVisible();
    expect(heldNotDisabled(button, "Reply"), "Reply, past the upload's deadline").toBe(false);
    await user.click(button);
    await tick(1_000);
    expect(pressesOf([create]), "a press past the upload's deadline posts the reply").toBe(1);
  } finally {
    realClock();
    view.unmount();
    client.clear();
    upload.mockRestore();
    create.mockRestore();
  }
});

const reply: Comment = {
  anchor: null,
  ask_id: answeredAsk.id,
  author: { id: "alice", kind: "user" },
  body: "Holding on #q2 for now.",
  created_at: "2026-09-09T02:00:00Z",
  edited_at: null,
  id: "comment-1",
  issue_key: "CORE-1",
  reply_to: null,
  resolved: false,
  resolved_at: null,
  resolved_by: null,
  suggestion: null,
  turn: null,
  ...commentDeliveryFields(),
};

const read: AskRead = {
  answers: [],
  ask: answeredAsk,
  claimant: null,
  edits: [],
  followers: [],
  replies: [reply],
  route: null,
  route_holder: null,
  route_status: null,
  route_status_reason: null,
};

/** The ask `#q2` names in the reply, read by the reference the reply draws to title it. */
const designAsk: Ask = {
  ...answeredAsk,
  answer: null,
  id: "ask-design",
  question: "Which way round?",
  state: "open",
};

test("an Inbox reply's `#q2` names the question of the document its comment is anchored to", async () => {
  // The reply is anchored to `design.md`, which is no primary document of the issue: the server
  // scopes the comment's text to that document (`refs.ScopeOf`), so `#q2` is that document's
  // question. A map built from the issue's ask list would have chosen the spec's `q2` instead.
  const listIssueAsks = spyOn(api, "listIssueAsks").mockResolvedValue([specQuestionAsk]);
  const getAsk = spyOn(api, "getAsk").mockResolvedValue({ ...read, ask: designAsk, replies: [] });
  const getReferenceQuestions = spyOn(api, "getReferenceQuestions").mockResolvedValue({
    document: "artifact-design",
    questions: [
      { ask: "ask-design", block_id: "q2", reference: "dispatch://CORE-1/ask/ask-design" },
    ],
  });
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const thread = { data: read } as unknown as AskThreadQuery;
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskThread ask={answeredAsk} order="oldest" showComposer={false} thread={thread} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const linked = () =>
      [...view.container.querySelectorAll("[data-reply] [data-dispatch-ref]")].map((anchor) =>
        anchor.getAttribute("data-dispatch-ref")
      );
    await waitFor(() => expect(linked()).toEqual(["dispatch://CORE-1/ask/ask-design"]));
    expect(getReferenceQuestions).toHaveBeenCalledWith("comment", "comment-1");
    // One map: the page reads the server's, never the issue's whole ask list.
    expect(listIssueAsks).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getAsk.mockRestore();
    getReferenceQuestions.mockRestore();
    listIssueAsks.mockRestore();
  }
});

test("a deleted reply in an ask thread reads as deleted and gives no part of its body", async () => {
  // The reply keeps its body beside the marks, so only the thread's own reading of the marks
  // keeps "Holding on #q2 for now." off the page.
  const deleted: Comment = {
    ...reply,
    deleted_at: "2026-09-09T03:00:00Z",
    deleted_by: { id: "alice", kind: "user" },
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const thread = { data: { ...read, replies: [deleted] } } as unknown as AskThreadQuery;
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskThread ask={answeredAsk} order="oldest" showComposer={false} thread={thread} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const row = view.container.querySelector("[data-reply]");
    if (row === null) throw new Error("the thread drew no reply");
    await waitFor(() => expect(row.textContent).toContain("Deleted by alice"));
    expect(row.textContent).not.toContain("Holding on");
  } finally {
    view.unmount();
  }
});

// A reply its author edited carries ` · edited` after its byline's time, as the post's own
// card writes it (`EditedMark`), and a reply nobody edited carries nothing there.
test("an edited reply in an ask thread marks its byline edited, and one nobody edited does not", async () => {
  const edited: Comment = {
    ...reply,
    body: "Holding until Friday.",
    edited_at: "2026-09-09T03:00:00Z",
  };
  const standing: Comment = {
    ...reply,
    body: "Agreed.",
    created_at: "2026-09-09T04:00:00Z",
    id: "comment-2",
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const thread = { data: { ...read, replies: [edited, standing] } } as unknown as AskThreadQuery;
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskThread ask={answeredAsk} order="oldest" showComposer={false} thread={thread} />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    const rows = [...view.container.querySelectorAll("[data-reply]")];
    expect(rows).toHaveLength(2);
    const [corrected, kept] = rows as [Element, Element];
    // The byline is the reply's last line, under its body.
    await waitFor(() => expect(corrected.textContent).toContain("Holding until Friday."));
    expect(corrected.lastElementChild?.textContent).toMatch(/^alice · .+ · edited$/);
    await waitFor(() => expect(kept.textContent).toContain("Agreed."));
    expect(kept.lastElementChild?.textContent).toMatch(/^alice · /);
    expect(kept.textContent).not.toContain("edited");
  } finally {
    view.unmount();
  }
});

// R2 and R3 of dispatch://INFRA-1523, and the architect's ruling of 2026-11-20 00:23Z. A thread
// drawn inline reads the ask's shared thread (`["ask-thread", id]`) and says nothing when its
// refetch fails: it keeps the replies it held, so a reply's correction writes its own facts into
// the thread the cache holds (`markCorrectedPost`). The edited reply reads as edited, the deleted
// one as the deleted line with no word of its body, and a reply quoting a block of it holds that
// block as a read of the reply answers it: edited, then removed with no text.
test("a thread whose refetch fails after a reply's edit and delete reads the reply as edited, then deleted", async () => {
  const id = "d4e5f6a7-b8c9-4d0e-8f1a-2b3c4d5e6f7a";
  const corrected: Comment = { ...reply, body: "Holding the release for review.", id };
  const block = { block_id: "b1", node: `comment/${id}#b1`, removed: false, text: "Holding" };
  const quoting: Comment = {
    ...reply,
    body: "Agreed.",
    created_at: "2026-09-09T04:00:00Z",
    id: "e5f6a7b8-c9d0-4e1f-9a2b-3c4d5e6f7a8b",
    reply_block: block,
  };
  let failing = false;
  const getAsk = spyOn(api, "getAsk").mockImplementation(async () => {
    if (failing) throw new Error("the thread's read failed");
    return { ...read, replies: [corrected, quoting] };
  });
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(whoAmIQuery().queryKey, { event_head: "0", kind: "user", login: "alice" });
  const feed = correctionFeed(client);
  function Page(): ReactNode {
    useEventFeed();
    const thread = useQuery(askThreadQuery(answeredAsk.id));
    return <AskThread ask={answeredAsk} order="oldest" showComposer={false} thread={thread} />;
  }
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <Page />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const row = () => view.container.querySelector("[data-reply]");
  const quoted = () =>
    client.getQueryData(askThreadQuery(answeredAsk.id).queryKey)?.replies?.[1]?.reply_block;
  const ask = { ask_id: answeredAsk.id };

  try {
    await waitFor(() => expect(row()?.textContent).toContain("Holding the release for review."));
    feed.start();
    failing = true;

    await feed.deliver(correction("comment", id, true, ask), [["ask-thread"]]);
    expect(row()?.lastElementChild?.textContent).toMatch(/^alice · .+ · edited$/);
    expect(quoted()).toEqual({ ...block, edited_at: EDITED_AT });

    await feed.deliver(correction("comment", id, false, ask), [["ask-thread"]]);
    await waitFor(() => expect(row()?.textContent).toContain("Deleted by alice"));
    expect(row()?.textContent).not.toContain("Holding");
    expect(quoted()).toEqual({
      ...block,
      ...DELETION,
      edited_at: EDITED_AT,
      removed: true,
      text: "",
    });
  } finally {
    view.unmount();
    feed.restore();
    client.clear();
    getAsk.mockRestore();
  }
});

// A settled question's conversation goes on: however an ask was resolved, its thread keeps the
// reply composer an answered ask's has, and what it sends is a reply on that ask.
test("a resolved ask's thread takes a reply on that ask, whichever way it was resolved", async () => {
  const user = userEvent.setup();
  for (const kind of ["resolved", "retracted", "declined", "withdrawn"]) {
    const resolved: Ask = {
      ...answeredAsk,
      answer: null,
      resolution: {
        actor: { id: "session-1", kind: "session" },
        at: "2026-09-09T03:00:00Z",
        kind,
        reason: "Settled elsewhere.",
      },
      state: "resolved",
    };
    const sent: [string, CreateCommentInput][] = [];
    const createReply: CreateReply = async (issueKey, input) => {
      sent.push([issueKey, input]);
      return { ...reply, body: input.body ?? "", id: `reply-${kind}` };
    };
    const queryClient = new QueryClient({
      defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
    });
    const thread = { data: { ...read, ask: resolved, replies: [] } } as unknown as AskThreadQuery;
    const view = render(
      <MemoryRouter>
        <QueryClientProvider client={queryClient}>
          <AskThread ask={resolved} createReply={createReply} order="oldest" thread={thread} />
        </QueryClientProvider>
      </MemoryRouter>
    );

    try {
      await user.click(view.getByRole("textbox", { name: "Reply" }));
      await user.paste(`Still worth a look (${kind}).`);
      await user.click(view.getByRole("button", { name: "Reply" }));
      await waitFor(() =>
        expect(sent).toEqual([
          ["CORE-1", { ask_id: "ask-1", body: `Still worth a look (${kind}).` }],
        ])
      );
    } finally {
      view.unmount();
    }
  }
});
