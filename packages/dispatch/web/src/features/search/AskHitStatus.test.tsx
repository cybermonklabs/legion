import { afterEach, beforeAll, expect, test } from "bun:test";
import { cleanup, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import type { SearchAsk } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { AskHitStatus } from "./AskHitStatus";

beforeAll(async () => {
  await warmMarkdownRenderer();
});

afterEach(cleanup);

// A hit's `Chose …` draws each label from the tree the hit carries beside it, at the label's own
// index (`search-ask-chosen-options`), and parses nothing: the trees here read `Bronze` and
// `Iron` where the labels' markdown says `Brass` and `Steel`, so a line that parsed its labels,
// or took one label's tree for another, would show other words.
test("an ask hit's chosen options are drawn from the trees of their labels", async () => {
  const ask = {
    answer: {
      at: "2026-10-08T03:00:00Z",
      selected: ["**Brass** it", "Steel"],
      selected_trees: [displayTree("**Bronze** it"), displayTree("Iron")],
      text: null,
      user: "alice",
    },
    state: "answered",
  } as unknown as SearchAsk;
  const view = render(
    <MemoryRouter>
      <AskHitStatus ask={ask} muted={false} />
    </MemoryRouter>
  );
  try {
    await waitFor(() => expect(view.container.textContent).toContain("Chose Bronze it, Iron"));
    expect(view.container.querySelector("strong")?.textContent).toBe("Bronze");
  } finally {
    view.unmount();
  }
});
