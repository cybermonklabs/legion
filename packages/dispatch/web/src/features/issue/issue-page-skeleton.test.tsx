import { afterEach, expect, test } from "bun:test";
import { cleanup, render, screen } from "@testing-library/react";

import { tabTrailingClassName } from "../../components/tab-classes";
import { IssueTabsSkeleton } from "./issue-page-skeleton";

afterEach(cleanup);

// The space the page keeps for its tab strip stands at the strip's own size, row by row
// (`issue-page-skeleton.tsx`). Below `md` the toolbar the strip trails is `w-full` and takes a
// row of its own under the tabs - a `min-h-11` select and the row's gap, about 48 px - and the
// space drew the dot inline instead, so none of that row was kept and the strip landed on the
// reader by a row on exactly the viewport this space is written for (dispatch://INFRA-341).
test("the strip's space keeps the toolbar's own row, at the toolbar's own height", () => {
  render(<IssueTabsSkeleton artifactCount={1} trailing={<span data-testid="held-dot" />} />);

  const kept = screen.getByTestId("held-dot").parentElement;
  // The toolbar's own box, from the one place both read it (`components/tab-classes.ts`), so
  // the two cannot drift; and the height of the control standing in it.
  expect(kept?.className).toContain(tabTrailingClassName);
  expect(kept?.className).toContain("min-h-11");
  expect(kept?.className).toContain("md:min-h-8");
});

// With nothing to trail, the strip draws no toolbar and the space keeps no row for one.
test("the strip's space keeps no toolbar row where the strip draws none", () => {
  render(<IssueTabsSkeleton artifactCount={1} />);

  const box = screen.getByTestId("issue-tabs-box");
  expect(box.children).toHaveLength(1);
});
