import { afterEach, beforeAll, expect, test } from "bun:test";
import { cleanup, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import type { AskAnswerWithTrees } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { AskAnswerHistory } from "./AskCompletionCard";
import { markdownText } from "./ask-text";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(cleanup);

// Each label an earlier answer chose is drawn from the tree the answer carries beside it, at the
// label's own index, and never parsed in the page: the trees here read `soon` and `Went` where
// the labels' markdown says `now` and `Gone`. The second label names an option the ask no
// longer holds, which is drawn the same way.
test("an earlier answer's chosen options are drawn from the trees of their labels", async () => {
  const answers = [
    {
      at: "2026-10-08T01:00:00Z",
      selected: ["Ship **now**", "**Gone**"],
      selected_trees: [displayTree("Ship **soon**"), displayTree("**Went**")],
      text: null,
      user: "alice",
    },
    { at: "2026-10-08T02:00:00Z", selected: ["Hold"], text: null, user: "alice" },
  ] as unknown as AskAnswerWithTrees[];
  const view = render(
    <MemoryRouter>
      <AskAnswerHistory answers={answers} askId="ask-1" issue="CORE-1" renderText={markdownText} />
    </MemoryRouter>
  );
  try {
    await waitFor(() =>
      expect(
        [...view.container.querySelectorAll("strong")].map((bold) => bold.textContent)
      ).toEqual(["soon", "Went"])
    );
    expect(view.container.textContent).toContain("Ship soon, Went");
  } finally {
    view.unmount();
  }
});
