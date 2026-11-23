import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { renderWithQueries as render } from "../../__tests__/query-provider";
import type { AskAnswer, DispatchEvent } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { EventBody, EventEditedMark } from "./EventBody";

const markdownMessage: DispatchEvent = {
  actor: { id: "alice", kind: "user" },
  created_at: "2026-09-10T00:00:00Z",
  id: 1,
  issue_key: "CORE-1",
  notify: true,
  payload: {
    author: { id: "alice", kind: "user" },
    body: "# Status\n\nThe **document** is [ready](https://example.com).",
    broadcast_id: null,
    reply_block: null,
    created_at: "2026-09-10T00:00:00Z",
    deliveries: [],
    id: "message-1",
    in_reply_to: null,
    issue_key: "CORE-1",
    target: null,
  },
  seq: 1,
  type: "message.created",
};

test("EventBody renders Markdown event content", async () => {
  const view = render(<EventBody event={markdownMessage} />);

  try {
    expect(await screen.findByRole("heading", { name: "Status" })).toBeDefined();
    expect(screen.getByText("document", { selector: "strong" })).toBeDefined();
    expect(screen.getByRole("link", { name: "ready" }).getAttribute("href")).toBe(
      "https://example.com"
    );
  } finally {
    view.unmount();
  }
});

