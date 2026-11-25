import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, IssueReferences } from "../../api/types";
import { IssueReferenceClosure } from "./IssueReferenceClosure";
import { SHOW_MORE, showMoreName } from "./show-more-fixture";

/** The issue's reference closure, which the Artifacts list drew at its head and an issue's
 *  artifact draws on its own page now (dispatch://INFRA-1196 R3). */

const notes = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-notes",
  issue_key: null,
  kind: "doc",
  name: "Design notes",
  primary: false,
  project: "CORE",
  ref_key: "CORE/design-notes",
  session_id: null,
  slug: "design-notes",
  versions: [],
} satisfies Artifact;

function renderClosure(references: IssueReferences) {
  const getIssueReferences = spyOn(api, "getIssueReferences").mockResolvedValue(references);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <IssueReferenceClosure issueKey="CORE-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getIssueReferences, view };
}

// Departure 6: the case was "...names truncation, and starts collapsed"; R2 draws every list
// beside an opened document open, so its members are drawn with no press.
test("the closure lists its members with their owner, via and depth, names truncation, and is drawn open, 20 members at a time", async () => {
  const { getIssueReferences, view } = renderClosure({
    linked_from: [],
    links: [],
    members: [
      {
        artifact: notes,
        depth: 1,
        via: { id: "comment-1", kind: "comment" },
      },
    ],
    truncated: true,
  });
  try {
    const references = await screen.findByRole("region", { name: "References" });
    expect(view.container.querySelector("details")).toBeNull();
    expect(within(references).getByRole("heading")).toHaveTextContent("References (1)");
    const link = within(references).getByRole("link", { name: "Design notes" });
    expect(link).toBeVisible();
    expect(link).toHaveAttribute("href", "/projects/CORE/documents/design-notes");
    expect(references).toHaveTextContent("CORE · via comment · depth 1");
    expect(references).toHaveTextContent("more references beyond 8 hops");
    expect(within(references).queryByRole("button", { name: SHOW_MORE })).toBeNull();
  } finally {
    view.unmount();
    getIssueReferences.mockRestore();
  }
});

/** 25 documents the issue's references reach, each of its own name. */
const twentyFive = Array.from({ length: 25 }, (_, index) => {
  const number = String(index + 1).padStart(2, "0");
  return {
    artifact: {
      ...notes,
      id: `artifact-note-${number}`,
      name: `Note ${number}`,
      ref_key: `CORE/note-${number}`,
      slug: `note-${number}`,
    },
    depth: 1,
    via: { id: "comment-1", kind: "comment" },
  } satisfies NonNullable<IssueReferences["members"]>[number];
});

// Root R40: the closure is a list of unbounded length, drawn as Documents' list is: its first 20
// members and, under them, the control that says how many are left, whose press draws the rest
// and puts the focus on the first member it drew.
test("the closure draws 20 members and a control that names how many are left", async () => {
  const user = userEvent.setup();
  const { getIssueReferences, view } = renderClosure({
    linked_from: [],
    links: [],
    members: twentyFive,
    truncated: false,
  });
  try {
    const references = await screen.findByRole("region", { name: "References" });
    expect(within(references).getByRole("heading")).toHaveTextContent("References (25)");
    expect(within(references).getAllByRole("link", { name: /^Note/ })).toHaveLength(20);
    const more = within(references).getByRole("button", { name: SHOW_MORE });
    expect(more.textContent).toBe(showMoreName(5));
    await user.click(more);
    expect(within(references).getAllByRole("link", { name: /^Note/ })).toHaveLength(25);
    await waitFor(() =>
      expect(document.activeElement).toBe(within(references).getByRole("link", { name: "Note 21" }))
    );
    expect(within(references).queryByRole("button", { name: SHOW_MORE })).toBeNull();
  } finally {
    view.unmount();
    getIssueReferences.mockRestore();
  }
});

// A conversation's picture is owned by the agent whose conversation holds it: the row names that
// agent through `AgentName`, a link to its live conversation.
test("the closure names the agent that owns a conversation's artifact", async () => {
  const listAgents = spyOn(api, "listAgents").mockResolvedValue([
    {
      capabilities: [],
      dir: "/w",
      host: "host",
      last_activity: null,
      last_seen: 1,
      machine_id: "host",
      open_asks: 0,
      roles: [],
      session_id: "planner-session",
      state: "live",
      title: "Planner",
    },
  ]);
  const picture = {
    ...notes,
    id: "artifact-shot",
    kind: "image",
    name: "shot.png",
    project: "",
    session_id: "planner-session",
    slug: "shot-png",
  } satisfies Artifact;
  const { getIssueReferences, view } = renderClosure({
    linked_from: [],
    links: [],
    members: [{ artifact: picture, depth: 1, via: { id: "comment-1", kind: "comment" } }],
    truncated: false,
  });
  try {
    const references = await screen.findByRole("region", { hidden: true, name: "References" });
    const owner = await within(references).findByRole("link", { hidden: true, name: "Planner" });
    expect(owner).toHaveAttribute("href", "/agents/planner-session/live");
    expect(references).toHaveTextContent("agent Planner · via comment · depth 1");
  } finally {
    view.unmount();
    getIssueReferences.mockRestore();
    listAgents.mockRestore();
  }
});

test("an issue whose artifacts reference nothing draws no closure", async () => {
  const { getIssueReferences, view } = renderClosure({
    linked_from: [],
    links: [],
    members: [],
    truncated: false,
  });
  try {
    await expect(screen.findByText(/^References/, {}, { timeout: 200 })).rejects.toThrow();
  } finally {
    view.unmount();
    getIssueReferences.mockRestore();
  }
});
