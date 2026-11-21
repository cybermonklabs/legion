import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen } from "@testing-library/react";

import { tabClassName } from "../../components/tab-classes";
import { IssueHeaderSkeleton, IssueTabsSkeleton } from "./issue-page-skeleton";

afterEach(cleanup);

// The space the page keeps for its tab strip stands at the strip's own size, row by row
// (`issue-page-skeleton.tsx`). What the strip trails - the Spec tab's toolbar - takes a row of
// its own below `md`, and wraps onto a second there, so the space draws what the strip trails
// where the strip draws it rather than a box of its own: a box holding the dot alone kept one
// of the toolbar's two rows, 52 px short at 390 px (dispatch://INFRA-341).
test("the strip's space draws what the strip trails, where the strip draws it", () => {
  render(<IssueTabsSkeleton artifactCount={1} trailing={<div data-testid="held-toolbar" />} />);

  const box = screen.getByTestId("issue-tabs-box");
  expect(screen.getByTestId("held-toolbar").parentElement).toBe(box);
});

// The selected tab's underline is 2 px of the strip's height from md up wherever that tab holds
// a count, so the space draws the tab the strip will select as selected: drawn unselected, the
// Artifacts tab's box was 2 px short and the panel moved by 2 px as the strip landed - CI run
// 43579010512's diff of `issue-artifacts`.
test("the strip's space draws the tab the strip selects as selected, and no other", () => {
  render(<IssueTabsSkeleton activeTab="artifacts" artifactCount={5} />);

  const tabs = [...(screen.getByTestId("issue-tabs-box").firstElementChild?.children ?? [])];
  const selected = tabs.filter((tab) => tab.className === tabClassName(true, true));
  expect(selected).toHaveLength(1);
  expect(selected[0]?.textContent).toBe("Artifacts");
  expect(tabs.filter((tab) => tab.className === tabClassName(true, false))).toHaveLength(
    tabs.length - 1
  );
});

// The title is the header line that wraps, so the header's space draws the issue's own title in
// the heading's own font, as words nobody reads: a bar stood one line short at 1280 px and two
// at 390 px. They are generated content, so the page holds no second copy of the title as text
// for a reader or a query to find before the header lands.
test("the header's space draws the key and the title as held words, in no one's text", () => {
  render(<IssueHeaderSkeleton issueKey="CORE-1" title="Local testing workflow for Dispatch" />);

  const box = screen.getByTestId("issue-header-box");
  const held = [...box.querySelectorAll("[data-words]")].map((node) =>
    node.getAttribute("data-words")
  );
  expect(held).toEqual(["CORE-1", "Local testing workflow for Dispatch"]);
  expect(screen.queryByText("Local testing workflow for Dispatch")).toBeNull();
  expect(box.querySelector("h1 [data-words]")?.getAttribute("aria-hidden")).toBe("true");
});

// With nothing to trail, the strip draws no toolbar and the space keeps no row for one.
test("the strip's space keeps no toolbar row where the strip draws none", () => {
  render(<IssueTabsSkeleton artifactCount={1} />);

  const box = screen.getByTestId("issue-tabs-box");
  expect(box.children).toHaveLength(1);
});