test("EventBody renders unsupported raw HTML as literal text", async () => {
  const view = render(
    <EventBody
      event={{
        ...markdownMessage,
        payload: { ...markdownMessage.payload, body: '<img src="x">' },
      }}
    />
  );

  try {
    expect(await screen.findByText('<img src="x">')).not.toBeNull();
    expect(view.container.querySelector("img")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("EventBody shows the answer a change replaced, struck through", async () => {
  const changed: Extract<DispatchEvent, { type: "ask.answered" }> = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-11T00:00:00Z",
    id: 3,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      anchor: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      block_id: null,
      answer: { at: "2026-09-11T00:00:00Z", selected: ["Hold"], text: null, user: "alice" },
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [{ label: "Ship" }, { label: "Hold" }],
      previous_answer: {
        at: "2026-09-10T12:00:00Z",
        selected: ["Ship"],
        text: "Go now.",
        user: "alice",
      },
      question: "Publish?",
      state: "answered",
      urgency: "med",
    },
    seq: 3,
    type: "ask.answered",
  };
  const view = render(<EventBody event={changed} />);

  try {
    const was = await screen.findByText("was: Ship - Go now.");
    expect(was.closest("s")).not.toBeNull();
    const { previous_answer: _dropped, ...first } = changed.payload;
    view.rerender(<EventBody event={{ ...changed, payload: first }} />);
    expect(screen.queryByText(/^was:/)).toBeNull();
  } finally {
    view.unmount();
  }
});

// An answer an agent gave for its person is recorded as the person's, so the feed names the
// agent beside them: a reader must not take the agent's answer for the person's own words.
test("EventBody names the agent an answer was given through, beside the person", async () => {
  // The answer below is a frame's payload, not a route's read of one: the route states
  // `selected` as a nilable list because the Go field is a nilable slice, while an answer
  // Dispatch writes always carries the list (`model.AskAnswer` normalises nil to `[]`). So it
  // is stated as the payload states it rather than borrowed from the read's wider type.
  const alice = {
    at: "2026-09-11T00:00:00Z",
    selected: ["Ship"],
    text: null,
    user: "alice",
  } satisfies AskAnswer;
  const answered: Extract<DispatchEvent, { type: "ask.answered" }> = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-11T00:00:00Z",
    id: 5,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      anchor: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      block_id: null,
      answer: { ...alice, act: { session: "0123456789abcdef", scopes: ["person"] } },
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-2",
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [{ label: "Ship" }, { label: "Hold" }],
      question: "Publish?",
      state: "answered",
      urgency: "med",
    },
    seq: 5,
    type: "ask.answered",
  };
  const answeredWith = (
    answer: Extract<DispatchEvent, { type: "ask.answered" }>["payload"]["answer"]
  ): Extract<DispatchEvent, { type: "ask.answered" }> => ({
    ...answered,
    payload: { ...answered.payload, answer },
  });
  const view = render(<EventBody event={answered} />);

  try {
    expect(
      await screen.findByText("Answered by alice, acting through session 0123456789abcdef")
    ).toBeDefined();
    view.rerender(
      <EventBody event={answeredWith({ ...alice, act: { token_name: "architect" } })} />
    );
    expect(screen.getByText("Answered by alice, acting through token architect")).toBeDefined();
    view.rerender(<EventBody event={answeredWith(alice)} />);
    expect(screen.getByText("Answered by alice")).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("EventBody names how an ask was resolved: a withdrawal by the close, a retraction with its reason", async () => {
  const withdrawn: Extract<DispatchEvent, { type: "ask.resolved" }> = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-10-25T00:00:00Z",
    id: 4,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      anchor: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      block_id: null,
      answer: null,
      author: { id: "session-1", kind: "session" },
      created_at: "2026-10-24T00:00:00Z",
      edited_at: null,
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [{ label: "Ship" }, { label: "Hold" }],
      question: "Publish?",
      resolution: {
        actor: { id: "alice", kind: "user" },
        at: "2026-10-25T00:00:00Z",
        kind: "withdrawn",
        reason: "Withdrawn by the close of CORE-1.",
      },
      state: "resolved",
      urgency: "med",
    },
    seq: 4,
    type: "ask.resolved",
  };
  // A session actor renders through `AgentName`, which links to its live page and reads the
  // agent registry.
  const client = new QueryClient();
  const view = render(<EventBody event={withdrawn} />, {
    wrapper: ({ children }) => (
      <MemoryRouter>
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      </MemoryRouter>
    ),
  });

  try {
    expect(await screen.findByText(/^Withdrawn when alice closed the issue/)).toBeDefined();
    expect(screen.queryByText(/Withdrawn by the close/)).toBeNull();
    expect(screen.queryByText(/Resolved by/)).toBeNull();
    view.rerender(
      <EventBody
        event={{
          ...withdrawn,
          payload: {
            ...withdrawn.payload,
            resolution: {
              actor: { id: "session-1", kind: "session" },
              at: "2026-10-25T00:00:00Z",
              kind: "retracted",
              reason: "Superseded by **the spec**.",
            },
          },
        }}
      />
    );
    await waitFor(() =>
      expect(view.container.textContent).toContain(
        "Retracted by session:session-… - Superseded by the spec."
      )
    );
    expect((await screen.findByText("the spec")).tagName).toBe("STRONG");
  } finally {
    view.unmount();
  }
});

// An ask.answered event written before answers stored an empty list holds null where the
// contract says a list; the body reads it as no option chosen rather than throwing.
test("EventBody reads a null selected list on an answer and its previous answer as empty", async () => {
  // The answer is read as soon as the struck line shows, and only a loaded engine has drawn it by
  // then (`MarkdownBody`): the first body of a file takes the asynchronous path.
  await warmMarkdownRenderer();
  const stored: Extract<DispatchEvent, { type: "ask.answered" }> = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-11T00:00:00Z",
    id: 4,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      anchor: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      block_id: null,
      answer: {
        at: "2026-09-11T00:00:00Z",
        selected: null as unknown as string[],
        text: "Hold it.",
        user: "alice",
      },
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [{ label: "Ship" }, { label: "Hold" }],
      previous_answer: {
        at: "2026-09-10T12:00:00Z",
        selected: null as unknown as string[],
        text: "Go now.",
        user: "alice",
      },
      question: "Publish?",
      state: "answered",
      urgency: "med",
    },
    seq: 4,
    type: "ask.answered",
  };
  const view = render(<EventBody event={stored} />);

  try {
    const was = await screen.findByText("was: Go now.");
    expect(was.closest("s")).not.toBeNull();
    expect(screen.getByText("Hold it.")).not.toBeNull();
  } finally {
    view.unmount();
  }
});

