import { afterEach, beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { ApiError, api } from "../../api/client";
import type { AskRead, IssueReferenceSummary, MessageRead } from "../../api/types";
import { MarkdownBody } from "./MarkdownBody";
import { warmMarkdownRenderer } from "./markdown-engine";
import { PlainText } from "./plain-text";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

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

// An issue reference reads as its status mark and its key, on one line; its title is in the
// accessible name the element it renders into carries, after the key and the status's words.
test("an issue reference reads as its status mark and its key, its title in its name", async () => {
  const queryClient = freshClient();
  queryClient.setQueryData(["issue-summary", "CORE-1"], summary);
  const view = render(
    withQueries(queryClient, <MarkdownBody markdown="Tied to dispatch://CORE-1 here." />)
  );

  try {
    const link = await within(view.container).findByRole("link", {
      name: "CORE-1, In progress, Fix the thing",
    });
    expect(link.getAttribute("href")).toBe("/issues/CORE-1");
    const mark = link.querySelector("svg[data-status-glyph]");
    expect(mark).toHaveAttribute("aria-label", "In progress");
    expect(mark?.parentElement).toHaveClass("whitespace-nowrap");
    expect(link.textContent?.trim()).toBe("CORE-1");
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

/** The issue summary reads a body draws `markdown` with - painted, or as its plain-text
 *  projection - once its one real key has drawn. */
async function summaryReads(
  markdown: string,
  projects: readonly string[] | undefined,
  drawn: "painted" | "projected" = "painted"
): Promise<string[]> {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockImplementation((key: string) =>
    key === "CORE-1"
      ? Promise.resolve(summary)
      : Promise.reject(new ApiError(404, { code: "NOT_FOUND", error: `no issue ${key}` }))
  );
  const queryClient = freshClient();
  if (projects !== undefined) {
    queryClient.setQueryData(
      ["projects"],
      projects.map((key) => ({ key, name: key }))
    );
  }
  const view = render(
    withQueries(
      queryClient,
      drawn === "painted" ? (
        <MarkdownBody markdown={markdown} />
      ) : (
        <PlainText markdown={markdown}>
          {(text) => <span data-testid="line">{text ?? ""}</span>}
        </PlainText>
      )
    )
  );
  try {
    // Every key the text writes is asked in the one batch its first reading starts, so once the
    // real one has been read, every read the text makes has been made. The painted link names
    // it by its key, its status and its title; the projection writes the key alone, as the text
    // already wrote it, so it is the summary's read that says the real one has been read.
    if (drawn === "painted") {
      await within(view.container).findByRole("link", {
        name: "CORE-1, In progress, Fix the thing",
      });
    } else {
      await waitFor(() =>
        expect(queryClient.getQueryState(["issue-summary", "CORE-1"])?.status).toBe("success")
      );
      expect(view.getByTestId("line").textContent).toBe("Encoded as UTF-16, tracked in CORE-1.");
    }
    return getIssueSummary.mock.calls.map(([key]) => key);
  } finally {
    view.unmount();
    getIssueSummary.mockRestore();
  }
}

// A word of prose shaped like an issue key - `UTF-16`, `SHA-256` - is a key only where its
// prefix is a project. Where the page already holds its project list (the sidebar reads it),
// no such word costs a read; where it holds none yet, every key-shaped word is asked, as before.
test("a key-shaped word whose prefix is no project asks nothing where the page holds its projects", async () => {
  const markdown = "Encoded as UTF-16, tracked in CORE-1.";
  expect(await summaryReads(markdown, ["CORE"])).toEqual(["CORE-1"]);
  expect(await summaryReads(markdown, undefined)).toEqual(["UTF-16", "CORE-1"]);
});

// The plain-text projection keeps the same rule (`project-keys.ts`): the label and the line
// beside it ask the same reads.
test("a key-shaped word whose prefix is no project costs the projection no read where the page holds its projects", async () => {
  const markdown = "Encoded as UTF-16, tracked in CORE-1.";
  expect(await summaryReads(markdown, ["CORE"], "projected")).toEqual(["CORE-1"]);
  expect(await summaryReads(markdown, undefined, "projected")).toEqual(["UTF-16", "CORE-1"]);
});
