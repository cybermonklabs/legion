import { afterEach, beforeAll, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { fakeDocumentRuntime } from "../../__tests__/document-runtime";
import { realClock, tick } from "../../__tests__/fake-clock";
import { api } from "../../api/client";
import type { IssueDetails, MyReplyThreadRow } from "../../api/types";
import { DocumentRuntime } from "../doc/runtime";
import { MarginProvider } from "../margin/margin-context";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { installInboxApiMocks } from "./inbox-fixture";
import { ThreadPane } from "./ThreadPane";

/**
 * The reading pane over a comment on document text whose anchor it cannot place
 * (the in-document pane, R3): it draws the thread with its quote and the document
 * page's sentence that the text changed, and reads no document.
 */

// The thread's subject draws through the shared renderer, whose Markdown engine loads lazily,
// and the rows run in random order, so the file loads the engine before its first row.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

installInboxApiMocks();

/** How long the pane waits for the selection to stand still (`DOCUMENT_SETTLE_MS`). */
const SETTLE_MS = 200;

let getArtifact: Mock<typeof api.getArtifact>;
let getIssue: Mock<typeof api.getIssue>;
let listComments: Mock<typeof api.listComments>;

beforeEach(() => {
  getArtifact = spyOn(api, "getArtifact").mockRejectedValue(new Error("no document is read"));
  // The thread's own body reads its issue and the issue's comments, as the Conversation does.
  getIssue = spyOn(api, "getIssue").mockResolvedValue({
    closed_at: null,
  } as unknown as IssueDetails);
  listComments = spyOn(api, "listComments").mockResolvedValue([]);
});

afterEach(() => {
  getArtifact.mockRestore();
  getIssue.mockRestore();
  listComments.mockRestore();
});

test("an anchor that names no block draws the quote and the changed-text sentence", async () => {
  // On the fake clock from before the first render, so the settle the pane would wait out
  // before a document read is a timer this row moves the clock past.
  jest.useFakeTimers();
  const thread: MyReplyThreadRow = {
    // A comment on issue CORE-1's spec whose anchor names no block of it.
    anchor: {
      artifact_id: "artifact-1",
      block_id: null,
      mark_id: "mark-1",
      orphaned: false,
      quote: "three attempts",
      version: 1,
    },
    issue: { key: "CORE-1", title: "Retry budget" },
    kind: "comment",
    read_through: "2026-10-12T00:30:00Z",
    ref: "/issues/CORE-1/comments/comment-1",
    reply: {
      author: { id: "bob", kind: "user" },
      created_at: "2026-10-12T01:00:00Z",
      first_line: "Agreed.",
      id: "comment-2",
      kind: "reply",
    },
    subject: "The retry budget",
    thread_id: "comment-1",
    unread: true,
    unread_count: 1,
    version: 1,
  };
  const fake = fakeDocumentRuntime({ text: "The live document" });
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { event_head: "0", kind: "user", login: "alice" });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DocumentRuntime.Provider value={fake.runtime}>
          <MarginProvider>
            <ThreadPane thread={thread} />
          </MarginProvider>
        </DocumentRuntime.Provider>
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    await tick(SETTLE_MS * 2);
    const pane = screen.getByTestId("inbox-pane");
    const body = pane.querySelector<HTMLElement>('[data-inbox-pane-body="document"]');
    if (body === null) throw new Error("the pane drew no document body");
    expect(body.hasAttribute("data-inbox-pane-fallback")).toBe(true);
    // The quote, as a card draws it, and the document page's words for text that changed.
    expect(body.querySelector("[data-inbox-thread-quote]")?.textContent).toBe("three attempts");
    expect(body).toHaveTextContent(
      "Text changed. The selected range no longer exists in this document."
    );
    // Nothing read the document, and nothing opened it.
    expect(getArtifact).not.toHaveBeenCalled();
    expect(fake.connections).toHaveLength(0);
  } finally {
    realClock();
    view.unmount();
  }
});