// A body is bounded block by block, so one event carries its first part alone and says so with
// `body_truncated`. The row then shows the text it has and a link to the item's own page, which
// reads the stored body whole; an event that carries its body whole shows nothing extra.
test("EventBody links to the whole item when the event carried the body in part", async () => {
  const cut: DispatchEvent = {
    ...markdownMessage,
    payload: { ...markdownMessage.payload, body: "The first part.", body_truncated: true },
  };
  const view = render(<EventBody event={cut} />, {
    wrapper: ({ children }) => <MemoryRouter>{children}</MemoryRouter>,
  });

  try {
    expect(await screen.findByText("The first part.")).not.toBeNull();
    const link = screen.getByRole("link", { name: "Read the whole message" });
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/messages/message-1");

    view.rerender(<EventBody event={markdownMessage} />);
    await waitFor(() => expect(screen.queryByRole("link", { name: /Read the whole/ })).toBeNull());
  } finally {
    view.unmount();
  }
});

// A comment's link reads "Read the whole comment" and opens the comment's own page.
test("EventBody links to the whole comment when a comment event carried its body in part", async () => {
  const cut: DispatchEvent = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-10T00:00:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      anchor: null,
      answers: null,
      artifact_id: null,
      reply_block: null,
      artifact_name: "",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "The first paragraph.",
      body_truncated: true,
      created_at: "2026-09-10T00:00:00Z",
      deliveries: [],
      edited_at: null,
      id: "comment-1",
      issue_key: "CORE-1",
      mentions: [],
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: null,
    },
    seq: 2,
    type: "comment.created",
  };
  const view = render(<EventBody event={cut} />, {
    wrapper: ({ children }) => <MemoryRouter>{children}</MemoryRouter>,
  });

  try {
    expect(await screen.findByText("The first paragraph.")).not.toBeNull();
    expect(screen.getByRole("link", { name: "Read the whole comment" }).getAttribute("href")).toBe(
      "/issues/CORE-1/comments/comment-1"
    );
  } finally {
    view.unmount();
  }
});

// A comment on a project document belongs to no issue: its link opens the document with the
// comment in view, the address every other in-app link to one uses.
test("EventBody links a cut comment on a project document through that document", async () => {
  const cut: DispatchEvent = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-10T00:00:00Z",
    id: 3,
    issue_key: null,
    notify: true,
    payload: {
      anchor: null,
      answers: null,
      reply_block: null,
      artifact_id: "artifact-1",
      artifact_name: "runbook.md",
      artifact_slug: "runbook",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "The first paragraph.",
      body_truncated: true,
      created_at: "2026-09-10T00:00:00Z",
      deliveries: [],
      edited_at: null,
      id: "comment-2",
      issue_key: null,
      mentions: [],
      project_key: "CORE",
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: null,
    },
    seq: 3,
    type: "comment.created",
  };
  const view = render(<EventBody event={cut} />, {
    wrapper: ({ children }) => <MemoryRouter>{children}</MemoryRouter>,
  });

  try {
    expect(await screen.findByText("The first paragraph.")).not.toBeNull();
    expect(screen.getByRole("link", { name: "Read the whole comment" }).getAttribute("href")).toBe(
      "/projects/CORE/documents/runbook?comment=comment-2"
    );
  } finally {
    view.unmount();
  }
});

