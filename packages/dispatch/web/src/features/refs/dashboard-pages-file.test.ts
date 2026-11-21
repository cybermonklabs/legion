import { expect, test } from "bun:test";

import { DASHBOARD_PAGES_FILE, renderDashboardPages } from "./dashboard-pages-file";

// The committed pages file is what the route table renders today: a tab added to `ISSUE_TABS`, a
// segment added to `ISSUE_DOCUMENT_SEGMENTS`, or a page that comes to draw another document
// (`issueDocuments`, the choice `IssuePage.tsx` draws each tab from) changes the render, and this
// fails until `bun web/src/features/refs/dashboard-pages-file.ts --write` (from packages/dispatch)
// writes the file again for the server and the agent client.
test("the committed pages file is the route table's render", async () => {
  expect(await Bun.file(DASHBOARD_PAGES_FILE).text()).toBe(renderDashboardPages());
});

// What the render holds today, so a change of the route table's reading is a change of this test
// as well as of the file.
test("the render names the eight tabs of the issue, both segments of a document and its pages", () => {
  const file = JSON.parse(renderDashboardPages());
  expect(file.tabs).toEqual([
    { page: "verification", reference: "verification" },
    { page: "plan", reference: "plan" },
    { page: "steps", reference: "steps" },
    { page: "conversation", reference: "log" },
    { page: "children", reference: "children" },
    { page: "artifacts", reference: "artifacts" },
    { page: "activity", reference: "activity" },
    { page: "collaborators", reference: "collaborators" },
  ]);
  expect(file.document_segments).toEqual(["artifacts", "artifact"]);
  expect(file.document_pages).toEqual({
    primary: [
      "/issues/{key}",
      "/issues/{key}/spec",
      "/issues/{key}/artifacts/{slug}",
      "/issues/{key}/artifact/{slug}",
    ],
    verification: [
      "/issues/{key}/verification",
      "/issues/{key}/artifacts/{slug}",
      "/issues/{key}/artifact/{slug}",
    ],
    plan: ["/issues/{key}/plan", "/issues/{key}/artifacts/{slug}", "/issues/{key}/artifact/{slug}"],
    decisions: [
      "/issues/{key}/decisions",
      "/issues/{key}/artifacts/{slug}",
      "/issues/{key}/artifact/{slug}",
    ],
    issue: ["/issues/{key}/artifacts/{slug}", "/issues/{key}/artifact/{slug}"],
    project: ["/projects/{project}/documents/{slug}"],
    agent: ["/agents/{session}/artifacts/{slug}"],
  });
});
