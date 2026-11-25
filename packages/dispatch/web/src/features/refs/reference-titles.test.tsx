import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { ApiError, api } from "../../api/client";
import type { AskRead, CommentRead, IssueReferenceSummary, MessageRead } from "../../api/types";
import { MarkdownBody } from "./MarkdownBody";

const ASK_ID = "2a7f1c90-1f4e-4a2b-9c3d-5e6f70819a2b";

const summary: IssueReferenceSummary = {
  documents: [],
  key: "CORE-1",
  project: "CORE",
  status: "in_progress",
  title: "Fix the thing",
};

function askRead(id: string, question: string): AskRead {
  return {
    answers: [],
    ask: {
      anchor: null,
      answer: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      author: { id: "planner", kind: "session" },
      block_id: null,
      created_at: "2026-01-01T00:00:00Z",
      edited_at: null,
      id,
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [],
      question,
      state: "open",
      urgency: "med",
    },
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
  };
}

function withQueries(queryClient: QueryClient, children: ReactNode): ReactNode {
  return (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{children}</MemoryRouter>
    </QueryClientProvider>
  );
}

function freshClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

const spies: { mockRestore(): void }[] = [];

afterEach(() => {
  for (const spy of spies.splice(0)) {
    spy.mockRestore();
  }
});

test("an issue reference reads as its key then its title", async () => {
  const queryClient = freshClient();
  queryClient.setQueryData(["issue-summary", "CORE-1"], summary);
  const view = render(
    withQueries(queryClient, <MarkdownBody markdown="Tied to dispatch://CORE-1 here." />)
  );

  try {
    const link = await within(view.container).findByRole("link", { name: "CORE-1 Fix the thing" });
    expect(link.getAttribute("href")).toBe("/issues/CORE-1");
  } finally {
    view.unmount();
  }
});

test("a message reference reads as the headline of its body, as a comment's does", async () => {
  const message: MessageRead = {
    message: {
      author: { id: "planner", kind: "session" },
      body: "## Shipped it\n\nThe deploy went out at noon.",
      created_at: "2026-01-01T00:00:00Z",
      deliveries: [],
      id: "4c9f3e12-3b60-4c4d-9e5f-70819a2b3c4d",
      in_reply_to: null,
      issue_key: "CORE-1",
    },
    replies: [],
  } as unknown as MessageRead;
  spies.push(spyOn(api, "getMessage").mockResolvedValue(message));
  const view = render(
    withQueries(
      freshClient(),
      <MarkdownBody markdown="see dispatch://CORE-1/message/4c9f3e12-3b60-4c4d-9e5f-70819a2b3c4d" />
    )
  );

  try {
    const link = await within(view.container).findByRole("link");
    // The author is no title: the headline of what the message says is.
    await waitFor(() => expect(link.textContent).toBe("Shipped it The deploy went out at noon."));
  } finally {
    view.unmount();
  }
});

// A reference link's text is the headline of the post it names, so the link is a print of that
// post: a link to a post its author deleted says that it is deleted, and a link to a message its
// author edited marks it as edited (INFRA-1523, R2 and R3). A deleted post's body is empty, and a
// headline cut from it is an empty title, which drew the link as an anchor with no text and no
// size: the reader could not see that the post cited anything, nor hover or click the link.
const DELETED_COMMENT = "5b0e2c41-7d3a-4f6e-8a1b-2c3d4e5f6a7b";
const DELETED_MESSAGE = "6c1f3d52-8e4b-4a7f-9b2c-3d4e5f6a7b8c";
const EDITED_MESSAGE = "7d2a4e63-9f5c-4b8a-8c3d-4e5f6a7b8c9d";
const DELETED_AT = "2026-11-13T12:00:00Z";

function messageRead(id: string, fields: Record<string, unknown>): MessageRead {
  return {
    message: {
      author: { id: "planner", kind: "session" },
      body: "",
      created_at: "2026-01-01T00:00:00Z",
      deleted_at: null,
      deleted_by: null,
      deliveries: [],
      edited_at: null,
      id,
      in_reply_to: null,
      issue_key: "CORE-1",
      ...fields,
    },
    replies: [],
  } as unknown as MessageRead;
}

test("a reference to a deleted comment says it is deleted and never reads as an empty link", async () => {
  spies.push(
    spyOn(api, "getComment").mockResolvedValue({
      comment: {
        anchor: null,
        ask_id: null,
        author: { id: "alice", kind: "user" },
        body: "",
        created_at: "2026-01-01T00:00:00Z",
        deleted_at: DELETED_AT,
        deleted_by: { id: "alice", kind: "user" },
        deliveries: [],
        edited_at: null,
        id: DELETED_COMMENT,
        issue_key: "CORE-1",
        reply_to: null,
        resolved: false,
        resolved_at: null,
        resolved_by: null,
        suggestion: null,
        turn: null,
      },
      replies: [],
    } as unknown as CommentRead)
  );
  const view = render(
    withQueries(
      freshClient(),
      <MarkdownBody markdown={`see dispatch://CORE-1/comment/${DELETED_COMMENT}`} />
    )
  );

  try {
    const link = () => view.container.querySelector("a");
    await waitFor(() => expect(link()?.textContent ?? "").toContain("Deleted by alice"), {
      timeout: 2000,
    });
  } finally {
    view.unmount();
  }
});

