import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { act, type RenderResult, render, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { api } from "../../api/client";
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

/** A comment whose one link, to a Dispatch reference, carries a mark on some of its words. */
const MARKED_REFERENCE = "[the **spec** rule](dispatch://CORE-1/spec)";

/** The page's outline of one comment of alice's whose words are `text`, under the page's own
 *  providers and its `issue` scope. */
function drawComment(text: string): RenderResult {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  const root = issueTree([
    node({
      author: { id: "alice", kind: "user" },
      id: "comment-1",
      text,
      tree: displayTree(text),
      type: "comment",
    }),
  ]);
  return render(
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
}

/** The comment's row in `view`, once it draws its words' link. */
function commentRow(view: RenderResult): Promise<HTMLElement> {
  return waitFor(() => {
    const drawn = view.container.querySelector<HTMLElement>(
      '[data-node-row][data-node-id="comment-1"]'
    );
    expect(drawn?.querySelector("a[href]"), "the comment's row drew no link").not.toBeNull();
    return drawn as HTMLElement;
  });
}

// The display schema ranks `link` innermost, so the reading's painter closes the anchor around a
// marked run and opens it again after it: the one link drew three anchors, and `f` stopped on
// each of them, as a screen reader read three links (decision 407). The row joins them into one.
test("a row's marked link is one stop of the f key", async () => {
  const user = userEvent.setup();
  const view = drawComment(MARKED_LINK);
  try {
    const row = await commentRow(view);
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

// Each anchor of a Dispatch reference is a portal's host: the painter empties it for the
// portal that draws its words (`collectReferenceAnchors`). Joined into one, the second and the
// third anchors left the page with their portals, and "spec rule" with them.
test("a row's reference link whose words carry a mark keeps all its words", async () => {
  const getIssueSummary = spyOn(api, "getIssueSummary").mockResolvedValue({
    documents: [
      { id: "spec", kind: "doc", latest_version: 1, name: "Spec.md", primary: true, slug: "spec" },
    ],
    key: "CORE-1",
    project: "CORE",
    status: "in_progress",
    title: "Core one",
  });
  const view = drawComment(MARKED_REFERENCE);
  try {
    const row = await commentRow(view);
    await waitFor(() =>
      expect(
        row.querySelector("[data-node-words]")?.textContent,
        `the row drew its words as ${row.querySelector("[data-node-words]")?.innerHTML}`
      ).toBe("the spec rule")
    );
  } finally {
    getIssueSummary.mockRestore();
    view.unmount();
  }
});
