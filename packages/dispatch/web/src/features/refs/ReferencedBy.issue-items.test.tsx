import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { graphNode } from "../../__tests__/graph-fixture";
import { api } from "../../api/client";
import type { GraphEdge, GraphReferences } from "../../api/types";
import { ReferencedBy } from "./ReferencedBy";

// The new page's list of what references a node leaves out what the issue's own asks, comments
// and messages cite of it, since that page's outline is the issue's conversation (`page`). A
// document opened in place of the outline stands beside no outline, so its list keeps them
// (`keepIssueItems`, dispatch://INFRA-1196 part N's R2); without that prop the page's rule is
// today's.

const DOCUMENT = "dispatch://CORE-1/artifact/notes-md";
const OWN_COMMENT = "11111111-1111-4111-8111-111111111111";
const OTHER_COMMENT = "44444444-4444-4444-8444-444444444444";
const OWN_WORDS = "CORE-1's own comment cites the notes.";
const OTHER_WORDS = "CORE-2's comment cites the notes.";

/** A comment of `issueKey` whose text cites the notes document. */
function commentEdge(issueKey: string, id: string, text: string): GraphEdge {
  return {
    created_at: "2026-11-16T10:00:00Z",
    direction: "in",
    excerpt: { text },
    kind: "mentions",
    node: graphNode({
      id,
      issue_key: issueKey,
      kind: "comment",
      project: "CORE",
      ref: `dispatch://${issueKey}/comment/${id}`,
    }),
    source_seq: 7,
  };
}

const READ: GraphReferences = {
  edges: [
    commentEdge("CORE-1", OWN_COMMENT, OWN_WORDS),
    commentEdge("CORE-2", OTHER_COMMENT, OTHER_WORDS),
  ],
  node: graphNode({ id: "notes-md", issue_key: "CORE-1", kind: "artifact", ref: DOCUMENT }),
};

test("with page, keepIssueItems keeps the issue's own comment, and without it the page's rule leaves the comment out as before", async () => {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(READ);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const panel = (keepIssueItems: boolean | undefined) => (
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ReferencedBy keepIssueItems={keepIssueItems} page reference={DOCUMENT} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(panel(true));
  try {
    const kept = await screen.findByRole("region", { name: "Referenced by" });
    expect(
      await within(kept).findByText(OWN_WORDS),
      "the list that keeps the issue's own items leaves out its own comment"
    ).toBeInTheDocument();
    expect(within(kept).getByRole("link", { name: "Comment · CORE-1" })).toHaveAttribute(
      "href",
      `/issues/CORE-1/comments/${OWN_COMMENT}`
    );
    expect(within(kept).getByText(OTHER_WORDS)).toBeInTheDocument();
    expect(within(kept).getByRole("heading")).toHaveTextContent("Referenced by (2)");
    // Its quotes are what their sources wrote, so the page reads them as no words of its own.
    expect(within(kept).getByText(OWN_WORDS)).toHaveAttribute("data-author-text", "");

    view.rerender(panel(undefined));
    const byRule = screen.getByRole("region", { name: "Referenced by" });
    await waitFor(() =>
      expect(within(byRule).getByRole("heading")).toHaveTextContent("Referenced by (1)")
    );
    expect(
      within(byRule).queryByText(OWN_WORDS),
      "the page's list lists the issue's own comment with keepIssueItems left out"
    ).toBeNull();
    expect(within(byRule).getByText(OTHER_WORDS)).toBeInTheDocument();
    expect(getReferences).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
    client.clear();
    getReferences.mockRestore();
  }
});

test("off the new page the list draws no author mark on its quotes, as before", async () => {
  const getReferences = spyOn(api, "getReferences").mockResolvedValue(READ);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <ReferencedBy reference={DOCUMENT} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const list = await screen.findByRole("region", { name: "Referenced by" });
    expect(await within(list).findByText(OWN_WORDS)).not.toHaveAttribute("data-author-text");
    expect(list.querySelectorAll("[data-author-text]")).toHaveLength(0);
  } finally {
    view.unmount();
    client.clear();
    getReferences.mockRestore();
  }
});