test("the event of a deleted post reads as deleted, with no body and no whole-post link", async () => {
  // The read of the event takes `body` and `body_truncated` out of the payload and stamps the
  // two marks in their place (`api/events.go`). This payload keeps both beside the marks, so the
  // heading and the whole-post link below can be drawn, and only the card's own reading of the
  // marks keeps them off: the same body renders its heading (the first row of this file), and
  // the same flag on a post that stands draws the link (the next row).
  const payload = {
    ...(markdownMessage.payload as unknown as Record<string, unknown>),
    body_truncated: true,
  };
  // With the engine loaded a body paints in the same pass as the deleted line, so a heading the
  // card drew would be on the page when the check below reads it.
  await warmMarkdownRenderer();
  const view = render(
    <MemoryRouter>
      <EventBody
        event={
          {
            ...markdownMessage,
            payload: {
              ...payload,
              deleted_at: "2026-09-10T00:05:00Z",
              deleted_by: { id: "alice", kind: "user" },
            },
          } as DispatchEvent
        }
      />
    </MemoryRouter>
  );

  try {
    await waitFor(() => expect(screen.getByText(/Deleted by/)).toBeDefined());
    expect(screen.queryByRole("heading", { name: "Status" })).toBeNull();
    expect(screen.queryByRole("link", { name: /Read the whole/ })).toBeNull();
  } finally {
    view.unmount();
  }
});

// The positive control of the row above: the same payload with no delete marks, a post that
// stands, draws the heading of its body and the link to the whole post that row says are absent.
test("the event of a standing post whose body was cut draws its body and the whole-post link", async () => {
  const view = render(
    <MemoryRouter>
      <EventBody
        event={
          {
            ...markdownMessage,
            payload: { ...markdownMessage.payload, body_truncated: true },
          } as DispatchEvent
        }
      />
    </MemoryRouter>
  );

  try {
    expect(await screen.findByRole("heading", { name: "Status" })).toBeDefined();
    expect(screen.getByRole("link", { name: "Read the whole message" })).toBeDefined();
  } finally {
    view.unmount();
  }
});

// The byline under a comment's event carries ` · edited` once its author replaced the body the
// event holds (`EventEditedMark`): the read of the event stamps the comment's own `edited_at` on
// it. A comment nobody edited carries nothing there, and neither does one edited and then deleted,
// whose deleted line already says what became of its text; a session's reply to a mention
// (`comment.answered`) reads the same way.
test("the byline of an edited comment's event is marked edited, and a standing or deleted one's is not", () => {
  const comment = (
    type: "comment.created" | "comment.answered",
    marks: Record<string, unknown>
  ): DispatchEvent =>
    ({
      actor: { id: "alice", kind: "user" },
      created_at: "2026-09-10T00:00:00Z",
      id: 4,
      issue_key: "CORE-1",
      notify: true,
      payload: {
        anchor: null,
        answers: null,
        artifact_id: null,
        artifact_name: "",
        ask_id: null,
        author: { id: "alice", kind: "user" },
        body: "Ship it on Friday.",
        created_at: "2026-09-10T00:00:00Z",
        deliveries: [],
        edited_at: null,
        id: "comment-4",
        issue_key: "CORE-1",
        mentions: [],
        reply_block: null,
        reply_to: null,
        resolved: false,
        resolved_at: null,
        resolved_by: null,
        suggestion: null,
        turn: null,
        ...marks,
      },
      seq: 4,
      type,
    }) as DispatchEvent;
  const byline = (event: DispatchEvent): string | null => {
    const view = render(<EventEditedMark event={event} />);
    try {
      return view.container.textContent;
    } finally {
      view.unmount();
    }
  };
  const edited = { edited_at: "2026-09-10T00:05:00Z" };
  const deleted = { deleted_at: "2026-09-10T00:10:00Z", deleted_by: { id: "alice", kind: "user" } };

  expect(byline(comment("comment.created", edited))).toBe(" · edited");
  expect(byline(comment("comment.answered", edited))).toBe(" · edited");
  expect(byline(comment("comment.created", {}))).toBe("");
  expect(byline(comment("comment.created", { ...edited, ...deleted }))).toBe("");
});
