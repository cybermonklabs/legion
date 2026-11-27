import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { IssueChild } from "../../api/types";
import { ChildIssueFacts, ChildIssueTag } from "./IssueChildren";
import { NoTooltips } from "./NoTooltips";
import { CONTROL_LABELS } from "./page-words";

/**
 * What a child issue's row of the outline draws beside its title (the architect's ruling 5 on
 * R12): how it stands, what blocks it and who holds it, and its key as the tag. Two builds answer
 * one load balancer during a rollout (`e2e/two-builds.multiprocess.e2e.ts`), so this page can read
 * `GET /api/v1/issues/{key}` from a build whose `IssueChild` carries no `claim` at all, and a field
 * read as `null` alone would put every issue with children into the page's error boundary.
 */

/** A child that waits on nothing and that nobody holds: the read leaves `needs` and `claim` out. */
const child: IssueChild = {
  active_at: "2026-11-05T00:00:00Z",
  external_links: [],
  key: "CORE-2",
  status: "in_progress",
  subtree_done: 0,
  subtree_total: 1,
  title: "Draw the children as rows",
};

function drawChild(one: IssueChild): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  // The registry names no session, so a holder is named by the title its claim stamped.
  client.setQueryData(["agents"], []);
  render(
    <MemoryRouter initialEntries={["/issues/CORE-1"]}>
      <QueryClientProvider client={client}>
        <NoTooltips>
          <ChildIssueFacts child={one} />
          <ChildIssueTag child={one} />
        </NoTooltips>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

test("a child that waits on nothing and that nobody holds says how it stands, and no more", () => {
  drawChild(child);
  expect(screen.getByText("In progress")).toBeTruthy();
  expect(screen.queryByText("Claimed by")).toBeNull();
  expect(screen.queryByTestId("issue-child-blocked-by")).toBeNull();
});

// A Dispatch older than this contract answers a child nobody holds with `claim: null`, and two
// builds answer one load balancer during a rollout.
test("a child an older build answered with a null claim draws its row", () => {
  drawChild({ ...child, claim: null } as unknown as IssueChild);
  expect(screen.getByText("In progress")).toBeTruthy();
  expect(screen.queryByText("Claimed by")).toBeNull();
});

test("a child holding a claim names who holds it", () => {
  drawChild({
    ...child,
    claim: { actor: { id: "alice", kind: "user" }, at: "2026-11-05T01:00:00Z" },
  });
  expect(screen.getByText("Claimed by")).toBeTruthy();
});

// R5: a child's holder is named as the page names an author (`AgentName`'s `author`): a session
// by its title, never by its id, in words with no link and not in the application's blue.
test("a child held by a session is named as the page names an author, with no link", () => {
  drawChild({
    ...child,
    claim: {
      actor: {
        id: "0f8e2c4a-5b6d-4e7f-8a9b-0c1d2e3f4a5b",
        kind: "session",
        origin: { session_title: "Legion implementer · CORE-2" },
      },
      at: "2026-11-05T01:00:00Z",
    },
  });
  const facts = screen.getByText("Claimed by").closest("[data-child-facts]");
  const holder = facts?.querySelector("[data-child-holder]");
  // `AgentName` draws a no-break space before a title's "·", so no line starts with the dot.
  expect(holder?.textContent).toContain("Legion implementer\u00a0· CORE-2");
  expect(facts?.textContent).not.toContain("session:");
  expect(facts?.textContent).not.toContain("0f8e2c4a");
  expect(facts?.querySelectorAll("a")).toHaveLength(0);
  expect(holder?.querySelector("[class*='text-blue'], [class*='text-sky']")).toBeNull();
});

// R5: a session that neither the registry nor its claim names a title for reads as the page's
// word for an agent nothing names, never as its id.
test("a child held by a session nothing names reads as the page's word for an unnamed agent", () => {
  drawChild({
    ...child,
    claim: {
      actor: { id: "0f8e2c4a-5b6d-4e7f-8a9b-0c1d2e3f4a5b", kind: "session" },
      at: "2026-11-05T01:00:00Z",
    },
  });
  const facts = screen.getByText("Claimed by").closest("[data-child-facts]");
  expect(facts?.querySelector("[data-child-holder]")?.textContent).toBe(
    CONTROL_LABELS.unnamed_author
  );
  expect(facts?.textContent).not.toContain("session:");
  expect(facts?.textContent).not.toContain("0f8e2c4a");
});

// Two keys drawn with nothing between them read as one key: "Blocked by CORE-7CORE-8" names no
// issue a reader can look up.
test("a child waiting on two issues reads them as two keys", () => {
  drawChild({ ...child, needs: ["CORE-7", "CORE-8"] });
  const line = screen.getByTestId("issue-child-blocked-by");
  expect(line.textContent).toBe(" · Blocked by CORE-7, CORE-8");
  expect(
    within(line)
      .getAllByRole("link")
      .map((link) => link.textContent)
  ).toEqual(["CORE-7", "CORE-8"]);
});

test("a child's key is its row's tag and opens its own page", () => {
  drawChild(child);
  const tag = screen.getByRole("link", { name: "CORE-2" });
  expect(tag.getAttribute("href")).toBe("/issues/CORE-2");
});
