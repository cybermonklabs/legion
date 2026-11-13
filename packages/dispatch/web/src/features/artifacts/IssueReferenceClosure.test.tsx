import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Artifact, IssueReferences } from "../../api/types";
import { IssueReferenceClosure } from "./IssueReferenceClosure";

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

test("the closure lists its members with their owner, via and depth, names truncation, and starts collapsed", async () => {
  const { getIssueReferences, view } = renderClosure({
    members: [
      {
        artifact: notes,
        depth: 1,
        via: { id: "comment-1", kind: "comment" },
        via_blocks: ["b1"],
      },
    ],
    truncated: true,
  });
  try {
    const summary = await screen.findByText("References (1)");
    const details = summary.closest("details");
    expect(details).toBeInstanceOf(HTMLDetailsElement);
    expect((details as HTMLDetailsElement).open).toBe(false);

    const references = within(view.container).getByRole("region", {
      hidden: true,
      name: "References",
    });
    expect(
      within(references).getByRole("link", { hidden: true, name: "Design notes" })
    ).toHaveAttribute("href", "/projects/CORE/documents/design-notes");
    expect(references).toHaveTextContent("CORE · via comment · depth 1");
    expect(references).toHaveTextContent("more references beyond 8 hops");
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
    members: [
      { artifact: picture, depth: 1, via: { id: "comment-1", kind: "comment" }, via_blocks: null },
    ],
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
  const { getIssueReferences, view } = renderClosure({ members: [], truncated: false });
  try {
    await expect(screen.findByText(/^References/, {}, { timeout: 200 })).rejects.toThrow();
  } finally {
    view.unmount();
    getIssueReferences.mockRestore();
  }
});
