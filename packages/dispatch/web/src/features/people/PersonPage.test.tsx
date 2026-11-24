import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { PersonNode, PersonNodes } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { PERSON_PATH } from "../refs/routes";
import { PersonPage } from "./PersonPage";

function node(id: string, overrides: Partial<PersonNode> = {}): PersonNode {
  return {
    created_at: "2026-11-20T00:00:00Z",
    href: `/issues/CORE-1/comments/${id}`,
    id,
    kind: "comment",
    relations: ["authored_by"],
    title: "Agreed.",
    ...overrides,
  };
}

function renderPage(rows: PersonNode[]) {
  const answer: PersonNodes = { limit: 50, offset: 0, person: "alice", rows, total: rows.length };
  const listPersonNodes = spyOn(api, "listPersonNodes").mockResolvedValue(answer);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter initialEntries={["/people/alice"]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route element={<PersonPage />} path={PERSON_PATH} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  /** The row of the node `id`, by the path its Open link goes to. */
  const row = (id: string) =>
    view.container
      .querySelector(`ul[aria-label="Person nodes"] a[href="/issues/CORE-1/comments/${id}"]`)
      ?.closest("li");
  return {
    restore: () => {
      view.unmount();
      listPersonNodes.mockRestore();
    },
    row,
  };
}

test("a row whose comment its author edited says so after its kind, and one nobody edited does not", async () => {
  await warmMarkdownRenderer();
  const page = renderPage([
    node("comment-edited", { edited_at: "2026-11-20T00:30:00Z", title: "Ship it on Thursday." }),
    node("comment-standing", { title: "Ship it on Friday." }),
  ]);

  try {
    await waitFor(() => {
      expect(page.row("comment-edited")?.textContent).toContain("comment · edited");
      expect(page.row("comment-edited")?.textContent).toContain("Ship it on Thursday.");
      expect(page.row("comment-standing")?.textContent).toContain("Ship it on Friday.");
    });
    expect(page.row("comment-standing")?.textContent).not.toContain("edited");
  } finally {
    page.restore();
  }
});

test("a row whose comment its author deleted keeps its place as the deleted line, with none of its old words", async () => {
  // With the engine loaded a title paints in the same pass as the deleted line, so a title the
  // row drew would be on the page when the check below first passes.
  await warmMarkdownRenderer();
  const page = renderPage([
    node("comment-deleted", {
      deleted_at: "2026-11-20T01:00:00Z",
      deleted_by: { id: "alice", kind: "user" },
      // An edit before the delete: the deleted line says what became of the text, and no edited
      // mark stands beside it.
      edited_at: "2026-11-20T00:30:00Z",
      // The title stays beside the marks, so only the row's own reading of the marks keeps it
      // off the page.
      title: "Hold the deploy until Monday.",
    }),
  ]);

  try {
    await waitFor(() => {
      const row = page.row("comment-deleted");
      expect(row?.textContent).toContain("Deleted by alice");
      expect(row?.textContent).not.toContain("Hold the deploy until Monday.");
    });
    const row = page.row("comment-deleted");
    // The words are the deleted line's own (`DeletedBody`), in the title's place.
    expect(row?.querySelector("[data-post-deleted]")?.textContent ?? "").toContain(
      "Deleted by alice"
    );
    expect(row?.textContent).not.toContain("edited");
    expect(row?.textContent).toContain("Open");
  } finally {
    page.restore();
  }
});
