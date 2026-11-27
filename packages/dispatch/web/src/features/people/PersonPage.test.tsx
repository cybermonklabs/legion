import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueReferenceSummary, PersonNode, PersonNodes } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ReferenceNamesProvider } from "../refs/reference-names";
import { PERSON_PATH } from "../refs/routes";
import { PersonPage } from "./PersonPage";

// The line draws through the shared renderer, whose Markdown engine loads lazily, and the rows
// run in random order, so the file loads the engine before its first row.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The issue the node's line names, as its summary read answers it. */
const named: IssueReferenceSummary = {
  documents: [],
  key: "CORE-9",
  project: "CORE",
  status: "in_progress",
  title: "Warm the cache",
};

/** One comment alice wrote, as the person nodes read sends it: the first line of its text, which
 *  holds a mark and a reference, under both its names (`first_line`, and `title`, the name a page
 *  of the release before reads), here with no tree of that line, so the page parses it. */
const node: PersonNode = {
  created_at: "2026-11-20T09:00:00Z",
  first_line: "Ask **dispatch://CORE-9** first.",
  href: "/issues/CORE-1/comments/comment-1",
  id: "comment-1",
  kind: "comment",
  relations: ["authored_by"],
  title: "Ask **dispatch://CORE-9** first.",
};

// The reading list's `person-node-title`: the line is drawn by the renderer, never as the
// characters its author typed. Its mark is drawn as a mark, so no `**` shows, and its reference
// reads as its target's key and title, a link to that issue.
test("a node's line is drawn through the renderer, its marks drawn and a reference read as its target", async () => {
  const listPersonNodes = spyOn(api, "listPersonNodes").mockResolvedValue({
    limit: 50,
    offset: 0,
    person: "alice",
    rows: [node],
    total: 1,
  });
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue(named);
  const view = render(
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      <MemoryRouter initialEntries={["/people/alice"]}>
        <ReferenceNamesProvider>
          <Routes>
            <Route element={<PersonPage />} path="/people/:login" />
          </Routes>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    const list = await view.findByRole("list", { name: "Person nodes" });
    const [row] = within(list).getAllByRole("listitem");
    if (row === undefined) throw new Error("the page drew no row for the node");
    await waitFor(() => expect(row.textContent).toContain("Ask CORE-9 Warm the cache first."));
    expect(row.textContent).not.toContain("**");
    expect(row.textContent).not.toContain("dispatch://");
    const reference = row.querySelector('a[href="/issues/CORE-9"]');
    expect(reference?.textContent).toBe("CORE-9 Warm the cache");
    expect(row.querySelector("strong")?.textContent).toBe("CORE-9 Warm the cache");
  } finally {
    view.unmount();
    getIssueSummary.mockRestore();
    listPersonNodes.mockRestore();
  }
});

// The line is read with its own issue as the text's source: a role reference reads as the role
// the issue its comment is on records, read by that issue.
test("a role the line's issue records reads as that role, read by the issue its comment is on", async () => {
  const listPersonNodes = spyOn(api, "listPersonNodes").mockResolvedValue({
    limit: 50,
    offset: 0,
    person: "alice",
    rows: [
      {
        ...node,
        first_line: "Ask dispatch://role/legion-p-core-1-reviewer first.",
        title: "Ask dispatch://role/legion-p-core-1-reviewer first.",
      },
    ],
    total: 1,
  });
  const collaborators = spyOn(api, "getIssueCollaborators").mockResolvedValue({
    agents: [
      {
        kind: "session",
        last_at: null,
        legion: {
          replaced_at: null,
          replaced_by: null,
          role: "reviewer",
          role_token: "legion-p-core-1-reviewer",
          started_at: "2026-10-26T02:00:00Z",
        },
        reasons: [],
        session_id: "a-reviewer",
        title: "A reviewer",
      },
    ],
    issue_key: "CORE-1",
    legion_registry: "ok",
    people: [],
  } as never);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["agents"], []);
  const view = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/people/alice"]}>
        <ReferenceNamesProvider>
          <Routes>
            <Route element={<PersonPage />} path="/people/:login" />
          </Routes>
        </ReferenceNamesProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    const list = await view.findByRole("list", { name: "Person nodes" });
    const [row] = within(list).getAllByRole("listitem");
    if (row === undefined) throw new Error("the page drew no row for the node");
    await waitFor(() => expect(row.textContent).toContain("Ask legion-p-core-1-reviewer first."));
    expect(collaborators.mock.calls.map((call) => call[0])).toContain("CORE-1");
  } finally {
    view.unmount();
    collaborators.mockRestore();
    listPersonNodes.mockRestore();
  }
});

/** One comment of the person's, by its id, with `overrides` over a standing comment's fields; its
 *  `title` is its first line, as the read answers the one line under both names. */
function commentNode(id: string, overrides: Partial<PersonNode> = {}): PersonNode {
  const row = {
    created_at: "2026-11-20T00:00:00Z",
    first_line: "Agreed.",
    href: `/issues/CORE-1/comments/${id}`,
    id,
    kind: "comment",
    relations: ["authored_by"],
    ...overrides,
  };
  return { title: row.first_line, ...row };
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
    commentNode("comment-edited", {
      edited_at: "2026-11-20T00:30:00Z",
      first_line: "Ship it on Thursday.",
    }),
    commentNode("comment-standing", { first_line: "Ship it on Friday." }),
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
  // With the engine loaded a first line paints in the same pass as the deleted line, so a first
  // line the row drew would be on the page when the check below first passes.
  await warmMarkdownRenderer();
  const page = renderPage([
    commentNode("comment-deleted", {
      deleted_at: "2026-11-20T01:00:00Z",
      deleted_by: { id: "alice", kind: "user" },
      // An edit before the delete: the deleted line says what became of the text, and no edited
      // mark stands beside it.
      edited_at: "2026-11-20T00:30:00Z",
      // The first line stays beside the marks, so only the row's own reading of the marks keeps
      // it off the page.
      first_line: "Hold the deploy until Monday.",
    }),
  ]);

  try {
    await waitFor(() => {
      const row = page.row("comment-deleted");
      expect(row?.textContent).toContain("Deleted by alice");
      expect(row?.textContent).not.toContain("Hold the deploy until Monday.");
    });
    const row = page.row("comment-deleted");
    // The words are the deleted line's own (`DeletedBody`), in the first line's place.
    expect(row?.querySelector("[data-post-deleted]")?.textContent ?? "").toContain(
      "Deleted by alice"
    );
    expect(row?.textContent).not.toContain("edited");
    expect(row?.textContent).toContain("Open");
  } finally {
    page.restore();
  }
});

// A server of the release before answers each row's line as `title` alone, with no `first_line`
// and no tree of it, and a roll or a rollback leaves such a pod serving this page: the page draws
// that line all the same.
test("a row a server of the release before answers, with its line only as title, draws that line", async () => {
  await warmMarkdownRenderer();
  const { first_line: _absent, ...older } = commentNode("comment-older", {
    first_line: "Ship it on **Monday**.",
  });
  // The client's type is this build's answer, which always carries first_line; the older pod's
  // row is that answer without it.
  const page = renderPage([older as PersonNode]);

  try {
    await waitFor(() =>
      expect(page.row("comment-older")?.textContent).toContain("Ship it on Monday.")
    );
    expect(page.row("comment-older")?.textContent).not.toContain("**");
    expect(page.row("comment-older")?.querySelector("strong")?.textContent).toBe("Monday");
  } finally {
    page.restore();
  }
});
