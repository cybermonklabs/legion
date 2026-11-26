import { beforeAll, expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { NOTHING_PUBLISHED } from "../refs/reference-names";
import { useKeymapScope } from "../shell/keymap";
import { issueTree, node } from "./decisions-filter/decisions-fixtures";
import { IssueTree } from "./IssueTree";
import { AppProviders } from "./slots/stage-fixtures";

/**
 * A row's words as the page draws them from the server's reading (`RowText`), read through the
 * keys a reader reaches them by: the outline is one tab stop, and `f` steps through the focused
 * row's links and controls (`useTreeKeymap.ts`'s `rowStops`).
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The page's own `issue` scope, which every one of the outline's keys is registered in. */
function IssueScope(): ReactNode {
  useKeymapScope("issue");
  return null;
}

/** A comment whose one link carries a mark on some of its words. */
const MARKED_LINK = "[the **HTTP** rule](https://example.com/x)";

// The display schema ranks `link` innermost, so the reading's painter closes the anchor around a
// marked run and opens it again after it: the one link drew three anchors, and `f` stopped on
// each of them, as a screen reader read three links (decision 407). The row joins them into one.
test("a row's marked link is one stop of the f key", async () => {
  const user = userEvent.setup();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const root = issueTree([
    node({
      author: { id: "alice", kind: "user" },
      id: "comment-1",
      text: MARKED_LINK,
      tree: displayTree(MARKED_LINK),
      type: "comment",
    }),
  ]);
  const view = render(
    <MemoryRouter>
      <AppProviders client={client}>
        <IssueScope />
        <IssueTree
          artifacts={new Map()}
          context={new Set()}
          filtering={false}
          focusReveal={false}
          holdsEscape={false}
          isClosed={false}
          onEditing={() => {}}
          published={NOTHING_PUBLISHED}
          reveal={undefined}
          root={root}
          rootRow
        />
      </AppProviders>
    </MemoryRouter>
  );
  try {
    const row = await waitFor(() => {
      const drawn = view.container.querySelector<HTMLElement>(
        '[data-node-row][data-node-id="comment-1"]'
      );
      expect(drawn?.querySelector("a[href]"), "the comment's row drew no link").not.toBeNull();
      return drawn as HTMLElement;
    });
    act(() => row.focus());
    // Each `f` moves to the row's next stop, and the last comes back round to the first.
    const stops: Element[] = [];
    for (let press = 0; press < 10; press += 1) {
      await user.keyboard("f");
      const on = document.activeElement;
      if (on === null || stops.includes(on)) break;
      stops.push(on);
    }
    expect(
      stops
        .filter((stop) => stop.getAttribute("href") === "https://example.com/x")
        .map((stop) => stop.textContent),
      `\`f\` stopped on ${stops.map((stop) => stop.outerHTML).join(", ")}`
    ).toEqual(["the HTTP rule"]);
  } finally {
    view.unmount();
  }
});
