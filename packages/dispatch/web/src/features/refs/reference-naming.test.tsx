import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import type { IssueReferenceSummary, MyReplyRow } from "../../api/types";
import { ThreadTitle } from "../inbox/ThreadTitle";
import { MarkdownBody } from "./MarkdownBody";
import { PlainText } from "./plain-text";
import { ReferenceHostIs } from "./reference-host";

/**
 * The one naming rule and the four host shapes, read on a painted body and on the plain-text
 * projection beside it. Every row here names CORE-1, whose summary the cache already holds, so
 * what is being read is how the rule draws a resolved reference - never whether it resolves.
 */

const summary: IssueReferenceSummary = {
  documents: [],
  key: "CORE-1",
  project: "CORE",
  status: "in_progress",
  title: "Fix the thing",
};

/** A query client holding CORE-1's summary, so every reference below resolves at once. */
function clientWithIssue(): QueryClient {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["issue-summary", "CORE-1"], summary);
  return client;
}

function draw(children: ReactNode, client: QueryClient = clientWithIssue()) {
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>{children}</MemoryRouter>
    </QueryClientProvider>
  );
}

function words(element: HTMLElement): string {
  return (element.textContent ?? "").replace(/\s+/g, " ").trim();
}

// R8's first case: an address standing on its own reads as the target's current name.
test("an address standing on its own reads as the target's current name", async () => {
  const view = draw(<MarkdownBody markdown="Tied to dispatch://CORE-1 here." />);
  try {
    const link = await within(view.container).findByRole("link", { name: "CORE-1 Fix the thing" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1");
  } finally {
    view.unmount();
  }
});

// R8's second case: a link whose words are its own address - the form a document stores a
// Markdown autolink in - reads as the title too, as the bare address does.
test("a link whose words are its own address reads as the target's current name", async () => {
  const view = draw(<MarkdownBody markdown="See [dispatch://CORE-1](dispatch://CORE-1)." />);
  try {
    const link = await within(view.container).findByRole("link", { name: "CORE-1 Fix the thing" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1");
  } finally {
    view.unmount();
  }
});

// R8's third case, the only one that changes: a link whose words differ from its address keeps
// those words. The link still works and still carries the reference, so its hover card is the
// same card; only the words are the author's.
test("a link whose words its author wrote keeps those words", async () => {
  const view = draw(<MarkdownBody markdown="See [the rollout plan](dispatch://CORE-1) first." />);
  try {
    const link = await within(view.container).findByRole("link", { name: "the rollout plan" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1");
    expect(link).toHaveAttribute("data-dispatch-ref", "dispatch://CORE-1");
    await waitFor(() => expect(words(view.container)).toBe("See the rollout plan first."));
  } finally {
    view.unmount();
  }
});

// The plain-text projection reads the same reference the painted line reads, and names it the
// same way: a label and the line beside it never say two different things.
test("the plain-text projection names a reference by its resolved name", async () => {
  const view = draw(
    <PlainText markdown="Tied to **dispatch://CORE-1** here.">
      {(text) => <span data-testid="line">{text ?? ""}</span>}
    </PlainText>
  );
  try {
    await waitFor(() =>
      expect(view.getByTestId("line").textContent).toBe("Tied to CORE-1 Fix the thing here.")
    );
  } finally {
    view.unmount();
  }
});

// Shape 2: inside a link or a button there is no link of its own. The words and the reference
// stay - the hover card is still the card - and the host is what a click follows.
test("a reference inside a link or a button is drawn inert", async () => {
  const view = draw(
    <ReferenceHostIs host="control">
      <MarkdownBody markdown="Tied to dispatch://CORE-1 here." />
    </ReferenceHostIs>
  );
  try {
    await waitFor(() => expect(words(view.container)).toBe("Tied to CORE-1 Fix the thing here."));
    expect(view.container.querySelector("a")).toBeNull();
    const inert = view.container.querySelector("[data-markdown-link]");
    expect(inert).toHaveAttribute("data-dispatch-ref", "dispatch://CORE-1");
  } finally {
    view.unmount();
  }
});

// Shape 4: a suggestion's replacement shows the exact characters it swaps, so no title is drawn
// over a reference there - the reader has to see what accepting it would write.
test("a reference in a diff is drawn as its characters", async () => {
  const view = draw(
    <ReferenceHostIs host="diff">
      <MarkdownBody markdown="Tied to **dispatch://CORE-1** here." variant="inline" />
    </ReferenceHostIs>
  );
  try {
    expect(words(view.container)).toBe("Tied to **dispatch://CORE-1** here.");
    expect(view.container.querySelector("a")).toBeNull();
    expect(view.container.querySelector("strong")).toBeNull();
  } finally {
    view.unmount();
  }
});

const threadRow = {
  kind: "comment",
  reply: { author: { id: "planner", kind: "session" }, created_at: "2026-01-01T00:00:00Z" },
  ref: "dispatch://CORE-1/comment/comment-1",
  subject: "## Ship **behind a flag**, or wait?",
  thread_id: "comment-1",
  unread: false,
  unread_count: 0,
} as unknown as MyReplyRow;

// The Inbox's own line is drawn by the renderer, not stripped by a regular expression: the
// heading's words format as the thread formats them, and no syntax is left standing.
test("an Inbox thread title draws its line through the renderer", async () => {
  const view = draw(<ThreadTitle className="text-sm" thread={threadRow} />);
  try {
    await waitFor(() =>
      expect(view.container.querySelector("strong")?.textContent).toBe("behind a flag")
    );
    expect(words(view.container)).toBe("Ship behind a flag, or wait?");
    // One line, never the heading's own block.
    expect(view.container.querySelector("h1, h2, h3, p, ul")).toBeNull();
  } finally {
    view.unmount();
  }
});