test("a reference to a deleted message says it is deleted and never reads as an empty link", async () => {
  spies.push(
    spyOn(api, "getMessage").mockResolvedValue(
      messageRead(DELETED_MESSAGE, {
        deleted_at: DELETED_AT,
        deleted_by: { id: "alice", kind: "user" },
      })
    )
  );
  const view = render(
    withQueries(
      freshClient(),
      <MarkdownBody markdown={`see dispatch://CORE-1/message/${DELETED_MESSAGE}`} />
    )
  );

  try {
    const link = () => view.container.querySelector("a");
    await waitFor(() => expect(link()?.textContent ?? "").toContain("Deleted by alice"), {
      timeout: 2000,
    });
  } finally {
    view.unmount();
  }
});

test("a reference to an edited message marks it as edited, and one to a standing message does not", async () => {
  const standing = "8e3b5f74-af6d-4c9b-9d4e-5f6a7b8c9d0e";
  spies.push(
    spyOn(api, "getMessage").mockImplementation((_key: string, id: string) =>
      Promise.resolve(
        id === EDITED_MESSAGE
          ? messageRead(id, { body: "Shipped it, as corrected.", edited_at: DELETED_AT })
          : messageRead(id, { body: "Shipped it." })
      )
    )
  );
  const view = render(
    withQueries(
      freshClient(),
      <MarkdownBody
        markdown={`see dispatch://CORE-1/message/${EDITED_MESSAGE}\n\nand dispatch://CORE-1/message/${standing}`}
      />
    )
  );

  try {
    const paragraph = (id: string) =>
      view.container.querySelector(`a[data-dispatch-ref$="${id}"]`)?.closest("p")?.textContent ??
      "";
    await waitFor(() => expect(paragraph(EDITED_MESSAGE)).toContain("Shipped it, as corrected."));
    await waitFor(() => expect(paragraph(standing)).toContain("Shipped it."));
    expect(paragraph(EDITED_MESSAGE)).toContain("edited");
    expect(paragraph(standing)).not.toContain("edited");
  } finally {
    view.unmount();
  }
});

test("a reference whose target is missing reads as the words its author wrote", async () => {
  spies.push(
    spyOn(api, "getAsk").mockRejectedValue(
      new ApiError(404, { code: "NOT_FOUND", error: "no ask" })
    )
  );
  const written = `dispatch://CORE-1/ask/${ASK_ID}`;
  const view = render(withQueries(freshClient(), <MarkdownBody markdown={`see ${written}`} />));

  try {
    const link = await within(view.container).findByRole("link", { name: written });
    // Never the short form `CORE-1 ask`: a target that never resolves reads as the page's words.
    await waitFor(() => expect(link.textContent).toBe(written));
  } finally {
    view.unmount();
  }
});

test("a reference whose id is a prefix reads the item under the whole id, caching nothing under the prefix", async () => {
  const whole = `dispatch://CORE-1/ask/${ASK_ID}`;
  const getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: graphNode({ id: ASK_ID, kind: "ask", ref: whole }),
  });
  const getAsk = spyOn(api, "getAsk").mockResolvedValue(askRead(ASK_ID, "Ship it?"));
  spies.push(getReferences, getAsk);
  const queryClient = freshClient();
  const view = render(
    withQueries(queryClient, <MarkdownBody markdown="See dispatch://CORE-1/ask/2a7f1c90 here." />)
  );

  try {
    // The link appears once the prefix resolves, and the paint that draws it replaces the
    // element, so the anchor is read from the container each time rather than held.
    const link = () => view.container.querySelector("a");
    await waitFor(() => expect(link()?.textContent).toBe("Ship it?"));
    expect(link()?.getAttribute("data-dispatch-ref")).toBe(whole);
    expect(getReferences).toHaveBeenCalledWith("dispatch://CORE-1/ask/2a7f1c90");
    expect(getAsk).toHaveBeenCalledWith(ASK_ID);
    expect(getAsk).not.toHaveBeenCalledWith("2a7f1c90");
    // One cache entry per record, under the whole id: nothing is kept under the prefix.
    expect(queryClient.getQueryData(["ask", ASK_ID])).toBeDefined();
    expect(queryClient.getQueryData(["ask", "2a7f1c90"])).toBeUndefined();
  } finally {
    view.unmount();
  }
});
