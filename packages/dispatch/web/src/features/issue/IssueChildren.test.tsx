import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { IssueChild, IssueDetails } from "../../api/types";
import { IssueChildren } from "./IssueChildren";

/**
 * The children section against an answer an older Dispatch wrote. Two builds answer one load
 * balancer during a rollout (`e2e/two-builds.multiprocess.e2e.ts`), so this page can read
 * `GET /api/v1/issues/{key}` from a build whose `IssueChild` carries no `claim` at all. The
 * section opens by default from Tailwind's `md` up, so a field read as `null` alone would put
 * every issue with children into the page's error boundary for the length of a rollout.
 */

const child: IssueChild = {
  active_at: "2026-11-05T00:00:00Z",
  blocked_by: [],
  claim: null,
  external_links: [],
  key: "CORE-2",
  status: "in_progress",
  subtree_done: 0,
  subtree_total: 1,
  title: "Draw the children section",
};

function issueWith(children: readonly IssueChild[]): IssueDetails {
  return {
    artifacts: [],
    assignee: null,
    blocked_by: [],
    children: [...children],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    decisions_artifact_id: "",
    design_gates: [],
    external_links: [],
    holder_state: null,
    key: "CORE-1",
    labels: [],
    last_seen: null,
    last_seq: 1,
    number: 1,
    open_asks: [],
    parent: null,
    plan_artifact_id: "",
    primary_artifact_id: "",
    priority: null,
    progress: { children: null, tasks: null },
    project: "CORE",
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "todo",
    takeable_at: null,
    title: "The issue page draws one ordered body",
    updated_at: "2026-11-05T00:00:00Z",
    verification_artifact_id: "",
  };
}

function drawChildren(children: readonly IssueChild[]): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // The section's open state is a preference of the signed-in identity, which it reads from
  // this cache rather than from the network (`useChildrenSection`).
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={client}>
        <IssueChildren issue={issueWith(children)} />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("a child an older build answered with no claim at all draws its row", () => {
  // The field the older build never wrote: `claim` is absent, not null.
  const { claim: _claim, ...older } = child;
  drawChildren([older as IssueChild]);
  expect(screen.getByText(/Draw the children section/)).toBeTruthy();
});

test("a child holding a claim names who holds it", () => {
  drawChildren([
    { ...child, claim: { actor: { id: "alice", kind: "user" }, at: "2026-11-05T01:00:00Z" } },
  ]);
  expect(screen.getByText("Claimed by")).toBeTruthy();
});

// Two keys drawn with nothing between them read as one key: "Blocked by CORE-7CORE-8" names no
// issue a reader can look up.
test("a child waiting on two issues reads them as two keys", () => {
  drawChildren([{ ...child, blocked_by: ["CORE-7", "CORE-8"] }]);
  const line = screen.getByTestId("issue-child-blocked-by");
  expect(line.textContent).toBe("Blocked by CORE-7, CORE-8");
  expect(
    within(line)
      .getAllByRole("link")
      .map((link) => link.textContent)
  ).toEqual(["CORE-7", "CORE-8"]);
});
