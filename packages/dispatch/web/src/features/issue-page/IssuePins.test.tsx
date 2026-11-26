import { beforeAll, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { displayTree } from "../../__tests__/display-trees";
import { issueAskThreadsQuery } from "../../api/queries";
import type { CommentWithTrees } from "../../api/types";
import { conversationItemId, pinnedItemMarker } from "../issue/pins";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { IssuePins } from "./IssuePins";
import { CONTROL_LABELS } from "./page-words";
import { issueCommentsQuery, issueMessagesQuery } from "./useIssueTree";

/** The reader's Pinned list (`IssuePins`), drawn over the reads it shares with the outline. */

const KEY = "CORE-1";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

// A pin's line is read as its row in the outline reads it: from the first block of the server's
// reading of its record, so a pinned block quote of two paragraphs is drawn by the first.
test("a pinned comment that quotes two paragraphs is drawn by the first paragraph's words", () => {
  const id = "4a7c2e19-8b3d-4f60-a5e1-6c9d0b2f3e47";
  const body = "> Yes\n>\n> Keep the old page.";
  const comment: CommentWithTrees = {
    ...commentDeliveryFields(),
    anchor: null,
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    body_tree: displayTree(body),
    created_at: "2026-11-05T01:00:00Z",
    edited_at: null,
    id,
    issue_key: KEY,
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
  };
  // Every read the list shares with the outline holds its answer, fresh until an event says
  // otherwise, so none is asked for again.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(issueAskThreadsQuery(KEY).queryKey, []);
  client.setQueryData(issueCommentsQuery(KEY).queryKey, [comment]);
  client.setQueryData(issueMessagesQuery(KEY).queryKey, []);
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <IssuePins
          dismissed={[pinnedItemMarker(conversationItemId("comment", id))]}
          issueKey={KEY}
        />
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    const pins = view.getByRole("region", { name: CONTROL_LABELS.pinned });
    expect(
      within(pins).getByRole("link").textContent,
      "the pin is not drawn by its quote's first paragraph"
    ).toBe("Yes");
  } finally {
    view.unmount();
  }
});
